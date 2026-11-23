package coord

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	sessionpkg "github.com/losinggeneration/tau/agent/session"
)

const (
	launchTimeout     = 20 * time.Second
	maxClients        = 64
	maxWorkers        = 64
	maxLaunches       = 8
	maxDirectoryReads = 8
	maxConnections    = maxClients + maxWorkers + 16
	heartbeatInterval = 2 * time.Second
	heartbeatTimeout  = 8 * time.Second
)

var shutdownCommand = json.RawMessage(`{"type":"shutdown"}`)

// LaunchRequest describes a worker the coordinator expects to register.
// The worker must Dial with role worker, Token, and an Instance whose ID is
// InstanceID. LaunchRequestID, when set, is the requesting client's
// correlation ID; the launcher passes it to the worker, whose hello may echo
// it, so the instance can later be matched to the start request.
type LaunchRequest struct {
	InstanceID      string
	LaunchRequestID string
	Token           string
	CWD             string
	SessionID       string
	Ready           <-chan struct{}
	// Lifetime, when set, must be inherited by the child and held open
	// until process exit. The launcher must not unlock or close this copy.
	Lifetime *os.File
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

// SessionDirectoryFunc resolves the daemon's saved-session storage for a project.
type SessionDirectoryFunc func(context.Context, string) (string, error)

// SessionLocatorFunc finds a saved session's canonical ID and project from an
// ID or unique prefix. hint is the client's directory, which may not exist on
// this machine; it only disambiguates sessions that never recorded a project.
type SessionLocatorFunc func(ctx context.Context, ref, hint string) (id, project string, err error)

type coordinator struct {
	ctx            context.Context
	launch         LaunchFunc
	launchTimeout  time.Duration
	generation     string
	receipts       *receiptStore
	resolveDir     func(string) (string, error)
	sessionDir     SessionDirectoryFunc
	locateSession  SessionLocatorFunc
	workerBuild    func() string
	directoryReads chan struct{}
	conns          chan struct{}
	wg             sync.WaitGroup

	buildMu          sync.Mutex
	mu               sync.Mutex
	launchBuild      string
	clients          map[string]*client
	workers          map[string]*worker
	launches         map[string]*pendingLaunch
	startingIDs      map[string]struct{}
	startingSessions map[string]struct{}
	// validating counts starts whose directory check holds a launch slot.
	validating int
	stopping   int
}

type client struct {
	id              string
	identity        string
	link            *Link
	pending         map[string]pendingRef
	subs            map[string]struct{}
	watching        bool
	instancePrompts bool
	instanceBuild   bool
}

// pendingRef locates an in-flight client request. An empty routeID marks a
// start_instance request awaiting registration.
type pendingRef struct {
	instanceID string
	routeID    string
}

type route struct {
	client        *client
	requestID     string
	correlationID string
}

type worker struct {
	legacyRestarting bool
	legacyChangeID   string // retained until the worker replies, even if the caller disconnects
	instance         Instance
	firstPrompt      string
	buildID          string
	restart          bool
	link             *Link
	routes           map[string]route
	tokenHash        [sha256.Size]byte
	epoch            string
	receiptID        string
	lastPong         time.Time
}

type pendingLaunch struct {
	token      string
	cwd        string
	sessionID  string
	requestID  string
	ready      chan Instance
	registered chan struct{}
	lifetime   *os.File
}

// Run owns the coordinator endpoint at path until ctx is canceled. It returns
// an error wrapping ErrAlreadyRunning when another process owns path.
// Cancellation disconnects clients and workers but never stops workers.
func Run(ctx context.Context, path string, launch LaunchFunc) error {
	return run(ctx, path, launch, launchTimeout)
}

// RunRecoverable is Run with private launch receipts in a persistent directory
// dedicated to path. Its parent must already exist.
// The launcher must honor LaunchRequest.Lifetime for restart recovery. New
// correlated launches require a UUIDv7 issued in the last 24 hours (up to
// five minutes of future clock skew is allowed). Live receipts do not expire.
// The endpoint accepts at most 4096 retained receipts.
func RunRecoverable(ctx context.Context, path, receiptDir string, launch LaunchFunc, opts ...RecoverableOption) error {
	if !filepath.IsAbs(receiptDir) {
		return errors.New("recoverable coordinator requires an absolute receipt directory")
	}
	return runWithReceipts(ctx, path, launch, launchTimeout, receiptDir, opts...)
}

func run(ctx context.Context, path string, launch LaunchFunc, timeout time.Duration) error {
	return runWithReceipts(ctx, path, launch, timeout, "")
}

// RecoverableOption configures the daemon's local coordinator.
type RecoverableOption func(*coordinator)

// WithSessionDirectory enables directory lookup for local clients.
func WithSessionDirectory(fn SessionDirectoryFunc) RecoverableOption {
	return func(co *coordinator) { co.sessionDir = fn }
}

// WithSessionLocator enables resuming a session by ID from any directory.
func WithSessionLocator(fn SessionLocatorFunc) RecoverableOption {
	return func(co *coordinator) { co.locateSession = fn }
}

// WithWorkerBuild supplies the build identity of the executable used for new workers.
// The callback runs outside the registry lock and returns empty when unknown.
func WithWorkerBuild(build func() string) RecoverableOption {
	return func(co *coordinator) { co.workerBuild = build }
}

func runWithReceipts(ctx context.Context, path string, launch LaunchFunc, timeout time.Duration, receiptDir string, opts ...RecoverableOption) (err error) {
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

	var receipts *receiptStore
	if receiptDir != "" {
		receipts, err = openReceiptStore(receiptDir)
		if err != nil {
			return fmt.Errorf("initialize launch receipts: %w", err)
		}
		if _, err := receipts.prune(time.Now()); err != nil {
			return fmt.Errorf("initialize launch receipts: %w", err)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	co := &coordinator{
		ctx:              ctx,
		launch:           launch,
		launchTimeout:    timeout,
		generation:       uuid.NewString(),
		receipts:         receipts,
		resolveDir:       resolveProjectDir,
		conns:            make(chan struct{}, maxConnections),
		clients:          make(map[string]*client),
		workers:          make(map[string]*worker),
		launches:         make(map[string]*pendingLaunch),
		startingIDs:      make(map[string]struct{}),
		startingSessions: make(map[string]struct{}),
		directoryReads:   make(chan struct{}, maxDirectoryReads),
	}
	for _, opt := range opts {
		opt(co)
	}
	defer co.wg.Wait()
	defer cancel()
	co.wg.Go(co.monitorWorkers)
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
	co.serveClient(link, hello)
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

func (co *coordinator) serveClient(link *Link, hello Frame) {
	identity := hello.ClientID
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
		id:              uuid.NewString(),
		identity:        identity,
		link:            link,
		pending:         make(map[string]pendingRef),
		subs:            make(map[string]struct{}),
		instancePrompts: slices.Contains(hello.Operations, OperationInstancePrompts),
		instanceBuild:   slices.Contains(hello.Operations, OperationInstanceBuild),
	}
	co.clients[c.id] = c
	launchVersion := 4
	if co.receipts != nil {
		launchVersion = 7
	}
	ops := []string{TypeListInstances, TypeWatchInstances, TypeUnwatchInstances, TypeStartInstance, TypeAttach, TypeDetach, TypeCommand, TypeStopInstance, OperationSavedSessionLaunch}
	if co.sessionDir != nil {
		ops = append(ops, TypeSessionDirectory, OperationInstanceRestart)
	}
	if co.locateSession != nil {
		ops = append(ops, TypeLocateSession)
	}
	send(link, Frame{Type: TypeWelcome, Version: Version, Role: RoleClient, LaunchRequestVersion: launchVersion, CoordinatorTimeUnixMS: time.Now().UnixMilli(), Generation: co.generation, PID: os.Getpid(), ClientID: c.identity, Operations: ops, Limits: protocolLimits()})
	co.mu.Unlock()
	defer co.dropClient(c)

	for {
		f, err := link.Read()
		if err != nil {
			return
		}
		if f.Type == TypeSessionDirectory {
			co.lookupSessionDirectory(c, f)
			continue
		}
		if f.Type == TypeLocateSession {
			co.lookupSession(c, f)
			continue
		}
		if c.instanceBuild && (f.Type == TypeListInstances || f.Type == TypeWatchInstances) {
			co.refreshLaunchBuild()
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

// receiptKey uses the launch intent when supplied. A legacy start without
// a request ID still needs its own receipt to authenticate worker recovery.
func receiptKey(instance, request string) string {
	if request != "" {
		return "r-" + request
	}
	return "i-" + instance
}

func (co *coordinator) register(link *Link, hello Frame) (*worker, *Error) {
	inst := hello.Instance
	if inst == nil || inst.ID == "" || hello.Token == "" {
		return nil, &Error{Code: CodeUnauthorized, Message: "worker hello requires instance and token"}
	}
	buildID := inst.BuildID
	if buildID == "" && co.workerBuild != nil {
		buildID = peerBuildID(link.conn, inst.PID)
	}
	hash := sha256.Sum256([]byte(hello.Token))
	var epoch string
	if p := hello.Position; p != nil && p.InstanceID == inst.ID {
		epoch = p.Epoch
	}
	key := receiptKey(inst.ID, inst.LaunchRequestID)
	var recoverable bool
	if co.receipts != nil && ValidLaunchRequestID(cmp.Or(inst.LaunchRequestID, inst.ID)) {
		// Prevent pruning from racing the receipt lookup/exit check and registry insertion.
		co.receipts.mu.RLock()
		defer co.receipts.mu.RUnlock()
		r, err := co.receipts.read(key)
		if errors.Is(err, os.ErrNotExist) && ValidRequestID(cmp.Or(inst.LaunchRequestID, inst.ID)) {
			key = cmp.Or(inst.LaunchRequestID, inst.ID)
			r, err = co.receipts.read(key)
		}
		if err == nil && r.Generation != co.generation && r.InstanceID == inst.ID && r.CWD == inst.CWD &&
			subtle.ConstantTimeCompare(hash[:], r.TokenHash[:]) == 1 {
			if exited, err := co.receipts.exited(key); err == nil && !exited {
				recoverable = true
			}
		}
	}

	co.mu.Lock()
	defer co.mu.Unlock()
	pl, existing := co.launches[inst.ID], co.workers[inst.ID]
	var requestID string
	switch {
	case existing != nil:
		if subtle.ConstantTimeCompare(hash[:], existing.tokenHash[:]) != 1 || existing.epoch == "" || epoch != existing.epoch {
			return nil, &Error{Code: CodeUnauthorized, Message: "unknown worker identity"}
		}
		if existing.link != nil || existing.instance.LinkState == LinkExited || inst.PID != existing.instance.PID ||
			inst.NodeID != existing.instance.NodeID || inst.CWD != existing.instance.CWD || inst.LaunchRequestID != existing.instance.LaunchRequestID {
			return nil, &Error{Code: CodeConflict, Message: "instance identity conflict"}
		}
		requestID = inst.LaunchRequestID
	case pl != nil:
		if subtle.ConstantTimeCompare([]byte(pl.token), []byte(hello.Token)) != 1 {
			return nil, &Error{Code: CodeUnauthorized, Message: "unknown instance or token"}
		}
		if inst.CWD != pl.cwd || (pl.sessionID != "" && inst.SessionID != pl.sessionID) || (inst.LaunchRequestID != "" && inst.LaunchRequestID != pl.requestID) {
			return nil, &Error{Code: CodeConflict, Message: "worker readiness metadata does not match launch"}
		}
		requestID = pl.requestID
	case recoverable && epoch != "" && inst.PID > 0:
		if len(co.workers)+len(co.launches)+co.validating >= maxWorkers && !co.evictExited() {
			return nil, &Error{Code: CodeLimitExceeded, Message: "too many instances"}
		}
		requestID = inst.LaunchRequestID
	default:
		return nil, &Error{Code: CodeUnauthorized, Message: "unknown instance or token"}
	}
	if inst.NodeID == "" || inst.SessionID == "" || (co.receipts != nil && (epoch == "" || inst.PID <= 0)) {
		return nil, &Error{Code: CodeConflict, Message: "worker readiness metadata is incomplete"}
	}
	for id, w := range co.workers {
		if id != inst.ID && w.instance.LinkState != LinkExited && w.instance.NodeID == inst.NodeID && w.instance.SessionID == inst.SessionID {
			return nil, &Error{Code: CodeConflict, Message: "session already owned by a worker"}
		}
	}
	record := *inst
	record.FirstPrompt, record.BuildID, record.Outdated = "", "", false
	record.LaunchRequestID = requestID
	record.LinkState = LinkConnected
	if record.State == "" {
		record.State = StateAwaitingUser
	}
	if existing != nil {
		buildID = cmp.Or(existing.buildID, buildID)
	}
	firstPrompt := inst.FirstPrompt
	if existing != nil && existing.instance.SessionID == inst.SessionID {
		firstPrompt = cmp.Or(existing.firstPrompt, firstPrompt)
	}
	w := &worker{instance: record, firstPrompt: firstPrompt, buildID: buildID, restart: slices.Contains(hello.Operations, OperationInstanceRestart), link: link, routes: make(map[string]route), tokenHash: hash, epoch: epoch}
	if existing != nil {
		w.legacyRestarting, w.legacyChangeID = existing.legacyRestarting, existing.legacyChangeID
	}
	if co.receipts != nil {
		w.receiptID = receiptKey(record.ID, requestID)
		switch {
		case existing != nil:
			w.receiptID = existing.receiptID
		case pl == nil:
			w.receiptID = key
		}
		w.lastPong = time.Now()
	}
	co.workers[record.ID] = w
	send(link, Frame{Type: TypeWelcome, Version: Version, Role: RoleWorker, Generation: co.generation, InstanceID: record.ID, Instance: &record, Operations: []string{TypeCommand, TypeRegister, TypeEvent, TypeResponse, OperationInstancePrompts, OperationInstanceBuild}, Limits: protocolLimits()})
	co.broadcast(Frame{Type: TypeInstanceUpdate, InstanceID: record.ID, SessionID: record.SessionID, Instance: &record})
	if pl != nil {
		delete(co.launches, record.ID)
		if pl.registered != nil {
			close(pl.registered)
		}
		pl.ready <- record
	}
	return w, nil
}

func (co *coordinator) evictExited() bool {
	for id, w := range co.workers {
		if w.instance.LinkState == LinkExited && w.link == nil {
			delete(co.workers, id)
			co.broadcast(Frame{Type: TypeInstanceUpdate, InstanceID: id, Error: &Error{Code: CodeNotFound, Message: "exited worker removed from registry"}})
			return true
		}
	}
	return false
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
	link := w.link
	w.link = nil
	if co.receipts == nil {
		delete(co.workers, id)
	}
	if w.instance.LinkState != LinkExited {
		w.instance.LinkState = LinkDisconnected
	}
	// Routed commands were queued for the worker and may have run.
	unknown := &Error{Code: CodeOutcomeUnknown, Message: "worker link lost after routing; command outcome unknown"}
	for _, r := range w.routes {
		delete(r.client.pending, r.requestID)
		send(r.client.link, Frame{Type: TypeResponse, ID: r.requestID, RequestID: r.correlationID, InstanceID: id, Error: unknown})
	}
	clear(w.routes)
	message := "worker link closed; the worker may still be running"
	if w.instance.LinkState == LinkExited {
		message = "worker exited"
	} else if errors.Is(link.Err(), ErrSlowConsumer) {
		message = "worker link closed: outbound queue full; the worker may still be running"
	}
	lost := Frame{Type: TypeInstanceUpdate, InstanceID: id, SessionID: w.instance.SessionID, Error: &Error{
		Code: CodeWorkerLinkLost, Message: message,
	}}
	if co.receipts != nil {
		inst := w.instance
		lost.Instance = &inst
	}
	co.broadcast(lost)
	lost.Type = TypeEvent
	co.broadcast(lost)
	for _, c := range co.clients {
		delete(c.subs, id)
	}
}

// monitorWorkers probes the transport, not the agent. A pending approval or
// idle session still responds. A closed socket alone never proves exit.
func (co *coordinator) monitorWorkers() {
	if co.receipts == nil && co.workerBuild == nil {
		return
	}
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	prune := time.NewTicker(time.Minute)
	defer prune.Stop()
	for {
		select {
		case <-co.ctx.Done():
			return
		case now := <-ticker.C:
			if co.receipts != nil {
				co.checkWorkers(now)
			}
			co.refreshLaunchBuild()
		case now := <-prune.C:
			if co.receipts == nil {
				continue
			}
			removed, err := co.receipts.prune(now)
			if err != nil {
				logf("prune launch receipts: %v", err)
			}
			co.mu.Lock()
			for _, w := range co.workers {
				if _, ok := removed[w.receiptID]; ok {
					co.markExited(w)
				}
			}
			co.mu.Unlock()
		}
	}
}

func (co *coordinator) checkWorkers(now time.Time) {
	type suspect struct{ id, receipt string }
	var disconnected []suspect
	co.mu.Lock()
	for id, w := range co.workers {
		switch {
		case w.link != nil:
			if now.Sub(w.lastPong) >= heartbeatTimeout {
				w.link.fail(errors.New("worker heartbeat timed out"))
			} else {
				send(w.link, Frame{Type: TypePing})
			}
		case w.instance.LinkState == LinkDisconnected:
			disconnected = append(disconnected, suspect{id, w.receiptID})
		}
	}
	co.mu.Unlock()
	for _, worker := range disconnected {
		exited, err := co.receipts.exited(worker.receipt)
		if err != nil || !exited {
			continue
		}
		co.mu.Lock()
		if w := co.workers[worker.id]; w != nil {
			co.markExited(w)
		}
		co.mu.Unlock()
	}
}

// markExited requires co.mu and proof that the worker released its lifetime lock.
func (co *coordinator) markExited(w *worker) {
	if w.instance.LinkState == LinkExited {
		return
	}
	w.instance.LinkState = LinkExited
	if w.link != nil {
		w.link.fail(errors.New("worker exited"))
		return // dropWorker publishes the exit after draining the routes.
	}
	inst := w.instance
	co.broadcast(Frame{Type: TypeInstanceUpdate, InstanceID: inst.ID, SessionID: inst.SessionID, Instance: &inst})
}

// This local read is serialized on its client link, not on the registry lock.
// Bound filesystem work even if a lookup stalls beyond its deadline.
func (co *coordinator) lookupSessionDirectory(c *client, f Frame) {
	if co.sessionDir == nil {
		replyError(c, f, CodeUnavailable, "saved session directory lookup is unavailable")
		return
	}
	if f.ID == "" || f.RequestID != "" || !filepath.IsAbs(f.CWD) {
		replyError(c, f, CodeInvalidFrame, "request id and absolute project directory required")
		return
	}
	co.mu.Lock()
	_, duplicate := c.pending[f.ID]
	co.mu.Unlock()
	if duplicate {
		replyError(c, f, CodeDuplicateID, "request id already in flight")
		return
	}
	select {
	case co.directoryReads <- struct{}{}:
	default:
		replyError(c, f, CodeLimitExceeded, "too many session directory lookups")
		return
	}
	ctx, cancel := context.WithTimeout(co.ctx, 5*time.Second)
	defer cancel()
	type result struct {
		dir string
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-co.directoryReads }()
		cwd, err := co.resolveDir(f.CWD)
		var dir string
		if err == nil {
			dir, err = co.sessionDir(ctx, cwd)
		}
		done <- result{dir, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			replyError(c, f, CodeUnavailable, boundedText(r.err.Error()))
			return
		}
		if !filepath.IsAbs(r.dir) || len(r.dir) > maxTextBytes {
			replyError(c, f, CodeUnavailable, "invalid session directory")
			return
		}
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, CWD: r.dir})
	case <-ctx.Done():
		replyError(c, f, CodeUnavailable, "session directory lookup timed out")
	}
}

// lookupSession resolves a session reference to the project that owns it. A
// running worker is authoritative, since its session may not be saved yet.
// Like directory lookups, the disk read is bounded and off the registry lock.
func (co *coordinator) lookupSession(c *client, f Frame) {
	if co.locateSession == nil {
		replyError(c, f, CodeUnavailable, "session lookup is unavailable")
		return
	}
	if f.ID == "" || f.RequestID != "" || f.SessionID == "" || (f.CWD != "" && !filepath.IsAbs(f.CWD)) {
		replyError(c, f, CodeInvalidFrame, "request id and session reference required")
		return
	}
	live := make(map[string]string)
	co.mu.Lock()
	_, duplicate := c.pending[f.ID]
	for _, w := range co.workers {
		if w.instance.LinkState != LinkExited && strings.HasPrefix(w.instance.SessionID, f.SessionID) {
			live[w.instance.SessionID] = w.instance.CWD
		}
	}
	co.mu.Unlock()
	if duplicate {
		replyError(c, f, CodeDuplicateID, "request id already in flight")
		return
	}
	if project, ok := live[f.SessionID]; ok {
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, SessionID: f.SessionID, CWD: project})
		return
	}
	select {
	case co.directoryReads <- struct{}{}:
	default:
		replyError(c, f, CodeLimitExceeded, "too many session lookups")
		return
	}
	ctx, cancel := context.WithTimeout(co.ctx, 5*time.Second)
	defer cancel()
	type result struct {
		id, project string
		err         error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-co.directoryReads }()
		id, project, err := co.locateSession(ctx, f.SessionID, f.CWD)
		done <- result{id, project, err}
	}()
	select {
	case r := <-done:
		if errors.Is(r.err, sessionpkg.ErrSessionNotFound) && len(live) == 1 {
			for id, project := range live {
				r = result{id: id, project: project}
			}
		}
		if r.err != nil {
			replyError(c, f, CodeUnavailable, boundedText(r.err.Error()))
			return
		}
		if project, ok := live[r.id]; ok {
			r.project = project
		}
		if len(live) > 1 || (len(live) == 1 && live[r.id] == "") {
			replyError(c, f, CodeConflict, "ambiguous session ID")
			return
		}
		if !filepath.IsAbs(r.project) || len(r.project) > maxTextBytes || len(r.id) > maxIDBytes {
			replyError(c, f, CodeUnavailable, "invalid saved session location")
			return
		}
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, SessionID: r.id, CWD: r.project})
	case <-ctx.Done():
		replyError(c, f, CodeUnavailable, "session lookup timed out")
	}
}

func (co *coordinator) refreshLaunchBuild() {
	if co.workerBuild == nil {
		return
	}
	co.buildMu.Lock()
	defer co.buildMu.Unlock()
	build := co.workerBuild()
	co.mu.Lock()
	defer co.mu.Unlock()
	if co.launchBuild == build {
		return
	}
	previous := co.launchBuild
	co.launchBuild = build
	for _, w := range co.workers {
		if buildOutdated(previous, w.buildID) == buildOutdated(build, w.buildID) {
			continue
		}
		for _, c := range co.clients {
			if c.watching && c.instanceBuild {
				inst := co.instanceSummary(c, w)
				send(c.link, Frame{Type: TypeInstanceUpdate, InstanceID: inst.ID, SessionID: inst.SessionID, Instance: &inst})
			}
		}
	}
}

func buildOutdated(launchBuild, workerBuild string) bool {
	return launchBuild != "" && workerBuild != "" && launchBuild != workerBuild
}

func (co *coordinator) handleClient(c *client, f Frame) {
	co.mu.Lock()
	defer co.mu.Unlock()
	if f.ID == "" {
		replyError(c, f, CodeInvalidFrame, "id required")
		return
	}
	if f.RequestID != "" && !ValidOperationRequestID(f.Type, f.RequestID) {
		f.RequestID = ""
		replyError(c, f, CodeInvalidFrame, "invalid request_id for operation")
		return
	}
	if _, duplicate := c.pending[f.ID]; duplicate {
		replyError(c, f, CodeDuplicateID, "request id already in flight")
		return
	}
	switch f.Type {
	case TypeListInstances:
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, Generation: co.generation, Instances: co.instances(c)})
	case TypeWatchInstances:
		c.watching = true
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, Generation: co.generation, Instances: co.instances(c)})
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
		if w.link == nil {
			replyError(c, f, CodeUnavailable, "worker is not connected")
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
		co.forward(c, f, f.Command, nested.Type)
	case TypeStopInstance:
		cmd := shutdownCommand
		if w := co.workers[f.InstanceID]; f.SessionID != "" && w != nil && w.link != nil {
			if w.instance.SessionID != f.SessionID {
				replyError(c, f, CodeConflict, "instance switched sessions; select it again")
				return
			}
			if !w.restart {
				co.restartLegacyWorker(c, f, w)
				return
			}
			var err error
			cmd, err = json.Marshal(map[string]any{"type": "shutdown", "args": map[string]string{"expected_session_id": f.SessionID}})
			if err != nil {
				replyError(c, f, CodeInvalidFrame, "cannot encode shutdown target")
				return
			}
		}
		co.forward(c, f, cmd, "shutdown")
	default:
		replyError(c, f, CodeInvalidFrame, "unexpected frame type")
	}
}

// restartLegacyWorker is called with co.mu held. Old workers can shut down
// gracefully, but cannot materialize an untouched session or check its ID at
// dispatch. Check storage before routing and don't overlap a session switch.
func (co *coordinator) restartLegacyWorker(c *client, f Frame, w *worker) {
	if co.sessionDir == nil {
		replyError(c, f, CodeUnavailable, "cannot verify a saved session for this worker")
		return
	}
	if w.legacyRestarting {
		replyError(c, f, CodeBusy, "worker is already restarting")
		return
	}
	id, err := uuid.Parse(f.SessionID)
	if err != nil || id.String() != f.SessionID {
		replyError(c, f, CodeInvalidFrame, "canonical session ID required")
		return
	}
	project := w.instance.CWD
	co.mu.Unlock()
	err = co.checkLegacyRestartSave(project, f.SessionID)
	co.mu.Lock()
	if err != nil {
		replyError(c, f, CodeUnavailable, boundedText(fmt.Sprintf("no resumable save; worker left running: %v", err)))
		return
	}
	if co.workers[f.InstanceID] != w || w.link == nil || w.instance.SessionID != f.SessionID {
		replyError(c, f, CodeConflict, "instance changed while checking its save; select it again")
		return
	}
	if w.legacyRestarting {
		replyError(c, f, CodeBusy, "worker is already restarting")
		return
	}
	if w.legacyChangeID != "" {
		replyError(c, f, CodeBusy, "instance is switching sessions; select it again")
		return
	}
	if co.forward(c, f, shutdownCommand, "shutdown") {
		w.legacyRestarting = true
	}
}

func (co *coordinator) checkLegacyRestartSave(project, id string) error {
	select {
	case co.directoryReads <- struct{}{}:
	default:
		return errors.New("too many saved session checks")
	}
	ctx, cancel := context.WithTimeout(co.ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() { <-co.directoryReads }()
		done <- co.verifyLegacyRestartSave(ctx, project, id)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("check saved session: %w", ctx.Err())
	}
}

func (co *coordinator) verifyLegacyRestartSave(ctx context.Context, project, id string) error {
	cwd, err := co.resolveDir(project)
	if err != nil {
		return err
	}
	dir, err := co.sessionDir(ctx, cwd)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(dir) {
		return errors.New("invalid session directory")
	}
	storage, err := sessionpkg.NewStorageAt(dir)
	if err != nil {
		return err
	}
	header, err := storage.ProjectHeader(id, cwd)
	if err != nil {
		return err
	}
	if header.Type != sessionpkg.EntryTypeSession {
		return errors.New("saved file does not have a session header")
	}
	if header.ID != id {
		return errors.New("saved session does not match the selected session and project")
	}
	return nil
}

func (co *coordinator) startInstance(c *client, f Frame) {
	if f.SessionID != "" {
		id, err := uuid.Parse(f.SessionID)
		if err != nil || id.String() != f.SessionID {
			replyError(c, f, CodeInvalidFrame, "canonical session ID required")
			return
		}
	}
	if !filepath.IsAbs(f.CWD) {
		replyError(c, f, CodeInvalidFrame, "absolute cwd required")
		return
	}
	if e := admit(c, f.ID); e != nil {
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, Error: e})
		return
	}
	if f.RequestID != "" {
		if _, ok := co.startingIDs[f.RequestID]; ok {
			replyError(c, f, CodeOutcomeUnknown, "launch intent already in progress; check the registry")
			return
		}
		for _, w := range co.workers {
			if w.instance.LaunchRequestID == f.RequestID {
				co.reconcileLaunch(c, f, w)
				return
			}
		}
	}
	if f.SessionID != "" {
		for _, w := range co.workers {
			if w.instance.SessionID != f.SessionID || w.instance.LinkState == LinkExited {
				continue
			}
			if f.CWD != w.instance.CWD {
				replyError(c, f, CodeConflict, "session already has a worker in another project")
				return
			}
			if w.link == nil || w.instance.LinkState != LinkConnected {
				replyError(c, f, CodeBusy, "session's worker is disconnected")
				return
			}
			inst := w.instance
			send(c.link, Frame{Type: TypeResponse, ID: f.ID, RequestID: f.RequestID, InstanceID: inst.ID, SessionID: inst.SessionID, Instance: &inst})
			return
		}
		if _, starting := co.startingSessions[f.SessionID]; starting {
			replyError(c, f, CodeBusy, "another worker is starting this session")
			return
		}
	}
	starting := len(co.launches) + co.validating + co.stopping
	if starting >= maxLaunches || (len(co.workers)+starting >= maxWorkers && !co.evictExited()) {
		replyError(c, f, CodeLimitExceeded, "too many instances")
		return
	}
	id := uuid.NewString()
	if co.receipts != nil && f.RequestID == "" {
		id = uuid.Must(uuid.NewV7()).String()
	}
	if f.RequestID != "" {
		co.startingIDs[f.RequestID] = struct{}{}
	}
	if f.SessionID != "" {
		co.startingSessions[f.SessionID] = struct{}{}
	}
	co.validating++
	c.pending[f.ID] = pendingRef{instanceID: id}
	co.wg.Go(func() { co.runLaunch(c, f, id) })
}

// An existing intent identifies one worker; resolving cwd is only for a new launch.
func (co *coordinator) reconcileLaunch(c *client, f Frame, w *worker) {
	if w.link == nil || w.instance.LinkState == LinkExited {
		replyError(c, f, CodeOutcomeUnknown, "launch intent already recorded; no running worker has confirmed its outcome")
		return
	}
	inst := w.instance
	send(c.link, Frame{Type: TypeResponse, ID: f.ID, RequestID: f.RequestID, InstanceID: inst.ID, SessionID: inst.SessionID, Instance: &inst})
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

func (co *coordinator) runLaunch(c *client, f Frame, id string) {
	defer func() {
		co.mu.Lock()
		delete(co.startingIDs, f.RequestID)
		delete(co.startingSessions, f.SessionID)
		co.mu.Unlock()
	}()
	requestID := f.ID
	ref := pendingRef{instanceID: id}
	var receiptErr error
	if co.receipts != nil {
		receiptErr = co.receipts.check(receiptKey(id, f.RequestID), time.Now())
	}
	var cwd string
	var err error
	if receiptErr == nil {
		cwd, err = co.projectDir(f.CWD)
	}
	reply := Frame{ID: requestID, RequestID: f.RequestID}

	co.mu.Lock()
	co.validating--
	if c.pending[requestID] != ref {
		co.mu.Unlock()
		return
	}
	if co.ctx.Err() != nil {
		delete(c.pending, requestID)
		replyError(c, reply, CodeUnavailable, "coordinator is shutting down")
		co.mu.Unlock()
		return
	}
	if receiptErr != nil {
		delete(c.pending, requestID)
		co.replyReceiptError(c, f, receiptErr)
		co.mu.Unlock()
		return
	}
	if err != nil {
		delete(c.pending, requestID)
		replyError(c, reply, CodeInvalidFrame, boundedText(err.Error()))
		co.mu.Unlock()
		return
	}
	pl := &pendingLaunch{token: rand.Text(), cwd: cwd, sessionID: f.SessionID, requestID: f.RequestID, ready: make(chan Instance, 1), registered: make(chan struct{})}
	co.launches[id] = pl
	co.mu.Unlock()

	var inst Instance
	if co.receipts != nil {
		pl.lifetime, receiptErr = co.receipts.create(receiptKey(id, f.RequestID), launchReceipt{
			InstanceID: id, CWD: cwd, Generation: co.generation, TokenHash: sha256.Sum256([]byte(pl.token)),
		})
	}
	if receiptErr == nil {
		inst, err = co.superviseLaunch(id, pl)
	}

	co.mu.Lock()
	defer co.mu.Unlock()
	if co.launches[id] == pl {
		delete(co.launches, id)
	}
	if c.pending[requestID] != ref {
		return
	}
	delete(c.pending, requestID)
	if receiptErr != nil {
		co.replyReceiptError(c, f, receiptErr)
		return
	}
	if err != nil {
		msg := boundedText(strings.ReplaceAll(err.Error(), pl.token, "[redacted]"))
		send(c.link, Frame{Type: TypeResponse, ID: requestID, RequestID: f.RequestID, InstanceID: id, Error: &Error{Code: CodeLaunchFailed, Message: msg}})
		return
	}
	send(c.link, Frame{Type: TypeResponse, ID: requestID, RequestID: f.RequestID, InstanceID: id, SessionID: inst.SessionID, Instance: &inst})
}

func (co *coordinator) replyReceiptError(c *client, f Frame, err error) {
	switch {
	case errors.Is(err, os.ErrExist), errors.Is(err, errExpiredLaunchIntent),
		errors.Is(err, errFutureLaunchIntent), errors.Is(err, errStaleLaunchIntent):
		// Registration may have completed while the receipt was being checked.
		if f.RequestID != "" {
			for _, w := range co.workers {
				if w.instance.LaunchRequestID == f.RequestID {
					co.reconcileLaunch(c, f, w)
					return
				}
			}
		}
	}
	switch {
	case errors.Is(err, os.ErrExist), errors.Is(err, errExpiredLaunchIntent):
		replyError(c, f, CodeOutcomeUnknown, "launch intent was recorded or is outside the receipt window; check the registry")
	case errors.Is(err, errFutureLaunchIntent), errors.Is(err, errStaleLaunchIntent):
		// No receipt exists and the durable cutoff proves none was pruned.
		replyError(c, f, CodeClockSkew, err.Error())
	case errors.Is(err, errLegacyLaunchIntent):
		replyError(c, f, CodeInvalidFrame, err.Error())
	case errors.Is(err, errReceiptLimit):
		replyError(c, f, CodeLimitExceeded, err.Error())
	default:
		replyError(c, f, CodeLaunchFailed, boundedText(err.Error()))
	}
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
		err := co.launch(ctx, LaunchRequest{InstanceID: id, LaunchRequestID: pl.requestID, Token: pl.token, CWD: pl.cwd, SessionID: pl.sessionID, Ready: pl.registered, Lifetime: pl.lifetime})
		if pl.lifetime != nil {
			err = errors.Join(err, pl.lifetime.Close())
		}
		launched <- err
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
// IDs never reach workers and cannot collide across clients. The worker sees
// the authenticated client identity, the explicit target, and the client's
// correlation ID, which identifies the request but authorizes nothing. A
// failed enqueue is definitive non-delivery; once queued, a lost link makes
// the outcome unknown.
func (co *coordinator) forward(c *client, f Frame, cmd json.RawMessage, kind string) bool {
	if f.InstanceID == "" {
		replyError(c, f, CodeInvalidFrame, "instance_id required")
		return false
	}
	w := co.workers[f.InstanceID]
	if w == nil {
		replyError(c, f, CodeNotFound, "instance not found")
		return false
	}
	if w.link == nil {
		replyError(c, f, CodeUnavailable, "worker is not connected")
		return false
	}
	legacyChange := !w.restart && (kind == "new_session" || kind == "create_session" || kind == "resume_session" || kind == "fork")
	if legacyChange {
		if w.legacyRestarting {
			replyError(c, f, CodeBusy, "worker is restarting")
			return false
		}
		if w.legacyChangeID != "" {
			replyError(c, f, CodeBusy, "worker is already switching sessions")
			return false
		}
	}
	if e := admit(c, f.ID); e != nil {
		send(c.link, Frame{Type: TypeResponse, ID: f.ID, InstanceID: f.InstanceID, Error: e})
		return false
	}
	routeID := uuid.NewString()
	err := w.link.Send(Frame{
		Type:       TypeCommand,
		ID:         routeID,
		InstanceID: w.instance.ID,
		SessionID:  w.instance.SessionID,
		ClientID:   c.identity,
		RequestID:  f.RequestID,
		Command:    cmd,
	})
	if err != nil {
		code := CodeUnavailable
		if errors.Is(err, ErrFrameTooLarge) {
			code = CodeLimitExceeded
		}
		replyError(c, f, code, "command not delivered")
		return false
	}
	if legacyChange {
		w.legacyChangeID = routeID
	}
	w.routes[routeID] = route{client: c, requestID: f.ID, correlationID: f.RequestID}
	c.pending[f.ID] = pendingRef{instanceID: w.instance.ID, routeID: routeID}
	return true
}

func (co *coordinator) handleWorker(w *worker, f Frame) {
	co.mu.Lock()
	defer co.mu.Unlock()
	id := w.instance.ID
	switch f.Type {
	case TypePong:
		w.lastPong = time.Now()
	case TypeResponse:
		if w.legacyChangeID == f.ID {
			w.legacyChangeID = ""
		}
		r, ok := w.routes[f.ID]
		if !ok {
			return
		}
		delete(w.routes, f.ID)
		delete(r.client.pending, r.requestID)
		send(r.client.link, Frame{Type: TypeResponse, ID: r.requestID, RequestID: r.correlationID, InstanceID: id, Result: f.Result, Error: f.Error})
	case TypeEvent:
		if !isObject(f.Event) {
			send(w.link, Frame{Type: TypeResponse, ID: f.ID, Error: &Error{Code: CodeInvalidFrame, Message: "event object required"}})
			return
		}
		w.instance.LastActivity = time.Now().UTC()
		// A worker labels events from a newly switched session before its
		// register update arrives, so its own session ID wins.
		session := cmp.Or(f.SessionID, w.instance.SessionID)
		co.broadcast(Frame{Type: TypeEvent, InstanceID: id, SessionID: session, Position: f.Position, Event: f.Event})
	case TypeRegister:
		if f.Instance == nil || f.Instance.ID != id {
			send(w.link, Frame{Type: TypeResponse, ID: f.ID, Error: &Error{Code: CodeConflict, Message: "instance mismatch"}})
			return
		}
		if w.instance.SessionID != f.Instance.SessionID || w.firstPrompt == "" {
			w.firstPrompt = f.Instance.FirstPrompt
		}
		w.instance.SessionID = f.Instance.SessionID
		w.buildID = cmp.Or(w.buildID, f.Instance.BuildID)
		if f.Instance.State != "" {
			w.instance.State = f.Instance.State
			w.instance.StateDetail = f.Instance.StateDetail
		}
		w.instance.PendingInputs = f.Instance.PendingInputs
		if f.Instance.LastActivity.After(w.instance.LastActivity) {
			w.instance.LastActivity = f.Instance.LastActivity
		}
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
// with the events they expose. Reuse the encoding across recipients.
func (co *coordinator) broadcast(f Frame) {
	if f.Type == TypeInstanceUpdate {
		f.Generation = co.generation
	}
	var data [4][]byte
	var errs [4]error
	for _, c := range co.clients {
		_, attached := c.subs[f.InstanceID]
		if (f.Type == TypeInstanceUpdate && !c.watching) || (f.Type != TypeInstanceUpdate && !attached) {
			continue
		}
		var key int
		if f.Type == TypeInstanceUpdate && f.Instance != nil {
			if c.instancePrompts {
				key |= 1
			}
			if c.instanceBuild {
				key |= 2
			}
		}
		if data[key] == nil && errs[key] == nil {
			frame := f
			if key != 0 {
				if w := co.workers[f.InstanceID]; w != nil {
					frame.Instance = new(co.instanceSummary(c, w))
				}
			}
			data[key], errs[key] = encodeOrLimit(frame)
		}
		queueFrame(c.link, data[key], errs[key])
	}
}

func (co *coordinator) instanceSummary(c *client, w *worker) Instance {
	inst := w.instance
	if c.instancePrompts {
		inst.FirstPrompt = w.firstPrompt
	}
	if c.instanceBuild {
		inst.Outdated = buildOutdated(co.launchBuild, w.buildID)
	}
	return inst
}

func (co *coordinator) instances(c *client) []Instance {
	list := make([]Instance, 0, len(co.workers))
	for _, w := range co.workers {
		list = append(list, co.instanceSummary(c, w))
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
	send(c.link, Frame{Type: TypeResponse, ID: f.ID, RequestID: f.RequestID, InstanceID: f.InstanceID, Error: &Error{Code: code, Message: msg}})
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
		RequestID:  f.RequestID,
		InstanceID: f.InstanceID,
		SessionID:  f.SessionID,
		Position:   f.Position,
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
