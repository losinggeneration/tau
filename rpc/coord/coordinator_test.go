//go:build unix

package coord

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	sessionpkg "github.com/losinggeneration/tau/agent/session"
	"golang.org/x/sys/unix"
)

type fakeWorker struct {
	req     LaunchRequest
	link    *Link
	welcome Frame
	frames  chan Frame
}

type harness struct {
	t       *testing.T
	path    string
	workers chan *fakeWorker
	cwd     string
}

func startCoordinator(t *testing.T, launch func(*harness) LaunchFunc, timeout time.Duration, opts ...RecoverableOption) *harness {
	t.Helper()
	h := &harness{t: t, path: filepath.Join(socketDir(t), "c.sock"), workers: make(chan *fakeWorker, 8), cwd: t.TempDir()}
	if launch == nil {
		launch = (*harness).spawnWorker
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runWithReceipts(ctx, h.path, launch(h), timeout, "", opts...) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(testTimeout):
			t.Errorf("coordinator did not stop")
		}
	})
	return h
}

// spawnWorker models a detached worker: it dials back with the launch token.
func (h *harness) spawnWorker() LaunchFunc {
	return h.spawnWorkerWithPID(42)
}

func (h *harness) spawnWorkerWithPID(pid int) LaunchFunc {
	return func(_ context.Context, req LaunchRequest) error {
		go func() {
			hello := Frame{Role: RoleWorker, Token: req.Token, Instance: &Instance{
				ID: req.InstanceID, NodeID: "node", CWD: req.CWD, SessionID: cmp.Or(req.SessionID, "session-"+req.InstanceID), PID: pid,
				LaunchRequestID: req.LaunchRequestID,
			}}
			link, welcome, err := Dial(context.Background(), h.path, hello)
			if err != nil {
				h.t.Errorf("worker dial: %v", err)
				return
			}
			w := &fakeWorker{req: req, link: link, welcome: welcome, frames: make(chan Frame, 2*MaxPending)}
			h.workers <- w
			for {
				f, err := link.Read()
				if err != nil {
					close(w.frames)
					return
				}
				w.frames <- f
			}
		}()
		return nil
	}
}

func (h *harness) dial(role string) (*Link, Frame) {
	h.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		link, welcome, err := Dial(ctx, h.path, Frame{Role: role})
		cancel()
		if err == nil {
			h.t.Cleanup(func() { _ = link.Close() })
			return link, welcome
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("dial: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) client() (*Link, Frame) {
	h.t.Helper()
	return h.dial(RoleClient)
}

func (h *harness) nextWorker() *fakeWorker {
	h.t.Helper()
	select {
	case w := <-h.workers:
		return w
	case <-time.After(testTimeout):
		h.t.Fatal("worker did not register")
		return nil
	}
}

func readFrame(t *testing.T, l *Link) Frame {
	t.Helper()
	type result struct {
		f   Frame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := l.Read()
		ch <- result{f, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read: %v", r.err)
		}
		return r.f
	case <-time.After(testTimeout):
		t.Fatal("read timed out")
		return Frame{}
	}
}

func (w *fakeWorker) next(t *testing.T) Frame {
	t.Helper()
	select {
	case f, ok := <-w.frames:
		if !ok {
			t.Fatal("worker link closed")
		}
		return f
	case <-time.After(testTimeout):
		t.Fatal("worker read timed out")
		return Frame{}
	}
}

func mustSend(t *testing.T, l *Link, f Frame) {
	t.Helper()
	if err := l.Send(f); err != nil {
		t.Fatalf("send %s: %v", f.Type, err)
	}
}

func request(t *testing.T, l *Link, f Frame) Frame {
	t.Helper()
	mustSend(t, l, f)
	return readFrame(t, l)
}

func wantError(t *testing.T, f Frame, id, code string) {
	t.Helper()
	if f.Type != TypeResponse || f.ID != id || f.Error == nil || f.Error.Code != code {
		t.Fatalf("frame = %+v (error %v), want response %q with %s", f, f.Error, id, code)
	}
}

func (h *harness) start(c *Link, id string) (Instance, *fakeWorker) {
	h.t.Helper()
	mustSend(h.t, c, Frame{Type: TypeStartInstance, ID: id, CWD: h.cwd})
	w := h.nextWorker()
	resp := readFrame(h.t, c)
	if resp.Type != TypeResponse || resp.ID != id || resp.Error != nil || resp.Instance == nil {
		h.t.Fatalf("start response = %+v (error %v)", resp, resp.Error)
	}
	if resp.Instance.ID != w.req.InstanceID || resp.Instance.CWD != h.cwd || resp.Instance.State != StateAwaitingUser {
		h.t.Fatalf("started instance = %+v, launch %+v", resp.Instance, w.req)
	}
	if w.welcome.Generation == "" || w.welcome.InstanceID != w.req.InstanceID {
		h.t.Fatalf("worker welcome = %+v", w.welcome)
	}
	return *resp.Instance, w
}

func TestHandshakeRejections(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	_, welcome := h.client()
	if welcome.ClientID == "" || welcome.Generation == "" || welcome.Role != RoleClient {
		t.Fatalf("client welcome = %+v", welcome)
	}

	tests := map[string]struct {
		hello Frame
		code  string
	}{
		"version":     {Frame{Version: Version + 1, Role: RoleClient}, CodeUnsupportedVersion},
		"role":        {Frame{Role: "admin"}, CodeInvalidRole},
		"no role":     {Frame{}, CodeInvalidRole},
		"no token":    {Frame{Role: RoleWorker, Instance: &Instance{ID: "x"}}, CodeUnauthorized},
		"bad token":   {Frame{Role: RoleWorker, Token: "guess", Instance: &Instance{ID: "x"}}, CodeUnauthorized},
		"no instance": {Frame{Role: RoleWorker, Token: "guess"}, CodeUnauthorized},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := Dial(context.Background(), h.path, tc.hello)
			var protoErr *Error
			if !errors.As(err, &protoErr) || protoErr.Code != tc.code {
				t.Fatalf("Dial error = %v, want %s", err, tc.code)
			}
		})
	}

	t.Run("first frame not hello", func(t *testing.T) {
		conn, err := net.Dial("unix", h.path)
		if err != nil {
			t.Fatal(err)
		}
		raw := NewLink(conn)
		defer raw.Close()
		wantError(t, request(t, raw, Frame{Type: TypeListInstances, ID: "1"}), "", CodeInvalidFrame)
	})
}

func TestWorkerTokenIsSingleUseAndBoundToInstance(t *testing.T) {
	var (
		mu   sync.Mutex
		reqs []LaunchRequest
	)
	record := func(*harness) LaunchFunc {
		return func(_ context.Context, req LaunchRequest) error {
			mu.Lock()
			reqs = append(reqs, req)
			mu.Unlock()
			return nil
		}
	}
	h := startCoordinator(t, record, 200*time.Millisecond)
	c, _ := h.client()
	mustSend(t, c, Frame{Type: TypeStartInstance, ID: "s1", CWD: h.cwd})
	var req LaunchRequest
	deadline := time.Now().Add(testTimeout)
	for {
		mu.Lock()
		if len(reqs) == 1 {
			req = reqs[0]
		}
		mu.Unlock()
		if req.Token != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if req.Token == "" || req.InstanceID == "" || req.CWD != h.cwd {
		t.Fatalf("launch request = %+v", req)
	}

	_, _, err := Dial(context.Background(), h.path, Frame{Role: RoleWorker, Token: req.Token, Instance: &Instance{ID: "other"}})
	var protoErr *Error
	if !errors.As(err, &protoErr) || protoErr.Code != CodeUnauthorized {
		t.Fatalf("mismatched instance error = %v", err)
	}

	resp := readFrame(t, c)
	wantError(t, resp, "s1", CodeLaunchFailed)
	if strings.Contains(resp.Error.Message, req.Token) {
		t.Fatalf("launch error leaks token: %q", resp.Error.Message)
	}

	_, _, err = Dial(context.Background(), h.path, Frame{Role: RoleWorker, Token: req.Token, Instance: &Instance{ID: req.InstanceID}})
	if !errors.As(err, &protoErr) || protoErr.Code != CodeUnauthorized {
		t.Fatalf("late registration error = %v, want unauthorized", err)
	}
}

func TestLaunchErrorClearsPendingAndRedactsToken(t *testing.T) {
	failing := func(*harness) LaunchFunc {
		return func(_ context.Context, req LaunchRequest) error {
			return fmt.Errorf("spawn failed with --token=%s", req.Token)
		}
	}
	h := startCoordinator(t, failing, launchTimeout)
	c, _ := h.client()
	for range 2 {
		resp := request(t, c, Frame{Type: TypeStartInstance, ID: "same", CWD: h.cwd})
		wantError(t, resp, "same", CodeLaunchFailed)
		if !strings.Contains(resp.Error.Message, "[redacted]") || !strings.Contains(resp.Error.Message, "spawn failed") {
			t.Fatalf("launch error message = %q", resp.Error.Message)
		}
	}
	list := request(t, c, Frame{Type: TypeListInstances, ID: "l"})
	if len(list.Instances) != 0 {
		t.Fatalf("failed launch registered instances: %+v", list.Instances)
	}
}

func TestStartRejectsRelativeCWD(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	wantError(t, request(t, c, Frame{Type: TypeStartInstance, ID: "1", CWD: "project"}), "1", CodeInvalidFrame)
	wantError(t, request(t, c, Frame{Type: TypeStartInstance, ID: "2"}), "2", CodeInvalidFrame)
}

func TestCommandTargetValidation(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	inst, _ := h.start(c, "s")
	cmd := json.RawMessage(`{"type":"get_state"}`)

	wantError(t, request(t, c, Frame{Type: TypeCommand, ID: "1", Command: cmd}), "1", CodeInvalidFrame)
	wantError(t, request(t, c, Frame{Type: TypeCommand, ID: "2", InstanceID: "missing", Command: cmd}), "2", CodeNotFound)
	wantError(t, request(t, c, Frame{Type: TypeCommand, ID: "3", InstanceID: inst.ID}), "3", CodeInvalidFrame)
	wantError(t, request(t, c, Frame{Type: TypeCommand, InstanceID: inst.ID, Command: cmd}), "", CodeInvalidFrame)
	wantError(t, request(t, c, Frame{Type: TypeRegister, ID: "4"}), "4", CodeInvalidFrame)
	wantError(t, request(t, c, Frame{Type: TypeAttach, ID: "5", InstanceID: "missing"}), "5", CodeNotFound)
}

func TestRouteIDsAreIsolatedPerClient(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	a, aWelcome := h.client()
	b, bWelcome := h.client()
	inst, w := h.start(a, "start")
	cmd := json.RawMessage(`{"type":"get_state","id":"1"}`)

	mustSend(t, a, Frame{Type: TypeCommand, ID: "1", InstanceID: inst.ID, Command: cmd})
	mustSend(t, b, Frame{Type: TypeCommand, ID: "1", InstanceID: inst.ID, Command: cmd})
	routes := map[string]string{}
	for range 2 {
		f := w.next(t)
		if f.Type != TypeCommand || f.InstanceID != inst.ID || f.SessionID != inst.SessionID {
			t.Fatalf("worker frame = %+v", f)
		}
		if f.ID == "1" || f.ID == "" {
			t.Fatalf("client request ID forwarded unchanged: %q", f.ID)
		}
		if string(f.Command) != string(cmd) {
			t.Fatalf("command payload = %s, want %s", f.Command, cmd)
		}
		routes[f.ClientID] = f.ID
	}
	aRoute, bRoute := routes[aWelcome.ClientID], routes[bWelcome.ClientID]
	if aRoute == "" || bRoute == "" || aRoute == bRoute {
		t.Fatalf("routes = %v, clients %q %q", routes, aWelcome.ClientID, bWelcome.ClientID)
	}

	mustSend(t, w.link, Frame{Type: TypeResponse, ID: "1", Result: json.RawMessage(`{"spoof":true}`)})
	mustSend(t, w.link, Frame{Type: TypeResponse, ID: bRoute, Result: json.RawMessage(`{"who":"b"}`)})
	mustSend(t, w.link, Frame{Type: TypeResponse, ID: aRoute, Result: json.RawMessage(`{"who":"a"}`)})
	mustSend(t, w.link, Frame{Type: TypeResponse, ID: aRoute, Result: json.RawMessage(`{"who":"replay"}`)})

	for name, tc := range map[string]struct {
		link *Link
		want string
	}{"a": {a, `{"who":"a"}`}, "b": {b, `{"who":"b"}`}} {
		resp := readFrame(t, tc.link)
		if resp.Type != TypeResponse || resp.ID != "1" || resp.InstanceID != inst.ID || string(resp.Result) != tc.want {
			t.Fatalf("client %s response = %+v result %s", name, resp, resp.Result)
		}
	}

	list := request(t, a, Frame{Type: TypeListInstances, ID: "after"})
	if list.ID != "after" || len(list.Instances) != 1 {
		t.Fatalf("replayed route reached client: %+v", list)
	}
}

func TestDuplicateAndPendingLimits(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	inst, w := h.start(c, "start")
	cmd := json.RawMessage(`{"type":"prompt"}`)

	mustSend(t, c, Frame{Type: TypeCommand, ID: "dup", InstanceID: inst.ID, Command: cmd})
	first := w.next(t)
	wantError(t, request(t, c, Frame{Type: TypeCommand, ID: "dup", InstanceID: inst.ID, Command: cmd}), "dup", CodeDuplicateID)

	for i := 1; i < MaxPending; i++ {
		mustSend(t, c, Frame{Type: TypeCommand, ID: fmt.Sprint(i), InstanceID: inst.ID, Command: cmd})
		w.next(t)
	}
	wantError(t, request(t, c, Frame{Type: TypeCommand, ID: "over", InstanceID: inst.ID, Command: cmd}), "over", CodeTooManyPending)

	mustSend(t, w.link, Frame{Type: TypeResponse, ID: first.ID, Result: json.RawMessage(`{}`)})
	if resp := readFrame(t, c); resp.ID != "dup" || resp.Error != nil {
		t.Fatalf("response = %+v", resp)
	}
	mustSend(t, c, Frame{Type: TypeCommand, ID: "dup", InstanceID: inst.ID, Command: cmd})
	if f := w.next(t); f.ID == first.ID {
		t.Fatalf("route ID reused: %q", f.ID)
	}
}

func TestEventsReachOnlySubscribersWithWorkerEnvelope(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	a, _ := h.client()
	b, _ := h.client()
	inst, w := h.start(a, "start")

	attach := request(t, a, Frame{Type: TypeAttach, ID: "att", InstanceID: inst.ID})
	if attach.Error != nil || attach.Instance == nil || attach.SessionID != inst.SessionID {
		t.Fatalf("attach = %+v", attach)
	}

	mustSend(t, w.link, Frame{Type: TypeEvent, InstanceID: "spoof", ClientID: "spoof", Event: json.RawMessage(`{"type":"message"}`)})
	ev := readFrame(t, a)
	if ev.Type != TypeEvent || ev.InstanceID != inst.ID || ev.SessionID != inst.SessionID || ev.ClientID != "" || string(ev.Event) != `{"type":"message"}` {
		t.Fatalf("event = %+v", ev)
	}
	if resp := request(t, b, Frame{Type: TypeListInstances, ID: "b"}); resp.Type != TypeResponse || resp.ID != "b" {
		t.Fatalf("unsubscribed client got %+v", resp)
	}

	mustSend(t, w.link, Frame{Type: TypeRegister, ID: "r", Instance: &Instance{ID: inst.ID, SessionID: "session-2", State: "busy"}})
	if reg := w.next(t); reg.ID != "r" || reg.Error != nil || reg.Instance == nil || reg.Instance.SessionID != "session-2" {
		t.Fatalf("register ack = %+v", reg)
	}
	mustSend(t, w.link, Frame{Type: TypeRegister, ID: "bad", Instance: &Instance{ID: "other"}})
	if reg := w.next(t); reg.ID != "bad" || reg.Error == nil || reg.Error.Code != CodeConflict {
		t.Fatalf("conflicting register = %+v", reg)
	}
	update := readFrame(t, a)
	if update.Type != TypeEvent || update.SessionID != "session-2" || update.Instance == nil || update.Instance.State != "busy" {
		t.Fatalf("register update = %+v", update)
	}
	mustSend(t, w.link, Frame{Type: TypeEvent, Event: json.RawMessage(`{"type":"next"}`)})
	if ev := readFrame(t, a); ev.SessionID != "session-2" {
		t.Fatalf("event after register = %+v", ev)
	}

	if resp := request(t, a, Frame{Type: TypeDetach, ID: "det", InstanceID: inst.ID}); resp.Error != nil {
		t.Fatalf("detach = %+v", resp)
	}
	mustSend(t, w.link, Frame{Type: TypeEvent, Event: json.RawMessage(`{"type":"after-detach"}`)})
	if resp := request(t, a, Frame{Type: TypeListInstances, ID: "a"}); resp.Type != TypeResponse || resp.ID != "a" {
		t.Fatalf("detached client got %+v", resp)
	}
}

func TestWatchInstancesOnlyDeliversSummaries(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	actor, _ := h.client()
	watcher, _ := h.client()
	first, _ := h.start(actor, "first")
	initial := request(t, watcher, Frame{Type: TypeWatchInstances, ID: "watch"})
	if initial.Error != nil || len(initial.Instances) != 1 || initial.Instances[0].ID != first.ID {
		t.Fatalf("initial watch snapshot = %+v", initial)
	}
	second, w := h.start(actor, "second")
	if update := readFrame(t, watcher); update.Type != TypeInstanceUpdate || update.InstanceID != second.ID || update.SessionID != second.SessionID {
		t.Fatalf("new instance summary = %+v", update)
	}
	mustSend(t, w.link, Frame{Type: TypeEvent, Event: json.RawMessage(`{"type":"private_chat_update"}`)})
	attention := second
	attention.State = StateWorking
	attention.PendingInputs = 2
	mustSend(t, w.link, Frame{Type: TypeRegister, Instance: &attention})
	if update := readFrame(t, watcher); update.Type != TypeInstanceUpdate || update.Instance == nil || update.Instance.State != attention.State || update.Instance.PendingInputs != 2 || update.Instance.LastActivity.IsZero() || update.Generation != initial.Generation || len(update.Event) != 0 {
		t.Fatalf("attention summary = %+v", update)
	}
	if err := w.link.Close(); err != nil {
		t.Fatal(err)
	}
	if update := readFrame(t, watcher); update.Type != TypeInstanceUpdate || update.InstanceID != second.ID || update.SessionID != second.SessionID || update.Error == nil || update.Error.Code != CodeWorkerLinkLost {
		t.Fatalf("lost instance summary = %+v", update)
	}
	if reply := request(t, watcher, Frame{Type: TypeUnwatchInstances, ID: "unwatch"}); reply.Type != TypeResponse || reply.Error != nil {
		t.Fatalf("unwatch = %+v", reply)
	}
	third, _ := h.start(actor, "third")
	if reply := request(t, watcher, Frame{Type: TypeListInstances, ID: "list"}); reply.ID != "list" || !slices.ContainsFunc(reply.Instances, func(i Instance) bool { return i.ID == third.ID }) {
		t.Fatalf("after unwatch = %+v", reply)
	}
}

func TestInstanceWatchCachesFirstPrompt(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	legacy, _ := h.client()
	watcher, _, err := Dial(t.Context(), h.path, Frame{Role: RoleClient, Operations: []string{OperationInstancePrompts}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := watcher.Close(); err != nil {
			t.Error(err)
		}
	})
	inst, w := h.start(legacy, "start")
	request(t, legacy, Frame{Type: TypeWatchInstances, ID: "legacy"})
	request(t, watcher, Frame{Type: TypeWatchInstances, ID: "watch"})
	inst.FirstPrompt = "Fix the picker"
	mustSend(t, w.link, Frame{Type: TypeRegister, Instance: &inst})
	for _, tc := range []struct {
		link *Link
		want string
	}{{watcher, "Fix the picker"}, {legacy, ""}} {
		f := readFrame(t, tc.link)
		if f.Type != TypeInstanceUpdate || f.Instance == nil || f.Instance.FirstPrompt != tc.want {
			t.Fatalf("watch update = %+v, want prompt %q", f, tc.want)
		}
		f = request(t, tc.link, Frame{Type: TypeListInstances, ID: "list"})
		if len(f.Instances) != 1 || f.Instances[0].FirstPrompt != tc.want {
			t.Fatalf("cached list = %+v, want prompt %q", f, tc.want)
		}
	}
	inst.FirstPrompt = ""
	inst.State = StateWorking
	mustSend(t, w.link, Frame{Type: TypeRegister, Instance: &inst})
	if f := readFrame(t, watcher); f.Instance == nil || f.Instance.FirstPrompt != "Fix the picker" || f.Instance.State != StateWorking {
		t.Fatalf("status update lost cached preview: %+v", f.Instance)
	}
	inst.SessionID = "next-session"
	mustSend(t, w.link, Frame{Type: TypeRegister, Instance: &inst})
	if f := readFrame(t, watcher); f.Instance == nil || f.Instance.SessionID != "next-session" || f.Instance.FirstPrompt != "" {
		t.Fatalf("session switch retained old preview: %+v", f.Instance)
	}
	if err := w.link.Close(); err != nil {
		t.Fatal(err)
	}
	if f := readFrame(t, watcher); f.Instance != nil || f.InstanceID != inst.ID {
		t.Fatalf("removed worker = %+v", f)
	}
	if f := request(t, watcher, Frame{Type: TypeListInstances, ID: "removed"}); len(f.Instances) != 0 {
		t.Fatalf("removed worker still cached: %+v", f.Instances)
	}
}

func TestOutdatedIndicatorTracksLaunchBuild(t *testing.T) {
	var build atomic.Value
	build.Store("build-a")
	h := startCoordinator(t, nil, launchTimeout, WithWorkerBuild(func() string { return build.Load().(string) }))
	legacy, _ := h.client()
	watcher, _, err := Dial(t.Context(), h.path, Frame{Role: RoleClient, Operations: []string{OperationInstanceBuild}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := watcher.Close(); err != nil {
			t.Error(err)
		}
	})
	inst, w := h.start(legacy, "start")
	request(t, watcher, Frame{Type: TypeWatchInstances, ID: "watch"})
	for _, buildID := range []string{"", "build-a"} {
		inst.BuildID = buildID
		mustSend(t, w.link, Frame{Type: TypeRegister, Instance: &inst})
		if f := readFrame(t, watcher); f.Instance == nil || f.Instance.Outdated || f.Instance.BuildID != "" {
			t.Fatalf("unexpected update for %q: %+v", buildID, f.Instance)
		}
	}
	build.Store("build-b")
	mustSend(t, watcher, Frame{Type: TypeListInstances, ID: "list"}) // refresh without a timer
	update, list := readFrame(t, watcher), readFrame(t, watcher)
	if update.Type != TypeInstanceUpdate || update.Instance == nil || !update.Instance.Outdated || update.Instance.BuildID != "" {
		t.Fatalf("outdated update = %+v, instance = %+v", update, update.Instance)
	}
	if len(list.Instances) != 1 || !list.Instances[0].Outdated || list.Instances[0].BuildID != "" {
		t.Fatalf("outdated list = %+v", list.Instances)
	}
	if list := request(t, legacy, Frame{Type: TypeListInstances, ID: "legacy"}); len(list.Instances) != 1 || list.Instances[0].Outdated || list.Instances[0].BuildID != "" {
		t.Fatalf("legacy list = %+v", list.Instances)
	}
}

func TestOutdatedIndicatorForLegacyLocalWorker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux Unix peer credentials and procfs")
	}
	if _, err := os.Stat("/proc/self/exe"); err != nil {
		t.Skipf("cannot inspect process executable: %v", err)
	}
	installed := filepath.Join(t.TempDir(), "tau")
	if err := os.Symlink("/proc/self/exe", installed); err != nil {
		t.Fatal(err)
	}
	// The test process is the Unix peer, simulating a worker built before
	// build reporting existed. Its executable will not change on update.
	launchBuild := func() string { return ExecutableBuildID(installed) }
	h := startCoordinator(t, func(h *harness) LaunchFunc { return h.spawnWorkerWithPID(os.Getpid()) }, launchTimeout, WithWorkerBuild(launchBuild))
	h.client() // wait for the coordinator to accept connections
	c, _, err := Dial(t.Context(), h.path, Frame{Role: RoleClient, Operations: []string{OperationInstanceBuild}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	h.start(c, "start")
	if f := request(t, c, Frame{Type: TypeWatchInstances, ID: "before"}); len(f.Instances) != 1 || f.Instances[0].Outdated {
		t.Fatalf("current legacy worker marked outdated: %+v", f.Instances)
	}
	newExe := installed + ".next"
	if err := os.WriteFile(newExe, []byte("updated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newExe, installed); err != nil {
		t.Fatal(err)
	}
	mustSend(t, c, Frame{Type: TypeListInstances, ID: "after"})
	f := readFrame(t, c)
	if f.Type != TypeInstanceUpdate || f.Instance == nil || !f.Instance.Outdated {
		t.Fatalf("legacy worker must be marked outdated after update, got %s: %+v %+v", f.Type, f.Instance, f.Instances)
	}
	if f := readFrame(t, c); f.ID != "after" || len(f.Instances) != 1 || !f.Instances[0].Outdated {
		t.Fatalf("legacy worker not marked outdated in list: %+v", f.Instances)
	}
}

func TestLegacyRestartRequiresSavedSession(t *testing.T) {
	dir := t.TempDir()
	h := startCoordinator(t, nil, launchTimeout, WithSessionDirectory(func(context.Context, string) (string, error) { return dir, nil }))
	c, _ := h.client()
	inst, w := h.start(c, "start")
	storage, err := sessionpkg.NewStorageAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	header := sessionpkg.NewHeader(inst.CWD, nil, nil)
	inst.SessionID = header.ID
	mustSend(t, w.link, Frame{Type: TypeRegister, ID: "bind", Instance: &inst})
	if f := w.next(t); f.ID != "bind" || f.Error != nil {
		t.Fatalf("register saved session: %+v", f)
	}
	stop := Frame{Type: TypeStopInstance, ID: "restart", InstanceID: inst.ID, SessionID: header.ID}
	if f := request(t, c, stop); f.Error == nil || !strings.Contains(f.Error.Message, "no resumable save") {
		t.Errorf("unsaved legacy session should be left running: %+v (%v)", f, f.Error)
	}
	invalid := header
	invalid.Type = sessionpkg.EntryTypeMessage
	invalidSave, err := storage.Create(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := invalidSave.Close(); err != nil {
		t.Fatal(err)
	}
	if f := request(t, c, stop); f.Error == nil || f.Error.Code != CodeUnavailable {
		t.Fatalf("invalid saved header should leave the worker running: %+v (%v)", f, f.Error)
	}
	if err := os.Remove(invalidSave.Path()); err != nil {
		t.Fatal(err)
	}
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	})
	// A lost caller does not cancel a switch already queued at an old worker.
	other, _ := h.client()
	switchSession := Frame{Type: TypeCommand, ID: "switch", InstanceID: inst.ID, Command: json.RawMessage(`{"type":"fork"}`)}
	mustSend(t, other, switchSession)
	pendingSwitch := w.next(t)
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	wantError(t, request(t, c, stop), "restart", CodeBusy)
	mustSend(t, w.link, Frame{Type: TypeResponse, ID: pendingSwitch.ID, Result: json.RawMessage(`{"ok":false}`)})
	mustSend(t, w.link, Frame{Type: TypeRegister, ID: "ready", Instance: &inst})
	if f := w.next(t); f.ID != "ready" || f.Error != nil {
		t.Fatalf("worker ready: %+v", f)
	}
	mustSend(t, c, stop)
	type routed struct {
		f   Frame
		err error
	}
	worker, caller := make(chan Frame, 1), make(chan routed, 1)
	go func() { worker <- <-w.frames }()
	go func() { f, err := c.Read(); caller <- routed{f, err} }()
	select {
	case r := <-caller:
		t.Fatalf("saved legacy worker refused restart: %v / %v", r.f.Error, r.err)
	case f := <-worker:
		if string(f.Command) != `{"type":"shutdown"}` || f.SessionID != header.ID {
			t.Fatalf("not a legacy graceful shutdown: %+v", f)
		}
		other, _ := h.client()
		wantError(t, request(t, other, switchSession), "switch", CodeBusy)
		mustSend(t, w.link, Frame{Type: TypeResponse, ID: f.ID, Result: json.RawMessage(`{"ok":true}`)})
	case <-time.After(testTimeout):
		t.Fatal("restart neither routed nor rejected")
	}
	select {
	case r := <-caller:
		if r.err != nil || r.f.ID != stop.ID || r.f.Error != nil {
			t.Fatalf("shutdown response: %+v / %v", r.f, r.err)
		}
	case <-time.After(testTimeout):
		t.Fatal("shutdown response was not forwarded")
	}
}

func TestStopInstanceRoutesShutdownAndReportsOutcomeUnknown(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	inst, w := h.start(c, "start")
	if resp := request(t, c, Frame{Type: TypeAttach, ID: "att", InstanceID: inst.ID}); resp.Error != nil {
		t.Fatalf("attach = %+v", resp)
	}

	mustSend(t, c, Frame{Type: TypeCommand, ID: "busy", InstanceID: inst.ID, Command: json.RawMessage(`{"type":"prompt"}`)})
	w.next(t)
	mustSend(t, c, Frame{Type: TypeStopInstance, ID: "stop", InstanceID: inst.ID})
	stop := w.next(t)
	if stop.Type != TypeCommand || string(stop.Command) != `{"type":"shutdown"}` || stop.ID == "stop" {
		t.Fatalf("stop forwarded as %+v", stop)
	}

	mustSend(t, w.link, Frame{Type: TypeResponse, ID: stop.ID, Result: json.RawMessage(`{"stopped":true}`)})
	if err := w.link.Close(); err != nil {
		t.Fatalf("close worker: %v", err)
	}
	if resp := readFrame(t, c); resp.ID != "stop" || resp.Error != nil {
		t.Fatalf("stop response = %+v", resp)
	}
	wantError(t, readFrame(t, c), "busy", CodeOutcomeUnknown)
	gone := readFrame(t, c)
	if gone.Type != TypeEvent || gone.InstanceID != inst.ID || gone.Error == nil || gone.Error.Code != CodeWorkerLinkLost || !strings.Contains(gone.Error.Message, "may still be running") {
		t.Fatalf("unavailable event = %+v", gone)
	}
	if list := request(t, c, Frame{Type: TypeListInstances, ID: "l"}); len(list.Instances) != 0 {
		t.Fatalf("stopped instance still listed: %+v", list.Instances)
	}
}

func TestRoutedCommandOutcomeUnknownAfterWorkerLinkLoss(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	inst, w := h.start(c, "start")
	requestID := uuid.NewString()
	cmd := json.RawMessage(`{"type":"prompt"}`)
	mustSend(t, c, Frame{Type: TypeCommand, ID: "prompt", RequestID: requestID, InstanceID: inst.ID, Command: cmd})
	if routed := w.next(t); routed.Type != TypeCommand || routed.RequestID != requestID {
		t.Fatalf("routed command = %+v", routed)
	}
	if err := w.link.Close(); err != nil {
		t.Fatal(err)
	}
	resp := readFrame(t, c)
	wantError(t, resp, "prompt", CodeOutcomeUnknown)
	if resp.RequestID != requestID || resp.InstanceID != inst.ID {
		t.Fatalf("outcome unknown reply = %+v", resp)
	}
	for f := range w.frames {
		t.Fatalf("worker received a frame after the routed command: %+v", f)
	}
	wantError(t, request(t, c, Frame{Type: TypeCommand, ID: "after", RequestID: requestID, InstanceID: inst.ID, Command: cmd}), "after", CodeNotFound)
}

func TestCommandCorrelationCarriesIdentityWithoutAuthority(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	a, aWelcome := h.client()
	b, bWelcome := h.client()
	inst, w := h.start(a, "start")
	requestID := uuid.NewString()
	cmd := json.RawMessage(`{"type":"get_state"}`)

	mustSend(t, a, Frame{Type: TypeCommand, ID: "a", RequestID: requestID, InstanceID: inst.ID, Command: cmd})
	fromA := w.next(t)
	mustSend(t, b, Frame{Type: TypeCommand, ID: "b", RequestID: requestID, InstanceID: inst.ID, Command: cmd})
	fromB := w.next(t)
	if fromA.RequestID != requestID || fromA.ClientID != aWelcome.ClientID || fromA.InstanceID != inst.ID ||
		fromB.RequestID != requestID || fromB.ClientID != bWelcome.ClientID || fromB.InstanceID != inst.ID || fromA.ID == fromB.ID {
		t.Fatalf("routed frames = %+v, %+v", fromA, fromB)
	}

	mustSend(t, w.link, Frame{Type: TypeResponse, ID: requestID, Result: json.RawMessage(`{"spoof":true}`)})
	mustSend(t, w.link, Frame{Type: TypeResponse, ID: fromB.ID, RequestID: uuid.NewString(), Result: json.RawMessage(`{"who":"b"}`)})
	mustSend(t, w.link, Frame{Type: TypeResponse, ID: fromA.ID, Result: json.RawMessage(`{"who":"a"}`)})
	for name, tc := range map[string]struct {
		link *Link
		id   string
		want string
	}{"a": {a, "a", `{"who":"a"}`}, "b": {b, "b", `{"who":"b"}`}} {
		resp := readFrame(t, tc.link)
		if resp.Type != TypeResponse || resp.ID != tc.id || resp.RequestID != requestID || string(resp.Result) != tc.want {
			t.Fatalf("client %s response = %+v result %s", name, resp, resp.Result)
		}
	}

	for _, bad := range []string{"not-a-uuid", strings.ToUpper(uuid.NewString()), "6ba7b810-9dad-11d1-80b4-00c04fd430c8"} {
		resp := request(t, a, Frame{Type: TypeCommand, ID: "bad", RequestID: bad, InstanceID: inst.ID, Command: cmd})
		wantError(t, resp, "bad", CodeInvalidFrame)
		if resp.RequestID != "" {
			t.Fatalf("invalid request ID echoed: %+v", resp)
		}
	}
	mustSend(t, a, Frame{Type: TypeCommand, ID: "legacy", InstanceID: inst.ID, Command: cmd})
	if f := w.next(t); f.ClientID != aWelcome.ClientID || f.RequestID != "" {
		t.Fatalf("legacy command routed as %+v", f)
	}
}

func TestLaunchRequestIDIsRetainedByCoordinator(t *testing.T) {
	spoofed := make(chan error, 2)
	launch := func(h *harness) LaunchFunc {
		spawn := h.spawnWorker()
		return func(ctx context.Context, req LaunchRequest) error {
			_, _, err := Dial(ctx, h.path, Frame{Role: RoleWorker, Token: req.Token, Instance: &Instance{
				ID: req.InstanceID, NodeID: "node", CWD: req.CWD, SessionID: "spoof", LaunchRequestID: uuid.NewString(),
			}})
			spoofed <- err
			return spawn(ctx, req)
		}
	}
	h := startCoordinator(t, launch, launchTimeout)
	c, _ := h.client()
	requestID := uuid.NewString()
	mustSend(t, c, Frame{Type: TypeStartInstance, ID: "start", RequestID: requestID, CWD: h.cwd})
	w := h.nextWorker()
	resp := readFrame(t, c)
	if resp.Error != nil || resp.ID != "start" || resp.RequestID != requestID || resp.Instance == nil ||
		resp.Instance.ID != w.req.InstanceID || resp.Instance.LaunchRequestID != requestID || resp.Instance.ID == requestID {
		t.Fatalf("start response = %+v (instance %+v)", resp, resp.Instance)
	}
	if w.req.LaunchRequestID != requestID || w.welcome.Instance == nil || w.welcome.Instance.LaunchRequestID != requestID {
		t.Fatalf("launch request = %+v, worker welcome = %+v", w.req, w.welcome)
	}
	var protoErr *Error
	if err := <-spoofed; !errors.As(err, &protoErr) || protoErr.Code != CodeConflict {
		t.Fatalf("mismatched launch request ID registration = %v, want conflict", err)
	}

	summary := *resp.Instance
	summary.LaunchRequestID = uuid.NewString()
	summary.State = StateWorking
	mustSend(t, w.link, Frame{Type: TypeRegister, ID: "reg", Instance: &summary})
	if ack := w.next(t); ack.Instance == nil || ack.Instance.LaunchRequestID != requestID || ack.Instance.State != StateWorking {
		t.Fatalf("register ack = %+v", ack)
	}
	list := request(t, c, Frame{Type: TypeListInstances, ID: "list"})
	if len(list.Instances) != 1 || list.Instances[0].LaunchRequestID != requestID {
		t.Fatalf("listed instances = %+v", list.Instances)
	}

	legacy, _ := h.start(c, "legacy")
	if legacy.LaunchRequestID != "" {
		t.Fatalf("start without request ID retained %q", legacy.LaunchRequestID)
	}
	wantError(t, request(t, c, Frame{Type: TypeStartInstance, ID: "bad", RequestID: "bad", CWD: h.cwd}), "bad", CodeInvalidFrame)
}

func TestLostLaunchReplyLeavesRetainedRequestIDObservable(t *testing.T) {
	var launches atomic.Int32
	launch := func(h *harness) LaunchFunc {
		spawn := h.spawnWorker()
		return func(ctx context.Context, req LaunchRequest) error {
			launches.Add(1)
			return spawn(ctx, req)
		}
	}
	h := startCoordinator(t, launch, launchTimeout)
	c, _ := h.client()
	requestID := uuid.NewString()
	mustSend(t, c, Frame{Type: TypeStartInstance, ID: "start", RequestID: requestID, CWD: h.cwd})
	w := h.nextWorker()
	// The accepted launch's reply is never read: the connection drops.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	other, _ := h.client()
	list := request(t, other, Frame{Type: TypeListInstances, ID: "list"})
	if len(list.Instances) != 1 || list.Instances[0].ID != w.req.InstanceID || list.Instances[0].LaunchRequestID != requestID {
		t.Fatalf("instances after lost reply = %+v, launch %+v", list.Instances, w.req)
	}
	if n := launches.Load(); n != 1 {
		t.Fatalf("launches = %d, want 1", n)
	}
}

func TestIndependentInstancesInSameDirectory(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	first, _ := h.start(c, "one")
	second, _ := h.start(c, "two")
	if first.ID == second.ID || first.SessionID == second.SessionID {
		t.Fatalf("instances not independent: %+v %+v", first, second)
	}
	list := request(t, c, Frame{Type: TypeListInstances, ID: "l"})
	if len(list.Instances) != 2 {
		t.Fatalf("instances = %+v", list.Instances)
	}
}

func TestDirectShutdownRequiresInstanceStop(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	inst, _ := h.start(c, "start")
	response := request(t, c, Frame{Type: TypeCommand, ID: "stop", InstanceID: inst.ID, Command: json.RawMessage(`{"type":"shutdown"}`)})
	wantError(t, response, "stop", CodeInvalidFrame)
}

func TestDuplicateRequestCannotChangeSubscription(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	inst, w := h.start(c, "start")
	mustSend(t, c, Frame{Type: TypeCommand, ID: "pending", InstanceID: inst.ID, Command: json.RawMessage(`{"type":"get_state"}`)})
	w.next(t)
	response := request(t, c, Frame{Type: TypeAttach, ID: "pending", InstanceID: inst.ID})
	wantError(t, response, "pending", CodeDuplicateID)
}

func TestClientIdentitySurvivesConnectionReplacement(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	bootstrap, _ := h.client()
	if err := bootstrap.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		c, welcome, err := Dial(t.Context(), h.path, Frame{Role: RoleClient, ClientID: "stable-client"})
		if err != nil {
			t.Fatal(err)
		}
		if welcome.ClientID != "stable-client" {
			t.Errorf("identity replaced: %s", welcome.ClientID)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWelcomeAdvertisesOperationsAndLimits(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	_, welcome := h.client()
	if welcome.Limits == nil || welcome.Limits.FrameBytes != MaxFrameBytes || welcome.Limits.OutboundBytes != maxQueuedBytes || !slices.Contains(welcome.Operations, TypeWatchInstances) || !slices.Contains(welcome.Operations, TypeUnwatchInstances) {
		t.Fatalf("missing negotiated capabilities: %+v", welcome)
	}
}

func TestRegistrationCannotReplaceExistingWorker(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	link := NewLink(a)
	defer link.Close()
	original := &worker{instance: Instance{ID: "instance", SessionID: "owned", NodeID: "node", CWD: "/project"}}
	co := &coordinator{workers: map[string]*worker{"instance": original}, launches: map[string]*pendingLaunch{"instance": {token: "secret", cwd: "/project", ready: make(chan Instance, 1)}}}
	_, rejected := co.register(link, Frame{Token: "secret", Instance: &Instance{ID: "instance", SessionID: "different", NodeID: "node", CWD: "/project"}})
	if rejected == nil || co.workers["instance"] != original {
		t.Fatal("conflicting claimant replaced a live worker")
	}
}

func TestLaunchCanceledAfterStartupTimeout(t *testing.T) {
	launch := func(*harness) LaunchFunc {
		return func(ctx context.Context, req LaunchRequest) error {
			if req.Ready == nil {
				return errors.New("missing readiness signal")
			}
			<-ctx.Done()
			return ctx.Err()
		}
	}
	h := startCoordinator(t, launch, 10*time.Millisecond)
	c, _ := h.client()
	response := request(t, c, Frame{Type: TypeStartInstance, ID: "failed", CWD: h.cwd})
	wantError(t, response, "failed", CodeLaunchFailed)
}

func TestStartupTimeoutRevokesTokenBeforeCancelingLaunch(t *testing.T) {
	late := make(chan error, 1)
	launch := func(h *harness) LaunchFunc {
		return func(ctx context.Context, req LaunchRequest) error {
			<-ctx.Done()
			hello := Frame{Role: RoleWorker, Token: req.Token, Instance: &Instance{
				ID: req.InstanceID, NodeID: "node", CWD: req.CWD, SessionID: "late",
			}}
			link, _, err := Dial(context.Background(), h.path, hello)
			if err == nil {
				err = errors.Join(errors.New("registered after launch cancellation"), link.Close())
			}
			late <- err
			return ctx.Err()
		}
	}
	h := startCoordinator(t, launch, 50*time.Millisecond)
	c, _ := h.client()
	wantError(t, request(t, c, Frame{Type: TypeStartInstance, ID: "slow", CWD: h.cwd}), "slow", CodeLaunchFailed)
	select {
	case err := <-late:
		var protoErr *Error
		if !errors.As(err, &protoErr) || protoErr.Code != CodeUnauthorized {
			t.Fatalf("registration after cancellation = %v, want unauthorized", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("launch was not canceled")
	}
	if list := request(t, c, Frame{Type: TypeListInstances, ID: "l"}); len(list.Instances) != 0 {
		t.Fatalf("canceled launch registered: %+v", list.Instances)
	}
}

func TestLaunchContextOutlivesRegistrationDecision(t *testing.T) {
	early := make(chan bool, 1)
	launch := func(h *harness) LaunchFunc {
		spawn := h.spawnWorker()
		return func(ctx context.Context, req LaunchRequest) error {
			if err := spawn(ctx, req); err != nil {
				return err
			}
			<-ctx.Done()
			select {
			case <-req.Ready:
				early <- false
			default:
				early <- true
			}
			return ctx.Err()
		}
	}
	h := startCoordinator(t, launch, launchTimeout)
	c, _ := h.client()
	h.start(c, "s")
	select {
	case canceledBeforeReady := <-early:
		if canceledBeforeReady {
			t.Fatal("launch canceled before registration was observable")
		}
	case <-time.After(testTimeout):
		t.Fatal("launch context not released after registration")
	}
}

func TestProjectValidationDoesNotBlockCoordinator(t *testing.T) {
	entered := make(chan struct{}, maxLaunches+1)
	release := make(chan struct{})
	original := resolveProjectDir
	resolveProjectDir = func(string) (string, error) {
		entered <- struct{}{}
		<-release
		return "", errors.New("unavailable")
	}
	// Registered first so it runs after the coordinator stops: shutdown must
	// not wait for blocked filesystem checks.
	t.Cleanup(func() {
		close(release)
		resolveProjectDir = original
	})
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	other, _ := h.client()
	for i := range maxLaunches {
		mustSend(t, c, Frame{Type: TypeStartInstance, ID: fmt.Sprint(i), CWD: h.cwd})
		select {
		case <-entered:
		case <-time.After(testTimeout):
			t.Fatalf("validation %d did not start", i)
		}
	}
	if list := request(t, other, Frame{Type: TypeListInstances, ID: "l"}); list.ID != "l" || list.Error != nil {
		t.Fatalf("list during validation = %+v", list)
	}
	wantError(t, request(t, other, Frame{Type: TypeStartInstance, ID: "over", CWD: h.cwd}), "over", CodeLimitExceeded)
}

func TestProjectValidationFailureReleasesSlot(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	missing := filepath.Join(h.cwd, "missing")
	for i := range maxLaunches + 1 {
		wantError(t, request(t, c, Frame{Type: TypeStartInstance, ID: fmt.Sprint(i), CWD: missing}), fmt.Sprint(i), CodeInvalidFrame)
	}
	h.start(c, "ok")
}

// fill pads f through set so it encodes, with its newline, to slack bytes
// under MaxFrameBytes.
func fill(t *testing.T, f Frame, slack int, set func(*Frame, string)) Frame {
	t.Helper()
	set(&f, "")
	base, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	set(&f, strings.Repeat("x", MaxFrameBytes-1-slack-len(base)))
	return f
}

func waitFlushed(t *testing.T, l *Link) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		l.mu.Lock()
		queued := l.frames
		l.mu.Unlock()
		if queued == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("link did not flush")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOversizedResponseAfterRewriteRepliesLimitExceeded(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	inst, w := h.start(c, "start")
	longID := strings.Repeat("r", maxIDBytes)
	correlation := "9d5dae32-70d6-4cb7-819e-cf108314ec14"
	mustSend(t, c, Frame{Type: TypeCommand, ID: longID, RequestID: correlation, InstanceID: inst.ID, Command: json.RawMessage(`{"type":"get_state"}`)})
	route := w.next(t)
	mustSend(t, w.link, fill(t, Frame{Type: TypeResponse, ID: route.ID}, 16, func(f *Frame, s string) {
		f.Result = json.RawMessage(`{"d":"` + s + `"}`)
	}))
	resp := readFrame(t, c)
	wantError(t, resp, longID, CodeLimitExceeded)
	if resp.InstanceID != inst.ID || resp.RequestID != correlation || resp.Result != nil {
		t.Fatalf("limit response = %+v", resp)
	}

	mustSend(t, c, Frame{Type: TypeCommand, ID: "worker-limit", InstanceID: inst.ID, Command: json.RawMessage(`{"type":"get_state"}`)})
	route = w.next(t)
	mustSend(t, w.link, Frame{Type: TypeResponse, ID: route.ID, Error: &Error{Code: CodeLimitExceeded, Message: "result exceeds frame limit"}})
	resp = readFrame(t, c)
	wantError(t, resp, "worker-limit", CodeLimitExceeded)
	if resp.Error.Message != "result exceeds frame limit" {
		t.Fatalf("worker error not forwarded as-is: %+v", resp.Error)
	}
}

func TestOversizedCommandKeepsWorkerConnected(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	inst, w := h.start(c, "start")
	big := fill(t, Frame{Type: TypeCommand, ID: "big", InstanceID: inst.ID}, 16, func(f *Frame, s string) {
		f.Command = json.RawMessage(`{"type":"prompt","d":"` + s + `"}`)
	})
	wantError(t, request(t, c, big), "big", CodeLimitExceeded)
	mustSend(t, c, Frame{Type: TypeCommand, ID: "small", InstanceID: inst.ID, Command: json.RawMessage(`{"type":"get_state"}`)})
	if f := w.next(t); f.Type != TypeCommand || string(f.Command) != `{"type":"get_state"}` {
		t.Fatalf("worker frame after oversized command = %+v", f)
	}
}

func TestOversizedEventSendsLimitNotice(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	inst, w := h.start(c, "start")
	if resp := request(t, c, Frame{Type: TypeAttach, ID: "att", InstanceID: inst.ID}); resp.Error != nil {
		t.Fatalf("attach = %+v", resp)
	}
	position := Position{InstanceID: inst.ID, Epoch: "epoch", Offset: 1}
	mustSend(t, w.link, fill(t, Frame{Type: TypeEvent, Position: &position}, 16, func(f *Frame, s string) {
		f.Event = json.RawMessage(`{"d":"` + s + `"}`)
	}))
	notice := readFrame(t, c)
	if notice.Type != TypeEvent || notice.Event != nil || notice.Error == nil || notice.Error.Code != CodeLimitExceeded ||
		notice.InstanceID != inst.ID || notice.SessionID != inst.SessionID || notice.Position == nil || *notice.Position != position {
		t.Fatalf("oversized event notice = %+v (error %v)", notice, notice.Error)
	}
	mustSend(t, w.link, Frame{Type: TypeEvent, Event: json.RawMessage(`{"type":"next"}`)})
	if ev := readFrame(t, c); string(ev.Event) != `{"type":"next"}` || ev.Error != nil {
		t.Fatalf("event after notice = %+v", ev)
	}
}

func TestWorkerQueueOverflowReportsLostLink(t *testing.T) {
	workerSide, stalled := net.Pipe()
	defer stalled.Close()
	clientSide, reader := net.Pipe()
	defer reader.Close()
	clientLink, clientReader := NewLink(clientSide), NewLink(reader)
	defer clientLink.fail(io.EOF)
	defer clientReader.fail(io.EOF)
	workerLink := NewLink(workerSide)
	defer workerLink.fail(io.EOF)
	w := &worker{instance: Instance{ID: "worker", SessionID: "session"}, link: workerLink, routes: make(map[string]route)}
	c := &client{link: clientLink, pending: make(map[string]pendingRef), subs: map[string]struct{}{w.instance.ID: {}}}
	co := &coordinator{clients: map[string]*client{"client": c}, workers: map[string]*worker{w.instance.ID: w}}
	for i := range maxQueuedFrames - 1 {
		co.forward(c, Frame{ID: fmt.Sprintf("request-%d", i), InstanceID: w.instance.ID}, json.RawMessage(`{"type":"get_state"}`), "get_state")
	}
	mustSend(t, workerLink, Frame{Type: TypeResponse})
	co.forward(c, Frame{ID: "overflow", InstanceID: w.instance.ID}, json.RawMessage(`{"type":"get_state"}`), "get_state")
	wantError(t, readFrame(t, clientReader), "overflow", CodeUnavailable)
	if err := workerLink.Err(); !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("worker link error = %v, want overflow", err)
	}
	co.dropWorker(w)
	for range maxQueuedFrames - 1 {
		f := readFrame(t, clientReader)
		if f.Error == nil || f.Error.Code != CodeOutcomeUnknown || !strings.Contains(f.Error.Message, "outcome unknown") {
			t.Fatalf("pending reply = %+v (error %v)", f, f.Error)
		}
	}
	f := readFrame(t, clientReader)
	if f.Type != TypeEvent || f.InstanceID != w.instance.ID || f.SessionID != w.instance.SessionID ||
		f.Error == nil || f.Error.Code != CodeWorkerLinkLost || !strings.Contains(f.Error.Message, "outbound queue full") {
		t.Fatalf("lost link = %+v (error %v)", f, f.Error)
	}
}

func TestSlowSubscriberClosedWithoutAffectingWorker(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	inst, w := h.start(c, "start")
	if resp := request(t, c, Frame{Type: TypeAttach, ID: "att", InstanceID: inst.ID}); resp.Error != nil {
		t.Fatalf("attach = %+v", resp)
	}
	fast, _ := h.client()
	if resp := request(t, fast, Frame{Type: TypeAttach, ID: "attach-fast", InstanceID: inst.ID}); resp.Error != nil {
		t.Fatalf("attach fast reader = %+v", resp)
	}
	const events = 80
	ev := Frame{Type: TypeEvent, Event: json.RawMessage(`{"d":"` + strings.Repeat("x", 100_000) + `"}`)}
	for i := range events {
		mustSend(t, w.link, ev)
		if f := readFrame(t, fast); f.InstanceID != inst.ID || string(f.Event) != string(ev.Event) {
			t.Fatalf("fast subscriber event %d: wrong instance or payload", i)
		}
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	received := 0
	for {
		if _, err := c.Read(); err != nil {
			break
		}
		received++
	}
	if received >= events {
		t.Fatalf("slow subscriber received all %d events", received)
	}
	if err := w.link.Err(); err != nil {
		t.Fatalf("worker link closed: %v", err)
	}
	other, _ := h.client()
	if list := request(t, other, Frame{Type: TypeListInstances, ID: "l"}); len(list.Instances) != 1 {
		t.Fatalf("instances after slow subscriber = %+v", list.Instances)
	}
}

func TestEventSessionFollowsWorkerSwitch(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	c, _ := h.client()
	inst, w := h.start(c, "start")
	if resp := request(t, c, Frame{Type: TypeAttach, ID: "att", InstanceID: inst.ID}); resp.Error != nil {
		t.Fatalf("attach = %+v", resp)
	}
	mustSend(t, w.link, Frame{Type: TypeEvent, SessionID: "session-next", Event: json.RawMessage(`{"type":"session_start"}`)})
	if ev := readFrame(t, c); ev.SessionID != "session-next" || ev.InstanceID != inst.ID {
		t.Fatalf("switched session event = %+v", ev)
	}
	mustSend(t, w.link, Frame{Type: TypeEvent, Event: json.RawMessage(`{"type":"message"}`)})
	if ev := readFrame(t, c); ev.SessionID != inst.SessionID {
		t.Fatalf("event without session = %+v, want registry session %q", ev, inst.SessionID)
	}
}

func TestLegacyFirstFrameGetsCompatibleRejection(t *testing.T) {
	h := startCoordinator(t, nil, launchTimeout)
	h.client()
	tests := map[string]struct {
		line   string
		wantID string
	}{
		"get_state":   {`{"id":"cmd-1","type":"get_state"}`, "cmd-1"},
		"with args":   {`{"id":"p-1","type":"prompt","args":{"text":"hi"}}`, "p-1"},
		"no id":       {`{"type":"get_state"}`, ""},
		"not json":    {`hello`, ""},
		"coord frame": {`{"id":"x","type":"command","admin":true}`, ""},
		"long id":     {`{"id":"` + strings.Repeat("x", maxIDBytes+1) + `","type":"get_state"}`, ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			conn, err := net.Dial("unix", h.path)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write([]byte(tc.line + "\n")); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(conn)
			if err != nil {
				t.Fatalf("read reply: %v", err)
			}
			if tc.wantID == "" {
				if len(data) != 0 {
					t.Fatalf("non-legacy frame got reply %s", data)
				}
				return
			}
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], `"ok":false`) {
				t.Fatalf("legacy reply = %q", data)
			}
			var resp struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				OK    bool   `json:"ok"`
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Type != "response" || resp.ID != tc.wantID || resp.OK || resp.Error == nil ||
				resp.Error.Code == "" || !strings.Contains(resp.Error.Message, "has been removed") {
				t.Fatalf("legacy reply = %+v (error %+v)", resp, resp.Error)
			}
		})
	}
}

func TestRecoveredLaunchDoesNotSpawnAgain(t *testing.T) {
	path := filepath.Join(socketDir(t), "c.sock")
	cwd := t.TempDir()
	alias := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(cwd, alias); err != nil {
		t.Fatal(err)
	}
	receiptDir := filepath.Join(t.TempDir(), "receipts")
	var launches atomic.Int32
	held := make(chan *os.File, 1)
	identity := make(chan Frame, 1)
	dial := func(hello Frame) (*Link, Frame, error) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		return Dial(ctx, path, hello)
	}
	launch := func(ctx context.Context, r LaunchRequest) error {
		launches.Add(1)
		if r.Lifetime == nil {
			return errors.New("missing lifetime lock")
		}
		fd, err := unix.Dup(int(r.Lifetime.Fd()))
		if err != nil {
			return err
		}
		held <- os.NewFile(uintptr(fd), "fake worker lifetime")
		inst := Instance{ID: r.InstanceID, LaunchRequestID: r.LaunchRequestID, CWD: r.CWD, NodeID: "node", SessionID: "session", PID: 42}
		hello := Frame{Role: RoleWorker, Instance: &inst, Token: r.Token, Position: &Position{InstanceID: inst.ID, Epoch: "epoch"}}
		link, _, err := dial(hello)
		if err != nil {
			return err
		}
		identity <- hello
		<-ctx.Done()
		// The worker side stays connected independently of launch readiness.
		go func() {
			if _, err := link.Read(); err != nil && !errors.Is(err, io.EOF) {
				t.Error(err)
			}
		}()
		return nil
	}
	start := func() (func(), *Link, Frame) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := RunRecoverable(ctx, path, receiptDir, launch); err != nil {
				t.Error(err)
			}
		}()
		stop := func() { cancel(); <-done }
		t.Cleanup(stop)
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
			link, welcome, err := dial(Frame{Role: RoleClient})
			if err == nil {
				return stop, link, welcome
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("coordinator did not start")
		return nil, nil, Frame{}
	}
	stop, first, welcome := start()
	if welcome.LaunchRequestVersion != 7 {
		t.Fatalf("recoverable coordinator advertised launch ID version %d", welcome.LaunchRequestVersion)
	}
	if skew := time.Since(time.UnixMilli(welcome.CoordinatorTimeUnixMS)); skew < 0 || skew > testTimeout {
		t.Fatalf("welcome coordinator time is %v from now", skew)
	}
	wantError(t, request(t, first, Frame{Type: TypeStartInstance, ID: "v4", RequestID: uuid.NewString(), CWD: alias}), "v4", CodeInvalidFrame)
	future := launchIDAt(time.Now().Add(launchClockSkew + time.Hour))
	wantError(t, request(t, first, Frame{Type: TypeStartInstance, ID: "future", RequestID: future, CWD: alias}), "future", CodeClockSkew)
	reqID := launchIDAt(time.Now().Add(time.Second))
	mustSend(t, first, Frame{Type: TypeStartInstance, ID: "start", RequestID: reqID, CWD: alias})
	var hello Frame
	select {
	case hello = <-identity:
	case <-time.After(testTimeout):
		t.Fatal("worker did not register")
	}
	lock := <-held
	t.Cleanup(func() {
		if lock != nil {
			if err := lock.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	// The client does not consume the start reply before this generation ends.
	stop()
	_, next, recovered := start()
	baseline := request(t, next, Frame{Type: TypeWatchInstances, ID: "watch"})
	if baseline.Generation == welcome.Generation || baseline.Generation != recovered.Generation || len(baseline.Instances) != 0 {
		t.Fatalf("wrong baseline for a new generation: %+v", baseline)
	}
	startAgain := Frame{Type: TypeStartInstance, ID: "retry", RequestID: reqID, CWD: alias}
	wantError(t, request(t, next, startAgain), "retry", CodeOutcomeUnknown)
	forged := hello
	forged.Token = "wrong secret"
	_, _, err := dial(forged)
	var rejection *Error
	if !errors.As(err, &rejection) || rejection.Code != CodeUnauthorized {
		t.Fatalf("forged recovery: %v", err)
	}
	hello.Instance.PendingInputs = 2
	workerLink, _, err := dial(hello)
	if err != nil {
		t.Fatal(err)
	}
	if update := readFrame(t, next); update.Generation != recovered.Generation || update.Instance == nil || update.Instance.PendingInputs != 2 || update.Instance.LinkState != LinkConnected {
		t.Fatalf("missing recovered status: %+v", update)
	}
	if reply := request(t, next, startAgain); reply.Instance == nil || reply.Instance.ID != hello.Instance.ID || reply.Instance.LaunchRequestID != reqID || launches.Load() != 1 {
		t.Fatalf("retry did not reconcile: %+v, %d launches", reply, launches.Load())
	}
	wrongEpoch := hello
	wrongEpoch.Position = &Position{InstanceID: hello.Instance.ID, Epoch: "conflict"}
	_, _, err = dial(wrongEpoch)
	if !errors.As(err, &rejection) || rejection.Code != CodeUnauthorized {
		t.Fatalf("conflicting epoch: %v", err)
	}
	if err := workerLink.Close(); err != nil {
		t.Fatal(err)
	}
	if update := readFrame(t, next); update.Instance == nil || update.Instance.LinkState != LinkDisconnected {
		t.Fatalf("socket close did not leave disconnected status: %+v", update)
	}
	workerLink, _, err = dial(hello)
	if err != nil {
		t.Fatalf("same-generation redial: %v", err)
	}
	if update := readFrame(t, next); update.Instance == nil || update.Instance.LinkState != LinkConnected || update.Instance.ID != hello.Instance.ID {
		t.Fatalf("redial changed identity: %+v", update)
	}
	if err := workerLink.Close(); err != nil {
		t.Fatal(err)
	}
	if update := readFrame(t, next); update.Instance == nil || update.Instance.LinkState != LinkDisconnected {
		t.Fatalf("socket close did not leave disconnected status: %+v", update)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	lock = nil
	wantError(t, request(t, next, startAgain), "retry", CodeOutcomeUnknown)
}

func TestLauncherExistErrorIsNotAReceiptCollision(t *testing.T) {
	h := startCoordinator(t, func(*harness) LaunchFunc {
		return func(context.Context, LaunchRequest) error { return fmt.Errorf("create worker log: %w", os.ErrExist) }
	}, launchTimeout)
	c, _ := h.client()
	got := request(t, c, Frame{Type: TypeStartInstance, ID: "start", CWD: h.cwd, RequestID: uuid.NewString()})
	wantError(t, got, "start", CodeLaunchFailed)
}

func TestEmbeddedRunRemovesAnEpochWorkerOnDisconnect(t *testing.T) {
	h := startCoordinator(t, func(h *harness) LaunchFunc {
		return func(ctx context.Context, r LaunchRequest) error {
			inst := Instance{ID: r.InstanceID, CWD: r.CWD, NodeID: "node", SessionID: "session", PID: 42}
			dialCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), testTimeout)
			defer cancel()
			link, welcome, err := Dial(dialCtx, h.path, Frame{
				Role: RoleWorker, Instance: &inst, Token: r.Token,
				Position: &Position{InstanceID: inst.ID, Epoch: "epoch"},
			})
			if err == nil {
				h.workers <- &fakeWorker{req: r, link: link, welcome: welcome}
			}
			return err
		}
	}, launchTimeout)
	c, _ := h.client()
	request(t, c, Frame{Type: TypeWatchInstances, ID: "watch"})
	for range 2 {
		mustSend(t, c, Frame{Type: TypeStartInstance, ID: "start", CWD: h.cwd})
		w := h.nextWorker()
		readFrame(t, c) // instance update comes before readiness
		if f := readFrame(t, c); f.Error != nil || f.Instance == nil || f.Instance.ID != w.req.InstanceID {
			t.Fatalf("worker did not start: %+v", f)
		}
		if err := w.link.Close(); err != nil {
			t.Fatal(err)
		}
		if f := readFrame(t, c); f.Type != TypeInstanceUpdate || f.Instance != nil || f.Error == nil || f.Error.Code != CodeWorkerLinkLost {
			t.Fatalf("expected removal, got %+v", f)
		}
		if f := request(t, c, Frame{Type: TypeListInstances, ID: "list"}); len(f.Instances) != 0 {
			t.Fatalf("closed embedded workers remain in registry: %+v", f)
		}
	}
}

func TestStartSavedSessionBindsWorkerRegistration(t *testing.T) {
	wrong := make(chan error, 1)
	h := startCoordinator(t, func(h *harness) LaunchFunc {
		return func(ctx context.Context, req LaunchRequest) error {
			if req.SessionID == "" {
				return errors.New("launcher did not receive saved session ID")
			}
			instance := &Instance{ID: req.InstanceID, NodeID: "node", CWD: req.CWD, SessionID: uuid.NewString(), PID: 42}
			link, _, err := Dial(ctx, h.path, Frame{Role: RoleWorker, Token: req.Token, Instance: instance})
			if err == nil {
				wrong <- errors.Join(errors.New("registered with wrong session"), link.Close())
				return errors.New("wrong session accepted")
			}
			wrong <- err
			return h.spawnWorker()(ctx, req)
		}
	}, launchTimeout)
	c, _ := h.client()
	wantError(t, request(t, c, Frame{Type: TypeStartInstance, ID: "bad", CWD: h.cwd, SessionID: "/tmp/file"}), "bad", CodeInvalidFrame)
	id := uuid.NewString()
	resp := request(t, c, Frame{Type: TypeStartInstance, ID: "resume", CWD: h.cwd, SessionID: id})
	if resp.Error != nil || resp.Instance == nil || resp.Instance.SessionID != id {
		t.Fatalf("start saved session: %+v", resp)
	}
	var conflict *Error
	if err := <-wrong; !errors.As(err, &conflict) || conflict.Code != CodeConflict {
		t.Fatalf("wrong session registration: %v", err)
	}
}

func TestSavedSessionLaunchReservesOwner(t *testing.T) {
	started := make(chan struct{}, 2)
	allow := make(chan struct{})
	h := startCoordinator(t, func(h *harness) LaunchFunc {
		return func(ctx context.Context, req LaunchRequest) error {
			started <- struct{}{}
			select {
			case <-allow:
				return h.spawnWorker()(ctx, req)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}, time.Second)
	a, _ := h.client()
	b, _ := h.client()
	id := uuid.NewString()
	launch := Frame{Type: TypeStartInstance, ID: "start", CWD: h.cwd, SessionID: id}
	mustSend(t, a, launch)
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("first launch did not reach the launcher")
	}
	wantError(t, request(t, b, launch), "start", CodeBusy)
	close(allow)
	first := readFrame(t, a)
	if first.Instance == nil || first.Instance.SessionID != id {
		t.Fatalf("first launch lost its reservation: %+v", first)
	}
	if got := request(t, b, launch); got.Error != nil || got.Instance == nil || got.Instance.ID != first.Instance.ID {
		t.Fatalf("second resume did not return the first worker: %+v", got)
	}
	if len(started) != 0 {
		t.Fatal("second resume launched another worker")
	}
}

func TestInstanceStatus(t *testing.T) {
	resets := time.Date(2026, 11, 24, 14, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		instance Instance
		text     string
		active   bool
	}{
		{Instance{}, "unknown", false},
		{Instance{State: StateAwaitingUser}, "awaiting user", false},
		{Instance{State: StateWorking}, "working", true},
		{Instance{State: StateNeedsInput, PendingInputs: 2}, "needs input (2)", true},
		{Instance{State: StateUsageLimited, StateDetail: StateDetail{Provider: "codex", ResetsAt: resets}}, "usage limit: codex, resets " + resets.Local().Format("Mon Jan 2 15:04"), true},
		{Instance{State: StateRetrying, StateDetail: StateDetail{Attempt: 2, MaxAttempts: 5}}, "retrying (2/5)", true},
		{Instance{State: StatePaused, StateDetail: StateDetail{Target: "codex"}}, "paused: codex", true},
		{Instance{State: StateError, StateDetail: StateDetail{Message: "bad key"}}, "error: bad key", false},
	} {
		if got := test.instance.StatusText(); got != test.text {
			t.Errorf("StatusText() = %q, want %q", got, test.text)
		}
		if got := test.instance.Active(); got != test.active {
			t.Errorf("%s Active() = %v, want %v", test.instance.State, got, test.active)
		}
	}
}
