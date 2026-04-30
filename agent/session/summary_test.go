package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSummaryMatchesReaderSummary(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test/cwd", nil, nil, "header name")
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}

	root := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"hello"}`), &TokenCounts{Input: 3})
	if err := w.Append(root); err != nil {
		t.Fatalf("Append root: %v", err)
	}
	if err := w.Append(NewModelChangeEntry("anthropic", "claude-sonnet-4-6")); err != nil {
		t.Fatalf("Append model_change: %v", err)
	}
	if err := w.Append(NewSessionInfoEntry("renamed session")); err != nil {
		t.Fatalf("Append session_info: %v", err)
	}
	assistant := NewMessageEntry(&root.ID, json.RawMessage(`{"role":"assistant","content":"hi"}`), &TokenCounts{Output: 5, CachedInput: 7})
	if err := w.Append(assistant); err != nil {
		t.Fatalf("Append assistant: %v", err)
	}
	if err := w.Append(NewQueueUpdateEntry(0, 2)); err != nil {
		t.Fatalf("Append queue_update: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader, err := ReadFile(w.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := summaryFromReader(w.Path(), reader)

	got, err := ReadSummary(w.Path())
	if err != nil {
		t.Fatalf("ReadSummary: %v", err)
	}
	if got != want {
		t.Fatalf("ReadSummary = %#v, want %#v", got, want)
	}
}

func TestReadSummaryDerivesCompleteStatus(t *testing.T) {
	writeSession := func(messages ...string) SessionSummary {
		t.Helper()
		path := filepath.Join(t.TempDir(), "s.jsonl")
		header := NewHeader("/workspace", nil, nil, "complete-status")
		w, err := CreateSessionFile(filepath.Dir(path), header)
		if err != nil {
			t.Fatalf("CreateSessionFile: %v", err)
		}
		for _, raw := range messages {
			if err := w.Append(NewMessageEntry(nil, json.RawMessage(raw), nil)); err != nil {
				t.Fatalf("Append %s: %v", raw, err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		got, err := ReadSummary(w.Path())
		if err != nil {
			t.Fatalf("ReadSummary: %v", err)
		}
		return got
	}

	if got := writeSession(`{"role":"assistant","content":"done","stopReason":"stop"}`); got.Status != "complete" {
		t.Errorf("stop = %q, want complete", got.Status)
	}
	if got := writeSession(`{"role":"assistant","content":"cut","stopReason":"length"}`); got.Status != "complete" {
		t.Errorf("length = %q, want complete", got.Status)
	}
	if got := writeSession(`{"role":"assistant","content":"tool","stopReason":"toolUse"}`); got.Status != "complete" {
		t.Errorf("toolUse = %q, want complete", got.Status)
	}
	if got := writeSession(`{"role":"assistant","content":"no reason"}`); got.Status != "waiting" {
		t.Errorf("legacy empty stop reason = %q, want waiting", got.Status)
	}
	if got := writeSession(`{"role":"user","content":"hi"}`); got.Status != "waiting" {
		t.Errorf("user only = %q, want waiting", got.Status)
	}
	// The last status-setting entry wins: a normal turn followed by an abort
	// reads aborted, and an aborted turn followed by a clean one reads complete.
	if got := writeSession(
		`{"role":"assistant","content":"ok","stopReason":"stop"}`,
		`{"role":"assistant","content":"aborted","stopReason":"aborted"}`,
	); got.Status != "aborted" {
		t.Errorf("trailing abort = %q, want aborted", got.Status)
	}
	if got := writeSession(
		`{"role":"assistant","content":"aborted","stopReason":"aborted"}`,
		`{"role":"assistant","content":"ok","stopReason":"stop"}`,
	); got.Status != "complete" {
		t.Errorf("trailing clean turn = %q, want complete", got.Status)
	}
}

func TestReadSummaryDerivesWorkspaceFields(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/workspace", nil, nil, "workspace")
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}
	if err := w.Append(NewModelChangeEntry("test", "model")); err != nil {
		t.Fatalf("Append model_change: %v", err)
	}
	if err := w.Append(NewQueueUpdateEntry(1, 0)); err != nil {
		t.Fatalf("Append queue_update: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := ReadSummary(w.Path())
	if err != nil {
		t.Fatalf("ReadSummary: %v", err)
	}
	if got.Model != "test/model" {
		t.Fatalf("Model = %q, want test/model", got.Model)
	}
	if got.Status != "queued" {
		t.Fatalf("Status = %q, want queued", got.Status)
	}
	if got.LastModified == "" || got.LastModified < got.Created {
		t.Fatalf("LastModified = %q, Created = %q", got.LastModified, got.Created)
	}
}

func TestReadSummaryStreamsLargeMessageLines(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/large", nil, nil)
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}
	path := w.Path()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	large := strings.Repeat("x", 11*1024*1024)
	_, writeErr := f.WriteString(`{"type":"message","id":"large","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"` + large + `"},"tokens":{"input":1}}` + "\n")
	closeErr := f.Close()
	if writeErr != nil {
		t.Fatalf("WriteString: %v", writeErr)
	}
	if closeErr != nil {
		t.Fatalf("Close append file: %v", closeErr)
	}

	got, err := ReadSummary(path)
	if err != nil {
		t.Fatalf("ReadSummary: %v", err)
	}
	if got.ID != header.ID || got.MessageCount != 1 || got.Tokens.Input != 1 {
		t.Fatalf("ReadSummary = %#v, want id %q, 1 message, 1 input token", got, header.ID)
	}
}

func TestReadSummaryIncludesFirstUserPrompt(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test/cwd", nil, nil)
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}

	assistant := NewMessageEntry(nil, json.RawMessage(`{"role":"assistant","content":"hello"}`), nil)
	if err := w.Append(assistant); err != nil {
		t.Fatalf("Append assistant: %v", err)
	}
	user := NewMessageEntry(&assistant.ID, json.RawMessage(`{"role":"user","content":[{"type":"text","text":"first prompt"},{"type":"image","data":"..."}]}`), nil)
	if err := w.Append(user); err != nil {
		t.Fatalf("Append user: %v", err)
	}
	second := NewMessageEntry(&user.ID, json.RawMessage(`{"role":"user","content":"second prompt"}`), nil)
	if err := w.Append(second); err != nil {
		t.Fatalf("Append second user: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := ReadSummary(w.Path())
	if err != nil {
		t.Fatalf("ReadSummary: %v", err)
	}
	if got.FirstPrompt != "first prompt" {
		t.Fatalf("FirstPrompt = %q, want %q", got.FirstPrompt, "first prompt")
	}
}
