package tui

import (
	"testing"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/render"

	"github.com/stretchr/testify/assert"
)

func TestInputComponentInsertsShiftedCharacter(t *testing.T) {
	c := &InputComponent{}
	assert.True(t, c.Handle(KeyEvent{Key: KeyRune, Rune: '=', ShiftedRune: '+', Mod: ModShift}, nil))
	assert.Equal(t, "+", c.Value())
	assert.False(t, c.Handle(KeyEvent{Key: KeyRune, Rune: '=', ShiftedRune: '+', Mod: ModShift | ModCtrl}, nil))
	assert.Equal(t, "+", c.Value())
}

func TestInputPaintResolvesCursorWithoutLeakingMarkers(t *testing.T) {
	c := NewInputComponent()
	c.SetValue("界ab")
	c.Model.SetCursor(len("界"))
	c.Layout(Rect{X: 1, Y: 1, W: 12, H: 1})
	for _, focused := range []bool{true, false} {
		r := render.NewBuffer(20, 3)
		d := rovel.NewDrawer(rovel.NewPainter(render.NewPainter(r, Rect{W: 20, H: 3}, Style{}), Style{}))
		probe := &cursorRecordingDrawer{Drawer: d}
		ctx := &Ctx{}
		if focused {
			ctx.FocusedID = c.ID()
		}
		c.Paint(probe, ctx)
		assert.Equal(t, focused, probe.visible)
		if focused {
			assert.Equal(t, Point{X: 5, Y: 1}, probe.cursor)
		}
		assert.Equal(t, 'a', r.At(5, 1).R)
		assert.Equal(t, !focused, r.At(5, 1).Style.Attr&AttrReverse != 0)
		assert.Equal(t, 'b', r.At(6, 1).R)
	}
}
