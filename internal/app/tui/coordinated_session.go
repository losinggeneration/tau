package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/losinggeneration/tau/ai"
	rpcclient "github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
	baseui "github.com/losinggeneration/tau/ui"
)

// Synthetic event types a CoordinatedSession adds to the shared stream.
const (
	// CoordinatedResetEvent replaces the whole view. Its Payload is a
	// CoordinatedReset; discard the transcript, live rows and dialogs and
	// rebuild them from it.
	CoordinatedResetEvent = "coordinated_reset"
	// CoordinatedStatusEvent reports transport state. Its Payload is a map
	// with "state" (connected, reconnecting or exited),
	// "instance_id", and "error" when a connection failed.
	CoordinatedStatusEvent = "coordinated_status"
)

// Transport states reported by CoordinatedStatusEvent.
const (
	CoordinatedConnected    = "connected"
	CoordinatedReconnecting = "reconnecting"
	CoordinatedExited       = "exited"
)

// Reasons reported by CoordinatedReset.
const (
	CoordinatedResetAttach    = "attach"
	CoordinatedResetResync    = "resync"
	CoordinatedResetReconnect = "reconnect"
)

const (
	coordinatedEventBuffer     = 256
	coordinatedMaxUIRequests   = 16
	coordinatedDialTimeout     = 10 * time.Second
	coordinatedReadTimeout     = 5 * time.Second
	coordinatedMutationTimeout = 30 * time.Second
	// Compaction and user shell commands run inside the worker's handler.
	coordinatedLongTimeout = 10 * time.Minute
	coordinatedRetryMin    = 100 * time.Millisecond
	coordinatedRetryMax    = 3 * time.Second
	omittedHistoryText     = "(history entry too large to display)"
)

var (
	// ErrCoordinatorUnavailable reports a call made while the session is
	// reconnecting. Nothing was sent.
	ErrCoordinatorUnavailable = errors.New("coordinated session: coordinator unavailable")
	// ErrCoordinatedSessionClosed reports use after Close. Nothing was sent.
	ErrCoordinatedSessionClosed = errors.New("coordinated session: closed")
	errCoordinatedWorkerExited  = errors.New("worker exited")
)

// CoordinatedReset is the Payload of a CoordinatedResetEvent.
type CoordinatedReset struct {
	Reason     string
	InstanceID string
	SessionID  string
	// History is the newest persisted page, oldest first. Pass
	// HistoryBefore to History for the preceding page.
	History       []sdk.HistoryEntry
	HistoryBefore string
	// Live is the in-progress tail at the snapshot position; events that
	// follow on the stream continue it.
	Live       CoordinatedLive
	UIRequests []baseui.SharedRequest
	Snapshot   *rpcclient.Snapshot
}

// CoordinatedLive holds in-progress messages, tool calls and shell runs.
// Truncated marks a tail that dropped items or bytes.
type CoordinatedLive struct {
	Messages  []CoordinatedLiveItem `json:"messages"`
	Tools     []CoordinatedLiveItem `json:"tools"`
	Bash      []CoordinatedLiveItem `json:"bash"`
	Truncated bool                  `json:"truncated,omitempty"`
	// UsageLimitWait is set while the run sleeps out a provider usage limit.
	UsageLimitWait *CoordinatedUsageLimitWait `json:"usage_limit_wait,omitempty"`
}

// CoordinatedUsageLimitWait names the limited provider and its RFC 3339 reset.
type CoordinatedUsageLimitWait struct {
	Provider string `json:"provider,omitempty"`
	ResetsAt string `json:"resets_at"`
}

// CoordinatedLiveItem is one in-progress row; text fields hold a tail.
type CoordinatedLiveItem struct {
	ID        string `json:"id"`
	Role      string `json:"role,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     string `json:"input,omitempty"`
	Progress  string `json:"progress,omitempty"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// CoordinatedSession presents one coordinator-owned instance as an
// sdk.Session. Every event, including the results of this client's own
// prompts, arrives on Events; Send only issues the command. Mutations are
// never resent: an uncertain outcome is returned to the caller. After a
// transport loss the session reconnects to the same instance, resuming from
// its cursor or rebuilding from a snapshot. Close detaches; the instance
// keeps running.
type CoordinatedSession struct {
	socket     string
	instanceID string
	clientID   string

	events chan sdk.Event
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	closeOnce sync.Once
	closeErr  error

	mu             sync.Mutex
	client         *rpcclient.CoordinatedClient
	closed         bool
	sessionID      string
	model          string
	thinking       string
	stats          sdk.SessionStats
	cwd            string
	streaming      bool
	steering       int
	followUp       int
	steeringPaused bool
	followUpPaused bool
	queued         []sdk.QueuedPrompt
	history        []sdk.HistoryEntry
	historyBefore  string
	uiRequests     []baseui.SharedRequest
	commands       []sdk.SlashCommandInfo
	attachments    []sdk.Attachment
}

var (
	_ sdk.Session           = (*CoordinatedSession)(nil)
	_ sdk.Statser           = (*CoordinatedSession)(nil)
	_ sdk.HistoryPager      = (*CoordinatedSession)(nil)
	_ sdk.Steerer           = (*CoordinatedSession)(nil)
	_ sdk.FollowUpper       = (*CoordinatedSession)(nil)
	_ sdk.Queuer            = (*CoordinatedSession)(nil)
	_ sdk.ModelCycler       = (*CoordinatedSession)(nil)
	_ sdk.ThinkingCycler    = (*CoordinatedSession)(nil)
	_ sdk.QueueModeSetter   = (*CoordinatedSession)(nil)
	_ sdk.AttachmentManager = (*CoordinatedSession)(nil)
	_ sdk.BashExecutor      = (*CoordinatedSession)(nil)
)

// OpenCoordinatedSession dials the coordinator at socket under a fresh
// stable client ID and attaches to instanceID. The first event on Events is
// a CoordinatedResetEvent carrying the snapshot.
func OpenCoordinatedSession(ctx context.Context, socket, instanceID string) (*CoordinatedSession, error) {
	if instanceID == "" {
		return nil, rpcclient.ErrTargetRequired
	}
	s := &CoordinatedSession{
		socket:     socket,
		instanceID: instanceID,
		clientID:   uuid.NewString(),
		events:     make(chan sdk.Event, coordinatedEventBuffer),
		done:       make(chan struct{}),
	}
	client, handoff, err := s.connect(ctx, nil, "")
	if err != nil {
		return nil, fmt.Errorf("open coordinated session %s: %w", instanceID, err)
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	pending, err := s.install(client, handoff, CoordinatedResetAttach)
	if err != nil {
		s.cancel()
		return nil, errors.Join(err, client.Close())
	}
	go s.run(client, handoff.Cursor, pending)
	return s, nil
}

// Events is the single stream of chat, UI, synthetic reset and status
// events. It is closed after Close. Drain it promptly: a stalled consumer
// eventually costs a reconnect and a reset.
func (s *CoordinatedSession) Events() <-chan sdk.Event { return s.events }

// InstanceID returns the coordinator instance this session targets.
func (s *CoordinatedSession) InstanceID() string { return s.instanceID }

// ClientID returns the stable identity announced on every connection.
func (s *CoordinatedSession) ClientID() string { return s.clientID }

// Connected reports whether a coordinator connection is installed.
func (s *CoordinatedSession) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client != nil
}

// IsStreaming reports whether the instance is running an agent turn.
func (s *CoordinatedSession) IsStreaming() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streaming
}

// ThinkingLevel returns the instance's last reported thinking level.
func (s *CoordinatedSession) ThinkingLevel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.thinking
}

// Stats returns the worker statistics last received by the client.
func (s *CoordinatedSession) Stats() sdk.SessionStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := s.stats
	stats.SessionID, stats.Model, stats.ThinkingLevel = s.sessionID, s.model, s.thinking
	return stats
}

// PendingUIRequests returns the dialogs awaiting an answer from any client,
// in creation order.
func (s *CoordinatedSession) PendingUIRequests() []baseui.SharedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]baseui.SharedRequest, len(s.uiRequests))
	for i, req := range s.uiRequests {
		out[i] = req
		out[i].Options = slices.Clone(req.Options)
	}
	return out
}

// RespondUI answers a shared dialog. resolved is false when another client
// already answered it; the ui_resolved event removes it either way.
func (s *CoordinatedSession) RespondUI(ctx context.Context, resp baseui.UIResponse) (resolved bool, err error) {
	if resp.ID == "" {
		return false, errors.New("ui_response: id required")
	}
	var out struct {
		Resolved bool `json:"resolved"`
	}
	if err := s.call(ctx, coordinatedMutationTimeout, "ui_response", resp, &out); err != nil {
		return false, err
	}
	return out.Resolved, nil
}

// Send issues msg as a prompt and returns an already-closed channel; its
// events arrive on Events. While a turn runs the worker steers it.
func (s *CoordinatedSession) Send(ctx context.Context, msg sdk.Message) (<-chan sdk.Event, error) {
	if err := s.sendDraft(ctx, s.ID(), msg, ""); err != nil {
		return nil, err
	}
	events := make(chan sdk.Event)
	close(events)
	return events, nil
}

// SendInitialDraft submits the first composer message from the worker's CWD.
func (s *CoordinatedSession) SendInitialDraft(ctx context.Context, text string) error {
	state := s.State()
	expanded, images, err := expandTextFileReferences(text, state.CWD())
	if err != nil {
		return err
	}
	return s.sendDraft(ctx, state.SessionID(), sdk.Message{Role: "user", Text: expanded, Images: images}, "")
}

// sendDraft checks the session that was visible when the draft was submitted,
// not the one that happens to be active after preparing its attachments.
func (s *CoordinatedSession) sendDraft(ctx context.Context, sessionID string, msg sdk.Message, behavior string) error {
	args := struct {
		ExpectedSessionID string   `json:"expected_session_id,omitempty"`
		Text              string   `json:"text"`
		Images            []string `json:"images,omitempty"`
		AttachmentIDs     []string `json:"attachment_ids,omitempty"`
		StreamingBehavior string   `json:"streaming_behavior,omitempty"`
	}{sessionID, messageText(msg), messageImages(msg), msg.AttachmentIDs, behavior}
	return s.call(ctx, coordinatedMutationTimeout, "send_message", args, nil)
}

// SendText sends a text-only prompt.
func (s *CoordinatedSession) SendText(ctx context.Context, text string) (<-chan sdk.Event, error) {
	return s.Send(ctx, sdk.Message{Role: "user", Text: text})
}

// Interrupt cancels the instance's in-flight turn and pending dialogs.
func (s *CoordinatedSession) Interrupt() error {
	return s.call(s.ctx, coordinatedReadTimeout, "interrupt", struct{}{}, nil)
}

// Compact asks the instance to compact its session.
func (s *CoordinatedSession) Compact(ctx context.Context, customInstructions ...string) error {
	args := struct {
		CustomInstructions string `json:"custom_instructions,omitempty"`
	}{strings.TrimSpace(strings.Join(customInstructions, "\n"))}
	return s.call(ctx, coordinatedLongTimeout, "compact", args, nil)
}

// State returns the last state reported by the instance.
func (s *CoordinatedSession) State() sdk.SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sdk.NewSessionState(s.sessionID, s.model, s.cwd)
}

// ID returns the instance's active session ID.
func (s *CoordinatedSession) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// IsPersisted reports true: coordinated workers own persisted sessions.
func (s *CoordinatedSession) IsPersisted() bool { return true }

// Close detaches from the instance and closes the connection. The instance
// keeps running.
func (s *CoordinatedSession) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.cancel()
		<-s.done
		s.mu.Lock()
		client := s.client
		s.client, s.closed = nil, true
		s.mu.Unlock()
		if client == nil {
			return
		}
		detachCtx, cancel := context.WithTimeout(ctx, coordinatedReadTimeout)
		defer cancel()
		// Closing the connection ends its subscriptions too, so a detach
		// lost with the connection is not a failure.
		if err := client.Detach(detachCtx, s.instanceID); err != nil && !errors.Is(err, rpcclient.ErrOutcomeUnknown) && client.Err() == nil {
			s.closeErr = fmt.Errorf("detach %s: %w", s.instanceID, err)
		}
		s.closeErr = errors.Join(s.closeErr, client.Close())
	})
	return s.closeErr
}

// SetThinkingLevel changes the instance's thinking level.
func (s *CoordinatedSession) SetThinkingLevel(level string) error {
	args := struct {
		Level string `json:"level"`
	}{level}
	if err := s.call(s.ctx, coordinatedMutationTimeout, "set_thinking_level", args, nil); err != nil {
		return err
	}
	s.mu.Lock()
	s.thinking = level
	s.mu.Unlock()
	return nil
}

// SetModel changes the instance's model for future turns.
func (s *CoordinatedSession) SetModel(model string) error {
	args := struct {
		ModelSpec string `json:"model_spec"`
	}{model}
	var out struct {
		Model string `json:"model"`
	}
	if err := s.call(s.ctx, coordinatedMutationTimeout, "set_model", args, &out); err != nil {
		return err
	}
	s.mu.Lock()
	s.model = cmp.Or(out.Model, model)
	s.mu.Unlock()
	return nil
}

// SlashCommands returns the instance's commands as of the last connection.
func (s *CoordinatedSession) SlashCommands() []sdk.SlashCommandInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

// History pages the instance's persisted history. The newest page comes
// from the last snapshot; older pages are read from the worker in its
// fixed page size and trimmed to limit.
func (s *CoordinatedSession) History(before string, limit int) ([]sdk.HistoryEntry, string, error) {
	s.mu.Lock()
	client, sessionID := s.client, s.sessionID
	entries, next := slices.Clone(s.history), s.historyBefore
	closed := s.closed
	s.mu.Unlock()
	if before != "" {
		switch {
		case closed:
			return nil, "", ErrCoordinatedSessionClosed
		case client == nil:
			return nil, "", ErrCoordinatorUnavailable
		}
		ctx, cancel := context.WithTimeout(s.ctx, coordinatedReadTimeout)
		defer cancel()
		page, err := client.History(ctx, s.instanceID, sessionID, before)
		if err != nil {
			return nil, "", fmt.Errorf("read session history: %w", err)
		}
		entries, next = historyEntries(page.Entries), page.Before
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
		next = entries[0].EntryID
	}
	return entries, next, nil
}

// GetForgetEntries lists the entries /forget can drop from, oldest first.
func (s *CoordinatedSession) GetForgetEntries() ([]sdk.ForkEntry, error) {
	args := struct {
		ExpectedSessionID string `json:"expected_session_id"`
	}{s.ID()}
	var out struct {
		Entries []struct {
			EntryID string `json:"entry_id"`
			Role    string `json:"role"`
			Text    string `json:"text"`
		} `json:"entries"`
	}
	if err := s.call(s.ctx, coordinatedReadTimeout, "get_forget_entries", args, &out); err != nil {
		return nil, err
	}
	entries := make([]sdk.ForkEntry, 0, len(out.Entries))
	for _, e := range out.Entries {
		entries = append(entries, sdk.ForkEntry{EntryID: e.EntryID, Role: e.Role, Text: e.Text})
	}
	return entries, nil
}

// Forget drops entryID and everything after it. The worker announces the
// change, and the snapshot that follows redraws every attached view.
func (s *CoordinatedSession) Forget(entryID string) error {
	args := struct {
		ExpectedSessionID string `json:"expected_session_id"`
		EntryID           string `json:"entry_id"`
	}{s.ID(), entryID}
	return s.call(s.ctx, coordinatedMutationTimeout, "forget", args, nil)
}

// Steer queues msg for the running turn.
func (s *CoordinatedSession) Steer(msg sdk.Message) error {
	args := struct {
		Text   string   `json:"text"`
		Images []string `json:"images,omitempty"`
	}{messageText(msg), messageImages(msg)}
	return s.call(s.ctx, coordinatedMutationTimeout, "steer", args, nil)
}

// FollowUp queues msg to run after the current turn.
func (s *CoordinatedSession) FollowUp(msg sdk.Message) error {
	args := struct {
		ExpectedSessionID string   `json:"expected_session_id,omitempty"`
		Text              string   `json:"text"`
		Images            []string `json:"images,omitempty"`
		AttachmentIDs     []string `json:"attachment_ids,omitempty"`
	}{s.ID(), messageText(msg), messageImages(msg), msg.AttachmentIDs}
	return s.call(s.ctx, coordinatedMutationTimeout, "follow_up", args, nil)
}

// QueueSizes returns the instance's last reported queue sizes.
func (s *CoordinatedSession) QueueSizes() (steering, followUp int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.steering, s.followUp
}

// PauseQueue stops the instance taking messages from one queue.
func (s *CoordinatedSession) PauseQueue(kind sdk.QueueKind) error {
	return s.queueCommand(s.ctx, "pause_queue", kind, nil)
}

// ResumeQueue lets the instance take from one queue again. Any run it starts
// reaches this view through the instance's event stream, so the returned
// channel is always nil.
func (s *CoordinatedSession) ResumeQueue(ctx context.Context, kind sdk.QueueKind) (<-chan sdk.Event, error) {
	return nil, s.queueCommand(ctx, "resume_queue", kind, nil)
}

// QueuesPaused returns the instance's last reported pause state.
func (s *CoordinatedSession) QueuesPaused() (steering, followUp bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.steeringPaused, s.followUpPaused
}

// QueuedPrompts returns the instance's last reported queue previews.
func (s *CoordinatedSession) QueuedPrompts() []sdk.QueuedPrompt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.queued)
}

// EditQueuedText replaces a queued message's text on the instance. It reports
// false when the worker rejects the edit or can't be reached.
func (s *CoordinatedSession) EditQueuedText(id, text string) bool {
	args := struct {
		ExpectedSessionID string `json:"expected_session_id"`
		ID                string `json:"id"`
		Text              string `json:"text"`
	}{s.ID(), id, text}
	return s.call(s.ctx, coordinatedMutationTimeout, "edit_queued_text", args, nil) == nil
}

// takeQueuedText removes the instance's plain-text queued messages and returns
// them in delivery order, with the number left queued for their attachments.
func (s *CoordinatedSession) takeQueuedText(ctx context.Context, sessionID string) ([]string, int, error) {
	args := struct {
		ExpectedSessionID string `json:"expected_session_id"`
	}{sessionID}
	var out struct {
		Steering []string `json:"steering"`
		FollowUp []string `json:"follow_up"`
		Kept     int      `json:"kept"`
	}
	if err := s.call(ctx, coordinatedMutationTimeout, "take_queued_text", args, &out); err != nil {
		return nil, 0, err
	}
	return append(out.Steering, out.FollowUp...), out.Kept, nil
}

// ClearQueuedMessages removes one of the instance's queues and returns its text.
func (s *CoordinatedSession) ClearQueuedMessages(kind sdk.QueueKind) ([]sdk.Message, error) {
	var out struct {
		Steering []string `json:"steering"`
		FollowUp []string `json:"follow_up"`
	}
	if err := s.queueCommand(s.ctx, "clear_queue", kind, &out); err != nil {
		return nil, err
	}
	texts := out.Steering
	if kind == sdk.QueueFollowUp {
		texts = out.FollowUp
	}
	messages := make([]sdk.Message, 0, len(texts))
	for _, text := range texts {
		messages = append(messages, sdk.Message{Role: "user", Text: text})
	}
	return messages, nil
}

func (s *CoordinatedSession) queueCommand(ctx context.Context, command string, kind sdk.QueueKind, out any) error {
	args := struct {
		ExpectedSessionID string `json:"expected_session_id"`
		Queue             string `json:"queue"`
	}{s.ID(), string(kind)}
	return s.call(ctx, coordinatedMutationTimeout, command, args, out)
}

// CycleModel advances the instance to its next model.
func (s *CoordinatedSession) CycleModel() (*ai.Model, error) {
	var out struct {
		Model *struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"model"`
	}
	if err := s.call(s.ctx, coordinatedMutationTimeout, "cycle_model", struct{}{}, &out); err != nil {
		return nil, err
	}
	if out.Model == nil {
		return nil, nil
	}
	spec := out.Model.Provider + "/" + out.Model.ID
	s.mu.Lock()
	s.model = spec
	s.mu.Unlock()
	if model, err := ai.ResolveModelID(spec); err == nil {
		return model, nil
	}
	return &ai.Model{ID: out.Model.ID, Provider: out.Model.Provider}, nil
}

// CycleThinkingLevel advances the instance's thinking level. On failure it
// returns the current level unchanged.
func (s *CoordinatedSession) CycleThinkingLevel() string {
	var out struct {
		Level string `json:"level"`
	}
	if err := s.call(s.ctx, coordinatedMutationTimeout, "cycle_thinking_level", struct{}{}, &out); err != nil {
		return s.ThinkingLevel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.thinking = out.Level
	return s.thinking
}

// SetSteeringMode sets the instance's steering queue mode.
func (s *CoordinatedSession) SetSteeringMode(mode string) error {
	return s.setMode("set_steering_mode", mode)
}

// SetFollowUpMode sets the instance's follow-up queue mode.
func (s *CoordinatedSession) SetFollowUpMode(mode string) error {
	return s.setMode("set_follow_up_mode", mode)
}

func (s *CoordinatedSession) setMode(command, mode string) error {
	args := struct {
		Mode string `json:"mode"`
	}{mode}
	return s.call(s.ctx, coordinatedMutationTimeout, command, args, nil)
}

// ImportAttachmentFile imports a file the worker can read. Coordinated
// workers share this host, so local paths resolve the same way.
func (s *CoordinatedSession) ImportAttachmentFile(path string) (sdk.Attachment, error) {
	args := struct {
		Path string `json:"path"`
	}{path}
	return s.importAttachment("attach_file", args)
}

// ImportAttachmentData imports inline data into the instance's session.
func (s *CoordinatedSession) ImportAttachmentData(filename, mimeType, data string) (sdk.Attachment, error) {
	args := struct {
		ExpectedSessionID string `json:"expected_session_id,omitempty"`
		Filename          string `json:"filename"`
		MimeType          string `json:"mime_type"`
		Data              string `json:"data"`
	}{s.ID(), filename, mimeType, data}
	return s.importAttachment("attach_data", args)
}

func (s *CoordinatedSession) importAttachment(command string, args any) (sdk.Attachment, error) {
	var out struct {
		Attachment sdk.Attachment `json:"attachment"`
	}
	if err := s.call(s.ctx, coordinatedMutationTimeout, command, args, &out); err != nil {
		return sdk.Attachment{}, err
	}
	s.mu.Lock()
	s.attachments = append(s.attachments, out.Attachment)
	s.mu.Unlock()
	return out.Attachment, nil
}

// ListAttachments returns the attachments imported through this session.
func (s *CoordinatedSession) ListAttachments() []sdk.Attachment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.attachments)
}

// DeleteAttachment removes an attachment from the instance's session.
func (s *CoordinatedSession) DeleteAttachment(id string) error {
	args := struct {
		ID string `json:"id"`
	}{id}
	if err := s.call(s.ctx, coordinatedMutationTimeout, "delete_attachment", args, nil); err != nil {
		return err
	}
	s.mu.Lock()
	s.attachments = slices.DeleteFunc(s.attachments, func(a sdk.Attachment) bool { return a.ID == id })
	s.mu.Unlock()
	return nil
}

// ExecuteBash runs a user shell command on the instance. Output streams on
// Events as bash_execution_update; OnOutput is not called. Cancelling ctx
// stops waiting for the reply without interrupting the shared worker.
func (s *CoordinatedSession) ExecuteBash(ctx context.Context, command string, options sdk.BashOptions) (sdk.BashExecution, error) {
	if strings.TrimSpace(command) == "" {
		return sdk.BashExecution{}, errors.New("execute bash: command cannot be empty")
	}
	args := struct {
		Command            string `json:"command"`
		Dir                string `json:"dir,omitempty"`
		TimeoutS           int    `json:"timeout_s,omitempty"`
		ExcludeFromContext bool   `json:"excludeFromContext,omitempty"`
	}{command, options.Dir, int(options.Timeout.Seconds()), options.ExcludeFromContext}
	timeout := coordinatedLongTimeout
	if options.Timeout > 0 {
		timeout = options.Timeout + coordinatedMutationTimeout
	}
	var out struct {
		Output         string `json:"output"`
		ExitCode       int    `json:"exit_code"`
		CancelReason   string `json:"cancel_reason"`
		Cancelled      bool   `json:"cancelled"`
		Truncated      bool   `json:"truncated"`
		FullOutputPath string `json:"full_output_path"`
	}
	err := s.call(ctx, timeout, "bash", args, &out)
	if perr, ok := errors.AsType[*coord.Error](err); ok && perr.Code == "blocked" {
		return sdk.BashExecution{}, fmt.Errorf("%w: %s", sdk.ErrBashBlocked, perr.Message)
	}
	if err != nil {
		return sdk.BashExecution{}, err
	}
	return sdk.BashExecution{
		Command:            command,
		Output:             out.Output,
		ExitCode:           out.ExitCode,
		CancelReason:       out.CancelReason,
		Cancelled:          out.Cancelled,
		Truncated:          out.Truncated,
		FullOutputPath:     out.FullOutputPath,
		ExcludeFromContext: options.ExcludeFromContext,
	}, nil
}

func (s *CoordinatedSession) abortBash() error {
	return s.call(s.ctx, coordinatedReadTimeout, "abort_bash", struct{}{}, nil)
}

// call sends one worker command, bounded by timeout, and decodes its
// result into out when out is non-nil. It never resends.
func (s *CoordinatedSession) call(ctx context.Context, timeout time.Duration, command string, args, out any) error {
	s.mu.Lock()
	client, closed := s.client, s.closed
	s.mu.Unlock()
	switch {
	case closed:
		return fmt.Errorf("%s: %w", command, ErrCoordinatedSessionClosed)
	case client == nil:
		return fmt.Errorf("%s: %w", command, ErrCoordinatorUnavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := client.Command(ctx, s.instanceID, rpcclient.Command{Type: command, Args: args})
	if err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	if !resp.OK {
		if resp.Error != nil {
			return fmt.Errorf("%s: %w", command, &coord.Error{Code: resp.Error.Code, Message: resp.Error.Message})
		}
		return fmt.Errorf("%s: failed without error details", command)
	}
	if out == nil || resp.Result == nil {
		return nil
	}
	data, err := json.Marshal(resp.Result)
	if err != nil {
		return fmt.Errorf("%s: encode result: %w", command, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: decode result: %w", command, err)
	}
	return nil
}

// connect dials and attaches. With resume it first continues from that
// position, falling back to a snapshot when the worker requires a resync.
func (s *CoordinatedSession) connect(ctx context.Context, resume *coord.Position, previousGeneration string) (*rpcclient.CoordinatedClient, *rpcclient.Handoff, error) {
	ctx, cancel := context.WithTimeout(ctx, coordinatedDialTimeout)
	defer cancel()
	client, err := rpcclient.NewCoordinatedClient(ctx, s.socket, rpcclient.WithClientID(s.clientID))
	if err != nil {
		return nil, nil, err
	}
	var handoff *rpcclient.Handoff
	if resume != nil {
		handoff, err = client.Resume(ctx, *resume)
		if errors.Is(err, rpcclient.ErrResyncRequired) {
			handoff, err = nil, nil
		}
	}
	if err == nil && handoff == nil {
		handoff, err = client.AttachSnapshot(ctx, s.instanceID)
	}
	if err != nil {
		if previousGeneration != "" {
			err = s.checkWorkerExit(ctx, client, previousGeneration, err)
		}
		return nil, nil, errors.Join(err, client.Close())
	}
	return client, handoff, nil
}

func (s *CoordinatedSession) checkWorkerExit(ctx context.Context, client *rpcclient.CoordinatedClient, previousGeneration string, err error) error {
	protocolErr, ok := errors.AsType[*coord.Error](err)
	if !ok || (protocolErr.Code != coord.CodeNotFound && protocolErr.Code != coord.CodeUnavailable) {
		return err
	}
	instances, listErr := client.ListInstances(ctx)
	if listErr != nil {
		return errors.Join(err, listErr)
	}
	for _, inst := range instances {
		if inst.ID == s.instanceID {
			if inst.LinkState == coord.LinkExited {
				return errCoordinatedWorkerExited
			}
			return err
		}
	}
	// A replacement coordinator starts with an empty registry. Give the
	// worker time to register rather than treating its absence as an exit.
	if client.Generation() == previousGeneration {
		return errCoordinatedWorkerExited
	}
	return err
}

// install makes client current and converts its handoff into the events
// that must be emitted before live ones.
func (s *CoordinatedSession) install(client *rpcclient.CoordinatedClient, handoff *rpcclient.Handoff, reason string) ([]sdk.Event, error) {
	var events []sdk.Event
	if handoff.Snapshot != nil {
		events = append(events, s.applySnapshot(handoff.Snapshot, reason))
	}
	for _, f := range handoff.Events {
		ev, ok, err := coordinatedEvent(f)
		if err != nil {
			return nil, err
		}
		if ok {
			if s.needsSnapshot(ev) {
				return nil, rpcclient.ErrResyncRequired
			}
			events = append(events, ev)
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrCoordinatedSessionClosed
	}
	s.client = client
	s.mu.Unlock()
	go s.refreshCommands()
	return events, nil
}

// run delivers events until Close, reconnecting to the same instance
// whenever the stream ends or can no longer be continued.
func (s *CoordinatedSession) run(client *rpcclient.CoordinatedClient, cursor *rpcclient.EventCursor, pending []sdk.Event) {
	defer close(s.done)
	defer close(s.events)
	for {
		for _, ev := range pending {
			s.observe(ev)
			if !s.emit(ev) {
				return
			}
		}
		resync, err := s.pump(client, cursor)
		if s.ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		if s.client == client {
			s.client = nil
		}
		s.mu.Unlock()
		err = errors.Join(err, client.Close())
		if errors.Is(err, errCoordinatedWorkerExited) {
			s.reportWorkerExit(err)
			return
		}
		if !s.emit(coordinatedStatus(s.instanceID, CoordinatedReconnecting, err)) {
			return
		}
		var resume *coord.Position
		if !resync {
			position := cursor.Position()
			resume = &position
		}
		client, cursor, pending = s.reconnect(resume, client.Generation(), err)
		if client == nil {
			return
		}
	}
}

// pump applies live frames for the instance. It returns when the client's
// stream ends, or with resync set when the cursor rejects a frame.
func (s *CoordinatedSession) pump(client *rpcclient.CoordinatedClient, cursor *rpcclient.EventCursor) (resync bool, err error) {
	for {
		select {
		case <-s.ctx.Done():
			return false, nil
		case f, ok := <-client.Events():
			if !ok {
				return false, client.Err()
			}
			if f.Type != coord.TypeEvent || f.InstanceID != s.instanceID {
				continue
			}
			if f.Instance != nil && f.Instance.LinkState == coord.LinkExited {
				return false, errCoordinatedWorkerExited
			}
			apply, err := cursor.Accept(f)
			if err != nil {
				return true, err
			}
			if !apply {
				continue
			}
			ev, ok, err := coordinatedEvent(f)
			if err != nil {
				return true, err
			}
			if !ok {
				continue
			}
			if s.needsSnapshot(ev) {
				if ev.Type != "compaction_end" && ev.Type != "context_edited" {
					return true, fmt.Errorf("%s changed the session view", ev.Type)
				}
				// The worker publishes these events before replying to the command
				// that caused them. Keep the connection open so the reply can arrive.
				ctx, cancel := context.WithTimeout(s.ctx, coordinatedReadTimeout)
				snap, err := client.Snapshot(ctx, s.instanceID)
				cancel()
				if err != nil {
					return true, fmt.Errorf("refresh after %s: %w", ev.Type, err)
				}
				pos := cursor.Position()
				if snap.Position.Epoch != pos.Epoch || snap.Position.Offset < pos.Offset {
					return true, fmt.Errorf("snapshot does not cover %s", ev.Type)
				}
				*cursor = *rpcclient.NewEventCursor(snap.Position)
				ev = s.applySnapshot(snap, CoordinatedResetResync)
			}
			s.observe(ev)
			if !s.emit(ev) {
				return false, nil
			}
		}
	}
}

func (s *CoordinatedSession) reportWorkerExit(err error) {
	s.mu.Lock()
	s.streaming = false
	s.steering, s.followUp = 0, 0
	s.steeringPaused, s.followUpPaused = false, false
	s.queued = nil
	s.uiRequests = nil
	s.mu.Unlock()
	s.emit(coordinatedStatus(s.instanceID, CoordinatedExited, err))
}

// reconnect retries until it reattaches, confirms exit, or the TUI closes.
func (s *CoordinatedSession) reconnect(resume *coord.Position, previousGeneration string, cause error) (*rpcclient.CoordinatedClient, *rpcclient.EventCursor, []sdk.Event) {
	reported := errorText(cause)
	for delay := coordinatedRetryMin; ; delay = min(2*delay, coordinatedRetryMax) {
		timer := time.NewTimer(delay)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return nil, nil, nil
		case <-timer.C:
		}
		client, handoff, err := s.connect(s.ctx, resume, previousGeneration)
		if err == nil {
			reason := CoordinatedResetReconnect
			if resume == nil {
				reason = CoordinatedResetResync
			}
			var pending []sdk.Event
			if pending, err = s.install(client, handoff, reason); err == nil {
				status := coordinatedStatus(s.instanceID, CoordinatedConnected, nil)
				return client, handoff.Cursor, append([]sdk.Event{status}, pending...)
			}
			err = errors.Join(err, client.Close())
		}
		if s.ctx.Err() != nil {
			return nil, nil, nil
		}
		if errors.Is(err, errCoordinatedWorkerExited) {
			s.reportWorkerExit(err)
			return nil, nil, nil
		}
		if errors.Is(err, rpcclient.ErrResyncRequired) {
			resume = nil
		}
		if text := errorText(err); text != reported {
			reported = text
			if !s.emit(coordinatedStatus(s.instanceID, CoordinatedReconnecting, err)) {
				return nil, nil, nil
			}
		}
	}
}

func (s *CoordinatedSession) emit(ev sdk.Event) bool {
	select {
	case s.events <- ev:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// refreshCommands replaces the cached command list. On failure the
// previous list stays until the next connection refreshes it.
func (s *CoordinatedSession) refreshCommands() {
	var out struct {
		Commands []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
		} `json:"commands"`
	}
	if err := s.call(s.ctx, coordinatedReadTimeout, "get_commands", struct{}{}, &out); err != nil {
		return
	}
	commands := make([]sdk.SlashCommandInfo, 0, len(out.Commands))
	for _, c := range out.Commands {
		commands = append(commands, sdk.SlashCommandInfo{Name: c.Name, Description: c.Description, Source: c.Source})
	}
	s.mu.Lock()
	s.commands = commands
	s.mu.Unlock()
}

// applySnapshot replaces cached state with snap and returns its reset event.
func (s *CoordinatedSession) applySnapshot(snap *rpcclient.Snapshot, reason string) sdk.Event {
	reset := CoordinatedReset{
		Reason:        reason,
		InstanceID:    s.instanceID,
		SessionID:     cmp.Or(snap.State.SessionID, snap.Instance.SessionID),
		History:       historyEntries(snap.History.Entries),
		HistoryBefore: snap.History.Before,
		Snapshot:      snap,
	}
	if len(snap.Live) > 0 {
		if err := json.Unmarshal(snap.Live, &reset.Live); err != nil {
			reset.Live = CoordinatedLive{Truncated: true}
		}
	}
	for _, raw := range snap.UIRequests {
		var req baseui.SharedRequest
		if err := json.Unmarshal(raw, &req); err != nil || req.ID == "" {
			continue
		}
		if len(reset.UIRequests) < coordinatedMaxUIRequests {
			reset.UIRequests = append(reset.UIRequests, req)
		}
	}
	var queue struct {
		Steering       int                `json:"steering"`
		FollowUp       int                `json:"follow_up"`
		SteeringPaused bool               `json:"steering_paused"`
		FollowUpPaused bool               `json:"follow_up_paused"`
		Items          []sdk.QueuedPrompt `json:"items"`
	}
	if len(snap.State.Queue) > 0 && json.Unmarshal(snap.State.Queue, &queue) != nil {
		queue.Steering, queue.FollowUp = 0, 0
		queue.SteeringPaused, queue.FollowUpPaused, queue.Items = false, false, nil
	}
	s.mu.Lock()
	s.sessionID = reset.SessionID
	s.model = snapshotModel(snap.State.Model)
	s.thinking = snap.State.ThinkingLevel
	s.stats = sdk.SessionStats{}
	if stats := snap.State.Stats; stats != nil {
		s.stats = *stats
		s.thinking = stats.ThinkingLevel
	}
	s.streaming = snap.State.IsStreaming
	if snap.Instance.CWD != "" {
		s.cwd = snap.Instance.CWD
	}
	s.steering, s.followUp = queue.Steering, queue.FollowUp
	s.steeringPaused, s.followUpPaused = queue.SteeringPaused, queue.FollowUpPaused
	s.queued = queue.Items
	s.history, s.historyBefore = slices.Clone(reset.History), reset.HistoryBefore
	s.uiRequests = slices.Clone(reset.UIRequests)
	s.mu.Unlock()
	return sdk.Event{Type: CoordinatedResetEvent, SessionID: reset.SessionID, Timestamp: time.Now().Format(time.RFC3339Nano), Payload: reset}
}

func (s *CoordinatedSession) needsSnapshot(ev sdk.Event) bool {
	switch ev.Type {
	case "context_edited":
		return true
	case "compaction_end":
		fields, _ := ev.Payload.(map[string]any)
		aborted, _ := fields["aborted"].(bool)
		return !aborted && payloadString(fields, "error_message") == ""
	case "session_start":
		return ev.SessionID != "" && ev.SessionID != s.ID()
	default:
		return false
	}
}

// observe tracks the state an event changes before it is emitted.
func (s *CoordinatedSession) observe(ev sdk.Event) {
	p, _ := ev.Payload.(map[string]any)
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.SessionID != "" && ev.SessionID != s.sessionID && ev.Type != "session_start" {
		return
	}
	if stats, ok := p["stats"].(sdk.SessionStats); ok {
		s.stats = stats
		s.thinking = stats.ThinkingLevel
		s.model = cmp.Or(stats.Model, s.model)
	}
	switch ev.Type {
	case "session_start":
		s.sessionID = cmp.Or(payloadString(p, "id"), ev.SessionID, s.sessionID)
		s.model = cmp.Or(payloadString(p, "model"), s.model)
		s.cwd = cmp.Or(payloadString(p, "cwd"), s.cwd)
	case "agent_start":
		s.streaming = true
	case "agent_end", "session_end", "session_shutdown":
		s.streaming = false
	case "model_change":
		s.model = cmp.Or(payloadString(p, "model"), s.model)
	case "thinking_level_change":
		if _, ok := p["stats"]; !ok {
			s.thinking = cmp.Or(payloadString(p, "level"), s.thinking)
		}
	case "queue_update":
		s.steering, s.followUp = payloadInt(p, "steering"), payloadInt(p, "follow_up")
		steeringPaused, _ := p["steering_paused"].(bool)
		followUpPaused, _ := p["follow_up_paused"].(bool)
		s.steeringPaused, s.followUpPaused = steeringPaused, followUpPaused
		s.queued = queuedPromptsFromPayload(p["items"])
	case "ui_confirm", "ui_select", "ui_input":
		req := baseui.SharedRequest{
			ID:          payloadString(p, "id"),
			Method:      baseui.UIMethod(strings.TrimPrefix(ev.Type, "ui_")),
			Title:       payloadString(p, "title"),
			Message:     payloadString(p, "message"),
			Placeholder: payloadString(p, "placeholder"),
		}
		if options, ok := p["options"].([]any); ok {
			for _, option := range options {
				if text, ok := option.(string); ok {
					req.Options = append(req.Options, text)
				}
			}
		}
		if req.ID == "" {
			return
		}
		if i := slices.IndexFunc(s.uiRequests, func(r baseui.SharedRequest) bool { return r.ID == req.ID }); i >= 0 {
			s.uiRequests[i] = req
		} else if len(s.uiRequests) < coordinatedMaxUIRequests {
			s.uiRequests = append(s.uiRequests, req)
		}
	case "ui_resolved":
		id := payloadString(p, "id")
		s.uiRequests = slices.DeleteFunc(s.uiRequests, func(r baseui.SharedRequest) bool { return r.ID == id })
	}
}

// coordinatedEvent converts a worker event frame into an sdk.Event whose
// Payload holds the flattened event fields. ok is false for untyped events.
func coordinatedEvent(f coord.Frame) (sdk.Event, bool, error) {
	var fields map[string]any
	if err := json.Unmarshal(f.Event, &fields); err != nil {
		return sdk.Event{}, false, fmt.Errorf("decode coordinated event: %w", err)
	}
	ev := sdk.Event{
		Type:      payloadString(fields, "type"),
		SessionID: cmp.Or(payloadString(fields, "session_id"), f.SessionID),
		Timestamp: payloadString(fields, "ts"),
	}
	if raw, ok := fields["stats"]; ok {
		data, err := json.Marshal(raw)
		if err != nil {
			return sdk.Event{}, false, fmt.Errorf("encode worker statistics: %w", err)
		}
		var stats sdk.SessionStats
		if err := json.Unmarshal(data, &stats); err != nil {
			return sdk.Event{}, false, fmt.Errorf("decode worker statistics: %w", err)
		}
		fields["stats"] = stats
	}
	if seq, ok := fields["seq"].(float64); ok {
		ev.Seq = int64(seq)
	}
	for _, key := range []string{"type", "session_id", "seq", "ts"} {
		delete(fields, key)
	}
	if fields == nil {
		fields = map[string]any{}
	}
	ev.Payload = fields
	return ev, ev.Type != "", nil
}

func coordinatedStatus(instanceID, state string, err error) sdk.Event {
	payload := map[string]any{"state": state, "instance_id": instanceID}
	if err != nil {
		payload["error"] = err.Error()
	}
	return sdk.Event{Type: CoordinatedStatusEvent, Timestamp: time.Now().Format(time.RFC3339Nano), Payload: payload}
}

func historyEntries(entries []rpcclient.HistoryEntry) []sdk.HistoryEntry {
	out := make([]sdk.HistoryEntry, 0, len(entries))
	for _, entry := range entries {
		msg := sdk.Message{Role: "system", Text: omittedHistoryText}
		if !entry.Omitted && len(entry.Message) > 0 {
			var decoded sdk.Message
			if err := json.Unmarshal(entry.Message, &decoded); err == nil {
				msg = decoded
			}
		}
		out = append(out, sdk.HistoryEntry{EntryID: entry.EntryID, Message: msg})
	}
	return out
}

// snapshotModel accepts a "provider/model" string or a model object.
func snapshotModel(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var spec string
	if err := json.Unmarshal(raw, &spec); err == nil {
		return spec
	}
	var model struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(raw, &model); err != nil || model.ID == "" {
		return ""
	}
	if model.Provider == "" {
		return model.ID
	}
	return model.Provider + "/" + model.ID
}

func messageText(msg sdk.Message) string {
	if msg.Text != "" {
		return msg.Text
	}
	var parts []string
	for _, block := range msg.Content {
		if block.Type == ai.ContentBlockText && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// messageImages returns base64 image data for the worker to decode.
func messageImages(msg sdk.Message) []string {
	var images []string
	for _, block := range slices.Concat(msg.Content, msg.Images) {
		if block.Type == ai.ContentBlockImage && block.Data != "" {
			images = append(images, block.Data)
		}
	}
	return images
}

func payloadString(fields map[string]any, key string) string {
	text, _ := fields[key].(string)
	return text
}

// queuedPromptsFromPayload decodes a queue_update's previews. A malformed list
// reads as empty, which only makes those messages uneditable from this view.
func queuedPromptsFromPayload(raw any) []sdk.QueuedPrompt {
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var items []sdk.QueuedPrompt
	if err := json.Unmarshal(data, &items); err != nil {
		return nil
	}
	return items
}

func payloadInt(fields map[string]any, key string) int {
	n, _ := fields[key].(float64)
	return int(n)
}
