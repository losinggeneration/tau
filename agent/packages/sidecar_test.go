package packages

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSidecarWriteDoesNotFollowExistingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("original"), 0o600))
	require.NoError(t, os.Symlink(outside, sidecarPath(root)))
	sidecar := InstallSidecar{Source: "local", Scope: string(ScopeUser)}
	require.NoError(t, sidecar.Write(root))
	content, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "original", string(content))
	info, err := os.Lstat(sidecarPath(root))
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular())
	actual, err := LoadSidecar(root)
	require.NoError(t, err)
	require.Equal(t, sidecar, *actual)
}

func TestSidecarFailedReplacementLeavesNoTemporaryFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(sidecarPath(root), 0o700))
	require.Error(t, (InstallSidecar{Source: "local"}).Write(root))
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, ".tau-install.json", entries[0].Name())
	require.True(t, entries[0].IsDir())
}

func TestLoadSidecarRejectsInvalidRoots(t *testing.T) {
	for _, data := range []string{"null", "[]", "true", "\"text\""} {
		t.Run(data, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(sidecarPath(root), []byte(data), 0o600))
			sidecar, err := LoadSidecar(root)
			require.Error(t, err)
			require.Nil(t, sidecar)
		})
	}
}
