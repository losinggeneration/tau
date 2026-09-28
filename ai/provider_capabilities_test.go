package ai

import "testing"

func TestClaudeAgentCapabilities(t *testing.T) {
	if !ClaudeAgentCapabilities.Vision {
		t.Fatal("ClaudeAgentCapabilities.Vision = false, want true: the provider builds base64 image blocks for streaming input")
	}
}
