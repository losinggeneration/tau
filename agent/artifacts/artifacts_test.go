package artifacts

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/commands"
)

// TestThemeRegistry tests the ThemeRegistry implementation.
func TestThemeRegistry(t *testing.T) {
	registry := NewThemeRegistry(nil)

	t.Run("Register and Get", func(t *testing.T) {
		entry := Entry{
			Tier:       TierUser,
			Kind:       KindTheme,
			SourcePath: "/test/dark.json",
			Raw:        []byte(`{"name": "dark", "colors": {"bg": "#000"}}`),
		}
		err := registry.Register(entry)
		require.NoError(t, err)

		theme, ok := registry.Get("dark")
		require.True(t, ok)
		assert.Equal(t, "dark", theme.ID)
		assert.Equal(t, "dark", theme.Name)
		assert.Equal(t, TierUser, theme.Tier)
		assert.NotNil(t, theme.ExtraFields)
	})

	t.Run("Get by prefix", func(t *testing.T) {
		entry := Entry{
			Tier:       TierUser,
			Kind:       KindTheme,
			SourcePath: "/test/solarized-dark.json",
			Raw:        []byte(`{"name": "solarized-dark"}`),
		}
		err := registry.Register(entry)
		require.NoError(t, err)

		theme, ok := registry.Get("solar")
		require.True(t, ok)
		assert.Equal(t, "solarized-dark", theme.ID)
	})

	t.Run("Get non-existent", func(t *testing.T) {
		_, ok := registry.Get("nonexistent")
		assert.False(t, ok)
	})

	t.Run("List", func(t *testing.T) {
		themes := registry.List()
		assert.GreaterOrEqual(t, len(themes), 2)
	})

	t.Run("ListByTier", func(t *testing.T) {
		themes := registry.ListByTier(TierUser)
		assert.GreaterOrEqual(t, len(themes), 1)

		themes = registry.ListByTier(TierBuiltin)
		assert.Equal(t, 0, len(themes))
	})

	t.Run("Reload", func(t *testing.T) {
		entries := []Entry{
			{
				Tier:       TierCLI,
				Kind:       KindTheme,
				SourcePath: "/cli/test.json",
				Raw:        []byte(`{"name": "cli-theme"}`),
			},
		}
		require.NoError(t, registry.Reload(entries))

		themes := registry.List()
		assert.Equal(t, 1, len(themes))

		_, ok := registry.Get("dark")
		assert.False(t, ok) // Old entries should be cleared
	})

	t.Run("First wins on collision", func(t *testing.T) {
		// Create fresh registry for this test
		r := NewThemeRegistry(nil)

		entry1 := Entry{
			Tier:       TierUser,
			Kind:       KindTheme,
			SourcePath: "/first/theme.json",
			Raw:        []byte(`{"name": "test-theme", "version": "1"}`),
		}
		entry2 := Entry{
			Tier:       TierProject,
			Kind:       KindTheme,
			SourcePath: "/second/theme.json",
			Raw:        []byte(`{"name": "test-theme", "version": "2"}`),
		}

		err := r.Register(entry1)
		require.NoError(t, err)
		err = r.Register(entry2)
		require.NoError(t, err) // Should not error, just skip

		theme, ok := r.Get("test-theme")
		require.True(t, ok)
		assert.Equal(t, "/first/theme.json", theme.SourcePath) // First should win
	})

	t.Run("Fallback to filename slug", func(t *testing.T) {
		entry := Entry{
			Tier:       TierUser,
			Kind:       KindTheme,
			SourcePath: "/test/my-custom-theme.json",
			Raw:        []byte(`{}`), // No name field
		}
		err := registry.Register(entry)
		require.NoError(t, err)

		theme, ok := registry.Get("my-custom-theme")
		require.True(t, ok)
		assert.Equal(t, "my-custom-theme", theme.ID)
	})

	t.Run("Invalid JSON", func(t *testing.T) {
		entry := Entry{
			Tier:       TierUser,
			Kind:       KindTheme,
			SourcePath: "/test/invalid.json",
			Raw:        []byte(`{invalid json`),
		}
		err := registry.Register(entry)
		assert.Error(t, err)
	})
}

// TestBuiltinThemes tests the built-in themes.
func TestBuiltinThemes(t *testing.T) {
	themes := BuiltinThemes()
	assert.Equal(t, 2, len(themes))

	names := make(map[string]bool)
	for _, entry := range themes {
		names[entry.SourcePath] = true
		assert.Equal(t, TierBuiltin, entry.Tier)
		assert.Equal(t, KindTheme, entry.Kind)
		assert.NotEmpty(t, entry.Raw)
	}

	assert.True(t, names["builtin:dark"])
	assert.True(t, names["builtin:light"])
}

// TestDiscoveryLooseSkillDirRules tests that loose-skill discovery covers all
// four cases per the spec:
//   - Root-level .md files are discovered.
//   - Nested .md files are NOT discovered.
//   - Subdirs with SKILL.md at the root are discovered as directory skills.
//   - Subdirs without SKILL.md are skipped (invalid).
func TestDiscoveryLooseSkillDirRules(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a project dir so .tau/ discovery fires.
	projectDir := filepath.Join(tmpDir, "project")
	err := os.MkdirAll(projectDir, 0o755)
	require.NoError(t, err)

	// Create a skills dir at the project-local location (.tau/skills/).
	skillsDir := filepath.Join(projectDir, ".tau", "skills")

	// Case 1: Root-level .md file — must be discovered.
	err = os.MkdirAll(skillsDir, 0o755)
	require.NoError(t, err)
	rootSkill := `---
name: root-skill
description: A root-level skill
---
# Root Skill`
	err = os.WriteFile(filepath.Join(skillsDir, "root-skill.md"), []byte(rootSkill), 0o644)
	require.NoError(t, err)

	// Case 2: Nested .md file — must NOT be discovered.
	err = os.MkdirAll(filepath.Join(skillsDir, "subdir"), 0o755)
	require.NoError(t, err)
	nestedSkill := `# Nested Skill
This should not be discovered.`
	err = os.WriteFile(filepath.Join(skillsDir, "subdir", "nested-skill.md"), []byte(nestedSkill), 0o644)
	require.NoError(t, err)

	// Case 3: Subdir with SKILL.md at root — valid directory skill, must be discovered.
	err = os.MkdirAll(filepath.Join(skillsDir, "valid-dir-skill"), 0o755)
	require.NoError(t, err)
	validSkill := `---
name: valid-dir-skill
description: A valid directory skill
---
# Valid Dir Skill`
	err = os.WriteFile(filepath.Join(skillsDir, "valid-dir-skill", "SKILL.md"), []byte(validSkill), 0o644)
	require.NoError(t, err)

	// Case 4: Subdir without SKILL.md — invalid, must NOT be discovered.
	err = os.MkdirAll(filepath.Join(skillsDir, "invalid-subdir"), 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(skillsDir, "invalid-subdir", "some-file.md"),
		[]byte("# Not a skill"), 0o644)
	require.NoError(t, err)

	cfg := DiscoveryConfig{
		HomeDir:        tmpDir,
		CWD:            projectDir,
		DisablePrompts: true,
		DisableThemes:  true,
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	var foundSkills []string
	for _, e := range entries {
		if e.Kind == KindSkill && e.Tier == TierProject {
			foundSkills = append(foundSkills, e.SourcePath)
		}
	}

	// Should find root-skill.md and valid-dir-skill/SKILL.md, exactly.
	require.Len(t, foundSkills, 2, "should find exactly 2 skills: %v", foundSkills)

	// Sort for deterministic assertion.
	slices.Sort(foundSkills)

	assert.True(t, strings.HasSuffix(foundSkills[0], "root-skill.md"),
		"first should be root-skill.md, got: %s", foundSkills[0])
	assert.True(t, strings.HasSuffix(foundSkills[1], "SKILL.md"),
		"second should be valid-dir-skill/SKILL.md, got: %s", foundSkills[1])

	// Ensure the invalid subdir and nested .md are not present.
	for _, p := range foundSkills {
		assert.NotContains(t, p, "subdir/nested-skill.md",
			"nested .md should not be found")
		assert.NotContains(t, p, "invalid-subdir",
			"subdir without SKILL.md should not be found")
	}
}

// TestDiscoveryWithinTierAlphabeticalPrecedence tests that within a tier,
// entries are sorted alphabetically by source path and the first wins on collision.
func TestDiscoveryWithinTierAlphabeticalPrecedence(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a project dir so .tau/ discovery fires.
	projectDir := filepath.Join(tmpDir, "project")
	err := os.MkdirAll(projectDir, 0o755)
	require.NoError(t, err)

	// Create a skills dir at the project-local location (.tau/skills/).
	skillsDir := filepath.Join(projectDir, ".tau", "skills")
	err = os.MkdirAll(skillsDir, 0o755)
	require.NoError(t, err)

	// Create two skills with the same ID but different alphabetically-ordered paths.
	// "alpha-skill.md" sorts before "zulu-skill.md".
	alphaSkill := `---
name: my-skill
description: Alpha skill - earlier alphabetically
---
# Alpha Skill`
	err = os.WriteFile(filepath.Join(skillsDir, "alpha-skill.md"), []byte(alphaSkill), 0o644)
	require.NoError(t, err)

	zuluSkill := `---
name: my-skill
description: Zulu skill - later alphabetically
---
# Zulu Skill`
	err = os.WriteFile(filepath.Join(skillsDir, "zulu-skill.md"), []byte(zuluSkill), 0o644)
	require.NoError(t, err)

	cfg := DiscoveryConfig{
		HomeDir:        tmpDir,
		CWD:            projectDir,
		DisablePrompts: true,
		DisableThemes:  true,
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	// Collect project-tier skill entries.
	var skillEntries []Entry
	for _, e := range entries {
		if e.Kind == KindSkill && e.Tier == TierProject {
			skillEntries = append(skillEntries, e)
		}
	}

	// Sort by source path to match the internal ordering.
	slices.SortFunc(skillEntries, func(a, b Entry) int {
		return cmp.Compare(a.SourcePath, b.SourcePath)
	})

	require.Len(t, skillEntries, 2, "should find both skill files: %v", skillEntries)

	// The first (alpha) should win in the registry.
	registry := NewSkillRegistry(nil)
	require.NoError(t, registry.Load(entries))

	skill, ok := registry.Get("my-skill")
	require.True(t, ok, "skill should be registered")

	assert.Contains(t, skill.SourcePath, "alpha-skill.md",
		"earlier alphabetically should win: got %s", skill.SourcePath)
	assert.Contains(t, skill.Description, "Alpha",
		"description should be from alpha skill: got %s", skill.Description)

	// Verify alphabetical order in raw entries.
	assert.True(t, strings.HasSuffix(skillEntries[0].SourcePath, "alpha-skill.md"),
		"first entry should be alpha: %s", skillEntries[0].SourcePath)
	assert.True(t, strings.HasSuffix(skillEntries[1].SourcePath, "zulu-skill.md"),
		"second entry should be zulu: %s", skillEntries[1].SourcePath)
}

// TestDiscoveryFirstWinsDeterministicAcrossFilesystems tests that first-load-wins
// is deterministic by verifying the ordering is stable across multiple Walk calls.
func TestDiscoveryFirstWinsDeterministicAcrossFilesystems(t *testing.T) {
	tmpDir := t.TempDir()

	projectDir := filepath.Join(tmpDir, "project")
	err := os.MkdirAll(projectDir, 0o755)
	require.NoError(t, err)

	skillsDir := filepath.Join(projectDir, ".tau", "skills")
	err = os.MkdirAll(skillsDir, 0o755)
	require.NoError(t, err)

	// Three skills with the same ID, sorted by path: first, second, third.
	contents := map[string]string{
		"aaa-skill.md": `---
name: collision-skill
description: First loaded
---
# First`,
		"mmm-skill.md": `---
name: collision-skill
description: Second loaded
---
# Second`,
		"zzz-skill.md": `---
name: collision-skill
description: Third loaded
---
# Third`,
	}

	for name, content := range contents {
		err = os.WriteFile(filepath.Join(skillsDir, name), []byte(content), 0o644)
		require.NoError(t, err)
	}

	cfg := DiscoveryConfig{
		HomeDir:        tmpDir,
		CWD:            projectDir,
		DisablePrompts: true,
		DisableThemes:  true,
	}

	// Run Walk multiple times to verify determinism.
	var winners []string
	for i := 0; i < 5; i++ {
		entries, err := Walk(cfg)
		require.NoError(t, err)

		registry := NewSkillRegistry(nil)
		require.NoError(t, registry.Load(entries))

		skill, ok := registry.Get("collision-skill")
		require.True(t, ok)

		winners = append(winners, skill.Description)
	}

	// All runs should produce the same winner.
	for i, w := range winners[1:] {
		assert.Equal(t, winners[0], w, "run %d should match run 0 for determinism", i+1)
	}

	// The winner should be from the first alphabetically (aaa-skill.md).
	assert.Equal(t, "First loaded", winners[0],
		"first alphabetically should always win")
}

// TestDiscoveryNoSubdirRecursion tests that prompts/themes only discover
// root-level files (no recursion into subdirs) per the spec.
func TestDiscoveryNoSubdirRecursion(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a project dir so .tau/ discovery fires.
	projectDir := filepath.Join(tmpDir, "project")
	err := os.MkdirAll(projectDir, 0o755)
	require.NoError(t, err)

	// Create a prompts dir at the project-local location (.tau/prompts/).
	promptsDir := filepath.Join(projectDir, ".tau", "prompts")
	err = os.MkdirAll(filepath.Join(promptsDir, "subdir"), 0o755)
	require.NoError(t, err)

	promptContent := `---
description: Review code
---
Please review: $@`
	err = os.WriteFile(filepath.Join(promptsDir, "review.md"), []byte(promptContent), 0o644)
	require.NoError(t, err)
	// File in a subdir should NOT be discovered.
	err = os.WriteFile(filepath.Join(promptsDir, "subdir", "nested-prompt.md"),
		[]byte("Nested prompt: $@"), 0o644)
	require.NoError(t, err)

	// Create a themes dir at the project-local location (.tau/themes/).
	themesDir := filepath.Join(projectDir, ".tau", "themes")
	err = os.MkdirAll(filepath.Join(themesDir, "subdir"), 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(themesDir, "test-dark.json"),
		[]byte(`{"name": "test-dark"}`), 0o644)
	require.NoError(t, err)
	// File in a subdir should NOT be discovered.
	err = os.WriteFile(filepath.Join(themesDir, "subdir", "nested-theme.json"),
		[]byte(`{"name": "nested-theme"}`), 0o644)
	require.NoError(t, err)

	cfg := DiscoveryConfig{
		HomeDir:       tmpDir,
		CWD:           projectDir,
		DisableThemes: true, // Exclude built-in themes to isolate this test.
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	// Count only project-level prompts from .tau/prompts/.
	var foundPrompts int
	var promptPaths []string
	for _, e := range entries {
		if e.Kind == KindPrompt && e.Tier == TierProject {
			foundPrompts++
			promptPaths = append(promptPaths, e.SourcePath)
		}
	}

	// Should find exactly one prompt (root-level only).
	assert.Equal(t, 1, foundPrompts, "should only find root-level prompts, not subdir files: %v", promptPaths)
	assert.True(t, strings.HasSuffix(promptPaths[0], "review.md"), "should find review.md, not nested-prompt.md")

	// Also test themes: check that subdir files are not found.
	cfg2 := DiscoveryConfig{
		HomeDir:        tmpDir,
		CWD:            projectDir,
		DisablePrompts: true,
	}

	entries2, err := Walk(cfg2)
	require.NoError(t, err)

	var foundThemes int
	var themePaths []string
	for _, e := range entries2 {
		if e.Kind == KindTheme && e.Tier == TierProject {
			foundThemes++
			themePaths = append(themePaths, e.SourcePath)
		}
	}

	// Should find exactly one theme (root-level only).
	assert.Equal(t, 1, foundThemes, "should only find root-level themes, not subdir files: %v", themePaths)
	assert.True(t, strings.HasSuffix(themePaths[0], "test-dark.json"), "should find test-dark.json, not nested-theme.json")
}

// TestDiscoveryWalk tests the discovery walker with fixtures.
func TestDiscoveryWalk(t *testing.T) {
	// Create temp directory structure
	tmpDir := t.TempDir()

	// Create skills dir with a skill
	skillsDir := filepath.Join(tmpDir, "skills")
	err := os.MkdirAll(skillsDir, 0o755)
	require.NoError(t, err)

	skillContent := `---
name: test-skill
description: A test skill
---
# Test Skill
This is a test skill.`
	err = os.WriteFile(filepath.Join(skillsDir, "test-skill.md"), []byte(skillContent), 0o644)
	require.NoError(t, err)

	// Create prompts dir
	promptsDir := filepath.Join(tmpDir, "prompts")
	err = os.MkdirAll(promptsDir, 0o755)
	require.NoError(t, err)

	promptContent := `---
description: Review code changes
argument-hint: "<files>"
---
Please review the following files: $@`
	err = os.WriteFile(filepath.Join(promptsDir, "review.md"), []byte(promptContent), 0o644)
	require.NoError(t, err)

	// Create themes dir
	themesDir := filepath.Join(tmpDir, "themes")
	err = os.MkdirAll(themesDir, 0o755)
	require.NoError(t, err)

	themeContent := `{"name": "test-dark", "colors": {"bg": "#000", "fg": "#fff"}}`
	err = os.WriteFile(filepath.Join(themesDir, "test-dark.json"), []byte(themeContent), 0o644)
	require.NoError(t, err)

	// Walk with config pointing to our temp dir
	cfg := DiscoveryConfig{
		HomeDir: tmpDir,
		CWD:     tmpDir,
		SettingsPaths: map[ArtifactKind][]string{
			KindSkill:  {skillsDir},
			KindPrompt: {promptsDir},
			KindTheme:  {themesDir},
		},
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	// Should have our fixtures
	hasSkill := false
	hasPrompt := false
	hasTheme := false

	for _, e := range entries {
		switch e.Kind {
		case KindSkill:
			if strings.Contains(e.SourcePath, "test-skill") {
				hasSkill = true
			}
		case KindPrompt:
			if strings.Contains(e.SourcePath, "review") {
				hasPrompt = true
			}
		case KindTheme:
			if strings.Contains(e.SourcePath, "test-dark") {
				hasTheme = true
			}
		}
	}

	assert.True(t, hasSkill, "should discover skill")
	assert.True(t, hasPrompt, "should discover prompt")
	assert.True(t, hasTheme, "should discover theme")
}

// TestExpandPath tests path expansion.
func TestExpandPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	t.Run("Expand tilde", func(t *testing.T) {
		path := expandPath("~/test")
		assert.True(t, strings.HasPrefix(path, home))
		assert.True(t, strings.HasSuffix(path, "/test"))
	})

	t.Run("Expand env var", func(t *testing.T) {
		t.Setenv("TEST_VAR", "test-value")

		path := expandPath("$TEST_VAR/test")
		assert.Contains(t, path, "test-value")
	})

	t.Run("No expansion needed", func(t *testing.T) {
		path := expandPath("/absolute/path")
		assert.Equal(t, "/absolute/path", path)
	})
}

// TestParseFrontmatter tests frontmatter parsing.
func TestParseSimpleYAMLFoldedDescription(t *testing.T) {
	fm, err := parseSimpleYAML([]byte("name: ponytail\ndescription: >\n  Forces the laziest solution\n  that actually works\n"))
	require.NoError(t, err)
	assert.Equal(t, "Forces the laziest solution that actually works", fm["description"])
}

func TestParseFrontmatter(t *testing.T) {
	t.Run("With frontmatter", func(t *testing.T) {
		data := []byte(`---
name: test
description: A test
---
# Body content`)
		yaml, body, err := parseFrontmatter(data)
		require.NoError(t, err)
		assert.Equal(t, "name: test\ndescription: A test", strings.TrimSpace(string(yaml)))
		assert.Contains(t, string(body), "Body content")
	})

	t.Run("Without frontmatter", func(t *testing.T) {
		data := []byte(`# Just body content`)
		yaml, body, err := parseFrontmatter(data)
		require.NoError(t, err)
		assert.Nil(t, yaml)
		assert.Contains(t, string(body), "Just body content")
	})

	t.Run("With CRLF", func(t *testing.T) {
		data := []byte("---\r\nname: test\r\n---\r\n\r\nBody")
		yaml, _, err := parseFrontmatter(data)
		require.NoError(t, err)
		assert.Contains(t, string(yaml), "name: test")
	})
}

func TestParsePromptDescriptionFallbackAndTruncation(t *testing.T) {
	longLine := "This first non-empty prompt body line is intentionally longer than sixty characters for parity."
	prompt, err := parsePrompt(Entry{
		Kind:       KindPrompt,
		SourcePath: "/tmp/review.md",
		Raw:        []byte("\n\n" + longLine + "\nbody"),
	})
	require.NoError(t, err)
	require.Len(t, []rune(prompt.Description), 60)
	assert.Equal(t, string([]rune(longLine)[:60]), prompt.Description)

	prompt, err = parsePrompt(Entry{
		Kind:       KindPrompt,
		SourcePath: "/tmp/review.md",
		Raw: []byte(`---
description: Frontmatter description that should win over body text even when body starts differently
---
Body line`),
	})
	require.NoError(t, err)
	assert.Equal(t, string([]rune("Frontmatter description that should win over body text even when body starts differently")[:60]), prompt.Description)
}

// TestSortEntries tests alphabetical sorting of entries.
func TestSortEntries(t *testing.T) {
	entries := []Entry{
		{Tier: TierUser, Kind: KindTheme, SourcePath: "/path/z-theme.json", Raw: []byte(`{"name": "z-theme"}`)},
		{Tier: TierUser, Kind: KindTheme, SourcePath: "/path/a-theme.json", Raw: []byte(`{"name": "a-theme"}`)},
		{Tier: TierUser, Kind: KindTheme, SourcePath: "/path/m-theme.json", Raw: []byte(`{"name": "m-theme"}`)},
	}
	sortEntries(entries)

	assert.Equal(t, "/path/a-theme.json", entries[0].SourcePath)
	assert.Equal(t, "/path/m-theme.json", entries[1].SourcePath)
	assert.Equal(t, "/path/z-theme.json", entries[2].SourcePath)
}

// TestSourceTierPrecedence locks the invariant that TierCLI has the highest
// precedence and TierBuiltin has the lowest. Reordering the iota block above
// should break this test, forcing the author to also update the precedence
// helpers and consumers.
func TestSourceTierPrecedence(t *testing.T) {
	if TierCLI.Precedence() <= TierBuiltin.Precedence() {
		t.Errorf("TierCLI.Precedence() (%d) must be greater than TierBuiltin.Precedence() (%d)",
			TierCLI.Precedence(), TierBuiltin.Precedence())
	}

	// Project, user, settings, packages should all be above builtin.
	for _, tier := range []SourceTier{TierProject, TierUser, TierSettings, TierPackages} {
		if tier.Precedence() <= TierBuiltin.Precedence() {
			t.Errorf("%s.Precedence() (%d) must be greater than TierBuiltin.Precedence() (%d)",
				tier, tier.Precedence(), TierBuiltin.Precedence())
		}
	}

	// CLI > Project > User > Settings > Packages > Builtin.
	want := []SourceTier{TierCLI, TierProject, TierUser, TierSettings, TierPackages, TierBuiltin}
	for i := 1; i < len(want); i++ {
		if want[i-1].Precedence() <= want[i].Precedence() {
			t.Errorf("%s.Precedence() (%d) must be greater than %s.Precedence() (%d)",
				want[i-1], want[i-1].Precedence(), want[i], want[i].Precedence())
		}
	}
}

// TestSortEntriesByTierCLIWinsOverBuiltin is a regression test for the bug
// where TierBuiltin shadowed higher-precedence tiers under first-wins
// collision handling. After sortEntriesByTier, the higher-precedence entry
// must appear first so that registries register it (and skip the lower-tier
// duplicate) instead of the reverse.
func TestSortEntriesByTierCLIWinsOverBuiltin(t *testing.T) {
	entries := []Entry{
		{Tier: TierBuiltin, Kind: KindTheme, SourcePath: "builtin:dark", Raw: []byte(`{"name": "dark"}`)},
		{Tier: TierCLI, Kind: KindTheme, SourcePath: "/cli/dark.json", Raw: []byte(`{"name": "dark"}`)},
		{Tier: TierProject, Kind: KindTheme, SourcePath: "/project/dark.json", Raw: []byte(`{"name": "dark"}`)},
		{Tier: TierUser, Kind: KindTheme, SourcePath: "/user/dark.json", Raw: []byte(`{"name": "dark"}`)},
	}

	sorted := sortEntriesByTier(entries)

	if len(sorted) != 4 {
		t.Fatalf("sortEntriesByTier returned %d entries, want 4", len(sorted))
	}

	// TierCLI must come first; TierBuiltin must come last.
	if sorted[0].Tier != TierCLI {
		t.Errorf("sorted[0].Tier = %s, want %s (CLI must win over higher-precedence tiers)",
			sorted[0].Tier, TierCLI)
	}
	if sorted[3].Tier != TierBuiltin {
		t.Errorf("sorted[3].Tier = %s, want %s (builtin must lose to all higher tiers)",
			sorted[3].Tier, TierBuiltin)
	}

	// The full order must be CLI > Project > User > Builtin.
	wantTiers := []SourceTier{TierCLI, TierProject, TierUser, TierBuiltin}
	for i, want := range wantTiers {
		if sorted[i].Tier != want {
			t.Errorf("sorted[%d].Tier = %s, want %s", i, sorted[i].Tier, want)
		}
	}

	// Drive the entries through a real first-wins registry to verify the bug
	// is fixed end-to-end. If builtin shadowed project (the previous bug),
	// the project entry would never get registered.
	registry := NewThemeRegistry(nil)
	for _, e := range sorted {
		_ = registry.Register(e)
	}

	got, ok := registry.Get("dark")
	if !ok {
		t.Fatal("registry.Get(dark) missing after registering all tiers")
	}
	if got.Tier != TierCLI {
		t.Errorf("registry.Get(dark).Tier = %s, want %s (CLI should win on collision)",
			got.Tier, TierCLI)
	}
	if got.SourcePath != "/cli/dark.json" {
		t.Errorf("registry.Get(dark).SourcePath = %s, want /cli/dark.json", got.SourcePath)
	}
}

// TestSortEntriesByTierAlphabeticalWithinTier verifies that entries sharing a
// tier are sorted alphabetically by source path before tier ordering is
// applied, so first-wins collision handling remains deterministic.
func TestSortEntriesByTierAlphabeticalWithinTier(t *testing.T) {
	entries := []Entry{
		{Tier: TierUser, Kind: KindTheme, SourcePath: "/path/z-theme.json", Raw: []byte(`{"name": "theme"}`)},
		{Tier: TierCLI, Kind: KindTheme, SourcePath: "/path/a-theme.json", Raw: []byte(`{"name": "theme"}`)},
		{Tier: TierUser, Kind: KindTheme, SourcePath: "/path/a-theme.json", Raw: []byte(`{"name": "theme"}`)},
		{Tier: TierBuiltin, Kind: KindTheme, SourcePath: "builtin:theme", Raw: []byte(`{"name": "theme"}`)},
	}

	sorted := sortEntriesByTier(entries)

	// CLI > User (a before z) > Builtin.
	wantTiers := []SourceTier{TierCLI, TierUser, TierUser, TierBuiltin}
	for i, want := range wantTiers {
		if sorted[i].Tier != want {
			t.Errorf("sorted[%d].Tier = %s, want %s", i, sorted[i].Tier, want)
		}
	}
	// Within user tier: a-theme before z-theme.
	if sorted[1].SourcePath != "/path/a-theme.json" {
		t.Errorf("sorted[1].SourcePath = %s, want /path/a-theme.json", sorted[1].SourcePath)
	}
	if sorted[2].SourcePath != "/path/z-theme.json" {
		t.Errorf("sorted[2].SourcePath = %s, want /path/z-theme.json", sorted[2].SourcePath)
	}
}

// TestSymlinkFollowingAndCycleDetection tests that symlinks are followed per spec
// without infinite recursion on cycles.
func TestSymlinkFollowingAndCycleDetection(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a skills dir at the project-local location (.tau/skills/).
	projectDir := filepath.Join(tmpDir, "project")
	err := os.MkdirAll(projectDir, 0o755)
	require.NoError(t, err)

	skillsDir := filepath.Join(projectDir, ".tau", "skills")
	err = os.MkdirAll(skillsDir, 0o755)
	require.NoError(t, err)

	// Create a skill in the real skills directory.
	err = os.WriteFile(filepath.Join(skillsDir, "real-skill.md"),
		[]byte("---\nname: real-skill\ndescription: A real skill\n---\n# Real Skill"), 0o644)
	require.NoError(t, err)

	// Create a regular symlink to a directory with a skill.
	linkedSkillsDir := filepath.Join(tmpDir, "linked-skills")
	err = os.MkdirAll(linkedSkillsDir, 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(linkedSkillsDir, "linked-skill.md"),
		[]byte("---\nname: linked-skill\ndescription: A linked skill\n---\n# Linked Skill"), 0o644)
	require.NoError(t, err)

	// Create a symlink inside skills/ pointing to the linked-skills directory.
	linkToLinked := filepath.Join(skillsDir, "link-to-linked")
	err = os.Symlink(linkedSkillsDir, linkToLinked)
	require.NoError(t, err)

	// Verify symlink was created.
	info, err := os.Lstat(linkToLinked)
	require.NoError(t, err)
	assert.True(t, info.Mode()&os.ModeSymlink != 0, "should be a symlink")

	// Verify target is readable via symlink.
	entries, err := os.ReadDir(linkToLinked)
	require.NoError(t, err, "should be able to read through symlink")
	assert.Len(t, entries, 1, "should have one entry through symlink")

	// Call discoverDir directly to test the logic.
	t.Log("Calling discoverDirForTest...")
	discovered, err := discoverDirForTest(skillsDir)
	require.NoError(t, err)

	t.Logf("Direct discoverDir found %d entries:", len(discovered))
	for _, p := range discovered {
		t.Logf("  %s", p)
	}

	// Also call discoverDir directly from the package via a wrapper.
	// This bypasses any issues in Walk itself.
	directEntries, err := discoverDirDirect(skillsDir, KindSkill, TierProject, false)
	require.NoError(t, err)

	t.Logf("Package discoverDir returned %d entries:", len(directEntries))
	for _, e := range directEntries {
		t.Logf("  Tier=%v, Kind=%v, Path=%s", e.Tier, e.Kind, e.SourcePath)
	}

	// Also test the actual discoverDir from the package via a different approach.
	// Create a CLI path that points to the linked-skills dir and verify it gets discovered.
	linkedCfg := DiscoveryConfig{
		HomeDir: tmpDir,
		CWD:     projectDir,
		CLIPaths: map[ArtifactKind][]string{
			KindSkill: {linkedSkillsDir},
		},
	}
	linkedEntries, err := Walk(linkedCfg)
	require.NoError(t, err)

	t.Logf("CLI-path Walk found %d entries:", len(linkedEntries))
	for _, e := range linkedEntries {
		t.Logf("  Tier=%v, Kind=%v, Path=%s", e.Tier, e.Kind, e.SourcePath)
	}

	cfg := DiscoveryConfig{
		HomeDir: tmpDir,
		CWD:     projectDir,
	}

	// Walk should complete without hanging or erroring on the cycle.
	walkEntries, err := Walk(cfg)
	require.NoError(t, err)

	t.Logf("Walk found %d total entries:", len(walkEntries))
	for _, e := range walkEntries {
		t.Logf("  Tier=%v, Kind=%v, Path=%s", e.Tier, e.Kind, e.SourcePath)
	}

	// Should find real-skill.md and linked-skill.md (via the symlink).
	var foundPaths []string
	for _, e := range walkEntries {
		if e.Kind == KindSkill && e.Tier == TierProject {
			foundPaths = append(foundPaths, e.SourcePath)
		}
	}

	t.Logf("Project-tier skills: %v", foundPaths)

	// Verify real-skill is present.
	hasReal := false
	hasLinked := false
	for _, p := range foundPaths {
		if strings.Contains(p, "real-skill") {
			hasReal = true
		}
		if strings.Contains(p, "linked-skill") {
			hasLinked = true
		}
	}
	assert.True(t, hasReal, "should find real-skill: %v", foundPaths)
	assert.True(t, hasLinked, "should find linked-skill via symlink: %v", foundPaths)

	// Verify no duplicates from cycle.
	seen := make(map[string]bool)
	for _, p := range foundPaths {
		assert.False(t, seen[p], "should not have duplicate entries: %s", p)
		seen[p] = true
	}
}

// discoverDirForTest exposes discoverDir for testing purposes.
func discoverDirForTest(dir string) ([]string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	entries := make([]string, 0)
	visited := make(map[string]bool)
	queue := []string{dir}

	for len(queue) > 0 {
		currentDir := queue[0]
		queue = queue[1:]

		realPath, err := filepath.EvalSymlinks(currentDir)
		if err != nil {
			realPath = currentDir
		}

		if visited[realPath] {
			continue
		}
		visited[realPath] = true

		files, err := os.ReadDir(currentDir)
		if err != nil {
			continue
		}

		for _, entry := range files {
			path := filepath.Join(currentDir, entry.Name())

			if entry.Type()&os.ModeSymlink != 0 {
				symTarget, err := filepath.EvalSymlinks(path)
				if err != nil {
					continue
				}
				if visited[symTarget] {
					continue
				}

				symInfo, err := os.Stat(path)
				if err != nil {
					continue
				}

				if symInfo.IsDir() {
					queue = append(queue, path)
				}
				continue
			}

			if entry.IsDir() {
				continue
			}

			if entry.Type().IsRegular() {
				ext := strings.ToLower(filepath.Ext(path))
				if ext == ".md" && filepath.Base(path) != "SKILL.md" {
					entries = append(entries, path)
				}
			}
		}
	}

	return entries, nil
}

// discoverDirDirect wraps the package-level discoverDir function for testing.
func discoverDirDirect(dir string, kind ArtifactKind, tier SourceTier, skillDirMode bool) ([]Entry, error) {
	return discoverDir(dir, kind, tier, skillDirMode, dir)
}

// TestSymlinkFollowing tests that files are discovered through symlinks to directories.
func TestSymlinkFollowing(t *testing.T) {
	tmpDir := t.TempDir()

	projectDir := filepath.Join(tmpDir, "project")
	err := os.MkdirAll(projectDir, 0o755)
	require.NoError(t, err)

	skillsDir := filepath.Join(projectDir, ".tau", "skills")
	err = os.MkdirAll(skillsDir, 0o755)
	require.NoError(t, err)

	// Create a real skill.
	err = os.WriteFile(filepath.Join(skillsDir, "real-skill.md"),
		[]byte("---\nname: real-skill\ndescription: A real skill\n---\n# Real"), 0o644)
	require.NoError(t, err)

	// Create a linked skills directory (outside project tree).
	linkedSkillsDir := filepath.Join(tmpDir, "linked-skills")
	err = os.MkdirAll(linkedSkillsDir, 0o755)
	require.NoError(t, err)

	// Create a skill in the linked directory.
	err = os.WriteFile(filepath.Join(linkedSkillsDir, "linked-skill.md"),
		[]byte("---\nname: linked-skill\ndescription: A linked skill\n---\n# Linked"), 0o644)
	require.NoError(t, err)

	// Create a symlink in the skills directory pointing to the linked directory.
	linkPath := filepath.Join(skillsDir, "link-to-linked")
	err = os.Symlink(linkedSkillsDir, linkPath)
	require.NoError(t, err)

	cfg := DiscoveryConfig{
		HomeDir: tmpDir,
		CWD:     projectDir,
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	// Collect all skill paths found.
	var foundPaths []string
	for _, e := range entries {
		if e.Kind == KindSkill && e.Tier == TierProject {
			foundPaths = append(foundPaths, e.SourcePath)
		}
	}

	// Should find both the real skill and the linked skill through symlink.
	assert.Len(t, foundPaths, 2, "should find 2 skills (real + through symlink): %v", foundPaths)

	// Verify we found both specific files.
	hasReal := false
	hasLinked := false
	for _, p := range foundPaths {
		if strings.Contains(p, "real-skill.md") {
			hasReal = true
		}
		if strings.Contains(p, "linked-skill.md") {
			hasLinked = true
		}
	}
	assert.True(t, hasReal, "should find real-skill.md")
	assert.True(t, hasLinked, "should find linked-skill.md through symlink")
}

// TestSymlinkCycleDetection tests that symlink cycles are detected and don't cause infinite loops.
func TestSymlinkCycleDetection(t *testing.T) {
	tmpDir := t.TempDir()

	projectDir := filepath.Join(tmpDir, "project")
	err := os.MkdirAll(projectDir, 0o755)
	require.NoError(t, err)

	skillsDir := filepath.Join(projectDir, ".tau", "skills")
	err = os.MkdirAll(skillsDir, 0o755)
	require.NoError(t, err)

	// Create a skill.
	err = os.WriteFile(filepath.Join(skillsDir, "skill.md"),
		[]byte("---\nname: skill\ndescription: A skill\n---\n# Skill"), 0o644)
	require.NoError(t, err)

	// Create a self-referencing symlink (cycle).
	cycleLink := filepath.Join(skillsDir, "cycle-link")
	err = os.Symlink(skillsDir, cycleLink)
	require.NoError(t, err)

	cfg := DiscoveryConfig{
		HomeDir: tmpDir,
		CWD:     projectDir,
	}

	// Walk should complete without hanging.
	entries, err := Walk(cfg)
	require.NoError(t, err)

	// Should find the skill, but not duplicate it via the cycle.
	var foundPaths []string
	for _, e := range entries {
		if e.Kind == KindSkill && e.Tier == TierProject {
			foundPaths = append(foundPaths, e.SourcePath)
		}
	}

	// Should find exactly one skill.
	assert.Len(t, foundPaths, 1, "should find exactly 1 skill (cycle should not cause duplicates): %v", foundPaths)
	assert.True(t, strings.Contains(foundPaths[0], "skill.md"), "should find skill.md")
}

// TestWithinTier tests that within-tier ordering is alphabetical.
func TestWithinTier(t *testing.T) {
	// Create temp directory with multiple skills in user-global location
	tmpDir := t.TempDir()
	userSkillsDir := filepath.Join(tmpDir, ".tau", "agent", "skills")
	err := os.MkdirAll(userSkillsDir, 0o755)
	require.NoError(t, err)

	// Create skills in non-alphabetical order
	skills := []string{"zebra", "apple", "mango"}
	for _, name := range skills {
		content := fmt.Sprintf(`---
name: %s-skill
---
# %s skill`, name, name)
		err = os.WriteFile(filepath.Join(userSkillsDir, name+".md"), []byte(content), 0o644)
		require.NoError(t, err)
	}

	// Also create a non-existent project dir
	projectDir := filepath.Join(tmpDir, "project")
	err = os.MkdirAll(projectDir, 0o755)
	require.NoError(t, err)

	cfg := DiscoveryConfig{
		HomeDir: tmpDir,
		CWD:     projectDir,
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	// Filter to just skills
	var skillEntries []Entry
	for _, e := range entries {
		if e.Kind == KindSkill {
			skillEntries = append(skillEntries, e)
		}
	}

	// Should be sorted alphabetically by source path
	require.GreaterOrEqual(t, len(skillEntries), 3, "should find at least 3 skills")
	paths := make([]string, 0, len(skillEntries))
	for _, e := range skillEntries {
		paths = append(paths, e.SourcePath)
	}

	// Check alphabetical ordering
	sorted := make([]string, len(paths))
	copy(sorted, paths)
	slices.Sort(sorted)
	assert.Equal(t, sorted, paths, "entries should be sorted alphabetically by source path")
}

// TestSymlinkRealPathDuplicateSuppression tests that entries resolving to
// the same real path via symlinks are deduplicated, keeping only the first.
// This matches TS behavior where realpathSync is used to detect duplicates.
func TestSymlinkRealPathDuplicateSuppression(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project")
	err := os.MkdirAll(projectDir, 0o755)
	require.NoError(t, err)

	skillsDir := filepath.Join(projectDir, ".tau", "skills")
	err = os.MkdirAll(skillsDir, 0o755)
	require.NoError(t, err)

	// Create an actual skills directory with a skill.
	actualSkillsDir := filepath.Join(tmpDir, "actual-skills")
	err = os.MkdirAll(actualSkillsDir, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(actualSkillsDir, "shared-skill.md"),
		[]byte("---\nname: shared-skill\ndescription: A skill accessible via symlink\n---\n# Shared Skill"), 0o644)
	require.NoError(t, err)

	// Create a symlink from the skills directory to the actual skills directory.
	linkPath := filepath.Join(skillsDir, "link-to-actual")
	err = os.Symlink(actualSkillsDir, linkPath)
	require.NoError(t, err)

	// Verify the symlink target and the original point to the same real path.
	realViaSymlink, err := filepath.EvalSymlinks(filepath.Join(skillsDir, "link-to-actual", "shared-skill.md"))
	require.NoError(t, err)
	realDirect, err := filepath.EvalSymlinks(filepath.Join(actualSkillsDir, "shared-skill.md"))
	require.NoError(t, err)
	assert.Equal(t, realDirect, realViaSymlink, "symlink and direct path should resolve to same real path")

	cfg := DiscoveryConfig{
		HomeDir: tmpDir,
		CWD:     projectDir,
		CLIPaths: map[ArtifactKind][]string{
			KindSkill: {filepath.Join(actualSkillsDir, "shared-skill.md")},
		},
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	// Collect all skill entries with their real paths.
	var skillEntries []Entry
	for _, e := range entries {
		if e.Kind == KindSkill {
			skillEntries = append(skillEntries, e)
		}
	}

	// Track real paths to detect duplicates.
	realPathCount := make(map[string]int)
	for _, e := range skillEntries {
		key := e.RealPath
		if key == "" {
			key = e.SourcePath
		}
		realPathCount[key]++
	}

	// Each unique real path should appear only once.
	for realPath, count := range realPathCount {
		assert.Equal(t, 1, count, "real path %q should appear exactly once, got %d", realPath, count)
	}

	// Verify that we found the skill (not that we found duplicates).
	require.NotEmpty(t, skillEntries, "should find at least one skill")
}

// TestDiscoverDirSymlinkedUserSkill covers the case that broke the manual
// pi-extension install: a symlink at ~/.tau/agent/skills/<name> pointing to
// the extension package's skills/<name> directory, with the symlink and
// the target sharing the same basename. Pre-fix, the eager
// `visited[symTarget] = true` on the first encounter of the symlink
// caused the queued symlink path to be skipped as a "cycle" on its own
// first traversal, so the skill was never discovered.
func TestDiscoverDirSymlinkedUserSkill(t *testing.T) {
	tmpDir := t.TempDir()

	// Real skill package dir with SKILL.md.
	realDir := filepath.Join(tmpDir, "pkg", "example")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "SKILL.md"),
		[]byte("---\nname: example\ndescription: Generic fixture skill.\n---\n# Body\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// User skills dir with a symlink of the same basename as the target.
	skillsDir := filepath.Join(tmpDir, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(skillsDir, "example")); err != nil {
		t.Fatal(err)
	}

	entries, err := discoverDir(skillsDir, KindSkill, TierUser, false, skillsDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "expected 1 skill entry from symlinked dir, got %d: %+v", len(entries), entries)
	assert.Equal(t, KindSkill, entries[0].Kind)
}

// TestDiscoverDirDuplicateSymlinksViaUserHome covers two symlinks in
// ~/.tau/agent/skills pointing at the same target dir (manual installs
// sometimes alias the same package under multiple names). The skill must
// be reported exactly once.
func TestDiscoverDirDuplicateSymlinksViaUserHome(t *testing.T) {
	tmpDir := t.TempDir()

	realDir := filepath.Join(tmpDir, "pkg", "example")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "SKILL.md"),
		[]byte("---\nname: example\ndescription: Generic fixture skill.\n---\n# Body\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	skillsDir := filepath.Join(tmpDir, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(skillsDir, "example")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(skillsDir, "example-copy")); err != nil {
		t.Fatal(err)
	}

	entries, err := discoverDir(skillsDir, KindSkill, TierUser, false, skillsDir)
	require.NoError(t, err)

	count := 0
	for _, e := range entries {
		if e.Kind == KindSkill {
			count++
		}
	}
	assert.Equal(t, 1, count, "expected exactly 1 skill entry via duplicate symlinks, got %d", count)
}

// TestDedupByRealPathFunction tests the deduplicateByRealPath function directly.
func TestDedupByRealPathFunction(t *testing.T) {
	t.Run("no duplicates", func(t *testing.T) {
		entries := []Entry{
			{SourcePath: "/a/skill1.md", RealPath: "/real1.md"},
			{SourcePath: "/a/skill2.md", RealPath: "/real2.md"},
			{SourcePath: "/a/skill3.md", RealPath: "/real3.md"},
		}
		result := deduplicateByRealPath(entries)
		assert.Len(t, result, 3)
	})

	t.Run("duplicate real paths", func(t *testing.T) {
		entries := []Entry{
			{SourcePath: "/a/skill1.md", RealPath: "/real.md"},  // First wins
			{SourcePath: "/b/skill1.md", RealPath: "/real.md"},  // Duplicate, should be removed
			{SourcePath: "/c/skill2.md", RealPath: "/real2.md"}, // Unique
		}
		result := deduplicateByRealPath(entries)
		assert.Len(t, result, 2, "should remove one duplicate")
		assert.Equal(t, "/a/skill1.md", result[0].SourcePath, "first entry should be kept")
		assert.Equal(t, "/c/skill2.md", result[1].SourcePath, "unique entry should be kept")
	})

	t.Run("empty real path falls back to source path", func(t *testing.T) {
		entries := []Entry{
			{SourcePath: "/a/skill1.md", RealPath: ""}, // Empty RealPath
			{SourcePath: "/a/skill1.md", RealPath: ""}, // Same SourcePath, should deduplicate
		}
		result := deduplicateByRealPath(entries)
		assert.Len(t, result, 1, "should deduplicate entries with empty RealPath using SourcePath")
	})

	t.Run("nil entries", func(t *testing.T) {
		result := deduplicateByRealPath(nil)
		assert.Nil(t, result)
	})

	t.Run("empty slice", func(t *testing.T) {
		result := deduplicateByRealPath([]Entry{})
		assert.Len(t, result, 0)
	})
}

// TestCrossHarnessRepoRootBoundary tests that project .agents/skills discovery
// walks from session CWD up to the git repo root only, and never above it.
func TestCrossHarnessRepoRootBoundary(t *testing.T) {
	// Create an outer dir (no git), a git repo nested inside it, and a subdir
	// inside the repo. Ensure we only find .agents/skills within the repo.
	tmpDir := t.TempDir()

	// Outer dir: no git repo.
	outerDir := filepath.Join(tmpDir, "outer")
	err := os.MkdirAll(outerDir, 0o755)
	require.NoError(t, err)

	// Create .agents/skills in the outer dir; it should NOT be found.
	outerAgentsDir := filepath.Join(outerDir, ".agents", "skills")
	err = os.MkdirAll(outerAgentsDir, 0o755)
	require.NoError(t, err)

	// Outer skill (should NOT be found).
	err = os.MkdirAll(filepath.Join(outerAgentsDir, "outer-skill-dir"), 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(outerAgentsDir, "outer-skill-dir", "SKILL.md"),
		[]byte("---\nname: outer-skill\n---\n# Outer"), 0o644)
	require.NoError(t, err)

	// Inner dir: git repo root.
	repoRoot := filepath.Join(outerDir, "myrepo")
	err = os.MkdirAll(repoRoot, 0o755)
	require.NoError(t, err)

	// Init git repo at repoRoot.
	gitDir := filepath.Join(repoRoot, ".git")
	err = os.MkdirAll(gitDir, 0o755)
	require.NoError(t, err)

	// Create .agents/skills at the repo root (should be found).
	repoAgentsDir := filepath.Join(repoRoot, ".agents", "skills")
	err = os.MkdirAll(repoAgentsDir, 0o755)
	require.NoError(t, err)

	err = os.MkdirAll(filepath.Join(repoAgentsDir, "repo-root-skill"), 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(repoAgentsDir, "repo-root-skill", "SKILL.md"),
		[]byte("---\nname: repo-root-skill\n---\n# Repo Root"), 0o644)
	require.NoError(t, err)

	// Create .agents/skills in a subdir of the repo (should be found).
	subDir := filepath.Join(repoRoot, "subproject")
	err = os.MkdirAll(subDir, 0o755)
	require.NoError(t, err)

	subAgentsDir := filepath.Join(subDir, ".agents", "skills")
	err = os.MkdirAll(subAgentsDir, 0o755)
	require.NoError(t, err)

	err = os.MkdirAll(filepath.Join(subAgentsDir, "sub-skill"), 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(subAgentsDir, "sub-skill", "SKILL.md"),
		[]byte("---\nname: sub-skill\n---\n# Sub"), 0o644)
	require.NoError(t, err)

	// Run discovery from subDir (inside the repo).
	cfg := DiscoveryConfig{
		HomeDir: tmpDir,
		CWD:     subDir,
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	// Collect found skill source paths.
	var foundPaths []string
	for _, e := range entries {
		if e.Kind == KindSkill && e.Tier == TierProject {
			foundPaths = append(foundPaths, e.SourcePath)
		}
	}

	// Should find repo-root-skill and sub-skill, but NOT outer-skill.
	var foundNames []string
	for _, p := range foundPaths {
		name := filepath.Base(filepath.Dir(p))
		foundNames = append(foundNames, name)
	}

	assert.Contains(t, foundNames, "repo-root-skill", "should find skill at repo root")
	assert.Contains(t, foundNames, "sub-skill", "should find skill in subdir")
	assert.NotContains(t, foundNames, "outer-skill", "should NOT find skill above repo root")
}

// TestCrossHarnessNoGitRepo tests discovery when there is no git repo.
// Should check cwd only.
func TestCrossHarnessNoGitRepo(t *testing.T) {
	tmpDir := t.TempDir()

	// Create .agents/skills directly in tmpDir (no git repo).
	agentsDir := filepath.Join(tmpDir, ".agents", "skills")
	err := os.MkdirAll(agentsDir, 0o755)
	require.NoError(t, err)

	err = os.MkdirAll(filepath.Join(agentsDir, "no-git-skill"), 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(agentsDir, "no-git-skill", "SKILL.md"),
		[]byte("---\nname: no-git-skill\n---\n# No Git"), 0o644)
	require.NoError(t, err)

	cfg := DiscoveryConfig{
		HomeDir: tmpDir,
		CWD:     tmpDir,
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	var found bool
	for _, e := range entries {
		if e.Kind == KindSkill && strings.Contains(e.SourcePath, "no-git-skill") {
			found = true
		}
	}

	assert.True(t, found, "should find .agents/skills skill when no git repo exists")
}

// TestSlugify tests slug generation.
func TestSlugify(t *testing.T) {
	assert.Equal(t, "test-skill", slugify("Test Skill"))
	assert.Equal(t, "my-long-skill-name", slugify("My Long Skill Name"))
	assert.Equal(t, "test-123", slugify("Test 123"))
	assert.Equal(t, "a", slugify("A"))
}

// TestValidateName tests name validation per Agent Skills spec.
func TestValidateName(t *testing.T) {
	t.Run("Valid names", func(t *testing.T) {
		// Name matches parent dir
		assert.Empty(t, validateName("test-skill", "test-skill"))
		assert.Empty(t, validateName("a", "a"))
		assert.Empty(t, validateName("my-skill-123", "my-skill-123"))
	})

	t.Run("Name mismatch with parent dir", func(t *testing.T) {
		errors := validateName("test-skill", "different-dir")
		require.Len(t, errors, 1)
		assert.Contains(t, errors[0], "does not match parent directory")
	})

	t.Run("Name too long", func(t *testing.T) {
		longName := strings.Repeat("a", MaxNameLength+1)
		errors := validateName(longName, longName)
		require.Len(t, errors, 1)
		assert.Contains(t, errors[0], "exceeds 64 characters")
	})

	t.Run("Invalid characters", func(t *testing.T) {
		errors := validateName("TestSkill", "TestSkill")
		require.Len(t, errors, 1)
		assert.Contains(t, errors[0], "invalid characters")
	})

	t.Run("Uppercase characters", func(t *testing.T) {
		errors := validateName("Test", "Test")
		require.Len(t, errors, 1)
		assert.Contains(t, errors[0], "invalid characters")
	})

	t.Run("Leading hyphen", func(t *testing.T) {
		errors := validateName("-test", "-test")
		require.Len(t, errors, 1)
		assert.Contains(t, errors[0], "must not start or end with a hyphen")
	})

	t.Run("Trailing hyphen", func(t *testing.T) {
		errors := validateName("test-", "test-")
		require.Len(t, errors, 1)
		assert.Contains(t, errors[0], "must not start or end with a hyphen")
	})

	t.Run("Consecutive hyphens", func(t *testing.T) {
		errors := validateName("test--skill", "test--skill")
		require.Len(t, errors, 1)
		assert.Contains(t, errors[0], "must not contain consecutive hyphens")
	})

	t.Run("Multiple errors", func(t *testing.T) {
		errors := validateName("Test-Skill-", "different")
		assert.Len(t, errors, 3) // mismatch, invalid chars, leading/trailing
	})
}

// TestValidateDescription tests description validation per Agent Skills spec.
func TestValidateDescription(t *testing.T) {
	t.Run("Valid descriptions", func(t *testing.T) {
		assert.Empty(t, validateDescription("A useful skill for testing"))
		assert.Empty(t, validateDescription("Short"))
		assert.Empty(t, validateDescription(strings.Repeat("a", MaxDescriptionLength)))
	})

	t.Run("Empty description", func(t *testing.T) {
		errors := validateDescription("")
		require.Len(t, errors, 1)
		assert.Equal(t, "description is required", errors[0])
	})

	t.Run("Whitespace only description", func(t *testing.T) {
		errors := validateDescription("   ")
		require.Len(t, errors, 1)
		assert.Equal(t, "description is required", errors[0])
	})

	t.Run("Description too long", func(t *testing.T) {
		longDesc := strings.Repeat("a", MaxDescriptionLength+1)
		errors := validateDescription(longDesc)
		require.Len(t, errors, 1)
		assert.Contains(t, errors[0], "exceeds 1024 characters")
	})
}

// TestIsValidSkillName tests the skill name character validator.
func TestIsValidSkillName(t *testing.T) {
	assert.True(t, isValidSkillName("test"))
	assert.True(t, isValidSkillName("test-skill"))
	assert.True(t, isValidSkillName("test-skill-123"))
	assert.False(t, isValidSkillName("Test"))
	assert.False(t, isValidSkillName("test_skill"))
	assert.False(t, isValidSkillName("test skill"))
	assert.False(t, isValidSkillName("test.skill"))
	assert.False(t, isValidSkillName("test!skill"))
	assert.False(t, isValidSkillName(""))
}

// TestSkillRegistry tests the SkillRegistry.
func TestSkillRegistry(t *testing.T) {
	registry := NewSkillRegistry(nil)

	t.Run("Register and Get", func(t *testing.T) {
		entry := Entry{
			Tier:       TierUser,
			Kind:       KindSkill,
			SourcePath: "/test/test-skill.md",
			Raw: []byte(`---
name: test-skill
description: A test skill
tags: [test, unit]
---
# Test Skill`),
		}
		_, err := registry.Register(entry)
		require.NoError(t, err)

		skill, ok := registry.Get("test-skill")
		require.True(t, ok)
		assert.Equal(t, "test-skill", skill.ID)
		assert.Equal(t, "A test skill", skill.Description)
		assert.Equal(t, 2, len(skill.Tags))
	})

	t.Run("Fallback to filename slug", func(t *testing.T) {
		entry := Entry{
			Tier:       TierUser,
			Kind:       KindSkill,
			SourcePath: "/test/my-unnamed-skill.md",
			Raw: []byte(`---
description: A skill with a description but no name
---
# Just body`),
		}
		_, err := registry.Register(entry)
		require.NoError(t, err)

		skill, ok := registry.Get("my-unnamed-skill")
		require.True(t, ok)
		assert.Equal(t, "my-unnamed-skill", skill.ID)
	})
}

// TestDisableFlags tests that --no-* flags skip discovery but explicit CLI paths still load.
func TestDisableFlags(t *testing.T) {
	// Create temp directory structure
	tmpDir := t.TempDir()

	// Create skills dir with a skill
	skillsDir := filepath.Join(tmpDir, "skills")
	err := os.MkdirAll(skillsDir, 0o755)
	require.NoError(t, err)

	skillContent := `---
name: test-skill
description: A test skill
---
# Test Skill`
	err = os.WriteFile(filepath.Join(skillsDir, "test-skill.md"), []byte(skillContent), 0o644)
	require.NoError(t, err)

	// Create prompts dir
	promptsDir := filepath.Join(tmpDir, "prompts")
	err = os.MkdirAll(promptsDir, 0o755)
	require.NoError(t, err)

	promptContent := `---
description: Review code
---
Please review: $@`
	err = os.WriteFile(filepath.Join(promptsDir, "review.md"), []byte(promptContent), 0o644)
	require.NoError(t, err)

	// Create themes dir
	themesDir := filepath.Join(tmpDir, "themes")
	err = os.MkdirAll(themesDir, 0o755)
	require.NoError(t, err)

	themeContent := `{"name": "test-dark"}`
	err = os.WriteFile(filepath.Join(themesDir, "test-dark.json"), []byte(themeContent), 0o644)
	require.NoError(t, err)

	t.Run("DisableSkills skips discovery but CLI paths still load", func(t *testing.T) {
		cfg := DiscoveryConfig{
			HomeDir:       tmpDir,
			CWD:           tmpDir,
			DisableSkills: true,
			CLIPaths: map[ArtifactKind][]string{
				KindSkill: {skillsDir},
			},
		}

		entries, err := Walk(cfg)
		require.NoError(t, err)

		// Should not discover skill from user/project dirs
		for _, e := range entries {
			if e.Kind == KindSkill && e.Tier != TierCLI {
				t.Errorf("expected no non-CLI skill entries, got %s", e.SourcePath)
			}
		}

		// Should still load from CLI path
		hasCLISkill := false
		for _, e := range entries {
			if e.Kind == KindSkill && e.Tier == TierCLI && strings.Contains(e.SourcePath, "test-skill") {
				hasCLISkill = true
			}
		}
		assert.True(t, hasCLISkill, "should still load skill from CLI path")
	})

	t.Run("DisablePrompts skips discovery but CLI paths still load", func(t *testing.T) {
		cfg := DiscoveryConfig{
			HomeDir:        tmpDir,
			CWD:            tmpDir,
			DisablePrompts: true,
			CLIPaths: map[ArtifactKind][]string{
				KindPrompt: {promptsDir},
			},
		}

		entries, err := Walk(cfg)
		require.NoError(t, err)

		// Should not discover prompt from user/project dirs
		for _, e := range entries {
			if e.Kind == KindPrompt && e.Tier != TierCLI {
				t.Errorf("expected no non-CLI prompt entries, got %s", e.SourcePath)
			}
		}

		// Should still load from CLI path
		hasCLIPrompt := false
		for _, e := range entries {
			if e.Kind == KindPrompt && e.Tier == TierCLI && strings.Contains(e.SourcePath, "review") {
				hasCLIPrompt = true
			}
		}
		assert.True(t, hasCLIPrompt, "should still load prompt from CLI path")
	})

	t.Run("DisableThemes skips discovery but CLI paths still load", func(t *testing.T) {
		cfg := DiscoveryConfig{
			HomeDir:       tmpDir,
			CWD:           tmpDir,
			DisableThemes: true,
			CLIPaths: map[ArtifactKind][]string{
				KindTheme: {themesDir},
			},
		}

		entries, err := Walk(cfg)
		require.NoError(t, err)

		// Should not discover theme from user/project dirs
		for _, e := range entries {
			if e.Kind == KindTheme && e.Tier != TierCLI {
				t.Errorf("expected no non-CLI theme entries, got %s", e.SourcePath)
			}
		}

		// Should still load from CLI path
		hasCLITheme := false
		for _, e := range entries {
			if e.Kind == KindTheme && e.Tier == TierCLI && strings.Contains(e.SourcePath, "test-dark") {
				hasCLITheme = true
			}
		}
		assert.True(t, hasCLITheme, "should still load theme from CLI path")
	})
}

// TestPromptRegistryExpand tests bash-style argument substitution.
// Supports $1, $2, $@, $ARGUMENTS, ${@:N}, ${@:N:L} patterns.
func TestPromptRegistryExpand(t *testing.T) {
	registry := NewPromptRegistry(nil)

	entry := Entry{
		Tier:       TierUser,
		Kind:       KindPrompt,
		SourcePath: "/test/review.md",
		Raw: []byte(`---
description: Review code
---
Please review: $@
End.`),
	}
	err := registry.Register(entry)
	require.NoError(t, err)

	t.Run("Expand with $@", func(t *testing.T) {
		expanded, ok := registry.Expand("review", "file1.go file2.go")
		require.True(t, ok)
		assert.Contains(t, expanded, "file1.go file2.go")
		assert.NotContains(t, expanded, "$@")
	})

	t.Run("Expand with positional args $1, $2", func(t *testing.T) {
		registry2 := NewPromptRegistry(nil)
		entry2 := Entry{
			Tier:       TierUser,
			Kind:       KindPrompt,
			SourcePath: "/test/review2.md",
			Raw: []byte(`---
description: Review specific files
---
Please review $1 and $2.`),
		}
		err := registry2.Register(entry2)
		require.NoError(t, err)

		expanded, ok := registry2.Expand("review2", "file1.go \"file with spaces.go\"")
		require.True(t, ok)
		assert.Contains(t, expanded, "file1.go")
		assert.Contains(t, expanded, "file with spaces.go")
		assert.NotContains(t, expanded, "$1")
		assert.NotContains(t, expanded, "$2")
	})

	t.Run("Expand with quoted args", func(t *testing.T) {
		registry3 := NewPromptRegistry(nil)
		entry3 := Entry{
			Tier:       TierUser,
			Kind:       KindPrompt,
			SourcePath: "/test/review3.md",
			Raw: []byte(`---
description: Review files
---
Please review: $1 and $2`),
		}
		err := registry3.Register(entry3)
		require.NoError(t, err)

		// Test quoted argument handling - quotes are removed by parser
		// Input: "arg with spaces" normal  -> args: ["arg with spaces", "normal"]
		expanded, ok := registry3.Expand("review3", "\"arg with spaces\" normal")
		require.True(t, ok)
		assert.Contains(t, expanded, "arg with spaces")
		assert.Contains(t, expanded, "normal")
		assert.NotContains(t, expanded, "$1")
		assert.NotContains(t, expanded, "$2")
	})

	t.Run("Expand with slice ${@:N}", func(t *testing.T) {
		registry4 := NewPromptRegistry(nil)
		entry4 := Entry{
			Tier:       TierUser,
			Kind:       KindPrompt,
			SourcePath: "/test/review4.md",
			Raw: []byte(`---
description: Review files
---
Files from 2 onwards: ${@:2}`),
		}
		err := registry4.Register(entry4)
		require.NoError(t, err)

		expanded, ok := registry4.Expand("review4", "file1.go file2.go file3.go")
		require.True(t, ok)
		assert.Contains(t, expanded, "file2.go")
		assert.Contains(t, expanded, "file3.go")
		assert.NotContains(t, expanded, "file1.go")
	})

	t.Run("Expand with $ARGUMENTS", func(t *testing.T) {
		registry5 := NewPromptRegistry(nil)
		entry5 := Entry{
			Tier:       TierUser,
			Kind:       KindPrompt,
			SourcePath: "/test/review5.md",
			Raw: []byte(`---
description: Review files
---
Please review: $ARGUMENTS`),
		}
		err := registry5.Register(entry5)
		require.NoError(t, err)

		expanded, ok := registry5.Expand("review5", "file1.go file2.go")
		require.True(t, ok)
		assert.Contains(t, expanded, "file1.go file2.go")
		assert.NotContains(t, expanded, "$ARGUMENTS")
	})

	t.Run("Non-existent prompt", func(t *testing.T) {
		_, ok := registry.Expand("nonexistent", "args")
		assert.False(t, ok)
	})
}

// stubUI implements commands.CommandUI for testing.
type stubUI struct{}

func (stubUI) Confirm(_ context.Context, _, _ string) (bool, error) { return true, nil }
func (stubUI) Select(_ context.Context, _ string, _ []string) (int, error) {
	return 0, nil
}
func (stubUI) Input(_ context.Context, _, _ string) (string, error) { return "test", nil }

// stubSession implements commands.CommandSession for testing.
type stubSession struct{}

func (stubSession) GetLastAssistantText() string { return "" }
func (stubSession) GetSessionName() string       { return "test-session" }
func (stubSession) Compact(context.Context, ...string) error {
	return nil
}

// TestLoaderCommandRegistration tests that prompts and skills register
// into the commands registry.
func TestLoaderCommandRegistration(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a skills dir with a skill.
	skillsDir := filepath.Join(tmpDir, "skills")
	err := os.MkdirAll(skillsDir, 0o755)
	require.NoError(t, err)

	skillContent := `---
name: python-test-helper
description: Diagnose and fix flaky pytest failures
---
# Python Test Helper
Use pytest to diagnose failures.`
	err = os.WriteFile(filepath.Join(skillsDir, "python-test-helper.md"), []byte(skillContent), 0o644)
	require.NoError(t, err)

	// Create a prompts dir with a prompt.
	promptsDir := filepath.Join(tmpDir, "prompts")
	err = os.MkdirAll(promptsDir, 0o755)
	require.NoError(t, err)

	promptContent := `---
description: Review code changes
argument-hint: "<files>"
---
Please review: $@`
	err = os.WriteFile(filepath.Join(promptsDir, "review.md"), []byte(promptContent), 0o644)
	require.NoError(t, err)

	// Create a command registry.
	cmdRegistry := commands.NewInMemoryRegistry()

	// Create loader with the command registry.
	loader, err := NewLoader(LoaderOptions{
		Log:              nil,
		CWD:              tmpDir,
		CommandsRegistry: cmdRegistry,
		SettingsPaths: map[ArtifactKind][]string{
			KindSkill:  {skillsDir},
			KindPrompt: {promptsDir},
		},
	})
	require.NoError(t, err)

	// Test prompt registration: "review" from "review.md".
	t.Run("Prompt registered as slash command", func(t *testing.T) {
		handler, info, ok := cmdRegistry.Lookup("review")
		require.True(t, ok, "review command should be registered")
		assert.Equal(t, "review", info.Name)
		assert.Equal(t, "Review code changes", info.Description)
		assert.Equal(t, "\"<files>\"", info.ArgumentHint)
		assert.Equal(t, commands.Prompt, info.Source)

		// Execute the command.
		result, err := handler(context.Background(), "file1.go file2.go", stubUI{}, stubSession{})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "file1.go file2.go")
		assert.Contains(t, result.Output, "Please review:")
	})

	// Test skill registration: "skill:python-test-helper".
	t.Run("Skill registered as /skill:<id>", func(t *testing.T) {
		handler, info, ok := cmdRegistry.Lookup("skill:python-test-helper")
		require.True(t, ok, "skill:python-test-helper command should be registered")
		assert.Equal(t, "skill:python-test-helper", info.Name)
		assert.Equal(t, "Diagnose and fix flaky pytest failures", info.Description)
		assert.Equal(t, commands.Skill, info.Source)

		// Execute the command - should return the skill body.
		result, err := handler(context.Background(), "", stubUI{}, stubSession{})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "# Python Test Helper")
	})

	// Test reload re-registers commands.
	t.Run("Reload re-registers commands", func(t *testing.T) {
		// Add a new skill.
		newSkillContent := `---
name: new-skill
description: A new skill
---
# New Skill Content`
		err = os.WriteFile(filepath.Join(skillsDir, "new-skill.md"), []byte(newSkillContent), 0o644)
		require.NoError(t, err)

		// Reload.
		err = loader.Reload(context.Background())
		require.NoError(t, err)

		// New skill should be registered.
		_, _, ok := cmdRegistry.Lookup("skill:new-skill")
		assert.True(t, ok, "new skill should be registered after reload")
	})

	t.Run("Reload removes deleted commands", func(t *testing.T) {
		err = os.Remove(filepath.Join(skillsDir, "new-skill.md"))
		require.NoError(t, err)

		err = loader.Reload(context.Background())
		require.NoError(t, err)

		_, _, ok := cmdRegistry.Lookup("skill:new-skill")
		assert.False(t, ok, "deleted skill command should be removed after reload")
	})
}

// TestLoaderCommandCollision tests that first-wins precedence works for commands.
func TestLoaderCommandCollision(t *testing.T) {
	tmpDir := t.TempDir()

	// Create dirs at different tiers.
	projectSkillsDir := filepath.Join(tmpDir, ".tau", "skills")
	err := os.MkdirAll(projectSkillsDir, 0o755)
	require.NoError(t, err)

	userSkillsDir := filepath.Join(tmpDir, ".tau", "agent", "skills")
	err = os.MkdirAll(userSkillsDir, 0o755)
	require.NoError(t, err)

	// Create skill in project dir.
	projectSkill := `---
name: test-skill
description: Project skill
---
Project skill body`
	err = os.WriteFile(filepath.Join(projectSkillsDir, "test-skill.md"), []byte(projectSkill), 0o644)
	require.NoError(t, err)

	// Create skill in user dir with same name.
	userSkill := `---
name: test-skill
description: User skill
---
User skill body`
	err = os.WriteFile(filepath.Join(userSkillsDir, "test-skill.md"), []byte(userSkill), 0o644)
	require.NoError(t, err)

	// Create loader with the user dir as CWD (project tier).
	cmdRegistry := commands.NewInMemoryRegistry()
	_, err = NewLoader(LoaderOptions{
		Log:              nil,
		CWD:              tmpDir,
		CommandsRegistry: cmdRegistry,
	})
	require.NoError(t, err)

	// Project tier wins - the skill from .tau/skills should be registered.
	handler, info, ok := cmdRegistry.Lookup("skill:test-skill")
	require.True(t, ok)
	result, _ := handler(nil, "", nil, nil)
	// Body is XML-wrapped per FormatSkillInvocation; assert presence of body
	// and the project-tier source path in the location attribute.
	assert.Contains(t, string(result.Output), "Project skill body")
	assert.Contains(t, string(result.Output), filepath.Join(projectSkillsDir, "test-skill.md"))
	assert.Equal(t, "skill:test-skill", info.Name)
}

// TestPackageDiscovery tests that artifacts are discovered from installed packages.
func TestPackageDiscovery(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a packages directory structure:
	// ~/.tau/agent/packages/
	//   my-pkg/
	//     tau-package.toml
	//     skills/
	//       pkg-skill.md
	//     prompts/
	//       pkg-prompt.md
	//     themes/
	//       pkg-theme.json
	packagesDir := filepath.Join(tmpDir, "packages")
	pkgDir := filepath.Join(packagesDir, "my-pkg")

	err := os.MkdirAll(filepath.Join(pkgDir, "skills"), 0o755)
	require.NoError(t, err)

	err = os.MkdirAll(filepath.Join(pkgDir, "prompts"), 0o755)
	require.NoError(t, err)

	err = os.MkdirAll(filepath.Join(pkgDir, "themes"), 0o755)
	require.NoError(t, err)

	// Create tau-package.toml with explicit paths
	manifestContent := `
[package]
name = "my-pkg"
version = "1.0.0"

[[skill]]
path = "skills/"

[[prompt]]
path = "prompts/"

[[theme]]
path = "themes/"
`
	err = os.WriteFile(filepath.Join(pkgDir, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	// Create skill
	skillContent := `---
name: pkg-skill
description: A skill from a package
---
# Package Skill`
	err = os.WriteFile(filepath.Join(pkgDir, "skills", "pkg-skill.md"), []byte(skillContent), 0o644)
	require.NoError(t, err)

	// Create prompt
	promptContent := `---
description: Package review
argument-hint: "<files>"
---
Please review from package: $@`
	err = os.WriteFile(filepath.Join(pkgDir, "prompts", "pkg-prompt.md"), []byte(promptContent), 0o644)
	require.NoError(t, err)

	// Create theme
	themeContent := `{"name": "pkg-theme"}`
	err = os.WriteFile(filepath.Join(pkgDir, "themes", "pkg-theme.json"), []byte(themeContent), 0o644)
	require.NoError(t, err)

	cfg := DiscoveryConfig{
		HomeDir:      tmpDir,
		PackagePaths: []string{packagesDir},
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	// Verify package artifacts are discovered at TierPackages
	var pkgSkills, pkgPrompts, pkgThemes int
	for _, e := range entries {
		if e.Tier == TierPackages {
			switch e.Kind {
			case KindSkill:
				if strings.Contains(e.SourcePath, "pkg-skill") {
					pkgSkills++
				}
			case KindPrompt:
				if strings.Contains(e.SourcePath, "pkg-prompt") {
					pkgPrompts++
				}
			case KindTheme:
				if strings.Contains(e.SourcePath, "pkg-theme") {
					pkgThemes++
				}
			}
		}
	}

	assert.Equal(t, 1, pkgSkills, "should discover skill from package")
	assert.Equal(t, 1, pkgPrompts, "should discover prompt from package")
	assert.Equal(t, 1, pkgThemes, "should discover theme from package")
}

// TestPackageDiscoveryConventionFallback tests that convention fallback works
// when no manifest artifact paths are declared.
func TestPackageDiscoveryConventionFallback(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a package without explicit artifact paths in manifest.
	packagesDir := filepath.Join(tmpDir, "packages")
	pkgDir := filepath.Join(packagesDir, "convention-pkg")

	err := os.MkdirAll(filepath.Join(pkgDir, "skills"), 0o755)
	require.NoError(t, err)

	err = os.MkdirAll(filepath.Join(pkgDir, "prompts"), 0o755)
	require.NoError(t, err)

	err = os.MkdirAll(filepath.Join(pkgDir, "themes"), 0o755)
	require.NoError(t, err)

	// Create manifest with only package info (no explicit artifact paths).
	manifestContent := `
[package]
name = "convention-pkg"
version = "1.0.0"

[plugin]
api_version = "1"
`
	err = os.WriteFile(filepath.Join(pkgDir, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	// Create artifacts at convention paths.
	err = os.WriteFile(filepath.Join(pkgDir, "skills", "conv-skill.md"),
		[]byte("# Convention Skill"), 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(pkgDir, "prompts", "conv-prompt.md"),
		[]byte("Convention Prompt: $@"), 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(pkgDir, "themes", "conv-theme.json"),
		[]byte(`{"name": "conv-theme"}`), 0o644)
	require.NoError(t, err)

	cfg := DiscoveryConfig{
		HomeDir:      tmpDir,
		PackagePaths: []string{packagesDir},
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	// Verify convention fallback discovers artifacts.
	var foundSkill, foundPrompt, foundTheme bool
	for _, e := range entries {
		if e.Tier == TierPackages {
			switch e.Kind {
			case KindSkill:
				if strings.Contains(e.SourcePath, "conv-skill") {
					foundSkill = true
				}
			case KindPrompt:
				if strings.Contains(e.SourcePath, "conv-prompt") {
					foundPrompt = true
				}
			case KindTheme:
				if strings.Contains(e.SourcePath, "conv-theme") {
					foundTheme = true
				}
			}
		}
	}

	assert.True(t, foundSkill, "should discover skill via convention fallback")
	assert.True(t, foundPrompt, "should discover prompt via convention fallback")
	assert.True(t, foundTheme, "should discover theme via convention fallback")
}

// TestPackageDiscoveryMultipleEntries tests multiple [[skills]] entries per package.
func TestPackageDiscoveryMultipleEntries(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a package with multiple skill directories.
	packagesDir := filepath.Join(tmpDir, "packages")
	pkgDir := filepath.Join(packagesDir, "multi-skill-pkg")

	err := os.MkdirAll(filepath.Join(pkgDir, "core-skills"), 0o755)
	require.NoError(t, err)

	err = os.MkdirAll(filepath.Join(pkgDir, "extra-skills"), 0o755)
	require.NoError(t, err)

	// Create manifest with multiple [[skill]] entries.
	manifestContent := `
[package]
name = "multi-skill-pkg"
version = "1.0.0"

[[skill]]
path = "core-skills/"

[[skill]]
path = "extra-skills/"
`
	err = os.WriteFile(filepath.Join(pkgDir, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	// Create skills in both directories.
	err = os.WriteFile(filepath.Join(pkgDir, "core-skills", "core.md"),
		[]byte("# Core Skill"), 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(pkgDir, "extra-skills", "extra.md"),
		[]byte("# Extra Skill"), 0o644)
	require.NoError(t, err)

	cfg := DiscoveryConfig{
		HomeDir:      tmpDir,
		PackagePaths: []string{packagesDir},
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	// Verify both skill directories are discovered.
	var foundCore, foundExtra bool
	for _, e := range entries {
		if e.Tier == TierPackages && e.Kind == KindSkill {
			if strings.Contains(e.SourcePath, "core") {
				foundCore = true
			}
			if strings.Contains(e.SourcePath, "extra") {
				foundExtra = true
			}
		}
	}

	assert.True(t, foundCore, "should discover core skill")
	assert.True(t, foundExtra, "should discover extra skill")
}

// TestPackageDiscoveryLinked tests that --link installs work correctly.
// The install creates a symlink at <pkgPath>/<name> pointing to the source,
// and artifact discovery must resolve the symlink to read the actual manifest
// and artifact files from the source directory.
func TestPackageDiscoveryLinked(t *testing.T) {
	tmpDir := t.TempDir()

	// Create source package directory (the "source tree" in spec terms).
	srcDir := filepath.Join(tmpDir, "source-pkg")
	err := os.MkdirAll(filepath.Join(srcDir, "skills"), 0o755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(srcDir, "prompts"), 0o755)
	require.NoError(t, err)

	// Create tau-package.toml.
	manifestContent := `
[package]
name = "linked-pkg"
version = "1.0.0"

[[skill]]
path = "skills/"

[[prompt]]
path = "prompts/"
`
	err = os.WriteFile(filepath.Join(srcDir, "tau-package.toml"), []byte(manifestContent), 0o644)
	require.NoError(t, err)

	// Create artifacts.
	err = os.WriteFile(filepath.Join(srcDir, "skills", "linked-skill.md"),
		[]byte("# Linked Skill"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(srcDir, "prompts", "linked-prompt.md"),
		[]byte("Linked Prompt: $@"), 0o644)
	require.NoError(t, err)

	// Simulate --link install: create symlink at <packagesDir>/<name> -> srcDir.
	packagesDir := filepath.Join(tmpDir, "packages")
	err = os.MkdirAll(packagesDir, 0o755)
	require.NoError(t, err)

	linkPath := filepath.Join(packagesDir, "linked-pkg")
	err = os.Symlink(srcDir, linkPath)
	require.NoError(t, err)

	// Verify it's a symlink.
	info, err := os.Lstat(linkPath)
	require.NoError(t, err)
	assert.True(t, info.Mode()&os.ModeSymlink != 0, "should be a symlink")

	cfg := DiscoveryConfig{
		HomeDir:      tmpDir,
		PackagePaths: []string{packagesDir},
	}

	entries, err := Walk(cfg)
	require.NoError(t, err)

	// Verify artifacts from the symlinked package are discovered.
	var foundSkill, foundPrompt bool
	for _, e := range entries {
		if e.Tier == TierPackages {
			switch e.Kind {
			case KindSkill:
				if strings.Contains(e.SourcePath, "linked-skill") {
					foundSkill = true
				}
			case KindPrompt:
				if strings.Contains(e.SourcePath, "linked-prompt") {
					foundPrompt = true
				}
			}
		}
	}

	assert.True(t, foundSkill, "should discover skill from symlinked package")
	assert.True(t, foundPrompt, "should discover prompt from symlinked package")
}

// TestDiscoveryHonorsIgnoreFiles tests that artifact discovery honors .gitignore,
// .ignore, and .fdignore files during directory traversal.
func TestDiscoveryHonorsIgnoreFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Create directory structure:
	// tmpDir/
	//   .gitignore
	//   SKILL.md
	//   should-find.md
	//   should-ignore.md
	//   ignored-dir/
	//     SKILL.md
	//     nested.md
	//   normal-dir/
	//     SKILL.md
	//     nested.md
	//   .hidden-file.md
	//   also-ignore.txt

	// Create .gitignore
	// Use /*.md to ignore only root-level .md files, not files in subdirectories
	gitignore := `.hidden-file.md
*.txt
/*.md
ignored-dir/`
	err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(gitignore), 0o644)
	require.NoError(t, err)

	// Create root-level SKILL.md for directory skill
	skillMd := `# Test Skill
`
	err = os.WriteFile(filepath.Join(tmpDir, "SKILL.md"), []byte(skillMd), 0o644)
	require.NoError(t, err)

	// Create root-level .md files
	err = os.WriteFile(filepath.Join(tmpDir, "should-find.md"), []byte("# Find Me"), 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "should-ignore.md"), []byte("# Ignore Me"), 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, ".hidden-file.md"), []byte("# Hidden"), 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tmpDir, "also-ignore.txt"), []byte("ignore"), 0o644)
	require.NoError(t, err)

	// Create ignored-dir
	ignoredDir := filepath.Join(tmpDir, "ignored-dir")
	err = os.MkdirAll(ignoredDir, 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(ignoredDir, "SKILL.md"), []byte(skillMd), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(ignoredDir, "nested.md"), []byte("# Nested"), 0o644)
	require.NoError(t, err)

	// Create normal-dir (not ignored)
	normalDir := filepath.Join(tmpDir, "normal-dir")
	err = os.MkdirAll(normalDir, 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(normalDir, "SKILL.md"), []byte(skillMd), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(normalDir, "nested.md"), []byte("# Nested"), 0o644)
	require.NoError(t, err)

	// Create .ignore file to ignore also-ignore.txt specifically
	ignoreFile := `also-ignore.txt
`
	err = os.WriteFile(filepath.Join(tmpDir, ".ignore"), []byte(ignoreFile), 0o644)
	require.NoError(t, err)

	// Create .fdignore file
	fdignore := `.fdignore
`
	err = os.WriteFile(filepath.Join(tmpDir, ".fdignore"), []byte(fdignore), 0o644)
	require.NoError(t, err)

	// Test discoverDir directly since Walk expects standard directory structure
	// (.tau/skills/, .tau/prompts/, etc.). We test the core ignore functionality
	// by calling discoverDir directly with tmpDir as both the directory and root.

	// Debug: Test the ignore matcher directly
	m := newIgnoreMatcher(tmpDir, tmpDir)
	if combined, ok := m.GitIgnore.(*combinedPatternsMatcher); ok {
		t.Logf("Ignore patterns loaded (%d): %v", len(combined.patterns), combined.patterns)
	}
	t.Logf("Testing ignores for different paths:")
	t.Logf("  ignores('.hidden-file.md') = %v", m.ignores(".hidden-file.md"))
	t.Logf("  ignores('also-ignore.txt') = %v", m.ignores("also-ignore.txt"))
	t.Logf("  ignores('normal-dir') = %v", m.ignores("normal-dir"))
	t.Logf("  ignores('normal-dir/') = %v", m.ignores("normal-dir/"))
	t.Logf("  ignores('ignored-dir') = %v", m.ignores("ignored-dir"))
	t.Logf("  ignores('ignored-dir/') = %v", m.ignores("ignored-dir/"))
	t.Logf("  ignores('normal-dir/SKILL.md') = %v", m.ignores("normal-dir/SKILL.md"))
	t.Logf("  ignores('normal-dir/nested.md') = %v", m.ignores("normal-dir/nested.md"))
	t.Logf("  ignores('SKILL.md') = %v", m.ignores("SKILL.md"))
	t.Logf("  ignores('.fdignore') = %v", m.ignores(".fdignore"))
	t.Logf("  isIgnored(m, filepath.Join(tmpDir, 'normal-dir'), true) = %v", isIgnored(m, filepath.Join(tmpDir, "normal-dir"), true))
	t.Logf("  isIgnored(m, filepath.Join(tmpDir, 'normal-dir', 'nested.md'), false) = %v", isIgnored(m, filepath.Join(tmpDir, "normal-dir", "nested.md"), false))

	entries, err := discoverDir(tmpDir, KindSkill, TierBuiltin, false, tmpDir)
	require.NoError(t, err)

	// Debug: print all entries found
	t.Logf("Total entries found: %d", len(entries))

	// Debug: list tmpDir contents
	t.Logf("tmpDir: %s", tmpDir)
	entries2, _ := os.ReadDir(tmpDir)
	for _, e := range entries2 {
		t.Logf("  %s (isDir=%v)", e.Name(), e.IsDir())
	}

	for _, e := range entries {
		t.Logf("Found entry: %s", e.SourcePath)
	}

	// Build a map of source paths for easy checking
	found := make(map[string]bool)
	for _, e := range entries {
		found[e.SourcePath] = true
		t.Logf("Found entry: %s", e.SourcePath)
	}

	// Should find: normal-dir/... only (root .md files are all ignored by *.md)
	assert.True(t, found[filepath.Join(normalDir, "SKILL.md")], "should find normal-dir/SKILL.md")
	assert.True(t, found[filepath.Join(normalDir, "nested.md")], "should find normal-dir/nested.md")

	// Should NOT find: root-level files (all ignored by *.md), ignored files/directories
	assert.False(t, found[filepath.Join(tmpDir, "SKILL.md")], "root SKILL.md should be ignored by *.md")
	assert.False(t, found[filepath.Join(tmpDir, "should-find.md")], "should-find.md should be ignored by *.md")
	assert.False(t, found[filepath.Join(tmpDir, "should-ignore.md")], "should-ignore.md should be ignored by *.md")
	assert.False(t, found[filepath.Join(tmpDir, ".hidden-file.md")], ".hidden-file.md should be ignored by .gitignore")
	assert.False(t, found[filepath.Join(tmpDir, "also-ignore.txt")], "also-ignore.txt should be ignored by *.txt")
	assert.False(t, found[filepath.Join(ignoredDir, "SKILL.md")], "ignored-dir should be ignored")
	assert.False(t, found[filepath.Join(ignoredDir, "nested.md")], "ignored-dir/nested.md should be ignored")

	// Verify we have expected number of entries (normal-dir only, no root-level .md files)
	// normal-dir: SKILL.md, nested.md
	// Total: 2 entries
	assert.Equal(t, 2, len(entries), "should have exactly 2 entries")
}

// TestSkillRegistryRejectsInvalidNames verifies that skills failing a
// structural validation rule (length, charset, hyphen placement) are NOT
// registered, while a name-vs-parent-dir mismatch (soft warning) IS.
func TestSkillRegistryRejectsInvalidNames(t *testing.T) {
	tests := []struct {
		name            string
		frontmatterName string
		parentDirName   string
		wantRegistered  bool
	}{
		{"valid lowercase name matches parent", "my-skill", "my-skill", true},
		{"soft mismatch still registers", "my-skill", "different", true},
		{"uppercase rejected", "MySkill", "MySkill", false},
		{"trailing hyphen rejected", "my-skill-", "my-skill-", false},
		{"leading hyphen rejected", "-my-skill", "-my-skill", false},
		{"consecutive hyphens rejected", "my--skill", "my--skill", false},
		{"invalid chars rejected", "my_skill", "my_skill", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := NewSkillRegistry(nil)
			entry := Entry{
				Tier:       TierProject,
				Kind:       KindSkill,
				SourcePath: "/tmp/" + tt.parentDirName + "/SKILL.md",
				Raw:        []byte("---\nname: " + tt.frontmatterName + "\ndescription: A test skill\n---\n# Body"),
			}
			_, err := registry.Register(entry)
			if tt.wantRegistered {
				if err != nil {
					t.Errorf("expected registration, got error: %v", err)
				}
				if registry.Len() != 1 {
					t.Errorf("registry.Len() = %d, want 1", registry.Len())
				}
			} else {
				if err == nil {
					t.Errorf("expected rejection, got nil error")
				}
				if registry.Len() != 0 {
					t.Errorf("registry.Len() = %d, want 0 (rejected skill must not be registered)", registry.Len())
				}
			}
		})
	}
}

// TestSkillRegistryRejectsEmptyDescription verifies that a skill with no
// description is rejected (the existing description-empty behavior).
func TestSkillRegistryRejectsEmptyDescription(t *testing.T) {
	registry := NewSkillRegistry(nil)
	entry := Entry{
		Tier:       TierProject,
		Kind:       KindSkill,
		SourcePath: "/tmp/my-skill/SKILL.md",
		Raw:        []byte("---\nname: my-skill\n---\n# Body"),
	}
	_, err := registry.Register(entry)
	if err == nil {
		t.Fatal("expected rejection for missing description, got nil")
	}
	if registry.Len() != 0 {
		t.Errorf("registry.Len() = %d, want 0 (skill with no description must not be registered)", registry.Len())
	}
}

// TestSkillRegistryLoadWithDiagnosticsReturnsAllDiagnostics verifies that
// LoadWithDiagnostics surfaces diagnostics for warnings, validation errors,
// and collisions in a single slice.
func TestSkillRegistryLoadWithDiagnosticsReturnsAllDiagnostics(t *testing.T) {
	registry := NewSkillRegistry(nil)
	entries := []Entry{
		{
			Tier:       TierProject,
			Kind:       KindSkill,
			SourcePath: "/tmp/valid-skill/SKILL.md",
			Raw:        []byte("---\nname: valid-skill\ndescription: A valid skill\n---\n# Body"),
		},
		{
			Tier:       TierProject,
			Kind:       KindSkill,
			SourcePath: "/tmp/InvalidSkill/SKILL.md",
			Raw:        []byte("---\nname: InvalidSkill\ndescription: A skill with bad name\n---\n# Body"),
		},
		{
			Tier:       TierProject,
			Kind:       KindSkill,
			SourcePath: "/tmp/valid-skill/SKILL.md",
			Raw:        []byte("---\nname: valid-skill\ndescription: Same name as entry 0\n---\n# Body"),
		},
	}

	diags := registry.LoadWithDiagnostics(entries)
	if len(diags) != 2 {
		t.Fatalf("LoadWithDiagnostics returned %d diagnostics, want 2 (1 error + 1 collision)", len(diags))
	}

	// Find the error and collision diagnostics.
	var errDiag, collDiag *Diagnostic
	for i := range diags {
		switch diags[i].Type {
		case DiagnosticError:
			errDiag = &diags[i]
		case DiagnosticCollision:
			collDiag = &diags[i]
		}
	}
	if errDiag == nil {
		t.Errorf("expected one DiagnosticError diagnostic, got none")
	} else if !strings.Contains(errDiag.Message, "invalid characters") {
		t.Errorf("error diagnostic message = %q, want substring 'invalid characters'", errDiag.Message)
	}
	if collDiag == nil {
		t.Errorf("expected one DiagnosticCollision diagnostic, got none")
	}

	// Registry should contain only the valid skill (invalid rejected, collision skipped).
	if registry.Len() != 1 {
		t.Errorf("registry.Len() = %d, want 1 (only valid-skill)", registry.Len())
	}
}

// TestPackageDiscoveryNestedSkillCategories reproduces manifest-less installs
// like github.com/mattpocock/skills, where skills/ groups directory skills
// under category subdirectories (skills/<category>/<name>/SKILL.md).
// Discovery must recurse past the category dirs without loading their loose
// .md files or walking VCS internals.
func TestPackageDiscoveryNestedSkillCategories(t *testing.T) {
	tmpDir := t.TempDir()

	packagesDir := filepath.Join(tmpDir, "packages")
	pkgDir := filepath.Join(packagesDir, "grouped-skills")

	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(pkgDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	write(filepath.Join("skills", "engineering", "tdd", "SKILL.md"),
		"---\nname: tdd\ndescription: Test-driven development\n---\n# TDD")
	write(filepath.Join("skills", "productivity", "handoff", "SKILL.md"),
		"---\nname: handoff\ndescription: Hand off work in progress\n---\n# Handoff")
	// Loose .md directly in a category dir (not a skill dir) must not load.
	write(filepath.Join("skills", "engineering", "README.md"),
		"---\nname: readme\ndescription: Not a skill\n---\n# README")
	// VCS internals must never be walked.
	write(filepath.Join("skills", "engineering", ".git", "objects", "pack", "SKILL.md"),
		"---\nname: leaked\n.description: bogus\n---\n# Leaked")

	entries, err := Walk(DiscoveryConfig{
		HomeDir:      tmpDir,
		PackagePaths: []string{packagesDir},
	})
	require.NoError(t, err)

	found := make(map[string]bool)
	for _, e := range entries {
		if e.Tier == TierPackages && e.Kind == KindSkill {
			found[e.SourcePath] = true
		}
	}

	assert.True(t, found[filepath.Join(pkgDir, "skills", "engineering", "tdd", "SKILL.md")],
		"should discover skill nested under a category dir, got: %v", found)
	assert.True(t, found[filepath.Join(pkgDir, "skills", "productivity", "handoff", "SKILL.md")],
		"should discover skill nested under a category dir, got: %v", found)
	assert.False(t, found[filepath.Join(pkgDir, "skills", "engineering", "README.md")],
		"category-dir .md must not load as a loose skill")
	assert.Len(t, found, 2, "exactly the two directory skills, got: %v", found)
}
