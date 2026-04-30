package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	airoot "github.com/losinggeneration/tau/ai"
)

// GitHub Copilot OAuth provider.
type githubCopilotProvider struct {
	clientID     string
	authURL      string
	tokenURL     string
	callbackPort int
}

func NewGitHubCopilotProvider() Provider {
	return &githubCopilotProvider{
		clientID:     "01ab58ac19854d7297f5d8e34fc69819",
		authURL:      "https://github.com/login/device/code",
		tokenURL:     "https://github.com/login/oauth/access_token",
		callbackPort: 53693,
	}
}

func (p *githubCopilotProvider) ID() ProviderID {
	return ProviderGitHubCopilot
}

func (p *githubCopilotProvider) Name() string {
	return "GitHub Copilot"
}

func (p *githubCopilotProvider) UsesCallbackServer() bool {
	return true
}

func (p *githubCopilotProvider) ModifyModels(models []*airoot.Model, credentials *Credentials) []*airoot.Model {
	baseURL := GetGitHubCopilotBaseURL(credentials.Access, "")
	if credentials.Extra != nil {
		if domain, ok := credentials.Extra["enterprise_url"].(string); ok && domain != "" {
			baseURL = fmt.Sprintf("https://copilot-api.%s", normalizeDomain(domain))
		}
	}

	// Update base URL for all GitHub Copilot models
	for _, m := range models {
		if m.Provider == "github-copilot" {
			m.BaseURL = baseURL
		}
	}

	return models
}

func normalizeDomain(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}

	raw := trimmed
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	return parsed.Hostname()
}

func getBaseURLFromToken(token string) string {
	match := strings.Split(token, "proxy-ep=")
	if len(match) < 2 {
		return ""
	}

	host := match[1]
	if idx := strings.Index(host, ";"); idx >= 0 {
		host = host[:idx]
	}
	if host == "" {
		return ""
	}

	host = strings.Replace(host, "proxy.", "api.", 1)
	return "https://" + host
}

// NormalizeDomain normalizes an enterprise GitHub URL/domain to a hostname.
func NormalizeDomain(input string) string {
	return normalizeDomain(input)
}

// GetGitHubCopilotBaseURL resolves the API base URL for the given token/domain.
// The Copilot token contains a proxy-ep field that encodes the API endpoint.
func GetGitHubCopilotBaseURL(token string, enterpriseDomain string) string {
	if fromToken := getBaseURLFromToken(token); fromToken != "" {
		return fromToken
	}

	if domain := normalizeDomain(enterpriseDomain); domain != "" {
		return "https://copilot-api." + domain
	}

	return "https://api.individual.githubcopilot.com"
}

const (
	copilotUserAgent     = "GitHubCopilotChat/0.35.0"
	copilotEditorVersion = "vscode/1.107.0"
	copilotPluginVersion = "copilot-chat/0.35.0"
	copilotIntegrationID = "vscode-chat"
)

// CopilotHeaders returns the headers required for Copilot API requests.
func CopilotHeaders() map[string]string {
	return map[string]string{
		"User-Agent":             copilotUserAgent,
		"Editor-Version":         copilotEditorVersion,
		"Editor-Plugin-Version":  copilotPluginVersion,
		"Copilot-Integration-Id": copilotIntegrationID,
	}
}

// EnableGitHubCopilotModel enables a specific model for the user's GitHub Copilot account.
// This is required for some models (like Claude, Grok) before they can be used.
// Returns true if the model was enabled successfully.
func EnableGitHubCopilotModel(ctx context.Context, token string, modelID string, enterpriseDomain string) (bool, error) {
	baseURL := GetGitHubCopilotBaseURL(token, enterpriseDomain)
	policyURL := fmt.Sprintf("%s/models/%s/policy", baseURL, modelID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, policyURL, strings.NewReader(`{"state":"enabled"}`))
	if err != nil {
		return false, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("Openai-Intent", "chat-policy")
	req.Header.Set("X-Interaction-Type", "chat-policy")

	for k, v := range CopilotHeaders() {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		if closeErr := closeBody(resp); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	return resp.StatusCode == http.StatusOK, nil
}

// EnableAllGitHubCopilotModels enables all known GitHub Copilot models.
// This is called after successful login to ensure all models are available.
// The callback is called with (modelID, success) for each model.
func EnableAllGitHubCopilotModels(ctx context.Context, token string, enterpriseDomain string, callback func(modelID string, success bool)) error {
	// List of known GitHub Copilot models that may require policy acceptance
	// This should be kept in sync with the model registry
	copilotModels := []string{
		"gpt-4o",
		"gpt-4o-mini",
		"claude-3-5-sonnet",
		"claude-3-5-sonnet-latest",
		"claude-3-5-haiku",
		"claude-3-opus",
		"claude-3-sonnet",
		"o1",
		"o1-mini",
		"o1-preview",
		"o3-mini",
		"o3-mini-high",
		"grok-2",
		"grok-2-mini",
		"grok-beta",
		"grok-v1-beta",
	}

	// Enable models concurrently
	type result struct {
		modelID string
		success bool
		err     error
	}

	results := make(chan result, len(copilotModels))

	for _, modelID := range copilotModels {
		go func() {
			success, err := EnableGitHubCopilotModel(ctx, token, modelID, enterpriseDomain)
			results <- result{modelID: modelID, success: success, err: err}
		}()
	}

	var lastErr error
	for range copilotModels {
		r := <-results
		if r.err != nil && lastErr == nil {
			lastErr = r.err
		}
		if callback != nil {
			callback(r.modelID, r.success)
		}
	}

	return lastErr
}

func (p *githubCopilotProvider) Login(ctx context.Context, callbacks LoginCallbacks) (creds *Credentials, err error) {
	// For basic login without enterprise domain, delegate to LoginWithEnterpriseDomain with empty domain
	return p.LoginWithEnterpriseDomain(ctx, callbacks, "", nil)
}

// LoginWithEnterpriseDomain runs the GitHub Copilot OAuth login flow with enterprise domain support.
// If enterpriseDomain is empty, it uses github.com. After successful login, it enables all known models.
func (p *githubCopilotProvider) LoginWithEnterpriseDomain(
	ctx context.Context,
	callbacks LoginCallbacks,
	enterpriseDomain string,
	onProgress func(modelID string, success bool),
) (creds *Credentials, err error) {
	// GitHub Device Flow doesn't use PKCE
	server := newCallbackServer("github-copilot", p.callbackPort)
	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}
	defer func() {
		if stopErr := server.Stop(); err == nil && stopErr != nil {
			err = stopErr
		}
	}()

	// Step 1: Request device code
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.authURL, strings.NewReader("client_id="+p.clientID+"&scope=read:user"))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
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

	var deviceResp struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		Interval        int    `json:"interval"`
	}
	if err := json.Unmarshal(body, &deviceResp); err != nil {
		return nil, fmt.Errorf("parse device response: %w", err)
	}

	// Show user code
	callbacks.OnAuth(deviceResp.VerificationURI+"?input="+deviceResp.UserCode, "Enter this code: "+deviceResp.UserCode)

	// Step 2: Poll for token
	ticker := time.NewTicker(time.Duration(deviceResp.Interval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			tokenResp, err := p.exchangeTokenWithEnterprise(ctx, deviceResp.DeviceCode, enterpriseDomain)
			if err != nil {
				if strings.Contains(err.Error(), "authorization_pending") {
					continue
				}

				return nil, err
			}

			// Store enterprise domain in credentials
			if enterpriseDomain != "" {
				if tokenResp.Extra == nil {
					tokenResp.Extra = make(map[string]any)
				}
				tokenResp.Extra["enterprise_url"] = enterpriseDomain
			}

			// Enable all known models
			if onProgress != nil {
				callbacks.OnProgress("Enabling models...")
			}
			if err := EnableAllGitHubCopilotModels(ctx, tokenResp.Access, enterpriseDomain, onProgress); err != nil {
				// Log but don't fail - model enablement is best effort
				callbacks.OnProgress(fmt.Sprintf("Warning: failed to enable some models: %v", err))
			}

			return tokenResp, nil
		}
	}
}

func (p *githubCopilotProvider) exchangeTokenWithEnterprise(ctx context.Context, deviceCode string, enterpriseDomain string) (*Credentials, error) {
	body := "grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code=" + deviceCode + "&client_id=" + p.clientID

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := closeBody(resp); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange status: %d body: %s", resp.StatusCode, respBody)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token,omitempty"`
		ExpiresIn    int    `json:"expires_in,omitempty"`
		Scope        string `json:"scope,omitempty"`
		ExpiresAt    int64  `json:"expires_at,omitempty"`
	}
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}

	creds := &Credentials{
		Access:  tokenResp.AccessToken,
		Refresh: tokenResp.RefreshToken,
		Expires: int64(tokenResp.ExpiresIn)*1000 - 5*60*1000,
	}

	return creds, nil
}

func (p *githubCopilotProvider) RefreshToken(ctx context.Context, credentials *Credentials) (*Credentials, error) {
	if credentials.Refresh == "" {
		return nil, fmt.Errorf("missing refresh token")
	}

	// Get the Copilot token endpoint
	copilotTokenURL := "https://api.individual.githubcopilot.com/copilot_internal/v2/token"
	if credentials.Extra != nil {
		if domain, ok := credentials.Extra["enterprise_url"].(string); ok && domain != "" {
			copilotTokenURL = fmt.Sprintf("https://api.%s/copilot_internal/v2/token", normalizeDomain(domain))
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, copilotTokenURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", credentials.Refresh))
	req.Header.Set("User-Agent", copilotUserAgent)
	req.Header.Set("Editor-Version", copilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", copilotPluginVersion)
	req.Header.Set("Copilot-Integration-Id", copilotIntegrationID)

	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := closeBody(resp); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh failed (status %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		Token     string `json:"token"`
		ExpiresAt int    `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse refresh response: %w", err)
	}

	if tokenResp.Token == "" {
		return nil, fmt.Errorf("refresh response missing token: %s", string(body))
	}

	// Calculate new expiration time, subtracting 5 minutes for safety margin
	expires := int64(tokenResp.ExpiresAt)*1000 - 5*60*1000

	return &Credentials{
		Access:  tokenResp.Token,
		Refresh: credentials.Refresh, // Keep original refresh token
		Expires: expires,
		Extra:   credentials.Extra,
	}, nil
}

func (p *githubCopilotProvider) GetAPIKey(credentials *Credentials) string {
	return credentials.Access
}
