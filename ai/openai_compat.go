package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// mapReasoningEffort applies the optional effort mapping.
func mapReasoningEffort(effort ReasoningLevel, effortMap map[string]string) string {
	if effortMap == nil {
		return string(effort)
	}
	if mapped, ok := effortMap[string(effort)]; ok {
		return mapped
	}
	return string(effort)
}

func hasOpenRouterRouting(routing *OpenRouterRouting) bool {
	if routing == nil {
		return false
	}

	return routing.AllowFallbacks != nil ||
		routing.RequireParameters != nil ||
		routing.DataCollection != "" ||
		routing.ZDR != nil ||
		routing.EnforceDistillableText != nil ||
		len(routing.Order) > 0 ||
		len(routing.Only) > 0 ||
		len(routing.Ignore) > 0 ||
		len(routing.Quantizations) > 0 ||
		routing.Sort != nil ||
		routing.MaxPrice != nil ||
		routing.PreferredMinThroughput != nil ||
		routing.PreferredMaxLatency != nil
}

// maybeAddOpenRouterAnthropicCacheControl adds Anthropic-style cache_control
// to the last text content part when routing through OpenRouter to Anthropic.
// This enables prompt caching on the Anthropic provider path.
func maybeAddOpenRouterAnthropicCacheControl(model *Model, messages []map[string]any) {
	if model.Provider != ProviderOpenRouter || !strings.HasPrefix(model.ID, "anthropic/") {
		return
	}

	// Walk backwards from the end to find the last user/assistant message with text
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		role, _ := msg["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}

		content := msg["content"]
		switch c := content.(type) {
		case string:
			if c != "" {
				msg["content"] = []map[string]any{
					{"type": "text", "text": c, "cache_control": map[string]any{"type": "ephemeral"}},
				}
				return
			}
		case []any:
			// Find last text part and add cache_control
			for j := len(c) - 1; j >= 0; j-- {
				if part, ok := c[j].(map[string]any); ok {
					if partType, _ := part["type"].(string); partType == "text" {
						part["cache_control"] = map[string]any{"type": "ephemeral"}
						return
					}
				}
			}
		}
	}
}

// OpenAICompatProvider handles the shared OpenAI-compatible transport used by
// several provider families. Support for a provider family still depends on the
// public support taxonomy rather than this shared implementation alone.
type OpenAICompatProvider struct{}

// API implements Provider.
func (p *OpenAICompatProvider) API() string {
	return APIOpenAICompatible
}

// IsAlias implements Provider.
func (p *OpenAICompatProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *OpenAICompatProvider) NewClient(model *Model) (Client, error) {
	return &openAICompatClient{model: model}, nil
}

// openAICompatClient implements Client for OpenAI-compatible providers.
type openAICompatClient struct {
	model *Model
}

// Capabilities implements CapableProvider.
// Compat providers default to OpenAI-compatible baseline capabilities;
// per-instance overrides can be declared in settings.json.
func (p *OpenAICompatProvider) Capabilities() Capabilities {
	return OpenAICompletionsCapabilities
}

// Complete implements Client.
func (c *openAICompatClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

// Stream implements Client.Streamer.
func (c *openAICompatClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) (err error) {
	var o options
	o.apply(opts)

	if resolveOpenAIAPIKey(c.model, &o) == "" {
		return fmt.Errorf("%s: missing API key (set %s)", c.model.Provider, envVarNameForProvider(c.model.Provider))
	}

	transport, err := newOpenAIChatCompletionsTransport(c.model)
	if err != nil {
		return err
	}

	// Build request body
	body := buildOpenAICompatRequestBody(model, prompt, &o)
	if hasPayloadCallback(&o) {
		if next := invokePayloadCallback(&o, model, body); next != nil {
			if replacement, ok := next.(map[string]any); ok {
				body = replacement
			}
		}
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s: marshal request: %w", c.model.Provider, err)
	}

	resp, err := doOpenAIChatCompletions(ctx, model, &o, transport.withRequestOptions(c.model, &o), string(bodyBytes))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	// Stream response
	return streamOpenAIResponseWithAPI(ctx, model, resp.Body, handler, APIOpenAICompatible, c.model.Provider)
}

// streamOpenAICompatResponse streams the SSE response.
func streamOpenAICompatResponse(ctx context.Context, model *Model, body io.Reader, handler EventHandler, provider string) error {
	return streamOpenAIResponseWithAPI(ctx, model, body, handler, APIOpenAICompatible, provider)
}

// mapOpenAICompatStopReason maps OpenAI finish_reason to StopReason, matching pi-mono's mapStopReason.
// P7-OC20: Unknown finish_reason values map to StopReasonError.
func mapOpenAICompatStopReason(reason string) (StopReason, string) {
	return mapOpenAIStopReason(reason)
}

// buildOpenAICompatRequestBody builds the request body.
func buildOpenAICompatRequestBody(model *Model, aiCtx *Context, o *options) map[string]any {
	req, err := buildOpenAIChatCompletionsRequest(model, aiCtx, o)
	if err != nil {
		// Preserve the old helper's best-effort behavior for callers/tests that use
		// it as an inspection helper rather than as the actual provider path.
		req = &openAIRequest{Model: model.ID, Stream: true}
	}
	return openAIRequestToMap(req)
}

func openAIRequestToMap(req *openAIRequest) map[string]any {
	body := map[string]any{
		"model":    req.Model,
		"messages": openAIMessagesToMaps(req.Messages),
		"stream":   req.Stream,
	}
	if len(req.Tools) > 0 {
		body["tools"] = openAIToolsToMaps(req.Tools)
	} else if req.Tools != nil {
		body["tools"] = []any{}
	}
	if req.Metadata != nil {
		body["metadata"] = req.Metadata
	}
	if req.Store != nil {
		body["store"] = *req.Store
	}
	if req.MaxTokens != 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.MaxCompletionTokens != 0 {
		body["max_completion_tokens"] = req.MaxCompletionTokens
	}
	if req.ReasoningEffort != "" {
		body["reasoning_effort"] = req.ReasoningEffort
	}
	if req.EnableThinking != nil {
		body["enable_thinking"] = *req.EnableThinking
	}
	if req.Thinking != nil {
		thinking := map[string]any{"type": req.Thinking.Type}
		if req.Thinking.BudgetTokens > 0 {
			thinking["budget_tokens"] = req.Thinking.BudgetTokens
		}
		body["thinking"] = thinking
	}
	if len(req.ChatTemplate_kwargs) > 0 {
		chatTemplate := make(map[string]any, len(req.ChatTemplate_kwargs))
		for k, v := range req.ChatTemplate_kwargs {
			chatTemplate[k] = v
		}
		body["chat_template_kwargs"] = chatTemplate
	}
	if req.Reasoning != nil {
		body["reasoning"] = map[string]any{"effort": req.Reasoning.Effort}
	}
	if req.Provider != nil {
		body["provider"] = req.Provider
	}
	if req.ProviderOptions != nil {
		body["provider_options"] = req.ProviderOptions
		body["providerOptions"] = req.ProviderOptions
	}
	if req.ToolChoice != nil {
		body["tool_choice"] = req.ToolChoice
	}
	if req.ToolStream {
		body["tool_stream"] = true
	}
	if req.StreamOptions != nil {
		body["stream_options"] = map[string]bool{"include_usage": req.StreamOptions.IncludeUsage}
	}
	return body
}

func openAIMessagesToMaps(messages []openAIMessage) []map[string]any {
	result := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		entry := map[string]any{"role": msg.Role}
		if msg.Content != nil {
			entry["content"] = msg.Content
		}
		if msg.Name != "" {
			entry["name"] = msg.Name
		}
		if msg.ToolCallID != "" {
			entry["tool_call_id"] = msg.ToolCallID
		}
		if len(msg.ToolCalls) > 0 {
			entry["tool_calls"] = openAIToolCallsToMaps(msg.ToolCalls)
		}
		if len(msg.ReasoningDetails) > 0 {
			details := make([]any, len(msg.ReasoningDetails))
			for i, detail := range msg.ReasoningDetails {
				details[i] = detail
			}
			entry["reasoning_details"] = details
		}
		result = append(result, entry)
	}
	return result
}

func openAIToolCallsToMaps(toolCalls []toolCall) []map[string]any {
	result := make([]map[string]any, len(toolCalls))
	for i, tc := range toolCalls {
		var args any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			args = tc.Function.Arguments
		}
		result[i] = map[string]any{
			"id":   tc.ID,
			"type": tc.Type,
			"function": map[string]any{
				"name":      tc.Function.Name,
				"arguments": args,
			},
		}
	}
	return result
}

func openAIToolsToMaps(tools []toolDefinition) []map[string]any {
	result := make([]map[string]any, len(tools))
	for i, tool := range tools {
		function := map[string]any{
			"name":        tool.Function.Name,
			"description": tool.Function.Description,
			"parameters":  tool.Function.Parameters,
		}
		entry := map[string]any{"type": tool.Type, "function": function}
		if tool.Function.Strict != nil {
			entry["strict"] = *tool.Function.Strict
			function["strict"] = *tool.Function.Strict
		}
		result[i] = entry
	}
	return result
}

func convertMessagesForOpenAICompat(aiCtx *Context, model *Model, compat *OpenAICompletionsCompat) []map[string]any {
	client := &openAIClient{model: model}
	messages, err := client.convertMessages(aiCtx, model, compat)
	if err != nil {
		return nil
	}
	return openAIMessagesToMaps(messages)
}

// convertMessageForOpenAICompat converts a single Message to OpenAI-compatible format.
func convertMessageForOpenAICompat(msg Message) map[string]any {
	ctx := &Context{Messages: []Message{msg}}
	messages := convertMessagesForOpenAICompat(ctx, &Model{ID: "compat-helper"}, &OpenAICompletionsCompat{})
	if len(messages) == 0 {
		return map[string]any{}
	}
	return messages[0]
}

// parseOpenAICompatUsage parses usage with detailed token counting.
// P7-OC04: Handles prompt_tokens_details.{cached_tokens, cache_write_tokens}
// P7-OC04: Handles completion_tokens_details.reasoning_tokens
func parseOpenAICompatUsage(raw *openAICompatUsage, model *Model) Usage {
	promptTokens := raw.PromptTokens
	completionTokens := raw.CompletionTokens

	// P7-OC04: Parse prompt_tokens_details
	cachedTokens := 0
	cacheWriteTokens := 0
	if raw.PromptTokensDetails != nil {
		cachedTokens = raw.PromptTokensDetails.CachedTokens
		cacheWriteTokens = raw.PromptTokensDetails.CacheWriteTokens
	}

	// P7-OC04: Parse completion_tokens_details
	reasoningTokens := 0
	if raw.CompletionTokensDetails != nil {
		reasoningTokens = raw.CompletionTokensDetails.ReasoningTokens
	}

	// Normalize to pi-ai semantics:
	// - cacheRead: hits from cache created by previous requests only
	// - cacheWrite: tokens written to cache in this request
	// Some providers report cached_tokens as (previous hits + current writes).
	// In that case, remove cacheWrite from cacheRead.
	cacheReadTokens := cacheWriteTokens
	if cacheReadTokens > 0 {
		cacheReadTokens = cachedTokens - cacheWriteTokens
		if cacheReadTokens < 0 {
			cacheReadTokens = 0
		}
	} else {
		cacheReadTokens = cachedTokens
	}

	// Compute non-cached input tokens
	input := promptTokens - cacheReadTokens - cacheWriteTokens
	if input < 0 {
		input = 0
	}

	// Add reasoning tokens to output (some providers don't include them in total)
	outputTokens := completionTokens + reasoningTokens

	usage := Usage{
		Input:      input,
		Output:     outputTokens,
		CacheRead:  cacheReadTokens,
		CacheWrite: cacheWriteTokens,
	}
	usage.CalculateCost(model.Cost)

	return usage
}
