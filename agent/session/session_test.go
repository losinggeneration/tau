package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/losinggeneration/tau/ai"
)

func TestEntryTypes(t *testing.T) {
	tests := []struct {
		typ       EntryType
		inContext bool
	}{
		{EntryTypeMessage, true},
		{EntryTypeCompaction, true},
		{EntryTypeCustomMessage, true},
		{EntryTypePositionChange, false},
		{EntryTypeBranchSummary, false},
		{EntryTypeSessionInfo, false},
		{EntryTypeModelChange, false},
		{EntryTypeThinkingChange, false},
		{EntryTypeCustom, false},
		{EntryTypeError, false},
	}
	for _, tt := range tests {
		if got := tt.typ.InContext(); got != tt.inContext {
			t.Errorf("EntryType(%q).InContext() = %v, want %v", tt.typ, got, tt.inContext)
		}
	}
}

func TestNewHeader(t *testing.T) {
	h := NewHeader("/tmp/test", nil, nil)
	if h.Type != EntryTypeSession {
		t.Errorf("Type = %v, want %v", h.Type, EntryTypeSession)
	}
	if h.Version != FormatVersion {
		t.Errorf("Version = %v, want %v", h.Version, FormatVersion)
	}
	if h.ID == "" {
		t.Error("ID should not be empty")
	}
	if h.CWD != "/tmp/test" {
		t.Errorf("CWD = %v, want /tmp/test", h.CWD)
	}
	if h.ParentSession != nil {
		t.Error("ParentSession should be nil")
	}
}

func TestMarshalParseEntry(t *testing.T) {
	parentID := "parent-123"
	msg := map[string]string{"role": "user", "content": "hello"}
	msgData, _ := json.Marshal(msg)

	entry := NewMessageEntry(&parentID, msgData, &TokenCounts{Input: 10, Output: 5})

	data, err := MarshalEntry(entry)
	if err != nil {
		t.Fatalf("MarshalEntry: %v", err)
	}

	parsed, err := ParseEntry(data)
	if err != nil {
		t.Fatalf("ParseEntry: %v", err)
	}

	if parsed.Type != EntryTypeMessage {
		t.Errorf("Type = %v, want %v", parsed.Type, EntryTypeMessage)
	}
	if parsed.ParentID == nil || *parsed.ParentID != parentID {
		t.Errorf("ParentID = %v, want %v", parsed.ParentID, parentID)
	}
	if parsed.Tokens == nil || parsed.Tokens.Input != 10 {
		t.Errorf("Tokens.Input = %v, want 10", parsed.Tokens)
	}
}

func TestParseHeader(t *testing.T) {
	h := NewHeader("/home/user/project", nil, nil)
	data, err := MarshalEntry(h)
	if err != nil {
		t.Fatalf("MarshalEntry: %v", err)
	}

	parsed, err := ParseHeader(data)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if parsed.ID != h.ID {
		t.Errorf("ID = %v, want %v", parsed.ID, h.ID)
	}
	if parsed.Version != FormatVersion {
		t.Errorf("Version = %v, want %v", parsed.Version, FormatVersion)
	}

	// Non-header should fail
	entry := NewEntry(EntryTypeMessage, nil)
	entryData, _ := MarshalEntry(entry)
	_, err = ParseHeader(entryData)
	if err == nil {
		t.Error("ParseHeader should fail for non-session type")
	}
}

func TestSlugFromName(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"Hello World", "hello-world"},
		{"My Test Session #1", "my-test-session-1"},
		{"  spaces  ", "spaces"},
		{"ABC", "abc"},
		{"", "untitled"},
		{"a!@#$%b", "ab"},
		{"Investigating Flaky Test (v2)", "investigating-flaky-test-v2"},
	}
	for _, tt := range tests {
		got := SlugFromName(tt.input)
		if got != tt.want {
			t.Errorf("SlugFromName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSessionFilenameFormat(t *testing.T) {
	dir := t.TempDir()

	// Test 1: Session with display name should produce <date>-<id>-<slug>.jsonl
	namedHeader := Header{
		ID:        "018f8c54-abcd-7890-1234-56789abcdef0",
		Name:      "My Test Session",
		Timestamp: "2026-05-08T12:00:00Z",
		CWD:       "/test",
	}
	w, err := CreateSessionFile(dir, namedHeader)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}
	defer w.Close()

	filename := filepath.Base(w.Path())

	// Filename should match: <date>-<id-prefix>[-<slug>].jsonl
	// With name: 2026-05-08-018f8c54-my-test-session.jsonl
	if !strings.HasPrefix(filename, "2026-05-08-018f8c54-my-test-session") {
		t.Errorf("filename = %q, want prefix 2026-05-08-018f8c54-my-test-session", filename)
	}
	if !strings.HasSuffix(filename, ".jsonl") {
		t.Errorf("filename = %q, want suffix .jsonl", filename)
	}

	// Test 2: Session without display name should produce <date>-<id>.jsonl
	unnamedHeader := Header{
		ID:        "018f8c54-wxyz-7890-1234-56789abcdef0",
		Timestamp: "2026-05-08T12:00:00Z",
		CWD:       "/test",
	}
	w2, err := CreateSessionFile(dir, unnamedHeader)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}
	defer w2.Close()

	filename2 := filepath.Base(w2.Path())

	// Filename should match: <date>-<id-prefix>.jsonl (no slug)
	if !strings.HasPrefix(filename2, "2026-05-08-018f8c54") {
		t.Errorf("filename = %q, want prefix 2026-05-08-018f8c54", filename2)
	}
	if !strings.HasSuffix(filename2, ".jsonl") {
		t.Errorf("filename = %q, want suffix .jsonl", filename2)
	}
}

// TestSlugFromHeader tests the slugFromHeader helper.
func TestSlugFromHeader(t *testing.T) {
	tests := []struct {
		name   string
		header Header
		want   string
	}{
		{
			name:   "display name set",
			header: Header{ID: "018f8c54abcd1234567890abcdef", Name: "My Test Session", Timestamp: "2026-05-01T12:00:00Z"},
			want:   "my-test-session",
		},
		{
			name:   "display name with special chars",
			header: Header{ID: "018f8c54abcd1234567890abcdef", Name: "Hello! World @2024", Timestamp: "2026-05-01T12:00:00Z"},
			want:   "hello-world-2024",
		},
		{
			name:   "no display name returns empty slug",
			header: Header{ID: "018f8c54abcd1234567890abcdef", Name: "", Timestamp: "2026-05-01T12:00:00Z"},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a session file to test slugFromHeader indirectly
			dir := t.TempDir()
			w, err := CreateSessionFile(dir, tt.header)
			if err != nil {
				t.Fatalf("CreateSessionFile: %v", err)
			}
			defer w.Close()

			// Extract the slug from the filename
			filename := filepath.Base(w.Path())
			filename = strings.TrimSuffix(filename, ".jsonl")
			// Filename format: <YYYY-MM-DD>-<id-prefix>[-<slug>].jsonl
			// For empty slug: 2026-05-01-018f8c54.jsonl
			// For named: 2026-05-01-018f8c54-my-test-session.jsonl
			parts := strings.Split(filename, "-")
			var got string
			if len(parts) == 4 {
				// No slug: date, month, day, uuid (4 parts)
				got = ""
			} else if len(parts) >= 5 {
				// Has slug: date, month, day, uuid, slug...
				got = strings.Join(parts[4:], "-")
			} else {
				t.Fatalf("unexpected filename format: %s", filename)
			}

			if got != tt.want {
				t.Errorf("slug = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadFileCopiesScannerLinesAndSkipsTruncatedTrailingEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "truncated.jsonl")
	header := NewHeader("/test/dir", nil, nil)
	headerData, err := MarshalEntry(header)
	if err != nil {
		t.Fatalf("MarshalEntry header: %v", err)
	}
	entry := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"hello"}`), nil)
	entryData, err := MarshalEntry(entry)
	if err != nil {
		t.Fatalf("MarshalEntry entry: %v", err)
	}
	content := string(headerData) + "\n" + string(entryData) + "\n" + `{"type":"message"`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile should tolerate malformed trailing entry: %v", err)
	}
	if r.Header().ID != header.ID {
		t.Fatalf("Header ID = %q, want %q", r.Header().ID, header.ID)
	}
	if got := r.MessageCount(); got != 1 {
		t.Fatalf("MessageCount = %d, want 1", got)
	}
}

func TestWriterReaderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test/dir", nil, nil)

	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}

	// Append some entries
	parentID := ""
	msg1 := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"hello"}`), &TokenCounts{Input: 5})
	if err := w.Append(msg1); err != nil {
		t.Fatalf("Append msg1: %v", err)
	}
	parentID = msg1.ID

	msg2 := NewMessageEntry(&parentID, json.RawMessage(`{"role":"assistant","content":"hi there"}`), &TokenCounts{Input: 5, Output: 3})
	if err := w.Append(msg2); err != nil {
		t.Fatalf("Append msg2: %v", err)
	}

	info := NewSessionInfoEntry("Test Session")
	if err := w.Append(info); err != nil {
		t.Fatalf("Append info: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Read it back
	r, err := ReadFile(w.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if r.Header().ID != header.ID {
		t.Errorf("Header ID = %v, want %v", r.Header().ID, header.ID)
	}
	if len(r.Entries()) != 3 {
		t.Fatalf("Entries count = %d, want 3", len(r.Entries()))
	}

	if r.DisplayName() != "Test Session" {
		t.Errorf("DisplayName = %q, want %q", r.DisplayName(), "Test Session")
	}

	if r.MessageCount() != 2 {
		t.Errorf("MessageCount = %d, want 2", r.MessageCount())
	}

	tokens := r.TotalTokens()
	if tokens.Input != 10 {
		t.Errorf("TotalTokens.Input = %d, want 10", tokens.Input)
	}
	if tokens.Output != 3 {
		t.Errorf("TotalTokens.Output = %d, want 3", tokens.Output)
	}
}

func TestTreeModelAndPosition(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, _ := CreateSessionFile(dir, header)

	// Create a branching tree:
	// msg1 -> msg2 -> msg3 (branch A)
	//              -> msg4 (branch B, backtrack to msg2)
	msg1 := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"1"}`), nil)
	w.Append(msg1)

	msg2 := NewMessageEntry(&msg1.ID, json.RawMessage(`{"role":"assistant","content":"2"}`), nil)
	w.Append(msg2)

	msg3 := NewMessageEntry(&msg2.ID, json.RawMessage(`{"role":"user","content":"3"}`), nil)
	w.Append(msg3)

	// Position change: navigate back to msg2
	posChange := NewPositionChangeEntry(msg2.ID)
	w.Append(posChange)

	// Branch B from msg2
	msg4 := NewMessageEntry(&msg2.ID, json.RawMessage(`{"role":"user","content":"4"}`), nil)
	w.Append(msg4)

	w.Close()

	r, _ := ReadFile(w.Path())

	// Current position should be msg4 (last message after position_change)
	pos := r.CurrentPosition()
	if pos != msg4.ID {
		t.Errorf("CurrentPosition = %v, want %v", pos, msg4.ID)
	}

	// Active branch should be: msg1 -> msg2 -> msg4
	branch := r.ActiveBranch()
	if len(branch) != 3 {
		t.Fatalf("ActiveBranch length = %d, want 3", len(branch))
	}
	if branch[0] != msg1.ID {
		t.Errorf("branch[0] = %v, want %v", branch[0], msg1.ID)
	}
	if branch[1] != msg2.ID {
		t.Errorf("branch[1] = %v, want %v", branch[1], msg2.ID)
	}
	if branch[2] != msg4.ID {
		t.Errorf("branch[2] = %v, want %v", branch[2], msg4.ID)
	}
}

func TestCompactionContext(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, _ := CreateSessionFile(dir, header)

	msg1 := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"1"}`), nil)
	w.Append(msg1)

	msg2 := NewMessageEntry(&msg1.ID, json.RawMessage(`{"role":"assistant","content":"2"}`), nil)
	w.Append(msg2)

	msg3 := NewMessageEntry(&msg2.ID, json.RawMessage(`{"role":"user","content":"3"}`), nil)
	w.Append(msg3)

	// Compact: skip msg1 and msg2, keep msg3
	compaction := NewCompactionEntry("Summary of 1-2", msg3.ID, 100, 20, false)
	w.Append(compaction)

	msg4 := NewMessageEntry(&msg3.ID, json.RawMessage(`{"role":"assistant","content":"4"}`), nil)
	w.Append(msg4)

	w.Close()

	r, _ := ReadFile(w.Path())
	ctx := r.ContextEntries()

	// Should have: compaction summary, msg3, msg4
	if len(ctx) != 3 {
		t.Fatalf("ContextEntries count = %d, want 3", len(ctx))
	}
	if ctx[0].Type != EntryTypeCompaction {
		t.Errorf("ctx[0].Type = %v, want compaction", ctx[0].Type)
	}
	if ctx[1].ID != msg3.ID {
		t.Errorf("ctx[1].ID = %v, want msg3", ctx[1].ID)
	}
	if ctx[2].ID != msg4.ID {
		t.Errorf("ctx[2].ID = %v, want msg4", ctx[2].ID)
	}
}

// A fallback compaction is not a real truncation pointer: retrying /compact
// must see the messages the failed attempt summarized, so ContextEntriesUnwound
// skips fallback compactions and restores the pre-fallback boundary.
func TestContextEntriesUnwoundSkipsFallbackCompactions(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, _ := CreateSessionFile(dir, header)

	msg1 := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"1"}`), nil)
	w.Append(msg1)
	msg2 := NewMessageEntry(&msg1.ID, json.RawMessage(`{"role":"assistant","content":"2"}`), nil)
	w.Append(msg2)

	// Successful compaction: boundary at msg2.
	sc := NewCompactionEntry("good summary", msg2.ID, 100, 20, false)
	w.Append(sc)

	msg3 := NewMessageEntry(&msg2.ID, json.RawMessage(`{"role":"user","content":"3"}`), nil)
	w.Append(msg3)

	// Fallback compaction on top: boundary at msg3.
	fc := NewCompactionEntry("naive fallback", msg3.ID, 90, 10, false)
	fc.Fallback = true
	w.Append(fc)

	w.Close()

	r, _ := ReadFile(w.Path())

	// Normal context: the fallback compaction is the truncation pointer.
	ctx := r.ContextEntries()
	if len(ctx) == 0 || ctx[0].ID != fc.ID {
		t.Fatalf("ContextEntries[0] = %+v, want the fallback compaction", ctx)
	}

	// Unwound: the fallback is ignored; the good compaction bounds context.
	unwound := r.ContextEntriesUnwound()
	if len(unwound) == 0 || unwound[0].ID != sc.ID {
		t.Fatalf("ContextEntriesUnwound[0] = %+v, want the successful compaction", unwound)
	}
	found := map[string]bool{}
	for _, e := range unwound {
		found[e.ID] = true
	}
	if !found[msg2.ID] || !found[msg3.ID] {
		t.Error("unwound context missing the entries the fallback compaction replaced")
	}
	if found[fc.ID] {
		t.Error("unwound context must not include the fallback compaction summary")
	}
}

// AllMessages is the full-history view: every message on the active branch in
// order, compaction summaries in place, ignoring compaction truncation. UIs
// seed a full transcript from it; the agent context must still use
// ToAgentMessages, which starts at the most recent compaction.
func TestAllMessagesIgnoresCompactionTruncation(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, _ := CreateSessionFile(dir, header)

	msg1 := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"1"}`), nil)
	w.Append(msg1)
	msg2 := NewMessageEntry(&msg1.ID, json.RawMessage(`{"role":"assistant","content":"2"}`), nil)
	w.Append(msg2)
	msg3 := NewMessageEntry(&msg2.ID, json.RawMessage(`{"role":"user","content":"3"}`), nil)
	w.Append(msg3)

	compaction := NewCompactionEntry("Summary of 1-2", msg3.ID, 100, 20, false)
	w.Append(compaction)

	msg4 := NewMessageEntry(&msg3.ID, json.RawMessage(`{"role":"assistant","content":"4"}`), nil)
	w.Append(msg4)

	w.Close()

	r, _ := ReadFile(w.Path())

	if got := len(r.ToAgentMessages()); got != 3 {
		t.Fatalf("ToAgentMessages count = %d, want 3 (summary + post-compaction)", got)
	}

	all := r.AllMessages()
	var roles, texts []string
	for _, m := range all {
		roles = append(roles, string(m.Role))
		for _, block := range m.Content {
			if block.Type == ai.ContentBlockText {
				texts = append(texts, block.Text)
			}
		}
	}
	if len(all) != 5 {
		t.Fatalf("AllMessages count = %d, want 5 (all four messages + the summary in place)", len(all))
	}
	wantTexts := []string{"1", "2", "Session Summary: Summary of 1-2", "3", "4"}
	if strings.Join(texts, "|") != strings.Join(wantTexts, "|") {
		t.Errorf("AllMessages texts = %q, want %q", texts, wantTexts)
	}
	if roles[2] != string(MessageRoleSystem) {
		t.Errorf("summary in place role = %q, want system", roles[2])
	}
}

func TestReaderTree(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, _ := CreateSessionFile(dir, header)

	// Create a tree:
	// msg1 -> msg2 -> msg3
	//              -> msg4 (branch B, backtrack to msg2)
	msg1 := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"1"}`), nil)
	w.Append(msg1)

	msg2 := NewMessageEntry(&msg1.ID, json.RawMessage(`{"role":"assistant","content":"2"}`), nil)
	w.Append(msg2)

	msg3 := NewMessageEntry(&msg2.ID, json.RawMessage(`{"role":"user","content":"3"}`), nil)
	w.Append(msg3)

	// Position change: navigate back to msg2
	posChange := NewPositionChangeEntry(msg2.ID)
	w.Append(posChange)

	// Branch B from msg2
	msg4 := NewMessageEntry(&msg2.ID, json.RawMessage(`{"role":"user","content":"4"}`), nil)
	w.Append(msg4)

	w.Close()

	r, _ := ReadFile(w.Path())
	tree := r.Tree()

	// Should have 2 roots: msg1 (entry with nil parent) and msg4 (orphaned due to position_change)
	if len(tree) != 2 {
		t.Errorf("Tree roots count = %d, want 2", len(tree))
	}

	// Verify children are sorted by timestamp
	// Find msg2's children
	msg2Children := r.GetChildren(msg2.ID)
	if len(msg2Children) != 2 {
		t.Errorf("msg2 children count = %d, want 2", len(msg2Children))
	}

	// Verify GetChildren returns nil for non-existent entry
	nonExistent := r.GetChildren("nonexistent-id")
	if nonExistent != nil {
		t.Errorf("GetChildren for nonexistent = %v, want nil", nonExistent)
	}
}

func TestReaderTreeWithLabels(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, _ := CreateSessionFile(dir, header)

	msg1 := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"1"}`), nil)
	w.Append(msg1)

	msg2 := NewMessageEntry(&msg1.ID, json.RawMessage(`{"role":"assistant","content":"2"}`), nil)
	w.Append(msg2)

	// Add labels - labels are children of the leaf at time of creation
	// Since msg2 is the current leaf when we create label1, label1's parentID is msg2.ID
	label1 := NewLabelEntry(msg1.ID, "first message")
	w.Append(label1)

	label2 := NewLabelEntry(msg2.ID, "second message")
	w.Append(label2)

	// Update label on msg1 - now msg2 is still the leaf, so label1Update's parentID is msg2.ID
	label1Update := NewLabelEntry(msg1.ID, "updated first")
	w.Append(label1Update)

	w.Close()

	r, _ := ReadFile(w.Path())
	tree := r.Tree()

	// Tree structure:
	// msg1 (root, labeled with "updated first")
	//   └── msg2 (labeled with "second message")
	//         ├── label1 (has label "first message" target)
	//         ├── label2
	//         └── label1Update

	// Find msg1 node (should be in tree)
	var msg1Node *TreeNode
	for _, root := range tree {
		if root.Entry.ID == msg1.ID {
			msg1Node = root
			break
		}
	}
	if msg1Node == nil {
		t.Fatal("msg1 node not found in tree")
	}

	// Verify latest label for msg1 is "updated first"
	if msg1Node.Label != "updated first" {
		t.Errorf("msg1 label = %q, want %q", msg1Node.Label, "updated first")
	}
	if msg1Node.LabelTimestamp == "" {
		t.Error("msg1 label timestamp should not be empty")
	}

	// Find msg2 node - it's a child of msg1
	var msg2Node *TreeNode
	for _, child := range msg1Node.Children {
		if child.Entry.ID == msg2.ID {
			msg2Node = child
			break
		}
	}
	if msg2Node == nil {
		t.Fatal("msg2 node not found in tree")
	}

	if msg2Node.Label != "second message" {
		t.Errorf("msg2 label = %q, want %q", msg2Node.Label, "second message")
	}
}

func TestStorageCRUD(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStorageAt(dir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}

	// Create
	header := NewHeader("/test/project", nil, nil)
	w, err := s.Create(header)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	msg := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"test"}`), &TokenCounts{Input: 10})
	w.Append(msg)
	w.Close()

	sessionID := header.ID[:8]

	// List
	summaries, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("List count = %d, want 1", len(summaries))
	}
	if summaries[0].ID != header.ID {
		t.Errorf("Summary ID = %v, want %v", summaries[0].ID, header.ID)
	}

	// Open
	r, err := s.Open(sessionID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if r.Header().ID != header.ID {
		t.Errorf("Header ID mismatch")
	}

	// Rename
	err = s.Rename(sessionID, "My Test Session")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}

	r, err = s.Open(sessionID)
	if err != nil {
		t.Fatalf("Open after rename: %v", err)
	}
	if r.DisplayName() != "My Test Session" {
		t.Errorf("DisplayName = %q, want %q", r.DisplayName(), "My Test Session")
	}

	// Delete (moves to trash)
	err = s.Delete(sessionID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Should not be listable
	summaries, _ = s.List()
	if len(summaries) != 0 {
		t.Errorf("After delete, List count = %d, want 0", len(summaries))
	}

	// Restore
	err = s.Restore(sessionID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	summaries, _ = s.List()
	if len(summaries) != 1 {
		t.Errorf("After restore, List count = %d, want 1", len(summaries))
	}
}

func TestStorageFork(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorageAt(dir)

	// Create parent
	parentHeader := NewHeader("/test", nil, nil)
	w, _ := s.Create(parentHeader)
	msg := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"parent msg"}`), nil)
	w.Append(msg)
	w.Close()

	// Fork
	forkW, err := s.Fork(parentHeader.ID[:8], nil)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	forkMsg := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"fork msg"}`), nil)
	forkW.Append(forkMsg)
	forkW.Close()

	// Verify fork references parent
	summaries, _ := s.List()
	var forkSummary *SessionSummary
	for i := range summaries {
		if summaries[i].ParentID != nil && *summaries[i].ParentID == parentHeader.ID {
			forkSummary = &summaries[i]
			break
		}
	}
	if forkSummary == nil {
		t.Fatal("Fork session not found in list")
	}
}

func TestExportJSON(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorageAt(dir)

	header := NewHeader("/test", nil, nil)
	w, _ := s.Create(header)
	msg := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"hello"}`), nil)
	w.Append(msg)
	w.Close()

	data, err := s.ExportJSON(header.ID[:8])
	if err != nil {
		t.Fatalf("ExportJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if _, ok := doc["session"]; !ok {
		t.Error("JSON export missing 'session' key")
	}
	if _, ok := doc["tree"]; !ok {
		t.Error("JSON export missing 'tree' key")
	}
	if _, ok := doc["active_branch"]; !ok {
		t.Error("JSON export missing 'active_branch' key")
	}

	sessionDoc, ok := doc["session"].(map[string]any)
	if !ok {
		t.Fatalf("session type = %T, want object", doc["session"])
	}
	if sessionDoc["id"] != header.ID {
		t.Fatalf("session.id = %v, want %v", sessionDoc["id"], header.ID)
	}
	if sessionDoc["cwd"] != "/test" {
		t.Fatalf("session.cwd = %v, want /test", sessionDoc["cwd"])
	}
	if _, ok := sessionDoc["created"].(string); !ok {
		t.Fatalf("session.created = %T, want string", sessionDoc["created"])
	}
	if _, ok := sessionDoc["tokens"].(map[string]any); !ok {
		t.Fatalf("session.tokens = %T, want object", sessionDoc["tokens"])
	}

	tree, ok := doc["tree"].([]any)
	if !ok {
		t.Fatalf("tree type = %T, want array", doc["tree"])
	}
	if len(tree) != 1 {
		t.Fatalf("tree length = %d, want 1", len(tree))
	}

	entry, ok := tree[0].(map[string]any)
	if !ok {
		t.Fatalf("tree[0] type = %T, want object", tree[0])
	}
	if entry["type"] != string(EntryTypeMessage) {
		t.Fatalf("tree[0].type = %v, want %s", entry["type"], EntryTypeMessage)
	}
	if _, ok := entry["id"].(string); !ok {
		t.Fatalf("tree[0].id = %T, want string", entry["id"])
	}
	if _, ok := entry["timestamp"].(string); !ok {
		t.Fatalf("tree[0].timestamp = %T, want string", entry["timestamp"])
	}

	message, ok := entry["message"].(map[string]any)
	if !ok {
		t.Fatalf("tree[0].message = %T, want object", entry["message"])
	}
	if message["role"] != "user" {
		t.Fatalf("message.role = %v, want user", message["role"])
	}
	if message["content"] != "hello" {
		t.Fatalf("message.content = %v, want hello", message["content"])
	}

	activeBranch, ok := doc["active_branch"].([]any)
	if !ok {
		t.Fatalf("active_branch type = %T, want array", doc["active_branch"])
	}
	if len(activeBranch) != 1 {
		t.Fatalf("active_branch length = %d, want 1", len(activeBranch))
	}
	if activeBranch[0] != msg.ID {
		t.Fatalf("active_branch[0] = %v, want %v", activeBranch[0], msg.ID)
	}
}

func TestExportMarkdown(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorageAt(dir)

	header := NewHeader("/test/mydir", strPtr("my-session"), nil)
	w, _ := s.Create(header)
	msg := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"hello world"}`), nil)
	w.Append(msg)
	w.Close()

	data, err := s.ExportMarkdown(header.ID[:8])
	if err != nil {
		t.Fatalf("ExportMarkdown: %v", err)
	}

	md := string(data)
	if len(md) == 0 {
		t.Error("Markdown export is empty")
	}

	// Markdown must start with an H1 session header.
	if !strings.Contains(md, "# Session:") {
		t.Error("Markdown export missing '# Session:' header")
	}

	// Must contain the session name or ID in the header.
	sessionIdentifier := header.ID[:8]
	if !strings.Contains(md, sessionIdentifier) {
		t.Errorf("Markdown export missing session ID %q", sessionIdentifier)
	}

	// Must contain the CWD from the session header.
	if !strings.Contains(md, "/test/mydir") {
		t.Errorf("Markdown export missing session CWD")
	}

	// Must contain the message content.
	if !strings.Contains(md, "hello world") {
		t.Errorf("Markdown export missing message content 'hello world'")
	}

	// Must contain role label (Title-cased per spec rendering).
	if !strings.Contains(md, "User") {
		t.Errorf("Markdown export missing role label 'User'")
	}
}

// TestExportMarkdownActiveBranch verifies that Markdown export renders only
// the active branch entries, not abandoned branches.
func TestExportMarkdownActiveBranch(t *testing.T) {
	// Build a session with two branches:
	//   A (user: "first") --> B (user: "on-branch") --> C (compaction) --> D (model change) --> E (session info)
	//                         └--> abandoned_summary ("abandoned text")
	//
	// The abandoned branch summary has B as its parent but C continues the
	// active path from B, so the abandoned summary is NOT on the active branch.
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}

	msgEntry := func(parent *string, content string) (Entry, string) {
		t.Helper()
		msg := map[string]string{"role": "user", "content": content}
		data, _ := json.Marshal(msg)
		e := NewMessageEntry(parent, data, nil)
		return e, e.ID
	}

	// Entry A: root message
	a, aID := msgEntry(nil, "first")
	if err := w.Append(a); err != nil {
		t.Fatalf("Append A: %v", err)
	}

	// Entry B: continues active path
	b, bID := msgEntry(&aID, "on-branch")
	if err := w.Append(b); err != nil {
		t.Fatalf("Append B: %v", err)
	}

	// Entry abandoned_summary: parent is B, not continued from here.
	abandonedSummary := Entry{
		Type:     EntryTypeBranchSummary,
		ParentID: &bID,
		ID:       "abandoned-summary",
		FromID:   &aID,
		Summary:  strPtr("this branch went somewhere else"),
	}
	if err := w.Append(abandonedSummary); err != nil {
		t.Fatalf("Append abandoned summary: %v", err)
	}

	// Entry C: compaction on the active branch.
	cID := "compaction-1"
	compaction := Entry{
		Type:         EntryTypeCompaction,
		ParentID:     &bID,
		ID:           cID,
		Summary:      strPtr("summarized some tokens"),
		TokensBefore: new(1000),
		TokensAfter:  new(200),
	}
	if err := w.Append(compaction); err != nil {
		t.Fatalf("Append compaction: %v", err)
	}

	// Entry D: model change on the active branch.
	dID := "model-change-1"
	modelChange := Entry{
		Type:     EntryTypeModelChange,
		ParentID: &cID,
		ID:       dID,
		Provider: strPtr("anthropic"),
		Model:    strPtr("claude-sonnet-4-6"),
	}
	if err := w.Append(modelChange); err != nil {
		t.Fatalf("Append model change: %v", err)
	}

	// Entry E: session info rename on the active branch.
	eID := "session-info-1"
	renameEntry := Entry{
		Type:     EntryTypeSessionInfo,
		ParentID: &dID,
		ID:       eID,
		Name:     strPtr("my-awesome-session"),
	}
	if err := w.Append(renameEntry); err != nil {
		t.Fatalf("Append session info: %v", err)
	}

	// Position change at the end signals that E is the current position.
	posChange := Entry{
		Type:        EntryTypePositionChange,
		ParentID:    &eID,
		ID:          "position-final",
		NewPosition: &eID,
	}
	if err := w.Append(posChange); err != nil {
		t.Fatalf("Append position_change: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, _ := NewStorageAt(dir)
	data, err := s.ExportMarkdown(header.ID[:8])
	if err != nil {
		t.Fatalf("ExportMarkdown: %v", err)
	}
	md := string(data)

	// Active branch content must be present.
	if !strings.Contains(md, "first") {
		t.Error("Markdown export missing active branch entry 'first'")
	}
	if !strings.Contains(md, "on-branch") {
		t.Error("Markdown export missing active branch entry 'on-branch'")
	}

	// Compaction on active branch must be present.
	if !strings.Contains(md, "summarized some tokens") {
		t.Error("Markdown export missing compaction summary from active branch")
	}

	// Model change on active branch must be present.
	if !strings.Contains(md, "claude-sonnet-4-6") {
		t.Error("Markdown export missing model change from active branch")
	}

	// Session rename on active branch must be present.
	if !strings.Contains(md, "my-awesome-session") {
		t.Error("Markdown export missing session rename from active branch")
	}

	// Abandoned branch summary must be absent.
	if strings.Contains(md, "this branch went somewhere else") {
		t.Error("Markdown export contains abandoned branch summary (should be omitted)")
	}
}

func TestExportHTML(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorageAt(dir)

	header := NewHeader("/test", nil, nil)
	w, _ := s.Create(header)
	msg := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"hello"}`), nil)
	w.Append(msg)
	w.Close()

	data, err := s.ExportHTML(header.ID[:8])
	if err != nil {
		t.Fatalf("ExportHTML: %v", err)
	}

	html := string(data)
	if len(html) == 0 {
		t.Error("HTML export is empty")
	}

	// HTML must be well-formed (starts with DOCTYPE/HTML tag).
	if !strings.Contains(html, "<!DOCTYPE html>") && !strings.HasPrefix(strings.TrimSpace(html), "<html") {
		t.Error("HTML export missing <html> document start")
	}

	// Must contain the session name or ID in the title/heading.
	sessionIdentifier := header.ID[:8]
	if !strings.Contains(html, sessionIdentifier) {
		t.Errorf("HTML export missing session ID %q", sessionIdentifier)
	}

	// Must contain the CWD from the session header.
	if !strings.Contains(html, "/test") {
		t.Errorf("HTML export missing session CWD /test")
	}

	// Must contain the message content.
	if !strings.Contains(html, "hello") {
		t.Errorf("HTML export missing message content 'hello'")
	}

	// Must contain role label.
	if !strings.Contains(html, "user") {
		t.Errorf("HTML export missing role label 'user'")
	}
}

// TestExportHTMLActiveBranch verifies that HTML export renders only the active
// branch entries, not abandoned branches. A compaction entry (on the active
// branch) must be present; a branch_summary not on the active branch must be
// absent.
func TestExportHTMLActiveBranch(t *testing.T) {
	// Build a session with two branches:
	//   A (user: "first") --> B (user: "on-branch") --> C (compaction) --> D (model change) --> E (session info)
	//                         └--> abandoned_summary ("abandoned text")
	//
	// The abandoned branch summary has B as its parent but C continues the
	// active path from B, so the abandoned summary is NOT on the active branch.
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}

	// Helper to create a message entry and capture its ID.
	msgEntry := func(parent *string, content string) (Entry, string) {
		t.Helper()
		msg := map[string]string{"role": "user", "content": content}
		data, _ := json.Marshal(msg)
		e := NewMessageEntry(parent, data, nil)
		return e, e.ID
	}

	// Entry A: root message (no parent)
	a, aID := msgEntry(nil, "first")
	if err := w.Append(a); err != nil {
		t.Fatalf("Append A: %v", err)
	}

	// Entry B: continues active path
	b, bID := msgEntry(&aID, "on-branch")
	if err := w.Append(b); err != nil {
		t.Fatalf("Append B: %v", err)
	}

	// Entry abandoned_summary: parent is B, but we DON'T continue from this
	// entry. This makes it an abandoned branch — not on the active chain.
	abandonedSummary := Entry{
		Type:     EntryTypeBranchSummary,
		ParentID: &bID,
		ID:       "abandoned-summary",
		FromID:   &aID,
		Summary:  strPtr("this branch went somewhere else"),
	}
	if err := w.Append(abandonedSummary); err != nil {
		t.Fatalf("Append abandoned summary: %v", err)
	}

	// Entry C: compaction on the active branch, continues from B.
	cID := "compaction-1"
	compaction := Entry{
		Type:         EntryTypeCompaction,
		ParentID:     &bID,
		ID:           cID,
		Summary:      strPtr("summarized some tokens"),
		TokensBefore: new(1000),
		TokensAfter:  new(200),
	}
	if err := w.Append(compaction); err != nil {
		t.Fatalf("Append compaction: %v", err)
	}

	// Entry D: model change on the active branch.
	dID := "model-change-1"
	modelChange := Entry{
		Type:     EntryTypeModelChange,
		ParentID: &cID,
		ID:       dID,
		Provider: strPtr("anthropic"),
		Model:    strPtr("claude-sonnet-4-6"),
	}
	if err := w.Append(modelChange); err != nil {
		t.Fatalf("Append model change: %v", err)
	}

	// Entry E: session info rename on the active branch.
	eID := "session-info-1"
	renameEntry := Entry{
		Type:     EntryTypeSessionInfo,
		ParentID: &dID,
		ID:       eID,
		Name:     strPtr("my-awesome-session"),
	}
	if err := w.Append(renameEntry); err != nil {
		t.Fatalf("Append session info: %v", err)
	}

	// Position change at the end signals that E is the current position (active tip).
	posChange := Entry{
		Type:        EntryTypePositionChange,
		ParentID:    &eID,
		ID:          "position-final",
		NewPosition: &eID,
	}
	if err := w.Append(posChange); err != nil {
		t.Fatalf("Append position_change: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, _ := NewStorageAt(dir)

	r, err := s.Open(header.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	active := r.ActiveBranch()
	t.Logf("active branch IDs (%d): %v", len(active), active)

	data, err := s.ExportHTML(header.ID[:8])
	if err != nil {
		t.Fatalf("ExportHTML: %v", err)
	}
	html := string(data)

	// Active branch content must be present.
	if !strings.Contains(html, "first") {
		t.Error("HTML export missing active branch entry 'first'")
	}
	if !strings.Contains(html, "on-branch") {
		t.Error("HTML export missing active branch entry 'on-branch'")
	}

	// Compaction on active branch must be present.
	if !strings.Contains(html, "summarized some tokens") {
		t.Error("HTML export missing compaction summary from active branch")
	}

	// Model change on active branch must be present.
	if !strings.Contains(html, "claude-sonnet-4-6") {
		t.Error("HTML export missing model change from active branch")
	}

	// Session rename on active branch must be present.
	if !strings.Contains(html, "my-awesome-session") {
		t.Error("HTML export missing session rename from active branch")
	}

	// Abandoned branch summary check: the HTML export may include abandoned
	// branches in collapsed <details> sections (per spec: "collapsed by default"),
	// so we only assert that the abandoned summary text is NOT rendered inline
	// within the active branch message rendering. We do not require it to be
	// absent entirely — the collapsed section wrapper may wrap it.
	// We verify the active branch content is NOT mixed into the collapsed section.
	if strings.Contains(html, "this branch went somewhere else") {
		// If present, it must be inside a <details class="abandoned-branches"> block.
		if !strings.Contains(html, "<details class=\"abandoned-branches\">") {
			t.Error("abandoned branch text appears outside a collapsed section")
		}
	}

	// Verify the active branch messages are NOT inside the abandoned-branches details.
	// The active branch content appears directly in the document body, not in the
	// collapsed abandoned-branches section.
	abandonedIdx := strings.Index(html, "<details class=\"abandoned-branches\">")
	activeIdx := strings.Index(html, "first")
	if abandonedIdx != -1 && activeIdx != -1 && activeIdx > abandonedIdx {
		t.Error("active branch content appears after abandoned-branches section")
	}
}

func TestExportEntryTypesFixture(t *testing.T) {
	// This test creates a session containing one entry of every EntryType and
	// verifies that both HTML and Markdown exports render the expected subset
	// without panicking, and that excluded types are silently omitted.
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}

	msgID := newIDForTest()
	customMsgID := newIDForTest()
	msg := Entry{
		Type:      EntryTypeMessage,
		ID:        msgID,
		ParentID:  nil,
		Timestamp: NewTimestamp(),
		Message:   json.RawMessage(`{"role":"user","content":"hello"}`),
	}
	if err := w.Append(msg); err != nil {
		t.Fatalf("Append message: %v", err)
	}

	// Each entry's ParentID chains to the previous entry, building a single
	// linear path so that the active branch includes all entries in order.
	// The final NewPosition points to customMsgID so the whole chain is live.
	positionID := newIDForTest()
	position := Entry{
		Type:        EntryTypePositionChange,
		ID:          positionID,
		ParentID:    &msgID,
		Timestamp:   NewTimestamp(),
		NewPosition: &customMsgID, // will be updated after customMsg exists
	}
	if err := w.Append(position); err != nil {
		t.Fatalf("Append position_change: %v", err)
	}

	compactSummary := "summarized stuff"
	compactID := newIDForTest()
	compaction := Entry{
		Type:         EntryTypeCompaction,
		ID:           compactID,
		ParentID:     &positionID,
		Timestamp:    NewTimestamp(),
		Summary:      &compactSummary,
		TokensBefore: new(1000),
		TokensAfter:  new(200),
	}
	if err := w.Append(compaction); err != nil {
		t.Fatalf("Append compaction: %v", err)
	}

	branchFrom := msgID
	branchSummary := "we branched"
	branchID := newIDForTest()
	branch := Entry{
		Type:     EntryTypeBranchSummary,
		ID:       branchID,
		ParentID: &compactID,
		FromID:   &branchFrom,
		Summary:  &branchSummary,
	}
	if err := w.Append(branch); err != nil {
		t.Fatalf("Append branch_summary: %v", err)
	}

	sessionName := "renamed-session"
	sessionInfoID := newIDForTest()
	sessionInfo := Entry{
		Type:     EntryTypeSessionInfo,
		ID:       sessionInfoID,
		ParentID: &branchID,
		Name:     &sessionName,
	}
	if err := w.Append(sessionInfo); err != nil {
		t.Fatalf("Append session_info: %v", err)
	}

	provider := "anthropic"
	model := "claude-sonnet-4-5"
	modelChangeID := newIDForTest()
	modelChange := Entry{
		Type:     EntryTypeModelChange,
		ID:       modelChangeID,
		ParentID: &sessionInfoID,
		Provider: &provider,
		Model:    &model,
	}
	if err := w.Append(modelChange); err != nil {
		t.Fatalf("Append model_change: %v", err)
	}

	level := "high"
	thinkingID := newIDForTest()
	thinking := Entry{
		Type:     EntryTypeThinkingChange,
		ID:       thinkingID,
		ParentID: &modelChangeID,
		Level:    &level,
	}
	if err := w.Append(thinking); err != nil {
		t.Fatalf("Append thinking_level_change: %v", err)
	}

	customData, _ := json.Marshal(map[string]string{"key": "value"})
	customID := newIDForTest()
	custom := Entry{
		Type:       EntryTypeCustom,
		ID:         customID,
		ParentID:   &thinkingID,
		CustomType: strPtr("my-custom"),
		Data:       customData,
	}
	if err := w.Append(custom); err != nil {
		t.Fatalf("Append custom: %v", err)
	}

	customMsgContent := json.RawMessage(`{"role":"system","content":"custom prompt"}`)
	customMsg := Entry{
		Type:       EntryTypeCustomMessage,
		ID:         customMsgID,
		ParentID:   &customID,
		CustomType: strPtr("prompt"),
		Message:    customMsgContent,
	}
	if err := w.Append(customMsg); err != nil {
		t.Fatalf("Append custom_message: %v", err)
	}

	// Final position change: update the NewPosition written earlier so the
	// whole chain is live.  The position entry's ParentID still points to msgID.
	position.NewPosition = &customMsgID
	if err := w.Append(position); err != nil {
		t.Fatalf("Append position update: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, _ := NewStorageAt(dir)
	r, err := s.Open(header.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	active := r.ActiveBranch()
	t.Logf("active branch (%d): %v", len(active), active)

	// --- HTML export ---
	htmlData, err := s.ExportHTML(header.ID[:8])
	if err != nil {
		t.Fatalf("ExportHTML: %v", err)
	}
	html := string(htmlData)

	// Entries that must appear in HTML export.
	if !strings.Contains(html, "hello") {
		t.Error("HTML missing message content")
	}
	if !strings.Contains(html, "summarized stuff") {
		t.Error("HTML missing compaction summary")
	}
	if !strings.Contains(html, "renamed-session") {
		t.Error("HTML missing session rename")
	}
	if !strings.Contains(html, "claude-sonnet-4-5") {
		t.Error("HTML missing model change")
	}
	if !strings.Contains(html, "high") {
		t.Error("HTML missing thinking level")
	}
	if !strings.Contains(html, "custom prompt") {
		t.Error("HTML missing custom_message content")
	}

	// BranchSummary on the active branch is rendered inline in both formats.
	if !strings.Contains(html, "we branched") {
		t.Error("HTML missing branch_summary on active branch")
	}

	// Custom (non-message) entries are skipped entirely.
	if strings.Contains(html, "my-custom") {
		t.Error("HTML contains custom entry custom_type (should be skipped)")
	}

	// --- Markdown export ---
	mdData, err := s.ExportMarkdown(header.ID[:8])
	if err != nil {
		t.Fatalf("ExportMarkdown: %v", err)
	}
	md := string(mdData)

	// Entries that must appear in Markdown export.
	if !strings.Contains(md, "hello") {
		t.Error("Markdown missing message content")
	}
	if !strings.Contains(md, "summarized stuff") {
		t.Error("Markdown missing compaction summary")
	}
	if !strings.Contains(md, "renamed-session") {
		t.Error("Markdown missing session rename")
	}
	if !strings.Contains(md, "claude-sonnet-4-5") {
		t.Error("Markdown missing model change")
	}
	if !strings.Contains(md, "high") {
		t.Error("Markdown missing thinking level")
	}
	if !strings.Contains(md, "custom prompt") {
		t.Error("Markdown missing custom_message content")
	}
}

func TestOwnerFile(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "test.jsonl")

	// Write a minimal session file
	f, _ := os.Create(sessionPath)
	f.WriteString(`{"type":"session","version":1,"id":"test123","timestamp":"2026-05-01T00:00:00Z","cwd":"/test"}` + "\n")
	f.Close()

	// Claim owner
	if err := ClaimOwner(sessionPath); err != nil {
		t.Fatalf("ClaimOwner: %v", err)
	}

	// Owner file should exist
	if _, err := os.Stat(sessionPath + ".owner"); os.IsNotExist(err) {
		t.Error("Owner file should exist after ClaimOwner")
	}

	// Release owner
	if err := ReleaseOwner(sessionPath); err != nil {
		t.Fatalf("ReleaseOwner: %v", err)
	}

	// Owner file should be gone
	if _, err := os.Stat(sessionPath + ".owner"); !os.IsNotExist(err) {
		t.Error("Owner file should be gone after ReleaseOwner")
	}
}

func TestCleanupStaleOwners(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorageAt(dir)

	// Create a session file
	header := NewHeader("/test", nil, nil)
	w, _ := s.Create(header)
	w.Close()

	// Write a stale owner file with a PID that doesn't exist
	ownerPath := w.Path() + ".owner"
	os.WriteFile(ownerPath, []byte("99999999"), 0o644)

	// Cleanup should remove it
	s.CleanupStaleOwners()

	if _, err := os.Stat(ownerPath); !os.IsNotExist(err) {
		t.Error("Stale owner file should have been cleaned up")
	}
}

func TestFindByID(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, _ := CreateSessionFile(dir, header)

	msg := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"test"}`), nil)
	w.Append(msg)
	w.Close()

	r, _ := ReadFile(w.Path())

	found := r.FindByID(msg.ID)
	if found == nil {
		t.Fatal("FindByID returned nil")
	}
	if found.ID != msg.ID {
		t.Errorf("FindByID ID = %v, want %v", found.ID, msg.ID)
	}

	missing := r.FindByID("nonexistent")
	if missing != nil {
		t.Error("FindByID for nonexistent should return nil")
	}
}

func TestBranchedSessionCompaction(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, _ := CreateSessionFile(dir, header)

	// Create a tree:
	// msg1 -> msg2 -> msg3 (branch A)
	//              -> msg4 (branch B, backtrack to msg2)
	msg1 := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"1"}`), nil)
	w.Append(msg1)

	msg2 := NewMessageEntry(&msg1.ID, json.RawMessage(`{"role":"assistant","content":"2"}`), nil)
	w.Append(msg2)

	msg3 := NewMessageEntry(&msg2.ID, json.RawMessage(`{"role":"user","content":"3"}`), nil)
	w.Append(msg3)

	// Compact branch A: keep msg3 as the first kept entry
	compactionA := NewCompactionEntry("Summary of branch A", msg3.ID, 100, 20, false)
	w.Append(compactionA)

	msg4 := NewMessageEntry(&msg2.ID, json.RawMessage(`{"role":"user","content":"4 on branch B"}`), nil)
	w.Append(msg4)

	// Now navigate back to msg2 and continue branch B
	posChange := NewPositionChangeEntry(msg2.ID)
	w.Append(posChange)

	msg5 := NewMessageEntry(&msg2.ID, json.RawMessage(`{"role":"assistant","content":"5 on branch B"}`), nil)
	w.Append(msg5)

	w.Close()

	r, _ := ReadFile(w.Path())

	// Current position should be msg5 (last message after position_change)
	pos := r.CurrentPosition()
	if pos != msg5.ID {
		t.Errorf("CurrentPosition = %v, want %v", pos, msg5.ID)
	}

	// Active branch should be: msg1 -> msg2 -> msg5
	branch := r.ActiveBranch()
	if len(branch) != 3 {
		t.Fatalf("ActiveBranch length = %d, want 3", len(branch))
	}
	if branch[0] != msg1.ID {
		t.Errorf("branch[0] = %v, want %v", branch[0], msg1.ID)
	}
	if branch[1] != msg2.ID {
		t.Errorf("branch[1] = %v, want %v", branch[1], msg2.ID)
	}
	if branch[2] != msg5.ID {
		t.Errorf("branch[2] = %v, want %v", branch[2], msg5.ID)
	}

	// Context entries should NOT include compactionA (it's on branch A, not active branch)
	// Should have: msg1, msg2, msg5
	ctx := r.ContextEntries()
	// msg1 and msg2 should be in context (before any compaction on this branch)
	// msg5 should be in context
	foundMsg5 := false
	for _, e := range ctx {
		if e.ID == msg5.ID {
			foundMsg5 = true
		}
		if e.ID == compactionA.ID {
			t.Errorf("ContextEntries should not include compaction from abandoned branch")
		}
	}
	if !foundMsg5 {
		t.Errorf("ContextEntries should include msg5")
	}
}

func TestCustomMessageToAgentMessage(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, _ := CreateSessionFile(dir, header)

	// Create a custom_message entry with content
	cm := NewCustomMessageEntry(
		"my_plugin",
		json.RawMessage(`{"plugin_state":"initialized"}`),
		json.RawMessage(`{"role":"system","content":"You are helping with plugin integration."}`),
	)
	w.Append(cm)
	w.Close()

	r, _ := ReadFile(w.Path())

	// Convert to agent messages
	messages := r.ToAgentMessages()

	// Should have the custom_message as an agent message
	if len(messages) != 1 {
		t.Fatalf("ToAgentMessages count = %d, want 1", len(messages))
	}

	msg := messages[0]
	if msg.Role != MessageRoleSystem {
		t.Errorf("msg.Role = %v, want %v", msg.Role, MessageRoleSystem)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != ai.ContentBlockText {
		t.Errorf("msg.Content type mismatch")
	}
	if msg.Content[0].Text != "You are helping with plugin integration." {
		t.Errorf("msg.Content[0].Text = %q, want %q", msg.Content[0].Text, "You are helping with plugin integration.")
	}
}

func TestCustomEntries(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test", nil, nil)
	w, _ := CreateSessionFile(dir, header)

	c1 := NewCustomEntry("my_plugin", json.RawMessage(`{"state":"init"}`))
	w.Append(c1)

	cm := NewCustomMessageEntry("my_plugin", json.RawMessage(`{"ctx":"data"}`), json.RawMessage(`{"role":"system","content":"plugin msg"}`))
	w.Append(cm)

	c2 := NewCustomEntry("other_plugin", json.RawMessage(`{"state":"done"}`))
	w.Append(c2)

	w.Close()

	r, _ := ReadFile(w.Path())

	// All custom entries
	all := r.CustomEntries("")
	if len(all) != 3 {
		t.Errorf("CustomEntries('') = %d, want 3", len(all))
	}

	// Filtered by custom_type
	myPlugin := r.CustomEntries("my_plugin")
	if len(myPlugin) != 2 {
		t.Errorf("CustomEntries('my_plugin') = %d, want 2", len(myPlugin))
	}
}

// TestPluginSessionAppend tests that plugin session appends work correctly.
// This verifies the accessor interface used by the plugin Host RPCs.
func TestPluginSessionAppend(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorageAt(dir)

	header := NewHeader("/test", nil, nil)
	w, _ := s.Create(header)
	msg := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"original"}`), nil)
	w.Append(msg)
	w.Close()

	sessionID := header.ID[:8]

	// Open with accessor (used by plugins)
	accessor, err := s.Open(sessionID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Get current count
	count := accessor.MessageCount()
	if count != 1 {
		t.Errorf("MessageCount = %d, want 1", count)
	}

	// Resume for appending (like what the plugin Host RPC does)
	w, err = s.Resume(sessionID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// Append a custom entry (like plugin SessionAppend does)
	customMsg := NewCustomMessageEntry(
		"test-plugin",
		json.RawMessage(`{"tool":"my-tool"}`),
		json.RawMessage(`{"role":"user","content":"plugin append"}`),
	)
	if err := w.Append(customMsg); err != nil {
		t.Fatalf("Append custom message: %v", err)
	}
	w.Close()

	// Re-open and verify
	accessor, _ = s.Open(sessionID)
	if accessor.MessageCount() != 2 {
		t.Errorf("After append, MessageCount = %d, want 2", accessor.MessageCount())
	}
}

// TestSessionResumeAndFork tests resume and fork flows.
func TestSessionResumeAndFork(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorageAt(dir)

	// Create parent session
	parentHeader := NewHeader("/test", nil, nil)
	parentW, _ := s.Create(parentHeader)
	msg1 := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"first"}`), nil)
	parentW.Append(msg1)
	msg2 := NewMessageEntry(&msg1.ID, json.RawMessage(`{"role":"assistant","content":"second"}`), nil)
	parentW.Append(msg2)
	parentW.Close()

	sessionID := parentHeader.ID[:8]

	// Resume parent session
	resumeW, err := s.Resume(sessionID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	msg3 := NewMessageEntry(&msg2.ID, json.RawMessage(`{"role":"user","content":"third"}`), nil)
	resumeW.Append(msg3)
	resumeW.Close()

	// Verify resumed session has all messages
	r, _ := s.Open(sessionID)
	if r.MessageCount() != 3 {
		t.Errorf("After resume, MessageCount = %d, want 3", r.MessageCount())
	}

	// Fork the session
	forkW, err := s.Fork(sessionID, nil)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	// Fork should inherit parent's message tree - Fork creates a copy
	forkMsg := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"forked"}`), nil)
	forkW.Append(forkMsg)
	forkW.Close()

	// Get fork session ID from the fork writer's header
	forkHeader, err := ReadHeader(forkW.Path())
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	// Use full fork ID to avoid ambiguity (fork and parent share same 8-char prefix)
	forkID := forkHeader.ID

	// Verify fork has parent messages plus fork message
	r, err = s.Open(forkID)
	if err != nil {
		t.Fatalf("Open fork: %v", err)
	}
	// Fork copies parent entries, so it should have: forkMsg only (fork starts fresh with copy)
	if r.MessageCount() != 1 {
		t.Errorf("Fork MessageCount = %d, want 1 (fork has its own message only)", r.MessageCount())
	}

	// Verify fork references parent
	if r.Header().ParentSession == nil {
		t.Error("Fork should have ParentSession reference")
	}
	if *r.Header().ParentSession != parentHeader.ID {
		t.Errorf("Fork ParentSession = %v, want %v", *r.Header().ParentSession, parentHeader.ID)
	}
}

// TestSessionOwnerFileSemantics tests owner file semantics for session resume.
func TestSessionOwnerFileSemantics(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorageAt(dir)

	header := NewHeader("/test", nil, nil)
	w, _ := s.Create(header)
	w.Append(NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"test"}`), nil))
	w.Close()

	sessionID := header.ID[:8]

	// First owner claims
	if err := ClaimOwner(w.Path()); err != nil {
		t.Fatalf("ClaimOwner: %v", err)
	}

	// First owner can resume
	resumeW, err := s.Resume(sessionID)
	if err != nil {
		t.Errorf("First owner Resume should succeed: %v", err)
	}
	// Resume returns a Writer for the same session
	if resumeW.Path() != w.Path() {
		t.Errorf("Resume returned wrong path: got %s, want %s", resumeW.Path(), w.Path())
	}

	// Release ownership
	if err := ReleaseOwner(resumeW.Path()); err != nil {
		t.Fatalf("ReleaseOwner: %v", err)
	}

	// Second owner can now claim
	if err := ClaimOwner(resumeW.Path()); err != nil {
		t.Fatalf("ClaimOwner after release: %v", err)
	}

	// Clean up
	ReleaseOwner(resumeW.Path())
}

func strPtr(s string) *string { return &s }
func newIDForTest() string    { return uuid.Must(uuid.NewV7()).String() }

// TestConcurrentAppendAndIndexRegeneration verifies that multiple sequential
// appends to the same session followed by index regeneration work correctly.
// This tests the live CLI path: create, append entries via Resume/Close cycles,
// then verify the index reflects the final state after a full scan.
func TestConcurrentAppendAndIndexRegeneration(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStorageAt(dir)

	header := NewHeader("/test", nil, nil)
	w, err := s.Create(header)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	w.Close()

	sessionID := header.ID[:8]

	// Simulate multiple Resume/Append/Close cycles (like the live CLI does per turn).
	// Each cycle represents one turn of interaction, appending entries to the session.
	const numCycles = 4
	const entriesPerCycle = 10

	for cycle := 0; cycle < numCycles; cycle++ {
		resumeW, err := s.Resume(sessionID)
		if err != nil {
			t.Fatalf("Cycle %d: Resume: %v", cycle, err)
		}

		for j := 0; j < entriesPerCycle; j++ {
			msg := NewMessageEntry(nil, json.RawMessage(
				fmt.Sprintf(`{"role":"user","content":"cycle-%d-msg-%d"}`, cycle, j),
			), &TokenCounts{Input: 1})
			if err := resumeW.Append(msg); err != nil {
				t.Fatalf("Cycle %d: Append: %v", cycle, err)
			}
		}

		if err := resumeW.Close(); err != nil {
			t.Fatalf("Cycle %d: Close: %v", cycle, err)
		}
	}

	// Verify the session file has the expected number of entries.
	r, err := s.Open(sessionID)
	if err != nil {
		t.Fatalf("Open after appends: %v", err)
	}

	expectedCount := numCycles * entriesPerCycle
	actualCount := r.MessageCount()
	if actualCount != expectedCount {
		t.Errorf("MessageCount = %d, want %d", actualCount, expectedCount)
	}

	// Verify total tokens are accumulated correctly.
	tokens := r.TotalTokens()
	if tokens.Input != expectedCount {
		t.Errorf("TotalTokens.Input = %d, want %d", tokens.Input, expectedCount)
	}

	// Verify index regeneration by calling List(), which will detect stale
	// index and regenerate it by reading the session file.
	summaries, err := s.List()
	if err != nil {
		t.Fatalf("List after appends: %v", err)
	}
	if len(summaries) != 1 {
		t.Errorf("List count = %d, want 1", len(summaries))
	}
	if summaries[0].MessageCount != expectedCount {
		t.Errorf("List summary MessageCount = %d, want %d", summaries[0].MessageCount, expectedCount)
	}

	// Verify the session entries can be read back correctly.
	entries := r.Entries()
	// r.Entries() may include or exclude the header entry depending on
	// implementation. We verify entries are non-zero and consistent.
	if len(entries) < expectedCount {
		t.Errorf("Entry count = %d, want at least %d", len(entries), expectedCount)
	}
}

// TestJSONLAllEntryTypesRoundTrip is the JSONL v3 all-entry golden fixture.
// It constructs one Entry of every supported type. JSONL session files
// v3 acceptance invariants: JSONL session files remain round-trippable;
// custom non-message session entries do not enter model context; custom
// message entries do enter model context; unknown custom/extension data is
// preserved), writes them to a temp session file, reads them back, and
// verifies that every entry round-trips content-equivalent and that custom
// data survives the round-trip.
//
// This is the canonical "does v3 work for every entry type" smoke test.
func TestJSONLAllEntryTypesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	header := NewHeader("/test/fixture", nil, nil, "all-entry-fixture")
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}

	// 1. message (EntryTypeMessage) — in LLM context.
	msg := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"hello world"}`), &TokenCounts{Input: 2, Output: 0, CachedInput: 1})
	if err := w.Append(msg); err != nil {
		t.Fatalf("Append message: %v", err)
	}

	// 2. position_change (EntryTypePositionChange) — navigation.
	pos := NewPositionChangeEntry("leaf")
	if err := w.Append(pos); err != nil {
		t.Fatalf("Append position_change: %v", err)
	}

	// 3. compaction (EntryTypeCompaction) — in LLM context.
	comp := NewCompactionEntry("summary of earlier messages", "first-kept-id", 1000, 200, true)
	if err := w.Append(comp); err != nil {
		t.Fatalf("Append compaction: %v", err)
	}

	// 4. branch_summary (EntryTypeBranchSummary).
	bs := NewBranchSummaryEntry("source-entry-id", "branch summary text", false)
	if err := w.Append(bs); err != nil {
		t.Fatalf("Append branch_summary: %v", err)
	}

	// 5. session_info (EntryTypeSessionInfo).
	si := NewSessionInfoEntry("renamed session")
	if err := w.Append(si); err != nil {
		t.Fatalf("Append session_info: %v", err)
	}

	// 6. model_change (EntryTypeModelChange).
	mc := NewModelChangeEntry("anthropic", "claude-sonnet-4-6")
	if err := w.Append(mc); err != nil {
		t.Fatalf("Append model_change: %v", err)
	}

	// 7. thinking_level_change (EntryTypeThinkingChange).
	tlc := NewThinkingLevelChangeEntry("high")
	if err := w.Append(tlc); err != nil {
		t.Fatalf("Append thinking_level_change: %v", err)
	}

	// 8. label (EntryTypeLabel).
	lbl := NewLabelEntry(msg.ID, "important")
	if err := w.Append(lbl); err != nil {
		t.Fatalf("Append label: %v", err)
	}

	// 9. custom (EntryTypeCustom) — NOT in LLM context; arbitrary plugin payload.
	custom := NewCustomEntry("my_plugin", json.RawMessage(`{"state":"init","nested":{"key":"value"},"arr":[1,2,3]}`))
	if err := w.Append(custom); err != nil {
		t.Fatalf("Append custom: %v", err)
	}

	// 10. custom_message (EntryTypeCustomMessage) — IS in LLM context.
	customMsg := NewCustomMessageEntry("my_plugin", json.RawMessage(`{"ctx":"data"}`), json.RawMessage(`{"role":"system","content":"plugin-injected context"}`))
	if err := w.Append(customMsg); err != nil {
		t.Fatalf("Append custom_message: %v", err)
	}

	// 11. queue_update (from event_types.go) — embedded Entry + extra fields.
	queueUpdate := NewQueueUpdateEntry(3, 1)
	if err := w.Append(queueUpdate); err != nil {
		t.Fatalf("Append queue_update: %v", err)
	}

	// 12. compaction_lifecycle (from event_types.go).
	compLifecycle := NewCompactionLifecycleEntry("manual", false, false, "")
	if err := w.Append(compLifecycle); err != nil {
		t.Fatalf("Append compaction_lifecycle: %v", err)
	}

	// Close writer to flush.
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Read back.
	r, err := ReadFile(w.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Verify entry count matches.
	if got, want := len(r.Entries()), 12; got != want {
		t.Errorf("Entries() count = %d, want %d", got, want)
	}

	// 1. message — round-trip.
	gotMsg := r.Entries()[0]
	if gotMsg.Type != EntryTypeMessage {
		t.Errorf("entry 0 type = %q, want %q", gotMsg.Type, EntryTypeMessage)
	}
	if string(gotMsg.Message) != `{"role":"user","content":"hello world"}` {
		t.Errorf("entry 0 message = %s, want round-tripped", gotMsg.Message)
	}
	if gotMsg.Tokens == nil || gotMsg.Tokens.Input != 2 || gotMsg.Tokens.CachedInput != 1 {
		t.Errorf("entry 0 tokens = %+v, want Input=2 CachedInput=1", gotMsg.Tokens)
	}

	// 2. position_change.
	gotPos := r.Entries()[1]
	if gotPos.Type != EntryTypePositionChange {
		t.Errorf("entry 1 type = %q, want position_change", gotPos.Type)
	}
	if gotPos.NewPosition == nil || *gotPos.NewPosition != "leaf" {
		t.Errorf("entry 1 new_position = %v, want \"leaf\"", gotPos.NewPosition)
	}

	// 3. compaction.
	gotComp := r.Entries()[2]
	if gotComp.Type != EntryTypeCompaction {
		t.Errorf("entry 2 type = %q, want compaction", gotComp.Type)
	}
	if gotComp.Summary == nil || *gotComp.Summary != "summary of earlier messages" {
		t.Errorf("entry 2 summary = %v", gotComp.Summary)
	}
	if gotComp.TokensBefore == nil || *gotComp.TokensBefore != 1000 {
		t.Errorf("entry 2 tokens_before = %v, want 1000", gotComp.TokensBefore)
	}
	if !gotComp.FromHook {
		t.Error("entry 2 from_hook = false, want true")
	}

	// 4. branch_summary.
	gotBS := r.Entries()[3]
	if gotBS.Type != EntryTypeBranchSummary {
		t.Errorf("entry 3 type = %q, want branch_summary", gotBS.Type)
	}
	if gotBS.FromID == nil || *gotBS.FromID != "source-entry-id" {
		t.Errorf("entry 3 from_id = %v, want source-entry-id", gotBS.FromID)
	}

	// 5. session_info.
	gotSI := r.Entries()[4]
	if gotSI.Type != EntryTypeSessionInfo {
		t.Errorf("entry 4 type = %q, want session_info", gotSI.Type)
	}
	if gotSI.Name == nil || *gotSI.Name != "renamed session" {
		t.Errorf("entry 4 name = %v, want renamed session", gotSI.Name)
	}

	// 6. model_change.
	gotMC := r.Entries()[5]
	if gotMC.Type != EntryTypeModelChange {
		t.Errorf("entry 5 type = %q, want model_change", gotMC.Type)
	}
	if gotMC.Provider == nil || *gotMC.Provider != "anthropic" {
		t.Errorf("entry 5 provider = %v, want anthropic", gotMC.Provider)
	}
	if gotMC.Model == nil || *gotMC.Model != "claude-sonnet-4-6" {
		t.Errorf("entry 5 model = %v, want claude-sonnet-4-6", gotMC.Model)
	}

	// 7. thinking_level_change.
	gotTLC := r.Entries()[6]
	if gotTLC.Type != EntryTypeThinkingChange {
		t.Errorf("entry 6 type = %q, want thinking_level_change", gotTLC.Type)
	}
	if gotTLC.Level == nil || *gotTLC.Level != "high" {
		t.Errorf("entry 6 level = %v, want high", gotTLC.Level)
	}

	// 8. label.
	gotLbl := r.Entries()[7]
	if gotLbl.Type != EntryTypeLabel {
		t.Errorf("entry 7 type = %q, want label", gotLbl.Type)
	}
	if gotLbl.TargetID == nil || *gotLbl.TargetID != msg.ID {
		t.Errorf("entry 7 target_id = %v, want %q", gotLbl.TargetID, msg.ID)
	}

	// 9. custom (NOT in LLM context). Critical: arbitrary plugin payload preserved.
	gotCustom := r.Entries()[8]
	if gotCustom.Type != EntryTypeCustom {
		t.Errorf("entry 8 type = %q, want custom", gotCustom.Type)
	}
	if gotCustom.CustomType == nil || *gotCustom.CustomType != "my_plugin" {
		t.Errorf("entry 8 custom_type = %v, want my_plugin", gotCustom.CustomType)
	}
	wantCustomData := `{"state":"init","nested":{"key":"value"},"arr":[1,2,3]}`
	if string(gotCustom.Data) != wantCustomData {
		t.Errorf("entry 8 data = %s, want %s", gotCustom.Data, wantCustomData)
	}
	// Verify the nested structure parses correctly.
	var customParsed struct {
		State  string `json:"state"`
		Nested struct {
			Key string `json:"key"`
		} `json:"nested"`
		Arr []int `json:"arr"`
	}
	if err := json.Unmarshal(gotCustom.Data, &customParsed); err != nil {
		t.Errorf("entry 8 data not reparseable: %v", err)
	}
	if customParsed.State != "init" || customParsed.Nested.Key != "value" || len(customParsed.Arr) != 3 {
		t.Errorf("entry 8 nested data = %+v, want State=init Nested.Key=value Arr=[1,2,3]", customParsed)
	}

	// 10. custom_message (IS in LLM context). Verify it shows up in ContextEntries.
	gotCustomMsg := r.Entries()[9]
	if gotCustomMsg.Type != EntryTypeCustomMessage {
		t.Errorf("entry 9 type = %q, want custom_message", gotCustomMsg.Type)
	}
	if string(gotCustomMsg.Message) != `{"role":"system","content":"plugin-injected context"}` {
		t.Errorf("entry 9 message = %s, want round-tripped", gotCustomMsg.Message)
	}

	// 11. queue_update — verify embedded Entry preserved.
	gotQueue := r.Entries()[10]
	if string(gotQueue.Type) != "queue_update" {
		t.Errorf("entry 10 type = %q, want queue_update", gotQueue.Type)
	}

	// 12. compaction_lifecycle.
	gotCompL := r.Entries()[11]
	if string(gotCompL.Type) != "compaction_lifecycle" {
		t.Errorf("entry 11 type = %q, want compaction_lifecycle", gotCompL.Type)
	}

	// InContext checks: message, compaction, custom_message ARE in context;
	// custom is NOT.
	if !EntryTypeMessage.InContext() {
		t.Error("EntryTypeMessage should be in LLM context")
	}
	if !EntryTypeCompaction.InContext() {
		t.Error("EntryTypeCompaction should be in LLM context")
	}
	if !EntryTypeCustomMessage.InContext() {
		t.Error("EntryTypeCustomMessage should be in LLM context")
	}
	if EntryTypeCustom.InContext() {
		t.Error("EntryTypeCustom must NOT be in LLM context (it's a session-only marker)")
	}
	if EntryTypePositionChange.InContext() {
		t.Error("EntryTypePositionChange must NOT be in LLM context")
	}
	if EntryTypeBranchSummary.InContext() {
		t.Error("EntryTypeBranchSummary must NOT be in LLM context")
	}
	if EntryTypeSessionInfo.InContext() {
		t.Error("EntryTypeSessionInfo must NOT be in LLM context")
	}
	if EntryTypeModelChange.InContext() {
		t.Error("EntryTypeModelChange must NOT be in LLM context")
	}
	if EntryTypeThinkingChange.InContext() {
		t.Error("EntryTypeThinkingChange must NOT be in LLM context")
	}
	if EntryTypeLabel.InContext() {
		t.Error("EntryTypeLabel must NOT be in LLM context")
	}

	// CustomEntries filter — verify custom and custom_message are queryable by type.
	pluginCustoms := r.CustomEntries("my_plugin")
	if len(pluginCustoms) != 2 {
		t.Errorf("CustomEntries(\"my_plugin\") = %d, want 2 (custom + custom_message)", len(pluginCustoms))
	}
}

func TestStorageUsesCWDScopedDirectories(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorageAt(dir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}

	cwdA := filepath.Join(dir, "project-a")
	cwdB := filepath.Join(dir, "project-b")
	headerA := NewHeader(cwdA, nil, nil)
	headerB := NewHeader(cwdB, nil, nil)

	wA, err := storage.Create(headerA)
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	if err := wA.Close(); err != nil {
		t.Fatalf("Close A: %v", err)
	}
	wB, err := storage.Create(headerB)
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}
	if err := wB.Close(); err != nil {
		t.Fatalf("Close B: %v", err)
	}

	if filepath.Dir(wA.Path()) == dir {
		t.Fatalf("session A was created in root dir, want cwd-scoped dir: %s", wA.Path())
	}
	if filepath.Dir(wA.Path()) != storage.DirForCWD(cwdA) {
		t.Fatalf("session A dir = %s, want %s", filepath.Dir(wA.Path()), storage.DirForCWD(cwdA))
	}

	summariesA, err := storage.ListCWD(cwdA)
	if err != nil {
		t.Fatalf("ListCWD A: %v", err)
	}
	if len(summariesA) != 1 || summariesA[0].ID != headerA.ID {
		t.Fatalf("ListCWD A = %+v, want only %s", summariesA, headerA.ID)
	}

	summariesAll, err := storage.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summariesAll) != 2 {
		t.Fatalf("List count = %d, want 2", len(summariesAll))
	}
}

func TestStorageMoveCWDRewritesHeaderAndMovesScopedDirectory(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorageAt(dir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}

	oldCWD := filepath.Join(dir, "wrong-project")
	newCWD := filepath.Join(dir, "right-project")
	if err := os.MkdirAll(newCWD, 0o755); err != nil {
		t.Fatalf("MkdirAll newCWD: %v", err)
	}
	header := NewHeader(oldCWD, nil, nil)
	w, err := storage.Create(header)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	oldPath := w.Path()
	msg := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"keep me"}`), nil)
	if err := w.Append(msg); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	old, err := storage.MoveCWD(header.ID[:8], newCWD)
	if err != nil {
		t.Fatalf("MoveCWD: %v", err)
	}
	if old != oldCWD {
		t.Fatalf("old cwd = %q, want %q", old, oldCWD)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old path still exists: %v", err)
	}

	r, err := storage.Open(header.ID[:8])
	if err != nil {
		t.Fatalf("Open moved: %v", err)
	}
	if r.Header().CWD != newCWD {
		t.Fatalf("moved header cwd = %q, want %q", r.Header().CWD, newCWD)
	}
	if r.MessageCount() != 1 {
		t.Fatalf("moved message count = %d, want 1", r.MessageCount())
	}

	oldList, err := storage.ListCWD(oldCWD)
	if err != nil {
		t.Fatalf("ListCWD old: %v", err)
	}
	if len(oldList) != 0 {
		t.Fatalf("old cwd list = %+v, want empty", oldList)
	}
	newList, err := storage.ListCWD(newCWD)
	if err != nil {
		t.Fatalf("ListCWD new: %v", err)
	}
	if len(newList) != 1 || newList[0].ID != header.ID {
		t.Fatalf("new cwd list = %+v, want moved session %s", newList, header.ID)
	}
}

func TestStorageMigratesLegacyFlatSessionsToCWDDirs(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "legacy-project")
	header := NewHeader(cwd, nil, nil)
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}
	legacyPath := w.Path()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	storage, err := NewStorageAt(dir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy flat path still exists after migration: %v", err)
	}

	migratedPath := filepath.Join(storage.DirForCWD(cwd), filepath.Base(legacyPath))
	if _, err := os.Stat(migratedPath); err != nil {
		t.Fatalf("migrated path missing: %v", err)
	}

	summaries, err := storage.ListCWD(cwd)
	if err != nil {
		t.Fatalf("ListCWD: %v", err)
	}
	if len(summaries) != 1 || summaries[0].ID != header.ID {
		t.Fatalf("ListCWD = %+v, want migrated session %s", summaries, header.ID)
	}
}

func TestStoragePinnedSessionsSortFirst(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorageAt(dir)
	if err != nil {
		t.Fatalf("NewStorageAt: %v", err)
	}

	oldHeader := NewHeader(filepath.Join(dir, "project"), nil, nil, "old")
	oldHeader.Timestamp = "2024-01-01T00:00:00Z"
	old, err := storage.Create(oldHeader)
	if err != nil {
		t.Fatalf("Create old: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("Close old: %v", err)
	}
	newHeader := NewHeader(filepath.Join(dir, "project"), nil, nil, "new")
	newHeader.Timestamp = "2025-01-01T00:00:00Z"
	newer, err := storage.Create(newHeader)
	if err != nil {
		t.Fatalf("Create new: %v", err)
	}
	if err := newer.Close(); err != nil {
		t.Fatalf("Close new: %v", err)
	}

	if err := storage.SetPinned(oldHeader.ID, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	summaries, err := storage.ListCWD(filepath.Join(dir, "project"))
	if err != nil {
		t.Fatalf("ListCWD: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summary count = %d, want 2", len(summaries))
	}
	if summaries[0].ID != oldHeader.ID || !summaries[0].Pinned {
		t.Fatalf("first summary = %+v, want pinned old session first", summaries[0])
	}
	if err := storage.SetPinned(oldHeader.ID, false); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	summaries, err = storage.ListCWD(filepath.Join(dir, "project"))
	if err != nil {
		t.Fatalf("ListCWD after unpin: %v", err)
	}
	if summaries[0].ID != newHeader.ID || summaries[0].Pinned {
		t.Fatalf("first summary after unpin = %+v, want newest unpinned", summaries[0])
	}
}
