package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildAzureRequestBodyUsesResponsesFormat(t *testing.T) {
	body := requireBody(t)(buildAzureRequestBody(&Model{
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
	}))

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

func TestResolveAzureConfigNormalizesFoundryEndpointsLikeUpstream(t *testing.T) {
	t.Setenv("AZURE_OPENAI_RESOURCE_NAME", "")
	for _, tc := range []struct{ in, want string }{
		{"https://r.openai.azure.com", "https://r.openai.azure.com/openai/v1"},
		{"https://r.openai.azure.com/openai/", "https://r.openai.azure.com/openai/v1"},
		{"https://r.cognitiveservices.azure.com/openai/v1/responses?api-version=x", "https://r.cognitiveservices.azure.com/openai/v1"},
		{"https://r.services.ai.azure.com/", "https://r.services.ai.azure.com/openai/v1"},
		{"https://r.openai.azure.com/openai/deployments/d", "https://r.openai.azure.com/openai/deployments/d"},
		{"https://proxy.example.com/", "https://proxy.example.com"},
	} {
		t.Setenv("AZURE_OPENAI_BASE_URL", tc.in)
		baseURL, _, _, err := resolveAzureConfig(&Model{ID: "m"}, nil)
		if err != nil || baseURL != tc.want {
			t.Errorf("%s: baseURL = %q, %v; want %q", tc.in, baseURL, err, tc.want)
		}
	}
	t.Setenv("AZURE_OPENAI_BASE_URL", "://bad")
	if _, _, _, err := resolveAzureConfig(&Model{ID: "m"}, nil); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("invalid base URL error = %v", err)
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

func TestAzureUsesModelReasoningMapping(t *testing.T) {
	model := &Model{ID: "reasoner", Reasoning: true, ThinkingLevelMap: map[string]*string{"low": new("minimal")}}
	config := applyOptions([]Option{WithReasoning(ReasoningLow)})
	body := requireBody(t)(buildAzureRequestBody(model, &Context{}, config))
	if body["reasoning"].(map[string]any)["effort"] != "minimal" {
		t.Fatalf("reasoning=%v", body)
	}
}

func TestAzureResponsesHeaderTimeout(t *testing.T) {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := (&azureClient{model: &Model{ID: "test"}}).Complete(ctx, &Model{ID: "test", BaseURL: "https://test.invalid"}, &Context{}, WithAPIKey("key"), WithAzureBaseURL("https://test.invalid"), WithTimeout(time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || !strings.Contains(err.Error(), "response headers timed out") {
		t.Fatalf("header timeout ignored or lost: %v", err)
	}
}

func TestAzureResponsesHeaderTimeoutDoesNotLimitBody(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Millisecond} {
		for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
			t.Run(fmt.Sprintf("%s/%d", timeout, status), func(t *testing.T) {
				var body *codexTestBody
				previous := http.DefaultClient
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if timeout == 0 {
						timer := time.NewTimer(10 * time.Millisecond)
						defer timer.Stop()
						select {
						case <-req.Context().Done():
							return nil, req.Context().Err()
						case <-timer.C:
						}
					}
					payload := "rejected"
					if status == http.StatusOK {
						payload = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
					}
					body = &codexTestBody{Reader: strings.NewReader(payload), ctx: req.Context(), delay: 10 * time.Millisecond}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
				})}
				t.Cleanup(func() { http.DefaultClient = previous })
				message, err := (&azureClient{model: &Model{ID: "test"}}).Complete(t.Context(), &Model{ID: "test", BaseURL: "https://test.invalid"}, &Context{}, WithAPIKey("key"), WithAzureBaseURL("https://test.invalid"), WithTimeout(timeout))
				if status == http.StatusOK {
					if err != nil || message == nil || message.StopReason != StopReasonStop {
						t.Fatalf("body timed out: message=%#v err=%v", message, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "status 400: rejected") {
					t.Fatalf("error body timed out: %v", err)
				}
				if body == nil || !body.closed {
					t.Fatal("body not closed")
				}
				if timeout > 0 && body.ctx.Err() == nil {
					t.Fatal("request context not released")
				}
			})
		}
	}
}

func TestAzureResponsesBodyCancellationAndCleanupCauses(t *testing.T) {
	readFailure := errors.New("read failed")
	closeFailure := errors.New("close failed")
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		for _, cancelBody := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/cancel=%v", status, cancelBody), func(t *testing.T) {
				body := &codexTestBody{Reader: strings.NewReader("rejected"), readErr: readFailure, closeErr: closeFailure}
				if cancelBody {
					body.delay = time.Hour
				}
				previous := http.DefaultClient
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					body.ctx = req.Context()
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
				})}
				t.Cleanup(func() { http.DefaultClient = previous })
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer cancel()
				var terminal *StreamError
				err := (&azureClient{model: &Model{ID: "test"}}).Stream(ctx, &Model{ID: "test", BaseURL: "https://test.invalid"}, &Context{}, func(_ context.Context, event StreamEvent) {
					if failure, ok := event.(StreamError); ok {
						terminal = &failure
					}
				}, WithAPIKey("key"), WithAzureBaseURL("https://test.invalid"), WithTimeout(time.Millisecond))
				if !body.closed || body.ctx.Err() == nil || !errors.Is(err, closeFailure) {
					t.Fatalf("cleanup lost: closed=%v err=%v", body.closed, err)
				}
				cause := readFailure
				if cancelBody {
					cause = context.DeadlineExceeded
					if ctx.Err() == nil {
						t.Fatal("provider timeout interrupted body")
					}
				}
				if status == http.StatusOK {
					if terminal == nil || !errors.Is(terminal.Err, cause) {
						t.Fatalf("terminal cause lost: %#v", terminal)
					}
				} else if !errors.Is(err, cause) {
					t.Fatalf("body cause lost: %v", err)
				}
			})
		}
	}
}

func TestAzureCacheKeyReachesTransport(t *testing.T) {
	for _, scenario := range []struct{ name, session, retention, want string }{
		{name: "empty"},
		{name: "short", session: "session", want: "session"},
		{name: "boundary", session: strings.Repeat("界", 64), want: strings.Repeat("界", 64)},
		{name: "ascii overflow", session: strings.Repeat("x", 67), want: strings.Repeat("x", 64)},
		{name: "unicode overflow", session: strings.Repeat("界", 63) + "😀z", want: strings.Repeat("界", 63) + "😀"},
		{name: "combining marks", session: strings.Repeat("e\u0301", 33), want: strings.Repeat("e\u0301", 32)},
		{name: "disabled", session: strings.Repeat("x", 67), retention: "none"},
		{name: "explicit short", session: strings.Repeat("x", 67), retention: "short", want: strings.Repeat("x", 64)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("PI_CACHE_RETENTION", "long")
			original := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = original })
			var requests, callbacks, done int
			check := func(body map[string]any) {
				if scenario.want == "" {
					if _, ok := body["prompt_cache_key"]; ok {
						t.Errorf("unexpected cache key: %v", body["prompt_cache_key"])
					}
				} else if body["prompt_cache_key"] != scenario.want {
					t.Errorf("cache key = %v, want %q", body["prompt_cache_key"], scenario.want)
				}
			}
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				check(body)
				var wantAffinity string
				if scenario.retention == "none" {
					wantAffinity = ""
				}
				if req.Header.Get("session_id") != wantAffinity || req.Header.Get("x-client-request-id") != wantAffinity {
					t.Errorf("affinity changed: %q / %q, want %q", req.Header.Get("session_id"), req.Header.Get("x-client-request-id"), wantAffinity)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}, nil
			})}
			model := &Model{ID: "test", API: APIAzureOpenAIResponses, Provider: ProviderAzureOpenAI, BaseURL: "https://test.invalid/v1"}
			key := "test-key"

			err := (&azureClient{model: model}).Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hello"}}}, func(_ context.Context, event StreamEvent) {
				if _, ok := event.(Done); ok {
					done++
				}
			}, WithAPIKey(key), WithSessionID(scenario.session), WithCacheRetention(scenario.retention), WithTransport(TransportSSE), WithAzureBaseURL("https://test.invalid/v1"), WithPayloadCallback(func(payload any) any {
				callbacks++
				body, ok := payload.(map[string]any)
				if !ok {
					t.Fatalf("payload type = %T", payload)
				}
				check(body)
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if requests != 1 || callbacks != 1 || done != 1 {
				t.Fatalf("requests=%d callbacks=%d done=%d", requests, callbacks, done)
			}
		})
	}
}

func TestAzureRequestLimitsReachesTransport(t *testing.T) {
	for _, scenario := range []struct {
		name                                          string
		maxTokens                                     *int
		wantMax                                       int
		modelSampling, levelSampling, requestSampling map[string]any
	}{
		{name: "unset"},
		{name: "zero", maxTokens: new(0)},
		{name: "negative", maxTokens: new(-1)},
		{name: "one", maxTokens: new(1), wantMax: 16},
		{name: "below minimum", maxTokens: new(15), wantMax: 16},
		{name: "minimum", maxTokens: new(16), wantMax: 16},
		{name: "above minimum", maxTokens: new(17), wantMax: 17},
		{name: "large", maxTokens: new(1000), wantMax: 1000},
		{name: "model sampling overrides minimum", maxTokens: new(1), modelSampling: map[string]any{"max_output_tokens": 7}, wantMax: 7},
		{name: "thinking sampling overrides model", maxTokens: new(1), modelSampling: map[string]any{"max_output_tokens": 7}, levelSampling: map[string]any{"max_output_tokens": 8}, wantMax: 8},
		{name: "request sampling overrides thinking", maxTokens: new(1), modelSampling: map[string]any{"max_output_tokens": 7}, levelSampling: map[string]any{"max_output_tokens": 8}, requestSampling: map[string]any{"max_output_tokens": 9}, wantMax: 9},
		{name: "sampling supplies absent limit", requestSampling: map[string]any{"max_output_tokens": 5}, wantMax: 5},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			original := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = original })
			var requests, callbacks, done int
			check := func(body map[string]any) {
				t.Helper()
				if scenario.wantMax == 0 {
					if value, ok := body["max_output_tokens"]; ok {
						t.Errorf("unexpected output limit: %v", value)
					}
				} else {
					assert.EqualValues(t, scenario.wantMax, body["max_output_tokens"])
				}
				if body["model"] != "deployment" || body["stream"] != true || body["temperature"] != 0.0 || body["top_p"] != 0.7 || body["prompt_cache_key"] != "session" {
					t.Errorf("unrelated request fields changed: %v", body)
				}
				require.Len(t, body["input"], 1)
			}
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.URL.String() != "https://test.invalid/v1/responses?api-version=v1" || req.Header.Get("api-key") != "test-key" {
					t.Error("Azure endpoint or authentication changed")
				}
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				check(body)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}, nil
			})}
			model := &Model{
				ID: "test", API: APIAzureOpenAIResponses, Provider: ProviderAzureOpenAI,
				SamplingParams: scenario.modelSampling, SamplingParamsByThinkingLevel: map[string]map[string]any{"off": scenario.levelSampling},
			}
			opts := []Option{
				WithAPIKey("test-key"), WithAzureBaseURL("https://test.invalid/v1"), WithAzureAPIVersion("v1"), WithAzureDeploymentName("deployment"),
				WithTemperature(0), WithTopP(0.7), WithSessionID("session"), WithCacheRetention("short"), WithSamplingParams(scenario.requestSampling), WithPayloadCallback(func(payload any) any {
					callbacks++
					body, ok := payload.(map[string]any)
					if !ok {
						t.Fatalf("payload type = %T", payload)
					}
					check(body)
					return nil
				}),
			}
			if scenario.maxTokens != nil {
				opts = append(opts, WithMaxTokens(*scenario.maxTokens))
			}
			err := (&azureClient{model: model}).Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hello"}}}, func(_ context.Context, event StreamEvent) {
				if _, ok := event.(Done); ok {
					done++
				}
			}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			if requests != 1 || callbacks != 1 || done != 1 {
				t.Fatalf("requests=%d callbacks=%d done=%d", requests, callbacks, done)
			}
		})
	}
}

func TestAzureResponseStorageReachesTransport(t *testing.T) {
	for _, scenario := range []struct {
		name, session, retention, callback   string
		compat                               *CompatibilitySettings
		modelStore, levelStore, requestStore *bool
		wantPayload, wantWire                any
	}{
		{name: "no session", wantPayload: false, wantWire: false},
		{name: "cached session", session: "session", wantPayload: false, wantWire: false},
		{name: "cache disabled", session: "session", retention: "none", wantPayload: false, wantWire: false},
		{name: "compat false does not omit store", compat: &CompatibilitySettings{SupportsStore: new(false)}, wantPayload: false, wantWire: false},
		{name: "model sampling", modelStore: new(true), wantPayload: true, wantWire: true},
		{name: "thinking sampling", modelStore: new(true), levelStore: new(false), wantPayload: false, wantWire: false},
		{name: "request sampling", modelStore: new(true), levelStore: new(false), requestStore: new(true), wantPayload: true, wantWire: true},
		{name: "callback mutation", callback: "mutate", wantPayload: false, wantWire: true},
		{name: "callback replacement", callback: "replace", wantPayload: false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("PI_CACHE_RETENTION", "long")
			original := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = original })
			var requests, callbacks, done int
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				require.Equal(t, "https://test.invalid/v1/responses?api-version=v1", req.URL.String())
				require.Equal(t, "test-key", req.Header.Get("api-key"))
				var body map[string]any
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				if scenario.wantWire == nil {
					assert.NotContains(t, body, "store")
				} else {
					assert.Equal(t, scenario.wantWire, body["store"], "serialized storage policy")
				}
				assert.Equal(t, "deployment", body["model"])
				assert.Equal(t, true, body["stream"])
				require.Len(t, body["input"], 1)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}, nil
			})}
			model := &Model{ID: "test", API: APIAzureOpenAIResponses, Provider: ProviderAzureOpenAI, Compat: scenario.compat}
			if scenario.modelStore != nil {
				model.SamplingParams = map[string]any{"store": *scenario.modelStore}
			}
			if scenario.levelStore != nil {
				model.SamplingParamsByThinkingLevel = map[string]map[string]any{"off": {"store": *scenario.levelStore}}
			}
			opts := []Option{
				WithAPIKey("test-key"), WithAzureBaseURL("https://test.invalid/v1"), WithAzureAPIVersion("v1"), WithAzureDeploymentName("deployment"),
				WithSessionID(scenario.session), WithCacheRetention(scenario.retention), WithPayloadCallback(func(payload any) any {
					callbacks++
					body, ok := payload.(map[string]any)
					require.True(t, ok)
					assert.Equal(t, scenario.wantPayload, body["store"], "callback storage policy")
					if scenario.retention == "none" || scenario.session == "" {
						assert.NotContains(t, body, "prompt_cache_key")
					} else {
						assert.Equal(t, scenario.session, body["prompt_cache_key"])
					}
					switch scenario.callback {
					case "mutate":
						body["store"] = true
					case "replace":
						return map[string]any{"model": body["model"], "input": body["input"], "stream": true}
					}
					return nil
				}),
			}
			if scenario.requestStore != nil {
				opts = append(opts, WithSamplingParams(map[string]any{"store": *scenario.requestStore}))
			}
			err := (&azureClient{model: model}).Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hello"}}}, func(_ context.Context, event StreamEvent) {
				if _, ok := event.(Done); ok {
					done++
				}
			}, opts...)
			require.NoError(t, err)
			require.Equal(t, 1, requests)
			require.Equal(t, 1, callbacks)
			require.Equal(t, 1, done)
		})
	}
}
