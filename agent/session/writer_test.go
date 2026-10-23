package session

import (
	"encoding/json"
	"errors"
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

func TestUniqueFilenamePropagatesStatErrors(t *testing.T) {
	for _, blocked := range []string{"session.jsonl", "session-2.jsonl"} {
		t.Run(blocked, func(t *testing.T) {
			dir := t.TempDir()
			if blocked != "session.jsonl" {
				if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, blocked)
			if err := os.Symlink(blocked, path); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err == nil || os.IsNotExist(err) {
				t.Fatalf("fixture needs a non-not-exist stat error, got %v", err)
			}
			if name, err := uniqueFilename(dir, "session.jsonl"); err == nil {
				t.Fatalf("ignored stat error and selected %q", name)
			}
		})
	}
}

func TestRenameSessionPreservesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil, "original")
	header.ID = "12345678-abcd-1234-abcd-123456789abc"
	writer, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatal(err)
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	other := header
	other.ID = "12345678-abcd-1234-abcd-987654321abc"
	other.Name = "renamed"
	writer, err = CreateSessionFile(dir, other)
	if err != nil {
		t.Fatal(err)
	}
	occupied := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(occupied)
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := RenameSession(path, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(occupied)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("rename overwrote another session")
	}
	if renamed == occupied {
		t.Fatal("rename selected occupied destination")
	}
	reader, err := ReadFile(renamed)
	if err != nil {
		t.Fatal(err)
	}
	if reader.Header().ID != header.ID {
		t.Fatal("renamed session lost its identity")
	}
}

func TestWriterCloseReleasesOwnerAfterFileCloseFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writer, err := NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ClaimOwner(path); err != nil {
		t.Fatal(err)
	}
	if err := writer.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("close error = %v", err)
	}
	if _, err := os.Stat(path + ".owner"); !os.IsNotExist(err) {
		t.Fatalf("owner remains after failed close: %v", err)
	}
}
