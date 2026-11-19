package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

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
	s := NewServer(rt, reg, mgr)
	s.coordinated = true
	return &Worker{server: s}
}

// Run initializes a session, registers it, and serves until explicit shutdown
// or process cancellation. The worker does not redial after coordinator loss.
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
	instance.SessionID = s.session.ID()
	link, _, err := coord.Dial(ctx, path, coord.Frame{
		Role: coord.RoleWorker, Version: coord.Version, Instance: &instance, Token: token,
	})
	if err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	instance.State = coord.StateReady
	output.connect(link, instance)
	var reader sync.WaitGroup
	readerStop := make(chan struct{})
	defer func() {
		if !cleaned {
			err = errors.Join(err, cleanup())
		}
		close(readerStop)
		// Stop late event writers from scheduling another asynchronous close.
		output.mu.Lock()
		output.link = nil
		output.mu.Unlock()
		if closeErr := link.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		reader.Wait()
		output.closing.Wait()
	}()
	frames := make(chan coord.Frame)
	reader.Go(func() {
		defer close(frames)
		for {
			frame, readErr := link.Read()
			if readErr != nil {
				return
			}
			select {
			case frames <- frame:
			case <-readerStop:
				return
			}
		}
	})
	// Only one ordinary handler runs at a time. Frame size bounds the total
	// payload in the waiting slots to 4 MiB.
	// The reader never waits for that handler, so UI replies and cancellation
	// remain serviceable even while slash commands or compaction block.
	pending := make(chan *Command, workerAdmissionSlots)
	reconstruction := make(chan *Command, workerAdmissionSlots)
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
				// outlives dispatch. Slash commands and compaction run inline.
				if cmd.Type != "slash_command" && cmd.Type != "compact" {
					w.dispatch(ctx, cmd)
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
				// Do not derive workload cancellation from EOF or link failure.
				output.loseLink(link.Err())
				frames = nil
				continue
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
			case "replay_events", "get_snapshot", "get_history":
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
	server   *Server
	mu       sync.Mutex
	link     *coord.Link
	instance coord.Instance
	closing  sync.WaitGroup
	journal  eventJournal
	live     liveSnapshot
}

func (w *workerWriter) connect(link *coord.Link, instance coord.Instance) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.link, w.instance = link, instance
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
	var changedSession bool
	if w.server != nil {
		id, _ := w.server.eventSessionID.Load().(string)
		if id != "" && id != w.instance.SessionID {
			w.instance.SessionID = id
			w.instance.State = coord.StateReady
			w.live = liveSnapshot{}
			changedSession = true
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
			if err := w.live.update(record.Type, json.RawMessage(data)); err != nil {
				w.live.Truncated = true
				workerLog("%v", err)
			}
		}
		w.journal.position = position
		w.journal.publish(frame, size)
	}
	if w.link != nil && changedSession {
		if err := w.link.Send(coord.Frame{Type: coord.TypeRegister, Instance: &w.instance}); err != nil {
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
		if err := w.publishSummaryLocked(record.Type); err != nil {
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

// publishSummaryLocked reports lifecycle and attention, without copying chat
// payloads to clients watching only summaries. Caller holds mu.
func (w *workerWriter) publishSummaryLocked(kind string) error {
	previous := w.instance
	switch kind {
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
	return w.link.Send(coord.Frame{Type: coord.TypeRegister, Instance: &w.instance})
}

func (w *workerWriter) loseLink(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.link != nil {
		w.disconnect(err)
	}
}

// Caller holds mu. Closing asynchronously never stalls the SDK event drain.
func (w *workerWriter) disconnect(err error) {
	workerLog("coordinator link lost for %s; worker is still running and retains session %s: %v", w.instance.ID, w.instance.SessionID, err)
	link := w.link
	w.link = nil
	w.closing.Go(func() {
		if closeErr := link.Close(); closeErr != nil {
			workerLog("close coordinator link: %v", closeErr)
		}
	})
}

func workerLog(format string, args ...any) {
	// Keep diagnostics off the coordinated event transport.
	if _, err := fmt.Fprintf(os.Stderr, "tau worker: "+format+"\n", args...); err != nil {
		return
	}
}
