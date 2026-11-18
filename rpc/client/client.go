// Package client provides a Go client library for the tau rpc protocol.
// Embedders can spawn `tau rpc` as a subprocess and use this client to
// communicate with it over JSONL on stdio.
package client // import "github.com/losinggeneration/tau/rpc/client"

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Command represents a client-to-server command.
type Command struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Args any    `json:"args,omitempty"`
}

// Response represents a server-to-client response.
type Response struct {
	Type   string         `json:"type"`
	ID     string         `json:"id"`
	OK     bool           `json:"ok"`
	Result map[string]any `json:"result,omitempty"`
	Error  *ErrorInfo     `json:"error,omitempty"`
}

// ErrorInfo contains error details for failed responses.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Event represents a server-to-client event (no ID correlation).
type Event struct {
	Type      string         `json:"type"`
	SessionID string         `json:"session_id,omitempty"`
	Seq       int64          `json:"seq,omitempty"`
	TS        string         `json:"ts,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
	// Typed payload fields for fidelity preservation
	Delta      string `json:"delta,omitempty"`        // text_delta, thinking_delta
	MessageID  string `json:"message_id,omitempty"`   // message metadata
	ToolCallID string `json:"tool_call_id,omitempty"` // tool_call, tool_result
	Tokens     int64  `json:"tokens,omitempty"`       // usage events
	Level      string `json:"level,omitempty"`        // thinking_level_change
}

// FromJSON parses a server event from raw JSON bytes, extracting flattened
// payload fields into the typed struct while preserving the full payload map.
func (e *Event) FromJSON(data []byte) error {
	// First unmarshal into a raw map to capture all fields
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Extract top-level fields
	if v, ok := raw["type"].(string); ok {
		e.Type = v
	}
	if v, ok := raw["session_id"].(string); ok {
		e.SessionID = v
	}
	if v, ok := raw["seq"].(float64); ok {
		e.Seq = int64(v)
	}
	if v, ok := raw["ts"].(string); ok {
		e.TS = v
	}

	// Extract typed payload fields (flattened by server)
	if v, ok := raw["delta"].(string); ok {
		e.Delta = v
	}
	if v, ok := raw["message_id"].(string); ok {
		e.MessageID = v
	}
	if v, ok := raw["tool_call_id"].(string); ok {
		e.ToolCallID = v
	}
	if v, ok := raw["tokens"].(float64); ok {
		e.Tokens = int64(v)
	}
	if v, ok := raw["level"].(string); ok {
		e.Level = v
	}

	// Build payload map excluding top-level envelope fields
	payload := make(map[string]any)
	skipKeys := map[string]bool{
		"type": true, "session_id": true, "seq": true, "ts": true,
	}
	for k, v := range raw {
		if !skipKeys[k] {
			payload[k] = v
		}
	}
	if len(payload) > 0 {
		e.Payload = payload
	}

	return nil
}

// Client is a tau rpc client. It spawns a `tau rpc` subprocess and
// communicates with it over JSONL on stdio.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr io.Reader

	// Pending requests correlating ID to response channel
	pending map[string]chan<- Response
	mu      sync.Mutex

	// Event channel for server-emitted events
	events chan Event
	done   chan struct{}
}

// NewClient spawns a new tau rpc subprocess.
func NewClient() (*Client, error) {
	return NewClientWithArgs("tau", []string{"rpc"})
}

// NewClientWithArgs spawns a tau rpc subprocess with custom arguments.
// It creates subprocess pipes for stdin/stdout so the client can communicate
// with the tau rpc process without conflicting with the embedder's own stdin/stdout.
// If you want to run tau rpc with its stdin/stdout connected to the embedder's
// streams, use NewClientWithStdio(executable, args) instead.
func NewClientWithArgs(executable string, args []string) (*Client, error) {
	cmd := exec.Command(executable, args...)

	// Create pipes for subprocess communication
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		if closeErr := stdinPipe.Close(); closeErr != nil {
			return nil, fmt.Errorf("stdout pipe: %w; close stdin pipe: %w", err, closeErr)
		}
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		if closeErr := stdinPipe.Close(); closeErr != nil {
			return nil, fmt.Errorf("start tau rpc: %w; close stdin pipe: %w", err, closeErr)
		}
		if closeErr := stdoutPipe.Close(); closeErr != nil {
			return nil, fmt.Errorf("start tau rpc: %w; close stdout pipe: %w", err, closeErr)
		}
		return nil, fmt.Errorf("start tau rpc: %w", err)
	}

	c := &Client{
		cmd:     cmd,
		stdin:   stdinPipe,
		stdout:  bufio.NewReader(stdoutPipe),
		pending: make(map[string]chan<- Response),
		events:  make(chan Event, 64),
		done:    make(chan struct{}),
	}

	// Start reading events from server
	go c.readEvents()

	return c, nil
}

// NewClientWithStdio spawns a tau rpc subprocess with stdin/stdout connected
// directly to os.Stdin/os.Stdout. This is useful when the embedder wants
// tau rpc to be the main interactive process (like Claude Code spawning tau rpc).
func NewUnixClient(ctx context.Context, path string) (*Client, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial tau unix socket: %w", err)
	}

	c := &Client{
		stdin:   conn,
		stdout:  bufio.NewReader(conn),
		pending: make(map[string]chan<- Response),
		events:  make(chan Event, 64),
		done:    make(chan struct{}),
	}
	go c.readEvents()
	return c, nil
}

func NewClientWithStdio(executable string, args []string) (*Client, error) {
	cmd := exec.Command(executable, args...)
	// Connect directly to embedder's streams
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start tau rpc: %w", err)
	}

	// The client reads from os.Stdin (where tau rpc writes responses)
	// and writes to os.Stdin (where tau rpc reads commands)
	c := &Client{
		cmd:     cmd,
		stdin:   os.Stdin,
		stdout:  bufio.NewReader(os.Stdin),
		stderr:  os.Stderr,
		pending: make(map[string]chan<- Response),
		events:  make(chan Event, 64),
		done:    make(chan struct{}),
	}

	// Start reading events from server
	go c.readEvents()

	return c, nil
}

// readEvents continuously reads events and responses from stdout.
func (c *Client) readEvents() {
	defer close(c.events)
	defer close(c.done)

	scanner := bufio.NewScanner(c.stdout)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Try to determine if it's a response or event
		var base struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(line, &base); err != nil {
			continue
		}

		// ui_* events also carry an id, so only the type identifies responses.
		if base.Type == "response" {
			var resp Response
			if err := json.Unmarshal(line, &resp); err != nil {
				continue
			}
			c.mu.Lock()
			if ch, ok := c.pending[resp.ID]; ok {
				select {
				case ch <- resp:
				default:
				}
				delete(c.pending, resp.ID)
			}
			c.mu.Unlock()
		} else if IsClientEventType(base.Type) {
			// It's an event — use FromJSON to preserve flattened payload fields
			var evt Event
			if err := evt.FromJSON(line); err != nil {
				continue
			}
			select {
			case c.events <- evt:
			default:
			}
		}
	}
}

// Events returns a channel of server-emitted events.
func (c *Client) Events() <-chan Event {
	return c.events
}

// IsClientEventType reports whether a server-originated frame with the given
// `type` field should be surfaced to client.Events(). Frames with other
// types are silently dropped (e.g. internal heartbeats).
func IsClientEventType(t string) bool {
	switch t {
	case "response", "meta",
		"session_start", "session_end", "session_shutdown",
		"message_start", "message_end",
		"text_delta", "thinking_delta",
		"tool_call", "tool_progress", "tool_result",
		"usage", "error", "interrupt",
		"model_change", "thinking_level_change",
		"queue_update", "compaction_start", "compaction_end", "paused",
		"ui_confirm", "ui_select", "ui_input",
		"ui_notify", "ui_set_status", "ui_set_widget":
		return true
	}
	return false
}

// sendCommand sends a command and waits for a response.
func (c *Client) sendCommand(ctx context.Context, cmd Command) (*Response, error) {
	respCh := make(chan Response, 1)

	c.mu.Lock()
	c.pending[cmd.ID] = respCh
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, cmd.ID)
		c.mu.Unlock()
	}()

	// Marshal and send
	data, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshal command: %w", err)
	}
	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write command: %w", err)
	}

	// Wait for response
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-respCh:
		return &resp, nil
	}
}

// nextID generates a unique command ID.
func nextID() string {
	return fmt.Sprintf("cmd-%d", time.Now().UnixNano())
}

// SendMessage sends a user message and returns a response.
func (c *Client) SendMessage(ctx context.Context, text string) (*Response, error) {
	cmd := Command{
		ID:   nextID(),
		Type: "send_message",
		Args: map[string]any{"text": text},
	}
	return c.sendCommand(ctx, cmd)
}

// Interrupt cancels the in-flight assistant response.
func (c *Client) Interrupt(ctx context.Context) (*Response, error) {
	cmd := Command{
		ID:   nextID(),
		Type: "interrupt",
		Args: map[string]any{},
	}
	return c.sendCommand(ctx, cmd)
}

// SlashCommand invokes a slash command.
func (c *Client) SlashCommand(ctx context.Context, name, args string) (*Response, error) {
	cmd := Command{
		ID:   nextID(),
		Type: "slash_command",
		Args: map[string]any{"name": name, "args": args},
	}
	return c.sendCommand(ctx, cmd)
}

// GetState returns the current session state.
func (c *Client) GetState(ctx context.Context) (*Response, error) {
	cmd := Command{
		ID:   nextID(),
		Type: "get_state",
		Args: map[string]any{},
	}
	return c.sendCommand(ctx, cmd)
}

// SetModel switches the active model.
func (c *Client) SetModel(ctx context.Context, modelSpec string) (*Response, error) {
	cmd := Command{
		ID:   nextID(),
		Type: "set_model",
		Args: map[string]any{"model_spec": modelSpec},
	}
	return c.sendCommand(ctx, cmd)
}

// ListSessions returns the list of sessions.
func (c *Client) ListSessions(ctx context.Context, limit int) (*Response, error) {
	cmd := Command{
		ID:   nextID(),
		Type: "list_sessions",
		Args: map[string]any{"limit": limit},
	}
	return c.sendCommand(ctx, cmd)
}

// ResumeSession switches to an existing session.
func (c *Client) ResumeSession(ctx context.Context, id string) (*Response, error) {
	cmd := Command{
		ID:   nextID(),
		Type: "resume_session",
		Args: map[string]any{"id": id},
	}
	return c.sendCommand(ctx, cmd)
}

// NewSession starts a fresh session.
func (c *Client) NewSession(ctx context.Context, cwd string) (*Response, error) {
	cmd := Command{
		ID:   nextID(),
		Type: "new_session",
		Args: map[string]any{"cwd": cwd},
	}
	return c.sendCommand(ctx, cmd)
}

// Compact triggers compaction.
func (c *Client) Compact(ctx context.Context) (*Response, error) {
	cmd := Command{
		ID:   nextID(),
		Type: "compact",
		Args: map[string]any{},
	}
	return c.sendCommand(ctx, cmd)
}

// Shutdown gracefully exits the server.
func (c *Client) Shutdown(ctx context.Context) (*Response, error) {
	cmd := Command{
		ID:   nextID(),
		Type: "shutdown",
		Args: map[string]any{},
	}
	return c.sendCommand(ctx, cmd)
}

// UIResponse replies to a ui_* event.
func (c *Client) UIResponse(ctx context.Context, id, value string) (*Response, error) {
	cmd := Command{
		ID:   nextID(),
		Type: "ui_response",
		Args: map[string]any{"id": id, "value": value},
	}
	return c.sendCommand(ctx, cmd)
}

// SetThinkingLevel changes the thinking level mid-session.
func (c *Client) SetThinkingLevel(ctx context.Context, level string) (*Response, error) {
	cmd := Command{
		ID:   nextID(),
		Type: "set_thinking_level",
		Args: map[string]any{"level": level},
	}
	return c.sendCommand(ctx, cmd)
}

// Close terminates the tau rpc subprocess and reaps it.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}

	var errs []error
	if c.stdin != nil {
		if err := c.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, fmt.Errorf("close stdin: %w", err))
		}
	}

	if c.cmd != nil && c.cmd.Process != nil {
		if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			errs = append(errs, fmt.Errorf("kill process: %w", err))
		}

		waitErr := c.cmd.Wait()
		if waitErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) && !errors.Is(waitErr, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("wait process: %w", waitErr))
			}
		}
	}

	select {
	case <-c.done:
	case <-time.After(time.Second):
		errs = append(errs, fmt.Errorf("wait for reader shutdown: timeout"))
	}

	return errors.Join(errs...)
}
