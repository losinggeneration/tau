package sdk

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/agent/codemode"
	"github.com/losinggeneration/tau/agent/mcp"
	"github.com/losinggeneration/tau/paths"
	"github.com/losinggeneration/tau/ui"
)

// mcpStartupWait bounds how long the first prompt waits for servers that can
// expose tools through direct declarations or active discovery.
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
	s.activateDiscovery(cfg)
	s.agent.SetSystemSection(mcpServersSection, renderMCPServersSection(s.mcp.States()))
	s.mcp.Start()
}

// mcpServersSection names the system prompt section listing the servers
// whose tools aren't declared to the model.
const mcpServersSection = "mcp_servers"

const (
	maxServerDescriptionChars = 250
	maxServersSectionChars    = 4096
)

// indirectMCP reports a server with tools reached through discovery.
func indirectMCP(exposures []mcp.Exposure) bool {
	return slices.Contains(exposures, mcp.ExposureCodemode) || slices.Contains(exposures, mcp.ExposureDeferred)
}

func (s *agentSession) activateDiscovery(cfg mcp.Config) {
	var needsCodemode, needsSearch bool
	for _, entry := range cfg.Servers {
		if !entry.Config.IsEnabled() {
			continue
		}
		exposures := entry.Config.Exposures()
		needsCodemode = needsCodemode || slices.Contains(exposures, mcp.ExposureCodemode)
		needsSearch = needsSearch || slices.Contains(exposures, mcp.ExposureDeferred)
	}
	if !needsCodemode && !needsSearch {
		return
	}
	auto := cfg.AutoEnableCodemode == nil || *cfg.AutoEnableCodemode
	tools := s.agent.State().Tools
	active := s.agent.ActiveTools()
	var hasCodemode, reachable, changed bool
	for i, tool := range tools {
		if tool.ReachesTools == nil && tool.ReachTools == nil {
			continue
		}
		if tool.Name != codemode.ToolName && tool.Name != toolSearchName {
			continue
		}
		hasCodemode = hasCodemode || tool.Name == codemode.ToolName
		activate := tool.Name == codemode.ToolName && needsCodemode && auto || tool.Name == toolSearchName && needsSearch
		if activate && !slices.Contains(active, tool.Name) {
			active = append(active, tool.Name)
			tools[i].DefaultActive = nil
			changed = true
		}
		reachable = reachable || slices.Contains(active, tool.Name)
	}
	if changed {
		// Keep late direct registrations and restored deferred declarations;
		// an exact startup loadout would replace them.
		s.agent.SetTools(tools...)
	}
	if reachable {
		return
	}
	reason := ""
	if needsCodemode && hasCodemode && !auto {
		reason = " (autoEnableCodemode is false)"
	}
	s.ui.Notify(ui.NotifyWarn, "MCP tools are only reachable from the codemode or tool_search tool, but neither is active"+reason+"; they cannot be called.")
}

func (s *agentSession) discoveryActive() bool {
	active := s.agent.ActiveTools()
	return slices.ContainsFunc(s.agent.State().Tools, func(tool agent.Tool) bool {
		return (tool.Name == codemode.ToolName || tool.Name == toolSearchName) && (tool.ReachesTools != nil || tool.ReachTools != nil) && (slices.Contains(active, tool.Name) || tool.DefaultActive == nil || *tool.DefaultActive)
	})
}

// renderMCPServersSection lists every enabled server with codemode or
// deferred tools and a one-line summary, so the model learns of servers no
// declaration names. Nil when there are none.
func renderMCPServersSection(states []mcp.ServerState) *string {
	listed := slices.DeleteFunc(slices.Clone(states), func(state mcp.ServerState) bool {
		return state.Status == mcp.StatusDisabled || !indirectMCP(state.Exposures)
	})
	if len(listed) == 0 {
		return nil
	}
	slices.SortFunc(listed, func(a, b mcp.ServerState) int { return strings.Compare(a.Name, b.Name) })
	intro := "MCP servers whose tools are not declared to you."
	if slices.ContainsFunc(listed, func(state mcp.ServerState) bool { return slices.Contains(state.Exposures, mcp.ExposureCodemode) }) {
		intro += " Call the tools of `codemode` servers from codemode scripts."
	}
	if slices.ContainsFunc(listed, func(state mcp.ServerState) bool { return !slices.Contains(state.Exposures, mcp.ExposureCodemode) }) {
		intro += " Load the tools of `tool_search` servers with `tool_search`."
	}
	heads := make([]string, len(listed))
	for i, state := range listed {
		reach := "tool_search"
		if slices.Contains(state.Exposures, mcp.ExposureCodemode) {
			reach = "codemode"
		}
		heads[i] = "- " + mcp.Namespace(state.Name) + " (" + reach + ")"
	}
	omitted := func(count int) []string {
		switch count {
		case 0:
			return nil
		case 1:
			return []string{"- … 1 more server; find their tools with searchTools()"}
		}
		return []string{fmt.Sprintf("- … %d more servers; find their tools with searchTools()", count)}
	}
	size := func(kept int) int {
		return utf8.RuneCountInString(strings.Join(append(append([]string{intro}, heads[:kept]...), omitted(len(listed)-kept)...), "\n"))
	}
	kept := len(listed)
	for kept > 0 && size(kept) > maxServersSectionChars {
		kept--
	}
	perServer := 0
	if kept > 0 {
		perServer = min(maxServerDescriptionChars, (maxServersSectionChars-size(kept))/kept-2)
	}
	lines := []string{intro}
	for i, state := range listed[:kept] {
		summary := ""
		if perServer > 0 {
			summary = truncateRunes(state.Summary, perServer)
		}
		if summary != "" {
			lines = append(lines, heads[i]+": "+summary)
		} else {
			lines = append(lines, heads[i])
		}
	}
	section := strings.Join(append(lines, omitted(len(listed)-kept)...), "\n")
	return &section
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit <= 1 {
		return ""
	}
	return strings.TrimRight(string(runes[:limit-1]), " \t\n") + "…"
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
		// Discovery sees a request's tool snapshot, so the first request
		// waits for every server when a discovery tool is active.
		waitFor := s.mcp.WaitDirect
		if s.discoveryActive() {
			waitFor = s.mcp.WaitAll
		}
		if err := waitFor(wait); errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
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
	s.agent.SetSystemSection(mcpServersSection, renderMCPServersSection(s.mcp.States()))
	s.mu.Lock()
	listeners := slices.Collect(maps.Values(s.mcpListeners))
	s.mu.Unlock()
	for _, fn := range listeners {
		fn()
	}
}
