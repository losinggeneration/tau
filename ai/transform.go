package ai // import "github.com/losinggeneration/tau/ai"

import (
	"encoding/json"
	"strings"
	"time"
)

// ToolCallIDNormalizer is a function that normalizes a tool call ID for a
// specific provider. Returns the normalized ID, or the original if no
// normalization is needed.
type ToolCallIDNormalizer func(id string, model *Model, source *AssistantMessage) string

// TransformContext transforms a context for a specific model/provider.
// It implements the same behavior as pi-mono's transformMessages.ts.
//
// The transformation includes:
// - Same-model vs cross-model handling for thinking blocks
// - Preserving thought signatures for same-model, stripping for cross-model
// - Synthetic tool result injection for orphaned tool calls
// - Skipping errored/aborted assistant messages
func TransformContext(ctx *Context, model *Model, normalizeToolCallID ToolCallIDNormalizer) (*Context, error) {
	messages := transformMessages(ctx.Messages, model, nil, normalizeToolCallID)

	result := &Context{
		SystemPrompt: ctx.SystemPrompt,
		Messages:     messages,
		Tools:        ctx.Tools,
	}

	return result, nil
}

// transformMessages implements the two-pass transform algorithm from
// pi-mono's transformMessages.ts.
func transformMessages(messages []Message, model *Model, messageModels []ModelInfo, normalizeToolCallID ToolCallIDNormalizer) []Message {
	// Build a map of original tool call IDs to normalized IDs
	toolCallIDMap := make(map[string]string)

	// First pass: transform messages (thinking blocks, tool call ID normalization)
	transformed := make([]Message, 0, len(messages))
	for i, msg := range messages {
		var info *ModelInfo
		if messageModels != nil && i < len(messageModels) {
			info = &messageModels[i]
		}
		transformed = append(transformed, transformMessage(msg, model, info, toolCallIDMap, normalizeToolCallID))
	}

	// Second pass: insert synthetic empty tool results for orphaned tool calls
	return insertSyntheticToolResults(transformed, toolCallIDMap)
}

// transformMessage transforms a single message for the target model.
// This is the first pass of the transform algorithm.
func transformMessage(msg Message, model *Model, info *ModelInfo, toolCallIDMap map[string]string, normalizeToolCallID ToolCallIDNormalizer) Message {
	role := normalizeMessageRole(msg.Role)
	if (role == "user" || role == "toolResult") && !model.SupportsInput(InputImage) {
		if blocks, ok := msg.Content.([]ContentBlock); ok && len(blocks) > 0 {
			placeholder := "(image omitted: model does not support images)"
			if role == "toolResult" {
				placeholder = "(tool image omitted: model does not support images)"
			}
			content := make([]ContentBlock, 0, len(blocks))
			var previousWasPlaceholder bool
			for _, block := range blocks {
				if block.Type == ContentBlockImage {
					if !previousWasPlaceholder {
						content = append(content, ContentBlock{Type: ContentBlockText, Text: placeholder})
					}
					previousWasPlaceholder = true
					continue
				}
				content = append(content, block)
				previousWasPlaceholder = block.Type == ContentBlockText && block.Text == placeholder
			}
			msg.Content = content
		}
	}

	if msg.Role == "user" {
		return msg
	}

	// Handle toolResult messages - normalize toolCallId if we have a mapping
	if msg.Role == "toolResult" || normalizeMessageRole(msg.Role) == "toolResult" {
		if normalizedID, ok := toolCallIDMap[msg.ToolCallID]; ok && normalizedID != msg.ToolCallID {
			msg.ToolCallID = normalizedID
		}
		return msg
	}

	// Assistant messages need transformation check
	if msg.Role == "assistant" {
		return transformAssistantMessage(msg, model, info, toolCallIDMap, normalizeToolCallID)
	}

	return msg
}

// isSameModel checks if an assistant message is from the same model as the target.
func isSameModel(info *ModelInfo, model *Model) bool {
	return info != nil && info.Provider == model.Provider &&
		info.API == model.API && info.Model == model.ID
}

// transformAssistantMessage transforms an assistant message for the target model.
func transformAssistantMessage(msg Message, model *Model, info *ModelInfo, toolCallIDMap map[string]string, normalizeToolCallID ToolCallIDNormalizer) Message {
	if !msg.IsAssistantMessage() {
		return msg
	}

	// Check same model - first from messageModels info, then from message's own metadata
	sameModel := isSameModel(info, model)
	if !sameModel && msg.Provider != "" && msg.API != "" && msg.Model != "" {
		sameModel = isSameModel(&ModelInfo{
			Provider: msg.Provider,
			API:      msg.API,
			Model:    msg.Model,
		}, model)
	}

	assistantMsg, _ := msg.ToAssistantMessage()

	contentBlocks := msg.ContentBlocks()

	transformedContent := make([]ContentBlock, 0, len(contentBlocks))
	for _, block := range contentBlocks {
		transformed := transformContentBlock(block, sameModel, model, &assistantMsg, toolCallIDMap, normalizeToolCallID)
		transformedContent = append(transformedContent, transformed...)
	}

	msg.Content = transformedContent
	return msg
}

// transformContentBlock transforms a single content block for the target model.
func transformContentBlock(block ContentBlock, isSameModel bool, model *Model, assistantMsg *AssistantMessage, toolCallIDMap map[string]string, normalizeToolCallID ToolCallIDNormalizer) []ContentBlock {
	switch block.Type {
	case ContentBlockThinking:
		// Redacted thinking is opaque encrypted content, only valid for the same model.
		// Drop it for cross-model to avoid API errors.
		if block.Redacted {
			if isSameModel {
				return []ContentBlock{block}
			}
			return nil
		}

		// For same model: keep thinking blocks with signatures (needed for replay)
		// even if the thinking text is empty (OpenAI encrypted reasoning)
		if isSameModel && block.ThoughtSignature != "" {
			return []ContentBlock{block}
		}
		// Skip empty thinking blocks, convert others to plain text for cross-model
		thinking := block.Thinking
		if thinking == "" {
			thinking = block.Text
		}
		if strings.TrimSpace(thinking) == "" {
			return nil
		}

		// For cross-model: convert thinking to plain text (drop tags)
		if isSameModel {
			return []ContentBlock{block}
		}
		return []ContentBlock{
			{
				Type: ContentBlockText,
				Text: thinking,
			},
		}

	case ContentBlockText:
		if isSameModel {
			return []ContentBlock{block}
		}
		// For cross-model, strip text signatures
		return []ContentBlock{
			{
				Type: block.Type,
				Text: block.Text,
			},
		}

	case ContentBlockToolCall:
		result := block

		if !isSameModel {
			result.ThoughtSignature = ""
		}

		// Normalize tool call ID if needed for cross-model
		if !isSameModel && normalizeToolCallID != nil && assistantMsg != nil {
			normalizedID := normalizeToolCallID(block.ID, model, assistantMsg)
			if normalizedID != block.ID {
				toolCallIDMap[block.ID] = normalizedID
				result.ID = normalizedID
			}
		}

		return []ContentBlock{result}

	default:
		return []ContentBlock{block}
	}
}

// insertSyntheticToolResults inserts synthetic empty tool results for orphaned
// tool calls. This is the second pass of the transform algorithm.
func insertSyntheticToolResults(messages []Message, toolCallIDMap map[string]string) []Message {
	result := make([]Message, 0, len(messages))
	pendingToolCalls := make([]ContentBlock, 0)
	existingToolResultIDs := make(map[string]bool)
	var heldSystemMessages []Message

	insertSynthetic := func() {
		if len(pendingToolCalls) > 0 {
			for _, tc := range pendingToolCalls {
				if !existingToolResultIDs[tc.ID] {
					result = append(result, Message{
						Role:       "toolResult",
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Content:    []ContentBlock{{Type: "text", Text: "No result provided"}},
						IsError:    true,
						Timestamp:  time.Now().UnixMilli(),
					})
				}
			}
			pendingToolCalls = pendingToolCalls[:0]
			existingToolResultIDs = make(map[string]bool)
		}
		result = append(result, heldSystemMessages...)
		heldSystemMessages = nil
	}

	for _, msg := range messages {
		switch normalizeMessageRole(msg.Role) {
		case "assistant":
			// If we have pending orphaned tool calls from a previous assistant, insert synthetic results now
			insertSynthetic()

			// Skip errored/aborted assistant messages entirely.
			// Skip errored/aborted assistant messages entirely.
			// These are incomplete turns that shouldn't be replayed:
			// - May have partial content (reasoning without message, incomplete tool calls)
			// - Replaying them can cause API errors (e.g., OpenAI "reasoning without following item")
			// - The model should retry from the last valid state
			if msg.IsErrorMessage() {
				continue
			}

			// Track tool calls from this assistant message
			for _, block := range msg.ContentBlocks() {
				if block.Type == ContentBlockToolCall {
					pendingToolCalls = append(pendingToolCalls, block)
				}
			}

			result = append(result, msg)
		case "toolResult", "result":
			// Mark this tool result ID as existing
			existingToolResultIDs[msg.ToolCallID] = true
			result = append(result, msg)

		case "system":
			// Keep real and synthetic results together before applying system updates.
			if len(pendingToolCalls) > 0 {
				heldSystemMessages = append(heldSystemMessages, msg)
			} else {
				result = append(result, msg)
			}

		case "user":
			// User message interrupts tool flow - insert synthetic results for orphaned calls
			insertSynthetic()
			result = append(result, msg)

		default:
			result = append(result, msg)
		}
	}

	// If the transformed context ends immediately after an assistant tool-use
	// message, the model still needs a matching tool result on replay. Pi fills
	// this dangling turn with an error tool result instead of leaving an orphaned
	// tool call for the next provider request to reject.
	insertSynthetic()

	return result
}

// TransformMessages is the exported version of transformMessages for use by providers.
// It transforms a slice of messages for a specific model/provider.
// messageModels provides optional model info for each message (from ProviderInfo).
func TransformMessages(messages []Message, model *Model, messageModels []ModelInfo, normalizeToolCallID ToolCallIDNormalizer) []Message {
	return transformMessages(messages, model, messageModels, normalizeToolCallID)
}

// SerializeContext serializes a context to JSON.
func SerializeContext(ctx *Context) ([]byte, error) {
	return json.Marshal(ctx)
}

// DeserializeContext deserializes a context from JSON.
func DeserializeContext(data []byte) (*Context, error) {
	var ctx Context
	if err := json.Unmarshal(data, &ctx); err != nil {
		return nil, err
	}

	return &ctx, nil
}

// CloneContext copies the context and message slice. Nested content and tool
// declarations are shared and must be treated as immutable.
func CloneContext(ctx *Context) *Context {
	if ctx == nil {
		return nil
	}

	messages := make([]Message, len(ctx.Messages))
	for i, msg := range ctx.Messages {
		messages[i] = msg
		messages[i].Role = normalizeMessageRole(msg.Role)
	}

	return &Context{
		SystemPrompt: ctx.SystemPrompt,
		Messages:     messages,
		Tools:        ctx.Tools,
	}
}

// TruncateContext truncates a context to fit within a model's context window.
// This is a helper function that can be used by providers.
func TruncateContext(ctx *Context, model *Model, maxMessages int) *Context {
	if ctx == nil {
		return nil
	}

	result := &Context{
		SystemPrompt: ctx.SystemPrompt,
		Tools:        ctx.Tools,
	}
	cut := len(ctx.Messages) - min(len(ctx.Messages), max(0, maxMessages))
	for i, message := range ctx.Messages {
		// Prompt and tool declarations remain effective after earlier turns are removed.
		if i >= cut || message.Role == "system" {
			result.Messages = append(result.Messages, message)
		}
	}

	return result
}
