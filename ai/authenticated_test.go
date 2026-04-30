package ai

import (
	"context"
	"testing"
)

type fakeCredentialRefLister struct {
	refs []CredentialRef
	err  error
}

func (f fakeCredentialRefLister) List(context.Context) ([]CredentialRef, error) {
	return f.refs, f.err
}

func TestAuthenticatedModelsUsesCredentialRefsWithoutRefreshing(t *testing.T) {
	models := AuthenticatedModels(context.Background(), fakeCredentialRefLister{refs: []CredentialRef{
		{Provider: ProviderAnthropic, Type: CredentialTypeAPIKey},
	}}, ProviderAnthropic)
	if len(models) < 2 {
		t.Fatalf("AuthenticatedModels(anthropic) returned %d models, want multiple available models", len(models))
	}
	for _, model := range models {
		if model.Provider != ProviderAnthropic {
			t.Fatalf("model provider = %q, want %q", model.Provider, ProviderAnthropic)
		}
	}
}

func TestAuthenticatedModelsSkipsReauthAndNoneCredentials(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	models := AuthenticatedModels(context.Background(), fakeCredentialRefLister{refs: []CredentialRef{
		{Provider: ProviderMistral, Type: CredentialTypeNone},
		{Provider: ProviderMistral, Type: CredentialTypeOAuth, NeedsReauth: true},
	}}, ProviderMistral)
	if len(models) != 0 {
		t.Fatalf("AuthenticatedModels(mistral) returned %d models for unauthenticated refs, want 0", len(models))
	}
}

func TestAuthenticatedModelsIncludesEnvironmentAuth(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	models := AuthenticatedModels(context.Background(), nil, ProviderOpenAI)
	if len(models) < 2 {
		t.Fatalf("AuthenticatedModels(openai env) returned %d models, want multiple available models", len(models))
	}
	for _, model := range models {
		if model.Provider != ProviderOpenAI {
			t.Fatalf("model provider = %q, want %q", model.Provider, ProviderOpenAI)
		}
	}
}
