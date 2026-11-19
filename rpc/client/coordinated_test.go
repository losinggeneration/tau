package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/losinggeneration/tau/rpc/coord"
)

// fakeCoordinator accepts one client on a real Unix socket, completes the
// handshake, and hands the server side of the link to the test.
func fakeCoordinator(t *testing.T, eventBuffer int) (*CoordinatedClient, *coord.Link, coord.Frame) {
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
			err = link.Send(coord.Frame{Type: coord.TypeWelcome, Version: coord.Version, Role: coord.RoleClient, Generation: "gen-1", ClientID: "assigned"})
		}
		ready <- accepted{link: link, hello: hello, err: err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := dialCoordinated(ctx, path, eventBuffer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	srv := <-ready
	if srv.err != nil {
		t.Fatalf("server handshake: %v", srv.err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
		if err := srv.link.Close(); err != nil {
			t.Errorf("close server link: %v", err)
		}
	})
	return c, srv.link, srv.hello
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
		Instance: &coord.Instance{ID: req.InstanceID, NodeID: "node", CWD: req.CWD, SessionID: "sess-" + req.InstanceID},
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

func TestLegacyUnixClientGetsCoordinatorModeDiagnostic(t *testing.T) {
	path, _ := runCoordinator(t)
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	c, err := NewUnixClient(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	r, err := c.GetState(ctx)
	if err != nil {
		t.Fatalf("expected an explicit mode diagnostic: %v", err)
	}
	if r.OK || r.Error == nil || r.Error.Code != coord.CodeLegacyProtocol || !strings.Contains(r.Error.Message, "tau daemon --legacy") {
		t.Fatalf("wrong mode diagnostic: %+v, error: %+v", r, r.Error)
	}
}
