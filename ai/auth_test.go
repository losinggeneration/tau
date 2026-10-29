package ai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewAnthropicClientDoesNotRequireEnvAtConstruction(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	client, err := newAnthropicClient(&Model{ID: "claude-test"})
	if err != nil {
		t.Fatalf("newAnthropicClient: %v", err)
	}
	if client == nil {
		t.Fatal("expected client")
	}
}

func TestNewGoogleClientDoesNotRequireEnvAtConstruction(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")

	client, err := newGoogleClient(&Model{ID: "gemini-test"})
	if err != nil {
		t.Fatalf("newGoogleClient: %v", err)
	}
	if client == nil {
		t.Fatal("expected client")
	}
}

func TestNewMistralClientDoesNotRequireEnvAtConstruction(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")

	client, err := newMistralClient(&Model{ID: "mistral-test"})
	if err != nil {
		t.Fatalf("newMistralClient: %v", err)
	}
	if client == nil {
		t.Fatal("expected client")
	}
}

func TestHTTPTransportWithAPIKeyOverridesAuth(t *testing.T) {
	transport := NewTransport("https://example.com", "env-key", nil)
	override := transport.withAPIKey("runtime-key")

	if override == transport {
		t.Fatal("expected clone when overriding API key")
	}
	if override.apiKey != "runtime-key" {
		t.Fatalf("apiKey = %q, want runtime-key", override.apiKey)
	}
	if transport.apiKey != "env-key" {
		t.Fatalf("original apiKey = %q, want env-key", transport.apiKey)
	}
}

// TestGetEnvAPIKeyAnthropicPrecedence verifies OAuth token takes priority over API key.
func TestGetEnvAPIKeyAnthropicPrecedence(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")

	// No keys set
	if key := GetEnvAPIKey(ProviderAnthropic); key != "" {
		t.Errorf("expected empty, got %q", key)
	}

	// API key only
	t.Setenv("ANTHROPIC_API_KEY", "api-key-value")
	defer t.Setenv("ANTHROPIC_API_KEY", "")
	if key := GetEnvAPIKey(ProviderAnthropic); key != "api-key-value" {
		t.Errorf("expected api-key-value, got %q", key)
	}

	// Subscription tokens are never read from the environment.
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "oauth-token-value")
	if key := GetEnvAPIKey(ProviderAnthropic); key != "api-key-value" {
		t.Errorf("ANTHROPIC_OAUTH_TOKEN must be ignored, got %q", key)
	}
}

func TestGetEnvAPIKeyOpenAICodexPrecedence(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("COPILOT_GITHUB_TOKEN", "")

	if key := GetEnvAPIKey(ProviderOpenAICodex); key != "" {
		t.Errorf("expected empty, got %q", key)
	}

	t.Setenv("GITHUB_TOKEN", "github-token")
	if key := GetEnvAPIKey(ProviderOpenAICodex); key != "github-token" {
		t.Errorf("expected github-token, got %q", key)
	}

	t.Setenv("GH_TOKEN", "gh-token")
	if key := GetEnvAPIKey(ProviderOpenAICodex); key != "gh-token" {
		t.Errorf("expected gh-token, got %q", key)
	}

	t.Setenv("COPILOT_GITHUB_TOKEN", "copilot-token")
	if key := GetEnvAPIKey(ProviderOpenAICodex); key != "copilot-token" {
		t.Errorf("expected copilot-token, got %q", key)
	}
}

// TestGetEnvAPIKeyGitHubCopilotPrecedence verifies token priority order.
func TestGetEnvAPIKeyGitHubCopilotPrecedence(t *testing.T) {
	// Clear all
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("COPILOT_GITHUB_TOKEN", "")

	if key := GetEnvAPIKey(ProviderGitHubCopilot); key != "" {
		t.Errorf("expected empty, got %q", key)
	}

	// GITHUB_TOKEN only
	t.Setenv("GITHUB_TOKEN", "github-token")
	defer t.Setenv("GITHUB_TOKEN", "")
	if key := GetEnvAPIKey(ProviderGitHubCopilot); key != "github-token" {
		t.Errorf("expected github-token, got %q", key)
	}

	// GH_TOKEN takes priority
	t.Setenv("GH_TOKEN", "gh-token")
	defer t.Setenv("GH_TOKEN", "")
	if key := GetEnvAPIKey(ProviderGitHubCopilot); key != "gh-token" {
		t.Errorf("expected gh-token, got %q", key)
	}

	// COPILOT_GITHUB_TOKEN takes highest priority
	t.Setenv("COPILOT_GITHUB_TOKEN", "copilot-token")
	defer t.Setenv("COPILOT_GITHUB_TOKEN", "")
	if key := GetEnvAPIKey(ProviderGitHubCopilot); key != "copilot-token" {
		t.Errorf("expected copilot-token, got %q", key)
	}
}

// TestGetEnvAPIKeyVertex verifies Vertex ADC and env var handling.
func TestGetEnvAPIKeyVertex(t *testing.T) {
	// Clear all
	t.Setenv("GOOGLE_CLOUD_API_KEY", "")
	t.Setenv("VERTEXAI_API_KEY", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCLOUD_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "")

	if key := GetEnvAPIKey(ProviderGoogleVertex); key != "" {
		t.Errorf("expected empty when no credentials, got %q", key)
	}

	// GOOGLE_CLOUD_API_KEY takes priority
	t.Setenv("GOOGLE_CLOUD_API_KEY", "vertex-api-key")
	defer t.Setenv("GOOGLE_CLOUD_API_KEY", "")
	if key := GetEnvAPIKey(ProviderGoogleVertex); key != "vertex-api-key" {
		t.Errorf("expected vertex-api-key, got %q", key)
	}

	// Legacy VERTEXAI_API_KEY is provider-internal only and should not affect the
	// public helper surface.
	t.Setenv("GOOGLE_CLOUD_API_KEY", "")
	t.Setenv("VERTEXAI_API_KEY", "legacy-vertex-key")
	defer t.Setenv("VERTEXAI_API_KEY", "")
	if key := GetEnvAPIKey(ProviderGoogleVertex); key != "" {
		t.Errorf("expected empty when only VERTEXAI_API_KEY is set, got %q", key)
	}

	// ADC marker requires credentials + project + location.
	t.Setenv("VERTEXAI_API_KEY", "")
	adcPath := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adcPath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("WriteFile(adc.json): %v", err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", adcPath)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "test-project")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "us-central1")
	if key := GetEnvAPIKey(ProviderGoogleVertex); key != "<authenticated>" {
		t.Errorf("expected <authenticated>, got %q", key)
	}
}

// TestGetEnvAPIKeyBedrock verifies Bedrock credential sources.
func TestGetEnvAPIKeyBedrock(t *testing.T) {
	// Clear all
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")

	if key := GetEnvAPIKey(ProviderAmazonBedrock); key != "" {
		t.Errorf("expected empty, got %q", key)
	}

	// Explicit credentials require both key ID and secret access key.
	t.Setenv("AWS_ACCESS_KEY_ID", "key-id")
	if key := GetEnvAPIKey(ProviderAmazonBedrock); key != "" {
		t.Errorf("expected empty with incomplete explicit credentials, got %q", key)
	}

	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")
	defer func() {
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	}()
	if key := GetEnvAPIKey(ProviderAmazonBedrock); key != "<authenticated>" {
		t.Errorf("expected <authenticated>, got %q", key)
	}

	// AWS_PROFILE returns authenticated sentinel
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_PROFILE", "my-profile")
	defer t.Setenv("AWS_PROFILE", "")
	if key := GetEnvAPIKey(ProviderAmazonBedrock); key != "<authenticated>" {
		t.Errorf("expected <authenticated>, got %q", key)
	}

	// AWS_BEARER_TOKEN_BEDROCK
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bearer-token")
	defer t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	if key := GetEnvAPIKey(ProviderAmazonBedrock); key != "<authenticated>" {
		t.Errorf("expected <authenticated>, got %q", key)
	}
}

func TestHasBedrockCredentialsDoesNotInferFromCredentialsFileAlone(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.aws): %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "credentials"), []byte("[default]\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(credentials): %v", err)
	}

	if HasBedrockCredentials() {
		t.Fatal("HasBedrockCredentials() = true with file-only credentials, want false")
	}
}

// TestEnvVarNamesVertex verifies Vertex env var list.
func TestEnvVarNamesVertex(t *testing.T) {
	names := envVarNames(ProviderGoogleVertex)
	expected := []string{
		"GOOGLE_CLOUD_API_KEY",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_CLOUD_PROJECT",
		"GCLOUD_PROJECT",
		"GOOGLE_CLOUD_LOCATION",
	}
	if len(names) != len(expected) {
		t.Fatalf("expected %d env vars, got %d", len(expected), len(names))
	}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("envVarNames[%d] = %q, want %q", i, name, expected[i])
		}
	}
}

// TestHTTPTransport401RefreshAndRetry verifies that the transport handles 401
// responses by calling the refresh callback once and then retrying the request.
func TestHTTPTransport401RefreshAndRetry(t *testing.T) {
	refreshCallCount := 0
	var refreshCredsCalled bool

	refreshCreds := func(ctx context.Context, provider string) (string, error) {
		refreshCallCount++
		refreshCredsCalled = true
		return "new-api-key", nil
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "Bearer new-api-key" {
			// Retry with new key - success
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"ok": true}`))
			return
		}
		// First request - unauthorized
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	transport := NewTransportWithOptions(server.URL, "old-api-key", nil, TransportOptions{
		RefreshCreds: refreshCreds,
		Provider:     "test-provider",
	})

	resp, err := transport.Do(context.Background(), "GET", "/test", "")
	if err != nil {
		t.Fatalf("transport.Do() error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("final response status = %d, want 200", resp.StatusCode)
	}

	if !refreshCredsCalled {
		t.Error("refreshCreds was not called")
	}

	if refreshCallCount != 1 {
		t.Errorf("refreshCreds called %d times, want exactly 1", refreshCallCount)
	}
}

// TestHTTPTransport401RefreshFailure verifies that a refresh failure returns
// an error without retrying.
func TestHTTPTransport401RefreshFailure(t *testing.T) {
	refreshCreds := func(ctx context.Context, provider string) (string, error) {
		return "", fmt.Errorf("refresh failed")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	transport := NewTransportWithOptions(server.URL, "old-api-key", nil, TransportOptions{
		RefreshCreds: refreshCreds,
		Provider:     "test-provider",
	})

	_, err := transport.Do(context.Background(), "GET", "/test", "")
	if err == nil {
		t.Fatal("expected error from failed refresh, got nil")
	}
	if !strings.Contains(err.Error(), "refresh failed") {
		t.Errorf("error = %q, want it to contain 'refresh failed'", err.Error())
	}
	if !strings.Contains(err.Error(), "401 received") {
		t.Errorf("error = %q, want it to contain '401 received'", err.Error())
	}
}

// TestHTTPTransportNoRefreshOn200 verifies that successful responses don't
// trigger the refresh callback.
func TestHTTPTransportNoRefreshOn200(t *testing.T) {
	refreshCalled := false
	refreshCreds := func(ctx context.Context, provider string) (string, error) {
		refreshCalled = true
		return "new-key", nil
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	transport := NewTransportWithOptions(server.URL, "api-key", nil, TransportOptions{
		RefreshCreds: refreshCreds,
	})

	resp, err := transport.Do(context.Background(), "GET", "/test", "")
	if err != nil {
		t.Fatalf("transport.Do() error: %v", err)
	}
	defer resp.Body.Close()

	if refreshCalled {
		t.Error("refreshCreds was called on successful response")
	}
}

// TestHTTPTransportNoRetryOn401WithoutRefresh verifies that 401 without a
// refresh callback just returns the 401 response.
func TestHTTPTransportNoRetryOn401WithoutRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	transport := NewTransport(server.URL, "api-key", nil)

	resp, err := transport.Do(context.Background(), "GET", "/test", "")
	if err != nil {
		t.Fatalf("transport.Do() error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("response status = %d, want 401", resp.StatusCode)
	}
}

// TestEnvAPIKeysMapContainsRequired verifies key providers are mapped.
func TestEnvAPIKeysMapContainsRequired(t *testing.T) {
	required := []string{
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
		"AZURE_OPENAI_API_KEY",
		"GEMINI_API_KEY",
		"MISTRAL_API_KEY",
		"GOOGLE_CLOUD_API_KEY",
		"GOOGLE_CLOUD_PROJECT",
		"GCLOUD_PROJECT",
		"AI_GATEWAY_API_KEY",
		"AWS_ACCESS_KEY_ID",
		"AWS_PROFILE",
	}
	for _, envVar := range required {
		if _, ok := EnvAPIKeys[envVar]; !ok {
			t.Errorf("EnvAPIKeys missing required entry for %s", envVar)
		}
	}
}
