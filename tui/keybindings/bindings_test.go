package keybindings

import (
	"testing"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompileKeyPlus(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  event.ModMask
	}{
		{"+", 0}, {"ctrl++", event.ModCtrl}, {"shift+alt+control++", event.ModShift | event.ModAlt | event.ModCtrl},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompileKey(tc.name)
			require.NoError(t, err)
			assert.Equal(t, ui.Keystroke{Key: event.KeyRune, Rune: '+', Mod: tc.mod}, got)
		})
	}
	for _, bad := range []string{"ctrl+", "++", "ctrl+++", "ctrl+shift+", "ctrl+ctrl++", "shift+shift+a"} {
		_, err := CompileKey(bad)
		assert.Error(t, err, bad)
	}
}

func TestShiftedSymbolsMatchPhysicalAndProducedKeys(t *testing.T) {
	for _, tc := range []struct {
		key              ui.Keystroke
		matches, rejects []string
	}{
		{ui.Keystroke{Key: event.KeyRune, Rune: '=', ShiftedRune: '+', Mod: event.ModShift}, []string{"+", "shift+="}, []string{"=", "ctrl++", "shift++"}},
		{ui.Keystroke{Key: event.KeyRune, Rune: '=', ShiftedRune: '+', Mod: event.ModCtrl | event.ModShift}, []string{"ctrl++", "ctrl+shift+="}, []string{"+", "ctrl+=", "alt++"}},
		{ui.Keystroke{Key: event.KeyRune, Rune: '+', ShiftedRune: '+', Mod: event.ModCtrl | event.ModShift}, []string{"ctrl++", "ctrl+shift++"}, []string{"+", "ctrl+="}},
		{ui.Keystroke{Key: event.KeyRune, Rune: '[', ShiftedRune: '{', Mod: event.ModAlt | event.ModShift}, []string{"alt+{", "alt+shift+["}, []string{"{", "alt+["}},
		{ui.Keystroke{Key: event.KeyRune, Rune: 'E', ShiftedRune: 'E', Mod: event.ModCtrl | event.ModShift}, []string{"ctrl+shift+e"}, []string{"ctrl+e", "e"}},
		{ui.Keystroke{Key: event.KeyRune, Rune: '=', ShiftedRune: '+', Mod: 0}, []string{"="}, []string{"+"}},
	} {
		for _, binding := range tc.matches {
			compiled, err := CompileKey(binding)
			require.NoError(t, err)
			assert.True(t, KeystrokesMatch(tc.key, compiled), "%+v should match %s", tc.key, binding)
			manager := New(map[string]Definition{"symbol": {DefaultKeys: []string{binding}}}, nil)
			assert.True(t, manager.Matches(tc.key, "symbol"))
		}
		for _, binding := range tc.rejects {
			compiled, err := CompileKey(binding)
			require.NoError(t, err)
			assert.False(t, KeystrokesMatch(tc.key, compiled), "%+v should not match %s", tc.key, binding)
		}
	}
}
