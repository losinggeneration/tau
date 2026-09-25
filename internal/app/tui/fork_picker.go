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
		a.conv.AddError("fork: session switching is unavailable")
		a.refreshChat()
		return
	}
	if a.selectorMode != selectorModeFork {
		lister, ok := a.sess.(sdk.ForkEntryLister)
		if !ok {
			a.forkEntries = nil
		} else {
			entries, err := lister.GetForkEntries()
			if err != nil {
				a.conv.AddError("fork: " + err.Error())
				a.refreshChat()
				return
			}
			a.forkEntries = entries
		}
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
	a.selectorMode = selectorModeFork
	a.invalidateSelectorLayout(wasShown, wasShown && a.selector.MinSize() != wasSize, ctx)
}

func (a *App) forkSelectorItems() []tauui.SelectorItem {
	items := make([]tauui.SelectorItem, 0, len(a.forkEntries))
	for _, entry := range a.forkEntries {
		if !a.forkShowAll && entry.Role != "user" {
			continue
		}
		text := strings.Join(strings.Fields(entry.Text), " ")
		if text == "" {
			text = "(no text)"
		}
		role := entry.Role
		if role == "user" {
			role = "you"
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
