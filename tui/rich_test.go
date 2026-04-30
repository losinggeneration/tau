package tui

import (
	"strings"
	"testing"
)

func TestTextUtilities(t *testing.T) {
	if got := TextWidth("界"); got != 2 {
		t.Fatalf("TextWidth(wide) = %d, want 2", got)
	}

	truncated := Truncate("hello", 3, true)
	if TextWidth(truncated) > 3 {
		t.Fatalf("Truncate width = %d, want <= 3", TextWidth(truncated))
	}

	wrapped := Wrap("abcdef", 3)
	if !equalStrings(wrapped, []string{"abc", "def"}) {
		t.Fatalf("Wrap = %#v, want abc/def", wrapped)
	}
}

func TestMarkdown_EmptyRendersNothing(t *testing.T) {
	md := NewMarkdown(" \n\t")
	if got := md.Render(10); got != nil {
		t.Fatalf("Render whitespace = %#v, want nil", got)
	}
}

func TestMarkdown_WrapsPadsAndExpandsTabs(t *testing.T) {
	md := NewMarkdown("a\tbcd")
	got := md.Render(4)
	want := []string{"a   ", "bcd "}
	if !equalStrings(got, want) {
		t.Fatalf("Render = %#v, want %#v", got, want)
	}
}

func TestMarkdown_HeadingsListsAndCode(t *testing.T) {
	md := NewMarkdown("# Title\n- one\n```\nfmt.Println(1)\n```")
	got := md.Render(40)
	joined := strings.Join(got, "\n")
	for _, want := range []string{"Title", "- one", "```", "  fmt.Println(1)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Render output %q does not contain %q", joined, want)
		}
	}
}

func TestImage_FallbackTextAndRender(t *testing.T) {
	img := NewImage("cat.png", "image/png", 800, 600)
	fallback := img.FallbackText()
	for _, want := range []string{"cat.png", "image/png", "800x600"} {
		if !strings.Contains(fallback, want) {
			t.Fatalf("FallbackText %q does not contain %q", fallback, want)
		}
	}

	got := img.Render(12)
	if len(got) == 0 {
		t.Fatal("Image.Render returned no rows")
	}
	for _, row := range got {
		if TextWidth(row) != 12 {
			t.Fatalf("row %q width = %d, want 12", row, TextWidth(row))
		}
	}
}
