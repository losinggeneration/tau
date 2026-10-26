package ai // import "github.com/losinggeneration/tau/ai"

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
)

// ModelRegistryFile is the models.json configuration.
type ModelRegistryFile struct {
	Providers map[string]ModelRegistryProvider `json:"providers"`
}

// ModelRegistryProvider is a single entry in models.json's providers map.
type ModelRegistryProvider struct {
	BaseURL        string                           `json:"baseUrl,omitempty"`
	APIKey         string                           `json:"apiKey,omitempty"`
	API            string                           `json:"api,omitempty"`
	Headers        map[string]string                `json:"headers,omitempty"`
	AuthHeader     bool                             `json:"authHeader,omitempty"`
	Compat         json.RawMessage                  `json:"compat,omitempty"`
	Models         []ModelRegistryModel             `json:"models,omitempty"`
	ModelOverrides map[string]ModelRegistryOverride `json:"modelOverrides,omitempty"`
}

// ModelRegistryModel defines a custom model entry.
type ModelRegistryModel struct {
	Type                          string                    `json:"type,omitempty"`
	API                           string                    `json:"api,omitempty"`
	InputLimits                   *ModelInputLimits         `json:"inputLimits,omitempty"`
	SamplingParams                map[string]any            `json:"samplingParams,omitempty"`
	SamplingParamsByThinkingLevel map[string]map[string]any `json:"samplingParamsByThinkingLevel,omitempty"`
	ThinkingLevelMap              map[string]*string        `json:"thinkingLevelMap,omitempty"`
	ID                            string                    `json:"id"`
	Name                          string                    `json:"name,omitempty"`
	Reasoning                     bool                      `json:"reasoning,omitempty"`
	Input                         []string                  `json:"input,omitempty"`
	Cost                          ModelRegistryCost         `json:"cost,omitempty"`
	ContextWindow                 int                       `json:"contextWindow,omitempty"`
	MaxTokens                     int                       `json:"maxTokens,omitempty"`
}

// ModelRegistryCost records token costs.
type ModelRegistryCost struct {
	Tiers      []CostTier `json:"tiers,omitempty"`
	Input      float64    `json:"input,omitempty"`
	Output     float64    `json:"output,omitempty"`
	CacheRead  float64    `json:"cacheRead,omitempty"`
	CacheWrite float64    `json:"cacheWrite,omitempty"`
}

// ModelRegistryOverride holds per-built-in-model overrides keyed by model id.
type ModelRegistryOverride struct {
	ContextWindow int               `json:"contextWindow,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
}

// ModelRegistry is the in-memory representation of models.json. It is
// additive: callers can read built-in providers and override or extend them
// with registry entries.
type ModelRegistry struct {
	values    configValueResolver
	providers map[string]ModelRegistryProvider
	mu        sync.RWMutex
	virtual   map[string]VirtualModelDefinition
}

// NewModelRegistry returns an empty registry.
func NewModelRegistry() *ModelRegistry {
	return &ModelRegistry{providers: map[string]ModelRegistryProvider{}}
}

// LoadModelRegistry reads models.json from path. A missing or empty file
// returns an empty registry (not an error). A malformed file returns an
// error.
func LoadModelRegistry(path string) (*ModelRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewModelRegistry(), nil
		}
		return nil, fmt.Errorf("read models.json: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return NewModelRegistry(), nil
	}
	var file ModelRegistryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse models.json: %w", err)
	}
	reg := NewModelRegistry()
	for k, v := range file.Providers {
		if canonicalProvider(k) != k {
			if _, exists := file.Providers[canonicalProvider(k)]; exists {
				continue
			}
		}
		reg.providers[canonicalProvider(k)] = v
	}
	return reg, nil
}

// SaveModelRegistry writes the registry to path with mode 0644.
func SaveModelRegistry(path string, reg *ModelRegistry) error {
	if reg == nil {
		reg = NewModelRegistry()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create models.json dir: %w", err)
	}
	out := ModelRegistryFile{Providers: reg.providers}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal models.json: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write models.json: %w", err)
	}
	return nil
}

// Provider returns the registry entry for provider, if present.
func (r *ModelRegistry) Provider(provider string) (ModelRegistryProvider, bool) {
	if r == nil {
		return ModelRegistryProvider{}, false
	}
	p, ok := r.providers[canonicalProvider(provider)]
	return p, ok
}

// ResolveAPIKey resolves a configured provider key at request time. An empty
// result means no key is configured; stored credentials take precedence.
func (r *ModelRegistry) ResolveAPIKey(ctx context.Context, provider string) (string, error) {
	return r.ResolveAPIKeyWithEnv(ctx, provider, nil)
}

// ResolveAPIKeyWithEnv resolves configured references against scoped variables.
func (r *ModelRegistry) ResolveAPIKeyWithEnv(ctx context.Context, provider string, env map[string]string) (string, error) {
	p, ok := r.Provider(provider)
	if !ok || p.APIKey == "" {
		return "", nil
	}
	key, err := r.values.resolveWithEnv(ctx, p.APIKey, env)
	if err != nil {
		return "", fmt.Errorf("provider %s API key: %w", provider, err)
	}
	return key, nil
}

// ProviderIDs returns the registry's provider ids in sorted order.
func (r *ModelRegistry) ProviderIDs() []string {
	if r == nil {
		return nil
	}
	ids := make([]string, 0, len(r.providers))
	for k := range r.providers {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	return ids
}

// SetCustomModels replaces the provider's custom models list. Pass nil to
// remove the provider's custom models entirely.
func (r *ModelRegistry) SetCustomModels(provider string, models []string) error {
	if r == nil {
		return fmt.Errorf("nil registry")
	}
	p, ok := r.providers[canonicalProvider(provider)]
	if !ok {
		p = ModelRegistryProvider{}
	}
	p.Models = make([]ModelRegistryModel, 0, len(models))
	for _, m := range models {
		if m == "" {
			continue
		}
		p.Models = append(p.Models, ModelRegistryModel{
			ID:            m,
			Name:          m,
			ContextWindow: 128000,
			MaxTokens:     16384,
			Input:         []string{"text"},
		})
	}
	r.providers[canonicalProvider(provider)] = p
	return nil
}

// SetBaseURL updates a provider's baseUrl, creating the entry if missing.
func (r *ModelRegistry) SetBaseURL(provider, baseURL string) {
	if r == nil {
		return
	}
	p, ok := r.providers[canonicalProvider(provider)]
	if !ok {
		p = ModelRegistryProvider{}
	}
	p.BaseURL = baseURL
	r.providers[canonicalProvider(provider)] = p
}

// ResolveModel resolves local metadata without changing the shared catalog.
func (r *ModelRegistry) ResolveModel(spec string) (*Model, error) {
	return r.ResolveOperationModel("chat", spec)
}

// ResolveOperationModel resolves runtime-local metadata for one operation.
func (r *ModelRegistry) ResolveOperationModel(operation, spec string) (*Model, error) {
	if effectiveModelOperation(operation) == "chat" {
		if model := r.virtualModel(spec); model != nil {
			return model, nil
		}
	}
	return r.resolvePhysicalModel(operation, spec)
}

func (r *ModelRegistry) resolvePhysicalModel(operation, spec string) (*Model, error) {
	provider, id, qualified := strings.Cut(spec, "/")
	parsed := ModelSpec{Provider: canonicalProvider(provider), Model: id}
	if !qualified {
		parsed = ModelSpec{Model: spec}
	}
	p, configured := r.Provider(parsed.Provider)
	applyCompat := func(model *Model) (*Model, error) {
		if configured {
			model.requestConfig = &modelRequestConfig{resolver: &r.values, apiKey: p.APIKey, authHeader: p.AuthHeader}
		}
		if len(p.Compat) == 0 {
			return model, nil
		}
		compat := CompatibilitySettings{}
		if model.Compat != nil {
			compat = *model.Compat
		}
		if err := json.Unmarshal(p.Compat, &compat); err != nil {
			return nil, fmt.Errorf("provider %s compatibility: %w", parsed.Provider, err)
		}
		model.Compat = &compat
		return model, nil
	}

	for _, entry := range p.Models {
		if entry.ID != parsed.Model || effectiveModelOperation(entry.Type) != effectiveModelOperation(operation) {
			continue
		}
		api := entry.API
		if api == "" {
			api = p.API
		}
		if api == "" {
			if builtin, err := ResolveModelID(spec); err == nil {
				api = builtin.API
			}
		}
		if api == "" {
			return nil, fmt.Errorf("custom model %s requires an API", spec)
		}
		inputs := make([]InputType, 0, len(entry.Input))
		for _, input := range entry.Input {
			inputs = append(inputs, InputType(input))
		}
		if len(inputs) == 0 {
			inputs = []InputType{InputText}
		}
		name := entry.Name
		if name == "" {
			name = entry.ID
		}
		headers := maps.Clone(p.Headers)
		contextWindow := entry.ContextWindow
		if override, ok := p.ModelOverrides[entry.ID]; ok {
			if override.ContextWindow > 0 {
				contextWindow = override.ContextWindow
			}
			if headers == nil {
				headers = make(map[string]string)
			}
			copyConfiguredHeaders(headers, override.Headers)
		}
		return applyCompat(&Model{Type: entry.Type, ID: entry.ID, Name: name, Provider: parsed.Provider, API: api, BaseURL: p.BaseURL, Headers: headers, Reasoning: entry.Reasoning, Input: inputs, Output: []OutputType{OutputText}, ContextWindow: contextWindow, MaxTokens: entry.MaxTokens, ThinkingLevelMap: maps.Clone(entry.ThinkingLevelMap), InputLimits: entry.InputLimits, SamplingParams: maps.Clone(entry.SamplingParams), SamplingParamsByThinkingLevel: maps.Clone(entry.SamplingParamsByThinkingLevel), Cost: CostProfile{Input: entry.Cost.Input, Output: entry.Cost.Output, CacheRead: entry.Cost.CacheRead, CacheWrite: entry.Cost.CacheWrite, Tiers: slices.Clone(entry.Cost.Tiers)}})
	}
	var builtin *Model
	var err error
	if effectiveModelOperation(operation) == "chat" {
		builtin, err = ResolveModelID(spec)
	} else {
		builtin, err = GetOperationModel(operation, parsed.Provider, parsed.Model)
	}
	if err != nil {
		return nil, err
	}
	if !qualified {
		parsed.Provider = builtin.Provider
		p, configured = r.Provider(parsed.Provider)
	}
	if !configured {
		return builtin, nil
	}
	model := *builtin
	if p.BaseURL != "" {
		model.BaseURL = p.BaseURL
	}
	if p.API != "" {
		model.API = p.API
	}
	model.Headers = maps.Clone(builtin.Headers)
	if model.Headers == nil {
		model.Headers = map[string]string{}
	}
	copyConfiguredHeaders(model.Headers, p.Headers)
	if override, ok := p.ModelOverrides[model.ID]; ok {
		if override.ContextWindow > 0 {
			model.ContextWindow = override.ContextWindow
		}
		copyConfiguredHeaders(model.Headers, override.Headers)
	}
	return applyCompat(&model)
}

// Models returns the runtime-local catalog with provider and model overrides applied.
func (r *ModelRegistry) Models() ([]*Model, error) {
	var catalog []*Model
	seen := map[string]bool{}
	appendModel := func(operation, spec string) error {
		key := effectiveModelOperation(operation) + ":" + spec
		if seen[key] {
			return nil
		}
		model, err := r.resolvePhysicalModel(operation, spec)
		if err != nil {
			return err
		}
		if effectiveModelOperation(model.Type) == "chat" && r.virtualModel(model.Provider+"/"+model.ID) != nil {
			return nil
		}
		catalog = append(catalog, model)
		seen[key] = true
		return nil
	}
	for _, builtin := range ModelsList() {
		if err := appendModel(builtin.Type, builtin.Provider+"/"+builtin.ID); err != nil {
			return nil, err
		}
	}
	for _, provider := range r.ProviderIDs() {
		p, _ := r.Provider(provider)
		for _, entry := range p.Models {
			if err := appendModel(entry.Type, provider+"/"+entry.ID); err != nil {
				return nil, err
			}
		}
	}
	r.mu.RLock()
	for _, key := range slices.Sorted(maps.Keys(r.virtual)) {
		catalog = append(catalog, newVirtualModel(r.virtual[key]))
	}
	r.mu.RUnlock()
	return catalog, nil
}

func effectiveModelOperation(operation string) string {
	if operation == "" {
		return "chat"
	}
	return operation
}
