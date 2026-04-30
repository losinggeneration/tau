package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/losinggeneration/tau/piext"
)

func TestShimStartupEnsureRunsForManagedManifest(t *testing.T) {
	configDir := t.TempDir()
	pkgDir := filepath.Join(configDir, "packages", "pi-test")
	if err := os.MkdirAll(pkgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(pkgDir, "tau-package.toml")
	manifest := `[package]
name = "pi-test"
version = "0.0.1"

[plugin]
api_version = "1"

[run]
command = "node ` + filepath.ToSlash(filepath.Join(configDir, "pi-ext-shim", "index.js")) + ` pi-extension/index.js"
`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	var installs atomic.Int64
	mgr := NewManager(configDir, nil, nil, WithShimInstall(func(_ context.Context, dir string) error {
		installs.Add(1)
		if err := piext.WriteShimFilesystem(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dir, "node_modules", "jiti"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
	}))
	if err := mgr.scanPackagesDir(filepath.Join(configDir, "packages")); err != nil {
		t.Fatal(err)
	}

	mgr.mu.Lock()
	manifestParsed, err := parseManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	err = mgr.waitForShimReady(context.Background(), manifestParsed)
	mgr.mu.Unlock()
	if err != nil {
		t.Fatalf("waitForShimReady() error = %v", err)
	}
	if installs.Load() != 1 {
		t.Fatalf("install count = %d, want 1", installs.Load())
	}
	if !piext.IsShimCurrent(filepath.Join(configDir, "pi-ext-shim")) {
		t.Fatal("shim is not current")
	}
}

func TestShimStartupStatusMessages(t *testing.T) {
	configDir := t.TempDir()
	var messages []string
	mgr := NewManager(configDir, nil, nil,
		WithShimInstall(func(_ context.Context, dir string) error {
			if err := piext.WriteShimFilesystem(dir); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(dir, "node_modules", "jiti"), 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "node_modules", "jiti", "package.json"), []byte(`{"name":"jiti"}`), 0o600)
		}),
		WithShimStatus(func(message string) { messages = append(messages, message) }),
	)

	mgr.mu.Lock()
	mgr.startShimEnsureLocked(context.Background(), filepath.Join(configDir, "pi-ext-shim"))
	ready := mgr.shimReady
	mgr.mu.Unlock()
	if err := <-ready; err != nil {
		t.Fatalf("shim setup error = %v", err)
	}
	joined := strings.Join(messages, "\n")
	if !strings.Contains(joined, "Setting up pi-ext-shim") {
		t.Fatalf("status messages %q missing setup line", joined)
	}
}
