package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

func TestGetClientRejectsAPIMismatch(t *testing.T) {
	provider := &testProvider{api: "declared-routing-api"}
	const registeredAPI = "incorrect-routing-api"
	Register(registeredAPI, provider)
	t.Cleanup(func() { Unregister(registeredAPI) })
	client, err := GetClient(&Model{ID: "model", API: registeredAPI})
	require.Nil(t, client)
	require.ErrorContains(t, err, "api mismatch")
	require.ErrorContains(t, err, registeredAPI)
	require.ErrorContains(t, err, provider.API())
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

func TestTransportExpandsCloudflarePlaceholders(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
	}))
	defer server.Close()

	transport := NewTransport(server.URL+"/accounts/{CLOUDFLARE_ACCOUNT_ID}/gw/{CLOUDFLARE_GATEWAY_ID}", "", nil)
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	if _, err := transport.Do(t.Context(), http.MethodPost, "/run", "{}"); err == nil || !strings.Contains(err.Error(), "CLOUDFLARE_ACCOUNT_ID") {
		t.Fatalf("missing account id error = %v", err)
	}

	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "gw1")
	resp, err := transport.Do(t.Context(), http.MethodPost, "/run", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if path != "/accounts/acct/gw/gw1/run" {
		t.Fatalf("path = %q", path)
	}
}

func TestGetClientRejectsNonChatModels(t *testing.T) {
	for _, kind := range []string{"image", "classifier"} {
		if _, err := GetClient(&Model{ID: "m", Type: kind, API: APIOpenAICompletions, Provider: ProviderOpenAI}); err == nil || !strings.Contains(err.Error(), kind) {
			t.Fatalf("%s model streamed: %v", kind, err)
		}
	}
	if _, err := GetClient(&Model{ID: "m", Type: "chat", API: APIOpenAICompletions, Provider: ProviderOpenAI}); err != nil {
		t.Fatal(err)
	}
}

func testHTTPProviderHeaderTimeout(t *testing.T, client Client, model *Model, payload string) {
	t.Helper()
	for _, refresh := range []bool{false, true} {
		for _, phase := range []string{"headers", "disabled", "success body", "error body", "read failure", "cancel body"} {
			t.Run(fmt.Sprintf("%s/refresh=%v", phase, refresh), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				timeout := time.Millisecond
				if phase == "disabled" {
					timeout = 0
				}
				var body *codexTestBody
				readErr, closeErr := errors.New("read failed"), errors.New("close failed")
				previous := http.DefaultClient
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if phase == "headers" {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					if phase == "disabled" {
						require.NoError(t, sleepWithContext(req.Context(), 5*time.Millisecond))
					}
					body = &codexTestBody{Reader: strings.NewReader(payload), ctx: req.Context(), delay: 5 * time.Millisecond}
					status := http.StatusOK
					if phase == "error body" {
						status = http.StatusBadRequest
						body.Reader = strings.NewReader("rejected")
					}
					if phase == "read failure" {
						status = http.StatusBadRequest
						body.readErr, body.closeErr = readErr, closeErr
					}
					if phase == "cancel body" {
						status = http.StatusBadRequest
						body.delay, body.closeErr = time.Hour, closeErr
						cancel()
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
				})}
				t.Cleanup(func() { http.DefaultClient = previous })
				// Clients constructed before the test retain their own transport client.
				switch c := client.(type) {
				case *openAIClient:
					c.transport.httpClient = http.DefaultClient
				case *anthropicClient:
					c.transport.httpClient = http.DefaultClient
				}
				opts := []Option{WithAPIKey("test-key"), WithTimeout(timeout)}
				if refresh {
					opts = append(opts, WithTransportOption(TransportOption{Provider: model.Provider, RefreshCreds: func(context.Context, string) (string, error) {
						t.Fatal("unexpected credential refresh")
						return "", nil
					}}))
				}
				err := client.Stream(ctx, model, &Context{}, func(context.Context, StreamEvent) {}, opts...)
				switch phase {
				case "headers":
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.ErrorContains(t, err, "response headers timed out")
					require.NoError(t, ctx.Err())
					return
				case "error body":
					require.ErrorContains(t, err, "rejected")
					require.NoError(t, ctx.Err())
				case "read failure":
					require.ErrorIs(t, err, readErr)
					require.ErrorIs(t, err, closeErr)
				case "cancel body":
					require.ErrorIs(t, err, context.Canceled)
					require.ErrorIs(t, err, closeErr)
				default:
					require.NoError(t, err)
				}
				require.NotNil(t, body)
				require.True(t, body.closed)
				if timeout > 0 {
					require.ErrorIs(t, body.ctx.Err(), context.Canceled)
				}
			})
		}
	}
}

func TestHTTPTransportHeaderTimeoutRefresh(t *testing.T) {
	for _, phase := range []string{"success", "retry headers", "close failure", "refresh cancellation"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			var bodies []*codexTestBody
			var requests []*http.Request
			closeErr := errors.New("close unauthorized failed")
			calls, refreshes := 0, 0
			transport := NewTransportWithOptions("https://test.invalid", "old", nil, TransportOptions{
				Provider: "test", RefreshCreds: func(refreshCtx context.Context, provider string) (string, error) {
					refreshes++
					require.Equal(t, "test", provider)
					require.True(t, bodies[0].closed)
					require.ErrorIs(t, requests[0].Context().Err(), context.Canceled)
					require.NoError(t, refreshCtx.Err())
					if phase == "refresh cancellation" {
						cancel()
						return "", refreshCtx.Err()
					}
					return "new", nil
				},
			})
			transport.httpClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				requests = append(requests, req)
				require.NoError(t, req.Context().Err())
				require.Equal(t, "kept", req.Header.Get("X-Test"))
				data, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Equal(t, `{"request":true}`, string(data))
				if calls == 2 && phase == "retry headers" {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				body := &codexTestBody{Reader: strings.NewReader("ok"), ctx: req.Context(), delay: 5 * time.Millisecond}
				bodies = append(bodies, body)
				status := http.StatusOK
				if calls == 1 {
					status = http.StatusUnauthorized
					require.Equal(t, "Bearer old", req.Header.Get("Authorization"))
					if phase == "close failure" {
						body.closeErr = closeErr
					}
				} else {
					require.Equal(t, "Bearer new", req.Header.Get("Authorization"))
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
			})}
			model := &Model{Provider: "test"}
			transport = transport.withRequestOptions(model, applyOptions([]Option{WithAPIKey("old"), WithTimeout(time.Millisecond), WithHeaders(map[string]string{"X-Test": "kept"})}))
			resp, err := transport.Do(ctx, http.MethodPost, "/test", `{"request":true}`)
			switch phase {
			case "success":
				require.NoError(t, err)
				data, readErr := io.ReadAll(resp.Body)
				require.NoError(t, readErr)
				require.Equal(t, "ok", string(data))
				require.NoError(t, resp.Body.Close())
				require.ErrorIs(t, requests[1].Context().Err(), context.Canceled)
			case "retry headers":
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.ErrorContains(t, err, "response headers timed out")
				require.NoError(t, ctx.Err())
			case "close failure":
				require.ErrorIs(t, err, closeErr)
				require.Equal(t, 1, calls)
				require.Zero(t, refreshes)
			case "refresh cancellation":
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, 1, calls)
			}
			for _, body := range bodies {
				require.True(t, body.closed)
			}
		})
	}
}

func TestStreamingProvidersConsumeProviderRetries(t *testing.T) {
	t.Setenv("GOOGLE_ACCESS_TOKEN", "test-token")
	completions := "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	model := func(api, provider string) *Model {
		return &Model{ID: "test", API: api, Provider: provider, BaseURL: "https://test.invalid"}
	}
	for _, tc := range []struct {
		name         string
		model        *Model
		client       func(*Model) (Client, error)
		opts         []Option
		payload      string
		defaultCalls int
	}{
		{name: "openai completions", model: model(APIOpenAICompletions, ProviderOpenAI), client: func(m *Model) (Client, error) { return newOpenAIClient(m) }, payload: completions, defaultCalls: 2},
		{name: "openai compatible", model: model(APIOpenAICompatible, ProviderOpenAI), client: func(m *Model) (Client, error) { return &openAICompatClient{model: m}, nil }, payload: completions, defaultCalls: 2},
		{name: "anthropic", model: model(APIAnthropicMessages, ProviderAnthropic), client: func(m *Model) (Client, error) { return newAnthropicClient(m) }, payload: `{"content":[],"stop_reason":"end_turn"}`, defaultCalls: 1},
		{name: "openai responses", model: model(APIOpenAIResponses, ProviderOpenAI), client: func(*Model) (Client, error) { return &openAIResponsesClient{}, nil }, defaultCalls: 1},
		{name: "azure", model: model(APIAzureOpenAIResponses, ProviderAzureOpenAI), client: func(m *Model) (Client, error) { return &azureClient{model: m}, nil }, opts: []Option{WithAzureBaseURL("https://test.invalid")}, defaultCalls: 1},
		{name: "vertex", model: model(APIGoogleVertex, ProviderGoogleVertex), client: func(m *Model) (Client, error) { return &vertexClient{model: m}, nil }, opts: []Option{WithVertexProject("test"), WithVertexLocation("test")}, defaultCalls: 1},
	} {
		for _, retries := range []*int{nil, new(1)} {
			t.Run(fmt.Sprintf("%s/retries=%v", tc.name, retries != nil), func(t *testing.T) {
				calls := 0
				previous := http.DefaultClient
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After-Ms": {"0"}}, Body: io.NopCloser(strings.NewReader("busy"))}, nil
					}
					header := http.Header{"Content-Type": {"application/json"}}
					if tc.payload == completions {
						header.Set("Content-Type", "text/event-stream")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(tc.payload))}, nil
				})}
				t.Cleanup(func() { http.DefaultClient = previous })
				client, err := tc.client(tc.model)
				require.NoError(t, err)
				opts := append([]Option{WithAPIKey("key")}, tc.opts...)
				want := tc.defaultCalls
				if retries != nil {
					opts, want = append(opts, WithMaxRetries(*retries)), 2
				}
				if tc.payload == completions {
					withFastChatCompletionsRetries(t)
				}
				_, err = client.Complete(t.Context(), tc.model, &Context{}, opts...)
				require.Equal(t, want, calls)
				if want == 1 {
					require.ErrorContains(t, err, "503")
					return
				}
				if err != nil {
					require.NotContains(t, err.Error(), "503")
				}
				if tc.payload != "" {
					require.NoError(t, err)
				}
			})
		}
	}
}
