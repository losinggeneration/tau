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
