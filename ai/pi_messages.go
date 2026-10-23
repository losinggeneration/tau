package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// PiMessagesProvider streams the pi-messages protocol: one POST of
// {model, context, options} to <baseURL>/messages answered by an SSE stream of
// serialized assistant-message events. The Radius gateway speaks it, and so can
// any models.json backend declaring "api": "pi-messages".
type PiMessagesProvider struct{}

// API implements Provider.
func (p *PiMessagesProvider) API() string { return APIPiMessages }

// IsAlias implements Provider.
func (p *PiMessagesProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *PiMessagesProvider) NewClient(*Model) (Client, error) { return &piMessagesClient{}, nil }

type piMessagesClient struct{}

// Complete implements Client.
func (c *piMessagesClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

type piMessagesEvent struct {
	Type             string                     `json:"type"`
	ContentIndex     int                        `json:"contentIndex"`
	Delta            string                     `json:"delta"`
	Content          string                     `json:"content"`
	ContentSignature string                     `json:"contentSignature"`
	Redacted         bool                       `json:"redacted"`
	ID               string                     `json:"id"`
	ToolName         string                     `json:"toolName"`
	ToolCall         *ContentBlock              `json:"toolCall"`
	Reason           StopReason                 `json:"reason"`
	Usage            Usage                      `json:"usage"`
	ErrorMessage     string                     `json:"errorMessage"`
	ResponseID       string                     `json:"responseId"`
	Rewrite          map[string]json.RawMessage `json:"rewrite"`
}

// Stream implements Client.
func (c *piMessagesClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) (err error) {
	resolvedOptions, resolveErr := resolveRequestOptions(ctx, model, opts)
	if resolveErr != nil {
		return resolveErr
	}
	opts = resolvedOptions
	handler = timedEventHandler(handler)
	if err := ValidateContextLimits(model, prompt); err != nil {
		return err
	}
	cfg := applyOptions(opts)
	key := cfg.apiKey
	if key == "" {
		key = GetEnvAPIKey(model.Provider)
	}
	if key == "" {
		return fmt.Errorf("no API key provided for provider %q", model.Provider)
	}
	transcript, err := piMessagesTranscript(prompt)
	if err != nil {
		return err
	}
	options := map[string]any{}
	if cfg.temperature != nil {
		options["temperature"] = *cfg.temperature
	}
	if cfg.maxTokens > 0 {
		options["maxTokens"] = cfg.maxTokens
	}
	if cfg.reasoning != "" {
		options["reasoning"] = cfg.reasoning
	}
	retention := cfg.cacheRetention
	if retention == "" && os.Getenv("PI_CACHE_RETENTION") == "long" {
		retention = string(CacheRetentionLong)
	}
	if retention != "" {
		options["cacheRetention"] = retention
	}
	if cfg.sessionID != "" {
		options["sessionId"] = cfg.sessionID
	}
	if cfg.toolChoice != nil {
		options["toolChoice"] = cfg.toolChoice
	}
	var payload any = map[string]any{"model": model.ID, "context": transcript, "options": options}
	if hasPayloadCallback(cfg) {
		if next := invokePayloadCallback(cfg, model, payload); next != nil {
			payload = next
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal pi-messages request: %w", err)
	}
	baseURL, err := expandBaseURL(strings.TrimRight(model.BaseURL, "/"))
	if err != nil {
		return err
	}
	url := baseURL + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create pi-messages request: %w", err)
	}
	for name, value := range model.Headers {
		req.Header.Set(name, value)
	}
	for name, value := range cfg.headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")

	partial := AssistantMessage{Role: "assistant", API: model.API, Provider: model.Provider, Model: model.ID, Content: []ContentBlock{}, Timestamp: timestamp()}
	fail := func(err error) error {
		reason := StopReasonError
		if ctx.Err() != nil {
			reason = StopReasonAborted
		}
		emitTerminalStreamError(ctx, handler, partial, err, reason)
		return nil
	}
	resp, err := doHeaderRequest(req, cfg.timeout, string(model.Provider))
	if err != nil {
		return fail(fmt.Errorf("%s request: %w", model.Provider, err))
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	if hasResponseCallback(cfg) {
		invokeResponseCallback(cfg, model, responseMetadata(resp))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fail(fmt.Errorf("read %s error response: %w", model.Provider, readErr))
		}
		failure := piMessagesResponseFailure(model, url, resp, data)
		partial.Diagnostics = append(partial.Diagnostics, failure)
		return fail(errors.New(failure.Error["message"]))
	}

	toolJSON := map[int]string{}
	reader := NewSSEReader(resp.Body)
	for {
		data, ok := reader.Next()
		if !ok {
			break
		}
		var event piMessagesEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fail(fmt.Errorf("decode %s event: %w", model.Provider, err))
		}
		index := event.ContentIndex
		if kind, phase, ok := strings.Cut(event.Type, "_"); ok {
			if index < 0 || index > len(partial.Content) || (phase != "start" && index == len(partial.Content)) {
				return fail(fmt.Errorf("%s event %s references missing content %d", model.Provider, event.Type, index))
			}
			if phase == "start" && index == len(partial.Content) {
				partial.Content = append(partial.Content, ContentBlock{})
			}
			want := map[string]string{"text": ContentBlockText, "thinking": ContentBlockThinking, "toolcall": ContentBlockToolCall}[kind]
			if phase != "start" && partial.Content[index].Type != want {
				return fail(fmt.Errorf("%s event %s targets %s content %d", model.Provider, event.Type, partial.Content[index].Type, index))
			}
		}
		switch event.Type {
		case "start":
			handler(ctx, Start{Partial: partial})
		case "text_start":
			partial.Content[index] = ContentBlock{Type: ContentBlockText}
			handler(ctx, TextStart{ContentIndex: index, Partial: partial})
		case "text_delta":
			partial.Content[index].Text += event.Delta
			handler(ctx, TextDelta{ContentIndex: index, Delta: event.Delta, Partial: partial})
		case "text_end":
			partial.Content[index].Text = event.Content
			partial.Content[index].TextSignature = event.ContentSignature
			handler(ctx, TextEnd{ContentIndex: index, Content: event.Content, ContentSignature: event.ContentSignature, Partial: partial})
		case "thinking_start":
			partial.Content[index] = ContentBlock{Type: ContentBlockThinking}
			handler(ctx, ThinkingStart{ContentIndex: index, Partial: partial})
		case "thinking_delta":
			partial.Content[index].Thinking += event.Delta
			handler(ctx, ThinkingDelta{ContentIndex: index, Delta: event.Delta, Partial: partial})
		case "thinking_end":
			partial.Content[index].Thinking = event.Content
			partial.Content[index].ThoughtSignature = event.ContentSignature
			partial.Content[index].Redacted = event.Redacted
			handler(ctx, ThinkingEnd{ContentIndex: index, Content: event.Content, ContentSignature: event.ContentSignature, Partial: partial})
		case "toolcall_start":
			partial.Content[index] = ContentBlock{Type: ContentBlockToolCall, ID: event.ID, Name: event.ToolName, Arguments: json.RawMessage("{}")}
			toolJSON[index] = ""
			handler(ctx, ToolCallStart{ContentIndex: index, Partial: partial})
		case "toolcall_delta":
			toolJSON[index] += event.Delta
			arguments, err := json.Marshal(ParseStreamingJSON(toolJSON[index]))
			if err != nil {
				return fail(fmt.Errorf("encode %s tool arguments: %w", model.Provider, err))
			}
			partial.Content[index].Arguments = arguments
			handler(ctx, ToolCallDelta{ContentIndex: index, Delta: event.Delta, Partial: partial})
		case "toolcall_end":
			block := &partial.Content[index]
			if call := event.ToolCall; call != nil {
				block.ID, block.Name, block.Arguments = call.ID, call.Name, call.Arguments
				block.Namespace = call.Namespace
				if call.ThoughtSignature != "" {
					block.ThoughtSignature = call.ThoughtSignature
				}
			}
			delete(toolJSON, index)
			handler(ctx, ToolCallEnd{ContentIndex: index, ToolCall: ToolCall{ID: block.ID, Name: block.Name, Arguments: block.Arguments, Namespace: block.Namespace}, Partial: partial})
		case "done", "error":
			partial.StopReason = event.Reason
			partial.Usage = event.Usage
			partial.ResponseID = event.ResponseID
			if event.Rewrite != nil {
				partial.Diagnostics = append(partial.Diagnostics, AssistantDiagnostic{Type: "pi_messages_rewrite", Timestamp: time.Now().UnixMilli(), Details: jsonStringMap(event.Rewrite)})
			}
			if event.Type == "done" {
				handler(ctx, Done{Reason: event.Reason, Message: partial})
				return nil
			}
			partial.ErrorMessage = event.ErrorMessage
			emitTerminalStreamError(ctx, handler, partial, errors.New(event.ErrorMessage), event.Reason)
			return nil
		}
	}
	if err := reader.Error(); err != nil {
		return fail(err)
	}
	return fail(fmt.Errorf("%s stream ended without a terminal event", model.Provider))
}

// piMessagesTranscript sends the transcript form, with thinking signatures
// under the thinkingSignature field.
func piMessagesTranscript(prompt *Context) (map[string]any, error) {
	data, err := json.Marshal(NormalizeContext(prompt))
	if err != nil {
		return nil, fmt.Errorf("marshal pi-messages context: %w", err)
	}
	var transcript map[string]any
	if err := json.Unmarshal(data, &transcript); err != nil {
		return nil, fmt.Errorf("decode pi-messages context: %w", err)
	}
	messages, _ := transcript["messages"].([]any)
	for _, message := range messages {
		content, _ := message.(map[string]any)["content"].([]any)
		for _, item := range content {
			block, _ := item.(map[string]any)
			if block["type"] != ContentBlockThinking {
				continue
			}
			if signature, ok := block["thoughtSignature"]; ok {
				block["thinkingSignature"] = signature
				delete(block, "thoughtSignature")
			}
		}
	}
	return transcript, nil
}

func piMessagesResponseFailure(model *Model, url string, resp *http.Response, body []byte) AssistantDiagnostic {
	details := map[string]string{
		"provider":   model.Provider,
		"model":      model.ID,
		"url":        url,
		"status":     strconv.Itoa(resp.StatusCode),
		"statusText": http.StatusText(resp.StatusCode),
	}
	text := string(body)
	var parsed struct {
		Error json.RawMessage `json:"error"`
	}
	var fields map[string]any
	if json.Unmarshal(body, &parsed) == nil && json.Unmarshal(parsed.Error, &fields) == nil && fields != nil {
		details["error"] = string(parsed.Error)
		if message, ok := fields["message"].(string); ok && message != "" {
			text = message
		}
		if code, ok := fields["code"].(string); ok && code != "" {
			text += " (" + code + ")"
		}
	} else {
		details["body"] = truncateErrorText(text, 8192)
	}
	message := fmt.Sprintf("%d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), text)
	return AssistantDiagnostic{
		Type:      "pi_messages_response_failure",
		Timestamp: time.Now().UnixMilli(),
		Error:     map[string]string{"name": "PiMessagesResponseError", "message": message},
		Details:   details,
	}
}

func jsonStringMap(values map[string]json.RawMessage) map[string]string {
	out := make(map[string]string, len(values))
	for name, raw := range values {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			out[name] = text
		} else {
			out[name] = string(raw)
		}
	}
	return out
}
