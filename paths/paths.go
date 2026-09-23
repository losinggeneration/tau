// Package paths resolves tau's on-disk locations. Every directory honors
// environment overrides so parallel tau installs (e.g. ~/.tau.work) can
// coexist; matching settings.json fields offer the same per-directory
// control at one level below the environment.
package paths

import (
	"os"
	"path/filepath"
)

// ConfigDir returns the tau agent config directory: TAU_CONFIG_DIR, else
// ~/.tau/agent. Settings, credentials, sessions, plugins, and artifacts
// all root here unless individually overridden.
func ConfigDir() string {
	if dir := os.Getenv("TAU_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".tau"
	}
	return filepath.Join(home, ".tau", "agent")
}

// Dir resolves a directory override chain: the environment variable named
// by env, then the settings.json value, then the default. Empty strings
// fall through to the next level.
func Dir(env, configured, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	if configured != "" {
		return configured
	}
	return def
}
