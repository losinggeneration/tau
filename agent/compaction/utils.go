// Package compaction implements session context compaction with LLM-based
// summarization.
package compaction

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ============================================================================
// File Operation Tracking
// ============================================================================

// FileOperations tracks file read/write operations for compaction summaries.
type FileOperations struct {
	Read    map[string]struct{}
	Written map[string]struct{}
	Edited  map[string]struct{}
}

// NewFileOperations creates an empty FileOperations tracker.
func NewFileOperations() FileOperations {
	return FileOperations{
		Read:    make(map[string]struct{}),
		Written: make(map[string]struct{}),
		Edited:  make(map[string]struct{}),
	}
}

// ExtractFileOpsFromMessage extracts file operations from tool calls in an assistant message.
func ExtractFileOpsFromMessage(msg *AgentMessage, ops *FileOperations) {
	if msg.Role != RoleAssistant {
		return
	}

	for _, block := range msg.Content {
		if block.Type != "toolCall" {
			continue
		}
		if block.Name == "" || block.Arguments == nil {
			continue
		}

		// Parse the arguments to get the path
		var args map[string]json.RawMessage
		if err := json.Unmarshal(block.Arguments, &args); err != nil {
			continue
		}

		pathRaw, ok := args["path"]
		if !ok {
			continue
		}

		var path string
		if err := json.Unmarshal(pathRaw, &path); err != nil || path == "" {
			continue
		}

		switch block.Name {
		case "read":
			ops.Read[path] = struct{}{}
		case "write":
			ops.Written[path] = struct{}{}
		case "edit":
			ops.Edited[path] = struct{}{}
		}
	}
}

// ComputeFileLists computes final file lists from file operations.
// Returns readFiles (files only read, not modified) and modifiedFiles.
func ComputeFileLists(ops FileOperations) (readFiles []string, modifiedFiles []string) {
	modified := make(map[string]struct{})
	for f := range ops.Edited {
		modified[f] = struct{}{}
	}
	for f := range ops.Written {
		modified[f] = struct{}{}
	}

	var readOnly []string
	for f := range ops.Read {
		if _, ok := modified[f]; !ok {
			readOnly = append(readOnly, f)
		}
	}

	var modifiedList []string
	for f := range modified {
		modifiedList = append(modifiedList, f)
	}

	// Sort for deterministic output
	slices.Sort(readOnly)
	slices.Sort(modifiedList)

	return readOnly, modifiedList
}

// FormatFileOperations formats file operations as XML tags for summary.
func FormatFileOperations(readFiles []string, modifiedFiles []string) string {
	var sections []string

	if len(readFiles) > 0 {
		sections = append(sections, fmt.Sprintf("<read-files>\n%s\n</read-files>", strings.Join(readFiles, "\n")))
	}
	if len(modifiedFiles) > 0 {
		sections = append(sections, fmt.Sprintf("<modified-files>\n%s\n</modified-files>", strings.Join(modifiedFiles, "\n")))
	}

	if len(sections) == 0 {
		return ""
	}

	return "\n\n" + strings.Join(sections, "\n\n")
}

// ============================================================================
// Tool Result Truncation
// ============================================================================

// ToolResultMaxChars is the maximum characters for a tool result in serialized summaries.
const ToolResultMaxChars = 2000

// TruncateForSummary truncates text to a maximum character length for summarization.
// Keeps the beginning and appends a truncation marker.
func TruncateForSummary(text string, maxChars int) string {
	if len(text) <= maxChars {
		return text
	}
	truncatedChars := len(text) - maxChars
	return text[:maxChars] + fmt.Sprintf("\n\n[... %d more characters truncated]", truncatedChars)
}

// ============================================================================
// Conversation Serialization
// ============================================================================

// SummarizationSystemPrompt is the system prompt used for summarization.
const SummarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI coding assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

// SerializeConversation serializes LLM messages to text for summarization.
// This prevents the model from treating it as a conversation to continue.
func SerializeConversation(messages []LLMMessage) string {
	var parts []string

	for _, msg := range messages {
		switch msg.Role {
		case "user":
			content := extractTextContent(msg.Content)
			if content != "" {
				parts = append(parts, fmt.Sprintf("[User]: %s", content))
			}

		case "assistant":
			var textParts, thinkingParts, toolCalls []string

			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						textParts = append(textParts, block.Text)
					}
				case "thinking":
					if block.Thinking != "" {
						thinkingParts = append(thinkingParts, block.Thinking)
					}
				case "toolCall":
					if block.Name != "" {
						argsStr := formatToolCallArgs(block.Arguments)
						toolCalls = append(toolCalls, fmt.Sprintf("%s(%s)", block.Name, argsStr))
					}
				}
			}

			if len(thinkingParts) > 0 {
				parts = append(parts, fmt.Sprintf("[Assistant thinking]: %s", strings.Join(thinkingParts, "\n")))
			}
			if len(textParts) > 0 {
				parts = append(parts, fmt.Sprintf("[Assistant]: %s", strings.Join(textParts, "\n")))
			}
			if len(toolCalls) > 0 {
				parts = append(parts, fmt.Sprintf("[Assistant tool calls]: %s", strings.Join(toolCalls, "; ")))
			}

		case "toolResult":
			content := extractTextContent(msg.Content)
			if content != "" {
				parts = append(parts, fmt.Sprintf("[Tool result]: %s", TruncateForSummary(content, ToolResultMaxChars)))
			}
		}
	}

	return strings.Join(parts, "\n\n")
}

// formatToolCallArgs formats tool call arguments as a string.
func formatToolCallArgs(args map[string]any) string {
	if args == nil {
		return ""
	}
	var parts []string
	for k, v := range args {
		// Simple JSON marshal for the value
		vJSON, _ := json.Marshal(v)
		parts = append(parts, fmt.Sprintf("%s=%s", k, string(vJSON)))
	}
	return strings.Join(parts, ", ")
}

// extractTextContent extracts text content from message content blocks.
func extractTextContent(content []ContentBlock) string {
	var parts []string
	for _, block := range content {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "")
}

// LLMMessage represents a message in LLM format for serialization.
type LLMMessage struct {
	Role    string
	Content []ContentBlock
}

// ContentBlock represents a content block in LLM messages.
type ContentBlock struct {
	Type      string
	Text      string
	Thinking  string
	Name      string
	Arguments map[string]any
}

// ============================================================================
// Compaction Details
// ============================================================================

// CompactionDetails stores details for compaction entries.
type CompactionDetails struct {
	ReadFiles     []string `json:"readFiles"`
	ModifiedFiles []string `json:"modifiedFiles"`
}
