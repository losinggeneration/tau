// Package oauth provides OAuth credential management for AI providers.
//
// Supported providers:
//   - Anthropic (Claude Pro/Max)
//   - GitHub Copilot
//   - Google Gemini CLI
//   - OpenAI Codex
//   - Google Antigravity
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	airoot "github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/logging"
	"golang.org/x/oauth2"
)

// closeBody is a helper to close an HTTP response body.
// If closing fails and there's no existing error, the close error is returned.
// If there's an existing error, the close error is logged.
func closeBody(resp *http.Response) error {
	closeErr := resp.Body.Close()
	if closeErr != nil {
		return closeErr
	}
	return nil
}

// ProviderID identifies an OAuth provider.
type ProviderID string

const (
	ProviderAnthropic         ProviderID = "anthropic"
	ProviderGitHubCopilot     ProviderID = "github-copilot"
	ProviderGeminiCLI         ProviderID = "google-gemini-cli"
	ProviderOpenAICodex       ProviderID = "openai-codex"
	ProviderGoogleAntigravity ProviderID = "google-antigravity"
	ProviderAntigravity       ProviderID = ProviderGoogleAntigravity
)

// Credentials represents OAuth credentials with tokens and expiry.
// Credentials represents OAuth credentials with tokens and expiry.
type Credentials struct {
	Refresh   string         `json:"refresh"`             // Refresh token
	Access    string         `json:"access"`              // Access token
	Expires   int64          `json:"expires"`             // Unix timestamp when access token expires (milliseconds)
	ProjectID string         `json:"projectId,omitempty"` // Google Cloud Code Assist project identifier
	Email     string         `json:"email,omitempty"`     // Optional user email for OAuth-backed providers
	AccountID string         `json:"accountId,omitempty"` // Provider-specific account identifier (e.g., OpenAI Codex)
	Extra     map[string]any `json:"extra,omitempty"`     // Additional provider-specific data (e.g., enterpriseUrl for GitHub Copilot)
}

// IsExpired returns true if the access token is expired or will expire within the given buffer.
func (c *Credentials) IsExpired(buffer time.Duration) bool {
	return time.Now().Add(buffer).UnixMilli() >= c.Expires
}

// LoginCallbacks provides callbacks for interactive OAuth flows.
type LoginCallbacks interface {
	OnAuth(url string, instructions string)
	OnPrompt(message string, placeholder string) (string, error)
	OnProgress(message string)
	OnManualCodeInput() (string, error)
}

// DefaultLoginCallbacks provides console-based implementations of LoginCallbacks.
type DefaultLoginCallbacks struct {
	FnAuth            func(url string, instructions string)
	FnPrompt          func(message string, placeholder string) (string, error)
	FnProgress        func(message string)
	FnManualCodeInput func() (string, error)
}

func (c *DefaultLoginCallbacks) OnAuth(url string, instructions string) {
	if c.FnAuth != nil {
		c.FnAuth(url, instructions)
	} else {
		fmt.Printf("Visit this URL to authenticate:\n%s\n%s\n\n", url, instructions)
	}
}

func (c *DefaultLoginCallbacks) OnPrompt(message string, placeholder string) (string, error) {
	if c.FnPrompt != nil {
		return c.FnPrompt(message, placeholder)
	}

	fmt.Print(message)

	var input string
	_, _ = fmt.Scanln(&input)

	return input, nil
}

func (c *DefaultLoginCallbacks) OnProgress(message string) {
	if c.FnProgress != nil {
		c.FnProgress(message)
	} else {
		fmt.Println(message)
	}
}

func (c *DefaultLoginCallbacks) OnManualCodeInput() (string, error) {
	if c.FnManualCodeInput != nil {
		return c.FnManualCodeInput()
	}

	fmt.Print("Enter authorization code or redirect URL: ")

	var input string
	_, _ = fmt.Scanln(&input)

	return input, nil
}

// ProviderInfo represents information about an OAuth provider.
type ProviderInfo struct {
	ID        ProviderID
	Name      string
	Available bool
}

// Provider represents an OAuth provider interface.
type Provider interface {
	ID() ProviderID
	Name() string
	UsesCallbackServer() bool
	Login(ctx context.Context, callbacks LoginCallbacks) (*Credentials, error)
	RefreshToken(ctx context.Context, credentials *Credentials) (*Credentials, error)
	GetAPIKey(credentials *Credentials) string
	ModifyModels(models []*airoot.Model, credentials *Credentials) []*airoot.Model
}

// registry is the global provider registry.
var (
	registry         = make(map[ProviderID]Provider)
	registryOrder    []ProviderID
	registerMu       sync.Once
	registryInitOnce sync.Once
)

const providerGeminiCLILegacy ProviderID = "gemini-cli"

// builtInProviders returns the list of built-in OAuth providers.
func builtInProviders() []Provider {
	return []Provider{
		AnthropicOAuthProvider,
		GitHubCopilotOAuthProvider,
		GeminiCLIOAuthProvider,
		AntigravityOAuthProvider,
		OpenAICodexOAuthProvider,
	}
}

func registerProviders() {
	registryOrder = registryOrder[:0]
	for _, p := range builtInProviders() {
		id := canonicalProviderID(p.ID())
		registry[id] = p
		registryOrder = append(registryOrder, id)
	}
}

// Register registers an OAuth provider.
func Register(provider Provider) {
	id := canonicalProviderID(provider.ID())
	if _, exists := registry[id]; !exists {
		registryOrder = append(registryOrder, id)
	}
	registry[id] = provider
}

// RegisterOAuthProvider is a TypeScript-parity alias for Register.
func RegisterOAuthProvider(provider Provider) {
	Register(provider)
}

// GetProvider returns a registered provider by ID.
func GetProvider(id ProviderID) (Provider, bool) {
	registerMu.Do(registerProviders)
	id = canonicalProviderID(id)

	p, ok := registry[id]

	return p, ok
}

// GetOAuthProvider is a TypeScript-parity alias for GetProvider.
func GetOAuthProvider(id ProviderID) (Provider, bool) {
	return GetProvider(id)
}

// ListProviders returns all registered providers.
func ListProviders() []Provider {
	registerMu.Do(registerProviders)

	providers := make([]Provider, 0, len(registryOrder))
	for _, id := range registryOrder {
		p, ok := registry[id]
		if !ok {
			continue
		}
		providers = append(providers, p)
	}

	return providers
}

// GetOAuthProviders is a TypeScript-parity alias for ListProviders.
func GetOAuthProviders() []Provider {
	return ListProviders()
}

// GetProviderInfoList returns information about all registered OAuth providers.
// This is the Go equivalent of pi-mono's getOAuthProviderInfoList().
func GetProviderInfoList() []ProviderInfo {
	providers := ListProviders()
	infos := make([]ProviderInfo, len(providers))
	for i, p := range providers {
		infos[i] = ProviderInfo{
			ID:        p.ID(),
			Name:      p.Name(),
			Available: true,
		}
	}
	return infos
}

// Unregister removes a provider from the registry.
// If the provider is built-in, restores the built-in implementation.
// Custom providers are removed completely.
func Unregister(id ProviderID) {
	registerMu.Do(registerProviders)
	id = canonicalProviderID(id)

	// Check if this is a built-in provider
	builtIn := false
	for _, p := range builtInProviders() {
		if canonicalProviderID(p.ID()) == id {
			builtIn = true
			break
		}
	}

	if builtIn {
		// Restore the built-in implementation
		for _, p := range builtInProviders() {
			if canonicalProviderID(p.ID()) == id {
				registry[id] = p
				return
			}
		}
	}

	// Remove custom provider
	delete(registry, id)
	registryOrder = removeProviderID(registryOrder, id)
}

// UnregisterOAuthProvider is a TypeScript-parity alias for Unregister.
func UnregisterOAuthProvider(id ProviderID) {
	Unregister(id)
}

// ResetProviders resets the OAuth provider registry to built-in providers only.
// This removes all custom providers and restores built-ins.
func ResetProviders() {
	registryInitOnce.Do(func() {
		registerMu.Do(registerProviders)
	})

	// Clear and reinitialize
	registry = make(map[ProviderID]Provider)
	registryOrder = nil
	for _, p := range builtInProviders() {
		id := canonicalProviderID(p.ID())
		registry[id] = p
		registryOrder = append(registryOrder, id)
	}
}

// ResetOAuthProviders is a TypeScript-parity alias for ResetProviders.
func ResetOAuthProviders() {
	ResetProviders()
}

// GeneratePKCE generates a PKCE code verifier and S256 challenge.
//
// The verifier/challenge derivation is delegated to golang.org/x/oauth2 so this
// package does not carry security-sensitive OAuth/PKCE primitives itself.
func GeneratePKCE() (verifier, challenge string, err error) {
	verifier = oauth2.GenerateVerifier()
	challenge = oauth2.S256ChallengeFromVerifier(verifier)
	return verifier, challenge, nil
}

func canonicalProviderID(id ProviderID) ProviderID {
	switch id {
	case providerGeminiCLILegacy:
		return ProviderGeminiCLI
	default:
		return id
	}
}

func removeProviderID(ids []ProviderID, target ProviderID) []ProviderID {
	for i, id := range ids {
		if id != target {
			continue
		}
		return append(ids[:i], ids[i+1:]...)
	}
	return ids
}

func escapeHTML(value string) string {
	return html.EscapeString(value)
}

func renderOAuthPage(title, heading, message, details string) string {
	var detailsHTML string
	if details != "" {
		detailsHTML = `<div class="details">` + escapeHTML(details) + `</div>`
	}

	return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>` + escapeHTML(title) + `</title>
  <style>
    :root {
      --text: #fafafa;
      --text-dim: #a1a1aa;
      --page-bg: #09090b;
      --font-sans: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, "Noto Sans", sans-serif, "Apple Color Emoji", "Segoe UI Emoji", "Segoe UI Symbol", "Noto Color Emoji";
      --font-mono: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace;
    }
    * { box-sizing: border-box; }
    html { color-scheme: dark; }
    body {
      margin: 0;
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 24px;
      background: var(--page-bg);
      color: var(--text);
      font-family: var(--font-sans);
      text-align: center;
    }
    main {
      width: 100%;
      max-width: 560px;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
    }
    h1 {
      margin: 0 0 10px;
      font-size: 28px;
      line-height: 1.15;
      font-weight: 650;
      color: var(--text);
    }
    p {
      margin: 0;
      line-height: 1.7;
      color: var(--text-dim);
      font-size: 15px;
    }
    .details {
      margin-top: 16px;
      font-family: var(--font-mono);
      font-size: 13px;
      color: var(--text-dim);
      white-space: pre-wrap;
      word-break: break-word;
    }
  </style>
</head>
<body>
  <main>
    <h1>` + escapeHTML(heading) + `</h1>
    <p>` + escapeHTML(message) + `</p>
    ` + detailsHTML + `
  </main>
</body>
</html>`
}

// OAuthSuccessHTML renders the success page for browser-based OAuth flows.
func OAuthSuccessHTML(message string) string {
	return renderOAuthPage("Authentication successful", "Authentication successful", message, "")
}

// OAuthErrorHTML renders the error page for browser-based OAuth flows.
func OAuthErrorHTML(message string, details string) string {
	return renderOAuthPage("Authentication failed", "Authentication failed", message, details)
}

// CallbackServer handles OAuth callback requests.
type CallbackServer struct {
	server   *http.Server
	port     int
	expected string
	code     chan string
	state    chan string
	err      chan error
	ready    chan int
	mu       sync.Mutex
	handled  bool
}

func newCallbackServer(expectedState string, port int) *CallbackServer {
	return &CallbackServer{
		port:     port,
		expected: expectedState,
		code:     make(chan string, 1),
		state:    make(chan string, 1),
		err:      make(chan error, 1),
		ready:    make(chan int, 1),
	}
}

func (s *CallbackServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", s.handleCallback)
	mux.HandleFunc("/auth/callback", s.handleCallback)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		// Try any available port
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("failed to listen: %w", err)
		}
	}

	s.port = listener.Addr().(*net.TCPAddr).Port
	s.ready <- s.port

	s.server = &http.Server{Handler: mux}

	go func() {
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case s.err <- err:
			default:
			}
		}
	}()

	return nil
}

func (s *CallbackServer) Port() int {
	select {
	case p := <-s.ready:
		return p
	default:
		return s.port
	}
}

func (s *CallbackServer) URL() string {
	return fmt.Sprintf("http://localhost:%d/callback", s.port)
}

func (s *CallbackServer) AuthCallbackURL() string {
	return fmt.Sprintf("http://localhost:%d/auth/callback", s.port)
}

func (s *CallbackServer) CancelWait() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.handled {
		close(s.code)
		s.handled = true
	}
}

func (s *CallbackServer) WaitForCode(ctx context.Context) (code, state string, err error) {
	select {
	case code = <-s.code:
		if code == "" {
			return "", "", errors.New("cancelled")
		}

		return code, "", nil
	case err = <-s.err:
		return "", "", err
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

func (s *CallbackServer) Stop() error {
	s.CancelWait()

	if s.server != nil {
		return s.server.Close()
	}

	return nil
}

func (s *CallbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.handled {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(OAuthSuccessHTML("Authentication already handled. You can close this window."))); err != nil {
			logging.Discard().Warn("oauth callback write response failed", "error", err)
		}
		return
	}

	query := r.URL.Query()

	if errMsg := query.Get("error"); errMsg != "" {
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write([]byte(OAuthErrorHTML("Authentication did not complete.", "Error: "+errMsg))); err != nil {
			logging.Discard().Warn("oauth callback write error response failed", "error", err)
		}

		s.err <- fmt.Errorf("oauth error: %s", errMsg)

		s.handled = true

		return
	}

	code := query.Get("code")
	state := query.Get("state")

	if code == "" || state == "" {
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write([]byte(OAuthErrorHTML("Missing code or state parameter.", ""))); err != nil {
			logging.Discard().Warn("oauth callback write missing params response failed", "error", err)
		}

		s.err <- errors.New("missing code or state")

		s.handled = true

		return
	}

	if state != s.expected {
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write([]byte(OAuthErrorHTML("State mismatch.", ""))); err != nil {
			logging.Discard().Warn("oauth callback write state mismatch response failed", "error", err)
		}

		s.err <- errors.New("state mismatch")

		s.handled = true

		return
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(OAuthSuccessHTML("Authentication completed. You can close this window."))); err != nil {
		logging.Discard().Warn("oauth callback write success response failed", "error", err)
	}

	s.code <- code

	s.handled = true
}

// TokenResponse represents an OAuth token response.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

// ExchangeCode exchanges an authorization code for tokens.
func ExchangeCode(ctx context.Context, tokenURL string, params map[string]string) (*Credentials, error) {
	data := url.Values{}
	for k, v := range params {
		data.Set(k, v)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		if closeErr := closeBody(resp); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed (status %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}

	// Subtract 5 minutes buffer from expiry
	expires := time.Now().UnixMilli() + int64(tokenResp.ExpiresIn)*1000 - 5*60*1000

	return &Credentials{
		Access:  tokenResp.AccessToken,
		Refresh: tokenResp.RefreshToken,
		Expires: expires,
		Extra:   make(map[string]any),
	}, nil
}

// CredentialsStore manages OAuth credentials with automatic refresh.
// It provides thread-safe storage and retrieval of credentials for multiple providers.
type CredentialsStore struct {
	creds map[ProviderID]*Credentials
	mu    sync.RWMutex
}

// NewCredentialsStore creates a new credentials store.
func NewCredentialsStore() *CredentialsStore {
	return &CredentialsStore{
		creds: make(map[ProviderID]*Credentials),
	}
}

// Get returns stored credentials for a provider.
func (s *CredentialsStore) Get(providerID ProviderID) (*Credentials, bool) {
	providerID = canonicalProviderID(providerID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	creds, ok := s.creds[providerID]
	return creds, ok
}

// Set stores credentials for a provider.
func (s *CredentialsStore) Set(providerID ProviderID, credentials *Credentials) {
	providerID = canonicalProviderID(providerID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds[providerID] = credentials
}

// Delete removes stored credentials for a provider.
func (s *CredentialsStore) Delete(providerID ProviderID) {
	providerID = canonicalProviderID(providerID)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.creds, providerID)
}

// GetAPIKey retrieves an API key for a provider from the credentials store,
// automatically refreshing expired tokens.
// This matches pi-mono's getOAuthApiKey behavior.
//
// Returns (credentials, apiKey, nil) on success.
// Returns (nil, "", nil) if no credentials are stored for the provider.
// Returns an error if the provider is not registered or refresh fails.
func (s *CredentialsStore) GetAPIKey(ctx context.Context, providerID ProviderID) (*Credentials, string, error) {
	providerID = canonicalProviderID(providerID)
	provider, ok := GetProvider(providerID)
	if !ok {
		return nil, "", fmt.Errorf("unknown OAuth provider: %s", providerID)
	}

	creds, ok := s.Get(providerID)
	if !ok {
		return nil, "", nil
	}

	// Refresh if expired (with 5 minute buffer to avoid edge cases)
	if creds.IsExpired(5 * time.Minute) {
		var refreshErr error
		creds, refreshErr = provider.RefreshToken(ctx, creds)
		if refreshErr != nil {
			return nil, "", fmt.Errorf("failed to refresh OAuth token for %s: %w", providerID, refreshErr)
		}
		// Store updated credentials
		s.Set(providerID, creds)
	}

	apiKey := provider.GetAPIKey(creds)
	return creds, apiKey, nil
}

// RefreshCredentials refreshes credentials for a provider if expired.
// Returns the (possibly updated) credentials.
func (s *CredentialsStore) RefreshCredentials(ctx context.Context, providerID ProviderID) (*Credentials, error) {
	providerID = canonicalProviderID(providerID)
	provider, ok := GetProvider(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown OAuth provider: %s", providerID)
	}

	creds, ok := s.Get(providerID)
	if !ok {
		return nil, fmt.Errorf("no credentials stored for provider: %s", providerID)
	}

	// Refresh if expired (with 5 minute buffer)
	if creds.IsExpired(5 * time.Minute) {
		var refreshErr error
		creds, refreshErr = provider.RefreshToken(ctx, creds)
		if refreshErr != nil {
			return nil, fmt.Errorf("failed to refresh OAuth token for %s: %w", providerID, refreshErr)
		}
		// Store updated credentials
		s.Set(providerID, creds)
	}

	return creds, nil
}

// GetAllProviders returns all stored provider IDs.
func (s *CredentialsStore) GetAllProviders() []ProviderID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]ProviderID, 0, len(s.creds))
	for id := range s.creds {
		result = append(result, id)
	}
	return result
}

// RefreshTokens refreshes OAuth tokens.
// This is a convenience function that calls RefreshToken on a provider.
func RefreshTokens(ctx context.Context, tokenURL string, params map[string]string) (*Credentials, error) {
	return ExchangeCode(ctx, tokenURL, params)
}

// RefreshOAuthToken refreshes credentials for a specific OAuth provider without
// using the package-global credentials store.
func RefreshOAuthToken(ctx context.Context, providerID ProviderID, credentials *Credentials) (*Credentials, error) {
	providerID = canonicalProviderID(providerID)
	provider, ok := GetOAuthProvider(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown OAuth provider: %s", providerID)
	}
	if credentials == nil {
		return nil, fmt.Errorf("missing OAuth credentials for %s", providerID)
	}
	return provider.RefreshToken(ctx, credentials)
}

// BuildAuthURL builds an OAuth authorization URL.
func BuildAuthURL(baseURL string, params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}

	return baseURL + "?" + q.Encode()
}

// getOAuthApiKey is a package-level credentials store for convenience functions.
// Use NewCredentialsStore() for isolated stores.
var defaultStore = NewCredentialsStore()

// GetOAuthAPIKey retrieves an API key for a provider from the default store,
// automatically refreshing expired tokens.
// This matches pi-mono's getOAuthApiKey behavior.
//
// Returns (credentials, apiKey, nil) on success.
// Returns (nil, "", nil) if no credentials are stored for the provider.
// Returns an error if the provider is not registered or refresh fails.
func GetOAuthAPIKey(ctx context.Context, providerID ProviderID) (*Credentials, string, error) {
	return defaultStore.GetAPIKey(ctx, providerID)
}

// ResolveOAuthAPIKey mirrors the TS stateless getOAuthApiKey helper: it uses a
// caller-owned credentials map, automatically refreshes expired tokens, and
// returns the updated credentials plus API key.
func ResolveOAuthAPIKey(ctx context.Context, providerID ProviderID, credentials map[ProviderID]*Credentials) (*Credentials, string, error) {
	originalID := providerID
	providerID = canonicalProviderID(providerID)
	provider, ok := GetOAuthProvider(providerID)
	if !ok {
		return nil, "", fmt.Errorf("unknown OAuth provider: %s", providerID)
	}

	creds := credentials[providerID]
	if creds == nil && originalID != providerID {
		creds = credentials[originalID]
	}
	if creds == nil {
		return nil, "", nil
	}

	if creds.IsExpired(0) {
		refreshed, err := provider.RefreshToken(ctx, creds)
		if err != nil {
			return nil, "", fmt.Errorf("failed to refresh OAuth token for %s: %w", providerID, err)
		}
		creds = refreshed
	}

	return creds, provider.GetAPIKey(creds), nil
}

// SetOAuthCredentials stores credentials for a provider in the default store.
func SetOAuthCredentials(providerID ProviderID, credentials *Credentials) {
	defaultStore.Set(providerID, credentials)
}

// GetOAuthCredentials retrieves stored credentials for a provider from the default store.
func GetOAuthCredentials(providerID ProviderID) (*Credentials, bool) {
	return defaultStore.Get(providerID)
}

// DeleteOAuthCredentials removes stored credentials for a provider from the default store.
func DeleteOAuthCredentials(providerID ProviderID) {
	defaultStore.Delete(providerID)
}

// RefreshOAuthCredentials refreshes credentials for a provider in the default store.
func RefreshOAuthCredentials(ctx context.Context, providerID ProviderID) (*Credentials, error) {
	return defaultStore.RefreshCredentials(ctx, providerID)
}
