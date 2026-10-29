package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/agent/mcp"
	tauui "github.com/losinggeneration/tau/tui"
)

type mcpFakeSession struct {
	fakeSession
	mu        sync.Mutex
	servers   []mcp.ServerState
	errors    []string
	listeners map[int]func()
	next      int
	calls     []string
	signIn    func(context.Context, mcp.SignInPrompt) error
}

func (s *mcpFakeSession) MCPServers() ([]mcp.ServerState, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mcp.ServerState(nil), s.servers...), s.errors
}

func (s *mcpFakeSession) OnMCPChange(fn func()) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := s.next
	s.listeners[id] = fn
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.listeners, id)
	}
}

func (s *mcpFakeSession) MCPSignIn(ctx context.Context, name string, prompt mcp.SignInPrompt) error {
	s.mu.Lock()
	s.calls = append(s.calls, "signin "+name)
	s.mu.Unlock()
	prompt.ShowURL("https://auth.example/authorize?x=1")
	if s.signIn != nil {
		return s.signIn(ctx, prompt)
	}
	return nil
}

func (s *mcpFakeSession) MCPSignOut(_ context.Context, name string) (bool, error) {
	s.mu.Lock()
	s.calls = append(s.calls, "signout "+name)
	servers := slices.Clone(s.servers)
	s.mu.Unlock()
	for i := range servers {
		if servers[i].Name == name {
			servers[i].Status, servers[i].Tools = mcp.StatusNeedsAuth, nil
		}
	}
	s.set(servers...)
	return true, nil
}

func (s *mcpFakeSession) MCPReconnect(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "reconnect "+name)
	for i := range s.servers {
		if s.servers[i].Name == name {
			s.servers[i].Status, s.servers[i].Tools = mcp.StatusConnected, mcpTools(2)
		}
	}
	return nil
}

func (s *mcpFakeSession) MCPConfigure(name string, patch mcp.ConfigPatch) error {
	s.mu.Lock()
	call := "configure " + name
	servers := slices.Clone(s.servers)
	s.mu.Unlock()
	for i := range servers {
		if servers[i].Name != name {
			continue
		}
		if patch.Exposure != "" {
			call += " " + string(patch.Exposure)
			servers[i].Exposure = patch.Exposure
		}
		if patch.Enabled != nil {
			call += fmt.Sprintf(" enabled=%v project=%v", *patch.Enabled, patch.Project)
			servers[i].Status = mcp.StatusDisabled
			if *patch.Enabled {
				servers[i].Status = mcp.StatusConnected
			}
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	s.set(servers...)
	return nil
}

func mcpTools(n int) []mcp.ToolState {
	tools := make([]mcp.ToolState, n)
	for i := range tools {
		tools[i] = mcp.ToolState{Name: fmt.Sprintf("t%d", i), Exposure: mcp.ExposureDirect}
	}
	return tools
}

func (s *mcpFakeSession) set(servers ...mcp.ServerState) {
	s.mu.Lock()
	s.servers = servers
	var listeners []func()
	for _, fn := range s.listeners {
		listeners = append(listeners, fn)
	}
	s.mu.Unlock()
	for _, fn := range listeners {
		fn()
	}
}

func (s *mcpFakeSession) listenerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.listeners)
}

func TestMCPManagerOpensImmediatelyAndFollowsConnections(t *testing.T) {
	a, be := nativeDialogApp(t)
	sess := &mcpFakeSession{
		listeners: map[int]func(){},
		errors:    []string{"bad entry"},
		servers: []mcp.ServerState{
			{Name: "docs", Scope: mcp.ScopeGlobal, Status: mcp.StatusConnecting, Exposure: mcp.ExposureDirect},
			{Name: "api", Scope: mcp.ScopeProject, Status: mcp.StatusConnecting, Exposure: mcp.ExposureCodemode},
		},
	}
	a.sess = sess
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	descriptions := func() []string {
		var out []string
		for _, item := range a.dialogPicker.Items() {
			out = append(out, item.Name+": "+item.Description)
		}
		return out
	}
	header := "MCP servers\nconfig: bad entry"

	require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) { cmdMCP(a, "") }))
	onFocusedDialog(t, a, header, func(*tauui.UpdateCtx) {
		assert.Equal(t, []string{
			"api: connecting… · codemode · project",
			"docs: connecting… · direct · global",
		}, descriptions())
		go sess.set(
			mcp.ServerState{Name: "docs", Scope: mcp.ScopeGlobal, Override: "/p/.tau/mcp.json", Status: mcp.StatusConnected, Exposure: mcp.ExposureDirect, Tools: mcpTools(1), Resources: true},
			mcp.ServerState{Name: "api", Scope: mcp.ScopeProject, Status: mcp.StatusFailed, Error: "connection refused\nmore", Exposure: mcp.ExposureCodemode},
			mcp.ServerState{Name: "zauth", Scope: mcp.ScopeGlobal, Status: mcp.StatusNeedsAuth, Exposure: mcp.ExposureDeferred},
		)
		var updated func(*tauui.UpdateCtx)
		updated = func(*tauui.UpdateCtx) {
			if items := a.dialogPicker.Items(); len(items) == 0 || items[0].Description == "connecting… · codemode · project" {
				require.NoError(t, a.app.Post(updated))
				return
			}
			assert.Equal(t, []string{
				"zauth: needs sign-in · deferred · global",
				"api: failed: connection refused · codemode · project",
				"docs: connected · 1 tool · resources · direct · global, project override",
			}, descriptions())
			be.SendEvent(tauui.KeyEvent{Key: tauui.KeyEsc})
			var closed func(*tauui.UpdateCtx)
			closed = func(*tauui.UpdateCtx) {
				if a.dialogPicker.Shown() {
					require.NoError(t, a.app.Post(closed))
					return
				}
				a.app.Quit()
			}
			require.NoError(t, a.app.Post(closed))
		}
		require.NoError(t, a.app.Post(updated))
	})
	require.NoError(t, a.Run(ctx))
	assert.Zero(t, sess.listenerCount(), "closing /mcp leaves its listener subscribed")
}

func TestMCPManagerShowsEmptyConfigAndUsage(t *testing.T) {
	t.Setenv("TAU_CONFIG_DIR", "/cfg")
	a := newTestApp(t)
	a.sess = &mcpFakeSession{listeners: map[int]func(){}}
	cmdMCP(a, "")
	assert.Contains(t, a.conv.Messages()[len(a.conv.Messages())-1].Text, "No MCP servers configured. Add them to /cfg/mcp.json or .tau/mcp.json.")
	cmdMCP(a, "bogus")
	assert.Equal(t, mcpUsage, a.conv.Messages()[len(a.conv.Messages())-1].Text)
}

func TestFormatMCPStatusPointsAtSignIn(t *testing.T) {
	sess := &mcpFakeSession{servers: []mcp.ServerState{
		{Name: "docs", Status: mcp.StatusNeedsAuth, Exposure: mcp.ExposureDirect},
		{Name: "api", Status: mcp.StatusConnected, Exposure: mcp.ExposureCodemode, Tools: mcpTools(2)},
	}}
	assert.Equal(t, "docs: needs sign-in, run /mcp login docs (direct)\napi: connected, 2 tools (codemode)", formatMCPStatus(sess))
}

// runUntilMessage runs the app until a chat message contains want.
func runUntilMessage(t *testing.T, a *App, start func(), want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) { start() }))
	var check func(*tauui.UpdateCtx)
	check = func(*tauui.UpdateCtx) {
		for _, m := range a.conv.Messages() {
			if strings.Contains(m.Text, want) {
				a.app.Quit()
				return
			}
		}
		require.NoError(t, a.app.Post(check))
	}
	require.NoError(t, a.app.Post(check))
	require.NoError(t, a.Run(ctx))
}

func TestMCPLoginSignsInOpensBrowserAndReconnects(t *testing.T) {
	a, _ := nativeDialogApp(t)
	var opened []string
	a.openURL = func(u string) error { opened = append(opened, u); return nil }
	sess := &mcpFakeSession{listeners: map[int]func(){}, servers: []mcp.ServerState{
		{Name: "docs", OAuth: true, Status: mcp.StatusNeedsAuth},
		{Name: "local", Status: mcp.StatusConnected},
	}}
	a.sess = sess
	runUntilMessage(t, a, func() { cmdMCP(a, "login") }, `Signed in to MCP server "docs" (2 tools).`)
	assert.Equal(t, []string{"signin docs", "reconnect docs"}, sess.calls)
	assert.Equal(t, []string{"https://auth.example/authorize?x=1"}, opened)
	var texts []string
	for _, m := range a.conv.Messages() {
		texts = append(texts, m.Text)
	}
	assert.Contains(t, texts, "Sign in to MCP server \"docs\" in your browser:\nhttps://auth.example/authorize?x=1")
}

func TestMCPSubcommandsReportUpstreamMessages(t *testing.T) {
	for _, tc := range []struct{ args, want string }{
		{"logout docs", `Signed out of MCP server "docs".`},
		{"logout local", mcpNoOAuth},
		{"reconnect nope", `No MCP server named "nope".`},
		{"reconnect local", `Reconnected to MCP server "local" (connected · 2 tools).`},
		{"login docs extra", mcpUsage},
		{"bogus", mcpUsage},
	} {
		t.Run(tc.args, func(t *testing.T) {
			a, _ := nativeDialogApp(t)
			a.sess = &mcpFakeSession{listeners: map[int]func(){}, servers: []mcp.ServerState{
				{Name: "docs", OAuth: true, Status: mcp.StatusConnected},
				{Name: "local", Status: mcp.StatusFailed},
			}}
			runUntilMessage(t, a, func() { cmdMCP(a, tc.args) }, tc.want)
		})
	}
}

func TestMCPLoginReportsCancelAndFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{mcp.ErrSignInCancelled, "Sign-in cancelled."},
		{errors.New("token exchange failed"), "Sign-in failed: token exchange failed"},
	} {
		a, _ := nativeDialogApp(t)
		a.sess = &mcpFakeSession{
			listeners: map[int]func(){}, servers: []mcp.ServerState{{Name: "docs", OAuth: true, Status: mcp.StatusNeedsAuth}},
			signIn: func(context.Context, mcp.SignInPrompt) error { return tc.err },
		}
		runUntilMessage(t, a, func() { cmdMCP(a, "login docs") }, tc.want)
	}
}

func TestCompleteMCPArgs(t *testing.T) {
	a := newTestApp(t)
	a.sess = &mcpFakeSession{servers: []mcp.ServerState{
		{Name: "docs", OAuth: true, Status: mcp.StatusNeedsAuth},
		{Name: "dev", Status: mcp.StatusConnected, Tools: mcpTools(1)},
		{Name: "off", OAuth: true, Status: mcp.StatusDisabled},
	}}
	values := func(prefix string) []string {
		var out []string
		for _, item := range completeMCPArgs(a, prefix) {
			out = append(out, item.Value+"|"+item.Description)
		}
		return out
	}
	assert.Equal(t, []string{"login |", "logout |", "reconnect |"}, values(""))
	assert.Equal(t, []string{"login |", "logout |"}, values("lo"))
	assert.Equal(t, []string{"login docs|needs sign-in"}, values("login "))
	assert.Equal(t, []string{"reconnect docs|needs sign-in", "reconnect dev|connected · 1 tool"}, values("reconnect d"))
	assert.Nil(t, values("login docs more"))
	assert.Nil(t, values("bogus "))

	sel := a.commandArgItems("/mcp log", len("/mcp log"))
	require.Len(t, sel.Items, 2)
	res, ok := sel.Apply([]string{"/mcp log"}, 0, len("/mcp log"), 0, "login")
	require.True(t, ok)
	assert.Equal(t, "/mcp login ", res.Lines[0])
}

func TestMCPServerMenuOffersActionsForItsState(t *testing.T) {
	a, be := nativeDialogApp(t)
	sess := &mcpFakeSession{listeners: map[int]func(){}, servers: []mcp.ServerState{{
		Name: "docs", Scope: mcp.ScopeGlobal, Source: "/a/mcp.json", Transport: "https://docs.example/mcp", ProjectConfig: "/p/.tau/mcp.json",
		OAuth: true, Status: mcp.StatusConnected, Tools: []mcp.ToolState{{Name: "search", Description: "Search.\nMore.", Exposure: mcp.ExposureDirect}, {Name: "drop", Exposure: mcp.ExposureHidden}},
		Exposure: mcp.ExposureDirect,
	}}}
	a.sess = sess
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	rows := func() []string {
		var out []string
		for _, item := range a.dialogPicker.Items() {
			out = append(out, strings.TrimSpace(item.Name+" "+item.Description))
		}
		return out
	}
	choose := func(i int) {
		a.dialogPicker.SetFocused(i)
		be.SendEvent(tauui.KeyEvent{Key: tauui.KeyEnter})
	}
	server := func(state string) string {
		return "MCP server docs\nhttps://docs.example/mcp\nglobal: /a/mcp.json\nState: " + state
	}
	require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) { cmdMCP(a, "") }))
	onFocusedDialog(t, a, mcpManagerTitle, func(*tauui.UpdateCtx) {
		be.SendEvent(tauui.KeyEvent{Key: tauui.KeyEnter})
		onFocusedDialog(t, a, server("connected · 2 tools"), func(*tauui.UpdateCtx) {
			assert.Equal(t, []string{
				"Tools 2 offered", "Reconnect", "Sign out deletes the stored credentials", "Exposure direct",
				"Disable saved to the global mcp.json", "Disable in this project saved to the project mcp.json",
			}, rows())
			choose(0)
			onFocusedDialog(t, a, "Tools of docs\nExposure direct: declared to the model like built-in tools\nSome tools override it with toolExposure.", func(*tauui.UpdateCtx) {
				assert.Equal(t, []string{"search Search.", "drop [hidden]"}, rows())
				be.SendEvent(tauui.KeyEvent{Key: tauui.KeyEsc})
				onFocusedDialog(t, a, server("connected · 2 tools"), func(*tauui.UpdateCtx) {
					choose(3)
					onFocusedDialog(t, a, "Exposure of docs\nSaved to /a/mcp.json.", func(*tauui.UpdateCtx) {
						assert.Equal(t, 2, a.dialogPicker.Focused(), "the current exposure is focused")
						assert.Equal(t, "✓ direct", a.dialogPicker.Items()[2].Name)
						choose(1)
						onFocusedDialog(t, a, server("connected · 2 tools"), func(*tauui.UpdateCtx) {
							assert.Contains(t, rows(), "Exposure deferred")
							choose(2)
							onFocusedDialog(t, a, server("needs sign-in"), func(*tauui.UpdateCtx) {
								assert.Equal(t, []string{"Sign in opens the browser", "Reconnect", "Exposure deferred", "Disable saved to the global mcp.json", "Disable in this project saved to the project mcp.json"}, rows())
								choose(4)
								onFocusedDialog(t, a, server("disabled"), func(*tauui.UpdateCtx) {
									assert.Equal(t, []string{"Enable saved to the global mcp.json", "Enable in this project saved to the project mcp.json"}, rows())
									a.app.Quit()
								})
							})
						})
					})
				})
			})
		})
	})
	require.NoError(t, a.Run(ctx))
	assert.Equal(t, []string{"configure docs deferred", "signout docs", "configure docs enabled=false project=true"}, sess.calls)
}
