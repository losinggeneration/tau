package keybindings

import (
	"sort"
	"sync"

	"github.com/losinggeneration/rovel/ui"
)

type Definition struct {
	DefaultKeys []string
	Description string
}
type (
	Config   map[string][]string
	Conflict struct {
		Key         string
		Keybindings []string
	}
)

type Manager struct {
	definitions map[string]Definition
	user        Config
	resolved    Config
	compiled    map[string][]Binding
	conflicts   []Conflict
}

var defaultState = struct {
	sync.RWMutex
	manager *Manager
}{}

func DefaultManager() *Manager {
	defaultState.RLock()
	manager := defaultState.manager
	defaultState.RUnlock()
	if manager != nil {
		return manager
	}

	defaultState.Lock()
	defer defaultState.Unlock()
	if defaultState.manager == nil {
		defaultState.manager = NewDefault(nil)
	}
	return defaultState.manager
}

func SetDefaultManager(manager *Manager) {
	if manager == nil {
		manager = NewDefault(nil)
	}
	defaultState.Lock()
	defaultState.manager = manager
	defaultState.Unlock()
}

func TUIDefinitions() map[string]Definition {
	return map[string]Definition{
		"tui.editor.historyPrevious": {nil, "Select previous prompt history entry"},
		"tui.editor.historyNext":     {nil, "Select next prompt history entry"},
		"tui.editor.cursorUp":        {[]string{"up"}, "Move cursor up"}, "tui.editor.cursorDown": {[]string{"down"}, "Move cursor down"}, "tui.editor.cursorLeft": {[]string{"left", "ctrl+b"}, "Move cursor left"}, "tui.editor.cursorRight": {[]string{"right", "ctrl+f"}, "Move cursor right"}, "tui.editor.cursorWordLeft": {[]string{"alt+left", "ctrl+left", "alt+b"}, "Move cursor word left"}, "tui.editor.cursorWordRight": {[]string{"alt+right", "ctrl+right", "alt+f"}, "Move cursor word right"}, "tui.editor.cursorLineStart": {[]string{"home", "ctrl+a"}, "Move to line start"}, "tui.editor.cursorLineEnd": {[]string{"end", "ctrl+e"}, "Move to line end"}, "tui.editor.jumpForward": {[]string{"ctrl+]"}, "Jump forward to character"}, "tui.editor.jumpBackward": {[]string{"ctrl+alt+]"}, "Jump backward to character"}, "tui.editor.pageUp": {[]string{"pageUp"}, "Page up"}, "tui.editor.pageDown": {[]string{"pageDown"}, "Page down"}, "tui.editor.deleteCharBackward": {[]string{"backspace"}, "Delete character backward"}, "tui.editor.deleteCharForward": {[]string{"delete", "ctrl+d"}, "Delete character forward"}, "tui.editor.deleteWordBackward": {[]string{"ctrl+w", "alt+backspace"}, "Delete word backward"}, "tui.editor.deleteWordForward": {[]string{"alt+d", "alt+delete"}, "Delete word forward"}, "tui.editor.deleteToLineStart": {[]string{"ctrl+u"}, "Delete to line start"}, "tui.editor.deleteToLineEnd": {[]string{"ctrl+k"}, "Delete to line end"}, "tui.editor.yank": {[]string{"ctrl+y"}, "Yank"}, "tui.editor.yankPop": {[]string{"alt+y"}, "Yank pop"}, "tui.editor.undo": {[]string{"ctrl+-"}, "Undo"}, "tui.input.newLine": {[]string{"shift+enter"}, "Insert newline"}, "tui.input.submit": {[]string{"enter"}, "Submit input"}, "tui.input.tab": {[]string{"tab"}, "Tab / autocomplete"}, "tui.input.copy": {[]string{"ctrl+c"}, "Copy selection"}, "tui.select.up": {[]string{"up"}, "Move selection up"}, "tui.select.down": {[]string{"down"}, "Move selection down"}, "tui.select.pageUp": {[]string{"pageUp"}, "Selection page up"}, "tui.select.pageDown": {[]string{"pageDown"}, "Selection page down"}, "tui.select.confirm": {[]string{"enter"}, "Confirm selection"}, "tui.select.cancel": {[]string{"escape", "ctrl+c"}, "Cancel selection"},
	}
}

func New(defs map[string]Definition, user Config) *Manager {
	m := &Manager{definitions: map[string]Definition{}, user: Config{}}
	for id, d := range defs {
		d.DefaultKeys = dedupe(d.DefaultKeys)
		m.definitions[id] = d
	}
	for id, ks := range user {
		m.user[id] = dedupe(ks)
	}
	m.rebuild()
	return m
}
func NewDefault(user Config) *Manager { return New(TUIDefinitions(), user) }
func (m *Manager) Matches(k ui.Keystroke, id string) bool {
	for _, binding := range m.compiled[id] {
		if matchKeystroke(k, binding.Keystroke) {
			return true
		}
	}
	return false
}
func (m *Manager) Keys(id string) []string { return append([]string(nil), m.resolved[id]...) }
func (m *Manager) Bindings(id string) []Binding {
	return append([]Binding(nil), m.compiled[id]...)
}

func (m *Manager) Definition(id string) (Definition, bool) {
	d, ok := m.definitions[id]
	d.DefaultKeys = append([]string(nil), d.DefaultKeys...)
	return d, ok
}

func (m *Manager) Conflicts() []Conflict {
	out := make([]Conflict, len(m.conflicts))
	for i, c := range m.conflicts {
		out[i] = Conflict{Key: c.Key, Keybindings: append([]string(nil), c.Keybindings...)}
	}
	return out
}

func (m *Manager) SetUserBindings(user Config) {
	m.user = Config{}
	for id, ks := range user {
		m.user[id] = dedupe(ks)
	}
	m.rebuild()
}
func (m *Manager) UserBindings() Config     { return cloneConfig(m.user) }
func (m *Manager) ResolvedBindings() Config { return cloneConfig(m.resolved) }
func (m *Manager) rebuild() {
	m.resolved = Config{}
	m.compiled = map[string][]Binding{}
	claims := map[string][]string{}
	for id, ks := range m.user {
		if _, ok := m.definitions[id]; !ok {
			continue
		}
		for _, k := range ks {
			claims[k] = append(claims[k], id)
		}
	}
	m.conflicts = nil
	for k, ids := range claims {
		if len(ids) > 1 {
			sort.Strings(ids)
			m.conflicts = append(m.conflicts, Conflict{k, append([]string(nil), ids...)})
		}
	}
	sort.Slice(m.conflicts, func(i, j int) bool { return m.conflicts[i].Key < m.conflicts[j].Key })
	for id, d := range m.definitions {
		if ks, ok := m.user[id]; ok {
			m.resolved[id] = append([]string(nil), ks...)
		} else {
			m.resolved[id] = append([]string(nil), d.DefaultKeys...)
		}
		m.compiled[id] = compileBindings(m.resolved[id])
	}
}

func compileBindings(keys []string) []Binding {
	out := make([]Binding, 0, len(keys))
	for _, key := range keys {
		ks, err := CompileKey(key)
		if err != nil {
			continue
		}
		out = append(out, Binding{Key: key, Keystroke: ks})
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func cloneConfig(c Config) Config {
	out := Config{}
	for k, v := range c {
		out[k] = append([]string(nil), v...)
	}
	return out
}
