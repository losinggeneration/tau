package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "tau-cli-tests-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create cli test temp dir: %v\n", err)
		os.Exit(1)
	}

	homeDir := filepath.Join(tmpDir, "home")
	configDir := filepath.Join(tmpDir, "config")
	sessionsDir := filepath.Join(tmpDir, "sessions")
	workDir := filepath.Join(tmpDir, "work")
	for _, dir := range []string{homeDir, configDir, sessionsDir, workDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "create cli test dir %s: %v\n", dir, err)
			os.Exit(1)
		}
	}

	oldHome, hadHome := os.LookupEnv("HOME")
	oldUserProfile, hadUserProfile := os.LookupEnv("USERPROFILE")
	oldConfigDir, hadConfigDir := os.LookupEnv("TAU_CONFIG_DIR")
	oldSessionDir, hadSessionDir := os.LookupEnv("TAU_SESSION_DIR")
	oldWd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "get cli test cwd: %v\n", err)
		os.Exit(1)
	}

	mustSetenv := func(key, value string) {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintf(os.Stderr, "set %s: %v\n", key, err)
			os.Exit(1)
		}
	}
	mustSetenv("HOME", homeDir)
	mustSetenv("USERPROFILE", homeDir)
	mustSetenv("TAU_CONFIG_DIR", configDir)
	mustSetenv("TAU_SESSION_DIR", sessionsDir)
	if err := os.Chdir(workDir); err != nil {
		fmt.Fprintf(os.Stderr, "chdir cli test work dir: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := os.Chdir(oldWd); err != nil {
		fmt.Fprintf(os.Stderr, "restore cli test cwd: %v\n", err)
		code = 1
	}
	restoreTestEnv("HOME", oldHome, hadHome)
	restoreTestEnv("USERPROFILE", oldUserProfile, hadUserProfile)
	restoreTestEnv("TAU_CONFIG_DIR", oldConfigDir, hadConfigDir)
	restoreTestEnv("TAU_SESSION_DIR", oldSessionDir, hadSessionDir)
	if err := os.RemoveAll(tmpDir); err != nil {
		fmt.Fprintf(os.Stderr, "remove cli test temp dir: %v\n", err)
		code = 1
	}

	os.Exit(code)
}

func restoreTestEnv(key, value string, ok bool) {
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
