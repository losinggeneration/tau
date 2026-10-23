package tui

import (
	"strings"
	"testing"

	"github.com/losinggeneration/rovel/backend/headless"
	"github.com/losinggeneration/rovel/geom"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCmdCopyReportsLocalClipboardFailure(t *testing.T) {
	for _, key := range []string{"WAYLAND_DISPLAY", "TERMUX_VERSION", "SSH_CLIENT", "SSH_CONNECTION", "MOSH_CONNECTION", "PI_CLIPBOARD_OSC52"} {
		t.Setenv(key, "")
	}
	t.Setenv("DISPLAY", ":0")
	t.Setenv("PATH", t.TempDir())
	a := newTestApp(t)
	a.conv.AddAssistantMessage("answer")
	be := headless.New(geom.Size{W: 20, H: 4})
	defer be.Close()
	var err error
	a.app, err = tauui.New(tauui.Opts{Root: tauui.NewText("test"), Backend: be})
	require.NoError(t, err)
	a.loopCtx = &tauui.Ctx{ClipboardWrite: func(string) { t.Fatal("error-returning copy path bypassed") }}
	var last string
	var poll func(*tauui.UpdateCtx)
	poll = func(*tauui.UpdateCtx) {
		msgs := a.conv.Messages()
		if last = msgs[len(msgs)-1].Text; strings.HasPrefix(last, "copy:") {
			a.app.Quit()
			return
		}
		go func() { assert.NoError(t, a.app.Post(poll)) }()
	}
	require.NoError(t, a.app.Post(func(u *tauui.UpdateCtx) {
		cmdCopy(a, "")
		poll(u)
	}))
	require.NoError(t, a.app.Run())
	assert.Contains(t, last, "copy: clipboard unavailable")
	assert.NotContains(t, last, "copied last")
}
