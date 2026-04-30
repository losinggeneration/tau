package tui

import (
	"context"
	"testing"
	"time"
)

// The toolkit loop blocks until the user quits, so a cancelled parent context
// has to be translated into a quit — otherwise a SIGTERM or a shutting-down
// host leaves the TUI running with the terminal in raw mode.
func TestQuitOnContextCancelQuitsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	quit := make(chan struct{})

	stop := quitOnContextCancel(ctx, func() { close(quit) })
	defer stop()

	cancel()

	select {
	case <-quit:
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not reach the quit callback")
	}
}

// The ordinary exit path (the user quit) must not fire the callback, and the
// watcher must not outlive the run.
func TestQuitOnContextCancelStopsWithoutQuitting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var quits int
	done := make(chan struct{})
	stop := quitOnContextCancel(ctx, func() {
		quits++
		close(done)
	})

	stop()
	// Cancelling after the watcher stopped must not call quit.
	cancel()

	select {
	case <-done:
		t.Fatal("quit fired after the watcher was stopped")
	case <-time.After(100 * time.Millisecond):
	}
	if quits != 0 {
		t.Fatalf("quit called %d times on the ordinary exit path, want 0", quits)
	}
}

// Run defers stop, and a panic path could call it again; it must be idempotent.
func TestQuitOnContextCancelStopIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := quitOnContextCancel(ctx, func() {})
	stop()
	stop()
}

func TestQuitOnContextCancelHandlesNilArguments(t *testing.T) {
	quitOnContextCancel(nil, func() {})()
	quitOnContextCancel(context.Background(), nil)()
}
