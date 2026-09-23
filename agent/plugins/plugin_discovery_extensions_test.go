package plugins

import (
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
