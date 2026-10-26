// Package store provides persistent credential storage for tau providers.
// It defines the CredentialStore interface and ships file-backed and in-memory
// implementations with refresh-on-read orchestration.
package store // import "github.com/losinggeneration/tau/ai/store"

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
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
		return maps.Clone(o.RefreshFuncs)
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
			return refreshedOAuthCredential(refreshed, cred)
		}
	}
	return funcs
}

// APIKeyFromCredential converts a stored credential into the bearer/API key
// expected by the given provider. OAuth credentials are delegated to the
// provider's GetAPIKey hook so provider-specific formats stay centralized.
func APIKeyFromCredential(ctx context.Context, provider string, cred ai.Credential) (string, error) {
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
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
	refreshMu    sync.Map // per-key refresh admission
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
	if err := context.Cause(ctx); err != nil {
		return ai.Credential{}, err
	}
	key := credKey(provider)

	m.mu.RLock()
	cred, ok := m.creds[key]
	cred = cloneMemoryCredential(cred)
	m.mu.RUnlock()

	if !ok {
		return ai.Credential{}, fmt.Errorf("%w: %s", ai.ErrCredentialNotFound, key)
	}

	// Check if refresh is needed
	if !cred.IsExpired(m.margin) || !cred.IsRefreshable() {
		return cred, nil
	}

	// Serialize refresh for the same key
	release, err := acquireRefresh(ctx, &m.refreshMu, key)
	if err != nil {
		return ai.Credential{}, err
	}
	defer release()

	// Re-read after acquiring lock (another goroutine may have refreshed)
	m.mu.RLock()
	cred, ok = m.creds[key]
	cred = cloneMemoryCredential(cred)
	m.mu.RUnlock()
	if !ok {
		return ai.Credential{}, fmt.Errorf("%w: %s", ai.ErrCredentialNotFound, key)
	}
	if !cred.IsExpired(m.margin) || !cred.IsRefreshable() {
		return cred, nil
	}

	// Perform refresh
	refreshFn, hasRefresh := m.refreshFuncs[key]
	if !hasRefresh {
		return cred, nil // no refresh function, return as-is
	}

	expected := authFile{key: credentialToAuthCredential(cred)}.clone()[key]
	refreshed, err := refreshFn(ctx, cred)
	if cause := context.Cause(ctx); cause != nil {
		return cred, errors.Join(cause, err)
	}
	if err != nil {
		if isErrCredentialNeedsReauth(err) {
			cred.NeedsReauth = true
			if writeErr := m.writeRefreshedCredential(ctx, key, expected, cred); writeErr != nil {
				return cred, errors.Join(err, writeErr)
			}
			return cred, err
		}
		return cred, fmt.Errorf("refresh %s: %w", key, err)
	}

	if err := m.writeRefreshedCredential(ctx, key, expected, refreshed); err != nil {
		return refreshed, err
	}

	return refreshed, nil
}

// Set stores a credential.
func (m *MemoryBackend) Set(ctx context.Context, provider string, cred ai.Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m.creds[credKey(provider)] = cloneMemoryCredential(cred)
	return nil
}

// Delete removes a credential.
func (m *MemoryBackend) Delete(ctx context.Context, provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	delete(m.creds, credKey(provider))
	return nil
}

// List returns all stored credential references.
func (m *MemoryBackend) List(ctx context.Context) ([]ai.CredentialRef, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}

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
	if err := context.Cause(ctx); err != nil {
		return ai.Credential{}, err
	}
	key := credKey(provider)

	m.mu.RLock()
	cred, ok := m.creds[key]
	cred = cloneMemoryCredential(cred)
	m.mu.RUnlock()

	if !ok {
		return ai.Credential{}, fmt.Errorf("%w: %s", ai.ErrCredentialNotFound, key)
	}

	if cred.Type != ai.CredentialTypeOAuth {
		return cred, fmt.Errorf("credential %s is not OAuth", key)
	}

	// Serialize refresh for the same key
	release, err := acquireRefresh(ctx, &m.refreshMu, key)
	if err != nil {
		return ai.Credential{}, err
	}
	defer release()

	// Re-read after acquiring lock
	m.mu.RLock()
	cred, ok = m.creds[key]
	cred = cloneMemoryCredential(cred)
	m.mu.RUnlock()
	if !ok {
		return ai.Credential{}, fmt.Errorf("%w: %s", ai.ErrCredentialNotFound, key)
	}

	if cred.Type != ai.CredentialTypeOAuth {
		return cred, fmt.Errorf("credential %s is not OAuth", key)
	}

	// Convert to oauth.Credentials
	expected := authFile{key: credentialToAuthCredential(cred)}.clone()[key]
	oauthCreds := OAuthCredentialToCredentials(cred)

	// Refresh
	refreshed, err := oauthProvider.RefreshToken(ctx, oauthCreds)
	if cause := context.Cause(ctx); cause != nil {
		return cred, errors.Join(cause, err)
	}
	if err != nil {
		// Check for terminal error
		if isErrCredentialNeedsReauth(err) {
			cred.NeedsReauth = true
			if writeErr := m.writeRefreshedCredential(ctx, key, expected, cred); writeErr != nil {
				return cred, errors.Join(err, writeErr)
			}
			return cred, errors.Join(err, ai.ErrCredentialNeedsReauth{Provider: provider})
		}
		return cred, fmt.Errorf("refresh %s: %w", key, err)
	}

	// Convert back and store
	cred, err = refreshedOAuthCredential(refreshed, cred)
	if err != nil {
		return ai.Credential{}, err
	}
	if err := m.writeRefreshedCredential(ctx, key, expected, cred); err != nil {
		return cred, err
	}

	return cred, nil
}

func (m *MemoryBackend) writeRefreshedCredential(ctx context.Context, key string, expected authCredential, refreshed ai.Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	current, exists := m.creds[key]
	if !exists || !reflect.DeepEqual(credentialToAuthCredential(current), expected) {
		return errors.New("credential changed during refresh")
	}
	m.creds[key] = cloneMemoryCredential(refreshed)
	return nil
}

func refreshedOAuthCredential(creds *oauth.Credentials, previous ai.Credential) (ai.Credential, error) {
	if creds == nil {
		return ai.Credential{}, fmt.Errorf("OAuth refresh returned missing credentials")
	}
	if creds.Access == "" {
		return ai.Credential{}, fmt.Errorf("OAuth refresh returned no access token")
	}
	cred := CredentialFromOAuth(creds)
	cred.Scope = previous.Scope
	if previous.Extra != nil {
		extra := cloneAuthJSON(previous.Extra).(map[string]any)
		for key, value := range cred.Extra {
			extra[key] = cloneAuthJSON(value)
		}
		cred.Extra = extra
	}
	if cred.RefreshToken == "" {
		cred.RefreshToken = previous.RefreshToken
	}
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

func cloneMemoryCredential(cred ai.Credential) ai.Credential {
	cred.Env = maps.Clone(cred.Env)
	if cred.Extra != nil {
		cred.Extra = cloneAuthJSON(cred.Extra).(map[string]any)
	}
	return cred
}
