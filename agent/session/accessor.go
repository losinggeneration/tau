// Package session provides the session storage and accessors for tau.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/losinggeneration/tau/agent/plugins"
)

// Compile-time check that SessionStoreAccessor implements plugins.SessionAccessor.
var _ plugins.SessionAccessor = (*SessionStoreAccessor)(nil)

// SessionStoreAccessor implements plugins.SessionAccessor by delegating to
// session storage. It is used by the plugin Host service to fulfill
// SessionAppend and SessionScan RPC calls.
type SessionStoreAccessor struct {
	storage *Storage
}

// NewSessionStoreAccessor creates a SessionStoreAccessor.
func NewSessionStoreAccessor(storage *Storage) *SessionStoreAccessor {
	return &SessionStoreAccessor{storage: storage}
}

// AppendEntry appends a custom or custom_message entry to the active session.
// Returns the new entry ID or an error.
func (s *SessionStoreAccessor) AppendEntry(ctx context.Context, sessionID string, entryType string, payload []byte, parentID *string) (_ string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	w, err := s.storage.Resume(sessionID)
	if err != nil {
		return "", fmt.Errorf("resume session: %w", err)
	}
	defer func() {
		if closeErr := w.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close session writer: %w", closeErr))
		}
	}()

	var entry Entry
	switch entryType {
	case "custom":
		var data any
		if err := json.Unmarshal(payload, &data); err != nil {
			return "", fmt.Errorf("unmarshal custom data: %w", err)
		}
		dataJSON, _ := json.Marshal(data)
		customType := ""
		if m, ok := data.(map[string]any); ok {
			if ct, ok := m["custom_type"].(string); ok {
				customType = ct
			}
		}
		entry = NewCustomEntry(customType, dataJSON)
	case "custom_message":
		var data any
		if err := json.Unmarshal(payload, &data); err != nil {
			return "", fmt.Errorf("unmarshal custom_message data: %w", err)
		}
		dataJSON, _ := json.Marshal(data)
		customType := ""
		var msg json.RawMessage
		if m, ok := data.(map[string]any); ok {
			if ct, ok := m["custom_type"].(string); ok {
				customType = ct
			}
			if msgRaw, ok := m["message"]; ok {
				msg, _ = json.Marshal(msgRaw)
			}
		}
		entry = NewCustomMessageEntry(customType, dataJSON, msg)
	default:
		return "", fmt.Errorf("unsupported entry type: %s (must be 'custom' or 'custom_message')", entryType)
	}

	// Override the parent ID if provided
	if parentID != nil {
		entry.ParentID = parentID
	}

	if err := w.Append(entry); err != nil {
		return "", fmt.Errorf("append entry: %w", err)
	}

	return entry.ID, nil
}

// ScanEntries returns entries from the active session matching the filters.
func (s *SessionStoreAccessor) ScanEntries(ctx context.Context, sessionID string, entryType string, customType string) ([]plugins.SessionEntry, error) {
	r, err := s.storage.Open(sessionID)
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}

	var entries []Entry
	if entryType != "" {
		entries = r.EntriesByType(EntryType(entryType))
	} else {
		entries = r.Entries()
	}

	// Filter by custom_type if specified
	if customType != "" {
		var filtered []Entry
		for _, e := range entries {
			if e.Type == EntryTypeCustom || e.Type == EntryTypeCustomMessage {
				if e.CustomType != nil && *e.CustomType == customType {
					filtered = append(filtered, e)
				}
			}
		}
		entries = filtered
	}

	result := make([]plugins.SessionEntry, len(entries))
	for i, e := range entries {
		entryTypeStr := string(e.Type)
		customTypeStr := ""
		if e.CustomType != nil {
			customTypeStr = *e.CustomType
		}
		// Use the correct field based on entry type:
		// - message entries store content in Message
		// - custom/custom_message entries store data in Data
		var payload []byte
		switch e.Type {
		case EntryTypeMessage:
			payload = e.Message
		default:
			payload = e.Data
		}
		result[i] = plugins.SessionEntry{
			ID:         e.ID,
			EntryType:  entryTypeStr,
			CustomType: customTypeStr,
			Payload:    payload,
			ParentID:   e.ParentID,
		}
	}

	return result, nil
}

// GetLabel returns the label for an entry, or empty string if none.
func (s *SessionStoreAccessor) GetLabel(ctx context.Context, sessionID string, entryID string) (string, error) {
	r, err := s.storage.Open(sessionID)
	if err != nil {
		return "", fmt.Errorf("open session: %w", err)
	}

	// Find all label entries targeting this entry
	labelEntries := r.EntriesByType(EntryTypeLabel)
	var latestLabel string
	var latestTime time.Time

	for _, e := range labelEntries {
		if e.TargetID != nil && *e.TargetID == entryID {
			if e.Label != nil && *e.Label != "" {
				// Parse timestamp for comparison
				t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
				if err != nil {
					continue
				}
				// Keep the most recent non-empty label
				if latestLabel == "" || t.After(latestTime) {
					latestLabel = *e.Label
					latestTime = t
				}
			}
		}
	}

	return latestLabel, nil
}

// SetLabel sets or clears a label on an entry.
// Pass empty string to clear the label.
// Returns the new label entry ID or an error.
func (s *SessionStoreAccessor) SetLabel(ctx context.Context, sessionID string, entryID string, label string) (_ string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// First, verify the target entry exists
	r, err := s.storage.Open(sessionID)
	if err != nil {
		return "", fmt.Errorf("open session: %w", err)
	}

	targetEntry := r.FindByID(entryID)
	if targetEntry == nil {
		return "", fmt.Errorf("entry %q not found", entryID)
	}

	// Now resume for appending
	w, err := s.storage.Resume(sessionID)
	if err != nil {
		return "", fmt.Errorf("resume session: %w", err)
	}
	defer func() {
		if closeErr := w.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close session writer: %w", closeErr))
		}
	}()

	entry := NewLabelEntry(entryID, label)
	if err := w.Append(entry); err != nil {
		return "", fmt.Errorf("append label entry: %w", err)
	}

	return entry.ID, nil
}

// entryToSessionEntry converts an Entry to a plugins.SessionEntry.
func entryToSessionEntry(e Entry) plugins.SessionEntry {
	entryTypeStr := string(e.Type)
	customTypeStr := ""
	if e.CustomType != nil {
		customTypeStr = *e.CustomType
	}
	var payload []byte
	switch e.Type {
	case EntryTypeMessage:
		payload = e.Message
	default:
		payload = e.Data
	}
	return plugins.SessionEntry{
		ID:         e.ID,
		EntryType:  entryTypeStr,
		CustomType: customTypeStr,
		Payload:    payload,
		ParentID:   e.ParentID,
	}
}

// treeNodeToPluginNode converts a TreeNode to a plugins.SessionTreeNode.
func treeNodeToPluginNode(node *TreeNode) plugins.SessionTreeNode {
	children := make([]plugins.SessionTreeNode, len(node.Children))
	for i, child := range node.Children {
		children[i] = treeNodeToPluginNode(child)
	}
	return plugins.SessionTreeNode{
		Entry:          entryToSessionEntry(node.Entry),
		Children:       children,
		Label:          node.Label,
		LabelTimestamp: node.LabelTimestamp,
	}
}

// GetTree returns the session tree structure.
func (s *SessionStoreAccessor) GetTree(ctx context.Context, sessionID string) ([]plugins.SessionTreeNode, error) {
	r, err := s.storage.Open(sessionID)
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}

	tree := r.Tree()
	result := make([]plugins.SessionTreeNode, len(tree))
	for i, node := range tree {
		result[i] = treeNodeToPluginNode(node)
	}

	return result, nil
}

// GetChildren returns direct children of an entry.
func (s *SessionStoreAccessor) GetChildren(ctx context.Context, sessionID string, parentID string) ([]plugins.SessionTreeNode, error) {
	r, err := s.storage.Open(sessionID)
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}

	children := r.GetChildren(parentID)
	if children == nil {
		return nil, nil
	}

	result := make([]plugins.SessionTreeNode, len(children))
	for i, child := range children {
		result[i] = treeNodeToPluginNode(child)
	}

	return result, nil
}

// GetHeader returns the session header (id, name, cwd, timestamp).
func (s *SessionStoreAccessor) GetHeader(ctx context.Context, sessionID string) (plugins.SessionHeader, error) {
	r, err := s.storage.Open(sessionID)
	if err != nil {
		return plugins.SessionHeader{}, fmt.Errorf("open session: %w", err)
	}

	header := r.Header()
	return plugins.SessionHeader{
		ID:        header.ID,
		Name:      header.Name,
		CWD:       header.CWD,
		Timestamp: header.Timestamp,
	}, nil
}

// GetSessionName returns the session display name.
func (s *SessionStoreAccessor) GetSessionName(ctx context.Context, sessionID string) (string, error) {
	r, err := s.storage.Open(sessionID)
	if err != nil {
		return "", fmt.Errorf("open session: %w", err)
	}

	return r.DisplayName(), nil
}

// Branch moves the current position to a different entry by appending a
// position_change entry. New entries will be children of that entry.
// This is equivalent to TypeScript's SessionManager.branch().
func (s *SessionStoreAccessor) Branch(ctx context.Context, sessionID string, branchFromID string) error {
	return s.storage.BranchTo(sessionID, branchFromID)
}

// GetBranch returns all entries from the given entry back to root,
// in chronological order. This is equivalent to TypeScript's
// SessionManager.getBranch().
func (s *SessionStoreAccessor) GetBranch(ctx context.Context, sessionID string, fromID string) ([]plugins.SessionEntry, error) {
	entries, err := s.storage.GetBranchEntries(sessionID, fromID)
	if err != nil {
		return nil, fmt.Errorf("get branch entries: %w", err)
	}

	result := make([]plugins.SessionEntry, len(entries))
	for i, e := range entries {
		entryTypeStr := string(e.Type)
		customTypeStr := ""
		if e.CustomType != nil {
			customTypeStr = *e.CustomType
		}
		var payload []byte
		switch e.Type {
		case EntryTypeMessage:
			payload = e.Message
		default:
			payload = e.Data
		}
		result[i] = plugins.SessionEntry{
			ID:         e.ID,
			EntryType:  entryTypeStr,
			CustomType: customTypeStr,
			Payload:    payload,
			ParentID:   e.ParentID,
		}
	}

	return result, nil
}
