package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// originalWD is the package source directory before TestMain chdirs to the
// temp work dir. Tests that reference repo-relative fixtures (e.g. the
// pi-ext-shim interop test) use it.
var originalWD string

func TestMain(m *testing.M) {
	if runPluginLoadTestHelperIfRequested() {
		return
	}

	var err error
	originalWD, err = os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "get plugin test cwd: %v\n", err)
		os.Exit(1)
	}

	tmpDir, err := os.MkdirTemp("", "tau-plugin-tests-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create plugin test temp dir: %v\n", err)
		os.Exit(1)
	}

	homeDir := filepath.Join(tmpDir, "home")
	workDir := filepath.Join(tmpDir, "work")
	for _, dir := range []string{homeDir, workDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "create plugin test dir %s: %v\n", dir, err)
			os.Exit(1)
		}
	}

	oldHome, hadHome := os.LookupEnv("HOME")
	oldUserProfile, hadUserProfile := os.LookupEnv("USERPROFILE")
	oldConfigDir, hadConfigDir := os.LookupEnv("TAU_CONFIG_DIR")
	oldSessionDir, hadSessionDir := os.LookupEnv("TAU_SESSION_DIR")
	mustSetenv := func(key, value string) {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintf(os.Stderr, "set %s: %v\n", key, err)
			os.Exit(1)
		}
	}
	mustSetenv("HOME", homeDir)
	mustSetenv("USERPROFILE", homeDir)
	mustSetenv("TAU_CONFIG_DIR", filepath.Join(tmpDir, "config"))
	mustSetenv("TAU_SESSION_DIR", filepath.Join(tmpDir, "sessions"))
	oldWd := originalWD
	if err := os.Chdir(workDir); err != nil {
		fmt.Fprintf(os.Stderr, "chdir plugin test work dir: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := os.Chdir(oldWd); err != nil {
		fmt.Fprintf(os.Stderr, "restore plugin test cwd: %v\n", err)
		code = 1
	}
	restorePluginTestEnv("HOME", oldHome, hadHome)
	restorePluginTestEnv("USERPROFILE", oldUserProfile, hadUserProfile)
	restorePluginTestEnv("TAU_CONFIG_DIR", oldConfigDir, hadConfigDir)
	restorePluginTestEnv("TAU_SESSION_DIR", oldSessionDir, hadSessionDir)
	if err := os.RemoveAll(tmpDir); err != nil {
		fmt.Fprintf(os.Stderr, "remove plugin test temp dir: %v\n", err)
		code = 1
	}

	os.Exit(code)
}

func restorePluginTestEnv(key, value string, ok bool) {
	var err error
	if ok {
		err = os.Setenv(key, value)
	} else {
		err = os.Unsetenv(key)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore %s: %v\n", key, err)
	}
}
