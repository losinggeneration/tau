package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/paths"
)

// PluginToggleState is one plugin or plugin tool as /plugins shows it.
type PluginToggleState struct {
	Name string
	// Plugin owns a tool row and is empty on a plugin row.
	Plugin string
	// Scope set the state; empty means nothing names it.
	Scope   ai.ToggleScope
	Enabled bool
	// Loaded reports a running plugin process on plugin rows.
	Loaded bool
}

// PluginToggler lists and toggles plugins and their tools. Changes reach
// the model at the next request boundary.
type PluginToggler interface {
	PluginStates() []PluginToggleState
	EnablePlugin(ctx context.Context, name string, scope ai.ToggleScope) error
	DisablePlugin(ctx context.Context, name string, scope ai.ToggleScope) error
}

// pluginToolEnabled requires both the owning plugin and the tool to be on.
func pluginToolEnabled(settings *ai.Settings, session ai.Toggles, plugin, tool string) (bool, ai.ToggleScope) {
	if enabled, scope := settings.ResolveToggle(session, ai.TogglePlugin, plugin); !enabled {
		return false, scope
	}
	return settings.ResolveToggle(session, ai.ToggleTool, tool)
}

// toggledPluginTool hides a tool that is turned off while keeping it
// registered, so a stale call reports it as disabled.
func toggledPluginTool(tool agent.Tool, desc plugins.ToolHandlerDesc, settings *ai.Settings, session ai.Toggles) agent.Tool {
	if enabled, _ := pluginToolEnabled(settings, session, desc.Plugin, desc.Name); !enabled {
		tool.Exposure = agent.ToolDisabled
	}
	return tool
}

func (s *agentSession) PluginStates() []PluginToggleState {
	if s.pluginManager == nil {
		return nil
	}
	s.mu.Lock()
	settings, session := s.settings, s.pluginToggles
	s.mu.Unlock()
	var states []PluginToggleState
	for _, plugin := range s.pluginManager.PluginStatuses() {
		enabled, scope := settings.ResolveToggle(session, ai.TogglePlugin, plugin.Name)
		states = append(states, PluginToggleState{Name: plugin.Name, Scope: scope, Enabled: enabled, Loaded: plugin.Loaded})
		for _, tool := range plugin.Tools {
			enabled, scope := pluginToolEnabled(settings, session, plugin.Name, tool)
			states = append(states, PluginToggleState{Name: tool, Plugin: plugin.Name, Scope: scope, Enabled: enabled})
		}
	}
	return states
}

func (s *agentSession) EnablePlugin(ctx context.Context, name string, scope ai.ToggleScope) error {
	return s.togglePlugin(ctx, name, scope, true)
}

func (s *agentSession) DisablePlugin(ctx context.Context, name string, scope ai.ToggleScope) error {
	return s.togglePlugin(ctx, name, scope, false)
}

func (s *agentSession) togglePlugin(ctx context.Context, name string, scope ai.ToggleScope, enabled bool) error {
	if s.pluginManager == nil {
		return errors.New("this session has no plugins")
	}
	statuses := s.pluginManager.PluginStatuses()
	kind := ai.ToggleTool
	index := slices.IndexFunc(statuses, func(status plugins.PluginStatus) bool { return status.Name == name })
	if index >= 0 {
		kind = ai.TogglePlugin
	} else if !slices.ContainsFunc(statuses, func(status plugins.PluginStatus) bool { return slices.Contains(status.Tools, name) }) {
		return fmt.Errorf("unknown plugin or tool %q", name)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	before, _ := s.settings.ResolveToggle(s.pluginToggles, kind, name)
	if err := s.storeToggleLocked(ctx, scope, kind, name, enabled); err != nil {
		s.mu.Unlock()
		return err
	}
	after, _ := s.settings.ResolveToggle(s.pluginToggles, kind, name)
	s.mu.Unlock()

	var errs error
	if kind == ai.TogglePlugin && before != after {
		if after && !statuses[index].Loaded {
			// A session enable overrides a persisted disable that kept the process from starting.
			errs = s.pluginManager.ReloadNamed(ctx, name)
		} else if statuses[index].Loaded {
			event := "plugin_disabled"
			if after {
				event = "plugin_enabled"
			}
			payload, err := json.Marshal(map[string]string{"plugin": name, "scope": string(scope)})
			if err != nil {
				return fmt.Errorf("marshal %s: %w", event, err)
			}
			s.pluginManager.NotifyPlugin(ctx, name, event, payload, s.id)
		}
	}
	if s.runtime.noTools {
		return errs
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	disabled := make(map[string]bool)
	for _, tool := range s.agent.State().Tools {
		disabled[tool.Name] = tool.Exposure == agent.ToolDisabled
	}
	if err := s.reloadToolRegistryLocked(); err != nil {
		return errors.Join(errs, err)
	}
	// The transcript keeps a removal settled, so returning tools rejoin the
	// loadout explicitly, as a new registration would.
	var returning []string
	for _, tool := range s.agent.State().Tools {
		if disabled[tool.Name] && (tool.Exposure == "" || tool.Exposure == agent.ToolDirect || tool.Exposure == agent.ToolModelOnly) && (tool.DefaultActive == nil || *tool.DefaultActive) {
			returning = append(returning, tool.Name)
		}
	}
	if len(returning) > 0 {
		errs = errors.Join(errs, s.agent.SetActiveTools(append(s.agent.ActiveTools(), returning...)...))
	}
	return errs
}

// storeToggleLocked keeps session toggles in memory and writes project and
// global toggles to their settings.json. Caller must hold s.mu.
func (s *agentSession) storeToggleLocked(ctx context.Context, scope ai.ToggleScope, kind ai.ToggleKind, name string, enabled bool) error {
	var path string
	switch scope {
	case ai.ToggleSession:
		s.pluginToggles.Set(kind, name, enabled)
		return nil
	case ai.ToggleProject:
		if !s.settings.ProjectResourcesTrustedAt(s.cwd) {
			return errors.New("project settings are not trusted in this directory")
		}
		path = filepath.Join(s.cwd, ".tau", "settings.json")
	case ai.ToggleGlobal:
		path = filepath.Join(paths.ConfigDir(), "settings.json")
	default:
		return fmt.Errorf("unknown scope %q", scope)
	}
	if err := ai.UpdateSettings(ctx, path, map[string]any{"enabled": map[ai.ToggleKind]map[string]bool{kind: {name: enabled}}}); err != nil {
		return fmt.Errorf("save %s toggle: %w", scope, err)
	}
	s.settings = s.settings.WithToggle(scope, kind, name, enabled)
	s.runtime.replaceSettings(s.settings)
	s.pluginManager.SetSettings(s.settings)
	return nil
}
