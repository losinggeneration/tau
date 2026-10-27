package artifacts

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/agent/packages"
	"github.com/losinggeneration/tau/ai"
)

func TestWalkSettingsPathsUseConfiguredScopeAndModifiers(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	for _, name := range []string{"keep.md", "drop.md", "last.md"} {
		path := filepath.Join(cwd, "extra", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("prompt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	auto := filepath.Join(cwd, ".tau", "prompts", "auto.md")
	if err := os.MkdirAll(filepath.Dir(auto), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auto, []byte("auto"), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(home, "shared", "home.md")
	if err := os.MkdirAll(filepath.Dir(shared), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte("home"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := Walk(DiscoveryConfig{
		HomeDir: home, CWD: cwd, DisableSkills: true, DisableThemes: true, DisableExtensions: true,
		SettingsPaths: map[ArtifactKind][]string{KindPrompt: {
			"extra", "~/shared", "!extra/*", "+extra/keep.md", "+extra/last.md", "-extra/last.md", "!.tau/prompts/*", "!~/shared/*",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.SourcePath)
	}
	want := []string{filepath.Join(cwd, "extra", "keep.md")}
	if !slices.Equal(got, want) {
		t.Fatalf("paths=%v, want %v", got, want)
	}
	// CLI entries remain explicit even when settings exclude the same path.
	entries, err = Walk(DiscoveryConfig{
		HomeDir: home, CWD: cwd, DisableSkills: true, DisableThemes: true, DisableExtensions: true,
		CLIPaths:      map[ArtifactKind][]string{KindPrompt: {"~/shared/home.md"}},
		SettingsPaths: map[ArtifactKind][]string{KindPrompt: {"!~/shared/*", "!.tau/prompts/*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SourcePath != shared || entries[0].Tier != TierCLI {
		t.Fatalf("CLI paths = %+v", entries)
	}
}

func TestSettingsTierUsesDirectoryBoundaries(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	home := filepath.Join(root, "home")
	for _, tc := range []struct {
		path string
		want SourceTier
	}{
		{filepath.Join(cwd, "prompt.md"), TierProject},
		{filepath.Join(home, "prompt.md"), TierUser},
		{filepath.Join(root, "project-other", "prompt.md"), TierSettings},
		{filepath.Join(root, "home-other", "prompt.md"), TierSettings},
	} {
		if err := os.MkdirAll(filepath.Dir(tc.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tc.path, []byte("prompt"), 0o600); err != nil {
			t.Fatal(err)
		}
		entries, tier := discoverArtifactPathWithSettingsTier(tc.path, cwd, home, KindPrompt)
		if len(entries) != 1 || tier != tc.want {
			t.Errorf("%s tier=%v, want %v", tc.path, tier, tc.want)
		}
	}
}

func TestWalkKeepsResourceScopeFiltersIndependent(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	userDir, projectDir := filepath.Join(home, "shared"), filepath.Join(cwd, "shared")
	for _, dir := range []string{userDir, projectDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"keep.md", "drop.md"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("prompt"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := Walk(DiscoveryConfig{
		HomeDir: home, CWD: cwd, DisableSkills: true, DisableThemes: true, DisableExtensions: true,
		SettingsScopes: map[SourceTier]map[ArtifactKind][]string{
			TierUser:    {KindPrompt: {userDir, filepath.Join(userDir, "drop*")}},
			TierProject: {KindPrompt: {projectDir, "!" + projectDir + "/*", "+" + filepath.Join(projectDir, "keep.md")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].SourcePath != filepath.Join(projectDir, "keep.md") || entries[0].Tier != TierProject || entries[1].SourcePath != filepath.Join(userDir, "drop.md") || entries[1].Tier != TierUser {
		t.Fatalf("independent scopes = %+v", entries)
	}
}

func TestWalkFindsScopedPackagesAndKeepsSearchPriority(t *testing.T) {
	root := t.TempDir()
	project, user := filepath.Join(root, "z-project"), filepath.Join(root, "a-user")
	for _, base := range []string{project, user} {
		for _, name := range []string{"plain", "@scope/scoped"} {
			pkg := filepath.Join(base, name)
			files := map[string]string{
				"prompts/" + filepath.Base(name) + ".md":      base,
				"skills/" + filepath.Base(name) + "/SKILL.md": "---\nname: " + filepath.Base(name) + "\ndescription: test\n---\n" + base,
				"themes/" + filepath.Base(name) + ".json":     `{}`,
			}
			if name == "@scope/scoped" {
				files["tau-package.toml"] = `[package]
name = "scoped"
version = "1.0.0"
[[prompt]]
path = "prompts/"
[[skill]]
path = "skills/"
[[theme]]
path = "themes/"
`
			}
			for path, contents := range files {
				path = filepath.Join(pkg, path)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	entries, err := Walk(DiscoveryConfig{CWD: t.TempDir(), HomeDir: t.TempDir(), PackagePaths: []string{project, user}})
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[ArtifactKind]int)
	for _, entry := range entries {
		if entry.Tier == TierPackages {
			counts[entry.Kind]++
		}
	}
	for _, kind := range []ArtifactKind{KindSkill, KindPrompt, KindTheme} {
		if counts[kind] != 4 {
			t.Errorf("%s entries = %d, want scoped and plain packages from both directories", kind, counts[kind])
		}
	}
	registry := NewPromptRegistry(nil)
	if err := registry.Load(entries); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"plain", "scoped"} {
		prompt, ok := registry.Get(name)
		if !ok || string(prompt.Body) != project {
			t.Errorf("%s prompt = %+v, want project package", name, prompt)
		}
	}
}

func TestWalkConfiguredPackageFiltersDoNotLeakInstalledResources(t *testing.T) {
	root := t.TempDir()
	pkgRoot := filepath.Join(root, "packages", "example")
	for _, name := range []string{"keep", "drop"} {
		for _, path := range []string{filepath.Join(pkgRoot, "prompts", name+".md"), filepath.Join(pkgRoot, "skills", name, "SKILL.md"), filepath.Join(pkgRoot, "themes", name+".json")} {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("body"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := DiscoveryConfig{CWD: t.TempDir(), HomeDir: t.TempDir(), PackagePaths: []string{filepath.Dir(pkgRoot)}, Packages: []packages.ResolvedPackage{{Root: pkgRoot, Source: ai.PackageSource{Prompts: []string{"+prompts/keep.md", "!prompts/*"}, Themes: []string{}, Skills: []string{"skills/keep"}}}}}
	entries, err := Walk(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entries = slices.DeleteFunc(entries, func(entry Entry) bool { return entry.Tier != TierPackages })
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
	}
	for _, entry := range entries {
		if !strings.Contains(entry.SourcePath, "keep") {
			t.Fatalf("unfiltered entry %v", entry)
		}
	}
}

func TestPiManifestResourcesDoNotLeakConventions(t *testing.T) {
	root := t.TempDir()
	for path, body := range map[string]string{
		"package.json":                      `{"pi":{"skills":["custom-skills/*"],"prompts":["custom-prompts/**/*.md","!custom-prompts/**/drop.md"],"themes":[]}}`,
		"custom-skills/keep/SKILL.md":       "---\nname: keep\ndescription: kept\n---\nbody",
		"custom-prompts/keep.md":            "kept",
		"custom-prompts/nested/keep-too.md": "also kept",
		"custom-prompts/nested/drop.md":     "excluded",
		"prompts/convention.md":             "must not load",
		"themes/convention.json":            `{}`,
		"custom-prompts/.hidden.md":         "must not load",
	} {
		full := filepath.Join(root, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o600))
	}
	found := discoverPackageRoot(root, DiscoveryConfig{})
	var paths []string
	for _, entry := range found {
		rel, err := filepath.Rel(root, entry.SourcePath)
		require.NoError(t, err)
		paths = append(paths, filepath.ToSlash(rel))
	}
	assert.ElementsMatch(t, []string{"custom-skills/keep/SKILL.md", "custom-prompts/keep.md", "custom-prompts/nested/keep-too.md"}, paths)
	for _, body := range []string{`{"pi":{}}`, `{"pi":{"skills":[],"prompts":[],"themes":[]}}`} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(body), 0o600))
		assert.Empty(t, discoverPackageRoot(root, DiscoveryConfig{}), "Pi manifest absence/empty declarations must not load undeclared conventions")
	}
}

func TestWalkDeniedProjectKeepsGlobalAndExplicitResources(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	for _, base := range []string{filepath.Join(home, ".tau", "agent", "prompts"), filepath.Join(cwd, ".tau", "prompts"), filepath.Join(cwd, "extra")} {
		require.NoError(t, os.MkdirAll(base, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(base, "example.md"), []byte("prompt"), 0o600))
	}
	entries, err := Walk(DiscoveryConfig{HomeDir: home, CWD: cwd, ProjectTrusted: new(false), CLIPaths: map[ArtifactKind][]string{KindPrompt: {filepath.Join(cwd, "extra")}}})
	require.NoError(t, err)
	var tiers []SourceTier
	for _, entry := range entries {
		if entry.Kind == KindPrompt {
			tiers = append(tiers, entry.Tier)
		}
	}
	require.ElementsMatch(t, []SourceTier{TierCLI, TierUser}, tiers)
}

func TestWalkDeniedProjectSkipsAncestorSkillsAndImplicitPackages(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	cwd := filepath.Join(root, "nested")
	require.NoError(t, os.Mkdir(cwd, 0o700))
	for _, base := range []string{filepath.Join(root, ".agents", "skills", "unsafe"), filepath.Join(home, ".agents", "skills", "safe")} {
		require.NoError(t, os.MkdirAll(base, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(base, "SKILL.md"), []byte("---\nname: test\ndescription: test\n---\nbody"), 0o600))
	}
	pkg := filepath.Join(cwd, ".tau", "packages", "unsafe")
	require.NoError(t, os.MkdirAll(filepath.Join(pkg, "prompts"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(pkg, "prompts", "unsafe.md"), []byte("prompt"), 0o600))
	entries, err := Walk(DiscoveryConfig{HomeDir: home, CWD: cwd, ProjectTrusted: new(false), PackagePaths: []string{filepath.Dir(pkg)}})
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.SourcePath, root)
	}
	var skills []Entry
	for _, entry := range entries {
		if entry.Kind == KindSkill {
			skills = append(skills, entry)
		}
	}
	require.Len(t, skills, 1)
	require.Equal(t, TierUser, skills[0].Tier)
}
