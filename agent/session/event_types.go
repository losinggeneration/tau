// Package session implements the append-only JSONL session format with a
// tree-of-entries model, flock-based concurrency, and index management.
package session

// QueueUpdateEntry records a queue state change.
type QueueUpdateEntry struct {
	Entry
	SteeringCount int `json:"steering_count"`
	FollowUpCount int `json:"follow_up_count"`
}

// CompactionLifecycleEntry records compaction lifecycle events.
type CompactionLifecycleEntry struct {
	Entry
	Reason       string `json:"reason"`
	Aborted      bool   `json:"aborted"`
	WillRetry    bool   `json:"will_retry"`
	ErrorMessage string `json:"error,omitempty"`
}

// NewQueueUpdateEntry creates a queue_update entry.
func NewQueueUpdateEntry(steeringCount, followUpCount int) QueueUpdateEntry {
	e := NewEntry(EntryType("queue_update"), nil)
	e.SteeringCount = steeringCount
	e.FollowUpCount = followUpCount
	return QueueUpdateEntry{Entry: e, SteeringCount: steeringCount, FollowUpCount: followUpCount}
}

// NewCompactionLifecycleEntry creates a compaction lifecycle entry.
func NewCompactionLifecycleEntry(reason string, aborted, willRetry bool, errMsg string) CompactionLifecycleEntry {
	e := NewEntry(EntryType("compaction_lifecycle"), nil)
	e.Aborted = aborted
	e.ErrorMessage = errMsg
	return CompactionLifecycleEntry{Entry: e, Reason: reason, Aborted: aborted, WillRetry: willRetry, ErrorMessage: errMsg}
}
