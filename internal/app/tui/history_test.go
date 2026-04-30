package tui

import (
	"reflect"
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
)

func TestHistorySearchItemsBackwardStartFromCurrentHistoryPosition(t *testing.T) {
	entries := []string{"one", "two", "three", "four"}
	items := historySearchItems(entries, "", historySearchBackward, 2)
	got := dialogItemValues(items)
	want := []string{"two", "one", "four", "three"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order=%v, want %v", got, want)
	}
}

func TestHistorySearchItemsForwardStartFromCurrentHistoryPosition(t *testing.T) {
	entries := []string{"one", "two", "three", "four"}
	items := historySearchItems(entries, "", historySearchForward, 2)
	got := dialogItemValues(items)
	want := []string{"three", "four", "one", "two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order=%v, want %v", got, want)
	}
}

func TestHistorySearchItemsFilterAndDisplayFirstLine(t *testing.T) {
	entries := []string{"alpha\nsecond line", "beta", "alphabet"}
	items := historySearchItems(entries, "alpha", historySearchBackward, len(entries))
	if len(items) != 2 {
		t.Fatalf("items=%#v, want 2", items)
	}
	if items[0].Name != "alphabet" || items[0].Value != "alphabet" {
		t.Fatalf("first item=%#v", items[0])
	}
	if items[1].Name != "alpha" || items[1].Value != "alpha\nsecond line" {
		t.Fatalf("second item=%#v", items[1])
	}
}

func TestOpenHistorySearchAppliesFocusedItemToEditor(t *testing.T) {
	a := &App{
		conv:         NewConversationModel(),
		editor:       newPromptEditorForTest(),
		dialogPicker: tauui.NewDialogPicker(),
	}
	a.dialogPicker.SetOnCancel(a.dismissDialog)
	a.editor.Model().AddHistory("first")
	a.editor.Model().AddHistory("second")
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	a.loopCtx = ctx

	if !a.openHistorySearch(historySearchBackward, ctx) {
		t.Fatalf("openHistorySearch returned false")
	}
	if !a.dialogPicker.Shown() {
		t.Fatalf("dialog picker not shown")
	}
	a.dialogPicker.HandleAction(int(tauui.ActionActivate), ctx)
	if got := a.editor.Text(); got != "second" {
		t.Fatalf("editor text=%q, want selected history entry", got)
	}
	if a.dialogPicker.Shown() {
		t.Fatalf("dialog picker still shown after selection")
	}
}

func TestOpenHistorySearchSeedsFilterWithCurrentEntry(t *testing.T) {
	a := &App{
		conv:         NewConversationModel(),
		editor:       newPromptEditorForTest(),
		dialogPicker: tauui.NewDialogPicker(),
	}
	a.dialogPicker.SetOnCancel(a.dismissDialog)
	a.editor.Model().AddHistory("find this one")
	a.editor.Model().AddHistory("alpha")
	a.editor.Model().AddHistory("beta")
	a.editor.Model().HistoryPrev()
	if a.editor.Model().Text() != "beta" {
		t.Fatalf("seeded text=%q, want latest history entry", a.editor.Model().Text())
	}

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	a.loopCtx = ctx

	if !a.openHistorySearch(historySearchBackward, ctx) {
		t.Fatalf("openHistorySearch returned false")
	}
	if got := a.dialogPicker.Filter(); got != "beta" {
		t.Fatalf("picker filter=%q, want seeded with current entry text", got)
	}
}

func TestHistorySearchSeedUsesDraftWhenNotInHistory(t *testing.T) {
	if got := historySearchSeed(historySearchBackward, 0, []string{"alpha", "beta"}, "fresh prompt"); got != "fresh prompt" {
		t.Fatalf("seed=%q, want draft text", got)
	}
}

func dialogItemValues(items []tauui.DialogPickerItem) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.Value
	}
	return out
}
