package tui

import (
	"github.com/losinggeneration/rovel/ui/layout"
)

// Layout aliases exported from the top-level tui package for simple app-shell
// composition. The dedicated tui/layout subpackage remains available for code
// that wants a layout-focused import, but top-level app components can use
// these aliases without importing the lower-level package and without creating
// package test cycles.
type VStack = layout.VStack

type LayoutChild = layout.Child

var (
	NewVStack        = layout.NewVStack
	NewLayoutChild   = layout.NewChild
	GrowYLayoutChild = layout.GrowYChild
)
