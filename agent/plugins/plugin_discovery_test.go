package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestScanPackagesDirFindsScopedPackageManifest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "@scope", "pkg")
	if err := os.MkdirAll(pkgDir, 0o700); err != nil {
		t.Fatalf("mkdir package: %v", err)
	}
	manifest := `[package]
name = "@scope/pkg"
version = "1.0.0"

[plugin]
api_version = "1"

[run]
command = "node"
args = ["index.js"]
`
	if err := os.WriteFile(filepath.Join(pkgDir, "tau-package.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	m := NewManager(t.TempDir(), nil, nil)
	m.loadFn = func(ctx context.Context, path string, manifestPath ...string) (*PluginInfo, error) {
		t.Fatal("loadFn should not be called")
		return nil, nil
	}

	if err := m.scanPackagesDir(dir); err != nil {
		t.Fatalf("scan packages: %v", err)
	}

	wantManifest := filepath.Join(pkgDir, "tau-package.toml")
	if got := m.manifestPaths[filepath.Join(pkgDir, "node")]; got != "" {
		t.Fatalf("unexpected fake node manifest path = %q", got)
	}
	if len(m.manifestPaths) != 1 {
		t.Fatalf("manifest paths = %#v, want one", m.manifestPaths)
	}
	for _, got := range m.manifestPaths {
		if got != wantManifest {
			t.Fatalf("manifest path = %q, want %q", got, wantManifest)
		}
	}
}

func TestScanPackagesDirDoesNotSearchPastScopedDepth(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "too", "deep", "pkg")
	if err := os.MkdirAll(pkgDir, 0o700); err != nil {
		t.Fatalf("mkdir package: %v", err)
	}
	manifest := `[package]
name = "deep"
version = "1.0.0"

[plugin]
api_version = "1"

[run]
command = "node"
args = ["index.js"]
`
	if err := os.WriteFile(filepath.Join(pkgDir, "tau-package.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	m := NewManager(t.TempDir(), nil, nil)
	if err := m.scanPackagesDir(dir); err != nil {
		t.Fatalf("scan packages: %v", err)
	}
	if len(m.manifestPaths) != 0 {
		t.Fatalf("manifest paths = %#v, want none", m.manifestPaths)
	}
}
