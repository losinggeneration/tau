package ai

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func codexTestToken() string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct_123"}}`))
	return "header." + payload + ".signature"
}

func TestFetchCodexModelIDsReturnsVisibleModels(t *testing.T) {
	token := codexTestToken()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/models" {
			t.Errorf("path = %q, want /codex/models", r.URL.Path)
		}
		if got := r.URL.Query().Get("client_version"); got != codexModelsClientVersion {
			t.Errorf("client_version = %q, want %q", got, codexModelsClientVersion)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		if got := r.Header.Get("chatgpt-account-id"); got != "acct_123" {
			t.Errorf("chatgpt-account-id = %q, want acct_123", got)
		}
		_, _ = io.WriteString(w, `{"models":[
			{"slug":"gpt-6-sol","visibility":"list"},
			{"slug":"gpt-reserve","visibility":"hide"},
			{"slug":"gpt-5.6-luna","visibility":"list"},
			{"slug":"","visibility":"list"}
		]}`)
	}))
	defer server.Close()

	ids, err := FetchCodexModelIDs(context.Background(), server.Client(), server.URL, token)
	if err != nil {
		t.Fatalf("FetchCodexModelIDs: %v", err)
	}
	if want := []string{"gpt-5.6-luna", "gpt-6-sol"}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestFetchCodexModelIDsReportsStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `token expired`)
	}))
	defer server.Close()

	_, err := FetchCodexModelIDs(context.Background(), server.Client(), server.URL, codexTestToken())
	if err == nil {
		t.Fatal("FetchCodexModelIDs: want error for 401")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "token expired") {
		t.Fatalf("err = %v, want status and body", err)
	}
}

func TestFetchCodexModelIDsRejectsInvalidToken(t *testing.T) {
	if _, err := FetchCodexModelIDs(context.Background(), http.DefaultClient, "", "not-a-jwt"); err == nil {
		t.Fatal("FetchCodexModelIDs: want error for invalid token")
	}
}

func TestResolveCodexModelsURL(t *testing.T) {
	for base, want := range map[string]string{
		"":                                     defaultCodexBaseURL + "/codex/models",
		"https://example.test/":                "https://example.test/codex/models",
		"https://example.test/codex":           "https://example.test/codex/models",
		"https://example.test/codex/responses": "https://example.test/codex/models",
	} {
		if got := resolveCodexModelsURL(base); got != want {
			t.Errorf("resolveCodexModelsURL(%q) = %q, want %q", base, got, want)
		}
	}
}
