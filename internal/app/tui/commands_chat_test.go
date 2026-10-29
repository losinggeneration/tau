package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/losinggeneration/rovel/backend/headless"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/tau/sdk"
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

func TestCmdImportConfirmsImportsAndResumes(t *testing.T) {
	for _, answer := range []string{"yes", "no", "fail"} {
		t.Run(answer, func(t *testing.T) {
			a, _ := nativeDialogApp(t)
			a.sess.(*fakeSession).stateCWD = "/work"
			var imported []string
			a.importSession = func(_ context.Context, src, cwd string) (string, error) {
				imported = append(imported, src+"|"+cwd)
				if answer == "fail" {
					return "", errors.New("session x is already stored")
				}
				return "imported-id", nil
			}
			var resumed string
			a.sessionFactory = func(_ context.Context, opts sdk.SessionOptions) (sdk.Session, error) {
				resumed = opts.SessionID
				return &fakeSession{stateID: "imported-id", stateCWD: "/work"}, nil
			}
			require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) { cmdImport(a, `"/tmp/my session.jsonl" extra`) }))
			onFocusedDialog(t, a, "Import session\nReplace current session with /tmp/my session.jsonl?", func(*tauui.UpdateCtx) {
				if answer == "no" {
					a.dialogPicker.HandleAction(int(tauui.ActionMoveDown), nil)
				}
				a.dialogPicker.HandleAction(int(tauui.ActionActivate), nil)
				var settled func(*tauui.UpdateCtx)
				settled = func(*tauui.UpdateCtx) {
					msgs := a.conv.Messages()
					if len(msgs) == 0 {
						require.NoError(t, a.app.Post(settled))
						return
					}
					a.app.Quit()
				}
				require.NoError(t, a.app.Post(settled))
			})
			require.NoError(t, a.app.Run())

			msgs := a.conv.Messages()
			last := msgs[len(msgs)-1].Text
			switch answer {
			case "yes":
				assert.Equal(t, []string{"/tmp/my session.jsonl|/work"}, imported)
				assert.Equal(t, "imported-id", resumed)
				assert.Equal(t, "Session imported from: /tmp/my session.jsonl", last)
				assert.Equal(t, "imported-id", a.sess.State().SessionID())
			case "no":
				assert.Empty(t, imported)
				assert.Empty(t, resumed)
				assert.Equal(t, "Import cancelled", last)
			case "fail":
				assert.Empty(t, resumed)
				assert.Equal(t, "Failed to import session: session x is already stored", last)
			}
		})
	}
}

func TestPathCommandArgumentHonorsQuotes(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "",
		"  a.jsonl b":       "a.jsonl",
		`"my file.jsonl" x`: "my file.jsonl",
		`'it.jsonl'`:        "it.jsonl",
		`"unclosed.jsonl`:   "",
	} {
		assert.Equal(t, want, pathCommandArgument(in), in)
	}
	a := newTestApp(t)
	cmdImport(a, "")
	msgs := a.conv.Messages()
	require.NotEmpty(t, msgs)
	assert.Equal(t, "Usage: /import <path.jsonl>", msgs[len(msgs)-1].Text)
}
