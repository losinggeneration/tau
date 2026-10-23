package session

import (
	"encoding/json"
	"testing"
	"time"
)

func TestToAgentMessageRetainsDiagnosticsAndSampling(t *testing.T) {
	entry := &Entry{Type: EntryTypeMessage, Message: json.RawMessage(`{"role":"assistant","content":"ok","diagnostics":[{"type":"anthropic_strict_tools_fallback","timestamp":123,"details":{"reason":"strict_tool_limits_exceeded"}}],"toolsAdded":[{"name":"lookup","parameters":{"type":"object"},"constrainedSampling":{"type":"json_schema","strict":"require"}}],"providerThinkingLevel":"xhigh"}`)}
	message := entry.ToAgentMessage()
	if message == nil || message.ProviderThinkingLevel != "xhigh" || len(message.Diagnostics) != 1 || message.Diagnostics[0].Type != "anthropic_strict_tools_fallback" || len(message.ToolsAdded) != 1 || string(message.ToolsAdded[0].ConstrainedSampling) != `{"type":"json_schema","strict":"require"}` {
		t.Fatalf("message=%+v", message)
	}
}

func TestToAgentMessageRetainsNumericTranscriptTimestamp(t *testing.T) {
	for _, timestamp := range []string{"0", "1780000000123"} {
		t.Run(timestamp, func(t *testing.T) {
			entry := &Entry{Type: EntryTypeMessage, Timestamp: "2026-01-01T00:00:00Z", Message: json.RawMessage(`{"role":"system","content":"policy","timestamp":` + timestamp + `,"sections":{"policy":"section"},"toolsAdded":[{"name":"lookup"}]}`)}
			message := entry.ToAgentMessage()
			if message == nil {
				t.Fatal("numeric transcript timestamp discarded system entry")
			}
			var milliseconds int64
			if err := json.Unmarshal([]byte(timestamp), &milliseconds); err != nil {
				t.Fatal(err)
			}
			if message.Timestamp.UnixMilli() != milliseconds || len(message.ToolsAdded) != 1 || *message.Sections["policy"] != "section" {
				t.Fatalf("restored message = %+v", message)
			}
		})
	}
}

func TestToAgentMessageTimestampCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, timestamp string
		valid           bool
		want            time.Time
	}{
		{"string", `"2026-01-02T03:04:05.123Z"`, true, time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)},
		{"missing", "", true, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"null", "null", true, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"invalid string", `"invalid"`, false, time.Time{}},
		{"fraction", "1.5", false, time.Time{}},
		{"object", "{}", false, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"role":"user","content":"hello"`
			if tc.timestamp != "" {
				payload += `,"timestamp":` + tc.timestamp
			}
			payload += "}"
			entry := &Entry{Type: EntryTypeMessage, Timestamp: "2026-01-01T00:00:00Z", Message: json.RawMessage(payload)}
			message := entry.ToAgentMessage()
			if (message != nil) != tc.valid {
				t.Fatalf("message=%+v, valid=%v", message, tc.valid)
			}
			if message != nil && !message.Timestamp.Equal(tc.want) {
				t.Fatalf("timestamp=%v, want %v", message.Timestamp, tc.want)
			}
		})
	}
}

func TestToAgentMessageNormalizesPublicToolResultRole(t *testing.T) {
	entry := &Entry{Type: EntryTypeMessage, Message: json.RawMessage(`{"role":"toolResult","content":"result","toolCallId":"call","toolName":"lookup"}`)}
	message := entry.ToAgentMessage()
	if message == nil || message.Role != MessageRoleTool {
		t.Fatalf("tool result role not normalized: %+v", message)
	}
}

func TestToAgentMessageIncludesBranchSummary(t *testing.T) {
	entry := NewBranchSummaryEntry("abandoned", "decisions", false)
	message := entry.ToAgentMessage()
	if message == nil || message.Role != MessageRoleUser || len(message.Content) != 1 || message.Content[0].Text != "The following is a summary of a branch that this conversation came back from:\n\n<summary>\ndecisions</summary>" {
		t.Fatalf("branch summary=%+v", message)
	}
}

func TestToAgentMessageRetainsNestedRecords(t *testing.T) {
	entry := &Entry{Type: EntryTypeMessage, Message: json.RawMessage(`{"role":"toolResult","toolName":"caller","content":"ok","nestedCalls":[{"id":"outer/1","name":"lookup","status":"ok","durationMs":2}],"toolUsage":{"input":4}}`)}
	message := entry.ToAgentMessage()
	if message == nil || len(message.NestedCalls) != 1 || message.NestedCalls[0].ID != "outer/1" || message.ToolUsage == nil || message.ToolUsage.Input != 4 {
		t.Fatalf("lost nested execution: %+v", message)
	}
}
