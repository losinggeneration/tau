package keybindings

import (
	"fmt"
	"strings"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
)

// Binding is a configured key string compiled to rovel's normalized
// keystroke representation. The original key string is retained for display
// and diagnostics.
type Binding struct {
	Key       string
	Keystroke ui.Keystroke
}

// CompileKey compiles a user-facing keybinding string such as "ctrl+x" or
// "pageDown" into rovel's normalized keystroke model. It intentionally parses
// configuration names, not terminal byte protocols.
func CompileKey(key string) (ui.Keystroke, error) {
	orig := key
	key = strings.TrimSpace(key)
	if key == "" {
		return ui.Keystroke{}, fmt.Errorf("keybindings: empty key")
	}

	parts := strings.Split(strings.ToLower(key), "+")
	name := parts[len(parts)-1]
	var mod event.ModMask
	for _, part := range parts[:len(parts)-1] {
		switch part {
		case "shift":
			mod |= event.ModShift
		case "alt", "option", "meta":
			mod |= event.ModAlt
		case "ctrl", "control":
			mod |= event.ModCtrl
		default:
			return ui.Keystroke{}, fmt.Errorf("keybindings: unknown modifier %q in %q", part, orig)
		}
	}

	switch strings.ToLower(name) {
	case "escape", "esc":
		return ui.Keystroke{Key: event.KeyEsc, Mod: mod}, nil
	case "enter", "return":
		return ui.Keystroke{Key: event.KeyEnter, Mod: mod}, nil
	case "tab":
		if mod == event.ModShift {
			return ui.Keystroke{Key: event.KeyShiftTab}, nil
		}
		return ui.Keystroke{Key: event.KeyTab, Mod: mod}, nil
	case "backspace", "bs":
		return ui.Keystroke{Key: event.KeyBackspace, Mod: mod}, nil
	case "space":
		return ui.Keystroke{Key: event.KeyRune, Rune: ' ', Mod: mod}, nil
	case "up":
		return ui.Keystroke{Key: event.KeyUp, Mod: mod}, nil
	case "down":
		return ui.Keystroke{Key: event.KeyDown, Mod: mod}, nil
	case "left":
		return ui.Keystroke{Key: event.KeyLeft, Mod: mod}, nil
	case "right":
		return ui.Keystroke{Key: event.KeyRight, Mod: mod}, nil
	case "home":
		return ui.Keystroke{Key: event.KeyHome, Mod: mod}, nil
	case "end":
		return ui.Keystroke{Key: event.KeyEnd, Mod: mod}, nil
	case "insert":
		return ui.Keystroke{Key: event.KeyInsert, Mod: mod}, nil
	case "delete", "del":
		return ui.Keystroke{Key: event.KeyDelete, Mod: mod}, nil
	case "pageup":
		return ui.Keystroke{Key: event.KeyPageUp, Mod: mod}, nil
	case "pagedown":
		return ui.Keystroke{Key: event.KeyPageDown, Mod: mod}, nil
	}

	if len(name) >= 2 && name[0] == 'f' {
		fn := map[string]event.Key{
			"f1": event.KeyF1, "f2": event.KeyF2, "f3": event.KeyF3, "f4": event.KeyF4,
			"f5": event.KeyF5, "f6": event.KeyF6, "f7": event.KeyF7, "f8": event.KeyF8,
			"f9": event.KeyF9, "f10": event.KeyF10, "f11": event.KeyF11, "f12": event.KeyF12,
		}
		if k, ok := fn[name]; ok {
			return ui.Keystroke{Key: k, Mod: mod}, nil
		}
	}

	runes := []rune(name)
	if len(runes) == 1 {
		if mod == event.ModCtrl && runes[0] == 'c' {
			return ui.Keystroke{Key: event.KeyCtrlC}, nil
		}
		return ui.Keystroke{Key: event.KeyRune, Rune: runes[0], Mod: mod}, nil
	}

	return ui.Keystroke{}, fmt.Errorf("keybindings: unknown key %q", orig)
}

// canonicalKeystroke folds the terminal's alternate encodings of a key onto one
// representative, preserving any other modifiers.
//
// Terminals encode Ctrl+C as either a dedicated key or Ctrl + the 'c' rune, and
// Backspace as either a dedicated key or Ctrl+H. Those are two spellings of one
// physical keystroke, so they must compare equal — but only *as* that
// keystroke. Alt+Backspace is a different key from Backspace and must not fold
// onto it.
func canonicalKeystroke(k ui.Keystroke) ui.Keystroke {
	switch {
	case k.Key == event.KeyCtrlC:
		return ui.Keystroke{Key: event.KeyCtrlC, Mod: k.Mod &^ event.ModCtrl}
	case k.Key == event.KeyRune && k.Rune == 'c' && k.Mod&event.ModCtrl != 0:
		return ui.Keystroke{Key: event.KeyCtrlC, Mod: k.Mod &^ event.ModCtrl}
	case k.Key == event.KeyRune && k.Rune == 'h' && k.Mod&event.ModCtrl != 0:
		return ui.Keystroke{Key: event.KeyBackspace, Mod: k.Mod &^ event.ModCtrl}
	}
	return k
}

// matchKeystroke reports whether actual is the key binding describes.
//
// This used to special-case the alternate encodings with clauses that tested
// only Key and dropped Mod entirely, so a binding of "alt+backspace" matched a
// bare Backspace: the clause saw binding.Key == KeyBackspace and returned true
// for any actual whose Key was Backspace, modifiers be damned. That silently
// stole Backspace from the editor.
func matchKeystroke(actual, binding ui.Keystroke) bool {
	return canonicalKeystroke(actual) == canonicalKeystroke(binding)
}

// KeystrokesMatch reports whether actual is the key binding describes, folding
// the alternate control-code encodings the same way the registry does. Exported
// for callers that hold compiled keystrokes outside a Manager — extension
// shortcuts, which are matched against their own compiled keys.
func KeystrokesMatch(actual, binding ui.Keystroke) bool {
	return matchKeystroke(actual, binding)
}
