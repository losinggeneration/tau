// Package session implements the append-only JSONL session format with a
// tree-of-entries model, flock-based concurrency, and index management.
package session

import (
	"encoding/json"
	"time"

	"github.com/losinggeneration/tau/ai"
)

// ToAgentMessage converts a session Entry to an agent.Message.
// It returns nil if the entry is not a message entry or cannot be parsed.
// For custom_message entries, the entry's Message field contains the content
// to inject into the LLM context.
func (e *Entry) ToAgentMessage() *Message {
	if e.Type == EntryTypeCompaction {
		if e.Summary == nil || *e.Summary == "" {
			return nil
		}
		ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		if err != nil {
			ts = time.Time{}
		}
		return &Message{
			Role:      MessageRoleSystem,
			Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "Session Summary: " + *e.Summary}},
			Timestamp: ts,
		}
	}
	if e.Type != EntryTypeMessage && e.Type != EntryTypeCustomMessage {
		return nil
	}

	// Handle custom_message entries - their content is in the Message field
	var raw struct {
		Role          string          `json:"role"`
		Content       json.RawMessage `json:"content"`
		ToolCallID    string          `json:"toolCallId"`
		ToolCallID2   string          `json:"tool_call_id"`
		ToolName      string          `json:"toolName"`
		ToolName2     string          `json:"tool_name"`
		IsError       bool            `json:"isError"`
		IsError2      bool            `json:"is_error"`
		Provider      string          `json:"provider"`
		API           string          `json:"api"`
		Model         string          `json:"model"`
		StopReason    string          `json:"stopReason"`
		StopReason2   string          `json:"stop_reason"`
		ErrorMessage  string          `json:"errorMessage"`
		ErrorMessage2 string          `json:"error_message"`
		ResponseID    string          `json:"responseId"`
		ResponseID2   string          `json:"response_id"`
	}

	// For custom_message entries, the message field contains the content
	if e.Type == EntryTypeCustomMessage && e.Message != nil {
		if err := json.Unmarshal(e.Message, &raw); err != nil {
			return nil
		}
	} else if e.Type == EntryTypeMessage && e.Message != nil {
		// For regular message entries, parse the message field
		if err := json.Unmarshal(e.Message, &raw); err != nil {
			return nil
		}
	} else {
		return nil
	}

	var content []ai.ContentBlock
	if raw.Content != nil {
		// Try to parse as []ContentBlock first
		if err := json.Unmarshal(raw.Content, &content); err != nil {
			// Fall back to treating it as plain text
			var text string
			if err := json.Unmarshal(raw.Content, &text); err == nil {
				content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: text}}
			}
		}
	}

	if raw.ToolCallID == "" {
		raw.ToolCallID = raw.ToolCallID2
	}
	if raw.ToolName == "" {
		raw.ToolName = raw.ToolName2
	}
	if !raw.IsError {
		raw.IsError = raw.IsError2
	}
	if raw.StopReason == "" {
		raw.StopReason = raw.StopReason2
	}
	if raw.ErrorMessage == "" {
		raw.ErrorMessage = raw.ErrorMessage2
	}
	if raw.ResponseID == "" {
		raw.ResponseID = raw.ResponseID2
	}

	ts, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
	return &Message{
		Role:         MessageRole(raw.Role),
		Content:      content,
		ToolCallID:   raw.ToolCallID,
		ToolName:     raw.ToolName,
		IsError:      raw.IsError,
		Provider:     raw.Provider,
		API:          raw.API,
		Model:        raw.Model,
		StopReason:   raw.StopReason,
		ErrorMessage: raw.ErrorMessage,
		ResponseID:   raw.ResponseID,
		Timestamp:    ts,
	}
}

// Message is an alias for agent.Message used internally for session conversion.
type Message = agentMessage

// agentMessage represents a message in the session, matching agent.Message structure.
type agentMessage struct {
	Role         MessageRole
	Content      []ai.ContentBlock
	ToolCallID   string
	ToolName     string
	IsError      bool
	Provider     string
	API          string
	Model        string
	StopReason   string
	ErrorMessage string
	ResponseID   string
	Timestamp    time.Time
}

// MessageRole represents the role of a message.
type MessageRole string

const (
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleTool      MessageRole = "tool"
	MessageRoleSystem    MessageRole = "system"
)
