// Package coord implements the local multi-instance coordinator and its
// JSONL wire protocol.
package coord

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

const (
	// Version is the coordinated protocol version negotiated in hello.
	Version = 1
	// MaxFrameBytes bounds one encoded frame, including its newline.
	MaxFrameBytes = 1 << 20
	// MaxPending bounds in-flight requests per client connection.
	MaxPending = 128
)

// Frame types.
const (
	TypeHello            = "hello"
	TypeWelcome          = "welcome"
	TypeListInstances    = "list_instances"
	TypeWatchInstances   = "watch_instances"
	TypeUnwatchInstances = "unwatch_instances"
	TypeInstanceUpdate   = "instance_update"
	TypeStartInstance    = "start_instance"
	TypeAttach           = "attach"
	TypeDetach           = "detach"
	TypeCommand          = "command"
	TypeStopInstance     = "stop_instance"
	TypeResponse         = "response"
	TypeEvent            = "event"
	TypeRegister         = "register"
	TypePing             = "ping"
	TypePong             = "pong"
)

// Connection roles advertised in hello.
const (
	RoleClient = "client"
	RoleWorker = "worker"
)

// Worker states reported in Instance summaries.
const (
	StateReady = "ready"
	StateBusy  = "busy"
)

// Worker link states are separate from the agent lifecycle.
const (
	LinkConnected    = "connected"
	LinkDisconnected = "disconnected"
	LinkExited       = "exited"
)

// Error codes.
const (
	CodeInvalidFrame       = "invalid_frame"
	CodeUnsupportedVersion = "unsupported_version"
	CodeInvalidRole        = "invalid_role"
	CodeUnauthorized       = "unauthorized"
	CodeNotFound           = "not_found"
	CodeDuplicateID        = "duplicate_id"
	CodeTooManyPending     = "too_many_pending"
	CodeLimitExceeded      = "limit_exceeded"
	CodeLaunchFailed       = "launch_failed"
	CodeUnavailable        = "unavailable"
	CodeOutcomeUnknown     = "outcome_unknown"
	CodeResyncRequired     = "resync_required"
	CodeWorkerLinkLost     = "worker_link_lost"
	CodeConflict           = "conflict"
	CodeBusy               = "busy"
	// CodeClockSkew definitely refuses a launch request ID whose timestamp is
	// outside the receipt window although it was never recorded. Nothing was
	// started; a new ID from a corrected clock may be used.
	CodeClockSkew = "clock_skew"
	// CodeLegacyProtocol answers a legacy RPC command sent in place of hello.
	CodeLegacyProtocol = "unsupported_protocol"
)

const (
	maxIDBytes   = 256
	maxTextBytes = 4096
	maxInstances = maxWorkers
)

var (
	// ErrInvalidFrame reports a malformed or out-of-bounds frame.
	ErrInvalidFrame = errors.New("invalid frame")
	// ErrFrameTooLarge reports a frame exceeding MaxFrameBytes.
	ErrFrameTooLarge = errors.New("frame too large")
)

// ValidRequestID reports whether id is a canonical UUIDv4. It grants no authority.
func ValidRequestID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.Version() == 4 && parsed.Variant() == uuid.RFC4122 && parsed.String() == id
}

// ValidLaunchRequestID reports whether id is a canonical UUIDv7 or a legacy
// UUIDv4. Only UUIDv7 can reserve a new intent on a recoverable coordinator.
func ValidLaunchRequestID(id string) bool {
	return ValidRequestID(id) || !launchTime(id).IsZero()
}

func launchTime(id string) time.Time {
	u, err := uuid.Parse(id)
	if err != nil || u.Version() != 7 || u.Variant() != uuid.RFC4122 || u.String() != id {
		return time.Time{}
	}
	sec, ns := u.Time().UnixTime()
	return time.Unix(sec, ns)
}

// ValidOperationRequestID checks the correlation ID for a client operation.
func ValidOperationRequestID(operation, id string) bool {
	if operation == TypeStartInstance {
		return ValidLaunchRequestID(id)
	}
	return ValidRequestID(id)
}

// Frame is one JSONL message exchanged with the coordinator.
type Frame struct {
	Type       string          `json:"type"`
	ID         string          `json:"id,omitempty"`
	RequestID  string          `json:"request_id,omitempty"`
	Position   *Position       `json:"position,omitempty"`
	Version    int             `json:"version,omitempty"`
	Role       string          `json:"role,omitempty"`
	Generation string          `json:"generation,omitempty"`
	PID        int             `json:"pid,omitempty"`
	InstanceID string          `json:"instance_id,omitempty"`
	SessionID  string          `json:"session_id,omitempty"`
	CWD        string          `json:"cwd,omitempty"`
	ClientID   string          `json:"client_id,omitempty"`
	Token      string          `json:"token,omitempty"`
	Command    json.RawMessage `json:"command,omitempty"`
	Event      json.RawMessage `json:"event,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *Error          `json:"error,omitempty"`
	Instance   *Instance       `json:"instance,omitempty"`
	Instances  []Instance      `json:"instances,omitempty"`
	Operations []string        `json:"operations,omitempty"`
	Limits     *Limits         `json:"limits,omitempty"`
	// LaunchRequestVersion advertises the preferred UUID version for new starts.
	// An absent value means UUIDv4.
	LaunchRequestVersion int `json:"launch_request_version,omitempty"`
	// CoordinatorTimeUnixMS is the coordinator's wall clock when it sent a
	// client welcome. Clients derive UUIDv7 launch IDs from it plus their own
	// monotonic elapsed time, so their wall clock does not matter. An absent
	// value means an older coordinator; use the local clock.
	CoordinatorTimeUnixMS int64 `json:"coordinator_time_unix_ms,omitempty"`
}

// Position identifies a point in one worker's event stream. Offset zero is
// the position before the first event, not a missing position.
type Position struct {
	InstanceID string `json:"instance_id"`
	Epoch      string `json:"epoch"`
	Offset     uint64 `json:"offset"`
}

// Limits describes the coordinator's negotiated resource budgets.
type Limits struct {
	FrameBytes         int   `json:"frame_bytes"`
	PendingRequests    int   `json:"pending_requests"`
	OutboundFrames     int   `json:"outbound_frames"`
	OutboundBytes      int   `json:"outbound_bytes"`
	Clients            int   `json:"clients"`
	Workers            int   `json:"workers"`
	ConcurrentLaunches int   `json:"concurrent_launches"`
	WriteTimeoutMS     int64 `json:"write_timeout_ms"`
	HandshakeTimeoutMS int64 `json:"handshake_timeout_ms"`
}

func protocolLimits() *Limits {
	return &Limits{FrameBytes: MaxFrameBytes, PendingRequests: MaxPending, OutboundFrames: maxQueuedFrames, OutboundBytes: maxQueuedBytes, Clients: maxClients, Workers: maxWorkers, ConcurrentLaunches: maxLaunches, WriteTimeoutMS: writeTimeout.Milliseconds(), HandshakeTimeoutMS: handshakeTimeout.Milliseconds()}
}

// Error is a structured protocol error.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// Error implements error so protocol errors can be matched with errors.As.
func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// Instance summarizes one registered worker.
type Instance struct {
	ID              string    `json:"instance_id"`
	NodeID          string    `json:"node_id,omitempty"`
	CWD             string    `json:"cwd,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
	State           string    `json:"state,omitempty"`
	PID             int       `json:"pid,omitempty"`
	PendingInputs   int       `json:"pending_inputs,omitempty"`
	LaunchRequestID string    `json:"launch_request_id,omitempty"`
	LinkState       string    `json:"link_state,omitempty"`
	LastActivity    time.Time `json:"last_activity,omitzero"`
}

func decodeFrame(line []byte) (Frame, error) {
	var f Frame
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Frame{}, fmt.Errorf("%w: %w", ErrInvalidFrame, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Frame{}, fmt.Errorf("%w: trailing data", ErrInvalidFrame)
	}
	if err := f.validate(); err != nil {
		return Frame{}, err
	}
	return f, nil
}

func isFrameType(t string) bool {
	switch t {
	case TypeHello, TypeWelcome, TypeListInstances, TypeStartInstance, TypeAttach,
		TypeDetach, TypeCommand, TypeStopInstance, TypeResponse, TypeEvent, TypeRegister,
		TypeWatchInstances, TypeUnwatchInstances, TypeInstanceUpdate, TypePing, TypePong:
		return true
	}
	return false
}

// legacyRejection answers a first line that is a connection-owned RPC
// command rather than a hello, in that protocol's response shape. It returns
// nil for anything else so the strict handshake stays silent.
func legacyRejection(line []byte) ([]byte, error) {
	var cmd struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &cmd) != nil || cmd.ID == "" || cmd.Type == "" || len(cmd.ID) > maxIDBytes || len(cmd.Type) > maxIDBytes || isFrameType(cmd.Type) {
		return nil, nil
	}
	data, err := json.Marshal(legacyResponse{
		Type:  TypeResponse,
		ID:    cmd.ID,
		Error: &Error{Code: CodeLegacyProtocol, Message: legacyGuidance},
	})
	if err != nil {
		return nil, fmt.Errorf("encode legacy rejection: %w", err)
	}
	return append(data, '\n'), nil
}

type legacyResponse struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error *Error `json:"error"`
}

const legacyGuidance = "this socket serves coordinated protocol v1 and requires a hello handshake; " +
	"connection-owned RPC clients need a daemon started with `tau daemon --legacy`"

func (f *Frame) validate() error {
	if !isFrameType(f.Type) {
		return fmt.Errorf("%w: unknown type %.32q", ErrInvalidFrame, f.Type)
	}
	for _, s := range []string{f.ID, f.Role, f.Generation, f.InstanceID, f.SessionID, f.ClientID, f.Token, f.RequestID} {
		if len(s) > maxIDBytes {
			return fmt.Errorf("%w: identifier exceeds %d bytes", ErrInvalidFrame, maxIDBytes)
		}
	}
	if f.Position != nil && (len(f.Position.InstanceID) > maxIDBytes || len(f.Position.Epoch) > maxIDBytes) {
		return fmt.Errorf("%w: event position exceeds limits", ErrInvalidFrame)
	}
	if len(f.CWD) > maxTextBytes {
		return fmt.Errorf("%w: cwd exceeds %d bytes", ErrInvalidFrame, maxTextBytes)
	}
	if f.CoordinatorTimeUnixMS < 0 {
		return fmt.Errorf("%w: negative coordinator time", ErrInvalidFrame)
	}
	if f.Error != nil && (len(f.Error.Code) > maxIDBytes || len(f.Error.Message) > maxTextBytes) {
		return fmt.Errorf("%w: error exceeds limits", ErrInvalidFrame)
	}
	if len(f.Operations) > 16 {
		return fmt.Errorf("%w: too many operations", ErrInvalidFrame)
	}
	for _, op := range f.Operations {
		if len(op) > maxIDBytes {
			return fmt.Errorf("%w: operation exceeds limit", ErrInvalidFrame)
		}
	}
	if f.Instance != nil {
		if err := f.Instance.validate(); err != nil {
			return err
		}
	}
	if len(f.Instances) > maxInstances {
		return fmt.Errorf("%w: more than %d instances", ErrInvalidFrame, maxInstances)
	}
	for i := range f.Instances {
		if err := f.Instances[i].validate(); err != nil {
			return err
		}
	}
	return nil
}

func (i *Instance) validate() error {
	if i.PendingInputs < 0 {
		return fmt.Errorf("%w: negative pending input count", ErrInvalidFrame)
	}
	for _, s := range []string{i.ID, i.NodeID, i.SessionID, i.State, i.LaunchRequestID, i.LinkState} {
		if len(s) > maxIDBytes {
			return fmt.Errorf("%w: instance identifier exceeds %d bytes", ErrInvalidFrame, maxIDBytes)
		}
	}
	if len(i.CWD) > maxTextBytes {
		return fmt.Errorf("%w: instance cwd exceeds %d bytes", ErrInvalidFrame, maxTextBytes)
	}
	return nil
}

func isObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}
