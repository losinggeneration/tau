// Package session implements the append-only JSONL session format with a
// tree-of-entries model, flock-based concurrency, and index management.
package session

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"
)

// managerLabelInfo holds the latest label and timestamp for an entry.
type managerLabelInfo struct {
	Label        string
	Timestamp    time.Time
	TimestampStr string
}

// Manager provides in-memory session management without persistence.
// It maintains all session state in memory and is useful for:
// - Headless/embedded agents that don't need disk-backed sessions
// - Testing scenarios
// - Temporary sessions during processing
type Manager struct {
	mu        sync.RWMutex
	sessionID string
	header    Header
	entries   []Entry
	byID      map[string]int // index into entries by ID
	labels    map[string]managerLabelInfo
	leafID    *string // current position in the tree

	// flush indicates whether entries have been flushed to underlying storage.
	// When false, new appends should write all entries.
	flushed bool
}

// NewManager creates an in-memory session manager.
// If an existing header is provided, it starts with that state.
// Otherwise, creates a new session with the given cwd.
func NewManager(cwd string, opts ...ManagerOption) *Manager {
	header := NewHeader(cwd, nil, nil)
	m := &Manager{
		sessionID: header.ID,
		header:    header,
		entries:   []Entry{},
		byID:      map[string]int{},
		labels:    make(map[string]managerLabelInfo),
		leafID:    nil,
		flushed:   true, // no persistence, so consider flushed
	}

	// Store header separately - entries slice holds only non-header entries
	for _, opt := range opts {
		opt(m)
	}

	return m
}

// ManagerOption configures Manager behavior.
type ManagerOption func(*Manager)

// WithSessionID sets a specific session ID for the manager.
func WithSessionID(id string) ManagerOption {
	return func(m *Manager) {
		m.sessionID = id
		m.header.ID = id
	}
}

// WithParentSession sets the parent session ID.
func WithParentSession(parentID string) ManagerOption {
	return func(m *Manager) {
		m.header.ParentSession = &parentID
	}
}

// WithHeader sets a complete session header.
// Note: This preserves ParentSession if it was already set by WithParentSession.
func WithHeader(h Header) ManagerOption {
	return func(m *Manager) {
		// Preserve ParentSession if it was set by a previous option
		if m.header.ParentSession != nil && h.ParentSession == nil {
			h.ParentSession = m.header.ParentSession
		}
		m.header = h
		m.sessionID = h.ID
	}
}

// NewSessionOptions contains options for creating a new session.
type NewSessionOptions struct {
	ID            string
	ParentSession string
	ParentEntry   string
}

// NewSession starts a fresh session, clearing all entries.
func (m *Manager) NewSession(cwd string, opts NewSessionOptions) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sessionID := opts.ID
	if sessionID == "" {
		sessionID = NewID()
	}

	var parentSession *string
	if opts.ParentSession != "" {
		parentSession = &opts.ParentSession
	}
	var parentEntry *string
	if opts.ParentEntry != "" {
		parentEntry = &opts.ParentEntry
	}

	m.header = NewHeader(cwd, parentSession, parentEntry)
	m.header.ID = sessionID
	m.sessionID = sessionID

	m.entries = []Entry{}
	m.byID = map[string]int{}
	m.labels = make(map[string]managerLabelInfo)
	m.leafID = nil
	m.flushed = true
}

// SessionID returns the current session ID.
func (m *Manager) SessionID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessionID
}

// CWD returns the session's working directory.
func (m *Manager) CWD() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.header.CWD
}

// Header returns the session header.
func (m *Manager) Header() Header {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.header
}

// IsPersisted returns false since this is an in-memory manager.
func (m *Manager) IsPersisted() bool {
	return false
}

// CurrentPosition returns the current leaf position.
func (m *Manager) CurrentPosition() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.leafID == nil {
		return ""
	}
	return *m.leafID
}

// SetCurrentPosition moves the leaf to a specific entry.
func (m *Manager) SetCurrentPosition(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if it's the header
	if id == m.header.ID {
		// Allow setting position to header (before any entries)
		m.leafID = nil
		return nil
	}

	_, ok := m.byID[id]
	if !ok {
		return &sessionError{fmt: "entry %q not found", args: []any{id}}
	}

	m.leafID = &id
	return nil
}

// BranchTo moves the leaf to a specific entry for branching.
func (m *Manager) BranchTo(id string) error {
	return m.SetCurrentPosition(id)
}

// ResetLeaf resets the leaf to before any entries (creates new root).
func (m *Manager) ResetLeaf() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leafID = nil
}

// LoadEntries loads entries from a Reader, resetting internal state.
func (m *Manager) LoadEntries(r *Reader) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.header = r.Header()
	m.sessionID = m.header.ID
	m.entries = r.Entries()
	m.byID = make(map[string]int, len(m.entries)+1)
	m.byID[m.header.ID] = -1 // header at index -1 (not in entries slice)
	for i, e := range m.entries {
		m.byID[e.ID] = i
	}

	// Build label index
	m.labels = make(map[string]managerLabelInfo)
	for _, e := range m.entries {
		if e.Type == EntryTypeLabel && e.TargetID != nil && e.Label != nil && *e.Label != "" {
			t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
			if err != nil {
				continue
			}
			existing := m.labels[*e.TargetID]
			if existing.Timestamp.IsZero() || t.After(existing.Timestamp) {
				m.labels[*e.TargetID] = managerLabelInfo{
					Label:        *e.Label,
					Timestamp:    t,
					TimestampStr: e.Timestamp,
				}
			}
		}
	}

	// Set leaf to last entry
	if len(m.entries) > 0 {
		last := m.entries[len(m.entries)-1]
		if last.Type != EntryTypeSession {
			m.leafID = &last.ID
		}
	}
}

// ActiveBranch returns the chain of entry IDs from current position back to root.
func (m *Manager) ActiveBranch() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.activeBranchLocked()
}

func (m *Manager) activeBranchLocked() []string {
	var branch []string
	visited := make(map[string]bool)
	current := ""
	if m.leafID != nil {
		current = *m.leafID
	}

	for current != "" {
		if visited[current] {
			break // cycle protection
		}
		visited[current] = true

		idx, ok := m.byID[current]
		if !ok {
			break
		}
		branch = append(branch, current)

		var parentID *string
		if idx < 0 {
			break // reached header
		}
		parentID = m.entries[idx].ParentID
		if parentID == nil {
			break
		}
		current = *parentID
	}

	// Reverse to chronological order
	for i, j := 0, len(branch)-1; i < j; i, j = i+1, j-1 {
		branch[i], branch[j] = branch[j], branch[i]
	}

	return branch
}

// Entries returns all entries (excludes header).
func (m *Manager) Entries() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]Entry, len(m.entries))
	copy(result, m.entries)
	return result
}

// EntriesByType returns all entries of a given type.
func (m *Manager) EntriesByType(typ EntryType) []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []Entry
	for _, e := range m.entries {
		if e.Type == typ {
			result = append(result, e)
		}
	}
	return result
}

// FindByID returns an entry by its ID, or nil.
func (m *Manager) FindByID(id string) *Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Header cannot be returned as Entry - check if it's a valid entry ID
	if id == m.header.ID {
		return nil // header is not an Entry
	}

	idx, ok := m.byID[id]
	if !ok {
		return nil
	}
	return &m.entries[idx]
}

// GetLabel returns the label for an entry, if any.
func (m *Manager) GetLabel(id string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.labels[id].Label
}

// Tree returns the session as a tree structure.
func (m *Manager) Tree() []*TreeNode {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Create nodes (entries only, excludes header)
	nodeMap := make(map[string]*TreeNode, len(m.entries))
	for _, e := range m.entries {
		info := m.labels[e.ID]
		nodeMap[e.ID] = &TreeNode{
			Entry:          e,
			Children:       nil,
			Label:          info.Label,
			LabelTimestamp: info.TimestampStr,
		}
	}

	// Build tree structure
	var roots []*TreeNode
	for _, e := range m.entries {
		node := nodeMap[e.ID]
		if e.ParentID == nil {
			roots = append(roots, node)
		} else if parent, ok := nodeMap[*e.ParentID]; ok {
			parent.Children = append(parent.Children, node)
		} else {
			// Orphan - treat as root
			roots = append(roots, node)
		}
	}

	// Sort children by timestamp
	sortChildrenRecursive(roots)
	return roots
}

// GetChildren returns all direct children of an entry.
func (m *Manager) GetChildren(parentID string) []*TreeNode {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var children []*TreeNode
	for _, e := range m.entries {
		if e.ParentID != nil && *e.ParentID == parentID {
			info := m.labels[e.ID]
			children = append(children, &TreeNode{
				Entry:          e,
				Children:       nil,
				Label:          info.Label,
				LabelTimestamp: info.TimestampStr,
			})
		}
	}

	// Sort by timestamp
	slices.SortFunc(children, func(a, b *TreeNode) int {
		return cmp.Compare(a.Entry.Timestamp, b.Entry.Timestamp)
	})

	return children
}

// GetBranch returns all entries from the given entry to root.
func (m *Manager) GetBranch(fromID string) []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var path []Entry
	currentID := fromID

	for currentID != "" {
		// Check header first
		if currentID == m.header.ID {
			break
		}

		idx, ok := m.byID[currentID]
		if !ok {
			break
		}
		path = append(path, m.entries[idx])

		if m.entries[idx].ParentID == nil {
			break
		}
		currentID = *m.entries[idx].ParentID
	}

	// Reverse to chronological order
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}

	return path
}

// ContextEntries returns entries that should be included in LLM context.
func (m *Manager) ContextEntries() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	branch := m.activeBranchLocked()
	if len(branch) == 0 {
		return nil
	}

	// Build map for lookups
	byID := make(map[string]int, len(m.entries)+1)
	byID[m.header.ID] = -1
	for i, e := range m.entries {
		byID[e.ID] = i
	}

	// Find most recent compaction on active branch
	compactionIdx := -1
	for i := len(m.entries) - 1; i >= 0; i-- {
		e := &m.entries[i]
		if e.Type == EntryTypeCompaction && e.FirstKeptID != nil {
			if _, ok := byID[*e.FirstKeptID]; ok {
				// Check if it's on the active branch
				for _, id := range branch {
					if id == *e.FirstKeptID || id == e.ID {
						compactionIdx = i
						break
					}
				}
				if compactionIdx >= 0 {
					break
				}
			}
		}
	}

	var result []Entry
	var firstKeptID string
	afterCompaction := compactionIdx < 0

	if compactionIdx >= 0 {
		firstKeptID = *m.entries[compactionIdx].FirstKeptID
		result = append(result, m.entries[compactionIdx])
	}

	for _, id := range branch {
		// Skip header
		if id == m.header.ID {
			continue
		}

		idx, ok := m.byID[id]
		if !ok {
			continue
		}

		if !afterCompaction {
			if id == firstKeptID {
				afterCompaction = true
			} else {
				continue
			}
		}

		e := &m.entries[idx]
		if e.Type.InContext() {
			result = append(result, *e)
		}
	}

	return result
}

// AppendMessage appends a message entry and returns its ID.
func (m *Manager) AppendMessage(msg json.RawMessage, tokens *TokenCounts) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry := NewMessageEntry(m.leafID, msg, tokens)
	m.appendEntry(&entry)
	return entry.ID
}

// AppendPositionChange appends a position_change entry.
func (m *Manager) AppendPositionChange(newPos string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry := NewPositionChangeEntry(newPos)
	m.appendEntry(&entry)
	return entry.ID
}

// AppendCompaction appends a compaction entry.
func (m *Manager) AppendCompaction(summary string, firstKeptID string, tokensBefore, tokensAfter int) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry := NewCompactionEntry(summary, firstKeptID, tokensBefore, tokensAfter, false)
	m.appendEntry(&entry)
	return entry.ID
}

// AppendBranchSummary appends a branch_summary entry.
func (m *Manager) AppendBranchSummary(fromID, summary string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry := NewBranchSummaryEntry(fromID, summary, false)
	m.appendEntry(&entry)
	return entry.ID
}

// AppendSessionInfo appends a session_info entry.
func (m *Manager) AppendSessionInfo(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry := NewSessionInfoEntry(name)
	m.appendEntry(&entry)
	return entry.ID
}

// AppendModelChange appends a model_change entry.
func (m *Manager) AppendModelChange(provider, model string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry := NewModelChangeEntry(provider, model)
	m.appendEntry(&entry)
	return entry.ID
}

// AppendThinkingLevelChange appends a thinking_level_change entry.
func (m *Manager) AppendThinkingLevelChange(level string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry := NewThinkingLevelChangeEntry(level)
	m.appendEntry(&entry)
	return entry.ID
}

// AppendLabel appends a label entry.
func (m *Manager) AppendLabel(targetID string, label string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Verify target exists
	if _, ok := m.byID[targetID]; !ok && targetID != m.header.ID {
		return "", &sessionError{fmt: "entry %q not found", args: []any{targetID}}
	}

	entry := NewLabelEntry(targetID, label)
	m.appendEntry(&entry)

	// Update label index
	if label != "" {
		m.labels[targetID] = managerLabelInfo{
			Label:        label,
			Timestamp:    time.Now(),
			TimestampStr: entry.Timestamp,
		}
	} else {
		delete(m.labels, targetID)
	}

	return entry.ID, nil
}

// AppendCustom appends a custom entry (hidden from LLM context).
func (m *Manager) AppendCustom(customType string, data json.RawMessage) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry := NewCustomEntry(customType, data)
	m.appendEntry(&entry)
	return entry.ID
}

// AppendCustomMessage appends a custom_message entry (injected into context).
func (m *Manager) AppendCustomMessage(customType string, data, msg json.RawMessage) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry := NewCustomMessageEntry(customType, data, msg)
	m.appendEntry(&entry)
	return entry.ID
}

// appendEntry adds an entry to the manager, updating indexes.
func (m *Manager) appendEntry(entry *Entry) {
	entry.ParentID = m.leafID
	m.entries = append(m.entries, *entry)
	m.byID[entry.ID] = len(m.entries) - 1
	m.leafID = &entry.ID
}

// DisplayName returns the session display name.
func (m *Manager) DisplayName() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for i := len(m.entries) - 1; i >= 0; i-- {
		if m.entries[i].Type == EntryTypeSessionInfo && m.entries[i].Name != nil {
			return *m.entries[i].Name
		}
	}
	return ""
}

// MessageCount returns the number of message entries.
func (m *Manager) MessageCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	n := 0
	for _, e := range m.entries {
		if e.Type == EntryTypeMessage || e.Type == EntryTypeCustomMessage {
			n++
		}
	}
	return n
}

// TotalTokens sums token counts across all message entries.
func (m *Manager) TotalTokens() TokenCounts {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var total TokenCounts
	for _, e := range m.entries {
		if e.Type == EntryTypeMessage && e.Tokens != nil {
			total.Input += e.Tokens.Input
			total.Output += e.Tokens.Output
			total.CachedInput += e.Tokens.CachedInput
		}
	}
	return total
}

// ToReader creates a Reader from the current state for export/compatibility.
func (m *Manager) ToReader(path string) *Reader {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return &Reader{
		path:    path,
		header:  m.header,
		entries: m.entries,
	}
}

// LeafID returns the current leaf entry ID.
func (m *Manager) LeafID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.leafID == nil {
		return ""
	}
	return *m.leafID
}

// sessionError represents a session operation error.
type sessionError struct {
	fmt  string
	args []any
}

func (e *sessionError) Error() string {
	return fmt.Sprintf(e.fmt, e.args...)
}
