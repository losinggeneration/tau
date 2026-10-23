package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPrimitivesInsertShiftedCharacter(t *testing.T) {
	input := &InputPrimitive{}
	editor := &EditorPrimitive{}
	key := KeyEvent{Key: KeyRune, Rune: '=', ShiftedRune: '+', Mod: ModShift}
	input.HandleKey(key)
	editor.HandleKey(key)
	assert.Equal(t, "+", input.Value)
	assert.Equal(t, "+", editor.ensureModel().Text())
	key.Mod |= ModCtrl
	input.HandleKey(key)
	editor.HandleKey(key)
	assert.Equal(t, "+", input.Value)
	assert.Equal(t, "+", editor.ensureModel().Text())
}

func TestInputPrimitivePasteNormalizesAndReplacesSelection(t *testing.T) {
	p := &InputPrimitive{Value: "before after"}
	p.SetSelection(0, 6)
	p.Paste("a\r\n\tb\x00\x07")
	assert.Equal(t, "a b after", p.Value)
	p.HandleKey(KeyEvent{Key: KeyHome})
	p.HandleKey(KeyEvent{Key: KeyDelete})
	p.Paste("x")
	assert.Equal(t, "x b after", p.Value)
}
