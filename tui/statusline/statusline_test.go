package statusline

import "testing"

func TestRenderHeader(t *testing.T) {
	got := RenderHeader(State{Session: "chat", Branch: "main", Provider: "openai", Model: "gpt", Thinking: true}, 80)
	for _, w := range []string{"chat", " main", "openai/gpt", "thinking"} {
		if !contains(got, w) {
			t.Fatalf("header %q missing %q", got, w)
		}
	}
}

func TestRenderFooter(t *testing.T) {
	got := RenderFooter(State{Running: true, Pending: 2, ToolsEnabled: 4, Cost: Cost{InputTokens: 10, OutputTokens: 20, Dollars: 0.125}}, 80)
	for _, w := range []string{"running", "queued 2", "tools 4", "tok 10/20", "$0.13"} {
		if !contains(got, w) {
			t.Fatalf("footer %q missing %q", got, w)
		}
	}
}

func TestFitWidth(t *testing.T) {
	if got := RenderHeader(State{Session: "abcdef"}, 4); got != "abc…" {
		t.Fatalf("got=%q", got)
	}
	if got := RenderFooter(State{}, 6); len([]rune(got)) != 6 {
		t.Fatalf("width=%d got=%q", len([]rune(got)), got)
	}
}

func contains(s, sub string) bool { return len(sub) == 0 || (len(s) >= len(sub) && index(s, sub) >= 0) }

func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestRenderFooterAbortableRunningState(t *testing.T) {
	got := RenderFooter(State{Running: true, Abortable: true}, 30)
	if !contains(got, "running (esc abort)") {
		t.Fatalf("footer %q missing abort hint", got)
	}
}
