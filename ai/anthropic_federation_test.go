package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func federationOptions(t *testing.T) *options {
	t.Helper()
	file := filepath.Join(t.TempDir(), "identity")
	require.NoError(t, os.WriteFile(file, []byte("identity-assertion\n"), 0o600))
	return applyOptions([]Option{WithProviderEnv(map[string]string{"ANTHROPIC_FEDERATION_RULE_ID": "rule", "ANTHROPIC_ORGANIZATION_ID": "organization", "ANTHROPIC_IDENTITY_TOKEN_FILE": file, "ANTHROPIC_SERVICE_ACCOUNT_ID": "service", "ANTHROPIC_WORKSPACE_ID": "workspace"})})
}

func TestAnthropicFederationExchangeAndCache(t *testing.T) {
	o := federationOptions(t)
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		require.Equal(t, "https://api.anthropic.invalid/v1/oauth/token", req.URL.String())
		require.Equal(t, "POST", req.Method)
		require.Equal(t, "oauth-2025-04-20,oidc-federation-2026-04-01", req.Header.Get("anthropic-beta"))
		var body map[string]string
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		require.Equal(t, map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer", "assertion": "identity-assertion", "federation_rule_id": "rule", "organization_id": "organization", "service_account_id": "service", "workspace_id": "workspace"}, body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"access","expires_in":3600,"token_type":"Bearer"}`)), Header: make(http.Header)}, nil
	})}
	cache := &anthropicFederation{}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			token, err := cache.accessToken(t.Context(), "https://api.anthropic.invalid", client, o)
			require.NoError(t, err)
			require.Equal(t, "access", token)
		})
	}
	wg.Wait()
	require.EqualValues(t, 1, calls.Load())
	cache.expires = time.Now()
	_, err := cache.accessToken(t.Context(), "https://api.anthropic.invalid", client, o)
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load())
}

func TestAnthropicFederationValidationAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"server rejection", `identity-assertion access secret`, 401},
		{"redirect", ``, 302},
		{"malformed", `identity-assertion`, 200},
		{"missing token", `{"expires_in":3600}`, 200},
		{"unsupported type", `{"access_token":"secret","expires_in":3600,"token_type":"other"}`, 200},
		{"missing expiry", `{"access_token":"secret"}`, 200},
		{"expired", `{"access_token":"secret","expires_in":-1}`, 200},
		{"oversized", strings.Repeat("x", 1024*1024+1), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			token, err := (&anthropicFederation{}).accessToken(t.Context(), "https://api.anthropic.invalid", client, federationOptions(t))
			require.Error(t, err)
			require.Empty(t, token)
			require.NotContains(t, err.Error(), "identity-assertion")
			require.NotContains(t, err.Error(), "secret")
		})
	}
	o := federationOptions(t)
	for _, endpoint := range []string{"http://outside.invalid", "file:///tmp/token", "https://user:password@api.invalid"} {
		_, err := (&anthropicFederation{}).accessToken(t.Context(), endpoint, http.DefaultClient, o)
		require.Error(t, err)
	}
	require.NoError(t, os.WriteFile(o.providerEnv["ANTHROPIC_IDENTITY_TOKEN_FILE"], []byte(strings.Repeat("x", 16*1024+1)), 0o600))
	_, err := (&anthropicFederation{}).accessToken(t.Context(), "https://api.invalid", http.DefaultClient, o)
	require.ErrorContains(t, err, "16 KiB")
}

func TestAnthropicFederationCancellationWait(t *testing.T) {
	o := federationOptions(t)
	cache := &anthropicFederation{gate: make(chan struct{}, 1)}
	cache.gate <- struct{}{}
	cause := errors.New("stop exchange wait")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	_, err := cache.accessToken(ctx, "https://api.invalid", http.DefaultClient, o)
	require.ErrorIs(t, err, cause)
}

func TestAnthropicFederationRotationAndCloseFailure(t *testing.T) {
	o := federationOptions(t)
	calls := 0
	closeErr := errors.New("close exchange")
	rejectClose := false
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		body := io.ReadCloser(io.NopCloser(strings.NewReader(`{"access_token":"access","expires_in":3600}`)))
		if rejectClose {
			body = &federationCloseBody{Reader: strings.NewReader(`{"access_token":"access","expires_in":3600}`), err: closeErr}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})}
	cache := &anthropicFederation{}
	_, err := cache.accessToken(t.Context(), "https://api.invalid", client, o)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(o.providerEnv["ANTHROPIC_IDENTITY_TOKEN_FILE"], []byte("rotated-identity"), 0o600))
	_, err = cache.accessToken(t.Context(), "https://api.invalid", client, o)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	o.providerEnv["ANTHROPIC_WORKSPACE_ID"] = "another"
	rejectClose = true
	token, err := cache.accessToken(t.Context(), "https://api.invalid", client, o)
	require.ErrorIs(t, err, closeErr)
	require.Empty(t, token)
}

type federationCloseBody struct {
	io.Reader
	err error
}

func (b *federationCloseBody) Close() error { return b.err }

func TestAnthropicFederationWaitCanBeCancelled(t *testing.T) {
	cache := &anthropicFederation{gate: make(chan struct{}, 1)}
	cache.gate <- struct{}{}
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("cancel queued exchange")
	done := make(chan error, 1)
	o := federationOptions(t)
	go func() { _, err := cache.accessToken(ctx, "https://api.invalid", http.DefaultClient, o); done <- err }()
	cancel(cause)
	require.ErrorIs(t, <-done, cause)
	<-cache.gate
}

func TestAnthropicFederationNeverFollowsRedirect(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://outside.invalid/token"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	_, err := (&anthropicFederation{}).accessToken(t.Context(), "https://api.invalid", client, federationOptions(t))
	require.ErrorContains(t, err, "status 302")
	require.Equal(t, 1, calls)
}

func TestAnthropicFederationDiscoveryNeedsCompleteConfiguration(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE"} {
		t.Setenv(name, "")
	}
	env := federationOptions(t).providerEnv
	require.True(t, ProviderEnvHasCredentials(ProviderAnthropic, env))
	require.False(t, ProviderEnvHasCredentials(ProviderOpenAI, env))
	require.False(t, HasEnvCredentials(ProviderAnthropic))
	for _, name := range []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE"} {
		value := env[name]
		delete(env, name)
		require.False(t, ProviderEnvHasCredentials(ProviderAnthropic, env))
		env[name] = value
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
	require.True(t, HasEnvCredentials(ProviderAnthropic))
}

func TestAnthropicFederationExchangeRetainsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("cancel federation exchange")
	failure := errors.New("transport failed")
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { cancel(cause); return nil, failure })}
	_, err := (&anthropicFederation{}).accessToken(ctx, "https://api.invalid", client, federationOptions(t))
	require.ErrorIs(t, err, cause)
	require.ErrorIs(t, err, failure)
}

func TestAnthropicFederationInvalidIdentitySources(t *testing.T) {
	for _, kind := range []string{"missing", "empty", "directory"} {
		t.Run(kind, func(t *testing.T) {
			o := federationOptions(t)
			file := o.providerEnv["ANTHROPIC_IDENTITY_TOKEN_FILE"]
			require.NoError(t, os.Remove(file))
			switch kind {
			case "empty":
				require.NoError(t, os.WriteFile(file, nil, 0o600))
			case "directory":
				require.NoError(t, os.Mkdir(file, 0o700))
			}
			calls := 0
			client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected exchange") })}
			_, err := (&anthropicFederation{}).accessToken(t.Context(), "https://api.invalid", client, o)
			require.Error(t, err)
			require.Zero(t, calls)
		})
	}
}
