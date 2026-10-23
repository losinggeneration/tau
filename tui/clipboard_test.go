package tui

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/tau/tui/internal/testbackend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClipboardWriteUsesActualTransportAndSharedContext(t *testing.T) {
	for _, key := range []string{"DISPLAY", "WAYLAND_DISPLAY", "TERMUX_VERSION", "SSH_CLIENT", "SSH_CONNECTION", "MOSH_CONNECTION"} {
		t.Setenv(key, "")
	}
	t.Setenv("PI_CLIPBOARD_OSC52", "1")
	t.Setenv("PATH", t.TempDir())
	view := &themeRecordingView{paintCountingView: newPaintCountingView()}
	be := testbackend.New(geom.Size{W: 20, H: 4}, nil)
	defer be.Close()
	app, err := New(Opts{Root: view, Backend: be})
	require.NoError(t, err)
	var copied bool
	view.onPaint = func(*Ctx) {
		if copied {
			return
		}
		copied = true
		app.ClipboardWrite("héllo\x1b\a", func(err error) {
			assert.NoError(t, err)
			app.Quit()
		})
	}
	require.NoError(t, app.Run())
	encoded := base64.StdEncoding.EncodeToString([]byte("héllo\x1b\a"))
	assert.Contains(t, string(be.Output()), "\x1b]52;c;"+encoded+"\x07")
}

type clipboardFailBackend struct {
	*testbackend.Backend
	writeErr        error
	flushErr        error
	pendingFlushErr error
}

// Only the OSC 52 write, and the flush that follows it, fail.
func (b *clipboardFailBackend) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("\x1b]52;")) {
		if b.writeErr != nil {
			return 0, b.writeErr
		}
		b.pendingFlushErr = b.flushErr
	}
	return b.Backend.Write(p)
}

func (b *clipboardFailBackend) Flush() error {
	err := b.pendingFlushErr
	b.pendingFlushErr = nil
	return err
}

func TestClipboardWritePreservesTerminalErrors(t *testing.T) {
	for _, key := range []string{"DISPLAY", "WAYLAND_DISPLAY", "TERMUX_VERSION", "SSH_CLIENT", "SSH_CONNECTION", "MOSH_CONNECTION"} {
		t.Setenv(key, "")
	}
	t.Setenv("PI_CLIPBOARD_OSC52", "1")
	t.Setenv("PATH", t.TempDir())
	for _, phase := range []string{"write", "flush"} {
		t.Run(phase, func(t *testing.T) {
			be := &clipboardFailBackend{Backend: testbackend.New(geom.Size{W: 20, H: 4}, nil)}
			defer be.Close()
			view := &themeRecordingView{paintCountingView: newPaintCountingView()}
			app, err := New(Opts{Root: view, Backend: be})
			require.NoError(t, err)
			boom := errors.New("clipboard transport failed")
			if phase == "write" {
				be.writeErr = boom
			} else {
				be.flushErr = boom
			}
			var copied bool
			var copyErr error
			view.onPaint = func(*Ctx) {
				if copied {
					return
				}
				copied = true
				app.ClipboardWrite("text", func(err error) {
					copyErr = err
					app.Quit()
				})
			}
			require.NoError(t, app.Run())
			require.ErrorIs(t, copyErr, boom)
		})
	}
	var app *App
	var nilErr error
	app.ClipboardWrite("text", func(err error) { nilErr = err })
	require.ErrorIs(t, nilErr, ErrUnsupported)
}
