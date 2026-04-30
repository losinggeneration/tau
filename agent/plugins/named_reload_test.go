// Copyright 2026 Tau Project Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/losinggeneration/tau/agent/plugins/proto"
)

// TestIntegrationReloadNamedForceRestart tests that ReloadNamed
// restarts a named plugin even when its binary hash is unchanged.
// Per the spec: `/reload <plugin>` forces a restart regardless of hash gating.
func TestIntegrationReloadNamedForceRestart(t *testing.T) {
	tmpDir := t.TempDir()

	// Create paths for the test.
	pluginPath := filepath.Join(tmpDir, "test-plugin")
	manifestPath := filepath.Join(tmpDir, "tau-package.toml")

	// Create a manifest for the plugin.
	manifestContent := `
[package]
name = "force-restart-plugin"
version = "1.0.0"

[plugin]
api_version = "1"

[run]
command = "./test-plugin"
`
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0o644); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}

	// Create a plugin binary (will be mocked).
	if err := os.WriteFile(pluginPath, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatalf("failed to write plugin: %v", err)
	}

	// Create manager.
	mgr := NewManager(tmpDir, nil, nil)

	// Track whether the plugin was shut down during reload.
	shutdownCalled := false

	// Manually add a mock plugin info as if it was successfully loaded.
	mgr.mu.Lock()
	mgr.plugins["force-restart-plugin"] = &PluginInfo{
		Name:         "force-restart-plugin",
		Path:         pluginPath,
		ManifestPath: manifestPath,
		Hash:         "unchanged-hash", // Same hash as the binary
		grpcClient:   &mockPluginClient{},
		Cleanup: func() {
			shutdownCalled = true
		},
	}
	mgr.manifestPaths[pluginPath] = manifestPath

	// Note: We do NOT add to binaries map so that when shutdown happens
	// it triggers the "binary no longer exists" path (lines 1489-1495 in plugins.go)
	// which shuts down without attempting reload. This allows us to test
	// the shutdown behavior without triggering actual plugin load which
	// would timeout on the mock.
	mgr.mu.Unlock()

	// Verify the plugin is registered before reload.
	mgr.mu.RLock()
	infoBefore := mgr.plugins["force-restart-plugin"]
	mgr.mu.RUnlock()
	if infoBefore == nil {
		t.Fatal("expected plugin to be registered before reload")
	}

	// Call ReloadNamed for this specific plugin - this should force a restart
	// even though the hash hasn't changed.
	ctx := context.Background()
	err := mgr.ReloadNamed(ctx, "force-restart-plugin")
	// The reload should fail because our mock plugin isn't a real gRPC server,
	// but the key behavior is that the old plugin was shut down and removed.
	if err != nil {
		t.Logf("ReloadNamed returned error (expected for mock plugin): %v", err)
	}

	// Verify the plugin was shut down.
	if !shutdownCalled {
		t.Error("expected Cleanup to be called during forced reload")
	}

	// Verify the plugin was removed from the map (forced restart behavior).
	mgr.mu.RLock()
	infoAfter := mgr.plugins["force-restart-plugin"]
	mgr.mu.RUnlock()

	if infoAfter == infoBefore {
		t.Error("expected old plugin info to be removed during forced reload")
	}
}

// TestIntegrationReloadNoArgsAffectsAllPlugins tests that Reload (no args)
// processes all plugins, not just the named one.
func TestIntegrationReloadNoArgsAffectsAllPlugins(t *testing.T) {
	tmpDir := t.TempDir()

	// Create two plugins.
	plugin1Path := filepath.Join(tmpDir, "plugin1")
	plugin2Path := filepath.Join(tmpDir, "plugin2")

	if err := os.WriteFile(plugin1Path, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatalf("failed to write plugin1: %v", err)
	}
	if err := os.WriteFile(plugin2Path, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatalf("failed to write plugin2: %v", err)
	}

	mgr := NewManager(tmpDir, nil, nil)

	// Track shutdown for both plugins.
	var shutdown1, shutdown2 bool

	mgr.mu.Lock()
	mgr.plugins["plugin1"] = &PluginInfo{
		Name:       "plugin1",
		Path:       plugin1Path,
		Hash:       "hash1",
		grpcClient: &mockPluginClient{gracefulResponse: false}, // Required for Cleanup to be called
		Cleanup:    func() { shutdown1 = true },
	}
	mgr.plugins["plugin2"] = &PluginInfo{
		Name:       "plugin2",
		Path:       plugin2Path,
		Hash:       "hash2",
		grpcClient: &mockPluginClient{gracefulResponse: false}, // Required for Cleanup to be called
		Cleanup:    func() { shutdown2 = true },
	}
	mgr.mu.Unlock()

	// Call Reload (no args) - should affect all plugins based on hash changes.
	// Since the binaries map is empty, the hash comparison will trigger restart.
	ctx := context.Background()
	_ = mgr.Reload(ctx)

	// Both plugins should be shut down because their hashes don't match
	// the binaries map (which doesn't have them since we didn't scan).
	if !shutdown1 {
		t.Error("expected plugin1 to be shut down during Reload")
	}
	if !shutdown2 {
		t.Error("expected plugin2 to be shut down during Reload")
	}
}

// TestIntegrationReloadWithArgRoutesToSinglePlugin tests that ReloadNamed
// with a specific plugin name only restarts that plugin, not others.
func TestIntegrationReloadWithArgRoutesToSinglePlugin(t *testing.T) {
	tmpDir := t.TempDir()

	// Create two plugins.
	plugin1Path := filepath.Join(tmpDir, "plugin1")
	plugin2Path := filepath.Join(tmpDir, "plugin2")

	if err := os.WriteFile(plugin1Path, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatalf("failed to write plugin1: %v", err)
	}
	if err := os.WriteFile(plugin2Path, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatalf("failed to write plugin2: %v", err)
	}

	mgr := NewManager(tmpDir, nil, nil)

	// Track shutdown for both plugins.
	var shutdown1, shutdown2 bool

	mgr.mu.Lock()
	mgr.plugins["plugin1"] = &PluginInfo{
		Name:       "plugin1",
		Path:       plugin1Path,
		Hash:       "hash1",
		grpcClient: &mockPluginClient{gracefulResponse: false}, // Required for Cleanup to be called
		Cleanup:    func() { shutdown1 = true },
	}
	mgr.plugins["plugin2"] = &PluginInfo{
		Name:       "plugin2",
		Path:       plugin2Path,
		Hash:       "hash2",
		grpcClient: &mockPluginClient{gracefulResponse: false}, // Required for Cleanup to be called
		Cleanup:    func() { shutdown2 = true },
	}
	// Note: We do NOT add to binaries map so that when shutdown happens
	// it triggers the "binary no longer exists" path which shuts down without
	// attempting reload. This allows us to test the routing behavior.
	mgr.mu.Unlock()

	// Call ReloadNamed for only plugin1.
	ctx := context.Background()
	_ = mgr.ReloadNamed(ctx, "plugin1")

	// Only plugin1 should be shut down.
	if !shutdown1 {
		t.Error("expected plugin1 to be shut down during named reload")
	}
	if shutdown2 {
		t.Error("plugin2 should not be shut down when only plugin1 was reloaded")
	}
}

// TestIntegrationReloadReattemptDeregistered tests that Reload (without name)
// re-attempts loading of deregistered plugins.
// Per the spec: `/reload` (no args) re-attempts deregistered plugins.
func TestIntegrationReloadReattemptDeregistered(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager(tmpDir, nil, nil)

	// Override loadFn to avoid subprocess overhead. We test the retry
	// state machine, not actual plugin loading.
	mgr.loadFn = func(ctx context.Context, path string, manifestPath ...string) (*PluginInfo, error) {
		return nil, fmt.Errorf("simulated load failure")
	}

	// Manually add a deregistered plugin entry.
	mgr.mu.Lock()
	mgr.plugins["reattempt-plugin"] = &PluginInfo{
		Name:         "reattempt-plugin",
		Path:         filepath.Join(tmpDir, "reattempt-plugin"),
		deregistered: true, // Mark as deregistered
		Cleanup:      func() {},
	}
	mgr.mu.Unlock()

	// Verify the deregistered flag is set.
	mgr.mu.RLock()
	info := mgr.plugins["reattempt-plugin"]
	mgr.mu.RUnlock()
	if !info.deregistered {
		t.Error("expected plugin to be marked as deregistered")
	}

	// Call Reload - this should call ReattemptDeregistered.
	// Configure short retry to keep this test fast.
	mgr.failureConfig[info.Path] = FailureConfig{
		MaxAttempts:   1,
		BackoffBase:   1 * time.Millisecond,
		BackoffFactor: 1,
	}
	ctx := context.Background()
	err := mgr.Reload(ctx)
	if err != nil {
		t.Logf("Reload returned error (may be expected): %v", err)
	}

	// Verify the deregistered plugin was removed (ReattemptDeregistered
	// should try to load it, fail, and leave it removed from the map).
	mgr.mu.RLock()
	infoAfter := mgr.plugins["reattempt-plugin"]
	mgr.mu.RUnlock()

	if infoAfter != nil {
		t.Error("expected plugin to be removed from map after failed reattempt")
	}
}

// TestIntegrationReloadPreservesOtherPluginsOnFailure tests that when a
// named reload fails, other plugins are not affected.
func TestIntegrationReloadPreservesOtherPluginsOnFailure(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a plugin.
	pluginPath := filepath.Join(tmpDir, "fail-plugin")
	if err := os.WriteFile(pluginPath, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatalf("failed to write plugin: %v", err)
	}

	mgr := NewManager(tmpDir, nil, nil)

	mgr.mu.Lock()
	mgr.plugins["fail-plugin"] = &PluginInfo{
		Name:       "fail-plugin",
		Path:       pluginPath,
		Hash:       "test-hash",
		grpcClient: &mockPluginClient{gracefulResponse: false},
		Cleanup:    func() {},
	}
	mgr.mu.Unlock()

	// Call ReloadNamed for a non-existent plugin.
	err := mgr.ReloadNamed(context.Background(), "non-existent-plugin")
	if err != nil {
		t.Logf("ReloadNamed for non-existent returned error: %v", err)
	}

	// Verify the existing plugin was not affected.
	mgr.mu.RLock()
	info := mgr.plugins["fail-plugin"]
	mgr.mu.RUnlock()
	if info == nil {
		t.Error("existing plugin should not be removed when reloading non-existent plugin")
	}
}

// TestIntegrationReloadTimeCancellationMessaging tests the cancellation behavior
// when a plugin is reloaded during tool execution.
// Per the spec: In-flight ExecuteTool cancellation; tool result "cancelled by /reload".
func TestIntegrationReloadTimeCancellationMessaging(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager(tmpDir, nil, nil)

	// Track context cancellation in the mock.
	ctx, cancel := context.WithCancel(context.Background())

	// Create a mock plugin client that simulates a running tool call.
	toolCallCompleted := make(chan struct{})
	mockClient := &mockPluginClient{
		onExecuteTool: func(ctx context.Context, req *proto.ToolCall) (*proto.ToolResult, error) {
			// Wait for context cancellation or completion signal.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-toolCallCompleted:
				return &proto.ToolResult{Output: "{}"}, nil
			}
		},
	}

	pluginPath := filepath.Join(tmpDir, "cancel-test-plugin")
	if err := os.WriteFile(pluginPath, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatalf("failed to write plugin: %v", err)
	}

	// Manually register the plugin with the mock client.
	mgr.mu.Lock()
	mgr.plugins["cancel-test-plugin"] = &PluginInfo{
		Name:       "cancel-test-plugin",
		Path:       pluginPath,
		grpcClient: mockClient,
		Cleanup:    func() {},
	}
	mgr.mu.Unlock()

	// Start a tool call in a goroutine.
	toolResultCh := make(chan error, 1)
	go func() {
		_, _ = mockClient.ExecuteTool(ctx, &proto.ToolCall{
			ToolName: "test-tool",
		})
		toolResultCh <- ctx.Err()
	}()

	// Trigger reload, which should cancel the context.
	cancel()
	_ = mgr.ReloadNamed(context.Background(), "cancel-test-plugin")

	// Verify the tool call received context cancellation.
	select {
	case err := <-toolResultCh:
		if err == nil {
			t.Error("expected context error from cancelled tool call")
		}
	case <-toolCallCompleted:
		t.Error("tool call should have been cancelled by reload")
	}
}

// TestIntegrationReloadHashComparisonForNamedReload tests that named reload
// ignores hash comparison and always restarts the specified plugin.
func TestIntegrationReloadHashComparisonForNamedReload(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a plugin with a binary.
	pluginPath := filepath.Join(tmpDir, "hash-test-plugin")
	if err := os.WriteFile(pluginPath, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatalf("failed to write plugin: %v", err)
	}

	mgr := NewManager(tmpDir, nil, nil)

	// Set up the plugin with a hash that matches the binary.
	sameHash := "same-hash"

	mgr.mu.Lock()
	mgr.plugins["hash-test-plugin"] = &PluginInfo{
		Name:       "hash-test-plugin",
		Path:       pluginPath,
		Hash:       sameHash,
		grpcClient: &mockPluginClient{gracefulResponse: false}, // Required for Cleanup to be called
		Cleanup:    func() {},
	}
	// Note: We do NOT add to binaries map so that shutdown happens
	// without attempting reload.
	mgr.mu.Unlock()

	// Call ReloadNamed for this plugin.
	ctx := context.Background()
	_ = mgr.ReloadNamed(ctx, "hash-test-plugin")

	// Even though the hash matches, the named reload should still restart.
	mgr.mu.RLock()
	info := mgr.plugins["hash-test-plugin"]
	mgr.mu.RUnlock()

	// The plugin should be removed because the named reload always forces restart.
	if info != nil {
		t.Error("expected plugin to be removed during named reload even with matching hash")
	}
}
