package coord

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	launchTimeout  = 20 * time.Second
	maxClients     = 64
	maxWorkers     = 64
	maxLaunches    = 8
	maxConnections = maxClients + maxWorkers + 16
)

var shutdownCommand = json.RawMessage(`{"type":"shutdown"}`)

// LaunchRequest describes a worker the coordinator expects to register.
// The worker must Dial with role worker, Token, and an Instance whose ID is
// InstanceID.
type LaunchRequest struct {
	InstanceID string
	Token      string
	CWD        string
	Ready      <-chan struct{}
}

// LaunchFunc starts a detached worker for a request. It may wait for Ready
// or for ctx. The coordinator cancels ctx only after it has either accepted
// the worker's registration (Ready is closed first) or revoked the launch
// token so the worker can no longer register; a launcher may therefore kill
// its worker when ctx is done and Ready is still open. A registered worker's
// lifetime must not depend on ctx. Returned errors are reported to the
// requesting client unless the worker registered.
type LaunchFunc func(context.Context, LaunchRequest) error

// resolveProjectDir validates a start_instance directory. It runs without
// the coordinator lock because filesystem checks can block indefinitely.
var resolveProjectDir = func(dir string) (string, error) {
	cwd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("project directory is unavailable: %w", err)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("project directory must exist: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("project directory must be a directory")
	}
	return filepath.Clean(cwd), nil
}

type coordinator struct {
	ctx           context.Context
	launch        LaunchFunc
	launchTimeout time.Duration
	generation    string
	resolveDir    func(string) (string, error)
	conns         chan struct{}
	wg            sync.WaitGroup

	mu       sync.Mutex
	clients  map[string]*client
	workers  map[string]*worker
	launches map[string]*pendingLaunch
	// validating counts starts whose directory check holds a launch slot.
	validating int
	stopping   int
}

type client struct {
	id       string
	identity string
	link     *Link
	pending  map[string]pendingRef
	subs     map[string]struct{}
	watching bool
}

// pendingRef locates an in-flight client request. An empty routeID marks a
// start_instance request awaiting registration.
type pendingRef struct {
	instanceID string
	routeID    string
}

type route struct {
	client    *client
	requestID string
}

type worker struct {
	instance Instance
	link     *Link
	routes   map[string]route
}

type pendingLaunch struct {
	token      string
	cwd        string
	ready      chan Instance
	registered chan struct{}
}

// Run owns the coordinator endpoint at path until ctx is canceled. It returns
// an error wrapping ErrAlreadyRunning when another process owns path.
// Cancellation disconnects clients and workers but never stops workers.
func Run(ctx context.Context, path string, launch LaunchFunc) error {
	return run(ctx, path, launch, launchTimeout)
}

func run(ctx context.Context, path string, launch LaunchFunc, timeout time.Duration) (err error) {
	if launch == nil {
		return errors.New("coordinator: launch function required")
	}
	ep, err := listen(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := ep.close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	ctx, cancel := context.WithCancel(ctx)
	co := &coordinator{
		ctx:           ctx,
		launch:        launch,
		launchTimeout: timeout,
		generation:    uuid.NewString(),
		resolveDir:    resolveProjectDir,
		conns:         make(chan struct{}, maxConnections),
		clients:       make(map[string]*client),
		workers:       make(map[string]*worker),
		launches:      make(map[string]*pendingLaunch),
	}
	defer co.wg.Wait()
	defer cancel()
	stop := context.AfterFunc(ctx, ep.closeListener)
	defer stop()

	for {
		conn, err := ep.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept coordinator connection: %w", err)
		}
		select {
		case co.conns <- struct{}{}:
		default:
			if closeErr := conn.Close(); closeErr != nil {
				logf("close rejected connection: %v", closeErr)
			}
			continue
		}
		co.wg.Go(func() {
			defer func() { <-co.conns }()
			co.serve(conn)
		})
	}
}

func (co *coordinator) serve(conn net.Conn) {
	link := NewLink(conn)
	stop := context.AfterFunc(co.ctx, func() { link.fail(co.ctx.Err()) })
	defer func() {
		stop()
		if err := link.Close(); err != nil {
			logf("%v", err)
		}
	}()
	if err := conn.SetReadDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		logf("set handshake deadline: %v", err)
		return
	}
	line, err := link.readLine()
	if err != nil {
		return
	}
	hello, err := decodeFrame(line)
	if err != nil {
		rejectLegacy(link, line)
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		logf("clear handshake deadline: %v", err)
		return
	}
	if rejection := checkHello(hello); rejection != nil {
		reject(link, rejection)
		return
	}
	if hello.Role == RoleWorker {
		co.serveWorker(link, hello)
		return
	}
	co.serveClient(link, hello.ClientID)
}

func checkHello(f Frame) *Error {
	switch {
	case f.Type != TypeHello:
		return &Error{Code: CodeInvalidFrame, Message: "expected hello"}
	case f.Version != Version:
		return &Error{Code: CodeUnsupportedVersion, Message: fmt.Sprintf("version %d unsupported; want %d", f.Version, Version)}
	case f.Role != RoleClient && f.Role != RoleWorker:
		return &Error{Code: CodeInvalidRole, Message: "role must be client or worker"}
	}
	return nil
}

func reject(link *Link, e *Error) {
	send(link, Frame{Type: TypeResponse, Version: Version, Error: e})
}

// rejectLegacy queues a compatible error for a legacy RPC command so its
// client fails with guidance instead of a silent disconnect. The caller's
// deferred Close flushes it.
func rejectLegacy(link *Link, line []byte) {
	reply, err := legacyRejection(line)
	if err != nil {
		logf("%v", err)
		return
	}
	if reply == nil {
		return
	}
	if err := link.enqueue(reply); err != nil {
		logf("reply to legacy client: %v", err)
	}
}

func (co *coordinator) serveClient(link *Link, identity string) {
	if identity == "" {
		identity = uuid.NewString()
	}
	co.mu.Lock()
	if len(co.clients) >= maxClients {
		co.mu.Unlock()
		reject(link, &Error{Code: CodeLimitExceeded, Message: "too many clients"})
		return
	}
	c := &client{
		id:       uuid.NewString(),
		identity: identity,
		link:     link,
		pending:  make(map[string]pendingRef),
		subs:     make(map[string]struct{}),
	}
	co.clients[c.id] = c
	send(link, Frame{Type: TypeWelcome, Version: Version, Role: RoleClient, Generation: co.generation, ClientID: c.identity, Operations: []string{TypeListInstances, TypeWatchInstances, TypeUnwatchInstances, TypeStartInstance, TypeAttach, TypeDetach, TypeCommand, TypeStopInstance}, Limits: protocolLimits()})
	co.mu.Unlock()
	defer co.dropClient(c)

	for {
		f, err := link.Read()
		if err != nil {
			return
		}
		co.handleClient(c, f)
	}
}

func (co *coordinator) serveWorker(link *Link, hello Frame) {
	w, rejection := co.register(link, hello)
	if rejection != nil {
		reject(link, rejection)
		return
	}
	defer co.dropWorker(w)

	for {
		f, err := link.Read()
		if err != nil {
			return
		}
		co.handleWorker(w, f)
	}
}

// register accepts a worker only against the token issued for its launch.
// Tokens are single use, so a second claimant for the same instance fails.
func (co *coordinator) register(link *Link, hello Frame) (*worker, *Error) {
	co.mu.Lock()
	defer co.mu.Unlock()
	inst := hello.Instance
	if inst == nil || inst.ID == "" || hello.Token == "" {
		return nil, &Error{Code: CodeUnauthorized, Message: "worker hello requires instance and token"}
	}
	pl := co.launches[inst.ID]
	if pl == nil || subtle.ConstantTimeCompare([]byte(pl.token), []byte(hello.Token)) != 1 {
		return nil, &Error{Code: CodeUnauthorized, Message: "unknown instance or token"}
	}
	if co.workers[inst.ID] != nil {
		return nil, &Error{Code: CodeConflict, Message: "instance already registered"}
	}
	if inst.NodeID == "" || inst.SessionID == "" || inst.CWD != pl.cwd {
		return nil, &Error{Code: CodeConflict, Message: "worker readiness metadata does not match launch"}
	}
	for _, existing := range co.workers {
		if existing.instance.NodeID == inst.NodeID && existing.instance.SessionID == inst.SessionID {
			return nil, &Error{Code: CodeConflict, Message: "session already owned by a worker"}
		}
	}
	delete(co.launches, inst.ID)
	if pl.registered != nil {
		close(pl.registered)
	}

	record := *inst
	if record.CWD == "" {
		record.CWD = pl.cwd
	}
	if record.State == "" {
		record.State = StateReady
	}
	w := &worker{instance: record, link: link, routes: make(map[string]route)}
	co.workers[record.ID] = w
	send(link, Frame{Type: TypeWelcome, Version: Version, Role: RoleWorker, Generation: co.generation, InstanceID: record.ID, Instance: &record, Operations: []string{TypeCommand, TypeRegister, TypeEvent, TypeResponse}, Limits: protocolLimits()})
	co.broadcast(Frame{Type: TypeInstanceUpdate, InstanceID: record.ID, SessionID: record.SessionID, Instance: &record})
	pl.ready <- record
	return w, nil
}

func (co *coordinator) dropClient(c *client) {
	co.mu.Lock()
	defer co.mu.Unlock()
	delete(co.clients, c.id)
	for _, ref := range c.pending {
		if w := co.workers[ref.instanceID]; w != nil && ref.routeID != "" {
			delete(w.routes, ref.routeID)
		}
	}
	clear(c.pending)
	clear(c.subs)
}

func (co *coordinator) dropWorker(w *worker) {
	co.mu.Lock()
	defer co.mu.Unlock()
	id := w.instance.ID
	if co.workers[id] != w {
		return
	}
	delete(co.workers, id)
	unavailable := &Error{Code: CodeUnavailable, Message: "worker link lost; command outcome unknown"}
	for _, r := range w.routes {
		delete(r.client.pending, r.requestID)
		send(r.client.link, Frame{Type: TypeResponse, ID: r.requestID, InstanceID: id, Error: unavailable})
	}
	clear(w.routes)
	message := "worker link closed; the worker may still be running"
	if errors.Is(w.link.Err(), ErrSlowConsumer) {
		message = "worker link closed: outbound queue full; the worker may still be running"
	}
	lost := Frame{Type: TypeInstanceUpdate, InstanceID: id, SessionID: w.instance.SessionID, Error: &Error{
		Code: CodeWorkerLinkLost, Message: message,
	}}
	co.broadcast(lost)
	lost.Type = TypeEvent
	co.broadcast(lost)
	for _, c := range co.clients {
		delete(c.subs, id)
	}
}

func (co *coordinator) handleClient(c *client, f Frame) {
	co.mu.Lock()
	defer co.mu.Unlock()
	if f.ID == "" {
		replyError(c, f, CodeInvalidFrame, "id required")
		return
	}
	if _, duplicate := c.pending[f.ID]; duplicate {
		replyError(c, f, CodeDuplicateID, "request id already in flight")
		return
	}
	switch f.Type {
	case TypeListInstances:
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, Instances: co.instances()})
	case TypeWatchInstances:
		c.watching = true
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, Instances: co.instances()})
	case TypeUnwatchInstances:
		c.watching = false
		send(c.link, Frame{Type: TypeResponse, ID: f.ID})
	case TypeStartInstance:
		co.startInstance(c, f)
	case TypeAttach:
		w := co.workers[f.InstanceID]
		if w == nil {
			replyError(c, f, CodeNotFound, "instance not found")
			return
		}
		c.subs[f.InstanceID] = struct{}{}
		inst := w.instance
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, InstanceID: inst.ID, SessionID: inst.SessionID, Instance: &inst})
	case TypeDetach:
		if f.InstanceID == "" {
			replyError(c, f, CodeInvalidFrame, "instance_id required")
			return
		}
		delete(c.subs, f.InstanceID)
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, InstanceID: f.InstanceID})
	case TypeCommand:
		if !isObject(f.Command) {
			replyError(c, f, CodeInvalidFrame, "command object required")
			return
		}
		var nested struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(f.Command, &nested); err != nil || nested.Type == "" || nested.Type == "shutdown" {
			replyError(c, f, CodeInvalidFrame, "command type required; use stop_instance for shutdown")
			return
		}
		co.forward(c, f, f.Command)
	case TypeStopInstance:
		co.forward(c, f, shutdownCommand)
	default:
		replyError(c, f, CodeInvalidFrame, "unexpected frame type")
	}
}

func (co *coordinator) startInstance(c *client, f Frame) {
	if !filepath.IsAbs(f.CWD) {
		replyError(c, f, CodeInvalidFrame, "absolute cwd required")
		return
	}
	if e := admit(c, f.ID); e != nil {
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, Error: e})
		return
	}
	starting := len(co.launches) + co.validating + co.stopping
	if starting >= maxLaunches || len(co.workers)+starting >= maxWorkers {
		replyError(c, f, CodeLimitExceeded, "too many instances")
		return
	}
	id := uuid.NewString()
	co.validating++
	c.pending[f.ID] = pendingRef{instanceID: id}
	co.wg.Go(func() { co.runLaunch(c, f.ID, id, f.CWD) })
}

// projectDir validates dir off the coordinator lock. The caller holds a
// launch slot until this returns, which bounds blocked checks to
// maxLaunches; shutdown abandons a blocked check rather than waiting.
func (co *coordinator) projectDir(dir string) (string, error) {
	type result struct {
		cwd string
		err error
	}
	done := make(chan result, 1)
	resolve := co.resolveDir
	go func() {
		cwd, err := resolve(dir)
		done <- result{cwd, err}
	}()
	select {
	case r := <-done:
		return r.cwd, r.err
	case <-co.ctx.Done():
		return "", co.ctx.Err()
	}
}

func (co *coordinator) runLaunch(c *client, requestID, id, dir string) {
	ref := pendingRef{instanceID: id}
	cwd, err := co.projectDir(dir)

	co.mu.Lock()
	co.validating--
	if c.pending[requestID] != ref {
		co.mu.Unlock()
		return
	}
	if co.ctx.Err() != nil {
		delete(c.pending, requestID)
		replyError(c, Frame{ID: requestID}, CodeUnavailable, "coordinator is shutting down")
		co.mu.Unlock()
		return
	}
	if err != nil {
		delete(c.pending, requestID)
		replyError(c, Frame{ID: requestID}, CodeInvalidFrame, boundedText(err.Error()))
		co.mu.Unlock()
		return
	}
	pl := &pendingLaunch{token: rand.Text(), cwd: cwd, ready: make(chan Instance, 1), registered: make(chan struct{})}
	co.launches[id] = pl
	co.mu.Unlock()

	inst, err := co.superviseLaunch(id, pl)

	co.mu.Lock()
	defer co.mu.Unlock()
	if c.pending[requestID] != ref {
		return
	}
	delete(c.pending, requestID)
	if err != nil {
		msg := boundedText(strings.ReplaceAll(err.Error(), pl.token, "[redacted]"))
		send(c.link, Frame{Type: TypeResponse, ID: requestID, InstanceID: id, Error: &Error{Code: CodeLaunchFailed, Message: msg}})
		return
	}
	send(c.link, Frame{Type: TypeResponse, ID: requestID, InstanceID: id, SessionID: inst.SessionID, Instance: &inst})
}

// superviseLaunch runs the launcher and decides its outcome under co.mu.
// A timeout or shutdown first revokes the launch so register can no longer
// accept the token, and only then cancels the launcher; a worker that
// registered first is reported as started and its launch is never canceled
// before Ready closes.
func (co *coordinator) superviseLaunch(id string, pl *pendingLaunch) (Instance, error) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(co.ctx))
	defer cancel()
	launched := make(chan error, 1)
	co.wg.Go(func() {
		launched <- co.launch(ctx, LaunchRequest{InstanceID: id, Token: pl.token, CWD: pl.cwd, Ready: pl.registered})
	})
	timer := time.NewTimer(co.launchTimeout)
	defer timer.Stop()

	running := launched
	var cause error
	for cause == nil {
		select {
		case inst := <-pl.ready:
			return inst, nil
		case err := <-running:
			running = nil
			if err != nil {
				cause = fmt.Errorf("launch worker: %w", err)
			}
		case <-timer.C:
			cause = errors.New("worker did not register before deadline")
		case <-co.ctx.Done():
			cause = fmt.Errorf("launch worker: %w", co.ctx.Err())
		}
	}

	co.mu.Lock()
	if co.launches[id] != pl {
		co.mu.Unlock()
		// register sends on ready before releasing co.mu.
		return <-pl.ready, nil
	}
	delete(co.launches, id)
	co.stopping++
	co.mu.Unlock()
	cancel()
	if running != nil {
		if err := <-running; err != nil {
			cause = errors.Join(cause, fmt.Errorf("stop launch: %w", err))
		}
	}
	co.mu.Lock()
	co.stopping--
	co.mu.Unlock()
	return Instance{}, cause
}

// forward routes a targeted command under a fresh route ID so client request
// IDs never reach workers and cannot collide across clients.
func (co *coordinator) forward(c *client, f Frame, cmd json.RawMessage) {
	if f.InstanceID == "" {
		replyError(c, f, CodeInvalidFrame, "instance_id required")
		return
	}
	w := co.workers[f.InstanceID]
	if w == nil {
		replyError(c, f, CodeNotFound, "instance not found")
		return
	}
	if e := admit(c, f.ID); e != nil {
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, InstanceID: f.InstanceID, Error: e})
		return
	}
	routeID := uuid.NewString()
	err := w.link.Send(Frame{
		Type:       TypeCommand,
		ID:         routeID,
		InstanceID: w.instance.ID,
		SessionID:  w.instance.SessionID,
		ClientID:   c.identity,
		Command:    cmd,
	})
	if err != nil {
		code := CodeUnavailable
		if errors.Is(err, ErrFrameTooLarge) {
			code = CodeLimitExceeded
		}
		replyError(c, f, code, "command not delivered")
		return
	}
	w.routes[routeID] = route{client: c, requestID: f.ID}
	c.pending[f.ID] = pendingRef{instanceID: w.instance.ID, routeID: routeID}
}

func (co *coordinator) handleWorker(w *worker, f Frame) {
	co.mu.Lock()
	defer co.mu.Unlock()
	id := w.instance.ID
	switch f.Type {
	case TypeResponse:
		r, ok := w.routes[f.ID]
		if !ok {
			return
		}
		delete(w.routes, f.ID)
		delete(r.client.pending, r.requestID)
		send(r.client.link, Frame{Type: TypeResponse, ID: r.requestID, InstanceID: id, Result: f.Result, Error: f.Error})
	case TypeEvent:
		if !isObject(f.Event) {
			send(w.link, Frame{Type: TypeResponse, ID: f.ID, Error: &Error{Code: CodeInvalidFrame, Message: "event object required"}})
			return
		}
		// A worker labels events from a newly switched session before its
		// register update arrives, so its own session ID wins.
		session := cmp.Or(f.SessionID, w.instance.SessionID)
		co.broadcast(Frame{Type: TypeEvent, InstanceID: id, SessionID: session, Event: f.Event})
	case TypeRegister:
		if f.Instance == nil || f.Instance.ID != id {
			send(w.link, Frame{Type: TypeResponse, ID: f.ID, Error: &Error{Code: CodeConflict, Message: "instance mismatch"}})
			return
		}
		w.instance.SessionID = f.Instance.SessionID
		if f.Instance.State != "" {
			w.instance.State = f.Instance.State
		}
		w.instance.PendingInputs = f.Instance.PendingInputs
		inst := w.instance
		if f.ID != "" {
			send(w.link, Frame{Type: TypeResponse, ID: f.ID, InstanceID: id, Instance: &inst})
		}
		co.broadcast(Frame{Type: TypeInstanceUpdate, InstanceID: id, SessionID: inst.SessionID, Instance: &inst})
		co.broadcast(Frame{Type: TypeEvent, InstanceID: id, SessionID: inst.SessionID, Instance: &inst})
	default:
		send(w.link, Frame{Type: TypeResponse, ID: f.ID, Error: &Error{Code: CodeInvalidFrame, Message: "unexpected frame type"}})
	}
}

// broadcast runs under co.mu so replies and subscription changes are ordered
// with the events they expose. Encode at most once for all recipients.
func (co *coordinator) broadcast(f Frame) {
	var data []byte
	var err error
	for _, c := range co.clients {
		_, attached := c.subs[f.InstanceID]
		if (f.Type == TypeInstanceUpdate && !c.watching) || (f.Type != TypeInstanceUpdate && !attached) {
			continue
		}
		if data == nil && err == nil {
			data, err = encodeOrLimit(f)
		}
		queueFrame(c.link, data, err)
	}
}

func (co *coordinator) instances() []Instance {
	list := make([]Instance, 0, len(co.workers))
	for _, w := range co.workers {
		list = append(list, w.instance)
	}
	slices.SortFunc(list, func(a, b Instance) int { return cmp.Compare(a.ID, b.ID) })
	return list
}

func admit(c *client, requestID string) *Error {
	if _, dup := c.pending[requestID]; dup {
		return &Error{Code: CodeDuplicateID, Message: "request id already in flight"}
	}
	if len(c.pending) >= MaxPending {
		return &Error{Code: CodeTooManyPending, Message: fmt.Sprintf("at most %d requests may be in flight", MaxPending)}
	}
	return nil
}

func replyError(c *client, f Frame, code, msg string) {
	send(c.link, Frame{Type: TypeResponse, ID: f.ID, InstanceID: f.InstanceID, Error: &Error{Code: code, Message: msg}})
}

// send queues f on link. A frame that outgrew MaxFrameBytes in transit, such
// as a worker reply after route-ID rewriting, is replaced by a limit error
// carrying the same routing fields, so a healthy peer stays connected and
// learns what it missed. Any other failure, including a slow consumer, closes
// link so the peer must resynchronize instead of missing a frame silently.
func send(link *Link, f Frame) {
	data, err := encodeOrLimit(f)
	queueFrame(link, data, err)
}

func encodeOrLimit(f Frame) ([]byte, error) {
	data, err := encodeFrame(f)
	if !errors.Is(err, ErrFrameTooLarge) {
		return data, err
	}
	return encodeFrame(Frame{
		Type:       f.Type,
		ID:         f.ID,
		InstanceID: f.InstanceID,
		SessionID:  f.SessionID,
		Error:      &Error{Code: CodeLimitExceeded, Message: f.Type + " exceeds frame limit"},
	})
}

func queueFrame(link *Link, data []byte, err error) {
	if err == nil {
		err = link.enqueue(data)
	}
	if err != nil {
		link.fail(err)
	}
}

// boundedText trims s to valid UTF-8 within maxTextBytes so it survives the
// peer's frame validation.
func boundedText(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) > maxTextBytes {
		s = strings.ToValidUTF8(s[:maxTextBytes], "")
	}
	return s
}

func logf(format string, args ...any) {
	if _, err := fmt.Fprintf(os.Stderr, "tau coordinator: "+format+"\n", args...); err != nil {
		return
	}
}
