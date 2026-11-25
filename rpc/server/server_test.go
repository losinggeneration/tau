package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/agent/artifacts"
	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
)

func bashResponse(t *testing.T, sess *mockSession, ctx context.Context, args string) Response {
	t.Helper()
	var output bytes.Buffer
	server := &Server{writer: &output, session: sess}
	if err := server.handleBash(ctx, sess, &Command{ID: "bash", Args: json.RawMessage(args)}); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestRPCBashMapsExecutorOutcomes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{fmt.Errorf("%w: denied", sdk.ErrBashBlocked), "blocked"},
		{agent.ErrInvalidUserBashOverride, "invalid_override"},
		{io.ErrUnexpectedEOF, "error"},
	} {
		sess := newMockSession("s", "/")
		sess.bash = func(context.Context, string, sdk.BashOptions) (sdk.BashExecution, error) {
			return sdk.BashExecution{}, tc.err
		}
		response := bashResponse(t, sess, t.Context(), `{"command":"unsafe"}`)
		if response.OK || response.Error == nil || response.Error.Code != tc.code {
			t.Fatalf("%v: %+v", tc.err, response)
		}
	}
	sess := newMockSession("s", "/")
	var got sdk.BashOptions
	sess.bash = func(_ context.Context, command string, options sdk.BashOptions) (sdk.BashExecution, error) {
		got = options
		options.OnOutput("partial")
		return sdk.BashExecution{Command: command, Output: "intercepted", ExitCode: 7}, nil
	}
	var output bytes.Buffer
	server := &Server{writer: &output, session: sess}
	if err := server.handleBash(t.Context(), sess, &Command{ID: "bash", Args: json.RawMessage(`{"command":"ls","dir":"/tmp","timeout_s":3,"excludeFromContext":true}`)}); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n"))
	if len(lines) != 2 || !bytes.Contains(lines[0], []byte(`"type":"bash_execution_update"`)) || !bytes.Contains(lines[0], []byte(`"delta":"partial"`)) {
		t.Fatalf("output not streamed before result: %s", output.String())
	}
	var response Response
	if err := json.Unmarshal(lines[1], &response); err != nil {
		t.Fatal(err)
	}
	result, _ := response.Result.(map[string]any)
	if !response.OK || result["output"] != "intercepted" || result["exit_code"] != float64(7) {
		t.Fatalf("result lost: %+v", response)
	}
	if !got.ExcludeFromContext || got.Dir != "/tmp" || got.Timeout != 3*time.Second {
		t.Fatalf("options lost: %+v", got)
	}
}

func TestCancelledRPCShellDoesNotExecute(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sess := newMockSession("s", "/")
	sess.bash = func(context.Context, string, sdk.BashOptions) (sdk.BashExecution, error) {
		t.Fatal("cancelled request reached the executor")
		return sdk.BashExecution{}, nil
	}
	if response := bashResponse(t, sess, ctx, `{"command":"unsafe"}`); response.OK {
		t.Fatal("cancelled shell reported success")
	}
}

func TestEditContextRequiresExplicitReplacement(t *testing.T) {
	for _, args := range []string{`{}`, `{"entry_id":"target"}`, `{"replacement":null}`, `invalid`} {
		var output bytes.Buffer
		s := &Server{writer: &output}
		if err := s.handleCommand(context.Background(), &Command{ID: "edit", Type: "edit_context", Args: json.RawMessage(args)}); err != nil {
			t.Fatal(err)
		}
		var response Response
		if err := json.Unmarshal(output.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.OK || response.Error == nil {
			t.Fatalf("expected validation error: %s", output.String())
		}
	}
}

func TestBashAdmitsAbortAndShutdownJoins(t *testing.T) {
	started, settled := make(chan struct{}), make(chan struct{})
	sess := newMockSession("s", "/")
	sess.bash = func(ctx context.Context, _ string, _ sdk.BashOptions) (sdk.BashExecution, error) {
		close(started)
		<-ctx.Done()
		close(settled)
		return sdk.BashExecution{Cancelled: true, CancelReason: string(agent.BashCancelUserAbort)}, nil
	}
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()
	h.srv.session = sess
	response := func(id string) Response {
		t.Helper()
		lines := h.readUntil(t, timeout, func(line string) bool {
			var r Response
			return json.Unmarshal([]byte(line), &r) == nil && r.Type == "response" && r.ID == id
		})
		if len(lines) == 0 {
			t.Fatalf("no response for %s", id)
		}
		var r Response
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}

	h.send(t, `{"id":"shell","type":"bash","args":{"command":"wait"}}`)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shell did not start")
	}
	h.send(t, `{"id":"second","type":"bash","args":{"command":"wait"}}`)
	if r := response("second"); r.OK || r.Error == nil || r.Error.Code != "busy" {
		t.Fatalf("second shell = %+v, want busy", r)
	}
	h.send(t, `{"id":"abort","type":"abort_bash"}`)
	if r := response("abort"); !r.OK || r.Result.(map[string]any)["aborted"] != true {
		t.Fatalf("abort = %+v", r)
	}
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("abort did not reach the running shell")
	}
	h.send(t, `{"id":"shutdown","type":"shutdown"}`)
	if r := response("shutdown"); !r.OK {
		t.Fatalf("shutdown = %+v", r)
	}
	waitOrFail(t, h.done, timeout, "shutdown joining the shell")
}

type closeErrorSession struct {
	sdk.Session
	err    error
	closed bool
	ctx    context.Context
	ctxErr error
}

func (s *closeErrorSession) Close(ctx context.Context) error {
	s.closed = true
	s.ctx, s.ctxErr = ctx, ctx.Err()
	return s.err
}

func TestCleanupGivesSessionBoundedLiveContextAfterCancellation(t *testing.T) {
	sess := &closeErrorSession{Session: newMockSession("cleanup", "/")}
	s := newServer(nil, nil, nil)
	s.session = sess
	s.writer = io.Discard
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.cleanup(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if sess.ctx == nil {
		t.Fatal("session was not closed")
	}
	if err := sess.ctxErr; err != nil {
		t.Fatalf("session close context already ended: %v", err)
	}
	if deadline, ok := sess.ctx.Deadline(); !ok || time.Until(deadline) > sessionCleanupTimeout {
		t.Fatalf("session close context is not bounded: %v %v", deadline, ok)
	}
}

type shutdownErrorWriter struct{ err error }

func (w shutdownErrorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestCleanupReportsSessionAndEventErrors(t *testing.T) {
	closeErr, writeErr := errors.New("close failed"), errors.New("write failed")
	sess := &closeErrorSession{Session: newMockSession("cleanup", "/"), err: closeErr}
	s := newServer(nil, nil, nil)
	s.session = sess
	s.writer = shutdownErrorWriter{err: writeErr}
	err := s.cleanup(t.Context())
	if !errors.Is(err, closeErr) || !errors.Is(err, writeErr) {
		t.Fatalf("cleanup lost errors: %v", err)
	}
	if !sess.closed {
		t.Fatal("session was not closed")
	}
}

func TestRebindSessionReportsPreviousCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	sess := &closeErrorSession{Session: newMockSession("previous", "/"), err: closeErr}
	s := newServer(nil, nil, nil)
	s.session = sess
	next := newMockSession("next", "/")
	if err := s.rebindSession(t.Context(), next); !errors.Is(err, closeErr) {
		t.Fatalf("rebind lost close error: %v", err)
	}
	if s.session != next {
		t.Fatal("replacement session was not installed")
	}
	if s.sessionCancel != nil {
		s.sessionCancel()
	}
}

func TestPromptDispositionsAdmitCommandsWhileRunning(t *testing.T) {
	run := make(chan sdk.Event, 4)
	running := false
	sess := newMockSession("s", "/")
	sess.send = func(_ context.Context, msg sdk.Message) (<-chan sdk.Event, error) {
		if msg.Text == "/name" {
			handled := make(chan sdk.Event, 2)
			handled <- sdk.Event{Type: "session_start"}
			handled <- sdk.Event{Type: "command_result", Payload: map[string]any{"output": "ok"}}
			close(handled)
			return handled, nil
		}
		if running {
			return nil, fmt.Errorf("send: %w", sdk.ErrSessionBusy)
		}
		running = true
		run <- sdk.Event{Type: "session_start"}
		run <- sdk.Event{Type: "agent_start"}
		return run, nil
	}
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()
	h.srv.session = sess

	response := func(id string) map[string]any {
		t.Helper()
		lines := h.readUntil(t, timeout, func(line string) bool {
			var obj map[string]any
			return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == id
		})
		if len(lines) == 0 {
			t.Fatalf("no response for %s", id)
		}
		return parseResp(t, lines[len(lines)-1])
	}
	disposition := func(resp map[string]any) any {
		result, _ := resp["result"].(map[string]any)
		return result["disposition"]
	}

	h.send(t, `{"id":"first","type":"prompt","args":{"text":"work"}}`)
	if got := disposition(response("first")); got != "started" {
		t.Fatalf("first disposition = %v", got)
	}
	h.send(t, `{"id":"busy","type":"send_message","args":{"text":"more"}}`)
	if resp := response("busy"); resp["ok"] != false {
		t.Fatalf("prompt without streaming_behavior during a run = %v", resp)
	}
	h.send(t, `{"id":"later","type":"prompt","args":{"text":"more","streaming_behavior":"follow_up"}}`)
	if got := disposition(response("later")); got != "queued" || sess.followUpCount != 1 {
		t.Fatalf("follow-up prompt disposition = %v, queued %d", got, sess.followUpCount)
	}
	h.send(t, `{"id":"now","type":"prompt","args":{"text":"more","streamingBehavior":"steer"}}`)
	if got := disposition(response("now")); got != "queued" || sess.steeringCount != 1 {
		t.Fatalf("steer prompt disposition = %v, queued %d", got, sess.steeringCount)
	}
	h.send(t, `{"id":"steer","type":"steer","args":{"text":"adjust"}}`)
	if got := disposition(response("steer")); got != "queued" {
		t.Fatalf("steer disposition = %v", got)
	}
	h.send(t, `{"id":"follow","type":"follow_up","args":{"text":"next"}}`)
	if got := disposition(response("follow")); got != "queued" {
		t.Fatalf("follow_up disposition = %v", got)
	}
	h.send(t, `{"id":"cmd","type":"prompt","args":{"text":"/name"}}`)
	if got := disposition(response("cmd")); got != "handled" {
		t.Fatalf("command disposition = %v", got)
	}
	h.send(t, `{"id":"abort","type":"abort"}`)
	if resp := response("abort"); resp["ok"] != true {
		t.Fatalf("abort during a run = %v", resp)
	}
	run <- sdk.Event{Type: "agent_end"}
	close(run)
	h.readUntil(t, timeout, func(line string) bool { return strings.Contains(line, `"agent_end"`) })
}

type reloadableRPCSession struct {
	sdk.Session
	called bool
	cause  error
}

func (s *reloadableRPCSession) ReloadResources(context.Context, *ai.Settings, *artifacts.PreparedReload, *plugins.PreparedReload, string) error {
	s.called = true
	return s.cause
}

func TestRPCCommandSessionForwardsResourceReload(t *testing.T) {
	cause := errors.New("reload failed")
	session := &reloadableRPCSession{cause: cause}
	server := &Server{session: session}
	adapter := any(&rpcCommandSession{server: server})
	reloader, ok := adapter.(sdk.ResourceReloader)
	if !ok {
		t.Fatal("RPC command adapter hides coordinated reload")
	}
	if err := reloader.ReloadResources(t.Context(), &ai.Settings{}, nil, nil, "worker"); !errors.Is(err, cause) || !session.called {
		t.Fatalf("reload lost: %v", err)
	}
	server.session = nil
	if err := reloader.ReloadResources(t.Context(), &ai.Settings{}, nil, nil, ""); err == nil {
		t.Fatal("reload without session succeeded")
	}
	server.session = newMockSession("unsupported", "/")
	if err := reloader.ReloadResources(t.Context(), &ai.Settings{}, nil, nil, ""); err == nil {
		t.Fatal("unsupported reload succeeded")
	}
}

func TestRPCCommandUIRegistersBeforePublishing(t *testing.T) {
	s := newServer(nil, nil, nil)
	s.ui = ui.NewRPCUI()
	s.ui.Writer = rpcUIWriter{server: s}
	// An immediate reply during Write used to arrive before WaitForResponse
	// registered the request, leaving the slash command blocked until timeout.
	s.writer = immediateUIReplyWriter{ui: s.ui}
	adapter := rpcCommandUI{server: s}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if confirmed, err := adapter.Confirm(ctx, "Confirm", "Proceed?"); err != nil || !confirmed {
		t.Fatalf("confirm = %v, %v", confirmed, err)
	}
	if selected, err := adapter.Select(ctx, "Select", []string{"first", "second"}); err != nil || selected != 1 {
		t.Fatalf("select = %d, %v", selected, err)
	}
	if value, err := adapter.Input(ctx, "Input", "value"); err != nil || value != "second" {
		t.Fatalf("input = %q, %v", value, err)
	}
}

// Without a coordinated worker, ui_response keeps its direct reply shape;
// shared snapshots are coordinated-only.
func TestStandaloneUIResponseKeepsReplyShape(t *testing.T) {
	var out bytes.Buffer
	s := newServer(nil, nil, nil)
	s.writer = &out
	s.ui = ui.NewRPCUI()
	if err := s.handleCommand(t.Context(), &Command{ID: "reply", Type: "ui_response", Args: json.RawMessage(`{"id":"unknown","value":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.handleCommand(t.Context(), &Command{ID: "list", Type: "get_ui_requests"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("responses: %q", lines)
	}
	if lines[0] != `{"type":"response","id":"reply","ok":true,"result":{"ok":true}}` {
		t.Fatalf("standalone ui_response reply = %s", lines[0])
	}
	var list Response
	if err := json.Unmarshal([]byte(lines[1]), &list); err != nil {
		t.Fatal(err)
	}
	if list.OK || list.Error == nil || list.Error.Code != "not_supported" {
		t.Fatalf("uncoordinated get_ui_requests = %+v", list)
	}
}

type immediateUIReplyWriter struct{ ui *ui.RPCUI }

func (w immediateUIReplyWriter) Write(data []byte) (int, error) {
	var event struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return 0, err
	}
	confirmed := true
	w.ui.DispatchResponseTyped(ui.UIResponse{ID: event.ID, Value: "second", Confirmed: &confirmed})
	return len(data), nil
}

func TestEmitEventSessionSnapshotDoesNotTakeSessionLock(t *testing.T) {
	s := newServer(nil, nil, nil)
	s.writer = io.Discard
	s.eventSession.Store(&eventSession{id: "initial"})
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 50 {
				s.eventSession.Store(&eventSession{id: "next"})
				if err := s.emitEvent(sdk.Event{Type: "queue_update"}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("event observer deadlocked against session Close")
	}
}

func TestFollowUpQueuesUploadedAttachment(t *testing.T) {
	sess := newMockSession("s", t.TempDir())
	record, err := sess.ImportAttachmentData("note.txt", "text/plain", "bm90ZQ==")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	s := &Server{writer: &output, session: sess, coordinated: true}
	cmd := &Command{ID: "f", Type: "follow_up", Args: fmt.Appendf(nil, `{"text":"use note","attachment_ids":[%q],"expected_session_id":"s"}`, record.ID)}
	if err := s.handleCommand(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	var reply Response
	if err := json.Unmarshal(lastResponseLine(output.Bytes()), &reply); err != nil {
		t.Fatal(err)
	}
	if !reply.OK || len(sess.followUps) != 1 || sess.followUps[0].Text != "use note" || !slices.Equal(sess.followUps[0].AttachmentIDs, []string{record.ID}) {
		t.Fatalf("reply = %s, queued messages = %+v", &output, sess.followUps)
	}
}

func TestStaleSessionRejectsDraftSubmission(t *testing.T) {
	for _, tc := range []struct{ typ, args string }{
		{"send_message", `{"text":"draft","expected_session_id":"previous"}`},
		{"follow_up", `{"text":"draft","expected_session_id":"previous"}`},
		{"attach_data", `{"filename":"note.txt","mime_type":"text/plain","data":"bm90ZQ==","expected_session_id":"previous"}`},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			sess := newMockSession("current", t.TempDir())
			var output bytes.Buffer
			s := &Server{writer: &output, session: sess, coordinated: true}
			if err := s.handleCommand(t.Context(), &Command{ID: "stale", Type: tc.typ, Args: json.RawMessage(tc.args)}); err != nil {
				t.Fatal(err)
			}
			s.promptWG.Wait()
			var reply Response
			if err := json.Unmarshal(lastResponseLine(output.Bytes()), &reply); err != nil {
				t.Fatalf("decode %s: %v", &output, err)
			}
			if reply.OK || reply.Error == nil || reply.Error.Code != "session_changed" {
				t.Fatalf("reply = %s, want session_changed", &output)
			}
			if len(sess.sent) != 0 || sess.followUpCount != 0 || len(sess.attachments) != 0 {
				t.Fatal("stale command mutated the current session")
			}
		})
	}
}

func TestSendMessageRefusesRetryWhileBusy(t *testing.T) {
	sess := newMockSession("s", "/")
	sess.send = func(context.Context, sdk.Message) (<-chan sdk.Event, error) {
		return nil, fmt.Errorf("send: %w", sdk.ErrSessionBusy)
	}
	var output bytes.Buffer
	s := &Server{writer: &output, session: sess, coordinated: true}
	send := func(text string) Response {
		t.Helper()
		output.Reset()
		args, err := json.Marshal(map[string]string{"text": text})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.handleCommand(t.Context(), &Command{ID: "prompt", Type: "send_message", Args: args}); err != nil {
			t.Fatal(err)
		}
		var reply Response
		if err := json.Unmarshal(lastResponseLine(output.Bytes()), &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	reply := send(" /retry \n")
	if reply.OK || reply.Error == nil || reply.Error.Code != "busy" || sess.steeringCount != 0 || sess.followUpCount != 0 {
		t.Fatalf("reply = %+v, queued %d/%d", reply, sess.steeringCount, sess.followUpCount)
	}
	if reply := send("/retry please"); !reply.OK || sess.steeringCount != 1 {
		t.Fatalf("ordinary busy prompt was not steered: reply=%+v queue=%d", reply, sess.steeringCount)
	}
}

func TestGetStateReportsSessionSettings(t *testing.T) {
	sess := newSettingsMockSession("s")
	sess.autoCompaction, sess.steeringMode, sess.followUpMode, sess.thinkingLevel = true, "one-at-a-time", "all", "medium"
	var output bytes.Buffer
	s := &Server{writer: &output, session: sess}
	var state map[string]any
	if err := json.Unmarshal(runCommand(t, s, &output, "get_state", `{}`).Result, &state); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"auto_compaction": true,
		"auto_retry":      false,
		"steering_mode":   "one-at-a-time",
		"follow_up_mode":  "all",
		"thinking_level":  "medium",
	} {
		if got, ok := state[key]; !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", key, got, ok, want)
		}
	}

	s = &Server{writer: &output, session: newMockSession("plain", "/"), thinkingLevel: "low"}
	state = nil
	if err := json.Unmarshal(runCommand(t, s, &output, "get_state", `{}`).Result, &state); err != nil {
		t.Fatal(err)
	}
	if state["thinking_level"] != "low" {
		t.Errorf("thinking_level = %v, want the RPC level", state["thinking_level"])
	}
	for _, key := range []string{"auto_compaction", "auto_retry", "steering_mode", "follow_up_mode"} {
		if _, ok := state[key]; ok {
			t.Errorf("%s reported for a session without settings", key)
		}
	}
}

func TestGetAvailableModelsReportsCapabilities(t *testing.T) {
	sess := newSettingsMockSession("s")
	sess.models = []*ai.Model{nil, {ID: "m", Provider: "p", API: "openai-completions", Name: "Model", ContextWindow: 1000}}
	var output bytes.Buffer
	s := &Server{writer: &output, session: sess}
	wantResult(t, runCommand(t, s, &output, "get_available_models", `{}`), `{"models":[{"api":"openai-completions","context_window":1000,"id":"m","name":"Model","provider":"p","reasoning":false,"thinking_levels":["off"]}]}`)
}

func lastResponseLine(output []byte) []byte {
	output = bytes.TrimSpace(output)
	return output[bytes.LastIndexByte(output, '\n')+1:]
}

type forgetMockSession struct {
	*mockSession
	forgot []string
}

func (s *forgetMockSession) GetForgetEntries() ([]sdk.ForkEntry, error) {
	return []sdk.ForkEntry{{EntryID: "e1", Role: "user", Text: "first"}}, nil
}

func (s *forgetMockSession) Forget(entryID string) error {
	s.forgot = append(s.forgot, entryID)
	return nil
}

func TestForkGuardsExpectedSession(t *testing.T) {
	var out bytes.Buffer
	rt := &mockRuntime{}
	original := newMockSession("s", "/")
	s := &Server{writer: &out, runtime: rt, session: original}

	wantErrorCode(t, runCommand(t, s, &out, "fork", `{"expected_session_id":"previous","entry_id":"e1"}`), "session_changed")
	if rt.newCount != 0 || s.session != sdk.Session(original) {
		t.Fatalf("stale fork created %d sessions, active %q", rt.newCount, s.session.ID())
	}

	wantResult(t, runCommand(t, s, &out, "fork", `{"expected_session_id":"s","entry_id":"e1"}`), `{"ok":true,"session_id":"test-session"}`)
	if rt.newCount != 1 {
		t.Fatalf("guarded fork created %d sessions, want 1", rt.newCount)
	}

	wantResult(t, runCommand(t, s, &out, "fork", `{"entry_id":"e1"}`), `{"ok":true,"session_id":"test-session"}`)
	if rt.newCount != 2 {
		t.Fatalf("unguarded fork created %d sessions, want 2", rt.newCount)
	}
}

func TestForgetCommands(t *testing.T) {
	sess := &forgetMockSession{mockSession: newMockSession("s", t.TempDir())}
	var output bytes.Buffer
	s := &Server{writer: &output, session: sess, coordinated: true}
	run := func(typ, args string) []byte {
		t.Helper()
		output.Reset()
		if err := s.handleCommand(t.Context(), &Command{ID: typ, Type: typ, Args: json.RawMessage(args)}); err != nil {
			t.Fatal(err)
		}
		return output.Bytes()
	}

	var listed struct {
		Response
		Result struct {
			Entries []map[string]string `json:"entries"`
		} `json:"result"`
	}
	if err := json.Unmarshal(lastResponseLine(run("get_forget_entries", `{"expected_session_id":"s"}`)), &listed); err != nil {
		t.Fatal(err)
	}
	if want := []map[string]string{{"entry_id": "e1", "role": "user", "text": "first"}}; !listed.OK || fmt.Sprint(listed.Result.Entries) != fmt.Sprint(want) {
		t.Fatalf("entries reply = %s", &output)
	}

	out := run("forget", `{"expected_session_id":"previous","entry_id":"e1"}`)
	var reply Response
	if err := json.Unmarshal(lastResponseLine(out), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.OK || reply.Error == nil || reply.Error.Code != "session_changed" || len(sess.forgot) != 0 {
		t.Fatalf("stale forget reply = %s, forgot %v", out, sess.forgot)
	}

	out = run("forget", `{"expected_session_id":"s","entry_id":"e1"}`)
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	if len(lines) != 2 || !bytes.Contains(lines[0], []byte(`"type":"context_edited"`)) {
		t.Fatalf("forget output = %s, want context_edited before the reply", out)
	}
	if err := json.Unmarshal(lines[1], &reply); err != nil {
		t.Fatal(err)
	}
	if !reply.OK || !slices.Equal(sess.forgot, []string{"e1"}) {
		t.Fatalf("forget reply = %s, forgot %v", out, sess.forgot)
	}
}

type queueControlSession struct {
	*mockSession
	steerPaused, followPaused bool
	steer, follow             []sdk.Message
	run                       <-chan sdk.Event
}

func (s *queueControlSession) PauseQueue(kind sdk.QueueKind) error {
	if kind == sdk.QueueSteering {
		s.steerPaused = true
	} else {
		s.followPaused = true
	}
	return nil
}

func (s *queueControlSession) ResumeQueue(_ context.Context, kind sdk.QueueKind) (<-chan sdk.Event, error) {
	if kind == sdk.QueueSteering {
		s.steerPaused = false
	} else {
		s.followPaused = false
	}
	return s.run, nil
}

func (s *queueControlSession) QueuesPaused() (steering, followUp bool) {
	return s.steerPaused, s.followPaused
}

func (s *queueControlSession) ClearQueuedMessages(kind sdk.QueueKind) ([]sdk.Message, error) {
	queue := &s.steer
	if kind == sdk.QueueFollowUp {
		queue = &s.follow
	}
	cleared := *queue
	*queue = nil
	return cleared, nil
}

func TestQueueControlCommands(t *testing.T) {
	run := make(chan sdk.Event, 1)
	run <- sdk.Event{Type: "agent_start"}
	close(run)
	sess := &queueControlSession{
		mockSession: newMockSession("s", "/"),
		steer:       []sdk.Message{{Text: "steer"}},
		follow:      []sdk.Message{{Text: "follow one"}},
	}
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()
	h.srv.session = sess
	response := func(id, command string) map[string]any {
		t.Helper()
		h.send(t, command)
		lines := h.readUntil(t, timeout, func(line string) bool {
			var obj map[string]any
			return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == id
		})
		if len(lines) == 0 {
			t.Fatalf("no response for %s", id)
		}
		return parseResp(t, lines[len(lines)-1])
	}

	if resp := response("bad", `{"id":"bad","type":"pause_queue","args":{"queue":"nope"}}`); resp["ok"] != false {
		t.Fatalf("invalid queue = %v", resp)
	}
	if resp := response("unnamed", `{"id":"unnamed","type":"pause_queue"}`); resp["ok"] != false {
		t.Fatalf("pause without queue = %v", resp)
	}
	if resp := response("pause", `{"id":"pause","type":"pause_queue","args":{"queue":"follow_up"}}`); resp["ok"] != true {
		t.Fatalf("pause = %v", resp)
	}
	if sess.steerPaused || !sess.followPaused {
		t.Fatalf("paused steering=%v followUp=%v", sess.steerPaused, sess.followPaused)
	}
	state := response("state", `{"id":"state","type":"get_state"}`)["result"].(map[string]any)["queue"].(map[string]any)
	if state["steering_paused"] != false || state["follow_up_paused"] != true {
		t.Fatalf("state queue = %v", state)
	}

	cleared := response("one", `{"id":"one","type":"clear_queue","args":{"queue":"steer"}}`)["result"].(map[string]any)
	if fmt.Sprint(cleared["steering"]) != "[steer]" || fmt.Sprint(cleared["follow_up"]) != "[]" || len(sess.follow) != 1 {
		t.Fatalf("clear steering = %v, follow-up left %v", cleared, sess.follow)
	}
	cleared = response("all", `{"id":"all","type":"clear_queue"}`)["result"].(map[string]any)
	if fmt.Sprint(cleared["followUp"]) != "[follow one]" || fmt.Sprint(cleared["follow_up"]) != "[follow one]" {
		t.Fatalf("clear all = %v", cleared)
	}

	if got := response("idle", `{"id":"idle","type":"resume_queue","args":{"queue":"steer"}}`)["result"].(map[string]any)["disposition"]; got != "resumed" {
		t.Fatalf("resume without run = %v", got)
	}
	sess.run = run
	if got := response("resume", `{"id":"resume","type":"resume_queue","args":{"queue":"followUp"}}`)["result"].(map[string]any)["disposition"]; got != "started" {
		t.Fatalf("resume with run = %v", got)
	}
	if sess.followPaused {
		t.Fatal("follow-up queue still paused")
	}
}

type queueEditSession struct {
	*mockSession
	items []sdk.QueuedPrompt
}

func (s *queueEditSession) QueuedPrompts() []sdk.QueuedPrompt { return slices.Clone(s.items) }

func (s *queueEditSession) EditQueuedText(id, text string) bool {
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].ID, s.items[i].Text = id+"-edited", text
			return true
		}
	}
	return false
}

func TestEditQueuedTextPublishesQueueToOtherClients(t *testing.T) {
	sess := &queueEditSession{
		mockSession: newMockSession("s", "/"),
		items:       []sdk.QueuedPrompt{{ID: "q1", Text: "before", Kind: "follow_up", Editable: true}},
	}
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()
	h.srv.session = sess

	h.send(t, `{"id":"edit","type":"edit_queued_text","args":{"expected_session_id":"s","id":"q1","text":"after"}}`)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "edit"
	})
	var published bool
	for _, line := range lines {
		published = published || (strings.Contains(line, `"queue_update"`) && strings.Contains(line, `"after"`) && strings.Contains(line, `"q1-edited"`))
	}
	if !published {
		t.Fatalf("edit did not publish the changed queue: %q", lines)
	}
}

func TestRunPublishesQueueWhenAgentTakesMessage(t *testing.T) {
	run := make(chan sdk.Event, 4)
	sess := newMockSession("s", "/")
	sess.send = func(context.Context, sdk.Message) (<-chan sdk.Event, error) {
		run <- sdk.Event{Type: "agent_start"}
		return run, nil
	}
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()
	defer close(run)
	h.srv.session = sess
	responded := func(id string) func(string) bool {
		return func(line string) bool {
			var obj map[string]any
			return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == id
		}
	}
	h.send(t, `{"id":"first","type":"prompt","args":{"text":"work"}}`)
	h.readUntil(t, timeout, responded("first"))
	h.send(t, `{"id":"follow","type":"follow_up","args":{"text":"next"}}`)
	h.readUntil(t, timeout, responded("follow"))

	sess.followUpCount = 0
	run <- sdk.Event{Type: "message_start"}
	lines := h.readUntil(t, timeout, func(line string) bool {
		return strings.Contains(line, `"queue_update"`) && strings.Contains(line, `"follow_up":0`)
	})
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], `"queue_update"`) {
		t.Fatalf("taking a queued message published no update: %q", lines)
	}
}

type queueRestoreSession struct{ *mockSession }

func (s *queueRestoreSession) TakeQueuedText() (steering, followUp []string, kept int) {
	return []string{"steer"}, []string{"follow"}, 2
}

func TestTakeQueuedTextReportsKeptMessages(t *testing.T) {
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()
	h.srv.session = &queueRestoreSession{newMockSession("s", "/")}
	h.send(t, `{"id":"take","type":"take_queued_text","args":{"expected_session_id":"s"}}`)
	lines := h.readUntil(t, timeout, func(line string) bool {
		var obj map[string]any
		return json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == "response" && obj["id"] == "take"
	})
	result, _ := parseResp(t, lines[len(lines)-1])["result"].(map[string]any)
	if fmt.Sprint(result["steering"]) != "[steer]" || fmt.Sprint(result["follow_up"]) != "[follow]" || result["kept"] != float64(2) {
		t.Fatalf("take result = %v", result)
	}
}
