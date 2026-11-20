package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/losinggeneration/tau/agent/mcp"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
)

// errNotSupported marks a session lacking an optional capability; replies
// carry the not_supported code.
var errNotSupported = errors.New("not supported")

// sessionAs returns the target session (see draftSession) as T.
func sessionAs[T any](ctx context.Context, s *Server, expected, what string) (sdk.Session, T, error) {
	var zero T
	sess, err := s.draftSession(ctx, expected)
	if err != nil {
		return nil, zero, err
	}
	capable, ok := sess.(T)
	if !ok {
		return nil, zero, fmt.Errorf("%w: this session doesn't support %s", errNotSupported, what)
	}
	return sess, capable, nil
}

// unmarshalArgs accepts omitted args for commands whose fields are optional.
func unmarshalArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// handleGetForkEntries lists the current branch's fork points, oldest first.
func (s *Server) handleGetForkEntries(ctx context.Context, cmd *Command) error {
	var args struct {
		Session string `json:"expected_session_id"`
	}
	if err := unmarshalArgs(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	_, lister, err := sessionAs[sdk.ForkEntryLister](ctx, s, args.Session, "forking")
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	entries, err := lister.GetForkEntries()
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	type entry struct {
		EntryID string `json:"entry_id"`
		Role    string `json:"role"`
		Text    string `json:"text"`
	}
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, entry{e.EntryID, e.Role, e.Text})
	}
	return s.sendResult(cmd.ID, map[string]any{"entries": out})
}

// handleGetScopedModels reports the enabledModels patterns and the models
// they select. Null patterns mean every model is in scope.
func (s *Server) handleGetScopedModels(ctx context.Context, cmd *Command) error {
	sess, provider, err := sessionAs[sdk.SettingsProvider](ctx, s, "", "settings")
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	type scoped struct {
		Model         string `json:"model"`
		ThinkingLevel string `json:"thinking_level"`
	}
	var patterns []string
	if settings := provider.Settings(); settings != nil {
		patterns = slices.Clone(settings.EnabledModels)
	}
	out := []scoped{}
	if len(patterns) > 0 {
		models, err := availableModels(ctx, sess)
		if err != nil {
			return s.sendErrorResult(cmd.ID, err)
		}
		keys := make([]string, 0, len(models))
		for _, m := range models {
			if m != nil && m.ID != "" {
				keys = append(keys, m.Provider+"/"+m.ID)
			}
		}
		for _, m := range ai.ExpandModelScope(patterns, keys) {
			out = append(out, scoped{Model: m.Model.String(), ThinkingLevel: m.ThinkingLevel})
		}
	}
	return s.sendResult(cmd.ID, map[string]any{"patterns": patterns, "models": out})
}

// handleGetPlugins lists plugins and their tools as /plugins shows them.
func (s *Server) handleGetPlugins(ctx context.Context, cmd *Command) error {
	_, toggler, err := sessionAs[sdk.PluginToggler](ctx, s, "", "plugins")
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	type entry struct {
		Name    string `json:"name"`
		Plugin  string `json:"plugin"`
		Scope   string `json:"scope"`
		Enabled bool   `json:"enabled"`
		Loaded  bool   `json:"loaded"`
	}
	states := toggler.PluginStates()
	out := make([]entry, 0, len(states))
	for _, st := range states {
		out = append(out, entry{Name: st.Name, Plugin: st.Plugin, Scope: string(st.Scope), Enabled: st.Enabled, Loaded: st.Loaded})
	}
	return s.sendResult(cmd.ID, map[string]any{"entries": out})
}

func (s *Server) handleEnablePlugin(ctx context.Context, cmd *Command) error {
	return s.togglePlugin(ctx, cmd, sdk.PluginToggler.EnablePlugin)
}

func (s *Server) handleDisablePlugin(ctx context.Context, cmd *Command) error {
	return s.togglePlugin(ctx, cmd, sdk.PluginToggler.DisablePlugin)
}

func (s *Server) togglePlugin(ctx context.Context, cmd *Command, toggle func(sdk.PluginToggler, context.Context, string, ai.ToggleScope) error) error {
	var args struct {
		Session string `json:"expected_session_id"`
		Name    string `json:"name"`
		Scope   string `json:"scope,omitempty"`
	}
	if err := unmarshalArgs(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if args.Name == "" {
		return s.sendError(cmd.ID, "invalid_args", "name is required")
	}
	scope := ai.ToggleScope(cmp.Or(args.Scope, string(ai.ToggleSession)))
	switch scope {
	case ai.ToggleSession, ai.ToggleProject, ai.ToggleGlobal:
	default:
		return s.sendError(cmd.ID, "invalid_args", fmt.Sprintf("invalid scope %q: use session, project or global", args.Scope))
	}
	_, toggler, err := sessionAs[sdk.PluginToggler](ctx, s, args.Session, "plugins")
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if err := toggle(toggler, ctx, args.Name, scope); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	return s.sendResult(cmd.ID, map[string]any{"ok": true})
}

// handleGetMCPServers snapshots the configured MCP servers.
func (s *Server) handleGetMCPServers(ctx context.Context, cmd *Command) error {
	_, status, err := sessionAs[sdk.MCPStatus](ctx, s, "", "MCP servers")
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	type tool struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Exposure    string `json:"exposure"`
	}
	type mcpServer struct {
		Name      string   `json:"name"`
		Scope     string   `json:"scope"`
		Source    string   `json:"source"`
		Transport string   `json:"transport"`
		OAuth     bool     `json:"oauth"`
		Status    string   `json:"status"`
		Error     string   `json:"error"`
		Exposure  string   `json:"exposure"`
		Exposures []string `json:"exposures"`
		Summary   string   `json:"summary"`
		ToolCount int      `json:"tool_count"`
		Tools     []tool   `json:"tools"`
	}
	states, configErrors := status.MCPServers()
	out := make([]mcpServer, 0, len(states))
	for _, st := range states {
		exposures := make([]string, 0, len(st.Exposures))
		for _, e := range st.Exposures {
			exposures = append(exposures, string(e))
		}
		tools := make([]tool, 0, len(st.Tools))
		for _, t := range st.Tools {
			tools = append(tools, tool{Name: t.Name, Description: t.Description, Exposure: string(t.Exposure)})
		}
		out = append(out, mcpServer{
			Name:      st.Name,
			Scope:     string(st.Scope),
			Source:    st.Source,
			Transport: st.Transport,
			OAuth:     st.OAuth,
			Status:    string(st.Status),
			Error:     st.Error,
			Exposure:  string(st.Exposure),
			Exposures: exposures,
			Summary:   st.Summary,
			ToolCount: len(st.Tools),
			Tools:     tools,
		})
	}
	if configErrors == nil {
		configErrors = []string{}
	}
	return s.sendResult(cmd.ID, map[string]any{"servers": out, "config_errors": configErrors})
}

type mcpNameArgs struct {
	Session string `json:"expected_session_id"`
	Name    string `json:"name"`
}

func (s *Server) handleMCPReconnect(ctx context.Context, cmd *Command) error {
	var args mcpNameArgs
	if err := unmarshalArgs(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if args.Name == "" {
		return s.sendError(cmd.ID, "invalid_args", "name is required")
	}
	_, status, err := sessionAs[sdk.MCPStatus](ctx, s, args.Session, "MCP servers")
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if err := status.MCPReconnect(ctx, args.Name); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	return s.sendResult(cmd.ID, map[string]any{"ok": true})
}

func (s *Server) handleMCPConfigure(ctx context.Context, cmd *Command) error {
	var args struct {
		Session  string `json:"expected_session_id"`
		Name     string `json:"name"`
		Enabled  *bool  `json:"enabled,omitempty"`
		Exposure string `json:"exposure,omitempty"`
	}
	if err := unmarshalArgs(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if args.Name == "" || (args.Enabled == nil && args.Exposure == "") {
		return s.sendError(cmd.ID, "invalid_args", "name and enabled or exposure are required")
	}
	switch mcp.Exposure(args.Exposure) {
	case "", mcp.ExposureCodemode, mcp.ExposureDeferred, mcp.ExposureDirect, mcp.ExposureHidden:
	default:
		return s.sendError(cmd.ID, "invalid_args", fmt.Sprintf("invalid exposure %q: use codemode, deferred, direct or hidden", args.Exposure))
	}
	_, status, err := sessionAs[sdk.MCPStatus](ctx, s, args.Session, "MCP servers")
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if err := status.MCPConfigure(args.Name, mcp.ConfigPatch{Enabled: args.Enabled, Exposure: mcp.Exposure(args.Exposure)}); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	return s.sendResult(cmd.ID, map[string]any{"ok": true})
}

func (s *Server) handleMCPSignOut(ctx context.Context, cmd *Command) error {
	var args mcpNameArgs
	if err := unmarshalArgs(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if args.Name == "" {
		return s.sendError(cmd.ID, "invalid_args", "name is required")
	}
	_, status, err := sessionAs[sdk.MCPStatus](ctx, s, args.Session, "MCP servers")
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	removed, err := status.MCPSignOut(ctx, args.Name)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	return s.sendResult(cmd.ID, map[string]any{"removed": removed})
}

// handleMCPSignIn replies at once and signs in off the input loop, since the
// browser flow can take minutes. Progress arrives as mcp_auth_url and
// mcp_sign_in_end events; the session context ends a sign-in left running.
func (s *Server) handleMCPSignIn(ctx context.Context, cmd *Command) error {
	var args mcpNameArgs
	if err := unmarshalArgs(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if args.Name == "" {
		return s.sendError(cmd.ID, "invalid_args", "name is required")
	}
	sess, status, err := sessionAs[sdk.MCPStatus](ctx, s, args.Session, "MCP servers")
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	s.sessionMu.Lock()
	signInCtx := s.sessionCtx
	s.sessionMu.Unlock()
	if signInCtx == nil {
		signInCtx = ctx
	}
	name := args.Name
	// Remote clients that only read snapshots see the link in the input
	// request rather than the mcp_auth_url event.
	var authURL atomic.Pointer[string]
	prompt := mcp.SignInPrompt{
		ShowURL: func(url string) {
			authURL.Store(&url)
			event := sdk.Event{Type: "mcp_auth_url", SessionID: sess.ID(), Payload: map[string]any{"name": name, "url": url}}
			if err := s.emitEvent(event); err != nil {
				workerLog("publish MCP sign-in URL: %v", err)
			}
		},
	}
	if s.ui != nil {
		prompt.RedirectURL = func(ctx context.Context) (string, error) {
			title := fmt.Sprintf("Waiting for sign-in to %q. If the browser cannot reach the Tau host, paste the URL it was redirected to.", name)
			if url := authURL.Load(); url != nil {
				title = fmt.Sprintf("Sign in to %q at %s, then paste the URL the browser was redirected to if it cannot reach the Tau host.", name, *url)
			}
			value, err := s.ui.Input(ctx, title, "http://127.0.0.1:.../callback?code=...")
			if errors.Is(err, ui.ErrNoInput) {
				return "", nil
			}
			return value, err
		}
	}
	if err := s.sendResult(cmd.ID, map[string]any{"ok": true, "started": true}); err != nil {
		return err
	}
	s.promptWG.Go(func() {
		err := status.MCPSignIn(signInCtx, name, prompt)
		if err == nil {
			if reconnectErr := status.MCPReconnect(signInCtx, name); reconnectErr != nil {
				err = fmt.Errorf("signed in, but reconnect failed: %w", reconnectErr)
			}
		}
		payload := map[string]any{"name": name, "ok": err == nil}
		if err != nil {
			payload["error"] = err.Error()
		}
		if emitErr := s.emitEvent(sdk.Event{Type: "mcp_sign_in_end", SessionID: sess.ID(), Payload: payload}); emitErr != nil {
			workerLog("publish MCP sign-in result: %v", emitErr)
		}
	})
	return nil
}
