package keymap

import (
	"sort"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
)

// Binding pairs a key context and keystroke.
type Binding struct {
	Context KeyContext
	Key     Keystroke
}

// Conflict records a user-configured key claimed by more than one action.
type Conflict struct {
	Binding Binding
	Actions []ui.Action
}

type bindingState struct {
	keys []Binding
	set  bool
}

// Manager resolves semantic actions from default bindings plus user overrides.
// Undefined user bindings use defaults. An explicitly empty binding disables
// the action. Conflicts are reported only among user-configured actions.
type Manager struct {
	defaults map[ui.Action][]Binding
	user     map[ui.Action]bindingState
}

var _ Keymap = (*Manager)(nil)

// NewManager creates a manager from default bindings.
func NewManager(defaults map[ui.Action][]Binding) *Manager {
	m := &Manager{
		defaults: make(map[ui.Action][]Binding, len(defaults)),
		user:     make(map[ui.Action]bindingState),
	}
	for action, bindings := range defaults {
		m.defaults[action] = dedupeBindings(bindings)
	}

	return m
}

// NewDefaultManager creates a manager with the package's standard editor
// bindings.
func NewDefaultManager() *Manager {
	return NewManager(DefaultBindings())
}

// Set replaces the user binding for action. Passing an empty slice explicitly
// disables the action.
func (m *Manager) Set(action ui.Action, bindings []Binding) {
	if m.user == nil {
		m.user = make(map[ui.Action]bindingState)
	}

	m.user[action] = bindingState{keys: dedupeBindings(bindings), set: true}
}

// Clear removes the user override for action, restoring its default.
func (m *Manager) Clear(action ui.Action) {
	delete(m.user, action)
}

// Resolve implements Keymap.
func (m *Manager) Resolve(ctx KeyContext, k Keystroke) (ui.Action, bool) {
	if m == nil {
		return ui.ActionNone, false
	}

	for _, action := range m.actions() {
		for _, binding := range m.Keys(action) {
			if binding.Context == ctx && binding.Key == k {
				return action, true
			}
		}
	}

	return ui.ActionNone, false
}

// Keys returns the effective bindings for action.
func (m *Manager) Keys(action ui.Action) []Binding {
	if m == nil {
		return nil
	}

	if state, ok := m.user[action]; ok && state.set {
		return cloneBindings(state.keys)
	}

	return cloneBindings(m.defaults[action])
}

// Conflicts returns keys claimed by more than one user-configured action.
func (m *Manager) Conflicts() []Conflict {
	if m == nil {
		return nil
	}

	claims := map[Binding][]ui.Action{}
	for action, state := range m.user {
		if !state.set || len(state.keys) == 0 {
			continue
		}
		for _, binding := range state.keys {
			claims[binding] = append(claims[binding], action)
		}
	}

	out := []Conflict{}
	for binding, actions := range claims {
		if len(actions) < 2 {
			continue
		}
		sort.Slice(actions, func(i, j int) bool { return actions[i] < actions[j] })
		out = append(out, Conflict{Binding: binding, Actions: actions})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Binding.Context != out[j].Binding.Context {
			return out[i].Binding.Context < out[j].Binding.Context
		}
		if out[i].Binding.Key.Key != out[j].Binding.Key.Key {
			return out[i].Binding.Key.Key < out[j].Binding.Key.Key
		}
		if out[i].Binding.Key.Rune != out[j].Binding.Key.Rune {
			return out[i].Binding.Key.Rune < out[j].Binding.Key.Rune
		}
		return out[i].Binding.Key.Mod < out[j].Binding.Key.Mod
	})

	return out
}

func (m *Manager) actions() []ui.Action {
	seen := map[ui.Action]bool{}
	for action := range m.defaults {
		seen[action] = true
	}
	for action := range m.user {
		seen[action] = true
	}

	actions := make([]ui.Action, 0, len(seen))
	for action := range seen {
		actions = append(actions, action)
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i] < actions[j] })

	return actions
}

func dedupeBindings(bindings []Binding) []Binding {
	seen := map[Binding]bool{}
	out := make([]Binding, 0, len(bindings))
	for _, binding := range bindings {
		if seen[binding] {
			continue
		}
		seen[binding] = true
		out = append(out, binding)
	}

	return out
}

func cloneBindings(bindings []Binding) []Binding {
	out := make([]Binding, len(bindings))
	copy(out, bindings)

	return out
}

// B is a convenience constructor for a binding.
func B(ctx KeyContext, key event.Key, r rune, mod event.ModMask) Binding {
	return Binding{Context: ctx, Key: ui.Keystroke{Key: key, Rune: r, Mod: mod}}
}
