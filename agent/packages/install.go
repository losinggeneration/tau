package packages

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/gofrs/flock"
)

// IsOfflineMode reports whether Tau or Pi has disabled package network operations.
func IsOfflineMode() bool {
	for _, name := range []string{"TAU_OFFLINE", "PI_OFFLINE"} {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
		case "1", "true", "yes":
			return true
		}
	}
	return false
}

// Config holds configuration for installing packages.
type Config struct {
	expected *InstallSidecar
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
	if err := context.Cause(ctx); err != nil {
		return false, err
	}

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
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		return false, fmt.Errorf("read trust prompt response: %w", errors.Join(err, context.Cause(ctx)))
	}
	if err := context.Cause(ctx); err != nil {
		return false, err
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
	if err := context.Cause(ctx); err != nil {
		return err
	}

	if IsOfflineMode() && (strings.HasPrefix(cfg.Source, "npm:") || isGitURL(cfg.Source)) {
		if cfg.Pi && strings.HasPrefix(cfg.Source, "npm:") {
			return fmt.Errorf("tau install --pi needs network; re-run online")
		}
		return fmt.Errorf("offline mode: remote package installation skipped")
	}

	// --link is only supported for local paths
	if cfg.Link && (isGitURL(cfg.Source) || strings.HasPrefix(cfg.Source, "npm:")) {
		return fmt.Errorf("--link is only supported for local paths, not remote packages")
	}

	if strings.HasPrefix(cfg.Source, "npm:") {
		if err := validatePackageName(parseNpmSpec(strings.TrimPrefix(cfg.Source, "npm:")).Name); err != nil {
			return err
		}
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
	source, err := filepath.Abs(cfg.Source)
	if err != nil {
		return fmt.Errorf("resolve local package source: %w", err)
	}
	cfg.Source = source

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

	manifest, err := loadManifestOrConventionContext(ctx, cfg.Source)
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

	if err := validatePackageParent(installBase, pkgName); err != nil {
		return err
	}
	installPath := filepath.Join(installBase, pkgName)
	if err := validateLocalReplacement(cfg.Source, installPath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(installPath), 0o700); err != nil {
		return fmt.Errorf("create package parent: %w", err)
	}

	err = stagePackageExpected(ctx, installPath, cfg.expected, func(staged string) error {
		if err := validatePackageParent(installBase, pkgName); err != nil {
			return err
		}
		if !cfg.Force {
			if err := NewManager(m.userDir, m.projectDir).CheckDuplicate(pkgName, scope); err != nil {
				return err
			}
		}
		if err := validateLocalReplacement(cfg.Source, installPath); err != nil {
			return err
		}
		if cfg.Link {
			if err := os.Mkdir(staged, 0o700); err != nil {
				return fmt.Errorf("create linked package: %w", err)
			}
			if err := linkSourceEntries(ctx, cfg.Source, staged, ".tau-install.json"); err != nil {
				return err
			}
		} else if err := copyDirContext(ctx, cfg.Source, staged); err != nil {
			return fmt.Errorf("copy package: %w", err)
		}
		sidecar := InstallSidecar{Source: cfg.Source, InstalledAt: info.ModTime().Format("2006-01-02T15:04:05Z07:00"), Scope: string(scope)}
		return sidecar.Write(staged)
	})
	if err != nil {
		return err
	}

	m.notifyReloaders(ctx, pkgName, manifest)

	return nil
}

func validateLocalReplacement(source, destination string) error {
	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat replacement destination: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	resolvedSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("resolve replacement source: %w", err)
	}
	resolvedDestination, err := filepath.EvalSymlinks(destination)
	if err != nil {
		return fmt.Errorf("resolve replacement destination: %w", err)
	}
	resolvedSource, err = filepath.Abs(resolvedSource)
	if err != nil {
		return err
	}
	resolvedDestination, err = filepath.Abs(resolvedDestination)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(resolvedDestination, resolvedSource)
	if err != nil {
		return fmt.Errorf("compare replacement paths: %w", err)
	}
	if filepath.IsLocal(rel) {
		return errors.New("replacement source must not be inside its destination")
	}
	return nil
}

// lockPackageWriter uses a persistent sibling lock file so independent managers
// and processes share ownership even while the destination is renamed.
func lockPackageWriter(ctx context.Context, destination string) (func() error, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return nil, fmt.Errorf("resolve package writer parent: %w", err)
	}
	lockPath := filepath.Join(parent, ".tau-install-lock-"+filepath.Base(destination))
	if info, err := os.Lstat(lockPath); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("package writer lock must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat package writer lock: %w", err)
	}
	lock := flock.New(lockPath)
	acquired, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil || !acquired {
		if err == nil {
			err = errors.New("package writer lock not acquired")
		}
		return nil, errors.Join(fmt.Errorf("acquire package writer: %w", err), context.Cause(ctx), lock.Close())
	}
	if err := context.Cause(ctx); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	if err := recoverPackageReplacement(destination); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return lock.Close, nil
}

func packageRecoveryPath(destination string) string {
	return filepath.Join(filepath.Dir(destination), ".tau-install-recovery-"+filepath.Base(destination))
}

func recoverPackageReplacement(destination string) error {
	recovery := packageRecoveryPath(destination)
	info, err := os.Lstat(recovery)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat package recovery: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("package recovery must be a real directory")
	}
	if _, err := os.Lstat(destination); errors.Is(err, os.ErrNotExist) {
		previous := filepath.Join(recovery, "previous")
		if _, err := os.Lstat(previous); err == nil {
			if err := os.Rename(previous, destination); err != nil {
				return fmt.Errorf("restore interrupted package from %s: %w", previous, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat previous package: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat recovery destination: %w", err)
	}
	if err := os.RemoveAll(recovery); err != nil {
		return fmt.Errorf("remove package recovery: %w", err)
	}
	return nil
}

// stageLocalPackage prepares a sibling before moving the previous installation aside.
func stageLocalPackage(ctx context.Context, destination string, prepare func(string) error) error {
	return stagePackageExpected(ctx, destination, nil, prepare)
}

func stagePackageExpected(ctx context.Context, destination string, expected *InstallSidecar, prepare func(string) error) (err error) {
	unlock, err := lockPackageWriter(ctx, destination)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	if err := checkPackageSnapshot(destination, expected); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(destination), ".tau-install-*")
	if err != nil {
		return fmt.Errorf("stage package: %w", err)
	}
	keepRecovery := false
	defer func() {
		if !keepRecovery {
			err = errors.Join(err, os.RemoveAll(dir))
		}
	}()
	staged := filepath.Join(dir, "package")
	if err := prepare(staged); err != nil {
		return err
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := checkPackageSnapshot(destination, expected); err != nil {
		return err
	}
	recovery := packageRecoveryPath(destination)
	if err := os.Rename(dir, recovery); err != nil {
		return fmt.Errorf("publish package recovery: %w", err)
	}
	dir = recovery
	staged = filepath.Join(dir, "package")
	backup := filepath.Join(dir, "previous")
	previous := false
	if _, err := os.Lstat(destination); err == nil {
		if err := os.Rename(destination, backup); err != nil {
			return fmt.Errorf("save previous package: %w", err)
		}
		previous = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat installed package: %w", err)
	}
	if err := os.Rename(staged, destination); err != nil {
		if previous {
			if restoreErr := os.Rename(backup, destination); restoreErr != nil {
				keepRecovery = true
				return fmt.Errorf("restore package from %s: %w", backup, errors.Join(err, restoreErr))
			}
		}
		return fmt.Errorf("admit package: %w", err)
	}
	return nil
}

// installFromGit installs a package from a git URL with optional ref.
func (m *InstallManager) installFromGit(ctx context.Context, cfg Config) (err error) {
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
	defer func() { err = errors.Join(err, os.RemoveAll(tmpDir)) }()

	// Clone the repository
	cloneOpts := &git.CloneOptions{
		URL:      gitURL,
		Progress: os.Stderr,
	}

	repo, err := git.PlainCloneContext(ctx, tmpDir, false, cloneOpts)
	if err != nil {
		return fmt.Errorf("clone repository: %w", err)
	}

	if ref != "" {
		ref, err = resolveRef(ctx, repo, ref)
	} else {
		ref, err = resolveToLatest(ctx, repo)
	}
	if err != nil {
		return fmt.Errorf("resolve package revision: %w", err)
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		return fmt.Errorf("resolve package commit: %w", err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("open package worktree: %w", err)
	}
	if err := tree.Checkout(&git.CheckoutOptions{Hash: *hash}); err != nil {
		return fmt.Errorf("checkout package commit: %w", err)
	}
	commitSHA := hash.String()

	if cfg.Pi {
		return m.installPiFromGitDir(ctx, cfg, tmpDir, ref, commitSHA, commitTime(repo))
	}

	manifest, err := loadManifestOrConventionContext(ctx, tmpDir)
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

	installBase := m.userDir
	if scope == ScopeProject {
		installBase = m.projectDir
	}
	if err := validatePackageParent(installBase, pkgName); err != nil {
		return err
	}
	installPath := filepath.Join(installBase, pkgName)
	if err := os.MkdirAll(filepath.Dir(installPath), 0o700); err != nil {
		return fmt.Errorf("create package parent: %w", err)
	}
	sidecar := InstallSidecar{Source: cfg.Source, Ref: ref, CommitSHA: commitSHA, InstalledAt: strings.TrimSuffix(commitTime(repo), "\n"), Scope: string(scope)}
	err = stagePackageExpected(ctx, installPath, cfg.expected, func(staged string) error {
		if !cfg.Force {
			if err := NewManager(m.userDir, m.projectDir).CheckDuplicate(pkgName, scope); err != nil {
				return err
			}
		}
		release := findRelease(manifest)
		if release != nil && release.URL != "" && release.SHA256 != "" {
			if err := downloadAndExtract(ctx, release, staged); err != nil {
				return fmt.Errorf("download release: %w", err)
			}
		} else {
			if manifest.Build.Command != "" {
				if err := m.runBuildCommand(ctx, tmpDir, manifest.Build.Command); err != nil {
					return fmt.Errorf("source build failed: %w", err)
				}
			}
			if err := copyDirContext(ctx, tmpDir, staged); err != nil {
				return fmt.Errorf("copy package: %w", err)
			}
		}
		return sidecar.Write(staged)
	})
	if err != nil {
		return err
	}

	m.notifyReloaders(ctx, pkgName, manifest)

	return nil
}

func loadManifestOrConvention(root string) (*Manifest, error) {
	return loadManifestOrConventionContext(context.Background(), root)
}

func loadManifestOrConventionContext(ctx context.Context, root string) (*Manifest, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(root, "tau-package.toml")
	manifest, err := Load(manifestPath)
	if err == nil {
		if err := CheckRuntimeRequirementContext(ctx, manifest.Reqs.Runtime); err != nil {
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
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("stat conventional %s resource: %w", entry.path, statErr)
		}
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
	if idx := strings.LastIndex(source, "@"); idx > 0 && source[idx-1] != '/' {
		if scheme := strings.Index(source, "://"); scheme >= 0 {
			authorityEnd := strings.Index(source[scheme+3:], "/")
			if authorityEnd < 0 || idx < scheme+3+authorityEnd {
				return source, ""
			}
		} else if strings.HasPrefix(source, "git@") && idx == 3 {
			return source, ""
		}
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
	Pinned  bool   // true for an exact semantic version
}

var exactNPMVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

func isExactNPMVersion(version string) bool {
	parts := exactNPMVersion.FindStringSubmatch(strings.TrimSpace(version))
	if parts == nil {
		return false
	}
	for _, part := range parts[1:4] {
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil || n > 9007199254740991 {
			return false
		}
	}
	for _, part := range strings.Split(parts[4], ".") {
		if len(part) < 2 || part[0] != '0' {
			continue
		}
		numeric := true
		for _, char := range part {
			if char < '0' || char > '9' {
				numeric = false
				break
			}
		}
		if numeric {
			return false
		}
	}
	return true
}

func validateNPMVersion(requested NpmSpec, actual string) error {
	if !isExactNPMVersion(actual) {
		return fmt.Errorf("npm archive has an invalid version")
	}
	if requested.Pinned {
		normalize := func(version string) string {
			version = strings.TrimPrefix(strings.TrimSpace(version), "v")
			version, _, _ = strings.Cut(version, "+")
			return version
		}
		if normalize(requested.Version) != normalize(actual) {
			return fmt.Errorf("npm archive version does not match requested version")
		}
	}
	return nil
}

// parseNpmSpec parses an npm package specifier (without the "npm:" prefix).
// Examples:
//   - "lodash" -> NpmSpec{Name: "lodash", Version: "", Pinned: false}
//   - "lodash@4.0.0" -> NpmSpec{Name: "lodash", Version: "4.0.0", Pinned: true}
//   - "@types/node" -> NpmSpec{Name: "@types/node", Version: "", Pinned: false}
//   - "@types/node@18.0.0" -> NpmSpec{Name: "@types/node", Version: "18.0.0", Pinned: true}
func parseNpmSpec(spec string) NpmSpec {
	spec = strings.TrimSpace(spec)
	index := strings.LastIndexByte(spec, '@')
	if index <= 0 {
		return NpmSpec{Name: spec}
	}
	name, version := spec[:index], spec[index+1:]
	return NpmSpec{Name: name, Version: version, Pinned: isExactNPMVersion(version)}
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
func (m *InstallManager) UninstallFromNPM(ctx context.Context, name string, scope Scope) (err error) {
	if err := validatePackageName(name); err != nil {
		return err
	}

	if err := context.Cause(ctx); err != nil {
		return err
	}

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

	// Remove our tracking directory
	if err := validatePackageParent(installBase, name); err != nil {
		return err
	}
	installPath := filepath.Join(installBase, name)
	unlock, err := lockPackageWriter(ctx, installPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	if err := validatePackageParent(installBase, name); err != nil {
		return err
	}
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
	return copyDirContext(context.Background(), src, dst)
}

func copyDirContext(ctx context.Context, src, dst string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	source, err := filepath.EvalSymlinks(src)
	if err != nil {
		return fmt.Errorf("resolve package source: %w", err)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve package source: %w", err)
	}
	destination, err := filepath.Abs(dst)
	if err != nil {
		return fmt.Errorf("resolve package destination: %w", err)
	}
	parent := destination
	for {
		resolved, resolveErr := filepath.EvalSymlinks(parent)
		if resolveErr == nil {
			suffix, relErr := filepath.Rel(parent, destination)
			if relErr != nil {
				return fmt.Errorf("resolve package destination suffix: %w", relErr)
			}
			destination = filepath.Join(resolved, suffix)
			break
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return fmt.Errorf("resolve package destination: %w", resolveErr)
		}
		parent = filepath.Dir(parent)
	}
	rel, err := filepath.Rel(source, destination)
	if err != nil {
		return fmt.Errorf("compare package paths: %w", err)
	}
	if filepath.IsLocal(rel) {
		return errors.New("package destination must not be inside its source")
	}
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return fmt.Errorf("relative package path: %w", err)
		}
		dstPath := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}
		return copyFileContext(ctx, path, dstPath)
	})
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error { return copyFileContext(context.Background(), src, dst) }

func copyFileContext(ctx context.Context, src, dst string) (err error) {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat package file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("package source must be a regular file: %s", src)
	}
	input, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open package file: %w", err)
	}
	defer func() { err = errors.Join(err, input.Close()) }()
	output, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create package file: %w", err)
	}
	defer func() { err = errors.Join(err, output.Close()) }()
	buffer := make([]byte, 32*1024)
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		n, readErr := input.Read(buffer)
		if n > 0 {
			if _, err := output.Write(buffer[:n]); err != nil {
				return fmt.Errorf("copy package file: %w", err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read package file: %w", readErr)
		}
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return output.Chmod(info.Mode().Perm())
}

// installFromNPM installs a package from the npm registry.
func (m *InstallManager) installFromNPM(ctx context.Context, cfg Config) (err error) {
	parsed := parseNpmSpec(strings.TrimSpace(strings.TrimPrefix(cfg.Source, "npm:")))
	scope := ScopeUser
	base := m.userDir
	if cfg.Project {
		scope = ScopeProject
		base = m.projectDir
	}
	if !cfg.Force {
		if err := NewManager(m.userDir, m.projectDir).CheckDuplicate(parsed.Name, scope); err != nil {
			return err
		}
	}
	if err := validatePackageParent(base, parsed.Name); err != nil {
		return err
	}
	destination := filepath.Join(base, parsed.Name)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create package parent: %w", err)
	}
	var manifest *Manifest
	err = stagePackageExpected(ctx, destination, cfg.expected, func(staged string) error {
		if !cfg.Force {
			if err := NewManager(m.userDir, m.projectDir).CheckDuplicate(parsed.Name, scope); err != nil {
				return err
			}
		}
		archiveDir, err := os.MkdirTemp(filepath.Dir(staged), "archive-")
		if err != nil {
			return fmt.Errorf("create npm archive directory: %w", err)
		}
		spec := parsed.Name
		if parsed.Version != "" {
			spec += "@" + parsed.Version
		}
		archive, err := m.npmPackPackage(ctx, spec, archiveDir)
		if err != nil {
			return fmt.Errorf("npm pack: %w", err)
		}
		if err := extractNpmPackageContext(ctx, archive, staged); err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(staged, "package.json"))
		if err != nil {
			return fmt.Errorf("read npm metadata: %w", err)
		}
		var pkg struct {
			Name         string                     `json:"name"`
			Version      string                     `json:"version"`
			Dependencies map[string]json.RawMessage `json:"dependencies"`
		}
		if err := json.Unmarshal(data, &pkg); err != nil {
			return fmt.Errorf("parse npm metadata: %w", err)
		}
		if pkg.Name != parsed.Name || pkg.Version == "" {
			return fmt.Errorf("npm archive identity does not match requested package %q", parsed.Name)
		}
		if err := validateNPMVersion(parsed, pkg.Version); err != nil {
			return err
		}
		if len(pkg.Dependencies) > 0 {
			if err := m.installPiDeps(ctx, staged, nil); err != nil {
				return fmt.Errorf("install npm dependencies: %w", err)
			}
		}
		manifest, err = Load(filepath.Join(staged, "tau-package.toml"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if manifest != nil && manifest.Package.Name != parsed.Name {
			return fmt.Errorf("native manifest identity %q does not match npm package %q", manifest.Package.Name, parsed.Name)
		}
		sidecar := InstallSidecar{Source: cfg.Source, Ref: pkg.Version, InstalledAt: time.Now().Format(time.RFC3339), Scope: string(scope), Pinned: parsed.Pinned}
		return sidecar.Write(staged)
	})
	if err != nil {
		return err
	}
	m.notifyReloaders(ctx, parsed.Name, manifest)
	return nil
}

func checkPackageSnapshot(destination string, expected *InstallSidecar) error {
	if expected == nil {
		return nil
	}
	current, err := LoadSidecar(destination)
	if err != nil {
		return fmt.Errorf("package changed during update: %w", err)
	}
	if *current != *expected {
		return errors.New("package changed during update")
	}
	return nil
}
