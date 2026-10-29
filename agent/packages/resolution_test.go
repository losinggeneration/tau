package packages

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPackageIdentityNormalizesSources(t *testing.T) {
	base := t.TempDir()
	for _, sources := range [][]string{
		{"npm:@scope/example@1", "npm:@scope/example@2"},
		{"https://github.com/team/repo.git@main", "git@github.com:team/repo.git@v2", "ssh://git@github.com/team/repo", "git:github.com/team/repo@main", "team/repo"},
		{"./local/../example", filepath.Join(base, "example")},
	} {
		want, err := packageIdentity(sources[0], base)
		require.NoError(t, err)
		for _, source := range sources {
			got, err := packageIdentity(source, base)
			require.NoError(t, err)
			assert.Equal(t, want, got, source)
		}
	}
	for _, source := range []string{"", "npm:../../escape", "npm:@scope/../escape", "npm:", "https://host/../escape", "ssh://host"} {
		_, err := packageIdentity(source, base)
		require.Error(t, err, source)
	}
}

func TestResolvePackageSourcesKeepsScopePrecedenceAndDeltas(t *testing.T) {
	root := t.TempDir()
	userBase, projectBase := filepath.Join(root, "user"), filepath.Join(root, "project", ".tau")
	userDir, projectDir := filepath.Join(userBase, "packages"), filepath.Join(projectBase, "packages")
	install := func(base, name, source string) string {
		t.Helper()
		dir := filepath.Join(base, name)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, (InstallSidecar{Source: source}).Write(dir))
		return dir
	}
	old := install(userDir, "@scope/shared", "npm:@scope/shared@1")
	current := install(projectDir, "@scope/shared", "npm:@scope/shared@2")
	gitRoot := install(userDir, "manifest-name", "git@github.com:team/repo.git@main")
	implicit := install(userDir, "implicit", "npm:implicit")
	local := filepath.Join(root, "local")
	require.NoError(t, os.MkdirAll(local, 0o700))
	load := func(base, raw string) *ai.Settings {
		t.Helper()
		require.NoError(t, os.MkdirAll(base, 0o700))
		path := filepath.Join(base, "settings.json")
		require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
		s, err := ai.LoadSettings(path)
		require.NoError(t, err)
		return s
	}
	user := load(userBase, `{"packages":[{"source":"npm:@scope/shared@1","prompts":["user"]},"https://github.com/team/repo@v1","../local"]}`)
	project := load(projectBase, `{"packages":[{"source":"npm:@scope/shared@2","prompts":[]},{"source":"git:github.com/team/repo@v2","autoload":false,"prompts":["-prompts/drop.md"]},"../../local"]}`)
	got, err := ResolvePackageSources(ai.MergeSettings(user, project), userDir, projectDir)
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, current, got[0].Root)
	assert.NotNil(t, got[0].Source.Prompts)
	assert.Equal(t, gitRoot, got[1].Root)
	require.NotNil(t, got[1].Delta)
	assert.Equal(t, []string{"-prompts/drop.md"}, got[1].Delta.Prompts)
	assert.Equal(t, local, got[2].Root)
	assert.Equal(t, implicit, got[3].Root)
	for _, pkg := range got {
		assert.NotEqual(t, old, pkg.Root)
	}
	// A project delta reuses the user install even if a project install exists.
	project = load(projectBase, `{"packages":[{"source":"npm:@scope/shared@99","autoload":false,"themes":[]}]}`)
	got, err = ResolvePackageSources(ai.MergeSettings(user, project), userDir, projectDir)
	require.NoError(t, err)
	require.NotEmpty(t, got)
	assert.Equal(t, old, got[0].Root)
	require.NotNil(t, got[0].Delta)
	assert.Equal(t, []string{"user"}, got[0].Source.Prompts)
}

func TestResolvePackageSourcesResolvesProjectEntryAgainstUserInstall(t *testing.T) {
	root := t.TempDir()
	userBase, projectBase := filepath.Join(root, "user"), filepath.Join(root, "project", ".tau")
	userDir, projectDir := filepath.Join(userBase, "packages"), filepath.Join(projectBase, "packages")
	installed := filepath.Join(userDir, "example")
	require.NoError(t, os.MkdirAll(installed, 0o700))
	require.NoError(t, (InstallSidecar{Source: "npm:example"}).Write(installed))
	load := func(base, raw string) *ai.Settings {
		t.Helper()
		require.NoError(t, os.MkdirAll(base, 0o700))
		path := filepath.Join(base, "settings.json")
		require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
		s, err := ai.LoadSettings(path)
		require.NoError(t, err)
		return s
	}
	user := load(userBase, `{}`)
	project := load(projectBase, `{"packages":[{"source":"npm:example","prompts":["project"]}]}`)
	got, err := ResolvePackageSources(ai.MergeSettings(user, project), userDir, projectDir)
	require.NoError(t, err)
	require.Len(t, got, 1, "project entry did not resolve the user-scope install")
	assert.Equal(t, installed, got[0].Root)
	assert.Equal(t, []string{"project"}, got[0].Source.Prompts)
}

func TestResolvePackageSourcesUserEntryDoesNotAdoptProjectInstall(t *testing.T) {
	root := t.TempDir()
	userBase, projectBase := filepath.Join(root, "user"), filepath.Join(root, "project", ".tau")
	userDir, projectDir := filepath.Join(userBase, "packages"), filepath.Join(projectBase, "packages")
	installed := filepath.Join(projectDir, "example")
	require.NoError(t, os.MkdirAll(installed, 0o700))
	require.NoError(t, (InstallSidecar{Source: "npm:example"}).Write(installed))
	load := func(base, raw string) *ai.Settings {
		t.Helper()
		require.NoError(t, os.MkdirAll(base, 0o700))
		path := filepath.Join(base, "settings.json")
		require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
		s, err := ai.LoadSettings(path)
		require.NoError(t, err)
		return s
	}
	user := load(userBase, `{"packages":["npm:example"]}`)
	project := load(projectBase, `{}`)
	got, err := ResolvePackageSources(ai.MergeSettings(user, project), userDir, projectDir)
	require.NoError(t, err)
	assert.Empty(t, got, "user entry adopted a project-local install")
}

func TestResolvePackageSourcesRejectsMalformedInstallMetadata(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "example")
	require.NoError(t, os.MkdirAll(pkg, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(pkg, ".tau-install.json"), []byte("{"), 0o600))
	_, err := ResolvePackageSources(nil, dir, "")
	require.ErrorContains(t, err, "sidecar")
}

func TestResolvePackageSourcesClaimsUnstampedNPMInstallations(t *testing.T) {
	userDir, projectDir := t.TempDir(), t.TempDir()
	for _, base := range []string{userDir, projectDir} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, "example"), 0o700))
	}
	settings := ai.MergeSettings(&ai.Settings{Packages: []ai.PackageSource{{Source: "npm:example@1"}}}, &ai.Settings{Packages: []ai.PackageSource{{Source: "npm:example@2", Prompts: []string{}}}})
	resolved, err := ResolvePackageSources(settings, userDir, projectDir)
	require.NoError(t, err)
	require.Len(t, resolved, 1, "unfiltered user installation bypassed project policy")
	assert.Equal(t, filepath.Join(projectDir, "example"), resolved[0].Root)
}

func TestResolvePackageSourcesCanonicalizesRelativeInstallRoots(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	path := filepath.Join(".tau", "packages", "example")
	require.NoError(t, os.MkdirAll(path, 0o700))
	require.NoError(t, (InstallSidecar{Source: "npm:example"}).Write(path))
	got, err := ResolvePackageSources(nil, "", filepath.Join(".tau", "packages"))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, filepath.Join(root, path), got[0].Root)
}

func TestInstalledRootsOnlyIncludesDirectoryTargets(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(base, "@scope", "real"), 0o700))
	require.NoError(t, os.Symlink(target, filepath.Join(base, "linked")))
	require.NoError(t, os.Symlink(target, filepath.Join(base, "@scope", "linked")))
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("not a package"), 0o600))
	for _, prefix := range []string{"", "@scope"} {
		require.NoError(t, os.Symlink(file, filepath.Join(base, prefix, "file")))
		require.NoError(t, os.Symlink(filepath.Join(target, "missing"), filepath.Join(base, prefix, "broken")))
	}
	got, err := InstalledRoots(base)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(base, "@scope", "linked"), filepath.Join(base, "@scope", "real"), filepath.Join(base, "linked")}, got)
	resolved, err := ResolvePackageSources(nil, base, "")
	require.NoError(t, err)
	require.Len(t, resolved, 2)
}

func TestInstalledRootsReportsLinkedTargetStatFailure(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, os.Symlink("loop", filepath.Join(base, "loop")))
	got, err := InstalledRoots(base)
	require.ErrorContains(t, err, "stat installed package")
	require.Empty(t, got)
}

func TestInstalledRootsExcludePendingAndRecoveryPackages(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"visible", ".tau-install-pending", ".tau-install-recovery", "@scope/visible", "@scope/.tau-install-pending"} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, name), 0o700))
	}
	roots, err := InstalledRoots(base)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{filepath.Join(base, "visible"), filepath.Join(base, "@scope", "visible")}, roots)
}

func TestPackageIdentityRetainsAtScopedGitPaths(t *testing.T) {
	identity, err := packageIdentity("file:///tmp/@scope/plugin", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "git:/tmp/@scope/plugin", identity)
	pinned, err := packageIdentity("file:///tmp/@scope/plugin@feature/fix", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, identity, pinned)
}

func TestInstalledRootsRecoversInterruptedPackages(t *testing.T) {
	for _, name := range []string{"plugin", "@scope/plugin"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, filepath.FromSlash(name))
			previous := filepath.Join(packageRecoveryPath(destination), "previous")
			require.NoError(t, os.MkdirAll(previous, 0o700))
			require.NoError(t, (InstallSidecar{Source: "npm:" + name, Ref: "1.0.0"}).Write(previous))
			roots, err := InstalledRoots(base)
			require.NoError(t, err)
			require.Equal(t, []string{destination}, roots)
			require.NoDirExists(t, packageRecoveryPath(destination))
		})
	}
}

func TestDeniedProjectPackagesAreNotInspected(t *testing.T) {
	global, project := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(global, "settings.json"), []byte(`{"defaultProjectTrust":"never"}`), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(project, ".tau"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(project, ".tau", "settings.json"), []byte("malformed"), 0o600))
	settings, err := ai.LoadTrustedSettings(t.Context(), global, project, t.TempDir(), ai.ProjectTrustOptions{})
	require.NoError(t, err)
	base := filepath.Join(project, ".tau", "packages")
	require.NoError(t, os.MkdirAll(filepath.Join(base, "broken"), 0o700))
	require.NoError(t, os.WriteFile(sidecarPath(filepath.Join(base, "broken")), []byte("malformed"), 0o600))
	resolved, err := ResolvePackageSources(settings, t.TempDir(), base)
	require.NoError(t, err)
	require.Empty(t, resolved)
}

func TestInstallMissingSourcesFollowsScopeDecisionAndOffline(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	t.Setenv("TAU_OFFLINE", "")
	setup := func(t *testing.T, trusted bool) (*InstallManager, *ai.Settings, string, string, *[]string) {
		t.Helper()
		global, project := t.TempDir(), t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(global, "settings.json"), []byte(`{"packages":["npm:user-pkg@1.0.0","./local-missing"]}`), 0o600))
		require.NoError(t, os.Mkdir(filepath.Join(project, ".tau"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(project, ".tau", "settings.json"), []byte(`{"packages":["npm:project-pkg@2.0.0"]}`), 0o600))
		settings, err := ai.LoadTrustedSettings(t.Context(), global, project, t.TempDir(), ai.ProjectTrustOptions{Override: new(trusted)})
		require.NoError(t, err)
		userDir, projectDir := filepath.Join(global, "packages"), filepath.Join(project, ".tau", "packages")
		manager := NewInstallManager(userDir, projectDir)
		var packed []string
		manager.npmPack = func(_ context.Context, spec, dst string) (string, error) {
			packed = append(packed, spec)
			name, version, _ := strings.Cut(spec, "@")
			return writePiTarball(t, dst, `{"name":"`+name+`","version":"`+version+`"}`), nil
		}
		return manager, settings, userDir, projectDir, &packed
	}

	t.Run("no decision installs each source into its scope", func(t *testing.T) {
		manager, settings, userDir, projectDir, packed := setup(t, true)
		installed, err := manager.InstallMissingSources(t.Context(), settings, nil)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"npm:user-pkg@1.0.0", "npm:project-pkg@2.0.0"}, installed)
		assert.ElementsMatch(t, []string{"user-pkg@1.0.0", "project-pkg@2.0.0"}, *packed)
		require.FileExists(t, filepath.Join(userDir, "user-pkg", "package.json"))
		require.FileExists(t, filepath.Join(projectDir, "project-pkg", "package.json"))
		resolved, err := ResolvePackageSources(settings, userDir, projectDir)
		require.NoError(t, err)
		assert.Len(t, resolved, 2)
		installed, err = manager.InstallMissingSources(t.Context(), settings, nil)
		require.NoError(t, err)
		assert.Empty(t, installed)
		assert.Len(t, *packed, 2, "installed sources were installed again")
	})
	t.Run("skip and error decisions install nothing", func(t *testing.T) {
		manager, settings, _, _, packed := setup(t, true)
		var asked []string
		installed, err := manager.InstallMissingSources(t.Context(), settings, func(_ context.Context, source string) (MissingSourceAction, error) {
			asked = append(asked, source)
			return MissingSourceSkip, nil
		})
		require.NoError(t, err)
		assert.Empty(t, installed)
		assert.ElementsMatch(t, []string{"npm:user-pkg@1.0.0", "npm:project-pkg@2.0.0"}, asked)
		_, err = manager.InstallMissingSources(t.Context(), settings, func(context.Context, string) (MissingSourceAction, error) {
			return MissingSourceError, nil
		})
		require.ErrorContains(t, err, "missing source")
		assert.Empty(t, *packed)
	})
	t.Run("untrusted project installs only user sources", func(t *testing.T) {
		manager, settings, _, projectDir, packed := setup(t, false)
		_, err := manager.InstallMissingSources(t.Context(), settings, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"user-pkg@1.0.0"}, *packed)
		require.NoDirExists(t, projectDir)
	})
	t.Run("offline installs nothing", func(t *testing.T) {
		t.Setenv("TAU_OFFLINE", "1")
		manager, settings, _, _, packed := setup(t, true)
		_, err := manager.InstallMissingSources(t.Context(), settings, func(context.Context, string) (MissingSourceAction, error) {
			return MissingSourceError, nil
		})
		require.NoError(t, err)
		assert.Empty(t, *packed)
	})
}

func TestInstallMissingSourcesReinstallsVersionOutsideRange(t *testing.T) {
	t.Setenv("PI_OFFLINE", "")
	t.Setenv("TAU_OFFLINE", "")
	for _, tc := range []struct {
		name, source, installed, packs string
		reinstall                      bool
	}{
		{name: "outside caret range", source: "npm:pkg@^1.2.0", installed: `{"name":"pkg","version":"1.0.0"}`, packs: "1.5.0", reinstall: true},
		{name: "inside caret range", source: "npm:pkg@^1.2.0", installed: `{"name":"pkg","version":"1.4.1"}`},
		{name: "pinned mismatch", source: "npm:pkg@2.0.0", installed: `{"name":"pkg","version":"1.0.0"}`, packs: "2.0.0", reinstall: true},
		{name: "dist-tag always matches", source: "npm:pkg@latest", installed: `{"name":"pkg","version":"0.1.0"}`},
		{name: "no version always matches", source: "npm:pkg", installed: `{"name":"pkg","version":"0.1.0"}`},
		{name: "missing installed version", source: "npm:pkg@^1.0.0", installed: `{"name":"pkg"}`, packs: "1.5.0", reinstall: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(global, "settings.json"), []byte(`{"packages":["`+tc.source+`"]}`), 0o600))
			settings, err := ai.LoadTrustedSettings(t.Context(), global, t.TempDir(), t.TempDir(), ai.ProjectTrustOptions{Override: new(false)})
			require.NoError(t, err)
			userDir := filepath.Join(global, "packages")
			require.NoError(t, os.MkdirAll(filepath.Join(userDir, "pkg"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(userDir, "pkg", "package.json"), []byte(tc.installed), 0o600))
			manager := NewInstallManager(userDir, "")
			var packed []string
			manager.npmPack = func(_ context.Context, spec, dst string) (string, error) {
				packed = append(packed, spec)
				return writePiTarball(t, dst, `{"name":"pkg","version":"`+tc.packs+`"}`), nil
			}
			installed, err := manager.InstallMissingSources(t.Context(), settings, nil)
			require.NoError(t, err)
			if !tc.reinstall {
				assert.Empty(t, installed)
				assert.Empty(t, packed)
				return
			}
			assert.Equal(t, []string{tc.source}, installed)
			data, err := os.ReadFile(filepath.Join(userDir, "pkg", "package.json"))
			require.NoError(t, err)
			assert.Contains(t, string(data), `"`+tc.packs+`"`)
		})
	}
}
