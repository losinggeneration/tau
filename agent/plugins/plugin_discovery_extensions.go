package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/losinggeneration/tau/agent/artifacts"
	"github.com/losinggeneration/tau/agent/packages"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/paths"
	"github.com/pelletier/go-toml/v2"
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
	user, project := m.settings.ResourcePathsByScope("extensions")
	var all []string
	for path := range m.binaries {
		all = append(all, path)
	}
	allowed := make(map[string]bool, len(all))
	claimed := make(map[string]bool, len(all))
	for _, scope := range []struct {
		entries []string
		root    string
	}{{project, m.projectPluginDir()}, {user, m.userExtensionDir()}} {
		roots := []string{scope.root}
		var patterns []string
		for _, entry := range scope.entries {
			if isExtensionPattern(entry) {
				patterns = append(patterns, entry)
			} else {
				roots = append(roots, entry)
			}
		}
		var candidates []string
		for _, path := range all {
			if claimed[path] {
				continue
			}
			for _, root := range roots {
				if extensionPathWithin(root, path) {
					candidates = append(candidates, path)
					claimed[path] = true
					break
				}
			}
		}
		for _, path := range artifacts.ApplyFilter(candidates, patterns, "") {
			allowed[path] = true
		}
	}

	for _, path := range all {
		if allowed[path] || !claimed[path] {
			continue
		}
		if !m.isCLIExtension(path) {
			delete(m.binaries, path)
			delete(m.manifestPaths, path)
		}
	}
}

// isCLIExtension requires m.mu to be held.
func (m *Manager) isCLIExtension(path string) bool {
	for _, cliPath := range m.extensionPaths {
		if extensionPathWithin(paths.ResourcePath(cliPath, "", ""), path) {
			return true
		}
	}
	return false
}

func extensionPathWithin(root, path string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
	if !m.settings.ProjectResourcesTrustedAt(cwd) {
		return ""
	}
	return filepath.Join(cwd, ".tau", "extensions")
}

// scanDir scans a directory for plugin binaries.
func (m *Manager) scanDir(dir string) error {
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		if isPiExtensionFile(dir) {
			return m.scanPiExtensionFile(dir)
		}
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

	rootManifest := filepath.Join(dir, "tau-package.toml")
	if data, err := os.ReadFile(rootManifest); err == nil {
		var manifest Manifest
		if err := toml.Unmarshal(data, &manifest); err != nil {
			return fmt.Errorf("parse root extension manifest: %w", err)
		}
		if manifest.Plugin != nil {
			return m.scanPackageDir(dir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read root extension manifest: %w", err)
	}

	extensionEntries, err := packages.PiResourcePaths(dir, []string{"."}, packages.ResourceKindExtension)
	if err != nil {
		return fmt.Errorf("collect Pi extensions: %w", err)
	}
	for _, path := range extensionEntries {
		if err := m.scanPiExtensionFile(path); err != nil {
			return err
		}
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

		if isPiExtensionFile(path) {
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
			return fmt.Errorf("walk plugin extensions: %w", err)
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
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read extension manifest: %w", err)
			}
			if isIgnoredPluginSupportDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		var manifest Manifest
		if err := toml.Unmarshal(data, &manifest); err != nil {
			return fmt.Errorf("parse plugin extension manifest: %w", err)
		}
		if manifest.Plugin == nil {
			return nil
		}

		binaryPath := path
		if !manifest.HasRunCommand() {
			binaryPath, err = findPluginBinary(path, &manifest)
			if err != nil {
				return fmt.Errorf("find plugin extension binary: %w", err)
			}
		}

		hash, err := pluginDiscoveryHash(path, manifestPath, &manifest, binaryPath, m.computeHash)
		if err != nil {
			return fmt.Errorf("hash plugin extension: %w", err)
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

func isPiExtensionFile(path string) bool {
	switch filepath.Ext(path) {
	case ".js", ".ts":
		return true
	default:
		return false
	}
}

func (m *Manager) scanPiExtensionFile(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve extension path: %w", err)
	}
	args := []string{"node", filepath.Join(m.shimDir(), "index.js"), path}
	manifest := &Manifest{Run: &RunSection{Command: strings.Join(args, " "), args: args}}
	manifest.Package.Name = filepath.Base(path)
	hash, err := computeManagedShimHash(path, manifest)
	if err != nil {
		return fmt.Errorf("hash Pi extension: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.packageManifests[path] = manifest
	m.manifestPaths[path] = path
	m.binaries[path] = hash
	return nil
}

func (m *Manager) scanExtensionCandidates() error {
	m.mu.Lock()
	for path := range m.binaries {
		if m.packageBinaries[path] {
			continue
		}
		delete(m.packageManifests, m.manifestPaths[path])
		delete(m.manifestPaths, path)
		delete(m.binaries, path)
	}
	m.mu.Unlock()
	for _, dir := range m.pluginDirs() {
		if err := m.scanDir(dir); err != nil {
			return fmt.Errorf("scan extensions in %s: %w", dir, err)
		}
	}
	return nil
}

func (m *Manager) discoverReloadCandidates(ctx context.Context) (err error) {
	m.mu.RLock()
	binaries := maps.Clone(m.binaries)
	manifestPaths := maps.Clone(m.manifestPaths)
	packageBinaries := maps.Clone(m.packageBinaries)
	packageManifests := maps.Clone(m.packageManifests)
	m.mu.RUnlock()
	defer func() {
		if err == nil {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.binaries = binaries
		m.manifestPaths = manifestPaths
		m.packageBinaries = packageBinaries
		m.packageManifests = packageManifests
	}()
	if err := m.scanExtensionCandidates(); err != nil {
		return err
	}
	if err := m.scanConfiguredPackages(); err != nil {
		return fmt.Errorf("discover configured packages: %w", err)
	}
	m.filterConfiguredExtensions()
	return context.Cause(ctx)
}

// PreparedReload contains plugin candidates discovered without changing live state.
type PreparedReload struct {
	manager    *Manager
	candidates *Manager
}

// PrepareReload validates discovery with the proposed settings without restarting plugins.
func (m *Manager) PrepareReload(ctx context.Context, settings *ai.Settings) (*PreparedReload, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if settings == nil {
		settings = &ai.Settings{}
	}
	m.mu.RLock()
	candidates := &Manager{
		configDir: m.configDir, settings: settings, logger: m.logger,
		extensionPaths: slices.Clone(m.extensionPaths),
		binaries:       make(map[string]string), manifestPaths: make(map[string]string),
		packageManifests: make(map[string]*Manifest),
	}
	m.mu.RUnlock()
	if err := candidates.discoverReloadCandidates(ctx); err != nil {
		return nil, err
	}
	return &PreparedReload{manager: m, candidates: candidates}, nil
}

// Reload admits the prepared candidates and restarts the selected plugins.
func (p *PreparedReload) Reload(ctx context.Context, name string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return p.ReloadWithAdmission(ctx, name, nil)
}

// ReloadWithAdmission validates the staged processes before calling admit.
// If admit fails, the current plugins and discovery policy remain installed.
// The callback must not call the plugin manager.
func (p *PreparedReload) ReloadWithAdmission(ctx context.Context, name string, admit func() error) error {
	m := p.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if m.shimClosed {
		return errors.New("plugin manager is shut down")
	}

	changedConfig := make(map[string]bool)
	for pluginName := range m.plugins {
		var oldConfig, nextConfig json.RawMessage
		if current := m.plugins[pluginName]; current.configCaptured {
			oldConfig = current.configSnapshot
		} else if m.settings != nil {
			oldConfig = m.settings.Plugins[pluginName]
		}
		if p.candidates.settings != nil {
			nextConfig = p.candidates.settings.Plugins[pluginName]
		}
		changedConfig[pluginName] = !bytes.Equal(oldConfig, nextConfig)
	}
	for _, path := range slices.Sorted(maps.Keys(p.candidates.binaries)) {
		pluginName := filepath.Base(path)
		if name != "" && name != pluginName {
			continue
		}
		if current := m.plugins[pluginName]; current != nil && !current.deregistered && name == "" && current.Path == path && current.Hash == p.candidates.binaries[path] && !changedConfig[pluginName] {
			continue
		}
		manifestPath := p.candidates.manifestPaths[path]
		if manifestPath == "" {
			continue
		}
		manifest, err := p.candidates.packageManifest(manifestPath)
		if err != nil {
			return fmt.Errorf("prepare plugin manifest: %w", err)
		}
		if err := m.waitForShimReady(ctx, manifest); err != nil {
			return err
		}
	}
	previousSettings := m.settings
	previousBinaries := m.binaries
	previousPaths := m.manifestPaths
	previousManifests := m.packageManifests
	previousPackages := m.packageBinaries
	m.settings = p.candidates.settings
	m.binaries = p.candidates.binaries
	m.manifestPaths = p.candidates.manifestPaths
	m.packageManifests = p.candidates.packageManifests
	m.packageBinaries = p.candidates.packageBinaries
	admitted, reloadErr := m.reloadCandidates(ctx, name, changedConfig, admit)
	if !admitted {
		m.settings = previousSettings
		m.binaries = previousBinaries
		m.manifestPaths = previousPaths
		m.packageManifests = previousManifests
		m.packageBinaries = previousPackages
	}
	return reloadErr
}
