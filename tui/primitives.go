package tui

import (
	"strings"

	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/style"
)

// RowRenderer renders logical terminal rows for a given width.
type RowRenderer interface {
	Render(width int) []string
}

// RenderRows renders v to logical rows when it exposes RowRenderer.
func RenderRows(v View, width int) []string {
	if width <= 0 || v == nil {
		return nil
	}

	if rr, ok := v.(RowRenderer); ok {
		return rr.Render(width)
	}

	return nil
}

// RowDiagnostic reports a rendered component row whose visible width exceeds
// the layout width it was rendered for. This is a component-level diagnostic;
// rovel owns the terminal renderer and clipping/damage behavior.
type RowDiagnostic struct {
	Index int
	Width int
	Limit int
	Row   string
}

// ValidateRows returns diagnostics for rows wider than width visible cells.
func ValidateRows(rows []string, width int) []RowDiagnostic {
	if width <= 0 {
		return nil
	}

	var diagnostics []RowDiagnostic
	for i, row := range rows {
		rowWidth := TextWidth(row)
		if rowWidth > width {
			diagnostics = append(diagnostics, RowDiagnostic{
				Index: i,
				Width: rowWidth,
				Limit: width,
				Row:   row,
			})
		}
	}

	return diagnostics
}

// ValidateRenderRows renders v and returns diagnostics for overwide component
// rows. It is intended for tau component tests and debug checks, not as a
// renderer/diff invariant; rovel owns renderer behavior.
func ValidateRenderRows(v View, width int) []RowDiagnostic {
	return ValidateRows(RenderRows(v, width), width)
}

func normalizeRows(s string) []string {
	if s == "" || isWhitespaceOnly(s) {
		return nil
	}

	expanded := expandTabs(s, 3)
	expanded = strings.ReplaceAll(expanded, "\r\n", "\n")
	expanded = strings.ReplaceAll(expanded, "\r", "\n")

	return strings.Split(expanded, "\n")
}

func wrapRow(row string, width int) []string {
	if width <= 0 {
		return nil
	}
	if row == "" {
		return []string{""}
	}

	wrapped := ANSIWrap(row, width)
	if len(wrapped) == 0 {
		return []string{row}
	}

	return wrapped
}

func padRows(rows []string, width int) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, padRight(row, width))
	}

	return out
}

func isWhitespaceOnly(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}

	return true
}

// Spacer renders blank rows without padding them to terminal width.
type Spacer struct {
	id   ID
	rows int
	rect Rect
}

func NewSpacer(rows int) *Spacer {
	if rows < 0 {
		rows = 0
	}

	return &Spacer{id: NewID(), rows: rows}
}

func (s *Spacer) SetRows(rows int) {
	if rows < 0 {
		rows = 0
	}
	s.rows = rows
}

func (s *Spacer) Render(_ int) []string {
	out := make([]string, s.rows)
	for i := range out {
		out[i] = ""
	}

	return out
}

func (s *Spacer) ID() ID                      { return s.id }
func (s *Spacer) Rect() Rect                  { return s.rect }
func (s *Spacer) MinSize() Size               { return Size{W: 0, H: s.rows} }
func (s *Spacer) Layout(r Rect)               { s.rect = r }
func (s *Spacer) Handle(_ Event, _ *Ctx) bool { return false }
func (s *Spacer) Focusable() bool             { return false }
func (s *Spacer) Paint(d Drawer, _ *Ctx) {
	for i, row := range s.Render(s.rect.W) {
		y := s.rect.Y + i
		if y >= s.rect.Y+s.rect.H {
			break
		}
		d.DrawText(geom.Point{X: s.rect.X, Y: y}, row, style.Style{})
	}
}

// TruncatedText renders text rows truncated rather than wrapped.
type TruncatedText struct {
	*Text
	paddingX int
}

func NewTruncatedText(s string) *TruncatedText {
	return &TruncatedText{Text: NewText(s)}
}

func NewPaddedTruncatedText(s string, paddingX, paddingY int) *TruncatedText {
	t := NewTruncatedText(s)
	t.SetPadding(paddingX, paddingY)

	return t
}

func (t *TruncatedText) SetPadding(horizontal, vertical int) {
	if horizontal < 0 {
		horizontal = 0
	}
	t.paddingX = horizontal
	t.SetVerticalPadding(vertical)
}

func (t *TruncatedText) Render(width int) []string {
	if t == nil || t.Text == nil || width <= 0 || !t.visible {
		return nil
	}

	rows := normalizeRows(t.text)
	if len(rows) == 0 {
		return nil
	}

	available := width - 2*t.paddingX
	if available < 1 {
		available = 1
	}

	out := make([]string, 0, 1+2*t.vertical)
	for range t.vertical {
		out = append(out, repeatSpace(width))
	}
	row := rows[0]
	line := repeatSpace(t.paddingX) + ANSITruncate(row, available, true) + repeatSpace(t.paddingX)
	out = append(out, padRightNoEllipsis(line, width))
	for range t.vertical {
		out = append(out, repeatSpace(width))
	}

	return out
}

// Box renders child rows inside horizontal and vertical padding.
type Box struct {
	id       ID
	children []RowRenderer
	rect     Rect
	hPad     int
	vPad     int
	bgStyle  *style.Style

	cacheWidth int
	cacheKey   string
	cacheRows  []string
}

func NewBox(children ...RowRenderer) *Box {
	return &Box{id: NewID(), children: children}
}

func (b *Box) AddChild(child RowRenderer) {
	if child != nil {
		b.children = append(b.children, child)
		b.Invalidate()
	}
}

func (b *Box) Clear() {
	b.children = nil
	b.Invalidate()
}

func (b *Box) SetPadding(horizontal, vertical int) {
	if horizontal < 0 {
		horizontal = 0
	}
	if vertical < 0 {
		vertical = 0
	}
	b.hPad = horizontal
	b.vPad = vertical
	b.Invalidate()
}

func (b *Box) SetBackground(s *style.Style) {
	b.bgStyle = s
	b.Invalidate()
}

func (b *Box) Render(width int) []string {
	if b == nil || width <= 0 || len(b.children) == 0 {
		return nil
	}

	contentW := width - 2*b.hPad
	if contentW < 1 {
		contentW = 1
	}

	content := []string{}
	for _, child := range b.children {
		content = append(content, child.Render(contentW)...)
	}
	if len(content) == 0 {
		b.Invalidate()
		return nil
	}

	key := b.renderCacheKey(width, content)
	if b.cacheRows != nil && b.cacheWidth == width && b.cacheKey == key {
		return append([]string(nil), b.cacheRows...)
	}

	out := make([]string, 0, len(content)+2*b.vPad)
	blank := repeatSpace(width)
	for range b.vPad {
		out = append(out, blank)
	}
	prefix := repeatSpace(b.hPad)
	for _, row := range content {
		out = append(out, padRightNoEllipsis(prefix+row, width))
	}
	for range b.vPad {
		out = append(out, blank)
	}

	b.cacheWidth = width
	b.cacheKey = key
	b.cacheRows = append([]string(nil), out...)

	return append([]string(nil), out...)
}

func (b *Box) ID() ID          { return b.id }
func (b *Box) Rect() Rect      { return b.rect }
func (b *Box) Layout(r Rect)   { b.rect = r }
func (b *Box) Focusable() bool { return false }
func (b *Box) MinSize() Size {
	rows := b.Render(80)
	return Size{W: 1, H: len(rows)}
}
func (b *Box) Handle(_ Event, _ *Ctx) bool { return false }

// Invalidate clears the cached rendered rows and propagates invalidation to
// children that expose an Invalidate method.
func (b *Box) Invalidate() {
	if b == nil {
		return
	}
	b.cacheWidth = 0
	b.cacheKey = ""
	b.cacheRows = nil
	for _, child := range b.children {
		if invalidator, ok := child.(interface{ Invalidate() }); ok {
			invalidator.Invalidate()
		}
	}
}

func (b *Box) renderCacheKey(width int, content []string) string {
	var sb strings.Builder
	writeInt := func(n int) {
		if n == 0 {
			sb.WriteByte('0')
			return
		}
		if n < 0 {
			sb.WriteByte('-')
			n = -n
		}
		var digits [20]byte
		i := len(digits)
		for n > 0 {
			i--
			digits[i] = byte('0' + n%10)
			n /= 10
		}
		sb.Write(digits[i:])
	}
	writeInt(width)
	sb.WriteByte('|')
	writeInt(b.hPad)
	sb.WriteByte('|')
	writeInt(b.vPad)
	for _, row := range content {
		sb.WriteByte('\x00')
		sb.WriteString(row)
	}
	return sb.String()
}

func (b *Box) Paint(d Drawer, _ *Ctx) {
	rows := b.Render(b.rect.W)
	for i, row := range rows {
		y := b.rect.Y + i
		if y >= b.rect.Y+b.rect.H {
			break
		}
		if b.bgStyle != nil {
			d.FillRect(geom.Rect{X: b.rect.X, Y: y, W: b.rect.W, H: 1}, *b.bgStyle)
		}
		d.DrawText(geom.Point{X: b.rect.X, Y: y}, row, style.Style{})
	}
}

// VisualTruncate returns the last maxLines lines and the count of skipped lines.
func VisualTruncate(lines []string, maxLines int) ([]string, int) {
	if maxLines <= 0 {
		return nil, len(lines)
	}
	if len(lines) <= maxLines {
		out := make([]string, len(lines))
		copy(out, lines)

		return out, 0
	}

	skipped := len(lines) - maxLines
	out := make([]string, maxLines)
	copy(out, lines[skipped:])

	return out, skipped
}

func padRightNoEllipsis(s string, w int) string {
	if w <= 0 {
		return ""
	}
	width := TextWidth(s)
	if width > w {
		return ANSITruncate(s, w, false)
	}
	return s + repeatSpace(w-width)
}
