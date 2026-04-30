// Package layout provides layout containers and helpers for the tau TUI.
//
// The package is a thin re-export of github.com/losinggeneration/rovel/ui/layout,
// presenting the same names with stable types so consumers of
// github.com/losinggeneration/tau/tui/layout don't import the underlying
// library directly.
//
// # Common containers
//
//   - VStack: vertical stack of children
//   - HStack: horizontal stack of children
//   - Border: draws a border around a child
//   - Padding: insets a child on all four sides
//   - Split:  two-pane split (horizontal or vertical)
//
// # Sizing helpers
//
//   - GrowChild(view, sx, sy)  — child that grows in both directions
//   - GrowXChild(view, n)      — child that grows horizontally
//   - GrowYChild(view, n)      — child that grows vertically
//   - AlignChild(view, x, y)   — child with cross-axis alignment
//
// Container layout primitives (general container semantics).
package layout // import "github.com/losinggeneration/tau/tui/layout"
