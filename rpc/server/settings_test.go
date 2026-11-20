package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/losinggeneration/tau/agent/mcp"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
)

// settingsMockSession implements the optional interfaces the settings
// commands read; the embedded mockSession implements none of them.
type settingsMockSession struct {
	*mockSession
	settings       *ai.Settings
	models         []*ai.Model
	toggles        []string
	mcpCalls       []string
	patch          mcp.ConfigPatch
	signInErr      error
	signInRelease  chan struct{}
	autoCompaction bool
	autoRetry      bool
	steeringMode   string
	followUpMode   string
	thinkingLevel  string
}

func newSettingsMockSession(id string) *settingsMockSession {
	return &settingsMockSession{mockSession: newMockSession(id, "/")}
}

func (s *settingsMockSession) GetForkEntries() ([]sdk.ForkEntry, error) {
	return []sdk.ForkEntry{{EntryID: "e1", Role: "user", Text: "hi"}}, nil
}

func (s *settingsMockSession) Settings() *ai.Settings { return s.settings }

func (s *settingsMockSession) GetAvailableModels(context.Context) ([]*ai.Model, error) {
	return s.models, nil
}

func (s *settingsMockSession) Stats() sdk.SessionStats {
	return sdk.SessionStats{SessionID: s.id, ThinkingLevel: s.thinkingLevel}
}

func (s *settingsMockSession) AutoCompactionEnabled() bool { return s.autoCompaction }
func (s *settingsMockSession) AutoRetryEnabled() bool      { return s.autoRetry }
func (s *settingsMockSession) SteeringMode() string        { return s.steeringMode }
func (s *settingsMockSession) FollowUpMode() string        { return s.followUpMode }

func (s *settingsMockSession) PluginStates() []sdk.PluginToggleState {
	return []sdk.PluginToggleState{{Name: "tool", Plugin: "p", Scope: ai.ToggleProject, Enabled: true}}
}

func (s *settingsMockSession) EnablePlugin(_ context.Context, name string, scope ai.ToggleScope) error {
	s.toggles = append(s.toggles, "enable "+name+" "+string(scope))
	return nil
}

func (s *settingsMockSession) DisablePlugin(_ context.Context, name string, scope ai.ToggleScope) error {
	s.toggles = append(s.toggles, "disable "+name+" "+string(scope))
	return nil
}

func (s *settingsMockSession) MCPServers() ([]mcp.ServerState, []string) {
	return []mcp.ServerState{{
		Name:      "docs",
		Scope:     mcp.ScopeGlobal,
		Source:    "/cfg/mcp.json",
		Transport: "https://docs.example/mcp",
		OAuth:     true,
		Status:    mcp.StatusConnected,
		Exposure:  mcp.ExposureDirect,
		Exposures: []mcp.Exposure{mcp.ExposureDirect, mcp.ExposureHidden},
		Summary:   "Docs search",
		Tools:     []mcp.ToolState{{Name: "search", Description: "Search docs", Exposure: mcp.ExposureDirect}},
	}}, []string{"bad entry"}
}

func (s *settingsMockSession) OnMCPChange(func()) func() { return func() {} }

func (s *settingsMockSession) MCPSignIn(_ context.Context, name string, prompt mcp.SignInPrompt) error {
	prompt.ShowURL("https://auth.example/" + name)
	if s.signInRelease != nil {
		<-s.signInRelease
	}
	s.mcpCalls = append(s.mcpCalls, "sign-in "+name)
	return s.signInErr
}

func (s *settingsMockSession) MCPSignOut(_ context.Context, name string) (bool, error) {
	s.mcpCalls = append(s.mcpCalls, "sign-out "+name)
	return true, nil
}

func (s *settingsMockSession) MCPReconnect(_ context.Context, name string) error {
	s.mcpCalls = append(s.mcpCalls, "reconnect "+name)
	return nil
}

func (s *settingsMockSession) MCPConfigure(name string, patch mcp.ConfigPatch) error {
	s.mcpCalls = append(s.mcpCalls, "configure "+name)
	s.patch = patch
	return nil
}

type commandReply struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *ErrorInfo      `json:"error"`
}

// runCommand dispatches one command and decodes the last line it wrote.
func runCommand(t *testing.T, s *Server, out *bytes.Buffer, typ, args string) commandReply {
	t.Helper()
	out.Reset()
	if err := s.handleCommand(t.Context(), &Command{ID: typ, Type: typ, Args: json.RawMessage(args)}); err != nil {
		t.Fatal(err)
	}
	var reply commandReply
	if err := json.Unmarshal(lastResponseLine(out.Bytes()), &reply); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	return reply
}

func wantResult(t *testing.T, reply commandReply, want string) {
	t.Helper()
	if !reply.OK || string(reply.Result) != want {
		t.Fatalf("reply = %+v %s, want %s", reply, reply.Result, want)
	}
}

func wantErrorCode(t *testing.T, reply commandReply, code string) {
	t.Helper()
	if reply.OK || reply.Error == nil || reply.Error.Code != code {
		t.Fatalf("reply = %+v, want error %s", reply, code)
	}
}

func TestSettingsCommandsReportNotSupported(t *testing.T) {
	var out bytes.Buffer
	s := &Server{writer: &out, session: newMockSession("s", "/")}
	for _, tc := range []struct{ typ, args string }{
		{"get_fork_entries", `{}`},
		{"get_scoped_models", ``},
		{"get_plugins", `{}`},
		{"enable_plugin", `{"name":"p"}`},
		{"disable_plugin", `{"name":"p"}`},
		{"get_mcp_servers", `{}`},
		{"mcp_reconnect", `{"name":"docs"}`},
		{"mcp_configure", `{"name":"docs","enabled":true}`},
		{"mcp_sign_in", `{"name":"docs"}`},
		{"mcp_sign_out", `{"name":"docs"}`},
		{"get_forget_entries", `{}`},
		{"forget", `{"entry_id":"e1"}`},
	} {
		wantErrorCode(t, runCommand(t, s, &out, tc.typ, tc.args), "not_supported")
	}
}

func TestGetForkEntries(t *testing.T) {
	var out bytes.Buffer
	s := &Server{writer: &out, session: newSettingsMockSession("s")}
	wantResult(t, runCommand(t, s, &out, "get_fork_entries", `{"expected_session_id":"s"}`), `{"entries":[{"entry_id":"e1","role":"user","text":"hi"}]}`)
	wantResult(t, runCommand(t, s, &out, "get_fork_entries", ``), `{"entries":[{"entry_id":"e1","role":"user","text":"hi"}]}`)
	wantErrorCode(t, runCommand(t, s, &out, "get_fork_entries", `{"expected_session_id":"previous"}`), "session_changed")
}

func TestGetScopedModels(t *testing.T) {
	var out bytes.Buffer
	sess := newSettingsMockSession("s")
	sess.models = []*ai.Model{{ID: "m", Provider: "p"}, {ID: "other", Provider: "p"}}
	s := &Server{writer: &out, session: sess}
	wantResult(t, runCommand(t, s, &out, "get_scoped_models", `{}`), `{"models":[],"patterns":null}`)
	sess.settings = &ai.Settings{EnabledModels: []string{"p/m:high"}}
	wantResult(t, runCommand(t, s, &out, "get_scoped_models", `{}`), `{"models":[{"model":"p/m","thinking_level":"high"}],"patterns":["p/m:high"]}`)
}

func TestPluginCommands(t *testing.T) {
	var out bytes.Buffer
	sess := newSettingsMockSession("s")
	s := &Server{writer: &out, session: sess}
	wantResult(t, runCommand(t, s, &out, "get_plugins", `{}`), `{"entries":[{"name":"tool","plugin":"p","scope":"project","enabled":true,"loaded":false}]}`)
	wantResult(t, runCommand(t, s, &out, "enable_plugin", `{"name":"p"}`), `{"ok":true}`)
	wantResult(t, runCommand(t, s, &out, "disable_plugin", `{"name":"p","scope":"global"}`), `{"ok":true}`)
	wantErrorCode(t, runCommand(t, s, &out, "enable_plugin", `{"name":"p","scope":"team"}`), "invalid_args")
	wantErrorCode(t, runCommand(t, s, &out, "disable_plugin", `{}`), "invalid_args")
	if want := []string{"enable p session", "disable p global"}; !slices.Equal(sess.toggles, want) {
		t.Fatalf("toggles = %q, want %q", sess.toggles, want)
	}
}

func TestMCPCommands(t *testing.T) {
	var out bytes.Buffer
	sess := newSettingsMockSession("s")
	s := &Server{writer: &out, session: sess}
	wantResult(t, runCommand(t, s, &out, "get_mcp_servers", `{}`), `{"config_errors":["bad entry"],"servers":[{"name":"docs","scope":"global","source":"/cfg/mcp.json","transport":"https://docs.example/mcp","oauth":true,"status":"connected","error":"","exposure":"direct","exposures":["direct","hidden"],"summary":"Docs search","tool_count":1,"tools":[{"name":"search","description":"Search docs","exposure":"direct"}]}]}`)
	wantResult(t, runCommand(t, s, &out, "mcp_reconnect", `{"name":"docs"}`), `{"ok":true}`)
	wantResult(t, runCommand(t, s, &out, "mcp_configure", `{"name":"docs","enabled":false,"exposure":"hidden"}`), `{"ok":true}`)
	if sess.patch.Enabled == nil || *sess.patch.Enabled || sess.patch.Exposure != mcp.ExposureHidden || sess.patch.Project {
		t.Fatalf("patch = %+v", sess.patch)
	}
	wantErrorCode(t, runCommand(t, s, &out, "mcp_configure", `{"name":"docs","exposure":"loud"}`), "invalid_args")
	wantErrorCode(t, runCommand(t, s, &out, "mcp_configure", `{"name":"docs"}`), "invalid_args")
	wantErrorCode(t, runCommand(t, s, &out, "mcp_reconnect", `{}`), "invalid_args")
	wantResult(t, runCommand(t, s, &out, "mcp_sign_out", `{"name":"docs"}`), `{"removed":true}`)
	if want := []string{"reconnect docs", "configure docs", "sign-out docs"}; !slices.Equal(sess.mcpCalls, want) {
		t.Fatalf("calls = %q, want %q", sess.mcpCalls, want)
	}
}

func TestSettingsMutationsGuardExpectedSession(t *testing.T) {
	for _, tc := range []struct{ typ, fields string }{
		{"enable_plugin", `"name":"p"`},
		{"disable_plugin", `"name":"p"`},
		{"mcp_reconnect", `"name":"docs"`},
		{"mcp_configure", `"name":"docs","enabled":true`},
		{"mcp_sign_in", `"name":"docs"`},
		{"mcp_sign_out", `"name":"docs"`},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			var out bytes.Buffer
			sess := newSettingsMockSession("s")
			s := &Server{writer: &out, session: sess}
			wantErrorCode(t, runCommand(t, s, &out, tc.typ, `{"expected_session_id":"previous",`+tc.fields+`}`), "session_changed")
			s.promptWG.Wait()
			if len(sess.toggles) != 0 || len(sess.mcpCalls) != 0 {
				t.Fatalf("stale command mutated session: toggles %q, calls %q", sess.toggles, sess.mcpCalls)
			}
			cmd := &Command{ID: tc.typ, Type: tc.typ, Args: json.RawMessage(`{"expected_session_id":"s",` + tc.fields + `}`)}
			if err := s.handleCommand(t.Context(), cmd); err != nil {
				t.Fatal(err)
			}
			s.promptWG.Wait()
			if len(sess.toggles) == 0 && len(sess.mcpCalls) == 0 {
				t.Fatalf("matching command did not reach session: %s", &out)
			}
		})
	}
}

type blockingReconnectSession struct {
	*settingsMockSession
	started chan struct{}
}

func (s *blockingReconnectSession) MCPReconnect(ctx context.Context, _ string) error {
	close(s.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestWorkerInterruptCancelsMCPReconnect(t *testing.T) {
	sess := &blockingReconnectSession{settingsMockSession: newSettingsMockSession("session"), started: make(chan struct{})}
	rt := newWorkerRuntime()
	rt.newFn = func(sdk.SessionOptions) (sdk.Session, error) { return sess, nil }
	h := startWorker(t, rt, nil)
	h.send(t, "reconnect", `{"type":"mcp_reconnect","args":{"name":"docs"}}`)
	select {
	case <-sess.started:
	case <-time.After(time.Second):
		t.Fatal("reconnect did not start")
	}
	h.send(t, "abort", `{"type":"interrupt"}`)

	replies := make(map[string]Response, 2)
	for len(replies) < 2 {
		f := h.next(t)
		if f.Type != coord.TypeResponse {
			continue
		}
		var r Response
		if err := json.Unmarshal(f.Result, &r); err != nil {
			t.Fatal(err)
		}
		replies[f.ID] = r
	}
	if r := replies["abort"]; !r.OK {
		t.Fatalf("interrupt reply = %+v", r)
	}
	if r := replies["reconnect"]; r.OK || r.Error == nil || r.Error.Message != context.Canceled.Error() {
		t.Fatalf("reconnect reply = %+v, want context canceled", r)
	}
}

func TestMCPSignInRunsOffDispatchLoop(t *testing.T) {
	var out bytes.Buffer
	sess := newSettingsMockSession("s")
	sess.signInRelease = make(chan struct{})
	s := &Server{writer: &out, session: sess}
	// The mock blocks until released, so a reply proves dispatch didn't wait.
	if err := s.handleCommand(t.Context(), &Command{ID: "in", Type: "mcp_sign_in", Args: json.RawMessage(`{"name":"docs"}`)}); err != nil {
		t.Fatal(err)
	}
	close(sess.signInRelease)
	s.promptWG.Wait()
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("output = %s", &out)
	}
	var reply commandReply
	if err := json.Unmarshal(lines[0], &reply); err != nil {
		t.Fatal(err)
	}
	wantResult(t, reply, `{"ok":true,"started":true}`)
	authURL, end := parseResp(t, string(lines[1])), parseResp(t, string(lines[2]))
	if authURL["type"] != "mcp_auth_url" || authURL["name"] != "docs" || authURL["url"] != "https://auth.example/docs" {
		t.Fatalf("auth event = %v", authURL)
	}
	if _, hasError := end["error"]; end["type"] != "mcp_sign_in_end" || end["name"] != "docs" || end["ok"] != true || hasError {
		t.Fatalf("end event = %v", end)
	}
	if want := []string{"sign-in docs", "reconnect docs"}; !slices.Equal(sess.mcpCalls, want) {
		t.Fatalf("calls = %q, want %q", sess.mcpCalls, want)
	}

	sess.signInRelease = nil
	sess.signInErr = errors.New("denied")
	out.Reset()
	if err := s.handleCommand(t.Context(), &Command{ID: "again", Type: "mcp_sign_in", Args: json.RawMessage(`{"name":"docs"}`)}); err != nil {
		t.Fatal(err)
	}
	s.promptWG.Wait()
	end = parseResp(t, string(lastResponseLine(out.Bytes())))
	if end["type"] != "mcp_sign_in_end" || end["ok"] != false || end["error"] != "denied" {
		t.Fatalf("failed sign-in end event = %v", end)
	}
}
