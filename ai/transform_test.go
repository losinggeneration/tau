package ai

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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

func TestCloneContextPreservesTranscriptMetadata(t *testing.T) {
	ctx := &Context{
		SystemPrompt: "base",
		Tools:        []Tool{{Name: "lookup"}},
		Messages: []Message{{
			Role: "system", ID: "baseline", Content: "instructions",
			Sections:   map[string]*string{"policy": new("be careful")},
			ToolsAdded: []Tool{{Name: "search"}}, ToolsRemoved: []ToolReference{{Name: "lookup"}},
		}, {
			Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "answer"}},
			Provider: ProviderAnthropic, API: APIAnthropicMessages, Model: "claude-sonnet-4",
			StopReason: string(StopReasonStop), DurationMs: new(12.5),
			Diagnostics: []AssistantDiagnostic{{Type: "anthropic_strict_tools_fallback"}},
		}},
	}
	cloned := CloneContext(ctx)
	require.Equal(t, ctx, cloned)
	cloned.Messages[0].ID = "changed"
	require.Equal(t, "baseline", ctx.Messages[0].ID)
	require.Nil(t, CloneContext(nil))
}

func TestTruncateContextPreservesTranscriptBaseline(t *testing.T) {
	ctx := &Context{SystemPrompt: "legacy", Tools: []Tool{{Name: "base"}}, Messages: []Message{
		{Role: "system", Content: "instructions", Sections: map[string]*string{"policy": new("careful")}, ToolsAdded: []Tool{{Name: "search"}}},
		{Role: "user", Content: "old"},
		{Role: "system", Sections: map[string]*string{"policy": new("updated")}, ToolsRemoved: []ToolReference{{Name: "base"}}},
		{Role: "user", Content: "latest"},
	}}
	truncated := TruncateContext(ctx, nil, 2)
	want := providerContext(ctx)
	want.Messages = want.Messages[len(want.Messages)-1:]
	require.Equal(t, want, providerContext(truncated))
	require.Equal(t, ctx.Messages[2:], truncated.Messages[1:])
	require.Len(t, ctx.Messages, 4)
	for _, limit := range []int{0, -1} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			got := providerContext(TruncateContext(ctx, nil, limit))
			require.Empty(t, got.Messages)
			require.Equal(t, want.SystemPrompt, got.SystemPrompt)
			require.Equal(t, want.Tools, got.Tools)
		})
	}
}

func TestTransformContextNormalizerReceivesSourceModel(t *testing.T) {
	message := Message{Role: "assistant", Provider: ProviderAnthropic, API: APIAnthropicMessages, Model: "source-model", StopReason: string(StopReasonToolUse), Content: []ContentBlock{{Type: ContentBlockToolCall, ID: "original", Name: "read"}}}
	called := false
	_, err := TransformContext(&Context{Messages: []Message{message}}, &Model{Provider: ProviderOpenAI, API: APIOpenAIResponses, ID: "target-model"}, func(id string, _ *Model, source *AssistantMessage) string {
		called = true
		require.Equal(t, message.Provider, source.Provider)
		require.Equal(t, message.API, source.API)
		require.Equal(t, message.Model, source.Model)
		require.Equal(t, StopReasonToolUse, source.StopReason)
		return id
	})
	require.NoError(t, err)
	require.True(t, called)
}

func TestSerializedContextRetainsReplayableContent(t *testing.T) {
	model := &Model{ID: "local", API: APIOpenAIResponses, Provider: ProviderOpenAI, Input: []InputType{InputText, InputImage}}
	original := &Context{Messages: []Message{
		{Role: "system", Content: []ContentBlock{{Type: ContentBlockText, Text: "instructions"}}, Timestamp: 1700000000000},
		{Role: "user", Content: []ContentBlock{{Type: ContentBlockText, Text: "look"}, {Type: ContentBlockImage, MimeType: "image/png", Data: "aGVsbG8="}}, Timestamp: 1700000000001},
		{Role: "assistant", Provider: model.Provider, API: model.API, Model: model.ID, StopReason: string(StopReasonToolUse), Content: []ContentBlock{
			{Type: ContentBlockText, Text: "checking"},
			{Type: ContentBlockThinking, Thinking: "reasoning", ThoughtSignature: `{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"}`},
			{Type: ContentBlockToolCall, Namespace: new("loaded"), ID: "call_1|fc_1", Name: "read", Arguments: json.RawMessage(`{"id":9007199254740993}`)},
		}, Timestamp: 1700000000002},
		{Role: "toolResult", ToolCallID: "call_1|fc_1", ToolName: "read", Content: []ContentBlock{{Type: ContentBlockText, Text: "result"}}, Timestamp: 1700000000003},
	}}
	data, err := SerializeContext(original)
	require.NoError(t, err)
	decoded, err := DeserializeContext(data)
	require.NoError(t, err)
	t.Run("transform", func(t *testing.T) {
		transformed, err := TransformContext(decoded, model, nil)
		require.NoError(t, err)
		require.Equal(t, original, transformed)
	})
	t.Run("provider replay", func(t *testing.T) {
		want, err := ConvertResponsesMessagesForOpenAI(model, original)
		require.NoError(t, err)
		got, err := ConvertResponsesMessagesForOpenAI(model, decoded)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})
	t.Run("vision headers", func(t *testing.T) {
		require.True(t, HasCopilotVisionInput(decoded.Messages))
	})
	t.Run("token estimation", func(t *testing.T) {
		require.Equal(t, estimateMessageTokens(original.Messages[2]), estimateMessageTokens(decoded.Messages[2]))
	})
	t.Run("cross model", func(t *testing.T) {
		crossModel := *model
		crossModel.ID = "other"
		wantCross, err := TransformContext(original, &crossModel, nil)
		require.NoError(t, err)
		gotCross, err := TransformContext(decoded, &crossModel, nil)
		require.NoError(t, err)
		require.Equal(t, wantCross, gotCross)
	})
	t.Run("orphan tool call", func(t *testing.T) {
		originalOrphan := &Context{Messages: original.Messages[:3]}
		decodedOrphan := &Context{Messages: decoded.Messages[:3]}
		want, err := TransformContext(originalOrphan, model, nil)
		require.NoError(t, err)
		got, err := TransformContext(decodedOrphan, model, nil)
		require.NoError(t, err)
		require.Equal(t, want.Messages[:3], got.Messages[:min(3, len(got.Messages))])
		require.Len(t, got.Messages, 4)
		require.Equal(t, "No result provided", got.Messages[3].ContentBlocks()[0].Text)
		require.True(t, got.Messages[3].IsError)
		require.Equal(t, "call_1|fc_1", got.Messages[3].ToolCallID)
	})
	t.Run("source unchanged", func(t *testing.T) {
		require.Equal(t, original, decoded)
	})
}

func TestTransformMessagesRecognizesLegacyToolResults(t *testing.T) {
	for _, roles := range [][]string{{"toolResult", "toolResult"}, {"tool", "tool"}, {"toolResult", "tool"}} {
		for _, ending := range []string{"end", "user", "assistant"} {
			t.Run(roles[0]+"/"+roles[1]+"/"+ending, func(t *testing.T) {
				model := &Model{ID: "target", Provider: ProviderAnthropic, API: APIAnthropicMessages}
				prompt := &Context{SystemPrompt: "base", Tools: []Tool{{Name: "read"}}, Messages: []Message{
					{Role: "assistant", Content: []ContentBlock{
						{Type: ContentBlockToolCall, ID: "a|foreign", Name: "read"},
						{Type: ContentBlockToolCall, ID: "b|foreign", Name: "list"},
						{Type: ContentBlockToolCall, ID: "missing|foreign", Name: "grep"},
					}},
					{Role: roles[0], ToolCallID: "a|foreign", ToolName: "read", Content: "first", Timestamp: 1234},
					{Role: roles[1], ToolCallID: "b|foreign", ToolName: "list", Content: []ContentBlock{{Type: ContentBlockText, Text: "last"}}, IsError: true, Timestamp: 5678},
				}}
				if ending != "end" {
					prompt.Messages = append(prompt.Messages, Message{Role: ending, Content: []ContentBlock{{Type: ContentBlockText, Text: "next"}}})
				}
				normalizer := func(id string, _ *Model, _ *AssistantMessage) string {
					return strings.ReplaceAll(id, "|", "_")
				}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				for _, api := range []string{"context", "messages"} {
					t.Run(api, func(t *testing.T) {
						var messages []Message
						if api == "context" {
							transformed, err := TransformContext(prompt, model, normalizer)
							require.NoError(t, err)
							require.Equal(t, prompt.SystemPrompt, transformed.SystemPrompt)
							require.Equal(t, prompt.Tools, transformed.Tools)
							messages = transformed.Messages
						} else {
							messages = TransformMessages(prompt.Messages, model, nil, normalizer)
						}
						require.Len(t, messages, len(prompt.Messages)+1)
						for i, id := range []string{"a_foreign", "b_foreign"} {
							want := prompt.Messages[i+1]
							want.ToolCallID = id
							require.Equal(t, want, messages[i+1])
							require.Equal(t, id, messages[0].ContentBlocks()[i].ID)
						}
						synthetic := messages[3]
						require.Equal(t, "toolResult", synthetic.Role)
						require.Equal(t, "missing_foreign", synthetic.ToolCallID)
						require.Equal(t, "grep", synthetic.ToolName)
						require.True(t, synthetic.IsError)
						require.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "No result provided"}}, synthetic.Content)
						if ending != "end" {
							require.Equal(t, prompt.Messages[3], messages[4])
						}
						after, err := json.Marshal(prompt)
						require.NoError(t, err)
						require.Equal(t, string(before), string(after))
					})
				}
			})
		}
	}
}

func TestTransformMessagesHoldsSystemsUntilToolResults(t *testing.T) {
	model := &Model{ID: "claude-test", Provider: ProviderAnthropic, API: APIAnthropicMessages}
	for _, role := range []string{"toolResult", "tool"} {
		for _, boundary := range []string{"end", "user", "assistant", "error", "aborted"} {
			for _, shape := range []string{"answered", "missing", "no calls"} {
				t.Run(role+"/"+boundary+"/"+shape, func(t *testing.T) {
					leading := Message{Role: "system", Content: "base", Timestamp: 1}
					assistant := Message{Role: "assistant", Content: []ContentBlock{
						{Type: ContentBlockToolCall, ID: "a|foreign", Name: "read"},
						{Type: ContentBlockToolCall, ID: "b|foreign", Name: "list"},
					}}
					if shape == "no calls" {
						assistant.Content = []ContentBlock{{Type: ContentBlockText, Text: "done"}}
					}
					systems := []Message{
						{Role: "system", ID: "before", Content: "before results", Timestamp: 2, Sections: map[string]*string{"policy": new("careful")}},
						{Role: "system", ID: "between", Content: "between results", Timestamp: 3, ToolsAdded: []Tool{{Name: "grep"}}},
						{Role: "system", ID: "after", Content: "after results", Timestamp: 4, ToolsRemoved: []ToolReference{{Name: "read"}}},
					}
					first := Message{Role: role, ToolCallID: "a|foreign", ToolName: "read", Content: "first", Timestamp: 5}
					last := Message{Role: role, ToolCallID: "b|foreign", ToolName: "list", Content: "last", IsError: true, Timestamp: 6}
					messages := []Message{leading, assistant, systems[0]}
					if shape != "no calls" {
						messages = append(messages, first)
					}
					messages = append(messages, systems[1])
					if shape == "answered" {
						messages = append(messages, last)
					}
					messages = append(messages, systems[2])
					var next Message
					if boundary != "end" {
						next = Message{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "next"}}}
						switch boundary {
						case "user":
							next.Role = "user"
						case "error", "aborted":
							next.StopReason = boundary
						}
						messages = append(messages, next)
					}
					prompt := &Context{Messages: messages, SystemPrompt: "shorthand", Tools: []Tool{{Name: "read"}}}
					before, err := json.Marshal(prompt)
					require.NoError(t, err)
					normalizer := func(id string, _ *Model, _ *AssistantMessage) string { return strings.ReplaceAll(id, "|", "_") }
					for _, entry := range []string{"context", "messages"} {
						t.Run(entry, func(t *testing.T) {
							var got []Message
							if entry == "context" {
								transformed, err := TransformContext(prompt, model, normalizer)
								require.NoError(t, err)
								require.Equal(t, prompt.SystemPrompt, transformed.SystemPrompt)
								require.Equal(t, prompt.Tools, transformed.Tools)
								got = transformed.Messages
							} else {
								got = TransformMessages(messages, model, nil, normalizer)
							}
							require.Equal(t, leading, got[0])
							index := 2
							if shape != "no calls" {
								calls := got[1].ContentBlocks()
								require.Equal(t, "a_foreign", calls[0].ID)
								require.Equal(t, "b_foreign", calls[1].ID)
								first.ToolCallID = "a_foreign"
								require.Equal(t, first, got[index])
								index++
								if shape == "answered" {
									last.ToolCallID = "b_foreign"
									require.Equal(t, last, got[index])
								} else {
									synthetic := got[index]
									require.Equal(t, "toolResult", synthetic.Role)
									require.Equal(t, "b_foreign", synthetic.ToolCallID)
									require.Equal(t, "list", synthetic.ToolName)
									require.True(t, synthetic.IsError)
									require.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "No result provided"}}, synthetic.Content)
								}
								index++
							}
							require.Equal(t, systems, got[index:index+len(systems)])
							index += len(systems)
							if boundary == "user" || boundary == "assistant" {
								require.Equal(t, next, got[index])
								index++
							}
							require.Len(t, got, index)
							after, err := json.Marshal(prompt)
							require.NoError(t, err)
							require.JSONEq(t, string(before), string(after))
						})
					}
				})
			}
		}
	}
}

func TestTransformMessagesDowngradesUnsupportedImages(t *testing.T) {
	for _, input := range [][]InputType{{InputText}, nil, {InputText, InputImage}} {
		for _, role := range []string{"user", "toolResult", "tool"} {
			t.Run(fmt.Sprint(input)+"/"+role, func(t *testing.T) {
				placeholder := "(image omitted: model does not support images)"
				if role != "user" {
					placeholder = "(tool image omitted: model does not support images)"
				}
				image := ContentBlock{Type: ContentBlockImage, MimeType: "image/png", Data: "aGVsbG8="}
				text := ContentBlock{Type: ContentBlockText, Text: "between", TextSignature: "keep"}
				marker := ContentBlock{Type: ContentBlockText, Text: placeholder}
				marked := marker
				marked.TextSignature = "existing"
				blocks := []ContentBlock{image, image, text, image, image, marked, image, {Type: ContentBlockText}, image}
				wantBlocks := []ContentBlock{marker, text, marker, marked, {Type: ContentBlockText}, marker}
				model := &Model{ID: "target", Input: input}
				if model.SupportsInput(InputImage) {
					wantBlocks = blocks
				}
				messages := []Message{
					{Role: "system", Content: []ContentBlock{image}, Timestamp: 1},
					{Role: "assistant", Content: []ContentBlock{image}, Timestamp: 2},
				}
				want := append([]Message(nil), messages...)
				for _, content := range []any{blocks, []ContentBlock{image, image}, "literal", nil, []ContentBlock{}, []ContentBlock{text}} {
					message := Message{Role: role, Content: content, ID: "message", ToolCallID: "call", ToolName: "read", IsError: true, Timestamp: 9, DurationMs: new(12.0)}
					messages = append(messages, message)
					if contentBlocks, ok := content.([]ContentBlock); ok && len(contentBlocks) > 0 && contentBlocks[0].Type == ContentBlockImage && !model.SupportsInput(InputImage) {
						message.Content = []ContentBlock{marker}
						if len(contentBlocks) > 2 {
							message.Content = wantBlocks
						}
					}
					want = append(want, message)
				}
				prompt := &Context{Messages: messages, SystemPrompt: "shorthand", Tools: []Tool{{Name: "read"}}}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				for _, entry := range []string{"context", "messages"} {
					t.Run(entry, func(t *testing.T) {
						var got []Message
						if entry == "context" {
							transformed, err := TransformContext(prompt, model, nil)
							require.NoError(t, err)
							require.Equal(t, prompt.SystemPrompt, transformed.SystemPrompt)
							require.Equal(t, prompt.Tools, transformed.Tools)
							got = transformed.Messages
						} else {
							got = TransformMessages(messages, model, nil, nil)
						}
						require.Equal(t, want, got)
						repeated := TransformMessages(got, model, nil, nil)
						require.Equal(t, got, repeated)
						after, err := json.Marshal(prompt)
						require.NoError(t, err)
						require.JSONEq(t, string(before), string(after))
					})
				}
			})
		}
	}
}

func TestTransformMessagesDropsUnsignedBlankThinking(t *testing.T) {
	cases := []struct {
		name  string
		block ContentBlock
		text  string
	}{
		{name: "empty", block: ContentBlock{Type: ContentBlockThinking}},
		{name: "spaces", block: ContentBlock{Type: ContentBlockThinking, Thinking: " \t\r\n"}},
		{name: "unicode spaces", block: ContentBlock{Type: ContentBlockThinking, Thinking: "\u00a0\u2003\u2028"}},
		{name: "legacy blank", block: ContentBlock{Type: ContentBlockThinking, Text: " \t\n"}},
		{name: "thinking takes precedence", block: ContentBlock{Type: ContentBlockThinking, Thinking: " \t", Text: "not reasoning"}},
		{name: "nonblank", block: ContentBlock{Type: ContentBlockThinking, Thinking: " \t reason \n"}, text: " \t reason \n"},
		{name: "legacy nonblank", block: ContentBlock{Type: ContentBlockThinking, Text: " \t legacy \n"}, text: " \t legacy \n"},
		{name: "signed empty", block: ContentBlock{Type: ContentBlockThinking, ThoughtSignature: "opaque"}},
		{name: "signed blank", block: ContentBlock{Type: ContentBlockThinking, Thinking: " \t\n", ThoughtSignature: "opaque"}},
		{name: "signed nonblank", block: ContentBlock{Type: ContentBlockThinking, Thinking: " reason ", ThoughtSignature: "opaque"}, text: " reason "},
		{name: "redacted", block: ContentBlock{Type: ContentBlockThinking, Redacted: true, Thinking: "hidden", ThoughtSignature: "encrypted"}},
	}
	model := &Model{Provider: ProviderAnthropic, API: APIAnthropicMessages, ID: "target"}
	for _, tc := range cases {
		for _, source := range []string{"target", "other"} {
			for _, entry := range []string{"context", "messages"} {
				t.Run(tc.name+"/"+source+"/"+entry, func(t *testing.T) {
					tail := ContentBlock{Type: ContentBlockText, Text: "answer"}
					prompt := &Context{SystemPrompt: "base", Messages: []Message{{
						Role: "assistant", Provider: model.Provider, API: model.API, Model: source,
						ID: "retained", Timestamp: 42, StopReason: string(StopReasonStop),
						Content: []ContentBlock{tc.block, tail},
					}}}
					before, err := json.Marshal(prompt)
					require.NoError(t, err)
					want := prompt.Messages[0]
					want.Content = []ContentBlock{tail}
					if source == model.ID && (tc.text != "" || tc.block.ThoughtSignature != "" || tc.block.Redacted) {
						want.Content = []ContentBlock{tc.block, tail}
					} else if source != model.ID && tc.text != "" && !tc.block.Redacted {
						want.Content = []ContentBlock{{Type: ContentBlockText, Text: tc.text}, tail}
					}
					var got []Message
					if entry == "context" {
						transformed, err := TransformContext(prompt, model, nil)
						require.NoError(t, err)
						require.Equal(t, prompt.SystemPrompt, transformed.SystemPrompt)
						got = transformed.Messages
					} else {
						got = TransformMessages(prompt.Messages, model, nil, nil)
					}
					require.Equal(t, []Message{want}, got)
					after, err := json.Marshal(prompt)
					require.NoError(t, err)
					require.JSONEq(t, string(before), string(after))
				})
			}
		}
	}
}

func TestTransformSignedToolCallsKeepNamespaceAcrossModels(t *testing.T) {
	source := &Model{ID: "source", API: APIOpenAIResponses, Provider: ProviderOpenAI}
	block := ContentBlock{Type: ContentBlockToolCall, ID: "call_1", Name: "lookup", Namespace: new("loaded"), Arguments: json.RawMessage(`{"id":9007199254740993}`), ThoughtSignature: "opaque"}
	messages := []Message{
		{Role: "assistant", Provider: source.Provider, API: source.API, Model: source.ID, StopReason: string(StopReasonToolUse), Content: []ContentBlock{block}},
		{Role: "toolResult", ToolCallID: block.ID, ToolName: block.Name, Content: []ContentBlock{{Type: ContentBlockText, Text: "found"}}},
	}
	for _, same := range []bool{true, false} {
		for _, normalize := range []bool{true, false} {
			t.Run(fmt.Sprintf("same=%v/normalize=%v", same, normalize), func(t *testing.T) {
				target := *source
				if !same {
					target.ID = "target"
				}
				var normalizer ToolCallIDNormalizer
				if normalize {
					normalizer = func(id string, _ *Model, _ *AssistantMessage) string { return "normalized_" + id }
				}
				ctx, err := TransformContext(&Context{Messages: messages}, &target, normalizer)
				require.NoError(t, err)
				got := ctx.Messages[0].ContentBlocks()[0]
				want := block
				if !same {
					want.ThoughtSignature = ""
					if normalize {
						want.ID = "normalized_call_1"
					}
				}
				assert.Equal(t, want, got)
				assert.Equal(t, want.ID, ctx.Messages[1].ToolCallID)
				assert.Equal(t, ctx.Messages, TransformMessages(messages, &target, nil, normalizer))
				assert.Equal(t, block, messages[0].ContentBlocks()[0])
			})
		}
	}
}

func TestTransformNormalizerReceivesNativeEffort(t *testing.T) {
	source := Message{Role: "assistant", Provider: ProviderAnthropic, API: APIAnthropicMessages, Model: "source", ProviderThinkingLevel: "high", StopReason: string(StopReasonToolUse), Content: []ContentBlock{{Type: ContentBlockToolCall, ID: "call", Name: "read", Arguments: json.RawMessage(`{}`)}}}
	target := &Model{Provider: ProviderOpenAI, API: APIOpenAICompletions, ID: "target"}
	var calls int
	normalized, err := TransformContext(&Context{Messages: []Message{source, {Role: "toolResult", ToolCallID: "call", Content: []ContentBlock{{Type: ContentBlockText, Text: "ok"}}}}}, target, func(id string, _ *Model, msg *AssistantMessage) string {
		calls++
		assert.Equal(t, "high", msg.ProviderThinkingLevel)
		return id + "_normalized"
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	require.Len(t, normalized.Messages, 2)
	assert.Equal(t, "high", normalized.Messages[0].ProviderThinkingLevel)
	assert.Equal(t, "call_normalized", normalized.Messages[1].ToolCallID)
	assert.Equal(t, "high", source.ProviderThinkingLevel)
}
