package autocomplete

import (
	"context"
	"sync"
	"time"
)

// Debouncer serializes autocomplete requests, cancels older in-flight work, and
// ignores stale responses that arrive after a newer request was issued.
type Debouncer struct {
	provider *CombinedProvider
	delay    time.Duration

	requestMu sync.Mutex
	mu        sync.Mutex
	seq       int
	cancel    context.CancelFunc
	stale     int
}

// NewDebouncer creates a request debouncer for editor autocomplete.
func NewDebouncer(provider *CombinedProvider, delay time.Duration) *Debouncer {
	return &Debouncer{provider: provider, delay: delay}
}

// Request fetches suggestions for the current editor state. Any previous
// request is cancelled. If this request becomes stale before it completes, nil
// suggestions and nil error are returned.
func (d *Debouncer) Request(ctx context.Context, lines []string, cursorLine, cursorCol int, force bool) (*Suggestions, error) {
	if d == nil || d.provider == nil {
		return nil, nil
	}

	d.mu.Lock()
	if d.cancel != nil {
		d.cancel()
	}
	d.seq++
	seq := d.seq
	reqCtx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	d.mu.Unlock()
	defer cancel()

	if d.delay > 0 {
		timer := time.NewTimer(d.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-reqCtx.Done():
			return nil, ctx.Err()
		}
	}

	d.requestMu.Lock()
	defer d.requestMu.Unlock()
	var suggestions *Suggestions
	err := reqCtx.Err()
	if err == nil {
		suggestions, err = d.provider.GetSuggestions(reqCtx, lines, cursorLine, cursorCol, force)
	}
	if reqCtx.Err() != nil {
		suggestions = nil
		if err == nil {
			err = reqCtx.Err()
		}
	}

	d.mu.Lock()
	stale := seq != d.seq
	if stale {
		d.stale++
	}
	d.mu.Unlock()
	if stale {
		return nil, nil
	}

	return suggestions, err
}

// Cancel aborts the current request.
func (d *Debouncer) Cancel() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
}

// StaleResponses reports how many responses were ignored because newer input
// had already started another request.
func (d *Debouncer) StaleResponses() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stale
}
