package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	airoot "github.com/losinggeneration/tau/ai"
	"golang.org/x/oauth2"
)

// mockProvider is a test implementation of Provider for testing the registry.
type mockProvider struct {
	id   ProviderID
	name string
}

func (m *mockProvider) ID() ProviderID           { return m.id }
func (m *mockProvider) Name() string             { return m.name }
func (m *mockProvider) UsesCallbackServer() bool { return true }

func (m *mockProvider) Login(ctx context.Context, callbacks LoginCallbacks) (*Credentials, error) {
	return &Credentials{}, nil
}

func (m *mockProvider) RefreshToken(ctx context.Context, credentials *Credentials) (*Credentials, error) {
	return credentials, nil
}

func (m *mockProvider) GetAPIKey(credentials *Credentials) string {
	return "mock-api-key"
}

func (m *mockProvider) ModifyModels(models []*airoot.Model, credentials *Credentials) []*airoot.Model {
	return models
}

type helperTestProvider struct {
	id               ProviderID
	loginCreds       *Credentials
	refreshCreds     *Credentials
	loginCalls       int
	refreshCalls     int
	lastRefreshToken string
	lastProjectID    string
}

func (p *helperTestProvider) ID() ProviderID           { return p.id }
func (p *helperTestProvider) Name() string             { return string(p.id) }
func (p *helperTestProvider) UsesCallbackServer() bool { return true }

func (p *helperTestProvider) Login(ctx context.Context, callbacks LoginCallbacks) (*Credentials, error) {
	p.loginCalls++
	return p.loginCreds, nil
}

func (p *helperTestProvider) RefreshToken(ctx context.Context, credentials *Credentials) (*Credentials, error) {
	p.refreshCalls++
	if credentials != nil {
		p.lastRefreshToken = credentials.Refresh
		p.lastProjectID = credentials.ProjectID
	}
	return p.refreshCreds, nil
}

func (p *helperTestProvider) GetAPIKey(credentials *Credentials) string {
	return ""
}

func (p *helperTestProvider) ModifyModels(models []*airoot.Model, credentials *Credentials) []*airoot.Model {
	return models
}

func TestGeneratePKCEUsesRFC7636S256Challenge(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if verifier == "" {
		t.Fatal("verifier is empty")
	}
	if got := oauth2.S256ChallengeFromVerifier(verifier); challenge != got {
		t.Fatalf("challenge = %q, want S256(verifier) %q", challenge, got)
	}
}

func TestListProviders(t *testing.T) {
	ResetProviders()

	providers := ListProviders()
	if len(providers) != 7 {
		t.Fatalf("ListProviders count = %d, want 7 built-in providers", len(providers))
	}

	// Check that all expected providers are present
	expectedIDs := map[ProviderID]bool{
		ProviderAnthropic:         true,
		ProviderGitHubCopilot:     true,
		ProviderGeminiCLI:         true,
		ProviderOpenAICodex:       true,
		ProviderGoogleAntigravity: true,
		ProviderRadius:            true,
		ProviderOpenRouter:        true,
	}

	for _, p := range providers {
		if !expectedIDs[p.ID()] {
			t.Errorf("ListProviders contains unexpected provider: %s", p.ID())
		}
	}
}

func TestGetProvider(t *testing.T) {
	ResetProviders()

	// Test getting existing provider
	p, ok := GetProvider(ProviderAnthropic)
	if !ok {
		t.Fatal("GetProvider(ProviderAnthropic) returned false, want true")
	}
	if p.ID() != ProviderAnthropic {
		t.Errorf("GetProvider(ProviderAnthropic).ID() = %s, want %s", p.ID(), ProviderAnthropic)
	}

	// Test getting non-existent provider
	_, ok = GetProvider("non-existent")
	if ok {
		t.Error("GetProvider(non-existent) returned true, want false")
	}
}

func TestRegister(t *testing.T) {
	ResetProviders()

	initialCount := len(ListProviders())

	// Register a custom provider
	Register(&mockProvider{id: "test-provider", name: "Test Provider"})

	providers := ListProviders()
	if len(providers) != initialCount+1 {
		t.Fatalf("ListProviders count after Register = %d, want %d", len(providers), initialCount+1)
	}

	// Verify the custom provider can be retrieved
	p, ok := GetProvider("test-provider")
	if !ok {
		t.Fatal("GetProvider(test-provider) returned false after Register")
	}
	if p.Name() != "Test Provider" {
		t.Errorf("GetProvider(test-provider).Name() = %q, want %q", p.Name(), "Test Provider")
	}
}

func TestUnregister(t *testing.T) {
	ResetProviders()

	// Register a custom provider
	Register(&mockProvider{id: "custom-unregister-test", name: "Custom Test"})

	// Verify it exists
	_, ok := GetProvider("custom-unregister-test")
	if !ok {
		t.Fatal("Custom provider not registered")
	}

	// Unregister the custom provider
	Unregister("custom-unregister-test")

	// Verify it's gone
	_, ok = GetProvider("custom-unregister-test")
	if ok {
		t.Error("Unregister did not remove custom provider")
	}
}

func TestUnregisterBuiltInRestoresDefault(t *testing.T) {
	ResetProviders()

	original, ok := GetProvider(ProviderAnthropic)
	if !ok {
		t.Fatal("Original Anthropic provider not found")
	}
	originalName := original.Name()

	// Unregister the built-in Anthropic provider
	Unregister(ProviderAnthropic)

	// It should be restored, not removed
	restored, ok := GetProvider(ProviderAnthropic)
	if !ok {
		t.Fatal("Unregister removed built-in provider instead of restoring")
	}
	if restored.Name() != originalName {
		t.Errorf("Unregister restored provider with different name: %q vs %q", restored.Name(), originalName)
	}
}

func TestResetProviders(t *testing.T) {
	ResetProviders()

	// Register several custom providers
	Register(&mockProvider{id: "custom-1", name: "Custom 1"})
	Register(&mockProvider{id: "custom-2", name: "Custom 2"})

	// Verify they're registered
	_, ok := GetProvider("custom-1")
	if !ok {
		t.Fatal("Custom provider 1 not registered")
	}

	// Reset
	ResetProviders()

	// Custom providers should be gone
	_, ok = GetProvider("custom-1")
	if ok {
		t.Error("ResetProviders did not remove custom providers")
	}
	_, ok = GetProvider("custom-2")
	if ok {
		t.Error("ResetProviders did not remove custom provider 2")
	}

	// Built-in providers should still be present
	_, ok = GetProvider(ProviderAnthropic)
	if !ok {
		t.Error("ResetProviders removed built-in Anthropic provider")
	}
}

func TestGetProviderInfoList(t *testing.T) {
	ResetProviders()

	infos := GetProviderInfoList()
	if len(infos) != 7 {
		t.Fatalf("GetProviderInfoList count = %d, want 7", len(infos))
	}

	for _, info := range infos {
		if info.ID == "" {
			t.Error("GetProviderInfoList contains entry with empty ID")
		}
		if info.Name == "" {
			t.Error("GetProviderInfoList contains entry with empty Name")
		}
		if !info.Available {
			t.Error("GetProviderInfoList contains entry with Available=false")
		}
	}
}

func TestOAuthBuiltInProviderOrder(t *testing.T) {
	ResetProviders()

	providers := ListProviders()
	got := make([]ProviderID, 0, len(providers))
	for _, provider := range providers {
		got = append(got, provider.ID())
	}

	want := []ProviderID{
		ProviderAnthropic,
		ProviderGitHubCopilot,
		ProviderGeminiCLI,
		ProviderGoogleAntigravity,
		ProviderOpenAICodex,
		ProviderRadius,
		ProviderOpenRouter,
	}

	if len(got) != len(want) {
		t.Fatalf("ListProviders() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListProviders()[%d] = %q, want %q; full order %v", i, got[i], want[i], got)
		}
	}
}

func TestProviderInfoType(t *testing.T) {
	info := ProviderInfo{
		ID:        ProviderAnthropic,
		Name:      "Anthropic",
		Available: true,
	}

	if info.ID != ProviderAnthropic {
		t.Errorf("ProviderInfo.ID = %s, want %s", info.ID, ProviderAnthropic)
	}
	if info.Name != "Anthropic" {
		t.Errorf("ProviderInfo.Name = %s, want %s", info.Name, "Anthropic")
	}
	if !info.Available {
		t.Error("ProviderInfo.Available = false, want true")
	}
}

func TestOAuthRegistryAliases(t *testing.T) {
	ResetOAuthProviders()

	providers := GetOAuthProviders()
	if len(providers) != len(ListProviders()) {
		t.Fatalf("GetOAuthProviders len = %d, want %d", len(providers), len(ListProviders()))
	}

	provider, ok := GetOAuthProvider(ProviderAnthropic)
	if !ok || provider.ID() != ProviderAnthropic {
		t.Fatal("GetOAuthProvider(ProviderAnthropic) failed")
	}
}

func TestGitHubCopilotHelperParity(t *testing.T) {
	if got := NormalizeDomain("github.example.com"); got != "github.example.com" {
		t.Fatalf("NormalizeDomain = %q", got)
	}

	got := GetGitHubCopilotBaseURL("tid=x;proxy-ep=proxy.individual.githubcopilot.com;exp=y", "")
	if got != "https://api.individual.githubcopilot.com" {
		t.Fatalf("GetGitHubCopilotBaseURL = %q", got)
	}
}

func TestOAuthHelperWrappers(t *testing.T) {
	origAnthropic := anthropicProviderFactory
	origCopilot := githubCopilotProviderFactory
	origGemini := geminiCLIProviderFactory
	origAntigravity := antigravityProviderFactory
	t.Cleanup(func() {
		anthropicProviderFactory = origAnthropic
		githubCopilotProviderFactory = origCopilot
		geminiCLIProviderFactory = origGemini
		antigravityProviderFactory = origAntigravity
	})

	type loginFunc func(context.Context, LoginCallbacks) (*Credentials, error)
	type refreshFunc func(context.Context, string) (*Credentials, error)

	tests := []struct {
		name       string
		setFactory func(*helperTestProvider)
		login      loginFunc
		refresh    refreshFunc
	}{
		{
			name: "anthropic",
			setFactory: func(p *helperTestProvider) {
				anthropicProviderFactory = func() Provider { return p }
			},
			login:   LoginAnthropic,
			refresh: RefreshAnthropicToken,
		},
		{
			name: "github-copilot",
			setFactory: func(p *helperTestProvider) {
				githubCopilotProviderFactory = func() Provider { return p }
			},
			login:   LoginGitHubCopilot,
			refresh: RefreshGitHubCopilotToken,
		},
		{
			name: "gemini-cli",
			setFactory: func(p *helperTestProvider) {
				geminiCLIProviderFactory = func() Provider { return p }
			},
			login: LoginGeminiCLI,
			refresh: func(ctx context.Context, refreshToken string) (*Credentials, error) {
				return RefreshGoogleCloudToken(ctx, refreshToken, "project-123")
			},
		},
		{
			name: "antigravity",
			setFactory: func(p *helperTestProvider) {
				antigravityProviderFactory = func() Provider { return p }
			},
			login:   LoginAntigravity,
			refresh: RefreshAntigravityToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &helperTestProvider{
				id:           ProviderID(tt.name),
				loginCreds:   &Credentials{Access: "login-token"},
				refreshCreds: &Credentials{Access: "refresh-token"},
			}
			tt.setFactory(provider)

			loginCreds, err := tt.login(context.Background(), &DefaultLoginCallbacks{})
			if err != nil {
				t.Fatalf("login wrapper: %v", err)
			}
			if loginCreds != provider.loginCreds {
				t.Fatalf("login wrapper returned %+v, want %+v", loginCreds, provider.loginCreds)
			}
			if provider.loginCalls != 1 {
				t.Fatalf("loginCalls = %d, want 1", provider.loginCalls)
			}

			refreshCreds, err := tt.refresh(context.Background(), "refresh-input")
			if err != nil {
				t.Fatalf("refresh wrapper: %v", err)
			}
			if refreshCreds != provider.refreshCreds {
				t.Fatalf("refresh wrapper returned %+v, want %+v", refreshCreds, provider.refreshCreds)
			}
			if provider.refreshCalls != 1 {
				t.Fatalf("refreshCalls = %d, want 1", provider.refreshCalls)
			}
			if provider.lastRefreshToken != "refresh-input" {
				t.Fatalf("lastRefreshToken = %q, want refresh-input", provider.lastRefreshToken)
			}
			if tt.name == "gemini-cli" && provider.lastProjectID != "project-123" {
				t.Fatalf("lastProjectID = %q, want project-123", provider.lastProjectID)
			}
		})
	}
}

func TestRefreshAntigravityTokenWithProjectID(t *testing.T) {
	oldFactory := antigravityProviderFactory
	defer func() { antigravityProviderFactory = oldFactory }()

	provider := &helperTestProvider{
		id:           ProviderGoogleAntigravity,
		refreshCreds: &Credentials{Access: "refresh-token"},
	}
	antigravityProviderFactory = func() Provider { return provider }

	_, err := RefreshAntigravityTokenWithProjectID(context.Background(), "refresh-input", "project-123")
	if err != nil {
		t.Fatalf("RefreshAntigravityTokenWithProjectID: %v", err)
	}
	if provider.lastRefreshToken != "refresh-input" {
		t.Fatalf("lastRefreshToken = %q, want refresh-input", provider.lastRefreshToken)
	}
	if provider.lastProjectID != "project-123" {
		t.Fatalf("lastProjectID = %q, want project-123", provider.lastProjectID)
	}
}

func TestGeminiCLIProviderParity(t *testing.T) {
	if ProviderGeminiCLI != "google-gemini-cli" {
		t.Fatalf("ProviderGeminiCLI = %q, want google-gemini-cli", ProviderGeminiCLI)
	}

	provider, ok := GetProvider("gemini-cli")
	if !ok {
		t.Fatal("legacy gemini-cli alias did not resolve")
	}
	if provider.ID() != ProviderGeminiCLI {
		t.Fatalf("legacy gemini-cli alias resolved to %q, want %q", provider.ID(), ProviderGeminiCLI)
	}
}

func TestGeminiCLIGetAPIKeyIncludesProjectID(t *testing.T) {
	provider := NewGeminiCLIProvider()
	apiKey := provider.GetAPIKey(&Credentials{
		Access:    "access-token",
		ProjectID: "project-123",
	})

	var got map[string]string
	if err := json.Unmarshal([]byte(apiKey), &got); err != nil {
		t.Fatalf("GetAPIKey returned invalid JSON: %v", err)
	}
	if got["token"] != "access-token" {
		t.Fatalf("token = %q, want access-token", got["token"])
	}
	if got["projectId"] != "project-123" {
		t.Fatalf("projectId = %q, want project-123", got["projectId"])
	}
}

func TestOAuthHTMLHelpers(t *testing.T) {
	success := OAuthSuccessHTML(`done <ok>`)
	if !strings.Contains(success, "Authentication successful") {
		t.Fatalf("OAuthSuccessHTML missing title/heading: %q", success)
	}
	if !strings.Contains(success, "done &lt;ok&gt;") {
		t.Fatalf("OAuthSuccessHTML did not escape content: %q", success)
	}

	failure := OAuthErrorHTML("failed", `<details>`)
	if !strings.Contains(failure, "Authentication failed") {
		t.Fatalf("OAuthErrorHTML missing title/heading: %q", failure)
	}
	if !strings.Contains(failure, "&lt;details&gt;") {
		t.Fatalf("OAuthErrorHTML did not escape details: %q", failure)
	}
}

// refreshableMockProvider is a test provider that tracks refresh calls.
type refreshableMockProvider struct {
	id                ProviderID
	name              string
	refreshCount      int
	refreshShouldFail bool
	returnedCreds     *Credentials
}

func (m *refreshableMockProvider) ID() ProviderID           { return m.id }
func (m *refreshableMockProvider) Name() string             { return m.name }
func (m *refreshableMockProvider) UsesCallbackServer() bool { return true }
func (m *refreshableMockProvider) GetAPIKey(credentials *Credentials) string {
	return "mock-api-key"
}

func (m *refreshableMockProvider) Login(ctx context.Context, callbacks LoginCallbacks) (*Credentials, error) {
	return &Credentials{}, nil
}

func (m *refreshableMockProvider) RefreshToken(ctx context.Context, credentials *Credentials) (*Credentials, error) {
	m.refreshCount++
	if m.refreshShouldFail {
		return nil, errors.New("refresh failed")
	}
	// Return updated credentials with extended expiry
	if m.returnedCreds != nil {
		return m.returnedCreds, nil
	}
	return &Credentials{
		Refresh: credentials.Refresh,
		Access:  "new-access-token",
		Expires: time.Now().UnixMilli() + 3600*1000, // 1 hour from now
	}, nil
}

func (m *refreshableMockProvider) ModifyModels(models []*airoot.Model, credentials *Credentials) []*airoot.Model {
	return models
}

func TestCredentialsStoreSetAndGet(t *testing.T) {
	store := NewCredentialsStore()

	// Initially empty
	_, ok := store.Get(ProviderAnthropic)
	if ok {
		t.Error("store.Get returned true for empty store")
	}

	// Set credentials
	creds := &Credentials{
		Refresh: "refresh-token",
		Access:  "access-token",
		Expires: time.Now().UnixMilli() + 3600*1000,
	}
	store.Set(ProviderAnthropic, creds)

	// Get should return them
	got, ok := store.Get(ProviderAnthropic)
	if !ok {
		t.Fatal("store.Get returned false after Set")
	}
	if got.Refresh != creds.Refresh {
		t.Errorf("store.Get().Refresh = %q, want %q", got.Refresh, creds.Refresh)
	}
}

func TestCredentialsStoreDelete(t *testing.T) {
	store := NewCredentialsStore()

	store.Set(ProviderAnthropic, &Credentials{
		Refresh: "refresh",
		Access:  "access",
		Expires: time.Now().UnixMilli() + 3600*1000,
	})

	// Delete
	store.Delete(ProviderAnthropic)

	// Should be gone
	_, ok := store.Get(ProviderAnthropic)
	if ok {
		t.Error("store.Get returned true after Delete")
	}
}

func TestCredentialsStoreGetAPIKeyNoCredentials(t *testing.T) {
	store := NewCredentialsStore()

	// Register a provider
	Register(&mockProvider{id: "test-no-creds", name: "Test No Creds"})

	// No credentials stored - should return nil, ""
	creds, apiKey, err := store.GetAPIKey(context.Background(), "test-no-creds")
	if err != nil {
		t.Errorf("GetAPIKey returned error: %v", err)
	}
	if creds != nil {
		t.Error("GetAPIKey returned credentials when none stored")
	}
	if apiKey != "" {
		t.Error("GetAPIKey returned non-empty apiKey when none stored")
	}
}

func TestCredentialsStoreGetAPIKeyWithValidCredentials(t *testing.T) {
	ResetProviders()
	store := NewCredentialsStore()

	// Register a mock provider that returns a specific API key
	Register(&mockProvider{id: "test-valid-creds", name: "Test Valid Creds"})

	// Store valid (non-expired) credentials
	store.Set("test-valid-creds", &Credentials{
		Refresh: "refresh-token",
		Access:  "access-token",
		Expires: time.Now().UnixMilli() + 3600*1000, // 1 hour from now
	})

	// GetAPIKey should return the api key without refresh
	creds, apiKey, err := store.GetAPIKey(context.Background(), "test-valid-creds")
	if err != nil {
		t.Errorf("GetAPIKey returned error: %v", err)
	}
	if creds == nil {
		t.Fatal("GetAPIKey returned nil credentials")
	}
	if apiKey != "mock-api-key" {
		t.Errorf("GetAPIKey apiKey = %q, want %q", apiKey, "mock-api-key")
	}
}

func TestCredentialsStoreGetAPIKeyAutoRefresh(t *testing.T) {
	ResetProviders()
	store := NewCredentialsStore()

	// Create a refreshable provider
	provider := &refreshableMockProvider{
		id:   "test-auto-refresh",
		name: "Test Auto Refresh",
	}
	Register(provider)

	// Store expired credentials
	store.Set("test-auto-refresh", &Credentials{
		Refresh: "old-refresh",
		Access:  "old-access",
		Expires: time.Now().UnixMilli() - 1000, // Expired 1 second ago
	})

	// GetAPIKey should trigger refresh
	_, apiKey, err := store.GetAPIKey(context.Background(), "test-auto-refresh")
	if err != nil {
		t.Errorf("GetAPIKey returned error: %v", err)
	}
	if provider.refreshCount != 1 {
		t.Errorf("refreshCount = %d, want 1", provider.refreshCount)
	}
	if apiKey != "mock-api-key" {
		t.Errorf("GetAPIKey apiKey = %q, want %q", apiKey, "mock-api-key")
	}

	// Credentials should be updated in store
	updated, ok := store.Get("test-auto-refresh")
	if !ok {
		t.Fatal("Credentials not updated in store")
	}
	if updated.Access != "new-access-token" {
		t.Errorf("Updated credentials.Access = %q, want %q", updated.Access, "new-access-token")
	}
}

func TestCredentialsStoreGetAPIKeyRefreshFailure(t *testing.T) {
	ResetProviders()
	store := NewCredentialsStore()

	// Create a provider that fails refresh
	provider := &refreshableMockProvider{
		id:                "test-refresh-fail",
		name:              "Test Refresh Fail",
		refreshShouldFail: true,
	}
	Register(provider)

	// Store expired credentials
	store.Set("test-refresh-fail", &Credentials{
		Refresh: "old-refresh",
		Access:  "old-access",
		Expires: time.Now().UnixMilli() - 1000,
	})

	// GetAPIKey should return error
	_, _, err := store.GetAPIKey(context.Background(), "test-refresh-fail")
	if err == nil {
		t.Error("GetAPIKey should return error when refresh fails")
	}
}

func TestCredentialsStoreGetAPIKeyUnknownProvider(t *testing.T) {
	store := NewCredentialsStore()

	// Unknown provider should return error
	_, _, err := store.GetAPIKey(context.Background(), "unknown-provider")
	if err == nil {
		t.Error("GetAPIKey should return error for unknown provider")
	}
}

func TestCredentialsStoreRefreshCredentialsNoStore(t *testing.T) {
	store := NewCredentialsStore()
	Register(&mockProvider{id: "test-no-store", name: "Test No Store"})

	// No credentials stored - should return error
	_, err := store.RefreshCredentials(context.Background(), "test-no-store")
	if err == nil {
		t.Error("RefreshCredentials should return error when no credentials stored")
	}
}

func TestCredentialsStoreGetAllProviders(t *testing.T) {
	store := NewCredentialsStore()

	// Initially empty
	providers := store.GetAllProviders()
	if len(providers) != 0 {
		t.Errorf("GetAllProviders() count = %d, want 0", len(providers))
	}

	// Add some credentials
	store.Set(ProviderAnthropic, &Credentials{})
	store.Set(ProviderGeminiCLI, &Credentials{})

	providers = store.GetAllProviders()
	if len(providers) != 2 {
		t.Errorf("GetAllProviders() count = %d, want 2", len(providers))
	}

	found := map[ProviderID]bool{}
	for _, p := range providers {
		found[p] = true
	}
	if !found[ProviderAnthropic] || !found[ProviderGeminiCLI] {
		t.Errorf("GetAllProviders missing providers: %v", found)
	}
}

func TestDefaultStoreGetOAuthAPIKey(t *testing.T) {
	// Reset and clear any stored credentials
	ResetProviders()
	DeleteOAuthCredentials("test-default-oauth")

	// Register a test provider
	Register(&mockProvider{id: "test-default-oauth", name: "Test Default OAuth"})

	// No credentials - should return nil
	creds, apiKey, err := GetOAuthAPIKey(context.Background(), "test-default-oauth")
	if err != nil {
		t.Errorf("GetOAuthAPIKey returned error: %v", err)
	}
	if creds != nil || apiKey != "" {
		t.Errorf("GetOAuthAPIKey = (%v, %q), want (nil, \"\")", creds, apiKey)
	}

	// Set credentials
	testCreds := &Credentials{
		Refresh: "test-refresh",
		Access:  "test-access",
		Expires: time.Now().UnixMilli() + 3600*1000,
	}
	SetOAuthCredentials("test-default-oauth", testCreds)

	// Now should return the api key (mock provider returns "mock-api-key")
	_, apiKey, err = GetOAuthAPIKey(context.Background(), "test-default-oauth")
	if err != nil {
		t.Errorf("GetOAuthAPIKey returned error: %v", err)
	}
	if apiKey != "mock-api-key" {
		t.Errorf("GetOAuthAPIKey apiKey = %q, want %q", apiKey, "mock-api-key")
	}
}

func TestDefaultStoreGetOAuthCredentials(t *testing.T) {
	ResetProviders()
	DeleteOAuthCredentials(ProviderAnthropic)

	// Initially nil
	_, ok := GetOAuthCredentials(ProviderAnthropic)
	if ok {
		t.Error("GetOAuthCredentials returned true when none stored")
	}

	// Set and retrieve
	testCreds := &Credentials{
		Refresh: "test-refresh",
		Access:  "test-access",
		Expires: time.Now().UnixMilli() + 3600*1000,
	}
	SetOAuthCredentials(ProviderAnthropic, testCreds)

	got, ok := GetOAuthCredentials(ProviderAnthropic)
	if !ok {
		t.Fatal("GetOAuthCredentials returned false after SetOAuthCredentials")
	}
	if got.Access != "test-access" {
		t.Errorf("GetOAuthCredentials().Access = %q, want %q", got.Access, "test-access")
	}
}

func TestDefaultStoreDeleteOAuthCredentials(t *testing.T) {
	ResetProviders()

	// Set credentials
	SetOAuthCredentials(ProviderAnthropic, &Credentials{
		Refresh: "test",
		Access:  "test",
		Expires: time.Now().UnixMilli() + 3600*1000,
	})

	// Delete
	DeleteOAuthCredentials(ProviderAnthropic)

	// Should be gone
	_, ok := GetOAuthCredentials(ProviderAnthropic)
	if ok {
		t.Error("GetOAuthCredentials returned true after DeleteOAuthCredentials")
	}
}

func TestDefaultStoreRefreshOAuthCredentials(t *testing.T) {
	ResetProviders()
	DeleteOAuthCredentials("test-refresh-oauth")

	// Register a test provider
	Register(&mockProvider{id: "test-refresh-oauth", name: "Test Refresh OAuth"})

	// No credentials stored
	_, err := RefreshOAuthCredentials(context.Background(), "test-refresh-oauth")
	if err == nil {
		t.Error("RefreshOAuthCredentials should return error when no credentials")
	}

	// Set expired credentials
	SetOAuthCredentials("test-refresh-oauth", &Credentials{
		Refresh: "old-refresh",
		Access:  "old-access",
		Expires: time.Now().UnixMilli() - 1000,
	})

	// Refresh should succeed (mock provider refreshes)
	creds, err := RefreshOAuthCredentials(context.Background(), "test-refresh-oauth")
	if err != nil {
		t.Errorf("RefreshOAuthCredentials returned error: %v", err)
	}
	if creds == nil {
		t.Fatal("RefreshOAuthCredentials returned nil credentials")
	}
}

func TestDefaultStoreRefreshOAuthCredentialsNoProvider(t *testing.T) {
	ResetProviders()
	DeleteOAuthCredentials(ProviderAnthropic)

	// Set credentials but unregister provider
	SetOAuthCredentials("nonexistent-provider", &Credentials{
		Refresh: "test",
		Access:  "test",
		Expires: time.Now().UnixMilli() + 3600*1000,
	})

	// Should fail because provider doesn't exist
	_, err := RefreshOAuthCredentials(context.Background(), "nonexistent-provider")
	if err == nil {
		t.Error("RefreshOAuthCredentials should return error for unknown provider")
	}
}

func TestCallbackServerFallsBackToLoopbackPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}()
	occupied := listener.Addr().(*net.TCPAddr).Port
	server := newCallbackServer("expected-state", occupied)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	}()
	if server.Port() == occupied {
		t.Fatal("did not select a fallback port")
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.AuthCallbackURL()+"?code=code&state=expected-state", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	code, state, err := server.WaitForCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if code != "code" || state != "expected-state" {
		t.Fatalf("code=%q state=%q", code, state)
	}
}

func TestCallbackServerRejectsStraysWithoutConsumingLogin(t *testing.T) {
	for _, query := range []string{"code=wrong&state=stray", "error=denied&state=stray", "state=expected", "code=wrong", ""} {
		t.Run(query, func(t *testing.T) {
			server := newCallbackServer("expected", 0)
			for _, method := range []string{http.MethodPost, http.MethodGet} {
				request := httptest.NewRequest(method, "/auth/callback?"+query, nil)
				response := httptest.NewRecorder()
				server.handleCallback(response, request)
				if response.Code < 400 || server.handled {
					t.Fatalf("stray %s accepted or consumed login: status=%d handled=%v", method, response.Code, server.handled)
				}
			}
			response := httptest.NewRecorder()
			server.handleCallback(response, httptest.NewRequest(http.MethodGet, "/auth/callback?code=valid&state=expected", nil))
			code, state, err := server.WaitForCode(t.Context())
			if err != nil || code != "valid" || state != "expected" || response.Code != http.StatusOK {
				t.Fatalf("valid callback: code=%q state=%q err=%v status=%d", code, state, err, response.Code)
			}
			response = httptest.NewRecorder()
			server.handleCallback(response, httptest.NewRequest(http.MethodGet, "/auth/callback?code=again&state=expected", nil))
			if response.Code != http.StatusConflict {
				t.Fatalf("duplicate status=%d", response.Code)
			}
		})
	}
}

func TestCallbackServerValidatesStateBeforeOAuthError(t *testing.T) {
	server := newCallbackServer("expected", 0)
	response := httptest.NewRecorder()
	server.handleCallback(response, httptest.NewRequest(http.MethodGet, "/callback?error=denied&error_description=declined&state=expected", nil))
	_, _, err := server.WaitForCode(t.Context())
	if err == nil || !strings.Contains(err.Error(), "declined") || response.Code != http.StatusBadRequest {
		t.Fatalf("error=%v status=%d", err, response.Code)
	}
}

func TestWaitForCallbackOrManualInputWithdrawsTheLosingSide(t *testing.T) {
	blocked := func(started chan<- struct{}, ended chan<- error) func(context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			close(started)
			<-ctx.Done()
			ended <- ctx.Err()
			return "", ctx.Err()
		}
	}

	t.Run("callback wins", func(t *testing.T) {
		started, ended := make(chan struct{}), make(chan error, 1)
		prompt := blocked(started, ended)
		callbacks := &DefaultLoginCallbacks{FnManualCodeInput: prompt}
		code, input, err := waitForCallbackOrManualInput(t.Context(), callbacks, func(context.Context) (string, error) {
			<-started
			return "abc", nil
		})
		if err != nil || code != "abc" || input != "" {
			t.Fatalf("result = %q, %q, %v", code, input, err)
		}
		select {
		case err := <-ended:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("prompt ended with %v", err)
			}
		default:
			t.Fatal("prompt still pending after the callback won")
		}
	})

	t.Run("manual wins", func(t *testing.T) {
		started, ended := make(chan struct{}), make(chan error, 1)
		wait := blocked(started, ended)
		callbacks := &DefaultLoginCallbacks{FnManualCodeInput: func(context.Context) (string, error) {
			<-started
			return "pasted", nil
		}}
		code, input, err := waitForCallbackOrManualInput(t.Context(), callbacks, wait)
		if err != nil || code != "" || input != "pasted" {
			t.Fatalf("result = %q, %q, %v", code, input, err)
		}
		select {
		case <-ended:
		default:
			t.Fatal("callback wait still pending after manual input won")
		}
	})

	t.Run("unavailable prompt leaves the callback", func(t *testing.T) {
		release := make(chan struct{})
		callbacks := &DefaultLoginCallbacks{FnManualCodeInput: func(context.Context) (string, error) {
			defer close(release)
			return "", fmt.Errorf("no terminal: %w", ErrPromptUnavailable)
		}}
		code, _, err := waitForCallbackOrManualInput(t.Context(), callbacks, func(context.Context) (string, error) {
			<-release
			return "abc", nil
		})
		if err != nil || code != "abc" {
			t.Fatalf("result = %q, %v", code, err)
		}
		unavailable := &DefaultLoginCallbacks{FnManualCodeInput: func(context.Context) (string, error) { return "", ErrPromptUnavailable }}
		if _, _, err := waitForCallbackOrManualInput(t.Context(), unavailable, nil); !errors.Is(err, ErrPromptUnavailable) {
			t.Fatalf("manual-only unavailable prompt err = %v", err)
		}
	})

	t.Run("prompt failure fails the login", func(t *testing.T) {
		promptErr := errors.New("prompt broke")
		callbacks := &DefaultLoginCallbacks{FnManualCodeInput: func(context.Context) (string, error) { return "", promptErr }}
		_, _, err := waitForCallbackOrManualInput(t.Context(), callbacks, func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		})
		if !errors.Is(err, promptErr) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		wait := func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }
		callbacks := &DefaultLoginCallbacks{FnManualCodeInput: wait}
		if _, _, err := waitForCallbackOrManualInput(ctx, callbacks, wait); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLineReaderCancelsAWaitWithoutLosingTheNextLine(t *testing.T) {
	pr, pw := io.Pipe()
	reader := &lineReader{r: pr}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read err = %v", err)
	}
	go func() {
		_, err := io.WriteString(pw, "first\nsecond\n")
		pw.CloseWithError(errors.Join(err, io.EOF))
	}()
	for _, want := range []string{"first", "second"} {
		line, err := reader.read(t.Context())
		if err != nil || line != want {
			t.Fatalf("read = %q, %v; want %q", line, err, want)
		}
	}
	for range 2 {
		if _, err := reader.read(t.Context()); !errors.Is(err, io.EOF) {
			t.Fatalf("read after close err = %v", err)
		}
	}
}
