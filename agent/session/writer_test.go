package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestOwnerFileSameProcessRefCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	if err := ClaimOwner(path); err != nil {
		t.Fatalf("claim owner first: %v", err)
	}
	if err := ClaimOwner(path); err != nil {
		t.Fatalf("claim owner second: %v", err)
	}

	if err := ReleaseOwner(path); err != nil {
		t.Fatalf("release owner first: %v", err)
	}
	if _, err := os.Stat(path + ".owner"); err != nil {
		t.Fatalf("owner file removed before last same-process writer closed: %v", err)
	}

	if err := ReleaseOwner(path); err != nil {
		t.Fatalf("release owner second: %v", err)
	}
	if _, err := os.Stat(path + ".owner"); !os.IsNotExist(err) {
		t.Fatalf("owner file after final release err = %v, want not exist", err)
	}
}

func TestWriterMarkDeleted(t *testing.T) {
	w, err := CreateSessionFile(t.TempDir(), NewHeader("/test", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	m1 := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"1"}`), nil)
	m2 := NewMessageEntry(&m1.ID, json.RawMessage(`{"role":"assistant","content":"2"}`), nil)
	for _, e := range []Entry{m1, m2} {
		if err := w.Append(e); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.MarkDeleted([]string{m2.ID}); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	// Appends after the rewrite must land in the replacement file.
	m3 := NewMessageEntry(&m1.ID, json.RawMessage(`{"role":"assistant","content":"3"}`), nil)
	if err := w.Append(m3); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), `"status":"deleted"`); n != 1 {
		t.Errorf("deleted markers = %d, want 1:\n%s", n, raw)
	}
	r, err := ReadFile(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range r.Entries() {
		ids = append(ids, e.ID)
	}
	if want := []string{m1.ID, m3.ID}; !slices.Equal(ids, want) {
		t.Errorf("entries = %v, want %v", ids, want)
	}
	if got := r.CurrentPosition(); got != m3.ID {
		t.Errorf("CurrentPosition = %q, want %q", got, m3.ID)
	}
}
