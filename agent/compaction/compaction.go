// Package compaction implements session context compaction with LLM-based
// summarization. It provides token estimation, cut-point detection, turn-split
// handling, and branch summarization capabilities.
package compaction

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/losinggeneration/tau/ai"
)

// DefaultSettings contains default compaction behavior.
var DefaultSettings = Settings{
	Enabled:          true,
	ReserveTokens:    16384,
	KeepRecentTokens: 20000,
}

// Settings controls compaction behavior.
type Settings struct {
	// Enabled indicates whether compaction is active.
	Enabled bool

	// ReserveTokens is the number of tokens reserved for the LLM prompt
	// and response during summarization.
	ReserveTokens int

	// KeepRecentTokens is the target number of recent tokens to keep
	// uncompacted when cutting history.
	KeepRecentTokens int
}

// ShouldCompact returns true if the estimated token count exceeds the
// threshold determined by context window and reserve tokens.
func (s Settings) ShouldCompact(contextTokens, contextWindow int) bool {
	if !s.Enabled || contextWindow <= 0 {
		return false
	}
	threshold := contextWindow - s.ReserveTokens
	return contextTokens > threshold
}

// Config controls auto-compaction triggered by context-window usage. It is the
// percentage-based companion to Settings, used by callers that want to
// auto-compact when a fraction of the model's context window has been consumed.
type Config struct {
	// TriggerPct is the context window usage percentage (0-100) that triggers
	// auto-compaction. A value <= 0 disables auto-compaction.
	TriggerPct int

	// TriggerTokens caps the trigger at an absolute token count. When > 0,
	// auto-compaction fires at min(TriggerPct% of the context window,
	// TriggerTokens). This lets large-context models (e.g. 1M-token windows)
	// compact well before the percentage threshold would fire.
	TriggerTokens int

	// PreserveRecent is the number of recent entries to keep uncompacted.
	PreserveRecent int
}

// DefaultConfig returns the default auto-compaction configuration.
func DefaultConfig() Config {
	return Config{
		TriggerPct:     75,
		PreserveRecent: 10,
	}
}

// ShouldCompact reports whether auto-compaction should fire given the estimated
// context-token usage, the model's context window, and the configured trigger
// percentage.
func ShouldCompact(contextTokens, contextWindow int, cfg Config) bool {
	if cfg.TriggerPct <= 0 || contextWindow <= 0 {
		return false
	}
	threshold := contextWindow * cfg.TriggerPct / 100
	if cfg.TriggerTokens > 0 && cfg.TriggerTokens < threshold {
		threshold = cfg.TriggerTokens
	}
	return contextTokens >= threshold
}

// ============================================================================
// Token Estimation
// ============================================================================

// ImageTokenEstimate is the estimated token count for an image in a message.
// Based on the TS approximation of ~1200 tokens (4800 chars / 4).
const ImageTokenEstimate = 1200

// estimateTokensChars returns an estimated token count using chars/4 heuristic.
// This is conservative (overestimates tokens).
func estimateTokensChars(chars int) int {
	return int(math.Ceil(float64(chars) / 4))
}

// EstimateTokens estimates the token count for an agent message.
// Uses a chars/4 heuristic for conservative overestimation.
func EstimateTokens(msg *AgentMessage) int {
	chars := 0

	switch msg.Role {
	case RoleUser:
		for _, block := range msg.Content {
			if block.Type == ai.ContentBlockText && block.Text != "" {
				chars += len(block.Text)
			}
		}

	case RoleAssistant:
		for _, block := range msg.Content {
			switch block.Type {
			case ai.ContentBlockText:
				if block.Text != "" {
					chars += len(block.Text)
				}
			case ai.ContentBlockThinking:
				if block.Thinking != "" {
					chars += len(block.Thinking)
				}
			case ai.ContentBlockToolCall:
				if block.Name != "" {
					chars += len(block.Name)
				}
				if block.Arguments != nil {
					chars += len(block.Arguments)
				}
			}
		}

	case RoleTool:
		for _, block := range msg.Content {
			if block.Type == ai.ContentBlockText && block.Text != "" {
				chars += len(block.Text)
			}
			if block.Type == ai.ContentBlockImage {
				// Estimate images as ~1200 tokens (4800 chars / 4)
				chars += 4800
			}
		}

	case RoleBashExecution:
		chars = len(msg.Command) + len(msg.Output)

	case RoleBranchSummary, RoleCompactionSummary:
		if msg.Summary != "" {
			chars = len(msg.Summary)
		}
	}

	return estimateTokensChars(chars)
}

//go:embed prompt.txt
var summarizationPrompt embed.FS

// BuildSummarizationPrompt builds the system prompt for summarization from a transcript.
func BuildSummarizationPrompt(transcript string) string {
	promptTemplate, err := summarizationPrompt.ReadFile("prompt.txt")
	if err != nil {
		// Fallback to inline prompt
		return inlineSummarizationPrompt(transcript)
	}
	return strings.Replace(string(promptTemplate), "%s", transcript, 1)
}

func inlineSummarizationPrompt(transcript string) string {
	return fmt.Sprintf(`You are a session compaction assistant. Your job is to produce a concise but complete summary of the following conversation transcript. The summary will replace the original messages in the session's LLM context, so it MUST preserve all information needed for the assistant to continue the conversation effectively.

## Instructions

1. **Preserve declared user goals**: What is the user trying to accomplish?
2. **Preserve key decisions**: What approaches were chosen and why?
3. **Preserve open questions**: What remains unresolved?
4. **Preserve files touched**: List all files that were read, created, or modified.
5. **Preserve error/gotcha context**: Any errors encountered, workarounds applied, or non-obvious constraints discovered.
6. **Preserve code state**: If code was written or modified, summarize the current state of the implementation.
7. **Be factual**: Do not add information not present in the transcript.
8. **Be concise**: Aim for 10-20%% of the original transcript length.

## Transcript

%s

## Summary`, transcript)
}

// ContextUsageEstimate contains the estimated context token breakdown.
type ContextUsageEstimate struct {
	// Tokens is the total estimated context tokens.
	Tokens int
	// UsageTokens is the tokens from the last usage report.
	UsageTokens int
	// TrailingTokens is the estimated tokens for messages after the last usage.
	TrailingTokens int
	// LastUsageIndex is the index of the last message with usage data, or -1.
	LastUsageIndex int
}

// EstimateContextTokens estimates context tokens from messages.
// Uses the last assistant usage when available for more accurate counting.
// If there are messages after the last usage, estimates their tokens with EstimateTokens.
func EstimateContextTokens(messages []*AgentMessage) ContextUsageEstimate {
	if len(messages) == 0 {
		return ContextUsageEstimate{
			Tokens:         0,
			UsageTokens:    0,
			TrailingTokens: 0,
			LastUsageIndex: -1,
		}
	}

	// Find last assistant with usage
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == RoleAssistant {
			if msg.StopReason != ai.StopReasonAborted && msg.StopReason != ai.StopReasonError && hasUsageTokens(msg.Usage) {
				usageTokens := calculateContextTokens(msg.Usage)
				var trailingTokens int
				for j := i + 1; j < len(messages); j++ {
					trailingTokens += EstimateTokens(messages[j])
				}
				return ContextUsageEstimate{
					Tokens:         usageTokens + trailingTokens,
					UsageTokens:    usageTokens,
					TrailingTokens: trailingTokens,
					LastUsageIndex: i,
				}
			}
		}
	}

	// No usage found - estimate all messages
	var total int
	for _, msg := range messages {
		total += EstimateTokens(msg)
	}
	return ContextUsageEstimate{
		Tokens:         total,
		UsageTokens:    0,
		TrailingTokens: total,
		LastUsageIndex: -1,
	}
}

func hasUsageTokens(usage ai.Usage) bool {
	return usage.TotalTokens > 0 || usage.Input > 0 || usage.Output > 0 || usage.CacheRead > 0 || usage.CacheWrite > 0
}

// calculateContextTokens computes total tokens from usage.
func calculateContextTokens(usage ai.Usage) int {
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	return usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
}

// ============================================================================
// Cut Point Detection
// ============================================================================

// CutPointResult contains the result of finding a cut point.
type CutPointResult struct {
	// FirstKeptIndex is the index of the first entry to keep.
	FirstKeptIndex int
	// TurnStartIndex is the index of the user message that starts the turn
	// being split, or -1 if not splitting.
	TurnStartIndex int
	// IsSplitTurn is true if the cut point is in the middle of a turn.
	IsSplitTurn bool
}

// MessageRole represents the role of a message.
type MessageRole string

const (
	RoleUser              MessageRole = "user"
	RoleAssistant         MessageRole = "assistant"
	RoleTool              MessageRole = "tool"
	RoleSystem            MessageRole = "system"
	RoleBashExecution     MessageRole = "bashExecution"
	RoleBranchSummary     MessageRole = "branchSummary"
	RoleCompactionSummary MessageRole = "compactionSummary"
	RoleCustom            MessageRole = "custom"
)

// EntryScanner defines the interface for session entry access used in cut-point detection.
type EntryScanner interface {
	// GetEntry returns the entry at the given index.
	GetEntry(index int) *ScannerEntry
	// Len returns the number of entries.
	Len() int
}

// ScannerEntry represents a session entry for cut-point detection.
// This is a minimal interface that doesn't require session package imports.
type ScannerEntry struct {
	ID       string
	Type     string
	ParentID *string
	Message  []byte // Raw JSON message content

	// FirstKeptID is populated by the caller for compaction entries and
	// carries the UUID of the first entry kept after that compaction.
	// For non-compaction entries the field is meaningless and empty.
	// The session storage layer records this as session.Entry.FirstKeptID
	// (json: first_kept_entry_id); the caller is responsible for
	// translating between the two representations.
	FirstKeptID string
}

// validCutPointRoles contains message roles that are valid cut points.
// Tool results are never valid cut points (they must follow their tool call).
var validCutPointRoles = map[MessageRole]bool{
	RoleUser:              true,
	RoleAssistant:         true,
	RoleBranchSummary:     true,
	RoleCompactionSummary: true,
}

// ParseEntryRole extracts the role from a scanner entry's message.
// Returns empty string if parsing fails.
func ParseEntryRole(entry *ScannerEntry) MessageRole {
	if entry.Message == nil {
		return ""
	}

	// Simple parse - we just need the role
	// Try to find "role" in the JSON
	start := bytes.Index(entry.Message, []byte(`"role"`))
	if start < 0 {
		return ""
	}
	colon := bytes.Index(entry.Message[start:], []byte(`:`))
	if colon < 0 {
		return ""
	}
	valueStart := start + colon + 1
	for valueStart < len(entry.Message) && (entry.Message[valueStart] == ' ' || entry.Message[valueStart] == '"') {
		if entry.Message[valueStart] == '"' {
			valueStart++
			break
		}
		valueStart++
	}
	valueEnd := valueStart
	for valueEnd < len(entry.Message) && entry.Message[valueEnd] != '"' {
		valueEnd++
	}
	if valueEnd > valueStart {
		return MessageRole(string(entry.Message[valueStart:valueEnd]))
	}
	return ""
}

// IsValidCutPointEntry returns true if this entry type is a valid cut point
// (can appear at cut boundaries).
func IsValidCutPointEntry(typ string) bool {
	switch typ {
	case "message", "custom_message", "branch_summary":
		return true
	default:
		return false
	}
}

// IsValidCutPointRole returns true if this role is a valid cut point.
// Tool results are never valid cut points (they must follow their tool call).
func IsValidCutPointRole(role MessageRole) bool {
	return validCutPointRoles[role]
}

// FindTurnStartIndex finds the user message (or bash execution) that starts
// the turn containing the given entry index.
// Returns -1 if no turn start found before the index.
func FindTurnStartIndex(scanner EntryScanner, entryIndex, startIndex int) int {
	for i := entryIndex; i >= startIndex; i-- {
		entry := scanner.GetEntry(i)
		if entry == nil {
			continue
		}
		if entry.Type == "custom_message" || entry.Type == "branch_summary" {
			return i
		}
		if entry.Type == "message" {
			role := ParseEntryRole(entry)
			if role == RoleUser || role == RoleBashExecution {
				return i
			}
		}
	}
	return -1
}

// findValidCutPoints returns the indices of valid cut points in the range.
func findValidCutPoints(scanner EntryScanner, startIndex, endIndex int) []int {
	var cutPoints []int
	for i := startIndex; i < endIndex; i++ {
		entry := scanner.GetEntry(i)
		if entry == nil {
			continue
		}
		if !IsValidCutPointEntry(entry.Type) {
			continue
		}
		// For message entries, also check the role
		if entry.Type == "message" {
			role := ParseEntryRole(entry)
			if !IsValidCutPointRole(role) {
				continue
			}
		}
		cutPoints = append(cutPoints, i)
	}
	return cutPoints
}

// FindCutPoint finds the cut point in session entries that keeps approximately
// `keepRecentTokens` of recent content.
//
// Algorithm: Walk backwards from newest, accumulating estimated message sizes.
// Stop when we've accumulated >= keepRecentTokens. Cut at that point.
//
// Can cut at user OR assistant messages (never tool results). When cutting at an
// assistant message with tool calls, its tool results come after and will be kept.
func FindCutPoint(scanner EntryScanner, startIndex, endIndex, keepRecentTokens int) CutPointResult {
	cutPoints := findValidCutPoints(scanner, startIndex, endIndex)

	if len(cutPoints) == 0 {
		return CutPointResult{
			FirstKeptIndex: startIndex,
			TurnStartIndex: -1,
			IsSplitTurn:    false,
		}
	}

	// Walk backwards from newest, accumulating estimated message sizes
	var accumulatedTokens int
	cutIndex := cutPoints[0] // Default: keep from first message (not header)

	for i := endIndex - 1; i >= startIndex; i-- {
		entry := scanner.GetEntry(i)
		if entry == nil || entry.Type != "message" {
			continue
		}

		// Estimate this message's size
		msg, err := ParseAgentMessage(entry.Message)
		if err != nil || msg == nil {
			continue
		}
		messageTokens := EstimateTokens(msg)
		accumulatedTokens += messageTokens

		// Check if we've exceeded the budget
		if accumulatedTokens >= keepRecentTokens {
			// Find the closest valid cut point at or after this entry
			for _, cp := range cutPoints {
				if cp >= i {
					cutIndex = cp
					break
				}
			}
			break
		}
	}

	// Scan backwards from cutIndex to include any non-message entries (bash, settings, etc.)
	for cutIndex > startIndex {
		prevEntry := scanner.GetEntry(cutIndex - 1)
		if prevEntry == nil {
			break
		}
		// Stop at session header or compaction boundaries
		if prevEntry.Type == "compaction" {
			break
		}
		if prevEntry.Type == "message" {
			// Stop if we hit any message
			break
		}
		// Include this non-message entry (bash, settings change, etc.)
		cutIndex--
	}

	// Determine if this is a split turn
	cutEntry := scanner.GetEntry(cutIndex)
	isUserMessage := false
	if cutEntry != nil && cutEntry.Type == "message" {
		role := ParseEntryRole(cutEntry)
		isUserMessage = role == RoleUser
	}
	turnStartIndex := -1
	if !isUserMessage {
		turnStartIndex = FindTurnStartIndex(scanner, cutIndex, startIndex)
	}

	return CutPointResult{
		FirstKeptIndex: cutIndex,
		TurnStartIndex: turnStartIndex,
		IsSplitTurn:    !isUserMessage && turnStartIndex != -1,
	}
}

// AgentMessage represents a message for compaction purposes.
// This mirrors the TypeScript AgentMessage interface.
type AgentMessage struct {
	Role       MessageRole
	Content    []ai.ContentBlock
	Command    string // for bashExecution role
	Output     string // for bashExecution role
	Summary    string // for branchSummary/compactionSummary role
	Usage      ai.Usage
	StopReason ai.StopReason
}

// ParseAgentMessage parses a raw JSON message into an AgentMessage.
func ParseAgentMessage(data []byte) (*AgentMessage, error) {
	if data == nil {
		return nil, nil
	}

	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	msg := &AgentMessage{Role: MessageRole(raw.Role)}

	if raw.Content != nil {
		if err := json.Unmarshal(raw.Content, &msg.Content); err != nil {
			// Fall back to plain text
			var text string
			if err := json.Unmarshal(raw.Content, &text); err == nil {
				msg.Content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: text}}
			}
		}
	}

	// Extract special fields based on role
	switch msg.Role {
	case RoleBashExecution:
		var exec struct {
			Command string `json:"command"`
			Output  string `json:"output"`
		}
		if raw.Content != nil {
			_ = json.Unmarshal(raw.Content, &exec)
			msg.Command = exec.Command
			msg.Output = exec.Output
		}
	case RoleBranchSummary, RoleCompactionSummary:
		var sum struct {
			Summary string `json:"summary"`
		}
		if raw.Content != nil {
			_ = json.Unmarshal(raw.Content, &sum)
			msg.Summary = sum.Summary
		}
	case RoleAssistant:
		if raw.Content != nil {
			var assistant struct {
				Usage ai.Usage `json:"usage"`
			}
			_ = json.Unmarshal(raw.Content, &assistant)
			msg.Usage = assistant.Usage
		}
	}

	return msg, nil
}
