// Package ai config file schema for ~/.tau/agent/settings.json and
// <cwd>/.tau/settings.json. Mirrors the Pi settings schema (Pi reference);
// only the tau prefix and a few Tau-specific tool knobs differ.
package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// BashSettings controls bash tool behavior from settings.json.
type BashSettings struct {
	// Shell is the shell executable. Overrides TAU_SHELL. Defaults to /bin/sh.
	Shell string `json:"shell,omitempty"`
	// OutputCap is the maximum combined stdout+stderr output in bytes.
	// Defaults to 102400 (100 KiB).
	OutputCap int `json:"outputCap,omitempty"`
	// EnvAllowlist lists environment variable names to pass through to
	// the bash subprocess. When non-empty, only these keys are inherited.
	EnvAllowlist []string `json:"envAllowlist,omitempty"`
	// EnvExtra is additional environment variables to inject/override.
	EnvExtra map[string]string `json:"envExtra,omitempty"`
	// CommandPrefix is prepended to every bash command before execution.
	CommandPrefix string `json:"commandPrefix,omitempty"`
	// StreamWindowMs is the coalescing window for streaming output chunks.
	StreamWindowMs int `json:"streamWindowMs,omitempty"`
	// KillGraceMs is the grace period before SIGKILL on cancellation.
	KillGraceMs int `json:"killGraceMs,omitempty"`
}

// ReadSettings controls the read tool.
type ReadSettings struct {
	// LineCap is the maximum lines returned when no explicit range is given.
	// Defaults to 2000.
	LineCap int `json:"lineCap,omitempty"`
}

// ImageSettings controls image handling for file tools and model input.
type ImageSettings struct {
	// AutoResize enables automatic image resizing before returning image blocks.
	// Nil means use the default behavior (enabled).
	AutoResize *bool `json:"autoResize,omitempty"`
	// MaxWidth is the maximum image width for resizing. Defaults to 2000.
	MaxWidth int `json:"maxWidth,omitempty"`
	// MaxHeight is the maximum image height for resizing. Defaults to 2000.
	MaxHeight int `json:"maxHeight,omitempty"`
	// BlockImages returns placeholders instead of image blocks when true.
	BlockImages bool `json:"blockImages,omitempty"`
}

// TerminalSettings controls TUI/terminal behavior.
type TerminalSettings struct {
	// ShowImages enables inline image rendering. Nil means use the default
	// behavior (enabled). A plain bool cannot express this: with omitempty,
	// "absent" and "false" encode identically, so every user who never wrote
	// the key would silently get images disabled.
	ShowImages *bool `json:"showImages,omitempty"`
	// ClearOnShrink enables clearing the screen on TUI shrink. If unset,
	// PI_CLEAR_ON_SHRINK=1 enables it.
	ClearOnShrink bool `json:"clearOnShrink,omitempty"`
}

// MarkdownSettings controls markdown rendering.
type MarkdownSettings struct {
	// CodeBlockIndent is the indent used inside fenced code blocks.
	// Defaults to two spaces.
	CodeBlockIndent string `json:"codeBlockIndent,omitempty"`
}

// CompactionSettings controls auto-compaction. Matches agent/compaction.Settings.
type CompactionSettings struct {
	// Enabled indicates whether compaction is active. Defaults to true.
	Enabled bool `json:"enabled,omitempty"`
	// ReserveTokens is the number of tokens reserved for the LLM prompt
	// and response during summarization. Defaults to 16384.
	ReserveTokens int `json:"reserveTokens,omitempty"`
	// KeepRecentTokens is the target number of recent tokens to keep
	// uncompacted when cutting history. Defaults to 20000.
	KeepRecentTokens int `json:"keepRecentTokens,omitempty"`
}

// RetrySettings controls automatic retry of transient failures.
type RetrySettings struct {
	// Enabled indicates whether automatic retry is active. Defaults to true.
	Enabled bool `json:"enabled,omitempty"`
	// MaxRetries is the maximum number of retry attempts. Defaults to 3.
	MaxRetries int `json:"maxRetries,omitempty"`
	// BaseDelayMs is the initial retry delay in milliseconds. Defaults to 2000.
	BaseDelayMs int `json:"baseDelayMs,omitempty"`
	// MaxDelayMs is the maximum retry delay cap in milliseconds. Defaults to 60000.
	MaxDelayMs int `json:"maxDelayMs,omitempty"`
}

// ThinkingBudgetsSettings customizes token budgets per thinking level from
// settings.json. Named to avoid collision with the runtime ai.ThinkingBudgets
// type used for per-call budget overrides.
type ThinkingBudgetsSettings struct {
	Minimal int `json:"minimal,omitempty"`
	Low     int `json:"low,omitempty"`
	Medium  int `json:"medium,omitempty"`
	High    int `json:"high,omitempty"`
}

// ModelSettings holds per-model overrides from settings.json. Entries live in
// the top-level `models` map keyed by "provider/model-id" — the same key shape
// enabledModels uses. A nested providers shape is not an option: the settings
// parser rejects a top-level `providers` key (deprecated schema).
type ModelSettings struct {
	// ThinkingLevel overrides the reasoning/thinking effort used with this
	// model. Valid values match ai.ThinkingLevels ("off" through "xhigh").
	// Empty means no override — the defaultThinkingLevel applies.
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
}

// BranchSummarySettings controls branch-summary behavior.
type BranchSummarySettings struct {
	// ReserveTokens is the token budget for branch summaries. Defaults to 16384.
	ReserveTokens int `json:"reserveTokens,omitempty"`
	// SkipPrompt skips the confirmation prompt before summarizing. Defaults to false.
	SkipPrompt bool `json:"skipPrompt,omitempty"`
}

// PackageSource is one entry in settings.json `packages`. It may be a string
// ("npm:...", "github.com/...", "./local") or an object with filters.
type PackageSource struct {
	// Source is the package source specifier.
	Source string `json:"source"`
	// Extensions, Skills, Prompts, Themes filter which package resources load.
	Extensions []string `json:"extensions,omitempty"`
	Skills     []string `json:"skills,omitempty"`
	Prompts    []string `json:"prompts,omitempty"`
	Themes     []string `json:"themes,omitempty"`
}

// MarshalJSON renders PackageSource as either a string or an object.
func (p PackageSource) MarshalJSON() ([]byte, error) {
	if p.Extensions == nil && p.Skills == nil && p.Prompts == nil && p.Themes == nil {
		return json.Marshal(p.Source)
	}
	type pkgObj PackageSource
	return json.Marshal(pkgObj(p))
}

// UnmarshalJSON accepts either a string source or an object form.
func (p *PackageSource) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*p = PackageSource{Source: s}
		return nil
	}
	type pkgObj PackageSource
	var obj pkgObj
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*p = PackageSource(obj)
	return nil
}

// Settings is the merged contents of ~/.tau/agent/settings.json and
// <cwd>/.tau/settings.json. Mirrors the Pi settings.json schema documented
// only the tau/Tau prefix substitutions are intentional.
//
// Unknown top-level keys are preserved through Plugins so user extensions
// survive a Save round-trip.
type Settings struct {
	// === Top-level scalars ===

	LastChangelogVersion string `json:"lastChangelogVersion,omitempty"`
	DefaultProvider      string `json:"defaultProvider,omitempty"`
	DefaultModel         string `json:"defaultModel,omitempty"`
	DefaultThinkingLevel string `json:"defaultThinkingLevel,omitempty"`
	Transport            string `json:"transport,omitempty"` // sse | websocket
	SteeringMode         string `json:"steeringMode,omitempty"`
	FollowUpMode         string `json:"followUpMode,omitempty"`
	Theme                string `json:"theme,omitempty"`
	HideThinkingBlock    bool   `json:"hideThinkingBlock,omitempty"`
	ShellPath            string `json:"shellPath,omitempty"`
	QuietStartup         bool   `json:"quietStartup,omitempty"`
	// LayoutMode selects the interactive region arrangement: "fixed" (header
	// and editor pinned, chat scrolls on its own) or "document" (all regions in
	// one bottom-anchored scrolling document). Empty uses the default.
	LayoutMode             string `json:"layoutMode,omitempty"`
	ShellCommandPrefix     string `json:"shellCommandPrefix,omitempty"`
	CollapseChangelog      bool   `json:"collapseChangelog,omitempty"`
	EnableInstallTelemetry bool   `json:"enableInstallTelemetry,omitempty"`
	ShowHardwareCursor     bool   `json:"showHardwareCursor,omitempty"`
	EditorPaddingX         int    `json:"editorPaddingX,omitempty"`
	AutocompleteMaxVisible int    `json:"autocompleteMaxVisible,omitempty"`
	EnableSkillCommands    bool   `json:"enableSkillCommands,omitempty"`
	DoubleEscapeAction     string `json:"doubleEscapeAction,omitempty"` // fork | tree | none
	TreeFilterMode         string `json:"treeFilterMode,omitempty"`     // default|no-tools|user-only|labeled-only|all
	SessionDir             string `json:"sessionDir,omitempty"`

	// === Nested groups ===

	Compaction      CompactionSettings       `json:"compaction,omitempty"`
	BranchSummary   BranchSummarySettings    `json:"branchSummary,omitempty"`
	Retry           RetrySettings            `json:"retry,omitempty"`
	ThinkingBudgets ThinkingBudgetsSettings  `json:"thinkingBudgets,omitempty"`
	Terminal        TerminalSettings         `json:"terminal,omitempty"`
	Images          ImageSettings            `json:"images,omitempty"`
	Markdown        MarkdownSettings         `json:"markdown,omitempty"`
	Bash            BashSettings             `json:"bash,omitempty"`
	Read            ReadSettings             `json:"read,omitempty"`
	Models          map[string]ModelSettings `json:"models,omitempty"`

	// === Resource lists ===

	Packages      []PackageSource `json:"packages,omitempty"`
	Extensions    []string        `json:"extensions,omitempty"`
	Skills        []string        `json:"skills,omitempty"`
	Prompts       []string        `json:"prompts,omitempty"`
	Themes        []string        `json:"themes,omitempty"`
	NPMCommand    []string        `json:"npmCommand,omitempty"`
	EnabledModels []string        `json:"enabledModels,omitempty"`

	// Plugins holds arbitrary per-extension JSON, keyed by plugin/extension id.
	// Round-trips unknown top-level keys so user extensions are preserved.
	Plugins map[string]json.RawMessage `json:"plugins,omitempty"`
}

// LoadSettingsFromPath is the file-system entry point used by both
// model_resolver.go's shim and any future direct callers.
func LoadSettingsFromPath(path string) (*Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Settings{}, nil
		}
		return nil, fmt.Errorf("read settings: %w", err)
	}
	return parseSettings(data)
}

// LoadSettings reads settings.json from path. A missing file returns an
// empty Settings. A malformed file returns an error. Old Tau shapes
// ({default, providers}) are rejected with a migration error.
func LoadSettings(path string) (*Settings, error) {
	return LoadSettingsFromPath(path)
}

// ParseSettings decodes settings.json bytes. Exposed for callers (SDK, tests)
// that have already loaded the bytes.
func ParseSettings(data []byte) (*Settings, error) {
	return parseSettings(data)
}

func parseSettings(data []byte) (*Settings, error) {
	// Reject pre-Pi schema first so users get a clear migration error
	// instead of a zero-filled settings that silently drops everything.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parse settings: %w", err)
	}
	if _, ok := probe["providers"]; ok {
		return nil, fmt.Errorf("settings.json uses the deprecated {default, providers} shape; this version of tau uses the flat schema documented as the flat schema — re-create your settings.json with defaultProvider/defaultModel/etc. or rename the existing file out of the way")
	}
	if _, ok := probe["default"]; ok {
		return nil, fmt.Errorf("settings.json uses the deprecated {default} string; this version of tau uses defaultProvider and defaultModel fields")
	}

	// First pass: typed decode.
	var s Settings
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		// Unknown-field errors from extensions are not actionable; fall back
		// to permissive decoding so we still preserve unknowns in Plugins.
		s = Settings{}
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("parse settings: %w", err)
		}
	}

	// Second pass: capture top-level keys not in the typed schema and stash
	// them in Plugins so a Save round-trip preserves them.
	if s.Plugins == nil {
		s.Plugins = make(map[string]json.RawMessage)
	}
	for k, v := range probe {
		if _, known := knownSettingsKey(k); known {
			continue
		}
		if _, dup := s.Plugins[k]; dup {
			continue
		}
		s.Plugins[k] = v
	}
	return &s, nil
}

// knownSettingsKey returns true for every top-level Settings field name as it
// appears in JSON (camelCase per the Pi spec). The map is rebuilt here rather
// than cached because it changes only when the struct does.
func knownSettingsKey(k string) (struct{}, bool) {
	if knownSettingsKeyMap == nil {
		m := make(map[string]struct{})
		t := reflect.TypeOf(Settings{})
		for i := 0; i < t.NumField(); i++ {
			tag := t.Field(i).Tag.Get("json")
			name, _, _ := strings.Cut(tag, ",")
			if name == "" || name == "-" {
				continue
			}
			m[name] = struct{}{}
		}
		knownSettingsKeyMap = m
	}
	v, ok := knownSettingsKeyMap[k]
	return v, ok
}

var knownSettingsKeyMap map[string]struct{}

// SaveSettingsToPath is the file-system entry point used by both
// model_resolver.go's shim and any future direct callers.
func SaveSettingsToPath(path string, s *Settings) error {
	return SaveSettings(path, s)
}

// SaveSettings writes settings to path with mode 0644, creating parent
// directories as needed. Unknown keys held in Settings.Plugins are merged
// back into the top-level JSON so user extensions round-trip.
func SaveSettings(path string, s *Settings) error {
	if s == nil {
		s = &Settings{}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create settings dir: %w", err)
	}

	// Marshal typed fields first.
	typed, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}

	// Inject preserved Plugins entries so the user sees their unknown keys
	// back in the saved file.
	out := typed
	if len(s.Plugins) > 0 {
		var typedMap map[string]json.RawMessage
		if err := json.Unmarshal(typed, &typedMap); err != nil {
			return fmt.Errorf("re-parse settings: %w", err)
		}
		for k, v := range s.Plugins {
			if _, known := knownSettingsKey(k); known {
				continue
			}
			if _, exists := typedMap[k]; exists {
				continue
			}
			typedMap[k] = v
		}
		out, err = json.Marshal(typedMap)
		if err != nil {
			return fmt.Errorf("marshal settings with plugins: %w", err)
		}
	}

	pretty, err := prettyJSON(out)
	if err != nil {
		return fmt.Errorf("pretty-print settings: %w", err)
	}
	if err := os.WriteFile(path, pretty, 0o644); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}

// parseDefaultModelSpec accepts "provider/model", "provider/account/model",
// or "model". Returns provider and model (account dropped, matching Pi's flat
// settings schema).
func parseDefaultModelSpec(spec string) (provider, model string, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", "", fmt.Errorf("empty model specifier")
	}
	parts := strings.Split(spec, "/")
	switch len(parts) {
	case 1:
		return "", parts[0], nil
	case 2:
		return parts[0], parts[1], nil
	case 3:
		// Account segment is dropped — see flat-schema docs.
		return parts[0], parts[2], nil
	default:
		return "", "", fmt.Errorf("invalid model specifier %q: expected 1-3 slash-separated parts, got %d", spec, len(parts))
	}
}

// MigrateSettings applies any necessary migrations to settings.
// No migrations apply to the new flat schema; this is a no-op kept for
// callers that imported it.
func MigrateSettings(settings *Settings) (*Settings, bool) {
	return settings, false
}

// SaveDefaultModel writes the defaultProvider/defaultModel pair to path.
// The input may be a "provider/model" shorthand or a fully-qualified
// "provider/account/model" spec (the account is discarded, matching Pi).
func SaveDefaultModel(settingsPath, modelSpec string) error {
	provider, model, err := parseDefaultModelSpec(modelSpec)
	if err != nil {
		return fmt.Errorf("parse model spec: %w", err)
	}
	s, err := LoadSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	s.DefaultProvider = provider
	s.DefaultModel = model
	return SaveSettings(settingsPath, s)
}

// MergeSettingsInternal is the entry point used by model_resolver.go's shim.
func MergeSettingsInternal(user, project *Settings) *Settings {
	return MergeSettings(user, project)
}

// MergeSettings deep-merges project over user settings. Project scalars
// override when non-zero. Project lists replace (no concat — Pi semantics).
// Embedded groups merge per-field; project nil json.RawMessage within Plugins
// signals explicit removal.
func MergeSettings(user, project *Settings) *Settings {
	if user == nil {
		user = &Settings{}
	}
	if project == nil {
		project = &Settings{}
	}
	out := *user // shallow copy

	out.LastChangelogVersion = pickStr(user.LastChangelogVersion, project.LastChangelogVersion)
	out.DefaultProvider = pickStr(user.DefaultProvider, project.DefaultProvider)
	out.DefaultModel = pickStr(user.DefaultModel, project.DefaultModel)
	out.DefaultThinkingLevel = pickStr(user.DefaultThinkingLevel, project.DefaultThinkingLevel)
	out.Transport = pickStr(user.Transport, project.Transport)
	out.SteeringMode = pickStr(user.SteeringMode, project.SteeringMode)
	out.FollowUpMode = pickStr(user.FollowUpMode, project.FollowUpMode)
	out.Theme = pickStr(user.Theme, project.Theme)
	out.ShellPath = pickStr(user.ShellPath, project.ShellPath)
	out.ShellCommandPrefix = pickStr(user.ShellCommandPrefix, project.ShellCommandPrefix)
	out.DoubleEscapeAction = pickStr(user.DoubleEscapeAction, project.DoubleEscapeAction)
	out.LayoutMode = pickStr(user.LayoutMode, project.LayoutMode)
	out.TreeFilterMode = pickStr(user.TreeFilterMode, project.TreeFilterMode)
	out.SessionDir = pickStr(user.SessionDir, project.SessionDir)

	// Bools: project overrides only when explicitly true (Pi behavior —
	// project setting `false` does not silently flip a user-set `true`).
	if project.HideThinkingBlock {
		out.HideThinkingBlock = true
	}
	if project.QuietStartup {
		out.QuietStartup = true
	}
	if project.CollapseChangelog {
		out.CollapseChangelog = true
	}
	if project.DisableInstallTelemetryExplicit() {
		out.EnableInstallTelemetry = project.EnableInstallTelemetry
	}
	if project.ShowHardwareCursor {
		out.ShowHardwareCursor = true
	}
	if project.EnableSkillCommands {
		out.EnableSkillCommands = true
	}
	if project.EditorPaddingX != 0 {
		out.EditorPaddingX = project.EditorPaddingX
	}
	if project.AutocompleteMaxVisible != 0 {
		out.AutocompleteMaxVisible = project.AutocompleteMaxVisible
	}

	// Lists: project overrides entirely.
	if project.Packages != nil {
		out.Packages = project.Packages
	}
	if project.Extensions != nil {
		out.Extensions = project.Extensions
	}
	if project.Skills != nil {
		out.Skills = project.Skills
	}
	if project.Prompts != nil {
		out.Prompts = project.Prompts
	}
	if project.Themes != nil {
		out.Themes = project.Themes
	}
	if project.NPMCommand != nil {
		out.NPMCommand = project.NPMCommand
	}
	if project.EnabledModels != nil {
		out.EnabledModels = project.EnabledModels
	}

	// Embedded groups: per-field override.
	out.Compaction = mergeCompaction(user.Compaction, project.Compaction)
	out.Retry = mergeRetry(user.Retry, project.Retry)
	out.ThinkingBudgets = mergeThinkingBudgetsSettings(user.ThinkingBudgets, project.ThinkingBudgets)
	out.Terminal = mergeTerminal(user.Terminal, project.Terminal)
	out.Images = mergeImages(user.Images, project.Images)
	out.Markdown = mergeMarkdown(user.Markdown, project.Markdown)
	out.Bash = mergeBash(user.Bash, project.Bash)
	out.Read = mergeRead(user.Read, project.Read)
	out.BranchSummary = mergeBranchSummary(user.BranchSummary, project.BranchSummary)
	out.Models = mergeModelSettingsMap(user.Models, project.Models)

	// Plugins: project shadows user per-key. nil raw value = explicit removal.
	out.Plugins = make(map[string]json.RawMessage, len(user.Plugins)+len(project.Plugins))
	for k, v := range user.Plugins {
		out.Plugins[k] = v
	}
	for k, v := range project.Plugins {
		if v == nil {
			delete(out.Plugins, k)
		} else {
			out.Plugins[k] = v
		}
	}
	return &out
}

// DisableInstallTelemetryExplicit reports whether EnableInstallTelemetry was
// explicitly set to false on the project scope. Bools don't carry tri-state
// information in Go, so project must use a different mechanism to flip the
// default-on user setting off. For now, treat EnableInstallTelemetry as
// true-only-overridable, matching Pi's preference for "project sets true to
// override, otherwise user wins".
func (s *Settings) DisableInstallTelemetryExplicit() bool {
	return !s.EnableInstallTelemetry
}

func pickStr(user, project string) string {
	if project != "" {
		return project
	}
	return user
}

func mergeCompaction(u, p CompactionSettings) CompactionSettings {
	if p.Enabled {
		u.Enabled = true
	}
	if p.ReserveTokens != 0 {
		u.ReserveTokens = p.ReserveTokens
	}
	if p.KeepRecentTokens != 0 {
		u.KeepRecentTokens = p.KeepRecentTokens
	}
	return u
}

func mergeRetry(u, p RetrySettings) RetrySettings {
	if !u.Enabled {
		u.Enabled = p.Enabled
	}
	if p.Enabled {
		u.Enabled = true
	}
	if p.MaxRetries != 0 {
		u.MaxRetries = p.MaxRetries
	}
	if p.BaseDelayMs != 0 {
		u.BaseDelayMs = p.BaseDelayMs
	}
	if p.MaxDelayMs != 0 {
		u.MaxDelayMs = p.MaxDelayMs
	}
	return u
}

func mergeThinkingBudgetsSettings(u, p ThinkingBudgetsSettings) ThinkingBudgetsSettings {
	if p.Minimal != 0 {
		u.Minimal = p.Minimal
	}
	if p.Low != 0 {
		u.Low = p.Low
	}
	if p.Medium != 0 {
		u.Medium = p.Medium
	}
	if p.High != 0 {
		u.High = p.High
	}
	return u
}

func mergeTerminal(u, p TerminalSettings) TerminalSettings {
	if p.ShowImages != nil {
		u.ShowImages = p.ShowImages
	}
	if p.ClearOnShrink {
		u.ClearOnShrink = true
	}
	return u
}

func mergeImages(u, p ImageSettings) ImageSettings {
	if p.AutoResize != nil {
		u.AutoResize = p.AutoResize
	}
	if p.MaxWidth != 0 {
		u.MaxWidth = p.MaxWidth
	}
	if p.MaxHeight != 0 {
		u.MaxHeight = p.MaxHeight
	}
	if p.BlockImages {
		u.BlockImages = true
	}
	return u
}

func mergeMarkdown(u, p MarkdownSettings) MarkdownSettings {
	if p.CodeBlockIndent != "" {
		u.CodeBlockIndent = p.CodeBlockIndent
	}
	return u
}

func mergeBash(u, p BashSettings) BashSettings {
	if p.Shell != "" {
		u.Shell = p.Shell
	}
	if p.OutputCap != 0 {
		u.OutputCap = p.OutputCap
	}
	if len(p.EnvAllowlist) > 0 {
		u.EnvAllowlist = p.EnvAllowlist
	}
	if len(p.EnvExtra) > 0 {
		u.EnvExtra = p.EnvExtra
	}
	if p.CommandPrefix != "" {
		u.CommandPrefix = p.CommandPrefix
	}
	if p.StreamWindowMs != 0 {
		u.StreamWindowMs = p.StreamWindowMs
	}
	if p.KillGraceMs != 0 {
		u.KillGraceMs = p.KillGraceMs
	}
	return u
}

func mergeRead(u, p ReadSettings) ReadSettings {
	if p.LineCap != 0 {
		u.LineCap = p.LineCap
	}
	return u
}

func mergeBranchSummary(u, p BranchSummarySettings) BranchSummarySettings {
	if p.ReserveTokens != 0 {
		u.ReserveTokens = p.ReserveTokens
	}
	if p.SkipPrompt {
		u.SkipPrompt = true
	}
	return u
}

// mergeModelSettingsMap merges project per-model settings over user settings,
// per key and per field. Keys are "provider/model-id" strings.
func mergeModelSettingsMap(u, p map[string]ModelSettings) map[string]ModelSettings {
	if len(p) == 0 {
		return u
	}
	if u == nil {
		u = make(map[string]ModelSettings)
	}
	for key, ps := range p {
		us := u[key]
		if ps.ThinkingLevel != "" {
			us.ThinkingLevel = ps.ThinkingLevel
		}
		u[key] = us
	}
	return u
}

// ThinkingLevelFor returns the thinking level configured for the
// "provider/model-id" key, falling back to the global defaultThinkingLevel.
// Returns "" when neither is set to a valid level, so callers can apply their
// own default. Values are normalized to the lowercase ladder form (validation
// is case-insensitive, but the provider request path compares exactly), and
// invalid values are treated as unset rather than passed through: a typo in
// settings.json must not reach the request path.
func (s *Settings) ThinkingLevelFor(modelKey string) string {
	if s == nil {
		return ""
	}
	if m, ok := s.Models[modelKey]; ok && IsValidThinkingLevel(m.ThinkingLevel) {
		return strings.ToLower(m.ThinkingLevel)
	}
	if IsValidThinkingLevel(s.DefaultThinkingLevel) {
		return strings.ToLower(s.DefaultThinkingLevel)
	}
	return ""
}

// prettyJSON reformats compact JSON with 2-space indentation.
func prettyJSON(data []byte) ([]byte, error) {
	var any any
	if err := json.Unmarshal(data, &any); err != nil {
		return nil, err
	}
	return json.MarshalIndent(any, "", "  ")
}
