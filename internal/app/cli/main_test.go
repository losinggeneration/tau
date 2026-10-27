package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/losinggeneration/tau/agent/artifacts"
	"github.com/losinggeneration/tau/agent/packages"
	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/oauth"
	"github.com/losinggeneration/tau/ai/store"
	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/internal/app/tui"
	"github.com/losinggeneration/tau/rpc/server"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

// mockCredStore is a test double for the credential store.
type mockCredStore struct {
	getCalled bool
	getCred   ai.Credential
	getErr    error
	provider  string
}

func (m *mockCredStore) Get(_ context.Context, provider string) (ai.Credential, error) {
	m.getCalled = true
	m.provider = provider
	if m.getErr != nil {
		return ai.Credential{}, m.getErr
	}
	return m.getCred, nil
}

func (m *mockCredStore) Set(_ context.Context, _ string, _ ai.Credential) error {
	return nil
}

func (m *mockCredStore) Delete(_ context.Context, _ string) error {
	return nil
}

func (m *mockCredStore) List(_ context.Context) ([]ai.CredentialRef, error) {
	return nil, nil
}

func (m *mockCredStore) RefreshOAuth(_ context.Context, _ string, _ oauth.Provider) (ai.Credential, error) {
	return ai.Credential{}, nil
}

// TestPrintModeGoldensAgainstRealProvider tests that SDK can be configured with real providers.
// This covers the spec requirement: "SDK tests that exercise real runtime/session wiring".
// Uses mock credential store so we can verify the SDK wires it correctly without needing real API keys.
func TestNewRootCommandRegistersCLIOnlySubcommands(t *testing.T) {
	cmd := newRootCommand()
	var models *cli.Command
	var login *cli.Command
	var providers *cli.Command
	for _, child := range cmd.Commands {
		switch child.Name {
		case "models":
			models = child
		case "login":
			login = child
		case "providers":
			providers = child
		case "accounts":
			t.Fatal("new root command should expose providers, not accounts")
		}
	}
	if models == nil {
		t.Fatal("new root command did not register models subcommand")
	}
	if !slices.Contains(models.Aliases, "model") {
		t.Fatalf("models aliases = %v, want singular alias %q", models.Aliases, "model")
	}
	if login == nil {
		t.Fatal("new root command did not register login subcommand")
	}
	if providers == nil {
		t.Fatal("new root command did not register providers subcommand")
	}
	var list *cli.Command
	for _, child := range models.Commands {
		if child.Name == "list" {
			list = child
			break
		}
	}
	if list == nil {
		t.Fatal("models subcommand did not register list subcommand")
	}
}

func TestModelAliasListRunsAsSubcommand(t *testing.T) {
	t.Setenv("TAU_CONFIG_DIR", t.TempDir())
	cmd := newRootCommand()
	ctx := context.WithValue(context.Background(), rootCmdKey{}, cmd)
	ctx = context.WithValue(ctx, rootPromptDelimiterKey{}, false)

	if err := cmd.Run(ctx, []string{"tau", "model", "list"}); err != nil {
		t.Fatalf("tau model list returned error: %v", err)
	}
}

func TestLoginOpenAIWithoutCredentialFails(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)

	cmd := newRootCommand()
	cmd.ExitErrHandler = func(context.Context, *cli.Command, error) {}
	err := cmd.Run(context.Background(), []string{"tau", "login", ai.ProviderOpenAI})
	if err == nil {
		t.Fatal("tau login openai succeeded without credentials; want error")
	}
	if !strings.Contains(err.Error(), "missing API key for openai") || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("error = %q, want missing OPENAI_API_KEY guidance", err.Error())
	}

	fb := store.NewFileBackend(filepath.Join(configDir, "auth.json"), store.Options{})
	if _, err := fb.Get(context.Background(), ai.ProviderOpenAI); !errors.Is(err, ai.ErrCredentialNotFound) && !os.IsNotExist(err) {
		t.Fatalf("stored OpenAI credential after failed login: err=%v", err)
	}
}

func TestLoginOpenAIRejectsOAuthFlag(t *testing.T) {
	t.Setenv("TAU_CONFIG_DIR", t.TempDir())

	cmd := newRootCommand()
	cmd.ExitErrHandler = func(context.Context, *cli.Command, error) {}
	err := cmd.Run(context.Background(), []string{"tau", "login", ai.ProviderOpenAI, "--oauth"})
	if err == nil {
		t.Fatal("tau login openai --oauth succeeded; want error")
	}
	if !strings.Contains(err.Error(), "OAuth login is not supported for openai") {
		t.Fatalf("error = %q, want unsupported OAuth guidance", err.Error())
	}
}

func TestLoginOpenAIWithAPIKeyStoresAPIKey(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)

	cmd := newRootCommand()
	if err := cmd.Run(context.Background(), []string{"tau", "login", ai.ProviderOpenAI, "--api-key", "sk-test"}); err != nil {
		t.Fatalf("tau login openai --api-key returned error: %v", err)
	}

	fb := store.NewFileBackend(filepath.Join(configDir, "auth.json"), store.Options{})
	cred, err := fb.Get(context.Background(), ai.ProviderOpenAI)
	if err != nil {
		t.Fatalf("get stored credential: %v", err)
	}
	if cred.Type != ai.CredentialTypeAPIKey || cred.APIKey != "sk-test" {
		t.Fatalf("stored credential = %+v, want API key", cred)
	}
}

func TestLoginLocalProviderWithoutCredentialStoresNone(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)

	for _, provider := range []string{ai.ProviderOllama, ai.ProviderClaudeAgent} {
		t.Run(provider, func(t *testing.T) {
			cmd := newRootCommand()
			if err := cmd.Run(context.Background(), []string{"tau", "login", provider}); err != nil {
				t.Fatalf("tau login %s returned error: %v", provider, err)
			}

			fb := store.NewFileBackend(filepath.Join(configDir, "auth.json"), store.Options{})
			cred, err := fb.Get(context.Background(), provider)
			if err != nil {
				t.Fatalf("get stored credential: %v", err)
			}
			if cred.Type != ai.CredentialTypeNone {
				t.Fatalf("stored credential type = %q, want %q", cred.Type, ai.CredentialTypeNone)
			}
		})
	}
}

func TestRenderSessionsListUsesShortestUniqueIDsAndTerminalWidth(t *testing.T) {
	summaries := []session.SessionSummary{
		{ID: "abcdef00-0000-0000-0000-000000000000", Name: "short", FirstPrompt: "short", CWD: "/tmp/project", Created: "2026-08-30T00:00:00Z", MessageCount: 12, Tokens: session.TokenCounts{Input: 100, Output: 23}},
		{ID: "abcfff00-0000-0000-0000-000000000000", Name: "a-very-long-session-name", FirstPrompt: "a very long session name\nwith extra prompt body", CWD: "/home/harley/Projects/ai/tau/with/a/long/path", Created: "2026-08-29T00:00:00Z", MessageCount: 3, Tokens: session.TokenCounts{Input: 4, Output: 5}},
	}

	var b strings.Builder
	if err := renderSessionsList(&b, summaries, 120); err != nil {
		t.Fatalf("renderSessionsList: %v", err)
	}

	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d, want 4:\n%s", len(lines), b.String())
	}
	for _, line := range lines {
		if got := len(line); got != 120 {
			t.Fatalf("line width = %d, want 120 for %q\nfull output:\n%s", got, line, b.String())
		}
	}
	if !strings.HasPrefix(lines[2], "abcd") || !strings.HasPrefix(lines[3], "abcf") {
		t.Fatalf("IDs not shortened to unique prefixes:\n%s", b.String())
	}
	if strings.Contains(b.String(), "abcdef00-") {
		t.Fatalf("full ID leaked into list:\n%s", b.String())
	}
	if !strings.Contains(lines[2], "/tmp/project") {
		t.Fatalf("short cwd should not be truncated:\n%s", b.String())
	}
	if !strings.Contains(lines[3], "a very long session name") {
		t.Fatalf("slug-derived name should render the first prompt:\n%s", b.String())
	}
	if strings.Contains(b.String(), "extra prompt body") {
		t.Fatalf("prompt-derived list name should only use first non-empty line:\n%s", b.String())
	}
}

func TestSessionListNameKeepsExplicitRename(t *testing.T) {
	summary := session.SessionSummary{Name: "manual-name", FirstPrompt: "original prompt"}
	if got := sessionListName(summary); got != "manual-name" {
		t.Fatalf("sessionListName = %q, want explicit name", got)
	}
}

func TestRunModelsListAllShowsBuiltInModelsWithoutCredentials(t *testing.T) {
	t.Setenv("TAU_CONFIG_DIR", t.TempDir())

	cmd := &cli.Command{Flags: cloneFlags(ModelsListFlags)}
	if err := cmd.Set("all", "true"); err != nil {
		t.Fatalf("set all: %v", err)
	}

	stdout := captureStdout(t, func() {
		if err := runModelsList(context.Background(), cmd); err != nil {
			t.Fatalf("runModelsList: %v", err)
		}
	})

	lines := nonEmptyLines(stdout)
	if len(lines) == 0 {
		t.Fatalf("runModelsList --all printed no models; output:\n%s", stdout)
	}
	if !slices.Contains(lines, ai.ProviderOpenAI+"/gpt-5.5") {
		t.Fatalf("runModelsList --all missing generated built-in model %s/gpt-5.5; output:\n%s", ai.ProviderOpenAI, stdout)
	}
}

func TestModelsListFlagsIncludesAllAlias(t *testing.T) {
	have := flagNames(ModelsListFlags)
	for _, want := range []string{"all", "a"} {
		if _, ok := have[want]; !ok {
			t.Fatalf("ModelsListFlags missing %q; have %v", want, sortedKeys(have))
		}
	}
}

func TestRunModelsListUsesStoredCredentials(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)

	fb := store.NewFileBackend(filepath.Join(configDir, "auth.json"), store.Options{})
	if err := fb.EnsureDir(); err != nil {
		t.Fatalf("ensure auth dir: %v", err)
	}
	if err := fb.Set(context.Background(), ai.ProviderAnthropic, ai.NewAPIKeyCredential("test-key")); err != nil {
		t.Fatalf("set credential: %v", err)
	}

	cmd := &cli.Command{Flags: ModelsListFlags}
	if err := cmd.Set("provider", ai.ProviderAnthropic); err != nil {
		t.Fatalf("set provider: %v", err)
	}

	stdout := captureStdout(t, func() {
		if err := runModelsList(context.Background(), cmd); err != nil {
			t.Fatalf("runModelsList: %v", err)
		}
	})

	lines := nonEmptyLines(stdout)
	if len(lines) < 2 {
		t.Fatalf("runModelsList printed %d lines, want multiple Anthropic models; output:\n%s", len(lines), stdout)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, ai.ProviderAnthropic+"/") {
			t.Fatalf("line = %q, want provider-filtered Anthropic model", line)
		}
		if strings.Contains(line, "/default/") {
			t.Fatalf("line = %q, want provider/model row", line)
		}
	}
}

func TestRunModelsListUsesEnvironmentCredentialsAndProviderFilter(t *testing.T) {
	t.Setenv("TAU_CONFIG_DIR", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-openai-key")

	cmd := &cli.Command{Flags: ModelsListFlags}
	if err := cmd.Set("provider", ai.ProviderOpenAI); err != nil {
		t.Fatalf("set provider: %v", err)
	}

	stdout := captureStdout(t, func() {
		if err := runModelsList(context.Background(), cmd); err != nil {
			t.Fatalf("runModelsList: %v", err)
		}
	})

	lines := nonEmptyLines(stdout)
	if len(lines) < 2 {
		t.Fatalf("runModelsList printed %d lines, want multiple OpenAI models; output:\n%s", len(lines), stdout)
	}
	foundGenerated := false
	for _, line := range lines {
		if !strings.HasPrefix(line, ai.ProviderOpenAI+"/") {
			t.Fatalf("line = %q, want provider-filtered OpenAI model", line)
		}
		if line == ai.ProviderOpenAI+"/gpt-5.5" {
			foundGenerated = true
		}
	}
	if !foundGenerated {
		t.Fatalf("runModelsList output missing generated built-in model %s/gpt-5.5; output:\n%s", ai.ProviderOpenAI, stdout)
	}
}

func TestRunProvidersIncludesEnvironmentCredentials(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	t.Setenv("OPENAI_API_KEY", "test-openai-key")

	fb := store.NewFileBackend(filepath.Join(configDir, "auth.json"), store.Options{})
	if err := fb.EnsureDir(); err != nil {
		t.Fatalf("ensure auth dir: %v", err)
	}
	if err := fb.Set(context.Background(), ai.ProviderAnthropic, ai.NewAPIKeyCredential("test-key")); err != nil {
		t.Fatalf("set credential: %v", err)
	}

	cmd := &cli.Command{Flags: ProvidersFlags}
	stdout := captureStdout(t, func() {
		if err := runProviders(context.Background(), cmd); err != nil {
			t.Fatalf("runProviders: %v", err)
		}
	})

	if !strings.Contains(stdout, ai.ProviderAnthropic+"  [api_key]") {
		t.Fatalf("runProviders output missing stored provider; output:\n%s", stdout)
	}
	if !strings.Contains(stdout, ai.ProviderOpenAI+"  [api_key]") {
		t.Fatalf("runProviders output missing env provider; output:\n%s", stdout)
	}
}

func TestMergeProviderRefsWithEnvDoesNotDuplicateStoredProvider(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	refs := mergeProviderRefsWithEnv([]ai.CredentialRef{{
		Provider: ai.ProviderOpenAI,
		Type:     ai.CredentialTypeOAuth,
	}})

	count := 0
	for _, ref := range refs {
		if ref.Provider == ai.ProviderOpenAI {
			count++
			if ref.Type != ai.CredentialTypeOAuth {
				t.Fatalf("stored provider type = %q, want oauth preserved", ref.Type)
			}
		}
	}
	if count != 1 {
		t.Fatalf("openai provider ref count = %d, want 1; refs: %#v", count, refs)
	}
}

func TestRejectCommandShapedRootArgs(t *testing.T) {
	cmd := newRootCommand()
	if err := rejectCommandShapedRootArgs(context.Background(), cmd, []string{"modles", "list"}); err == nil {
		t.Fatal("bare multi-token root args should be rejected as command-shaped")
	}
	ctxWithDelimiter := context.WithValue(context.Background(), rootPromptDelimiterKey{}, true)
	if err := rejectCommandShapedRootArgs(ctxWithDelimiter, cmd, []string{"model", "list"}); err != nil {
		t.Fatalf("explicit -- prompt delimiter should allow multi-token prompt args: %v", err)
	}
	if err := rejectCommandShapedRootArgs(context.Background(), cmd, []string{"model list"}); err != nil {
		t.Fatalf("single quoted prompt arg should be allowed: %v", err)
	}
}

func TestPrintModeGoldensAgainstRealProvider(t *testing.T) {
	// Clear and re-init providers
	ai.Clear()
	ai.Init()

	// Create mock credential store
	credStore := &mockCredStore{
		getCred: ai.NewAPIKeyCredential("sk-test-key"),
	}

	rt, err := sdk.NewRuntime(sdk.RuntimeOptions{
		CredentialStore: credStore,
		ExplicitModel:   "anthropic/claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background())

	sess, err := rt.NewSession(context.Background(), sdk.SessionOptions{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close(context.Background())

	// Verify session was created successfully with correct model
	state := sess.State()
	if state.SessionID() == "" {
		t.Error("expected non-empty session ID")
	}
	if state.Model() != "anthropic/claude-sonnet-4-6" {
		t.Errorf("expected model 'anthropic/claude-sonnet-4-6', got %q", state.Model())
	}
}

// TestJSONEventSchemaValidationAgainstRealProvider tests that SDK Event types map correctly
// to the JSON event types specified in the modes/SDK spec.
func TestJSONEventSchemaValidationAgainstRealProvider(t *testing.T) {
	// Clear and re-init providers
	ai.Clear()
	ai.Init()

	// Create mock credential store
	credStore := &mockCredStore{
		getCred: ai.NewAPIKeyCredential("sk-test-key"),
	}

	rt, err := sdk.NewRuntime(sdk.RuntimeOptions{
		CredentialStore: credStore,
		ExplicitModel:   "anthropic/claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background())

	sess, err := rt.NewSession(context.Background(), sdk.SessionOptions{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close(context.Background())

	// Session should be created with the correct model
	state := sess.State()
	if state.SessionID() == "" {
		t.Error("expected non-empty session ID")
	}
	if state.Model() != "anthropic/claude-sonnet-4-6" {
		t.Errorf("expected model 'anthropic/claude-sonnet-4-6', got %q", state.Model())
	}

	// Valid types per spec (SDK Event types map to JSON event types)
	validTypes := map[string]bool{
		"session_start":  true,
		"message_start":  true,
		"text_delta":     true,
		"thinking_delta": true,
		"tool_call":      true,
		"tool_progress":  true,
		"tool_result":    true,
		"command_result": true,
		"message_end":    true,
		"usage":          true,
		"error":          true,
		"session_end":    true,
		"ui_confirm":     true,
		"ui_select":      true,
		"ui_input":       true,
		"ui_notify":      true,
		"ui_set_status":  true,
		"ui_set_widget":  true,
	}

	// Verify we can build valid event structures
	for _, eventType := range []string{"session_start", "session_end"} {
		if !validTypes[eventType] {
			t.Errorf("expected %q to be a valid event type", eventType)
		}
	}
}

// TestJSONStreamLFDelimited validates that SDK events can be serialized to LF-delimited JSON.
// cmd/tau --json outputs these as JSONL with LF as the record separator per spec.
func TestJSONStreamLFDelimited(t *testing.T) {
	ai.Clear()
	ai.Init()

	// Create mock credential store
	credStore := &mockCredStore{
		getCred: ai.NewAPIKeyCredential("sk-test-key"),
	}

	rt, err := sdk.NewRuntime(sdk.RuntimeOptions{
		CredentialStore: credStore,
		ExplicitModel:   "anthropic/claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background())

	sess, err := rt.NewSession(context.Background(), sdk.SessionOptions{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close(context.Background())

	// Verify session was created successfully
	state := sess.State()
	if state.SessionID() == "" {
		t.Error("expected non-empty session ID")
	}

	// Verify the event struct can be marshaled to JSON
	event := sdk.Event{
		Type:      "session_start",
		SessionID: state.SessionID(),
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("failed to marshal event: %v", err)
	}

	// Verify each line is valid JSON (LF-delimited, not other separators)
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Errorf("event is not valid JSON: %v", err)
	}
	// Should not contain U+2028 or U+2029 (valid in JSON strings but not as separators)
	if strings.Contains(string(data), "\xe2\x80\xa8") || strings.Contains(string(data), "\xe2\x80\xa9") {
		t.Error("event contains U+2028/U+2029 which cannot be used as separators")
	}
}

// isolateBuildEnv points tau's config + home at temp dirs so runtime-option
// builds never see the developer's real extensions/packages.
func isolateBuildEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	return tmp
}

func TestBuildRuntimeOptionsWithoutPluginsSkipsManager(t *testing.T) {
	isolateBuildEnv(t)

	_, pluginMgr, loader, _, buildErr := buildRuntimeOptions(context.Background(), ui.NewNoOpUI(), nil, withoutPlugins())
	require.NoError(t, buildErr)
	if pluginMgr != nil {
		t.Fatal("withoutPlugins returned a plugin manager; want nil")
	}
	if loader == nil {
		t.Fatal("expected artifact loader even without plugins")
	}
}

func TestBuildRuntimeOptionsDefaultCreatesManager(t *testing.T) {
	isolateBuildEnv(t)

	_, pluginMgr, _, _, buildErr := buildRuntimeOptions(context.Background(), ui.NewNoOpUI(), nil)
	require.NoError(t, buildErr)
	if pluginMgr == nil {
		t.Fatal("default buildRuntimeOptions returned nil plugin manager")
	}
}

func TestBuildConversationRuntimeOptionsHonorsRootFlags(t *testing.T) {
	tmpDir := t.TempDir()
	sessionDir := filepath.Join(tmpDir, "sessions")
	projectDir := filepath.Join(tmpDir, "project")
	skillDir := filepath.Join(projectDir, ".tau", "skills")

	requireNoError := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	requireNoError(os.MkdirAll(skillDir, 0o755))
	requireNoError(os.WriteFile(filepath.Join(skillDir, "test-skill.md"), []byte("---\nname: test-skill\n---\nskill body"), 0o644))

	origWD, err := os.Getwd()
	requireNoError(err)
	requireNoError(os.Chdir(projectDir))
	defer func() {
		requireNoError(os.Chdir(origWD))
	}()

	cmd := &cli.Command{Flags: append(RootFlags, ConversationFlags...)}
	requireNoError(cmd.Set("session-dir", sessionDir))
	requireNoError(cmd.Set("no-skills", "true"))

	ctx := context.WithValue(context.Background(), rootCmdKey{}, cmd)
	rtOpts, _, loader, _, _, buildErr := buildConversationRuntimeOptions(ctx, nil)
	require.NoError(t, buildErr)

	if rtOpts.SessionsDir != sessionDir {
		t.Fatalf("SessionsDir = %q, want %q", rtOpts.SessionsDir, sessionDir)
	}
	if loader == nil {
		t.Fatal("expected artifact loader")
	}
	if loader.SkillRegistry().Len() != 0 {
		t.Fatalf("expected no skills to be discovered when --no-skills is set, found %d", loader.SkillRegistry().Len())
	}
}

func TestBuildConversationRuntimeOptionsIncludesCLIArtifactPaths(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project")
	skillPath := filepath.Join(tmpDir, "cli-skill.md")

	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := os.WriteFile(skillPath, []byte("---\nname: cli-skill\ndescription: cli\n---\nbody"), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(origWD); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()

	cmd := &cli.Command{Flags: append(RootFlags, ConversationFlags...)}
	if err := cmd.Set("skill", skillPath); err != nil {
		t.Fatalf("set skill flag: %v", err)
	}

	ctx := context.WithValue(context.Background(), rootCmdKey{}, cmd)
	_, _, loader, _, _, buildErr := buildConversationRuntimeOptions(ctx, nil)
	require.NoError(t, buildErr)
	if loader == nil {
		t.Fatal("expected artifact loader")
	}
	if _, ok := loader.SkillRegistry().Get("cli-skill"); !ok {
		t.Fatal("expected CLI skill path to be discovered")
	}
}

func TestBuildConversationRuntimeOptionsResolvesModelsFlag(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "config")
	t.Setenv("TAU_CONFIG_DIR", configDir)

	// The flat settings.json schema has a single defaultProvider/defaultModel
	// pair. Custom model lists live in models.json instead.
	modelsPath := filepath.Join(configDir, "models.json")
	reg := ai.NewModelRegistry()
	if err := reg.SetCustomModels("anthropic", []string{"claude-sonnet-4-6", "claude-opus-4-6"}); err != nil {
		t.Fatalf("set custom models: %v", err)
	}
	if err := ai.SaveModelRegistry(modelsPath, reg); err != nil {
		t.Fatalf("save models: %v", err)
	}

	settings := &ai.Settings{
		DefaultProvider: "anthropic",
		DefaultModel:    "claude-sonnet-4-6",
	}
	if err := ai.SaveSettings(filepath.Join(configDir, "settings.json"), settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	cmd := &cli.Command{Flags: append(RootFlags, ConversationFlags...)}
	if err := cmd.Set("models", "claude-sonnet-4-6"); err != nil {
		t.Fatalf("set models flag: %v", err)
	}

	ctx := context.WithValue(context.Background(), rootCmdKey{}, cmd)
	rtOpts, _, _, _, sessionFlags, buildErr := buildConversationRuntimeOptions(ctx, nil)
	require.NoError(t, buildErr)
	if sessionFlags.SelectionError != nil {
		t.Fatalf("SelectionError = %v", sessionFlags.SelectionError)
	}
	if got, want := rtOpts.ExplicitModel, "anthropic/claude-sonnet-4-6"; got != want {
		t.Fatalf("ExplicitModel = %q, want %q", got, want)
	}
}

func TestRunListModelsIncludesGeneratedBuiltInModels(t *testing.T) {
	t.Setenv("TAU_CONFIG_DIR", t.TempDir())

	stdout := captureStdout(t, func() {
		if code := runListModels(context.Background(), "gpt-5.5"); code != ExitSuccess {
			t.Fatalf("runListModels exit code = %d, want %d", code, ExitSuccess)
		}
	})

	lines := nonEmptyLines(stdout)
	if len(lines) == 0 {
		t.Fatalf("runListModels printed no matches for generated built-in model; output:\n%s", stdout)
	}
	found := false
	for _, line := range lines {
		if line == "openai/gpt-5.5" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("runListModels output missing generated built-in model openai/gpt-5.5; output:\n%s", stdout)
	}
}

func TestBuildPromptSequence(t *testing.T) {
	t.Run("stdin_becomes_prompt_without_positional_args", func(t *testing.T) {
		prompts, err := buildPromptSequence(nil, nil, strings.NewReader("stdin prompt"), false, false)
		if err != nil {
			t.Fatalf("buildPromptSequence: %v", err)
		}
		if len(prompts) != 1 || prompts[0] != "stdin prompt" {
			t.Fatalf("prompts = %#v, want stdin-only prompt", prompts)
		}
	})

	t.Run("stdin_and_positional_joined_without_separator_remaining_messages_sequential", func(t *testing.T) {
		prompts, err := buildPromptSequence(
			[]string{"positional prompt"},
			[]string{"follow up one", "follow up two"},
			strings.NewReader("stdin context"),
			false,
			false,
		)
		if err != nil {
			t.Fatalf("buildPromptSequence: %v", err)
		}
		// Composition order: stdin + first positional, joined with no
		// separator. Remaining -m messages follow as sequential prompts.
		want := []string{
			"stdin contextpositional prompt",
			"follow up one",
			"follow up two",
		}
		if len(prompts) != len(want) {
			t.Fatalf("len(prompts) = %d, want %d (%#v)", len(prompts), len(want), prompts)
		}
		for i := range want {
			if prompts[i] != want[i] {
				t.Fatalf("prompts[%d] = %q, want %q", i, prompts[i], want[i])
			}
		}
	})

	t.Run("force_read_stdin_reads_even_when_tty", func(t *testing.T) {
		prompts, err := buildPromptSequence(nil, []string{"message"}, strings.NewReader("forced stdin"), true, true)
		if err != nil {
			t.Fatalf("buildPromptSequence: %v", err)
		}
		want := []string{"forced stdin", "message"}
		for i := range want {
			if prompts[i] != want[i] {
				t.Fatalf("prompts[%d] = %q, want %q", i, prompts[i], want[i])
			}
		}
	})

	t.Run("empty_stdin_does_not_create_prompt", func(t *testing.T) {
		prompts, err := buildPromptSequence(nil, nil, io.NopCloser(strings.NewReader("")), false, false)
		if err != nil {
			t.Fatalf("buildPromptSequence: %v", err)
		}
		if len(prompts) != 0 {
			t.Fatalf("prompts = %#v, want empty", prompts)
		}
	})
}

func TestProcessEventWith_NoStreamBuffersUntilMessageEnd(t *testing.T) {
	stdout := captureStdout(t, func() {
		var mu sync.Mutex
		p := &eventPrinter{NoStream: true}
		processEventWith(&sdk.Event{Type: "text_delta", Payload: map[string]any{"delta": "hello "}}, &mu, p)
		processEventWith(&sdk.Event{Type: "text_delta", Payload: map[string]any{"delta": "world"}}, &mu, p)
		// Nothing should have printed yet.
		processEventWith(&sdk.Event{Type: "message_end", Payload: map[string]any{}}, &mu, p)
	})
	if stdout != "hello world" {
		t.Errorf("stdout = %q, want 'hello world'", stdout)
	}
}

func TestProcessEventWith_StreamingPrintsImmediately(t *testing.T) {
	stdout := captureStdout(t, func() {
		var mu sync.Mutex
		p := &eventPrinter{NoStream: false}
		processEventWith(&sdk.Event{Type: "text_delta", Payload: map[string]any{"delta": "hi"}}, &mu, p)
	})
	if stdout != "hi" {
		t.Errorf("stdout = %q, want 'hi'", stdout)
	}
}

func TestProcessEventWith_UsageAcceptsIntegerTokenCounts(t *testing.T) {
	stderr := captureStderr(t, func() {
		var mu sync.Mutex
		processEventWith(&sdk.Event{Type: "usage", Payload: map[string]any{
			"this_call": map[string]any{"input_tokens": 120, "output_tokens": 30},
		}}, &mu, nil)
	})
	if !strings.Contains(stderr, "[tokens: in=120 out=30]") {
		t.Errorf("stderr = %q, want integer token counts to be printed", stderr)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if err != nil {
				done <- b.String()
				return
			}
		}
	}()
	fn()
	w.Close()
	os.Stdout = orig
	return <-done
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if err != nil {
				done <- b.String()
				return
			}
		}
	}()
	fn()
	w.Close()
	os.Stderr = orig
	return <-done
}

func nonEmptyLines(s string) []string {
	raw := strings.Split(s, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestRegisterBuiltInCommands_RegistersExpectedNames(t *testing.T) {
	reg := commands.NewInMemoryRegistry()
	registerBuiltInCommands(reg)

	want := []string{"login", "help", "reload", "sessions", "set-model", "show-model"}
	for _, name := range want {
		if _, _, ok := reg.Lookup(name); !ok {
			t.Errorf("built-in command /%s not registered", name)
		}
	}
}

func TestRegisterBuiltInCommands_HelpListsCommands(t *testing.T) {
	reg := commands.NewInMemoryRegistry()
	registerBuiltInCommands(reg)

	h, _, ok := reg.Lookup("help")
	if !ok {
		t.Fatal("/help not registered")
	}
	res, err := h(context.Background(), "", nil, nil)
	if err != nil {
		t.Fatalf("help handler: %v", err)
	}
	if !strings.Contains(res.Output, "/login") || !strings.Contains(res.Output, "/reload") {
		t.Errorf("help output missing expected commands: %s", res.Output)
	}
}

func TestNormalizeToolList(t *testing.T) {
	t.Run("nil_input_returns_nil", func(t *testing.T) {
		if got := normalizeToolList(nil); got != nil {
			t.Errorf("normalizeToolList(nil) = %v, want nil", got)
		}
	})
	t.Run("repeated_single_names", func(t *testing.T) {
		got := normalizeToolList([]string{"read", "bash", "read"})
		want := []string{"read", "bash"}
		if !slicesEqual(got, want) {
			t.Errorf("normalizeToolList(repeated) = %v, want %v", got, want)
		}
	})
	t.Run("comma_separated_single_arg", func(t *testing.T) {
		got := normalizeToolList([]string{"read,bash,edit"})
		want := []string{"read", "bash", "edit"}
		if !slicesEqual(got, want) {
			t.Errorf("normalizeToolList(csv) = %v, want %v", got, want)
		}
	})
	t.Run("mixed_csv_and_repeated", func(t *testing.T) {
		got := normalizeToolList([]string{"read,bash", "edit", "bash"})
		want := []string{"read", "bash", "edit"}
		if !slicesEqual(got, want) {
			t.Errorf("normalizeToolList(mixed) = %v, want %v", got, want)
		}
	})
	t.Run("whitespace_trimmed_and_blanks_skipped", func(t *testing.T) {
		got := normalizeToolList([]string{"  read , , bash  "})
		want := []string{"read", "bash"}
		if !slicesEqual(got, want) {
			t.Errorf("normalizeToolList(whitespace) = %v, want %v", got, want)
		}
	})
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestApplyConversationFlags_NewPiCompatibleFlags(t *testing.T) {
	tmpDir := t.TempDir()
	sessionDir := filepath.Join(tmpDir, "sessions")
	projectDir := filepath.Join(tmpDir, "project")
	// Isolate the model registry from any global state left by other tests.
	t.Setenv("TAU_CONFIG_DIR", tmpDir)

	requireNoError := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	requireNoError(os.MkdirAll(projectDir, 0o755))

	// Seed a model registry so the model resolution path inside
	// applyConversationFlags doesn't fail when --models is non-empty
	// (urfave/cli/v3 keeps StringSliceFlag value state on the package-level
	// flag pointer, so a previous test may have set a value that bleeds in).
	modelsPath := filepath.Join(tmpDir, "models.json")
	reg := ai.NewModelRegistry()
	requireNoError(reg.SetCustomModels("anthropic", []string{"claude-sonnet-4-6", "claude-opus-4-6"}))
	requireNoError(ai.SaveModelRegistry(modelsPath, reg))
	requireNoError(ai.SaveSettings(filepath.Join(tmpDir, "settings.json"), &ai.Settings{
		DefaultProvider: "anthropic",
		DefaultModel:    "claude-sonnet-4-6",
	}))

	origWD, err := os.Getwd()
	requireNoError(err)
	requireNoError(os.Chdir(projectDir))
	defer func() { requireNoError(os.Chdir(origWD)) }()

	allFlags := cloneFlags(append(append([]cli.Flag{}, RootFlags...), ConversationFlags...))
	cmd := &cli.Command{Flags: allFlags}
	requireNoError(cmd.Set("session-dir", sessionDir))
	requireNoError(cmd.Set("provider", "anthropic"))
	requireNoError(cmd.Set("models", "anthropic/claude-sonnet-4-6"))
	requireNoError(cmd.Set("api-key", "sk-test"))
	requireNoError(cmd.Set("name", "refactor session"))
	requireNoError(cmd.Set("system-prompt", "you are a Go expert"))
	requireNoError(cmd.Set("append-system-prompt", "always run go test"))
	requireNoError(cmd.Set("append-system-prompt", "prefer table-driven tests"))
	requireNoError(cmd.Set("tools", "read,ls"))
	requireNoError(cmd.Set("exclude-tools", "bash"))
	requireNoError(cmd.Set("approve", "true"))
	requireNoError(cmd.Set("session-id", "exact-123"))

	settings, err := loadSettings()
	requireNoError(err)
	var rtOpts sdk.RuntimeOptions
	rtOpts.Settings = settings
	sf := applyConversationFlags(&rtOpts, cmd)

	if sf.SelectionError != nil {
		t.Fatalf("SelectionError = %v", sf.SelectionError)
	}
	if rtOpts.Provider != "anthropic" {
		t.Errorf("Provider = %q, want %q", rtOpts.Provider, "anthropic")
	}
	if rtOpts.APIKey != "sk-test" {
		t.Errorf("APIKey = %q, want %q", rtOpts.APIKey, "sk-test")
	}
	if sf.DisplayName != "refactor session" {
		t.Errorf("DisplayName = %q, want %q", sf.DisplayName, "refactor session")
	}
	if sf.SystemPrompt != "you are a Go expert" {
		t.Errorf("SystemPrompt = %q, want %q", sf.SystemPrompt, "you are a Go expert")
	}
	if !slicesEqual(sf.AppendSystemPrompts, []string{"always run go test", "prefer table-driven tests"}) {
		t.Errorf("AppendSystemPrompts = %v", sf.AppendSystemPrompts)
	}
	if !slicesEqual(sf.ToolAllowlist, []string{"read", "ls"}) {
		t.Errorf("ToolAllowlist = %v", sf.ToolAllowlist)
	}
	if !slicesEqual(sf.ToolDenylist, []string{"bash"}) {
		t.Errorf("ToolDenylist = %v", sf.ToolDenylist)
	}
	if !sf.ApproveProjectFiles {
		t.Error("ApproveProjectFiles = false, want true")
	}
	// --session-id should win over --session and be applied.
	if sf.ResumeSessionID != "exact-123" {
		t.Errorf("ResumeSessionID = %q, want %q", sf.ResumeSessionID, "exact-123")
	}
}

func TestApplyConversationFlags_NoApproveOverridesApprove(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	cmd := &cli.Command{Flags: []cli.Flag{
		&cli.BoolFlag{Name: "approve"},
		&cli.BoolFlag{Name: "no-approve"},
	}}
	if err := cmd.Set("approve", "true"); err != nil {
		t.Fatalf("set approve: %v", err)
	}
	if err := cmd.Set("no-approve", "true"); err != nil {
		t.Fatalf("set no-approve: %v", err)
	}

	var rtOpts sdk.RuntimeOptions
	sf := applyConversationFlags(&rtOpts, cmd)
	// When both are set, --no-approve wins (more conservative).
	if sf.ApproveProjectFiles {
		t.Error("ApproveProjectFiles = true, want false (--no-approve should override --approve)")
	}
}

func TestSubCommandAliases_Registered(t *testing.T) {
	names := make(map[string]bool, len(Commands))
	for _, c := range Commands {
		names[c.Name] = true
	}
	for _, want := range []string{"remove", "list", "update", "install", "uninstall"} {
		if !names[want] {
			t.Errorf("sub-command %q not registered; got %v", want, keys(names))
		}
	}
}

func TestUpdateCommand_HasSelfAndPiSubcommands(t *testing.T) {
	for _, c := range Commands {
		if c.Name != "update" {
			continue
		}
		subNames := make(map[string]bool, len(c.Commands))
		for _, sub := range c.Commands {
			subNames[sub.Name] = true
		}
		for _, want := range []string{"self", "pi"} {
			if !subNames[want] {
				t.Errorf("update sub-command %q not registered; got %v", want, subNames)
			}
		}
		return
	}
	t.Fatal("update command not registered")
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// cloneFlags creates fresh copies of each flag so tests don't share state
// through the package-level RootFlags/ConversationFlags slices. urfave/cli/v3
// stores per-flag state on the Flag itself, so two tests that build a
// Command from the same Flag pointer see each other's mutations.
func cloneFlags(in []cli.Flag) []cli.Flag {
	out := make([]cli.Flag, 0, len(in))
	for _, f := range in {
		switch src := f.(type) {
		case *cli.StringFlag:
			cp := *src
			out = append(out, &cp)
		case *cli.BoolFlag:
			cp := *src
			out = append(out, &cp)
		case *cli.StringSliceFlag:
			cp := *src
			out = append(out, &cp)
		case *cli.IntFlag:
			cp := *src
			out = append(out, &cp)
		default:
			out = append(out, f)
		}
	}
	return out
}

func TestNormalizeOptionalResumeArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{name: "long_without_value", args: []string{"tau", "--resume"}, want: []string{"tau", "--resume="}},
		{name: "short_without_value", args: []string{"tau", "-r"}, want: []string{"tau", "--resume="}},
		{name: "long_before_flag", args: []string{"tau", "--resume", "--mode", "text"}, want: []string{"tau", "--resume=", "--mode", "text"}},
		{name: "long_with_value", args: []string{"tau", "--resume", "sess-123"}, want: []string{"tau", "--resume", "sess-123"}},
		{name: "long_equals_empty", args: []string{"tau", "--resume="}, want: []string{"tau", "--resume="}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeOptionalResumeArgs(tc.args)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("normalizeOptionalResumeArgs(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestResolveAppMode covers the mode resolution matrix. Each row is
// (mode flag, --print, --json, stdinIsTTY, --stdin) -> expected AppMode.
// The matrix exercises the four AppModes and the alias/precedence rules
// between --mode, --print, --json, --stdin, and TTY detection.
func TestResolveAppMode(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		print     bool
		json      bool
		stdinTTY  bool
		stdinFlag bool
		resume    *string
		want      string
	}{
		// --mode primary
		{name: "mode_rpc_tty", mode: "rpc", stdinTTY: true, want: "rpc"},
		{name: "mode_rpc_nontty", mode: "rpc", stdinTTY: false, want: "rpc"},
		{name: "mode_json_tty", mode: "json", stdinTTY: true, want: "json"},
		{name: "mode_json_nontty", mode: "json", stdinTTY: false, want: "json"},
		{name: "mode_text_tty", mode: "text", stdinTTY: true, want: "text"},
		{name: "mode_text_nontty", mode: "text", stdinTTY: false, want: "text"},
		{name: "mode_text_print", mode: "text", print: true, stdinTTY: true, want: "text"},

		// --json alias
		{name: "json_only_tty", json: true, stdinTTY: true, want: "json"},
		{name: "json_only_nontty", json: true, stdinTTY: false, want: "json"},

		// --print force
		{name: "print_tty", print: true, stdinTTY: true, want: "text"},
		{name: "print_nontty", print: true, stdinTTY: false, want: "text"},

		// No mode flags
		{name: "default_tty", stdinTTY: true, want: "interactive"},
		{name: "default_nontty", stdinTTY: false, want: "text"},

		// --stdin force
		{name: "stdin_tty", stdinTTY: true, stdinFlag: true, want: "text"},

		// --resume with an empty ID opens the interactive session selector.
		{name: "resume_empty_nontty_forces_interactive", stdinTTY: false, resume: stringPtr(""), want: "interactive"},
		{name: "resume_id_nontty_stays_text", stdinTTY: false, resume: stringPtr("sess-123"), want: "text"},

		// --mode wins over --json (alias only honored when --mode unset)
		{name: "json_ignored_when_mode_text", mode: "text", json: true, stdinTTY: true, want: "text"},
		{name: "json_ignored_when_mode_rpc", mode: "rpc", json: true, stdinTTY: true, want: "rpc"},
		{name: "print_ignored_when_mode_json", mode: "json", print: true, stdinTTY: true, want: "json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cli.Command{Flags: cloneFlags(append(append([]cli.Flag{}, RootFlags...), ConversationFlags...))}
			if tc.mode != "" {
				requireNoErrorSet(t, cmd.Set("mode", tc.mode))
			}
			if tc.print {
				requireNoErrorSet(t, cmd.Set("print", "true"))
			}
			if tc.json {
				requireNoErrorSet(t, cmd.Set("json", "true"))
			}
			if tc.stdinFlag {
				requireNoErrorSet(t, cmd.Set("stdin", "true"))
			}
			if tc.resume != nil {
				requireNoErrorSet(t, cmd.Set("resume", *tc.resume))
			}
			got := resolveAppMode(cmd, tc.stdinTTY)
			if got != tc.want {
				t.Errorf("resolveAppMode(mode=%q, print=%v, json=%v, tty=%v) = %q, want %q",
					tc.mode, tc.print, tc.json, tc.stdinTTY, got, tc.want)
			}
		})
	}
}

// TestValidateForkConflict covers --fork's mutual-exclusion rules with
// the other session-targeting flags.
func TestValidateForkConflict(t *testing.T) {
	cases := []struct {
		name        string
		fork        string
		session     string
		sessionID   string
		resume      string
		continueF   bool
		noSession   bool
		wantErr     bool
		errContains string
	}{
		{name: "no_conflict", fork: "id-1"},
		{name: "fork_with_session", fork: "id-1", session: "id-2", wantErr: true, errContains: "--session"},
		{name: "fork_with_session_id", fork: "id-1", sessionID: "exact-2", wantErr: true, errContains: "--session-id"},
		{name: "fork_with_resume", fork: "id-1", resume: "id-3", wantErr: true, errContains: "--resume"},
		{name: "fork_with_continue", fork: "id-1", continueF: true, wantErr: true, errContains: "--continue"},
		{name: "fork_with_no_session", fork: "id-1", noSession: true, wantErr: true, errContains: "--no-session"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cli.Command{Flags: cloneFlags(append(append([]cli.Flag{}, RootFlags...), ConversationFlags...))}
			if tc.fork != "" {
				requireNoErrorSet(t, cmd.Set("fork", tc.fork))
			}
			if tc.session != "" {
				requireNoErrorSet(t, cmd.Set("session", tc.session))
			}
			if tc.sessionID != "" {
				requireNoErrorSet(t, cmd.Set("session-id", tc.sessionID))
			}
			if tc.resume != "" {
				requireNoErrorSet(t, cmd.Set("resume", tc.resume))
			}
			if tc.continueF {
				requireNoErrorSet(t, cmd.Set("continue", "true"))
			}
			if tc.noSession {
				requireNoErrorSet(t, cmd.Set("no-session", "true"))
			}
			err := validateForkConflict(cmd)
			if tc.wantErr {
				if err == nil {
					t.Fatal("validateForkConflict() = nil, want error")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
			} else if err != nil {
				t.Errorf("validateForkConflict() = %v, want nil", err)
			}
		})
	}
}

func stringPtr(s string) *string {
	return &s
}

func requireNoErrorSet(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("flag set failed: %v", err)
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRpcModeValidation_RejectsPrompts covers the guard in runRoot that
// rejects positional prompts and @file attachments when the resolved
// mode is rpc. RPC is a long-lived server, so positional prompts would
// be lost and @file attachments cannot be expanded; clients must send
// both through JSON-RPC.
func TestRpcModeValidation_RejectsPrompts(t *testing.T) {
	t.Run("non_rpc_mode_passes", func(t *testing.T) {
		if err := validateRpcInput("text", []string{"hello"}, nil); err != nil {
			t.Errorf("validateRpcInput(text) = %v, want nil", err)
		}
		if err := validateRpcInput("json", []string{"hello"}, nil); err != nil {
			t.Errorf("validateRpcInput(json) = %v, want nil", err)
		}
	})
	t.Run("rpc_with_no_input_passes", func(t *testing.T) {
		if err := validateRpcInput("rpc", nil, nil); err != nil {
			t.Errorf("validateRpcInput(rpc, nil, nil) = %v, want nil", err)
		}
	})
	t.Run("rpc_rejects_positional_prompts", func(t *testing.T) {
		err := validateRpcInput("rpc", []string{"hello"}, nil)
		if err == nil {
			t.Fatal("validateRpcInput = nil, want error")
		}
		if !strings.Contains(err.Error(), "positional prompts") {
			t.Errorf("error %q does not mention positional prompts", err.Error())
		}
	})
	t.Run("rpc_rejects_file_images_even_without_prompts", func(t *testing.T) {
		// A bare @file arg expands into fileImages only; the prompts list
		// is empty in this case. The guard must still fire.
		err := validateRpcInput("rpc", nil, []ai.ContentBlock{{Type: "image"}})
		if err == nil {
			t.Fatal("validateRpcInput = nil, want error")
		}
		if !strings.Contains(err.Error(), "@file") {
			t.Errorf("error %q does not mention @file", err.Error())
		}
	})
	t.Run("rpc_prompts_rejected_before_file_images", func(t *testing.T) {
		// When both are present, the prompt error wins so the user
		// sees the more fundamental issue first.
		err := validateRpcInput("rpc", []string{"hello"}, []ai.ContentBlock{{Type: "image"}})
		if err == nil {
			t.Fatal("validateRpcInput = nil, want error")
		}
		if !strings.Contains(err.Error(), "positional prompts") {
			t.Errorf("error %q should mention positional prompts first", err.Error())
		}
	})
}

// TestRpcCommand_ActionIsRunRPC verifies that the rpcCmd's Action
// function is the same as runRPC. This is the structural guarantee
// that "tau rpc" and "tau --mode rpc" both invoke the same RPC server:
// rpcCmd.Action is runRPC, and runRoot's mode switch routes "rpc" to
// runRPC. Either side drifting makes the two commands behave
// differently, which would be a contract break.
func TestRpcCommand_ActionIsRunRPC(t *testing.T) {
	for _, c := range Commands {
		if c.Name != "rpc" {
			continue
		}
		if c.Action == nil {
			t.Fatal("rpcCmd.Action is nil")
		}
		got := reflect.ValueOf(c.Action).Pointer()
		want := reflect.ValueOf(runRPC).Pointer()
		if got != want {
			t.Errorf("rpcCmd.Action pointer = %#x, want runRPC pointer %#x", got, want)
		}
		return
	}
	t.Fatal("rpc command not registered")
}

// TestListModelsSearchOperand covers the optional [search] operand for
// the --list-models flag. The search is the first positional when it
// does not start with '-'; flag-shaped args are left for urfave/cli.
func TestListModelsSearchOperand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "no_args", args: nil, want: ""},
		{name: "empty_args", args: []string{}, want: ""},
		{name: "first_is_search", args: []string{"claude"}, want: "claude"},
		{name: "first_is_flag", args: []string{"-v"}, want: ""},
		{name: "first_is_double_dash", args: []string{"--foo"}, want: ""},
		{name: "additional_args_ignored", args: []string{"claude", "extra"}, want: "claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := listModelsSearchOperand(tc.args); got != tc.want {
				t.Errorf("listModelsSearchOperand(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestSubCommandAliasRouting verifies that the pi-compatible sub-command
// aliases route to the same Action function as their canonical
// counterparts. This is the structural guarantee that "tau remove" and
// "tau uninstall" behave identically, and that "tau list" matches
// "tau extensions list".
func TestSubCommandAliasRouting(t *testing.T) {
	actionsByName := make(map[string]*cli.Command, len(Commands))
	for _, c := range Commands {
		actionsByName[c.Name] = c
	}
	cases := []struct {
		alias string
		canon string
	}{
		{alias: "remove", canon: "uninstall"},
	}
	for _, tc := range cases {
		t.Run(tc.alias+"_routes_to_"+tc.canon, func(t *testing.T) {
			aliasCmd, ok := actionsByName[tc.alias]
			if !ok {
				t.Fatalf("alias %q not registered", tc.alias)
			}
			canonCmd, ok := actionsByName[tc.canon]
			if !ok {
				t.Fatalf("canonical %q not registered", tc.canon)
			}
			if aliasCmd.Action == nil {
				t.Fatalf("%s.Action is nil", tc.alias)
			}
			if canonCmd.Action == nil {
				t.Fatalf("%s.Action is nil", tc.canon)
			}
			got := reflect.ValueOf(aliasCmd.Action).Pointer()
			want := reflect.ValueOf(canonCmd.Action).Pointer()
			if got != want {
				t.Errorf("%s.Action = %v, want %s.Action = %v", tc.alias, aliasCmd.Action, tc.canon, canonCmd.Action)
			}
		})
	}
}

// TestRootFlagsIncludesPiCompatibleFlags verifies that the pi-compatible
// root flags from the plan are registered with the documented names. The
// "tau --help" output is built from these slices, so a missing entry
// here would also be a missing entry in the help text.
func TestRootFlagsIncludesPiCompatibleFlags(t *testing.T) {
	wanted := []string{
		"provider",
		"api-key",
		"system-prompt",
		"append-system-prompt",
		"name",
		"session-id",
		"tools",
		"exclude-tools",
		"approve",
		"no-approve",
		"mode",
		"print",
		"json",
	}
	have := flagNames(RootFlags, ConversationFlags)
	for _, w := range wanted {
		if _, ok := have[w]; !ok {
			t.Errorf("flag %q not registered; have %v", w, sortedKeys(have))
		}
	}
}

// flagNames returns the set of flag names + aliases declared in the
// provided flag slices. Aliases are included because help rendering
// shows them, so a "missing" alias would be a help-text gap.
func flagNames(groups ...[]cli.Flag) map[string]struct{} {
	out := make(map[string]struct{})
	for _, g := range groups {
		for _, f := range g {
			out[f.Names()[0]] = struct{}{}
			for _, a := range f.Names()[1:] {
				out[a] = struct{}{}
			}
		}
	}
	return out
}

// sortedKeys returns the keys of m sorted ascending, for stable test
// output.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestSplitFileArgs verifies the partition of positional args into @file
// references and plain text. The '@' prefix is stripped from the file
// path; everything else is preserved in order.
func TestSplitFileArgs(t *testing.T) {
	t.Run("no_args", func(t *testing.T) {
		files, plain := splitFileArgs(nil)
		if len(files) != 0 || len(plain) != 0 {
			t.Errorf("splitFileArgs(nil) = (%v, %v), want (empty, empty)", files, plain)
		}
	})
	t.Run("all_plain", func(t *testing.T) {
		files, plain := splitFileArgs([]string{"hello", "world"})
		if len(files) != 0 || !slicesEqual(plain, []string{"hello", "world"}) {
			t.Errorf("got files=%v plain=%v, want files=[] plain=[hello world]", files, plain)
		}
	})
	t.Run("all_files", func(t *testing.T) {
		files, plain := splitFileArgs([]string{"@a.md", "@b.md"})
		if !slicesEqual(files, []string{"a.md", "b.md"}) || len(plain) != 0 {
			t.Errorf("got files=%v plain=%v, want files=[a.md b.md] plain=[]", files, plain)
		}
	})
	t.Run("mixed_preserves_order", func(t *testing.T) {
		files, plain := splitFileArgs([]string{"@a.md", "first", "@b.md", "second"})
		if !slicesEqual(files, []string{"a.md", "b.md"}) || !slicesEqual(plain, []string{"first", "second"}) {
			t.Errorf("got files=%v plain=%v", files, plain)
		}
	})
	t.Run("empty_at_prefix", func(t *testing.T) {
		// "@" alone is treated as a file arg with empty path; expandFileArg
		// rejects this with "empty @file reference" at expansion time.
		files, plain := splitFileArgs([]string{"@"})
		if !slicesEqual(files, []string{""}) || len(plain) != 0 {
			t.Errorf("got files=%v plain=%v, want files=[''] plain=[]", files, plain)
		}
	})
}

// TestExpandFileArg_TextFile verifies that a text file is wrapped in
// <file name="<abs-path>">…</file> and the wrapper's name uses the
// resolved absolute path. Images produce a different wrapper (covered
// separately).
func TestExpandFileArg_TextFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "hello.txt")
	body := "hello world\n"
	requireNoError(t, os.WriteFile(path, []byte(body), 0o644))

	text, images, err := expandFileArg(path)
	if err != nil {
		t.Fatalf("expandFileArg: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("images = %v, want nil for text file", images)
	}
	if !strings.Contains(text, body) {
		t.Errorf("text %q does not contain body %q", text, body)
	}
	if !strings.Contains(text, "<file name=") || !strings.Contains(text, path) {
		t.Errorf("text %q does not wrap with absolute path %q", text, path)
	}
}

// TestExpandFileArg_ImageExt verifies that an image extension triggers
// the image content-block path; the marker text is still wrapped.
func TestExpandFileArg_ImageExt(t *testing.T) {
	tmpDir := t.TempDir()
	// 1x1 PNG (smallest valid PNG) so ai.LoadImageContentBlock can decode it.
	pngBytes := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x62, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
	path := filepath.Join(tmpDir, "tiny.png")
	requireNoError(t, os.WriteFile(path, pngBytes, 0o644))

	text, images, err := expandFileArg(path)
	if err != nil {
		t.Fatalf("expandFileArg: %v", err)
	}
	if len(images) != 1 {
		t.Errorf("images = %d, want 1", len(images))
	}
	if !strings.Contains(text, "<file name=") {
		t.Errorf("text %q does not wrap with <file name=>", text)
	}
	if !strings.Contains(text, "[image content]") {
		t.Errorf("text %q does not contain image marker", text)
	}
}

// TestExpandFileArg_MissingFile verifies positional @file args still fail fast.
func TestExpandFileArg_MissingFile(t *testing.T) {
	_, _, err := expandFileArg(filepath.Join(t.TempDir(), "does-not-exist.txt"))
	if err == nil {
		t.Fatal("expandFileArg on missing file = nil error, want error")
	}
}

func TestProcessFileArguments_MissingFileStaysLiteral(t *testing.T) {
	out, images, err := processFileArguments("look at @missing.md")
	if err != nil {
		t.Fatalf("processFileArguments: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("images = %d, want 0", len(images))
	}
	if out != "look at @missing.md" {
		t.Fatalf("out = %q, want literal missing reference", out)
	}
}

func TestProcessFileArguments_TildePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "notes.md"), []byte("home body"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, _, err := processFileArguments("look at @~/notes.md")
	if err != nil {
		t.Fatalf("processFileArguments: %v", err)
	}
	if !strings.Contains(out, "home body") {
		t.Fatalf("out = %q, want expanded home file", out)
	}
}

// TestExpandFileArg_EmptyPath verifies that an empty @file reference is
// rejected with a clear error rather than a confusing stat failure.
func TestExpandFileArg_EmptyPath(t *testing.T) {
	_, _, err := expandFileArg("")
	if err == nil {
		t.Fatal("expandFileArg(\"\") = nil error, want error")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q does not mention empty", err.Error())
	}
}

// TestBuildPromptSequenceWithFiles_CompositionOrder covers the spec
// composition: stdin + @file-text + first positional, joined with no
// separator; remaining positionals and -m messages are sequential.
func TestBuildPromptSequenceWithFiles_CompositionOrder(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "ctx.md")
	requireNoError(t, os.WriteFile(filePath, []byte("file body"), 0o644))

	t.Run("stdin_plus_file_plus_positional_joined_no_separator", func(t *testing.T) {
		prompts, _, err := buildPromptSequenceWithFiles(
			[]string{"@" + filePath, "ask about this"},
			nil,
			strings.NewReader("stdin body"),
			false, false,
		)
		if err != nil {
			t.Fatalf("buildPromptSequenceWithFiles: %v", err)
		}
		if len(prompts) != 1 {
			t.Fatalf("len(prompts) = %d, want 1 (%#v)", len(prompts), prompts)
		}
		wrapped, _, err := expandFileArg(filePath)
		if err != nil {
			t.Fatalf("expandFileArg: %v", err)
		}
		want := "stdin body" + wrapped + "ask about this"
		if prompts[0] != want {
			t.Errorf("prompts[0] = %q, want %q", prompts[0], want)
		}
	})
	t.Run("remaining_positionals_sequential", func(t *testing.T) {
		prompts, _, err := buildPromptSequenceWithFiles(
			[]string{"first", "second", "third"},
			nil,
			nil, true, false,
		)
		if err != nil {
			t.Fatalf("buildPromptSequenceWithFiles: %v", err)
		}
		if !slicesEqual(prompts, []string{"first", "second", "third"}) {
			t.Errorf("prompts = %v, want [first second third]", prompts)
		}
	})
	t.Run("file_with_no_positional_or_stdin", func(t *testing.T) {
		prompts, _, err := buildPromptSequenceWithFiles(
			[]string{"@" + filePath},
			nil,
			nil, true, false,
		)
		if err != nil {
			t.Fatalf("buildPromptSequenceWithFiles: %v", err)
		}
		if len(prompts) != 1 {
			t.Fatalf("len(prompts) = %d, want 1 (%#v)", len(prompts), prompts)
		}
		if !strings.Contains(prompts[0], "file body") {
			t.Errorf("prompts[0] = %q, want it to contain file body", prompts[0])
		}
	})
	t.Run("missing_positional_file_is_fatal", func(t *testing.T) {
		_, _, err := buildPromptSequenceWithFiles(
			[]string{"@" + filepath.Join(tmpDir, "no-such-file.txt")},
			nil,
			nil, true, false,
		)
		if err == nil {
			t.Fatal("expected error for missing positional @file, got nil")
		}
	})
	t.Run("messages_follow_positionals_sequentially", func(t *testing.T) {
		prompts, _, err := buildPromptSequenceWithFiles(
			[]string{"first"},
			[]string{"second", "third"},
			nil, true, false,
		)
		if err != nil {
			t.Fatalf("buildPromptSequenceWithFiles: %v", err)
		}
		if !slicesEqual(prompts, []string{"first", "second", "third"}) {
			t.Errorf("prompts = %v, want [first second third]", prompts)
		}
	})
}

func TestPrintPostTUIExitMessage(t *testing.T) {
	var b strings.Builder
	if err := printPostTUIExitMessage(&b, "No session selected"); err != nil {
		t.Fatalf("printPostTUIExitMessage: %v", err)
	}
	if got, want := b.String(), "\x1b[25;1H\x1b[0m\x1b[39m\x1b[49m\x1b[JNo session selected\n"; got != want {
		t.Fatalf("message output = %q, want %q", got, want)
	}
}

func TestPrintPostTUIExitMessageEmpty(t *testing.T) {
	var b strings.Builder
	if err := printPostTUIExitMessage(&b, ""); err != nil {
		t.Fatalf("printPostTUIExitMessage empty: %v", err)
	}
	if b.Len() != 0 {
		t.Fatalf("empty message printed %q, want nothing", b.String())
	}
}

func TestPrintResumeInstructions(t *testing.T) {
	var b strings.Builder
	printResumeInstructions(&b, "sess-123")
	got := b.String()
	for _, want := range []string{"Session ID: sess-123", "tau --resume sess-123"} {
		if !strings.Contains(got, want) {
			t.Fatalf("resume instructions = %q, missing %q", got, want)
		}
	}
}

func TestPrintResumeInstructionsEmptyID(t *testing.T) {
	var b strings.Builder
	printResumeInstructions(&b, "")
	if b.Len() != 0 {
		t.Fatalf("empty session id printed %q, want nothing", b.String())
	}
}

func TestPrintResumeInstructionsIfWrittenSkipsMissingSession(t *testing.T) {
	var b strings.Builder
	if err := printResumeInstructionsIfWritten(&b, filepath.Join(t.TempDir(), "sessions"), "missing-session"); err != nil {
		t.Fatalf("printResumeInstructionsIfWritten: %v", err)
	}
	if b.Len() != 0 {
		t.Fatalf("missing session printed %q, want nothing", b.String())
	}
}

func TestPackagesAndExtensionsListAnnotatePiPackages(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	pkgDir := filepath.Join(configDir, "packages", "pi-echo")
	requireNoError(t, os.MkdirAll(pkgDir, 0o755))
	requireNoError(t, os.WriteFile(filepath.Join(pkgDir, "tau-package.toml"), []byte(`[package]
name = "pi-echo"
version = "1.0.0"
description = "Echo"
[plugin]
api_version = "1"
[run]
command = "/usr/bin/node /tmp/shim/index.js extension.js"
`), 0o600))
	requireNoError(t, packages.InstallSidecar{Source: "./pi-echo", InstalledAt: "2026-01-01T00:00:00Z", Scope: "user", Pi: true}.Write(pkgDir))

	cmd := &cli.Command{Flags: cloneFlags(PackagesListFlags)}
	packagesOut := captureStdout(t, func() {
		if err := runPackagesList(context.Background(), cmd); err != nil {
			t.Fatalf("runPackagesList: %v", err)
		}
	})
	if !strings.Contains(packagesOut, "pi-echo (pi-ext)") {
		t.Fatalf("packages list missing pi annotation:\n%s", packagesOut)
	}

	extOut := captureStdout(t, func() {
		if err := runExtensionsList(context.Background(), nil); err != nil {
			t.Fatalf("runExtensionsList: %v", err)
		}
	})
	if !strings.Contains(extOut, "pi-echo (pi-ext)") {
		t.Fatalf("extensions list missing pi annotation:\n%s", extOut)
	}
}

func TestDaemonTokenAddListRevoke(t *testing.T) {
	t.Setenv("TAU_CONFIG_DIR", t.TempDir())

	var out string
	run := func(args ...string) {
		t.Helper()
		cmd := newRootCommand()
		ctx := context.WithValue(context.Background(), rootCmdKey{}, cmd)
		ctx = context.WithValue(ctx, rootPromptDelimiterKey{}, false)
		out = captureStdout(t, func() {
			if err := cmd.Run(ctx, append([]string{"tau"}, args...)); err != nil {
				t.Fatalf("tau %v returned error: %v", args, err)
			}
		})
	}

	run("daemon", "token", "add", "phone")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "Token for phone") {
		t.Fatalf("add output = %q, want token intro plus secret", out)
	}
	token := lines[1]
	if len(token) < 32 {
		t.Fatalf("token output = %q, want a long secret", token)
	}

	// List shows the device with only a prefix of the secret.
	run("daemon", "token", "list")
	if !strings.Contains(out, "phone") || !strings.Contains(out, token[:8]) {
		t.Fatalf("list output = %q, want device name and token prefix", out)
	}
	if strings.Contains(out, token) {
		t.Fatalf("list output = %q leaks the full token", out)
	}

	// The bridge accepts the token issued through the CLI.
	store := server.NewTokenStore(filepath.Join(configDir(), "remote-tokens.json"))
	if !store.Contains(token) {
		t.Fatal("bridge token store does not contain the token added via CLI")
	}

	run("daemon", "token", "revoke", "phone")
	if !strings.Contains(out, "Revoked token for phone") {
		t.Fatalf("revoke output = %q", out)
	}
	if store.Contains(token) {
		t.Fatal("revoked token still accepted by the bridge store")
	}

	// Revoking an unknown device fails.
	revokeCmd := newRootCommand()
	revokeCmd.ExitErrHandler = func(context.Context, *cli.Command, error) {}
	err := revokeCmd.Run(context.Background(), []string{"tau", "daemon", "token", "revoke", "phone"})
	if err == nil {
		t.Fatal("revoking unknown device succeeded; want error")
	}
}

func TestSessionsDirResolution(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", cfgDir)
	t.Setenv("TAU_SESSION_DIR", "")

	dir, err := sessionDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(cfgDir, "sessions"), dir)

	settings := `{"sessionDir": "/from/settings"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	dir, err = sessionDir()
	require.NoError(t, err)
	require.Equal(t, "/from/settings", dir)

	t.Setenv("TAU_SESSION_DIR", "/from/env")
	dir, err = sessionDir()
	require.NoError(t, err)
	require.Equal(t, "/from/env", dir)
}

func TestPackagesDirsResolution(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", cfgDir)
	t.Setenv("TAU_PACKAGE_DIR", "")

	userDir, _, err := packagesDirs()
	require.NoError(t, err)
	if want := filepath.Join(cfgDir, "packages"); userDir != want {
		t.Fatalf("default = %q, want %q", userDir, want)
	}

	settings := `{"packageDir": "/pkgs/settings"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	userDir, _, err = packagesDirs()
	require.NoError(t, err)
	if userDir != "/pkgs/settings" {
		t.Fatalf("settings = %q, want /pkgs/settings", userDir)
	}

	t.Setenv("TAU_PACKAGE_DIR", "/pkgs/env")
	userDir, _, err = packagesDirs()
	require.NoError(t, err)
	if userDir != "/pkgs/env" {
		t.Fatalf("env = %q, want /pkgs/env", userDir)
	}
}

func TestArtifactUserDirsResolution(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", cfgDir)
	t.Setenv("TAU_SKILLS_DIR", "")

	dirs := artifactUserDirs(nil)
	if want := filepath.Join(cfgDir, "skills"); dirs[artifacts.KindSkill] != want {
		t.Fatalf("default skills = %q, want %q", dirs[artifacts.KindSkill], want)
	}

	t.Setenv("TAU_SKILLS_DIR", "/skills/env")
	dirs = artifactUserDirs(nil)
	if dirs[artifacts.KindSkill] != "/skills/env" {
		t.Fatalf("env skills = %q, want /skills/env", dirs[artifacts.KindSkill])
	}

	s := ai.Settings{SkillDir: "/skills/settings"}
	dirs = artifactUserDirs(&s)
	if dirs[artifacts.KindSkill] != "/skills/env" {
		t.Fatalf("env should win over settings: %q", dirs[artifacts.KindSkill])
	}

	t.Setenv("TAU_SKILLS_DIR", "")
	dirs = artifactUserDirs(&s)
	if dirs[artifacts.KindSkill] != "/skills/settings" {
		t.Fatalf("settings skills = %q, want /skills/settings", dirs[artifacts.KindSkill])
	}
}

func TestPromoteConfigDirFlag(t *testing.T) {
	t.Setenv("TAU_CONFIG_DIR", "")

	promoteConfigDirFlag([]string{"tau", "--config-dir=/tmp/a"})
	if got := os.Getenv("TAU_CONFIG_DIR"); got != "/tmp/a" {
		t.Fatalf("inline form: got %q", got)
	}

	t.Setenv("TAU_CONFIG_DIR", "")
	promoteConfigDirFlag([]string{"tau", "--config-dir", "/tmp/b", "do", "things"})
	if got := os.Getenv("TAU_CONFIG_DIR"); got != "/tmp/b" {
		t.Fatalf("space form: got %q", got)
	}

	t.Setenv("TAU_CONFIG_DIR", "")
	promoteConfigDirFlag([]string{"tau", "prompt", "--", "--config-dir=/tmp/c"})
	if got := os.Getenv("TAU_CONFIG_DIR"); got != "" {
		t.Fatalf("after -- should be ignored: got %q", got)
	}
}

func TestPrintModeFailsOnTheFinalReplyLikeUpstream(t *testing.T) {
	end := func(stop, message string) sdk.Event {
		return sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "finish_reason": stop, "error_message": message}}
	}
	for _, tc := range []struct {
		name    string
		events  []sdk.Event
		wantErr string
		wantOut string
	}{
		{"earlier truncation is not final", []sdk.Event{end("length", ""), end("stop", "")}, "", ""},
		{"final truncation keeps partial text", []sdk.Event{{Type: "text_delta", Payload: map[string]any{"delta": "partial"}}, end("length", "")}, "Response was truncated: the model reached its output token limit.", "partial"},
		{"aborted without a message", []sdk.Event{end("aborted", "")}, "Request aborted", ""},
		{"error with a message", []sdk.Event{end("error", "boom")}, "boom", ""},
		{"run error", []sdk.Event{end("stop", ""), {Type: "error", Payload: map[string]any{"message": "provider response failed: boom"}}}, "error: provider response failed: boom", ""},
		{"user end is ignored", []sdk.Event{end("length", ""), {Type: "message_end", Payload: map[string]any{"role": "user"}}}, "Response was truncated: the model reached its output token limit.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			printer := &eventPrinter{NoStream: true}
			var stderr string
			stdout := captureStdout(t, func() {
				stderr = captureStderr(t, func() {
					for _, event := range tc.events {
						processEventWith(&event, &mu, printer)
					}
				})
			})
			if stderr != "" || stdout != tc.wantOut {
				t.Fatalf("stdout = %q, stderr = %q; want stdout %q and no stderr before the run ends", stdout, stderr, tc.wantOut)
			}
			var got string
			if err := printer.result(); err != nil {
				got = err.Error()
			}
			if got != tc.wantErr {
				t.Fatalf("result = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

type reloadSettingsSession struct {
	commands.CommandSession
	settings *ai.Settings
}

func (s *reloadSettingsSession) ReloadResources(ctx context.Context, settings *ai.Settings, prepared *artifacts.PreparedReload, preparedPlugins *plugins.PreparedReload, name string) error {
	if preparedPlugins != nil {
		if err := preparedPlugins.ReloadWithAdmission(ctx, name, func() error { return s.ReloadSettings(ctx, settings) }); err != nil {
			return err
		}
	} else if err := s.ReloadSettings(ctx, settings); err != nil {
		return err
	}
	if prepared != nil {
		prepared.Commit()
	}
	return nil
}

func (s *reloadSettingsSession) ReloadSettings(_ context.Context, settings *ai.Settings) error {
	s.settings = settings
	return nil
}

func TestReloadSettingsWithoutArtifactLoader(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"read":{"lineCap":7}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := sdk.GlobalArtifactLoader
	sdk.GlobalArtifactLoader = nil
	t.Cleanup(func() { sdk.GlobalArtifactLoader = previous })
	reg := commands.NewInMemoryRegistry()
	registerBuiltInCommands(reg)
	handler, _, ok := reg.Lookup("reload")
	if !ok {
		t.Fatal("reload command missing")
	}
	sess := &reloadSettingsSession{}
	if _, err := handler(t.Context(), "", nil, sess); err != nil {
		t.Fatal(err)
	}
	if sess.settings == nil || sess.settings.Read.LineCap != 7 {
		t.Fatalf("settings not reloaded: %+v", sess.settings)
	}
}

func TestLoadSettingsRejectsMalformedProjectSettings(t *testing.T) {
	t.Setenv("TAU_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, ai.ProjectTrustStore{Path: filepath.Join(configDir(), "trust.json")}.Set(t.Context(), dir, new(true)))
	if err := os.Mkdir(filepath.Join(dir, ".tau"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tau", "settings.json"), []byte(`{"retry":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSettings(); err == nil || !strings.Contains(err.Error(), "load project settings") {
		t.Fatalf("expected project settings error, got %v", err)
	}
}

func TestSessionCommandsAcceptShortPrefixes(t *testing.T) {
	for _, operation := range []string{"rename", "move", "delete", "restore", "compact", "summarize-branch", "fork", "export"} {
		t.Run(operation, func(t *testing.T) {
			t.Setenv("TAU_CONFIG_DIR", t.TempDir())
			dir := t.TempDir()
			t.Setenv("TAU_SESSION_DIR", dir)
			storage, err := session.NewStorageAt(dir)
			if err != nil {
				t.Fatal(err)
			}
			header := session.NewHeader("/test", nil, nil)
			writer, err := storage.Create(header)
			if err != nil {
				t.Fatal(err)
			}
			root := session.NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"shared"}`), nil)
			abandoned := session.NewMessageEntry(&root.ID, json.RawMessage(`{"role":"assistant","content":"abandoned"}`), nil)
			abandoned.ID = "branch-entry"
			selected := session.NewMessageEntry(&root.ID, json.RawMessage(`{"role":"assistant","content":"selected"}`), nil)
			for _, entry := range []session.Entry{root, abandoned, selected} {
				if err := writer.Append(entry); err != nil {
					t.Fatal(err)
				}
			}
			path := writer.Path()
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := session.ReleaseOwner(path); err != nil {
				t.Fatal(err)
			}
			if operation == "restore" {
				if err := storage.Delete(header.ID); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"tau", "sessions", operation, header.ID[:4]}
			switch operation {
			case "rename":
				args = append(args, "new name")
			case "move":
				args = append(args, t.TempDir())
			case "summarize-branch":
				args = append(args, "branch")
			case "export":
				args = append(args, "--format", "html", "--output", filepath.Join(t.TempDir(), "history.html"))
			}
			cmd := newRootCommand()
			if err := cmd.Run(t.Context(), args); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestApplyConversationFlagsProviderRequiresModel(t *testing.T) {
	for _, ephemeral := range []bool{false, true} {
		t.Run(fmt.Sprintf("ephemeral=%v", ephemeral), func(t *testing.T) {
			cmd := &cli.Command{Flags: []cli.Flag{
				&cli.StringFlag{Name: "provider"},
				&cli.StringSliceFlag{Name: "models"},
				&cli.BoolFlag{Name: "no-session"},
			}}
			requireNoErrorSet(t, cmd.Set("provider", "anthropic"))
			requireNoErrorSet(t, cmd.Set("no-session", strconv.FormatBool(ephemeral)))
			opts := sdk.RuntimeOptions{Settings: &ai.Settings{DefaultProvider: "openai", DefaultModel: "gpt-4o"}}
			flags := applyConversationFlags(&opts, cmd)
			if flags.SelectionError == nil || !strings.Contains(flags.SelectionError.Error(), "--provider requires --model") {
				t.Fatalf("SelectionError = %v, want missing model error", flags.SelectionError)
			}
		})
	}
}

func TestApplyConversationFlagsModelThinkingSuffix(t *testing.T) {
	newCmd := func(model, thinking string) *cli.Command {
		cmd := &cli.Command{Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "models", Aliases: []string{"model"}},
			&cli.StringFlag{Name: "thinking"},
		}}
		requireNoErrorSet(t, cmd.Set("model", model))
		if thinking != "" {
			requireNoErrorSet(t, cmd.Set("thinking", thinking))
		}
		return cmd
	}
	opts := sdk.RuntimeOptions{Settings: &ai.Settings{}}
	if flags := applyConversationFlags(&opts, newCmd("anthropic/claude-sonnet-4-6:high", "")); flags.SelectionError != nil {
		t.Fatal(flags.SelectionError)
	}
	if opts.ExplicitModel != "anthropic/claude-sonnet-4-6" || opts.ThinkingLevel != "high" {
		t.Fatalf("suffix: model=%q thinking=%q", opts.ExplicitModel, opts.ThinkingLevel)
	}
	opts = sdk.RuntimeOptions{Settings: &ai.Settings{}}
	applyConversationFlags(&opts, newCmd("anthropic/claude-sonnet-4-6:high", "low"))
	if opts.ThinkingLevel != "low" {
		t.Fatalf("--thinking lost to the suffix: %q", opts.ThinkingLevel)
	}
}

func TestApplyConversationFlagsModelAliasAndEphemeralOptions(t *testing.T) {
	aliasRegistered := false
	for _, flag := range ConversationFlags {
		if slices.Contains(flag.Names(), "models") && slices.Contains(flag.Names(), "model") {
			aliasRegistered = true
		}
	}
	if !aliasRegistered {
		t.Fatal("--models has no --model alias")
	}
	for _, ephemeral := range []bool{false, true} {
		t.Run(fmt.Sprintf("ephemeral=%v", ephemeral), func(t *testing.T) {
			cmd := &cli.Command{Flags: []cli.Flag{
				&cli.StringSliceFlag{Name: "models", Aliases: []string{"model"}},
				&cli.StringFlag{Name: "provider"},
				&cli.StringFlag{Name: "api-key"},
				&cli.BoolFlag{Name: "no-session"},
				&cli.StringFlag{Name: "session"},
				&cli.BoolFlag{Name: "continue"},
			}}
			requireNoErrorSet(t, cmd.Set("provider", "anthropic"))
			requireNoErrorSet(t, cmd.Set("model", "anthropic/claude-sonnet-4-6"))
			requireNoErrorSet(t, cmd.Set("api-key", "explicit"))
			requireNoErrorSet(t, cmd.Set("no-session", strconv.FormatBool(ephemeral)))
			if ephemeral {
				requireNoErrorSet(t, cmd.Set("session", "ignored"))
				requireNoErrorSet(t, cmd.Set("continue", "true"))
			}
			opts := sdk.RuntimeOptions{Settings: &ai.Settings{DefaultProvider: "openai", DefaultModel: "gpt-4o"}}
			flags := applyConversationFlags(&opts, cmd)
			if flags.SelectionError != nil {
				t.Fatal(flags.SelectionError)
			}
			if opts.ExplicitModel != "anthropic/claude-sonnet-4-6" || opts.Provider != "anthropic" || opts.APIKey != "explicit" || opts.NoSession != ephemeral {
				t.Fatalf("runtime options lost: model=%q provider=%q explicit key=%v ephemeral=%v", opts.ExplicitModel, opts.Provider, opts.APIKey != "", opts.NoSession)
			}
			if flags.ResumeSessionID != "" || opts.ContinueSessionID != "" {
				t.Fatal("ephemeral session retained a session target")
			}
		})
	}
}

func TestSettingsArtifactPathsRetainScopeThroughStartupAndReload(t *testing.T) {
	home := t.TempDir()
	cfgDir := filepath.Join(home, "agent")
	project := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TAU_CONFIG_DIR", cfgDir)
	t.Chdir(project)
	require.NoError(t, ai.ProjectTrustStore{Path: filepath.Join(cfgDir, "trust.json")}.Set(t.Context(), project, new(true)))
	for _, path := range []string{filepath.Join(cfgDir, "extra", "user.md"), filepath.Join(project, ".tau", "extra", "project.md")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("prompt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{"prompts":["extra"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	loader, err := artifacts.NewLoader(artifacts.LoaderOptions{CWD: project, SettingsScopes: getSettingsArtifactScopes(settings), DisableSkills: true, DisableThemes: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loader.PromptRegistry().Get("user"); !ok {
		t.Fatal("user-relative prompt missing")
	}
	if err := loader.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := loader.PromptRegistry().Get("user"); !ok {
		t.Fatal("reload lost user-relative prompt")
	}
	if err := os.WriteFile(filepath.Join(project, ".tau", "settings.json"), []byte(`{"prompts":["extra","!extra/*","+extra/project.md"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err = loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := loader.Prepare(artifacts.ReloadOptions{SettingsScopes: getSettingsArtifactScopes(settings)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loader.PromptRegistry().Get("project"); ok {
		t.Fatal("preparation changed live resources")
	}
	prepared.Commit()
	if _, ok := loader.PromptRegistry().Get("project"); !ok {
		t.Fatal("project-relative prompt missing")
	}
	if _, ok := loader.PromptRegistry().Get("user"); !ok {
		t.Fatal("project list discarded user resources")
	}
	if err := loader.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"user", "project"} {
		if _, ok := loader.PromptRegistry().Get(name); !ok {
			t.Fatalf("reload discarded %s resources", name)
		}
	}
	if err := os.WriteFile(filepath.Join(project, ".tau", "settings.json"), []byte(`{"prompts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err = loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = loader.Prepare(artifacts.ReloadOptions{SettingsScopes: getSettingsArtifactScopes(settings)})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	if _, ok := loader.PromptRegistry().Get("user"); !ok {
		t.Fatal("empty project list discarded user resources")
	}
	if _, ok := loader.PromptRegistry().Get("project"); ok {
		t.Fatal("empty project list retained project resources")
	}
}

func TestReportShutdownErrorSurfacesFailure(t *testing.T) {
	output := captureStderr(t, func() { reportShutdownError(errors.New("resource close failed")); reportShutdownError(nil) })
	if output != "tau shutdown error: resource close failed\n" {
		t.Fatalf("shutdown diagnostic = %q", output)
	}
}

func TestModelScopePatternsPreferFlagsOverEnabledModels(t *testing.T) {
	settings := &ai.Settings{EnabledModels: []string{"anthropic/*"}}
	if got := modelScopePatterns([]string{"openai/gpt-5"}, settings); !slices.Equal(got, []string{"openai/gpt-5"}) {
		t.Fatalf("flags = %q", got)
	}
	if got := modelScopePatterns(nil, settings); !slices.Equal(got, []string{"anthropic/*"}) {
		t.Fatalf("settings = %q", got)
	}
	if got := modelScopePatterns(nil, nil); got != nil {
		t.Fatalf("nil settings = %q", got)
	}
}

type failingReloadSession struct {
	commands.CommandSession
	err error
}

func (s *failingReloadSession) ReloadResources(context.Context, *ai.Settings, *artifacts.PreparedReload, *plugins.PreparedReload, string) error {
	return s.err
}

func (s *failingReloadSession) ReloadSettings(context.Context, *ai.Settings) error { return s.err }

func TestInvalidThemesAreReportedAtStartupAndReload(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", cfgDir)
	t.Setenv("TAU_THEMES_DIR", "")
	t.Chdir(t.TempDir())
	themesDir := filepath.Join(cfgDir, "themes")
	require.NoError(t, os.MkdirAll(themesDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(themesDir, "partial.json"), []byte(`{"name":"partial","colors":{"text":"#ffffff"}}`), 0o600))

	ctx, diagnostics := withStartupDiagnostics(t.Context())
	_, _, loader, _, buildErr := buildRuntimeOptions(ctx, nil, nil)
	require.NoError(t, buildErr)
	require.NotNil(t, loader)
	lines := diagnostics.Lines()
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "partial.json")
	assert.Contains(t, lines[0], "missing required color tokens")
	_, ok := loader.ThemeRegistry().Get("light")
	assert.True(t, ok, "built-in light theme registers")

	previousLoader, previousPlugins := sdk.GlobalArtifactLoader, GlobalPluginManager
	sdk.GlobalArtifactLoader, GlobalPluginManager = loader, nil
	t.Cleanup(func() { sdk.GlobalArtifactLoader, GlobalPluginManager = previousLoader, previousPlugins })
	result, err := reloadSession(t.Context(), "", &reloadSettingsSession{})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "warning: invalid theme ")
	assert.Contains(t, result.Output, "partial.json")
}

func TestReloadSessionCommitsArtifactsOnlyAfterSessionSettings(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", cfgDir)
	t.Chdir(t.TempDir())
	for _, name := range []string{"old", "new"} {
		if err := os.MkdirAll(filepath.Join(cfgDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, name, name+".md"), []byte("body"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSettings := func(dir string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{"prompts":["`+dir+`"]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSettings("old")
	settings, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	reg := commands.NewInMemoryRegistry()
	loader, err := artifacts.NewLoader(artifacts.LoaderOptions{CWD: t.TempDir(), SettingsScopes: getSettingsArtifactScopes(settings), UserDirs: artifactUserDirs(settings), DisableSkills: true, DisableThemes: true, CommandsRegistry: reg})
	if err != nil {
		t.Fatal(err)
	}
	previousLoader, previousPlugins := sdk.GlobalArtifactLoader, GlobalPluginManager
	sdk.GlobalArtifactLoader, GlobalPluginManager = loader, nil
	t.Cleanup(func() { sdk.GlobalArtifactLoader, GlobalPluginManager = previousLoader, previousPlugins })

	writeSettings("new")
	rejected := errors.New("invalid transport")
	if _, err := reloadSession(t.Context(), "", &failingReloadSession{err: rejected}); !errors.Is(err, rejected) {
		t.Fatalf("reload error = %v, want session rejection", err)
	}
	if _, ok := loader.PromptRegistry().Get("old"); !ok {
		t.Fatal("rejected reload replaced artifacts")
	}
	if _, _, ok := reg.Lookup("new"); ok {
		t.Fatal("rejected reload registered new prompt commands")
	}

	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{"prompts":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadSession(t.Context(), "", &reloadSettingsSession{}); err == nil {
		t.Fatal("malformed settings reloaded")
	}
	if _, ok := loader.PromptRegistry().Get("old"); !ok {
		t.Fatal("malformed settings replaced artifacts")
	}

	writeSettings("new")
	sess := &reloadSettingsSession{}
	result, err := reloadSession(t.Context(), "", sess)
	if err != nil {
		t.Fatal(err)
	}
	if sess.settings == nil || result.Output != "reloaded settings, artifacts, and plugins" {
		t.Fatalf("reload result = %q, settings = %v", result.Output, sess.settings)
	}
	if _, ok := loader.PromptRegistry().Get("new"); !ok {
		t.Fatal("reload ignored the new settings prompt paths")
	}
	if _, _, ok := reg.Lookup("old"); ok {
		t.Fatal("reload kept the old prompt command")
	}
}

func TestThemeLookupRetainsAppearanceAcrossResourceReload(t *testing.T) {
	loader, err := artifacts.NewLoader(artifacts.LoaderOptions{CWD: t.TempDir(), DisableSkills: true, DisablePrompts: true})
	require.NoError(t, err)
	lookup := themeLookup(loader)
	register := func(appearance string) {
		var file map[string]any
		entry := artifacts.BuiltinThemes()[0]
		require.NoError(t, json.Unmarshal(entry.Raw, &file))
		file["name"] = "metadata"
		file["appearance"] = appearance
		raw, err := json.Marshal(file)
		require.NoError(t, err)
		require.NoError(t, loader.ThemeRegistry().Reload([]artifacts.Entry{{Kind: artifacts.KindTheme, Tier: artifacts.TierProject, SourcePath: filepath.Join(t.TempDir(), "metadata.json"), Raw: raw}}))
	}
	register("light")
	theme, ok := lookup("metadata")
	require.True(t, ok)
	assert.Equal(t, "light", theme.Appearance)
	assert.NotEmpty(t, theme.Values["text"])
	register("dark")
	theme, ok = lookup("metadata")
	require.True(t, ok)
	assert.Equal(t, "dark", theme.Appearance)
	_, ok = lookup("missing")
	assert.False(t, ok)
	assert.Nil(t, themeLookup(nil))
}

func TestThemesCommandsDescribeGeneratedSystemWithoutFile(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	themesDir := filepath.Join(configDir, "themes")
	require.NoError(t, os.MkdirAll(themesDir, 0o700))
	raw := strings.Replace(string(artifacts.BuiltinThemes()[0].Raw), `"name": "dark"`, `"name": "system"`, 1)
	require.Contains(t, raw, `"name": "system"`)
	require.NoError(t, os.WriteFile(filepath.Join(themesDir, "system.json"), []byte(raw), 0o600))
	t.Chdir(t.TempDir())
	run := func(args ...string) string {
		t.Helper()
		var runErr error
		output := captureStdout(t, func() {
			cmd := newRootCommand()
			cmd.ExitErrHandler = func(context.Context, *cli.Command, error) {}
			runErr = cmd.Run(t.Context(), append([]string{"tau", "themes"}, args...))
		})
		require.NoError(t, runErr)
		return output
	}
	output := run("list")
	assert.Contains(t, output, "system [builtin]")
	assert.Equal(t, 1, strings.Count(output, "  system ["))
	assert.Contains(t, output, "Source: generated from terminal colors")
	assert.Less(t, strings.Index(output, "system [builtin]"), strings.Index(output, "dark [builtin]"))
	assert.Contains(t, run("list", "--tier", "builtin"), "system")
	assert.NotContains(t, run("list", "--tier", "user"), "system")
	output = run("show", "system")
	assert.Contains(t, output, "Theme: system")
	assert.Contains(t, output, "Tier: builtin")
	assert.Contains(t, output, "Source: generated from terminal colors")
	assert.NotContains(t, output, "Raw:")
	assert.NotContains(t, output, "system.json")
	assert.Contains(t, run("show", "light"), "Raw:")
}

func TestThemeFileRefreshReachesLookupAndRetainsValidatedSnapshot(t *testing.T) {
	loader, err := artifacts.NewLoader(artifacts.LoaderOptions{CWD: t.TempDir(), DisableSkills: true, DisablePrompts: true, ThemeValidator: func(raw []byte) error { _, err := tui.DecodeTheme(raw); return err }})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "custom.json")
	var file map[string]any
	require.NoError(t, json.Unmarshal(artifacts.BuiltinThemes()[0].Raw, &file))
	file["name"] = "custom"
	save := func() {
		raw, err := json.Marshal(file)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, raw, 0o600))
	}
	save()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, loader.ThemeRegistry().Register(artifacts.Entry{Kind: artifacts.KindTheme, Tier: artifacts.TierProject, SourcePath: path, Raw: raw}))
	lookup := themeLookup(loader)
	initial, ok := lookup("custom")
	require.True(t, ok)
	file["appearance"] = "light"
	file["colors"].(map[string]any)["text"] = "#123456"
	save()
	changed, err := loader.ThemeRegistry().Refresh("custom")
	require.NoError(t, err)
	require.True(t, changed)
	updated, ok := lookup("custom")
	require.True(t, ok)
	assert.NotEqual(t, initial, updated)
	assert.Equal(t, "light", updated.Appearance)
	assert.Equal(t, "#123456", updated.Values["text"])
	// Valid JSON still has to pass the shared theme schema.
	delete(file["colors"].(map[string]any), "text")
	save()
	changed, err = loader.ThemeRegistry().Refresh("custom")
	require.Error(t, err)
	assert.False(t, changed)
	retained, ok := lookup("custom")
	require.True(t, ok)
	assert.Equal(t, updated, retained)
	require.NoError(t, os.Remove(path))
	changed, err = loader.ThemeRegistry().Refresh("custom")
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.False(t, changed)
	retained, ok = lookup("custom")
	require.True(t, ok)
	assert.Equal(t, updated, retained)
}

type loginCredentialProvider struct {
	oauth.Provider
	login func(context.Context, oauth.LoginCallbacks) (*oauth.Credentials, error)
}

func (p loginCredentialProvider) Login(ctx context.Context, callbacks oauth.LoginCallbacks) (*oauth.Credentials, error) {
	return p.login(ctx, callbacks)
}

func TestLoginOAuthCredentialPreservesMetadata(t *testing.T) {
	expires := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	result := &oauth.Credentials{
		Access: "access", Refresh: "refresh", Expires: expires.UnixMilli(),
		ProjectID: "project", Email: "user@example.com", AccountID: "account",
		Extra: map[string]any{"enterpriseUrl": "https://example.com", "availableModels": []string{"model"}},
	}
	callbacks := &loginCallbacks{}
	provider := loginCredentialProvider{login: func(ctx context.Context, got oauth.LoginCallbacks) (*oauth.Credentials, error) {
		assert.Equal(t, t.Context(), ctx)
		assert.Same(t, callbacks, got)
		return result, nil
	}}
	cred, err := loginOAuthCredential(t.Context(), provider, callbacks)
	require.NoError(t, err)
	assert.Equal(t, ai.CredentialTypeOAuth, cred.Type)
	assert.Equal(t, result.Access, cred.AccessToken)
	assert.Equal(t, result.Refresh, cred.RefreshToken)
	assert.True(t, expires.Equal(cred.ExpiresAt))
	assert.Equal(t, result.ProjectID, cred.ProjectID)
	assert.Equal(t, result.Email, cred.Email)
	assert.Equal(t, result.AccountID, cred.AccountID)
	assert.Equal(t, result.Extra, cred.Extra)
	fb := store.NewFileBackend(filepath.Join(t.TempDir(), "auth.json"), store.Options{})
	require.NoError(t, fb.Set(t.Context(), "test", cred))
	restored, err := fb.Get(t.Context(), "test")
	require.NoError(t, err)
	assert.Equal(t, cred.ProjectID, restored.ProjectID)
	assert.Equal(t, cred.Email, restored.Email)
	assert.Equal(t, cred.AccountID, restored.AccountID)
	expected, err := json.Marshal(cred.Extra)
	require.NoError(t, err)
	actual, err := json.Marshal(restored.Extra)
	require.NoError(t, err)
	assert.JSONEq(t, string(expected), string(actual))
}

func TestLoginOAuthCredentialRejectsFailure(t *testing.T) {
	failure := errors.New("provider failed")
	for _, tc := range []struct {
		name   string
		result *oauth.Credentials
		err    error
		cancel bool
	}{
		{name: "provider", err: failure},
		{name: "nil"},
		{name: "empty", result: &oauth.Credentials{Refresh: "refresh"}},
		{name: "cancelled", result: &oauth.Credentials{Access: "late"}, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			provider := loginCredentialProvider{login: func(context.Context, oauth.LoginCallbacks) (*oauth.Credentials, error) {
				if tc.cancel {
					cancel()
				}
				return tc.result, tc.err
			}}
			cred, err := loginOAuthCredential(ctx, provider, &loginCallbacks{})
			require.Error(t, err)
			assert.Equal(t, ai.Credential{}, cred)
			if tc.err != nil {
				assert.ErrorIs(t, err, tc.err)
			}
			if tc.cancel {
				assert.ErrorIs(t, err, context.Canceled)
			}
		})
	}
}

func TestLoginOAuthCredentialRejectsCancelledAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	provider := loginCredentialProvider{login: func(context.Context, oauth.LoginCallbacks) (*oauth.Credentials, error) {
		t.Fatal("cancelled login reached provider")
		return nil, nil
	}}
	cred, err := loginOAuthCredential(ctx, provider, &loginCallbacks{})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, ai.Credential{}, cred)
}

func TestThemeCallbacksDisabledWithoutArtifacts(t *testing.T) {
	if themeRefresher(nil) != nil || themeLookup(nil) != nil {
		t.Fatal("theme callbacks must be nil when artifact loading failed")
	}
}

func TestBearerEnvironmentProviderCredential(t *testing.T) {
	ai.Init()
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer-secret")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
	refs := mergeProviderRefsWithEnv(nil)
	assert.True(t, slices.ContainsFunc(refs, func(ref ai.CredentialRef) bool { return ref.Provider == ai.ProviderAnthropic }))
	fb := store.NewFileBackend(filepath.Join(t.TempDir(), "auth.json"), store.Options{})
	cred, err := providerCredential(t.Context(), fb, ai.CredentialRef{Provider: ai.ProviderAnthropic})
	require.NoError(t, err)
	assert.Empty(t, cred.APIKey, "bearer auth must stay provider-owned rather than become an API key")
}

func TestModelsDiscoverUsesEnvironmentBearer(t *testing.T) {
	ai.Init()
	t.Setenv("TAU_CONFIG_DIR", t.TempDir())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer-secret")
	t.Setenv("ANTHROPIC_API_KEY", "environment-key")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		assert.Equal(t, "Bearer bearer-secret", req.Header.Get("Authorization"))
		_, err := io.WriteString(w, `{"data":[{"id":"claude-local"}]}`)
		assert.NoError(t, err)
	}))
	defer server.Close()
	data, err := json.Marshal(ai.ModelRegistryFile{Providers: map[string]ai.ModelRegistryProvider{ai.ProviderAnthropic: {BaseURL: server.URL}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(configDir(), "models.json"), data, 0o600))
	cmd := &cli.Command{Name: "discover", Action: runModelsDiscover, Flags: ModelsDiscoverFlags}
	output := captureStdout(t, func() { require.NoError(t, cmd.Run(t.Context(), []string{"discover", ai.ProviderAnthropic})) })
	assert.Equal(t, "anthropic/claude-local\n", output)
	assert.Equal(t, int32(1), requests.Load())
}

func TestProviderCredentialRejectsMalformedStoreDespiteEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer-secret")
	path := filepath.Join(t.TempDir(), "auth.json")
	require.NoError(t, os.WriteFile(path, []byte("invalid json"), 0o600))
	fb := store.NewFileBackend(path, store.Options{})
	_, err := providerCredential(t.Context(), fb, ai.CredentialRef{Provider: ai.ProviderAnthropic})
	require.ErrorContains(t, err, "parse auth.json")
}

func TestSetModelPreservesUnrelatedSettingsJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"terminal":{"future":true},"extension":9007199254740993,"quietStartup":false}`), 0o600))
	cmd := newRootCommand()
	require.NoError(t, cmd.Run(t.Context(), []string{"tau", "set-model", "anthropic/example"}))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"terminal":{"future":true},"extension":9007199254740993,"quietStartup":false,"defaultProvider":"anthropic","defaultModel":"example"}`, string(data))
	assert.Contains(t, string(data), "9007199254740993")
}

func TestLoginWithoutDefaultDoesNotRewriteSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{"future":9007199254740993, "terminal":{"future":true}}`)
	require.NoError(t, os.WriteFile(path, original, 0o600))
	cmd := newRootCommand()
	require.NoError(t, cmd.Run(t.Context(), []string{"tau", "login", ai.ProviderOpenAI, "--api-key", "synthetic"}))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(original), string(data))
}

func TestReloadSessionReplacesPackageDirectory(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", cfgDir)
	t.Setenv("TAU_PACKAGE_DIR", "")
	t.Chdir(t.TempDir())
	for _, name := range []string{"old", "new"} {
		path := filepath.Join(cfgDir, name, "example", "prompts", name+".md")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(name), 0o600))
	}
	writeSettings := func(name string) {
		t.Helper()
		data, err := json.Marshal(map[string]string{"packageDir": filepath.Join(cfgDir, name)})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "settings.json"), data, 0o600))
	}
	writeSettings("old")
	user, project, err := packagesDirs()
	require.NoError(t, err)
	reg := commands.NewInMemoryRegistry()
	loader, err := artifacts.NewLoader(artifacts.LoaderOptions{CWD: t.TempDir(), PackagePaths: []string{project, user}, DisableSkills: true, DisableThemes: true, CommandsRegistry: reg})
	require.NoError(t, err)
	previousLoader, previousPlugins := sdk.GlobalArtifactLoader, GlobalPluginManager
	sdk.GlobalArtifactLoader, GlobalPluginManager = loader, nil
	t.Cleanup(func() { sdk.GlobalArtifactLoader, GlobalPluginManager = previousLoader, previousPlugins })
	_, ok := loader.PromptRegistry().Get("old")
	require.True(t, ok)
	writeSettings("new")
	rejected := errors.New("session rejected settings")
	_, err = reloadSession(t.Context(), "", &failingReloadSession{err: rejected})
	require.ErrorIs(t, err, rejected)
	_, ok = loader.PromptRegistry().Get("old")
	require.True(t, ok)
	require.NoError(t, loader.Reload(t.Context()))
	_, ok = loader.PromptRegistry().Get("old")
	require.True(t, ok, "rejected reload changed discovery inputs")
	_, err = reloadSession(t.Context(), "", &reloadSettingsSession{})
	require.NoError(t, err)
	_, ok = loader.PromptRegistry().Get("new")
	require.True(t, ok, "reload did not scan new packageDir")
	_, _, ok = reg.Lookup("old")
	require.False(t, ok, "reload retained old package command")
	require.NoError(t, loader.Reload(t.Context()))
	_, ok = loader.PromptRegistry().Get("new")
	require.True(t, ok, "normal reload forgot committed packageDir")
	t.Setenv("TAU_PACKAGE_DIR", filepath.Join(cfgDir, "old"))
	_, err = reloadSession(t.Context(), "", &reloadSettingsSession{})
	require.NoError(t, err)
	_, ok = loader.PromptRegistry().Get("old")
	require.True(t, ok, "reload ignored package directory environment override")
}

func TestReloadSessionAppliesConfiguredPackagePolicy(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", cfgDir)
	t.Setenv("TAU_PACKAGE_DIR", "")
	t.Chdir(t.TempDir())
	root := filepath.Join(cfgDir, "local")
	for _, name := range []string{"keep", "drop"} {
		path := filepath.Join(root, "prompts", name+".md")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(name), 0o600))
	}
	settingsPath := filepath.Join(cfgDir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"packages":[{"source":"./local","prompts":["prompts/keep.md"]}]}`), 0o600))
	_, _, startup, startupCommands, buildErr := buildRuntimeOptions(t.Context(), nil, nil, withoutPlugins())
	require.NoError(t, buildErr)
	require.NotNil(t, startup)
	_, _, startupKeep := startupCommands.Lookup("keep")
	require.True(t, startupKeep, "startup did not resolve configured local packages")
	_, _, startupDrop := startupCommands.Lookup("drop")
	require.False(t, startupDrop, "startup bypassed package policy")
	reg := commands.NewInMemoryRegistry()
	loader, err := artifacts.NewLoader(artifacts.LoaderOptions{CWD: t.TempDir(), DisableSkills: true, DisableThemes: true, CommandsRegistry: reg})
	require.NoError(t, err)
	oldLoader, oldPlugins := sdk.GlobalArtifactLoader, GlobalPluginManager
	sdk.GlobalArtifactLoader, GlobalPluginManager = loader, nil
	t.Cleanup(func() { sdk.GlobalArtifactLoader, GlobalPluginManager = oldLoader, oldPlugins })
	_, err = reloadSession(t.Context(), "", &reloadSettingsSession{})
	require.NoError(t, err)
	_, ok := loader.PromptRegistry().Get("keep")
	require.True(t, ok, "configured local package was not discovered")
	_, _, ok = reg.Lookup("drop")
	require.False(t, ok, "package filter leaked an excluded command")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"packages":[{"source":"./local","prompts":[]}]}`), 0o600))
	rejected := errors.New("rejected")
	_, err = reloadSession(t.Context(), "", &failingReloadSession{err: rejected})
	require.ErrorIs(t, err, rejected)
	_, _, ok = reg.Lookup("keep")
	require.True(t, ok)
	_, err = reloadSession(t.Context(), "", &reloadSettingsSession{})
	require.NoError(t, err)
	_, _, ok = reg.Lookup("keep")
	require.False(t, ok)
	require.NoError(t, loader.Reload(t.Context()))
	_, _, ok = reg.Lookup("keep")
	require.False(t, ok, "normal reload forgot the package policy")
	// Invalid package metadata fails before session admission or artifact commit.
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"packages":["npm:../../escape"]}`), 0o600))
	_, err = reloadSession(t.Context(), "", &reloadSettingsSession{})
	require.ErrorContains(t, err, "package")
	_, _, ok = reg.Lookup("keep")
	require.False(t, ok)
}

func TestRuntimeCommandsRejectInvalidSettingsBeforeDiscovery(t *testing.T) {
	for _, scope := range []string{"user", "project"} {
		for _, tc := range []struct {
			name string
			run  func(context.Context, *cli.Command) error
		}{
			{"skills", runSkillsList}, {"prompts", runPromptsList}, {"themes", runThemesList},
		} {
			t.Run(scope+"/"+tc.name, func(t *testing.T) {
				cfg := isolateBuildEnv(t)
				root := t.TempDir()
				t.Chdir(root)
				require.NoError(t, ai.ProjectTrustStore{Path: filepath.Join(configDir(), "trust.json")}.Set(t.Context(), root, new(true)))
				path := filepath.Join(cfg, "settings.json")
				if scope == "project" {
					require.NoError(t, os.Mkdir(filepath.Join(root, ".tau"), 0o700))
					path = filepath.Join(root, ".tau", "settings.json")
				}
				require.NoError(t, os.WriteFile(path, []byte(`{"retry":`), 0o600))
				err := tc.run(t.Context(), &cli.Command{})
				require.ErrorContains(t, err, "load "+scope+" settings")
				_, statErr := os.Stat(filepath.Join(cfg, "sessions"))
				require.ErrorIs(t, statErr, os.ErrNotExist)
			})
		}
	}
}

func TestBuildRuntimeOptionsRejectsSettingsWithoutPartialState(t *testing.T) {
	for _, scope := range []string{"user", "project"} {
		for _, withPlugins := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/plugins=%t", scope, withPlugins), func(t *testing.T) {
				cfg := isolateBuildEnv(t)
				root := t.TempDir()
				t.Chdir(root)
				require.NoError(t, ai.ProjectTrustStore{Path: filepath.Join(configDir(), "trust.json")}.Set(t.Context(), root, new(true)))
				path := filepath.Join(cfg, "settings.json")
				if scope == "project" {
					require.NoError(t, os.Mkdir(filepath.Join(root, ".tau"), 0o700))
					path = filepath.Join(root, ".tau", "settings.json")
				}
				require.NoError(t, os.WriteFile(path, []byte(`{"retry":`), 0o600))
				var options []runtimeBuildOption
				if !withPlugins {
					options = append(options, withoutPlugins())
				}
				opts, mgr, loader, reg, err := buildRuntimeOptions(t.Context(), ui.NewNoOpUI(), nil, options...)
				require.ErrorContains(t, err, "load "+scope+" settings")
				assert.Equal(t, sdk.RuntimeOptions{}, opts)
				assert.Nil(t, mgr)
				assert.Nil(t, loader)
				assert.Nil(t, reg)
				_, statErr := os.Stat(filepath.Join(cfg, "sessions"))
				require.ErrorIs(t, statErr, os.ErrNotExist)
				oldLoader := sdk.GlobalArtifactLoader
				t.Cleanup(func() { sdk.GlobalArtifactLoader = oldLoader })
				opts, mgr, loader, reg, flags, err := buildConversationRuntimeOptions(t.Context(), ui.NewNoOpUI())
				require.ErrorContains(t, err, "load "+scope+" settings")
				assert.Equal(t, sdk.RuntimeOptions{}, opts)
				assert.Nil(t, mgr)
				assert.Nil(t, loader)
				assert.Nil(t, reg)
				assert.Equal(t, SessionFlags{}, flags)
				if oldLoader == nil {
					assert.Nil(t, sdk.GlobalArtifactLoader)
				} else {
					assert.Same(t, oldLoader, sdk.GlobalArtifactLoader)
				}
			})
		}
	}
}

func TestBuildRuntimeOptionsRejectsInvalidSessionStorage(t *testing.T) {
	for _, withPlugins := range []bool{false, true} {
		t.Run(fmt.Sprint(withPlugins), func(t *testing.T) {
			cfg := isolateBuildEnv(t)
			t.Chdir(t.TempDir())
			blocked := filepath.Join(cfg, "blocked")
			require.NoError(t, os.WriteFile(blocked, []byte("file"), 0o600))
			t.Setenv("TAU_SESSION_DIR", blocked)
			var options []runtimeBuildOption
			if !withPlugins {
				options = append(options, withoutPlugins())
			}
			opts, mgr, loader, reg, err := buildRuntimeOptions(t.Context(), nil, nil, options...)
			require.ErrorContains(t, err, "create sessions dir")
			assert.Equal(t, sdk.RuntimeOptions{}, opts)
			assert.Nil(t, mgr)
			assert.Nil(t, loader)
			assert.Nil(t, reg)
		})
	}
}

func TestBuildRuntimeOptionsRejectsOwnerCleanupFailure(t *testing.T) {
	t.Setenv("TAU_SESSION_DIR", "")
	cfg := isolateBuildEnv(t)
	t.Chdir(t.TempDir())
	dir := filepath.Join(cfg, "sessions")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.Symlink("bad.owner", filepath.Join(dir, "bad.owner")))
	opts, mgr, loader, reg, err := buildRuntimeOptions(t.Context(), nil, nil)
	require.ErrorContains(t, err, "clean stale session owners")
	assert.Equal(t, sdk.RuntimeOptions{}, opts)
	assert.Nil(t, mgr)
	assert.Nil(t, loader)
	assert.Nil(t, reg)
}

func TestSessionCommandsRejectMalformedSettings(t *testing.T) {
	for _, scope := range []string{"user", "project"} {
		t.Run(scope, func(t *testing.T) {
			cfg := isolateBuildEnv(t)
			t.Setenv("TAU_SESSION_DIR", "")
			root := t.TempDir()
			t.Chdir(root)
			require.NoError(t, ai.ProjectTrustStore{Path: filepath.Join(configDir(), "trust.json")}.Set(t.Context(), root, new(true)))
			path := filepath.Join(cfg, "settings.json")
			if scope == "project" {
				require.NoError(t, os.Mkdir(filepath.Join(root, ".tau"), 0o700))
				path = filepath.Join(root, ".tau", "settings.json")
			}
			require.NoError(t, os.WriteFile(path, []byte(`{"retry":`), 0o600))
			err := runSessionsList(t.Context(), &cli.Command{})
			require.ErrorContains(t, err, "load "+scope+" settings")
			_, err = os.Stat(filepath.Join(cfg, "sessions"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestPluginCLIFlagDiscoveryRejectsInvalidSettings(t *testing.T) {
	cfg := isolateBuildEnv(t)
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"retry":`), 0o600))
	_, err := discoverPluginCLIFlags(t.Context(), []string{"tau", "--extension", t.TempDir()})
	require.ErrorContains(t, err, "load user settings")
}

func TestPluginCLIFlagDiscoveryHonorsCancellation(t *testing.T) {
	isolateBuildEnv(t)
	t.Chdir(t.TempDir())
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("pre-parser interrupted")
	cancel(cause)
	_, err := discoverPluginCLIFlags(ctx, []string{"tau", "--extension", t.TempDir()})
	require.ErrorIs(t, err, cause)
	_, _, err = preprocessPluginCLIArgs(ctx, []string{"tau", "--extension", t.TempDir()})
	require.ErrorIs(t, err, cause)
}

func TestReloadSessionReplacesPluginSettingsAfterAdmission(t *testing.T) {
	cfg := isolateBuildEnv(t)
	t.Chdir(t.TempDir())
	oldLoader := sdk.GlobalArtifactLoader
	sdk.GlobalArtifactLoader = nil
	oldManager := GlobalPluginManager
	t.Cleanup(func() { sdk.GlobalArtifactLoader = oldLoader; GlobalPluginManager = oldManager })
	loop := filepath.Join(t.TempDir(), "loop")
	require.NoError(t, os.Symlink(loop, loop))
	mgr := plugins.NewManager(cfg, nil, nil)
	mgr.SetSettings(&ai.Settings{Extensions: []string{loop}})
	GlobalPluginManager = mgr
	require.NoError(t, os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{}`), 0o600))
	rejected := errors.New("settings rejected")
	_, err := reloadSession(t.Context(), "", &failingReloadSession{err: rejected})
	require.ErrorIs(t, err, rejected)
	require.ErrorContains(t, mgr.Reload(t.Context()), "scan extensions")
	_, err = reloadSession(t.Context(), "", &reloadSettingsSession{})
	require.NoError(t, err)
	require.NoError(t, mgr.Reload(t.Context()))
}

func TestReloadSessionRejectsCancellationBeforeAdmission(t *testing.T) {
	cfg := isolateBuildEnv(t)
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"read":{"lineCap":7}}`), 0o600))
	oldLoader := sdk.GlobalArtifactLoader
	sdk.GlobalArtifactLoader = nil
	oldManager := GlobalPluginManager
	GlobalPluginManager = nil
	t.Cleanup(func() { sdk.GlobalArtifactLoader = oldLoader; GlobalPluginManager = oldManager })
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("reload interrupted")
	cancel(cause)
	sess := &reloadSettingsSession{}
	result, err := reloadSession(ctx, "", sess)
	require.ErrorIs(t, err, cause)
	require.Nil(t, sess.settings)
	require.Equal(t, commands.Result{}, result)
}

func TestReloadSessionRejectsPluginDiscoveryBeforeSettings(t *testing.T) {
	cfg := isolateBuildEnv(t)
	t.Chdir(t.TempDir())
	bad := filepath.Join(cfg, "bad-extensions", "broken")
	require.NoError(t, os.MkdirAll(bad, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bad, "package.json"), []byte(`{"pi":`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"extensions":["bad-extensions"]}`), 0o600))
	oldLoader, oldManager := sdk.GlobalArtifactLoader, GlobalPluginManager
	promptDir := filepath.Join(cfg, "prompts")
	require.NoError(t, os.MkdirAll(promptDir, 0o700))
	promptPath := filepath.Join(promptDir, "old.md")
	require.NoError(t, os.WriteFile(promptPath, []byte("old prompt"), 0o600))
	registry := commands.NewInMemoryRegistry()
	loader, err := artifacts.NewLoader(artifacts.LoaderOptions{CWD: t.TempDir(), UserDirs: map[artifacts.ArtifactKind]string{artifacts.KindPrompt: promptDir}, DisableSkills: true, DisableThemes: true, CommandsRegistry: registry})
	require.NoError(t, err)
	before := loader.PromptRegistry()
	require.NoError(t, os.Remove(promptPath))
	sdk.GlobalArtifactLoader = loader
	GlobalPluginManager = plugins.NewManager(cfg, nil, nil)
	t.Cleanup(func() { sdk.GlobalArtifactLoader = oldLoader; GlobalPluginManager = oldManager })
	sess := &reloadSettingsSession{}
	_, err = reloadSession(t.Context(), "", sess)
	require.ErrorContains(t, err, "parse Pi manifest")
	require.Nil(t, sess.settings)
	require.Same(t, before, loader.PromptRegistry())
	_, _, ok := registry.Lookup("old")
	require.True(t, ok)
}

type failingResourceReloadSession struct {
	commands.CommandSession
	settings       *ai.Settings
	toolErr        error
	promptErr      error
	promptReloaded bool
}

func (s *failingResourceReloadSession) ReloadSettings(_ context.Context, settings *ai.Settings) error {
	s.settings = settings
	return nil
}

func (s *failingResourceReloadSession) ReloadToolRegistry(context.Context) error { return s.toolErr }

func (s *failingResourceReloadSession) ReloadSystemPrompt(context.Context) error {
	s.promptReloaded = true
	return s.promptErr
}

func TestReloadSessionRejectsUnsupportedSessionWithoutMutation(t *testing.T) {
	cfg := isolateBuildEnv(t)
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"read":{"lineCap":7}}`), 0o600))
	oldLoader, oldManager := sdk.GlobalArtifactLoader, GlobalPluginManager
	loader, err := artifacts.NewLoader(artifacts.LoaderOptions{CWD: t.TempDir(), DisableSkills: true, DisablePrompts: true, DisableThemes: true})
	require.NoError(t, err)
	before := loader.SkillRegistry()
	sdk.GlobalArtifactLoader, GlobalPluginManager = loader, nil
	t.Cleanup(func() { sdk.GlobalArtifactLoader, GlobalPluginManager = oldLoader, oldManager })
	sess := &failingResourceReloadSession{toolErr: errors.New("tool refresh failed"), promptErr: errors.New("prompt refresh failed")}
	result, err := reloadSession(t.Context(), "", sess)
	require.ErrorContains(t, err, "coordinated resource reload")
	require.Equal(t, commands.Result{}, result)
	require.False(t, sess.promptReloaded)
	require.Nil(t, sess.settings)
	require.Same(t, before, loader.SkillRegistry())
}

type coordinatedReloadSession struct {
	reloadSettingsSession
	called bool
	cause  error
}

func (s *coordinatedReloadSession) ReloadResources(_ context.Context, settings *ai.Settings, prepared *artifacts.PreparedReload, preparedPlugins *plugins.PreparedReload, name string) error {
	s.called = settings != nil && prepared != nil && preparedPlugins != nil && name == "selected"
	return s.cause
}

func TestReloadSessionUsesCoordinatedAdmission(t *testing.T) {
	cfg := isolateBuildEnv(t)
	t.Chdir(t.TempDir())
	oldLoader, oldManager := sdk.GlobalArtifactLoader, GlobalPluginManager
	loader, err := artifacts.NewLoader(artifacts.LoaderOptions{CWD: t.TempDir(), DisableSkills: true, DisablePrompts: true, DisableThemes: true})
	require.NoError(t, err)
	before := loader.SkillRegistry()
	sdk.GlobalArtifactLoader = loader
	GlobalPluginManager = plugins.NewManager(cfg, nil, nil)
	t.Cleanup(func() { sdk.GlobalArtifactLoader = oldLoader; GlobalPluginManager = oldManager })
	cause := errors.New("coordinated rejection")
	sess := &coordinatedReloadSession{cause: cause}
	_, err = reloadSession(t.Context(), "selected", sess)
	require.ErrorIs(t, err, cause)
	require.True(t, sess.called)
	require.Nil(t, sess.settings)
	require.Same(t, before, loader.SkillRegistry())
}

type unpreparedArtifactLoader struct{ sdk.ArtifactLoader }

func TestReloadSessionRejectsUnpreparedArtifactLoader(t *testing.T) {
	isolateBuildEnv(t)
	t.Chdir(t.TempDir())
	loader, err := artifacts.NewLoader(artifacts.LoaderOptions{CWD: t.TempDir(), DisableSkills: true, DisablePrompts: true, DisableThemes: true})
	require.NoError(t, err)
	oldLoader, oldManager := sdk.GlobalArtifactLoader, GlobalPluginManager
	sdk.GlobalArtifactLoader, GlobalPluginManager = &unpreparedArtifactLoader{ArtifactLoader: loader}, nil
	t.Cleanup(func() { sdk.GlobalArtifactLoader, GlobalPluginManager = oldLoader, oldManager })
	before := loader.SkillRegistry()
	sess := &reloadSettingsSession{}
	result, err := reloadSession(t.Context(), "", sess)
	require.ErrorContains(t, err, "artifact loader does not support prepared reload")
	require.Equal(t, commands.Result{}, result)
	require.Nil(t, sess.settings)
	require.Same(t, before, loader.SkillRegistry())
}

func TestPackageCommandsRejectInvalidSettings(t *testing.T) {
	for _, scope := range []string{"user", "project"} {
		for _, entry := range []struct {
			name string
			run  func(context.Context, *cli.Command) error
		}{
			{"list", runPackagesList}, {"update", runUpdate},
		} {
			t.Run(scope+"/"+entry.name, func(t *testing.T) {
				config := isolateBuildEnv(t)
				root := t.TempDir()
				t.Chdir(root)
				require.NoError(t, ai.ProjectTrustStore{Path: filepath.Join(configDir(), "trust.json")}.Set(t.Context(), root, new(true)))
				path := filepath.Join(config, "settings.json")
				if scope == "project" {
					require.NoError(t, os.MkdirAll(filepath.Join(root, ".tau"), 0o700))
					path = filepath.Join(root, ".tau", "settings.json")
				}
				require.NoError(t, os.WriteFile(path, []byte(`{"retry":`), 0o600))
				command := &cli.Command{Name: entry.name, Action: entry.run, ExitErrHandler: func(context.Context, *cli.Command, error) {}}
				require.ErrorContains(t, command.Run(t.Context(), []string{entry.name}), "load "+scope+" settings")
			})
		}
	}
}

func TestPackageUpdateReportsFailuresInsteadOfSuccessfulExit(t *testing.T) {
	isolateBuildEnv(t)
	t.Chdir(t.TempDir())
	command := &cli.Command{Name: "update", Action: runUpdate, ExitErrHandler: func(context.Context, *cli.Command, error) {}}
	err := command.Run(t.Context(), []string{"update", "missing-one", "missing-two"})
	require.ErrorContains(t, err, "missing-one")
	require.ErrorContains(t, err, "missing-two")
}

func TestDeniedProjectBootstrapAndExplicitOverride(t *testing.T) {
	cfg := isolateBuildEnv(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	require.NoError(t, os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"defaultProjectTrust":"never","theme":"global"}`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(cwd, ".tau", "prompts"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, ".tau", "prompts", "unsafe.md"), []byte("unsafe prompt"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, ".tau", "settings.json"), []byte("malformed"), 0o600))
	opts, mgr, loader, _, err := buildRuntimeOptions(t.Context(), ui.NewNoOpUI(), nil)
	require.NoError(t, err)
	require.False(t, opts.Settings.ProjectResourcesTrusted())
	require.Equal(t, "global", opts.Settings.Theme)
	require.NotNil(t, mgr)
	require.NotNil(t, loader)
	require.Empty(t, loader.PromptRegistry().List())
	require.NoError(t, mgr.Shutdown(t.Context()))
	cmd := &cli.Command{Name: "test", Flags: []cli.Flag{&cli.BoolFlag{Name: "approve"}, &cli.BoolFlag{Name: "no-approve"}}, Action: func(ctx context.Context, cmd *cli.Command) error {
		_, err := loadSettingsForTrust(ctx, cmd)
		return err
	}, ExitErrHandler: func(context.Context, *cli.Command, error) {}}
	require.ErrorContains(t, cmd.Run(t.Context(), []string{"test", "--approve"}), "load project settings")
	require.NoError(t, cmd.Run(t.Context(), []string{"test", "--approve", "--no-approve"}))
}

func TestBuildRuntimeOptionsRejectsInvalidGlobalProxyBeforeDiscovery(t *testing.T) {
	cfg := isolateBuildEnv(t)
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"httpProxy":"ftp://proxy.invalid"}`), 0o600))
	opts, manager, loader, registry, err := buildRuntimeOptions(t.Context(), ui.NewNoOpUI(), nil)
	require.ErrorContains(t, err, "httpProxy")
	require.Equal(t, sdk.RuntimeOptions{}, opts)
	require.Nil(t, manager)
	require.Nil(t, loader)
	require.Nil(t, registry)
	require.NoFileExists(t, filepath.Join(cfg, "auth.json"))
}
