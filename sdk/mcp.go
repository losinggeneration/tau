package sdk

import (
	"context"
	"errors"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/agent/mcp"
	"github.com/losinggeneration/tau/paths"
	"github.com/losinggeneration/tau/ui"
)

// mcpStartupWait bounds how long the first prompt waits for servers that can
// expose direct tools.
var mcpStartupWait = 10 * time.Second

// startMCP connects the session's configured MCP servers in the background.
func (s *agentSession) startMCP(projectTrusted bool) {
	cfg := mcp.LoadConfig(paths.ConfigDir(), s.cwd, projectTrusted)
	s.mcpErrors = cfg.Errors
	if len(cfg.Errors) > 0 {
		s.ui.Notify(ui.NotifyWarn, "MCP servers need attention:\n  config: "+strings.Join(cfg.Errors, "\n  config: ")+"\nRun /mcp to fix.")
	}
	if len(cfg.Servers) == 0 {
		return
	}
	version := "dev"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	opts := []mcp.Option{mcp.WithOnChange(s.mcpChanged), mcp.WithCredentials(mcp.NewCredentialStore(paths.ConfigDir()))}
	if s.runtime.mcpTransport != nil {
		opts = append(opts, mcp.WithTransport(s.runtime.mcpTransport))
	}
	s.mcp = mcp.NewManager(cfg, s.cwd, version, opts...)
	s.mcp.Start()
}

// syncMCPTools replaces the session's MCP tools with the connected servers'
// current ones. The agent declares them at its next request boundary.
func (s *agentSession) syncMCPTools() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.mcp == nil {
		return
	}
	want := selectSessionTools(s.mcp.Tools(), s.toolAllowlist, s.toolDenylist)
	if slices.EqualFunc(want, s.mcpTools, func(a, b agent.Tool) bool { return a.Name == b.Name && a.Exposure == b.Exposure }) {
		return
	}
	tools := slices.DeleteFunc(slices.Clone(s.agent.State().Tools), func(tool agent.Tool) bool {
		return slices.ContainsFunc(s.mcpTools, func(t agent.Tool) bool { return t.Name == tool.Name })
	})
	s.agent.SetTools(append(tools, want...)...)
	s.mcpTools = want
}

// waitMCPStartup holds the first prompt until servers that can expose direct
// tools settle, so their tools are declared in its first request.
func (s *agentSession) waitMCPStartup(ctx context.Context) {
	if s.mcp == nil {
		return
	}
	s.mcpStartup.Do(func() {
		wait, cancel := context.WithTimeout(ctx, mcpStartupWait)
		defer cancel()
		if err := s.mcp.WaitDirect(wait); errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			s.ui.Notify(ui.NotifyInfo, "MCP servers are still connecting; their tools become available once connected.")
		}
		s.syncMCPTools()
	})
}

// MCPStatus reports a session's MCP servers for /mcp.
type MCPStatus interface {
	// MCPServers returns a snapshot of the configured servers and the
	// problems found loading their configuration.
	MCPServers() (servers []mcp.ServerState, configErrors []string)
	// OnMCPChange calls fn after every server state change until the
	// returned function unsubscribes it.
	OnMCPChange(fn func()) (unsubscribe func())
	// MCPSignIn signs in to an OAuth server; MCPReconnect then uses the tokens.
	MCPSignIn(ctx context.Context, name string, prompt mcp.SignInPrompt) error
	// MCPSignOut deletes an OAuth server's credentials and disconnects it.
	MCPSignOut(ctx context.Context, name string) (removed bool, err error)
	// MCPReconnect connects a server again.
	MCPReconnect(ctx context.Context, name string) error
	// MCPConfigure saves a server's enabled state or exposure and applies it.
	MCPConfigure(name string, patch mcp.ConfigPatch) error
}

var errNoMCP = errors.New("no MCP servers are configured")

func (s *agentSession) MCPSignIn(ctx context.Context, name string, prompt mcp.SignInPrompt) error {
	if s.mcp == nil {
		return errNoMCP
	}
	return s.mcp.SignIn(ctx, name, prompt)
}

func (s *agentSession) MCPSignOut(ctx context.Context, name string) (bool, error) {
	if s.mcp == nil {
		return false, errNoMCP
	}
	return s.mcp.SignOut(ctx, name)
}

func (s *agentSession) MCPReconnect(ctx context.Context, name string) error {
	if s.mcp == nil {
		return errNoMCP
	}
	return s.mcp.Reconnect(ctx, name)
}

func (s *agentSession) MCPConfigure(name string, patch mcp.ConfigPatch) error {
	if s.mcp == nil {
		return errNoMCP
	}
	return s.mcp.Configure(name, patch)
}

func (s *agentSession) MCPServers() ([]mcp.ServerState, []string) {
	if s.mcp == nil {
		return nil, slices.Clone(s.mcpErrors)
	}
	return s.mcp.States(), slices.Clone(s.mcpErrors)
}

func (s *agentSession) OnMCPChange(fn func()) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mcpListenerSeq++
	id := s.mcpListenerSeq
	if s.mcpListeners == nil {
		s.mcpListeners = map[int]func(){}
	}
	s.mcpListeners[id] = fn
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.mcpListeners, id)
	}
}

func (s *agentSession) mcpChanged() {
	s.syncMCPTools()
	s.mu.Lock()
	listeners := slices.Collect(maps.Values(s.mcpListeners))
	s.mu.Unlock()
	for _, fn := range listeners {
		fn()
	}
}
