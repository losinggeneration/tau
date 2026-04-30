package tui

import "testing"

// The header collapses once the transcript has content, in both layout models
// (agreed deviation, see the Header doc). It is not permanently collapsed.
func TestFixedHeaderCollapsesOnceTheTranscriptHasContent(t *testing.T) {
	h := NewHeader("2.0.0")
	if out, _ := h.Render(80); out == "" {
		t.Fatal("header should show the banner before the first message")
	}

	h.SetHidden(true)
	if out, _ := h.Render(80); out != "" {
		t.Fatalf("header should collapse once the transcript has content, got %q", out)
	}
}

func TestAppTerminalTitleUsesTauBrand(t *testing.T) {
	if got := appTerminalTitle("", false); got != "tau" {
		t.Fatalf("empty session title = %q, want tau", got)
	}
	if got := appTerminalTitle("abc", false); got != "tau — abc" {
		t.Fatalf("session title = %q, want tau — abc", got)
	}
	if got := appTerminalTitle("abc", true); got != "⚙ tau — abc" {
		t.Fatalf("streaming title = %q, want ⚙ tau — abc", got)
	}
}
