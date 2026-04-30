package tui

import (
	"fmt"
	"strings"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/style"
	"github.com/losinggeneration/rovel/text"
	"github.com/losinggeneration/rovel/ui"
)

// SelectorItem is one entry in a Selector's item list. Name is the
// primary text shown left-aligned; Description is a short summary shown
// to its right. Either field may be empty; the layout adapts to whichever
// side has content.
type SelectorItem struct {
	Name        string
	Description string
	Value       string
}

// Selector is a generic inline list widget shared by tau's slash
// command completion and the model picker (and any future inline
// pickers). It renders a select-list-style window of items, optionally
// followed by an indented scroll indicator when the list is clipped.
//
// The selector is non-modal: the parent view (the prompt editor) stays
// focused. Up/Down move the focused row, Enter/Tab apply the focused
// item via onSelect, Esc dismisses via onClose. The selector does not
// own any filtering logic — the parent reacts to the editor's text
// changes and updates the items slice.
type Selector struct {
	id       rovel.ID
	rect     geom.Rect
	allItems []SelectorItem
	items    []SelectorItem
	visible  int // number of items to show; 0 or negative = default
	focused  int
	maxNameW int
	maxDescW int

	// shown toggles whether the selector takes layout space. When false
	// the selector reports a zero MinSize and refuses focus, so a parent
	// VStack treats it as if it were not present (no allocated height,
	// skipped by Tab navigation). The widget's item and focus state is
	// retained so re-showing it doesn't lose the last selection.
	shown bool

	onSelect func(idx int, item SelectorItem)
	onClose  func()
}

// DefaultSelectorVisible is the number of items the selector shows
// when no override is set. Pi uses 5; we match that.
const DefaultSelectorVisible = 5

// NewSelector returns an empty Selector with the default visible count.
func NewSelector() *Selector {
	return &Selector{
		id:      rovel.NewID(),
		visible: DefaultSelectorVisible,
	}
}

// SetItems replaces the visible item list and resets selection to the first
// item. The app calls this after autocomplete filter changes, matching the
// select-list behavior that filter changes reset selection to zero.
func (s *Selector) SetItems(items []SelectorItem) {
	s.allItems = append([]SelectorItem(nil), items...)
	s.items = append([]SelectorItem(nil), items...)
	s.focused = 0
	s.recomputeItemWidths()
}

func FilterPrefix(items []SelectorItem, query string) []SelectorItem {
	if query == "" {
		return append([]SelectorItem(nil), items...)
	}
	query = strings.ToLower(query)
	out := []SelectorItem{}
	for _, item := range items {
		if strings.HasPrefix(strings.ToLower(item.Name), query) {
			out = append(out, item)
		}
	}
	return out
}

// SetFilter applies Pi select-list prefix filtering against item values/names
// and resets selection to the first match.
func (s *Selector) SetFilter(query string) {
	s.items = FilterPrefix(s.allItems, query)
	s.focused = 0
	s.recomputeItemWidths()
}

// Items returns a copy of the current item list.
func (s *Selector) Items() []SelectorItem {
	out := make([]SelectorItem, len(s.items))
	copy(out, s.items)
	return out
}

// SetVisible overrides the number of items shown. Values <= 0 fall
// back to DefaultSelectorVisible.
func (s *Selector) SetVisible(n int) {
	if n <= 0 {
		n = DefaultSelectorVisible
	}
	s.visible = n
}

// Visible returns the current visible count.
func (s *Selector) Visible() int {
	if s.visible <= 0 {
		return DefaultSelectorVisible
	}
	return s.visible
}

// SetShown toggles whether the selector participates in the parent's
// layout. When shown is false the selector reports a zero MinSize and
// refuses focus, so it takes no vertical space and Tab navigation
// skips over it. The header, items, and focused index are retained
// so the selector can be re-shown without rebuilding state.
func (s *Selector) SetShown(v bool) {
	s.shown = v
}

// Shown reports whether the selector currently takes layout space.
func (s *Selector) Shown() bool { return s.shown }

// SetFocused moves the focus to idx, clamping into range. Safe to call
// when the item list is empty (in which case the focus stays at 0).
func (s *Selector) SetFocused(idx int) {
	if len(s.items) == 0 {
		s.focused = 0
		return
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s.items) {
		idx = len(s.items) - 1
	}
	s.focused = idx
}

// Focused returns the current focused index.
func (s *Selector) Focused() int { return s.focused }

// FocusedItem returns the currently focused SelectorItem. Returns the
// zero value and false when the list is empty.
func (s *Selector) FocusedItem() (SelectorItem, bool) {
	if s.focused < 0 || s.focused >= len(s.items) {
		return SelectorItem{}, false
	}
	return s.items[s.focused], true
}

// SetOnSelect registers the callback fired when the user confirms the
// focused item (Enter or Tab).
func (s *Selector) SetOnSelect(fn func(idx int, item SelectorItem)) {
	s.onSelect = fn
}

// SetOnClose registers the callback fired when the user dismisses the
// selector (Esc).
func (s *Selector) SetOnClose(fn func()) {
	s.onClose = fn
}

// View interface implementation.

func (s *Selector) ID() rovel.ID        { return s.id }
func (s *Selector) Rect() rovel.Rect    { return s.rect }
func (s *Selector) Layout(r rovel.Rect) { s.rect = r }
func (s *Selector) Focusable() bool     { return s.shown }

// MinSize returns the size the selector needs to render its visible items and
// clipped-list scroll indicator. Width tracks
// the longest name plus the longest description plus a marker gutter, clamped to
// a reasonable range so the selector doesn't overflow the screen or
// shrink to uselessness.
func (s *Selector) MinSize() geom.Size {
	if !s.shown {
		return geom.Size{}
	}
	// "→ " marker (2) + name + 1 gap + right-aligned description.
	w := 2 + s.maxNameW + 1 + s.maxDescW
	if w < 30 {
		w = 30
	}
	// Prefer using the full available row so session pickers can right-align
	// metadata at the terminal edge. Narrow parents still get a sane minimum.
	if s.rect.W > w {
		w = s.rect.W
	} else if w > 80 {
		w = 80
	}
	h := min(len(s.items), s.Visible())
	if s.clipped() {
		h++ // scroll indicator
	}
	return geom.Size{W: w, H: h}
}

func (s *Selector) PreferredSize() geom.Size { return s.MinSize() }

// Paint draws the selector. The visual order is:
//
//	item rows: items, with the focused row in the focus palette and the "→"
//	           marker on the row matching the active selection
//	final row: "  (focused+1)/total" aligned with the marker gutter, only when clipped
func (s *Selector) Paint(d rovel.Drawer, ctx *rovel.Ctx) {
	if !s.shown {
		return
	}
	r := s.rect
	if r.W <= 0 || r.H <= 0 {
		return
	}
	surfSt := ctx.Theme.Palette.Surface
	if surfSt == (style.Style{}) {
		surfSt = ctx.Theme.Base
	}
	accentSt := ctx.Theme.Palette.Accent
	if accentSt == (style.Style{}) {
		accentSt = surfSt
	}
	mutedSt := ctx.Theme.Palette.SurfaceMuted
	if mutedSt == (style.Style{}) {
		mutedSt = surfSt
	}

	itemsY := r.Y

	// Item rows. We render up to Visible() rows starting at
	// the current focused index (so a long list "follows" the focus
	// without the parent having to manage a viewport). If the list
	// fits within Visible(), we show everything from index 0.
	start := 0
	if len(s.items) > s.Visible() {
		half := s.Visible() / 2
		start = s.focused - half
		if start < 0 {
			start = 0
		}
		if start+s.Visible() > len(s.items) {
			start = len(s.items) - s.Visible()
		}
	}
	rowsShown := 0
	for i := 0; i < s.Visible(); i++ {
		rowY := itemsY + i
		if rowY >= r.Y+r.H {
			break
		}
		idx := start + i
		if idx < 0 || idx >= len(s.items) {
			// Empty slot in the viewport.
			d.FillRect(geom.Rect{X: r.X, Y: rowY, W: r.W, H: 1}, surfSt)
			continue
		}
		item := s.items[idx]
		var rowSt style.Style
		if idx == s.focused {
			rowSt = ctx.Theme.Palette.Focus
		} else {
			rowSt = surfSt
		}
		d.FillRect(geom.Rect{X: r.X, Y: rowY, W: r.W, H: 1}, rowSt)
		// Marker column: 2 cells. "→ " for the focused row, "  " otherwise.
		marker := "  "
		markerSt := rowSt
		if idx == s.focused {
			marker = "\u2192 "
			markerSt = accentSt
		}
		d.DrawText(rovel.Point{X: r.X, Y: rowY}, marker, markerSt)
		// Name + right-aligned description columns.
		nameX := r.X + 2
		desc := normalizeSelectorDescription(item.Description)
		descW := text.Width(desc)
		gap := 1
		if desc == "" {
			gap = 0
		}
		nameAvail := r.W - 2 - gap - descW
		if nameAvail < 0 {
			nameAvail = 0
		}
		d.DrawText(rovel.Point{X: nameX, Y: rowY}, text.Truncate(item.Name, nameAvail, true), rowSt)
		if desc != "" && r.W > 2 {
			descX := r.X + r.W - descW
			if descX < nameX {
				rowsShown++
				continue
			}
			descStyle := rowSt
			if idx != s.focused {
				descStyle = mutedSt
			}
			d.DrawText(rovel.Point{X: descX, Y: rowY}, text.Truncate(desc, r.X+r.W-descX, false), descStyle)
		}
		rowsShown++
	}

	// Final row: scroll indicator "  (focused+1)/total" in the muted style.
	// It starts at the marker column so the count text itself begins under item
	// names, matching the item-row marker gutter.
	counterY := itemsY + rowsShown
	if s.clipped() && counterY < r.Y+r.H {
		d.FillRect(geom.Rect{X: r.X, Y: counterY, W: r.W, H: 1}, surfSt)
		counter := fmt.Sprintf("  (%d/%d)", s.focused+1, len(s.items))
		d.DrawText(rovel.Point{X: r.X, Y: counterY}, text.Truncate(counter, r.W, false), mutedSt)
	}
}

func (s *Selector) recomputeItemWidths() {
	s.maxNameW = 0
	s.maxDescW = 0
	for _, it := range s.items {
		if w := text.Width(it.Name); w > s.maxNameW {
			s.maxNameW = w
		}
		if w := text.Width(it.Description); w > s.maxDescW {
			s.maxDescW = w
		}
	}
}

func (s *Selector) moveFocusedBy(delta int, ctx *rovel.Ctx) bool {
	if len(s.items) == 0 || delta == 0 {
		return true
	}
	oldFocused := s.focused
	s.SetFocused(s.focused + delta)
	if s.focused != oldFocused && ctx != nil {
		ctx.Invalidate(s.rect)
	}
	return true
}

func (s *Selector) clipped() bool {
	return len(s.items) > s.Visible()
}

func normalizeSelectorDescription(description string) string {
	return strings.Join(strings.Fields(description), " ")
}

// Handle processes raw key events. Esc closes; everything else
// falls through to HandleAction.
func (s *Selector) Handle(e rovel.Event, ctx *rovel.Ctx) bool {
	if ke, ok := e.(rovel.KeyEvent); ok {
		switch ke.Key {
		case rovel.KeyEsc, rovel.KeyCtrlC:
			if s.onClose != nil {
				s.onClose()
			}
			return true
		}
	}
	return false
}

// HandleAction handles the navigation/activation semantic actions.
func (s *Selector) HandleAction(act int, ctx *rovel.Ctx) bool {
	switch ui.Action(act) {
	case ui.ActionMoveUp:
		return s.moveFocusedBy(-1, ctx)
	case ui.ActionMoveDown:
		return s.moveFocusedBy(1, ctx)
	case ui.ActionPageUp:
		return s.moveFocusedBy(-s.Visible(), ctx)
	case ui.ActionPageDown:
		return s.moveFocusedBy(s.Visible(), ctx)
	case ui.ActionActivate:
		// Enter: invoke the select callback for the focused item.
		if it, ok := s.FocusedItem(); ok && s.onSelect != nil {
			s.onSelect(s.focused, it)
		}
		return true
	}
	// Tab is application-defined (ActionFocusNext by default) so we
	// don't claim it here. Callers that want Tab to apply the focused
	// item must map Tab to ActionActivate (or a custom action) in
	// their keymap.
	return false
}

// ActionSelect is the semantic action a caller can map Tab to in order
// to make Tab behave like Enter in the selector. Using a dedicated id
// (rather than ActionActivate) avoids conflicts with other widgets
// that also listen for Enter.
const ActionSelect = ui.Action(9001)

// HandleActionWithTab is like HandleAction but also honors ActionSelect,
// so a parent keymap can map Tab → ActionSelect to apply the focused
// item. Kept separate from HandleAction so the default action
// resolution is preserved.
func (s *Selector) HandleActionWithTab(act int, ctx *rovel.Ctx) bool {
	if ui.Action(act) == ActionSelect {
		if it, ok := s.FocusedItem(); ok && s.onSelect != nil {
			s.onSelect(s.focused, it)
		}
		return true
	}
	return s.HandleAction(act, ctx)
}
