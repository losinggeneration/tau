package tui

import (
	"slices"
	"strings"

	"github.com/losinggeneration/tau/ai"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/extensions"
	"github.com/losinggeneration/tau/tui/keybindings"
)

// Handlers for the §12 application actions that are not tied to a dialog.
//
// Every id registered in appKeybindingDefinitions has a handler here or in
// Root.HandleAction. That is the rule the registry documents: an id with no
// handler would claim its keystroke from the toolkit and then swallow it.

// thinkingLevelOrDefault normalises a thinking level for the ladder. The
// session reports "" when the model has no reasoning support and sometimes when
// the level has simply never been set; both are "off" as far as cycling goes.
func thinkingLevelOrDefault(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))
	if !ai.IsValidThinkingLevel(level) {
		return "off"
	}
	return level
}

// currentThinkingLevel reports the session's thinking level as tracked by the
// footer, which is the same record /settings reads.
func (a *App) currentThinkingLevel() string {
	if a == nil || a.footer == nil || a.footer.data == nil {
		return "off"
	}
	return thinkingLevelOrDefault(a.footer.data.thinkingLevel)
}

// supportsThinking reports whether the active model has reasoning support. The
// thinking actions are no-ops without it: sess.SetThinkingLevel would either
// error or silently do nothing, and either way changing a level the model
// cannot use is a lie in the footer.
func (a *App) supportsThinking() bool {
	return a != nil && a.footer != nil && a.footer.data != nil && a.footer.data.supportsReasoning
}

// setThinkingLevel routes through applySettingsChange rather than calling
// sess.SetThinkingLevel directly, so the keybinding and the settings list share
// one apply path: the session call, the footer update, the error reporting when
// the session refuses, and the editor rule colour.
//
// Deliberately no transcript line. §10.3 makes the footer's thinking suffix and
// the editor rule the standing indicator, and a system row per keypress would
// bury the conversation under chrome. Nor does this call refreshFooterStats:
// that recomputes the level from the session's reported stats, which lag a
// just-applied change by a turn.
func (a *App) setThinkingLevel(level string) {
	applySettingsChange(a, "settings.thinkingLevel", level)
}

// cycleThinkingLevel steps one rung up the ladder, wrapping past xhigh back to
// off (app.thinking.cycle). Reports whether anything changed.
func (a *App) cycleThinkingLevel() bool {
	if !a.supportsThinking() {
		return false
	}
	current := a.currentThinkingLevel()
	i := slices.Index(ai.ThinkingLevels, current)
	if i < 0 {
		i = 0
	}
	next := ai.ThinkingLevels[(i+1)%len(ai.ThinkingLevels)]
	if next == current {
		return false
	}
	a.setThinkingLevel(next)
	return true
}

// toggleThinkingLevel flips between off and the last non-off level
// (app.thinking.toggle). Turning thinking off and back on returns to the effort
// the user was working at rather than to a fixed default — that is the whole
// point of having a toggle distinct from the cycle.
func (a *App) toggleThinkingLevel() bool {
	if !a.supportsThinking() {
		return false
	}
	if current := a.currentThinkingLevel(); current != "off" {
		// Record it here too, not only in the apply path: at startup the level
		// comes from the session's reported stats, so a user who has not
		// touched /settings or the cycle key this run would otherwise toggle
		// off and come back to the default instead of where they were.
		a.rememberThinkingLevel(current)
		a.setThinkingLevel("off")
		return true
	}
	restore := a.lastThinkingLevel
	if !ai.IsValidThinkingLevel(restore) || restore == "off" {
		restore = ai.DefaultThinkingLevel
	}
	a.setThinkingLevel(restore)
	return true
}

// rememberThinkingLevel records a non-off level as the one a later toggle
// restores. Called from the single apply path, so every way of choosing a level
// — the cycle key, the toggle key, the /settings row — feeds the same memory.
func (a *App) rememberThinkingLevel(level string) {
	if a == nil || level == "" || level == "off" {
		return
	}
	a.lastThinkingLevel = level
}

// suspend stops the process and hands the terminal back to the shell
// (app.suspend). rovel restores cooked mode on the way out, re-enters raw mode
// on SIGCONT, and re-queries the terminal size, so all this has to do is ask
// and repaint.
//
// Known deviation: §14 also requires suspend to ignore SIGINT while stopped and
// restore the handler on SIGCONT. tau cannot do that from here — interactive
// SIGINT is owned by the CLI's signal.NotifyContext, and signal.Ignore would
// undo that registration with no way to re-establish it, turning a later Ctrl+C
// into a hard terminate instead of a graceful shutdown. Recorded in
// app-vis.plan rather than half-implemented.
func (a *App) suspend(ctx *tauui.Ctx) bool {
	if a == nil || ctx == nil || ctx.Suspend == nil {
		return false
	}
	if err := ctx.Suspend(); err != nil {
		a.conv.AddError("suspend: " + err.Error())
		a.refreshChat()
		return true
	}
	if ctx.InvalidateAll != nil {
		ctx.InvalidateAll()
	}
	return true
}

// openModelSelector opens the model picker (app.model.select). It goes through
// cmdModel so the key and /model cannot drift apart.
func (a *App) openModelSelector() bool {
	if a == nil {
		return false
	}
	cmdModel(a, "")
	return true
}

// resolveExtensionShortcut matches a keystroke against the registered extension
// shortcuts (§12: extension shortcuts are checked before app keybindings in the
// editor). Reserved actions are exempt — an extension may not take Enter, Esc,
// Ctrl+C, or the other §12-protected keys out from under the app.
//
// Records the matched shortcut on the App because tauui.Keymap.Resolve can only
// return an Action, and one Go action has to stand for every extension
// shortcut. Resolve and the subsequent HandleAction run on the same loop turn,
// so the value is consumed before another key can be resolved.
func (k appKeymap) resolveExtensionShortcut(ks tauui.Keystroke) (tauui.Action, bool) {
	if k.app == nil || k.app.extensions == nil {
		return tauui.ActionNone, false
	}
	manager := k.manager
	if manager == nil {
		manager = defaultAppKeybindings()
	}
	normalized := normalizeKeystrokeForRouting(ks)
	if manager.ClaimsReservedKey(normalized) || keybindings.DefaultManager().ClaimsReservedKey(normalized) {
		return tauui.ActionNone, false
	}
	shortcut, ok := k.app.extensions.ShortcutForKeystroke(normalized)
	if !ok || shortcut.Handler == nil {
		return tauui.ActionNone, false
	}
	k.app.setPendingShortcut(shortcut)
	return AppActionExtensionShortcut, true
}

// setPendingShortcut records the shortcut the keymap matched.
func (a *App) setPendingShortcut(s extensions.Shortcut) {
	a.pendingMu.Lock()
	a.pendingShortcut = &s
	a.pendingMu.Unlock()
}

// runPendingShortcut runs the shortcut the keymap matched and clears it.
// Returns false when there is nothing pending, so the key falls through rather
// than being swallowed.
func (a *App) runPendingShortcut(ctx *tauui.Ctx) bool {
	if a == nil {
		return false
	}
	a.pendingMu.Lock()
	shortcut := a.pendingShortcut
	a.pendingShortcut = nil
	a.pendingMu.Unlock()

	if shortcut == nil || shortcut.Handler == nil {
		return false
	}
	shortcut.Handler()
	// An extension handler is arbitrary code — it may have written to the
	// transcript, the editor, or a status. Repaint rather than trusting it to.
	if ctx != nil && ctx.InvalidateAll != nil {
		ctx.InvalidateAll()
	}
	return true
}
