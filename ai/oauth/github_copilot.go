package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	airoot "github.com/losinggeneration/tau/ai"
)

const (
	copilotClientID   = "Iv1.b507a08c87ecfe98"
	copilotAPIVersion = "2026-06-01"
	copilotIndividual = "https://api.individual.githubcopilot.com"

	// CopilotAvailableModelsKey is the credential Extra key holding the model
	// IDs the signed-in account can use.
	CopilotAvailableModelsKey = "availableModelIds"
	copilotEnterpriseKey      = "enterprise_url"
)

// GitHub Copilot OAuth provider.
type githubCopilotProvider struct {
	client *http.Client
	// sleep waits between rate-limited model requests; tests replace it.
	sleep func(context.Context, time.Duration) error
}

func NewGitHubCopilotProvider() Provider {
	return &githubCopilotProvider{client: &http.Client{Timeout: 30 * time.Second}, sleep: sleepContext}
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *githubCopilotProvider) ID() ProviderID {
	return ProviderGitHubCopilot
}

func (p *githubCopilotProvider) Name() string {
	return "GitHub Copilot"
}

func (p *githubCopilotProvider) UsesCallbackServer() bool {
	return false
}

func (p *githubCopilotProvider) ModifyModels(models []*airoot.Model, credentials *Credentials) []*airoot.Model {
	baseURL := GetGitHubCopilotBaseURL(credentials.Access, copilotEnterpriseDomain(credentials))
	for _, m := range models {
		if m.Provider == airoot.ProviderGitHubCopilot {
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

// NormalizeDomain normalizes an enterprise GitHub URL/domain to a hostname.
func NormalizeDomain(input string) string {
	return normalizeDomain(input)
}

// GetGitHubCopilotBaseURL resolves the API base URL for the given token/domain.
// The Copilot token contains a proxy-ep field that encodes the API endpoint.
func GetGitHubCopilotBaseURL(token string, enterpriseDomain string) string {
	if fromToken := airoot.CopilotBaseURL(token); fromToken != "" {
		return fromToken
	}
	if domain := normalizeDomain(enterpriseDomain); domain != "" {
		return "https://copilot-api." + domain
	}
	return copilotIndividual
}

// CopilotHeaders returns the headers required for Copilot API requests.
func CopilotHeaders() map[string]string {
	return map[string]string{
		"User-Agent":             "GitHubCopilotChat/0.35.0",
		"Editor-Version":         "vscode/1.107.0",
		"Editor-Plugin-Version":  "copilot-chat/0.35.0",
		"Copilot-Integration-Id": "vscode-chat",
	}
}

func copilotEnterpriseDomain(credentials *Credentials) string {
	if credentials == nil {
		return ""
	}
	domain, _ := credentials.Extra[copilotEnterpriseKey].(string)
	return normalizeDomain(domain)
}

// CopilotAvailableModels returns the model IDs recorded at sign-in or refresh,
// and false when the credential records none.
func CopilotAvailableModels(extra map[string]any) ([]string, bool) {
	switch ids := extra[CopilotAvailableModelsKey].(type) {
	case []string:
		return ids, true
	case []any:
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			text, ok := id.(string)
			if !ok {
				return nil, false
			}
			out = append(out, text)
		}
		return out, true
	}
	return nil, false
}

func (p *githubCopilotProvider) Login(ctx context.Context, callbacks LoginCallbacks) (*Credentials, error) {
	input, err := callbacks.OnPrompt(ctx, "GitHub Enterprise URL/domain (blank for github.com)", "company.ghe.com")
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	enterpriseDomain := normalizeDomain(input)
	if strings.TrimSpace(input) != "" && enterpriseDomain == "" {
		return nil, errors.New("invalid GitHub Enterprise URL/domain")
	}
	domain := enterpriseDomain
	if domain == "" {
		domain = "github.com"
	}

	config := oauth2.Config{
		ClientID: copilotClientID,
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: "https://" + domain + "/login/device/code",
			TokenURL:      "https://" + domain + "/login/oauth/access_token",
		},
	}
	oauthCtx := context.WithValue(ctx, oauth2.HTTPClient, p.client)
	device, err := config.DeviceAuth(oauthCtx, oauth2.SetAuthURLParam("scope", "read:user"))
	if err != nil {
		return nil, fmt.Errorf("github device authorization: %w", err)
	}
	// The verification URI is opened in a browser, so it must be a web URL.
	if parsed, err := url.Parse(device.VerificationURI); err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("untrusted verification_uri in device code response")
	}
	callbacks.OnAuth(device.VerificationURI, "Enter code "+device.UserCode)
	githubToken, err := p.pollDeviceToken(ctx, config.Endpoint.TokenURL, device)
	if err != nil {
		return nil, err
	}

	creds, err := p.copilotToken(ctx, githubToken, enterpriseDomain)
	if err != nil {
		return nil, err
	}
	available, policy, err := p.copilotModels(ctx, creds.Access, enterpriseDomain, 2, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if len(policy) > 0 {
		callbacks.OnProgress("Enabling models...")
		enabled, err := p.enableModels(ctx, creds.Access, enterpriseDomain, policy)
		if err != nil {
			return nil, err
		}
		for _, id := range enabled {
			if !slices.Contains(available, id) {
				available = append(available, id)
			}
		}
	}
	creds.Extra[CopilotAvailableModelsKey] = available
	return creds, nil
}

// pollDeviceToken polls for the GitHub access token as RFC 8628 describes,
// following GitHub's interval on slow_down. GitHub reports pending and failed
// authorizations as successful responses with an error field.
func (p *githubCopilotProvider) pollDeviceToken(ctx context.Context, tokenURL string, device *oauth2.DeviceAuthResponse) (string, error) {
	interval := 5 * time.Second
	if device.Interval > 0 {
		interval = time.Duration(device.Interval) * time.Second
	}
	headers := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/x-www-form-urlencoded",
		"User-Agent":   CopilotHeaders()["User-Agent"],
	}
	form := url.Values{
		"client_id":   {copilotClientID},
		"device_code": {device.DeviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}.Encode()
	var slowDowns int
	for {
		wait := interval
		if !device.Expiry.IsZero() {
			remaining := time.Until(device.Expiry)
			if remaining <= 0 {
				break
			}
			wait = min(wait, remaining)
		}
		if err := p.sleep(ctx, wait); err != nil {
			return "", err
		}
		if !device.Expiry.IsZero() && !time.Now().Before(device.Expiry) {
			break
		}
		resp, body, err := p.do(ctx, http.MethodPost, tokenURL, headers, form)
		if err != nil {
			return "", fmt.Errorf("github device token request: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return "", fmt.Errorf("github device token request: %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		var raw struct {
			AccessToken *string  `json:"access_token"`
			Error       *string  `json:"error"`
			Description string   `json:"error_description"`
			Interval    *float64 `json:"interval"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || (raw.AccessToken == nil && raw.Error == nil) {
			return "", errors.New("invalid device token response")
		}
		if raw.AccessToken != nil {
			return *raw.AccessToken, nil
		}
		switch *raw.Error {
		case "authorization_pending":
		case "slow_down":
			slowDowns++
			if raw.Interval != nil && *raw.Interval > 0 {
				interval = max(time.Duration(*raw.Interval*float64(time.Second)), time.Second)
			} else {
				interval += 5 * time.Second
			}
		default:
			if raw.Description != "" {
				return "", fmt.Errorf("device flow failed: %s: %s", *raw.Error, raw.Description)
			}
			return "", fmt.Errorf("device flow failed: %s", *raw.Error)
		}
	}
	if slowDowns > 0 {
		return "", errors.New("device flow timed out after one or more slow_down responses; this is often caused by clock drift in WSL or VM environments, so sync or restart the VM clock and try again")
	}
	return "", errors.New("device flow timed out")
}

// RefreshToken exchanges the stored GitHub token for a new Copilot token and
// records the models the account can currently use.
func (p *githubCopilotProvider) RefreshToken(ctx context.Context, credentials *Credentials) (*Credentials, error) {
	if credentials == nil || credentials.Refresh == "" {
		return nil, errors.New("missing refresh token")
	}
	enterpriseDomain := copilotEnterpriseDomain(credentials)
	creds, err := p.copilotToken(ctx, credentials.Refresh, enterpriseDomain)
	if err != nil {
		return nil, err
	}
	available, _, err := p.copilotModels(ctx, creds.Access, enterpriseDomain, 0, 0)
	if err != nil {
		return nil, err
	}
	creds.Extra[CopilotAvailableModelsKey] = available
	return creds, nil
}

func (p *githubCopilotProvider) GetAPIKey(credentials *Credentials) string {
	return credentials.Access
}

// copilotToken exchanges a GitHub access token for a Copilot API token.
func (p *githubCopilotProvider) copilotToken(ctx context.Context, githubToken, enterpriseDomain string) (*Credentials, error) {
	domain := enterpriseDomain
	if domain == "" {
		domain = "github.com"
	}
	headers := CopilotHeaders()
	headers["Accept"] = "application/json"
	headers["Authorization"] = "Bearer " + githubToken
	resp, body, err := p.do(ctx, http.MethodGet, "https://api."+domain+"/copilot_internal/v2/token", headers, "")
	if err != nil {
		return nil, fmt.Errorf("copilot token request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot token request: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var raw struct {
		Token     *string  `json:"token"`
		ExpiresAt *float64 `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Token == nil || raw.ExpiresAt == nil {
		return nil, errors.New("invalid Copilot token response fields")
	}
	extra := map[string]any{}
	if enterpriseDomain != "" {
		extra[copilotEnterpriseKey] = enterpriseDomain
	}
	return &Credentials{
		Refresh: githubToken,
		Access:  *raw.Token,
		Expires: int64(*raw.ExpiresAt*1000) - 5*60*1000,
		Extra:   extra,
	}, nil
}

// copilotModels lists the account's tool-capable models: those it can use now,
// and shipped models whose policy still needs enabling.
func (p *githubCopilotProvider) copilotModels(ctx context.Context, token, enterpriseDomain string, retries int, budget time.Duration) (available, policy []string, err error) {
	baseURL := GetGitHubCopilotBaseURL(token, enterpriseDomain)
	headers := CopilotHeaders()
	headers["Accept"] = "application/json"
	headers["Authorization"] = "Bearer " + token
	headers["X-GitHub-Api-Version"] = copilotAPIVersion
	resp, body, err := p.doRateLimited(ctx, http.MethodGet, baseURL+"/models", headers, "", retries, budget)
	if err != nil {
		return nil, nil, fmt.Errorf("copilot models request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, fmt.Errorf("copilot models request: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var catalog struct {
		Data []struct {
			ID           *string `json:"id"`
			Picker       bool    `json:"model_picker_enabled"`
			Capabilities struct {
				Supports struct {
					ToolCalls *bool `json:"tool_calls"`
				} `json:"supports"`
			} `json:"capabilities"`
			Policy struct {
				State string `json:"state"`
			} `json:"policy"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil || catalog.Data == nil {
		return nil, nil, errors.New("invalid Copilot models response")
	}
	type accountModel struct {
		id, state string
		picker    bool
	}
	var models []accountModel
	for _, item := range catalog.Data {
		if item.ID == nil || (item.Capabilities.Supports.ToolCalls != nil && !*item.Capabilities.Supports.ToolCalls) {
			continue
		}
		models = append(models, accountModel{id: *item.ID, state: item.Policy.State, picker: item.Picker})
	}
	available = []string{}
	for _, model := range models {
		if model.picker && model.state != "disabled" {
			available = append(available, model.id)
		}
	}
	// Some Individual accounts report every picker flag false despite enabled
	// policies; only that endpoint falls back to the policy state.
	fallback := baseURL == copilotIndividual && len(available) == 0
	if fallback {
		for _, model := range models {
			if model.state == "enabled" {
				available = append(available, model.id)
			}
		}
	}
	for _, model := range models {
		if model.state != "unconfigured" || (!model.picker && !fallback) {
			continue
		}
		if _, err := airoot.GetModel(airoot.ProviderGitHubCopilot, model.id); err == nil {
			policy = append(policy, model.id)
		}
	}
	return available, policy, nil
}

// enableModels accepts each model's policy in turn and returns the ones that
// succeeded. Policy updates are best effort; exhausted rate limiting stops the
// batch.
func (p *githubCopilotProvider) enableModels(ctx context.Context, token, enterpriseDomain string, ids []string) ([]string, error) {
	baseURL := GetGitHubCopilotBaseURL(token, enterpriseDomain)
	headers := CopilotHeaders()
	headers["Content-Type"] = "application/json"
	headers["Authorization"] = "Bearer " + token
	headers["openai-intent"] = "chat-policy"
	headers["x-interaction-type"] = "chat-policy"
	var enabled []string
	for _, id := range ids {
		resp, _, err := p.doRateLimited(ctx, http.MethodPost, baseURL+"/models/"+url.PathEscape(id)+"/policy", headers, `{"state":"enabled"}`, 2, 5*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			break
		}
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			enabled = append(enabled, id)
		}
	}
	return enabled, nil
}

// doRateLimited retries 429 responses within budget, honoring retry-after.
// Each attempt has its own five-second limit.
func (p *githubCopilotProvider) doRateLimited(ctx context.Context, method, target string, headers map[string]string, body string, retries int, budget time.Duration) (*http.Response, []byte, error) {
	deadline := time.Now().Add(budget)
	for retry := 0; ; retry++ {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, data, err := p.do(attemptCtx, method, target, headers, body)
		cancel()
		if err != nil || resp.StatusCode != http.StatusTooManyRequests || retry >= retries || budget <= 0 {
			return resp, data, err
		}
		delay := 500 * time.Millisecond << retry
		if value := resp.Header.Get("Retry-After"); value != "" {
			if seconds, err := strconv.ParseFloat(value, 64); err == nil {
				delay = time.Duration(seconds * float64(time.Second))
			} else if at, err := http.ParseTime(value); err == nil {
				delay = time.Until(at)
			} else {
				return resp, data, nil
			}
		}
		delay = max(delay, 0)
		if delay >= time.Until(deadline) {
			return resp, data, nil
		}
		if err := p.sleep(ctx, delay); err != nil {
			return nil, nil, err
		}
	}
}

// do sends one request and reads its whole body, keeping close failures.
func (p *githubCopilotProvider) do(ctx context.Context, method, target string, headers map[string]string, body string) (resp *http.Response, data []byte, err error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err = p.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if closeErr := closeBody(resp); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	data, err = io.ReadAll(resp.Body)
	return resp, data, err
}
