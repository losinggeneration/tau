package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSaveScopedModelsPreservesEmptyAndUnrelatedSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"theme":"tau","enabledModels":["p/m"],"extension":{"future":true}}`), 0o600))
	app := &App{settingsPath: path, scopedModelKeys: []string{}}
	require.NoError(t, app.saveScopedModels())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"theme":"tau","enabledModels":[],"extension":{"future":true}}`, string(data))
	app.scopedModelKeys = nil
	require.NoError(t, app.saveScopedModels())
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"theme":"tau","enabledModels":null,"extension":{"future":true}}`, string(data))
}
