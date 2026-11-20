package client

import "testing"

func TestEventFromJSONDelta(t *testing.T) {
	// Test that delta field is extracted correctly from flattened JSON
	data := []byte(`{"type":"text_delta","session_id":"s1","seq":5,"ts":"2026-05-01T10:00:00Z","delta":"Hello ","message_id":"msg-1"}`)
	var evt Event
	if err := evt.FromJSON(data); err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	if evt.Type != "text_delta" {
		t.Errorf("Type = %q, want %q", evt.Type, "text_delta")
	}
	if evt.Delta != "Hello " {
		t.Errorf("Delta = %q, want %q", evt.Delta, "Hello ")
	}
	if evt.SessionID != "s1" {
		t.Errorf("SessionID = %q, want %q", evt.SessionID, "s1")
	}
	if evt.Seq != 5 {
		t.Errorf("Seq = %d, want %d", evt.Seq, 5)
	}
	if evt.MessageID != "msg-1" {
		t.Errorf("MessageID = %q, want %q", evt.MessageID, "msg-1")
	}
}

func TestEventFromJSONThinkingDelta(t *testing.T) {
	data := []byte(`{"type":"thinking_delta","delta":"thinking...","level":"medium"}`)
	var evt Event
	if err := evt.FromJSON(data); err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	if evt.Type != "thinking_delta" {
		t.Errorf("Type = %q, want %q", evt.Type, "thinking_delta")
	}
	if evt.Delta != "thinking..." {
		t.Errorf("Delta = %q, want %q", evt.Delta, "thinking...")
	}
	if evt.Level != "medium" {
		t.Errorf("Level = %q, want %q", evt.Level, "medium")
	}
}

func TestEventFromJSONToolCall(t *testing.T) {
	data := []byte(`{"type":"tool_call","tool_call_id":"tc-123","name":"read","input":{"path":"/tmp/test.txt"}}`)
	var evt Event
	if err := evt.FromJSON(data); err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	if evt.Type != "tool_call" {
		t.Errorf("Type = %q, want %q", evt.Type, "tool_call")
	}
	if evt.ToolCallID != "tc-123" {
		t.Errorf("ToolCallID = %q, want %q", evt.ToolCallID, "tc-123")
	}
}

func TestEventFromJSONUsageTokens(t *testing.T) {
	data := []byte(`{"type":"usage","tokens":12345}`)
	var evt Event
	if err := evt.FromJSON(data); err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	if evt.Type != "usage" {
		t.Errorf("Type = %q, want %q", evt.Type, "usage")
	}
	if evt.Tokens != 12345 {
		t.Errorf("Tokens = %d, want %d", evt.Tokens, 12345)
	}
}

func TestEventFromJSONPreservesPayload(t *testing.T) {
	data := []byte(`{"type":"tool_result","tool_call_id":"tc-456","content":"file contents here","is_error":false}`)
	var evt Event
	if err := evt.FromJSON(data); err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	if evt.ToolCallID != "tc-456" {
		t.Errorf("ToolCallID = %q, want %q", evt.ToolCallID, "tc-456")
	}
	// Payload should contain any extra fields
	if evt.Payload == nil {
		t.Error("Payload should not be nil")
	}
	if content, ok := evt.Payload["content"].(string); !ok || content != "file contents here" {
		t.Errorf("Payload[content] = %v, want %q", evt.Payload["content"], "file contents here")
	}
}

func TestEventFromJSONRoundTrip(t *testing.T) {
	// Simulate what a worker sends with flattened fields
	serverJSON := []byte(`{"type":"text_delta","session_id":"session-abc","seq":42,"ts":"2026-05-07T12:00:00Z","delta":"final text","message_id":"msg-xyz"}`)
	var evt Event
	if err := evt.FromJSON(serverJSON); err != nil {
		t.Fatalf("FromJSON: %v", err)
	}

	if evt.Type != "text_delta" {
		t.Errorf("Type = %q", evt.Type)
	}
	if evt.SessionID != "session-abc" {
		t.Errorf("SessionID = %q", evt.SessionID)
	}
	if evt.Seq != 42 {
		t.Errorf("Seq = %d", evt.Seq)
	}
	if evt.TS != "2026-05-07T12:00:00Z" {
		t.Errorf("TS = %q", evt.TS)
	}
	if evt.Delta != "final text" {
		t.Errorf("Delta = %q", evt.Delta)
	}
	if evt.MessageID != "msg-xyz" {
		t.Errorf("MessageID = %q", evt.MessageID)
	}
}

func TestEventFromJSONMalformed(t *testing.T) {
	badJSON := []byte(`{invalid json}`)
	var evt Event
	if err := evt.FromJSON(badJSON); err == nil {
		t.Error("expected error for malformed JSON")
	}
}
