package tui

import (
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/style"
)

// Text is a static text display component. It renders a single line of text
// padded to the available width. Multi-line text is supported; newlines in
// the text produce multiple rows.
//
// Text follows the spec's Text component semantics:
//   - empty/whitespace text renders no rows
//   - tabs render as three spaces
//   - output lines are padded to full width
//   - background function (SetBackground) applies across full width
//
// Text widget.
type Text struct {
	id       ID
	text     string
	rect     Rect
	style    style.Style
	bgStyle  *style.Style
	visible  bool
	paddingX int // horizontal padding in cells
	vertical int // vertical padding in rows

	cacheText     string
	cacheWidth    int
	cachePaddingX int
	cacheVertical int
	cacheVisible  bool
	cacheRows     []string
}

// NewText returns a Text component with the given content.
func NewText(s string) *Text {
	return &Text{
		id:      NewID(),
		text:    s,
		style:   style.Style{}, // default (terminal default FG/BG)
		visible: true,
	}
}

// SetText changes the text content. Per spec, an empty/whitespace text renders
// no rows.
func (t *Text) SetText(s string) {
	t.text = s
	t.clearCache()
}

// SetStyle sets the foreground/background style.
func (t *Text) SetStyle(s style.Style) {
	t.style = s
}

// SetBackground sets a background style applied across the full row width.
func (t *Text) SetBackground(s *style.Style) {
	t.bgStyle = s
}

// SetPadding sets horizontal cell padding and vertical blank-row padding.
func (t *Text) SetPadding(horizontal, vertical int) {
	if horizontal < 0 {
		horizontal = 0
	}
	if vertical < 0 {
		vertical = 0
	}
	t.paddingX = horizontal
	t.vertical = vertical
	t.clearCache()
}

// SetHorizontalPadding sets horizontal cell padding.
func (t *Text) SetHorizontalPadding(n int) {
	if n < 0 {
		n = 0
	}
	t.paddingX = n
	t.clearCache()
}

// SetVerticalPadding adds blank full-width rows above and below the text.
func (t *Text) SetVerticalPadding(n int) {
	if n < 0 {
		n = 0
	}

	t.vertical = n
	t.clearCache()
}

// SetVisible toggles rendering. Hidden text is not painted but still
// participates in layout.
func (t *Text) SetVisible(v bool) {
	t.visible = v
	t.clearCache()
}

// Visible reports the current visibility flag.
func (t *Text) Visible() bool { return t.visible }

// Invalidate marks the text's rect for repaint.
func (t *Text) Invalidate(ctx *Ctx) {
	if ctx == nil || ctx.Invalidate == nil {
		return
	}

	if t.rect.W <= 0 || t.rect.H <= 0 {
		return
	}

	ctx.Invalidate(t.rect)
}

// ID implements View.
func (t *Text) ID() ID { return t.id }

// MinSize implements View. Empty text reports 1x1; non-empty reports the
// width of the rendered text and the number of lines.
func (t *Text) MinSize() Size {
	lines := t.lines()
	w := 0
	for _, line := range lines {
		if lw := TextWidth(line); lw > w {
			w = lw
		}
	}

	w += 2 * t.paddingX
	if w < 1 {
		w = 1
	}

	h := len(lines) + 2*t.vertical
	if h < 1 {
		h = 1
	}

	return Size{W: w, H: h}
}

// Rect returns the last layout rect.
func (t *Text) Rect() Rect { return t.rect }

// Layout stores the rect. The text itself does not need to position
// individual lines; Paint handles that based on t.rect.
func (t *Text) Layout(r Rect) { t.rect = r }

// Paint renders the text. Per spec, empty/whitespace text renders no rows.
// Vertical padding rows are blank full-width rows per spec.
func (t *Text) Paint(d Drawer, _ *Ctx) {
	if !t.visible {
		return
	}

	lines := t.Render(t.rect.W)
	if len(lines) == 0 {
		return
	}

	// If a background style is set, fill the whole rect with it before drawing.
	if t.bgStyle != nil {
		d.FillRect(t.rect, *t.bgStyle)
	}

	for i, line := range lines {
		y := t.rect.Y + i
		if y < t.rect.Y || y >= t.rect.Y+t.rect.H {
			continue
		}

		d.DrawText(geom.Point{X: t.rect.X, Y: y}, line, t.style)
	}
}

// Handle does nothing. Text does not consume input.
func (t *Text) Handle(_ Event, _ *Ctx) bool { return false }

// Focusable returns false. Text is a static display.
func (t *Text) Focusable() bool { return false }

// Render renders the text to padded, wrapped rows.
func (t *Text) Render(width int) []string {
	if t == nil || width <= 0 || !t.visible {
		return nil
	}
	if t.cacheRows != nil && t.cacheText == t.text && t.cacheWidth == width && t.cachePaddingX == t.paddingX && t.cacheVertical == t.vertical && t.cacheVisible == t.visible {
		return append([]string(nil), t.cacheRows...)
	}

	rows := normalizeRows(t.text)
	if len(rows) == 0 {
		t.clearCache()
		return nil
	}

	contentW := width - 2*t.paddingX
	if contentW < 1 {
		contentW = 1
	}
	prefix := repeatSpace(t.paddingX)

	out := []string{}
	for range t.vertical {
		out = append(out, repeatSpace(width))
	}
	for _, row := range rows {
		for _, wrapped := range wrapRow(row, contentW) {
			out = append(out, padRightNoEllipsis(prefix+wrapped+prefix, width))
		}
	}
	for range t.vertical {
		out = append(out, repeatSpace(width))
	}

	t.cacheText = t.text
	t.cacheWidth = width
	t.cachePaddingX = t.paddingX
	t.cacheVertical = t.vertical
	t.cacheVisible = t.visible
	t.cacheRows = append([]string(nil), out...)

	return append([]string(nil), out...)
}

// lines returns the text split into lines with tabs expanded to three spaces.
func (t *Text) lines() []string {
	return normalizeRows(t.text)
}

func (t *Text) clearCache() {
	if t == nil {
		return
	}
	t.cacheRows = nil
	t.cacheWidth = 0
	t.cacheText = ""
	t.cachePaddingX = 0
	t.cacheVertical = 0
	t.cacheVisible = false
}

// expandTabs returns s with each tab character replaced by n spaces.
func expandTabs(s string, n int) string {
	if n <= 0 {
		return s
	}

	pad := ""
	for range n {
		pad += " "
	}

	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\t' {
			for _, p := range pad {
				out = append(out, p)
			}
		} else {
			out = append(out, r)
		}
	}

	return string(out)
}

// padRight returns s padded with spaces to exactly w display cells. If s is
// already at least w cells wide, it is truncated to w.
func padRight(s string, w int) string {
	if w <= 0 {
		return ""
	}

	width := TextWidth(s)
	if width > w {
		// Text is too wide; truncate. Use ellipsis=true so the user sees
		// that content was cut off.
		return Truncate(s, w, true)
	}

	if width == w {
		// Text already fills the row exactly.
		return s
	}

	// Pad to fill the row.
	return s + repeatSpace(w-width)
}

// repeatSpace returns n spaces.
func repeatSpace(n int) string {
	if n <= 0 {
		return ""
	}

	out := make([]byte, n)
	for i := range out {
		out[i] = ' '
	}

	return string(out)
}
