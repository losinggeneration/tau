package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
)

const workerAdmissionSlots = 4

// Worker owns one runtime independently of its coordinator connection.
// A Worker is single-use; losing its link does not stop admitted work.
type Worker struct {
	server *Server
}

// NewWorker transfers runtime ownership to a coordinated worker.
func NewWorker(rt sdk.Runtime, reg commands.Registry, mgr *plugins.Manager) *Worker {
	s := newServer(rt, reg, mgr)
	s.coordinated = true
	return &Worker{server: s}
}

// Run initializes a session and serves until explicit shutdown or process
// cancellation. A failed link is replaced without cancelling the runtime.
func (w *Worker) Run(ctx context.Context, path string, instance coord.Instance, token string) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := w.server
	output := &workerWriter{
		instance: instance, server: s,
		journal: eventJournal{position: coord.Position{InstanceID: instance.ID, Epoch: uuid.NewString()}},
	}
	s.writer = output
	s.ui = ui.NewRPCUI()
	s.ui.Writer = rpcUIWriter{server: s}
	s.ui.EnableSharedRequests()
	if s.pluginManager != nil {
		s.pluginManager.SetUI(s.ui)
	}
	var admission, reads sync.WaitGroup
	cleaned := false
	cleanup := func() error {
		cancel()
		admission.Wait()
		reads.Wait()
		cleaned = true
		cleanupErr := s.cleanup(ctx)
		closeCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
		defer stop()
		if s.runtime != nil {
			if closeErr := s.runtime.Close(closeCtx); closeErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close worker runtime: %w", closeErr))
			}
		}
		// SDK runtimes own their manager; alternate runtimes may not.
		if s.pluginManager != nil && (s.runtime == nil || s.runtime.PluginManager() != s.pluginManager) {
			if closeErr := s.pluginManager.Shutdown(closeCtx); closeErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close worker plugins: %w", closeErr))
			}
		}
		return cleanupErr
	}
	defer func() {
		if !cleaned {
			err = errors.Join(err, cleanup())
		}
	}()
	if s.runtime == nil {
		return errors.New("worker runtime required")
	}
	if err := s.replaceSession(ctx, sdk.SessionOptions{CWD: instance.CWD, SessionID: instance.SessionID}, "worker_start"); err != nil {
		return fmt.Errorf("initialize worker session: %w", err)
	}
	output.mu.Lock()
	if output.instance.State == "" {
		output.instance.State = coord.StateReady
	}
	output.mu.Unlock()
	link, err := output.dial(ctx, path, token)
	if err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	linkCtx, stopLinks := context.WithCancel(context.WithoutCancel(ctx))
	var reader sync.WaitGroup
	frames := make(chan coord.Frame)
	reader.Go(func() {
		defer close(frames)
		output.readLinks(linkCtx, ctx, path, token, link, frames)
	})
	defer func() {
		if !cleaned {
			err = errors.Join(err, cleanup())
		}
		stopLinks()
		reader.Wait()
		output.closing.Wait()
	}()
	// Only one ordinary handler runs at a time. Frame size bounds the total
	// payload in the waiting slots to 4 MiB.
	// The reader never waits for that handler, so UI replies and cancellation
	// remain serviceable even while slash commands or compaction block.
	pending := make(chan *Command, workerAdmissionSlots)
	reconstruction := make(chan *Command, workerAdmissionSlots)
	var sessionChangeMu sync.Mutex
	var (
		handlerMu     sync.Mutex
		cancelHandler context.CancelFunc
	)
	admission.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case cmd := <-pending:
				if ctx.Err() != nil {
					return
				}
				// Prompt, bash, and session handlers pass ctx on to work that
				// outlives dispatch. Slash commands, compaction, and MCP
				// reconnects run inline and stop on interrupt.
				if cmd.Type != "slash_command" && cmd.Type != "compact" && cmd.Type != "mcp_reconnect" {
					switch cmd.Type {
					case "new_session", "create_session", "resume_session", "fork":
						sessionChangeMu.Lock()
						if ctx.Err() == nil {
							w.dispatch(ctx, cmd)
						}
						sessionChangeMu.Unlock()
					default:
						w.dispatch(ctx, cmd)
					}
					continue
				}
				handlerCtx, stop := context.WithCancel(ctx)
				handlerMu.Lock()
				cancelHandler = stop
				handlerMu.Unlock()
				w.dispatch(handlerCtx, cmd)
				stop()
				handlerMu.Lock()
				cancelHandler = nil
				handlerMu.Unlock()
			}
		}
	})
	reads.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case cmd := <-reconstruction:
				if ctx.Err() != nil {
					return
				}
				handle := output.replayEvents
				switch cmd.Type {
				case "get_snapshot":
					handle = output.getSnapshot
				case "get_history":
					handle = output.getHistory
				case "get_entry_content":
					handle = output.getEntryContent
				}
				if err := handle(cmd); err != nil {
					workerLog("send reconstruction reply: %v", err)
				}
			}
		}
	})
	for {
		select {
		case <-ctx.Done():
			return nil
		case frame, ok := <-frames:
			if !ok {
				return nil
			}
			if frame.Type != coord.TypeCommand {
				continue
			}
			if frame.InstanceID != "" && frame.InstanceID != instance.ID {
				if err := s.sendError(frame.ID, "invalid_target", "command targets another instance"); err != nil {
					return err
				}
				continue
			}
			var cmd Command
			if decodeErr := json.Unmarshal(frame.Command, &cmd); decodeErr != nil {
				if err := s.sendError(frame.ID, "invalid_command", decodeErr.Error()); err != nil {
					return err
				}
				continue
			}
			cmd.ID = frame.ID
			if cmd.Type == "shutdown" {
				var args struct {
					ExpectedSessionID string `json:"expected_session_id"`
				}
				if len(cmd.Args) != 0 {
					if err := json.Unmarshal(cmd.Args, &args); err != nil {
						if err := s.sendError(cmd.ID, coord.CodeInvalidFrame, "invalid shutdown arguments"); err != nil {
							return err
						}
						continue
					}
				}
				if args.ExpectedSessionID != "" {
					if !sessionChangeMu.TryLock() {
						if err := s.sendError(cmd.ID, coord.CodeBusy, "instance is switching sessions; select it again"); err != nil {
							return err
						}
						continue
					}
					problem := s.prepareRestart(ctx, args.ExpectedSessionID, instance.CWD)
					if problem == nil {
						cancel()
					}
					sessionChangeMu.Unlock()
					if problem != nil {
						if err := s.sendError(cmd.ID, problem.Code, problem.Message); err != nil {
							return err
						}
						continue
					}
				}
				cleanupErr := cleanup()
				if cleanupErr != nil {
					return errors.Join(cleanupErr, s.sendErrorResult(cmd.ID, cleanupErr))
				}
				return s.sendResult(cmd.ID, map[string]any{"ok": true})
			}
			switch cmd.Type {
			case "interrupt", "abort":
				// SDK prompt admission can also run a slash command inline.
				// Resolving dialogs before cancelling the handler reports them
				// as interrupted rather than racing the handler's context.
				s.ui.CancelPending()
				handlerMu.Lock()
				if cancelHandler != nil {
					cancelHandler()
				}
				handlerMu.Unlock()
				w.dispatch(ctx, &cmd)
			case "replay_events", "get_snapshot", "get_history", "get_entry_content":
				select {
				case reconstruction <- &cmd:
				default:
					if err := s.sendError(cmd.ID, coord.CodeBusy, "worker reconstruction admission is full"); err != nil {
						return err
					}
				}
			case "ui_response", "get_ui_requests", "abort_bash":
				w.dispatch(ctx, &cmd)
			default:
				select {
				case pending <- &cmd:
				default:
					if err := s.sendError(cmd.ID, "busy", "worker command admission is full"); err != nil {
						return err
					}
				}
			}
		}
	}
}

func (s *Server) prepareRestart(ctx context.Context, expected, project string) *coord.Error {
	if !s.sessionMu.TryLock() {
		return &coord.Error{Code: coord.CodeBusy, Message: "instance is switching sessions; select it again"}
	}
	sess := s.session
	s.sessionMu.Unlock()
	if sess == nil || sess.ID() != expected {
		return &coord.Error{Code: coord.CodeConflict, Message: "instance switched sessions; select it again"}
	}
	if sess.State().CWD() != project {
		return &coord.Error{Code: coord.CodeConflict, Message: "active session belongs to another project; worker was not stopped"}
	}
	persistent, ok := sess.(interface{ PrepareRestart(context.Context) error })
	if !ok {
		return &coord.Error{Code: coord.CodeUnavailable, Message: "worker cannot save this session for restart"}
	}
	if err := persistent.PrepareRestart(ctx); err != nil {
		return &coord.Error{Code: coord.CodeUnavailable, Message: err.Error()}
	}
	return nil
}

func (w *Worker) dispatch(ctx context.Context, cmd *Command) {
	s := w.server
	if err := s.handleCommand(ctx, cmd); err != nil {
		if sendErr := s.sendError(cmd.ID, "handler_error", err.Error()); sendErr != nil {
			// Encoding failures are surfaced to the process, not retried.
			workerLog("send handler error: %v", errors.Join(err, sendErr))
		}
	}
	if cmd.Type == "interrupt" || cmd.Type == "abort" {
		select {
		case <-s.interrupt:
			if err := s.emitEvent(sdk.Event{Type: "interrupt"}); err != nil {
				workerLog("emit interrupt: %v", err)
			}
		default:
		}
	}
}

// workerWriter adapts serialized RPC records to the bounded worker stream.
// Link.Send itself is nonblocking and bounded.
type workerWriter struct {
	server            *Server
	bound             *eventSession
	mu                sync.Mutex
	link              *coord.Link
	instancePrompts   bool
	instanceBuild     bool
	instance          coord.Instance
	closing           sync.WaitGroup
	journal           eventJournal
	live              liveState
	contentGeneration uint64
	content           []entryContentCache
}

func (w *workerWriter) dial(ctx context.Context, path, token string) (*coord.Link, error) {
	w.mu.Lock()
	inst, pos := w.instance, w.journal.position
	hello := inst
	hello.FirstPrompt, hello.BuildID = "", ""
	w.mu.Unlock()
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	link, welcome, err := coord.Dial(probe, path, coord.Frame{
		Role: coord.RoleWorker, Instance: &hello, Token: token, Position: &pos,
		Operations: []string{coord.OperationInstanceRestart},
	})
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.link = link
	w.instancePrompts = slices.Contains(welcome.Operations, coord.OperationInstancePrompts)
	w.instanceBuild = slices.Contains(welcome.Operations, coord.OperationInstanceBuild)
	// A hello must also work with coordinators that predate previews.
	// Changes made during the handshake were journaled but could not be
	// forwarded. Publish the latest summary before forwarding new events.
	if w.instance != inst || (w.instancePrompts && inst.FirstPrompt != "") || (w.instanceBuild && inst.BuildID != "") {
		if err := w.sendSummaryLocked(); err != nil {
			w.disconnect(err)
		}
	}
	return link, nil
}

func (w *workerWriter) readLinks(ctx, work context.Context, path, token string, link *coord.Link, frames chan<- coord.Frame) {
	for {
		// A blocked Read must finish after explicit stop, even if the peer
		// never sends another frame. Close flushes the shutdown reply first.
		closed := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			closeWorkerLink(link)
			close(closed)
		})
		w.readCommands(ctx, work, link, frames)
		if !stop() {
			<-closed
		}
		w.loseLink(link)
		if ctx.Err() != nil || work.Err() != nil {
			return
		}
		for delay := 100 * time.Millisecond; ; delay = min(2*delay, 3*time.Second) {
			timer := time.NewTimer(delay)
			select {
			case <-work.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			next, err := w.dial(work, path, token)
			if err == nil {
				link = next
				break
			}
			if delay == 100*time.Millisecond {
				workerLog("reconnect coordinator: %v", err)
			}
		}
	}
}

func (w *workerWriter) readCommands(ctx, work context.Context, link *coord.Link, frames chan<- coord.Frame) {
	for {
		frame, err := link.Read()
		if err != nil {
			return
		}
		if frame.Type == coord.TypePing {
			if err := link.Send(coord.Frame{Type: coord.TypePong}); err != nil {
				w.loseLink(link)
				return
			}
			continue
		}
		if frame.Type != coord.TypeCommand {
			continue
		}
		select {
		case frames <- frame:
		case <-ctx.Done():
			return
		case <-work.Done():
			// Keep reading pings while cleanup owns the command loop.
			if err := link.Send(coord.Frame{Type: coord.TypeResponse, ID: frame.ID, Error: &coord.Error{
				Code: coord.CodeUnavailable, Message: "worker is stopping",
			}}); err != nil {
				w.loseLink(link)
				return
			}
		}
	}
}

func (w *workerWriter) Write(data []byte) (int, error) {
	var record struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
		Seq       int64  `json:"seq"`
		Timestamp string `json:"ts"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return 0, fmt.Errorf("decode worker output: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var summaryChanged bool
	if w.server != nil {
		if bound := w.server.eventSession.Load(); bound != nil && bound.id != "" && bound != w.bound {
			w.bound = bound
			if bound.id != w.instance.SessionID {
				w.instance.SessionID = bound.id
				w.instance.State = coord.StateReady
				w.live = liveState{}
				w.content = nil
				w.contentGeneration++
				summaryChanged = true
			}
			if w.instance.FirstPrompt != bound.firstPrompt {
				w.instance.FirstPrompt = bound.firstPrompt
				summaryChanged = true
			}
		}
	}
	frame := coord.Frame{Type: coord.TypeEvent, InstanceID: w.instance.ID, SessionID: record.SessionID, Event: json.RawMessage(data)}
	var omitted bool
	if record.Type == "response" {
		frame.Type, frame.ID, frame.Event, frame.Result = coord.TypeResponse, record.ID, nil, json.RawMessage(data)
	} else {
		if frame.SessionID == "" {
			frame.SessionID = w.instance.SessionID
		}
		position := w.journal.position
		position.Offset++
		frame.Position = &position
		// The replay entry has two bounded identifiers from registration and
		// the raw event, plus fixed fields. Leave room for JSON escaping.
		size := len(data) + 256 + 12*len(frame.InstanceID) + 6*(len(frame.SessionID)+len(position.Epoch))
		if size > replayPageBytes {
			notice, err := json.Marshal(map[string]any{
				"type": "event_omitted", "code": coord.CodeLimitExceeded, "event_type": record.Type,
				"session_id": frame.SessionID, "seq": record.Seq, "ts": record.Timestamp,
			})
			if err != nil {
				return 0, fmt.Errorf("encode omitted event: %w", err)
			}
			size += len(notice) - len(data)
			frame.Event, omitted = notice, true
		}
		if frame.SessionID == w.instance.SessionID {
			if record.Type == "context_edited" {
				w.content = nil
				w.contentGeneration++
			}
			if err := w.live.update(record.Type, json.RawMessage(data)); err != nil {
				w.live.truncated = true
				workerLog("%v", err)
			}
		}
		w.instance.LastActivity = time.Now().UTC()
		w.journal.position = position
		w.journal.publish(frame, size)
	}
	if w.link != nil && summaryChanged {
		if err := w.sendSummaryLocked(); err != nil {
			w.disconnect(err)
		}
	}
	if w.link != nil {
		err := w.link.Send(frame)
		if errors.Is(err, coord.ErrFrameTooLarge) && frame.Type == coord.TypeResponse {
			frame.Result = nil
			frame.Error = &coord.Error{Code: coord.CodeLimitExceeded, Message: "worker reply exceeds frame size limit"}
			err = w.link.Send(frame)
		}
		if err != nil {
			w.disconnect(err)
		}
	}
	if frame.Type == coord.TypeEvent && frame.SessionID == w.instance.SessionID {
		if err := w.publishSummaryLocked(record.Type, data); err != nil {
			w.disconnect(err)
		}
	}
	if omitted && record.Type != "ui_resolved" && isSharedUIRecord(record.Type) {
		return 0, coord.ErrFrameTooLarge
	}
	return len(data), nil
}

func isSharedUIRecord(kind string) bool {
	switch kind {
	case "ui_confirm", "ui_select", "ui_input", "ui_resolved":
		return true
	}
	return false
}

// publishSummaryLocked reports lifecycle, attention, and the first user
// prompt preview. Caller holds mu.
func (w *workerWriter) publishSummaryLocked(kind string, data []byte) error {
	previous := w.instance
	switch kind {
	case "message_end":
		if w.instance.FirstPrompt == "" {
			var msg struct {
				Role, Content string
			}
			if err := json.Unmarshal(data, &msg); err != nil {
				workerLog("decode prompt preview: %v", err)
			} else if msg.Role == "user" {
				w.instance.FirstPrompt = instancePromptPreview(msg.Content)
				if !w.instancePrompts {
					return nil
				}
			}
		}
	case "agent_start":
		w.instance.State = coord.StateBusy
	case "agent_end", "session_shutdown":
		w.instance.State = coord.StateReady
	}
	if isSharedUIRecord(kind) && w.server != nil && w.server.ui != nil {
		w.instance.PendingInputs = w.server.ui.PendingRequests()
	}
	if w.link == nil || w.instance == previous {
		return nil
	}
	return w.sendSummaryLocked()
}

func (w *workerWriter) sendSummaryLocked() error {
	inst := w.instance
	if !w.instancePrompts {
		inst.FirstPrompt = ""
	}
	if !w.instanceBuild {
		inst.BuildID = ""
	}
	return w.link.Send(coord.Frame{Type: coord.TypeRegister, Instance: &inst})
}

func instancePromptPreview(prompt string) string {
	var preview strings.Builder
	var space bool
	for _, r := range prompt {
		if unicode.IsSpace(r) {
			space = preview.Len() > 0
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		n := utf8.RuneLen(r)
		if space {
			n++
		}
		if preview.Len()+n > coord.MaxInstancePromptBytes-len("…") {
			return preview.String() + "…"
		}
		if space {
			preview.WriteByte(' ')
			space = false
		}
		preview.WriteRune(r)
	}
	return preview.String()
}

func (w *workerWriter) loseLink(link *coord.Link) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.link == link {
		w.disconnect(link.Err())
	}
}

// Caller holds mu. Closing asynchronously never stalls the SDK event drain.
func (w *workerWriter) disconnect(err error) {
	workerLog("coordinator link lost for %s; worker is still running and retains session %s: %v", w.instance.ID, w.instance.SessionID, err)
	link := w.link
	w.link = nil
	w.closing.Go(func() { closeWorkerLink(link) })
}

func closeWorkerLink(link *coord.Link) {
	if err := link.Close(); err != nil {
		workerLog("close coordinator link: %v", err)
	}
}

func workerLog(format string, args ...any) {
	// Keep diagnostics off the coordinated event transport.
	if _, err := fmt.Fprintf(os.Stderr, "tau worker: "+format+"\n", args...); err != nil {
		return
	}
}
