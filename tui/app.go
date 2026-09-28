package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/backend"
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/ui"
)

// Opts configures an App. Zero values are sensible defaults.
type Opts struct {
	// Root is the top-level view. Required.
	Root View

	// Title optionally sets the terminal window title while the app is running.
	// When set, Start/Run emits an OSC title sequence before the first render and
	// Restore/Stop clears the title as part of terminal lifecycle cleanup.
	Title string

	// Theme sets the visual style. Zero value uses DefaultTheme.
	Theme Theme

	// Keymap maps keystrokes to actions. Zero value uses DefaultKeymap,
	// which resolves only Esc and Ctrl+C to ActionQuit.
	Keymap Keymap

	// OnQuit is called when the user quits. The app stops after OnQuit
	// returns. If nil, the app simply stops.
	OnQuit func()

	// ClearOnExit controls what happens to the rendered content on
	// Restore(). When false (the default), the region is left intact and
	// the cursor parks just below it so subsequent output appears on a
	// new line. When true, the region is erased and the cursor returns to
	// where rendering began. Only meaningful in cbreak mode.
	ClearOnExit bool

	// Mouse enables mouse tracking. Default: false.
	Mouse bool

	// BracketedPaste enables bracketed paste. Default: true.
	BracketedPaste *bool

	// Stdin / Stdout override the input/output streams. Nil uses os.Stdin
	// and os.Stdout. Used by tests and embedders.
	Stdin  io.Reader
	Stdout io.Writer

	// Backend is an optional custom terminal backend. When non-nil, the
	// app uses it instead of the default process terminal. Used by tests
	// to inject a headless backend.
	Backend backend.Backend

	// TerminalMode controls how the process terminal configures input.
	// The zero value (ModeRaw) enables full raw mode. Use ModeCBreak for
	// inline components that should leave signal keys and output processing
	// enabled.
	TerminalMode TerminalMode
}

// App is a running TUI application. Create one with New and call Run to
// start the event loop. Call Quit from any goroutine to stop the loop.
type App struct {
	inner  *rovel.App
	opts   Opts
	action int // ActionQuit

	backend backend.Backend

	mu     sync.Mutex
	closed bool

	inputListeners      []inputListenerEntry
	nextInputListenerID int
	focusedID           ID

	overlayMu sync.Mutex
	overlays  map[ID]*Overlay
}

// OnQuit registers a callback invoked when the user quits. The app stops
// after the callback returns. Must be called before Run.
func (a *App) OnQuit(fn func()) {
	a.opts.OnQuit = fn
}

// New creates a new App. The Root view must be set in opts before Run is
// called. The returned App is not running until Run is invoked.
func New(opts Opts) (*App, error) {
	return NewWithBackend(opts, nil, nil)
}

// NewWithBackend is like New but allows injecting a custom backend (for tests)
// and an output writer. When backend is non-nil, it is used as the terminal
// backend; when stdout is non-nil, all output is written there.
//
// The backend must implement the underlying library's backend.Backend
// interface. The testbackend subpackage provides a headless implementation
// suitable for tests.
func NewWithBackend(opts Opts, customBackend backend.Backend, stdout io.Writer) (*App, error) {
	if opts.Root == nil {
		return nil, fmt.Errorf("tui: Opts.Root is required")
	}

	theme := opts.Theme
	if isZeroTheme(theme) {
		theme = rovel.DefaultTheme()
	}

	km := opts.Keymap
	if km == nil {
		km = DefaultKeymap{}
	}

	bracketed := true
	if opts.BracketedPaste != nil {
		bracketed = *opts.BracketedPaste
	}

	backendForApp := customBackend
	if backendForApp == nil {
		backendForApp = opts.Backend
	}

	stdoutForApp := stdout
	if stdoutForApp == nil {
		stdoutForApp = opts.Stdout
	}

	if backendForApp == nil && (stdoutForApp != nil || opts.Stdin != nil) {
		backendForApp = newStreamBackend(opts.Stdin, geom.Size{W: 80, H: 24})
	}
	if backendForApp != nil && stdoutForApp != nil {
		backendForApp = &stdoutBackend{Backend: backendForApp, stdout: stdoutForApp}
	}

	a := &App{
		opts:     opts,
		action:   int(ActionQuit),
		backend:  backendForApp,
		overlays: make(map[ID]*Overlay),
	}

	if backendForApp != nil {
		backendForApp = &listenerBackend{Backend: backendForApp, app: a}
		a.backend = backendForApp
	}

	libraryOpts := rovel.AppOpts{
		Theme:        theme,
		Backend:      backendForApp,
		TerminalMode: opts.TerminalMode,
		Input: rovel.InputOpts{
			Mouse:          opts.Mouse,
			BracketedPaste: bracketed,
			ModifiedKeys:   opts.TerminalMode == ModeRaw,
		},
		ResolveAction: ui.NewResolver(keymapAdapter{km}),
	}

	inner, err := rovel.New(libraryOpts)
	if err != nil {
		return nil, fmt.Errorf("tui: new app: %w", err)
	}

	a.inner = inner
	inner.SetRoot(opts.Root)

	return a, nil
}

// Backend is the interface for terminal backends. It mirrors the underlying
// library's backend.Backend without forcing callers to import it.
//
// Implementations include the default process terminal (used when no custom
// backend is provided) and the headless test backend.
type Backend interface {
	Enable() (Size, error)
	Restore() error
	ReadEvent() Event
	Size() Size
}

// Start claims the terminal: it installs the quit handler, enables raw mode and
// the configured input protocols, and sets the terminal title. It does not
// start the event loop — call Run for that, or drive the loop yourself.
//
// Every Start must be paired with a Stop, including when a later step fails;
// Run does this for you.
//
// Deviation from the compliance checklist, deliberate: the checklist names
// these `Start(ctx)` / `Stop(ctx)`. Both are synchronous terminal operations
// with nothing to cancel partway through, and Stop must run to completion even
// under a cancelled context or the user is left in raw mode. A context
// parameter here would be ignored, so it is omitted rather than implied.
func (a *App) Start() error {
	if a.inner == nil {
		return fmt.Errorf("tui: app not initialized")
	}

	// Wire up the quit callback by intercepting the root view. The App's
	// resolver returns ActionQuit for the configured keys; we install a
	// top-level action handler that calls Quit when that action fires.
	a.installQuitHandler()

	if err := a.inner.Enable(); err != nil {
		return fmt.Errorf("tui: enable: %w", err)
	}

	if a.opts.Title != "" {
		if err := writeTerminalTitle(a.backend, a.opts.Title); err != nil && !errors.Is(err, ErrUnsupported) {
			return fmt.Errorf("tui: set terminal title: %w", err)
		}
	}

	return nil
}

// Stop releases the terminal: it clears the title and restores the previous
// terminal mode. It is safe to call after a failed Start, and reports the
// restore error rather than only logging it. Errors clearing the title are
// logged and do not mask a restore failure — leaving the terminal in raw mode
// is the failure that matters.
func (a *App) Stop() error {
	if a.inner == nil {
		return nil
	}

	if a.opts.Title != "" {
		if err := writeTerminalTitle(a.backend, ""); err != nil && !errors.Is(err, ErrUnsupported) {
			fmt.Fprintf(os.Stderr, "tui: clear terminal title failed: %v\n", err)
		}
	}

	if err := a.inner.Restore(); err != nil {
		return fmt.Errorf("tui: restore: %w", err)
	}

	return nil
}

// Run starts the event loop and blocks until the app quits. The terminal is
// placed in raw mode during Run and restored when Run returns, including on
// panic. It is the convenience wrapper around Start, the loop, and Stop.
func (a *App) Run() (err error) {
	if err := a.Start(); err != nil {
		// Start may have enabled the terminal before failing on the title.
		if serr := a.Stop(); serr != nil {
			fmt.Fprintf(os.Stderr, "tui: %v\n", serr)
		}

		return err
	}

	// Always restore the terminal before Run returns, even on panic. This keeps
	// callers that print immediately after Run from racing terminal teardown.
	defer func() {
		if stopErr := a.Stop(); stopErr != nil {
			fmt.Fprintf(os.Stderr, "tui: %v\n", stopErr)
			if err == nil {
				err = stopErr
			}
		}
	}()

	return a.inner.Run()
}

// Quit requests the app to stop. Safe to call from any goroutine.
func (a *App) Quit() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()

		return
	}

	a.closed = true
	a.mu.Unlock()

	if a.inner != nil {
		if err := a.inner.Post(func(_ *rovel.UpdateCtx) {
			a.inner.Quit()
		}); err != nil {
			a.inner.Quit()
		}
	}
}

// Post schedules fn to run on the app loop. Safe to call from any goroutine
// before or during Run. Returns ErrClosed if the app is no longer running.
func (a *App) Post(fn func(ctx *UpdateCtx)) error {
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()

	if closed {
		return ErrClosed
	}

	if a.inner == nil {
		return ErrClosed
	}

	return a.inner.Post(fn)
}

// PostInvalidate schedules a repaint of the given rect on the app loop.
func (a *App) PostInvalidate(r Rect) error {
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()

	if closed || a.inner == nil {
		return ErrClosed
	}

	return a.inner.PostInvalidate(r)
}

// PostInvalidateAll schedules a full repaint of the screen on the app loop.
func (a *App) PostInvalidateAll() error {
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()

	if closed || a.inner == nil {
		return ErrClosed
	}

	return a.inner.PostInvalidateAll()
}

// Errors returns accumulated errors from terminal operations.
func (a *App) Errors() []error {
	if a.inner == nil {
		return nil
	}

	return a.inner.Errors()
}

// UpdateCtx is passed to posted callbacks, providing app-loop-safe mutations.
type UpdateCtx = rovel.UpdateCtx

// installQuitHandler walks the root view and installs a HandleAction that
// calls Quit when ActionQuit is dispatched. We use a synthetic root wrapper
// so the quit handler is always at the top of the action chain.
func (a *App) installQuitHandler() {
	root := a.opts.Root
	a.inner.SetRoot(&quitWrapper{root: root, app: a})
}

// quitWrapper wraps a root view and intercepts the configured quit action
// before the wrapped view sees it.
type quitWrapper struct {
	root rovel.View
	app  *App
}

func (q *quitWrapper) ID() rovel.ID                         { return q.root.ID() }
func (q *quitWrapper) Rect() rovel.Rect                     { return q.root.Rect() }
func (q *quitWrapper) MinSize() rovel.Size                  { return q.root.MinSize() }
func (q *quitWrapper) Layout(r rovel.Rect)                  { q.root.Layout(r) }
func (q *quitWrapper) Paint(d rovel.Drawer, ctx *rovel.Ctx) { q.root.Paint(d, ctx) }
func (q *quitWrapper) Handle(e rovel.Event, ctx *rovel.Ctx) bool {
	return q.root.Handle(e, ctx)
}

// Children exposes the wrapped root's children so the underlying toolkit can
// still build the focus/action tree through this synthetic root wrapper.
func (q *quitWrapper) Children() []rovel.View {
	if c, ok := q.root.(interface{ Children() []rovel.View }); ok {
		return c.Children()
	}

	return nil
}

// MouseOpaque stops the toolkit's hit test at the wrapper. Without it the
// hit test descends through Children straight into the wrapped root's
// regions and every mouse event is delivered to the deepest region view —
// the root's Handle never runs, so its mouse routing (document wheel,
// status-line clicks, deepest-view dispatch) is dead code. With it, mouse
// events arrive at the wrapper, which forwards them to the wrapped root's
// Handle for redistribution.
func (q *quitWrapper) MouseOpaque() {}

// HandleAction implements the action-handler contract.
func (q *quitWrapper) HandleAction(act ui.Action, ctx *rovel.Ctx) bool {
	if act == ui.Action(q.app.action) {
		if q.app.opts.OnQuit != nil {
			q.app.opts.OnQuit()
		}

		q.app.Quit()

		return true
	}

	// Fall through to the wrapped view's handler if any.
	if ah, ok := q.root.(interface {
		HandleAction(ui.Action, *rovel.Ctx) bool
	}); ok {
		return ah.HandleAction(act, ctx)
	}

	return false
}

// --- Keymap adapter ---

// Action is a semantic key-binding action.
type Action = ui.Action

// Standard actions mirror ui.Action constants.
const (
	ActionNone       = ui.ActionNone
	ActionFocusNext  = ui.ActionFocusNext
	ActionFocusPrev  = ui.ActionFocusPrev
	ActionFocusFirst = ui.ActionFocusFirst
	ActionFocusLast  = ui.ActionFocusLast
	ActionActivate   = ui.ActionActivate
	ActionCancel     = ui.ActionCancel
	ActionSubmit     = ui.ActionSubmit
	ActionMoveLeft   = ui.ActionMoveLeft
	ActionMoveRight  = ui.ActionMoveRight
	ActionMoveUp     = ui.ActionMoveUp
	ActionMoveDown   = ui.ActionMoveDown
	ActionPageUp     = ui.ActionPageUp
	ActionPageDown   = ui.ActionPageDown
	ActionHome       = ui.ActionHome
	ActionEnd        = ui.ActionEnd
	ActionDeleteBack = ui.ActionDeleteBackward
	ActionDeleteFwd  = ui.ActionDeleteForward
	ActionPaste      = ui.ActionPaste
	ActionSelectAll  = ui.ActionSelectAll
	ActionCopy       = ui.ActionCopy
	ActionCut        = ui.ActionCut
	ActionQuit       = ui.Action(1000) // tau-reserved
)

// Keymap maps a keystroke to an action. Implementations are passed to Opts.Keymap.
type Keymap interface {
	Resolve(ctx ui.KeyContext, k ui.Keystroke) (Action, bool)
}

// Keystroke is a normalized key press for keymap lookup.
type Keystroke = ui.Keystroke

// KeyContext indicates the keymap context (global, text input, overlay).
type KeyContext = ui.KeyContext

const (
	KeyCtxGlobal    = ui.KeyCtxGlobal
	KeyCtxTextInput = ui.KeyCtxTextInput
	KeyCtxOverlay   = ui.KeyCtxOverlay
)

// DefaultKeymap resolves Esc and Ctrl+C to ActionQuit. It is the zero-value
// sensible default.
type DefaultKeymap struct{}

// Resolve implements Keymap.
func (DefaultKeymap) Resolve(_ ui.KeyContext, k ui.Keystroke) (Action, bool) {
	switch k.Key {
	case event.KeyEsc, event.KeyCtrlC:
		return ActionQuit, true
	default:
		return ActionNone, false
	}
}

// StandardKeymap resolves the underlying toolkit's standard navigation and
// editing bindings. Apps with their own semantic actions can call this after
// app-specific bindings decline a key.
type StandardKeymap struct{}

// Resolve implements Keymap.
func (StandardKeymap) Resolve(ctx ui.KeyContext, k ui.Keystroke) (Action, bool) {
	return ui.DefaultKeymap{}.Resolve(ctx, k)
}

// keymapAdapter bridges a Keymap into the tui library's resolver signature.
type keymapAdapter struct {
	km Keymap
}

func (a keymapAdapter) Resolve(c ui.KeyContext, k ui.Keystroke) (Action, bool) {
	return a.km.Resolve(KeyContextFromUI(c), k)
}

// KeyContextFromUI converts the library's KeyContext to the tau/tui alias.
// This is a no-op cast for now; it exists so callers don't have to import the
// library package.
func KeyContextFromUI(c ui.KeyContext) KeyContext {
	return KeyContext(c)
}

// DefaultTheme returns the toolkit's default theme.
func DefaultTheme() Theme {
	return rovel.DefaultTheme()
}

// DefaultThemeModern returns the colored Unicode theme used by full Tau apps.
func DefaultThemeModern() Theme {
	return rovel.DefaultThemeModern()
}

// isZeroTheme reports whether t is the zero value. Theme contains a Chrome
// field with non-comparable values, so we can't use == directly.
func isZeroTheme(t Theme) bool {
	return t.Base.FG == 0 && t.Base.BG == 0 && t.Base.Attr == 0
}
