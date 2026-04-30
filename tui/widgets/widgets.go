// Package widgets re-exports the underlying tui library's widget package.
package widgets

import (
	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/ui/widgets"
)

// Widget re-exports.
type (
	Label       = widgets.Label
	Button      = widgets.Button
	TextInput   = widgets.TextInput
	TextArea    = widgets.TextArea
	Checkbox    = widgets.Checkbox
	RadioGroup  = widgets.RadioGroup
	Select      = widgets.Select
	Tabs        = widgets.Tabs
	ScrollView  = widgets.ScrollView
	ProgressBar = widgets.ProgressBar
	Dialog      = widgets.Dialog
)

// Opts/Enum re-exports needed for construction.
type (
	ScrollViewOpts   = widgets.ScrollViewOpts
	ScrollbarMode    = widgets.ScrollbarMode
	CursorMoveEvent  = widgets.CursorMoveEvent
	CursorMoveReason = widgets.CursorMoveReason
)

const (
	ScrollbarAuto   = widgets.ScrollbarAuto
	ScrollbarAlways = widgets.ScrollbarAlways
	ScrollbarHidden = widgets.ScrollbarHidden
)

// Constructor re-exports.
var (
	NewLabel         = widgets.NewLabel
	NewButton        = widgets.NewButton
	NewTextInput     = widgets.NewTextInput
	NewTextInputOpts = widgets.NewTextInputOpts
	NewTextArea      = widgets.NewTextArea
	NewTextAreaOpts  = widgets.NewTextAreaOpts
	NewCheckbox      = widgets.NewCheckbox
	NewRadioGroup    = widgets.NewRadioGroup
	NewSelect        = widgets.NewSelect
	NewTabs          = widgets.NewTabs
	NewScrollView    = widgets.NewScrollView
	NewProgressBar   = widgets.NewProgressBar
	NewDialog        = widgets.NewDialog
)

// View is re-exported so consumers don't need to import the underlying
// library directly.
type View = rovel.View
