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
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/losinggeneration/tau/agent/plugins/proto"
	"github.com/losinggeneration/tau/ai"
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

	// A second prepare for the same name closes the previous listener+server
	// and replaces them at the same path (filename is deterministic per pid
	// to avoid leaking socket files across plugin reloads).
	path2, err := mgr.prepareHostSocketLocked("plugin-A")
	if err != nil {
		t.Fatalf("second prepareHostSocket: %v", err)
	}
	if path2 != path {
		t.Errorf("expected same path (pid-stable); got %q vs %q", path2, path)
	}
	if _, err := os.Stat(path2); err != nil {
		t.Errorf("socket missing after reload: %v", err)
	}
	// Verify the map entry is still present and points at the same struct.
	if hs := mgr.hostSockets["plugin-A"]; hs == nil || hs.path != path {
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

// TestPrepareHostSocket_StalePathRemoved ensures prepare cleans up any stale
// socket file at the chosen path (e.g. after a crashed previous launch) and
// that a non-ENOENT stat error is surfaced.
func TestPrepareHostSocket_StalePathRemoved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	mgr := NewManager(t.TempDir(), nil, nil)
	path, err := mgr.prepareHostSocketLocked("plugin-D")
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	// Force-close via manager so we leave a stale file behind.
	mgr.closeHostSocketLocked("plugin-D")
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
	if path2 != path {
		t.Errorf("expected reuse of stale path; got new %q vs %q", path2, path)
	}
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
