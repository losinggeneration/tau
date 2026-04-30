// Package widgets provides interactive widgets for the tau TUI.
//
// Like tui/layout, this package is a thin re-export of the underlying
// library's widget set so consumers of github.com/losinggeneration/tau/tui
// don't import the underlying library directly.
//
// # Available widgets
//
//   - Label:      static text
//   - Button:     pressable button
//   - TextInput:  single-line input
//   - TextArea:   multi-line editor
//   - Checkbox:   boolean toggle
//   - RadioGroup: single-selection from a group
//   - Select:     option list
//   - Tabs:       tabbed layout
//   - ScrollView: scrollable view
//   - ProgressBar: progress indicator
//   - Dialog:     modal dialog
//
// For cell-precise painting (used by applications that need exact control),
// see cell.Canvas.
//
// Re-exports of the tui library widget components.
package widgets // import "github.com/losinggeneration/tau/tui/widgets"
