package tui

import (
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
	"github.com/stretchr/testify/assert"
)

func TestTerminalInputTextUsesProducedCharacter(t *testing.T) {
	assert.Equal(t, "+", terminalInputText(tauui.KeyEvent{Key: tauui.KeyRune, Rune: '=', ShiftedRune: '+', Mod: tauui.ModShift | tauui.ModCtrl}))
	assert.Equal(t, "=", terminalInputText(tauui.KeyEvent{Key: tauui.KeyRune, Rune: '=', ShiftedRune: '+'}))
}
