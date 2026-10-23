package oauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestOpenAICodexLoginFallsBackToManualOnOccupiedPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	for _, inputKind := range []string{"url", "query", "fragment", "code", "mismatch", "empty", "error", "cancel"} {
		t.Run(inputKind, func(t *testing.T) {
			var authQuery url.Values
			var exchanged bool
			tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				exchanged = true
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Form.Get("code") != "pasted" || r.Form.Get("redirect_uri") != fmt.Sprintf("http://localhost:%d/auth/callback", port) || oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != authQuery.Get("code_challenge") {
					t.Errorf("exchange does not match authorization: %v", r.Form)
				}
				w.Header().Set("Content-Type", "application/json")
				if _, err := fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"refresh","expires_in":3600}`, testOpenAIAccountJWT(t, "account")); err != nil {
					t.Error(err)
				}
			}))
			defer tokens.Close()
			provider := NewOpenAICodexProvider("my-app").(*openaiCodexProvider)
			provider.callbackPort = port
			provider.authURL = "https://auth.example.test/authorize"
			provider.tokenURL = tokens.URL
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			promptErr := errors.New("manual prompt failed")
			callbacks := &DefaultLoginCallbacks{
				FnAuth: func(rawURL, _ string) {
					parsed, err := url.Parse(rawURL)
					if err != nil {
						t.Fatal(err)
					}
					authQuery = parsed.Query()
					if authQuery.Get("originator") != "my-app" || authQuery.Get("redirect_uri") != fmt.Sprintf("http://localhost:%d/auth/callback", port) {
						t.Errorf("authorization query: %v", authQuery)
					}
					if inputKind == "cancel" {
						cancel()
					}
				},
				FnProgress: func(string) {},
				FnManualCodeInput: func(context.Context) (string, error) {
					if inputKind == "cancel" {
						t.Error("prompt started after cancellation")
					}
					switch inputKind {
					case "url":
						return "http://localhost/auth/callback?code=pasted&state=" + authQuery.Get("state"), nil
					case "query":
						return "code=pasted&state=" + authQuery.Get("state"), nil
					case "fragment":
						return "pasted#" + authQuery.Get("state"), nil
					case "mismatch":
						return "code=pasted&state=wrong", nil
					case "empty":
						return "  ", nil
					case "error":
						return "", promptErr
					default:
						return " pasted ", nil
					}
				},
			}
			creds, err := provider.Login(ctx, callbacks)
			switch inputKind {
			case "mismatch", "empty", "error", "cancel":
				if err == nil || exchanged {
					t.Fatalf("invalid sign-in exchanged=%v error=%v", exchanged, err)
				}
				if inputKind == "error" && !errors.Is(err, promptErr) || inputKind == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("error cause lost: %v", err)
				}
			default:
				if err != nil || creds == nil || creds.AccountID != "account" || !exchanged {
					t.Fatalf("login credentials=%+v error=%v exchanged=%v", creds, err, exchanged)
				}
			}
		})
	}
}

func TestParseCodexAuthorizationInputRejectsBadState(t *testing.T) {
	for _, input := range []string{"pasted#wrong", "code=pasted&state=wrong", "http://localhost/callback?code=pasted&state=wrong"} {
		if _, err := parseAuthorizationInput(input, "expected"); err == nil || !strings.Contains(err.Error(), "state mismatch") {
			t.Fatalf("input=%q err=%v", input, err)
		}
	}
}

func TestOpenAICodexLoginUsesConfiguredOriginatorAndCallbackHost(t *testing.T) {
	t.Setenv("PI_OAUTH_CALLBACK_HOST", "127.0.0.2")
	for _, originator := range []string{"", "my-app"} {
		provider := NewOpenAICodexProvider(originator).(*openaiCodexProvider)
		provider.callbackPort = 0
		provider.authURL = "https://auth.example.test/authorize"
		ctx, cancel := context.WithCancel(t.Context())
		var notified bool
		_, err := provider.Login(ctx, &DefaultLoginCallbacks{
			FnAuth: func(rawURL, _ string) {
				notified = true
				parsed, err := url.Parse(rawURL)
				if err != nil {
					t.Fatal(err)
				}
				want := originator
				if want == "" {
					want = "pi"
				}
				if parsed.Query().Get("originator") != want {
					t.Errorf("originator=%q want=%q", parsed.Query().Get("originator"), want)
				}
				redirect, err := url.Parse(parsed.Query().Get("redirect_uri"))
				if err != nil {
					t.Fatal(err)
				}
				listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", redirect.Port()))
				if err != nil {
					t.Errorf("callback bound the default host instead of configured host: %v", err)
				} else if err := listener.Close(); err != nil {
					t.Error(err)
				}
				cancel()
			},
			FnProgress: func(string) {},
		})
		cancel()
		if !notified || !errors.Is(err, context.Canceled) {
			t.Fatalf("notified=%v error=%v", notified, err)
		}
	}
}
