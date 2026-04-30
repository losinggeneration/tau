package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestModelSupportsInput(t *testing.T) {
	model := &Model{
		ID:            "test-model",
		Input:         []InputType{InputText, InputImage},
		Output:        []OutputType{OutputText},
		ContextWindow: 128000,
	}

	if !model.SupportsInput(InputText) {
		t.Error("model should support text input")
	}

	if !model.SupportsInput(InputImage) {
		t.Error("model should support image input")
	}

	if model.SupportsInput(InputAudio) {
		t.Error("model should not support audio input")
	}
}

func TestModelSupportsOutput(t *testing.T) {
	model := &Model{
		ID:            "test-model",
		Input:         []InputType{InputText},
		Output:        []OutputType{OutputText},
		ContextWindow: 128000,
	}

	if !model.SupportsOutput(OutputText) {
		t.Error("model should support text output")
	}

	if model.SupportsOutput(OutputAudio) {
		t.Error("model should not support audio output")
	}
}

func TestCostProfile(t *testing.T) {
	cost := CostProfile{
		Input:  0.5, // $0.50 per million tokens
		Output: 1.5, // $1.50 per million tokens
	}

	usage := Usage{
		Input:  1000,
		Output: 500,
	}

	usage.CalculateCost(cost)

	expectedInputCost := 0.5 / 1_000_000 * 1000 // $0.0005
	expectedOutputCost := 1.5 / 1_000_000 * 500 // $0.00075

	if usage.Cost.Input != expectedInputCost {
		t.Errorf("input cost = %v, want %v", usage.Cost.Input, expectedInputCost)
	}

	if usage.Cost.Output != expectedOutputCost {
		t.Errorf("output cost = %v, want %v", usage.Cost.Output, expectedOutputCost)
	}

	if usage.TotalTokens != 1500 {
		t.Errorf("total tokens = %d, want %d", usage.TotalTokens, 1500)
	}

	expectedTotal := expectedInputCost + expectedOutputCost
	if usage.Cost.Total != expectedTotal {
		t.Errorf("total cost = %v, want %v", usage.Cost.Total, expectedTotal)
	}
}

func TestCostProfileWithCache(t *testing.T) {
	usage := Usage{
		Input:      1000,
		Output:     500,
		CacheRead:  200,
		CacheWrite: 300,
	}

	usage.CalculateCost(CostProfile{
		Input:      1,
		Output:     2,
		CacheRead:  0.5,
		CacheWrite: 0.75,
	})

	if usage.TotalTokens != 2000 {
		t.Fatalf("total tokens = %d, want 2000", usage.TotalTokens)
	}

	if usage.Cost.CacheRead == 0 {
		t.Fatal("expected cache read cost to be populated")
	}

	if usage.Cost.CacheWrite == 0 {
		t.Fatal("expected cache write cost to be populated")
	}
}

func TestPublicTypesJSONRoundTripStableShape(t *testing.T) {
	model := Model{ID: "gpt-test", Name: "GPT Test", API: APIOpenAIResponses, Provider: ProviderOpenAI, Reasoning: true, Input: []InputType{InputText, InputImage}, Output: []OutputType{OutputText}, Cost: CostProfile{Input: 1, Output: 2, CacheRead: 0.5, CacheWrite: 0.75}, ContextWindow: 128000, MaxTokens: 32000}
	ctx := Context{SystemPrompt: "system", Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: ContentBlockText, Text: "hello"}}, Timestamp: 1700000000000}}, Tools: []Tool{{Name: "read", Description: "Read", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	msg := Message{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockToolCall, ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"path":"a.txt"}`)}}, Timestamp: 1700000000001, Provider: ProviderOpenAI, API: APIOpenAIResponses, Model: "gpt-test", StopReason: string(StopReasonToolUse)}
	assistant := AssistantMessage{Role: "assistant", Provider: ProviderOpenAI, API: APIOpenAIResponses, Model: "gpt-test", Content: []ContentBlock{{Type: ContentBlockText, Text: "ok", TextSignature: "sig"}}, StopReason: StopReasonStop, Usage: Usage{Input: 10, Output: 5, CacheRead: 2, CacheWrite: 3, TotalTokens: 20, Cost: Cost{Input: 0.1, Output: 0.2, CacheRead: 0.03, CacheWrite: 0.04, Total: 0.37}}, Timestamp: 1700000000002, ResponseID: "resp_1"}
	block := ContentBlock{Type: ContentBlockThinking, Thinking: "think", ThoughtSignature: "sig", Redacted: true}
	toolCall := ToolCall{ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"path":"a.txt"}`)}
	usage := Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, TotalTokens: 10, Cost: Cost{Input: 0.1, Output: 0.2, CacheRead: 0.3, CacheWrite: 0.4, Total: 1.0}}
	cost := Cost{Input: 0.1, Output: 0.2, CacheRead: 0.3, CacheWrite: 0.4, Total: 1.0}

	roundTripStableJSON(t, model, &Model{})
	roundTripStableJSON(t, ctx, &Context{})
	roundTripStableJSON(t, msg, &Message{})
	roundTripStableJSON(t, assistant, &AssistantMessage{})
	roundTripStableJSON(t, block, &ContentBlock{})
	roundTripStableJSON(t, toolCall, &ToolCall{})
	roundTripStableJSON(t, usage, &Usage{})
	roundTripStableJSON(t, cost, &Cost{})
}

func roundTripStableJSON[T any](t *testing.T, input T, output *T) {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal %T: %v", input, err)
	}
	if err := json.Unmarshal(data, output); err != nil {
		t.Fatalf("unmarshal %T: %v", input, err)
	}
	again, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("remarshal %T: %v", input, err)
	}
	var first, second any
	requireJSON(t, data, &first)
	requireJSON(t, again, &second)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("round-trip JSON changed for %T:\nfirst:  %s\nsecond: %s", input, data, again)
	}
}

func requireJSON(t *testing.T, data []byte, target *any) {
	t.Helper()
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("unmarshal normalized JSON: %v", err)
	}
}

func TestContextSerialization(t *testing.T) {
	ctx := &Context{
		SystemPrompt: "You are a helpful assistant.",
		Messages: []Message{
			{
				Role:    "user",
				Content: "Hello, world!",
			},
		},
		Tools: []Tool{
			{
				Name:        "read",
				Description: "Read a file",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
			},
		},
	}

	// Verify JSON serialization works
	data, err := json.Marshal(ctx)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// Verify deserialization
	var decoded Context
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if decoded.SystemPrompt != ctx.SystemPrompt {
		t.Errorf("SystemPrompt = %q, want %q", decoded.SystemPrompt, ctx.SystemPrompt)
	}

	if len(decoded.Messages) != len(ctx.Messages) {
		t.Errorf("Messages length = %d, want %d", len(decoded.Messages), len(ctx.Messages))
	}

	if len(decoded.Tools) != len(ctx.Tools) {
		t.Errorf("Tools length = %d, want %d", len(decoded.Tools), len(ctx.Tools))
	}
}

func TestProviderResponseJSON(t *testing.T) {
	resp := ProviderResponse{
		Status: 200,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded ProviderResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if decoded.Status != 200 {
		t.Fatalf("status = %d, want 200", decoded.Status)
	}
	if decoded.Headers["Content-Type"] != "application/json" {
		t.Fatalf("content-type = %q", decoded.Headers["Content-Type"])
	}
}

func TestMessageTypes(t *testing.T) {
	msg := Message{
		Role:       "user",
		Content:    "test content",
		ToolCallID: "",
		ToolName:   "",
		IsError:    false,
		Timestamp:  1234567890,
	}

	if msg.Role != "user" {
		t.Errorf("Role = %q, want %q", msg.Role, "user")
	}

	content, ok := msg.Content.(string)
	if !ok {
		t.Fatalf("Content should be string")
	}

	if content != "test content" {
		t.Errorf("Content = %q, want %q", content, "test content")
	}
}

func TestDeserializeContextNormalizesLegacyToolRole(t *testing.T) {
	data := []byte(`{
		"messages": [
			{
				"role": "tool",
				"toolCallId": "call_1",
				"toolName": "read",
				"content": "output"
			}
		]
	}`)

	ctx, err := DeserializeContext(data)
	if err != nil {
		t.Fatalf("DeserializeContext: %v", err)
	}

	if len(ctx.Messages) != 1 {
		t.Fatalf("len(ctx.Messages) = %d, want 1", len(ctx.Messages))
	}

	if ctx.Messages[0].Role != "toolResult" {
		t.Fatalf("ctx.Messages[0].Role = %q, want toolResult", ctx.Messages[0].Role)
	}
}

func TestDeserializeContextNormalizesSecondTimestampsToMilliseconds(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	data := []byte(fmt.Sprintf(`{
		"messages": [
			{
				"role": "user",
				"content": "hello",
				"timestamp": %d
			}
		]
	}`, now.Unix()))

	ctx, err := DeserializeContext(data)
	if err != nil {
		t.Fatalf("DeserializeContext: %v", err)
	}

	if len(ctx.Messages) != 1 {
		t.Fatalf("len(ctx.Messages) = %d, want 1", len(ctx.Messages))
	}

	if ctx.Messages[0].Timestamp != now.UnixMilli() {
		t.Fatalf("ctx.Messages[0].Timestamp = %d, want %d", ctx.Messages[0].Timestamp, now.UnixMilli())
	}
}

func TestAssistantMessageJSONNormalizesSecondTimestampsToMilliseconds(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	data := []byte(fmt.Sprintf(`{
		"role": "assistant",
		"content": [],
		"stopReason": "stop",
		"usage": {},
		"timestamp": %d
	}`, now.Unix()))

	var msg AssistantMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if msg.Timestamp != now.UnixMilli() {
		t.Fatalf("msg.Timestamp = %d, want %d", msg.Timestamp, now.UnixMilli())
	}
}

func TestUsageJSONPreservesCompleteStableShapeAtZeroValues(t *testing.T) {
	data, err := json.Marshal(&Usage{})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	for _, field := range []string{"input", "output", "cacheRead", "cacheWrite", "totalTokens", "cost"} {
		if _, ok := got[field]; !ok {
			t.Fatalf("usage field %q missing from %s", field, string(data))
		}
	}

	cost, ok := got["cost"].(map[string]any)
	if !ok {
		t.Fatalf("cost type = %T, want object", got["cost"])
	}

	for _, field := range []string{"input", "output", "cacheRead", "cacheWrite", "total"} {
		if _, ok := cost[field]; !ok {
			t.Fatalf("cost field %q missing from %s", field, string(data))
		}
	}
}

func TestAssistantMessageJSONDerivesUsageTotals(t *testing.T) {
	data := []byte(`{
		"role": "assistant",
		"content": [],
		"stopReason": "stop",
		"usage": {
			"input": 7,
			"output": 3,
			"cacheRead": 2,
			"cacheWrite": 1,
			"cost": {
				"input": 0.11,
				"output": 0.22,
				"cacheRead": 0.03,
				"cacheWrite": 0.04
			}
		}
	}`)

	var msg AssistantMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if msg.Usage.TotalTokens != 13 {
		t.Fatalf("msg.Usage.TotalTokens = %d, want 13", msg.Usage.TotalTokens)
	}

	if msg.Usage.Cost.Total != 0.40 {
		t.Fatalf("msg.Usage.Cost.Total = %v, want 0.4", msg.Usage.Cost.Total)
	}
}

func TestContentBlock(t *testing.T) {
	block := ContentBlock{
		Type: ContentBlockText,
		Text: "Hello, world!",
	}

	if block.Type != ContentBlockText {
		t.Errorf("Type = %q, want %q", block.Type, ContentBlockText)
	}

	if block.Text != "Hello, world!" {
		t.Errorf("Text = %q, want %q", block.Text, "Hello, world!")
	}
}

func TestToolCall(t *testing.T) {
	tc := ToolCall{
		ID:        "call_123",
		Name:      "read_file",
		Arguments: json.RawMessage(`{"path":"/tmp/test.txt"}`),
	}

	if tc.ID != "call_123" {
		t.Errorf("ID = %q, want %q", tc.ID, "call_123")
	}

	if tc.Name != "read_file" {
		t.Errorf("Name = %q, want %q", tc.Name, "read_file")
	}

	var args map[string]any
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("Failed to unmarshal arguments: %v", err)
	}

	if args["path"] != "/tmp/test.txt" {
		t.Errorf("path = %v, want %q", args["path"], "/tmp/test.txt")
	}
}

func TestStopReasons(t *testing.T) {
	tests := []struct {
		reason   StopReason
		expected string
	}{
		{StopReasonStop, "stop"},
		{StopReasonLength, "length"},
		{StopReasonToolUse, "toolUse"},
		{StopReasonError, "error"},
		{StopReasonAborted, "aborted"},
	}

	for _, tt := range tests {
		if string(tt.reason) != tt.expected {
			t.Errorf("StopReason = %q, want %q", tt.reason, tt.expected)
		}
	}
}

func TestReasoningLevels(t *testing.T) {
	tests := []struct {
		level    ReasoningLevel
		expected string
	}{
		{ReasoningOff, "off"},
		{ReasoningMinimal, "minimal"},
		{ReasoningLow, "low"},
		{ReasoningMedium, "medium"},
		{ReasoningHigh, "high"},
		{ReasoningXHigh, "xhigh"},
	}

	for _, tt := range tests {
		if string(tt.level) != tt.expected {
			t.Errorf("ReasoningLevel = %q, want %q", tt.level, tt.expected)
		}
	}
}

func TestProviders(t *testing.T) {
	providers := []string{
		ProviderOpenAI,
		ProviderAnthropic,
		ProviderGoogle,
		ProviderMistral,
		ProviderGroq,
		ProviderCerebras,
		ProviderxAI,
		ProviderOpenRouter,
	}

	for _, p := range providers {
		if p == "" {
			t.Error("Provider should not be empty")
		}
	}
}

func TestStreamEventTypes(t *testing.T) {
	// Verify all stream event types implement the interface
	var (
		_ StreamEvent = TextStart{ContentIndex: 0}
		_ StreamEvent = TextDelta{ContentIndex: 0, Delta: "test"}
		_ StreamEvent = TextEnd{ContentIndex: 0, Content: "test"}
		_ StreamEvent = ThinkingStart{ContentIndex: 0}
		_ StreamEvent = ThinkingDelta{ContentIndex: 0, Delta: "thinking"}
		_ StreamEvent = ThinkingEnd{ContentIndex: 0, Content: "thought"}
		_ StreamEvent = ToolCallStart{ContentIndex: 0}
		_ StreamEvent = ToolCallEnd{ToolCall: ToolCall{ID: "t1", Name: "test", Arguments: nil}}
		_ StreamEvent = Done{Reason: StopReasonStop, Message: AssistantMessage{}}
	)
}

func TestStreamErrorImplementsError(t *testing.T) {
	err := StreamError{
		Reason: "test error",
		Err:    context.DeadlineExceeded,
	}

	if err.Error() != context.DeadlineExceeded.Error() {
		t.Errorf("Error() = %q, want %q", err.Error(), context.DeadlineExceeded.Error())
	}

	unwrapped := err.Unwrap()
	if !errors.Is(unwrapped, context.DeadlineExceeded) {
		t.Error("Unwrap() should return the underlying error")
	}
}

func TestDetectModelCompatGLM53OpenCodeGo(t *testing.T) {
	// GLM-5.3 on opencode-go always engages thinking and rejects an empty
	// reasoning_effort as an attempt to disable it, so the compat surface
	// must not claim reasoning_effort support.
	model := &Model{
		ID:       "glm-5.3",
		API:      APIOpenAICompletions,
		Provider: ProviderOpenCodeGo,
		BaseURL:  "https://opencode.ai/zen/go/v1",
	}

	if compat := model.GetCompat(); compat.SupportsReasoningEffort {
		t.Error("glm-5.3 on opencode-go should not support reasoning_effort")
	}

	// The registered model must resolve through the same defense.
	registered, err := GetAvailableModel(ProviderOpenCodeGo, "glm-5.3")
	if err != nil {
		t.Fatalf("glm-5.3 missing from registry: %v", err)
	}
	if compat := registered.GetCompat(); compat.SupportsReasoningEffort {
		t.Error("registered glm-5.3 should not support reasoning_effort")
	}

	// Other opencode-go models keep reasoning_effort support.
	other := &Model{
		ID:       "glm-5.2",
		API:      APIOpenAICompletions,
		Provider: ProviderOpenCodeGo,
		BaseURL:  "https://opencode.ai/zen/go/v1",
	}
	if compat := other.GetCompat(); !compat.SupportsReasoningEffort {
		t.Error("non-glm-5.3 opencode-go models should support reasoning_effort")
	}
}
