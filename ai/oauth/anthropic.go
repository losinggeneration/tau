package oauth

import (
	"context"
	"fmt"

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

const anthropicClientID = "9d1c250a-e61b-44d9-889e-5944d1962f5e"

func NewAnthropicProvider() Provider {
	return &anthropicProvider{
		clientID:     anthropicClientID,
		authURL:      "https://claude.ai/oauth/authorize",
		tokenURL:     "https://platform.claude.com/v1/oauth/token",
		callbackPort: 53692,
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

func (p *anthropicProvider) Login(ctx context.Context, callbacks LoginCallbacks) (creds *Credentials, err error) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}

	server := newCallbackServer(verifier, p.callbackPort)
	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}
	defer func() {
		if stopErr := server.Stop(); err == nil && stopErr != nil {
			err = stopErr
		}
	}()

	redirectURI := server.URL()

	authParams := map[string]string{
		"code":                  "true",
		"client_id":             p.clientID,
		"response_type":         "code",
		"redirect_uri":          redirectURI,
		"scope":                 p.scopes,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"state":                 verifier,
	}

	authURL := BuildAuthURL(p.authURL, authParams)

	callbacks.OnAuth(authURL, "Complete login in your browser. If the browser is on another machine, paste the final redirect URL here.")

	code, _, err := server.WaitForCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("wait for code: %w", err)
	}

	callbacks.OnProgress("Exchanging authorization code for tokens...")

	creds, err = ExchangeCode(ctx, p.tokenURL, map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     p.clientID,
		"code":          code,
		"state":         verifier,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
	_ = server.Stop()
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}

	return creds, nil
}

func (p *anthropicProvider) RefreshToken(ctx context.Context, credentials *Credentials) (*Credentials, error) {
	return RefreshTokens(ctx, p.tokenURL, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     p.clientID,
		"refresh_token": credentials.Refresh,
	})
}

func (p *anthropicProvider) GetAPIKey(credentials *Credentials) string {
	return credentials.Access
}
