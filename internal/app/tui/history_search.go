package tui

import (
	"fmt"
	"strings"

	tauui "github.com/losinggeneration/tau/tui"
)

type historySearchDirection int

const (
	historySearchBackward historySearchDirection = -1
	historySearchForward  historySearchDirection = 1
)

func (a *App) openHistorySearch(direction historySearchDirection, ctx *tauui.Ctx) bool {
	if a == nil || a.editor == nil || a.dialogPicker == nil || ctx == nil || ctx.FocusedID != a.editor.ID() {
		return false
	}
	entries := a.editor.Model().HistoryEntries()
	if len(entries) == 0 {
		return true
	}
	a.closeDialogComponent()
	a.dialogPicker.SetComponent(nil)
	a.dialogPicker.Reset()
	a.dialogPicker.SetHeader(historySearchHeader(direction))
	a.dialogPicker.SetVisible(12)
	seed := historySearchSeed(direction, a.editor.Model().HistoryIndex(), entries, a.editor.Model().Text())
	a.dialogPicker.SetFilter(seed)
	a.dialogPicker.SetItems(historySearchItems(entries, seed, direction, a.editor.Model().HistoryIndex()))
	a.dialogPicker.SetOnChange(func(filter string) {
		a.dialogPicker.SetItems(historySearchItems(entries, filter, direction, a.editor.Model().HistoryIndex()))
	})
	a.dialogPicker.SetOnSelect(func(_ int, item tauui.DialogPickerItem) {
		a.applyHistorySearchItem(item)
	})
	a.dialogPicker.SetOnCancel(a.dismissDialog)
	a.dialogPicker.SetShown(true)
	if ctx.InvalidateLayout != nil {
		ctx.InvalidateLayout()
	}
	if ctx.InvalidateAll != nil {
		ctx.InvalidateAll()
	}
	if a.app != nil {
		a.app.SetFocus(a.dialogPicker.ID())
	}
	return true
}

func (a *App) applyHistorySearchItem(item tauui.DialogPickerItem) {
	if a == nil || a.editor == nil {
		return
	}
	ctx := a.loopContext()
	if ctx == nil {
		return
	}
	value := item.Value
	if value == "" {
		value = item.Name
	}
	a.editor.SetText(ctx, value)
	a.dismissDialogCtx(ctx)
}

func historySearchHeader(direction historySearchDirection) string {
	if direction == historySearchForward {
		return "history search forward"
	}
	return "history search backward"
}

func historySearchItems(entries []string, filter string, direction historySearchDirection, start int) []tauui.DialogPickerItem {
	if len(entries) == 0 {
		return nil
	}
	if start < 0 || start > len(entries) {
		start = len(entries)
	}
	filter = strings.ToLower(strings.TrimSpace(filter))
	items := make([]tauui.DialogPickerItem, 0, len(entries))
	for _, idx := range historySearchOrder(len(entries), start, direction) {
		entry := entries[idx]
		if filter != "" && !strings.Contains(strings.ToLower(entry), filter) {
			continue
		}
		items = append(items, tauui.DialogPickerItem{
			Name:        firstHistoryLine(entry),
			Description: fmt.Sprintf("#%d", idx+1),
			Value:       entry,
		})
	}
	return items
}

func historySearchOrder(length, start int, direction historySearchDirection) []int {
	if length <= 0 {
		return nil
	}
	if start < 0 || start > length {
		start = length
	}
	order := make([]int, 0, length)
	if direction == historySearchForward {
		for i := start; i < length; i++ {
			order = append(order, i)
		}
		for i := 0; i < start; i++ {
			order = append(order, i)
		}
		return order
	}
	for i := start - 1; i >= 0; i-- {
		order = append(order, i)
	}
	for i := length - 1; i >= start; i-- {
		order = append(order, i)
	}
	return order
}

func firstHistoryLine(entry string) string {
	entry = strings.TrimRight(entry, "\n")
	line, _, _ := strings.Cut(entry, "\n")
	return line
}

func historySearchSeed(direction historySearchDirection, historyIndex int, entries []string, draft string) string {
	if direction != historySearchBackward {
		return ""
	}
	if historyIndex < 0 || historyIndex >= len(entries) {
		return ""
	}
	current := entries[historyIndex]
	if draft != "" && draft == current {
		return firstHistoryLine(current)
	}
	return firstHistoryLine(draft)
}
