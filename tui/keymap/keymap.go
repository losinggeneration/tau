// Package keymap re-exports the underlying tui library's keymap types and
// adds tau-specific helpers.
package keymap

import (
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
	"github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/keybindings"
)

// KeyContext re-exports the library's KeyContext.
type KeyContext = ui.KeyContext

// Keystroke re-exports the library's Keystroke.
type Keystroke = ui.Keystroke

// Keymap re-exports the library's Keymap interface.
type Keymap = ui.Keymap

// DefaultKeymap re-exports the library's DefaultKeymap.
type DefaultKeymap = ui.DefaultKeymap

// CompositeKeymap re-exports the library's CompositeKeymap.
type CompositeKeymap = ui.CompositeKeymap

// NewResolver creates a ResolveAction closure that detects KeyContext from
// the focused view and resolves keystrokes via the given keymap.
var NewResolver = ui.NewResolver

// KeyContext constants.
const (
	KeyCtxGlobal    = ui.KeyCtxGlobal
	KeyCtxTextInput = ui.KeyCtxTextInput
	KeyCtxOverlay   = ui.KeyCtxOverlay
)

// EditorKeymap provides readline-style editor bindings suitable for the
// multiline editor component.
//
// Resolves:
//   - Arrow keys:        MoveLeft/Right/Up/Down
//   - Home/End:          Home/End
//   - PageUp/PageDown:   PageUp/PageDown
//   - Backspace/Delete:  DeleteBackward/DeleteForward
//   - Ctrl-D:            DeleteForward
//   - Ctrl-B:            MoveLeft
//   - Ctrl-F:            MoveRight
//   - Alt/Ctrl-Left:     MoveWordLeft
//   - Alt/Ctrl-Right:    MoveWordRight
//   - Alt-B:             MoveWordLeft
//   - Alt-F:             MoveWordRight
//   - Ctrl-A:            MoveLineStart (Home)
//   - Ctrl-E:            MoveLineEnd (End)
//   - Ctrl-K:            DeleteToLineEnd
//   - Ctrl-U:            DeleteToLineStart
//   - Ctrl-W/Alt-BS:     DeleteWordBackward
//   - Alt-D/Alt-Delete:  DeleteWordForward
//   - Ctrl-Y:            Yank
//   - Alt-Y:             YankPop
//   - Ctrl--:            Undo (Ctrl+Z is reserved for app.suspend)
//   - Ctrl-C:            Cancel (treated as Copy in text input context)
//   - Tab/Shift-Tab:     FocusNext/Prev
type EditorKeymap struct{}

func (EditorKeymap) Resolve(ctx ui.KeyContext, k ui.Keystroke) (ui.Action, bool) {
	kb := keybindings.DefaultManager()
	switch {
	case kb.Matches(k, "tui.editor.historyPrevious"):
		return ActionHistoryPrevious, true
	case kb.Matches(k, "tui.editor.historyNext"):
		return ActionHistoryNext, true
	}
	if kb.Matches(k, "tui.input.newLine") || kb.Matches(k, "tui.input.submit") ||
		k.Key == event.KeyEnter && k.Mod == event.ModAlt && kb.Matches(ui.Keystroke{Key: event.KeyEnter, Mod: event.ModShift}, "tui.input.newLine") {
		return ui.ActionSubmit, true
	}

	return NewDefaultManager().Resolve(ctx, k)
}

// Resolver returns a ResolveAction closure using the EditorKeymap.
func (EditorKeymap) Resolver() func(event.KeyEvent, any) (int, bool) {
	// Imported via the tau/tui shim — see NewResolver above.
	// Returns a closure that the underlying library can call.
	// Implementation detail: this is a thin wrapper that converts the
	// library's Keystroke form back into our resolution path.
	km := EditorKeymap{}

	return func(e event.KeyEvent, _ any) (int, bool) {
		ks := ui.KeystrokeOf(e)
		// We don't know the context here; assume global. The framework
		// will reroute via its own focused-view detection.
		a, ok := km.Resolve(ui.KeyCtxGlobal, ks)
		if !ok {
			return 0, false
		}

		return int(a), true
	}
}

// KeystrokeOf re-exports ui.KeystrokeOf for convenience.
func KeystrokeOf(e event.KeyEvent) ui.Keystroke {
	return ui.KeystrokeOf(e)
}

// DefaultAction resolves the tui Action enum for tau's standard quit key.
// Returns tui.ActionQuit.
func DefaultAction() ui.Action {
	return ui.Action(tui.ActionQuit)
}
