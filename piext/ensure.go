package piext

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
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
	if IsShimCurrent(dir) {
		return dir, nil
	}
	if isOfflineMode() {
		return "", fmt.Errorf("tau install --pi needs network; re-run online")
	}
	lockPath := dir + ".lock"
	if err := acquireLock(ctx, lockPath, status); err != nil {
		return "", err
	}
	defer func() {
		if removeErr := os.Remove(lockPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
			err = fmt.Errorf("remove shim lock: %w", removeErr)
		}
	}()

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
	if install == nil {
		install = NPMInstall
	}
	if err := install(ctx, newDir); err != nil {
		return "", fmt.Errorf("install shim: %w", err)
	}
	if _, err := os.Stat(filepath.Join(newDir, "node_modules", "jiti", "package.json")); err != nil {
		return "", fmt.Errorf("verify shim deps: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("remove old shim: %w", err)
	}
	if err := os.Rename(newDir, dir); err != nil {
		return "", fmt.Errorf("replace shim: %w", err)
	}
	return dir, nil
}

func shimCurrent(dir, embeddedHash string) bool {
	data, err := os.ReadFile(filepath.Join(dir, ".tau-shim-hash"))
	if err != nil || strings.TrimSpace(string(data)) != embeddedHash {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, "node_modules", "jiti", "package.json"))
	return err == nil
}

func acquireLock(ctx context.Context, lockPath string, status StatusFn) error {
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	reportedWait := false
	for {
		if err := tryWriteLock(lockPath); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create shim lock: %w", err)
		}
		alive, err := lockAlive(lockPath)
		if err != nil || !alive {
			if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove stale shim lock: %w", err)
			}
			continue
		}
		if !reportedWait && status != nil {
			status("Another tau instance is updating the shim; waiting…")
			reportedWait = true
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for shim lock: %w", ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("wait for shim lock timed out")
		case <-time.After(lockPoll):
		}
	}
}

func tryWriteLock(lockPath string) (err error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close lock: %w", closeErr)
		}
	}()
	if _, err := fmt.Fprintf(f, "%d\n%s\n", os.Getpid(), time.Now().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("write lock: %w", err)
	}
	return nil
}

func lockAlive(lockPath string) (bool, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read shim lock: %w", err)
	}
	line, _, _ := strings.Cut(string(data), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 0 {
		return false, nil
	}
	err = syscall.Kill(pid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return true, nil
}
