package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/losinggeneration/tau/ai"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/extensions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThemeFileRefreshRepaintsAndProtectsExtensionOwnership(t *testing.T) {
	values := map[string]string{"text": "#123456", "thinkingMax": "#654321"}
	var reads int
	var failure error
	changed := false
	a, err := New(Options{Session: &settingsSession{fakeSession: &fakeSession{}, settings: &ai.Settings{Theme: "custom"}}, ThemeLookup: func(name string) (ThemeDefinition, bool) {
		return ThemeDefinition{Values: values, Appearance: "light"}, name == "custom"
	}, RefreshTheme: func(name string) (bool, error) { reads++; assert.Equal(t, "custom", name); return changed, failure }})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	var publications int
	a.extensions.OnThemeChanged(func(theme extensions.Theme) { publications++; a.applyThemeFromExtension(theme) })
	a.refreshThemeFile()
	assert.Equal(t, 1, reads)
	assert.Zero(t, publications)
	values = map[string]string{"text": "#abcdef", "thinkingMax": "#fedcba"}
	changed = true
	a.refreshThemeFile()
	assert.Equal(t, "#abcdef", a.extensions.Theme().Values["text"])
	assert.Equal(t, 1, publications)
	last := a.chat.ThemeTokens()
	failure = errors.New("incomplete theme")
	a.refreshThemeFile()
	assert.Equal(t, last, a.chat.ThemeTokens())
	assert.Equal(t, 1, publications)
	a.applyThemeFromExtension(extensions.Theme{Name: "memory", Values: map[string]string{"text": "#444444"}})
	before := reads
	a.refreshThemeFile()
	assert.Equal(t, before, reads)
	assert.Equal(t, "memory", a.footer.data.theme)
	failure = nil
	a.applyTheme("custom")
	a.refreshThemeFile()
	assert.Greater(t, reads, before)
}

func TestThemeFileWatcherCoalescesAndJoinsOnStop(t *testing.T) {
	a := newTestApp(t)
	var reads int
	a.terminalTheme.active = "custom"
	a.refreshTheme = func(string) (bool, error) { reads++; return false, nil }
	ticks := make(chan time.Time)
	posts := make(chan func(*tauui.UpdateCtx), 3)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stop := a.watchThemeFiles(ctx, ticks, func(fn func(*tauui.UpdateCtx)) error { posts <- fn; return nil })
	ticks <- time.Now()
	callback := <-posts
	ticks <- time.Now()
	// Processing a subsequent tick proves the preceding one was coalesced.
	ticks <- time.Now()
	assert.Empty(t, posts)
	callback(nil)
	assert.Equal(t, 1, reads)
	ticks <- time.Now()
	callback = <-posts
	cancel()
	stop()
	stop()
	callback(nil)
	assert.Equal(t, 1, reads, "queued callbacks do nothing after shutdown")
	assert.Empty(t, posts)
}

func TestThemeFileWatcherStopsAfterPostFailure(t *testing.T) {
	a := newTestApp(t)
	ticks := make(chan time.Time)
	failed := make(chan struct{})
	stop := a.watchThemeFiles(t.Context(), ticks, func(func(*tauui.UpdateCtx)) error { close(failed); return tauui.ErrClosed })
	ticks <- time.Now()
	<-failed
	stop()
}

func TestThemeFileRefreshUsesCurrentSelectionForQueuedTicks(t *testing.T) {
	a := newTestApp(t)
	var names []string
	a.refreshTheme = func(name string) (bool, error) { names = append(names, name); return false, nil }
	a.terminalTheme.active = "first"
	ticks := make(chan time.Time)
	posts := make(chan func(*tauui.UpdateCtx), 1)
	stop := a.watchThemeFiles(t.Context(), ticks, func(fn func(*tauui.UpdateCtx)) error { posts <- fn; return nil })
	defer stop()
	ticks <- time.Now()
	callback := <-posts
	a.applyTheme("dark")
	callback(nil)
	assert.Equal(t, []string{"dark"}, names)
}
