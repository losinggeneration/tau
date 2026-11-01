package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/tui/keybindings"
	"github.com/stretchr/testify/assert"
)

func TestSelectListUsesConfiguredBindings(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.select.up": {"ctrl+p"}, "tui.select.down": {"ctrl+n"}, "tui.select.confirm": {"ctrl+o"}, "tui.select.cancel": {"ctrl+q"}}))
	list := NewSelectList([]SelectItem{{Value: "one"}, {Value: "two"}}, SelectListLayoutOptions{}, SelectListTheme{})
	var confirmed string
	var canceled int
	list.OnConfirm = func(item SelectItem) { confirmed = item.Value }
	list.OnCancel = func() { canceled++ }
	for _, key := range []KeyEvent{{Key: KeyDown}, {Key: KeyUp}, {Key: KeyEnter}, {Key: KeyEsc}, {Key: KeyCtrlC}, {Key: KeyRune, Rune: 'n', Mod: ModAlt}} {
		assert.False(t, list.Handle(key, nil), "disabled key %+v", key)
	}
	for _, key := range []KeyEvent{{Key: KeyRune, Rune: 'n', Mod: ModCtrl}, {Key: KeyRune, Rune: 'o', Mod: ModCtrl}, {Key: KeyRune, Rune: 'p', Mod: ModCtrl}, {Key: KeyRune, Rune: 'p', Mod: ModCtrl}, {Key: KeyRune, Rune: 'q', Mod: ModCtrl}} {
		assert.True(t, list.Handle(key, nil), "configured key %+v", key)
	}
	assert.Equal(t, "two", confirmed)
	assert.Equal(t, 1, list.SelectedIndex())
	assert.Equal(t, 1, canceled)
}

func TestSelectListEmptyNavigationAndDisabledBindings(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	list := NewSelectList(nil, SelectListLayoutOptions{}, SelectListTheme{})
	assert.True(t, list.Handle(KeyEvent{Key: KeyDown}, nil))
	assert.True(t, list.Handle(KeyEvent{Key: KeyUp}, nil))
	assert.Zero(t, list.SelectedIndex())
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.select.up": {}, "tui.select.down": {}, "tui.select.confirm": {}, "tui.select.cancel": {}}))
	for _, key := range []KeyEvent{{Key: KeyDown}, {Key: KeyUp}, {Key: KeyEnter}, {Key: KeyEsc}, {Key: KeyCtrlC}} {
		assert.False(t, list.Handle(key, nil))
	}
}

func TestSelectListSelectionAPIClampsAndNotifies(t *testing.T) {
	list := NewSelectList([]SelectItem{{Value: "one"}, {Value: "two"}, {Value: "three"}}, SelectListLayoutOptions{}, SelectListTheme{})
	var changed []string
	list.OnSelectionChange = func(item SelectItem) { changed = append(changed, item.Value) }
	list.SetSelectedIndex(100)
	assert.Equal(t, 2, list.SelectedIndex())
	assert.Empty(t, changed)
	list.Handle(KeyEvent{Key: KeyDown}, nil)
	list.Handle(KeyEvent{Key: KeyUp}, nil)
	assert.Equal(t, []string{"one", "three"}, changed)
	list.SetSelectedIndex(-100)
	assert.Zero(t, list.SelectedIndex())
	list.SetFilter("two")
	list.SetSelectedIndex(100)
	assert.Zero(t, list.SelectedIndex())
	list.Handle(KeyEvent{Key: KeyDown}, nil)
	assert.Equal(t, []string{"one", "three", "two"}, changed)
	list.SetFilter("missing")
	list.SetSelectedIndex(100)
	list.Handle(KeyEvent{Key: KeyDown}, nil)
	assert.Zero(t, list.SelectedIndex())
	assert.Len(t, changed, 3)
}

func TestSelectListFilteringRetainsOriginalItems(t *testing.T) {
	items := []SelectItem{{Value: "alpha", Label: "First"}, {Value: "beta", Label: "Second"}, {Value: "bravo", Label: "Third"}}
	list := NewSelectList(items, SelectListLayoutOptions{}, SelectListTheme{})
	list.SetFilter("b")
	assert.Equal(t, items[1:], list.Items())
	list.SetFilter("a")
	assert.Equal(t, items[:1], list.Items())
	list.SetFilter("")
	assert.Equal(t, items, list.Items())
	list.SetFilter("missing")
	assert.Empty(t, list.Items())
	list.SetFilter("B")
	assert.Equal(t, items[1:], list.Items())
	returned := list.Items()
	returned[0].Value = "changed"
	list.SetFilter("")
	assert.Equal(t, items, list.Items())
}

func TestSelectListFilterUsesUnicodeLowercasePrefixes(t *testing.T) {
	items := []SelectItem{{Value: "İstanbul"}, {Value: "ΟΣ"}, {Value: "İzmir", Label: "Other"}}
	list := NewSelectList(items, SelectListLayoutOptions{}, SelectListTheme{})
	for _, tc := range []struct {
		query string
		want  []SelectItem
	}{{"i\u0307", []SelectItem{items[0], items[2]}}, {"i\u0307s", items[:1]}, {"ος", items[1:2]}, {"οσ", nil}, {"other", nil}, {"", items}} {
		list.SetFilter(tc.query)
		assert.Equal(t, tc.want, list.Items(), "query %q", tc.query)
	}
}

func TestSelectListDescriptionsStayOnOneRow(t *testing.T) {
	for _, description := range []string{"first\r\n\nsecond\rthird", " \tfirst\nsecond\r\nthird\u00a0", "\ufefffirst\rsecond\nthird\ufeff"} {
		list := NewSelectList([]SelectItem{{Value: "one", Description: description}}, SelectListLayoutOptions{MinPrimaryWidth: 8, MaxPrimaryWidth: 8}, SelectListTheme{})
		rows := list.Render(60)
		assert.Len(t, rows, 1)
		assert.Equal(t, "→ one     first second third", strings.TrimRight(rows[0], " "))
		assert.NotContains(t, rows[0], "\r")
		assert.NotContains(t, rows[0], "\n")
		assert.Equal(t, 60, TextWidth(rows[0]))
		assert.Equal(t, description, list.Items()[0].Description)
	}
	list := NewSelectList([]SelectItem{{Value: "one", Description: "\u0085first\u0085"}}, SelectListLayoutOptions{MinPrimaryWidth: 8, MaxPrimaryWidth: 8}, SelectListTheme{})
	assert.Contains(t, list.Render(60)[0], "\u0085first\u0085")
}

func TestSelectListNormalizesPrimaryColumnBounds(t *testing.T) {
	for _, tc := range []struct {
		name             string
		opts             SelectListLayoutOptions
		min, max, column int
	}{
		{"defaults", SelectListLayoutOptions{}, 32, 32, 32},
		{"only minimum", SelectListLayoutOptions{MinPrimaryWidth: 40}, 40, 40, 40},
		{"only maximum", SelectListLayoutOptions{MaxPrimaryWidth: 8}, 8, 8, 8},
		{"range", SelectListLayoutOptions{MinPrimaryWidth: 1, MaxPrimaryWidth: 8}, 1, 8, 3},
		{"reversed range", SelectListLayoutOptions{MinPrimaryWidth: 8, MaxPrimaryWidth: 1}, 1, 8, 3},
		{"negative range", SelectListLayoutOptions{MinPrimaryWidth: -8, MaxPrimaryWidth: -1}, 1, 1, 1},
		{"negative minimum", SelectListLayoutOptions{MinPrimaryWidth: -8}, 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := NewSelectList([]SelectItem{{Value: "a", Description: "description"}}, tc.opts, SelectListTheme{})
			assert.Equal(t, tc.min, list.layout.MinPrimaryWidth)
			assert.Equal(t, tc.max, list.layout.MaxPrimaryWidth)
			assert.Equal(t, max(2, tc.column)+2, TextWidth(strings.SplitN(list.Render(100)[0], "description", 2)[0]))
		})
	}
}

func TestSelectListUsesAvailableWidthWithoutDescription(t *testing.T) {
	label := strings.Repeat("a", 50)
	for _, description := range []string{"", "description"} {
		list := NewSelectList([]SelectItem{{Value: label, Description: description}}, SelectListLayoutOptions{MinPrimaryWidth: 70}, SelectListTheme{})
		// A description that cannot fit falls back to the full label row.
		assert.Equal(t, "→ "+label, strings.TrimRight(list.Render(60)[0], " "))
	}
	list := NewSelectList([]SelectItem{{Value: label}}, SelectListLayoutOptions{}, SelectListTheme{})
	assert.Equal(t, "→ "+label, strings.TrimRight(list.Render(80)[0], " "))
	assert.Equal(t, "→ aaaa", strings.TrimRight(list.Render(8)[0], " "))
}

func TestSelectListDescriptionReservesGapAndRightMargin(t *testing.T) {
	list := NewSelectList([]SelectItem{{Value: strings.Repeat("a", 40), Description: strings.Repeat("b", 80)}}, SelectListLayoutOptions{}, SelectListTheme{})
	row := list.Render(60)[0]
	assert.Equal(t, "→ "+strings.Repeat("a", 30)+"  "+strings.Repeat("b", 24)+"  ", row)
	for width := 1; width <= 60; width++ {
		rows := list.Render(width)
		for _, row := range rows {
			assert.Equal(t, width, TextWidth(row), "width %d, row %q", width, row)
		}
	}
}

func TestSelectListPrimaryTruncationHookKeepsRowsBounded(t *testing.T) {
	items := []SelectItem{{Value: "long-internal-value", Label: "Display label", Description: "first"}, {Value: "short", Description: "second"}}
	var calls []SelectListTruncateContext
	list := NewSelectList(items, SelectListLayoutOptions{MinPrimaryWidth: 12, TruncatePrimary: func(ctx SelectListTruncateContext) string {
		calls = append(calls, ctx)
		return "\x1b[31m" + strings.Repeat("界", 40) + "\x1b[0m"
	}}, SelectListTheme{})
	rows := list.Render(80)
	assert.Len(t, calls, 2)
	assert.Equal(t, SelectListTruncateContext{Text: "Display label", MaxWidth: 10, ColumnWidth: 12, Item: items[0], IsSelected: true}, calls[0])
	assert.Equal(t, SelectListTruncateContext{Text: "short", MaxWidth: 10, ColumnWidth: 12, Item: items[1]}, calls[1])
	assert.Equal(t, 14, TextWidth(strings.SplitN(rows[0], "first", 2)[0]))
	assert.Equal(t, 14, TextWidth(strings.SplitN(rows[1], "second", 2)[0]))
	for _, row := range rows {
		assert.Equal(t, 80, TextWidth(row))
		assert.Contains(t, row, "\x1b[31m")
	}
	calls = nil
	rows = list.Render(12)
	assert.Len(t, calls, 2)
	assert.Equal(t, 8, calls[0].MaxWidth)
	assert.Equal(t, 8, calls[0].ColumnWidth)
	assert.Equal(t, "→ 界界界界  ", StripANSI(rows[0]))
	assert.Equal(t, 12, TextWidth(rows[1]))
}

func TestSelectListScrollCounterKeepsRightMargin(t *testing.T) {
	list := NewSelectList([]SelectItem{{Value: "a"}, {Value: "b"}, {Value: "c"}}, SelectListLayoutOptions{MaxVisible: 1}, SelectListTheme{})
	for _, width := range []int{6, 7, 8, 12} {
		rows := list.Render(width)
		assert.Equal(t, padRightNoEllipsis(ANSITruncate("  (1/3)", max(0, width-2), false), width), rows[1])
	}
}

func TestSelectListDefaultWindowShowsFiveChoices(t *testing.T) {
	items := make([]SelectItem, 8)
	for i := range items {
		items[i] = SelectItem{Value: strconv.Itoa(i)}
	}
	list := NewSelectList(items, SelectListLayoutOptions{}, SelectListTheme{})
	rows := list.Render(20)
	assert.Len(t, rows, 6)
	assert.Contains(t, rows[len(rows)-1], "(1/8)")
	list.SetSelectedIndex(7)
	rows = list.Render(20)
	assert.Contains(t, rows[0], "3")
	assert.Contains(t, rows[4], "→ 7")
	assert.Contains(t, rows[5], "(8/8)")
}
