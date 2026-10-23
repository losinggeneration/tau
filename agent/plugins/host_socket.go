package plugins

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"github.com/losinggeneration/tau/agent/plugins/proto"
	"google.golang.org/grpc"
)

// hostSocket pairs the per-plugin Host service gRPC server with its unix
// socket listener. The socket is created BEFORE the plugin process launches
// so its path can be injected via the TAU_HOST_SOCKET environment variable.
type hostSocket struct {
	path   string
	server *grpc.Server
	ln     net.Listener
}

// close stops the server and removes the socket file. Stopping the server
// already closes the listener, which may also unlink the socket.
func (h *hostSocket) close() error {
	h.server.Stop()
	var err error
	if closeErr := h.ln.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		err = fmt.Errorf("close host socket %s: %w", h.path, closeErr)
	}
	if removeErr := os.Remove(h.path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		err = errors.Join(err, fmt.Errorf("remove host socket %s: %w", h.path, removeErr))
	}
	return err
}

// hostSocketDir returns the directory used for Host service sockets. Prefers
// $XDG_RUNTIME_DIR (private, tmpfs on systemd systems), then ~/.tau/run, then
// the shared temp dir as a last resort.
func hostSocketDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "tau")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".tau", "run")
	}
	return os.TempDir()
}

// prepareHostSocketLocked creates and starts serving the Host service socket
// for the named plugin, closing any previous socket for that name (reload).
// The returned path is what the plugin dials for Host RPCs.
//
// NOTE: takes hostMu, not m.mu — loadLocked runs while the caller holds m.mu.
func (m *Manager) prepareHostSocketLocked(pluginName string) (string, error) {
	m.hostMu.Lock()
	defer m.hostMu.Unlock()

	if prev := m.hostSockets[pluginName]; prev != nil {
		delete(m.hostSockets, pluginName)
		if err := prev.close(); err != nil {
			return "", err
		}
	}

	dir := hostSocketDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create host socket dir %s: %w", dir, err)
	}

	socketPath := filepath.Join(dir, fmt.Sprintf("tau-host-%s-%d.sock", pluginName, os.Getpid()))
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove stale host socket %s: %w", socketPath, err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return "", fmt.Errorf("listen host socket %s: %w", socketPath, err)
	}
	// Lock down the socket: Host RPCs expose UI prompts, session writes, and
	// per-plugin state, so other local users must not connect.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		err = fmt.Errorf("chmod host socket %s: %w", socketPath, err)
		if closeErr := ln.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close host socket %s: %w", socketPath, closeErr))
		}
		if removeErr := os.Remove(socketPath); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove host socket %s: %w", socketPath, removeErr))
		}
		return "", err
	}

	server := grpc.NewServer()
	proto.RegisterHostServer(server, m.host)

	hs := &hostSocket{path: socketPath, server: server, ln: ln}
	m.hostSockets[pluginName] = hs

	go func() {
		if err := server.Serve(ln); err != nil {
			m.logger.Warn("plugin host server stopped", "plugin", pluginName, "error", err)
		}
	}()

	return socketPath, nil
}

// closeHostSocketLocked stops and removes the Host socket for a plugin.
func (m *Manager) closeHostSocketLocked(pluginName string) error {
	m.hostMu.Lock()
	defer m.hostMu.Unlock()
	hs := m.hostSockets[pluginName]
	if hs == nil {
		return nil
	}
	delete(m.hostSockets, pluginName)
	return hs.close()
}
