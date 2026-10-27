package ai

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestGetOrCreateDeviceIDRetainsSettingsAcrossConcurrentCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte("\xef\xbb\xbf"+`{"theme":"kept","plugins":{"extra":{"large":9007199254740993}}}`), 0o600))
	var wg sync.WaitGroup
	ids := make([]string, 12)
	errs := make([]error, len(ids))
	for i := range ids {
		wg.Go(func() { ids[i], errs[i] = GetOrCreateDeviceID(t.Context(), path) })
	}
	wg.Wait()
	for i, id := range ids {
		require.NoError(t, errs[i])
		require.Equal(t, ids[0], id)
		parsed, err := uuid.Parse(id)
		require.NoError(t, err)
		require.Equal(t, uuid.Version(4), parsed.Version())
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), "9007199254740993")
	require.Contains(t, string(data), `"theme": "kept"`)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestGetOrCreateDeviceIDFailuresDoNotChangeSettings(t *testing.T) {
	for _, data := range []string{`{`, `{"deviceId":"not-a-uuid"}`, `null`} {
		path := filepath.Join(t.TempDir(), "settings.json")
		require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
		id, err := GetOrCreateDeviceID(t.Context(), path)
		require.Error(t, err)
		require.Empty(t, id)
		retained, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, data, string(retained))
	}
	path := filepath.Join(t.TempDir(), "nested", "settings.json")
	cause := errors.New("cancelled identity")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	_, err := GetOrCreateDeviceID(ctx, path)
	require.ErrorIs(t, err, cause)
	require.NoFileExists(t, path)
	id, err := GetOrCreateDeviceID(t.Context(), path)
	require.NoError(t, err)
	require.NotEmpty(t, id)
}

func TestMergeSettingsDeviceIDIsGlobalOnly(t *testing.T) {
	global, err := ParseSettings([]byte(`{"deviceId":"global"}`))
	require.NoError(t, err)
	project, err := ParseSettings([]byte(`{"deviceId":"project"}`))
	require.NoError(t, err)
	merged := MergeSettings(global, project)
	require.Equal(t, "global", merged.DeviceID)
	merged = MergeSettings(nil, project)
	require.Empty(t, merged.DeviceID)
	data, err := merged.MarshalJSON()
	require.NoError(t, err)
	require.NotContains(t, string(data), "deviceId")
}

func TestGetOrCreateDeviceIDDoesNotRewriteExistingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := `{"deviceId":"8DFB4786-146F-4C98-9846-CF1B6DD94AAC", "theme":"kept"}`
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	id, err := GetOrCreateDeviceID(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, "8DFB4786-146F-4C98-9846-CF1B6DD94AAC", id)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, string(data))
}
