package compaction

import (
	"encoding/json"
	"testing"
)

func TestPreparationRetainsToolResults(t *testing.T) {
	for _, role := range []string{"tool", "toolResult"} {
		t.Run(role, func(t *testing.T) {
			entries := []ScannerEntry{
				{ID: "user", Type: "message", Message: json.RawMessage(`{"role":"user","content":"goal"}`)},
				{ID: "assistant", Type: "message", Message: json.RawMessage(`{"role":"assistant","content":"call"}`)},
				{ID: "result", Type: "message", Message: json.RawMessage(`{"role":"` + role + `","content":"important output"}`)},
				{ID: "recent", Type: "message", Message: json.RawMessage(`{"role":"user","content":"next"}`)},
			}
			scanner := &entrySliceScanner{entries: entries, end: len(entries)}
			messages := EntryScannerToMessages(scanner)
			if len(messages) != 4 {
				t.Errorf("scanner dropped tool output: got %d messages", len(messages))
			}
			for _, budget := range []int{1, 2} {
				prep := PrepareCompaction(entries, Settings{KeepRecentTokens: budget})
				if prep == nil {
					t.Fatal("missing preparation")
				}
				if prep.TokensBefore != 7 {
					t.Errorf("tokens before=%d, want 7 including tool output", prep.TokensBefore)
				}
				summarized := append(prep.MessagesToSummarize, prep.TurnPrefixMessages...)
				found := false
				for _, message := range summarized {
					if message.ExtractTextContent() == "important output" {
						found = true
					}
				}
				if !found {
					t.Errorf("budget %d: tool output missing from summary input", budget)
				}
			}
		})
	}
}

func TestPreparationOmitsContextExcludedMessages(t *testing.T) {
	entries := []ScannerEntry{
		{ID: "excluded", Type: "message", Message: json.RawMessage(`{"role":"user","content":"private excluded context","excludeFromContext":true}`)},
		{ID: "visible", Type: "message", Message: json.RawMessage(`{"role":"user","content":"goal"}`)},
		{ID: "recent", Type: "message", Message: json.RawMessage(`{"role":"user","content":"next"}`)},
	}
	scanner := &entrySliceScanner{entries: entries, end: len(entries)}
	messages := EntryScannerToMessages(scanner)
	if len(messages) != 2 {
		t.Errorf("scanner includes excluded context: %d messages", len(messages))
	}
	prep := PrepareCompaction(entries, Settings{KeepRecentTokens: 1})
	if prep == nil {
		t.Fatal("missing preparation")
	}
	if prep.TokensBefore != 2 {
		t.Errorf("tokens before=%d, want 2", prep.TokensBefore)
	}
	if len(prep.MessagesToSummarize) != 1 || prep.MessagesToSummarize[0].ExtractTextContent() != "goal" {
		t.Fatalf("summary includes excluded context: %+v", prep.MessagesToSummarize)
	}
}

func TestPreparationRestoresEntrySummary(t *testing.T) {
	entries := []ScannerEntry{
		{ID: "compaction", Type: "compaction", FirstKeptID: "kept", Summary: "previous decisions"},
		makeMessageEntry("dropped", "old context"),
		makeMessageEntry("kept", "new context"),
		makeMessageEntry("recent", "next"),
	}
	prep := PrepareCompaction(entries, Settings{KeepRecentTokens: 1})
	if prep == nil {
		t.Fatal("missing preparation")
	}
	if prep.PreviousSummary != "previous decisions" {
		t.Fatalf("previous summary=%q", prep.PreviousSummary)
	}
	if len(prep.MessagesToSummarize) != 1 || prep.MessagesToSummarize[0].ExtractTextContent() != "new context" {
		t.Fatalf("wrong summary boundary: %+v", prep.MessagesToSummarize)
	}
}

func TestPreparationIncludesBranchSummary(t *testing.T) {
	entries := []ScannerEntry{{ID: "branch", Type: "branch_summary", Summary: "decisions"}, makeMessageEntry("recent", "next")}
	prep := PrepareCompaction(entries, Settings{KeepRecentTokens: 1})
	if prep == nil || prep.TokensBefore != 4 || len(prep.MessagesToSummarize) != 1 || prep.MessagesToSummarize[0].Role != RoleBranchSummary || prep.MessagesToSummarize[0].Summary != "decisions" {
		t.Fatalf("branch summary preparation=%+v", prep)
	}
}

func TestEntryScannerToMessagesIncludesBranchSummary(t *testing.T) {
	scanner := &entrySliceScanner{entries: []ScannerEntry{{ID: "branch", Type: "branch_summary", Summary: "decisions"}}, end: 1}
	messages := EntryScannerToMessages(scanner)
	if len(messages) != 1 || messages[0].Role != RoleBranchSummary || messages[0].Summary != "decisions" {
		t.Fatalf("scanner messages=%+v", messages)
	}
}
