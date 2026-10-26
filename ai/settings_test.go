package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelRegistry_LoadMissing verifies a missing models.json returns empty.
func TestModelRegistry_LoadMissing(t *testing.T) {
	reg, err := LoadModelRegistry(filepath.Join(t.TempDir(), "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.ProviderIDs()) != 0 {
		t.Errorf("expected empty registry, got %d providers", len(reg.ProviderIDs()))
	}
}

// TestModelRegistry_LoadAndSave verifies the Pi-compatible on-disk shape.
func TestModelRegistry_LoadAndSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")

	contents := `{
  "providers": {
    "my-provider": {
      "baseUrl": "https://api.example.com/v1",
      "apiKey": "MY_PROVIDER_API_KEY",
      "api": "openai-chat-completions",
      "authHeader": true,
      "models": [
        {"id": "my-model", "contextWindow": 128000, "maxTokens": 16384}
      ],
      "modelOverrides": {
        "existing-model-id": {"contextWindow": 200000}
      }
    }
  }
}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadModelRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := reg.Provider("my-provider")
	if !ok {
		t.Fatal("expected my-provider")
	}
	if p.BaseURL != "https://api.example.com/v1" {
		t.Errorf("baseUrl: %s", p.BaseURL)
	}
	if p.API != "openai-chat-completions" {
		t.Errorf("api: %s", p.API)
	}
	if !p.AuthHeader {
		t.Error("expected authHeader=true")
	}
	if len(p.Models) != 1 || p.Models[0].ID != "my-model" {
		t.Errorf("models: %+v", p.Models)
	}
	if ov, ok := p.ModelOverrides["existing-model-id"]; !ok || ov.ContextWindow != 200000 {
		t.Errorf("override: %+v", p.ModelOverrides)
	}
}

// TestModelRegistry_SetCustomModels verifies the discover-save flow.
func TestModelRegistry_SetCustomModels(t *testing.T) {
	reg := NewModelRegistry()
	if err := reg.SetCustomModels("openai", []string{"gpt-4o", "gpt-5.4"}); err != nil {
		t.Fatal(err)
	}
	p, ok := reg.Provider("openai")
	if !ok {
		t.Fatal("expected openai")
	}
	if len(p.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(p.Models))
	}
	if p.Models[0].ID != "gpt-4o" || p.Models[1].ID != "gpt-5.4" {
		t.Errorf("models: %+v", p.Models)
	}
}

// TestKeybindings_LoadAndLookup verifies the canonical id → binding lookup.
func TestKeybindings_LoadAndLookup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keybindings.json")
	contents := `{
  "app.model.select": "ctrl+l",
  "app.session.tree": ["ctrl+x", "t"],
  "rovel.input.submit": "enter"
}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := LoadKeybindings(path)
	if err != nil {
		t.Fatal(err)
	}

	got := kb.Lookup("app.model.select")
	if len(got) != 1 || got[0] != "ctrl+l" {
		t.Errorf("expected ctrl+l, got %v", got)
	}
	got = kb.Lookup("app.session.tree")
	if len(got) != 2 || got[0] != "ctrl+x" || got[1] != "t" {
		t.Errorf("expected [ctrl+x t], got %v", got)
	}
}

// TestKeybindings_InvalidEntriesIgnored verifies the spec's "ignore" policy.
func TestKeybindings_InvalidEntriesIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keybindings.json")
	// "bogus" has an invalid value (number). It should be skipped, not crash.
	contents := `{
  "app.model.select": "ctrl+l",
  "bogus": 42
}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := LoadKeybindings(path)
	if err != nil {
		t.Fatalf("expected tolerant load, got %v", err)
	}
	if kb.Lookup("app.model.select")[0] != "ctrl+l" {
		t.Error("valid entry should load")
	}
	if kb.Lookup("bogus") != nil {
		t.Error("invalid entry should be ignored")
	}
}

// TestSettings_RoundTripFullFields exercises every Pi-schema field to confirm
// the on-disk shape matches the settings schema.
func TestSettings_RoundTripFullFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	original := &Settings{
		DefaultProvider:        "anthropic",
		DefaultModel:           "claude-sonnet-4-5",
		Theme:                  "dark",
		DefaultThinkingLevel:   "high",
		SteeringMode:           "all",
		FollowUpMode:           "one-at-a-time",
		Transport:              "sse",
		HideThinkingBlock:      true,
		ShellPath:              "/bin/bash",
		QuietStartup:           true,
		ShellCommandPrefix:     "set -e; ",
		CollapseChangelog:      true,
		EnableInstallTelemetry: false,
		ShowHardwareCursor:     true,
		EditorPaddingX:         2,
		AutocompleteMaxVisible: 7,
		EnableSkillCommands:    true,
		DoubleEscapeAction:     "fork",
		TreeFilterMode:         "labeled-only",
		SessionDir:             "/tmp/tau-sessions",
		Compaction:             CompactionSettings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 20000},
		Retry:                  RetrySettings{Enabled: true, MaxRetries: 3, BaseDelayMs: 2000, MaxDelayMs: 60000},
		BranchSummary:          BranchSummarySettings{ReserveTokens: 16384, SkipPrompt: true},
		ThinkingBudgets:        ThinkingBudgetsSettings{Minimal: 1024, Low: 2048, Medium: 8192, High: 16384},
		Terminal:               TerminalSettings{ShowImages: ptrBool(true), ClearOnShrink: true},
		Markdown:               MarkdownSettings{CodeBlockIndent: "    "},
	}

	if err := SaveSettings(path, original); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.Compaction.ReserveTokens != 16384 || !loaded.Compaction.Enabled {
		t.Errorf("compaction roundtrip: %+v", loaded.Compaction)
	}
	if loaded.Retry.MaxRetries != 3 || loaded.Retry.BaseDelayMs != 2000 {
		t.Errorf("retry roundtrip: %+v", loaded.Retry)
	}
	if loaded.ThinkingBudgets.Minimal != 1024 || loaded.ThinkingBudgets.High != 16384 {
		t.Errorf("thinking budgets roundtrip: %+v", loaded.ThinkingBudgets)
	}
	if loaded.BranchSummary.SkipPrompt != true || loaded.BranchSummary.ReserveTokens != 16384 {
		t.Errorf("branch summary roundtrip: %+v", loaded.BranchSummary)
	}
	if loaded.Terminal.ClearOnShrink != true {
		t.Errorf("terminal.clearOnShrink roundtrip: %+v", loaded.Terminal)
	}
	if loaded.Markdown.CodeBlockIndent != "    " {
		t.Errorf("markdown.codeBlockIndent roundtrip: %q", loaded.Markdown.CodeBlockIndent)
	}
}

// TestSettings_ModelsRoundTrip verifies the per-model settings map parses
// from and saves back to settings.json, keyed by "provider/model-id".
func TestSettings_ModelsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if err := os.WriteFile(path, []byte(`{
  "defaultThinkingLevel": "medium",
  "models": {
    "anthropic/claude-opus-4-6": {"thinkingLevel": "high"},
    "openai/gpt-5.4": {"thinkingLevel": "off"}
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Models["anthropic/claude-opus-4-6"].ThinkingLevel; got != "high" {
		t.Errorf("anthropic per-model level = %q, want high", got)
	}
	if got := loaded.Models["openai/gpt-5.4"].ThinkingLevel; got != "off" {
		t.Errorf("openai per-model level = %q, want off", got)
	}

	if err := SaveSettings(path, loaded); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Models["anthropic/claude-opus-4-6"].ThinkingLevel; got != "high" {
		t.Errorf("anthropic per-model level after save = %q, want high", got)
	}
	if len(reloaded.Models) != 2 {
		t.Errorf("models map size after save = %d, want 2", len(reloaded.Models))
	}
}

// TestMergeSettings_ModelsPerKey verifies project settings merge per model
// key: project overrides the same key and adds new keys without dropping
// user-configured ones.
func TestMergeSettings_ModelsPerKey(t *testing.T) {
	user := &Settings{Models: map[string]ModelSettings{
		"anthropic/claude-opus-4-6": {ThinkingLevel: "high"},
		"google/gemini-2.5-pro":     {ThinkingLevel: "off"},
	}}
	project := &Settings{Models: map[string]ModelSettings{
		"anthropic/claude-opus-4-6": {ThinkingLevel: "low"},
		"openai/gpt-5.4":            {ThinkingLevel: "minimal"},
	}}

	merged := MergeSettings(user, project)

	if got := merged.Models["anthropic/claude-opus-4-6"].ThinkingLevel; got != "low" {
		t.Errorf("project override = %q, want low", got)
	}
	if got := merged.Models["google/gemini-2.5-pro"].ThinkingLevel; got != "off" {
		t.Errorf("user-only key = %q, want off (must survive merge)", got)
	}
	if got := merged.Models["openai/gpt-5.4"].ThinkingLevel; got != "minimal" {
		t.Errorf("project-added key = %q, want minimal", got)
	}
}

// TestSettings_ThinkingLevelFor verifies the per-model lookup precedence:
// per-model override, then global defaultThinkingLevel, then "".
func TestSettings_ThinkingLevelFor(t *testing.T) {
	s := &Settings{
		DefaultThinkingLevel: "medium",
		Models: map[string]ModelSettings{
			"anthropic/claude-opus-4-6": {ThinkingLevel: "high"},
			"broken/typo-model":         {ThinkingLevel: "hight"},
		},
	}

	if got := s.ThinkingLevelFor("anthropic/claude-opus-4-6"); got != "high" {
		t.Errorf("per-model = %q, want high", got)
	}
	if got := s.ThinkingLevelFor("unknown/model"); got != "medium" {
		t.Errorf("fallback to default = %q, want medium", got)
	}
	// An invalid per-model value must not reach the request path; fall through
	// to the global default instead.
	if got := s.ThinkingLevelFor("broken/typo-model"); got != "medium" {
		t.Errorf("invalid per-model = %q, want medium (global default)", got)
	}
	// Hand-edited values can carry case; the request path compares exactly, so
	// the lookup normalizes to the lowercase ladder form.
	casey := &Settings{Models: map[string]ModelSettings{
		"anthropic/claude-opus-4-6": {ThinkingLevel: "High"},
	}}
	if got := casey.ThinkingLevelFor("anthropic/claude-opus-4-6"); got != "high" {
		t.Errorf("normalized = %q, want high", got)
	}

	// No per-model and an invalid global default leave nothing configured.
	empty := &Settings{DefaultThinkingLevel: "sometimes"}
	if got := empty.ThinkingLevelFor("any/model"); got != "" {
		t.Errorf("invalid global = %q, want empty", got)
	}
	var nilSettings *Settings
	if got := nilSettings.ThinkingLevelFor("any/model"); got != "" {
		t.Errorf("nil settings = %q, want empty", got)
	}
}

// TestDirectoryOverrideFieldsRoundTrip verifies the per-directory override
// fields parse from JSON, merge project-over-user, and save back.
func TestDirectoryOverrideFieldsRoundTrip(t *testing.T) {
	raw := `{
		"sessionDir": "/sessions",
		"packageDir": "/pkgs",
		"extensionDir": "/ext",
		"skillDir": "/skills",
		"promptDir": "/prompts",
		"themeDir": "/themes"
	}`
	user, err := ParseSettings([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if user.SessionDir != "/sessions" || user.PackageDir != "/pkgs" || user.ExtensionDir != "/ext" ||
		user.SkillDir != "/skills" || user.PromptDir != "/prompts" || user.ThemeDir != "/themes" {
		t.Fatalf("parsed fields: %+v", user)
	}

	project := &Settings{SkillDir: "/project-skills"}
	merged := MergeSettings(user, project)
	if merged.SkillDir != "/project-skills" {
		t.Fatalf("project should override skillsDir: %q", merged.SkillDir)
	}
	if merged.SessionDir != "/sessions" || merged.PackageDir != "/pkgs" {
		t.Fatalf("other fields should survive merge: %+v", merged)
	}

	path := filepath.Join(t.TempDir(), "settings.json")
	if err := SaveSettings(path, merged); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.SkillDir != "/project-skills" || reloaded.ExtensionDir != "/ext" {
		t.Fatalf("round-trip lost fields: %+v", reloaded)
	}
}

func TestDefaultToolsLayerModifiers(t *testing.T) {
	user := &Settings{DefaultTools: []string{"read", "bash"}}
	merged := MergeSettings(user, &Settings{DefaultTools: []string{"-bash", "+grep"}})
	if got := merged.DefaultTools; !slices.Equal(got, []string{"read", "bash", "-bash", "+grep"}) {
		t.Fatalf("merged tools: %v", got)
	}
	replaced := MergeSettings(user, &Settings{DefaultTools: []string{}})
	if replaced.DefaultTools == nil || len(replaced.DefaultTools) != 0 {
		t.Fatalf("explicit empty selection lost: %v", replaced.DefaultTools)
	}
}

func TestDefaultToolsEmptySelectionRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := SaveSettings(path, &Settings{DefaultTools: []string{}}); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.DefaultTools == nil || len(settings.DefaultTools) != 0 {
		t.Fatalf("empty tool selection lost: %v", settings.DefaultTools)
	}
}

func TestPerModelCompactionSettings(t *testing.T) {
	settings := &Settings{Compaction: CompactionSettings{ReserveTokens: 100, KeepRecentTokens: 200, ModelOverrides: map[string]CompactionModelOverride{"test/model": {ReserveTokens: new(0), KeepRecentTokens: new(50)}}}}
	resolved, err := settings.ResolveCompaction(&Model{Provider: "test", ID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ReserveTokens != 0 || resolved.KeepRecentTokens != 50 {
		t.Fatalf("resolved: %#v", resolved)
	}
	settings.Compaction.ModelOverrides["test/model"] = CompactionModelOverride{ReserveTokens: new(-1)}
	if _, err := settings.ResolveCompaction(&Model{Provider: "test", ID: "model"}); err == nil {
		t.Fatal("negative override accepted")
	}
}

func TestExplicitRetryAndCompactionSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"retry":{"enabled":false,"maxRetries":0,"baseDelayMs":0},"compaction":{"enabled":false,"reserveTokens":0,"keepRecentTokens":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	merged := MergeSettings(&Settings{Retry: RetrySettings{Enabled: true, MaxRetries: 5, BaseDelayMs: 10}, Compaction: CompactionSettings{Enabled: true, ReserveTokens: 100, KeepRecentTokens: 100}}, project)
	retry := merged.ResolveRetry()
	if retry.Enabled || retry.MaxRetries != 0 || retry.BaseDelayMs != 0 {
		t.Fatalf("retry=%+v", retry)
	}
	limits, err := merged.ResolveCompaction(nil)
	if err != nil {
		t.Fatal(err)
	}
	if limits.Enabled || limits.ReserveTokens != 0 || limits.KeepRecentTokens != 0 {
		t.Fatalf("compaction=%+v", limits)
	}
	if err := SaveSettings(path, merged); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ResolveRetry().Enabled || restored.ResolveRetry().MaxRetries != 0 {
		t.Fatal("explicit values lost on save")
	}
	defaults := (&Settings{}).ResolveRetry()
	if !defaults.Enabled || defaults.MaxRetries != 3 {
		t.Fatalf("defaults=%+v", defaults)
	}
}

func TestPresentSettingsSerializeCurrentValues(t *testing.T) {
	var settings Settings
	if err := json.Unmarshal([]byte(`{"retry":{"enabled":true,"maxRetries":4},"compaction":{"enabled":true,"keepRecentTokens":100}}`), &settings); err != nil {
		t.Fatal(err)
	}
	settings.Retry.Enabled = false
	settings.Retry.MaxRetries = 0
	settings.Compaction.Enabled = false
	settings.Compaction.KeepRecentTokens = 0
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	var restored Settings
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if retry := restored.ResolveRetry(); retry.Enabled || retry.MaxRetries != 0 {
		t.Fatalf("mutated retry values lost: %s", data)
	}
	limits, err := restored.ResolveCompaction(nil)
	if err != nil {
		t.Fatal(err)
	}
	if limits.Enabled || limits.KeepRecentTokens != 0 {
		t.Fatalf("mutated compaction values lost: %s", data)
	}
}

func TestMergeSettingsExplicitImageAndTerminalFalse(t *testing.T) {
	user, err := parseSettings([]byte(`{"images":{"blockImages":true},"terminal":{"clearOnShrink":true}}`))
	require.NoError(t, err)
	project, err := parseSettings([]byte(`{"images":{"blockImages":false},"terminal":{"clearOnShrink":false}}`))
	require.NoError(t, err)
	merged := MergeSettings(user, project)
	assert.False(t, merged.Images.BlockImages)
	assert.False(t, merged.Terminal.ClearOnShrink)
	data, err := json.Marshal(merged)
	require.NoError(t, err)
	roundTrip, err := parseSettings(data)
	require.NoError(t, err)
	merged = MergeSettings(user, roundTrip)
	assert.False(t, merged.Images.BlockImages)
	assert.False(t, merged.Terminal.ClearOnShrink)
}

func TestMergeSettingsExplicitTopLevelValues(t *testing.T) {
	user, err := parseSettings([]byte(`{"hideThinkingBlock":true,"quietStartup":true,"collapseChangelog":true,"showHardwareCursor":true,"enableSkillCommands":true,"enableInstallTelemetry":true,"editorPaddingX":4,"autocompleteMaxVisible":8,"shellCommandPrefix":"sandbox","theme":"dark","skills":["user-skill"]}`))
	require.NoError(t, err)
	project, err := parseSettings([]byte(`{"hideThinkingBlock":false,"quietStartup":false,"collapseChangelog":false,"showHardwareCursor":false,"enableSkillCommands":false,"editorPaddingX":0,"autocompleteMaxVisible":0,"shellCommandPrefix":"","theme":"","skills":[]}`))
	require.NoError(t, err)
	check := func(t *testing.T, merged *Settings) {
		t.Helper()
		assert.False(t, merged.HideThinkingBlock)
		assert.False(t, merged.QuietStartup)
		assert.False(t, merged.CollapseChangelog)
		assert.False(t, merged.ShowHardwareCursor)
		assert.False(t, merged.EnableSkillCommands)
		assert.True(t, merged.EnableInstallTelemetry, "absent project field must preserve user preference")
		assert.Zero(t, merged.EditorPaddingX)
		assert.Zero(t, merged.AutocompleteMaxVisible)
		assert.Empty(t, merged.ShellCommandPrefix)
		assert.Empty(t, merged.Theme)
		require.NotNil(t, merged.Skills)
		assert.Empty(t, merged.Skills)
	}
	check(t, MergeSettings(user, project))
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, SaveSettings(path, project))
	reloaded, err := LoadSettings(path)
	require.NoError(t, err)
	check(t, MergeSettings(user, reloaded))
	merged := MergeSettings(user, &Settings{})
	assert.True(t, merged.EnableInstallTelemetry)
	assert.True(t, merged.HideThinkingBlock)
}

func TestMergeSettingsExplicitNestedReset(t *testing.T) {
	user, err := parseSettings([]byte(`{"bash":{"commandPrefix":"sandbox","envAllowlist":["HOME"],"envExtra":{"MODE":"safe"},"outputCap":100},"read":{"lineCap":12},"markdown":{"codeBlockIndent":"  "},"branchSummary":{"skipPrompt":true,"reserveTokens":100},"images":{"maxWidth":400}}`))
	require.NoError(t, err)
	project, err := parseSettings([]byte(`{"bash":{"commandPrefix":"","envAllowlist":[],"envExtra":{},"outputCap":0},"read":{"lineCap":0},"markdown":{"codeBlockIndent":""},"branchSummary":{"skipPrompt":false,"reserveTokens":0},"images":{"maxWidth":0}}`))
	require.NoError(t, err)
	check := func(merged *Settings) {
		t.Helper()
		assert.Empty(t, merged.Bash.CommandPrefix)
		assert.Empty(t, merged.Bash.EnvAllowlist)
		assert.Empty(t, merged.Bash.EnvExtra)
		assert.Zero(t, merged.Bash.OutputCap)
		assert.Zero(t, merged.Read.LineCap)
		assert.Empty(t, merged.Markdown.CodeBlockIndent)
		assert.False(t, merged.BranchSummary.SkipPrompt)
		assert.Zero(t, merged.BranchSummary.ReserveTokens)
		assert.Zero(t, merged.Images.MaxWidth)
	}
	check(MergeSettings(user, project))
	data, err := json.Marshal(project)
	require.NoError(t, err)
	project, err = parseSettings(data)
	require.NoError(t, err)
	check(MergeSettings(user, project))
}

func TestMergeSettingsDoesNotMutateModelOverrides(t *testing.T) {
	user := &Settings{Models: map[string]ModelSettings{"test/model": {ThinkingLevel: "high"}}}
	project := &Settings{Models: map[string]ModelSettings{"test/model": {ThinkingLevel: "low"}}}
	merged := MergeSettings(user, project)
	assert.Equal(t, "high", user.Models["test/model"].ThinkingLevel)
	merged.Models["test/model"] = ModelSettings{ThinkingLevel: "off"}
	assert.Equal(t, "low", project.Models["test/model"].ThinkingLevel)
	assert.Equal(t, "high", user.Models["test/model"].ThinkingLevel)
}

func TestThinkingBudgetsExplicitZeroSurvivesMergeAndSave(t *testing.T) {
	project, err := parseSettings([]byte(`{"thinkingBudgets":{"medium":0}}`))
	require.NoError(t, err)
	merged := MergeSettings(&Settings{ThinkingBudgets: ThinkingBudgetsSettings{Medium: 1234, High: 4321}}, project)
	budgets := merged.ResolveThinkingBudgets()
	require.NotNil(t, budgets.Medium)
	assert.Zero(t, *budgets.Medium)
	require.NotNil(t, budgets.High)
	assert.Equal(t, 4321, *budgets.High)
	data, err := json.Marshal(merged)
	require.NoError(t, err)
	loaded, err := parseSettings(data)
	require.NoError(t, err)
	require.NotNil(t, loaded.ResolveThinkingBudgets().Medium)
	assert.Zero(t, *loaded.ResolveThinkingBudgets().Medium)
}

func TestParseSettingsMigratesLegacyAzureProviderKey(t *testing.T) {
	settings, err := parseSettings([]byte(`{
		"defaultProvider": "azure-openai-responses",
		"enabledModels": ["azure-openai-responses/gpt-5.4:high", "azure-openai-responses/*", "anthropic/*"],
		"models": {"azure-openai-responses/gpt-5.4": {"thinkingLevel": "low"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if settings.DefaultProvider != ProviderAzureOpenAI {
		t.Errorf("defaultProvider = %q", settings.DefaultProvider)
	}
	if want := []string{"azure/gpt-5.4:high", "azure/*", "anthropic/*"}; !slices.Equal(settings.EnabledModels, want) {
		t.Errorf("enabledModels = %q, want %q", settings.EnabledModels, want)
	}
	if settings.Models["azure/gpt-5.4"].ThinkingLevel != "low" || len(settings.Models) != 1 {
		t.Errorf("models = %+v", settings.Models)
	}
}

func TestSettingsResourcePathsKeepScopeAndRawValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	load := func(dir, contents string) *Settings {
		t.Helper()
		require.NoError(t, os.MkdirAll(dir, 0o755))
		path := filepath.Join(dir, "settings.json")
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
		s, err := LoadSettings(path)
		require.NoError(t, err)
		return s
	}
	userDir := filepath.Join(home, "agent")
	projectDir := filepath.Join(t.TempDir(), ".tau")
	user := load(userDir, `{"skills":["skills","!~/shared/*","+./skills/keep","-./skills/drop"],"themes":["themes"],"extensions":["extensions"]}`)
	project := load(projectDir, `{"prompts":["prompts","!prompts/skip*","+~/shared/keep.md","-~/shared/drop.md"]}`)
	merged := MergeSettings(user, project)
	assert.Equal(t, []string{
		filepath.Join(userDir, "skills"), "!" + filepath.Join(home, "shared", "*"),
		"+" + filepath.Join(userDir, "skills", "keep"), "-" + filepath.Join(userDir, "skills", "drop"),
	}, merged.ResourcePaths("skills"))
	assert.Equal(t, []string{
		filepath.Join(projectDir, "prompts"), "!" + filepath.Join(projectDir, "prompts", "skip*"),
		"+" + filepath.Join(home, "shared", "keep.md"), "-" + filepath.Join(home, "shared", "drop.md"),
	}, merged.ResourcePaths("prompts"))
	assert.Equal(t, []string{filepath.Join(userDir, "extensions")}, merged.ResourcePaths("extensions"))
	assert.Equal(t, []string{filepath.Join(userDir, "themes")}, merged.ResourcePaths("themes"))
	assert.Equal(t, user.Skills, merged.Skills)
	data, err := json.Marshal(merged)
	require.NoError(t, err)
	assert.NotContains(t, string(data), userDir)
	assert.NotContains(t, string(data), projectDir)
	assert.Contains(t, string(data), "!~/shared/*")
	assert.Equal(t, user.ResourcePaths("skills"), MergeSettings(user, load(projectDir, `{"skills":[]}`)).ResourcePaths("skills"))
}

func TestRetrySettingsSeparateAgentAndProviderLimits(t *testing.T) {
	user, err := ParseSettings([]byte(`{"retry":{"maxDelayMs":45000,"provider":{"timeoutMs":30000},"maxAgentDelayMs":7}}`))
	require.NoError(t, err)
	project, err := ParseSettings([]byte(`{"retry":{"provider":{"maxRetries":0,"maxRetryDelayMs":0},"maxAgentDelayMs":0,"baseDelayMs":0}}`))
	require.NoError(t, err)
	merged := MergeSettings(user, project)
	data, err := json.Marshal(merged)
	require.NoError(t, err)
	require.NotContains(t, string(data), `"maxDelayMs"`)
	reloaded, err := ParseSettings(data)
	require.NoError(t, err)
	resolved := reloaded.ResolveRetry()
	assert.Zero(t, resolved.MaxAgentDelayMs)
	assert.Zero(t, resolved.BaseDelayMs)
	require.NotNil(t, resolved.Provider.TimeoutMs)
	require.NotNil(t, resolved.Provider.MaxRetries)
	require.NotNil(t, resolved.Provider.MaxRetryDelayMs)
	assert.Equal(t, 30000, *resolved.Provider.TimeoutMs)
	assert.Zero(t, *resolved.Provider.MaxRetries)
	assert.Zero(t, *resolved.Provider.MaxRetryDelayMs)
	legacy := user.ResolveRetry()
	assert.Equal(t, 7, legacy.MaxAgentDelayMs)
	assert.Equal(t, 45000, *legacy.Provider.MaxRetryDelayMs)
	defaults := (*Settings)(nil).ResolveRetry()
	assert.Equal(t, 60000, defaults.MaxAgentDelayMs)
	assert.Equal(t, 60000, *defaults.Provider.MaxRetryDelayMs)
}

func TestRetrySettingsLegacyLimitDoesNotOverrideExplicitProviderLimit(t *testing.T) {
	settings, err := ParseSettings([]byte(`{"retry":{"maxDelayMs":90000,"provider":{"maxRetryDelayMs":0}}}`))
	require.NoError(t, err)
	assert.Zero(t, *settings.ResolveRetry().Provider.MaxRetryDelayMs)
	assert.Equal(t, 60000, settings.ResolveRetry().MaxAgentDelayMs)
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, SaveSettings(path, settings))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"maxDelayMs"`)
	restored, err := LoadSettings(path)
	require.NoError(t, err)
	require.NotNil(t, restored.ResolveRetry().Provider.MaxRetryDelayMs)
	assert.Zero(t, *restored.ResolveRetry().Provider.MaxRetryDelayMs)
}

func TestRetrySettingsLegacyNullUsesDefaultProviderLimit(t *testing.T) {
	settings, err := ParseSettings([]byte(`{"retry":{"maxDelayMs":null}}`))
	require.NoError(t, err)
	assert.Equal(t, 60000, *settings.ResolveRetry().Provider.MaxRetryDelayMs)
}

func TestConcurrentSettingsLoadAndSavePreserveKeys(t *testing.T) {
	dir := t.TempDir()
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range 32 {
		workers.Go(func() {
			<-start
			var settings *Settings
			if i%2 == 0 {
				parsed, err := ParseSettings([]byte(`{"defaultModel":"configured","quietStartup":false,"editorPaddingX":0,"extensionSetting":{"enabled":true}}`))
				if err != nil {
					t.Error(err)
					return
				}
				settings = parsed
			} else {
				settings = &Settings{DefaultModel: "configured", Plugins: map[string]json.RawMessage{
					"defaultModel":     json.RawMessage(`"must not override"`),
					"extensionSetting": json.RawMessage(`{"enabled":true}`),
				}}
			}
			path := filepath.Join(dir, fmt.Sprintf("%d.json", i))
			if err := SaveSettings(path, settings); err != nil {
				t.Error(err)
				return
			}
			restored, err := LoadSettings(path)
			if err != nil {
				t.Error(err)
				return
			}
			if restored.DefaultModel != "configured" {
				t.Errorf("typed key overridden: %q", restored.DefaultModel)
			}
			if _, ok := restored.Plugins["defaultModel"]; i%2 == 0 && ok {
				t.Error("typed key preserved as an extension")
			}
			var extension struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.Unmarshal(restored.Plugins["extensionSetting"], &extension); err != nil {
				t.Error(err)
				return
			}
			if !extension.Enabled {
				t.Error("extension key lost")
			}
		})
	}
	close(start)
	workers.Wait()
}

func TestSettingsRuntimeDefaultsMatchUpstream(t *testing.T) {
	parse := func(raw string) *Settings {
		t.Helper()
		settings, err := ParseSettings([]byte(raw))
		require.NoError(t, err)
		return settings
	}

	t.Run("transport", func(t *testing.T) {
		for raw, want := range map[string]Transport{
			`{}`:                                     TransportAuto,
			`{"transport":"sse"}`:                    TransportSSE,
			`{"transport":"websocket-cached"}`:       TransportWS,
			`{"websockets":true}`:                    TransportWS,
			`{"websockets":false}`:                   TransportSSE,
			`{"transport":"auto","websockets":true}`: TransportAuto,
		} {
			got, err := parse(raw).ResolveTransport()
			require.NoError(t, err, raw)
			assert.Equal(t, want, got, raw)
		}
		migrated := parse(`{"websockets":true}`)
		assert.NotContains(t, migrated.Plugins, "websockets")
		assert.Equal(t, "websocket", migrated.Transport)
		_, err := parse(`{"transport":"carrier-pigeon"}`).ResolveTransport()
		require.ErrorContains(t, err, `invalid transport "carrier-pigeon"`)
		got, err := (*Settings)(nil).ResolveTransport()
		require.NoError(t, err)
		assert.Equal(t, TransportAuto, got)
	})

	t.Run("skill commands", func(t *testing.T) {
		assert.True(t, parse(`{}`).SkillCommandsEnabled())
		assert.True(t, (*Settings)(nil).SkillCommandsEnabled())
		assert.False(t, parse(`{"enableSkillCommands":false}`).SkillCommandsEnabled())
		assert.False(t, MergeSettings(parse(`{"enableSkillCommands":true}`), parse(`{"enableSkillCommands":false}`)).SkillCommandsEnabled())
		assert.True(t, MergeSettings(parse(`{"enableSkillCommands":false}`), parse(`{"enableSkillCommands":true}`)).SkillCommandsEnabled())
	})

	t.Run("code block indent", func(t *testing.T) {
		assert.Equal(t, "  ", parse(`{}`).ResolveCodeBlockIndent())
		assert.Equal(t, "  ", (*Settings)(nil).ResolveCodeBlockIndent())
		assert.Equal(t, "\t", parse(`{"markdown":{"codeBlockIndent":"\t"}}`).ResolveCodeBlockIndent())
		empty := parse(`{"markdown":{"codeBlockIndent":""}}`)
		assert.Empty(t, empty.ResolveCodeBlockIndent())
		assert.Empty(t, MergeSettings(parse(`{"markdown":{"codeBlockIndent":"    "}}`), empty).ResolveCodeBlockIndent())
		path := filepath.Join(t.TempDir(), "settings.json")
		require.NoError(t, SaveSettings(path, empty))
		loaded, err := LoadSettings(path)
		require.NoError(t, err)
		assert.Empty(t, loaded.ResolveCodeBlockIndent())
	})
}

func TestFullscreenWheelSettingsBoundsAndRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{`{}`, 0},
		{`{"fullscreenWheelScrollLines":"auto"}`, 0},
		{`{"fullscreenWheelScrollLines":null}`, 0},
		{`{"fullscreenWheelScrollLines":0}`, 1},
		{`{"fullscreenWheelScrollLines":-2}`, 1},
		{`{"fullscreenWheelScrollLines":2.9}`, 2},
		{`{"fullscreenWheelScrollLines":999}`, 100},
		{`{"fullscreenWheelScrollLines":"invalid"}`, 0},
	} {
		s, err := ParseSettings([]byte(tc.raw))
		require.NoError(t, err)
		assert.Equal(t, tc.want, s.ResolveFullscreenWheelScrollLines(), tc.raw)
		path := filepath.Join(t.TempDir(), "settings.json")
		require.NoError(t, SaveSettings(path, s))
		restored, err := LoadSettings(path)
		require.NoError(t, err)
		assert.Equal(t, tc.want, restored.ResolveFullscreenWheelScrollLines(), tc.raw)
	}
	user, err := ParseSettings([]byte(`{"fullscreenWheelScrollLines":7}`))
	require.NoError(t, err)
	project, err := ParseSettings([]byte(`{"fullscreenWheelScrollLines":0}`))
	require.NoError(t, err)
	assert.Equal(t, 1, MergeSettings(user, project).ResolveFullscreenWheelScrollLines())
	assert.Equal(t, 7, MergeSettings(user, &Settings{}).ResolveFullscreenWheelScrollLines())
}

func TestSettingsValueMarshalKeepsExplicitZeros(t *testing.T) {
	var settings Settings
	if err := json.Unmarshal([]byte(`{"hideThinkingBlock":false,"retry":{"enabled":false}}`), &settings); err != nil {
		t.Fatal(err)
	}
	byValue, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	byPointer, err := json.Marshal(&settings)
	if err != nil {
		t.Fatal(err)
	}
	if string(byValue) != string(byPointer) || !strings.Contains(string(byValue), `"hideThinkingBlock":false`) {
		t.Fatalf("value=%s pointer=%s", byValue, byPointer)
	}
}

func TestParseSettingsMigratesLegacyShapesBeforeTypedDecode(t *testing.T) {
	for _, tc := range []struct {
		name, input, mode string
		skills            []string
		commands          bool
	}{
		{name: "queue", input: `{"queueMode":"one-at-a-time"}`, mode: "one-at-a-time"},
		{name: "canonical queue wins", input: `{"queueMode":"all","steeringMode":"one-at-a-time"}`, mode: "one-at-a-time"},
		{name: "canonical empty queue wins", input: `{"queueMode":"all","steeringMode":""}`},
		{name: "skills", input: `{"skills":{"enableSkillCommands":true,"customDirectories":["~/skills","!private"]}}`, skills: []string{"~/skills", "!private"}, commands: true},
		{name: "canonical false commands wins", input: `{"enableSkillCommands":false,"skills":{"enableSkillCommands":true,"customDirectories":["./skills"]}}`, skills: []string{"./skills"}},
		{name: "empty legacy skills", input: `{"skills":{"enableSkillCommands":false,"customDirectories":[]}}`},
		{name: "absent legacy directories", input: `{"skills":{"enableSkillCommands":true}}`, commands: true},
		{name: "nonarray legacy directories", input: `{"skills":{"customDirectories":"ignored"}}`},
		{name: "modern skills", input: `{"skills":["./skills"],"enableSkillCommands":false}`, skills: []string{"./skills"}},
	} {
		for _, bom := range []string{"", "\xef\xbb\xbf"} {
			t.Run(fmt.Sprintf("%s/BOM=%t", tc.name, bom != ""), func(t *testing.T) {
				settings, err := ParseSettings([]byte(bom + tc.input))
				require.NoError(t, err)
				assert.Equal(t, tc.mode, settings.SteeringMode)
				assert.Equal(t, tc.skills, settings.Skills)
				assert.Equal(t, tc.commands, settings.EnableSkillCommands)
				if !strings.Contains(tc.name, "canonical") {
					assert.NotContains(t, settings.Plugins, "queueMode")
				}
				path := filepath.Join(t.TempDir(), "settings.json")
				require.NoError(t, SaveSettings(path, settings))
				reloaded, err := LoadSettings(path)
				require.NoError(t, err)
				assert.Equal(t, settings.SteeringMode, reloaded.SteeringMode)
				assert.Equal(t, settings.Skills, reloaded.Skills)
				assert.Equal(t, settings.EnableSkillCommands, reloaded.EnableSkillCommands)
				encoded, err := os.ReadFile(path)
				require.NoError(t, err)
				var fields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(encoded, &fields))
				if strings.Contains(tc.input, `"enableSkillCommands":false`) {
					assert.JSONEq(t, "false", string(fields["enableSkillCommands"]))
				}
			})
		}
	}
}

func TestSaveSettingsReplacesFileWithoutTruncatingReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(`{"theme":"old","extension":{"enabled":true}}`)
	require.NoError(t, os.WriteFile(path, original, 0o600))
	reader, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	require.NoError(t, SaveSettings(path, &Settings{Theme: "new"}))
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, string(original), string(data), "readers admitted before replacement must retain the original file")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSettingsFieldUpdatesPreserveUnrelatedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte("\xef\xbb\xbf"+`{"theme":"old","retry":{"enabled":true,"provider":{"maxRetries":5,"future":9007199254740993}},"models":{"p/m":{"thinkingLevel":"low","future":false}},"extension":{"id":9007199254740993}}`), 0o600))
	require.NoError(t, UpdateSettings(t.Context(), path, map[string]any{
		"theme": "", "quietStartup": false, "skills": []string{},
		"retry":  map[string]any{"provider": map[string]any{"maxRetries": 0}},
		"models": map[string]any{"p/m": map[string]any{"thinkingLevel": "high"}},
	}))
	require.NoError(t, SaveDefaultModel(path, "anthropic/example"))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "9007199254740993")
	assert.NotContains(t, string(data), "9007199254740992")
	assert.JSONEq(t, `{"theme":"","quietStartup":false,"skills":[],"retry":{"enabled":true,"provider":{"maxRetries":0,"future":9007199254740993}},"models":{"p/m":{"thinkingLevel":"high","future":false}},"extension":{"id":9007199254740993},"defaultProvider":"anthropic","defaultModel":"example"}`, string(data))
}

func TestSettingsUpdatesSerializeSameFileWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range 24 {
		workers.Go(func() {
			<-start
			if err := UpdateSettings(t.Context(), path, map[string]any{"extension": map[string]any{fmt.Sprintf("worker%d", i): i}}); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	workers.Wait()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var saved struct{ Extension map[string]int }
	require.NoError(t, json.Unmarshal(data, &saved))
	require.Len(t, saved.Extension, 24)
	for i := range 24 {
		assert.Equal(t, i, saved.Extension[fmt.Sprintf("worker%d", i)])
	}
}

func TestSettingsUpdateFailureRetainsOriginalFile(t *testing.T) {
	for _, original := range []string{`{"theme":`, `{"retry":{"enabled":"invalid"}}`, `{"default":"legacy"}`} {
		t.Run(original, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
			require.Error(t, UpdateSettings(t.Context(), path, map[string]any{"theme": "new"}))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, original, string(data))
		})
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"theme":"old"}`), 0o600))
	for _, patch := range []map[string]any{{"retry": map[string]any{"enabled": "invalid"}}, {"theme": make(chan int)}} {
		require.Error(t, UpdateSettings(t.Context(), path, patch))
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.JSONEq(t, `{"theme":"old"}`, string(data))
	}
	entries, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".settings-*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestSettingsUpdateErrorNamesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"theme":`), 0o600))
	err := UpdateSettings(t.Context(), path, map[string]any{"theme": "new"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), path, "error does not name the file the user must repair")
}

func TestSettingsUpdateCancellationWhileWaitingForLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"theme":"old"}`), 0o600))
	lock := flock.New(path + ".lock")
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, UpdateSettings(ctx, path, map[string]any{"theme": "new"}), context.DeadlineExceeded)
	require.NoError(t, lock.Unlock())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"theme":"old"}`, string(data))
}

func TestReplaceSettingsCleansUpUncommittedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"theme":"old"}`), 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, replaceSettings(ctx, path, []byte(`{"theme":"new"}`)), context.Canceled)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"theme":"old"}`, string(data))
	entries, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".settings-*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, entries)
	blocked := filepath.Join(t.TempDir(), "directory")
	require.NoError(t, os.Mkdir(blocked, 0o755))
	require.Error(t, replaceSettings(t.Context(), blocked, []byte("{}")))
	entries, err = filepath.Glob(filepath.Join(filepath.Dir(blocked), ".settings-*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestResourceListsRetainIndependentScopes(t *testing.T) {
	root := t.TempDir()
	load := func(scope, contents string) *Settings {
		t.Helper()
		dir := filepath.Join(root, scope)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		path := filepath.Join(dir, "settings.json")
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
		settings, err := LoadSettings(path)
		require.NoError(t, err)
		return settings
	}
	user := load("user", `{"skills":["extra"],"prompts":["extra"],"themes":["extra"],"extensions":["extra"]}`)
	project := load("project", `{"skills":["extra"],"prompts":["extra"],"themes":["extra"],"extensions":["extra"]}`)
	for _, kind := range []string{"skills", "prompts", "themes", "extensions"} {
		assert.Equal(t, []string{filepath.Join(root, "project", "extra"), filepath.Join(root, "user", "extra")}, MergeSettings(user, project).ResourcePaths(kind), kind)
		merged := MergeSettings(user, project)
		userPaths, projectPaths := merged.ResourcePathsByScope(kind)
		assert.Equal(t, []string{filepath.Join(root, "user", "extra")}, userPaths)
		assert.Equal(t, []string{filepath.Join(root, "project", "extra")}, projectPaths)
		userPaths[0], projectPaths[0] = "mutated-user", "mutated-project"
		assert.Equal(t, merged.ResourcePaths(kind), MergeSettings(merged, &Settings{}).ResourcePaths(kind))
		assert.Equal(t, []string{filepath.Join(root, "user", "extra")}, MergeSettings(user, load("empty", `{"skills":[],"prompts":[],"themes":[],"extensions":[]}`)).ResourcePaths(kind), kind)
	}
}

func TestPackageSourcePreservesExplicitFiltersAndAutoload(t *testing.T) {
	for _, raw := range []string{
		`"./local"`,
		`{"source":"npm:@scope/package@1","extensions":[],"skills":[],"prompts":[],"themes":[]}`,
		`{"source":"./local","autoload":false}`,
		`{"source":"./local","autoload":true,"prompts":["prompts/*","!prompts/private/*","+prompts/keep.md","-prompts/drop.md"]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			settings, err := ParseSettings([]byte(`{"packages":[` + raw + `]}`))
			require.NoError(t, err)
			encoded, err := json.Marshal(settings)
			require.NoError(t, err)
			var saved map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(encoded, &saved))
			assert.JSONEq(t, `[`+raw+`]`, string(saved["packages"]))
			path := filepath.Join(t.TempDir(), "settings.json")
			require.NoError(t, SaveSettings(path, settings))
			loaded, err := LoadSettings(path)
			require.NoError(t, err)
			assert.Equal(t, settings.Packages, loaded.Packages)
		})
	}
}

func TestPackageSourceNullFiltersAndReuseReset(t *testing.T) {
	var source PackageSource
	require.NoError(t, json.Unmarshal([]byte(`{"source":"./local","autoload":false,"prompts":[]}`), &source))
	require.NotNil(t, source.Autoload)
	require.NotNil(t, source.Prompts)
	require.NoError(t, json.Unmarshal([]byte(`{"source":"./other","autoload":null,"prompts":null}`), &source))
	assert.Nil(t, source.Autoload)
	assert.Nil(t, source.Prompts)
	assert.Equal(t, "./other", source.Source)
	require.NoError(t, json.Unmarshal([]byte(`"./string"`), &source))
	assert.Equal(t, PackageSource{Source: "./string"}, source)
}

func TestSettingsPackageScopesPreserveSourcesAndOwnership(t *testing.T) {
	load := func(dir, contents string) *Settings {
		t.Helper()
		require.NoError(t, os.MkdirAll(dir, 0o700))
		path := filepath.Join(dir, "settings.json")
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
		s, err := LoadSettings(path)
		require.NoError(t, err)
		return s
	}
	userDir, projectDir := t.TempDir(), t.TempDir()
	user := load(userDir, `{"packages":[{"source":"./local","autoload":false,"prompts":[]},"npm:@scope/example@1"]}`)
	project := load(projectDir, `{"packages":[{"source":"../shared","skills":["+skills/keep"]}]}`)
	merged := MergeSettings(user, project)
	u, p := merged.PackageSourcesByScope()
	assert.Equal(t, userDir, u.BaseDir)
	assert.Equal(t, projectDir, p.BaseDir)
	assert.Equal(t, user.Packages, u.Sources)
	assert.Equal(t, project.Packages, p.Sources)
	assert.Equal(t, project.Packages, merged.Packages)
	*u.Sources[0].Autoload = true
	p.Sources[0].Skills[0] = "mutated"
	u.Sources[0].Source = "mutated"
	u, p = merged.PackageSourcesByScope()
	assert.Equal(t, "./local", u.Sources[0].Source)
	assert.False(t, *u.Sources[0].Autoload)
	assert.NotNil(t, u.Sources[0].Prompts)
	assert.Equal(t, []string{"+skills/keep"}, p.Sources[0].Skills)
	data, err := json.Marshal(merged)
	require.NoError(t, err)
	assert.NotContains(t, string(data), userDir)
	assert.NotContains(t, string(data), projectDir)
	assert.Contains(t, string(data), "../shared")
	u, p = MergeSettings(user, load(projectDir, `{"packages":[]}`)).PackageSourcesByScope()
	assert.Len(t, u.Sources, 2)
	assert.NotNil(t, p.Sources)
	assert.Empty(t, p.Sources)
	u, p = MergeSettings(merged, nil).PackageSourcesByScope()
	assert.Equal(t, user.Packages, u.Sources)
	assert.Equal(t, project.Packages, p.Sources)
}

func TestMergeSettingsOwnsPluginJSON(t *testing.T) {
	user := &Settings{Plugins: map[string]json.RawMessage{"user": json.RawMessage(`{"value":1}`)}}
	project := &Settings{Plugins: map[string]json.RawMessage{"project": json.RawMessage(`{"value":2}`)}}
	merged := MergeSettings(user, project)
	merged.Plugins["user"][9] = '3'
	merged.Plugins["project"][9] = '4'
	assert.JSONEq(t, `{"value":1}`, string(user.Plugins["user"]))
	assert.JSONEq(t, `{"value":2}`, string(project.Plugins["project"]))
	user.Plugins["user"][9] = '5'
	project.Plugins["project"][9] = '6'
	assert.JSONEq(t, `{"value":3}`, string(merged.Plugins["user"]))
	assert.JSONEq(t, `{"value":4}`, string(merged.Plugins["project"]))
}

func TestMergeSettingsOwnsLists(t *testing.T) {
	for _, scope := range []string{"user", "project"} {
		t.Run(scope, func(t *testing.T) {
			source, err := parseSettings([]byte(`{"packages":[{"source":"./extension","extensions":["index.js"],"autoload":true}],"extensions":["extension"],"skills":["skill"],"prompts":["prompt"],"themes":["theme"],"npmCommand":["npm"],"enabledModels":["provider/model"],"defaultTools":["read"],"fullscreenWheelScrollLines":3}`))
			require.NoError(t, err)
			before, err := json.Marshal(source)
			require.NoError(t, err)
			var merged *Settings
			if scope == "user" {
				merged = MergeSettings(source, nil)
			} else {
				merged = MergeSettings(nil, source)
			}
			for _, list := range [][]string{merged.Extensions, merged.Skills, merged.Prompts, merged.Themes, merged.NPMCommand, merged.EnabledModels, merged.DefaultTools} {
				list[0] = "changed"
			}
			merged.Packages[0].Source = "changed"
			merged.Packages[0].Extensions[0] = "changed.js"
			*merged.Packages[0].Autoload = false
			merged.FullscreenWheelScrollLines[0] = '7'
			after, err := json.Marshal(source)
			require.NoError(t, err)
			assert.JSONEq(t, string(before), string(after))
		})
	}
}

func TestMergeSettingsOwnsNestedRuntimeGroups(t *testing.T) {
	for _, scope := range []string{"user", "project"} {
		t.Run(scope, func(t *testing.T) {
			source, err := parseSettings([]byte(`{"bash":{"envAllowlist":["HOME"],"envExtra":{"MODE":"safe"}},"terminal":{"showImages":true},"images":{"autoResize":true},"retry":{"provider":{"timeoutMs":10,"maxRetries":2,"maxRetryDelayMs":20}},"compaction":{"modelOverrides":{"p/m":{"reserveTokens":30,"keepRecentTokens":40}}}}`))
			require.NoError(t, err)
			before, err := json.Marshal(source)
			require.NoError(t, err)
			var merged *Settings
			if scope == "user" {
				merged = MergeSettings(source, nil)
			} else {
				merged = MergeSettings(nil, source)
			}
			merged.Bash.EnvAllowlist[0] = "OTHER"
			merged.Bash.EnvExtra["MODE"] = "unsafe"
			*merged.Terminal.ShowImages = false
			*merged.Images.AutoResize = false
			*merged.Retry.Provider.TimeoutMs = 99
			*merged.Retry.Provider.MaxRetries = 99
			*merged.Retry.Provider.MaxRetryDelayMs = 99
			override := merged.Compaction.ModelOverrides["p/m"]
			*override.ReserveTokens = 99
			*override.KeepRecentTokens = 99
			after, err := json.Marshal(source)
			require.NoError(t, err)
			assert.JSONEq(t, string(before), string(after))
		})
	}
}

func TestSettingsWritesPreserveCancellationCause(t *testing.T) {
	for _, operation := range []string{"update", "replace", "lock wait"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			original := []byte(`{"theme":"original"}`)
			require.NoError(t, os.WriteFile(path, original, 0o600))
			cause := errors.New("settings canceled by caller")
			ctx, cancel := context.WithCancelCause(t.Context())
			if operation == "lock wait" {
				lock := flock.New(path + ".lock")
				require.NoError(t, lock.Lock())
				defer func() { require.NoError(t, lock.Close()) }()
				cancel(cause)
				var stop context.CancelFunc
				ctx, stop = context.WithTimeoutCause(t.Context(), time.Millisecond, cause)
				defer stop()
			} else {
				cancel(cause)
			}
			var err error
			if operation == "replace" {
				err = replaceSettings(ctx, path, []byte(`{"theme":"new"}`))
			} else {
				err = UpdateSettings(ctx, path, map[string]any{"theme": "new"})
			}
			require.ErrorIs(t, err, cause)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, original, got)
		})
	}
}

func TestSettingsRetainUnknownNestedFieldsWhenSaved(t *testing.T) {
	for _, group := range []string{"compaction", "retry", "thinkingBudgets", "terminal", "images", "markdown", "bash", "read", "branchSummary"} {
		t.Run(group, func(t *testing.T) {
			data := []byte(fmt.Sprintf(`{"%s":{"future":{"count":9007199254740993,"enabled":false,"items":[]}}}`, group))
			settings, err := parseSettings(data)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "settings.json")
			require.NoError(t, SaveSettings(path, settings))
			saved, err := os.ReadFile(path)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(saved, &fields))
			var nested map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(fields[group], &nested))
			require.JSONEq(t, `{"count":9007199254740993,"enabled":false,"items":[]}`, string(nested["future"]))
			require.Contains(t, string(nested["future"]), "9007199254740993")
		})
	}
}

func TestSettingsUnknownTopLevelEditsReplaceOriginalJSON(t *testing.T) {
	settings, err := parseSettings([]byte(`{"custom":{"value":"old"}}`))
	require.NoError(t, err)
	settings.Plugins["custom"] = json.RawMessage(`{"value":"new"}`)
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, SaveSettings(path, settings))
	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(saved, &fields))
	require.JSONEq(t, `{"value":"new"}`, string(fields["custom"]))
}

func TestMergeSettingsOwnsUnknownNestedFieldBytes(t *testing.T) {
	user, err := parseSettings([]byte(`{"bash":{"future":{"value":"user"}},"read":{"future":{"value":"user"}}}`))
	require.NoError(t, err)
	project, err := parseSettings([]byte(`{"bash":{"future":{"value":"project"}}}`))
	require.NoError(t, err)
	merged := MergeSettings(user, project)
	project.Bash.present["future"][0] = '!'
	user.Read.present["future"][0] = '!'
	data, err := json.Marshal(merged)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	require.JSONEq(t, `{"future":{"value":"project"}}`, string(fields["bash"]))
	require.JSONEq(t, `{"future":{"value":"user"}}`, string(fields["read"]))
}

func TestSettingsRetainUnknownProviderAndModelFields(t *testing.T) {
	data := []byte(`{"retry":{"provider":{"timeoutMs":0,"future":{"n":9007199254740993}}},"compaction":{"modelOverrides":{"p/m":{"reserveTokens":0,"future":[false,null]}}},"models":{"p/m":{"thinkingLevel":"low","future":{"enabled":false}}}}`)
	settings, err := parseSettings(data)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, SaveSettings(path, settings))
	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(saved), "9007199254740993")
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(saved, &fields))
	var retry map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fields["retry"], &retry))
	require.JSONEq(t, `{"timeoutMs":0,"future":{"n":9007199254740993}}`, string(retry["provider"]))
	var compaction map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fields["compaction"], &compaction))
	require.JSONEq(t, `{"p/m":{"reserveTokens":0,"future":[false,null]}}`, string(compaction["modelOverrides"]))
	require.JSONEq(t, `{"p/m":{"thinkingLevel":"low","future":{"enabled":false}}}`, string(fields["models"]))
}

func TestMergeSettingsOwnsProviderAndModelMetadata(t *testing.T) {
	user, err := parseSettings([]byte(`{"retry":{"provider":{"timeoutMs":7,"future":"user"}},"compaction":{"modelOverrides":{"p/m":{"reserveTokens":5,"future":"user"}}},"models":{"p/m":{"thinkingLevel":"high","future":"user"}}}`))
	require.NoError(t, err)
	project, err := parseSettings([]byte(`{"retry":{"provider":{"maxRetries":0,"future":"project"}},"compaction":{"modelOverrides":{"p/m":{"keepRecentTokens":0,"future":"project"}}},"models":{"p/m":{"thinkingLevel":"","future":"project"}}}`))
	require.NoError(t, err)
	merged := MergeSettings(user, project)
	project.Retry.Provider.present["future"][0] = '!'
	project.Compaction.ModelOverrides["p/m"].present["future"][0] = '!'
	project.Models["p/m"].present["future"][0] = '!'
	require.Empty(t, merged.Models["p/m"].ThinkingLevel)
	require.Equal(t, 7, *merged.Retry.Provider.TimeoutMs)
	require.Zero(t, *merged.Retry.Provider.MaxRetries)
	require.Equal(t, 5, *merged.Compaction.ModelOverrides["p/m"].ReserveTokens)
	require.Zero(t, *merged.Compaction.ModelOverrides["p/m"].KeepRecentTokens)
	for _, raw := range []json.RawMessage{merged.Retry.Provider.present["future"], merged.Compaction.ModelOverrides["p/m"].present["future"], merged.Models["p/m"].present["future"]} {
		require.Equal(t, `"project"`, string(raw))
	}
	_, err = json.Marshal(merged)
	require.NoError(t, err)
}

func TestMergedResourceScopesOwnTheirEntrySlices(t *testing.T) {
	user, err := parseSettings([]byte(`{"skills":["user"]}`))
	require.NoError(t, err)
	project, err := parseSettings([]byte(`{"skills":["project"]}`))
	require.NoError(t, err)
	first := MergeSettings(user, project)
	next := MergeSettings(first, nil)
	scopes := first.resourceScopes["skills"]
	scopes[0].entries[0] = "changed-user"
	scopes[1].entries[0] = "changed-project"
	u, p := next.ResourcePathsByScope("skills")
	require.Equal(t, []string{"user"}, u)
	require.Equal(t, []string{"project"}, p)
}

func TestSettingsRejectNonObjectRootBeforeRewrite(t *testing.T) {
	for _, data := range []string{"null", "[]", "true", "42", `"settings"`} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
			_, err := LoadSettings(path)
			require.Error(t, err)
			require.Error(t, UpdateSettings(t.Context(), path, map[string]any{"theme": "dark"}))
			unchanged, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, data, string(unchanged))
		})
	}
}

func TestMergeSettingsMergesBashEnvironmentPerKey(t *testing.T) {
	user, err := parseSettings([]byte(`{"bash":{"envExtra":{"KEEP":"user","REPLACE":"user"},"envAllowlist":["HOME"]}}`))
	require.NoError(t, err)
	project, err := parseSettings([]byte(`{"bash":{"envExtra":{"REPLACE":"project","ADD":"project"},"envAllowlist":[]}}`))
	require.NoError(t, err)
	merged := MergeSettings(user, project)
	require.Equal(t, map[string]string{"KEEP": "user", "REPLACE": "project", "ADD": "project"}, merged.Bash.EnvExtra)
	require.NotNil(t, merged.Bash.EnvAllowlist)
	require.Empty(t, merged.Bash.EnvAllowlist)
	merged.Bash.EnvExtra["KEEP"] = "changed"
	require.Equal(t, "user", user.Bash.EnvExtra["KEEP"])
	require.Equal(t, "project", project.Bash.EnvExtra["REPLACE"])
	empty, err := parseSettings([]byte(`{"bash":{"envExtra":{}}}`))
	require.NoError(t, err)
	require.Empty(t, MergeSettings(user, empty).Bash.EnvExtra)
}
