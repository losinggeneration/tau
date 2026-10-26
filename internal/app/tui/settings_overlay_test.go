package tui

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsPersistenceMergesConcurrentChoicesAndPreservesUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"models":{"p/m":{"future":9007199254740993}},"terminal":{"future":true},"extension":false}`), 0o600))
	app := &App{settingsPath: path, ctx: t.Context()}
	var workers sync.WaitGroup
	for _, save := range []func() error{
		func() error { return app.persistThemeSetting("tau") },
		func() error { return app.persistLayoutSetting(LayoutMode("compact")) },
		func() error { return app.persistModelThinkingSetting("p/m", "high") },
	} {
		workers.Go(func() {
			if err := save(); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"models":{"p/m":{"thinkingLevel":"high","future":9007199254740993}},"terminal":{"future":true},"extension":false,"theme":"tau","layoutMode":"compact"}`, string(data))
	assert.Contains(t, string(data), "9007199254740993")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	app.ctx = ctx
	require.ErrorIs(t, app.persistThemeSetting("different"), context.Canceled)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(data), string(after))
}
