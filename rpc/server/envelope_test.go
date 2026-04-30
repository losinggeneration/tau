package server

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/losinggeneration/tau/sdk"
)

func TestJSONEventEnvelopeShape(t *testing.T) {
	event := sdk.Event{
		Type:      "session_start",
		SessionID: "sess-123",
		Seq:       0,
		Timestamp: "2026-05-05T12:00:00Z",
		Payload: map[string]any{
			"id":    "sess-123",
			"cwd":   "/tmp",
			"model": "anthropic/claude-sonnet-4-6",
		},
	}

	envelope := jsonEventEnvelope(event)
	if _, ok := envelope["payload"]; ok {
		t.Fatalf("expected flattened event envelope, got payload wrapper: %#v", envelope)
	}
	if envelope["type"] != "session_start" {
		t.Fatalf("type = %v, want session_start", envelope["type"])
	}
	if envelope["session_id"] != "sess-123" {
		t.Fatalf("session_id = %v, want sess-123", envelope["session_id"])
	}
	if envelope["id"] != "sess-123" || envelope["cwd"] != "/tmp" {
		t.Fatalf("flattened payload fields missing: %#v", envelope)
	}
}

func TestEmitEventAssignsMonotonicSeqPerSession(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{writer: &buf}

	if err := s.emitEvent(sdk.Event{
		Type:      "session_start",
		SessionID: "sess-1",
		Timestamp: "2026-05-06T12:00:00Z",
		Payload:   map[string]any{"id": "sess-1"},
	}); err != nil {
		t.Fatalf("emit first event: %v", err)
	}

	if err := s.emitEvent(sdk.Event{
		Type:      "ui_confirm",
		SessionID: "sess-1",
		Payload:   map[string]any{"id": "ui-1", "title": "Confirm"},
	}); err != nil {
		t.Fatalf("emit second event: %v", err)
	}

	if err := s.emitEvent(sdk.Event{
		Type:      "session_start",
		SessionID: "sess-2",
		Timestamp: "2026-05-06T12:00:01Z",
		Payload:   map[string]any{"id": "sess-2"},
	}); err != nil {
		t.Fatalf("emit third event: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("emitted %d lines, want 3", len(lines))
	}

	var got []struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
		Seq       int64  `json:"seq"`
	}
	for i, line := range lines {
		var evt struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Seq       int64  `json:"seq"`
		}
		if err := json.Unmarshal(line, &evt); err != nil {
			t.Fatalf("unmarshal line %d: %v", i, err)
		}
		got = append(got, evt)
	}

	if got[0].Seq != 0 || got[1].Seq != 1 {
		t.Fatalf("first session seqs = [%d %d], want [0 1]", got[0].Seq, got[1].Seq)
	}
	if got[2].SessionID != "sess-2" || got[2].Seq != 0 {
		t.Fatalf("new session reset = {%q %d}, want {\"sess-2\" 0}", got[2].SessionID, got[2].Seq)
	}
}
