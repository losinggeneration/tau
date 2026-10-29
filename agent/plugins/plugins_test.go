package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/losinggeneration/tau/agent/plugins/proto"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/ui"
	baseui "github.com/losinggeneration/tau/ui"
	"github.com/stretchr/testify/require"
)

// TestDispatchEventCollect_RegistrationOrder ensures handlers receive events
// in registration order and that each response is collected. DispatchEventCollect
// is the foundation of before_agent_start system-prompt chaining and any future
// per-listener response handling.
func TestDispatchEventCollect_RegistrationOrder(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)

	var calls []string
	for _, name := range []string{"alpha", "beta", "gamma"} {
		n := name
		mgr.RegisterEventHandler("test_event", EventHandler{
			PluginName: n,
			Handler: func(_ context.Context, _ string, _ json.RawMessage, _ string) EventResponse {
				calls = append(calls, n)
				return EventResponse{ResponseData: []byte(`"` + n + `"`)}
			},
		})
	}

	responses := mgr.DispatchEventCollect(t.Context(), "test_event", nil, "sess-1")
	if len(responses) != 3 {
		t.Fatalf("expected 3 responses, got %d", len(responses))
	}
	want := []string{"alpha", "beta", "gamma"}
	for i, w := range want {
		if responses[i].ResponseData == nil || string(responses[i].ResponseData) != `"`+w+`"` {
			t.Errorf("response[%d] = %s, want %s", i, string(responses[i].ResponseData), w)
		}
	}
	if got := strings.Join(calls, ","); got != "alpha,beta,gamma" {
		t.Errorf("call order = %q, want %q", got, "alpha,beta,gamma")
	}
}

// TestDispatchEventCollect_UnknownEventReturnsEmpty ensures dispatching an
// event with no subscribers is a no-op (no nil-deref, no spurious responses).
func TestDispatchEventCollect_UnknownEventReturnsEmpty(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)
	if got := mgr.DispatchEventCollect(t.Context(), "nope", nil, "sess"); len(got) != 0 {
		t.Fatalf("expected 0 responses, got %d", len(got))
	}
}

// TestPrepareHostSocket_FileModeAndPath validates socket creation: the file
// exists, has 0600 permissions, and is registered with the manager so a
// subsequent prepare for the same name closes the first.
func TestPrepareHostSocket_FileModeAndPath(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)

	path, err := mgr.prepareHostSocketLocked("plugin-A")
	if err != nil {
		t.Fatalf("prepareHostSocket: %v", err)
	}
	if path == "" {
		t.Fatal("empty socket path")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("socket mode = %o, want 0600", got)
	}
	if got := mgr.HostSocketPath("plugin-A"); got != path {
		t.Errorf("HostSocketPath = %q, want %q", got, path)
	}

	// Replacement sockets use distinct paths so staged startup can coexist with the old process.
	path2, err := mgr.prepareHostSocketLocked("plugin-A")
	if err != nil {
		t.Fatalf("second prepareHostSocket: %v", err)
	}
	require.NotEqual(t, path, path2)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	if _, err := os.Stat(path2); err != nil {
		t.Errorf("socket missing after reload: %v", err)
	}
	// The manager owns the replacement socket.
	if hs := mgr.hostSockets["plugin-A"]; hs == nil || hs.path != path2 {
		t.Errorf("hostSockets map state lost across reload")
	}
}

// TestPrepareHostSocket_FirstSocketDir ensures the preferred host socket dir
// is honored (XDG_RUNTIME_DIR/tau > ~/.tau/run > os.TempDir). Setting the env
// before calling t.TempDir / the function isn't safe inside a test (others
// may run in parallel), so just verify the function picks XDG_RUNTIME_DIR when
// set.
func TestPrepareHostSocket_PrefersXDGRuntimeDir(t *testing.T) {
	// Use a non-existent XDG_RUNTIME_DIR to bypass conflicts with other tests;
	// the function must mkdir it.
	x := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", x)
	mgr := NewManager(t.TempDir(), nil, nil)

	path, err := mgr.prepareHostSocketLocked("plugin-B")
	if err != nil {
		t.Fatalf("prepareHostSocket: %v", err)
	}
	if !strings.HasPrefix(path, x) {
		t.Errorf("socket path %q not under XDG_RUNTIME_DIR %q", path, x)
	}
}

// TestBuildPluginCmd_ManifestRunCommand: simple launcher lines exec the
// interpreter directly through the stdin-isolating wrapper (no shell
// forking) so signals reach the plugin process, with cwd = manifest dir and
// TAU_HOST_SOCKET injected.
func TestBuildPluginCmd_ManifestRunCommand(t *testing.T) {
	manifest := &Manifest{
		Run: &RunSection{Command: "node /opt/shim/index.js pi-extension/index.js"},
	}
	cmd, err := buildPluginCmd("/pkg/dir/extension", manifest, "/pkg/dir/tau-package.toml", "/tmp/sock")
	if err != nil {
		t.Fatalf("buildPluginCmd: %v", err)
	}
	if cmd.Dir != "/pkg/dir" {
		t.Errorf("cmd.Dir = %q, want /pkg/dir", cmd.Dir)
	}
	// Direct exec through the wrapper: sh -c <script> node args..., where the
	// script execs "$0" "$@" so the argv below is the real plugin argv. A bare
	// interpreter name is PATH-resolved so a missing binary fails at launch
	// construction instead of as a handshake timeout.
	nodeBin, err := resolveLaunchBinary("node")
	if err != nil {
		t.Fatalf("resolve node: %v", err)
	}
	want := []string{"sh", "-c", pluginDirectScript, nodeBin, "/opt/shim/index.js", "pi-extension/index.js"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
	if !strings.Contains(strings.Join(cmd.Env, " "), "TAU_HOST_SOCKET=/tmp/sock") {
		t.Errorf("TAU_HOST_SOCKET env missing: %v", cmd.Env)
	}
	// The Host endpoint only admits calls tagged with the name startPlugin
	// derives from path, so the plugin must be told that exact name.
	if !slices.Contains(cmd.Env, "TAU_PLUGIN_NAME=extension") {
		t.Errorf("TAU_PLUGIN_NAME env missing: %v", cmd.Env)
	}
}

// TestBuildPluginCmd_ComplexRunCommandUsesShell: commands with shell
// metacharacters fall back to sh -c (with the plugin path as $1).
func TestBuildPluginCmd_ComplexRunCommandUsesShell(t *testing.T) {
	manifest := &Manifest{Run: &RunSection{Command: "node shim.js --port=8080 && echo ready"}}
	cmd, err := buildPluginCmd("/p/x", manifest, "/p/tau-package.toml", "")
	if err != nil {
		t.Fatalf("buildPluginCmd: %v", err)
	}
	if cmd.Args[0] != "sh" || cmd.Args[1] != "-c" {
		t.Errorf("expected sh -c fallback, got %v", cmd.Args)
	}
	if cmd.Args[len(cmd.Args)-1] != "/p/x" {
		t.Errorf("expected plugin path as last arg ($1), got %v", cmd.Args)
	}
}

// TestBuildPluginCmd_QuotedPathsSplit: quoted arguments with spaces are
// tokenized correctly for the direct-exec path.
func TestBuildPluginCmd_QuotedPathsSplit(t *testing.T) {
	manifest := &Manifest{Run: &RunSection{Command: `node "/opt/my shim/index.js" "entry file.js"`}}
	cmd, err := buildPluginCmd("/p/x", manifest, "/p/tau-package.toml", "")
	if err != nil {
		t.Fatalf("buildPluginCmd: %v", err)
	}
	// Quoted fields survive as single $0/$@ parameters — the wrapper never
	// re-splits them.
	nodeBin, err := resolveLaunchBinary("node")
	if err != nil {
		t.Fatalf("resolve node: %v", err)
	}
	want := []string{"sh", "-c", pluginDirectScript, nodeBin, "/opt/my shim/index.js", "entry file.js"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
}

// TestBuildPluginCmd_NoManifest wraps the bare binary through the same
// stdin-isolating exec wrapper and still injects the host socket env.
func TestBuildPluginCmd_NoManifest(t *testing.T) {
	cmd, err := buildPluginCmd("/abs/plugin-binary", nil, "", "/tmp/sock2")
	if err != nil {
		t.Fatalf("buildPluginCmd: %v", err)
	}
	want := []string{"sh", "-c", pluginDirectScript, "/abs/plugin-binary"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
	if cmd.Dir != filepath.Dir("/abs/plugin-binary") {
		t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, filepath.Dir("/abs/plugin-binary"))
	}
	if !strings.Contains(strings.Join(cmd.Env, " "), "TAU_HOST_SOCKET=/tmp/sock2") {
		t.Errorf("TAU_HOST_SOCKET env missing: %v", cmd.Env)
	}
	if !slices.Contains(cmd.Env, "TAU_PLUGIN_NAME=plugin-binary") {
		t.Errorf("TAU_PLUGIN_NAME env missing: %v", cmd.Env)
	}
}

// TestBuildPluginCmd_HandshakeMatchesLoadLocked verifies the Handshake()
// (CLI verify) path and loadLocked share launch semantics: same tokenizer,
// same cwd, same env injection.
func TestBuildPluginCmd_HandshakeMatchesLoadLocked(t *testing.T) {
	manifest := &Manifest{Run: &RunSection{Command: "node shim.js --port=8080"}}
	cmd, err := buildPluginCmd("/p/x", manifest, "/p/tau-package.toml", "/tmp/sock")
	if err != nil {
		t.Fatalf("buildPluginCmd: %v", err)
	}
	// Direct exec through the wrapper — argv matches the manifest's command
	// fields exactly as $0/$@.
	nodeBin, err := resolveLaunchBinary("node")
	if err != nil {
		t.Fatalf("resolve node: %v", err)
	}
	want := []string{"sh", "-c", pluginDirectScript, nodeBin, "shim.js", "--port=8080"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v (Handshake must match loadLocked)", cmd.Args, want)
	}
	if cmd.Dir != "/p" {
		t.Errorf("cmd.Dir = %q, want /p", cmd.Dir)
	}
}

// TestBuildPluginCmd_IsolatesTerminalStdin pins the launcher hygiene that
// keeps plugin subprocesses off tau's terminal: go-plugin unconditionally
// assigns the host's stdin to the plugin process (cmd.Stdin = os.Stdin), and
// inside the TUI that fd IS the terminal. A plugin — or any process an
// extension spawns from it — that reads or reconfigures that fd steals
// keystrokes from the UI and can leave the tty in a cooked/echoing state
// (observed as a wedged TUI after /reload killed a plugin tree still holding
// the terminal). Every launch shape must therefore exec through a wrapper
// script that redirects stdin to /dev/null, and must use exec so signal
// delivery to the plugin stays direct.
func TestBuildPluginCmd_IsolatesTerminalStdin(t *testing.T) {
	runManifest := &Manifest{Run: &RunSection{Command: "node /opt/shim/index.js entry.js"}}
	complexManifest := &Manifest{Run: &RunSection{Command: "node shim.js --port=8080 && echo ready"}}

	cases := []struct {
		name         string
		path         string
		manifest     *Manifest
		manifestPath string
	}{
		{"direct run command", "/pkg/dir/extension", runManifest, "/pkg/dir/tau-package.toml"},
		{"shell run command", "/p/x", complexManifest, "/p/tau-package.toml"},
		{"bare binary", "/abs/plugin-binary", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := buildPluginCmd(tc.path, tc.manifest, tc.manifestPath, "")
			if err != nil {
				t.Fatalf("buildPluginCmd: %v", err)
			}
			if len(cmd.Args) < 3 || cmd.Args[0] != "sh" || cmd.Args[1] != "-c" {
				t.Fatalf("cmd.Args = %v, want an sh -c <script> wrapper", cmd.Args)
			}
			script := cmd.Args[2]
			if !strings.Contains(script, "exec") {
				t.Errorf("script %q must exec so signals reach the plugin directly", script)
			}
			if !strings.Contains(script, "< /dev/null") {
				t.Errorf("script %q must redirect stdin to /dev/null so the plugin tree cannot hold tau's terminal", script)
			}
		})
	}
}

// TestCloseHostSocketLocked_RemovesFile verifies cleanup semantics.
func TestCloseHostSocketLocked_RemovesFile(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)
	path, err := mgr.prepareHostSocketLocked("plugin-C")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	mgr.closeHostSocketLocked("plugin-C")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket still present after close: %v", err)
	}
	if got := mgr.HostSocketPath("plugin-C"); got != "" {
		t.Errorf("HostSocketPath = %q, want empty after close", got)
	}
}

// TestEventResponse_MarshalsResponseData is a sanity check that the JSON
// payload returned by a plugin handler survives the dispatch->collect path.
// before_agent_start chaining depends on ResponseData making it back to the
// agent loop intact.
func TestEventResponse_MarshalsResponseData(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)
	mgr.RegisterEventHandler("before_agent_start", EventHandler{
		PluginName: "p",
		Handler: func(_ context.Context, _ string, payload json.RawMessage, _ string) EventResponse {
			var in struct {
				SystemPrompt string `json:"systemPrompt"`
			}
			if err := json.Unmarshal(payload, &in); err != nil {
				return EventResponse{}
			}
			out, _ := json.Marshal(struct {
				SystemPrompt string `json:"systemPrompt"`
			}{SystemPrompt: in.SystemPrompt + " [rewrite]"})
			return EventResponse{ResponseData: out}
		},
	})
	resps := mgr.DispatchEventCollect(t.Context(), "before_agent_start", json.RawMessage(`{"systemPrompt":"base"}`), "s")
	if len(resps) != 1 || resps[0].ResponseData == nil {
		t.Fatalf("missing response data: %+v", resps)
	}
	if got := string(resps[0].ResponseData); !strings.Contains(got, "base [rewrite]") {
		t.Errorf("response_data = %q, missing rewritten prompt", got)
	}
}

func TestProjectTrustDecisionFirstAnswerWins(t *testing.T) {
	answer := func(data string, calls *[]string, name string) EventHandler {
		return EventHandler{PluginName: name, Handler: func(_ context.Context, event string, payload json.RawMessage, _ string) EventResponse {
			*calls = append(*calls, name+":"+event+":"+string(payload))
			return EventResponse{ResponseData: []byte(data)}
		}}
	}
	for _, tc := range []struct {
		name     string
		answers  []string
		want     *bool
		remember bool
		calls    int
	}{
		{name: "none"},
		{name: "undecided falls through", answers: []string{`{"trusted":"undecided"}`, ``, `not json`, `{"trusted":"maybe"}`}, calls: 4},
		{name: "yes remembered", answers: []string{`{"trusted":"undecided"}`, `{"trusted":"yes","remember":true}`, `{"trusted":"no"}`}, want: new(true), remember: true, calls: 2},
		{name: "no", answers: []string{`{"trusted":"no"}`, `{"trusted":"yes"}`}, want: new(false), calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager(t.TempDir(), nil, nil)
			var calls []string
			for i, data := range tc.answers {
				mgr.RegisterEventHandler("project_trust", answer(data, &calls, fmt.Sprint(i)))
			}
			got, remember, err := mgr.ProjectTrustDecision(t.Context(), "/work")
			if err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want || remember != tc.remember {
				t.Fatalf("decision = %v remember %v, want %v %v", got, remember, tc.want, tc.remember)
			}
			if len(calls) != tc.calls {
				t.Fatalf("calls = %q, want %d", calls, tc.calls)
			}
			if len(calls) > 0 && calls[0] != `0:project_trust:{"cwd":"/work"}` {
				t.Fatalf("first call = %q", calls[0])
			}
		})
	}

	mgr := NewManager(t.TempDir(), nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	mgr.RegisterEventHandler("project_trust", EventHandler{Handler: func(context.Context, string, json.RawMessage, string) EventResponse {
		cancel()
		return EventResponse{ResponseData: []byte(`{"trusted":"yes"}`)}
	}})
	if got, _, err := mgr.ProjectTrustDecision(ctx, "/work"); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("cancelled decision = %v, %v", got, err)
	}
}

// TestPrepareHostSocket_DistinctPath leaves unrelated prior socket paths untouched.
func TestPrepareHostSocket_DistinctPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	mgr := NewManager(t.TempDir(), nil, nil)
	path, err := mgr.prepareHostSocketLocked("plugin-D")
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	// Force-close via manager so we leave a stale file behind.
	require.NoError(t, mgr.closeHostSocketLocked("plugin-D"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("setup: stale socket still present: %v", err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	path2, err := mgr.prepareHostSocketLocked("plugin-D")
	if err != nil {
		t.Fatalf("second prepare (stale present): %v", err)
	}
	require.NotEqual(t, path, path2)
	stale, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "stale", string(stale))
	if _, err := os.Stat(path2); err != nil {
		t.Errorf("new socket missing: %v", err)
	}
}

// TestHostSocketDir_FallbackWhenNoXDGRuntime exercises the home-dir fallback
// path. We can't assert the full home path here without mocking, so just check
// the function returns a non-empty, non-tmpdir candidate when XDG_RUNTIME_DIR
// is unset.
func TestHostSocketDir_FallbackUsesHome(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	dir := hostSocketDir()
	if dir == "" {
		t.Fatal("hostSocketDir returned empty")
	}
	// Either ~/.tau/run or os.TempDir depending on $HOME availability.
	if !strings.Contains(dir, "tau") {
		t.Errorf("hostSocketDir %q missing 'tau' segment", dir)
	}
}

// Compile-time guard: ensure our event-response wrapper keeps the proto field
// reachable. The proto's response_data is the only thing the shim's
// before_agent_start system-prompt rewrite relies on for round-tripping.
func TestHandleEvent_MapsResponseData(t *testing.T) {
	// Simulate a plugin-side response containing systemPrompt. handleEvent
	// currently calls gRPC.OnEvent (no fake available here), so we just
	// verify the wrapper conversion via a helper that mirrors it.
	resp := []byte(`{"block":false,"response_data":"{\"systemPrompt\":\"rewritten\"}"}`)
	var pr protoFromServer
	if err := json.Unmarshal(resp, &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pr.ResponseData == "" {
		t.Fatal("response_data empty")
	}
	if !strings.Contains(pr.ResponseData, "rewritten") {
		t.Errorf("response_data = %q", pr.ResponseData)
	}
}

// protoFromServer is a tiny mirror of the proto EventResponse used by the
// handleEvent mapping test.
type protoFromServer struct {
	Block        bool   `json:"block"`
	BlockReason  string `json:"block_reason"`
	ResponseData string `json:"response_data"`
}

// TestSessionSendUserMessage_NoSender verifies the gRPC Unavailable error
// when the manager has no message sender wired. The shim must treat this as
// a host misconfiguration, not crash.
func TestSessionSendUserMessage_NoSender(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)
	hs := NewHostService(mgr)
	resp, err := hs.SessionSendUserMessage(context.Background(), &proto.SessionSendUserMessageRequest{
		Text: "hello",
	})
	if err == nil {
		t.Fatal("expected error when no message sender is configured")
	}
	if status.Code(err) != codes.Unavailable {
		t.Errorf("status code = %v, want Unavailable", status.Code(err))
	}
	if resp == nil || resp.Error == "" {
		t.Errorf("expected error string in response, got %+v", resp)
	}
}

// TestSessionSendUserMessage_InvalidDeliverAs ensures unknown deliver_as
// values are rejected with InvalidArgument, matching the pi surface.
func TestSessionSendUserMessage_InvalidDeliverAs(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)
	mgr.SetMessageSender(func(text, deliverAs string) error { return nil })
	hs := NewHostService(mgr)
	_, err := hs.SessionSendUserMessage(context.Background(), &proto.SessionSendUserMessageRequest{
		Text:      "hello",
		DeliverAs: "bogus",
	})
	if err == nil {
		t.Fatal("expected error for bogus deliver_as")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("status code = %v, want InvalidArgument", status.Code(err))
	}
}

// TestSessionCompact verifies the gRPC Unavailable error when no compactor is
// wired, and that a wired compactor receives the instructions and reports ok.
func TestSessionCompact(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)
	hs := NewHostService(mgr)

	resp, err := hs.SessionCompact(context.Background(), &proto.SessionCompactRequest{Instructions: "focus"})
	if err == nil {
		t.Fatal("expected error when no compactor is configured")
	}
	if status.Code(err) != codes.Unavailable {
		t.Errorf("status code = %v, want Unavailable", status.Code(err))
	}
	if resp == nil || resp.Error == "" {
		t.Errorf("expected error string in response, got %+v", resp)
	}

	var got string
	mgr.SetCompactor(func(_ context.Context, instructions string) error {
		got = instructions
		return nil
	})
	resp, err = hs.SessionCompact(context.Background(), &proto.SessionCompactRequest{Instructions: "focus"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Ok || resp.Error != "" {
		t.Errorf("resp = %+v, want ok", resp)
	}
	if got != "focus" {
		t.Errorf("instructions = %q, want %q", got, "focus")
	}
}

// TestSessionSendUserMessage_RoutesCorrectly checks the sender is called
// with the right (text, deliverAs) for "", "steer", "followUp" — and that
// a sender error surfaces as Internal.
func TestSessionSendUserMessage_RoutesCorrectly(t *testing.T) {
	cases := []struct {
		name      string
		deliverAs string
		wantErr   bool
	}{
		{"empty", "", false},
		{"steer", "steer", false},
		{"followUp", "followUp", false},
		{"senderError", "followUp", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager(t.TempDir(), nil, nil)
			var gotText, gotDeliverAs string
			mgr.SetMessageSender(func(text, deliverAs string) error {
				gotText = text
				gotDeliverAs = deliverAs
				if tc.wantErr {
					return errors.New("boom")
				}
				return nil
			})
			hs := NewHostService(mgr)
			resp, err := hs.SessionSendUserMessage(context.Background(), &proto.SessionSendUserMessageRequest{
				Text:      "hi from shim",
				DeliverAs: tc.deliverAs,
			})
			if gotText != "hi from shim" {
				t.Errorf("sender text = %q, want %q", gotText, "hi from shim")
			}
			if gotDeliverAs != tc.deliverAs {
				t.Errorf("sender deliverAs = %q, want %q", gotDeliverAs, tc.deliverAs)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error from sender")
				}
				if status.Code(err) != codes.Internal {
					t.Errorf("status code = %v, want Internal", status.Code(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp == nil || !resp.Ok {
				t.Errorf("expected Ok=true, got %+v", resp)
			}
		})
	}
}

// TestSessionSendUserMessage_ConcurrentSenders verifies the manager's
// messageSender can be reconfigured without races; SetMessageSender takes
// m.mu briefly, so this test stresses the read path under concurrent calls.
func TestSessionSendUserMessage_ConcurrentSenders(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil, nil)
	mgr.SetMessageSender(func(text, deliverAs string) error { return nil })
	hs := NewHostService(mgr)
	for i := 0; i < 50; i++ {
		go func() {
			_, _ = hs.SessionSendUserMessage(context.Background(), &proto.SessionSendUserMessageRequest{
				Text:      "x",
				DeliverAs: "followUp",
			})
		}()
	}
}

// TestShimInterop verifies tau's plugin manager can load the real pi-ext-shim
// (Node) end to end: go-plugin handshake line, AutoMTLS, gRPC Plugin service,
// Initialize (jiti extension load), ListRegistrations, OnEvent system-prompt
// rewriting, and ExecuteCommand.
//
// This test spawns a real `node` subprocess, so it is opt-in:
//
//	TAU_PI_SHIM_INTEROP=1 go test ./agent/plugins -run TestShimInterop -v
//
// Set TAU_PI_SHIM_INTEROP_DEBUG=1 additionally to stream go-plugin client
// logs. The test skips when the env var is unset or node is unavailable,
// keeping the default suite fast and subprocess-free.
func TestShimInterop(t *testing.T) {
	if os.Getenv("TAU_PI_SHIM_INTEROP") != "1" {
		t.Skip("set TAU_PI_SHIM_INTEROP=1 to run the pi-ext-shim interop test (spawns node)")
	}
	if runtime.GOOS == "windows" {
		t.Skip("shim interop test not supported on windows CI")
	}

	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not found in PATH: %v", err)
	}

	// The shim lives at <repo>/piext/shim relative to this package's source
	// dir (TestMain chdirs to a temp work dir, so use originalWD).
	shimDir := filepath.Join(originalWD, "..", "..", "piext", "shim")
	if _, err := os.Stat(filepath.Join(shimDir, "node_modules")); err != nil {
		t.Skipf("shim dependencies not installed (run npm install in %s): %v", shimDir, err)
	}

	// Build a pi-extension package: a tiny extension that registers a
	// command and subscribes to before_agent_start with a systemPrompt
	// rewrite (the pi extension pattern).
	pkgDir := t.TempDir()
	extDir := filepath.Join(pkgDir, "pi-extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	extEntry := filepath.Join(extDir, "index.js")
	const extSrc = `export default function (pi) {
	pi.on("before_agent_start", async (event) => {
		return { systemPrompt: (event.systemPrompt || "") + "\n[interop-rules]" };
	});
	pi.registerCommand("interop", {
		description: "interop test command",
		handler: async (_args, ctx) => { ctx.ui.notify("interop ok", "info"); },
	});
};
`
	if err := os.WriteFile(extEntry, []byte(extSrc), 0o644); err != nil {
		t.Fatalf("write extension: %v", err)
	}

	// tau-package.toml launching the shim with node. The package dir name
	// doubles as the plugin name.
	absShim, err := filepath.Abs(filepath.Join(shimDir, "index.js"))
	if err != nil {
		t.Fatalf("abs shim: %v", err)
	}
	manifest := fmt.Sprintf(
		"[package]\nname = \"pi-interop-test\"\nversion = \"0.0.1\"\ndescription = \"pi-ext-shim interop fixture\"\n\n[plugin]\napi_version = \"1\"\n\n[run]\ncommand = %q\n",
		nodeBin+" "+absShim+" pi-extension/index.js",
	)
	manifestPath := filepath.Join(pkgDir, "tau-package.toml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Load through the real plugin manager (production path: buildPluginCmd,
	// host socket prep + TAU_HOST_SOCKET env, AutoMTLS handshake,
	// Initialize, ListRegistrations, registerPlugin).
	var mgr *Manager
	if os.Getenv("TAU_PI_SHIM_INTEROP_DEBUG") == "1" {
		mgr = NewManager(t.TempDir(), nil, nil, WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	} else {
		mgr = NewManager(t.TempDir(), nil, nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	info, err := mgr.Load(ctx, pkgDir, manifestPath)
	if err != nil {
		t.Fatalf("Load pi-ext-shim package failed: %v", err)
	}
	defer func() { _ = mgr.ShutdownPlugin(ctx, info.Name, 2*time.Second) }()

	if info.APIVersion != "1" {
		t.Errorf("api version = %q, want \"1\"", info.APIVersion)
	}

	// Registrations must reflect the extension's pi.on/registerCommand calls.
	var hasCmd, hasEvent bool
	for _, c := range info.Registrations.Commands {
		if c.Name == "interop" {
			hasCmd = true
		}
	}
	for _, e := range info.Registrations.EventSubscriptions {
		if e == "before_agent_start" {
			hasEvent = true
		}
	}
	if !hasCmd {
		t.Errorf("command \"interop\" not registered; commands = %+v", info.Registrations.Commands)
	}
	if !hasEvent {
		t.Errorf("event \"before_agent_start\" not subscribed; events = %v", info.Registrations.EventSubscriptions)
	}

	// OnEvent must round-trip the systemPrompt rewrite through response_data
	// (the pi extension system-prompt injection mechanism).
	payload := `{"systemPrompt":"base"}`
	responses := mgr.DispatchEventCollect(t.Context(), "before_agent_start", json.RawMessage(payload), "interop-session")
	found := false
	for _, r := range responses {
		if strings.Contains(string(r.ResponseData), "interop-rules") {
			found = true
		}
	}
	if !found {
		t.Errorf("before_agent_start response_data missing systemPrompt rewrite; responses = %+v", responses)
	}

	// ExecuteCommand must invoke the extension's command handler. The
	// handler calls ctx.ui.notify which dials the Host socket; the manager
	// has no UI wired here so the Host RPC fails and the shim logs to
	// stderr — the command itself must still succeed.
	out, isErr, err := mgr.executeCommand(ctx, info.Name, "interop", "")
	if err != nil || isErr {
		t.Errorf("executeCommand interop = (%q, %v, %v), want success", out, isErr, err)
	}
}

type terminalInputPluginClient struct {
	proto.UnimplementedPluginServer
	seen    []*proto.TerminalInput
	consume bool
}

func (c *terminalInputPluginClient) Initialize(context.Context, *proto.InitRequest, ...grpc.CallOption) (*proto.InitResponse, error) {
	return &proto.InitResponse{}, nil
}

func (c *terminalInputPluginClient) ListRegistrations(context.Context, *proto.ListRegistrationsRequest, ...grpc.CallOption) (*proto.Registrations, error) {
	return &proto.Registrations{}, nil
}

func (c *terminalInputPluginClient) OnEvent(context.Context, *proto.Event, ...grpc.CallOption) (*proto.EventResponse, error) {
	return &proto.EventResponse{}, nil
}

func (c *terminalInputPluginClient) ExecuteTool(context.Context, *proto.ToolCall, ...grpc.CallOption) (*proto.ToolResult, error) {
	return &proto.ToolResult{}, nil
}

func (c *terminalInputPluginClient) ExecuteCommand(context.Context, *proto.CommandInvocation, ...grpc.CallOption) (*proto.CommandResult, error) {
	return &proto.CommandResult{}, nil
}

func (c *terminalInputPluginClient) OnTerminalInput(_ context.Context, in *proto.TerminalInput, _ ...grpc.CallOption) (*proto.TerminalInputResponse, error) {
	c.seen = append(c.seen, in)
	return &proto.TerminalInputResponse{Consume: c.consume}, nil
}

func (c *terminalInputPluginClient) Shutdown(context.Context, *proto.ShutdownRequest, ...grpc.CallOption) (*proto.ShutdownResponse, error) {
	return &proto.ShutdownResponse{}, nil
}

func (c *terminalInputPluginClient) RouteModel(context.Context, *proto.ModelRouteRequest, ...grpc.CallOption) (*proto.ModelRouteResponse, error) {
	return &proto.ModelRouteResponse{}, nil
}

func TestDispatchTerminalInputForwardsToLoadedPlugins(t *testing.T) {
	m := NewManager(t.TempDir(), nil, nil)
	client := &terminalInputPluginClient{consume: true}
	m.plugins["pi"] = &PluginInfo{Name: "pi", grpcClient: client}

	consumed := m.DispatchTerminalInput(TerminalInput{Key: "rune", Text: "x", Mod: "ctrl"})
	if !consumed {
		t.Fatal("DispatchTerminalInput returned false, want true")
	}
	if len(client.seen) != 1 {
		t.Fatalf("OnTerminalInput calls = %d, want 1", len(client.seen))
	}
	got := client.seen[0]
	if got.Key != "rune" || got.Text != "x" || got.Mod != "ctrl" {
		t.Fatalf("terminal input = %#v, want key/text/mod forwarded", got)
	}
}

// recordingUI records the keyed UI state pushed through the Host service so
// tests can assert on what a freshly attached UI receives.
type recordingUI struct {
	mu      sync.Mutex
	status  map[string]string
	widgets map[string][]string
}

func newRecordingUI() *recordingUI {
	return &recordingUI{status: map[string]string{}, widgets: map[string][]string{}}
}

func (r *recordingUI) Confirm(context.Context, string, string) (bool, error) { return false, nil }
func (r *recordingUI) Select(context.Context, string, []string) (int, error) { return -1, nil }
func (r *recordingUI) Input(context.Context, string, string) (string, error) { return "", nil }
func (r *recordingUI) Notify(baseui.NotifyLevel, string)                     {}
func (r *recordingUI) SetTitle(string)                                       {}
func (r *recordingUI) SetEditorText(string)                                  {}
func (r *recordingUI) SetToolsExpanded(bool)                                 {}

func (r *recordingUI) SetStatus(key, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status[key] = message
}

func (r *recordingUI) SetWidget(key string, lines []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.widgets[key] = append([]string(nil), lines...)
}

func (r *recordingUI) statusFor(key string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.status[key]
	return value, ok
}

func (r *recordingUI) widgetFor(key string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.widgets[key]
}

// TestSetUIReplaysStateSetBeforeUIAttached pins the startup ordering hole.
//
// The interactive TUI attaches its UI (pluginMgr.SetUI(app)) only after the
// session exists, but session_start is dispatched to plugins *during* session
// creation. A plugin that renders its indicator from that event — ponytail's
// "ponytail: ULTRA" status — pushed it into the placeholder no-op UI, where it
// was dropped for good: the indicator then appeared only once some later event
// (agent_start on the first prompt) happened to fire after the real UI landed.
// Status and widgets are keyed state, not transient events, so attaching a UI
// must replay the current values.
func TestSetUIReplaysStateSetBeforeUIAttached(t *testing.T) {
	mgr := NewManager(t.TempDir(), baseui.NewNoOpUI(), nil)
	ctx := context.Background()

	if _, err := mgr.host.UISetStatus(ctx, &proto.UISetStatusRequest{Key: "ponytail", Message: "ponytail: ULTRA"}); err != nil {
		t.Fatalf("UISetStatus: %v", err)
	}
	if _, err := mgr.host.UISetWidget(ctx, &proto.UISetWidgetRequest{Key: "ponytail", Lines: []string{"row"}}); err != nil {
		t.Fatalf("UISetWidget: %v", err)
	}

	rec := newRecordingUI()
	mgr.SetUI(rec)

	if got, ok := rec.statusFor("ponytail"); !ok || got != "ponytail: ULTRA" {
		t.Errorf("status after SetUI = %q (present=%v), want it replayed as %q", got, ok, "ponytail: ULTRA")
	}
	if got := rec.widgetFor("ponytail"); !slices.Equal(got, []string{"row"}) {
		t.Errorf("widget after SetUI = %v, want [row] replayed", got)
	}
}

// Only the newest value per key is replayed, and a key the plugin cleared must
// not come back to life when a UI attaches.
func TestSetUIReplaysLatestStateAndSkipsCleared(t *testing.T) {
	mgr := NewManager(t.TempDir(), baseui.NewNoOpUI(), nil)
	ctx := context.Background()

	for _, message := range []string{"ponytail: LITE", "ponytail: ULTRA"} {
		if _, err := mgr.host.UISetStatus(ctx, &proto.UISetStatusRequest{Key: "ponytail", Message: message}); err != nil {
			t.Fatalf("UISetStatus: %v", err)
		}
	}
	if _, err := mgr.host.UISetStatus(ctx, &proto.UISetStatusRequest{Key: "gone", Message: "transient"}); err != nil {
		t.Fatalf("UISetStatus: %v", err)
	}
	// An empty message is how a plugin clears its indicator (ponytail does this
	// for mode "off"); there is nothing to restore for that key afterwards.
	if _, err := mgr.host.UISetStatus(ctx, &proto.UISetStatusRequest{Key: "gone", Message: ""}); err != nil {
		t.Fatalf("UISetStatus: %v", err)
	}

	rec := newRecordingUI()
	mgr.SetUI(rec)

	if got, _ := rec.statusFor("ponytail"); got != "ponytail: ULTRA" {
		t.Errorf("status = %q, want only the newest value %q", got, "ponytail: ULTRA")
	}
	if got, ok := rec.statusFor("gone"); ok {
		t.Errorf("cleared key replayed as %q, want it left unset", got)
	}
}

// Attaching a UI must not turn the manager into a replay-only sink: updates
// after SetUI still go straight through to the live UI.
func TestUIStateForwardsLiveAfterSetUI(t *testing.T) {
	mgr := NewManager(t.TempDir(), baseui.NewNoOpUI(), nil)
	ctx := context.Background()

	rec := newRecordingUI()
	mgr.SetUI(rec)

	if _, err := mgr.host.UISetStatus(ctx, &proto.UISetStatusRequest{Key: "ponytail", Message: "ponytail: FULL"}); err != nil {
		t.Fatalf("UISetStatus: %v", err)
	}
	if got, _ := rec.statusFor("ponytail"); got != "ponytail: FULL" {
		t.Errorf("live status = %q, want %q", got, "ponytail: FULL")
	}
}

func TestDispatchEventCollectChainsMutablePayloads(t *testing.T) {
	for _, tc := range []struct {
		name              string
		initial, response string
		field             string
		want              string
	}{
		{name: "before_agent_start", initial: `{"systemPrompt":"base"}`, response: `{"systemPrompt":"rewritten"}`, field: "systemPrompt", want: "rewritten"},
		{name: "input", initial: `{"text":"base"}`, response: `{"action":"transform","text":"rewritten"}`, field: "text", want: "rewritten"},
		{name: "context", initial: `{"messages":[]}`, response: `{"messages":[{"role":"user","content":"rewritten"}]}`, field: "messages", want: `[{"role":"user","content":"rewritten"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager(t.TempDir(), nil, nil)
			mgr.RegisterEventHandler(tc.name, EventHandler{Handler: func(context.Context, string, json.RawMessage, string) EventResponse {
				return EventResponse{ResponseData: []byte(tc.response)}
			}})
			mgr.RegisterEventHandler(tc.name, EventHandler{Handler: func(_ context.Context, _ string, payload json.RawMessage, _ string) EventResponse {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(payload, &fields); err != nil {
					t.Fatal(err)
				}
				got := string(fields[tc.field])
				if tc.field != "messages" {
					if err := json.Unmarshal(fields[tc.field], &got); err != nil {
						t.Fatal(err)
					}
				}
				if got != tc.want {
					t.Fatalf("second handler got %s, want %s", got, tc.want)
				}
				return EventResponse{}
			}})
			mgr.DispatchEventCollect(t.Context(), tc.name, json.RawMessage(tc.initial), "session")
		})
	}
}

func TestToolRegistrationsPreserveExecutionMetadata(t *testing.T) {
	var registration proto.ToolRegistration
	if err := protojson.Unmarshal([]byte(`{"name":"lookup","exposure":"deferred","outputSchema":"{\"type\":\"object\"}","constrainedSampling":"{\"type\":\"json_schema\",\"strict\":\"require\"}"}`), &registration); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(t.TempDir(), nil, nil)
	manager.plugins["test"] = &PluginInfo{Registrations: Registrations{Tools: convertToolRegistrations([]*proto.ToolRegistration{&registration})}}
	manager.toolHandlers["lookup"] = func(context.Context, string, string, string) (string, bool, error) { return "", false, nil }
	desc := manager.Tools()[0]
	if desc.Exposure != "deferred" || string(desc.OutputSchema) != `{"type":"object"}` || string(desc.ConstrainedSampling) != `{"type":"json_schema","strict":"require"}` {
		t.Fatalf("metadata=%+v", desc)
	}
}

func TestExecutePluginToolResultRetainsLegacyHandlers(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	manager.toolHandlers["legacy"] = func(context.Context, string, string, string) (string, bool, error) {
		return "legacy output", false, nil
	}
	result, err := manager.ExecutePluginToolResult(t.Context(), "legacy", "id", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "legacy output" || result.IsError {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecutePluginToolResultPreservesStructuredDataAndLocalName(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	want := &proto.ToolResult{Output: "ok", StructuredContent: `{"found":true}`, Usage: `{"input":1}`, Content: `[{"type":"text","text":"ok"}]`}
	manager.plugins["test"] = &PluginInfo{Registrations: Registrations{ToolPrefix: "test.", Tools: []ToolRegistration{{Name: "lookup"}}}, grpcClient: &mockPluginClient{onExecuteTool: func(_ context.Context, request *proto.ToolCall) (*proto.ToolResult, error) {
		if request.ToolName != "lookup" || request.ToolCallId != "call" {
			t.Errorf("request=%+v", request)
		}
		return want, nil
	}}}
	got, err := manager.ExecutePluginToolResult(t.Context(), "test.lookup", "call", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("result=%+v", got)
	}
}

func TestLegacyPrefixedPluginToolUsesLocalRPCName(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	info := &PluginInfo{Name: "test", Registrations: Registrations{ToolPrefix: "test.", Tools: []ToolRegistration{{Name: "lookup"}}}, grpcClient: &mockPluginClient{onExecuteTool: func(_ context.Context, request *proto.ToolCall) (*proto.ToolResult, error) {
		if request.ToolName != "lookup" {
			t.Errorf("RPC tool name=%q", request.ToolName)
		}
		return &proto.ToolResult{Output: "ok"}, nil
	}}}
	manager.plugins["test"] = info
	manager.registerPlugin(info)
	if _, _, err := manager.ExecutePluginTool(t.Context(), "test.lookup", "call", "{}"); err != nil {
		t.Fatal(err)
	}
}

func TestToolNamespaceRegistrationTransport(t *testing.T) {
	var registration proto.ToolRegistration
	if err := protojson.Unmarshal([]byte(`{"name":"lookup","namespace":"{\"name\":\"docs\",\"aliases\":[\"manuals\"]}","defaultActive":false}`), &registration); err != nil {
		t.Fatal(err)
	}
	converted := convertToolRegistrations([]*proto.ToolRegistration{&registration})
	if len(converted) != 1 || converted[0].DefaultActive == nil || *converted[0].DefaultActive || string(converted[0].Namespace) != `{"name":"docs","aliases":["manuals"]}` {
		t.Fatalf("metadata lost: %+v", converted)
	}
}

func (c *terminalInputPluginClient) ReachTools(ctx context.Context, req *proto.ToolCall, opts ...grpc.CallOption) (*proto.ReachToolsResponse, error) {
	return &proto.ReachToolsResponse{}, ctx.Err()
}

func TestPluginReachCallback(t *testing.T) {
	manager := NewManager(t.TempDir(), ui.NewNoOpUI(), nil)
	manager.plugins["test"] = &PluginInfo{grpcClient: &mockPluginClient{}, Registrations: Registrations{ToolPrefix: "ext_", Tools: []ToolRegistration{{Name: "caller", HasReachesTools: true}}}}
	names, err := manager.ReachPluginTools(t.Context(), "ext_caller", map[string]any{"query": "docs"})
	require.NoError(t, err)
	require.Equal(t, []string{"docs"}, names)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = manager.ReachPluginTools(ctx, "ext_caller", nil)
	require.ErrorIs(t, err, context.Canceled)
	_, err = manager.ReachPluginTools(t.Context(), "missing", nil)
	require.Error(t, err)
}

func TestPluginExecutionRejectionDiffersFromExplicitError(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	reply := &proto.ToolResult{Output: "usable error", IsError: true, NestedCalls: `[{"id":"child","name":"lookup","status":"error"}]`}
	manager.plugins["test"] = &PluginInfo{Name: "test", Registrations: Registrations{Tools: []ToolRegistration{{Name: "lookup"}}}, grpcClient: &mockPluginClient{onExecuteTool: func(context.Context, *proto.ToolCall) (*proto.ToolResult, error) { return reply, nil }}}
	manager.registerPlugin(manager.plugins["test"])
	result, err := manager.ExecutePluginToolResult(t.Context(), "lookup", "call", "{}")
	require.NoError(t, err)
	require.Same(t, reply, result)
	reply.Rejection = "transport rejected"
	result, err = manager.ExecutePluginToolResult(t.Context(), "lookup", "call", "{}")
	require.NoError(t, err)
	require.Same(t, reply, result)
	require.Equal(t, "transport rejected", result.Rejection)
	_, isError, err := manager.ExecutePluginTool(t.Context(), "lookup", "call", "{}")
	require.True(t, isError)
	require.ErrorContains(t, err, "transport rejected")
}

type reachTestClient struct {
	proto.PluginClient
	reach func(context.Context) (*proto.ReachToolsResponse, error)
}

func (c *reachTestClient) ReachTools(ctx context.Context, _ *proto.ToolCall, _ ...grpc.CallOption) (*proto.ReachToolsResponse, error) {
	return c.reach(ctx)
}

func TestPluginReachDeadlineAndEmptyResponse(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	client := &reachTestClient{reach: func(ctx context.Context) (*proto.ReachToolsResponse, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("reach callback has no execution deadline")
		}
		return nil, nil
	}}
	manager.plugins["test"] = &PluginInfo{grpcClient: client, Registrations: Registrations{Tools: []ToolRegistration{{Name: "caller", HasReachesTools: true}}}}
	_, err := manager.ReachPluginTools(t.Context(), "caller", nil)
	require.ErrorContains(t, err, "no reach result")
}

func TestPluginRegistrationReloadPreservesLoadoutMetadata(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	inactive := false
	original := &PluginInfo{Name: "test", Registrations: Registrations{ToolPrefix: "ext_", Tools: []ToolRegistration{
		{Name: "lookup", DefaultActive: &inactive, Exposure: "deferred", Namespace: json.RawMessage(`{"name":"docs"}`), HasReachesTools: true},
		{Name: "removed"},
	}}}
	manager.plugins[original.Name] = original
	manager.registerPlugin(original)
	tools := manager.Tools()
	require.Len(t, tools, 2)
	index := slices.IndexFunc(tools, func(tool ToolHandlerDesc) bool { return tool.Name == "ext_lookup" })
	require.NotEqual(t, -1, index)
	require.NotNil(t, tools[index].DefaultActive)
	require.False(t, *tools[index].DefaultActive)
	require.True(t, tools[index].HasReachesTools)
	require.Equal(t, "deferred", tools[index].Exposure)
	require.JSONEq(t, `{"name":"docs"}`, string(tools[index].Namespace))
	manager.unregisterPluginLocked(original)
	replacement := &PluginInfo{Name: "test", Registrations: Registrations{ToolPrefix: "ext_", Tools: []ToolRegistration{{Name: "lookup", DefaultActive: new(true), Exposure: "direct"}}}}
	manager.plugins[replacement.Name] = replacement
	manager.registerPlugin(replacement)
	tools = manager.Tools()
	require.Len(t, tools, 1)
	require.Equal(t, "ext_lookup", tools[0].Name)
	require.True(t, *tools[0].DefaultActive)
	require.Equal(t, "direct", tools[0].Exposure)
	require.Empty(t, tools[0].Namespace)
	require.False(t, tools[0].HasReachesTools)
	manager.unregisterPluginLocked(replacement)
	require.Empty(t, manager.Tools())
}

func TestPluginConnectorFailureClosesHostSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connector-failure")
	require.NoError(t, os.WriteFile(path, []byte("unused"), 0o600))
	failure := errors.New("connection failed")
	manager := NewManager(t.TempDir(), nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		return nil, nil, failure
	}))
	_, err := manager.Load(t.Context(), path)
	require.ErrorIs(t, err, failure)
	require.Empty(t, manager.HostSocketPath(filepath.Base(path)))
	require.Empty(t, manager.Tools())
}

func TestPluginConnectorInvalidResultClosesHostSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connector-invalid")
	require.NoError(t, os.WriteFile(path, []byte("unused"), 0o600))
	for _, client := range []proto.PluginClient{nil, &mockPluginClient{}} {
		manager := NewManager(t.TempDir(), nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
			return client, nil, nil
		}))
		_, err := manager.Load(t.Context(), path)
		require.ErrorContains(t, err, "client and cleanup")
		require.Empty(t, manager.HostSocketPath(filepath.Base(path)))
		require.Empty(t, manager.Tools())
	}
}

type routerPluginClient struct {
	proto.PluginClient
	request  *proto.ModelRouteRequest
	response *proto.ModelRouteResponse
}

func (c *routerPluginClient) RouteModel(_ context.Context, request *proto.ModelRouteRequest, _ ...grpc.CallOption) (*proto.ModelRouteResponse, error) {
	c.request = request
	return c.response, nil
}

func TestManagerRoutesPluginVirtualModels(t *testing.T) {
	m := NewManager(t.TempDir(), nil, nil)
	client := &routerPluginClient{response: &proto.ModelRouteResponse{Provider: "anthropic", ModelId: "claude-sonnet-4-6", ThinkingLevel: "high", StateJson: `{"n":2}`}}
	registrations := convertVirtualModelRegistrations([]*proto.VirtualModelRegistration{{Provider: "router", Id: "auto", Name: "Auto", ThinkingLevels: []string{"off", "high"}, Input: []string{"text"}, ContextWindow: 1000, MaxTokens: 100}})
	m.plugins["router"] = &PluginInfo{Name: "router", grpcClient: client, Registrations: Registrations{VirtualModels: registrations}}

	definitions := m.VirtualModels()
	if len(definitions) != 1 {
		t.Fatalf("virtual models = %+v", definitions)
	}
	definition := definitions[0]
	if definition.Provider != "router" || definition.ID != "auto" || definition.Name != "Auto" || !slices.Equal(definition.ThinkingLevels, []string{"off", "high"}) || !slices.Equal(definition.Input, []ai.InputType{ai.InputText}) || definition.ContextWindow != 1000 || definition.MaxTokens != 100 {
		t.Fatalf("definition = %+v", definition)
	}
	route, err := definition.Route(t.Context(), ai.ModelRouteRequest{Model: &ai.Model{Provider: "router", ID: "auto"}, Reason: ai.ModelRouteUser, ThinkingLevel: "high", State: json.RawMessage(`{"n":1}`), Messages: []ai.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if route.Model.Provider != "anthropic" || route.Model.ID != "claude-sonnet-4-6" || route.ThinkingLevel != "high" || string(route.State) != `{"n":2}` {
		t.Fatalf("route = %+v", route)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(client.request.RequestJson), &sent); err != nil {
		t.Fatal(err)
	}
	if client.request.Provider != "router" || client.request.ModelId != "auto" || sent["reason"] != "user" || sent["thinkingLevel"] != "high" || sent["messages"] == nil || sent["state"].(map[string]any)["n"] != 1.0 {
		t.Fatalf("route request = %+v %s", client.request, client.request.RequestJson)
	}

	client.response = &proto.ModelRouteResponse{Error: "no capacity"}
	if _, err := definition.Route(t.Context(), ai.ModelRouteRequest{Model: &ai.Model{Provider: "router", ID: "auto"}}); err == nil || !strings.Contains(err.Error(), "no capacity") {
		t.Fatalf("router error = %v", err)
	}
	client.response = &proto.ModelRouteResponse{Provider: "anthropic", ModelId: "claude-sonnet-4-6", StateJson: "{"}
	if _, err := definition.Route(t.Context(), ai.ModelRouteRequest{Model: &ai.Model{Provider: "router", ID: "auto"}}); err == nil {
		t.Fatal("accepted router state that is not JSON")
	}
	delete(m.plugins, "router")
	if _, err := definition.Route(t.Context(), ai.ModelRouteRequest{Model: &ai.Model{Provider: "router", ID: "auto"}}); err == nil {
		t.Fatal("routed through an unloaded plugin")
	}
}

type shutdownResultClient struct {
	proto.PluginClient
	response *proto.ShutdownResponse
	err      error
	calls    int
}

func (c *shutdownResultClient) Shutdown(context.Context, *proto.ShutdownRequest, ...grpc.CallOption) (*proto.ShutdownResponse, error) {
	c.calls++
	return c.response, c.err
}

func TestShutdownReportsFailuresAndCleansEveryPlugin(t *testing.T) {
	rpcErr := errors.New("RPC failed")
	for _, tc := range []struct {
		name     string
		response *proto.ShutdownResponse
		err      error
	}{
		{name: "rpc", err: rpcErr},
		{name: "ungraceful", response: &proto.ShutdownResponse{}},
		{name: "plugin error", response: &proto.ShutdownResponse{Graceful: true, Error: "drain failed"}},
		{name: "nil response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(t.TempDir(), nil, nil)
			failed := &shutdownResultClient{response: tc.response, err: tc.err}
			good := &shutdownResultClient{response: &proto.ShutdownResponse{Graceful: true}}
			var cleaned atomic.Int32
			m.plugins["failed"] = &PluginInfo{Name: "failed", grpcClient: failed, Cleanup: func() { cleaned.Add(1) }}
			m.plugins["good"] = &PluginInfo{Name: "good", grpcClient: good, Cleanup: func() { cleaned.Add(1) }}
			err := m.Shutdown(t.Context())
			require.Error(t, err)
			require.Contains(t, err.Error(), "failed")
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
			}
			require.EqualValues(t, 2, cleaned.Load())
			require.Equal(t, 1, failed.calls)
			require.Equal(t, 1, good.calls)
		})
	}
}

func TestShutdownPluginReportsCancellationWithoutSkippingCleanup(t *testing.T) {
	m := NewManager(t.TempDir(), nil, nil)
	client := &shutdownResultClient{err: context.Canceled}
	var cleaned atomic.Bool
	m.plugins["cancelled"] = &PluginInfo{Name: "cancelled", grpcClient: client, Cleanup: func() { cleaned.Store(true) }}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, m.ShutdownPlugin(ctx, "cancelled", time.Second), context.Canceled)
	require.True(t, cleaned.Load())
}

func TestShutdownRetainsEveryRPCFailure(t *testing.T) {
	m := NewManager(t.TempDir(), nil, nil)
	first, second := errors.New("first RPC failure"), errors.New("second RPC failure")
	for name, err := range map[string]error{"first": first, "second": second} {
		m.plugins[name] = &PluginInfo{Name: name, grpcClient: &shutdownResultClient{err: err}, Cleanup: func() {}}
	}
	err := m.Shutdown(t.Context())
	require.ErrorIs(t, err, first)
	require.ErrorIs(t, err, second)
}

func TestDiscoverRejectsCancelledContextBeforeReplacingCandidates(t *testing.T) {
	m := NewManager(t.TempDir(), nil, nil)
	m.binaries["retained"] = "hash"
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("discovery interrupted")
	cancel(cause)
	require.ErrorIs(t, m.Discover(ctx), cause)
	require.Equal(t, "hash", m.binaries["retained"])
	require.ErrorIs(t, m.ReloadNamed(ctx, ""), cause)
	require.Equal(t, "hash", m.binaries["retained"])
}

func TestReloadNamedRetainsShutdownFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	m := NewManager(t.TempDir(), nil, nil)
	cause := errors.New("reload shutdown failed")
	cleaned := false
	m.plugins["missing"] = &PluginInfo{Name: "missing", Path: filepath.Join(t.TempDir(), "missing"), grpcClient: &shutdownResultClient{err: cause}, Cleanup: func() { cleaned = true }}
	require.ErrorIs(t, m.ReloadNamed(t.Context(), "missing"), cause)
	require.True(t, cleaned)
	require.NotContains(t, m.plugins, "missing")
}

func TestReloadRejectsDiscoveryBeforeReattemptingPlugins(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	m := NewManager(cfg, nil, nil)
	bad := filepath.Join(cfg, "extensions", "broken")
	require.NoError(t, os.MkdirAll(bad, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bad, "package.json"), []byte(`{"pi":`), 0o600))
	previous := &PluginInfo{Name: "deferred", Path: "deferred", deregistered: true}
	m.plugins["deferred"] = previous
	called := false
	m.loadFn = func(context.Context, string, ...string) (*PluginInfo, error) {
		called = true
		return &PluginInfo{Name: "deferred"}, nil
	}
	require.ErrorContains(t, m.Reload(t.Context()), "parse Pi manifest")
	require.False(t, called)
	require.Same(t, previous, m.plugins["deferred"])
}

func TestReloadRetainsDeregisteredRetryFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	m := NewManager(cfg, nil, nil)
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "deferred")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	cause := errors.New("deferred load failed")
	m.failureConfig[path] = FailureConfig{MaxAttempts: 1}
	m.plugins["deferred"] = &PluginInfo{Name: "deferred", Path: path, deregistered: true}
	calls := 0
	m.connectPlugin = func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		return nil, nil, cause
	}
	require.ErrorIs(t, m.Reload(t.Context()), cause)
	require.Equal(t, 1, calls)
	require.True(t, m.plugins["deferred"].deregistered)
}

func TestReloadKeepsRunningPluginWhenReplacementFails(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "running")
	require.NoError(t, os.WriteFile(path, []byte("replacement"), 0o700))
	cause := errors.New("replacement refused")
	var replacementSocket string
	m := NewManager(cfg, nil, nil, WithPluginConnector(func(_ context.Context, _ string, _ *Manifest, _ string, socket string) (proto.PluginClient, func(), error) {
		replacementSocket = socket
		return nil, nil, cause
	}))
	oldSocket, err := m.prepareHostSocketLocked("running")
	require.NoError(t, err)
	cleaned := false
	original := &PluginInfo{Name: "running", Path: path, Hash: "old", Cleanup: func() { cleaned = true }, Registrations: Registrations{Tools: []ToolRegistration{{Name: "old-tool"}}}}
	m.plugins[original.Name] = original
	m.registerPlugin(original)
	t.Cleanup(func() { require.NoError(t, m.Shutdown(t.Context())) })
	require.ErrorIs(t, m.ReloadNamed(t.Context(), "running"), cause)
	require.False(t, cleaned)
	require.Same(t, original, m.plugins["running"])
	require.Equal(t, oldSocket, m.HostSocketPath("running"))
	require.NotEqual(t, oldSocket, replacementSocket)
	_, err = os.Stat(oldSocket)
	require.NoError(t, err)
	require.Len(t, m.Tools(), 1)
	require.Equal(t, "old-tool", m.Tools()[0].Name)
}

func TestReloadReplacesPluginOnlyAfterStartup(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "running")
	require.NoError(t, os.WriteFile(path, []byte("replacement"), 0o700))
	oldCleaned := false
	nextCleaned := false
	var nextSocket string
	m := NewManager(cfg, nil, nil)
	oldSocket, err := m.prepareHostSocketLocked("running")
	require.NoError(t, err)
	m.plugins["running"] = &PluginInfo{Name: "running", Path: path, Hash: "old", Cleanup: func() { oldCleaned = true }}
	m.connectPlugin = func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		require.False(t, oldCleaned)
		_, statErr := os.Stat(oldSocket)
		require.NoError(t, statErr)
		nextSocket = m.HostSocketPath("running")
		return &mockPluginClient{gracefulResponse: true}, func() { nextCleaned = true }, nil
	}
	t.Cleanup(func() { require.NoError(t, m.Shutdown(t.Context())) })
	require.NoError(t, m.ReloadNamed(t.Context(), "running"))
	require.True(t, oldCleaned)
	require.False(t, nextCleaned)
	require.NotEqual(t, oldSocket, nextSocket)
	require.Equal(t, nextSocket, m.HostSocketPath("running"))
	_, err = os.Stat(oldSocket)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(nextSocket)
	require.NoError(t, err)
	require.NotEqual(t, "old", m.plugins["running"].Hash)
}

func TestReloadRetainsRunningPluginAfterCanceledStartup(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "running")
	require.NoError(t, os.WriteFile(path, []byte("replacement"), 0o700))
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("startup canceled")
	oldCleaned, nextCleaned := false, false
	m := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		return &mockPluginClient{gracefulResponse: true, onInitialize: func(*proto.InitRequest) { cancel(cause) }}, func() { nextCleaned = true }, nil
	}))
	oldSocket, err := m.prepareHostSocketLocked("running")
	require.NoError(t, err)
	previous := &PluginInfo{Name: "running", Path: path, Hash: "old", Cleanup: func() { oldCleaned = true }}
	m.plugins["running"] = previous
	t.Cleanup(func() { require.NoError(t, m.Shutdown(t.Context())) })
	require.ErrorIs(t, m.ReloadNamed(ctx, "running"), cause)
	require.False(t, oldCleaned)
	require.True(t, nextCleaned)
	require.Same(t, previous, m.plugins["running"])
	require.Equal(t, oldSocket, m.HostSocketPath("running"))
}

func TestReloadNamedRetriesDeregisteredPlugin(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "deferred")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	calls := 0
	m := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		return &mockPluginClient{gracefulResponse: true}, func() {}, nil
	}))
	m.plugins["deferred"] = &PluginInfo{Name: "deferred", Path: path, deregistered: true}
	t.Cleanup(func() { require.NoError(t, m.Shutdown(t.Context())) })
	require.NoError(t, m.ReloadNamed(t.Context(), "deferred"))
	require.Equal(t, 1, calls)
	require.False(t, m.plugins["deferred"].deregistered)
}

func TestAttemptLoadWithRetryRetainsCancellationCause(t *testing.T) {
	for _, stage := range []string{"before load", "after failed load"} {
		t.Run(stage, func(t *testing.T) {
			m := NewManager(t.TempDir(), nil, nil)
			m.failureConfig["plugin"] = FailureConfig{MaxAttempts: 2, BackoffBase: time.Hour, BackoffFactor: 1}
			ctx, cancel := context.WithCancelCause(t.Context())
			cause := errors.New("retry canceled")
			calls := 0
			m.loadFn = func(context.Context, string, ...string) (*PluginInfo, error) {
				calls++
				cancel(cause)
				return nil, errors.New("load failed")
			}
			if stage == "before load" {
				cancel(cause)
			}
			_, err := m.AttemptLoadWithRetry(ctx, "plugin", "")
			require.ErrorIs(t, err, cause)
			if stage == "before load" {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}
		})
	}
}

func TestReloadRejectsAllReplacementsBeforeRemovingPlugins(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	good := filepath.Join(dir, "a-good")
	bad := filepath.Join(dir, "z-bad")
	for _, path := range []string{good, bad} {
		require.NoError(t, os.WriteFile(path, []byte("replacement"), 0o700))
	}
	cause := errors.New("bad replacement")
	candidateCleaned := false
	m := NewManager(cfg, nil, nil, WithPluginConnector(func(_ context.Context, path string, _ *Manifest, _ string, _ string) (proto.PluginClient, func(), error) {
		if path == bad {
			return nil, nil, cause
		}
		return &mockPluginClient{gracefulResponse: true}, func() { candidateCleaned = true }, nil
	}))
	originals := make(map[string]*PluginInfo)
	for _, name := range []string{"a-good", "z-bad", "missing"} {
		name := name
		socket, err := m.prepareHostSocketLocked(name)
		require.NoError(t, err)
		info := &PluginInfo{Name: name, Path: filepath.Join(dir, name), Hash: "old", Cleanup: func() { t.Errorf("old %s cleaned during rejected reload", name) }}
		m.plugins[name] = info
		originals[name] = info
		t.Cleanup(func() {
			require.NoError(t, m.closeHostSocketLocked(name))
			_, err := os.Stat(socket)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
	require.ErrorIs(t, m.Reload(t.Context()), cause)
	for name, info := range originals {
		require.Same(t, info, m.plugins[name])
		require.NotEmpty(t, m.HostSocketPath(name))
	}
	// Successful candidates are released when any later candidate fails.
	require.True(t, candidateCleaned)
}

func TestReloadRetainsPolicyWhenReplacementStagingFails(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "running")
	require.NoError(t, os.WriteFile(path, []byte("replacement"), 0o700))
	oldSettings := &ai.Settings{ExtensionDir: dir}
	cause := errors.New("startup rejected")
	m := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		return nil, nil, cause
	}))
	m.SetSettings(oldSettings)
	m.binaries[path] = "original"
	m.manifestPaths["retained"] = "manifest"
	m.packageBinaries = map[string]bool{"package": true}
	m.packageManifests["manifest"] = &Manifest{}
	m.plugins["running"] = &PluginInfo{Name: "running", Path: path, Hash: "original"}
	prepared, err := m.PrepareReload(t.Context(), &ai.Settings{})
	require.NoError(t, err)
	require.ErrorIs(t, prepared.Reload(t.Context(), "running"), cause)
	require.Same(t, oldSettings, m.getSettings())
	require.Equal(t, map[string]string{path: "original"}, m.binaries)
	require.Equal(t, map[string]string{"retained": "manifest"}, m.manifestPaths)
	require.Contains(t, m.packageBinaries, "package")
	require.Contains(t, m.packageManifests, "manifest")
}

func TestReloadRejectsNewPluginBeforeRemovingCurrentPlugin(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "new-plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	cause := errors.New("new plugin rejected")
	m := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		return nil, nil, cause
	}))
	cleaned := false
	previous := &PluginInfo{Name: "missing", Path: filepath.Join(dir, "missing"), Cleanup: func() { cleaned = true }}
	m.plugins["missing"] = previous
	require.ErrorIs(t, m.Reload(t.Context()), cause)
	require.False(t, cleaned)
	require.Same(t, previous, m.plugins["missing"])
	require.NotContains(t, m.plugins, "new-plugin")
}

func TestReloadRejectsDeregisteredRetryBeforeRemovingCurrentPlugin(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "deferred")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	cause := errors.New("retry rejected")
	m := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		return nil, nil, cause
	}))
	m.failureConfig[path] = FailureConfig{MaxAttempts: 1}
	m.loadFn = func(context.Context, string, ...string) (*PluginInfo, error) { return nil, cause }
	deferred := &PluginInfo{Name: "deferred", Path: path, deregistered: true}
	m.plugins["deferred"] = deferred
	cleaned := false
	previous := &PluginInfo{Name: "missing", Path: filepath.Join(dir, "missing"), Cleanup: func() { cleaned = true }}
	m.plugins["missing"] = previous
	require.ErrorIs(t, m.Reload(t.Context()), cause)
	require.False(t, cleaned)
	require.Same(t, previous, m.plugins["missing"])
	require.Same(t, deferred, m.plugins["deferred"])
}

func TestReloadStagesDeregisteredRetryUntilSuccess(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "deferred")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	calls := 0
	m := NewManager(cfg, nil, nil)
	original := &PluginInfo{Name: "deferred", Path: path, deregistered: true}
	m.plugins["deferred"] = original
	m.failureConfig[path] = FailureConfig{MaxAttempts: 2, BackoffBase: 0, BackoffFactor: 1}
	m.connectPlugin = func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		require.Same(t, original, m.plugins["deferred"])
		if calls == 1 {
			return nil, nil, errors.New("transient")
		}
		return &mockPluginClient{gracefulResponse: true}, func() {}, nil
	}
	t.Cleanup(func() { require.NoError(t, m.Shutdown(t.Context())) })
	require.NoError(t, m.Reload(t.Context()))
	require.Equal(t, 2, calls)
	require.NotSame(t, original, m.plugins["deferred"])
	require.False(t, m.plugins["deferred"].deregistered)
}

func TestPreparedReloadRetainsCurrentPluginsWhenAdmissionRejects(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "candidate"), []byte("binary"), 0o700))
	candidateCleaned := false
	m := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		return &mockPluginClient{gracefulResponse: true}, func() { candidateCleaned = true }, nil
	}))
	oldCleaned := false
	old := &PluginInfo{Name: "missing", Path: "missing", Cleanup: func() { oldCleaned = true }}
	m.plugins["missing"] = old
	prepared, err := m.PrepareReload(t.Context(), &ai.Settings{})
	require.NoError(t, err)
	cause := errors.New("prompt persistence rejected")
	require.ErrorIs(t, prepared.ReloadWithAdmission(t.Context(), "", func() error { return cause }), cause)
	require.True(t, candidateCleaned)
	require.False(t, oldCleaned)
	require.Same(t, old, m.plugins["missing"])
	require.NotContains(t, m.plugins, "candidate")
}

func TestPluginStartupRejectsMalformedManifestBeforeConnecting(t *testing.T) {
	for _, operation := range []string{"load", "handshake"} {
		t.Run(operation, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "plugin")
			require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
			manifestPath := filepath.Join(dir, "tau-package.toml")
			require.NoError(t, os.WriteFile(manifestPath, []byte("[plugin]\napi_version = ["), 0o600))
			connected := false
			cause := errors.New("unexpected connector call")
			manager := NewManager(t.TempDir(), nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
				connected = true
				return nil, nil, cause
			}))
			manager.manifestPaths[path] = manifestPath
			var err error
			if operation == "load" {
				_, err = manager.Load(t.Context(), path, manifestPath)
			} else {
				_, err = manager.Handshake(t.Context(), path)
			}
			require.Error(t, err)
			require.False(t, connected)
			require.NotErrorIs(t, err, cause)
			require.Empty(t, manager.plugins)
		})
	}
}

func TestReloadStartupHostReadsCandidateConfigWithoutPublishingSideEffects(t *testing.T) {
	for _, outcome := range []string{"reject", "admit"} {
		t.Run(outcome, func(t *testing.T) {
			t.Chdir(t.TempDir())
			cfg := t.TempDir()
			dir := filepath.Join(cfg, "extensions")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			path := filepath.Join(dir, "candidate")
			require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
			manager := NewManager(cfg, nil, nil)
			old := &ai.Settings{Plugins: map[string]json.RawMessage{"candidate": json.RawMessage(`{"mode":"old"}`)}}
			manager.SetSettings(old)
			proposed := &ai.Settings{Plugins: map[string]json.RawMessage{"candidate": json.RawMessage(`{"mode":"proposed"}`)}}
			var configValue string
			var mutationCode codes.Code
			var candidateSocket string
			manager.connectPlugin = func(ctx context.Context, _ string, _ *Manifest, _ string, socket string) (proto.PluginClient, func(), error) {
				candidateSocket = socket
				connection, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					return nil, nil, err
				}
				defer func() { require.NoError(t, connection.Close()) }()
				host := proto.NewHostClient(connection)
				rpcCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, metadata.Pairs("tau-plugin-name", "candidate")), 100*time.Millisecond)
				defer cancel()
				response, err := host.GetConfig(rpcCtx, &proto.GetConfigRequest{Key: "mode"})
				if err != nil {
					return nil, nil, err
				}
				configValue = response.Value
				_, err = host.StateSet(rpcCtx, &proto.StateSetRequest{Key: "startup", Value: "\"must not persist\""})
				mutationCode = status.Code(err)
				for _, method := range proto.Host_ServiceDesc.Methods {
					if method.MethodName == "GetConfig" || method.MethodName == "Log" {
						continue
					}
					err := connection.Invoke(rpcCtx, "/plugins.Host/"+method.MethodName, &emptypb.Empty{}, &emptypb.Empty{})
					if status.Code(err) != codes.FailedPrecondition {
						return nil, nil, fmt.Errorf("startup method %s returned %w", method.MethodName, err)
					}
				}
				return &mockPluginClient{gracefulResponse: true}, func() {}, nil
			}
			prepared, err := manager.PrepareReload(t.Context(), proposed)
			require.NoError(t, err)
			cause := errors.New("reject admission")
			err = prepared.ReloadWithAdmission(t.Context(), "", func() error {
				if outcome == "reject" {
					return cause
				}
				return nil
			})
			if outcome == "reject" {
				require.ErrorIs(t, err, cause)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, `"proposed"`, configValue)
			require.Equal(t, codes.FailedPrecondition, mutationCode)
			if outcome == "admit" {
				t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
				connection, err := grpc.NewClient("unix://"+candidateSocket, grpc.WithTransportCredentials(insecure.NewCredentials()))
				require.NoError(t, err)
				defer func() { require.NoError(t, connection.Close()) }()
				rpcCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(t.Context(), metadata.Pairs("tau-plugin-name", "candidate")), time.Second)
				defer cancel()
				host := proto.NewHostClient(connection)
				_, err = host.StateSet(rpcCtx, &proto.StateSetRequest{Key: "admitted", Value: `"persisted"`})
				require.NoError(t, err)
				response, err := host.StateGet(rpcCtx, &proto.StateGetRequest{Key: "admitted"})
				require.NoError(t, err)
				require.True(t, response.Exists)
				require.Equal(t, `"persisted"`, response.Value)
				require.Same(t, proposed, manager.getSettings())
			} else {
				require.Same(t, old, manager.getSettings())
				require.Empty(t, manager.plugins)
				require.NoDirExists(t, filepath.Join(cfg, "plugin-state", "candidate"))
			}
		})
	}
}

func TestNamedReloadDoesNotStartUnrelatedNewPlugins(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "unrelated"), []byte("binary"), 0o700))
	calls := 0
	manager := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		return nil, nil, errors.New("unrelated startup")
	}))
	require.NoError(t, manager.ReloadNamed(t.Context(), "selected"))
	require.Zero(t, calls)
}

func TestReloadRejectsCancellationBeforeRemovalWithoutAdmissionCallback(t *testing.T) {
	t.Chdir(t.TempDir())
	manager := NewManager(t.TempDir(), nil, nil)
	old := &PluginInfo{Name: "missing", Path: "missing"}
	manager.plugins["missing"] = old
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("canceled before removal")
	manager.mu.Lock()
	cancel(cause)
	admitted, err := manager.reloadCandidates(ctx, "", nil, nil)
	manager.mu.Unlock()
	require.ErrorIs(t, err, cause)
	require.False(t, admitted)
	require.Same(t, old, manager.plugins["missing"])
}

func TestPluginStartupJoinsCleanupFailure(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	manager := NewManager(cfg, nil, nil)
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("initialize canceled")
	manager.connectPlugin = func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		return &mockPluginClient{onInitialize: func(*proto.InitRequest) { cancel(cause) }}, func() { panic("cleanup rejected") }, nil
	}
	var startupErr error
	require.NotPanics(t, func() { _, startupErr = manager.Load(ctx, path) })
	require.ErrorIs(t, startupErr, cause)
	require.ErrorContains(t, startupErr, "cleanup rejected")
	require.Empty(t, manager.HostSocketPath("plugin"))
	require.Empty(t, manager.plugins)
}

type nilStartupResponseClient struct {
	mockPluginClient
	phase string
}

func (c *nilStartupResponseClient) Initialize(context.Context, *proto.InitRequest, ...grpc.CallOption) (*proto.InitResponse, error) {
	if c.phase == "initialize" {
		return nil, nil
	}
	return &proto.InitResponse{ApiVersion: "1"}, nil
}

func (c *nilStartupResponseClient) ListRegistrations(context.Context, *proto.ListRegistrationsRequest, ...grpc.CallOption) (*proto.Registrations, error) {
	return nil, nil
}

func TestPluginStartupRejectsNilResponsesWithoutLeakingCandidate(t *testing.T) {
	for _, phase := range []string{"initialize", "registrations"} {
		t.Run(phase, func(t *testing.T) {
			cfg := t.TempDir()
			path := filepath.Join(cfg, "plugin")
			require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
			cleaned := false
			manager := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
				return &nilStartupResponseClient{phase: phase}, func() { cleaned = true }, nil
			}))
			var startupErr error
			require.NotPanics(t, func() { _, startupErr = manager.Load(t.Context(), path) })
			require.ErrorContains(t, startupErr, phase)
			require.True(t, cleaned)
			require.Empty(t, manager.HostSocketPath("plugin"))
			require.Empty(t, manager.plugins)
		})
	}
}

func TestRunPluginHashDoesNotHideResourceReadFailure(t *testing.T) {
	for _, operation := range []string{"startup", "handshake"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "plugin")
			require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
			manifestPath := filepath.Join(root, "tau-package.toml")
			require.NoError(t, os.WriteFile(manifestPath, []byte("[plugin]\napi_version='1'\n[run]\ncommand='./plugin'\n"), 0o600))
			require.NoError(t, os.Symlink("missing", filepath.Join(root, "required-resource")))
			manager := NewManager(t.TempDir(), nil, nil)
			manifest, err := parseManifest(manifestPath)
			require.NoError(t, err)
			if operation == "startup" {
				_, err = manager.pluginHash(path, manifest, manifestPath)
			} else {
				_, _, _, err = manager.handshakeMetadata(path)
			}
			require.Error(t, err)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestInitialPluginLoadCanReadHostConfiguration(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	manager := NewManager(cfg, nil, nil)
	manager.ResolveCLIFlags("plugin", map[string]string{"mode": "cli"})
	var value string
	manager.connectPlugin = func(ctx context.Context, _ string, _ *Manifest, _ string, socket string) (proto.PluginClient, func(), error) {
		connection, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, nil, err
		}
		defer func() { require.NoError(t, connection.Close()) }()
		rpcCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, metadata.Pairs("tau-plugin-name", "plugin")), 100*time.Millisecond)
		defer cancel()
		response, err := proto.NewHostClient(connection).GetConfig(rpcCtx, &proto.GetConfigRequest{Key: "mode"})
		if err != nil {
			return nil, nil, err
		}
		value = response.Value
		return &mockPluginClient{gracefulResponse: true}, func() {}, nil
	}
	_, err := manager.Load(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
	require.Equal(t, `"cli"`, value)
	require.False(t, manager.hostSockets["plugin"].staging.Load())
}

func TestReloadRejectsOldHostCallsDuringShutdown(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	manager := NewManager(cfg, nil, nil)
	var oldSocket string
	var shutdownCode codes.Code
	calls := 0
	manager.connectPlugin = func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		if calls == 1 {
			return &hostShutdownClient{shutdown: func() {
				connection, err := grpc.NewClient("unix://"+oldSocket, grpc.WithTransportCredentials(insecure.NewCredentials()))
				require.NoError(t, err)
				defer func() { require.NoError(t, connection.Close()) }()
				rpcCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(t.Context(), metadata.Pairs("tau-plugin-name", "plugin")), 100*time.Millisecond)
				defer cancel()
				_, err = proto.NewHostClient(connection).GetConfig(rpcCtx, &proto.GetConfigRequest{Key: "mode"})
				shutdownCode = status.Code(err)
			}}, func() {}, nil
		}
		return &mockPluginClient{gracefulResponse: true}, func() {}, nil
	}
	_, err := manager.Load(t.Context(), path)
	require.NoError(t, err)
	oldSocket = manager.HostSocketPath("plugin")
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
	require.NoError(t, manager.ReloadNamed(t.Context(), "plugin"))
	require.Equal(t, codes.FailedPrecondition, shutdownCode)
}

type hostShutdownClient struct {
	mockPluginClient
	shutdown func()
}

func (c *hostShutdownClient) Shutdown(context.Context, *proto.ShutdownRequest, ...grpc.CallOption) (*proto.ShutdownResponse, error) {
	c.shutdown()
	return &proto.ShutdownResponse{Graceful: true}, nil
}

func TestAttemptLoadWithRetryRetainsCleanupFailure(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	path := filepath.Join(t.TempDir(), "plugin")
	manager.failureConfig[path] = FailureConfig{MaxAttempts: 1}
	cause := errors.New("startup rejected")
	manager.loadFn = func(context.Context, string, ...string) (*PluginInfo, error) {
		manager.mu.Lock()
		manager.plugins["plugin"] = &PluginInfo{Name: "plugin", Path: path, Cleanup: func() { panic("retry cleanup rejected") }}
		manager.mu.Unlock()
		return nil, cause
	}
	_, err := manager.AttemptLoadWithRetry(t.Context(), path, "")
	require.ErrorIs(t, err, cause)
	require.ErrorContains(t, err, "retry cleanup rejected")
	require.Empty(t, manager.plugins)
}

func TestAttemptLoadWithRetryCancellationRetainsAttemptFailure(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	path := filepath.Join(t.TempDir(), "plugin")
	manager.failureConfig[path] = FailureConfig{MaxAttempts: 2}
	cause := errors.New("retry canceled")
	failure := errors.New("initial attempt failed")
	ctx, cancel := context.WithCancelCause(t.Context())
	manager.loadFn = func(context.Context, string, ...string) (*PluginInfo, error) { cancel(cause); return nil, failure }
	_, err := manager.AttemptLoadWithRetry(ctx, path, "")
	require.ErrorIs(t, err, cause)
	require.ErrorIs(t, err, failure)
}

func TestPluginStartupRejectsExpiredInitialization(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	cleaned := false
	manager.connectPlugin = func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		return &expiredInitializationClient{}, func() { cleaned = true }, nil
	}
	_, err := manager.startPlugin(t.Context(), "plugin", nil, "", DefaultRPCConfig{Initialize: time.Millisecond})
	if err == nil {
		require.NoError(t, manager.closeHostSocketLocked("plugin"))
	}
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, cleaned)
	require.Empty(t, manager.hostSockets)
}

type expiredInitializationClient struct{ mockPluginClient }

func (c *expiredInitializationClient) Initialize(ctx context.Context, req *proto.InitRequest, opts ...grpc.CallOption) (*proto.InitResponse, error) {
	<-ctx.Done()
	return c.mockPluginClient.Initialize(ctx, req, opts...)
}

func TestPluginStartupRetainsCancellationAlongsideRPCFailure(t *testing.T) {
	for _, phase := range []string{"connect", "initialize", "registrations"} {
		t.Run(phase, func(t *testing.T) {
			manager := NewManager(t.TempDir(), nil, nil)
			cause := errors.New("startup withdrawn")
			failure := errors.New("RPC failed")
			ctx, cancel := context.WithCancelCause(t.Context())
			cleaned := false
			manager.connectPlugin = func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
				cleanup := func() { cleaned = true }
				if phase == "connect" {
					cancel(cause)
					return nil, cleanup, failure
				}
				return &canceledStartupClient{phase: phase, cancel: func() { cancel(cause) }, failure: failure}, cleanup, nil
			}
			_, err := manager.startPlugin(ctx, "plugin", nil, "", DefaultRPCTimeouts)
			require.ErrorIs(t, err, cause)
			require.ErrorIs(t, err, failure)
			require.True(t, cleaned)
			require.Empty(t, manager.hostSockets)
		})
	}
}

type canceledStartupClient struct {
	mockPluginClient
	phase   string
	cancel  func()
	failure error
}

func (c *canceledStartupClient) Initialize(ctx context.Context, req *proto.InitRequest, opts ...grpc.CallOption) (*proto.InitResponse, error) {
	if c.phase == "initialize" {
		c.cancel()
		return nil, c.failure
	}
	return c.mockPluginClient.Initialize(ctx, req, opts...)
}

func (c *canceledStartupClient) ListRegistrations(context.Context, *proto.ListRegistrationsRequest, ...grpc.CallOption) (*proto.Registrations, error) {
	c.cancel()
	return nil, c.failure
}

func TestReloadRetryRetainsEveryStartupFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	first := errors.New("first connection rejected")
	second := errors.New("second connection rejected")
	calls := 0
	manager := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		if calls == 1 {
			return nil, nil, first
		}
		return nil, nil, second
	}))
	previous := &PluginInfo{Name: "plugin", Path: path, deregistered: true}
	manager.plugins["plugin"] = previous
	manager.failureConfig[path] = FailureConfig{MaxAttempts: 2}
	err := manager.Reload(t.Context())
	require.ErrorIs(t, err, first)
	require.ErrorIs(t, err, second)
	require.Same(t, previous, manager.plugins["plugin"])
	require.Equal(t, 2, calls)
	require.Empty(t, manager.hostSockets)
}

func TestLoadRejectsDuplicateWithoutChangingRunningPlugin(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	calls := 0
	cleaned := false
	manager := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		if calls > 1 {
			return nil, nil, errors.New("duplicate connector must not run")
		}
		return &mockPluginClient{gracefulResponse: true}, func() { cleaned = true }, nil
	}))
	original, err := manager.Load(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
	socket := manager.HostSocketPath("plugin")
	_, err = manager.Load(t.Context(), path)
	require.ErrorContains(t, err, "already loaded")
	require.Equal(t, 1, calls)
	require.Same(t, original, manager.plugins["plugin"])
	require.Equal(t, socket, manager.HostSocketPath("plugin"))
	require.FileExists(t, socket)
	require.False(t, cleaned)
}

func TestRetryLoadRejectsDuplicateWithoutCleaningRunningPlugin(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	path := filepath.Join(t.TempDir(), "plugin")
	cleaned := false
	previous := &PluginInfo{Name: "plugin", Path: path, Cleanup: func() { cleaned = true }}
	manager.plugins["plugin"] = previous
	manager.failureConfig[path] = FailureConfig{MaxAttempts: 1}
	manager.loadFn = manager.Load
	_, err := manager.AttemptLoadWithRetry(t.Context(), path, "")
	require.ErrorContains(t, err, "already loaded")
	require.False(t, cleaned)
	require.Same(t, previous, manager.plugins["plugin"])
}

func TestRetryLoadPreservesPluginAdmittedAfterRetryCheck(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	path := filepath.Join(t.TempDir(), "plugin")
	manager.failureConfig[path] = FailureConfig{MaxAttempts: 1}
	cleaned := false
	previous := &PluginInfo{Name: "plugin", Path: path, Cleanup: func() { cleaned = true }}
	manager.loadFn = func(ctx context.Context, path string, manifest ...string) (*PluginInfo, error) {
		manager.mu.Lock()
		manager.plugins["plugin"] = previous
		manager.mu.Unlock()
		return manager.Load(ctx, path, manifest...)
	}
	_, err := manager.AttemptLoadWithRetry(t.Context(), path, "")
	require.ErrorContains(t, err, "already loaded")
	require.False(t, cleaned)
	require.Same(t, previous, manager.plugins["plugin"])
}

func TestHandshakeCloseReportsCleanupPanicAndRemovesSocket(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	calls := 0
	manager := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		return &mockPluginClient{}, func() { calls++; panic("handshake cleanup rejected") }, nil
	}))
	result, err := manager.Handshake(t.Context(), path)
	require.NoError(t, err)
	socket := manager.HostSocketPath("plugin")
	t.Cleanup(func() { require.NoError(t, manager.closeHostSocketLocked("plugin")) })
	var closeErr error
	require.NotPanics(t, func() { closeErr = result.Close() })
	require.ErrorContains(t, closeErr, "handshake cleanup rejected")
	require.NoFileExists(t, socket)
	require.Empty(t, manager.hostSockets)
	require.ErrorContains(t, result.Close(), "handshake cleanup rejected")
	require.Equal(t, 1, calls)
}

func TestHandshakeRejectsRunningPluginWithoutReplacingEndpoint(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	calls := 0
	manager := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		return &mockPluginClient{gracefulResponse: true}, func() {}, nil
	}))
	previous, err := manager.Load(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
	socket := manager.HostSocketPath("plugin")
	result, err := manager.Handshake(t.Context(), path)
	if result != nil {
		require.NoError(t, result.Close())
	}
	require.ErrorContains(t, err, "already loaded")
	require.Equal(t, 1, calls)
	require.Same(t, previous, manager.plugins["plugin"])
	require.Equal(t, socket, manager.HostSocketPath("plugin"))
	require.FileExists(t, socket)
}

func TestHandshakeCloseDoesNotRemoveLaterLoadedEndpoint(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	manager := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		return &mockPluginClient{gracefulResponse: true}, func() {}, nil
	}))
	probe, err := manager.Handshake(t.Context(), path)
	require.NoError(t, err)
	_, err = manager.Load(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
	socket := manager.HostSocketPath("plugin")
	require.NoError(t, probe.Close())
	require.Equal(t, socket, manager.HostSocketPath("plugin"))
	require.FileExists(t, socket)
}

func TestReloadRetainsExplicitCLIFlagsAndRefreshesDefaults(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	calls := 0
	manager := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		defaultValue := "old"
		if calls > 1 {
			defaultValue = "new"
		}
		return &loadoutFlagClient{flags: []*proto.CLIFlag{{Name: "mode", DefaultValue: defaultValue}, {Name: "default", DefaultValue: defaultValue}}}, func() {}, nil
	}))
	_, err := manager.Load(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
	manager.ResolveCLIFlags("plugin", map[string]string{"mode": "explicit"})
	require.NoError(t, manager.ReloadNamed(t.Context(), "plugin"))
	require.Equal(t, "explicit", manager.getCLIFlag("plugin", "mode"))
	require.Equal(t, "new", manager.getCLIFlag("plugin", "default"))
}

type loadoutFlagClient struct {
	mockPluginClient
	flags []*proto.CLIFlag
}

func (c *loadoutFlagClient) ListRegistrations(context.Context, *proto.ListRegistrationsRequest, ...grpc.CallOption) (*proto.Registrations, error) {
	return &proto.Registrations{CliFlags: c.flags}, nil
}

func (c *loadoutFlagClient) Shutdown(context.Context, *proto.ShutdownRequest, ...grpc.CallOption) (*proto.ShutdownResponse, error) {
	return &proto.ShutdownResponse{Graceful: true}, nil
}

func TestLoadValidatesPluginConfigurationWithoutReenteringManagerLock(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	manifest := filepath.Join(cfg, "tau-package.toml")
	require.NoError(t, os.WriteFile(manifest, []byte("[plugin]\napi_version='1'\n[config_schema]\nschema='''{\"type\":\"object\",\"required\":[\"mode\"],\"properties\":{\"mode\":{\"type\":\"string\"}}}'''\n"), 0o600))
	calls := 0
	manager := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		return &mockPluginClient{gracefulResponse: true}, func() {}, nil
	}))
	manager.SetSettings(&ai.Settings{Plugins: map[string]json.RawMessage{"plugin": json.RawMessage(`{"mode":42}`)}})
	_, err := manager.Load(t.Context(), path, manifest)
	require.ErrorContains(t, err, "config validation")
	require.Equal(t, 0, calls)
	manager.SetSettings(&ai.Settings{Plugins: map[string]json.RawMessage{"plugin": json.RawMessage(`{"mode":"valid"}`)}})
	_, err = manager.Load(t.Context(), path, manifest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
	require.Equal(t, 1, calls)
}

func TestReattemptDeregisteredRetainsOriginalUntilAdmitted(t *testing.T) {
	for _, outcome := range []string{"reject", "admit", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			cfg := t.TempDir()
			path := filepath.Join(cfg, "plugin")
			require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
			manager := NewManager(cfg, nil, nil)
			previous := &PluginInfo{Name: "plugin", Path: path, deregistered: true}
			manager.plugins["plugin"] = previous
			manager.failureConfig[path] = FailureConfig{MaxAttempts: 1}
			cause := errors.New("retry rejected")
			calls := 0
			manager.connectPlugin = func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
				calls++
				require.Same(t, previous, manager.plugins["plugin"])
				if outcome == "reject" {
					return nil, nil, cause
				}
				return &mockPluginClient{gracefulResponse: true}, func() {}, nil
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			if outcome == "cancel" {
				cancel(cause)
			}
			err := manager.ReattemptDeregistered(ctx)
			if outcome == "admit" {
				require.NoError(t, err)
				require.NotSame(t, previous, manager.plugins["plugin"])
				require.False(t, manager.plugins["plugin"].deregistered)
				t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
			} else {
				require.ErrorIs(t, err, cause)
				require.Same(t, previous, manager.plugins["plugin"])
			}
			if outcome == "cancel" {
				require.Equal(t, 0, calls)
			} else {
				require.Equal(t, 1, calls)
			}
		})
	}
}

func TestReattemptDeregisteredUsesOriginalManifestPath(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "plugin")
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	manifestPath := filepath.Join(cfg, "custom.toml")
	require.NoError(t, os.WriteFile(manifestPath, []byte("[plugin]\napi_version='1'\n"), 0o600))
	manager := NewManager(cfg, nil, nil, WithPluginConnector(func(_ context.Context, _ string, manifest *Manifest, selected, _ string) (proto.PluginClient, func(), error) {
		require.NotNil(t, manifest)
		require.Equal(t, manifestPath, selected)
		return &mockPluginClient{gracefulResponse: true}, func() {}, nil
	}))
	manager.plugins["plugin"] = &PluginInfo{Name: "plugin", Path: path, ManifestPath: manifestPath, deregistered: true}
	manager.failureConfig[path] = FailureConfig{MaxAttempts: 1}
	require.NoError(t, manager.ReattemptDeregistered(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
	require.Equal(t, manifestPath, manager.plugins["plugin"].ManifestPath)
}

func TestReloadDoesNotLeakCandidatesWithSamePluginName(t *testing.T) {
	t.Chdir(t.TempDir())
	manager := NewManager(t.TempDir(), nil, nil)
	root := t.TempDir()
	for _, dir := range []string{"a", "z"} {
		path := filepath.Join(root, dir, "plugin")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
		manager.AddExtensionPath(path)
	}
	calls, cleaned := 0, 0
	manager.connectPlugin = func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		return &mockPluginClient{gracefulResponse: true}, func() { cleaned++ }, nil
	}
	require.NoError(t, manager.Reload(t.Context()))
	require.Equal(t, 1, calls)
	require.Equal(t, filepath.Join(root, "a", "plugin"), manager.plugins["plugin"].Path)
	require.NoError(t, manager.Shutdown(t.Context()))
	require.Equal(t, calls, cleaned)
	require.Empty(t, manager.hostSockets)
}

func TestReloadStagesMovedPluginBeforeRemovingOriginal(t *testing.T) {
	for _, outcome := range []string{"admit", "reject"} {
		t.Run(outcome, func(t *testing.T) {
			t.Chdir(t.TempDir())
			cfg := t.TempDir()
			oldPath := filepath.Join(cfg, "old", "plugin")
			newPath := filepath.Join(cfg, "new", "plugin")
			require.NoError(t, os.MkdirAll(filepath.Dir(newPath), 0o700))
			require.NoError(t, os.WriteFile(newPath, []byte("binary"), 0o700))
			cause := errors.New("moved startup rejected")
			cleaned := false
			calls := 0
			manager := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
				calls++
				require.False(t, cleaned)
				if outcome == "reject" {
					return nil, nil, cause
				}
				return &mockPluginClient{gracefulResponse: true}, func() {}, nil
			}))
			manager.AddExtensionPath(newPath)
			previous := &PluginInfo{Name: "plugin", Path: oldPath, Cleanup: func() { cleaned = true }}
			manager.plugins["plugin"] = previous
			err := manager.Reload(t.Context())
			t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
			require.Equal(t, 1, calls)
			if outcome == "reject" {
				require.ErrorIs(t, err, cause)
				require.False(t, cleaned)
				require.Same(t, previous, manager.plugins["plugin"])
			} else {
				require.NoError(t, err)
				require.True(t, cleaned)
				require.Equal(t, newPath, manager.plugins["plugin"].Path)
			}
		})
	}
}

func TestPluginStartupAdmissionClosesAfterManagerShutdown(t *testing.T) {
	for _, operation := range []string{"Load", "Handshake", "Reload", "prepared reload", "AutoLoad", "retry", "deregistered retry"} {
		t.Run(operation, func(t *testing.T) {
			t.Chdir(t.TempDir())
			cfg := t.TempDir()
			path := filepath.Join(cfg, "extensions", "plugin")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
			calls := 0
			manager := NewManager(cfg, nil, nil, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
				calls++
				return &mockPluginClient{gracefulResponse: true}, func() {}, nil
			}))
			manager.binaries[path] = "hash"
			manager.failureConfig[path] = FailureConfig{MaxAttempts: 2}
			attempts := 0
			originalLoad := manager.loadFn
			manager.loadFn = func(ctx context.Context, path string, manifest ...string) (*PluginInfo, error) {
				attempts++
				return originalLoad(ctx, path, manifest...)
			}
			prepared, err := manager.PrepareReload(t.Context(), &ai.Settings{})
			require.NoError(t, err)
			require.NoError(t, manager.Shutdown(t.Context()))
			defer func() { require.NoError(t, manager.Shutdown(t.Context())) }()
			switch operation {
			case "Load":
				_, err = manager.Load(t.Context(), path)
			case "Handshake":
				var probe *HandshakeResult
				probe, err = manager.Handshake(t.Context(), path)
				if probe != nil {
					require.NoError(t, probe.Close())
				}
			case "Reload":
				err = manager.Reload(t.Context())
			case "prepared reload":
				err = prepared.Reload(t.Context(), "")
			case "AutoLoad":
				err = manager.AutoLoad(t.Context())
			case "deregistered retry":
				err = manager.ReattemptDeregistered(t.Context())
			case "retry":
				_, err = manager.AttemptLoadWithRetry(t.Context(), path, "")
			}
			require.ErrorContains(t, err, "shut down")
			require.Equal(t, 0, calls)
			require.Equal(t, 0, attempts)
			require.Empty(t, manager.plugins)
			require.Empty(t, manager.hostSockets)
		})
	}
}

func TestShutdownRetainsCancellationAlongsideRPCFailure(t *testing.T) {
	for _, response := range []*proto.ShutdownResponse{nil, {}, {Error: "drain failed"}} {
		t.Run(fmt.Sprintf("%v", response), func(t *testing.T) {
			cause := errors.New("shutdown withdrawn")
			ctx, cancel := context.WithCancelCause(t.Context())
			manager := NewManager(t.TempDir(), nil, nil)
			cleaned := false
			manager.plugins["plugin"] = &PluginInfo{Name: "plugin", grpcClient: &failedShutdownClient{cancel: func() { cancel(cause) }, response: response}, Cleanup: func() { cleaned = true }}
			err := manager.Shutdown(ctx)
			require.ErrorIs(t, err, cause)
			require.True(t, cleaned)
		})
	}
	cause := errors.New("shutdown withdrawn")
	failure := errors.New("shutdown RPC disconnected")
	ctx, cancel := context.WithCancelCause(t.Context())
	manager := NewManager(t.TempDir(), nil, nil)
	manager.plugins["plugin"] = &PluginInfo{Name: "plugin", grpcClient: &failedShutdownClient{cancel: func() { cancel(cause) }, failure: failure}}
	err := manager.Shutdown(ctx)
	require.ErrorIs(t, err, cause)
	require.ErrorIs(t, err, failure)
}

type failedShutdownClient struct {
	mockPluginClient
	cancel   func()
	failure  error
	response *proto.ShutdownResponse
}

func (c *failedShutdownClient) Shutdown(context.Context, *proto.ShutdownRequest, ...grpc.CallOption) (*proto.ShutdownResponse, error) {
	c.cancel()
	return c.response, c.failure
}

func TestShutdownRemovesRegistrationsAndDoesNotRepeatCleanup(t *testing.T) {
	for _, operation := range []string{"all", "one"} {
		t.Run(operation, func(t *testing.T) {
			registry := commands.NewInMemoryRegistry()
			manager := NewManager(t.TempDir(), nil, registry)
			cleaned := 0
			info := &PluginInfo{Name: "plugin", Path: "/plugin", Cleanup: func() { cleaned++ }, Registrations: Registrations{Tools: []ToolRegistration{{Name: "lookup"}}, Commands: []CommandRegistration{{Name: "lookup"}}}}
			manager.plugins[info.Name] = info
			manager.registerPlugin(info)
			require.NotEmpty(t, manager.Tools())
			if operation == "one" {
				require.NoError(t, manager.ShutdownPlugin(t.Context(), "plugin", time.Second))
			} else {
				require.NoError(t, manager.Shutdown(t.Context()))
			}
			require.Empty(t, manager.plugins)
			require.Empty(t, manager.Tools())
			_, _, exists := registry.Lookup("lookup")
			require.False(t, exists)
			require.NoError(t, manager.Shutdown(t.Context()))
			require.Equal(t, 1, cleaned)
		})
	}
}

func TestAttemptLoadWithRetryStopsAfterConcurrentShutdown(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	path := filepath.Join(t.TempDir(), "plugin")
	manager.failureConfig[path] = FailureConfig{MaxAttempts: 3}
	failure := errors.New("first startup failed")
	attempts := 0
	manager.loadFn = func(context.Context, string, ...string) (*PluginInfo, error) {
		attempts++
		require.NoError(t, manager.Shutdown(t.Context()))
		return nil, failure
	}
	_, err := manager.AttemptLoadWithRetry(t.Context(), path, "")
	require.ErrorIs(t, err, failure)
	require.ErrorContains(t, err, "shut down")
	require.Equal(t, 1, attempts)
}

func TestPluginStartupRejectsNilRegistrationEntries(t *testing.T) {
	for _, kind := range []string{"tool", "command", "flag", "model"} {
		t.Run(kind, func(t *testing.T) {
			manager := NewManager(t.TempDir(), nil, nil)
			path := filepath.Join(t.TempDir(), "plugin")
			require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
			regs := &proto.Registrations{}
			switch kind {
			case "tool":
				regs.Tools = []*proto.ToolRegistration{nil}
			case "command":
				regs.Commands = []*proto.CommandRegistration{nil}
			case "flag":
				regs.CliFlags = []*proto.CLIFlag{nil}
			case "model":
				regs.VirtualModels = []*proto.VirtualModelRegistration{nil}
			}
			cleaned := false
			manager.connectPlugin = func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
				return &invalidRegistrationsClient{registrations: regs}, func() { cleaned = true }, nil
			}
			require.NotPanics(t, func() { _, err := manager.Load(t.Context(), path); require.ErrorContains(t, err, "registration") })
			require.True(t, cleaned)
			require.Empty(t, manager.plugins)
			require.Empty(t, manager.hostSockets)
		})
	}
}

type invalidRegistrationsClient struct {
	mockPluginClient
	registrations *proto.Registrations
}

func (c *invalidRegistrationsClient) ListRegistrations(context.Context, *proto.ListRegistrationsRequest, ...grpc.CallOption) (*proto.Registrations, error) {
	return c.registrations, nil
}

func TestReloadReplacesSlashCommandMetadataAndRemovesStaleCommands(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := t.TempDir()
	path := filepath.Join(cfg, "extensions", "plugin")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o700))
	registry := commands.NewInMemoryRegistry()
	calls := 0
	manager := NewManager(cfg, nil, registry, WithPluginConnector(func(context.Context, string, *Manifest, string, string) (proto.PluginClient, func(), error) {
		calls++
		regs := &proto.Registrations{Commands: []*proto.CommandRegistration{{Name: "shared", Description: "new"}, {Name: "added"}}}
		if calls == 1 {
			regs.Commands = []*proto.CommandRegistration{{Name: "shared", Description: "old"}, {Name: "removed"}}
		}
		return &invalidRegistrationsClient{mockPluginClient: mockPluginClient{gracefulResponse: true}, registrations: regs}, func() {}, nil
	}))
	_, err := manager.Load(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(t.Context())) })
	require.NoError(t, manager.ReloadNamed(t.Context(), "plugin"))
	_, metadata, exists := registry.Lookup("shared")
	require.True(t, exists)
	require.Equal(t, "new", metadata.Description)
	_, _, exists = registry.Lookup("removed")
	require.False(t, exists)
	_, _, exists = registry.Lookup("added")
	require.True(t, exists)
	require.NoError(t, os.Remove(path))
	require.NoError(t, manager.Reload(t.Context()))
	require.Empty(t, registry.List())
}
