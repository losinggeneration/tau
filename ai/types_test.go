package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
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

func TestCalculateCostSelectsHighestInputTier(t *testing.T) {
	profile := CostProfile{Input: 1, Output: 2, CacheRead: 0.5, CacheWrite: 1, Tiers: []CostTier{
		{InputTokensAbove: 200, Input: 4, Output: 8, CacheRead: 2, CacheWrite: 4},
		{InputTokensAbove: 100, Input: 2, Output: 4, CacheRead: 1, CacheWrite: 2},
	}}
	for _, tt := range []struct {
		input int
		want  float64
	}{{100, 0.00012}, {101, 0.000242}, {201, 0.000884}} {
		usage := Usage{Input: tt.input, Output: 10}
		usage.CalculateCost(profile)
		if math.Abs(usage.Cost.Total-tt.want) > 1e-9 {
			t.Fatalf("input=%d cost=%g want=%g", tt.input, usage.Cost.Total, tt.want)
		}
	}
	usage := Usage{Input: 50, CacheRead: 50, CacheWrite: 1, Output: 10}
	usage.CalculateCost(profile)
	if math.Abs(usage.Cost.Total-0.000192) > 1e-9 {
		t.Fatalf("cached input did not select tier: %+v", usage.Cost)
	}
	usage.CacheRead = 0
	usage.CacheWrite = 0
	usage.CalculateCost(profile)
	if usage.Cost.CacheRead != 0 || usage.Cost.CacheWrite != 0 {
		t.Fatalf("stale cache costs: %+v", usage.Cost)
	}
}

func TestToAssistantMessagePreservesSourceMetadata(t *testing.T) {
	msg := Message{
		Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "ok"}},
		Provider: ProviderAnthropic, API: APIAnthropicMessages, Model: "claude-sonnet-4",
		StopReason: string(StopReasonStop), Timestamp: 1740000000000, DurationMs: new(12.5),
		Diagnostics:           []AssistantDiagnostic{{Type: "strict_fallback"}},
		ProviderThinkingLevel: "high",
	}
	got, ok := msg.ToAssistantMessage()
	want := AssistantMessage{
		Role: msg.Role, Content: msg.ContentBlocks(), Provider: msg.Provider, API: msg.API,
		Model: msg.Model, StopReason: StopReasonStop, Timestamp: msg.Timestamp,
		DurationMs: msg.DurationMs, Diagnostics: msg.Diagnostics,
		ProviderThinkingLevel: msg.ProviderThinkingLevel,
	}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("ToAssistantMessage = %#v, %v; want %#v, true", got, ok, want)
	}
}

func TestCatalogCompatExplicitFalseOverridesDetection(t *testing.T) {
	model := &Model{ID: "m", API: APIOpenAICompletions, Provider: ProviderOpenAI, BaseURL: "https://api.openai.com/v1", Compat: &CompatibilitySettings{SupportsStore: new(false), SupportsDeveloperRole: new(false), SupportsReasoningEffort: new(false), SupportsStrictMode: new(false)}}
	compat := model.GetCompat()
	if compat.SupportsStore || compat.SupportsDeveloperRole || compat.SupportsReasoningEffort || compat.SupportsStrictMode {
		t.Fatalf("explicit false lost: %+v", compat)
	}
	if detected := (&Model{ID: "m", API: APIOpenAICompletions, Provider: ProviderOpenAI, BaseURL: "https://api.openai.com/v1"}).GetCompat(); !detected.SupportsStore || !detected.SupportsDeveloperRole {
		t.Fatalf("detection changed: %+v", detected)
	}
}

func TestModelDecodesShippedCatalogMetadata(t *testing.T) {
	entry := `{"type":"chat","id":"claude-fable-5","name":"Claude Fable 5","api":"anthropic-messages","provider":"anthropic","baseUrl":"https://api.anthropic.com","reasoning":true,"input":["text","image"],
	"cost":{"input":10,"output":50,"cacheRead":1,"cacheWrite":12.5,"tiers":[{"inputTokensAbove":200000,"input":20,"output":75,"cacheRead":2,"cacheWrite":25}]},"contextWindow":1000000,"maxTokens":128000,
	"compat":{"supportsMidConvoSystemMessages":true,"supportsMidConvoToolChanges":true,"supportsMidConvoToolAdditions":true,"forceAdaptiveThinking":true,"supportsStrictTools":true,"allowEmptySignature":true,"supportsLongCacheRetention":false,"supportsTemperature":false,"supportsMidConvoEffort":true,"cacheControlFormat":"anthropic","sessionAffinityFormat":"openai-nosession","supportsOpenAIGrammarTools":true,"supportsAdditionalTools":true,"supportsToolSearch":true,"supportsExplicitPromptCacheMode":true,"supportsEagerToolInputStreaming":false,"supportsCacheControlOnTools":false,"requiresReasoningContentOnAssistantMessages":true,"chatTemplateArgs":{"enable_thinking":{"$var":"thinking.enabled"}},
	"allowedFallbackModels":[{"provider":"anthropic","model":"claude-opus-4-8","cost":{"input":5,"output":25,"cacheRead":0.5,"cacheWrite":6.25}}]},
	"thinkingLevelMap":{"off":null,"xhigh":"xhigh"},"promptCache":{"short":300,"long":3600},
	"inputLimits":{"maxRequestBytes":33554432,"images":{"maxPerRequest":600,"resize":{"maxWidth":2000,"maxHeight":2000,"maxBytes":4718592,"jpegQuality":80}}},
	"lab":"Anthropic","enabled":true,"providers":[{"id":"fireworks","name":"Fireworks AI","credential":"radius","source":"radius"}]}`
	decoder := json.NewDecoder(strings.NewReader(entry))
	decoder.DisallowUnknownFields()
	var model Model
	if err := decoder.Decode(&model); err != nil {
		t.Fatal(err)
	}
	compat := model.Compat
	if compat == nil || !compat.ForceAdaptiveThinking || compat.SupportsLongCacheRetention == nil || *compat.SupportsLongCacheRetention || compat.SupportsTemperature == nil || len(compat.AllowedFallbackModels) != 1 || compat.AllowedFallbackModels[0].Cost.Input != 5 || string(compat.ChatTemplateArgs) == "" {
		t.Fatalf("compat = %+v", compat)
	}
	if model.PromptCache == nil || model.PromptCache.Long != 3600 || model.Lab != "Anthropic" || model.Enabled == nil || !*model.Enabled || len(model.Providers) != 1 || model.Providers[0].Credential != "radius" || len(model.Cost.Tiers) != 1 {
		t.Fatalf("model = %+v", model)
	}
}

// streamTerminal runs a stream and returns its only terminal event's stop
// reason and error message.
func streamTerminal(t *testing.T, run func(EventHandler)) (StopReason, string) {
	t.Helper()
	var terminal []StreamEvent
	run(func(_ context.Context, event StreamEvent) {
		switch event.(type) {
		case Done, StreamError:
			terminal = append(terminal, event)
		}
	})
	if len(terminal) != 1 {
		t.Fatalf("terminal events = %#v, want exactly one", terminal)
	}
	if e, ok := terminal[0].(StreamError); ok {
		return e.Msg.StopReason, e.Msg.ErrorMessage
	}
	done := terminal[0].(Done)
	if done.Reason != done.Message.StopReason {
		t.Fatalf("done reason %s, message %s", done.Reason, done.Message.StopReason)
	}

	return done.Reason, ""
}

func TestFinishStreamFailsUnstoppedErroredAndAbortedStreams(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		output  AssistantMessage
		stopped bool
		reason  StopReason
		message string
	}{
		{name: "done", ctx: context.Background(), output: AssistantMessage{StopReason: StopReasonLength}, stopped: true, reason: StopReasonLength},
		{name: "unstopped", ctx: context.Background(), reason: StopReasonError, message: "x ended without a stop reason"},
		{name: "error", ctx: context.Background(), output: AssistantMessage{StopReason: StopReasonError, ErrorMessage: "boom"}, stopped: true, reason: StopReasonError, message: "boom"},
		{name: "provider aborted", ctx: context.Background(), output: AssistantMessage{StopReason: StopReasonAborted}, stopped: true, reason: StopReasonError, message: "an unknown error occurred"},
		{name: "canceled", ctx: canceled, output: AssistantMessage{StopReason: StopReasonStop}, stopped: true, reason: StopReasonAborted, message: "request was aborted: context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, message := streamTerminal(t, func(handler EventHandler) {
				finishStream(tc.ctx, handler, tc.output, tc.stopped, "x ended without a stop reason")
			})
			if reason != tc.reason || message != tc.message {
				t.Fatalf("got %s %q, want %s %q", reason, message, tc.reason, tc.message)
			}
		})
	}
}

func TestMessageJSONDecodesTypedContent(t *testing.T) {
	blocks := []ContentBlock{
		{Type: ContentBlockText, Text: "hello", TextSignature: "text-signature"},
		{Type: ContentBlockThinking, Thinking: "reasoning", ThoughtSignature: "thought-signature"},
		{Type: ContentBlockImage, MimeType: "image/png", Data: "aGVsbG8="},
		{Type: ContentBlockToolCall, Namespace: new(""), ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"id":9007199254740993}`)},
	}
	for _, role := range []string{"system", "user", "assistant", "tool", "toolResult"} {
		t.Run(role, func(t *testing.T) {
			original := Message{Role: role, Content: blocks, Timestamp: 1700000000000, StopReason: string(StopReasonStop)}
			data, err := json.Marshal(&original)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Message
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(blocks, decoded.ContentBlocks()) {
				t.Fatalf("decoded content = %#v, want typed blocks %#v", decoded.Content, blocks)
			}
			if decoded.Role != normalizeMessageRole(role) || decoded.Timestamp != original.Timestamp {
				t.Fatalf("message metadata changed: %+v", decoded)
			}
			if role == "assistant" {
				assistant, ok := decoded.ToAssistantMessage()
				if !ok || decoded.IsErrorMessage() || !reflect.DeepEqual(assistant.Content, blocks) {
					t.Fatalf("assistant classification failed: %+v, %v", assistant, ok)
				}
			}
		})
	}
}

func TestMessageJSONContentCompatibilityAndErrors(t *testing.T) {
	for _, test := range []struct {
		content string
		want    any
		invalid bool
	}{
		{content: `"plain text"`, want: "plain text"},
		{content: `[]`, want: []ContentBlock{}},
		{content: `null`},
		{content: `false`, want: false},
		{content: `42`, want: float64(42)},
		{content: `{"custom":"value"}`, want: map[string]any{"custom": "value"}},
		{content: `  [ ]  `, want: []ContentBlock{}},
		{content: `[1]`, invalid: true},
		{content: `[{"type":"text","text":42}]`, invalid: true},
		{content: `[{"type":"toolCall","namespace":42}]`, invalid: true},
	} {
		t.Run(test.content, func(t *testing.T) {
			decoded := Message{Role: "user", Content: "unchanged", Timestamp: 123}
			err := json.Unmarshal([]byte(`{"role":"assistant","content":`+test.content+`,"timestamp":0}`), &decoded)
			if test.invalid {
				var typeError *json.UnmarshalTypeError
				if !errors.As(err, &typeError) {
					t.Fatalf("error = %v, want retained JSON type error", err)
				}
				if decoded.Role != "user" || decoded.Content != "unchanged" || decoded.Timestamp != 123 {
					t.Fatalf("failed decode mutated receiver: %+v", decoded)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded.Content, test.want) {
				t.Fatalf("content = %#v, want %#v", decoded.Content, test.want)
			}
		})
	}
	var decoded Message
	if err := json.Unmarshal([]byte(`{"role":"user","timestamp":0}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Content != nil {
		t.Fatalf("missing content = %#v, want nil", decoded.Content)
	}
}

func TestCompletionsCacheCompatibilityResolution(t *testing.T) {
	for _, scenario := range []struct {
		provider, baseURL string
		long              bool
	}{
		{provider: ProviderOpenAI, long: true},
		{provider: ProviderTogether},
		{baseURL: "https://api.together.ai/v1"},
		{provider: ProviderCloudflareWorkersAI},
		{baseURL: "https://gateway.ai.cloudflare.com/v1"},
		{provider: ProviderNVIDIA},
		{provider: ProviderAntLing},
	} {
		model := &Model{Provider: scenario.provider, BaseURL: scenario.baseURL}
		if got := model.GetCompat().SupportsLongCacheRetention; got != scenario.long {
			t.Fatalf("%s/%s long=%v, want %v", scenario.provider, scenario.baseURL, got, scenario.long)
		}
		model.Compat = &CompatibilitySettings{SupportsLongCacheRetention: new(!scenario.long), CacheControlFormat: "anthropic"}
		compat := model.GetCompat()
		if compat.SupportsLongCacheRetention == scenario.long || compat.CacheControlFormat != "anthropic" {
			t.Fatalf("override lost: %+v", compat)
		}
	}
	model := &Model{Provider: ProviderOpenRouter, ID: "anthropic/model:batch"}
	if got := model.GetCompat().CacheControlFormat; got != "anthropic" {
		t.Fatalf("detected format=%q", got)
	}
	model.Compat = &CompatibilitySettings{CacheControlFormat: "none"}
	if got := model.GetCompat().CacheControlFormat; got != "none" {
		t.Fatalf("overridden format=%q", got)
	}
}
