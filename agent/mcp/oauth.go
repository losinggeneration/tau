package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const (
	// refreshSkew refreshes access tokens this close to expiry before sending them.
	refreshSkew = 30 * time.Second
	// oauthRequestTimeout bounds each request to an authorization server.
	oauthRequestTimeout = 15 * time.Second
	// refreshLockWait outlasts another process's refresh.
	refreshLockWait = 25 * time.Second
)

// ErrNeedsAuth reports a server that needs the user to sign in with /mcp.
var ErrNeedsAuth = errors.New("MCP server requires sign-in")

// Credential is one server's OAuth client registration and tokens.
type Credential struct {
	ClientID     string           `json:"clientId,omitempty"`
	ClientSecret string           `json:"clientSecret,omitempty"`
	AuthStyle    oauth2.AuthStyle `json:"authStyle,omitempty"`
	TokenURL     string           `json:"tokenUrl,omitempty"`
	RedirectURL  string           `json:"redirectUrl,omitempty"`
	Scopes       []string         `json:"scopes,omitempty"`
	Token        *oauth2.Token    `json:"tokens,omitempty"`
}

// CredentialStore keeps MCP OAuth credentials in mcp-auth.json, keyed by
// server name and URL so servers sharing a URL keep separate accounts.
type CredentialStore struct {
	dir string
}

// NewCredentialStore stores credentials in agentDir/mcp-auth.json.
func NewCredentialStore(agentDir string) *CredentialStore { return &CredentialStore{dir: agentDir} }

func (c *CredentialStore) path() string { return filepath.Join(c.dir, "mcp-auth.json") }

// storeKeys returns a server's key and the legacy URL-only key.
func storeKeys(name, serverURL string) (key, legacy string) {
	legacy = serverURL
	if u, err := url.Parse(serverURL); err == nil {
		if u.Path == "" {
			u.Path = "/"
		}
		legacy = u.String()
	}
	return Namespace(name) + "|" + legacy, legacy
}

// update runs fn on the stored credentials under the file lock and writes
// them back when fn reports a change.
func (c *CredentialStore) update(ctx context.Context, fn func(map[string]*Credential) bool) (err error) {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", c.dir, err)
	}
	lock := flock.New(c.path() + ".lock")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := lock.TryLockContext(ctx, 10*time.Millisecond); err != nil {
		return errors.Join(fmt.Errorf("lock %s: %w", c.path(), err), lock.Close())
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	states := map[string]*Credential{}
	data, err := os.ReadFile(c.path())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("read %s: %w", c.path(), err)
	case strings.TrimSpace(string(data)) != "":
		if err := json.Unmarshal(data, &states); err != nil {
			return fmt.Errorf("parse %s: %w", c.path(), err)
		}
	}
	if !fn(states) {
		return nil
	}
	data, err = json.MarshalIndent(states, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", c.path(), err)
	}
	tmp := c.path() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", c.path(), err)
	}
	if err := os.Rename(tmp, c.path()); err != nil {
		return fmt.Errorf("replace %s: %w", c.path(), err)
	}
	return nil
}

// Load returns the server's credential, or nil. The first server to load a
// legacy URL-only entry takes it over.
func (c *CredentialStore) Load(ctx context.Context, name, serverURL string) (*Credential, error) {
	key, legacy := storeKeys(name, serverURL)
	var cred *Credential
	err := c.update(ctx, func(states map[string]*Credential) bool {
		if cred = states[key]; cred != nil || states[legacy] == nil {
			return false
		}
		cred = states[legacy]
		states[key] = cred
		delete(states, legacy)
		return true
	})
	return cred, err
}

// Save stores the server's credential.
func (c *CredentialStore) Save(ctx context.Context, name, serverURL string, cred *Credential) error {
	key, _ := storeKeys(name, serverURL)
	return c.update(ctx, func(states map[string]*Credential) bool {
		states[key] = cred
		return true
	})
}

// Remove deletes the server's credential, including a legacy entry it would
// take over, and reports whether one was stored.
func (c *CredentialStore) Remove(ctx context.Context, name, serverURL string) (bool, error) {
	key, legacy := storeKeys(name, serverURL)
	var removed bool
	err := c.update(ctx, func(states map[string]*Credential) bool {
		for _, k := range []string{key, legacy} {
			if _, ok := states[k]; ok {
				delete(states, k)
				removed = true
				return true
			}
		}
		return false
	})
	return removed, err
}

// refreshLock keeps other processes from refreshing the server's tokens.
func (c *CredentialStore) refreshLock(ctx context.Context, name, serverURL string) (func() error, error) {
	key, _ := storeKeys(name, serverURL)
	sum := sha256.Sum256([]byte(key))
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", c.dir, err)
	}
	lock := flock.New(filepath.Join(c.dir, "mcp-auth-refresh-"+hex.EncodeToString(sum[:8])))
	ctx, cancel := context.WithTimeout(ctx, refreshLockWait)
	defer cancel()
	if _, err := lock.TryLockContext(ctx, 100*time.Millisecond); err != nil {
		return nil, errors.Join(fmt.Errorf("refresh lock: %w", err), lock.Close())
	}
	return lock.Close, nil
}

// usesOAuth reports whether a server signs in with OAuth: HTTP servers
// without a provider token or an Authorization header.
func usesOAuth(cfg ServerConfig) bool {
	if cfg.URL == "" || cfg.Auth != nil {
		return false
	}
	for k := range cfg.Headers {
		if strings.EqualFold(k, "authorization") {
			return false
		}
	}
	return true
}

// tokenAuth sends a server's stored token and refreshes it. It never starts a
// browser sign-in.
type tokenAuth struct {
	store  *CredentialStore
	name   string
	url    string
	ctx    context.Context // the manager's; ends at shutdown
	client *http.Client

	mu sync.Mutex
	// challenge is the latest WWW-Authenticate header, for sign-in's
	// resource metadata URL and scope.
	challenge []string
}

func newTokenAuth(ctx context.Context, store *CredentialStore, name, serverURL string) *tokenAuth {
	return &tokenAuth{store: store, name: name, url: serverURL, ctx: ctx, client: &http.Client{Timeout: oauthRequestTimeout}}
}

func (a *tokenAuth) TokenSource(context.Context) (oauth2.TokenSource, error) { return a, nil }

// Token returns the stored access token, refreshed first when about to
// expire. A failed refresh sends the old token and lets a 401 decide.
func (a *tokenAuth) Token() (*oauth2.Token, error) {
	ctx := context.WithoutCancel(a.ctx)
	cred, err := a.store.Load(ctx, a.name, a.url)
	if err != nil || cred == nil || cred.Token == nil {
		return nil, err
	}
	token := cred.Token
	if !token.Expiry.IsZero() && time.Until(token.Expiry) <= refreshSkew && token.RefreshToken != "" {
		if a.refresh(token.AccessToken) == nil {
			if cred, err := a.store.Load(ctx, a.name, a.url); err == nil && cred != nil && cred.Token != nil {
				token = cred.Token
			}
		}
	}
	return &oauth2.Token{AccessToken: token.AccessToken, TokenType: "Bearer"}, nil
}

// Authorize handles a 401 or 403 by refreshing the rejected token. More scope
// or a missing refresh token needs a new sign-in.
func (a *tokenAuth) Authorize(_ context.Context, req *http.Request, resp *http.Response) error {
	_, copyErr := io.Copy(io.Discard, resp.Body)
	err := errors.Join(copyErr, resp.Body.Close())
	header := resp.Header.Values("WWW-Authenticate")
	a.mu.Lock()
	a.challenge = header
	a.mu.Unlock()
	if challengeParam(header, "error") == "insufficient_scope" {
		return ErrNeedsAuth
	}
	if resp.StatusCode == http.StatusForbidden {
		return errors.Join(err, fmt.Errorf("MCP server answered %s", resp.Status))
	}
	stale, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	return a.refresh(stale)
}

// lastChallenge returns the server's latest WWW-Authenticate header.
func (a *tokenAuth) lastChallenge() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.challenge
}

// refresh replaces stale, the access token that expired or was rejected.
// Tokens saved meanwhile by another refresh or a sign-in are kept. Nothing
// refreshes after shutdown, but a refresh already sent isn't cancelled: the
// server may have rotated the refresh token, so its answer must be saved.
func (a *tokenAuth) refresh(stale string) (err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ctx.Err(); err != nil {
		return err
	}
	unlock, err := a.store.refreshLock(a.ctx, a.name, a.url)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	cred, err := a.store.Load(a.ctx, a.name, a.url)
	if err != nil {
		return err
	}
	if cred == nil || cred.Token == nil {
		return ErrNeedsAuth
	}
	if cred.Token.AccessToken != stale {
		return nil
	}
	if cred.Token.RefreshToken == "" || cred.TokenURL == "" {
		return ErrNeedsAuth
	}
	if err := a.ctx.Err(); err != nil {
		return err
	}
	cfg := oauth2.Config{
		ClientID: cred.ClientID, ClientSecret: cred.ClientSecret,
		Endpoint: oauth2.Endpoint{TokenURL: cred.TokenURL, AuthStyle: cred.AuthStyle},
	}
	ctx := context.WithValue(context.WithoutCancel(a.ctx), oauth2.HTTPClient, a.client)
	token, err := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: cred.Token.RefreshToken}).Token()
	if err != nil {
		if errors.As(err, new(*oauth2.RetrieveError)) {
			return fmt.Errorf("%w: %w", ErrNeedsAuth, err)
		}
		return fmt.Errorf("refresh MCP token: %w", err)
	}
	cred.Token = token
	return a.store.Save(context.WithoutCancel(a.ctx), a.name, a.url, cred)
}

func challengeParam(header []string, param string) string {
	challenges, err := oauthex.ParseWWWAuthenticate(header)
	if err != nil {
		return ""
	}
	for _, c := range challenges {
		if c.Scheme == "bearer" && c.Params[param] != "" {
			return c.Params[param]
		}
	}
	return ""
}
