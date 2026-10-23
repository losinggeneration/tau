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

func openRouterTestKeyServer(t *testing.T, bodies chan<- map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		if body["code"] == "bad" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid code"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"key":"sk-or-` + body["code"] + `"}`))
	}))
}

func blockManualInput(ctx context.Context) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestOpenRouterLoginExchangesCallbackCodeForPermanentKey(t *testing.T) {
	t.Setenv("PI_OAUTH_CALLBACK_HOST", "127.0.0.1")
	bodies := make(chan map[string]string, 2)
	keys := openRouterTestKeyServer(t, bodies)
	defer keys.Close()
	provider := newOpenRouterProvider("https://openrouter.test/auth", keys.URL)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	callbacks := &DefaultLoginCallbacks{
		FnProgress:        func(string) {},
		FnManualCodeInput: blockManualInput,
		FnAuth: func(authURL, _ string) {
			parsed, err := url.Parse(authURL)
			if err != nil {
				t.Error(err)
				return
			}
			query := parsed.Query()
			callback := query.Get("callback_url")
			if parsed.Host != "openrouter.test" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" ||
				!strings.HasPrefix(callback, "http://127.0.0.1:") || !strings.Contains(callback, "/oauth/callback/") {
				t.Errorf("auth URL = %s", authURL)
			}
			go func() {
				stray, err := http.Get(callback[:strings.LastIndex(callback, "/")] + "/guess?code=x")
				if err != nil {
					t.Error(err)
					return
				}
				if err := stray.Body.Close(); err != nil {
					t.Error(err)
				}
				if stray.StatusCode != http.StatusNotFound {
					t.Errorf("stray callback status = %d, want 404", stray.StatusCode)
				}
				resp, err := http.Get(callback + "?code=abc")
				if err != nil {
					t.Error(err)
					return
				}
				if err := resp.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
		},
	}
	creds, err := provider.Login(ctx, callbacks)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Access != "sk-or-abc" || creds.Refresh != "" || creds.IsExpired(time.Hour) {
		t.Fatalf("credentials = %+v", creds)
	}
	body := <-bodies
	if body["code"] != "abc" || body["code_verifier"] == "" || body["code_challenge_method"] != "S256" {
		t.Fatalf("exchange body = %v", body)
	}
	refreshed, err := provider.RefreshToken(ctx, creds)
	if err != nil || refreshed.Access != creds.Access {
		t.Fatalf("refresh = %+v, %v", refreshed, err)
	}
	if got := provider.GetAPIKey(creds); got != "sk-or-abc" {
		t.Fatalf("api key = %q", got)
	}
}

func TestOpenRouterLoginAcceptsPastedRedirectOrCodeAndReportsFailures(t *testing.T) {
	bodies := make(chan map[string]string, 4)
	keys := openRouterTestKeyServer(t, bodies)
	defer keys.Close()
	provider := newOpenRouterProvider("https://openrouter.test/auth", keys.URL)

	login := func(input string) (*Credentials, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		return provider.Login(ctx, &DefaultLoginCallbacks{
			FnAuth:            func(string, string) {},
			FnProgress:        func(string) {},
			FnManualCodeInput: func(context.Context) (string, error) { return input, nil },
		})
	}

	for _, input := range []string{"http://127.0.0.1:1/oauth/callback/x?code=pasted", "code=pasted", "  pasted  "} {
		creds, err := login(input)
		if err != nil || creds.Access != "sk-or-pasted" {
			t.Fatalf("login(%q) = %+v, %v", input, creds, err)
		}
	}
	if _, err := login("   "); err == nil || !strings.Contains(err.Error(), "missing authorization code") {
		t.Fatalf("blank input err = %v", err)
	}
	if _, err := login("bad"); err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "invalid code") {
		t.Fatalf("rejected exchange err = %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := provider.Login(ctx, &DefaultLoginCallbacks{FnAuth: func(string, string) {}, FnProgress: func(string) {}, FnManualCodeInput: blockManualInput})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled login err = %v", err)
	}
}
