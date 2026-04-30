package selectors

import (
	"strings"

	"github.com/losinggeneration/tau/tui/fuzzy"
)

type (
	Session struct{ ID, Name, Path, Updated string }
	Model   struct {
		ID, Provider, Description     string
		SupportsVision, SupportsTools bool
		MaxThinking                   string
	}
)
type Tool struct {
	Name, Description string
	Enabled           bool
}
type Theme struct {
	Name, Description string
	Dark              bool
}

type Item struct {
	Value, Label, Description string
	Selected                  bool
}

func Sessions(sessions []Session, query string) []Item {
	items := make([]Item, 0, len(sessions))
	for _, s := range sessions {
		label := s.Name
		if label == "" {
			label = s.ID
		}
		desc := strings.TrimSpace(strings.Join(nonempty(s.Path, s.Updated), " · "))
		items = append(items, Item{Value: s.ID, Label: label, Description: desc})
	}
	return filter(items, query)
}

func Models(models []Model, query, current string) []Item {
	items := make([]Item, 0, len(models))
	for _, m := range models {
		desc := strings.TrimSpace(strings.Join(nonempty(m.Provider, m.Description, capDesc(m)), " · "))
		items = append(items, Item{Value: m.ID, Label: m.ID, Description: desc, Selected: m.ID == current})
	}
	return filter(items, query)
}

func Tools(tools []Tool, query string) []Item {
	items := make([]Item, 0, len(tools))
	for _, tool := range tools {
		mark := "off"
		if tool.Enabled {
			mark = "on"
		}
		items = append(items, Item{Value: tool.Name, Label: tool.Name, Description: mark + " · " + tool.Description, Selected: tool.Enabled})
	}
	return filter(items, query)
}

func Themes(themes []Theme, query, current string) []Item {
	items := make([]Item, 0, len(themes))
	for _, th := range themes {
		mode := "light"
		if th.Dark {
			mode = "dark"
		}
		items = append(items, Item{Value: th.Name, Label: th.Name, Description: mode + " · " + th.Description, Selected: th.Name == current})
	}
	return filter(items, query)
}

func ToggleTool(tools []Tool, name string) []Tool {
	out := append([]Tool(nil), tools...)
	for i := range out {
		if out[i].Name == name {
			out[i].Enabled = !out[i].Enabled
		}
	}
	return out
}

func filter(items []Item, query string) []Item {
	if strings.TrimSpace(query) == "" {
		return append([]Item(nil), items...)
	}
	return fuzzy.Filter(items, query, func(i Item) string { return i.Label + " " + i.Description })
}

func nonempty(v ...string) []string {
	out := []string{}
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func capDesc(m Model) string {
	caps := []string{}
	if m.SupportsTools {
		caps = append(caps, "tools")
	}
	if m.SupportsVision {
		caps = append(caps, "vision")
	}
	if m.MaxThinking != "" {
		caps = append(caps, "thinking "+m.MaxThinking)
	}
	return strings.Join(caps, "/")
}
