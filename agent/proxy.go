package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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
	Reasoning string
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
		return fmt.Errorf("proxy: marshal request: %w", err)
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, streamURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return fmt.Errorf("proxy: create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")

	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	// Execute request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("proxy: request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("proxy: status %d", resp.StatusCode)
		}

		// Emit a terminal StreamError event before returning the error.
		// This matches the TypeScript reference behavior where non-OK responses
		// surface as terminal assistant error events rather than raw errors.
		errMsg := ai.AssistantMessage{
			Role:         "assistant",
			StopReason:   ai.StopReasonError,
			ErrorMessage: fmt.Sprintf("proxy: status %d: %s", resp.StatusCode, string(body)),
			Timestamp:    time.Now().UnixMilli(),
			API:          model.API,
			Provider:     model.Provider,
			Model:        model.ID,
		}
		handler(ctx, ai.StreamError{
			Reason: string(ai.StopReasonError),
			Msg:    errMsg,
		})

		return fmt.Errorf("proxy: status %d: %s", resp.StatusCode, string(body))
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
		defer cancel()

		go func() {
			select {
			case <-options.Signal:
				cancel()
			case <-runCtx.Done():
			}
		}()
		ctx = runCtx
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
		StopReason: ai.StopReasonStop,
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

	currentToolCall := (*ai.ToolCall)(nil)
	toolCallPartialJSON := "" // Tracks partial JSON for streaming tool call arguments

	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}

		if err != nil {
			return fmt.Errorf("proxy: read: %w", err)
		}

		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		var proxyEvent proxyEvent
		if err := json.Unmarshal([]byte(data), &proxyEvent); err != nil {
			// Treat malformed JSON as a protocol error. This matches the TypeScript
			// reference behavior which throws on malformed data: lines.
			handler(ctx, ai.StreamError{
				Reason: string(ai.StopReasonError),
				Msg:    *partial,
			})
			return fmt.Errorf("proxy: malformed event: %w", err)
		}

		switch proxyEvent.Type {
		case "start":
			if proxyEvent.Message != nil {
				partial = proxyEvent.Message
			}

			handler(ctx, ai.Start{Partial: *partial})

		case "text_start":
			partial.Content = append(partial.Content, ai.ContentBlock{
				Type: "text",
			})
			handler(ctx, ai.TextStart{ContentIndex: proxyEvent.ContentIndex, Partial: *partial})

		case "text_delta":
			if len(partial.Content) > proxyEvent.ContentIndex {
				partial.Content[proxyEvent.ContentIndex].Text += proxyEvent.Delta
			}

			handler(ctx, ai.TextDelta{
				ContentIndex: proxyEvent.ContentIndex,
				Delta:        proxyEvent.Delta,
				Partial:      *partial,
			})

		case "text_end":
			content := ""
			if len(partial.Content) > proxyEvent.ContentIndex {
				content = partial.Content[proxyEvent.ContentIndex].Text
				// Update text signature for replay verification (pi-mono: proxy.ts:240)
				partial.Content[proxyEvent.ContentIndex].TextSignature = proxyEvent.ContentSignature
			}
			handler(ctx, ai.TextEnd{
				ContentIndex:     proxyEvent.ContentIndex,
				Content:          content,
				ContentSignature: proxyEvent.ContentSignature, // pi-mono parity
				Partial:          *partial,
			})

		case "thinking_start":
			partial.Content = append(partial.Content, ai.ContentBlock{
				Type: "thinking",
			})
			handler(ctx, ai.ThinkingStart{ContentIndex: proxyEvent.ContentIndex, Partial: *partial})

		case "thinking_delta":
			if len(partial.Content) > proxyEvent.ContentIndex {
				partial.Content[proxyEvent.ContentIndex].Thinking += proxyEvent.Delta
			}

			handler(ctx, ai.ThinkingDelta{
				ContentIndex: proxyEvent.ContentIndex,
				Delta:        proxyEvent.Delta,
				Partial:      *partial,
			})

		case "thinking_end":
			content := ""
			if len(partial.Content) > proxyEvent.ContentIndex {
				content = partial.Content[proxyEvent.ContentIndex].Thinking
				// Update thinking signature for replay verification (pi-mono: proxy.ts:272)
				partial.Content[proxyEvent.ContentIndex].ThoughtSignature = proxyEvent.ContentSignature
			}
			handler(ctx, ai.ThinkingEnd{
				ContentIndex:     proxyEvent.ContentIndex,
				Content:          content,
				ContentSignature: proxyEvent.ContentSignature, // pi-mono parity
				Partial:          *partial,
			})

		case "toolcall_start":
			currentToolCall = &ai.ToolCall{
				ID:        proxyEvent.ID,
				Name:      proxyEvent.ToolName,
				Arguments: json.RawMessage(`{}`),
			}
			toolCallPartialJSON = "" // Reset partial JSON tracking
			// Initialize the content block (pi-mono: proxy.ts:283-290)
			if len(partial.Content) > proxyEvent.ContentIndex {
				partial.Content[proxyEvent.ContentIndex] = ai.ContentBlock{
					Type: "toolCall",
					ID:   proxyEvent.ID,
					Name: proxyEvent.ToolName,
				}
			} else {
				partial.Content = append(partial.Content, ai.ContentBlock{
					Type:      "toolCall",
					ID:        proxyEvent.ID,
					Name:      proxyEvent.ToolName,
					Arguments: json.RawMessage(`{}`),
				})
			}
			handler(ctx, ai.ToolCallStart{ContentIndex: proxyEvent.ContentIndex, Partial: *partial})
		case "toolcall_delta":
			if currentToolCall != nil {
				// Accumulate partial JSON for streaming tool call arguments (pi-mono: proxy.ts:293-298)
				toolCallPartialJSON += proxyEvent.Delta
				// Use partial JSON parsing for streaming arguments
				parsedArgs := ai.ParseStreamingJSON(toolCallPartialJSON)
				argsJSON, err := json.Marshal(parsedArgs)
				if err == nil {
					currentToolCall.Arguments = argsJSON
					if len(partial.Content) > proxyEvent.ContentIndex {
						partial.Content[proxyEvent.ContentIndex].Arguments = argsJSON
					}
				}
			}

			handler(ctx, ai.ToolCallDelta{
				ContentIndex: proxyEvent.ContentIndex,
				Delta:        proxyEvent.Delta,
				Partial:      *partial,
			})

		case "toolcall_end":
			if currentToolCall != nil {
				// Update the content block with final arguments
				if len(partial.Content) > proxyEvent.ContentIndex {
					partial.Content[proxyEvent.ContentIndex].ID = currentToolCall.ID
					partial.Content[proxyEvent.ContentIndex].Name = currentToolCall.Name
					partial.Content[proxyEvent.ContentIndex].Arguments = currentToolCall.Arguments
				}
			}

			if currentToolCall != nil {
				handler(ctx, ai.ToolCallEnd{
					ContentIndex: proxyEvent.ContentIndex,
					ToolCall:     *currentToolCall,
					Partial:      *partial,
				})
			}

			currentToolCall = nil
			toolCallPartialJSON = "" // Clear partial JSON tracking

		case "done":
			partial.StopReason = proxyEvent.Reason
			if proxyEvent.Usage != nil {
				partial.Usage = *proxyEvent.Usage
			}

			handler(ctx, ai.Done{Reason: proxyEvent.Reason, Message: *partial})

		case "error":
			partial.StopReason = proxyEvent.Reason
			partial.ErrorMessage = proxyEvent.ErrorMessage
			if proxyEvent.Usage != nil {
				partial.Usage = *proxyEvent.Usage
			}
			handler(ctx, ai.StreamError{
				Reason: string(proxyEvent.Reason),
				Msg:    *partial,
			})
		}
	}

	return nil
}

// extractSimpleStreamOptions extracts simple-stream options from the provided options.
func extractSimpleStreamOptions(opts []ai.Option) map[string]any {
	simpleOpts := ai.ExtractSimpleStreamOptions(opts)

	options := make(map[string]any)
	if simpleOpts.Temperature != nil {
		options["temperature"] = *simpleOpts.Temperature
	}
	if simpleOpts.MaxTokens > 0 {
		options["maxTokens"] = simpleOpts.MaxTokens
	}
	if simpleOpts.Reasoning != "" {
		options["reasoning"] = simpleOpts.Reasoning
	}

	return options
}

// proxyEvent represents the raw event from the proxy server.
// Matches pi-mono's ProxyAssistantMessageEvent type (proxy.ts:36-57)
type proxyEvent struct {
	Type             string               `json:"type"`
	ContentIndex     int                  `json:"contentIndex,omitempty"`
	Delta            string               `json:"delta,omitempty"`
	Content          string               `json:"content,omitempty"`
	ContentSignature string               `json:"contentSignature,omitempty"` // pi-mono parity: for text/thinking signatures
	ID               string               `json:"id,omitempty"`
	ToolName         string               `json:"toolName,omitempty"`
	Reason           ai.StopReason        `json:"reason,omitempty"`
	ErrorMessage     string               `json:"errorMessage,omitempty"`
	Usage            *ai.Usage            `json:"usage,omitempty"`
	Message          *ai.AssistantMessage `json:"message,omitempty"`
}
