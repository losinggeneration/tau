package tui

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/losinggeneration/rovel/backend"
)

// ErrUnsupported is returned when a control surface is not supported by the
// configured backend.
var ErrUnsupported = errors.New("tui: unsupported operation")

// InputListener observes raw input before normal dispatch. Returning consumed
// true stops dispatch. Returning a non-nil Event rewrites the event for later
// listeners and dispatch.
type InputListener func(Event) (rewritten Event, consumed bool)

type inputListenerEntry struct {
	id int
	fn InputListener
}

// AddInputListener registers a pre-dispatch input listener. The returned
// unsubscribe function is safe to call multiple times.
func (a *App) AddInputListener(fn InputListener) func() {
	if fn == nil {
		return func() {}
	}

	a.mu.Lock()
	a.nextInputListenerID++
	id := a.nextInputListenerID
	a.inputListeners = append(a.inputListeners, inputListenerEntry{id: id, fn: fn})
	a.mu.Unlock()

	var unsubscribed bool
	var unsubscribeMu sync.Mutex

	return func() {
		unsubscribeMu.Lock()
		defer unsubscribeMu.Unlock()
		if unsubscribed {
			return
		}
		unsubscribed = true

		a.mu.Lock()
		defer a.mu.Unlock()
		for i, entry := range a.inputListeners {
			if entry.id == id {
				a.inputListeners = append(a.inputListeners[:i], a.inputListeners[i+1:]...)
				return
			}
		}
	}
}

func (a *App) filterInputEvent(e Event) (Event, bool) {
	a.mu.Lock()
	listeners := make([]inputListenerEntry, len(a.inputListeners))
	copy(listeners, a.inputListeners)
	a.mu.Unlock()

	cur := e
	for _, listener := range listeners {
		next, consumed := listener.fn(cur)
		if consumed {
			return nil, true
		}
		if next != nil {
			cur = next
		}
	}

	return cur, false
}

type listenerBackend struct {
	backend.Backend
	app *App
}

func (b *listenerBackend) Restore() error {
	var err error
	if enabler, ok := b.Backend.(backend.InputFeatureEnabler); ok {
		err = enabler.SetInputFeatures(backend.InputFeatures{})
	}

	return errors.Join(err, b.Backend.Restore())
}

func (b *listenerBackend) ReadEvent() Event {
	for {
		e := b.Backend.ReadEvent()
		if e == nil {
			return nil
		}

		// Resize events are terminal lifecycle events, not raw input. They must
		// always reach the app loop so width/height changes force relayout and a
		// full redraw even when a raw input listener consumes keyboard input.
		if _, ok := e.(ResizeEvent); ok {
			return e
		}

		filtered, consumed := b.app.filterInputEvent(e)
		if consumed {
			continue
		}
		if filtered == nil {
			continue
		}

		return filtered
	}
}

func (b *listenerBackend) Write(p []byte) (int, error) {
	if transport, ok := b.Backend.(backend.ANSITransport); ok {
		return transport.Write(p)
	}

	return len(p), nil
}

func (b *listenerBackend) Flush() error {
	if transport, ok := b.Backend.(backend.ANSITransport); ok {
		return transport.Flush()
	}

	return nil
}

func (b *listenerBackend) InputCapabilities() backend.InputCapabilities {
	if reporter, ok := b.Backend.(backend.CapabilityReporter); ok {
		return reporter.InputCapabilities()
	}

	return backend.InputCapabilities{}
}

func (b *listenerBackend) SetInputFeatures(f backend.InputFeatures) error {
	if enabler, ok := b.Backend.(backend.InputFeatureEnabler); ok {
		if err := enabler.SetInputFeatures(f); err != nil {
			return err
		}
	}

	return nil
}

func (b *listenerBackend) DrainInput(max time.Duration, idle time.Duration) error {
	if drainer, ok := b.Backend.(inputDrainer); ok {
		return drainer.DrainInput(max, idle)
	}

	return ErrUnsupported
}

// SetTitle updates the terminal window title at runtime when the backend
// exposes an ANSI transport. Backends without title support return
// ErrUnsupported. The configured lifecycle title in Opts.Title is still applied
// and cleared by Run.
func (a *App) SetTitle(title string) error {
	if a == nil || a.backend == nil {
		return ErrUnsupported
	}
	return writeTerminalTitle(a.backend, title)
}

// FocusedID returns the last focus id requested through this wrapper. It is a
// wrapper-level mirror of focus control, intended for app-layer coordination.
func (a *App) FocusedID() ID {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.focusedID
}

// IsFocused reports whether id is the wrapper's current focused id.
func (a *App) IsFocused(id ID) bool {
	return id != 0 && a.FocusedID() == id
}

type inputDrainer interface {
	DrainInput(max time.Duration, idle time.Duration) error
}

// DrainInput asks the backend to drain late input when supported.
func (a *App) DrainInput(max time.Duration, idle time.Duration) error {
	if a.backend == nil {
		return ErrUnsupported
	}

	drainer, ok := a.backend.(inputDrainer)
	if !ok {
		return ErrUnsupported
	}

	if err := drainer.DrainInput(max, idle); err != nil {
		return err
	}

	return nil
}

func writeTerminalTitle(target any, title string) error {
	transport, ok := target.(backend.ANSITransport)
	if !ok {
		return ErrUnsupported
	}

	escaped := sanitizeTerminalTitle(title)
	if err := writeTerminalTitleToWriter(transport, escaped); err != nil {
		return err
	}

	if err := transport.Flush(); err != nil {
		return fmt.Errorf("flush terminal title: %w", err)
	}

	return nil
}

func writeTerminalTitleToWriter(w io.Writer, title string) error {
	if w == nil {
		return ErrUnsupported
	}

	if _, err := fmt.Fprintf(w, "\x1b]0;%s\x07", title); err != nil {
		return fmt.Errorf("write terminal title: %w", err)
	}

	return nil
}

func sanitizeTerminalTitle(title string) string {
	return strings.NewReplacer("\x1b", "", "\x07", "", "\x9c", "").Replace(title)
}
