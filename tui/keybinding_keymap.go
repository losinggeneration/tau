package tui

import (
	"github.com/losinggeneration/rovel/ui"
	"github.com/losinggeneration/tau/tui/keybindings"
)

// KeybindingKeymap adapts the Pi-style keybindings Manager into the app-level
// semantic action resolver used by the live TUI.
type KeybindingKeymap struct {
	Manager *keybindings.Manager
	Actions map[string]Action
}

// NewKeybindingKeymap returns a Keymap backed by manager. Unknown actions are
// ignored, allowing higher-level apps to pass a larger registry safely.
func NewKeybindingKeymap(manager *keybindings.Manager, actions map[string]Action) KeybindingKeymap {
	return KeybindingKeymap{Manager: manager, Actions: actions}
}

// Resolve implements Keymap.
func (m KeybindingKeymap) Resolve(_ ui.KeyContext, k ui.Keystroke) (Action, bool) {
	manager := m.Manager
	if manager == nil {
		manager = keybindings.DefaultManager()
	}
	router := keybindings.NewActionRouter(manager)
	id, ok := router.Resolve(k)
	if !ok {
		return ActionNone, false
	}
	act, ok := m.Actions[id]
	if !ok {
		return ActionNone, false
	}

	return act, true
}
