package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrate_SkipsCurrentVersion(t *testing.T) {
	// v3 sessions should not be migrated
	entries := []json.RawMessage{
		json.RawMessage(`{"type":"session","version":3,"id":"abc123","timestamp":"2026-01-01T00:00:00Z","cwd":"/test"}`),
	}

	migrated, err := Migrate(entries)
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if migrated {
		t.Error("Migrate() migrated current version session")
	}
}

func TestMigrate_V2ToV3_HookMessageToCustom(t *testing.T) {
	entries := []json.RawMessage{
		json.RawMessage(`{"type":"session","version":2,"id":"abc123","timestamp":"2026-01-01T00:00:00Z","cwd":"/test"}`),
		json.RawMessage(`{"type":"message","id":"msg1","timestamp":"2026-01-01T00:01:00Z","message":{"role":"hookMessage","content":"test"}}`),
	}

	migrated, err := Migrate(entries)
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if !migrated {
		t.Error("Migrate() should have migrated v2 to v3")
	}

	// Check that version was updated
	var header Header
	if err := json.Unmarshal(entries[0], &header); err != nil {
		t.Fatalf("Parse header: %v", err)
	}
	if header.Version != 3 {
		t.Errorf("header.Version = %d, want 3", header.Version)
	}

	// Check that hookMessage was renamed to custom
	var msg Entry
	if err := json.Unmarshal(entries[1], &msg); err != nil {
		t.Fatalf("Parse message: %v", err)
	}
	var msgContent map[string]any
	if err := json.Unmarshal(msg.Message, &msgContent); err != nil {
		t.Fatalf("Parse message content: %v", err)
	}
	role, ok := msgContent["role"].(string)
	if !ok {
		t.Fatal("role not found in message content")
	}
	if role != "custom" {
		t.Errorf("role = %q, want %q", role, "custom")
	}
}

func TestMigrate_V1ToV2_AddsIDsAndParentIds(t *testing.T) {
	// v1 sessions don't have version field and entries don't have id/parentId
	entries := []json.RawMessage{
		json.RawMessage(`{"type":"session","id":"header1","timestamp":"2026-01-01T00:00:00Z","cwd":"/test"}`),
		json.RawMessage(`{"type":"message","timestamp":"2026-01-01T00:01:00Z","message":{"role":"user","content":"hello"}}`),
		json.RawMessage(`{"type":"message","timestamp":"2026-01-01T00:02:00Z","message":{"role":"assistant","content":"hi"}}`),
	}

	migrated, err := Migrate(entries)
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if !migrated {
		t.Error("Migrate() should have migrated v1 to current version")
	}

	// Check header version was set to current (3), not just v2
	var header Header
	if err := json.Unmarshal(entries[0], &header); err != nil {
		t.Fatalf("Parse header: %v", err)
	}
	if header.Version != CurrentSessionVersion {
		t.Errorf("header.Version = %d, want %d", header.Version, CurrentSessionVersion)
	}

	// Check entries got IDs and parentIds
	var entriesList []Entry
	for _, raw := range entries[1:] {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("Parse entry: %v", err)
		}
		entriesList = append(entriesList, e)
	}

	// First entry should have no parent (or nil parent)
	if entriesList[0].ID == "" {
		t.Error("First entry should have an ID")
	}
	if entriesList[0].ParentID != nil {
		t.Errorf("First entry ParentID = %v, want nil", entriesList[0].ParentID)
	}

	// Second entry should have parentId pointing to first
	if entriesList[1].ID == "" {
		t.Error("Second entry should have an ID")
	}
	if entriesList[1].ParentID == nil || *entriesList[1].ParentID != entriesList[0].ID {
		t.Errorf("Second entry ParentID = %v, want %q", entriesList[1].ParentID, entriesList[0].ID)
	}
}

func TestMigrate_V1ToV2_ConvertsFirstKeptEntryIndex(t *testing.T) {
	// v1 compaction uses firstKeptEntryIndex (number) instead of firstKeptEntryId
	entries := []json.RawMessage{
		json.RawMessage(`{"type":"session","id":"header1","timestamp":"2026-01-01T00:00:00Z","cwd":"/test"}`),
		json.RawMessage(`{"type":"message","timestamp":"2026-01-01T00:01:00Z","message":{"role":"user","content":"hello"}}`),
		json.RawMessage(`{"type":"compaction","timestamp":"2026-01-01T00:02:00Z","summary":"old stuff","firstKeptEntryIndex":1}`),
	}

	migrated, err := Migrate(entries)
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if !migrated {
		t.Error("Migrate() should have migrated v1 to current version")
	}

	// Check target entry got an ID
	var target Entry
	if err := json.Unmarshal(entries[1], &target); err != nil {
		t.Fatalf("Parse target: %v", err)
	}
	if target.ID == "" {
		t.Error("Target entry should have an ID after migration")
	}
	t.Logf("Target entry ID: %s", target.ID)

	// Check compaction entry got firstKeptEntryId
	var compaction Entry
	if err := json.Unmarshal(entries[2], &compaction); err != nil {
		t.Fatalf("Parse compaction: %v", err)
	}
	if compaction.FirstKeptID == nil {
		t.Error("Compaction should have firstKeptEntryId after migration")
	} else if *compaction.FirstKeptID != target.ID {
		t.Errorf("Compaction.FirstKeptID = %q, want %q", *compaction.FirstKeptID, target.ID)
	} else {
		t.Logf("Compaction.FirstKeptID correctly set to %q", *compaction.FirstKeptID)
	}
}

func TestMigrate_EmptyEntries(t *testing.T) {
	entries := []json.RawMessage{}

	migrated, err := Migrate(entries)
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if migrated {
		t.Error("Migrate() should not migrate empty entries")
	}
}

func TestMigrate_NonSessionHeader(t *testing.T) {
	entries := []json.RawMessage{
		json.RawMessage(`{"type":"message","id":"msg1"}`),
	}

	_, err := Migrate(entries)
	if err == nil {
		t.Error("Migrate() should error on non-session header")
	}
}

func TestMigrate_FullChain(t *testing.T) {
	// Test v1 -> v2 -> v3 migration
	entries := []json.RawMessage{
		json.RawMessage(`{"type":"session","id":"header1","timestamp":"2026-01-01T00:00:00Z","cwd":"/test"}`),
		json.RawMessage(`{"type":"message","timestamp":"2026-01-01T00:01:00Z","message":{"role":"user","content":"hello"}}`),
		json.RawMessage(`{"type":"message","timestamp":"2026-01-01T00:02:00Z","message":{"role":"hookMessage","content":"internal"}}`),
	}

	migrated, err := Migrate(entries)
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if !migrated {
		t.Error("Migrate() should have migrated v1 to v3")
	}

	// Check final version is 3
	var header Header
	if err := json.Unmarshal(entries[0], &header); err != nil {
		t.Fatalf("Parse header: %v", err)
	}
	if header.Version != 3 {
		t.Errorf("header.Version = %d, want 3", header.Version)
	}

	// Check all entries have IDs
	for i := 1; i < len(entries); i++ {
		var e Entry
		if err := json.Unmarshal(entries[i], &e); err != nil {
			t.Fatalf("Parse entry %d: %v", i, err)
		}
		if e.ID == "" {
			t.Errorf("Entry %d should have an ID", i)
		}
	}

	// Check hookMessage was converted
	var hookMsg Entry
	if err := json.Unmarshal(entries[2], &hookMsg); err != nil {
		t.Fatalf("Parse hook message: %v", err)
	}
	var content map[string]any
	if err := json.Unmarshal(hookMsg.Message, &content); err != nil {
		t.Fatalf("Parse message content: %v", err)
	}
	role, ok := content["role"].(string)
	if !ok || role != "custom" {
		t.Errorf("role = %v, want %q", content["role"], "custom")
	}
}

func TestMigrate_ReaderIntegration(t *testing.T) {
	// Create a temporary v1 session file
	tmpDir := t.TempDir()
	v1Session := filepath.Join(tmpDir, "test-session.jsonl")

	v1Content := `{"type":"session","id":"abc123","timestamp":"2026-01-01T00:00:00Z","cwd":"/test"}
{"type":"message","timestamp":"2026-01-01T00:01:00Z","message":{"role":"user","content":"hello"}}
{"type":"message","timestamp":"2026-01-01T00:02:00Z","message":{"role":"hookMessage","content":"internal"}}
`
	if err := os.WriteFile(v1Session, []byte(v1Content), 0o644); err != nil {
		t.Fatalf("Write session file: %v", err)
	}

	// Read with migration
	reader, err := ReadFile(v1Session)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	// Check header version
	if reader.header.Version != 3 {
		t.Errorf("reader.header.Version = %d, want 3", reader.header.Version)
	}

	// Check all entries have IDs
	for _, e := range reader.entries {
		if e.ID == "" {
			t.Error("Entry should have ID after migration")
		}
	}

	// Check first message has correct parent chain
	if len(reader.entries) >= 2 {
		if reader.entries[0].ParentID != nil {
			t.Errorf("First entry ParentID = %v, want nil", reader.entries[0].ParentID)
		}
		if reader.entries[1].ParentID == nil || *reader.entries[1].ParentID != reader.entries[0].ID {
			t.Errorf("Second entry ParentID = %v, want %q", reader.entries[1].ParentID, reader.entries[0].ID)
		}
	}

	// Check that hookMessage was converted
	if len(reader.entries) >= 2 {
		var content map[string]any
		if err := json.Unmarshal(reader.entries[1].Message, &content); err != nil {
			t.Fatalf("Parse message content: %v", err)
		}
		role, ok := content["role"].(string)
		if !ok || role != "custom" {
			t.Errorf("role = %v, want %q", content["role"], "custom")
		}
	}

	// Check that file was updated
	content, err := os.ReadFile(v1Session)
	if err != nil {
		t.Fatalf("Read file after migration: %v", err)
	}

	var lines []string
	for _, line := range splitLines(string(content)) {
		if line != "" {
			lines = append(lines, line)
		}
	}

	if len(lines) < 1 {
		t.Fatal("Session file should have lines")
	}

	// Check first line has version 3
	var checkHeader Header
	if err := json.Unmarshal([]byte(lines[0]), &checkHeader); err != nil {
		t.Fatalf("Parse migrated header: %v", err)
	}
	if checkHeader.Version != 3 {
		t.Errorf("Migrated file header.Version = %d, want 3", checkHeader.Version)
	}
}

func splitLines(s string) []string {
	var lines []string
	for i := 0; i < len(s); {
		eol := i
		for eol < len(s) && s[eol] != '\n' {
			eol++
		}
		lines = append(lines, s[i:eol])
		if eol < len(s) && s[eol] == '\n' {
			eol++
		}
		i = eol
	}
	return lines
}
