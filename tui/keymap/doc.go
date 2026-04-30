// Package keymap provides tau-specific keybinding helpers built on top of the
// tui library's keymap system.
//
// The package re-exports the standard rovel.KeyContext values (global,
// text-input, overlay) and provides tau-specific keymaps:
//
//   - QuitKeymap:  resolves Esc and Ctrl+C to ActionQuit (the default)
//   - EditorKeymap: standard readline-style editor bindings
//
// Keymap re-exports and helpers.
//
// Action groups include:
//   - editor cursor movement
//   - word/line movement
//   - page movement
//   - delete backward/forward
//   - delete word backward/forward
//   - delete to line start/end
//   - yank/yank-pop
//   - undo
//   - input newline/submit/tab/copy
//   - selection up/down/page/confirm/cancel
//   - generic editor, input, and select-list actions
package keymap // import "github.com/losinggeneration/tau/tui/keymap"
