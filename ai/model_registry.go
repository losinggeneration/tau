package ai // import "github.com/losinggeneration/tau/ai"

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ModelRegistryFile mirrors Pi's models.json schema
// (with pi → tau). The Tau implementation provides
// load/save/lookup helpers around the schema but does not interpret the
// API key/headers compatibility fields at runtime yet.
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
	ID            string            `json:"id"`
	Name          string            `json:"name,omitempty"`
	Reasoning     bool              `json:"reasoning,omitempty"`
	Input         []string          `json:"input,omitempty"`
	Cost          ModelRegistryCost `json:"cost,omitempty"`
	ContextWindow int               `json:"contextWindow,omitempty"`
	MaxTokens     int               `json:"maxTokens,omitempty"`
}

// ModelRegistryCost records token costs.
type ModelRegistryCost struct {
	Input      float64 `json:"input,omitempty"`
	Output     float64 `json:"output,omitempty"`
	CacheRead  float64 `json:"cacheRead,omitempty"`
	CacheWrite float64 `json:"cacheWrite,omitempty"`
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
	providers map[string]ModelRegistryProvider
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
		reg.providers[k] = v
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
	p, ok := r.providers[provider]
	return p, ok
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
	p, ok := r.providers[provider]
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
	r.providers[provider] = p
	return nil
}

// SetBaseURL updates a provider's baseUrl, creating the entry if missing.
func (r *ModelRegistry) SetBaseURL(provider, baseURL string) {
	if r == nil {
		return
	}
	p, ok := r.providers[provider]
	if !ok {
		p = ModelRegistryProvider{}
	}
	p.BaseURL = baseURL
	r.providers[provider] = p
}
