package editor

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/style"
	"github.com/losinggeneration/rovel/text"
	"github.com/losinggeneration/rovel/ui"
)

// CursorMoveReason describes what caused an OnCursorMove notification.
type CursorMoveReason uint8

const (
	CursorMoveNavigate CursorMoveReason = iota
	CursorMoveEdit
	CursorMovePaste
	CursorMoveSetText
)

// CursorMoveEvent is delivered to OnCursorMove when the cursor byte offset changes.
type CursorMoveEvent struct {
	Cursor int
	Reason CursorMoveReason
}

// Widget is a retained-mode multiline editor view backed by the top-level
// editor model. It is intended for prompt editors that need readline-style
// editing behavior while still participating in the rovel view tree.
type Widget struct {
	id   rovel.ID
	rect rovel.Rect

	model *Editor

	readOnly bool

	onChange     func(string, *rovel.Ctx)
	onCursorMove func(CursorMoveEvent, *rovel.Ctx)

	// dragActive tracks an in-progress left-button drag so drag events know
	// which byte offset they started from. anchor is the byte offset at press
	// time; the selection grows from anchor to the cursor on each drag event.
	dragActive bool
	dragAnchor int
}

// WidgetOpts configures a Widget.
type WidgetOpts struct {
	ID           rovel.ID
	Model        *Editor
	OnChange     func(string, *rovel.Ctx)
	OnCursorMove func(CursorMoveEvent, *rovel.Ctx)
}

// NewWidget returns a new multiline editor widget.
func NewWidget() *Widget { return NewWidgetOpts(WidgetOpts{}) }

// NewWidgetOpts returns a new multiline editor widget with options.
func NewWidgetOpts(opts WidgetOpts) *Widget {
	id := opts.ID
	if id == 0 {
		id = rovel.NewID()
	}
	model := opts.Model
	if model == nil {
		model = New()
	}
	return &Widget{id: id, model: model, onChange: opts.OnChange, onCursorMove: opts.OnCursorMove}
}

func (w *Widget) Model() *Editor {
	if w.model == nil {
		w.model = New()
	}
	return w.model
}

func (w *Widget) SetOnChange(fn func(string, *rovel.Ctx)) { w.onChange = fn }
func (w *Widget) SetOnCursorMove(fn func(CursorMoveEvent, *rovel.Ctx)) {
	w.onCursorMove = fn
}

func (w *Widget) SetText(ctx *rovel.Ctx, s string) {
	old := w.Model().Cursor()
	w.Model().SetText(text.Sanitize(s))
	w.invalidate(ctx)
	w.notifyChange(ctx)
	w.notifyCursorMoveIfChanged(old, CursorMoveSetText, ctx)
}

// Paste inserts a bracketed-paste payload at the current cursor using the
// editor model's paste semantics, including newline normalization, atomic undo,
// leading-space handling, and large-paste markers. It returns the visible editor
// text and cursor after insertion.
func (w *Widget) Paste(ctx *rovel.Ctx, s string) (string, int) {
	old := w.Model().Cursor()
	w.Model().Paste(s)
	w.invalidate(ctx)
	w.notifyChange(ctx)
	w.notifyCursorMoveIfChanged(old, CursorMovePaste, ctx)
	return w.Model().Text(), w.Model().Cursor()
}

func (w *Widget) Text() string        { return w.Model().Text() }
func (w *Widget) SetReadOnly(ro bool) { w.readOnly = ro }

// VisualRowCount reports how many wrapped display rows the current text
// occupies at the given content width. Chrome that sizes itself to the editor
// (a prompt frame growing with the text) needs this before layout has run, so
// it takes the width rather than reading the widget's rect.
//
// limit caps the work: callers that only size themselves up to a maximum pass
// it so a large paste costs the cap rather than the whole buffer. The result
// saturates at limit. A limit <= 0 counts everything.
func (w *Widget) VisualRowCount(width, limit int) int {
	if w == nil {
		return 0
	}
	if width <= 0 {
		width = 1
	}
	value := w.Model().Text()
	rows := 0
	for lineStart := 0; ; {
		idx := strings.IndexByte(value[lineStart:], '\n')
		lineEnd := len(value)
		if idx >= 0 {
			lineEnd = lineStart + idx
		}
		wrapped := text.WrapLines(value[lineStart:lineEnd], text.WrapOptions{Width: width, Mode: text.WrapCluster})
		rows += max(len(wrapped), 1)
		if limit > 0 && rows >= limit {
			return limit
		}
		if idx < 0 {
			break
		}
		lineStart = lineEnd + 1
		if lineStart == len(value) {
			rows++
			break
		}
	}
	return max(rows, 1)
}

// ScrollOffset reports the first visible display row, for chrome that draws
// scroll indicators around the editor.
func (w *Widget) ScrollOffset() int {
	if w == nil {
		return 0
	}
	return w.Model().ScrollOffset()
}

// EnsureCursorVisible settles the scroll offset for a viewport of rows x width.
// Paint does this for itself, but surrounding chrome that reports on the scroll
// position has to draw before the editor does, so it needs to be able to settle
// the offset first.
func (w *Widget) EnsureCursorVisible(rows, width int) {
	if w == nil || rows <= 0 {
		return
	}
	w.Model().EnsureCursorVisible(rows, max(1, width))
}

func (w *Widget) ID() rovel.ID          { return w.id }
func (w *Widget) Rect() rovel.Rect      { return w.rect }
func (w *Widget) Layout(r rovel.Rect)   { w.rect = r }
func (w *Widget) MinSize() geom.Size    { return geom.Size{W: 10, H: 3} }
func (w *Widget) Focusable() bool       { return true }
func (w *Widget) IsTextInputMode() bool { return !w.readOnly }

func (w *Widget) Paint(d rovel.Drawer, ctx *rovel.Ctx) {
	if w == nil || w.rect.W <= 0 || w.rect.H <= 0 || ctx == nil {
		return
	}
	cd, ok := rovel.CellDrawerOf(d)
	if !ok {
		return
	}

	m := w.Model()
	width := w.rect.W
	rows := editorVisualRows(m.Text(), width)
	if len(rows) == 0 {
		rows = []visualRow{{text: "", start: 0, end: 0}}
	}
	cursorRow, cursorCol := visualCursor(rows, m.Cursor(), width)
	focused := ctx.FocusedID == w.id
	base := ctx.Theme.Base
	cursorStyle := ctx.Theme.Palette.Focus

	selectionStyle := ctx.Theme.Palette.Selection
	if selectionStyle == (style.Style{}) {
		selectionStyle = base.WithAttr(style.AttrReverse)
	}
	selStart, selEnd, hasSel := m.Selection()
	m.EnsureCursorVisible(w.rect.H, width)
	start := m.ScrollOffset()
	if start > len(rows) {
		start = len(rows)
	}

	for y := range w.rect.H {
		rowIdx := start + y
		x := w.rect.X
		remaining := width
		if rowIdx < len(rows) {
			row := rows[rowIdx]
			col := 0
			rowSelStart, rowSelEnd := rowSelectionRange(row, selStart, selEnd, hasSel)
			for off := 0; off < len(row.text) && remaining > 0; {
				next := text.NextCluster(row.text, off)
				if next <= off {
					break
				}
				cluster := row.text[off:next]
				st := base
				switch {
				case focused && rowIdx == cursorRow && col == cursorCol:
					st = cursorStyle
				case rowSelStart >= 0 && col >= rowSelStart && col < rowSelEnd:
					st = layerSelectionStyle(base, selectionStyle)
				}
				for _, r := range cluster {
					rw := rovel.RuneWidth(r)
					if rw <= 0 {
						continue
					}
					if rw > remaining {
						remaining = 0
						break
					}
					cd.SetCell(x, w.rect.Y+y, r, st)
					x += rw
					remaining -= rw
					col += rw
				}
				off = next
			}
			if focused && rowIdx == cursorRow && cursorCol >= col && remaining > 0 {
				cd.SetCell(x, w.rect.Y+y, ' ', cursorStyle)
				x++
				remaining--
			}
		}
		for remaining > 0 {
			cd.SetCell(x, w.rect.Y+y, ' ', base)
			x++
			remaining--
		}
	}
}

// rowSelectionRange clips the active selection [selStart,selEnd) (in absolute
// byte offsets within the editor's text) to the visible column range of one
// wrapped row. Returns (-1,-1) when the row has no selection overlap.
func rowSelectionRange(row visualRow, selStart, selEnd int, hasSel bool) (int, int) {
	if !hasSel || selEnd <= row.start || selStart >= row.end {
		return -1, -1
	}
	from := selStart
	if from < row.start {
		from = row.start
	}
	to := selEnd
	if to > row.end {
		to = row.end
	}
	if from >= to {
		return -1, -1
	}
	return text.ColumnOf(row.text, from-row.start), text.ColumnOf(row.text, to-row.start)
}

func layerSelectionStyle(base, sel style.Style) style.Style {
	out := base
	if sel.FG != style.ColorDefault {
		out.FG = sel.FG
	}
	if sel.BG != style.ColorDefault {
		out.BG = sel.BG
	}
	out.Attr |= sel.Attr
	return out
}

func (w *Widget) Handle(e rovel.Event, ctx *rovel.Ctx) bool {
	if w == nil || ctx == nil {
		return false
	}
	if me, ok := e.(event.MouseEvent); ok {
		return w.handleMouse(me, ctx)
	}
	if pe, ok := e.(event.PasteEvent); ok {
		return w.handlePaste(pe, ctx)
	}
	ke, ok := e.(event.KeyEvent)
	if !ok || ctx.FocusedID != w.id || w.readOnly {
		return false
	}
	if ke.Key != event.KeyRune || ke.Mod != 0 || ke.Rune < 32 {
		return false
	}
	old := w.Model().Cursor()
	w.Model().Insert(string(ke.Rune))
	w.invalidate(ctx)
	w.notifyChange(ctx)
	w.notifyCursorMoveIfChanged(old, CursorMoveEdit, ctx)
	return true
}

func (w *Widget) handleMouse(me event.MouseEvent, ctx *rovel.Ctx) bool {
	if me.Button != event.MouseButtonLeft {
		return false
	}
	m := w.Model()
	width := max(1, w.rect.W)
	offset, ok := w.byteOffsetFromMouse(me, width)
	if !ok {
		w.dragActive = false
		return false
	}
	old := m.Cursor()

	switch me.Action {
	case event.MousePress:
		w.dragActive = true
		w.dragAnchor = offset
		switch {
		case me.ClickCount >= 3:
			a, b, lineOK := lineByteRangeForOffset(m, offset)
			if lineOK {
				m.SetCursor(offset)
				m.SetSelection(a, b)
			}
		case me.ClickCount == 2:
			a, b, wordOK := wordByteRangeForOffset(m, offset)
			if wordOK {
				m.SetCursor(offset)
				m.SetSelection(a, b)
			}
		default:
			if me.Mod&event.ModShift != 0 {
				prevStart, prevEnd, hasPrev := m.Selection()
				m.SetCursor(offset)
				if hasPrev {
					anchor := prevStart
					if offset > prevStart && offset < prevEnd {
						anchor = prevEnd
					}
					m.SetSelection(anchor, offset)
				}
			} else {
				m.SetCursor(offset)
			}
		}
	case event.MouseDrag:
		if !w.dragActive {
			return false
		}
		m.SetCursor(offset)
		m.SetSelection(w.dragAnchor, offset)
	case event.MouseRelease:
		if !w.dragActive {
			return false
		}
		w.dragActive = false
		if text := m.CopySelection(); text != "" && ctx.ClipboardWrite != nil {
			ctx.ClipboardWrite(text)
		}
	default:
		return false
	}

	w.invalidate(ctx)
	w.notifyCursorMoveIfChanged(old, CursorMoveNavigate, ctx)
	return true
}

func (w *Widget) byteOffsetFromMouse(me event.MouseEvent, width int) (int, bool) {
	if w.rect.W <= 0 || w.rect.H <= 0 {
		return 0, false
	}
	relX := me.X - w.rect.X
	relY := me.Y - w.rect.Y
	if relY < 0 || relY >= w.rect.H {
		return 0, false
	}
	m := w.Model()
	rows := editorVisualRows(m.Text(), width)
	if len(rows) == 0 {
		rows = []visualRow{{text: "", start: 0, end: 0}}
	}
	row := rows[min(m.ScrollOffset()+relY, len(rows)-1)]
	col := min(max(relX, 0), text.Width(row.text))
	return row.start + text.OffsetAtColumnBias(row.text, col, text.BiasLeft), true
}

func wordByteRangeForOffset(m *Editor, offset int) (int, int, bool) {
	if m == nil {
		return 0, 0, false
	}
	value := m.Text()
	if offset < 0 || offset > len(value) {
		return 0, 0, false
	}
	if offset < len(value) {
		r, _ := utf8.DecodeRuneInString(value[offset:])
		if unicode.IsSpace(r) {
			return 0, 0, false
		}
	}
	start := offset
	for start > 0 {
		r, _ := utf8.DecodeLastRuneInString(value[:start])
		if unicode.IsSpace(r) {
			break
		}
		start--
	}
	end := offset
	for end < len(value) {
		r, sz := utf8.DecodeRuneInString(value[end:])
		if unicode.IsSpace(r) {
			break
		}
		end += sz
	}
	if start >= end {
		return 0, 0, false
	}
	return start, end, true
}

func lineByteRangeForOffset(m *Editor, offset int) (int, int, bool) {
	if m == nil {
		return 0, 0, false
	}
	value := m.Text()
	if offset < 0 || offset > len(value) {
		return 0, 0, false
	}
	start := strings.LastIndex(value[:offset], "\n") + 1
	end := len(value)
	if next := strings.IndexByte(value[offset:], '\n'); next >= 0 {
		end = offset + next
	}
	return start, end, true
}

func (w *Widget) HandleAction(act ui.Action, ctx *rovel.Ctx) bool {
	if w == nil || ctx == nil || ctx.FocusedID != w.id {
		return false
	}
	m := w.Model()
	old := m.Cursor()
	oldText := m.Text()
	reason := CursorMoveNavigate
	changed := false
	textChanged := false

	switch act {
	case ui.ActionMoveLeft:
		m.MoveLeft()
		changed = true
	case ui.ActionMoveRight:
		m.MoveRight()
		changed = true
	case ui.ActionMoveUp:
		if m.AtVisualTop(max(1, w.rect.W)) {
			m.HistoryPrev()
			textChanged = m.Text() != oldText
			reason = CursorMoveSetText
		} else {
			m.MoveVisualUp(max(1, w.rect.W))
		}
		changed = true
	case ui.ActionMoveDown:
		if m.AtVisualBottom(max(1, w.rect.W)) {
			m.HistoryNext()
			textChanged = m.Text() != oldText
			reason = CursorMoveSetText
		} else {
			m.MoveVisualDown(max(1, w.rect.W))
		}
		changed = true
	case ui.ActionHome:
		m.MoveLineStart()
		changed = true
	case ui.ActionEnd:
		m.MoveLineEnd()
		changed = true
	case ui.ActionPageUp:
		m.PageUp(max(1, w.rect.H))
		changed = true
	case ui.ActionPageDown:
		m.PageDown(max(1, w.rect.H))
		changed = true
	case ui.ActionDeleteBackward:
		if w.readOnly {
			return false
		}
		m.DeleteBackward()
		changed = true
		textChanged = true
		reason = CursorMoveEdit
	case ui.ActionDeleteForward:
		if w.readOnly {
			return false
		}
		m.DeleteForward()
		changed = true
		textChanged = true
		reason = CursorMoveEdit
	case ui.ActionSubmit:
		if w.readOnly {
			return false
		}
		m.Newline()
		changed = true
		textChanged = true
		reason = CursorMoveEdit
	default:
		return false
	}

	if changed {
		w.invalidate(ctx)
		if textChanged {
			w.notifyChange(ctx)
		}
		if reason == CursorMoveSetText || textChanged {
			w.notifyCursorMoveIfChanged(old, reason, ctx)
		} else {
			w.notifyCursorMoveIfChanged(old, reason, ctx)
		}
	}
	return true
}

func (w *Widget) handlePaste(e event.PasteEvent, ctx *rovel.Ctx) bool {
	if ctx.FocusedID != w.id || w.readOnly {
		return false
	}
	w.Paste(ctx, e.Text)
	return true
}

func (w *Widget) invalidate(ctx *rovel.Ctx) {
	if ctx != nil && ctx.Invalidate != nil {
		ctx.Invalidate(w.rect)
	}
}

func (w *Widget) notifyChange(ctx *rovel.Ctx) {
	if w.onChange != nil {
		w.onChange(w.Model().Text(), ctx)
	}
}

func (w *Widget) notifyCursorMoveIfChanged(old int, reason CursorMoveReason, ctx *rovel.Ctx) {
	cur := w.Model().Cursor()
	if cur != old && w.onCursorMove != nil {
		w.onCursorMove(CursorMoveEvent{Cursor: cur, Reason: reason}, ctx)
	}
}

type visualRow struct {
	text       string
	start, end int
}

func editorVisualRows(value string, width int) []visualRow {
	if width <= 0 {
		width = 1
	}
	var rows []visualRow
	lineStart := 0
	for {
		idx := strings.IndexByte(value[lineStart:], '\n')
		lineEnd := len(value)
		if idx >= 0 {
			lineEnd = lineStart + idx
		}
		line := value[lineStart:lineEnd]
		wrapped := text.WrapLines(line, text.WrapOptions{Width: width, Mode: text.WrapCluster})
		if len(wrapped) == 0 {
			rows = append(rows, visualRow{text: "", start: lineStart, end: lineStart})
		} else {
			for _, wr := range wrapped {
				rows = append(rows, visualRow{text: line[wr.StartByte:wr.EndByte], start: lineStart + wr.StartByte, end: lineStart + wr.EndByte})
			}
		}
		if idx < 0 {
			break
		}
		lineStart = lineEnd + 1
		if lineStart == len(value) {
			rows = append(rows, visualRow{text: "", start: lineStart, end: lineStart})
			break
		}
	}
	return rows
}

func visualCursor(rows []visualRow, cursor int, width int) (int, int) {
	if len(rows) == 0 {
		return 0, 0
	}
	for i, row := range rows {
		if cursor >= row.start && cursor <= row.end {
			return i, text.ColumnOf(row.text, cursor-row.start)
		}
	}
	last := rows[len(rows)-1]
	return len(rows) - 1, min(width, text.Width(last.text))
}
