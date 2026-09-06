package ai

import (
	"os"
	"path/filepath"
	"testing"
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
