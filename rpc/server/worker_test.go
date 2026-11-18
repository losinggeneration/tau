package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
)

type workerSession struct {
	*mockSession
	sendFn         func(context.Context, sdk.Message) (<-chan sdk.Event, error)
	compactFn      func(context.Context) error
	closeFn        func(context.Context) error
	interruptCount atomic.Int32
	steerCount     atomic.Int32
	observerReady  chan func(sdk.Event)
	messages       []sdk.Message
}

func (s *workerSession) Send(ctx context.Context, msg sdk.Message) (<-chan sdk.Event, error) {
	return s.sendFn(ctx, msg)
}
func (s *workerSession) GetMessages() []sdk.Message { return s.messages }

func (s *workerSession) Interrupt() error        { s.interruptCount.Add(1); return nil }
func (s *workerSession) Steer(sdk.Message) error { s.steerCount.Add(1); return nil }
func (s *workerSession) QueueSizes() (int, int)  { return int(s.steerCount.Load()), 0 }
func (s *workerSession) Compact(ctx context.Context, _ ...string) error {
	return s.compactFn(ctx)
}

func (s *workerSession) Close(ctx context.Context) error {
	if s.closeFn != nil {
		return s.closeFn(ctx)
	}
	return nil
}
func (s *workerSession) SetEventObserver(fn func(sdk.Event)) { s.observerReady <- fn }

type workerRuntime struct {
	mockRuntime
	sess    *workerSession
	opts    chan sdk.SessionOptions
	closeFn func(context.Context) error
	newErr  error
}

func (r *workerRuntime) NewSession(_ context.Context, opts sdk.SessionOptions) (sdk.Session, error) {
	r.opts <- opts
	return r.sess, r.newErr
}

func (r *workerRuntime) Close(ctx context.Context) error {
	if r.closeFn != nil {
		return r.closeFn(ctx)
	}
	return nil
}

func newWorkerRuntime() *workerRuntime {
	return &workerRuntime{sess: &workerSession{mockSession: newMockSession("session", "/project"), observerReady: make(chan func(sdk.Event), 1)}, opts: make(chan sdk.SessionOptions, 1)}
}

type workerHarness struct {
	link   *coord.Link
	done   chan error
	frames chan coord.Frame
	hello  coord.Frame
}

func startWorker(t *testing.T, rt *workerRuntime, reg commands.Registry) *workerHarness {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "w"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	h := &workerHarness{done: make(chan error, 1), frames: make(chan coord.Frame, 256)}
	go func() {
		h.done <- NewWorker(rt, reg, nil).Run(ctx, listener.Addr().String(), coord.Instance{ID: "instance", CWD: "/project"}, "secret")
	}()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	h.link = coord.NewLink(conn)
	h.hello, err = h.link.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.link.Send(coord.Frame{Type: coord.TypeWelcome, Version: coord.Version}); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(h.frames)
		for {
			f, err := h.link.Read()
			if err != nil {
				return
			}
			h.frames <- f
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(time.Second):
			t.Error("worker did not stop")
		}
		if err := h.link.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}

func (h *workerHarness) send(t *testing.T, id, command string) {
	t.Helper()
	if err := h.link.Send(coord.Frame{Type: coord.TypeCommand, ID: id, InstanceID: "instance", Command: json.RawMessage(command)}); err != nil {
		t.Fatal(err)
	}
}

func (h *workerHarness) next(t *testing.T) coord.Frame {
	t.Helper()
	select {
	case f, ok := <-h.frames:
		if !ok {
			t.Fatal("worker link closed")
		}
		return f
	case <-time.After(time.Second):
		t.Fatal("worker frame timed out")
		return coord.Frame{}
	}
}

func (h *workerHarness) response(t *testing.T, id string) Response {
	t.Helper()
	for {
		f := h.next(t)
		if f.Type == coord.TypeResponse && f.ID == id {
			var r Response
			if err := json.Unmarshal(f.Result, &r); err != nil {
				t.Fatal(err)
			}
			if r.ID != id || r.Type != "response" || f.InstanceID != "instance" {
				t.Fatalf("lost RPC envelope: %+v %+v", f, r)
			}
			return r
		}
	}
}

func TestWorkerDisconnectKeepsRun(t *testing.T) {
	rt := newWorkerRuntime()
	started, release, drained := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var runCtx context.Context
	rt.sess.sendFn = func(ctx context.Context, _ sdk.Message) (<-chan sdk.Event, error) {
		runCtx = ctx
		events := make(chan sdk.Event)
		go func() {
			defer close(events)
			events <- sdk.Event{Type: "agent_start"}
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			for range 300 {
				events <- sdk.Event{Type: "text_delta"}
			}
			close(drained)
		}()
		return events, nil
	}
	h := startWorker(t, rt, nil)
	if h.hello.Role != coord.RoleWorker || h.hello.Token != "secret" || h.hello.Instance.SessionID != "session" {
		t.Fatalf("hello: %+v", h.hello)
	}
	if opts := <-rt.opts; opts.UI == nil || opts.CWD != "/project" {
		t.Fatalf("session options: %+v", opts)
	}
	h.send(t, "route", `{"id":"untrusted","type":"prompt","args":{"text":"work"}}`)
	if r := h.response(t, "route"); !r.OK {
		t.Fatalf("prompt: %+v", r)
	}
	<-started
	if err := h.link.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("SDK event stream stopped draining")
	}
	if err := runCtx.Err(); err != nil {
		t.Fatalf("disconnect cancelled run: %v", err)
	}
	select {
	case err := <-h.done:
		h.done <- err
		t.Fatalf("disconnect stopped worker: %v", err)
	default:
	}
}

func TestWorkerStopWaitsForCleanup(t *testing.T) {
	rt := newWorkerRuntime()
	closing, release := make(chan struct{}), make(chan struct{})
	var runtimeClosed atomic.Bool
	rt.sess.closeFn = func(ctx context.Context) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		close(closing)
		<-release
		return errors.New("writer close failed")
	}
	rt.closeFn = func(context.Context) error { runtimeClosed.Store(true); return errors.New("runtime close failed") }
	h := startWorker(t, rt, nil)
	h.send(t, "stop", `{"type":"shutdown"}`)
	<-closing
	select {
	case f := <-h.frames:
		t.Fatalf("reply before session close: %+v", f)
	default:
	}
	close(release)
	if r := h.response(t, "stop"); r.OK || r.Error == nil || !strings.Contains(r.Error.Message, "writer close failed") || !strings.Contains(r.Error.Message, "runtime close failed") {
		t.Fatalf("cleanup errors lost: %+v", r)
	}
	if !runtimeClosed.Load() {
		t.Fatal("stop replied before runtime close")
	}
}

func TestWorkerControlsBypassCompaction(t *testing.T) {
	rt := newWorkerRuntime()
	started := make(chan struct{})
	bashStarted, bashStopped := make(chan struct{}), make(chan struct{})
	rt.sess.bash = func(ctx context.Context, _ string, _ sdk.BashOptions) (sdk.BashExecution, error) {
		close(bashStarted)
		<-ctx.Done()
		close(bashStopped)
		return sdk.BashExecution{Cancelled: true}, nil
	}
	rt.sess.compactFn = func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }
	h := startWorker(t, rt, nil)
	h.send(t, "bash", `{"type":"bash","args":{"command":"wait"}}`)
	<-bashStarted
	h.send(t, "compact", `{"type":"compact"}`)
	<-started
	h.send(t, "abort", `{"type":"interrupt"}`)
	if r := h.response(t, "abort"); !r.OK || rt.sess.interruptCount.Load() != 1 {
		t.Fatalf("interrupt: %+v", r)
	}
	h.send(t, "shell", `{"type":"abort_bash"}`)
	if r := h.response(t, "shell"); !r.OK {
		t.Fatalf("abort bash: %+v", r)
	}
	select {
	case <-bashStopped:
	case <-time.After(time.Second):
		t.Fatal("abort did not reach active bash during compaction")
	}
	h.send(t, "stop", `{"type":"shutdown"}`)
	if r := h.response(t, "stop"); !r.OK {
		t.Fatalf("stop: %+v", r)
	}
}

func TestWorkerUIAndObserverShareStream(t *testing.T) {
	rt := newWorkerRuntime()
	reg := commands.NewInMemoryRegistry()
	if err := reg.RegisterAt(commands.Info{Name: "ask"}, func(ctx context.Context, _ string, commandUI commands.CommandUI, _ commands.CommandSession) (commands.Result, error) {
		confirmed, err := commandUI.Confirm(ctx, "Approve", "Continue?")
		if !confirmed && err == nil {
			return commands.Result{}, errors.New("not confirmed")
		}
		return commands.Result{Output: "confirmed"}, err
	}, "", commands.TierBuiltin); err != nil {
		t.Fatal(err)
	}
	h := startWorker(t, rt, reg)
	observer := <-rt.sess.observerReady
	h.send(t, "ask", `{"type":"slash_command","args":{"name":"ask"}}`)
	f := h.next(t)
	var event struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Seq  int64  `json:"seq"`
	}
	if err := json.Unmarshal(f.Event, &event); err != nil {
		t.Fatal(err)
	}
	if f.Type != coord.TypeEvent || f.InstanceID != "instance" || f.SessionID != "session" || event.Type != "ui_confirm" {
		t.Fatalf("UI envelope: %+v %s", f, f.Event)
	}
	args, err := json.Marshal(map[string]any{"type": "ui_response", "args": map[string]any{"id": event.ID, "confirmed": true}})
	if err != nil {
		t.Fatal(err)
	}
	h.send(t, "reply", string(args))
	// Either response can win; both must arrive without waiting on the slash handler.
	seen := make(map[string]bool)
	for len(seen) < 2 {
		frame := h.next(t)
		if frame.Type == coord.TypeResponse {
			var r Response
			if err := json.Unmarshal(frame.Result, &r); err != nil {
				t.Fatal(err)
			}
			if !r.OK {
				t.Fatalf("UI response: %+v", r)
			}
			seen[frame.ID] = true
		}
	}
	if !seen["ask"] || !seen["reply"] {
		t.Fatalf("response routes: %v", seen)
	}
	observer(sdk.Event{Type: "queue_update"})
	f = h.next(t)
	var observed struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(f.Event, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Seq != event.Seq+1 || f.SessionID != "session" {
		t.Fatalf("observer stream diverged: %+v", f)
	}
}

func TestWorkerStartupFailureClosesRuntime(t *testing.T) {
	rt := newWorkerRuntime()
	rt.newErr = io.ErrUnexpectedEOF
	var closed bool
	rt.closeFn = func(context.Context) error { closed = true; return nil }
	if err := NewWorker(rt, nil, nil).Run(t.Context(), "unused", coord.Instance{ID: "instance"}, "secret"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("startup error: %v", err)
	}
	if !closed {
		t.Fatal("startup failure leaked runtime")
	}
}

func TestWorkerRegistrationFailureClosesSessionAndRuntime(t *testing.T) {
	rt := newWorkerRuntime()
	var sessionClosed, runtimeClosed bool
	rt.sess.closeFn = func(context.Context) error { sessionClosed = true; return nil }
	rt.closeFn = func(context.Context) error { runtimeClosed = true; return nil }
	err := NewWorker(rt, nil, nil).Run(t.Context(), filepath.Join(t.TempDir(), "missing.sock"), coord.Instance{ID: "instance"}, "secret")
	if err == nil || !strings.Contains(err.Error(), "register worker") {
		t.Fatalf("registration error: %v", err)
	}
	if !sessionClosed || !runtimeClosed {
		t.Fatalf("registration leaked resources: session=%v runtime=%v", sessionClosed, runtimeClosed)
	}
}

func TestWorkerStopReleasesBlockedUI(t *testing.T) {
	rt := newWorkerRuntime()
	reg := commands.NewInMemoryRegistry()
	settled := make(chan struct{})
	if err := reg.RegisterAt(commands.Info{Name: "wait"}, func(ctx context.Context, _ string, commandUI commands.CommandUI, _ commands.CommandSession) (commands.Result, error) {
		defer close(settled)
		_, err := commandUI.Input(ctx, "Waiting", "value")
		return commands.Result{}, err
	}, "", commands.TierBuiltin); err != nil {
		t.Fatal(err)
	}
	h := startWorker(t, rt, reg)
	h.send(t, "wait", `{"type":"slash_command","args":{"name":"wait"}}`)
	if f := h.next(t); f.Type != coord.TypeEvent {
		t.Fatalf("UI event: %+v", f)
	}
	h.send(t, "stop", `{"type":"shutdown"}`)
	if r := h.response(t, "stop"); !r.OK {
		t.Fatalf("stop: %+v", r)
	}
	select {
	case <-settled:
	default:
		t.Fatal("stop replied before blocked UI handler joined")
	}
}

func TestWorkerAdmissionBoundAndTarget(t *testing.T) {
	rt := newWorkerRuntime()
	started := make(chan struct{})
	rt.sess.compactFn = func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }
	h := startWorker(t, rt, nil)
	h.send(t, "blocking", `{"type":"compact"}`)
	<-started
	h.send(t, "waiting", `{"type":"get_state"}`)
	h.send(t, "overflow", `{"type":"get_state"}`)
	if r := h.response(t, "overflow"); r.OK || r.Error == nil || r.Error.Code != "busy" {
		t.Fatalf("unbounded admission: %+v", r)
	}
	if err := h.link.Send(coord.Frame{Type: coord.TypeCommand, ID: "wrong", InstanceID: "another", Command: json.RawMessage(`{"type":"shutdown"}`)}); err != nil {
		t.Fatal(err)
	}
	if r := h.response(t, "wrong"); r.OK || r.Error == nil || r.Error.Code != "invalid_target" {
		t.Fatalf("wrong target accepted: %+v", r)
	}
	h.send(t, "stop", `{"type":"shutdown"}`)
	if r := h.response(t, "stop"); !r.OK {
		t.Fatalf("stop: %+v", r)
	}
}

func TestWorkerBusyPromptDefaultsToSteer(t *testing.T) {
	rt := newWorkerRuntime()
	rt.sess.sendFn = func(context.Context, sdk.Message) (<-chan sdk.Event, error) { return nil, sdk.ErrSessionBusy }
	h := startWorker(t, rt, nil)
	h.send(t, "more", `{"type":"send_message","args":{"text":"adjust"}}`)
	if r := h.response(t, "more"); !r.OK || rt.sess.steerCount.Load() != 1 {
		t.Fatalf("busy ordinary send: %+v", r)
	}
}

func TestWorkerOversizedOutputKeepsLink(t *testing.T) {
	rt := newWorkerRuntime()
	large := strings.Repeat("a", coord.MaxFrameBytes)
	rt.sess.messages = []sdk.Message{{Role: "assistant", Text: large}}
	h := startWorker(t, rt, nil)
	observer := <-rt.sess.observerReady
	h.send(t, "history", `{"type":"get_messages"}`)
	if f := h.next(t); f.Type != coord.TypeResponse || f.ID != "history" || f.Error == nil || f.Error.Code != coord.CodeLimitExceeded {
		t.Fatalf("history did not return a size error: %+v", f)
	}
	observer(sdk.Event{Type: "text_delta", Payload: map[string]any{"delta": large}})
	if f := h.next(t); f.Type != coord.TypeEvent || f.SessionID != "session" || !strings.Contains(string(f.Event), `"type":"event_omitted"`) {
		t.Fatalf("oversized event did not leave a notice: %+v", f)
	}
	h.send(t, "state", `{"type":"get_state"}`)
	if r := h.response(t, "state"); !r.OK {
		t.Fatalf("worker no longer routes replies: %+v", r)
	}
}

func TestWorkerInterruptReleasesBlockedSlashUI(t *testing.T) {
	rt := newWorkerRuntime()
	reg := commands.NewInMemoryRegistry()
	if err := reg.RegisterAt(commands.Info{Name: "ask"}, func(ctx context.Context, _ string, commandUI commands.CommandUI, _ commands.CommandSession) (commands.Result, error) {
		_, err := commandUI.Input(ctx, "Waiting", "value")
		return commands.Result{}, err
	}, "", commands.TierBuiltin); err != nil {
		t.Fatal(err)
	}
	h := startWorker(t, rt, reg)
	h.send(t, "ask", `{"type":"slash_command","args":{"name":"ask"}}`)
	if f := h.next(t); f.Type != coord.TypeEvent || !strings.Contains(string(f.Event), `"type":"ui_input"`) {
		t.Fatalf("no dialog: %+v", f)
	}
	h.send(t, "interrupt", `{"type":"interrupt"}`)
	h.send(t, "state", `{"type":"get_state"}`)
	seen := make(map[string]Response)
	for len(seen) < 3 {
		f := h.next(t)
		if f.Type != coord.TypeResponse {
			continue
		}
		var r Response
		if err := json.Unmarshal(f.Result, &r); err != nil {
			t.Fatal(err)
		}
		seen[f.ID] = r
	}
	if r := seen["ask"]; r.OK || r.Error == nil || !strings.Contains(r.Error.Message, "context canceled") {
		t.Fatalf("slash command not cancelled: %+v", r)
	}
	if !seen["interrupt"].OK || !seen["state"].OK {
		t.Fatalf("worker did not recover after interrupt: %+v", seen)
	}
}

func TestWorkerInterruptReleasesDialogInPromptAdmission(t *testing.T) {
	rt := newWorkerRuntime()
	var sessionUI ui.UI
	rt.sess.sendFn = func(ctx context.Context, _ sdk.Message) (<-chan sdk.Event, error) {
		_, err := sessionUI.Input(ctx, "Waiting", "value")
		return nil, err
	}
	h := startWorker(t, rt, nil)
	sessionUI = (<-rt.opts).UI
	h.send(t, "ask", `{"type":"prompt","args":{"text":"/ask"}}`)
	if f := h.next(t); f.Type != coord.TypeEvent || !strings.Contains(string(f.Event), `"type":"ui_input"`) {
		t.Fatalf("no dialog: %+v", f)
	}
	h.send(t, "interrupt", `{"type":"interrupt"}`)
	h.send(t, "state", `{"type":"get_state"}`)
	seen := make(map[string]Response)
	for len(seen) < 3 {
		f := h.next(t)
		if f.Type != coord.TypeResponse {
			continue
		}
		var r Response
		if err := json.Unmarshal(f.Result, &r); err != nil {
			t.Fatal(err)
		}
		seen[f.ID] = r
	}
	if seen["ask"].OK || !seen["interrupt"].OK || !seen["state"].OK {
		t.Fatalf("worker did not recover after interrupt: %+v", seen)
	}
}
