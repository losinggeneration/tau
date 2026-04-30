package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamAcceptsModelObject(t *testing.T) {
	Register("test-api-stream-model", &mockProvider{apiName: "test-api-stream-model"})

	model := &Model{
		ID:       "test-model",
		API:      "test-api-stream-model",
		Provider: "test-provider",
	}

	var doneCount int
	err := Stream(context.Background(), model, &Context{
		Messages: []Message{{Role: "user", Content: "hello", Timestamp: 1}},
	}, func(_ context.Context, event StreamEvent) {
		if _, ok := event.(Done); ok {
			doneCount++
		}
	})
	if err != nil {
		t.Fatalf("Stream(model): %v", err)
	}
	if doneCount != 1 {
		t.Fatalf("doneCount = %d, want 1", doneCount)
	}
}

func TestCompleteWithClientOverride(t *testing.T) {
	model := &Model{ID: "test-model", API: "override-api", Provider: "override-provider"}
	want := &AssistantMessage{
		Role:       "assistant",
		Content:    []ContentBlock{{Type: ContentBlockText, Text: "override"}},
		StopReason: StopReasonStop,
		Timestamp:  1,
	}

	client := &mockClient{completeMsg: want}
	got, err := CompleteWithModel(context.Background(), model, &Context{
		Messages: []Message{{Role: "user", Content: "hello", Timestamp: 1}},
	}, WithClient(client))
	if err != nil {
		t.Fatalf("CompleteWithModel override: %v", err)
	}
	if got.Content[0].Text != "override" {
		t.Fatalf("got content = %q, want override", got.Content[0].Text)
	}
}

func TestValidateToolCallReturnsCoercedArguments(t *testing.T) {
	tool := Tool{
		Name:       "read",
		Parameters: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"},"flag":{"type":"boolean"}},"required":["count","flag"]}`),
	}
	call := ToolCall{
		Name:      "read",
		Arguments: json.RawMessage(`{"count":"2","flag":"true"}`),
	}

	args, err := ValidateToolCall([]Tool{tool}, call)
	if err != nil {
		t.Fatalf("ValidateToolCall: %v", err)
	}

	count, ok := args["count"].(int)
	if !ok || count != 2 {
		t.Fatalf("count = %#v, want int(2)", args["count"])
	}

	flag, ok := args["flag"].(bool)
	if !ok || !flag {
		t.Fatalf("flag = %#v, want true", args["flag"])
	}
}

func TestValidateToolCallMissingRequiredField(t *testing.T) {
	tool := Tool{
		Name:       "read",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["path","replace_all"]}`),
	}
	call := ToolCall{
		Name:      "read",
		Arguments: json.RawMessage(`{"path":"/tmp/file.txt"}`),
	}

	_, err := ValidateToolCall([]Tool{tool}, call)
	if err == nil {
		t.Fatal("expected validation error for missing required field")
	}
	if !strings.Contains(err.Error(), "replace_all") {
		t.Fatalf("expected error to mention replace_all, got %v", err)
	}
}

func TestValidateToolCallInvalidArgumentType(t *testing.T) {
	tool := Tool{
		Name:       "read",
		Parameters: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`),
	}
	call := ToolCall{
		Name:      "read",
		Arguments: json.RawMessage(`{"count":"not-a-number"}`),
	}

	_, err := ValidateToolCall([]Tool{tool}, call)
	if err == nil {
		t.Fatal("expected validation error for invalid argument type")
	}
	if !strings.Contains(err.Error(), "count") {
		t.Fatalf("expected error to mention count, got %v", err)
	}
}

func TestValidateToolCallInvalidSchema(t *testing.T) {
	tool := Tool{
		Name:       "read",
		Parameters: json.RawMessage(`{"type":"object","properties":{"count":{"type":"definitely-not-a-type"}}}`),
	}
	call := ToolCall{
		Name:      "read",
		Arguments: json.RawMessage(`{"count":1}`),
	}

	_, err := ValidateToolCall([]Tool{tool}, call)
	if err == nil {
		t.Fatal("expected validation error for invalid schema")
	}
	if !strings.Contains(err.Error(), "invalid JSON schema") {
		t.Fatalf("expected invalid schema error, got %v", err)
	}
}

func TestOpenAIStructuredToolChoiceParity(t *testing.T) {
	client := &openAIClient{model: &Model{ID: "gpt-4o", Provider: ProviderOpenAI}}
	reqBody, err := client.buildRequest(&Model{ID: "gpt-4o", Provider: ProviderOpenAI}, &Context{
		Messages: []Message{{Role: "user", Content: "hello", Timestamp: 1}},
	}, &options{
		toolChoice: map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file",
			},
		},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req openAIRequest
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	choice, ok := req.ToolChoice.(map[string]any)
	if !ok {
		t.Fatalf("ToolChoice type = %T", req.ToolChoice)
	}
	if choice["type"] != "function" {
		t.Fatalf("type = %v, want function", choice["type"])
	}
}

func TestAnthropicStructuredToolChoiceParity(t *testing.T) {
	client, err := newAnthropicClient(&Model{ID: "claude-test", Provider: ProviderAnthropic, MaxTokens: 256})
	if err != nil {
		t.Fatalf("newAnthropicClient: %v", err)
	}

	reqBody, err := client.buildRequest(&Model{ID: "claude-test", Provider: ProviderAnthropic, MaxTokens: 256}, &Context{
		Messages: []Message{{Role: "user", Content: "hello", Timestamp: 1}},
	}, &options{
		toolChoice: map[string]any{
			"type": "tool",
			"name": "read_file",
		},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req anthropicRequest
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	choice, ok := req.ToolChoice.(map[string]any)
	if !ok {
		t.Fatalf("ToolChoice type = %T", req.ToolChoice)
	}
	if choice["name"] != "read_file" {
		t.Fatalf("name = %v, want read_file", choice["name"])
	}
}

func TestAzureConfigSupportsPerCallOverrides(t *testing.T) {
	baseURL, version, deployment, err := resolveAzureConfig(&Model{ID: "gpt-4.1", BaseURL: "https://ignored"}, &options{
		azureAPIVersion:     "2025-01-01-preview",
		azureResourceName:   "ignored-resource",
		azureBaseURL:        "https://custom.azure.com/openai/v1",
		azureDeploymentName: "custom-deployment",
	})
	if err != nil {
		t.Fatalf("resolveAzureConfig: %v", err)
	}
	if baseURL != "https://custom.azure.com/openai/v1" {
		t.Fatalf("baseURL = %q", baseURL)
	}
	if version != "2025-01-01-preview" {
		t.Fatalf("version = %q", version)
	}
	if deployment != "custom-deployment" {
		t.Fatalf("deployment = %q", deployment)
	}
}

func TestCalculateCostMutatesUsageAndReturnsCost(t *testing.T) {
	model := &Model{Cost: CostProfile{Input: 2, Output: 4, CacheRead: 0.5, CacheWrite: 1}}
	usage := &Usage{Input: 1_000_000, Output: 500_000, CacheRead: 200_000, CacheWrite: 100_000}

	cost := CalculateCost(model, usage)

	if cost.Input != 2 || cost.Output != 2 || cost.CacheRead != 0.1 || cost.CacheWrite != 0.1 {
		t.Fatalf("CalculateCost returned %+v", cost)
	}
	if usage.Cost != cost {
		t.Fatalf("usage.Cost = %+v, want returned cost %+v", usage.Cost, cost)
	}
	if cost.Total != 4.2 || usage.TotalTokens != 1_800_000 {
		t.Fatalf("total cost/tokens = %v/%d, want 4.2/1800000", cost.Total, usage.TotalTokens)
	}
}

func TestRegistryPreservesInsertionOrderAndReplacementPosition(t *testing.T) {
	registry := &Registry{providers: make(map[string]Provider)}
	registry.Register("b", &testProvider{api: "b"})
	registry.Register("a", &testProvider{api: "a"})
	registry.Register("b", &testProvider{api: "b"})

	apis := registry.APIs()
	if len(apis) != 2 || apis[0] != "b" || apis[1] != "a" {
		t.Fatalf("registry.APIs() = %v, want insertion order [b a]", apis)
	}
}

func TestRegisterCustomProviderParity(t *testing.T) {
	original := globalRegistry
	globalRegistry = &Registry{providers: make(map[string]Provider), sources: make(map[string]string)}
	t.Cleanup(func() {
		globalRegistry = original
	})

	RegisterWithSource("custom-api", &testProvider{api: "custom-api"}, "custom-source")

	provider, ok := Get("custom-api")
	if !ok {
		t.Fatal("custom provider not registered")
	}
	if provider.API() != "custom-api" {
		t.Fatalf("API = %q, want custom-api", provider.API())
	}

	UnregisterProviders("custom-source")
	if _, ok := Get("custom-api"); ok {
		t.Fatal("custom provider still registered after source unregister")
	}
}

func TestStreamResolvesModelArgumentErrors(t *testing.T) {
	handler := func(_ context.Context, _ StreamEvent) {
		t.Fatal("handler should not be called when Stream returns a resolution error")
	}

	err := Stream(context.Background(), 123, &Context{}, handler)
	if err == nil {
		t.Fatal("expected error for unsupported model argument type")
	}
	if !strings.Contains(err.Error(), "unsupported model argument type") {
		t.Fatalf("err = %v, want unsupported model argument type", err)
	}
}
