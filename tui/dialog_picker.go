package tui

import (
	"fmt"
	"strings"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/style"
	"github.com/losinggeneration/rovel/text"
	"github.com/losinggeneration/rovel/ui"
)

// DialogPickerItem is one entry in a DialogPicker's item list. Name
// is the primary text shown left-aligned; Description is a short
// summary shown to its right. Either field may be empty; the
// layout adapts to whichever side has content.
type DialogPickerItem struct {
	Name        string
	Description string
	Value       string
}

// DialogPickerComponent is the contract a custom component hosted by
// DialogPicker must implement. The picker keeps its header/cancel
// behavior but delegates row rendering and input forwarding to the
// component. The settings overlay uses this to host a settingslist
// inside the existing dialog picker plumbing.
type DialogPickerComponent interface {
	Render(width int) []string
	HandleKey(event.KeyEvent)
}

// DialogPicker is a focusable picker widget that replaces the editor
// region when active. Unlike the inline Selector (which is a
// non-capturing overlay that filters from the editor's text), the
// DialogPicker has its own single-line search input at the top of
// the widget, takes focus when shown, and processes printable
// runes / backspace / arrow keys / Enter / Esc directly.
//
// The visual layout is:
//
//	<title>           <-- header row, e.g. "/model" or "Pick a session"
//	> <filter>█       <-- search input row (cursor at end)
//	───────...        <-- separator
//	→ first item      Description
//	  second item     Description
//	  ...
//	(1/80)            <-- (focused+1)/total counter
//
// The picker delegates item-list management to the App; the App
// updates items via SetItems whenever the filter changes. This
// matches the inline Selector's contract so the App's
// `modelSelectorItems(filter)` helper can be reused.
//
// When a custom component is installed via SetComponent, the picker
// delegates rendering and input forwarding to the component instead
// of rendering its default item list. The header/cancel/filter
// machinery stays in place so the user gets the same Esc handling
// regardless of the hosted component.
type DialogPicker struct {
	id       rovel.ID
	rect     geom.Rect
	header   string
	items    []DialogPickerItem
	visible  int
	focused  int
	maxNameW int
	maxDescW int

	// filter is the current search-input text. Mutated by Handle
	// (printable runes, backspace). The App reads Filter() and
	// recomputes items; the picker doesn't filter itself.
	filter string

	// shown toggles whether the picker participates in the layout.
	// When false the picker reports a zero MinSize and refuses
	// focus. The other state (header, items, filter, focused) is
	// retained so re-showing doesn't lose the user's last filter
	// or focus position.
	shown bool

	// component is an optional hosted component. When non-nil the
	// picker renders its rows and forwards input to the component
	// instead of using its default item list. The component is set
	// via SetComponent and toggled back to nil by passing nil to
	// the same method.
	component DialogPickerComponent

	// onSelect fires when the user confirms the focused item
	// (Enter). It receives the focused index and item; the App
	// uses this to apply the selection (e.g. switch the model).
	onSelect func(idx int, item DialogPickerItem)

	// onChange fires after any text-mutating input (printable
	// rune, backspace). The App uses this to recompute the items
	// list based on the new filter. Fires on the toolkit loop so
	// the App can update items synchronously without an extra
	// dispatch.
	onChange func(filter string)

	// onCancel fires when the user dismisses the picker (Esc).
	// The App uses this to restore the editor region (which was
	// cleared when the picker opened) and move focus back to the
	// editor.
	onCancel            func()
	onVisibilityChanged func(bool)
}

// DefaultDialogPickerVisible is the number of items the dialog
// picker shows when no override is set.
const DefaultDialogPickerVisible = 8

// NewDialogPicker returns an empty dialog picker. The picker
// starts hidden; the App calls SetShown(true) when activating.
func NewDialogPicker() *DialogPicker {
	return &DialogPicker{
		id:      rovel.NewID(),
		visible: DefaultDialogPickerVisible,
	}
}

// SetHeader updates the title row text. e.g. "/model".
func (d *DialogPicker) SetHeader(header string) { d.header = header }

// Header returns the current header text.
func (d *DialogPicker) Header() string { return d.header }

// SetItems replaces the visible item list. The focused index is
// clamped into range so a shorter list doesn't leave the picker
// pointing past the end.
func (d *DialogPicker) SetItems(items []DialogPickerItem) {
	d.items = items
	d.recomputeItemWidths()
	if d.focused >= len(items) {
		d.focused = len(items) - 1
	}
	if d.focused < 0 {
		d.focused = 0
	}
}

// Items returns a copy of the current item list.
func (d *DialogPicker) Items() []DialogPickerItem {
	out := make([]DialogPickerItem, len(d.items))
	copy(out, d.items)
	return out
}

// SetVisible overrides the number of items shown. Values <= 0 fall
// back to DefaultDialogPickerVisible.
func (d *DialogPicker) SetVisible(n int) {
	if n <= 0 {
		n = DefaultDialogPickerVisible
	}
	d.visible = n
}

// Visible returns the current visible count.
func (d *DialogPicker) Visible() int {
	if d.visible <= 0 {
		return DefaultDialogPickerVisible
	}
	return d.visible
}

// SetShown toggles whether the picker takes layout space. When
// shown is true the picker is focusable, has its own search input,
// and routes key events to itself. When false the picker reports
// MinSize 0x0 and the layout treats it as absent.
func (d *DialogPicker) SetShown(v bool) {
	if d.shown == v {
		return
	}
	d.shown = v
	if d.onVisibilityChanged != nil {
		d.onVisibilityChanged(v)
	}
}

// SetOnVisibilityChanged observes dialog opening and closing on the app loop.
func (d *DialogPicker) SetOnVisibilityChanged(fn func(bool)) { d.onVisibilityChanged = fn }

// Shown reports whether the picker currently takes layout space.
func (d *DialogPicker) Shown() bool { return d.shown }

// SetFilter updates the search-input text directly. Used when the
// App opens the picker with a pre-populated filter (e.g. when
// re-opening after a cancellation). The onChange callback does
// not fire from this path — the caller is expected to call
// SetItems separately if needed.
func (d *DialogPicker) SetFilter(text string) { d.filter = text }

// Filter returns the current search-input text. The App reads this
// in onChange to compute the next item list.
func (d *DialogPicker) Filter() string { return d.filter }

// SetFocused moves the focus to idx, clamping into range.
func (d *DialogPicker) SetFocused(idx int) {
	if len(d.items) == 0 {
		d.focused = 0
		return
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(d.items) {
		d.focused = len(d.items) - 1
		return
	}
	d.focused = idx
}

// Focused returns the current focused index.
func (d *DialogPicker) Focused() int { return d.focused }

// FocusedItem returns the currently focused item, or zero+false
// when the list is empty.
func (d *DialogPicker) FocusedItem() (DialogPickerItem, bool) {
	if d.focused < 0 || d.focused >= len(d.items) {
		return DialogPickerItem{}, false
	}
	return d.items[d.focused], true
}

// SetOnSelect registers the callback fired when the user confirms
// the focused item (Enter).
func (d *DialogPicker) SetOnSelect(fn func(idx int, item DialogPickerItem)) {
	d.onSelect = fn
}

// SetOnChange registers the callback fired after a text mutation
// in the search input.
func (d *DialogPicker) SetOnChange(fn func(filter string)) {
	d.onChange = fn
}

// SetOnCancel registers the callback fired when the user dismisses
// the picker (Esc).
func (d *DialogPicker) SetOnCancel(fn func()) { d.onCancel = fn }

// Cancel runs the cancel handler as Esc does and reports whether one is set.
func (d *DialogPicker) Cancel() bool {
	if d.onCancel == nil {
		return false
	}
	d.onCancel()
	return true
}

// SetComponent installs a custom component to host inside the
// picker. When non-nil, the picker delegates body rendering and
// input forwarding to the component instead of using its default
// item list. The header/cancel/filter machinery stays in place so
// the user gets the same Esc handling regardless of the hosted
// component. Passing nil restores the default item-list behavior.
//
// The component is responsible for whatever input semantics it
// needs (submenu navigation, value cycling, etc.). The picker
// forwards printable runes, backspace, Enter, and Esc to the
// component via HandleInput.
func (d *DialogPicker) SetComponent(c DialogPickerComponent) { d.component = c }

// Component returns the currently installed custom component, or
// nil if the picker is using its default item list rendering.
func (d *DialogPicker) Component() DialogPickerComponent { return d.component }

// View interface implementation.

func (d *DialogPicker) ID() rovel.ID     { return d.id }
func (d *DialogPicker) Rect() rovel.Rect { return d.rect }
func (d *DialogPicker) Layout(r rovel.Rect) {
	d.rect = r
}
func (d *DialogPicker) Focusable() bool { return d.shown }

// MinSize returns the size the picker needs to render. Header
// (1) + filter input (1) + separator (1) + items + counter (1).
// When a custom component is installed, MinSize is based on the
// component's Render output (which already includes its own header
// and footer typically); the picker still advertises at least
// 3 rows of usable chrome so the component is visible.
func (d *DialogPicker) MinSize() geom.Size {
	if !d.shown {
		return geom.Size{}
	}
	if d.component != nil {
		// Render once at full width to determine the component's
		// minimum height. Width 80 is the default dialog width; use
		// a wider value so the component doesn't truncate its own
		// rendering for the size probe.
		rows := d.component.Render(80)
		h := len(rows)
		if h < 3 {
			h = 3
		}
		w := 40
		for _, r := range rows {
			if rw := text.Width(r); rw > w {
				w = rw
			}
		}
		if w > 80 {
			w = 80
		}
		return geom.Size{W: w, H: h}
	}
	w := 80
	h := d.Visible() + len(strings.Split(d.header, "\n")) + 3 // header + filter + separator + items + counter
	if h < 4 {
		h = 4
	}
	return geom.Size{W: w, H: h}
}

func (d *DialogPicker) PreferredSize() geom.Size { return d.MinSize() }

// Paint draws the picker. Layout (top → bottom):
//
//	row 0:   header (accent style)
//	row 1:   "> <filter>█" search input (focused style)
//	row 2:   horizontal rule
//	rows 3..N+2: items, with the focused row in the focus palette
//	row N+3: "(focused+1)/total" right-aligned
//
// When a custom component is installed, the picker delegates the
// body rendering to the component. The component is responsible for
// its own header/footer chrome.
func (d *DialogPicker) Paint(dw rovel.Drawer, ctx *rovel.Ctx) {
	if !d.shown {
		return
	}
	r := d.rect
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
	focusSt := ctx.Theme.Palette.Focus
	if focusSt == (style.Style{}) {
		focusSt = surfSt
	}

	// Component-hosted mode: trust the component's own rendering.
	// The component is responsible for its own header/footer.
	if d.component != nil {
		rows := d.component.Render(r.W)
		for i, row := range rows {
			rowY := r.Y + i
			if rowY >= r.Y+r.H {
				break
			}
			dw.FillRect(geom.Rect{X: r.X, Y: rowY, W: r.W, H: 1}, surfSt)
			drawCursorRow(dw, Point{X: r.X, Y: rowY}, ANSITruncate(row, r.W, false), surfSt, ctx.FocusedID == d.ID())
		}
		return
	}

	// Multiline titles keep confirmation messages above the filter and choices.
	headers := strings.Split(d.header, "\n")
	headerH := len(headers)
	for i, header := range headers {
		if i >= r.H {
			break
		}
		dw.FillRect(geom.Rect{X: r.X, Y: r.Y + i, W: r.W, H: 1}, surfSt)
		dw.DrawText(rovel.Point{X: r.X, Y: r.Y + i}, text.Truncate(header, r.W, false), accentSt)
	}
	// Row 1: search input. Render it on the normal surface instead of the focus
	// palette so the dialog matches the inline picker: selection highlight belongs
	// to rows, not to the filter prompt.
	if r.H > headerH {
		inputY := r.Y + headerH
		dw.FillRect(geom.Rect{X: r.X, Y: inputY, W: r.W, H: 1}, surfSt)
		inputText := "> " + d.filter
		// The last cell of the input row is reserved for the
		// cursor block (rendered by the toolkit if there's room).
		// We leave one cell of trailing space by truncating with
		// max=r.W-1 when the input fills the row.
		avail := r.W - 1
		if avail < 4 {
			avail = r.W
		}
		dw.DrawText(rovel.Point{X: r.X, Y: inputY}, text.Truncate(inputText, avail, false), surfSt)
		cursorX := r.X + text.Width(inputText)
		if cursorX >= r.X+r.W {
			cursorX = r.X + r.W - 1
		}
		if cursor, ok := dw.(rovel.CursorDrawer); ok && ctx.FocusedID == d.ID() {
			cursor.SetCursor(Point{X: cursorX, Y: inputY})
		}
	}

	// Row 2: separator.
	if r.H > headerH+1 {
		sepY := r.Y + headerH + 1
		dw.FillRect(geom.Rect{X: r.X, Y: sepY, W: r.W, H: 1}, surfSt)
		glyph := string(ctx.Theme.Chrome.Border.Glyphs.H)
		if glyph == "" {
			glyph = "─"
		}
		rule := strings.Repeat(glyph, r.W)
		dw.DrawText(rovel.Point{X: r.X, Y: sepY}, text.Truncate(rule, r.W, false), surfSt)
	}

	// Rows 3..N+2: items. Viewport follows focus for long lists.
	start := 0
	if len(d.items) > d.Visible() {
		half := d.Visible() / 2
		start = d.focused - half
		if start < 0 {
			start = 0
		}
		if start+d.Visible() > len(d.items) {
			start = len(d.items) - d.Visible()
		}
	}
	for i := 0; i < d.Visible(); i++ {
		rowY := r.Y + headerH + 2 + i
		if rowY >= r.Y+r.H {
			break
		}
		idx := start + i
		if idx < 0 || idx >= len(d.items) {
			dw.FillRect(geom.Rect{X: r.X, Y: rowY, W: r.W, H: 1}, surfSt)
			continue
		}
		item := d.items[idx]
		var rowSt style.Style
		if idx == d.focused {
			rowSt = focusSt
		} else {
			rowSt = surfSt
		}
		dw.FillRect(geom.Rect{X: r.X, Y: rowY, W: r.W, H: 1}, rowSt)
		marker := "  "
		if idx == d.focused {
			marker = "→ "
		}
		dw.DrawText(rovel.Point{X: r.X, Y: rowY}, marker, rowSt)
		meta := item.Description
		metaW := text.Width(meta)
		metaGap := 1
		if meta == "" {
			metaGap = 0
		}
		nameAvail := r.W - 2 - metaGap - metaW
		if nameAvail < 0 {
			nameAvail = 0
		}
		dw.DrawText(rovel.Point{X: r.X + 2, Y: rowY}, text.Truncate(item.Name, nameAvail, true), rowSt)
		if meta != "" && r.W > 2 {
			metaX := r.X + r.W - metaW
			if metaX < r.X+2 {
				metaX = r.X + 2
			}
			descStyle := mutedSt
			if idx == d.focused {
				descStyle = rowSt
			}
			dw.DrawText(rovel.Point{X: metaX, Y: rowY}, text.Truncate(meta, r.X+r.W-metaX, false), descStyle)
		}
	}

	// Counter row at the bottom: "(focused+1)/total".
	totalRows := headerH + 2 + d.Visible()
	if r.H > totalRows {
		counterY := r.Y + totalRows
		dw.FillRect(geom.Rect{X: r.X, Y: counterY, W: r.W, H: 1}, surfSt)
		counter := fmt.Sprintf("(%d/%d)", d.focused+1, len(d.items))
		dw.DrawText(rovel.Point{X: r.X + r.W - text.Width(counter), Y: counterY}, counter, mutedSt)
	}
}

// Handle processes raw key events. Printable runes append to the
// filter; backspace removes a rune; Esc invokes onCancel. The
// rest falls through to the toolkit / HandleAction.
//
// When a custom component is installed, the picker forwards all
// key events to the component via HandleInput and lets the
// component decide whether to consume them. Esc still fires
// onCancel so the user always has a way to dismiss the dialog
// regardless of the hosted component's input semantics.
func (d *DialogPicker) Handle(e rovel.Event, ctx *rovel.Ctx) bool {
	if !d.shown {
		return false
	}
	var paste string
	switch value := e.(type) {
	case rovel.PasteEvent:
		paste = value.Text
	case rovel.ClipboardResponseEvent:
		paste = value.Text
	}
	if paste != "" && d.component != nil {
		if input, ok := d.component.(interface{ Paste(string) }); ok {
			input.Paste(paste)
			if ctx != nil {
				ctx.Invalidate(d.rect)
			}
			return true
		}
	}
	if ke, ok := e.(rovel.KeyEvent); ok {
		// Esc always fires onCancel — the custom component is
		// responsible for not swallowing it.
		if ke.Key == rovel.KeyEsc {
			if d.onCancel != nil {
				d.onCancel()
			}
			return true
		}
		// Custom component hosting: forward input and let the
		// component decide what to do.
		if d.component != nil {
			d.component.HandleKey(ke)
			if ctx != nil {
				ctx.Invalidate(d.rect)
			}
			return true
		}
		switch ke.Key {
		case rovel.KeyEsc:
			if d.onCancel != nil {
				d.onCancel()
			}
			return true
		case rovel.KeyBackspace:
			if len(d.filter) > 0 {
				// Drop the last UTF-8 rune, not the last byte.
				runes := []rune(d.filter)
				d.filter = string(runes[:len(runes)-1])
				if d.onChange != nil {
					d.onChange(d.filter)
				}
				if ctx != nil {
					ctx.Invalidate(d.rect)
				}
			}
			return true
		case rovel.KeyRune:
			if ke.Rune != 0 {
				d.filter += string(ke.Rune)
				if d.onChange != nil {
					d.onChange(d.filter)
				}
				if ctx != nil {
					ctx.Invalidate(d.rect)
				}
			}
			return true
		case rovel.KeyPageUp:
			d.moveFocusedBy(-d.Visible(), ctx)
			return true
		case rovel.KeyPageDown:
			d.moveFocusedBy(d.Visible(), ctx)
			return true
		}
	}
	return false
}

// HandleAction handles navigation/activation semantic actions.
// The picker claims MoveUp / MoveDown / Activate (Enter) when
// shown. Tab is application-defined (focus next) and not claimed
// here — the App's keymap maps Tab to "apply focused item" when
// the picker is visible, by routing through HandleAction on the
// App's action-handler path.
//
// DeleteBackward is also claimed here so the toolkit's default
// keymap (which resolves Backspace to ActionDeleteBackward in
// the global context) doesn't swallow the keystroke before our
// Handle() gets a chance to mutate the filter. Without this,
// Backspace in the picker's search input would silently no-op.
//
// When a custom component is installed, the navigation and
// activation actions are forwarded to the component as raw key
// sequences. The component controls its own internal state.
func (d *DialogPicker) HandleAction(act int, ctx *rovel.Ctx) bool {
	if !d.shown {
		return false
	}
	if d.component != nil {
		switch ui.Action(act) {
		case ui.ActionMoveUp:
			d.component.HandleKey(event.KeyEvent{Key: event.KeyUp})
		case ui.ActionMoveDown:
			d.component.HandleKey(event.KeyEvent{Key: event.KeyDown})
		case ui.ActionPageUp:
			d.component.HandleKey(event.KeyEvent{Key: event.KeyPageUp})
		case ui.ActionPageDown:
			d.component.HandleKey(event.KeyEvent{Key: event.KeyPageDown})
		case ui.ActionActivate:
			d.component.HandleKey(event.KeyEvent{Key: event.KeyEnter})
		case ui.ActionDeleteBackward:
			d.component.HandleKey(event.KeyEvent{Key: event.KeyBackspace})
		default:
			return false
		}
		if ctx != nil {
			ctx.Invalidate(d.rect)
		}
		return true
	}
	switch ui.Action(act) {
	case ui.ActionMoveUp:
		d.moveFocusedBy(-1, ctx)
		return true
	case ui.ActionMoveDown:
		d.moveFocusedBy(1, ctx)
		return true
	case ui.ActionPageUp:
		d.moveFocusedBy(-d.Visible(), ctx)
		return true
	case ui.ActionPageDown:
		d.moveFocusedBy(d.Visible(), ctx)
		return true
	case ui.ActionActivate:
		if it, ok := d.FocusedItem(); ok && d.onSelect != nil {
			d.onSelect(d.focused, it)
		}
		return true
	case ui.ActionDeleteBackward:
		if len(d.filter) > 0 {
			runes := []rune(d.filter)
			d.filter = string(runes[:len(runes)-1])
			if d.onChange != nil {
				d.onChange(d.filter)
			}
			if ctx != nil {
				ctx.Invalidate(d.rect)
			}
		}
		return true
	}
	return false
}

func (d *DialogPicker) moveFocusedBy(delta int, ctx *rovel.Ctx) {
	if len(d.items) == 0 || delta == 0 {
		return
	}
	oldFocused := d.focused
	d.SetFocused(d.focused + delta)
	if d.focused != oldFocused && ctx != nil {
		ctx.Invalidate(d.rect)
	}
}

// Reset clears the filter and items but keeps the picker state
// otherwise intact. Used when opening a fresh dialog.
func (d *DialogPicker) Reset() {
	d.filter = ""
	d.items = nil
	d.focused = 0
	d.recomputeItemWidths()
}

func (d *DialogPicker) recomputeItemWidths() {
	d.maxNameW = 0
	d.maxDescW = 0
	for _, it := range d.items {
		if w := text.Width(it.Name); w > d.maxNameW {
			d.maxNameW = w
		}
		if w := text.Width(it.Description); w > d.maxDescW {
			d.maxDescW = w
		}
	}
}
