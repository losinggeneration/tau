package export

import (
	"strings"
	"testing"
)

func TestAnsiToHTML_PlainText(t *testing.T) {
	if got := AnsiToHTML("hello world"); got != "hello world" {
		t.Errorf("got %q", got)
	}
}

func TestAnsiToHTML_EscapesHTML(t *testing.T) {
	got := AnsiToHTML("<a> & <b>")
	if strings.Contains(got, "<a>") || strings.Contains(got, "<b>") {
		t.Errorf("HTML not escaped: %q", got)
	}
	if !strings.Contains(got, "&lt;a&gt;") || !strings.Contains(got, "&amp;") {
		t.Errorf("expected entities: %q", got)
	}
}

func TestAnsiToHTML_BoldRed(t *testing.T) {
	got := AnsiToHTML("\x1b[1;31merror\x1b[0m")
	if !strings.Contains(got, "font-weight:bold") {
		t.Errorf("missing bold: %q", got)
	}
	if !strings.Contains(got, "color:#cc0000") {
		t.Errorf("missing red: %q", got)
	}
	if !strings.Contains(got, "error") {
		t.Errorf("missing text: %q", got)
	}
	if !strings.HasSuffix(got, "</span>") {
		t.Errorf("span not closed: %q", got)
	}
}

func TestAnsiToHTML_8BitColor(t *testing.T) {
	got := AnsiToHTML("\x1b[38;5;196mX\x1b[0m")
	if !strings.Contains(got, "<span") || !strings.Contains(got, "X") {
		t.Errorf("got %q", got)
	}
	// 196 is in the 6×6×6 cube. Index 196: i=196-16=180; r=180/36=5, g=(180%36)/6=0, b=180%6=0 → (255, 0, 0)
	if !strings.Contains(got, "color:#ff0000") {
		t.Errorf("expected #ff0000, got %q", got)
	}
}

func TestAnsiToHTML_TrueColor(t *testing.T) {
	got := AnsiToHTML("\x1b[38;2;100;150;200mX\x1b[0m")
	if !strings.Contains(got, "color:#6496c8") {
		t.Errorf("expected #6496c8 (100,150,200), got %q", got)
	}
}

func TestAnsiToHTML_Reset(t *testing.T) {
	got := AnsiToHTML("\x1b[31mred\x1b[0mplain")
	// After reset, "plain" should not be inside a span
	if !strings.HasSuffix(got, "plain") {
		t.Errorf("expected reset to close span before 'plain': %q", got)
	}
}

func TestAnsiToHTML_StripsNonSGR(t *testing.T) {
	// CSI cursor-up: \x1b[2A — should be stripped, not rendered
	got := AnsiToHTML("a\x1b[2Ab")
	if got != "ab" {
		t.Errorf("expected 'ab', got %q", got)
	}
}
