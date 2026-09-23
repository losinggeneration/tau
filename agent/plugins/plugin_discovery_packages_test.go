package plugins

import (
	"path/filepath"
	"testing"

	"github.com/losinggeneration/tau/ai"
)

func TestUserPackagesDirResolution(t *testing.T) {
	t.Setenv("TAU_PACKAGE_DIR", "")
	cfgDir := t.TempDir()
	mgr := NewManager(cfgDir, nil, nil)

	// Default: <configDir>/packages.
	if want := filepath.Join(cfgDir, "packages"); mgr.userPackageDir() != want {
		t.Fatalf("default = %q, want %q", mgr.userPackageDir(), want)
	}

	// settings.json packageDir wins over the default.
	mgr.SetSettings(&ai.Settings{PackageDir: "/settings/pkgs"})
	if mgr.userPackageDir() != "/settings/pkgs" {
		t.Fatalf("settings = %q, want /settings/pkgs", mgr.userPackageDir())
	}

	// TAU_PACKAGE_DIR wins over settings.
	t.Setenv("TAU_PACKAGE_DIR", "/env/pkgs")
	if mgr.userPackageDir() != "/env/pkgs" {
		t.Fatalf("env = %q, want /env/pkgs", mgr.userPackageDir())
	}
}
