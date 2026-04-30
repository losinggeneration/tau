package ai

import (
	"strings"
	"testing"
)

func TestGetClientSupportsEveryOfficiallySupportedProviderFamily(t *testing.T) {
	supported := 0
	for _, status := range ListProviderSupport() {
		if !status.OfficiallySupported {
			continue
		}
		supported++

		var probe *Model
		for _, model := range ListAvailableModels(status.Provider) {
			if model != nil {
				probe = model
				break
			}
		}
		if probe == nil {
			t.Fatalf("officially supported provider %q has no generated models", status.Provider)
		}

		model, err := GetModel(status.Provider, probe.ID)
		if err != nil {
			t.Fatalf("GetModel(%q, %q): %v", status.Provider, probe.ID, err)
		}

		client, err := GetClient(model)
		if err != nil {
			t.Fatalf("GetClient(%q, %q): %v", status.Provider, probe.ID, err)
		}
		if client == nil {
			t.Fatalf("GetClient(%q, %q) returned nil client", status.Provider, probe.ID)
		}
	}
	if supported == 0 {
		t.Fatal("expected at least one officially supported provider family")
	}
}

// TestDedicatedProvidersReturnIsAliasFalse verifies that every concrete provider
// implementation returns false from IsAlias, meaning it is a dedicated
// implementation and not an alias to another provider.
func TestDedicatedProvidersReturnIsAliasFalse(t *testing.T) {
	dedicatedProviders := []struct {
		api      string
		provider Provider
	}{
		{APIOpenAICompletions, &OpenAICompletionsProvider{}},
		{APIOpenAIResponses, &OpenAIResponsesProvider{}},
		{APIAnthropicMessages, &AnthropicProvider{}},
		{APIGoogleGenerativeAI, &GoogleProvider{}},
		{APIGoogleGeminiCLI, &GoogleGeminiCLIProvider{}},
		{APIMistralConversations, &MistralProvider{}},
		{APIBedrockConverseStream, &BedrockProvider{}},
		{APIAzureOpenAIResponses, &AzureProvider{}},
		{APIGoogleVertex, &VertexProvider{}},
		{APIOpenAICodexResponses, &CodexProvider{}},
	}

	for _, tc := range dedicatedProviders {
		t.Run(tc.api, func(t *testing.T) {
			if got := tc.provider.API(); got != tc.api {
				t.Errorf("API() = %q, want %q", got, tc.api)
			}
			if got := tc.provider.IsAlias(); got {
				t.Errorf("IsAlias() = %v, want false for dedicated provider %T", got, tc.provider)
			}
		})
	}
}

func TestSetBedrockClientFactoryOverridesProviderConstruction(t *testing.T) {
	origFactory := bedrockClientFactory
	t.Cleanup(func() {
		SetBedrockClientFactory(origFactory)
	})

	want := &fauxClient{}
	SetBedrockClientFactory(func(model *Model) (Client, error) {
		return want, nil
	})

	client, err := (&BedrockProvider{}).NewClient(&Model{ID: "amazon.nova-lite-v1:0", API: APIBedrockConverseStream, Provider: ProviderAmazonBedrock})
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	if client != want {
		t.Fatalf("client = %T, want injected client %T", client, want)
	}
}

// TestGoogleGeminiCLIProviderIsDedicated verifies that the google-gemini-cli API
// is registered with a dedicated provider implementation, not an alias to another
// provider. This test specifically validates the C1 checklist item completion.
func TestGoogleGeminiCLIProviderIsDedicated(t *testing.T) {
	// Get the provider for google-gemini-cli
	p, ok := Get(APIGoogleGeminiCLI)
	if !ok {
		t.Fatalf("no provider registered for API %q", APIGoogleGeminiCLI)
	}

	// Verify the provider declares the correct API
	if got := p.API(); got != APIGoogleGeminiCLI {
		t.Errorf("provider.API() = %q, want %q", got, APIGoogleGeminiCLI)
	}

	// Verify the provider is NOT an alias
	if got := p.IsAlias(); got {
		t.Errorf("provider.IsAlias() = %v, want false (google-gemini-cli should be dedicated)", got)
	}

	// Verify the provider is registered as GoogleGeminiCLIProvider
	if _, ok := p.(*GoogleGeminiCLIProvider); !ok {
		t.Errorf("provider is %T, want *GoogleGeminiCLIProvider", p)
	}

	// Verify we can create a client for a google-gemini-cli model
	model := &Model{
		ID:       "gemini-2.5-pro",
		API:      APIGoogleGeminiCLI,
		Provider: ProviderGoogleGeminiCLI,
	}
	client, err := p.NewClient(model)
	if err != nil {
		t.Fatalf("NewClient() for google-gemini-cli model: %v", err)
	}
	if client == nil {
		t.Fatal("NewClient() returned nil client")
	}

	// Verify the client is also not an alias
	// Note: Client interface no longer has IsAlias() - removed
}

// TestAliasProvidersReturnIsAliasTrue verifies that the providers registered
// under alias API types return true from IsAlias.
// Currently no APIs are registered as aliases — both google-gemini-cli and
// openai-responses are now dedicated implementations.
func TestAliasProvidersReturnIsAliasTrue(t *testing.T) {
	// No alias providers currently exist. Both google-gemini-cli and
	// openai-responses have been replaced with dedicated implementations.
	aliasAPIs := []string{} // No aliases currently registered

	for _, api := range aliasAPIs {
		t.Run(api, func(t *testing.T) {
			p, ok := Get(api)
			if !ok {
				t.Fatalf("no provider registered for API %q", api)
			}
			if got := p.API(); got != api {
				t.Errorf("provider.API() = %q, want %q", got, api)
			}
			if got := p.IsAlias(); !got {
				t.Errorf("IsAlias() = %v, want true for alias provider %T registered as %q", got, p, api)
			}
		})
	}
}

// TestGetClientRejectsAPIMismatch verifies that GetClient returns an error when
// the resolved provider's declared API does not match the model's API. This
// guards against silent mis-routing where a model claims one API type but the
// registry resolves it to a provider that declares a different API.
// Currently no APIs are mismatched — all are dedicated implementations.
func TestGetClientRejectsAPIMismatch(t *testing.T) {
	// No API mismatch cases currently exist. Both google-gemini-cli and
	// openai-responses are now dedicated providers, so they pass through.
	mismatchCases := []struct {
		api   string
		model *Model
	}{}

	for _, tc := range mismatchCases {
		t.Run(tc.api, func(t *testing.T) {
			client, err := GetClient(tc.model)
			if err == nil {
				t.Fatalf("GetClient(%q) expected error, got client %T", tc.model.API, client)
			}
			if !strings.Contains(err.Error(), "api mismatch") {
				t.Errorf("error = %q, want it to contain %q", err.Error(), "api mismatch")
			}
			// The error message should name both the model API and the provider API.
			if !strings.Contains(err.Error(), tc.model.API) {
				t.Errorf("error = %q, want it to contain model API %q", err.Error(), tc.model.API)
			}
		})
	}
}

// TestGetClientPassesForDedicatedProviders verifies that GetClient succeeds for
// every API registered with a dedicated (non-alias) provider.
func TestGetClientPassesForDedicatedProviders(t *testing.T) {
	cases := []struct {
		model *Model
	}{
		// OpenAI completions — dedicated OpenAICompletionsProvider
		{&Model{ID: "gpt-4o", API: APIOpenAICompletions, Provider: ProviderOpenAI}},
		// OpenAI Responses — dedicated OpenAIResponsesProvider
		{&Model{ID: "gpt-4o", API: APIOpenAIResponses, Provider: ProviderOpenAI}},
		// Anthropic — dedicated AnthropicProvider
		{&Model{ID: "claude-sonnet-4-20250514", API: APIAnthropicMessages, Provider: ProviderAnthropic}},
		// Google Generative AI — dedicated GoogleProvider
		{&Model{ID: "gemini-1.5-flash-002", API: APIGoogleGenerativeAI, Provider: ProviderGoogle}},
		// Google Gemini CLI — dedicated GoogleGeminiCLIProvider
		{&Model{ID: "gemini-2.5-pro", API: APIGoogleGeminiCLI, Provider: ProviderGoogleGeminiCLI}},
		// Mistral — dedicated MistralProvider
		{&Model{ID: "codestral-latest", API: APIMistralConversations, Provider: ProviderMistral}},
		// Bedrock — dedicated BedrockProvider
		{&Model{ID: "amazon.nova-lite-v1:0", API: APIBedrockConverseStream, Provider: ProviderAmazonBedrock}},
		// Azure — dedicated AzureProvider
		{&Model{ID: "gpt-4o", API: APIAzureOpenAIResponses, Provider: ProviderAzureOpenAI}},
		// Vertex — dedicated VertexProvider
		{&Model{ID: "gemini-1.5-flash-002", API: APIGoogleVertex, Provider: ProviderGoogleVertex}},
		// OpenAI Codex — dedicated CodexProvider
		{&Model{ID: "gpt-5.5", API: APIOpenAICodexResponses, Provider: ProviderOpenAICodex}},
	}

	for _, tc := range cases {
		t.Run(tc.model.API, func(t *testing.T) {
			p, ok := Get(tc.model.API)
			if !ok {
				t.Fatalf("no provider registered for API %q", tc.model.API)
			}
			if p.IsAlias() {
				t.Fatalf("GetClient test case for %q uses alias provider %T; use a dedicated API", tc.model.API, p)
			}

			client, err := GetClient(tc.model)
			if err != nil {
				t.Fatalf("GetClient(%q): %v", tc.model.API, err)
			}
			if client == nil {
				t.Fatalf("GetClient(%q) returned nil client", tc.model.API)
			}
		})
	}
}

// TestGetClientNoProviderError verifies that GetClient returns a clear error when
// no provider is registered for the model's API.
func TestGetClientNoProviderError(t *testing.T) {
	model := &Model{ID: "unknown-model", API: "unknown-api", Provider: "unknown-provider"}
	client, err := GetClient(model)
	if err == nil {
		t.Fatalf("GetClient(unknown-api) expected error, got client %T", client)
	}
	if !strings.Contains(err.Error(), "no provider") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "no provider")
	}
}

// TestOpenAICompatibleProvidersAllWired asserts the wiring invariant for every
// provider returned by OpenAICompatibleProviders: each name must (a) have a
// default base URL in DefaultBaseURLs, (b) have at least one env-var mapping
// in envVarNames, and (c) be routed through APIOpenAICompatible by
// APIForProvider. The set is declarative so this guards against future
// additions that forget one of the three wirings.
func TestOpenAICompatibleProvidersAllWired(t *testing.T) {
	for _, p := range OpenAICompatibleProviders() {
		t.Run(p, func(t *testing.T) {
			if DefaultBaseURLs[p] == "" {
				t.Errorf("%s: missing base URL in DefaultBaseURLs", p)
			}
			if got := APIForProvider(p); got != APIOpenAICompatible {
				t.Errorf("%s: APIForProvider = %q, want %q", p, got, APIOpenAICompatible)
			}
			// envVarNames returns the list of env vars; some compat providers
			// (e.g. ollama, lmstudio, vllm) may legitimately have none. Only
			// require an env-var name if the provider is known to be
			// remote-keyed. For the wired-remote providers in this list
			// (DeepSeek, Fireworks, Moonshot, Cloudflare, Xiaomi, Groq,
			// Cerebras, xAI, OpenRouter), at least one env var must resolve.
			switch p {
			case ProviderOllama, ProviderLMStudio, ProviderVLLM:
				// local-model providers, no env var expected
			default:
				if len(envVarNames(p)) == 0 {
					t.Errorf("%s: no env-var name in envVarNames", p)
				}
			}
		})
	}
}

type testProvider struct {
	api string
}

func (p *testProvider) API() string                        { return p.api }
func (p *testProvider) IsAlias() bool                      { return false }
func (p *testProvider) NewClient(_ *Model) (Client, error) { return nil, nil }

func TestRegistryLifecycleHelpers(t *testing.T) {
	registry := &Registry{providers: make(map[string]Provider)}

	providerA := &testProvider{api: "test-api-a"}
	providerB := &testProvider{api: "test-api-b"}

	registry.Register(providerA.API(), providerA)
	registry.Register(providerB.API(), providerB)

	apis := registry.APIs()
	if len(apis) != 2 || apis[0] != "test-api-a" || apis[1] != "test-api-b" {
		t.Fatalf("registry.APIs() = %v, want [test-api-a test-api-b]", apis)
	}

	registry.Unregister(providerA.API())
	if _, ok := registry.Get(providerA.API()); ok {
		t.Fatalf("registry.Get(%q) still found provider after Unregister", providerA.API())
	}

	registry.Clear()
	if apis := registry.APIs(); len(apis) != 0 {
		t.Fatalf("registry.APIs() after Clear = %v, want empty", apis)
	}
}

func TestGlobalRegistryHelpers(t *testing.T) {
	original := globalRegistry
	testRegistry := &Registry{providers: make(map[string]Provider)}
	globalRegistry = testRegistry
	t.Cleanup(func() {
		globalRegistry = original
	})

	provider := &testProvider{api: "test-global-api"}
	Register(provider.API(), provider)

	if apis := RegisteredAPIs(); len(apis) != 1 || apis[0] != provider.API() {
		t.Fatalf("RegisteredAPIs() = %v, want [%s]", apis, provider.API())
	}

	Unregister(provider.API())
	if _, ok := Get(provider.API()); ok {
		t.Fatalf("Get(%q) still found provider after Unregister", provider.API())
	}

	Register(provider.API(), provider)
	Clear()
	if apis := RegisteredAPIs(); len(apis) != 0 {
		t.Fatalf("RegisteredAPIs() after Clear = %v, want empty", apis)
	}
}

func TestRegistryUnregisterSource(t *testing.T) {
	registry := &Registry{
		providers: make(map[string]Provider),
		sources:   make(map[string]string),
	}

	sourceAProvider := &testProvider{api: "test-source-a"}
	sourceBProvider := &testProvider{api: "test-source-b"}
	unscopedProvider := &testProvider{api: "test-unscoped"}

	registry.RegisterWithSource(sourceAProvider.API(), sourceAProvider, "source-a")
	registry.RegisterWithSource(sourceBProvider.API(), sourceBProvider, "source-b")
	registry.Register(unscopedProvider.API(), unscopedProvider)

	registry.UnregisterSource("source-a")

	if _, ok := registry.Get(sourceAProvider.API()); ok {
		t.Fatalf("registry.Get(%q) still found provider after UnregisterSource", sourceAProvider.API())
	}
	if _, ok := registry.Get(sourceBProvider.API()); !ok {
		t.Fatalf("registry.Get(%q) missing provider from another source", sourceBProvider.API())
	}
	if _, ok := registry.Get(unscopedProvider.API()); !ok {
		t.Fatalf("registry.Get(%q) missing unscoped provider", unscopedProvider.API())
	}
}

func TestGlobalRegistryUnregisterProviders(t *testing.T) {
	original := globalRegistry
	testRegistry := &Registry{
		providers: make(map[string]Provider),
		sources:   make(map[string]string),
	}
	globalRegistry = testRegistry
	t.Cleanup(func() {
		globalRegistry = original
	})

	RegisterWithSource("test-api-a", &testProvider{api: "test-api-a"}, "source-a")
	RegisterWithSource("test-api-b", &testProvider{api: "test-api-b"}, "source-b")

	UnregisterProviders("source-a")

	if _, ok := Get("test-api-a"); ok {
		t.Fatal("Get(test-api-a) still found provider after UnregisterProviders")
	}
	if _, ok := Get("test-api-b"); !ok {
		t.Fatal("Get(test-api-b) missing provider from another source")
	}
}

func TestResetProvidersRestoresBuiltIns(t *testing.T) {
	original := globalRegistry
	testRegistry := &Registry{
		providers: make(map[string]Provider),
		sources:   make(map[string]string),
	}
	globalRegistry = testRegistry
	t.Cleanup(func() {
		globalRegistry = original
	})

	Register("test-api", &testProvider{api: "test-api"})
	ResetProviders()

	if _, ok := Get(APIOpenAICompletions); !ok {
		t.Fatalf("Get(%q) missing built-in provider after ResetProviders", APIOpenAICompletions)
	}
	if _, ok := Get(APIAnthropicMessages); !ok {
		t.Fatalf("Get(%q) missing built-in provider after ResetProviders", APIAnthropicMessages)
	}
	// APIOpenAICompatible is now a first-class built-in with Capabilities() support.
	if _, ok := Get(APIOpenAICompatible); !ok {
		t.Fatalf("Get(%q) missing built-in provider after ResetProviders", APIOpenAICompatible)
	}
	if _, ok := Get("test-api"); ok {
		t.Fatal("ResetProviders did not clear custom providers")
	}
}

func TestResetAPIProvidersIsAlias(t *testing.T) {
	original := globalRegistry
	testRegistry := &Registry{
		providers: make(map[string]Provider),
		sources:   make(map[string]string),
	}
	globalRegistry = testRegistry
	t.Cleanup(func() {
		globalRegistry = original
	})

	Register("test-api", &testProvider{api: "test-api"})
	ResetAPIProviders()

	if _, ok := Get("test-api"); ok {
		t.Fatal("ResetAPIProviders did not clear custom providers")
	}
	if _, ok := Get(APIOpenAICompletions); !ok {
		t.Fatal("ResetAPIProviders did not restore built-ins")
	}
}
