package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test harness: feeds JSONL commands to the worker's shared handlers
// in-process. No subprocess, no binary build, no sleeps for synchronization.
// ─────────────────────────────────────────────────────────────────────────────

type testHarness struct {
	srv    *Server
	stdin  io.Writer      // writes to the server's reader
	stdout *bufio.Scanner // reads lines from the server's writer
	cancel context.CancelFunc
	done   chan struct{}
}

// timeout is the default test deadline for reading server responses.
// Tests should be deterministic; this is a safety net for hangs.
const timeout = 5 * time.Second

func waitOrFail(t *testing.T, done <-chan struct{}, dur time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(dur):
		t.Fatalf("%s did not complete within %v", what, dur)
	}
}

// sdkNewSessionState is a thin alias so test files don't need to import
// the full sdk namespace just for construction.
func sdkNewSessionState(id, model, cwd string) sdk.SessionState {
	return sdk.NewSessionState(id, model, cwd)
}

func newTestHarness(t *testing.T, rt sdk.Runtime, reg commands.Registry) *testHarness {
	t.Helper()
	return startTestHarness(t, newServer(rt, reg, nil))
}

func startTestHarness(t *testing.T, srv *Server) *testHarness {
	t.Helper()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	srv.writer = stdoutW
	srv.ui = ui.NewRPCUI()
	srv.ui.Writer = stdoutW
	if srv.pluginManager != nil {
		srv.pluginManager.SetUI(srv.ui)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &testHarness{
		srv:    srv,
		stdin:  stdinW,
		stdout: bufio.NewScanner(stdoutR),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	h.stdout.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	go func() {
		defer close(h.done)
		defer stdoutW.Close()
		serveTestCommands(ctx, srv, bufio.NewReader(stdinR))
	}()
	return h
}

// serveTestCommands dispatches each JSONL command the way a worker dispatches
// coordinator frames, including its shutdown handling, until EOF.
func serveTestCommands(ctx context.Context, s *Server, r *bufio.Reader) {
	w := &Worker{server: s}
	for ctx.Err() == nil {
		line, err := r.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			break
		}
		var cmd Command
		if decodeErr := json.Unmarshal([]byte(line), &cmd); decodeErr != nil {
			if sendErr := s.sendError("", "invalid_command", decodeErr.Error()); sendErr != nil {
				break
			}
			continue
		}
		if cmd.Type == "shutdown" {
			if cleanupErr := s.cleanup(ctx); cleanupErr != nil {
				_ = s.sendErrorResult(cmd.ID, cleanupErr)
				return
			}
			_ = s.sendResult(cmd.ID, map[string]any{"ok": true})
			return
		}
		w.dispatch(ctx, &cmd)
	}
	_ = s.cleanup(ctx)
}

func (h *testHarness) send(t *testing.T, cmd string) {
	t.Helper()
	if _, err := io.WriteString(h.stdin, cmd+"\n"); err != nil {
		t.Fatalf("write command %q: %v", cmd, err)
	}
}

func (h *testHarness) readLine(t *testing.T) string {
	t.Helper()
	if !h.stdout.Scan() {
		if err := h.stdout.Err(); err != nil {
			t.Fatalf("scanner error: %v", err)
		}
		t.Fatal("unexpected EOF on server stdout")
	}
	return h.stdout.Text()
}

// readUntil reads lines until predicate matches or timeout elapses.
func (h *testHarness) readUntil(t *testing.T, dur time.Duration, match func(string) bool) []string {
	t.Helper()
	deadline := time.NewTimer(dur)
	defer deadline.Stop()
	var lines []string
	for {
		select {
		case <-deadline.C:
			return lines
		default:
		}
		if !h.stdout.Scan() {
			return lines
		}
		line := h.stdout.Text()
		lines = append(lines, line)
		if match(line) {
			return lines
		}
	}
}

func (h *testHarness) close() {
	// Close stdin so the server's blocking ReadString returns EOF,
	// then drain any remaining output (session_shutdown etc.) so the
	// server doesn't block writing to a full pipe.
	if w, ok := h.stdin.(io.Closer); ok {
		_ = w.Close()
	}
	// Drain remaining lines with a short timeout. This lets the server
	// emit its final cleanup events without blocking.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for h.stdout.Scan() {
			// discard
		}
	}()
	select {
	case <-drainDone:
	case <-time.After(timeout):
		// Server didn't finish writing within timeout; force exit.
		h.cancel()
		<-drainDone
	}
	<-h.done
}

// ─────────────────────────────────────────────────────────────────────────────
// mockRuntime satisfies sdk.Runtime.
// ─────────────────────────────────────────────────────────────────────────────

type mockRuntime struct {
	session       *mockSession
	sessions      []sdk.SessionInfo
	resolvedModel *sdk.ResolvedModel
	storage       *session.Storage // optional, set by tests that exercise persistence
	newCount      int
	resumeCount   int
}

func (m *mockRuntime) NewSession(ctx context.Context, opts sdk.SessionOptions) (sdk.Session, error) {
	m.newCount++
	m.session = newMockSession("test-session", opts.CWD)
	if opts.Model != "" {
		m.session.state = sdk.NewSessionState("test-session", opts.Model, opts.CWD)
	}
	m.session.runtime = m
	return m.session, nil
}

func (m *mockRuntime) Close(ctx context.Context) error { return nil }

func (m *mockRuntime) ListSessions(ctx context.Context) ([]sdk.SessionInfo, error) {
	return m.sessions, nil
}

func (m *mockRuntime) PinSession(ctx context.Context, id string) error   { return nil }
func (m *mockRuntime) UnpinSession(ctx context.Context, id string) error { return nil }

func (m *mockRuntime) ResumeSession(ctx context.Context, id string, opts sdk.SessionOptions) (sdk.Session, error) {
	m.resumeCount++
	m.session = newMockSession(id, opts.CWD)
	m.session.runtime = m
	return m.session, nil
}

func (m *mockRuntime) SessionID() string {
	if m.session == nil {
		return ""
	}
	return m.session.id
}

func (m *mockRuntime) SetupDefaults() error { return nil }

func (m *mockRuntime) ActiveModel() *ai.Model {
	if m.resolvedModel != nil && m.resolvedModel.Model != nil {
		return m.resolvedModel.Model
	}
	return nil
}

func (m *mockRuntime) ResolveModel() (*sdk.ResolvedModel, error) {
	if m.resolvedModel != nil {
		return m.resolvedModel, nil
	}
	return &sdk.ResolvedModel{}, nil
}

func (m *mockRuntime) TelemetryEnabled() bool { return false }
func (m *mockRuntime) PluginManager() *plugins.Manager {
	return nil
}

func (m *mockRuntime) GetSessionStorage() *session.Storage {
	if m.storage != nil {
		return m.storage
	}
	// Lazily create storage. Honor TAU_SESSION_DIR for test isolation.
	dir := os.Getenv("TAU_SESSION_DIR")
	var (
		storage *session.Storage
		err     error
	)
	if dir != "" {
		storage, err = session.NewStorageAt(dir)
	} else {
		storage, err = session.NewStorage()
	}
	if err != nil {
		return nil
	}
	m.storage = storage
	return storage
}

// ─────────────────────────────────────────────────────────────────────────────
// mockSession satisfies sdk.Session.
// ─────────────────────────────────────────────────────────────────────────────

type mockSession struct {
	id            string
	runtime       *mockRuntime
	thinking      string
	setModels     []string
	state         sdk.SessionState
	events        []sdk.Event // canned events to emit from Send
	persist       bool
	closeErr      error
	steeringCount int
	followUpCount int
	followUps     []sdk.Message
	observer      func(sdk.Event)
	sent          []sdk.Message
	attachments   map[string]sdk.Attachment
	bash          func(context.Context, string, sdk.BashOptions) (sdk.BashExecution, error)
	send          func(context.Context, sdk.Message) (<-chan sdk.Event, error)
	interrupts    int
}

func (s *mockSession) ExecuteBash(ctx context.Context, command string, options sdk.BashOptions) (sdk.BashExecution, error) {
	if s.bash == nil {
		return sdk.BashExecution{}, errors.New("bash not configured")
	}
	return s.bash(ctx, command, options)
}

func newMockSession(id, cwd string) *mockSession {
	return &mockSession{
		id:      id,
		state:   sdk.NewSessionState(id, "anthropic/claude-sonnet-4-6", cwd),
		persist: true,
	}
}

func (s *mockSession) ID() string { return s.id }
func (s *mockSession) State() sdk.SessionState {
	state := s.state
	if s.steeringCount+s.followUpCount > 0 {
		state = sdk.NewSessionState(s.id, state.Model(), state.CWD())
	}
	return state
}

func (s *mockSession) IsPersisted() bool { return s.persist }

func (s *mockSession) Close(ctx context.Context) error { return s.closeErr }

func (s *mockSession) Interrupt() error { s.interrupts++; return nil }

func (s *mockSession) Compact(ctx context.Context, customInstructions ...string) error { return nil }

func (s *mockSession) SlashCommands() []sdk.SlashCommandInfo { return nil }

func (s *mockSession) SetModel(model string) error {
	s.setModels = append(s.setModels, model)
	s.state = sdk.NewSessionState(s.id, model, s.state.CWD())
	return nil
}

func (s *mockSession) SetThinkingLevel(level string) error {
	s.thinking = level
	return nil
}

func (s *mockSession) Send(ctx context.Context, msg sdk.Message) (<-chan sdk.Event, error) {
	s.sent = append(s.sent, msg)
	if s.send != nil {
		return s.send(ctx, msg)
	}
	ch := make(chan sdk.Event, len(s.events)+2)
	for _, e := range s.events {
		e.SessionID = s.id
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (s *mockSession) SendText(ctx context.Context, text string) (<-chan sdk.Event, error) {
	return s.Send(ctx, sdk.Message{Role: "user", Text: text})
}

func (s *mockSession) Steer(msg sdk.Message) error {
	s.steeringCount++
	return nil
}

func (s *mockSession) FollowUp(msg sdk.Message) error {
	s.followUpCount++
	s.followUps = append(s.followUps, msg)
	return nil
}

func (s *mockSession) QueueSizes() (steering, followUp int) {
	return s.steeringCount, s.followUpCount
}

func (s *mockSession) SetEventObserver(fn func(sdk.Event)) {
	s.observer = fn
}

func (s *mockSession) ImportAttachmentFile(path string) (sdk.Attachment, error) {
	store := sdk.NewAttachmentStore(filepath.Join(os.TempDir(), "tau-test-attachments", s.id))
	record, err := store.ImportFile(path)
	if err != nil {
		return sdk.Attachment{}, err
	}
	if s.attachments == nil {
		s.attachments = make(map[string]sdk.Attachment)
	}
	s.attachments[record.ID] = record
	return record, nil
}

func (s *mockSession) ImportAttachmentData(filename, mimeType, data string) (sdk.Attachment, error) {
	store := sdk.NewAttachmentStore(filepath.Join(os.TempDir(), "tau-test-attachments", s.id))
	record, err := store.ImportData(filename, mimeType, data)
	if err != nil {
		return sdk.Attachment{}, err
	}
	if s.attachments == nil {
		s.attachments = make(map[string]sdk.Attachment)
	}
	s.attachments[record.ID] = record
	return record, nil
}

func (s *mockSession) ListAttachments() []sdk.Attachment {
	out := make([]sdk.Attachment, 0, len(s.attachments))
	for _, record := range s.attachments {
		out = append(out, record)
	}
	return out
}

func (s *mockSession) DeleteAttachment(id string) error {
	delete(s.attachments, id)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// testRegistry creates an InMemoryRegistry with a set of built-in commands
// that mirror the production slash-command behavior (help, reload, etc.).
// ─────────────────────────────────────────────────────────────────────────────

func newTestRegistry() *commands.InMemoryRegistry {
	reg := commands.NewInMemoryRegistry()
	registerBuiltinCommands(reg)
	return reg
}

func registerBuiltinCommands(reg *commands.InMemoryRegistry) {
	type builtin struct {
		name    string
		message string
	}
	builtins := []builtin{
		{"help", "Available slash commands: /help, /reload, /sessions, /set-model, /show-model, /login"},
		{"reload", "Configuration reloaded."},
		{"sessions", "Use `tau sessions` CLI to list sessions."},
		{"set-model", "Use `tau set-model` CLI to change the active model."},
		{"show-model", "Use `tau show-model` CLI to show the active model."},
		{"login", "Use `tau login` CLI to authenticate with a provider."},
	}
	for _, b := range builtins {
		msg := b.message
		_ = reg.RegisterAt(commands.Info{Name: b.name}, func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
			return commands.Result{Output: msg}, nil
		}, "", commands.TierBuiltin)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Small helpers
// ─────────────────────────────────────────────────────────────────────────────

func readSessionFile(t *testing.T, dir string) string {
	t.Helper()
	files, err := os.ReadDir(dir)
	if err != nil || len(files) == 0 {
		t.Fatalf("no session files in %s: %v", dir, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	return string(data)
}

func parseResp(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("parse response %q: %v", line, err)
	}
	return m
}

func containsLine(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}
