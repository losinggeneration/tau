package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/losinggeneration/tau/agent/artifacts"
	"github.com/losinggeneration/tau/paths"
)

// pluginDirs returns the plugin search paths.
func (m *Manager) pluginDirs() []string {
	var dirs []string

	// Project-local first, then global.
	if projectDir := m.projectPluginDir(); projectDir != "" {
		dirs = append(dirs, projectDir)
	}
	dirs = append(dirs, m.userExtensionDir())

	// Add extension paths from CLI flags (--extension).
	// Caller holds the lock (ReloadNamed holds m.mu.Lock(),
	// discoverUnlocked is only called from autoLoadLocked which holds the lock).
	for _, extPath := range m.extensionPaths {
		if extPath != "" {
			dirs = append(dirs, paths.ResourcePath(extPath, "", ""))
		}
	}

	for _, entry := range m.settings.ResourcePaths("extensions") {
		if !isExtensionPattern(entry) {
			dirs = append(dirs, entry)
		}
	}
	return dirs
}

func isExtensionPattern(entry string) bool {
	return strings.HasPrefix(entry, "!") || strings.HasPrefix(entry, "+") ||
		strings.HasPrefix(entry, "-") || strings.ContainsAny(entry, "*?")
}

func (m *Manager) filterConfiguredExtensions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	var patterns []string
	for _, entry := range m.settings.ResourcePaths("extensions") {
		if isExtensionPattern(entry) {
			patterns = append(patterns, entry)
		}
	}
	if len(patterns) == 0 {
		return
	}
	all := make([]string, 0, len(m.binaries))
	for path := range m.binaries {
		all = append(all, path)
	}
	enabled := artifacts.ApplyFilter(all, patterns, "")
	allowed := make(map[string]bool, len(enabled))
	for _, path := range enabled {
		allowed[path] = true
	}
	for _, path := range all {
		if allowed[path] {
			continue
		}
		explicit := false
		for _, cliPath := range m.extensionPaths {
			cliPath = paths.ResourcePath(cliPath, "", "")
			rel, err := filepath.Rel(cliPath, path)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				explicit = true
				break
			}
		}
		if !explicit {
			delete(m.binaries, path)
			delete(m.manifestPaths, path)
		}
	}
}

// userExtensionDir resolves the user-global extensions directory:
// TAU_EXTENSIONS_DIR, then extensionsDir from settings.json, then
// <configDir>/extensions.
func (m *Manager) userExtensionDir() string {
	var configured string
	if m.settings != nil {
		configured = m.settings.ExtensionDir
	}
	return paths.Dir("TAU_EXTENSIONS_DIR", configured, filepath.Join(m.configDir, "extensions"))
}

// AddExtensionPath adds a path to the list of extension search paths.
// Extensions from these paths are scanned in addition to the standard directories.
func (m *Manager) AddExtensionPath(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.extensionPaths = append(m.extensionPaths, path)
}

// projectPluginDir returns the project-local plugin directory.
func (m *Manager) projectPluginDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Join(cwd, ".tau", "extensions")
}

// scanDir scans a directory for plugin binaries.
func (m *Manager) scanDir(dir string) error {
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		if info.Mode()&0o111 == 0 {
			return nil
		}
		hash, err := m.computeHash(dir)
		if err != nil {
			return fmt.Errorf("hash extension: %w", err)
		}
		m.mu.Lock()
		m.binaries[dir] = hash
		m.manifestPaths[dir] = ""
		m.mu.Unlock()
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read dir: %w", err)
	}

	// Collect and sort loose executable entries alphabetically for consistent
	// load order. Directory-style plugins are handled below by looking for
	// tau-package.toml files.
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)

	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		// Skip non-executable files.
		if info.Mode()&0o111 == 0 {
			continue
		}

		// Compute hash.
		hash, err := m.computeHash(path)
		if err != nil {
			continue
		}

		m.mu.Lock()
		m.binaries[path] = hash
		m.manifestPaths[path] = "" // loose binaries have no manifest
		m.mu.Unlock()
	}

	return m.scanNestedManifestPlugins(dir)
}

func (m *Manager) scanNestedManifestPlugins(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			m.logger.Warn("plugin extension walk failed", "path", path, "error", err)
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path == root {
			return nil
		}

		manifestPath := filepath.Join(path, "tau-package.toml")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			if isIgnoredPluginSupportDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !hasPluginSection(data) {
			return nil
		}

		manifest, err := parseManifest(manifestPath)
		if err != nil {
			m.logger.Warn("plugin extension manifest parse failed", "path", manifestPath, "error", err)
			return filepath.SkipDir
		}

		binaryPath, err := findPluginBinary(path, manifest)
		if err != nil {
			m.logger.Warn("plugin extension binary not found", "dir", path, "error", err)
			return filepath.SkipDir
		}

		hash, err := m.computeHash(binaryPath)
		if err != nil {
			m.logger.Warn("plugin extension hash failed", "path", binaryPath, "error", err)
			return filepath.SkipDir
		}

		m.mu.Lock()
		m.binaries[binaryPath] = hash
		m.manifestPaths[binaryPath] = manifestPath
		m.mu.Unlock()

		// The manifest directory is a plugin boundary. Do not recurse into its
		// runtime/support directories, which may contain executable files that are
		// not tau plugins (for example .venv/bin/python).
		return filepath.SkipDir
	})
}

func isIgnoredPluginSupportDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", ".venv", "node_modules", "vendor", "__pycache__":
		return true
	default:
		return false
	}
}
