package plugins

import (
	"runtime/debug"
	"time"
)

const cleanupReturnGrace = 10 * time.Millisecond

// pluginCleanup invokes go-plugin cleanup/kill hooks without letting a
// wedged subprocess block tau's foreground CLI/TUI path. Cleanup hooks must be
// idempotent; go-plugin Client.Kill is safe to call multiple times.
//
// Most cleanup hooks return immediately. Waiting briefly for those fast paths
// makes lifecycle operations deterministic for callers while still bounding the
// time spent on a wedged cleanup hook.
func pluginCleanup(cleanup func()) {
	if cleanup == nil {
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recover() != nil {
				_ = debug.Stack()
			}
		}()
		cleanup()
	}()

	select {
	case <-done:
	case <-time.After(cleanupReturnGrace):
	}
}
