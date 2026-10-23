package agent

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
	"testing/iotest"
	"time"

	"github.com/losinggeneration/tau/ai"
	"github.com/stretchr/testify/require"
)

func testProxyModel() *ai.Model {
	return &ai.Model{
		ID:       "test-model",
		API:      ai.APIOpenAIResponses,
		Provider: ai.ProviderOpenAI,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func withMockHTTPClient(t *testing.T, fn roundTripFunc) {
	t.Helper()

	prev := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: fn}
	t.Cleanup(func() {
		http.DefaultClient = prev
	})
}

// TestStreamProxyResponseEmitsTypedEvents verifies basic event typing.
func TestStreamProxyResponseEmitsTypedEvents(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"type":"start"}`,
		`data: {"type":"text_start","contentIndex":0}`,
		`data: {"type":"text_delta","contentIndex":0,"delta":"hello"}`,
		`data: {"type":"text_end","contentIndex":0}`,
		`data: {"type":"toolcall_start","contentIndex":1,"id":"call_1","toolName":"read"}`,
		`data: {"type":"toolcall_delta","contentIndex":1,"delta":"{\"path\":\"x\"}"}`,
		`data: {"type":"toolcall_end","contentIndex":1}`,
		`data: {"type":"done","reason":"toolUse","usage":{"input":1,"output":2,"totalTokens":3,"cost":{"total":0.1}}}`,
		"",
	}, "\n"))

	var events []ai.StreamEvent
	handler := func(_ context.Context, event ai.StreamEvent) {
		events = append(events, event)
	}

	if err := streamProxyResponse(context.Background(), testProxyModel(), body, handler); err != nil {
		t.Fatalf("streamProxyResponse: %v", err)
	}

	if len(events) < 8 {
		t.Fatalf("expected at least 8 events, got %d", len(events))
	}

	if _, ok := events[1].(ai.TextStart); !ok {
		t.Fatalf("event[1] = %T, want ai.TextStart", events[1])
	}
	if _, ok := events[2].(ai.TextDelta); !ok {
		t.Fatalf("event[2] = %T, want ai.TextDelta", events[2])
	}
	if _, ok := events[3].(ai.TextEnd); !ok {
		t.Fatalf("event[3] = %T, want ai.TextEnd", events[3])
	}
	if _, ok := events[4].(ai.ToolCallStart); !ok {
		t.Fatalf("event[4] = %T, want ai.ToolCallStart", events[4])
	}
	if _, ok := events[5].(ai.ToolCallDelta); !ok {
		t.Fatalf("event[5] = %T, want ai.ToolCallDelta", events[5])
	}
	if _, ok := events[6].(ai.ToolCallEnd); !ok {
		t.Fatalf("event[6] = %T, want ai.ToolCallEnd", events[6])
	}

	done, ok := events[len(events)-1].(ai.Done)
	if !ok {
		t.Fatalf("final event = %T, want ai.Done", events[len(events)-1])
	}
	if done.Reason != ai.StopReasonToolUse {
		t.Fatalf("done reason = %q, want %q", done.Reason, ai.StopReasonToolUse)
	}
}

// TestStreamProxyRequestContract verifies that StreamProxy sends the correct request
// body matching the reference contract.
func TestStreamProxyRequestContract(t *testing.T) {
	var receivedBody map[string]any

	withMockHTTPClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// Verify Content-Type header
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want %q", got, "application/json")
		}

		// Verify Authorization header
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
		}

		// Verify URL ends with /api/stream
		if !strings.HasSuffix(r.URL.Path, "/api/stream") {
			t.Errorf("URL path = %q, want path ending with %q", r.URL.Path, "/api/stream")
		}

		// Decode request body
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("Failed to decode request body: %v", err)
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"done\",\"reason\":\"stop\",\"usage\":{\"input\":0,\"output\":0,\"totalTokens\":0}}\n\n")),
		}, nil
	}))

	model := &ai.Model{
		ID:       "gpt-4o",
		Provider: "openai",
		API:      "openai-responses",
	}
	aiCtx := &ai.Context{
		Messages: []ai.Message{
			{Role: "user", Content: "Hello"},
		},
	}

	handler := func(ctx context.Context, event ai.StreamEvent) {}

	err := StreamProxy(context.Background(), model, aiCtx, handler, "https://proxy.example", "test-token",
		ai.WithTemperature(0.7),
		ai.WithMaxTokens(1000),
	)
	if err != nil {
		t.Fatalf("StreamProxy returned error: %v", err)
	}

	// H1.1: Verify full model object is sent, not just model.ID
	modelValue := receivedBody["model"]
	if modelValue == nil {
		t.Error("request body missing 'model' field")
	} else {
		modelMap, ok := modelValue.(map[string]any)
		if !ok {
			t.Fatalf("model field is not a map, got %T", modelValue)
		}
		// Verify we get the full model structure
		if id, ok := modelMap["id"]; ok && id != "gpt-4o" {
			t.Errorf("model.id = %v, want %q", id, "gpt-4o")
		}
		if provider, ok := modelMap["provider"]; ok && provider != "openai" {
			t.Errorf("model.provider = %v, want %q", provider, "openai")
		}
		if api, ok := modelMap["api"]; ok && api != "openai-responses" {
			t.Errorf("model.api = %v, want %q", api, "openai-responses")
		}
	}

	// H1.1: Verify context is included
	if _, ok := receivedBody["context"]; !ok {
		t.Error("request body missing 'context' field")
	}

	// H1.2: Verify options field is present with forwarded values
	options, ok := receivedBody["options"]
	if !ok {
		t.Error("request body missing 'options' field - simple-stream options not forwarded")
	} else {
		optsMap, ok := options.(map[string]any)
		if !ok {
			t.Fatalf("options field is not a map, got %T", options)
		}
		// Verify temperature was forwarded
		if temp, ok := optsMap["temperature"]; !ok {
			t.Errorf("options.temperature not found")
		} else if temp != 0.7 {
			t.Errorf("options.temperature = %v, want 0.7", temp)
		}
		// Verify maxTokens was forwarded
		if maxTok, ok := optsMap["maxTokens"]; !ok {
			t.Errorf("options.maxTokens not found")
		} else if maxTok != float64(1000) {
			t.Errorf("options.maxTokens = %v, want 1000", maxTok)
		}
	}
}

// TestStreamProxyURLConstruction verifies that the proxy URL is correctly
// constructed with the /api/stream suffix.
func TestStreamProxyURLConstruction(t *testing.T) {
	tests := []struct {
		name       string
		inputURL   string
		wantSuffix string
	}{
		{
			name:       "URL without trailing slash",
			inputURL:   "http://localhost:8080",
			wantSuffix: "/api/stream",
		},
		{
			name:       "URL with trailing slash",
			inputURL:   "http://localhost:8080/",
			wantSuffix: "/api/stream",
		},
		{
			name:       "URL already has /api/stream",
			inputURL:   "http://localhost:8080/api/stream",
			wantSuffix: "/api/stream",
		},
		{
			name:       "URL with path prefix",
			inputURL:   "http://localhost:8080/proxy",
			wantSuffix: "/api/stream",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedURL string

			withMockHTTPClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				receivedURL = r.URL.Path
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"done\",\"reason\":\"stop\",\"usage\":{\"input\":0,\"output\":0,\"totalTokens\":0}}\n\n")),
				}, nil
			}))

			model := &ai.Model{ID: "gpt-4o"}
			handler := func(ctx context.Context, event ai.StreamEvent) {}

			err := StreamProxy(context.Background(), model, &ai.Context{}, handler, tt.inputURL, "token")
			if err != nil {
				t.Fatalf("StreamProxy returned error: %v", err)
			}

			if !strings.HasSuffix(receivedURL, tt.wantSuffix) {
				t.Errorf("URL path = %q, want path ending with %q", receivedURL, tt.wantSuffix)
			}
		})
	}
}

// TestStreamProxyEmptyOptions verifies that empty options don't add extra fields.
func TestStreamProxyEmptyOptions(t *testing.T) {
	var receivedBody map[string]any

	withMockHTTPClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("Failed to decode request body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"done\",\"reason\":\"stop\",\"usage\":{\"input\":0,\"output\":0,\"totalTokens\":0}}\n\n")),
		}, nil
	}))

	model := &ai.Model{ID: "gpt-4o"}
	handler := func(ctx context.Context, event ai.StreamEvent) {}

	// Pass no options
	err := StreamProxy(context.Background(), model, &ai.Context{}, handler, "https://proxy.example", "token")
	if err != nil {
		t.Fatalf("StreamProxy returned error: %v", err)
	}

	// Verify options field exists but only contains explicitly set values
	opts, ok := receivedBody["options"].(map[string]any)
	if !ok {
		t.Fatal("options field missing or not a map")
	}

	// Should only have fields that were explicitly set (none in this case)
	// Note: temperature/maxTokens/reasoning should only be added if they have values
	if _, ok := opts["temperature"]; ok {
		t.Error("temperature should not be included when no temperature option was set")
	}
	if _, ok := opts["maxTokens"]; ok {
		t.Error("maxTokens should not be included when no maxTokens option was set")
	}
	if _, ok := opts["reasoning"]; ok {
		t.Error("reasoning should not be included when no reasoning option was set")
	}
}

// TestCompleteProxyRequestContract verifies that CompleteProxy also sends the
// correct request body.
func TestCompleteProxyRequestContract(t *testing.T) {
	var receivedBody map[string]any

	withMockHTTPClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("Failed to decode request body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"done\",\"reason\":\"stop\",\"usage\":{\"input\":0,\"output\":0,\"totalTokens\":0}}\n\n")),
		}, nil
	}))

	model := &ai.Model{ID: "gpt-4o"}
	aiCtx := &ai.Context{
		Messages: []ai.Message{
			{Role: "user", Content: "Hello"},
		},
	}

	_, err := CompleteProxy(context.Background(), model, aiCtx, "https://proxy.example", "test-token",
		ai.WithTemperature(0.5),
		ai.WithMaxTokens(500),
	)
	if err != nil {
		t.Fatalf("CompleteProxy returned error: %v", err)
	}

	// Verify model is the full object
	modelValue := receivedBody["model"]
	if modelValue == nil {
		t.Fatal("request body missing 'model' field")
	}

	// Verify options are forwarded
	opts, ok := receivedBody["options"].(map[string]any)
	if !ok {
		t.Fatal("options field missing or not a map")
	}

	if temp, ok := opts["temperature"]; !ok {
		t.Errorf("temperature not found")
	} else if temp != 0.5 {
		t.Errorf("temperature = %v, want 0.5", temp)
	}
	if maxTok, ok := opts["maxTokens"]; !ok {
		t.Errorf("maxTokens not found")
	} else if maxTok != float64(500) {
		t.Errorf("maxTokens = %v, want 500", maxTok)
	}
}

// TestProxyContentSignatureText verifies that text_end events include contentSignature
// matching pi-mono's behavior (proxy.ts:239-246).
func TestProxyContentSignatureText(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"type":"start"}`,
		`data: {"type":"text_start","contentIndex":0}`,
		`data: {"type":"text_delta","contentIndex":0,"delta":"Hello "}`,
		`data: {"type":"text_delta","contentIndex":0,"delta":"World"}`,
		`data: {"type":"text_end","contentIndex":0,"contentSignature":"sig_abc123"}`,
		`data: {"type":"done","reason":"stop","usage":{"input":0,"output":0,"totalTokens":0}}`,
		"",
	}, "\n"))

	var events []ai.StreamEvent
	handler := func(_ context.Context, event ai.StreamEvent) {
		events = append(events, event)
	}

	if err := streamProxyResponse(context.Background(), testProxyModel(), body, handler); err != nil {
		t.Fatalf("streamProxyResponse: %v", err)
	}

	// Find the text_end event
	var foundTextEnd bool
	for _, event := range events {
		if te, ok := event.(ai.TextEnd); ok {
			foundTextEnd = true
			if te.Content != "Hello World" {
				t.Errorf("text content = %q, want %q", te.Content, "Hello World")
			}
			if te.ContentSignature != "sig_abc123" {
				t.Errorf("contentSignature = %q, want %q", te.ContentSignature, "sig_abc123")
			}
			// Verify partial message has the signature
			if te.Partial.Content[0].TextSignature != "sig_abc123" {
				t.Errorf("partial content signature = %q, want %q", te.Partial.Content[0].TextSignature, "sig_abc123")
			}
		}
	}

	if !foundTextEnd {
		t.Error("text_end event not found")
	}
}

// TestProxyContentSignatureThinking verifies that thinking_end events include contentSignature
// matching pi-mono's behavior (proxy.ts:269-278).
func TestProxyContentSignatureThinking(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"type":"start"}`,
		`data: {"type":"thinking_start","contentIndex":0}`,
		`data: {"type":"thinking_delta","contentIndex":0,"delta":"Let me think"}`,
		`data: {"type":"thinking_delta","contentIndex":0,"delta":" about this"}`,
		`data: {"type":"thinking_end","contentIndex":0,"contentSignature":"think_sig_xyz"}`,
		`data: {"type":"text_start","contentIndex":1}`,
		`data: {"type":"text_end","contentIndex":1}`,
		`data: {"type":"done","reason":"stop","usage":{"input":0,"output":0,"totalTokens":0}}`,
		"",
	}, "\n"))

	var events []ai.StreamEvent
	handler := func(_ context.Context, event ai.StreamEvent) {
		events = append(events, event)
	}

	if err := streamProxyResponse(context.Background(), testProxyModel(), body, handler); err != nil {
		t.Fatalf("streamProxyResponse: %v", err)
	}

	// Find the thinking_end event
	var foundThinkingEnd bool
	for _, event := range events {
		if te, ok := event.(ai.ThinkingEnd); ok {
			foundThinkingEnd = true
			if te.Content != "Let me think about this" {
				t.Errorf("thinking content = %q, want %q", te.Content, "Let me think about this")
			}
			if te.ContentSignature != "think_sig_xyz" {
				t.Errorf("contentSignature = %q, want %q", te.ContentSignature, "think_sig_xyz")
			}
			// Verify partial message has the signature
			if te.Partial.Content[0].ThoughtSignature != "think_sig_xyz" {
				t.Errorf("partial thought signature = %q, want %q", te.Partial.Content[0].ThoughtSignature, "think_sig_xyz")
			}
		}
	}

	if !foundThinkingEnd {
		t.Error("thinking_end event not found")
	}
}

// TestProxyPartialJSONToolCall verifies that tool call arguments are parsed using
// partial JSON parsing (pi-mono: proxy.ts:293-298).
func TestProxyPartialJSONToolCall(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"type":"start"}`,
		`data: {"type":"toolcall_start","contentIndex":0,"id":"call_1","toolName":"getWeather"}`,
		`data: {"type":"toolcall_delta","contentIndex":0,"delta":"{\"city\":"}`,
		`data: {"type":"toolcall_delta","contentIndex":0,"delta":"\"San Francisco\""}`,
		`data: {"type":"toolcall_delta","contentIndex":0,"delta":","}`,
		`data: {"type":"toolcall_delta","contentIndex":0,"delta":"\"unit\":\"fahrenheit\""}`,
		`data: {"type":"toolcall_delta","contentIndex":0,"delta":"}"}`,
		`data: {"type":"toolcall_end","contentIndex":0}`,
		`data: {"type":"done","reason":"toolUse","usage":{"input":0,"output":0,"totalTokens":0}}`,
		"",
	}, "\n"))

	var events []ai.StreamEvent
	handler := func(_ context.Context, event ai.StreamEvent) {
		events = append(events, event)
	}

	if err := streamProxyResponse(context.Background(), testProxyModel(), body, handler); err != nil {
		t.Fatalf("streamProxyResponse: %v", err)
	}

	// Find the toolcall_end event and verify final arguments
	var foundToolCallEnd bool
	for _, event := range events {
		if tce, ok := event.(ai.ToolCallEnd); ok {
			foundToolCallEnd = true
			if tce.ToolCall.ID != "call_1" {
				t.Errorf("tool call ID = %q, want %q", tce.ToolCall.ID, "call_1")
			}
			if tce.ToolCall.Name != "getWeather" {
				t.Errorf("tool call name = %q, want %q", tce.ToolCall.Name, "getWeather")
			}
			// Verify the arguments are parsed as valid JSON
			var args map[string]any
			if err := json.Unmarshal(tce.ToolCall.Arguments, &args); err != nil {
				t.Errorf("tool call arguments are not valid JSON: %v", err)
			}
			if city, ok := args["city"].(string); !ok || city != "San Francisco" {
				t.Errorf("city = %v (%T), want %q", args["city"], args["city"], "San Francisco")
			}
			if unit, ok := args["unit"].(string); !ok || unit != "fahrenheit" {
				t.Errorf("unit = %v (%T), want %q", args["unit"], args["unit"], "fahrenheit")
			}
		}
	}

	if !foundToolCallEnd {
		t.Error("toolcall_end event not found")
	}
}

// TestProxyPartialJSONToolCallIncomplete verifies that incomplete JSON is handled
// gracefully with partial parsing.
func TestProxyPartialJSONToolCallIncomplete(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"type":"start"}`,
		`data: {"type":"toolcall_start","contentIndex":0,"id":"call_2","toolName":"search"}`,
		// Send incomplete JSON - missing closing brace
		`data: {"type":"toolcall_delta","contentIndex":0,"delta":"{\"query\""}`,
		`data: {"type":"toolcall_delta","contentIndex":0,"delta":": \"test\""}`,
		// Intentionally incomplete - no final delta
		`data: {"type":"toolcall_end","contentIndex":0}`,
		`data: {"type":"done","reason":"stop","usage":{"input":0,"output":0,"totalTokens":0}}`,
		"",
	}, "\n"))

	var events []ai.StreamEvent
	handler := func(_ context.Context, event ai.StreamEvent) {
		events = append(events, event)
	}

	if err := streamProxyResponse(context.Background(), testProxyModel(), body, handler); err != nil {
		t.Fatalf("streamProxyResponse: %v", err)
	}

	// Find the toolcall_end event - should have empty/partial JSON
	var foundToolCallEnd bool
	for _, event := range events {
		if tce, ok := event.(ai.ToolCallEnd); ok {
			foundToolCallEnd = true
			// Partial JSON parsing should return {} for incomplete input
			var args map[string]any
			if err := json.Unmarshal(tce.ToolCall.Arguments, &args); err != nil {
				t.Errorf("tool call arguments should be valid JSON: %v", err)
			}
			// When the JSON is incomplete, ParseStreamingJSON returns {}
			if len(args) != 0 {
				t.Logf("partial JSON parsed as: %v (may be empty for incomplete)", args)
			}
		}
	}

	if !foundToolCallEnd {
		t.Error("toolcall_end event not found")
	}
}

// TestProxyEndToEndEventFlow verifies the complete event flow from start to done
// including all event types.
func TestProxyEndToEndEventFlow(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"type":"start"}`,
		`data: {"type":"text_start","contentIndex":0}`,
		`data: {"type":"text_delta","contentIndex":0,"delta":"I need to "}`,
		`data: {"type":"text_delta","contentIndex":0,"delta":"call a function."}`,
		`data: {"type":"text_end","contentIndex":0,"contentSignature":"text_sig_1"}`,
		`data: {"type":"toolcall_start","contentIndex":1,"id":"call_test","toolName":"testTool"}`,
		`data: {"type":"toolcall_delta","contentIndex":1,"delta":"{\"arg\":true}"}`,
		`data: {"type":"toolcall_end","contentIndex":1}`,
		`data: {"type":"done","reason":"toolUse","usage":{"input":100,"output":50,"totalTokens":150}}`,
		"",
	}, "\n"))

	var events []ai.StreamEvent
	handler := func(_ context.Context, event ai.StreamEvent) {
		events = append(events, event)
	}

	if err := streamProxyResponse(context.Background(), testProxyModel(), body, handler); err != nil {
		t.Fatalf("streamProxyResponse: %v", err)
	}

	// Verify event sequence
	eventTypes := make([]string, 0, len(events))
	for _, e := range events {
		switch e.(type) {
		case ai.Start:
			eventTypes = append(eventTypes, "start")
		case ai.TextStart:
			eventTypes = append(eventTypes, "text_start")
		case ai.TextDelta:
			eventTypes = append(eventTypes, "text_delta")
		case ai.TextEnd:
			eventTypes = append(eventTypes, "text_end")
		case ai.ToolCallStart:
			eventTypes = append(eventTypes, "toolcall_start")
		case ai.ToolCallDelta:
			eventTypes = append(eventTypes, "toolcall_delta")
		case ai.ToolCallEnd:
			eventTypes = append(eventTypes, "toolcall_end")
		case ai.Done:
			eventTypes = append(eventTypes, "done")
		}
	}

	expected := []string{"start", "text_start", "text_delta", "text_delta", "text_end", "toolcall_start", "toolcall_delta", "toolcall_end", "done"}
	if len(eventTypes) != len(expected) {
		t.Fatalf("event count = %d, want %d", len(eventTypes), len(expected))
	}

	for i := range expected {
		if eventTypes[i] != expected[i] {
			t.Errorf("event[%d] = %s, want %s", i, eventTypes[i], expected[i])
		}
	}

	// Verify final message has usage
	done, ok := events[len(events)-1].(ai.Done)
	if !ok {
		t.Fatal("final event is not Done")
	}
	if done.Reason != ai.StopReasonToolUse {
		t.Errorf("stop reason = %q, want %q", done.Reason, ai.StopReasonToolUse)
	}
	if done.Message.Usage.TotalTokens != 150 {
		t.Errorf("total tokens = %d, want %d", done.Message.Usage.TotalTokens, 150)
	}
}

// TestProxyErrorEvent verifies error event handling matches pi-mono (proxy.ts:328-332).
func TestProxyErrorEvent(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"type":"start"}`,
		`data: {"type":"text_start","contentIndex":0}`,
		`data: {"type":"text_delta","contentIndex":0,"delta":"Hello"}`,
		`data: {"type":"error","reason":"error","errorMessage":"Model rate limited","usage":{"input":10,"output":5,"totalTokens":15}}`,
		"",
	}, "\n"))

	var events []ai.StreamEvent
	handler := func(_ context.Context, event ai.StreamEvent) {
		events = append(events, event)
	}

	if err := streamProxyResponse(context.Background(), testProxyModel(), body, handler); err != nil {
		t.Fatalf("streamProxyResponse: %v", err)
	}

	// Find the error event
	var foundError bool
	for _, event := range events {
		if se, ok := event.(ai.StreamError); ok {
			foundError = true
			if se.Reason != "error" {
				t.Errorf("error reason = %q, want %q", se.Reason, "error")
			}
			if se.Msg.ErrorMessage != "Model rate limited" {
				t.Errorf("error message = %q, want %q", se.Msg.ErrorMessage, "Model rate limited")
			}
		}
	}

	if !foundError {
		t.Error("error event not found")
	}
}

// TestProxyUsageOnDone verifies usage is correctly populated on done events.
func TestProxyUsageOnDone(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"type":"start"}`,
		`data: {"type":"text_start","contentIndex":0}`,
		`data: {"type":"text_end","contentIndex":0}`,
		`data: {"type":"done","reason":"stop","usage":{"input":100,"output":200,"cacheRead":50,"cacheWrite":25,"totalTokens":300,"cost":{"input":0.01,"output":0.02,"cacheRead":0.005,"cacheWrite":0.0025,"total":0.0375}}}`,
		"",
	}, "\n"))

	var events []ai.StreamEvent
	handler := func(_ context.Context, event ai.StreamEvent) {
		events = append(events, event)
	}

	if err := streamProxyResponse(context.Background(), testProxyModel(), body, handler); err != nil {
		t.Fatalf("streamProxyResponse: %v", err)
	}

	done, ok := events[len(events)-1].(ai.Done)
	if !ok {
		t.Fatal("final event is not Done")
	}

	if done.Message.Usage.Input != 100 {
		t.Errorf("input tokens = %d, want %d", done.Message.Usage.Input, 100)
	}
	if done.Message.Usage.Output != 200 {
		t.Errorf("output tokens = %d, want %d", done.Message.Usage.Output, 200)
	}
	if done.Message.Usage.CacheRead != 50 {
		t.Errorf("cache read = %d, want %d", done.Message.Usage.CacheRead, 50)
	}
	if done.Message.Usage.CacheWrite != 25 {
		t.Errorf("cache write = %d, want %d", done.Message.Usage.CacheWrite, 25)
	}
	if done.Message.Usage.TotalTokens != 300 {
		t.Errorf("total tokens = %d, want %d", done.Message.Usage.TotalTokens, 300)
	}
	if done.Message.Usage.Cost.Total != 0.0375 {
		t.Errorf("total cost = %f, want %f", done.Message.Usage.Cost.Total, 0.0375)
	}
}

func TestStreamProxyNonOKEmitsTerminalStreamError(t *testing.T) {
	// Verify that a non-OK HTTP response emits a terminal StreamError event
	// before returning the error. This matches the TypeScript reference behavior.
	withMockHTTPClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Body:       io.NopCloser(strings.NewReader(`{"error":"rate limited"}`)),
		}, nil
	}))

	var streamEvents []ai.StreamEvent
	handler := func(_ context.Context, event ai.StreamEvent) {
		streamEvents = append(streamEvents, event)
	}

	err := StreamProxy(
		context.Background(),
		testProxyModel(),
		&ai.Context{},
		handler,
		"https://proxy.example.com/api/stream",
		"token",
	)

	if err == nil {
		t.Fatal("expected error for non-OK response")
	}

	// Should have emitted a StreamError event before returning
	var foundError bool
	for _, event := range streamEvents {
		if se, ok := event.(ai.StreamError); ok {
			foundError = true
			if se.Reason != "error" {
				t.Errorf("StreamError reason = %q, want error", se.Reason)
			}
			if se.Msg.ErrorMessage == "" {
				t.Error("StreamError should have an error message")
			}
			if se.Msg.StopReason != "error" {
				t.Errorf("msg.StopReason = %q, want error", se.Msg.StopReason)
			}
		}
	}
	if !foundError {
		t.Error("expected StreamError event for non-OK HTTP response")
	}
}

func TestStreamProxyResponseMalformedJSONEmitsTerminalError(t *testing.T) {
	// Verify that malformed JSON in the proxy stream emits a terminal StreamError
	// and returns an error rather than silently continuing.
	body := strings.NewReader(strings.Join([]string{
		`data: {"type":"start"}`,
		`data: {"type":"text_start","contentIndex":0}`,
		`data: NOT_VALID_JSON_HERE`,
		`data: {"type":"text_delta","contentIndex":0,"delta":"hello"}`,
		`data: {"type":"done","reason":"stop"}`,
		"",
	}, "\n"))

	var streamEvents []ai.StreamEvent
	var err error
	handler := func(_ context.Context, event ai.StreamEvent) {
		streamEvents = append(streamEvents, event)
	}

	err = streamProxyResponse(context.Background(), testProxyModel(), body, handler)

	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}

	// Should have emitted a StreamError for the malformed line
	var foundError bool
	for _, event := range streamEvents {
		if _, ok := event.(ai.StreamError); ok {
			foundError = true
			break
		}
	}
	if !foundError {
		t.Error("expected StreamError event for malformed JSON line")
	}
}

func TestStreamProxyResponseTerminalOutcomesMatchUpstream(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name, body, message string
		ctx                 context.Context
		reason              ai.StopReason
		done                bool
	}{
		{name: "unterminated final done", body: "data: {\"type\":\"start\"}\ndata: {\"type\":\"done\",\"reason\":\"stop\"}", reason: ai.StopReasonStop, done: true},
		{name: "closed early", body: "data: {\"type\":\"start\"}\n", reason: ai.StopReasonError, message: "connection closed by proxy server before the response completed"},
		{name: "canceled", ctx: canceled, body: "data: {\"type\":\"start\"}\n", reason: ai.StopReasonAborted, message: "request aborted by user"},
		{name: "malformed", body: "data: nope\n", reason: ai.StopReasonError, message: "proxy: malformed event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			var terminal []ai.StreamEvent
			_ = streamProxyResponse(ctx, testProxyModel(), strings.NewReader(tc.body), func(_ context.Context, event ai.StreamEvent) {
				switch event.(type) {
				case ai.Done, ai.StreamError:
					terminal = append(terminal, event)
				}
			})
			if len(terminal) != 1 {
				t.Fatalf("terminal = %#v, want one", terminal)
			}
			switch e := terminal[0].(type) {
			case ai.Done:
				if !tc.done || e.Reason != tc.reason {
					t.Fatalf("done %s, want %s %q", e.Reason, tc.reason, tc.message)
				}
			case ai.StreamError:
				if tc.done || e.Msg.StopReason != tc.reason || !strings.HasPrefix(e.Msg.ErrorMessage, tc.message) {
					t.Fatalf("error %s %q, want %s %q", e.Msg.StopReason, e.Msg.ErrorMessage, tc.reason, tc.message)
				}
			}
		})
	}
}

func TestStreamProxyRequestFailureEmitsTerminalError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	var failures []ai.StreamError
	err := StreamProxy(context.Background(), testProxyModel(), &ai.Context{}, func(_ context.Context, event ai.StreamEvent) {
		if e, ok := event.(ai.StreamError); ok {
			failures = append(failures, e)
		}
	}, server.URL, "")
	if err == nil || len(failures) != 1 || failures[0].Msg.StopReason != ai.StopReasonError || failures[0].Msg.ErrorMessage != err.Error() {
		t.Fatalf("err=%v failures=%+v", err, failures)
	}
}

func TestExtractSimpleStreamOptionsForwardsSamplingParams(t *testing.T) {
	options := extractSimpleStreamOptions([]ai.Option{ai.WithSamplingParams(map[string]any{"top_k": 40})})
	params, ok := options["samplingParams"].(map[string]any)
	if !ok || params["top_k"] != 40 {
		t.Fatalf("options = %v, want samplingParams forwarded", options)
	}
	if options := extractSimpleStreamOptions([]ai.Option{ai.WithTransport(ai.TransportWS)}); options["transport"] != ai.TransportWS {
		t.Fatalf("options = %v, want transport forwarded", options)
	}
}

func TestStreamProxyReconstructsIndexedContentAndFinalMetadata(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"start"}`,
		`data: {"type":"text_start","contentIndex":0}`,
		`data: {"type":"text_delta","contentIndex":0,"delta":"discard"}`,
		`data: {"type":"text_start","contentIndex":0}`,
		`data: {"type":"text_delta","contentIndex":0,"delta":"kept"}`,
		`data: {"type":"text_end","contentIndex":0,"contentSignature":"text-sig"}`,
		`data: {"type":"toolcall_start","contentIndex":1,"id":"first","toolName":"read"}`,
		`data: {"type":"toolcall_start","contentIndex":2,"id":"second","toolName":"write"}`,
		`data: {"type":"toolcall_delta","contentIndex":1,"delta":"{\"path\":\"a\"}"}`,
		`data: {"type":"toolcall_delta","contentIndex":2,"delta":"{\"path\":\"b\"}"}`,
		`data: {"type":"toolcall_end","contentIndex":2}`,
		`data: {"type":"toolcall_end","contentIndex":1,"toolCall":{"id":"final-first","name":"lookup","arguments":{"path":"final"}}}`,
		`data: {"type":"done","reason":"toolUse","providerThinkingLevel":"high","usage":{"totalTokens":17}}`,
	}, "\n")
	var done ai.Done
	var calls []ai.ToolCall
	err := streamProxyResponse(t.Context(), testProxyModel(), strings.NewReader(body), func(_ context.Context, event ai.StreamEvent) {
		switch e := event.(type) {
		case ai.Done:
			done = e
		case ai.ToolCallEnd:
			calls = append(calls, e.ToolCall)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(done.Message.Content) != 3 || len(calls) != 2 {
		t.Fatalf("content=%+v calls=%+v", done.Message.Content, calls)
	}
	content := done.Message.Content
	if content[0].Text != "kept" || content[0].TextSignature != "text-sig" ||
		content[1].ID != "final-first" || content[1].Name != "lookup" || string(content[1].Arguments) != `{"path":"final"}` ||
		content[2].ID != "second" || string(content[2].Arguments) != `{"path":"b"}` ||
		calls[0].ID != "second" || calls[1].ID != "final-first" ||
		done.Message.ProviderThinkingLevel != "high" || done.Message.Usage.TotalTokens != 17 {
		t.Fatalf("done=%+v calls=%+v", done, calls)
	}
}

func TestStreamProxyRejectsInvalidContentEvents(t *testing.T) {
	for _, event := range []string{
		`{"type":"text_delta","contentIndex":-1,"delta":"bad"}`,
		`{"type":"thinking_start","contentIndex":-1}`,
		`{"type":"toolcall_start","contentIndex":-1}`,
		`{"type":"text_start","contentIndex":2147483647}`,
		`{"type":"text_delta","contentIndex":1,"delta":"bad"}`,
		`{"type":"text_end","contentIndex":1}`,
		`{"type":"thinking_delta","contentIndex":0,"delta":"bad"}`,
		`{"type":"thinking_end","contentIndex":0}`,
		`{"type":"toolcall_delta","contentIndex":0,"delta":"{}"}`,
		`{"type":"toolcall_end","contentIndex":0}`,
		`{"type":"done","reason":"error"}`,
		`{"type":"error","reason":"stop"}`,
	} {
		t.Run(event, func(t *testing.T) {
			body := "data: {\"type\":\"text_start\",\"contentIndex\":0}\ndata: " + event + "\ndata: {\"type\":\"done\",\"reason\":\"stop\"}\n"
			var terminal []ai.StreamEvent
			err := streamProxyResponse(t.Context(), testProxyModel(), strings.NewReader(body), func(_ context.Context, event ai.StreamEvent) {
				switch event.(type) {
				case ai.Done, ai.StreamError:
					terminal = append(terminal, event)
				}
			})
			if err == nil || len(terminal) != 1 {
				t.Fatalf("err=%v terminal=%+v", err, terminal)
			}
			failure, ok := terminal[0].(ai.StreamError)
			if !ok || failure.Msg.StopReason != ai.StopReasonError {
				t.Fatalf("terminal=%+v", terminal)
			}
		})
	}
}

func TestStreamProxyStopsAtFirstTerminalEvent(t *testing.T) {
	for _, terminal := range []string{
		`{"type":"done","reason":"stop","providerThinkingLevel":"high"}`,
		`{"type":"error","reason":"aborted","errorMessage":"stopped","providerThinkingLevel":"low"}`,
	} {
		t.Run(terminal, func(t *testing.T) {
			body := "data: \n\ndata: " + terminal + "\ndata: NOT_JSON\n"
			var events []ai.StreamEvent
			err := streamProxyResponse(t.Context(), testProxyModel(), strings.NewReader(body), func(_ context.Context, event ai.StreamEvent) {
				events = append(events, event)
			})
			if err != nil || len(events) != 1 {
				t.Fatalf("err=%v events=%+v", err, events)
			}
			switch e := events[0].(type) {
			case ai.Done:
				if e.Message.ProviderThinkingLevel != "high" {
					t.Fatalf("done=%+v", e)
				}
			case ai.StreamError:
				if e.Msg.ProviderThinkingLevel != "low" || e.Msg.ErrorMessage != "stopped" {
					t.Fatalf("error=%+v", e)
				}
			default:
				t.Fatalf("event=%T", events[0])
			}
		})
	}
}

type proxyTestBody struct {
	io.Reader
	closeErr error
	closed   int
}

func (b *proxyTestBody) Close() error {
	b.closed++
	return b.closeErr
}

func TestStreamProxyRetainsCleanupAndCancellationCauses(t *testing.T) {
	closeErr := errors.New("proxy body close failed")
	cancelErr := errors.New("proxy caller stopped")
	for _, tc := range []struct {
		name, body string
		status     int
		cancel     bool
	}{
		{name: "success", status: http.StatusOK, body: `data: {"type":"done","reason":"stop"}`},
		{name: "http failure", status: http.StatusForbidden, body: "denied"},
		{name: "parser failure", status: http.StatusOK, body: "data: nope"},
		{name: "cancelled read", status: http.StatusOK, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			reader := io.Reader(strings.NewReader(tc.body))
			if tc.cancel {
				reader = iotest.ErrReader(io.ErrUnexpectedEOF)
			}
			body := &proxyTestBody{Reader: reader, closeErr: closeErr}
			withMockHTTPClient(t, roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				if tc.cancel {
					cancel(cancelErr)
				}
				return &http.Response{StatusCode: tc.status, Body: body}, nil
			}))
			var events []ai.StreamEvent
			err := StreamProxy(ctx, testProxyModel(), &ai.Context{}, func(_ context.Context, event ai.StreamEvent) {
				events = append(events, event)
			}, "https://proxy.invalid", "")
			if !errors.Is(err, closeErr) || body.closed != 1 || len(events) != 1 {
				t.Fatalf("err=%v closed=%d events=%+v", err, body.closed, events)
			}
			if tc.cancel && !errors.Is(err, cancelErr) {
				t.Fatalf("err=%v, want cancellation cause", err)
			}
			if tc.name == "http failure" && !strings.Contains(err.Error(), "403") {
				t.Fatalf("err=%v, want HTTP failure", err)
			}
		})
	}
}

func TestStreamProxySetupFailuresEmitOneTerminalError(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		opts      []ai.Option
	}{
		{name: "bad URL", url: "://invalid"},
		{name: "unserializable metadata", url: "https://proxy.invalid", opts: []ai.Option{ai.WithMetadata(map[string]any{"invalid": make(chan int)})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var failures []ai.StreamError
			err := StreamProxy(t.Context(), testProxyModel(), &ai.Context{}, func(_ context.Context, event ai.StreamEvent) {
				if failure, ok := event.(ai.StreamError); ok {
					failures = append(failures, failure)
				}
			}, tc.url, "", tc.opts...)
			if err == nil || len(failures) != 1 || failures[0].Msg.ErrorMessage != err.Error() {
				t.Fatalf("err=%v failures=%+v", err, failures)
			}
		})
	}
}

func TestStreamProxyForwardsSerializableOptions(t *testing.T) {
	var received map[string]json.RawMessage
	withMockHTTPClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request struct {
			Options map[string]json.RawMessage `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		received = request.Options
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`data: {"type":"done","reason":"stop"}`))}, nil
	}))
	_, err := CompleteProxy(t.Context(), testProxyModel(), &ai.Context{}, "https://proxy.invalid", "proxy-token",
		ai.WithTemperature(0), ai.WithMaxTokens(128), ai.WithReasoning(ai.ReasoningOff),
		ai.WithThinkingBudgets(&ai.ThinkingBudgets{Low: new(0), High: new(3000)}),
		ai.WithCacheRetention("none"), ai.WithSessionID("session"),
		ai.WithHeaders(map[string]string{"X-Provider": "value"}), ai.WithMetadata(map[string]any{"tag": "value"}),
		ai.WithSamplingParams(map[string]any{"top_k": 3}), ai.WithTransport(ai.TransportSSE), ai.WithMaxRetryDelayMs(0),
		ai.WithAPIKey("not-forwarded"), ai.WithPayloadCallback(func(payload any) any { return payload }),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"thinkingBudgets": `{"low":0,"high":3000}`,
		"temperature":     "0", "maxTokens": "128", "reasoning": `"off"`, "cacheRetention": `"none"`, "sessionId": `"session"`,
		"headers": `{"X-Provider":"value"}`, "metadata": `{"tag":"value"}`, "samplingParams": `{"top_k":3}`, "transport": `"sse"`, "maxRetryDelayMs": "0",
	}
	if len(received) != len(want) {
		t.Errorf("options=%s, want exactly %d serializable fields", received, len(want))
	}
	for key, value := range want {
		if string(received[key]) != value {
			t.Errorf("%s=%s, want %s", key, received[key], value)
		}
	}
}

func TestProxyOptionsForwardingAndSignalCancellation(t *testing.T) {
	signal := make(chan struct{})
	admitted := make(chan struct{})
	withMockHTTPClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request struct {
			Options map[string]any `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		if request.Options["sessionId"] != "wrapper-session" || request.Options["cacheRetention"] != "none" || request.Options["maxRetryDelayMs"] != float64(0) || !reflect.DeepEqual(request.Options["thinkingBudgets"], map[string]any{"low": float64(0), "high": float64(3000)}) {
			t.Errorf("options=%+v", request.Options)
		}
		close(admitted)
		<-r.Context().Done()
		return nil, context.Cause(r.Context())
	}))
	finished := make(chan error, 1)
	go func() {
		_, err := CompleteProxyWithOptions(t.Context(), testProxyModel(), &ai.Context{}, ProxyStreamOptions{
			ProxyURL: "https://proxy.invalid", Signal: signal, SessionID: "wrapper-session", CacheRetention: "none", MaxRetryDelayMs: new(0),
			ThinkingBudgets: &ai.ThinkingBudgets{Low: new(0), High: new(3000)},
		})
		finished <- err
	}()
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("proxy request not admitted")
	}
	close(signal)
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy signal did not cancel request")
	}
}

func TestProxyClosedSignalRejectsRequestBeforeTransport(t *testing.T) {
	signal := make(chan struct{})
	close(signal)
	withMockHTTPClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Context().Err() == nil {
			t.Error("closed signal admitted uncancelled transport")
		}
		return nil, context.Cause(r.Context())
	}))
	var failures []ai.StreamError
	err := StreamProxyWithOptions(t.Context(), testProxyModel(), &ai.Context{}, func(_ context.Context, event ai.StreamEvent) {
		if failure, ok := event.(ai.StreamError); ok {
			failures = append(failures, failure)
		}
	}, ProxyStreamOptions{ProxyURL: "https://proxy.invalid", Signal: signal})
	if !errors.Is(err, context.Canceled) || len(failures) != 1 || failures[0].Msg.StopReason != ai.StopReasonAborted {
		t.Fatalf("err=%v failures=%+v", err, failures)
	}
}

func TestProxyToolNamespaceSurvivesReconstruction(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"toolcall_start","contentIndex":0,"id":"call","toolName":"lookup"}`,
		`data: {"type":"toolcall_end","contentIndex":0,"toolCall":{"id":"call","name":"lookup","arguments":{},"namespace":"dynamic_tools"}}`,
		`data: {"type":"done","reason":"toolUse"}`,
	}, "\n\n")
	var values []any
	require.NoError(t, streamProxyResponse(t.Context(), testProxyModel(), strings.NewReader(body), func(_ context.Context, event ai.StreamEvent) {
		switch e := event.(type) {
		case ai.ToolCallEnd:
			values = append(values, e.ToolCall, e.Partial.Content[0])
		case ai.Done:
			values = append(values, e.Message.Content[0])
		}
	}))
	require.Len(t, values, 3)
	for _, value := range values {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		require.Contains(t, string(data), `"namespace":"dynamic_tools"`)
	}
}
