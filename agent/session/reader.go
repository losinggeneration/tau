package session

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// TreeNode represents a node in the session tree with resolved labels.
type TreeNode struct {
	Entry          Entry
	Children       []*TreeNode
	Label          string
	LabelTimestamp string
}

// Reader reads and parses entries from a session JSONL file.
type Reader struct {
	path    string
	header  Header
	entries []Entry
}

// ReadFile reads an entire session file into memory.
// It automatically applies format migrations if needed.
func ReadFile(path string) (reader *Reader, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return nil, fmt.Errorf("lock session for read: %w", err)
	}
	locked := true
	closed := false
	defer func() {
		if locked {
			if unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); unlockErr != nil && err == nil {
				err = fmt.Errorf("unlock session read: %w", unlockErr)
			}
		}
		if !closed {
			if closeErr := f.Close(); closeErr != nil && err == nil {
				err = fmt.Errorf("close session: %w", closeErr)
			}
		}
	}()

	scanner := bufio.NewScanner(f)
	// Allow large lines (up to 10MB for messages with large content)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	// Read raw lines for migration
	var rawLines []json.RawMessage

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)
		rawLines = append(rawLines, json.RawMessage(lineCopy))
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}

	if len(rawLines) == 0 {
		return nil, fmt.Errorf("empty session file")
	}

	// Apply migrations if needed
	migrated, err := Migrate(rawLines)
	if err != nil {
		return nil, fmt.Errorf("migrate session: %w", err)
	}

	// If migrated, write back to file for persistence
	if migrated {
		locked = false
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			return nil, fmt.Errorf("unlock session before migration write: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close session before migration write: %w", err)
		}
		closed = true
		if err := writeMigratedFile(path, rawLines); err != nil {
			return nil, fmt.Errorf("write migrated session file: %w", err)
		}
	}

	// Parse header
	header, err := ParseHeader(rawLines[0])
	if err != nil {
		return nil, fmt.Errorf("parse header: %w", err)
	}

	// Parse entries
	var entries []Entry
	for i := 1; i < len(rawLines); i++ {
		entry, err := ParseEntry(rawLines[i])
		if err != nil {
			// Skip malformed entries but continue reading
			continue
		}
		entries = append(entries, entry)
	}

	return &Reader{path: path, header: header, entries: entries}, nil
}

// writeMigratedFile atomically writes the migrated raw lines back to the session file.
func writeMigratedFile(path string, lines []json.RawMessage) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".migrate-*")
	if err != nil {
		return fmt.Errorf("create temp migrated session: %w", err)
	}
	tmpPath := f.Name()
	defer func() {
		if err != nil {
			if removeErr := os.Remove(tmpPath); removeErr != nil && !os.IsNotExist(removeErr) {
				err = fmt.Errorf("%w; remove temp migrated session: %v", err, removeErr)
			}
		}
	}()
	closed := false
	defer func() {
		if closed {
			return
		}
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close migrated session: %w", closeErr)
		}
	}()

	for _, line := range lines {
		if _, err := f.Write(line); err != nil {
			return fmt.Errorf("write line: %w", err)
		}
		if _, err := f.Write([]byte{'\n'}); err != nil {
			return fmt.Errorf("write newline: %w", err)
		}
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync migrated session: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close migrated session: %w", err)
	}
	closed = true
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("chmod migrated session: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace migrated session: %w", err)
	}

	return nil
}

// Header returns the session header.
func (r *Reader) Header() Header {
	return r.header
}

// Entries returns all entries in the file.
func (r *Reader) Entries() []Entry {
	return r.entries
}

// Path returns the file path.
func (r *Reader) Path() string {
	return r.path
}

// ToAgentMessages converts the context entries (messages on the active branch
// that should be included in LLM context) to agent.Message format.
func (r *Reader) ToAgentMessages() []*Message {
	contextEntries := r.ContextEntries()
	messages := make([]*Message, 0, len(contextEntries))
	for _, entry := range contextEntries {
		if msg := entry.ToAgentMessage(); msg != nil {
			messages = append(messages, msg)
		}
	}
	return messages
}

// CurrentPosition resolves the current position in the session tree.
// It returns the entry ID of the current head. If a position_change exists,
// it finds the last message after that position_change whose ancestry includes
// the position target. If no such message exists, it returns the target itself.
// If no position_change exists, it returns the last message entry.
// If no message entry exists but there are custom_message entries, returns the
// last custom_message entry.
func (r *Reader) CurrentPosition() string {
	// Find the last position_change
	posChangeIdx := -1
	var startPos string
	for i := len(r.entries) - 1; i >= 0; i-- {
		if r.entries[i].Type == EntryTypePositionChange && r.entries[i].NewPosition != nil {
			posChangeIdx = i
			startPos = *r.entries[i].NewPosition
			break
		}
	}

	if startPos == "" {
		// No position change; return the last message or custom_message entry
		for i := len(r.entries) - 1; i >= 0; i-- {
			if r.entries[i].Type == EntryTypeMessage || r.entries[i].Type == EntryTypeCustomMessage {
				return r.entries[i].ID
			}
		}
		return ""
	}

	// Build ID -> Entry map for ancestry checks
	byID := make(map[string]*Entry, len(r.entries))
	for i := range r.entries {
		byID[r.entries[i].ID] = &r.entries[i]
	}

	// Find the last message or custom_message after the position_change that descends from startPos
	for i := len(r.entries) - 1; i > posChangeIdx; i-- {
		if r.entries[i].Type != EntryTypeMessage && r.entries[i].Type != EntryTypeCustomMessage {
			continue
		}
		// Walk ancestry to check if this message descends from startPos
		current := r.entries[i].ID
		for current != "" {
			if current == startPos {
				return r.entries[i].ID
			}
			e, ok := byID[current]
			if !ok || e.ParentID == nil {
				break
			}
			current = *e.ParentID
		}
	}

	return startPos
}

// ActiveBranch returns the chain of entry IDs from the current position
// back to the root, in chronological order.
func (r *Reader) ActiveBranch() []string {
	pos := r.CurrentPosition()
	if pos == "" {
		return nil
	}

	// Build a map of ID -> Entry
	byID := make(map[string]*Entry, len(r.entries))
	for i := range r.entries {
		byID[r.entries[i].ID] = &r.entries[i]
	}

	// Walk from position to root
	var branch []string
	visited := make(map[string]bool)
	current := pos

	for current != "" {
		if visited[current] {
			break // cycle protection
		}
		visited[current] = true

		e, ok := byID[current]
		if !ok {
			break
		}
		branch = append(branch, current)

		if e.ParentID == nil {
			break
		}
		current = *e.ParentID
	}

	// Reverse to chronological order
	for i, j := 0, len(branch)-1; i < j; i, j = i+1, j-1 {
		branch[i], branch[j] = branch[j], branch[i]
	}

	return branch
}

// ContextEntries returns entries on the active branch that should be included
// in LLM context, respecting the most recent compaction as a truncation pointer.
func (r *Reader) ContextEntries() []Entry {
	branch := r.ActiveBranch()
	if len(branch) == 0 {
		return nil
	}

	// Build a map of ID -> Entry
	byID := make(map[string]*Entry, len(r.entries))
	for i := range r.entries {
		byID[r.entries[i].ID] = &r.entries[i]
	}

	// Build set of branch entry IDs for O(1) lookup
	onBranch := make(map[string]bool, len(branch))
	for _, id := range branch {
		onBranch[id] = true
	}

	// Find the most recent compaction entry on the active branch
	// Only consider compactions that are on the current branch
	var compactionEntry *Entry
	for i := len(r.entries) - 1; i >= 0; i-- {
		e := &r.entries[i]
		if e.Type == EntryTypeCompaction && e.FirstKeptID != nil {
			// Check if this compaction is on the active branch by checking
			// if first_kept_id is on the branch
			if onBranch[*e.FirstKeptID] {
				compactionEntry = e
				break
			}
			// Also check if the compaction entry itself is on the branch
			if onBranch[e.ID] {
				compactionEntry = e
				break
			}
		}
	}

	// Collect context-eligible entries on the branch after the compaction pointer
	var result []Entry

	// If there's a compaction on the active branch, inject its summary
	var firstKeptID string
	afterCompaction := compactionEntry == nil // true if no compaction on branch
	if compactionEntry != nil {
		firstKeptID = *compactionEntry.FirstKeptID
		result = append(result, *compactionEntry)
	}

	for _, id := range branch {
		if !afterCompaction {
			if id == firstKeptID {
				afterCompaction = true
				// Fall through to include this entry
			} else {
				continue
			}
		}

		e, ok := byID[id]
		if !ok {
			continue
		}

		if e.Type.InContext() {
			result = append(result, *e)
		}
	}

	return result
}

// DisplayName returns the session display name from the most recent
// session_info entry, or an empty string if none was set.
func (r *Reader) DisplayName() string {
	for i := len(r.entries) - 1; i >= 0; i-- {
		if r.entries[i].Type == EntryTypeSessionInfo && r.entries[i].Name != nil {
			return *r.entries[i].Name
		}
	}
	return ""
}

// TotalTokens sums token counts across all message entries.
func (r *Reader) TotalTokens() TokenCounts {
	var total TokenCounts
	for _, e := range r.entries {
		if e.Type == EntryTypeMessage && e.Tokens != nil {
			total.Input += e.Tokens.Input
			total.Output += e.Tokens.Output
			total.CachedInput += e.Tokens.CachedInput
		}
	}
	return total
}

// FirstPrompt returns the first user prompt recorded in the session, or an
// empty string if the session has no user message.
func (r *Reader) FirstPrompt() string {
	for _, e := range r.entries {
		if e.Type != EntryTypeMessage && e.Type != EntryTypeCustomMessage {
			continue
		}
		if prompt := firstPromptFromSummaryMessage(e.Message); prompt != "" {
			return prompt
		}
	}
	return ""
}

// MessageCount returns the number of message entries (regular and custom).
func (r *Reader) MessageCount() int {
	n := 0
	for _, e := range r.entries {
		if e.Type == EntryTypeMessage || e.Type == EntryTypeCustomMessage {
			n++
		}
	}
	return n
}

// FindByID returns an entry by its ID, or nil.
func (r *Reader) FindByID(id string) *Entry {
	for i := range r.entries {
		if r.entries[i].ID == id {
			return &r.entries[i]
		}
	}
	return nil
}

// EntriesByType returns all entries of a given type.
func (r *Reader) EntriesByType(typ EntryType) []Entry {
	var result []Entry
	for _, e := range r.entries {
		if e.Type == typ {
			result = append(result, e)
		}
	}
	return result
}

// CustomEntries returns all custom/custom_message entries, optionally filtered
// by custom_type.
func (r *Reader) CustomEntries(customType string) []Entry {
	var result []Entry
	for _, e := range r.entries {
		if e.Type != EntryTypeCustom && e.Type != EntryTypeCustomMessage {
			continue
		}
		if customType != "" && (e.CustomType == nil || *e.CustomType != customType) {
			continue
		}
		result = append(result, e)
	}
	return result
}

// Tree returns the session as a tree structure. A well-formed session has
// exactly one root (first entry with parentId === nil). Orphaned entries
// (broken parent chain) are also returned as roots.
func (r *Reader) Tree() []*TreeNode {
	// Build label index (entryID -> latest non-empty label)
	labelIndex := make(map[string]labelInfo)
	for _, e := range r.entries {
		if e.Type == EntryTypeLabel && e.TargetID != nil && e.Label != nil && *e.Label != "" {
			t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
			if err != nil {
				continue
			}
			existing := labelIndex[*e.TargetID]
			if existing.timestamp.IsZero() || t.After(existing.timestamp) {
				labelIndex[*e.TargetID] = labelInfo{label: *e.Label, timestamp: t}
			}
		}
	}

	// Create nodes
	nodeMap := make(map[string]*TreeNode, len(r.entries))
	for _, e := range r.entries {
		info := labelIndex[e.ID]
		nodeMap[e.ID] = &TreeNode{
			Entry:          e,
			Children:       nil,
			Label:          info.label,
			LabelTimestamp: info.timestampStr(),
		}
	}

	// Build tree structure
	var roots []*TreeNode
	for _, e := range r.entries {
		node := nodeMap[e.ID]
		if e.ParentID == nil {
			// Root entry
			roots = append(roots, node)
		} else if parent, ok := nodeMap[*e.ParentID]; ok {
			parent.Children = append(parent.Children, node)
		} else {
			// Orphan - treat as root
			roots = append(roots, node)
		}
	}

	// Sort children by timestamp (oldest first)
	sortChildrenRecursive(roots)

	return roots
}

// labelInfo holds the latest label and timestamp for an entry.
type labelInfo struct {
	label     string
	timestamp time.Time
}

func (li labelInfo) timestampStr() string {
	if li.timestamp.IsZero() {
		return ""
	}
	return li.timestamp.Format(time.RFC3339Nano)
}

// sortChildrenRecursive sorts all children arrays by timestamp using iterative DFS.
func sortChildrenRecursive(nodes []*TreeNode) {
	stack := make([]*TreeNode, len(nodes))
	copy(stack, nodes)

	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if len(n.Children) > 1 {
			slices.SortFunc(n.Children, func(a, b *TreeNode) int {
				return cmp.Compare(a.Entry.Timestamp, b.Entry.Timestamp)
			})
		}
		stack = append(stack, n.Children...)
	}
}

// GetChildren returns all direct children of an entry with the given ID.
func (r *Reader) GetChildren(parentID string) []*TreeNode {
	// First, build the tree to get proper label resolution
	tree := r.Tree()

	// Build a flat index for fast lookup
	nodeMap := make(map[string]*TreeNode)
	var collect func(nodes []*TreeNode)
	collect = func(nodes []*TreeNode) {
		for _, n := range nodes {
			nodeMap[n.Entry.ID] = n
			collect(n.Children)
		}
	}
	collect(tree)

	// Find the parent node
	parent, ok := nodeMap[parentID]
	if !ok {
		return nil
	}

	// Return a copy of children
	children := make([]*TreeNode, len(parent.Children))
	copy(children, parent.Children)
	return children
}

// MarshalJSON serializes the full session as a structured JSON document.
// The output includes session metadata, the full tree of entries, the active
// branch entry IDs, and token totals.
func (r *Reader) MarshalJSON() ([]byte, error) {
	doc := struct {
		Session struct {
			ID       string      `json:"id"`
			Name     string      `json:"name"`
			CWD      string      `json:"cwd"`
			Created  string      `json:"created"`
			ParentID *string     `json:"parent_id,omitempty"`
			Tokens   TokenCounts `json:"tokens"`
		} `json:"session"`
		Tree         []Entry  `json:"tree"`
		ActiveBranch []string `json:"active_branch"`
	}{
		Session: struct {
			ID       string      `json:"id"`
			Name     string      `json:"name"`
			CWD      string      `json:"cwd"`
			Created  string      `json:"created"`
			ParentID *string     `json:"parent_id,omitempty"`
			Tokens   TokenCounts `json:"tokens"`
		}{
			ID:       r.header.ID,
			Name:     r.DisplayName(),
			CWD:      r.header.CWD,
			Created:  r.header.Timestamp,
			ParentID: r.header.ParentSession,
			Tokens:   r.TotalTokens(),
		},
		Tree:         r.entries,
		ActiveBranch: r.ActiveBranch(),
	}
	return json.MarshalIndent(doc, "", "  ")
}
