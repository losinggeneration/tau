package ai // import "github.com/losinggeneration/tau/ai"

import (
	"fmt"
	"sync"
)

// DefaultThinkingLevel is the default thinking level when none is specified.
const DefaultThinkingLevel = "medium"

// SettingsVersion tracks the settings file version for migrations.
const SettingsVersion = 1

// InitialModelResult contains the result of finding an initial model.
type InitialModelResult struct {
	Model           ModelSpec
	ThinkingLevel   string
	FallbackMessage string
}

// ResolvedModel describes a resolved model, including where the resolution came from.
type ResolvedModel struct {
	// ModelID is the resolved model identifier (e.g., "claude-sonnet-4-20250514").
	ModelID string `json:"model_id"`
	// ProviderKey is the provider name (e.g., "anthropic").
	ProviderKey string `json:"provider_key"`
	// Source describes where the resolution came from (e.g., "explicit", "settings", "default").
	Source string `json:"source"`
}

// ModelResolver resolves model identifiers through a chain:
//
//	explicit override → settings.json → provider default
type ModelResolver struct {
	mu       sync.RWMutex
	settings *Settings
}

// NewModelResolver creates a ModelResolver with the given settings.
func NewModelResolver(settings *Settings) *ModelResolver {
	if settings == nil {
		settings = &Settings{}
	}
	return &ModelResolver{settings: settings}
}

// ResolveDefaultModel returns the user-configured default from settings, or
// an empty ModelSpec if none is configured. This replaces the per-account
// Resolve method that depended on the deprecated providers map.
func (r *ModelResolver) ResolveDefaultModel() ModelSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.settings == nil {
		return ModelSpec{}
	}
	if r.settings.DefaultProvider == "" || r.settings.DefaultModel == "" {
		return ModelSpec{}
	}
	return ModelSpec{
		Provider: r.settings.DefaultProvider,
		Model:    r.settings.DefaultModel,
	}
}

// Settings returns a copy of the current settings.
func (r *ModelResolver) Settings() Settings {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return *r.settings
}

// ScopedModel represents a model resolved from a scoped pattern with optional thinking level.
type ScopedModel struct {
	Model         ModelSpec
	ThinkingLevel string // Thinking level if explicitly specified in pattern, empty otherwise
}

// RestoreModelResult contains the result of attempting to restore a model.
type RestoreModelResult struct {
	Model           ModelSpec
	FallbackMessage string
}

// FindInitialModel finds the initial model based on priority:
// 1. CLI args (provider + model)
// 2. First model from scoped models (if not continuing/resuming)
// 3. Restored from session (if continuing/resuming)
// 4. Saved default from settings
// 5. First available model with valid API key
func FindInitialModel(options struct {
	CLIProvider    string
	CLIModel       string
	ScopedModels   []ScopedModel
	IsContinuing   bool
	DefaultSpec    ModelSpec
	DefaultLevel   string
	AvailableSpecs []ModelSpec
},
) InitialModelResult {
	// 1. CLI args take priority
	if options.CLIProvider != "" && options.CLIModel != "" {
		result := ResolveCliModel(options.CLIProvider, options.CLIModel, true)
		if result.Error != "" {
			return InitialModelResult{
				FallbackMessage: result.Error,
			}
		}
		if result.Spec.Model != "" {
			return InitialModelResult{
				Model:           result.Spec,
				ThinkingLevel:   result.ThinkingLevel,
				FallbackMessage: "",
			}
		}
	}

	// 2. Use first model from scoped models (skip if continuing/resuming)
	if len(options.ScopedModels) > 0 && !options.IsContinuing {
		sm := options.ScopedModels[0]
		level := sm.ThinkingLevel
		if level == "" {
			level = options.DefaultLevel
		}
		if level == "" {
			level = DefaultThinkingLevel
		}
		return InitialModelResult{
			Model:           sm.Model,
			ThinkingLevel:   level,
			FallbackMessage: "",
		}
	}

	// 3. Try saved default from settings
	if options.DefaultSpec.Provider != "" && options.DefaultSpec.Model != "" {
		level := options.DefaultLevel
		if level == "" {
			level = DefaultThinkingLevel
		}
		return InitialModelResult{
			Model:           options.DefaultSpec,
			ThinkingLevel:   level,
			FallbackMessage: "",
		}
	}

	// 4. Try first available model with valid API key
	if len(options.AvailableSpecs) > 0 {
		// Try to find a default model from known providers
		for provider, defaultID := range DefaultModelPerProvider {
			for _, spec := range options.AvailableSpecs {
				if spec.Provider == provider && spec.Model == defaultID {
					return InitialModelResult{
						Model:           spec,
						ThinkingLevel:   DefaultThinkingLevel,
						FallbackMessage: "",
					}
				}
			}
		}
		// If no default found, use first available
		return InitialModelResult{
			Model:           options.AvailableSpecs[0],
			ThinkingLevel:   DefaultThinkingLevel,
			FallbackMessage: "",
		}
	}

	// 5. No model found
	return InitialModelResult{
		Model:           ModelSpec{},
		ThinkingLevel:   DefaultThinkingLevel,
		FallbackMessage: "",
	}
}

// RestoreModelFromSession attempts to restore a model from saved provider/model ID.
// If the model no longer exists or isn't available, it falls back to the current model,
// and if that's not available either, to the first available model from known providers.
func RestoreModelFromSession(
	savedProvider, savedModelID string,
	current ModelSpec,
	availableSpecs []ModelSpec,
	printMessages bool,
) RestoreModelResult {
	// Try to find the saved model
	for _, spec := range availableSpecs {
		if spec.Provider == savedProvider && spec.Model == savedModelID {
			return RestoreModelResult{
				Model:           spec,
				FallbackMessage: "",
			}
		}
	}

	// Model not found - determine reason
	reason := "model no longer exists"
	for _, spec := range availableSpecs {
		if spec.Provider == savedProvider {
			reason = "model not in available models"
			break
		}
	}

	// Try to use the current model as fallback
	if current.Provider != "" && current.Model != "" {
		msg := fmt.Sprintf("Could not restore model %s/%s (%s). Using %s/%s.", savedProvider, savedModelID, reason, current.Provider, current.Model)
		return RestoreModelResult{
			Model:           current,
			FallbackMessage: msg,
		}
	}

	// Try first available model from known providers
	for provider, defaultID := range DefaultModelPerProvider {
		for _, spec := range availableSpecs {
			if spec.Provider == provider && spec.Model == defaultID {
				msg := fmt.Sprintf("Could not restore model %s/%s (%s). Using %s/%s.", savedProvider, savedModelID, reason, spec.Provider, spec.Model)
				return RestoreModelResult{
					Model:           spec,
					FallbackMessage: msg,
				}
			}
		}
	}

	// Use first available model as last resort
	if len(availableSpecs) > 0 {
		spec := availableSpecs[0]
		msg := fmt.Sprintf("Could not restore model %s/%s (%s). Using %s/%s.", savedProvider, savedModelID, reason, spec.Provider, spec.Model)
		return RestoreModelResult{
			Model:           spec,
			FallbackMessage: msg,
		}
	}

	// No models available
	msg := fmt.Sprintf("Could not restore model %s/%s (%s). No available models.", savedProvider, savedModelID, reason)
	return RestoreModelResult{
		Model:           ModelSpec{},
		FallbackMessage: msg,
	}
}
