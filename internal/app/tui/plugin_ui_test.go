package tui

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/losinggeneration/rovel/backend/headless"
	"github.com/losinggeneration/rovel/geom"
	tauui "github.com/losinggeneration/tau/tui"
	baseui "github.com/losinggeneration/tau/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtensionDialogsRejectCancelledContexts(t *testing.T) {
	a := newTestApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := a.Confirm(ctx, "title", "message")
	require.ErrorIs(t, err, context.Canceled)
	_, err = a.Select(ctx, "title", []string{"one"})
	require.ErrorIs(t, err, context.Canceled)
	_, err = a.Input(ctx, "title", "hint")
	require.ErrorIs(t, err, context.Canceled)
}

func TestExtensionDialogsThroughNativePicker(t *testing.T) {
	for _, kind := range []string{"confirm", "no", "select", "filter", "empty-select", "input", "empty-input", "cancel-confirm", "cancel-select", "cancel-input"} {
		t.Run(kind, func(t *testing.T) {
			a := newTestApp(t)
			a.editor = tauui.NewPromptEditor()
			a.dialogPicker = tauui.NewDialogPicker()
			be := headless.New(geom.Size{W: 60, H: 20})
			defer be.Close()
			var err error
			a.app, err = tauui.New(tauui.Opts{Root: a.dialogPicker, Backend: be})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				switch kind {
				case "confirm":
					value, e := a.Confirm(ctx, "allow", "message")
					if e != nil || !value {
						t.Errorf("confirm=%v,%v", value, e)
					}
				case "no":
					value, e := a.Confirm(ctx, "deny", "message")
					if e != nil || value {
						t.Errorf("no=%v,%v", value, e)
					}
				case "filter":
					value, e := a.Select(ctx, "filter", []string{"one", "two", "three"})
					if e != nil || value != 2 {
						t.Errorf("filter=%v,%v", value, e)
					}
				case "empty-select":
					value, e := a.Select(ctx, "empty", nil)
					if e != nil || value != -1 {
						t.Errorf("empty select=%v,%v", value, e)
					}
				case "empty-input":
					value, e := a.Input(ctx, "empty", "hint")
					if e != nil || value != "" {
						t.Errorf("empty input=%q,%v", value, e)
					}
				case "select":
					value, e := a.Select(ctx, "pick", []string{"same", "same", "other"})
					if e != nil || value != 1 {
						t.Errorf("select=%v,%v", value, e)
					}
				case "input":
					value, e := a.Input(ctx, "name", "hint")
					if e != nil || value != "a b" {
						t.Errorf("input=%q,%v", value, e)
					}
				case "cancel-confirm":
					value, e := a.Confirm(ctx, "allow", "message")
					if e != nil || value {
						t.Errorf("cancel confirm=%v,%v", value, e)
					}
				case "cancel-select":
					value, e := a.Select(ctx, "pick", []string{"one"})
					if e != nil || value != -1 {
						t.Errorf("cancel select=%v,%v", value, e)
					}
				case "cancel-input":
					_, e := a.Input(ctx, "name", "hint")
					if e != baseui.ErrNoInput {
						t.Errorf("cancel input=%v", e)
					}
				}
				a.app.Quit()
			}()
			// Poll on the loop, not from the caller goroutine, so picker state stays serialized.
			var answer func(*tauui.UpdateCtx)
			answer = func(u *tauui.UpdateCtx) {
				if !a.dialogPicker.Shown() {
					if e := a.app.Post(answer); e != nil {
						t.Errorf("post: %v", e)
					}
					return
				}
				a.app.SetFocus(a.dialogPicker.ID())
				switch kind {
				case "no":
					a.dialogPicker.HandleAction(int(tauui.ActionMoveDown), nil)
				case "empty-select":
					a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyEsc}, nil)
					return
				case "filter":
					a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'h'}, nil)
				case "select":
					a.dialogPicker.HandleAction(int(tauui.ActionMoveDown), nil)
				case "input":
					for _, r := range "a b" {
						a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: r}, nil)
					}
				}
				if len(kind) > 7 && kind[:7] == "cancel-" {
					a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyEsc}, nil)
				} else {
					a.dialogPicker.HandleAction(int(tauui.ActionActivate), nil)
				}
			}
			require.NoError(t, a.app.Post(answer))
			require.NoError(t, a.app.Run())
			<-done
			require.False(t, a.dialogPicker.Shown())
		})
	}
}

func nativeDialogApp(t *testing.T) (*App, *headless.Backend) {
	t.Helper()
	a, err := New(Options{Session: &fakeSession{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	be := headless.New(geom.Size{W: 60, H: 20})
	t.Cleanup(be.Close)
	a.app, err = tauui.New(tauui.Opts{Root: a.root, Backend: be, Theme: appTheme(), Keymap: appKeymap{app: a, manager: a.keybindings}})
	require.NoError(t, err)
	return a, be
}

func onDialog(t *testing.T, a *App, title string, fn func(*tauui.UpdateCtx)) {
	t.Helper()
	var check func(*tauui.UpdateCtx)
	check = func(u *tauui.UpdateCtx) {
		if a.dialogPicker.Shown() && a.dialogPicker.Header() == title {
			fn(u)
			return
		}
		if err := a.app.Post(check); err != nil {
			t.Errorf("wait for dialog: %v", err)
		}
	}
	require.NoError(t, a.app.Post(check))
}

func TestExtensionDialogNativeInputRoutingAndPermissionStatus(t *testing.T) {
	for _, kind := range []string{"confirm", "input"} {
		t.Run(kind, func(t *testing.T) {
			a, be := nativeDialogApp(t)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				if kind == "confirm" {
					value, err := a.Confirm(ctx, "allow", "execute?")
					if err != nil || !value {
						t.Errorf("confirm=%v,%v", value, err)
					}
				} else {
					value, err := a.Input(ctx, "input", "hint")
					if err != nil || value != "a b paste text" {
						t.Errorf("input=%q,%v", value, err)
					}
				}
				a.app.Quit()
			}()
			title := "input"
			if kind == "confirm" {
				title = "allow\nexecute?"
			}
			onDialog(t, a, title, func(*tauui.UpdateCtx) {
				want := "question"
				if kind == "confirm" {
					want = "permission"
				}
				assert.Equal(t, want, a.programStatus.current().Kind)
				// The queued focus request runs before these backend events.
				if kind == "input" {
					for _, r := range "a b" {
						be.SendEvent(tauui.KeyEvent{Key: tauui.KeyRune, Rune: r})
					}
					be.SendEvent(tauui.PasteEvent{Text: " paste\ntext"})
				}
				be.SendEvent(tauui.KeyEvent{Key: tauui.KeyEnter})
			})
			require.NoError(t, a.Run(ctx))
			<-done
			assert.False(t, a.dialogPicker.Shown())
			assert.Empty(t, a.programStatus.blocked.State)
			assert.Empty(t, a.editor.Text(), "dialog input leaked into prompt")
		})
	}
}

func TestExtensionDialogCancellationReplacementAndShutdown(t *testing.T) {
	for _, mode := range []string{"context", "replacement", "native-replacement", "session", "shutdown", "quit"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := nativeDialogApp(t)
			runCtx, stop := context.WithTimeout(t.Context(), time.Second)
			defer stop()
			ctx, cancel := context.WithCancel(runCtx)
			defer cancel()
			first := make(chan error, 1)
			go func() { _, err := a.Input(ctx, "first", "hint"); first <- err }()
			var second chan error
			onDialog(t, a, "first", func(*tauui.UpdateCtx) {
				switch mode {
				case "context":
					cancel()
				case "session":
					a.replaceConversation(NewConversationModel())
				case "native-replacement":
					cmdSettingsOverlay(a, "")
					assert.NotNil(t, a.dialogPicker.Component())
					assert.Empty(t, a.extensionDialog)
				case "shutdown":
					a.app.Quit()
				case "quit":
					a.quit()
				case "replacement":
					second = make(chan error, 1)
					go func() { _, err := a.Input(runCtx, "second", ""); second <- err }()
					onDialog(t, a, "second", func(*tauui.UpdateCtx) {
						cancel()
						assert.Equal(t, "question", a.programStatus.current().Kind)
						a.dialogPicker.HandleAction(int(tauui.ActionActivate), nil)
					})
				}
			})
			// Waiters are joined outside the UI loop; finishing a dialog must never need
			// the blocked extension caller to service UI events.
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				err := <-first
				switch mode {
				case "context":
					assert.ErrorIs(t, err, context.Canceled)
				case "shutdown", "quit":
					assert.ErrorIs(t, err, tauui.ErrClosed)
				default:
					assert.ErrorIs(t, err, baseui.ErrNoInput)
				}
				if mode == "replacement" {
					if err := <-second; err != nil {
						t.Errorf("second dialog: %v", err)
					}
				}
				if mode != "shutdown" && mode != "quit" {
					if err := a.app.Post(func(*tauui.UpdateCtx) {
						if mode == "native-replacement" {
							assert.True(t, a.dialogPicker.Shown())
							a.dismissDialog()
						}
						a.app.Quit()
					}); err != nil {
						t.Errorf("finish loop: %v", err)
					}
				}
			}()
			require.NoError(t, a.Run(runCtx))
			<-joined
		})
	}
}

func TestExtensionDialogCancelledOpenerAndClosedAdmission(t *testing.T) {
	a, _ := nativeDialogApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := a.Input(ctx, "stale", ""); done <- err }()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) { assert.False(t, a.dialogPicker.Shown()); a.app.Quit() }))
	require.NoError(t, a.Run(t.Context()))
	_, err := a.Select(t.Context(), "closed", []string{"one"})
	require.ErrorIs(t, err, tauui.ErrClosed)
}

func TestExtensionDialogSettlementRetainsCancellationIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	d := &extensionDialog{ctx: ctx, result: make(chan extensionDialogResult, 1)}
	cancel()
	d.finish(extensionDialogResult{index: 0, text: "late"})
	d.finish(extensionDialogResult{index: 1})
	result := <-d.result
	require.ErrorIs(t, result.err, context.Canceled)
	require.Equal(t, -1, result.index)
	require.Empty(t, result.text)
	require.Empty(t, d.result)
}

func TestExtensionDialogShutdownRejectsQueuedOpeners(t *testing.T) {
	a, _ := nativeDialogApp(t)
	result := make(chan error, 1)
	go func() { _, err := a.Input(t.Context(), "never", "hint"); result <- err }()
	// Admission is closed before any posted opener can run.
	a.stopExtensionDialogs()
	require.ErrorIs(t, <-result, tauui.ErrClosed)
	require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) { assert.False(t, a.dialogPicker.Shown()); a.app.Quit() }))
	require.NoError(t, a.Run(t.Context()))
}

func TestExtensionDialogCancelledCleanupCannotHideNativeReplacement(t *testing.T) {
	a, _ := nativeDialogApp(t)
	runCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	ctx, cancel := context.WithCancel(runCtx)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := a.Input(ctx, "cancel then replace", ""); result <- err }()
	onDialog(t, a, "cancel then replace", func(*tauui.UpdateCtx) {
		cancel()
		cmdSettingsOverlay(a, "")
	})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		assert.ErrorIs(t, <-result, context.Canceled)
		if err := a.app.Post(func(*tauui.UpdateCtx) {
			assert.True(t, a.dialogPicker.Shown())
			assert.Equal(t, "/settings", a.dialogPicker.Header())
			assert.NotNil(t, a.dialogPicker.Component())
			assert.Equal(t, "question", a.programStatus.current().Kind)
			a.dismissDialog()
			a.app.Quit()
		}); err != nil {
			t.Errorf("check replacement: %v", err)
		}
	}()
	require.NoError(t, a.Run(runCtx))
	<-joined
}

func TestExtensionDialogNativeEditingKeysAndControlCancel(t *testing.T) {
	for _, cancelKey := range []tauui.KeyEvent{{Key: tauui.KeyEsc}, {Key: tauui.KeyCtrlC}, {Key: tauui.KeyRune, Rune: 'c', Mod: tauui.ModCtrl}} {
		t.Run(fmt.Sprint(cancelKey), func(t *testing.T) {
			a, be := nativeDialogApp(t)
			ctx, stop := context.WithTimeout(t.Context(), time.Second)
			defer stop()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err := a.Input(ctx, "editing", "")
				assert.ErrorIs(t, err, baseui.ErrNoInput)
				a.app.Quit()
			}()
			onDialog(t, a, "editing", func(*tauui.UpdateCtx) {
				for _, key := range []tauui.KeyEvent{{Key: tauui.KeyRune, Rune: 'a'}, {Key: tauui.KeyRune, Rune: 'b'}, {Key: tauui.KeyHome}, {Key: tauui.KeyDelete}, {Key: tauui.KeyEnd}, {Key: tauui.KeyRune, Rune: 'd', Mod: tauui.ModCtrl}, cancelKey} {
					be.SendEvent(key)
				}
			})
			require.NoError(t, a.Run(ctx))
			<-done
			assert.Empty(t, a.editor.Text())
		})
	}
}

func TestExtensionDialogDeadlineAndFailedAdmission(t *testing.T) {
	a, _ := nativeDialogApp(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := a.Input(ctx, "expired", "")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	a.app.Quit()
	_, err = a.Input(t.Context(), "closed", "")
	require.ErrorIs(t, err, tauui.ErrClosed)
}
