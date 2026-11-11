package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/agent/artifacts"
	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
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

type responseWriter struct{ responses chan []byte }

func (w *responseWriter) Write(p []byte) (int, error) {
	w.responses <- bytes.Clone(p)
	return len(p), nil
}

func TestRunAdmitsBashAbortAndJoins(t *testing.T) {
	input, writer := io.Pipe()
	defer func() {
		if err := input.Close(); err != nil {
			t.Error(err)
		}
	}()
	defer func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	}()
	started, settled := make(chan struct{}), make(chan struct{})
	sess := newMockSession("s", "/")
	sess.bash = func(ctx context.Context, _ string, _ sdk.BashOptions) (sdk.BashExecution, error) {
		close(started)
		<-ctx.Done()
		close(settled)
		return sdk.BashExecution{Cancelled: true, CancelReason: string(agent.BashCancelUserAbort)}, nil
	}
	output := &responseWriter{responses: make(chan []byte, 16)}
	server := NewServer(nil, nil, nil)
	server.reader = bufio.NewReader(input)
	server.writer = output
	server.session = sess
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	send := func(command string) {
		t.Helper()
		written := make(chan error, 1)
		go func() { _, err := io.WriteString(writer, command+"\n"); written <- err }()
		select {
		case err := <-written:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(200 * time.Millisecond):
			cancel()
			t.Fatal("RPC input loop blocked command admission")
		}
	}
	send(`{"id":"shell","type":"bash","args":{"command":"wait"}}`)
	select {
	case <-started:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("shell did not start")
	}
	send(`{"id":"second","type":"bash","args":{"command":"wait"}}`)
	send(`{"id":"abort","type":"abort_bash"}`)
	select {
	case <-settled:
	case <-time.After(200 * time.Millisecond):
		cancel()
		t.Fatal("RPC input loop could not deliver abort while shell was running")
	}
	send(`{"id":"shutdown","type":"shutdown"}`)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("server did not join shell")
	}
	var aborted, busy bool
	for len(output.responses) > 0 {
		var response struct {
			ID     string     `json:"id"`
			Error  *ErrorInfo `json:"error"`
			Result struct {
				Aborted bool `json:"aborted"`
			} `json:"result"`
		}
		if err := json.Unmarshal(<-output.responses, &response); err != nil {
			t.Fatal(err)
		}
		switch response.ID {
		case "abort":
			aborted = response.Result.Aborted
		case "second":
			busy = response.Error != nil && response.Error.Code == "busy"
		}
	}
	if !aborted || !busy {
		t.Fatalf("aborted=%v busy=%v", aborted, busy)
	}
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
	s := NewServer(nil, nil, nil)
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
	s := NewServer(nil, nil, nil)
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
	s := NewServer(nil, nil, nil)
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

func TestSendInterruptCancelsARunWithoutWaitingForInput(t *testing.T) {
	run := make(chan sdk.Event, 2)
	sess := newMockSession("s", "/")
	sess.send = func(context.Context, sdk.Message) (<-chan sdk.Event, error) {
		run <- sdk.Event{Type: "agent_start"}
		return run, nil
	}
	h := newTestHarness(t, &mockRuntime{}, nil)
	defer h.close()
	h.srv.session = sess
	h.send(t, `{"id":"run","type":"prompt","args":{"text":"work"}}`)
	h.readUntil(t, timeout, func(line string) bool { return strings.Contains(line, `"agent_start"`) })

	interrupted := make(chan struct{})
	go func() {
		defer close(interrupted)
		h.readUntil(t, timeout, func(line string) bool { return strings.Contains(line, `"type":"interrupt"`) })
	}()
	h.srv.SendInterrupt()
	select {
	case <-interrupted:
	case <-time.After(time.Second):
		t.Fatal("SIGINT waited for the next RPC command before interrupting the run")
	}
	if sess.interrupts != 1 {
		t.Fatalf("interrupts = %d", sess.interrupts)
	}
	close(run)
}

func TestReadCommandProcessesFinalRecordWithoutLF(t *testing.T) {
	for _, end := range []string{"", "\n", "\r\n"} {
		s := &Server{reader: bufio.NewReader(strings.NewReader("{\"id\":\"a\u2028b\u2029界\",\"type\":\"get_state\"}" + end))}
		cmd, err := s.readCommand()
		if err != nil {
			t.Fatalf("end %q: %v", end, err)
		}
		if cmd.ID != "a\u2028b\u2029界" || cmd.Type != "get_state" {
			t.Fatalf("command=%+v", cmd)
		}
		if _, err := s.readCommand(); !errors.Is(err, io.EOF) {
			t.Fatalf("expected EOF, got %v", err)
		}
	}
}

func TestReadCommandReportsMalformedFinalRecord(t *testing.T) {
	s := &Server{reader: bufio.NewReader(strings.NewReader("{\"type\":"))}
	if _, err := s.readCommand(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("expected JSON error, got %v", err)
	}
	if _, err := s.readCommand(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestRunHandlesFinalCommandBeforeDisconnect(t *testing.T) {
	var output bytes.Buffer
	s := NewServer(&mockRuntime{}, nil, nil)
	s.reader = bufio.NewReader(strings.NewReader(`{"id":"last","type":"unknown"}`))
	s.writer = &output
	if err := s.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	var response Response
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var r Response
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.ID == "last" {
			response = r
		}
	}
	if response.ID != "last" || response.OK || response.Error == nil || response.Error.Code != "unknown_command" {
		t.Fatalf("response=%+v output=%s", response, output.String())
	}
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
