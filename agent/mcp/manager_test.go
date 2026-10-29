package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/agent"
)

// fakeServers connects each configured name to an in-memory go-sdk server.
// Names without a server fail like an unreachable one; a nil server never
// answers initialize.
func fakeServers(t *testing.T, servers map[string]*sdkmcp.Server) TransportFactory {
	t.Helper()
	return func(entry ServerEntry, _ string) (sdkmcp.Transport, error) {
		srv, ok := servers[entry.Name]
		if !ok {
			return nil, errors.New("connection refused")
		}
		clientT, serverT := sdkmcp.NewInMemoryTransports()
		if srv == nil {
			conn, err := serverT.Connect(context.Background())
			if err != nil {
				return nil, err
			}
			go func() {
				for {
					if _, err := conn.Read(context.Background()); err != nil {
						return
					}
				}
			}()
			t.Cleanup(func() { _ = conn.Close() }) // closing an already closed pipe errors harmlessly
			return clientT, nil
		}
		ss, err := srv.Connect(context.Background(), serverT, nil)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { _ = ss.Close() }) // the client side may already have closed it
		return clientT, nil
	}
}

func entry(name string, cfg ServerConfig) ServerEntry {
	if cfg.Command == "" && cfg.URL == "" {
		cfg.Command = "fake"
	}
	return ServerEntry{Name: name, Config: cfg, Scope: ScopeGlobal}
}

func addTool(srv *sdkmcp.Server, name, description string, h sdkmcp.ToolHandler) {
	srv.AddTool(&sdkmcp.Tool{Name: name, Description: description, InputSchema: map[string]any{"type": "object"}}, h)
}

func textResult(s string) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: s}}}
}

func waitConnected(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for _, s := range m.servers {
		select {
		case <-s.ready:
		case <-ctx.Done():
			t.Fatalf("server %s never settled", s.entry.Name)
		}
	}
}

func toolsByName(m *Manager) map[string]agent.Tool {
	out := map[string]agent.Tool{}
	for _, tool := range m.Tools() {
		out[tool.Name] = tool
	}
	return out
}

func TestManagerConnectsServersAndAdaptsTools(t *testing.T) {
	docs := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "docs"}, &sdkmcp.ServerOptions{Instructions: "Search the docs."})
	addTool(docs, "search", "  Search docs.  ", func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return textResult("ok"), nil
	})
	addTool(docs, "a-b", "", nil)
	addTool(docs, "a_b", "", nil)
	docs.AddResource(&sdkmcp.Resource{URI: "file:///r", Name: "r"}, func(context.Context, *sdkmcp.ReadResourceRequest) (*sdkmcp.ReadResourceResult, error) {
		return nil, errors.New("resources must not be read on connect")
	})
	plain := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "plain"}, &sdkmcp.ServerOptions{Instructions: " Plain tools.\nSecond line."})
	addTool(plain, strings.Repeat("x", 80), "long", nil)

	var changes atomic.Int32
	cfg := Config{Servers: []ServerEntry{
		entry("my-docs", ServerConfig{Description: "Docs", ToolExposure: []ToolExposure{{"search", ExposureDirect}, {"a*", ExposureHidden}}}),
		entry("plain", ServerConfig{}),
		entry("down", ServerConfig{Exposure: ExposureDirect}),
		entry("off", ServerConfig{Enabled: new(false)}),
	}}
	m := NewManager(cfg, "/work", "1.0", WithTransport(fakeServers(t, map[string]*sdkmcp.Server{"my-docs": docs, "plain": plain})),
		WithOnChange(func() { changes.Add(1) }))
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	assert.Empty(t, m.Tools())
	assert.Equal(t, StatusDisabled, m.States()[0].Status, "nothing connects before Start")
	m.Start()
	require.NoError(t, m.WaitDirect(t.Context()))
	require.NoError(t, m.WaitAll(t.Context()))
	waitConnected(t, m)

	states := m.States()
	require.Len(t, states, 4)
	assert.Equal(t, ServerState{
		Name: "my-docs", Scope: ScopeGlobal, Transport: "fake", Status: StatusConnected, Exposure: ExposureCodemode, Resources: true,
		Tools:     []ToolState{{"a-b", "", ExposureHidden}, {"a_b", "", ExposureHidden}, {"search", "Search docs.", ExposureDirect}},
		Exposures: []Exposure{ExposureCodemode, ExposureDirect, ExposureHidden}, Summary: "Docs",
	}, states[0])
	assert.Equal(t, StatusConnected, states[1].Status)
	assert.Equal(t, "Plain tools.", states[1].Summary, "connected servers fall back to their instructions")
	assert.Equal(t, []Exposure{ExposureCodemode}, states[1].Exposures)
	assert.False(t, states[1].Resources)
	assert.Equal(t, StatusFailed, states[2].Status)
	assert.Equal(t, `MCP server "down" failed to connect: connection refused`, states[2].Error)
	assert.Equal(t, StatusDisabled, states[3].Status)
	assert.Positive(t, changes.Load())

	tools := toolsByName(m)
	require.Len(t, tools, 4)
	search := tools["mcp__my_docs__search"]
	assert.Equal(t, "my-docs/search", search.Label)
	assert.Equal(t, "Search docs.", search.Description)
	assert.Equal(t, agent.ToolDirect, search.Exposure)
	assert.Equal(t, &agent.ToolNamespace{Name: "mcp__my_docs", Description: "Docs", Instructions: "Search the docs."}, search.Namespace)
	assert.Equal(t, map[string]any{"type": "object", "properties": map[string]any{}}, search.Parameters)
	assert.Contains(t, tools, "mcp__my_docs__a_b", "the first of two colliding tools keeps the plain name")
	var hashed string
	for name := range tools {
		if strings.HasPrefix(name, "mcp__my_docs__a_b_") {
			hashed = name
		}
	}
	assert.Len(t, hashed, len("mcp__my_docs__a_b_")+8, "the second gets a hash suffix")
	assert.Equal(t, agent.ToolHidden, tools[hashed].Exposure)
	var long agent.Tool
	for name, tool := range tools {
		if strings.HasPrefix(name, "mcp__plain__") {
			long = tool
		}
	}
	assert.Len(t, long.Name, maxToolNameLen)
	assert.Equal(t, agent.ToolDeferred, long.Exposure, "codemode exposure is deferred")
	assert.Equal(t, "long", long.Description)
}

func TestManagerCallsConvertResultsAndReportProgress(t *testing.T) {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "s"}, nil)
	addTool(srv, "echo", "", func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if err := req.Session.NotifyProgress(ctx, &sdkmcp.ProgressNotificationParams{ProgressToken: req.Params.GetProgressToken(), Progress: 1, Total: 2}); err != nil {
			return nil, err
		}
		var args map[string]any
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "hi " + args["who"].(string)}}, StructuredContent: []any{1, "two"}}, nil
	})
	addTool(srv, "structured", "", func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{StructuredContent: map[string]any{"n": 1}}, nil
	})
	addTool(srv, "fails", "", func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{IsError: true}, nil
	})
	m := NewManager(Config{Servers: []ServerEntry{entry("s", ServerConfig{})}}, "", "", WithTransport(fakeServers(t, map[string]*sdkmcp.Server{"s": srv})))
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	m.Start()
	waitConnected(t, m)
	tools := toolsByName(m)

	var (
		mu      sync.Mutex
		updates []string
	)
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(updates)
	}
	result, err := tools["mcp__s__echo"].Execute(agent.ToolContext{
		Context: t.Context(), Args: map[string]any{"who": "tau"},
		OnUpdate: func(r *agent.ToolResult) {
			mu.Lock()
			defer mu.Unlock()
			updates = append(updates, r.Content)
		},
	})
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return len(snapshot()) == 1 }, time.Second, time.Millisecond)
	assert.Equal(t, []string{"Progress 1/2"}, snapshot())
	assert.Equal(t, "hi tau", result.Content)
	assert.JSONEq(t, `{"content":[{"type":"text","text":"hi tau"}],"structuredContent":[1,"two"]}`, string(result.StructuredContent))
	assert.Equal(t, Details{Server: "s", Tool: "echo"}, result.Details)

	result, err = tools["mcp__s__structured"].Execute(agent.ToolContext{Context: t.Context()})
	require.NoError(t, err)
	assert.Equal(t, "{\n  \"n\": 1\n}", result.Content, "structured content without blocks becomes JSON text")

	result, err = tools["mcp__s__fails"].Execute(agent.ToolContext{Context: t.Context()})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Equal(t, "MCP tool s/fails returned an error", result.Content)
}

func TestManagerCallsTimeOutUnlessProgressRenews(t *testing.T) {
	var calls atomic.Int32
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "s"}, nil)
	addTool(srv, "hang", "", func(ctx context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	addTool(srv, "slow", "", func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		for i := range 4 {
			time.Sleep(30 * time.Millisecond)
			if err := req.Session.NotifyProgress(ctx, &sdkmcp.ProgressNotificationParams{ProgressToken: req.Params.GetProgressToken(), Progress: float64(i)}); err != nil {
				return nil, err
			}
		}
		return textResult("done"), nil
	})
	m := NewManager(Config{Servers: []ServerEntry{entry("s", ServerConfig{Timeout: 0.08})}}, "", "", WithTransport(fakeServers(t, map[string]*sdkmcp.Server{"s": srv})))
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	m.Start()
	waitConnected(t, m)
	tools := toolsByName(m)

	_, err := tools["mcp__s__hang"].Execute(agent.ToolContext{Context: t.Context()})
	require.EqualError(t, err, "MCP tool s/hang timed out after 80ms")

	result, err := tools["mcp__s__slow"].Execute(agent.ToolContext{Context: t.Context()})
	require.NoError(t, err, "progress renews the timeout")
	assert.Equal(t, "done", result.Content)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	before := calls.Load()
	_, err = tools["mcp__s__hang"].Execute(agent.ToolContext{Context: cancelled})
	require.ErrorIs(t, err, context.Canceled)
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, before, calls.Load(), "a cancelled call is never sent")
}

func TestManagerCloseSettlesPendingConnections(t *testing.T) {
	m := NewManager(Config{Servers: []ServerEntry{entry("silent", ServerConfig{})}}, "", "", WithTransport(fakeServers(t, map[string]*sdkmcp.Server{"silent": nil})))
	m.Start()
	assert.Equal(t, StatusConnecting, m.States()[0].Status)
	done := make(chan error, 1)
	go func() { done <- m.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Close waited on a connection that never answers")
	}
	assert.Equal(t, StatusClosed, m.States()[0].Status)
	assert.Empty(t, m.Tools())
	m.Start()
	assert.Equal(t, StatusClosed, m.States()[0].Status, "a closed manager doesn't reconnect")
}

func TestManagerWaitDirectHonorsContext(t *testing.T) {
	m := NewManager(Config{Servers: []ServerEntry{entry("silent", ServerConfig{Exposure: ExposureDirect}), entry("other", ServerConfig{})}}, "", "",
		WithTransport(fakeServers(t, map[string]*sdkmcp.Server{"silent": nil, "other": nil})))
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	m.Start()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, m.WaitDirect(ctx), context.DeadlineExceeded)
	require.ErrorIs(t, m.WaitAll(ctx), context.DeadlineExceeded)
}

func TestManagerConfigureSavesAndApplies(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "mcp.json")
	project := filepath.Join(dir, "project", "mcp.json")
	require.NoError(t, os.WriteFile(global, []byte(`{"mcpServers":{"s":{"command":"fake"},"t":{"command":"fake"}}}`), 0o600))
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "s"}, nil)
	addTool(srv, "echo", "Echo.", nil)
	other := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "t"}, nil)
	cfg := Config{ProjectConfig: project, Servers: []ServerEntry{
		{Name: "s", Config: ServerConfig{Command: "fake"}, Source: global, Scope: ScopeGlobal},
		{Name: "t", Config: ServerConfig{Command: "fake"}, Source: global, Scope: ScopeGlobal},
	}}
	m := NewManager(cfg, "", "", WithTransport(fakeServers(t, map[string]*sdkmcp.Server{"s": srv, "t": other})))
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	m.Start()
	waitConnected(t, m)
	assert.Equal(t, project, m.States()[0].ProjectConfig)

	require.NoError(t, m.Configure("s", ConfigPatch{Exposure: ExposureDirect}))
	assert.Equal(t, agent.ToolDirect, toolsByName(m)["mcp__s__echo"].Exposure, "connected tools take the new exposure")
	assert.Equal(t, ExposureDirect, m.States()[0].Tools[0].Exposure)

	require.NoError(t, m.Configure("s", ConfigPatch{Enabled: new(false)}))
	assert.Equal(t, StatusDisabled, m.States()[0].Status)
	assert.Empty(t, toolsByName(m))
	data, err := os.ReadFile(global)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{"s":{"command":"fake","exposure":"direct","enabled":false},"t":{"command":"fake"}}}`, string(data))

	require.NoError(t, m.Configure("s", ConfigPatch{Enabled: new(true)}))
	waitConnected(t, m)
	assert.Equal(t, StatusConnected, m.States()[0].Status)
	assert.Contains(t, toolsByName(m), "mcp__s__echo")

	require.NoError(t, m.Configure("t", ConfigPatch{Enabled: new(false), Project: true}))
	states := m.States()
	assert.Equal(t, StatusDisabled, states[1].Status)
	assert.Equal(t, project, states[1].Override)
	assert.Empty(t, states[1].ProjectConfig, "an overridden server has no project override to add")
	data, err = os.ReadFile(project)
	require.NoError(t, err)
	assert.Equal(t, "{\n  \"mcpServers\": {\n    \"t\": {\n      \"enabled\": false\n    }\n  }\n}\n", string(data))

	assert.ErrorContains(t, m.Configure("missing", ConfigPatch{Enabled: new(true)}), "not configured")
}

func TestManagerDisableDuringConnectDiscardsTheSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"mcpServers":{"silent":{"command":"fake"}}}`), 0o600))
	m := NewManager(Config{Servers: []ServerEntry{{Name: "silent", Config: ServerConfig{Command: "fake", Timeout: 0.05}, Source: path, Scope: ScopeGlobal}}}, "", "",
		WithTransport(fakeServers(t, map[string]*sdkmcp.Server{"silent": nil})))
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	m.Start()
	require.NoError(t, m.Configure("silent", ConfigPatch{Enabled: new(false)}))
	waitConnected(t, m)
	assert.Equal(t, StatusDisabled, m.States()[0].Status, "the superseded attempt's failure doesn't land")
	assert.Empty(t, m.States()[0].Error)
}
