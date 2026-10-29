package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/tau/ai"
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
	url      string
	hint     string
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
				a.completeProviderLogin(string(provider.ID()), "Logged in to "+string(provider.ID())+" via OAuth")
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
		d.url = url
		d.hint = d.app.keyHint(actionIDMessageCopy, "to copy")
		d.lines = nil
		if instructions != "" {
			d.lines = append(d.lines, instructions)
		}
		if err := d.app.openBrowser(url); err != nil {
			d.hint = "Could not open a browser: " + err.Error()
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
	lines := []string{d.title}
	if d.url != "" {
		lines = append(lines, d.url, "Ctrl+click to open • "+d.hint)
	}
	lines = append(lines, d.lines...)
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

// copyURL copies the sign-in URL and reports the result in the hint row, as
// upstream's AuthUrlComponent does.
func (d *loginDialog) copyURL() {
	if d.url == "" {
		return
	}
	d.app.writeClipboard(d.url, func(err error) {
		if d.app.loginDialog != d {
			return
		}
		if err != nil {
			d.hint = err.Error()
		} else {
			d.hint = "Copied URL to clipboard"
		}
		d.app.refreshChat()
	})
}

func (a *App) writeClipboard(text string, done func(error)) {
	switch {
	case a.clipboardWrite != nil:
		a.clipboardWrite(text, done)
	case a.app != nil:
		a.app.ClipboardWrite(text, done)
	default:
		done(tauui.ErrUnsupported)
	}
}

func (a *App) openBrowser(url string) error {
	if a.openURL != nil {
		return a.openURL(url)
	}
	// A test that forgets to stub openURL must not open the developer's browser.
	if testing.Testing() {
		return errors.New("browser launch is disabled under go test")
	}
	name, args := "xdg-open", []string{url}
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	}
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	// The launcher's exit status says nothing about the browser; Wait only reaps it.
	go func() { _ = cmd.Wait() }()
	return nil
}

// keyHint formats the first key bound to id with a description, as upstream's
// keyHint does; an unbound action yields just the description.
func (a *App) keyHint(id, description string) string {
	manager := a.keybindings
	if manager == nil {
		manager = defaultAppKeybindings()
	}
	if keys := manager.Keys(id); len(keys) > 0 {
		return keys[0] + " " + description
	}
	return description
}

// completeProviderLogin reports a finished login. With no current model it
// selects and saves the provider's default model, as upstream's
// completeProviderAuthentication does, and says why when it can't.
func (a *App) completeProviderLogin(provider, label string) {
	if a.currentModel != "" || a.sess == nil {
		a.conv.AddSystem(label)
		return
	}
	var problem string
	defaultID, ok := ai.DefaultModelPerProvider[provider]
	var models []*ai.Model
	for _, m := range a.availableModels() {
		if m.Provider == provider {
			models = append(models, m)
		}
	}
	var selected *ai.Model
	switch {
	case !ok:
		problem = fmt.Sprintf("no default model is configured for provider %q", provider)
	case len(models) == 0:
		problem = "no models are available for that provider"
	default:
		if i := slices.IndexFunc(models, func(m *ai.Model) bool { return m.ID == defaultID }); i >= 0 {
			selected = models[i]
		} else if provider == "radius" {
			// Radius catalogs vary by account, so the first model stands in.
			selected = models[0]
		} else {
			problem = fmt.Sprintf("its default model %q is not available", defaultID)
		}
	}
	if selected != nil {
		key := selected.Provider + "/" + selected.ID
		if err := a.commitModel(key); err != nil {
			problem = "selecting its default model failed: " + err.Error()
		} else {
			a.conv.AddSystem(label + ". Selected " + selected.ID)
			a.applySavedThinkingLevel(key)
			return
		}
	}
	a.conv.AddSystem(label)
	a.conv.AddError(label + ", but " + problem + ". Use /model to select a model.")
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

const (
	loginAccountLabel = "Sign in with an account"
	loginAPIKeyLabel  = "Sign in with an API key"
)

// loginOption is one provider and authentication method in the /login
// selectors, as upstream's AuthSelectorProvider is.
type loginOption struct {
	id, name string
	oauth    bool
}

// loginOptions lists account (OAuth) and API-key providers. method narrows the
// list to one label; empty lists both.
func loginOptions(method string) []loginOption {
	var options []loginOption
	if method != loginAPIKeyLabel {
		for _, p := range oauth.ListProviders() {
			options = append(options, loginOption{id: string(p.ID()), name: p.Name(), oauth: true})
		}
	}
	if method != loginAccountLabel {
		for _, id := range ai.ListSupportedProviders() {
			// Anthropic and Bedrock resolve keys outside the env registry.
			if ai.EnvVarForProvider(id) != "" || id == ai.ProviderAnthropic || id == ai.ProviderAmazonBedrock {
				options = append(options, loginOption{id: id, name: id})
			}
		}
	}
	return options
}

// startLogin opens the selection step for a /login reference, following
// upstream handleLoginCommand.
func (a *App) startLogin(ref string) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if ref == "" {
		a.showLoginMethods(nil, nil)
		return
	}
	var matches []loginOption
	for _, option := range loginOptions("") {
		if strings.ToLower(option.id) == ref || strings.ToLower(option.name) == ref {
			matches = append(matches, option)
		}
	}
	switch {
	case len(matches) == 1:
		a.startLoginOption(matches[0], nil)
	case len(matches) > 1 && matches[0].id == matches[len(matches)-1].id:
		a.showLoginMethods(matches, nil)
	default:
		a.showLoginProviders("", ref)
	}
}

func (a *App) startLoginOption(option loginOption, back func()) {
	if option.oauth {
		if p, ok := oauth.GetProvider(oauth.ProviderID(option.id)); ok {
			a.startOAuthLogin(p)
			return
		}
	}
	a.showAPIKeyLogin(option, back)
}

// showLoginMethods offers account and API-key sign-in. For one provider's
// options it picks that provider's method directly.
func (a *App) showLoginMethods(options []loginOption, back func()) {
	header := "Select authentication method:"
	labels := []string{loginAccountLabel, loginAPIKeyLabel}
	if options != nil {
		header = "Select authentication method for " + options[0].name + ":"
	}
	a.openLoginPicker(header, "", labels, func(label string) {
		if options == nil {
			a.showLoginProviders(label, "")
			return
		}
		for _, option := range options {
			if option.oauth == (label == loginAccountLabel) {
				a.startLoginOption(option, func() { a.showLoginMethods(options, back) })
				return
			}
		}
	}, back)
}

func (a *App) showLoginProviders(method, filter string) {
	options := loginOptions(method)
	labels := make([]string, len(options))
	for i, option := range options {
		labels[i] = option.name
	}
	var back func()
	if method != "" {
		back = func() { a.showLoginMethods(nil, nil) }
	}
	a.openLoginPicker("Select provider:", filter, labels, func(label string) {
		for _, option := range options {
			if option.name == label {
				a.startLoginOption(option, func() { a.showLoginProviders(method, filter) })
				return
			}
		}
	}, back)
}

// showAPIKeyLogin asks for a key and saves it, as upstream's API-key login
// dialog does. Cancelling returns to the step that opened it.
func (a *App) showAPIKeyLogin(option loginOption, back func()) {
	header := "Enter API key for " + option.name
	input := &tauui.InputPrimitive{Prompt: "> "}
	input.OnSubmit = func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		a.dismissDialog()
		if a.authStore == nil {
			a.conv.AddError("login: credential store unavailable")
		} else if err := a.authStore.Set(a.ctxOrBackground(), option.id, ai.NewAPIKeyCredential(value)); err != nil {
			a.conv.AddError("Failed to save API key for " + option.name + ": " + err.Error())
		} else {
			a.completeProviderLogin(option.id, "Saved API key for "+option.name)
		}
		a.refreshChat()
	}
	cancel := func() {
		a.dismissDialog()
		if back != nil {
			back()
		}
	}
	input.OnCancel = cancel
	a.showLoginStep(header, &extensionInput{title: header, input: input}, nil, cancel)
}

func (a *App) openLoginPicker(header, filter string, labels []string, onSelect func(string), back func()) {
	update := func(filter string) {
		var items []tauui.DialogPickerItem
		for _, label := range labels {
			if strings.Contains(strings.ToLower(label), strings.ToLower(filter)) {
				items = append(items, tauui.DialogPickerItem{Name: sanitizeStatus(label), Value: label})
			}
		}
		a.dialogPicker.SetItems(items)
	}
	cancel := func() {
		a.dismissDialog()
		if back != nil {
			back()
		}
	}
	a.showLoginStep(header, nil, func() {
		a.dialogPicker.SetFilter(filter)
		update(filter)
		a.dialogPicker.SetOnChange(update)
		a.dialogPicker.SetOnSelect(func(_ int, item tauui.DialogPickerItem) {
			a.dismissDialog()
			onSelect(item.Value)
		})
	}, cancel)
}

// showLoginStep replaces the dialog picker contents with one login step and
// focuses it once the rebuilt view tree contains the picker.
func (a *App) showLoginStep(header string, component tauui.DialogPickerComponent, configure func(), cancel func()) {
	if a.app == nil || a.dialogPicker == nil {
		a.conv.AddError("login: no dialog available; use /login <provider> <api-key>")
		a.refreshChat()
		return
	}
	a.closeDialogComponent()
	a.dismissSelector()
	picker := a.dialogPicker
	picker.Reset()
	picker.SetHeader(header)
	picker.SetComponent(component)
	if configure != nil {
		configure()
	}
	picker.SetOnCancel(cancel)
	picker.SetShown(true)
	a.dialogInputActive.Store(component != nil)
	a.programStatus.blocked = tauui.ProgramStatus{State: "blocked", Kind: "auth", Message: header}
	a.refreshProgramStatus()
	a.refreshChat()
	if err := a.app.Post(func(u *tauui.UpdateCtx) {
		if picker.Shown() && picker.Header() == header {
			u.InvalidateLayout()
			u.InvalidateAll()
			a.app.SetFocus(picker.ID())
		}
	}); err != nil {
		a.dismissDialog()
		a.conv.AddError("login: " + err.Error())
		a.refreshChat()
	}
}
