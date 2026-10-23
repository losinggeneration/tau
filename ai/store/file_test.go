package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/losinggeneration/tau/ai"
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
