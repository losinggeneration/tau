// Package ai config file schema for ~/.tau/agent/settings.json and
// <cwd>/.tau/settings.json. Mirrors the Pi settings schema (Pi reference);
// only the tau prefix and a few Tau-specific tool knobs differ.
package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/losinggeneration/tau/paths"
)

// BashSettings controls bash tool behavior from settings.json.
type BashSettings struct {
	present map[string]json.RawMessage

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
	present map[string]json.RawMessage

	// LineCap is the maximum lines returned when no explicit range is given.
	// Defaults to 2000.
	LineCap int `json:"lineCap,omitempty"`
}

// ImageSettings controls image handling for file tools and model input.
type ImageSettings struct {
	present map[string]json.RawMessage

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
	present map[string]json.RawMessage

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
	present map[string]json.RawMessage

	// CodeBlockIndent is the indent used inside fenced code blocks.
	// Defaults to two spaces.
	CodeBlockIndent string `json:"codeBlockIndent,omitempty"`
}

// CompactionSettings controls auto-compaction. Matches agent/compaction.Settings.
// CompactionModelOverride provides explicit per-model token budgets.
type CompactionModelOverride struct {
	ReserveTokens    *int `json:"reserveTokens,omitempty"`
	KeepRecentTokens *int `json:"keepRecentTokens,omitempty"`
}

type CompactionSettings struct {
	present        map[string]json.RawMessage
	ModelOverrides map[string]CompactionModelOverride `json:"modelOverrides,omitempty"`
	// Enabled indicates whether compaction is active. Defaults to true.
	Enabled bool `json:"enabled,omitempty"`
	// ReserveTokens is the number of tokens reserved for the LLM prompt
	// and response during summarization. Defaults to 16384.
	ReserveTokens int `json:"reserveTokens,omitempty"`
	// KeepRecentTokens is the target number of recent tokens to keep
	// uncompacted when cutting history. Defaults to 20000.
	KeepRecentTokens int `json:"keepRecentTokens,omitempty"`
}

// ResolveCompaction resolves token budgets and rejects negative limits.
func (s *Settings) ResolveCompaction(model *Model) (CompactionSettings, error) {
	resolved := CompactionSettings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 20000}
	if s == nil {
		return resolved, nil
	}
	if s.Compaction.Enabled || s.Compaction.present["enabled"] != nil {
		resolved.Enabled = s.Compaction.Enabled
	}
	if s.Compaction.ReserveTokens < 0 || s.Compaction.KeepRecentTokens < 0 {
		return resolved, fmt.Errorf("compaction token settings must be non-negative")
	}
	if s.Compaction.ReserveTokens != 0 || s.Compaction.present["reserveTokens"] != nil {
		resolved.ReserveTokens = s.Compaction.ReserveTokens
	}
	if s.Compaction.KeepRecentTokens != 0 || s.Compaction.present["keepRecentTokens"] != nil {
		resolved.KeepRecentTokens = s.Compaction.KeepRecentTokens
	}
	if model != nil {
		override := s.Compaction.ModelOverrides[model.Provider+"/"+model.ID]
		if override.ReserveTokens != nil {
			resolved.ReserveTokens = *override.ReserveTokens
		}
		if override.KeepRecentTokens != nil {
			resolved.KeepRecentTokens = *override.KeepRecentTokens
		}
	}
	if resolved.ReserveTokens < 0 || resolved.KeepRecentTokens < 0 {
		return resolved, fmt.Errorf("compaction model token settings must be non-negative")
	}
	return resolved, nil
}

// RetrySettings controls automatic retry of transient failures.
type RetrySettings struct {
	present map[string]json.RawMessage
	// Enabled indicates whether automatic retry is active. Defaults to true.
	Enabled bool `json:"enabled,omitempty"`
	// MaxRetries is the maximum number of retry attempts. Defaults to 3.
	MaxRetries int `json:"maxRetries,omitempty"`
	// BaseDelayMs is the initial retry delay in milliseconds. Defaults to 2000.
	BaseDelayMs int `json:"baseDelayMs,omitempty"`
	// MaxAgentDelayMs caps agent-level exponential backoff. Defaults to 60000.
	MaxAgentDelayMs int `json:"maxAgentDelayMs,omitempty"`
	// Provider controls retries inside an individual provider request.
	Provider ProviderRetrySettings `json:"provider,omitempty"`
	// MaxDelayMs is the legacy provider delay limit, migrated on decoding.
	// Deprecated: use Provider.MaxRetryDelayMs.
	MaxDelayMs int `json:"maxDelayMs,omitempty"`
}

// ProviderRetrySettings configures provider requests independently of agent retries.
// Pointers preserve explicit zero values when merging settings scopes.
type ProviderRetrySettings struct {
	TimeoutMs       *int `json:"timeoutMs,omitempty"`
	MaxRetries      *int `json:"maxRetries,omitempty"`
	MaxRetryDelayMs *int `json:"maxRetryDelayMs,omitempty"`
}

// RequestOptions returns the explicitly configured provider options.
func (s ProviderRetrySettings) RequestOptions() []Option {
	var opts []Option
	if s.TimeoutMs != nil {
		opts = append(opts, WithTimeout(time.Duration(*s.TimeoutMs)*time.Millisecond))
	}
	if s.MaxRetries != nil {
		opts = append(opts, WithMaxRetries(*s.MaxRetries))
	}
	if s.MaxRetryDelayMs != nil {
		opts = append(opts, WithMaxRetryDelayMs(*s.MaxRetryDelayMs))
	}
	return opts
}

// ThinkingBudgetsSettings customizes token budgets per thinking level from
// settings.json. Named to avoid collision with the runtime ai.ThinkingBudgets
// type used for per-call budget overrides.
type ThinkingBudgetsSettings struct {
	present map[string]json.RawMessage

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
	present map[string]json.RawMessage

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
//
//nolint:recvcheck // MarshalJSON takes a value so Settings values keep explicit zeros.
type Settings struct {
	present       map[string]json.RawMessage
	resourceBases map[string]string

	// === Top-level scalars ===

	LastChangelogVersion string `json:"lastChangelogVersion,omitempty"`
	DefaultProvider      string `json:"defaultProvider,omitempty"`
	DefaultModel         string `json:"defaultModel,omitempty"`
	DefaultThinkingLevel string `json:"defaultThinkingLevel,omitempty"`
	Transport            string `json:"transport,omitempty"` // auto | sse | websocket | websocket-cached
	SteeringMode         string `json:"steeringMode,omitempty"`
	FollowUpMode         string `json:"followUpMode,omitempty"`
	Theme                string `json:"theme,omitempty"`
	HideThinkingBlock    bool   `json:"hideThinkingBlock,omitempty"`
	ShellPath            string `json:"shellPath,omitempty"`
	QuietStartup         bool   `json:"quietStartup,omitempty"`
	// LayoutMode selects the interactive region arrangement: "fixed" (header
	// and editor pinned, chat scrolls on its own) or "document" (all regions in
	// one bottom-anchored scrolling document). Empty uses the default.
	LayoutMode                 string          `json:"layoutMode,omitempty"`
	ShellCommandPrefix         string          `json:"shellCommandPrefix,omitempty"`
	CollapseChangelog          bool            `json:"collapseChangelog,omitempty"`
	EnableInstallTelemetry     bool            `json:"enableInstallTelemetry,omitempty"`
	FullscreenWheelScrollLines json.RawMessage `json:"fullscreenWheelScrollLines,omitempty"`
	ShowHardwareCursor         bool            `json:"showHardwareCursor,omitempty"`
	EditorPaddingX             int             `json:"editorPaddingX,omitempty"`
	AutocompleteMaxVisible     int             `json:"autocompleteMaxVisible,omitempty"`
	EnableSkillCommands        bool            `json:"enableSkillCommands,omitempty"`
	DoubleEscapeAction         string          `json:"doubleEscapeAction,omitempty"` // fork | tree | none
	TreeFilterMode             string          `json:"treeFilterMode,omitempty"`     // default|no-tools|user-only|labeled-only|all
	SessionDir                 string          `json:"sessionDir,omitempty"`

	// Directory overrides for shared or split installs (e.g. ~/.tau.work
	// alongside ~/.tau, or skills shared between them). Each field is one
	// level below the matching TAU_*_DIR environment variable and one above
	// the default under the config dir. packageDir overrides the user
	// packages directory, extensionDir the user extensions directory, and
	// skillDir/promptDir/themeDir the user artifact directories.
	PackageDir   string `json:"packageDir,omitempty"`
	ExtensionDir string `json:"extensionDir,omitempty"`
	SkillDir     string `json:"skillDir,omitempty"`
	PromptDir    string `json:"promptDir,omitempty"`
	ThemeDir     string `json:"themeDir,omitempty"`

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
	DefaultTools  []string        `json:"defaultTools,omitzero"`

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
	settings, err := parseSettings(data)
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve settings path: %w", err)
	}
	settings.resourceBases = make(map[string]string)
	for _, kind := range []string{"skills", "prompts", "themes", "extensions"} {
		settings.resourceBases[kind] = filepath.Dir(absolute)
	}
	return settings, nil
}

// ResourcePaths returns resource entries resolved relative to their originating
// settings file. The stored values remain unchanged for persistence.
func (s *Settings) ResourcePaths(kind string) []string {
	if s == nil {
		return nil
	}
	var entries []string
	switch kind {
	case "skills":
		entries = s.Skills
	case "prompts":
		entries = s.Prompts
	case "themes":
		entries = s.Themes
	case "extensions":
		entries = s.Extensions
	default:
		return nil
	}
	if entries == nil {
		return nil
	}
	result := make([]string, len(entries))
	for i, entry := range entries {
		result[i] = paths.ResourcePath(entry, s.resourceBases[kind], "")
	}
	return result
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
	if raw, ok := s.Plugins["websockets"]; ok && probe["transport"] == nil {
		var websockets bool
		if json.Unmarshal(raw, &websockets) == nil {
			s.Transport = string(TransportSSE)
			if websockets {
				s.Transport = string(TransportWS)
			}
			delete(s.Plugins, "websockets")
		}
	}
	s.DefaultProvider = canonicalProvider(s.DefaultProvider)
	const legacyAzure = "azure-openai-responses/"
	for i, pattern := range s.EnabledModels {
		if rest, ok := strings.CutPrefix(pattern, legacyAzure); ok {
			s.EnabledModels[i] = ProviderAzureOpenAI + "/" + rest
		}
	}
	for key, value := range s.Models {
		if rest, ok := strings.CutPrefix(key, legacyAzure); ok {
			migrated := ProviderAzureOpenAI + "/" + rest
			if _, exists := s.Models[migrated]; !exists {
				s.Models[migrated] = value
			}
			delete(s.Models, key)
		}
	}

	return &s, nil
}

func knownSettingsKey(k string) (struct{}, bool) {
	v, ok := knownSettingsKeyMap[k]
	return v, ok
}

var knownSettingsKeyMap = func() map[string]struct{} {
	m := make(map[string]struct{})
	t := reflect.TypeFor[Settings]()
	for i := range t.NumField() {
		field := t.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			m[name] = struct{}{}
		}
	}
	return m
}()

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
	out.resourceBases = maps.Clone(user.resourceBases)
	if out.resourceBases == nil {
		out.resourceBases = make(map[string]string)
	}
	for kind, entries := range map[string][]string{
		"skills": project.Skills, "prompts": project.Prompts, "themes": project.Themes, "extensions": project.Extensions,
	} {
		if entries != nil {
			out.resourceBases[kind] = project.resourceBases[kind]
		}
	}

	out.LastChangelogVersion = pickStr(user.LastChangelogVersion, project.LastChangelogVersion)
	out.DefaultProvider = pickStr(user.DefaultProvider, project.DefaultProvider)
	out.DefaultModel = pickStr(user.DefaultModel, project.DefaultModel)
	out.DefaultThinkingLevel = pickStr(user.DefaultThinkingLevel, project.DefaultThinkingLevel)
	out.Transport = pickStr(user.Transport, project.Transport)
	if project.FullscreenWheelScrollLines != nil {
		out.FullscreenWheelScrollLines = slices.Clone(project.FullscreenWheelScrollLines)
	}
	out.SteeringMode = pickStr(user.SteeringMode, project.SteeringMode)
	out.FollowUpMode = pickStr(user.FollowUpMode, project.FollowUpMode)
	out.Theme = pickStr(user.Theme, project.Theme)
	out.ShellPath = pickStr(user.ShellPath, project.ShellPath)
	out.ShellCommandPrefix = pickStr(user.ShellCommandPrefix, project.ShellCommandPrefix)
	out.DoubleEscapeAction = pickStr(user.DoubleEscapeAction, project.DoubleEscapeAction)
	out.LayoutMode = pickStr(user.LayoutMode, project.LayoutMode)
	out.TreeFilterMode = pickStr(user.TreeFilterMode, project.TreeFilterMode)
	out.SessionDir = pickStr(user.SessionDir, project.SessionDir)
	out.PackageDir = pickStr(user.PackageDir, project.PackageDir)
	out.ExtensionDir = pickStr(user.ExtensionDir, project.ExtensionDir)
	out.SkillDir = pickStr(user.SkillDir, project.SkillDir)
	out.PromptDir = pickStr(user.PromptDir, project.PromptDir)
	out.ThemeDir = pickStr(user.ThemeDir, project.ThemeDir)

	// Non-zero programmatic values override without JSON presence metadata.
	if project.HideThinkingBlock {
		out.HideThinkingBlock = true
	}
	if project.QuietStartup {
		out.QuietStartup = true
	}
	if project.CollapseChangelog {
		out.CollapseChangelog = true
	}
	if project.EnableInstallTelemetry || project.DisableInstallTelemetryExplicit() {
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

	// Explicit JSON scalars override even when their current value is zero.
	destination, source := reflect.ValueOf(&out).Elem(), reflect.ValueOf(project).Elem()
	for i := range source.NumField() {
		field := source.Type().Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if project.present[name] == nil {
			continue
		}
		switch source.Field(i).Kind() {
		case reflect.Bool, reflect.Int, reflect.String:
			destination.Field(i).Set(source.Field(i))
		}
	}
	out.present = mergeSettingsPresence(user.present, project.present)

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

	if project.DefaultTools != nil {
		out.DefaultTools = slices.Clone(project.DefaultTools)
		modifiersOnly := true
		for _, entry := range project.DefaultTools {
			if !strings.HasPrefix(entry, "+") && !strings.HasPrefix(entry, "-") {
				modifiersOnly = false
				break
			}
		}
		if user.DefaultTools != nil && len(project.DefaultTools) > 0 && modifiersOnly {
			out.DefaultTools = append(slices.Clone(user.DefaultTools), project.DefaultTools...)
		}
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

// ResolveTransport returns the configured provider transport, defaulting to
// auto. Tau has no cached-context WebSocket mode, so websocket-cached uses the
// plain WebSocket transport.
func (s *Settings) ResolveTransport() (Transport, error) {
	if s == nil || s.Transport == "" {
		return TransportAuto, nil
	}
	switch Transport(s.Transport) {
	case TransportAuto, TransportSSE, TransportWS:
		return Transport(s.Transport), nil
	case "websocket-cached":
		return TransportWS, nil
	}
	return "", fmt.Errorf("invalid transport %q", s.Transport)
}

// SkillCommandsEnabled reports whether skills appear as slash commands. It
// defaults to true; only an explicit false disables them.
func (s *Settings) SkillCommandsEnabled() bool {
	return s == nil || s.EnableSkillCommands || s.present["enableSkillCommands"] == nil
}

// ResolveCodeBlockIndent returns the indent for code block lines. It defaults
// to two spaces; an explicit empty string removes the indent.
func (s *Settings) ResolveCodeBlockIndent() string {
	if s == nil || (s.Markdown.CodeBlockIndent == "" && s.Markdown.present["codeBlockIndent"] == nil) {
		return "  "
	}
	return s.Markdown.CodeBlockIndent
}

// ResolveEditorPaddingX returns the prompt editor's horizontal padding. Tau
// defaults to one column; an explicit zero removes it.
func (s *Settings) ResolveEditorPaddingX() int {
	if s == nil || (s.EditorPaddingX == 0 && s.present["editorPaddingX"] == nil) {
		return 1
	}
	return max(s.EditorPaddingX, 0)
}

// ResolveAutocompleteMaxVisible returns the autocomplete row limit, defaulting
// to five as upstream does.
func (s *Settings) ResolveAutocompleteMaxVisible() int {
	if s == nil || s.AutocompleteMaxVisible <= 0 {
		return 5
	}
	return s.AutocompleteMaxVisible
}

// DisableInstallTelemetryExplicit reports an explicit JSON opt-out.
func (s *Settings) DisableInstallTelemetryExplicit() bool {
	return s != nil && !s.EnableInstallTelemetry && s.present["enableInstallTelemetry"] != nil
}

func pickStr(user, project string) string {
	if project != "" {
		return project
	}
	return user
}

func mergeCompaction(u, p CompactionSettings) CompactionSettings {
	overrides := maps.Clone(u.ModelOverrides)
	if overrides == nil {
		overrides = make(map[string]CompactionModelOverride)
	}
	for key, value := range p.ModelOverrides {
		existing := overrides[key]
		if value.ReserveTokens != nil {
			existing.ReserveTokens = value.ReserveTokens
		}
		if value.KeepRecentTokens != nil {
			existing.KeepRecentTokens = value.KeepRecentTokens
		}
		overrides[key] = existing
	}
	u.ModelOverrides = overrides
	u.present = mergeSettingsPresence(u.present, p.present)

	if p.Enabled || p.present["enabled"] != nil {
		u.Enabled = p.Enabled
	}
	if p.ReserveTokens != 0 || p.present["reserveTokens"] != nil {
		u.ReserveTokens = p.ReserveTokens
	}
	if p.KeepRecentTokens != 0 || p.present["keepRecentTokens"] != nil {
		u.KeepRecentTokens = p.KeepRecentTokens
	}
	return u
}

func mergeRetry(u, p RetrySettings) RetrySettings {
	if p.Enabled || p.present["enabled"] != nil {
		u.Enabled = p.Enabled
	}
	if p.MaxRetries != 0 || p.present["maxRetries"] != nil {
		u.MaxRetries = p.MaxRetries
	}
	if p.BaseDelayMs != 0 || p.present["baseDelayMs"] != nil {
		u.BaseDelayMs = p.BaseDelayMs
	}
	if p.MaxDelayMs != 0 || p.present["maxDelayMs"] != nil {
		u.MaxDelayMs = p.MaxDelayMs
	}
	if p.MaxAgentDelayMs != 0 || p.present["maxAgentDelayMs"] != nil {
		u.MaxAgentDelayMs = p.MaxAgentDelayMs
	}
	if p.Provider.TimeoutMs != nil {
		u.Provider.TimeoutMs = p.Provider.TimeoutMs
	}
	if p.Provider.MaxRetries != nil {
		u.Provider.MaxRetries = p.Provider.MaxRetries
	}
	if p.Provider.MaxRetryDelayMs != nil {
		u.Provider.MaxRetryDelayMs = p.Provider.MaxRetryDelayMs
	}
	u.present = mergeSettingsPresence(u.present, p.present)
	return u
}

func mergeThinkingBudgetsSettings(u, p ThinkingBudgetsSettings) ThinkingBudgetsSettings {
	u.present = mergeSettingsPresence(u.present, p.present)
	if p.Minimal != 0 || p.present["minimal"] != nil {
		u.Minimal = p.Minimal
	}
	if p.Low != 0 || p.present["low"] != nil {
		u.Low = p.Low
	}
	if p.Medium != 0 || p.present["medium"] != nil {
		u.Medium = p.Medium
	}
	if p.High != 0 || p.present["high"] != nil {
		u.High = p.High
	}
	return u
}

func mergeTerminal(u, p TerminalSettings) TerminalSettings {
	u.present = mergeSettingsPresence(u.present, p.present)
	if p.ShowImages != nil {
		u.ShowImages = p.ShowImages
	}
	if p.ClearOnShrink || p.present["clearOnShrink"] != nil {
		u.ClearOnShrink = p.ClearOnShrink
	}
	return u
}

func mergeImages(u, p ImageSettings) ImageSettings {
	u.present = mergeSettingsPresence(u.present, p.present)
	if p.AutoResize != nil {
		u.AutoResize = p.AutoResize
	}
	if p.MaxWidth != 0 || p.present["maxWidth"] != nil {
		u.MaxWidth = p.MaxWidth
	}
	if p.MaxHeight != 0 || p.present["maxHeight"] != nil {
		u.MaxHeight = p.MaxHeight
	}
	if p.BlockImages || p.present["blockImages"] != nil {
		u.BlockImages = p.BlockImages
	}
	return u
}

func mergeMarkdown(u, p MarkdownSettings) MarkdownSettings {
	u.present = mergeSettingsPresence(u.present, p.present)
	if p.CodeBlockIndent != "" || p.present["codeBlockIndent"] != nil {
		u.CodeBlockIndent = p.CodeBlockIndent
	}
	return u
}

func mergeBash(u, p BashSettings) BashSettings {
	u.present = mergeSettingsPresence(u.present, p.present)
	if p.Shell != "" || p.present["shell"] != nil {
		u.Shell = p.Shell
	}
	if p.OutputCap != 0 || p.present["outputCap"] != nil {
		u.OutputCap = p.OutputCap
	}
	if p.EnvAllowlist != nil {
		u.EnvAllowlist = p.EnvAllowlist
	}
	if p.EnvExtra != nil {
		u.EnvExtra = p.EnvExtra
	}
	if p.CommandPrefix != "" || p.present["commandPrefix"] != nil {
		u.CommandPrefix = p.CommandPrefix
	}
	if p.StreamWindowMs != 0 || p.present["streamWindowMs"] != nil {
		u.StreamWindowMs = p.StreamWindowMs
	}
	if p.KillGraceMs != 0 || p.present["killGraceMs"] != nil {
		u.KillGraceMs = p.KillGraceMs
	}
	return u
}

func mergeRead(u, p ReadSettings) ReadSettings {
	u.present = mergeSettingsPresence(u.present, p.present)
	if p.LineCap != 0 || p.present["lineCap"] != nil {
		u.LineCap = p.LineCap
	}
	return u
}

func mergeBranchSummary(u, p BranchSummarySettings) BranchSummarySettings {
	u.present = mergeSettingsPresence(u.present, p.present)
	if p.ReserveTokens != 0 || p.present["reserveTokens"] != nil {
		u.ReserveTokens = p.ReserveTokens
	}
	if p.SkipPrompt || p.present["skipPrompt"] != nil {
		u.SkipPrompt = p.SkipPrompt
	}
	return u
}

// mergeModelSettingsMap merges project per-model settings over user settings,
// per key and per field. Keys are "provider/model-id" strings.
func mergeModelSettingsMap(u, p map[string]ModelSettings) map[string]ModelSettings {
	u = maps.Clone(u)
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

func mergeSettingsPresence(base, override map[string]json.RawMessage) map[string]json.RawMessage {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]json.RawMessage{}
	}
	maps.Copy(out, override)
	return out
}

func marshalPresentSettings(value any, present map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	current := reflect.ValueOf(value)
	for i := range current.NumField() {
		field := current.Type().Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if present[name] == nil || fields[name] != nil {
			continue
		}
		raw, err := json.Marshal(current.Field(i).Interface())
		if err != nil {
			return nil, fmt.Errorf("marshal settings field %s: %w", name, err)
		}
		fields[name] = raw
	}
	return json.Marshal(fields)
}

func (s *CompactionSettings) UnmarshalJSON(data []byte) error {
	type plain CompactionSettings
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = CompactionSettings(value)
	return json.Unmarshal(data, &s.present)
}

func (s CompactionSettings) MarshalJSON() ([]byte, error) {
	type plain CompactionSettings
	return marshalPresentSettings(plain(s), s.present)
}

func (s *RetrySettings) UnmarshalJSON(data []byte) error {
	type plain RetrySettings
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = RetrySettings(value)
	if err := json.Unmarshal(data, &s.present); err != nil {
		return err
	}
	if s.present["maxDelayMs"] != nil {
		if s.Provider.MaxRetryDelayMs == nil && string(s.present["maxDelayMs"]) != "null" {
			s.Provider.MaxRetryDelayMs = new(s.MaxDelayMs)
		}
		s.MaxDelayMs = 0
		delete(s.present, "maxDelayMs")
	}
	return nil
}

func (s RetrySettings) MarshalJSON() ([]byte, error) {
	type plain RetrySettings
	return marshalPresentSettings(plain(s), s.present)
}

// ResolveRetry applies defaults while preserving explicit false and zero settings.
func (s *Settings) ResolveRetry() RetrySettings {
	resolved := RetrySettings{Enabled: true, MaxRetries: 3, BaseDelayMs: 2000, MaxAgentDelayMs: 60000, Provider: ProviderRetrySettings{MaxRetryDelayMs: new(60000)}}
	if s != nil {
		resolved = mergeRetry(resolved, s.Retry)
		if s.Retry.MaxDelayMs != 0 && s.Retry.Provider.MaxRetryDelayMs == nil {
			resolved.Provider.MaxRetryDelayMs = new(s.Retry.MaxDelayMs)
		}
	}
	return resolved
}

func (s *ImageSettings) UnmarshalJSON(data []byte) error {
	type plain ImageSettings
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = ImageSettings(value)
	return json.Unmarshal(data, &s.present)
}

func (s ImageSettings) MarshalJSON() ([]byte, error) {
	type plain ImageSettings
	return marshalPresentSettings(plain(s), s.present)
}

func (s *TerminalSettings) UnmarshalJSON(data []byte) error {
	type plain TerminalSettings
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = TerminalSettings(value)
	return json.Unmarshal(data, &s.present)
}

func (s TerminalSettings) MarshalJSON() ([]byte, error) {
	type plain TerminalSettings
	return marshalPresentSettings(plain(s), s.present)
}

func (s *Settings) UnmarshalJSON(data []byte) error {
	type plain Settings
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = Settings(value)
	return json.Unmarshal(data, &s.present)
}

func (s Settings) MarshalJSON() ([]byte, error) {
	type plain Settings
	return marshalPresentSettings(plain(s), s.present)
}

func (s *BashSettings) UnmarshalJSON(data []byte) error {
	type plain BashSettings
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = BashSettings(value)
	return json.Unmarshal(data, &s.present)
}

func (s BashSettings) MarshalJSON() ([]byte, error) {
	type plain BashSettings
	return marshalPresentSettings(plain(s), s.present)
}

func (s *ReadSettings) UnmarshalJSON(data []byte) error {
	type plain ReadSettings
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = ReadSettings(value)
	return json.Unmarshal(data, &s.present)
}

func (s ReadSettings) MarshalJSON() ([]byte, error) {
	type plain ReadSettings
	return marshalPresentSettings(plain(s), s.present)
}

func (s *MarkdownSettings) UnmarshalJSON(data []byte) error {
	type plain MarkdownSettings
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = MarkdownSettings(value)
	return json.Unmarshal(data, &s.present)
}

func (s MarkdownSettings) MarshalJSON() ([]byte, error) {
	type plain MarkdownSettings
	return marshalPresentSettings(plain(s), s.present)
}

func (s *BranchSummarySettings) UnmarshalJSON(data []byte) error {
	type plain BranchSummarySettings
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = BranchSummarySettings(value)
	return json.Unmarshal(data, &s.present)
}

func (s BranchSummarySettings) MarshalJSON() ([]byte, error) {
	type plain BranchSummarySettings
	return marshalPresentSettings(plain(s), s.present)
}

func (s *ThinkingBudgetsSettings) UnmarshalJSON(data []byte) error {
	type plain ThinkingBudgetsSettings
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = ThinkingBudgetsSettings(value)
	return json.Unmarshal(data, &s.present)
}

func (s ThinkingBudgetsSettings) MarshalJSON() ([]byte, error) {
	type plain ThinkingBudgetsSettings
	return marshalPresentSettings(plain(s), s.present)
}

// ResolveThinkingBudgets returns only the explicitly configured budgets.
func (s *Settings) ResolveThinkingBudgets() *ThinkingBudgets {
	if s == nil {
		return nil
	}
	budgets := &ThinkingBudgets{}
	value := s.ThinkingBudgets
	if value.Minimal != 0 || value.present["minimal"] != nil {
		budgets.Minimal = new(value.Minimal)
	}
	if value.Low != 0 || value.present["low"] != nil {
		budgets.Low = new(value.Low)
	}
	if value.Medium != 0 || value.present["medium"] != nil {
		budgets.Medium = new(value.Medium)
	}
	if value.High != 0 || value.present["high"] != nil {
		budgets.High = new(value.High)
	}
	return budgets
}

// ResolveFullscreenWheelScrollLines returns 1-100 lines, or zero for auto.
// Unknown values use auto, matching upstream's settings getter.
func (s *Settings) ResolveFullscreenWheelScrollLines() int {
	if s == nil {
		return 0
	}
	var lines float64
	if err := json.Unmarshal(s.FullscreenWheelScrollLines, &lines); err != nil || bytes.Equal(bytes.TrimSpace(s.FullscreenWheelScrollLines), []byte("null")) {
		return 0
	}
	return int(min(100, max(1, lines)))
}
