package plugins

import (
	"context"
	"testing"

	"github.com/losinggeneration/tau/agent/plugins/proto"
	"github.com/losinggeneration/tau/ui"
)

func TestRefreshToolRegistryRebuildsRegistrations(t *testing.T) {
	manager := NewManager(t.TempDir(), ui.NewNoOpUI(), nil)
	manager.plugins["test"] = &PluginInfo{Name: "test", Registrations: Registrations{ToolPrefix: "test.", Tools: []ToolRegistration{{Name: "current"}}}}
	manager.toolHandlers["stale"] = func(context.Context, string, string, string) (string, bool, error) { return "", false, nil }
	called := false
	manager.RegisterToolRefreshCallback(func() {
		called = true
		tools := manager.Tools()
		if len(tools) != 1 || tools[0].Name != "test.current" {
			t.Fatalf("stale registry: %+v", tools)
		}
	})
	manager.RefreshToolRegistry()
	if !called {
		t.Fatal("refresh callback omitted")
	}
}

func TestRefreshToolRegistryPreservesLocalExecutionName(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	var called string
	manager.plugins["test"] = &PluginInfo{Name: "test", Registrations: Registrations{ToolPrefix: "ext_", Tools: []ToolRegistration{{Name: "lookup"}}}, grpcClient: &mockPluginClient{onExecuteTool: func(_ context.Context, call *proto.ToolCall) (*proto.ToolResult, error) {
		called = call.ToolName
		return &proto.ToolResult{Output: "ok"}, nil
	}}}
	manager.RefreshToolRegistry()
	output, isError, err := manager.ExecutePluginTool(t.Context(), "ext_lookup", "call", "{}")
	if err != nil || isError || output != "ok" || called != "lookup" {
		t.Fatalf("output=%q isError=%v err=%v local name=%q", output, isError, err, called)
	}
}
