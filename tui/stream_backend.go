package tui

import (
	"io"
	"sync"

	"github.com/losinggeneration/rovel/backend"
	rovelansi "github.com/losinggeneration/rovel/backend/ansi"
	"github.com/losinggeneration/rovel/backend/headless"
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/geom"
)

// streamBackend is a lightweight non-terminal backend used when callers inject
// Stdin/Stdout without providing a full backend. It is intentionally small: it
// makes stream injection testable without pretending to be a complete process
// terminal.
type streamBackend struct {
	inner *headless.Backend
	in    io.Reader

	once sync.Once
}

var (
	_ backend.Backend             = (*streamBackend)(nil)
	_ backend.ANSITransport       = (*streamBackend)(nil)
	_ backend.CapabilityReporter  = (*streamBackend)(nil)
	_ backend.InputFeatureEnabler = (*streamBackend)(nil)
)

func newStreamBackend(in io.Reader, size geom.Size) *streamBackend {
	return &streamBackend{inner: headless.New(size), in: in}
}

func (b *streamBackend) Enable() (geom.Size, error) {
	size, err := b.inner.Enable()
	if err != nil {
		return geom.Size{}, err
	}

	if b.in != nil {
		b.once.Do(func() {
			go b.readInput()
		})
	}

	return size, nil
}

func (b *streamBackend) Restore() error              { return b.inner.Restore() }
func (b *streamBackend) ReadEvent() Event            { return b.inner.ReadEvent() }
func (b *streamBackend) Size() Size                  { return b.inner.Size() }
func (b *streamBackend) Write(p []byte) (int, error) { return b.inner.Write(p) }
func (b *streamBackend) Flush() error                { return b.inner.Flush() }
func (b *streamBackend) InputCapabilities() backend.InputCapabilities {
	return b.inner.InputCapabilities()
}

func (b *streamBackend) SetInputFeatures(f backend.InputFeatures) error {
	return b.inner.SetInputFeatures(f)
}

func (b *streamBackend) readInput() {
	var decoder rovelansi.DecodeInput
	chunk := make([]byte, 256)
	for {
		n, err := b.in.Read(chunk)
		if n > 0 {
			b.sendEvents(decoder.Push(nil, chunk[:n]))
		}
		if err != nil {
			if err != io.EOF {
				b.inner.SendEvent(event.KeyEvent{Key: event.KeyNone})
			}
			b.sendEvents(decoder.Finalize(nil))
			b.inner.Close()

			return
		}
	}
}

func (b *streamBackend) sendEvents(events []event.Event) {
	for _, ev := range events {
		b.inner.SendEvent(ev)
	}
}
