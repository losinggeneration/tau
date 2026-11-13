package tui

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
)

type pluginsFakeSession struct {
	fakeSession
	states []sdk.PluginToggleState
	calls  []string
}

func (s *pluginsFakeSession) PluginStates() []sdk.PluginToggleState { return s.states }

func (s *pluginsFakeSession) EnablePlugin(_ context.Context, name string, scope ai.ToggleScope) error {
	s.calls = append(s.calls, "enable "+name+" "+string(scope))
	return nil
}

func (s *pluginsFakeSession) DisablePlugin(_ context.Context, name string, scope ai.ToggleScope) error {
	s.calls = append(s.calls, "disable "+name+" "+string(scope))
	if name == "missing" {
		return errors.New(`unknown plugin or tool "missing"`)
	}
	return nil
}

func TestPluginsCommandListsAndToggles(t *testing.T) {
	a := newTestApp(t)
	sess := &pluginsFakeSession{states: []sdk.PluginToggleState{
		{Name: "tau-subagent", Enabled: true, Loaded: true},
		{Name: "subagent", Plugin: "tau-subagent", Enabled: true},
		{Name: "subagent_parallel", Plugin: "tau-subagent", Scope: ai.ToggleProject},
		{Name: "dormant", Scope: ai.ToggleGlobal},
	}}
	a.sess = sess
	last := func() string { return a.conv.Messages()[len(a.conv.Messages())-1].Text }

	cmdPlugins(a, "")
	assert.Equal(t, "tau-subagent: enabled, running\n  subagent: enabled\n  subagent_parallel: disabled by project\ndormant: disabled by global, not loaded", last())
	cmdPlugins(a, "disable tau-subagent")
	assert.Equal(t, "Disabled tau-subagent at session scope. The model sees the change from its next request.", last())
	cmdPlugins(a, "enable dormant --global")
	cmdPlugins(a, "disable missing --project")
	assert.Contains(t, last(), `unknown plugin or tool "missing"`)
	for _, bad := range []string{"toggle x", "disable", "disable x --forever", "disable x --session extra"} {
		cmdPlugins(a, bad)
		assert.Equal(t, pluginsUsage, last(), bad)
	}
	assert.Equal(t, []string{"disable tau-subagent session", "enable dormant global", "disable missing project"}, sess.calls)

	var values []string
	for _, item := range completePluginsArgs(a, "disable sub") {
		values = append(values, item.Value)
	}
	assert.Equal(t, []string{"disable subagent ", "disable subagent_parallel "}, values)
	values = nil
	for _, item := range completePluginsArgs(a, "disable subagent --p") {
		values = append(values, item.Value)
	}
	assert.Equal(t, []string{"disable subagent --project"}, values)

	a.sess = &fakeSession{}
	cmdPlugins(a, "")
	assert.Contains(t, last(), "no plugins")
}
