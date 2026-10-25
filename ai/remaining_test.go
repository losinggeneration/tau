package ai

import (
	"context"
	"errors"
	"testing"
)

func TestModelAwarePayloadAndResponseCallbacks(t *testing.T) {
	model := &Model{ID: "m", Provider: "p", API: "a"}
	payloadSeen := false
	responseSeen := false
	opts := applyOptions([]Option{
		WithPayloadCallbackForModel(func(payload any, got *Model) any {
			payloadSeen = true
			if got != model {
				t.Fatalf("payload callback model = %#v, want %#v", got, model)
			}
			return map[string]any{"replaced": true}
		}),
		WithResponseCallbackForModel(func(response ProviderResponse, got *Model) {
			responseSeen = true
			if got != model {
				t.Fatalf("response callback model = %#v, want %#v", got, model)
			}
			if response.Status != 204 {
				t.Fatalf("response status = %d, want 204", response.Status)
			}
		}),
	})

	next := invokePayloadCallback(opts, model, map[string]any{"original": true})
	if !payloadSeen || next == nil || next.(map[string]any)["replaced"] != true {
		t.Fatalf("model-aware payload callback not invoked/replaced: seen=%v next=%#v", payloadSeen, next)
	}
	invokeResponseCallback(opts, model, ProviderResponse{Status: 204})
	if !responseSeen {
		t.Fatal("model-aware response callback not invoked")
	}
}

func TestStringEnumSchemaParity(t *testing.T) {
	schema := StringEnum([]string{"add", "subtract"}, map[string]any{
		"description": "operation",
		"default":     "add",
	})
	if schema["type"] != "string" || schema["description"] != "operation" || schema["default"] != "add" {
		t.Fatalf("StringEnum schema metadata = %#v", schema)
	}
	enum := schema["enum"].([]any)
	if len(enum) != 2 || enum[0] != "add" || enum[1] != "subtract" {
		t.Fatalf("StringEnum enum = %#v", enum)
	}
}

func TestCopilotDynamicHeadersParamParityAndEmptyMessages(t *testing.T) {
	headers := BuildCopilotDynamicHeadersWithParams(CopilotDynamicHeaderParams{
		Messages:  nil,
		HasImages: true,
	})
	if headers["X-Initiator"] != "user" {
		t.Fatalf("X-Initiator = %q, want user", headers["X-Initiator"])
	}
	if headers["Openai-Intent"] != "conversation-edits" {
		t.Fatalf("Openai-Intent = %q", headers["Openai-Intent"])
	}
	if headers["Copilot-Vision-Request"] != "true" {
		t.Fatalf("Copilot-Vision-Request = %q, want true", headers["Copilot-Vision-Request"])
	}
}

func TestPiEnvAPIKeyMapParity(t *testing.T) {
	clearEnvForAPIKeyParity(t)
	tests := []struct {
		provider string
		env      string
	}{
		{ProviderOpenAI, "OPENAI_API_KEY"},
		{ProviderAzureOpenAI, "AZURE_OPENAI_API_KEY"},
		{ProviderGoogle, "GEMINI_API_KEY"},
		{ProviderGroq, "GROQ_API_KEY"},
		{ProviderCerebras, "CEREBRAS_API_KEY"},
		{ProviderxAI, "XAI_API_KEY"},
		{ProviderOpenRouter, "OPENROUTER_API_KEY"},
		{ProviderVercelGateway, "AI_GATEWAY_API_KEY"},
		{ProviderZAI, "ZAI_API_KEY"},
		{ProviderMistral, "MISTRAL_API_KEY"},
		{ProviderMiniMax, "MINIMAX_API_KEY"},
		{ProviderMiniMaxCN, "MINIMAX_CN_API_KEY"},
		{ProviderHuggingFace, "HF_TOKEN"},
		{ProviderOpenCode, "OPENCODE_API_KEY"},
		{ProviderOpenCodeGo, "OPENCODE_API_KEY"},
		{ProviderKimiCoding, "KIMI_API_KEY"},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			clearEnvForAPIKeyParity(t)
			t.Setenv(tt.env, "value-for-"+tt.provider)
			if got := GetEnvAPIKey(tt.provider); got != "value-for-"+tt.provider {
				t.Fatalf("GetEnvAPIKey(%q) = %q, want env %s", tt.provider, got, tt.env)
			}
		})
	}
}

func TestStreamConvertsClientErrorToTerminalEvent(t *testing.T) {
	original := globalRegistry
	globalRegistry = &Registry{providers: make(map[string]Provider), sources: make(map[string]string)}
	Register("error-api", &mockProviderWithClient{api: "error-api", client: &mockClient{streamErr: errors.New("boom")}})
	t.Cleanup(func() { globalRegistry = original })

	called := false
	handler := func(_ context.Context, event StreamEvent) {
		called = true
		_ = event
	}

	err := Stream(context.Background(), &Model{ID: "m", Provider: "p", API: "error-api"}, &Context{}, handler)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("Stream() err = %v, want boom", err)
	}
	if called {
		t.Fatal("handler should not be called when Stream returns an error before emitting events")
	}
}

type mockProviderWithClient struct {
	api    string
	client Client
}

func (p *mockProviderWithClient) API() string                        { return p.api }
func (p *mockProviderWithClient) IsAlias() bool                      { return false }
func (p *mockProviderWithClient) NewClient(_ *Model) (Client, error) { return p.client, nil }

func clearEnvForAPIKeyParity(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OPENAI_API_KEY", "AZURE_OPENAI_API_KEY", "GEMINI_API_KEY", "GROQ_API_KEY",
		"CEREBRAS_API_KEY", "XAI_API_KEY", "OPENROUTER_API_KEY", "AI_GATEWAY_API_KEY",
		"ZAI_API_KEY", "MISTRAL_API_KEY", "MINIMAX_API_KEY", "MINIMAX_CN_API_KEY",
		"HF_TOKEN", "HUGGINGFACE_HUB_TOKEN", "OPENCODE_API_KEY", "KIMI_API_KEY",
		"ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN",
		"GOOGLE_CLOUD_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_PROFILE", "AWS_BEARER_TOKEN_BEDROCK",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_WEB_IDENTITY_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
}
