package tui

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	tauui "github.com/losinggeneration/tau/tui"
)

func (a *App) refreshThemeFile() {
	if a.refreshTheme == nil || a.terminalTheme.inMemory {
		return
	}
	name := a.terminalTheme.active
	if name == systemThemeName {
		return
	}
	changed, err := a.refreshTheme(name)
	if err != nil {
		if message := err.Error(); message != a.terminalTheme.fileError {
			a.terminalTheme.fileError = message
			a.conv.AddError("theme reload: " + message)
			a.refreshChat()
		}
		return
	}
	a.terminalTheme.fileError = ""
	if changed {
		a.reapplyTerminalTheme()
	}
}

func (a *App) startThemeFileWatcher(ctx context.Context) func() {
	if a.refreshTheme == nil {
		return func() {}
	}
	// ponytail: poll one selected file at 100 ms; use directory notifications if file IO becomes measurable.
	ticker := time.NewTicker(100 * time.Millisecond)
	stop := a.watchThemeFiles(ctx, ticker.C, a.app.Post)
	return func() { ticker.Stop(); stop() }
}

func (a *App) watchThemeFiles(ctx context.Context, ticks <-chan time.Time, post func(func(*tauui.UpdateCtx)) error) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var pending atomic.Bool
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ticks:
				if !ok {
					return
				}
				if !pending.CompareAndSwap(false, true) {
					continue
				}
				if err := post(func(*tauui.UpdateCtx) {
					defer pending.Store(false)
					if ctx.Err() == nil {
						a.refreshThemeFile()
					}
				}); err != nil {
					return
				}
			}
		}
	}()
	return sync.OnceFunc(func() { cancel(); <-done })
}
