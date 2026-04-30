package keymap

import (
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
)

const (
	ActionDeleteToLineEnd ui.Action = 2000 + iota
	ActionDeleteToLineStart
	ActionDeleteWordBackward
	ActionDeleteWordForward
	ActionUndo
	ActionYank
	ActionYankPop
	ActionMoveWordLeft
	ActionMoveWordRight
)

// DefaultBindings returns the standard editor/global key bindings.
func DefaultBindings() map[ui.Action][]Binding {
	return map[ui.Action][]Binding{
		ui.ActionFocusNext:       {B(KeyCtxGlobal, event.KeyTab, 0, 0), B(KeyCtxTextInput, event.KeyTab, 0, 0)},
		ui.ActionFocusPrev:       {B(KeyCtxGlobal, event.KeyShiftTab, 0, 0), B(KeyCtxTextInput, event.KeyShiftTab, 0, 0)},
		ui.ActionCancel:          {B(KeyCtxGlobal, event.KeyEsc, 0, 0), B(KeyCtxTextInput, event.KeyEsc, 0, 0), B(KeyCtxGlobal, event.KeyCtrlC, 0, 0)},
		ui.ActionSubmit:          {B(KeyCtxTextInput, event.KeyEnter, 0, 0)},
		ui.ActionActivate:        {B(KeyCtxGlobal, event.KeyEnter, 0, 0), B(KeyCtxGlobal, event.KeyRune, ' ', 0)},
		ui.ActionMoveLeft:        {B(KeyCtxGlobal, event.KeyLeft, 0, 0), B(KeyCtxTextInput, event.KeyLeft, 0, 0), B(KeyCtxGlobal, event.KeyRune, 'b', event.ModCtrl), B(KeyCtxTextInput, event.KeyRune, 'b', event.ModCtrl)},
		ui.ActionMoveRight:       {B(KeyCtxGlobal, event.KeyRight, 0, 0), B(KeyCtxTextInput, event.KeyRight, 0, 0), B(KeyCtxGlobal, event.KeyRune, 'f', event.ModCtrl), B(KeyCtxTextInput, event.KeyRune, 'f', event.ModCtrl)},
		ui.ActionMoveUp:          {B(KeyCtxGlobal, event.KeyUp, 0, 0), B(KeyCtxTextInput, event.KeyUp, 0, 0)},
		ui.ActionMoveDown:        {B(KeyCtxGlobal, event.KeyDown, 0, 0), B(KeyCtxTextInput, event.KeyDown, 0, 0)},
		ui.ActionPageUp:          {B(KeyCtxGlobal, event.KeyPageUp, 0, 0), B(KeyCtxTextInput, event.KeyPageUp, 0, 0)},
		ui.ActionPageDown:        {B(KeyCtxGlobal, event.KeyPageDown, 0, 0), B(KeyCtxTextInput, event.KeyPageDown, 0, 0)},
		ui.ActionHome:            {B(KeyCtxGlobal, event.KeyHome, 0, 0), B(KeyCtxTextInput, event.KeyHome, 0, 0), B(KeyCtxGlobal, event.KeyRune, 'a', event.ModCtrl), B(KeyCtxTextInput, event.KeyRune, 'a', event.ModCtrl)},
		ui.ActionEnd:             {B(KeyCtxGlobal, event.KeyEnd, 0, 0), B(KeyCtxTextInput, event.KeyEnd, 0, 0), B(KeyCtxGlobal, event.KeyRune, 'e', event.ModCtrl), B(KeyCtxTextInput, event.KeyRune, 'e', event.ModCtrl)},
		ui.ActionDeleteBackward:  {B(KeyCtxGlobal, event.KeyBackspace, 0, 0), B(KeyCtxTextInput, event.KeyBackspace, 0, 0)},
		ui.ActionDeleteForward:   {B(KeyCtxGlobal, event.KeyDelete, 0, 0), B(KeyCtxTextInput, event.KeyDelete, 0, 0), B(KeyCtxGlobal, event.KeyRune, 'd', event.ModCtrl), B(KeyCtxTextInput, event.KeyRune, 'd', event.ModCtrl)},
		ui.ActionCopy:            {B(KeyCtxTextInput, event.KeyCtrlC, 0, 0), B(KeyCtxTextInput, event.KeyRune, 'c', event.ModCtrl)},
		ui.ActionCut:             {B(KeyCtxTextInput, event.KeyRune, 'x', event.ModCtrl)},
		ui.ActionSelectAll:       {B(KeyCtxTextInput, event.KeyRune, 'a', event.ModCtrl)},
		ui.ActionPaste:           {B(KeyCtxTextInput, event.KeyRune, 'v', event.ModCtrl)},
		ActionDeleteToLineEnd:    {B(KeyCtxGlobal, event.KeyRune, 'k', event.ModCtrl), B(KeyCtxTextInput, event.KeyRune, 'k', event.ModCtrl)},
		ActionDeleteToLineStart:  {B(KeyCtxGlobal, event.KeyRune, 'u', event.ModCtrl), B(KeyCtxTextInput, event.KeyRune, 'u', event.ModCtrl)},
		ActionDeleteWordBackward: {B(KeyCtxGlobal, event.KeyRune, 'w', event.ModCtrl), B(KeyCtxTextInput, event.KeyRune, 'w', event.ModCtrl), B(KeyCtxGlobal, event.KeyBackspace, 0, event.ModAlt), B(KeyCtxTextInput, event.KeyBackspace, 0, event.ModAlt)},
		ActionDeleteWordForward:  {B(KeyCtxGlobal, event.KeyRune, 'd', event.ModAlt), B(KeyCtxTextInput, event.KeyRune, 'd', event.ModAlt), B(KeyCtxGlobal, event.KeyDelete, 0, event.ModAlt), B(KeyCtxTextInput, event.KeyDelete, 0, event.ModAlt)},
		ActionUndo:               {B(KeyCtxGlobal, event.KeyRune, '-', event.ModCtrl), B(KeyCtxTextInput, event.KeyRune, '-', event.ModCtrl)},
		ActionYank:               {B(KeyCtxGlobal, event.KeyRune, 'y', event.ModCtrl), B(KeyCtxTextInput, event.KeyRune, 'y', event.ModCtrl)},
		ActionYankPop:            {B(KeyCtxGlobal, event.KeyRune, 'y', event.ModAlt), B(KeyCtxTextInput, event.KeyRune, 'y', event.ModAlt)},
		ActionMoveWordLeft:       {B(KeyCtxGlobal, event.KeyLeft, 0, event.ModAlt), B(KeyCtxTextInput, event.KeyLeft, 0, event.ModAlt), B(KeyCtxGlobal, event.KeyLeft, 0, event.ModCtrl), B(KeyCtxTextInput, event.KeyLeft, 0, event.ModCtrl), B(KeyCtxGlobal, event.KeyRune, 'b', event.ModAlt), B(KeyCtxTextInput, event.KeyRune, 'b', event.ModAlt)},
		ActionMoveWordRight:      {B(KeyCtxGlobal, event.KeyRight, 0, event.ModAlt), B(KeyCtxTextInput, event.KeyRight, 0, event.ModAlt), B(KeyCtxGlobal, event.KeyRight, 0, event.ModCtrl), B(KeyCtxTextInput, event.KeyRight, 0, event.ModCtrl), B(KeyCtxGlobal, event.KeyRune, 'f', event.ModAlt), B(KeyCtxTextInput, event.KeyRune, 'f', event.ModAlt)},
	}
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
