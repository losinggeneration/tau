package packages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager_CheckDuplicate(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewManager(filepath.Join(tmpDir, "user"), filepath.Join(tmpDir, "project"))

	// No packages installed — should be clean.
	err := m.CheckDuplicate("foo", ScopeUser)
	require.NoError(t, err)

	// Create a user-scope install of "foo".
	fooDir := m.InstallPath("foo", ScopeUser)
	err = os.MkdirAll(fooDir, 0o755)
	require.NoError(t, err)
	// Write a manifest to make it look like a real install.
	err = os.WriteFile(filepath.Join(fooDir, "tau-package.toml"), []byte(`
[package]
name = "foo"
version = "1.0.0"
[plugin]
api_version = "1"
`), 0o644)
	require.NoError(t, err)

	// Duplicate should be caught.
	err = m.CheckDuplicate("foo", ScopeUser)
	assert.Error(t, err)

	// Project scope is clean.
	err = m.CheckDuplicate("foo", ScopeProject)
	require.NoError(t, err)

	// Project install of "bar" doesn't collide with user "foo".
	err = m.CheckDuplicate("foo", ScopeUser)
	assert.Error(t, err)
}

func TestManager_InstallPath(t *testing.T) {
	m := NewManager("/home/user/.tau/agent/packages", "/home/user/project/.tau/packages")
	assert.Equal(t, "/home/user/.tau/agent/packages/foo", m.InstallPath("foo", ScopeUser))
	assert.Equal(t, "/home/user/project/.tau/packages/bar", m.InstallPath("bar", ScopeProject))
}

func TestManager_ListInstalled(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewManager(tmpDir, "")

	names, err := m.ListInstalled(ScopeUser)
	require.NoError(t, err)
	assert.Empty(t, names)

	// Add a few installs.
	for _, name := range []string{"alpha", "beta", "gamma", filepath.Join("@scope", "pkg")} {
		dir := m.InstallPath(name, ScopeUser)
		err = os.MkdirAll(dir, 0o755)
		require.NoError(t, err)
	}

	names, err = m.ListInstalled(ScopeUser)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"alpha", "beta", "gamma", "@scope/pkg"}, names)
}

func TestIsGitURL(t *testing.T) {
	tests := []struct {
		source   string
		expected bool
	}{
		// Git URLs
		{"https://github.com/user/repo", true},
		{"http://gitlab.com/user/repo", true},
		{"git@github.com:user/repo.git", true},
		{"user/repo", true}, // GitHub shorthand
		{"github.com/user/repo", true},
		{"git@gitlab.com:group/repo.git", true},
		// Local paths
		{"./my-pkg", false},
		{"../my-pkg", false},
		{"/absolute/path/to/pkg", false},
		{"relative/path/to/pkg", false},
		{"~/my-local-pkg", false},
		{"./foo/bar", false},
		{"/home/user/projects/tau-pkg", false},
	}

	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			result := isGitURL(tt.source)
			assert.Equal(t, tt.expected, result, "isGitURL(%q) = %v, want %v", tt.source, result, tt.expected)
		})
	}
}

func TestInstallManager_InstallFromLocal(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewInstallManager(filepath.Join(tmpDir, "user"), filepath.Join(tmpDir, "project"))

	// Create a source package directory
	srcDir := filepath.Join(tmpDir, "source-pkg")
	err := os.MkdirAll(srcDir, 0o755)
	require.NoError(t, err)

	// Write manifest
	manifestContent := `
[package]
name = "test-pkg"
version = "1.0.0"
description = "Test package"
[plugin]
api_version = "1"
`
	err = os.WriteFile(filepath.Join(srcDir, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	// Create a skills directory
	err = os.MkdirAll(filepath.Join(srcDir, "skills"), 0o755)
	require.NoError(t, err)

	// Install from local path
	cfg := Config{
		Source:  srcDir,
		Project: false,
		Yes:     true,
	}
	err = mgr.Install(context.Background(), cfg)
	require.NoError(t, err)

	// Verify installation
	installPath := filepath.Join(tmpDir, "user", "test-pkg")
	_, err = os.Stat(installPath)
	require.NoError(t, err)

	// Verify manifest was copied
	manifestBytes, err := os.ReadFile(filepath.Join(installPath, "tau-package.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(manifestBytes), "test-pkg")

	// Verify sidecar was written
	sidecar, err := LoadSidecar(installPath)
	require.NoError(t, err)
	assert.Equal(t, srcDir, sidecar.Source)
	assert.Empty(t, sidecar.CommitSHA)
	assert.Equal(t, "user", sidecar.Scope)

	// Verify skills directory was copied
	_, err = os.Stat(filepath.Join(installPath, "skills"))
	require.NoError(t, err)
}

func TestInstallManager_InstallFromLocal_NonExistentPath(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewInstallManager(filepath.Join(tmpDir, "user"), "")

	cfg := Config{
		Source: filepath.Join(tmpDir, "non-existent"),
	}
	err := mgr.Install(context.Background(), cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

func TestInstallManager_InstallFromLocal_NoManifest(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewInstallManager(filepath.Join(tmpDir, "user"), "")

	// Create a source dir without manifest
	srcDir := filepath.Join(tmpDir, "no-manifest")
	err := os.MkdirAll(srcDir, 0o755)
	require.NoError(t, err)

	cfg := Config{Source: srcDir}
	err = mgr.Install(context.Background(), cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no tau-package.toml")
}

func TestInstallManager_InstallFromLocal_ConventionFallback(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewInstallManager(filepath.Join(tmpDir, "user"), "")

	srcDir := filepath.Join(tmpDir, "skill-pack")
	err := os.MkdirAll(filepath.Join(srcDir, "skills"), 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(srcDir, "skills", "helper.md"), []byte("---\nname: helper\n---\nbody"), 0o644)
	require.NoError(t, err)

	cfg := Config{Source: srcDir, Yes: true}
	err = mgr.Install(context.Background(), cfg)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(tmpDir, "user", "skill-pack", "skills", "helper.md"))
	require.NoError(t, err)
}

func TestInstallManager_InstallFromLocal_Link(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewInstallManager(filepath.Join(tmpDir, "user"), filepath.Join(tmpDir, "project"))

	// Create a source package directory
	srcDir := filepath.Join(tmpDir, "source-pkg")
	err := os.MkdirAll(srcDir, 0o755)
	require.NoError(t, err)

	// Write manifest
	manifestContent := `
[package]
name = "test-pkg"
version = "1.0.0"
description = "Test package"
[plugin]
api_version = "1"
`
	err = os.WriteFile(filepath.Join(srcDir, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	// Create a skills directory to verify symlink follows
	err = os.MkdirAll(filepath.Join(srcDir, "skills"), 0o755)
	require.NoError(t, err)

	// Install from local path with --link
	cfg := Config{
		Source:  srcDir,
		Project: false,
		Yes:     true,
		Link:    true,
	}
	err = mgr.Install(context.Background(), cfg)
	require.NoError(t, err)

	// Verify installation is a symlink
	installPath := filepath.Join(tmpDir, "user", "test-pkg")
	link, err := os.Readlink(installPath)
	require.NoError(t, err)
	assert.Equal(t, srcDir, link)

	// Verify sidecar has null commit SHA for linked install
	sidecar, err := LoadSidecar(installPath)
	require.NoError(t, err)
	assert.Empty(t, sidecar.CommitSHA)
}

// TestInstallManager_InstallFromGit_ArtifactReload tests that artifact reload hooks
// fire after a git install. This is a black-box test that uses go-git to create
// a temporary bare repository, pushes a commit with a tau-package.toml, and verifies
// the ArtifactReloader callback is invoked.
func TestInstallManager_InstallFromGit_ArtifactReload(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a bare git repository to serve as the "remote"
	bareDir := filepath.Join(tmpDir, "repo.git")
	_, err := git.PlainInit(bareDir, true)
	require.NoError(t, err)

	// Create a working directory to prepare commits
	workDir := filepath.Join(tmpDir, "work")
	err = os.MkdirAll(workDir, 0o755)
	require.NoError(t, err)

	// Init a git repo in the work directory
	repo, err := git.PlainInit(workDir, false)
	require.NoError(t, err)

	// Create tau-package.toml and skills at the root of the work directory
	manifest := `
[package]
name = "git-artifact-test"
version = "1.0.0"
[[skills]]
path = "skills/"
`
	err = os.WriteFile(filepath.Join(workDir, "tau-package.toml"), []byte(manifest), 0o644)
	require.NoError(t, err)

	// Create skills directory at the root
	err = os.MkdirAll(filepath.Join(workDir, "skills"), 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(workDir, "skills", "test-skill.md"), []byte("---\nname: test\n---\ntest"), 0o644)
	require.NoError(t, err)

	// Stage and commit
	w, err := repo.Worktree()
	require.NoError(t, err)

	_, err = w.Add(".")
	require.NoError(t, err)

	_, err = w.Commit("add package", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err)

	// Push to the bare repo
	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{bareDir},
	})
	require.NoError(t, err)

	err = repo.Push(&git.PushOptions{RemoteName: "origin"})
	require.NoError(t, err)

	// Now install from the bare repo using file:// URL
	userDir := filepath.Join(tmpDir, "packages")
	installMgr := NewInstallManager(userDir, "")

	var artifactReloaded bool
	installMgr.ArtifactReloader = func(ctx context.Context) error {
		artifactReloaded = true
		return nil
	}

	cfg := Config{
		Source: "file://" + bareDir,
		Yes:    true,
	}
	err = installMgr.Install(context.Background(), cfg)
	require.NoError(t, err, "git install should succeed: %v", err)

	// Verify ArtifactReloader was called
	assert.True(t, artifactReloaded, "ArtifactReloader should be called after git install")

	// Verify package was installed
	pkgPath := filepath.Join(userDir, "git-artifact-test")
	_, err = os.Stat(filepath.Join(pkgPath, "tau-package.toml"))
	require.NoError(t, err)
}

// TestInstallManager_InstallFromGit_PluginReload tests that plugin reload hooks
// fire after a git install of a package that has a plugin.
func TestInstallManager_InstallFromGit_PluginReload(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a bare git repository
	bareDir := filepath.Join(tmpDir, "plugin-repo.git")
	_, err := git.PlainInit(bareDir, true)
	require.NoError(t, err)

	// Create a working directory
	workDir := filepath.Join(tmpDir, "work-plugin")
	err = os.MkdirAll(workDir, 0o755)
	require.NoError(t, err)

	repo, err := git.PlainInit(workDir, false)
	require.NoError(t, err)

	// Create tau-package.toml with a plugin at the root
	manifest := `
[package]
name = "git-plugin-test"
version = "1.0.0"
description = "Test plugin package"
[plugin]
api_version = "1"
`
	err = os.WriteFile(filepath.Join(workDir, "tau-package.toml"), []byte(manifest), 0o644)
	require.NoError(t, err)

	w, err := repo.Worktree()
	require.NoError(t, err)

	_, err = w.Add(".")
	require.NoError(t, err)

	_, err = w.Commit("add plugin package", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err)

	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{bareDir},
	})
	require.NoError(t, err)

	err = repo.Push(&git.PushOptions{RemoteName: "origin"})
	require.NoError(t, err)

	// Install from the bare repo
	userDir := filepath.Join(tmpDir, "packages")
	installMgr := NewInstallManager(userDir, "")

	var (
		pluginReloaded     bool
		reloadedPluginName string
	)
	installMgr.PluginReloader = func(ctx context.Context, pluginName string) error {
		pluginReloaded = true
		reloadedPluginName = pluginName
		return nil
	}

	cfg := Config{
		Source: "file://" + bareDir,
		Yes:    true,
	}
	err = installMgr.Install(context.Background(), cfg)
	require.NoError(t, err, "git install should succeed: %v", err)

	// Verify PluginReloader was called with the correct package name
	assert.True(t, pluginReloaded, "PluginReloader should be called after git install of plugin package")
	assert.Equal(t, "git-plugin-test", reloadedPluginName, "PluginReloader should receive the package name")

	// Verify package was installed
	pkgPath := filepath.Join(userDir, "git-plugin-test")
	_, err = os.Stat(filepath.Join(pkgPath, "tau-package.toml"))
	require.NoError(t, err)
}

func TestInstallManager_Install_LinkNotAllowedForGit(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewInstallManager(tmpDir, "")

	// Git URL with --link should error
	cfg := Config{
		Source: "https://github.com/user/repo",
		Link:   true,
	}
	err := mgr.Install(context.Background(), cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "--link is only supported for local paths")
}

func TestClassifyRef(t *testing.T) {
	tests := []struct {
		name    string
		sidecar InstallSidecar
		want    RefType
	}{
		{
			name:    "linked package",
			sidecar: InstallSidecar{Source: "/local/path", Ref: "", CommitSHA: ""},
			want:    RefTypeLink,
		},
		{
			name:    "commit-pinned",
			sidecar: InstallSidecar{Source: "https://github.com/user/repo@abc123def456abc123def456abc123def456abcd", Ref: "abc123def456abc123def456abc123def456abcd", CommitSHA: "abc123def456abc123def456abc123def456abcd"},
			want:    RefTypeCommit,
		},
		{
			name:    "tag-pinned",
			sidecar: InstallSidecar{Source: "https://github.com/user/repo@v1.2.3", Ref: "v1.2.3", CommitSHA: "abc123"},
			want:    RefTypeTag,
		},
		{
			name:    "branch-pinned",
			sidecar: InstallSidecar{Source: "https://github.com/user/repo@main", Ref: "main", CommitSHA: "abc123"},
			want:    RefTypeBranch,
		},
		{
			name:    "unpinned (empty ref)",
			sidecar: InstallSidecar{Source: "https://github.com/user/repo", Ref: "", CommitSHA: "abc123"},
			want:    RefTypeUnpinned,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRef(&tt.sidecar)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoadManifest(t *testing.T) {
	tmpDir := t.TempDir()

	// Valid manifest
	manifestPath := filepath.Join(tmpDir, "tau-package.toml")
	validManifest := `
[package]
name = "test-pkg"
version = "1.0.0"
description = "A test package"
[plugin]
api_version = "1"
`
	err := os.WriteFile(manifestPath, []byte(validManifest), 0o644)
	require.NoError(t, err)

	m, err := LoadManifest(tmpDir)
	require.NoError(t, err)
	assert.Equal(t, "test-pkg", m.Package.Name)
	assert.Equal(t, "1.0.0", m.Package.Version)
	assert.Equal(t, "A test package", m.Package.Description)
	assert.True(t, m.HasPlugin())

	// Missing manifest
	missingDir := filepath.Join(tmpDir, "missing")
	err = os.MkdirAll(missingDir, 0o755)
	require.NoError(t, err)

	_, err = LoadManifest(missingDir)
	assert.Error(t, err)
}

func TestLoadManifestOrConvention(t *testing.T) {
	tmpDir := t.TempDir()

	// Test 1: explicit manifest takes precedence
	pkgDir := filepath.Join(tmpDir, "explicit")
	err := os.MkdirAll(pkgDir, 0o755)
	require.NoError(t, err)

	manifestContent := `
[package]
name = "explicit-pkg"
version = "2.0.0"
[plugin]
api_version = "1"
`
	err = os.WriteFile(filepath.Join(pkgDir, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	m, err := loadManifestOrConvention(pkgDir)
	require.NoError(t, err)
	assert.Equal(t, "explicit-pkg", m.Package.Name)

	// Test 2: convention fallback - skills dir creates artifact-only manifest
	skillDir := filepath.Join(tmpDir, "skill-only")
	err = os.MkdirAll(filepath.Join(skillDir, "skills"), 0o755)
	require.NoError(t, err)

	m, err = loadManifestOrConvention(skillDir)
	require.NoError(t, err)
	assert.Equal(t, "skill-only", m.Package.Name)
	assert.Equal(t, "0.0.0", m.Package.Version)
	assert.False(t, m.HasPlugin())
	assert.NotEmpty(t, m.AllSkills())

	// Test 3: convention fallback - prompts dir
	promptDir := filepath.Join(tmpDir, "prompt-only")
	err = os.MkdirAll(filepath.Join(promptDir, "prompts"), 0o755)
	require.NoError(t, err)

	m, err = loadManifestOrConvention(promptDir)
	require.NoError(t, err)
	assert.NotEmpty(t, m.AllPrompts())

	// Test 4: neither manifest nor convention - should error
	emptyDir := filepath.Join(tmpDir, "empty")
	err = os.MkdirAll(emptyDir, 0o755)
	require.NoError(t, err)

	_, err = loadManifestOrConvention(emptyDir)
	assert.Error(t, err)
}

func TestManifest_Validate(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name: "valid",
			yaml: `
[package]
name = "test"
version = "1.0.0"
[plugin]
api_version = "1"
`,
			wantErr: false,
		},
		{
			name: "missing name",
			yaml: `
[package]
version = "1.0.0"
`,
			wantErr: true,
		},
		{
			name: "missing version",
			yaml: `
[package]
name = "test"
`,
			wantErr: true,
		},
		{
			name: "neither plugin nor artifacts",
			yaml: `
[package]
name = "test"
version = "1.0.0"
`,
			wantErr: true,
		},
		{
			name: "with skills is valid",
			yaml: `
[package]
name = "test"
version = "1.0.0"
[[skills]]
path = "skills/"
`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(tt.yaml))
			// We only care about validation errors, not parsing errors
			// (parsing should succeed for all these cases)
			if err == nil {
				err = m.Validate()
			}
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestInstallSidecar_WriteLoad(t *testing.T) {
	tmpDir := t.TempDir()

	sidecar := InstallSidecar{
		Source:      "https://github.com/user/repo",
		Ref:         "v1.0.0",
		CommitSHA:   "abc123def456",
		InstalledAt: "2026-05-01T00:00:00Z",
		Scope:       string(ScopeUser),
	}

	err := sidecar.Write(tmpDir)
	require.NoError(t, err)

	loaded, err := LoadSidecar(tmpDir)
	require.NoError(t, err)
	assert.Equal(t, sidecar.Source, loaded.Source)
	assert.Equal(t, sidecar.Ref, loaded.Ref)
	assert.Equal(t, sidecar.CommitSHA, loaded.CommitSHA)
	assert.Equal(t, sidecar.InstalledAt, loaded.InstalledAt)
	assert.Equal(t, sidecar.Scope, loaded.Scope)
}

func TestUpdateManager_Update_PinBehavior(t *testing.T) {
	tmpDir := t.TempDir()
	userDir := filepath.Join(tmpDir, "packages")

	// Simulate a tag-pinned install
	pkgDir := filepath.Join(userDir, "pin-test")
	err := os.MkdirAll(pkgDir, 0o755)
	require.NoError(t, err)

	manifest := `
[package]
name = "pin-test"
version = "1.0.0"
[plugin]
api_version = "1"
`
	err = os.WriteFile(filepath.Join(pkgDir, "tau-package.toml"), []byte(manifest), 0o644)
	require.NoError(t, err)

	sidecar := InstallSidecar{
		Source:    "https://github.com/user/repo@v1.0.0",
		Ref:       "v1.0.0", // tag-pinned
		CommitSHA: "abc123",
		Scope:     string(ScopeUser),
	}
	err = sidecar.Write(pkgDir)
	require.NoError(t, err)

	// UpdateManager should detect tag-pinned and skip
	updateMgr := NewUpdateManager(userDir, "")
	updated, err := updateMgr.Update(context.Background(), "pin-test", ScopeUser)
	require.NoError(t, err)
	assert.Contains(t, updated.Message, "tag-pinned")
}

func TestUpdateManager_Update_Unpinned(t *testing.T) {
	// For unpinned installs (Ref is empty), UpdateManager should attempt refresh.
	// We can't test actual network refresh, but we can verify the sidecar loading
	// and that the ref type is correctly classified.
	tmpDir := t.TempDir()
	userDir := filepath.Join(tmpDir, "packages")

	pkgDir := filepath.Join(userDir, "unpinned-test")
	err := os.MkdirAll(pkgDir, 0o755)
	require.NoError(t, err)

	manifest := `
[package]
name = "unpinned-test"
version = "1.0.0"
`
	err = os.WriteFile(filepath.Join(pkgDir, "tau-package.toml"), []byte(manifest), 0o644)
	require.NoError(t, err)

	sidecar := InstallSidecar{
		Source:    "https://github.com/user/repo",
		Ref:       "", // unpinned
		CommitSHA: "abc123",
		Scope:     string(ScopeUser),
	}
	err = sidecar.Write(pkgDir)
	require.NoError(t, err)

	refType := ClassifyRef(&sidecar)
	assert.Equal(t, RefTypeUnpinned, refType, "empty ref should be unpinned")
}

func TestSourceBuild(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewInstallManager(filepath.Join(tmpDir, "packages"), "")

	// Create a package with a build section
	srcDir := filepath.Join(tmpDir, "build-pkg")
	err := os.MkdirAll(srcDir, 0o755)
	require.NoError(t, err)

	manifestContent := `
[package]
name = "build-test"
version = "1.0.0"
[plugin]
api_version = "1"
[build]
command = "echo built"
`
	err = os.WriteFile(filepath.Join(srcDir, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	cfg := Config{Source: srcDir, Yes: true}
	err = mgr.Install(context.Background(), cfg)
	require.NoError(t, err)

	// Verify installation
	installPath := filepath.Join(tmpDir, "packages", "build-test")
	_, err = os.Stat(installPath)
	require.NoError(t, err)
}

func TestConflictDetection(t *testing.T) {
	tmpDir := t.TempDir()
	userDir := filepath.Join(tmpDir, "packages")

	mgr := NewManager(userDir, "")

	// Install "foo"
	fooDir := mgr.InstallPath("foo", ScopeUser)
	err := os.MkdirAll(fooDir, 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(fooDir, "tau-package.toml"), []byte(`
[package]
name = "foo"
version = "1.0.0"
[plugin]
api_version = "1"
`), 0o644)
	require.NoError(t, err)

	// Try to install "foo" again - should error
	err = mgr.CheckDuplicate("foo", ScopeUser)
	assert.Error(t, err)

	// Project scope should be clean
	err = mgr.CheckDuplicate("foo", ScopeProject)
	require.NoError(t, err)
}

func TestInstallFromGit_SameBasenameDifferentNames(t *testing.T) {
	// Regression test: two repos with the same basename (e.g., both "repo")
	// but different [package].name values in their manifests should both
	// install successfully without basename-based false-positive collision.
	tmpDir := t.TempDir()

	// Create two fake git repos with different package names but same basename.
	// Simulate by pre-populating the install manager's userDir with an already-
	// installed package under a different manifest name, then verify that a
	// second install with a different manifest name succeeds.

	// Repo 1: github.com/foo/repo with package name "foo-helper"
	repo1Dir := filepath.Join(tmpDir, "repo1")
	err := os.MkdirAll(repo1Dir, 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(repo1Dir, "tau-package.toml"), []byte(`
[package]
name = "foo-helper"
version = "1.0.0"
[plugin]
api_version = "1"
`), 0o644)
	require.NoError(t, err)
	// Make it look like a git repo (go-git needs at least a .git dir for some ops)
	os.MkdirAll(filepath.Join(repo1Dir, ".git"), 0o755)

	// Repo 2: github.com/bar/repo with package name "bar-helper"
	repo2Dir := filepath.Join(tmpDir, "repo2")
	err = os.MkdirAll(repo2Dir, 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(repo2Dir, "tau-package.toml"), []byte(`
[package]
name = "bar-helper"
version = "2.0.0"
[[skills]]
path = "skills/"
`), 0o644)
	require.NoError(t, err)
	os.MkdirAll(filepath.Join(repo2Dir, ".git"), 0o755)

	// Pre-install repo1 (simulate it being already there).
	userDir := filepath.Join(tmpDir, "packages")
	fooPath := filepath.Join(userDir, "foo-helper")
	err = os.MkdirAll(fooPath, 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(fooPath, "tau-package.toml"), []byte(`
[package]
name = "foo-helper"
version = "1.0.0"
`), 0o644)
	require.NoError(t, err)

	// Now install repo2 — it has the same basename "repo" but a different
	// [package].name. It should succeed, not be blocked by the "repo" collision.
	mgr := NewInstallManager(userDir, "")
	cfg := Config{
		Source: repo2Dir,
		Yes:    true,
	}
	err = mgr.Install(context.Background(), cfg)
	require.NoError(t, err, "install should succeed despite same URL basename; identity is [package].name")

	// Verify bar-helper was installed.
	barPath := filepath.Join(userDir, "bar-helper")
	_, err = os.Stat(filepath.Join(barPath, "tau-package.toml"))
	require.NoError(t, err)

	// Verify foo-helper is still there (not overwritten).
	_, err = os.Stat(filepath.Join(fooPath, "tau-package.toml"))
	require.NoError(t, err)
}

func TestClassifyRef_UnpinnedRefOnly(t *testing.T) {
	// Unpinned install: ref is empty (resolved to latest tag/branch), but commit SHA is recorded.
	// This should be classified as RefTypeUnpinned so update can re-resolve.
	sidecar := InstallSidecar{
		Source:    "https://github.com/user/repo",
		Ref:       "",
		CommitSHA: "abc123def456abc123def456abc123def456abcd",
	}
	got := ClassifyRef(&sidecar)
	assert.Equal(t, RefTypeUnpinned, got)
}

func TestUnpinnedInstallSidecarPreservesUpdateMetadata(t *testing.T) {
	// Verify that unpinned installs record enough metadata in the sidecar
	// to support update behavior: source, ref (resolved), and commit SHA.
	tmpDir := t.TempDir()
	pkgDir := filepath.Join(tmpDir, "test-pkg")
	err := os.MkdirAll(pkgDir, 0o755)
	require.NoError(t, err)

	// Simulate an unpinned install (ref was empty when installed, resolved to latest tag)
	sidecar := InstallSidecar{
		Source:      "github.com/user/test-pkg",
		Ref:         "", // unpinned - was resolved at install time
		CommitSHA:   "abc123def456abc123def456abc123def456abcd",
		InstalledAt: "2026-05-01T00:00:00Z",
		Scope:       string(ScopeUser),
	}

	// Write sidecar
	err = sidecar.Write(pkgDir)
	require.NoError(t, err)

	// Load sidecar and verify it has all the metadata needed for update
	loaded, err := LoadSidecar(pkgDir)
	require.NoError(t, err)
	assert.Equal(t, "github.com/user/test-pkg", loaded.Source)
	assert.Equal(t, "abc123def456abc123def456abc123def456abcd", loaded.CommitSHA)

	// Verify ClassifyRef returns RefTypeUnpinned for unpinned installs
	// This ensures update behavior will re-resolve to latest tag
	refType := ClassifyRef(loaded)
	assert.Equal(t, RefTypeUnpinned, refType)
}

func TestUninstallRejectsCancelledAdmission(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root, t.TempDir())
	path := filepath.Join(root, "retained")
	require.NoError(t, os.MkdirAll(path, 0o700))
	sentinel := filepath.Join(path, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("uninstall withdrawn")
	cancel(cause)
	require.ErrorIs(t, manager.Uninstall(ctx, "retained", ScopeUser), cause)
	require.FileExists(t, sentinel)
}

func TestUpdateRejectsCancelledPinnedPackage(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	root := t.TempDir()
	path := filepath.Join(root, "retained")
	require.NoError(t, os.MkdirAll(path, 0o700))
	require.NoError(t, (InstallSidecar{Source: "npm:retained", Pinned: true}).Write(path))
	manager := NewUpdateManager(root, t.TempDir())
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("update withdrawn")
	cancel(cause)
	result, err := manager.Update(ctx, "retained", ScopeUser)
	require.ErrorIs(t, err, cause)
	require.Nil(t, result)
}

func TestUninstallRejectsEscapingName(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "packages")
	require.NoError(t, os.MkdirAll(base, 0o700))
	outside := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o700))
	sentinel := filepath.Join(outside, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	manager := NewManager(base, t.TempDir())
	require.Error(t, manager.Uninstall(t.Context(), "../outside", ScopeUser))
	require.FileExists(t, sentinel)
}

func TestListInstalledExcludesPendingAndRecoveryPackages(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"visible", ".tau-install-pending", "@scope/visible", "@scope/.tau-install-recovery"} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, name), 0o700))
	}
	manager := NewManager(base, t.TempDir())
	names, err := manager.ListInstalled(ScopeUser)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"visible", filepath.Join("@scope", "visible")}, names)
}

func TestUninstallRejectsScopedSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(base, "@scope")))
	destination := filepath.Join(outside, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	sentinel := filepath.Join(destination, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	manager := NewManager(base, t.TempDir())
	require.Error(t, manager.Uninstall(t.Context(), "@scope/plugin", ScopeUser))
	require.FileExists(t, sentinel)
}

func TestUpdateRejectsChangedPackageIdentity(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	source := t.TempDir()
	repo, err := git.PlainInit(source, false)
	require.NoError(t, err)
	tree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = \"unexpected\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
	_, err = tree.Add(".")
	require.NoError(t, err)
	hash, err := tree.Commit("different package", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
	require.NoError(t, err)
	base := t.TempDir()
	destination := filepath.Join(base, "original")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	sentinel := filepath.Join(destination, "old")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	manager := NewUpdateManager(base, t.TempDir())
	require.ErrorContains(t, manager.updatePackage(t.Context(), destination, "file://"+source, "HEAD", hash.String(), ScopeUser), "identity")
	require.FileExists(t, sentinel)
}

func TestUpdateUsesSelectedCommitAfterBranchMoves(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	root := t.TempDir()
	for i, name := range []string{"plugin", "@scope/plugin"} {
		t.Run(name, func(t *testing.T) {
			source := filepath.Join(root, fmt.Sprint(i))
			require.NoError(t, os.MkdirAll(source, 0o700))
			repo, err := git.PlainInit(source, false)
			require.NoError(t, err)
			tree, err := repo.Worktree()
			require.NoError(t, err)
			manifest := fmt.Sprintf("[package]\nname = %q\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n", name)
			require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte(manifest), 0o600))
			commit := func(content string) plumbing.Hash {
				require.NoError(t, os.WriteFile(filepath.Join(source, "content"), []byte(content), 0o600))
				_, err := tree.Add(".")
				require.NoError(t, err)
				hash, err := tree.Commit(content, &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
				require.NoError(t, err)
				return hash
			}
			selected := commit("selected")
			head, err := repo.Head()
			require.NoError(t, err)
			ref := head.Name().String()
			commit("later branch head")
			base := t.TempDir()
			destination := filepath.Join(base, filepath.FromSlash(name))
			require.NoError(t, os.MkdirAll(destination, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(destination, "old"), []byte("old"), 0o600))
			manager := NewUpdateManager(base, t.TempDir())
			require.NoError(t, manager.updatePackage(t.Context(), destination, "file://"+source, ref, selected.String(), ScopeUser))
			content, err := os.ReadFile(filepath.Join(destination, "content"))
			require.NoError(t, err)
			require.Equal(t, "selected", string(content))
			require.NoFileExists(t, filepath.Join(destination, "old"))
			sidecar, err := LoadSidecar(destination)
			require.NoError(t, err)
			require.Equal(t, selected.String(), sidecar.CommitSHA)
			require.Equal(t, ref, sidecar.Ref)
			require.Equal(t, string(ScopeUser), sidecar.Scope)
		})
	}
}

func TestUpdateReportsSuccessWithShortPreviousCommit(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	source := t.TempDir()
	repo, err := git.PlainInit(source, false)
	require.NoError(t, err)
	tree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = \"plugin\"\nversion = \"1\"\n[plugin]\napi_version = \"1\"\n"), 0o600))
	_, err = tree.Add(".")
	require.NoError(t, err)
	hash, err := tree.Commit("new", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
	require.NoError(t, err)
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	require.NoError(t, (InstallSidecar{Source: "file://" + source, Ref: "HEAD", CommitSHA: "old", Scope: string(ScopeUser)}).Write(destination))
	manager := NewUpdateManager(base, t.TempDir())
	reloads := 0
	manager.ArtifactReloader = func(context.Context) error { reloads++; return nil }
	result, err := manager.Update(t.Context(), "plugin", ScopeUser)
	require.NoError(t, err)
	require.True(t, result.Updated)
	require.Equal(t, "old", result.OldCommit)
	require.Equal(t, hash.String(), result.NewCommit)
	require.Contains(t, result.Message, "old -> "+hash.String()[:7])
	require.Equal(t, 1, reloads)
}

func TestPackageMutationsRejectUnknownScope(t *testing.T) {
	manager := NewManager(t.TempDir(), t.TempDir())
	updater := NewUpdateManager(manager.userDir, manager.projectDir)
	scope := Scope("invalid")
	require.Error(t, manager.CheckDuplicate("plugin", scope))
	require.Error(t, manager.Uninstall(t.Context(), "plugin", scope))
	_, err := updater.Update(t.Context(), "plugin", scope)
	require.Error(t, err)
}

func TestListInstalledIncludesLinkedPackages(t *testing.T) {
	base := t.TempDir()
	linked := t.TempDir()
	require.NoError(t, os.Symlink(linked, filepath.Join(base, "linked")))
	require.NoError(t, os.MkdirAll(filepath.Join(base, "@scope"), 0o700))
	require.NoError(t, os.Symlink(linked, filepath.Join(base, "@scope", "linked")))
	manager := NewManager(base, t.TempDir())
	names, err := manager.ListInstalled(ScopeUser)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"linked", filepath.Join("@scope", "linked")}, names)
}

func TestClassifyGitRefUsesRequestedPinRatherThanResolvedTag(t *testing.T) {
	for _, test := range []struct {
		source, ref string
		want        RefType
	}{
		{"https://example.invalid/repo", "refs/tags/v1.0.0", RefTypeUnpinned},
		{"https://example.invalid/repo@v1.0.0", "refs/tags/v1.0.0", RefTypeTag},
		{"https://example.invalid/repo@main", "refs/heads/main", RefTypeBranch},
		{"https://example.invalid/repo", "refs/heads/main", RefTypeUnpinned},
	} {
		t.Run(test.source+test.ref, func(t *testing.T) {
			require.Equal(t, test.want, ClassifyRef(&InstallSidecar{Source: test.source, Ref: test.ref, CommitSHA: strings.Repeat("a", 40)}))
		})
	}
}

func TestUnpinnedGitUpdateAdvancesLatestTag(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	source := t.TempDir()
	repo, err := git.PlainInit(source, false)
	require.NoError(t, err)
	tree, err := repo.Worktree()
	require.NoError(t, err)
	commit := func(version string) plumbing.Hash {
		manifest := fmt.Sprintf("[package]\nname = \"plugin\"\nversion = %q\n[plugin]\napi_version = \"1\"\n", version)
		require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte(manifest), 0o600))
		_, err := tree.Add(".")
		require.NoError(t, err)
		hash, err := tree.Commit(version, &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
		require.NoError(t, err)
		_, err = repo.CreateTag("v"+version, hash, nil)
		require.NoError(t, err)
		return hash
	}
	first := commit("1.0.0")
	base := t.TempDir()
	installer := NewInstallManager(base, t.TempDir())
	require.NoError(t, installer.Install(t.Context(), Config{Source: "file://" + source, Yes: true}))
	second := commit("2.0.0")
	updater := NewUpdateManager(base, t.TempDir())
	result, err := updater.Update(t.Context(), "plugin", ScopeUser)
	require.NoError(t, err)
	require.True(t, result.Updated)
	require.Equal(t, first.String(), result.OldCommit)
	require.Equal(t, second.String(), result.NewCommit)
	installed, err := Load(filepath.Join(base, "plugin", "tau-package.toml"))
	require.NoError(t, err)
	require.Equal(t, "2.0.0", installed.Package.Version)
	sidecar, err := LoadSidecar(filepath.Join(base, "plugin"))
	require.NoError(t, err)
	require.Equal(t, "refs/tags/v2.0.0", sidecar.Ref)
	require.Equal(t, RefTypeUnpinned, ClassifyRef(sidecar))
	result, err = updater.Update(t.Context(), "plugin", ScopeUser)
	require.NoError(t, err)
	require.False(t, result.Updated)
	require.Equal(t, "already up to date", result.Message)
}

func TestUninstallRetainsPackageWhenSidecarIsUnreadableOrMalformed(t *testing.T) {
	for _, kind := range []string{"malformed", "directory", "loop"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, "plugin")
			require.NoError(t, os.MkdirAll(destination, 0o700))
			sentinel := filepath.Join(destination, "keep")
			require.NoError(t, os.WriteFile(sentinel, []byte("original"), 0o600))
			switch kind {
			case "malformed":
				require.NoError(t, os.WriteFile(sidecarPath(destination), []byte("{"), 0o600))
			case "directory":
				require.NoError(t, os.MkdirAll(sidecarPath(destination), 0o700))
			case "loop":
				require.NoError(t, os.Symlink(sidecarPath(destination), sidecarPath(destination)))
			}
			manager := NewManager(base, t.TempDir())
			require.Error(t, manager.Uninstall(t.Context(), "plugin", ScopeUser))
			require.FileExists(t, sentinel)
		})
	}
}

func TestUnpinnedNPMUpdateUsesPackageInstaller(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "")
	t.Setenv("PI_OFFLINE", "")
	for _, scope := range []Scope{ScopeUser, ScopeProject} {
		t.Run(string(scope), func(t *testing.T) {
			user, project := t.TempDir(), t.TempDir()
			manager := NewUpdateManager(user, project)
			destination := manager.mgr.InstallPath("plugin", scope)
			require.NoError(t, os.MkdirAll(destination, 0o700))
			require.NoError(t, (InstallSidecar{Source: "npm:plugin", Ref: "1.0.0", Scope: string(scope)}).Write(destination))
			packs := 0
			manager.installer.npmPack = func(ctx context.Context, spec, dir string) (string, error) {
				packs++
				require.Equal(t, "plugin", spec)
				return writePiTarball(t, dir, `{"name":"plugin","version":"2.0.0"}`), nil
			}
			reloads := 0
			manager.ArtifactReloader = func(context.Context) error {
				reloads++
				require.FileExists(t, filepath.Join(destination, "package.json"))
				return nil
			}
			result, err := manager.Update(t.Context(), "plugin", scope)
			require.NoError(t, err)
			require.True(t, result.Updated)
			require.Equal(t, "updated 1.0.0 -> 2.0.0", result.Message)
			require.Equal(t, 1, packs)
			require.Equal(t, 1, reloads)
			sidecar, err := LoadSidecar(destination)
			require.NoError(t, err)
			require.Equal(t, "2.0.0", sidecar.Ref)
			require.False(t, sidecar.Pinned)
			result, err = manager.Update(t.Context(), "plugin", scope)
			require.NoError(t, err)
			require.False(t, result.Updated)
			require.Equal(t, "already up to date", result.Message)
			require.Equal(t, 2, packs)
			require.Equal(t, 2, reloads)
		})
	}
}

func TestNPMUpdateFailureRetainsInstallation(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "")
	t.Setenv("PI_OFFLINE", "")
	for _, phase := range []string{"pack", "identity", "dependencies", "cancel"} {
		t.Run(phase, func(t *testing.T) {
			manager := NewUpdateManager(t.TempDir(), t.TempDir())
			destination := manager.mgr.InstallPath("plugin", ScopeUser)
			require.NoError(t, os.MkdirAll(destination, 0o700))
			before := InstallSidecar{Source: "npm:plugin", Ref: "1.0.0"}
			require.NoError(t, before.Write(destination))
			require.NoError(t, os.WriteFile(filepath.Join(destination, "keep"), []byte("original"), 0o600))
			cause := errors.New("npm update rejected")
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			manager.installer.npmPack = func(ctx context.Context, spec, dir string) (string, error) {
				if phase == "pack" {
					return "", cause
				}
				metadata := `{"name":"plugin","version":"2.0.0"}`
				if phase == "identity" {
					metadata = `{"name":"other","version":"2.0.0"}`
				}
				if phase == "dependencies" {
					metadata = `{"name":"plugin","version":"2.0.0","dependencies":{"dep":"1"}}`
				}
				archive := writePiTarball(t, dir, metadata)
				if phase == "cancel" {
					cancel(cause)
				}
				return archive, nil
			}
			manager.installer.npmInstall = func(context.Context, string, []string) error { return cause }
			manager.ArtifactReloader = func(context.Context) error { t.Error("reloaded rejected update"); return nil }
			result, err := manager.Update(ctx, "plugin", ScopeUser)
			require.Error(t, err)
			require.Nil(t, result)
			if phase != "identity" {
				require.ErrorIs(t, err, cause)
			}
			require.FileExists(t, filepath.Join(destination, "keep"))
			after, err := LoadSidecar(destination)
			require.NoError(t, err)
			require.Equal(t, before, *after)
			entries, err := packageEntriesWithoutLocks(manager.userDir)
			require.NoError(t, err)
			require.Len(t, entries, 1)
		})
	}
}

func TestPinnedNPMUpdateNeverPacks(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "")
	t.Setenv("PI_OFFLINE", "")
	for _, previous := range []InstallSidecar{
		{Source: "npm:plugin@1.0.0", Ref: "1.0.0", Pinned: true},
		{Source: "npm:plugin@1.0.0", Ref: "1.0.0"},
	} {
		t.Run(fmt.Sprint(previous.Pinned), func(t *testing.T) {
			manager := NewUpdateManager(t.TempDir(), t.TempDir())
			destination := manager.mgr.InstallPath("plugin", ScopeUser)
			require.NoError(t, os.MkdirAll(destination, 0o700))
			require.NoError(t, previous.Write(destination))
			calls := 0
			manager.installer.npmPack = func(context.Context, string, string) (string, error) { calls++; return "", errors.New("must not pack") }
			result, err := manager.Update(t.Context(), "plugin", ScopeUser)
			require.NoError(t, err)
			require.False(t, result.Updated)
			require.Zero(t, calls)
		})
	}
}

func TestPiNPMUpdateRetainsPiMetadata(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "")
	t.Setenv("PI_OFFLINE", "")
	manager := NewUpdateManager(t.TempDir(), t.TempDir())
	destination := manager.mgr.InstallPath("plugin", ScopeProject)
	require.NoError(t, os.MkdirAll(destination, 0o700))
	require.NoError(t, (InstallSidecar{Source: "npm:plugin", Ref: "1.0.0", Pi: true}).Write(destination))
	manager.installer.npmPack = func(ctx context.Context, spec, dir string) (string, error) {
		return writePiTarball(t, dir, `{"name":"plugin","version":"2.0.0","pi":{"extensions":["ext.js"]}}`), nil
	}
	manager.installer.ensureShim = func(context.Context, string) error { return nil }
	manager.installer.lookPath = func(string) (string, error) { return "/node", nil }
	artifactCalls, pluginCalls := 0, 0
	manager.ArtifactReloader = func(context.Context) error { artifactCalls++; return nil }
	manager.PluginReloader = func(ctx context.Context, name string) error {
		pluginCalls++
		require.Equal(t, "plugin", name)
		return nil
	}
	result, err := manager.Update(t.Context(), "plugin", ScopeProject)
	require.NoError(t, err)
	require.True(t, result.Updated)
	require.Equal(t, 1, artifactCalls)
	require.Equal(t, 1, pluginCalls)
	after, err := LoadSidecar(destination)
	require.NoError(t, err)
	require.True(t, after.Pi)
	require.Equal(t, "2.0.0", after.Ref)
	require.Equal(t, string(ScopeProject), after.Scope)
	require.False(t, after.Pinned)
}

func TestNPMUpdateRejectsChangedSourceIdentity(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "")
	t.Setenv("PI_OFFLINE", "")
	manager := NewUpdateManager(t.TempDir(), t.TempDir())
	destination := manager.mgr.InstallPath("plugin", ScopeUser)
	require.NoError(t, os.MkdirAll(destination, 0o700))
	require.NoError(t, (InstallSidecar{Source: "npm:other", Ref: "1.0.0"}).Write(destination))
	packs := 0
	manager.installer.npmPack = func(ctx context.Context, spec, dir string) (string, error) {
		packs++
		return writePiTarball(t, dir, `{"name":"other","version":"2.0.0"}`), nil
	}
	_, err := manager.Update(t.Context(), "plugin", ScopeUser)
	require.ErrorContains(t, err, "identity")
	require.Zero(t, packs)
	require.NoDirExists(t, manager.mgr.InstallPath("other", ScopeUser))
}

func TestPiGitUpdateUsesSelectedRevisionAndRetainsFormat(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "")
	t.Setenv("PI_OFFLINE", "")
	source := t.TempDir()
	repo, err := git.PlainInit(source, false)
	require.NoError(t, err)
	tree, err := repo.Worktree()
	require.NoError(t, err)
	commit := func(version string) plumbing.Hash {
		require.NoError(t, os.WriteFile(filepath.Join(source, "package.json"), fmt.Appendf(nil, `{"name":"plugin","version":%q,"pi":{"prompts":["prompt.md"]}}`, version), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(source, "prompt.md"), []byte(version), 0o600))
		_, err := tree.Add(".")
		require.NoError(t, err)
		hash, err := tree.Commit(version, &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
		require.NoError(t, err)
		_, err = repo.CreateTag("v"+version, hash, nil)
		require.NoError(t, err)
		return hash
	}
	first := commit("1.0.0")
	base := t.TempDir()
	installer := NewInstallManager(base, t.TempDir())
	require.NoError(t, installer.Install(t.Context(), Config{Source: "file://" + source, Yes: true, Pi: true}))
	second := commit("2.0.0")
	manager := NewUpdateManager(base, t.TempDir())
	manager.installer.ensureShim = func(context.Context, string) error {
		t.Error("resource-only package must not prepare shim")
		return nil
	}
	result, err := manager.Update(t.Context(), "plugin", ScopeUser)
	require.NoError(t, err)
	require.True(t, result.Updated)
	require.Equal(t, first.String(), result.OldCommit)
	require.Equal(t, second.String(), result.NewCommit)
	after, err := LoadSidecar(filepath.Join(base, "plugin"))
	require.NoError(t, err)
	require.True(t, after.Pi)
	require.Equal(t, second.String(), after.CommitSHA)
	content, err := os.ReadFile(filepath.Join(base, "plugin", "prompt.md"))
	require.NoError(t, err)
	require.Equal(t, "2.0.0", string(content))
	manifest, err := Load(filepath.Join(base, "plugin", "tau-package.toml"))
	require.NoError(t, err)
	require.Equal(t, "2.0.0", manifest.Package.Version)
	result, err = manager.Update(t.Context(), "plugin", ScopeUser)
	require.NoError(t, err)
	require.False(t, result.Updated)
}

func TestPiGitUpdateIdentityFailureRetainsPreviousInstall(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "")
	t.Setenv("PI_OFFLINE", "")
	source := t.TempDir()
	repo, err := git.PlainInit(source, false)
	require.NoError(t, err)
	tree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "package.json"), []byte(`{"name":"other","version":"2.0.0","pi":{"prompts":["prompt.md"]}}`), 0o600))
	_, err = tree.Add(".")
	require.NoError(t, err)
	hash, err := tree.Commit("changed identity", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
	require.NoError(t, err)
	manager := NewUpdateManager(t.TempDir(), t.TempDir())
	destination := manager.mgr.InstallPath("plugin", ScopeUser)
	require.NoError(t, os.MkdirAll(destination, 0o700))
	previous := InstallSidecar{Source: "file://" + source, Ref: "HEAD", CommitSHA: "old", Pi: true}
	require.NoError(t, previous.Write(destination))
	require.NoError(t, os.WriteFile(filepath.Join(destination, "keep"), []byte("original"), 0o600))
	err = manager.updatePackage(t.Context(), destination, previous.Source, "HEAD", hash.String(), ScopeUser)
	require.ErrorContains(t, err, "identity")
	require.FileExists(t, filepath.Join(destination, "keep"))
	after, err := LoadSidecar(destination)
	require.NoError(t, err)
	require.Equal(t, previous, *after)
	require.NoDirExists(t, manager.mgr.InstallPath("other", ScopeUser))
}

func TestConventionGitUpdateRetainsResourcePackage(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "")
	t.Setenv("PI_OFFLINE", "")
	source := filepath.Join(t.TempDir(), "plugin")
	require.NoError(t, os.MkdirAll(filepath.Join(source, "prompts"), 0o700))
	repo, err := git.PlainInit(source, false)
	require.NoError(t, err)
	tree, err := repo.Worktree()
	require.NoError(t, err)
	commit := func(content string) plumbing.Hash {
		require.NoError(t, os.WriteFile(filepath.Join(source, "prompts", "prompt.md"), []byte(content), 0o600))
		_, err := tree.Add(".")
		require.NoError(t, err)
		hash, err := tree.Commit(content, &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
		require.NoError(t, err)
		return hash
	}
	first := commit("first")
	manager := NewUpdateManager(t.TempDir(), t.TempDir())
	destination := manager.mgr.InstallPath("plugin", ScopeUser)
	require.NoError(t, os.MkdirAll(destination, 0o700))
	previous := InstallSidecar{Source: "file://" + source, Ref: "HEAD", CommitSHA: first.String()}
	require.NoError(t, previous.Write(destination))
	second := commit("second")
	result, err := manager.Update(t.Context(), "plugin", ScopeUser)
	require.NoError(t, err)
	require.True(t, result.Updated)
	require.Equal(t, second.String(), result.NewCommit)
	data, err := os.ReadFile(filepath.Join(destination, "prompts", "prompt.md"))
	require.NoError(t, err)
	require.Equal(t, "second", string(data))
}

func TestConventionalManifestReportsResourceStatFailures(t *testing.T) {
	for _, resource := range []string{"skills", "prompts", "themes", "extensions"} {
		t.Run(resource, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "prompts"), 0o700))
			if resource == "prompts" {
				require.NoError(t, os.Remove(filepath.Join(root, "prompts")))
				require.NoError(t, os.Mkdir(filepath.Join(root, "skills"), 0o700))
			}
			target := filepath.Join(root, resource)
			require.NoError(t, os.Symlink(target, target))
			_, err := loadManifestOrConvention(root)
			require.Error(t, err)
		})
	}
}

func TestNPMRangeAndTagUpdatesRetainRequestedSpecifier(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "")
	t.Setenv("PI_OFFLINE", "")
	for _, version := range []string{"latest", "next", "^1.0.0", "~1.0.0", "1.x", ">=1.0.0 <3.0.0"} {
		for _, pinned := range []bool{false, true} {
			t.Run(version+fmt.Sprint(pinned), func(t *testing.T) {
				manager := NewUpdateManager(t.TempDir(), t.TempDir())
				destination := manager.mgr.InstallPath("plugin", ScopeUser)
				require.NoError(t, os.MkdirAll(destination, 0o700))
				require.NoError(t, (InstallSidecar{Source: "npm:plugin@" + version, Ref: "1.0.0", Pinned: pinned}).Write(destination))
				packs := 0
				manager.installer.npmPack = func(ctx context.Context, spec, dir string) (string, error) {
					packs++
					require.Equal(t, "plugin@"+version, spec)
					return writePiTarball(t, dir, `{"name":"plugin","version":"1.0.1"}`), nil
				}
				result, err := manager.Update(t.Context(), "plugin", ScopeUser)
				require.NoError(t, err)
				require.True(t, result.Updated)
				require.Equal(t, 1, packs)
				sidecar, err := LoadSidecar(destination)
				require.NoError(t, err)
				require.False(t, sidecar.Pinned)
				require.Equal(t, "npm:plugin@"+version, sidecar.Source)
			})
		}
	}
}

func TestUninstallWaitsForPackagePreparation(t *testing.T) {
	for _, npm := range []bool{false, true} {
		t.Run(fmt.Sprint(npm), func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, "plugin")
			require.NoError(t, os.MkdirAll(destination, 0o700))
			source := "/local/plugin"
			if npm {
				source = "npm:plugin"
			}
			require.NoError(t, (InstallSidecar{Source: source}).Write(destination))
			sentinel := filepath.Join(destination, "old")
			require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
			unlock, err := lockPackageWriter(t.Context(), destination)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeoutCause(t.Context(), 30*time.Millisecond, errors.New("uninstall wait cancelled"))
			defer cancel()
			err = NewManager(base, t.TempDir()).Uninstall(ctx, "plugin", ScopeUser)
			require.NoError(t, unlock())
			require.ErrorIs(t, err, context.Cause(ctx))
			require.FileExists(t, sentinel)
			require.NoError(t, NewManager(base, t.TempDir()).Uninstall(t.Context(), "plugin", ScopeUser))
			require.NoDirExists(t, destination)
		})
	}
}

func TestNPMUpdateRejectsStaleInstallationSnapshot(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	t.Setenv("TAU_OFFLINE", "")
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	previous := InstallSidecar{Source: "npm:plugin", Ref: "1.0.0", Scope: string(ScopeUser)}
	current := previous
	current.Ref = "2.0.0"
	require.NoError(t, current.Write(destination))
	manager := NewUpdateManager(base, t.TempDir())
	called := false
	manager.installer.npmPack = func(ctx context.Context, spec, dst string) (string, error) {
		called = true
		return writePiTarball(t, dst, `{"name":"plugin","version":"1.1.0"}`), nil
	}
	_, err := manager.updateNPM(t.Context(), "plugin", ScopeUser, destination, &previous)
	require.ErrorContains(t, err, "changed during update")
	require.False(t, called)
	after, err := LoadSidecar(destination)
	require.NoError(t, err)
	require.Equal(t, current, *after)
}

func TestGitUpdateRejectsStaleInstallationSnapshot(t *testing.T) {
	for _, pi := range []bool{false, true} {
		t.Run(fmt.Sprint(pi), func(t *testing.T) {
			source := t.TempDir()
			repo, err := git.PlainInit(source, false)
			require.NoError(t, err)
			tree, err := repo.Worktree()
			require.NoError(t, err)
			if pi {
				require.NoError(t, os.WriteFile(filepath.Join(source, "package.json"), []byte(`{"name":"plugin","version":"2.0.0","pi":{"prompts":["prompt.md"]}}`), 0o600))
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(source, "tau-package.toml"), []byte("[package]\nname = 'plugin'\nversion = '2.0.0'\n[[prompts]]\npath = 'prompt.md'\n"), 0o600))
			}
			require.NoError(t, os.WriteFile(filepath.Join(source, "prompt.md"), []byte("candidate"), 0o600))
			_, err = tree.Add(".")
			require.NoError(t, err)
			hash, err := tree.Commit("candidate", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}})
			require.NoError(t, err)
			base := t.TempDir()
			destination := filepath.Join(base, "plugin")
			require.NoError(t, os.MkdirAll(destination, 0o700))
			previous := InstallSidecar{Source: "file://" + source, CommitSHA: "previous", Pi: pi}
			current := previous
			current.CommitSHA = "newer installation"
			require.NoError(t, current.Write(destination))
			require.NoError(t, os.WriteFile(filepath.Join(destination, "keep"), []byte("current"), 0o600))
			err = NewUpdateManager(base, t.TempDir()).updatePackageExpected(t.Context(), destination, previous.Source, "HEAD", hash.String(), ScopeUser, &previous)
			require.ErrorContains(t, err, "changed during update")
			require.FileExists(t, filepath.Join(destination, "keep"))
			after, err := LoadSidecar(destination)
			require.NoError(t, err)
			require.Equal(t, current, *after)
			require.NoFileExists(t, filepath.Join(destination, "prompt.md"))
		})
	}
}

func TestPiNPMUpdateRejectsStaleInstallationSnapshot(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	t.Setenv("TAU_OFFLINE", "")
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	previous := InstallSidecar{Source: "npm:plugin", Ref: "1.0.0", Pi: true}
	current := previous
	current.Ref = "2.0.0"
	require.NoError(t, current.Write(destination))
	manager := NewUpdateManager(base, t.TempDir())
	manager.installer.npmPack = func(ctx context.Context, spec, dst string) (string, error) {
		return writePiTarball(t, dst, `{"name":"plugin","version":"1.1.0","dependencies":{"dep":"1"},"pi":{"extensions":["extension.ts"]}}`), nil
	}
	setup := false
	manager.installer.npmInstall = func(context.Context, string, []string) error { setup = true; return nil }
	manager.installer.ensureShim = func(context.Context, string) error { setup = true; return nil }
	_, err := manager.updateNPM(t.Context(), "plugin", ScopeUser, destination, &previous)
	require.ErrorContains(t, err, "changed during update")
	require.False(t, setup)
	after, err := LoadSidecar(destination)
	require.NoError(t, err)
	require.Equal(t, current, *after)
}

func TestUpdateRecoversInterruptedInstallationBeforeReadingMetadata(t *testing.T) {
	t.Setenv("TAU_OFFLINE", "")
	t.Setenv("PI_OFFLINE", "")
	base := t.TempDir()
	destination := filepath.Join(base, "plugin")
	previous := filepath.Join(packageRecoveryPath(destination), "previous")
	require.NoError(t, os.MkdirAll(previous, 0o700))
	require.NoError(t, (InstallSidecar{Source: "npm:plugin@1.0.0", Ref: "1.0.0", Pinned: true}).Write(previous))
	result, err := NewUpdateManager(base, t.TempDir()).Update(t.Context(), "plugin", ScopeUser)
	require.NoError(t, err)
	require.False(t, result.Updated)
	require.Contains(t, result.Message, "version-pinned")
	require.FileExists(t, sidecarPath(destination))
	require.NoDirExists(t, packageRecoveryPath(destination))
}
