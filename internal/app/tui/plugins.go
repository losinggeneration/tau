package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/tui/autocomplete"
)

const pluginsUsage = "Usage: /plugins [enable|disable <plugin|tool> [--session|--project|--global]]"

// cmdPlugins lists plugins and their tools, or toggles one by name.
func cmdPlugins(app *App, args string) {
	defer app.refreshChat()
	toggler, ok := app.sess.(sdk.PluginToggler)
	if !ok {
		app.conv.AddError("plugins: this session has no plugins")
		return
	}
	fields := strings.Fields(args)
	if len(fields) == 0 {
		app.conv.AddSystem(formatPluginStates(toggler.PluginStates()))
		return
	}
	scope := ai.ToggleSession
	if len(fields) == 3 {
		flag, found := strings.CutPrefix(fields[2], "--")
		scope = ai.ToggleScope(flag)
		if !found || !slices.Contains([]ai.ToggleScope{ai.ToggleSession, ai.ToggleProject, ai.ToggleGlobal}, scope) {
			app.conv.AddError(pluginsUsage)
			return
		}
	}
	if len(fields) < 2 || len(fields) > 3 || (fields[0] != "enable" && fields[0] != "disable") {
		app.conv.AddError(pluginsUsage)
		return
	}
	toggle := toggler.EnablePlugin
	if fields[0] == "disable" {
		toggle = toggler.DisablePlugin
	}
	if err := toggle(app.ctxOrBackground(), fields[1], scope); err != nil {
		app.conv.AddError(fmt.Sprintf("plugins: %v", err))
		return
	}
	app.conv.AddSystem(fmt.Sprintf("%sd %s at %s scope. The model sees the change from its next request.", strings.ToUpper(fields[0][:1])+fields[0][1:], fields[1], scope))
}

func formatPluginStates(states []sdk.PluginToggleState) string {
	if len(states) == 0 {
		return "No plugins loaded."
	}
	var lines []string
	for _, state := range states {
		line := state.Name + ": disabled"
		if state.Enabled {
			line = state.Name + ": enabled"
		}
		if state.Scope != "" {
			line += " by " + string(state.Scope)
		}
		if state.Plugin != "" {
			lines = append(lines, "  "+line)
			continue
		}
		if state.Loaded {
			line += ", running"
		} else {
			line += ", not loaded"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// completePluginsArgs completes the action, then plugin and tool names, then the scope.
func completePluginsArgs(app *App, prefix string) []autocomplete.Item {
	fields := strings.Fields(prefix)
	if strings.HasSuffix(prefix, " ") || len(fields) == 0 {
		fields = append(fields, "")
	}
	var candidates []string
	switch len(fields) {
	case 1:
		candidates = []string{"enable ", "disable "}
	case 2:
		toggler, ok := app.sess.(sdk.PluginToggler)
		if !ok {
			return nil
		}
		for _, state := range toggler.PluginStates() {
			candidates = append(candidates, state.Name+" ")
		}
	case 3:
		candidates = []string{"--session", "--project", "--global"}
	}
	head := strings.Join(fields[:len(fields)-1], " ")
	if head != "" {
		head += " "
	}
	var items []autocomplete.Item
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, fields[len(fields)-1]) {
			items = append(items, autocomplete.Item{Value: head + candidate, Label: strings.TrimSpace(candidate)})
		}
	}
	return items
}
