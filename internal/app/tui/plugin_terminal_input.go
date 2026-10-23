package tui

import (
	"strings"

	"github.com/losinggeneration/tau/agent/plugins"
	tauui "github.com/losinggeneration/tau/tui"
)

// WirePluginTerminalInput forwards raw TUI key events to plugin onTerminalInput
// subscribers before normal dispatch. If any plugin consumes the input, the key
// does not continue into tau's own keymap/editor path.
func (a *App) WirePluginTerminalInput(mgr *plugins.Manager) func() {
	if a == nil || a.app == nil || mgr == nil {
		return func() {}
	}
	return a.app.AddInputListener(func(e tauui.Event) (tauui.Event, bool) {
		ke, ok := e.(tauui.KeyEvent)
		if !ok {
			return e, false
		}
		consumed := mgr.DispatchTerminalInput(plugins.TerminalInput{
			Key:  terminalInputKey(ke),
			Text: terminalInputText(ke),
			Mod:  terminalInputMod(ke.Mod),
		})
		return e, consumed
	})
}

func terminalInputKey(ke tauui.KeyEvent) string {
	switch ke.Key {
	case tauui.KeyRune:
		return "rune"
	case tauui.KeyEnter:
		return "enter"
	case tauui.KeyEsc:
		return "escape"
	case tauui.KeyTab:
		return "tab"
	case tauui.KeyBackspace:
		return "backspace"
	case tauui.KeyCtrlC:
		return "ctrl+c"
	case tauui.KeyUp:
		return "up"
	case tauui.KeyDown:
		return "down"
	case tauui.KeyLeft:
		return "left"
	case tauui.KeyRight:
		return "right"
	case tauui.KeyHome:
		return "home"
	case tauui.KeyEnd:
		return "end"
	case tauui.KeyInsert:
		return "insert"
	case tauui.KeyDelete:
		return "delete"
	case tauui.KeyPageUp:
		return "pageup"
	case tauui.KeyPageDown:
		return "pagedown"
	case tauui.KeyShiftTab:
		return "shift+tab"
	case tauui.KeyF1:
		return "f1"
	case tauui.KeyF2:
		return "f2"
	case tauui.KeyF3:
		return "f3"
	case tauui.KeyF4:
		return "f4"
	case tauui.KeyF5:
		return "f5"
	case tauui.KeyF6:
		return "f6"
	case tauui.KeyF7:
		return "f7"
	case tauui.KeyF8:
		return "f8"
	case tauui.KeyF9:
		return "f9"
	case tauui.KeyF10:
		return "f10"
	case tauui.KeyF11:
		return "f11"
	case tauui.KeyF12:
		return "f12"
	default:
		return ""
	}
}

func terminalInputText(ke tauui.KeyEvent) string {
	if ke.Key == tauui.KeyRune && ke.Rune != 0 {
		if ke.Mod&tauui.ModShift != 0 && ke.ShiftedRune != 0 {
			return string(ke.ShiftedRune)
		}
		return string(ke.Rune)
	}
	return ""
}

func terminalInputMod(mod tauui.ModMask) string {
	var parts []string
	if mod&tauui.ModCtrl != 0 {
		parts = append(parts, "ctrl")
	}
	if mod&tauui.ModAlt != 0 {
		parts = append(parts, "alt")
	}
	if mod&tauui.ModShift != 0 {
		parts = append(parts, "shift")
	}
	return strings.Join(parts, "+")
}
