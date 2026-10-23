package tui

import (
	"strings"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/text"
	editormodel "github.com/losinggeneration/tau/tui/editor"
	inputmodel "github.com/losinggeneration/tau/tui/input"
)

// InputPrimitive is a minimal single-line input overlay primitive. It composes
// existing overlay machinery with an input-capable row component for app and
// plugin dialogs.
type InputPrimitive struct {
	Prompt   string
	Value    string
	OnSubmit func(string)
	OnCancel func()

	model       *inputmodel.Input
	initialized bool
	scroll      int
}

func (p *InputPrimitive) ensureModel() *inputmodel.Input {
	if p.model == nil {
		p.model = inputmodel.New()
	}
	if !p.initialized || p.model.Value() != p.Value {
		p.model.SetValue(p.Value)
		p.initialized = true
	}
	return p.model
}

func (p *InputPrimitive) syncValue() {
	if p == nil || p.model == nil {
		return
	}
	p.Value = p.model.Value()
}

func (p *InputPrimitive) Render(width int) []string {
	if p == nil || width <= 0 {
		return nil
	}
	m := p.ensureModel()
	available := width - TextWidth(p.Prompt)
	if available < 1 {
		available = 1
	}
	before, after := splitAtByte(m.Value(), m.Cursor())
	beforeVisible := suffixByWidth(before, max(0, available-1), &p.scroll)
	usedBefore := TextWidth(beforeVisible)
	remaining := available - usedBefore - 1
	if remaining < 0 {
		remaining = 0
	}
	cursor, rest := cursorCell(after)
	afterVisible := prefixByWidth(rest, remaining)
	visibleText := p.Prompt + beforeVisible + cursor + afterVisible
	visible := p.Prompt + beforeVisible + cursorString(cursor, true) + afterVisible
	return []string{visible + repeatSpace(width-TextWidth(visibleText))}
}

func (p *InputPrimitive) HandleInput(data string) {
	if ke, ok := primitiveKeyFromInput(data); ok {
		p.HandleKey(ke)
	}
}

func (p *InputPrimitive) HandleKey(ke event.KeyEvent) {
	if p == nil {
		return
	}
	m := p.ensureModel()
	switch ke.Key {
	case event.KeyEnter:
		p.syncValue()
		if p.OnSubmit != nil {
			p.OnSubmit(p.Value)
		}
	case event.KeyEsc, event.KeyCtrlC:
		if p.OnCancel != nil {
			p.OnCancel()
		}
	case event.KeyLeft:
		if ke.Mod == event.ModAlt || ke.Mod == event.ModCtrl {
			m.MoveWordLeft()
		} else {
			m.MoveLeft()
		}
	case event.KeyRight:
		if ke.Mod == event.ModAlt || ke.Mod == event.ModCtrl {
			m.MoveWordRight()
		} else {
			m.MoveRight()
		}
	case event.KeyHome:
		m.Home()
	case event.KeyEnd:
		m.End()
	case event.KeyBackspace:
		if ke.Mod == event.ModAlt {
			m.DeleteWordBackward()
		} else {
			m.DeleteBackward()
		}
	case event.KeyDelete:
		if ke.Mod == event.ModAlt {
			m.DeleteWordForward()
		} else {
			m.DeleteForward()
		}
	case event.KeyRune:
		switch {
		case ke.Mod == event.ModCtrl && ke.Rune == 'b':
			m.MoveLeft()
		case ke.Mod == event.ModCtrl && ke.Rune == 'f':
			m.MoveRight()
		case ke.Mod == event.ModAlt && ke.Rune == 'b':
			m.MoveWordLeft()
		case ke.Mod == event.ModAlt && ke.Rune == 'f':
			m.MoveWordRight()
		case ke.Mod == event.ModCtrl && ke.Rune == 'a':
			m.Home()
		case ke.Mod == event.ModCtrl && ke.Rune == 'e':
			m.End()
		case ke.Mod == event.ModCtrl && ke.Rune == 'd':
			m.DeleteForward()
		case ke.Mod == event.ModCtrl && ke.Rune == 'w':
			m.DeleteWordBackward()
		case ke.Mod == event.ModAlt && ke.Rune == 'd':
			m.DeleteWordForward()
		case ke.Mod == event.ModCtrl && ke.Rune == 'u':
			m.DeleteToStart()
		case ke.Mod == event.ModCtrl && ke.Rune == 'k':
			m.DeleteToEnd()
		case ke.Mod == event.ModCtrl && ke.Rune == 'y':
			m.Yank()
		case ke.Mod == event.ModAlt && ke.Rune == 'y':
			m.YankPop()
		case ke.Mod == event.ModCtrl && ke.Rune == '-':
			m.Undo()
		default:
			if r, ok := ke.TextRune(); ok {
				m.Insert(string(r))
			}
		}
	}
	p.syncValue()
}

// Paste inserts normalized single-line clipboard text at the current cursor.
func (p *InputPrimitive) Paste(value string) {
	if p == nil {
		return
	}
	p.ensureModel().Paste(value)
	p.syncValue()
}

func (p *InputPrimitive) SetSelection(start, end int) {
	if p == nil {
		return
	}
	p.ensureModel().SetSelection(start, end)
	p.syncValue()
}

func (p *InputPrimitive) CopySelection() string {
	if p == nil {
		return ""
	}
	return p.ensureModel().CopySelection()
}

func (p *InputPrimitive) CutSelection() string {
	if p == nil {
		return ""
	}
	out := p.ensureModel().CutSelection()
	p.syncValue()
	return out
}

func splitAtByte(s string, pos int) (string, string) {
	if pos < 0 {
		pos = 0
	}
	if pos > len(s) {
		pos = len(s)
	}
	pos = text.ClampCluster(s, pos)
	return s[:pos], s[pos:]
}

func suffixByWidth(s string, width int, _ *int) string {
	if TextWidth(s) <= width {
		return s
	}
	return ANSISliceStrict(s, max(0, TextWidth(s)-width), TextWidth(s))
}

func prefixByWidth(s string, width int) string {
	return ANSISliceStrict(s, 0, width)
}

// EditorPrimitive renders and routes keys for the multiline editor model used
// by prompt-style overlays and app prompt integration.
type EditorPrimitive struct {
	Model       *editormodel.Editor
	OnSubmit    func(string)
	OnCancel    func()
	MaxRows     int
	SubmitSlash bool
	renderWidth int
}

func (p *EditorPrimitive) ensureModel() *editormodel.Editor {
	if p.Model == nil {
		p.Model = editormodel.New()
	}
	if p.OnSubmit != nil {
		p.Model.SetOnSubmit(p.OnSubmit)
	}
	return p.Model
}

func (p *EditorPrimitive) Render(width int) []string {
	if p == nil || width <= 0 {
		return nil
	}
	m := p.ensureModel()
	p.renderWidth = width
	lines := m.Lines()
	pos := m.Position()
	out := []string{}
	for i, line := range lines {
		visual := editormodel.VisualLines(line, width)
		if len(visual) == 0 {
			visual = []string{""}
		}
		segmentStartCol := 0
		for _, vline := range visual {
			lineWidth := TextWidth(vline)
			if i == pos.Line && pos.Col >= segmentStartCol && pos.Col <= segmentStartCol+lineWidth {
				col := pos.Col - segmentStartCol
				before := ANSISliceStrict(vline, 0, col)
				after := strings.TrimPrefix(vline, before)
				out = append(out, before+CursorMarker+after+repeatSpace(width-TextWidth(before+after)))
				segmentStartCol += lineWidth
				continue
			}
			out = append(out, padRightANSI(vline, width))
			segmentStartCol += lineWidth
		}
	}
	if len(out) == 0 {
		out = append(out, CursorMarker+repeatSpace(width))
	}
	if p.MaxRows > 0 {
		m.EnsureCursorVisible(p.MaxRows, width)
		start := m.ScrollOffset()
		if start > len(out) {
			start = len(out)
		}
		end := start + p.MaxRows
		if end > len(out) {
			end = len(out)
		}
		out = out[start:end]
	}
	return out
}

func (p *EditorPrimitive) lastRenderWidth() int {
	if p == nil || p.renderWidth <= 0 {
		return 80
	}
	return p.renderWidth
}

func (p *EditorPrimitive) HandleInput(data string) {
	if ke, ok := primitiveKeyFromInput(data); ok {
		p.HandleKey(ke)
	}
}

func (p *EditorPrimitive) HandleKey(ke event.KeyEvent) {
	if p == nil {
		return
	}
	m := p.ensureModel()
	switch ke.Key {
	case event.KeyEnter:
		if ke.Mod == event.ModShift {
			m.Newline()
			return
		}
		if m.AutocompleteVisible() {
			m.ApplyAutocomplete()
			return
		}
		if strings.HasSuffix(m.Text(), "\\") {
			m.SetText(strings.TrimSuffix(m.Text(), "\\"))
			m.Newline()
			return
		}
		m.Submit()
	case event.KeyTab:
		if m.AutocompleteVisible() {
			m.ApplyAutocomplete()
		}
	case event.KeyEsc:
		if m.AutocompleteVisible() {
			m.CancelAutocomplete()
			return
		}
		m.Cancel()
		if p.OnCancel != nil {
			p.OnCancel()
		}
	case event.KeyCtrlC:
		return
	case event.KeyLeft:
		if ke.Mod == event.ModAlt || ke.Mod == event.ModCtrl {
			m.MoveWordLeft()
		} else {
			m.MoveLeft()
		}
	case event.KeyRight:
		if ke.Mod == event.ModAlt || ke.Mod == event.ModCtrl {
			m.MoveWordRight()
		} else {
			m.MoveRight()
		}
	case event.KeyUp:
		if m.AutocompleteVisible() {
			m.AutocompletePrev()
			return
		}
		m.MoveVisualUp(max(1, p.lastRenderWidth()))
	case event.KeyDown:
		if m.AutocompleteVisible() {
			m.AutocompleteNext()
			return
		}
		m.MoveVisualDown(max(1, p.lastRenderWidth()))
	case event.KeyPageUp:
		m.PageUp(10)
	case event.KeyPageDown:
		m.PageDown(10)
	case event.KeyHome:
		m.MoveLineStart()
	case event.KeyEnd:
		m.MoveLineEnd()
	case event.KeyBackspace:
		if ke.Mod == event.ModAlt {
			m.DeleteWordBackward()
		} else {
			m.DeleteBackward()
		}
	case event.KeyDelete:
		if ke.Mod == event.ModAlt {
			m.DeleteWordForward()
		} else {
			m.DeleteForward()
		}
	case event.KeyRune:
		switch {
		case ke.Mod == event.ModCtrl && ke.Rune == 'b':
			m.MoveLeft()
		case ke.Mod == event.ModCtrl && ke.Rune == 'f':
			m.MoveRight()
		case ke.Mod == event.ModCtrl && ke.Rune == 'a':
			m.MoveLineStart()
		case ke.Mod == event.ModCtrl && ke.Rune == 'e':
			m.MoveLineEnd()
		case ke.Mod == event.ModCtrl && ke.Rune == 'd':
			m.DeleteForward()
		case ke.Mod == event.ModCtrl && ke.Rune == 'w':
			m.DeleteWordBackward()
		case ke.Mod == event.ModAlt && ke.Rune == 'd':
			m.DeleteWordForward()
		case ke.Mod == event.ModCtrl && ke.Rune == 'u':
			m.DeleteToLineStart()
		case ke.Mod == event.ModCtrl && ke.Rune == 'k':
			m.DeleteToLineEnd()
		case ke.Mod == event.ModCtrl && ke.Rune == 'y':
			m.Yank()
		case ke.Mod == event.ModAlt && ke.Rune == 'y':
			m.YankPop()
		case ke.Mod == event.ModCtrl && ke.Rune == '-':
			m.Undo()
		case ke.Mod == event.ModAlt && ke.Rune == 'b':
			m.MoveWordLeft()
		case ke.Mod == event.ModAlt && ke.Rune == 'f':
			m.MoveWordRight()
		default:
			if r, ok := ke.TextRune(); ok {
				m.Insert(string(r))
			}
		}
	}
}

func primitiveKeyFromInput(data string) (event.KeyEvent, bool) {
	switch data {
	case "\r", "\n":
		return event.KeyEvent{Key: event.KeyEnter}, true
	case "\x1b":
		return event.KeyEvent{Key: event.KeyEsc}, true
	case "\x03":
		return event.KeyEvent{Key: event.KeyCtrlC}, true
	case "\t":
		return event.KeyEvent{Key: event.KeyTab}, true
	case "\x7f", "\b":
		return event.KeyEvent{Key: event.KeyBackspace}, true
	case "\x1b[A":
		return event.KeyEvent{Key: event.KeyUp}, true
	case "\x1b[B":
		return event.KeyEvent{Key: event.KeyDown}, true
	case "\x1b[C":
		return event.KeyEvent{Key: event.KeyRight}, true
	case "\x1b[D":
		return event.KeyEvent{Key: event.KeyLeft}, true
	}
	if len([]rune(data)) == 1 {
		r := []rune(data)[0]
		if r >= 32 {
			return event.KeyEvent{Key: event.KeyRune, Rune: r}, true
		}
	}
	return event.KeyEvent{}, false
}

func (p *EditorPrimitive) SetSelection(start, end int) {
	if p == nil {
		return
	}
	p.ensureModel().SetSelection(start, end)
}

func (p *EditorPrimitive) CopySelection() string {
	if p == nil {
		return ""
	}
	return p.ensureModel().CopySelection()
}

func (p *EditorPrimitive) CutSelection() string {
	if p == nil {
		return ""
	}
	return p.ensureModel().CutSelection()
}

// ConfirmPrimitive is a yes/no overlay primitive.
type ConfirmPrimitive struct {
	Message string
	OnDone  func(bool)
}

func (p *ConfirmPrimitive) Render(width int) []string {
	if p == nil || width <= 0 {
		return nil
	}
	return []string{padRight(p.Message+" [y/N]", width)}
}

func (p *ConfirmPrimitive) HandleInput(data string) {
	if p == nil || p.OnDone == nil {
		return
	}
	switch data {
	case "y", "Y", "\r", "\n":
		p.OnDone(true)
	case "n", "N", "\x1b", "\x03":
		p.OnDone(false)
	}
}
