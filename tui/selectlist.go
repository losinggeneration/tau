package tui

import (
	"strconv"
	"strings"
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

type SelectListLayoutOptions struct {
	MinPrimaryWidth int
	MaxPrimaryWidth int
	MaxVisible      int
}

type SelectList struct {
	items     []SelectItem
	filtered  []SelectItem
	selected  int
	filter    string
	layout    SelectListLayoutOptions
	theme     SelectListTheme
	OnConfirm func(SelectItem)
	OnCancel  func()
}

func NewSelectList(items []SelectItem, opts SelectListLayoutOptions, theme SelectListTheme) *SelectList {
	if opts.MinPrimaryWidth <= 0 {
		opts.MinPrimaryWidth = 32
	}
	if opts.MaxPrimaryWidth <= 0 {
		opts.MaxPrimaryWidth = 32
	}
	if opts.MaxVisible <= 0 {
		opts.MaxVisible = len(items)
	}
	if opts.MaxVisible <= 0 {
		opts.MaxVisible = 1
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
		l.filtered = l.filtered[:0]
		f := strings.ToLower(filter)
		for _, item := range l.items {
			if strings.HasPrefix(strings.ToLower(item.Value), f) {
				l.filtered = append(l.filtered, item)
			}
		}
	}
	l.selected = 0
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
		label := ANSITruncate(displayLabel(item), primary, true)
		line := prefix + padRightNoEllipsis(label, primary)
		remaining := width - TextWidth(line)
		if width > 40 && item.Description != "" && remaining > 10 {
			line += ANSITruncate(item.Description, remaining, true)
		}
		line = padRightNoEllipsis(line, width)
		if i == l.selected && l.theme.StyleSelected != nil {
			line = l.theme.StyleSelected(line)
		}
		out = append(out, line)
	}
	if start > 0 || end < len(items) {
		out = append(out, padRightNoEllipsis("  ("+strconv.Itoa(l.selected+1)+"/"+strconv.Itoa(len(items))+")", width))
	}
	return out
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
	switch ke.Key {
	case KeyUp:
		if len(l.filtered) > 0 {
			l.selected = (l.selected + len(l.filtered) - 1) % len(l.filtered)
		}
		return true
	case KeyDown:
		if len(l.filtered) > 0 {
			l.selected = (l.selected + 1) % len(l.filtered)
		}
		return true
	case KeyEnter:
		if len(l.filtered) > 0 && l.OnConfirm != nil {
			l.OnConfirm(l.filtered[l.selected])
		}
		return true
	case KeyEsc, KeyCtrlC:
		if l.OnCancel != nil {
			l.OnCancel()
		}
		return true
	}
	return false
}
