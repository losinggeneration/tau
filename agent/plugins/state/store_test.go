package state

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPluginStateRejectsEscapingIdentities(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../outside", "../../outside", "/absolute", `..\outside`} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			store, err := NewStore(filepath.Join(base, "plugin-state"), name)
			require.Error(t, err)
			require.Nil(t, store)
			require.Error(t, RemovePlugin(base, name))
		})
	}
}

func TestPluginStateRemovalCannotTraverseScopeSymlink(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	root := filepath.Join(base, "plugin-state")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "@scope")))
	destination := filepath.Join(outside, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(destination, "keep"), []byte("original"), 0o600))
	require.Error(t, RemovePlugin(base, "@scope/plugin"))
	require.FileExists(t, filepath.Join(destination, "keep"))
	store, err := NewStore(root, "@scope/plugin")
	require.Error(t, err)
	require.Nil(t, store)
}

func TestPluginStateAllowsScopedNamesAndMissingRemoval(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, RemovePlugin(base, "missing"))
	store, err := NewStore(filepath.Join(base, "plugin-state"), "@scope/plugin")
	require.NoError(t, err)
	require.NotNil(t, store)
	require.DirExists(t, filepath.Join(base, "plugin-state", "@scope", "plugin"))
	require.NoError(t, RemovePlugin(base, "@scope/plugin"))
	require.NoDirExists(t, filepath.Join(base, "plugin-state", "@scope", "plugin"))
}

func TestStateFilesCannotEscapePluginDirectory(t *testing.T) {
	for _, name := range []string{"kv.json", "log.jsonl"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			store, err := NewStore(base, "plugin")
			require.NoError(t, err)
			outside := filepath.Join(t.TempDir(), "secret")
			original := []byte("private data")
			require.NoError(t, os.WriteFile(outside, original, 0o600))
			require.NoError(t, os.Symlink(outside, filepath.Join(base, "plugin", name)))
			_, err = NewStore(base, "plugin")
			require.Error(t, err)
			if name == "kv.json" {
				require.NoError(t, store.Set("key", "value"))
			} else {
				require.NoError(t, store.Append("key", "value"))
			}
			data, err := os.ReadFile(outside)
			require.NoError(t, err)
			require.Equal(t, original, data)
			reopened, err := NewStore(base, "plugin")
			require.NoError(t, err)
			if name == "kv.json" {
				value, ok := reopened.Get("key")
				require.True(t, ok)
				require.Equal(t, "value", value)
			} else {
				require.Len(t, reopened.Log(), 1)
			}
		})
	}
}

func TestStateFailedMutationsRetainMemory(t *testing.T) {
	for _, operation := range []string{"set-new", "set-existing", "delete", "append", "clear-kv", "clear-log"} {
		t.Run(operation, func(t *testing.T) {
			store, err := NewStore(t.TempDir(), "plugin")
			require.NoError(t, err)
			require.NoError(t, store.Set("existing", "original"))
			require.NoError(t, store.Append("existing", "original"))
			blocked := store.kvPath()
			if operation == "append" || operation == "clear-log" {
				blocked = store.logPath()
			}
			require.NoError(t, os.Remove(blocked))
			require.NoError(t, os.Mkdir(blocked, 0o700))
			switch operation {
			case "set-new":
				err = store.Set("new", "changed")
			case "set-existing":
				err = store.Set("existing", "changed")
			case "delete":
				err = store.Delete("existing")
			case "append":
				err = store.Append("new", "changed")
			default:
				err = store.Clear()
			}
			require.Error(t, err)
			_, ok := store.Get("new")
			require.False(t, ok)
			value, ok := store.Get("existing")
			if operation == "clear-log" {
				require.False(t, ok)
			} else {
				require.True(t, ok)
				require.Equal(t, "original", value)
			}
			require.Len(t, store.Log(), 1)
		})
	}
}

func TestStateLoadsIndependentLogWithoutKV(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "plugin")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "log.jsonl"), []byte("{\"ID\":\"1\",\"Key\":\"key\",\"Value\":\"value\"}\n"), 0o600))
	store, err := NewStore(base, "plugin")
	require.NoError(t, err)
	require.Len(t, store.Log(), 1)
	require.Equal(t, "value", store.Log()[0].Value)
}

func TestStateRejectsNullKV(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(base, "plugin"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(base, "plugin", "kv.json"), []byte("null"), 0o600))
	_, err := NewStore(base, "plugin")
	require.Error(t, err)
}

func TestStateRejectsReadableOutsideFiles(t *testing.T) {
	for _, test := range []struct{ name, content string }{
		{"kv.json", `{"secret":"outside"}`},
		{"log.jsonl", "{\"ID\":\"1\",\"Key\":\"secret\",\"Value\":\"outside\"}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "plugin")
			require.NoError(t, os.Mkdir(dir, 0o700))
			outside := filepath.Join(t.TempDir(), "readable")
			require.NoError(t, os.WriteFile(outside, []byte(test.content), 0o600))
			require.NoError(t, os.Symlink(outside, filepath.Join(dir, test.name)))
			store, err := NewStore(base, "plugin")
			require.Error(t, err)
			require.Nil(t, store)
		})
	}
}

func TestStateMutationRoundTrip(t *testing.T) {
	base := t.TempDir()
	store, err := NewStore(base, "plugin")
	require.NoError(t, err)
	require.NoError(t, store.Set("keep", "first"))
	require.NoError(t, store.Set("remove", "second"))
	require.NoError(t, store.Set("keep", "updated"))
	require.NoError(t, store.Delete("remove"))
	require.NoError(t, store.Delete("absent"))
	require.NoError(t, store.Append("event", "one\nline"))
	require.NoError(t, store.Append("event", "two"))
	reopened, err := NewStore(base, "plugin")
	require.NoError(t, err)
	value, ok := reopened.Get("keep")
	require.True(t, ok)
	require.Equal(t, "updated", value)
	_, ok = reopened.Get("remove")
	require.False(t, ok)
	require.Equal(t, store.Log(), reopened.Log())
	require.NoError(t, reopened.Clear())
	cleared, err := NewStore(base, "plugin")
	require.NoError(t, err)
	entries, err := cleared.KVEntries()
	require.NoError(t, err)
	require.Empty(t, entries)
	require.Empty(t, cleared.Log())
	files, err := os.ReadDir(filepath.Join(base, "plugin"))
	require.NoError(t, err)
	require.Len(t, files, 2)
	for _, file := range files {
		info, err := file.Info()
		require.NoError(t, err)
		if runtime.GOOS != "windows" {
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		}
	}
}

func TestStateFailureLeavesNoTemporaryFiles(t *testing.T) {
	store, err := NewStore(t.TempDir(), "plugin")
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(store.kvPath(), 0o700))
	require.Error(t, store.Set("new", "value"))
	files, err := os.ReadDir(store.dir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "kv.json", files[0].Name())
}

func TestStateRejectsMalformedLogAndTrailingKV(t *testing.T) {
	for _, test := range []struct{ name, content string }{
		{"kv.json", `{"key":"value"} {"extra":"value"}`},
		{"kv.json", `[]`},
		{"log.jsonl", "{\"ID\":\"1\"}\n{"},
	} {
		t.Run(test.content, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "plugin")
			require.NoError(t, os.Mkdir(dir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, test.name), []byte(test.content), 0o600))
			_, err := NewStore(base, "plugin")
			require.Error(t, err)
		})
	}
}

func TestExistingStoreCannotFollowReplacedPluginDirectory(t *testing.T) {
	for _, operation := range []string{"set", "append", "remove"} {
		t.Run(operation, func(t *testing.T) {
			base := t.TempDir()
			store, err := NewStore(base, "plugin")
			require.NoError(t, err)
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "keep")
			require.NoError(t, os.WriteFile(sentinel, []byte("original"), 0o600))
			require.NoError(t, os.RemoveAll(store.dir))
			require.NoError(t, os.Symlink(outside, store.dir))
			switch operation {
			case "set":
				err = store.Set("key", "value")
			case "append":
				err = store.Append("key", "value")
			default:
				err = store.Remove()
			}
			require.Error(t, err)
			files, err := os.ReadDir(outside)
			require.NoError(t, err)
			require.Len(t, files, 1)
			require.Equal(t, "keep", files[0].Name())
		})
	}
}
