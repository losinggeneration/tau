// Package testbackend provides a hermetic test backend for the tui package.
//
// It wraps the underlying tui library's headless backend with a small
// adapter so rovel.New can be called from tests without touching a real
// terminal. Output is captured to an io.Writer.
package testbackend // import "github.com/losinggeneration/tau/tui/internal/testbackend"
