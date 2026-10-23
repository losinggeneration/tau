package compaction

import (
	"strings"
	"testing"
)

func TestSerializeConversationOmitsThinking(t *testing.T) {
	got := SerializeConversation([]LLMMessage{{Role: "assistant", Content: []ContentBlock{
		{Type: "thinking", Thinking: "private reasoning"},
		{Type: "text", Text: "visible answer"},
		{Type: "toolCall", Name: "read"},
	}}})
	if strings.Contains(got, "private reasoning") || strings.Contains(got, "thinking") {
		t.Fatalf("thinking leaked into summary: %q", got)
	}
	if !strings.Contains(got, "visible answer") || !strings.Contains(got, "read(") {
		t.Fatalf("conversation content lost: %q", got)
	}
}
