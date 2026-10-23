package tui

import (
	"strings"
	"testing"
)

func TestDynamicBorderRowsAreExactlyWidth(t *testing.T) {
	for _, width := range []int{4, 10, 40, 81} {
		b := DynamicBorder{Title: " bash · running ", Width: width}
		rows := map[string]string{
			"top":    b.Top(),
			"rule":   b.Rule(),
			"bottom": b.Bottom(),
			"line":   b.Line("some content"),
			"empty":  b.Line(""),
			"long":   b.Line(strings.Repeat("x", width*2)),
		}
		for name, row := range rows {
			if got := TextWidth(row); got != width {
				t.Fatalf("width %d: %s row is %d cells wide (%q)", width, name, got, row)
			}
		}
	}
}

func TestDynamicBorderCorners(t *testing.T) {
	b := DynamicBorder{Width: 10}
	if !strings.HasPrefix(b.Top(), "╭") || !strings.HasSuffix(b.Top(), "╮") {
		t.Fatalf("top = %q, want rounded corners", b.Top())
	}
	if !strings.HasPrefix(b.Bottom(), "╰") || !strings.HasSuffix(b.Bottom(), "╯") {
		t.Fatalf("bottom = %q, want rounded corners", b.Bottom())
	}
	if !strings.HasPrefix(b.Rule(), "├") || !strings.HasSuffix(b.Rule(), "┤") {
		t.Fatalf("rule = %q, want tee ends", b.Rule())
	}
	line := b.Line("hi")
	if !strings.HasPrefix(line, "│") || !strings.HasSuffix(line, "│") {
		t.Fatalf("line = %q, want vertical edges", line)
	}
}

func TestDynamicBorderTitleIsInlinedAndTruncated(t *testing.T) {
	b := DynamicBorder{Title: " bash · running ", Width: 30}
	if !strings.Contains(b.Top(), "bash · running") {
		t.Fatalf("top = %q, want the title inlined", b.Top())
	}

	narrow := DynamicBorder{Title: " a very long title indeed ", Width: 12}
	if got := TextWidth(narrow.Top()); got != 12 {
		t.Fatalf("narrow top is %d cells wide (%q), want 12", got, narrow.Top())
	}
}

// Degenerate widths must not panic or produce oversized rows: the chat log
// renders at whatever width the terminal reports.
func TestDynamicBorderDegenerateWidths(t *testing.T) {
	for _, width := range []int{-5, 0, 1, 2, 3} {
		b := DynamicBorder{Title: "title", Width: width}
		for _, row := range []string{b.Top(), b.Rule(), b.Bottom(), b.Line("content")} {
			if TextWidth(row) > max(width, 0) {
				t.Fatalf("width %d: row %q exceeds the box", width, row)
			}
		}
	}
}

func TestDynamicBorderWrapProducesCompleteBox(t *testing.T) {
	b := DynamicBorder{Title: " section ", Width: 20}
	rows := b.Wrap([]string{"one", "a much longer line that has to wrap across rows", ""})

	if len(rows) < 4 {
		t.Fatalf("Wrap produced %d rows, want a top, body and bottom", len(rows))
	}
	if rows[0] != b.Top() {
		t.Fatalf("first row = %q, want the top edge", rows[0])
	}
	if rows[len(rows)-1] != b.Bottom() {
		t.Fatalf("last row = %q, want the bottom edge", rows[len(rows)-1])
	}
	for i, row := range rows {
		if got := TextWidth(row); got != 20 {
			t.Fatalf("row %d is %d cells wide (%q), want 20", i, got, row)
		}
	}
}

func TestDynamicBorderInnerWidth(t *testing.T) {
	if got := (DynamicBorder{Width: 40}).InnerWidth(); got != 36 {
		t.Fatalf("InnerWidth at 40 = %d, want 36 (two edges plus their padding)", got)
	}
	if got := (DynamicBorder{Width: 1}).InnerWidth(); got < 1 {
		t.Fatalf("InnerWidth must stay positive, got %d", got)
	}
}

func TestDynamicBorderConfigurablePadding(t *testing.T) {
	for _, pad := range []int{0, 3} {
		border := DynamicBorder{Width: 20, PaddingX: new(pad)}
		if border.InnerWidth() != 18-2*pad {
			t.Fatalf("inner width=%d", border.InnerWidth())
		}
		row := border.Line("text")
		if !strings.HasPrefix(row, "│"+strings.Repeat(" ", pad)+"text") || TextWidth(row) != 20 {
			t.Fatalf("pad %d: %q", pad, row)
		}
	}
}
