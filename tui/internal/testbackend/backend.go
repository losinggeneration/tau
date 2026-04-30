// Package testbackend wraps the headless backend from the underlying tui
// library so it can be used in tests of the tau/tui package.
package testbackend

import (
	"io"
	"sync"

	"github.com/losinggeneration/rovel/backend"
	headlesslib "github.com/losinggeneration/rovel/backend/headless"
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/geom"
)

// Backend is a hermetic test backend that implements the underlying library's
// backend.Backend interface, allowing it to be passed directly to the
// library's rovel.New.
//
// It captures writes to an io.Writer (typically a bytes.Buffer for tests).
type Backend struct {
	inner  *headlesslib.Backend
	stdout io.Writer

	mu      sync.Mutex
	enabled bool
}

// New returns a headless backend with the given initial size and a captured
// output writer. The returned backend is safe for concurrent use.
func New(size geom.Size, stdout io.Writer) *Backend {
	return &Backend{
		inner:  headlesslib.New(size),
		stdout: stdout,
	}
}

// Compile-time check that Backend implements the library's backend.Backend
// interface (subset of methods that we expose).
var _ backend.Backend = (*Backend)(nil)

// Enable records that the backend was enabled and delegates to the inner
// backend.
func (b *Backend) Enable() (geom.Size, error) {
	b.mu.Lock()
	b.enabled = true
	b.mu.Unlock()

	return b.inner.Enable()
}

// Restore delegates to the inner backend.
func (b *Backend) Restore() error {
	return b.inner.Restore()
}

// ReadEvent delegates to the inner backend.
func (b *Backend) ReadEvent() event.Event {
	return b.inner.ReadEvent()
}

// Size returns the configured terminal size.
func (b *Backend) Size() geom.Size {
	return b.inner.Size()
}

// Write writes to the captured stdout and also to the inner backend's output
// buffer (so Output() returns what was written).
func (b *Backend) Write(p []byte) (int, error) {
	_, _ = b.inner.Write(p)

	if b.stdout == nil {
		return len(p), nil
	}

	return b.stdout.Write(p)
}

// Flush is a no-op.
func (b *Backend) Flush() error { return nil }

// --- Helpers used by tests ---

// SendEvent injects an event into the backend's event queue.
func (b *Backend) SendEvent(ev event.Event) {
	b.inner.SendEvent(ev)
}

// SendResize injects a resize event.
func (b *Backend) SendResize(w, h int) {
	b.inner.SendResize(w, h)
}

// Close closes the event channel, signaling shutdown.
func (b *Backend) Close() {
	b.inner.Close()
}

// Output returns the captured output bytes from the inner backend.
func (b *Backend) Output() []byte {
	return b.inner.Output()
}

// Restored reports whether Restore was called.
func (b *Backend) Restored() bool {
	return b.inner.Restored()
}

// Enabled reports whether Enable was called.
func (b *Backend) Enabled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.enabled
}

// --- Capability / Input feature support (for the library's reporter/enabler) ---

// InputCapabilities reports the features the headless backend supports.
// Implements backend.CapabilityReporter.
func (b *Backend) InputCapabilities() backend.InputCapabilities {
	caps := b.inner.InputCapabilities()
	caps.ModifiedKeys = true

	return caps
}

// SetInputFeatures records the requested input features.
// Implements backend.InputFeatureEnabler.
func (b *Backend) SetInputFeatures(f backend.InputFeatures) error {
	return b.inner.SetInputFeatures(f)
}

// Features returns the most recently requested input features.
func (b *Backend) Features() backend.InputFeatures {
	return b.inner.Features()
}
