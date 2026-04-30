package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/losinggeneration/tau/commands"
)

func TestAutoLoadRunCommandPackagesAreKeyedByPackage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packagesDir := filepath.Join(root, "packages")
	writeRunCommandPackage(t, packagesDir, "good")
	writeRunCommandPackage(t, packagesDir, "bad")

	reg := commands.NewInMemoryRegistry()
	m := NewManager(t.TempDir(), nil, reg)
	m.loadFn = func(ctx context.Context, path string, manifestPath ...string) (*PluginInfo, error) {
		if filepath.Base(path) == "bad" {
			return nil, fmt.Errorf("boom")
		}
		info := &PluginInfo{
			Name:         filepath.Base(path),
			Path:         path,
			ManifestPath: manifestPath[0],
			Registrations: Registrations{Commands: []CommandRegistration{{
				Name:        "pony",
				Description: "pony command",
			}}},
		}
		m.plugins[info.Name] = info
		m.registerPlugin(info)
		return info, nil
	}

	if err := m.scanPackagesDir(packagesDir); err != nil {
		t.Fatalf("scan packages: %v", err)
	}
	if len(m.binaries) != 2 {
		t.Fatalf("discovered binaries = %#v, want two package keys", m.binaries)
	}
	if err := m.AutoLoad(context.Background()); err == nil {
		t.Fatalf("AutoLoad error = nil, want bad package error")
	}
	if _, _, ok := reg.Lookup("pony"); !ok {
		t.Fatalf("good package command was not registered")
	}
}

func writeRunCommandPackage(t *testing.T, packagesDir, name string) {
	t.Helper()
	dir := filepath.Join(packagesDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir package: %v", err)
	}
	manifest := `[package]
name = "` + name + `"
version = "1.0.0"

[plugin]
api_version = "1"

[run]
command = "node /tmp/shim/index.js index.js"
`
	if err := os.WriteFile(filepath.Join(dir, "tau-package.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}
