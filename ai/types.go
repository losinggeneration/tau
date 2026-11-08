// Package ai provides a unified interface for interacting with large language models
// from various providers (OpenAI, Anthropic, Google, etc.).
//
// The package abstracts away provider-specific details like API formats,
// authentication, and streaming protocols to present a unified API surface.
package ai // import "github.com/losinggeneration/tau/ai"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

const (
	messageRoleTool       = "tool"
	messageRoleToolResult = "toolResult"
)

// Provider identifiers
const (
	ProviderOpenAI            = "openai"
	ProviderAnthropic         = "anthropic"
	ProviderClaudeAgent       = "claude-agent"
	ProviderGoogle            = "google"
	ProviderGoogleVertex      = "google-vertex"
	ProviderMistral           = "mistral"
	ProviderGroq              = "groq"
	ProviderCerebras          = "cerebras"
	ProviderxAI               = "xai"
	ProviderOpenRouter        = "openrouter"
	ProviderVercelGateway     = "vercel-ai-gateway"
	ProviderAzureOpenAI       = "azure"
	ProviderAmazonBedrock     = "amazon-bedrock"
	ProviderOllama            = "ollama"
	ProviderLMStudio          = "lm-studio"
	ProviderVLLM              = "vllm"
	ProviderHuggingFace       = "huggingface"
	ProviderMiniMax           = "minimax"
	ProviderMiniMaxCN         = "minimax-cn"
	ProviderOpenCode          = "opencode"
	ProviderOpenCodeGo        = "opencode-go"
	ProviderKimiCoding        = "kimi-coding"
	ProviderOpenAICodex       = "openai-codex"
	ProviderGitHubCopilot     = "github-copilot"
	ProviderGoogleGeminiCLI   = "google-gemini-cli"
	ProviderGoogleAntigravity = "google-antigravity"
	ProviderAntigravity       = ProviderGoogleAntigravity
	ProviderZAI               = "zai"
	// Additional provider identifiers from the environment-variable registry.
	ProviderDeepSeek            = "deepseek"
	ProviderFireworks           = "fireworks"
	ProviderXiaomi              = "xiaomi"
	ProviderRadius              = "radius"
	ProviderAntLing             = "ant-ling"
	ProviderNVIDIA              = "nvidia"
	ProviderTogether            = "together"
	ProviderZAICodingCN         = "zai-coding-cn"
	ProviderMoonshotAI          = "moonshotai"
	ProviderMoonshotAICN        = "moonshotai-cn"
	ProviderCloudflareWorkersAI = "cloudflare-workers-ai"
	ProviderCloudflareAIGateway = "cloudflare-ai-gateway"
	ProviderXiaomiTokenPlanCN   = "xiaomi-token-plan-cn"
	ProviderXiaomiTokenPlanAMS  = "xiaomi-token-plan-ams"
	ProviderXiaomiTokenPlanSGP  = "xiaomi-token-plan-sgp"
	// Legacy tau identifiers (retained for backward compatibility with
	// existing settings/registry entries).
	ProviderMoonshot   = "moonshot"
	ProviderCloudflare = "cloudflare"
)

// API type identifiers
const (
	APIOpenAIDecisions              = "openai-decisions"
	APIPiMessages                   = "pi-messages"
	APIOpenRouterImages             = "openrouter-images"
	APITypesafeSystemOne            = "typesafe-system-one"
	APICloudflareWorkersAISystemOne = "cloudflare-workers-ai-system-one"
	APIAnthropicMessages            = "anthropic-messages"
	APIClaudeAgent                  = "claude-agent"
	APIGoogleGenerativeAI           = "google-generative-ai"
	APIGoogleVertex                 = "google-vertex"
	APIGoogleGeminiCLI              = "google-gemini-cli"
	APIMistralConversations         = "mistral-conversations"
	APIOpenAICompletions            = "openai-completions"
	APIOpenAIResponses              = "openai-responses"
	APIOpenAICompatible             = "openai-compatible"
	APIOpenAICodexResponses         = "openai-codex-responses"
	APIAzureOpenAIResponses         = "azure-openai-responses"
	APIBedrockConverseStream        = "bedrock-converse-stream"
)

// ThinkingFormat represents the format of thinking/reasoning content.
const (
	ThinkingFormatOpenAI       = "openai"             // Native reasoning via completion API
	ThinkingFormatZAI          = "zai"                // ZAI-specific thinking format
	ThinkingFormatQwen         = "qwen"               // Qwen thinking format
	ThinkingFormatQwenChatTmpl = "qwen-chat-template" // Qwen with chat template
	ThinkingFormatOpenRouter   = "openrouter"         // OpenRouter reasoning: { effort }
	ThinkingFormatNative       = "native"             // Provider's native thinking support
	ThinkingFormatDeepSeek     = "deepseek"           // DeepSeek-style thinking: { type: "enabled"|"disabled" }
	ThinkingFormatTogether     = "together"           // Together: reasoning.enabled plus reasoning_effort
	ThinkingFormatBaseten      = "baseten"            // Baseten: configurable chat_template_args plus reasoning_effort
	ThinkingFormatChatTemplate = "chat-template"      // Configurable chat_template_kwargs
	ThinkingFormatString       = "string-thinking"    // Top-level thinking string
	ThinkingFormatAntLing      = "ant-ling"           // reasoning.effort only for mapped levels
)

// InputType represents the modalities a model can accept as input.
type InputType string

const (
	InputText  InputType = "text"
	InputImage InputType = "image"
	InputAudio InputType = "audio"
)

// OutputType represents the modalities a model can produce as output.
type OutputType string

const (
	OutputText  OutputType = "text"
	OutputAudio OutputType = "audio"
)

// Content block type constants
const (
	ContentBlockText       = "text"
	ContentBlockImage      = "image"
	ContentBlockAudio      = "audio"
	ContentBlockToolCall   = "toolCall"
	ContentBlockToolResult = "toolResult"
	ContentBlockFunction   = "function"
	ContentBlockThinking   = "thinking"
)

// CostProfile defines the pricing for a model per million tokens.
type CostProfile struct {
	Tiers      []CostTier `json:"tiers,omitempty"`
	Input      float64    `json:"input"`
	Output     float64    `json:"output"`
	CacheRead  float64    `json:"cacheRead,omitempty"`
	CacheWrite float64    `json:"cacheWrite,omitempty"`
}

// CostTier applies to the whole request when total input exceeds its threshold.
type CostTier struct {
	InputTokensAbove int     `json:"inputTokensAbove"`
	Input            float64 `json:"input"`
	Output           float64 `json:"output"`
	CacheRead        float64 `json:"cacheRead"`
	CacheWrite       float64 `json:"cacheWrite"`
}

// SupportsInput reports whether the model supports the given input type.
func (c CostProfile) SupportsInput(t InputType) bool {
	switch t {
	case InputText, InputImage, InputAudio:
		return true
	default:
		return false
	}
}

// SupportsOutput reports whether the model supports the given output type.
func (c CostProfile) SupportsOutput(t OutputType) bool {
	switch t {
	case OutputText, OutputAudio:
		return true
	default:
		return false
	}
}

// OpenRouterRouting defines OpenRouter-specific provider routing preferences.
// This mirrors the TS openRouterRouting surface used by OpenRouter-compatible
// request bodies.
type OpenRouterRouting struct {
	AllowFallbacks         *bool               `json:"allow_fallbacks,omitempty"`
	RequireParameters      *bool               `json:"require_parameters,omitempty"`
	DataCollection         string              `json:"data_collection,omitempty"`
	ZDR                    *bool               `json:"zdr,omitempty"`
	EnforceDistillableText *bool               `json:"enforce_distillable_text,omitempty"`
	Order                  []string            `json:"order,omitempty"`
	Only                   []string            `json:"only,omitempty"`
	Ignore                 []string            `json:"ignore,omitempty"`
	Quantizations          []string            `json:"quantizations,omitempty"`
	Sort                   any                 `json:"sort,omitempty"`
	MaxPrice               *OpenRouterMaxPrice `json:"max_price,omitempty"`
	PreferredMinThroughput any                 `json:"preferred_min_throughput,omitempty"`
	PreferredMaxLatency    any                 `json:"preferred_max_latency,omitempty"`
}

// OpenRouterMaxPrice mirrors the structured max_price object accepted by
// OpenRouter routing.
type OpenRouterMaxPrice struct {
	Prompt     any `json:"prompt,omitempty"`
	Completion any `json:"completion,omitempty"`
	Image      any `json:"image,omitempty"`
	Audio      any `json:"audio,omitempty"`
	Request    any `json:"request,omitempty"`
}

// VercelGatewayRouting defines Vercel AI Gateway routing preferences.
type VercelGatewayRouting struct {
	Only  []string `json:"only,omitempty"`
	Order []string `json:"order,omitempty"`
}

// CompatibilitySettings defines provider-specific compatibility flags.
// Pointer flags keep an explicit false from the catalog, which overrides
// detection; plain flags are only ever set to true.
type CompatibilitySettings struct {
	SupportsStore                   *bool `json:"supportsStore,omitempty"`
	SupportsDeveloperRole           *bool `json:"supportsDeveloperRole,omitempty"`
	SupportsReasoningEffort         *bool `json:"supportsReasoningEffort,omitempty"`
	SupportsUsageInStreaming        *bool `json:"supportsUsageInStreaming,omitempty"`
	SupportsFinishReason            *bool `json:"supportsFinishReason,omitempty"`
	SupportsStrictMode              *bool `json:"supportsStrictMode,omitempty"`
	SupportsMaxOutputTokens         *bool `json:"supportsMaxOutputTokens,omitempty"`
	SupportsLongCacheRetention      *bool `json:"supportsLongCacheRetention,omitempty"`
	SupportsTemperature             *bool `json:"supportsTemperature,omitempty"`
	SupportsEagerToolInputStreaming *bool `json:"supportsEagerToolInputStreaming,omitempty"`
	SupportsCacheControlOnTools     *bool `json:"supportsCacheControlOnTools,omitempty"`
	// ImageInput is how a System One classifier takes images: "assets",
	// "images" or "state-image-urls". Empty detects it from the provider.
	ImageInput                                  string                `json:"imageInput,omitempty"`
	SendImageOptInHeader                        *bool                 `json:"sendImageOptInHeader,omitempty"`
	ReasoningEffortMap                          map[string]string     `json:"reasoningEffortMap,omitempty"`
	OpenRouterRouting                           *OpenRouterRouting    `json:"openRouterRouting,omitempty"`
	VercelGatewayRouting                        *VercelGatewayRouting `json:"vercelGatewayRouting,omitempty"`
	ChatTemplateArgs                            json.RawMessage       `json:"chatTemplateArgs,omitempty"`
	ChatTemplateKwargs                          json.RawMessage       `json:"chatTemplateKwargs,omitempty"`
	ThinkingTokenBudgetField                    string                `json:"thinkingTokenBudgetField,omitempty"`
	SupportsThinkingTokenBudget                 bool                  `json:"supportsThinkingTokenBudget,omitempty"`
	AllowedFallbackModels                       []FallbackModel       `json:"allowedFallbackModels,omitempty"`
	MaxTokensField                              string                `json:"maxTokensField,omitempty"`
	ThinkingFormat                              string                `json:"thinkingFormat,omitempty"`
	CacheControlFormat                          string                `json:"cacheControlFormat,omitempty"`
	SessionAffinityFormat                       string                `json:"sessionAffinityFormat,omitempty"`
	SupportsStrictTools                         bool                  `json:"supportsStrictTools,omitempty"`
	RequiresToolResultName                      bool                  `json:"requiresToolResultName,omitempty"`
	RequiresAssistantAfterToolResult            bool                  `json:"requiresAssistantAfterToolResult,omitempty"`
	RequiresThinkingAsText                      bool                  `json:"requiresThinkingAsText,omitempty"`
	RequiresReasoningContentOnAssistantMessages bool                  `json:"requiresReasoningContentOnAssistantMessages,omitempty"`
	ZAIToolStream                               bool                  `json:"zaiToolStream,omitempty"`
	SendSessionAffinityHeaders                  bool                  `json:"sendSessionAffinityHeaders,omitempty"`
	AllowEmptySignature                         bool                  `json:"allowEmptySignature,omitempty"`
	ForceAdaptiveThinking                       bool                  `json:"forceAdaptiveThinking,omitempty"`
	SupportsMidConvoEffort                      bool                  `json:"supportsMidConvoEffort,omitempty"`
	SupportsMidConvoSystemMessages              bool                  `json:"supportsMidConvoSystemMessages,omitempty"`
	SupportsMidConvoToolAdditions               bool                  `json:"supportsMidConvoToolAdditions,omitempty"`
	SupportsMidConvoToolChanges                 bool                  `json:"supportsMidConvoToolChanges,omitempty"`
	SupportsOpenAIGrammarTools                  bool                  `json:"supportsOpenAIGrammarTools,omitempty"`
	SupportsAdditionalTools                     bool                  `json:"supportsAdditionalTools,omitempty"`
	SupportsToolSearch                          bool                  `json:"supportsToolSearch,omitempty"`
	SupportsExplicitPromptCacheMode             bool                  `json:"supportsExplicitPromptCacheMode,omitempty"`
}

// FallbackModel is a model a provider may answer with instead of the
// requested one, with its own pricing.
type FallbackModel struct {
	Provider string      `json:"provider"`
	Model    string      `json:"model"`
	Cost     CostProfile `json:"cost"`
}

// PromptCacheTTL lists a provider's prompt cache lifetimes in seconds.
type PromptCacheTTL struct {
	Short int `json:"short"`
	Long  int `json:"long"`
}

// ModelServingProvider is an upstream provider a catalog model can be served by.
type ModelServingProvider struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Credential string `json:"credential,omitempty"`
	Source     string `json:"source,omitempty"`
}

// OpenAIResponsesCompat mirrors the pi-mono OpenAIResponsesCompat interface
// for OpenAI Responses APIs. Currently reserved for future use.
type OpenAIResponsesCompat struct{}

// OpenAICompletionsCompat mirrors the pi-mono OpenAICompletionsCompat interface
// for OpenAI-compatible completions APIs.
type OpenAICompletionsCompat struct {
	CacheControlFormat         string
	SupportsLongCacheRetention bool
	// ReasoningDetails: whether reasoning_details/thought_signature is enabled
	// P7-OC01: When enabled, we get reasoning_content in stream chunks
	ReasoningDetails bool
	// StrictMode: whether strict mode is enabled for the model
	// P7-OC02: Used for OpenAI o-series models with strict mode
	StrictMode bool
	// ReasoningEffort: whether the provider supports reasoning effort configuration
	ReasoningEffort bool
	// ReasoningEffortValue: the provider-specific value for high reasoning effort
	ReasoningEffortValue string
	// SupportsStore: whether the provider supports the store field
	SupportsStore bool
	// SupportsDeveloperRole: whether the provider supports the developer role (vs system)
	SupportsDeveloperRole bool
	// SupportsReasoningEffort: whether the provider supports reasoning_effort
	SupportsReasoningEffort bool
	// ReasoningEffortMap: optional mapping from reasoning levels to provider-specific values
	ReasoningEffortMap map[string]string
	// SupportsUsageInStreaming: whether stream_options include_usage is supported
	SupportsUsageInStreaming bool
	// SupportsFinishReason: whether streams report finish_reason; when false a
	// missing one is inferred from the content.
	SupportsFinishReason bool
	// MaxTokensField: which field to use for max tokens ("max_completion_tokens" or "max_tokens")
	MaxTokensField string
	// RequiresToolResultName: whether tool results require the name field
	RequiresToolResultName bool
	// RequiresAssistantAfterToolResult: whether user message after tool result needs assistant bridge
	RequiresAssistantAfterToolResult bool
	// RequiresThinkingAsText: whether thinking blocks must be converted to text
	RequiresThinkingAsText bool
	// ThinkingFormat: format for reasoning/thinking param ("openai", "openrouter", "zai", "qwen", "qwen-chat-template")
	ThinkingFormat string
	// ZAIToolStream: whether z.ai supports tool_stream: true
	ZAIToolStream bool
	// RequiresReasoningContentOnAssistantMessages: whether replayed assistant
	// messages need a reasoning_content field, empty if nothing streamed.
	RequiresReasoningContentOnAssistantMessages bool
	// SendSessionAffinityHeaders: whether to send the session_id,
	// x-client-request-id, and x-session-affinity HTTP headers from the
	// session id when cache retention is enabled. Default false — matches
	// pi-mono's openai-completions provider. Providers that reject unknown
	// headers (e.g., opencode.ai/zen/go returns HTTP 500 with the session_id
	// header present) must keep this false.
	SendSessionAffinityHeaders bool
	// SupportsOpenAIGrammarTools: whether grammar-constrained tools are sent
	// as OpenAI custom tools.
	SupportsOpenAIGrammarTools bool
	// SupportsStrictMode: whether the provider supports strict field in tool definitions
	SupportsStrictMode bool
	// OpenRouterRouting: OpenRouter-specific provider routing preferences
	OpenRouterRouting *OpenRouterRouting
	// VercelGatewayRouting: Vercel AI Gateway routing preferences
	VercelGatewayRouting *VercelGatewayRouting
}

// Init initializes the OpenAICompletionsCompat struct from compatibility settings.
// P7-OC01: Parse reasoning_details for thoughtSignature
// P7-OC02: Add strict mode support (strictMode)
// P7-OC03: Add reasoning effort support (reasoningEffort, reasoningEffortValue)
func (c *OpenAICompletionsCompat) Init(settings *CompatibilitySettings) {
	if settings == nil {
		return
	}

	// P7-OC02: strict mode support
	c.StrictMode = settings.SupportsStrictMode != nil && *settings.SupportsStrictMode

	// P7-OC03: reasoning effort support
	c.ReasoningEffort = settings.SupportsReasoningEffort != nil && *settings.SupportsReasoningEffort
	if c.ReasoningEffort && settings.ReasoningEffortMap != nil {
		if val, ok := settings.ReasoningEffortMap["high"]; ok {
			c.ReasoningEffortValue = val
		}
	}
}

// InitFromModel initializes the OpenAICompletionsCompat struct from a Model.
// P7-OC01: Use model.Reasoning to enable reasoning_details/thoughtSignature support
func (c *OpenAICompletionsCompat) InitFromModel(model *Model) {
	if model == nil {
		return
	}

	// P7-OC01: When model has reasoning enabled, we expect reasoning_content in stream chunks
	c.ReasoningDetails = model.Reasoning
}

// ProviderResponse captures basic HTTP response metadata exposed to callbacks.
type ProviderResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
}

// ModelImageResizeOptions defines the cache-safe image resize profile.
type ModelImageResizeOptions struct {
	MaxWidth    int `json:"maxWidth,omitempty"`
	MaxHeight   int `json:"maxHeight,omitempty"`
	MaxBytes    int `json:"maxBytes,omitempty"`
	JPEGQuality int `json:"jpegQuality,omitempty"`
}

// ModelImageInputLimits bounds image counts and preprocessing.
type ModelImageInputLimits struct {
	Resize        *ModelImageResizeOptions `json:"resize,omitempty"`
	MaxPerMessage int                      `json:"maxPerMessage,omitempty"`
	MaxPerRequest int                      `json:"maxPerRequest,omitempty"`
}

// ModelInputLimits describes provider request limits.
type ModelInputLimits struct {
	MaxRequestBytes int                    `json:"maxRequestBytes,omitempty"`
	Images          *ModelImageInputLimits `json:"images,omitempty"`
}

// Model represents an operation-specific model from a provider.
type Model struct {
	requestConfig                 *modelRequestConfig
	Type                          string                    `json:"type,omitempty"`
	InputLimits                   *ModelInputLimits         `json:"inputLimits,omitempty"`
	SamplingParams                map[string]any            `json:"samplingParams,omitempty"`
	SamplingParamsByThinkingLevel map[string]map[string]any `json:"samplingParamsByThinkingLevel,omitempty"`
	ThinkingLevelMap              map[string]*string        `json:"thinkingLevelMap,omitempty"`
	ID                            string                    `json:"id"`
	Name                          string                    `json:"name"`
	API                           string                    `json:"api"`
	Provider                      string                    `json:"provider"`
	BaseURL                       string                    `json:"baseUrl,omitempty"`
	Headers                       map[string]string         `json:"headers,omitempty"`
	Reasoning                     bool                      `json:"reasoning"`
	Input                         []InputType               `json:"input"`
	Output                        []OutputType              `json:"output"`
	Cost                          CostProfile               `json:"cost"`
	ContextWindow                 int                       `json:"contextWindow"`
	MaxTokens                     int                       `json:"maxTokens"`
	Compat                        *CompatibilitySettings    `json:"compat,omitempty"`
	PromptCache                   *PromptCacheTTL           `json:"promptCache,omitempty"`
	// Lab, Enabled, and Providers describe models of catalog services, such
	// as Radius, that route each model to one of several providers.
	Lab       string                 `json:"lab,omitempty"`
	Free      bool                   `json:"free,omitempty"`
	Enabled   *bool                  `json:"enabled,omitempty"`
	Providers []ModelServingProvider `json:"providers,omitempty"`
}

// SupportsInput reports whether the model supports the given input type.
func (m *Model) SupportsInput(t InputType) bool {
	for _, in := range m.Input {
		if in == t {
			return true
		}
	}

	return false
}

// SupportsOutput reports whether the model supports the given output type.
func (m *Model) SupportsOutput(t OutputType) bool {
	for _, out := range m.Output {
		if out == t {
			return true
		}
	}

	return false
}

// GetCompat returns the resolved OpenAICompletionsCompat for this model.
// Uses model.Compat if set, otherwise auto-detects from provider/base URL.
func (m *Model) GetCompat() *OpenAICompletionsCompat {
	compat := &OpenAICompletionsCompat{}

	// Start with auto-detected defaults
	detected := detectModelCompat(m)

	// If model.Compat is set, use it to override defaults
	if m.Compat != nil {
		if m.Compat.CacheControlFormat != "" {
			detected.CacheControlFormat = m.Compat.CacheControlFormat
		}
		if m.Compat.SupportsLongCacheRetention != nil {
			detected.SupportsLongCacheRetention = *m.Compat.SupportsLongCacheRetention
		}
		if m.Compat.SupportsStore != nil {
			detected.SupportsStore = *m.Compat.SupportsStore
		}
		if m.Compat.SupportsDeveloperRole != nil {
			detected.SupportsDeveloperRole = *m.Compat.SupportsDeveloperRole
		}
		if m.Compat.SupportsReasoningEffort != nil {
			detected.SupportsReasoningEffort = *m.Compat.SupportsReasoningEffort
		}
		if m.Compat.ReasoningEffortMap != nil {
			detected.ReasoningEffortMap = m.Compat.ReasoningEffortMap
		}
		if m.Compat.SupportsUsageInStreaming != nil {
			detected.SupportsUsageInStreaming = *m.Compat.SupportsUsageInStreaming
		}
		if m.Compat.SupportsFinishReason != nil {
			detected.SupportsFinishReason = *m.Compat.SupportsFinishReason
		}
		if m.Compat.MaxTokensField != "" {
			detected.MaxTokensField = m.Compat.MaxTokensField
		}
		if m.Compat.RequiresToolResultName {
			detected.RequiresToolResultName = m.Compat.RequiresToolResultName
		}
		if m.Compat.RequiresAssistantAfterToolResult {
			detected.RequiresAssistantAfterToolResult = m.Compat.RequiresAssistantAfterToolResult
		}
		if m.Compat.RequiresThinkingAsText {
			detected.RequiresThinkingAsText = m.Compat.RequiresThinkingAsText
		}
		if m.Compat.ThinkingFormat != "" {
			detected.ThinkingFormat = m.Compat.ThinkingFormat
		}
		if m.Compat.OpenRouterRouting != nil {
			detected.OpenRouterRouting = m.Compat.OpenRouterRouting
		}
		if m.Compat.VercelGatewayRouting != nil {
			detected.VercelGatewayRouting = m.Compat.VercelGatewayRouting
		}
		if m.Compat.ZAIToolStream {
			detected.ZAIToolStream = m.Compat.ZAIToolStream
		}
		if m.Compat.RequiresReasoningContentOnAssistantMessages {
			detected.RequiresReasoningContentOnAssistantMessages = true
		}
		if m.Compat.SendSessionAffinityHeaders {
			detected.SendSessionAffinityHeaders = m.Compat.SendSessionAffinityHeaders
		}
		if m.Compat.SupportsStrictMode != nil {
			detected.SupportsStrictMode = *m.Compat.SupportsStrictMode
		}
		detected.SupportsOpenAIGrammarTools = m.Compat.SupportsOpenAIGrammarTools
	}

	*compat = detected
	return compat
}

// detectModelCompat auto-detects compatibility settings from provider and base URL.
// Mirrors the TypeScript detectCompat function from providers/openai-completions.ts.
func detectModelCompat(m *Model) OpenAICompletionsCompat {
	provider := m.Provider
	baseURL := m.BaseURL

	isZAI := provider == ProviderZAI || provider == ProviderZAICodingCN ||
		strings.Contains(baseURL, "api.z.ai") || strings.Contains(baseURL, "open.bigmodel.cn")
	isTogether := provider == ProviderTogether || strings.Contains(baseURL, "api.together.ai") || strings.Contains(baseURL, "api.together.xyz")
	isMoonshot := provider == ProviderMoonshotAI || provider == ProviderMoonshotAICN || strings.Contains(baseURL, "api.moonshot.")
	isOpenRouter := provider == ProviderOpenRouter || strings.Contains(baseURL, "openrouter.ai")
	isCloudflareWorkersAI := provider == ProviderCloudflareWorkersAI || strings.Contains(baseURL, "api.cloudflare.com")
	isCloudflareAIGateway := provider == ProviderCloudflareAIGateway || strings.Contains(baseURL, "gateway.ai.cloudflare.com")
	isNVIDIA := provider == ProviderNVIDIA || strings.Contains(baseURL, "integrate.api.nvidia.com")
	isAntLing := provider == ProviderAntLing || strings.Contains(baseURL, "api.ant-ling.com")
	isDeepSeek := provider == ProviderDeepSeek || strings.Contains(strings.ToLower(baseURL), "deepseek.com")

	isNonStandard := isNVIDIA ||
		provider == ProviderCerebras ||
		strings.Contains(baseURL, "cerebras.ai") ||
		provider == ProviderxAI ||
		strings.Contains(baseURL, "api.x.ai") ||
		isTogether ||
		strings.Contains(baseURL, "chutes.ai") ||
		isDeepSeek ||
		isZAI ||
		isMoonshot ||
		provider == ProviderOpenCode ||
		strings.Contains(baseURL, "opencode.ai") ||
		isCloudflareWorkersAI ||
		isCloudflareAIGateway ||
		isAntLing

	useMaxTokens := strings.Contains(baseURL, "chutes.ai") || isDeepSeek || isMoonshot || isCloudflareAIGateway ||
		isTogether || isNVIDIA || isAntLing || isZAI

	isGrok := provider == ProviderxAI || strings.Contains(baseURL, "api.x.ai")
	isGroq := provider == ProviderGroq || strings.Contains(baseURL, "groq.com")

	// Special reasoning effort map for groq qwen
	reasoningEffortMap := make(map[string]string)
	if isGroq && m.ID == "qwen/qwen3-32b" {
		reasoningEffortMap["minimal"] = "default"
		reasoningEffortMap["low"] = "default"
		reasoningEffortMap["medium"] = "default"
		reasoningEffortMap["high"] = "default"
		reasoningEffortMap["xhigh"] = "default"
	}

	// GLM-5.3 on opencode-go always engages in thinking and cannot be disabled.
	// Sending reasoning_effort: "" (empty) is interpreted as trying to disable
	// thinking and rejected. Disable reasoning_effort for this model.
	supportsReasoningEffort := !isGrok && !isZAI && !isMoonshot && !isTogether && !isCloudflareAIGateway && !isNVIDIA && !isAntLing
	if provider == ProviderOpenCodeGo && m.ID == "glm-5.3" {
		supportsReasoningEffort = false
	}

	thinkingFormat := ThinkingFormatOpenAI
	switch {
	case isDeepSeek:
		thinkingFormat = ThinkingFormatDeepSeek
	case isZAI:
		thinkingFormat = ThinkingFormatZAI
	case isTogether:
		thinkingFormat = ThinkingFormatTogether
	case isAntLing:
		thinkingFormat = ThinkingFormatAntLing
	case isOpenRouter:
		thinkingFormat = ThinkingFormatOpenRouter
	}

	var cacheControlFormat string
	if provider == ProviderOpenRouter && strings.HasPrefix(m.ID, "anthropic/") {
		cacheControlFormat = "anthropic"
	}
	return OpenAICompletionsCompat{
		CacheControlFormat:                          cacheControlFormat,
		SupportsLongCacheRetention:                  !isTogether && !isCloudflareWorkersAI && !isCloudflareAIGateway && !isNVIDIA && !isAntLing,
		SupportsStore:                               !isNonStandard,
		SupportsDeveloperRole:                       !isNonStandard,
		SupportsReasoningEffort:                     supportsReasoningEffort,
		ReasoningEffortMap:                          reasoningEffortMap,
		SupportsUsageInStreaming:                    true,
		SupportsFinishReason:                        true,
		MaxTokensField:                              map[bool]string{true: "max_tokens", false: "max_completion_tokens"}[useMaxTokens],
		RequiresToolResultName:                      false,
		RequiresAssistantAfterToolResult:            false,
		RequiresThinkingAsText:                      false,
		ThinkingFormat:                              thinkingFormat,
		RequiresReasoningContentOnAssistantMessages: isDeepSeek,
		OpenRouterRouting:                           nil,
		VercelGatewayRouting:                        nil,
		ZAIToolStream:                               false,
		SupportsStrictMode:                          true,
	}
}

// StopReason indicates why the model stopped generating.
type StopReason string

const (
	StopReasonPending StopReason = "pending"
	StopReasonStop    StopReason = "stop"
	StopReasonLength  StopReason = "length"
	StopReasonToolUse StopReason = "toolUse"
	StopReasonError   StopReason = "error"
	StopReasonAborted StopReason = "aborted"
)

// ReasoningLevel defines the level of reasoning effort.
type ReasoningLevel string

const (
	ReasoningOff     ReasoningLevel = "off"
	ReasoningMinimal ReasoningLevel = "minimal"
	ReasoningLow     ReasoningLevel = "low"
	ReasoningMedium  ReasoningLevel = "medium"
	ReasoningHigh    ReasoningLevel = "high"
	ReasoningXHigh   ReasoningLevel = "xhigh"
	ReasoningMax     ReasoningLevel = "max"
)

// ContentBlock represents a piece of content in a message.
type ContentBlock struct {
	// Namespace identifies dynamically loaded Responses tools for same-model replay.
	Namespace  *string         `json:"namespace,omitempty"`
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	Data       string          `json:"data,omitempty"`
	MimeType   string          `json:"mimeType,omitempty"`
	ID         string          `json:"id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Thinking   string          `json:"thinking,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"` // For tool call content blocks
	ToolName   string          `json:"toolName,omitempty"`   // For tool call content blocks
	// ThoughtSignature is set by providers that emit encrypted/thought signatures
	// (e.g., OpenAI encrypted reasoning). It should be preserved on same-model
	// replay and stripped on cross-model replay.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
	// Redacted indicates this is a redacted thinking block. Redacted blocks
	// should only be preserved on same-model boundaries; cross-model boundaries
	// should convert them to plain text or drop them.
	Redacted bool `json:"redacted,omitempty"`
	// TextSignature preserves the original text block signature for replay.
	TextSignature string `json:"textSignature,omitempty"`
}

// ToolCall represents a tool call from the model.
type ToolCall struct {
	Namespace *string         `json:"namespace,omitempty"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Tool defines a callable tool for the model.
type ConstrainedSamplingConfig struct {
	Type     string            `json:"type"`
	Strict   string            `json:"strict,omitempty"`
	Variants map[string]string `json:"variants,omitempty"`
}

type Tool struct {
	ConstrainedSampling json.RawMessage `json:"constrainedSampling,omitempty"`
	Name                string          `json:"name"`
	Description         string          `json:"description"`
	Parameters          json.RawMessage `json:"parameters"`
}

// Message represents a conversation message.
type ToolReference struct {
	Name string `json:"name"`
}

type Message struct {
	Diagnostics  []AssistantDiagnostic `json:"diagnostics,omitempty"`
	Sections     map[string]*string    `json:"sections,omitempty"`
	ToolsAdded   []Tool                `json:"toolsAdded,omitempty"`
	ToolsRemoved []ToolReference       `json:"toolsRemoved,omitempty"`
	DurationMs   *float64              `json:"durationMs,omitempty"`
	Role         string                `json:"role"`
	Content      any                   `json:"content"`
	ID           string                `json:"id,omitempty"`
	ToolCallID   string                `json:"toolCallId,omitempty"`
	ToolName     string                `json:"toolName,omitempty"`
	IsError      bool                  `json:"isError,omitempty"`
	// Timestamp is a Unix timestamp in milliseconds at the public ai boundary.
	Timestamp int64 `json:"timestamp"`
	// Provider, API, and Model store the source model info for cross-model
	// transformation tracking (e.g., for thinking block conversion).
	Provider   string `json:"provider,omitempty"`
	API        string `json:"api,omitempty"`
	Model      string `json:"model,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
	// ProviderThinkingLevel is the native effort an assistant turn was produced with.
	ProviderThinkingLevel string `json:"providerThinkingLevel,omitempty"`
}

type messageAlias Message

// ContentString extracts a simple string from Content.
// Returns (string, true) if Content is a string.
// Returns ("", false) if Content is []ContentBlock (caller should iterate blocks).
// Returns ("", false) if Content is anything else.
func (m *Message) ContentString() (string, bool) {
	switch c := m.Content.(type) {
	case string:
		return c, true
	case []ContentBlock:
		return "", false
	default:
		return "", false
	}
}

// ContentBlocks returns Content as []ContentBlock.
// Returns nil if Content is not []ContentBlock.
func (m *Message) ContentBlocks() []ContentBlock {
	switch c := m.Content.(type) {
	case []ContentBlock:
		return c
	default:
		return nil
	}
}

// IsAssistantMessage returns true if Content is []ContentBlock (assistant role).
func (m *Message) IsAssistantMessage() bool {
	return m.Role == "assistant" && m.ContentBlocks() != nil
}

// ToAssistantMessage converts a Message to an AssistantMessage if it has
// []ContentBlock content (assistant role). Returns (empty, false) if not an
// assistant message.
func (m *Message) ToAssistantMessage() (AssistantMessage, bool) {
	if !m.IsAssistantMessage() {
		return AssistantMessage{}, false
	}

	return AssistantMessage{
		Role:                  m.Role,
		Content:               m.ContentBlocks(),
		Provider:              m.Provider,
		API:                   m.API,
		Model:                 m.Model,
		StopReason:            StopReason(m.StopReason),
		Timestamp:             m.Timestamp,
		DurationMs:            m.DurationMs,
		Diagnostics:           m.Diagnostics,
		ProviderThinkingLevel: m.ProviderThinkingLevel,
	}, true
}

// IsErrorMessage returns true if this is an errored or aborted assistant message.
func (m *Message) IsErrorMessage() bool {
	return m.Role == "assistant" && (m.StopReason == string(StopReasonError) || m.StopReason == string(StopReasonAborted))
}

// ModelInfo holds model identification from an augmented message.
type ModelInfo struct {
	Provider string
	API      string
	Model    string
}

// MarshalJSON preserves the public role contract by emitting tool results as
// "toolResult" even when callers still populate the legacy "tool" role, and
// emits timestamps in Unix milliseconds at the public ai boundary.
func (m *Message) MarshalJSON() ([]byte, error) {
	alias := messageAlias(*m)
	alias.Role = normalizeMessageRole(alias.Role)
	alias.Timestamp = normalizeUnixMilliseconds(alias.Timestamp)

	return json.Marshal(alias)
}

// UnmarshalJSON preserves input compatibility for legacy "tool" payloads while
// normalizing the public role contract to "toolResult".
func (m *Message) UnmarshalJSON(data []byte) error {
	var alias messageAlias
	wire := struct {
		*messageAlias
		Content json.RawMessage `json:"content"`
	}{messageAlias: &alias}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if len(wire.Content) > 0 {
		if wire.Content[0] == '[' {
			var blocks []ContentBlock
			if err := json.Unmarshal(wire.Content, &blocks); err != nil {
				return fmt.Errorf("decode message content blocks: %w", err)
			}
			alias.Content = blocks
		} else if err := json.Unmarshal(wire.Content, &alias.Content); err != nil {
			return fmt.Errorf("decode message content: %w", err)
		}
	}

	alias.Role = normalizeMessageRole(alias.Role)
	alias.Timestamp = normalizeUnixMilliseconds(alias.Timestamp)
	*m = Message(alias)

	return nil
}

// AssistantMessage represents a response from the model.
type AssistantDiagnostic struct {
	Type      string            `json:"type"`
	Timestamp int64             `json:"timestamp"`
	Error     map[string]string `json:"error,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
}

type AssistantMessage struct {
	Diagnostics []AssistantDiagnostic `json:"diagnostics,omitempty"`
	DurationMs  *float64              `json:"durationMs,omitempty"`
	Role        string                `json:"role"`
	API         string                `json:"api,omitempty"`
	Provider    string                `json:"provider,omitempty"`
	Model       string                `json:"model,omitempty"`
	Content     []ContentBlock        `json:"content"`
	StopReason  StopReason            `json:"stopReason"`
	Usage       Usage                 `json:"usage"`
	// Timestamp is a Unix timestamp in milliseconds at the public ai boundary.
	Timestamp    int64  `json:"timestamp,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	ResponseID   string `json:"responseId,omitempty"`
	// ProviderThinkingLevel is the native effort an assistant turn was produced with.
	ProviderThinkingLevel string `json:"providerThinkingLevel,omitempty"`
}

type assistantMessageAlias AssistantMessage

// MarshalJSON preserves Unix-millisecond timestamps at the public ai boundary.
func (m AssistantMessage) MarshalJSON() ([]byte, error) {
	alias := assistantMessageAlias(m)
	alias.Timestamp = normalizeUnixMilliseconds(alias.Timestamp)
	alias.Usage = normalizeUsage(alias.Usage)

	return json.Marshal(alias)
}

// UnmarshalJSON preserves Unix-millisecond timestamps at the public ai
// boundary.
func (m *AssistantMessage) UnmarshalJSON(data []byte) error {
	var alias assistantMessageAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}

	alias.Timestamp = normalizeUnixMilliseconds(alias.Timestamp)
	alias.Usage = normalizeUsage(alias.Usage)
	*m = AssistantMessage(alias)

	return nil
}

func normalizeMessageRole(role string) string {
	if role == messageRoleTool {
		return messageRoleToolResult
	}

	return role
}

func normalizeUnixMilliseconds(ts int64) int64 {
	const secondsThreshold = int64(100_000_000_000)

	if ts == 0 {
		return 0
	}

	if ts > -secondsThreshold && ts < secondsThreshold {
		return ts * 1000
	}

	return ts
}

// Usage represents token usage and cost for a response.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead,omitempty"`
	CacheWrite int `json:"cacheWrite,omitempty"`
	// CacheWrite1h is the part of CacheWrite written with one-hour retention.
	CacheWrite1h int  `json:"cacheWrite1h,omitempty"`
	TotalTokens  int  `json:"totalTokens"`
	Cost         Cost `json:"cost"`
}

func (*Usage) isStreamEvent() {}

type usageAlias Usage

// Cost represents the calculated cost of token usage.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead,omitempty"`
	CacheWrite float64 `json:"cacheWrite,omitempty"`
	Total      float64 `json:"total"`
}

type costAlias Cost

// MarshalJSON preserves the stable Usage shape and derives totals when absent.
func (u *Usage) MarshalJSON() ([]byte, error) {
	normalized := normalizeUsage(*u)

	return json.Marshal(struct {
		Input       int  `json:"input"`
		Output      int  `json:"output"`
		CacheRead   int  `json:"cacheRead"`
		CacheWrite  int  `json:"cacheWrite"`
		TotalTokens int  `json:"totalTokens"`
		Cost        Cost `json:"cost"`
	}{
		Input:       normalized.Input,
		Output:      normalized.Output,
		CacheRead:   normalized.CacheRead,
		CacheWrite:  normalized.CacheWrite,
		TotalTokens: normalized.TotalTokens,
		Cost:        normalized.Cost,
	})
}

// UnmarshalJSON preserves the stable Usage shape without re-normalizing totals.
// The server sends the correct TotalTokens value, so we preserve it rather
// than recalculating (which would be incorrect for partial streams where the
// client has already seen some cache tokens).
func (u *Usage) UnmarshalJSON(data []byte) error {
	var alias usageAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}

	*u = Usage(alias)

	return nil
}

// MarshalJSON preserves the stable Cost shape and derives totals when absent.
func (c Cost) MarshalJSON() ([]byte, error) {
	normalized := normalizeCost(c)

	return json.Marshal(struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cacheRead"`
		CacheWrite float64 `json:"cacheWrite"`
		Total      float64 `json:"total"`
	}{
		Input:      normalized.Input,
		Output:     normalized.Output,
		CacheRead:  normalized.CacheRead,
		CacheWrite: normalized.CacheWrite,
		Total:      normalized.Total,
	})
}

// UnmarshalJSON preserves the stable Cost shape and derives totals when absent.
func (c *Cost) UnmarshalJSON(data []byte) error {
	var alias costAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}

	*c = normalizeCost(Cost(alias))

	return nil
}

// CalculateCost computes the cost based on the model's pricing.
func (u *Usage) CalculateCost(profile CostProfile) {
	inputTokens := u.Input + u.CacheRead + u.CacheWrite
	matchedThreshold := -1
	for _, tier := range profile.Tiers {
		if inputTokens > tier.InputTokensAbove && tier.InputTokensAbove > matchedThreshold {
			matchedThreshold = tier.InputTokensAbove
			profile.Input, profile.Output, profile.CacheRead, profile.CacheWrite = tier.Input, tier.Output, tier.CacheRead, tier.CacheWrite
		}
	}
	u.Cost = Cost{}
	u.Cost.Input = float64(u.Input) * profile.Input / 1_000_000
	u.Cost.Output = float64(u.Output) * profile.Output / 1_000_000
	if u.CacheRead > 0 {
		u.Cost.CacheRead = float64(u.CacheRead) * profile.CacheRead / 1_000_000
	}
	if u.CacheWrite > 0 {
		// Anthropic charges twice the base input rate for one-hour cache writes.
		long := min(u.CacheWrite1h, u.CacheWrite)
		u.Cost.CacheWrite = (float64(u.CacheWrite-long)*profile.CacheWrite + float64(long)*profile.Input*2) / 1_000_000
	}
	*u = normalizeUsage(*u)
}

func normalizeUsage(u Usage) Usage {
	u.TotalTokens = u.Input + u.Output + u.CacheRead + u.CacheWrite
	u.Cost = normalizeCost(u.Cost)

	return u
}

func normalizeCost(c Cost) Cost {
	c.Total = roundCost(c.Input + c.Output + c.CacheRead + c.CacheWrite)

	return c
}

func roundCost(v float64) float64 {
	const places = 1_000_000_000_000

	return math.Round(v*places) / places
}

// Context represents the conversation context passed to LLM calls.
type Context struct {
	SystemPrompt string    `json:"systemPrompt,omitempty"`
	Messages     []Message `json:"messages"`
	Tools        []Tool    `json:"tools,omitempty"`
}

// ThinkingBudgets defines token budgets for each thinking level.
type ThinkingBudgets struct {
	Minimal *int `json:"minimal,omitempty"`
	Low     *int `json:"low,omitempty"`
	Medium  *int `json:"medium,omitempty"`
	High    *int `json:"high,omitempty"`
}

// Transport represents the transport protocol for API requests.
type Transport string

const (
	TransportSSE  Transport = "sse"
	TransportWS   Transport = "websocket"
	TransportAuto Transport = "auto"
)

// CacheRetention represents the prompt cache retention preference.
type CacheRetention string

const (
	CacheRetentionNone  CacheRetention = "none"
	CacheRetentionShort CacheRetention = "short"
	CacheRetentionLong  CacheRetention = "long"
)

// StreamEvent represents an event during streaming.
//
// Contract: Streams should emit `Start` before partial updates, then terminate
// with either `Done` carrying the final successful AssistantMessage, or
// `Error` carrying the final AssistantMessage with stopReason "error" or "aborted".
type StreamEvent interface {
	isStreamEvent()
}

// Start marks the beginning of a streaming response with a partial message.
type Start struct {
	Partial AssistantMessage
}

func (Start) isStreamEvent() {}

// TextStart marks the start of a text content block.
type TextStart struct {
	ContentIndex int
	Partial      AssistantMessage
}

func (TextStart) isStreamEvent() {}

// TextDelta is a chunk of text content.
type TextDelta struct {
	ContentIndex int
	Delta        string
	Partial      AssistantMessage
}

func (TextDelta) isStreamEvent() {}

// TextEnd marks the end of a text content block.
type TextEnd struct {
	ContentIndex     int
	Content          string
	ContentSignature string // Signature for replay verification (pi-mono parity)
	Partial          AssistantMessage
}

func (TextEnd) isStreamEvent() {}

// ThinkingStart marks the start of a thinking block.
type ThinkingStart struct {
	ContentIndex int
	Partial      AssistantMessage
}

func (ThinkingStart) isStreamEvent() {}

// ThinkingDelta is a chunk of thinking content.
type ThinkingDelta struct {
	ContentIndex int
	Delta        string
	Partial      AssistantMessage
}

func (ThinkingDelta) isStreamEvent() {}

// ThinkingEnd marks the end of a thinking block.
type ThinkingEnd struct {
	ContentIndex     int
	Content          string
	ContentSignature string // Signature for replay verification (pi-mono parity)
	Partial          AssistantMessage
}

func (ThinkingEnd) isStreamEvent() {}

// ToolCallStart marks the start of a tool call.
type ToolCallStart struct {
	ContentIndex int
	Partial      AssistantMessage
}

func (ToolCallStart) isStreamEvent() {}

// ToolCallDelta is a chunk of tool call arguments.
type ToolCallDelta struct {
	ContentIndex int
	Delta        string
	Partial      AssistantMessage
}

func (ToolCallDelta) isStreamEvent() {}

// ToolCallEnd marks the end of a tool call.
type ToolCallEnd struct {
	ContentIndex int
	ToolCall     ToolCall
	Partial      AssistantMessage
}

func (ToolCallEnd) isStreamEvent() {}

// Done marks the completion of the stream.
type Done struct {
	Reason  StopReason
	Message AssistantMessage
}

func (Done) isStreamEvent() {}

// StreamError indicates an error occurred during streaming.
type StreamError struct {
	Reason string
	Err    error
	Msg    AssistantMessage
}

func (StreamError) isStreamEvent() {}

// Error implements error.
func (e StreamError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}

	return e.Reason
}

// Unwrap returns the underlying error.
func (e StreamError) Unwrap() error {
	return e.Err
}

func emitTerminalStreamError(ctx context.Context, handler EventHandler, partial AssistantMessage, err error, reason StopReason) {
	if partial.Role == "" {
		partial.Role = "assistant"
	}
	if partial.Timestamp == 0 {
		partial.Timestamp = timestamp()
	}
	partial.StopReason = reason
	if partial.ErrorMessage == "" && err != nil {
		partial.ErrorMessage = err.Error()
	}

	handler(ctx, StreamError{
		Reason: string(partial.StopReason),
		Err:    err,
		Msg:    partial,
	})
}

// finishStream emits the terminal event for a stream that reached its end. Like
// upstream providers, it fails when the request was aborted, when the provider
// never reported a stop reason (stopped is false), or when the stop reason is
// an error.
func finishStream(ctx context.Context, handler EventHandler, output AssistantMessage, stopped bool, unstopped string) {
	switch {
	case ctx.Err() != nil:
		emitTerminalStreamError(ctx, handler, output, fmt.Errorf("request was aborted: %w", context.Cause(ctx)), StopReasonAborted)
	case !stopped:
		emitTerminalStreamError(ctx, handler, output, errors.New(unstopped), StopReasonError)
	case output.StopReason == StopReasonError || output.StopReason == StopReasonAborted:
		message := output.ErrorMessage
		if message == "" {
			message = "an unknown error occurred"
		}
		emitTerminalStreamError(ctx, handler, output, errors.New(message), StopReasonError)
	default:
		handler(ctx, Done{Reason: output.StopReason, Message: output})
	}
}

// EventHandler is called for each streaming event.
type EventHandler func(ctx context.Context, event StreamEvent)

// Streamer defines the interface for streaming completions.
type Streamer interface {
	Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) error
}

// Completer defines the interface for blocking completions.
type Completer interface {
	Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error)
}

// Client is a combined interface for streaming and blocking completions.
type Client interface {
	Streamer
	Completer
}

// NestedToolCall records a nested execution without retaining arguments or output.
type NestedToolCall struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	DurationMs *float64 `json:"durationMs,omitempty"`
	Error      string   `json:"error,omitempty"`
}
