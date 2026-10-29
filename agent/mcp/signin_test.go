package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authServer is a fake MCP server with its own authorization server: it
// registers clients, trades code "the-code" for token "fresh" and accepts
// only that token.
type authServer struct {
	*httptest.Server
	issSupported bool
	mu           sync.Mutex
	registered   map[string]any
	scope        string
}

func newAuthServer(t *testing.T, issSupported bool) *authServer {
	t.Helper()
	a := &authServer{issSupported: issSupported}
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake"}, nil)
	addTool(srv, "echo", "Echo", nil)
	mcpHandler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(v))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"resource": a.URL + "/mcp", "authorization_servers": []string{a.URL}, "scopes_supported": []string{"read"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer": a.URL, "authorization_endpoint": a.URL + "/authorize", "token_endpoint": a.URL + "/token",
			"registration_endpoint": a.URL + "/register", "code_challenge_methods_supported": []string{"S256"},
			"response_types_supported":                       []string{"code"},
			"authorization_response_iss_parameter_supported": a.issSupported,
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		a.mu.Lock()
		a.registered = body
		a.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"client_id": "dyn", "redirect_uris": body["redirect_uris"], "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("code") != "the-code" || r.FormValue("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "invalid_grant"})
			return
		}
		writeJSON(w, map[string]any{"access_token": "fresh", "token_type": "Bearer", "refresh_token": "r1", "expires_in": 3600})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+a.URL+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
	a.Server = httptest.NewServer(mux)
	t.Cleanup(a.Close)
	return a
}

// browser answers an authorization URL like a user approving it: the
// callback gets the code, the state and the issuer.
func browser(t *testing.T, iss string) func(string) {
	t.Helper()
	return func(raw string) {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		q := u.Query()
		callback, err := url.Parse(q.Get("redirect_uri"))
		require.NoError(t, err)
		reply := url.Values{"code": {"the-code"}, "state": {q.Get("state")}}
		if iss != "" {
			reply.Set("iss", iss)
		}
		callback.RawQuery = reply.Encode()
		go func() {
			resp, err := http.Get(callback.String()) //nolint:noctx // test browser
			if err == nil {
				_ = resp.Body.Close() // test browser
			}
		}()
	}
}

func signInManager(t *testing.T, a *authServer) (*Manager, *CredentialStore) {
	t.Helper()
	store := NewCredentialStore(t.TempDir())
	cfg := Config{Servers: []ServerEntry{entry("docs", ServerConfig{URL: a.URL + "/mcp"})}}
	m := NewManager(cfg, t.TempDir(), "test", WithCredentials(store))
	t.Cleanup(func() { assert.NoError(t, m.Close()) })
	m.Start()
	waitConnected(t, m)
	require.Equal(t, StatusNeedsAuth, m.States()[0].Status)
	return m, store
}

func TestSignInRegistersNativeClientAndSavesTokens(t *testing.T) {
	a := newAuthServer(t, true)
	m, store := signInManager(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	var shown string
	show := browser(t, a.URL)
	require.NoError(t, m.SignIn(ctx, "docs", SignInPrompt{ShowURL: func(u string) { shown = u; show(u) }}))
	assert.Contains(t, shown, a.URL+"/authorize?")
	assert.Contains(t, shown, "code_challenge_method=S256")
	a.mu.Lock()
	assert.Equal(t, "native", a.registered["application_type"])
	assert.Equal(t, "tau", a.registered["client_name"])
	a.mu.Unlock()
	cred, err := store.Load(ctx, "docs", a.URL+"/mcp")
	require.NoError(t, err)
	assert.Equal(t, "dyn", cred.ClientID)
	assert.Equal(t, "fresh", cred.Token.AccessToken)
	assert.Equal(t, a.URL+"/token", cred.TokenURL)

	require.NoError(t, m.Reconnect(ctx, "docs"))
	assert.Equal(t, StatusConnected, m.States()[0].Status)

	removed, err := m.SignOut(ctx, "docs")
	require.NoError(t, err)
	assert.True(t, removed)
	assert.Equal(t, StatusNeedsAuth, m.States()[0].Status)
	cred, err = store.Load(ctx, "docs", a.URL+"/mcp")
	require.NoError(t, err)
	assert.Nil(t, cred)
}

func TestSignInAcceptsPastedRedirectURL(t *testing.T) {
	a := newAuthServer(t, false)
	m, store := signInManager(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	urls := make(chan string, 1)
	prompt := SignInPrompt{
		ShowURL: func(u string) { urls <- u },
		RedirectURL: func(context.Context) (string, error) {
			u, err := url.Parse(<-urls)
			if err != nil {
				return "", err
			}
			q := u.Query()
			return q.Get("redirect_uri") + "?code=the-code&state=" + url.QueryEscape(q.Get("state")), nil
		},
	}
	require.NoError(t, m.SignIn(ctx, "docs", prompt))
	cred, err := store.Load(ctx, "docs", a.URL+"/mcp")
	require.NoError(t, err)
	assert.Equal(t, "fresh", cred.Token.AccessToken)
}

func TestSignInRejectsUnexpectedIssuer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		issSupported bool
		iss          string
	}{
		{name: "mismatch", issSupported: true, iss: "https://evil.example"},
		{name: "missing", issSupported: true},
		{name: "unadvertised", iss: "self"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAuthServer(t, tc.issSupported)
			m, store := signInManager(t, a)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			iss := tc.iss
			if iss == "self" {
				iss = a.URL
			}
			err := m.SignIn(ctx, "docs", SignInPrompt{ShowURL: browser(t, iss)})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "iss")
			cred, err := store.Load(ctx, "docs", a.URL+"/mcp")
			require.NoError(t, err)
			assert.Nil(t, cred, "no tokens saved")
		})
	}
}

func TestSignInCancelsOnPromptContextAndShutdown(t *testing.T) {
	a := newAuthServer(t, true)
	m, _ := signInManager(t, a)

	empty := SignInPrompt{RedirectURL: func(context.Context) (string, error) { return " ", nil }}
	assert.ErrorIs(t, m.SignIn(t.Context(), "docs", empty), ErrSignInCancelled)

	ctx, cancel := context.WithCancel(t.Context())
	escape := SignInPrompt{ShowURL: func(string) { cancel() }}
	assert.ErrorIs(t, m.SignIn(ctx, "docs", escape), ErrSignInCancelled)

	shown := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- m.SignIn(t.Context(), "docs", SignInPrompt{ShowURL: func(string) { close(shown) }})
	}()
	<-shown
	require.NoError(t, m.Close())
	select {
	case err := <-done:
		assert.ErrorIs(t, err, ErrSignInCancelled)
	case <-time.After(time.Second):
		t.Fatal("shutdown left the sign-in waiting")
	}
}

func TestSignInTimesOut(t *testing.T) {
	a := newAuthServer(t, true)
	m, _ := signInManager(t, a)
	old := signInTimeout
	signInTimeout = 50 * time.Millisecond
	t.Cleanup(func() { signInTimeout = old })
	err := m.SignIn(t.Context(), "docs", SignInPrompt{})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrSignInCancelled))
	assert.Contains(t, err.Error(), "timed out")
}

func TestResponseFromRedirectURLChecksEverything(t *testing.T) {
	const redirect = "http://127.0.0.1:1234/callback"
	for input, want := range map[string]string{
		"not a url": "expected the full redirect URL",
		"http://127.0.0.1:9/callback?code=c&state=s":                                  "does not match",
		"http://127.0.0.1:1234/callback?code=c&state=other":                           "different sign-in",
		"http://127.0.0.1:1234/callback?error=access_denied&error_description=Denied": "Denied",
		"http://127.0.0.1:1234/callback?state=s":                                      "authorization code",
	} {
		_, err := responseFromRedirectURL(input, "s", redirect)
		require.Error(t, err, input)
		assert.Contains(t, err.Error(), want, input)
	}
	result, err := responseFromRedirectURL(" http://127.0.0.1:1234/callback?code=c&state=s&iss=i ", "s", redirect)
	require.NoError(t, err)
	assert.Equal(t, "c", result.Code)
	assert.Equal(t, "i", result.Iss)
}
