package ai // import "github.com/losinggeneration/tau/ai"

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
)

//go:generate go run ../internal/app/genmodels -output models_generated.go

func canonicalProvider(provider string) string {
	switch provider {
	case "azure-openai-responses":
		return ProviderAzureOpenAI
	case "antigravity":
		return ProviderGoogleAntigravity
	default:
		return provider
	}
}

// ResolveModelID parses a "provider/modelID" string and returns the model.
// This function uses the full generated registry, so it works for any model
// that has metadata, not just publicly supported models. This mirrors the
// TypeScript behavior where stream(model, ...) and complete(model, ...) work
// with any model that has metadata.
func ResolveModelID(modelID string) (*Model, error) {
	// Parse "provider/modelID" format
	var provider, id string
	if before, after, found := strings.Cut(modelID, "/"); found {
		provider = before
		id = after
	} else {
		// No provider prefix - try to find by ID alone
		// This handles cases like "claude-3-5-sonnet-20241022" where the caller
		// expects us to find it via provider inference or the full registry
		provider = ""
		id = modelID
	}

	if provider != "" {
		provider = canonicalProvider(provider)
	}

	// First try to get from full registry (GetAvailableModel uses the full registry)
	if provider != "" {
		model, err := GetAvailableModel(provider, id)
		if err == nil {
			return model, nil
		}
	}

	// If that failed and provider was empty, try to find by ID alone
	if provider == "" {
		for _, model := range models {
			if model != nil && model.ID == id && (model.Type == "" || model.Type == "chat") {
				return model, nil
			}
		}
	}

	return nil, fmt.Errorf("model not found: %s", modelID)
}

// GetModel returns a model from the full generated registry.
// This matches the TypeScript registry behavior.
func GetModel(provider, id string) (*Model, error) {
	provider = canonicalProvider(provider)
	return GetAvailableModel(provider, id)
}

// CalculateCost computes usage cost using the model's pricing, mutates
// usage.Cost in place, and returns the calculated Cost. This mirrors Pi's
// calculateCost(model, usage) helper.
func CalculateCost(model *Model, usage *Usage) Cost {
	if usage == nil {
		return Cost{}
	}
	if model == nil {
		*usage = normalizeUsage(*usage)
		return usage.Cost
	}
	usage.CalculateCost(model.Cost)
	return usage.Cost
}

// GetAvailableModel returns a model from the full generated registry, including
// models that are not part of the defended public support surface.
func GetAvailableModel(provider, id string) (*Model, error) {
	return GetOperationModel("chat", provider, id)
}

// GetOperationModel resolves a model without colliding with another operation.
func GetOperationModel(operation, provider, id string) (*Model, error) {
	provider = canonicalProvider(provider)
	key := provider + "/" + id
	if operation != "" && operation != "chat" {
		key = operation + ":" + key
	}
	model, ok := models[key]
	if !ok {
		return nil, fmt.Errorf("%s model not found: %s/%s", operation, provider, id)
	}
	return model, nil
}

// CredentialRefLister is the non-refreshing credential reference surface used
// to decide which providers are authenticated without touching the network or
// refreshing OAuth tokens.
type CredentialRefLister interface {
	List(context.Context) ([]CredentialRef, error)
}

// ListProviders returns provider IDs present in the full generated registry.
func ListProviders() []string {
	return ListAvailableProviders()
}

// ListAvailableProviders returns provider IDs present in the full generated
// model registry, regardless of official support status.
func ListAvailableProviders() []string {
	seen := make(map[string]struct{})

	for _, model := range models {
		if model == nil || model.Provider == "" {
			continue
		}
		seen[model.Provider] = struct{}{}
	}

	result := make([]string, 0, len(seen))
	for p := range seen {
		result = append(result, p)
	}
	slices.Sort(result)

	return result
}

// ListModels returns all registered models for a provider.
func ListModels(provider string) []*Model {
	return ListAvailableModels(provider)
}

// ListAvailableModels returns models from the full generated registry for a
// provider, including models that are not officially supported.
func ListAvailableModels(provider string) []*Model {
	provider = canonicalProvider(provider)
	return listModels(provider, false)
}

// GetSupportedModel returns a model from the defended support surface only.
func GetSupportedModel(provider, id string) (*Model, error) {
	provider = canonicalProvider(provider)
	model, err := GetAvailableModel(provider, id)
	if err != nil || !supportsPublicModelLookup(provider, id) {
		return nil, fmt.Errorf("model not found: %s/%s", provider, id)
	}

	return model, nil
}

// ListSupportedProviders returns provider IDs exposed through the defended
// public support surface.
func ListSupportedProviders() []string {
	seen := make(map[string]struct{})

	for _, model := range models {
		if model == nil || model.Provider == "" {
			continue
		}
		if !supportsPublicModelLookup(model.Provider, model.ID) {
			continue
		}
		seen[model.Provider] = struct{}{}
	}

	result := make([]string, 0, len(seen))
	for p := range seen {
		result = append(result, p)
	}
	slices.Sort(result)

	return result
}

// ListSupportedModels returns models from the defended support surface.
func ListSupportedModels(provider string) []*Model {
	provider = canonicalProvider(provider)
	return listModels(provider, true)
}

func listModels(provider string, supportedOnly bool) []*Model {
	provider = canonicalProvider(provider)
	if provider == "" {
		return nil
	}

	var result []*Model

	for _, model := range models {
		if model == nil || model.Provider != provider || (model.Type != "" && model.Type != "chat") {
			continue
		}
		if supportedOnly && !supportsPublicModelLookup(provider, model.ID) {
			continue
		}
		result = append(result, model)
	}

	slices.SortFunc(result, func(a, b *Model) int {
		return cmp.Compare(a.ID, b.ID)
	})

	return result
}

// Models returns a copy of all registered models.
func Models() map[string]*Model {
	result := make(map[string]*Model, len(models))
	for k, v := range models {
		result[k] = v
	}

	return result
}

// ModelsList returns all registered models as a slice sorted by
// (provider, id). Suitable for surfacing in a UI selector where stable
// ordering matters. Returns an empty slice if no models are registered.
func ModelsList() []*Model {
	all := Models()
	out := make([]*Model, 0, len(all))
	for _, m := range all {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// AuthenticatedProviderSet returns providers with credentials available from
// environment variables or from the credential reference store. It deliberately
// uses only CredentialRefLister.List and HasEnvCredentials, so callers can use it in
// model pickers and list commands without causing network calls or OAuth token
// refreshes. If the store cannot be listed, environment-authenticated providers
// are still returned.
func AuthenticatedProviderSet(ctx context.Context, credentialStore CredentialRefLister) map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, provider := range ListProviders() {
		if ProviderAllowsNoCredential(provider) || HasEnvCredentials(provider) {
			allowed[provider] = struct{}{}
		}
	}

	if credentialStore == nil {
		return allowed
	}
	refs, err := credentialStore.List(ctx)
	if err != nil {
		return allowed
	}
	for _, ref := range refs {
		provider := canonicalProvider(ref.Provider)
		if provider == "" || ref.NeedsReauth || (ref.Type == CredentialTypeNone && !ProviderAllowsNoCredential(provider)) {
			continue
		}
		allowed[provider] = struct{}{}
	}
	return allowed
}

// AuthenticatedModels returns all available models whose provider is currently
// authenticated via the environment or credential store. providerFilter narrows
// the result to one provider while preserving the same authentication rules.
func AuthenticatedModels(ctx context.Context, credentialStore CredentialRefLister, providerFilter string) []*Model {
	allowed := AuthenticatedProviderSet(ctx, credentialStore)
	if len(allowed) == 0 {
		return nil
	}

	providerFilter = canonicalProvider(providerFilter)
	models := ModelsList()
	out := make([]*Model, 0, len(models))
	for _, m := range models {
		if m == nil {
			continue
		}
		if providerFilter != "" && m.Provider != providerFilter {
			continue
		}
		if _, ok := allowed[m.Provider]; !ok {
			continue
		}
		out = append(out, m)
	}
	return out
}

// RegisterModel registers a model. This is called by init() in provider files.
func RegisterModel(model *Model) {
	registered := *model
	registered.Provider = canonicalProvider(registered.Provider)
	key := registered.Provider + "/" + registered.ID
	if registered.Type != "" && registered.Type != "chat" {
		key = registered.Type + ":" + key
	}
	models[key] = &registered
}

// models is the global model registry.
var models = make(map[string]*Model)

// ProviderAllowsNoCredential reports whether a provider can run without a tau-managed credential.
func ProviderAllowsNoCredential(provider string) bool {
	switch canonicalProvider(provider) {
	case ProviderClaudeAgent, ProviderOllama, ProviderLMStudio, ProviderVLLM, ProviderLlamaCpp:
		return true
	default:
		return false
	}
}

func supportsPublicModelLookup(provider, id string) bool {
	def, ok := providerSupportDefinitions[provider]
	if !ok || !def.hasImplementation || !def.tested {
		return false
	}
	if modelTier(provider, id) == SupportTierAvailable {
		return false
	}

	for _, api := range def.apis {
		if _, ok := Get(api); ok {
			return true
		}
	}

	return false
}

func init() {
	registerGeneratedModels()
	registerClaudeAgentModels()
}

// GetSupportedThinkingLevels returns the model's available effort ladder.
func GetSupportedThinkingLevels(model *Model) []string {
	if model == nil || !model.Reasoning {
		return []string{"off"}
	}
	var levels []string
	for _, level := range ThinkingLevels {
		mapped, exists := model.ThinkingLevelMap[level]
		if exists && mapped == nil {
			continue
		}
		if level == "max" && !exists {
			continue
		}
		if level == "xhigh" && !exists && !SupportsXhigh(model) {
			continue
		}
		levels = append(levels, level)
	}
	return levels
}

// ClampThinkingLevel selects an available level, preferring the next higher level.
func ClampThinkingLevel(model *Model, level string) string {
	levels := GetSupportedThinkingLevels(model)
	if slices.Contains(levels, level) {
		return level
	}
	index := slices.Index(ThinkingLevels, level)
	if index >= 0 {
		for _, candidate := range ThinkingLevels[index:] {
			if slices.Contains(levels, candidate) {
				return candidate
			}
		}
		for i := index - 1; i >= 0; i-- {
			if slices.Contains(levels, ThinkingLevels[i]) {
				return ThinkingLevels[i]
			}
		}
	}
	if len(levels) > 0 {
		return levels[0]
	}
	return "off"
}

// SupportsXhigh returns true if the model supports xhigh thinking level.
// Supported today:
//   - GPT-5.2+ model families
//   - Opus 4.6+ models (xhigh maps to adaptive effort "max" on Anthropic-compatible providers)
func SupportsXhigh(model *Model) bool {
	if model == nil {
		return false
	}
	id := model.ID
	if strings.Contains(id, "gpt-5.2") || strings.Contains(id, "gpt-5.3") || strings.Contains(id, "gpt-5.4") ||
		strings.Contains(id, "gpt-5.5") || strings.Contains(id, "gpt-5.6") || strings.Contains(id, "gpt-6") {
		return true
	}
	if strings.Contains(id, "opus-4-6") || strings.Contains(id, "opus-4.6") ||
		strings.Contains(id, "opus-4-7") || strings.Contains(id, "opus-4.7") {
		return true
	}
	return false
}

// ModelsAreEqual returns true if two models are equal by comparing both their ID and provider.
// Returns false if either model is nil.
func ModelsAreEqual(a, b *Model) bool {
	if a == nil || b == nil {
		return false
	}
	return a.ID == b.ID && a.Provider == b.Provider && effectiveModelType(a) == effectiveModelType(b)
}

func effectiveModelType(model *Model) string {
	if model.Type == "" {
		return "chat"
	}
	return model.Type
}
