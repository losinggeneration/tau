// Package compaction implements session context compaction with LLM-based
// summarization.
package compaction

import (
	"slices"
	"strings"
)

// ============================================================================
// Compaction Preparation
// ============================================================================

// Preparation contains the data needed to perform compaction.
type Preparation struct {
	// FirstKeptID is the UUID of the first entry to keep.
	FirstKeptID string

	// MessagesToSummarize are messages that will be summarized and discarded.
	MessagesToSummarize []*AgentMessage

	// TurnPrefixMessages are messages that will be turned into a turn prefix summary
	// (if splitting a turn).
	TurnPrefixMessages []*AgentMessage

	// IsSplitTurn indicates if the cut point is in the middle of a turn.
	IsSplitTurn bool

	// TokensBefore is the estimated token count before compaction.
	TokensBefore int

	// PreviousSummary is the summary from previous compaction (for iterative update).
	PreviousSummary string

	// FileOps are file operations extracted from messages.
	FileOps FileOperations

	// Settings are the compaction settings.
	Settings Settings
}

// EntryScannerToMessages converts an EntryScanner to AgentMessages for compaction.
func EntryScannerToMessages(scanner EntryScanner) []*AgentMessage {
	messages := make([]*AgentMessage, 0, scanner.Len())

	for i := 0; i < scanner.Len(); i++ {
		entry := scanner.GetEntry(i)
		if entry == nil {
			continue
		}

		msg, err := parseScannerMessage(entry)
		if err != nil || msg == nil {
			continue
		}

		messages = append(messages, msg)
	}

	return messages
}

// PrepareCompaction prepares the data needed for compaction.
// Returns nil if there's nothing to compact or if the session needs migration.
func PrepareCompaction(pathEntries []ScannerEntry, settings Settings) *Preparation {
	// Check if last entry is already a compaction
	if len(pathEntries) > 0 {
		lastEntry := pathEntries[len(pathEntries)-1]
		if lastEntry.Type == "compaction" {
			return nil
		}
	}

	// Find previous compaction index
	prevCompactionIndex := -1
	for i := len(pathEntries) - 1; i >= 0; i-- {
		if pathEntries[i].Type == "compaction" {
			prevCompactionIndex = i
			break
		}
	}

	// Determine boundary
	var previousSummary string
	var boundaryStart int
	if prevCompactionIndex >= 0 {
		// Get previous summary
		prevEntry := pathEntries[prevCompactionIndex]
		previousSummary = prevEntry.Summary
		if previousSummary == "" {
			if compMsg, err := ParseAgentMessage(prevEntry.Message); err == nil && compMsg != nil {
				previousSummary = compMsg.Summary
			}
		}

		// Find boundary start. Prefer the previous compaction's recorded
		// FirstKeptID so we don't include entries that were dropped at
		// compaction time (e.g., from branches or forks). Fall back to
		// "everything after the compaction entry" if the field is empty
		// (old sessions without first_kept_entry_id) or no longer matches
		// (stale reference / unrelated pruning). Returning a sensible
		// boundary is preferable to failing migration on every old session;
		// the next compaction will record a fresh FirstKeptID.
		boundaryStart = prevCompactionIndex + 1
		if prevEntry.FirstKeptID != "" {
			if idx := slices.IndexFunc(pathEntries[prevCompactionIndex+1:],
				func(e ScannerEntry) bool { return e.ID == prevEntry.FirstKeptID }); idx >= 0 {
				boundaryStart = prevCompactionIndex + 1 + idx
			}
		}
	}
	boundaryEnd := len(pathEntries)

	// Estimate tokens before
	var tokensBefore int
	for i := boundaryStart; i < boundaryEnd; i++ {
		entry := &pathEntries[i]
		if entry.Type == "message" || entry.Type == "custom_message" || entry.Type == "branch_summary" {
			msg, err := parseScannerMessage(entry)
			if err == nil && msg != nil {
				tokensBefore += EstimateTokens(msg)
			}
		}
	}

	// Find cut point
	cutPoint := FindCutPoint(&entrySliceScanner{entries: pathEntries, start: boundaryStart, end: boundaryEnd}, 0, boundaryEnd-boundaryStart, settings.KeepRecentTokens)

	// Adjust indices for boundary
	firstKeptIndex := boundaryStart + cutPoint.FirstKeptIndex

	// Get first kept entry ID
	if firstKeptIndex >= len(pathEntries) {
		return nil // Session needs migration
	}
	firstKeptEntry := pathEntries[firstKeptIndex]
	if firstKeptEntry.ID == "" {
		return nil // Session needs migration
	}

	// Determine history end
	historyEnd := firstKeptIndex
	if cutPoint.IsSplitTurn && cutPoint.TurnStartIndex >= 0 {
		historyEnd = boundaryStart + cutPoint.TurnStartIndex
	}

	// Collect messages to summarize
	var messagesToSummarize []*AgentMessage
	for i := boundaryStart; i < historyEnd; i++ {
		entry := &pathEntries[i]
		if entry.Type == "message" || entry.Type == "custom_message" || entry.Type == "branch_summary" {
			msg, err := parseScannerMessage(entry)
			if err == nil && msg != nil {
				messagesToSummarize = append(messagesToSummarize, msg)
			}
		}
	}

	// Collect turn prefix messages (if splitting)
	var turnPrefixMessages []*AgentMessage
	if cutPoint.IsSplitTurn && cutPoint.TurnStartIndex >= 0 {
		for i := boundaryStart + cutPoint.TurnStartIndex; i < firstKeptIndex; i++ {
			entry := &pathEntries[i]
			if entry.Type == "message" || entry.Type == "custom_message" || entry.Type == "branch_summary" {
				msg, err := parseScannerMessage(entry)
				if err == nil && msg != nil {
					turnPrefixMessages = append(turnPrefixMessages, msg)
				}
			}
		}
	}

	if len(messagesToSummarize) == 0 && len(turnPrefixMessages) == 0 {
		return nil
	}

	// Extract file operations
	fileOps := NewFileOperations()
	for _, msg := range messagesToSummarize {
		ExtractFileOpsFromMessage(msg, &fileOps)
	}
	if cutPoint.IsSplitTurn {
		for _, msg := range turnPrefixMessages {
			ExtractFileOpsFromMessage(msg, &fileOps)
		}
	}

	return &Preparation{
		FirstKeptID:         firstKeptEntry.ID,
		MessagesToSummarize: messagesToSummarize,
		TurnPrefixMessages:  turnPrefixMessages,
		IsSplitTurn:         cutPoint.IsSplitTurn,
		TokensBefore:        tokensBefore,
		PreviousSummary:     previousSummary,
		FileOps:             fileOps,
		Settings:            settings,
	}
}

// entrySliceScanner implements EntryScanner for a slice of ScannerEntry.
type entrySliceScanner struct {
	entries []ScannerEntry
	start   int
	end     int
}

func (s *entrySliceScanner) GetEntry(index int) *ScannerEntry {
	absIndex := s.start + index
	if absIndex < 0 || absIndex >= len(s.entries) || absIndex >= s.end {
		return nil
	}
	return &s.entries[absIndex]
}

func (s *entrySliceScanner) Len() int {
	if s.end > len(s.entries) {
		s.end = len(s.entries)
	}
	return s.end - s.start
}

// ============================================================================
// Compaction Result
// ============================================================================

// Result contains the output of a compaction operation.
type Result struct {
	// Summary is the generated summary text.
	Summary string

	// FirstKeptID is the UUID of the first entry kept after compaction.
	FirstKeptID string

	// TokensBefore is the estimated token count before compaction.
	TokensBefore int

	// Details contains extension-specific data (e.g., file operations).
	Details *CompactionDetails
}

// BuildSummaryFromPreparation builds a compaction summary string from preparation data.
// This is a helper for generating summaries from the preparation without calling an LLM.
func BuildSummaryFromPreparation(prep *Preparation, customInstructions string) string {
	var sb strings.Builder

	// Add custom instructions if provided
	if customInstructions != "" {
		sb.WriteString("## Custom Focus\n")
		sb.WriteString(customInstructions)
		sb.WriteString("\n\n")
	}

	// Add previous summary context if available
	if prep.PreviousSummary != "" {
		sb.WriteString("## Previous Context\n")
		sb.WriteString(prep.PreviousSummary)
		sb.WriteString("\n\n")
	}

	// Add turn prefix context if splitting
	if prep.IsSplitTurn && len(prep.TurnPrefixMessages) > 0 {
		sb.WriteString("## Turn Prefix Context\n")
		for _, msg := range prep.TurnPrefixMessages {
			sb.WriteString(formatMessageBrief(msg))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	// Add file operations
	readFiles, modifiedFiles := ComputeFileLists(prep.FileOps)
	sb.WriteString(FormatFileOperations(readFiles, modifiedFiles))

	return sb.String()
}

// formatMessageBrief formats a message briefly for summary context.
func formatMessageBrief(msg *AgentMessage) string {
	switch msg.Role {
	case RoleUser:
		content := msg.ExtractTextContent()
		if len(content) > 100 {
			content = content[:100] + "..."
		}
		return "[User]: " + content
	case RoleAssistant:
		var parts []string
		for _, block := range msg.Content {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
			if block.Type == "toolCall" && block.Name != "" {
				parts = append(parts, "[calls: "+block.Name+"]")
			}
		}
		content := strings.Join(parts, " ")
		if len(content) > 100 {
			content = content[:100] + "..."
		}
		return "[Assistant]: " + content
	default:
		return string(msg.Role)
	}
}

// extractTextContent extracts text from content blocks in AgentMessage.
func (a *AgentMessage) ExtractTextContent() string {
	var parts []string
	for _, block := range a.Content {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, " ")
}
