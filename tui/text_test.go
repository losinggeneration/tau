package tui

import (
	"testing"

	"github.com/losinggeneration/rovel"
)

func TestText_Empty_RendersNothing(t *testing.T) {
	tr := NewText("")

	// Empty text reports no rows.
	tr.Layout(Rect{X: 0, Y: 0, W: 10, H: 5})

	rec := &recordingDrawer{}
	tr.Paint(rec, &Ctx{})

	if len(rec.lines) != 0 {
		t.Errorf("empty text painted %d lines, want 0", len(rec.lines))
	}
}

func TestText_WhitespaceOnly_RendersNothing(t *testing.T) {
	tr := NewText("   \t\t  \n  ")
	tr.Layout(Rect{X: 0, Y: 0, W: 10, H: 5})

	rec := &recordingDrawer{}
	tr.Paint(rec, &Ctx{})

	if len(rec.lines) != 0 {
		t.Errorf("whitespace text painted %d lines, want 0", len(rec.lines))
	}
}

func TestText_SimplePaint(t *testing.T) {
	tr := NewText("hello")
	tr.Layout(Rect{X: 0, Y: 0, W: 10, H: 1})

	rec := &recordingDrawer{}
	tr.Paint(rec, &Ctx{})

	if len(rec.lines) != 1 {
		t.Fatalf("painted %d lines, want 1", len(rec.lines))
	}

	got := rec.lines[0]
	if got != "hello     " {
		t.Errorf("painted %q, want %q (padded to width)", got, "hello     ")
	}
}

func TestText_TabExpansion(t *testing.T) {
	tr := NewText("a\tb")
	tr.Layout(Rect{X: 0, Y: 0, W: 10, H: 1})

	rec := &recordingDrawer{}
	tr.Paint(rec, &Ctx{})

	if len(rec.lines) != 1 {
		t.Fatalf("painted %d lines, want 1", len(rec.lines))
	}

	// Tabs render as three spaces.
	got := rec.lines[0]
	want := "a   b     "
	if got != want {
		t.Errorf("painted %q, want %q (tab expanded to 3 spaces)", got, want)
	}
}

func TestText_NewlineProducesMultipleLines(t *testing.T) {
	tr := NewText("a\nb\nc")
	tr.Layout(Rect{X: 0, Y: 0, W: 10, H: 5})

	rec := &recordingDrawer{}
	tr.Paint(rec, &Ctx{})

	if len(rec.lines) != 3 {
		t.Fatalf("painted %d lines, want 3", len(rec.lines))
	}
}

func TestText_MinSize_NoText(t *testing.T) {
	tr := NewText("")

	got := tr.MinSize()
	if got.W < 1 || got.H < 1 {
		t.Errorf("MinSize() = %v, want positive", got)
	}
}

func TestText_MinSize_WithText(t *testing.T) {
	tr := NewText("hello")

	got := tr.MinSize()
	if got.W < 5 {
		t.Errorf("MinSize().W = %d, want >= 5", got.W)
	}

	if got.H < 1 {
		t.Errorf("MinSize().H = %d, want >= 1", got.H)
	}
}

func TestText_VerticalPadding(t *testing.T) {
	tr := NewText("hello")
	tr.SetVerticalPadding(2)
	tr.Layout(Rect{X: 0, Y: 0, W: 10, H: 10})

	rec := &recordingDrawer{}
	tr.Paint(rec, &Ctx{})

	// 2 top padding + 1 text + 2 bottom padding = 5 lines.
	if len(rec.lines) != 5 {
		t.Errorf("painted %d lines, want 5 (2 top + 1 text + 2 bottom)", len(rec.lines))
	}
}

func TestText_SetText(t *testing.T) {
	tr := NewText("foo")
	tr.SetText("bar")
	tr.Layout(Rect{X: 0, Y: 0, W: 10, H: 1})

	rec := &recordingDrawer{}
	tr.Paint(rec, &Ctx{})

	if len(rec.lines) != 1 || rec.lines[0] != "bar       " {
		t.Errorf("painted %v, want [%q]", rec.lines, "bar       ")
	}
}

func TestText_SetText_EmptyThenPaint(t *testing.T) {
	tr := NewText("foo")
	tr.SetText("")
	tr.Layout(Rect{X: 0, Y: 0, W: 10, H: 1})

	rec := &recordingDrawer{}
	tr.Paint(rec, &Ctx{})

	if len(rec.lines) != 0 {
		t.Errorf("set to empty then painted %d lines, want 0", len(rec.lines))
	}
}

func TestText_HiddenDoesNotPaint(t *testing.T) {
	tr := NewText("hello")
	tr.SetVisible(false)
	tr.Layout(Rect{X: 0, Y: 0, W: 10, H: 1})

	rec := &recordingDrawer{}
	tr.Paint(rec, &Ctx{})

	if len(rec.lines) != 0 {
		t.Errorf("hidden text painted %d lines, want 0", len(rec.lines))
	}
}

func TestText_Focusable(t *testing.T) {
	tr := NewText("hello")
	if tr.Focusable() {
		t.Errorf("Text should not be focusable")
	}
}

func TestText_HandleNeverConsumes(t *testing.T) {
	tr := NewText("hello")
	if tr.Handle(KeyEvent{Key: KeyRune, Rune: 'x'}, nil) {
		t.Errorf("Text should not consume events")
	}
}

func TestText_TruncatesWhenWiderThanRect(t *testing.T) {
	tr := NewText("hello world")
	tr.Layout(Rect{X: 0, Y: 0, W: 5, H: 1})

	rec := &recordingDrawer{}
	tr.Paint(rec, &Ctx{})

	if len(rec.lines) != 1 {
		t.Fatalf("painted %d lines, want 1", len(rec.lines))
	}

	if got := rec.lines[0]; runeWidth(got) > 5 {
		t.Errorf("painted line rune-width = %d, want <= 5 (truncated)", runeWidth(got))
	}
}

func TestText_Invalidate_BeforeLayoutIsNoOp(t *testing.T) {
	tr := NewText("hello")
	// Should not panic even though rect is empty.
	tr.Invalidate(&Ctx{})
}

// Spec invariant: tab width is 3 in text-component rendering, 4 in editor
// storage. This test pins down the 3-width behavior in the Text widget.
func TestText_Spec_TabIsThreeSpaces(t *testing.T) {
	tr := NewText("a\tb") // mix tab with content so it's not whitespace-only
	tr.Layout(Rect{X: 0, Y: 0, W: 5, H: 1})

	rec := &recordingDrawer{}
	tr.Paint(rec, &Ctx{})

	if len(rec.lines) != 1 {
		t.Fatalf("painted %d lines, want 1", len(rec.lines))
	}

	// "a" + 3 spaces + "b" = 5 cells, padded to 5.
	if got, want := rec.lines[0], "a   b"; got != want {
		t.Errorf("painted %q, want %q (tab expanded to 3 spaces)", got, want)
	}
}

// runeWidth returns the number of runes in s (used for width comparisons
// where byte length would be misleading for multi-byte chars).
func runeWidth(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// ensure tui import is used
var _ rovel.Style
