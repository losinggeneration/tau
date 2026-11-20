package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
)

const (
	coordinatedEventBuffer = 256
	maxClientIDBytes       = 256
	// Live frames withheld for one instance while its view is rebuilt.
	maxStagedFrames = coordinatedEventBuffer
	maxStagedBytes  = 8 << 20
	// A worker journal holds at most 2048 frames and 8 MiB, so a resume that
	// needs more is chasing an unbounded stream.
	maxReplayFrames = 4096
	maxReplayBytes  = 16 << 20
)

// Worker reconstruction commands.
const (
	cmdGetSnapshot  = "get_snapshot"
	cmdReplayEvents = "replay_events"
	cmdGetHistory   = "get_history"
	cmdEntryContent = "get_entry_content"
)

var (
	// ErrTargetRequired reports a targeted request without an instance ID.
	// The coordinated client never falls back to an implicitly selected
	// instance.
	ErrTargetRequired = errors.New("coordinated client: instance id required")
	// ErrTooManyPending reports that coord.MaxPending requests are already in
	// flight. The request was not sent.
	ErrTooManyPending = errors.New("coordinated client: too many pending requests")
	// ErrOutcomeUnknown reports that a request was sent but no definite reply
	// arrived. The coordinator may or may not have acted on it; requests are
	// never retried automatically. See
	// OutcomeUnknownError for the request's identity.
	ErrOutcomeUnknown = errors.New("coordinated client: request outcome unknown")
	// ErrSlowEventConsumer reports that Events was not drained quickly enough.
	// The client disconnects rather than dropping events.
	ErrSlowEventConsumer = errors.New("coordinated client: event consumer too slow")
	// ErrClientClosed reports use of a client after Close.
	ErrClientClosed = errors.New("coordinated client: closed")
	// ErrInvalidClientID reports an unusable stable client ID, or a
	// coordinator that bound the connection to a different one.
	ErrInvalidClientID = errors.New("coordinated client: invalid client id")
	// ErrInvalidRequestID reports a caller-supplied request ID that is not
	// a canonical UUID of the supported version for the operation. The
	// request was not sent.
	ErrInvalidRequestID = errors.New("coordinated client: invalid request id for operation")
	// ErrReconstructionBusy reports a second concurrent reconstruction of one
	// instance on the same client.
	ErrReconstructionBusy = errors.New("coordinated client: instance reconstruction already in progress")

	// ErrResyncRequired reports that a view cannot be continued from its
	// cursor. Discard the view and rebuild it with AttachSnapshot.
	ErrResyncRequired = errors.New("coordinated client: resync required")
	// ErrStaleCursor reports an event from another worker epoch.
	ErrStaleCursor = errors.New("coordinated client: event cursor stale")
	// ErrEventGap reports an event that skips past the next expected offset.
	ErrEventGap = errors.New("coordinated client: event gap")
	// ErrEventOmitted reports a placeholder for an event too large to deliver.
	ErrEventOmitted = errors.New("coordinated client: worker omitted event")
	// ErrEventTarget reports an event for an instance other than the cursor's.
	ErrEventTarget = errors.New("coordinated client: event for another instance")
	// ErrReconstructionOverflow reports staged live events or replay pages
	// exceeding the client's bounds. Nothing was silently dropped: the whole
	// reconstruction failed.
	ErrReconstructionOverflow = errors.New("coordinated client: reconstruction exceeded bounds")
)

// OutcomeUnknownError identifies a request whose outcome is unknown: the
// client gave up waiting, the connection ended, or the coordinator lost the
// worker after routing it (coord.CodeOutcomeUnknown). The client never
// resends it. Reconcile by inspecting state before repeating a mutation.
type OutcomeUnknownError struct {
	// Op is the coordinated frame type, such as coord.TypeCommand.
	Op         string
	InstanceID string
	// RequestID is the mutation's correlation ID; empty for reads.
	RequestID string
	// Err is the cause: a context error, the disconnect reason, or the
	// coordinator's *coord.Error.
	Err error
}

func (e *OutcomeUnknownError) Error() string {
	msg := e.Op
	if e.InstanceID != "" {
		msg += " " + e.InstanceID
	}
	if e.RequestID != "" {
		msg += " request " + e.RequestID
	}
	return fmt.Sprintf("%s: %v: %v", msg, ErrOutcomeUnknown, e.Err)
}

// Unwrap matches both ErrOutcomeUnknown and the cause.
func (e *OutcomeUnknownError) Unwrap() []error {
	return []error{ErrOutcomeUnknown, e.Err}
}

// ResyncError reports why an instance's event stream cannot continue from
// Cursor. It matches ErrResyncRequired and its cause: ErrStaleCursor,
// ErrEventGap, ErrEventTarget, ErrReconstructionOverflow, or the worker's
// *coord.Error with coord.CodeResyncRequired.
type ResyncError struct {
	InstanceID string
	// Cursor is the last position applied.
	Cursor coord.Position
	// Got is the offending event position, when there is one.
	Got *coord.Position
	Err error
}

func (e *ResyncError) Error() string {
	msg := fmt.Sprintf("%v: %s at %s/%d", ErrResyncRequired, e.InstanceID, e.Cursor.Epoch, e.Cursor.Offset)
	if e.Got != nil {
		msg += fmt.Sprintf(", got %s %s/%d", e.Got.InstanceID, e.Got.Epoch, e.Got.Offset)
	}
	return fmt.Sprintf("%s: %v", msg, e.Err)
}

// Unwrap matches both ErrResyncRequired and the cause.
func (e *ResyncError) Unwrap() []error {
	return []error{ErrResyncRequired, e.Err}
}

// CoordinatedOption configures NewCoordinatedClient.
type CoordinatedOption func(*coordinatedOptions)

type coordinatedOptions struct {
	clientID string
	stable   bool
}

// WithClientID announces id instead of a freshly generated client ID. Reuse
// it across reconnects so the coordinator binds each authenticated
// connection to the same client identity. It identifies the client and
// grants no authority. The handshake fails with ErrInvalidClientID if id is
// empty or too long, or if the coordinator assigns a different identity.
func WithClientID(id string) CoordinatedOption {
	return func(o *coordinatedOptions) {
		o.clientID, o.stable = id, true
	}
}

// NewRequestID returns a random request ID for a mutation. Persist it with
// the intent before sending so an unknown outcome can be matched later.
func NewRequestID() string {
	return uuid.NewString()
}

// NewLaunchRequestID returns a UUIDv7 for a start intent. Persist it with the
// intent before sending so a lost reply can be reconciled without a new launch.
func NewLaunchRequestID() string {
	return uuid.Must(uuid.NewV7()).String()
}

// CoordinatedClient talks to a local coordinator that owns many tau
// instances. Every instance-scoped request names its target explicitly, and
// attaching or detaching only changes event subscriptions; it never stops an
// instance.
type CoordinatedClient struct {
	link     *coord.Link
	clientID string
	welcome  coord.Frame
	welcomed time.Time
	events   chan coord.Frame
	done     chan struct{}

	closeOnce sync.Once
	closed    chan struct{}

	mu      sync.Mutex
	err     error
	pending map[string]chan coord.Frame
	stages  map[string]*liveStage
}

// NewCoordinatedClient connects to the coordinator socket at path and
// completes the handshake, under a freshly generated client ID unless
// WithClientID supplies a stable one.
func NewCoordinatedClient(ctx context.Context, path string, opts ...CoordinatedOption) (*CoordinatedClient, error) {
	return dialCoordinated(ctx, path, coordinatedEventBuffer, opts...)
}

func dialCoordinated(ctx context.Context, path string, eventBuffer int, opts ...CoordinatedOption) (*CoordinatedClient, error) {
	var o coordinatedOptions
	for _, opt := range opts {
		opt(&o)
	}
	clientID := uuid.NewString()
	if o.stable {
		if o.clientID == "" || len(o.clientID) > maxClientIDBytes {
			return nil, ErrInvalidClientID
		}
		clientID = o.clientID
	}
	link, welcome, err := coord.Dial(ctx, path, coord.Frame{
		Type:       coord.TypeHello,
		Version:    coord.Version,
		Role:       coord.RoleClient,
		ClientID:   clientID,
		Operations: []string{coord.OperationInstancePrompts, coord.OperationInstanceBuild},
	})
	if err != nil {
		return nil, err
	}
	if o.stable && welcome.ClientID != clientID {
		err := fmt.Errorf("%w: coordinator assigned %q", ErrInvalidClientID, welcome.ClientID)
		if closeErr := link.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, err
	}
	return newCoordinatedClient(link, welcome, clientID, eventBuffer), nil
}

// NewCoordinatedWebSocketClient connects to the authenticated bridge's /rpc
// endpoint. Use ws on loopback through an SSH forward, or wss with a trusted
// TLS certificate. The bridge assigns a new client identity on each dial.
func NewCoordinatedWebSocketClient(ctx context.Context, endpoint, token string) (_ *CoordinatedClient, err error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "/rpc" || u.Hostname() == "" {
		return nil, errors.New("expected a WebSocket /rpc endpoint without credentials, query or fragment")
	}
	if u.Scheme == "ws" {
		host := u.Hostname()
		ip, err := netip.ParseAddr(host)
		if !strings.EqualFold(host, "localhost") && (err != nil || !ip.IsLoopback()) {
			return nil, errors.New("plaintext WebSocket requires a loopback endpoint; use an SSH forward or wss")
		}
	} else if u.Scheme != "wss" {
		return nil, errors.New("expected a ws or wss endpoint")
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("a valid WebSocket bearer token is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dialer := websocket.Dialer{Subprotocols: []string{"tau.coord.v1"}}
	ws, response, err := dialer.DialContext(ctx, u.String(), http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		if response != nil {
			return nil, errors.Join(fmt.Errorf("WebSocket upgrade failed (HTTP %d): %w", response.StatusCode, err), response.Body.Close())
		}
		return nil, fmt.Errorf("dial coordinated WebSocket: %w", err)
	}
	link := coord.NewWebSocketLink(ws)
	defer func() {
		if err != nil {
			err = errors.Join(err, link.Close())
		}
	}()
	if ws.Subprotocol() != "tau.coord.v1" {
		return nil, fmt.Errorf("%w: missing coordinated WebSocket subprotocol", coord.ErrInvalidFrame)
	}
	stop := context.AfterFunc(ctx, func() {
		if err := ws.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return // Read reports the connection failure.
		}
	})
	defer stop()
	welcome, err := link.Read()
	if !stop() {
		return nil, fmt.Errorf("read WebSocket welcome: %w", ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("read WebSocket welcome: %w", err)
	}
	if welcome.Type != coord.TypeWelcome || welcome.Version != coord.Version || welcome.Role != coord.RoleClient || welcome.ClientID == "" {
		return nil, fmt.Errorf("%w: unexpected WebSocket welcome", coord.ErrInvalidFrame)
	}
	return newCoordinatedClient(link, welcome, welcome.ClientID, coordinatedEventBuffer), nil
}

func newCoordinatedClient(link *coord.Link, welcome coord.Frame, clientID string, eventBuffer int) *CoordinatedClient {
	c := &CoordinatedClient{
		link:     link,
		clientID: clientID,
		welcome:  welcome,
		welcomed: time.Now(),
		events:   make(chan coord.Frame, eventBuffer),
		done:     make(chan struct{}),
		closed:   make(chan struct{}),
		pending:  make(map[string]chan coord.Frame),
		stages:   make(map[string]*liveStage),
	}
	go c.readLoop()
	return c
}

// ClientID returns the identity announced by a local client or assigned by the bridge.
func (c *CoordinatedClient) ClientID() string {
	return c.clientID
}

// Welcome returns the coordinator's handshake reply, including its
// generation and the client ID it assigned.
func (c *CoordinatedClient) Welcome() coord.Frame {
	return c.welcome
}

// Generation identifies this connection's registry baseline. After reconnect,
// call WatchInstances again and replace all previous registry summaries.
func (c *CoordinatedClient) Generation() string { return c.welcome.Generation }

// CoordinatorPID is the process ID reported by the coordinator, or zero.
func (c *CoordinatedClient) CoordinatorPID() int { return c.welcome.PID }

// Events returns chat frames (TypeEvent) for attached instances and summary
// frames (TypeInstanceUpdate) when watching. Both identify the instance and
// session. Event holds a raw Tau RPC event; Instance holds updated metadata.
// Error reports an omitted event or a lost worker link. Drain the channel to
// stay connected. When it closes, Err reports why.
//
// While AttachSnapshot or Resume rebuilds an instance's view, that
// instance's chat frames are withheld here and returned in the Handoff
// instead; frames for other instances keep flowing.
func (c *CoordinatedClient) Events() <-chan coord.Frame {
	return c.events
}

// Done is closed once the connection has ended and Events is closed.
func (c *CoordinatedClient) Done() <-chan struct{} {
	return c.done
}

// Err reports why the client disconnected, or nil while it is connected.
func (c *CoordinatedClient) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// SessionDirectory returns the daemon's saved-session storage for cwd.
// The lookup is only available on the local daemon socket.
func (c *CoordinatedClient) SessionDirectory(ctx context.Context, cwd string) (string, error) {
	if !slices.Contains(c.welcome.Operations, coord.TypeSessionDirectory) {
		return "", errors.New("coordinator does not support looking up saved sessions; restart it with the current Tau version")
	}
	reply, err := c.request(ctx, coord.Frame{Type: coord.TypeSessionDirectory, CWD: cwd})
	return reply.CWD, err
}

// ListInstances returns the coordinator registry, including any retained
// disconnected or exited workers. Check LinkState before attaching.
func (c *CoordinatedClient) ListInstances(ctx context.Context) ([]coord.Instance, error) {
	reply, err := c.request(ctx, coord.Frame{Type: coord.TypeListInstances})
	if err != nil {
		return nil, err
	}
	return reply.Instances, nil
}

// WatchInstances subscribes to status and attention without subscribing to
// chat. The initial list is a full baseline for Generation(); replace any
// older registry view with it before applying subsequent summary frames.
func (c *CoordinatedClient) WatchInstances(ctx context.Context) ([]coord.Instance, error) {
	reply, err := c.request(ctx, coord.Frame{Type: coord.TypeWatchInstances})
	if err != nil {
		return nil, err
	}
	return reply.Instances, nil
}

// UnwatchInstances ends summary updates without changing chat subscriptions.
func (c *CoordinatedClient) UnwatchInstances(ctx context.Context) error {
	_, err := c.request(ctx, coord.Frame{Type: coord.TypeUnwatchInstances})
	return err
}

// StartInstance asks the coordinator to launch an instance rooted at cwd,
// which must be absolute, and waits for it to register. It sends a fresh
// request ID; see StartInstanceWithRequestID.
func (c *CoordinatedClient) StartInstance(ctx context.Context, cwd string) (*coord.Instance, error) {
	return c.StartInstanceWithRequestID(ctx, cwd, "")
}

// StartInstanceWithRequestID is StartInstance under the caller's request ID,
// which should come from c.NewStartRequestID.
// An empty ID uses the UUID version advertised at handshake. The started
// instance reports it as LaunchRequestID. A recoverable coordinator records
// the ID before spawning. Reusing it can return the matching live worker,
// regardless of cwd, but cannot launch a replacement for a recorded intent.
// An expired ID or a recorded ID with no live match reports
// *OutcomeUnknownError. A new UUIDv4 intent is rejected on a recoverable
// coordinator. Other mutations do not deduplicate.
func (c *CoordinatedClient) StartInstanceWithRequestID(ctx context.Context, cwd, requestID string) (*coord.Instance, error) {
	return c.startInstance(ctx, cwd, "", requestID)
}

// StartSavedSession returns the connected worker for the saved session in cwd,
// or launches one if it has no worker. If a worker is starting or reconnecting,
// it waits for the worker within ctx. It does not attach to the returned worker.
func (c *CoordinatedClient) StartSavedSession(ctx context.Context, cwd, sessionID string) (*coord.Instance, error) {
	if sessionID == "" {
		return nil, errors.New("saved session ID required")
	}
	if !slices.Contains(c.welcome.Operations, coord.OperationSavedSessionLaunch) {
		return nil, errors.New("coordinator does not support launching saved sessions; restart it with the current Tau version")
	}
	for {
		inst, err := c.startInstance(ctx, cwd, sessionID, "")
		// Busy guarantees that this request didn't start a worker. Don't
		// retry a launch whose outcome is unknown.
		if e, ok := errors.AsType[*coord.Error](err); !ok || e.Code != coord.CodeBusy || errors.Is(err, ErrOutcomeUnknown) {
			return inst, err
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("waiting for saved session worker: %w", errors.Join(ctx.Err(), err))
		case <-timer.C:
		}
	}
}

func (c *CoordinatedClient) startInstance(ctx context.Context, cwd, sessionID, requestID string) (*coord.Instance, error) {
	if requestID == "" {
		requestID = c.NewStartRequestID()
	} else if !coord.ValidLaunchRequestID(requestID) {
		return nil, ErrInvalidRequestID
	}
	reply, err := c.request(ctx, coord.Frame{Type: coord.TypeStartInstance, CWD: cwd, SessionID: sessionID, RequestID: requestID})
	if err != nil {
		return nil, err
	}
	if reply.Instance == nil {
		return nil, fmt.Errorf("start instance: %w: reply missing instance", coord.ErrInvalidFrame)
	}
	if sessionID != "" && reply.Instance.SessionID != sessionID {
		return nil, fmt.Errorf("start saved session: %w: reply names a different session", coord.ErrInvalidFrame)
	}
	return reply.Instance, nil
}

// NewStartRequestID makes a launch ID using the format and clock advertised
// in Welcome. Persist it before sending if you may need to reconcile a lost reply.
func (c *CoordinatedClient) NewStartRequestID() string {
	if c.welcome.LaunchRequestVersion != 7 {
		return NewRequestID()
	}
	ms := c.welcome.CoordinatorTimeUnixMS
	if ms == 0 {
		return NewLaunchRequestID()
	}
	ms += time.Since(c.welcomed).Milliseconds()
	id := uuid.New()
	for i := 5; i >= 0; i-- {
		id[i] = byte(ms)
		ms >>= 8
	}
	id[6] = id[6]&0x0f | 0x70
	return id.String()
}

// Attach subscribes to events from instanceID.
func (c *CoordinatedClient) Attach(ctx context.Context, instanceID string) error {
	_, err := c.targeted(ctx, coord.Frame{Type: coord.TypeAttach, InstanceID: instanceID})
	return err
}

// Detach unsubscribes from instanceID's events. The instance keeps running.
func (c *CoordinatedClient) Detach(ctx context.Context, instanceID string) error {
	_, err := c.targeted(ctx, coord.Frame{Type: coord.TypeDetach, InstanceID: instanceID})
	return err
}

// Command sends a tau command to instanceID and returns the instance's
// response. A missing cmd.ID is filled in, and a fresh request ID is sent;
// see CommandWithRequestID. Routing failures are returned as *coord.Error;
// command failures are reported in the Response.
func (c *CoordinatedClient) Command(ctx context.Context, instanceID string, cmd Command) (*Response, error) {
	return c.CommandWithRequestID(ctx, instanceID, "", cmd)
}

// CommandWithRequestID is Command under the caller's request ID, which must
// be a random UUID; empty generates one. Reuse one ID for one mutation
// intent. The ID correlates the request for the worker and in an
// *OutcomeUnknownError; it does not deduplicate, so sending it again may
// apply the mutation twice.
func (c *CoordinatedClient) CommandWithRequestID(ctx context.Context, instanceID, requestID string, cmd Command) (*Response, error) {
	if instanceID == "" {
		return nil, ErrTargetRequired
	}
	requestID, err := requestIDOrNew(requestID)
	if err != nil {
		return nil, err
	}
	reply, cmdID, err := c.command(ctx, instanceID, requestID, cmd)
	if err != nil {
		return nil, err
	}
	return commandResponse(cmdID, reply.Result)
}

// StopInstance asks instanceID to shut down. It sends a fresh request ID;
// see StopInstanceWithRequestID.
func (c *CoordinatedClient) StopInstance(ctx context.Context, instanceID string) error {
	return c.StopInstanceWithRequestID(ctx, instanceID, "")
}

// StopInstanceWithRequestID is StopInstance under the caller's request ID,
// which must be a random UUID; empty generates one.
func (c *CoordinatedClient) StopInstanceWithRequestID(ctx context.Context, instanceID, requestID string) error {
	return c.stopInstance(ctx, instanceID, "", requestID)
}

// StopInstanceForRestart gracefully stops the worker after it saves sessionID
// or the coordinator verifies an existing save for an older worker.
func (c *CoordinatedClient) StopInstanceForRestart(ctx context.Context, instanceID, sessionID string) error {
	if sessionID == "" {
		return errors.New("restart requires a session ID")
	}
	if !slices.Contains(c.welcome.Operations, coord.OperationInstanceRestart) {
		return errors.New("coordinator does not support session-preserving restart")
	}
	return c.stopInstance(ctx, instanceID, sessionID, "")
}

func (c *CoordinatedClient) stopInstance(ctx context.Context, instanceID, sessionID, requestID string) error {
	if instanceID == "" {
		return ErrTargetRequired
	}
	requestID, err := requestIDOrNew(requestID)
	if err != nil {
		return err
	}
	reply, err := c.targeted(ctx, coord.Frame{Type: coord.TypeStopInstance, InstanceID: instanceID, SessionID: sessionID, RequestID: requestID})
	if err != nil {
		return err
	}
	response, err := commandResponse(reply.ID, reply.Result)
	if err != nil {
		return err
	}
	if !response.OK {
		if response.Error != nil {
			return &coord.Error{Code: response.Error.Code, Message: response.Error.Message}
		}
		return errors.New("worker stop failed without error details")
	}
	return nil
}

// Close disconnects from the coordinator. Instances keep running, and any
// waiting requests return ErrOutcomeUnknown.
func (c *CoordinatedClient) Close() error {
	c.fail(ErrClientClosed)
	err := c.link.Close()
	<-c.done
	return err
}

// Snapshot is a bounded baseline of one instance. Position anchors Live.
// Persisted history, session state and pending requests can already reflect
// later events, so merge messages and requests by ID and apply state updates
// as replacements.
type Snapshot struct {
	Position coord.Position `json:"position"`
	Instance coord.Instance `json:"instance"`
	History  History        `json:"history"`
	State    SnapshotState  `json:"state"`
	// Live holds in-progress messages, tool calls, and bash runs.
	Live       json.RawMessage   `json:"live,omitempty"`
	UIRequests []json.RawMessage `json:"ui_requests"`
}

// SnapshotState holds the session state read during snapshot capture.
type SnapshotState struct {
	SessionID     string          `json:"session_id"`
	Model         json.RawMessage `json:"model,omitempty"`
	ThinkingLevel string          `json:"thinking_level,omitempty"`
	IsStreaming   bool            `json:"is_streaming"`
	// IsCompacting reflects the compaction events through Snapshot.Position.
	IsCompacting bool              `json:"is_compacting,omitempty"`
	Queue        json.RawMessage   `json:"queue,omitempty"`
	Stats        *sdk.SessionStats `json:"stats,omitempty"`
}

// History is one page of persisted conversation, newest last. Pass Before
// to History to fetch the preceding page; empty means no earlier entries.
type History struct {
	Entries []HistoryEntry `json:"entries"`
	Before  string         `json:"before"`
}

// HistoryEntry is one persisted message. Truncated entries exceeded the page
// bound and carry a bounded preview; use EntryContent to read the rest.
// Omitted entries could not fit even a preview and carry no message.
type HistoryEntry struct {
	EntryID   string          `json:"entry_id"`
	Message   json.RawMessage `json:"message,omitempty"`
	Omitted   bool            `json:"omitted,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

// ReplayPage is one contiguous page of events after a cursor.
type ReplayPage struct {
	Events []coord.Frame `json:"events"`
	More   bool          `json:"more"`
}

// Handoff is a rebuilt view of one instance and the live events that
// follow it.
type Handoff struct {
	// Snapshot is the baseline from AttachSnapshot. It is nil for Resume,
	// whose caller retained its own view.
	Snapshot *Snapshot
	// Events continue the baseline in position order, with no repeated
	// event positions. When Snapshot is present, merge by entry or request
	// ID because history and dialogs may already reflect these events.
	Events []coord.Frame
	// Cursor is positioned after Events. Pass every later frame for this
	// instance from Events() through Cursor.Accept.
	Cursor *EventCursor
}

// Snapshot fetches instanceID's current baseline without attaching. Use
// AttachSnapshot to continue it with live events.
func (c *CoordinatedClient) Snapshot(ctx context.Context, instanceID string) (*Snapshot, error) {
	return c.snapshot(ctx, instanceID, Command{Type: cmdGetSnapshot})
}

// SnapshotPreview fetches a snapshot whose history references images by entry ID.
func (c *CoordinatedClient) SnapshotPreview(ctx context.Context, instanceID string) (*Snapshot, error) {
	return c.snapshot(ctx, instanceID, Command{Type: cmdGetSnapshot, Args: map[string]bool{"preview": true}})
}

func (c *CoordinatedClient) snapshot(ctx context.Context, instanceID string, cmd Command) (*Snapshot, error) {
	if instanceID == "" {
		return nil, ErrTargetRequired
	}
	var s Snapshot
	if err := c.call(ctx, instanceID, cmd, &s); err != nil {
		return nil, err
	}
	if s.Position.InstanceID != instanceID || s.Position.Epoch == "" || (s.Instance.ID != "" && s.Instance.ID != instanceID) {
		return nil, fmt.Errorf("%s: %w: snapshot does not identify the target", cmdGetSnapshot, coord.ErrInvalidFrame)
	}
	return &s, nil
}

// History fetches the page of sessionID's persisted conversation before the
// entry ID before; empty fetches the newest page.
func (c *CoordinatedClient) History(ctx context.Context, instanceID, sessionID, before string) (*History, error) {
	return c.history(ctx, instanceID, sessionID, before, false)
}

// HistoryPreview fetches a history page whose images are referenced by entry ID.
func (c *CoordinatedClient) HistoryPreview(ctx context.Context, instanceID, sessionID, before string) (*History, error) {
	return c.history(ctx, instanceID, sessionID, before, true)
}

func (c *CoordinatedClient) history(ctx context.Context, instanceID, sessionID, before string, preview bool) (*History, error) {
	if instanceID == "" {
		return nil, ErrTargetRequired
	}
	if sessionID == "" {
		return nil, fmt.Errorf("%s: session id required", cmdGetHistory)
	}
	args := struct {
		SessionID string `json:"session_id"`
		Before    string `json:"before,omitempty"`
		Preview   bool   `json:"preview,omitempty"`
	}{sessionID, before, preview}
	var h History
	if err := c.call(ctx, instanceID, Command{Type: cmdGetHistory, Args: args}, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// EntryContentRequest selects a saved entry or a live item. LiveKind can be
// "message", "tool", or "bash". ImageIndex selects a zero-based saved image.
// To continue a read, pass the previous Revision and the total bytes read.
type EntryContentRequest struct {
	SessionID  string `json:"session_id"`
	EntryID    string `json:"entry_id"`
	LiveKind   string `json:"live_kind,omitempty"`
	ImageIndex *int   `json:"image_index,omitempty"`
	Offset     int64  `json:"offset,omitempty"`
	Revision   string `json:"revision,omitempty"`
}

// EntryContentChunk contains bytes of a JSON message or live item, or the
// base64 of a selected image. More indicates another chunk is available.
type EntryContentChunk struct {
	Data     []byte `json:"data"`
	Revision string `json:"revision"`
	More     bool   `json:"more"`
}

// EntryContent reads a bounded chunk from the named instance. If the read
// expired, the worker returns CodeResyncRequired, and the caller can restart
// at offset zero rather than combining bytes from different reads.
func (c *CoordinatedClient) EntryContent(ctx context.Context, instanceID string, req EntryContentRequest) (*EntryContentChunk, error) {
	if instanceID == "" {
		return nil, ErrTargetRequired
	}
	var chunk EntryContentChunk
	if err := c.call(ctx, instanceID, Command{Type: cmdEntryContent, Args: req}, &chunk); err != nil {
		return nil, err
	}
	if chunk.More && (chunk.Revision == "" || len(chunk.Data) == 0) || req.Offset > 0 && req.Revision != chunk.Revision {
		return nil, fmt.Errorf("%s: %w: invalid content chunk", cmdEntryContent, coord.ErrInvalidFrame)
	}
	return &chunk, nil
}

// ReplayEvents fetches the page of events after the position after, from
// the instance it names. The page is verified to continue after exactly;
// otherwise, or when the worker no longer holds that position, it returns a
// *ResyncError.
func (c *CoordinatedClient) ReplayEvents(ctx context.Context, after coord.Position) (*ReplayPage, error) {
	if after.InstanceID == "" {
		return nil, ErrTargetRequired
	}
	args := struct {
		After coord.Position `json:"after"`
	}{after}
	var page ReplayPage
	if err := c.call(ctx, after.InstanceID, Command{Type: cmdReplayEvents, Args: args}, &page); err != nil {
		if rerr, ok := errors.AsType[*ResyncError](err); ok {
			rerr.Cursor = after
		}
		return nil, err
	}
	if page.More && len(page.Events) == 0 {
		return nil, fmt.Errorf("%s: %w: empty page reports more", cmdReplayEvents, coord.ErrInvalidFrame)
	}
	cursor := NewEventCursor(after)
	for _, f := range page.Events {
		if err := cursor.next(f); err != nil {
			return nil, err
		}
	}
	return &page, nil
}

// AttachSnapshot attaches to instanceID, then fetches its snapshot. Live
// events arriving meanwhile are staged rather than sent to Events: those at
// or before the snapshot position are duplicates and dropped, and the rest
// must continue it contiguously. A gap, another epoch, or overflowing the
// stage returns a *ResyncError. On error the instance stays attached.
func (c *CoordinatedClient) AttachSnapshot(ctx context.Context, instanceID string) (*Handoff, error) {
	if instanceID == "" {
		return nil, ErrTargetRequired
	}
	if err := c.beginStage(instanceID); err != nil {
		return nil, err
	}
	snap, err := c.attachSnapshot(ctx, instanceID)
	stage := c.endStage(instanceID)
	if err != nil {
		return nil, err
	}
	cursor := NewEventCursor(snap.Position)
	events, err := cursor.handoff(stage)
	if err != nil {
		return nil, err
	}
	return &Handoff{Snapshot: snap, Events: events, Cursor: cursor}, nil
}

func (c *CoordinatedClient) attachSnapshot(ctx context.Context, instanceID string) (*Snapshot, error) {
	if err := c.Attach(ctx, instanceID); err != nil {
		return nil, err
	}
	return c.Snapshot(ctx, instanceID)
}

// Resume continues a view the caller retained up to after, typically on a
// new connection. It attaches to after.InstanceID, staging live events, and
// replays pages after the saved cursor until they reach the staged events
// or the journal's end. A cursor from another epoch, one the worker no
// longer holds, a wrong target, or a gap returns a *ResyncError; rebuild
// with AttachSnapshot. On error the instance stays attached.
func (c *CoordinatedClient) Resume(ctx context.Context, after coord.Position) (*Handoff, error) {
	instanceID := after.InstanceID
	if instanceID == "" {
		return nil, ErrTargetRequired
	}
	if err := c.beginStage(instanceID); err != nil {
		return nil, err
	}
	cursor := NewEventCursor(after)
	replayed, err := c.resume(ctx, cursor)
	stage := c.endStage(instanceID)
	if err != nil {
		return nil, err
	}
	live, err := cursor.handoff(stage)
	if err != nil {
		return nil, err
	}
	return &Handoff{Events: append(replayed, live...), Cursor: cursor}, nil
}

func (c *CoordinatedClient) resume(ctx context.Context, cursor *EventCursor) ([]coord.Frame, error) {
	if err := c.Attach(ctx, cursor.position.InstanceID); err != nil {
		return nil, err
	}
	var (
		events []coord.Frame
		size   int
	)
	for {
		page, err := c.ReplayEvents(ctx, cursor.position)
		if err != nil {
			return nil, err
		}
		for _, f := range page.Events {
			size += len(f.Event)
		}
		events = append(events, page.Events...)
		if n := len(page.Events); n > 0 {
			cursor.position = *page.Events[n-1].Position
		}
		if len(events) > maxReplayFrames || size > maxReplayBytes {
			return nil, cursor.resync(nil, ErrReconstructionOverflow)
		}
		covered, overflow := c.stageStatus(cursor.position)
		if overflow {
			return nil, cursor.resync(nil, ErrReconstructionOverflow)
		}
		if !page.More || covered {
			return events, nil
		}
	}
}

// EventCursor tracks the last applied position in one instance's event
// stream.
type EventCursor struct {
	position coord.Position
}

// NewEventCursor returns a cursor after position.
func NewEventCursor(position coord.Position) *EventCursor {
	return &EventCursor{position: position}
}

// Position returns the last applied position. Save it to Resume later.
func (c *EventCursor) Position() coord.Position {
	return c.position
}

// Accept reports whether f should be applied and advances past it. Frames
// without a position, such as instance metadata, are applied without
// advancing. A frame at or before the cursor is a duplicate. A delivery
// error, omitted event, wrong target or epoch, or gap returns a *ResyncError
// without advancing.
func (c *EventCursor) Accept(f coord.Frame) (bool, error) {
	if f.InstanceID != c.position.InstanceID {
		return false, c.resync(f.Position, ErrEventTarget)
	}
	if f.Error != nil {
		return false, c.resync(f.Position, f.Error)
	}
	if f.Position == nil {
		return true, nil
	}
	p := *f.Position
	switch {
	case p.InstanceID != c.position.InstanceID:
		return false, c.resync(f.Position, ErrEventTarget)
	case p.Epoch != c.position.Epoch:
		return false, c.resync(f.Position, ErrStaleCursor)
	case p.Offset <= c.position.Offset:
		return false, nil
	case p.Offset != c.position.Offset+1:
		return false, c.resync(f.Position, ErrEventGap)
	}
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(f.Event, &event); err != nil {
		return false, c.resync(f.Position, fmt.Errorf("decode event: %w", err))
	}
	if event.Type == "event_omitted" {
		return false, c.resync(f.Position, ErrEventOmitted)
	}
	c.position = p
	return true, nil
}

// next requires f to be exactly the next positioned event.
func (c *EventCursor) next(f coord.Frame) error {
	if f.Position == nil {
		return c.resync(nil, ErrEventGap)
	}
	ok, err := c.Accept(f)
	if err != nil {
		return err
	}
	if !ok {
		return c.resync(f.Position, ErrEventGap)
	}
	return nil
}

func (c *EventCursor) handoff(s *liveStage) ([]coord.Frame, error) {
	if s.overflow {
		return nil, c.resync(nil, ErrReconstructionOverflow)
	}
	events := make([]coord.Frame, 0, len(s.frames))
	for _, f := range s.frames {
		ok, err := c.Accept(f)
		if err != nil {
			return nil, err
		}
		if ok {
			events = append(events, f)
		}
	}
	return events, nil
}

func (c *EventCursor) resync(got *coord.Position, cause error) *ResyncError {
	return &ResyncError{InstanceID: c.position.InstanceID, Cursor: c.position, Got: got, Err: cause}
}

// liveStage holds one instance's live frames, in arrival order, while its
// view is rebuilt. Guarded by CoordinatedClient.mu.
type liveStage struct {
	frames   []coord.Frame
	bytes    int
	overflow bool
}

func (s *liveStage) add(f coord.Frame) {
	if s.overflow {
		return
	}
	if len(s.frames) == maxStagedFrames || s.bytes+len(f.Event) > maxStagedBytes {
		s.overflow, s.frames = true, nil
		return
	}
	s.frames = append(s.frames, f)
	s.bytes += len(f.Event)
}

func (c *CoordinatedClient) beginStage(instanceID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	if c.stages[instanceID] != nil {
		return ErrReconstructionBusy
	}
	c.stages[instanceID] = &liveStage{}
	return nil
}

func (c *CoordinatedClient) endStage(instanceID string) *liveStage {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stages[instanceID]
	delete(c.stages, instanceID)
	return s
}

// stageStatus reports whether staged live events already continue from
// cursor, so replay can stop, and whether the stage overflowed.
func (c *CoordinatedClient) stageStatus(cursor coord.Position) (covered, overflow bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stages[cursor.InstanceID]
	if s == nil {
		return false, false
	}
	for _, f := range s.frames {
		if f.Position != nil {
			return f.Position.Epoch == cursor.Epoch && f.Position.Offset <= cursor.Offset+1, s.overflow
		}
	}
	return false, s.overflow
}

// stageEvent withholds f for an instance under reconstruction.
func (c *CoordinatedClient) stageEvent(f coord.Frame) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stages[f.InstanceID]
	if s == nil {
		return false
	}
	s.add(f)
	return true
}

// call sends a read-only worker command and decodes its result into out.
func (c *CoordinatedClient) call(ctx context.Context, instanceID string, cmd Command, out any) error {
	for attempt := 1; ; attempt++ {
		err := c.readResult(ctx, instanceID, cmd, out)
		perr, ok := errors.AsType[*coord.Error](err)
		retry := ok && (perr.Code == coord.CodeBusy || (cmd.Type == cmdGetSnapshot && perr.Code == coord.CodeResyncRequired))
		if attempt == 4 || !retry {
			return err
		}
		timer := time.NewTimer(time.Duration(50*attempt) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *CoordinatedClient) readResult(ctx context.Context, instanceID string, cmd Command, out any) error {
	reply, _, err := c.command(ctx, instanceID, "", cmd)
	if err != nil {
		if perr, ok := errors.AsType[*coord.Error](err); ok && perr.Code == coord.CodeResyncRequired {
			return &ResyncError{InstanceID: instanceID, Err: perr}
		}
		return err
	}
	if len(reply.Result) == 0 {
		return fmt.Errorf("%s: %w: empty result", cmd.Type, coord.ErrInvalidFrame)
	}
	var resp struct {
		Type   string          `json:"type"`
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *ErrorInfo      `json:"error"`
	}
	if err := json.Unmarshal(reply.Result, &resp); err != nil {
		return fmt.Errorf("decode %s: %w", cmd.Type, err)
	}
	payload := reply.Result
	if resp.Type == "response" {
		if !resp.OK {
			if resp.Error == nil {
				return fmt.Errorf("%s failed without error details", cmd.Type)
			}
			perr := &coord.Error{Code: resp.Error.Code, Message: resp.Error.Message}
			if perr.Code == coord.CodeResyncRequired {
				return &ResyncError{InstanceID: instanceID, Err: perr}
			}
			return perr
		}
		payload = resp.Result
	}
	if len(payload) == 0 || string(payload) == "null" {
		return fmt.Errorf("%s: %w: empty result", cmd.Type, coord.ErrInvalidFrame)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode %s result: %w", cmd.Type, err)
	}
	return nil
}

// command routes cmd to instanceID and returns the reply and the command ID.
func (c *CoordinatedClient) command(ctx context.Context, instanceID, requestID string, cmd Command) (coord.Frame, string, error) {
	if cmd.ID == "" {
		cmd.ID = uuid.NewString()
	}
	raw, err := json.Marshal(cmd)
	if err != nil {
		return coord.Frame{}, "", fmt.Errorf("marshal command: %w", err)
	}
	reply, err := c.targeted(ctx, coord.Frame{Type: coord.TypeCommand, InstanceID: instanceID, RequestID: requestID, Command: raw})
	return reply, cmd.ID, err
}

// commandResponse accepts either a full worker response or a bare result
// object from the instance.
func commandResponse(id string, result json.RawMessage) (*Response, error) {
	resp := &Response{Type: "response", ID: id, OK: true}
	if len(result) == 0 || string(result) == "null" {
		return resp, nil
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(result, &probe); err != nil {
		return nil, fmt.Errorf("decode command result: %w", err)
	}
	if probe.Type == "response" {
		if err := json.Unmarshal(result, resp); err != nil {
			return nil, fmt.Errorf("decode command response: %w", err)
		}
		resp.ID = id
		return resp, nil
	}
	if err := json.Unmarshal(result, &resp.Result); err != nil {
		return nil, fmt.Errorf("decode command result: %w", err)
	}
	return resp, nil
}

func requestIDOrNew(id string) (string, error) {
	if id == "" {
		return uuid.NewString(), nil
	}
	if !coord.ValidRequestID(id) {
		return "", ErrInvalidRequestID
	}
	return id, nil
}

func (c *CoordinatedClient) targeted(ctx context.Context, f coord.Frame) (coord.Frame, error) {
	if f.InstanceID == "" {
		return coord.Frame{}, ErrTargetRequired
	}
	return c.request(ctx, f)
}

// request sends f under a fresh ID and waits for the matching response.
// Registration and send happen under one lock so a reply can never arrive
// for an unregistered ID.
func (c *CoordinatedClient) request(ctx context.Context, f coord.Frame) (coord.Frame, error) {
	if err := ctx.Err(); err != nil {
		return coord.Frame{}, err
	}
	f.ID = uuid.NewString()
	reply := make(chan coord.Frame, 1)

	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return coord.Frame{}, err
	}
	if len(c.pending) >= coord.MaxPending {
		c.mu.Unlock()
		return coord.Frame{}, ErrTooManyPending
	}
	if err := c.link.Send(f); err != nil {
		c.mu.Unlock()
		if !errors.Is(err, coord.ErrFrameTooLarge) {
			c.fail(fmt.Errorf("send %s: %w", f.Type, err))
		}
		return coord.Frame{}, fmt.Errorf("send %s: %w", f.Type, err)
	}
	c.pending[f.ID] = reply
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, f.ID)
		c.mu.Unlock()
	}()

	select {
	case r := <-reply:
		return replyResult(f, r)
	case <-c.closed:
		select {
		case r := <-reply:
			return replyResult(f, r)
		default:
		}
		return coord.Frame{}, outcomeUnknown(f, c.Err())
	case <-ctx.Done():
		return coord.Frame{}, outcomeUnknown(f, ctx.Err())
	}
}

func outcomeUnknown(f coord.Frame, cause error) *OutcomeUnknownError {
	return &OutcomeUnknownError{Op: f.Type, InstanceID: f.InstanceID, RequestID: f.RequestID, Err: cause}
}

func replyResult(sent, r coord.Frame) (coord.Frame, error) {
	if r.Error != nil {
		if r.Error.Code == coord.CodeOutcomeUnknown {
			return coord.Frame{}, outcomeUnknown(sent, r.Error)
		}
		return coord.Frame{}, r.Error
	}
	return r, nil
}

func (c *CoordinatedClient) readLoop() {
	defer close(c.done)
	defer close(c.events)
	for {
		f, err := c.link.Read()
		if err != nil {
			c.fail(fmt.Errorf("read coordinator: %w", err))
			return
		}
		switch f.Type {
		case coord.TypeResponse:
			c.resolve(f)
		case coord.TypeEvent, coord.TypeInstanceUpdate:
			if f.Type == coord.TypeEvent && c.stageEvent(f) {
				continue
			}
			select {
			case c.events <- f:
			default:
				c.fail(ErrSlowEventConsumer)
				if err := c.link.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					c.fail(err)
				}
				return
			}
		}
	}
}

func (c *CoordinatedClient) resolve(f coord.Frame) {
	c.mu.Lock()
	reply, ok := c.pending[f.ID]
	delete(c.pending, f.ID)
	c.mu.Unlock()
	if ok {
		reply <- f
	}
}

// fail records the first disconnect reason and releases every waiter.
func (c *CoordinatedClient) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.closeOnce.Do(func() { close(c.closed) })
}
