package artifacts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/agent/packages"
	"github.com/losinggeneration/tau/ai"

	"github.com/losinggeneration/tau/commands"
)

// TestLoader_SkillCommandHandlerReturnsXMLWrappedBody verifies that the
// /skill:<id> slash handler returns the FormatSkillInvocation output, not
// the raw skill body. This is the contract callers (TUI, RPC, headless print
// mode) rely on to produce the same prompt shape as the runtime path.
func TestLoader_SkillCommandHandlerReturnsXMLWrappedBody(t *testing.T) {
	skill := &Skill{
		ID:          "calendar",
		Name:        "calendar",
		Description: "schedule things",
		Body:        []byte("Use the calendar tool."),
		SourcePath:  "/tmp/skills/calendar/SKILL.md",
		BaseDir:     "/tmp/skills/calendar",
		Tier:        TierUser,
	}
	skills := NewSkillRegistry(nil)
	skills.skills[skill.ID] = skill
	skills.rebuildOrdered()

	reg := commands.NewInMemoryRegistry()
	l := &Loader{
		skills:           skills,
		prompts:          NewPromptRegistry(nil),
		themes:           NewThemeRegistry(nil),
		commandsRegistry: reg,
		commandSources:   map[string]struct{}{},
	}
	l.registerCommands()

	h, _, ok := reg.Lookup("skill:calendar")
	if !ok {
		t.Fatal("/skill:calendar not registered")
	}
	res, err := h(context.Background(), "do thing", nil, nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !strings.HasPrefix(res.Output, `<skill name="calendar"`) {
		t.Errorf("output should start with XML wrapper, got: %q", truncate(res.Output, 80))
	}
	if !strings.Contains(res.Output, "Use the calendar tool.") {
		t.Error("body missing from output")
	}
	if !strings.HasSuffix(res.Output, "</skill>\n\ndo thing") {
		t.Errorf("args not appended after blank line; got: %q", res.Output)
	}
}

func TestLoader_SkillCommandHandlerNoArgs(t *testing.T) {
	skill := &Skill{
		ID:         "x",
		Name:       "x",
		Body:       []byte("body"),
		SourcePath: "/p/SKILL.md",
		BaseDir:    "/p",
		Tier:       TierUser,
	}
	skills := NewSkillRegistry(nil)
	skills.skills[skill.ID] = skill
	skills.rebuildOrdered()

	reg := commands.NewInMemoryRegistry()
	l := &Loader{
		skills:           skills,
		prompts:          NewPromptRegistry(nil),
		themes:           NewThemeRegistry(nil),
		commandsRegistry: reg,
		commandSources:   map[string]struct{}{},
	}
	l.registerCommands()

	h, _, _ := reg.Lookup("skill:x")
	res, _ := h(context.Background(), "", nil, nil)
	if !strings.HasSuffix(res.Output, "</skill>") {
		t.Errorf("no-args output should end at </skill>, got: %q", res.Output)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func TestLoaderUsesThemeValidatorOnStartupAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "theme.json")
	if err := os.WriteFile(path, []byte(`{"name":"valid"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	loader, err := NewLoader(LoaderOptions{CWD: t.TempDir(), CLIPaths: map[ArtifactKind][]string{KindTheme: {path}}, DisableSkills: true, DisablePrompts: true, ThemeValidator: func(raw []byte) error {
		calls++
		if strings.Contains(string(raw), "invalid") {
			return errors.New("invalid colors")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loader.ThemeRegistry().Get("valid"); !ok {
		t.Fatal("valid theme absent")
	}
	before := calls
	if err := os.WriteFile(path, []byte(`{"name":"invalid"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loader.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls <= before {
		t.Fatal("reload bypassed validator")
	}
	if _, ok := loader.ThemeRegistry().Get("invalid"); ok {
		t.Fatal("invalid theme registered")
	}
}

func TestLoaderPrepareCommitsSettingsPathsOnlyOnCommit(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"old", "new"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, name+".md"), []byte("body"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg := commands.NewInMemoryRegistry()
	loader, err := NewLoader(LoaderOptions{
		CWD:              t.TempDir(),
		UserDirs:         map[ArtifactKind]string{KindPrompt: filepath.Join(dir, "none")},
		SettingsPaths:    map[ArtifactKind][]string{KindPrompt: {filepath.Join(dir, "old")}},
		DisableSkills:    true,
		DisableThemes:    true,
		CommandsRegistry: reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := loader.Prepare(ReloadOptions{SettingsPaths: map[ArtifactKind][]string{KindPrompt: {filepath.Join(dir, "new")}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loader.PromptRegistry().Get("new"); ok {
		t.Fatal("prepare replaced prompts before commit")
	}
	if _, _, ok := reg.Lookup("old"); !ok {
		t.Fatal("prepare removed the old command before commit")
	}
	prepared.Commit()
	if _, ok := loader.PromptRegistry().Get("old"); ok {
		t.Fatal("commit kept prompts from the old settings paths")
	}
	if _, _, ok := reg.Lookup("new"); !ok {
		t.Fatal("commit did not register the new prompt command")
	}
	if _, _, ok := reg.Lookup("old"); ok {
		t.Fatal("commit kept the old prompt command")
	}
	if err := loader.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := loader.PromptRegistry().Get("new"); !ok {
		t.Fatal("reload did not keep the committed settings paths")
	}
}

func TestLoaderPrepareReplacesPackageDirectoriesOnlyOnCommit(t *testing.T) {
	root := t.TempDir()
	oldDir, newDir := filepath.Join(root, "old"), filepath.Join(root, "new")
	for _, dir := range []string{oldDir, newDir} {
		path := filepath.Join(dir, "example", "prompts", filepath.Base(dir)+".md")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("body"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg := commands.NewInMemoryRegistry()
	loader, err := NewLoader(LoaderOptions{CWD: t.TempDir(), PackagePaths: []string{oldDir}, DisableSkills: true, DisableThemes: true, CommandsRegistry: reg})
	if err != nil {
		t.Fatal(err)
	}
	assertPrompt := func(name string, want bool) {
		t.Helper()
		if _, ok := loader.PromptRegistry().Get(name); ok != want {
			t.Fatalf("prompt %s present = %t, want %t", name, ok, want)
		}
		if _, _, ok := reg.Lookup(name); ok != want {
			t.Fatalf("command %s present = %t, want %t", name, ok, want)
		}
	}
	opts := ReloadOptions{PackagePaths: []string{newDir}}
	prepared, err := loader.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.PackagePaths[0] = "mutated"
	assertPrompt("old", true)
	assertPrompt("new", false)
	prepared.Commit()
	assertPrompt("old", false)
	assertPrompt("new", true)
	// Omitting package paths keeps the committed directories.
	prepared, err = loader.Prepare(ReloadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	if err := loader.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPrompt("new", true)
	prepared, err = loader.Prepare(ReloadOptions{PackagePaths: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	assertPrompt("new", true)
	prepared.Commit()
	assertPrompt("new", false)
	if err := loader.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPrompt("new", false)
}

func TestLoaderPreparedPackagePoliciesOwnTheirInputs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "prompts", "keep.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	loader, err := NewLoader(LoaderOptions{CWD: t.TempDir(), DisableSkills: true, DisableThemes: true})
	if err != nil {
		t.Fatal(err)
	}
	input := []packages.ResolvedPackage{{Root: root, Source: ai.PackageSource{Autoload: new(false)}, Delta: &ai.PackageSource{Prompts: []string{"+prompts/keep.md"}}}}
	prepared, err := loader.Prepare(ReloadOptions{Packages: input})
	if err != nil {
		t.Fatal(err)
	}
	input[0].Delta.Prompts[0] = "-prompts/keep.md"
	*input[0].Source.Autoload = true
	prepared.Commit()
	if err := loader.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := loader.PromptRegistry().Get("keep"); !ok {
		t.Fatal("prepared package policy retained caller aliases")
	}
	prepared, err = loader.Prepare(ReloadOptions{Packages: []packages.ResolvedPackage{}})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	if err := loader.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := loader.PromptRegistry().Get("keep"); ok {
		t.Fatal("explicit empty policy restored implicit scanning")
	}
}

func TestLoaderPiManifestPoliciesSurvivePreparedReload(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"keep", "drop"} {
		path := filepath.Join(root, "custom", name+".md")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(name), 0o600))
	}
	manifest := filepath.Join(root, "package.json")
	require.NoError(t, os.WriteFile(manifest, []byte(`{"pi":{"prompts":["custom/*.md","!custom/drop.md"]}}`), 0o600))
	loader, err := NewLoader(LoaderOptions{CWD: t.TempDir(), DisableSkills: true, DisableThemes: true, Packages: []packages.ResolvedPackage{{Root: root}}})
	require.NoError(t, err)
	_, ok := loader.PromptRegistry().Get("keep")
	require.True(t, ok)
	_, ok = loader.PromptRegistry().Get("drop")
	require.False(t, ok)
	require.NoError(t, os.WriteFile(manifest, []byte(`{"pi":{"prompts":[]}}`), 0o600))
	prepared, err := loader.Prepare(ReloadOptions{})
	require.NoError(t, err)
	_, ok = loader.PromptRegistry().Get("keep")
	require.True(t, ok, "prepare changed live manifest resources")
	prepared.Commit()
	require.NoError(t, loader.Reload(t.Context()))
	_, ok = loader.PromptRegistry().Get("keep")
	require.False(t, ok, "reload restored a resource disabled by the Pi manifest")
}
