package keybindings

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
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
// raw object mapping action ids to strings or string arrays, or {"keybindings": {...}}.
func LoadConfig(r io.Reader) (Config, error) {
	if r == nil {
		return Config{}, nil
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("keybindings: read config: %w", err)
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\ufeff")), &entries); err != nil {
		return nil, fmt.Errorf("keybindings: decode config: %w", err)
	}
	if wrapped, ok := entries["keybindings"]; ok {
		if err := json.Unmarshal(wrapped, &entries); err != nil {
			return nil, fmt.Errorf("keybindings: parse config: %w", err)
		}
	}
	out := Config{}
	for id, value := range entries {
		if canonical, ok := legacyActionNames[id]; ok {
			if _, present := entries[canonical]; present {
				continue
			}
			id = canonical
		}
		if id == "$schema" || string(value) == "null" {
			continue
		}
		var keys []string
		var key string
		if err := json.Unmarshal(value, &key); err == nil {
			keys = []string{key}
		} else if err := json.Unmarshal(value, &keys); err != nil {
			continue
		}
		if slices.ContainsFunc(keys, func(key string) bool { _, err := CompileKey(key); return err != nil }) {
			continue
		}
		out[id] = dedupe(keys)
	}
	return out, nil
}

var legacyActionNames = map[string]string{
	"cursorUp":                 "tui.editor.cursorUp",
	"cursorDown":               "tui.editor.cursorDown",
	"cursorLeft":               "tui.editor.cursorLeft",
	"cursorRight":              "tui.editor.cursorRight",
	"cursorWordLeft":           "tui.editor.cursorWordLeft",
	"cursorWordRight":          "tui.editor.cursorWordRight",
	"cursorLineStart":          "tui.editor.cursorLineStart",
	"cursorLineEnd":            "tui.editor.cursorLineEnd",
	"jumpForward":              "tui.editor.jumpForward",
	"jumpBackward":             "tui.editor.jumpBackward",
	"pageUp":                   "tui.editor.pageUp",
	"pageDown":                 "tui.editor.pageDown",
	"deleteCharBackward":       "tui.editor.deleteCharBackward",
	"deleteCharForward":        "tui.editor.deleteCharForward",
	"deleteWordBackward":       "tui.editor.deleteWordBackward",
	"deleteWordForward":        "tui.editor.deleteWordForward",
	"deleteToLineStart":        "tui.editor.deleteToLineStart",
	"deleteToLineEnd":          "tui.editor.deleteToLineEnd",
	"yank":                     "tui.editor.yank",
	"yankPop":                  "tui.editor.yankPop",
	"undo":                     "tui.editor.undo",
	"newLine":                  "tui.input.newLine",
	"submit":                   "tui.input.submit",
	"tab":                      "tui.input.tab",
	"copy":                     "tui.input.copy",
	"selectUp":                 "tui.select.up",
	"selectDown":               "tui.select.down",
	"selectPageUp":             "tui.select.pageUp",
	"selectPageDown":           "tui.select.pageDown",
	"selectConfirm":            "tui.select.confirm",
	"selectCancel":             "tui.select.cancel",
	"interrupt":                "app.interrupt",
	"clear":                    "app.clear",
	"exit":                     "app.exit",
	"suspend":                  "app.suspend",
	"cycleThinkingLevel":       "app.thinking.cycle",
	"cycleModelForward":        "app.model.cycleForward",
	"cycleModelBackward":       "app.model.cycleBackward",
	"selectModel":              "app.model.select",
	"expandTools":              "app.tools.expand",
	"toggleThinking":           "app.thinking.toggle",
	"toggleSessionNamedFilter": "app.session.toggleNamedFilter",
	"externalEditor":           "app.editor.external",
	"followUp":                 "app.message.followUp",
	"dequeue":                  "app.message.dequeue",
	"pasteImage":               "app.clipboard.pasteImage",
	"newSession":               "app.session.new",
	"tree":                     "app.session.tree",
	"fork":                     "app.session.fork",
	"resume":                   "app.session.resume",
	"treeFoldOrUp":             "app.tree.foldOrUp",
	"treeUnfoldOrDown":         "app.tree.unfoldOrDown",
	"treeEditLabel":            "app.tree.editLabel",
	"treeToggleLabelTimestamp": "app.tree.toggleLabelTimestamp",
	"toggleSessionPath":        "app.session.togglePath",
	"toggleSessionSort":        "app.session.toggleSort",
	"renameSession":            "app.session.rename",
	"deleteSession":            "app.session.delete",
	"deleteSessionNoninvasive": "app.session.deleteNoninvasive",
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

// reservedActions protects editor-global keys from extension shortcuts.
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
	"app.message.copy":        {},

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
