// Package layout re-exports the underlying tui library's layout package with
// tau-friendly aliases.
package layout

import (
	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/ui/layout"
)

// VStack is a vertical stack of children. Re-export of layout.VStack.
type VStack = layout.VStack

// HStack is a horizontal stack of children. Re-export of layout.HStack.
type HStack = layout.HStack

// Border wraps a child in a border.
type Border = layout.Border

// Padding insets a child by the given amounts.
type Padding = layout.Padding

// Split is a two-pane split (horizontal or vertical).
type Split = layout.Split

// Child pairs a view with a SizePolicy for layout hints.
type Child = layout.Child

// SizePolicy controls how a child is sized and aligned within a parent.
type SizePolicy = layout.SizePolicy

// Align is a cross-axis alignment.
type Align = layout.Align

// Align constants.
const (
	AlignStart   = layout.AlignStart
	AlignCenter  = layout.AlignCenter
	AlignEnd     = layout.AlignEnd
	AlignStretch = layout.AlignStretch
)

// Split orientation.
type Orientation = layout.Orientation

const (
	Horizontal = layout.Horizontal
	Vertical   = layout.Vertical
)

// Constructor re-exports.
var (
	NewVStack             = layout.NewVStack
	NewVStackWithChildren = layout.NewVStackWithChildren
	NewVStackWithGap      = layout.NewVStackWithGap
	NewHStack             = layout.NewHStack
	NewBorder             = layout.NewBorder
	NewPadding            = layout.NewPadding
	NewSplit              = layout.NewSplit
	NewChild              = layout.NewChild
)

// Sizing helpers.
var (
	GrowChild          = layout.GrowChild
	GrowXChild         = layout.GrowXChild
	GrowYChild         = layout.GrowYChild
	AlignChild         = layout.AlignChild
	InsetRect          = layout.InsetRect
	FindByID           = layout.FindByID
	FindFirstFocusable = layout.FindFirstFocusable
)

// View is re-exported so consumers don't need to import the underlying
// library directly.
type View = rovel.View
