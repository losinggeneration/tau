package tui

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"unicode"

	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/keybindings"
)

// The application action registry (app.md §12). Physical keys resolve to these
// ids through a keybindings.Manager, so user configuration can rebind or
// disable any of them; the Go constants stay fixed.
//
// Only ids tau actually dispatches are registered. The spec names several more
// (app.clear, app.tools.expand, app.editor.external, app.clipboard.pasteImage,
// app.session.*, app.tree.*); those have no handler yet, and registering an id
// whose key does nothing is worse than leaving it out — it would claim the
// keystroke from the toolkit and silently swallow it.
const (
	actionIDExit                  = "app.exit"
	actionIDSuspend               = "app.suspend"
	actionIDModelCycleForward     = "app.model.cycleForward"
	actionIDModelCycleBack        = "app.model.cycleBackward"
	actionIDModelSelect           = "app.model.select"
	actionIDThinkingCycle         = "app.thinking.cycle"
	actionIDThinkingToggle        = "app.thinking.toggle"
	actionIDMessageFollowUp       = "app.message.followUp"
	actionIDMessageDequeue        = "app.message.dequeue"
	actionIDHeaderExpand          = "app.header.expand"
	actionIDChatScrollBottom      = "app.chat.scrollBottom"
	actionIDHistorySearchBackward = "tau.input.historySearchBackward"
	actionIDHistorySearchForward  = "tau.input.historySearchForward"
	actionIDPaste                 = "app.clipboard.pasteImage"
	actionIDMessageCopy           = "app.message.copy"

	// Scoped-model dialog actions. Not spec ids — the dialog is tau's — but
	// they go through the same registry so they are configurable too.
	actionIDScopedModelsAll      = "app.scopedModels.all"
	actionIDScopedModelsClear    = "app.scopedModels.clear"
	actionIDScopedModelsProvider = "app.scopedModels.provider"
	actionIDScopedModelsSave     = "app.scopedModels.save"
	actionIDScopedModelsMoveUp   = "app.scopedModels.moveUp"
	actionIDScopedModelsMoveDown = "app.scopedModels.moveDown"
)

// appKeybindingDefinitions returns the app-layer action definitions and their
// default keys. These are layered over the toolkit registry rather than merged
// into it: the toolkit ids (tui.input.submit, tui.select.confirm, ...) stay
// under the toolkit's own resolution so Enter/Tab/arrow routing keeps its
// context sensitivity.
//
// One id is defined with no default key on purpose:
//
//   - app.message.followUp: most terminals encode Shift+Enter and Alt+Enter as
//     the same ESC+Enter sequence, and keeping that as "insert newline" matters
//     more than a completion-only queue shortcut.
//
// app.suspend ships on Ctrl+Z, the conventional suspend key. The terminal is
// in raw mode (ISIG off), so Ctrl+Z arrives as a keystroke rather than
// SIGTSTP and must be bound here to suspend at all; undo keeps its readline
// binding, Ctrl+- (tui.editor.undo).
func appKeybindingDefinitions() map[string]keybindings.Definition {
	return map[string]keybindings.Definition{
		actionIDExit:                  {DefaultKeys: []string{"ctrl+c"}, Description: "Clear input, then exit"},
		actionIDSuspend:               {DefaultKeys: []string{"ctrl+z"}, Description: "Suspend to the shell"},
		actionIDModelCycleForward:     {DefaultKeys: []string{"ctrl+p"}, Description: "Cycle to the next model"},
		actionIDModelCycleBack:        {DefaultKeys: []string{"ctrl+shift+p"}, Description: "Cycle to the previous model"},
		actionIDModelSelect:           {DefaultKeys: []string{"alt+m"}, Description: "Open the model picker"},
		actionIDThinkingCycle:         {DefaultKeys: []string{"ctrl+t"}, Description: "Cycle the thinking level"},
		actionIDThinkingToggle:        {DefaultKeys: []string{"alt+t"}, Description: "Toggle thinking off and back on"},
		actionIDMessageFollowUp:       {DefaultKeys: nil, Description: "Queue a follow-up message"},
		actionIDMessageDequeue:        {DefaultKeys: []string{"alt+backspace"}, Description: "Drop the first queued message"},
		actionIDHeaderExpand:          {DefaultKeys: []string{"ctrl+o"}, Description: "Expand or collapse startup help"},
		actionIDChatScrollBottom:      {DefaultKeys: []string{"ctrl+end"}, Description: "Scroll the chat to the bottom"},
		actionIDHistorySearchBackward: {DefaultKeys: []string{"ctrl+r"}, Description: "Search prompt history backward"},
		actionIDHistorySearchForward:  {DefaultKeys: []string{"ctrl+s"}, Description: "Search prompt history forward"},
		actionIDPaste:                 {DefaultKeys: []string{"ctrl+v"}, Description: "Paste from clipboard"},
		actionIDMessageCopy:           {DefaultKeys: []string{"ctrl+x"}, Description: "Copy the last reply or the sign-in URL"},

		actionIDScopedModelsAll:      {DefaultKeys: []string{"ctrl+a"}, Description: "Scoped models: enable all"},
		actionIDScopedModelsClear:    {DefaultKeys: []string{"ctrl+x"}, Description: "Scoped models: clear"},
		actionIDScopedModelsProvider: {DefaultKeys: []string{"ctrl+p"}, Description: "Scoped models: toggle provider"},
		actionIDScopedModelsSave:     {DefaultKeys: []string{"ctrl+s"}, Description: "Scoped models: save"},
		actionIDScopedModelsMoveUp:   {DefaultKeys: []string{"alt+up"}, Description: "Scoped models: move up"},
		actionIDScopedModelsMoveDown: {DefaultKeys: []string{"alt+down"}, Description: "Scoped models: move down"},
	}
}

// appActions maps action ids to the Go action constants Root dispatches on.
func appActions() map[string]tauui.Action {
	return map[string]tauui.Action{
		actionIDExit:                  AppActionExit,
		actionIDSuspend:               AppActionSuspend,
		actionIDModelCycleForward:     AppActionModelCycleForward,
		actionIDModelCycleBack:        AppActionModelCycleBackward,
		actionIDModelSelect:           AppActionModelSelect,
		actionIDThinkingCycle:         AppActionThinkingCycle,
		actionIDThinkingToggle:        AppActionThinkingToggle,
		actionIDMessageFollowUp:       AppActionMessageFollowUp,
		actionIDMessageDequeue:        AppActionMessageDequeue,
		actionIDHeaderExpand:          AppActionHeaderExpand,
		actionIDChatScrollBottom:      AppActionChatScrollBottom,
		actionIDHistorySearchBackward: AppActionHistorySearchBackward,
		actionIDHistorySearchForward:  AppActionHistorySearchForward,
		actionIDPaste:                 AppActionPaste,
		actionIDMessageCopy:           AppActionMessageCopy,

		actionIDScopedModelsAll:      AppActionScopedModelsAll,
		actionIDScopedModelsClear:    AppActionScopedModelsClear,
		actionIDScopedModelsProvider: AppActionScopedModelsProvider,
		actionIDScopedModelsSave:     AppActionScopedModelsSave,
		actionIDScopedModelsMoveUp:   AppActionScopedModelsMoveUp,
		actionIDScopedModelsMoveDown: AppActionScopedModelsMoveDown,
	}
}

// appKeybindingsPath locates keybindings.json beside settings.json. Keeping it
// a separate file rather than a settings key matches the toolkit's LoadConfig
// shape and keeps a syntax error in one from taking out the other.
func appKeybindingsPath(settingsPath string) string {
	if settingsPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(settingsPath), "keybindings.json")
}

// newAppKeybindingManager builds the app-layer manager, applying user overrides
// from path when it exists. A missing file is the normal case and is not an
// error; a malformed one is reported so the user learns their config was
// ignored rather than silently getting defaults.
func newAppKeybindingManager(path string) (*keybindings.Manager, error) {
	defs := appKeybindingDefinitions()
	if path == "" {
		return keybindings.New(defs, nil), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return keybindings.New(defs, nil), nil
		}
		return keybindings.New(defs, nil), fmt.Errorf("read keybindings: %w", err)
	}
	user, err := keybindings.LoadConfig(bytes.NewReader(data))
	if err != nil {
		return keybindings.New(defs, nil), err
	}
	return keybindings.New(defs, user), nil
}

func (a *App) reloadKeybindings() {
	if a.settingsPath == "" {
		return
	}
	manager, err := newAppKeybindingManager(appKeybindingsPath(a.settingsPath))
	if err != nil {
		a.conv.AddError(fmt.Sprintf("keybindings: using defaults: %v", err))
	}
	if a.keybindings == nil {
		a.keybindings = manager
	} else {
		a.keybindings.SetUserBindings(manager.UserBindings())
	}
	keybindings.SetDefaultManager(keybindings.NewDefault(manager.UserBindings()))
}

// normalizeKeystrokeForRouting folds a keystroke into the form CompileKey
// produces. Terminals disagree about whether Ctrl+Shift+P arrives as an
// uppercase rune, a shift modifier, or both; the compiled bindings always use
// the lowercase rune, so match against that and keep the modifiers intact.
func normalizeKeystrokeForRouting(ks tauui.Keystroke) tauui.Keystroke {
	if ks.Key == tauui.KeyRune && ks.Rune != 0 {
		ks.Rune = unicode.ToLower(ks.Rune)
	}
	return ks
}

// defaultAppKeybindings is the registry a zero-value appKeymap resolves
// against. It is not keybindings.DefaultManager(): that holds the *toolkit*
// definitions, so routing app keys through it would match nothing. Built once
// because the definitions are constant.
var defaultAppKeybindings = sync.OnceValue(func() *keybindings.Manager {
	return keybindings.New(appKeybindingDefinitions(), nil)
})

// resolveAppAction routes a keystroke through the app registry. It returns the
// matching action, or false when no app action claims the key — in which case
// the caller falls through to the toolkit keymap.
//
// allow filters candidate ids *before* matching rather than after. That
// distinction matters because one key may be bound to several actions — Ctrl+P
// is both app.model.cycleForward and app.scopedModels.provider — and the caller
// uses allow to say which of them is in scope right now. Rejecting after the
// match would discard the key entirely instead of falling through to the
// action the context actually wants.
func (k appKeymap) resolveAppAction(ks tauui.Keystroke, allow func(id string) bool) (tauui.Action, bool) {
	manager := k.manager
	if manager == nil {
		manager = defaultAppKeybindings()
	}
	normalized := normalizeKeystrokeForRouting(ks)
	actions := appActions()

	ids := make([]string, 0, len(actions))
	for id := range actions {
		if allow != nil && !allow(id) {
			continue
		}
		ids = append(ids, id)
	}
	// Sorted so a key bound to two in-scope actions resolves the same way every
	// time rather than following Go's map iteration order.
	sort.Strings(ids)

	for _, id := range ids {
		if manager.Matches(normalized, id) {
			return actions[id], true
		}
	}
	return tauui.ActionNone, false
}
