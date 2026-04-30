package keybindings

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/losinggeneration/rovel/ui"
)

// ActionRouter maps raw key input to action ids using a Manager. It is a small
// app-facing adapter so higher-level TUIs can route keys through the shared
// keybinding registry instead of hard-coded key checks.
type ActionRouter struct {
	manager *Manager
}

// NewActionRouter creates an action router. A nil manager uses DefaultManager.
func NewActionRouter(manager *Manager) *ActionRouter {
	if manager == nil {
		manager = DefaultManager()
	}

	return &ActionRouter{manager: manager}
}

// Resolve returns the first action whose configured keys match a normalized
// rovel keystroke. Actions are checked in sorted id order for deterministic
// behavior.
func (r *ActionRouter) Resolve(k ui.Keystroke) (string, bool) {
	if r == nil || r.manager == nil {
		return "", false
	}

	ids := make([]string, 0, len(r.manager.resolved))
	for id := range r.manager.resolved {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if r.manager.Matches(k, id) {
			return id, true
		}
	}

	return "", false
}

// LoadConfig reads user keybindings from JSON. The accepted shape is either a
// raw object mapping action ids to string arrays, or {"keybindings": {...}}.
func LoadConfig(r io.Reader) (Config, error) {
	if r == nil {
		return Config{}, nil
	}

	var raw json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("keybindings: decode config: %w", err)
	}

	var direct Config
	if err := json.Unmarshal(raw, &direct); err == nil && direct != nil {
		return normalizeConfig(direct), nil
	}

	var wrapped struct {
		Keybindings Config `json:"keybindings"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("keybindings: parse config: %w", err)
	}

	return normalizeConfig(wrapped.Keybindings), nil
}

func normalizeConfig(in Config) Config {
	out := Config{}
	for id, keys := range in {
		out[id] = dedupe(keys)
	}
	return out
}

// ConflictDiagnostics returns stable human-readable conflict messages suitable
// for settings/help UIs.
func (m *Manager) ConflictDiagnostics() []string {
	if m == nil {
		return nil
	}

	conflicts := m.Conflicts()
	out := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		out = append(out, fmt.Sprintf("%s is assigned to %s", conflict.Key, strings.Join(conflict.Keybindings, ", ")))
	}

	return out
}

// reservedActions is the exact set app.md §12 protects from extension-shortcut
// shadowing: twelve application actions plus five toolkit actions.
//
// It is a specific subset, not "everything the toolkit defines". Extension
// shortcuts are checked *before* app keybindings in the editor, so an extension
// binding ctrl+k would otherwise silently take over. But cursor movement,
// selection paging and the rest are deliberately *not* protected — an extension
// is allowed to rebind those, and reserving all 31 toolkit ids (which is what
// this used to do) forbade shortcuts the spec permits while leaving every
// app action, including app.exit, unprotected.
//
// The app.* ids live here rather than in internal/app/tui because this is where
// the check runs; they are plain strings, and §12 fixes them as stable.
var reservedActions = map[string]struct{}{
	// Application actions (§12).
	"app.interrupt":           {},
	"app.clear":               {},
	"app.exit":                {},
	"app.suspend":             {},
	"app.thinking.cycle":      {},
	"app.thinking.toggle":     {},
	"app.model.cycleForward":  {},
	"app.model.cycleBackward": {},
	"app.model.select":        {},
	"app.tools.expand":        {},
	"app.editor.external":     {},
	"app.message.followUp":    {},

	// Toolkit editor/select actions (§12).
	"tui.input.submit":           {},
	"tui.select.confirm":         {},
	"tui.select.cancel":          {},
	"tui.input.copy":             {},
	"tui.editor.deleteToLineEnd": {},
}

// IsReservedAction reports whether action is protected from extension-shortcut
// shadowing (app.md §12). Picker-specific and other app actions are not
// reserved and may be shadowed.
func IsReservedAction(action string) bool {
	_, ok := reservedActions[action]
	return ok
}

// ReservedActions returns the protected action ids, sorted. Callers use it to
// work out which *keys* are off-limits to extension shortcuts, which is the
// form the §12 rule actually takes: the protection is against a shortcut key
// shadowing a reserved action's key, not against reusing its id.
func ReservedActions() []string {
	out := make([]string, 0, len(reservedActions))
	for id := range reservedActions {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ClaimsReservedKey reports whether k is bound to any reserved action in this
// manager. Extension shortcuts are dispatched ahead of app keybindings, so this
// is the gate that stops one from swallowing Enter, Esc, or Ctrl+C.
//
// A nil manager reports false: with no registry there is nothing to protect,
// and refusing every key would disable extension shortcuts entirely.
func (m *Manager) ClaimsReservedKey(k ui.Keystroke) bool {
	if m == nil {
		return false
	}
	for id := range reservedActions {
		if m.Matches(k, id) {
			return true
		}
	}
	return false
}

// CanRegisterExtensionBinding validates that an extension shortcut action does
// not shadow a reserved TUI action id.
func CanRegisterExtensionBinding(action string) error {
	if IsReservedAction(action) {
		return fmt.Errorf("keybindings: %s is reserved", action)
	}

	return nil
}
