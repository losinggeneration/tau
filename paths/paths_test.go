package paths

import (
	"path/filepath"
	"testing"
)

func TestConfigDirEnvOverride(t *testing.T) {
	t.Setenv("TAU_CONFIG_DIR", "/tmp/tau-work/agent")
	if got := ConfigDir(); got != "/tmp/tau-work/agent" {
		t.Fatalf("ConfigDir() = %q, want /tmp/tau-work/agent", got)
	}
}

func TestConfigDirDefault(t *testing.T) {
	t.Setenv("TAU_CONFIG_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := ConfigDir(); got != filepath.Join(home, ".tau", "agent") {
		t.Fatalf("ConfigDir() = %q, want %q", got, filepath.Join(home, ".tau", "agent"))
	}
}

func TestDirChain(t *testing.T) {
	t.Setenv("TAU_TEST_DIR", "/env/dir")
	if got := Dir("TAU_TEST_DIR", "/settings/dir", "/def"); got != "/env/dir" {
		t.Fatalf("env should win: got %q", got)
	}

	t.Setenv("TAU_TEST_DIR", "")
	if got := Dir("TAU_TEST_DIR", "/settings/dir", "/def"); got != "/settings/dir" {
		t.Fatalf("settings should win over default: got %q", got)
	}

	if got := Dir("TAU_TEST_DIR", "", "/def"); got != "/def" {
		t.Fatalf("default should win when unset: got %q", got)
	}
}

func TestResourcePath(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(t.TempDir(), "config")
	t.Setenv("RESOURCE_TEST_PATH", "shared")
	for _, tc := range []struct{ entry, want string }{
		{"~", home},
		{"~/skills", filepath.Join(home, "skills")},
		{"!~/skills/*", "!" + filepath.Join(home, "skills", "*")},
		{"+./skills/a", "+" + filepath.Join(base, "skills", "a")},
		{"-../shared/b", "-" + filepath.Join(filepath.Dir(base), "shared", "b")},
		{"!$RESOURCE_TEST_PATH/*", "!" + filepath.Join(base, "shared", "*")},
		{"", ""},
		{"!", "!"},
	} {
		if got := ResourcePath(tc.entry, base, home); got != tc.want {
			t.Errorf("%q = %q, want %q", tc.entry, got, tc.want)
		}
	}
}
