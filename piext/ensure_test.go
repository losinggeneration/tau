package piext

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestEnsureShimMaterializesAndInstalls(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shim")
	var installed atomic.Int64
	got, err := EnsureShim(context.Background(), dir, func(_ context.Context, installDir string) error {
		installed.Add(1)
		if err := os.MkdirAll(filepath.Join(installDir, "node_modules", "jiti"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(installDir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
	}, nil)
	if err != nil {
		t.Fatalf("EnsureShim() error = %v", err)
	}
	if got != dir {
		t.Fatalf("EnsureShim() = %q, want %q", got, dir)
	}
	if installed.Load() != 1 {
		t.Fatalf("install count = %d, want 1", installed.Load())
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		t.Fatalf("package.json missing: %v", err)
	}
}

func TestEnsureShimFastPathSkipsInstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shim")
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "jiti"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tau-shim-hash"), []byte(EmbeddedShimHash()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := EnsureShim(context.Background(), dir, func(context.Context, string) error {
		t.Fatal("install should not run")
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("EnsureShim() error = %v", err)
	}
}

func TestEnsureShimRefreshesChangedSources(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shim")
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "jiti"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), EmbeddedPackageJSON(), 0o600); err != nil {
		t.Fatal(err)
	}

	var installed atomic.Int64
	_, err := EnsureShim(context.Background(), dir, func(_ context.Context, installDir string) error {
		installed.Add(1)
		if err := os.MkdirAll(filepath.Join(installDir, "node_modules", "jiti"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(installDir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
	}, nil)
	if err != nil {
		t.Fatalf("EnsureShim() error = %v", err)
	}
	if installed.Load() != 1 {
		t.Fatalf("install count = %d, want 1", installed.Load())
	}
}

func TestEnsureShimStaleLockTakeover(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shim")
	if err := os.WriteFile(dir+".lock", []byte("999999999\n2024-01-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := EnsureShim(context.Background(), dir, func(_ context.Context, installDir string) error {
		if err := os.MkdirAll(filepath.Join(installDir, "node_modules", "jiti"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(installDir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
	}, nil)
	if err != nil {
		t.Fatalf("EnsureShim() error = %v", err)
	}
	if _, err := os.Stat(dir + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock still exists: %v", err)
	}
}

func TestEnsureShimConcurrentSingleInstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shim")
	var installed atomic.Int64
	errCh := make(chan error, 2)
	install := func(_ context.Context, installDir string) error {
		installed.Add(1)
		if err := os.MkdirAll(filepath.Join(installDir, "node_modules", "jiti"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(installDir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
	}
	for range 2 {
		go func() {
			_, err := EnsureShim(context.Background(), dir, install, nil)
			errCh <- err
		}()
	}
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("EnsureShim() error = %v", err)
		}
	}
	if installed.Load() != 1 {
		t.Fatalf("install count = %d, want 1", installed.Load())
	}
}
