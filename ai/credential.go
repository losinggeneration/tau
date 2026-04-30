package ai // import "github.com/losinggeneration/tau/ai"

import (
	"errors"
	"fmt"
	"time"
)

// CredentialType identifies the kind of credential.
type CredentialType string

const (
	// CredentialTypeAPIKey is a static API key.
	CredentialTypeAPIKey CredentialType = "api_key"
	// CredentialTypeOAuth is an OAuth token pair with refresh capability.
	CredentialTypeOAuth CredentialType = "oauth"
	// CredentialTypeNone represents no credential (unauthenticated providers).
	CredentialTypeNone CredentialType = "none"
)

// Credential is a value union type for provider credentials.
// The Type field determines which field is populated.
type Credential struct {
	// Type identifies the credential kind.
	Type CredentialType `json:"type"`

	// APIKey holds the key when Type == CredentialTypeAPIKey.
	APIKey string `json:"key,omitempty"`

	// OAuth fields are populated when Type == CredentialTypeOAuth.
	AccessToken  string         `json:"access_token,omitempty"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time      `json:"expires_at,omitempty"`
	Scope        string         `json:"scope,omitempty"`
	ProjectID    string         `json:"projectId,omitempty"`
	Email        string         `json:"email,omitempty"`
	AccountID    string         `json:"accountId,omitempty"`
	Extra        map[string]any `json:"extra,omitempty"`

	// NeedsReauth is set when a terminal refresh failure occurred.
	// The credential is preserved but marked so the user can be prompted
	// to re-authenticate.
	NeedsReauth bool `json:"needs_reauth,omitempty"`
}

// NewAPIKeyCredential creates an API key credential.
func NewAPIKeyCredential(key string) Credential {
	return Credential{Type: CredentialTypeAPIKey, APIKey: key}
}

// NewOAuthCredential creates an OAuth credential.
func NewOAuthCredential(accessToken, refreshToken string, expiresAt time.Time, scope string) Credential {
	return Credential{
		Type:         CredentialTypeOAuth,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		Scope:        scope,
	}
}

// NewNoneCredential creates a no-credential sentinel.
func NewNoneCredential() Credential {
	return Credential{Type: CredentialTypeNone}
}

// IsExpired reports whether the credential is within the expiry margin.
// Non-OAuth credentials never expire.
func (c Credential) IsExpired(margin time.Duration) bool {
	if c.Type != CredentialTypeOAuth {
		return false
	}
	return !c.ExpiresAt.IsZero() && time.Now().Add(margin).After(c.ExpiresAt)
}

// IsRefreshable reports whether the credential supports refresh.
func (c Credential) IsRefreshable() bool {
	return c.Type == CredentialTypeOAuth && c.RefreshToken != ""
}

// ErrCredentialNeedsReauth is returned when a credential has suffered a
// terminal refresh failure and the user needs to re-authenticate.
type ErrCredentialNeedsReauth struct {
	Provider string
}

func (e ErrCredentialNeedsReauth) Error() string {
	return fmt.Sprintf(
		"credential %s needs re-auth: run `tau login %s`",
		e.Provider, e.Provider,
	)
}

// ErrCredentialNotFound is returned by credential stores when a provider has
// no stored credential. Callers use this to distinguish the "user hasn't run
// `tau login` for this provider" case (where environment-variable fallback is
// appropriate) from real store errors (decryption failure, file corruption,
// etc.) that should be surfaced.
var ErrCredentialNotFound = errors.New("credential not found")

// Is reports whether the target error matches ErrCredentialNeedsReauth.
func (ErrCredentialNeedsReauth) Is(target error) bool {
	var err ErrCredentialNeedsReauth
	return errors.As(target, &err)
}

// CredentialRef is a reference to a stored credential.
type CredentialRef struct {
	Provider string `json:"provider"`

	// Type and NeedsReauth are optional snapshot metadata populated by
	// CredentialStore.List implementations that can read credential details
	// without triggering refresh. Callers that only need provider references can
	// ignore them; callers that need an auth snapshot can use them to avoid
	// treating no-auth or reauth-needed entries as currently authenticated.
	Type        CredentialType `json:"type,omitempty"`
	NeedsReauth bool           `json:"needs_reauth,omitempty"`
}
