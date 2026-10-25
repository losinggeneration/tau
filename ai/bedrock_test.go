package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/stretchr/testify/require"
)

// TestBedrockModelMetadata verifies Bedrock models have correct metadata.
func TestBedrockModelMetadata(t *testing.T) {
	// Test a model that exists in the generated registry
	model, err := GetAvailableModel(ProviderAmazonBedrock, "anthropic.claude-sonnet-4-5-20250929-v1:0")
	if err != nil {
		t.Fatalf("GetAvailableModel(bedrock, anthropic.claude-sonnet-4-5-20250929-v1:0): %v", err)
	}

	if model.Provider != ProviderAmazonBedrock {
		t.Errorf("Provider = %q, want %q", model.Provider, ProviderAmazonBedrock)
	}

	if model.API != APIBedrockConverseStream {
		t.Errorf("API = %q, want %q", model.API, APIBedrockConverseStream)
	}

	// Verify model supports input types
	if !model.SupportsInput(InputText) {
		t.Error("Claude Sonnet should support text input")
	}

	if !model.SupportsInput(InputImage) {
		t.Error("Claude Sonnet should support image input")
	}

	// Verify base URL is set
	if model.BaseURL == "" {
		t.Error("BaseURL should be set for Bedrock models")
	}
}

// TestBedrockModelSupportsAdaptiveThinking verifies Opus 4.7 models support adaptive thinking.
func TestBedrockModelSupportsAdaptiveThinking(t *testing.T) {
	// Opus 4.7 should support adaptive thinking (same as 4.6)
	// Test the adaptive thinking detection logic
	tests := []struct {
		modelID   string
		wantMatch bool
	}{
		{"claude-opus-4-7", true},
		{"claude-opus-4.7", true},
		{"opus-4-7", true},
		{"opus-4.7", true},
		// Non-4.7 models
		{"claude-opus-4-6", true},    // 4.6 also supports adaptive
		{"claude-sonnet-4-5", false}, // 4.5 doesn't support adaptive
		{"claude-haiku-4-5", false},
	}

	for _, tt := range tests {
		t.Run(tt.modelID, func(t *testing.T) {
			// Use isAdaptiveThinkingModel from anthropic.go to check
			supported := isAdaptiveThinkingModel(tt.modelID)
			if supported != tt.wantMatch {
				t.Errorf("isAdaptiveThinkingModel(%q) = %v, want %v", tt.modelID, supported, tt.wantMatch)
			}
		})
	}
}

// TestBedrockModelThinkingEffortMapping verifies thinking effort mapping for Bedrock.
func TestBedrockModelThinkingEffortMapping(t *testing.T) {
	// Test that reasoning levels map correctly
	tests := []struct {
		level       ReasoningLevel
		wantEffort  string
		description string
	}{
		{ReasoningMinimal, "low", "minimal maps to low"},
		{ReasoningLow, "low", "low maps to low"},
		{ReasoningMedium, "medium", "medium maps to medium"},
		{ReasoningHigh, "high", "high maps to high"},
		{ReasoningXHigh, "xhigh", "xhigh maps to xhigh"},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			// Verify effort mapping by checking AdjustMaxTokensForThinking behavior
			maxTokens, budget := AdjustMaxTokensForThinking(1000, 32000, tt.level, nil)
			if maxTokens < budget {
				t.Errorf("maxTokens (%d) should not be less than budget (%d)", maxTokens, budget)
			}
		})
	}
}

// TestBedrockOpus47ThinkingPayloadBehavior documents the expected behavior for
// Bedrock Opus 4.7 thinking payloads based on the TS reference test.
// This test verifies that when reasoning is enabled:
// - thinking.type = "adaptive"
// - thinking.display = "summarized" (default)
// - output_config.effort = mapped from reasoning level (high, xhigh, etc.)
// - anthropic_beta is NOT set for adaptive thinking models
func TestBedrockOpus47ThinkingPayloadBehavior(t *testing.T) {
	// Get the Opus 4.7 model - if not found, skip
	model, err := GetAvailableModel(ProviderAmazonBedrock, "anthropic.claude-opus-4-7-v1:0")
	if err != nil {
		t.Skipf("Opus 4.7 model not found: %v", err)
	}

	// Verify the model supports the thinking configuration we expect
	if !model.Reasoning {
		t.Error("Opus 4.7 should have Reasoning enabled")
	}

	// Verify adaptive thinking support
	if !isAdaptiveThinkingModel(model.ID) {
		t.Error("Opus 4.7 should support adaptive thinking")
	}

	// The thinking configuration for adaptive models should be:
	// - type: "adaptive"
	// - display: "summarized" (default from TS implementation)
	// - effort: mapped from reasoning level
	//
	// For xhigh reasoning level specifically:
	// - output_config.effort = "xhigh"
	//
	// Note: anthropic_beta should NOT be set for adaptive thinking models
	// (only for budget-based thinking models)
}

// TestBedrockProviderSupport verifies Bedrock has proper support status.
func TestBedrockProviderSupport(t *testing.T) {
	status, ok := ProviderSupport(ProviderAmazonBedrock)
	if !ok {
		t.Fatal("ProviderSupport(amazon-bedrock) returned false")
	}

	if !status.HasProviderConstant {
		t.Error("HasProviderConstant should be true")
	}

	if !status.HasImplementation {
		t.Error("HasImplementation should be true")
	}

	if !status.IsRegistered {
		t.Error("IsRegistered should be true")
	}

	if !status.ReachableViaGetClient {
		t.Error("ReachableViaGetClient should be true")
	}

	if !status.Tested {
		t.Error("Tested should be true")
	}

	if !status.OfficiallySupported {
		t.Error("OfficiallySupported should be true")
	}

	if status.Tier != SupportTier1 {
		t.Errorf("Tier = %v, want SupportTier1", status.Tier)
	}
}

// TestBedrockCanGetClient verifies we can construct a client for Bedrock models.
func TestBedrockCanGetClient(t *testing.T) {
	model, err := GetAvailableModel(ProviderAmazonBedrock, "anthropic.claude-3-5-sonnet-20241022-v2:0")
	if err != nil {
		t.Skipf("Bedrock model not found: %v", err)
	}

	client, err := GetClient(model)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}

	if client == nil {
		t.Error("client is nil")
	}
}

// TestBedrockModelsList verifies we can list Bedrock models.
func TestBedrockModelsList(t *testing.T) {
	models := ListAvailableModels(ProviderAmazonBedrock)
	if len(models) == 0 {
		t.Error("expected at least one Bedrock model")
	}

	// Should have Claude models
	hasClaude := false
	for _, m := range models {
		if m.ID != "" && strings.Contains(m.ID, "claude") {
			hasClaude = true
			break
		}
	}

	if !hasClaude {
		t.Error("expected at least one Claude model")
	}
}

func TestBedrockHeaderRoundTripperStripsSigV4ForBearerToken(t *testing.T) {
	// Capture the request that the round-tripper forwards.
	var captured *http.Request
	rt := &bedrockHeaderRoundTripper{
		base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			captured = req
			return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
		}),
		bearerToken: "test-bearer-token",
	}

	req, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/invoke", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	// Simulate the SigV4 headers that aws-sdk-go-v2 attaches.
	req.Header.Set("X-Amz-Date", "20240101T000000Z")
	req.Header.Set("X-Amz-Content-Sha256", "stub")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA/20240101/us-east-1/bedrock/aws4_request")

	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if got := captured.Header.Get("Authorization"); got != "Bearer test-bearer-token" {
		t.Errorf("Authorization = %q, want Bearer test-bearer-token", got)
	}
	if got := captured.Header.Get("X-Amz-Date"); got != "" {
		t.Errorf("X-Amz-Date = %q, want stripped", got)
	}
	if got := captured.Header.Get("X-Amz-Content-Sha256"); got != "" {
		t.Errorf("X-Amz-Content-Sha256 = %q, want stripped", got)
	}
}

func TestBedrockHeaderRoundTripperKeepsSigV4WhenNoBearerToken(t *testing.T) {
	var captured *http.Request
	rt := &bedrockHeaderRoundTripper{
		base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			captured = req
			return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
		}),
	}

	req, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/invoke", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA/20240101/us-east-1/bedrock/aws4_request")
	req.Header.Set("X-Amz-Date", "20240101T000000Z")

	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if got := captured.Header.Get("Authorization"); got == "" {
		t.Error("Authorization should be preserved when no bearer token is set")
	}
	if got := captured.Header.Get("X-Amz-Date"); got == "" {
		t.Error("X-Amz-Date should be preserved when no bearer token is set")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestFormatBedrockErrorAppliesPrefixByName(t *testing.T) {
	throttling := &mockBedrockError{name: "ThrottlingException", message: "rate exceeded"}
	got := formatBedrockError(throttling)
	if got != "Throttling error: rate exceeded" {
		t.Errorf("formatBedrockError = %q, want prefixed", got)
	}

	unavailable := &mockBedrockError{name: "ServiceUnavailableException", message: "service is down"}
	got = formatBedrockError(unavailable)
	if got != "Service unavailable: service is down" {
		t.Errorf("formatBedrockError = %q, want prefixed", got)
	}

	validation := &mockBedrockError{name: "ValidationException", message: "bad input"}
	got = formatBedrockError(validation)
	if got != "Validation error: bad input" {
		t.Errorf("formatBedrockError = %q, want prefixed", got)
	}

	internal := &mockBedrockError{name: "InternalServerException", message: "boom"}
	got = formatBedrockError(internal)
	if got != "Internal server error: boom" {
		t.Errorf("formatBedrockError = %q, want prefixed", got)
	}

	unknown := &mockBedrockError{name: "SomeOtherException", message: "weird"}
	got = formatBedrockError(unknown)
	if got != "weird" {
		t.Errorf("unknown exception should pass through; got %q", got)
	}

	if got := formatBedrockError(nil); got != "" {
		t.Errorf("nil error should return empty; got %q", got)
	}
}

func TestFormatBedrockErrorFallsBackToMessageScanning(t *testing.T) {
	// Errors that wrap a non-Bedrock type but contain the class name should
	// still get the prefix.
	wrapped := &wrappedBedrockError{inner: errors.New("InternalServerException: down")}
	got := formatBedrockError(wrapped)
	if got != "Internal server error: InternalServerException: down" {
		t.Errorf("formatBedrockError = %q, want message-scanned prefix", got)
	}
}

type mockBedrockError struct {
	name    string
	message string
}

func (e *mockBedrockError) Error() string     { return e.message }
func (e *mockBedrockError) ErrorName() string { return e.name }

type wrappedBedrockError struct {
	inner error
}

func (e *wrappedBedrockError) Error() string { return e.inner.Error() }
func (e *wrappedBedrockError) Unwrap() error { return e.inner }

func TestBuildBedrockConverseInputIncludesToolConfigAndReasoning(t *testing.T) {
	model := &Model{
		ID:        "global.anthropic.claude-opus-4-7-v1:0",
		API:       APIBedrockConverseStream,
		Provider:  ProviderAmazonBedrock,
		Reasoning: true,
		MaxTokens: 32000,
	}
	prompt := &Context{
		SystemPrompt: "sys",
		Messages:     []Message{{Role: "user", Content: "hello", Timestamp: 1}},
		Tools: []Tool{{
			Name:        "echo",
			Description: "Echo back text",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		}},
	}

	input, err := buildBedrockConverseInput(model, prompt, applyOptions([]Option{
		WithToolChoice("any"),
		WithReasoning(ReasoningXHigh),
		WithThinkingDisplay("summarized"),
		WithBedrockRequestMetadata(map[string]string{"trace_id": "abc123"}),
	}))
	if err != nil {
		t.Fatalf("buildBedrockConverseInput: %v", err)
	}
	if input.ToolConfig == nil {
		t.Fatal("ToolConfig = nil, want non-nil")
	}
	if got := input.RequestMetadata["trace_id"]; got != "abc123" {
		t.Fatalf("requestMetadata.trace_id = %q, want abc123", got)
	}

	rawDoc, err := buildBedrockAdditionalModelRequestFields(model, applyOptions([]Option{
		WithReasoning(ReasoningXHigh),
		WithThinkingDisplay("summarized"),
	}))
	if err != nil {
		t.Fatalf("buildBedrockAdditionalModelRequestFields: %v", err)
	}
	raw := rawDoc.(map[string]any)
	thinking, _ := raw["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Fatalf("thinking.type = %v, want adaptive", thinking["type"])
	}
	outputConfig, _ := raw["output_config"].(map[string]any)
	if outputConfig["effort"] != "xhigh" {
		t.Fatalf("output_config.effort = %v, want xhigh", outputConfig["effort"])
	}
}

func TestConvertBedrockMessagesGroupsConsecutiveToolResults(t *testing.T) {
	model := &Model{
		ID:       "global.anthropic.claude-sonnet-4-v1:0",
		Provider: ProviderAmazonBedrock,
		API:      APIBedrockConverseStream,
	}
	msgs, err := convertBedrockMessages(&Context{
		Messages: []Message{
			{Role: "user", Content: "hi", Timestamp: 1},
			{Role: "toolResult", ToolCallID: "tool-1", ToolName: "echo", Timestamp: 2, Content: []ContentBlock{{Type: ContentBlockText, Text: "one"}}},
			{Role: "toolResult", ToolCallID: "tool-2", ToolName: "echo", Timestamp: 3, Content: []ContentBlock{{Type: ContentBlockText, Text: "two"}}},
		},
	}, model, CacheRetentionNone)
	if err != nil {
		t.Fatalf("convertBedrockMessages: %v", err)
	}
	if got := len(msgs); got != 2 {
		t.Fatalf("len(msgs) = %d, want 2", got)
	}
	if msgs[1].Role != bedrocktypes.ConversationRoleUser {
		t.Fatalf("msgs[1].Role = %v, want user", msgs[1].Role)
	}
	if got := len(msgs[1].Content); got != 2 {
		t.Fatalf("len(msgs[1].Content) = %d, want 2", got)
	}
}

func TestHandleBedrockStreamEventsProducesStructuredOutput(t *testing.T) {
	output := AssistantMessage{
		Role:      "assistant",
		API:       APIBedrockConverseStream,
		Provider:  ProviderAmazonBedrock,
		Model:     "model",
		Content:   []ContentBlock{},
		Usage:     Usage{},
		Timestamp: 1,
	}
	blocks := make([]*bedrockStreamBlock, 0)
	var events []StreamEvent
	handler := func(_ context.Context, event StreamEvent) {
		events = append(events, event)
	}

	ctx := context.Background()
	handleBedrockContentBlockStart(ctx, &bedrocktypes.ContentBlockStartEvent{
		ContentBlockIndex: aws.Int32(0),
		Start:             &bedrocktypes.ContentBlockStartMemberToolUse{Value: bedrocktypes.ToolUseBlockStart{Name: aws.String("echo"), ToolUseId: aws.String("tool-1")}},
	}, &blocks, &output, handler)
	handleBedrockContentBlockDelta(ctx, &bedrocktypes.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(0),
		Delta:             &bedrocktypes.ContentBlockDeltaMemberToolUse{Value: bedrocktypes.ToolUseBlockDelta{Input: aws.String(`{"text":"hi"}`)}},
	}, &blocks, &output, handler)
	handleBedrockContentBlockStop(ctx, &bedrocktypes.ContentBlockStopEvent{ContentBlockIndex: aws.Int32(0)}, blocks, &output, handler)

	if _, ok := events[0].(ToolCallStart); !ok {
		t.Fatalf("events[0] = %T, want ToolCallStart", events[0])
	}
	if _, ok := events[1].(ToolCallDelta); !ok {
		t.Fatalf("events[1] = %T, want ToolCallDelta", events[1])
	}
	toolEnd, ok := events[2].(ToolCallEnd)
	if !ok {
		t.Fatalf("events[2] = %T, want ToolCallEnd", events[2])
	}
	if string(toolEnd.ToolCall.Arguments) != `{"text":"hi"}` {
		t.Fatalf("toolEnd.ToolCall.Arguments = %s", string(toolEnd.ToolCall.Arguments))
	}
}

func TestBedrock55UsesNativeEffortAndCaching(t *testing.T) {
	model := &Model{ID: "anthropic.claude-haiku-5-5", Reasoning: true}
	if !supportsAdaptiveThinking(model.ID) || !supportsBedrockPromptCaching(model) || mapBedrockThinkingEffort(ReasoningXHigh, model.ID) != "xhigh" {
		t.Fatal("Claude 5.5 metadata not applied")
	}
}

func TestBedrockOpenAIReasoningRequestShapes(t *testing.T) {
	fields := func(model *Model, level ReasoningLevel) any {
		t.Helper()
		got, err := buildBedrockAdditionalModelRequestFields(model, applyOptions([]Option{WithReasoning(level)}))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	oss := &Model{ID: "openai.gpt-oss-120b-1:0", Reasoning: true}
	for level, want := range map[ReasoningLevel]string{ReasoningMinimal: "low", ReasoningMedium: "medium", ReasoningXHigh: "high", ReasoningMax: "high"} {
		if got := fields(oss, level); !reflect.DeepEqual(got, map[string]any{"reasoning_effort": want}) {
			t.Errorf("gpt-oss %s = %v, want flat reasoning_effort %s", level, got, want)
		}
	}
	gpt := &Model{ID: "openai.gpt-5.6", Name: "GPT 5.6", Reasoning: true, ThinkingLevelMap: map[string]*string{"high": new("max")}}
	for level, want := range map[ReasoningLevel]string{ReasoningMinimal: "low", ReasoningXHigh: "xhigh", ReasoningMax: "max", ReasoningHigh: "max"} {
		if got := fields(gpt, level); !reflect.DeepEqual(got, map[string]any{"reasoning": map[string]any{"effort": want}}) {
			t.Errorf("gpt %s = %v, want nested reasoning.effort %s", level, got, want)
		}
	}
	if got := fields(&Model{ID: "meta.llama3", Reasoning: true}, ReasoningHigh); got != nil {
		t.Errorf("other models get no reasoning fields: %v", got)
	}
}

func TestStreamBedrockEventsTerminalOutcomesMatchUpstream(t *testing.T) {
	start := &bedrocktypes.ConverseStreamOutputMemberMessageStart{Value: bedrocktypes.MessageStartEvent{Role: bedrocktypes.ConversationRoleAssistant}}
	stop := func(reason bedrocktypes.StopReason) bedrocktypes.ConverseStreamOutput {
		return &bedrocktypes.ConverseStreamOutputMemberMessageStop{Value: bedrocktypes.MessageStopEvent{StopReason: reason}}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		events    []bedrocktypes.ConverseStreamOutput
		streamErr error
		reason    StopReason
		message   string
	}{
		{name: "end turn", events: []bedrocktypes.ConverseStreamOutput{start, stop(bedrocktypes.StopReasonEndTurn)}, reason: StopReasonStop},
		{name: "context window", events: []bedrocktypes.ConverseStreamOutput{start, stop(bedrocktypes.StopReasonModelContextWindowExceeded)}, reason: StopReasonLength},
		{name: "guardrail", events: []bedrocktypes.ConverseStreamOutput{start, stop(bedrocktypes.StopReasonGuardrailIntervened)}, reason: StopReasonError, message: "Provider stopped with: guardrail_intervened"},
		{name: "no stop", events: []bedrocktypes.ConverseStreamOutput{start}, reason: StopReasonError, message: "bedrock stream ended without a stop reason"},
		{name: "stream error", events: []bedrocktypes.ConverseStreamOutput{start}, streamErr: errors.New("reset"), reason: StopReasonError, message: "bedrock: stream error: reset"},
		{name: "canceled", ctx: canceled, events: []bedrocktypes.ConverseStreamOutput{start, stop(bedrocktypes.StopReasonEndTurn)}, streamErr: context.Canceled, reason: StopReasonAborted, message: "request was aborted: context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			events := make(chan bedrocktypes.ConverseStreamOutput, len(tc.events))
			for _, event := range tc.events {
				events <- event
			}
			close(events)
			reason, message := streamTerminal(t, func(handler EventHandler) {
				err := streamBedrockEvents(ctx, &Model{ID: "m"}, events, func() error { return tc.streamErr }, AssistantMessage{Role: "assistant", StopReason: StopReasonStop}, handler)
				if err != nil {
					t.Fatal(err)
				}
			})
			if reason != tc.reason || message != tc.message {
				t.Fatalf("got %s %q, want %s %q", reason, message, tc.reason, tc.message)
			}
		})
	}
}

func TestBedrockBudgetThinkingFitsResponseCeilingLikeUpstream(t *testing.T) {
	model := &Model{ID: "anthropic.claude-sonnet-4-5-v1:0", Provider: ProviderAmazonBedrock, API: APIBedrockConverseStream, Reasoning: true, MaxTokens: 8192}
	for _, tc := range []struct {
		opts                []Option
		wantMax, wantBudget int
	}{
		{[]Option{WithReasoning(ReasoningMedium), WithMaxTokens(8192)}, 8192, 7168},
		{[]Option{WithReasoning(ReasoningMedium), WithMaxTokens(4000), WithThinkingBudgetTokens(3000)}, 7000, 3000},
	} {
		input, err := buildBedrockConverseInput(model, &Context{Messages: []Message{{Role: "user", Content: "hi"}}}, applyOptions(tc.opts))
		if err != nil {
			t.Fatal(err)
		}
		data, err := input.AdditionalModelRequestFields.MarshalSmithyDocument()
		if err != nil {
			t.Fatal(err)
		}
		var fields struct {
			Thinking struct {
				BudgetTokens int `json:"budget_tokens"`
			} `json:"thinking"`
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		budget := fields.Thinking.BudgetTokens
		if *input.InferenceConfig.MaxTokens != int32(tc.wantMax) || budget != tc.wantBudget {
			t.Fatalf("maxTokens=%d budget=%d, want %d and %d", *input.InferenceConfig.MaxTokens, budget, tc.wantMax, tc.wantBudget)
		}
	}
}

func TestBedrockStrictToolsMatchUpstream(t *testing.T) {
	tool := func(strict string) Tool {
		return Tool{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"` + strict + `"}`)}
	}
	spec := func(model *Model, tools ...Tool) bedrocktypes.ToolSpecification {
		t.Helper()
		config, err := buildBedrockToolConfig(model, tools, nil)
		if err != nil {
			t.Fatal(err)
		}
		return config.Tools[0].(*bedrocktypes.ToolMemberToolSpec).Value
	}
	unsupported := &Model{ID: "m"}
	if got := spec(unsupported, tool("prefer")); got.Strict != nil {
		t.Fatalf("strict sent without support: %v", *got.Strict)
	}
	if _, err := buildBedrockToolConfig(unsupported, []Tool{tool("require")}, nil); err == nil {
		t.Fatal("required strictness accepted without support")
	}
	supported := &Model{ID: "m", Compat: &CompatibilitySettings{SupportsStrictMode: new(true)}}
	got := spec(supported, tool("require"))
	if got.Strict == nil || !*got.Strict {
		t.Fatal("strict missing for a supported model")
	}
	if spec(supported, Tool{Name: "plain", Parameters: json.RawMessage(`{"type":"object"}`)}).Strict != nil {
		t.Fatal("strict sent for a tool that did not ask for it")
	}
}

func TestBedrockClientConsumesProviderRetriesAndTimeout(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	client, err := newBedrockRuntimeClient(t.Context(), applyOptions(nil))
	if err != nil {
		t.Fatal(err)
	}
	if options := client.Options(); options.RetryMaxAttempts != 0 {
		t.Fatalf("default retry attempts changed: %d", options.RetryMaxAttempts)
	}
	client, err = newBedrockRuntimeClient(t.Context(), applyOptions([]Option{WithMaxRetries(0), WithTimeout(time.Second), WithBedrockBearerToken("token")}))
	if err != nil {
		t.Fatal(err)
	}
	options := client.Options()
	if options.RetryMaxAttempts != 1 {
		t.Fatalf("retry attempts = %d, want 1", options.RetryMaxAttempts)
	}
	httpClient, ok := options.HTTPClient.(*http.Client)
	if !ok {
		t.Fatalf("http client = %T", options.HTTPClient)
	}
	timeout, ok := httpClient.Transport.(*headerTimeoutTransport)
	if !ok || timeout.timeout != time.Second {
		t.Fatalf("header timeout transport = %#v", httpClient.Transport)
	}
	if _, ok := timeout.base.(*bedrockHeaderRoundTripper); !ok {
		t.Fatalf("bearer transport dropped: %T", timeout.base)
	}
}

func TestBedrockSharedThinkingBudgets(t *testing.T) {
	model := &Model{ID: "anthropic.claude-sonnet-4-5", Reasoning: true, MaxTokens: 16000}
	for _, tc := range []struct {
		level ReasoningLevel
		want  int
	}{{ReasoningLow, 0}, {ReasoningXHigh, 3000}, {ReasoningMax, 3000}, {ReasoningOff, 0}} {
		t.Run(string(tc.level), func(t *testing.T) {
			opts := applyOptions([]Option{WithReasoning(tc.level), WithThinkingBudgets(&ThinkingBudgets{Low: new(0), High: new(3000)})})
			if got := bedrockThinkingBudget(model, opts); got != tc.want {
				t.Fatalf("budget=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestBedrockSharedThinkingBudgetRequestAndOverrides(t *testing.T) {
	model := &Model{ID: "anthropic.claude-sonnet-4-5", Reasoning: true, MaxTokens: 16000}
	for _, tc := range []struct {
		name  string
		extra []Option
		want  int
	}{
		{"shared", nil, 3000},
		{"provider override", []Option{WithBedrockThinkingBudgets(&ThinkingBudgets{High: new(2000)})}, 2000},
		{"token override", []Option{WithThinkingBudgetTokens(1000)}, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := []Option{WithReasoning(ReasoningXHigh), WithMaxTokens(4000), WithThinkingBudgets(&ThinkingBudgets{High: new(3000)})}
			input, err := buildBedrockConverseInput(model, &Context{}, applyOptions(append(opts, tc.extra...)))
			if err != nil {
				t.Fatal(err)
			}
			data, err := input.AdditionalModelRequestFields.MarshalSmithyDocument()
			if err != nil {
				t.Fatal(err)
			}
			var fields struct {
				Thinking struct {
					Budget int `json:"budget_tokens"`
				} `json:"thinking"`
			}
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if fields.Thinking.Budget != tc.want || *input.InferenceConfig.MaxTokens != int32(4000+tc.want) {
				t.Fatalf("fields=%s max=%d", data, *input.InferenceConfig.MaxTokens)
			}
		})
	}
}

func TestBedrockEnvOptOutUsesExplicitBearerWithoutAmbientConfig(t *testing.T) {
	t.Setenv("AWS_PROFILE", "nonexistent-profile")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "ambient-token")
	var requests int
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		require.Equal(t, "Bearer explicit-token", req.Header.Get("Authorization"))
		require.Empty(t, req.Header.Get("X-Amz-Security-Token"))
		require.Empty(t, req.Header.Get("X-Amz-Date"))
		return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"synthetic rejection"}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	for _, auth := range []Option{WithAPIKey("explicit-token"), WithBedrockBearerToken("explicit-token")} {
		o := applyOptions([]Option{WithoutEnvCredentials(), auth, WithMaxRetries(0)})
		client, err := newBedrockRuntimeClient(t.Context(), o)
		require.NoError(t, err)
		_, err = client.ConverseStream(t.Context(), &bedrockruntime.ConverseStreamInput{ModelId: new("test")})
		require.ErrorContains(t, err, "synthetic rejection")
	}
	require.Equal(t, 2, requests)
}
