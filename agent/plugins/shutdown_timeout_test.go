package plugins

import (
	"context"
	"testing"
	"time"
)

func TestShutdownPluginDoesNotHangWhenCleanupBlocks(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)
	cleanupStarted := make(chan struct{})

	mgr.mu.Lock()
	mgr.plugins["blocking-cleanup"] = &PluginInfo{
		Name: "blocking-cleanup",
		Path: "/fake/path",
		grpcClient: &mockPluginClient{
			gracefulResponse: true,
		},
		Cleanup: func() {
			close(cleanupStarted)
			select {}
		},
	}
	mgr.mu.Unlock()

	start := time.Now()
	err := mgr.ShutdownPlugin(context.Background(), "blocking-cleanup", 10*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("shutdown plugin: %v", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("shutdown hung for %v", elapsed)
	}

	select {
	case <-cleanupStarted:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cleanup was not invoked")
	}
}
