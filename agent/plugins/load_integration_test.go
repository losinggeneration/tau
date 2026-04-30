package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/losinggeneration/tau/agent/plugins/proto"
)

const pluginLoadTestHelperEnv = "TAU_PLUGIN_LOAD_TEST_HELPER"

func runPluginLoadTestHelperIfRequested() bool {
	if os.Getenv(pluginLoadTestHelperEnv) != "1" {
		return false
	}

	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  1,
			MagicCookieKey:   "TAU_PLUGIN_API_VERSION",
			MagicCookieValue: "1",
		},
		Plugins: map[string]plugin.Plugin{
			"tau": &loadTestPlugin{impl: &loadTestPluginServer{}},
		},
		GRPCServer: plugin.DefaultGRPCServer,
		Logger:     hclog.New(&hclog.LoggerOptions{Name: "tau-load-test-plugin", Output: os.Stderr, Level: hclog.Warn}),
	})
	return true
}

type loadTestPlugin struct {
	plugin.NetRPCUnsupportedPlugin

	impl *loadTestPluginServer
}

func (p *loadTestPlugin) GRPCServer(_ *plugin.GRPCBroker, server *grpc.Server) error {
	proto.RegisterPluginServer(server, p.impl)
	return nil
}

func (p *loadTestPlugin) GRPCClient(context.Context, *plugin.GRPCBroker, *grpc.ClientConn) (interface{}, error) {
	return nil, fmt.Errorf("load test plugin only implements the plugin server")
}

type loadTestPluginServer struct {
	proto.UnimplementedPluginServer
}

func (s *loadTestPluginServer) Initialize(context.Context, *proto.InitRequest) (*proto.InitResponse, error) {
	return &proto.InitResponse{ApiVersion: "1"}, nil
}

func (s *loadTestPluginServer) ListRegistrations(context.Context, *proto.ListRegistrationsRequest) (*proto.Registrations, error) {
	return &proto.Registrations{
		Tools: []*proto.ToolRegistration{
			{
				Name:        "echo",
				Description: "Echo a message from the plugin load integration test helper.",
				Parameters: map[string]string{
					"type":       "object",
					"properties": `{"message":{"type":"string"}}`,
				},
			},
		},
		Commands: []*proto.CommandRegistration{
			{Name: "ping", Description: "Return pong from the plugin load integration test helper."},
		},
		EventSubscriptions: []string{"plugin_load_test_event"},
		ToolPrefix:         "loadtest_",
	}, nil
}

func (s *loadTestPluginServer) ExecuteTool(_ context.Context, call *proto.ToolCall) (*proto.ToolResult, error) {
	var args struct {
		Message string `json:"message"`
	}
	if strings.TrimSpace(call.Arguments) != "" {
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return &proto.ToolResult{IsError: true, ErrorMessage: err.Error()}, nil
		}
	}
	if args.Message == "" {
		args.Message = "default"
	}
	return &proto.ToolResult{Output: "echo:" + args.Message}, nil
}

func (s *loadTestPluginServer) ExecuteCommand(context.Context, *proto.CommandInvocation) (*proto.CommandResult, error) {
	return &proto.CommandResult{Output: "pong"}, nil
}

func (s *loadTestPluginServer) OnEvent(context.Context, *proto.Event) (*proto.EventResponse, error) {
	return &proto.EventResponse{}, nil
}

func (s *loadTestPluginServer) Shutdown(context.Context, *proto.ShutdownRequest) (*proto.ShutdownResponse, error) {
	return &proto.ShutdownResponse{Graceful: true}, nil
}

func TestIntegrationLoadPluginFromScriptWrapper(t *testing.T) {
	t.Parallel()

	pluginPath := writePluginLoadTestScript(t)

	mgr := NewManager(t.TempDir(), nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := mgr.Load(ctx, pluginPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if info.Name != filepath.Base(pluginPath) {
		t.Errorf("expected plugin name %q, got %q", filepath.Base(pluginPath), info.Name)
	}
	if info.APIVersion != "1" {
		t.Errorf("expected API version 1, got %q", info.APIVersion)
	}
	if len(info.Registrations.Tools) != 1 || info.Registrations.Tools[0].Name != "echo" {
		t.Fatalf("expected echo tool registration, got %+v", info.Registrations.Tools)
	}
	if len(info.Registrations.Commands) != 1 || info.Registrations.Commands[0].Name != "ping" {
		t.Fatalf("expected ping command registration, got %+v", info.Registrations.Commands)
	}

	output, isError, err := mgr.ExecutePluginTool(ctx, "loadtest_echo", "call-1", `{"message":"hello"}`)
	if err != nil {
		t.Fatalf("ExecutePluginTool failed: %v", err)
	}
	if isError {
		t.Fatal("ExecutePluginTool returned isError=true")
	}
	if output != "echo:hello" {
		t.Errorf("expected echo output, got %q", output)
	}

	if err := mgr.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func writePluginLoadTestScript(t *testing.T) string {
	t.Helper()

	pluginPath := filepath.Join(t.TempDir(), "tau-load-test-plugin")
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("get test executable: %v", err)
	}

	content := fmt.Sprintf("#!/bin/sh\n%s=1 exec %q\n", pluginLoadTestHelperEnv, testBinary)
	if err := os.WriteFile(pluginPath, []byte(content), 0o755); err != nil {
		t.Fatalf("write plugin wrapper: %v", err)
	}
	return pluginPath
}
