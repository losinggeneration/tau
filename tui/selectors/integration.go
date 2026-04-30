package selectors

import "sort"

// StoreState is the app-facing selector state used by menu integrations.
type StoreState struct {
	Sessions []Session
	Models   []Model
	Tools    []Tool
	Themes   []Theme
	Model    string
	Theme    string
}

// SortSessionsNewestFirst returns sessions ordered by Updated descending.
func SortSessionsNewestFirst(sessions []Session) []Session {
	out := append([]Session(nil), sessions...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out
}

func (s StoreState) SessionItems(query string) []Item {
	return Sessions(SortSessionsNewestFirst(s.Sessions), query)
}
func (s StoreState) ModelItems(query string) []Item { return Models(s.Models, query, s.Model) }
func (s StoreState) ToolItems(query string) []Item  { return Tools(s.Tools, query) }
func (s StoreState) ThemeItems(query string) []Item { return Themes(s.Themes, query, s.Theme) }

func (s StoreState) WithToolToggled(name string) StoreState {
	s.Tools = ToggleTool(s.Tools, name)
	return s
}
func (s StoreState) WithModel(id string) StoreState   { s.Model = id; return s }
func (s StoreState) WithTheme(name string) StoreState { s.Theme = name; return s }
