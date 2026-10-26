package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/oauth"
	"github.com/stretchr/testify/require"
)

func TestAzureCredentialMigrationPreservesCanonicalKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	for _, data := range []string{
		`{"azure-openai-responses":{"type":"api_key","key":"legacy"}}`,
		`{"azure-openai-responses":{"type":"api_key","key":"legacy"},"azure":{"type":"api_key","key":"canonical"}}`,
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		store := NewFileBackend(path, Options{})
		credential, err := store.Get(t.Context(), ai.ProviderAzureOpenAI)
		if err != nil {
			t.Fatal(err)
		}
		if credential.APIKey != "legacy" && credential.APIKey != "canonical" {
			t.Fatalf("credential=%+v", credential)
		}
		if len(data) > 100 && credential.APIKey != "canonical" {
			t.Fatal("legacy key overwrote canonical")
		}
		if err := store.Set(t.Context(), ai.ProviderAzureOpenAI, credential); err != nil {
			t.Fatal(err)
		}
		file, err := parseAuthFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := file["azure-openai-responses"]; exists {
			t.Fatal("legacy key remains")
		}
	}
}

func TestFileBackendDoesNotPersistCanceledMutations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	store := NewFileBackend(path, Options{})
	if err := store.Set(t.Context(), "openai", ai.NewAPIKeyCredential("old")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.Set(ctx, "openai", ai.NewAPIKeyCredential("new")); !errors.Is(err, context.Canceled) {
		t.Errorf("Set error=%v", err)
	}
	if err := store.Delete(ctx, "openai"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete error=%v", err)
	}
	cred, err := store.Get(t.Context(), "openai")
	if err != nil || cred.APIKey != "old" {
		t.Fatalf("canceled mutation changed credentials=%+v err=%v", cred, err)
	}
}

func TestFileBackendCancelsContendedWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	store := NewFileBackend(path, Options{})
	go func() { result <- store.Set(ctx, "openai", ai.NewAPIKeyCredential("new")) }()
	cancel()
	var writeErr error
	select {
	case writeErr = <-result:
	case <-time.After(100 * time.Millisecond):
		t.Error("write did not honor cancellation while waiting for lock")
		if err := lock.Unlock(); err != nil {
			t.Error(err)
		}
		writeErr = <-result
	}
	if err := lock.Unlock(); err != nil {
		t.Error(err)
	}
	if !errors.Is(writeErr, context.Canceled) {
		t.Errorf("write error=%v", writeErr)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("canceled write created credential file: %v", err)
	}
}

func TestAcquireCredentialLockRetriesOnlyWindowsPermissionErrors(t *testing.T) {
	permission := fmt.Errorf("lock file: %w", syscall.EPERM)
	for _, tc := range []struct {
		name      string
		platform  string
		failure   error
		failures  int
		wantCalls int
	}{
		{"transient Windows permission", "windows", permission, 2, 3},
		{"persistent Windows permission", "windows", permission, 9, 6},
		{"Unix permission", "linux", permission, 1, 1},
		{"Windows other error", "windows", os.ErrNotExist, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			err := acquireCredentialLock(t.Context(), tc.platform, func() error {
				calls++
				if calls <= tc.failures {
					return tc.failure
				}
				return nil
			})
			if calls != tc.wantCalls {
				t.Errorf("calls=%d, want %d", calls, tc.wantCalls)
			}
			if tc.failures < tc.wantCalls {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, tc.failure) {
				t.Fatalf("lost cause: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	var calls int
	err := acquireCredentialLock(ctx, "windows", func() error {
		calls++
		cancel()
		return permission
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancellation err=%v calls=%d", err, calls)
	}
}

func TestEnsureDirReportsInvalidParents(t *testing.T) {
	for _, kind := range []string{"file", "loop"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "parent")
			if kind == "file" {
				if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink("parent", dir); err != nil {
					t.Fatal(err)
				}
			}
			backend := NewFileBackend(filepath.Join(dir, "auth.json"), Options{})
			if err := backend.EnsureDir(); err == nil {
				t.Fatal("invalid credential parent accepted")
			}
		})
	}
}

func TestEnsureDirCreatesMissingParentsAndRetainsPermissions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nested", "credentials")
	backend := NewFileBackend(filepath.Join(dir, "auth.json"), Options{})
	if err := backend.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("unsafe credential directory mode %v", info.Mode())
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := backend.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("existing permissions changed: %v", info.Mode())
	}
}

func TestGetOwnsCachedCredentialMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"api":{"type":"api_key","key":"key","env":{"MODE":"safe"}},"oauth":{"type":"oauth","access":"token","extra":{"nested":{"value":"safe"},"list":[{"value":"safe"}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileBackend(path, Options{})
	cred, err := store.Get(t.Context(), "api")
	if err != nil {
		t.Fatal(err)
	}
	cred.Env["MODE"] = "changed"
	cred, err = store.Get(t.Context(), "api")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Env["MODE"] != "safe" {
		t.Errorf("cached environment mutated: %v", cred.Env)
	}
	cred, err = store.Get(t.Context(), "oauth")
	if err != nil {
		t.Fatal(err)
	}
	cred.Extra["nested"].(map[string]any)["value"] = "changed"
	cred.Extra["list"].([]any)[0].(map[string]any)["value"] = "changed"
	cred, err = store.Get(t.Context(), "oauth")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Extra["nested"].(map[string]any)["value"] != "safe" || cred.Extra["list"].([]any)[0].(map[string]any)["value"] != "safe" {
		t.Errorf("cached OAuth metadata mutated: %v", cred.Extra)
	}
}

func TestFileBackendRejectsCanceledReads(t *testing.T) {
	store := NewFileBackend(filepath.Join(t.TempDir(), "auth.json"), Options{})
	if err := store.Set(t.Context(), "api", ai.NewAPIKeyCredential("key")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("credential request canceled")
	cancel(cause)
	if _, err := store.Get(ctx, "api"); !errors.Is(err, cause) {
		t.Errorf("Get lost cancellation: %v", err)
	}
	if _, err := store.List(ctx); !errors.Is(err, cause) {
		t.Errorf("List lost cancellation: %v", err)
	}
}

type terminalRefreshProvider struct {
	oauth.Provider
	refresh func(context.Context) error
}

func (p terminalRefreshProvider) RefreshToken(ctx context.Context, _ *oauth.Credentials) (*oauth.Credentials, error) {
	return nil, p.refresh(ctx)
}

func TestTerminalRefreshReportsPersistenceFailure(t *testing.T) {
	for _, method := range []string{"Get", "RefreshOAuth"} {
		t.Run(method, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			terminal := ai.ErrCredentialNeedsReauth{Provider: "provider"}
			refresh := func(context.Context) error {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return terminal
			}
			store := NewFileBackend(path, Options{RefreshFuncs: map[string]RefreshFunc{"provider": func(ctx context.Context, _ ai.Credential) (ai.Credential, error) {
				return ai.Credential{}, refresh(ctx)
			}}})
			if err := store.Set(t.Context(), "provider", ai.NewOAuthCredential("old", "refresh", time.Now().Add(-time.Hour), "scope")); err != nil {
				t.Fatal(err)
			}
			var err error
			if method == "Get" {
				_, err = store.Get(t.Context(), "provider")
			} else {
				_, err = store.RefreshOAuth(t.Context(), "provider", terminalRefreshProvider{refresh: refresh})
			}
			if !errors.Is(err, terminal) {
				t.Errorf("lost terminal error: %v", err)
			}
			var pathErr *os.PathError
			if !errors.As(err, &pathErr) {
				t.Errorf("lost persistence error: %v", err)
			}
		})
	}
}

func TestRefreshOAuthRejectsCanceledAdmission(t *testing.T) {
	store := NewFileBackend(filepath.Join(t.TempDir(), "auth.json"), Options{})
	if err := store.Set(t.Context(), "provider", ai.NewOAuthCredential("old", "refresh", time.Now().Add(time.Hour), "scope")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("refresh canceled")
	cancel(cause)
	provider := terminalRefreshProvider{refresh: func(context.Context) error {
		t.Error("provider called after canceled admission")
		return errors.New("provider invoked")
	}}
	if _, err := store.RefreshOAuth(ctx, "provider", provider); !errors.Is(err, cause) {
		t.Errorf("lost cancellation cause: %v", err)
	}
}

func TestRefreshWaitHonorsCancellation(t *testing.T) {
	for _, method := range []string{"Get", "RefreshOAuth"} {
		t.Run(method, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			firstDone := make(chan error, 1)
			store := NewFileBackend(filepath.Join(t.TempDir(), "auth.json"), Options{RefreshFuncs: map[string]RefreshFunc{"provider": func(context.Context, ai.Credential) (ai.Credential, error) {
				close(entered)
				<-release
				return ai.NewOAuthCredential("new", "refresh", time.Now().Add(time.Hour), "scope"), nil
			}}})
			if err := store.Set(t.Context(), "provider", ai.NewOAuthCredential("old", "refresh", time.Now().Add(-time.Hour), "scope")); err != nil {
				t.Fatal(err)
			}
			go func() { _, err := store.Get(t.Context(), "provider"); firstDone <- err }()
			<-entered
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if method == "Get" {
					_, err = store.Get(ctx, "provider")
				} else {
					_, err = store.RefreshOAuth(ctx, "provider", terminalRefreshProvider{refresh: func(context.Context) error { return errors.New("unexpected provider call") }})
				}
				done <- err
			}()
			received := false
			defer func() {
				close(release)
				if err := <-firstDone; err != nil {
					t.Error(err)
				}
				if !received {
					<-done
				}
			}()
			select {
			case err := <-done:
				received = true
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("lost wait cancellation: %v", err)
				}
			case <-time.After(200 * time.Millisecond):
				t.Error("refresh wait ignored deadline")
			}
		})
	}
}

type callbackRefreshProvider struct {
	oauth.Provider
	refresh func() (*oauth.Credentials, error)
}

func (p callbackRefreshProvider) RefreshToken(context.Context, *oauth.Credentials) (*oauth.Credentials, error) {
	return p.refresh()
}

func TestRefreshDoesNotOverwriteConcurrentCredentialChange(t *testing.T) {
	for _, method := range []string{"Get", "RefreshOAuth"} {
		for _, change := range []string{"login", "logout"} {
			for _, terminal := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/terminal=%v", method, change, terminal), func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "auth.json")
					other := NewFileBackend(path, Options{})
					refresh := func() (*oauth.Credentials, error) {
						var err error
						if change == "login" {
							err = other.Set(t.Context(), "provider", ai.NewAPIKeyCredential("new-login"))
						} else {
							err = other.Delete(t.Context(), "provider")
						}
						if err != nil {
							t.Fatal(err)
						}
						if terminal {
							return nil, ai.ErrCredentialNeedsReauth{Provider: "provider"}
						}
						return &oauth.Credentials{Access: "stale-refresh", Refresh: "rotated", Expires: time.Now().Add(time.Hour).UnixMilli()}, nil
					}
					store := NewFileBackend(path, Options{RefreshFuncs: map[string]RefreshFunc{"provider": func(context.Context, ai.Credential) (ai.Credential, error) {
						cred, err := refresh()
						return CredentialFromOAuth(cred), err
					}}})
					if err := store.Set(t.Context(), "provider", ai.NewOAuthCredential("old", "refresh", time.Now().Add(-time.Hour), "scope")); err != nil {
						t.Fatal(err)
					}
					var err error
					if method == "Get" {
						_, err = store.Get(t.Context(), "provider")
					} else {
						_, err = store.RefreshOAuth(t.Context(), "provider", callbackRefreshProvider{refresh: refresh})
					}
					if err == nil {
						t.Error("stale refresh reported success")
					}
					got, err := NewFileBackend(path, Options{}).Get(t.Context(), "provider")
					if change == "logout" {
						if !errors.Is(err, ai.ErrCredentialNotFound) {
							t.Errorf("refresh resurrected logout: %+v, %v", got, err)
						}
					} else if err != nil || got.APIKey != "new-login" {
						t.Errorf("refresh overwrote login: %+v, %v", got, err)
					}
				})
			}
		}
	}
}

func TestRefreshMetadataMutationDoesNotChangeConflictSnapshot(t *testing.T) {
	for _, method := range []string{"Get", "RefreshOAuth"} {
		t.Run(method, func(t *testing.T) {
			refresh := func(cred ai.Credential) ai.Credential {
				cred.Extra["nested"].(map[string]any)["value"] = "updated"
				cred.AccessToken = "new"
				cred.ExpiresAt = time.Now().Add(time.Hour)
				return cred
			}
			store := NewFileBackend(filepath.Join(t.TempDir(), "auth.json"), Options{RefreshFuncs: map[string]RefreshFunc{"provider": func(_ context.Context, cred ai.Credential) (ai.Credential, error) { return refresh(cred), nil }}})
			cred := ai.NewOAuthCredential("old", "refresh", time.Now().Add(-time.Hour), "scope")
			cred.Extra = map[string]any{"nested": map[string]any{"value": "original"}}
			if err := store.Set(t.Context(), "provider", cred); err != nil {
				t.Fatal(err)
			}
			var got ai.Credential
			var err error
			if method == "Get" {
				got, err = store.Get(t.Context(), "provider")
			} else {
				got, err = store.RefreshOAuth(t.Context(), "provider", metadataRefreshProvider{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.AccessToken != "new" || got.Extra["nested"].(map[string]any)["value"] != "updated" {
				t.Errorf("metadata update lost: %+v", got)
			}
		})
	}
}

type metadataRefreshProvider struct{ oauth.Provider }

func (metadataRefreshProvider) RefreshToken(_ context.Context, cred *oauth.Credentials) (*oauth.Credentials, error) {
	cred.Extra["nested"].(map[string]any)["value"] = "updated"
	cred.Access = "new"
	cred.Expires = time.Now().Add(time.Hour).UnixMilli()
	return cred, nil
}

func TestInvalidAuthShapeCannotBeReadOrRewritten(t *testing.T) {
	for _, data := range []string{`null`, `{"provider":null}`, `{"provider":{}}`, `{"provider":{"type":"unexpected","key":"private-value"}}`} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			store := NewFileBackend(path, Options{})
			if _, err := store.List(t.Context()); err == nil {
				t.Error("accepted invalid auth shape")
			} else if strings.Contains(err.Error(), "private-value") {
				t.Error("error leaked credential content")
			}
			if err := store.Set(t.Context(), "other", ai.NewAPIKeyCredential("new")); err == nil {
				t.Error("rewrote invalid auth file")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != data {
				t.Error("invalid auth file changed")
			}
		})
	}
}

func TestAuthShapeAcceptsSupportedCredentialTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	data := `{"api":{"type":"api_key","key":""},"oauth":{"type":"oauth","access":"token"},"none":{"type":"none"}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	refs, err := NewFileBackend(path, Options{}).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("got %d references", len(refs))
	}
}

func TestCredentialWritesPreserveUnknownStoredFields(t *testing.T) {
	for _, operation := range []string{"set other", "delete other", "refresh", "explicit refresh", "terminal refresh"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			data := `{"provider":{"type":"oauth","access":"old","refresh":"refresh","expires":1,"future":{"number":9007199254740993,"items":[true,null]}},"other":{"type":"api_key","key":"key"}}`
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			store := NewFileBackend(path, Options{RefreshFuncs: map[string]RefreshFunc{"provider": func(context.Context, ai.Credential) (ai.Credential, error) {
				return ai.NewOAuthCredential("new", "rotated", time.Now().Add(time.Hour), "scope"), nil
			}}})
			var err error
			switch operation {
			case "set other":
				err = store.Set(t.Context(), "other", ai.NewAPIKeyCredential("replacement"))
			case "delete other":
				err = store.Delete(t.Context(), "other")
			case "refresh":
				_, err = store.Get(t.Context(), "provider")
			case "explicit refresh":
				_, err = store.RefreshOAuth(t.Context(), "provider", callbackRefreshProvider{refresh: func() (*oauth.Credentials, error) {
					return &oauth.Credentials{Access: "explicit", Refresh: "rotated", Expires: time.Now().Add(time.Hour).UnixMilli()}, nil
				}})
			case "terminal refresh":
				_, err = store.RefreshOAuth(t.Context(), "provider", terminalRefreshProvider{refresh: func(context.Context) error { return ai.ErrCredentialNeedsReauth{Provider: "provider"} }})
				if !isErrCredentialNeedsReauth(err) {
					t.Fatalf("expected terminal refresh: %v", err)
				}
				err = nil
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var file map[string]map[string]json.RawMessage
			if err := json.Unmarshal(got, &file); err != nil {
				t.Fatal(err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, file["provider"]["future"]); err != nil {
				t.Fatalf("unknown metadata missing: %v", err)
			}
			if compact.String() != `{"number":9007199254740993,"items":[true,null]}` {
				t.Fatalf("changed unknown field: %s", compact.String())
			}
		})
	}
}

func TestAuthReplacementFailureCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "keep")
	if err := os.WriteFile(marker, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileBackend(path, Options{})
	if err := store.writeFileLocked(authFile{"provider": {Type: ai.CredentialTypeAPIKey, Key: "new"}}); err == nil {
		t.Fatal("replacement should fail")
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".auth-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary credentials left: %v", matches)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatal("changed existing target")
	}
}

func TestAuthFileAcceptsUTF8BOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	require.NoError(t, os.WriteFile(path, append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"provider":{"type":"api_key","key":"original"}}`)...), 0o600))
	store := NewFileBackend(path, Options{})
	got, err := store.Get(t.Context(), "provider")
	require.NoError(t, err)
	require.Equal(t, "original", got.APIKey)
	require.NoError(t, store.Set(t.Context(), "other", ai.NewAPIKeyCredential("new")))
	got, err = store.Get(t.Context(), "provider")
	require.NoError(t, err)
	require.Equal(t, "original", got.APIKey)
	got, err = store.Get(t.Context(), "other")
	require.NoError(t, err)
	require.Equal(t, "new", got.APIKey)
}

func TestAuthMutationLockWaitRetainsCancellationCause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	store := NewFileBackend(path, Options{})
	require.NoError(t, store.Set(t.Context(), "provider", ai.NewAPIKeyCredential("original")))
	lock := flock.New(path + ".lock")
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	cause := errors.New("auth write canceled while waiting")
	ctx, cancel := context.WithTimeoutCause(t.Context(), time.Millisecond, cause)
	defer cancel()
	require.ErrorIs(t, store.Set(ctx, "provider", ai.NewAPIKeyCredential("replacement")), cause)
	got, err := store.Get(t.Context(), "provider")
	require.NoError(t, err)
	require.Equal(t, "original", got.APIKey)
}

func TestFileBackendObservesCredentialEditsWithUnchangedMetadata(t *testing.T) {
	for _, operation := range []string{"overwrite", "replace", "invalid", "delete provider"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			original := []byte(`{"api":{"type":"api_key","key":"old"}}`)
			require.NoError(t, os.WriteFile(path, original, 0o600))
			backend := NewFileBackend(path, Options{})
			credential, err := backend.Get(t.Context(), "api")
			require.NoError(t, err)
			require.Equal(t, "old", credential.APIKey)
			info, err := os.Stat(path)
			require.NoError(t, err)
			replacement := []byte(`{"api":{"type":"api_key","key":"new"}}`)
			switch operation {
			case "invalid":
				replacement = bytes.Repeat([]byte("!"), len(original))
			case "delete provider":
				replacement = append([]byte(`{}`), bytes.Repeat([]byte(" "), len(original)-2)...)
			}
			require.Len(t, replacement, len(original))
			if operation == "replace" {
				temporary := filepath.Join(filepath.Dir(path), "replacement")
				require.NoError(t, os.WriteFile(temporary, replacement, 0o600))
				require.NoError(t, os.Chtimes(temporary, info.ModTime(), info.ModTime()))
				require.NoError(t, os.Rename(temporary, path))
			} else {
				require.NoError(t, os.WriteFile(path, replacement, 0o600))
				require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
			}
			credential, err = backend.Get(t.Context(), "api")
			switch operation {
			case "invalid":
				require.ErrorContains(t, err, "parse auth.json")
			case "delete provider":
				require.ErrorContains(t, err, "credential not found")
				require.Empty(t, credential.APIKey)
			default:
				require.NoError(t, err)
				require.Equal(t, "new", credential.APIKey)
			}
		})
	}
}

func TestFileRefreshRetainsCancellationAndProviderFailure(t *testing.T) {
	for _, method := range []string{"Get", "RefreshOAuth"} {
		t.Run(method, func(t *testing.T) {
			cause := errors.New("refresh withdrawn")
			failure := errors.New("provider disconnected")
			ctx, cancel := context.WithCancelCause(t.Context())
			refresh := func(context.Context) error { cancel(cause); return failure }
			backend := NewFileBackend(filepath.Join(t.TempDir(), "auth.json"), Options{RefreshFuncs: map[string]RefreshFunc{"provider": func(ctx context.Context, _ ai.Credential) (ai.Credential, error) {
				return ai.Credential{}, refresh(ctx)
			}}})
			require.NoError(t, backend.Set(t.Context(), "provider", ai.NewOAuthCredential("old", "refresh", time.Now().Add(-time.Hour), "scope")))
			var err error
			if method == "Get" {
				_, err = backend.Get(ctx, "provider")
			} else {
				_, err = backend.RefreshOAuth(ctx, "provider", terminalRefreshProvider{refresh: refresh})
			}
			require.ErrorIs(t, err, cause)
			require.ErrorIs(t, err, failure)
			stored, err := backend.readCredential("provider")
			require.NoError(t, err)
			require.Equal(t, "old", stored.AccessToken)
		})
	}
}
