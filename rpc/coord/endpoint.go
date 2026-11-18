package coord

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gofrs/flock"
)

const staleProbeTimeout = time.Second

// ErrAlreadyRunning reports that another process owns the coordinator endpoint.
var ErrAlreadyRunning = errors.New("coordinator already running")

// endpoint is a Unix listener owned through an exclusive lock held for its
// whole lifetime. Only the lock owner removes the socket.
type endpoint struct {
	path     string
	lock     *flock.Flock
	listener *net.UnixListener

	closeOnce sync.Once
	closeErr  error
}

func listen(path string) (ep *endpoint, err error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("coordinator socket path must be absolute: %q", path)
	}
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	lockPath := path + ".lock"
	if err := refuseNonRegular(lockPath); err != nil {
		return nil, err
	}
	// The lock file stays in place so contenders always lock the same inode.
	lock := flock.New(lockPath, flock.SetPermissions(0o600))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock coordinator endpoint: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("%w: %s is locked", ErrAlreadyRunning, lockPath)
	}
	defer func() {
		if err != nil {
			if unlockErr := lock.Unlock(); unlockErr != nil {
				err = errors.Join(err, fmt.Errorf("unlock coordinator endpoint: %w", unlockErr))
			}
		}
	}()

	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen coordinator socket: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	ep = &endpoint{path: path, lock: lock, listener: listener}
	if err := os.Chmod(path, 0o600); err != nil {
		err = fmt.Errorf("chmod coordinator socket: %w", err)
		if cleanupErr := ep.cleanup(); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		return nil, err
	}
	return ep, nil
}

func (ep *endpoint) closeListener() {
	ep.closeOnce.Do(func() {
		if err := ep.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			ep.closeErr = fmt.Errorf("close coordinator listener: %w", err)
		}
	})
}

// cleanup removes the socket while still holding the lock. Unlocking is left
// to the caller.
func (ep *endpoint) cleanup() error {
	ep.closeListener()
	err := ep.closeErr
	if removeErr := os.Remove(ep.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, fmt.Errorf("remove coordinator socket: %w", removeErr))
	}
	return err
}

func (ep *endpoint) close() error {
	err := ep.cleanup()
	if unlockErr := ep.lock.Unlock(); unlockErr != nil {
		err = errors.Join(err, fmt.Errorf("unlock coordinator endpoint: %w", unlockErr))
	}
	return err
}

// ensurePrivateDir creates dir (but not its parents) and refuses symlinks,
// other owners, and group/world access. It never changes existing modes.
func ensurePrivateDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create coordinator dir: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat coordinator dir: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("coordinator dir %s is a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("coordinator dir %s is not a directory", dir)
	}
	if uid, ok := fileOwner(info); !ok || uid != os.Getuid() {
		return fmt.Errorf("coordinator dir %s is not owned by the current user", dir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("coordinator dir %s has mode %o; want no group or other access", dir, perm)
	}
	return nil
}

func refuseNonRegular(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular file %s", path)
	}
	return nil
}

// removeStaleSocket removes a socket nothing is listening on. A live
// listener, such as a daemon that predates the lock, is left untouched.
func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat coordinator socket: %w", err)
	}
	if info.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("refusing to replace non-socket %s", path)
	}
	conn, err := net.DialTimeout("unix", path, staleProbeTimeout)
	if err == nil {
		if closeErr := conn.Close(); closeErr != nil {
			return fmt.Errorf("%w: live listener at %s: %w", ErrAlreadyRunning, path, closeErr)
		}
		return fmt.Errorf("%w: live listener at %s", ErrAlreadyRunning, path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("probe existing coordinator socket: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale coordinator socket: %w", err)
	}
	return nil
}
