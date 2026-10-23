package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	airoot "github.com/losinggeneration/tau/ai"
	"golang.org/x/oauth2"
)

// ProviderOpenRouter is the OpenRouter OAuth provider.
const ProviderOpenRouter ProviderID = "openrouter"

const (
	openRouterLoginTimeout    = 5 * time.Minute
	openRouterExchangeTimeout = 30 * time.Second
	// OpenRouter keys never expire; this matches upstream's Number.MAX_SAFE_INTEGER.
	openRouterKeyExpiry = 1<<53 - 1
)

// OpenRouterOAuthProvider exchanges an OpenRouter PKCE sign-in for a
// permanent, user-controlled API key.
var OpenRouterOAuthProvider Provider = newOpenRouterProvider("https://openrouter.ai/auth", "https://openrouter.ai/api/v1/auth/keys")

type openRouterProvider struct {
	authURL  string
	tokenURL string
}

func newOpenRouterProvider(authURL, tokenURL string) *openRouterProvider {
	return &openRouterProvider{authURL: authURL, tokenURL: tokenURL}
}

func (p *openRouterProvider) ID() ProviderID           { return ProviderOpenRouter }
func (p *openRouterProvider) Name() string             { return "OpenRouter" }
func (p *openRouterProvider) UsesCallbackServer() bool { return true }

func (p *openRouterProvider) ModifyModels(models []*airoot.Model, _ *Credentials) []*airoot.Model {
	return models
}

func (p *openRouterProvider) GetAPIKey(credentials *Credentials) string { return credentials.Access }

func (p *openRouterProvider) RefreshToken(_ context.Context, credentials *Credentials) (*Credentials, error) {
	if credentials == nil || credentials.Access == "" {
		return nil, errors.New("missing OpenRouter API key")
	}
	return credentials, nil
}

type openRouterResult struct {
	creds *Credentials
	err   error
}

func (p *openRouterProvider) Login(ctx context.Context, callbacks LoginCallbacks) (creds *Credentials, err error) {
	ctx, cancel := context.WithTimeoutCause(ctx, openRouterLoginTimeout, errors.New("OpenRouter sign-in timed out"))
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("openrouter sign-in: %w", err)
	}

	verifier := oauth2.GenerateVerifier()
	host := callbackHost()
	// OpenRouter sends no state; the random path keeps stray requests from completing the sign-in.
	path := "/oauth/callback/" + rand.Text()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, fmt.Errorf("start OpenRouter callback server: %w", err)
	}

	// Callback and manual input each send at most once.
	results := make(chan openRouterResult, 2)
	var claimed atomic.Bool
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != path {
			writeOAuthPage(w, http.StatusNotFound, OAuthErrorHTML("Callback route not found.", ""))
			return
		}
		query := r.URL.Query()
		code, oauthErr := query.Get("code"), query.Get("error")
		if code == "" && oauthErr == "" {
			writeOAuthPage(w, http.StatusBadRequest, OAuthErrorHTML("Missing authorization code.", ""))
			return
		}
		if !claimed.CompareAndSwap(false, true) {
			writeOAuthPage(w, http.StatusConflict, OAuthErrorHTML("This sign-in has already been handled.", ""))
			return
		}
		if oauthErr != "" {
			description := query.Get("error_description")
			if description == "" {
				description = oauthErr
			}
			writeOAuthPage(w, http.StatusBadRequest, OAuthErrorHTML("OpenRouter authorization failed.", description))
			results <- openRouterResult{err: fmt.Errorf("OpenRouter authorization failed: %s", description)}
			return
		}
		creds, err := p.exchange(r.Context(), code, verifier)
		if err != nil {
			writeOAuthPage(w, http.StatusBadGateway, OAuthErrorHTML("OpenRouter sign-in failed.", err.Error()))
		} else {
			writeOAuthPage(w, http.StatusOK, OAuthSuccessHTML("Signed in to OpenRouter. You may now close this page."))
		}
		results <- openRouterResult{creds: creds, err: err}
	})}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			results <- openRouterResult{err: fmt.Errorf("OpenRouter callback server: %w", err)}
		}
	}()
	defer func() { err = errors.Join(err, server.Close()) }()

	redirect := (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)), Path: path}).String()
	authURL, err := url.Parse(p.authURL)
	if err != nil {
		return nil, fmt.Errorf("openrouter authorize URL: %w", err)
	}
	authURL.RawQuery = url.Values{
		"callback_url":          {redirect},
		"code_challenge":        {oauth2.S256ChallengeFromVerifier(verifier)},
		"code_challenge_method": {"S256"},
	}.Encode()
	callbacks.OnProgress("Listening for OpenRouter OAuth callback on " + redirect)
	callbacks.OnAuth(authURL.String(), "Complete sign-in in your browser. If the browser is on another machine, paste the final redirect URL here.")

	var result openRouterResult
	callback, input, err := waitForCallbackOrManualInput(ctx, callbacks, func(ctx context.Context) (string, error) {
		select {
		case result = <-results:
			return "callback", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	if ctx.Err() != nil {
		return nil, fmt.Errorf("openrouter sign-in: %w", errors.Join(ctx.Err(), context.Cause(ctx)))
	}
	if err != nil {
		return nil, err
	}
	if callback != "" {
		return result.creds, result.err
	}
	code := parseOpenRouterAuthorizationInput(input)
	if code == "" {
		return nil, errors.New("missing authorization code")
	}
	if !claimed.CompareAndSwap(false, true) {
		select {
		case result = <-results:
			return result.creds, result.err
		case <-ctx.Done():
			return nil, fmt.Errorf("openrouter sign-in: %w", errors.Join(ctx.Err(), context.Cause(ctx)))
		}
	}
	callbacks.OnProgress("Exchanging authorization code for an API key...")
	return p.exchange(ctx, code, verifier)
}

func parseOpenRouterAuthorizationInput(input string) string {
	value := strings.TrimSpace(input)
	if value == "" {
		return ""
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.Query().Get("code")
	}
	if strings.Contains(value, "code=") {
		values, err := url.ParseQuery(value)
		if err != nil {
			return ""
		}
		return values.Get("code")
	}
	return value
}

func (p *openRouterProvider) exchange(ctx context.Context, code, verifier string) (creds *Credentials, err error) {
	ctx, cancel := context.WithTimeoutCause(ctx, openRouterExchangeTimeout, errors.New("OpenRouter OAuth token exchange timed out"))
	defer cancel()
	payload, err := json.Marshal(map[string]string{"code": code, "code_verifier": verifier, "code_challenge_method": "S256"})
	if err != nil {
		return nil, fmt.Errorf("encode OpenRouter key exchange: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openrouter key exchange request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter key exchange: %w", errors.Join(err, context.Cause(ctx)))
	}
	defer func() { err = errors.Join(err, closeBody(resp)) }()

	var body map[string]any
	decodeErr := json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := fmt.Sprintf("OpenRouter OAuth key exchange failed (HTTP %d)", resp.StatusCode)
		if detail := openRouterErrorDetail(body); detail != "" {
			message += ": " + detail
		}
		return nil, errors.New(message)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("OpenRouter OAuth returned invalid JSON: %w", decodeErr)
	}
	key, _ := body["key"].(string)
	if key == "" {
		return nil, errors.New(`OpenRouter OAuth response carries no "key"`)
	}
	return &Credentials{Access: key, Expires: openRouterKeyExpiry}, nil
}

func openRouterErrorDetail(body map[string]any) string {
	for _, field := range []string{"error_description", "message", "error"} {
		if text, ok := body[field].(string); ok {
			return text
		}
	}
	if nested, ok := body["error"].(map[string]any); ok {
		if text, ok := nested["message"].(string); ok {
			return text
		}
	}
	return ""
}

func writeOAuthPage(w http.ResponseWriter, status int, page string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	// The browser closing the page early is not a sign-in failure.
	_, _ = w.Write([]byte(page))
}
