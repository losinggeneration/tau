// Package testutil provides an in-process Plugin SDK test harness for plugin authors.
// It allows plugins to be unit-tested without launching tau or a subprocess.
package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/losinggeneration/tau/agent/plugins/proto"
)

// MockHost implements the Host service for in-process testing.
// It collects calls made by the plugin under test for assertion.
type MockHost struct {
	// mu protects the collected calls.
	mu    sync.RWMutex
	calls []Call

	// Configuration for the plugin.
	config map[string]string

	// Plugin state storage.
	state   map[string]string
	stateMu sync.RWMutex
}

// Call represents a single call made to the host.
type Call struct {
	Method  string
	Request any
}

// NewMockHost creates a new MockHost with optional initial configuration.
func NewMockHost(config map[string]string) *MockHost {
	return &MockHost{
		config: config,
		state:  make(map[string]string),
	}
}

// Calls returns all calls made to the host.
func (h *MockHost) Calls() []Call {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]Call, len(h.calls))
	copy(result, h.calls)
	return result
}

// CallCount returns the number of calls for a specific method.
func (h *MockHost) CallCount(method string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	count := 0
	for _, c := range h.calls {
		if c.Method == method {
			count++
		}
	}
	return count
}

// ClearCalls resets the collected calls.
func (h *MockHost) ClearCalls() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = nil
}

// UIConfirm implements HostService.UIConfirm.
func (h *MockHost) UIConfirm(ctx context.Context, req *proto.UIConfirmRequest) (*proto.UIConfirmResponse, error) {
	h.recordCall("UIConfirm", req)
	return &proto.UIConfirmResponse{Confirmed: false}, nil
}

// UISelect implements HostService.UISelect.
func (h *MockHost) UISelect(ctx context.Context, req *proto.UISelectRequest) (*proto.UISelectResponse, error) {
	h.recordCall("UISelect", req)
	return &proto.UISelectResponse{SelectedIndex: -1}, nil
}

// UIInput implements HostService.UIInput.
func (h *MockHost) UIInput(ctx context.Context, req *proto.UIInputRequest) (*proto.UIInputResponse, error) {
	h.recordCall("UIInput", req)
	return &proto.UIInputResponse{Value: ""}, nil
}

// UINotify implements HostService.UINotify.
func (h *MockHost) UINotify(ctx context.Context, req *proto.UINotifyRequest) (*proto.UINotifyResponse, error) {
	h.recordCall("UINotify", req)
	return &proto.UINotifyResponse{}, nil
}

// UISetStatus implements HostService.UISetStatus.
func (h *MockHost) UISetStatus(ctx context.Context, req *proto.UISetStatusRequest) (*proto.UISetStatusResponse, error) {
	h.recordCall("UISetStatus", req)
	return &proto.UISetStatusResponse{}, nil
}

// UISetWidget implements HostService.UISetWidget.
func (h *MockHost) UISetWidget(ctx context.Context, req *proto.UISetWidgetRequest) (*proto.UISetWidgetResponse, error) {
	h.recordCall("UISetWidget", req)
	return &proto.UISetWidgetResponse{}, nil
}

// Log implements HostService.Log.
func (h *MockHost) Log(ctx context.Context, req *proto.LogRequest) (*proto.LogResponse, error) {
	h.recordCall("Log", req)
	return &proto.LogResponse{}, nil
}

// GetConfig implements HostService.GetConfig.
func (h *MockHost) GetConfig(ctx context.Context, req *proto.GetConfigRequest) (*proto.GetConfigResponse, error) {
	h.recordCall("GetConfig", req)
	if h.config == nil {
		return &proto.GetConfigResponse{Exists: false}, nil
	}
	if req.Key == "" {
		val, _ := json.Marshal(h.config)
		return &proto.GetConfigResponse{Value: string(val), Exists: true}, nil
	}
	val, ok := h.config[req.Key]
	if !ok {
		return &proto.GetConfigResponse{Exists: false}, nil
	}
	valJSON, _ := json.Marshal(val)
	return &proto.GetConfigResponse{Value: string(valJSON), Exists: true}, nil
}

// ToolProgress implements HostService.ToolProgress.
func (h *MockHost) ToolProgress(ctx context.Context, req *proto.ToolProgressEvent) (*proto.ToolProgressResponse, error) {
	h.recordCall("ToolProgress", req)
	return &proto.ToolProgressResponse{}, nil
}

// StateGet implements HostService.StateGet.
func (h *MockHost) StateGet(ctx context.Context, req *proto.StateGetRequest) (*proto.StateGetResponse, error) {
	h.recordCall("StateGet", req)
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()
	val, ok := h.state[req.Key]
	return &proto.StateGetResponse{Value: val, Exists: ok}, nil
}

// StateSet implements HostService.StateSet.
func (h *MockHost) StateSet(ctx context.Context, req *proto.StateSetRequest) (*proto.StateSetResponse, error) {
	h.recordCall("StateSet", req)
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	h.state[req.Key] = req.Value
	return &proto.StateSetResponse{}, nil
}

// StateAppend implements HostService.StateAppend.
func (h *MockHost) StateAppend(ctx context.Context, req *proto.StateAppendRequest) (*proto.StateAppendResponse, error) {
	h.recordCall("StateAppend", req)
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	h.state[req.Key] += req.Value
	return &proto.StateAppendResponse{}, nil
}

// StateDelete implements HostService.StateDelete.
func (h *MockHost) StateDelete(ctx context.Context, req *proto.StateDeleteRequest) (*proto.StateDeleteResponse, error) {
	h.recordCall("StateDelete", req)
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	delete(h.state, req.Key)
	return &proto.StateDeleteResponse{}, nil
}

// SessionAppend implements HostService.SessionAppend.
func (h *MockHost) SessionAppend(ctx context.Context, req *proto.SessionAppendRequest) (*proto.SessionAppendResponse, error) {
	h.recordCall("SessionAppend", req)
	return &proto.SessionAppendResponse{EntryId: "test-entry-id"}, nil
}

// SessionScan implements HostService.SessionScan.
func (h *MockHost) SessionScan(ctx context.Context, req *proto.SessionScanRequest) (*proto.SessionScanResponse, error) {
	h.recordCall("SessionScan", req)
	return &proto.SessionScanResponse{Entries: nil}, nil
}

// recordCall records a call for later assertion.
func (h *MockHost) recordCall(method string, req any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, Call{Method: method, Request: req})
}

// PluginTestHarness provides a complete test environment for plugins.
type PluginTestHarness struct {
	host   *MockHost
	server *mockServer
}

// NewPluginTestHarness creates a new test harness.
func NewPluginTestHarness(config map[string]string) *PluginTestHarness {
	host := NewMockHost(config)
	return &PluginTestHarness{
		host:   host,
		server: newMockServer(host),
	}
}

// Host returns the MockHost for assertions.
func (h *PluginTestHarness) Host() *MockHost {
	return h.host
}

// Server returns the mock gRPC server for the Host service.
func (h *PluginTestHarness) Server() *mockServer {
	return h.server
}

// mockServer implements a simple gRPC-like server for testing.
// In a real implementation, this would use a gRPC test library.
type mockServer struct {
	host *MockHost
}

// newMockServer creates a new mock server.
func newMockServer(host *MockHost) *mockServer {
	return &mockServer{host: host}
}

// PluginTestHelper provides assertion utilities for plugin tests.
type PluginTestHelper struct {
	harness *PluginTestHarness
}

// NewPluginTestHelper creates a new test helper.
func NewPluginTestHelper(config map[string]string) *PluginTestHelper {
	return &PluginTestHelper{
		harness: NewPluginTestHarness(config),
	}
}

// AssertCallCount asserts the number of calls to a specific method.
func (h *PluginTestHelper) AssertCallCount(t assertT, method string, expected int) {
	actual := h.harness.host.CallCount(method)
	if actual != expected {
		t.Errorf("expected %d calls to %s, got %d", expected, method, actual)
	}
}

// AssertCallCountAtLeast asserts at least N calls to a specific method.
func (h *PluginTestHelper) AssertCallCountAtLeast(t assertT, method string, min int) {
	actual := h.harness.host.CallCount(method)
	if actual < min {
		t.Errorf("expected at least %d calls to %s, got %d", min, method, actual)
	}
}

// AssertNoCalls asserts no calls were made to a specific method.
func (h *PluginTestHelper) AssertNoCalls(t assertT, method string) {
	h.AssertCallCount(t, method, 0)
}

// GetCalls returns all calls made to the host.
func (h *PluginTestHelper) GetCalls() []Call {
	return h.harness.host.Calls()
}

// assertT is an interface compatible with testing.T and testing.B.
type assertT interface {
	Errorf(format string, args ...any)
}

// AssertNotifications asserts that the specified number of UINotify calls were made.
func (h *PluginTestHelper) AssertNotifications(t assertT, min int) {
	h.AssertCallCountAtLeast(t, "UINotify", min)
}

// AssertConfirmCalled asserts that UIConfirm was called.
func (h *PluginTestHelper) AssertConfirmCalled(t assertT) {
	h.AssertCallCountAtLeast(t, "UIConfirm", 1)
}

// AssertStateSet asserts that StateSet was called with the given key.
func (h *PluginTestHelper) AssertStateSet(t assertT, key string) {
	calls := h.GetCalls()
	for _, call := range calls {
		if call.Method == "StateSet" {
			if req, ok := call.Request.(*proto.StateSetRequest); ok {
				if req.Key == key {
					return
				}
			}
		}
	}
	t.Errorf("expected StateSet call with key %q", key)
}

// RunPluginTest runs a plugin test with the given initialization.
// It simulates the tau-side of the plugin initialization handshake.
func RunPluginTest(
	ctx context.Context,
	plugin PluginUnderTest,
	harness *PluginTestHarness,
) error {
	// Simulate tau calling Initialize.
	resp, err := plugin.Initialize(ctx, &proto.InitRequest{
		Version:      "0.1.0",
		SessionId:    "test-session",
		Cwd:          "/tmp",
		ManifestPath: "",
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("plugin init error: %s", resp.Error)
	}

	// Simulate tau calling ListRegistrations.
	regs, err := plugin.ListRegistrations(ctx, &proto.ListRegistrationsRequest{})
	if err != nil {
		return fmt.Errorf("list registrations: %w", err)
	}

	// Return the registrations for the test to verify.
	_ = regs // Registrations are available via the harness if the plugin calls back

	return nil
}

// PluginUnderTest is the interface that plugins must implement for testing.
type PluginUnderTest interface {
	Initialize(ctx context.Context, req *proto.InitRequest) (*proto.InitResponse, error)
	ListRegistrations(ctx context.Context, req *proto.ListRegistrationsRequest) (*proto.Registrations, error)
}
