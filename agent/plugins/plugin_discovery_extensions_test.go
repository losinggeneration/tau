package plugins

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/losinggeneration/tau/ai"
)

func TestUserExtensionsDirResolution(t *testing.T) {
	t.Setenv("TAU_EXTENSIONS_DIR", "")
	cfgDir := t.TempDir()
	mgr := NewManager(cfgDir, nil, nil)

	// Default: <configDir>/extensions.
	if want := filepath.Join(cfgDir, "extensions"); mgr.userExtensionDir() != want {
		t.Fatalf("default = %q, want %q", mgr.userExtensionDir(), want)
	}

	// settings.json extensionsDir wins over the default.
	mgr.SetSettings(&ai.Settings{ExtensionDir: "/settings/ext"})
	if mgr.userExtensionDir() != "/settings/ext" {
		t.Fatalf("settings = %q, want /settings/ext", mgr.userExtensionDir())
	}

	// TAU_EXTENSIONS_DIR wins over settings.
	t.Setenv("TAU_EXTENSIONS_DIR", "/env/ext")
	if mgr.userExtensionDir() != "/env/ext" {
		t.Fatalf("env = %q, want /env/ext", mgr.userExtensionDir())
	}
}

func TestExtensionSettingsPathsAndModifiers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, "agent")
	extra := filepath.Join(cfgDir, "extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keep", "drop", "last"} {
		if err := os.WriteFile(filepath.Join(extra, name), []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	settingsFile := filepath.Join(cfgDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"extensions":["extra","!~/agent/extra/*","+extra/keep","+extra/last","-extra/last"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := ai.LoadSettings(settingsFile)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(cfgDir, nil, nil)
	mgr.SetSettings(settings)
	for _, dir := range mgr.pluginDirs() {
		if err := mgr.scanDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	mgr.filterConfiguredExtensions()
	if len(mgr.binaries) != 1 {
		t.Fatalf("binaries = %v", mgr.binaries)
	}
	if _, ok := mgr.binaries[filepath.Join(extra, "keep")]; !ok {
		t.Fatalf("missing keep: %v", mgr.binaries)
	}
	// Explicit CLI files bypass configured exclusions.
	mgr.AddExtensionPath("~/agent/extra/drop")
	for _, dir := range mgr.pluginDirs() {
		if err := mgr.scanDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	mgr.filterConfiguredExtensions()
	if _, ok := mgr.binaries[filepath.Join(extra, "drop")]; !ok {
		t.Fatalf("missing CLI file: %v", mgr.binaries)
	}
}
