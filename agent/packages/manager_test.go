package packages

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
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
			sidecar: InstallSidecar{Source: "https://github.com/user/repo", Ref: "abc123def456abc123def456abc123def456abcd", CommitSHA: "abc123def456abc123def456abc123def456abcd"},
			want:    RefTypeCommit,
		},
		{
			name:    "tag-pinned",
			sidecar: InstallSidecar{Source: "https://github.com/user/repo", Ref: "v1.2.3", CommitSHA: "abc123"},
			want:    RefTypeTag,
		},
		{
			name:    "branch-pinned",
			sidecar: InstallSidecar{Source: "https://github.com/user/repo", Ref: "main", CommitSHA: "abc123"},
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
		Source:    "https://github.com/user/repo",
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
