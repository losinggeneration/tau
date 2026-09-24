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

// TestMarkdownRenderWrappedMarksSoftWrapContinuations pins the wrap state
// RenderWrapped reports: rows a word wrap produced beyond the first carry
// Cont=true, while rows that start a new source line do not. Copy/reflow
// consumers need this to join wrapped rows without inventing line breaks.
func TestMarkdownRenderWrappedMarksSoftWrapContinuations(t *testing.T) {
	md := NewMarkdown("alpha beta gamma delta epsilon zeta")
	rows := md.RenderWrapped(12)
	if len(rows) < 3 {
		t.Fatalf("RenderWrapped rows = %#v, want at least 3 wrapped rows", rows)
	}
	for i, row := range rows {
		if i == 0 {
			if row.Cont {
				t.Fatalf("first row marked as continuation: %#v", rows)
			}
			continue
		}
		if !row.Cont {
			t.Fatalf("row %d (%q) not marked as continuation: %#v", i, row.Text, rows)
		}
	}

	two := NewMarkdown("short\n\nalso short").RenderWrapped(40)
	for i, row := range two {
		if row.Cont {
			t.Fatalf("separate source rows must not be continuations: row %d = %#v", i, row)
		}
	}
}

// TestMarkdownRenderDelegatesToRenderWrapped keeps Render an exact projection
// of RenderWrapped's text so the two paths can never drift.
func TestMarkdownRenderDelegatesToRenderWrapped(t *testing.T) {
	md := NewMarkdown("# Title\n- one\nsome long paragraph that wraps at a narrow width for sure")
	for _, width := range []int{5, 12, 40} {
		got := md.Render(width)
		wrapped := md.RenderWrapped(width)
		want := make([]string, len(wrapped))
		for i, row := range wrapped {
			want[i] = row.Text
		}
		if !equalStrings(got, want) {
			t.Fatalf("Render(%d) = %#v, want %#v", width, got, want)
		}
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
