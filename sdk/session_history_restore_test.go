package sdk

import (
	"encoding/json"
	"testing"

	"github.com/losinggeneration/tau/agent"
	sessionpkg "github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
)

func TestLoadSessionHistoryPreservesToolResultMetadata(t *testing.T) {
	t.Parallel()

	storage, err := sessionpkg.NewStorageAt(t.TempDir())
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}
	header := sessionpkg.NewHeader(".", nil, nil, "tool-history")
	header.ID = "tool-history"
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer func() {
		if err := writer.Close(); err != nil {
			t.Errorf("close writer: %v", err)
		}
	}()

	assistantMsg := &agent.Message{
		Role: agent.RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:      ai.ContentBlockToolCall,
			ID:        "call_123",
			Name:      "bash",
			Arguments: json.RawMessage(`{"command":"pwd"}`),
		}},
	}
	assistantRaw, err := marshalSessionMessageEnvelope(assistantMsg)
	if err != nil {
		t.Fatalf("marshal assistant: %v", err)
	}
	assistantEntry := sessionpkg.NewMessageEntry(nil, assistantRaw, nil)
	assistantEntry.ID = sessionpkg.NewID()
	if err := writer.Append(assistantEntry); err != nil {
		t.Fatalf("append assistant: %v", err)
	}

	toolMsg := &agent.Message{
		Role:       agent.RoleTool,
		ToolCallID: "call_123",
		ToolName:   "bash",
		Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "/tmp/project"}},
		IsError:    true,
	}
	toolRaw, err := marshalSessionMessageEnvelope(toolMsg)
	if err != nil {
		t.Fatalf("marshal tool: %v", err)
	}
	toolEntry := sessionpkg.NewMessageEntry(&assistantEntry.ID, toolRaw, nil)
	toolEntry.ID = sessionpkg.NewID()
	if err := writer.Append(toolEntry); err != nil {
		t.Fatalf("append tool: %v", err)
	}

	history, err := loadSessionHistory(storage, "tool-history")
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history len = %d, want 2", len(history))
	}
	got := history[1]
	if got.Role != agent.RoleTool {
		t.Fatalf("tool role = %q, want %q", got.Role, agent.RoleTool)
	}
	if got.ToolCallID != "call_123" || got.ToolName != "bash" || !got.IsError {
		t.Fatalf("tool metadata = id %q name %q isError %v, want call_123 bash true", got.ToolCallID, got.ToolName, got.IsError)
	}
}
