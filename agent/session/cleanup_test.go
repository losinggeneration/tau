package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupEmptyMovesOnlySessionsWithoutMessages(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorageAt(dir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}

	empty, err := storage.Create(NewHeader("/empty", nil, nil))
	if err != nil {
		t.Fatalf("create empty: %v", err)
	}
	emptyPath := empty.Path()
	if err := empty.Close(); err != nil {
		t.Fatalf("close empty: %v", err)
	}

	metadataOnly, err := storage.Create(NewHeader("/metadata", nil, nil))
	if err != nil {
		t.Fatalf("create metadata: %v", err)
	}
	metadataPath := metadataOnly.Path()
	if err := metadataOnly.Append(NewSessionInfoEntry("scratch")); err != nil {
		t.Fatalf("append metadata: %v", err)
	}
	if err := metadataOnly.Close(); err != nil {
		t.Fatalf("close metadata: %v", err)
	}

	withMessage, err := storage.Create(NewHeader("/message", nil, nil))
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	messagePath := withMessage.Path()
	if err := withMessage.Append(NewMessageEntry(nil, []byte(`{"role":"user","content":"hello"}`), nil)); err != nil {
		t.Fatalf("append message: %v", err)
	}
	if err := withMessage.Close(); err != nil {
		t.Fatalf("close message: %v", err)
	}

	result, err := storage.CleanupEmpty(context.Background(), CleanupEmptyOptions{})
	if err != nil {
		t.Fatalf("CleanupEmpty: %v", err)
	}
	if len(result.Matched) != 2 {
		t.Fatalf("matched %d sessions, want 2", len(result.Matched))
	}
	if len(result.Removed) != 2 {
		t.Fatalf("removed %d sessions, want 2", len(result.Removed))
	}

	assertMissing(t, emptyPath)
	assertMissing(t, metadataPath)
	assertExists(t, filepath.Join(storage.TrashDir(), filepath.Base(emptyPath)))
	assertExists(t, filepath.Join(storage.TrashDir(), filepath.Base(metadataPath)))
	assertExists(t, messagePath)
}

func TestCleanupEmptyDryRunDoesNotMove(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorageAt(dir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}

	w, err := storage.Create(NewHeader("/empty", nil, nil))
	if err != nil {
		t.Fatalf("create empty: %v", err)
	}
	path := w.Path()
	if err := w.Close(); err != nil {
		t.Fatalf("close empty: %v", err)
	}

	result, err := storage.CleanupEmpty(context.Background(), CleanupEmptyOptions{DryRun: true})
	if err != nil {
		t.Fatalf("CleanupEmpty: %v", err)
	}
	if len(result.Matched) != 1 {
		t.Fatalf("matched %d sessions, want 1", len(result.Matched))
	}
	if len(result.Removed) != 0 {
		t.Fatalf("removed %d sessions, want 0", len(result.Removed))
	}
	assertExists(t, path)
	assertMissing(t, filepath.Join(storage.TrashDir(), filepath.Base(path)))
}

func TestCleanupEmptyRemovesSessionDataDirs(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorageAt(dir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}

	empty, err := storage.Create(NewHeader("/empty", nil, nil))
	if err != nil {
		t.Fatalf("create empty: %v", err)
	}
	emptyID := mustHeaderID(t, empty.Path())
	if err := empty.Close(); err != nil {
		t.Fatalf("close empty: %v", err)
	}
	emptyData := filepath.Join(dir, ".data", emptyID)
	if err := os.MkdirAll(emptyData, 0o755); err != nil {
		t.Fatal(err)
	}

	orphanData := filepath.Join(dir, ".data", "orphan")
	if err := os.MkdirAll(orphanData, 0o755); err != nil {
		t.Fatal(err)
	}

	withMessage, err := storage.Create(NewHeader("/message", nil, nil))
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	messageID := mustHeaderID(t, withMessage.Path())
	if err := withMessage.Append(NewMessageEntry(nil, []byte(`{"role":"user","content":"hello"}`), nil)); err != nil {
		t.Fatalf("append message: %v", err)
	}
	if err := withMessage.Close(); err != nil {
		t.Fatalf("close message: %v", err)
	}
	messageData := filepath.Join(dir, ".data", messageID)
	if err := os.MkdirAll(messageData, 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := storage.CleanupEmpty(context.Background(), CleanupEmptyOptions{})
	if err != nil {
		t.Fatalf("CleanupEmpty: %v", err)
	}
	if len(result.RemovedDataDirs) != 2 {
		t.Fatalf("removed data dirs = %v, want empty session dir and orphan", result.RemovedDataDirs)
	}
	assertMissing(t, emptyData)
	assertMissing(t, orphanData)
	assertExists(t, messageData)
}

func TestCleanupEmptyDryRunKeepsDataDirs(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorageAt(dir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}
	orphanData := filepath.Join(dir, ".data", "orphan")
	if err := os.MkdirAll(orphanData, 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := storage.CleanupEmpty(context.Background(), CleanupEmptyOptions{DryRun: true})
	if err != nil {
		t.Fatalf("CleanupEmpty: %v", err)
	}
	if len(result.OrphanDataDirs) != 1 || result.OrphanDataDirs[0] != "orphan" {
		t.Fatalf("orphan dirs = %v, want [orphan]", result.OrphanDataDirs)
	}
	if len(result.RemovedDataDirs) != 0 {
		t.Fatalf("removed data dirs = %v, want none", result.RemovedDataDirs)
	}
	assertExists(t, orphanData)
}

func mustHeaderID(t *testing.T, path string) string {
	t.Helper()
	header, err := ReadHeader(path)
	if err != nil {
		t.Fatal(err)
	}
	return header.ID
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be missing, stat err=%v", path, err)
	}
}
