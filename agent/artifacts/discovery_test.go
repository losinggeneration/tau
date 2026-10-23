package artifacts

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
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
