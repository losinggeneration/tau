package tui

import (
	"testing"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
	"github.com/losinggeneration/tau/tui/keybindings"
)

func TestKeybindingKeymapRoutesConfiguredActions(t *testing.T) {
	manager := keybindings.NewDefault(keybindings.Config{"tui.input.submit": {"ctrl+x"}})
	km := NewKeybindingKeymap(manager, map[string]Action{"tui.input.submit": ActionSubmit})

	got, ok := km.Resolve(ui.KeyCtxTextInput, ui.Keystroke{Key: event.KeyRune, Rune: 'x', Mod: event.ModCtrl})
	if !ok || got != ActionSubmit {
		t.Fatalf("Resolve(ctrl+x) = (%v, %v), want submit/true", got, ok)
	}
}
