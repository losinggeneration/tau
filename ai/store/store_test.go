package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/losinggeneration/tau/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryBackend_SetGet(t *testing.T) {
	store := NewMemoryBackend(Options{})

	cred := ai.NewAPIKeyCredential("sk-test-123")
	err := store.Set(context.Background(), "anthropic", cred)
	require.NoError(t, err)

	got, err := store.Get(context.Background(), "anthropic")
	require.NoError(t, err)
	assert.Equal(t, ai.CredentialTypeAPIKey, got.Type)
	assert.Equal(t, "sk-test-123", got.APIKey)
}

func TestMemoryBackend_GetNotFound(t *testing.T) {
	store := NewMemoryBackend(Options{})

	_, err := store.Get(context.Background(), "anthropic")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestMemoryBackend_Delete(t *testing.T) {
	store := NewMemoryBackend(Options{})

	err := store.Set(context.Background(), "anthropic", ai.NewAPIKeyCredential("key"))
	require.NoError(t, err)

	err = store.Delete(context.Background(), "anthropic")
	require.NoError(t, err)

	_, err = store.Get(context.Background(), "anthropic")
	assert.Error(t, err)
}

func TestMemoryBackend_List(t *testing.T) {
	store := NewMemoryBackend(Options{})

	require.NoError(t, store.Set(context.Background(), "anthropic", ai.NewAPIKeyCredential("key1")))
	require.NoError(t, store.Set(context.Background(), "openai", ai.NewAPIKeyCredential("key2")))

	refs, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, refs, 2)

	// Should contain both providers. Pi-compatible auth storage ignores accounts.
	found := map[string]bool{}
	for _, ref := range refs {
		found[ref.Provider] = true
	}
	assert.True(t, found["anthropic"])
	assert.True(t, found["openai"])
}

func TestMemoryBackend_RefreshOnRead(t *testing.T) {
	refreshed := ai.NewOAuthCredential("new-access", "new-refresh", time.Now().Add(1*time.Hour), "scope")

	refreshCalled := 0
	store := NewMemoryBackend(Options{
		RefreshFuncs: map[string]RefreshFunc{
			"anthropic": func(_ context.Context, _ ai.Credential) (ai.Credential, error) {
				refreshCalled++
				return refreshed, nil
			},
		},
	})

	// Store an expired OAuth credential
	expired := ai.NewOAuthCredential("old-access", "old-refresh", time.Now().Add(-1*time.Hour), "scope")
	require.NoError(t, store.Set(context.Background(), "anthropic", expired))

	got, err := store.Get(context.Background(), "anthropic")
	require.NoError(t, err)
	assert.Equal(t, "new-access", got.AccessToken)
	assert.Equal(t, 1, refreshCalled)
}

func TestMemoryBackend_RefreshConcurrent(t *testing.T) {
	refreshCount := int64(0)
	var mu sync.Mutex

	store := NewMemoryBackend(Options{
		RefreshFuncs: map[string]RefreshFunc{
			"anthropic": func(_ context.Context, _ ai.Credential) (ai.Credential, error) {
				mu.Lock()
				refreshCount++
				mu.Unlock()
				time.Sleep(50 * time.Millisecond) // simulate slow refresh
				return ai.NewOAuthCredential("refreshed", "refresh-token", time.Now().Add(1*time.Hour), "scope"), nil
			},
		},
	})

	expired := ai.NewOAuthCredential("old", "refresh", time.Now().Add(-1*time.Hour), "scope")
	require.NoError(t, store.Set(context.Background(), "anthropic", expired))

	// Concurrent Gets
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := store.Get(context.Background(), "anthropic")
			assert.NoError(t, err)
			assert.Equal(t, "refreshed", got.AccessToken)
		}()
	}
	wg.Wait()

	// Should have refreshed exactly once
	mu.Lock()
	assert.Equal(t, int64(1), refreshCount)
	mu.Unlock()
}

func TestMemoryBackend_RefreshTerminalFailure(t *testing.T) {
	store := NewMemoryBackend(Options{
		RefreshFuncs: map[string]RefreshFunc{
			"anthropic": func(_ context.Context, _ ai.Credential) (ai.Credential, error) {
				return ai.Credential{}, ai.ErrCredentialNeedsReauth{Provider: "anthropic"}
			},
		},
	})

	expired := ai.NewOAuthCredential("old", "refresh", time.Now().Add(-1*time.Hour), "scope")
	require.NoError(t, store.Set(context.Background(), "anthropic", expired))

	got, err := store.Get(context.Background(), "anthropic")
	assert.Error(t, err)
	assert.True(t, got.NeedsReauth)
}

func TestMemoryBackend_NoRefreshFunc(t *testing.T) {
	store := NewMemoryBackend(Options{})

	expired := ai.NewOAuthCredential("old", "refresh", time.Now().Add(-1*time.Hour), "scope")
	require.NoError(t, store.Set(context.Background(), "anthropic", expired))

	// Should return the expired credential as-is since no refresh func
	got, err := store.Get(context.Background(), "anthropic")
	require.NoError(t, err)
	assert.Equal(t, "old", got.AccessToken)
}

func TestMemoryBackend_APIKeyNeverExpires(t *testing.T) {
	store := NewMemoryBackend(Options{})

	cred := ai.NewAPIKeyCredential("sk-test")
	require.NoError(t, store.Set(context.Background(), "openai", cred))

	got, err := store.Get(context.Background(), "openai")
	require.NoError(t, err)
	assert.Equal(t, "sk-test", got.APIKey)
}

func TestMemoryBackend_NoneCredential(t *testing.T) {
	store := NewMemoryBackend(Options{})

	cred := ai.NewNoneCredential()
	require.NoError(t, store.Set(context.Background(), "ollama", cred))

	got, err := store.Get(context.Background(), "ollama")
	require.NoError(t, err)
	assert.Equal(t, ai.CredentialTypeNone, got.Type)
}

func TestFileBackend_SetGet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	store := NewFileBackend(path, Options{})

	cred := ai.NewAPIKeyCredential("sk-test-123")
	err := store.Set(context.Background(), "anthropic", cred)
	require.NoError(t, err)

	got, err := store.Get(context.Background(), "anthropic")
	require.NoError(t, err)
	assert.Equal(t, ai.CredentialTypeAPIKey, got.Type)
	assert.Equal(t, "sk-test-123", got.APIKey)
}

func TestFileBackend_WritesPiCompatibleAPIKeyFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	store := NewFileBackend(path, Options{})
	require.NoError(t, store.Set(context.Background(), "anthropic", ai.NewAPIKeyCredential("sk-test-123")))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	jsonText := string(data)
	assert.NotContains(t, jsonText, "version")
	assert.NotContains(t, jsonText, "credentials")
	assert.NotContains(t, jsonText, "anthropic/default")

	var got map[string]authCredential
	require.NoError(t, json.Unmarshal(data, &got))
	require.Contains(t, got, "anthropic")
	assert.Equal(t, ai.CredentialTypeAPIKey, got["anthropic"].Type)
	assert.Equal(t, "sk-test-123", got["anthropic"].Key)
}

func TestFileBackend_WritesPiCompatibleOAuthFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	expires := time.UnixMilli(1760000000000)

	store := NewFileBackend(path, Options{})
	cred := ai.NewOAuthCredential("access-token", "refresh-token", expires, "scope")
	cred.ProjectID = "project-123"
	cred.Email = "user@example.com"
	require.NoError(t, store.Set(context.Background(), "github-copilot", cred))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	jsonText := string(data)
	assert.NotContains(t, jsonText, "version")
	assert.NotContains(t, jsonText, "credentials")
	assert.NotContains(t, jsonText, "access_token")
	assert.NotContains(t, jsonText, "refresh_token")
	assert.NotContains(t, jsonText, "expires_at")

	var got map[string]authCredential
	require.NoError(t, json.Unmarshal(data, &got))
	require.Contains(t, got, "github-copilot")
	assert.Equal(t, ai.CredentialTypeOAuth, got["github-copilot"].Type)
	assert.Equal(t, "access-token", got["github-copilot"].Access)
	assert.Equal(t, "refresh-token", got["github-copilot"].Refresh)
	assert.Equal(t, expires.UnixMilli(), got["github-copilot"].Expires)
	assert.Equal(t, "scope", got["github-copilot"].Scope)
	assert.Equal(t, "project-123", got["github-copilot"].ProjectID)
	assert.Equal(t, "user@example.com", got["github-copilot"].Email)
}

func TestStoreIgnoresAccountForPiCompatibleAuth(t *testing.T) {
	mem := NewMemoryBackend(Options{})
	require.NoError(t, mem.Set(context.Background(), "anthropic", ai.NewAPIKeyCredential("first")))
	require.NoError(t, mem.Set(context.Background(), "anthropic", ai.NewAPIKeyCredential("second")))
	got, err := mem.Get(context.Background(), "anthropic")
	require.NoError(t, err)
	assert.Equal(t, "second", got.APIKey)

	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	file := NewFileBackend(path, Options{})
	require.NoError(t, file.Set(context.Background(), "openai", ai.NewAPIKeyCredential("first")))
	require.NoError(t, file.Set(context.Background(), "openai", ai.NewAPIKeyCredential("second")))
	got, err = file.Get(context.Background(), "openai")
	require.NoError(t, err)
	assert.Equal(t, "second", got.APIKey)
}

func TestFileBackend_GetNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	store := NewFileBackend(path, Options{})

	_, err := store.Get(context.Background(), "anthropic")
	assert.Error(t, err)
}

func TestFileBackend_Delete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	store := NewFileBackend(path, Options{})

	require.NoError(t, store.Set(context.Background(), "anthropic", ai.NewAPIKeyCredential("key")))
	require.NoError(t, store.Delete(context.Background(), "anthropic"))

	_, err := store.Get(context.Background(), "anthropic")
	assert.Error(t, err)
}

func TestFileBackend_List(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	store := NewFileBackend(path, Options{})

	require.NoError(t, store.Set(context.Background(), "anthropic", ai.NewAPIKeyCredential("key1")))
	require.NoError(t, store.Set(context.Background(), "openai", ai.NewAPIKeyCredential("key2")))

	refs, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, refs, 2)
}

func TestFileBackend_RefreshOnRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	refreshed := ai.NewOAuthCredential("new-access", "new-refresh", time.Now().Add(1*time.Hour), "scope")
	refreshCalled := 0

	store := NewFileBackend(path, Options{
		RefreshFuncs: map[string]RefreshFunc{
			"anthropic": func(_ context.Context, _ ai.Credential) (ai.Credential, error) {
				refreshCalled++
				return refreshed, nil
			},
		},
	})

	expired := ai.NewOAuthCredential("old-access", "old-refresh", time.Now().Add(-1*time.Hour), "scope")
	require.NoError(t, store.Set(context.Background(), "anthropic", expired))

	got, err := store.Get(context.Background(), "anthropic")
	require.NoError(t, err)
	assert.Equal(t, "new-access", got.AccessToken)
	assert.Equal(t, 1, refreshCalled)
}

func TestFileBackend_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "auth.json")

	store := NewFileBackend(path, Options{})
	require.NoError(t, store.Set(context.Background(), "anthropic", ai.NewAPIKeyCredential("key")))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
}

func TestFileBackend_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	store1 := NewFileBackend(path, Options{})
	require.NoError(t, store1.Set(context.Background(), "anthropic", ai.NewAPIKeyCredential("sk-persist")))

	// New instance reading the same file
	store2 := NewFileBackend(path, Options{})
	got, err := store2.Get(context.Background(), "anthropic")
	require.NoError(t, err)
	assert.Equal(t, "sk-persist", got.APIKey)
}

func TestFileBackend_RefreshTerminalFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	store := NewFileBackend(path, Options{
		RefreshFuncs: map[string]RefreshFunc{
			"anthropic": func(_ context.Context, _ ai.Credential) (ai.Credential, error) {
				return ai.Credential{}, ai.ErrCredentialNeedsReauth{Provider: "anthropic"}
			},
		},
	})

	expired := ai.NewOAuthCredential("old", "refresh", time.Now().Add(-1*time.Hour), "scope")
	require.NoError(t, store.Set(context.Background(), "anthropic", expired))

	got, err := store.Get(context.Background(), "anthropic")
	assert.Error(t, err)
	assert.True(t, got.NeedsReauth)

	// Verify persisted
	store2 := NewFileBackend(path, Options{})
	got2, err := store2.Get(context.Background(), "anthropic")
	require.NoError(t, err) // not expired anymore since we read it fresh
	assert.True(t, got2.NeedsReauth)
}

func TestCredentialIsExpired(t *testing.T) {
	// API key never expires
	apiKey := ai.NewAPIKeyCredential("key")
	assert.False(t, apiKey.IsExpired(5*time.Minute))

	// None never expires
	none := ai.NewNoneCredential()
	assert.False(t, none.IsExpired(5*time.Minute))

	// OAuth not expired
	oauth := ai.NewOAuthCredential("access", "refresh", time.Now().Add(10*time.Minute), "scope")
	assert.False(t, oauth.IsExpired(5*time.Minute))

	// OAuth expired (within margin)
	oauthExpired := ai.NewOAuthCredential("access", "refresh", time.Now().Add(3*time.Minute), "scope")
	assert.True(t, oauthExpired.IsExpired(5*time.Minute))

	// OAuth definitely expired
	oauthDefExpired := ai.NewOAuthCredential("access", "refresh", time.Now().Add(-1*time.Hour), "scope")
	assert.True(t, oauthDefExpired.IsExpired(5*time.Minute))
}

func TestCredentialIsRefreshable(t *testing.T) {
	apiKey := ai.NewAPIKeyCredential("key")
	assert.False(t, apiKey.IsRefreshable())

	none := ai.NewNoneCredential()
	assert.False(t, none.IsRefreshable())

	oauth := ai.NewOAuthCredential("access", "refresh", time.Now().Add(1*time.Hour), "scope")
	assert.True(t, oauth.IsRefreshable())

	oauthNoRefresh := ai.NewOAuthCredential("access", "", time.Now().Add(1*time.Hour), "scope")
	assert.False(t, oauthNoRefresh.IsRefreshable())
}

func TestErrCredentialNeedsReauth(t *testing.T) {
	err := ai.ErrCredentialNeedsReauth{Provider: "anthropic"}
	assert.Contains(t, err.Error(), "anthropic")
	assert.NotContains(t, err.Error(), "anthropic/default")
	assert.Contains(t, err.Error(), "tau login anthropic")
	assert.True(t, errors.Is(err, ai.ErrCredentialNeedsReauth{}))
}

// The tests below cover what the deleted TestFileBackend_ContentionAcrossProcesses
// claimed to: concurrent mutation of one auth.json must not lose credentials or
// corrupt the file.
//
// That test spawned two `go run` subprocesses. It compiled a helper on every
// run, and it did not actually test the property — both helpers wrote the *same*
// key, so a lost update was indistinguishable from a won race, and the
// assertions ("anthropic present, openai present") passed either way. The bugs
// below were all live underneath it.
//
// Separate FileBackend instances stand in for separate processes. That is not a
// weaker test: flock locks are held per open file description, so two
// flock.New() handles on one path contend exactly as they would across
// processes, and each instance has its own mutex and its own cache — which is
// precisely the state a process boundary duplicates.

// Two writers adding *different* credentials must both survive. Holding the
// cross-process lock only around the write (the original behaviour) loses one:
// both read the same base map, both add a key, and the second write erases the
// first writer's entry.
func TestFileBackend_ConcurrentSetsDoNotLoseCredentials(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")

	seed := NewFileBackend(authPath, Options{})
	require.NoError(t, seed.Set(context.Background(), "anthropic", ai.NewAPIKeyCredential("initial-key")))

	const writers = 8
	providers := make([]string, writers)
	for i := range providers {
		providers[i] = fmt.Sprintf("provider-%d", i)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i, provider := range providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each writer gets its own backend: separate lock handle, separate
			// cache, same file — the process boundary in miniature.
			s := NewFileBackend(authPath, Options{})
			<-start
			errs[i] = s.Set(context.Background(), provider, ai.NewAPIKeyCredential("key-"+provider))
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "writer %d failed", i)
	}

	reader := NewFileBackend(authPath, Options{})
	for _, provider := range providers {
		cred, err := reader.Get(context.Background(), provider)
		require.NoErrorf(t, err, "credential for %s was lost", provider)
		require.Equal(t, "key-"+provider, cred.APIKey)
	}
	// The credential that was there before the writers started must survive too.
	cred, err := reader.Get(context.Background(), "anthropic")
	require.NoError(t, err)
	require.Equal(t, "initial-key", cred.APIKey)
}

// Deleting one credential must not drop concurrent additions. Delete rewrites
// the whole map, so it has the same lost-update exposure as Set.
func TestFileBackend_ConcurrentDeleteDoesNotDropOtherWrites(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	ctx := context.Background()

	seed := NewFileBackend(authPath, Options{})
	require.NoError(t, seed.Set(ctx, "doomed", ai.NewAPIKeyCredential("doomed-key")))
	require.NoError(t, seed.Set(ctx, "keeper", ai.NewAPIKeyCredential("keeper-key")))

	deleter := NewFileBackend(authPath, Options{})
	writer := NewFileBackend(authPath, Options{})

	start := make(chan struct{})
	var wg sync.WaitGroup
	var delErr, setErr error
	wg.Add(2)
	go func() { defer wg.Done(); <-start; delErr = deleter.Delete(ctx, "doomed") }()
	go func() {
		defer wg.Done()
		<-start
		setErr = writer.Set(ctx, "newcomer", ai.NewAPIKeyCredential("newcomer-key"))
	}()
	close(start)
	wg.Wait()

	require.NoError(t, delErr)
	require.NoError(t, setErr)

	reader := NewFileBackend(authPath, Options{})
	if _, err := reader.Get(ctx, "doomed"); err == nil {
		t.Error("deleted credential is still readable")
	}
	for _, provider := range []string{"keeper", "newcomer"} {
		if _, err := reader.Get(ctx, provider); err != nil {
			t.Errorf("credential %q was lost by a concurrent delete: %v", provider, err)
		}
	}
}

// Readers take no lock, so a write that truncates before rewriting lets them
// observe a partial file. Every read must see either the old contents or the
// new ones — never a parse error and never an empty map.
func TestFileBackend_ReadsNeverObserveAPartialFile(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	ctx := context.Background()

	seed := NewFileBackend(authPath, Options{})
	// Enough entries that a non-atomic write has a wide window to be caught in.
	for i := range 40 {
		require.NoError(t, seed.Set(ctx, fmt.Sprintf("seed-%02d", i), ai.NewAPIKeyCredential(strings.Repeat("x", 256))))
	}

	writer := NewFileBackend(authPath, Options{})
	stop := make(chan struct{})
	var writerDone sync.WaitGroup

	writerDone.Add(1)
	go func() {
		defer writerDone.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := writer.Set(ctx, "churn", ai.NewAPIKeyCredential(fmt.Sprintf("key-%d", i))); err != nil {
				t.Errorf("write failed: %v", err)
				return
			}
		}
	}()

	// Read on this goroutine so the writer's stop signal is sent before we
	// wait on it; signalling after the wait would deadlock.
	var readErr error
	var shortRead int
	for range 300 {
		// A fresh backend each time so the read goes to disk rather than
		// being served from a cache the writer just refreshed.
		refs, err := NewFileBackend(authPath, Options{}).List(ctx)
		if err != nil {
			readErr = err
			break
		}
		if len(refs) < 40 {
			shortRead++
		}
	}

	close(stop)
	writerDone.Wait()

	require.NoError(t, readErr, "a concurrent read observed an unparseable auth.json")
	require.Zero(t, shortRead, "%d reads observed a truncated credential set", shortRead)
}

// A failed write must not leave the cache advertising a credential that was
// never persisted. readFile used to hand out the live cache map, which
// writeCredential then mutated before attempting the write.
func TestFileBackend_FailedWriteDoesNotPoisonTheCache(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	ctx := context.Background()

	s := NewFileBackend(authPath, Options{})
	require.NoError(t, s.Set(ctx, "anthropic", ai.NewAPIKeyCredential("initial-key")))

	// Make the directory read-only so the write fails after the read succeeds.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := s.Set(ctx, "openai", ai.NewAPIKeyCredential("never-persisted")); err == nil {
		t.Skip("filesystem allowed the write despite a read-only directory")
	}

	require.NoError(t, os.Chmod(dir, 0o700))
	if _, err := s.Get(ctx, "openai"); err == nil {
		t.Fatal("a credential whose write failed is readable from the cache")
	}
	cred, err := s.Get(ctx, "anthropic")
	require.NoError(t, err)
	require.Equal(t, "initial-key", cred.APIKey)
}

// An external writer's change must be picked up. The cache was stamped with
// time.Now() rather than the file's own mtime, so a write landing between the
// two instants could be treated as already-seen.
func TestFileBackend_PicksUpExternalWrites(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	ctx := context.Background()

	reader := NewFileBackend(authPath, Options{})
	other := NewFileBackend(authPath, Options{})

	require.NoError(t, other.Set(ctx, "anthropic", ai.NewAPIKeyCredential("v1")))
	cred, err := reader.Get(ctx, "anthropic")
	require.NoError(t, err)
	require.Equal(t, "v1", cred.APIKey)

	require.NoError(t, other.Set(ctx, "anthropic", ai.NewAPIKeyCredential("v2")))
	cred, err = reader.Get(ctx, "anthropic")
	require.NoError(t, err)
	require.Equal(t, "v2", cred.APIKey, "reader served a stale cached credential")
}

// auth.json holds secrets; the atomic-replace path must not widen its mode.
func TestFileBackend_AtomicReplaceKeepsPermissions(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	ctx := context.Background()

	s := NewFileBackend(authPath, Options{})
	require.NoError(t, s.Set(ctx, "anthropic", ai.NewAPIKeyCredential("k1")))
	require.NoError(t, s.Set(ctx, "openai", ai.NewAPIKeyCredential("k2")))

	info, err := os.Stat(authPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// The temp file used for the replace must not be left behind.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".auth-") {
			t.Errorf("temp file %q left behind after write", e.Name())
		}
	}
}
