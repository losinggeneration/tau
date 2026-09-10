package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// =============================================================================
// Shared Responses Stream Types
// =============================================================================

// ResponsesStreamOptions contains shared options for Responses-family providers.
type ResponsesStreamOptions struct {
	// ServiceTier is the requested service tier for the API.
	ServiceTier string
	// ApplyServiceTierPricing applies pricing multipliers based on service tier.
	// If nil, the default 1.0 multiplier is applied.
	ApplyServiceTierPricing func(usage *Usage, tier string)
	// ResolveServiceTier resolves the effective service tier from response and request tiers.
	// If nil, defaults to using response tier or falling back to opts.ServiceTier.
	ResolveServiceTier func(responseTier, requestTier string) string
}

// responsesStreamReader reads SSE data for Responses API.
// newResponsesStreamReader creates an SSE reader for the Responses API family.
// It previously buffered the entire body with io.ReadAll before yielding a
// single line — and discarded the read error while doing so — so Responses
// providers delivered every event only after the stream had finished.
func newResponsesStreamReader(body io.Reader) *SSEReader {
	return NewSSEReader(body)
}

// =============================================================================
// Responses Stream Events
// =============================================================================

// responsesEvent is the base event type from the Responses API.
type responsesEvent struct {
	Type     string                `json:"type"`
	Code     string                `json:"code,omitempty"`
	Message  string                `json:"message,omitempty"`
	Error    responsesError        `json:"error,omitempty"`
	Delta    string                `json:"delta,omitempty"`
	Response responsesResponseBody `json:"response,omitempty"`
	Item     responsesItem         `json:"item,omitempty"`
	Part     responsesPart         `json:"part,omitempty"`
}

// responsesResponseBody contains response-level data.
type responsesResponseBody struct {
	ID          string              `json:"id,omitempty"`
	Status      string              `json:"status,omitempty"`
	Usage       *responsesUsage     `json:"usage,omitempty"`
	Error       responsesError      `json:"error,omitempty"`
	ServiceTier string              `json:"service_tier,omitempty"`
	Incomplete  responsesIncomplete `json:"incomplete_details,omitempty"`
}

type responsesError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type responsesIncomplete struct {
	Reason string `json:"reason,omitempty"`
}

type responsesUsage struct {
	InputTokens        int                         `json:"input_tokens,omitempty"`
	OutputTokens       int                         `json:"output_tokens,omitempty"`
	TotalTokens        int                         `json:"total_tokens,omitempty"`
	InputTokensDetails responsesInputTokensDetails `json:"input_tokens_details,omitempty"`
}

type responsesInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type responsesItem struct {
	Type      string             `json:"type,omitempty"`
	ID        string             `json:"id,omitempty"`
	CallID    string             `json:"call_id,omitempty"`
	Name      string             `json:"name,omitempty"`
	Arguments string             `json:"arguments,omitempty"`
	Content   []responsesPart    `json:"content,omitempty"`
	Summary   []responsesSummary `json:"summary,omitempty"`
	Status    string             `json:"status,omitempty"`
	Phase     string             `json:"phase,omitempty"`
}

type responsesPart struct {
	Type    string `json:"type,omitempty"`
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

type responsesSummary struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// =============================================================================
// Shared Stream Processing
// =============================================================================

// ProcessResponsesStream processes the Responses API SSE stream and emits events.
// This is shared by OpenAI Responses, Azure Responses, and Codex.
func ProcessResponsesStream(
	ctx context.Context,
	reader *SSEReader,
	output *AssistantMessage,
	handler EventHandler,
	model *Model,
	opts *ResponsesStreamOptions,
) error {
	handler(ctx, Start{Partial: *output})

	var currentItem *responsesItem
	var partialJson string

	// Track content indices for each block type
	currentTextIndex := -1
	currentThinkingIndex := -1
	currentToolIndex := -1

	for {
		data, ok := reader.Next()
		if !ok {
			if err := reader.Error(); err != nil {
				streamErr := fmt.Errorf("responses: read: %w", err)
				emitTerminalStreamError(ctx, handler, *output, streamErr, StopReasonError)
				return nil
			}

			break
		}

		var event responsesEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "response.created":
			output.ResponseID = event.Response.ID

		case "response.output_item.added":
			currentItem = &event.Item

			switch event.Item.Type {
			case "reasoning":
				output.Content = append(output.Content, ContentBlock{
					Type:     ContentBlockThinking,
					Thinking: "",
				})
				currentThinkingIndex = len(output.Content) - 1
				currentTextIndex = -1
				currentToolIndex = -1
				handler(ctx, ThinkingStart{ContentIndex: currentThinkingIndex, Partial: *output})

			case "message":
				output.Content = append(output.Content, ContentBlock{
					Type: ContentBlockText,
				})
				currentTextIndex = len(output.Content) - 1
				currentThinkingIndex = -1
				currentToolIndex = -1
				handler(ctx, TextStart{ContentIndex: currentTextIndex, Partial: *output})

			case "function_call":
				output.Content = append(output.Content, ContentBlock{
					Type:      ContentBlockToolCall,
					ID:        toolCallID(event.Item.CallID, event.Item.ID),
					Name:      event.Item.Name,
					Arguments: json.RawMessage(event.Item.Arguments),
				})
				currentToolIndex = len(output.Content) - 1
				currentTextIndex = -1
				currentThinkingIndex = -1
				partialJson = event.Item.Arguments
				handler(ctx, ToolCallStart{ContentIndex: currentToolIndex, Partial: *output})
			}

		case "response.reasoning_summary_text.delta":
			if currentThinkingIndex >= 0 && currentThinkingIndex < len(output.Content) {
				output.Content[currentThinkingIndex].Thinking += event.Delta
				handler(ctx, ThinkingDelta{ContentIndex: currentThinkingIndex, Delta: event.Delta, Partial: *output})
			}

		case "response.reasoning_summary_part.done":
			if currentThinkingIndex >= 0 && currentThinkingIndex < len(output.Content) {
				output.Content[currentThinkingIndex].Thinking += "\n\n"
				handler(ctx, ThinkingDelta{ContentIndex: currentThinkingIndex, Delta: "\n\n", Partial: *output})
			}

		case "response.reasoning_summary_part.added":
			// Aggregate summary parts for later use in reasoning item
			if currentItem != nil && currentItem.Type == "reasoning" {
				if currentItem.Summary == nil {
					currentItem.Summary = []responsesSummary{}
				}
				// Also accumulate from event.Item.Summary if present
				if len(event.Item.Summary) > 0 {
					currentItem.Summary = append(currentItem.Summary, event.Item.Summary...)
				}
			}
			// Also handle event.Part for reasoning_summary_part.added
			if event.Part.Text != "" {
				if currentItem != nil && currentItem.Type == "reasoning" {
					if currentItem.Summary == nil {
						currentItem.Summary = []responsesSummary{}
					}
					currentItem.Summary = append(currentItem.Summary, responsesSummary{Type: "summary", Text: event.Part.Text})
				}
			}

		case "response.content_part.added":
			// Handle content part additions
			if currentTextIndex >= 0 && currentTextIndex < len(output.Content) {
				// Accumulate text/refusal from part
				if event.Part.Type == "output_text" && event.Part.Text != "" {
					output.Content[currentTextIndex].Text += event.Part.Text
				}
				// refusal is handled via response.refusal.delta
			}

		case "response.output_text.delta", "response.refusal.delta":
			if currentTextIndex >= 0 && currentTextIndex < len(output.Content) {
				output.Content[currentTextIndex].Text += event.Delta
				handler(ctx, TextDelta{ContentIndex: currentTextIndex, Delta: event.Delta, Partial: *output})
			}

		case "response.function_call_arguments.delta":
			if currentToolIndex >= 0 && currentToolIndex < len(output.Content) {
				partialJson += event.Delta
				args := mustMarshalStreamingJSON(ParseStreamingJSON(partialJson))
				output.Content[currentToolIndex].Arguments = args
				handler(ctx, ToolCallDelta{ContentIndex: currentToolIndex, Delta: event.Delta, Partial: *output})
			}

		case "response.function_call_arguments.done":
			if currentToolIndex >= 0 && currentToolIndex < len(output.Content) {
				previousPartial := partialJson
				newArgs := event.Item.Arguments
				partialJson = newArgs
				args := mustMarshalStreamingJSON(ParseStreamingJSON(newArgs))
				output.Content[currentToolIndex].Arguments = args

				// Emit delta for the new content
				if strings.HasPrefix(newArgs, previousPartial) {
					if delta := newArgs[len(previousPartial):]; delta != "" {
						handler(ctx, ToolCallDelta{ContentIndex: currentToolIndex, Delta: delta, Partial: *output})
					}
				}
			}

		case "response.output_item.done":
			item := event.Item
			// Preserve accumulated summary for reasoning items before reassigning currentItem
			var accumulatedSummary []responsesSummary
			if currentItem != nil && currentItem.Type == "reasoning" && len(currentItem.Summary) > 0 {
				accumulatedSummary = currentItem.Summary
			}
			// Update currentItem for non-reasoning items (reasoning items keep the accumulated state)
			if item.Type != "reasoning" {
				currentItem = &item
			}
			switch item.Type {
			case "message":
				if currentTextIndex >= 0 && currentTextIndex < len(output.Content) {
					// Extract text from content parts, including refusal
					var text strings.Builder
					// First add any text accumulated from deltas/content_parts
					text.WriteString(output.Content[currentTextIndex].Text)
					// Then add any refusal text from content parts
					// Only add refusal if it wasn't already captured via refusal.delta
					for _, part := range item.Content {
						if part.Type == "refusal" && part.Refusal != "" {
							// Check if this refusal is already at the end of accumulated text
							if !strings.HasSuffix(text.String(), part.Refusal) {
								text.WriteString(part.Refusal)
							}
						}
					}
					output.Content[currentTextIndex].Text = text.String()

					// Build text signature from item ID and phase
					if item.ID != "" {
						output.Content[currentTextIndex].TextSignature = encodeTextSignatureV1(item.ID, item.Phase)
					}

					handler(ctx, TextEnd{ContentIndex: currentTextIndex, Content: text.String(), Partial: *output})
					currentTextIndex = -1
				}

			case "reasoning":
				if currentThinkingIndex >= 0 && currentThinkingIndex < len(output.Content) {
					// Use accumulated summary from reasoning_summary_part events, or event.Item summary
					summarySource := accumulatedSummary
					if len(summarySource) == 0 {
						summarySource = item.Summary
					}

					// Build thinking text from summary
					var thinking strings.Builder
					for _, s := range summarySource {
						if s.Text != "" {
							if thinking.Len() > 0 {
								thinking.WriteString("\n\n")
							}
							thinking.WriteString(s.Text)
						}
					}
					output.Content[currentThinkingIndex].Thinking = thinking.String()

					// Store reasoning signature using the done event's item (has complete summary)
					if len(summarySource) > 0 {
						sigBytes, _ := json.Marshal(&item)
						output.Content[currentThinkingIndex].ThoughtSignature = string(sigBytes)
					}

					handler(ctx, ThinkingEnd{ContentIndex: currentThinkingIndex, Content: thinking.String(), Partial: *output})
					currentThinkingIndex = -1
				}

			case "function_call":
				if currentToolIndex >= 0 && currentToolIndex < len(output.Content) {
					if item.Arguments != "" {
						args := mustMarshalStreamingJSON(ParseStreamingJSON(item.Arguments))
						output.Content[currentToolIndex].Arguments = args
					}
					tc := ToolCall{
						ID:        output.Content[currentToolIndex].ID,
						Name:      output.Content[currentToolIndex].Name,
						Arguments: output.Content[currentToolIndex].Arguments,
					}
					handler(ctx, ToolCallEnd{ContentIndex: currentToolIndex, ToolCall: tc, Partial: *output})
					currentToolIndex = -1
				}
			}
			currentItem = nil
		case "response.completed":
			if event.Response.ID != "" {
				output.ResponseID = event.Response.ID
			}

			if event.Response.Usage != nil {
				cachedTokens := event.Response.Usage.InputTokensDetails.CachedTokens
				output.Usage = Usage{
					Input:       event.Response.Usage.InputTokens - cachedTokens,
					Output:      event.Response.Usage.OutputTokens,
					CacheRead:   cachedTokens,
					CacheWrite:  0,
					TotalTokens: event.Response.Usage.TotalTokens,
				}
				output.Usage.CalculateCost(model.Cost)

				// Apply service tier pricing if configured
				if opts != nil {
					// Resolve effective service tier
					tier := event.Response.ServiceTier
					if opts.ResolveServiceTier != nil {
						tier = opts.ResolveServiceTier(tier, opts.ServiceTier)
					} else if tier == "" && opts.ServiceTier != "" {
						tier = opts.ServiceTier
					}

					if opts.ApplyServiceTierPricing != nil {
						opts.ApplyServiceTierPricing(&output.Usage, tier)
					}
				}
				handler(ctx, &output.Usage)
			}

			// Map status to stop reason
			output.StopReason = mapResponsesStopReason(event.Response.Status)
			if hasToolCalls(output.Content) && output.StopReason == StopReasonStop {
				output.StopReason = StopReasonToolUse
			}

		case "response.failed":
			msg := buildResponseErrorMessage(event.Response)
			streamErr := fmt.Errorf("responses: %s", msg)
			if event.Response.ID != "" {
				output.ResponseID = event.Response.ID
			}
			output.ErrorMessage = msg
			emitTerminalStreamError(ctx, handler, *output, streamErr, StopReasonError)
			return nil

		case "error":
			msg := buildResponsesErrorEventMessage(event, data)
			streamErr := fmt.Errorf("responses: %s", msg)
			output.ErrorMessage = msg
			emitTerminalStreamError(ctx, handler, *output, streamErr, StopReasonError)
			return nil
		}
	}

	handler(ctx, Done{Reason: output.StopReason, Message: *output})

	return nil
}

// toolCallID builds a composite tool call ID from call_id and item id.
func toolCallID(callID, itemID string) string {
	if callID == "" {
		return itemID
	}
	if itemID == "" {
		return callID
	}
	return callID + "|" + itemID
}

// mapResponsesStopReason maps API status to our StopReason.
func mapResponsesStopReason(status string) StopReason {
	switch status {
	case "completed":
		return StopReasonStop
	case "incomplete":
		return StopReasonLength
	case "cancelled":
		return StopReasonAborted
	case "failed":
		return StopReasonError
	default:
		return StopReasonStop
	}
}

// buildResponseErrorMessage constructs an error message from response error details.
func buildResponseErrorMessage(resp responsesResponseBody) string {
	if resp.Error.Message != "" {
		if resp.Error.Code != "" {
			return resp.Error.Code + ": " + resp.Error.Message
		}
		return resp.Error.Message
	}
	if resp.Incomplete.Reason != "" {
		return "incomplete: " + resp.Incomplete.Reason
	}
	return "unknown error"
}

func buildResponsesErrorEventMessage(event responsesEvent, raw string) string {
	code := event.Code
	message := event.Message
	if code == "" {
		code = event.Error.Code
	}
	if message == "" {
		message = event.Error.Message
	}

	if code != "" && message != "" {
		return "error code " + code + ": " + message
	}
	if code != "" {
		return "error code " + code
	}
	if message != "" {
		return message
	}
	if raw != "" {
		return "error event with empty code/message: " + raw
	}
	return "error event with empty code/message"
}

// hasToolCalls returns true if the content has any tool call blocks.
func hasToolCalls(content []ContentBlock) bool {
	for _, block := range content {
		if block.Type == ContentBlockToolCall {
			return true
		}
	}
	return false
}

// ParseStreamingJSON parses a streaming JSON string that may be partial.
func ParseStreamingJSON(partial string) map[string]any {
	result, ok := ExtractValidJSON(partial)
	if !ok || result == nil {
		return map[string]any{}
	}
	return result
}

func mustMarshalStreamingJSON(data map[string]any) json.RawMessage {
	encoded, err := json.Marshal(data)
	if err != nil {
		return json.RawMessage("{}")
	}
	return encoded
}

// =============================================================================
// Service Tier Pricing
// =============================================================================

// ServiceTierPricing returns pricing multipliers for OpenAI Responses service tiers.
//   - "flex": 0.5x cost
//   - "priority": 2x cost
//   - default/nil: 1x cost
func ServiceTierPricing(usage *Usage, tier string) {
	if usage == nil {
		return
	}

	switch tier {
	case "flex":
		// 50% discount on all costs
		usage.Cost.Input *= 0.5
		usage.Cost.Output *= 0.5
		usage.Cost.CacheRead *= 0.5
		usage.Cost.CacheWrite *= 0.5
		usage.Cost.Total *= 0.5

	case "priority":
		// 2x cost multiplier
		usage.Cost.Input *= 2
		usage.Cost.Output *= 2
		usage.Cost.CacheRead *= 2
		usage.Cost.CacheWrite *= 2
		usage.Cost.Total *= 2

	default:
		// No change for default tier or unknown tier
	}
}

// =============================================================================
// Codex-specific helpers
// =============================================================================

// ResolveCodexServiceTier implements Codex's service tier resolution logic.
// If the response returns "default" but we requested "flex" or "priority",
// use the requested tier. Otherwise use the response tier.
func ResolveCodexServiceTier(responseTier, requestTier string) string {
	if responseTier == "default" && (requestTier == "flex" || requestTier == "priority") {
		return requestTier
	}
	if responseTier != "" {
		return responseTier
	}
	return requestTier
}
