package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type anthropicFederation struct {
	mu      sync.Mutex
	gate    chan struct{}
	key     [32]byte
	token   string
	expires time.Time
}

func (f *anthropicFederation) accessToken(ctx context.Context, baseURL string, client *http.Client, o *options) (token string, err error) {
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	rule, organization, file := o.providerEnvValue("ANTHROPIC_FEDERATION_RULE_ID"), o.providerEnvValue("ANTHROPIC_ORGANIZATION_ID"), o.providerEnvValue("ANTHROPIC_IDENTITY_TOKEN_FILE")
	if rule == "" || organization == "" || file == "" {
		return "", nil
	}
	endpoint, e := url.Parse(strings.TrimRight(baseURL, "/"))
	if e != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", errors.New("anthropic federation: invalid token endpoint")
	}
	loopback := endpoint.Hostname() == "localhost" || endpoint.Hostname() == "127.0.0.1" || endpoint.Hostname() == "::1"
	if endpoint.Scheme != "https" && (endpoint.Scheme != "http" || !loopback) {
		return "", errors.New("anthropic federation: refusing insecure token endpoint")
	}
	body := map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer", "federation_rule_id": rule, "organization_id": organization}
	for _, entry := range []struct{ field, variable string }{{"service_account_id", "ANTHROPIC_SERVICE_ACCOUNT_ID"}, {"workspace_id", "ANTHROPIC_WORKSPACE_ID"}} {
		if value := o.providerEnvValue(entry.variable); value != "" {
			body[entry.field] = value
		}
	}
	identity, err := os.Open(file)
	if err != nil {
		return "", fmt.Errorf("anthropic federation: open identity token: %w", err)
	}
	info, statErr := identity.Stat()
	if statErr != nil {
		return "", errors.Join(fmt.Errorf("anthropic federation: stat identity token: %w", statErr), identity.Close())
	}
	if !info.Mode().IsRegular() {
		return "", errors.Join(errors.New("anthropic federation: identity token is not a regular file"), identity.Close())
	}
	assertion, readErr := io.ReadAll(io.LimitReader(identity, 16*1024+1))
	if err = errors.Join(readErr, identity.Close(), context.Cause(ctx)); err != nil {
		return "", fmt.Errorf("anthropic federation: read identity token: %w", err)
	}
	if len(assertion) > 16*1024 {
		return "", errors.New("anthropic federation: identity token exceeds 16 KiB")
	}
	if strings.TrimSpace(string(assertion)) == "" {
		return "", errors.New("anthropic federation: empty identity token")
	}
	body["assertion"] = strings.TrimSpace(string(assertion))
	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("anthropic federation: marshal exchange: %w", err)
	}
	// ponytail: one cached configuration per client; use a keyed cache if alternating identities become common.
	f.mu.Lock()
	if f.gate == nil {
		f.gate = make(chan struct{}, 1)
	}
	gate := f.gate
	f.mu.Unlock()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return "", context.Cause(ctx)
	}
	defer func() { <-gate }()
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	key := sha256.Sum256(append([]byte(baseURL+"\x00"), data...))
	if key == f.key && time.Until(f.expires) > 30*time.Second {
		return f.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/oauth/token", strings.NewReader(string(data)))
	if err != nil {
		return "", fmt.Errorf("anthropic federation: create exchange: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20,oidc-federation-2026-04-01")
	req.Header.Set("User-Agent", "tau-ai oidc-federation")
	if client == nil {
		client = http.DefaultClient
	}
	exchangeClient := *client
	if o.timeout > 0 {
		exchangeClient.Timeout = o.timeout
	}
	exchangeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := exchangeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic federation: exchange: %w", errors.Join(err, context.Cause(ctx)))
	}
	defer func() {
		err = errors.Join(err, resp.Body.Close())
		if err != nil {
			f.token = ""
			f.expires = time.Time{}
			token = ""
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("anthropic federation: token exchange status %d", resp.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil {
		return "", fmt.Errorf("anthropic federation: read exchange: %w", errors.Join(err, context.Cause(ctx)))
	}
	if len(payload) > 1024*1024 {
		return "", errors.New("anthropic federation: token response exceeds 1 MiB")
	}
	var result struct {
		AccessToken string  `json:"access_token"`
		TokenType   string  `json:"token_type"`
		ExpiresIn   float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return "", errors.New("anthropic federation: invalid token response")
	}
	if result.AccessToken == "" || (result.TokenType != "" && !strings.EqualFold(result.TokenType, "Bearer")) || result.ExpiresIn <= 0 || result.ExpiresIn > float64((24*time.Hour*365)/time.Second) {
		return "", errors.New("anthropic federation: invalid access token or expiry")
	}
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	f.key, f.token, f.expires = key, result.AccessToken, time.Now().Add(time.Duration(result.ExpiresIn*float64(time.Second)))
	return f.token, nil
}
