package tui

import (
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/keybindings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtensionShortcutsRespectToolkitAndMessageCopyReservations(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.input.submit": {"ctrl+j"}}))
	for _, key := range []string{"ctrl+j", "ctrl+k", "ctrl+x"} {
		a, _, _ := shortcutApp(t, key)
		stroke, err := keybindings.CompileKey(key)
		require.NoError(t, err)
		km := appKeymap{app: a}
		_, ok := km.resolveExtensionShortcut(stroke)
		assert.False(t, ok, "reserved %s", key)
	}
	a, _, _ := shortcutApp(t, "enter")
	_, ok := (appKeymap{app: a}).resolveExtensionShortcut(tauui.Keystroke{Key: tauui.KeyEnter})
	assert.False(t, ok, "select confirmation stays reserved even after submit changes")
	a, _, _ = shortcutApp(t, "ctrl+b")
	_, ok = (appKeymap{app: a}).resolveExtensionShortcut(tauui.Keystroke{Key: tauui.KeyRune, Rune: 'b', Mod: tauui.ModCtrl})
	assert.True(t, ok, "ordinary editing keys remain shadowable")
}
