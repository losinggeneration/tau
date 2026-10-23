// Package store provides persistent credential storage for tau providers.
// It defines the CredentialStore interface and ships file-backed and in-memory
// implementations with refresh-on-read orchestration.
package store // import "github.com/losinggeneration/tau/ai/store"

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/oauth"
)

// RefreshFunc refreshes an OAuth credential. Returns a fresh credential
// or an error. Terminal failures (invalid_grant, revoked token) should
// be wrapped in ai.ErrCredentialNeedsReauth.
type RefreshFunc func(ctx context.Context, cred ai.Credential) (ai.Credential, error)

// CredentialStore is the interface for credential storage.
type CredentialStore interface {
	// Get retrieves a credential. If the credential is OAuth and within
	// the expiry margin, Get refreshes it synchronously, persists the
	// result, and returns the fresh value.
	Get(ctx context.Context, provider string) (ai.Credential, error)
	// Set stores a credential.
	Set(ctx context.Context, provider string, cred ai.Credential) error
	// Delete removes a credential.
	Delete(ctx context.Context, provider string) error
	// List returns all stored credential references.
	List(ctx context.Context) ([]ai.CredentialRef, error)
	// RefreshOAuth refreshes an OAuth credential using the provided
	// oauth.Provider and returns the updated credential.
	RefreshOAuth(ctx context.Context, provider string, oauthProvider oauth.Provider) (ai.Credential, error)
}

// Options configures the credential store behavior.
type Options struct {
	// ExpiryMargin is how far before actual expiry a token should be
	// refreshed. Defaults to 5 minutes.
	ExpiryMargin time.Duration
	// RefreshFuncs maps provider names to their refresh functions.
	// If a provider has no refresh function, its OAuth credentials
	// cannot be refreshed.
	RefreshFuncs map[string]RefreshFunc
	// OAuthProviders maps provider names to their OAuth providers
	// for credential refresh support.
	OAuthProviders map[string]oauth.Provider
}

func (o Options) expiryMargin() time.Duration {
	if o.ExpiryMargin > 0 {
		return o.ExpiryMargin
	}
	return 5 * time.Minute
}

func (o Options) refreshFuncs() map[string]RefreshFunc {
	if len(o.RefreshFuncs) > 0 {
		return o.RefreshFuncs
	}
	if len(o.OAuthProviders) == 0 {
		return nil
	}

	providers := make([]oauth.Provider, 0, len(o.OAuthProviders))
	for _, provider := range o.OAuthProviders {
		providers = append(providers, provider)
	}
	return OAuthProviderRefreshFuncs(providers...)
}

// OAuthProviderRefreshFuncs adapts OAuth providers to CredentialStore refresh
// callbacks. If no providers are passed, all registered OAuth providers are
// used.
func OAuthProviderRefreshFuncs(providers ...oauth.Provider) map[string]RefreshFunc {
	if len(providers) == 0 {
		providers = oauth.ListProviders()
	}

	funcs := make(map[string]RefreshFunc, len(providers))
	for _, provider := range providers {
		if provider == nil {
			continue
		}
		provider := provider
		providerID := string(provider.ID())
		funcs[providerID] = func(ctx context.Context, cred ai.Credential) (ai.Credential, error) {
			refreshed, err := provider.RefreshToken(ctx, OAuthCredentialToCredentials(cred))
			if err != nil {
				return ai.Credential{}, err
			}
			return CredentialFromOAuth(refreshed), nil
		}
	}
	return funcs
}

// APIKeyFromCredential converts a stored credential into the bearer/API key
// expected by the given provider. OAuth credentials are delegated to the
// provider's GetAPIKey hook so provider-specific formats stay centralized.
func APIKeyFromCredential(ctx context.Context, provider string, cred ai.Credential) (string, error) {
	switch cred.Type {
	case ai.CredentialTypeAPIKey:
		if cred.APIKey == "" {
			return "", nil
		}
		key, err := ai.ResolveConfigValue(ctx, cred.APIKey, cred.Env)
		if err != nil {
			return "", fmt.Errorf("resolve stored API key for %s: %w", provider, err)
		}
		return key, nil
	case ai.CredentialTypeOAuth:
		oauthProvider, ok := oauth.GetProvider(oauth.ProviderID(provider))
		if !ok {
			return "", fmt.Errorf("no OAuth provider for %s", provider)
		}
		return oauthProvider.GetAPIKey(OAuthCredentialToCredentials(cred)), nil
	case ai.CredentialTypeNone:
		return "", nil
	default:
		return "", fmt.Errorf("unknown credential type %q", cred.Type)
	}
}

// MemoryBackend is an in-memory CredentialStore for testing.
type MemoryBackend struct {
	mu           sync.RWMutex
	creds        map[string]ai.Credential // key = provider
	refreshFuncs map[string]RefreshFunc
	margin       time.Duration
	refreshMu    sync.Map // per-key mutex for refresh serialization
}

// NewMemoryBackend creates a new in-memory credential store.
func NewMemoryBackend(opts Options) *MemoryBackend {
	return &MemoryBackend{
		creds:        make(map[string]ai.Credential),
		refreshFuncs: opts.refreshFuncs(),
		margin:       opts.expiryMargin(),
	}
}

// credKey returns the storage key for a provider. Flat auth storage is
// keyed by provider only — there is no per-account dimension.
func credKey(provider string) string {
	if provider == "azure-openai-responses" {
		return ai.ProviderAzureOpenAI
	}
	return provider
}

// Get retrieves a credential, refreshing if needed.
func (m *MemoryBackend) Get(ctx context.Context, provider string) (ai.Credential, error) {
	key := credKey(provider)

	m.mu.RLock()
	cred, ok := m.creds[key]
	m.mu.RUnlock()

	if !ok {
		return ai.Credential{}, fmt.Errorf("%w: %s", ai.ErrCredentialNotFound, key)
	}

	// Check if refresh is needed
	if !cred.IsExpired(m.margin) || !cred.IsRefreshable() {
		return cred, nil
	}

	// Serialize refresh for the same key
	keyMu, _ := m.refreshMu.LoadOrStore(key, &sync.Mutex{})
	keyMu.(*sync.Mutex).Lock()
	defer keyMu.(*sync.Mutex).Unlock()

	// Re-read after acquiring lock (another goroutine may have refreshed)
	m.mu.RLock()
	cred, ok = m.creds[key]
	m.mu.RUnlock()
	if !ok {
		return ai.Credential{}, fmt.Errorf("%w: %s", ai.ErrCredentialNotFound, key)
	}
	if !cred.IsExpired(m.margin) {
		return cred, nil
	}

	// Perform refresh
	refreshFn, hasRefresh := m.refreshFuncs[provider]
	if !hasRefresh {
		return cred, nil // no refresh function, return as-is
	}

	refreshed, err := refreshFn(ctx, cred)
	if err != nil {
		if isErrCredentialNeedsReauth(err) {
			cred.NeedsReauth = true
			m.mu.Lock()
			m.creds[key] = cred
			m.mu.Unlock()
			return cred, err
		}
		return cred, fmt.Errorf("refresh %s: %w", key, err)
	}

	m.mu.Lock()
	m.creds[key] = refreshed
	m.mu.Unlock()

	return refreshed, nil
}

// Set stores a credential.
func (m *MemoryBackend) Set(_ context.Context, provider string, cred ai.Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creds[credKey(provider)] = cred
	return nil
}

// Delete removes a credential.
func (m *MemoryBackend) Delete(_ context.Context, provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.creds, credKey(provider))
	return nil
}

// List returns all stored credential references.
func (m *MemoryBackend) List(_ context.Context) ([]ai.CredentialRef, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	refs := make([]ai.CredentialRef, 0, len(m.creds))
	for provider, cred := range m.creds {
		refs = append(refs, ai.CredentialRef{
			Provider:    provider,
			Type:        cred.Type,
			NeedsReauth: cred.NeedsReauth,
		})
	}
	return refs, nil
}

// RefreshOAuth refreshes an OAuth credential using the provided oauth.Provider.
func (m *MemoryBackend) RefreshOAuth(ctx context.Context, provider string, oauthProvider oauth.Provider) (ai.Credential, error) {
	key := credKey(provider)

	m.mu.RLock()
	cred, ok := m.creds[key]
	m.mu.RUnlock()

	if !ok {
		return ai.Credential{}, fmt.Errorf("%w: %s", ai.ErrCredentialNotFound, key)
	}

	if cred.Type != ai.CredentialTypeOAuth {
		return cred, fmt.Errorf("credential %s is not OAuth", key)
	}

	// Serialize refresh for the same key
	keyMu, _ := m.refreshMu.LoadOrStore(key, &sync.Mutex{})
	keyMu.(*sync.Mutex).Lock()
	defer keyMu.(*sync.Mutex).Unlock()

	// Re-read after acquiring lock
	m.mu.RLock()
	cred, ok = m.creds[key]
	m.mu.RUnlock()
	if !ok {
		return ai.Credential{}, fmt.Errorf("%w: %s", ai.ErrCredentialNotFound, key)
	}

	// Convert to oauth.Credentials
	oauthCreds := OAuthCredentialToCredentials(cred)

	// Refresh
	refreshed, err := oauthProvider.RefreshToken(ctx, oauthCreds)
	if err != nil {
		// Check for terminal error
		if isErrCredentialNeedsReauth(err) {
			cred.NeedsReauth = true
			m.mu.Lock()
			m.creds[key] = cred
			m.mu.Unlock()
			return cred, ai.ErrCredentialNeedsReauth{Provider: provider}
		}
		return cred, fmt.Errorf("refresh %s: %w", key, err)
	}

	// Convert back and store
	cred = CredentialFromOAuth(refreshed)
	m.mu.Lock()
	m.creds[key] = cred
	m.mu.Unlock()

	return cred, nil
}

// CredentialFromOAuth converts an OAuth credentials object to an ai.Credential.
func CredentialFromOAuth(creds *oauth.Credentials) ai.Credential {
	if creds == nil {
		return ai.Credential{}
	}
	cred := ai.NewOAuthCredential(
		creds.Access,
		creds.Refresh,
		time.UnixMilli(creds.Expires),
		"", // OAuth credentials don't store scope
	)
	cred.ProjectID = creds.ProjectID
	cred.Email = creds.Email
	cred.AccountID = creds.AccountID
	cred.Extra = creds.Extra
	return cred
}

// OAuthCredentialToCredentials converts an ai.Credential (OAuth type) to oauth.Credentials.
func OAuthCredentialToCredentials(cred ai.Credential) *oauth.Credentials {
	return &oauth.Credentials{
		Access:    cred.AccessToken,
		Refresh:   cred.RefreshToken,
		Expires:   cred.ExpiresAt.UnixMilli(),
		ProjectID: cred.ProjectID,
		Email:     cred.Email,
		AccountID: cred.AccountID,
		Extra:     cred.Extra,
	}
}
