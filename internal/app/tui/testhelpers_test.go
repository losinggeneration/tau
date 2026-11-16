package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/store"
	sdkpkg "github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

// fakeSession is a minimal sdk.Session stub for unit tests. The interactive
// TUI only relies on Send/Interrupt/Close/Compact during normal operation;
// tests that don't drive a real session leave it idle. Methods not called
// by the code under test return zero values. Send and SendText record
// their arguments on the instance so tests can assert on forwarded
// commands. SetModel records the requested model id.
//
// fakeSession also implements FollowUpper, Steerer, Queuer, and
// Statser so tests can exercise the follow-up queue, steering queue,
// and footer-stats code paths without standing up a real session.
// The recording slices on the struct let tests assert on the
// dispatch order.
type fakeSession struct {
	mu               sync.Mutex
	sentTexts        []string
	sentImages       [][]ai.ContentBlock
	sentAttachments  [][]string
	attachments      map[string]sdkpkg.Attachment
	slashInfos       []sdkpkg.SlashCommandInfo
	slashCalls       int
	setModels        []string
	stateID          string
	stateModel       string
	stateCWD         string
	stateDataDir     string
	followUps        []string
	steers           []string
	interrupts       int
	steeringOverride int // when non-zero, QueueSizes reports this steering count
	queueOverride    int // when non-zero, QueueSizes reports this followUp count
	totalTokens      int
	contextWindow    int
	usedPct          float64
	autoCompact      bool
	usage            ai.Usage
	setModelErr      error
	setThinkingErr   error
	thinkingLevels   []string
	messages         []sdkpkg.Message
	allMessages      []sdkpkg.Message
	compactCalls     int
	compactArgs      []string
	compactErr       error
}

func (f *fakeSession) Send(ctx context.Context, msg sdkpkg.Message) (<-chan sdkpkg.Event, error) {
	f.mu.Lock()
	f.sentTexts = append(f.sentTexts, msg.Text)
	f.sentImages = append(f.sentImages, append([]ai.ContentBlock(nil), msg.Images...))
	f.sentAttachments = append(f.sentAttachments, append([]string(nil), msg.AttachmentIDs...))
	f.mu.Unlock()
	return nil, nil
}

func (f *fakeSession) SendText(ctx context.Context, text string) (<-chan sdkpkg.Event, error) {
	return nil, nil
}

func (f *fakeSession) Interrupt() error {
	f.mu.Lock()
	f.interrupts++
	f.mu.Unlock()
	return nil
}

func (f *fakeSession) Compact(ctx context.Context, customInstructions ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compactCalls++
	if len(customInstructions) > 0 {
		f.compactArgs = append(f.compactArgs, customInstructions[0])
	} else {
		f.compactArgs = append(f.compactArgs, "")
	}
	return f.compactErr
}

func (f *fakeSession) State() sdkpkg.SessionState {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.stateID
	if id == "" {
		id = "test"
	}
	cwd := f.stateCWD
	return sdkpkg.NewSessionStateWithDataDir(id, f.stateModel, cwd, f.stateDataDir)
}

func (f *fakeSession) ID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stateID != "" {
		return f.stateID
	}
	return "test"
}
func (f *fakeSession) IsPersisted() bool { return false }
func (f *fakeSession) Close(ctx context.Context) error {
	if f.stateDataDir != "" && len(f.sentTexts) == 0 {
		return os.RemoveAll(f.stateDataDir)
	}
	return nil
}

func (f *fakeSession) SetThinkingLevel(level string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setThinkingErr != nil {
		return f.setThinkingErr
	}
	f.thinkingLevels = append(f.thinkingLevels, level)
	return nil
}

// thinkingLevelsSeen returns the levels the app pushed into the session, so a
// test can assert the keybinding actually reached the session rather than only
// repainting the footer.
func (f *fakeSession) thinkingLevelsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.thinkingLevels...)
}

func (f *fakeSession) SetModel(model string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setModelErr != nil {
		return f.setModelErr
	}
	f.setModels = append(f.setModels, model)
	f.stateModel = model
	return nil
}

func (f *fakeSession) SlashCommands() []sdkpkg.SlashCommandInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slashCalls++
	return f.slashInfos
}

func (f *fakeSession) GetMessages() []sdkpkg.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sdkpkg.Message, len(f.messages))
	copy(out, f.messages)
	return out
}

// GetAllMessages reports allMessages when set, mimicking the full persisted
// transcript; otherwise it mirrors GetMessages like an ephemeral session does.
func (f *fakeSession) GetAllMessages() []sdkpkg.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.allMessages == nil {
		out := make([]sdkpkg.Message, len(f.messages))
		copy(out, f.messages)
		return out
	}
	out := make([]sdkpkg.Message, len(f.allMessages))
	copy(out, f.allMessages)
	return out
}

// FollowUp records the queued message in followUps. Tests use the
// recorded slice to assert on the dispatch.
func (f *fakeSession) FollowUp(msg sdkpkg.Message) error {
	f.mu.Lock()
	f.followUps = append(f.followUps, msg.Text)
	f.mu.Unlock()
	return nil
}

// Steer records the steered message. Not exercised by current tests
// but included so a future test can verify the steer-vs-follow-up
// routing.
func (f *fakeSession) Steer(msg sdkpkg.Message) error {
	f.mu.Lock()
	f.steers = append(f.steers, msg.Text)
	f.mu.Unlock()
	return nil
}

// testDataRoot holds attachments for fakes without a data directory, so tests
// never write into the package source tree.
var testDataRoot string

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "tau-tui-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testDataRoot = root
	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func (f *fakeSession) attachmentDataDir() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stateDataDir == "" {
		f.stateDataDir = filepath.Join(testDataRoot, fmt.Sprintf("session-%p", f))
	}
	return f.stateDataDir
}

func (f *fakeSession) ImportAttachmentFile(path string) (sdkpkg.Attachment, error) {
	store := sdkpkg.NewAttachmentStore(filepath.Join(f.attachmentDataDir(), "attachments"))
	record, err := store.ImportFile(path)
	if err != nil {
		return sdkpkg.Attachment{}, err
	}
	f.mu.Lock()
	if f.attachments == nil {
		f.attachments = map[string]sdkpkg.Attachment{}
	}
	f.attachments[record.ID] = record
	f.mu.Unlock()
	return record, nil
}

func (f *fakeSession) ImportAttachmentData(filename, mimeType, data string) (sdkpkg.Attachment, error) {
	store := sdkpkg.NewAttachmentStore(filepath.Join(f.attachmentDataDir(), "attachments"))
	record, err := store.ImportData(filename, mimeType, data)
	if err != nil {
		return sdkpkg.Attachment{}, err
	}
	f.mu.Lock()
	if f.attachments == nil {
		f.attachments = map[string]sdkpkg.Attachment{}
	}
	f.attachments[record.ID] = record
	f.mu.Unlock()
	return record, nil
}

func (f *fakeSession) ListAttachments() []sdkpkg.Attachment {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sdkpkg.Attachment, 0, len(f.attachments))
	for _, record := range f.attachments {
		out = append(out, record)
	}
	return out
}

func (f *fakeSession) DeleteAttachment(id string) error {
	f.mu.Lock()
	delete(f.attachments, id)
	f.mu.Unlock()
	return nil
}

// ClearQueue hands back and forgets every recorded steer and follow-up.
func (f *fakeSession) ClearQueue() (steering, followUp []sdkpkg.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, text := range f.steers {
		steering = append(steering, sdkpkg.Message{Role: "user", Text: text})
	}
	for _, text := range f.followUps {
		followUp = append(followUp, sdkpkg.Message{Role: "user", Text: text})
	}
	f.steers, f.followUps = nil, nil
	return steering, followUp
}

// QueueSizes returns the current follow-up count. When queueOverride
// is set it shadows followUps so tests can simulate the agent
// draining the queue without rewriting the followUps slice.
func (f *fakeSession) QueueSizes() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.steeringOverride > 0 || f.queueOverride > 0 {
		return f.steeringOverride, f.queueOverride
	}
	return len(f.steers), len(f.followUps)
}

// Stats returns the configured session statistics so tests can
// assert that the footer pulls the right fields.
func (f *fakeSession) Stats() sdkpkg.SessionStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.stateID
	if id == "" {
		id = "test"
	}
	return sdkpkg.SessionStats{
		SessionID:      id,
		Model:          f.stateModel,
		TotalTokens:    f.totalTokens,
		ContextWindow:  f.contextWindow,
		ContextUsedPct: f.usedPct,
		AutoCompaction: f.autoCompact,
		Usage:          f.usage,
	}
}

// newTestApp builds an App with a populated conversation model and command
// registry suitable for unit tests of command dispatch and conversation
// helpers. It does NOT start the toolkit loop or create a real tauui.App —
// tests that exercise overlay/keymap paths use the package-level test
// fixtures or the existing tuiapp_test.go helpers.
func newTestApp(t *testing.T) *App {
	t.Helper()
	conv := NewConversationModel()
	cmds := NewCommands()
	a := &App{
		conv: conv,
		cmds: cmds,
		sess: &fakeSession{},
		chat: NewChatLog(conv),
	}
	t.Cleanup(func() { _ = a.chat.Close() })
	a.cancelRun = func() {}
	a.openURL = func(string) error { return nil }
	return a
}

func appCredentialStore(t *testing.T) *store.FileBackend {
	t.Helper()
	fb := store.NewFileBackend(filepath.Join(t.TempDir(), "auth.json"), store.Options{})
	if err := fb.EnsureDir(); err != nil {
		t.Fatalf("ensure auth dir: %v", err)
	}
	return fb
}

// newPromptEditorForTest creates a real prompt editor without the toolkit loop.
// The component is fully usable for unit tests that exercise its text and
// OnChange APIs; methods that need a *tauui.Ctx pass testCtx().
func newPromptEditorForTest() *tauui.PromptEditor { //nolint:unused // shared prompt-editor fixture for package tests
	return tauui.NewPromptEditor()
}

// newFooterForTest returns a Footer with a minimal data struct so the
// TUI's setStreaming call (used by submit) has a non-nil footer to
// touch. Tests that drive the full submit path need this; tests that
// only exercise command dispatch do not.
func newFooterForTest() *Footer { //nolint:unused // exposed for command tests
	return NewFooter(&footerData{})
}

// testCtx returns a *tauui.Ctx with safe no-op callbacks. It's enough to
// satisfy the focused-id check used by trySubmit and to satisfy the
// prompt editor's Invalidate calls in tests that don't need real rendering.
func testCtx() *tauui.Ctx { //nolint:unused // exposed for command tests
	return &tauui.Ctx{
		Invalidate:       func(tauui.Rect) {},
		InvalidateAll:    func() {},
		InvalidateLayout: func() {},
		RequestFocus:     func(tauui.ID) {},
		Quit:             func() {},
	}
}

// stubCtx returns a *tauui.Ctx with all invalidation/focus callbacks
// stubbed as no-ops. Tests that drive the selector's OnChange pipeline
// use this instead of &tauui.Ctx{Invalidate: ...} so all
// callbacks the App's refreshSelector calls (InvalidateAll,
// InvalidateLayout, Invalidate) are covered.
func stubCtx() *tauui.Ctx {
	return &tauui.Ctx{
		Invalidate:       func(tauui.Rect) {},
		InvalidateAll:    func() {},
		InvalidateLayout: func() {},
		RequestFocus:     func(tauui.ID) {},
		Quit:             func() {},
	}
}
