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

func TestInputComponentTypingPreservesUnicodeFormatCharacters(t *testing.T) {
	c := NewInputComponent()
	value := "👩\u200d💻ش\u200cم\u00a0\u2028\U000e0100"
	for _, r := range value {
		assert.True(t, c.Handle(KeyEvent{Key: KeyRune, Rune: r}, nil), "U+%04X", r)
	}
	assert.Equal(t, value, c.Value())
}

func TestInputComponentRenderBoundsPromptAndWideCursor(t *testing.T) {
	for _, tc := range []struct {
		prompt, value string
		width, cursor int
		want          string
	}{
		{"> ", "abc", 1, 0, ">"}, {"> ", "abc", 2, 0, "> "}, {"> ", "界x", 3, 0, ">  "}, {"> ", "界x", 4, 0, "> 界"},
	} {
		c := NewInputComponent()
		c.Prompt = tc.prompt
		c.SetValue(tc.value)
		c.Model.SetCursor(tc.cursor)
		rows := c.Render(tc.width)
		assert.Len(t, rows, 1)
		assert.LessOrEqual(t, TextWidth(rows[0]), tc.width)
		assert.Equal(t, tc.want, StripANSI(rows[0]))
	}
}

func TestInputComponentRenderCentersScrollingAroundCursor(t *testing.T) {
	c := NewInputComponent()
	c.SetValue("abcdefghijklmnop")
	c.Model.SetCursor(8)
	assert.Equal(t, "> fghijk", StripANSI(c.Render(8)[0]))
	c.Model.SetCursor(0)
	assert.Equal(t, "> abcdef", StripANSI(c.Render(8)[0]))
	c.Model.End()
	assert.Equal(t, "> lmnop ", StripANSI(c.Render(8)[0]))
	c.SetValue("界abcdefghij")
	c.Model.SetCursor(len("界abcd"))
	assert.Equal(t, "> bcdefg", StripANSI(c.Render(8)[0]))
}

func TestInputComponentUsesModelEditingKeysAndPaste(t *testing.T) {
	c := NewInputComponent()
	c.SetValue("one two")
	c.Model.SetCursor(4)
	assert.True(t, c.Handle(KeyEvent{Key: KeyRune, Rune: 'k', Mod: ModCtrl}, nil))
	assert.Equal(t, "one ", c.Value())
	assert.True(t, c.Handle(KeyEvent{Key: KeyRune, Rune: 'y', Mod: ModCtrl}, nil))
	assert.Equal(t, "one two", c.Value())
	assert.True(t, c.Handle(KeyEvent{Key: KeyLeft, Mod: ModCtrl}, nil))
	assert.Equal(t, 4, c.Model.Cursor())
	assert.True(t, c.Handle(KeyEvent{Key: KeyRune, Rune: '-', Mod: ModCtrl}, nil))
	assert.Equal(t, "one ", c.Value())
	assert.True(t, c.Handle(PasteEvent{Text: "a\nb"}, nil))
	assert.Equal(t, "one ab", c.Value())
	assert.True(t, c.Handle(KeyEvent{Key: KeyRune, Rune: '-', Mod: ModCtrl}, nil))
	assert.Equal(t, "one ", c.Value())
	assert.False(t, c.Handle(KeyEvent{Key: KeyRune, Rune: 'q', Mod: ModCtrl}, nil))
	assert.False(t, c.Handle(KeyEvent{Key: KeyUp}, nil))
}
