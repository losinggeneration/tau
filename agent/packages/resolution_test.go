package packages

import (
	"os"
	"path/filepath"
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
