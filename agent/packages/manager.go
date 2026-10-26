package packages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// Scope indicates whether a package is installed globally or per-project.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// ErrAlreadyInstalled is returned when a package with the same name is
// already installed at the given scope.
type ErrAlreadyInstalled struct {
	Name  string
	Scope Scope
	Path  string
}

func (e ErrAlreadyInstalled) Error() string {
	return fmt.Sprintf("package %q already installed at %s scope (%s); use `tau uninstall %s` first or pass --force", e.Name, e.Scope, e.Path, e.Name)
}

// Manager handles package installation, uninstallation, and queries.
// It enforces [package].name uniqueness within a given scope.
type Manager struct {
	userDir    string
	projectDir string
	// NPMCommand replaces the npm executable when removing npm packages.
	NPMCommand []string
}

// NewManager returns a Manager that installs packages into userDir and
// projectDir. Pass empty strings to use tau's default paths.
func NewManager(userDir, projectDir string) *Manager {
	return &Manager{
		userDir:    userDir,
		projectDir: projectDir,
	}
}

// InstallPath returns the directory where a package named pkgName would be
// installed at the given scope.
func (m *Manager) InstallPath(pkgName string, scope Scope) string {
	var base string
	switch scope {
	case ScopeProject:
		base = m.projectDir
	case ScopeUser:
		base = m.userDir
	default:
		panic("unknown scope")
	}
	return filepath.Join(base, pkgName)
}

// CheckDuplicate checks whether a package with the given name is already
// installed at the specified scope. Returns ErrAlreadyInstalled if found.
func (m *Manager) CheckDuplicate(pkgName string, scope Scope) error {
	if scope != ScopeUser && scope != ScopeProject {
		return fmt.Errorf("unknown scope: %v", scope)
	}

	if err := validatePackageName(pkgName); err != nil {
		return err
	}

	base := m.userDir
	if scope == ScopeProject {
		base = m.projectDir
	}
	if err := validatePackageParent(base, pkgName); err != nil {
		return err
	}
	dir := m.InstallPath(pkgName, scope)
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat package dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil
	}
	// Verify it looks like a real package install by checking for the sidecar
	// or a manifest. If the dir is empty or non-package, allow install.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read package directory: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	// A real install will have at least tau-package.toml or .tau-install.json.
	hasManifest, hasSidecar := false, false
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if name == "tau-package.toml" {
			hasManifest = true
		}
		if name == ".tau-install.json" {
			hasSidecar = true
		}
	}
	if !hasManifest && !hasSidecar {
		return nil
	}
	return ErrAlreadyInstalled{
		Name:  pkgName,
		Scope: scope,
		Path:  dir,
	}
}

// ListInstalled returns the names of packages installed at the given scope.
func (m *Manager) ListInstalled(scope Scope) ([]string, error) {
	var base string
	switch scope {
	case ScopeProject:
		base = m.projectDir
	case ScopeUser:
		base = m.userDir
	default:
		return nil, fmt.Errorf("unknown scope: %v", scope)
	}
	roots, err := InstalledRoots(base)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, root := range roots {
		name, err := filepath.Rel(base, root)
		if err != nil {
			return nil, fmt.Errorf("resolve installed package name: %w", err)
		}
		names = append(names, name)
	}

	return names, nil
}

// Uninstall removes a package from the specified scope.
// Removal is package-local, including npm archives.
func (m *Manager) Uninstall(ctx context.Context, name string, scope Scope) (err error) {
	if scope != ScopeUser && scope != ScopeProject {
		return fmt.Errorf("unknown scope: %v", scope)
	}

	if err := validatePackageName(name); err != nil {
		return err
	}

	if err := context.Cause(ctx); err != nil {
		return err
	}

	base := m.userDir
	if scope == ScopeProject {
		base = m.projectDir
	}
	if err := validatePackageParent(base, name); err != nil {
		return err
	}
	installPath := m.InstallPath(name, scope)
	unlock, err := lockPackageWriter(ctx, installPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	if err := validatePackageParent(base, name); err != nil {
		return err
	}

	// Check if package exists
	if _, err := os.Stat(installPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("package %q not found at %s scope", name, scope)
		}
		return fmt.Errorf("stat package: %w", err)
	}

	// Reject malformed metadata before removal.
	_, err = LoadSidecar(installPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load package sidecar: %w", err)
	}

	// Regular package removal
	if err := os.RemoveAll(installPath); err != nil {
		return fmt.Errorf("remove package: %w", err)
	}
	return nil
}

// IsNpmPackage checks if a package was installed from npm registry.
func (m *Manager) IsNpmPackage(name string, scope Scope) bool {
	installPath := m.InstallPath(name, scope)
	sidecar, err := LoadSidecar(installPath)
	if err != nil {
		return false
	}
	return strings.HasPrefix(sidecar.Source, "npm:")
}

// UpdateManager handles updating installed packages.
type UpdateManager struct {
	userDir    string
	projectDir string
	mgr        *Manager
	installer  *InstallManager

	// NPMCommand replaces the npm executable with an argv prefix.
	NPMCommand []string

	// ArtifactReloader is called after a successful update to reload artifact registries.
	ArtifactReloader func(context.Context) error
	// PluginReloader is called after a successful update if the plugin binary changed.
	// It receives the plugin name as a parameter.
	PluginReloader func(ctx context.Context, pluginName string) error
}

// NewUpdateManager creates a new update manager.
func NewUpdateManager(userDir, projectDir string) *UpdateManager {
	return &UpdateManager{
		userDir:    userDir,
		projectDir: projectDir,
		mgr:        NewManager(userDir, projectDir),
		installer:  NewInstallManager(userDir, projectDir),
	}
}

// RefType categorizes the type of ref used for installation.
type RefType int

const (
	RefTypeLink     RefType = iota // Linked package (--link), no update possible
	RefTypeTag                     // Tag-pinned (e.g., v1.0.0)
	RefTypeBranch                  // Branch-pinned (e.g., main, develop)
	RefTypeCommit                  // Commit-pinned (full SHA)
	RefTypeUnpinned                // No ref specified, auto-resolved
)

// ClassifyRef determines the type of ref from the sidecar.
func ClassifyRef(sidecar *InstallSidecar) RefType {
	// Non-git sources (e.g., local path installs) have no ref or commit SHA
	if sidecar.CommitSHA == "" && sidecar.Ref == "" {
		return RefTypeLink
	}

	if sidecar.Source != "" && sidecar.CommitSHA != "" {
		if _, requested := parseSourceRef(sidecar.Source); requested == "" {
			return RefTypeUnpinned
		}
	}

	ref := sidecar.Ref
	if ref == "" {
		// Unpinned install: resolved to a tag or branch, ref was empty in source
		// but we have a commit SHA recorded. We need to re-examine to determine
		// if it was a tag or branch. Default to RefTypeUnpinned behavior: re-resolve.
		return RefTypeUnpinned
	}

	// Commit SHA detection (40 hex chars)
	if isCommitHash(ref) {
		return RefTypeCommit
	}

	// Tag detection
	if strings.HasPrefix(ref, "refs/tags/") || strings.HasPrefix(ref, "tag:") {
		return RefTypeTag
	}
	// Also check for bare tag names (semver-like or v-prefixed)
	if !strings.Contains(ref, "/") || strings.HasPrefix(ref, "v") {
		// Could be a tag, check if it looks like one
		if isTagName(ref) {
			return RefTypeTag
		}
	}
	// Branch detection (refs/heads/ or bare branch name)
	if strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "heads/") {
		return RefTypeBranch
	}
	if !strings.Contains(ref, "/") || (strings.Count(ref, "/") == 1 && !strings.Contains(ref, "tags")) {
		return RefTypeBranch
	}

	return RefTypeBranch // Default to branch for any other ref
}

// isCommitHash checks if a string looks like a git commit hash.
func isCommitHash(s string) bool {
	if len(s) != 40 && len(s) != 41 { // 40 hex + optional newline
		return false
	}
	hex := strings.TrimSuffix(s, "\n")
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// isTagName checks if a string looks like a tag name.
func isTagName(s string) bool {
	// Remove v prefix for checking
	s = strings.TrimPrefix(s, "v")
	// Simple semver-ish check
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return false
	}
	for _, p := range parts {
		for _, c := range p {
			if (c < '0' || c > '9') && c != '-' && c != '_' {
				return false
			}
		}
	}
	return true
}

// UpdateResult contains the result of an update attempt.
type UpdateResult struct {
	Name      string
	Updated   bool
	OldCommit string
	NewCommit string
	Message   string
}

// Update updates a package to the latest version based on its ref type.
func (m *UpdateManager) Update(ctx context.Context, name string, scope Scope) (*UpdateResult, error) {
	if scope != ScopeUser && scope != ScopeProject {
		return nil, fmt.Errorf("unknown scope: %v", scope)
	}

	if err := validatePackageName(name); err != nil {
		return nil, err
	}

	if err := context.Cause(ctx); err != nil {
		return nil, err
	}

	// Check if offline mode is enabled
	if IsOfflineMode() {
		return nil, fmt.Errorf("offline mode: update skipped")
	}

	base := m.userDir
	if scope == ScopeProject {
		base = m.projectDir
	}
	if err := validatePackageParent(base, name); err != nil {
		return nil, err
	}
	installPath := m.mgr.InstallPath(name, scope)

	sidecar, err := readPackageForUpdate(ctx, installPath)
	if err != nil {
		return nil, err
	}

	result := &UpdateResult{Name: name}
	if strings.HasPrefix(sidecar.Source, "npm:") {
		requested := parseNpmSpec(strings.TrimPrefix(sidecar.Source, "npm:"))
		if requested.Name != name {
			return nil, fmt.Errorf("npm source identity %q does not match installed package %q", requested.Name, name)
		}
		// Old sidecars can lack a source version while retaining an explicit pin.
		if requested.Pinned || (sidecar.Pinned && requested.Version == "") {
			result.Message = "version-pinned npm package - cannot update"
			return result, nil
		}
		return m.updateNPM(ctx, name, scope, installPath, sidecar)
	}
	refType := ClassifyRef(sidecar)

	switch refType {
	case RefTypeLink:
		result.Message = "linked package - working tree is source of truth"
		return result, nil
	case RefTypeCommit:
		result.Message = "commit-pinned - cannot update"
		return result, nil
	case RefTypeTag:
		result.Message = "tag-pinned - cannot update"
		return result, nil
	case RefTypeBranch, RefTypeUnpinned:
		// Need to re-pull
	default:
		result.Message = "unknown ref type"
		return result, nil
	}

	// For branch-pinned or unpinned, we need to re-pull
	result.OldCommit = sidecar.CommitSHA

	// Re-clone the repo and get the latest commit
	oldRef := sidecar.Ref
	if refType == RefTypeUnpinned {
		oldRef = ""
	}
	newCommit, newRef, err := m.fetchLatest(ctx, sidecar.Source, oldRef)
	if err != nil {
		return nil, fmt.Errorf("fetch latest: %w", err)
	}

	result.NewCommit = newCommit

	// Check if anything changed
	if newCommit == result.OldCommit && sidecar.Ref == newRef {
		result.Message = "already up to date"
		return result, nil
	}

	// Update the package
	if err := m.updatePackageExpected(ctx, installPath, sidecar.Source, newRef, newCommit, scope, sidecar); err != nil {
		return nil, fmt.Errorf("update package: %w", err)
	}

	result.Updated = true
	result.Message = fmt.Sprintf("updated %s -> %s", result.OldCommit[:min(7, len(result.OldCommit))], result.NewCommit[:min(7, len(result.NewCommit))])

	// Reload artifacts after successful update.
	if m.ArtifactReloader != nil {
		if err := m.ArtifactReloader(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warning: artifact reload after update failed: %v\n", err)
		}
	}

	// Check if package has a plugin and reload it if hash changed.
	if m.PluginReloader != nil && newCommit != result.OldCommit {
		manifest, _ := Load(filepath.Join(installPath, "tau-package.toml"))
		if manifest != nil && manifest.HasPlugin() {
			pluginName := manifest.Package.Name
			if err := m.PluginReloader(ctx, pluginName); err != nil {
				fmt.Fprintf(os.Stderr, "warning: plugin reload for %s failed: %v\n", pluginName, err)
			}
		}
	}

	return result, nil
}

func (m *UpdateManager) updateNPM(ctx context.Context, name string, scope Scope, path string, previous *InstallSidecar) (*UpdateResult, error) {
	m.installer.NPMCommand = slices.Clone(m.NPMCommand)
	if err := m.installer.Install(ctx, Config{Source: previous.Source, Force: true, Yes: true, Project: scope == ScopeProject, Pi: previous.Pi, expected: previous}); err != nil {
		return nil, fmt.Errorf("update npm package: %w", err)
	}
	current, err := LoadSidecar(path)
	if err != nil {
		return nil, fmt.Errorf("load updated npm sidecar: %w", err)
	}
	result := &UpdateResult{Name: name, Message: "already up to date"}
	if current.Ref != previous.Ref {
		result.Updated = true
		result.Message = fmt.Sprintf("updated %s -> %s", previous.Ref, current.Ref)
	}
	manifest, err := Load(filepath.Join(path, "tau-package.toml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("load updated manifest: %w", err)
	}
	notifier := InstallManager{ArtifactReloader: m.ArtifactReloader, PluginReloader: m.PluginReloader}
	notifier.notifyReloaders(ctx, name, manifest)
	return result, nil
}

// fetchLatest fetches the latest commit for a given source and ref.
func (m *UpdateManager) fetchLatest(ctx context.Context, source, oldRef string) (commit, ref string, err error) {
	gitURL, _ := parseSourceRef(source)
	gitURL = normalizeGitURL(gitURL)

	tmpDir, err := os.MkdirTemp("", "tau-update-*")
	if err != nil {
		return "", "", fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmpDir)) }()

	cloneOpts := &git.CloneOptions{
		URL:      gitURL,
		Progress: os.Stderr,
	}

	repo, err := git.PlainCloneContext(ctx, tmpDir, false, cloneOpts)
	if err != nil {
		return "", "", fmt.Errorf("clone: %w", err)
	}

	var newRef string
	if oldRef == "" {
		// Unpinned - resolve to latest tag, then fallback to default branch
		newRef, err = resolveToLatest(ctx, repo)
	} else {
		// Branch-pinned - re-resolve the branch
		newRef, err = resolveRef(ctx, repo, oldRef)
	}
	if err != nil {
		return "", "", fmt.Errorf("resolve ref: %w", err)
	}

	// Get commit hash
	hash, err := repo.ResolveRevision(plumbing.Revision(newRef))
	if err != nil {
		return "", "", fmt.Errorf("resolve commit: %w", err)
	}

	return hash.String(), newRef, nil
}

// updatePackage replaces the package directory with fresh content.
func (m *UpdateManager) updatePackage(ctx context.Context, installPath, source, ref, commitSHA string, scope Scope) error {
	return m.updatePackageExpected(ctx, installPath, source, ref, commitSHA, scope, nil)
}

func (m *UpdateManager) updatePackageExpected(ctx context.Context, installPath, source, ref, commitSHA string, scope Scope, expected *InstallSidecar) (err error) {
	base := m.userDir
	if scope == ScopeProject {
		base = m.projectDir
	}
	name, err := filepath.Rel(base, installPath)
	if err != nil {
		return fmt.Errorf("resolve installed package identity: %w", err)
	}
	name = filepath.ToSlash(name)
	if err := validatePackageParent(base, name); err != nil {
		return err
	}
	if !isCommitHash(commitSHA) {
		return fmt.Errorf("invalid update commit identity")
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}

	gitURL, _ := parseSourceRef(source)
	gitURL = normalizeGitURL(gitURL)

	// Clone to temp directory
	tmpDir, err := os.MkdirTemp("", "tau-update-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmpDir)) }()

	cloneOpts := &git.CloneOptions{
		URL:      gitURL,
		Progress: os.Stderr,
	}

	if _, err := git.PlainCloneContext(ctx, tmpDir, false, cloneOpts); err != nil {
		return fmt.Errorf("clone: %w", err)
	}

	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("get worktree: %w", err)
	}

	if err := w.Checkout(&git.CheckoutOptions{
		Hash: plumbing.NewHash(commitSHA),
	}); err != nil {
		return fmt.Errorf("checkout: %w", err)
	}

	previous, err := LoadSidecar(installPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load previous sidecar: %w", err)
	}
	if previous != nil && previous.Pi {
		m.installer.NPMCommand = slices.Clone(m.NPMCommand)
		pkg, err := ParsePiPackage(tmpDir)
		if err != nil {
			return err
		}
		if pkg.Name != name {
			return fmt.Errorf("updated Pi package identity %q does not match %q", pkg.Name, name)
		}
		return m.installer.installPiFromGitDir(ctx, Config{Source: source, Force: true, Yes: true, Project: scope == ScopeProject, Pi: true, expected: expected}, tmpDir, ref, commitSHA, commitTimeFromHash(repo, commitSHA))
	}

	// Load manifest to check for build section
	manifestPath := filepath.Join(tmpDir, "tau-package.toml")
	manifest, err := Load(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		manifest, err = loadManifestOrConventionContext(ctx, tmpDir)
		if err == nil {
			manifest.Package.Name = name
		}
	}
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}

	if manifest.Package.Name != name {
		return fmt.Errorf("updated package identity %q does not match %q", manifest.Package.Name, name)
	}
	if err := CheckRuntimeRequirementContext(ctx, manifest.Reqs.Runtime); err != nil {
		return fmt.Errorf("updated package runtime: %w", err)
	}

	// If there's a build section, run the build
	if manifest.Build.Command != "" {
		installMgr := NewInstallManager(m.userDir, m.projectDir)
		if err := installMgr.runBuildCommand(ctx, tmpDir, manifest.Build.Command); err != nil {
			return fmt.Errorf("build: %w", err)
		}
	}

	// Get commit time for sidecar
	commitTimeStr := commitTimeFromHash(repo, commitSHA)

	// Update sidecar
	sidecar := InstallSidecar{
		Source:      source,
		Ref:         ref,
		CommitSHA:   commitSHA,
		InstalledAt: commitTimeStr,
		Scope:       string(scope),
	}
	return stagePackageExpected(ctx, installPath, expected, func(staged string) error {
		if err := copyDirContext(ctx, tmpDir, staged); err != nil {
			return fmt.Errorf("copy updated package: %w", err)
		}
		return sidecar.Write(staged)
	})
}

// commitTimeFromHash returns the commit time for a given commit hash.
func commitTimeFromHash(repo *git.Repository, hashStr string) string {
	hash := plumbing.NewHash(hashStr)
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return ""
	}
	return commit.Author.When.String()
}

func readPackageForUpdate(ctx context.Context, path string) (sidecar *InstallSidecar, err error) {
	unlock, err := lockPackageWriter(ctx, path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	sidecar, err = LoadSidecar(path)
	if err != nil {
		return nil, fmt.Errorf("load sidecar for update: %w", err)
	}
	return sidecar, nil
}
