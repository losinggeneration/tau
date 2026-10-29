package ai // import "github.com/losinggeneration/tau/ai"

import "os"

// EnvVarRegistry is the normative, user-facing registry mapping each
// built-in API-key provider identifier to the primary environment variable
// the package consults for credential discovery.
//
// This registry is part of the public contract: users configure credentials
// by setting these variables, so the variable names are stable and
// documented. Do not rename entries in this map.
//
// Providers absent from this map either have no environment-based discovery
// path (local-only providers such as ollama, lm-studio, vllm) or are
// special-cased in the package's discovery logic and resolved through
// dedicated functions (anthropic, github-copilot, openai-codex,
// amazon-bedrock, google-vertex, cloudflare-ai-gateway).
//
// Several provider identifiers share one environment variable (for example
// opencode and opencode-go both read OPENCODE_API_KEY; moonshotai and
// moonshotai-cn both read MOONSHOT_API_KEY; cloudflare-workers-ai and
// cloudflare-ai-gateway both read CLOUDFLARE_API_KEY). The selected provider
// identifier determines which variable is consulted; the variable alone does
// not disambiguate the provider.
var EnvVarRegistry = map[string]string{
	ProviderAntLing:              "ANT_LING_API_KEY",
	ProviderOpenAI:               "OPENAI_API_KEY",
	ProviderAzureOpenAI:          "AZURE_OPENAI_API_KEY",
	ProviderNVIDIA:               "NVIDIA_API_KEY",
	ProviderDeepSeek:             "DEEPSEEK_API_KEY",
	ProviderGoogle:               "GEMINI_API_KEY",
	ProviderGoogleVertex:         "GOOGLE_CLOUD_API_KEY",
	ProviderGroq:                 "GROQ_API_KEY",
	ProviderCerebras:             "CEREBRAS_API_KEY",
	ProviderxAI:                  "XAI_API_KEY",
	ProviderOpenRouter:           "OPENROUTER_API_KEY",
	ProviderVercelGateway:        "AI_GATEWAY_API_KEY",
	ProviderZAI:                  "ZAI_API_KEY",
	ProviderZAICodingCN:          "ZAI_CODING_CN_API_KEY",
	ProviderMistral:              "MISTRAL_API_KEY",
	ProviderMiniMax:              "MINIMAX_API_KEY",
	ProviderMiniMaxCN:            "MINIMAX_CN_API_KEY",
	ProviderMoonshotAI:           "MOONSHOT_API_KEY",
	ProviderMoonshotAICN:         "MOONSHOT_API_KEY",
	ProviderHuggingFace:          "HF_TOKEN",
	ProviderFireworks:            "FIREWORKS_API_KEY",
	ProviderTogether:             "TOGETHER_API_KEY",
	ProviderOpenCode:             "OPENCODE_API_KEY",
	ProviderOpenCodeGo:           "OPENCODE_API_KEY",
	ProviderKimiCoding:           "KIMI_API_KEY",
	ProviderCloudflareWorkersAI:  "CLOUDFLARE_API_KEY",
	ProviderCloudflareAIGateway:  "CLOUDFLARE_API_KEY",
	ProviderXiaomi:               "XIAOMI_API_KEY",
	ProviderRadius:               "RADIUS_API_KEY",
	ProviderLlamaCpp:             "LLAMA_API_KEY",
	ProviderXiaomiTokenPlanCN:    "XIAOMI_TOKEN_PLAN_CN_API_KEY",
	ProviderXiaomiTokenPlanAMS:   "XIAOMI_TOKEN_PLAN_AMS_API_KEY",
	ProviderXiaomiTokenPlanSGP:   "XIAOMI_TOKEN_PLAN_SGP_API_KEY",
	"qwen-token-plan":            "QWEN_TOKEN_PLAN_API_KEY",
	"qwen-token-plan-cn":         "QWEN_TOKEN_PLAN_CN_API_KEY",
	"qwen-token-plan-individual": "QWEN_TOKEN_PLAN_API_KEY",
	"typesafe":                   "TYPESAFE_API_KEY",
	"baseten":                    "BASETEN_API_KEY",
	"meta":                       "META_API_KEY",
	// Legacy tau identifiers (retained for backward compatibility with
	// existing settings/registry entries).
	ProviderMoonshot:   "MOONSHOT_API_KEY",
	ProviderCloudflare: "CLOUDFLARE_API_KEY",
}

// EnvVarForProvider returns the primary environment variable name that the
// package consults for credential discovery for the given provider
// identifier. Returns empty string for providers with no registry entry
// (local-only or special-cased providers).
//
// Use this when you need the canonical env var name for a provider, e.g.
// for help text, configuration validation, or custom credential resolution.
func EnvVarForProvider(provider string) string {
	return EnvVarRegistry[provider]
}

// HasCloudflareGatewayConfig reports whether the Cloudflare AI Gateway
// provider has both the account ID and gateway ID configured in the
// environment, as required for the gateway endpoint.
//
// This is in addition to the CLOUDFLARE_API_KEY env var (see EnvVarRegistry).
// Callers should verify CLOUDFLARE_API_KEY is also set when configuring
// cloudflare-ai-gateway credentials.
func HasCloudflareGatewayConfig() bool {
	return os.Getenv("CLOUDFLARE_ACCOUNT_ID") != "" && os.Getenv("CLOUDFLARE_GATEWAY_ID") != ""
}
