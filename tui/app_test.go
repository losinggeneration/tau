package tui

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/style"
	"github.com/losinggeneration/tau/tui/internal/testbackend"
)

func TestNewWithBackend_UsesInjectedBackend(t *testing.T) {
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello")}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	if be.Enabled() {
		t.Fatal("backend enabled before Run")
	}

	be.Close()
	if err := app.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !be.Enabled() {
		t.Error("backend was not enabled")
	}
	if !be.Restored() {
		t.Error("backend was not restored")
	}
}

func TestNewWithBackend_MirrorsOutputToStdout(t *testing.T) {
	var out bytes.Buffer
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello")}, be, &out)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	be.Close()
	if err := app.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if out.Len() == 0 {
		t.Fatal("stdout captured no rendered output")
	}
}

func TestNew_UsesOptsBackendAndStdout(t *testing.T) {
	var out bytes.Buffer
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := New(Opts{Root: NewText("hello"), Backend: be, Stdout: &out})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	be.Close()
	if err := app.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !be.Enabled() {
		t.Error("opts backend was not enabled")
	}
	if out.Len() == 0 {
		t.Fatal("opts stdout captured no rendered output")
	}
}

func TestNew_UsesOptsStdin(t *testing.T) {
	state := &counterState{}
	app, err := New(Opts{Root: newCounterView(state), Stdin: bytes.NewBufferString("+")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := app.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if state.Value() != 1 {
		t.Fatalf("counter value = %d, want 1", state.Value())
	}
}

func TestNew_ConfiguresTerminalMode(t *testing.T) {
	app, err := New(Opts{Root: NewText("hello"), TerminalMode: ModeCBreak})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if app.opts.TerminalMode != ModeCBreak {
		t.Fatalf("TerminalMode = %v, want ModeCBreak", app.opts.TerminalMode)
	}
}

func TestNewWithBackend_PreservesTerminalMode(t *testing.T) {
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello"), TerminalMode: ModeCBreak}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	if app.opts.TerminalMode != ModeCBreak {
		t.Fatalf("TerminalMode = %v, want ModeCBreak", app.opts.TerminalMode)
	}
}

func TestApp_RawModeEnablesAndRestoresInputProtocols(t *testing.T) {
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello")}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()
	waitFor(t, func() bool {
		features := be.Features()

		return be.Enabled() && features.BracketedPaste && features.ModifiedKeys
	})

	app.Quit()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit")
	}

	if !be.Restored() {
		t.Fatal("backend was not restored")
	}
	if features := be.Features(); features.BracketedPaste || features.ModifiedKeys {
		t.Fatalf("input protocols were not disabled on restore: %+v", features)
	}
}

func TestApp_RunReturnsStopErrorAfterLoop(t *testing.T) {
	be := &restoreFailingBackend{Backend: testbackend.New(geom.Size{W: 20, H: 4}, nil)}
	app, err := NewWithBackend(Opts{Root: NewText("hello")}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	be.Close()
	err = app.Run()
	if !errors.Is(err, errForcedRestore) {
		t.Fatalf("Run error = %v, want %v", err, errForcedRestore)
	}
	if !be.restoreCalled.Load() {
		t.Fatal("backend restore was not called")
	}
}

func TestApp_BracketedPasteLifecycleDefaultEnableAndRestore(t *testing.T) {
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello")}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()
	waitFor(t, func() bool { return be.Enabled() && be.Features().BracketedPaste })

	app.Quit()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit")
	}

	if !be.Restored() {
		t.Fatal("backend was not restored")
	}
	if be.Features().BracketedPaste {
		t.Fatalf("bracketed paste was not disabled on restore: %+v", be.Features())
	}
}

func TestApp_BracketedPasteLifecycleRespectsDisabledOption(t *testing.T) {
	bracketed := false
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello"), BracketedPaste: &bracketed}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	be.Close()
	if err := app.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if be.Features().BracketedPaste {
		t.Fatalf("bracketed paste enabled despite disabled option: %+v", be.Features())
	}
	if !be.Restored() {
		t.Fatal("backend was not restored")
	}
}

func TestApp_RawModeRestoresAfterPostEnableStartupError(t *testing.T) {
	be := &flushFailingBackend{
		Backend: testbackend.New(geom.Size{W: 20, H: 4}, nil),
		failAt:  2,
	}
	app, err := NewWithBackend(Opts{Root: NewText("hello"), Title: "fail during startup"}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	err = app.Run()
	if !errors.Is(err, errForcedFlush) {
		t.Fatalf("Run error = %v, want %v", err, errForcedFlush)
	}
	if !be.Enabled() {
		t.Fatal("backend was not enabled before startup error")
	}
	if !be.Restored() {
		t.Fatal("backend was not restored after startup error")
	}
	if features := be.Features(); features.BracketedPaste || features.ModifiedKeys {
		t.Fatalf("input protocols were not disabled after startup error: %+v", features)
	}

	out := string(be.Output())
	hideAt := strings.Index(out, "\x1b[?25l")
	if hideAt == -1 {
		t.Fatalf("output %q does not hide cursor before startup error", out)
	}
	showAt := strings.LastIndex(out, "\x1b[?25h")
	if showAt == -1 {
		t.Fatalf("output %q does not show cursor after startup error", out)
	}
	if showAt < hideAt {
		t.Fatalf("show cursor emitted before hide cursor after startup error: output %q", out)
	}
}

func TestApp_CBreakModeDoesNotEnableModifiedKeyProtocol(t *testing.T) {
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello"), TerminalMode: ModeCBreak}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	be.Close()
	if err := app.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if be.Features().ModifiedKeys {
		t.Fatal("cbreak mode enabled modified key protocol")
	}
}

func TestApp_RawModeHidesCursorOnStartAndShowsCursorOnStop(t *testing.T) {
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello")}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	be.Close()
	if err := app.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := string(be.Output())
	hideAt := strings.Index(out, "\x1b[?25l")
	if hideAt == -1 {
		t.Fatalf("output %q does not hide cursor on start", out)
	}

	showAt := strings.LastIndex(out, "\x1b[?25h")
	if showAt == -1 {
		t.Fatalf("output %q does not show cursor on stop", out)
	}
	if showAt < hideAt {
		t.Fatalf("show cursor emitted before hide cursor: output %q", out)
	}
}

func TestApp_TerminalTitleLifecycle(t *testing.T) {
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello"), Title: "Tau Session"}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	be.Close()
	if err := app.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := string(be.Output())
	setAt := strings.Index(out, "\x1b]0;Tau Session\x07")
	if setAt == -1 {
		t.Fatalf("output %q does not set terminal title", out)
	}

	clearAt := strings.LastIndex(out, "\x1b]0;\x07")
	if clearAt == -1 {
		t.Fatalf("output %q does not clear terminal title", out)
	}
	if clearAt < setAt {
		t.Fatalf("terminal title cleared before it was set: output %q", out)
	}
}

func TestApp_TerminalTitleEscapesTerminators(t *testing.T) {
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello"), Title: "ta\x1b\x07u"}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	be.Close()
	if err := app.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out := string(be.Output()); !strings.Contains(out, "\x1b]0;tau\x07") {
		t.Fatalf("output %q does not contain escaped title sequence", out)
	}
}

func TestApp_OverlayFocusAndRenderAPIs(t *testing.T) {
	be := testbackend.New(geom.Size{W: 30, H: 6}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("root")}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- app.Run()
	}()

	waitFor(t, func() bool { return be.Enabled() })

	overlay := app.ShowOverlay(OverlayOpts{Root: NewText("overlay"), Modal: true})
	waitFor(t, func() bool { return overlay.ID() != 0 && app.HasOverlay(overlay.ID()) })

	app.SetFocus(overlay.Root().ID())
	waitFor(t, func() bool { return overlay.Focused() })
	if !app.IsFocused(overlay.Root().ID()) {
		t.Fatal("overlay root should be focused")
	}
	if got := app.FocusedID(); got != overlay.Root().ID() {
		t.Fatalf("FocusedID = %v, want %v", got, overlay.Root().ID())
	}

	if err := app.RequestRender(true); err != nil {
		t.Fatalf("RequestRender: %v", err)
	}

	overlay.SetHidden(true)
	waitFor(t, func() bool { return overlay.Hidden() })
	overlay.SetHidden(false)
	waitFor(t, func() bool { return !overlay.Hidden() && app.HasOverlay(overlay.ID()) })

	app.HideOverlay(overlay.ID())
	waitFor(t, func() bool { return !app.HasOverlay(overlay.ID()) })

	app.Quit()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit")
	}
}

func TestApp_NonCapturingOverlayDoesNotChangeWrapperFocus(t *testing.T) {
	be := testbackend.New(geom.Size{W: 30, H: 6}, nil)
	base := NewText("root")
	app, err := NewWithBackend(Opts{Root: base}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()
	waitFor(t, func() bool { return be.Enabled() })

	app.SetFocus(base.ID())
	waitFor(t, func() bool { return app.IsFocused(base.ID()) })

	overlay := app.ShowOverlay(OverlayOpts{Root: NewText("overlay"), Modal: true, NonCapturing: true})
	waitFor(t, func() bool { return overlay.ID() != 0 && app.HasOverlay(overlay.ID()) })

	if !app.IsFocused(base.ID()) {
		t.Fatal("non-capturing overlay changed wrapper focus")
	}
	if overlay.Modal() {
		t.Fatal("non-capturing overlay should not be modal")
	}

	app.Quit()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit")
	}
}

func TestApp_InputListeners(t *testing.T) {
	state := &counterState{}
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: newCounterView(state)}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	calls := 0
	unsubscribe := app.AddInputListener(func(e Event) (Event, bool) {
		calls++
		if ke, ok := e.(KeyEvent); ok && ke.Key == KeyRune && ke.Rune == '+' {
			return KeyEvent{Key: KeyRune, Rune: '-'}, false
		}

		return nil, false
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()
	waitFor(t, func() bool { return be.Enabled() })

	be.SendEvent(KeyEvent{Key: KeyRune, Rune: '+'})
	waitFor(t, func() bool { return state.Value() == -1 })
	if calls != 1 {
		t.Fatalf("listener calls = %d, want 1", calls)
	}

	unsubscribe()
	unsubscribe()
	be.SendEvent(KeyEvent{Key: KeyRune, Rune: '+'})
	waitFor(t, func() bool { return state.Value() == 0 })

	app.Quit()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit")
	}
}

func TestApp_InputListenerCanConsume(t *testing.T) {
	state := &counterState{}
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: newCounterView(state)}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	app.AddInputListener(func(e Event) (Event, bool) { return nil, true })

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()
	waitFor(t, func() bool { return be.Enabled() })

	be.SendEvent(KeyEvent{Key: KeyRune, Rune: '+'})
	time.Sleep(20 * time.Millisecond)
	if state.Value() != 0 {
		t.Fatalf("state value = %d, want consumed event to leave it at 0", state.Value())
	}

	app.Quit()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit")
	}
}

func TestApp_ResizeBypassesInputListenersAndRelayouts(t *testing.T) {
	var out syncBuffer
	root := newResizeRecordingView()
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: root}, be, &out)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	app.AddInputListener(func(Event) (Event, bool) { return nil, true })

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()
	waitFor(t, func() bool { return be.Enabled() && root.paints.Load() > 0 })

	paintsBefore := root.paints.Load()
	be.SendResize(30, 6)
	waitFor(t, func() bool {
		return root.paints.Load() > paintsBefore && root.width.Load() == 30 && root.height.Load() == 6
	})

	if !strings.Contains(out.String(), "resize 30x6") {
		t.Fatalf("resized full redraw did not render new size; output=%q", out.String())
	}

	app.Quit()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit")
	}
}

func TestApp_CoalescesPostedInvalidationsIntoSingleRender(t *testing.T) {
	root := newPaintCountingView()
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: root}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()
	waitFor(t, func() bool { return be.Enabled() && root.paints.Load() > 0 })

	initialPaints := root.paints.Load()
	blocked := make(chan struct{})
	release := make(chan struct{})
	if err := app.Post(func(*UpdateCtx) {
		close(blocked)
		<-release
	}); err != nil {
		t.Fatalf("Post blocker: %v", err)
	}

	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("app loop did not run blocker")
	}

	for range 10 {
		if err := app.PostInvalidate(root.Rect()); err != nil {
			t.Fatalf("PostInvalidate: %v", err)
		}
	}
	close(release)

	waitFor(t, func() bool { return root.paints.Load() > initialPaints })
	time.Sleep(50 * time.Millisecond)
	if got := root.paints.Load() - initialPaints; got != 1 {
		t.Fatalf("paint passes after coalesced invalidations = %d, want 1", got)
	}

	app.Quit()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit")
	}
}

func TestApp_DrainUnsupported(t *testing.T) {
	app, err := NewWithBackend(Opts{Root: NewText("root")}, testbackend.New(geom.Size{W: 20, H: 4}, nil), nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	if err := app.DrainInput(time.Millisecond, time.Millisecond); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("DrainInput error = %v, want ErrUnsupported", err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}

	t.Fatal("condition not met before timeout")
}

// TestApp_New_RequiresRoot verifies that New returns an error when Root is nil.
func TestApp_New_RequiresRoot(t *testing.T) {
	_, err := New(Opts{})
	if err == nil {
		t.Errorf("New({}) error = nil, want error")
	}
}

// TestApp_Quit_BeforeRun is safe to call.
func TestApp_Quit_BeforeRun(t *testing.T) {
	app, err := New(Opts{Root: NewContainer()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Should not panic.
	app.Quit()
}

// TestApp_Post_AfterQuitReturnsErrClosed verifies ErrClosed is returned
// after the app is shut down.
func TestApp_Post_AfterQuitReturnsErrClosed(t *testing.T) {
	app, err := New(Opts{Root: NewContainer()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	app.Quit()

	// Post to a closed app should return ErrClosed.
	err = app.Post(func(*UpdateCtx) {})
	if !errors.Is(err, ErrClosed) {
		t.Errorf("Post after Quit error = %v, want ErrClosed", err)
	}
}

// TestApp_PostInvalidate_AfterQuitReturnsErrClosed verifies PostInvalidate
// returns ErrClosed after Quit.
func TestApp_PostInvalidate_AfterQuitReturnsErrClosed(t *testing.T) {
	app, err := New(Opts{Root: NewContainer()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	app.Quit()

	if err := app.PostInvalidate(Rect{X: 0, Y: 0, W: 10, H: 1}); !errors.Is(err, ErrClosed) {
		t.Errorf("PostInvalidate after Quit error = %v, want ErrClosed", err)
	}

	if err := app.PostInvalidateAll(); !errors.Is(err, ErrClosed) {
		t.Errorf("PostInvalidateAll after Quit error = %v, want ErrClosed", err)
	}
}

// TestDefaultKeymap_EscAndCtrlCResolveToQuit verifies the default keymap
// resolves both Esc and Ctrl+C to ActionQuit.
func TestDefaultKeymap_EscAndCtrlCResolveToQuit(t *testing.T) {
	km := DefaultKeymap{}

	for _, k := range []Keystroke{
		{Key: event.KeyEsc},
		{Key: event.KeyCtrlC},
	} {
		got, ok := km.Resolve(KeyCtxGlobal, k)
		if !ok {
			t.Errorf("DefaultKeymap.Resolve(%v) = !ok, want true", k)
			continue
		}

		if got != ActionQuit {
			t.Errorf("DefaultKeymap.Resolve(%v) = %v, want ActionQuit", k, got)
		}
	}
}

// TestDefaultKeymap_UnboundReturnsFalse verifies unbound keys do not resolve.
func TestDefaultKeymap_UnboundReturnsFalse(t *testing.T) {
	km := DefaultKeymap{}

	got, ok := km.Resolve(KeyCtxGlobal, Keystroke{Key: event.KeyRune, Rune: 'x'})
	if ok || got != ActionNone {
		t.Errorf("Resolve(unbound) = (%v, %v), want (ActionNone, false)", got, ok)
	}
}

// TestIsZeroTheme verifies the theme zero-check helper.
func TestIsZeroTheme(t *testing.T) {
	if !isZeroTheme(Theme{}) {
		t.Errorf("isZeroTheme(zero) = false, want true")
	}

	nonZero := Theme{Base: style.Style{FG: style.ColorRed}}
	if isZeroTheme(nonZero) {
		t.Errorf("isZeroTheme(non-zero) = true, want false")
	}
}

// TestApp_ErrorsInitiallyEmpty verifies Errors() returns nil/empty before
// any operations.
func TestApp_ErrorsInitiallyEmpty(t *testing.T) {
	app, err := New(Opts{Root: NewContainer()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := app.Errors(); len(got) != 0 {
		t.Errorf("Errors() = %v, want empty", got)
	}
}

// TestApp_OnQuit_RegistersCallback verifies OnQuit stores the callback.
func TestApp_OnQuit_RegistersCallback(t *testing.T) {
	app, err := New(Opts{Root: NewContainer()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	called := false
	app.OnQuit(func() { called = true })

	if app.opts.OnQuit == nil {
		t.Errorf("OnQuit callback not stored")
	}

	// Manually invoke to verify the callback works.
	app.opts.OnQuit()
	if !called {
		t.Errorf("OnQuit callback not invoked")
	}
}

// TestKeymapAdapter_ForwardsToKeymap verifies the adapter passes through
// resolution.
func TestKeymapAdapter_ForwardsToKeymap(t *testing.T) {
	called := false
	km := &recordingKeymap{
		fn: func(_ KeyContext, _ Keystroke) (Action, bool) {
			called = true
			return ActionSubmit, true
		},
	}

	got, ok := keymapAdapter{km: km}.Resolve(KeyCtxGlobal, Keystroke{Key: event.KeyEnter})
	if !ok {
		t.Errorf("Resolve = !ok, want true")
	}

	if got != ActionSubmit {
		t.Errorf("Resolve = %v, want ActionSubmit", got)
	}

	if !called {
		t.Errorf("recordingKeymap.fn not called")
	}
}

func TestKeymapAdapter_ForwardsContext(t *testing.T) {
	km := &recordingKeymap{
		fn: func(ctx KeyContext, _ Keystroke) (Action, bool) {
			if ctx == KeyCtxTextInput {
				return ActionSubmit, true
			}

			return ActionActivate, true
		},
	}

	got, ok := keymapAdapter{km: km}.Resolve(KeyCtxTextInput, Keystroke{Key: event.KeyEnter})
	if !ok || got != ActionSubmit {
		t.Fatalf("Resolve text input = (%v, %v), want submit/true", got, ok)
	}

	got, ok = keymapAdapter{km: km}.Resolve(KeyCtxGlobal, Keystroke{Key: event.KeyEnter})
	if !ok || got != ActionActivate {
		t.Fatalf("Resolve global = (%v, %v), want activate/true", got, ok)
	}
}

// TestQuitWrapper_ActionQuitCallsQuit verifies the wrapper intercepts the
// configured quit action and calls Quit.
func TestQuitWrapper_ActionQuitCallsQuit(t *testing.T) {
	root := NewContainer()
	app, err := New(Opts{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := &quitWrapper{root: root, app: app}

	// ActionQuit should be intercepted.
	handled := w.HandleAction(ActionQuit, nil)
	if !handled {
		t.Errorf("HandleAction(ActionQuit) = false, want true")
	}

	// The Quit call should have been issued; Post() should now return ErrClosed
	// because the app loop has shut down.
	if err := app.Post(nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Post after Quit = %v, want ErrClosed", err)
	}
}

// The toolkit's hit test stops at views implementing MouseOpaque and
// delivers them every mouse event for their rect. The wrapper must implement
// it: without it the hit test descends through Children into the wrapped
// root's regions, and the root's Handle never sees a mouse event — its
// mouse routing (document wheel, status-line clicks, deepest-view dispatch)
// becomes dead code.
var _ interface{ MouseOpaque() } = (*quitWrapper)(nil)

// TestQuitWrapper_PassesThroughOtherActions verifies the wrapper falls
// through to the wrapped view for non-quit actions.
func TestQuitWrapper_PassesThroughOtherActions(t *testing.T) {
	inner := &actionRecordingView{id: NewID()}
	root := NewContainer()
	app, err := New(Opts{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := &quitWrapper{root: inner, app: app}

	handled := w.HandleAction(ActionSubmit, nil)
	if !handled {
		t.Errorf("HandleAction(ActionSubmit) = false, want true (from inner)")
	}

	if inner.lastAction != ActionSubmit {
		t.Errorf("inner.lastAction = %d, want %d", inner.lastAction, ActionSubmit)
	}
}

// recordingKeymap is a test Keymap that records resolution calls.
type recordingKeymap struct {
	fn func(KeyContext, Keystroke) (Action, bool)
}

func (r *recordingKeymap) Resolve(ctx KeyContext, k Keystroke) (Action, bool) {
	return r.fn(ctx, k)
}

// actionRecordingView is a test View that implements HandleAction.
type actionRecordingView struct {
	id         ID
	lastAction Action
	called     bool
}

type flushFailingBackend struct {
	*testbackend.Backend
	failAt  int64
	flushes atomic.Int64
}

var (
	errForcedFlush   = errors.New("forced flush")
	errForcedRestore = errors.New("forced restore")
)

func (b *flushFailingBackend) Flush() error {
	if b.flushes.Add(1) == b.failAt {
		return errForcedFlush
	}

	return b.Backend.Flush()
}

type restoreFailingBackend struct {
	*testbackend.Backend
	restoreCalled atomic.Bool
}

func (b *restoreFailingBackend) Restore() error {
	b.restoreCalled.Store(true)
	if err := b.Backend.Restore(); err != nil {
		return fmt.Errorf("restore test backend: %w", err)
	}
	return errForcedRestore
}

func (a *actionRecordingView) ID() ID                      { return a.id }
func (a *actionRecordingView) Rect() Rect                  { return Rect{} }
func (a *actionRecordingView) MinSize() Size               { return Size{W: 1, H: 1} }
func (a *actionRecordingView) Layout(_ Rect)               {}
func (a *actionRecordingView) Paint(_ Drawer, _ *Ctx)      {}
func (a *actionRecordingView) Handle(_ Event, _ *Ctx) bool { return false }
func (a *actionRecordingView) HandleAction(act Action, _ *Ctx) bool {
	a.lastAction = act
	a.called = true
	return true
}
func (a *actionRecordingView) Focusable() bool { return false }

type resizeRecordingView struct {
	id     ID
	rect   Rect
	paints atomic.Int64
	width  atomic.Int64
	height atomic.Int64
}

type paintCountingView struct {
	id     ID
	rect   Rect
	paints atomic.Int64
}

func newPaintCountingView() *paintCountingView {
	return &paintCountingView{id: NewID()}
}

func (p *paintCountingView) ID() ID                      { return p.id }
func (p *paintCountingView) Rect() Rect                  { return p.rect }
func (p *paintCountingView) MinSize() Size               { return Size{W: 1, H: 1} }
func (p *paintCountingView) Focusable() bool             { return false }
func (p *paintCountingView) Handle(_ Event, _ *Ctx) bool { return false }
func (p *paintCountingView) Layout(rect Rect)            { p.rect = rect }
func (p *paintCountingView) Paint(d Drawer, _ *Ctx) {
	p.paints.Add(1)
	d.DrawText(Point{X: p.rect.X, Y: p.rect.Y}, "paint", Style{})
}

func newResizeRecordingView() *resizeRecordingView {
	return &resizeRecordingView{id: NewID()}
}

func (r *resizeRecordingView) ID() ID                      { return r.id }
func (r *resizeRecordingView) Rect() Rect                  { return r.rect }
func (r *resizeRecordingView) MinSize() Size               { return Size{W: 1, H: 1} }
func (r *resizeRecordingView) Focusable() bool             { return false }
func (r *resizeRecordingView) Handle(_ Event, _ *Ctx) bool { return false }
func (r *resizeRecordingView) Layout(rect Rect) {
	r.rect = rect
	r.width.Store(int64(rect.W))
	r.height.Store(int64(rect.H))
}

func (r *resizeRecordingView) Paint(d Drawer, _ *Ctx) {
	r.paints.Add(1)
	d.DrawText(Point{X: r.rect.X, Y: r.rect.Y}, fmt.Sprintf("resize %dx%d", r.rect.W, r.rect.H), Style{})
}

func TestNewAppLayoutRegionOrder(t *testing.T) {
	header := NewText("header")
	chat := NewText("chat")
	pending := NewText("pending")
	status := NewText("status")
	above := NewText("above")
	editor := NewText("editor")
	below := NewText("below")
	footer := NewText("footer")

	appLayout := NewAppLayout(AppLayoutOpts{
		Header:       header,
		Chat:         chat,
		Pending:      pending,
		Status:       status,
		WidgetsAbove: above,
		Editor:       editor,
		WidgetsBelow: below,
		Footer:       footer,
	})

	children := appLayout.Children()
	want := []View{header, chat, pending, status, above, editor, below, footer}
	if len(children) != len(want) {
		t.Fatalf("child count = %d, want %d", len(children), len(want))
	}
	for i := range want {
		if children[i] != want[i] {
			t.Fatalf("child[%d] = %#v, want %#v", i, children[i], want[i])
		}
	}
}

func TestNewAppLayoutSkipsNilRegions(t *testing.T) {
	header := NewText("header")
	editor := NewText("editor")

	appLayout := NewAppLayout(AppLayoutOpts{Header: header, Editor: editor})
	children := appLayout.Children()
	if len(children) != 2 {
		t.Fatalf("child count = %d, want 2", len(children))
	}
	if children[0] != header || children[1] != editor {
		t.Fatalf("children = %#v, want header/editor", children)
	}
}

// Start claims the terminal without running the loop, so a host can drive the
// loop itself or interleave other work between claiming and running.
func TestAppStartClaimsTerminalWithoutRunningLoop(t *testing.T) {
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello")}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	if be.Enabled() {
		t.Fatal("backend enabled before Start")
	}
	if err := app.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !be.Enabled() {
		t.Fatal("Start did not enable the backend")
	}
	if be.Restored() {
		t.Fatal("Start restored the backend; that is Stop's job")
	}

	if err := app.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !be.Restored() {
		t.Fatal("Stop did not restore the backend")
	}
}

// Leaving the terminal in raw mode is the failure that matters, so Stop
// surfaces a restore error rather than only logging it.
func TestAppStopIsSafeWithoutStart(t *testing.T) {
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello")}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	// Must not panic, and must not report a spurious error.
	if err := app.Stop(); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
}

// Run remains the convenience wrapper: it must still enable and restore.
func TestAppRunStillWrapsStartAndStop(t *testing.T) {
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello")}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	be.Close()
	if err := app.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !be.Enabled() {
		t.Error("Run did not enable the backend")
	}
	if !be.Restored() {
		t.Error("Run did not restore the backend")
	}
}

// Start/Stop must be usable more than once — a host that suspends and resumes
// claims the terminal again.
func TestAppStartStopCycle(t *testing.T) {
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	app, err := NewWithBackend(Opts{Root: NewText("hello")}, be, nil)
	if err != nil {
		t.Fatalf("NewWithBackend: %v", err)
	}

	for i := range 2 {
		if err := app.Start(); err != nil {
			t.Fatalf("Start #%d: %v", i+1, err)
		}
		if !be.Enabled() {
			t.Fatalf("cycle %d: backend not enabled after Start", i+1)
		}
		if err := app.Stop(); err != nil {
			t.Fatalf("Stop #%d: %v", i+1, err)
		}
	}
}
