package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/losinggeneration/tau/agent/plugins/proto"
	"github.com/losinggeneration/tau/ai"
	"google.golang.org/grpc"
)

var hostSocketSequence atomic.Uint64

type stagedHostContextKey struct{}

// hostSocket pairs the per-plugin Host service gRPC server with its unix
// socket listener. The socket is created BEFORE the plugin process launches
// so its path can be injected via the TAU_HOST_SOCKET environment variable.
type hostSocket struct {
	path    string
	server  *grpc.Server
	ln      net.Listener
	staging *atomic.Bool
	retired *atomic.Bool
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
	return m.prepareHostSocket(context.Background(), pluginName)
}

func (m *Manager) prepareHostSocket(ctx context.Context, pluginName string) (string, error) {
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

	socketPath := filepath.Join(dir, fmt.Sprintf("tau-host-%s-%d-%d.sock", pluginName, os.Getpid(), hostSocketSequence.Add(1)))
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

	var staging atomic.Bool
	var retired atomic.Bool
	staging.Store(ctx.Value(stagedHostContextKey{}) != nil)
	var snapshot *HostService
	if staging.Load() {
		snapshotSettings := &ai.Settings{}
		if m.settings != nil {
			snapshotSettings.Plugins = map[string]json.RawMessage{pluginName: slices.Clone(m.settings.Plugins[pluginName])}
		}
		snapshot = NewHostService(&Manager{settings: snapshotSettings, cliFlags: map[string]map[string]string{pluginName: maps.Clone(m.cliFlags[pluginName])}, cliFlagOverrides: map[string]map[string]string{pluginName: maps.Clone(m.cliFlagOverrides[pluginName])}})
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		identity, _ := metadata.FromIncomingContext(ctx)
		names := identity.Get("tau-plugin-name")
		if len(names) != 1 || names[0] != pluginName {
			return nil, status.Error(codes.PermissionDenied, "plugin identity does not match Host endpoint")
		}
		if retired.Load() {
			return nil, status.Error(codes.FailedPrecondition, "plugin Host endpoint is retired")
		}
		if !staging.Load() {
			return handler(ctx, req)
		}
		switch info.FullMethod {
		case proto.Host_GetConfig_FullMethodName:
			return snapshot.GetConfig(ctx, req.(*proto.GetConfigRequest))
		case proto.Host_Log_FullMethodName:
			return handler(ctx, req)
		default:
			return nil, status.Error(codes.FailedPrecondition, "plugin reload is not admitted")
		}
	}))
	proto.RegisterHostServer(server, m.host)

	hs := &hostSocket{path: socketPath, server: server, ln: ln, staging: &staging, retired: &retired}
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
