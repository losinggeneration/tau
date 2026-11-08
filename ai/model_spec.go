package ai // import "github.com/losinggeneration/tau/ai"

import (
	"cmp"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// ThinkingLevels lists the valid thinking levels in ascending order of effort.
// The order is meaningful: it is the ladder the app.thinking.cycle action steps
// through and the order the settings list offers them in, so both read it from
// here rather than each keeping a copy that can drift.
var ThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// ValidThinkingLevels contains all valid thinking level values, derived from
// ThinkingLevels so the set and the ladder cannot disagree.
var ValidThinkingLevels = func() map[string]bool {
	m := make(map[string]bool, len(ThinkingLevels))
	for _, level := range ThinkingLevels {
		m[level] = true
	}
	return m
}()

// DefaultModelPerProvider maps each known provider to their default model ID.
var DefaultModelPerProvider = map[string]string{
	"amazon-bedrock":             "us.anthropic.claude-opus-4-6-v1",
	"ant-ling":                   "Ring-2.6-1T",
	"anthropic":                  "claude-opus-4-8",
	"openai":                     "gpt-6.1-sol",
	"azure":                      "gpt-5.4",
	"openai-codex":               "gpt-6.1-sol",
	"radius":                     "balanced",
	"nvidia":                     "nvidia/nemotron-3.5-super-vl-120b-a12b",
	"deepseek":                   "deepseek-v4-pro",
	"google":                     "gemini-3.1-pro-preview",
	"google-vertex":              "gemini-3.1-pro-preview",
	"github-copilot":             "gpt-6.1-sol",
	"openrouter":                 "moonshotai/kimi-k2.6",
	"vercel-ai-gateway":          "zai/glm-5.1",
	"xai":                        "grok-4.7",
	"groq":                       "openai/gpt-oss-120b",
	"cerebras":                   "gpt-oss-120b",
	"zai":                        "glm-5.3",
	"zai-coding-cn":              "glm-5.3",
	"mistral":                    "devstral-medium-latest",
	"minimax":                    "MiniMax-M2.7",
	"minimax-cn":                 "MiniMax-M2.7",
	"moonshotai":                 "kimi-k2.6",
	"moonshotai-cn":              "kimi-k2.6",
	"huggingface":                "moonshotai/Kimi-K2.6",
	"fireworks":                  "accounts/fireworks/models/kimi-k3",
	"together":                   "moonshotai/Kimi-K3",
	"baseten":                    "zai-org/GLM-5.2",
	"opencode":                   "kimi-k2.6",
	"opencode-go":                "kimi-k3",
	"kimi-coding":                "kimi-for-coding",
	"meta":                       "muse-spark-1.3",
	"cloudflare-workers-ai":      "@cf/moonshotai/kimi-k2.6",
	"cloudflare-ai-gateway":      "workers-ai/@cf/moonshotai/kimi-k2.6",
	"qwen-token-plan":            "qwen3.7-max",
	"qwen-token-plan-cn":         "qwen3.7-max",
	"qwen-token-plan-individual": "qwen3.8-max",
	"xiaomi":                     "mimo-v2.6-pro",
	"xiaomi-token-plan-cn":       "mimo-v2.6-pro",
	"xiaomi-token-plan-ams":      "mimo-v2.6-pro",
	"xiaomi-token-plan-sgp":      "mimo-v2.6-pro",
	"google-gemini-cli":          "gemini-2.5-pro",
	"google-antigravity":         "gemini-3.1-pro-high",
}
var errNoModelPatternMatch = errors.New("no models matched pattern")

// IsValidThinkingLevel reports whether the given string is a valid thinking level.
func IsValidThinkingLevel(level string) bool {
	return ValidThinkingLevels[strings.ToLower(level)]
}

// ParsedModelResult contains the result of parsing a model pattern with optional thinking level.
type ParsedModelResult struct {
	// Spec is the resolved model spec.
	Spec ModelSpec
	// ThinkingLevel is the thinking level if explicitly specified via suffix, empty otherwise.
	ThinkingLevel string
	// Warning contains an optional warning message.
	Warning string
}

// ParseModelPattern parses a model pattern string, extracting optional thinking level suffix.
// Supports patterns like "claude-sonnet-4:high" where ":high" is a thinking level suffix.
// If the suffix is a valid thinking level, it's extracted and the prefix is used for model matching.
// If the suffix is invalid, the pattern is used as-is (for models with colons in their IDs like OpenRouter's "model:extended").
//
// The strictMode parameter controls behavior when an invalid thinking level suffix is found:
// - In strictMode (true): the suffix is treated as part of the model ID and no fallback occurs
// - In non-strict mode (false): the suffix is stripped and a warning is returned
func ParseModelPattern(pattern string, candidates []ModelSpec, strictMode bool) ParsedModelResult {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ParsedModelResult{Spec: ModelSpec{}, Warning: "empty pattern"}
	}

	// Try to match the pattern as-is first
	if match, warning := tryMatchModelPattern(pattern, candidates); match.Model != "" {
		return ParsedModelResult{
			Spec:          match,
			ThinkingLevel: "",
			Warning:       warning,
		}
	}

	// No match - try splitting on last colon if present
	lastColonIndex := strings.LastIndex(pattern, ":")
	if lastColonIndex == -1 {
		// No colons, pattern simply doesn't match any model
		return ParsedModelResult{Spec: ModelSpec{}, Warning: fmt.Sprintf("no models match pattern %q", pattern)}
	}

	prefix := pattern[:lastColonIndex]
	suffix := pattern[lastColonIndex+1:]

	if IsValidThinkingLevel(suffix) {
		// Valid thinking level - try matching the prefix with this level
		prefixLower := strings.ToLower(prefix)
		for _, candidate := range candidates {
			candidateLower := strings.ToLower(candidate.String())
			modelLower := strings.ToLower(candidate.Model)
			if candidateLower == prefixLower || modelLower == prefixLower {
				return ParsedModelResult{
					Spec:          candidate,
					ThinkingLevel: strings.ToLower(suffix),
					Warning:       "",
				}
			}
		}
		// Also try with provider/model format
		slashIdx := strings.Index(prefix, "/")
		if slashIdx != -1 {
			provider := strings.ToLower(prefix[:slashIdx])
			modelPart := prefix[slashIdx+1:]
			modelPartLower := strings.ToLower(modelPart)
			for _, candidate := range candidates {
				if strings.ToLower(candidate.Provider) == provider && strings.ToLower(candidate.Model) == modelPartLower {
					return ParsedModelResult{
						Spec:          candidate,
						ThinkingLevel: strings.ToLower(suffix),
						Warning:       "",
					}
				}
			}
		}

		// Try without the thinking level suffix but with warning
		if match, _ := tryMatchModelPattern(prefix, candidates); match.Model != "" {
			return ParsedModelResult{
				Spec:          match,
				ThinkingLevel: "",
				Warning:       fmt.Sprintf("invalid thinking level %q in pattern %q", suffix, pattern),
			}
		}
		return ParsedModelResult{Spec: ModelSpec{}, Warning: fmt.Sprintf("no models match pattern %q", pattern)}
	}

	// Invalid suffix - in strict mode, treat it as part of the model id and return no match
	if strictMode {
		return ParsedModelResult{Spec: ModelSpec{}, Warning: fmt.Sprintf("no models match pattern %q", pattern)}
	}

	// Non-strict mode: recurse on prefix and warn
	result := ParseModelPattern(prefix, candidates, strictMode)
	if result.Spec.Model != "" {
		result.Warning = fmt.Sprintf("invalid thinking level %q in pattern %q", suffix, pattern)
		result.ThinkingLevel = ""
	}
	return result
}

// tryMatchModelPattern attempts to match a pattern to a model from candidates.
// Returns the best match and an optional warning.
func tryMatchModelPattern(pattern string, candidates []ModelSpec) (ModelSpec, string) {
	patternLower := strings.ToLower(strings.TrimSpace(pattern))
	if patternLower == "" {
		return ModelSpec{}, ""
	}

	// Exact match on fully qualified or provider/model
	var exactMatches []ModelSpec
	for _, c := range candidates {
		candidateLower := strings.ToLower(c.String())
		providerModelLower := strings.ToLower(c.Provider + "/" + c.Model)
		modelLower := strings.ToLower(c.Model)
		if candidateLower == patternLower || providerModelLower == patternLower || modelLower == patternLower {
			exactMatches = append(exactMatches, c)
		}
	}
	if len(exactMatches) == 1 {
		return exactMatches[0], ""
	}
	if len(exactMatches) > 1 {
		// Multiple exact matches - apply alias preference
		return selectBestModelMatch(patternLower, exactMatches), ""
	}

	// Fuzzy/substring match
	var fuzzyMatches []ModelSpec
	for _, c := range candidates {
		candidateLower := strings.ToLower(c.String())
		providerModelLower := strings.ToLower(c.Provider + "/" + c.Model)
		modelLower := strings.ToLower(c.Model)
		if strings.Contains(candidateLower, patternLower) ||
			strings.Contains(providerModelLower, patternLower) ||
			strings.Contains(modelLower, patternLower) {
			fuzzyMatches = append(fuzzyMatches, c)
		}
	}
	if len(fuzzyMatches) == 0 {
		return ModelSpec{}, ""
	}
	if len(fuzzyMatches) > 1 {
		return selectBestModelMatch(patternLower, fuzzyMatches), ""
	}
	return fuzzyMatches[0], ""
}

// selectBestModelMatch selects the best model when multiple matches exist.
// Prefers alias models (no date suffix) over dated versions.
// When multiple aliases exist, picks the one that sorts highest.
// When multiple dated versions exist, picks the latest (highest sort).
func selectBestModelMatch(pattern string, matches []ModelSpec) ModelSpec {
	aliases := []ModelSpec{}
	datedVersions := []ModelSpec{}

	for _, m := range matches {
		if IsAliasModelID(m.Model) {
			aliases = append(aliases, m)
		} else {
			datedVersions = append(datedVersions, m)
		}
	}

	if len(aliases) > 0 {
		// Prefer alias - if multiple, pick the one that sorts highest
		slices.SortFunc(aliases, func(a, b ModelSpec) int {
			return cmp.Compare(b.Model, a.Model)
		})
		return aliases[0]
	}

	// No alias found, pick latest dated version
	slices.SortFunc(datedVersions, func(a, b ModelSpec) int {
		return cmp.Compare(b.Model, a.Model)
	})
	return datedVersions[0]
}

// datePattern matches date suffixes like -20241022 or -20250929
var datePattern = regexp.MustCompile(`-\d{8}$`)

// IsAliasModelID reports whether a model ID looks like an alias (no date suffix).
// Dates are typically in format: -YYYYMMDD
// Alias IDs typically end in "-latest" or have no date at all.
func IsAliasModelID(id string) bool {
	// Check if ID ends with -latest
	if strings.HasSuffix(id, "-latest") {
		return true
	}
	// Check if ID ends with a date pattern (-YYYYMMDD)
	return !datePattern.MatchString(id)
}

// ModelSpec represents a parsed model specifier in one of these forms:
//   - <provider>/<model>            (preferred provider-key form)
//   - <provider>/<legacy>/<model>   (legacy three-segment form, middle segment accepted but ignored)
//   - <model>                       (search configured providers)
type ModelSpec struct {
	Provider string
	Model    string
}

// ParseModelSpec parses a model specifier string into its components.
// It accepts three forms:
//   - "provider/model"        → preferred provider-key form
//   - "provider/legacy/model" → legacy three-segment form (middle segment silently discarded)
//   - "model"                 → provider empty (caller resolves)
func ParseModelSpec(spec string) (ModelSpec, error) {
	if spec == "" {
		return ModelSpec{}, fmt.Errorf("empty model specifier")
	}

	parts := strings.Split(spec, "/")
	switch len(parts) {
	case 3:
		// Legacy three-segment form: drop the middle (account) segment.
		return ModelSpec{Provider: canonicalProvider(parts[0]), Model: parts[2]}, nil
	case 2:
		return ModelSpec{Provider: canonicalProvider(parts[0]), Model: parts[1]}, nil
	case 1:
		return ModelSpec{Model: parts[0]}, nil
	default:
		return ModelSpec{}, fmt.Errorf("invalid model specifier %q: expected 1-3 slash-separated parts, got %d", spec, len(parts))
	}
}

// String returns the canonical "<provider>/<model>" form,
// or just the model if no provider is set.
func (s ModelSpec) String() string {
	if s.Provider == "" {
		return s.Model
	}
	return s.Provider + "/" + s.Model
}

// IsFullyQualified reports whether the spec has both provider and model.
func (s ModelSpec) IsFullyQualified() bool {
	return s.Provider != "" && s.Model != ""
}

// ResolveShortModel attempts to resolve a bare model name. It first checks
// the settings default; if that matches, it wins. Otherwise it searches
// all built-in models across providers. Returns an error if ambiguous.
func ResolveShortModel(modelName string, settings *Settings) (ModelSpec, error) {
	if settings != nil && settings.DefaultModel == modelName && settings.DefaultProvider != "" {
		return ModelSpec{
			Provider: settings.DefaultProvider,
			Model:    settings.DefaultModel,
		}, nil
	}

	var candidates []ModelSpec
	for _, provider := range ListProviders() {
		for _, model := range ListAvailableModels(provider) {
			if model == nil || model.ID == "" {
				continue
			}
			if model.ID == modelName {
				candidates = append(candidates, ModelSpec{Provider: provider, Model: modelName})
			}
		}
	}

	switch len(candidates) {
	case 0:
		return ModelSpec{}, fmt.Errorf("model %q not found", modelName)
	case 1:
		return candidates[0], nil
	default:
		var names []string
		for _, c := range candidates {
			names = append(names, c.String())
		}
		return ModelSpec{}, fmt.Errorf("model %q is ambiguous, matches: %s", modelName, strings.Join(names, ", "))
	}
}

// ResolveModelPatterns resolves an ordered list of model patterns to a single
// fully-qualified model spec. Patterns may be exact model specs
// ("provider/model" or a legacy three-segment form), exact bare model IDs,
// glob-style patterns, or case-insensitive substring matches.
//
// Resolution prefers exact matches over glob matches and glob matches over
// substring matches. Bare-model ambiguity remains an error. A ":level"
// thinking suffix is returned with the model rather than matched.
func ResolveModelPatterns(patterns []string, settings *Settings) (ScopedModel, error) {
	if len(patterns) == 0 {
		return ScopedModel{}, fmt.Errorf("no model patterns provided")
	}

	candidates := buildModelPatternCandidates(settings)
	if len(candidates) == 0 {
		return ScopedModel{}, fmt.Errorf("no configured or environment-backed model candidates available")
	}

	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		var level string
		if i := strings.LastIndex(pattern, ":"); i >= 0 && IsValidThinkingLevel(strings.ToLower(pattern[i+1:])) {
			pattern, level = pattern[:i], strings.ToLower(pattern[i+1:])
		}
		if pattern == "" {
			continue
		}

		// Fully-qualified non-pattern input wins immediately. This preserves
		// explicit user intent even if the configured candidate set is narrower.
		if spec, err := ParseModelSpec(pattern); err == nil && spec.IsFullyQualified() && !hasGlobMeta(pattern) {
			return ScopedModel{Model: spec, ThinkingLevel: level}, nil
		}

		match, err := matchModelPattern(pattern, candidates)
		if err == nil {
			return ScopedModel{Model: match.Spec, ThinkingLevel: level}, nil
		}
		if !errors.Is(err, errNoModelPatternMatch) {
			return ScopedModel{}, err
		}
	}

	return ScopedModel{}, fmt.Errorf("no models matched patterns: %s", strings.Join(patterns, ", "))
}

type modelPatternCandidate struct {
	Spec           ModelSpec
	FullyQualified string
	ProviderModel  string
}

func buildModelPatternCandidates(settings *Settings) []modelPatternCandidate {
	seen := make(map[string]struct{})
	var candidates []modelPatternCandidate

	add := func(spec ModelSpec) {
		if !spec.IsFullyQualified() {
			return
		}
		key := spec.String()
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, modelPatternCandidate{
			Spec:           spec,
			FullyQualified: strings.ToLower(spec.String()),
			ProviderModel:  strings.ToLower(spec.Provider + "/" + spec.Model),
		})
	}

	if settings != nil && settings.DefaultProvider != "" && settings.DefaultModel != "" {
		add(ModelSpec{Provider: settings.DefaultProvider, Model: settings.DefaultModel})
	}

	for _, provider := range ListProviders() {
		if !HasEnvCredentials(provider) {
			continue
		}
		for _, model := range ListAvailableModels(provider) {
			if model == nil || model.ID == "" {
				continue
			}
			add(ModelSpec{Provider: provider, Model: model.ID})
		}
	}

	slices.SortFunc(candidates, func(a, b modelPatternCandidate) int {
		return cmp.Compare(a.FullyQualified, b.FullyQualified)
	})

	return candidates
}

func matchModelPattern(pattern string, candidates []modelPatternCandidate) (modelPatternCandidate, error) {
	patternLower := strings.ToLower(strings.TrimSpace(pattern))
	if patternLower == "" {
		return modelPatternCandidate{}, fmt.Errorf("empty pattern")
	}

	exact := filterModelPatternCandidates(candidates, func(c modelPatternCandidate) bool {
		return c.FullyQualified == patternLower || c.ProviderModel == patternLower || strings.ToLower(c.Spec.Model) == patternLower
	})
	if len(exact) == 1 {
		return exact[0], nil
	}
	if len(exact) > 1 {
		return modelPatternCandidate{}, ambiguousModelPatternError(pattern, exact)
	}

	if hasGlobMeta(patternLower) {
		glob := filterModelPatternCandidates(candidates, func(c modelPatternCandidate) bool {
			return globMatch(patternLower, c.FullyQualified) ||
				globMatch(patternLower, c.ProviderModel) ||
				globMatch(patternLower, strings.ToLower(c.Spec.Model))
		})
		if len(glob) == 1 {
			return glob[0], nil
		}
		if len(glob) > 1 {
			return modelPatternCandidate{}, ambiguousModelPatternError(pattern, glob)
		}
	}

	fuzzy := filterModelPatternCandidates(candidates, func(c modelPatternCandidate) bool {
		return strings.Contains(c.FullyQualified, patternLower) ||
			strings.Contains(c.ProviderModel, patternLower) ||
			strings.Contains(strings.ToLower(c.Spec.Model), patternLower)
	})
	if len(fuzzy) == 1 {
		return fuzzy[0], nil
	}
	if len(fuzzy) > 1 {
		return modelPatternCandidate{}, ambiguousModelPatternError(pattern, fuzzy)
	}

	return modelPatternCandidate{}, fmt.Errorf("%w %q", errNoModelPatternMatch, pattern)
}

func filterModelPatternCandidates(candidates []modelPatternCandidate, keep func(modelPatternCandidate) bool) []modelPatternCandidate {
	out := make([]modelPatternCandidate, 0, len(candidates))
	for _, c := range candidates {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

func ambiguousModelPatternError(pattern string, matches []modelPatternCandidate) error {
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match.Spec.String())
	}
	slices.Sort(names)
	return fmt.Errorf("model pattern %q is ambiguous, matches: %s", pattern, strings.Join(names, ", "))
}

func hasGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

// ExpandModelScope resolves enabledModels or --models patterns against
// "provider/id" keys, in pattern order and without duplicates. A pattern may
// carry a ":level" thinking suffix, which the first match of each model keeps.
// Globs match the full key or the bare id; other patterns match a key or id
// exactly, then a unique substring.
func ExpandModelScope(patterns, keys []string) []ScopedModel {
	var scope []ScopedModel
	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		var level string
		if i := strings.LastIndex(pattern, ":"); i >= 0 && IsValidThinkingLevel(pattern[i+1:]) {
			pattern, level = pattern[:i], pattern[i+1:]
		}
		if pattern == "" {
			continue
		}
		add := func(key string) {
			provider, id, _ := strings.Cut(key, "/")
			spec := ModelSpec{Provider: provider, Model: id}
			if !slices.ContainsFunc(scope, func(m ScopedModel) bool { return m.Model == spec }) {
				scope = append(scope, ScopedModel{Model: spec, ThinkingLevel: level})
			}
		}
		matches := func(match func(key, id string) bool) []string {
			var out []string
			for _, key := range keys {
				lower := strings.ToLower(key)
				_, id, _ := strings.Cut(lower, "/")
				if match(lower, id) {
					out = append(out, key)
				}
			}
			return out
		}
		if exact := matches(func(key, id string) bool { return key == pattern || id == pattern }); len(exact) > 0 {
			add(exact[0])
			continue
		}
		if hasGlobMeta(pattern) {
			for _, key := range matches(func(key, id string) bool { return globMatch(pattern, key) || globMatch(pattern, id) }) {
				add(key)
			}
			continue
		}
		if partial := matches(func(key, _ string) bool { return strings.Contains(key, pattern) }); len(partial) == 1 {
			add(partial[0])
		}
	}
	return scope
}

func globMatch(pattern, value string) bool {
	ok, err := filepath.Match(pattern, value)
	return err == nil && ok
}

// ResolveCliModelResult contains the result of CLI model resolution.
type ResolveCliModelResult struct {
	// Spec is the resolved model spec.
	Spec ModelSpec
	// ThinkingLevel is the thinking level if explicitly specified via suffix, empty otherwise.
	ThinkingLevel string
	// Warning contains an optional warning message.
	Warning string
	// Error contains an error message if resolution failed.
	Error string
}

// ResolveCliModel resolves a single model from CLI flags.
// Supports:
//   - --model <pattern> (uses all available models as candidates)
//   - --provider <provider> --model <pattern> (filters to specific provider)
//   - Fuzzy matching with alias preference over dated versions
//
// The strictMode parameter controls behavior when an invalid thinking level suffix is found:
// - strictMode (true): the suffix is treated as part of the model ID and resolution fails
// - strictMode (false): the suffix is stripped and a warning is returned
//
// Returns a result with Error set if no model could be resolved.
func ResolveCliModel(cliProvider, cliModel string, strictMode bool) ResolveCliModelResult {
	if cliModel == "" {
		return ResolveCliModelResult{}
	}

	// Build candidate list from all available models
	candidates := buildAllAvailableModelSpecs()
	if len(candidates) == 0 {
		return ResolveCliModelResult{
			Error: "no models available. Check your installation or add models to settings.",
		}
	}

	// Build provider map (case-insensitive lookup)
	providerMap := make(map[string]string)
	for _, c := range candidates {
		lower := strings.ToLower(c.Provider)
		if _, exists := providerMap[lower]; !exists {
			providerMap[lower] = c.Provider
		}
	}

	// Normalize provider
	var provider string
	if cliProvider != "" {
		normalized, ok := providerMap[strings.ToLower(cliProvider)]
		if !ok {
			return ResolveCliModelResult{
				Error: fmt.Sprintf("unknown provider %q. Use tau models list to see available providers.", cliProvider),
			}
		}
		provider = normalized
	}

	// Try to interpret "provider/model" format from cliModel
	var pattern string
	inferredProvider := false

	if provider == "" {
		slashIdx := strings.Index(cliModel, "/")
		if slashIdx != -1 {
			maybeProvider := cliModel[:slashIdx]
			if canonical, ok := providerMap[strings.ToLower(maybeProvider)]; ok {
				provider = canonical
				pattern = cliModel[slashIdx+1:]
				inferredProvider = true
			}
		}
	}

	// Default pattern to cliModel if not set
	if pattern == "" {
		pattern = cliModel
	}

	// Filter candidates by provider if one is set
	var filteredCandidates []ModelSpec
	if provider != "" {
		for _, c := range candidates {
			if c.Provider == provider {
				filteredCandidates = append(filteredCandidates, c)
			}
		}
	} else {
		filteredCandidates = candidates
	}

	// Try exact match without provider inference
	// This handles models whose IDs naturally contain slashes (e.g., OpenRouter-style IDs)
	if !inferredProvider && provider == "" {
		exact := FindExactModelSpecMatch(cliModel, candidates)
		if exact.Model != "" {
			return ResolveCliModelResult{
				Spec: exact,
			}
		}
	}

	// Parse pattern with thinking level extraction
	result := ParseModelPattern(pattern, filteredCandidates, strictMode)
	if result.Spec.Model != "" {
		return ResolveCliModelResult{
			Spec:          result.Spec,
			ThinkingLevel: result.ThinkingLevel,
			Warning:       result.Warning,
		}
	}

	// If we inferred a provider from the slash but found no match,
	// fall back to matching the full input as a raw model id across all models.
	// This handles OpenRouter-style IDs like "openai/gpt-4o:extended" where "openai"
	// looks like a provider but the full string is actually a model id.
	if inferredProvider {
		exact := FindExactModelSpecMatch(cliModel, candidates)
		if exact.Model != "" {
			return ResolveCliModelResult{
				Spec: exact,
			}
		}
		// Also try parseModelPattern on the full input against all models
		result = ParseModelPattern(cliModel, candidates, strictMode)
		if result.Spec.Model != "" {
			return ResolveCliModelResult{
				Spec:          result.Spec,
				ThinkingLevel: result.ThinkingLevel,
				Warning:       result.Warning,
			}
		}
	}

	// Try to build a fallback model for the provider
	if provider != "" {
		fallback := BuildFallbackModelSpec(provider, pattern, candidates)
		if fallback.Model != "" {
			warning := result.Warning
			if warning == "" {
				warning = fmt.Sprintf("model %q not found for provider %q. Using custom model id.", pattern, provider)
			} else {
				warning = fmt.Sprintf("%s Model %q not found for provider %q. Using custom model id.", warning, pattern, provider)
			}
			return ResolveCliModelResult{
				Spec:    fallback,
				Warning: warning,
			}
		}
	}

	// No match found
	display := cliModel
	if provider != "" {
		display = provider + "/" + pattern
	}
	return ResolveCliModelResult{
		Error: fmt.Sprintf("model %q not found. Use tau models list to see available models.", display),
	}
}

// buildAllAvailableModelSpecs builds a list of all available model specs
// from settings, configured models, and environment-backed models.
func buildAllAvailableModelSpecs() []ModelSpec {
	seen := make(map[string]struct{})
	var candidates []ModelSpec

	add := func(spec ModelSpec) {
		if !spec.IsFullyQualified() {
			return
		}
		key := spec.String()
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, spec)
	}

	// Add from configured settings (would need Settings passed in, so skip for now)
	// Add from environment-backed models
	for _, provider := range ListProviders() {
		if !HasEnvCredentials(provider) {
			continue
		}
		for _, model := range ListAvailableModels(provider) {
			if model == nil || model.ID == "" {
				continue
			}
			add(ModelSpec{Provider: provider, Model: model.ID})
		}
	}

	return candidates
}

// BuildFallbackModelSpec creates a virtual model spec for an unknown model ID.
// It uses the provider's default model as a base template if available,
// otherwise uses the first model from the provider.
func BuildFallbackModelSpec(provider, modelID string, candidates []ModelSpec) ModelSpec {
	var providerModels []ModelSpec
	for _, c := range candidates {
		if c.Provider == provider {
			providerModels = append(providerModels, c)
		}
	}

	if len(providerModels) == 0 {
		return ModelSpec{}
	}

	// Try to find the default model for this provider
	defaultID := DefaultModelPerProvider[provider]
	if defaultID != "" {
		for _, m := range providerModels {
			if m.Model == defaultID {
				return ModelSpec{
					Provider: m.Provider,
					Model:    modelID,
				}
			}
		}
	}

	// Use the first model as a template
	base := providerModels[0]
	return ModelSpec{
		Provider: base.Provider,
		Model:    modelID,
	}
}

// FindExactModelSpecMatch finds an exact model match by provider/model ID combination.
// Supports bare model ID or canonical provider/modelId reference.
// When matching by bare id, returns the match only if it's unique.
func FindExactModelSpecMatch(modelReference string, candidates []ModelSpec) ModelSpec {
	trimmed := strings.TrimSpace(modelReference)
	if trimmed == "" {
		return ModelSpec{}
	}

	lowerRef := strings.ToLower(trimmed)

	// Try canonical provider/modelId format
	for _, c := range candidates {
		candidateLower := strings.ToLower(c.Provider + "/" + c.Model)
		if candidateLower == lowerRef {
			return c
		}
	}

	// Try provider/model split
	if slashIdx := strings.Index(trimmed, "/"); slashIdx != -1 {
		provider := strings.ToLower(trimmed[:slashIdx])
		modelID := trimmed[slashIdx+1:]
		modelID = strings.TrimSpace(modelID)

		var matches []ModelSpec
		for _, c := range candidates {
			if strings.ToLower(c.Provider) == provider && strings.EqualFold(c.Model, modelID) {
				matches = append(matches, c)
			}
		}
		if len(matches) == 1 {
			return matches[0]
		}
	}

	// Try bare model ID (only if unique)
	var idMatches []ModelSpec
	for _, c := range candidates {
		if strings.ToLower(c.Model) == lowerRef {
			idMatches = append(idMatches, c)
		}
	}
	if len(idMatches) == 1 {
		return idMatches[0]
	}

	return ModelSpec{}
}

// RestoreModelSpecResult contains the result of attempting to restore a model from session.
type RestoreModelSpecResult struct {
	// Spec is the restored or fallback model spec.
	Spec ModelSpec
	// FallbackMessage is a descriptive message if a fallback was used.
	FallbackMessage string
}

// RestoreModelSpecFromSession attempts to restore a model spec from saved provider/model ID.
// If the model no longer exists or isn't available, it falls back to the current model,
// and if that's not available either, to the first available model from known providers.
func RestoreModelSpecFromSession(
	savedProvider, savedModelID string,
	current ModelSpec,
	candidates []ModelSpec,
) RestoreModelSpecResult {
	// Try to find the saved model
	for _, c := range candidates {
		if c.Provider == savedProvider && c.Model == savedModelID {
			return RestoreModelSpecResult{
				Spec:            c,
				FallbackMessage: "",
			}
		}
	}

	// Model not found - determine reason
	reason := "model no longer exists"
	for _, c := range candidates {
		if c.Provider == savedProvider {
			reason = "model not in available models"
			break
		}
	}

	// Try to use the current model as fallback
	if current.Provider != "" && current.Model != "" {
		return RestoreModelSpecResult{
			Spec:            current,
			FallbackMessage: fmt.Sprintf("Could not restore model %s/%s (%s). Using %s/%s.", savedProvider, savedModelID, reason, current.Provider, current.Model),
		}
	}

	// Try first available model from known providers
	for provider, defaultID := range DefaultModelPerProvider {
		for _, c := range candidates {
			if c.Provider == provider && c.Model == defaultID {
				return RestoreModelSpecResult{
					Spec:            c,
					FallbackMessage: fmt.Sprintf("Could not restore model %s/%s (%s). Using %s/%s.", savedProvider, savedModelID, reason, c.Provider, c.Model),
				}
			}
		}
	}

	// Use first available model as last resort
	if len(candidates) > 0 {
		c := candidates[0]
		return RestoreModelSpecResult{
			Spec:            c,
			FallbackMessage: fmt.Sprintf("Could not restore model %s/%s (%s). Using %s/%s.", savedProvider, savedModelID, reason, c.Provider, c.Model),
		}
	}

	// No models available
	return RestoreModelSpecResult{
		Spec:            ModelSpec{},
		FallbackMessage: fmt.Sprintf("Could not restore model %s/%s (%s). No available models.", savedProvider, savedModelID, reason),
	}
}
