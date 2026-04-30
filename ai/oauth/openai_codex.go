package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	airoot "github.com/losinggeneration/tau/ai"
	"golang.org/x/oauth2"
)

// OpenAI Codex OAuth provider.
type openaiCodexProvider struct {
	clientID     string
	authURL      string
	tokenURL     string
	callbackPort int
	originator   string
}

func NewOpenAICodexProvider(originator string) Provider {
	if originator == "" {
		originator = "pi"
	}
	return &openaiCodexProvider{
		clientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
		authURL:      "https://auth.openai.com/oauth/authorize",
		tokenURL:     "https://auth.openai.com/oauth/token",
		callbackPort: 1455,
		originator:   originator,
	}
}

func (p *openaiCodexProvider) ID() ProviderID {
	return ProviderOpenAICodex
}

func (p *openaiCodexProvider) Name() string {
	return "ChatGPT Plus/Pro (Codex Subscription)"
}

func (p *openaiCodexProvider) UsesCallbackServer() bool {
	return true
}

func (p *openaiCodexProvider) ModifyModels(models []*airoot.Model, credentials *Credentials) []*airoot.Model {
	return models
}

// decodeJWT decodes a JWT token and returns the payload as a map.
// Returns nil if the token is invalid.
func decodeJWT(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}

	// Decode the payload (second part)
	payload := parts[1]

	// Add padding if necessary
	padding := len(payload) % 4
	if padding > 0 {
		padding = 4 - padding
		payload += strings.Repeat("=", padding)
	}

	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		// Try URL-safe base64
		decoded, err = base64.URLEncoding.DecodeString(payload)
		if err != nil {
			return nil
		}
	}

	var result map[string]any
	if err := json.Unmarshal(decoded, &result); err != nil {
		return nil
	}

	return result
}

// getAccountID extracts the chatgpt_account_id from an access token.
// The claim is at: https://api.openai.com/auth -> chatgpt_account_id
func getAccountID(accessToken string) (string, bool) {
	payload := decodeJWT(accessToken)
	if payload == nil {
		return "", false
	}

	// Navigate to the auth claim: https://api.openai.com/auth -> chatgpt_account_id
	authClaim, ok := payload["https://api.openai.com/auth"].(map[string]any)
	if !ok {
		return "", false
	}

	accountID, ok := authClaim["chatgpt_account_id"].(string)
	if !ok || accountID == "" {
		return "", false
	}

	return accountID, true
}

func (p *openaiCodexProvider) oauthConfig(redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:    p.clientID,
		RedirectURL: redirectURI,
		Scopes:      []string{"openid", "profile", "email", "offline_access"},
		Endpoint: oauth2.Endpoint{
			AuthURL:   p.authURL,
			TokenURL:  p.tokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
}

func openAICodexCredentialsFromOAuth2Token(tok *oauth2.Token, previousRefresh string) (*Credentials, error) {
	if tok == nil {
		return nil, fmt.Errorf("token response missing token")
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}

	refresh := tok.RefreshToken
	if refresh == "" {
		refresh = previousRefresh
	}
	if refresh == "" {
		return nil, fmt.Errorf("token response missing refresh_token")
	}
	if tok.Expiry.IsZero() {
		return nil, fmt.Errorf("token response missing expires_in")
	}

	accountID, ok := getAccountID(tok.AccessToken)
	if !ok {
		return nil, fmt.Errorf("failed to extract accountId from token")
	}

	extra := map[string]any{}
	if tokenType := tok.Type(); tokenType != "" {
		extra["token_type"] = tokenType
	}
	if scope := tok.Extra("scope"); scope != nil {
		extra["scope"] = scope
	}
	if idToken := tok.Extra("id_token"); idToken != nil {
		extra["id_token"] = idToken
	}

	return &Credentials{
		Access:    tok.AccessToken,
		Refresh:   refresh,
		Expires:   tok.Expiry.UnixMilli(),
		AccountID: accountID,
		Extra:     extra,
	}, nil
}

func (p *openaiCodexProvider) Login(ctx context.Context, callbacks LoginCallbacks) (creds *Credentials, err error) {
	verifier := oauth2.GenerateVerifier()
	state := oauth2.GenerateVerifier()

	server := newCallbackServer(state, p.callbackPort)
	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}
	defer func() {
		if stopErr := server.Stop(); err == nil && stopErr != nil {
			err = stopErr
		}
	}()

	redirectURI := server.AuthCallbackURL()
	config := p.oauthConfig(redirectURI)
	authURL := config.AuthCodeURL(
		state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("id_token_add_organizations", "true"),
		oauth2.SetAuthURLParam("codex_cli_simplified_flow", "true"),
		oauth2.SetAuthURLParam("originator", p.originator),
	)

	callbacks.OnAuth(authURL, "A browser window should open. Complete login to finish.")

	// Wait for callback or timeout
	code, _, waitErr := server.WaitForCode(ctx)
	if waitErr != nil {
		return nil, fmt.Errorf("wait for code: %w", waitErr)
	}

	if code == "" {
		return nil, fmt.Errorf("no authorization code received")
	}

	callbacks.OnProgress("Exchanging authorization code for tokens...")

	tok, err := config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}

	return openAICodexCredentialsFromOAuth2Token(tok, "")
}

func (p *openaiCodexProvider) RefreshToken(ctx context.Context, credentials *Credentials) (*Credentials, error) {
	if credentials == nil || credentials.Refresh == "" {
		return nil, fmt.Errorf("missing refresh token")
	}

	config := p.oauthConfig("")
	tok, err := config.TokenSource(ctx, &oauth2.Token{
		RefreshToken: credentials.Refresh,
		Expiry:       time.Now().Add(-time.Second),
	}).Token()
	if err != nil {
		return nil, fmt.Errorf("refresh token: %w", err)
	}

	return openAICodexCredentialsFromOAuth2Token(tok, credentials.Refresh)
}

func (p *openaiCodexProvider) GetAPIKey(credentials *Credentials) string {
	return credentials.Access
}
