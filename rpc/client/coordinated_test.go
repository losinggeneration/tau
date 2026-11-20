package client

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/losinggeneration/tau/rpc/coord"
)

// fakeCoordinator accepts one client on a real Unix socket, completes the
// handshake, and hands the server side of the link to the test.
func fakeCoordinator(t *testing.T, eventBuffer int, opts ...CoordinatedOption) (*CoordinatedClient, *coord.Link, coord.Frame) {
	t.Helper()
	assign := func(coord.Frame) string { return "assigned" }
	if len(opts) > 0 {
		assign = func(hello coord.Frame) string { return hello.ClientID }
	}
	c, srv, hello, err := dialFake(t, eventBuffer, assign, 0, opts...)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c, srv, hello
}

// dialFake is fakeCoordinator with the welcome's client ID chosen by assign
// and the dial error returned.
func dialFake(t *testing.T, eventBuffer int, assign func(hello coord.Frame) string, launchVersion int, opts ...CoordinatedOption) (*CoordinatedClient, *coord.Link, coord.Frame, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close listener: %v", err)
		}
	})

	type accepted struct {
		link  *coord.Link
		hello coord.Frame
		err   error
	}
	ready := make(chan accepted, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ready <- accepted{err: err}
			return
		}
		link := coord.NewLink(conn)
		hello, err := link.Read()
		if err == nil {
			err = link.Send(coord.Frame{Type: coord.TypeWelcome, Version: coord.Version, Role: coord.RoleClient, Generation: "gen-1", ClientID: assign(hello), LaunchRequestVersion: launchVersion})
		}
		ready <- accepted{link: link, hello: hello, err: err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := dialCoordinated(ctx, path, eventBuffer, opts...)
	srv := <-ready
	if srv.err != nil {
		t.Fatalf("server handshake: %v", srv.err)
	}
	if err != nil {
		t.Cleanup(func() {
			if err := srv.link.Close(); err != nil {
				t.Errorf("close server link: %v", err)
			}
		})
		return nil, srv.link, srv.hello, err
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
		if err := srv.link.Close(); err != nil {
			t.Errorf("close server link: %v", err)
		}
	})
	return c, srv.link, srv.hello, nil
}

func readFrame(t *testing.T, link *coord.Link) coord.Frame {
	t.Helper()
	type result struct {
		f   coord.Frame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := link.Read()
		ch <- result{f, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("server read: %v", r.err)
		}
		return r.f
	case <-time.After(time.Second):
		t.Fatal("server read timed out")
	}
	return coord.Frame{}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCoordinatedWebSocketClient(t *testing.T) {
	const token = "device-token"
	const serverMS int64 = 1735689600000
	upgrader := websocket.Upgrader{Subprotocols: []string{"tau.coord.v1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rpc" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() {
			if err := ws.Close(); err != nil {
				t.Error(err)
			}
		}()
		if err := ws.WriteJSON(coord.Frame{Type: coord.TypeWelcome, Version: coord.Version, Role: coord.RoleClient, ClientID: "bridge-assigned", Generation: "generation", LaunchRequestVersion: 7, CoordinatorTimeUnixMS: serverMS}); err != nil {
			t.Error(err)
			return
		}
		var frame coord.Frame
		if err := ws.ReadJSON(&frame); err != nil {
			t.Error(err)
			return
		}
		u, err := uuid.Parse(frame.RequestID)
		sec, ns := u.Time().UnixTime()
		issued := time.Unix(sec, ns).UnixMilli()
		if frame.Type != coord.TypeStartInstance || frame.CWD != "/project" || frame.ID == "" || err != nil || u.Version() != 7 || issued < serverMS || issued > serverMS+2_000 {
			t.Errorf("unexpected start frame: %+v (issued %d)", frame, issued)
		}
		inst := coord.Instance{ID: "worker", LaunchRequestID: frame.RequestID, CWD: frame.CWD}
		if err := ws.WriteJSON(coord.Frame{Type: coord.TypeResponse, ID: frame.ID, InstanceID: inst.ID, Instance: &inst}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/rpc"
	ctx := testContext(t)
	if _, err := NewCoordinatedWebSocketClient(ctx, endpoint, "wrong-token"); err == nil || strings.Contains(err.Error(), "wrong-token") {
		t.Fatalf("unauthorized dial error: %v", err)
	}
	c, err := NewCoordinatedWebSocketClient(ctx, endpoint, token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	if c.ClientID() != "bridge-assigned" || c.Generation() != "generation" {
		t.Fatalf("welcome: %+v", c.Welcome())
	}
	inst, err := c.StartInstance(ctx, "/project")
	if err != nil || inst.ID != "worker" {
		t.Fatalf("start: %+v, %v", inst, err)
	}
}

func TestCoordinatedClientHandshakeSendsStableClientID(t *testing.T) {
	c, _, hello := fakeCoordinator(t, coordinatedEventBuffer)
	if hello.Type != coord.TypeHello || hello.Version != coord.Version || hello.Role != coord.RoleClient {
		t.Fatalf("hello = %+v", hello)
	}
	if hello.ClientID == "" || hello.ClientID != c.ClientID() {
		t.Fatalf("hello client id %q, client reports %q", hello.ClientID, c.ClientID())
	}
	if c.Welcome().Generation != "gen-1" {
		t.Fatalf("welcome = %+v", c.Welcome())
	}
}

func TestCoordinatedClientRequiresExplicitTarget(t *testing.T) {
	c, _, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)
	calls := map[string]func() error{
		"attach":  func() error { return c.Attach(ctx, "") },
		"detach":  func() error { return c.Detach(ctx, "") },
		"stop":    func() error { return c.StopInstance(ctx, "") },
		"command": func() error { _, err := c.Command(ctx, "", Command{Type: "get_state"}); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrTargetRequired) {
			t.Errorf("%s error = %v, want ErrTargetRequired", name, err)
		}
	}
	if n := pendingCount(c); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
}

func TestCoordinatedClientWatchInstances(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)
	send := func(f coord.Frame) {
		t.Helper()
		if err := srv.Send(f); err != nil {
			t.Fatalf("send %s: %v", f.Type, err)
		}
	}
	type result struct {
		instances []coord.Instance
		err       error
	}
	done := make(chan result, 1)
	go func() {
		instances, err := c.WatchInstances(ctx)
		done <- result{instances, err}
	}()
	f := readFrame(t, srv)
	if f.Type != coord.TypeWatchInstances || f.InstanceID != "" {
		t.Fatalf("watch frame = %+v", f)
	}
	inst := coord.Instance{ID: "instance", SessionID: "session"}
	send(coord.Frame{Type: coord.TypeResponse, ID: f.ID, Instances: []coord.Instance{inst}})
	inst.PendingInputs = 1
	send(coord.Frame{Type: coord.TypeInstanceUpdate, InstanceID: inst.ID, SessionID: inst.SessionID, Instance: &inst})
	if r := <-done; r.err != nil || len(r.instances) != 1 || r.instances[0].PendingInputs != 0 {
		t.Fatalf("watch snapshot = %+v", r)
	}
	select {
	case update := <-c.Events():
		if update.Type != coord.TypeInstanceUpdate || update.Instance == nil || *update.Instance != inst {
			t.Fatalf("summary event = %+v", update)
		}
	case <-ctx.Done():
		t.Fatal("missing summary event")
	}
	unwatched := make(chan error, 1)
	go func() { unwatched <- c.UnwatchInstances(ctx) }()
	f = readFrame(t, srv)
	if f.Type != coord.TypeUnwatchInstances {
		t.Fatalf("unwatch frame = %+v", f)
	}
	send(coord.Frame{Type: coord.TypeResponse, ID: f.ID})
	if err := <-unwatched; err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatedClientCommandRoundTrip(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)

	type result struct {
		resp *Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.Command(ctx, "inst-a", Command{ID: "cmd-1", Type: "get_state", Args: map[string]any{}})
		done <- result{resp, err}
	}()

	f := readFrame(t, srv)
	if f.Type != coord.TypeCommand || f.InstanceID != "inst-a" || f.ID == "" {
		t.Fatalf("command frame = %+v", f)
	}
	var nested Command
	if err := json.Unmarshal(f.Command, &nested); err != nil {
		t.Fatalf("nested command: %v", err)
	}
	if nested.ID != "cmd-1" || nested.Type != "get_state" {
		t.Fatalf("nested command = %+v", nested)
	}
	result1 := json.RawMessage(`{"type":"response","id":"cmd-1","ok":false,"error":{"code":"busy","message":"try later"}}`)
	if err := srv.Send(coord.Frame{Type: coord.TypeResponse, ID: f.ID, InstanceID: "inst-a", Result: result1}); err != nil {
		t.Fatalf("send reply: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("Command: %v", r.err)
	}
	if r.resp.OK || r.resp.Error == nil || r.resp.Error.Code != "busy" || r.resp.ID != "cmd-1" {
		t.Fatalf("response = %+v", r.resp)
	}
}

func TestCommandResponseAcceptsBareResult(t *testing.T) {
	resp, err := commandResponse("cmd-2", json.RawMessage(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("commandResponse: %v", err)
	}
	if !resp.OK || resp.ID != "cmd-2" || resp.Result["model"] != "m" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestCoordinatedClientReturnsProtocolErrors(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)

	done := make(chan error, 1)
	go func() { done <- c.Attach(ctx, "missing") }()
	f := readFrame(t, srv)
	if err := srv.Send(coord.Frame{Type: coord.TypeResponse, ID: f.ID, Error: &coord.Error{Code: coord.CodeNotFound, Message: "instance not found"}}); err != nil {
		t.Fatalf("send reply: %v", err)
	}
	err := <-done
	var perr *coord.Error
	if !errors.As(err, &perr) || perr.Code != coord.CodeNotFound {
		t.Fatalf("Attach error = %v, want not_found", err)
	}
}

func TestCoordinatedClientContextCancelCleansPending(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- c.Attach(ctx, "inst-a") }()
	f := readFrame(t, srv)
	if f.Type != coord.TypeAttach {
		t.Fatalf("frame = %+v", f)
	}
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("error = %v, want canceled with unknown outcome", err)
	}
	if n := pendingCount(c); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}

	// A late reply for the abandoned request must not disturb the client.
	if err := srv.Send(coord.Frame{Type: coord.TypeResponse, ID: f.ID}); err != nil {
		t.Fatalf("send late reply: %v", err)
	}
	listCtx := testContext(t)
	listDone := make(chan error, 1)
	go func() {
		_, err := c.ListInstances(listCtx)
		listDone <- err
	}()
	g := readFrame(t, srv)
	if err := srv.Send(coord.Frame{Type: coord.TypeResponse, ID: g.ID, Instances: []coord.Instance{{ID: "inst-a"}}}); err != nil {
		t.Fatalf("send list reply: %v", err)
	}
	if err := <-listDone; err != nil {
		t.Fatalf("ListInstances after late reply: %v", err)
	}
}

func TestCoordinatedClientBoundsPending(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan error, coord.MaxPending)
	for range coord.MaxPending {
		go func() { errs <- c.Attach(ctx, "inst-a") }()
		readFrame(t, srv)
	}
	if n := pendingCount(c); n != coord.MaxPending {
		t.Fatalf("pending = %d, want %d", n, coord.MaxPending)
	}
	if err := c.Attach(ctx, "inst-a"); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("over-limit error = %v, want ErrTooManyPending", err)
	}
	cancel()
	for range coord.MaxPending {
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatalf("pending error = %v, want canceled", err)
		}
	}
}

func TestCoordinatedClientTransportCloseResolvesWaiters(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)

	done := make(chan error, 1)
	go func() {
		_, err := c.Command(ctx, "inst-a", Command{Type: "send_message", Args: map[string]any{"text": "hi"}})
		done <- err
	}()
	readFrame(t, srv)
	if err := srv.Close(); err != nil {
		t.Fatalf("close server: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("error = %v, want ErrOutcomeUnknown", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter hung after transport close")
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("client did not observe disconnect")
	}
	if _, ok := <-c.Events(); ok {
		t.Fatal("events channel still open after disconnect")
	}
	if c.Err() == nil {
		t.Fatal("Err() = nil after disconnect")
	}
	if _, err := c.ListInstances(ctx); err == nil {
		t.Fatal("request after disconnect succeeded")
	}
}

func TestCoordinatedClientSlowConsumerDisconnects(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, 2)
	// Two events fill the buffer; the third must disconnect, not be dropped.
	for i := range 3 {
		ev := json.RawMessage(`{"type":"text_delta","delta":"x"}`)
		if err := srv.Send(coord.Frame{Type: coord.TypeEvent, InstanceID: "inst-a", SessionID: "s", Event: ev}); err != nil {
			t.Fatalf("send event %d: %v", i, err)
		}
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("slow consumer was not disconnected")
	}
	if !errors.Is(c.Err(), ErrSlowEventConsumer) {
		t.Fatalf("Err() = %v, want ErrSlowEventConsumer", c.Err())
	}
	var got int
	for f := range c.Events() {
		if f.InstanceID != "inst-a" {
			t.Fatalf("event = %+v", f)
		}
		got++
	}
	if got != 2 {
		t.Fatalf("buffered events = %d, want 2", got)
	}
}

func pendingCount(c *CoordinatedClient) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

// runCoordinator runs the real coordinator in-process with workers that are
// goroutines speaking the worker protocol.
func runCoordinator(t *testing.T) (string, chan json.RawMessage) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coord", "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	commands := make(chan json.RawMessage, 16)
	runErr := make(chan error, 1)
	go func() {
		runErr <- coord.Run(ctx, path, func(_ context.Context, req coord.LaunchRequest) error {
			go fakeWorker(ctx, path, req, commands)
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-runErr; err != nil {
			t.Errorf("coordinator: %v", err)
		}
	})

	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return path, commands
		}
		if time.Now().After(deadline) {
			t.Fatal("coordinator socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func fakeWorker(ctx context.Context, path string, req coord.LaunchRequest, commands chan<- json.RawMessage) {
	link, _, err := coord.Dial(ctx, path, coord.Frame{
		Role:     coord.RoleWorker,
		Token:    req.Token,
		Instance: &coord.Instance{ID: req.InstanceID, NodeID: "node", CWD: req.CWD, SessionID: cmp.Or(req.SessionID, "sess-"+req.InstanceID)},
	})
	if err != nil {
		return
	}
	defer func() {
		if err := link.Close(); err != nil {
			return
		}
	}()
	stop := context.AfterFunc(ctx, func() {
		if err := link.Close(); err != nil {
			return
		}
	})
	defer stop()
	for {
		f, err := link.Read()
		if err != nil {
			return
		}
		if f.Type != coord.TypeCommand {
			continue
		}
		commands <- f.Command
		var cmd Command
		if err := json.Unmarshal(f.Command, &cmd); err != nil {
			return
		}
		if cmd.Type == "ui_probe" {
			ev := json.RawMessage(`{"type":"ui_confirm","id":"ui-1","title":"ok?"}`)
			if err := link.Send(coord.Frame{Type: coord.TypeEvent, Event: ev}); err != nil {
				return
			}
		}
		result, err := json.Marshal(Response{Type: "response", ID: cmd.ID, OK: true, Result: map[string]any{"cmd": cmd.Type}})
		if err != nil {
			return
		}
		if err := link.Send(coord.Frame{Type: coord.TypeResponse, ID: f.ID, Result: result}); err != nil {
			return
		}
		if cmd.Type == "shutdown" {
			return
		}
	}
}

func TestCoordinatedClientLifecycleWithCoordinator(t *testing.T) {
	path, commands := runCoordinator(t)
	ctx := testContext(t)

	c, err := NewCoordinatedClient(ctx, path)
	if err != nil {
		t.Fatalf("NewCoordinatedClient: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	cwd := t.TempDir()
	a, err := c.StartInstance(ctx, cwd)
	if err != nil {
		t.Fatalf("StartInstance a: %v", err)
	}
	b, err := c.StartInstance(ctx, cwd)
	if err != nil {
		t.Fatalf("StartInstance b: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("instances share id %q", a.ID)
	}

	list, err := c.ListInstances(ctx)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("instances = %+v, want 2", list)
	}

	if err := c.Attach(ctx, a.ID); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	resp, err := c.Command(ctx, a.ID, Command{Type: "ui_probe"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if !resp.OK || resp.Result["cmd"] != "ui_probe" {
		t.Fatalf("response = %+v", resp)
	}
	<-commands

	select {
	case f := <-c.Events():
		if f.InstanceID != a.ID || f.SessionID != a.SessionID {
			t.Fatalf("event target = %q/%q, want %q/%q", f.InstanceID, f.SessionID, a.ID, a.SessionID)
		}
		var evt Event
		if err := evt.FromJSON(f.Event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if evt.Type != "ui_confirm" || evt.Payload["id"] != "ui-1" {
			t.Fatalf("event = %+v", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("no event from attached instance")
	}

	// Switching views detaches without stopping the instance.
	if err := c.Detach(ctx, a.ID); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if err := c.Attach(ctx, b.ID); err != nil {
		t.Fatalf("Attach b: %v", err)
	}
	select {
	case cmd := <-commands:
		t.Fatalf("view switch sent command %s", cmd)
	default:
	}
	list, err = c.ListInstances(ctx)
	if err != nil {
		t.Fatalf("ListInstances after detach: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("instances after detach = %+v, want 2", list)
	}

	if err := c.StopInstance(ctx, a.ID); err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	var stopped Command
	if err := json.Unmarshal(<-commands, &stopped); err != nil {
		t.Fatalf("decode stop command: %v", err)
	}
	if stopped.Type != "shutdown" {
		t.Fatalf("stop sent %+v, want shutdown", stopped)
	}
}

func TestCommandResponseRestoresCallerIdentity(t *testing.T) {
	resp, err := commandResponse("caller-id", json.RawMessage(`{"type":"response","id":"internal-route-id","ok":true,"result":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "caller-id" {
		t.Fatalf("internal route identity leaked: got %q", resp.ID)
	}
}

func TestStopInstanceReportsWorkerCleanupFailure(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	done := make(chan error, 1)
	go func() { done <- c.StopInstance(testContext(t), "inst-a") }()
	frame := readFrame(t, srv)
	if err := srv.Send(coord.Frame{Type: coord.TypeResponse, ID: frame.ID, Result: json.RawMessage(`{"type":"response","id":"route","ok":false,"error":{"code":"cleanup_failed","message":"storage close failed"}}`)}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("worker cleanup failure reported as successful stop")
	}
}

const testEpoch = "epoch-1"

func posEvent(instanceID string, offset uint64) coord.Frame {
	return coord.Frame{
		Type: coord.TypeEvent, InstanceID: instanceID, SessionID: "s",
		Position: &coord.Position{InstanceID: instanceID, Epoch: testEpoch, Offset: offset},
		Event:    json.RawMessage(fmt.Sprintf(`{"type":"text_delta","delta":"%d"}`, offset)),
	}
}

func sendFrames(t *testing.T, srv *coord.Link, frames ...coord.Frame) {
	t.Helper()
	for _, f := range frames {
		if err := srv.Send(f); err != nil {
			t.Fatalf("send %s: %v", f.Type, err)
		}
	}
}

type routedCommand struct {
	Type string          `json:"type"`
	Args json.RawMessage `json:"args"`
}

func readCommand(t *testing.T, srv *coord.Link) (coord.Frame, routedCommand) {
	t.Helper()
	f := readFrame(t, srv)
	if f.Type != coord.TypeCommand {
		t.Fatalf("frame = %+v, want command", f)
	}
	var cmd routedCommand
	if err := json.Unmarshal(f.Command, &cmd); err != nil {
		t.Fatalf("decode command: %v", err)
	}
	return f, cmd
}

// sendWorkerReply answers f the way a worker's response reaches a client:
// a full worker response inside the coordinated reply.
func sendWorkerReply(t *testing.T, srv *coord.Link, f coord.Frame, response map[string]any) {
	t.Helper()
	response["type"], response["id"] = "response", "route"
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("encode reply: %v", err)
	}
	sendFrames(t, srv, coord.Frame{Type: coord.TypeResponse, ID: f.ID, InstanceID: f.InstanceID, Result: data})
}

func sendWorkerResult(t *testing.T, srv *coord.Link, f coord.Frame, result any) {
	t.Helper()
	sendWorkerReply(t, srv, f, map[string]any{"ok": true, "result": result})
}

func offsets(frames []coord.Frame) []uint64 {
	out := make([]uint64, 0, len(frames))
	for _, f := range frames {
		if f.Position != nil {
			out = append(out, f.Position.Offset)
		}
	}
	return out
}

func stageCount(c *CoordinatedClient) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.stages)
}

func TestCoordinatedClientStableClientID(t *testing.T) {
	c, _, hello := fakeCoordinator(t, coordinatedEventBuffer, WithClientID("client-a"))
	if hello.ClientID != "client-a" || c.ClientID() != "client-a" || c.Welcome().ClientID != "client-a" {
		t.Fatalf("hello %q, client %q, welcome %q; want client-a", hello.ClientID, c.ClientID(), c.Welcome().ClientID)
	}
	for _, id := range []string{"", strings.Repeat("x", maxClientIDBytes+1)} {
		_, err := NewCoordinatedClient(testContext(t), filepath.Join(t.TempDir(), "none.sock"), WithClientID(id))
		if !errors.Is(err, ErrInvalidClientID) {
			t.Fatalf("client id of %d bytes: error = %v, want ErrInvalidClientID", len(id), err)
		}
	}
}

func TestCoordinatedClientRejectsReassignedClientID(t *testing.T) {
	_, _, hello, err := dialFake(t, coordinatedEventBuffer, func(coord.Frame) string { return "other" }, 0, WithClientID("client-a"))
	if hello.ClientID != "client-a" {
		t.Fatalf("hello client id = %q", hello.ClientID)
	}
	if !errors.Is(err, ErrInvalidClientID) {
		t.Fatalf("dial error = %v, want ErrInvalidClientID", err)
	}
}

func TestCoordinatedClientUsesAdvertisedLaunchIDVersion(t *testing.T) {
	c, srv, _, err := dialFake(t, coordinatedEventBuffer, func(coord.Frame) string { return "assigned" }, 7)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.StartInstance(t.Context(), "/project")
		done <- err
	}()
	f := readFrame(t, srv)
	if f.Type != coord.TypeStartInstance || !coord.ValidLaunchRequestID(f.RequestID) || coord.ValidRequestID(f.RequestID) {
		t.Fatalf("expected UUIDv7 start: %+v", f)
	}
	sendFrames(t, srv, coord.Frame{Type: coord.TypeResponse, ID: f.ID, Instance: &coord.Instance{ID: "inst", LaunchRequestID: f.RequestID}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatedClientRequestIDs(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)

	supplied := NewRequestID()
	done := make(chan error, 1)
	go func() {
		_, err := c.CommandWithRequestID(ctx, "inst-a", supplied, Command{Type: "prompt"})
		done <- err
	}()
	f := readFrame(t, srv)
	if f.RequestID != supplied {
		t.Fatalf("request id = %q, want caller's %q", f.RequestID, supplied)
	}
	sendWorkerResult(t, srv, f, map[string]any{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	go func() {
		_, err := c.Command(ctx, "inst-a", Command{Type: "prompt"})
		done <- err
	}()
	f = readFrame(t, srv)
	if !coord.ValidRequestID(f.RequestID) || f.RequestID == supplied {
		t.Fatalf("generated request id = %q", f.RequestID)
	}
	sendWorkerResult(t, srv, f, map[string]any{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	go func() {
		_, err := c.StartInstance(ctx, "/project")
		done <- err
	}()
	f = readFrame(t, srv)
	if f.Type != coord.TypeStartInstance || !coord.ValidRequestID(f.RequestID) {
		t.Fatalf("start frame = %+v", f)
	}
	sendFrames(t, srv, coord.Frame{Type: coord.TypeResponse, ID: f.ID, RequestID: f.RequestID, Instance: &coord.Instance{ID: "inst-b", LaunchRequestID: f.RequestID}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	invalid := []string{"not-a-uuid", strings.ToUpper(supplied), "00000000-0000-1000-8000-000000000000"}
	for _, id := range invalid {
		if _, err := c.CommandWithRequestID(ctx, "inst-a", id, Command{Type: "prompt"}); !errors.Is(err, ErrInvalidRequestID) {
			t.Errorf("command request id %q: error = %v", id, err)
		}
		if _, err := c.StartInstanceWithRequestID(ctx, "/project", id); !errors.Is(err, ErrInvalidRequestID) {
			t.Errorf("start request id %q: error = %v", id, err)
		}
		if err := c.StopInstanceWithRequestID(ctx, "inst-a", id); !errors.Is(err, ErrInvalidRequestID) {
			t.Errorf("stop request id %q: error = %v", id, err)
		}
	}
	// Rejected requests were never sent: the next frame is this one.
	go func() {
		_, err := c.ListInstances(ctx)
		done <- err
	}()
	f = readFrame(t, srv)
	if f.Type != coord.TypeListInstances {
		t.Fatalf("invalid request reached the coordinator: %+v", f)
	}
	sendFrames(t, srv, coord.Frame{Type: coord.TypeResponse, ID: f.ID})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatedClientOutcomeUnknownCarriesIdentity(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)

	requestID := NewRequestID()
	done := make(chan error, 1)
	go func() {
		_, err := c.CommandWithRequestID(ctx, "inst-a", requestID, Command{Type: "prompt"})
		done <- err
	}()
	f := readFrame(t, srv)
	sendFrames(t, srv, coord.Frame{Type: coord.TypeResponse, ID: f.ID, RequestID: requestID, InstanceID: "inst-a", Error: &coord.Error{Code: coord.CodeOutcomeUnknown, Message: "worker link lost after routing"}})
	err := <-done
	unknown, ok := errors.AsType[*OutcomeUnknownError](err)
	if !ok || unknown.RequestID != requestID || unknown.InstanceID != "inst-a" || unknown.Op != coord.TypeCommand {
		t.Fatalf("error = %#v, want outcome unknown for %s", err, requestID)
	}
	if perr, ok := errors.AsType[*coord.Error](err); !ok || perr.Code != coord.CodeOutcomeUnknown || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("error = %v, want coordinator outcome_unknown", err)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	second := NewRequestID()
	go func() {
		done <- c.StopInstanceWithRequestID(cancelCtx, "inst-a", second)
	}()
	readFrame(t, srv)
	cancel()
	err = <-done
	if unknown, ok := errors.AsType[*OutcomeUnknownError](err); !ok || unknown.RequestID != second || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stop error = %v", err)
	}
}

func TestEventCursorAccept(t *testing.T) {
	cursor := NewEventCursor(coord.Position{InstanceID: "inst-a", Epoch: testEpoch, Offset: 4})
	if ok, err := cursor.Accept(posEvent("inst-a", 4)); ok || err != nil {
		t.Fatalf("duplicate: %v, %v", ok, err)
	}
	meta := coord.Frame{Type: coord.TypeEvent, InstanceID: "inst-a", Instance: &coord.Instance{ID: "inst-a"}}
	if ok, err := cursor.Accept(meta); !ok || err != nil || cursor.Position().Offset != 4 {
		t.Fatalf("positionless frame: %v, %v, at %d", ok, err, cursor.Position().Offset)
	}
	if ok, err := cursor.Accept(posEvent("inst-a", 5)); !ok || err != nil || cursor.Position().Offset != 5 {
		t.Fatalf("next: %v, %v, at %d", ok, err, cursor.Position().Offset)
	}

	otherEpoch := posEvent("inst-a", 6)
	otherEpoch.Position.Epoch = "epoch-2"
	misaddressed := posEvent("inst-a", 6)
	misaddressed.Position.InstanceID = "inst-b"
	omitted := posEvent("inst-a", 6)
	omitted.Event = json.RawMessage(`{"type":"event_omitted"}`)
	linkLost := coord.Frame{Type: coord.TypeEvent, InstanceID: "inst-a", Error: &coord.Error{Code: coord.CodeWorkerLinkLost}}
	rejected := map[string]struct {
		frame coord.Frame
		cause error
	}{
		"gap":        {posEvent("inst-a", 7), ErrEventGap},
		"epoch":      {otherEpoch, ErrStaleCursor},
		"target":     {posEvent("inst-b", 6), ErrEventTarget},
		"position":   {misaddressed, ErrEventTarget},
		"untargeted": {coord.Frame{Type: coord.TypeEvent}, ErrEventTarget},
		"omitted":    {omitted, ErrEventOmitted},
		"lost link":  {linkLost, linkLost.Error},
	}
	for name, tc := range rejected {
		ok, err := cursor.Accept(tc.frame)
		resync, isResync := errors.AsType[*ResyncError](err)
		if ok || !errors.Is(err, ErrResyncRequired) || !errors.Is(err, tc.cause) || !isResync || resync.Cursor.Offset != 5 {
			t.Errorf("%s: %v, %v", name, ok, err)
		}
		if cursor.Position().Offset != 5 {
			t.Fatalf("%s moved cursor to %d", name, cursor.Position().Offset)
		}
	}
}

func TestLiveStageOverflowIsExplicit(t *testing.T) {
	cursor := NewEventCursor(coord.Position{InstanceID: "inst-a", Epoch: testEpoch})
	var byCount liveStage
	for i := range maxStagedFrames + 1 {
		byCount.add(posEvent("inst-a", uint64(i+1)))
	}
	byBytes := liveStage{bytes: maxStagedBytes}
	byBytes.add(posEvent("inst-a", 1))
	for name, s := range map[string]*liveStage{"count": &byCount, "bytes": &byBytes} {
		if !s.overflow || s.frames != nil {
			t.Errorf("%s: stage not marked overflowed: %d frames", name, len(s.frames))
		}
		if _, err := cursor.handoff(s); !errors.Is(err, ErrReconstructionOverflow) || !errors.Is(err, ErrResyncRequired) {
			t.Errorf("%s: handoff error = %v, want overflow", name, err)
		}
	}
}

const testSnapshot = `{
	"position": {"instance_id": "inst-a", "epoch": "epoch-1", "offset": 4},
	"instance": {"instance_id": "inst-a", "session_id": "s"},
	"history": {"entries": [{"entry_id": "e1", "message": {"role": "user"}}, {"entry_id": "e2", "omitted": true}], "before": "e1"},
	"state": {"session_id": "s", "model": "provider/model", "thinking_level": "high", "is_streaming": true, "queue": {"steering": 1, "follow_up": 0}},
	"live": {"messages": []},
	"ui_requests": [{"id": "ui-1"}]
}`

func TestAttachSnapshotStagesLiveEvents(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)

	type result struct {
		h   *Handoff
		err error
	}
	done := make(chan result, 1)
	go func() {
		h, err := c.AttachSnapshot(ctx, "inst-a")
		done <- result{h, err}
	}()
	attach := readFrame(t, srv)
	if attach.Type != coord.TypeAttach || attach.InstanceID != "inst-a" {
		t.Fatalf("first frame = %+v, want attach", attach)
	}
	sendFrames(t, srv, posEvent("inst-a", 3), posEvent("inst-b", 1), coord.Frame{Type: coord.TypeResponse, ID: attach.ID, InstanceID: "inst-a"})
	f, cmd := readCommand(t, srv)
	if cmd.Type != cmdGetSnapshot || f.InstanceID != "inst-a" || f.RequestID != "" {
		t.Fatalf("snapshot request = %+v %+v", f, cmd)
	}
	sendFrames(t, srv, posEvent("inst-a", 4), posEvent("inst-a", 5))
	sendWorkerResult(t, srv, f, json.RawMessage(testSnapshot))

	r := <-done
	if r.err != nil {
		t.Fatalf("AttachSnapshot: %v", r.err)
	}
	s := r.h.Snapshot
	if s.Position.Offset != 4 || s.State.SessionID != "s" || !s.State.IsStreaming || s.State.ThinkingLevel != "high" ||
		len(s.History.Entries) != 2 || s.History.Entries[0].EntryID != "e1" || !s.History.Entries[1].Omitted ||
		s.History.Before != "e1" || len(s.UIRequests) != 1 || len(s.Live) == 0 {
		t.Fatalf("snapshot = %+v", s)
	}
	if got := offsets(r.h.Events); len(got) != 1 || got[0] != 5 {
		t.Fatalf("handoff events = %v, want [5]", got)
	}
	if r.h.Cursor.Position().Offset != 5 || stageCount(c) != 0 {
		t.Fatalf("cursor at %d, stages %d", r.h.Cursor.Position().Offset, stageCount(c))
	}

	// Only the reconstructed instance was withheld from Events.
	select {
	case f := <-c.Events():
		if f.InstanceID != "inst-b" {
			t.Fatalf("event = %+v, want inst-b", f)
		}
	case <-ctx.Done():
		t.Fatal("other instance's event was withheld")
	}
	select {
	case f := <-c.Events():
		t.Fatalf("staged event leaked to Events: %+v", f)
	default:
	}

	sendFrames(t, srv, posEvent("inst-a", 6))
	select {
	case f := <-c.Events():
		if ok, err := r.h.Cursor.Accept(f); !ok || err != nil {
			t.Fatalf("live continuation: %v, %v", ok, err)
		}
	case <-ctx.Done():
		t.Fatal("live event after handoff missing")
	}
}

func TestAttachSnapshotRejectsStagedGap(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)
	done := make(chan error, 1)
	go func() {
		_, err := c.AttachSnapshot(ctx, "inst-a")
		done <- err
	}()
	attach := readFrame(t, srv)
	sendFrames(t, srv, coord.Frame{Type: coord.TypeResponse, ID: attach.ID, InstanceID: "inst-a"})
	f, _ := readCommand(t, srv)
	sendFrames(t, srv, posEvent("inst-a", 6))
	sendWorkerResult(t, srv, f, json.RawMessage(testSnapshot))
	err := <-done
	if resync, ok := errors.AsType[*ResyncError](err); !ok || !errors.Is(err, ErrEventGap) || resync.Got == nil || resync.Got.Offset != 6 {
		t.Fatalf("error = %v, want gap after 4", err)
	}
	if stageCount(c) != 0 {
		t.Fatal("stage left behind")
	}
}

func TestResumeReplaysAfterCursorAndMergesStagedLive(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)
	after := coord.Position{InstanceID: "inst-a", Epoch: testEpoch, Offset: 2}

	type result struct {
		h   *Handoff
		err error
	}
	done := make(chan result, 1)
	go func() {
		h, err := c.Resume(ctx, after)
		done <- result{h, err}
	}()
	attach := readFrame(t, srv)
	if attach.Type != coord.TypeAttach {
		t.Fatalf("first frame = %+v, want attach", attach)
	}
	sendFrames(t, srv, coord.Frame{Type: coord.TypeResponse, ID: attach.ID, InstanceID: "inst-a"}, posEvent("inst-a", 4), posEvent("inst-a", 5), posEvent("inst-a", 6))
	f, cmd := readCommand(t, srv)
	var args struct {
		After coord.Position `json:"after"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		t.Fatal(err)
	}
	if cmd.Type != cmdReplayEvents || args.After != after {
		t.Fatalf("replay request = %s after %+v", cmd.Type, args.After)
	}
	// More remains, but staged live events already continue from 4, so no
	// second page is requested.
	sendWorkerResult(t, srv, f, ReplayPage{Events: []coord.Frame{posEvent("inst-a", 3), posEvent("inst-a", 4)}, More: true})

	r := <-done
	if r.err != nil {
		t.Fatalf("Resume: %v", r.err)
	}
	if got := offsets(r.h.Events); fmt.Sprint(got) != "[3 4 5 6]" {
		t.Fatalf("handoff events = %v, want [3 4 5 6]", got)
	}
	if r.h.Snapshot != nil || r.h.Cursor.Position().Offset != 6 || stageCount(c) != 0 {
		t.Fatalf("handoff = %+v, cursor %+v", r.h, r.h.Cursor.Position())
	}
}

func TestResumeExposesResync(t *testing.T) {
	after := coord.Position{InstanceID: "inst-a", Epoch: testEpoch, Offset: 2}
	otherEpoch := posEvent("inst-a", 3)
	otherEpoch.Position.Epoch = "epoch-2"
	tests := map[string]struct {
		respond func(t *testing.T, srv *coord.Link, f coord.Frame)
		cause   error
	}{
		"expired": {func(t *testing.T, srv *coord.Link, f coord.Frame) {
			sendWorkerReply(t, srv, f, map[string]any{"ok": false, "error": map[string]any{"code": coord.CodeResyncRequired, "message": "event position missing"}})
		}, nil},
		"epoch": {func(t *testing.T, srv *coord.Link, f coord.Frame) {
			sendWorkerResult(t, srv, f, ReplayPage{Events: []coord.Frame{otherEpoch}})
		}, ErrStaleCursor},
		"gap": {func(t *testing.T, srv *coord.Link, f coord.Frame) {
			sendWorkerResult(t, srv, f, ReplayPage{Events: []coord.Frame{posEvent("inst-a", 4)}})
		}, ErrEventGap},
		"target": {func(t *testing.T, srv *coord.Link, f coord.Frame) {
			sendWorkerResult(t, srv, f, ReplayPage{Events: []coord.Frame{posEvent("inst-b", 3)}})
		}, ErrEventTarget},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
			ctx := testContext(t)
			done := make(chan error, 1)
			go func() {
				_, err := c.Resume(ctx, after)
				done <- err
			}()
			attach := readFrame(t, srv)
			sendFrames(t, srv, coord.Frame{Type: coord.TypeResponse, ID: attach.ID, InstanceID: "inst-a"})
			f, _ := readCommand(t, srv)
			tc.respond(t, srv, f)
			err := <-done
			resync, ok := errors.AsType[*ResyncError](err)
			if !ok || !errors.Is(err, ErrResyncRequired) || resync.Cursor != after {
				t.Fatalf("error = %v, want resync at %+v", err, after)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("error = %v, want %v", err, tc.cause)
			}
			if tc.cause == nil {
				if perr, ok := errors.AsType[*coord.Error](err); !ok || perr.Code != coord.CodeResyncRequired {
					t.Fatalf("error = %v, want worker resync_required", err)
				}
			}
			if stageCount(c) != 0 {
				t.Fatal("stage left behind")
			}
		})
	}
}

func TestReconstructionRejectsConcurrentStage(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)
	done := make(chan error, 1)
	go func() {
		_, err := c.AttachSnapshot(ctx, "inst-a")
		done <- err
	}()
	attach := readFrame(t, srv)
	if _, err := c.Resume(ctx, coord.Position{InstanceID: "inst-a", Epoch: testEpoch}); !errors.Is(err, ErrReconstructionBusy) {
		t.Fatalf("concurrent resume error = %v", err)
	}
	sendFrames(t, srv, coord.Frame{Type: coord.TypeResponse, ID: attach.ID, Error: &coord.Error{Code: coord.CodeNotFound}})
	if err := <-done; err == nil {
		t.Fatal("attach failure ignored")
	}
	if stageCount(c) != 0 {
		t.Fatal("stage left behind")
	}
}

func TestHistoryPage(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)
	type result struct {
		h   *History
		err error
	}
	done := make(chan result, 1)
	go func() {
		h, err := c.History(ctx, "inst-a", "s", "e5")
		done <- result{h, err}
	}()
	f, cmd := readCommand(t, srv)
	var args map[string]string
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		t.Fatal(err)
	}
	if cmd.Type != cmdGetHistory || args["session_id"] != "s" || args["before"] != "e5" {
		t.Fatalf("history request = %s %v", cmd.Type, args)
	}
	sendWorkerResult(t, srv, f, json.RawMessage(`{"entries":[{"entry_id":"e4","message":{"role":"assistant"}}],"before":""}`))
	r := <-done
	if r.err != nil || len(r.h.Entries) != 1 || r.h.Entries[0].EntryID != "e4" || r.h.Before != "" {
		t.Fatalf("history = %+v, %v", r.h, r.err)
	}
	if _, err := c.History(ctx, "inst-a", "", ""); err == nil {
		t.Fatal("history without session id was sent")
	}
}

func TestEntryContentRejectsMismatchedRevision(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)
	request := EntryContentRequest{SessionID: "s", EntryID: "e", Offset: 3, Revision: "first"}
	done := make(chan error, 1)
	go func() {
		_, err := c.EntryContent(ctx, "inst-a", request)
		done <- err
	}()
	f, cmd := readCommand(t, srv)
	var args EntryContentRequest
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		t.Fatal(err)
	}
	if cmd.Type != cmdEntryContent || f.InstanceID != "inst-a" || args != request {
		t.Fatalf("unexpected content request: %s %s %+v", f.InstanceID, cmd.Type, args)
	}
	sendWorkerResult(t, srv, f, EntryContentChunk{Revision: "second", Data: []byte("tail")})
	if err := <-done; !errors.Is(err, coord.ErrInvalidFrame) {
		t.Fatalf("EntryContent error = %v, want invalid frame", err)
	}
}

func TestAttachSnapshotRetriesTransientWorkerReads(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, coordinatedEventBuffer)
	ctx := testContext(t)
	done := make(chan error, 1)
	go func() {
		_, err := c.AttachSnapshot(ctx, "inst-a")
		done <- err
	}()
	attach := readFrame(t, srv)
	sendFrames(t, srv, coord.Frame{Type: coord.TypeResponse, ID: attach.ID})
	for _, code := range []string{coord.CodeBusy, coord.CodeResyncRequired} {
		f, cmd := readCommand(t, srv)
		if cmd.Type != cmdGetSnapshot || f.RequestID != "" {
			t.Fatalf("unexpected reconstruction command: %+v, %+v", f, cmd)
		}
		sendWorkerReply(t, srv, f, map[string]any{"ok": false, "error": map[string]string{"code": code, "message": "try again"}})
	}
	f, _ := readCommand(t, srv)
	sendWorkerResult(t, srv, f, json.RawMessage(testSnapshot))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStartSavedSessionReusesConnectedOwner(t *testing.T) {
	path, _ := runCoordinator(t)
	ctx := testContext(t)
	owner, err := NewCoordinatedClient(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	})
	joiner, err := NewCoordinatedClient(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := joiner.Close(); err != nil {
			t.Error(err)
		}
	})
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.NewString()
	started, err := owner.StartSavedSession(ctx, cwd, sessionID)
	if err != nil {
		t.Fatalf("start saved session: %v", err)
	}
	got, err := joiner.StartSavedSession(ctx, cwd, sessionID)
	if err != nil {
		t.Fatalf("resume with connected owner: %v", err)
	}
	if got.ID != started.ID || got.SessionID != sessionID {
		t.Fatalf("got %+v, want original worker %+v", got, started)
	}
	if err := joiner.Attach(ctx, got.ID); err != nil {
		t.Fatalf("attach to existing owner: %v", err)
	}
	list, err := joiner.ListInstances(ctx)
	if err != nil || len(list) != 1 || list[0].ID != started.ID {
		t.Fatalf("registry after resume: %+v, error %v", list, err)
	}
	_, err = joiner.StartSavedSession(ctx, t.TempDir(), sessionID)
	if conflict, ok := errors.AsType[*coord.Error](err); !ok || conflict.Code != coord.CodeConflict {
		t.Fatalf("resume from another project: got %v, want conflict", err)
	}
}

func TestStartSavedSessionWaitsForBusyOwner(t *testing.T) {
	c, srv, _ := fakeCoordinator(t, 16)
	c.welcome.Operations = []string{coord.OperationSavedSessionLaunch}
	ctx := testContext(t)
	id := uuid.NewString()
	inst := coord.Instance{ID: "owner", SessionID: id, CWD: "/work", LinkState: coord.LinkConnected}
	type result struct {
		inst *coord.Instance
		err  error
	}
	done := make(chan result, 1)
	go func() {
		got, err := c.StartSavedSession(ctx, inst.CWD, id)
		done <- result{got, err}
	}()
	first := readFrame(t, srv)
	sendFrames(t, srv, coord.Frame{Type: coord.TypeResponse, ID: first.ID, Error: &coord.Error{Code: coord.CodeBusy}})
	retry := readFrame(t, srv)
	if retry.Type != coord.TypeStartInstance || retry.SessionID != id || retry.CWD != inst.CWD {
		t.Fatalf("wrong retry: %+v", retry)
	}
	sendFrames(t, srv, coord.Frame{Type: coord.TypeResponse, ID: retry.ID, Instance: &inst})
	if got := <-done; got.err != nil || got.inst == nil || got.inst.ID != inst.ID {
		t.Fatalf("resume after busy: instance=%+v error=%v", got.inst, got.err)
	}
}

func TestSavedSessionOperationsRequireCoordinatorSupport(t *testing.T) {
	c, _, _ := fakeCoordinator(t, 16)
	if _, err := c.StartSavedSession(t.Context(), "/work", uuid.NewString()); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("old coordinator launch: %v", err)
	}
	if _, err := c.SessionDirectory(t.Context(), "/work"); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("old coordinator lookup: %v", err)
	}
}
