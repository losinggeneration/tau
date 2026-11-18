package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/google/uuid"

	"github.com/losinggeneration/tau/rpc/coord"
)

const coordinatedEventBuffer = 256

var (
	// ErrTargetRequired reports a targeted request without an instance ID.
	// The coordinated client never falls back to an implicitly selected
	// instance.
	ErrTargetRequired = errors.New("coordinated client: instance id required")
	// ErrTooManyPending reports that coord.MaxPending requests are already in
	// flight. The request was not sent.
	ErrTooManyPending = errors.New("coordinated client: too many pending requests")
	// ErrOutcomeUnknown reports that a request was sent but no reply arrived
	// before the wait ended. The coordinator may or may not have acted on it;
	// requests are never retried automatically.
	ErrOutcomeUnknown = errors.New("coordinated client: request outcome unknown")
	// ErrSlowEventConsumer reports that Events was not drained quickly enough.
	// The client disconnects rather than dropping events.
	ErrSlowEventConsumer = errors.New("coordinated client: event consumer too slow")
	// ErrClientClosed reports use of a client after Close.
	ErrClientClosed = errors.New("coordinated client: closed")
)

// CoordinatedClient talks to a local coordinator that owns many tau
// instances. Every instance-scoped request names its target explicitly, and
// attaching or detaching only changes event subscriptions; it never stops an
// instance.
type CoordinatedClient struct {
	link     *coord.Link
	clientID string
	welcome  coord.Frame
	events   chan coord.Frame
	done     chan struct{}

	closeOnce sync.Once
	closed    chan struct{}

	mu      sync.Mutex
	err     error
	pending map[string]chan coord.Frame
}

// NewCoordinatedClient connects to the coordinator socket at path and
// completes the handshake under a freshly generated client ID.
func NewCoordinatedClient(ctx context.Context, path string) (*CoordinatedClient, error) {
	return dialCoordinated(ctx, path, coordinatedEventBuffer)
}

func dialCoordinated(ctx context.Context, path string, eventBuffer int) (*CoordinatedClient, error) {
	clientID := uuid.NewString()
	link, welcome, err := coord.Dial(ctx, path, coord.Frame{
		Type:     coord.TypeHello,
		Version:  coord.Version,
		Role:     coord.RoleClient,
		ClientID: clientID,
	})
	if err != nil {
		return nil, err
	}
	c := &CoordinatedClient{
		link:     link,
		clientID: clientID,
		welcome:  welcome,
		events:   make(chan coord.Frame, eventBuffer),
		done:     make(chan struct{}),
		closed:   make(chan struct{}),
		pending:  make(map[string]chan coord.Frame),
	}
	go c.readLoop()
	return c, nil
}

// ClientID returns the ID this client announced in its hello.
func (c *CoordinatedClient) ClientID() string {
	return c.clientID
}

// Welcome returns the coordinator's handshake reply, including its
// generation and the client ID it assigned.
func (c *CoordinatedClient) Welcome() coord.Frame {
	return c.welcome
}

// Events returns targeted event frames from attached instances. Each frame
// carries InstanceID and SessionID; Event holds the raw tau rpc event, which
// Event.FromJSON can decode. A frame with Error set reports that the instance
// became unavailable, and a frame with Instance set reports updated metadata.
// The channel closes when the client disconnects; Err then reports why.
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

// ListInstances returns every instance registered with the coordinator.
func (c *CoordinatedClient) ListInstances(ctx context.Context) ([]coord.Instance, error) {
	reply, err := c.request(ctx, coord.Frame{Type: coord.TypeListInstances})
	if err != nil {
		return nil, err
	}
	return reply.Instances, nil
}

// StartInstance asks the coordinator to launch an instance rooted at cwd,
// which must be absolute, and waits for it to register.
func (c *CoordinatedClient) StartInstance(ctx context.Context, cwd string) (*coord.Instance, error) {
	reply, err := c.request(ctx, coord.Frame{Type: coord.TypeStartInstance, CWD: cwd})
	if err != nil {
		return nil, err
	}
	if reply.Instance == nil {
		return nil, fmt.Errorf("start instance: %w: reply missing instance", coord.ErrInvalidFrame)
	}
	return reply.Instance, nil
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

// Command sends a tau rpc command to instanceID and returns the instance's
// response. A missing cmd.ID is filled in. Routing failures are returned as
// *coord.Error; command failures are reported in the Response.
func (c *CoordinatedClient) Command(ctx context.Context, instanceID string, cmd Command) (*Response, error) {
	if instanceID == "" {
		return nil, ErrTargetRequired
	}
	if cmd.ID == "" {
		cmd.ID = uuid.NewString()
	}
	raw, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshal command: %w", err)
	}
	reply, err := c.targeted(ctx, coord.Frame{Type: coord.TypeCommand, InstanceID: instanceID, Command: raw})
	if err != nil {
		return nil, err
	}
	return commandResponse(cmd.ID, reply.Result)
}

// StopInstance asks instanceID to shut down.
func (c *CoordinatedClient) StopInstance(ctx context.Context, instanceID string) error {
	reply, err := c.targeted(ctx, coord.Frame{Type: coord.TypeStopInstance, InstanceID: instanceID})
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

// commandResponse accepts either a full tau rpc response or a bare result
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
		return replyResult(r)
	case <-c.closed:
		select {
		case r := <-reply:
			return replyResult(r)
		default:
		}
		return coord.Frame{}, fmt.Errorf("%s: %w: %w", f.Type, ErrOutcomeUnknown, c.Err())
	case <-ctx.Done():
		return coord.Frame{}, fmt.Errorf("%s: %w: %w", f.Type, ErrOutcomeUnknown, ctx.Err())
	}
}

func replyResult(r coord.Frame) (coord.Frame, error) {
	if r.Error != nil {
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
		case coord.TypeEvent:
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
