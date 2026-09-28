package ai

import "testing"

func TestClaudeAgentModelsSupportImageInput(t *testing.T) {
	for _, id := range []string{"haiku", "sonnet", "opus"} {
		m, err := GetModel(ProviderClaudeAgent, id)
		if err != nil {
			t.Fatalf("GetModel(claude-agent, %s): %v", id, err)
		}
		if !m.SupportsInput(InputImage) {
			t.Errorf("claude-agent %s: SupportsInput(InputImage) = false, want true", id)
		}
	}
}

func TestClaudeAgentMirroredModelsKeepImageInput(t *testing.T) {
	m, err := GetModel(ProviderClaudeAgent, "claude-opus-4-6")
	if err != nil {
		t.Skip("no mirrored anthropic models registered")
	}
	if !m.SupportsInput(InputImage) {
		t.Errorf("claude-agent claude-opus-4-6: SupportsInput(InputImage) = false, want true (mirrored from anthropic)")
	}
}
