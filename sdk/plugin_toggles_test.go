package sdk

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/losinggeneration/tau/agent"
	agentplugins "github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/agent/plugins/proto"
	"github.com/losinggeneration/tau/ai"
)

type toggleEventClient struct {
	*loadoutPluginClient
	mu     sync.Mutex
	events []string
}

func (c *toggleEventClient) OnEvent(_ context.Context, event *proto.Event, _ ...grpc.CallOption) (*proto.EventResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event.Name)
	return &proto.EventResponse{}, nil
}

func (c *toggleEventClient) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func sendToolNames(t *testing.T, provider *recordingProvider, sess Session, text string) []string {
	t.Helper()
	events, err := sess.Send(t.Context(), Message{Text: text})
	require.NoError(t, err)
	collectSessionEvents(t, events)
	var names []string
	for _, tool := range provider.LastPrompt().Tools {
		names = append(names, tool.Name)
	}
	return names
}

func toggleState(states []PluginToggleState, name string) PluginToggleState {
	for _, state := range states {
		if state.Name == name {
			return state
		}
	}
	return PluginToggleState{}
}

func TestPluginTogglesApplyAtNextRequest(t *testing.T) {
	provider, model := registerRecordingProvider(t, "reply")
	t.Setenv("TAU_CONFIG_DIR", t.TempDir())
	project := t.TempDir()
	t.Chdir(project)
	dir := t.TempDir()
	path := filepath.Join(dir, "extensions", "tau-subagent")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("not executed"), 0o700))
	client := &toggleEventClient{loadoutPluginClient: &loadoutPluginClient{registrations: &proto.Registrations{
		Tools:              []*proto.ToolRegistration{{Name: "subagent"}, {Name: "subagent_parallel"}},
		EventSubscriptions: []string{"plugin_disabled", "plugin_enabled"},
	}}}
	mgr := agentplugins.NewManager(dir, nil, nil, agentplugins.WithPluginConnector(func(context.Context, string, *agentplugins.Manifest, string, string) (proto.PluginClient, func(), error) {
		return client, func() {}, nil
	}))
	_, err := mgr.Load(t.Context(), path)
	require.NoError(t, err)
	rt, err := NewRuntime(RuntimeOptions{PluginManager: mgr, Settings: &ai.Settings{}, NoBuiltinTools: true, NoContextFiles: true, ExplicitModel: model, CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("key")}, SessionsDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(testContext(t))) })
	sess, err := rt.NewSession(t.Context(), SessionOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sess.Close(testContext(t))) })
	toggler := sess.(PluginToggler)
	all := []string{"subagent", "subagent_parallel"}
	require.ElementsMatch(t, all, sendToolNames(t, provider, sess, "first"))

	require.NoError(t, toggler.DisablePlugin(t.Context(), "tau-subagent", ai.ToggleSession))
	require.Empty(t, sendToolNames(t, provider, sess, "paused"))
	require.Equal(t, []string{"plugin_disabled"}, client.seen())
	plugin := toggleState(toggler.PluginStates(), "tau-subagent")
	require.Equal(t, PluginToggleState{Name: "tau-subagent", Scope: ai.ToggleSession, Loaded: true}, plugin, "a paused plugin keeps running")
	for _, tool := range sess.(*agentSession).agent.State().Tools {
		require.Equal(t, agent.ToolDisabled, tool.Exposure, "stale calls to %s must report it disabled", tool.Name)
	}
	// A paused plugin can still deliver background results to the session.
	request := &proto.SessionSendCustomMessageRequest{MessageJson: `{"customType":"subagent_result","content":"done"}`, TriggerTurn: new(false)}
	_, err = agentplugins.NewHostService(mgr).SessionSendCustomMessage(t.Context(), request)
	require.NoError(t, err)
	messages := sess.(*agentSession).agent.Messages()
	require.Equal(t, "subagent_result", messages[len(messages)-1].CustomType)

	require.NoError(t, toggler.EnablePlugin(t.Context(), "tau-subagent", ai.ToggleSession))
	require.ElementsMatch(t, all, sendToolNames(t, provider, sess, "resumed"))
	require.Equal(t, []string{"plugin_disabled", "plugin_enabled"}, client.seen())

	require.NoError(t, toggler.DisablePlugin(t.Context(), "subagent_parallel", ai.ToggleSession))
	require.Equal(t, []string{"subagent"}, sendToolNames(t, provider, sess, "one tool off"))
	require.NoError(t, toggler.EnablePlugin(t.Context(), "subagent_parallel", ai.ToggleSession))
	require.Error(t, toggler.DisablePlugin(t.Context(), "missing", ai.ToggleSession))

	// A project disable persists for new sessions in this project only.
	require.NoError(t, toggler.DisablePlugin(t.Context(), "tau-subagent", ai.ToggleProject))
	saved, err := ai.LoadSettings(filepath.Join(project, ".tau", "settings.json"))
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"tau-subagent": false}, saved.Enabled.Plugins)
	global, err := ai.LoadSettings(filepath.Join(os.Getenv("TAU_CONFIG_DIR"), "settings.json"))
	require.NoError(t, err)
	require.Empty(t, global.Enabled.Plugins)
	trusted, err := ai.LoadTrustedSettings(t.Context(), os.Getenv("TAU_CONFIG_DIR"), project, t.TempDir(), ai.ProjectTrustOptions{Override: new(true)})
	require.NoError(t, err)
	elsewhere, err := ai.LoadTrustedSettings(t.Context(), os.Getenv("TAU_CONFIG_DIR"), t.TempDir(), t.TempDir(), ai.ProjectTrustOptions{Override: new(true)})
	require.NoError(t, err)
	enabled, _ := trusted.ResolveToggle(ai.Toggles{}, ai.TogglePlugin, "tau-subagent")
	require.False(t, enabled)
	enabled, _ = elsewhere.ResolveToggle(ai.Toggles{}, ai.TogglePlugin, "tau-subagent")
	require.True(t, enabled)

	// The earlier session enable still wins for this session.
	require.ElementsMatch(t, all, sendToolNames(t, provider, sess, "session wins"))
	require.Equal(t, ai.ToggleSession, toggleState(toggler.PluginStates(), "tau-subagent").Scope)
	next, err := rt.NewSession(t.Context(), SessionOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, next.Close(testContext(t))) })
	require.Empty(t, sendToolNames(t, provider, next, "new session"))
	state := toggleState(next.(PluginToggler).PluginStates(), "subagent")
	require.Equal(t, PluginToggleState{Name: "subagent", Plugin: "tau-subagent", Scope: ai.ToggleProject}, state)

	require.NoError(t, next.(PluginToggler).DisablePlugin(t.Context(), "subagent_parallel", ai.ToggleGlobal))
	global, err = ai.LoadSettings(filepath.Join(os.Getenv("TAU_CONFIG_DIR"), "settings.json"))
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"subagent_parallel": false}, global.Enabled.Tools)
}

func TestSessionEnableStartsPluginDisabledInSettings(t *testing.T) {
	provider, model := registerRecordingProvider(t, "reply")
	dir := t.TempDir()
	path := filepath.Join(dir, "extensions", "dormant")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("not executed"), 0o700))
	var starts int
	mgr := agentplugins.NewManager(dir, nil, nil, agentplugins.WithPluginConnector(func(context.Context, string, *agentplugins.Manifest, string, string) (proto.PluginClient, func(), error) {
		starts++
		return &loadoutPluginClient{registrations: &proto.Registrations{Tools: []*proto.ToolRegistration{{Name: "wake"}}}}, func() {}, nil
	}))
	settings := &ai.Settings{Enabled: ai.Toggles{Plugins: map[string]bool{"dormant": false}}}
	mgr.SetSettings(settings)
	require.NoError(t, mgr.Discover(t.Context()))
	require.Zero(t, starts, "a globally disabled plugin never starts")
	rt, err := NewRuntime(RuntimeOptions{PluginManager: mgr, Settings: settings, NoBuiltinTools: true, NoContextFiles: true, ExplicitModel: model, CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("key")}, SessionsDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(testContext(t))) })
	sess, err := rt.NewSession(t.Context(), SessionOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sess.Close(testContext(t))) })
	toggler := sess.(PluginToggler)
	require.Equal(t, []PluginToggleState{{Name: "dormant", Scope: ai.ToggleGlobal}}, toggler.PluginStates())

	require.NoError(t, toggler.EnablePlugin(t.Context(), "dormant", ai.ToggleSession))
	require.Equal(t, 1, starts)
	require.Equal(t, []string{"wake"}, sendToolNames(t, provider, sess, "awake"))
	require.Equal(t, PluginToggleState{Name: "dormant", Scope: ai.ToggleSession, Enabled: true, Loaded: true}, toggleState(toggler.PluginStates(), "dormant"))
}
