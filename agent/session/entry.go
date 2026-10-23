// Package session implements the append-only JSONL session format with a
// tree-of-entries model, flock-based concurrency, and index management.
package session

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/losinggeneration/tau/ai"
)

// FormatVersion is the current session file format version.
const FormatVersion = CurrentSessionVersion

// EntryType enumerates the kinds of entries in a session JSONL file.
type EntryType string

const (
	EntryTypeContextEdit    EntryType = "context_edit"
	EntryTypeSession        EntryType = "session"
	EntryTypeMessage        EntryType = "message"
	EntryTypePositionChange EntryType = "position_change"
	EntryTypeCompaction     EntryType = "compaction"
	EntryTypeBranchSummary  EntryType = "branch_summary"
	EntryTypeSessionInfo    EntryType = "session_info"
	EntryTypeModelChange    EntryType = "model_change"
	EntryTypeThinkingChange EntryType = "thinking_level_change"
	EntryTypeLabel          EntryType = "label"
	EntryTypeError          EntryType = "error"
	EntryTypeCustom         EntryType = "custom"
	EntryTypeCustomMessage  EntryType = "custom_message"
)

// EntryStatusDeleted marks an entry dropped by /forget. Readers skip it, so
// it never reaches context, display, export, or forks.
const EntryStatusDeleted = "deleted"

// InContext returns true if the entry type is included in LLM context.
func (t EntryType) InContext() bool {
	switch t {
	case EntryTypeMessage, EntryTypeCompaction, EntryTypeCustomMessage, EntryTypeBranchSummary:
		return true
	default:
		return false
	}
}

// TokenCounts records token usage for a message.
type TokenCounts struct {
	Input       int `json:"input,omitempty"`
	Output      int `json:"output,omitempty"`
	CachedInput int `json:"cached_input,omitempty"`
}

// Total returns the sum of Input, Output, and CachedInput tokens.
func (tc TokenCounts) Total() int {
	return tc.Input + tc.Output + tc.CachedInput
}

// Header is the first line of every session file.
type Header struct {
	Type          EntryType `json:"type"`
	Version       int       `json:"version"`
	ID            string    `json:"id"`
	Name          string    `json:"name,omitempty"`
	Timestamp     string    `json:"timestamp"`
	CWD           string    `json:"cwd"`
	ParentSession *string   `json:"parent_session,omitempty"`
	ParentEntry   *string   `json:"parent_entry,omitempty"`
}

// Entry is the common envelope for all session entries after the header.
type Entry struct {
	Replacement json.RawMessage `json:"replacement,omitempty"`
	Type        EntryType       `json:"type"`
	ID          string          `json:"id"`
	ParentID    *string         `json:"parent_id,omitempty"`
	Timestamp   string          `json:"timestamp"`
	Status      string          `json:"status,omitempty"`

	// Message fields (EntryTypeMessage)
	Message json.RawMessage `json:"message,omitempty"`
	Tokens  *TokenCounts    `json:"tokens,omitempty"`

	// PositionChange fields
	NewPosition *string `json:"new_position,omitempty"`

	// Compaction fields
	Summary       *string `json:"summary,omitempty"`
	FirstKeptID   *string `json:"first_kept_entry_id,omitempty"`
	TokensBefore  *int    `json:"tokens_before,omitempty"`
	TokensAfter   *int    `json:"tokens_after,omitempty"`
	FromHook      bool    `json:"from_hook,omitempty"`
	Fallback      bool    `json:"fallback,omitempty"`
	FallbackError string  `json:"fallback_error,omitempty"`

	// BranchSummary fields
	FromID *string `json:"from_id,omitempty"`

	// SessionInfo fields
	Name *string `json:"name,omitempty"`

	// ModelChange / Error fields
	Provider *string `json:"provider,omitempty"`
	Model    *string `json:"model,omitempty"`

	// ThinkingLevelChange fields
	Level *string `json:"level,omitempty"`

	// Label fields
	TargetID *string `json:"target_id,omitempty"`
	Label    *string `json:"label,omitempty"`

	// QueueUpdate fields
	SteeringCount int `json:"steering_count,omitempty"`
	FollowUpCount int `json:"follow_up_count,omitempty"`

	// CompactionLifecycle / Error fields
	Aborted      bool   `json:"aborted,omitempty"`
	ErrorMessage string `json:"error,omitempty"`

	// Custom / CustomMessage / Error fields
	CustomType *string         `json:"custom_type,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// NewID generates a new UUIDv7 entry ID.
func NewID() string {
	return uuid.Must(uuid.NewV7()).String()
}

// NewTimestamp returns an RFC3339 timestamp for now.
func NewTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// NewHeader creates a session header with an optional display name.
// If name is empty and a displayName is provided, the slug will be derived
// from the first user message later via RenameSession.
func NewHeader(cwd string, parentSession, parentEntry *string, name ...string) Header {
	h := Header{
		Type:          EntryTypeSession,
		Version:       FormatVersion,
		ID:            NewID(),
		Timestamp:     NewTimestamp(),
		CWD:           cwd,
		ParentSession: parentSession,
		ParentEntry:   parentEntry,
	}
	if len(name) > 0 {
		h.Name = name[0]
	}
	return h
}

// NewEntry creates a base entry with type, id, and timestamp.
func NewEntry(typ EntryType, parentID *string) Entry {
	return Entry{
		Type:      typ,
		ID:        NewID(),
		ParentID:  parentID,
		Timestamp: NewTimestamp(),
	}
}

// NewMessageEntry creates a message entry.
func NewMessageEntry(parentID *string, msg json.RawMessage, tokens *TokenCounts) Entry {
	e := NewEntry(EntryTypeMessage, parentID)
	e.Message = msg
	e.Tokens = tokens
	return e
}

// NewPositionChangeEntry creates a position_change entry.
func NewPositionChangeEntry(newPos string) Entry {
	e := NewEntry(EntryTypePositionChange, nil)
	e.NewPosition = &newPos
	return e
}

// NewCompactionEntry creates a compaction entry.
func NewCompactionEntry(summary string, firstKeptID string, tokensBefore, tokensAfter int, fromHook bool) Entry {
	e := NewEntry(EntryTypeCompaction, nil)
	e.Summary = &summary
	e.FirstKeptID = &firstKeptID
	e.TokensBefore = &tokensBefore
	e.TokensAfter = &tokensAfter
	e.FromHook = fromHook
	return e
}

// NewBranchSummaryEntry creates a branch_summary entry.
func NewBranchSummaryEntry(fromID, summary string, fromHook bool) Entry {
	e := NewEntry(EntryTypeBranchSummary, nil)
	e.FromID = &fromID
	e.Summary = &summary
	e.FromHook = fromHook
	return e
}

// NewSessionInfoEntry creates a session_info entry.
func NewSessionInfoEntry(name string) Entry {
	e := NewEntry(EntryTypeSessionInfo, nil)
	e.Name = &name
	return e
}

// NewModelChangeEntry creates a model_change entry.
func NewModelChangeEntry(provider, model string) Entry {
	e := NewEntry(EntryTypeModelChange, nil)
	e.Provider = &provider
	e.Model = &model
	return e
}

// NewErrorEntry creates an error entry recording a failed provider request.
// Error entries are session-log diagnostics: they are excluded from LLM
// context, so replay never feeds the failure back to the model, but keep the
// provider, model, error text, and (in Data) a summary of the rejected
// request shape for troubleshooting.
func NewErrorEntry(provider, model, message string, data json.RawMessage) Entry {
	e := NewEntry(EntryTypeError, nil)
	e.Provider = &provider
	e.Model = &model
	e.ErrorMessage = message
	e.Data = data
	return e
}

// NewThinkingLevelChangeEntry creates a thinking_level_change entry.
func NewThinkingLevelChangeEntry(level string) Entry {
	e := NewEntry(EntryTypeThinkingChange, nil)
	e.Level = &level
	return e
}

// NewLabelEntry creates a label entry for user-defined bookmarks/markers.
// The targetID specifies which entry the label is attached to.
// Pass nil or empty string for label to clear the label.
func NewLabelEntry(targetID string, label string) Entry {
	e := NewEntry(EntryTypeLabel, nil)
	e.TargetID = &targetID
	e.Label = &label
	return e
}

// NewCustomEntry creates a custom entry (hidden from LLM context).
func NewCustomEntry(customType string, data json.RawMessage) Entry {
	e := NewEntry(EntryTypeCustom, nil)
	e.CustomType = &customType
	e.Data = data
	return e
}

// SummaryUsageType is the custom entry type that records billed usage of a
// compaction or branch summary request. It hangs off the branch position the
// summary was requested at, so forks of that branch keep it.
const SummaryUsageType = "summary_usage"

// NewSummaryUsageEntry records billed summary usage as a hidden child of parentID.
func NewSummaryUsageEntry(parentID *string, usage ai.Usage) (Entry, error) {
	data, err := json.Marshal(NormalizeUsage(usage))
	if err != nil {
		return Entry{}, fmt.Errorf("marshal summary usage: %w", err)
	}
	e := NewCustomEntry(SummaryUsageType, data)
	e.ParentID = parentID
	return e, nil
}

// NormalizeUsage fills the token and cost totals from their parts when a
// provider reported only the breakdown.
func NormalizeUsage(usage ai.Usage) ai.Usage {
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
	}
	if usage.Cost.Total == 0 {
		usage.Cost.Total = usage.Cost.Input + usage.Cost.Output + usage.Cost.CacheRead + usage.Cost.CacheWrite
	}
	return usage
}

// NewCustomMessageEntry creates a custom_message entry (injected into context).
func NewCustomMessageEntry(customType string, data json.RawMessage, msg json.RawMessage) Entry {
	e := NewEntry(EntryTypeCustomMessage, nil)
	e.CustomType = &customType
	e.Data = data
	e.Message = msg
	return e
}

// MarshalEntry serializes an entry or header to a JSON line.
func MarshalEntry(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal entry: %w", err)
	}
	return data, nil
}

// ParseHeader parses the first line of a session file as a Header.
func ParseHeader(line []byte) (Header, error) {
	var h Header
	if err := json.Unmarshal(line, &h); err != nil {
		return Header{}, fmt.Errorf("parse header: %w", err)
	}
	if h.Type != EntryTypeSession {
		return Header{}, fmt.Errorf("first line is not a session header, got type %q", h.Type)
	}
	return h, nil
}

// ParseEntry parses a single JSONL line as an Entry.
func ParseEntry(line []byte) (Entry, error) {
	var e Entry
	if err := json.Unmarshal(line, &e); err != nil {
		return Entry{}, fmt.Errorf("parse entry: %w", err)
	}
	if e.Type == EntryTypeContextEdit {
		if e.TargetID == nil || *e.TargetID == "" {
			return Entry{}, fmt.Errorf("context edit requires targetId")
		}
		normalized, err := normalizeContextReplacement(e.Replacement)
		if err != nil {
			return Entry{}, err
		}
		e.Replacement = normalized
	}
	return e, nil
}

func normalizeContextReplacement(replacement json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(replacement, &value); err != nil {
		return nil, fmt.Errorf("parse context replacement: %w", err)
	}
	if value == nil {
		return json.RawMessage("null"), nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("context replacement must be null or contain string/array content")
	}
	switch object["content"].(type) {
	case string, []any:
		data, err := json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("marshal context replacement: %w", err)
		}
		return data, nil
	default:
		return nil, fmt.Errorf("context replacement must contain string/array content")
	}
}
