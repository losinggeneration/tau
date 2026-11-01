package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestDrawANSITextSkipsControlsWithoutSwallowingText(t *testing.T) {
	for _, seq := range []string{"\x1b[2K", "\x1b[4G", "\x1b]8;;https://example.test\x1b\\", "\x1b]title\a", "\x1b_marker\a", "\x1bPpayload\x1b\\", "\x1b^payload\x1b\\", "\x1b(B"} {
		t.Run(seq, func(t *testing.T) {
			d := &ansiRecordingDrawer{}
			DrawANSIText(d, Point{X: 3, Y: 2}, "a"+seq+"b\x1b[31mc", Style{FG: ColorWhite})
			var text string
			for _, op := range d.ops {
				text += op.text
				assert.NotContains(t, op.text, "\x1b")
			}
			assert.Equal(t, "abc", text)
			require.NotEmpty(t, d.ops)
			assert.Equal(t, Point{X: 5, Y: 2}, d.ops[len(d.ops)-1].pos)
			assert.Equal(t, ColorRed, d.ops[len(d.ops)-1].st.FG)
		})
	}
}

func TestDrawANSITextTreatsControlPayloadsAsOpaque(t *testing.T) {
	for _, seq := range []string{"\x1b]title\x1b[31mhidden\a", "\x1b_hidden\x1b[31m\a", "\x1bP\x1b[31mhidden\x1b\\", "\x1b]title\x1b\\"} {
		d := &ansiRecordingDrawer{}
		DrawANSIText(d, Point{}, seq+"visible\a", Style{FG: ColorWhite})
		require.Len(t, d.ops, 1)
		assert.Equal(t, "visible\a", d.ops[0].text)
		assert.Equal(t, ColorWhite, d.ops[0].st.FG)
		assert.Equal(t, Point{}, d.ops[0].pos)
	}
	for _, suffix := range []string{"\x1b", "\x1b[31", "\x1b]title", "\x1b_payload", "\x1bPpayload", "\x1b("} {
		d := &ansiRecordingDrawer{}
		DrawANSIText(d, Point{}, "visible"+suffix, Style{})
		require.Len(t, d.ops, 1)
		assert.Equal(t, "visible", d.ops[0].text)
	}
}
