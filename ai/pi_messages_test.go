package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPiMessagesStreamsSerializedAssistantEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer key" || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("path=%s headers=%v", r.URL.Path, r.Header)
		}
		var body struct {
			Model   string         `json:"model"`
			Context map[string]any `json:"context"`
			Options map[string]any `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		messages, _ := body.Context["messages"].([]any)
		if body.Model != "balanced" || len(body.Context) != 1 || len(messages) != 4 || messages[0].(map[string]any)["role"] != "system" {
			t.Errorf("body = %+v", body)
		}
		assistant, _ := messages[2].(map[string]any)
		thinking, _ := assistant["content"].([]any)[0].(map[string]any)
		if thinking["thinkingSignature"] != "sig" || thinking["thoughtSignature"] != nil {
			t.Errorf("thinking block = %v", thinking)
		}
		wantOptions := map[string]any{"maxTokens": float64(100), "reasoning": "high", "sessionId": "s1", "cacheRetention": "long"}
		if !reflect.DeepEqual(body.Options, wantOptions) {
			t.Errorf("options = %v", body.Options)
		}
		events := []string{
			`{"type":"start"}`,
			`{"type":"thinking_start","contentIndex":0}`,
			`{"type":"thinking_delta","contentIndex":0,"delta":"hm"}`,
			`{"type":"thinking_end","contentIndex":0,"content":"hmm","contentSignature":"ts"}`,
			`{"type":"text_start","contentIndex":1}`,
			`{"type":"text_delta","contentIndex":1,"delta":"hi"}`,
			`{"type":"text_end","contentIndex":1,"content":"hi!","contentSignature":"xs"}`,
			`{"type":"toolcall_start","contentIndex":2,"id":"c1","toolName":"read"}`,
			`{"type":"toolcall_delta","contentIndex":2,"delta":"{\"path\":"}`,
			`{"type":"toolcall_end","contentIndex":2,"toolCall":{"type":"toolCall","id":"c1","name":"read","arguments":{"path":"a"}}}`,
			`{"type":"done","reason":"toolUse","responseId":"r1","usage":{"input":5,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":8,"cost":{"input":0.25,"output":0.25,"cacheRead":0,"cacheWrite":0,"total":0.5}},"rewrite":{"policyId":"p","policyVersion":2,"changed":true,"tokenCountChange":-4,"messageCountChange":0,"systemPromptChanged":false}}`,
		}
		for _, event := range events {
			if _, err := io.WriteString(w, "data: "+event+"\r\n\r\n"); err != nil {
				t.Error(err)
			}
		}
	}))
	defer server.Close()

	model := &Model{ID: "balanced", API: APIPiMessages, Provider: ProviderRadius, BaseURL: server.URL + "/v1/", Reasoning: true, ContextWindow: 100000, MaxTokens: 1000}
	prompt := &Context{SystemPrompt: "sys", Messages: []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Provider: ProviderRadius, API: APIPiMessages, Model: "balanced", Content: []ContentBlock{{Type: ContentBlockThinking, Thinking: "t", ThoughtSignature: "sig"}, {Type: ContentBlockText, Text: "ok"}}},
		{Role: "user", Content: "again"},
	}}
	var types []string
	var done Done
	client, err := (&PiMessagesProvider{}).NewClient(model)
	if err != nil {
		t.Fatal(err)
	}
	err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
		types = append(types, reflect.TypeOf(event).Name())
		if d, ok := event.(Done); ok {
			done = d
		}
	}, WithAPIKey("key"), WithMaxTokens(100), WithReasoning(ReasoningHigh), WithSessionID("s1"), WithCacheRetention("long"))
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{"Start", "ThinkingStart", "ThinkingDelta", "ThinkingEnd", "TextStart", "TextDelta", "TextEnd", "ToolCallStart", "ToolCallDelta", "ToolCallEnd", "Done"}
	if !reflect.DeepEqual(types, wantTypes) {
		t.Fatalf("events = %v", types)
	}
	msg := done.Message
	if done.Reason != StopReasonToolUse || msg.ResponseID != "r1" || msg.Usage.TotalTokens != 8 || msg.Usage.Cost.Total != 0.5 {
		t.Fatalf("done = %+v", done)
	}
	if msg.Content[0].Thinking != "hmm" || msg.Content[0].ThoughtSignature != "ts" || msg.Content[1].Text != "hi!" || msg.Content[1].TextSignature != "xs" {
		t.Fatalf("content = %+v", msg.Content)
	}
	if call := msg.Content[2]; call.Type != ContentBlockToolCall || call.ID != "c1" || call.Name != "read" || string(call.Arguments) != `{"path":"a"}` {
		t.Fatalf("tool call = %+v", call)
	}
	if len(msg.Diagnostics) != 1 || msg.Diagnostics[0].Type != "pi_messages_rewrite" || msg.Diagnostics[0].Details["tokenCountChange"] != "-4" {
		t.Fatalf("diagnostics = %+v", msg.Diagnostics)
	}
}

func TestPiMessagesReportsHTTPErrorsAndMissingTerminalEvent(t *testing.T) {
	tests := []struct {
		status    int
		body      string
		want      string
		diagnosed bool
	}{
		{http.StatusForbidden, `{"error":{"message":"model disabled","code":"model_disabled"}}`, "403 Forbidden: model disabled (model_disabled)", true},
		{http.StatusOK, "data: {\"type\":\"start\"}\n\n", "stream ended without a terminal event", false},
		{http.StatusOK, `data: {"type":"error","reason":"error","errorMessage":"upstream failed","usage":{"input":1,"output":0,"totalTokens":1,"cost":{"total":0}}}` + "\n\n", "upstream failed", false},
	}
	for _, tt := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tt.status)
			if _, err := io.WriteString(w, tt.body); err != nil {
				t.Error(err)
			}
		}))
		model := &Model{ID: "m", API: APIPiMessages, Provider: ProviderRadius, BaseURL: server.URL, ContextWindow: 1000}
		message, err := (&piMessagesClient{}).Complete(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "x"}}}, WithAPIKey("key"))
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if message.StopReason != StopReasonError || !strings.Contains(message.ErrorMessage, tt.want) {
			t.Fatalf("message = %+v, want %q", message, tt.want)
		}
		if tt.diagnosed != (len(message.Diagnostics) == 1 && message.Diagnostics[0].Type == "pi_messages_response_failure" && message.Diagnostics[0].Details["status"] == "403") {
			t.Fatalf("diagnostics = %+v", message.Diagnostics)
		}
	}
	if err := (&piMessagesClient{}).Stream(t.Context(), &Model{API: APIPiMessages, Provider: ProviderRadius, ContextWindow: 1000}, &Context{}, func(context.Context, StreamEvent) {}); err == nil {
		t.Fatal("missing API key accepted")
	}
}

func TestPiMessagesHeaderTimeout(t *testing.T) {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	model := &Model{ID: "m", API: APIPiMessages, Provider: ProviderRadius, BaseURL: "https://test.invalid", ContextWindow: 1000}
	message, err := (&piMessagesClient{}).Complete(ctx, model, &Context{Messages: []Message{{Role: "user", Content: "x"}}}, WithAPIKey("key"), WithTimeout(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil || message.StopReason != StopReasonError || !strings.Contains(message.ErrorMessage, "response headers timed out") {
		t.Fatalf("header timeout ignored: %+v", message)
	}
}

func TestPiMessagesToolNamespaceRoundTrip(t *testing.T) {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data, err := json.Marshal(body["context"])
		require.NoError(t, err)
		require.Contains(t, string(data), `"namespace":"dynamic_tools"`)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"toolcall_start","contentIndex":0,"id":"call","toolName":"lookup"}`,
			`data: {"type":"toolcall_end","contentIndex":0,"toolCall":{"id":"call","name":"lookup","arguments":{},"namespace":"dynamic_tools"}}`,
			`data: {"type":"done","reason":"toolUse"}`,
		}, "\n\n")))}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	var prompt Context
	require.NoError(t, json.Unmarshal([]byte(`{"messages":[{"role":"assistant","content":[{"type":"toolCall","id":"old","name":"lookup","arguments":{},"namespace":"dynamic_tools"}]}]}`), &prompt))
	var values []any
	require.NoError(t, (&piMessagesClient{}).Stream(t.Context(), &Model{ID: "m", Provider: ProviderRadius, API: APIPiMessages, BaseURL: "https://pi.invalid"}, &prompt, func(_ context.Context, event StreamEvent) {
		switch e := event.(type) {
		case ToolCallEnd:
			values = append(values, e.ToolCall, e.Partial.Content[0])
		case Done:
			values = append(values, e.Message.Content[0])
		}
	}, WithAPIKey("key")))
	require.Len(t, values, 3)
	for _, value := range values {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		require.Contains(t, string(data), `"namespace":"dynamic_tools"`)
	}
}

func TestPiMessagesRejectsInvalidContentIndexes(t *testing.T) {
	for _, body := range []string{
		`{"type":"text_start","contentIndex":-1}`,
		`{"type":"text_start","contentIndex":1000000}`,
		`{"type":"text_delta","contentIndex":0,"delta":"x"}`,
		`{"type":"text_start","contentIndex":0}` + "\n\ndata: " + `{"type":"toolcall_delta","contentIndex":0,"delta":"x"}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, err := io.WriteString(w, "data: "+body+"\n\n"); err != nil {
				t.Error(err)
			}
		}))
		model := &Model{ID: "m", API: APIPiMessages, Provider: ProviderRadius, BaseURL: server.URL, ContextWindow: 1000}
		message, err := (&piMessagesClient{}).Complete(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "x"}}}, WithAPIKey("key"))
		server.Close()
		require.NoError(t, err)
		require.Equal(t, StopReasonError, message.StopReason, body)
		require.LessOrEqual(t, len(message.Content), 1, body)
	}
}
