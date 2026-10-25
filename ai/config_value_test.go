package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConfigValueResolverTemplates(t *testing.T) {
	t.Setenv("TAU_CONFIG_VALUE", "secret")
	t.Setenv("TAU_CONFIG_MISSING", "")
	r := &configValueResolver{}
	for _, tc := range []struct{ input, want string }{
		{"literal", "literal"},
		{"TAU_CONFIG_VALUE", "TAU_CONFIG_VALUE"},
		{"$TAU_CONFIG_VALUE", "secret"},
		{"Bearer ${TAU_CONFIG_VALUE}/$TAU_CONFIG_VALUE", "Bearer secret/secret"},
		{"$$TAU_CONFIG_VALUE", "$TAU_CONFIG_VALUE"},
		{"$!literal", "!literal"},
		{"$", "$"},
		{"${bad-name}", "${bad-name}"},
		{"${TAU_CONFIG_VALUE", "${TAU_CONFIG_VALUE"},
		{"$1", "$1"},
		{"${}", "${}"},
		{"${bad-$TAU_CONFIG_VALUE}", "${bad-$TAU_CONFIG_VALUE}"},
	} {
		got, err := r.resolve(t.Context(), tc.input)
		if err != nil || got != tc.want {
			t.Errorf("input=%q got=%q want=%q err=%v", tc.input, got, tc.want, err)
		}
	}
	if _, err := r.resolve(t.Context(), "secret $TAU_CONFIG_MISSING"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("missing variable error=%v", err)
	}
}

func TestConfigCommandsCacheSuccessAndRespectCancellation(t *testing.T) {
	var calls atomic.Int32
	r := &configValueResolver{execute: func(ctx context.Context, command string) (string, error) {
		calls.Add(1)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Error("command is not bounded")
		}
		if command != "sensitive-command" {
			t.Error("unexpected command")
		}
		return "  secret\n", nil
	}}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			value, err := r.resolve(t.Context(), "!sensitive-command")
			if err != nil || value != "secret" {
				t.Errorf("command resolution failed: %v", err)
			}
		})
	}
	workers.Wait()
	if calls.Load() != 1 {
		t.Fatalf("executions=%d", calls.Load())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.resolve(ctx, "!sensitive-command"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cache read: %v", err)
	}
}

func TestConfigCommandsDoNotLeakErrorsOrCacheFailures(t *testing.T) {
	for _, failure := range []error{errors.New("secret command output"), context.Canceled, context.DeadlineExceeded} {
		var calls int
		r := &configValueResolver{execute: func(context.Context, string) (string, error) { calls++; return "secret", failure }}
		for range 2 {
			value, err := r.resolve(t.Context(), "!secret command")
			if value != "" || !errors.Is(err, failure) || strings.Contains(fmt.Sprint(err), "secret") {
				t.Fatalf("unsafe command error: %v", err)
			}
		}
		if calls != 2 {
			t.Fatal("failed command was cached")
		}
	}
	for _, value := range []string{"  ", strings.Repeat("a", configCommandOutputLimit+1)} {
		r := &configValueResolver{execute: func(context.Context, string) (string, error) { return value, nil }}
		if _, err := r.resolve(t.Context(), "!command"); err == nil {
			t.Fatal("invalid command output accepted")
		}
	}
	var output configOutput
	if _, err := output.Write(make([]byte, configCommandOutputLimit+1)); err == nil || output.Len() != 0 {
		t.Fatal("output writer is unbounded")
	}
}

func TestConfigCommandWaiterCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	r := &configValueResolver{execute: func(ctx context.Context, _ string) (string, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
			return "ok", nil
		}
	}}
	done := make(chan error, 1)
	go func() { _, err := r.resolve(t.Context(), "!command"); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.resolve(ctx, "!command"); !errors.Is(err, context.Canceled) {
		t.Errorf("waiter error=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConfigCommandOwnerCancellationAllowsRetry(t *testing.T) {
	entered := make(chan struct{})
	var calls int
	r := &configValueResolver{execute: func(ctx context.Context, _ string) (string, error) {
		calls++
		if calls == 1 {
			close(entered)
			<-ctx.Done()
			return "sensitive partial output", ctx.Err()
		}
		return "ok", nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := r.resolve(ctx, "!command"); done <- err }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("cancellation error=%v", err)
	}
	if value, err := r.resolve(t.Context(), "!command"); err != nil || value != "ok" || calls != 2 {
		t.Fatalf("retry value=%q calls=%d error=%v", value, calls, err)
	}
}

func TestResolveConfigValueCachesStoredCommandsForProcess(t *testing.T) {
	var calls atomic.Int32
	previous := storedCredentialValues.execute
	storedCredentialValues.execute = func(context.Context, string) (string, error) {
		calls.Add(1)
		return "stored-secret\n", nil
	}
	t.Cleanup(func() {
		storedCredentialValues.mu.Lock()
		storedCredentialValues.execute = previous
		delete(storedCredentialValues.commands, "stored-command-test")
		storedCredentialValues.mu.Unlock()
	})
	for range 2 {
		value, err := ResolveConfigValue(t.Context(), "!stored-command-test", map[string]string{"IGNORED": "x"})
		if err != nil || value != "stored-secret" {
			t.Fatalf("value=%q err=%v", value, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("executions=%d", calls.Load())
	}
}

func TestConfigValueResolverReportsEveryMissingVariable(t *testing.T) {
	t.Setenv("TAU_TEST_MISSING_A", "")
	t.Setenv("TAU_TEST_MISSING_B", "")
	_, err := (&configValueResolver{}).resolve(t.Context(), "$TAU_TEST_MISSING_A-${TAU_TEST_MISSING_B}-$TAU_TEST_MISSING_A")
	if err == nil || !strings.Contains(err.Error(), "variables TAU_TEST_MISSING_A, TAU_TEST_MISSING_B") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfiguredAuthHeaderUsesEnvironmentBearer(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "oauth")
	t.Setenv("ANTHROPIC_API_KEY", "environment-key")
	model := &Model{Provider: ProviderAnthropic, requestConfig: &modelRequestConfig{authHeader: true, resolver: &configValueResolver{}}}
	resolved, err := resolveRequestOptions(t.Context(), model, nil)
	if err != nil {
		t.Fatal(err)
	}
	o := applyOptions(resolved)
	if o.apiKey != "" || o.headers["Authorization"] != "Bearer bearer" {
		t.Errorf("expected header-owned bearer authentication, got key %q and headers %v", o.apiKey, o.headers)
	}
}

func TestRequestConfigEnvOptOutKeepsExplicitReferences(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-bearer")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-key")
	t.Setenv("TAU_CONFIGURED_SECRET", "configured-key")
	model := &Model{Provider: ProviderAnthropic, requestConfig: &modelRequestConfig{authHeader: true, resolver: &configValueResolver{}}}
	_, err := resolveRequestOptions(t.Context(), model, []Option{WithoutEnvCredentials()})
	if err == nil || strings.Contains(err.Error(), "ambient-") {
		t.Fatalf("authHeader must not fall through to environment: %v", err)
	}
	model.requestConfig.apiKey = "$TAU_CONFIGURED_SECRET"
	model.Headers = map[string]string{"X-Configured": "${TAU_CONFIGURED_SECRET}"}
	resolved, err := resolveRequestOptions(t.Context(), model, []Option{WithoutEnvCredentials()})
	if err != nil {
		t.Fatal(err)
	}
	o := applyOptions(resolved)
	if !o.noEnvCredentials || o.apiKey != "configured-key" || o.headers["Authorization"] != "Bearer configured-key" || o.headers["X-Configured"] != "configured-key" {
		t.Fatal("explicit configured references or opt-out were lost")
	}
}
