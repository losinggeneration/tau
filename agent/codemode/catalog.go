package codemode

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/losinggeneration/tau/agent"
)

// CatalogDescription lists non-deferred tools by namespace within an estimated
// token budget. A negative budget lists every tool.
func CatalogDescription(tools []agent.Tool, budget int) string {
	type entry struct {
		name, section string
		cost          int
	}
	type group struct {
		namespace      *agent.ToolNamespace
		entries, queue []entry
	}
	groups := []group{{}}
	for _, tool := range tools {
		if tool.Name == ToolName || tool.Exposure == agent.ToolDeferred {
			continue
		}
		index := 0
		if tool.Namespace != nil {
			index = slices.IndexFunc(groups, func(g group) bool { return g.namespace != nil && g.namespace.Name == tool.Namespace.Name })
			if index < 0 {
				index = len(groups)
				groups = append(groups, group{namespace: tool.Namespace})
			}
		}
		id := Identifier(tool.Name)
		heading := "### `" + id + "`"
		if id != tool.Name {
			heading += " (`" + tool.Name + "`)"
		}
		output := tool.OutputSchema
		if output == nil {
			output = textOutputSchema
		}
		section := heading + "\n" + strings.TrimSpace(ToolSample(tool.Name, tool.Description, tool.Parameters, output))
		groups[index].entries = append(groups[index].entries, entry{name: tool.Name, section: section, cost: (utf8.RuneCountInString(section) + 3) / 4})
	}
	slices.SortStableFunc(groups, func(a, b group) int {
		if a.namespace == nil {
			if b.namespace == nil {
				return 0
			}
			return -1
		}
		if b.namespace == nil {
			return 1
		}
		return strings.Compare(a.namespace.Name, b.namespace.Name)
	})
	shown := make(map[string]bool)
	for i := range groups {
		groups[i].queue = slices.Clone(groups[i].entries)
		slices.SortStableFunc(groups[i].queue, func(a, b entry) int { return cmp.Compare(a.cost, b.cost) })
	}
	for {
		progressed := false
		for i := range groups {
			queue := groups[i].queue
			if len(queue) == 0 {
				continue
			}
			next := queue[0]
			if budget >= 0 && next.cost > budget {
				groups[i].queue = nil
				continue
			}
			if budget >= 0 {
				budget -= next.cost
			}
			shown[next.name] = true
			groups[i].queue = queue[1:]
			progressed = true
		}
		if !progressed {
			break
		}
	}
	var sections []string
	for _, g := range groups {
		visible := slices.DeleteFunc(slices.Clone(g.entries), func(e entry) bool { return !shown[e.name] })
		if g.namespace != nil {
			listing := ""
			if len(visible) == 0 {
				listing = " (tools not listed)"
			} else if len(visible) != len(g.entries) {
				listing = " (some tools not listed)"
			}
			heading := fmt.Sprintf("## %s%s", g.namespace.Name, listing)
			if description := strings.TrimSpace(g.namespace.Description); description != "" {
				heading += "\n" + description
			}
			sections = append(sections, heading)
		}
		for _, e := range visible {
			sections = append(sections, e.section)
		}
	}
	if len(sections) == 0 {
		return Description
	}
	return Description + "\n\nNested tools:\n\n" + strings.Join(sections, "\n\n")
}

// PrepareLoadout describes script calls in on mode. In only mode, direct tools
// stay callable but their declarations are hidden from the provider.
func PrepareLoadout(loadout agent.ToolLoadout, mode string, budget int) agent.ToolLoadoutChanges {
	changes := agent.ToolLoadoutChanges{Descriptions: make(map[string]string)}
	var listed []agent.Tool
	for _, tool := range loadout.Callable {
		if tool.Name == ToolName {
			continue
		}
		direct := tool.Exposure == "" || tool.Exposure == agent.ToolDirect
		declared := slices.ContainsFunc(loadout.Declared, func(t agent.Tool) bool { return t.Name == tool.Name })
		if mode == "only" {
			if direct && declared {
				changes.HiddenDeclarations = append(changes.HiddenDeclarations, tool.Name)
			}
		} else if declared {
			changes.Descriptions[tool.Name] = strings.TrimSpace(tool.Description) + "\n\nCodemode: `tools." + Identifier(tool.Name) + "(args)` resolves to " + describeOutput(tool.OutputSchema) + "."
		}
		if mode == "only" || !direct {
			listed = append(listed, tool)
		}
	}
	changes.Descriptions[ToolName] = CatalogDescription(listed, budget)
	return changes
}

func describeOutput(schema map[string]any) string {
	if schema == nil {
		return "a string"
	}
	rendered := OutputType(schema)
	if rendered == "string" {
		return "a string"
	}
	if fields := describeFields(schema, 1); fields != "" {
		rendered = fields
	}
	return "`" + strings.Join(strings.Fields(rendered), " ") + "`"
}

func describeFields(schema map[string]any, depth int) string {
	properties, ok := schema["properties"].(map[string]any)
	if schema["type"] != "object" || !ok {
		return ""
	}
	var fields []string
	required := make(map[string]bool)
	if names, ok := list(schema["required"]); ok {
		for _, name := range names {
			if value, ok := name.(string); ok {
				required[value] = true
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(properties)) {
		field := name
		if !required[name] {
			field += "?"
		}
		if nested, ok := properties[name].(map[string]any); ok && depth > 0 {
			if description := describeFields(nested, depth-1); description != "" {
				field += ": " + description
			}
		}
		fields = append(fields, field)
	}
	return "{ " + strings.Join(fields, ", ") + " }"
}
