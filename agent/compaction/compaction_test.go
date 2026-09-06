package compaction

import (
	"encoding/json"
	"testing"

	"github.com/losinggeneration/tau/ai"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name    string
		msg     *AgentMessage
		wantMin int
		wantMax int
	}{
		{
			name: "simple user message",
			msg: &AgentMessage{
				Role:    RoleUser,
				Content: []ai.ContentBlock{{Type: "text", Text: "hello world"}},
			},
			wantMin: 2,
			wantMax: 6,
		},
		{
			name: "assistant with thinking",
			msg: &AgentMessage{
				Role: RoleAssistant,
				Content: []ai.ContentBlock{
					{Type: "thinking", Thinking: "thinking content"},
					{Type: "text", Text: "final response"},
				},
			},
			wantMin: 3,
			wantMax: 15,
		},
		{
			name: "assistant with tool call",
			msg: &AgentMessage{
				Role: RoleAssistant,
				Content: []ai.ContentBlock{
					{Type: "toolCall", Name: "read", Arguments: json.RawMessage(`{"path":"/test/file.go"}`)},
				},
			},
			wantMin: 5,
			wantMax: 20,
		},
		{
			name: "bash execution",
			msg: &AgentMessage{
				Role:    RoleBashExecution,
				Command: "ls -la",
				Output:  "total 100",
			},
			wantMin: 2,
			wantMax: 30,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateTokens(tt.msg)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("EstimateTokens() = %v, want between %v and %v", got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestDefaultSettingsMatchParity(t *testing.T) {
	if !DefaultSettings.Enabled {
		t.Fatal("DefaultSettings.Enabled = false, want true")
	}
	if DefaultSettings.ReserveTokens != 16384 {
		t.Fatalf("DefaultSettings.ReserveTokens = %d, want 16384", DefaultSettings.ReserveTokens)
	}
	if DefaultSettings.KeepRecentTokens != 20000 {
		t.Fatalf("DefaultSettings.KeepRecentTokens = %d, want 20000", DefaultSettings.KeepRecentTokens)
	}
}

func TestEstimateContextTokensAnchorsOnLastValidAssistantUsage(t *testing.T) {
	messages := []*AgentMessage{
		{Role: RoleUser, Content: []ai.ContentBlock{{Type: "text", Text: "start"}}},
		{Role: RoleAssistant, StopReason: ai.StopReasonStop, Usage: ai.Usage{Input: 100, Output: 50, CacheRead: 10, CacheWrite: 5}},
		{Role: RoleAssistant, StopReason: ai.StopReasonError, Usage: ai.Usage{TotalTokens: 9999}},
		{Role: RoleAssistant, StopReason: ai.StopReasonAborted, Usage: ai.Usage{TotalTokens: 8888}},
		{Role: RoleUser, Content: []ai.ContentBlock{{Type: "text", Text: "trailing"}}},
	}

	estimate := EstimateContextTokens(messages)
	if estimate.LastUsageIndex != 1 {
		t.Fatalf("LastUsageIndex = %d, want 1", estimate.LastUsageIndex)
	}
	if estimate.UsageTokens != 165 {
		t.Fatalf("UsageTokens = %d, want 165", estimate.UsageTokens)
	}
	if estimate.TrailingTokens <= 0 {
		t.Fatalf("TrailingTokens = %d, want > 0", estimate.TrailingTokens)
	}
	if estimate.Tokens != estimate.UsageTokens+estimate.TrailingTokens {
		t.Fatalf("Tokens = %d, want usage+trailing %d", estimate.Tokens, estimate.UsageTokens+estimate.TrailingTokens)
	}
}

func TestEstimateContextTokens(t *testing.T) {
	tests := []struct {
		name     string
		messages []*AgentMessage
		wantZero bool
	}{
		{
			name:     "empty messages",
			messages: []*AgentMessage{},
			wantZero: true,
		},
		{
			name: "single message without usage",
			messages: []*AgentMessage{
				{Role: RoleUser, Content: []ai.ContentBlock{{Type: "text", Text: "hello"}}},
			},
			wantZero: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateContextTokens(tt.messages)
			if tt.wantZero && got.Tokens != 0 {
				t.Errorf("EstimateContextTokens() = %v, want 0 for empty", got.Tokens)
			}
			if !tt.wantZero && got.Tokens == 0 {
				t.Errorf("EstimateContextTokens() = 0, want non-zero")
			}
		})
	}
}

func TestFindCutPoint(t *testing.T) {
	// Create test entries
	entries := []ScannerEntry{
		{ID: "1", Type: "message", Message: json.RawMessage(`{"role":"user","content":"first message"}`)},
		{ID: "2", Type: "message", Message: json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"response 1"}]}`)},
		{ID: "3", Type: "message", Message: json.RawMessage(`{"role":"tool","content":"tool result"}`)},
		{ID: "4", Type: "message", Message: json.RawMessage(`{"role":"user","content":"second message"}`)},
		{ID: "5", Type: "message", Message: json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"response 2"}]}`)},
	}

	scanner := &entrySliceScanner{entries: entries, start: 0, end: len(entries)}

	// With high keepRecentTokens, should keep most entries
	result := FindCutPoint(scanner, 0, len(entries), 100000)
	if result.FirstKeptIndex != 0 {
		t.Errorf("FindCutPoint() with high keepRecentTokens, FirstKeptIndex = %v, want 0", result.FirstKeptIndex)
	}

	// With low keepRecentTokens, should cut some entries
	result = FindCutPoint(scanner, 0, len(entries), 1)
	// The exact index depends on the algorithm, but it should be > 0
	if result.FirstKeptIndex >= len(entries) {
		t.Errorf("FindCutPoint() FirstKeptIndex = %v, should be < len(entries) = %v", result.FirstKeptIndex, len(entries))
	}
}

func TestSettings_ShouldCompact(t *testing.T) {
	settings := Settings{
		Enabled:          true,
		ReserveTokens:    1000,
		KeepRecentTokens: 5000,
	}

	tests := []struct {
		name          string
		contextTokens int
		contextWindow int
		want          bool
	}{
		{"below threshold", 5000, 10000, false},
		{"at threshold (not exceeding)", 9000, 10000, false},
		{"above threshold", 9001, 10000, true},
		{"disabled", 9000, 10000, false},
	}

	settings.Enabled = true
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := settings.ShouldCompact(tt.contextTokens, tt.contextWindow)
			if got != tt.want {
				t.Errorf("ShouldCompact() = %v, want %v", got, tt.want)
			}
		})
	}

	settings.Enabled = false
	if settings.ShouldCompact(9000, 10000) {
		t.Error("ShouldCompact() should return false when disabled")
	}
}

func TestParseAgentMessage(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantNil bool
		wantErr bool
	}{
		{"nil data", nil, true, false},
		{"valid user", []byte(`{"role":"user","content":"hello"}`), false, false},
		{"valid assistant with usage", []byte(`{"role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input":100,"output":50}}`), false, false},
		{"invalid json", []byte(`{invalid`), false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAgentMessage(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseAgentMessage() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantNil && got != nil {
				t.Errorf("ParseAgentMessage() = %v, want nil", got)
			}
		})
	}
}

func TestFileOperations(t *testing.T) {
	ops := NewFileOperations()
	ops.Read["file1.go"] = struct{}{}
	ops.Edited["file2.go"] = struct{}{}

	readFiles, modifiedFiles := ComputeFileLists(ops)

	if len(readFiles) != 1 || readFiles[0] != "file1.go" {
		t.Errorf("ComputeFileLists() readFiles = %v, want [file1.go]", readFiles)
	}
	if len(modifiedFiles) != 1 || modifiedFiles[0] != "file2.go" {
		t.Errorf("ComputeFileLists() modifiedFiles = %v, want [file2.go]", modifiedFiles)
	}
}

func TestTruncateForSummary(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxChars int
		want     string
	}{
		{"short text", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"truncated", "hello world", 5, "hello\n\n[... 6 more characters truncated]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateForSummary(tt.text, tt.maxChars)
			if got != tt.want {
				t.Errorf("TruncateForSummary() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSerializeConversation(t *testing.T) {
	messages := []LLMMessage{
		{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hello"}}},
		{Role: "assistant", Content: []ContentBlock{{Type: "text", Text: "hi there"}}},
		{Role: "toolResult", Content: []ContentBlock{{Type: "text", Text: "tool output"}}},
	}

	got := SerializeConversation(messages)
	if got == "" {
		t.Error("SerializeConversation() returned empty string")
	}

	// Should contain role labels
	if !contains(got, "[User]:") {
		t.Error("SerializeConversation() should contain [User]:")
	}
	if !contains(got, "[Assistant]:") {
		t.Error("SerializeConversation() should contain [Assistant]:")
	}
	if !contains(got, "[Tool result]:") {
		t.Error("SerializeConversation() should contain [Tool result]:")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// makeMessageEntry builds a ScannerEntry carrying a user message with the
// given ID and text. Used by the PrepareCompaction boundary tests below.
func makeMessageEntry(id, text string) ScannerEntry {
	body, _ := json.Marshal(map[string]any{
		"role": "user",
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	})
	return ScannerEntry{ID: id, Type: "message", Message: body}
}

// makeCompactionEntry builds a ScannerEntry that represents a compaction
// with the given first-kept ID and summary. The summary is encoded into
// the message body so ParseAgentMessage can recover it.
func makeCompactionEntry(id, firstKeptID, summary string) ScannerEntry {
	body, _ := json.Marshal(map[string]any{
		"role": "compactionSummary",
		"content": map[string]any{
			"summary": summary,
		},
	})
	return ScannerEntry{
		ID:          id,
		Type:        "compaction",
		Message:     body,
		FirstKeptID: firstKeptID,
	}
}

// TestPrepareCompaction_BoundaryStart covers the four ways the boundary
// start is resolved: no previous compaction, previous compaction without
// a recorded FirstKeptID, previous compaction with a matching FirstKeptID,
// and a stale FirstKeptID that no longer matches anything in the path.
func TestPrepareCompaction_BoundaryStart(t *testing.T) {
	tests := []struct {
		name             string
		entries          []ScannerEntry
		wantFirstKeptID  string
		wantSummaryCount int
	}{
		{
			name: "no previous compaction starts at index 0",
			entries: []ScannerEntry{
				makeMessageEntry("m1", "hello"),
				makeMessageEntry("m2", "world"),
			},
			wantFirstKeptID:  "m1",
			wantSummaryCount: 0, // nothing to compact with default keep-recent
		},
		{
			name: "previous compaction with no FirstKeptID falls back to post-compaction start",
			entries: []ScannerEntry{
				makeCompactionEntry("c1", "", "old summary"),
				makeMessageEntry("m1", "kept1"),
				makeMessageEntry("m2", "kept2"),
			},
			wantFirstKeptID:  "m1",
			wantSummaryCount: 0,
		},
		{
			name: "previous compaction with matching FirstKeptID uses it as boundary",
			entries: []ScannerEntry{
				makeCompactionEntry("c1", "m2", "old summary"),
				makeMessageEntry("m1", "should be excluded"),
				makeMessageEntry("m2", "first kept"),
				makeMessageEntry("m3", "second kept"),
			},
			// Boundary is set to m2 (per FirstKeptID), so m1 is excluded
			// from the kept range. With the default KeepRecentTokens,
			// the small kept range fits without needing a cut, so nothing
			// is summarized — the assertion is on FirstKeptID being m2
			// (not m1), proving m1 was excluded from the boundary.
			wantFirstKeptID:  "m2",
			wantSummaryCount: 0,
		},
		{
			name: "stale FirstKeptID falls back to post-compaction start",
			entries: []ScannerEntry{
				makeCompactionEntry("c1", "stale-id", "old summary"),
				makeMessageEntry("m1", "kept1"),
				makeMessageEntry("m2", "kept2"),
			},
			wantFirstKeptID:  "m1",
			wantSummaryCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prep := PrepareCompaction(tt.entries, DefaultSettings)
			if prep == nil {
				t.Fatal("PrepareCompaction returned nil")
			}
			if prep.FirstKeptID != tt.wantFirstKeptID {
				t.Errorf("FirstKeptID = %q, want %q", prep.FirstKeptID, tt.wantFirstKeptID)
			}
			if got := len(prep.MessagesToSummarize); got != tt.wantSummaryCount {
				t.Errorf("MessagesToSummarize len = %d, want %d", got, tt.wantSummaryCount)
			}
		})
	}
}

// TestPrepareCompaction_PreviousSummary verifies that the summary from a
// previous compaction entry is preserved into the preparation so the new
// summary can build on it (iterative compaction). Catches refactors that
// would silently drop the summary chain.
func TestPrepareCompaction_PreviousSummary(t *testing.T) {
	const wantSummary = "session has discussed X and Y and decided on Z"
	entries := []ScannerEntry{
		makeCompactionEntry("c1", "m1", wantSummary),
		makeMessageEntry("m1", "first new message"),
		makeMessageEntry("m2", "second new message"),
	}

	prep := PrepareCompaction(entries, DefaultSettings)
	if prep == nil {
		t.Fatal("PrepareCompaction returned nil")
	}
	if prep.PreviousSummary != wantSummary {
		t.Errorf("PreviousSummary = %q, want %q", prep.PreviousSummary, wantSummary)
	}
}

func TestShouldCompact_TriggerTokens(t *testing.T) {
	tests := []struct {
		name          string
		contextTokens int
		contextWindow int
		cfg           Config
		want          bool
	}{
		{
			name:          "token cap fires early on 1M window",
			contextTokens: 250_000,
			contextWindow: 1_000_000,
			cfg:           Config{TriggerPct: 75, TriggerTokens: 250_000},
			want:          true,
		},
		{
			name:          "token cap not yet reached on 1M window",
			contextTokens: 249_999,
			contextWindow: 1_000_000,
			cfg:           Config{TriggerPct: 75, TriggerTokens: 250_000},
			want:          false,
		},
		{
			name:          "pct threshold still applies to smaller windows",
			contextTokens: 200_000,
			contextWindow: 300_000,
			cfg:           Config{TriggerPct: 75, TriggerTokens: 250_000},
			want:          false, // 75% of 300k = 225k < 250k cap, so pct governs
		},
		{
			name:          "cap ignored when zero",
			contextTokens: 250_000,
			contextWindow: 1_000_000,
			cfg:           Config{TriggerPct: 75},
			want:          false, // 75% of 1M = 750k
		},
		{
			name:          "disabled overrides cap",
			contextTokens: 900_000,
			contextWindow: 1_000_000,
			cfg:           Config{TriggerTokens: 250_000},
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldCompact(tt.contextTokens, tt.contextWindow, tt.cfg); got != tt.want {
				t.Errorf("ShouldCompact() = %v, want %v", got, tt.want)
			}
		})
	}
}
