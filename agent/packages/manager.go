package packages

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	entries, _ := os.ReadDir(dir)
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
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read packages dir %s: %w", base, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "@") {
			scopedEntries, err := os.ReadDir(filepath.Join(base, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("read scoped packages dir %s: %w", filepath.Join(base, e.Name()), err)
			}
			for _, scoped := range scopedEntries {
				if scoped.IsDir() {
					names = append(names, filepath.Join(e.Name(), scoped.Name()))
				}
			}
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}

// Uninstall removes a package from the specified scope.
// For npm packages, it also removes from the npm registry if user scope.
func (m *Manager) Uninstall(ctx context.Context, name string, scope Scope) error {
	installPath := m.InstallPath(name, scope)

	// Check if package exists
	if _, err := os.Stat(installPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("package %q not found at %s scope", name, scope)
		}
		return fmt.Errorf("stat package: %w", err)
	}

	// Load sidecar to check if this is an npm package
	sidecar, _ := LoadSidecar(installPath)
	if sidecar != nil && strings.HasPrefix(sidecar.Source, "npm:") {
		// This is an npm package - use InstallManager for npm uninstall
		installMgr := NewInstallManager(m.userDir, m.projectDir)
		installMgr.NPMCommand = m.NPMCommand
		return installMgr.UninstallFromNPM(ctx, name, scope)
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
	// Check if offline mode is enabled
	if IsOfflineMode() {
		return nil, fmt.Errorf("offline mode: update skipped")
	}

	installPath := m.mgr.InstallPath(name, scope)

	// Check if package exists
	if _, err := os.Stat(installPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("package %q not found", name)
		}
		return nil, fmt.Errorf("stat package: %w", err)
	}

	// Load sidecar
	sidecar, err := LoadSidecar(installPath)
	if err != nil {
		return nil, fmt.Errorf("load sidecar: %w", err)
	}

	result := &UpdateResult{Name: name}

	// Classify the ref type
	refType := ClassifyRef(sidecar)

	// Check if this is a pinned npm package - pinned packages cannot be updated
	if sidecar.Pinned && strings.HasPrefix(sidecar.Source, "npm:") {
		result.Message = "version-pinned npm package - cannot update"
		return result, nil
	}

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
	newCommit, newRef, err := m.fetchLatest(ctx, sidecar.Source, sidecar.Ref)
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
	if err := m.updatePackage(ctx, installPath, sidecar.Source, newRef, newCommit, scope); err != nil {
		return nil, fmt.Errorf("update package: %w", err)
	}

	result.Updated = true
	result.Message = fmt.Sprintf("updated %s -> %s", result.OldCommit[:7], result.NewCommit[:7])

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

// fetchLatest fetches the latest commit for a given source and ref.
func (m *UpdateManager) fetchLatest(ctx context.Context, source, oldRef string) (commit, ref string, err error) {
	gitURL, _ := parseSourceRef(source)
	gitURL = normalizeGitURL(gitURL)

	tmpDir, err := os.MkdirTemp("", "tau-update-*")
	if err != nil {
		return "", "", fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

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
	gitURL, _ := parseSourceRef(source)
	gitURL = normalizeGitURL(gitURL)

	// Clone to temp directory
	tmpDir, err := os.MkdirTemp("", "tau-update-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

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

	// Resolve the ref and checkout
	resolvedRef, err := resolveRef(ctx, repo, ref)
	if err != nil {
		return fmt.Errorf("resolve ref: %w", err)
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

	// Load manifest to check for build section
	manifestPath := filepath.Join(tmpDir, "tau-package.toml")
	manifest, err := Load(manifestPath)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
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

	// Remove old install
	if err := os.RemoveAll(installPath); err != nil {
		return fmt.Errorf("remove old install: %w", err)
	}

	// Move new content to install path
	if err := os.Rename(tmpDir, installPath); err != nil {
		// Fallback: copy if rename fails
		if err := copyDir(tmpDir, installPath); err != nil {
			return fmt.Errorf("install: %w", err)
		}
		_ = os.RemoveAll(tmpDir)
	}

	// Update sidecar
	sidecar := InstallSidecar{
		Source:      source,
		Ref:         resolvedRef,
		CommitSHA:   commitSHA,
		InstalledAt: commitTimeStr,
		Scope:       string(scope),
	}
	if err := sidecar.Write(installPath); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}

	return nil
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
