package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	tauai "github.com/losinggeneration/tau/ai"
)

const (
	modelsDevURL     = "https://models.dev/api.json"
	openRouterURL    = "https://openrouter.ai/api/v1/models"
	aiGatewayModels  = "https://ai-gateway.vercel.sh/v1/models"
	aiGatewayBaseURL = "https://ai-gateway.vercel.sh"
	cloudCodeAssist  = "https://cloudcode-pa.googleapis.com"
	antigravityURL   = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	vertexBaseURL    = "https://{location}-aiplatform.googleapis.com"
	codexBaseURL     = "https://chatgpt.com/backend-api"
	codexContext     = 272000
	codexMaxTokens   = 128000
)

var copilotStaticHeaders = map[string]string{
	"Copilot-Integration-Id": "vscode-chat",
	"Editor-Plugin-Version":  "copilot-chat/0.35.0",
	"Editor-Version":         "vscode/1.107.0",
	"User-Agent":             "GitHubCopilotChat/0.35.0",
}

var zaiToolStreamUnsupportedModels = map[string]struct{}{
	"glm-4.5":       {},
	"glm-4.5-air":   {},
	"glm-4.5-flash": {},
	"glm-4.5v":      {},
}

type modelsDevResponse map[string]modelsDevProvider

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	Name      string `json:"name"`
	ToolCall  bool   `json:"tool_call"`
	Reasoning bool   `json:"reasoning"`
	Status    string `json:"status"`
	Limit     struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Cost struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	} `json:"cost"`
	Modalities struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	Provider struct {
		NPM string `json:"npm"`
	} `json:"provider"`
}

type openRouterResponse struct {
	Data []openRouterModel `json:"data"`
}

type openRouterModel struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	ContextLength       int      `json:"context_length"`
	SupportedParameters []string `json:"supported_parameters"`
	Architecture        struct {
		Modality any `json:"modality"`
	} `json:"architecture"`
	Pricing struct {
		Prompt          string `json:"prompt"`
		Completion      string `json:"completion"`
		InputCacheRead  string `json:"input_cache_read"`
		InputCacheWrite string `json:"input_cache_write"`
	} `json:"pricing"`
	TopProvider struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

type aiGatewayResponse struct {
	Data []aiGatewayModel `json:"data"`
}

type aiGatewayModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ContextWindow int      `json:"context_window"`
	MaxTokens     int      `json:"max_tokens"`
	Tags          []string `json:"tags"`
	Pricing       struct {
		Input           any `json:"input"`
		Output          any `json:"output"`
		InputCacheRead  any `json:"input_cache_read"`
		InputCacheWrite any `json:"input_cache_write"`
	} `json:"pricing"`
}

func main() {
	var output string
	flag.StringVar(&output, "output", "", "path to write the generated Go file")
	flag.Parse()

	if output == "" {
		fmt.Fprintln(os.Stderr, "missing required -output")
		os.Exit(2)
	}

	if err := run(output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	models, err := generateModels(ctx)
	if err != nil {
		return err
	}

	content, err := renderGo(models)
	if err != nil {
		return fmt.Errorf("render generated Go: %w", err)
	}

	if err := os.WriteFile(output, content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}

	fmt.Printf("generated %s with %d models\n", output, len(models))

	return nil
}

func generateModels(ctx context.Context) ([]*tauai.Model, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	modelsDevModels, err := loadModelsDev(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("load models.dev: %w", err)
	}

	openRouterModels, err := loadOpenRouter(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("load openrouter: %w", err)
	}

	aiGatewayModels, err := loadAIGateway(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("load ai gateway: %w", err)
	}

	allModels := append(modelsDevModels, openRouterModels...)
	allModels = append(allModels, aiGatewayModels...)
	allModels = filterModels(allModels)
	allModels = applyOverrides(allModels)
	addStaticModels(&allModels)
	addAzureModels(&allModels)

	models := dedupeModels(allModels)
	slices.SortFunc(models, func(a, b *tauai.Model) int {
		if c := cmp.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})

	return models, nil
}

func loadModelsDev(ctx context.Context, client *http.Client) ([]*tauai.Model, error) {
	var payload modelsDevResponse
	if err := fetchJSON(ctx, client, modelsDevURL, &payload); err != nil {
		return nil, err
	}

	var result []*tauai.Model

	appendProviderModels := func(sourceKey, provider, api, baseURL string) {
		data, ok := payload[sourceKey]
		if !ok {
			return
		}
		for id, model := range data.Models {
			if !model.ToolCall {
				continue
			}

			result = append(result, newModel(id, firstNonEmpty(model.Name, id), api, provider, baseURL, model.Reasoning, model.Modalities.Input, costProfile(model.Cost.Input, model.Cost.Output, model.Cost.CacheRead, model.Cost.CacheWrite), defaultInt(model.Limit.Context, 4096), defaultInt(model.Limit.Output, 4096)))
		}
	}

	appendProviderModels("anthropic", "anthropic", tauai.APIAnthropicMessages, "https://api.anthropic.com")
	appendProviderModels("google", "google", tauai.APIGoogleGenerativeAI, "https://generativelanguage.googleapis.com/v1beta")
	appendProviderModels("openai", "openai", tauai.APIOpenAIResponses, "https://api.openai.com/v1")
	appendProviderModels("groq", "groq", tauai.APIOpenAICompletions, "https://api.groq.com/openai/v1")
	appendProviderModels("cerebras", "cerebras", tauai.APIOpenAICompletions, "https://api.cerebras.ai/v1")
	appendProviderModels("xai", "xai", tauai.APIOpenAICompletions, "https://api.x.ai/v1")
	appendProviderModels("mistral", "mistral", tauai.APIMistralConversations, "https://api.mistral.ai")
	appendProviderModels("huggingface", "huggingface", tauai.APIOpenAICompletions, "https://router.huggingface.co/v1")

	if data, ok := payload["amazon-bedrock"]; ok {
		for id, model := range data.Models {
			if !model.ToolCall {
				continue
			}
			if strings.HasPrefix(id, "ai21.jamba") || strings.HasPrefix(id, "mistral.mistral-7b-instruct-v0") {
				continue
			}
			result = append(result, newModel(id, firstNonEmpty(model.Name, id), tauai.APIBedrockConverseStream, "amazon-bedrock", "https://bedrock-runtime.us-east-1.amazonaws.com", model.Reasoning, model.Modalities.Input, costProfile(model.Cost.Input, model.Cost.Output, model.Cost.CacheRead, model.Cost.CacheWrite), defaultInt(model.Limit.Context, 4096), defaultInt(model.Limit.Output, 4096)))
		}
	}

	if data, ok := payload["zai-coding-plan"]; ok {
		for id, model := range data.Models {
			if !model.ToolCall {
				continue
			}
			m := newModel(id, firstNonEmpty(model.Name, id), tauai.APIOpenAICompletions, "zai", "https://api.z.ai/api/coding/paas/v4", model.Reasoning, model.Modalities.Input, costProfile(model.Cost.Input, model.Cost.Output, model.Cost.CacheRead, model.Cost.CacheWrite), defaultInt(model.Limit.Context, 4096), defaultInt(model.Limit.Output, 4096))
			m.Compat = &tauai.CompatibilitySettings{
				SupportsDeveloperRole: false,
				ThinkingFormat:        tauai.ThinkingFormatZAI,
			}
			if _, ok := zaiToolStreamUnsupportedModels[id]; !ok {
				m.Compat.ZAIToolStream = true
			}
			result = append(result, m)
		}
	}

	opencodeVariants := []struct {
		SourceKey string
		Provider  string
		BasePath  string
	}{
		{SourceKey: "opencode", Provider: "opencode", BasePath: "https://opencode.ai/zen"},
		{SourceKey: "opencode-go", Provider: "opencode-go", BasePath: "https://opencode.ai/zen/go"},
	}
	for _, variant := range opencodeVariants {
		data, ok := payload[variant.SourceKey]
		if !ok {
			continue
		}
		for id, model := range data.Models {
			if !model.ToolCall || model.Status == "deprecated" {
				continue
			}

			api := tauai.APIOpenAICompletions
			baseURL := variant.BasePath + "/v1"
			switch model.Provider.NPM {
			case "@ai-sdk/openai":
				api = tauai.APIOpenAIResponses
			case "@ai-sdk/anthropic":
				api = tauai.APIAnthropicMessages
				baseURL = variant.BasePath
			case "@ai-sdk/google":
				api = tauai.APIGoogleGenerativeAI
			}

			m := newModel(id, firstNonEmpty(model.Name, id), api, variant.Provider, baseURL, model.Reasoning, model.Modalities.Input, costProfile(model.Cost.Input, model.Cost.Output, model.Cost.CacheRead, model.Cost.CacheWrite), defaultInt(model.Limit.Context, 4096), defaultInt(model.Limit.Output, 4096))
			// OpenCode Kimi K2.6 accepts DeepSeek-style thinking objects and
			// rejects string thinking values or combined reasoning_effort, so
			// tag it with the deepseek thinking format and disable
			// reasoning_effort. Mirrors pi-mono's generate-models.ts rule.
			if (variant.Provider == "opencode" || variant.Provider == "opencode-go") && id == "kimi-k2.6" {
				if m.Compat == nil {
					m.Compat = &tauai.CompatibilitySettings{MaxTokensField: "max_tokens"}
				}
				m.Compat.ThinkingFormat = tauai.ThinkingFormatDeepSeek
				m.Compat.SupportsReasoningEffort = false
			}
			// GLM-5.3 on opencode-go always engages in thinking and cannot be
			// disabled. Sending reasoning_effort: "" (empty) is interpreted as
			// trying to disable thinking and rejected. Disable reasoning_effort
			// to prevent the default case from setting it to an empty string.
			// The fix is also in detectModelCompat for robustness.
			if variant.Provider == "opencode-go" && id == "glm-5.3" {
				if m.Compat == nil {
					m.Compat = &tauai.CompatibilitySettings{MaxTokensField: "max_tokens"}
				}
				m.Compat.SupportsReasoningEffort = false
			}
			result = append(result, m)
		}
	}

	if data, ok := payload["github-copilot"]; ok {
		for id, model := range data.Models {
			if !model.ToolCall || model.Status == "deprecated" {
				continue
			}

			api := tauai.APIOpenAICompletions
			if isCopilotClaude4(id) {
				api = tauai.APIAnthropicMessages
			} else if strings.HasPrefix(id, "gpt-5") || strings.HasPrefix(id, "oswe") {
				api = tauai.APIOpenAIResponses
			}

			m := newModel(id, firstNonEmpty(model.Name, id), api, "github-copilot", "https://api.individual.githubcopilot.com", model.Reasoning, model.Modalities.Input, costProfile(model.Cost.Input, model.Cost.Output, model.Cost.CacheRead, model.Cost.CacheWrite), defaultInt(model.Limit.Context, 128000), defaultInt(model.Limit.Output, 8192))
			m.Headers = copyStringMap(copilotStaticHeaders)
			if api == tauai.APIOpenAICompletions {
				m.Compat = &tauai.CompatibilitySettings{
					SupportsStore:           false,
					SupportsDeveloperRole:   false,
					SupportsReasoningEffort: false,
				}
			}
			result = append(result, m)
		}
	}

	minimaxVariants := []struct {
		SourceKey string
		Provider  string
		BaseURL   string
	}{
		{SourceKey: "minimax", Provider: "minimax", BaseURL: "https://api.minimax.io/anthropic"},
		{SourceKey: "minimax-cn", Provider: "minimax-cn", BaseURL: "https://api.minimaxi.com/anthropic"},
	}
	for _, variant := range minimaxVariants {
		data, ok := payload[variant.SourceKey]
		if !ok {
			continue
		}
		for id, model := range data.Models {
			if !model.ToolCall {
				continue
			}
			result = append(result, newModel(id, firstNonEmpty(model.Name, id), tauai.APIAnthropicMessages, variant.Provider, variant.BaseURL, model.Reasoning, model.Modalities.Input, costProfile(model.Cost.Input, model.Cost.Output, model.Cost.CacheRead, model.Cost.CacheWrite), defaultInt(model.Limit.Context, 4096), defaultInt(model.Limit.Output, 4096)))
		}
	}

	if data, ok := payload["kimi-for-coding"]; ok {
		_, hasCanonicalModel := data.Models["kimi-for-coding"]
		for id, model := range data.Models {
			if !model.ToolCall {
				continue
			}
			if id == "k2p5" && hasCanonicalModel {
				continue
			}
			normalizedID := id
			normalizedName := firstNonEmpty(model.Name, id)
			if id == "k2p5" {
				normalizedID = "kimi-for-coding"
				normalizedName = "Kimi For Coding"
			}
			result = append(result, newModel(normalizedID, normalizedName, tauai.APIAnthropicMessages, "kimi-coding", "https://api.kimi.com/coding", model.Reasoning, model.Modalities.Input, costProfile(model.Cost.Input, model.Cost.Output, model.Cost.CacheRead, model.Cost.CacheWrite), defaultInt(model.Limit.Context, 4096), defaultInt(model.Limit.Output, 4096)))
		}
	}

	return result, nil
}

func loadOpenRouter(ctx context.Context, client *http.Client) ([]*tauai.Model, error) {
	var payload openRouterResponse
	if err := fetchJSON(ctx, client, openRouterURL, &payload); err != nil {
		return nil, err
	}

	var result []*tauai.Model
	for _, model := range payload.Data {
		if !containsString(model.SupportedParameters, "tools") {
			continue
		}
		result = append(result, newModel(
			model.ID,
			firstNonEmpty(model.Name, model.ID),
			tauai.APIOpenAICompletions,
			"openrouter",
			"https://openrouter.ai/api/v1",
			containsString(model.SupportedParameters, "reasoning"),
			openRouterModalities(model.Architecture.Modality),
			costProfile(parseFloat(model.Pricing.Prompt)*1_000_000, parseFloat(model.Pricing.Completion)*1_000_000, parseFloat(model.Pricing.InputCacheRead)*1_000_000, parseFloat(model.Pricing.InputCacheWrite)*1_000_000),
			defaultInt(model.ContextLength, 4096),
			defaultInt(model.TopProvider.MaxCompletionTokens, 4096),
		))
	}

	return result, nil
}

func loadAIGateway(ctx context.Context, client *http.Client) ([]*tauai.Model, error) {
	var payload aiGatewayResponse
	if err := fetchJSON(ctx, client, aiGatewayModels, &payload); err != nil {
		return nil, err
	}

	var result []*tauai.Model
	for _, model := range payload.Data {
		if !containsString(model.Tags, "tool-use") {
			continue
		}
		inputs := []string{"text"}
		if containsString(model.Tags, "vision") {
			inputs = append(inputs, "image")
		}
		result = append(result, newModel(
			model.ID,
			firstNonEmpty(model.Name, model.ID),
			tauai.APIAnthropicMessages,
			"vercel-ai-gateway",
			aiGatewayBaseURL,
			containsString(model.Tags, "reasoning"),
			inputs,
			costProfile(toFloat(model.Pricing.Input)*1_000_000, toFloat(model.Pricing.Output)*1_000_000, toFloat(model.Pricing.InputCacheRead)*1_000_000, toFloat(model.Pricing.InputCacheWrite)*1_000_000),
			defaultInt(model.ContextWindow, 4096),
			defaultInt(model.MaxTokens, 4096),
		))
	}

	return result, nil
}

func filterModels(models []*tauai.Model) []*tauai.Model {
	result := make([]*tauai.Model, 0, len(models))
	for _, model := range models {
		if (model.Provider == "opencode" || model.Provider == "opencode-go") && model.ID == "gpt-5.3-codex-spark" {
			continue
		}
		result = append(result, model)
	}
	return result
}

func applyOverrides(models []*tauai.Model) []*tauai.Model {
	for _, candidate := range models {
		if candidate.Provider == "anthropic" && candidate.ID == "claude-opus-4-5" {
			candidate.Cost.CacheRead = 0.5
			candidate.Cost.CacheWrite = 6.25
		}
		if candidate.Provider == "amazon-bedrock" && strings.Contains(candidate.ID, "anthropic.claude-opus-4-6-v1") {
			candidate.Cost.CacheRead = 0.5
			candidate.Cost.CacheWrite = 6.25
		}
		if (candidate.Provider == "anthropic" || candidate.Provider == "opencode" || candidate.Provider == "opencode-go" || candidate.Provider == "github-copilot") &&
			(candidate.ID == "claude-opus-4-6" || candidate.ID == "claude-sonnet-4-6" || candidate.ID == "claude-opus-4.6" || candidate.ID == "claude-sonnet-4.6") {
			candidate.ContextWindow = 1_000_000
		}
		if candidate.Provider == "google-antigravity" && (candidate.ID == "claude-opus-4-6-thinking" || candidate.ID == "claude-sonnet-4-6") {
			candidate.ContextWindow = 1_000_000
		}
		if (candidate.Provider == "opencode" || candidate.Provider == "opencode-go") && (candidate.ID == "claude-sonnet-4-5" || candidate.ID == "claude-sonnet-4") {
			candidate.ContextWindow = 200000
		}
		if (candidate.Provider == "opencode" || candidate.Provider == "opencode-go") && candidate.ID == "gpt-5.4" {
			candidate.ContextWindow = 272000
			candidate.MaxTokens = 128000
		}
		if candidate.Provider == "openai" && candidate.ID == "gpt-5.4" {
			candidate.ContextWindow = 272000
			candidate.MaxTokens = 128000
		}
		if candidate.Provider == "openrouter" && candidate.ID == "moonshotai/kimi-k2.5" {
			candidate.Cost.Input = 0.41
			candidate.Cost.Output = 2.06
			candidate.Cost.CacheRead = 0.07
			candidate.MaxTokens = 4096
		}
		if candidate.Provider == "openrouter" && candidate.ID == "z-ai/glm-5" {
			candidate.Cost.Input = 0.6
			candidate.Cost.Output = 1.9
			candidate.Cost.CacheRead = 0.119
		}
	}

	result := make([]*tauai.Model, 0, len(models))
	result = append(result, models...)

	return result
}

func addStaticModels(models *[]*tauai.Model) {
	appendIfMissing := func(model *tauai.Model) {
		for _, existing := range *models {
			if existing.Provider == model.Provider && existing.ID == model.ID {
				return
			}
		}
		*models = append(*models, model)
	}

	appendIfMissing(newModel("eu.anthropic.claude-opus-4-6-v1", "Claude Opus 4.6 (EU)", tauai.APIBedrockConverseStream, "amazon-bedrock", "https://bedrock-runtime.us-east-1.amazonaws.com", true, []string{"text", "image"}, costProfile(5, 25, 0.5, 6.25), 200000, 128000))
	appendIfMissing(newModel("claude-opus-4-6", "Claude Opus 4.6", tauai.APIAnthropicMessages, "anthropic", "https://api.anthropic.com", true, []string{"text", "image"}, costProfile(5, 25, 0.5, 6.25), 1_000_000, 128000))
	appendIfMissing(newModel("claude-opus-4-7", "Claude Opus 4.7", tauai.APIAnthropicMessages, "anthropic", "https://api.anthropic.com", true, []string{"text", "image"}, costProfile(5, 25, 0.5, 6.25), 1_000_000, 128000))
	appendIfMissing(newModel("claude-sonnet-4-6", "Claude Sonnet 4.6", tauai.APIAnthropicMessages, "anthropic", "https://api.anthropic.com", true, []string{"text", "image"}, costProfile(3, 15, 0.3, 3.75), 1_000_000, 64000))
	appendIfMissing(newModel("gemini-3.1-flash-lite-preview", "Gemini 3.1 Flash Lite Preview", tauai.APIGoogleGenerativeAI, "google", "https://generativelanguage.googleapis.com/v1beta", true, []string{"text", "image"}, costProfile(0, 0, 0, 0), 1048576, 65536))
	appendIfMissing(newModel("gpt-5-chat-latest", "GPT-5 Chat Latest", tauai.APIOpenAIResponses, "openai", "https://api.openai.com/v1", false, []string{"text", "image"}, costProfile(1.25, 10, 0.125, 0), 128000, 16384))
	appendIfMissing(newModel("codex-mini-latest", "Codex Mini", tauai.APIOpenAIResponses, "openai", "https://api.openai.com/v1", true, []string{"text", "image"}, costProfile(1.5, 6, 0.375, 0), 200000, 100000))
	appendIfMissing(newModel("gpt-5.1-codex", "GPT-5.1 Codex", tauai.APIOpenAIResponses, "openai", "https://api.openai.com/v1", true, []string{"text", "image"}, costProfile(1.25, 5, 0.125, 1.25), 400000, 128000))
	appendIfMissing(newModel("gpt-5.1-codex-max", "GPT-5.1 Codex Max", tauai.APIOpenAIResponses, "openai", "https://api.openai.com/v1", true, []string{"text", "image"}, costProfile(1.25, 10, 0.125, 0), 400000, 128000))
	appendIfMissing(newModel("gpt-5.3-codex-spark", "GPT-5.3 Codex Spark", tauai.APIOpenAIResponses, "openai", "https://api.openai.com/v1", true, []string{"text"}, costProfile(0, 0, 0, 0), 128000, 16384))
	appendIfMissing(newModel("gpt-5.4", "GPT-5.4", tauai.APIOpenAIResponses, "openai", "https://api.openai.com/v1", true, []string{"text", "image"}, costProfile(2.5, 15, 0.25, 0), 272000, 128000))

	copilotBaseModel := findModel(*models, "github-copilot", "gpt-5.2-codex")
	if copilotBaseModel != nil {
		appendIfMissing(cloneWithIDAndName(copilotBaseModel, "gpt-5.3-codex", "GPT-5.3 Codex"))
	}

	// OpenAI Codex (ChatGPT OAuth) serves a subset of the openai lineup. Clone
	// those models from the openai provider entries collected above; codex
	// responses are enforced to a ~272k context by the backend regardless of
	// the model's marketing window. codexModelIDs mirrors the models reported
	// by GET chatgpt.com/backend-api/codex/models (visible ones only).
	codexModelIDs := []string{"gpt-5.4-mini", "gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-6-astra"}
	for _, id := range codexModelIDs {
		base := findModel(*models, "openai", id)
		if base == nil {
			continue
		}
		codex := cloneWithIDAndName(base, base.ID, base.Name)
		codex.API = tauai.APIOpenAICodexResponses
		codex.Provider = "openai-codex"
		codex.BaseURL = codexBaseURL
		codex.ContextWindow = codexContext
		codex.MaxTokens = codexMaxTokens
		appendIfMissing(codex)
	}

	appendIfMissing(newModel("grok-code-fast-1", "Grok Code Fast 1", tauai.APIOpenAICompletions, "xai", "https://api.x.ai/v1", false, []string{"text"}, costProfile(0.2, 1.5, 0.02, 0), 32768, 8192))
	appendIfMissing(newModel("auto", "Auto", tauai.APIOpenAICompletions, "openrouter", "https://openrouter.ai/api/v1", true, []string{"text", "image"}, costProfile(0, 0, 0, 0), 2000000, 30000))

	cloudCodeAssistModels := []*tauai.Model{
		newModel("gemini-2.5-pro", "Gemini 2.5 Pro (Cloud Code Assist)", tauai.APIGoogleGeminiCLI, "google-gemini-cli", cloudCodeAssist, true, []string{"text", "image"}, costProfile(0, 0, 0, 0), 1048576, 65535),
		newModel("gemini-2.5-flash", "Gemini 2.5 Flash (Cloud Code Assist)", tauai.APIGoogleGeminiCLI, "google-gemini-cli", cloudCodeAssist, true, []string{"text", "image"}, costProfile(0, 0, 0, 0), 1048576, 65535),
		newModel("gemini-2.0-flash", "Gemini 2.0 Flash (Cloud Code Assist)", tauai.APIGoogleGeminiCLI, "google-gemini-cli", cloudCodeAssist, false, []string{"text", "image"}, costProfile(0, 0, 0, 0), 1048576, 8192),
		newModel("gemini-3-pro-preview", "Gemini 3 Pro Preview (Cloud Code Assist)", tauai.APIGoogleGeminiCLI, "google-gemini-cli", cloudCodeAssist, true, []string{"text", "image"}, costProfile(0, 0, 0, 0), 1048576, 65535),
		newModel("gemini-3-flash-preview", "Gemini 3 Flash Preview (Cloud Code Assist)", tauai.APIGoogleGeminiCLI, "google-gemini-cli", cloudCodeAssist, true, []string{"text", "image"}, costProfile(0, 0, 0, 0), 1048576, 65535),
		newModel("gemini-3.1-pro-preview", "Gemini 3.1 Pro Preview (Cloud Code Assist)", tauai.APIGoogleGeminiCLI, "google-gemini-cli", cloudCodeAssist, true, []string{"text", "image"}, costProfile(0, 0, 0, 0), 1048576, 65535),
	}
	for _, model := range cloudCodeAssistModels {
		appendIfMissing(model)
	}

	antigravityModels := []*tauai.Model{
		newModel("gemini-3.1-pro-high", "Gemini 3.1 Pro High (Antigravity)", tauai.APIGoogleGeminiCLI, "google-antigravity", antigravityURL, true, []string{"text", "image"}, costProfile(2, 12, 0.2, 2.375), 1048576, 65535),
		newModel("gemini-3.1-pro-low", "Gemini 3.1 Pro Low (Antigravity)", tauai.APIGoogleGeminiCLI, "google-antigravity", antigravityURL, true, []string{"text", "image"}, costProfile(2, 12, 0.2, 2.375), 1048576, 65535),
		newModel("gemini-3-flash", "Gemini 3 Flash (Antigravity)", tauai.APIGoogleGeminiCLI, "google-antigravity", antigravityURL, true, []string{"text", "image"}, costProfile(0.5, 3, 0.5, 0), 1048576, 65535),
		newModel("claude-sonnet-4-5", "Claude Sonnet 4.5 (Antigravity)", tauai.APIGoogleGeminiCLI, "google-antigravity", antigravityURL, false, []string{"text", "image"}, costProfile(3, 15, 0.3, 3.75), 200000, 64000),
		newModel("claude-sonnet-4-5-thinking", "Claude Sonnet 4.5 Thinking (Antigravity)", tauai.APIGoogleGeminiCLI, "google-antigravity", antigravityURL, true, []string{"text", "image"}, costProfile(3, 15, 0.3, 3.75), 200000, 64000),
		newModel("claude-opus-4-5-thinking", "Claude Opus 4.5 Thinking (Antigravity)", tauai.APIGoogleGeminiCLI, "google-antigravity", antigravityURL, true, []string{"text", "image"}, costProfile(5, 25, 0.5, 6.25), 200000, 64000),
		newModel("claude-opus-4-6-thinking", "Claude Opus 4.6 Thinking (Antigravity)", tauai.APIGoogleGeminiCLI, "google-antigravity", antigravityURL, true, []string{"text", "image"}, costProfile(5, 25, 0.5, 6.25), 200000, 128000),
		newModel("claude-sonnet-4-6", "Claude Sonnet 4.6 (Antigravity)", tauai.APIGoogleGeminiCLI, "google-antigravity", antigravityURL, true, []string{"text", "image"}, costProfile(3, 15, 0.3, 3.75), 200000, 64000),
		newModel("gpt-oss-120b-medium", "GPT-OSS 120B Medium (Antigravity)", tauai.APIGoogleGeminiCLI, "google-antigravity", antigravityURL, false, []string{"text"}, costProfile(0.09, 0.36, 0, 0), 131072, 32768),
	}
	for _, model := range antigravityModels {
		appendIfMissing(model)
	}

	vertexModels := []*tauai.Model{
		newModel("gemini-3-pro-preview", "Gemini 3 Pro Preview (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, true, []string{"text", "image"}, costProfile(2, 12, 0.2, 0), 1000000, 64000),
		newModel("gemini-3.1-pro-preview", "Gemini 3.1 Pro Preview (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, true, []string{"text", "image"}, costProfile(2, 12, 0.2, 0), 1048576, 65536),
		newModel("gemini-3.1-pro-preview-customtools", "Gemini 3.1 Pro Preview Custom Tools (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, true, []string{"text", "image"}, costProfile(2, 12, 0.2, 0), 1048576, 65536),
		newModel("gemini-3-flash-preview", "Gemini 3 Flash Preview (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, true, []string{"text", "image"}, costProfile(0.5, 3, 0.05, 0), 1048576, 65536),
		newModel("gemini-2.0-flash", "Gemini 2.0 Flash (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, false, []string{"text", "image"}, costProfile(0.15, 0.6, 0.0375, 0), 1048576, 8192),
		newModel("gemini-2.0-flash-lite", "Gemini 2.0 Flash Lite (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, true, []string{"text", "image"}, costProfile(0.075, 0.3, 0.01875, 0), 1048576, 65536),
		newModel("gemini-2.5-pro", "Gemini 2.5 Pro (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, true, []string{"text", "image"}, costProfile(1.25, 10, 0.125, 0), 1048576, 65536),
		newModel("gemini-2.5-flash", "Gemini 2.5 Flash (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, true, []string{"text", "image"}, costProfile(0.3, 2.5, 0.03, 0), 1048576, 65536),
		newModel("gemini-2.5-flash-lite-preview-09-2025", "Gemini 2.5 Flash Lite Preview 09-25 (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, true, []string{"text", "image"}, costProfile(0.1, 0.4, 0.01, 0), 1048576, 65536),
		newModel("gemini-2.5-flash-lite", "Gemini 2.5 Flash Lite (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, true, []string{"text", "image"}, costProfile(0.1, 0.4, 0.01, 0), 1048576, 65536),
		newModel("gemini-1.5-pro", "Gemini 1.5 Pro (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, false, []string{"text", "image"}, costProfile(1.25, 5, 0.3125, 0), 1000000, 8192),
		newModel("gemini-1.5-flash", "Gemini 1.5 Flash (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, false, []string{"text", "image"}, costProfile(0.075, 0.3, 0.01875, 0), 1000000, 8192),
		newModel("gemini-1.5-flash-8b", "Gemini 1.5 Flash-8B (Vertex)", tauai.APIGoogleVertex, "google-vertex", vertexBaseURL, false, []string{"text", "image"}, costProfile(0.0375, 0.15, 0.01, 0), 1000000, 8192),
	}
	for _, model := range vertexModels {
		appendIfMissing(model)
	}
}

func addAzureModels(models *[]*tauai.Model) {
	var clones []*tauai.Model
	for _, model := range *models {
		if model.Provider != "openai" || model.API != tauai.APIOpenAIResponses {
			continue
		}
		clones = append(clones, &tauai.Model{
			ID:            model.ID,
			Name:          model.Name,
			API:           tauai.APIAzureOpenAIResponses,
			Provider:      "azure-openai-responses",
			Reasoning:     model.Reasoning,
			Input:         append([]tauai.InputType(nil), model.Input...),
			Output:        append([]tauai.OutputType(nil), model.Output...),
			Cost:          model.Cost,
			ContextWindow: model.ContextWindow,
			MaxTokens:     model.MaxTokens,
		})
	}
	*models = append(*models, clones...)
}

func dedupeModels(models []*tauai.Model) []*tauai.Model {
	providers := make(map[string]map[string]*tauai.Model)

	for _, model := range models {
		normalizeModel(model)
		applyGoCompatDefaults(model)

		providerModels, ok := providers[model.Provider]
		if !ok {
			providerModels = make(map[string]*tauai.Model)
			providers[model.Provider] = providerModels
		}
		if _, exists := providerModels[model.ID]; !exists {
			providerModels[model.ID] = model
		}
	}

	var result []*tauai.Model
	for _, providerModels := range providers {
		for _, model := range providerModels {
			result = append(result, model)
		}
	}

	return result
}

func normalizeModel(model *tauai.Model) {
	if len(model.Input) == 0 {
		model.Input = []tauai.InputType{tauai.InputText}
	}
	if len(model.Output) == 0 {
		model.Output = []tauai.OutputType{tauai.OutputText}
	}
}

func applyGoCompatDefaults(model *tauai.Model) {
	if model.Compat == nil {
		model.Compat = &tauai.CompatibilitySettings{}
	}

	switch model.API {
	case tauai.APIAnthropicMessages:
		model.Compat.SupportsDeveloperRole = true
		model.Compat.SupportsReasoningEffort = true
		model.Compat.SupportsUsageInStreaming = true
		model.Compat.MaxTokensField = "max_tokens"
		model.Compat.RequiresToolResultName = true
		model.Compat.RequiresAssistantAfterToolResult = true
		model.Compat.RequiresThinkingAsText = true
		model.Compat.ThinkingFormat = tauai.ThinkingFormatZAI
	case tauai.APIOpenAICompletions:
		if model.Compat.MaxTokensField == "" {
			model.Compat.MaxTokensField = "max_tokens"
		}
	case tauai.APIOpenAIResponses:
		model.Compat.SupportsReasoningEffort = true
	case tauai.APIAzureOpenAIResponses:
		model.Compat.MaxTokensField = "max_completion_tokens"
		model.Compat.SupportsReasoningEffort = true
	case tauai.APIOpenAICodexResponses:
		model.Compat.SupportsReasoningEffort = true
	}

	if model.Provider == "zai" {
		model.Compat.ThinkingFormat = tauai.ThinkingFormatZAI
	}

	if isEmptyCompat(model.Compat) {
		model.Compat = nil
	}
}

func isEmptyCompat(compat *tauai.CompatibilitySettings) bool {
	if compat == nil {
		return true
	}
	return !compat.SupportsStore &&
		!compat.SupportsDeveloperRole &&
		!compat.SupportsReasoningEffort &&
		!compat.SupportsUsageInStreaming &&
		!compat.SupportsStrictMode &&
		compat.MaxTokensField == "" &&
		!compat.RequiresToolResultName &&
		!compat.RequiresAssistantAfterToolResult &&
		!compat.RequiresThinkingAsText &&
		compat.ThinkingFormat == "" &&
		!compat.ZAIToolStream
}

func renderGo(models []*tauai.Model) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("// Code generated by go generate; DO NOT EDIT.\n")
	buf.WriteString("// Source: internal/app/genmodels\n\n")
	buf.WriteString("package ai\n\n")
	buf.WriteString("func registerGeneratedModels() {\n")
	buf.WriteString("\tfor _, model := range generatedModels {\n")
	buf.WriteString("\t\tRegisterModel(model)\n")
	buf.WriteString("\t}\n")
	buf.WriteString("}\n\n")
	buf.WriteString("var generatedModels = []*Model{\n")
	for _, model := range models {
		writeModel(&buf, model)
	}
	buf.WriteString("}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("gofmt generated source: %w", err)
	}

	return formatted, nil
}

func writeModel(buf *bytes.Buffer, model *tauai.Model) {
	buf.WriteString("\t{\n")
	writeStringField(buf, "ID", model.ID)
	writeStringField(buf, "Name", model.Name)
	writeStringField(buf, "API", model.API)
	writeStringField(buf, "Provider", model.Provider)
	if model.BaseURL != "" {
		writeStringField(buf, "BaseURL", model.BaseURL)
	}
	if len(model.Headers) > 0 {
		buf.WriteString("\t\tHeaders: map[string]string{\n")
		keys := sortedKeys(model.Headers)
		for _, key := range keys {
			buf.WriteString("\t\t\t" + strconv.Quote(key) + ": " + strconv.Quote(model.Headers[key]) + ",\n")
		}
		buf.WriteString("\t\t},\n")
	}
	buf.WriteString("\t\tReasoning: " + strconv.FormatBool(model.Reasoning) + ",\n")
	writeInputTypes(buf, model.Input)
	writeOutputTypes(buf, model.Output)
	buf.WriteString("\t\tCost: CostProfile{")
	buf.WriteString("Input: " + formatFloat(model.Cost.Input) + ", ")
	buf.WriteString("Output: " + formatFloat(model.Cost.Output) + ", ")
	buf.WriteString("CacheRead: " + formatFloat(model.Cost.CacheRead) + ", ")
	buf.WriteString("CacheWrite: " + formatFloat(model.Cost.CacheWrite) + "},\n")
	buf.WriteString("\t\tContextWindow: " + strconv.Itoa(model.ContextWindow) + ",\n")
	buf.WriteString("\t\tMaxTokens: " + strconv.Itoa(model.MaxTokens) + ",\n")
	if model.Compat != nil {
		writeCompat(buf, model.Compat)
	}
	buf.WriteString("\t},\n")
}

func writeCompat(buf *bytes.Buffer, compat *tauai.CompatibilitySettings) {
	buf.WriteString("\t\tCompat: &CompatibilitySettings{\n")
	if compat.SupportsStore {
		buf.WriteString("\t\t\tSupportsStore: true,\n")
	}
	if compat.SupportsDeveloperRole {
		buf.WriteString("\t\t\tSupportsDeveloperRole: true,\n")
	}
	if compat.SupportsReasoningEffort {
		buf.WriteString("\t\t\tSupportsReasoningEffort: true,\n")
	}
	if compat.SupportsUsageInStreaming {
		buf.WriteString("\t\t\tSupportsUsageInStreaming: true,\n")
	}
	if compat.SupportsStrictMode {
		buf.WriteString("\t\t\tSupportsStrictMode: true,\n")
	}
	if compat.MaxTokensField != "" {
		writeStringFieldAt(buf, "\t\t\t", "MaxTokensField", compat.MaxTokensField)
	}
	if compat.RequiresToolResultName {
		buf.WriteString("\t\t\tRequiresToolResultName: true,\n")
	}
	if compat.RequiresAssistantAfterToolResult {
		buf.WriteString("\t\t\tRequiresAssistantAfterToolResult: true,\n")
	}
	if compat.RequiresThinkingAsText {
		buf.WriteString("\t\t\tRequiresThinkingAsText: true,\n")
	}
	if compat.ThinkingFormat != "" {
		writeStringFieldAt(buf, "\t\t\t", "ThinkingFormat", compat.ThinkingFormat)
	}
	if compat.ZAIToolStream {
		buf.WriteString("\t\t\tZAIToolStream: true,\n")
	}
	buf.WriteString("\t\t},\n")
}

func writeStringField(buf *bytes.Buffer, field, value string) {
	writeStringFieldAt(buf, "\t\t", field, value)
}

func writeStringFieldAt(buf *bytes.Buffer, indent, field, value string) {
	buf.WriteString(indent + field + ": " + strconv.Quote(value) + ",\n")
}

func writeInputTypes(buf *bytes.Buffer, inputs []tauai.InputType) {
	buf.WriteString("\t\tInput: []InputType{")
	for i, input := range inputs {
		if i > 0 {
			buf.WriteString(", ")
		}
		switch input {
		case tauai.InputText:
			buf.WriteString("InputText")
		case tauai.InputImage:
			buf.WriteString("InputImage")
		case tauai.InputAudio:
			buf.WriteString("InputAudio")
		default:
			buf.WriteString("InputType(" + strconv.Quote(string(input)) + ")")
		}
	}
	buf.WriteString("},\n")
}

func writeOutputTypes(buf *bytes.Buffer, outputs []tauai.OutputType) {
	buf.WriteString("\t\tOutput: []OutputType{")
	for i, output := range outputs {
		if i > 0 {
			buf.WriteString(", ")
		}
		switch output {
		case tauai.OutputText:
			buf.WriteString("OutputText")
		case tauai.OutputAudio:
			buf.WriteString("OutputAudio")
		default:
			buf.WriteString("OutputType(" + strconv.Quote(string(output)) + ")")
		}
	}
	buf.WriteString("},\n")
}

func fetchJSON(ctx context.Context, client *http.Client, url string, out any) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("new request %s: %w", url, err)
	}
	req.Header.Set("User-Agent", "tau-genmodels/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", url, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close response body %s: %w", url, closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr != nil {
			return fmt.Errorf("unexpected status %s for %s", resp.Status, url)
		}
		return fmt.Errorf("unexpected status %s for %s: %s", resp.Status, url, strings.TrimSpace(string(body)))
	}

	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}

	return nil
}

func newModel(id, name, api, provider, baseURL string, reasoning bool, modalities []string, cost tauai.CostProfile, contextWindow, maxTokens int) *tauai.Model {
	return &tauai.Model{
		ID:            id,
		Name:          name,
		API:           api,
		Provider:      provider,
		BaseURL:       baseURL,
		Reasoning:     reasoning,
		Input:         inputTypes(modalities),
		Output:        []tauai.OutputType{tauai.OutputText},
		Cost:          cost,
		ContextWindow: contextWindow,
		MaxTokens:     maxTokens,
	}
}

func inputTypes(modalities []string) []tauai.InputType {
	inputs := []tauai.InputType{tauai.InputText}
	if containsString(modalities, "image") {
		inputs = append(inputs, tauai.InputImage)
	}
	if containsString(modalities, "audio") {
		inputs = append(inputs, tauai.InputAudio)
	}
	return inputs
}

func openRouterModalities(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []any:
		result := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if ok {
				result = append(result, s)
			}
		}
		return result
	case []string:
		return v
	default:
		return nil
	}
}

func costProfile(input, output, cacheRead, cacheWrite float64) tauai.CostProfile {
	return tauai.CostProfile{
		Input:      input,
		Output:     output,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func isCopilotClaude4(id string) bool {
	return strings.HasPrefix(id, "claude-haiku-4") ||
		strings.HasPrefix(id, "claude-sonnet-4") ||
		strings.HasPrefix(id, "claude-opus-4")
}

func defaultInt(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func parseFloat(s string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return value
}

func toFloat(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case string:
		return parseFloat(v)
	default:
		return 0
	}
}

func findModel(models []*tauai.Model, provider, id string) *tauai.Model {
	for _, model := range models {
		if model.Provider == provider && model.ID == id {
			return model
		}
	}
	return nil
}

func cloneWithIDAndName(model *tauai.Model, id, name string) *tauai.Model {
	clone := *model
	clone.ID = id
	clone.Name = name
	if model.Headers != nil {
		clone.Headers = copyStringMap(model.Headers)
	}
	if model.Compat != nil {
		compat := *model.Compat
		clone.Compat = &compat
	}
	clone.Input = append([]tauai.InputType(nil), model.Input...)
	clone.Output = append([]tauai.OutputType(nil), model.Output...)
	return &clone
}

func copyStringMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
