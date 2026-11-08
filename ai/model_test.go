package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestModelResolver_DefaultModel returns the configured default provider/model
// from the flat settings schema.
func TestModelResolver_DefaultModel(t *testing.T) {
	r := NewModelResolver(&Settings{
		DefaultProvider: "anthropic",
		DefaultModel:    "claude-opus-4-6",
	})
	spec := r.ResolveDefaultModel()
	if spec.Provider != "anthropic" || spec.Model != "claude-opus-4-6" {
		t.Errorf("expected anthropic/claude-opus-4-6, got %s/%s", spec.Provider, spec.Model)
	}
}

func TestModelResolver_DefaultModelEmpty(t *testing.T) {
	r := NewModelResolver(&Settings{})
	if spec := r.ResolveDefaultModel(); spec.Provider != "" || spec.Model != "" {
		t.Errorf("expected empty, got %s/%s", spec.Provider, spec.Model)
	}
}

func TestModelResolver_DefaultModelPartial(t *testing.T) {
	r := NewModelResolver(&Settings{DefaultProvider: "anthropic"})
	if spec := r.ResolveDefaultModel(); spec.Provider != "" {
		t.Errorf("expected empty when only DefaultProvider set, got %s", spec.Provider)
	}
}

// TestLoadSettings_MissingFile returns an empty Settings for a missing file.
func TestLoadSettings_MissingFile(t *testing.T) {
	s, err := LoadSettings(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil Settings")
	}
	if s.DefaultProvider != "" || s.DefaultModel != "" {
		t.Errorf("expected empty settings, got %+v", s)
	}
}

// TestLoadSettings_FlatSchema verifies Pi-compatible JSON round-trips.
func TestLoadSettings_FlatSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	contents := `{
  "defaultProvider": "anthropic",
  "defaultModel": "claude-sonnet-4-5",
  "theme": "dark",
  "terminal": {"showImages": true},
  "compaction": {"enabled": true, "reserveTokens": 16384, "keepRecentTokens": 20000},
  "retry": {"enabled": true, "maxRetries": 3},
  "packages": ["npm:@acme/pi-tools@1.2.3", {"source": "./local", "skills": ["skills/review"]}],
  "extensions": ["extensions/local.ts", "!legacy.ts"]
}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if s.DefaultProvider != "anthropic" || s.DefaultModel != "claude-sonnet-4-5" {
		t.Errorf("expected anthropic/claude-sonnet-4-5, got %s/%s", s.DefaultProvider, s.DefaultModel)
	}
	if s.Theme != "dark" {
		t.Errorf("expected theme=dark, got %q", s.Theme)
	}
	if s.Terminal.ShowImages == nil || !*s.Terminal.ShowImages {
		t.Error("expected terminal.showImages=true")
	}
	if !s.Compaction.Enabled || s.Compaction.ReserveTokens != 16384 {
		t.Errorf("compaction: %+v", s.Compaction)
	}
	if !s.Retry.Enabled || s.Retry.MaxRetries != 3 {
		t.Errorf("retry: %+v", s.Retry)
	}
	if len(s.Packages) != 2 {
		t.Fatalf("expected 2 packages, got %d", len(s.Packages))
	}
	if s.Packages[0].Source != "npm:@acme/pi-tools@1.2.3" {
		t.Errorf("expected first package source, got %q", s.Packages[0].Source)
	}
	if s.Packages[1].Source != "./local" || len(s.Packages[1].Skills) != 1 {
		t.Errorf("expected object package form, got %+v", s.Packages[1])
	}
	if len(s.Extensions) != 2 || s.Extensions[1] != "!legacy.ts" {
		t.Errorf("extensions: %v", s.Extensions)
	}
}

// TestLoadSettings_RejectsDeprecatedShape ensures the old {default, providers}
// shape fails loudly instead of silently zero-filling.
func TestLoadSettings_RejectsDeprecatedShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"default":"anthropic/default/x","providers":{"anthropic":{"default":{}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(path); err == nil {
		t.Fatal("expected error for deprecated {default, providers} shape")
	} else if !strings.Contains(err.Error(), "deprecated") {
		t.Errorf("error should mention deprecation, got %q", err)
	}
}

// TestSaveAndLoadSettings_RoundTrip verifies that Load+Save+Load is stable
// for a Settings struct filled with Pi-compatible fields.
func TestSaveAndLoadSettings_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	original := &Settings{
		DefaultProvider: "openai",
		DefaultModel:    "gpt-5.4",
		Theme:           "light",
		Compaction:      CompactionSettings{Enabled: true, ReserveTokens: 20000, KeepRecentTokens: 30000},
		Retry:           RetrySettings{Enabled: true, MaxRetries: 5, BaseDelayMs: 500, MaxDelayMs: 30000},
		Bash:            BashSettings{Shell: "/bin/bash", OutputCap: 204800},
		Terminal:        TerminalSettings{ShowImages: ptrBool(true)},
		Images: ImageSettings{
			AutoResize: ptrBool(true),
			MaxWidth:   2400,
			MaxHeight:  2400,
		},
		Markdown: MarkdownSettings{CodeBlockIndent: "    "},
		Plugins: map[string]json.RawMessage{
			"my-plugin": json.RawMessage(`{"foo":"bar"}`),
		},
	}

	if err := SaveSettings(path, original); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	loaded, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if loaded.DefaultProvider != "openai" || loaded.DefaultModel != "gpt-5.4" {
		t.Errorf("default: %s/%s", loaded.DefaultProvider, loaded.DefaultModel)
	}
	if loaded.Theme != "light" {
		t.Errorf("theme: %q", loaded.Theme)
	}
	if loaded.Compaction.ReserveTokens != 20000 || !loaded.Compaction.Enabled {
		t.Errorf("compaction: %+v", loaded.Compaction)
	}
	if loaded.Retry.MaxRetries != 5 || loaded.Retry.BaseDelayMs != 500 {
		t.Errorf("retry: %+v", loaded.Retry)
	}
	if loaded.Bash.Shell != "/bin/bash" || loaded.Bash.OutputCap != 204800 {
		t.Errorf("bash: %+v", loaded.Bash)
	}
	if loaded.Images.MaxWidth != 2400 || loaded.Images.AutoResize == nil || !*loaded.Images.AutoResize {
		t.Errorf("images: %+v", loaded.Images)
	}
	if loaded.Markdown.CodeBlockIndent != "    " {
		t.Errorf("markdown.codeBlockIndent: %q", loaded.Markdown.CodeBlockIndent)
	}
	if got := string(loaded.Plugins["my-plugin"]); !jsonEquivalent(got, `{"foo":"bar"}`) {
		t.Errorf("plugin roundtrip: %s", loaded.Plugins["my-plugin"])
	}
}

// TestSaveSettings_PreservesUnknownKeys verifies that arbitrary top-level keys
// not in the typed Settings struct are preserved through a Save round-trip.
func TestSaveSettings_PreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if err := os.WriteFile(path, []byte(`{"defaultProvider":"anthropic","defaultModel":"x","myCustomKey":[1,2,3]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if s.DefaultProvider != "anthropic" {
		t.Errorf("default provider: %q", s.DefaultProvider)
	}
	// Round-trip
	if err := SaveSettings(path, s); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "myCustomKey") {
		t.Errorf("expected unknown key preserved, got %s", data)
	}
}

// TestMergeSettings_DefaultProvider verifies project overrides user.
func TestMergeSettings_DefaultProvider(t *testing.T) {
	user := &Settings{DefaultProvider: "anthropic", DefaultModel: "claude-opus-4-6"}
	project := &Settings{DefaultProvider: "openai", DefaultModel: "gpt-5.4"}

	merged := MergeSettings(user, project)
	if merged.DefaultProvider != "openai" || merged.DefaultModel != "gpt-5.4" {
		t.Errorf("project should override user, got %s/%s", merged.DefaultProvider, merged.DefaultModel)
	}
}

// TestMergeSettings_PreservesUserWhenProjectEmpty verifies user wins on empty.
func TestMergeSettings_PreservesUserWhenProjectEmpty(t *testing.T) {
	user := &Settings{DefaultProvider: "anthropic", DefaultModel: "claude-opus-4-6"}
	project := &Settings{}

	merged := MergeSettings(user, project)
	if merged.DefaultProvider != "anthropic" {
		t.Errorf("user should win when project empty, got %s", merged.DefaultProvider)
	}
}

// TestMergeSettings_PluginNullRemoval verifies nil json.RawMessage removes.
func TestMergeSettings_PluginNullRemoval(t *testing.T) {
	user := &Settings{
		Plugins: map[string]json.RawMessage{
			"keep": json.RawMessage(`{"v":1}`),
			"drop": json.RawMessage(`{"v":2}`),
		},
	}
	project := &Settings{
		Plugins: map[string]json.RawMessage{
			"drop": nil,
		},
	}
	merged := MergeSettings(user, project)
	if _, ok := merged.Plugins["drop"]; ok {
		t.Error("expected drop to be removed by nil")
	}
	if _, ok := merged.Plugins["keep"]; !ok {
		t.Error("expected keep to survive")
	}
}

// TestMergeSettings_NilInputs returns an empty settings.
func TestMergeSettings_NilInputs(t *testing.T) {
	merged := MergeSettings(nil, nil)
	if merged == nil {
		t.Fatal("expected non-nil merge of nils")
	}
	if merged.DefaultProvider != "" {
		t.Errorf("expected empty, got %s", merged.DefaultProvider)
	}
}

// TestMigrateSettings is a no-op for the flat schema.
func TestMigrateSettings(t *testing.T) {
	s := &Settings{DefaultProvider: "anthropic"}
	out, ok := MigrateSettings(s)
	if ok {
		t.Error("expected ok=false (no migration needed)")
	}
	if out.DefaultProvider != "anthropic" {
		t.Errorf("expected identity, got %s", out.DefaultProvider)
	}
}

func ptrBool(b bool) *bool { return &b }

// jsonEquivalent reports whether two JSON byte slices are semantically equal
// (ignoring whitespace differences).
func jsonEquivalent(a, b string) bool {
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	ar, _ := json.Marshal(av)
	br, _ := json.Marshal(bv)
	return string(ar) == string(br)
}

// TestSaveDefaultModel_PiFlat verifies the flat defaultProvider/defaultModel
// pair is written by SaveDefaultModel.
func TestSaveDefaultModel_PiFlat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if err := SaveDefaultModel(path, "openai/gpt-5.4"); err != nil {
		t.Fatalf("SaveDefaultModel: %v", err)
	}
	s, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if s.DefaultProvider != "openai" || s.DefaultModel != "gpt-5.4" {
		t.Errorf("expected openai/gpt-5.4, got %s/%s", s.DefaultProvider, s.DefaultModel)
	}
}

// TestSaveDefaultModel_AcceptsFullyQualified verifies a 3-segment input drops
// the account component and writes provider/model.
func TestSaveDefaultModel_AcceptsFullyQualified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if err := SaveDefaultModel(path, "anthropic/work/claude-opus-4-6"); err != nil {
		t.Fatalf("SaveDefaultModel: %v", err)
	}
	s, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if s.DefaultProvider != "anthropic" || s.DefaultModel != "claude-opus-4-6" {
		t.Errorf("expected anthropic/claude-opus-4-6, got %s/%s", s.DefaultProvider, s.DefaultModel)
	}
}

// TestSettings_PiJSONFieldNames verifies the Settings JSON tags match Pi.
func TestSettings_PiJSONFieldNames(t *testing.T) {
	s := Settings{
		DefaultProvider: "anthropic",
		DefaultModel:    "claude-opus-4-6",
		Theme:           "dark",
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"defaultProvider"`, `"defaultModel"`, `"theme"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %s in %s", want, data)
		}
	}
}

func TestParseModelSpec_FullyQualified(t *testing.T) {
	spec, err := ParseModelSpec("anthropic/work/claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Provider != "anthropic" {
		t.Errorf("provider: got %q, want %q", spec.Provider, "anthropic")
	}
	// Legacy three-segment form: the middle segment is silently discarded.
	if spec.Model != "claude-sonnet-4-6" {
		t.Errorf("model: got %q, want %q", spec.Model, "claude-sonnet-4-6")
	}
	if !spec.IsFullyQualified() {
		t.Error("expected fully qualified")
	}
}

func TestParseModelSpec_TwoParts(t *testing.T) {
	spec, err := ParseModelSpec("anthropic/claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Provider != "anthropic" {
		t.Errorf("provider: got %q, want %q", spec.Provider, "anthropic")
	}
	if spec.Model != "claude-sonnet-4-6" {
		t.Errorf("model: got %q, want %q", spec.Model, "claude-sonnet-4-6")
	}
	if !spec.IsFullyQualified() {
		t.Error("expected fully qualified")
	}
}

func TestParseModelSpec_BareModel(t *testing.T) {
	spec, err := ParseModelSpec("claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Provider != "" {
		t.Errorf("provider: got %q, want empty", spec.Provider)
	}
	if spec.Model != "claude-sonnet-4-6" {
		t.Errorf("model: got %q, want %q", spec.Model, "claude-sonnet-4-6")
	}
	if spec.IsFullyQualified() {
		t.Error("expected not fully qualified")
	}
}

func TestParseModelSpec_Empty(t *testing.T) {
	_, err := ParseModelSpec("")
	if err == nil {
		t.Error("expected error for empty specifier")
	}
}

func TestParseModelSpec_TooManyParts(t *testing.T) {
	_, err := ParseModelSpec("a/b/c/d")
	if err == nil {
		t.Error("expected error for too many parts")
	}
}

func TestModelSpec_String(t *testing.T) {
	tests := []struct {
		spec ModelSpec
		want string
	}{
		{ModelSpec{Provider: "anthropic", Model: "claude-sonnet-4-6"}, "anthropic/claude-sonnet-4-6"},
		{ModelSpec{Provider: "", Model: "claude-sonnet-4-6"}, "claude-sonnet-4-6"},
	}
	for _, tt := range tests {
		got := tt.spec.String()
		if got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

func TestResolveShortModel_Found(t *testing.T) {
	settings := &Settings{DefaultProvider: "anthropic", DefaultModel: "claude-sonnet-4-6"}
	spec, err := ResolveShortModel("claude-sonnet-4-6", settings)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Provider != "anthropic" {
		t.Errorf("provider: got %q, want %q", spec.Provider, "anthropic")
	}
}

func TestResolveShortModel_NotFound(t *testing.T) {
	settings := &Settings{DefaultProvider: "anthropic", DefaultModel: "claude-sonnet-4-6"}
	_, err := ResolveShortModel("nonexistent", settings)
	if err == nil {
		t.Error("expected error for not found model")
	}
}

// TestResolveShortModel_Ambiguous is no longer reachable through settings
// alone in the flat schema: only one defaultProvider/Model pair exists.
// Models from models.json or built-ins are checked but the flat shape
// disambiguates by provider. This test is intentionally skipped — kept here
// as documentation of the previous behavior.
func TestResolveShortModel_Ambiguous(t *testing.T) {
	t.Skip("ambiguous resolution via per-account providers no longer supported in flat schema")
}

func TestResolveShortModel_NilSettings(t *testing.T) {
	_, err := ResolveShortModel("anything", nil)
	if err == nil {
		t.Error("expected error with nil settings")
	}
}

// TestResolveModelPatterns_ConfiguredExactAndFuzzy is replaced by the new
// flat-schema semantics: candidates come from defaultProvider/Model plus
// built-in registry. The exact/glob/fuzzy coverage lives in TestResolveModelPatterns.
func TestResolveModelPatterns_ConfiguredExactAndFuzzy(t *testing.T) {
	settings := &Settings{
		DefaultProvider: "openai",
		DefaultModel:    "gpt-5.4",
	}

	scoped, err := ResolveModelPatterns([]string{"gpt-5.4"}, settings)
	if err != nil {
		t.Fatalf("ResolveModelPatterns exact: %v", err)
	}
	if got, want := scoped.Model.String(), "openai/gpt-5.4"; got != want {
		t.Fatalf("exact match = %q, want %q", got, want)
	}

	for _, pattern := range []string{"gpt-5.4:high", "openai/gpt-5.4:high"} {
		scoped, err = ResolveModelPatterns([]string{pattern}, settings)
		if err != nil {
			t.Fatalf("ResolveModelPatterns %q: %v", pattern, err)
		}
		if scoped.Model.String() != "openai/gpt-5.4" || scoped.ThinkingLevel != "high" {
			t.Fatalf("%q = %+v, want openai/gpt-5.4 at high", pattern, scoped)
		}
	}
}

func TestResolveModelPatterns_AmbiguousBareMatch(t *testing.T) {
	t.Skip("ambiguity resolution relies on built-in registry, not per-account providers")
}

func TestResolveModelPatterns_UsesEnvBackedCandidates(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-env")
	settings := &Settings{}

	scoped, err := ResolveModelPatterns([]string{"gpt-4o"}, settings)
	if err != nil {
		t.Fatalf("ResolveModelPatterns env-backed: %v", err)
	}
	if got, want := scoped.Model.String(), "openai/gpt-4o"; got != want {
		t.Fatalf("env-backed match = %q, want %q", got, want)
	}
}

func TestIsValidThinkingLevel(t *testing.T) {
	tests := []struct {
		level  string
		expect bool
	}{
		{"off", true},
		{"minimal", true},
		{"low", true},
		{"medium", true},
		{"high", true},
		{"xhigh", true},
		{"Xhigh", true}, // case insensitive
		{"HIGH", true},
		{"invalid", false},
		{"", false},
		{"ultra", false},
	}

	for _, tt := range tests {
		got := IsValidThinkingLevel(tt.level)
		if got != tt.expect {
			t.Errorf("IsValidThinkingLevel(%q) = %v, want %v", tt.level, got, tt.expect)
		}
	}
}

func TestParseModelPattern_WithThinkingLevel(t *testing.T) {
	candidates := []ModelSpec{
		{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		{Provider: "openai", Model: "gpt-4o"},
	}

	tests := []struct {
		pattern   string
		wantModel string
		wantLevel string
		wantWarn  bool
	}{
		{"claude-sonnet-4-6:high", "claude-sonnet-4-6", "high", false},
		{"gpt-4o:medium", "gpt-4o", "medium", false},
		{"claude-sonnet-4-6:xhigh", "claude-sonnet-4-6", "xhigh", false},
		{"openai/gpt-4o:low", "gpt-4o", "low", false}, // provider/model format
		{"nonexistent:high", "", "", true},            // no match, returns warning
	}

	for _, tt := range tests {
		result := ParseModelPattern(tt.pattern, candidates, false) // non-strict mode for backward compatibility
		if result.Spec.Model != tt.wantModel {
			t.Errorf("ParseModelPattern(%q).Spec.Model = %q, want %q", tt.pattern, result.Spec.Model, tt.wantModel)
		}
		if result.ThinkingLevel != tt.wantLevel {
			t.Errorf("ParseModelPattern(%q).ThinkingLevel = %q, want %q", tt.pattern, result.ThinkingLevel, tt.wantLevel)
		}
		if (result.Warning != "") != tt.wantWarn {
			t.Errorf("ParseModelPattern(%q).Warning = %q, wantWarn=%v", tt.pattern, result.Warning, tt.wantWarn)
		}
	}
}

func TestParseModelPattern_InvalidSuffixStrictMode(t *testing.T) {
	candidates := []ModelSpec{
		{Provider: "anthropic", Model: "claude-sonnet-4-6"},
	}

	// Strict mode: invalid suffix is treated as part of model ID, no fallback
	result := ParseModelPattern("claude-sonnet-4-6:invalid", candidates, true)
	if result.Spec.Model != "" {
		t.Errorf("strict mode: expected no match for invalid suffix, got %q", result.Spec.Model)
	}

	// Non-strict mode: invalid suffix is stripped, model is matched
	result = ParseModelPattern("claude-sonnet-4-6:invalid", candidates, false)
	if result.Spec.Model != "claude-sonnet-4-6" {
		t.Errorf("non-strict mode: expected match after stripping invalid suffix, got %q", result.Spec.Model)
	}
	if result.Warning == "" {
		t.Errorf("non-strict mode: expected warning for invalid suffix")
	}
	if result.ThinkingLevel != "" {
		t.Errorf("non-strict mode: expected no thinking level after stripping invalid suffix, got %q", result.ThinkingLevel)
	}
}

func TestIsAliasModelID(t *testing.T) {
	tests := []struct {
		id     string
		expect bool
	}{
		{"claude-sonnet-4-latest", true},
		{"claude-sonnet-4-6", true},
		{"gpt-4o", true},
		{"claude-sonnet-4-6-20250929", false}, // date suffix
		{"claude-sonnet-4-6-20241022", false},
	}

	for _, tt := range tests {
		got := IsAliasModelID(tt.id)
		if got != tt.expect {
			t.Errorf("IsAliasModelID(%q) = %v, want %v", tt.id, got, tt.expect)
		}
	}
}

func TestDefaultModelPerProvider(t *testing.T) {
	// Verify it contains expected entries
	if DefaultModelPerProvider["anthropic"] != "claude-opus-4-8" {
		t.Errorf("anthropic default: got %q, want %q", DefaultModelPerProvider["anthropic"], "claude-opus-4-8")
	}
	if DefaultModelPerProvider["openai"] != "gpt-6.1-sol" {
		t.Errorf("openai default: got %q, want %q", DefaultModelPerProvider["openai"], "gpt-6.1-sol")
	}
	if DefaultModelPerProvider["google"] != "gemini-3.1-pro-preview" {
		t.Errorf("google default: got %q, want %q", DefaultModelPerProvider["google"], "gemini-3.1-pro-preview")
	}

	// Every default must resolve against the generated registry. A dangling
	// default silently degrades InitialModel/RestoreModelFromSession to "first
	// available model". This catches registry churn (e.g. minimax models being
	// dropped by the generator, leaving MiniMax-M3 dangling) at test time.
	for provider, defaultID := range DefaultModelPerProvider {
		model, err := GetAvailableModel(provider, defaultID)
		if err != nil || model == nil {
			t.Errorf("default model %s/%s not found in registry: %v", provider, defaultID, err)
		}
	}
}

func TestBuildFallbackModelSpec(t *testing.T) {
	candidates := []ModelSpec{
		{Provider: "anthropic", Model: "claude-opus-4-6"},
		{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		{Provider: "openai", Model: "gpt-4o"},
	}

	result := BuildFallbackModelSpec("anthropic", "custom-model-id", candidates)
	if result.Model != "custom-model-id" {
		t.Errorf("Model = %q, want %q", result.Model, "custom-model-id")
	}
	if result.Provider != "anthropic" {
		t.Errorf("Provider = %q, want %q", result.Provider, "anthropic")
	}

	// With non-existent provider
	result = BuildFallbackModelSpec("unknown", "custom-model", candidates)
	if result.Model != "" {
		t.Errorf("expected empty for unknown provider, got %q", result.Model)
	}
}

func TestFindExactModelSpecMatch(t *testing.T) {
	candidates := []ModelSpec{
		{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		{Provider: "openai", Model: "gpt-4o"},
	}

	// Canonical format
	result := FindExactModelSpecMatch("anthropic/claude-sonnet-4-6", candidates)
	if result.Model != "claude-sonnet-4-6" {
		t.Errorf("canonical match: got %q", result.Model)
	}

	// Provider/model split
	result = FindExactModelSpecMatch("openai/gpt-4o", candidates)
	if result.Model != "gpt-4o" {
		t.Errorf("provider/model split: got %q", result.Model)
	}

	// Bare model ID - unique
	result = FindExactModelSpecMatch("gpt-4o", candidates)
	if result.Model != "gpt-4o" {
		t.Errorf("bare model unique: expected gpt-4o, got %q", result.Model)
	}

	// Unique bare model ID
	result = FindExactModelSpecMatch("claude-sonnet-4-6", candidates)
	if result.Model != "claude-sonnet-4-6" {
		t.Errorf("unique bare model: got %q", result.Model)
	}

	// Non-existent
	result = FindExactModelSpecMatch("nonexistent", candidates)
	if result.Model != "" {
		t.Errorf("nonexistent: expected empty, got %q", result.Model)
	}
}

func TestRestoreModelSpecFromSession(t *testing.T) {
	candidates := []ModelSpec{
		{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		{Provider: "openai", Model: "gpt-4o"},
	}

	// Model still exists - should restore
	result := RestoreModelSpecFromSession("anthropic", "claude-sonnet-4-6", ModelSpec{}, candidates)
	if result.Spec.Model != "claude-sonnet-4-6" {
		t.Errorf("restored model: got %q", result.Spec.Model)
	}
	if result.FallbackMessage != "" {
		t.Errorf("expected no fallback message, got %q", result.FallbackMessage)
	}

	// Model not found - no current model, uses first known provider default
	result = RestoreModelSpecFromSession("unknown", "model", ModelSpec{}, candidates)
	// Should fall back to anthropic default which is claude-opus-4-6 (but not in candidates, so first available)
	if result.Spec.Model == "" {
		t.Error("expected a fallback model")
	}
	if result.FallbackMessage == "" {
		t.Error("expected fallback message")
	}
}

func TestResolveCliModel(t *testing.T) {
	// Note: This test uses env-backed models via OPENAI_API_KEY
	// Set the env var to enable env-backed model resolution
	t.Setenv("OPENAI_API_KEY", "sk-test")

	tests := []struct {
		name      string
		provider  string
		model     string
		wantModel bool
		wantWarn  bool
		wantErr   bool
	}{
		{
			name:      "empty model returns no error",
			model:     "",
			wantModel: false,
			wantErr:   false,
		},
		{
			name:      "unknown provider",
			provider:  "nonexistent-provider",
			model:     "gpt-4o",
			wantModel: false,
			wantErr:   true,
		},
		{
			name:      "model not found - falls back to custom model id",
			provider:  "openai",
			model:     "nonexistent-model",
			wantModel: true,  // falls back to custom model id
			wantWarn:  true,  // has warning about model not found
			wantErr:   false, // custom model id is accepted
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ResolveCliModel(tt.provider, tt.model, true)
			if (result.Spec.Model != "") != tt.wantModel {
				t.Errorf("ResolveCliModel() model present = %v, want %v", result.Spec.Model != "", tt.wantModel)
			}
			if (result.Error != "") != tt.wantErr {
				t.Errorf("ResolveCliModel() error = %v, wantErr %v, error: %s", result.Error != "", tt.wantErr, result.Error)
			}
			if (result.Warning != "") != tt.wantWarn {
				t.Errorf("ResolveCliModel() warning = %v, wantWarn %v", result.Warning != "", tt.wantWarn)
			}
		})
	}
}

func TestResolveCliModel_WithThinkingLevel(t *testing.T) {
	// Test that thinking level suffix is properly parsed
	// This test verifies the function signature and mode parameter
	result := ResolveCliModel("", "", true) // empty, strict mode
	if result.Spec.Model != "" || result.Error != "" || result.Warning != "" {
		t.Error("empty model should return empty result")
	}

	// Verify strict mode vs non-strict mode behavior
	candidates := []ModelSpec{
		{Provider: "anthropic", Model: "claude-sonnet-4-6"},
	}

	// In strict mode, invalid suffix should cause failure
	resultStrict := ParseModelPattern("claude-sonnet-4-6:invalid", candidates, true)
	if resultStrict.Spec.Model != "" {
		t.Error("strict mode: invalid suffix should cause no match")
	}

	// In non-strict mode, invalid suffix should be stripped
	resultNonStrict := ParseModelPattern("claude-sonnet-4-6:invalid", candidates, false)
	if resultNonStrict.Spec.Model != "claude-sonnet-4-6" {
		t.Error("non-strict mode: invalid suffix should be stripped, model matched")
	}
	if resultNonStrict.Warning == "" {
		t.Error("non-strict mode: should have warning for invalid suffix")
	}
}
