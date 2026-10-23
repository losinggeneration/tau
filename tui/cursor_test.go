package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

type cursorRecordingDrawer struct {
	Drawer
	cursor  Point
	visible bool
}

func (d *cursorRecordingDrawer) SetCursor(pos Point) { d.cursor = pos; d.visible = true }

func TestCursorMarkersAreZeroWidthAndResolveBeforeTruncation(t *testing.T) {
	row := "ab" + cursorString("界", true) + "c"
	assert.Equal(t, 5, TextWidth(row))
	for _, width := range []int{4, 5} {
		d := &cursorRecordingDrawer{Drawer: &ansiRecordingDrawer{}}
		DrawANSIText(d, Point{Y: 1}, ANSITruncate(row, width, false), Style{})
		assert.True(t, d.visible)
		assert.Equal(t, Point{X: 2, Y: 1}, d.cursor)
		recorder := d.Drawer.(*ansiRecordingDrawer)
		var plain string
		for _, op := range recorder.ops {
			plain += op.text
			assert.Zero(t, op.st.Attr&AttrReverse)
		}
		assert.Equal(t, map[int]string{4: "ab界", 5: "ab界c"}[width], plain)
	}
}

func TestFakeCursorResolutionPreservesUnfocusedAndOtherReverseSpans(t *testing.T) {
	for _, row := range []string{
		CursorMarker + fakeCursorStart + "a" + fakeCursorEnd + ansiInverseOn + "b" + ansiInverseOff,
		CursorMarker + fakeCursorStart + "a" + fakeCursorEnd + ansiInverseOn + "b" + ansiInverseOff + "c",
	} {
		d := &cursorRecordingDrawer{Drawer: &ansiRecordingDrawer{}}
		DrawANSIText(d, Point{}, row, Style{})
		ops := d.Drawer.(*ansiRecordingDrawer).ops
		assert.Zero(t, ops[0].st.Attr&AttrReverse)
		assert.NotZero(t, ops[1].st.Attr&AttrReverse)
	}
	d := &cursorRecordingDrawer{Drawer: &ansiRecordingDrawer{}}
	drawCursorRow(d, Point{}, CursorMarker+fakeCursorStart+"x"+fakeCursorEnd, Style{}, false)
	assert.False(t, d.visible)
	assert.NotZero(t, d.Drawer.(*ansiRecordingDrawer).ops[0].st.Attr&AttrReverse)
	d = &cursorRecordingDrawer{Drawer: &ansiRecordingDrawer{}}
	DrawANSIText(d, Point{}, ANSITruncate("ab"+cursorString("c", true)+"def", 3, false), Style{})
	assert.True(t, d.visible)
	assert.Equal(t, Point{X: 2}, d.cursor)
	for _, op := range d.Drawer.(*ansiRecordingDrawer).ops {
		assert.Zero(t, op.st.Attr&AttrReverse)
	}
}
