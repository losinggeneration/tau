package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"syscall"
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
	for key, value := range a {
		value.Unknown = cloneAuthFields(value.Unknown)
		value.Env = maps.Clone(value.Env)
		if value.Extra != nil {
			value.Extra = cloneAuthJSON(value.Extra).(map[string]any)
		}
		out[key] = value
	}
	return out
}

func cloneAuthJSON(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, entry := range value {
			out[key] = cloneAuthJSON(entry)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, entry := range value {
			out[i] = cloneAuthJSON(entry)
		}
		return out
	default:
		return value
	}
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
	Type    ai.CredentialType          `json:"type"`
	Unknown map[string]json.RawMessage `json:"-"`

	// api_key
	Key string            `json:"key,omitempty"`
	Env map[string]string `json:"env,omitempty"`

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

func cloneAuthFields(fields map[string]json.RawMessage) map[string]json.RawMessage {
	if fields == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

func (c *authCredential) UnmarshalJSON(data []byte) error {
	type plain authCredential
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, key := range []string{"type", "key", "env", "access", "refresh", "expires", "scope", "projectId", "email", "accountId", "extra", "needs_reauth"} {
		delete(fields, key)
	}
	if len(fields) != 0 {
		decoded.Unknown = fields
	}
	*c = authCredential(decoded)
	return nil
}

func (c authCredential) MarshalJSON() ([]byte, error) {
	type plain authCredential
	data, err := json.Marshal(plain(c))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for key, value := range c.Unknown {
		if _, exists := fields[key]; !exists {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

func credentialToAuthCredential(cred ai.Credential) authCredential {
	out := authCredential{Type: cred.Type, NeedsReauth: cred.NeedsReauth}
	switch cred.Type {
	case ai.CredentialTypeAPIKey:
		out.Key = cred.APIKey
		out.Env = cred.Env
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
		cred.Env = wire.Env
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
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	return nil
}

// Get retrieves a credential, refreshing if needed.
func (f *FileBackend) Get(ctx context.Context, provider string) (ai.Credential, error) {
	if err := context.Cause(ctx); err != nil {
		return ai.Credential{}, err
	}
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
	release, err := acquireRefresh(ctx, &f.refreshMu, key)
	if err != nil {
		return ai.Credential{}, err
	}
	defer release()

	// Re-read after acquiring lock
	cred, err = f.readCredential(key)
	if err != nil {
		return ai.Credential{}, err
	}
	if !cred.IsExpired(f.margin) || !cred.IsRefreshable() {
		return cred, nil
	}

	// Perform refresh
	expected := authFile{key: credentialToAuthCredential(cred)}.clone()[key]
	refreshFn, hasRefresh := f.refreshFuncs[key]
	if !hasRefresh {
		return cred, nil
	}

	refreshed, err := refreshFn(ctx, cred)
	if err != nil {
		if isErrCredentialNeedsReauth(err) {
			cred.NeedsReauth = true
			if writeErr := f.writeRefreshedCredential(ctx, key, expected, cred); writeErr != nil {
				return cred, errors.Join(err, fmt.Errorf("persist credential reauthentication state: %w", writeErr))
			}
			return cred, err
		}
		return cred, fmt.Errorf("refresh %s: %w", key, err)
	}

	if writeErr := f.writeRefreshedCredential(ctx, key, expected, refreshed); writeErr != nil {
		return refreshed, fmt.Errorf("persist refreshed credential: %w", writeErr)
	}

	return refreshed, nil
}

// Set stores a credential.
func (f *FileBackend) Set(ctx context.Context, provider string, cred ai.Credential) error {
	return f.writeCredential(ctx, credKey(provider), cred)
}

// Delete removes a credential.
func (f *FileBackend) Delete(ctx context.Context, provider string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if _, err := os.Stat(f.path); os.IsNotExist(err) {
		return nil
	}
	// Routed through mutate for the same reason Set is: deleting one key by
	// rewriting the whole map from a stale read drops every credential another
	// process added in the meantime.
	return f.mutate(ctx, func(af authFile) error {
		delete(af, credKey(provider))
		return nil
	})
}

// List returns all stored credential references.
func (f *FileBackend) List(ctx context.Context) ([]ai.CredentialRef, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
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
	if err := context.Cause(ctx); err != nil {
		return ai.Credential{}, err
	}
	key := credKey(provider)

	cred, err := f.readCredential(key)
	if err != nil {
		return ai.Credential{}, err
	}

	if cred.Type != ai.CredentialTypeOAuth {
		return cred, fmt.Errorf("credential %s is not OAuth", key)
	}

	// Serialize refresh for the same key
	release, err := acquireRefresh(ctx, &f.refreshMu, key)
	if err != nil {
		return ai.Credential{}, err
	}
	defer release()

	// Re-read after acquiring lock
	cred, err = f.readCredential(key)
	if err != nil {
		return ai.Credential{}, err
	}

	if cred.Type != ai.CredentialTypeOAuth {
		return cred, fmt.Errorf("credential %s is not OAuth", key)
	}

	// Convert to oauth.Credentials
	expected := authFile{key: credentialToAuthCredential(cred)}.clone()[key]
	oauthCreds := OAuthCredentialToCredentials(cred)

	// Refresh
	refreshed, err := oauthProvider.RefreshToken(ctx, oauthCreds)
	if err != nil {
		// Check for terminal error
		if isErrCredentialNeedsReauth(err) {
			cred.NeedsReauth = true
			if writeErr := f.writeRefreshedCredential(ctx, key, expected, cred); writeErr != nil {
				return cred, errors.Join(err, fmt.Errorf("persist credential reauthentication state: %w", writeErr))
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
	if err := f.writeRefreshedCredential(ctx, key, expected, cred); err != nil {
		return cred, fmt.Errorf("persist refreshed credential: %w", err)
	}

	return cred, nil
}

func acquireRefresh(ctx context.Context, refreshMu *sync.Map, key string) (func(), error) {
	value, _ := refreshMu.LoadOrStore(key, make(chan struct{}, 1))
	gate := value.(chan struct{})
	select {
	case gate <- struct{}{}:
		if err := context.Cause(ctx); err != nil {
			<-gate
			return nil, err
		}
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
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

func (f *FileBackend) writeCredential(ctx context.Context, key string, cred ai.Credential) error {
	return f.mutate(ctx, func(af authFile) error {
		af[key] = credentialToAuthCredential(cred)
		return nil
	})
}

func (f *FileBackend) writeRefreshedCredential(ctx context.Context, key string, expected authCredential, refreshed ai.Credential) error {
	return f.mutate(ctx, func(af authFile) error {
		current, exists := af[key]
		unknown := current.Unknown
		current.Unknown = nil
		if !exists || !reflect.DeepEqual(current, expected) {
			return errors.New("credential changed during refresh")
		}
		updated := credentialToAuthCredential(refreshed)
		updated.Unknown = unknown
		af[key] = updated
		return nil
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
func (f *FileBackend) mutate(ctx context.Context, fn func(authFile) error) (err error) {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.EnsureDir(); err != nil {
		return fmt.Errorf("ensure auth dir: %w", err)
	}

	unlock, err := f.lockForWrite(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := unlock(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	af, err := f.readFileFromDisk()
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		af = make(authFile)
	}

	if err := fn(af); err != nil {
		return err
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return f.writeFileLocked(af)
}

// lockForWrite acquires the advisory lock that coordinates writers across
// processes. The returned func releases it.
func (f *FileBackend) lockForWrite(ctx context.Context) (func() error, error) {
	fl := flock.New(f.path + ".lock")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := acquireCredentialLock(ctx, runtime.GOOS, func() error {
		locked, err := fl.TryLockContext(ctx, 10*time.Millisecond)
		if err != nil {
			return errors.Join(err, context.Cause(ctx))
		}
		if !locked {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return errors.New("credential lock not acquired")
		}
		return nil
	}); err != nil {
		return nil, errors.Join(fmt.Errorf("acquire lock: %w", err), fl.Close())
	}
	return fl.Close, nil
}

func acquireCredentialLock(ctx context.Context, platform string, lock func() error) error {
	for attempt := range 6 {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		err := lock()
		if err == nil {
			return nil
		}
		if platform != "windows" || !errors.Is(err, syscall.EPERM) || attempt == 5 {
			return err
		}
		timer := time.NewTimer(time.Duration(10<<attempt) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
	return errors.New("credential lock retries exhausted")
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
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}), &af); err != nil {
		return nil, fmt.Errorf("parse auth.json: %w", err)
	}
	if af == nil {
		return nil, errors.New("parse auth.json: expected an object")
	}
	for provider, cred := range af {
		switch cred.Type {
		case ai.CredentialTypeAPIKey, ai.CredentialTypeOAuth, ai.CredentialTypeNone:
		default:
			return nil, fmt.Errorf("parse auth.json: invalid credential type for provider %q", provider)
		}
	}
	if legacy, exists := af["azure-openai-responses"]; exists {
		if _, exists := af[ai.ProviderAzureOpenAI]; !exists {
			af[ai.ProviderAzureOpenAI] = legacy
		}
		delete(af, "azure-openai-responses")
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
func (f *FileBackend) writeFileLocked(af authFile) (err error) {
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
	closed := false
	defer func() {
		if !closed {
			if closeErr := tmp.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close temp auth file: %w", closeErr))
			}
		}
		if removeErr := os.Remove(tmpName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove temp auth file: %w", removeErr))
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp auth file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write auth.json: %w", err)
	}
	// Durability: without the sync, a crash between rename and writeback can
	// leave the renamed file empty.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync auth.json: %w", err)
	}
	closed = true
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
