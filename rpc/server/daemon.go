package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"

	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/sdk"
)

const socketName = "tau.sock"

var (
	unixActiveConnections atomic.Int64
	unixTotalConnections  atomic.Int64
)

// UnixStats returns daemon connection counters for status and tests.
func UnixStats() (active, total int64) {
	return unixActiveConnections.Load(), unixTotalConnections.Load()
}

// SocketPath returns the default Unix socket path for the current user.
func SocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "tau", socketName)
	}
	return filepath.Join(os.TempDir(), "tau-"+strconv.Itoa(os.Getuid()), socketName)
}

// RunUnix exposes the JSONL control API on a Unix socket until ctx is canceled.
func RunUnix(ctx context.Context, path string, runtime sdk.Runtime, cmdRegistry commands.Registry, pluginManager *plugins.Manager) (err error) {
	if path == "" {
		path = SocketPath()
	}
	if err := prepareSocketPath(path); err != nil {
		return err
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen unix socket: %w", err)
	}
	defer func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			err = errors.Join(err, fmt.Errorf("close unix listener: %w", closeErr))
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove unix socket: %w", removeErr))
		}
	}()
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}

	go func() {
		<-ctx.Done()
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			_, writeErr := fmt.Fprintf(os.Stderr, "tau daemon listener close error: %v\n", closeErr)
			if writeErr != nil {
				return
			}
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) && ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept unix socket: %w", err)
		}
		go runConn(ctx, conn, runtime, cmdRegistry, pluginManager)
	}
}

func prepareSocketPath(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod socket dir: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}

func runConn(ctx context.Context, conn net.Conn, runtime sdk.Runtime, cmdRegistry commands.Registry, pluginManager *plugins.Manager) {
	unixActiveConnections.Add(1)
	unixTotalConnections.Add(1)
	defer unixActiveConnections.Add(-1)
	defer func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			if _, writeErr := fmt.Fprintf(os.Stderr, "tau daemon connection close error: %v\n", err); writeErr != nil {
				return
			}
		}
	}()
	srv := NewServer(runtime, cmdRegistry, pluginManager)
	srv.reader = bufio.NewReader(conn)
	srv.writer = conn
	if err := srv.Run(ctx); err != nil {
		if _, writeErr := fmt.Fprintf(os.Stderr, "tau daemon connection error: %v\n", err); writeErr != nil {
			return
		}
	}
}
