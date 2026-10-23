package piext

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// InstallFn installs shim runtime dependencies in dir.
type InstallFn func(context.Context, string) error

// StatusFn reports shim install progress.
type StatusFn func(string)

const lockPoll = 200 * time.Millisecond

// IsShimCurrent reports whether dir already contains the embedded shim and its runtime deps.
func IsShimCurrent(dir string) bool {
	return shimCurrent(dir, EmbeddedShimHash())
}

// EnsureShim materializes and dependency-installs the embedded shim when needed.
func EnsureShim(ctx context.Context, dir string, install InstallFn, status StatusFn) (shimPath string, err error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("ensure shim: %w", err)
	}
	if IsShimCurrent(dir) {
		return dir, nil
	}
	if isOfflineMode() {
		return "", fmt.Errorf("tau install --pi needs network; re-run online")
	}
	lockPath := dir + ".lock"
	lock, err := acquireLock(ctx, lockPath, status)
	if err != nil {
		return "", err
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("unlock shim: %w", unlockErr))
		}
	}()

	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("ensure shim: %w", err)
	}
	if IsShimCurrent(dir) {
		return dir, nil
	}
	newDir, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".new-*")
	if err != nil {
		return "", fmt.Errorf("create shim temp dir: %w", err)
	}
	defer func() {
		if err != nil {
			if rmErr := os.RemoveAll(newDir); rmErr != nil {
				err = fmt.Errorf("%w; remove temp dir: %w", err, rmErr)
			}
		}
	}()
	if err := WriteShimFilesystem(newDir); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("install shim: %w", err)
	}
	if install == nil {
		install = NPMInstall
	}
	if err := install(ctx, newDir); err != nil {
		return "", fmt.Errorf("install shim: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("install shim: %w", err)
	}
	if _, err := os.Stat(filepath.Join(newDir, "node_modules", "jiti", "package.json")); err != nil {
		return "", fmt.Errorf("verify shim deps: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("replace shim: %w", err)
	}
	if err := replaceShim(newDir, dir, os.Rename); err != nil {
		return "", err
	}
	return dir, nil
}

// ponytail: two renames leave a crash window; versioned directories with an atomic
// activation pointer are needed for crash-safe replacement.
func replaceShim(newDir, dir string, rename func(string, string) error) (err error) {
	backup, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".old-*")
	if err != nil {
		return fmt.Errorf("create shim backup: %w", err)
	}
	oldDir := filepath.Join(backup, "shim")
	var keepBackup bool
	defer func() {
		if !keepBackup {
			if cleanupErr := os.RemoveAll(backup); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove shim backup %s: %w", backup, cleanupErr))
			}
		}
	}()
	if err := rename(dir, oldDir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("back up installed shim: %w", err)
		}
	} else {
		keepBackup = true
	}
	if err := rename(newDir, dir); err != nil {
		replaceErr := fmt.Errorf("replace shim: %w", err)
		if keepBackup {
			if restoreErr := rename(oldDir, dir); restoreErr != nil {
				return errors.Join(replaceErr, fmt.Errorf("restore shim (preserved at %s): %w", oldDir, restoreErr))
			}
			keepBackup = false
		}
		return replaceErr
	}
	keepBackup = false
	return nil
}

func shimCurrent(dir, embeddedHash string) bool {
	data, err := os.ReadFile(filepath.Join(dir, ".tau-shim-hash"))
	if err != nil || strings.TrimSpace(string(data)) != embeddedHash {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, "node_modules", "jiti", "package.json"))
	return err == nil
}

func acquireLock(ctx context.Context, lockPath string, status StatusFn) (*flock.Flock, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("acquire shim lock: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	// Keep the lock file in place so contenders always lock the same inode.
	lock := flock.New(lockPath, flock.SetPermissions(0o600))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire shim lock: %w", err)
	}
	if locked {
		return lock, nil
	}
	if status != nil {
		status("Another tau instance is updating the shim; waiting…")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	locked, err = lock.TryLockContext(ctx, lockPoll)
	if err != nil {
		return nil, fmt.Errorf("wait for shim lock: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("wait for shim lock: %w", context.DeadlineExceeded)
	}
	return lock, nil
}
