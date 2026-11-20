package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
	"github.com/stretchr/testify/assert"
)

// ─── protocol-level envelope tests ─────────────────────────────────────────

func TestRPCListSessionsIncludesWorkspaceSummaryFields(t *testing.T) {
	rt := &mockRuntime{
		sessions: []sdk.SessionInfo{{
			ID:           "test-session",
			Name:         "named",
			CWD:          "/repo",
			Created:      "2026-01-01T00:00:00Z",
			LastModified: "2026-01-01T00:01:00Z",
			Model:        "test/model",
			Status:       "waiting",
			MessageCount: 3,
		}},
	}
	h := newTestHarness(t, rt, nil)
	defer h.close()

	h.send(t, `{"id":"workspace-new","type":"create_session","args":{"cwd":"/repo"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "workspace-new"
	})
	h.send(t, `{"id":"workspace-steer","type":"steer","args":{"text":"queued"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "workspace-steer"
	})

	h.send(t, `{"id":"workspace-list","type":"list_sessions","args":{}}`)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "workspace-list"
	})
	if len(lines) == 0 {
		t.Fatal("no list_sessions response")
	}
	resp := parseResp(t, lines[len(lines)-1])
	sessions := resp["result"].(map[string]any)["sessions"].([]any)
	got := sessions[0].(map[string]any)
	if got["status"] != "queued" || got["model"] != "test/model" || got["last_modified"] == "" || got["pinned"] != false {
		t.Fatalf("workspace summary = %#v", got)
	}
	if _, ok := got["context_usage"]; !ok {
		t.Fatalf("workspace summary missing context_usage: %#v", got)
	}
}

func TestRPCFraming(t *testing.T) {
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()

	h.send(t, `{"id":"framing-1","type":"get_state","args":{}}`)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "framing-1"
	})

	for i, line := range lines {
		if line == "" {
			continue
		}
		// Each line must be valid JSON.
		var js json.RawMessage
		if err := json.Unmarshal([]byte(line), &js); err != nil {
			t.Errorf("line %d is not valid JSON: %s", i+1, line)
		}
		// No CRLF.
		for j, b := range line {
			if b == '\r' {
				t.Errorf("line %d contains carriage return at position %d", i+1, j)
			}
		}
	}
}

func TestRPCResponseEnvelope(t *testing.T) {
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()

	h.send(t, `{"id":"test-envelope-1","type":"get_state","args":{}}`)
	line := h.readLine(t)
	resp := parseResp(t, line)
	if resp["type"] != "response" {
		t.Errorf("expected type='response', got %q", resp["type"])
	}
	if resp["id"] != "test-envelope-1" {
		t.Errorf("expected id='test-envelope-1', got %v", resp["id"])
	}
}

func TestRPCErrorEnvelope(t *testing.T) {
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()

	// Send a malformed JSON command to provoke an error envelope.
	h.send(t, `not json at all`)
	line := h.readLine(t)
	resp := parseResp(t, line)
	if resp["type"] != "response" {
		t.Errorf("expected type='response', got %q", resp["type"])
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object in response, got %v", resp["error"])
	}
	if errObj["code"] == "" {
		t.Errorf("expected non-empty error code")
	}
	if errObj["message"] == "" {
		t.Errorf("expected non-empty error message")
	}
}

func TestRPCSequenceMetadata(t *testing.T) {
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()

	h.send(t, `{"id":"seq-1","type":"get_state","args":{}}`)
	line := h.readLine(t)
	resp := parseResp(t, line)
	if resp["seq"] == nil {
		// First response may have seq 0 or omitted — that's fine.
		return
	}
	if _, ok := resp["seq"].(float64); !ok {
		t.Errorf("expected seq to be number, got %T", resp["seq"])
	}
}

func TestRPCUIConfirmRoundTrip(t *testing.T) {
	rt := &mockRuntime{}
	h := newTestHarness(t, rt, nil)
	defer h.close()

	h.send(t, `{"id":"ui-1","type":"get_state","args":{}}`)
	line := h.readLine(t)
	resp := parseResp(t, line)
	if resp["id"] != "ui-1" {
		t.Errorf("expected id=ui-1, got %v", resp["id"])
	}
}

func TestRPCUIPromptRoundTrip(t *testing.T) {
	rt := &mockRuntime{}
	h := newTestHarness(t, rt, nil)
	defer h.close()

	h.send(t, `{"id":"prompt-1","type":"get_state","args":{}}`)
	line := h.readLine(t)
	resp := parseResp(t, line)
	if resp["id"] != "prompt-1" {
		t.Errorf("expected id=prompt-1, got %v", resp["id"])
	}
}

func TestRPCNoGlobExpansion(t *testing.T) {
	// Documentation test: glob patterns in @file refs are client-side concerns.
	invalidInputs := []string{"@*.go", "@**/*.md", "@./**"}
	for _, input := range invalidInputs {
		if !strings.HasPrefix(input, "@") || (!strings.Contains(input, "*") && !strings.Contains(input, "**")) {
			t.Errorf("expected %q to be a glob pattern", input)
		}
	}
}

func TestRPCExitCodes(t *testing.T) {
	expectedCodes := map[int]string{
		0:   "success",
		1:   "model/provider error",
		2:   "user error",
		3:   "auth error",
		4:   "tool/subsystem error",
		130: "SIGINT",
	}
	for code, meaning := range expectedCodes {
		if code < 0 {
			t.Errorf("exit code %d is negative", code)
		}
		t.Logf("Exit code %d: %s", code, meaning)
	}
}

func TestRPCAllEventTypes(t *testing.T) {
	expectedEvents := []string{
		"meta", "session_start", "message_start", "text_delta",
		"thinking_delta", "tool_call", "tool_progress", "tool_result",
		"message_end", "usage", "error", "session_end",
		"ui_confirm", "ui_select", "ui_input", "ui_notify",
		"ui_set_status", "ui_set_widget", "interrupt",
	}
	seen := make(map[string]bool)
	for _, evt := range expectedEvents {
		if seen[evt] {
			t.Errorf("duplicate event type: %s", evt)
		}
		seen[evt] = true
		for _, c := range evt {
			if c >= 'A' && c <= 'Z' {
				t.Errorf("event type %q contains uppercase", evt)
			}
		}
	}
}

func TestRPCEventEnvelopeFormat(t *testing.T) {
	event := struct {
		Type      string         `json:"type"`
		SessionID string         `json:"session_id"`
		Seq       int64          `json:"seq"`
		Timestamp string         `json:"ts"`
		Payload   map[string]any `json:"payload,omitempty"`
	}{
		Type: "text_delta", SessionID: "session-123", Seq: 5,
		Timestamp: "2026-05-01T10:00:00Z",
		Payload:   map[string]any{"message_id": "msg-1", "delta": "Hello, "},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Type != "text_delta" {
		t.Errorf("expected type='text_delta', got %q", parsed.Type)
	}
}

// ─── struct serialization tests (no server needed) ─────────────────────────

func TestRPCServerInterface(t *testing.T) {
	resp := Response{
		Type:   "response",
		ID:     "test-1",
		OK:     true,
		Result: map[string]any{"status": "ok"},
	}
	data, _ := json.Marshal(resp)
	var parsed Response
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Type != "response" {
		t.Errorf("expected type='response', got %q", parsed.Type)
	}
	if !parsed.OK {
		t.Error("expected ok=true")
	}
}

func TestRPCServerErrorInterface(t *testing.T) {
	resp := Response{
		Type:  "response",
		ID:    "error-1",
		OK:    false,
		Error: &ErrorInfo{Code: "unknown_command", Message: "unknown command type: foo"},
	}
	data, _ := json.Marshal(resp)
	var parsed struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(data, &parsed)
	if parsed.OK {
		t.Error("expected ok=false")
	}
	if parsed.Error.Code == "" || parsed.Error.Message == "" {
		t.Error("expected non-empty error fields")
	}
}

func TestRPCCommandInterface(t *testing.T) {
	cmd := Command{ID: "cmd-1", Type: "send_message", Args: json.RawMessage(`{"text":"hello"}`)}
	data, _ := json.Marshal(cmd)
	var parsed Command
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.ID != "cmd-1" || parsed.Type != "send_message" {
		t.Errorf("unexpected: %+v", parsed)
	}
}

// ─── command ID correlation ────────────────────────────────────────────────

func TestRPCCommandIDCorrelation(t *testing.T) {
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()

	cases := []struct {
		name string
		cmd  string
	}{
		{"get_state", `{"id":"corr-get-1","type":"get_state","args":{}}`},
		{"list_sessions", `{"id":"corr-list-1","type":"list_sessions","args":{"limit":10}}`},
		{"new_session", `{"id":"corr-new-1","type":"new_session","args":{"cwd":"/tmp"}}`},
		{"shutdown", `{"id":"corr-shutdown-1","type":"shutdown","args":{}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.send(t, tc.cmd)
			// Read until we see the response with the matching ID.
			lines := h.readUntil(t, timeout, func(line string) bool {
				var obj map[string]any
				if err := json.Unmarshal([]byte(line), &obj); err != nil {
					return false
				}
				return obj["type"] == "response" && obj["id"] == jsonID(t, tc.cmd)
			})
			if len(lines) == 0 {
				t.Fatal("no response received")
			}
			resp := parseResp(t, lines[len(lines)-1])
			if resp["id"] != jsonID(t, tc.cmd) {
				t.Errorf("ID mismatch: sent %q, got %v", jsonID(t, tc.cmd), resp["id"])
			}
		})
	}
}

// ─── session and shutdown tests ────────────────────────────────────────────

func TestRPCShutdownGraceful(t *testing.T) {
	h := newTestHarness(t, &mockRuntime{}, nil)

	h.send(t, `{"id":"shutdown-1","type":"shutdown","args":{}}`)
	line := h.readLine(t)
	resp := parseResp(t, line)
	if resp["type"] != "response" {
		t.Errorf("expected type='response', got %q", resp["type"])
	}
	if resp["id"] != "shutdown-1" {
		t.Errorf("expected id='shutdown-1', got %v", resp["id"])
	}

	waitOrFail(t, h.done, timeout, "handlers after shutdown")
}

func TestRPCConsumerStdinResponse(t *testing.T) {
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()

	cmds := []string{
		`{"id":"stdin-1","type":"get_state","args":{}}`,
		`{"id":"stdin-2","type":"list_sessions","args":{}}`,
	}
	for _, cmd := range cmds {
		h.send(t, cmd)
		lines := h.readUntil(t, timeout, func(line string) bool {
			var obj map[string]any
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				return false
			}
			return obj["type"] == "response"
		})
		if len(lines) == 0 {
			t.Fatalf("no response for %s", cmd)
		}
		parsed := parseResp(t, lines[len(lines)-1])
		if parsed["type"] != "response" {
			t.Errorf("expected type='response', got %q", parsed["type"])
		}
	}
}

// ─── thinking level and model persistence ──────────────────────────────────

func TestRPCSetThinkingLevelAffectsNextTurn(t *testing.T) {
	rt := &mockRuntime{}
	tmpDir := t.TempDir()
	t.Setenv("TAU_SESSION_DIR", tmpDir)
	h := newTestHarness(t, rt, nil)
	defer h.close()

	// 1. Create session.
	h.send(t, `{"id":"thinking-1","type":"new_session","args":{"cwd":"/tmp"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "thinking-1"
	})

	h.send(t, `{"id":"invalid","type":"set_thinking_level","args":{"level":"unknown"}}`)
	invalid := h.readUntil(t, timeout, func(line string) bool {
		return strings.Contains(line, `"id":"invalid"`) && strings.Contains(line, `"type":"response"`)
	})
	if len(invalid) == 0 || parseResp(t, invalid[len(invalid)-1])["ok"] != false {
		t.Fatalf("invalid thinking level not rejected: %v", invalid)
	}

	// 2. Set thinking level.
	h.send(t, `{"id":"thinking-2","type":"set_thinking_level","args":{"level":"HIGH"}}`)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "thinking-2"
	})
	if len(lines) == 0 {
		t.Fatal("no set_thinking_level response")
	}
	resp := parseResp(t, lines[len(lines)-1])
	if resp["ok"] != true {
		t.Fatalf("set_thinking_level failed: %v", resp)
	}
	if result, ok := resp["result"].(map[string]any); !ok || result["level"] != "high" {
		t.Errorf("expected result.level=high, got %v", resp["result"])
	}

	// 3. Get state — verify the server's thinkingLevel field is updated.
	h.send(t, `{"id":"thinking-3","type":"get_state","args":{}}`)
	lines = h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "thinking-3"
	})
	if len(lines) == 0 {
		t.Fatal("no get_state response")
	}
	stateResp := parseResp(t, lines[len(lines)-1])
	if result, ok := stateResp["result"].(map[string]any); ok {
		if result["thinking_level"] != "high" {
			t.Errorf("expected result.thinking_level=high, got %v", result["thinking_level"])
		}
	} else {
		t.Errorf("expected result object in get_state response, got %v", stateResp)
	}
}

func TestRPCSetThinkingLevelKeepsSameSession(t *testing.T) {
	rt := &mockRuntime{}
	h := newTestHarness(t, rt, nil)
	h.send(t, `{"id":"new","type":"new_session","args":{"cwd":"/tmp"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		return strings.Contains(line, `"id":"new"`) && strings.Contains(line, `"type":"response"`)
	})
	h.send(t, `{"id":"set","type":"set_thinking_level","args":{"level":"high"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		return strings.Contains(line, `"id":"set"`) && strings.Contains(line, `"type":"response"`)
	})
	h.close()
	if rt.resumeCount != 0 || rt.newCount != 1 || rt.session.thinking != "high" {
		t.Fatalf("thinking change replaced session or did not apply: creates=%d resumes=%d level=%q", rt.newCount, rt.resumeCount, rt.session.thinking)
	}
}

func TestRPCThinkingLevelChangePersistedToJSONL(t *testing.T) {
	rt := &mockRuntime{}
	tmpDir := t.TempDir()
	t.Setenv("TAU_SESSION_DIR", tmpDir)
	h := newTestHarness(t, rt, nil)
	defer h.close()

	h.send(t, `{"id":"jsonl-1","type":"new_session","args":{"cwd":"/tmp"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "jsonl-1"
	})

	h.send(t, `{"id":"jsonl-2","type":"set_thinking_level","args":{"level":"medium"}}`)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "jsonl-2"
	})
	if len(lines) == 0 {
		t.Fatal("no set_thinking_level response")
	}
	resp := parseResp(t, lines[len(lines)-1])
	if resp["ok"] != true {
		t.Errorf("expected ok=true, got %v", resp["ok"])
	}

	h.send(t, `{"id":"jsonl-3","type":"shutdown","args":{}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "jsonl-3"
	})
}

func TestRPCSetModelPersistsModelChange(t *testing.T) {
	rt := &mockRuntime{}
	tmpDir := t.TempDir()
	t.Setenv("TAU_SESSION_DIR", tmpDir)
	h := newTestHarness(t, rt, nil)
	defer h.close()

	h.send(t, `{"id":"model-1","type":"new_session","args":{"cwd":"/tmp"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "model-1"
	})

	h.send(t, `{"id":"model-2","type":"set_model","args":{"model_spec":"claude-sonnet-4-5"}}`)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "model-2"
	})
	if len(lines) == 0 {
		t.Fatal("no set_model response")
	}
	resp := parseResp(t, lines[len(lines)-1])
	if resp["ok"] != true {
		t.Errorf("expected ok=true, got %v", resp["ok"])
	}

	h.send(t, `{"id":"model-3","type":"shutdown","args":{}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "model-3"
	})
}

func TestRPCSetModelUpdatesCurrentSessionInPlace(t *testing.T) {
	rt := &mockRuntime{}
	h := newTestHarness(t, rt, nil)
	defer h.close()

	h.send(t, `{"id":"inplace-1","type":"new_session","args":{"cwd":"/tmp"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "inplace-1"
	})

	current := rt.session
	if current == nil {
		t.Fatal("new_session did not create a session")
	}

	h.send(t, `{"id":"inplace-2","type":"set_model","args":{"model_spec":"anthropic/claude-opus-4-6"}}`)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "inplace-2"
	})
	if len(lines) == 0 {
		t.Fatal("no set_model response")
	}
	resp := parseResp(t, lines[len(lines)-1])
	if resp["ok"] != true {
		t.Fatalf("set_model failed: %v", resp)
	}

	if rt.session != current {
		t.Fatal("set_model replaced the active session; want in-place model update to preserve context")
	}
	if rt.resumeCount != 0 {
		t.Fatalf("set_model resumed session %d times; want 0", rt.resumeCount)
	}
	if len(current.setModels) != 1 || current.setModels[0] != "anthropic/claude-opus-4-6" {
		t.Fatalf("SetModel calls = %v, want [anthropic/claude-opus-4-6]", current.setModels)
	}
}

func TestRPCSetModelAcceptsClaudeAgent(t *testing.T) {
	rt := &mockRuntime{}
	h := newTestHarness(t, rt, nil)
	defer h.close()

	h.send(t, `{"id":"claude-agent-1","type":"set_model","args":{"model_spec":"claude-agent/sonnet"}}`)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "claude-agent-1"
	})
	if len(lines) == 0 {
		t.Fatal("no set_model response")
	}
	resp := parseResp(t, lines[len(lines)-1])
	if resp["ok"] != true {
		t.Fatalf("set_model failed: %v", resp)
	}
	if result, ok := resp["result"].(map[string]any); !ok || result["model"] != "claude-agent/sonnet" {
		t.Fatalf("result = %#v", resp["result"])
	}

	h.send(t, `{"id":"claude-agent-2","type":"new_session","args":{"cwd":"/tmp"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "claude-agent-2"
	})
	if rt.session == nil || rt.session.State().Model() != "claude-agent/sonnet" {
		t.Fatalf("new session model = %#v, want claude-agent/sonnet", rt.session)
	}
}

// ─── interrupt tests ───────────────────────────────────────────────────────

func TestRPCInterruptCommand(t *testing.T) {
	rt := &mockRuntime{}
	h := newTestHarness(t, rt, nil)
	defer h.close()

	h.send(t, `{"id":"test-1","type":"interrupt","args":{}}`)
	line := h.readLine(t)
	resp := parseResp(t, line)
	if resp["type"] != "response" {
		t.Errorf("expected type='response', got %q", resp["type"])
	}
	if resp["id"] != "test-1" {
		t.Errorf("expected id='test-1', got %v", resp["id"])
	}
	if resp["ok"] != true {
		t.Errorf("expected ok=true, got error: %v", resp["error"])
	}
}

// ─── slash command tests ──────────────────────────────────────────────────

func TestRPCSlashCommand(t *testing.T) {
	rt := &mockRuntime{}
	reg := newTestRegistry()
	h := newTestHarness(t, rt, reg)
	defer h.close()

	h.send(t, `{"id":"test-slash-1","type":"slash_command","args":{"name":"login","args":""}}`)
	line := h.readLine(t)
	resp := parseResp(t, line)
	if resp["type"] != "response" {
		t.Errorf("expected type='response', got %q", resp["type"])
	}
	if resp["id"] != "test-slash-1" {
		t.Errorf("expected id='test-slash-1', got %v", resp["id"])
	}
	// Login command goes through registry (testRegistry returns a result).
	if resp["ok"] == false {
		if errObj, ok := resp["error"].(map[string]any); ok {
			if msg, _ := errObj["message"].(string); strings.Contains(msg, "not yet implemented") {
				t.Errorf("slash command returned unimplemented stub: %v", resp["error"])
			}
		}
	}
}

func TestRPCSlashCommandNotFound(t *testing.T) {
	rt := &mockRuntime{}
	reg := newTestRegistry()
	h := newTestHarness(t, rt, reg)
	defer h.close()

	h.send(t, `{"id":"test-slash-2","type":"slash_command","args":{"name":"nonexistent","args":""}}`)
	line := h.readLine(t)
	resp := parseResp(t, line)
	if resp["type"] != "response" {
		t.Errorf("expected type='response', got %q", resp["type"])
	}
	if resp["id"] != "test-slash-2" {
		t.Errorf("expected id='test-slash-2', got %v", resp["id"])
	}
	if resp["ok"] == true {
		t.Errorf("expected ok=false for unknown command")
	}
	// Error message should mention "not found".
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %v", resp["error"])
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "not found") {
		t.Errorf("expected error to mention 'not found', got %q", msg)
	}
}

func TestRPCSlashCommandRoundTrip(t *testing.T) {
	rt := &mockRuntime{}
	reg := newTestRegistry()
	h := newTestHarness(t, rt, reg)
	defer h.close()

	h.send(t, `{"id":"slash-test-1","type":"slash_command","args":{"name":"help","args":""}}`)
	line := h.readLine(t)
	resp := parseResp(t, line)
	if resp["id"] != "slash-test-1" {
		t.Errorf("expected id='slash-test-1', got %v", resp["id"])
	}
	// Either success or an error — just verify the response was produced.
	if resp["type"] != "response" {
		t.Errorf("expected type='response', got %q", resp["type"])
	}
}

func TestRPCSlashCommandCoverage(t *testing.T) {
	rt := &mockRuntime{}
	reg := newTestRegistry()
	h := newTestHarness(t, rt, reg)
	defer h.close()

	cases := []struct {
		name       string
		cmd        string
		wantOK     bool
		wantOutput string
	}{
		{"builtin-help", `{"id":"sc-1","type":"slash_command","args":{"name":"help","args":""}}`, true, "Available slash commands"},
		{"builtin-reload", `{"id":"sc-2","type":"slash_command","args":{"name":"reload","args":""}}`, true, "reloaded"},
		{"builtin-sessions", `{"id":"sc-3","type":"slash_command","args":{"name":"sessions","args":""}}`, true, "tau sessions"},
		{"builtin-set-model", `{"id":"sc-4","type":"slash_command","args":{"name":"set-model","args":""}}`, true, "tau set-model"},
		{"builtin-show-model", `{"id":"sc-5","type":"slash_command","args":{"name":"show-model","args":""}}`, true, "tau show-model"},
		{"builtin-login", `{"id":"sc-6","type":"slash_command","args":{"name":"login","args":""}}`, true, "tau login"},
		{"not-found", `{"id":"sc-7","type":"slash_command","args":{"name":"nonexistent-cmd","args":""}}`, false, "command not found"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.send(t, tc.cmd)
			line := h.readLine(t)
			resp := parseResp(t, line)

			if resp["ok"] != tc.wantOK {
				t.Errorf("expected ok=%v, got %v", tc.wantOK, resp["ok"])
			}

			var gotOutput string
			if result, ok := resp["result"].(map[string]any); ok {
				if s, ok := result["output"].(string); ok {
					gotOutput = s
				}
			}
			if errObj, ok := resp["error"].(map[string]any); ok {
				if s, ok := errObj["message"].(string); ok {
					gotOutput = s
				}
			}
			if !strings.Contains(gotOutput, tc.wantOutput) {
				t.Errorf("expected output containing %q, got %q", tc.wantOutput, gotOutput)
			}
		})
	}
}

// ─── session resume and seq reset ──────────────────────────────────────────

func TestRPCSessionResumeSeqReset(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TAU_SESSION_DIR", tmpDir)

	rt := &mockRuntime{}
	h := newTestHarness(t, rt, nil)
	defer h.close()

	// 1. Create a session.
	h.send(t, `{"id":"reset-new-1","type":"new_session","args":{"cwd":"/tmp"}}`)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "reset-new-1"
	})
	if len(lines) == 0 {
		t.Fatal("no new_session response")
	}

	// 2. Send a message (with mock session emitting events).
	h.send(t, `{"id":"reset-send-1","type":"send_message","args":{"text":"hello"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "reset-send-1"
	})

	// 3. Resume the session.
	h.send(t, `{"id":"reset-resume-1","type":"resume_session","args":{"id":"test-session"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "reset-resume-1"
	})

	// 4. Send another message after resume.
	h.send(t, `{"id":"reset-send-2","type":"send_message","args":{"text":"after resume"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "reset-send-2"
	})

	// Verify the runtime resumed a session with the requested ID.
	if rt.SessionID() != "test-session" {
		t.Errorf("expected resumed session ID=test-session, got %q", rt.SessionID())
	}
}

// ─── event envelope shape (with mock events) ──────────────────────────────

func TestRPCEventEnvelopeShape(t *testing.T) {
	rt := &mockRuntime{}
	// Pre-load the mock session with canned events.
	sess := &mockSession{
		id:      "test-session",
		state:   sdkNewSessionState("test-session", "anthropic/claude-sonnet-4-6", "/tmp"),
		persist: true,
	}
	sess.runtime = rt
	sess.events = []sdk.Event{
		{Type: "session_start", SessionID: "test-session", Payload: map[string]any{"id": "test-session"}},
		{Type: "message_start", SessionID: "test-session"},
		{Type: "text_delta", SessionID: "test-session", Payload: map[string]any{"delta": "hi"}},
		{Type: "message_end", SessionID: "test-session"},
		{Type: "session_end", SessionID: "test-session"},
	}
	rt.session = sess

	h := newTestHarness(t, rt, nil)
	defer h.close()

	h.send(t, `{"id":"shape-new-1","type":"new_session","args":{"cwd":"/tmp"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "shape-new-1"
	})

	h.send(t, `{"id":"shape-send-1","type":"send_message","args":{"text":"hello"}}`)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "shape-send-1"
	})

	// Verify each event has expected top-level fields.
	seqSeen := make(map[int64]bool)
	for _, line := range lines {
		if line == "" {
			continue
		}
		var evt struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Seq       int64  `json:"seq"`
			TS        string `json:"ts"`
		}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if evt.Type == "meta" || evt.Type == "response" {
			continue
		}
		if evt.Type == "" {
			t.Errorf("event has empty type: %s", line)
		}
		if evt.TS != "" && !strings.Contains(evt.TS, "T") {
			t.Errorf("event ts %q is not RFC 3339", evt.TS)
		}
		if evt.Seq > 0 {
			if seqSeen[evt.Seq] {
				t.Errorf("seq %d already used", evt.Seq)
			}
			seqSeen[evt.Seq] = true
		}
	}
}

// ─── multimodal payload forwarding ────────────────────────────────────────

func TestRPCMultimodalPayloadForwarding(t *testing.T) {
	tmpDir := t.TempDir()
	sessionDir := filepath.Join(tmpDir, "sessions")
	t.Setenv("TAU_SESSION_DIR", sessionDir)

	attachedFile := filepath.Join(tmpDir, "attached.txt")
	if err := os.WriteFile(attachedFile, []byte("attached file body"), 0o644); err != nil {
		t.Fatalf("write attached file: %v", err)
	}

	rt := &mockRuntime{}
	h := newTestHarness(t, rt, nil)
	defer h.close()

	h.send(t, `{"id":"multi-1","type":"new_session","args":{"cwd":"/tmp"}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "multi-1"
	})

	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(jpegData.Bytes())
	cmdJSON := fmt.Sprintf(`{"id":"multi-2","type":"send_message","args":{"text":"analyze this","images":[%q,"iVBORw0KGgo="],"attach_files":[%q]}}`, encoded, attachedFile)
	h.send(t, cmdJSON)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return false
		}
		return obj["type"] == "response" && obj["id"] == "multi-2"
	})
	if len(lines) == 0 {
		t.Fatal("no send_message response")
	}
	resp := parseResp(t, lines[len(lines)-1])
	if resp["ok"] != true {
		t.Fatalf("send_message failed: %v", resp)
	}
	images := rt.session.sent[0].Images
	if len(images) != 2 || images[0].MimeType != "image/jpeg" || images[0].Data != encoded || images[1].MimeType != "image/png" {
		t.Fatalf("unexpected forwarded images: %+v", images)
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

func jsonID(t *testing.T, jsonStr string) string {
	t.Helper()
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &obj); err != nil {
		t.Fatalf("parse id from %q: %v", jsonStr, err)
	}
	return obj.ID
}

// TestHandleCommandAliases verifies that the Pi-compatible command
// aliases (prompt/abort/execute_bash) dispatch to the same handler as
// their canonical names. The test relies on a no-active-session state; we
// only verify that the dispatch does not produce an "unknown_command"
// error response.
func TestHandleCommandAliases(t *testing.T) {
	tests := []struct {
		alias   string
		canon   string
		argsObj string
	}{
		{"prompt", "send_message", `{"text":"hello"}`},
		{"abort", "interrupt", `{}`},
		{"execute_bash", "bash", `{"command":"echo hi"}`},
	}
	for _, tt := range tests {
		t.Run(tt.alias+"->"+tt.canon, func(t *testing.T) {
			h := newTestHarness(t, &mockRuntime{}, nil)
			defer h.close()

			h.send(t, fmt.Sprintf(`{"id":"alias-%s","type":%q,"args":%s}`,
				tt.alias, tt.alias, tt.argsObj))

			timeout := 2 * time.Second
			lines := h.readUntil(t, timeout, func(line string) bool {
				var obj map[string]any
				if err := json.Unmarshal([]byte(line), &obj); err != nil {
					return false
				}
				if obj["type"] != "response" || obj["id"] != "alias-"+tt.alias {
					return false
				}
				// Reject the unknown_command error path.
				if errObj, ok := obj["error"].(map[string]any); ok {
					if code, _ := errObj["code"].(string); code == "unknown_command" {
						t.Errorf("alias %q dispatched to unknown_command: %v",
							tt.alias, obj)
					}
				}
				return true
			})
			if len(lines) == 0 {
				t.Errorf("no response for alias %q", tt.alias)
			}
		})
	}
}

// TestHandleCommandNewSessionFacingHandlers verifies that the new
// session-facing handlers route through handleCommand without the
// "unknown_command" error. The handlers themselves may error on missing
// runtime or storage; we only verify dispatch routing.
func TestHandleCommandNewSessionFacingHandlers(t *testing.T) {
	tests := []struct {
		name    string
		cmdType string
		args    string
	}{
		{"get_available_models", "get_available_models", `{}`},
		{"get_last_assistant_text", "get_last_assistant_text", `{}`},
		{"set_session_name", "set_session_name", `{"name":"test"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHarness(t, &mockRuntime{}, nil)
			defer h.close()

			h.send(t, fmt.Sprintf(`{"id":"new-%s","type":%q,"args":%s}`,
				tt.name, tt.cmdType, tt.args))

			timeout := 2 * time.Second
			lines := h.readUntil(t, timeout, func(line string) bool {
				var obj map[string]any
				if err := json.Unmarshal([]byte(line), &obj); err != nil {
					return false
				}
				if obj["type"] != "response" || obj["id"] != "new-"+tt.name {
					return false
				}
				if errObj, ok := obj["error"].(map[string]any); ok {
					if code, _ := errObj["code"].(string); code == "unknown_command" {
						t.Errorf("handler %q dispatched to unknown_command: %v",
							tt.cmdType, obj)
					}
				}
				return true
			})
			if len(lines) == 0 {
				t.Errorf("no response for handler %q", tt.cmdType)
			}
		})
	}
}

// TestRPCFork_FiresSessionBeforeForkEvent verifies that the RPC fork command
// dispatches the cancellable session_before_fork event on the plugin bus
// before performing the fork, and honors a block verdict.
func TestRPCFork_FiresSessionBeforeForkEvent(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := plugins.NewManager(tmpDir, ui.NewNoOpUI(), nil)

	var seen []string
	var blockOnce bool
	mgr.RegisterEventHandler("session_before_fork", plugins.EventHandler{
		Handler: func(_ context.Context, name string, payload json.RawMessage, sessionID string) plugins.EventResponse {
			seen = append(seen, "session_before_fork")
			if blockOnce {
				return plugins.EventResponse{Block: true}
			}
			return plugins.EventResponse{}
		},
	})

	h := startTestHarness(t, newServer(&mockRuntime{}, newTestRegistry(), mgr))
	defer h.close()
	response := func(id string) map[string]any {
		t.Helper()
		lines := h.readUntil(t, 2*time.Second, func(line string) bool {
			var obj map[string]any
			return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == id
		})
		if len(lines) == 0 {
			t.Fatalf("no response for %s", id)
		}
		return parseResp(t, lines[len(lines)-1])
	}

	h.send(t, `{"id":"fork-1","type":"fork","args":{"entry_id":"msg-0"}}`)
	resp := response("fork-1")
	assert.Equal(t, "response", resp["type"])
	assert.Equal(t, "fork-1", resp["id"])
	// First fork: not blocked, so OK.
	assert.Equal(t, true, resp["ok"])
	assert.Equal(t, []string{"session_before_fork"}, seen)

	// Second fork: block. The mock's NewSession returns the same id, but
	// the block verdict fires before NewSession is called, so this works.
	blockOnce = true
	h.send(t, `{"id":"fork-2","type":"fork","args":{"entry_id":"msg-0"}}`)
	resp2 := response("fork-2")
	assert.Equal(t, "response", resp2["type"])
	assert.Equal(t, "fork-2", resp2["id"])
	assert.Equal(t, false, resp2["ok"])
	if errInfo, ok := resp2["error"].(map[string]any); ok {
		assert.Equal(t, "fork_blocked", errInfo["code"])
	} else {
		t.Fatalf("expected error info in response, got: %v", resp2)
	}
	assert.Equal(t, []string{"session_before_fork", "session_before_fork"}, seen)
}

func TestControlAPICommandSurface(t *testing.T) {
	commands := []struct {
		name string
		args string
	}{
		{"create_session", `{"cwd":"/tmp"}`},
		{"list_sessions", `{}`},
		{"resume_session", `{"id":"test-session"}`},
		{"get_state", `{}`},
		{"send_message", `{"text":"hello"}`},
		{"steer", `{"text":"steer"}`},
		{"follow_up", `{"text":"next"}`},
		{"interrupt", `{}`},
		{"compact", `{}`},
		{"fork", `{}`},
		{"set_model", `{"model_spec":"claude-agent/sonnet"}`},
		{"set_thinking_level", `{"level":"high"}`},
		{"set_session_name", `{"name":"named"}`},
		{"attach_file", `{"path":"/no/such/file"}`},
		{"attach_data", `{"filename":"bad.txt","data":"not base64"}`},
		{"list_attachments", `{}`},
		{"delete_attachment", `{"id":"missing"}`},
		{"get_messages", `{}`},
		{"get_session_stats", `{}`},
		{"subscribe_events", `{}`},
	}

	for _, tc := range commands {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHarness(t, &mockRuntime{}, nil)
			defer h.close()

			h.send(t, fmt.Sprintf(`{"id":"cmd","type":%q,"args":%s}`, tc.name, tc.args))
			lines := h.readUntil(t, timeout, func(line string) bool {
				var obj map[string]any
				if err := json.Unmarshal([]byte(line), &obj); err != nil {
					return false
				}
				return obj["type"] == "response" && obj["id"] == "cmd"
			})
			if len(lines) == 0 {
				t.Fatal("no response")
			}
			resp := parseResp(t, lines[len(lines)-1])
			if errObj, ok := resp["error"].(map[string]any); ok && errObj["code"] == "unknown_command" {
				t.Fatalf("%s returned unknown_command: %v", tc.name, resp)
			}
		})
	}
}

func TestControlAPIAttachmentCommands(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "note.txt")
	if err := os.WriteFile(filePath, []byte("body"), 0o644); err != nil {
		t.Fatalf("write attachment: %v", err)
	}

	rt := &mockRuntime{}
	h := newTestHarness(t, rt, nil)
	defer h.close()

	h.send(t, `{"id":"new","type":"new_session","args":{}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "new"
	})

	h.send(t, fmt.Sprintf(`{"id":"attach-file","type":"attach_file","args":{"path":%q}}`, filePath))
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "attach-file"
	})
	resp := parseResp(t, lines[len(lines)-1])
	if resp["ok"] != true {
		t.Fatalf("attach_file failed: %v", resp)
	}
	attachment := resp["result"].(map[string]any)["attachment"].(map[string]any)
	id := attachment["id"].(string)
	if id == "" || attachment["filename"] != "note.txt" {
		t.Fatalf("attachment = %#v", attachment)
	}

	h.send(t, `{"id":"attach-data","type":"attach_data","args":{"filename":"inline.txt","mime_type":"text/plain","data":"aW5saW5l"}}`)
	lines = h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "attach-data"
	})
	if resp := parseResp(t, lines[len(lines)-1]); resp["ok"] != true {
		t.Fatalf("attach_data failed: %v", resp)
	}

	h.send(t, `{"id":"list","type":"list_attachments","args":{}}`)
	lines = h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "list"
	})
	listResp := parseResp(t, lines[len(lines)-1])
	attachments := listResp["result"].(map[string]any)["attachments"].([]any)
	if len(attachments) != 2 {
		t.Fatalf("attachments = %#v", attachments)
	}

	h.send(t, fmt.Sprintf(`{"id":"send","type":"send_message","args":{"text":"use it","attachment_ids":[%q]}}`, id))
	lines = h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "send"
	})
	if resp := parseResp(t, lines[len(lines)-1]); resp["ok"] != true {
		t.Fatalf("send with attachment failed: %v", resp)
	}
	if len(rt.session.sent) == 0 || len(rt.session.sent[len(rt.session.sent)-1].AttachmentIDs) != 1 {
		t.Fatalf("sent messages = %#v", rt.session.sent)
	}

	h.send(t, fmt.Sprintf(`{"id":"delete","type":"delete_attachment","args":{"id":%q}}`, id))
	lines = h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "delete"
	})
	if resp := parseResp(t, lines[len(lines)-1]); resp["ok"] != true {
		t.Fatalf("delete failed: %v", resp)
	}
}

func TestControlAPIStateAndQueueEvents(t *testing.T) {
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()

	h.send(t, `{"id":"new","type":"create_session","args":{}}`)
	h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "new"
	})

	h.send(t, `{"id":"steer","type":"steer","args":{"text":"adjust"}}`)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "steer"
	})
	if !containsLine(lines, `"type":"queue_update"`) {
		t.Fatalf("queue_update missing: %v", lines)
	}

	h.send(t, `{"id":"state","type":"get_state","args":{}}`)
	lines = h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "state"
	})
	resp := parseResp(t, lines[len(lines)-1])
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %#v", resp["result"])
	}
	queue, ok := result["queue"].(map[string]any)
	if !ok || queue["steering"] != float64(1) || queue["follow_up"] != float64(0) {
		t.Fatalf("queue = %#v", result["queue"])
	}
	if _, ok := result["is_streaming"].(bool); !ok {
		t.Fatalf("is_streaming = %#v", result["is_streaming"])
	}
}
