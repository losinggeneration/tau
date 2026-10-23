package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
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

	body := requireBody(t)(client.buildRequestBody(&Model{ID: "codestral-latest"}, &Context{
		SystemPrompt: "system",
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: ContentBlockText, Text: "hi"}}},
		},
		Tools: []Tool{
			{Name: "read", Description: "Read", Parameters: nil},
		},
	}, &options{maxTokens: 10}))

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

	body := requireBody(t)(client.buildRequestBody(&Model{ID: "mistral-small-latest"}, &Context{
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
	}, &options{toolChoice: "auto"}))

	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto", body["tool_choice"])
	}
}

func TestMistralToolChoiceRequired(t *testing.T) {
	client := &mistralClient{}

	body := requireBody(t)(client.buildRequestBody(&Model{ID: "mistral-small-latest"}, &Context{
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
	}, &options{toolChoice: "required"}))

	if body["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v, want required", body["tool_choice"])
	}
}

func TestMistralToolChoiceNone(t *testing.T) {
	client := &mistralClient{}

	// When toolChoice is empty, should not include tool_choice in body
	body := requireBody(t)(client.buildRequestBody(&Model{ID: "mistral-small-latest"}, &Context{
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
	}, &options{}))

	if _, exists := body["tool_choice"]; exists {
		t.Fatalf("tool_choice should not be present when empty")
	}
}

// P7-MS02: Test promptMode for reasoning models
func TestMistralReasoningFieldsMatchUpstream(t *testing.T) {
	str := func(value string) *string { return &value }
	mapped := map[string]*string{"off": str("none"), "low": nil, "high": str("high")}
	tests := []struct {
		name      string
		model     *Model
		reasoning ReasoningLevel
		want      map[string]any
	}{
		{"prompt mode", &Model{ID: "magistral", Reasoning: true}, ReasoningMedium, map[string]any{"prompt_mode": "reasoning"}},
		{"prompt mode unset", &Model{ID: "magistral", Reasoning: true}, "", map[string]any{}},
		{"prompt mode off", &Model{ID: "magistral", Reasoning: true}, ReasoningOff, map[string]any{}},
		{"non-reasoning", &Model{ID: "mistral-small"}, ReasoningMedium, map[string]any{}},
		{"effort map", &Model{ID: "mistral-medium", Reasoning: true, ThinkingLevelMap: mapped}, ReasoningHigh, map[string]any{"reasoning_effort": "high"}},
		{"effort map clamps", &Model{ID: "mistral-medium", Reasoning: true, ThinkingLevelMap: mapped}, ReasoningLow, map[string]any{"reasoning_effort": "high"}},
		{"effort map off", &Model{ID: "mistral-medium", Reasoning: true, ThinkingLevelMap: mapped}, ReasoningOff, map[string]any{"reasoning_effort": "none"}},
	}
	for _, tt := range tests {
		body := requireBody(t)((&mistralClient{}).buildRequestBody(tt.model, &Context{Messages: []Message{{Role: "user", Content: "hello"}}}, &options{reasoning: tt.reasoning}))
		got := map[string]any{}
		for _, key := range []string{"prompt_mode", "reasoning_effort", "promptMode"} {
			if value, ok := body[key]; ok {
				got[key] = value
			}
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: fields = %v, want %v", tt.name, got, tt.want)
		}
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

	body := requireBody(t)(client.buildRequestBody(&Model{ID: "mistral-small-latest"}, &Context{
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
		Tools: []Tool{
			{Name: "test_tool", Description: "A test tool", Parameters: nil},
		},
	}, &options{}))

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

func TestMistralFinishErrorsAreTerminal(t *testing.T) {
	for _, reason := range []string{"error", "unexpected"} {
		var terminal StreamEvent
		client := &mistralClient{}
		data := `data: {"choices":[{"finish_reason":"` + reason + `"}]}` + "\n\ndata: [DONE]\n\n"
		if err := client.streamMistralResponse(context.Background(), &Model{ID: "test"}, strings.NewReader(data), func(_ context.Context, event StreamEvent) { terminal = event }); err != nil {
			t.Fatal(err)
		}
		failure, ok := terminal.(StreamError)
		if !ok || !strings.Contains(failure.Msg.ErrorMessage, reason) {
			t.Fatalf("finish %s produced %#v", reason, terminal)
		}
	}
}

func TestMistralHeaderTimeoutDoesNotLimitBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/slow/") {
			select {
			case <-r.Context().Done():
			case <-time.After(100 * time.Millisecond):
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(30 * time.Millisecond)
		if _, err := io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"); err != nil {
			return
		}
	}))
	defer server.Close()
	client := &mistralClient{apiKey: "key"}
	var final StreamEvent
	err := client.Stream(context.Background(), &Model{ID: "test", BaseURL: server.URL}, &Context{}, func(_ context.Context, event StreamEvent) { final = event }, WithTimeout(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := final.(Done); !ok {
		t.Fatalf("body timed out: %#v", final)
	}
	err = client.Stream(context.Background(), &Model{ID: "test", BaseURL: server.URL + "/slow"}, &Context{}, func(context.Context, StreamEvent) {}, WithTimeout(20*time.Millisecond))
	if err == nil {
		t.Fatal("missing response headers did not time out")
	}
}

func TestMistralTimeoutFinishAndCancellationOutcomes(t *testing.T) {
	stall := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow/v1/chat/completions":
			select {
			case <-r.Context().Done():
			case <-time.After(200 * time.Millisecond):
			}
		case "/quota/v1/chat/completions":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"message":"quota exceeded"}`)
		case "/unfinished/v1/chat/completions":
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-stall:
			case <-r.Context().Done():
			case <-time.After(200 * time.Millisecond):
			}
		}
	}))
	defer server.Close()
	defer close(stall)
	client := &mistralClient{apiKey: "key"}
	prompt := &Context{Messages: []Message{{Role: "user", Content: "hi"}}}

	err := client.Stream(t.Context(), &Model{ID: "m", BaseURL: server.URL + "/slow"}, prompt, func(context.Context, StreamEvent) {}, WithTimeout(20*time.Millisecond))
	if err == nil || errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "mistral response headers timed out after 20ms") {
		t.Fatalf("header timeout err = %v; it must not look like a user abort", err)
	}
	if !IsRetryableAssistantError(&AssistantMessage{StopReason: StopReasonError, ErrorMessage: err.Error()}) {
		t.Fatalf("header timeout is not retryable: %v", err)
	}

	err = client.Stream(t.Context(), &Model{ID: "m", BaseURL: server.URL + "/quota"}, prompt, func(context.Context, StreamEvent) {})
	if err == nil || !strings.Contains(err.Error(), `mistral API error (429): {"message":"quota exceeded"}`) ||
		IsRetryableAssistantError(&AssistantMessage{StopReason: StopReasonError, ErrorMessage: err.Error()}) {
		t.Fatalf("quota err = %v; quota exhaustion must not retry", err)
	}

	var terminal StreamEvent
	if err := client.Stream(t.Context(), &Model{ID: "m", BaseURL: server.URL + "/unfinished"}, prompt, func(_ context.Context, event StreamEvent) { terminal = event }); err != nil {
		t.Fatal(err)
	}
	if failure, ok := terminal.(StreamError); !ok || failure.Msg.StopReason != StopReasonError || failure.Msg.ErrorMessage != "mistral stream ended without a finish reason" {
		t.Fatalf("unfinished stream terminal = %#v", terminal)
	}

	ctx, cancel := context.WithCancel(t.Context())
	terminal = nil
	if err := client.Stream(ctx, &Model{ID: "m", BaseURL: server.URL}, prompt, func(_ context.Context, event StreamEvent) {
		if _, ok := event.(TextDelta); ok {
			cancel()
		}
		terminal = event
	}); err != nil {
		t.Fatal(err)
	}
	if failure, ok := terminal.(StreamError); !ok || failure.Msg.StopReason != StopReasonAborted || len(failure.Msg.Content) == 0 {
		t.Fatalf("cancelled stream terminal = %#v", terminal)
	}
}

func TestMistralStrictToolsMatchUpstream(t *testing.T) {
	client := &mistralClient{}
	prompt := &Context{Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"require"}`)}}}
	body := requireBody(t)(client.buildRequestBody(&Model{ID: "mistral-large-latest"}, prompt, &options{}))
	function := body["tools"].([]map[string]any)[0]["function"].(map[string]any)
	if function["strict"] != true || !strings.Contains(string(function["parameters"].(json.RawMessage)), `"additionalProperties":false`) {
		t.Fatalf("function = %v", function)
	}
	prompt.Tools[0].Parameters = json.RawMessage(`{"type":"object","additionalProperties":true}`)
	if _, err := client.buildRequestBody(&Model{ID: "mistral-large-latest"}, prompt, &options{}); err == nil {
		t.Fatal("required unsupported schema accepted")
	}
}

func TestMistralExplicitZeroDisablesHeaderTimeout(t *testing.T) {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		timer := time.NewTimer(time.Millisecond)
		defer timer.Stop()
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-timer.C:
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	var final StreamEvent
	err := (&mistralClient{apiKey: "key"}).Stream(t.Context(), &Model{ID: "test", BaseURL: "https://test.invalid"}, &Context{}, func(_ context.Context, event StreamEvent) { final = event }, WithTimeout(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := final.(Done); !ok {
		t.Fatalf("terminal=%#v", final)
	}
}
