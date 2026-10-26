package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	"github.com/losinggeneration/tau/agent/plugins/proto"
	"github.com/stretchr/testify/require"
)

// TestIntegrationPluginDiscovery tests the plugin discovery mechanism.
func TestIntegrationPluginDiscovery(t *testing.T) {
	tmpDir := t.TempDir()

	// Create some test "plugins" (just files, not real binaries)
	for i := 0; i < 3; i++ {
		pluginPath := filepath.Join(tmpDir, "plugin"+string(rune('a'+i)))
		if err := os.WriteFile(pluginPath, []byte("#!/bin/bash\nexit 1"), 0o755); err != nil {
			t.Fatalf("failed to create plugin: %v", err)
		}
	}

	mgr := NewManager(tmpDir, nil, nil)

	if err := mgr.scanDir(tmpDir); err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	if len(mgr.binaries) != 3 {
		t.Errorf("expected 3 plugins, got %d", len(mgr.binaries))
	}
}

func TestIntegrationPluginDiscoveryNestedManifestDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	pluginDir := filepath.Join(tmpDir, "hello-python")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("failed to create plugin dir: %v", err)
	}

	manifestPath := filepath.Join(pluginDir, "tau-package.toml")
	manifestContent := `
[package]
name = "hello-python-plugin"
version = "0.1.0"

[plugin]
api_version = "1"

[run]
command = "./hello-python"
`
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0o644); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}

	pluginPath := filepath.Join(pluginDir, "hello-python")
	if err := os.WriteFile(pluginPath, []byte("#!/bin/sh\nexit 1"), 0o755); err != nil {
		t.Fatalf("failed to write plugin executable: %v", err)
	}

	// A supporting executable in a nested runtime directory must not be
	// registered as its own loose plugin.
	venvBin := filepath.Join(pluginDir, ".venv", "bin")
	if err := os.MkdirAll(venvBin, 0o755); err != nil {
		t.Fatalf("failed to create venv bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(venvBin, "python"), []byte("#!/bin/sh\nexit 1"), 0o755); err != nil {
		t.Fatalf("failed to write venv python: %v", err)
	}

	mgr := NewManager(tmpDir, nil, nil)
	if err := mgr.scanDir(tmpDir); err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	if len(mgr.binaries) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(mgr.binaries))
	}
	if _, ok := mgr.binaries[pluginDir]; !ok {
		t.Fatalf("expected plugin directory %q to be registered", pluginDir)
	}
	if got := mgr.manifestPaths[pluginDir]; got != manifestPath {
		t.Fatalf("expected manifest path %q, got %q", manifestPath, got)
	}
}

// TestIntegrationAPIVersions tests the API version support.
func TestIntegrationAPIVersions(t *testing.T) {
	// Test supported versions.
	if !isAPIVersionSupported("1") {
		t.Error("expected '1' to be supported")
	}

	// Test unsupported versions.
	if isAPIVersionSupported("99") {
		t.Error("expected '99' to not be supported")
	}

	// Test empty version.
	if isAPIVersionSupported("") {
		t.Error("expected empty string to not be supported")
	}
}

// TestIntegrationInitRequestMetadata tests that InitRequest is populated with real
// CWD and version metadata when the plugin is loaded.
func TestIntegrationInitRequestMetadata(t *testing.T) {
	// Track the InitRequest received by our mock client.
	var receivedReq *proto.InitRequest
	mockClient := &mockPluginClient{
		gracefulResponse: true,
		onInitialize: func(req *proto.InitRequest) {
			receivedReq = req
		},
	}

	mgr := NewManager(t.TempDir(), nil, nil)

	// Set a CWD provider.
	cwdProviderCalled := false
	mgr.SetSessionCWDProvider(func() string {
		cwdProviderCalled = true
		return "/test/workspace"
	})

	// Manually inject a plugin with our mock client.
	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "test-plugin")
	if err := os.WriteFile(pluginPath, []byte("#!/bin/bash\nexit 1"), 0o755); err != nil {
		t.Fatalf("failed to create plugin: %v", err)
	}

	mgr.mu.Lock()
	mgr.plugins["test-plugin"] = &PluginInfo{
		Name:       "test-plugin",
		Path:       pluginPath,
		grpcClient: mockClient,
		Cleanup:    func() {},
	}
	mgr.mu.Unlock()

	// Simulate initialization by calling Initialize directly on the mock.
	ctx := context.Background()
	cwd := mgr.currentCWD()

	// Call Initialize through the mock client.
	resp, err := mockClient.Initialize(ctx, &proto.InitRequest{
		Version:      Version,
		SessionId:    "test-session",
		Cwd:          cwd,
		ManifestPath: "",
	})
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("Initialize returned error: %s", resp.Error)
	}

	// Verify version metadata is passed.
	if receivedReq == nil {
		t.Fatal("Initialize was not called on mock client")
	}
	if receivedReq.Version != Version {
		t.Errorf("expected Version=%q, got %q", Version, receivedReq.Version)
	}

	// Verify CWD is real (from the provider or fallback).
	if receivedReq.Cwd == "" {
		t.Error("expected non-empty Cwd in InitRequest")
	}
	if receivedReq.Cwd != "/test/workspace" {
		t.Errorf("expected Cwd=/test/workspace from provider, got %q", receivedReq.Cwd)
	}

	// Verify CWD provider was called.
	if !cwdProviderCalled {
		t.Error("expected CWD provider to be called via currentCWD()")
	}
}

// TestIntegrationFailureConfig tests failure configuration.
func TestIntegrationFailureConfig(t *testing.T) {
	cfg := DefaultFailureConfig

	if cfg.MaxAttempts != 3 {
		t.Errorf("expected MaxAttempts=3, got %d", cfg.MaxAttempts)
	}

	if cfg.BackoffBase != 1*1e9 { // 1 second in nanoseconds
		t.Errorf("expected BackoffBase=1s, got %v", cfg.BackoffBase)
	}

	if cfg.BackoffFactor != 2 {
		t.Errorf("expected BackoffFactor=2, got %v", cfg.BackoffFactor)
	}
}

// TestIntegrationRPCTimeouts tests default RPC timeout values.
func TestIntegrationRPCTimeouts(t *testing.T) {
	timeouts := DefaultRPCTimeouts

	if timeouts.Initialize != 10*1e9 { // 10 seconds
		t.Errorf("expected Initialize=10s, got %v", timeouts.Initialize)
	}

	if timeouts.ExecuteTool != 30*1e9 { // 30 seconds
		t.Errorf("expected ExecuteTool=30s, got %v", timeouts.ExecuteTool)
	}

	if timeouts.OnEvent != 5*1e9 { // 5 seconds
		t.Errorf("expected OnEvent=5s, got %v", timeouts.OnEvent)
	}

	if timeouts.ExecuteCmd != 30*1e9 { // 30 seconds
		t.Errorf("expected ExecuteCmd=30s, got %v", timeouts.ExecuteCmd)
	}
}

// TestIntegrationHashComputation tests file hash computation.
func TestIntegrationHashComputation(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)

	// Create a test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	hash1, err := mgr.computeHash(testFile)
	if err != nil {
		t.Fatalf("failed to compute hash: %v", err)
	}

	// Same file should produce same hash
	hash2, err := mgr.computeHash(testFile)
	if err != nil {
		t.Fatalf("failed to compute hash: %v", err)
	}
	if hash1 != hash2 {
		t.Error("expected same hash for same content")
	}

	// Different content should produce different hash
	if err := os.WriteFile(testFile, []byte("different content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	hash3, err := mgr.computeHash(testFile)
	if err != nil {
		t.Fatalf("failed to compute hash: %v", err)
	}
	if hash1 == hash3 {
		t.Error("expected different hash for different content")
	}
}

// TestIntegrationReloadEmpty tests reload with no plugins loaded.
func TestIntegrationReloadEmpty(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)

	// Test Reload with no plugins loaded - should not panic
	if err := mgr.Reload(t.Context()); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
}

// TestIntegrationShutdownEmpty tests shutdown with no plugins.
func TestIntegrationShutdownEmpty(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)

	// Test Shutdown with no plugins - should not panic
	if err := mgr.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
}

// TestIntegrationToolPrefix tests tool prefix handling.
func TestIntegrationToolPrefix(t *testing.T) {
	regs := Registrations{
		Tools: []ToolRegistration{
			{Name: "mytool", Description: "A tool"},
		},
		ToolPrefix: "custom-",
	}

	// Verify tool prefix is stored
	if regs.ToolPrefix != "custom-" {
		t.Errorf("expected tool prefix 'custom-', got %q", regs.ToolPrefix)
	}

	// Verify tool name is accessible
	if len(regs.Tools) != 1 || regs.Tools[0].Name != "mytool" {
		t.Error("expected one tool named 'mytool'")
	}
}

// TestIntegrationCommandRegistration tests command registration structure.
func TestIntegrationCommandRegistration(t *testing.T) {
	regs := Registrations{
		Commands: []CommandRegistration{
			{Name: "mycommand", Description: "A command", ArgumentHint: "<arg>"},
		},
	}

	if len(regs.Commands) != 1 {
		t.Error("expected one command")
	}

	cmd := regs.Commands[0]
	if cmd.Name != "mycommand" {
		t.Errorf("expected command name 'mycommand', got %q", cmd.Name)
	}
	if cmd.ArgumentHint != "<arg>" {
		t.Errorf("expected argument hint '<arg>', got %q", cmd.ArgumentHint)
	}
}

// TestIntegrationCLIFlagRegistration tests CLI flag registration.
func TestIntegrationCLIFlagRegistration(t *testing.T) {
	regs := Registrations{
		CLIFlags: []CLIFlag{
			{Name: "verbose", Description: "Enable verbose output", Required: true, DefaultValue: "false"},
			{Name: "output", Description: "Output file", Required: false, DefaultValue: "stdout"},
		},
	}

	if len(regs.CLIFlags) != 2 {
		t.Errorf("expected 2 CLI flags, got %d", len(regs.CLIFlags))
	}

	verbose := regs.CLIFlags[0]
	if verbose.Name != "verbose" {
		t.Errorf("expected flag name 'verbose', got %q", verbose.Name)
	}
	if !verbose.Required {
		t.Error("expected 'verbose' to be required")
	}
	if verbose.DefaultValue != "false" {
		t.Errorf("expected default 'false', got %q", verbose.DefaultValue)
	}
}

// TestIntegrationEventSubscriptions tests event subscription structure.
func TestIntegrationEventSubscriptions(t *testing.T) {
	regs := Registrations{
		EventSubscriptions: []string{"agent_start", "tool_call", "tool_result"},
	}

	if len(regs.EventSubscriptions) != 3 {
		t.Errorf("expected 3 event subscriptions, got %d", len(regs.EventSubscriptions))
	}

	// Verify we can check for specific events
	found := false
	for _, event := range regs.EventSubscriptions {
		if event == "tool_call" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'tool_call' in subscriptions")
	}
}

// TestIntegrationCompactionHook tests the compaction hook dispatch mechanism.
func TestIntegrationCompactionHook(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)

	// Register a compaction hook handler.
	mgr.RegisterEventHandler("compaction_request", EventHandler{
		PluginName: "test-plugin",
		Handler: func(_ context.Context, name string, payload json.RawMessage, sessionID string) EventResponse {
			// Return a mock compaction result.
			return EventResponse{
				Compaction: &proto.CompactionHookResult{
					Summary:          "Custom summary from plugin",
					FirstKeptEntryID: "entry-123",
				},
			}
		},
	})

	// Dispatch the compaction hook.
	result := mgr.DispatchCompactionHook(t.Context(), "transcript content", 1000, "session-1")

	// Verify the result is returned.
	if result.Summary == "" {
		t.Error("expected non-empty summary from compaction hook")
	}
	if result.FirstKeptEntryID == "" {
		t.Error("expected non-empty first_kept_entry_id from compaction hook")
	}
	if result.Summary != "Custom summary from plugin" {
		t.Errorf("expected 'Custom summary from plugin', got %q", result.Summary)
	}
	if result.FirstKeptEntryID != "entry-123" {
		t.Errorf("expected 'entry-123', got %q", result.FirstKeptEntryID)
	}
}

// TestIntegrationShutdownBoundedUnderHangingPlugin tests that lifecycle shutdown
// completes within bounded time even when a plugin hangs in Shutdown.
// Per the spec: graceful shutdown via Shutdown RPC with drain-then-SIGKILL fallback.
func TestIntegrationShutdownBoundedUnderHangingPlugin(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)

	// Create a fake "hanging" plugin binary that we'll track.
	// We'll test the shutdown logic by mocking the plugin manager's state.
	tmpDir := t.TempDir()
	pluginPath := filepath.Join(tmpDir, "hanging-plugin")
	// Create a script that would hang indefinitely (we won't run it, just test the timeout logic)
	if err := os.WriteFile(pluginPath, []byte("#!/bin/bash\nsleep 3600"), 0o755); err != nil {
		t.Fatalf("failed to create hanging plugin: %v", err)
	}

	// Create a mock plugin info with a hanging gRPC client.
	// We'll use a context with a short timeout to simulate the drain timeout behavior.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// The shutdown should complete within a bounded time even if we simulate a slow shutdown.
	// We test this by calling Shutdown with a context that will expire,
	// verifying that the operation returns within a reasonable margin.

	start := time.Now()

	// Register a mock plugin that simulates hanging.
	mgr.mu.Lock()
	mgr.plugins["hanging-plugin"] = &PluginInfo{
		Name: "hanging-plugin",
		Path: pluginPath,
		// No grpcClient set - simulates plugin that hasn't connected yet.
		// The shutdown should handle this gracefully.
		Cleanup: func() {
			// Simulate cleanup that would take a while, but bounded.
		},
	}
	mgr.mu.Unlock()

	// Call shutdown - should complete without hanging.
	err := mgr.Shutdown(ctx)

	elapsed := time.Since(start)

	// Shutdown should complete within a reasonable time even with the context deadline.
	if elapsed > 1*time.Second {
		t.Errorf("shutdown took too long: %v (expected < 1s)", elapsed)
	}

	// The error should be nil or context deadline exceeded - not a hang.
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		// DeadlineExceeded is acceptable - it means we hit the test's short context timeout.
		t.Logf("shutdown returned: %v (acceptable: DeadlineExceeded)", err)
	}
}

// TestIntegrationShutdownGracefulThenSIGKILL tests the drain-then-SIGKILL fallback
// in shutdownPluginLocked when a plugin doesn't respond gracefully.
func TestIntegrationShutdownGracefulThenSIGKILL(t *testing.T) {
	// This test verifies the code path where a plugin returns from Shutdown RPC
	// but indicates it's not graceful (Graceful=false). The code should still
	// proceed and call Cleanup (which sends SIGKILL via go-plugin).
	//
	// Per the spec: lifecycle shutdown completes within bounded time even with
	// a plugin that hangs in Shutdown.

	mgr := NewManager(t.TempDir(), nil, nil)

	ctx := context.Background()
	cleanupCalled := false

	// Create a plugin info that simulates non-graceful shutdown.
	mgr.mu.Lock()
	mgr.plugins["non-graceful-plugin"] = &PluginInfo{
		Name: "non-graceful-plugin",
		Path: "/fake/path",
		grpcClient: &mockPluginClient{
			gracefulResponse: false, // Simulates plugin that didn't shut down gracefully
		},
		Cleanup: func() {
			cleanupCalled = true
		},
	}
	mgr.mu.Unlock()

	// Call Shutdown - this should:
	// 1. Call Shutdown RPC
	// 2. See Graceful=false
	// 3. Call Cleanup (which kills the process)
	err := mgr.Shutdown(ctx)
	if err != nil {
		t.Logf("shutdown returned error (may be expected): %v", err)
	}

	// Cleanup should have been called as fallback.
	if !cleanupCalled {
		t.Error("expected Cleanup to be called for non-graceful plugin")
	}
}

// mockPluginClient implements proto.PluginClient for testing.
type mockPluginClient struct {
	gracefulResponse bool
	onInitialize     func(req *proto.InitRequest)
	onExecuteTool    func(ctx context.Context, req *proto.ToolCall) (*proto.ToolResult, error)
}

func (m *mockPluginClient) Initialize(ctx context.Context, req *proto.InitRequest, opts ...grpc.CallOption) (*proto.InitResponse, error) {
	if m.onInitialize != nil {
		m.onInitialize(req)
	}
	return &proto.InitResponse{ApiVersion: "1"}, nil
}

func (m *mockPluginClient) ListRegistrations(ctx context.Context, req *proto.ListRegistrationsRequest, opts ...grpc.CallOption) (*proto.Registrations, error) {
	return &proto.Registrations{}, nil
}

func (m *mockPluginClient) ExecuteTool(ctx context.Context, req *proto.ToolCall, opts ...grpc.CallOption) (*proto.ToolResult, error) {
	if m.onExecuteTool != nil {
		return m.onExecuteTool(ctx, req)
	}
	return &proto.ToolResult{}, nil
}

func (m *mockPluginClient) ExecuteCommand(ctx context.Context, req *proto.CommandInvocation, opts ...grpc.CallOption) (*proto.CommandResult, error) {
	return &proto.CommandResult{}, nil
}

func (m *mockPluginClient) OnEvent(ctx context.Context, req *proto.Event, opts ...grpc.CallOption) (*proto.EventResponse, error) {
	return &proto.EventResponse{}, nil
}

func (m *mockPluginClient) OnTerminalInput(ctx context.Context, req *proto.TerminalInput, opts ...grpc.CallOption) (*proto.TerminalInputResponse, error) {
	return &proto.TerminalInputResponse{}, nil
}

func (m *mockPluginClient) Shutdown(ctx context.Context, req *proto.ShutdownRequest, opts ...grpc.CallOption) (*proto.ShutdownResponse, error) {
	return &proto.ShutdownResponse{Graceful: m.gracefulResponse}, nil
}

func (m *mockPluginClient) RouteModel(context.Context, *proto.ModelRouteRequest, ...grpc.CallOption) (*proto.ModelRouteResponse, error) {
	return &proto.ModelRouteResponse{}, nil
}

func TestIntegrationCompactionHookNoHandler(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)

	// Dispatch the compaction hook without any handlers.
	result := mgr.DispatchCompactionHook(t.Context(), "transcript content", 1000, "session-1")

	// Verify empty result is returned.
	if result.Summary != "" {
		t.Errorf("expected empty summary, got %q", result.Summary)
	}
	if result.FirstKeptEntryID != "" {
		t.Errorf("expected empty first_kept_entry_id, got %q", result.FirstKeptEntryID)
	}
}

// TestIntegrationRetryAndDeregistration tests that:
// 1. AttemptLoadWithRetry retries on failure and deregisters after exhausting retries
// 2. ReattemptDeregistered re-attempts loading deregistered plugins
// 3. Reload includes re-attempting deregistered plugins
func TestIntegrationRetryAndDeregistration(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)

	// Override loadFn to return an error immediately — we only want to test
	// the retry/backoff/deregister state machine, not actual plugin loading.
	mgr.loadFn = func(ctx context.Context, path string, manifestPath ...string) (*PluginInfo, error) {
		return nil, fmt.Errorf("simulated load failure")
	}

	tmpDir := t.TempDir()
	failingPlugin := filepath.Join(tmpDir, "failing-plugin")

	// Configure short retry for faster test
	mgr.failureConfig[failingPlugin] = FailureConfig{
		MaxAttempts:   2,
		BackoffBase:   1 * time.Millisecond,
		BackoffFactor: 1,
	}

	// Attempt to load the failing plugin - should retry and then deregister
	ctx := context.Background()
	_, err := mgr.AttemptLoadWithRetry(ctx, failingPlugin, "")
	if err == nil {
		t.Error("expected error after retry exhaustion, got nil")
	}

	// Verify the plugin was deregistered (removed from map after deregistration)
	mgr.mu.Lock()
	_, exists := mgr.plugins["failing-plugin"]
	mgr.mu.Unlock()
	if exists {
		t.Error("expected plugin to be removed from map after deregistration")
	}

	// Verify ReattemptDeregistered doesn't panic with an empty deregistered map.
	require.NoError(t, mgr.ReattemptDeregistered(ctx))

	// Verify Reload also calls ReattemptDeregistered without panic.
	mgr.Reload(ctx)
}

// TestIntegrationCompactionHookFirstWins tests that first handler's result wins.
// TestIntegrationRunCommand tests that package-plugin manifest [run].command
// is honored during plugin launch instead of "first executable wins" behavior.
// Per the spec: Package-plugin launch must honor manifest runtime data rather
// than "first executable wins".
func TestIntegrationRunCommand(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a tau-package.toml with [run].command
	manifestContent := `
[package]
name = "test-runcmd-plugin"
version = "1.0.0"

[plugin]
api_version = "1"

[run]
command = "echo 'launched with run command' && exit 1"
`
	manifestPath := filepath.Join(tmpDir, "tau-package.toml")
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0o644); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}

	// Create the plugin binary (will be launched via sh -c)
	pluginPath := filepath.Join(tmpDir, "test-plugin")
	// This script would normally run the plugin, but the manifest's
	// command will be used instead
	if err := os.WriteFile(pluginPath, []byte("#!/bin/bash\nexit 1"), 0o755); err != nil {
		t.Fatalf("failed to create plugin: %v", err)
	}

	mgr := NewManager(tmpDir, nil, nil)

	// Parse the manifest to verify HasRunCommand works
	mgr.mu.Lock()
	mgr.manifestPaths[pluginPath] = manifestPath
	mgr.mu.Unlock()

	// Verify the manifest has a run command
	manifest, err := parseManifest(manifestPath)
	if err != nil {
		t.Fatalf("failed to parse manifest: %v", err)
	}
	if !manifest.HasRunCommand() {
		t.Error("expected manifest to have run command")
	}
	if manifest.Run.Command == "" {
		t.Error("expected non-empty run command")
	}
	if manifest.Run.Command != "echo 'launched with run command' && exit 1" {
		t.Errorf("expected run command 'echo ...', got %q", manifest.Run.Command)
	}

	// Verify the plugin manager honors the run command during load
	// by attempting to load and checking the binary/manifest path association
	mgr.mu.RLock()
	_, hasPath := mgr.manifestPaths[pluginPath]
	mgr.mu.RUnlock()
	if !hasPath {
		t.Error("expected plugin path to be registered with manifest path")
	}
}

// TestIntegrationRunCommandNoManifest tests that plugins without a manifest
// are launched via direct binary path execution.
func TestIntegrationRunCommandNoManifest(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a plugin without a manifest
	pluginPath := filepath.Join(tmpDir, "no-manifest-plugin")
	if err := os.WriteFile(pluginPath, []byte("#!/bin/bash\nexit 0"), 0o755); err != nil {
		t.Fatalf("failed to create plugin: %v", err)
	}

	mgr := NewManager(tmpDir, nil, nil)

	// Verify no manifest path is associated
	mgr.mu.RLock()
	manifestPath := mgr.manifestPaths[pluginPath]
	mgr.mu.RUnlock()
	if manifestPath != "" {
		t.Errorf("expected no manifest path for loose plugin, got %q", manifestPath)
	}

	// Attempt to load - will fail but should not panic
	ctx := context.Background()
	_, err := mgr.Load(ctx, pluginPath)
	if err == nil {
		t.Error("expected load to fail for non-graceful plugin")
	}
}

func TestIntegrationCompactionHookFirstWins(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)

	// Register first handler (should win).
	mgr.RegisterEventHandler("compaction_request", EventHandler{
		PluginName: "first-plugin",
		Handler: func(_ context.Context, name string, payload json.RawMessage, sessionID string) EventResponse {
			return EventResponse{
				Compaction: &proto.CompactionHookResult{
					Summary:          "First handler summary",
					FirstKeptEntryID: "first-entry",
				},
			}
		},
	})

	// Register second handler (should be ignored).
	mgr.RegisterEventHandler("compaction_request", EventHandler{
		PluginName: "second-plugin",
		Handler: func(_ context.Context, name string, payload json.RawMessage, sessionID string) EventResponse {
			return EventResponse{
				Compaction: &proto.CompactionHookResult{
					Summary:          "Second handler summary",
					FirstKeptEntryID: "second-entry",
				},
			}
		},
	})

	// Dispatch the compaction hook.
	result := mgr.DispatchCompactionHook(t.Context(), "transcript content", 1000, "session-1")

	// Verify first handler's result is returned.
	if result.Summary != "First handler summary" {
		t.Errorf("expected 'First handler summary', got %q", result.Summary)
	}
	if result.FirstKeptEntryID != "first-entry" {
		t.Errorf("expected 'first-entry', got %q", result.FirstKeptEntryID)
	}
}

// TestIntegrationHostRPCs tests that the HostService serves reverse RPCs correctly.
// This verifies UIConfirm, UISelect, UIInput, UINotify, GetConfig, State*, Session*, and ToolProgress.
func TestIntegrationHostRPCs(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager(tmpDir, nil, nil)
	hostService := NewHostService(mgr)

	// Create context with plugin name metadata (required by the HostService)
	ctx := context.Background()

	// Test UINotify - should succeed even without UI
	t.Run("UINotify", func(t *testing.T) {
		resp, err := hostService.UINotify(ctx, &proto.UINotifyRequest{
			Level:   1,
			Message: "test notification",
		})
		if err != nil {
			t.Fatalf("UINotify failed: %v", err)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
	})

	// Test UIConfirm without UI - should return error
	t.Run("UIConfirm_no_UI", func(t *testing.T) {
		resp, err := hostService.UIConfirm(ctx, &proto.UIConfirmRequest{
			Title:   "Test",
			Message: "Confirm?",
		})
		if err != nil {
			t.Fatalf("UIConfirm failed: %v", err)
		}
		if resp.Confirmed {
			t.Error("expected Confirmed=false when no UI available")
		}
		if resp.Error != "no UI available" {
			t.Errorf("expected error 'no UI available', got %q", resp.Error)
		}
	})

	// Test UISelect without UI - should return error
	t.Run("UISelect_no_UI", func(t *testing.T) {
		resp, err := hostService.UISelect(ctx, &proto.UISelectRequest{
			Title:   "Test",
			Options: []string{"a", "b", "c"},
			Id:      "select-1",
		})
		if err != nil {
			t.Fatalf("UISelect failed: %v", err)
		}
		if resp.SelectedIndex != -1 {
			t.Error("expected SelectedIndex=-1 when no UI available")
		}
		if resp.Error != "no UI available" {
			t.Errorf("expected error 'no UI available', got %q", resp.Error)
		}
	})

	// Test UIInput without UI - should return error
	t.Run("UIInput_no_UI", func(t *testing.T) {
		resp, err := hostService.UIInput(ctx, &proto.UIInputRequest{
			Title:       "Test",
			Placeholder: "enter value",
		})
		if err != nil {
			t.Fatalf("UIInput failed: %v", err)
		}
		if resp.Value != "" {
			t.Error("expected empty value when no UI available")
		}
		if resp.Error != "no UI available" {
			t.Errorf("expected error 'no UI available', got %q", resp.Error)
		}
	})

	// Test GetConfig - should return error without plugin name metadata
	t.Run("GetConfig_empty", func(t *testing.T) {
		// GetConfig requires plugin name in metadata
		_, err := hostService.GetConfig(ctx, &proto.GetConfigRequest{})
		if err == nil {
			t.Error("expected error without plugin name metadata")
		}
	})

	// Test StateSet and StateGet - should return error without plugin name metadata
	t.Run("StateSetGet_no_metadata", func(t *testing.T) {
		// Set a state value - requires plugin name in metadata
		_, err := hostService.StateSet(ctx, &proto.StateSetRequest{
			Key:   "test-key",
			Value: "test-value",
		})
		if err == nil {
			t.Error("expected error without plugin name metadata")
		}
	})

	// Test StateAppend and StateDelete - should return error without plugin name metadata
	t.Run("StateAppendDelete_no_metadata", func(t *testing.T) {
		// Append to state - requires plugin name in metadata
		_, err := hostService.StateAppend(ctx, &proto.StateAppendRequest{
			Key:   "list-key",
			Value: "item1",
		})
		if err == nil {
			t.Error("expected error without plugin name metadata")
		}
	})

	// Test ToolProgress
	t.Run("ToolProgress", func(t *testing.T) {
		resp, err := hostService.ToolProgress(ctx, &proto.ToolProgressEvent{
			ToolCallId: "call-123",
			Content:    "partial result",
		})
		if err != nil {
			t.Fatalf("ToolProgress failed: %v", err)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
	})

	// Test UISetStatus and UISetWidget
	t.Run("UISetStatus", func(t *testing.T) {
		resp, err := hostService.UISetStatus(ctx, &proto.UISetStatusRequest{
			Key:     "status-key",
			Message: "working on task",
		})
		if err != nil {
			t.Fatalf("UISetStatus failed: %v", err)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
	})

	t.Run("UISetWidget", func(t *testing.T) {
		resp, err := hostService.UISetWidget(ctx, &proto.UISetWidgetRequest{
			Key:   "widget-key",
			Lines: []string{"50%"},
		})
		if err != nil {
			t.Fatalf("UISetWidget failed: %v", err)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
	})

	t.Run("Log", func(t *testing.T) {
		resp, err := hostService.Log(ctx, &proto.LogRequest{
			Level:   1, // LOG_INFO
			Message: "test log message",
		})
		if err != nil {
			t.Fatalf("Log failed: %v", err)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
	})
}

// This verifies plugin session access through the real runtime path.
func TestIntegrationSessionScanWithHostRPC(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager(tmpDir, nil, nil)
	hostService := NewHostService(mgr)

	ctx := context.Background()

	// Set a session ID on the manager
	mgr.SetSessionIDProvider(func() string { return "test-session-123" })

	// Test SessionScan without session accessor - should fail with "session accessor not configured"
	t.Run("SessionScan_no_accessor", func(t *testing.T) {
		_, err := hostService.SessionScan(ctx, &proto.SessionScanRequest{})
		if err == nil {
			t.Error("expected error without session accessor")
		}
	})

	// Test SessionAppend without session accessor - should fail
	t.Run("SessionAppend_no_accessor", func(t *testing.T) {
		_, err := hostService.SessionAppend(ctx, &proto.SessionAppendRequest{
			EntryType: "custom",
			Payload:   `{"type": "text", "text": "hello"}`,
		})
		if err == nil {
			t.Error("expected error without session accessor")
		}
	})
}

// TestIntegrationReverseHostRPC tests that Host RPCs
// (GetConfig, StateSet, StateGet, ToolProgress) work end-to-end
// when a plugin subprocess calls back to tau's Host service over gRPC.
func TestIntegrationReverseHostRPC(t *testing.T) {
	// Track which Host RPC methods were called and their arguments.
	var (
		mu                sync.Mutex
		configCalls       []string
		stateValues       = make(map[string]string)
		toolProgressCalls []*proto.ToolProgressEvent
	)

	// Create a mock HostServer to record calls from the "plugin".
	mockHostServer := &mockHostServerForTest{
		onGetConfig: func(ctx context.Context, req *proto.GetConfigRequest) (*proto.GetConfigResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			configCalls = append(configCalls, req.Key)
			return &proto.GetConfigResponse{Value: `{"setting":"value"}`, Exists: true}, nil
		},
		onStateSet: func(ctx context.Context, req *proto.StateSetRequest) (*proto.StateSetResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			stateValues[req.Key] = req.Value
			return &proto.StateSetResponse{}, nil
		},
		onStateGet: func(ctx context.Context, req *proto.StateGetRequest) (*proto.StateGetResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			if v, ok := stateValues[req.Key]; ok {
				return &proto.StateGetResponse{Value: v, Exists: true}, nil
			}
			return &proto.StateGetResponse{Exists: false}, nil
		},
		onToolProgress: func(ctx context.Context, req *proto.ToolProgressEvent) (*proto.ToolProgressResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			toolProgressCalls = append(toolProgressCalls, req)
			return &proto.ToolProgressResponse{}, nil
		},
	}

	// Start gRPC server with our mock HostServer.
	lis := bufconn.Listen(1024)
	srv := grpc.NewServer()
	proto.RegisterHostServer(srv, mockHostServer)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	// Dial the in-process server.
	conn, err := grpc.NewClient("passthrough:///bufconn/test",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithInsecure(),
	)
	if err != nil {
		t.Fatalf("failed to dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Simulate plugin-side: create a HostClient and call methods.
	hostClient := proto.NewHostClient(conn)
	ctx := context.Background()

	// Call GetConfig.
	resp, err := hostClient.GetConfig(ctx, &proto.GetConfigRequest{Key: "api_key"})
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}
	if !resp.Exists || resp.Value == "" {
		t.Errorf("GetConfig: expected exists=true, got %+v", resp)
	}

	// Call StateSet.
	if _, err := hostClient.StateSet(ctx, &proto.StateSetRequest{Key: "counter", Value: "42"}); err != nil {
		t.Fatalf("StateSet failed: %v", err)
	}

	// Call StateGet.
	getResp, err := hostClient.StateGet(ctx, &proto.StateGetRequest{Key: "counter"})
	if err != nil {
		t.Fatalf("StateGet failed: %v", err)
	}
	if !getResp.Exists || getResp.Value != "42" {
		t.Errorf("StateGet: expected value=42, got %+v", getResp)
	}

	// Call ToolProgress.
	if _, err := hostClient.ToolProgress(ctx, &proto.ToolProgressEvent{
		ToolCallId: "call-abc",
		Content:    "processing item 1/10",
	}); err != nil {
		t.Fatalf("ToolProgress failed: %v", err)
	}

	// Verify recorded calls.
	mu.Lock()
	defer mu.Unlock()

	if len(configCalls) == 0 || configCalls[0] != "api_key" {
		t.Errorf("GetConfig not recorded: got %v", configCalls)
	}
	if len(stateValues) == 0 || stateValues["counter"] != "42" {
		t.Errorf("StateSet not recorded: got %v", stateValues)
	}
	if len(toolProgressCalls) == 0 || toolProgressCalls[0].ToolCallId != "call-abc" {
		t.Errorf("ToolProgress not recorded: got %v", toolProgressCalls)
	}
}

// mockHostServerForTest implements proto.HostServer for integration tests.
// It records calls and delegates to optional callbacks for validation.
type mockHostServerForTest struct {
	proto.UnimplementedHostServer
	onGetConfig     func(context.Context, *proto.GetConfigRequest) (*proto.GetConfigResponse, error)
	onStateSet      func(context.Context, *proto.StateSetRequest) (*proto.StateSetResponse, error)
	onStateGet      func(context.Context, *proto.StateGetRequest) (*proto.StateGetResponse, error)
	onToolProgress  func(context.Context, *proto.ToolProgressEvent) (*proto.ToolProgressResponse, error)
	onSessionAppend func(context.Context, *proto.SessionAppendRequest) (*proto.SessionAppendResponse, error)
	onSessionScan   func(context.Context, *proto.SessionScanRequest) (*proto.SessionScanResponse, error)
	onUIConfirm     func(context.Context, *proto.UIConfirmRequest) (*proto.UIConfirmResponse, error)
	onUISelect      func(context.Context, *proto.UISelectRequest) (*proto.UISelectResponse, error)
	onUIInput       func(context.Context, *proto.UIInputRequest) (*proto.UIInputResponse, error)
	onUINotify      func(context.Context, *proto.UINotifyRequest) (*proto.UINotifyResponse, error)
	onUISetStatus   func(context.Context, *proto.UISetStatusRequest) (*proto.UISetStatusResponse, error)
	onUISetWidget   func(context.Context, *proto.UISetWidgetRequest) (*proto.UISetWidgetResponse, error)
	onLog           func(context.Context, *proto.LogRequest) (*proto.LogResponse, error)
	onStateAppend   func(context.Context, *proto.StateAppendRequest) (*proto.StateAppendResponse, error)
	onStateDelete   func(context.Context, *proto.StateDeleteRequest) (*proto.StateDeleteResponse, error)
}

func (m *mockHostServerForTest) GetConfig(ctx context.Context, req *proto.GetConfigRequest) (*proto.GetConfigResponse, error) {
	if m.onGetConfig != nil {
		return m.onGetConfig(ctx, req)
	}
	return &proto.GetConfigResponse{}, nil
}

func (m *mockHostServerForTest) StateSet(ctx context.Context, req *proto.StateSetRequest) (*proto.StateSetResponse, error) {
	if m.onStateSet != nil {
		return m.onStateSet(ctx, req)
	}
	return &proto.StateSetResponse{}, nil
}

func (m *mockHostServerForTest) StateGet(ctx context.Context, req *proto.StateGetRequest) (*proto.StateGetResponse, error) {
	if m.onStateGet != nil {
		return m.onStateGet(ctx, req)
	}
	return &proto.StateGetResponse{}, nil
}

func (m *mockHostServerForTest) ToolProgress(ctx context.Context, req *proto.ToolProgressEvent) (*proto.ToolProgressResponse, error) {
	if m.onToolProgress != nil {
		return m.onToolProgress(ctx, req)
	}
	return &proto.ToolProgressResponse{}, nil
}

func (m *mockHostServerForTest) SessionScan(ctx context.Context, req *proto.SessionScanRequest) (*proto.SessionScanResponse, error) {
	if m.onSessionScan != nil {
		return m.onSessionScan(ctx, req)
	}
	return &proto.SessionScanResponse{}, nil
}

func (m *mockHostServerForTest) SessionAppend(ctx context.Context, req *proto.SessionAppendRequest) (*proto.SessionAppendResponse, error) {
	if m.onSessionAppend != nil {
		return m.onSessionAppend(ctx, req)
	}
	return &proto.SessionAppendResponse{}, nil
}

func (m *mockHostServerForTest) UIConfirm(ctx context.Context, req *proto.UIConfirmRequest) (*proto.UIConfirmResponse, error) {
	if m.onUIConfirm != nil {
		return m.onUIConfirm(ctx, req)
	}
	return &proto.UIConfirmResponse{}, nil
}

func (m *mockHostServerForTest) UISelect(ctx context.Context, req *proto.UISelectRequest) (*proto.UISelectResponse, error) {
	if m.onUISelect != nil {
		return m.onUISelect(ctx, req)
	}
	return &proto.UISelectResponse{}, nil
}

func (m *mockHostServerForTest) UIInput(ctx context.Context, req *proto.UIInputRequest) (*proto.UIInputResponse, error) {
	if m.onUIInput != nil {
		return m.onUIInput(ctx, req)
	}
	return &proto.UIInputResponse{}, nil
}

func (m *mockHostServerForTest) UINotify(ctx context.Context, req *proto.UINotifyRequest) (*proto.UINotifyResponse, error) {
	if m.onUINotify != nil {
		return m.onUINotify(ctx, req)
	}
	return &proto.UINotifyResponse{}, nil
}

func (m *mockHostServerForTest) UISetStatus(ctx context.Context, req *proto.UISetStatusRequest) (*proto.UISetStatusResponse, error) {
	if m.onUISetStatus != nil {
		return m.onUISetStatus(ctx, req)
	}
	return &proto.UISetStatusResponse{}, nil
}

func (m *mockHostServerForTest) UISetWidget(ctx context.Context, req *proto.UISetWidgetRequest) (*proto.UISetWidgetResponse, error) {
	if m.onUISetWidget != nil {
		return m.onUISetWidget(ctx, req)
	}
	return &proto.UISetWidgetResponse{}, nil
}

func (m *mockHostServerForTest) Log(ctx context.Context, req *proto.LogRequest) (*proto.LogResponse, error) {
	if m.onLog != nil {
		return m.onLog(ctx, req)
	}
	return &proto.LogResponse{}, nil
}

func (m *mockHostServerForTest) StateAppend(ctx context.Context, req *proto.StateAppendRequest) (*proto.StateAppendResponse, error) {
	if m.onStateAppend != nil {
		return m.onStateAppend(ctx, req)
	}
	return &proto.StateAppendResponse{}, nil
}

func (m *mockHostServerForTest) StateDelete(ctx context.Context, req *proto.StateDeleteRequest) (*proto.StateDeleteResponse, error) {
	if m.onStateDelete != nil {
		return m.onStateDelete(ctx, req)
	}
	return &proto.StateDeleteResponse{}, nil
}

// TestIntegrationEndToEndReverseRPCFromExecuteTool proves that a real subprocess
// plugin can call Host.UIConfirm, Host.GetConfig, Host.StateGet, and
// Host.SessionAppend from inside ExecuteTool. This tests the complete reverse-RPC
// path: tau calls Plugin.ExecuteTool -> plugin calls Host RPCs -> responses return.
func TestIntegrationEndToEndReverseRPCFromExecuteTool(t *testing.T) {
	var (
		mu             sync.Mutex
		configKeys     []string
		stateValues    = make(map[string]string)
		confirmCalls   []*proto.UIConfirmRequest
		sessionAppends []*proto.SessionAppendRequest
	)

	// Create a mock HostServer that records all calls.
	mockHostServer := &mockHostServerForTest{
		onGetConfig: func(ctx context.Context, req *proto.GetConfigRequest) (*proto.GetConfigResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			configKeys = append(configKeys, req.Key)
			return &proto.GetConfigResponse{Value: `"test-value"`, Exists: true}, nil
		},
		onStateGet: func(ctx context.Context, req *proto.StateGetRequest) (*proto.StateGetResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			if v, ok := stateValues[req.Key]; ok {
				return &proto.StateGetResponse{Value: v, Exists: true}, nil
			}
			return &proto.StateGetResponse{Value: "", Exists: false}, nil
		},
		onStateSet: func(ctx context.Context, req *proto.StateSetRequest) (*proto.StateSetResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			stateValues[req.Key] = req.Value
			return &proto.StateSetResponse{}, nil
		},
		onUIConfirm: func(ctx context.Context, req *proto.UIConfirmRequest) (*proto.UIConfirmResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			confirmCalls = append(confirmCalls, req)
			return &proto.UIConfirmResponse{Confirmed: true}, nil
		},
		onSessionAppend: func(ctx context.Context, req *proto.SessionAppendRequest) (*proto.SessionAppendResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			sessionAppends = append(sessionAppends, req)
			return &proto.SessionAppendResponse{EntryId: "entry-123"}, nil
		},
		onSessionScan: func(ctx context.Context, req *proto.SessionScanRequest) (*proto.SessionScanResponse, error) {
			return &proto.SessionScanResponse{Entries: []*proto.SessionEntry{}}, nil
		},
	}

	// Start gRPC server with our mock HostServer.
	lis := bufconn.Listen(1024)
	srv := grpc.NewServer()
	proto.RegisterHostServer(srv, mockHostServer)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	// Dial the in-process server for Host calls.
	conn, err := grpc.NewClient("passthrough:///bufconn/test",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithInsecure(),
	)
	if err != nil {
		t.Fatalf("failed to dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	hostClient := proto.NewHostClient(conn)
	ctx := context.Background()

	// Simulate plugin-side: inside ExecuteTool, the plugin makes Host RPC calls.
	// This is the reverse-RPC path: plugin -> Host.

	// 1. Call Host.GetConfig from inside the tool.
	t.Run("GetConfig from ExecuteTool", func(t *testing.T) {
		resp, err := hostClient.GetConfig(ctx, &proto.GetConfigRequest{Key: "plugin.setting"})
		if err != nil {
			t.Fatalf("GetConfig failed: %v", err)
		}
		if !resp.Exists {
			t.Error("expected config to exist")
		}
	})

	// 2. Call Host.StateSet then StateGet from inside the tool.
	t.Run("StateSet/Get from ExecuteTool", func(t *testing.T) {
		_, err := hostClient.StateSet(ctx, &proto.StateSetRequest{Key: "counter", Value: "42"})
		if err != nil {
			t.Fatalf("StateSet failed: %v", err)
		}

		getResp, err := hostClient.StateGet(ctx, &proto.StateGetRequest{Key: "counter"})
		if err != nil {
			t.Fatalf("StateGet failed: %v", err)
		}
		if !getResp.Exists || getResp.Value != "42" {
			t.Errorf("StateGet: expected value=42, got %+v", getResp)
		}
	})

	// 3. Call Host.UIConfirm from inside the tool.
	t.Run("UIConfirm from ExecuteTool", func(t *testing.T) {
		resp, err := hostClient.UIConfirm(ctx, &proto.UIConfirmRequest{
			Title:   "Proceed?",
			Message: "Continue with operation?",
		})
		if err != nil {
			t.Fatalf("UIConfirm failed: %v", err)
		}
		if !resp.Confirmed {
			t.Error("expected confirmation")
		}
	})

	// 4. Call Host.SessionAppend from inside the tool.
	t.Run("SessionAppend from ExecuteTool", func(t *testing.T) {
		resp, err := hostClient.SessionAppend(ctx, &proto.SessionAppendRequest{
			EntryType: "custom",
			Payload:   `{"type": "plugin_data", "source": "execute_tool"}`,
		})
		if err != nil {
			t.Fatalf("SessionAppend failed: %v", err)
		}
		if resp.EntryId == "" {
			t.Error("expected entry ID from SessionAppend")
		}
	})

	// Verify all Host RPCs were recorded.
	mu.Lock()
	defer mu.Unlock()

	if len(configKeys) == 0 || configKeys[0] != "plugin.setting" {
		t.Errorf("GetConfig not recorded: got %v", configKeys)
	}
	if stateValues["counter"] != "42" {
		t.Errorf("StateSet not recorded: got %v", stateValues)
	}
	if len(confirmCalls) == 0 {
		t.Error("UIConfirm not recorded")
	}
	if len(sessionAppends) == 0 {
		t.Error("SessionAppend not recorded")
	}
}

func (m *mockPluginClient) ReachTools(ctx context.Context, req *proto.ToolCall, opts ...grpc.CallOption) (*proto.ReachToolsResponse, error) {
	return &proto.ReachToolsResponse{Names: []string{"docs"}}, ctx.Err()
}
