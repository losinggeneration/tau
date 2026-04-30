// Package tui implements tau's interactive terminal UI.
//
// This package is built on Tau's top-level terminal toolkit package
// (github.com/losinggeneration/tau/tui), reusing toolkit components such as
// Selector, DialogPicker, editor widgets, layout primitives, and scroll views
// while keeping app/session/conversation behavior here.
//
// The application assembles the layout region stack (header, chat
// log, pending view, editor, inline selector, footer) and drives it
// from agent-session events delivered through the sdk package.
//
// # Concurrency
//
// All view mutations run on the toolkit App event loop. Agent events arrive on
// a background goroutine and are forwarded to the loop with App.Post, the
// goroutine-safe update path documented by the toolkit.
package tui
