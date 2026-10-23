package compaction

import (
	"encoding/json"
	"strings"
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
			wantFirstKeptID:  "m2",
			wantSummaryCount: 1,
		},
		{
			name: "previous compaction with no FirstKeptID falls back to post-compaction start",
			entries: []ScannerEntry{
				makeCompactionEntry("c1", "", "old summary"),
				makeMessageEntry("m1", "kept1"),
				makeMessageEntry("m2", "kept2"),
			},
			wantFirstKeptID:  "m2",
			wantSummaryCount: 1,
		},
		{
			name: "previous compaction with matching FirstKeptID uses it as boundary",
			entries: []ScannerEntry{
				makeCompactionEntry("c1", "m2", "old summary"),
				makeMessageEntry("m1", "should be excluded"),
				makeMessageEntry("m2", "first kept"),
				makeMessageEntry("m3", "second kept"),
			},
			// Boundary is set to m2 (per FirstKeptID), so only m2 is
			// summarized and m1 stays excluded.
			wantFirstKeptID:  "m3",
			wantSummaryCount: 1,
		},
		{
			name: "stale FirstKeptID falls back to post-compaction start",
			entries: []ScannerEntry{
				makeCompactionEntry("c1", "stale-id", "old summary"),
				makeMessageEntry("m1", "kept1"),
				makeMessageEntry("m2", "kept2"),
			},
			wantFirstKeptID:  "m2",
			wantSummaryCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prep := PrepareCompaction(tt.entries, Settings{KeepRecentTokens: 1})
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

	prep := PrepareCompaction(entries, Settings{KeepRecentTokens: 1})
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

func TestParseEntryRoleUsesTopLevelJSONRole(t *testing.T) {
	for _, scenario := range []struct {
		name, input string
		want        MessageRole
	}{
		{"nested role", `{"content":[{"role":"assistant","type":"text","text":"payload"}],"role":"tool"}`, RoleTool},
		{"whitespace", "{\"role\":\n\t\"user\",\"content\":\"hello\"}", RoleUser},
		{"escaped role", `{"role":"us\u0065r","content":"hello"}`, RoleUser},
		{"malformed", `{"role":"user"`, ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			entry := &ScannerEntry{Type: "message", Message: []byte(scenario.input)}
			if got := ParseEntryRole(entry); got != scenario.want {
				t.Fatalf("role=%q, want %q", got, scenario.want)
			}
		})
	}
}

func TestParseAgentMessageRetainsUsageAndStopReason(t *testing.T) {
	for _, reason := range []ai.StopReason{ai.StopReasonStop, ai.StopReasonError, ai.StopReasonAborted} {
		t.Run(string(reason), func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"role": "assistant", "content": []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "answer"}}, "usage": ai.Usage{Input: 100, Output: 20, TotalTokens: 120}, "stopReason": reason})
			if err != nil {
				t.Fatal(err)
			}
			message, err := ParseAgentMessage(data)
			if err != nil {
				t.Fatal(err)
			}
			if message.Usage.TotalTokens != 120 || message.StopReason != reason {
				t.Fatalf("metadata lost: %+v", message)
			}
			estimate := EstimateContextTokens([]*AgentMessage{message})
			if reason == ai.StopReasonStop {
				if estimate.UsageTokens != 120 {
					t.Fatalf("restored usage not counted: %+v", estimate)
				}
			} else if estimate.UsageTokens != 0 {
				t.Fatalf("failed assistant usage accepted: %+v", estimate)
			}
		})
	}
}

func TestEstimateTokensCountsCustomAndUserImages(t *testing.T) {
	for _, role := range []MessageRole{RoleUser, RoleCustom, RoleTool, "toolResult"} {
		t.Run(string(role), func(t *testing.T) {
			message := &AgentMessage{Role: role, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "text"}, {Type: ai.ContentBlockImage, Data: "image"}}}
			if got := EstimateTokens(message); got != 1201 {
				t.Fatalf("tokens=%d, want 1201", got)
			}
		})
	}
}

func TestEstimateTokensCountsSystemText(t *testing.T) {
	message, err := ParseAgentMessage([]byte(`{"role":"system","content":"system instructions"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := EstimateTokens(message); got != 5 {
		t.Fatalf("system prompt tokens=%d, want 5", got)
	}
}

func TestEstimateContextTokensIgnoresSparseEntries(t *testing.T) {
	messages := []*AgentMessage{nil, {Role: RoleAssistant, Usage: ai.Usage{TotalTokens: 100}, StopReason: ai.StopReasonStop}, nil, {Role: RoleUser, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "text"}}}, nil}
	estimate := EstimateContextTokens(messages)
	if estimate.Tokens != 101 || estimate.LastUsageIndex != 1 {
		t.Fatalf("sparse estimate=%+v", estimate)
	}
	if got := EstimateTokens(nil); got != 0 {
		t.Fatalf("nil estimate=%d", got)
	}
}

func TestFindCutPointRecognizesTurnStartRoles(t *testing.T) {
	for _, role := range []MessageRole{RoleUser, RoleCustom, RoleBashExecution, RoleBranchSummary, RoleCompactionSummary} {
		t.Run(string(role), func(t *testing.T) {
			entries := []ScannerEntry{
				{ID: "old", Type: "message", Message: json.RawMessage(`{"role":"user","content":"old"}`)},
				{ID: "start", Type: "message", Message: json.RawMessage(`{"role":"` + string(role) + `","content":"context","command":"ls","output":"files"}`)},
				{ID: "reply", Type: "message", Message: json.RawMessage(`{"role":"assistant","content":"reply"}`)},
			}
			scanner := &entrySliceScanner{entries: entries, start: 0, end: len(entries)}
			if !IsValidCutPointRole(role) {
				t.Errorf("%s is not a valid cut point", role)
			}
			if got := FindTurnStartIndex(scanner, 2, 0); got != 1 {
				t.Errorf("turn start = %d, want 1", got)
			}
			cut := FindCutPoint(scanner, 1, 2, 100000)
			if cut.FirstKeptIndex != 1 || cut.IsSplitTurn || cut.TurnStartIndex != -1 {
				t.Errorf("turn-start cut = %+v", cut)
			}
			split := FindCutPoint(scanner, 0, 3, 1)
			if split.FirstKeptIndex != 2 || !split.IsSplitTurn || split.TurnStartIndex != 1 {
				t.Errorf("assistant cut = %+v", split)
			}
		})
	}
}

func TestParseAgentMessageNormalizesLegacyStopReason(t *testing.T) {
	for _, scenario := range []struct {
		name, input string
		want        ai.StopReason
	}{
		{"legacy error", `{"role":"assistant","content":"failed","usage":{"totalTokens":120},"stop_reason":"error"}`, ai.StopReasonError},
		{"legacy aborted", `{"role":"assistant","content":"failed","usage":{"totalTokens":120},"stop_reason":"aborted"}`, ai.StopReasonAborted},
		{"canonical precedence", `{"role":"assistant","content":"failed","usage":{"totalTokens":120},"stopReason":"error","stop_reason":"stop"}`, ai.StopReasonError},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			message, err := ParseAgentMessage([]byte(scenario.input))
			if err != nil {
				t.Fatal(err)
			}
			if message.StopReason != scenario.want {
				t.Errorf("reason=%q, want %q", message.StopReason, scenario.want)
			}
			if estimate := EstimateContextTokens([]*AgentMessage{message}); estimate.UsageTokens != 0 {
				t.Fatalf("failed legacy assistant became usage anchor: %+v", estimate)
			}
		})
	}
}

func TestParseAgentMessageRestoresSpecialRoleFields(t *testing.T) {
	for _, scenario := range []struct {
		name, input string
		tokens      int
	}{
		{"bash", `{"role":"bashExecution","command":"ls","output":"files"}`, 2},
		{"branch", `{"role":"branchSummary","summary":"remember"}`, 2},
		{"compaction", `{"role":"compactionSummary","summary":"remember"}`, 2},
		{"legacy bash", `{"role":"bashExecution","content":{"command":"ls","output":"files"}}`, 2},
		{"legacy summary", `{"role":"compactionSummary","content":{"summary":"remember"}}`, 2},
		{"canonical precedence", `{"role":"compactionSummary","summary":"remember","content":{"summary":"obsolete long summary"}}`, 2},
		{"explicit empty summary", `{"role":"compactionSummary","summary":"","content":{"summary":"obsolete"}}`, 0},
		{"explicit empty bash", `{"role":"bashExecution","command":"","output":"","content":{"command":"ls","output":"files"}}`, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			message, err := ParseAgentMessage([]byte(scenario.input))
			if err != nil {
				t.Fatal(err)
			}
			if got := EstimateTokens(message); got != scenario.tokens {
				t.Fatalf("special fields lost: %+v, tokens=%d, want %d", message, got, scenario.tokens)
			}
		})
	}
}

func TestFindCutPointCountsCustomEntries(t *testing.T) {
	entries := []ScannerEntry{
		{ID: "old", Type: "message", Message: json.RawMessage(`{"role":"user","content":"old"}`)},
		{ID: "custom", Type: "custom_message", Message: json.RawMessage(`{"role":"custom","content":"large custom context"}`)},
		{ID: "reply", Type: "message", Message: json.RawMessage(`{"role":"assistant","content":"ok"}`)},
	}
	scanner := &entrySliceScanner{entries: entries, start: 0, end: len(entries)}
	cut := FindCutPoint(scanner, 0, len(entries), 3)
	if cut.FirstKeptIndex != 1 || cut.IsSplitTurn {
		t.Fatalf("custom context did not select its own boundary: %+v", cut)
	}
	cut = FindCutPoint(scanner, 0, len(entries), 1)
	if cut.FirstKeptIndex != 2 || !cut.IsSplitTurn || cut.TurnStartIndex != 1 {
		t.Fatalf("custom message incorrectly absorbed as metadata: %+v", cut)
	}
}

func TestFindCutPointIgnoresExcludedTurnStarts(t *testing.T) {
	for _, entryType := range []string{"message", "custom_message"} {
		t.Run(entryType, func(t *testing.T) {
			entries := []ScannerEntry{
				{ID: "user", Type: "message", Message: json.RawMessage(`{"role":"user","content":"goal"}`)},
				{ID: "excluded", Type: entryType, Message: json.RawMessage(`{"role":"custom","content":"private","excludeFromContext":true}`)},
				{ID: "assistant", Type: "message", Message: json.RawMessage(`{"role":"assistant","content":"answer"}`)},
			}
			scanner := &entrySliceScanner{entries: entries, end: len(entries)}
			if start := FindTurnStartIndex(scanner, 2, 0); start != 0 {
				t.Errorf("turn starts at excluded message %d", start)
			}
			cut := FindCutPoint(scanner, 0, len(entries), 1)
			if cut.FirstKeptIndex != 2 || cut.TurnStartIndex != 0 || !cut.IsSplitTurn {
				t.Fatalf("excluded message changed split turn: %+v", cut)
			}
		})
	}
}

func TestEstimateTokensIncludesSystemSections(t *testing.T) {
	message, err := ParseAgentMessage([]byte(`{"role":"system","content":"base","sections":{"policy":"abcdefgh","empty":"","removed":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := EstimateTokens(message); got != 3 {
		t.Fatalf("system section tokens=%d, want 3", got)
	}
}

func TestFindCutPointBudgetsBranchSummary(t *testing.T) {
	entries := []ScannerEntry{makeMessageEntry("old", "old"), {ID: "branch", Type: "branch_summary", Summary: "abcdefgh"}, makeMessageEntry("recent", "next")}
	scanner := &entrySliceScanner{entries: entries, end: len(entries)}
	cut := FindCutPoint(scanner, 0, len(entries), 2)
	if cut.FirstKeptIndex != 1 {
		t.Fatalf("branch summary omitted from retention budget: %+v", cut)
	}
}

func TestFindCutPointKeepsToolCallForOversizedTrailingResult(t *testing.T) {
	entries := []ScannerEntry{
		makeMessageEntry("goal", "goal"),
		{ID: "answer", Type: "message", Message: json.RawMessage(`{"role":"assistant","content":"done"}`)},
		makeMessageEntry("next", "next"),
		{ID: "call", Type: "message", Message: json.RawMessage(`{"role":"assistant","content":[{"type":"toolCall","name":"read","arguments":{}}]}`)},
		{ID: "result", Type: "message", Message: json.RawMessage(`{"role":"toolResult","content":"` + strings.Repeat("x", 400) + `"}`)},
	}
	scanner := &entrySliceScanner{entries: entries, end: len(entries)}
	cut := FindCutPoint(scanner, 0, len(entries), 10)
	if cut.FirstKeptIndex != 3 || !cut.IsSplitTurn || cut.TurnStartIndex != 2 {
		t.Fatalf("oversized trailing result fell back to keeping everything: %+v", cut)
	}
	prep := PrepareCompaction(entries, Settings{KeepRecentTokens: 10})
	if prep == nil || prep.FirstKeptID != "call" || len(prep.MessagesToSummarize) != 2 || len(prep.TurnPrefixMessages) != 1 {
		t.Fatalf("preparation=%+v", prep)
	}
}

func TestPrepareCompactionReturnsNilWithNothingToSummarize(t *testing.T) {
	entries := []ScannerEntry{makeMessageEntry("goal", "goal"), {ID: "reply", Type: "message", Message: json.RawMessage(`{"role":"assistant","content":"ok"}`)}}
	if prep := PrepareCompaction(entries, Settings{KeepRecentTokens: 100000}); prep != nil {
		t.Fatalf("preparation with nothing to summarize: %+v", prep)
	}
}
