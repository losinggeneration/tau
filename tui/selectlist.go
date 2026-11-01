package tui

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/losinggeneration/rovel/ui"
	"github.com/losinggeneration/tau/tui/keybindings"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

type SelectItem struct {
	Value       string
	Label       string
	Description string
}

type SelectListTheme struct {
	Cursor        string
	StyleSelected func(string) string
}

type SelectListTruncateContext struct {
	Text        string
	MaxWidth    int
	ColumnWidth int
	Item        SelectItem
	IsSelected  bool
}

type SelectListLayoutOptions struct {
	MinPrimaryWidth int
	MaxPrimaryWidth int
	MaxVisible      int
	TruncatePrimary func(SelectListTruncateContext) string
}

type SelectList struct {
	items             []SelectItem
	filtered          []SelectItem
	selected          int
	filter            string
	layout            SelectListLayoutOptions
	theme             SelectListTheme
	OnConfirm         func(SelectItem)
	OnCancel          func()
	OnSelectionChange func(SelectItem)
}

func NewSelectList(items []SelectItem, opts SelectListLayoutOptions, theme SelectListTheme) *SelectList {
	if opts.MinPrimaryWidth == 0 {
		opts.MinPrimaryWidth = opts.MaxPrimaryWidth
	}
	if opts.MaxPrimaryWidth == 0 {
		opts.MaxPrimaryWidth = opts.MinPrimaryWidth
	}
	if opts.MinPrimaryWidth == 0 {
		opts.MinPrimaryWidth, opts.MaxPrimaryWidth = 32, 32
	}
	opts.MinPrimaryWidth, opts.MaxPrimaryWidth = max(1, min(opts.MinPrimaryWidth, opts.MaxPrimaryWidth)), max(1, max(opts.MinPrimaryWidth, opts.MaxPrimaryWidth))
	if opts.MaxVisible <= 0 {
		opts.MaxVisible = 5
	}
	if theme.Cursor == "" {
		theme.Cursor = "→ "
	}
	cp := append([]SelectItem(nil), items...)
	return &SelectList{items: cp, filtered: cp, layout: opts, theme: theme}
}

func (l *SelectList) SetFilter(filter string) {
	if l == nil {
		return
	}
	l.filter = filter
	if filter == "" {
		l.filtered = append([]SelectItem(nil), l.items...)
	} else {
		l.filtered = nil
		lower := cases.Lower(language.Und)
		f := lower.String(filter)
		for _, item := range l.items {
			if strings.HasPrefix(lower.String(item.Value), f) {
				l.filtered = append(l.filtered, item)
			}
		}
	}
	l.selected = 0
}

// SetSelectedIndex clamps the selection without firing navigation callbacks.
func (l *SelectList) SetSelectedIndex(index int) {
	if l != nil {
		l.selected = max(0, min(index, len(l.filtered)-1))
	}
}

func (l *SelectList) SelectedIndex() int {
	if l == nil {
		return 0
	}
	return l.selected
}

func (l *SelectList) Items() []SelectItem {
	if l == nil {
		return nil
	}
	return append([]SelectItem(nil), l.filtered...)
}

func (l *SelectList) Render(width int) []string {
	if l == nil || width <= 0 {
		return nil
	}
	items := l.filtered
	if len(items) == 0 {
		return []string{padRightNoEllipsis("  No matching commands", width)}
	}
	start := l.selected - l.layout.MaxVisible/2
	if start < 0 {
		start = 0
	}
	if maxStart := len(items) - l.layout.MaxVisible; maxStart >= 0 && start > maxStart {
		start = maxStart
	}
	end := start + l.layout.MaxVisible
	if end > len(items) {
		end = len(items)
	}
	primary := 0
	for _, item := range items {
		label := displayLabel(item)
		if w := TextWidth(label); w > primary {
			primary = w
		}
	}
	primary += 2
	if primary < l.layout.MinPrimaryWidth {
		primary = l.layout.MinPrimaryWidth
	}
	if primary > l.layout.MaxPrimaryWidth {
		primary = l.layout.MaxPrimaryWidth
	}
	out := make([]string, 0, end-start+1)
	for i := start; i < end; i++ {
		item := items[i]
		prefix := "  "
		if i == l.selected {
			prefix = l.theme.Cursor
		}
		prefixWidth := TextWidth(prefix)
		var line string
		if width > 40 && item.Description != "" {
			description := strings.Join(strings.FieldsFunc(item.Description, func(r rune) bool { return r == '\r' || r == '\n' }), " ")
			description = strings.TrimFunc(description, func(r rune) bool { return r == '\ufeff' || r != '\u0085' && unicode.IsSpace(r) })
			column := max(1, min(primary, width-prefixWidth-4))
			label := l.truncatePrimary(item, i == l.selected, max(1, column-2), column)
			spacing := strings.Repeat(" ", max(1, column-TextWidth(label)))
			remaining := width - prefixWidth - TextWidth(label) - len(spacing) - 2
			if description != "" && remaining > 10 {
				line = prefix + label + spacing + ANSITruncate(description, remaining, false)
			}
		}
		if line == "" {
			limit := max(0, width-prefixWidth-2)
			line = prefix + l.truncatePrimary(item, i == l.selected, limit, limit)
		}

		line = padRightNoEllipsis(line, width)
		if i == l.selected && l.theme.StyleSelected != nil {
			line = l.theme.StyleSelected(line)
		}
		out = append(out, line)
	}
	if start > 0 || end < len(items) {
		out = append(out, padRightNoEllipsis(ANSITruncate("  ("+strconv.Itoa(l.selected+1)+"/"+strconv.Itoa(len(items))+")", max(0, width-2), false), width))
	}
	return out
}

func (l *SelectList) truncatePrimary(item SelectItem, selected bool, width, column int) string {
	text := displayLabel(item)
	if l.layout.TruncatePrimary != nil {
		text = l.layout.TruncatePrimary(SelectListTruncateContext{Text: text, MaxWidth: width, ColumnWidth: column, Item: item, IsSelected: selected})
	}
	return ANSITruncate(text, width, false)
}

func displayLabel(item SelectItem) string {
	if item.Label != "" {
		return item.Label
	}
	return item.Value
}

func (l *SelectList) Handle(e Event, _ *Ctx) bool {
	ke, ok := e.(KeyEvent)
	if !ok {
		return false
	}
	kb := keybindings.DefaultManager()
	key := ui.KeystrokeOf(ke)
	switch {
	case kb.Matches(key, "tui.select.up"):
		if len(l.filtered) > 0 {
			l.selected = (l.selected + len(l.filtered) - 1) % len(l.filtered)
			if l.OnSelectionChange != nil {
				l.OnSelectionChange(l.filtered[l.selected])
			}
		}
		return true
	case kb.Matches(key, "tui.select.down"):
		if len(l.filtered) > 0 {
			l.selected = (l.selected + 1) % len(l.filtered)
			if l.OnSelectionChange != nil {
				l.OnSelectionChange(l.filtered[l.selected])
			}
		}
		return true
	case kb.Matches(key, "tui.select.confirm"):
		if len(l.filtered) > 0 && l.OnConfirm != nil {
			l.OnConfirm(l.filtered[l.selected])
		}
		return true
	case kb.Matches(key, "tui.select.cancel"):
		if l.OnCancel != nil {
			l.OnCancel()
		}
		return true
	}
	return false
}
