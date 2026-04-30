package tui

import (
	"testing"

	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/style"
)

type ansiDrawOp struct {
	pos  geom.Point
	text string
	st   style.Style
}

type ansiRecordingDrawer struct {
	ops []ansiDrawOp
	raw []ansiDrawOp
}

func (d *ansiRecordingDrawer) FillRect(geom.Rect, style.Style) {}
func (d *ansiRecordingDrawer) DrawBorder(geom.Rect, BoxStyle)  {}
func (d *ansiRecordingDrawer) ClipRect() geom.Rect             { return geom.Rect{W: 200, H: 10} }
func (d *ansiRecordingDrawer) WithClip(_ geom.Rect, fn func(Drawer)) {
	fn(d)
}

func (d *ansiRecordingDrawer) WithOffset(_, _ int, fn func(Drawer)) {
	fn(d)
}

func (d *ansiRecordingDrawer) DrawText(pos geom.Point, text string, st style.Style) {
	d.ops = append(d.ops, ansiDrawOp{pos: pos, text: text, st: st})
}

func (d *ansiRecordingDrawer) DrawRaw(pos geom.Point, text string) {
	d.raw = append(d.raw, ansiDrawOp{pos: pos, text: text})
}

func TestMarkdownPaintMapsANSISGRToRovelStyles(t *testing.T) {
	md := NewMarkdown("**bold** *italic* __under__")
	md.Layout(Rect{X: 0, Y: 0, W: 80, H: 1})
	d := &ansiRecordingDrawer{}

	md.Paint(d, nil)

	assertStyledChunk := func(text string, attr style.AttrMask) {
		t.Helper()
		for _, op := range d.ops {
			if op.text == text && op.st.Attr&attr != 0 {
				return
			}
		}
		t.Fatalf("missing styled chunk %q attr=%v ops=%#v", text, attr, d.ops)
	}

	assertStyledChunk("bold", style.AttrBold)
	assertStyledChunk("italic", style.AttrItalic)
	assertStyledChunk("under", style.AttrUnderline)
}
