package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/oauth"
)

// authFile represents Pi's flat on-disk auth.json schema: provider id -> credential.
type authFile map[string]authCredential

// clone returns an independent copy. Callers mutate what readFile hands back,
// so the cache must never be the thing they get.
func (a authFile) clone() authFile {
	out := make(authFile, len(a))
	maps.Copy(out, a)
	return out
}

// fileStamp identifies a version of auth.json on disk.
//
// Size is part of it because mtime alone is not enough: filesystem timestamp
// granularity is coarse enough that two writes can share an mtime, and a cache
// keyed on mtime would then serve the first write's contents forever.
type fileStamp struct {
	modTime time.Time
	size    int64
}

func stampOf(info os.FileInfo) fileStamp {
	return fileStamp{modTime: info.ModTime(), size: info.Size()}
}

func (s fileStamp) matches(info os.FileInfo) bool {
	return s.modTime.Equal(info.ModTime()) && s.size == info.Size()
}

type authCredential struct {
	Type ai.CredentialType `json:"type"`

	// api_key
	Key string `json:"key,omitempty"`

	// oauth (Pi-compatible field names and millisecond expiry)
	Access    string         `json:"access,omitempty"`
	Refresh   string         `json:"refresh,omitempty"`
	Expires   int64          `json:"expires,omitempty"`
	Scope     string         `json:"scope,omitempty"`
	ProjectID string         `json:"projectId,omitempty"`
	Email     string         `json:"email,omitempty"`
	AccountID string         `json:"accountId,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`

	// Tau metadata; omitted unless a terminal refresh failure occurred.
	NeedsReauth bool `json:"needs_reauth,omitempty"`
}

func credentialToAuthCredential(cred ai.Credential) authCredential {
	out := authCredential{Type: cred.Type, NeedsReauth: cred.NeedsReauth}
	switch cred.Type {
	case ai.CredentialTypeAPIKey:
		out.Key = cred.APIKey
	case ai.CredentialTypeOAuth:
		out.Access = cred.AccessToken
		out.Refresh = cred.RefreshToken
		if !cred.ExpiresAt.IsZero() {
			out.Expires = cred.ExpiresAt.UnixMilli()
		}
		out.Scope = cred.Scope
		out.ProjectID = cred.ProjectID
		out.Email = cred.Email
		out.AccountID = cred.AccountID
		out.Extra = cred.Extra
	}
	return out
}

func authCredentialToCredential(wire authCredential) ai.Credential {
	switch wire.Type {
	case ai.CredentialTypeAPIKey:
		cred := ai.NewAPIKeyCredential(wire.Key)
		cred.NeedsReauth = wire.NeedsReauth
		return cred
	case ai.CredentialTypeOAuth:
		var expires time.Time
		if wire.Expires != 0 {
			expires = time.UnixMilli(wire.Expires)
		}
		cred := ai.NewOAuthCredential(wire.Access, wire.Refresh, expires, wire.Scope)
		cred.ProjectID = wire.ProjectID
		cred.Email = wire.Email
		cred.AccountID = wire.AccountID
		cred.Extra = wire.Extra
		cred.NeedsReauth = wire.NeedsReauth
		return cred
	case ai.CredentialTypeNone:
		cred := ai.NewNoneCredential()
		cred.NeedsReauth = wire.NeedsReauth
		return cred
	default:
		return ai.Credential{Type: wire.Type, NeedsReauth: wire.NeedsReauth}
	}
}

// FileBackend is a file-backed CredentialStore. Credentials are stored in
// auth.json at the configured path with mode 0600. The parent directory is
// created with mode 0700 if it doesn't exist. Cross-process concurrent
// access is coordinated via advisory file locking (gofrs/flock) during writes.
type FileBackend struct {
	mu           sync.RWMutex
	path         string
	refreshFuncs map[string]RefreshFunc
	margin       time.Duration
	refreshMu    sync.Map
	// cache holds the last parsed contents; cacheStamp identifies which
	// on-disk version it came from and cacheValid says whether it means
	// anything yet. Guarded by cacheMu, which is separate from f.mu so
	// callers already holding f.mu cannot deadlock.
	cache      authFile
	cacheStamp fileStamp
	cacheValid bool
	cacheMu    sync.Mutex
}

// NewFileBackend creates a new file-backed credential store at the given path.
func NewFileBackend(path string, opts Options) *FileBackend {
	return &FileBackend{
		path:         path,
		refreshFuncs: opts.refreshFuncs(),
		margin:       opts.expiryMargin(),
		cache:        make(authFile),
	}
}

// EnsureDir creates the parent directory with mode 0700 if it doesn't exist.
func (f *FileBackend) EnsureDir() error {
	dir := filepath.Dir(f.path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o700)
	}
	return nil
}

// Get retrieves a credential, refreshing if needed.
func (f *FileBackend) Get(ctx context.Context, provider string) (ai.Credential, error) {
	key := credKey(provider)

	cred, err := f.readCredential(key)
	if err != nil {
		return ai.Credential{}, err
	}

	// Check if refresh is needed
	if !cred.IsExpired(f.margin) || !cred.IsRefreshable() {
		return cred, nil
	}

	// Serialize refresh for the same key
	keyMu, _ := f.refreshMu.LoadOrStore(key, &sync.Mutex{})
	keyMu.(*sync.Mutex).Lock()
	defer keyMu.(*sync.Mutex).Unlock()

	// Re-read after acquiring lock
	cred, err = f.readCredential(key)
	if err != nil {
		return ai.Credential{}, err
	}
	if !cred.IsExpired(f.margin) {
		return cred, nil
	}

	// Perform refresh
	refreshFn, hasRefresh := f.refreshFuncs[provider]
	if !hasRefresh {
		return cred, nil
	}

	refreshed, err := refreshFn(ctx, cred)
	if err != nil {
		if isErrCredentialNeedsReauth(err) {
			cred.NeedsReauth = true
			_ = f.writeCredential(key, cred)
			return cred, err
		}
		return cred, fmt.Errorf("refresh %s: %w", key, err)
	}

	if writeErr := f.writeCredential(key, refreshed); writeErr != nil {
		return refreshed, fmt.Errorf("persist refreshed credential: %w", writeErr)
	}

	return refreshed, nil
}

// Set stores a credential.
func (f *FileBackend) Set(_ context.Context, provider string, cred ai.Credential) error {
	return f.writeCredential(credKey(provider), cred)
}

// Delete removes a credential.
func (f *FileBackend) Delete(_ context.Context, provider string) error {
	if _, err := os.Stat(f.path); os.IsNotExist(err) {
		return nil
	}
	// Routed through mutate for the same reason Set is: deleting one key by
	// rewriting the whole map from a stale read drops every credential another
	// process added in the meantime.
	return f.mutate(func(af authFile) {
		delete(af, credKey(provider))
	})
}

// List returns all stored credential references.
func (f *FileBackend) List(_ context.Context) ([]ai.CredentialRef, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	af, err := f.readFile()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	refs := make([]ai.CredentialRef, 0, len(af))
	for provider, cred := range af {
		refs = append(refs, ai.CredentialRef{
			Provider:    provider,
			Type:        cred.Type,
			NeedsReauth: cred.NeedsReauth,
		})
	}
	return refs, nil
}

// RefreshOAuth refreshes an OAuth credential using the provided oauth.Provider.
func (f *FileBackend) RefreshOAuth(ctx context.Context, provider string, oauthProvider oauth.Provider) (ai.Credential, error) {
	key := credKey(provider)

	cred, err := f.readCredential(key)
	if err != nil {
		return ai.Credential{}, err
	}

	if cred.Type != ai.CredentialTypeOAuth {
		return cred, fmt.Errorf("credential %s is not OAuth", key)
	}

	// Serialize refresh for the same key
	keyMu, _ := f.refreshMu.LoadOrStore(key, &sync.Mutex{})
	keyMu.(*sync.Mutex).Lock()
	defer keyMu.(*sync.Mutex).Unlock()

	// Re-read after acquiring lock
	cred, err = f.readCredential(key)
	if err != nil {
		return ai.Credential{}, err
	}

	// Convert to oauth.Credentials
	oauthCreds := OAuthCredentialToCredentials(cred)

	// Refresh
	refreshed, err := oauthProvider.RefreshToken(ctx, oauthCreds)
	if err != nil {
		// Check for terminal error
		if isErrCredentialNeedsReauth(err) {
			cred.NeedsReauth = true
			_ = f.writeCredential(key, cred)
			return cred, ai.ErrCredentialNeedsReauth{Provider: provider}
		}
		return cred, fmt.Errorf("refresh %s: %w", key, err)
	}

	// Convert back and store
	cred = CredentialFromOAuth(refreshed)
	if err := f.writeCredential(key, cred); err != nil {
		return cred, fmt.Errorf("persist refreshed credential: %w", err)
	}

	return cred, nil
}

func (f *FileBackend) readCredential(key string) (ai.Credential, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	af, err := f.readFile()
	if err != nil {
		return ai.Credential{}, err
	}

	cred, ok := af[key]
	if !ok {
		return ai.Credential{}, fmt.Errorf("%w: %s", ai.ErrCredentialNotFound, key)
	}
	return authCredentialToCredential(cred), nil
}

func (f *FileBackend) writeCredential(key string, cred ai.Credential) error {
	return f.mutate(func(af authFile) {
		af[key] = credentialToAuthCredential(cred)
	})
}

// mutate applies fn to the stored credential set as one atomic read-modify-write.
//
// The cross-process lock is held across the *whole* sequence, not just the
// write. Holding it only for the write loses updates: two processes both read
// {a:1}, each adds its own key, each writes the full map back, and whichever
// finishes second erases the other's credential. Re-reading from disk under the
// lock (rather than from cache) is the other half — a cached copy taken before
// the lock was acquired is exactly the stale base that causes the loss.
func (f *FileBackend) mutate(fn func(authFile)) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.EnsureDir(); err != nil {
		return fmt.Errorf("ensure auth dir: %w", err)
	}

	unlock, err := f.lockForWrite()
	if err != nil {
		return err
	}
	defer unlock()

	af, err := f.readFileFromDisk()
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		af = make(authFile)
	}

	fn(af)
	return f.writeFileLocked(af)
}

// lockForWrite acquires the advisory lock that coordinates writers across
// processes. The returned func releases it.
func (f *FileBackend) lockForWrite() (func(), error) {
	fl := flock.New(f.path + ".lock")
	if err := fl.Lock(); err != nil {
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	return func() { _ = fl.Unlock() }, nil
}

// readFile returns the stored credentials, serving a cached copy when the file
// has not changed since it was read.
//
// The returned map is always a copy. Handing out the cache itself let a caller
// mutate it in place: writeCredential added its entry to the live cache before
// attempting the write, so a failed write left the cache advertising a
// credential that was never persisted.
func (f *FileBackend) readFile() (authFile, error) {
	f.cacheMu.Lock()
	defer f.cacheMu.Unlock()

	info, err := os.Stat(f.path)
	if err != nil {
		return nil, err
	}

	if f.cacheValid && f.cacheStamp.matches(info) {
		return f.cache.clone(), nil
	}

	af, err := parseAuthFile(f.path)
	if err != nil {
		return nil, err
	}

	f.cache = af
	f.cacheStamp = stampOf(info)
	f.cacheValid = true

	return af.clone(), nil
}

// readFileFromDisk bypasses the cache. Callers holding the cross-process lock
// use it so their read-modify-write starts from what is actually on disk rather
// than from whatever this process saw last.
func (f *FileBackend) readFileFromDisk() (authFile, error) {
	af, err := parseAuthFile(f.path)
	if err != nil {
		return nil, err
	}
	return af, nil
}

func parseAuthFile(path string) (authFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var af authFile
	if err := json.Unmarshal(data, &af); err != nil {
		return nil, fmt.Errorf("parse auth.json: %w", err)
	}
	if af == nil {
		af = make(authFile)
	}
	return af, nil
}

// writeFileLocked persists af. The caller must already hold both f.mu and the
// cross-process lock.
//
// The replacement is atomic: the new contents are written to a sibling temp
// file and renamed over the target. os.WriteFile truncates first, so a reader
// — which takes no lock — could observe a half-written file and fail to parse
// it. rename(2) is atomic within a directory, so a reader sees either the old
// file or the new one, never a partial one.
func (f *FileBackend) writeFileLocked(af authFile) error {
	data, err := json.MarshalIndent(af, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal auth.json: %w", err)
	}

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp auth file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: a successful rename removes tmpName, after which
	// this is a no-op. On any failure path it stops a stray file being left.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp auth file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write auth.json: %w", err)
	}
	// Durability: without the sync, a crash between rename and writeback can
	// leave the renamed file empty.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync auth.json: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp auth file: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return fmt.Errorf("replace auth.json: %w", err)
	}

	// Stamp the cache from the file's real metadata. Using time.Now() here
	// recorded an instant later than the file's own mtime, so a subsequent
	// write by another process could land in between and be treated as stale.
	f.cacheMu.Lock()
	f.cache = af.clone()
	if info, statErr := os.Stat(f.path); statErr == nil {
		f.cacheStamp = stampOf(info)
		f.cacheValid = true
	} else {
		f.cacheValid = false
	}
	f.cacheMu.Unlock()

	return nil
}

func isErrCredentialNeedsReauth(err error) bool {
	var reauth ai.ErrCredentialNeedsReauth
	return err != nil && (errors.Is(err, reauth) || errors.As(err, &reauth))
}
