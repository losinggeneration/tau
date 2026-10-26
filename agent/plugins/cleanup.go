package plugins

import (
	"context"
	"errors"
	"fmt"
)

// runCleanup invokes a go-plugin cleanup/kill hook and waits until it returns
// or the manager's cleanup grace elapses. Caller cancellation does not shorten
// the wait, so a cancelled shutdown still reaps the plugin process. A hook that
// outlives the wait keeps running and is joined by Shutdown. Cleanup hooks must be idempotent;
// go-plugin Client.Kill is safe to call multiple times.
func (m *Manager) runCleanup(ctx context.Context, name string, cleanup func()) error {
	if cleanup == nil {
		return nil
	}
	done := make(chan error, 1)
	m.cleanups.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				err := fmt.Errorf("cleanup plugin %s: panic: %v", name, r)
				m.cleanupMu.Lock()
				m.cleanupErr = errors.Join(m.cleanupErr, err)
				m.cleanupMu.Unlock()
				done <- err
			}
		}()
		cleanup()
		done <- nil
	})
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.cleanupGrace)
	defer cancel()
	select {
	case err := <-done:
		return err
	case <-waitCtx.Done():
		return fmt.Errorf("cleanup plugin %s: %w", name, waitCtx.Err())
	}
}

// waitCleanups joins cleanup hooks that outlived their callers' waits.
func (m *Manager) waitCleanups(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		m.cleanups.Wait()
		close(done)
	}()
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.cleanupGrace)
	defer cancel()
	select {
	case <-done:
		m.cleanupMu.Lock()
		defer m.cleanupMu.Unlock()
		return m.cleanupErr
	case <-waitCtx.Done():
		return errors.Join(errors.New("plugin cleanup still running"), waitCtx.Err())
	}
}
