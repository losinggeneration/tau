package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMistralToolMessageConversion(t *testing.T) {
	client := &mistralClient{}

	messages := client.convertMessages(&Context{
		Messages: []Message{
			{
				Role:       "toolResult",
				ToolCallID: "call_1",
				ToolName:   "read",
				Content:    []ContentBlock{{Type: ContentBlockText, Text: "contents"}},
				IsError:    true,
			},
		},
	})

	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	if messages[0]["role"] != "tool" {
		t.Fatalf("role = %v, want tool", messages[0]["role"])
	}
	if messages[0]["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id = %v, want call_1", messages[0]["tool_call_id"])
	}
	if messages[0]["name"] != "read" {
		t.Fatalf("name = %v, want read", messages[0]["name"])
	}
	if messages[0]["content"] != "[tool error] contents" {
		t.Fatalf("content = %v, want [tool error] contents", messages[0]["content"])
	}
}

func TestMistralBuildRequestBodyIncludesSystemPromptAndTools(t *testing.T) {
	client := &mistralClient{}

	body := client.buildRequestBody(&Model{ID: "codestral-latest"}, &Context{
		SystemPrompt: "system",
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: ContentBlockText, Text: "hi"}}},
		},
		Tools: []Tool{
			{Name: "read", Description: "Read", Parameters: nil},
		},
	}, &options{maxTokens: 10})

	if body["model"] != "codestral-latest" {
		t.Fatalf("model = %v, want codestral-latest", body["model"])
	}
	if body["stream"] != true {
		t.Fatalf("stream = %v, want true", body["stream"])
	}
	msgs, ok := body["messages"].([]map[string]any)
	if !ok {
		t.Fatalf("messages type = %T", body["messages"])
	}
	if len(msgs) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(msgs))
	}
	if msgs[0]["role"] != "system" {
		t.Fatalf("messages[0].role = %v, want system", msgs[0]["role"])
	}
	tools, ok := body["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", body["tools"])
	}
}

// P7-MS01: Test toolChoice forwarding
func TestMistralToolChoice(t *testing.T) {
	client := &mistralClient{}

	body := client.buildRequestBody(&Model{ID: "mistral-small-latest"}, &Context{
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
	}, &options{toolChoice: "auto"})

	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto", body["tool_choice"])
	}
}

func TestMistralToolChoiceRequired(t *testing.T) {
	client := &mistralClient{}

	body := client.buildRequestBody(&Model{ID: "mistral-small-latest"}, &Context{
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
	}, &options{toolChoice: "required"})

	if body["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v, want required", body["tool_choice"])
	}
}

func TestMistralToolChoiceNone(t *testing.T) {
	client := &mistralClient{}

	// When toolChoice is empty, should not include tool_choice in body
	body := client.buildRequestBody(&Model{ID: "mistral-small-latest"}, &Context{
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
	}, &options{})

	if _, exists := body["tool_choice"]; exists {
		t.Fatalf("tool_choice should not be present when empty")
	}
}

// P7-MS02: Test promptMode for reasoning models
func TestMistralPromptModeForReasoningModel(t *testing.T) {
	client := &mistralClient{}

	body := client.buildRequestBody(&Model{ID: "mistral-large-latest", Reasoning: true}, &Context{
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
	}, &options{reasoning: ReasoningMedium})

	if body["promptMode"] != "reasoning" {
		t.Fatalf("promptMode = %v, want reasoning", body["promptMode"])
	}
}

func TestMistralNoPromptModeForNonReasoningModel(t *testing.T) {
	client := &mistralClient{}

	body := client.buildRequestBody(&Model{ID: "mistral-small-latest", Reasoning: false}, &Context{
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
	}, &options{reasoning: ReasoningMedium})

	if _, exists := body["promptMode"]; exists {
		t.Fatalf("promptMode should not be present for non-reasoning model")
	}
}

func TestMistralNoPromptModeWhenReasoningOff(t *testing.T) {
	client := &mistralClient{}

	body := client.buildRequestBody(&Model{ID: "mistral-large-latest", Reasoning: true}, &Context{
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
	}, &options{reasoning: ReasoningOff})

	if _, exists := body["promptMode"]; exists {
		t.Fatalf("promptMode should not be present when reasoning is off")
	}
}

// P7-MS07: Test tool call ID normalizer
func TestMistralToolCallIdNormalizer(t *testing.T) {
	normalizer := createMistralToolCallIdNormalizer()

	// Test 1: IDs are normalized to 9 chars or less
	id := normalizer("abc12345")
	if len(id) > mistralToolCallIDLength {
		t.Fatalf("normalized ID length = %d, want max %d", len(id), mistralToolCallIDLength)
	}

	// Test 2: Same input produces same output (deterministic)
	id2 := normalizer("abc12345")
	if id != id2 {
		t.Fatalf("normalizer not deterministic: %s != %s", id, id2)
	}

	// Test 3: Different inputs produce different outputs
	id3 := normalizer("different")
	if id == id3 {
		t.Fatalf("normalizer collision: %s == %s", id, id3)
	}
}

func TestDeriveMistralToolCallId(t *testing.T) {
	// Test exact 9-char ID
	id := deriveMistralToolCallId("123456789", 0)
	if id != "123456789" {
		t.Fatalf("deriveMistralToolCallId(%q, 0) = %q, want %q", "123456789", id, "123456789")
	}

	// Test non-alphanumeric removal
	id = deriveMistralToolCallId("abc-def_ghi", 0)
	if strings.ContainsAny(id, "-_") {
		t.Fatalf("deriveMistralToolCallId should remove non-alphanumeric: got %s", id)
	}

	// Test empty normalized string uses original
	id = deriveMistralToolCallId("---", 0)
	if id == "---" {
		t.Fatalf("deriveMistralToolCallId should hash empty normalized string, got %s", id)
	}
}

// P7-MS10: Test error body truncation
func TestMistralErrorBodyTruncation(t *testing.T) {
	longBody := strings.Repeat("x", 5000)
	expectedLen := maxMistralErrorBodyChars

	truncated := truncateErrorText(longBody, expectedLen)

	if len(truncated) > expectedLen+100 { // Allow overhead for truncation notice
		t.Fatalf("truncateErrorText should truncate to ~maxChars, got length %d", len(truncated))
	}

	if !strings.Contains(truncated, "...") {
		t.Fatalf("truncated text should contain '...'")
	}

	// Short text should not be truncated
	shortBody := "short error"
	if truncateErrorText(shortBody, 4000) != shortBody {
		t.Fatalf("short text should not be truncated")
	}
}

// Test convertMessages with thinking content
func TestMistralConvertMessagesWithThinking(t *testing.T) {
	client := &mistralClient{}

	messages := client.convertMessages(&Context{
		Messages: []Message{
			{
				Role:    "assistant",
				Content: []ContentBlock{{Type: ContentBlockThinking, Thinking: "Let me think..."}},
			},
		},
	})

	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	// Thinking should be extracted as text
	if messages[0]["role"] != "assistant" {
		t.Fatalf("role = %v, want assistant", messages[0]["role"])
	}
}

func TestMistralConvertMessagesWithMultipleContentBlocks(t *testing.T) {
	client := &mistralClient{}

	messages := client.convertMessages(&Context{
		Messages: []Message{
			{
				Role: "assistant",
				Content: []ContentBlock{
					{Type: ContentBlockText, Text: "Hello"},
					{Type: ContentBlockToolCall, ID: "call_1", Name: "test", Arguments: []byte(`{}`)},
				},
			},
		},
	})

	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}

	assistant := messages[0]
	if assistant["role"] != "assistant" {
		t.Fatalf("role = %v, want assistant", assistant["role"])
	}

	// Should have tool_calls
	toolCalls, ok := assistant["tool_calls"].([]map[string]any)
	if !ok {
		t.Fatalf("tool_calls not present")
	}
	if len(toolCalls) != 1 {
		t.Fatalf("len(tool_calls) = %d, want 1", len(toolCalls))
	}
}

// Test strict: false in tool definitions
func TestMistralBuildRequestBodyWithStrictTools(t *testing.T) {
	client := &mistralClient{}

	body := client.buildRequestBody(&Model{ID: "mistral-small-latest"}, &Context{
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
		Tools: []Tool{
			{Name: "test_tool", Description: "A test tool", Parameters: nil},
		},
	}, &options{})

	tools, ok := body["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not present or empty")
	}

	fn, ok := tools[0]["function"].(map[string]any)
	if !ok {
		t.Fatalf("function not present in tool")
	}

	// strict should be false
	if strict, ok := fn["strict"].(bool); !ok || strict != false {
		t.Fatalf("strict = %v, want false", fn["strict"])
	}
}

// Test server error with long body is truncated
func TestMistralStreamWithServerError(t *testing.T) {
	// Create a test server that returns an error with long body
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer server.Close()

	client := &mistralClient{
		model:  &Model{ID: "test-model", Provider: ProviderMistral},
		apiKey: "test-key",
	}

	_, err := client.Complete(context.Background(), &Model{ID: "test", BaseURL: server.URL}, &Context{
		Messages: []Message{{Role: "user", Content: "test"}},
	})

	if err == nil {
		t.Fatal("expected error from server")
	}

	// Error should be truncated
	errMsg := err.Error()
	if len(errMsg) > maxMistralErrorBodyChars+100 { // +100 for prefix overhead
		t.Fatalf("error message too long: %d chars", len(errMsg))
	}
}
