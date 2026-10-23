package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func radiusTestGateway(t *testing.T, forms chan<- url.Values) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch r.URL.Path {
		case "/v1/oauth":
			body = map[string]string{"authorizationEndpoint": "http://" + r.Host + "/authorize"}
		case "/v1/oauth/device":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			forms <- r.PostForm
			body = map[string]any{"device_code": "dev", "user_code": "ABCD", "verification_uri": "https://radius.example/device", "expires_in": 60}
		case "/v1/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			forms <- r.PostForm
			body = map[string]any{"access_token": "access-" + r.PostForm.Get("grant_type"), "refresh_token": "refresh", "expires_in": 3600, "token_type": "Bearer"}
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Error(err)
		}
	}))
}

func TestRadiusBrowserLoginAndRefresh(t *testing.T) {
	forms := make(chan url.Values, 4)
	server := radiusTestGateway(t, forms)
	defer server.Close()
	provider := NewRadiusProvider(server.URL + "/v1")

	callbacks := &DefaultLoginCallbacks{
		FnPrompt:   func(context.Context, string, string) (string, error) { return "", nil },
		FnProgress: func(string) {},
		FnAuth: func(authURL, _ string) {
			parsed, err := url.Parse(authURL)
			if err != nil {
				t.Error(err)
				return
			}
			query := parsed.Query()
			if parsed.Path != "/authorize" || query.Get("client_id") != "pi-gateway" || query.Get("scope") != "gateway offline_access" ||
				query.Get("code_challenge_method") != "S256" || query.Get("handoff") != "url" {
				t.Errorf("auth URL = %s", authURL)
			}
			go func() {
				resp, err := http.Get(query.Get("redirect_uri") + "?code=the-code&state=" + url.QueryEscape(query.Get("state")))
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	creds, err := provider.Login(ctx, callbacks)
	if err != nil {
		t.Fatal(err)
	}
	form := <-forms
	if form.Get("code") != "the-code" || form.Get("code_verifier") == "" || form.Get("client_id") != "pi-gateway" {
		t.Fatalf("token form = %v", form)
	}
	if creds.Access != "access-authorization_code" || creds.Refresh != "refresh" || provider.GetAPIKey(creds) != creds.Access {
		t.Fatalf("creds = %+v", creds)
	}
	if remaining := time.Until(time.UnixMilli(creds.Expires)); remaining > time.Hour-time.Minute+time.Second || remaining < time.Hour-2*time.Minute {
		t.Fatalf("expiry skew: %s", remaining)
	}

	refreshed, err := provider.RefreshToken(ctx, creds)
	if err != nil {
		t.Fatal(err)
	}
	if form := <-forms; form.Get("refresh_token") != "refresh" || refreshed.Access != "access-refresh_token" {
		t.Fatalf("refresh form = %v creds = %+v", form, refreshed)
	}
}

func TestRadiusDeviceLoginRequestsDeviceCode(t *testing.T) {
	forms := make(chan url.Values, 4)
	server := radiusTestGateway(t, forms)
	defer server.Close()
	provider := NewRadiusProvider(server.URL)
	var shown string
	ctx, cancel := context.WithCancel(t.Context())
	callbacks := &DefaultLoginCallbacks{
		FnPrompt:   func(context.Context, string, string) (string, error) { return "device-code", nil },
		FnProgress: func(string) {},
		FnAuth: func(authURL, instructions string) {
			shown = authURL + " " + instructions
			cancel()
		},
	}
	if _, err := provider.Login(ctx, callbacks); err == nil {
		t.Fatal("cancelled device login succeeded")
	}
	if form := <-forms; form.Get("client_id") != "pi-gateway" || form.Get("scope") != "gateway offline_access" {
		t.Fatalf("device form = %v", form)
	}
	if shown != "https://radius.example/device Enter code ABCD" {
		t.Fatalf("shown = %q", shown)
	}
	if _, err := provider.Login(t.Context(), &DefaultLoginCallbacks{FnPrompt: func(context.Context, string, string) (string, error) { return "smoke", nil }}); err == nil {
		t.Fatal("unknown sign-in method accepted")
	}
}
