package plugins

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/losinggeneration/tau/agent/artifacts"
	"github.com/losinggeneration/tau/agent/packages"
	"github.com/losinggeneration/tau/paths"
)

// packageDirs returns the package search paths (project-local first, then global).
func (m *Manager) packageDirs() []string {
	var dirs []string

	// Project-local first, then global.
	if projectDir := m.projectPluginDir(); projectDir != "" {
		// Project-local packages: .tau/packages/
		projectPkgs := filepath.Join(filepath.Dir(projectDir), "packages")
		dirs = append(dirs, projectPkgs)
	}
	dirs = append(dirs, m.userPackageDir())

	return dirs
}

// userPackageDir resolves the user-global packages directory:
// TAU_PACKAGE_DIR, then packageDir from settings.json, then
// <configDir>/packages.
func (m *Manager) userPackageDir() string {
	var configured string
	if m.settings != nil {
		configured = m.settings.PackageDir
	}
	return paths.Dir("TAU_PACKAGE_DIR", configured, filepath.Join(m.configDir, "packages"))
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

func (m *Manager) scanConfiguredPackages() error {
	dirs := m.packageDirs()
	projectDir := ""
	if len(dirs) > 1 {
		projectDir = dirs[0]
	}
	resolved, err := packages.ResolvePackageSources(m.settings, m.userPackageDir(), projectDir)
	if err != nil {
		return err
	}
	m.mu.Lock()
	for path := range m.packageBinaries {
		delete(m.packageManifests, m.manifestPaths[path])
		if m.isCLIExtension(path) {
			continue
		}
		delete(m.binaries, path)
		delete(m.manifestPaths, path)
	}
	m.packageBinaries = make(map[string]bool)
	m.mu.Unlock()
	for _, pkg := range resolved {
		if _, err := os.Stat(filepath.Join(pkg.Root, "tau-package.toml")); errors.Is(err, os.ErrNotExist) {
			if err := m.scanPiPackage(pkg); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return fmt.Errorf("stat package manifest: %w", err)
		}
		m.scanPackageDir(pkg.Root)
		m.mu.Lock()
		manifestPath := filepath.Join(pkg.Root, "tau-package.toml")
		manifest, manifestErr := parseManifest(manifestPath)
		if manifestErr == nil && manifestNeedsManagedShim(manifest) && !m.isCLIExtension(pkg.Root) {
			args := splitCommand(manifest.Run.Command)
			var entries []string
			for _, entry := range args[2:] {
				if !filepath.IsAbs(entry) {
					entry = filepath.Join(pkg.Root, entry)
				}
				entries = append(entries, entry)
			}
			pi, err := packages.ReadPiManifest(pkg.Root)
			if err != nil {
				m.mu.Unlock()
				return fmt.Errorf("read Pi extension manifest: %w", err)
			}
			if pi != nil {
				entries, err = packages.PiResourcePaths(pkg.Root, pi.Extensions, packages.ResourceKindExtension)
				if err != nil {
					m.mu.Unlock()
					return fmt.Errorf("discover Pi extensions: %w", err)
				}
				args = append(slices.Clone(args[:2]), entries...)
			}
			selected := artifacts.ApplyPackageFilter(entries, pkg, "extensions")
			m.packageBinaries[pkg.Root] = true
			if len(selected) == 0 {
				delete(m.binaries, pkg.Root)
				delete(m.manifestPaths, pkg.Root)
				m.mu.Unlock()
				continue
			}
			manifest.Run.args = slices.Clone(args[:2])
			allowed := make(map[string]bool, len(selected))
			for _, entry := range selected {
				allowed[entry] = true
			}
			for i, entry := range entries {
				if allowed[entry] {
					manifest.Run.args = append(manifest.Run.args, args[i+2])
				}
			}
			hash, err := computeManagedShimHash(manifestPath, manifest)
			if err != nil {
				m.mu.Unlock()
				return fmt.Errorf("hash selected Pi extensions: %w", err)
			}
			m.packageManifests[manifestPath] = manifest
			m.binaries[pkg.Root] = hash
			m.mu.Unlock()
			continue
		}
		var candidates []string
		for path, manifest := range m.manifestPaths {
			if manifest == filepath.Join(pkg.Root, "tau-package.toml") {
				candidates = append(candidates, path)
			}
		}
		allowed := make(map[string]bool)
		for _, path := range artifacts.ApplyPackageFilter(candidates, pkg, "extensions") {
			allowed[path] = true
		}
		for _, path := range candidates {
			m.packageBinaries[path] = true
			if !allowed[path] && !m.isCLIExtension(path) {
				delete(m.binaries, path)
				delete(m.manifestPaths, path)
			}
		}
		m.mu.Unlock()
	}
	return nil
}

// packageManifest requires m.mu to be held. Runtime selection never rewrites the installed manifest.
func (m *Manager) packageManifest(path string) (*Manifest, error) {
	if manifest := m.packageManifests[path]; manifest != nil {
		return manifest, nil
	}
	return parseManifest(path)
}

func (m *Manager) scanPiPackage(pkg packages.ResolvedPackage) error {
	pi, err := packages.ReadPiManifest(pkg.Root)
	if err != nil {
		return err
	}
	var entries []string
	if pi != nil {
		entries, err = packages.PiResourcePaths(pkg.Root, pi.Extensions, packages.ResourceKindExtension)
		if err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.packageBinaries[pkg.Root] = true
	if !m.isCLIExtension(pkg.Root) {
		entries = artifacts.ApplyPackageFilter(entries, pkg, "extensions")
	}
	if len(entries) == 0 {
		delete(m.binaries, pkg.Root)
		delete(m.manifestPaths, pkg.Root)
		return nil
	}
	args := append([]string{"node", filepath.Join(m.shimDir(), "index.js")}, entries...)
	manifest := &Manifest{Run: &RunSection{Command: strings.Join(args, " "), args: args}}
	manifest.Package.Name = filepath.Base(pkg.Root)
	manifestPath := filepath.Join(pkg.Root, "package.json")
	hash, err := computeManagedShimHash(manifestPath, manifest)
	if err != nil {
		return fmt.Errorf("hash local Pi package: %w", err)
	}
	m.packageManifests[manifestPath] = manifest
	m.binaries[pkg.Root] = hash
	m.manifestPaths[pkg.Root] = manifestPath
	return nil
}
