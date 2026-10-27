package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

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
	deviceID     func(context.Context) (string, error)
}

// OpenAICodexOption configures OpenAI sign-in.
type OpenAICodexOption func(*openaiCodexProvider)

// WithOpenAIDeviceID resolves the installation ID only when sign-in needs it.
func WithOpenAIDeviceID(resolve func(context.Context) (string, error)) OpenAICodexOption {
	return func(p *openaiCodexProvider) { p.deviceID = resolve }
}

func NewOpenAICodexProvider(originator string, opts ...OpenAICodexOption) Provider {
	if originator == "" {
		originator = "pi"
	}
	provider := &openaiCodexProvider{
		clientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
		authURL:      "https://auth.openai.com/oauth/authorize",
		tokenURL:     "https://auth.openai.com/oauth/token",
		callbackPort: 1455,
		originator:   originator,
	}
	for _, opt := range opts {
		opt(provider)
	}
	return provider
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

	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	var hostID string
	if p.deviceID != nil {
		id, err := p.deviceID(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve device ID: %w", err)
		}
		parsed, err := uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("invalid device ID: %w", err)
		}
		if len(id) != 36 {
			return nil, fmt.Errorf("invalid device ID: expected canonical UUID")
		}
		hostID = "urn:uuid:" + parsed.String()
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
	}
	server := newCallbackServer(state, p.callbackPort)
	listenErr := server.startFixed(ctx)
	defer func() { err = errors.Join(err, server.Stop()) }()

	redirectURI := server.AuthCallbackURL()
	config := p.oauthConfig(redirectURI)
	authOptions := []oauth2.AuthCodeOption{
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("id_token_add_organizations", "true"),
		oauth2.SetAuthURLParam("codex_cli_simplified_flow", "true"),
		oauth2.SetAuthURLParam("originator", p.originator),
	}
	if hostID != "" {
		authOptions = append(authOptions, oauth2.SetAuthURLParam("ext_agent_host_id", hostID))
	}
	authURL := config.AuthCodeURL(state, authOptions...)

	callbacks.OnAuth(authURL, "A browser window should open. Complete login to finish.")
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	listening := server
	if listenErr != nil {
		listening = nil
		callbacks.OnProgress("Callback port unavailable. Paste the authorization code or redirect URL to finish sign-in.")
	}
	code, err := waitForAuthorizationCode(ctx, callbacks, listening, state)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Join(ctxErr, err)
	}
	if err != nil {
		return nil, fmt.Errorf("wait for code: %w", err)
	}

	callbacks.OnProgress("Exchanging authorization code for tokens...")

	tok, err := config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}

	return openAICodexCredentialsFromOAuth2Token(tok, "")
}

// parseAuthorizationInput reads a pasted redirect URL, query string,
// code#state, or bare code.
func parseAuthorizationInput(input, expectedState string) (string, error) {
	value := strings.TrimSpace(input)
	var code, state string
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		code, state = parsed.Query().Get("code"), parsed.Query().Get("state")
	} else if before, after, ok := strings.Cut(value, "#"); ok {
		code, state = before, after
	} else if strings.Contains(value, "code=") {
		query, err := url.ParseQuery(value)
		if err != nil {
			return "", fmt.Errorf("parse authorization input: %w", err)
		}
		code, state = query.Get("code"), query.Get("state")
	} else {
		code = value
	}
	if state != "" && state != expectedState {
		return "", errors.New("state mismatch")
	}
	return code, nil
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
