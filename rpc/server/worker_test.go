package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	historyFn      func() ([]sdk.HistoryEntry, string, error)
}

func (s *workerSession) Send(ctx context.Context, msg sdk.Message) (<-chan sdk.Event, error) {
	return s.sendFn(ctx, msg)
}
func (s *workerSession) GetMessages() []sdk.Message { return s.messages }

func (s *workerSession) History(string, int) ([]sdk.HistoryEntry, string, error) {
	if s.historyFn != nil {
		return s.historyFn()
	}
	return nil, "", nil
}

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
	newFn   func(sdk.SessionOptions) (sdk.Session, error)
}

func (r *workerRuntime) NewSession(_ context.Context, opts sdk.SessionOptions) (sdk.Session, error) {
	if r.newFn != nil {
		return r.newFn(opts)
	}
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
	return startWorkerInstance(t, rt, reg, coord.Instance{ID: "instance", CWD: "/project"})
}

func startWorkerInstance(t *testing.T, rt sdk.Runtime, reg commands.Registry, instance coord.Instance) *workerHarness {
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
		h.done <- NewWorker(rt, reg, nil).Run(ctx, listener.Addr().String(), instance, "secret")
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

// nextEvent skips instance registrations, which interleave with events.
func (h *workerHarness) nextEvent(t *testing.T) coord.Frame {
	t.Helper()
	for {
		if f := h.next(t); f.Type != coord.TypeRegister {
			return f
		}
	}
}

func decodeResult(t *testing.T, r Response, v any) {
	t.Helper()
	data, err := json.Marshal(r.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
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

func TestWorkerReportsRunStatusFromSameEventStream(t *testing.T) {
	rt := newWorkerRuntime()
	h := startWorker(t, rt, nil)
	observe := <-rt.sess.observerReady
	h.send(t, "ready", `{"type":"get_state"}`)
	if r := h.response(t, "ready"); !r.OK {
		t.Fatalf("ready probe: %+v", r)
	}
	for _, pair := range [][2]string{{"agent_start", coord.StateBusy}, {"agent_end", coord.StateReady}} {
		observe(sdk.Event{Type: pair[0], SessionID: "session"})
		chat := h.next(t)
		state := h.next(t)
		if chat.Type != coord.TypeEvent || !strings.Contains(string(chat.Event), pair[0]) || state.Type != coord.TypeRegister ||
			state.Instance == nil || state.Instance.SessionID != "session" || state.Instance.State != pair[1] {
			t.Fatalf("%s frames = %+v / %+v", pair[0], chat, state)
		}
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

func TestStoppingWorkerAnswersHeartbeatWithConcurrentCommand(t *testing.T) {
	rt := newWorkerRuntime()
	closing, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	rt.sess.closeFn = func(context.Context) error {
		close(closing)
		<-release
		return nil
	}
	h := startWorker(t, rt, nil)
	t.Cleanup(unblock)
	h.send(t, "stop", `{"type":"shutdown"}`)
	<-closing
	h.send(t, "more", `{"type":"get_state"}`)
	if err := h.link.Send(coord.Frame{Type: coord.TypePing}); err != nil {
		t.Fatal(err)
	}
	rejected := false
	for {
		f := h.next(t)
		if f.Type == coord.TypePong {
			break
		}
		if f.ID == "more" && f.Error != nil && f.Error.Code == coord.CodeUnavailable {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("command during stop was not rejected")
	}
	unblock()
	if r := h.response(t, "stop"); !r.OK {
		t.Fatalf("stop reply: %+v", r)
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
	resolvedSeq := int64(-1)
	for len(seen) < 2 || resolvedSeq < 0 {
		frame := h.next(t)
		if frame.Type == coord.TypeEvent {
			var resolved struct {
				Type string `json:"type"`
				Seq  int64  `json:"seq"`
			}
			if err := json.Unmarshal(frame.Event, &resolved); err != nil {
				t.Fatal(err)
			}
			if resolved.Type == "ui_resolved" {
				resolvedSeq = resolved.Seq
			}
		}
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
	f = h.nextEvent(t)
	var observed struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(f.Event, &observed); err != nil {
		t.Fatal(err)
	}
	if resolvedSeq != event.Seq+1 || observed.Seq != resolvedSeq+1 || f.SessionID != "session" {
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
	for i := range workerAdmissionSlots {
		h.send(t, fmt.Sprintf("waiting-%d", i), `{"type":"get_state"}`)
	}
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

func TestWorkerRejectsDraftAfterQueuedSessionSwitch(t *testing.T) {
	rt := newWorkerRuntime()
	next := newWorkerRuntime().sess
	next.mockSession = newMockSession("new-session", "/other")
	var sent atomic.Int32
	countSend := func(context.Context, sdk.Message) (<-chan sdk.Event, error) {
		sent.Add(1)
		ch := make(chan sdk.Event)
		close(ch)
		return ch, nil
	}
	rt.sess.sendFn, next.sendFn = countSend, countSend
	rt.newFn = func(opts sdk.SessionOptions) (sdk.Session, error) {
		if opts.CWD == "/other" {
			return next, nil
		}
		return rt.sess, nil
	}
	started, release := make(chan struct{}), make(chan struct{})
	rt.sess.compactFn = func(ctx context.Context) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h := startWorker(t, rt, nil)
	h.send(t, "compact", `{"type":"compact"}`)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("compaction did not hold admission")
	}
	h.send(t, "switch", `{"type":"new_session","args":{"cwd":"/other"}}`)
	h.send(t, "draft", `{"type":"send_message","args":{"text":"for the old session","expected_session_id":"session"}}`)
	h.send(t, "barrier", `{"type":"get_ui_requests"}`)
	for {
		f := h.next(t)
		if f.Type != coord.TypeResponse {
			continue
		}
		if f.ID != "barrier" {
			t.Fatalf("ordinary command ran before admission released: %+v", f)
		}
		break
	}
	close(release)
	if r := h.response(t, "switch"); !r.OK {
		t.Fatalf("session switch: %+v", r)
	}
	if r := h.response(t, "draft"); r.OK || r.Error == nil || r.Error.Code != "session_changed" || sent.Load() != 0 {
		t.Fatalf("stale draft got %+v and reached Send %d times", r, sent.Load())
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

func TestWorkerReplaysEventPositions(t *testing.T) {
	rt := newWorkerRuntime()
	h := startWorker(t, rt, nil)
	observer := <-rt.sess.observerReady
	h.send(t, "ready", `{"type":"get_state"}`)
	h.response(t, "ready")
	observer(sdk.Event{Type: "agent_start"})
	first := h.nextEvent(t)
	observer(sdk.Event{Type: "text_delta", Payload: map[string]any{"delta": "during disconnect"}})
	last := h.nextEvent(t)
	if first.Position == nil || first.Position.Epoch == "" || first.Position.InstanceID != "instance" ||
		last.Position == nil || last.Position.Offset != first.Position.Offset+1 {
		t.Fatalf("non-contiguous positions: %+v, %+v", first.Position, last.Position)
	}
	after, err := json.Marshal(first.Position)
	if err != nil {
		t.Fatal(err)
	}
	h.send(t, "replay", `{"type":"replay_events","args":{"after":`+string(after)+`}}`)
	r := h.response(t, "replay")
	var page struct {
		Events []coord.Frame
		More   bool
	}
	decodeResult(t, r, &page)
	if !r.OK || page.More || len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0].Position, last.Position) || !strings.Contains(string(page.Events[0].Event), "during disconnect") {
		t.Fatalf("replay after %s: %+v", after, r)
	}
	h.send(t, "invalid", `{"type":"replay_events","args":{}}`)
	if r := h.response(t, "invalid"); r.OK || r.Error == nil || r.Error.Code != coord.CodeResyncRequired {
		t.Fatalf("missing cursor: %+v", r)
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
	var outcome string
	for len(seen) < 3 {
		f := h.next(t)
		if f.Type == coord.TypeEvent && strings.Contains(string(f.Event), `"type":"ui_resolved"`) {
			var resolved struct {
				Outcome string `json:"outcome"`
			}
			if err := json.Unmarshal(f.Event, &resolved); err != nil {
				t.Fatal(err)
			}
			outcome = resolved.Outcome
		}
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
	// The dialog resolves before the slash command replies.
	if outcome != ui.OutcomeInterrupted {
		t.Fatalf("dialog outcome = %q, want %q", outcome, ui.OutcomeInterrupted)
	}
	if !seen["interrupt"].OK || !seen["state"].OK {
		t.Fatalf("worker did not recover after interrupt: %+v", seen)
	}
}

func TestWorkerSharedDialogResolvesOnce(t *testing.T) {
	rt := newWorkerRuntime()
	reg := commands.NewInMemoryRegistry()
	if err := reg.RegisterAt(commands.Info{Name: "pick"}, func(ctx context.Context, _ string, commandUI commands.CommandUI, _ commands.CommandSession) (commands.Result, error) {
		idx, err := commandUI.Select(ctx, "Pick", []string{"red", "blue"})
		if err != nil {
			return commands.Result{}, err
		}
		return commands.Result{Output: []string{"red", "blue"}[idx]}, nil
	}, "", commands.TierBuiltin); err != nil {
		t.Fatal(err)
	}
	h := startWorker(t, rt, reg)
	h.send(t, "pick", `{"type":"slash_command","args":{"name":"pick"}}`)
	f := h.next(t)
	var asked struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(f.Event, &asked); err != nil {
		t.Fatal(err)
	}
	if f.Type != coord.TypeEvent || asked.Type != "ui_select" || asked.ID == "" {
		t.Fatalf("dialog event: %+v", f)
	}
	if f = h.next(t); f.Type != coord.TypeRegister || f.Instance == nil || f.Instance.ID != "instance" || f.Instance.PendingInputs != 1 {
		t.Fatalf("pending registration: %+v", f)
	}
	reply := func(id, value string) {
		t.Helper()
		data, err := json.Marshal(map[string]any{"type": "ui_response", "args": map[string]any{"id": asked.ID, "value": value}})
		if err != nil {
			t.Fatal(err)
		}
		h.send(t, id, string(data))
	}
	listed := func(id string) []ui.SharedRequest {
		t.Helper()
		// The slash command holds the dispatcher, so this must bypass it.
		h.send(t, id, `{"type":"get_ui_requests"}`)
		r := h.response(t, id)
		if !r.OK {
			t.Fatalf("get_ui_requests: %+v", r)
		}
		var result struct {
			Requests []ui.SharedRequest `json:"requests"`
		}
		decodeResult(t, r, &result)
		return result.Requests
	}
	want := []ui.SharedRequest{{ID: asked.ID, Method: ui.UIMethodSelect, Title: "Pick", Options: []string{"red", "blue"}}}
	if got := listed("list"); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %+v, want %+v", got, want)
	}
	reply("invalid", "green")
	if r := h.response(t, "invalid"); r.OK || r.Error == nil || r.Error.Code != "invalid_response" {
		t.Fatalf("invalid reply accepted: %+v", r)
	}
	if got := listed("relist"); !reflect.DeepEqual(got, want) {
		t.Fatalf("invalid reply consumed request: %+v", got)
	}

	reply("first", "blue")
	reply("second", "red")
	responses := make(map[string]Response)
	var resolved coord.Frame
	pending := -1
	for len(responses) < 3 || resolved.Event == nil || pending != 0 {
		f := h.next(t)
		switch f.Type {
		case coord.TypeResponse:
			var r Response
			if err := json.Unmarshal(f.Result, &r); err != nil {
				t.Fatal(err)
			}
			responses[f.ID] = r
		case coord.TypeRegister:
			pending = f.Instance.PendingInputs
		case coord.TypeEvent:
			if strings.Contains(string(f.Event), `"type":"ui_resolved"`) {
				resolved = f
			}
		}
	}
	var first, second struct {
		Resolved bool `json:"resolved"`
	}
	decodeResult(t, responses["first"], &first)
	decodeResult(t, responses["second"], &second)
	if !responses["first"].OK || !first.Resolved || !responses["second"].OK || second.Resolved {
		t.Fatalf("resolution was not first-wins: %+v", responses)
	}
	var output struct {
		Output string `json:"output"`
	}
	decodeResult(t, responses["pick"], &output)
	if output.Output != "blue" {
		t.Fatalf("slash command saw %+v", responses["pick"])
	}
	var notice map[string]any
	if err := json.Unmarshal(resolved.Event, &notice); err != nil {
		t.Fatal(err)
	}
	if notice["id"] != asked.ID || notice["method"] != "select" || notice["outcome"] != ui.OutcomeAnswered || resolved.SessionID != "session" {
		t.Fatalf("resolution notice: %v", notice)
	}
	if strings.Contains(string(resolved.Event), "blue") || notice["value"] != nil {
		t.Fatalf("resolution notice leaked the answer: %s", resolved.Event)
	}
	if got := listed("after"); len(got) != 0 {
		t.Fatalf("resolved request still listed: %+v", got)
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

func TestWorkerSnapshotDuringPublication(t *testing.T) {
	rt := newWorkerRuntime()
	reading, resume := make(chan struct{}), make(chan struct{})
	rt.sess.historyFn = func() ([]sdk.HistoryEntry, string, error) {
		close(reading)
		select {
		case <-resume:
			return []sdk.HistoryEntry{{EntryID: "saved", Message: sdk.Message{Role: "user", Text: "prompt"}}}, "older", nil
		case <-t.Context().Done():
			return nil, "", t.Context().Err()
		}
	}
	h := startWorker(t, rt, nil)
	observer := <-rt.sess.observerReady
	h.send(t, "ready", `{"type":"get_state"}`)
	h.response(t, "ready")
	observer(sdk.Event{Type: "text_delta", Payload: map[string]any{"message_id": "partial", "delta": "early"}})
	early := h.nextEvent(t)
	h.send(t, "snapshot", `{"type":"get_snapshot","args":{}}`)
	select {
	case <-reading:
	case <-time.After(time.Second):
		t.Fatal("snapshot did not reach history read")
	}
	observer(sdk.Event{Type: "text_delta", Payload: map[string]any{"message_id": "partial", "delta": " late"}})
	late := h.nextEvent(t)
	h.send(t, "controls", `{"type":"get_ui_requests"}`)
	h.response(t, "controls")
	close(resume)
	r := h.response(t, "snapshot")
	var snapshot struct {
		Position coord.Position
		History  historyPage
		Live     liveSnapshot
	}
	decodeResult(t, r, &snapshot)
	if !r.OK || early.Position == nil || late.Position == nil || snapshot.Position != *early.Position || late.Position.Offset != snapshot.Position.Offset+1 ||
		len(snapshot.Live.Messages) != 1 || snapshot.Live.Messages[0].Text != "early" || len(snapshot.History.Entries) != 1 || snapshot.History.Before != "older" {
		t.Fatalf("snapshot boundary incorrect: %+v, early=%+v late=%+v", snapshot, early.Position, late.Position)
	}
}

func TestWorkerQueuesConcurrentSnapshots(t *testing.T) {
	rt := newWorkerRuntime()
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	rt.sess.historyFn = func() ([]sdk.HistoryEntry, string, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-t.Context().Done():
				return nil, "", t.Context().Err()
			}
		}
		return []sdk.HistoryEntry{{EntryID: "entry"}}, "", nil
	}
	h := startWorker(t, rt, nil)
	h.send(t, "first", `{"type":"get_snapshot"}`)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first snapshot did not enter history")
	}
	h.send(t, "second", `{"type":"get_snapshot"}`)
	h.send(t, "ui", `{"type":"get_ui_requests"}`)
	h.response(t, "ui")
	close(release)
	for _, id := range []string{"first", "second"} {
		if r := h.response(t, id); !r.OK {
			t.Errorf("snapshot %s: %+v", id, r.Error)
		}
	}
}

func TestWorkerRedialsWithPendingInputAndSameEpoch(t *testing.T) {
	rt := newWorkerRuntime()
	path := filepath.Join(t.TempDir(), "w")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- NewWorker(rt, nil, nil).Run(ctx, path, coord.Instance{ID: "instance", CWD: "/project"}, "secret")
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("worker still running after cancellation")
		}
	})
	accept := func() (*coord.Link, coord.Frame) {
		t.Helper()
		if err := listener.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		conn, err := listener.AcceptUnix()
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		link := coord.NewLink(conn)
		t.Cleanup(func() {
			if err := link.Close(); err != nil {
				t.Error(err)
			}
		})
		hello, err := link.Read()
		if err != nil {
			t.Fatal(err)
		}
		if err := link.Send(coord.Frame{Type: coord.TypeWelcome, Version: coord.Version}); err != nil {
			t.Fatal(err)
		}
		return link, hello
	}
	read := func(link *coord.Link, kind string) coord.Frame {
		t.Helper()
		for {
			frame, err := link.Read()
			if err != nil {
				t.Fatal(err)
			}
			if frame.Type == kind {
				return frame
			}
		}
	}
	first, original := accept()
	if err := first.Send(coord.Frame{Type: coord.TypeCommand, ID: "ready", InstanceID: "instance", Command: json.RawMessage(`{"type":"get_state"}`)}); err != nil {
		t.Fatal(err)
	}
	read(first, coord.TypeResponse)
	sessionUI := (<-rt.opts).UI
	resolved := make(chan error, 1)
	go func() {
		_, err := sessionUI.Input(ctx, "Waiting", "value")
		resolved <- err
	}()
	t.Cleanup(func() { cancel(); <-resolved })
	event := read(first, coord.TypeEvent)
	if event.Position == nil || !strings.Contains(string(event.Event), `"type":"ui_input"`) {
		t.Fatalf("missing input event: %+v", event)
	}
	read(first, coord.TypeRegister)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, rejoined := accept()
	if rejoined.Instance.ID != original.Instance.ID || rejoined.Instance.SessionID != original.Instance.SessionID || rejoined.Instance.PendingInputs != 1 {
		t.Fatalf("lost session or input: %+v", rejoined.Instance)
	}
	if err := second.Send(coord.Frame{Type: coord.TypeCommand, ID: "snapshot", InstanceID: "instance", Command: json.RawMessage(`{"type":"get_snapshot"}`)}); err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Result snapshotResult
	}
	frame := read(second, coord.TypeResponse)
	if err := json.Unmarshal(frame.Result, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Result.Position != *event.Position || len(snap.Result.UIRequests) != 1 {
		t.Fatalf("snapshot after redial lost cursor or dialog: %+v", snap.Result)
	}
}
