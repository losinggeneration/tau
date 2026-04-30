// Package tui provides a terminal UI toolkit for tau.
//
// This package is a thin wrapper over the underlying tui library
// (github.com/losinggeneration/rovel), tailored to tau's needs. It re-exports
// the library's core types and provides tau-specific helpers and widgets.
//
// # Spec
//
// Behavior follows the tui library conventions; this package adapts them
// the contract; this package is the wrapper.
//
// # Subpackages
//
//   - tui/layout: layout containers (VStack, HStack, Border, Padding, Split)
//   - tui/widgets: interactive widgets (TextInput, TextArea, Select, etc.)
//   - tui/keymap: tau-specific keybinding helpers
//
// # Quick start
//
//	app, err := rovel.New(rovel.Opts{
//	    Root: rovel.NewContainer(myChild),
//	})
//	if err != nil {
//	    return err
//	}
//	defer app.Restore()
//
//	if err := app.Run(); err != nil {
//	    return err
//	}
//
// # Component model
//
// A Component is any view that implements the underlying rovel.View interface:
// ID(), MinSize(), Layout(rect), Rect(), Paint(drawer, ctx), Handle(event, ctx).
//
// Common operations are surfaced by this package:
//
//   - Container: ordered child list, vertical stack, add/remove/clear
//   - Text:      single-line text display
//
// # Concurrency
//
// All view mutations (Layout, Paint, Handle) must run on the app loop. Use
// app.Post(fn) to schedule a mutation from another goroutine.
//
// # Terminal mode
//
// New() uses raw mode by default, suitable for full-screen interactive TUIs.
// Use NewCBreak() for inline rendering (cbreak mode) when integrating with
// other output.
package tui // import "github.com/losinggeneration/tau/tui"
