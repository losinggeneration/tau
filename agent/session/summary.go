package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// ReadSummary reads the session metadata needed for listings without building a
// full in-memory Reader. It streams JSON values from the JSONL file and ignores
// large payload fields such as message bodies.
func ReadSummary(path string) (summary SessionSummary, err error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionSummary{}, fmt.Errorf("open session: %w", err)
	}

	locked := false
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

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return SessionSummary{}, fmt.Errorf("lock session for read: %w", err)
	}
	locked = true

	dec := json.NewDecoder(f)
	var header Header
	if err := dec.Decode(&header); err != nil {
		return SessionSummary{}, fmt.Errorf("decode header: %w", err)
	}
	if header.Type != EntryTypeSession {
		return SessionSummary{}, fmt.Errorf("first line is not a session header, got type %q", header.Type)
	}

	version := header.Version
	if version == 0 {
		version = 1
	}
	if version < CurrentSessionVersion {
		locked = false
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			return SessionSummary{}, fmt.Errorf("unlock session before migration read: %w", err)
		}
		if err := f.Close(); err != nil {
			return SessionSummary{}, fmt.Errorf("close session before migration read: %w", err)
		}
		closed = true

		reader, err := ReadFile(path)
		if err != nil {
			return SessionSummary{}, err
		}
		return summaryFromReader(path, reader), nil
	}

	summary = SessionSummary{
		ID:           header.ID,
		Name:         header.Name,
		Filename:     filepath.Base(path),
		CWD:          header.CWD,
		Created:      header.Timestamp,
		LastModified: header.Timestamp,
		Status:       "waiting",
		ParentID:     header.ParentSession,
	}

	for {
		var entry summaryEntry
		if err := dec.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return SessionSummary{}, fmt.Errorf("decode summary entry: %w", err)
		}
		summary.LastModified = newerTimestamp(summary.LastModified, entry.Timestamp)
		switch entry.Type {
		case EntryTypeMessage, EntryTypeCustomMessage:
			summary.MessageCount++
			if summary.FirstPrompt == "" {
				summary.FirstPrompt = firstPromptFromSummaryMessage(entry.Message)
			}
			if msgStatus := messageStatus(entry.Message); msgStatus != "" {
				summary.Status = msgStatus
			}
			if entry.Tokens != nil {
				summary.Tokens.Input += entry.Tokens.Input
				summary.Tokens.Output += entry.Tokens.Output
				summary.Tokens.CachedInput += entry.Tokens.CachedInput
			}
		case EntryTypeSessionInfo:
			if entry.Name != nil {
				summary.Name = *entry.Name
			}
		case EntryTypeModelChange:
			summary.Model = modelSpec(entry.Provider, entry.Model)
		case EntryType("queue_update"):
			if (entry.SteeringCount + entry.FollowUpCount) > 0 {
				summary.Status = "queued"
			} else if summary.Status == "queued" {
				summary.Status = "waiting"
			}
		case EntryType("compaction_lifecycle"):
			summary.Status = compactionLifecycleStatus(entry)
		}
	}

	return summary, nil
}

type summaryEntry struct {
	Type          EntryType       `json:"type"`
	Timestamp     string          `json:"timestamp,omitempty"`
	Name          *string         `json:"name,omitempty"`
	Tokens        *TokenCounts    `json:"tokens,omitempty"`
	Message       json.RawMessage `json:"message,omitempty"`
	Provider      *string         `json:"provider,omitempty"`
	Model         *string         `json:"model,omitempty"`
	SteeringCount int             `json:"steering_count,omitempty"`
	FollowUpCount int             `json:"follow_up_count,omitempty"`
	Aborted       bool            `json:"aborted,omitempty"`
	ErrorMessage  string          `json:"error,omitempty"`
}

func summaryFromReader(path string, reader *Reader) SessionSummary {
	summary := SessionSummary{
		ID:           reader.header.ID,
		Name:         reader.DisplayName(),
		Filename:     filepath.Base(path),
		CWD:          reader.header.CWD,
		Created:      reader.header.Timestamp,
		LastModified: reader.header.Timestamp,
		Status:       "waiting",
		MessageCount: reader.MessageCount(),
		Tokens:       reader.TotalTokens(),
		ParentID:     reader.header.ParentSession,
		FirstPrompt:  reader.FirstPrompt(),
	}
	for _, entry := range reader.Entries() {
		summary.LastModified = newerTimestamp(summary.LastModified, entry.Timestamp)
		switch entry.Type {
		case EntryTypeMessage, EntryTypeCustomMessage:
			if msg := entry.ToAgentMessage(); msg != nil {
				if msgStatus := agentMessageStatus(msg.StopReason, msg.ErrorMessage, msg.IsError); msgStatus != "" {
					summary.Status = msgStatus
				}
			}
		case EntryTypeModelChange:
			summary.Model = modelSpec(entry.Provider, entry.Model)
		case EntryType("queue_update"):
			if entry.SteeringCount+entry.FollowUpCount > 0 {
				summary.Status = "queued"
			} else if summary.Status == "queued" {
				summary.Status = "waiting"
			}
		case EntryType("compaction_lifecycle"):
			summary.Status = compactionLifecycleStatus(summaryEntry{Aborted: entry.Aborted, ErrorMessage: entry.ErrorMessage})
		}
	}
	return summary
}

func messageStatus(raw json.RawMessage) string {
	var msg struct {
		StopReason    string `json:"stopReason"`
		StopReason2   string `json:"stop_reason"`
		ErrorMessage  string `json:"errorMessage"`
		ErrorMessage2 string `json:"error_message"`
		IsError       bool   `json:"isError"`
		IsError2      bool   `json:"is_error"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &msg) != nil {
		return ""
	}
	if msg.StopReason == "" {
		msg.StopReason = msg.StopReason2
	}
	if msg.ErrorMessage == "" {
		msg.ErrorMessage = msg.ErrorMessage2
	}
	return agentMessageStatus(msg.StopReason, msg.ErrorMessage, msg.IsError || msg.IsError2)
}

func agentMessageStatus(stopReason, errorMessage string, isError bool) string {
	if stopReason == "aborted" {
		return "aborted"
	}
	if stopReason == "error" || errorMessage != "" || isError {
		return "errored"
	}
	// A persisted assistant message with a normal stop reason means its turn
	// finished; sessions whose last turn completed cleanly show "complete"
	// rather than the "waiting" default. Empty stop reasons (legacy files,
	// user messages) leave the running status untouched.
	switch stopReason {
	case "stop", "length", "toolUse":
		return "complete"
	}
	return ""
}
