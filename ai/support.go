package ai

import (
	"slices"
	"strings"
)

// SupportTier describes how strongly tau defends a provider path or model.
type SupportTier string

const (
	SupportTierAvailable SupportTier = "available"
	SupportTier2         SupportTier = "tier2"
	SupportTier1         SupportTier = "tier1"
)

// ProviderSupportStatus documents the support taxonomy for one provider family.
//
// The fields intentionally separate several concepts that were previously
// conflated in the public registry surface:
//   - HasProviderConstant: the provider family has a stable public constant.
//   - HasImplementation: tau has an implementation path for this provider
//     family, including shared implementations such as the OpenAI-compatible
//     path.
//   - IsRegistered: the API route used by this provider family is currently
//     registered in the global provider registry.
//   - HasModelMetadata: model metadata exists for the provider family through
//     the full generated registry, even if the provider remains outside the
//     defended public support surface.
//   - ReachableViaGetClient: callers can construct a client through GetClient
//     for the provider family's known API path.
//   - Tier: the provider-path tier itself. This is intentionally separate from
//     model-cohort promotions.
//   - HasSupportedModels: at least one model under the provider is in the
//     defended public support surface.
//   - HighestModelTier: the strongest current model tier under this provider.
//   - OfficiallySupported: the provider path itself is part of the defended
//     public support surface today.
//   - Tested: the provider family has targeted tests in this repository.
type ProviderSupportStatus struct {
	Provider              string
	HasProviderConstant   bool
	HasImplementation     bool
	IsRegistered          bool
	HasModelMetadata      bool
	ReachableViaGetClient bool
	Tier                  SupportTier
	HasSupportedModels    bool
	HighestModelTier      SupportTier
	OfficiallySupported   bool
	Tested                bool
}

// ModelSupportStatus documents the support taxonomy for one specific model.
//
// The fields intentionally mirror the availability/support split exposed at
// the provider level:
//   - Available: model metadata exists in the full generated registry.
//   - PubliclyListed: the model is exposed through the curated public lookup
//     surface via GetModel/ListModels.
//   - ReachableViaGetClient: tau can construct a client for the model's API
//     route based on current provider support metadata.
//   - Tier: the current defended tier for this model.
//   - OfficiallySupported: the model itself is part of the defended public
//     support surface.
type ModelSupportStatus struct {
	Provider              string
	ID                    string
	Available             bool
	PubliclyListed        bool
	ReachableViaGetClient bool
	Tier                  SupportTier
	OfficiallySupported   bool
}

type providerSupportDefinition struct {
	apis               []string
	hasImplementation  bool
	tested             bool
	tier               SupportTier
	reachableWithoutMD bool
}

var providerSupportDefinitions = map[string]providerSupportDefinition{
	ProviderAmazonBedrock: {
		apis:              []string{APIBedrockConverseStream},
		hasImplementation: true,
		tested:            true,
		tier:              SupportTier1,
	},
	ProviderAnthropic: {
		apis:              []string{APIAnthropicMessages},
		hasImplementation: true,
		tested:            true,
		tier:              SupportTier1,
	},
	ProviderClaudeAgent: {
		apis:              []string{APIClaudeAgent},
		hasImplementation: true,
		tested:            true,
		tier:              SupportTier2,
	},
	ProviderGoogleAntigravity: {
		apis:              []string{APIGoogleGeminiCLI},
		hasImplementation: true,
		tested:            true,
		tier:              SupportTier1,
	},
	ProviderAzureOpenAI: {
		apis:              []string{APIAzureOpenAIResponses},
		hasImplementation: true,
		tested:            true,
		tier:              SupportTier1,
	},
	ProviderRadius: {
		apis:              []string{APIPiMessages},
		hasImplementation: true,
	},
	ProviderCerebras: {
		apis:              []string{APIOpenAICompletions},
		hasImplementation: true,
	},
	ProviderGitHubCopilot: {
		apis:              []string{APIAnthropicMessages, APIOpenAIResponses},
		hasImplementation: true,
		tested:            true,
		tier:              SupportTier1,
	},
	ProviderGoogle: {
		apis:              []string{APIGoogleGenerativeAI},
		hasImplementation: true,
		tested:            true,
		tier:              SupportTier1,
	},
	ProviderGoogleGeminiCLI: {
		apis:               []string{APIGoogleGeminiCLI},
		hasImplementation:  true,
		tested:             true,
		reachableWithoutMD: true,
	},
	ProviderGoogleVertex: {
		apis:              []string{APIGoogleVertex},
		hasImplementation: true,
		tested:            true,
		tier:              SupportTier1,
	},
	ProviderGroq: {
		apis:              []string{APIOpenAICompletions},
		hasImplementation: true,
	},
	ProviderHuggingFace: {
		apis:              []string{APIOpenAICompletions},
		hasImplementation: true,
	},
	ProviderKimiCoding: {
		apis:              []string{APIAnthropicMessages},
		hasImplementation: true,
	},
	ProviderLMStudio: {
		apis:               []string{APIOpenAICompletions},
		hasImplementation:  true,
		reachableWithoutMD: true,
	},
	ProviderMiniMax: {
		apis:              []string{APIAnthropicMessages},
		hasImplementation: true,
		tested:            true,
	},
	ProviderMiniMaxCN: {
		apis:              []string{APIAnthropicMessages},
		hasImplementation: true,
	},
	ProviderMistral: {
		apis:              []string{APIMistralConversations},
		hasImplementation: true,
		tested:            true,
		tier:              SupportTier1,
	},
	ProviderOllama: {
		apis:               []string{APIOpenAICompletions},
		hasImplementation:  true,
		reachableWithoutMD: true,
	},
	ProviderOpenAI: {
		apis:              []string{APIOpenAICompletions, APIOpenAIResponses},
		hasImplementation: true,
		tested:            true,
		tier:              SupportTier1,
	},
	ProviderOpenAICodex: {
		apis:              []string{APIOpenAICodexResponses},
		hasImplementation: true,
		tested:            true,
		tier:              SupportTier1,
	},
	ProviderOpenRouter: {
		apis:              []string{APIOpenAICompletions},
		hasImplementation: true,
	},
	ProviderOpenCode: {
		apis:              []string{APIOpenAICompletions, APIOpenAIResponses, APIAnthropicMessages, APIGoogleGenerativeAI},
		hasImplementation: true,
	},
	ProviderOpenCodeGo: {
		apis:              []string{APIOpenAICompletions, APIOpenAIResponses, APIAnthropicMessages, APIGoogleGenerativeAI},
		hasImplementation: true,
	},
	ProviderVercelGateway: {
		apis:              []string{APIOpenAICompletions},
		hasImplementation: true,
	},
	ProviderVLLM: {
		apis:               []string{APIOpenAICompletions},
		hasImplementation:  true,
		reachableWithoutMD: true,
	},
	ProviderxAI: {
		apis:              []string{APIOpenAICompletions},
		hasImplementation: true,
	},
	ProviderZAI: {
		apis:              []string{APIOpenAICompletions},
		hasImplementation: true,
		tested:            true,
	},
}

// modelSupportCohort promotes a family of models, matched by ID prefix, into
// a defended support tier. Cohorts match ID prefixes rather than exact model
// IDs so upstream model version churn (e.g. glm-5.1 retiring in favor of
// glm-5.2) does not silently collapse a provider's supported surface.
type modelSupportCohort struct {
	provider string
	idPrefix string
	tier     SupportTier
}

var modelSupportCohorts = []modelSupportCohort{
	{provider: ProviderMiniMax, idPrefix: "MiniMax-M", tier: SupportTier2},
	{provider: ProviderZAI, idPrefix: "glm-5", tier: SupportTier2},
}

func modelTier(provider, id string) SupportTier {
	provider = canonicalProvider(provider)

	for _, cohort := range modelSupportCohorts {
		if cohort.provider == provider && strings.HasPrefix(id, cohort.idPrefix) {
			return cohort.tier
		}
	}

	def, ok := providerSupportDefinitions[provider]
	if !ok {
		return SupportTierAvailable
	}

	return providerTier(def.tier)
}

// ProviderSupport returns the support taxonomy for one provider family.
func ProviderSupport(provider string) (ProviderSupportStatus, bool) {
	provider = canonicalProvider(provider)
	def, ok := providerSupportDefinitions[provider]
	if !ok {
		return ProviderSupportStatus{}, false
	}

	status := ProviderSupportStatus{
		Provider:            provider,
		HasProviderConstant: true,
		HasImplementation:   def.hasImplementation,
		Tier:                providerTier(def.tier),
		Tested:              def.tested,
		OfficiallySupported: providerTier(def.tier) != SupportTierAvailable,
	}

	for _, api := range def.apis {
		if _, ok := Get(api); ok {
			status.IsRegistered = true
			break
		}
	}

	if len(ListAvailableModels(provider)) > 0 {
		status.HasModelMetadata = true
	}

	for _, model := range ListAvailableModels(provider) {
		if model == nil {
			continue
		}
		tier := modelTier(provider, model.ID)
		if tier != SupportTierAvailable {
			status.HasSupportedModels = true
		}
		if tierRank(tier) > tierRank(status.HighestModelTier) {
			status.HighestModelTier = tier
		}
	}

	status.ReachableViaGetClient = status.HasImplementation &&
		status.IsRegistered &&
		(status.HasModelMetadata || def.reachableWithoutMD)

	return status, true
}

// ModelSupport returns the support taxonomy for one model in the full
// generated registry.
func ModelSupport(provider, id string) (ModelSupportStatus, bool) {
	provider = canonicalProvider(provider)

	status := ModelSupportStatus{
		Provider: provider,
		ID:       id,
	}

	model, err := GetAvailableModel(provider, id)
	if err != nil || model == nil {
		return status, false
	}

	status.Available = true
	status.Tier = modelTier(provider, id)
	status.PubliclyListed = status.Tier != SupportTierAvailable

	providerStatus, ok := ProviderSupport(provider)
	if !ok {
		return status, true
	}

	status.ReachableViaGetClient = providerStatus.HasImplementation &&
		providerStatus.IsRegistered &&
		status.Available
	status.OfficiallySupported = status.Tier != SupportTierAvailable

	return status, true
}

// ListModelSupport returns model support statuses for all generated models
// under one provider.
func ListModelSupport(provider string) []ModelSupportStatus {
	provider = canonicalProvider(provider)
	models := ListAvailableModels(provider)
	if len(models) == 0 {
		return nil
	}

	result := make([]ModelSupportStatus, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		status, ok := ModelSupport(provider, model.ID)
		if !ok {
			continue
		}
		result = append(result, status)
	}

	return result
}

func tierRank(tier SupportTier) int {
	switch tier {
	case SupportTier1:
		return 3
	case SupportTier2:
		return 2
	case SupportTierAvailable:
		return 1
	default:
		return 0
	}
}

func providerTier(tier SupportTier) SupportTier {
	if tier == "" {
		return SupportTierAvailable
	}

	return tier
}

// ListProviderSupport returns the current support taxonomy for all known
// provider families.
func ListProviderSupport() []ProviderSupportStatus {
	providers := make([]string, 0, len(providerSupportDefinitions))
	for provider := range providerSupportDefinitions {
		providers = append(providers, provider)
	}
	slices.Sort(providers)

	result := make([]ProviderSupportStatus, 0, len(providers))
	for _, provider := range providers {
		status, ok := ProviderSupport(provider)
		if !ok {
			continue
		}
		result = append(result, status)
	}

	return result
}
