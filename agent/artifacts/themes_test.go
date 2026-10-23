package artifacts

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThemeRegistryUsesValidatorBeforeRegistration(t *testing.T) {
	registry := NewThemeRegistry(nil)
	rejected := errors.New("invalid colors")
	registry.validate = func([]byte) error { return rejected }
	if err := registry.Register(Entry{Kind: KindTheme, Raw: []byte(`{"name":"invalid"}`)}); !errors.Is(err, rejected) {
		t.Fatalf("error=%v", err)
	}
	if registry.Len() != 1 {
		t.Fatal("invalid theme registered")
	}
}

func TestThemeRegistryReportsAndClearsLoadDiagnostics(t *testing.T) {
	registry := NewThemeRegistry(nil)
	rejected := errors.New("missing required colors")
	registry.validate = func(raw []byte) error {
		if string(raw) == `{"name":"invalid"}` {
			return rejected
		}
		return nil
	}
	require.NoError(t, registry.Load([]Entry{
		{Kind: KindTheme, SourcePath: "invalid.json", Raw: []byte(`{"name":"invalid"}`)},
		{Kind: KindTheme, SourcePath: "valid.json", Raw: []byte(`{"name":"valid"}`)},
	}))
	diagnostics := registry.Diagnostics()
	require.Len(t, diagnostics, 1)
	assert.Contains(t, diagnostics[0], "invalid.json")
	assert.Contains(t, diagnostics[0], rejected.Error())
	diagnostics[0] = "changed by caller"
	assert.NotEqual(t, diagnostics, registry.Diagnostics())
	require.Len(t, registry.List(), 2)
	require.NoError(t, registry.Reload(nil))
	assert.Empty(t, registry.Diagnostics())
	assert.Equal(t, "system", registry.List()[0].ID)
	assert.Equal(t, 1, registry.Len())
}

func TestThemeRegistryIncludesReservedGeneratedSystemTheme(t *testing.T) {
	registry := NewThemeRegistry(nil)
	check := func() {
		t.Helper()
		theme, ok := registry.Get("system")
		require.True(t, ok, "generated system theme must be discoverable without a file")
		assert.Equal(t, TierBuiltin, theme.Tier)
		assert.Empty(t, theme.SourcePath)
		assert.Empty(t, theme.Raw)
		themes := registry.List()
		require.NotEmpty(t, themes)
		assert.Equal(t, "system", themes[0].ID)
		count := 0
		for _, entry := range themes {
			if entry.ID == "system" {
				count++
			}
		}
		assert.Equal(t, 1, count)
		assert.Equal(t, len(themes), registry.Len())
		assert.Contains(t, registry.ListByTier(TierBuiltin), theme)
		assert.NotContains(t, registry.ListByTier(TierUser), theme)
		assert.Equal(t, []*Theme{theme}, registry.Matches("system"))
		byPrefix, ok := registry.GetByID("sys")
		require.True(t, ok)
		assert.Equal(t, theme, byPrefix)
	}
	check()
	entries := []Entry{
		{Kind: KindTheme, Tier: TierUser, SourcePath: "system.json", Raw: []byte(`{"name":"system"}`)},
		{Kind: KindTheme, Tier: TierUser, SourcePath: "zebra.json", Raw: []byte(`{"name":"zebra"}`)},
		{Kind: KindTheme, Tier: TierUser, SourcePath: "alpha.json", Raw: []byte(`{"name":"alpha"}`)},
	}
	require.NoError(t, registry.Load(entries))
	check()
	assert.Equal(t, []string{"system", "alpha", "zebra"}, []string{registry.List()[0].ID, registry.List()[1].ID, registry.List()[2].ID})
	require.NoError(t, registry.Reload(entries))
	check()
	require.NoError(t, registry.Reload(nil))
	check()
	assert.Equal(t, 1, registry.Len())
}

func TestThemeRegistryRefreshPreservesLastValidTheme(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.json")
	raw := []byte(`{"name":"custom","colors":{"text":"#123456"}}`)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	r := NewThemeRegistry(nil)
	r.validate = func(raw []byte) error { var f map[string]any; return json.Unmarshal(raw, &f) }
	require.NoError(t, r.Register(Entry{Kind: KindTheme, Tier: TierProject, SourcePath: path, Raw: raw}))
	changed, err := r.Refresh("custom")
	require.NoError(t, err)
	assert.False(t, changed)
	replacement := filepath.Join(filepath.Dir(path), "replacement.json")
	updated := []byte(`{"name":"custom","colors":{"text":"#abcdef"}}`)
	require.NoError(t, os.WriteFile(replacement, updated, 0o600))
	require.NoError(t, os.Rename(replacement, path))
	changed, err = r.Refresh("custom")
	require.NoError(t, err)
	assert.True(t, changed)
	theme, ok := r.Get("custom")
	require.True(t, ok)
	assert.Equal(t, updated, theme.Raw)
	for _, mode := range []string{"invalid", "missing"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "invalid" {
				require.NoError(t, os.WriteFile(path, []byte(`{`), 0o600))
			} else {
				require.NoError(t, os.Remove(path))
			}
			changed, err = r.Refresh("custom")
			require.Error(t, err)
			assert.False(t, changed)
			retained, ok := r.Get("custom")
			require.True(t, ok)
			assert.Same(t, theme, retained)
		})
	}
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	changed, err = r.Refresh("custom")
	require.NoError(t, err)
	assert.True(t, changed)
	require.NoError(t, r.Load(BuiltinThemes()))
	for _, name := range []string{"system", "dark", "light", "unknown"} {
		changed, err = r.Refresh(name)
		require.NoError(t, err)
		assert.False(t, changed)
	}
}
