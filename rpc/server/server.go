// Package server implements the tau rpc daemon mode.
// JSONL on stdio with typed commands and event streams.
package server // import "github.com/losinggeneration/tau/rpc/server"

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/agent/artifacts"
	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
)

// Command represents a client-to-server command.
type Command struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Response envelope sent back to client.
type Response struct {
	Type   string     `json:"type"`
	ID     string     `json:"id"`
	OK     bool       `json:"ok"`
	Result any        `json:"result,omitempty"`
	Error  *ErrorInfo `json:"error,omitempty"`
}

// ErrorInfo contains error details for failed responses.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// UIResponse is a client reply to a ui_* event.
type UIResponse struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// Server handles the tau rpc protocol.
type Server struct {
	runtime         sdk.Runtime
	pluginManager   *plugins.Manager
	ui              *ui.RPCUI
	reader          *bufio.Reader
	writer          io.Writer
	session         sdk.Session
	sessionMu       sync.Mutex
	sessionCtx      context.Context
	sessionCancel   context.CancelFunc
	interrupt       chan struct{}
	shutdown        chan struct{}
	mu              sync.Mutex
	seq             int64 // next monotonic sequence number for the active event stream
	seqSessionID    string
	interrupted     bool // true after interrupt event emitted, cleared after client sends interrupt command
	commandRegistry commands.Registry
	modelOverride   string
	thinkingLevel   string
	// activeBashCancel is the context cancel func for an in-progress bash command.
	activeBashCancel context.CancelFunc
	activeBashMu     sync.Mutex
	// bashWG joins shell commands running outside the input loop.
	bashWG sync.WaitGroup
	// promptWG joins prompt event streams running outside the input loop.
	promptWG sync.WaitGroup
	// writeMu serializes JSONL writes from commands, shells, and session events.
	writeMu sync.Mutex
	// eventMu keeps sequence assignment and publication in the same order.
	eventMu sync.Mutex
	// eventSessionID can be read by observers while sessionMu is held during Close.
	eventSessionID    atomic.Value
	sessionGeneration atomic.Uint64
	coordinated       bool
	shutdownOnce      sync.Once
}

// NewServer creates a new RPC server with the given runtime and optional
// command registry for slash-command handling.
func NewServer(runtime sdk.Runtime, cmdRegistry commands.Registry, pluginManager *plugins.Manager) *Server {
	s := &Server{
		runtime:         runtime,
		pluginManager:   pluginManager,
		reader:          bufio.NewReader(os.Stdin),
		writer:          os.Stdout,
		interrupt:       make(chan struct{}, 1),
		shutdown:        make(chan struct{}),
		commandRegistry: cmdRegistry,
	}
	return s
}

// Run starts the RPC server loop.
func (s *Server) Run(ctx context.Context) error {
	// Set up RPCUI for UI event emission
	s.ui = ui.NewRPCUI()
	s.ui.Writer = rpcUIWriter{server: s}
	if s.pluginManager != nil {
		s.pluginManager.SetUI(s.ui)
	}
	// Note: we do NOT start ReadResponse here because ui_response commands
	// come through the same stdin that we read commands from. Instead,
	// handleUIResponse dispatches directly via s.ui.DispatchResponse().

	// Emit meta event
	if err := s.emitMeta(); err != nil {
		return fmt.Errorf("emit meta: %w", err)
	}

	// Main command loop
	for {
		select {
		case <-ctx.Done():
			return s.cleanup(ctx)
		case <-s.shutdown:
			return s.cleanup(ctx)
		case <-s.interrupt:
			// SIGINT received from tau process - translate to interrupt event for client
			s.emitInterruptEvent()
		default:
		}

		// Check if we need to skip reading (we had a pending interrupt)
		if s.isInterrupted() {
			continue
		}

		cmd, err := s.readCommand()
		if err != nil {
			if err == io.EOF || errors.Is(err, io.ErrClosedPipe) {
				// Client disconnected or the transport input was closed.
				s.emitDisconnectedSessionEnd()
				return s.cleanup(ctx)
			}
			if err := s.sendError("", "read_error", fmt.Sprintf("read command: %v", err)); err != nil {
				return fmt.Errorf("send read error: %w", err)
			}
			continue
		}

		if err := s.handleCommand(ctx, cmd); err != nil {
			if sendErr := s.sendError(cmd.ID, "handler_error", fmt.Sprintf("handle command: %v", err)); sendErr != nil {
				return fmt.Errorf("send handler error: %w", sendErr)
			}
		}
	}
}

// readCommand reads and parses a command from stdin.
func (s *Server) readCommand() (*Command, error) {
	line, err := s.reader.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
		return nil, err
	}

	var cmd Command
	if err := json.Unmarshal([]byte(line), &cmd); err != nil {
		return nil, fmt.Errorf("unmarshal command: %w", err)
	}

	return &cmd, nil
}

// handleCommand dispatches the command to the appropriate handler.
func (s *Server) handleCommand(ctx context.Context, cmd *Command) error {
	switch cmd.Type {
	case "send_message":
		return s.handleSendMessage(ctx, cmd)
	case "prompt":
		// Pi-compatible alias for send_message.
		return s.handleSendMessage(ctx, cmd)
	case "interrupt":
		return s.handleInterrupt(ctx, cmd)
	case "abort":
		// Pi-compatible alias for interrupt.
		return s.handleInterrupt(ctx, cmd)
	case "slash_command":
		return s.handleSlashCommand(ctx, cmd)
	case "get_state":
		return s.handleGetState(ctx, cmd)
	case "set_model":
		return s.handleSetModel(ctx, cmd)
	case "set_thinking_level":
		return s.handleSetThinkingLevel(ctx, cmd)
	case "list_sessions":
		return s.handleListSessions(ctx, cmd)
	case "pin_session":
		return s.handlePinSession(ctx, cmd)
	case "unpin_session":
		return s.handleUnpinSession(ctx, cmd)
	case "resume_session":
		return s.handleResumeSession(ctx, cmd)
	case "new_session", "create_session":
		return s.handleNewSession(ctx, cmd)
	case "compact":
		return s.handleCompact(ctx, cmd)
	case "shutdown":
		return s.handleShutdown(ctx, cmd)
	case "ui_response":
		return s.handleUIResponse(ctx, cmd)
	case "get_ui_requests":
		return s.handleGetUIRequests(ctx, cmd)
	case "steer":
		return s.handleSteer(ctx, cmd)
	case "follow_up":
		return s.handleFollowUp(ctx, cmd)
	case "cycle_model":
		return s.handleCycleModel(ctx, cmd)
	case "cycle_thinking_level":
		return s.handleCycleThinkingLevel(ctx, cmd)
	case "set_steering_mode":
		return s.handleSetSteeringMode(ctx, cmd)
	case "set_follow_up_mode":
		return s.handleSetFollowUpMode(ctx, cmd)
	case "set_auto_compaction":
		return s.handleSetAutoCompaction(ctx, cmd)
	case "set_auto_retry":
		return s.handleSetAutoRetry(ctx, cmd)
	case "abort_retry":
		return s.handleAbortRetry(ctx, cmd)
	case "edit_context":
		return s.handleEditContext(ctx, cmd)
	case "get_messages":
		return s.handleGetMessages(ctx, cmd)
	case "get_commands":
		return s.handleGetCommands(ctx, cmd)
	case "get_session_stats":
		return s.handleGetSessionStats(ctx, cmd)
	case "export_html":
		return s.handleExportHTML(ctx, cmd)
	case "fork":
		return s.handleFork(ctx, cmd)
	case "get_fork_messages":
		return s.handleGetForkMessages(ctx, cmd)
	case "bash", "execute_bash":
		return s.startBash(ctx, cmd)
	case "abort_bash":
		return s.handleAbortBash(ctx, cmd)
	case "get_available_models":
		return s.handleGetAvailableModels(ctx, cmd)
	case "get_last_assistant_text":
		return s.handleGetLastAssistantText(ctx, cmd)
	case "set_session_name":
		return s.handleSetSessionName(ctx, cmd)
	case "attach_file":
		return s.handleAttachFile(ctx, cmd)
	case "attach_data":
		return s.handleAttachData(ctx, cmd)
	case "list_attachments":
		return s.handleListAttachments(ctx, cmd)
	case "delete_attachment":
		return s.handleDeleteAttachment(ctx, cmd)
	case "subscribe_events":
		return s.sendResult(cmd.ID, map[string]any{"ok": true})
	default:
		return s.sendError(cmd.ID, "unknown_command", fmt.Sprintf("unknown command type: %s", cmd.Type))
	}
}

func (s *Server) handleEditContext(ctx context.Context, cmd *Command) error {
	var args struct {
		EntryID     string          `json:"entry_id"`
		Replacement json.RawMessage `json:"replacement"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if args.EntryID == "" || len(args.Replacement) == 0 {
		return s.sendError(cmd.ID, "invalid_args", "entry_id and replacement are required")
	}
	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	editor, ok := sess.(sdk.ContextEditor)
	if !ok {
		return s.sendError(cmd.ID, "unsupported", "session does not support context edits")
	}
	if err := editor.EditContext(ctx, args.EntryID, args.Replacement); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if s.coordinated {
		if err := s.emitEvent(sdk.Event{Type: "context_edited", SessionID: sess.ID()}); err != nil {
			return err
		}
	}
	return s.sendResult(cmd.ID, map[string]any{"ok": true})
}

var errSessionChanged = errors.New("session changed; draft kept")

// draftSession checks the target during dispatch, after earlier session
// switches have run. Only the ordinary dispatcher changes the active session.
func (s *Server) draftSession(ctx context.Context, expected string) (sdk.Session, error) {
	if expected == "" {
		return s.ensureSession(ctx)
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.session == nil || s.session.ID() != expected {
		return nil, errSessionChanged
	}
	return s.session, nil
}

// handleSendMessage sends a user message and streams events.
func (s *Server) handleSendMessage(ctx context.Context, cmd *Command) error {
	var args struct {
		ExpectedSessionID string   `json:"expected_session_id,omitempty"`
		Text              string   `json:"text"`
		Images            []string `json:"images,omitempty"`
		AttachFiles       []string `json:"attach_files,omitempty"`
		AttachmentIDs     []string `json:"attachment_ids,omitempty"`
		// StreamingBehavior queues the prompt when a run is active:
		// "steer", or "follow_up" ("followUp" also works).
		StreamingBehavior   string `json:"streaming_behavior,omitempty"`
		StreamingBehaviorPi string `json:"streamingBehavior,omitempty"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	behavior := cmp.Or(args.StreamingBehavior, args.StreamingBehaviorPi)
	if behavior == "" && s.coordinated {
		behavior = "steer"
	}
	switch behavior {
	case "", "steer", "follow_up", "followUp":
	default:
		return s.sendError(cmd.ID, "invalid_args", fmt.Sprintf("invalid streaming_behavior %q: use steer or follow_up", behavior))
	}

	sess, err := s.draftSession(ctx, args.ExpectedSessionID)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	content, attachmentIDs, err := messageContentBlocks(sess, args.Images, args.AttachFiles, args.AttachmentIDs)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	msg := sdk.Message{
		Role:          "user",
		Text:          args.Text,
		Images:        content,
		AttachmentIDs: attachmentIDs,
	}

	events, err := sess.Send(ctx, msg)
	if errors.Is(err, sdk.ErrSessionBusy) {
		if behavior == "steer" {
			return s.queueMessage(cmd.ID, sess, "steer", msg)
		}
		if behavior != "" {
			return s.queueMessage(cmd.ID, sess, "follow_up", msg)
		}
		err = fmt.Errorf("%w: set streaming_behavior to steer or follow_up to queue the message", err)
	}
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	s.promptWG.Go(func() {
		if err := s.streamPrompt(cmd.ID, sess.ID(), events); err != nil {
			if sendErr := s.sendError(cmd.ID, "handler_error", fmt.Sprintf("handle command: %v", err)); sendErr != nil {
				_, _ = fmt.Fprintf(os.Stderr, "warning: send prompt error: %v\n", errors.Join(err, sendErr))
			}
		}
	})
	return nil
}

// streamPrompt answers an accepted prompt with its disposition, then streams
// its events. The disposition is "started" once the run emits its first event
// after session_start, and "handled" when a slash command produced the result.
func (s *Server) streamPrompt(id, sessionID string, events <-chan sdk.Event) (err error) {
	defer func() {
		for range events {
		}
	}()
	var (
		pending   []sdk.Event
		responded bool
	)
	respond := func(disposition string) error {
		responded = true
		if err := s.sendResult(id, map[string]any{"ok": true, "disposition": disposition}); err != nil {
			return err
		}
		for _, event := range pending {
			if err := s.emitEvent(event); err != nil {
				return fmt.Errorf("emit event: %w", err)
			}
		}
		pending = nil
		return nil
	}
	for event := range events {
		if event.SessionID == "" {
			event.SessionID = sessionID
		}
		if responded {
			if err := s.emitEvent(event); err != nil {
				return fmt.Errorf("emit event: %w", err)
			}
			continue
		}
		pending = append(pending, event)
		switch event.Type {
		case "session_start":
		case "command_result":
			if err := respond("handled"); err != nil {
				return err
			}
		default:
			if err := respond("started"); err != nil {
				return err
			}
		}
	}
	if !responded {
		return respond("handled")
	}
	return nil
}

// handleInterrupt cancels the in-flight assistant response.
func (s *Server) handleInterrupt(ctx context.Context, cmd *Command) error {
	s.sessionMu.Lock()
	current := s.session
	s.sessionMu.Unlock()
	if current != nil {
		if err := current.Interrupt(); err != nil {
			return s.sendErrorResult(cmd.ID, err)
		}
	}

	// Emit interrupt event
	select {
	case s.interrupt <- struct{}{}:
	default:
	}

	return s.sendResult(cmd.ID, map[string]any{"ok": true})
}

// handleSlashCommand invokes a slash command through the command registry.
func (s *Server) handleSlashCommand(ctx context.Context, cmd *Command) error {
	var args struct {
		Name string `json:"name"`
		Args string `json:"args"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if s.commandRegistry == nil {
		return s.sendResult(cmd.ID, map[string]any{
			"ok":      false,
			"message": "no command registry configured",
		})
	}

	// Build adapters for the command registry
	uiAdapter := &rpcCommandUI{server: s}
	sessionAdapter := &rpcCommandSession{server: s}

	// Parse and execute the slash command
	line := "/" + args.Name
	if args.Args != "" {
		line += " " + args.Args
	}

	result, err := s.commandRegistry.ParseAndExecute(ctx, line, uiAdapter, sessionAdapter)
	if err != nil {
		var cmdErr commands.ErrCommandNotFound
		if errors.As(err, &cmdErr) {
			return s.sendError(cmd.ID, "command_not_found", fmt.Sprintf("command not found: /%s", cmdErr.Name))
		}
		return s.sendErrorResult(cmd.ID, err)
	}

	return s.sendResult(cmd.ID, map[string]any{
		"ok":     true,
		"output": result.Output,
		"error":  result.IsError,
	})
}

// rpcCommandUI adapts Server to commands.CommandUI for slash-command execution.
type rpcCommandUI struct {
	server *Server
}

func (a *rpcCommandUI) responseContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if a.server.coordinated {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, 60*time.Second)
}

func (a *rpcCommandUI) Confirm(ctx context.Context, title, message string) (bool, error) {
	ctx, cancel := a.responseContext(ctx)
	defer cancel()
	return a.server.ui.Confirm(ctx, title, message)
}

func (a *rpcCommandUI) Select(ctx context.Context, title string, options []string) (int, error) {
	ctx, cancel := a.responseContext(ctx)
	defer cancel()
	return a.server.ui.Select(ctx, title, options)
}

func (a *rpcCommandUI) Input(ctx context.Context, title, placeholder string) (string, error) {
	ctx, cancel := a.responseContext(ctx)
	defer cancel()
	return a.server.ui.Input(ctx, title, placeholder)
}

// rpcUIWriter merges UI and SDK events into the server's serialized stream.
type rpcUIWriter struct{ server *Server }

func (w rpcUIWriter) Write(data []byte) (int, error) {
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		return 0, fmt.Errorf("decode UI event: %w", err)
	}
	kind, _ := event["type"].(string)
	delete(event, "session_id")
	delete(event, "seq")
	delete(event, "ts")
	if err := w.server.emitEvent(sdk.Event{Type: kind, Payload: event}); err != nil {
		return 0, err
	}
	return len(data), nil
}

// rpcCommandSession adapts Server to commands.CommandSession for slash-command execution.
type rpcCommandSession struct {
	server *Server
}

// GetLastAssistantText returns the text content of the most recent assistant message.
func (a *rpcCommandSession) GetLastAssistantText() string {
	a.server.sessionMu.Lock()
	defer a.server.sessionMu.Unlock()
	if a.server.session == nil {
		return ""
	}
	if sessionHost, ok := a.server.session.(sdk.LastAssistantTexter); ok {
		return sessionHost.GetLastAssistantText()
	}
	return ""
}

// GetSessionName returns the session display name.
func (a *rpcCommandSession) GetSessionName() string {
	a.server.sessionMu.Lock()
	defer a.server.sessionMu.Unlock()
	if a.server.session == nil {
		return ""
	}
	if sessionHost, ok := a.server.session.(sdk.Namer); ok {
		return sessionHost.GetSessionName()
	}
	return ""
}

// Compact triggers session compaction.
func (a *rpcCommandSession) Compact(ctx context.Context, customInstructions ...string) error {
	a.server.sessionMu.Lock()
	current := a.server.session
	a.server.sessionMu.Unlock()
	if current == nil {
		return fmt.Errorf("no active session")
	}
	return current.Compact(ctx, customInstructions...)
}

// ReloadResources forwards coordinated slash-command reload to the current session.
func (a *rpcCommandSession) ReloadResources(ctx context.Context, settings *ai.Settings, resources *artifacts.PreparedReload, preparedPlugins *plugins.PreparedReload, name string) error {
	a.server.sessionMu.Lock()
	current := a.server.session
	a.server.sessionMu.Unlock()
	if current == nil {
		return errors.New("no active session")
	}
	reloader, ok := current.(sdk.ResourceReloader)
	if !ok {
		return errors.New("session does not support coordinated resource reload")
	}
	return reloader.ReloadResources(ctx, settings, resources, preparedPlugins, name)
}

// handleGetState returns current session state.
func (s *Server) handleGetState(ctx context.Context, cmd *Command) error {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	if s.session == nil {
		return s.sendResult(cmd.ID, map[string]any{
			"session_id":     "",
			"model":          s.modelOverride,
			"cwd":            "",
			"position":       0,
			"tokens":         0,
			"thinking_level": s.thinkingLevel,
			"is_streaming":   false,
			"queue": map[string]any{
				"steering":  0,
				"follow_up": 0,
			},
		})
	}

	state := s.session.State()
	model := state.Model()
	if s.modelOverride != "" {
		model = s.modelOverride
	}
	steering, followUp := queueSizes(s.session)
	return s.sendResult(cmd.ID, map[string]any{
		"session_id":     state.SessionID(),
		"model":          model,
		"cwd":            state.CWD(),
		"position":       state.Position(),
		"tokens":         state.TotalTokens(),
		"thinking_level": s.thinkingLevel,
		"is_streaming":   state.IsStreaming(),
		"queue": map[string]any{
			"steering":  steering,
			"follow_up": followUp,
		},
	})
}

// handleSetModel switches the active model for future turns without replacing
// the current session. Recreating/resuming the session here discards any
// in-memory context that has not been checkpointed exactly as replay expects,
// so model changes must be an in-place session mutation.
func (s *Server) handleSetModel(ctx context.Context, cmd *Command) error {
	var args struct {
		ModelSpec string `json:"model_spec"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if args.ModelSpec == "" {
		return s.sendErrorResult(cmd.ID, fmt.Errorf("model_spec cannot be empty"))
	}
	model, err := ai.ResolveModelID(args.ModelSpec)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	modelSpec := model.Provider + "/" + model.ID

	s.sessionMu.Lock()
	current := s.session
	s.modelOverride = modelSpec
	s.sessionMu.Unlock()

	if current == nil {
		if err := s.emitEvent(sdk.Event{Type: "model_change", Payload: map[string]any{"model": modelSpec}}); err != nil {
			return fmt.Errorf("emit model change: %w", err)
		}
		return s.sendResult(cmd.ID, map[string]any{"ok": true, "model": modelSpec})
	}

	if err := current.SetModel(modelSpec); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if err := s.emitEvent(sdk.Event{Type: "model_change", SessionID: current.ID(), Payload: map[string]any{"model": modelSpec}}); err != nil {
		return fmt.Errorf("emit model change: %w", err)
	}

	return s.sendResult(cmd.ID, map[string]any{"ok": true, "model": modelSpec})
}

func queueSizes(sess sdk.Session) (steering, followUp int) {
	if queuer, ok := sess.(sdk.Queuer); ok {
		return queuer.QueueSizes()
	}
	return 0, 0
}

func (s *Server) observeSessionEvents(sess sdk.Session) {
	observer, ok := sess.(sdk.EventObserver)
	if !ok {
		return
	}
	observer.SetEventObserver(func(event sdk.Event) {
		if err := s.emitEvent(event); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: emit session event: %v\n", err)
		}
	})
}

func (s *Server) emitQueueUpdate(sess sdk.Session) error {
	steering, followUp := queueSizes(sess)
	return s.emitEvent(sdk.Event{
		Type:      "queue_update",
		SessionID: sess.ID(),
		Payload: map[string]any{
			"steering":        steering,
			"follow_up":       followUp,
			"steering_count":  steering,
			"follow_up_count": followUp,
		},
	})
}

// handleSetThinkingLevel updates the thinking level mid-session.
func (s *Server) handleSetThinkingLevel(ctx context.Context, cmd *Command) error {
	var args struct {
		Level string `json:"level"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if args.Level == "" {
		return s.sendErrorResult(cmd.ID, fmt.Errorf("thinking level cannot be empty"))
	}

	// Update server-side thinking level
	s.sessionMu.Lock()
	s.thinkingLevel = args.Level
	current := s.session
	s.sessionMu.Unlock()

	if current == nil {
		if err := s.emitEvent(sdk.Event{Type: "thinking_level_change", Payload: map[string]any{"level": args.Level}}); err != nil {
			return fmt.Errorf("emit thinking level change: %w", err)
		}
		return s.sendResult(cmd.ID, map[string]any{"ok": true, "level": args.Level})
	}

	// Replace the session to apply the new thinking level
	if err := s.replaceSession(ctx, sdk.SessionOptions{
		CWD:           current.State().CWD(),
		Model:         s.modelOverride,
		SessionID:     current.State().SessionID(),
		ThinkingLevel: s.thinkingLevel,
	}, "thinking_level_change"); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	// Write thinking_level_change entry to session
	if err := s.writeThinkingLevelChangeEntry(ctx, s.session, args.Level); err != nil {
		fmt.Fprintf(os.Stderr, "warning: write thinking_level_change entry: %v\n", err)
	}
	if err := s.emitEvent(sdk.Event{Type: "thinking_level_change", SessionID: s.session.ID(), Payload: map[string]any{"level": args.Level}}); err != nil {
		return fmt.Errorf("emit thinking level change: %w", err)
	}

	return s.sendResult(cmd.ID, map[string]any{"ok": true, "level": args.Level})
}

// handleListSessions returns enumerate sessions.
func (s *Server) handleListSessions(ctx context.Context, cmd *Command) error {
	var args struct {
		Limit int    `json:"limit,omitempty"`
		Since string `json:"since,omitempty"`
	}
	_ = json.Unmarshal(cmd.Args, &args)

	sessions, err := s.runtime.ListSessions(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	// Apply limit
	if args.Limit > 0 && len(sessions) > args.Limit {
		sessions = sessions[:args.Limit]
	}

	active := s.activeSessionInfo()
	result := make([]map[string]any, len(sessions))
	for i, info := range sessions {
		result[i] = map[string]any{
			"id":               info.ID,
			"name":             info.Name,
			"cwd":              info.CWD,
			"created":          info.Created,
			"last_modified":    info.LastModified,
			"model":            info.Model,
			"status":           workspaceStatus(info, active),
			"message_count":    info.MessageCount,
			"context_usage":    info.Tokens.Total(),
			"context_used":     info.Tokens.Total(),
			"context_used_pct": 0,
			"pinned":           info.Pinned,
			"pinned_at":        info.PinnedAt,
		}
	}

	return s.sendResult(cmd.ID, map[string]any{"sessions": result})
}

// handleResumeSession switches to an existing session.
func (s *Server) handlePinSession(ctx context.Context, cmd *Command) error {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if args.ID == "" {
		return s.sendErrorResult(cmd.ID, fmt.Errorf("id cannot be empty"))
	}
	if err := s.runtime.PinSession(ctx, args.ID); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	return s.sendResult(cmd.ID, map[string]any{"ok": true})
}

func (s *Server) handleUnpinSession(ctx context.Context, cmd *Command) error {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if args.ID == "" {
		return s.sendErrorResult(cmd.ID, fmt.Errorf("id cannot be empty"))
	}
	if err := s.runtime.UnpinSession(ctx, args.ID); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	return s.sendResult(cmd.ID, map[string]any{"ok": true})
}

// handleResumeSession switches to an existing session.
func (s *Server) handleResumeSession(ctx context.Context, cmd *Command) error {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if err := s.replaceSession(ctx, sdk.SessionOptions{
		SessionID:     args.ID,
		Model:         s.modelOverride,
		ThinkingLevel: s.thinkingLevel,
	}, "resume"); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	return s.sendResult(cmd.ID, map[string]any{
		"ok":         true,
		"session_id": args.ID,
	})
}

// handleNewSession starts a fresh session.
func (s *Server) handleNewSession(ctx context.Context, cmd *Command) error {
	var args struct {
		CWD string `json:"cwd,omitempty"`
	}
	_ = json.Unmarshal(cmd.Args, &args)

	if err := s.replaceSession(ctx, sdk.SessionOptions{
		CWD:           args.CWD,
		Model:         s.modelOverride,
		ThinkingLevel: s.thinkingLevel,
	}, "new_session"); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	return s.sendResult(cmd.ID, map[string]any{
		"ok":         true,
		"session_id": s.session.State().SessionID(),
	})
}

// handleCompact triggers compaction.
func (s *Server) handleCompact(ctx context.Context, cmd *Command) error {
	var args struct {
		CustomInstructions      string `json:"customInstructions,omitempty"`
		CustomInstructionsSnake string `json:"custom_instructions,omitempty"`
	}
	if len(cmd.Args) > 0 {
		if err := json.Unmarshal(cmd.Args, &args); err != nil {
			return s.sendErrorResult(cmd.ID, err)
		}
	}
	instructions := strings.TrimSpace(args.CustomInstructions)
	if instructions == "" {
		instructions = strings.TrimSpace(args.CustomInstructionsSnake)
	}

	s.sessionMu.Lock()
	current := s.session
	s.sessionMu.Unlock()
	if current == nil {
		return s.sendErrorResult(cmd.ID, fmt.Errorf("no active session"))
	}
	if err := current.Compact(ctx, instructions); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	return s.sendResult(cmd.ID, map[string]any{"ok": true})
}

// handleShutdown gracefully exits.
func (s *Server) handleShutdown(ctx context.Context, cmd *Command) error {
	defer s.shutdownOnce.Do(func() { close(s.shutdown) })
	return s.sendResult(cmd.ID, map[string]any{"ok": true})
}

// handleUIResponse processes client reply to a ui_* event.
//
// The args payload may carry either a typed `UIResponse` (with `method`,
// `confirmed`, `cancelled`) or a legacy `{id, value}` pair. We unmarshal
// straight into UIResponse and dispatch via DispatchResponseTyped so the
// receiving goroutine sees the full discriminator.
func (s *Server) handleUIResponse(ctx context.Context, cmd *Command) error {
	var resp ui.UIResponse
	if err := json.Unmarshal(cmd.Args, &resp); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if s.coordinated {
		if resp.ID == "" {
			return s.sendError(cmd.ID, "invalid_args", "id is required")
		}
		resolved, err := s.ui.Resolve(resp)
		if err != nil {
			return s.sendError(cmd.ID, "invalid_response", err.Error())
		}
		return s.sendResult(cmd.ID, map[string]any{"ok": true, "resolved": resolved})
	}

	// Dispatch the typed UI response to the pending channel in RPCUI.
	s.ui.DispatchResponseTyped(resp)

	// Send result first to unblock the client
	return s.sendResult(cmd.ID, map[string]any{"ok": true})
}

// handleGetUIRequests snapshots dialogs that any attached client may answer.
func (s *Server) handleGetUIRequests(_ context.Context, cmd *Command) error {
	if !s.coordinated {
		return s.sendError(cmd.ID, "not_supported", "get_ui_requests requires a coordinated worker")
	}
	return s.sendResult(cmd.ID, map[string]any{"requests": s.ui.Requests()})
}

// ensureSession creates a session if none exists.
func (s *Server) ensureSession(ctx context.Context) (sdk.Session, error) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	if s.session != nil {
		return s.session, nil
	}

	sess, err := s.runtime.NewSession(ctx, sdk.SessionOptions{
		UI:            s.ui,
		Model:         s.modelOverride,
		ThinkingLevel: s.thinkingLevel,
	})
	if err != nil {
		return nil, err
	}

	s.session = sess
	s.sessionGeneration.Add(1)
	s.eventSessionID.Store(sess.ID())
	s.sessionCtx, s.sessionCancel = context.WithCancel(ctx)
	s.observeSessionEvents(sess)
	return sess, nil
}

func (s *Server) replaceSession(ctx context.Context, opts sdk.SessionOptions, reason string) error {
	opts.UI = s.ui
	s.sessionMu.Lock()
	prev := s.session
	prevCancel := s.sessionCancel
	s.session = nil
	s.sessionGeneration.Add(1)
	s.sessionCancel = nil
	s.sessionCtx = nil
	s.sessionMu.Unlock()

	if prevCancel != nil {
		prevCancel()
	}

	if prev != nil {
		// Write model_change entry to the previous session before closing
		if reason == "model_switch" {
			if err := s.writeModelChangeEntry(ctx, prev, opts.Model); err != nil {
				fmt.Fprintf(os.Stderr, "warning: write model_change entry: %v\n", err)
			}
		}

		_ = s.emitEvent(sdk.Event{
			Type:      "session_end",
			SessionID: prev.ID(),
			Payload: map[string]any{
				"id":           prev.ID(),
				"total_tokens": prev.State().TotalTokens(),
				"cost_usd":     0.0,
				"reason":       reason,
			},
		})
		if err := prev.Close(ctx); err != nil {
			return fmt.Errorf("close previous session: %w", err)
		}
	}

	var (
		sess sdk.Session
		err  error
	)
	if opts.SessionID != "" && opts.ForkFrom == "" {
		sess, err = s.runtime.ResumeSession(ctx, opts.SessionID, opts)
	} else {
		sess, err = s.runtime.NewSession(ctx, opts)
	}
	if err != nil {
		return err
	}

	s.sessionMu.Lock()
	s.session = sess
	s.sessionGeneration.Add(1)
	s.eventSessionID.Store(sess.ID())
	s.sessionCtx, s.sessionCancel = context.WithCancel(ctx)
	s.sessionMu.Unlock()
	s.observeSessionEvents(sess)

	return s.emitEvent(sdk.Event{
		Type:      "session_start",
		SessionID: sess.ID(),
		Payload: map[string]any{
			"id":    sess.ID(),
			"cwd":   sess.State().CWD(),
			"model": sess.State().Model(),
		},
	})
}

// writeModelChangeEntry writes a model_change entry to the previous session.
func (s *Server) writeModelChangeEntry(ctx context.Context, prev sdk.Session, modelSpec string) (err error) {
	sid := prev.ID()
	if sid == "" {
		return nil // Ephemeral session
	}

	// Parse the model spec to extract provider/model
	var provider, model string
	parts := strings.Split(modelSpec, "/")
	if len(parts) >= 2 {
		provider = parts[0]
		model = strings.Join(parts[1:], "/")
	} else {
		model = modelSpec
	}

	w, err := s.runtime.GetSessionStorage().Resume(sid)
	if err != nil {
		return fmt.Errorf("resume session for model_change entry: %w", err)
	}
	defer func() {
		if closeErr := w.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close session writer: %w", closeErr)
		}
	}()

	return w.Append(session.NewModelChangeEntry(provider, model))
}

// writeThinkingLevelChangeEntry writes a thinking_level_change entry to the session.
func (s *Server) writeThinkingLevelChangeEntry(ctx context.Context, sess sdk.Session, level string) (err error) {
	sid := sess.ID()
	if sid == "" {
		return nil // Ephemeral session
	}

	w, err := s.runtime.GetSessionStorage().Resume(sid)
	if err != nil {
		return fmt.Errorf("resume session for thinking_level_change entry: %w", err)
	}
	defer func() {
		if closeErr := w.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close session writer: %w", closeErr)
		}
	}()

	return w.Append(session.NewThinkingLevelChangeEntry(level))
}

func (s *Server) emitMeta() error {
	meta := map[string]any{
		"type":           "meta",
		"schema_version": 1,
		"tau_version":    "0.1.0",
	}
	// Let remote clients show which machine they are controlling.
	if host, err := os.Hostname(); err == nil && host != "" {
		meta["hostname"] = host
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	data = append(data, '\n')
	err = s.write(data)
	return err
}

// emitEvent writes an event to stdout as JSONL.
func (s *Server) emitEvent(event sdk.Event) error {
	if event.Type == "meta" {
		return nil
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	// Observers must not take sessionMu: session Close can wait for them.
	if event.SessionID == "" {
		event.SessionID, _ = s.eventSessionID.Load().(string)
	}

	// RPC owns the emitted event sequence so SDK-originated events and
	// server-originated UI/interrupt/session lifecycle events share one
	// monotonic stream per session.
	if event.Timestamp == "" {
		event.Timestamp = time.Now().Format(time.RFC3339Nano)
	}
	event.Seq = s.nextSeq(event.SessionID)
	data, err := json.Marshal(jsonEventEnvelope(event))
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	data = append(data, '\n')
	err = s.write(data)
	return err
}

func jsonEventEnvelope(event sdk.Event) map[string]any {
	out := map[string]any{
		"type": event.Type,
	}
	if event.SessionID != "" {
		out["session_id"] = event.SessionID
	}
	out["seq"] = event.Seq
	out["ts"] = event.Timestamp
	if payload, ok := event.Payload.(map[string]any); ok {
		for k, v := range payload {
			if k == "type" {
				continue
			}
			out[k] = v
		}
	} else if event.Payload != nil {
		out["payload"] = event.Payload
	}
	return out
}

// nextSeq returns the next sequence number for the current session stream.
// When the session ID changes, the stream resets to 0 for the new session.
func (s *Server) nextSeq(sessionID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID != "" && sessionID != s.seqSessionID {
		s.seqSessionID = sessionID
		s.seq = 0
	}
	seq := s.seq
	s.seq++
	return seq
}

// sendResult sends a successful response.
func (s *Server) sendResult(id string, result any) error {
	resp := Response{
		Type:   "response",
		ID:     id,
		OK:     true,
		Result: result,
	}
	return s.sendResponse(resp)
}

// sendError sends an error response.
func (s *Server) sendError(id, code, message string) error {
	resp := Response{
		Type:  "response",
		ID:    id,
		OK:    false,
		Error: &ErrorInfo{Code: code, Message: message},
	}
	return s.sendResponse(resp)
}

// sendErrorResult sends an error response derived from an error.
func (s *Server) sendErrorResult(id string, err error) error {
	if errors.Is(err, errSessionChanged) {
		return s.sendError(id, "session_changed", err.Error())
	}
	return s.sendError(id, "error", err.Error())
}

func (s *Server) write(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.writer.Write(data)
	return err
}

// sendResponse writes a response to stdout.
func (s *Server) sendResponse(resp Response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	data = append(data, '\n')
	err = s.write(data)
	return err
}

// handleSteer adds a steering message to the queue.
func (s *Server) handleSteer(ctx context.Context, cmd *Command) error {
	var args struct {
		Text   string   `json:"text"`
		Images []string `json:"images,omitempty"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	content := make([]ai.ContentBlock, 0, len(args.Images))
	for _, img := range args.Images {
		content = append(content, ai.ContentBlock{
			Type:     ai.ContentBlockImage,
			Data:     img,
			MimeType: "image/png",
		})
	}
	return s.queueMessage(cmd.ID, sess, "steer", sdk.Message{Role: "user", Text: args.Text, Images: content})
}

// handleFollowUp adds a follow-up message to the queue.
func (s *Server) handleFollowUp(ctx context.Context, cmd *Command) error {
	var args struct {
		ExpectedSessionID string   `json:"expected_session_id,omitempty"`
		Text              string   `json:"text"`
		Images            []string `json:"images,omitempty"`
		AttachmentIDs     []string `json:"attachment_ids,omitempty"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	sess, err := s.draftSession(ctx, args.ExpectedSessionID)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	content, ids, err := messageContentBlocks(sess, args.Images, nil, args.AttachmentIDs)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	return s.queueMessage(cmd.ID, sess, "follow_up", sdk.Message{Role: "user", Text: args.Text, Images: content, AttachmentIDs: ids})
}

// queueMessage queues msg as a steering ("steer") or follow-up message and
// answers with the "queued" disposition.
func (s *Server) queueMessage(id string, sess sdk.Session, kind string, msg sdk.Message) error {
	var err error
	switch kind {
	case "steer":
		steerer, ok := sess.(sdk.Steerer)
		if !ok {
			return s.sendError(id, "not_supported", "steer not supported in this session")
		}
		err = steerer.Steer(msg)
	default:
		followUpper, ok := sess.(sdk.FollowUpper)
		if !ok {
			return s.sendError(id, "not_supported", "follow_up not supported in this session")
		}
		err = followUpper.FollowUp(msg)
	}
	if err != nil {
		return s.sendErrorResult(id, err)
	}
	if err := s.emitQueueUpdate(sess); err != nil {
		return fmt.Errorf("emit queue update: %w", err)
	}
	return s.sendResult(id, map[string]any{"ok": true, "disposition": "queued"})
}

// handleCycleModel cycles to the next available model.
func (s *Server) handleCycleModel(ctx context.Context, cmd *Command) error {
	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if agentSess, ok := sess.(sdk.ModelCycler); ok {
		model, err := agentSess.CycleModel()
		if err != nil {
			return s.sendErrorResult(cmd.ID, err)
		}
		if model == nil {
			return s.sendResult(cmd.ID, map[string]any{"model": nil})
		}
		s.modelOverride = model.Provider + "/" + model.ID
		return s.sendResult(cmd.ID, map[string]any{
			"model": map[string]any{
				"id":       model.ID,
				"provider": model.Provider,
			},
		})
	}

	return s.sendError(cmd.ID, "not_supported", "cycle_model not supported in this session")
}

// handleCycleThinkingLevel cycles to the next thinking level.
func (s *Server) handleCycleThinkingLevel(ctx context.Context, cmd *Command) error {
	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if agentSess, ok := sess.(sdk.ThinkingCycler); ok {
		level := agentSess.CycleThinkingLevel()
		s.sessionMu.Lock()
		s.thinkingLevel = level
		s.sessionMu.Unlock()
		if s.coordinated {
			if err := s.emitEvent(sdk.Event{Type: "thinking_level_change", SessionID: sess.ID(), Payload: map[string]any{"level": level}}); err != nil {
				return err
			}
		}
		return s.sendResult(cmd.ID, map[string]any{"level": level})
	}

	return s.sendError(cmd.ID, "not_supported", "cycle_thinking_level not supported in this session")
}

// handleSetSteeringMode sets the steering queue mode.
func (s *Server) handleSetSteeringMode(ctx context.Context, cmd *Command) error {
	var args struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if agentSess, ok := sess.(sdk.QueueModeSetter); ok {
		if err := agentSess.SetSteeringMode(args.Mode); err != nil {
			return s.sendErrorResult(cmd.ID, err)
		}
		return s.sendResult(cmd.ID, map[string]any{"ok": true})
	}

	return s.sendError(cmd.ID, "not_supported", "set_steering_mode not supported in this session")
}

// handleSetFollowUpMode sets the follow-up queue mode.
func (s *Server) handleSetFollowUpMode(ctx context.Context, cmd *Command) error {
	var args struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if agentSess, ok := sess.(sdk.QueueModeSetter); ok {
		if err := agentSess.SetFollowUpMode(args.Mode); err != nil {
			return s.sendErrorResult(cmd.ID, err)
		}
		return s.sendResult(cmd.ID, map[string]any{"ok": true})
	}

	return s.sendError(cmd.ID, "not_supported", "set_follow_up_mode not supported in this session")
}

// handleSetAutoCompaction enables or disables auto-compaction.
func (s *Server) handleSetAutoCompaction(ctx context.Context, cmd *Command) error {
	var args struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if agentSess, ok := sess.(sdk.CompactionController); ok {
		agentSess.SetAutoCompactionEnabled(args.Enabled)
		return s.sendResult(cmd.ID, map[string]any{"ok": true})
	}

	return s.sendError(cmd.ID, "not_supported", "set_auto_compaction not supported in this session")
}

// handleSetAutoRetry enables or disables auto-retry.
func (s *Server) handleSetAutoRetry(ctx context.Context, cmd *Command) error {
	var args struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if agentSess, ok := sess.(sdk.RetryController); ok {
		agentSess.SetAutoRetryEnabled(args.Enabled)
		return s.sendResult(cmd.ID, map[string]any{"ok": true})
	}

	return s.sendError(cmd.ID, "not_supported", "set_auto_retry not supported in this session")
}

// handleAbortRetry aborts the current retry attempt.
func (s *Server) handleAbortRetry(ctx context.Context, cmd *Command) error {
	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if agentSess, ok := sess.(sdk.RetryController); ok {
		agentSess.AbortRetry()
		return s.sendResult(cmd.ID, map[string]any{"ok": true})
	}

	return s.sendError(cmd.ID, "not_supported", "abort_retry not supported in this session")
}

// handleGetMessages returns all messages in the current session.
func (s *Server) handleGetMessages(ctx context.Context, cmd *Command) error {
	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if agentSess, ok := sess.(sdk.MessageLister); ok {
		result := map[string]any{"messages": agentSess.GetMessages()}
		if s.coordinated {
			result["session_id"] = sess.ID()
		}
		return s.sendResult(cmd.ID, result)
	}

	return s.sendError(cmd.ID, "not_supported", "get_messages not supported in this session")
}

// handleGetCommands returns all available slash commands (from extensions, prompts, skills).
func (s *Server) handleGetCommands(ctx context.Context, cmd *Command) error {
	// Collect commands from the command registry and artifact loader
	type rpcCmdInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Source      string `json:"source"`
		SourceInfo  string `json:"sourceInfo,omitempty"`
	}

	var cmds []rpcCmdInfo
	// Skills and prompts are registered in the command registry (for slash
	// execution) AND enumerable from the artifact loader; the loader pass
	// below is a fallback. Dedupe by name so clients don't see doubles.
	seen := make(map[string]bool)
	add := func(c rpcCmdInfo) {
		if seen[c.Name] {
			return
		}
		seen[c.Name] = true
		cmds = append(cmds, c)
	}

	// Get commands from the command registry if available
	if s.commandRegistry != nil {
		for _, info := range s.commandRegistry.List() {
			source := "builtin"
			switch info.Source {
			case commands.Prompt:
				source = "prompt"
			case commands.Skill:
				source = "skill"
			case commands.Plugin:
				source = "extension"
			}
			add(rpcCmdInfo{
				Name:        info.Name,
				Description: info.Description,
				Source:      source,
				SourceInfo:  info.SourcePath,
			})
		}
	}

	// Get commands from artifact loader
	if s.runtime != nil {
		if loader, ok := s.runtime.(sdk.ArtifactLoaderProvider); ok {
			al := loader.ArtifactLoader()
			if al != nil {
				// Add skills as commands
				if reg := al.SkillRegistry(); reg != nil {
					for _, skill := range reg.List() {
						add(rpcCmdInfo{
							Name:        "skill:" + skill.Name,
							Description: skill.Description,
							Source:      "skill",
						})
					}
				}

				// Add prompts as commands
				if reg := al.PromptRegistry(); reg != nil {
					for _, prompt := range reg.List() {
						add(rpcCmdInfo{
							Name:        prompt.Name,
							Description: prompt.Description,
							Source:      "prompt",
						})
					}
				}
			}
		}
	}

	return s.sendResult(cmd.ID, map[string]any{"commands": cmds})
}

// handleGetSessionStats returns session statistics.
func (s *Server) handleGetSessionStats(ctx context.Context, cmd *Command) error {
	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if statser, ok := sess.(sdk.Statser); ok {
		return s.sendResult(cmd.ID, statser.Stats())
	}
	state := sess.State()
	return s.sendResult(cmd.ID, sdk.SessionStats{
		SessionID:     state.SessionID(),
		TotalTokens:   state.TotalTokens(),
		Model:         state.Model(),
		ThinkingLevel: s.thinkingLevel,
	})
}

// handleExportHTML exports the session to HTML.
func (s *Server) handleExportHTML(ctx context.Context, cmd *Command) error {
	var args struct {
		OutputPath string `json:"output_path,omitempty"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if agentSess, ok := sess.(sdk.Exporter); ok {
		path, err := agentSess.ExportHTML(ctx, args.OutputPath)
		if err != nil {
			return s.sendErrorResult(cmd.ID, err)
		}
		return s.sendResult(cmd.ID, map[string]any{"path": path})
	}

	return s.sendError(cmd.ID, "not_supported", "export_html not supported in this session")
}

// handleFork forks the session from a specific entry point.
func (s *Server) handleFork(ctx context.Context, cmd *Command) error {
	var args struct {
		EntryID       string `json:"entry_id,omitempty"`
		ForkFromEntry string `json:"fork_from_entry,omitempty"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if s.runtime == nil {
		return s.sendError(cmd.ID, "no_runtime", "no runtime available for fork")
	}

	forkEntryID := args.EntryID
	if forkEntryID == "" {
		forkEntryID = args.ForkFromEntry
	}

	current, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	// Fire `session_before_fork` (cancellable) so plugins can observe or
	// block the fork attempt.
	if s.pluginManager != nil {
		beforePayload, _ := json.Marshal(map[string]any{
			"session_id":      current.ID(),
			"fork_from_entry": forkEntryID,
		})
		if blocked := s.pluginManager.DispatchEvent(ctx, "session_before_fork", beforePayload, current.ID()); blocked {
			return s.sendError(cmd.ID, "fork_blocked", "fork blocked by plugin")
		}
	}

	opts := sdk.SessionOptions{
		UI:            s.ui,
		Model:         s.modelOverride,
		ThinkingLevel: s.thinkingLevel,
		ForkFrom:      current.ID(),
		ForkFromEntry: forkEntryID,
	}

	newSess, err := s.runtime.NewSession(ctx, opts)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	// Rebind session
	if err := s.rebindSession(ctx, newSess); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	return s.sendResult(cmd.ID, map[string]any{
		"ok":         true,
		"session_id": newSess.ID(),
	})
}

// handleGetForkMessages returns messages suitable for forking.
func (s *Server) handleGetForkMessages(ctx context.Context, cmd *Command) error {
	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}

	if agentSess, ok := sess.(sdk.ForkMessageLister); ok {
		messages := agentSess.GetUserMessagesForForking()
		return s.sendResult(cmd.ID, map[string]any{"messages": messages})
	}

	// Fallback: try to get all messages
	if agentSess, ok := sess.(sdk.MessageLister); ok {
		messages := agentSess.GetMessages()
		// Filter to user messages only
		var userMessages []sdk.Message
		for _, msg := range messages {
			if msg.Role == "user" {
				userMessages = append(userMessages, msg)
			}
		}
		return s.sendResult(cmd.ID, map[string]any{"messages": userMessages})
	}

	return s.sendError(cmd.ID, "not_supported", "get_fork_messages not supported in this session")
}

// handleBash executes a bash command and returns the result.
func (s *Server) handleBash(ctx context.Context, executor sdk.BashExecutor, cmd *Command) error {
	if err := ctx.Err(); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	var args struct {
		Command            string `json:"command"`
		Dir                string `json:"dir,omitempty"`
		TimeoutS           int    `json:"timeout_s,omitempty"`
		ExcludeFromContext bool   `json:"excludeFromContext,omitempty"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if args.Command == "" {
		return s.sendError(cmd.ID, "invalid_command", "command cannot be empty")
	}
	execution, err := executor.ExecuteBash(ctx, args.Command, sdk.BashOptions{
		ExcludeFromContext: args.ExcludeFromContext,
		Dir:                args.Dir,
		Timeout:            time.Duration(args.TimeoutS) * time.Second,
		OnOutput: func(delta string) {
			if err := s.emitEvent(sdk.Event{Type: "bash_execution_update", Payload: map[string]any{"id": cmd.ID, "delta": delta}}); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "warning: emit bash output: %v\n", err)
			}
		},
	})
	switch {
	case errors.Is(err, sdk.ErrBashBlocked):
		return s.sendError(cmd.ID, "blocked", err.Error())
	case errors.Is(err, agent.ErrInvalidUserBashOverride):
		return s.sendError(cmd.ID, "invalid_override", err.Error())
	case err != nil:
		return s.sendErrorResult(cmd.ID, err)
	}
	return s.sendResult(cmd.ID, map[string]any{
		"output":           execution.Output,
		"exit_code":        execution.ExitCode,
		"cancel_reason":    execution.CancelReason,
		"cancelled":        execution.Cancelled,
		"truncated":        execution.Truncated,
		"full_output_path": execution.FullOutputPath,
	})
}

// startBash admits one shell command and runs it off the input loop so
// abort_bash and other commands remain readable while it executes.
func (s *Server) startBash(ctx context.Context, cmd *Command) error {
	// The session outlives the shell, so it must not be created from the
	// shell's cancellable context.
	sess, err := s.ensureSession(ctx)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	executor, ok := sess.(sdk.BashExecutor)
	if !ok {
		return s.sendError(cmd.ID, "unsupported", "session does not support bash execution")
	}
	bashCtx, cancel := context.WithCancel(ctx)
	s.activeBashMu.Lock()
	if s.activeBashCancel != nil {
		s.activeBashMu.Unlock()
		cancel()
		return s.sendError(cmd.ID, "busy", "a bash command is already running")
	}
	s.activeBashCancel = cancel
	s.activeBashMu.Unlock()
	s.bashWG.Go(func() {
		defer func() {
			s.activeBashMu.Lock()
			s.activeBashCancel = nil
			s.activeBashMu.Unlock()
			cancel()
			if s.coordinated {
				if err := s.emitEvent(sdk.Event{Type: "bash_execution_end", SessionID: sess.ID(), Payload: map[string]any{"id": cmd.ID}}); err != nil {
					workerLog("publish shell completion: %v", err)
				}
			}
		}()
		if err := s.handleBash(bashCtx, executor, cmd); err != nil {
			if sendErr := s.sendError(cmd.ID, "handler_error", fmt.Sprintf("handle command: %v", err)); sendErr != nil {
				_, _ = fmt.Fprintf(os.Stderr, "warning: send bash error: %v\n", errors.Join(err, sendErr))
			}
		}
	})
	return nil
}

// stopBash cancels any admitted shell command and waits for it to settle.
func (s *Server) stopBash() {
	s.activeBashMu.Lock()
	if s.activeBashCancel != nil {
		s.activeBashCancel()
	}
	s.activeBashMu.Unlock()
	s.bashWG.Wait()
}

// handleAbortBash aborts an in-progress bash command.
func (s *Server) handleAbortBash(ctx context.Context, cmd *Command) error {
	s.activeBashMu.Lock()
	cancel := s.activeBashCancel
	s.activeBashMu.Unlock()

	if cancel == nil {
		return s.sendResult(cmd.ID, map[string]any{
			"ok":      true,
			"aborted": false,
			"message": "no active bash command to abort",
		})
	}

	cancel()

	return s.sendResult(cmd.ID, map[string]any{
		"ok":      true,
		"aborted": true,
	})
}

// rebindSession rebinds the server to a new session.
func (s *Server) rebindSession(ctx context.Context, newSess sdk.Session) error {
	s.sessionMu.Lock()
	prev := s.session
	prevCancel := s.sessionCancel
	s.session = newSess
	s.sessionGeneration.Add(1)
	s.eventSessionID.Store(newSess.ID())
	s.sessionCtx, s.sessionCancel = context.WithCancel(ctx)
	s.sessionMu.Unlock()
	s.observeSessionEvents(newSess)

	if prevCancel != nil {
		prevCancel()
	}
	if prev != nil {
		if err := prev.Close(ctx); err != nil {
			return fmt.Errorf("close previous session: %w", err)
		}
	}

	return nil
}

// sessionCleanupTimeout bounds session shutdown once the server loop ends,
// since the loop's own context is usually already cancelled by then.
const sessionCleanupTimeout = 10 * time.Second

// cleanup performs graceful shutdown.
func (s *Server) cleanup(ctx context.Context) (err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
	defer cancel()
	s.stopBash()
	s.sessionMu.Lock()
	if s.sessionCancel != nil {
		s.sessionCancel()
	}
	var sessID string
	if s.session != nil {
		sessID = s.session.ID()
		if closeErr := s.session.Close(ctx); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close RPC session: %w", closeErr))
		}
	}
	s.sessionMu.Unlock()
	s.promptWG.Wait()

	// Fire `session_shutdown` on the JSON stream so transports (and
	// tailers of the RPC event stream) observe the session tear-down.
	// The plugin-bus side is fired inside agentSession.Close.
	if sessID != "" {
		emitErr := s.emitEvent(sdk.Event{
			Type:      "session_shutdown",
			SessionID: sessID,
			Payload:   map[string]any{"session_id": sessID},
		})
		if emitErr != nil {
			err = errors.Join(err, fmt.Errorf("emit session shutdown: %w", emitErr))
		}
	}

	if s.ui != nil {
		if closeErr := s.ui.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close RPC UI: %w", closeErr))
		}
	}

	return err
}

// SendInterrupt interrupts the in-flight run and emits an interrupt event.
// This is called when tau receives SIGINT in RPC mode, translating it into an
// interrupt event for the connected client.
func (s *Server) SendInterrupt() {
	s.emitInterruptEvent()
}

// isInterrupted returns true if an interrupt is pending and clears the flag.
func (s *Server) isInterrupted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.interrupted {
		s.interrupted = false
		return true
	}
	return false
}

// emitInterruptEvent emits an "interrupt" event to the client.
func (s *Server) emitInterruptEvent() {
	// Mark that we've emitted an interrupt
	s.mu.Lock()
	s.interrupted = true
	s.mu.Unlock()

	// Cancel the in-flight session if any
	var sessionID string
	s.sessionMu.Lock()
	if s.session != nil {
		_ = s.session.Interrupt()
		sessionID = s.session.ID()
	}
	s.sessionMu.Unlock()

	event := map[string]any{
		"type": "interrupt",
	}
	_ = s.emitEvent(sdk.Event{Type: "interrupt", SessionID: sessionID, Payload: event})
}

// emitDisconnectedSessionEnd emits a session_end event with disconnected reason
// when the client disconnects without sending shutdown.
func (s *Server) emitDisconnectedSessionEnd() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	if s.session == nil {
		return
	}

	// Create an event channel to receive the session_end event
	events := make(chan sdk.Event, 1)

	if agentSess, ok := s.session.(sdk.DisconnectedSessionEndEmitter); ok {
		agentSess.EmitDisconnectedSessionEnd(events)
		// Drain the event and emit it
		select {
		case event := <-events:
			if err := s.emitEvent(event); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "warning: emit disconnected session end: %v\n", err)
			}
		default:
		}
	}
}

// handleGetAvailableModels returns the list of models known to the SDK.
// Pi-compatible: clients can call this to discover what models the server
// can serve without consulting any external catalog.
func (s *Server) handleGetAvailableModels(ctx context.Context, cmd *Command) error {
	models := ai.ListModels("")
	s.sessionMu.Lock()
	sess := s.session
	s.sessionMu.Unlock()
	if lister, ok := sess.(sdk.ModelLister); ok {
		var err error
		models, err = lister.GetAvailableModels(ctx)
		if err != nil {
			return s.sendErrorResult(cmd.ID, err)
		}
	}
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{
			"id":       m.ID,
			"provider": m.Provider,
			"api":      m.API,
		})
	}
	return s.sendResult(cmd.ID, map[string]any{"models": out})
}

// handleGetLastAssistantText returns the text content of the most recent
// assistant message in the active session. Pi-compatible: lets clients
// (e.g. CLI tools) display the latest model output without parsing the
// event stream.
func (s *Server) handleGetLastAssistantText(ctx context.Context, cmd *Command) error {
	s.sessionMu.Lock()
	current := s.session
	s.sessionMu.Unlock()
	if current == nil {
		return s.sendResult(cmd.ID, map[string]any{"text": ""})
	}
	type texter interface {
		GetLastAssistantText() string
	}
	if t, ok := current.(texter); ok {
		return s.sendResult(cmd.ID, map[string]any{"text": t.GetLastAssistantText()})
	}
	return s.sendErrorResult(cmd.ID, fmt.Errorf("session does not support GetLastAssistantText"))
}

// handleSetSessionName updates the active session's display name by
// appending a session_info entry. The accessor / Storage path is used
// directly so the name persists across resume.
func (s *Server) handleSetSessionName(ctx context.Context, cmd *Command) error {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if args.Name == "" {
		return s.sendErrorResult(cmd.ID, fmt.Errorf("name must not be empty"))
	}

	s.sessionMu.Lock()
	current := s.session
	s.sessionMu.Unlock()
	sid := ""
	if current != nil {
		sid = current.ID()
	}

	if current == nil {
		return s.sendErrorResult(cmd.ID, fmt.Errorf("no active session"))
	}
	if s.runtime == nil {
		return s.sendErrorResult(cmd.ID, fmt.Errorf("no runtime available"))
	}
	storage := s.runtime.GetSessionStorage()
	if storage == nil {
		return s.sendErrorResult(cmd.ID, fmt.Errorf("no session storage available"))
	}

	if sid == "" {
		return s.sendErrorResult(cmd.ID, fmt.Errorf("session has no ID"))
	}

	entry := session.NewSessionInfoEntry(args.Name)
	w, err := storage.Resume(sid)
	if err != nil {
		return s.sendErrorResult(cmd.ID, fmt.Errorf("resume session for set_name: %w", err))
	}
	if err := w.Append(entry); err != nil {
		if closeErr := w.Close(); closeErr != nil {
			return s.sendErrorResult(cmd.ID, fmt.Errorf("append session_info entry: %w; close session writer: %w", err, closeErr))
		}
		return s.sendErrorResult(cmd.ID, fmt.Errorf("append session_info entry: %w", err))
	}
	if err := w.Close(); err != nil {
		return s.sendErrorResult(cmd.ID, fmt.Errorf("close session writer: %w", err))
	}
	return s.sendResult(cmd.ID, map[string]any{"ok": true, "name": args.Name})
}
