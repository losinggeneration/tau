package session

import (
	"encoding/json"
	"testing"
)

func TestNewManager(t *testing.T) {
	m := NewManager("/tmp/test")
	if m.sessionID == "" {
		t.Error("SessionID should not be empty")
	}
	if m.header.CWD != "/tmp/test" {
		t.Errorf("CWD = %v, want /tmp/test", m.header.CWD)
	}
	if m.header.Type != EntryTypeSession {
		t.Errorf("Type = %v, want %v", m.header.Type, EntryTypeSession)
	}
	if m.IsPersisted() {
		t.Error("IsPersisted should return false")
	}
}

func TestManagerOptions(t *testing.T) {
	sessionID := "test-session-id"
	parentID := "parent-session-id"
	h := Header{
		Type:      EntryTypeSession,
		Version:   FormatVersion,
		ID:        "custom-header-id",
		Timestamp: "2024-01-01T00:00:00Z",
		CWD:       "/custom/cwd",
	}

	m := NewManager("/tmp/test",
		WithSessionID(sessionID),
		WithParentSession(parentID),
		WithHeader(h),
	)

	// WithHeader overwrites both sessionID and header.ID
	if m.sessionID != h.ID {
		t.Errorf("SessionID = %v, want %v (header ID)", m.sessionID, h.ID)
	}
	if m.header.ParentSession == nil || *m.header.ParentSession != parentID {
		t.Errorf("ParentSession = %v, want %v", m.header.ParentSession, parentID)
	}
	if m.header.ID != h.ID {
		t.Errorf("Header.ID = %v, want %v", m.header.ID, h.ID)
	}
	if m.header.CWD != h.CWD {
		t.Errorf("Header.CWD = %v, want %v", m.header.CWD, h.CWD)
	}
}

func TestManagerNewSession(t *testing.T) {
	m := NewManager("/tmp/test")
	oldID := m.sessionID

	opts := NewSessionOptions{
		ID:            "new-session-id",
		ParentSession: "parent-id",
	}
	m.NewSession("/new/cwd", opts)

	if m.sessionID != "new-session-id" {
		t.Errorf("SessionID = %v, want new-session-id", m.sessionID)
	}
	if m.header.CWD != "/new/cwd" {
		t.Errorf("CWD = %v, want /new/cwd", m.header.CWD)
	}
	if oldID == m.sessionID {
		t.Error("SessionID should have changed")
	}
	if len(m.entries) != 0 {
		t.Errorf("entries length = %v, want 0", len(m.entries))
	}
}

func TestManagerAppendMessage(t *testing.T) {
	m := NewManager("/tmp/test")
	msg := map[string]string{"role": "user", "content": "hello"}
	msgData, _ := json.Marshal(msg)

	id := m.AppendMessage(msgData, nil)
	if id == "" {
		t.Error("AppendMessage should return non-empty ID")
	}
	if len(m.entries) != 1 {
		t.Errorf("entries length = %v, want 1", len(m.entries))
	}
	if m.entries[0].Type != EntryTypeMessage {
		t.Errorf("Type = %v, want %v", m.entries[0].Type, EntryTypeMessage)
	}
}

func TestManagerAppendMessageChain(t *testing.T) {
	m := NewManager("/tmp/test")

	// Append first message
	id1 := m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	// Append second message
	id2 := m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)

	if id1 == id2 {
		t.Error("IDs should be unique")
	}
	if len(m.entries) != 2 {
		t.Errorf("entries length = %v, want 2", len(m.entries))
	}
	// Second message should have first as parent
	if m.entries[1].ParentID == nil || *m.entries[1].ParentID != id1 {
		t.Error("Second message should have first as parent")
	}
}

func TestManagerAppendLabel(t *testing.T) {
	m := NewManager("/tmp/test")

	// Append a message to label
	msgID := m.AppendMessage(json.RawMessage(`{"role":"user","content":"hello"}`), nil)

	labelID, err := m.AppendLabel(msgID, "important")
	if err != nil {
		t.Fatalf("AppendLabel failed: %v", err)
	}

	if labelID == "" {
		t.Error("AppendLabel should return non-empty ID")
	}
	if m.GetLabel(msgID) != "important" {
		t.Errorf("GetLabel = %v, want important", m.GetLabel(msgID))
	}
}

func TestManagerAppendLabelClear(t *testing.T) {
	m := NewManager("/tmp/test")
	msgID := m.AppendMessage(json.RawMessage(`{"role":"user","content":"hello"}`), nil)

	// Set label
	m.AppendLabel(msgID, "important")
	if m.GetLabel(msgID) != "important" {
		t.Errorf("GetLabel = %v, want important", m.GetLabel(msgID))
	}

	// Clear label
	m.AppendLabel(msgID, "")
	if m.GetLabel(msgID) != "" {
		t.Errorf("GetLabel after clear = %v, want empty", m.GetLabel(msgID))
	}
}

func TestManagerBranchTo(t *testing.T) {
	m := NewManager("/tmp/test")

	// Append two messages
	id1 := m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	id2 := m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)
	_ = id2 // declared but used for setup context

	// Branch to first message
	err := m.BranchTo(id1)
	if err != nil {
		t.Fatalf("BranchTo failed: %v", err)
	}

	// Append third message - should be child of first
	id3 := m.AppendMessage(json.RawMessage(`{"role":"user","content":"branched"}`), nil)

	if len(m.entries) != 3 {
		t.Errorf("entries length = %v, want 3", len(m.entries))
	}
	// New message should have first as parent
	if m.entries[2].ParentID == nil || *m.entries[2].ParentID != id1 {
		t.Error("Third message should have first as parent after branching")
	}
	// Leaf should be third message
	if m.LeafID() != id3 {
		t.Errorf("LeafID = %v, want %v", m.LeafID(), id3)
	}
}

func TestManagerResetLeaf(t *testing.T) {
	m := NewManager("/tmp/test")
	id1 := m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	id2 := m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)

	// After appends, current position should be the last entry
	if m.CurrentPosition() != id2 {
		t.Errorf("CurrentPosition = %v, want %v (last entry)", m.CurrentPosition(), id2)
	}

	m.ResetLeaf()

	if m.CurrentPosition() != "" {
		t.Errorf("CurrentPosition after reset = %v, want empty", m.CurrentPosition())
	}

	// Next append should create a new root
	id3 := m.AppendMessage(json.RawMessage(`{"role":"user","content":"new root"}`), nil)
	// After reset, new entry has no parent
	if m.entries[2].ParentID != nil {
		t.Errorf("New root ParentID = %v, want nil", m.entries[2].ParentID)
	}
	// It should be a child of the header (root)
	if m.entries[2].ParentID != nil {
		t.Error("New entry after reset should have nil parent (new root)")
	}
	_ = id1
	_ = id3
}

func TestManagerEntries(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	m.AppendLabel(m.entries[0].ID, "important")
	m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)

	entries := m.Entries()
	if len(entries) != 3 {
		t.Errorf("Entries() length = %v, want 3", len(entries))
	}
}

func TestManagerEntriesByType(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)

	msgEntries := m.EntriesByType(EntryTypeMessage)
	if len(msgEntries) != 2 {
		t.Errorf("EntriesByType(message) length = %v, want 2", len(msgEntries))
	}

	labelEntries := m.EntriesByType(EntryTypeLabel)
	if len(labelEntries) != 0 {
		t.Errorf("EntriesByType(label) length = %v, want 0", len(labelEntries))
	}
}

func TestManagerFindByID(t *testing.T) {
	m := NewManager("/tmp/test")
	id := m.AppendMessage(json.RawMessage(`{"role":"user","content":"hello"}`), nil)

	entry := m.FindByID(id)
	if entry == nil {
		t.Fatal("FindByID returned nil")
	}
	if entry.ID != id {
		t.Errorf("FindByID ID = %v, want %v", entry.ID, id)
	}

	// Non-existent ID
	if m.FindByID("non-existent") != nil {
		t.Error("FindByID should return nil for non-existent ID")
	}
}

func TestManagerTree(t *testing.T) {
	m := NewManager("/tmp/test")
	msgID := m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	childID := m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)

	// Set label
	m.AppendLabel(msgID, "important")

	tree := m.Tree()
	if len(tree) != 1 {
		t.Errorf("Tree root count = %v, want 1", len(tree))
	}
	// First message has 1 child (the response)
	if len(tree[0].Children) != 1 {
		t.Errorf("Tree root children count = %v, want 1", len(tree[0].Children))
	}
	// Label should be attached to the first message
	if tree[0].Label != "important" {
		t.Errorf("Tree root label = %v, want important", tree[0].Label)
	}
	_ = childID
}

func TestManagerGetChildren(t *testing.T) {
	m := NewManager("/tmp/test")
	msgID := m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)

	children := m.GetChildren(msgID)
	if len(children) != 1 {
		t.Errorf("GetChildren length = %v, want 1", len(children))
	}
}

func TestManagerGetBranch(t *testing.T) {
	m := NewManager("/tmp/test")
	id1 := m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	id2 := m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)

	// Branch to first message
	m.BranchTo(id1)
	id3 := m.AppendMessage(json.RawMessage(`{"role":"user","content":"branched"}`), nil)

	// Get branch from third message: id3 -> id1 (chronological order)
	branch := m.GetBranch(id3)
	if len(branch) != 2 {
		t.Errorf("GetBranch length = %v, want 2", len(branch))
	}

	// Check chronological order: id1 then id3
	if branch[0].ID != id1 {
		t.Errorf("First branch entry = %v, want %v", branch[0].ID, id1)
	}
	if branch[1].ID != id3 {
		t.Errorf("Second branch entry = %v, want %v", branch[1].ID, id3)
	}
	_ = id2
}

func TestManagerContextEntries(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)

	ctxEntries := m.ContextEntries()
	if len(ctxEntries) != 2 {
		t.Errorf("ContextEntries length = %v, want 2", len(ctxEntries))
	}

	// Custom entries should not be in context
	m.AppendCustom("test-type", json.RawMessage(`{}`))
	ctxEntries = m.ContextEntries()
	if len(ctxEntries) != 2 {
		t.Errorf("ContextEntries after custom = %v, want 2", len(ctxEntries))
	}
}

func TestManagerCustomMessageInContext(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	m.AppendCustomMessage("test-type", nil, json.RawMessage(`{"role":"custom","content":"injected"}`))
	m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)

	ctxEntries := m.ContextEntries()
	if len(ctxEntries) != 3 {
		t.Errorf("ContextEntries with custom_message = %v, want 3", len(ctxEntries))
	}
}

func TestManagerCompaction(t *testing.T) {
	m := NewManager("/tmp/test")
	id1 := m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)
	id3 := m.AppendMessage(json.RawMessage(`{"role":"user","content":"second"}`), nil)
	m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response2"}`), nil)

	// Add compaction (should include first message in summary)
	m.AppendCompaction("summarized content", id1, 100, 50)

	ctxEntries := m.ContextEntries()
	// Should include compaction entry and messages after firstKeptID
	if len(ctxEntries) == 0 {
		t.Error("ContextEntries should not be empty after compaction")
	}
	// First entry should be compaction
	if ctxEntries[0].Type != EntryTypeCompaction {
		t.Errorf("First entry type = %v, want Compaction", ctxEntries[0].Type)
	}
	_ = id3
}

func TestManagerDisplayName(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendSessionInfo("My Session")

	if m.DisplayName() != "My Session" {
		t.Errorf("DisplayName = %v, want My Session", m.DisplayName())
	}
}

func TestManagerMessageCount(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)
	m.AppendCustomMessage("test", nil, json.RawMessage(`{"role":"custom"}`))

	if m.MessageCount() != 3 {
		t.Errorf("MessageCount = %v, want 3", m.MessageCount())
	}
}

func TestManagerTotalTokens(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendMessage(json.RawMessage(`{"role":"user"}`), &TokenCounts{Input: 10, Output: 5})
	m.AppendMessage(json.RawMessage(`{"role":"assistant"}`), &TokenCounts{Input: 20, Output: 10})

	tokens := m.TotalTokens()
	if tokens.Input != 30 {
		t.Errorf("TotalTokens.Input = %v, want 30", tokens.Input)
	}
	if tokens.Output != 15 {
		t.Errorf("TotalTokens.Output = %v, want 15", tokens.Output)
	}
}

func TestManagerLoadEntries(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)

	// Create a Reader from the manager
	reader := m.ToReader("/test/path")

	// Create a new manager and load from reader
	m2 := NewManager("/tmp/test2")
	m2.LoadEntries(reader)

	if m2.sessionID != m.sessionID {
		t.Errorf("SessionID after load = %v, want %v", m2.sessionID, m.sessionID)
	}
	if len(m2.entries) != len(m.entries) {
		t.Errorf("Entries after load = %v, want %v", len(m2.entries), len(m.entries))
	}
}

func TestManagerToReader(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendMessage(json.RawMessage(`{"role":"user","content":"hello"}`), nil)

	reader := m.ToReader("/test/path")

	if reader.header.ID != m.sessionID {
		t.Errorf("Reader header ID = %v, want %v", reader.header.ID, m.sessionID)
	}
	if len(reader.entries) != 1 {
		t.Errorf("Reader entries length = %v, want 1", len(reader.entries))
	}
}

func TestManagerActiveBranch(t *testing.T) {
	m := NewManager("/tmp/test")
	id1 := m.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"response"}`), nil)

	branch := m.ActiveBranch()
	if len(branch) != 2 {
		t.Errorf("ActiveBranch length = %v, want 2", len(branch))
	}
	if branch[0] != id1 {
		t.Errorf("ActiveBranch[0] = %v, want %v", branch[0], id1)
	}
}

func TestManagerSessionInfo(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendSessionInfo("Test Session")
	m.AppendMessage(json.RawMessage(`{"role":"user"}`), nil)

	if m.DisplayName() != "Test Session" {
		t.Errorf("DisplayName = %v, want Test Session", m.DisplayName())
	}
}

func TestManagerModelChange(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendModelChange("openai", "gpt-4")

	entries := m.EntriesByType(EntryTypeModelChange)
	if len(entries) != 1 {
		t.Errorf("ModelChange entries = %v, want 1", len(entries))
	}
}

func TestManagerThinkingLevelChange(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendThinkingLevelChange("high")

	entries := m.EntriesByType(EntryTypeThinkingChange)
	if len(entries) != 1 {
		t.Errorf("ThinkingChange entries = %v, want 1", len(entries))
	}
}

func TestManagerBranchSummary(t *testing.T) {
	m := NewManager("/tmp/test")
	msgID := m.AppendMessage(json.RawMessage(`{"role":"user"}`), nil)
	m.AppendBranchSummary(msgID, "branch summary")

	entries := m.EntriesByType(EntryTypeBranchSummary)
	if len(entries) != 1 {
		t.Errorf("BranchSummary entries = %v, want 1", len(entries))
	}
}

func TestManagerCWD(t *testing.T) {
	m := NewManager("/test/cwd")
	if m.CWD() != "/test/cwd" {
		t.Errorf("CWD = %v, want /test/cwd", m.CWD())
	}
}

func TestManagerHeader(t *testing.T) {
	h := Header{
		Type:      EntryTypeSession,
		Version:   FormatVersion,
		ID:        "test-id",
		Timestamp: "2024-01-01T00:00:00Z",
		CWD:       "/test",
	}
	m := NewManager("/test", WithHeader(h))

	header := m.Header()
	if header.ID != h.ID {
		t.Errorf("Header().ID = %v, want %v", header.ID, h.ID)
	}
}

func TestManagerPositionChange(t *testing.T) {
	m := NewManager("/tmp/test")
	id1 := m.AppendMessage(json.RawMessage(`{"role":"user"}`), nil)
	m.AppendMessage(json.RawMessage(`{"role":"assistant"}`), nil)

	// Add position change
	m.AppendPositionChange(id1)

	entries := m.EntriesByType(EntryTypePositionChange)
	if len(entries) != 1 {
		t.Errorf("PositionChange entries = %v, want 1", len(entries))
	}
}

func TestManagerSetCurrentPosition(t *testing.T) {
	m := NewManager("/tmp/test")
	id1 := m.AppendMessage(json.RawMessage(`{"role":"user"}`), nil)
	m.AppendMessage(json.RawMessage(`{"role":"assistant"}`), nil)

	// Set position to first message
	err := m.SetCurrentPosition(id1)
	if err != nil {
		t.Fatalf("SetCurrentPosition failed: %v", err)
	}

	// Next append should be child of first message
	id3 := m.AppendMessage(json.RawMessage(`{"role":"user","content":"new"}`), nil)
	if m.entries[2].ParentID == nil || *m.entries[2].ParentID != id1 {
		t.Error("New message should have first as parent")
	}
	_ = id3
}

func TestManagerSetCurrentPositionHeader(t *testing.T) {
	m := NewManager("/tmp/test")
	m.AppendMessage(json.RawMessage(`{"role":"user"}`), nil)

	// Set position to header (null position)
	err := m.SetCurrentPosition(m.header.ID)
	if err != nil {
		t.Fatalf("SetCurrentPosition to header failed: %v", err)
	}

	if m.CurrentPosition() != "" {
		t.Errorf("CurrentPosition after header = %v, want empty", m.CurrentPosition())
	}
}

func TestManagerSetCurrentPositionNotFound(t *testing.T) {
	m := NewManager("/tmp/test")

	err := m.SetCurrentPosition("non-existent")
	if err == nil {
		t.Error("SetCurrentPosition should fail for non-existent ID")
	}
}

func TestManagerAppendLabelTargetNotFound(t *testing.T) {
	m := NewManager("/tmp/test")

	_, err := m.AppendLabel("non-existent", "label")
	if err == nil {
		t.Error("AppendLabel should fail for non-existent target")
	}
}

func TestAppendContextEditValidatesTargetAndContent(t *testing.T) {
	m := NewManager("/test")
	id := m.AppendMessage(json.RawMessage(`{"role":"assistant","content":"original"}`), nil)
	edit, err := m.AppendContextEdit(id, json.RawMessage(`{"content":"replacement"}`))
	if err != nil || edit == "" {
		t.Fatalf("edit=%q err=%v", edit, err)
	}
	if got := m.ContextEntries()[0].ToAgentMessage(); got.Content[0].Text != "replacement" {
		t.Fatalf("content=%+v", got.Content)
	}
	for _, replacement := range []string{`{}`, `{"content":42}`, `{"content":null}`, `bad`} {
		if _, err := m.AppendContextEdit(id, json.RawMessage(replacement)); err == nil {
			t.Fatalf("accepted %s", replacement)
		}
	}
	if _, err := m.AppendContextEdit("missing", json.RawMessage(`null`)); err == nil {
		t.Fatal("accepted missing target")
	}
	if err := m.BranchTo(id); err != nil {
		t.Fatal(err)
	}
	other := m.AppendMessage(json.RawMessage(`{"role":"user","content":"other"}`), nil)
	if err := m.BranchTo(id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendContextEdit(other, json.RawMessage(`null`)); err == nil {
		t.Fatal("accepted target on another branch")
	}
}

func TestManagerLoadEntriesRestoresSelectedPosition(t *testing.T) {
	first := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"first"}`), nil)
	later := NewMessageEntry(&first.ID, json.RawMessage(`{"role":"user","content":"later"}`), nil)
	position := NewPositionChangeEntry(first.ID)
	reader := &Reader{header: NewHeader("/test", nil, nil), entries: []Entry{first, later, position}}
	manager := NewManager("/test")
	manager.LoadEntries(reader)
	if got := manager.CurrentPosition(); got != first.ID {
		t.Fatalf("position = %q, want %q", got, first.ID)
	}
	if got := manager.ContextEntries(); len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("context = %+v", got)
	}
	manager.LoadEntries(&Reader{header: reader.header})
	if got := manager.CurrentPosition(); got != "" {
		t.Fatalf("empty reload retained position %q", got)
	}
}

func TestManagerToReaderPreservesSelectedBranch(t *testing.T) {
	manager := NewManager("/test")
	first := manager.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	manager.AppendMessage(json.RawMessage(`{"role":"user","content":"later"}`), nil)
	if err := manager.SetCurrentPosition(first); err != nil {
		t.Fatal(err)
	}
	reader := manager.ToReader("")
	if got := reader.CurrentPosition(); got != first {
		t.Fatalf("reader position = %q, want %q", got, first)
	}
	if got := len(manager.Entries()); got != 2 {
		t.Fatalf("reader conversion changed stored entries: %d", got)
	}
	manager.ResetLeaf()
	if got := manager.ToReader("").ContextEntries(); len(got) != 0 {
		t.Fatalf("reset leaf restored old context: %+v", got)
	}
}

func TestManagerBranchSummaryInContext(t *testing.T) {
	m := NewManager("/test")
	id := m.AppendMessage(json.RawMessage(`{"role":"user","content":"hello"}`), nil)
	summaryID := m.AppendBranchSummary(id, "decisions")
	entries := m.ContextEntries()
	if len(entries) != 2 || entries[1].ID != summaryID {
		t.Fatalf("branch summary missing from context: %+v", entries)
	}
}
