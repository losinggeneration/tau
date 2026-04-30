package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDebugMistralStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		data := `data: {"id":"resp_1","model":"mistral-large","choices":[{"index":0,"delta":{"content":["Hello ",{"type":"text","text":"world"},{"type":"thinking","thinking":[{"text":"thinking..."}]}]},"finish_reason":null}],"usage":{"prompt_tokens":10,"completion_tokens":0,"total_tokens":10}}
`
		_, _ = w.Write([]byte(data))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer server.Close()

	// Parse the test data
	data := `{"id":"resp_1","model":"mistral-large","choices":[{"index":0,"delta":{"content":["Hello ",{"type":"text","text":"world"},{"type":"thinking","thinking":[{"text":"thinking..."}]}]},"finish_reason":null}],"usage":{"prompt_tokens":10,"completion_tokens":0,"total_tokens":10}}
`
	var chunk mistralChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		t.Fatalf("Failed to parse chunk: %v", err)
	}
	t.Logf("Parsed chunk: %+v", chunk)

	for i, choice := range chunk.Choices {
		t.Logf("Choice %d: Delta.Content = %+v (type: %T)", i, choice.Delta.Content, choice.Delta.Content)
	}

	client := &mistralClient{apiKey: "test-key"}

	var events []StreamEvent
	err := client.Stream(context.Background(), &Model{ID: "test", BaseURL: server.URL, Provider: ProviderMistral}, &Context{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}, func(ctx context.Context, event StreamEvent) {
		events = append(events, event)
		t.Logf("Event: %T = %+v", event, event)
	})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}

	t.Logf("Total events: %d", len(events))
}

func TestDebugRefusal(t *testing.T) {
	sseData := `data: {"type":"response.created","response":{"id":"resp_abc123"}}
data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_001"}}
data: {"type":"response.content_part.added","part":{"type":"output_text","text":"I can help with"}}
data: {"type":"response.refusal.delta","delta":" that."}
data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_001","content":[{"type":"refusal","refusal":"that."}]}}
data: {"type":"response.completed","response":{"id":"resp_abc123","status":"completed"}}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o"}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var events []StreamEvent

	handler := func(ctx context.Context, event StreamEvent) {
		events = append(events, event)
	}

	err := ProcessResponsesStream(context.Background(), reader, output, handler, model, nil)
	if err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	t.Log("Events received:")
	for i, e := range events {
		switch v := e.(type) {
		case TextStart:
			t.Logf("  [%d] TextStart: ContentIndex=%d", i, v.ContentIndex)
		case TextDelta:
			t.Logf("  [%d] TextDelta: ContentIndex=%d, Delta=%q", i, v.ContentIndex, v.Delta)
		case TextEnd:
			t.Logf("  [%d] TextEnd: Content=%q", i, v.Content)
		case Done:
			t.Logf("  [%d] Done", i)
		}
	}
}
