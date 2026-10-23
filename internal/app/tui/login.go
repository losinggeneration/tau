package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/tau/ai/oauth"
	"github.com/losinggeneration/tau/ai/store"
	tauui "github.com/losinggeneration/tau/tui"
)

type loginReply struct {
	text string
	err  error
}
type loginPrompt struct {
	message, placeholder string
	result               chan loginReply
}
type loginDialog struct {
	app      *App
	ctx      context.Context
	cancel   context.CancelFunc
	title    string
	lines    []string
	progress string
	input    *tauui.InputPrimitive
	prompt   *loginPrompt
}

func (a *App) startOAuthLogin(provider oauth.Provider) {
	a.closeDialogComponent()
	a.dismissSelector()
	ctx, cancel := context.WithCancel(a.ctxOrBackground())
	d := &loginDialog{app: a, ctx: ctx, cancel: cancel, title: "Log in to " + provider.Name()}
	a.loginDialog = d
	picker := a.dialogPicker
	picker.Reset()
	picker.SetHeader(d.title)
	picker.SetComponent(d)
	picker.SetOnCancel(a.dismissDialog)
	picker.SetShown(true)
	a.dialogInputActive.Store(true)
	a.programStatus.blocked = tauui.ProgramStatus{State: "blocked", Kind: "auth", Message: d.title}
	a.refreshProgramStatus()
	a.refreshChat()
	if err := a.app.Post(func(u *tauui.UpdateCtx) {
		if a.loginDialog == d {
			u.InvalidateLayout()
			u.InvalidateAll()
			a.app.SetFocus(picker.ID())
		}
	}); err != nil {
		a.dismissDialog()
		a.conv.AddError("login: " + err.Error())
		a.refreshChat()
		return
	}
	credentials := a.authStore
	a.loginWorkers.Add(1)
	go func() {
		defer a.loginWorkers.Done()
		result, err := provider.Login(ctx, d)
		// Persistence runs on the loop so a displaced login cannot publish a late credential.
		if postErr := a.app.Post(func(*tauui.UpdateCtx) {
			if a.loginDialog != d || ctx.Err() != nil {
				return
			}
			if err == nil {
				if result == nil || result.Access == "" {
					err = errors.New("provider returned no access token")
				} else {
					err = credentials.Set(ctx, string(provider.ID()), store.CredentialFromOAuth(result))
				}
			}
			a.dismissDialog()
			if err != nil {
				a.conv.AddError("login: " + err.Error())
			} else {
				a.conv.AddSystem("Logged in to " + string(provider.ID()) + " via OAuth")
			}
			a.refreshChat()
			a.refreshFooterStats(a.loopContext())
		}); postErr != nil {
			cancel()
		}
	}()
}

func (a *App) stopLoginWorkers() {
	if d := a.loginDialog; d != nil {
		d.cancel()
		a.dismissDialogCtx(nil)
	}
	a.loginWorkers.Wait()
}

func (d *loginDialog) post(fn func()) error {
	if err := d.ctx.Err(); err != nil {
		return err
	}
	return d.app.app.Post(func(u *tauui.UpdateCtx) {
		if d.app.loginDialog != d || d.ctx.Err() != nil {
			return
		}
		fn()
		u.InvalidateLayout()
		u.InvalidateAll()
	})
}

func (d *loginDialog) OnAuth(url, instructions string) {
	if err := d.post(func() {
		d.lines = []string{url}
		if instructions != "" {
			d.lines = append(d.lines, instructions)
		}
	}); err != nil {
		d.cancel()
	}
}

func (d *loginDialog) OnProgress(message string) {
	if err := d.post(func() { d.progress = message }); err != nil {
		d.cancel()
	}
}

func (d *loginDialog) OnManualCodeInput(ctx context.Context) (string, error) {
	return d.OnPrompt(ctx, "Enter authorization code or redirect URL", "")
}

func (d *loginDialog) OnPrompt(ctx context.Context, message, placeholder string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p := &loginPrompt{message: message, placeholder: placeholder, result: make(chan loginReply, 1)}
	if err := d.post(func() {
		if ctx.Err() != nil {
			return
		}
		if old := d.prompt; old != nil {
			old.result <- loginReply{err: fmt.Errorf("login prompt replaced: %w", context.Canceled)}
		}
		d.prompt = p
		d.input = &tauui.InputPrimitive{Prompt: "> ", OnSubmit: func(value string) {
			if d.prompt != p {
				return
			}
			d.prompt = nil
			d.input = nil
			p.result <- loginReply{text: value, err: ctx.Err()}
		}}
	}); err != nil {
		return "", fmt.Errorf("open login prompt: %w", err)
	}
	select {
	case reply := <-p.result:
		if err := d.ctx.Err(); err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return reply.text, reply.err
	case <-ctx.Done():
	case <-d.ctx.Done():
	}
	if err := d.post(func() {
		if d.prompt == p {
			d.prompt = nil
			d.input = nil
		}
	}); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, tauui.ErrClosed) {
		return "", fmt.Errorf("withdraw login prompt: %w", err)
	}
	if err := d.ctx.Err(); err != nil {
		return "", err
	}
	return "", ctx.Err()
}

func (d *loginDialog) Render(width int) []string {
	var rows []string
	lines := append([]string{d.title}, d.lines...)
	if d.progress != "" {
		lines = append(lines, d.progress)
	}
	if d.prompt != nil {
		lines = append(lines, d.prompt.message)
		if d.prompt.placeholder != "" {
			lines = append(lines, d.prompt.placeholder)
		}
	}
	for _, line := range lines {
		for _, part := range strings.Split(line, "\n") {
			rows = append(rows, tauui.Wrap(sanitizeStatus(part), width)...)
		}
	}
	if d.input != nil {
		rows = append(rows, d.input.Render(width)...)
	}
	return append(rows, "Esc to cancel")
}

func (d *loginDialog) HandleKey(key event.KeyEvent) {
	if d.input != nil {
		d.input.HandleKey(key)
	}
}

func (d *loginDialog) Paste(value string) {
	if d.input != nil {
		d.input.Paste(value)
	}
}

func (a *App) hostedDialogInput() *tauui.InputPrimitive {
	if a == nil || a.dialogPicker == nil {
		return nil
	}
	switch d := a.dialogPicker.Component().(type) {
	case *extensionInput:
		return d.input
	case *loginDialog:
		return d.input
	}
	return nil
}

var _ oauth.LoginCallbacks = (*loginDialog)(nil)
