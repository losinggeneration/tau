package tui

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	baseui "github.com/losinggeneration/tau/ui"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/oauth"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type nativeLoginProvider struct {
	oauth.Provider
	login func(context.Context, oauth.LoginCallbacks) (*oauth.Credentials, error)
}

func (p nativeLoginProvider) ID() oauth.ProviderID { return "native-test" }
func (p nativeLoginProvider) Name() string         { return "Native test" }
func (p nativeLoginProvider) Login(ctx context.Context, c oauth.LoginCallbacks) (*oauth.Credentials, error) {
	return p.login(ctx, c)
}

func TestNativeLoginPromptsAndPreservesCredentials(t *testing.T) {
	t.Setenv("TAU_PROGRAM_STATUS", "")
	t.Setenv("PI_PROGRAM_STATUS", "1")
	a, be := nativeDialogApp(t)
	credentials := appCredentialStore(t)
	a.authStore = credentials
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	p := nativeLoginProvider{login: func(ctx context.Context, c oauth.LoginCallbacks) (*oauth.Credentials, error) {
		c.OnAuth("https://example.com/auth?state=test", "Enter code ABC")
		c.OnProgress("Waiting for authentication")
		value, err := c.OnPrompt(ctx, "Method", "browser or code")
		if !assert.NoError(t, err) {
			return nil, err
		}
		assert.Equal(t, "a b code", value)
		c.OnProgress("Exchanging code")
		return &oauth.Credentials{Access: "secret-access", Refresh: "secret-refresh", Expires: time.Now().Add(time.Hour).UnixMilli(), ProjectID: "project", Email: "test@example.com", AccountID: "account", Extra: map[string]any{"models": []string{"model"}}}, nil
	}}
	require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) { a.startOAuthLogin(p) }))
	var answer func(*tauui.UpdateCtx)
	answer = func(u *tauui.UpdateCtx) {
		if a.loginDialog == nil || a.loginDialog.input == nil {
			require.NoError(t, a.app.Post(answer))
			return
		}
		assert.Equal(t, "auth", a.programStatus.current().Kind)
		rows := a.loginDialog.Render(80)
		assert.Contains(t, rows, "https://example.com/auth?state=test")
		assert.Contains(t, rows, "Enter code ABC")
		for _, r := range "a b" {
			be.SendEvent(tauui.KeyEvent{Key: tauui.KeyRune, Rune: r})
		}
		be.SendEvent(tauui.PasteEvent{Text: " code"})
		be.SendEvent(tauui.KeyEvent{Key: tauui.KeyEnter})
		var finished func(*tauui.UpdateCtx)
		finished = func(*tauui.UpdateCtx) {
			if a.loginDialog != nil {
				require.NoError(t, a.app.Post(finished))
				return
			}
			a.app.Quit()
		}
		require.NoError(t, a.app.Post(finished))
	}
	require.NoError(t, a.app.Post(answer))
	require.NoError(t, a.Run(ctx))
	cred, err := credentials.Get(t.Context(), "native-test")
	require.NoError(t, err)
	assert.Equal(t, ai.CredentialTypeOAuth, cred.Type)
	assert.Equal(t, "secret-access", cred.AccessToken)
	assert.Equal(t, "secret-refresh", cred.RefreshToken)
	assert.Equal(t, "project", cred.ProjectID)
	assert.Equal(t, "account", cred.AccountID)
	assert.Equal(t, "test@example.com", cred.Email)
	assert.NotEmpty(t, cred.Extra)
	for _, m := range a.conv.Messages() {
		assert.NotContains(t, m.Text, "secret-")
		assert.NotContains(t, m.Text, "a b code")
	}
	assert.Empty(t, a.editor.Text())
	assert.Empty(t, a.programStatus.blocked.State)
	assert.Contains(t, string(be.Output()), "state=blocked:app=tau:kind=auth")
	assert.NotContains(t, string(be.Output()), "secret-access")
}

func TestNativeLoginCancellationReplacementAndShutdown(t *testing.T) {
	for _, mode := range []string{"escape", "ctrl-c", "native", "extension", "session", "shutdown", "quit", "late-success"} {
		t.Run(mode, func(t *testing.T) {
			a, be := nativeDialogApp(t)
			a.authStore = appCredentialStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			settled := make(chan struct{})
			var extensionDone chan struct{}
			p := nativeLoginProvider{login: func(ctx context.Context, c oauth.LoginCallbacks) (*oauth.Credentials, error) {
				defer close(settled)
				c.OnAuth("https://example.com", "Use code ABC")
				if mode == "late-success" {
					<-ctx.Done()
					return &oauth.Credentials{Access: "late"}, nil
				}
				_, err := c.OnManualCodeInput(ctx)
				return nil, err
			}}
			require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) { a.startOAuthLogin(p) }))
			onDialog(t, a, "Log in to Native test", func(*tauui.UpdateCtx) {
				switch mode {
				case "escape":
					be.SendEvent(tauui.KeyEvent{Key: tauui.KeyEsc})
				case "ctrl-c":
					be.SendEvent(tauui.KeyEvent{Key: tauui.KeyCtrlC})
				case "native":
					cmdSettingsOverlay(a, "")
				case "extension":
					extensionDone = make(chan struct{})
					go func() {
						defer close(extensionDone)
						_, err := a.Input(ctx, "replacement", "")
						assert.ErrorIs(t, err, baseui.ErrNoInput)
					}()
					onDialog(t, a, "replacement", func(*tauui.UpdateCtx) { a.dismissDialog() })
				case "session":
					a.replaceConversation(NewConversationModel())
				case "shutdown":
					a.app.Quit()
					return
				case "quit":
					a.quit()
					return
				case "late-success":
					a.dismissDialog()
				}
				var finished func(*tauui.UpdateCtx)
				finished = func(*tauui.UpdateCtx) {
					select {
					case <-settled:
						if mode == "native" {
							assert.Equal(t, "/settings", a.dialogPicker.Header())
							assert.True(t, a.dialogPicker.Shown())
							assert.Equal(t, "question", a.programStatus.current().Kind)
						}
						a.app.Quit()
					default:
						require.NoError(t, a.app.Post(finished))
					}
				}
				require.NoError(t, a.app.Post(finished))
			})
			require.NoError(t, a.Run(ctx))
			if extensionDone != nil {
				<-extensionDone
			}
			select {
			case <-settled:
			default:
				t.Fatal("login worker not joined")
			}
			_, err := a.authStore.Get(t.Context(), "native-test")
			assert.True(t, errors.Is(err, ai.ErrCredentialNotFound) || errors.Is(err, os.ErrNotExist), "unexpected credential result: %v", err)
			for _, m := range a.conv.Messages() {
				assert.NotContains(t, m.Text, "Logged in")
			}
		})
	}
}

func TestNativeLoginWithdrawsPromptWhenCallbackWins(t *testing.T) {
	a, _ := nativeDialogApp(t)
	a.authStore = appCredentialStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	withdraw := make(chan struct{})
	withdrawn := make(chan struct{})
	p := nativeLoginProvider{login: func(ctx context.Context, c oauth.LoginCallbacks) (*oauth.Credentials, error) {
		promptCtx, stop := context.WithCancel(ctx)
		result := make(chan error, 1)
		go func() { _, err := c.OnManualCodeInput(promptCtx); result <- err }()
		select {
		case <-withdraw:
		case <-ctx.Done():
		}
		stop()
		assert.ErrorIs(t, <-result, context.Canceled)
		close(withdrawn)
		_, err := c.OnPrompt(ctx, "Second prompt", "")
		return nil, err
	}}
	require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) { a.startOAuthLogin(p) }))
	var check func(*tauui.UpdateCtx)
	check = func(*tauui.UpdateCtx) {
		if a.loginDialog == nil || a.loginDialog.prompt == nil {
			require.NoError(t, a.app.Post(check))
			return
		}
		close(withdraw)
		var next func(*tauui.UpdateCtx)
		next = func(*tauui.UpdateCtx) {
			d := a.loginDialog
			if d == nil || d.prompt == nil || d.prompt.message != "Second prompt" || d.input == nil {
				require.NoError(t, a.app.Post(next))
				return
			}
			select {
			case <-withdrawn:
			default:
				t.Error("old prompt not joined")
			}
			assert.Empty(t, d.input.Value)
			a.dismissDialog()
			a.app.Quit()
		}
		require.NoError(t, a.app.Post(next))
	}
	require.NoError(t, a.app.Post(check))
	require.NoError(t, a.Run(ctx))
}

func TestNativeLoginReportsFailuresWithoutPersisting(t *testing.T) {
	for _, mode := range []string{"provider-error", "nil", "empty", "store-error"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := nativeDialogApp(t)
			a.authStore = appCredentialStore(t)
			if mode == "store-error" {
				a.authStore = failedLoginStore{CredentialStore: a.authStore}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			p := nativeLoginProvider{login: func(context.Context, oauth.LoginCallbacks) (*oauth.Credentials, error) {
				switch mode {
				case "provider-error":
					return nil, errors.New("denied")
				case "nil":
					return nil, nil
				case "empty":
					return &oauth.Credentials{}, nil
				}
				return &oauth.Credentials{Access: "secret"}, nil
			}}
			require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) {
				a.startOAuthLogin(p)
				var done func(*tauui.UpdateCtx)
				done = func(*tauui.UpdateCtx) {
					if a.loginDialog != nil {
						require.NoError(t, a.app.Post(done))
						return
					}
					a.app.Quit()
				}
				require.NoError(t, a.app.Post(done))
			}))
			require.NoError(t, a.Run(ctx))
			messages := a.conv.Messages()
			require.NotEmpty(t, messages)
			assert.Equal(t, RoleError, messages[len(messages)-1].Role)
			assert.NotContains(t, messages[len(messages)-1].Text, "secret")
			assert.Empty(t, a.programStatus.blocked.State)
		})
	}
}

type failedLoginStore struct{ CredentialStore }

func (failedLoginStore) Set(context.Context, string, ai.Credential) error {
	return errors.New("credential storage failed")
}

func TestNativeLoginShutdownJoinsProviderCleanup(t *testing.T) {
	a, _ := nativeDialogApp(t)
	a.authStore = appCredentialStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	cancelled := make(chan struct{})
	release := make(chan struct{})
	joined := make(chan struct{})
	p := nativeLoginProvider{login: func(ctx context.Context, _ oauth.LoginCallbacks) (*oauth.Credentials, error) {
		defer close(joined)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nil, ctx.Err()
	}}
	require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) { a.startOAuthLogin(p); a.app.Quit() }))
	releaseDone := make(chan struct{})
	go func() {
		defer close(releaseDone)
		select {
		case <-cancelled:
		case <-ctx.Done():
			t.Error("provider not cancelled")
		}
		close(release)
	}()
	require.NoError(t, a.Run(ctx))
	<-releaseDone
	select {
	case <-joined:
	default:
		t.Fatal("shutdown returned before provider cleanup")
	}
}

func TestNativeLoginRejectsCancelledPromptAndSanitizesDisplay(t *testing.T) {
	a, _ := nativeDialogApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d := &loginDialog{app: a, ctx: ctx, cancel: cancel, title: "login", lines: []string{"\x1b[31mred\x1b[0m\a\nsecond"}}
	_, err := d.OnPrompt(ctx, "expired", "")
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []string{"login", "red", "second", "Esc to cancel"}, d.Render(80))
	d.lines = []string{"https://example.com/a-long-authorization-path"}
	for _, row := range d.Render(10) {
		if row != "Esc to cancel" {
			assert.LessOrEqual(t, tauui.TextWidth(row), 10)
		}
	}
}

func TestNativeLoginCredentialCommandsCancelPendingAuthentication(t *testing.T) {
	for _, command := range []string{"login", "logout"} {
		t.Run(command, func(t *testing.T) {
			a, _ := nativeDialogApp(t)
			a.authStore = appCredentialStore(t)
			require.NoError(t, a.authStore.Set(t.Context(), "native-test", ai.NewAPIKeyCredential("old")))
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			settled := make(chan struct{})
			p := nativeLoginProvider{login: func(ctx context.Context, _ oauth.LoginCallbacks) (*oauth.Credentials, error) {
				defer close(settled)
				<-ctx.Done()
				return &oauth.Credentials{Access: "late"}, nil
			}}
			require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) {
				a.startOAuthLogin(p)
				if command == "login" {
					cmdLogin(a, "native-test explicit")
				} else {
					cmdLogout(a, "native-test")
				}
				assert.Nil(t, a.loginDialog)
				a.app.Quit()
			}))
			require.NoError(t, a.Run(ctx))
			<-settled
			cred, err := a.authStore.Get(t.Context(), "native-test")
			if command == "login" {
				require.NoError(t, err)
				assert.Equal(t, "explicit", cred.APIKey)
				assert.Equal(t, ai.CredentialTypeAPIKey, cred.Type)
			} else {
				require.Error(t, err)
			}
		})
	}
}
