package tui

import (
	"strings"
	"testing"
)

func TestThrobberFramesMatchPiReference(t *testing.T) {
	want := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	if len(throbberFrames) != len(want) {
		t.Fatalf("throbberFrames length = %d, want %d", len(throbberFrames), len(want))
	}
	for i, frame := range want {
		if throbberFrames[i] != frame {
			t.Fatalf("throbberFrames[%d] = %q, want %q", i, throbberFrames[i], frame)
		}
	}
}

func TestThrobberAdvanceWraps(t *testing.T) {
	var th throbber
	if got := th.Frame(); got != "⠋" {
		t.Fatalf("initial frame = %q, want %q", got, "⠋")
	}
	if got := th.Advance(); got != "⠙" {
		t.Fatalf("advanced frame = %q, want %q", got, "⠙")
	}
	for i := 1; i < len(throbberFrames); i++ {
		th.Advance()
	}
	if got := th.Frame(); got != "⠋" {
		t.Fatalf("wrapped frame = %q, want %q", got, "⠋")
	}
}

func TestFooterStreamingDoesNotRenderLoader(t *testing.T) {
	footer := NewFooter(&footerData{cwd: "/tmp/proj", streaming: true, workingFrame: "⠙"})
	got, _ := footer.render(80)
	if strings.Contains(got, "working") {
		t.Fatalf("footer render = %q, should not contain active-run loader", got)
	}
}

func TestThinkingRowUsesThrobberFrame(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hi")
	conv.AddThinking("⠙")
	messages := conv.Messages()
	if got := messages[len(messages)-1].Text; got != "⠙ Thinking..." {
		t.Fatalf("thinking label = %q, want throbber label", got)
	}

	conv.SetThinkingFrame("⠹")
	messages = conv.Messages()
	if got := messages[len(messages)-1].Text; got != "⠹ Thinking..." {
		t.Fatalf("updated thinking label = %q, want advanced throbber label", got)
	}
}
