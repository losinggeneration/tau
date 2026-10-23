package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	configTemplateReference = regexp.MustCompile(`\$\$|\$!|\$\{[^}]*\}|\$[A-Za-z_][A-Za-z0-9_]*`)
	configEnvironmentName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

const configCommandOutputLimit = 1024 * 1024

// configValueResolver keeps command results local to one loaded configuration.
type configValueResolver struct {
	mu       sync.Mutex
	commands map[string]*configCommandResult
	execute  func(context.Context, string) (string, error)
}

type configCommandResult struct {
	done  chan struct{}
	value string
	err   error
}

type configCommandError struct{ cause error }

func (e configCommandError) Error() string { return "configuration command failed" }
func (e configCommandError) Unwrap() error { return e.cause }

// storedCredentialValues caches stored credential commands for the process
// lifetime, as upstream's auth storage does.
var storedCredentialValues configValueResolver

// ResolveConfigValue resolves a stored credential value: a leading "!" runs a
// command, "$NAME" and "${NAME}" read env before the process environment, and
// "$$" and "$!" escape literal characters.
func ResolveConfigValue(ctx context.Context, value string, env map[string]string) (string, error) {
	return storedCredentialValues.resolveWithEnv(ctx, value, env)
}

func (r *configValueResolver) resolve(ctx context.Context, value string) (string, error) {
	return r.resolveWithEnv(ctx, value, nil)
}

func (r *configValueResolver) resolveWithEnv(ctx context.Context, value string, env map[string]string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if command, ok := strings.CutPrefix(value, "!"); ok {
		return r.command(ctx, command)
	}
	var missing []string
	resolved := configTemplateReference.ReplaceAllStringFunc(value, func(reference string) string {
		if reference == "$$" || reference == "$!" {
			return reference[1:]
		}
		name := strings.TrimPrefix(reference, "$")
		if strings.HasPrefix(name, "{") {
			name = name[1 : len(name)-1]
		}
		if !configEnvironmentName.MatchString(name) {
			return reference
		}
		v := env[name]
		if v == "" {
			v = os.Getenv(name)
		}
		if v == "" && !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) == 1 {
		return "", fmt.Errorf("missing configuration environment variable %s", missing[0])
	}
	if len(missing) > 1 {
		return "", fmt.Errorf("missing configuration environment variables %s", strings.Join(missing, ", "))
	}
	return resolved, nil
}

func (r *configValueResolver) command(ctx context.Context, command string) (string, error) {
	r.mu.Lock()
	if cached, ok := r.commands[command]; ok {
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-cached.done:
			return cached.value, cached.err
		}
	}
	result := &configCommandResult{done: make(chan struct{})}
	if r.commands == nil {
		r.commands = make(map[string]*configCommandResult)
	}
	r.commands[command] = result
	r.mu.Unlock()
	execute := r.execute
	if execute == nil {
		execute = executeConfigCommand
	}
	commandCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	value, err := execute(commandCtx, command)
	if commandCtx.Err() != nil {
		err = errors.Join(commandCtx.Err(), err)
	}
	cancel()
	value = strings.TrimSpace(value)
	if err == nil && len(value) > configCommandOutputLimit {
		err = errors.New("configuration command output exceeded limit")
	}
	if err == nil && value == "" {
		err = errors.New("configuration command returned no value")
	}
	if err != nil {
		err = configCommandError{cause: err}
		value = ""
	}
	r.mu.Lock()
	result.value, result.err = value, err
	if err != nil {
		delete(r.commands, command)
	}
	close(result.done)
	r.mu.Unlock()
	return value, err
}

type configOutput struct{ strings.Builder }

func (b *configOutput) Write(p []byte) (int, error) {
	if len(p) > configCommandOutputLimit-b.Len() {
		return 0, errors.New("configuration command output exceeded limit")
	}
	return b.Builder.Write(p)
}

func executeConfigCommand(ctx context.Context, command string) (string, error) {
	shell := os.Getenv("TAU_SHELL")
	args := []string{"-c", command}
	if shell == "" {
		shell = "/bin/sh"
		if runtime.GOOS == "windows" {
			shell = "cmd.exe"
			args = []string{"/C", command}
		}
	}
	cmd := exec.CommandContext(ctx, shell, args...)
	configureConfigCommand(cmd)
	cmd.WaitDelay = 100 * time.Millisecond
	var output configOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return output.String(), nil
}

type modelRequestConfig struct {
	resolver   *configValueResolver
	apiKey     string
	authHeader bool
}

// resolveRequestOptions resolves configuration only at the request boundary so
// catalogs and persisted model metadata never contain expanded secrets.
func resolveRequestOptions(ctx context.Context, model *Model, opts []Option) ([]Option, error) {
	if model == nil {
		return nil, errors.New("model is nil")
	}
	cfg := model.requestConfig
	if cfg == nil && len(model.Headers) == 0 {
		return opts, nil
	}
	o := applyOptions(opts)
	key := o.apiKey
	if key == "" && cfg != nil && cfg.apiKey != "" {
		var err error
		key, err = cfg.resolver.resolve(ctx, cfg.apiKey)
		if err != nil {
			return nil, fmt.Errorf("provider %s API key: %w", model.Provider, err)
		}
	}
	headers := make(http.Header)
	for name, value := range model.Headers {
		if cfg != nil {
			var err error
			value, err = cfg.resolver.resolve(ctx, value)
			if err != nil {
				return nil, fmt.Errorf("provider %s configured header: %w", model.Provider, err)
			}
		}
		headers.Set(name, value)
	}
	if cfg != nil && cfg.authHeader {
		if key == "" {
			key = GetEnvAPIKey(model.Provider)
		}
		if key == "" {
			return nil, errors.New("authHeader requires a resolved API key")
		}
		headers.Set("Authorization", "Bearer "+key)
	}
	for name, value := range o.headers {
		headers.Set(name, value)
	}
	merged := make(map[string]string, len(headers))
	for name := range headers {
		merged[name] = headers.Get(name)
	}
	resolved := append([]Option(nil), opts...)
	return append(resolved, WithAPIKey(key), WithHeaders(merged)), nil
}

func copyConfiguredHeaders(dst, src map[string]string) {
	for name, value := range src {
		for existing := range dst {
			if strings.EqualFold(existing, name) {
				delete(dst, existing)
			}
		}
		dst[name] = value
	}
}
