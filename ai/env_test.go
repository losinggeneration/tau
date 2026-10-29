package ai

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// TestEnvVarRegistryContainsRequiredProviders verifies the normative
// registry includes every provider documented in the public contract.
func TestEnvVarRegistryContainsRequiredProviders(t *testing.T) {
	required := map[string]string{
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
		ProviderXiaomiTokenPlanCN:    "XIAOMI_TOKEN_PLAN_CN_API_KEY",
		ProviderXiaomiTokenPlanAMS:   "XIAOMI_TOKEN_PLAN_AMS_API_KEY",
		ProviderXiaomiTokenPlanSGP:   "XIAOMI_TOKEN_PLAN_SGP_API_KEY",
		"qwen-token-plan":            "QWEN_TOKEN_PLAN_API_KEY",
		"qwen-token-plan-cn":         "QWEN_TOKEN_PLAN_CN_API_KEY",
		"qwen-token-plan-individual": "QWEN_TOKEN_PLAN_API_KEY",
		"typesafe":                   "TYPESAFE_API_KEY",
		"baseten":                    "BASETEN_API_KEY",
		"meta":                       "META_API_KEY",
	}
	for provider, envVar := range required {
		got, ok := EnvVarRegistry[provider]
		if !ok {
			t.Errorf("EnvVarRegistry missing entry for %q", provider)
			continue
		}
		if got != envVar {
			t.Errorf("EnvVarRegistry[%q] = %q, want %q", provider, got, envVar)
		}
	}
}

// TestEnvVarRegistrySharedEnvVars verifies that providers documented to
// share one environment variable both map to the same env var in the
// registry.
func TestEnvVarRegistrySharedEnvVars(t *testing.T) {
	shared := [][]string{
		{ProviderOpenCode, ProviderOpenCodeGo},
		{ProviderMoonshotAI, ProviderMoonshotAICN},
		{ProviderCloudflareWorkersAI, ProviderCloudflareAIGateway},
	}
	for _, group := range shared {
		first := EnvVarRegistry[group[0]]
		for _, p := range group[1:] {
			if got := EnvVarRegistry[p]; got != first {
				t.Errorf("shared env var mismatch: %q=%q, %q=%q (want same)",
					group[0], first, p, got)
			}
		}
	}
}

// TestEnvVarRegistryOmitsSpecialCasedProviders verifies that providers
// resolved through dedicated special-case logic (not the plain registry)
// are not also in the registry, so there is no ambiguity about where
// their env vars are read.
func TestEnvVarRegistryOmitsSpecialCasedProviders(t *testing.T) {
	// These providers have their own precedence chains and ADC/sentinel
	// resolution, so they should not appear in the plain registry.
	special := []string{
		ProviderAnthropic,
		ProviderGitHubCopilot,
		ProviderOpenAICodex,
		ProviderAmazonBedrock,
	}
	for _, p := range special {
		if _, ok := EnvVarRegistry[p]; ok {
			t.Errorf("EnvVarRegistry should not contain special-cased provider %q", p)
		}
	}
}

// TestEnvVarRegistryOmitsLocalOnlyProviders verifies that local-only
// providers (no env-based discovery) are not in the registry.
func TestEnvVarRegistryOmitsLocalOnlyProviders(t *testing.T) {
	local := []string{
		ProviderOllama,
		ProviderLMStudio,
		ProviderVLLM,
	}
	for _, p := range local {
		if _, ok := EnvVarRegistry[p]; ok {
			t.Errorf("EnvVarRegistry should not contain local-only provider %q", p)
		}
	}
}

// TestEnvVarForProvider returns the registry entry.
func TestEnvVarForProvider(t *testing.T) {
	cases := []struct {
		provider string
		want     string
	}{
		{ProviderOpenAI, "OPENAI_API_KEY"},
		{ProviderAnthropic, ""},     // special-cased
		{ProviderAmazonBedrock, ""}, // special-cased
		{ProviderOllama, ""},        // local-only
		{ProviderAntLing, "ANT_LING_API_KEY"},
		{ProviderCloudflareAIGateway, "CLOUDFLARE_API_KEY"},
		{ProviderXiaomiTokenPlanSGP, "XIAOMI_TOKEN_PLAN_SGP_API_KEY"},
		{"unknown-provider", ""},
	}
	for _, c := range cases {
		got := EnvVarForProvider(c.provider)
		if got != c.want {
			t.Errorf("EnvVarForProvider(%q) = %q, want %q", c.provider, got, c.want)
		}
	}
}

// TestGetEnvAPIKeyPlainProviders verifies that plain API-key providers
// read the single env var from the registry.
func TestGetEnvAPIKeyPlainProviders(t *testing.T) {
	cases := []struct {
		provider string
		envVar   string
	}{
		{ProviderOpenAI, "OPENAI_API_KEY"},
		{ProviderDeepSeek, "DEEPSEEK_API_KEY"},
		{ProviderGroq, "GROQ_API_KEY"},
		{ProviderxAI, "XAI_API_KEY"},
		{ProviderOpenRouter, "OPENROUTER_API_KEY"},
		{ProviderVercelGateway, "AI_GATEWAY_API_KEY"},
		{ProviderMistral, "MISTRAL_API_KEY"},
		{ProviderHuggingFace, "HF_TOKEN"}, // HUGGINGFACE_HUB_TOKEN is the secondary alias
		{ProviderZAI, "ZAI_API_KEY"},
		{ProviderKimiCoding, "KIMI_API_KEY"},
		{ProviderFireworks, "FIREWORKS_API_KEY"},
		{ProviderXiaomi, "XIAOMI_API_KEY"},
		{ProviderAntLing, "ANT_LING_API_KEY"},
		{ProviderNVIDIA, "NVIDIA_API_KEY"},
		{ProviderTogether, "TOGETHER_API_KEY"},
		{ProviderZAICodingCN, "ZAI_CODING_CN_API_KEY"},
		{ProviderXiaomiTokenPlanCN, "XIAOMI_TOKEN_PLAN_CN_API_KEY"},
		{ProviderXiaomiTokenPlanAMS, "XIAOMI_TOKEN_PLAN_AMS_API_KEY"},
		{ProviderXiaomiTokenPlanSGP, "XIAOMI_TOKEN_PLAN_SGP_API_KEY"},
	}
	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			t.Setenv(c.envVar, "test-key-for-"+c.provider)
			if got := GetEnvAPIKey(c.provider); got != "test-key-for-"+c.provider {
				t.Errorf("GetEnvAPIKey(%q) = %q, want test-key-for-%q",
					c.provider, got, c.provider)
			}
		})
	}
}

// TestGetEnvAPIKeySharedEnvVars verifies that providers sharing one env
// var each get the same value from that var.
func TestGetEnvAPIKeySharedEnvVars(t *testing.T) {
	groups := [][]string{
		{ProviderOpenCode, ProviderOpenCodeGo},
		{ProviderMoonshotAI, ProviderMoonshotAICN, ProviderMoonshot},
		{ProviderCloudflareWorkersAI, ProviderCloudflareAIGateway, ProviderCloudflare},
	}
	envVars := map[string]string{
		"OPENCODE_API_KEY":   "opencode-key",
		"MOONSHOT_API_KEY":   "moonshot-key",
		"CLOUDFLARE_API_KEY": "cloudflare-key",
	}
	for envVar, val := range envVars {
		t.Setenv(envVar, val)
	}
	for _, group := range groups {
		for _, p := range group {
			if got := GetEnvAPIKey(p); got == "" {
				t.Errorf("GetEnvAPIKey(%q) = empty, want non-empty from shared env", p)
			}
		}
	}
}

// TestGetEnvAPIKeyReturnsEmptyForUnregisteredProvider verifies that
// providers absent from the registry and not in the special-case set
// return empty string.
func TestGetEnvAPIKeyReturnsEmptyForUnregisteredProvider(t *testing.T) {
	if got := GetEnvAPIKey("unknown-provider"); got != "" {
		t.Errorf("GetEnvAPIKey(unknown) = %q, want empty", got)
	}
	if got := GetEnvAPIKey(ProviderOllama); got != "" {
		t.Errorf("GetEnvAPIKey(ollama) = %q, want empty (local-only)", got)
	}
}

// TestGetEnvAPIKeyAnthropicPrecedenceAfterRefactor is a regression test
// verifying the OAuth-token precedence is preserved after the registry
// refactor.
func TestGetEnvAPIKeyAnthropicPrecedenceAfterRefactor(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")

	if key := GetEnvAPIKey(ProviderAnthropic); key != "" {
		t.Errorf("expected empty when no keys set, got %q", key)
	}

	t.Setenv("ANTHROPIC_API_KEY", "api-key")
	if key := GetEnvAPIKey(ProviderAnthropic); key != "api-key" {
		t.Errorf("expected api-key, got %q", key)
	}

	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "oauth-token")
	if key := GetEnvAPIKey(ProviderAnthropic); key != "api-key" {
		t.Errorf("ANTHROPIC_OAUTH_TOKEN must be ignored, got %q", key)
	}
}

// TestHasCloudflareGatewayConfig verifies the account/gateway var check
// is independent of the API key var.
func TestHasCloudflareGatewayConfig(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "")

	if HasCloudflareGatewayConfig() {
		t.Fatal("expected false with no env vars set")
	}

	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct-123")
	if HasCloudflareGatewayConfig() {
		t.Fatal("expected false with only account ID set")
	}

	t.Setenv("CLOUDFLARE_GATEWAY_ID", "gw-456")
	if !HasCloudflareGatewayConfig() {
		t.Fatal("expected true with both account and gateway IDs set")
	}
}

// TestEnvAPIKeysDerivedFromRegistry verifies that the reverse map
// contains an entry for every env var in the registry.
func TestEnvAPIKeysDerivedFromRegistry(t *testing.T) {
	seen := make(map[string]bool)
	for _, envVar := range EnvVarRegistry {
		if _, ok := EnvAPIKeys[envVar]; !ok {
			t.Errorf("EnvAPIKeys missing entry for registry env var %q", envVar)
		}
		seen[envVar] = true
	}
}

// TestEnvAPIKeysIncludesSpecialCaseVars verifies that the special-case
// providers' env vars appear in the reverse map.
func TestEnvAPIKeysIncludesSpecialCaseVars(t *testing.T) {
	required := []string{
		"ANTHROPIC_API_KEY",
		"COPILOT_GITHUB_TOKEN",
		"GH_TOKEN",
		"GITHUB_TOKEN",
		"AWS_ACCESS_KEY_ID",
		"AWS_PROFILE",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_CLOUD_LOCATION",
		"CLOUDFLARE_ACCOUNT_ID",
		"CLOUDFLARE_GATEWAY_ID",
	}
	for _, envVar := range required {
		if _, ok := EnvAPIKeys[envVar]; !ok {
			t.Errorf("EnvAPIKeys missing required special-case env var %q", envVar)
		}
	}
}

// TestEnvVarNamesMirrorsRegistryForPlainProviders verifies that the
// internal envVarNames helper returns the registry entry for plain
// providers (those without a special case).
func TestEnvVarNamesMirrorsRegistryForPlainProviders(t *testing.T) {
	// Providers that have richer special-case logic and intentionally
	// return more than one env var from envVarNames.
	special := map[string]bool{
		ProviderAnthropic:           true,
		ProviderGitHubCopilot:       true,
		ProviderOpenAICodex:         true,
		ProviderGoogleVertex:        true,
		ProviderAmazonBedrock:       true,
		ProviderCloudflareAIGateway: true,
	}
	for provider, envVar := range EnvVarRegistry {
		names := envVarNames(provider)
		if special[provider] {
			if len(names) == 0 {
				t.Errorf("envVarNames(%q) returned empty for special-cased provider", provider)
			}
			continue
		}
		if len(names) != 1 {
			t.Errorf("envVarNames(%q) = %d vars, want 1 for plain provider",
				provider, len(names))
			continue
		}
		if names[0] != envVar {
			t.Errorf("envVarNames(%q)[0] = %q, want %q", provider, names[0], envVar)
		}
	}
}

// TestEnvVarNamesSpecialCases verifies the full ordered list for
// special-cased providers.
func TestEnvVarNamesSpecialCases(t *testing.T) {
	cases := []struct {
		provider string
		contains []string
	}{
		{ProviderAnthropic, []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"}},
		{ProviderGitHubCopilot, []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}},
		{ProviderOpenAICodex, []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}},
		{ProviderAmazonBedrock, []string{"AWS_PROFILE", "AWS_ACCESS_KEY_ID"}},
		{ProviderCloudflareAIGateway, []string{"CLOUDFLARE_API_KEY", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_GATEWAY_ID"}},
	}
	for _, c := range cases {
		got := envVarNames(c.provider)
		for _, want := range c.contains {
			found := false
			for _, name := range got {
				if name == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("envVarNames(%q) missing %q (got %v)", c.provider, want, got)
			}
		}
	}
}

// TestEnvVarNamesNilForUnregisteredProvider verifies that providers with
// no registry entry and no special case return nil.
func TestEnvVarNamesNilForUnregisteredProvider(t *testing.T) {
	if got := envVarNames("nonexistent"); got != nil {
		t.Errorf("envVarNames(nonexistent) = %v, want nil", got)
	}
	if got := envVarNames(ProviderOllama); got != nil {
		t.Errorf("envVarNames(ollama) = %v, want nil (local-only)", got)
	}
}

// TestEnvVarRegistryDoesNotIncludeRemovedTauInventions verifies that
// tau-invented env var names are not promoted into the normative
// registry.
func TestEnvVarRegistryDoesNotIncludeRemovedTauInventions(t *testing.T) {
	// These env var names were tau-invented and should not appear as
	// registry entries. (They may still appear in EnvAPIKeys for backward
	// compatibility with test fixtures; that is tested separately.)
	disallowed := map[string]string{
		"VERTEXAI_API_KEY":          ProviderGoogleVertex,
		"MIDJOURNEY_API_KEY":        ProviderMiniMax,
		"OPENCODE_GO_API_KEY":       ProviderOpenCodeGo,
		"VERCEL_AI_GATEWAY_API_KEY": ProviderVercelGateway,
	}
	for envVar, provider := range disallowed {
		if got := EnvVarRegistry[provider]; got == envVar {
			t.Errorf("EnvVarRegistry should not use tau-invented %q for %q", envVar, provider)
		}
	}
}

// TestGetEnvAPIKeyReturnsEmptyAfterClear verifies that unsetting the
// env var causes GetEnvAPIKey to return empty for plain providers.
func TestGetEnvAPIKeyReturnsEmptyAfterClear(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "present")
	if got := GetEnvAPIKey(ProviderOpenAI); got != "present" {
		t.Errorf("expected 'present', got %q", got)
	}
	t.Setenv("OPENAI_API_KEY", "")
	if got := GetEnvAPIKey(ProviderOpenAI); got != "" {
		t.Errorf("expected empty after clear, got %q", got)
	}
}

// Ensure no test relies on a non-existent env var leaking in.
func TestGetEnvAPIKeyConsistentAcrossCalls(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "stable")
	for i := 0; i < 3; i++ {
		if got := GetEnvAPIKey(ProviderDeepSeek); got != "stable" {
			t.Errorf("call %d: got %q, want 'stable'", i, got)
		}
	}
}

// Sanity check: os.Getenv is being consulted at call time, not cached.
func TestGetEnvAPIKeyConsultsAtCallTime(t *testing.T) {
	os.Unsetenv("DEEPSEEK_API_KEY")
	if got := GetEnvAPIKey(ProviderDeepSeek); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	t.Setenv("DEEPSEEK_API_KEY", "set-after-unset")
	if got := GetEnvAPIKey(ProviderDeepSeek); got != "set-after-unset" {
		t.Errorf("expected 'set-after-unset', got %q", got)
	}
}

func TestAnthropicBearerEnvironmentDiscoveryAndResolutionStaySeparate(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "oauth")
	t.Setenv("ANTHROPIC_API_KEY", "key")
	if EnvAPIKeys["ANTHROPIC_AUTH_TOKEN"] != ProviderAnthropic {
		t.Error("bearer variable missing from credential inventory")
	}
	if names := envVarNames(ProviderAnthropic); !slices.Equal(names, []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"}) {
		t.Errorf("unexpected discovery order: %v", names)
	}
	if key := GetEnvAPIKey(ProviderAnthropic); key != "key" {
		t.Errorf("API-key helper must skip bearer auth, got %q", key)
	}
	headers := GetEnvHeaders(ProviderAnthropic)
	headers["Authorization"] = "mutated"
	if GetEnvHeaders(ProviderAnthropic)["Authorization"] != "Bearer bearer" {
		t.Error("environment header result retained caller mutation")
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if HasEnvCredentials(ProviderAnthropic) || len(GetEnvHeaders(ProviderAnthropic)) != 0 {
		t.Error("cleared credentials remained cached")
	}
}

func TestVertexADCDiscoveryTracksCurrentPathAndFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	if HasVertexADC() {
		t.Fatal("unexpected ADC in empty home")
	}
	path := filepath.Join(home, "adc.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	if !HasVertexADC() {
		t.Fatal("newly configured ADC was hidden by stale discovery")
	}
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			if !HasVertexADC() {
				t.Error("concurrent ADC discovery failed")
			}
		})
	}
	workers.Wait()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if HasVertexADC() {
		t.Fatal("removed ADC remained available")
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	defaultPath := filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
	if err := os.MkdirAll(filepath.Dir(defaultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !HasVertexADC() {
		t.Fatal("new default ADC was not discovered")
	}
	t.Setenv("HOME", t.TempDir())
	if HasVertexADC() {
		t.Fatal("ADC discovery retained the previous home")
	}
}
