package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/losinggeneration/tau/agent/plugins/proto"
)

const (
	apiVersion = "1"
	pluginName = "hello-world"
)

type helloPlugin struct {
	plugin.NetRPCUnsupportedPlugin

	impl *helloServer
}

func (p *helloPlugin) GRPCServer(_ *plugin.GRPCBroker, server *grpc.Server) error {
	proto.RegisterPluginServer(server, p.impl)
	return nil
}

func (p *helloPlugin) GRPCClient(context.Context, *plugin.GRPCBroker, *grpc.ClientConn) (interface{}, error) {
	return nil, fmt.Errorf("%s only implements the plugin server", pluginName)
}

type helloServer struct {
	proto.UnimplementedPluginServer

	mu       sync.Mutex
	initInfo *proto.InitRequest
	events   []string
}

func (s *helloServer) Initialize(_ context.Context, req *proto.InitRequest) (*proto.InitResponse, error) {
	s.mu.Lock()
	s.initInfo = req
	s.mu.Unlock()

	return &proto.InitResponse{ApiVersion: apiVersion}, nil
}

func (s *helloServer) ListRegistrations(context.Context, *proto.ListRegistrationsRequest) (*proto.Registrations, error) {
	return &proto.Registrations{
		Tools: []*proto.ToolRegistration{
			{
				Name:        "hello",
				Description: "Return a hello-world greeting from the example plugin.",
				Parameters: map[string]string{
					"type":       "object",
					"properties": `{"name":{"type":"string","description":"Optional name to greet."}}`,
				},
			},
		},
		Commands: []*proto.CommandRegistration{
			{
				Name:         "hello",
				Description:  "Run the hello-world example plugin.",
				ArgumentHint: "[name]",
			},
		},
		EventSubscriptions: []string{"session_start", "session_shutdown"},
		CliFlags: []*proto.CLIFlag{
			{
				Name:         "hello-target",
				Description:  "Default name used by the hello-world example plugin.",
				DefaultValue: "tau",
			},
		},
		ToolPrefix: "",
	}, nil
}

func (s *helloServer) OnEvent(_ context.Context, event *proto.Event) (*proto.EventResponse, error) {
	s.mu.Lock()
	s.events = append(s.events, event.Name)
	s.mu.Unlock()

	return &proto.EventResponse{}, nil
}

func (s *helloServer) ExecuteTool(_ context.Context, call *proto.ToolCall) (*proto.ToolResult, error) {
	name := "tau"
	if strings.TrimSpace(call.Arguments) != "" {
		var args struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return &proto.ToolResult{IsError: true, ErrorMessage: fmt.Sprintf("parse arguments: %v", err)}, nil
		}
		if strings.TrimSpace(args.Name) != "" {
			name = strings.TrimSpace(args.Name)
		}
	}

	return &proto.ToolResult{Output: fmt.Sprintf("Hello, %s! This came from the tau hello-world plugin.", name)}, nil
}

func (s *helloServer) ExecuteCommand(_ context.Context, invocation *proto.CommandInvocation) (*proto.CommandResult, error) {
	name := strings.TrimSpace(invocation.Arguments)
	if name == "" {
		name = "tau"
	}

	s.mu.Lock()
	initInfo := s.initInfo
	events := append([]string(nil), s.events...)
	s.mu.Unlock()

	var details []string
	if initInfo != nil {
		details = append(details, fmt.Sprintf("cwd=%s", initInfo.Cwd))
		if initInfo.SessionId != "" {
			details = append(details, fmt.Sprintf("session=%s", initInfo.SessionId))
		}
	}
	if len(events) > 0 {
		details = append(details, fmt.Sprintf("events=%s", strings.Join(events, ",")))
	}

	output := fmt.Sprintf("Hello, %s! The hello-world plugin is loaded and responding.", name)
	if len(details) > 0 {
		output += "\n" + strings.Join(details, "\n")
	}

	return &proto.CommandResult{Output: output}, nil
}

func (s *helloServer) Shutdown(context.Context, *proto.ShutdownRequest) (*proto.ShutdownResponse, error) {
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}()
	return &proto.ShutdownResponse{Graceful: true}, nil
}

func main() {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  1,
			MagicCookieKey:   "TAU_PLUGIN_API_VERSION",
			MagicCookieValue: apiVersion,
		},
		Plugins: map[string]plugin.Plugin{
			"tau": &helloPlugin{impl: &helloServer{}},
		},
		GRPCServer: plugin.DefaultGRPCServer,
		Logger:     hclog.New(&hclog.LoggerOptions{Name: pluginName, Output: os.Stderr, Level: hclog.Warn}),
	})
}
