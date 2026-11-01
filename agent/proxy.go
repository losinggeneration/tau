package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/losinggeneration/tau/ai"
)

// ProxyStreamOptions contains options for proxy streaming.
// Matches pi-mono's ProxyStreamOptions interface.
type ProxyStreamOptions struct {
	// Temperature controls randomness. Higher values produce more varied outputs.
	Temperature *float64
	// MaxTokens limits the maximum number of tokens in the response.
	MaxTokens int
	// Reasoning sets the reasoning/thinking level.
	Reasoning       string
	ThinkingBudgets *ai.ThinkingBudgets
	CacheRetention  string
	SessionID       string
	Headers         map[string]string
	Metadata        map[string]any
	SamplingParams  map[string]any
	Transport       ai.Transport
	MaxRetryDelayMs *int
	// Signal allows cancellation of the request.
	Signal <-chan struct{}
	// AuthToken is the auth token for the proxy server.
	AuthToken string
	// ProxyURL is the proxy server URL (e.g., "https://genai.example.com").
	ProxyURL string
}

// StreamProxy streams responses through a proxy server.
func StreamProxy(
	ctx context.Context,
	model *ai.Model,
	aiCtx *ai.Context,
	handler ai.EventHandler,
	proxyURL string,
	authToken string,
	opts ...ai.Option,
) (err error) {
	handler = ai.TimedEventHandler(handler)
	// Like upstream, request failures surface as terminal assistant error
	// events as well as the returned error.
	failRequest := func(err error) error {
		reason := ai.StopReasonError
		if ctx.Err() != nil {
			reason = ai.StopReasonAborted
			err = errors.Join(err, context.Cause(ctx))
		}
		handler(ctx, ai.StreamError{Reason: string(reason), Err: err, Msg: ai.AssistantMessage{
			Role:         "assistant",
			StopReason:   reason,
			ErrorMessage: err.Error(),
			Timestamp:    time.Now().UnixMilli(),
			API:          model.API,
			Provider:     model.Provider,
			Model:        model.ID,
		}})

		return err
	}

	// Ensure proxy URL ends with /api/stream
	streamURL := strings.TrimRight(proxyURL, "/")
	if !strings.HasSuffix(streamURL, "/api/stream") {
		streamURL += "/api/stream"
	}

	// Extract simple-stream options from the provided options
	options := extractSimpleStreamOptions(opts)

	// Convert context and model to JSON
	bodyBytes, err := json.Marshal(map[string]any{
		"model":   model,
		"context": aiCtx,
		"options": options,
	})
	if err != nil {
		return failRequest(fmt.Errorf("proxy: marshal request: %w", err))
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, streamURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return failRequest(fmt.Errorf("proxy: create request: %w", err))
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")

	req.Header.Set("Authorization", "Bearer "+authToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return failRequest(fmt.Errorf("proxy: request: %w", err))
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("proxy: close response: %w", closeErr))
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return failRequest(fmt.Errorf("proxy: status %d: %w", resp.StatusCode, readErr))
		}

		message := fmt.Sprintf("Proxy error: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		var errorData struct {
			Error string `json:"error"`
		}
		if decodeErr := json.Unmarshal(body, &errorData); decodeErr == nil && errorData.Error != "" {
			message = "Proxy error: " + errorData.Error
		}
		return failRequest(errors.New(message))
	}

	// Stream response
	return streamProxyResponse(ctx, model, resp.Body, handler)
}

// CompleteProxy completes a request through a proxy server.
func CompleteProxy(
	ctx context.Context,
	model *ai.Model,
	aiCtx *ai.Context,
	proxyURL string,
	authToken string,
	opts ...ai.Option,
) (*ai.AssistantMessage, error) {
	var result ai.AssistantMessage

	handler := func(ctx context.Context, event ai.StreamEvent) {
		switch e := event.(type) {
		case ai.Done:
			result = e.Message
		case ai.StreamError:
			result = e.Msg
		}
	}

	if err := StreamProxy(ctx, model, aiCtx, handler, proxyURL, authToken, opts...); err != nil {
		return nil, err
	}

	return &result, nil
}

// StreamProxyWithOptions streams responses through a proxy server using options struct.
// Matches pi-mono's streamProxy function with ProxyStreamOptions.
func StreamProxyWithOptions(
	ctx context.Context,
	model *ai.Model,
	aiCtx *ai.Context,
	handler ai.EventHandler,
	options ProxyStreamOptions,
) error {
	if options.Signal != nil {
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		defer func() {
			cancel()
			<-done
		}()

		go func() {
			defer close(done)
			select {
			case <-options.Signal:
				cancel()
			case <-runCtx.Done():
			}
		}()
		ctx = runCtx
		select {
		case <-options.Signal:
			cancel()
		default:
		}
	}

	// Build options slice
	var opts []ai.Option
	if options.Temperature != nil {
		opts = append(opts, ai.WithTemperature(*options.Temperature))
	}
	if options.MaxTokens > 0 {
		opts = append(opts, ai.WithMaxTokens(options.MaxTokens))
	}
	if options.Reasoning != "" {
		opts = append(opts, ai.WithReasoning(ai.ReasoningLevel(options.Reasoning)))
	}
	if options.ThinkingBudgets != nil {
		opts = append(opts, ai.WithThinkingBudgets(options.ThinkingBudgets))
	}
	if options.CacheRetention != "" {
		opts = append(opts, ai.WithCacheRetention(options.CacheRetention))
	}
	if options.SessionID != "" {
		opts = append(opts, ai.WithSessionID(options.SessionID))
	}
	if len(options.Headers) > 0 {
		opts = append(opts, ai.WithHeaders(options.Headers))
	}
	if len(options.Metadata) > 0 {
		opts = append(opts, ai.WithMetadata(options.Metadata))
	}
	if len(options.SamplingParams) > 0 {
		opts = append(opts, ai.WithSamplingParams(options.SamplingParams))
	}
	if options.Transport != "" {
		opts = append(opts, ai.WithTransport(options.Transport))
	}
	if options.MaxRetryDelayMs != nil {
		opts = append(opts, ai.WithMaxRetryDelayMs(*options.MaxRetryDelayMs))
	}

	return StreamProxy(ctx, model, aiCtx, handler, options.ProxyURL, options.AuthToken, opts...)
}

// CompleteProxyWithOptions completes a request through a proxy server using options struct.
// Matches pi-mono's streamProxy pattern with ProxyStreamOptions.
func CompleteProxyWithOptions(
	ctx context.Context,
	model *ai.Model,
	aiCtx *ai.Context,
	options ProxyStreamOptions,
) (*ai.AssistantMessage, error) {
	var result ai.AssistantMessage

	handler := func(ctx context.Context, event ai.StreamEvent) {
		switch e := event.(type) {
		case ai.Done:
			result = e.Message
		case ai.StreamError:
			result = e.Msg
		}
	}

	if err := StreamProxyWithOptions(ctx, model, aiCtx, handler, options); err != nil {
		return nil, err
	}

	return &result, nil
}

// streamProxyResponse streams the proxy SSE response.
func streamProxyResponse(ctx context.Context, model *ai.Model, body io.Reader, handler ai.EventHandler) error {
	reader := bufio.NewReader(body)

	// Initialize partial message matching pi-mono's structure (proxy.ts:90-106)
	partial := &ai.AssistantMessage{
		Role:       "assistant",
		StopReason: ai.StopReasonPending,
		Content:    []ai.ContentBlock{},
		API:        model.API,
		Provider:   model.Provider,
		Model:      model.ID,
		Timestamp:  time.Now().UnixMilli(),
		Usage: ai.Usage{
			Input:       0,
			Output:      0,
			CacheRead:   0,
			CacheWrite:  0,
			TotalTokens: 0,
			Cost: ai.Cost{
				Input:      0,
				Output:     0,
				CacheRead:  0,
				CacheWrite: 0,
				Total:      0,
			},
		},
	}

	toolCallJSON := make(map[int]string)

	emit := func(err error) {
		partial.StopReason = ai.StopReasonError
		if ctx.Err() != nil {
			partial.StopReason = ai.StopReasonAborted
		}
		partial.ErrorMessage = err.Error()
		handler(ctx, ai.StreamError{Reason: string(partial.StopReason), Err: err, Msg: *partial})
	}
	fail := func(err error) error {
		emit(err)
		return err
	}

	for {
		if ctx.Err() != nil {
			return fail(fmt.Errorf("request aborted by user: %w", context.Cause(ctx)))
		}

		line, readErr := reader.ReadString('\n')
		if ctx.Err() != nil {
			return fail(fmt.Errorf("proxy: read: %w", errors.Join(context.Cause(ctx), readErr)))
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fail(fmt.Errorf("proxy: read: %w", readErr))
		}

		if !strings.HasPrefix(line, "data: ") {
			if readErr != nil {
				break
			}

			continue
		}

		data := strings.TrimFunc(strings.TrimPrefix(line, "data: "), func(r rune) bool {
			return (unicode.IsSpace(r) && r != '\u0085') || r == '\ufeff'
		})
		if data == "" {
			if readErr != nil {
				break
			}
			continue
		}

		var proxyEvent proxyEvent
		if err := json.Unmarshal([]byte(data), &proxyEvent); err != nil {
			return fail(fmt.Errorf("proxy: malformed event: %w", err))
		}
		index := proxyEvent.ContentIndex
		var content *ai.ContentBlock
		switch proxyEvent.Type {
		case "text_start", "thinking_start", "toolcall_start":
			// Content starts may replace an existing slot or append the next
			// slot, but never allocate a peer-controlled sparse slice.
			if index < 0 || index > len(partial.Content) {
				return fail(fmt.Errorf("proxy: invalid content index %d for %s", index, proxyEvent.Type))
			}
			if index == len(partial.Content) {
				partial.Content = append(partial.Content, ai.ContentBlock{})
			}
			delete(toolCallJSON, index)
			content = &partial.Content[index]
		case "text_delta", "text_end", "thinking_delta", "thinking_end", "toolcall_delta", "toolcall_end":
			kind, _, _ := strings.Cut(proxyEvent.Type, "_")
			if kind == "toolcall" {
				kind = "toolCall"
			}
			if index < 0 || index >= len(partial.Content) || partial.Content[index].Type != kind {
				return fail(fmt.Errorf("proxy: invalid content index or type for %s at %d", proxyEvent.Type, index))
			}
			content = &partial.Content[index]
		}

		switch proxyEvent.Type {
		case "start":
			if proxyEvent.Message != nil {
				partial = proxyEvent.Message
			}

			handler(ctx, ai.Start{Partial: *partial})

		case "text_start":
			*content = ai.ContentBlock{Type: "text"}
			handler(ctx, ai.TextStart{ContentIndex: index, Partial: *partial})

		case "text_delta":
			content.Text += proxyEvent.Delta
			handler(ctx, ai.TextDelta{ContentIndex: index, Delta: proxyEvent.Delta, Partial: *partial})

		case "text_end":
			content.TextSignature = proxyEvent.ContentSignature
			handler(ctx, ai.TextEnd{ContentIndex: index, Content: content.Text, ContentSignature: proxyEvent.ContentSignature, Partial: *partial})

		case "thinking_start":
			*content = ai.ContentBlock{Type: "thinking"}
			handler(ctx, ai.ThinkingStart{ContentIndex: index, Partial: *partial})

		case "thinking_delta":
			content.Thinking += proxyEvent.Delta
			handler(ctx, ai.ThinkingDelta{ContentIndex: index, Delta: proxyEvent.Delta, Partial: *partial})

		case "thinking_end":
			content.ThoughtSignature = proxyEvent.ContentSignature
			handler(ctx, ai.ThinkingEnd{ContentIndex: index, Content: content.Thinking, ContentSignature: proxyEvent.ContentSignature, Partial: *partial})

		case "toolcall_start":
			*content = ai.ContentBlock{Type: "toolCall", ID: proxyEvent.ID, Name: proxyEvent.ToolName, Arguments: json.RawMessage(`{}`)}
			toolCallJSON[index] = ""
			handler(ctx, ai.ToolCallStart{ContentIndex: index, Partial: *partial})

		case "toolcall_delta":
			previous, active := toolCallJSON[index]
			if !active {
				return fail(fmt.Errorf("proxy: toolcall_delta after toolcall_end at %d", index))
			}
			toolCallJSON[index] = previous + proxyEvent.Delta
			arguments, err := json.Marshal(ai.ParseStreamingJSON(toolCallJSON[index]))
			if err != nil {
				return fail(fmt.Errorf("proxy: encode tool arguments: %w", err))
			}
			content.Arguments = arguments
			handler(ctx, ai.ToolCallDelta{ContentIndex: index, Delta: proxyEvent.Delta, Partial: *partial})

		case "toolcall_end":
			if _, active := toolCallJSON[index]; !active {
				return fail(fmt.Errorf("proxy: repeated toolcall_end at %d", index))
			}
			if call := proxyEvent.ToolCall; call != nil {
				content.ID = call.ID
				content.Name = call.Name
				content.Namespace = call.Namespace
				content.Arguments = call.Arguments
			}
			delete(toolCallJSON, index)
			handler(ctx, ai.ToolCallEnd{
				ContentIndex: index,
				ToolCall:     ai.ToolCall{ID: content.ID, Name: content.Name, Arguments: content.Arguments, Namespace: content.Namespace},
				Partial:      *partial,
			})

		case "done":
			if proxyEvent.Reason != ai.StopReasonStop && proxyEvent.Reason != ai.StopReasonLength && proxyEvent.Reason != ai.StopReasonToolUse {
				return fail(fmt.Errorf("proxy: invalid done reason %q", proxyEvent.Reason))
			}
			partial.ProviderThinkingLevel = proxyEvent.ProviderThinkingLevel
			partial.StopReason = proxyEvent.Reason
			if proxyEvent.Usage != nil {
				partial.Usage = *proxyEvent.Usage
			}

			handler(ctx, ai.Done{Reason: proxyEvent.Reason, Message: *partial})
			return nil

		case "error":
			if proxyEvent.Reason != ai.StopReasonError && proxyEvent.Reason != ai.StopReasonAborted {
				return fail(fmt.Errorf("proxy: invalid error reason %q", proxyEvent.Reason))
			}
			partial.ProviderThinkingLevel = proxyEvent.ProviderThinkingLevel
			partial.StopReason = proxyEvent.Reason
			partial.ErrorMessage = proxyEvent.ErrorMessage
			if proxyEvent.Usage != nil {
				partial.Usage = *proxyEvent.Usage
			}
			handler(ctx, ai.StreamError{
				Reason: string(proxyEvent.Reason),
				Msg:    *partial,
			})
			return nil
		}

		if readErr != nil {
			break
		}
	}

	// A clean EOF without a terminal event means the server dropped the
	// response mid-stream.
	emit(errors.New("connection closed by proxy server before the response completed"))

	return nil
}

// extractSimpleStreamOptions extracts simple-stream options from the provided options.
func extractSimpleStreamOptions(opts []ai.Option) map[string]any {
	simpleOpts := ai.ExtractSimpleStreamOptions(opts)

	options := make(map[string]any)
	if simpleOpts.Env != nil {
		options["env"] = simpleOpts.Env
	}
	if simpleOpts.CacheRetention != "" {
		options["cacheRetention"] = simpleOpts.CacheRetention
	}
	if simpleOpts.SessionID != "" {
		options["sessionId"] = simpleOpts.SessionID
	}
	if len(simpleOpts.Headers) > 0 {
		options["headers"] = simpleOpts.Headers
	}
	if len(simpleOpts.Metadata) > 0 {
		options["metadata"] = simpleOpts.Metadata
	}
	if simpleOpts.MaxRetryDelaySet || simpleOpts.MaxRetryDelayMs > 0 {
		options["maxRetryDelayMs"] = simpleOpts.MaxRetryDelayMs
	}
	if simpleOpts.Temperature != nil {
		options["temperature"] = *simpleOpts.Temperature
	}
	if simpleOpts.MaxTokens > 0 {
		options["maxTokens"] = simpleOpts.MaxTokens
	}
	if simpleOpts.ThinkingBudgets != nil {
		options["thinkingBudgets"] = simpleOpts.ThinkingBudgets
	}
	if simpleOpts.Reasoning != "" {
		options["reasoning"] = simpleOpts.Reasoning
	}
	if len(simpleOpts.SamplingParams) > 0 {
		options["samplingParams"] = simpleOpts.SamplingParams
	}
	if simpleOpts.Transport != "" {
		options["transport"] = simpleOpts.Transport
	}

	return options
}

// proxyEvent represents the raw event from the proxy server.
// Matches pi-mono's ProxyAssistantMessageEvent type (proxy.ts:36-57)
type proxyEvent struct {
	ToolCall              *ai.ToolCall         `json:"toolCall,omitempty"`
	ProviderThinkingLevel string               `json:"providerThinkingLevel,omitempty"`
	Type                  string               `json:"type"`
	ContentIndex          int                  `json:"contentIndex,omitempty"`
	Delta                 string               `json:"delta,omitempty"`
	Content               string               `json:"content,omitempty"`
	ContentSignature      string               `json:"contentSignature,omitempty"` // text/thinking signatures
	ID                    string               `json:"id,omitempty"`
	ToolName              string               `json:"toolName,omitempty"`
	Reason                ai.StopReason        `json:"reason,omitempty"`
	ErrorMessage          string               `json:"errorMessage,omitempty"`
	Usage                 *ai.Usage            `json:"usage,omitempty"`
	Message               *ai.AssistantMessage `json:"message,omitempty"`
}
