package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	airoot "github.com/losinggeneration/tau/ai"
	"golang.org/x/oauth2"
)

// ProviderRadius is the Radius gateway OAuth provider.
const ProviderRadius ProviderID = "radius"

const (
	radiusCallbackPort = 1456
	radiusExpirySkew   = time.Minute
)

// RadiusOAuthProvider signs in to the hosted Radius gateway.
var RadiusOAuthProvider = NewRadiusProvider(airoot.DefaultRadiusGateway)

type radiusProvider struct {
	gateway string
}

// NewRadiusProvider returns the OAuth provider for a Radius gateway. The
// gateway serves the token and device endpoints; only the browser
// authorization endpoint is discovered.
func NewRadiusProvider(gateway string) Provider {
	return &radiusProvider{gateway: airoot.RadiusGateway(gateway)}
}

func (p *radiusProvider) ID() ProviderID           { return ProviderRadius }
func (p *radiusProvider) Name() string             { return "Radius" }
func (p *radiusProvider) UsesCallbackServer() bool { return true }

func (p *radiusProvider) ModifyModels(models []*airoot.Model, _ *Credentials) []*airoot.Model {
	return models
}

func (p *radiusProvider) GetAPIKey(credentials *Credentials) string {
	return credentials.Access
}

func (p *radiusProvider) config(authURL, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:    "pi-gateway",
		RedirectURL: redirectURL,
		Scopes:      []string{"gateway", "offline_access"},
		Endpoint: oauth2.Endpoint{
			AuthURL:       authURL,
			TokenURL:      p.gateway + "/v1/oauth/token",
			DeviceAuthURL: p.gateway + "/v1/oauth/device",
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}
}

func (p *radiusProvider) Login(ctx context.Context, callbacks LoginCallbacks) (*Credentials, error) {
	method, err := callbacks.OnPrompt(ctx, "Sign in to Radius with browser (recommended) or device code (when signing in from another device) [browser/device-code]: ", "browser")
	if err != nil {
		return nil, fmt.Errorf("radius sign-in method: %w", err)
	}
	var tok *oauth2.Token
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "", "browser", "1":
		tok, err = p.loginWithBrowser(ctx, callbacks)
	case "device-code", "device", "2":
		tok, err = p.loginWithDeviceCode(ctx, callbacks)
	default:
		return nil, fmt.Errorf("unknown Radius sign-in method: %s", method)
	}
	if err != nil {
		return nil, err
	}
	return radiusCredentials(tok, "")
}

func (p *radiusProvider) loginWithBrowser(ctx context.Context, callbacks LoginCallbacks) (tok *oauth2.Token, err error) {
	authURL, err := p.discoverAuthorizationEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()
	state := oauth2.GenerateVerifier()
	server := newCallbackServer(state, radiusCallbackPort)
	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}
	defer func() { err = errors.Join(err, server.Stop()) }()
	redirectURL := fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", server.Port())
	config := p.config(authURL, redirectURL)
	callbacks.OnProgress("Listening for OAuth callback on " + redirectURL)
	callbacks.OnAuth(config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("handoff", "url")), "Continue in your browser.")
	code, _, err := server.WaitForCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("wait for Radius authorization: %w", err)
	}
	tok, err = config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("radius OAuth token request failed: %w", err)
	}
	return tok, nil
}

func (p *radiusProvider) loginWithDeviceCode(ctx context.Context, callbacks LoginCallbacks) (*oauth2.Token, error) {
	config := p.config("", "")
	device, err := config.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("radius OAuth device authorization failed: %w", err)
	}
	callbacks.OnAuth(device.VerificationURI, "Enter code "+device.UserCode)
	tok, err := config.DeviceAccessToken(ctx, device)
	if err != nil {
		return nil, fmt.Errorf("radius device authorization: %w", err)
	}
	return tok, nil
}

func (p *radiusProvider) discoverAuthorizationEndpoint(ctx context.Context) (endpoint string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.gateway+"/v1/oauth", nil)
	if err != nil {
		return "", fmt.Errorf("radius OAuth config request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("load Radius OAuth config from %s: %w", p.gateway, err)
	}
	defer func() { err = errors.Join(err, closeBody(resp)) }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("load Radius OAuth config from %s: status %d", p.gateway, resp.StatusCode)
	}
	var discovery struct {
		AuthorizationEndpoint string `json:"authorizationEndpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&discovery); err != nil || discovery.AuthorizationEndpoint == "" {
		return "", errors.Join(fmt.Errorf("invalid Radius OAuth config from %s", p.gateway), err)
	}
	return discovery.AuthorizationEndpoint, nil
}

func (p *radiusProvider) RefreshToken(ctx context.Context, credentials *Credentials) (*Credentials, error) {
	if credentials == nil || credentials.Refresh == "" {
		return nil, fmt.Errorf("missing refresh token")
	}
	tok, err := p.config("", "").TokenSource(ctx, &oauth2.Token{RefreshToken: credentials.Refresh, Expiry: time.Now().Add(-time.Second)}).Token()
	if err != nil {
		return nil, fmt.Errorf("refresh Radius token: %w", err)
	}
	return radiusCredentials(tok, credentials.Refresh)
}

func radiusCredentials(tok *oauth2.Token, previousRefresh string) (*Credentials, error) {
	if tok == nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("radius token response missing access_token")
	}
	refresh := tok.RefreshToken
	if refresh == "" {
		refresh = previousRefresh
	}
	extra := map[string]any{}
	if scope := tok.Extra("scope"); scope != nil {
		extra["scope"] = scope
	}
	return &Credentials{Access: tok.AccessToken, Refresh: refresh, Expires: tok.Expiry.Add(-radiusExpirySkew).UnixMilli(), Extra: extra}, nil
}
