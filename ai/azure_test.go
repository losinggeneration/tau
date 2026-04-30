package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildAzureRequestBodyUsesResponsesFormat(t *testing.T) {
	body := buildAzureRequestBody(&Model{
		ID:        "gpt-4.1",
		Provider:  ProviderAzureOpenAI,
		Reasoning: true,
		Input:     []InputType{InputText, InputImage},
	}, &Context{
		SystemPrompt: "system",
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: ContentBlockText, Text: "hello"}}},
			{Role: "assistant", Content: []ContentBlock{
				{Type: ContentBlockText, Text: "hi"},
				{Type: ContentBlockToolCall, ID: "call_1|fc_1", Name: "read", Arguments: []byte(`{"path":"a.txt"}`)},
			}},
			{Role: "toolResult", ToolCallID: "call_1|fc_1", Content: []ContentBlock{{Type: ContentBlockText, Text: "contents"}}},
		},
		Tools: []Tool{
			{Name: "read", Description: "Read file", Parameters: []byte(`{"type":"object"}`)},
		},
	}, &options{
		sessionID: "sess_123",
		metadata:  map[string]any{"trace_id": "abc123"},
		reasoning: ReasoningMedium,
		maxTokens: 64,
	})

	if body["stream"] != true {
		t.Fatalf("stream = %v, want true", body["stream"])
	}
	if body["prompt_cache_key"] != "sess_123" {
		t.Fatalf("prompt_cache_key = %v, want sess_123", body["prompt_cache_key"])
	}
	if body["max_output_tokens"] != 64 {
		t.Fatalf("max_output_tokens = %v, want 64", body["max_output_tokens"])
	}
	if _, ok := body["input"]; !ok {
		t.Fatal("expected input field")
	}
	if _, ok := body["messages"]; ok {
		t.Fatal("did not expect legacy messages field")
	}

	input, ok := body["input"].([]map[string]any)
	if !ok {
		t.Fatalf("input type = %T", body["input"])
	}
	if len(input) != 5 {
		t.Fatalf("len(input) = %d, want 5", len(input))
	}
	if input[0]["role"] != "developer" {
		t.Fatalf("input[0].role = %v, want developer", input[0]["role"])
	}
	if input[3]["type"] != "function_call" {
		t.Fatalf("input[3].type = %v, want function_call", input[3]["type"])
	}
	if input[4]["type"] != "function_call_output" {
		t.Fatalf("input[4].type = %v, want function_call_output", input[4]["type"])
	}

	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning type = %T", body["reasoning"])
	}
	if reasoning["effort"] != "medium" {
		t.Fatalf("reasoning.effort = %v, want medium", reasoning["effort"])
	}
}

func TestStreamAzureResponseEmitsResponsesEvents(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"thinking"}`,
		`data: {"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"text":"thinking"}]}}`,
		`data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}`,
		`data: {"type":"response.output_text.delta","delta":"Hello "}`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"Hello world"}]}}`,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read"}}`,
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"path\":\"a"}`,
		`data: {"type":"response.function_call_arguments.done","arguments":"{\"path\":\"a.txt\"}"}`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\":\"a.txt\"}"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":16,"input_tokens_details":{"cached_tokens":2}}}}`,
		"",
	}, "\n")

	var events []StreamEvent
	handler := func(_ context.Context, event StreamEvent) {
		events = append(events, event)
	}

	err := streamAzureResponse(context.Background(), &Model{
		ID:       "gpt-4.1",
		Provider: ProviderAzureOpenAI,
	}, strings.NewReader(stream), handler, nil)
	if err != nil {
		t.Fatalf("streamAzureResponse: %v", err)
	}

	if len(events) < 11 {
		t.Fatalf("len(events) = %d, want at least 11", len(events))
	}
	if _, ok := events[0].(Start); !ok {
		t.Fatalf("events[0] = %T, want Start", events[0])
	}
	var sawThinkingStart bool
	var sawTextStart bool
	toolEndFound := false
	doneFound := false
	for _, event := range events {
		switch e := event.(type) {
		case ThinkingStart:
			sawThinkingStart = true
		case TextStart:
			sawTextStart = true
		case ToolCallEnd:
			toolEndFound = true
			if e.ToolCall.ID != "call_1|fc_1" {
				t.Fatalf("tool call id = %q, want call_1|fc_1", e.ToolCall.ID)
			}
		case Done:
			doneFound = true
			if e.Reason != StopReasonToolUse {
				t.Fatalf("done reason = %q, want %q", e.Reason, StopReasonToolUse)
			}
			if e.Message.ResponseID != "resp_1" {
				t.Fatalf("response id = %q, want resp_1", e.Message.ResponseID)
			}
			if e.Message.Usage.Input != 8 || e.Message.Usage.Output != 4 || e.Message.Usage.CacheRead != 2 || e.Message.Usage.TotalTokens != 14 {
				t.Fatalf("usage = %+v, want input=8 output=4 cacheRead=2 total=14", e.Message.Usage)
			}
		}
	}
	if !sawThinkingStart {
		t.Fatal("expected ThinkingStart event")
	}
	if !sawTextStart {
		t.Fatal("expected TextStart event")
	}
	if !toolEndFound {
		t.Fatal("expected ToolCallEnd event")
	}
	if !doneFound {
		t.Fatal("expected Done event")
	}

	assertStreamGolden(t, events, streamGolden{StopReason: StopReasonToolUse})
}

func TestResolveAzureConfigSupportsResourceAndDeploymentMap(t *testing.T) {
	t.Setenv("AZURE_OPENAI_BASE_URL", "")
	t.Setenv("AZURE_OPENAI_RESOURCE_NAME", "my-resource")
	t.Setenv("AZURE_OPENAI_API_VERSION", "2025-01-01-preview")
	t.Setenv("AZURE_OPENAI_DEPLOYMENT_NAME_MAP", "gpt-4.1=prod-deployment")

	baseURL, version, deployment, err := resolveAzureConfig(&Model{ID: "gpt-4.1"}, nil)
	if err != nil {
		t.Fatalf("resolveAzureConfig: %v", err)
	}
	if baseURL != "https://my-resource.openai.azure.com/openai/v1" {
		t.Fatalf("baseURL = %q, want resource-derived URL", baseURL)
	}
	if version != "2025-01-01-preview" {
		t.Fatalf("version = %q, want 2025-01-01-preview", version)
	}
	if deployment != "prod-deployment" {
		t.Fatalf("deployment = %q, want prod-deployment", deployment)
	}
}

func TestStreamAzureResponseWithServiceTierPricing(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","service_tier":"flex"}}`,
		`data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}`,
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"Hello"}]}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","service_tier":"flex","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":0}}}}`,
		"",
	}, "\n")

	var events []StreamEvent
	handler := func(_ context.Context, event StreamEvent) {
		events = append(events, event)
	}

	model := &Model{
		ID:   "gpt-4.1",
		Cost: CostProfile{Input: 1000, Output: 2000},
	}
	o := &options{serviceTier: "flex"}

	err := streamAzureResponse(context.Background(), model, strings.NewReader(stream), handler, o)
	if err != nil {
		t.Fatalf("streamAzureResponse: %v", err)
	}

	// Find the Done event with usage
	var done Done
	for _, event := range events {
		if d, ok := event.(Done); ok {
			done = d
			break
		}
	}

	// Base cost: 100 * 0.001 + 50 * 0.002 = 0.1 + 0.1 = 0.2
	// With flex (0.5x) multiplier: 0.1
	if done.Message.Usage.Cost.Total != 0.1 {
		t.Errorf("expected total cost 0.1 with flex multiplier, got %f", done.Message.Usage.Cost.Total)
	}
}

// TestStreamAzureResponseFunctionCallArgumentsDone verifies that the
// function_call_arguments.done event correctly uses event.Item.Arguments
// as the canonical source (matching the shared ProcessResponsesStream behavior).
func TestStreamAzureResponseFunctionCallArgumentsDoneUsesItemArguments(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather"}}`,
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"city\":"}`,
		`data: {"type":"response.function_call_arguments.delta","delta":"\"Seattle\"}"}`,
		// Done event with final arguments in item (canonical source)
		`data: {"type":"response.function_call_arguments.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Seattle\"}"}}`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Seattle\"}"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":50,"output_tokens":20,"total_tokens":70}}}`,
		"",
	}, "\n")

	var toolCallEnd ToolCallEnd
	handler := func(_ context.Context, event StreamEvent) {
		if e, ok := event.(ToolCallEnd); ok {
			toolCallEnd = e
		}
	}

	model := &Model{ID: "gpt-4.1", Provider: ProviderAzureOpenAI}
	err := streamAzureResponse(context.Background(), model, strings.NewReader(stream), handler, &options{})
	if err != nil {
		t.Fatalf("streamAzureResponse: %v", err)
	}

	if toolCallEnd.ToolCall.ID != "call_1|fc_1" {
		t.Errorf("tool call ID = %q, want call_1|fc_1", toolCallEnd.ToolCall.ID)
	}

	// Verify arguments are correctly parsed from event.Item.Arguments
	var args map[string]any
	if err := json.Unmarshal(toolCallEnd.ToolCall.Arguments, &args); err != nil {
		t.Fatalf("failed to unmarshal tool call arguments: %v", err)
	}
	if args["city"] != "Seattle" {
		t.Errorf("city = %v, want Seattle", args["city"])
	}
}

func TestStreamAzureResponseServiceTierFromResponse(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}`,
		`data: {"type":"response.output_text.delta","delta":"Hi"}`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"Hi"}]}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","service_tier":"priority","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":0}}}}`,
		"",
	}, "\n")

	var events []StreamEvent
	handler := func(_ context.Context, event StreamEvent) {
		events = append(events, event)
	}

	model := &Model{
		ID:   "gpt-4.1",
		Cost: CostProfile{Input: 1000, Output: 2000},
	}
	// No service tier requested, but response returns "priority"
	o := &options{}

	err := streamAzureResponse(context.Background(), model, strings.NewReader(stream), handler, o)
	if err != nil {
		t.Fatalf("streamAzureResponse: %v", err)
	}

	var done Done
	for _, event := range events {
		if d, ok := event.(Done); ok {
			done = d
			break
		}
	}

	// Base cost: 100 * 0.001 + 50 * 0.002 = 0.1 + 0.1 = 0.2
	// With priority (2x) multiplier: 0.4
	if done.Message.Usage.Cost.Total != 0.4 {
		t.Errorf("expected total cost 0.4 with priority multiplier, got %f", done.Message.Usage.Cost.Total)
	}
}

// TestStreamAzureResponseRefusal tests refusal content handling.
func TestStreamAzureResponseRefusal(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}`,
		`data: {"type":"response.output_text.delta","delta":"I can help with"}`,
		`data: {"type":"response.refusal.delta","delta":" that."}`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":[{"type":"refusal","refusal":"that."}]}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
		"",
	}, "\n")

	var done Done
	handler := func(_ context.Context, event StreamEvent) {
		if d, ok := event.(Done); ok {
			done = d
		}
	}

	err := streamAzureResponse(context.Background(), &Model{
		ID: "gpt-4.1",
	}, strings.NewReader(stream), handler, nil)
	if err != nil {
		t.Fatalf("streamAzureResponse: %v", err)
	}

	if len(done.Message.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(done.Message.Content))
	}

	block := done.Message.Content[0]
	// Azure builds text from refusal content in output_item.done
	if block.Text != "I can help with that." {
		t.Errorf("Text = %q, want 'I can help with that.'", block.Text)
	}
}

// TestStreamAzureResponseReasoningSummaryPartDone tests reasoning summary part done handling.
func TestStreamAzureResponseReasoningSummaryPartDone(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"thinking text"}`,
		`data: {"type":"response.reasoning_summary_part.done","item":{"type":"reasoning_summary","summary":[{"text":"thinking text"}]}}`,
		`data: {"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"text":"thinking text"}]}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
		"",
	}, "\n")

	var done Done
	handler := func(_ context.Context, event StreamEvent) {
		if d, ok := event.(Done); ok {
			done = d
		}
	}

	err := streamAzureResponse(context.Background(), &Model{
		ID: "gpt-4.1",
	}, strings.NewReader(stream), handler, nil)
	if err != nil {
		t.Fatalf("streamAzureResponse: %v", err)
	}

	if len(done.Message.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(done.Message.Content))
	}

	block := done.Message.Content[0]
	if block.Type != ContentBlockThinking {
		t.Fatalf("block.Type = %q, want %q", block.Type, ContentBlockThinking)
	}
	if block.Thinking != "thinking text" {
		t.Errorf("Thinking = %q, want 'thinking text'", block.Thinking)
	}
}
