package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/ai"
	"github.com/stretchr/testify/require"
)

func TestForkAtEntryRetainsLegacyCompaction(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	parentID := header.ID
	system := NewMessageEntry(nil, json.RawMessage(`{"role":"system","content":"policy","toolsRemoved":[{"name":"removed"}]}`), nil)
	old := NewMessageEntry(&system.ID, json.RawMessage(`{"role":"user","content":"old"}`), nil)
	kept := NewMessageEntry(&old.ID, json.RawMessage(`{"role":"user","content":"kept"}`), nil)
	compact := NewCompactionEntry("legacy summary", kept.ID, 10, 2, false)
	descendant := NewMessageEntry(&kept.ID, json.RawMessage(`{"role":"assistant","content":"recent"}`), nil)
	sibling := NewMessageEntry(&old.ID, json.RawMessage(`{"role":"user","content":"sibling"}`), nil)
	siblingCompact := NewCompactionEntry("sibling summary", kept.ID, 10, 2, false)
	siblingCompact.ParentID = &sibling.ID
	for _, entry := range []Entry{system, old, kept, compact, sibling, siblingCompact, descendant} {
		if err := writer.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	fork, err := storage.Fork(parentID, &descendant.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := fork.Path()
	if err := fork.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range reader.Entries() {
		if entry.ID == sibling.ID || entry.ID == siblingCompact.ID {
			t.Fatal("fork copied sibling state")
		}
	}
	entries := reader.ContextEntries()
	if len(entries) != 4 || entries[0].ID != system.ID || entries[1].ID != compact.ID || entries[2].ID != kept.ID || entries[3].ID != descendant.ID {
		t.Fatalf("fork changed selected context: %+v", entries)
	}
}

func TestStorageCompactionAttachesToSelectedHead(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	user := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"kept"}`), nil)
	sibling := NewMessageEntry(&user.ID, json.RawMessage(`{"role":"assistant","content":"sibling"}`), nil)
	for _, entry := range []Entry{user, sibling, NewPositionChangeEntry(user.ID)} {
		if err := writer.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOwner(path); err != nil {
		t.Fatal(err)
	}
	if err := storage.Compact(header.ID, "summary", user.ID, 10, 2); err != nil {
		t.Fatal(err)
	}
	reader, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	summaries := reader.EntriesByType(EntryTypeCompaction)
	if len(summaries) != 1 || summaries[0].ParentID == nil || *summaries[0].ParentID != user.ID || reader.CurrentPosition() != summaries[0].ID {
		t.Fatalf("compaction detached from selected head: %+v", summaries)
	}
}

func TestStorageBranchSummaryAttachesToSelectedHead(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	user := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"kept"}`), nil)
	sibling := NewMessageEntry(&user.ID, json.RawMessage(`{"role":"assistant","content":"sibling"}`), nil)
	for _, entry := range []Entry{user, sibling, NewPositionChangeEntry(user.ID)} {
		if err := writer.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOwner(path); err != nil {
		t.Fatal(err)
	}
	if err := storage.SummarizeBranch(header.ID, sibling.ID, "branch summary"); err != nil {
		t.Fatal(err)
	}
	reader, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	summaries := reader.EntriesByType(EntryTypeBranchSummary)
	if len(summaries) != 1 || summaries[0].ParentID == nil || *summaries[0].ParentID != user.ID || reader.CurrentPosition() != summaries[0].ID {
		t.Fatalf("branch summary detached from selected head: %+v", summaries)
	}

	messages := reader.ToAgentMessages()
	if len(messages) != 2 || messages[1].Role != MessageRoleUser || messages[1].Content[0].Text != "The following is a summary of a branch that this conversation came back from:\n\n<summary>\nbranch summary</summary>" {
		t.Fatalf("restored summary context=%+v", messages)
	}
	for _, id := range reader.ActiveBranch() {
		if id == sibling.ID {
			t.Fatal("abandoned sibling joined active branch")
		}
	}
}

func TestStorageRestoreRejectsAmbiguousPrefix(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, id := range []string{"shared-1", "shared-2"} {
		header := NewHeader("/test", nil, nil)
		header.ID = id
		writer, err := storage.Create(header)
		if err != nil {
			t.Fatal(err)
		}
		path := writer.Path()
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := ReleaseOwner(path); err != nil {
			t.Fatal(err)
		}
		if err := storage.Delete(id); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, filepath.Join(storage.TrashDir(), filepath.Base(path)))
	}
	if err := storage.Restore("shared-"); err == nil {
		t.Fatal("restored an arbitrary session from an ambiguous prefix")
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("ambiguous restore changed trash: %v", err)
		}
	}
	if err := storage.Restore("shared-1"); err != nil {
		t.Fatal(err)
	}
	reader, err := storage.Open("shared-1")
	if err != nil {
		t.Fatal(err)
	}
	if reader.Header().ID != "shared-1" {
		t.Fatal("restored wrong session")
	}
	if err := storage.Restore(""); err == nil {
		t.Fatal("empty ID restored the remaining trashed session")
	}
	if _, err := os.Stat(paths[1]); err != nil {
		t.Fatalf("empty restore changed trash: %v", err)
	}
}

func TestStoragePinnedSessionAliases(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{header.ID[:4], path} {
		if err := storage.SetPinned(alias, true); err != nil {
			t.Fatal(err)
		}
		summaries, err := storage.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(summaries) != 1 || !summaries[0].Pinned || summaries[0].PinnedAt == "" {
			t.Fatalf("pin via %q not reflected in list: %+v", alias, summaries)
		}
		if err := storage.SetPinned(header.ID, false); err != nil {
			t.Fatal(err)
		}
		if err := storage.SetPinned(header.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := storage.SetPinned(alias, false); err != nil {
			t.Fatal(err)
		}
		summaries, err = storage.ListCWD(header.CWD)
		if err != nil {
			t.Fatal(err)
		}
		if len(summaries) != 1 || summaries[0].Pinned {
			t.Fatalf("unpin via %q failed: %+v", alias, summaries)
		}
	}
}

func TestStorageBranchToRejectsMissingEntry(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	message := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"kept"}`), nil)
	if err := writer.Append(message); err != nil {
		t.Fatal(err)
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"missing", ""} {
		if err := storage.BranchTo(header.ID, target); err == nil {
			t.Fatalf("accepted missing branch target %q", target)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatal("invalid branch changed session")
		}
	}
	for _, target := range []string{message.ID, header.ID} {
		if err := storage.BranchTo(header.ID, target); err != nil {
			t.Fatal(err)
		}
		reader, err := storage.Open(header.ID)
		if err != nil {
			t.Fatal(err)
		}
		if reader.CurrentPosition() != target {
			t.Fatalf("position = %q, want %q", reader.CurrentPosition(), target)
		}
	}
}

func TestStorageRestoreFullID(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := storage.Delete(header.ID); err != nil {
		t.Fatal(err)
	}
	if err := storage.Restore(header.ID); err != nil {
		t.Fatal(err)
	}
	reader, err := storage.Open(header.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reader.Header().ID != header.ID {
		t.Fatal("restored wrong session")
	}
}

func TestStorageForkRejectsMissingEntryWithoutCreatingSession(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"missing", ""} {
		fork, err := storage.Fork(header.ID, &target)
		if fork != nil {
			if err := fork.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err == nil {
			t.Errorf("accepted missing fork target %q", target)
		}
	}
	sessions, err := storage.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("failed fork left %d sessions, want 1", len(sessions))
	}
}

func TestStorageGetBranchEntriesRejectsCycle(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	message := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"cycle"}`), nil)
	message.ParentID = &message.ID
	if err := writer.Append(message); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetBranchEntries(header.ID, message.ID); err == nil {
		t.Fatal("accepted cyclic branch")
	}
	if fork, err := storage.Fork(header.ID, &message.ID); err == nil {
		if err := fork.Close(); err != nil {
			t.Fatal(err)
		}
		t.Fatal("fork accepted cyclic branch")
	}
	sessions, err := storage.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("cyclic fork created %d sessions, want 1", len(sessions))
	}
}

func TestStorageRestoreMatchesLiteralIDPrefix(t *testing.T) {
	for _, target := range []string{"*", "decoy", "23456789", "123456789"} {
		t.Run(target, func(t *testing.T) {
			storage, err := NewStorageAt(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			header := NewHeader("/test", nil, nil, "decoy")
			header.ID = "12345678-abcd-1234-abcd-123456789abc"
			writer, err := storage.Create(header)
			if err != nil {
				t.Fatal(err)
			}
			path := writer.Path()
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := storage.Delete(header.ID); err != nil {
				t.Fatal(err)
			}
			if err := storage.Restore(target); err == nil {
				t.Errorf("restored unrelated ID for %q", target)
			}
			if _, err := os.Stat(filepath.Join(storage.TrashDir(), filepath.Base(path))); err != nil {
				t.Fatalf("invalid restore changed trash: %v", err)
			}
			if err := storage.Restore(header.ID[:12]); err != nil {
				t.Fatalf("restore longer ID prefix: %v", err)
			}
		})
	}
}

func TestStorageRestorePreservesExistingDestination(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	header.ID = "12345678-abcd-1234-abcd-123456789abc"
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := storage.Delete(header.ID); err != nil {
		t.Fatal(err)
	}
	active := header
	active.ID = "12345678-abcd-1234-abcd-987654321abc"
	writer, err = storage.Create(active)
	if err != nil {
		t.Fatal(err)
	}
	if writer.Path() != path {
		t.Fatal("fixture did not produce a destination collision")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Restore(header.ID); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("restore overwrote the active session")
	}
	files, err := storage.findSessionFiles("*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d session files, want both sessions", len(files))
	}
	var restored bool
	for _, file := range files {
		h, err := ReadHeader(file)
		if err != nil {
			t.Fatal(err)
		}
		if h.ID == header.ID {
			restored = true
		}
	}
	if !restored {
		t.Fatal("restored session is missing")
	}
}

func TestStorageRestoreRejectsDuplicateFullID(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	for range 2 {
		writer, err := storage.Create(header)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := storage.Delete(header.ID); err != nil {
			t.Fatal(err)
		}
	}
	files, err := filepath.Glob(filepath.Join(storage.TrashDir(), "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d trashed copies, want 2", len(files))
	}
	if err := storage.Restore(header.ID); err == nil {
		t.Fatal("restored an arbitrary copy of a duplicated full ID")
	}
	for _, path := range files {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("ambiguous restore changed trash: %v", err)
		}
	}
}

func TestStorageResolveIDUsesLiteralHeaders(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil, "decoy")
	header.ID = "12345678-abcd-1234-abcd-123456789abc"
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "*", "decoy", "2345678"} {
		if got, err := storage.resolveID(id); err == nil {
			t.Errorf("resolved invalid ID %q to %q", id, got)
		}
	}
	for _, id := range []string{header.ID, header.ID[:12], path} {
		got, err := storage.resolveID(id)
		if err != nil || got != path {
			t.Errorf("resolve %q = %q, %v; want %q", id, got, err, path)
		}
	}
	other := header
	other.ID += "-longer"
	writer, err = storage.Create(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.resolveID(header.ID); err != nil || got != path {
		t.Errorf("exact ID lost precedence: %q, %v", got, err)
	}
	if got, err := storage.resolveID(header.ID[:12]); err == nil {
		t.Errorf("resolved ambiguous prefix to %q", got)
	}
}

func TestForkAtEntryKeepsSummaryUsageOnItsBranch(t *testing.T) {
	storage, err := NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := NewHeader("/test", nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	root := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"root"}`), nil)
	kept := NewMessageEntry(&root.ID, json.RawMessage(`{"role":"assistant","content":"kept"}`), nil)
	sibling := NewMessageEntry(&root.ID, json.RawMessage(`{"role":"assistant","content":"sibling"}`), nil)
	keptUsage, err := NewSummaryUsageEntry(&kept.ID, ai.Usage{Input: 10, Output: 2})
	if err != nil {
		t.Fatal(err)
	}
	siblingUsage, err := NewSummaryUsageEntry(&sibling.ID, ai.Usage{Input: 99})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []Entry{root, kept, keptUsage, sibling, siblingUsage} {
		if err := writer.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	fork, err := storage.Fork(header.ID, &kept.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := fork.Path()
	if err := fork.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, entry := range reader.Entries() {
		ids = append(ids, entry.ID)
	}
	if !slices.Equal(ids, []string{root.ID, kept.ID, keptUsage.ID}) {
		t.Fatalf("fork entries = %v, want root, kept, and kept summary usage", ids)
	}
	if position := reader.CurrentPosition(); position != kept.ID {
		t.Fatalf("summary usage moved the fork position to %q", position)
	}
}

func TestCleanupStaleOwnersReportsReadFailuresAndContinues(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "a.owner")
	if err := os.Symlink("a.owner", bad); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "b.owner")
	if err := os.WriteFile(stale, []byte("99999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dir, "c.owner")
	if err := os.WriteFile(live, fmt.Appendf(nil, "%d", os.Getpid()), 0o600); err != nil {
		t.Fatal(err)
	}
	storage := &Storage{baseDir: dir}
	err := storage.CleanupStaleOwners()
	if err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("expected owner read failure, got %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale owner not removed: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live owner removed: %v", err)
	}
}

func TestRecursiveSessionDiscoveryReportsMissingRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	storage, err := NewStorageAt(dir)
	require.NoError(t, err)
	require.NoError(t, os.Remove(dir))
	_, err = storage.findSessionFiles("*.jsonl")
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = storage.ownerFiles()
	require.ErrorIs(t, err, os.ErrNotExist)
	require.ErrorIs(t, storage.CleanupStaleOwners(), os.ErrNotExist)
}
