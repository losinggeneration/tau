package piext

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureShimMaterializesAndInstalls(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shim")
	var installed atomic.Int64
	got, err := EnsureShim(context.Background(), dir, func(_ context.Context, installDir string) error {
		installed.Add(1)
		if err := os.MkdirAll(filepath.Join(installDir, "node_modules", "jiti"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(installDir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
	}, nil)
	if err != nil {
		t.Fatalf("EnsureShim() error = %v", err)
	}
	if got != dir {
		t.Fatalf("EnsureShim() = %q, want %q", got, dir)
	}
	if installed.Load() != 1 {
		t.Fatalf("install count = %d, want 1", installed.Load())
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		t.Fatalf("package.json missing: %v", err)
	}
}

func TestEnsureShimFastPathSkipsInstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shim")
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "jiti"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tau-shim-hash"), []byte(EmbeddedShimHash()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := EnsureShim(context.Background(), dir, func(context.Context, string) error {
		t.Fatal("install should not run")
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("EnsureShim() error = %v", err)
	}
}

func TestEnsureShimRefreshesChangedSources(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shim")
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "jiti"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), EmbeddedPackageJSON(), 0o600); err != nil {
		t.Fatal(err)
	}

	var installed atomic.Int64
	_, err := EnsureShim(context.Background(), dir, func(_ context.Context, installDir string) error {
		installed.Add(1)
		if err := os.MkdirAll(filepath.Join(installDir, "node_modules", "jiti"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(installDir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
	}, nil)
	if err != nil {
		t.Fatalf("EnsureShim() error = %v", err)
	}
	if installed.Load() != 1 {
		t.Fatalf("install count = %d, want 1", installed.Load())
	}
}

func TestEnsureShimStaleLockTakeover(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shim")
	if err := os.WriteFile(dir+".lock", []byte("999999999\n2024-01-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := EnsureShim(context.Background(), dir, func(_ context.Context, installDir string) error {
		if err := os.MkdirAll(filepath.Join(installDir, "node_modules", "jiti"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(installDir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
	}, nil)
	if err != nil {
		t.Fatalf("EnsureShim() error = %v", err)
	}
	lock, err := acquireLock(t.Context(), dir+".lock", nil)
	if err != nil {
		t.Fatalf("reacquire lock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureShimConcurrentSingleInstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shim")
	var installed atomic.Int64
	errCh := make(chan error, 2)
	install := func(_ context.Context, installDir string) error {
		installed.Add(1)
		if err := os.MkdirAll(filepath.Join(installDir, "node_modules", "jiti"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(installDir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
	}
	for range 2 {
		go func() {
			_, err := EnsureShim(context.Background(), dir, install, nil)
			errCh <- err
		}()
	}
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("EnsureShim() error = %v", err)
		}
	}
	if installed.Load() != 1 {
		t.Fatalf("install count = %d, want 1", installed.Load())
	}
}

func TestEnsureShimCancelledAdmissionLeavesInstallationUntouched(t *testing.T) {
	for _, state := range []string{"missing", "stale", "current", "offline"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("PI_OFFLINE", "0")
			root := t.TempDir()
			dir := filepath.Join(root, "shim")
			if state != "missing" {
				if err := WriteShimFilesystem(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(dir, "node_modules", "jiti"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if state != "current" {
					if err := os.WriteFile(filepath.Join(dir, ".tau-shim-hash"), []byte("old\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if state == "offline" {
				t.Setenv("PI_OFFLINE", "1")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			var installs, statuses int
			got, err := EnsureShim(ctx, dir, func(context.Context, string) error {
				installs++
				return nil
			}, func(string) { statuses++ })
			if !errors.Is(err, context.Canceled) || got != "" {
				t.Errorf("EnsureShim = %q, %v, want cancellation and no path", got, err)
			}
			if installs != 0 || statuses != 0 {
				t.Errorf("installs = %d, statuses = %d, want neither", installs, statuses)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			wantEntries := 1
			if state == "missing" {
				wantEntries = 0
			}
			if len(entries) != wantEntries {
				t.Fatalf("root entries = %v, want %d unchanged entries", entries, wantEntries)
			}
			if state != "missing" {
				data, err := os.ReadFile(filepath.Join(dir, ".tau-shim-hash"))
				if err != nil {
					t.Fatal(err)
				}
				wantHash := "old\n"
				if state == "current" {
					wantHash = EmbeddedShimHash() + "\n"
				}
				if string(data) != wantHash {
					t.Errorf("shim hash = %q, want %q", data, wantHash)
				}
			}
		})
	}
}

func TestAcquireLockCancelledAdmissionDoesNotWriteOrRemove(t *testing.T) {
	for _, state := range []string{"missing", "stale"} {
		t.Run(state, func(t *testing.T) {
			lockPath := filepath.Join(t.TempDir(), "nested", "shim.lock")
			if state == "stale" {
				if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(lockPath, []byte("invalid lock\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := acquireLock(ctx, lockPath, nil); !errors.Is(err, context.Canceled) {
				t.Errorf("acquireLock = %v, want cancellation", err)
			}
			if state == "missing" {
				if _, err := os.Stat(filepath.Dir(lockPath)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("lock parent stat = %v, want no directory creation", err)
				}
			} else {
				data, err := os.ReadFile(lockPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "invalid lock\n" {
					t.Errorf("lock contents = %q, want unchanged stale lock", data)
				}
			}
		})
	}
}

func TestEnsureShimCancellationAfterInstallPreservesOldShim(t *testing.T) {
	for _, deps := range []string{"installed", "missing"} {
		t.Run(deps, func(t *testing.T) {
			t.Setenv("PI_OFFLINE", "0")
			root := t.TempDir()
			dir := filepath.Join(root, "shim")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			oldPath := filepath.Join(dir, "old-source.js")
			if err := os.WriteFile(oldPath, []byte("old source"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var staged string
			got, err := EnsureShim(ctx, dir, func(installCtx context.Context, installDir string) error {
				if installCtx != ctx {
					t.Error("installer did not receive caller context")
				}
				staged = installDir
				if deps == "installed" {
					if err := os.MkdirAll(filepath.Join(installDir, "node_modules", "jiti"), 0o700); err != nil {
						return err
					}
					if err := os.WriteFile(filepath.Join(installDir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600); err != nil {
						return err
					}
				}
				cancel()
				return nil
			}, nil)
			if !errors.Is(err, context.Canceled) || got != "" {
				t.Errorf("EnsureShim = %q, %v, want cancellation and no path", got, err)
			}
			data, err := os.ReadFile(oldPath)
			if err != nil {
				t.Errorf("read old shim: %v", err)
			} else if string(data) != "old source" {
				t.Errorf("old shim = %q, want old source", data)
			}
			if staged == "" {
				t.Fatal("installer was not admitted")
			}
			if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("staged directory stat = %v, want cleaned up", err)
			}
			lock, err := acquireLock(t.Context(), dir+".lock", nil)
			if err != nil {
				t.Fatalf("reacquire lock: %v", err)
			}
			if err := lock.Unlock(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEnsureShimInstallFailureReleasesLock(t *testing.T) {
	t.Setenv("PI_OFFLINE", "0")
	dir := filepath.Join(t.TempDir(), "shim")
	installErr := errors.New("install failed")
	_, err := EnsureShim(t.Context(), dir, func(context.Context, string) error {
		return installErr
	}, nil)
	if !errors.Is(err, installErr) {
		t.Errorf("EnsureShim = %v, want install failure", err)
	}
	lock, err := acquireLock(t.Context(), dir+".lock", nil)
	if err != nil {
		t.Fatalf("reacquire lock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "shim.lock" {
		t.Errorf("entries = %v, want only persistent advisory lock", entries)
	}
}

func TestAcquireLockCancellationWhileContended(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "shim.lock")
	lock, err := acquireLock(t.Context(), lockPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Unlock(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var statuses int
	other, err := acquireLock(ctx, lockPath, func(string) {
		statuses++
		cancel()
	})
	if other != nil {
		if err := other.Unlock(); err != nil {
			t.Error(err)
		}
		t.Fatal("contender acquired held lock")
	}
	if !errors.Is(err, context.Canceled) || statuses != 1 {
		t.Errorf("acquireLock = %v, statuses = %d, want cancellation after wait notice", err, statuses)
	}
}

func TestReplaceShimFailurePreservesInstalledTree(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "shim")
	staged := filepath.Join(root, "new")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.MkdirAll(staged, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.js"), []byte("installed source"), 0o600))
	replaceErr := errors.New("replacement failed")
	err := replaceShim(staged, dir, func(from, to string) error {
		if from == staged {
			return replaceErr
		}
		return os.Rename(from, to)
	})
	require.ErrorIs(t, err, replaceErr)
	data, err := os.ReadFile(filepath.Join(dir, "old.js"))
	require.NoError(t, err, "failed replacement must leave the installed shim available")
	assert.Equal(t, "installed source", string(data))
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "successful rollback must remove its backup container")
}

func TestReplaceShimRetainsBackupWhenRollbackFails(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "shim")
	staged := filepath.Join(root, "new")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.MkdirAll(staged, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.js"), []byte("installed source"), 0o600))
	replaceErr := errors.New("replacement failed")
	restoreErr := errors.New("rollback failed")
	err := replaceShim(staged, dir, func(from, to string) error {
		switch from {
		case dir:
			return os.Rename(from, to)
		case staged:
			return replaceErr
		default:
			return restoreErr
		}
	})
	require.ErrorIs(t, err, replaceErr)
	require.ErrorIs(t, err, restoreErr)
	backups, globErr := filepath.Glob(dir + ".old-*")
	require.NoError(t, globErr)
	require.Len(t, backups, 1)
	preserved := filepath.Join(backups[0], "shim")
	assert.ErrorContains(t, err, preserved)
	data, readErr := os.ReadFile(filepath.Join(preserved, "old.js"))
	require.NoError(t, readErr)
	assert.Equal(t, "installed source", string(data))
}

func TestReplaceShimCommitAndBackupFailure(t *testing.T) {
	for _, state := range []string{"missing", "installed", "backup failure", "symlink"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "shim")
			staged := filepath.Join(root, "new")
			require.NoError(t, os.MkdirAll(staged, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(staged, "new.js"), []byte("replacement source"), 0o600))
			if state == "symlink" {
				target := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(target, "old.js"), []byte("installed source"), 0o600))
				require.NoError(t, os.Symlink(target, dir))
				t.Cleanup(func() {
					data, err := os.ReadFile(filepath.Join(target, "old.js"))
					require.NoError(t, err)
					assert.Equal(t, "installed source", string(data), "backup cleanup must not follow symlinks")
				})
			} else if state != "missing" {
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "old.js"), []byte("installed source"), 0o600))
			}
			backupErr := errors.New("backup denied")
			err := replaceShim(staged, dir, func(from, to string) error {
				if state == "backup failure" && from == dir {
					return backupErr
				}
				return os.Rename(from, to)
			})
			if state == "backup failure" {
				require.ErrorIs(t, err, backupErr)
				data, err := os.ReadFile(filepath.Join(dir, "old.js"))
				require.NoError(t, err)
				assert.Equal(t, "installed source", string(data))
				_, err = os.Stat(staged)
				require.NoError(t, err)
			} else {
				require.NoError(t, err)
				data, err := os.ReadFile(filepath.Join(dir, "new.js"))
				require.NoError(t, err)
				assert.Equal(t, "replacement source", string(data))
				_, err = os.Stat(staged)
				require.ErrorIs(t, err, os.ErrNotExist)
				_, err = os.Stat(filepath.Join(dir, "old.js"))
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			backups, err := filepath.Glob(dir + ".old-*")
			require.NoError(t, err)
			assert.Empty(t, backups)
		})
	}
}
