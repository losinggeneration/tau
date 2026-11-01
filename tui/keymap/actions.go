package keymap

import (
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
	"github.com/losinggeneration/tau/tui/editor"
	"github.com/losinggeneration/tau/tui/keybindings"
)

const (
	ActionDeleteToLineEnd    = editor.ActionDeleteToLineEnd
	ActionDeleteToLineStart  = editor.ActionDeleteToLineStart
	ActionDeleteWordBackward = editor.ActionDeleteWordBackward
	ActionDeleteWordForward  = editor.ActionDeleteWordForward
	ActionUndo               = editor.ActionUndo
	ActionYank               = editor.ActionYank
	ActionYankPop            = editor.ActionYankPop
	ActionMoveWordLeft       = editor.ActionMoveWordLeft
	ActionMoveWordRight      = editor.ActionMoveWordRight
	ActionHistoryPrevious    = editor.ActionHistoryPrevious
	ActionHistoryNext        = editor.ActionHistoryNext
)

// DefaultBindings returns the standard editor/global key bindings.
func DefaultBindings() map[ui.Action][]Binding {
	bindings := map[ui.Action][]Binding{
		ui.ActionFocusPrev: {B(KeyCtxGlobal, event.KeyShiftTab, 0, 0), B(KeyCtxTextInput, event.KeyShiftTab, 0, 0)},
		ui.ActionCancel:    {B(KeyCtxGlobal, event.KeyEsc, 0, 0), B(KeyCtxTextInput, event.KeyEsc, 0, 0), B(KeyCtxGlobal, event.KeyCtrlC, 0, 0)},
		ui.ActionActivate:  {B(KeyCtxGlobal, event.KeyEnter, 0, 0), B(KeyCtxGlobal, event.KeyRune, ' ', 0)},
		ui.ActionCopy:      {B(KeyCtxTextInput, event.KeyCtrlC, 0, 0), B(KeyCtxTextInput, event.KeyRune, 'c', event.ModCtrl)},
		ui.ActionCut:       {B(KeyCtxTextInput, event.KeyRune, 'x', event.ModCtrl)},
		ui.ActionSelectAll: {B(KeyCtxTextInput, event.KeyRune, 'a', event.ModCtrl)},
		ui.ActionPaste:     {B(KeyCtxTextInput, event.KeyRune, 'v', event.ModCtrl)},
	}
	kb := keybindings.DefaultManager()
	for action, id := range map[ui.Action]string{
		ui.ActionFocusNext:       "tui.input.tab",
		ActionHistoryPrevious:    "tui.editor.historyPrevious",
		ActionHistoryNext:        "tui.editor.historyNext",
		ui.ActionMoveLeft:        "tui.editor.cursorLeft",
		ui.ActionMoveRight:       "tui.editor.cursorRight",
		ui.ActionMoveUp:          "tui.editor.cursorUp",
		ui.ActionMoveDown:        "tui.editor.cursorDown",
		ui.ActionPageUp:          "tui.editor.pageUp",
		ui.ActionPageDown:        "tui.editor.pageDown",
		ui.ActionHome:            "tui.editor.cursorLineStart",
		ui.ActionEnd:             "tui.editor.cursorLineEnd",
		ui.ActionDeleteBackward:  "tui.editor.deleteCharBackward",
		ui.ActionDeleteForward:   "tui.editor.deleteCharForward",
		ActionDeleteToLineEnd:    "tui.editor.deleteToLineEnd",
		ActionDeleteToLineStart:  "tui.editor.deleteToLineStart",
		ActionDeleteWordBackward: "tui.editor.deleteWordBackward",
		ActionDeleteWordForward:  "tui.editor.deleteWordForward",
		ActionUndo:               "tui.editor.undo",
		ActionYank:               "tui.editor.yank",
		ActionYankPop:            "tui.editor.yankPop",
		ActionMoveWordLeft:       "tui.editor.cursorWordLeft",
		ActionMoveWordRight:      "tui.editor.cursorWordRight",
	} {
		for _, key := range kb.Bindings(id) {
			for _, ctx := range []KeyContext{KeyCtxGlobal, KeyCtxTextInput} {
				bindings[action] = append(bindings[action], Binding{Context: ctx, Key: key.Keystroke})
			}
		}
	}
	return bindings
}

// ReservedActions is the core editor/global set protected from extension
// shortcut shadowing at the tui layer.
func ReservedActions() map[ui.Action]bool {
	return map[ui.Action]bool{
		ui.ActionCancel:        true,
		ui.ActionSubmit:        true,
		ui.ActionCopy:          true,
		ui.ActionDeleteForward: true,
		ActionDeleteToLineEnd:  true,
		ui.ActionFocusNext:     true,
		ui.ActionFocusPrev:     true,
	}
}
