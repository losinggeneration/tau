package tui

import "testing"

func TestFixedLineViewPaintsRenderedRow(t *testing.T) {
	v := NewFixedLineView(func(width int) (string, Style) {
		if width != 5 {
			t.Fatalf("render width = %d, want 5", width)
		}
		return "hello", Style{}
	})
	v.Layout(Rect{W: 5, H: 1})
	buf := NewBuffer(5, 1)
	p := NewRenderPainter(buf, Rect{W: 5, H: 1}, Style{})
	v.Paint(NewDrawer(NewPainter(p, Style{})), &Ctx{Theme: DefaultThemeModern()})
	if got := bufferLine(buf, 5); got != "hello" {
		t.Fatalf("painted row = %q", got)
	}
}

func TestStatusEntriesViewCollapsesAndSorts(t *testing.T) {
	v := NewStatusEntriesView(5)
	if got := v.MinSize(); got != (Size{}) {
		t.Fatalf("empty MinSize = %+v, want zero", got)
	}
	v.SetEntries(map[string]string{"b": "two", "a": "one"})
	if got := v.MinSize().H; got != 2 {
		t.Fatalf("MinSize.H = %d, want 2", got)
	}
	v.Layout(Rect{W: 20, H: 2})
	buf := NewBuffer(20, 2)
	p := NewRenderPainter(buf, Rect{W: 20, H: 2}, Style{})
	v.Paint(NewDrawer(NewPainter(p, Style{})), &Ctx{Theme: DefaultThemeModern()})
	if got := bufferLine(buf, 20); got[:2] != "a:" {
		t.Fatalf("first row = %q, want sorted a first", got)
	}
}

func TestStatusEntriesViewUsesDefaultBackground(t *testing.T) {
	v := NewStatusEntriesView(5)
	v.SetEntries(map[string]string{"agent": "⠋ Working..."})
	v.Layout(Rect{W: 30, H: 1})

	theme := DefaultThemeModern()
	buf := NewBuffer(30, 1)
	p := NewRenderPainter(buf, Rect{W: 30, H: 1}, theme.Base)
	v.Paint(NewDrawer(NewPainter(p, theme.Base)), &Ctx{Theme: theme})

	for x := range 30 {
		cell := buf.At(x, 0)
		if cell.Style.BG != theme.Base.BG {
			t.Fatalf("cell %d background = %v, want default background %v", x, cell.Style.BG, theme.Base.BG)
		}
	}
}

func bufferLine(buf *Buffer, width int) string {
	out := make([]rune, 0, width)
	for x := range width {
		cell := buf.At(x, 0)
		if cell == nil || cell.R == 0 {
			out = append(out, ' ')
		} else {
			out = append(out, cell.R)
		}
	}
	return string(out)
}
