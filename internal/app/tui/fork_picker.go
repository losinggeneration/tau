package tui

import (
	"strings"

	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

func (a *App) openForkSelector(ctx *tauui.Ctx) {
	if a == nil || a.selector == nil {
		return
	}
	if a.sessionFactory == nil {
		a.messageSelectorFailed = selectorModeFork
		a.conv.AddError("fork: session switching is unavailable")
		a.refreshChat()
		return
	}
	a.openMessageSelector(ctx, selectorModeFork, func() ([]sdk.ForkEntry, error) {
		lister, ok := a.sess.(sdk.ForkEntryLister)
		if !ok {
			return nil, nil
		}
		return lister.GetForkEntries()
	})
}

func (a *App) openForgetSelector(ctx *tauui.Ctx) {
	if a == nil || a.selector == nil {
		return
	}
	if sess := a.coordinated; sess != nil && a.selectorMode != selectorModeForget {
		a.loadCoordinatedForgetEntries(sess)
		return
	}
	a.showForgetSelector(ctx, func() ([]sdk.ForkEntry, error) {
		f, ok := a.sess.(sdk.Forgetter)
		if !ok {
			return nil, nil
		}
		return f.GetForgetEntries()
	})
}

func (a *App) showForgetSelector(ctx *tauui.Ctx, load func() ([]sdk.ForkEntry, error)) {
	a.openMessageSelector(ctx, selectorModeForget, load)
	if a.selectorMode == selectorModeForget && len(a.forkEntries) == 0 {
		a.dismissSelector()
		a.invalidateSelectorLayout(true, false, ctx)
		a.messageSelectorFailed = selectorModeForget
		a.conv.AddError("forget: no messages to forget")
		a.refreshChat()
	}
}

// loadCoordinatedForgetEntries asks the worker for entries off the UI loop.
// The picker opens if the editor is still on /forget when they arrive.
func (a *App) loadCoordinatedForgetEntries(sess *CoordinatedSession) {
	if a.coordinatedForgetLoading {
		return
	}
	a.coordinatedForgetLoading = true
	go func() {
		entries, err := sess.GetForgetEntries()
		a.post(func(*tauui.UpdateCtx) {
			a.coordinatedForgetLoading = false
			text := a.editor.Text()
			if a.coordinated != sess || text != "/forget" && !messageSelectorCommand(text, selectorModeForget) {
				return
			}
			a.showForgetSelector(a.loopContext(), func() ([]sdk.ForkEntry, error) { return entries, err })
		})
	}()
}

// openMessageSelector shows the message picker shared by /fork and /forget,
// focused on the newest entry. Entries load once per opening.
func (a *App) openMessageSelector(ctx *tauui.Ctx, mode selectorMode, load func() ([]sdk.ForkEntry, error)) {
	if a.selectorMode != mode {
		entries, err := load()
		if err != nil {
			a.messageSelectorFailed = mode
			a.conv.AddError(selectorModeName(mode) + ": " + err.Error())
			a.refreshChat()
			return
		}
		a.forkEntries = entries
		a.forkShowAll = false
	}
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		a.dismissDialogCtx(ctx)
	}
	wasShown := a.selector.Shown()
	wasSize := a.selector.MinSize()
	a.selector.SetItems(a.forkSelectorItems())
	a.selector.SetFocused(len(a.selector.Items()) - 1)
	a.selector.SetShown(true)
	a.selectorMode = mode
	a.messageSelectorFailed = selectorModeNone
	a.invalidateSelectorLayout(wasShown, wasShown && a.selector.MinSize() != wasSize, ctx)
}

// messageSelectorCommand reports whether text is mode's command followed by
// whitespace, which opens its picker while typing.
func messageSelectorCommand(text string, mode selectorMode) bool {
	command := "/" + selectorModeName(mode)
	return len(text) > len(command) && strings.HasPrefix(text, command) && (text[len(command)] == ' ' || text[len(command)] == '\t')
}

func selectorModeName(mode selectorMode) string {
	if mode == selectorModeForget {
		return "forget"
	}
	return "fork"
}

func (a *App) forkSelectorItems() []tauui.SelectorItem {
	items := make([]tauui.SelectorItem, 0, len(a.forkEntries))
	for _, entry := range a.forkEntries {
		if !a.forkShowAll && entry.Role != "user" && entry.Role != "compaction" {
			continue
		}
		text := strings.Join(strings.Fields(entry.Text), " ")
		if text == "" {
			text = "(no text)"
		}
		role := entry.Role
		switch role {
		case "user":
			role = "you"
		case "compaction":
			role = "summary"
		}
		items = append(items, tauui.SelectorItem{Name: text, Description: role, Value: entry.EntryID})
	}
	return items
}

func (a *App) toggleForkSelector(ctx *tauui.Ctx) {
	selected, _ := a.selector.FocusedItem()
	wasSize := a.selector.MinSize()
	a.forkShowAll = !a.forkShowAll
	items := a.forkSelectorItems()
	focus := len(items) - 1
	for i, item := range items {
		if item.Value == selected.Value {
			focus = i
			break
		}
	}
	a.selector.SetItems(items)
	a.selector.SetFocused(focus)
	if ctx != nil {
		if a.selector.MinSize() != wasSize && ctx.InvalidateLayout != nil {
			ctx.InvalidateLayout()
		}
		ctx.InvalidateAll()
	}
}
