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

func TestDrawANSITextFaintAndIntensityReset(t *testing.T) {
	d := &ansiRecordingDrawer{}
	DrawANSIText(d, Point{}, "\x1b[2mfaint\x1b[22mnormal", Style{Attr: AttrItalic})
	if len(d.ops) != 2 {
		t.Fatalf("ops=%+v", d.ops)
	}
	if d.ops[0].st.Attr != AttrItalic|AttrDim || d.ops[1].st.Attr != AttrItalic {
		t.Fatalf("styles=%+v", d.ops)
	}
}

func TestDrawANSITextThemeColors(t *testing.T) {
	base := Style{FG: ColorWhite, BG: ColorBlack, Attr: AttrItalic | AttrDim}
	for _, tt := range []struct {
		seq  string
		want Color
	}{
		{"31", ColorRed}, {"97", ColorBrightWhite}, {"38;5;197", ColorIndex(197)}, {"38;2;12;34;56", ColorRGB(12, 34, 56)}, {"39", ColorDefault},
	} {
		t.Run(tt.seq, func(t *testing.T) {
			d := &ansiRecordingDrawer{}
			DrawANSIText(d, Point{}, "\x1b[22;"+tt.seq+"mcolor\x1b[0mbase", base)
			if len(d.ops) != 2 || d.ops[0].st.FG != tt.want || d.ops[0].st.BG != base.BG || d.ops[0].st.Attr != AttrItalic || d.ops[1].st != base {
				t.Fatalf("ops=%+v", d.ops)
			}
		})
	}
}

func TestDrawANSITextBackgroundColors(t *testing.T) {
	base := Style{FG: ColorWhite, BG: ColorBlack}
	for _, tt := range []struct {
		seq  string
		want Color
	}{
		{"41", ColorRed}, {"107", ColorBrightWhite}, {"48;5;197", ColorIndex(197)}, {"48;2;12;34;56;31", ColorRGB(12, 34, 56)}, {"41;49", ColorBlack},
	} {
		t.Run(tt.seq, func(t *testing.T) {
			d := &ansiRecordingDrawer{}
			DrawANSIText(d, Point{}, "\x1b["+tt.seq+"mcolor", base)
			if len(d.ops) != 1 || d.ops[0].st.BG != tt.want {
				t.Fatalf("ops=%+v", d.ops)
			}
		})
	}
}
