package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestCredentialStoreKeysByNameAndMigratesLegacy(t *testing.T) {
	dir := t.TempDir()
	legacy := map[string]*Credential{"https://example.com/": {ClientID: "old", Token: &oauth2.Token{AccessToken: "a"}}}
	data, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mcp-auth.json"), data, 0o600))
	store := NewCredentialStore(dir)
	ctx := t.Context()

	first, err := store.Load(ctx, "first", "https://example.com")
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, "old", first.ClientID, "the first server takes the URL-only entry over")
	second, err := store.Load(ctx, "second", "https://example.com")
	require.NoError(t, err)
	assert.Nil(t, second, "other servers with the same URL sign in again")

	require.NoError(t, store.Save(ctx, "second", "https://example.com", &Credential{ClientID: "new"}))
	again, err := store.Load(ctx, "first", "https://example.com")
	require.NoError(t, err)
	assert.Equal(t, "old", again.ClientID)

	removed, err := store.Remove(ctx, "first", "https://example.com")
	require.NoError(t, err)
	assert.True(t, removed)
	removed, err = store.Remove(ctx, "first", "https://example.com")
	require.NoError(t, err)
	assert.False(t, removed)
	info, err := os.Stat(filepath.Join(dir, "mcp-auth.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// oauthServer serves an MCP endpoint that accepts only the "fresh" token and
// a token endpoint that trades refresh token "r1" for it.
func oauthServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake"}, nil)
	addTool(srv, "echo", "Echo", nil)
	mcpHandler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
	var refreshes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		if r.FormValue("grant_type") != "refresh_token" || r.FormValue("refresh_token") != "r1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`)) // test server
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh","token_type":"Bearer","refresh_token":"r2","expires_in":3600}`)) // test server
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, &refreshes
}

func TestManagerRefreshesTokensAndReportsNeedsAuth(t *testing.T) {
	ts, refreshes := oauthServer(t)
	store := NewCredentialStore(t.TempDir())
	url := ts.URL + "/mcp"
	cred := func(access, refresh string, expiry time.Time) *Credential {
		return &Credential{ClientID: "c", TokenURL: ts.URL + "/token", Token: &oauth2.Token{AccessToken: access, RefreshToken: refresh, Expiry: expiry}}
	}
	ctx := t.Context()
	require.NoError(t, store.Save(ctx, "expiring", url, cred("old", "r1", time.Now().Add(10*time.Second))))
	require.NoError(t, store.Save(ctx, "rejected", url, cred("revoked", "r1", time.Time{})))
	require.NoError(t, store.Save(ctx, "no-refresh", url, cred("revoked", "", time.Time{})))
	require.NoError(t, store.Save(ctx, "bad-grant", url, cred("revoked", "bad", time.Time{})))
	cfg := Config{Servers: []ServerEntry{
		entry("expiring", ServerConfig{URL: url}),
		entry("rejected", ServerConfig{URL: url}),
		entry("no-refresh", ServerConfig{URL: url}),
		entry("bad-grant", ServerConfig{URL: url}),
		entry("signed-out", ServerConfig{URL: url}),
	}}
	m := NewManager(cfg, t.TempDir(), "test", WithCredentials(store))
	m.Start()
	waitConnected(t, m)

	status := map[string]Status{}
	for _, s := range m.States() {
		status[s.Name] = s.Status
		if s.Status == StatusNeedsAuth {
			assert.Empty(t, s.Error, s.Name)
		}
	}
	assert.Equal(t, map[string]Status{
		"expiring": StatusConnected, "rejected": StatusConnected,
		"no-refresh": StatusNeedsAuth, "bad-grant": StatusNeedsAuth, "signed-out": StatusNeedsAuth,
	}, status)
	saved, err := store.Load(ctx, "rejected", url)
	require.NoError(t, err)
	assert.Equal(t, "fresh", saved.Token.AccessToken)
	assert.Equal(t, "r2", saved.Token.RefreshToken, "rotated refresh token saved")

	require.NoError(t, m.Close())
	before := refreshes.Load()
	require.NoError(t, store.Save(ctx, "rejected", url, cred("revoked", "r1", time.Time{})))
	assert.ErrorIs(t, m.servers[1].auth.refresh("revoked"), context.Canceled)
	assert.Equal(t, before, refreshes.Load(), "nothing refreshes after shutdown")
}

func TestUsesOAuthSkipsProviderAndAuthorizationHeader(t *testing.T) {
	assert.True(t, usesOAuth(ServerConfig{URL: "https://x"}))
	assert.False(t, usesOAuth(ServerConfig{Command: "x"}))
	assert.False(t, usesOAuth(ServerConfig{URL: "https://x", Headers: map[string]string{"authorization": "Bearer k"}}))
	assert.False(t, usesOAuth(ServerConfig{URL: "https://x", Auth: &struct {
		Provider string `json:"provider"`
	}{Provider: "github"}}))
}
