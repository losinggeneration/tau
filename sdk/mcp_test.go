package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/agent/mcp"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ui"
)

type notifyRecordingUI struct {
	ui.NoOpUI
	mu       sync.Mutex
	messages []string
}

func (u *notifyRecordingUI) Notify(_ ui.NotifyLevel, message string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.messages = append(u.messages, message)
}

func (u *notifyRecordingUI) Messages() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.messages)
}

func mcpServerWithTool(name string) *sdkmcp.Server {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: name}, nil)
	srv.AddTool(&sdkmcp.Tool{Name: "search", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{}, nil
	})
	return srv
}

func promptToolNames(p *recordingProvider) []string {
	var names []string
	for _, tool := range p.LastPrompt().Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestSessionDeclaresMCPToolsAfterBoundedStartupWait(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "mcp.json"), []byte(`{"mcpServers":{
		"docs":{"command":"docs","exposure":"direct"},
		"late":{"command":"late","exposure":"direct"}}}`), 0o600))
	previous := mcpStartupWait
	mcpStartupWait = 50 * time.Millisecond
	t.Cleanup(func() { mcpStartupWait = previous })

	provider, explicitModel := registerRecordingProvider(t, "reply")
	userInterface := &notifyRecordingUI{}
	rt, err := NewRuntime(RuntimeOptions{
		CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("sk-test-key")},
		ExplicitModel:   explicitModel,
		UI:              userInterface,
		NoBuiltinTools:  true,
	})
	require.NoError(t, err)
	release := make(chan struct{})
	rt.(*runtime).mcpTransport = func(entry mcp.ServerEntry, _ string) (sdkmcp.Transport, error) {
		if entry.Name == "late" {
			<-release
		}
		clientT, serverT := sdkmcp.NewInMemoryTransports()
		ss, err := mcpServerWithTool(entry.Name).Connect(context.Background(), serverT, nil)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { _ = ss.Close() }) // the session may have closed it already
		return clientT, nil
	}

	sess, err := rt.NewSession(testContext(t), SessionOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sess.Close(context.Background())) })

	events, err := sess.Send(testContext(t), Message{Text: "first"})
	require.NoError(t, err)
	collectSessionEvents(t, events)
	assert.Equal(t, []string{"mcp__docs__search"}, promptToolNames(provider))
	assert.Contains(t, userInterface.Messages(), "MCP servers are still connecting; their tools become available once connected.")

	close(release)
	require.Eventually(t, func() bool {
		return slices.ContainsFunc(sess.(*agentSession).agent.State().Tools, func(tool agent.Tool) bool { return tool.Name == "mcp__late__search" })
	}, time.Second, 5*time.Millisecond)

	events, err = sess.Send(testContext(t), Message{Text: "second"})
	require.NoError(t, err)
	collectSessionEvents(t, events)
	assert.ElementsMatch(t, []string{"mcp__docs__search", "mcp__late__search"}, promptToolNames(provider))

	require.NoError(t, sess.(MCPStatus).MCPConfigure("late", mcp.ConfigPatch{Enabled: new(false)}))
	require.NoError(t, sess.(MCPStatus).MCPConfigure("docs", mcp.ConfigPatch{Exposure: mcp.ExposureHidden}))
	require.Eventually(t, func() bool {
		tools := sess.(*agentSession).agent.State().Tools
		return len(tools) == 1 && tools[0].Exposure == agent.ToolHidden
	}, time.Second, 5*time.Millisecond, "disabled servers' tools leave and changed exposures apply")
	events, err = sess.Send(testContext(t), Message{Text: "third"})
	require.NoError(t, err)
	collectSessionEvents(t, events)
	assert.Empty(t, promptToolNames(provider))
}

func TestSessionCloseClosesMCPServers(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "mcp.json"), []byte(`{"mcpServers":{"docs":{"command":"docs"}}}`), 0o600))
	_, explicitModel := registerRecordingProvider(t, "reply")
	rt, err := NewRuntime(RuntimeOptions{
		CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("sk-test-key")},
		ExplicitModel:   explicitModel,
	})
	require.NoError(t, err)
	closed := make(chan struct{})
	rt.(*runtime).mcpTransport = func(mcp.ServerEntry, string) (sdkmcp.Transport, error) {
		clientT, serverT := sdkmcp.NewInMemoryTransports()
		ss, err := mcpServerWithTool("docs").Connect(context.Background(), serverT, nil)
		if err != nil {
			return nil, err
		}
		go func() {
			defer close(closed)
			_ = ss.Wait() // any end of the server session counts
		}()
		return clientT, nil
	}
	sess, err := rt.NewSession(testContext(t), SessionOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		states := sess.(*agentSession).mcp.States()
		return len(states) == 1 && states[0].Status == mcp.StatusConnected
	}, time.Second, 5*time.Millisecond)

	var changes atomic.Int32
	unsubscribe := sess.(MCPStatus).OnMCPChange(func() { changes.Add(1) })
	defer unsubscribe()
	states, configErrors := sess.(MCPStatus).MCPServers()
	assert.Equal(t, "docs", states[0].Name)
	assert.Empty(t, configErrors)

	require.NoError(t, sess.Close(context.Background()))
	require.Eventually(t, func() bool { return changes.Load() > 0 }, time.Second, 5*time.Millisecond)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("MCP server session still open after session close")
	}
	assert.Equal(t, mcp.StatusClosed, sess.(*agentSession).mcp.States()[0].Status)
}

func TestRenderMCPServersSectionListsIndirectServers(t *testing.T) {
	assert.Nil(t, renderMCPServersSection([]mcp.ServerState{
		{Name: "direct", Exposures: []mcp.Exposure{mcp.ExposureDirect}},
		{Name: "off", Status: mcp.StatusDisabled, Exposures: []mcp.Exposure{mcp.ExposureCodemode}},
	}))
	section := renderMCPServersSection([]mcp.ServerState{
		{Name: "zeta", Exposures: []mcp.Exposure{mcp.ExposureDeferred}},
		{Name: "my-docs", Exposures: []mcp.Exposure{mcp.ExposureDirect, mcp.ExposureCodemode}, Summary: "Search the docs."},
	})
	require.NotNil(t, section)
	assert.Equal(t, "MCP servers whose tools are not declared to you. Call the tools of `codemode` servers from codemode scripts. Load the tools of `tool_search` servers with `tool_search`.\n"+
		"- mcp__my_docs (codemode): Search the docs.\n- mcp__zeta (tool_search)", *section)

	var many []mcp.ServerState
	for i := range 200 {
		many = append(many, mcp.ServerState{Name: fmt.Sprintf("server-%03d", i), Exposures: []mcp.Exposure{mcp.ExposureCodemode}, Summary: strings.Repeat("x", 400)})
	}
	section = renderMCPServersSection(many)
	require.NotNil(t, section)
	assert.LessOrEqual(t, utf8.RuneCountInString(*section), maxServersSectionChars)
	assert.Regexp(t, `\n- … \d+ more servers; find their tools with searchTools\(\)$`, *section)
	assert.NotContains(t, *section, "server-000:", "summaries drop when the server lines alone don't fit")
}

func TestSessionActivatesCodemodeAndListsMCPServers(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "mcp.json"), []byte(`{"mcpServers":{
		"docs":{"command":"docs","description":"Search the docs.\nMore."},
		"late":{"command":"late"}}}`), 0o600))
	previous := mcpStartupWait
	mcpStartupWait = 50 * time.Millisecond
	t.Cleanup(func() { mcpStartupWait = previous })

	provider, explicitModel := registerRecordingProvider(t, "reply")
	userInterface := &notifyRecordingUI{}
	rt, err := NewRuntime(RuntimeOptions{
		CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("sk-test-key")},
		ExplicitModel:   explicitModel,
		UI:              userInterface,
	})
	require.NoError(t, err)
	release := make(chan struct{})
	rt.(*runtime).mcpTransport = func(entry mcp.ServerEntry, _ string) (sdkmcp.Transport, error) {
		if entry.Name == "late" {
			<-release
		}
		clientT, serverT := sdkmcp.NewInMemoryTransports()
		ss, err := mcpServerWithTool(entry.Name).Connect(context.Background(), serverT, nil)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { _ = ss.Close() })
		return clientT, nil
	}
	sess, err := rt.NewSession(testContext(t), SessionOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sess.Close(context.Background())) })
	t.Cleanup(func() { close(release) }) // runs first: Close waits for the connect

	events, err := sess.Send(testContext(t), Message{Text: "first"})
	require.NoError(t, err)
	collectSessionEvents(t, events)
	assert.Contains(t, promptToolNames(provider), "codemode", "codemode activates for codemode servers")
	prompt := provider.LastPrompt()
	system := prompt.SystemPrompt + ai.CurrentSystemPrompt(prompt.Messages)
	assert.Contains(t, system, "MCP servers whose tools are not declared to you.")
	assert.Contains(t, system, "- mcp__docs (codemode): Search the docs.\n- mcp__late (codemode)")
	assert.Contains(t, userInterface.Messages(), "MCP servers are still connecting; their tools become available once connected.",
		"with codemode active the first prompt waits for codemode servers too")
}

func TestSessionWarnsWhenAutoEnableCodemodeIsOff(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "mcp.json"), []byte(`{"autoEnableCodemode":false,"mcpServers":{"docs":{"command":"docs"}}}`), 0o600))
	provider, explicitModel := registerRecordingProvider(t, "reply")
	userInterface := &notifyRecordingUI{}
	rt, err := NewRuntime(RuntimeOptions{
		CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("sk-test-key")},
		ExplicitModel:   explicitModel,
		UI:              userInterface,
	})
	require.NoError(t, err)
	rt.(*runtime).mcpTransport = func(entry mcp.ServerEntry, _ string) (sdkmcp.Transport, error) {
		clientT, serverT := sdkmcp.NewInMemoryTransports()
		ss, err := mcpServerWithTool(entry.Name).Connect(context.Background(), serverT, nil)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { _ = ss.Close() })
		return clientT, nil
	}
	sess, err := rt.NewSession(testContext(t), SessionOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sess.Close(context.Background())) })
	assert.Contains(t, userInterface.Messages(), "MCP tools are only reachable from the codemode or tool_search tool, but neither is active (autoEnableCodemode is false); they cannot be called.")

	events, err := sess.Send(testContext(t), Message{Text: "first"})
	require.NoError(t, err)
	collectSessionEvents(t, events)
	assert.NotContains(t, promptToolNames(provider), "codemode")
}

func TestDeferredMCPActivatesToolSearchAndLoadsNextRequest(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "mcp.json"), []byte(`{"autoEnableCodemode":false,"mcpServers":{"docs":{"command":"docs","exposure":"deferred"},"direct":{"command":"direct","exposure":"direct"}}}`), 0o600))
	provider, model := registerRecordingProvider(t, "reply")
	userInterface := &notifyRecordingUI{}
	rt, err := NewRuntime(RuntimeOptions{
		CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("sk-test-key")},
		ExplicitModel:   model, UI: userInterface, SessionsDir: t.TempDir(),
	})
	require.NoError(t, err)
	var generation atomic.Int32
	rt.(*runtime).mcpTransport = func(entry mcp.ServerEntry, _ string) (sdkmcp.Transport, error) {
		clientT, serverT := sdkmcp.NewInMemoryTransports()
		server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: entry.Name}, nil)
		description := "Direct search"
		if entry.Name == "docs" {
			description = fmt.Sprintf("Search docs generation %d", generation.Add(1))
		}
		server.AddTool(&sdkmcp.Tool{Name: "search", Description: description, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{}, nil
		})
		ss, err := server.Connect(t.Context(), serverT, nil)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { require.NoError(t, ss.Close()) })
		return clientT, nil
	}
	sess, err := rt.NewSession(testContext(t), SessionOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sess.Close(context.Background())) })
	s := sess.(*agentSession)
	events, err := sess.Send(testContext(t), Message{Text: "first"})
	require.NoError(t, err)
	collectSessionEvents(t, events)
	assert.Contains(t, promptToolNames(provider), toolSearchName)
	assert.Contains(t, promptToolNames(provider), "mcp__direct__search")
	assert.NotContains(t, promptToolNames(provider), "codemode")
	assert.NotContains(t, promptToolNames(provider), "mcp__docs__search")
	assert.Empty(t, userInterface.Messages())
	assert.Contains(t, provider.LastPrompt().SystemPrompt+ai.CurrentSystemPrompt(provider.LastPrompt().Messages), "- mcp__docs (tool_search)")
	result := agent.RunToolCall(t.Context(), &agent.AgentContext{Tools: s.RegisteredTools()}, nil,
		ai.ToolCall{Name: toolSearchName, Arguments: json.RawMessage(`{"query":"search"}`)}, agent.AgentLoopConfig{})
	require.False(t, result.IsError, result.Content)
	assert.Equal(t, map[string]any{"loaded": []string{"mcp__docs__search"}}, result.Details)
	events, err = sess.Send(testContext(t), Message{Text: "next"})
	require.NoError(t, err)
	collectSessionEvents(t, events)
	assert.Contains(t, promptToolNames(provider), "mcp__docs__search")
	require.NoError(t, sess.(MCPStatus).MCPReconnect(t.Context(), "docs"))
	events, err = sess.Send(testContext(t), Message{Text: "reconnected"})
	require.NoError(t, err)
	collectSessionEvents(t, events)
	assert.Contains(t, promptToolNames(provider), "mcp__docs__search")
	var description string
	for _, tool := range provider.LastPrompt().Tools {
		if tool.Name == "mcp__docs__search" {
			description = tool.Description
		}
	}
	assert.Contains(t, description, "generation 2", "reconnect refreshes the loaded declaration")
	id := sess.ID()
	require.NoError(t, sess.Close(t.Context()))
	resumed, err := rt.ResumeSession(t.Context(), id, SessionOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resumed.Close(context.Background())) })
	events, err = resumed.Send(testContext(t), Message{Text: "resumed"})
	require.NoError(t, err)
	collectSessionEvents(t, events)
	assert.Contains(t, promptToolNames(provider), "mcp__docs__search")
}

func TestMCPDiscoveryRequiresReachingTools(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tools    []agent.Tool
		exposure mcp.Exposure
		auto     bool
		want     []string
		warning  bool
	}{
		{name: "deferred ignores auto codemode", tools: []agent.Tool{toolSearchTool(nil)}, exposure: mcp.ExposureDeferred, want: []string{toolSearchName}},
		{name: "codemode disabled", tools: []agent.Tool{{Name: "codemode", DefaultActive: new(false), ReachesTools: func(map[string]any) []string { return []string{"all"} }}}, exposure: mcp.ExposureCodemode, warning: true},
		{name: "same name no reach", tools: []agent.Tool{{Name: toolSearchName, DefaultActive: new(false)}}, exposure: mcp.ExposureDeferred, auto: true, warning: true},
		{name: "active replacement reaches both", tools: []agent.Tool{{Name: toolSearchName, ReachTools: func(context.Context, map[string]any) ([]string, error) { return []string{"all"}, nil }}}, exposure: mcp.ExposureCodemode, want: []string{toolSearchName}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := agent.New(agent.NewConfig(agent.WithTools(tc.tools...)))
			require.NoError(t, err)
			userInterface := &notifyRecordingUI{}
			s := &agentSession{agent: a, ui: userInterface}
			s.activateDiscovery(mcp.Config{AutoEnableCodemode: &tc.auto, Servers: []mcp.ServerEntry{{Name: "docs", Config: mcp.ServerConfig{Exposure: tc.exposure}}}})
			var active []string
			for _, tool := range a.State().Tools {
				if slices.Contains(a.ActiveTools(), tool.Name) || tool.DefaultActive == nil || *tool.DefaultActive {
					active = append(active, tool.Name)
				}
			}
			assert.Equal(t, tc.want, active)
			assert.Equal(t, tc.warning, len(userInterface.Messages()) > 0)
		})
	}
}
