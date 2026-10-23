package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStatusEntriesViewPaintsMutedTextOnBaseBackground(t *testing.T) {
	view := NewStatusEntriesView(5)
	view.SetEntries(map[string]string{"compaction": "Working"})
	rect := Rect{W: 30, H: 1}
	view.Layout(rect)
	theme := DefaultThemeModern()
	theme.Base = Style{FG: ColorRGB(1, 2, 3), BG: ColorRGB(4, 5, 6)}
	theme.Palette.TextMuted = Style{FG: ColorRGB(7, 8, 9), BG: ColorRGB(10, 11, 12), Attr: AttrDim}
	theme.Palette.SurfaceMuted = Style{FG: ColorRGB(13, 14, 15)}
	buf := NewBuffer(rect.W, rect.H)
	painter := NewRenderPainter(buf, rect, theme.Base)
	view.Paint(NewDrawer(NewPainter(painter, theme.Base)), &Ctx{Theme: theme})
	st := buf.At(13, 0).Style
	assert.Equal(t, theme.Palette.TextMuted.FG, st.FG)
	assert.NotZero(t, st.Attr&AttrDim)
	assert.Equal(t, theme.Base.BG, st.BG)
	theme.Palette.TextMuted = Style{}
	view.Paint(NewDrawer(NewPainter(painter, theme.Base)), &Ctx{Theme: theme})
	assert.Equal(t, theme.Base, buf.At(13, 0).Style)
}
