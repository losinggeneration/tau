package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestCallbackServerServesAuthCallbackPath(t *testing.T) {
	server := newCallbackServer("state-123", 0)
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer server.Stop()

	resp, err := http.Get(server.AuthCallbackURL() + "?code=code-123&state=state-123")
	if err != nil {
		t.Fatalf("GET auth callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth callback status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	code, _, err := server.WaitForCode(ctx)
	if err != nil {
		t.Fatalf("WaitForCode: %v", err)
	}
	if code != "code-123" {
		t.Fatalf("code = %q, want %q", code, "code-123")
	}
}

func TestOpenAICodexLoginUsesServedAuthCallbackRedirect(t *testing.T) {
	accessToken := testOpenAIAccountJWT(t, "acct_123")

	var mu sync.Mutex
	var tokenRedirectURI string
	var tokenCodeVerifier string
	var authRedirectURI string
	var authCodeChallenge string
	var authCodeChallengeMethod string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		mu.Lock()
		tokenRedirectURI = r.Form.Get("redirect_uri")
		tokenCodeVerifier = r.Form.Get("code_verifier")
		mu.Unlock()
		if !strings.HasSuffix(tokenRedirectURI, "/auth/callback") {
			t.Errorf("token redirect_uri = %q, want /auth/callback", tokenRedirectURI)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"refresh-123","expires_in":3600,"token_type":"Bearer"}`, accessToken)
	}))
	defer tokenServer.Close()

	provider := &openaiCodexProvider{
		clientID:     "test-client",
		authURL:      "https://auth.example.test/oauth/authorize",
		tokenURL:     tokenServer.URL,
		callbackPort: 0,
		originator:   "pi",
	}

	callbacks := &DefaultLoginCallbacks{
		FnAuth: func(rawURL string, instructions string) {
			authURL, err := url.Parse(rawURL)
			if err != nil {
				t.Errorf("parse auth URL: %v", err)
				return
			}
			q := authURL.Query()
			authRedirectURI = q.Get("redirect_uri")
			authCodeChallenge = q.Get("code_challenge")
			authCodeChallengeMethod = q.Get("code_challenge_method")
			if !strings.HasSuffix(authRedirectURI, "/auth/callback") {
				t.Errorf("auth redirect_uri = %q, want /auth/callback", authRedirectURI)
			}
			state := q.Get("state")
			go func() {
				callbackURL := authRedirectURI + "?code=code-123&state=" + url.QueryEscape(state)
				resp, err := http.Get(callbackURL)
				if err != nil {
					t.Errorf("GET callback: %v", err)
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("callback status = %d, want %d", resp.StatusCode, http.StatusOK)
				}
			}()
		},
		FnProgress: func(string) {},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	creds, err := provider.Login(ctx, callbacks)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if creds.AccountID != "acct_123" {
		t.Fatalf("AccountID = %q, want acct_123", creds.AccountID)
	}
	if authRedirectURI == "" {
		t.Fatal("auth redirect_uri was not captured")
	}
	mu.Lock()
	gotTokenRedirect := tokenRedirectURI
	gotTokenCodeVerifier := tokenCodeVerifier
	mu.Unlock()
	if gotTokenRedirect != authRedirectURI {
		t.Fatalf("token redirect_uri = %q, want auth redirect_uri %q", gotTokenRedirect, authRedirectURI)
	}
	if authCodeChallengeMethod != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", authCodeChallengeMethod)
	}
	if gotTokenCodeVerifier == "" {
		t.Fatal("token request missing code_verifier")
	}
	if gotChallenge := oauth2.S256ChallengeFromVerifier(gotTokenCodeVerifier); gotChallenge != authCodeChallenge {
		t.Fatalf("code_challenge = %q, want S256(code_verifier) %q", authCodeChallenge, gotChallenge)
	}
}

func testOpenAIAccountJWT(t *testing.T, accountID string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payloadBytes, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	})
	if err != nil {
		t.Fatalf("marshal JWT payload: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return header + "." + payload + ".signature"
}
