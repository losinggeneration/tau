package tui

import (
	"strings"
	"testing"
)

// TestStatuslineIntegration_FooterFields verifies that the
// statusline helper, when fed footerData, emits the canonical
// "running/queued/tools/tok" tokens for the footer region.
func TestStatuslineIntegration_FooterFields(t *testing.T) {
	d := &footerData{
		cwd:           "/home/user/project",
		branch:        "main",
		model:         "openai/gpt-5",
		streaming:     true,
		queueCount:    2,
		providerCount: 4,
		contextUsed:   10,
		contextWindow: 100,
		totalTokens:   20,
		thinkingLevel: "high",
	}
	out := renderStatuslineFooter(d, 80)
	for _, w := range []string{"running", "queued 2", "tools 4", "tok 10/20"} {
		if !strings.Contains(out, w) {
			t.Fatalf("statusline footer missing %q: %q", w, out)
		}
	}
}

// TestStatuslineIntegration_HeaderFields verifies the header helper
// includes the session basename, branch, provider/model, and thinking
// marker when the underlying footerData carries those values.
func TestStatuslineIntegration_HeaderFields(t *testing.T) {
	d := &footerData{
		cwd:           "/tmp/somewhere/project",
		branch:        "feature-x",
		model:         "anthropic/claude",
		thinkingLevel: "medium",
	}
	out := renderStatuslineHeader(d, 80)
	for _, w := range []string{"project", "feature-x", "anthropic/claude", "thinking"} {
		if !strings.Contains(out, w) {
			t.Fatalf("statusline header missing %q: %q", w, out)
		}
	}
}

// TestStatuslineIntegration_ProviderModelSplit verifies that
// "provider/model" values stored in a single footerData.model field
// are split correctly for the statusline helper.
func TestStatuslineIntegration_ProviderModelSplit(t *testing.T) {
	d := &footerData{model: "openai/gpt-5-mini"}
	out := renderStatuslineHeader(d, 80)
	if !strings.Contains(out, "openai/gpt-5-mini") {
		t.Fatalf("expected combined provider/model in header, got %q", out)
	}
}

// TestStatuslineIntegration_FitWidth verifies the integration respects
// the requested width even when the data is rich.
func TestStatuslineIntegration_FitWidth(t *testing.T) {
	d := &footerData{
		streaming:     true,
		queueCount:    1,
		providerCount: 2,
		contextUsed:   1,
		contextWindow: 1,
		totalTokens:   2,
	}
	out := renderStatuslineFooter(d, 12)
	if w := len([]rune(out)); w != 12 {
		t.Fatalf("expected footer width 12, got %d (%q)", w, out)
	}
}
