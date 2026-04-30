package tui

import (
	"testing"
)

func TestInputPrimitiveSubmitCancelAndCursor(t *testing.T) {
	var submitted string
	cancelled := false
	input := &InputPrimitive{Prompt: "> ", OnSubmit: func(v string) { submitted = v }, OnCancel: func() { cancelled = true }}
	input.HandleInput("a")
	input.HandleInput("b")
	if got := input.Render(8); len(got) != 1 || got[0] != "> ab"+CursorMarker+ansiInverseOn+" "+ansiInverseOff+"   " {
		t.Fatalf("Render = %#v", got)
	}
	input.HandleInput("\x7f")
	input.HandleInput("\r")
	if submitted != "a" {
		t.Fatalf("submitted = %q, want a", submitted)
	}
	input.HandleInput("\x1b")
	if !cancelled {
		t.Fatalf("cancel callback not called")
	}
}

func TestConfirmPrimitiveDone(t *testing.T) {
	var got bool
	confirm := &ConfirmPrimitive{Message: "Proceed?", OnDone: func(v bool) { got = v }}
	if rows := confirm.Render(14); len(rows) != 1 || rows[0] != "Proceed? [y/N]" {
		t.Fatalf("Render = %#v", rows)
	}
	confirm.HandleInput("y")
	if !got {
		t.Fatalf("yes did not confirm")
	}
	confirm.HandleInput("n")
	if got {
		t.Fatalf("no did not cancel")
	}
}

func TestOverlayPlacementWrapperClampsMarginOffsetAndMaxHeight(t *testing.T) {
	base := fixedPlacement{rect: Rect{X: 18, Y: 8, W: 10, H: 8}}
	wrapped := placementWrapper{base: base, margin: 2, offset: Point{X: 3, Y: 3}, maxHeight: 3}
	got := wrapped.Resolve(NewText("overlay"), Size{W: 20, H: 10})
	want := Rect{X: 8, Y: 5, W: 10, H: 3}
	if got != want {
		t.Fatalf("Resolve = %#v, want %#v", got, want)
	}
}

func TestEdgeAnchoredSupportsPercentSizeMinAndAnchors(t *testing.T) {
	placement := EdgeAnchored{Anchor: AnchorBottomRight, Width: Percent(50), Height: Cells(4), MinW: 12}
	got := placement.Resolve(NewText("overlay"), Size{W: 40, H: 20})
	want := Rect{X: 20, Y: 16, W: 20, H: 4}
	if got != want {
		t.Fatalf("Resolve = %#v, want %#v", got, want)
	}
}

func TestEditorPrimitiveWrapsScrollsAndMovesVisualLines(t *testing.T) {
	ed := &EditorPrimitive{MaxRows: 2}
	ed.HandleInput("a")
	ed.HandleInput("b")
	ed.HandleInput("c")
	ed.HandleInput("d")
	ed.HandleInput("e")
	ed.HandleInput("f")
	rows := ed.Render(3)
	if len(rows) != 2 || rows[0] != "abc" || rows[1] != "def"+CursorMarker {
		t.Fatalf("rows=%#v", rows)
	}
	ed.HandleInput("\x1b[A")
	if got := ed.ensureModel().Position(); got.Line != 0 || got.Col != 3 {
		t.Fatalf("visual up pos=%+v", got)
	}
}

func TestRenderOverlayRowsTruncatesButPreservesImages(t *testing.T) {
	seq := "\x1b_Ga=T;data\x1b\\"
	rows := RenderOverlayRows(staticRows{rows: []string{"abcdef", seq}}, 3)
	if len(rows) != 2 || TextWidth(rows[0]) != 3 || rows[1] != seq {
		t.Fatalf("rows = %#v", rows)
	}
}

type fixedPlacement struct{ rect Rect }

func (f fixedPlacement) Resolve(View, Size) Rect { return f.rect }

type staticRows struct{ rows []string }

func (s staticRows) Render(int) []string { return append([]string(nil), s.rows...) }
