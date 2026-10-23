package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelRegistryResolvesCustomMetadataWithoutGlobalMutation(t *testing.T) {
	var file ModelRegistryFile
	err := json.Unmarshal([]byte(`{"providers":{"openai":{"baseUrl":"https://example.com/v1","api":"openai-responses","models":[{"id":"local-test","reasoning":true,"thinkingLevelMap":{"max":"max"},"inputLimits":{"images":{"maxPerRequest":2}},"samplingParams":{"temperature":0.2},"cost":{"tiers":[{"inputTokensAbove":100,"input":2}]},"contextWindow":1000,"maxTokens":100}]}}}`), &file)
	if err != nil {
		t.Fatal(err)
	}
	registry := &ModelRegistry{providers: file.Providers}
	model, err := registry.ResolveModel("openai/local-test")
	if err != nil {
		t.Fatal(err)
	}
	if model.BaseURL != "https://example.com/v1" || model.ThinkingLevelMap["max"] == nil || model.InputLimits == nil || model.InputLimits.Images.MaxPerRequest != 2 || model.SamplingParams["temperature"] != 0.2 || len(model.Cost.Tiers) != 1 {
		t.Fatalf("metadata lost: %+v", model)
	}
	if _, ok := Models()["openai/local-test"]; ok {
		t.Fatal("custom model polluted global catalog")
	}
}

func TestModelRegistryKeepsSameIDOperationsIndependent(t *testing.T) {
	registry := NewModelRegistry()
	registry.providers["operation-local-test"] = ModelRegistryProvider{API: APIOpenAIResponses, Models: []ModelRegistryModel{{Type: "chat", ID: "shared", Name: "chat"}, {Type: "classifier", ID: "shared", Name: "classifier", API: APIOpenAIDecisions}}}
	catalog, err := registry.Models()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, model := range catalog {
		if model.Provider == "operation-local-test" {
			found[model.Type] = model.Name
		}
	}
	if found["chat"] != "chat" || found["classifier"] != "classifier" {
		t.Fatalf("operation collision: %v", found)
	}
}

func TestModelRegistryAppliesProviderCompatibility(t *testing.T) {
	registry := NewModelRegistry()
	registry.providers[ProviderAnthropic] = ModelRegistryProvider{Compat: json.RawMessage(`{"supportsStrictTools":true}`), API: APIAnthropicMessages, Models: []ModelRegistryModel{{ID: "custom", ContextWindow: 1000}}}
	custom, err := registry.ResolveModel("anthropic/custom")
	if err != nil {
		t.Fatal(err)
	}
	if custom.Compat == nil || !custom.Compat.SupportsStrictTools {
		t.Fatalf("custom compat=%+v", custom.Compat)
	}
	builtin, err := GetModel(ProviderAnthropic, "claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	wasSupported := builtin.Compat != nil && builtin.Compat.SupportsStrictTools
	local, err := registry.ResolveModel("anthropic/claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	if local.Compat == nil || !local.Compat.SupportsStrictTools {
		t.Fatalf("builtin local compat=%+v", local.Compat)
	}
	if (builtin.Compat != nil && builtin.Compat.SupportsStrictTools) != wasSupported {
		t.Fatal("shared model mutated")
	}
	registry.providers[ProviderAnthropic] = ModelRegistryProvider{Compat: json.RawMessage(`{`)}
	if _, err := registry.ResolveModel("anthropic/claude-sonnet-4-6"); err == nil {
		t.Fatal("invalid compat ignored")
	}
}

func TestLoadModelRegistryMigratesAzureProviderWithoutChangingAPIs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	for _, data := range []string{
		`{"providers":{"azure-openai-responses":{"baseUrl":"https://legacy.example.com","api":"azure-openai-responses","models":[{"id":"local"}]}}}`,
		`{"providers":{"azure-openai-responses":{"baseUrl":"https://legacy.example.com"},"azure":{"baseUrl":"https://canonical.example.com","api":"azure-openai-responses","models":[{"id":"local"}]}}}`,
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		registry, err := LoadModelRegistry(path)
		if err != nil {
			t.Fatal(err)
		}
		wantURL := "https://legacy.example.com"
		if strings.Contains(data, "canonical") {
			wantURL = "https://canonical.example.com"
		}
		for _, provider := range []string{"azure", "azure-openai-responses"} {
			model, err := registry.ResolveModel(provider + "/local")
			if err != nil || model.Provider != ProviderAzureOpenAI || model.API != APIAzureOpenAIResponses || model.BaseURL != wantURL {
				t.Fatalf("%s: model = %+v, err = %v", provider, model, err)
			}
		}
		if err := SaveModelRegistry(path, registry); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var saved ModelRegistryFile
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if len(saved.Providers) != 1 || saved.Providers[ProviderAzureOpenAI].API != APIAzureOpenAIResponses {
			t.Fatalf("saved providers = %+v", saved.Providers)
		}
	}
}

func TestModelRegistryResolvesRequestCredentialsForEachOperation(t *testing.T) {
	t.Setenv("TAU_CONFIG_TEST_KEY", "configured-secret")
	t.Setenv("TAU_CONFIG_TEST_HEADER", "header-secret")
	for _, operation := range []string{"chat", "classifier", "image"} {
		t.Run(operation, func(t *testing.T) {
			var requests int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Header.Get("Authorization") != "Bearer configured-secret" || r.Header.Get("X-Configured") != "prefix-header-secret" || r.Header.Get("X-Override") != "caller" {
					t.Errorf("configured authentication or headers not applied")
				}
				w.Header().Set("Content-Type", "application/json")
				if operation == "chat" {
					w.Header().Set("Content-Type", "text/event-stream")
					if _, err := io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"); err != nil {
						t.Error(err)
					}
				} else if _, err := io.WriteString(w, `{"answers":[{"name":"ok","type":"predicate","probability":0.9}],"choices":[{"message":{"content":"{}"}}]}`); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			api := APIOpenAICompatible
			if operation == "classifier" {
				api = APIOpenAIDecisions
			}
			if operation == "image" {
				api = APIOpenRouterImages
			}
			registry := NewModelRegistry()
			registry.providers["config-test"] = ModelRegistryProvider{
				API: api, BaseURL: server.URL, APIKey: "$TAU_CONFIG_TEST_KEY", AuthHeader: true,
				Headers: map[string]string{"X-Configured": "prefix-${TAU_CONFIG_TEST_HEADER}", "X-Override": "provider"},
				Models:  []ModelRegistryModel{{ID: "local", Type: operation}},
			}
			model, err := registry.ResolveOperationModel(operation, "config-test/local")
			if err != nil {
				t.Fatal(err)
			}
			options := []Option{WithHeaders(map[string]string{"X-Override": "caller"})}
			switch operation {
			case "chat":
				client, createErr := (&OpenAICompatProvider{}).NewClient(model)
				if createErr != nil {
					t.Fatal(createErr)
				}
				_, err = client.Complete(t.Context(), model, &Context{}, options...)
			case "classifier":
				_, err = Classify(t.Context(), model, ClassifierContext{Questions: map[string]ClassifierQuestion{"ok": {Type: "bool", Criteria: map[string]string{}}}}, options...)
			case "image":
				_, err = GenerateImages(t.Context(), model, nil, options...)
			}
			if err != nil {
				t.Fatal(err)
			}
			if requests != 1 {
				t.Fatalf("requests=%d", requests)
			}
			data, err := json.Marshal(model)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "configured-secret") || strings.Contains(string(data), "header-secret") {
				t.Fatal("resolved secrets leaked into model metadata")
			}
		})
	}
}

func TestModelRegistryRequestConfigIsLazyAndOperationLocal(t *testing.T) {
	registry := NewModelRegistry()
	RegisterModel(&Model{ID: "config-lazy-unique", Provider: ProviderOpenAI, API: APIOpenAIResponses})
	t.Cleanup(func() { delete(models, "openai/config-lazy-unique") })
	var calls int
	registry.values.execute = func(ctx context.Context, command string) (string, error) {
		calls++
		if command != "key-command" {
			t.Fatal("unexpected command")
		}
		return "command-secret", nil
	}
	registry.providers[ProviderOpenAI] = ModelRegistryProvider{
		APIKey: "!key-command", AuthHeader: true,
		Headers:        map[string]string{"x-configured": "provider"},
		ModelOverrides: map[string]ModelRegistryOverride{"config-lazy-unique": {Headers: map[string]string{"X-Configured": "model"}}},
	}
	for _, spec := range []string{"openai/config-lazy-unique", "config-lazy-unique"} {
		model, err := registry.ResolveModel(spec)
		if err != nil {
			t.Fatal(err)
		}
		if calls != 0 {
			t.Fatal("catalog resolution executed a credential command")
		}
		options, err := resolveRequestOptions(t.Context(), model, []Option{WithAPIKey("explicit"), WithHeaders(map[string]string{"x-configured": "caller", "authorization": "Caller auth"})})
		if err != nil {
			t.Fatal(err)
		}
		cfg := applyOptions(options)
		if cfg.apiKey != "explicit" || cfg.headers["Authorization"] != "Caller auth" || cfg.headers["X-Configured"] != "caller" || calls != 0 {
			t.Fatal("explicit request values did not override config")
		}
	}
	for range 2 {
		model, err := registry.ResolveModel("openai/config-lazy-unique")
		if err != nil {
			t.Fatal(err)
		}
		options, err := resolveRequestOptions(t.Context(), model, nil)
		if err != nil {
			t.Fatal(err)
		}
		cfg := applyOptions(options)
		if cfg.apiKey != "command-secret" || cfg.headers["Authorization"] != "Bearer command-secret" || cfg.headers["X-Configured"] != "model" {
			t.Fatal("model overrides or configured auth missing")
		}
	}
	if calls != 1 {
		t.Fatalf("credential command calls=%d", calls)
	}
	if _, err := registry.Models(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("catalog executed credentials")
	}
}

func TestModelRegistryRejectsMissingRequestValuesWithoutLeakingSecrets(t *testing.T) {
	t.Setenv("TAU_CONFIG_MISSING", "")
	registry := NewModelRegistry()
	for _, provider := range []ModelRegistryProvider{
		{APIKey: "literal-secret-${TAU_CONFIG_MISSING}"},
		{Headers: map[string]string{"X-Key": "literal-secret-${TAU_CONFIG_MISSING}"}},
		{AuthHeader: true},
	} {
		registry.providers["missing-config"] = provider
		provider.API = APIOpenAICompatible
		provider.Models = []ModelRegistryModel{{ID: "local"}}
		registry.providers["missing-config"] = provider
		model, err := registry.ResolveModel("missing-config/local")
		if err != nil {
			t.Fatal(err)
		}
		_, err = resolveRequestOptions(t.Context(), model, nil)
		if err == nil || strings.Contains(err.Error(), "literal-secret") {
			t.Fatalf("invalid config accepted or leaked: %v", err)
		}
	}
}

func TestModelRegistryConfiguredHeadersReachConcreteProviders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider Provider
		key      string
	}{
		{"anthropic", &AnthropicProvider{}, "key"},
		{"responses", &OpenAIResponsesProvider{}, "key"},
		{"completions", &OpenAICompletionsProvider{}, "key"},
		{"mistral", &MistralProvider{}, "key"},
		{"google", &GoogleProvider{}, "key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TAU_CONFIG_HEADER", "resolved")
			var requests int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if got := r.Header.Get("X-Configured"); got != "resolved" {
					t.Errorf("configured header=%q", got)
				}
				if got := r.Header.Get("X-Override"); got != "caller" {
					t.Errorf("caller header=%q", got)
				}
				http.Error(w, "intentional test rejection", http.StatusUnauthorized)
			}))
			defer server.Close()
			registry := NewModelRegistry()
			registry.providers[tc.name] = ModelRegistryProvider{API: tc.provider.API(), BaseURL: server.URL, APIKey: tc.key, Headers: map[string]string{"X-Configured": "$TAU_CONFIG_HEADER", "x-override": "provider"}, Models: []ModelRegistryModel{{ID: "local"}}}
			model, err := registry.ResolveModel(tc.name + "/local")
			if err != nil {
				t.Fatal(err)
			}
			client, err := tc.provider.NewClient(model)
			if err != nil {
				t.Fatal(err)
			}
			err = client.Stream(t.Context(), model, &Context{}, func(context.Context, StreamEvent) {}, WithHeaders(map[string]string{"X-Override": "caller"}))
			if requests != 1 {
				t.Fatalf("requests=%d err=%v", requests, err)
			}
		})
	}
}
