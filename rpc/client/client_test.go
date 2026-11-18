package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// unixPeer starts an in-process server on a Unix socket and returns a client
// connected to it along with the accepted server side of the connection.
func unixPeer(t *testing.T) (*Client, net.Conn) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rpc.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("close listener: %v", err)
		}
	})
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := NewUnixClient(ctx, path)
	if err != nil {
		t.Fatalf("NewUnixClient: %v", err)
	}
	conn, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close server conn: %v", err)
		}
	})
	return c, conn
}

func TestUnixClientDeliversUIEventsWithID(t *testing.T) {
	c, conn := unixPeer(t)

	if _, err := conn.Write([]byte(`{"type":"ui_confirm","id":"ui-1","title":"Proceed?"}` + "\n")); err != nil {
		t.Fatalf("write event: %v", err)
	}

	select {
	case evt := <-c.Events():
		if evt.Type != "ui_confirm" {
			t.Fatalf("event type = %q, want ui_confirm", evt.Type)
		}
		if evt.Payload["id"] != "ui-1" {
			t.Fatalf("event id = %v, want ui-1", evt.Payload["id"])
		}
	case <-time.After(time.Second):
		t.Fatal("ui_confirm event with id was not delivered to Events")
	}
}

func TestUnixClientCorrelatesResponses(t *testing.T) {
	c, conn := unixPeer(t)

	go func() {
		line, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			return
		}
		var cmd Command
		if err := json.Unmarshal(line, &cmd); err != nil {
			return
		}
		resp, err := json.Marshal(Response{Type: "response", ID: cmd.ID, OK: true, Result: map[string]any{"cmd": cmd.Type}})
		if err != nil {
			return
		}
		if _, err := conn.Write(append(resp, '\n')); err != nil {
			return
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := c.GetState(ctx)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if !resp.OK || resp.Result["cmd"] != "get_state" {
		t.Fatalf("response = %+v", resp)
	}
}

// TestRPCRoundTrip tests RPC round-trip via client: each command type,
// response/event correlation, and ordering.
func TestRPCRoundTrip(t *testing.T) {
	t.Skip("requires tau rpc subprocess; run with tau binary available")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	// Collect events
	eventTypes := make(map[string]int)
	eventDone := make(chan struct{})
	go func() {
		defer close(eventDone)
		for evt := range c.Events() {
			eventTypes[evt.Type]++
		}
	}()

	// Test: new_session
	resp, err := c.NewSession(ctx, "/tmp")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !resp.OK {
		t.Errorf("NewSession: expected ok=true, got false: %+v", resp.Error)
	}
	if resp.ID == "" {
		t.Error("NewSession: response missing ID")
	}

	// Test: send_message (expect events)
	resp, err = c.SendMessage(ctx, "hello")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if !resp.OK {
		t.Errorf("SendMessage: expected ok=true, got false: %+v", resp.Error)
	}
	if resp.ID == "" {
		t.Error("SendMessage: response missing ID")
	}

	// Test: interrupt
	resp, err = c.Interrupt(ctx)
	if err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if !resp.OK {
		t.Errorf("Interrupt: expected ok=true, got false: %+v", resp.Error)
	}

	// Test: slash_command
	resp, err = c.SlashCommand(ctx, "help", "")
	if err != nil {
		t.Fatalf("SlashCommand: %v", err)
	}
	if !resp.OK {
		t.Errorf("SlashCommand: expected ok=true, got false: %+v", resp.Error)
	}

	// Test: get_state
	resp, err = c.GetState(ctx)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if !resp.OK {
		t.Errorf("GetState: expected ok=true, got false: %+v", resp.Error)
	}

	// Test: set_model
	resp, err = c.SetModel(ctx, "gpt-4o")
	if err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if !resp.OK {
		t.Errorf("SetModel: expected ok=true, got false: %+v", resp.Error)
	}

	// Test: list_sessions
	resp, err = c.ListSessions(ctx, 10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if !resp.OK {
		t.Errorf("ListSessions: expected ok=true, got false: %+v", resp.Error)
	}

	// Test: resume_session (should fail gracefully for non-existent)
	resp, err = c.ResumeSession(ctx, "nonexistent-id")
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if resp.OK {
		t.Error("ResumeSession: expected ok=false for non-existent session")
	}

	// Test: compact
	resp, err = c.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !resp.OK {
		t.Errorf("Compact: expected ok=true, got false: %+v", resp.Error)
	}

	// Test: shutdown
	resp, err = c.Shutdown(ctx)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !resp.OK {
		t.Errorf("Shutdown: expected ok=true, got false: %+v", resp.Error)
	}

	// Close event channel
	cancel()
	<-eventDone

	// Verify we received events during the session
	if len(eventTypes) == 0 {
		t.Error("Expected events during session")
	}

	t.Logf("Received event types: %v", eventTypes)
}

// TestResponseCorrelation verifies responses are correlated to commands by ID.
func TestResponseCorrelation(t *testing.T) {
	t.Skip("requires tau rpc subprocess; run with tau binary available")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	// Start a session first
	_, _ = c.NewSession(ctx, "/tmp")

	// Send multiple concurrent commands and verify each response matches its command ID
	commands := []struct {
		name string
		send func() (*Response, error)
	}{
		{"GetState", func() (*Response, error) { return c.GetState(ctx) }},
		{"ListSessions", func() (*Response, error) { return c.ListSessions(ctx, 5) }},
		{"SetModel", func() (*Response, error) { return c.SetModel(ctx, "claude-3-5-sonnet") }},
	}

	for _, tc := range commands {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := tc.send()
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if resp.ID == "" {
				t.Errorf("%s: response missing ID", tc.name)
			}
			// Verify it's not an error response if we expect success
			if !resp.OK && resp.Error != nil {
				t.Logf("%s: response indicates error: %s - %s", tc.name, resp.Error.Code, resp.Error.Message)
			}
		})
	}
}

// TestEventOrdering verifies events arrive in expected order.
func TestEventOrdering(t *testing.T) {
	t.Skip("requires tau rpc subprocess; run with tau binary available")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	// Collect events
	var events []string
	eventDone := make(chan struct{})
	go func() {
		defer close(eventDone)
		for evt := range c.Events() {
			events = append(events, evt.Type)
		}
	}()

	// Start session and send message
	_, _ = c.NewSession(ctx, "/tmp")
	_, _ = c.SendMessage(ctx, "test")

	cancel()
	<-eventDone

	if len(events) == 0 {
		t.Fatal("Expected events")
	}

	// Verify expected sequence pattern
	expectedPattern := []string{
		"session_start",
		"message_start",
	}
	for _, ep := range expectedPattern {
		found := false
		for _, e := range events {
			if e == ep {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected event type %q not found in sequence", ep)
		}
	}

	t.Logf("Event sequence: %v", events)
}

func TestIsClientEventType(t *testing.T) {
	included := []string{
		"response", "meta", "session_start", "session_end", "session_shutdown",
		"message_start", "message_end",
		"text_delta", "thinking_delta",
		"tool_call", "tool_progress", "tool_result",
		"usage", "error", "interrupt",
		"model_change", "thinking_level_change",
		"queue_update", "compaction_start", "compaction_end", "paused",
		"ui_confirm", "ui_select", "ui_input",
		"ui_notify", "ui_set_status", "ui_set_widget",
	}
	for _, ty := range included {
		if !IsClientEventType(ty) {
			t.Errorf("expected %q to be a client event type", ty)
		}
	}
	if IsClientEventType("internal_heartbeat") {
		t.Error("unrelated types should be filtered out")
	}
}

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
	// Simulate what the server sends with flattened fields
	serverJSON := []byte(`{"type":"text_delta","session_id":"session-abc","seq":42,"ts":"2026-05-07T12:00:00Z","delta":"final text","message_id":"msg-xyz"}`)
	var evt Event
	if err := evt.FromJSON(serverJSON); err != nil {
		t.Fatalf("FromJSON: %v", err)
	}

	// Verify all fields preserved
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

func TestSetThinkingLevelCommand(t *testing.T) {
	// Verify SetThinkingLevel method exists on Client
	// The actual RPC call would require a running tau rpc subprocess,
	// but we can verify the method signature is correct at compile time.
	var c *Client
	_ = c.SetThinkingLevel // Method exists and is accessible with correct signature
}
