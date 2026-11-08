package oauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type copilotRoundTripper func(*http.Request) (*http.Response, error)

func (f copilotRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func copilotResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

const copilotTestToken = "tid=1;exp=2;proxy-ep=proxy.business.githubcopilot.com;st=x"

// newCopilotTestProvider answers the GitHub and Copilot endpoints in process.
// tokenReplies are returned in order for successive device token polls.
func newCopilotTestProvider(t *testing.T, tokenReplies []string, models string, policyStatus int) (*githubCopilotProvider, *[]string, *[]time.Duration) {
	t.Helper()
	var (
		mu       sync.Mutex
		requests []string
		sleeps   []time.Duration
	)
	client := &http.Client{Transport: copilotRoundTripper(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, req.Method+" "+req.URL.String())
		switch req.URL.String() {
		case "https://github.com/login/device/code":
			require.NoError(t, req.ParseForm())
			assert.Equal(t, copilotClientID, req.Form.Get("client_id"))
			assert.Equal(t, "read:user", req.Form.Get("scope"))
			return copilotResponse(http.StatusOK, `{"device_code":"dev","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":2,"expires_in":900}`), nil
		case "https://github.com/login/oauth/access_token":
			require.NoError(t, req.ParseForm())
			assert.Equal(t, "dev", req.Form.Get("device_code"))
			reply := tokenReplies[0]
			tokenReplies = tokenReplies[1:]
			return copilotResponse(http.StatusOK, reply), nil
		case "https://api.github.com/copilot_internal/v2/token":
			assert.Equal(t, "Bearer gho_github", req.Header.Get("Authorization"))
			return copilotResponse(http.StatusOK, `{"token":"`+copilotTestToken+`","expires_at":2000000000}`), nil
		case "https://api.business.githubcopilot.com/models":
			assert.Equal(t, "Bearer "+copilotTestToken, req.Header.Get("Authorization"))
			assert.Equal(t, copilotAPIVersion, req.Header.Get("X-GitHub-Api-Version"))
			return copilotResponse(http.StatusOK, models), nil
		}
		if strings.HasSuffix(req.URL.Path, "/policy") {
			return copilotResponse(policyStatus, `{}`), nil
		}
		t.Errorf("unexpected request %s", req.URL)
		return copilotResponse(http.StatusNotFound, `{}`), nil
	})}
	provider := &githubCopilotProvider{client: client, sleep: func(ctx context.Context, delay time.Duration) error {
		mu.Lock()
		sleeps = append(sleeps, delay)
		mu.Unlock()
		return ctx.Err()
	}}
	return provider, &requests, &sleeps
}

func TestGitHubCopilotLoginExchangesDeviceTokenAndRecordsModels(t *testing.T) {
	models := `{"data":[
		{"id":"gpt-4o","model_picker_enabled":true,"policy":{"state":"enabled"}},
		{"id":"claude-sonnet-4.6","model_picker_enabled":true,"policy":{"state":"unconfigured"}},
		{"id":"embedding","model_picker_enabled":true,"capabilities":{"supports":{"tool_calls":false}}},
		{"id":"off","model_picker_enabled":true,"policy":{"state":"disabled"}}]}`
	provider, requests, sleeps := newCopilotTestProvider(t, []string{
		`{"error":"authorization_pending"}`,
		`{"error":"slow_down","interval":7}`,
		`{"error":"slow_down"}`,
		`{"access_token":"gho_github","token_type":"bearer"}`,
	}, models, http.StatusOK)
	var shown string
	var progress []string
	creds, err := provider.Login(t.Context(), &DefaultLoginCallbacks{
		FnPrompt:   func(context.Context, string, string) (string, error) { return "", nil },
		FnAuth:     func(authURL, instructions string) { shown = authURL + " " + instructions },
		FnProgress: func(message string) { progress = append(progress, message) },
	})
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/login/device Enter code ABCD-1234", shown)
	assert.Equal(t, []time.Duration{2 * time.Second, 2 * time.Second, 7 * time.Second, 12 * time.Second}, *sleeps, "polling did not wait first, honor slow_down intervals, or add five seconds")
	assert.Equal(t, "gho_github", creds.Refresh, "the GitHub token refreshes the Copilot token")
	assert.Equal(t, copilotTestToken, creds.Access)
	assert.Equal(t, int64(2000000000*1000-5*60*1000), creds.Expires)
	assert.Equal(t, []string{"Enabling models..."}, progress)
	available, ok := CopilotAvailableModels(creds.Extra)
	require.True(t, ok)
	assert.Equal(t, []string{"gpt-4o", "claude-sonnet-4.6"}, available, "picker-enabled models plus enabled policies")
	assert.Contains(t, *requests, "POST https://api.business.githubcopilot.com/models/claude-sonnet-4.6/policy")
}

func TestGitHubCopilotRefreshRecordsModelsAndRejectsInvalidTokens(t *testing.T) {
	provider, requests, _ := newCopilotTestProvider(t, nil, `{"data":[{"id":"gpt-4o","model_picker_enabled":true}]}`, http.StatusOK)
	creds, err := provider.RefreshToken(t.Context(), &Credentials{Refresh: "gho_github", Extra: map[string]any{}})
	require.NoError(t, err)
	assert.Equal(t, "gho_github", creds.Refresh)
	assert.Equal(t, copilotTestToken, creds.Access)
	available, _ := CopilotAvailableModels(creds.Extra)
	assert.Equal(t, []string{"gpt-4o"}, available)
	assert.Len(t, *requests, 2)

	secret := "ghu_secret_refresh_material"
	provider.client = &http.Client{Transport: copilotRoundTripper(func(*http.Request) (*http.Response, error) {
		return copilotResponse(http.StatusOK, `{"token":"","refresh":"`+secret+`"}`), nil
	})}
	_, err = provider.RefreshToken(t.Context(), &Credentials{Refresh: "gho_github"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret, "a successful token response reached the error message")

	_, err = provider.RefreshToken(t.Context(), &Credentials{})
	require.EqualError(t, err, "missing refresh token")
}

func TestGitHubCopilotDeviceFlowFailuresAndCancellation(t *testing.T) {
	callbacks := &DefaultLoginCallbacks{FnPrompt: func(context.Context, string, string) (string, error) { return "", nil }, FnAuth: func(string, string) {}}
	provider, _, _ := newCopilotTestProvider(t, []string{`{"error":"access_denied","error_description":"denied by user"}`}, "", http.StatusOK)
	_, err := provider.Login(t.Context(), callbacks)
	require.EqualError(t, err, "device flow failed: access_denied: denied by user")

	provider, _, _ = newCopilotTestProvider(t, []string{`{"unexpected":true}`}, "", http.StatusOK)
	_, err = provider.Login(t.Context(), callbacks)
	require.EqualError(t, err, "invalid device token response")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	provider, _, _ = newCopilotTestProvider(t, nil, "", http.StatusOK)
	_, err = provider.Login(ctx, callbacks)
	require.ErrorIs(t, err, context.Canceled)

	_, err = provider.Login(t.Context(), &DefaultLoginCallbacks{FnPrompt: func(context.Context, string, string) (string, error) { return "://", nil }})
	require.EqualError(t, err, "invalid GitHub Enterprise URL/domain")

	promptErr := errors.New("prompt closed")
	_, err = provider.Login(t.Context(), &DefaultLoginCallbacks{FnPrompt: func(context.Context, string, string) (string, error) { return "", promptErr }})
	require.ErrorIs(t, err, promptErr)

	expired := &githubCopilotProvider{client: provider.client, sleep: provider.sleep}
	_, err = expired.pollDeviceToken(t.Context(), "https://github.com/login/oauth/access_token", &oauth2.DeviceAuthResponse{DeviceCode: "dev", Expiry: time.Unix(1, 0)})
	require.EqualError(t, err, "device flow timed out")
}

func TestGitHubCopilotModelPolicyFallbackAndRateLimits(t *testing.T) {
	var policyCalls int
	var sleeps []time.Duration
	provider := &githubCopilotProvider{
		client: &http.Client{Transport: copilotRoundTripper(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/models") {
				return copilotResponse(http.StatusOK, `{"data":[
					{"id":"gpt-4o","policy":{"state":"enabled"}},
					{"id":"claude-sonnet-4.6","policy":{"state":"unconfigured"}},
					{"id":"gpt-5.6-sol","policy":{"state":"unconfigured"}}]}`), nil
			}
			policyCalls++
			response := copilotResponse(http.StatusTooManyRequests, `{}`)
			response.Header.Set("Retry-After", "1")
			return response, nil
		})},
		sleep: func(_ context.Context, delay time.Duration) error {
			sleeps = append(sleeps, delay)
			return nil
		},
	}
	available, policy, err := provider.copilotModels(t.Context(), "tid=1", "", 0, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"gpt-4o"}, available, "the Individual endpoint falls back to enabled policies")
	assert.Equal(t, []string{"claude-sonnet-4.6", "gpt-5.6-sol"}, policy)

	enabled, err := provider.enableModels(t.Context(), "tid=1", "", policy)
	require.NoError(t, err)
	assert.Empty(t, enabled)
	assert.Equal(t, 3, policyCalls, "exhausted rate limiting did not stop the batch after the first model's retries")
	assert.Equal(t, []time.Duration{time.Second, time.Second}, sleeps)

	_, _, err = provider.copilotModels(t.Context(), "tid=1", "company.ghe.com", 0, 0)
	require.NoError(t, err)
	provider.client = &http.Client{Transport: copilotRoundTripper(func(*http.Request) (*http.Response, error) {
		return copilotResponse(http.StatusOK, `{"models":[]}`), nil
	})}
	_, _, err = provider.copilotModels(t.Context(), "tid=1", "", 0, 0)
	require.EqualError(t, err, "invalid Copilot models response")
}

func TestCopilotAvailableModelsAcceptsDecodedJSON(t *testing.T) {
	ids, ok := CopilotAvailableModels(map[string]any{CopilotAvailableModelsKey: []any{"a", "b"}})
	require.True(t, ok)
	assert.Equal(t, []string{"a", "b"}, ids)
	_, ok = CopilotAvailableModels(map[string]any{CopilotAvailableModelsKey: []any{"a", 1}})
	assert.False(t, ok)
	_, ok = CopilotAvailableModels(nil)
	assert.False(t, ok)
	assert.Equal(t, "https://copilot-api.company.ghe.com", GetGitHubCopilotBaseURL("tid=1", "https://company.ghe.com/"))
}
