// Package coord implements the local multi-instance coordinator and its
// JSONL wire protocol.
package coord

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	TypeSessionDirectory = "session_directory"
	TypeLocateSession    = "locate_session"
	TypeWatchInstances   = "watch_instances"
	TypeUnwatchInstances = "unwatch_instances"
	TypeInstanceUpdate   = "instance_update"
	// TypeInstanceReplacement tells a client attached to a restarting worker
	// that it is going away. A second frame carries the replacement in Instance.
	TypeInstanceReplacement = OperationInstanceReplacement
	TypeStartInstance       = "start_instance"
	TypeAttach              = "attach"
	TypeDetach              = "detach"
	TypeCommand             = "command"
	TypeStopInstance        = "stop_instance"
	TypeResponse            = "response"
	TypeEvent               = "event"
	TypeRegister            = "register"
	TypePing                = "ping"
	TypePong                = "pong"
)

// OperationSavedSessionLaunch advertises that start_instance honors session_id.
const OperationSavedSessionLaunch = "start_saved_session"

// OperationInstancePrompts opts clients into prompt previews and tells workers
// the coordinator accepts prompt previews in registrations.
const OperationInstancePrompts = "instance_prompts"

// OperationInstanceNames opts clients into session names and tells workers
// the coordinator accepts them.
const OperationInstanceNames = "instance_names"

// OperationInstanceBuild opts local clients into the outdated indicator and
// allows workers to report their build identity in registrations.
const OperationInstanceBuild = "instance_build"

// OperationInstanceRestart advertises session-preserving shutdown support.
const OperationInstanceRestart = "instance_restart"

// OperationInstanceReplacement opts clients into instance_replacement frames.
const OperationInstanceReplacement = "instance_replacement"

// OperationInstanceRename opts clients into instance names and tells workers
// the coordinator accepts them.
const OperationInstanceRename = "instance_rename"

// MaxInstanceNameBytes bounds a user-assigned instance name.
const MaxInstanceNameBytes = 128

// MaxInstancePromptBytes bounds a preview before JSON escaping.
const MaxInstancePromptBytes = 256

// Connection roles advertised in hello.
const (
	RoleClient = "client"
	RoleWorker = "worker"
)

// Worker states reported in Instance summaries.
const (
	// StateAwaitingUser means the run finished and it's the user's turn.
	StateAwaitingUser = "awaiting_user"
	StateWorking      = "working"
	// StateNeedsInput means a run is blocked on a dialog answer.
	StateNeedsInput = "needs_input"
	// StateUsageLimited means a run is asleep until a provider quota resets.
	// Switching models wakes it early.
	StateUsageLimited = "usage_limited"
	// StateRetrying means a run is backing off after a provider failure.
	StateRetrying = "retrying"
	StatePaused   = "paused"
	// StateError means the last run failed and won't continue on its own.
	StateError = "error"
)

// StateDetail explains the current State. Fields apply only to some states.
type StateDetail struct {
	Provider    string    `json:"provider,omitempty"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
	Attempt     int       `json:"attempt,omitempty"`
	MaxAttempts int       `json:"max_attempts,omitempty"`
	Target      string    `json:"target,omitempty"`
	Message     string    `json:"message,omitempty"`
}

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
	// Name labels the worker launched by start_instance.
	Name string `json:"name,omitempty"`
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
	ID              string      `json:"instance_id"`
	Name            string      `json:"name,omitempty"`
	NodeID          string      `json:"node_id,omitempty"`
	CWD             string      `json:"cwd,omitempty"`
	SessionID       string      `json:"session_id,omitempty"`
	SessionName     string      `json:"session_name,omitempty"`
	FirstPrompt     string      `json:"first_prompt,omitempty"`
	BuildID         string      `json:"build_id,omitempty"`
	Outdated        bool        `json:"outdated,omitempty"`
	State           string      `json:"state,omitempty"`
	StateDetail     StateDetail `json:"state_detail,omitzero"`
	PID             int         `json:"pid,omitempty"`
	PendingInputs   int         `json:"pending_inputs,omitempty"`
	LaunchRequestID string      `json:"launch_request_id,omitempty"`
	LinkState       string      `json:"link_state,omitempty"`
	LastActivity    time.Time   `json:"last_activity,omitzero"`
}

// Active reports whether stopping the worker would interrupt a run.
func (i *Instance) Active() bool {
	if i.PendingInputs > 0 {
		return true
	}
	switch i.State {
	case StateWorking, StateNeedsInput, StateUsageLimited, StateRetrying, StatePaused:
		return true
	}
	return false
}

// StatusText describes the state for people.
func (i *Instance) StatusText() string {
	if i.PendingInputs > 0 {
		return fmt.Sprintf("needs input (%d)", i.PendingInputs)
	}
	d := i.StateDetail
	switch i.State {
	case StateAwaitingUser:
		return "awaiting user"
	case StateNeedsInput:
		return "needs input"
	case StateUsageLimited:
		if d.ResetsAt.IsZero() {
			return "usage limit: " + d.Provider
		}
		return fmt.Sprintf("usage limit: %s, resets %s", d.Provider, d.ResetsAt.Local().Format("Mon Jan 2 15:04"))
	case StateRetrying:
		return fmt.Sprintf("retrying (%d/%d)", d.Attempt, d.MaxAttempts)
	case StatePaused:
		if d.Target == "" {
			return "paused"
		}
		return "paused: " + d.Target
	case StateError:
		if d.Message == "" {
			return "error"
		}
		return "error: " + d.Message
	case "":
		return "unknown"
	}
	return i.State
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
	case TypeHello, TypeWelcome, TypeListInstances, TypeSessionDirectory, TypeLocateSession, TypeStartInstance, TypeAttach,
		TypeDetach, TypeCommand, TypeStopInstance, TypeResponse, TypeEvent, TypeRegister,
		TypeWatchInstances, TypeUnwatchInstances, TypeInstanceUpdate, TypeInstanceReplacement, TypePing, TypePong:
		return true
	}
	return false
}

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
	if len(f.Name) > MaxInstanceNameBytes {
		return fmt.Errorf("%w: instance name exceeds %d bytes", ErrInvalidFrame, MaxInstanceNameBytes)
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
	for _, s := range []string{i.ID, i.NodeID, i.SessionID, i.State, i.LaunchRequestID, i.LinkState, i.BuildID, i.StateDetail.Provider, i.StateDetail.Target} {
		if len(s) > maxIDBytes {
			return fmt.Errorf("%w: instance identifier exceeds %d bytes", ErrInvalidFrame, maxIDBytes)
		}
	}
	if len(i.Name) > MaxInstanceNameBytes {
		return fmt.Errorf("%w: instance name exceeds %d bytes", ErrInvalidFrame, MaxInstanceNameBytes)
	}
	if len(i.FirstPrompt) > MaxInstancePromptBytes {
		return fmt.Errorf("%w: instance prompt exceeds %d bytes", ErrInvalidFrame, MaxInstancePromptBytes)
	}
	if len(i.SessionName) > MaxInstancePromptBytes {
		return fmt.Errorf("%w: instance session name exceeds %d bytes", ErrInvalidFrame, MaxInstancePromptBytes)
	}
	if len(i.StateDetail.Message) > MaxInstancePromptBytes {
		return fmt.Errorf("%w: instance state message exceeds %d bytes", ErrInvalidFrame, MaxInstancePromptBytes)
	}
	if len(i.CWD) > maxTextBytes {
		return fmt.Errorf("%w: instance cwd exceeds %d bytes", ErrInvalidFrame, maxTextBytes)
	}
	return nil
}

// NormalizeInstanceName collapses whitespace and rejects names that can't be
// displayed on one line or passed through a process environment.
func NormalizeInstanceName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("%w: instance name contains invalid characters", ErrInvalidFrame)
	}
	if len(name) > MaxInstanceNameBytes {
		return "", fmt.Errorf("%w: instance name exceeds %d bytes", ErrInvalidFrame, MaxInstanceNameBytes)
	}
	return name, nil
}

func isObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}
