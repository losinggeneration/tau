package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
)

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
// or process cancellation. Reconnection and replay are deliberately not attempted.
func (w *Worker) Run(ctx context.Context, path string, instance coord.Instance, token string) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := w.server
	output := &workerWriter{instance: instance, server: s}
	s.writer = output
	s.ui = ui.NewRPCUI()
	s.ui.Writer = rpcUIWriter{server: s}
	if s.pluginManager != nil {
		s.pluginManager.SetUI(s.ui)
	}
	var admission sync.WaitGroup
	cleaned := false
	cleanup := func() error {
		cancel()
		admission.Wait()
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
	output.connect(link, instance)
	var reader sync.WaitGroup
	readerStop := make(chan struct{})
	defer func() {
		if !cleaned {
			err = errors.Join(err, cleanup())
		}
		close(readerStop)
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
	// Only one ordinary handler runs at a time, with one bounded waiting slot.
	// The reader never waits for that handler, so UI replies and cancellation
	// remain serviceable even while slash commands or compaction block.
	pending := make(chan *Command, 1)
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
	for {
		select {
		case <-ctx.Done():
			return nil
		case frame, ok := <-frames:
			if !ok {
				// Do not derive workload cancellation from EOF or link failure.
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
				handlerMu.Lock()
				if cancelHandler != nil {
					cancelHandler()
				}
				handlerMu.Unlock()
				// SDK prompt admission can also run a slash command inline.
				s.ui.CancelPending()
				w.dispatch(ctx, &cmd)
			case "ui_response", "abort_bash":
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

// workerWriter adapts complete serialized RPC records without retaining events
// while disconnected. Link.Send itself is nonblocking and bounded.
type workerWriter struct {
	server   *Server
	mu       sync.Mutex
	link     *coord.Link
	instance coord.Instance
	closing  sync.WaitGroup
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
	if w.link == nil {
		return len(data), nil
	}
	if w.server != nil {
		id, _ := w.server.eventSessionID.Load().(string)
		if id != "" && id != w.instance.SessionID {
			w.instance.SessionID = id
			if err := w.link.Send(coord.Frame{Type: coord.TypeRegister, Instance: &w.instance}); err != nil {
				w.disconnect(err)
				return len(data), nil
			}
		}
	}
	frame := coord.Frame{Type: coord.TypeEvent, InstanceID: w.instance.ID, SessionID: record.SessionID, Event: json.RawMessage(data)}
	if record.Type == "response" {
		frame.Type, frame.ID, frame.Event, frame.Result = coord.TypeResponse, record.ID, nil, json.RawMessage(data)
	} else {
		if frame.SessionID == "" {
			frame.SessionID = w.instance.SessionID
		}
	}
	err := w.link.Send(frame)
	if errors.Is(err, coord.ErrFrameTooLarge) {
		if frame.Type == coord.TypeResponse {
			frame.Result = nil
			frame.Error = &coord.Error{Code: coord.CodeLimitExceeded, Message: "worker reply exceeds frame size limit"}
		} else {
			notice, encodeErr := json.Marshal(map[string]any{
				"type": "event_omitted", "code": coord.CodeLimitExceeded,
				"session_id": frame.SessionID, "seq": record.Seq, "ts": record.Timestamp,
			})
			if encodeErr != nil {
				return 0, fmt.Errorf("encode omitted event: %w", encodeErr)
			}
			frame.Event = notice
		}
		err = w.link.Send(frame)
		// Fail an interactive dialog whose request cannot reach a client.
		switch record.Type {
		case "ui_confirm", "ui_select", "ui_input":
			if err == nil {
				return 0, coord.ErrFrameTooLarge
			}
		}
	}
	if err != nil {
		w.disconnect(err)
	}
	return len(data), nil
}

// Caller holds mu. Closing asynchronously never stalls the SDK event drain.
func (w *workerWriter) disconnect(err error) {
	workerLog("coordinator link lost: %v", err)
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
