package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAnthropicLoginMethodsAcceptPastedCodes(t *testing.T) {
	exchanged := make(chan map[string]string, 1)
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("token request %s: %v", r.Header.Get("Content-Type"), err)
		}
		exchanged <- body
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "refresh_token": "refresh", "expires_in": 3600}); err != nil {
			t.Error(err)
		}
	}))
	defer tokens.Close()
	provider := &anthropicProvider{authURL: "https://claude.test/authorize", tokenURL: tokens.URL}

	method := "browser"
	login := func(input func(state string) string) (*Credentials, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		var state string
		return provider.Login(ctx, &DefaultLoginCallbacks{
			FnPrompt:   func(context.Context, string, string) (string, error) { return method, nil },
			FnProgress: func(string) {},
			FnAuth: func(rawURL, _ string) {
				parsed, err := url.Parse(rawURL)
				if err != nil {
					t.Error(err)
				}
				state = parsed.Query().Get("state")
			},
			FnManualCodeInput: func(context.Context) (string, error) { return input(state), nil },
		})
	}

	creds, err := login(func(state string) string { return "pasted#" + state })
	if err != nil || creds.Access != "access" {
		t.Fatalf("login = %+v, %v", creds, err)
	}
	if got := <-exchanged; got["code"] != "pasted" || got["state"] != got["code_verifier"] || !strings.HasPrefix(got["redirect_uri"], "http://localhost:") {
		t.Fatalf("exchange = %v", got)
	}
	method = "copy-code"
	if _, err := login(func(state string) string { return "copied#" + state }); err != nil {
		t.Fatal(err)
	}
	if got := <-exchanged; got["code"] != "copied" || got["redirect_uri"] != anthropicCopyCodeRedirectURI {
		t.Fatalf("copy-code exchange = %v", got)
	}
	method = "telepathy"
	if _, err := login(func(string) string { return "x" }); err == nil || !strings.Contains(err.Error(), "unknown Anthropic login method") {
		t.Fatalf("unknown method err = %v", err)
	}
	method = "browser"
	if _, err := login(func(string) string { return "pasted#wrong" }); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("mismatched state err = %v", err)
	}
	if _, err := login(func(string) string { return " " }); err == nil || !strings.Contains(err.Error(), "missing authorization code") {
		t.Fatalf("blank input err = %v", err)
	}
	if len(exchanged) != 0 {
		t.Fatal("rejected input reached the token endpoint")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = provider.Login(ctx, &DefaultLoginCallbacks{FnPrompt: func(context.Context, string, string) (string, error) { return "", ErrPromptUnavailable }, FnAuth: func(string, string) {}, FnManualCodeInput: func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled login err = %v", err)
	}
}
