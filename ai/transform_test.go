package ai

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransformContext_SameModel_PreservesThinking(t *testing.T) {
	ctx := &Context{
		Messages: []Message{
			{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Hello"},
				},
			},
			{
				Role:     "assistant",
				Provider: ProviderAnthropic,
				API:      APIAnthropicMessages,
				Model:    "claude-sonnet-4-20250514",
				Content: []ContentBlock{
					{
						Type:     ContentBlockThinking,
						Thinking: "Let me think about this",
						Text:     "I'm thinking",
					},
					{
						Type: "text",
						Text: "Hello!",
					},
				},
				StopReason: string(StopReasonStop),
			},
		},
	}

	model := &Model{
		Provider: ProviderAnthropic,
		API:      APIAnthropicMessages,
		ID:       "claude-sonnet-4-20250514",
	}

	result, err := TransformContext(ctx, model, nil)
	require.NoError(t, err)
	require.Len(t, result.Messages, 2)

	// Second message should be assistant
	assistantMsg, ok := result.Messages[1].ToAssistantMessage()
	require.True(t, ok)

	// Should preserve thinking block for same model
	contentBlocks := assistantMsg.Content
	require.True(t, ok)
	assert.Len(t, contentBlocks, 2)
	assert.Equal(t, ContentBlockThinking, contentBlocks[0].Type)
	assert.Equal(t, "Let me think about this", contentBlocks[0].Thinking)
}

func TestTransformContext_CrossModel_ConvertsThinkingToText(t *testing.T) {
	ctx := &Context{
		Messages: []Message{
			{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Hello"},
				},
			},
			{
				Role:     "assistant",
				Provider: ProviderAnthropic,
				API:      APIAnthropicMessages,
				Model:    "claude-sonnet-4-20250514",
				Content: []ContentBlock{
					{
						Type:     ContentBlockThinking,
						Thinking: "Let me think about this",
					},
					{
						Type: "text",
						Text: "Hello!",
					},
				},
				StopReason: string(StopReasonStop),
			},
		},
	}

	// Target model is OpenAI (different provider)
	model := &Model{
		Provider: ProviderOpenAI,
		API:      APIOpenAICompletions,
		ID:       "gpt-4o",
	}

	result, err := TransformContext(ctx, model, nil)
	require.NoError(t, err)
	require.Len(t, result.Messages, 2)

	// Second message should be assistant
	assistantMsg, ok := result.Messages[1].ToAssistantMessage()
	require.True(t, ok)

	// Should convert thinking to text for cross-model
	contentBlocks := assistantMsg.Content
	assert.Len(t, contentBlocks, 2)
	assert.Equal(t, ContentBlockText, contentBlocks[0].Type)
	assert.Equal(t, "Let me think about this", contentBlocks[0].Text)
}

func TestTransformContext_SkipsErroredAssistantMessages(t *testing.T) {
	ctx := &Context{
		Messages: []Message{
			{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Hello"},
				},
			},
			{
				Role:     "assistant",
				Provider: ProviderAnthropic,
				API:      APIAnthropicMessages,
				Model:    "claude-sonnet-4-20250514",
				Content: []ContentBlock{
					{
						Type: "text",
						Text: "Partial response",
					},
				},
				StopReason: string(StopReasonError),
			},
		},
	}

	model := &Model{
		Provider: ProviderAnthropic,
		API:      APIAnthropicMessages,
		ID:       "claude-sonnet-4-20250514",
	}

	result, err := TransformContext(ctx, model, nil)
	require.NoError(t, err)

	// Errored assistant message should be skipped
	require.Len(t, result.Messages, 1)
	assert.Equal(t, "user", result.Messages[0].Role)
}

func TestTransformContext_SkipsAbortedAssistantMessages(t *testing.T) {
	ctx := &Context{
		Messages: []Message{
			{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Hello"},
				},
			},
			{
				Role:     "assistant",
				Provider: ProviderAnthropic,
				API:      APIAnthropicMessages,
				Model:    "claude-sonnet-4-20250514",
				Content: []ContentBlock{
					{
						Type: "text",
						Text: "Partial response",
					},
				},
				StopReason: string(StopReasonAborted),
			},
		},
	}

	model := &Model{
		Provider: ProviderAnthropic,
		API:      APIAnthropicMessages,
		ID:       "claude-sonnet-4-20250514",
	}

	result, err := TransformContext(ctx, model, nil)
	require.NoError(t, err)

	// Aborted assistant message should be skipped
	require.Len(t, result.Messages, 1)
	assert.Equal(t, "user", result.Messages[0].Role)
}

func TestTransformContext_InsertsSyntheticToolResultsForOrphanedToolCalls(t *testing.T) {
	ctx := &Context{
		Messages: []Message{
			{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Use a tool"},
				},
			},
			{
				Role:     "assistant",
				Provider: ProviderAnthropic,
				API:      APIAnthropicMessages,
				Model:    "claude-sonnet-4-20250514",
				Content: []ContentBlock{
					{
						Type: ContentBlockToolCall,
						ID:   "tool_call_1",
						Name: "some_tool",
					},
				},
				StopReason: string(StopReasonToolUse),
			},
			// No tool result for the tool call
			{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Continue"},
				},
			},
		},
	}

	model := &Model{
		Provider: ProviderAnthropic,
		API:      APIAnthropicMessages,
		ID:       "claude-sonnet-4-20250514",
	}

	result, err := TransformContext(ctx, model, nil)
	require.NoError(t, err)

	// Should insert synthetic tool result before the user message
	require.Len(t, result.Messages, 4)
	assert.Equal(t, "user", result.Messages[0].Role)
	assert.Equal(t, "assistant", result.Messages[1].Role)
	assert.Equal(t, "toolResult", result.Messages[2].Role)
	assert.Equal(t, "tool_call_1", result.Messages[2].ToolCallID)
	assert.True(t, result.Messages[2].IsError)
	assert.Equal(t, "user", result.Messages[3].Role)
}

func TestTransformContext_FinalOrphanedToolCallGetsSyntheticErrorResultAtEOF(t *testing.T) {
	ctx := &Context{
		Messages: []Message{
			{
				Role:    "user",
				Content: []ContentBlock{{Type: "text", Text: "Use a tool"}},
			},
			{
				Role:     "assistant",
				Provider: ProviderAnthropic,
				API:      APIAnthropicMessages,
				Model:    "claude-sonnet-4-20250514",
				Content: []ContentBlock{{
					Type: ContentBlockToolCall,
					ID:   "tool_call_eof",
					Name: "some_tool",
				}},
				StopReason: string(StopReasonToolUse),
			},
		},
	}

	result, err := TransformContext(ctx, &Model{
		Provider: ProviderAnthropic,
		API:      APIAnthropicMessages,
		ID:       "claude-sonnet-4-20250514",
	}, nil)
	require.NoError(t, err)
	require.Len(t, result.Messages, 3)

	toolResult := result.Messages[2]
	assert.Equal(t, "toolResult", toolResult.Role)
	assert.Equal(t, "tool_call_eof", toolResult.ToolCallID)
	assert.Equal(t, "some_tool", toolResult.ToolName)
	assert.True(t, toolResult.IsError)
	require.Len(t, toolResult.ContentBlocks(), 1)
	assert.Equal(t, "No result provided", toolResult.ContentBlocks()[0].Text)
}

func TestTransformContext_DoesNotInsertSyntheticWhenToolResultExists(t *testing.T) {
	ctx := &Context{
		Messages: []Message{
			{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Use a tool"},
				},
			},
			{
				Role:     "assistant",
				Provider: ProviderAnthropic,
				API:      APIAnthropicMessages,
				Model:    "claude-sonnet-4-20250514",
				Content: []ContentBlock{
					{
						Type: ContentBlockToolCall,
						ID:   "tool_call_1",
						Name: "some_tool",
					},
				},
				StopReason: string(StopReasonToolUse),
			},
			{
				Role:       "toolResult",
				ToolCallID: "tool_call_1",
				ToolName:   "some_tool",
				Content:    "Tool result",
			},
		},
	}

	model := &Model{
		Provider: ProviderAnthropic,
		API:      APIAnthropicMessages,
		ID:       "claude-sonnet-4-20250514",
	}

	result, err := TransformContext(ctx, model, nil)
	require.NoError(t, err)

	// Should not insert synthetic tool result since real one exists
	require.Len(t, result.Messages, 3)
}

func TestTransformContext_RewritesMatchingToolResultIDAfterToolCallNormalization(t *testing.T) {
	ctx := &Context{Messages: []Message{
		{
			Role: "assistant",
			Content: []ContentBlock{{
				Type: ContentBlockToolCall,
				ID:   "original-tool-call-id",
				Name: "read",
			}},
			Provider:   ProviderAnthropic,
			API:        APIAnthropicMessages,
			Model:      "claude-sonnet-4-20250514",
			StopReason: string(StopReasonToolUse),
		},
		{
			Role:       "toolResult",
			ToolCallID: "original-tool-call-id",
			ToolName:   "read",
			Content:    []ContentBlock{{Type: ContentBlockText, Text: "ok"}},
		},
	}}

	result, err := TransformContext(ctx, &Model{Provider: ProviderOpenAI, API: APIOpenAICompletions, ID: "gpt-4o"}, func(id string, model *Model, source *AssistantMessage) string {
		return "normalized-tool-call-id"
	})
	require.NoError(t, err)
	require.Len(t, result.Messages, 2)

	assistant, ok := result.Messages[0].ToAssistantMessage()
	require.True(t, ok)
	require.Len(t, assistant.Content, 1)
	assert.Equal(t, "normalized-tool-call-id", assistant.Content[0].ID)
	assert.Equal(t, "normalized-tool-call-id", result.Messages[1].ToolCallID)
}

func TestTransformContext_ToolCallIDNormalization(t *testing.T) {
	ctx := &Context{
		Messages: []Message{
			{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Use a tool"},
				},
			},
			{
				Role:     "assistant",
				Provider: ProviderOpenAI,
				API:      APIOpenAIResponses,
				Model:    "gpt-4o",
				Content: []ContentBlock{
					{
						Type: ContentBlockToolCall,
						// 70 chars - needs truncation to 64
						ID:   "very_long_tool_call_id_with_special_chars_12345678901234567890extra",
						Name: "some_tool",
					},
				},
				StopReason: string(StopReasonToolUse),
			},
			{
				Role:       "toolResult",
				ToolCallID: "very_long_tool_call_id_with_special_chars_12345678901234567890extra",
				ToolName:   "some_tool",
				Content:    "Tool result",
			},
		},
	}

	// Target model is Anthropic which has stricter ID requirements
	model := &Model{
		Provider: ProviderAnthropic,
		API:      APIAnthropicMessages,
		ID:       "claude-sonnet-4-20250514",
	}

	// Normalizer that truncates IDs to 64 chars
	normalizer := func(id string, m *Model, source *AssistantMessage) string {
		if len(id) > 64 {
			return id[:64]
		}
		return id
	}

	result, err := TransformContext(ctx, model, normalizer)
	require.NoError(t, err)

	// Should normalize the tool call ID from 70 to 64 chars
	require.Len(t, result.Messages, 3)

	assistantMsg, ok := result.Messages[1].ToAssistantMessage()
	require.True(t, ok)
	contentBlocks := assistantMsg.Content
	assert.Len(t, contentBlocks, 1)
	assert.Equal(t, "very_long_tool_call_id_with_special_chars_12345678901234567890extra"[:64], contentBlocks[0].ID)
}

func TestTransformMessages_EmptyThinkingBlocksSkipped(t *testing.T) {
	ctx := &Context{
		Messages: []Message{
			{
				Role:     "assistant",
				Provider: ProviderAnthropic,
				API:      APIAnthropicMessages,
				Model:    "claude-sonnet-4-20250514",
				Content: []ContentBlock{
					{
						Type: ContentBlockThinking,
						// Empty thinking
					},
					{
						Type: "text",
						Text: "Hello!",
					},
				},
				StopReason: string(StopReasonStop),
			},
		},
	}

	model := &Model{
		Provider: ProviderOpenAI,
		API:      APIOpenAICompletions,
		ID:       "gpt-4o",
	}

	result, err := TransformContext(ctx, model, nil)
	require.NoError(t, err)

	assistantMsg, ok := result.Messages[0].ToAssistantMessage()
	require.True(t, ok)
	contentBlocks := assistantMsg.Content
	require.True(t, ok)

	// Empty thinking should be skipped
	assert.Len(t, contentBlocks, 1)
	assert.Equal(t, ContentBlockText, contentBlocks[0].Type)
}

func TestTransformContext_SameModel_PreservesRedactedThinkingSignature(t *testing.T) {
	ctx := &Context{
		Messages: []Message{{
			Role:     "assistant",
			Provider: ProviderOpenAI,
			API:      APIOpenAIResponses,
			Model:    "gpt-4o",
			Content: []ContentBlock{
				{
					Type:             ContentBlockThinking,
					ThoughtSignature: `{"type":"reasoning.encrypted","id":"rs_1","data":"enc"}`,
					Redacted:         true,
				},
			},
			StopReason: string(StopReasonStop),
		}},
	}

	model := &Model{
		Provider: ProviderOpenAI,
		API:      APIOpenAIResponses,
		ID:       "gpt-4o",
	}

	result, err := TransformContext(ctx, model, nil)
	require.NoError(t, err)
	require.Len(t, result.Messages, 1)

	assistantMsg, ok := result.Messages[0].ToAssistantMessage()
	require.True(t, ok)
	require.Len(t, assistantMsg.Content, 1)
	assert.Equal(t, ContentBlockThinking, assistantMsg.Content[0].Type)
	assert.True(t, assistantMsg.Content[0].Redacted)
	assert.Equal(t, `{"type":"reasoning.encrypted","id":"rs_1","data":"enc"}`, assistantMsg.Content[0].ThoughtSignature)
}

func TestTransformContext_CrossProviderDropsRedactedThinking(t *testing.T) {
	ctx := &Context{Messages: []Message{{
		Role:     "assistant",
		Provider: ProviderOpenAI,
		API:      APIOpenAIResponses,
		Model:    "gpt-4o",
		Content: []ContentBlock{
			{Type: ContentBlockText, Text: "visible"},
			{Type: ContentBlockThinking, ThoughtSignature: `{"type":"reasoning.encrypted","data":"enc"}`, Redacted: true},
		},
		StopReason: string(StopReasonStop),
	}}}

	result, err := TransformContext(ctx, &Model{Provider: ProviderAnthropic, API: APIAnthropicMessages, ID: "claude-sonnet-4-20250514"}, nil)
	require.NoError(t, err)
	require.Len(t, result.Messages, 1)
	assistantMsg, ok := result.Messages[0].ToAssistantMessage()
	require.True(t, ok)
	require.Len(t, assistantMsg.Content, 1)
	assert.Equal(t, ContentBlockText, assistantMsg.Content[0].Type)
	assert.Equal(t, "visible", assistantMsg.Content[0].Text)
}

func TestTransformContext_SkippedErroredAssistantDoesNotCreateSyntheticToolResult(t *testing.T) {
	ctx := &Context{Messages: []Message{
		{Role: "user", Content: []ContentBlock{{Type: ContentBlockText, Text: "call tool"}}},
		{
			Role:       "assistant",
			Provider:   ProviderAnthropic,
			API:        APIAnthropicMessages,
			Model:      "claude-sonnet-4-20250514",
			Content:    []ContentBlock{{Type: ContentBlockToolCall, ID: "errored_call", Name: "read"}},
			StopReason: string(StopReasonError),
		},
		{Role: "user", Content: []ContentBlock{{Type: ContentBlockText, Text: "retry"}}},
	}}

	result, err := TransformContext(ctx, &Model{Provider: ProviderAnthropic, API: APIAnthropicMessages, ID: "claude-sonnet-4-20250514"}, nil)
	require.NoError(t, err)
	require.Len(t, result.Messages, 2)
	assert.Equal(t, "user", result.Messages[0].Role)
	assert.Equal(t, "user", result.Messages[1].Role)
}

func TestTransformContext_UserMessagesUnchanged(t *testing.T) {
	ctx := &Context{
		Messages: []Message{
			{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Hello world"},
				},
			},
		},
	}

	model := &Model{
		Provider: ProviderAnthropic,
		API:      APIAnthropicMessages,
		ID:       "claude-sonnet-4-20250514",
	}

	result, err := TransformContext(ctx, model, nil)
	require.NoError(t, err)

	require.Len(t, result.Messages, 1)
	assert.Equal(t, "user", result.Messages[0].Role)
	content, ok := result.Messages[0].Content.([]ContentBlock)
	require.True(t, ok)
	assert.Equal(t, "Hello world", content[0].Text)
}

func TestTransformContext_SystemPromptPreserved(t *testing.T) {
	ctx := &Context{
		SystemPrompt: "You are a helpful assistant",
		Messages: []Message{
			{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Hello"},
				},
			},
		},
	}

	model := &Model{
		Provider: ProviderAnthropic,
		API:      APIAnthropicMessages,
		ID:       "claude-sonnet-4-20250514",
	}

	result, err := TransformContext(ctx, model, nil)
	require.NoError(t, err)

	assert.Equal(t, "You are a helpful assistant", result.SystemPrompt)
}

func TestTransformContext_ToolsPreserved(t *testing.T) {
	ctx := &Context{
		Messages: []Message{
			{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "Hello"},
				},
			},
		},
		Tools: []Tool{
			{
				Name:        "test_tool",
				Description: "A test tool",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			},
		},
	}

	model := &Model{
		Provider: ProviderAnthropic,
		API:      APIAnthropicMessages,
		ID:       "claude-sonnet-4-20250514",
	}

	result, err := TransformContext(ctx, model, nil)
	require.NoError(t, err)

	require.Len(t, result.Tools, 1)
	assert.Equal(t, "test_tool", result.Tools[0].Name)
}
