package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// holdOwnerAsOtherProcess locks the owner file through its own open file
// description. flock treats that exactly like a lock held by another process.
func holdOwnerAsOtherProcess(t *testing.T, sessionPath, content string) {
	t.Helper()
	f, err := os.OpenFile(sessionPath+".owner", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("lock owner as other process: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

// requireOwnerHeld fails unless another process would be refused the owner lock.
func requireOwnerHeld(t *testing.T, sessionPath string) {
	t.Helper()
	f, err := os.Open(sessionPath + ".owner")
	if err != nil {
		t.Fatalf("session owner: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("another process could lock the session owner: %v", err)
	}
}

func requireOwnerReleased(t *testing.T, sessionPath string) {
	t.Helper()
	if _, err := os.Stat(sessionPath + ".owner"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session owner after release: %v", err)
	}
}

func TestNewWriterHoldsOwnerLockForItsLifetime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	w, err := NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	requireOwnerHeld(t, path)
	if err := w.Append(NewSessionInfoEntry("name")); err != nil {
		t.Fatal(err)
	}
	requireOwnerHeld(t, path)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	requireOwnerReleased(t, path)
}

func TestNewWriterRejectsSessionLockedByAnotherProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	holdOwnerAsOtherProcess(t, path, fmt.Sprintf("%d flock\n", os.Getppid()))
	w, err := NewWriter(path)
	if err == nil {
		if closeErr := w.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("opened a writer for a session locked by another process")
	}
	if !strings.Contains(err.Error(), "another process") {
		t.Fatalf("error = %v, want another-process ownership", err)
	}
	if err := ClaimOwner(path); err == nil {
		t.Fatal("claimed a session locked by another process")
	}
}

func stubOwnerBusyWait(t *testing.T, wait func()) {
	t.Helper()
	orig := ownerBusyWait
	ownerBusyWait = wait
	t.Cleanup(func() { ownerBusyWait = orig })
}

func TestNewWriterWaitsOutStaleOwnerCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	ownerPath := path + ".owner"
	// A crashed owner that CleanupStaleOwners has locked to remove.
	cleaner, err := os.OpenFile(ownerPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(cleaner, "%d flock\n", os.Getppid()); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(cleaner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	var waits int
	stubOwnerBusyWait(t, func() {
		waits++
		if waits != 1 {
			return
		}
		if err := os.Remove(ownerPath); err != nil {
			t.Error(err)
		}
		if err := cleaner.Close(); err != nil {
			t.Error(err)
		}
	})

	w, err := NewWriter(path)
	if err != nil {
		t.Fatalf("NewWriter during stale owner cleanup: %v", err)
	}
	if waits != 1 {
		t.Fatalf("busy waits = %d, want 1", waits)
	}
	requireOwnerHeld(t, path)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	requireOwnerReleased(t, path)
}

func TestNewWriterRetriesLiveOwnerBoundedTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	holdOwnerAsOtherProcess(t, path, fmt.Sprintf("%d flock\n", os.Getppid()))
	var waits int
	stubOwnerBusyWait(t, func() { waits++ })
	w, err := NewWriter(path)
	if err == nil {
		if closeErr := w.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("opened a writer for a session locked by another process")
	}
	if waits != ownerBusyRetries {
		t.Fatalf("busy waits = %d, want %d", waits, ownerBusyRetries)
	}
	requireOwnerHeld(t, path)
}

func TestOwnerClaimHonorsLegacyPIDFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		content   string
		claimable bool
	}{
		"live legacy owner":  {content: fmt.Sprintf("%d", os.Getppid())},
		"dead legacy owner":  {content: "99999999", claimable: true},
		"crashed lock owner": {content: fmt.Sprintf("%d flock\n", os.Getppid()), claimable: true},
		"unparsable owner":   {content: "garbage", claimable: true},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path+".owner", []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			w, err := NewWriter(path)
			if !tc.claimable {
				if err == nil {
					if closeErr := w.Close(); closeErr != nil {
						t.Error(closeErr)
					}
					t.Fatal("claimed a session owned by a running legacy process")
				}
				data, err := os.ReadFile(path + ".owner")
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != tc.content {
					t.Fatalf("legacy owner rewritten to %q", data)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			requireOwnerHeld(t, path)
			data, err := os.ReadFile(path + ".owner")
			if err != nil {
				t.Fatal(err)
			}
			// Older processes read only the leading PID.
			var pid int
			if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
				t.Fatalf("parse owner %q: %v", data, err)
			}
			if pid != os.Getpid() {
				t.Fatalf("owner PID = %d, want %d", pid, os.Getpid())
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			requireOwnerReleased(t, path)
		})
	}
}

func TestOwnerClaimSurvivesMarkDeleted(t *testing.T) {
	w, err := CreateSessionFile(t.TempDir(), NewHeader("/test", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	msg := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"1"}`), nil)
	if err := w.Append(msg); err != nil {
		t.Fatal(err)
	}
	if err := w.MarkDeleted([]string{msg.ID}); err != nil {
		t.Fatal(err)
	}
	requireOwnerHeld(t, w.Path())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	requireOwnerReleased(t, w.Path())
}

func TestWriterCloseReleasesOnlyItsOwnClaim(t *testing.T) {
	w, err := CreateSessionFile(t.TempDir(), NewHeader("/test", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	path := w.Path()
	if err := ClaimOwner(path); err != nil {
		t.Fatal(err)
	}
	aux, err := NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := aux.Close(); err != nil {
		t.Fatal(err)
	}
	requireOwnerHeld(t, path)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	requireOwnerHeld(t, path)
	if err := ReleaseOwner(path); err != nil {
		t.Fatal(err)
	}
	requireOwnerReleased(t, path)

	// A release without a matching ClaimOwner must not drop a writer's claim.
	live, err := NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOwner(path); err != nil {
		t.Fatal(err)
	}
	requireOwnerHeld(t, path)
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	requireOwnerReleased(t, path)
}

func TestRenameSessionMovesOwnerClaim(t *testing.T) {
	header := NewHeader("/test", nil, nil)
	header.ID = "12345678-abcd-1234-abcd-123456789abc"
	w, err := CreateSessionFile(t.TempDir(), header)
	if err != nil {
		t.Fatal(err)
	}
	oldPath := w.Path()
	newPath, err := RenameSession(oldPath, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if newPath == oldPath {
		t.Fatal("rename kept the old path")
	}
	requireOwnerHeld(t, newPath)
	requireOwnerReleased(t, oldPath)

	// The live writer hands over to a writer on the new path, as slug renames do.
	next, err := NewWriter(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	requireOwnerHeld(t, newPath)
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	requireOwnerReleased(t, newPath)
}

func TestRenameSessionRefusesSessionOwnedElsewhere(t *testing.T) {
	w, err := CreateSessionFile(t.TempDir(), NewHeader("/test", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	path := w.Path()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	holdOwnerAsOtherProcess(t, path, "")
	if _, err := RenameSession(path, "renamed"); err == nil {
		t.Fatal("renamed a session owned by another process")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session moved despite foreign owner: %v", err)
	}
	requireOwnerHeld(t, path)
}

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

func TestRenameSessionSkipsDestinationWithOwnerFile(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	header.ID = "12345678-abcd-1234-abcd-123456789abc"
	writer, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatal(err)
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	occupied := filepath.Join(dir, header.Timestamp[:10]+"-12345678-renamed.jsonl")
	stale := fmt.Sprintf("%d flock\n", os.Getppid())
	if err := os.WriteFile(occupied+".owner", []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	renamed, err := RenameSession(path, "renamed")
	if err != nil {
		t.Fatalf("rename beside a leftover destination owner: %v", err)
	}
	if renamed == occupied {
		t.Fatal("rename selected a destination with an owner file")
	}
	reader, err := ReadFile(renamed)
	if err != nil {
		t.Fatal(err)
	}
	if reader.Header().ID != header.ID {
		t.Fatal("renamed session lost its identity")
	}
	data, err := os.ReadFile(occupied + ".owner")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != stale {
		t.Fatalf("destination owner rewritten to %q", data)
	}
	requireOwnerReleased(t, path)
	requireOwnerReleased(t, renamed)
}

func TestUniqueFilenameSkipsNamesWithOwnerFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl.owner"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	name, err := uniqueFilename(dir, "session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if name != "session-2.jsonl" {
		t.Fatalf("uniqueFilename = %q, want session-2.jsonl", name)
	}
}

func TestWriterCloseReleasesOwnerAfterFileCloseFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writer, err := NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("close error = %v", err)
	}
	requireOwnerReleased(t, path)
}
