package oauth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	airoot "github.com/losinggeneration/tau/ai"
)

// Anthropic OAuth provider for Claude Pro/Max subscriptions.
type anthropicProvider struct {
	clientID     string
	authURL      string
	tokenURL     string
	callbackPort int
	scopes       string
}

const (
	anthropicClientID = "9d1c250a-e61b-44d9-889e-5944d1962f5e"
	// anthropicCallbackPort is preferred so the port can be forwarded; any
	// loopback port is accepted when it cannot be bound.
	anthropicCallbackPort        = 53692
	anthropicCopyCodeRedirectURI = "https://platform.claude.com/oauth/code/callback"
)

func NewAnthropicProvider() Provider {
	return &anthropicProvider{
		clientID:     anthropicClientID,
		authURL:      "https://claude.ai/oauth/authorize",
		tokenURL:     "https://platform.claude.com/v1/oauth/token",
		callbackPort: anthropicCallbackPort,
		scopes:       "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload",
	}
}

func (p *anthropicProvider) ID() ProviderID {
	return ProviderAnthropic
}

func (p *anthropicProvider) Name() string {
	return "Anthropic (Claude Pro/Max)"
}

func (p *anthropicProvider) UsesCallbackServer() bool {
	return true
}

func (p *anthropicProvider) ModifyModels(models []*airoot.Model, credentials *Credentials) []*airoot.Model {
	return models
}

func (p *anthropicProvider) Login(ctx context.Context, callbacks LoginCallbacks) (*Credentials, error) {
	method, err := callbacks.OnPrompt(ctx, "Sign in to Anthropic with browser (default) or copy code (headless) [browser/copy-code]: ", "browser")
	if errors.Is(err, ErrPromptUnavailable) {
		method, err = "browser", nil
	}
	if err != nil {
		return nil, fmt.Errorf("anthropic login method: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "", "browser", "1":
		return p.loginWithBrowser(ctx, callbacks)
	case "copy-code", "copy_code", "2":
		return p.loginWithCopyCode(ctx, callbacks)
	default:
		return nil, fmt.Errorf("unknown Anthropic login method: %s", method)
	}
}

func (p *anthropicProvider) authURLFor(redirectURI, challenge, verifier string) string {
	return BuildAuthURL(p.authURL, map[string]string{
		"code":                  "true",
		"client_id":             p.clientID,
		"response_type":         "code",
		"redirect_uri":          redirectURI,
		"scope":                 p.scopes,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"state":                 verifier,
	})
}

func (p *anthropicProvider) loginWithBrowser(ctx context.Context, callbacks LoginCallbacks) (creds *Credentials, err error) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}

	// Without a callback server, login continues with the pasted redirect URL.
	redirectURI := fmt.Sprintf("http://localhost:%d/callback", anthropicCallbackPort)
	server := newCallbackServer(verifier, p.callbackPort)
	if server.Start() == nil {
		defer func() { err = errors.Join(err, server.Stop()) }()
		redirectURI = server.URL()
	} else {
		server = nil
	}

	callbacks.OnAuth(p.authURLFor(redirectURI, challenge, verifier), "Complete login in your browser. If the browser is on another machine, paste the final redirect URL here.")
	code, err := waitForAuthorizationCode(ctx, callbacks, server, verifier)
	if err != nil {
		return nil, fmt.Errorf("wait for code: %w", err)
	}
	return p.exchange(ctx, callbacks, code, verifier, redirectURI)
}

func (p *anthropicProvider) loginWithCopyCode(ctx context.Context, callbacks LoginCallbacks) (*Credentials, error) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}
	callbacks.OnAuth(p.authURLFor(anthropicCopyCodeRedirectURI, challenge, verifier), "Complete login in your browser, then copy the code Anthropic shows and paste it here.")
	code, err := waitForAuthorizationCode(ctx, callbacks, nil, verifier)
	if err != nil {
		return nil, fmt.Errorf("read code: %w", err)
	}
	return p.exchange(ctx, callbacks, code, verifier, anthropicCopyCodeRedirectURI)
}

func (p *anthropicProvider) exchange(ctx context.Context, callbacks LoginCallbacks, code, verifier, redirectURI string) (*Credentials, error) {
	callbacks.OnProgress("Exchanging authorization code for tokens...")
	creds, err := exchangeCodeJSON(ctx, p.tokenURL, map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     p.clientID,
		"code":          code,
		"state":         verifier,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	return creds, nil
}

func (p *anthropicProvider) RefreshToken(ctx context.Context, credentials *Credentials) (*Credentials, error) {
	return exchangeCodeJSON(ctx, p.tokenURL, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     p.clientID,
		"refresh_token": credentials.Refresh,
	})
}

func (p *anthropicProvider) GetAPIKey(credentials *Credentials) string {
	return credentials.Access
}
