package plugins

import (
	"fmt"
	"os"
	"path/filepath"
)

// packageDirs returns the package search paths (project-local first, then global).
func (m *Manager) packageDirs() []string {
	home, _ := os.UserHomeDir()

	var dirs []string

	// Project-local first, then global.
	if projectDir := m.projectPluginDir(); projectDir != "" {
		// Project-local packages: .tau/packages/
		projectPkgs := filepath.Join(filepath.Dir(projectDir), "packages")
		dirs = append(dirs, projectPkgs)
	}
	dirs = append(dirs, filepath.Join(home, ".tau", "agent", "packages"))

	return dirs
}

// scanPackagesDir scans a packages directory for tau-package.toml files with [plugin] sections.
func (m *Manager) scanPackagesDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read dir: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}

		pkgDir := filepath.Join(dir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(pkgDir)
			if err == nil {
				pkgDir = resolved
			}
		}
		if entry.Name()[0] == '@' {
			scopedEntries, err := os.ReadDir(pkgDir)
			if err != nil {
				continue
			}
			for _, scopedEntry := range scopedEntries {
				if !scopedEntry.IsDir() && scopedEntry.Type()&os.ModeSymlink == 0 {
					continue
				}
				m.scanPackageDir(filepath.Join(pkgDir, scopedEntry.Name()))
			}
			continue
		}

		m.scanPackageDir(pkgDir)
	}

	return nil
}

func (m *Manager) scanPackageDir(pkgDir string) {
	resolved, err := filepath.EvalSymlinks(pkgDir)
	if err == nil {
		pkgDir = resolved
	}
	manifestPath := filepath.Join(pkgDir, "tau-package.toml")

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return
	}

	// Check if the manifest has a [plugin] section.
	if !hasPluginSection(data) {
		return
	}

	manifest, err := parseManifest(manifestPath)
	if err != nil {
		m.logger.Warn("plugin package manifest parse failed", "dir", pkgDir, "error", err)
		return
	}

	binaryPath := pkgDir
	if !manifest.HasRunCommand() {
		// Find the plugin binary in the package directory.
		var err error
		binaryPath, err = findPluginBinary(pkgDir, manifest)
		if err != nil {
			m.logger.Warn("plugin package binary not found", "dir", pkgDir, "error", err)
			return
		}
	}

	// Compute hash and register.
	hash, err := pluginDiscoveryHash(pkgDir, manifestPath, manifest, binaryPath, m.computeHash)
	if err != nil {
		return
	}

	m.mu.Lock()
	m.binaries[binaryPath] = hash
	m.manifestPaths[binaryPath] = manifestPath
	m.mu.Unlock()
}

// hasPluginSection checks if the tau-package.toml content has a [plugin] section.
func hasPluginSection(data []byte) bool {
	// Simple check: look for "[plugin]" as a section header.
	// TOML section headers are [section_name] on its own line.
	content := string(data)
	for _, line := range splitLines(content) {
		line = trimSpace(line)
		if line == "[plugin]" {
			return true
		}
	}
	return false
}

// findPluginBinary looks for an executable in the package directory.
// It prefers a binary named after the package directory, or any executable.
func findPluginBinary(pkgDir string, manifest *Manifest) (string, error) {
	if manifest != nil && manifest.HasRunCommand() {
		if resolved := resolveRunCommandPath(pkgDir, manifest.Run.Command); resolved != "" {
			return resolved, nil
		}
	}

	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return "", err
	}

	var executables []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.Mode()&0o111 != 0 {
			executables = append(executables, filepath.Join(pkgDir, entry.Name()))
		}
	}

	if len(executables) == 0 {
		return "", fmt.Errorf("no executable found in %s", pkgDir)
	}

	return executables[0], nil
}
