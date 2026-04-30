package oauth

import (
	"context"
	"fmt"

	airoot "github.com/losinggeneration/tau/ai"
)

// Google Antigravity OAuth provider for free Gemini 3, Claude, GPT-OSS via Google Cloud.
type antigravityProvider struct {
	clientID     string
	authURL      string
	tokenURL     string
	callbackPort int
}

func NewAntigravityProvider() Provider {
	return &antigravityProvider{
		clientID:     "686登山56a0o50cfb1o99t56aepqpbkf2q.apps.googleusercontent.com",
		authURL:      "https://accounts.google.com/o/oauth2/v2/auth",
		tokenURL:     "https://oauth2.googleapis.com/token",
		callbackPort: 53696,
	}
}

func (p *antigravityProvider) ID() ProviderID {
	return ProviderGoogleAntigravity
}

func (p *antigravityProvider) Name() string {
	return "Google Antigravity (Free Gemini/Claude/GPT-OSS)"
}

func (p *antigravityProvider) UsesCallbackServer() bool {
	return true
}

func (p *antigravityProvider) ModifyModels(models []*airoot.Model, credentials *Credentials) []*airoot.Model {
	return models
}

func (p *antigravityProvider) Login(ctx context.Context, callbacks LoginCallbacks) (creds *Credentials, err error) {
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
		"client_id":             p.clientID,
		"response_type":         "code",
		"redirect_uri":          redirectURI,
		"scope":                 "openid email https://www.googleapis.com/auth/cloud-platform",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"state":                 verifier,
		"access_type":           "offline",
		"prompt":                "consent",
	}

	authURL := BuildAuthURL(p.authURL, authParams)

	callbacks.OnAuth(authURL, "Complete login in your browser. Paste the final redirect URL here if authenticating on another machine.")

	code, _, err := server.WaitForCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("wait for code: %w", err)
	}

	callbacks.OnProgress("Exchanging authorization code for tokens...")

	creds, err = ExchangeCode(ctx, p.tokenURL, map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     p.clientID,
		"code":          code,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}

	return creds, nil
}

func (p *antigravityProvider) RefreshToken(ctx context.Context, credentials *Credentials) (*Credentials, error) {
	if credentials == nil {
		return nil, fmt.Errorf("missing credentials")
	}

	refreshed, err := RefreshTokens(ctx, p.tokenURL, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     p.clientID,
		"refresh_token": credentials.Refresh,
	})
	if err != nil {
		return nil, err
	}
	refreshed.ProjectID = credentials.ProjectID
	if refreshed.Email == "" {
		refreshed.Email = credentials.Email
	}
	return refreshed, nil
}

func (p *antigravityProvider) GetAPIKey(credentials *Credentials) string {
	return credentials.Access
}
