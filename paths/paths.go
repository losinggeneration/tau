// Package paths resolves tau's on-disk locations. Every directory honors
// environment overrides so parallel tau installs (e.g. ~/.tau.work) can
// coexist; matching settings.json fields offer the same per-directory
// control at one level below the environment.
package paths

import (
	"os"
	"path/filepath"
	"strings"
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

// ResourcePath expands a resource entry without consuming its optional !, +, or -
// prefix. Relative paths are rooted at baseDir; an empty base leaves them relative.
func ResourcePath(entry, baseDir, homeDir string) string {
	var prefix string
	if len(entry) > 0 && (entry[0] == '!' || entry[0] == '+' || entry[0] == '-') {
		prefix, entry = entry[:1], entry[1:]
	}
	if entry == "~" || strings.HasPrefix(entry, "~/") || strings.HasPrefix(entry, `~\`) {
		if homeDir == "" {
			var err error
			homeDir, err = os.UserHomeDir()
			if err != nil {
				return prefix + entry
			}
		}
		entry = filepath.Join(homeDir, entry[1:])
	}
	entry = os.ExpandEnv(entry)
	if entry != "" && baseDir != "" && !filepath.IsAbs(entry) {
		entry = filepath.Join(baseDir, entry)
	}
	return prefix + entry
}
