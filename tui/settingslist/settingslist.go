package settingslist

import (
	"strconv"
	"strings"

	"github.com/losinggeneration/tau/tui/ansi"

	rovelansi "github.com/losinggeneration/rovel/backend/ansi"
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/text"
	"github.com/losinggeneration/tau/tui/fuzzy"
)

type Item struct {
	ID           string
	Label        string
	Description  string
	CurrentValue string
	Values       []string
	Submenu      func(current string, done func(selected string, ok bool)) KeyComponent
}

type Component interface {
	Render(width int) []string
	HandleInput(data string)
}

type KeyComponent interface {
	Render(width int) []string
	HandleKey(event.KeyEvent)
}

type Theme struct {
	Label       func(string, bool) string
	Value       func(string, bool) string
	Description func(string) string
	Cursor      string
	Hint        func(string) string
}

func DefaultTheme() Theme {
	return Theme{Label: func(s string, _ bool) string { return s }, Value: func(s string, _ bool) string { return s }, Description: func(s string) string { return s }, Cursor: "> ", Hint: func(s string) string { return s }}
}

type Options struct{ EnableSearch bool }

type List struct {
	items         []Item
	filtered      []Item
	theme         Theme
	selected      int
	maxVisible    int
	onChange      func(id, newValue string)
	onCancel      func()
	searchEnabled bool
	search        string
	appliedQuery  string
	submenu       KeyComponent
	submenuIndex  int
	input         rovelansi.DecodeInput
}

func New(items []Item, maxVisible int, theme Theme, onChange func(string, string), onCancel func(), opts Options) *List {
	if maxVisible <= 0 {
		maxVisible = 1
	}
	if theme.Cursor == "" {
		theme = DefaultTheme()
	}
	cp := append([]Item(nil), items...)
	return &List{items: cp, filtered: cp, theme: theme, maxVisible: maxVisible, onChange: onChange, onCancel: onCancel, searchEnabled: opts.EnableSearch, submenuIndex: -1}
}

// FilteredItems returns a copy of the items currently displayed after search filtering.
func (l *List) FilteredItems() []Item { return append([]Item(nil), l.displayItems()...) }

func (l *List) UpdateValue(id, newValue string) {
	for i := range l.items {
		if l.items[i].ID == id {
			l.items[i].CurrentValue = newValue
		}
	}
	l.applyFilter(l.search)
}
func (l *List) SelectedIndex() int { return l.selected }
func (l *List) Search() string     { return l.search }
func (l *List) Items() []Item      { return append([]Item(nil), l.items...) }
func (l *List) Render(width int) []string {
	if l.submenu != nil {
		return l.submenu.Render(width)
	}
	return l.renderMain(width)
}

func (l *List) renderMain(width int) []string {
	var lines []string
	if l.searchEnabled {
		lines = append(lines, renderSearchInput(l.search, width))
	}
	if len(l.items) == 0 {
		lines = append(lines, l.theme.Hint("  No settings available"))
		if l.searchEnabled {
			l.addHint(&lines, width)
		}
		return lines
	}
	display := l.displayItems()
	if len(display) == 0 {
		lines = append(lines, truncate(l.theme.Hint("  No matching settings"), width))
		l.addHint(&lines, width)
		return lines
	}
	start := l.selected - l.maxVisible/2
	if start < 0 {
		start = 0
	}
	if max := len(display) - l.maxVisible; start > max && max >= 0 {
		start = max
	}
	end := start + l.maxVisible
	if end > len(display) {
		end = len(display)
	}
	maxLabel := 0
	for _, it := range l.items {
		if w := visibleWidth(it.Label); w > maxLabel {
			maxLabel = w
		}
	}
	if maxLabel > 30 {
		maxLabel = 30
	}
	for i := start; i < end; i++ {
		it := display[i]
		sel := i == l.selected
		prefix := "  "
		if sel {
			prefix = l.theme.Cursor
		}
		label := it.Label + strings.Repeat(" ", max(0, maxLabel-visibleWidth(it.Label)))
		sep := "  "
		used := visibleWidth(prefix) + maxLabel + visibleWidth(sep)
		valueMax := width - used - 2
		val := l.theme.Value(truncate(it.CurrentValue, valueMax), sel)
		lines = append(lines, truncate(prefix+l.theme.Label(label, sel)+sep+val, width))
	}
	if start > 0 || end < len(display) {
		lines = append(lines, l.theme.Hint(truncate("  ("+strconv.Itoa(l.selected+1)+"/"+strconv.Itoa(len(display))+")", width-2)))
	}
	if desc := display[l.selected].Description; desc != "" {
		lines = append(lines, "")
		for _, line := range wrap(desc, width-4) {
			lines = append(lines, l.theme.Description("  "+line))
		}
	}
	l.addHint(&lines, width)
	return lines
}

func (l *List) HandleInput(data string) {
	for _, ev := range l.input.FlushPending(l.input.Push(nil, []byte(data))) {
		if key, ok := ev.(event.KeyEvent); ok {
			l.HandleKey(key)
		}
	}
}

func (l *List) HandleKey(ke event.KeyEvent) {
	if l.submenu != nil {
		l.submenu.HandleKey(ke)
		return
	}
	display := l.displayItems()
	switch ke.Key {
	case event.KeyUp:
		if len(display) > 0 {
			if l.selected == 0 {
				l.selected = len(display) - 1
			} else {
				l.selected--
			}
		}
	case event.KeyDown:
		if len(display) > 0 {
			if l.selected == len(display)-1 {
				l.selected = 0
			} else {
				l.selected++
			}
		}
	case event.KeyEnter:
		l.activate()
	case event.KeyRune:
		if ke.Mod == 0 && ke.Rune == ' ' && (!l.searchEnabled || l.search == "") {
			l.activate()
			return
		}
		if r, ok := ansi.TextRune(ke); ok && l.searchEnabled {
			l.search += string(r)
			l.applyFilter(l.search)
		}
	case event.KeyEsc, event.KeyCtrlC:
		if l.onCancel != nil {
			l.onCancel()
		}
	case event.KeyBackspace:
		if l.searchEnabled && l.search != "" {
			l.search = l.search[:text.PrevCluster(l.search, len(l.search))]
			l.applyFilter(l.search)
		}
	}
}

func (l *List) activate() {
	display := l.displayItems()
	if len(display) == 0 || l.selected >= len(display) {
		return
	}
	item := display[l.selected]
	if item.Submenu != nil {
		idx := l.selected
		l.submenuIndex = idx
		l.submenu = item.Submenu(item.CurrentValue, func(v string, ok bool) {
			if ok {
				l.setValue(item.ID, v)
			}
			l.submenu = nil
			l.selected = idx
			l.submenuIndex = -1
		})
		return
	}
	if len(item.Values) > 0 {
		idx := 0
		for i, v := range item.Values {
			if v == item.CurrentValue {
				idx = i + 1
				break
			}
		}
		if idx >= len(item.Values) {
			idx = 0
		}
		l.setValue(item.ID, item.Values[idx])
	}
}

func (l *List) setValue(id, v string) {
	for i := range l.items {
		if l.items[i].ID == id {
			l.items[i].CurrentValue = v
		}
	}
	l.applyFilter(l.search)
	if l.onChange != nil {
		l.onChange(id, v)
	}
}

func (l *List) applyFilter(q string) {
	if !l.searchEnabled || strings.TrimSpace(q) == "" {
		l.filtered = append([]Item(nil), l.items...)
	} else {
		l.filtered = fuzzy.Filter(l.items, q, func(i Item) string { return i.Label })
	}
	// A changed query changes the item set, so reset the cursor. A re-apply
	// with the same query (a value change) must keep the cursor where it was,
	// otherwise changing the second setting jumps to the top and the next
	// keypress edits an unrelated row.
	if q != l.appliedQuery {
		l.selected = 0
	}
	l.appliedQuery = q
	if l.selected >= len(l.filtered) {
		l.selected = max(0, len(l.filtered)-1)
	}
}

func (l *List) displayItems() []Item {
	if l.searchEnabled {
		return l.filtered
	}
	return l.items
}

func (l *List) addHint(lines *[]string, width int) {
	*lines = append(*lines, "", truncate(l.theme.Hint(map[bool]string{true: "  Type to search · Enter/Space to change · Esc to cancel", false: "  Enter/Space to change · Esc to cancel"}[l.searchEnabled]), width))
}

// renderSearchInput renders the search prompt row. It must not embed
// ANSI escape sequences: the list renders through cell-based drawing
// where a zero-width ESC rune is overwritten by the next cell, leaving
// literal "[7m" text on screen.
func renderSearchInput(value string, width int) string {
	line := "> " + value
	return truncate(line+strings.Repeat(" ", max(0, width-visibleWidth(line))), width)
}

func visibleWidth(s string) int { return text.Width(s) }
func truncate(s string, w int) string {
	return text.Truncate(s, w, false)
}

func wrap(s string, w int) []string {
	if w <= 0 {
		return []string{""}
	}
	lines := text.WrapLines(s, text.WrapOptions{Width: w, Mode: text.WrapWord})
	if len(lines) == 0 {
		return []string{""}
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, s[line.StartByte:line.EndByte])
	}
	return out
}
