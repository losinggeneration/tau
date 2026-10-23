package packages

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// IsOfflineMode returns true if the PI_OFFLINE environment variable is set
// to indicate offline mode operation. When true, network operations like
// fetching package updates should be skipped.
func IsOfflineMode() bool {
	val := os.Getenv("PI_OFFLINE")
	if val == "" {
		return false
	}
	val = strings.ToLower(strings.TrimSpace(val))
	return val == "1" || val == "true" || val == "yes"
}

// Config holds configuration for installing packages.
type Config struct {
	// Source is the git URL, GitHub shorthand, or local path to install from.
	Source string
	// Force skips the already-installed check.
	Force bool
	// Yes skips the trust prompt.
	Yes bool
	// Project installs into project scope instead of user scope.
	Project bool
	// Link creates a symlink instead of copying (for local paths only).
	Link bool
	// Pi installs a pi extension package through pi-ext-shim.
	Pi bool
}

// InstallManager handles installing packages from various sources.
type InstallManager struct {
	userDir    string
	projectDir string

	// ArtifactReloader is called after a successful install to reload artifact registries.
	ArtifactReloader func(context.Context) error
	// PluginReloader is called after a successful install if a plugin was installed.
	// It receives the plugin name as a parameter.
	PluginReloader func(ctx context.Context, pluginName string) error
	// NPMCommand replaces the npm executable with an argv prefix, such as
	// ["mise", "exec", "node@20", "--", "npm"].
	NPMCommand []string

	npmInstall func(context.Context, string, []string) error
	npmPack    func(context.Context, string, string) (string, error)
	ensureShim func(context.Context, string) error
	lookPath   func(string) (string, error)
}

// notifyReloaders runs the post-install reload hooks so a freshly installed
// package becomes visible without a restart.
//
// Every install path ends here. It used to be an inline block copied into each
// one, which meant the only way to cover a path was to drive it end to end —
// four tests each stood up a git repo (two of them an HTTP server and a tarball
// as well) to assert that a callback fired.
//
// Both hooks are best-effort. The package is already on disk by this point, so
// a reload failure costs visibility until the next start; failing the install
// over it would be worse than a warning.
//
// manifest may be nil: the npm path loads it opportunistically and tolerates a
// missing or unreadable file. A nil manifest simply has no plugin to reload.
func (m *InstallManager) notifyReloaders(ctx context.Context, pkgName string, manifest *Manifest) {
	if m.ArtifactReloader != nil {
		if err := m.ArtifactReloader(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warning: artifact reload after install failed: %v\n", err)
		}
	}
	if m.PluginReloader != nil && manifest != nil && manifest.HasPlugin() {
		if err := m.PluginReloader(ctx, pkgName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: plugin reload for %s failed: %v\n", pkgName, err)
		}
	}
}

// NewInstallManager creates a new install manager with the given directories.
func NewInstallManager(userDir, projectDir string) *InstallManager {
	return &InstallManager{
		userDir:    userDir,
		projectDir: projectDir,
	}
}

// PromptTrust displays the trust prompt and returns true if the user accepts.
// If cfg.Yes is true, it skips the prompt and returns true.
func (m *InstallManager) PromptTrust(ctx context.Context, cfg Config, manifest *Manifest, reader io.Reader) (bool, error) {
	if cfg.Yes {
		return true, nil
	}

	// Find the sha256 for the release if available
	var sha256 string
	if release := findRelease(manifest); release != nil && release.SHA256 != "" {
		sha256 = release.SHA256
	}

	skills, prompts, themes, extensions := manifest.ArtifactCounts()

	// Format the trust prompt per spec
	fmt.Fprintf(os.Stderr, "Install %s %s from %s?\n",
		manifest.Package.Name, manifest.Package.Version, cfg.Source)
	if manifest.HasPlugin() {
		fmt.Fprintf(os.Stderr, "  Plugin:    yes  (executes arbitrary code as you)\n")
	} else {
		fmt.Fprintf(os.Stderr, "  Plugin:    no\n")
	}
	if cfg.Pi {
		fmt.Fprintf(os.Stderr, "  pi extension — executes arbitrary JavaScript in a Node subprocess\n")
	}
	fmt.Fprintf(os.Stderr, "  Skills:    %d\n", skills)
	fmt.Fprintf(os.Stderr, "  Prompts:   %d\n", prompts)
	fmt.Fprintf(os.Stderr, "  Themes:    %d\n", themes)
	fmt.Fprintf(os.Stderr, "  Extensions: %d\n", extensions)

	if sha256 != "" {
		fmt.Fprintf(os.Stderr, "\nPlugin release sha256: %s\n", sha256)
	}

	fmt.Fprint(os.Stderr, "[y/N] ")

	// Read answer
	if reader == nil {
		reader = os.Stdin
	}
	scanner := bufio.NewScanner(reader)
	if !scanner.Scan() {
		return false, fmt.Errorf("read trust prompt response")
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))

	return answer == "y" || answer == "yes", nil
}

// InstallFromGit installs a package from a git URL with optional ref.
func (m *InstallManager) InstallFromGit(ctx context.Context, source string, cfg Config) error {
	return m.Install(ctx, Config{
		Source:  source,
		Force:   cfg.Force,
		Yes:     cfg.Yes,
		Project: cfg.Project,
		Pi:      cfg.Pi,
	})
}

// Install installs a package based on the configuration.
// It determines the source type and delegates to the appropriate method.
func (m *InstallManager) Install(ctx context.Context, cfg Config) error {
	// --link is only supported for local paths
	if cfg.Link && isGitURL(cfg.Source) {
		return fmt.Errorf("--link is only supported for local paths, not git URLs")
	}

	// Check for npm: prefix
	if strings.HasPrefix(cfg.Source, "npm:") {
		if cfg.Pi {
			return m.installPiFromNPM(ctx, cfg)
		}
		return m.installFromNPM(ctx, cfg)
	}

	// Detect local path: not a git URL scheme and not a git shorthand
	// Local paths: ./relative, /absolute, relative/path
	// Git URLs: https://..., git@..., github.com/user/repo (bare shorthand)
	if isGitURL(cfg.Source) {
		return m.installFromGit(ctx, cfg)
	}
	return m.installFromLocal(ctx, cfg)
}

// isGitURL returns true if the source looks like a git URL rather than a local path.
func isGitURL(source string) bool {
	// Has scheme
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "file://") {
		return true
	}
	// Git SSH syntax
	if strings.HasPrefix(source, "git@") {
		return true
	}
	// Local path indicators:
	// - starts with ./ or ../
	if strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") {
		return false
	}
	// - starts with ~ (home directory)
	if strings.HasPrefix(source, "~") {
		return false
	}
	// - is an absolute path starting with /
	if strings.HasPrefix(source, "/") {
		return false
	}
	// GitHub shorthand: exactly "owner/repo" or "github.com/owner/repo"
	// Must have exactly 2 parts (owner/repo) or 3 parts (github.com/owner/repo)
	// with no special chars like : or @
	if strings.Contains(source, ":") || strings.HasPrefix(source, "@") {
		return false
	}
	slashCount := strings.Count(source, "/")
	if slashCount == 1 || slashCount == 2 {
		return true
	}
	// If it has slashes but more than 2, it's likely a path
	return false
}

// installFromLocal installs a package from a local directory.
func (m *InstallManager) installFromLocal(ctx context.Context, cfg Config) error {
	// Verify source is a valid directory
	info, err := os.Stat(cfg.Source)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("local path does not exist: %s", cfg.Source)
		}
		return fmt.Errorf("stat local path: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("local path is not a directory: %s", cfg.Source)
	}

	if cfg.Pi {
		return m.installPiFromLocal(ctx, cfg, info)
	}

	manifest, err := loadManifestOrConvention(cfg.Source)
	if err != nil {
		return fmt.Errorf("load package metadata from %s: %w", cfg.Source, err)
	}

	pkgName := manifest.Package.Name

	// Determine install scope
	scope := ScopeUser
	if cfg.Project {
		scope = ScopeProject
	}

	// Determine install base directory
	var installBase string
	switch scope {
	case ScopeProject:
		installBase = m.projectDir
	case ScopeUser:
		installBase = m.userDir
	}

	// Check for duplicate installation
	if !cfg.Force {
		mgr := NewManager(m.userDir, m.projectDir)
		if err := mgr.CheckDuplicate(pkgName, scope); err != nil {
			return err
		}
	}

	// Prompt for trust (declining leaves no partial state on disk)
	ok, err := m.PromptTrust(ctx, cfg, manifest, nil)
	if err != nil {
		return fmt.Errorf("trust prompt: %w", err)
	}
	if !ok {
		return fmt.Errorf("install declined")
	}

	// Create parent directory if needed
	if err := os.MkdirAll(installBase, 0o700); err != nil {
		return fmt.Errorf("create packages dir: %w", err)
	}

	installPath := filepath.Join(installBase, pkgName)

	// Remove existing installation if force
	if cfg.Force {
		if err := os.RemoveAll(installPath); err != nil {
			return fmt.Errorf("remove existing installation: %w", err)
		}
	}

	// Handle --link: create symlink instead of copying
	if cfg.Link {
		// Remove existing symlink if any
		if err := os.Remove(installPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove existing symlink: %w", err)
		}

		// Create symlink pointing to source
		if err := os.Symlink(cfg.Source, installPath); err != nil {
			return fmt.Errorf("create symlink: %w", err)
		}

		// Write sidecar with commitSHA as empty for non-git sources
		sidecar := InstallSidecar{
			Source:      cfg.Source,
			Ref:         "",
			CommitSHA:   "",
			InstalledAt: info.ModTime().Format("2006-01-02T15:04:05Z07:00"),
			Scope:       string(scope),
		}
		if err := sidecar.Write(installPath); err != nil {
			return fmt.Errorf("write install sidecar: %w", err)
		}

		m.notifyReloaders(ctx, pkgName, manifest)

		return nil
	}

	// Copy directory to install path
	if err := copyDir(cfg.Source, installPath); err != nil {
		return fmt.Errorf("copy package: %w", err)
	}

	// Write install sidecar with commitSHA as empty for local paths
	sidecar := InstallSidecar{
		Source:      cfg.Source,
		Ref:         "",
		CommitSHA:   "",
		InstalledAt: info.ModTime().Format("2006-01-02T15:04:05Z07:00"),
		Scope:       string(scope),
	}
	if err := sidecar.Write(installPath); err != nil {
		return fmt.Errorf("write install sidecar: %w", err)
	}

	m.notifyReloaders(ctx, pkgName, manifest)

	return nil
}

// installFromGit installs a package from a git URL with optional ref.
func (m *InstallManager) installFromGit(ctx context.Context, cfg Config) error {
	// Parse source: <git-url>[@<ref>]
	gitURL, ref := parseSourceRef(cfg.Source)

	// Normalize GitHub shorthand URLs
	gitURL = normalizeGitURL(gitURL)

	// Determine install scope
	scope := ScopeUser
	if cfg.Project {
		scope = ScopeProject
	}

	// Create manager for duplicate checking (after manifest load, once we have the real package name)
	tmpDir, err := os.MkdirTemp("", "tau-install-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Clone the repository
	cloneOpts := &git.CloneOptions{
		URL:      gitURL,
		Progress: os.Stderr,
	}

	repo, err := git.PlainCloneContext(ctx, tmpDir, false, cloneOpts)
	if err != nil {
		return fmt.Errorf("clone repository: %w", err)
	}

	// Resolve ref if specified
	var commitSHA string
	if ref != "" {
		resolvedRef, err := resolveRef(ctx, repo, ref)
		if err != nil {
			return fmt.Errorf("resolve ref %q: %w", ref, err)
		}
		ref = resolvedRef

		// Get commit hash for the resolved ref
		hash, err := repo.ResolveRevision(plumbing.Revision(ref))
		if err == nil {
			commitSHA = hash.String()
		}
	} else {
		// Resolve to latest tag or default branch
		resolvedRef, err := resolveToLatest(ctx, repo)
		if err != nil {
			return fmt.Errorf("resolve latest ref: %w", err)
		}
		ref = resolvedRef
		hash, err := repo.ResolveRevision(plumbing.Revision(ref))
		if err == nil {
			commitSHA = hash.String()
		}
	}

	if cfg.Pi {
		return m.installPiFromGitDir(ctx, cfg, tmpDir, ref, commitSHA, commitTime(repo))
	}

	manifest, err := loadManifestOrConvention(tmpDir)
	if err != nil {
		return fmt.Errorf("load package metadata: %w", err)
	}

	pkgName := manifest.Package.Name

	// Check for duplicate installation using the actual package name from the manifest.
	pkgMgr := NewManager(m.userDir, m.projectDir)
	if !cfg.Force {
		if err := pkgMgr.CheckDuplicate(pkgName, scope); err != nil {
			return err
		}
	}

	// Prompt for trust (declining leaves no partial state on disk)
	ok, err := m.PromptTrust(ctx, cfg, manifest, nil)
	if err != nil {
		return fmt.Errorf("trust prompt: %w", err)
	}
	if !ok {
		return fmt.Errorf("install declined")
	}

	// Check for matching release for current platform
	release := findRelease(manifest)
	if release != nil && release.URL != "" && release.SHA256 != "" {
		// Download and extract the release instead of using cloned source
		installPath := filepath.Join(m.userDir, pkgName) // always user scope for releases
		if scope == ScopeProject {
			installPath = filepath.Join(m.projectDir, pkgName)
		}

		// Create parent directory if needed
		if err := os.MkdirAll(filepath.Dir(installPath), 0o700); err != nil {
			return fmt.Errorf("create packages dir: %w", err)
		}

		// Remove existing installation if force
		if cfg.Force {
			if err := os.RemoveAll(installPath); err != nil {
				return fmt.Errorf("remove existing installation: %w", err)
			}
		}

		if err := downloadAndExtract(ctx, release, installPath); err != nil {
			return fmt.Errorf("download release: %w", err)
		}

		// Write install sidecar
		sidecar := InstallSidecar{
			Source:      cfg.Source,
			Ref:         ref,
			CommitSHA:   commitSHA,
			InstalledAt: strings.TrimSuffix(commitTime(repo), "\n"),
			Scope:       string(scope),
		}
		if err := sidecar.Write(installPath); err != nil {
			return fmt.Errorf("write install sidecar: %w", err)
		}

		// Clean up cloned repo (we got the release instead)
		_ = os.RemoveAll(tmpDir)

		m.notifyReloaders(ctx, pkgName, manifest)

		return nil
	}

	// No matching release; check for source-build fallback
	if manifest.Build.Command != "" {
		// Source-build fallback: run the build command
		installPath := filepath.Join(m.userDir, pkgName) // always user scope for source builds
		if scope == ScopeProject {
			installPath = filepath.Join(m.projectDir, pkgName)
		}

		// Create parent directory if needed
		if err := os.MkdirAll(filepath.Dir(installPath), 0o700); err != nil {
			return fmt.Errorf("create packages dir: %w", err)
		}

		// Remove existing installation if force
		if cfg.Force {
			if err := os.RemoveAll(installPath); err != nil {
				return fmt.Errorf("remove existing installation: %w", err)
			}
		}

		// Run the build command in the cloned repo
		if err := m.runBuildCommand(ctx, tmpDir, manifest.Build.Command); err != nil {
			return fmt.Errorf("source build failed: %w", err)
		}

		// Move cloned repo (with built artifacts) to install path
		if err := os.Rename(tmpDir, installPath); err != nil {
			// Fallback: copy if rename fails (cross-filesystem)
			if err := copyDir(tmpDir, installPath); err != nil {
				return fmt.Errorf("install package: %w", err)
			}
			_ = os.RemoveAll(tmpDir)
		}

		// Write install sidecar
		sidecar := InstallSidecar{
			Source:      cfg.Source,
			Ref:         ref,
			CommitSHA:   commitSHA,
			InstalledAt: strings.TrimSuffix(commitTime(repo), "\n"),
			Scope:       string(scope),
		}
		if err := sidecar.Write(installPath); err != nil {
			return fmt.Errorf("write install sidecar: %w", err)
		}

		m.notifyReloaders(ctx, pkgName, manifest)

		return nil
	}

	// No matching release and no build section; use cloned source as-is
	// Determine install path
	var installBase string
	switch scope {
	case ScopeProject:
		installBase = m.projectDir
	case ScopeUser:
		installBase = m.userDir
	}
	installPath := filepath.Join(installBase, pkgName)

	// Create parent directory if needed
	if err := os.MkdirAll(installBase, 0o700); err != nil {
		return fmt.Errorf("create packages dir: %w", err)
	}

	// Remove existing installation if force
	if cfg.Force {
		if err := os.RemoveAll(installPath); err != nil {
			return fmt.Errorf("remove existing installation: %w", err)
		}
	}

	// Move cloned repo to install path
	if err := os.Rename(tmpDir, installPath); err != nil {
		// Fallback: copy if rename fails (cross-filesystem)
		if err := copyDir(tmpDir, installPath); err != nil {
			return fmt.Errorf("install package: %w", err)
		}
		_ = os.RemoveAll(tmpDir)
	}

	// Write install sidecar
	sidecar := InstallSidecar{
		Source:      cfg.Source,
		Ref:         ref,
		CommitSHA:   commitSHA,
		InstalledAt: strings.TrimSuffix(commitTime(repo), "\n"),
		Scope:       string(scope),
	}
	if err := sidecar.Write(installPath); err != nil {
		return fmt.Errorf("write install sidecar: %w", err)
	}

	m.notifyReloaders(ctx, pkgName, manifest)

	return nil
}

func loadManifestOrConvention(root string) (*Manifest, error) {
	manifestPath := filepath.Join(root, "tau-package.toml")
	manifest, err := Load(manifestPath)
	if err == nil {
		if err := CheckRuntimeRequirement(manifest.Reqs.Runtime); err != nil {
			return nil, fmt.Errorf("runtime requirement not satisfied: %w", err)
		}
		return manifest, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	manifest = &Manifest{
		Package: PackageSection{
			Name:    filepath.Base(root),
			Version: "0.0.0",
		},
	}
	for _, entry := range []struct {
		path string
		dst  *[]ArtifactPath
	}{
		{path: "skills", dst: &manifest.Skills},
		{path: "prompts", dst: &manifest.Prompts},
		{path: "themes", dst: &manifest.Themes},
		{path: "extensions", dst: &manifest.Extensions},
	} {
		info, statErr := os.Stat(filepath.Join(root, entry.path))
		if statErr == nil && info.IsDir() {
			*entry.dst = append(*entry.dst, ArtifactPath{Path: entry.path})
		}
	}
	skills, prompts, themes, extensions := manifest.ArtifactCounts()
	if skills == 0 && prompts == 0 && themes == 0 && extensions == 0 {
		return nil, fmt.Errorf("no tau-package.toml and no conventional skills/, prompts/, themes/, or extensions/ directories")
	}
	return manifest, nil
}

// Example: "github.com/user/repo@v1.0.0" -> ("github.com/user/repo", "v1.0.0")
func parseSourceRef(source string) (gitURL, ref string) {
	if idx := strings.LastIndex(source, "@"); idx > 0 {
		return source[:idx], source[idx+1:]
	}
	return source, ""
}

// normalizeGitURL converts GitHub shorthand URLs to full HTTPS URLs.
// "github.com/user/repo" -> "https://github.com/user/repo"
// "user/repo" -> "https://github.com/user/repo"
func normalizeGitURL(src string) string {
	// If it already has a scheme, return as-is
	if strings.Contains(src, "://") {
		return src
	}

	// Handle GitHub shorthand
	if strings.HasPrefix(src, "github.com/") {
		return "https://" + src
	}
	if strings.Count(src, "/") == 1 {
		return "https://github.com/" + src
	}

	// Add https:// for other bare domains
	if !strings.Contains(src, "/") {
		return src
	}

	// Check if it looks like a URL without scheme
	u, err := url.Parse(src)
	if err == nil && u.Host != "" && u.Scheme == "" {
		return "https://" + src
	}

	return src
}

// resolveRef resolves a git ref (tag, branch, or commit) to a full ref.
func resolveRef(ctx context.Context, repo *git.Repository, ref string) (string, error) {
	// Try as-is first
	if _, err := repo.ResolveRevision(plumbing.Revision(ref)); err == nil {
		return ref, nil
	}

	// Try refs/heads/<ref> for branches
	branchRef := plumbing.NewBranchReferenceName(ref)
	if _, err := repo.ResolveRevision(plumbing.Revision(branchRef)); err == nil {
		return branchRef.String(), nil
	}

	// Try refs/tags/<ref> for tags (with and without v prefix)
	tagRef := plumbing.NewTagReferenceName(ref)
	if _, err := repo.ResolveRevision(plumbing.Revision(tagRef)); err == nil {
		return tagRef.String(), nil
	}
	// Try with v prefix
	if !strings.HasPrefix(ref, "v") {
		tagRefV := plumbing.NewTagReferenceName("v" + ref)
		if _, err := repo.ResolveRevision(plumbing.Revision(tagRefV)); err == nil {
			return tagRefV.String(), nil
		}
	}

	return ref, fmt.Errorf("ref %q not found", ref)
}

// resolveToLatest resolves to the latest tag (version-sort) or default branch.
func resolveToLatest(ctx context.Context, repo *git.Repository) (string, error) {
	// First try to find latest tag
	tagRefs, err := repo.Tags()
	if err == nil {
		var tags []string
		// Iterate over tag references
		err = tagRefs.ForEach(func(ref *plumbing.Reference) error {
			tags = append(tags, ref.Name().String())
			return nil
		})
		if err == nil && len(tags) > 0 {
			// Find latest tag by version sort
			latest := latestTag(tags)
			if latest != "" {
				return latest, nil
			}
		}
	}

	// Fall back to default branch
	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("get HEAD: %w", err)
	}
	return head.Name().String(), nil
}

// npmCommand builds an npm invocation, using command as the executable argv
// prefix when it is set.
func npmCommand(ctx context.Context, command []string, args ...string) *exec.Cmd {
	if len(command) == 0 {
		return exec.CommandContext(ctx, "npm", args...)
	}
	return exec.CommandContext(ctx, command[0], append(slices.Clone(command[1:]), args...)...)
}

// NpmSpec represents a parsed npm package specification.
type NpmSpec struct {
	Name    string // package name, e.g. "@foo/bar" or "lodash"
	Version string // optional version, e.g. "1.2.3" or "latest"
	Pinned  bool   // true if version was explicitly specified
}

// parseNpmSpec parses an npm package specifier (without the "npm:" prefix).
// Examples:
//   - "lodash" -> NpmSpec{Name: "lodash", Version: "", Pinned: false}
//   - "lodash@4.0.0" -> NpmSpec{Name: "lodash", Version: "4.0.0", Pinned: true}
//   - "@types/node" -> NpmSpec{Name: "@types/node", Version: "", Pinned: false}
//   - "@types/node@18.0.0" -> NpmSpec{Name: "@types/node", Version: "18.0.0", Pinned: true}
func parseNpmSpec(spec string) NpmSpec {
	spec = strings.TrimSpace(spec)

	// Check if this is a scoped package (starts with @)
	if strings.HasPrefix(spec, "@") {
		// Scoped package - find the second @ for version separator
		// e.g., "@types/node@18.0.0"
		// First @ is part of the scope, second @ separates name from version
		slashIndex := strings.IndexByte(spec, '/')
		if slashIndex != -1 && slashIndex < len(spec)-1 {
			// Find @ after the slash
			afterSlash := spec[slashIndex+1:]
			atIndex := strings.IndexByte(afterSlash, '@')
			if atIndex != -1 {
				// @types/node@18.0.0 -> name="@types/node", version="18.0.0"
				name := spec[:slashIndex+1+atIndex]
				version := afterSlash[atIndex+1:]
				return NpmSpec{
					Name:    name,
					Version: version,
					Pinned:  true,
				}
			}
		}
		// Scoped package without version: @types/node
		return NpmSpec{Name: spec, Version: "", Pinned: false}
	}

	// Non-scoped package - simple split on @
	atIndex := strings.IndexByte(spec, '@')
	if atIndex == -1 {
		return NpmSpec{Name: spec, Version: "", Pinned: false}
	}

	name := spec[:atIndex]
	version := spec[atIndex+1:]
	return NpmSpec{
		Name:    name,
		Version: version,
		Pinned:  true,
	}
}

// latestTag returns the latest tag from a list of tag refs using simple version sort.
func latestTag(tags []string) string {
	var latest string
	var latestVer [3]int
	for _, t := range tags {
		// Strip refs/tags/ prefix
		name := strings.TrimPrefix(t, "refs/tags/")
		// Simple semver extraction: v1.2.3 -> [1, 2, 3]
		var v [3]int
		s := strings.TrimPrefix(name, "v")
		parts := strings.Split(s, ".")
		for i := range min(len(parts), 3) {
			_, _ = fmt.Sscanf(parts[i], "%d", &v[i])
		}
		if v[0] > latestVer[0] || (v[0] == latestVer[0] && v[1] > latestVer[1]) ||
			(v[0] == latestVer[0] && v[1] == latestVer[1] && v[2] > latestVer[2]) {
			latest = t
			latestVer = v
		}
	}
	return latest
}

// commitTime returns the commit time of HEAD as a string.
func commitTime(repo *git.Repository) string {
	head, err := repo.Head()
	if err != nil {
		return ""
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return ""
	}
	return commit.Author.When.String()
}

// UninstallFromNPM uninstalls an npm package by name.
func (m *InstallManager) UninstallFromNPM(ctx context.Context, name string, scope Scope) error {
	// Determine the install base directory
	var installBase string
	switch scope {
	case ScopeProject:
		installBase = m.projectDir
	case ScopeUser:
		installBase = m.userDir
	default:
		return fmt.Errorf("unknown scope: %v", scope)
	}

	// For user scope, npm packages are installed globally, so we need to uninstall
	// from the npm registry AND remove our tracking directory
	if scope == ScopeUser {
		// Run npm uninstall -g to remove from global node_modules
		cmd := npmCommand(ctx, m.NPMCommand, "uninstall", name, "-g")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			// Don't fail on npm uninstall errors - we still want to clean up our tracking
			fmt.Fprintf(os.Stderr, "warning: npm uninstall -g %s failed: %v\n", name, err)
		}
	}

	// Remove our tracking directory
	installPath := filepath.Join(installBase, name)
	if err := os.RemoveAll(installPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove package directory: %w", err)
	}

	return nil
}

// runBuildCommand executes the build command in the specified directory.
func (m *InstallManager) runBuildCommand(ctx context.Context, dir, command string) error {
	// Use bash -c to run the command (allows shell features like &&, ||, etc.)
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr // Stream build output to stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build command %q failed: %w", command, err)
	}

	return nil
}

// copyDir copies a directory from src to dst.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		dstPath := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}
		return copyFile(path, dstPath)
	})
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

// installFromNPM installs a package from the npm registry.
func (m *InstallManager) installFromNPM(ctx context.Context, cfg Config) error {
	// Parse the npm spec (without "npm:" prefix)
	spec := strings.TrimPrefix(cfg.Source, "npm:")
	spec = strings.TrimSpace(spec)
	parsed := parseNpmSpec(spec)

	// Determine install scope
	scope := ScopeUser
	if cfg.Project {
		scope = ScopeProject
	}

	// Check for duplicate installation
	if !cfg.Force {
		mgr := NewManager(m.userDir, m.projectDir)
		if err := mgr.CheckDuplicate(parsed.Name, scope); err != nil {
			return err
		}
	}

	// Determine install path
	var installBase string
	switch scope {
	case ScopeProject:
		installBase = m.projectDir
	case ScopeUser:
		installBase = m.userDir
	}

	// Create parent directory if needed
	if err := os.MkdirAll(installBase, 0o700); err != nil {
		return fmt.Errorf("create packages dir: %w", err)
	}

	// Calculate install path for the package
	installPath := filepath.Join(installBase, parsed.Name)

	// Remove existing installation if force
	if cfg.Force {
		if err := os.RemoveAll(installPath); err != nil {
			return fmt.Errorf("remove existing installation: %w", err)
		}
	}

	// Install npm package
	installArgs := []string{"install"}
	if parsed.Version != "" {
		installArgs = append(installArgs, parsed.Name+"@"+parsed.Version)
	} else {
		installArgs = append(installArgs, parsed.Name)
	}

	if scope == ScopeUser {
		// Global install
		installArgs = append(installArgs, "-g")
	} else {
		// Project-scoped install - use prefix
		installArgs = append(installArgs, "--prefix", installBase)
	}

	cmd := npmCommand(ctx, m.NPMCommand, installArgs...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm install failed: %w", err)
	}

	// Get the actual installed version for the sidecar
	version := parsed.Version
	if version == "" {
		version = getInstalledNpmVersion(installPath)
	}

	// Write install sidecar
	sidecar := InstallSidecar{
		Source:      cfg.Source,
		Ref:         version, // Use version as "ref" for npm packages
		CommitSHA:   "",
		InstalledAt: strings.TrimPrefix(installedAt(), "\n"),
		Scope:       string(scope),
		Pinned:      parsed.Pinned,
	}
	if err := sidecar.Write(installPath); err != nil {
		return fmt.Errorf("write install sidecar: %w", err)
	}

	// The npm path has no manifest in hand; load it opportunistically so a
	// package that ships a plugin still triggers a plugin reload.
	manifest, _ := Load(filepath.Join(installPath, "tau-package.toml"))
	m.notifyReloaders(ctx, parsed.Name, manifest)

	return nil
}

// getInstalledNpmVersion returns the version from an installed npm package's package.json.
func getInstalledNpmVersion(installPath string) string {
	pkgJSONPath := filepath.Join(installPath, "package.json")
	data, err := os.ReadFile(pkgJSONPath)
	if err != nil {
		return ""
	}
	// Simple JSON parsing to extract version
	var version string
	re := regexp.MustCompile(`"version"\s*:\s*"([^"]+)"`)
	matches := re.FindSubmatch(data)
	if matches != nil {
		version = string(matches[1])
	}
	return version
}

// installedAt returns the current timestamp as an ISO 8601 string.
func installedAt() string {
	return strings.TrimPrefix(os.Getenv("SOURCE_DATE_EPOCH"), "")
}
