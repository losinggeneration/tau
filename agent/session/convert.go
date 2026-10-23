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
	if e.Type == EntryTypeCompaction || e.Type == EntryTypeBranchSummary {
		if e.Summary == nil || *e.Summary == "" {
			return nil
		}
		ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		if err != nil {
			ts = time.Time{}
		}
		role := MessageRoleSystem
		text := "Session Summary: " + *e.Summary
		if e.Type == EntryTypeBranchSummary {
			role = MessageRoleUser
			text = "The following is a summary of a branch that this conversation came back from:\n\n<summary>\n" + *e.Summary + "</summary>"
		}
		return &Message{
			Role:      role,
			Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: text}},
			Timestamp: ts,
		}
	}
	if e.Type != EntryTypeMessage && e.Type != EntryTypeCustomMessage {
		return nil
	}

	// Handle custom_message entries - their content is in the Message field
	var raw struct {
		ExcludeFromContext    bool                     `json:"excludeFromContext"`
		Timestamp             json.RawMessage          `json:"timestamp"`
		CustomType            string                   `json:"customType"`
		Display               bool                     `json:"display"`
		Details               any                      `json:"details"`
		Diagnostics           []ai.AssistantDiagnostic `json:"diagnostics"`
		Data                  any                      `json:"data"`
		StructuredContent     json.RawMessage          `json:"structuredContent"`
		NestedCalls           []ai.NestedToolCall      `json:"nestedCalls"`
		ToolUsage             *ai.Usage                `json:"toolUsage"`
		Sections              map[string]*string       `json:"sections"`
		ToolsAdded            []ai.Tool                `json:"toolsAdded"`
		ToolsRemoved          []ai.ToolReference       `json:"toolsRemoved"`
		ThinkingLevel         string                   `json:"thinkingLevel"`
		DurationMs            *float64                 `json:"durationMs"`
		Role                  string                   `json:"role"`
		Usage                 ai.Usage                 `json:"usage"`
		Content               json.RawMessage          `json:"content"`
		ToolCallID            string                   `json:"toolCallId"`
		ToolCallID2           string                   `json:"tool_call_id"`
		ToolName              string                   `json:"toolName"`
		ToolName2             string                   `json:"tool_name"`
		IsError               bool                     `json:"isError"`
		IsError2              bool                     `json:"is_error"`
		Provider              string                   `json:"provider"`
		API                   string                   `json:"api"`
		Model                 string                   `json:"model"`
		StopReason            string                   `json:"stopReason"`
		StopReason2           string                   `json:"stop_reason"`
		ErrorMessage          string                   `json:"errorMessage"`
		ErrorMessage2         string                   `json:"error_message"`
		ResponseID            string                   `json:"responseId"`
		ResponseID2           string                   `json:"response_id"`
		ProviderThinkingLevel string                   `json:"providerThinkingLevel"`
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

	if raw.Role == "toolResult" {
		raw.Role = string(MessageRoleTool)
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

	var ts time.Time
	if len(raw.Timestamp) > 0 && string(raw.Timestamp) != "null" {
		if raw.Timestamp[0] == '"' {
			if err := json.Unmarshal(raw.Timestamp, &ts); err != nil {
				return nil
			}
		} else {
			var milliseconds int64
			if err := json.Unmarshal(raw.Timestamp, &milliseconds); err != nil {
				return nil
			}
			ts = time.UnixMilli(milliseconds)
		}
	}
	if ts.IsZero() {
		parsed, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		if err == nil {
			ts = parsed
		}
	}
	return &Message{
		CustomType: raw.CustomType, Display: raw.Display, Details: raw.Details,
		Sections: raw.Sections, ToolsAdded: raw.ToolsAdded, ToolsRemoved: raw.ToolsRemoved,
		Data:              raw.Data,
		StructuredContent: raw.StructuredContent, ToolUsage: raw.ToolUsage, NestedCalls: raw.NestedCalls,
		ExcludeFromContext:    raw.ExcludeFromContext,
		Role:                  MessageRole(raw.Role),
		Usage:                 raw.Usage,
		Content:               content,
		ToolCallID:            raw.ToolCallID,
		ToolName:              raw.ToolName,
		IsError:               raw.IsError,
		Provider:              raw.Provider,
		API:                   raw.API,
		Model:                 raw.Model,
		StopReason:            raw.StopReason,
		ErrorMessage:          raw.ErrorMessage,
		ResponseID:            raw.ResponseID,
		ProviderThinkingLevel: raw.ProviderThinkingLevel,
		Diagnostics:           raw.Diagnostics,
		DurationMs:            raw.DurationMs,
		ThinkingLevel:         raw.ThinkingLevel,
		Timestamp:             ts,
	}
}

// Message is an alias for agent.Message used internally for session conversion.
type Message = agentMessage

// agentMessage represents a message in the session, matching agent.Message structure.
type agentMessage struct {
	ExcludeFromContext bool
	CustomType         string
	Display            bool
	Details            any
	Diagnostics        []ai.AssistantDiagnostic
	Data               any
	StructuredContent  json.RawMessage
	ToolUsage          *ai.Usage
	NestedCalls        []ai.NestedToolCall
	Sections           map[string]*string
	ToolsAdded         []ai.Tool
	ToolsRemoved       []ai.ToolReference
	ThinkingLevel      string
	DurationMs         *float64
	Role               MessageRole
	Usage              ai.Usage
	Content            []ai.ContentBlock
	ToolCallID         string
	ToolName           string
	IsError            bool
	Provider           string
	API                string
	Model              string
	StopReason         string
	ErrorMessage       string
	ResponseID         string
	// ProviderThinkingLevel is the native effort an assistant turn was produced with.
	ProviderThinkingLevel string
	Timestamp             time.Time
}

// MessageRole represents the role of a message.
type MessageRole string

const (
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleTool      MessageRole = "tool"
	MessageRoleSystem    MessageRole = "system"
)
