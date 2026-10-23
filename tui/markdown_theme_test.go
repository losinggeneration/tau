package tui

import (
	"strings"
	"testing"
)

func TestMarkdownStyleRestoresColorAndFaintAfterNestedReset(t *testing.T) {
	for _, color := range []Color{ColorDefault, ColorRed, ColorBrightWhite, ColorIndex(201), ColorRGB(12, 34, 56)} {
		st := Style{FG: color, Attr: AttrDim | AttrBold | AttrUnderline}
		row := markdownStyle("before "+ansiItalic+"inner"+ansiReset+" after", st)
		d := &ansiRecordingDrawer{}
		DrawANSIText(d, Point{}, row+" outside", Style{FG: ColorGreen, BG: ColorBlack})
		for _, op := range d.ops {
			if strings.TrimSpace(op.text) == "" {
				continue
			}
			if strings.Contains(op.text, "outside") {
				if op.st.FG != ColorGreen || op.st.Attr != 0 {
					t.Fatalf("outer style leaked: %+v", op)
				}
				continue
			}
			if op.st.FG != color || op.st.Attr&st.Attr != st.Attr || op.st.BG != ColorBlack {
				t.Fatalf("nested role style lost: %+v", op)
			}
		}
	}
}

func TestMarkdownThemedQuotesWrapWithStyledBorders(t *testing.T) {
	md := NewMarkdown("> Long **bold** quoted text that wraps across several rows")
	md.SetTheme(MarkdownTheme{Quote: Style{FG: ColorRed}, QuoteBorder: Style{FG: ColorBlue}})
	rows := md.Render(12)
	if len(rows) < 3 {
		t.Fatalf("rows=%q", rows)
	}
	for _, row := range rows {
		if strings.TrimSpace(StripANSI(row)) == "" {
			continue
		}
		if TextWidth(row) > 12 || !strings.HasPrefix(StripANSI(row), "│ ") {
			t.Fatalf("quote lost border or width: %q", row)
		}
		d := &ansiRecordingDrawer{}
		DrawANSIText(d, Point{}, row, Style{})
		if len(d.ops) == 0 || d.ops[0].st.FG != ColorBlue {
			t.Fatalf("quote border color lost: %+v", d.ops)
		}
	}
}
