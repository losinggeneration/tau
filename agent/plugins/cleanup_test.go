package plugins

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestShutdownPluginBoundsAndJoinsBlockedCleanup(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)
	mgr.cleanupGrace = 20 * time.Millisecond
	started, release := make(chan struct{}), make(chan struct{})
	var finished atomic.Bool
	mgr.plugins["blocking"] = &PluginInfo{
		Name:       "blocking",
		grpcClient: &mockPluginClient{gracefulResponse: true},
		Cleanup: func() {
			close(started)
			<-release
			finished.Store(true)
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	start := time.Now()
	err := mgr.ShutdownPlugin(ctx, "blocking", 10*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), time.Second)
	<-started
	require.ErrorIs(t, mgr.waitCleanups(t.Context()), context.DeadlineExceeded)

	close(release)
	require.NoError(t, mgr.waitCleanups(t.Context()))
	assert.True(t, finished.Load())
}

func TestShutdownPluginReportsCleanupPanic(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)
	mgr.plugins["panics"] = &PluginInfo{Name: "panics", Cleanup: func() { panic("boom") }}
	err := mgr.Shutdown(t.Context())
	require.ErrorContains(t, err, "cleanup plugin panics: panic: boom")
}

type failingListener struct {
	net.Listener
	err error
}

func (l failingListener) Close() error { return errors.Join(l.Listener.Close(), l.err) }

func TestShutdownPluginReportsHostSocketCloseFailure(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)
	path := filepath.Join(t.TempDir(), "host.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	closeErr := errors.New("socket close failed")
	mgr.hostSockets["socket"] = &hostSocket{path: path, server: grpc.NewServer(), ln: failingListener{Listener: ln, err: closeErr}}
	mgr.plugins["socket"] = &PluginInfo{Name: "socket", Cleanup: func() {}}

	require.ErrorIs(t, mgr.Shutdown(t.Context()), closeErr)
	assert.Empty(t, mgr.HostSocketPath("socket"))
	assert.NoFileExists(t, path)
}
