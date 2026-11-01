package tui

import (
	"strings"

	rovelansi "github.com/losinggeneration/rovel/backend/ansi"
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/text"
	"github.com/losinggeneration/rovel/ui"
	"github.com/losinggeneration/tau/tui/ansi"
	editormodel "github.com/losinggeneration/tau/tui/editor"
	inputmodel "github.com/losinggeneration/tau/tui/input"
	"github.com/losinggeneration/tau/tui/keybindings"
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
	input       rovelansi.DecodeInput
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
	return []string{renderInputRow(p.ensureModel(), p.Prompt, true, width)}
}

func (p *InputPrimitive) HandleInput(data string) {
	if p == nil {
		return
	}
	for _, ev := range p.input.FlushPending(p.input.Push(nil, []byte(data))) {
		switch ev := ev.(type) {
		case event.KeyEvent:
			p.HandleKey(ev)
		case event.PasteEvent:
			p.Paste(ev.Text)
		}
	}
}

func (p *InputPrimitive) HandleKey(ke event.KeyEvent) {
	if p == nil {
		return
	}
	m := p.ensureModel()
	kb := keybindings.DefaultManager()
	key := ui.KeystrokeOf(ke)
	switch {
	case kb.Matches(key, "tui.select.cancel"):
		if p.OnCancel != nil {
			p.OnCancel()
		}
	case kb.Matches(key, "tui.editor.undo"):
		m.Undo()
	case kb.Matches(key, "tui.input.submit"):
		p.syncValue()
		if p.OnSubmit != nil {
			p.OnSubmit(p.Value)
		}
	default:
		m.HandleKey(ke)
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

// EditorPrimitive renders and routes keys for the multiline editor model used
// by prompt-style overlays and app prompt integration.
type EditorPrimitive struct {
	Model       *editormodel.Editor
	OnSubmit    func(string)
	OnCancel    func()
	MaxRows     int
	SubmitSlash bool
	renderWidth int
	input       rovelansi.DecodeInput
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
		for j, vline := range visual {
			lineWidth := TextWidth(vline)
			if i == pos.Line && pos.Col >= segmentStartCol && (pos.Col < segmentStartCol+lineWidth || j == len(visual)-1 && pos.Col == segmentStartCol+lineWidth) {
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

func (p *EditorPrimitive) pageRows() int {
	if p.MaxRows > 0 {
		return p.MaxRows
	}
	return 10
}

func (p *EditorPrimitive) HandleInput(data string) {
	if p == nil {
		return
	}
	for _, ev := range p.input.FlushPending(p.input.Push(nil, []byte(data))) {
		switch ev := ev.(type) {
		case event.KeyEvent:
			p.HandleKey(ev)
		case event.PasteEvent:
			p.ensureModel().Paste(ev.Text)
		}
	}
}

func (p *EditorPrimitive) HandleKey(ke event.KeyEvent) {
	if p == nil {
		return
	}
	m := p.ensureModel()
	if m.HandleJumpKey(ke) {
		return
	}
	kb := keybindings.DefaultManager()
	key := ui.KeystrokeOf(ke)
	switch {
	case kb.Matches(key, "tui.input.copy"):
		return
	case kb.Matches(key, "tui.editor.undo"):
		m.Undo()
		return
	}
	if m.AutocompleteVisible() {
		switch {
		case kb.Matches(key, "tui.select.cancel"):
			m.CancelAutocomplete()
			return
		case kb.Matches(key, "tui.select.up"):
			m.AutocompletePrev()
			return
		case kb.Matches(key, "tui.select.down"):
			m.AutocompleteNext()
			return
		case kb.Matches(key, "tui.input.tab"):
			m.ApplyAutocomplete()
			return
		case kb.Matches(key, "tui.select.confirm"):
			slash := strings.HasPrefix(m.AutocompletePrefix(), "/")
			m.ApplyAutocomplete()
			if !slash {
				return
			}
		}
	}
	switch {
	case kb.Matches(key, "tui.input.tab"):
		return
	case kb.Matches(key, "tui.editor.deleteToLineEnd"):
		m.DeleteToLineEnd()
	case kb.Matches(key, "tui.editor.deleteToLineStart"):
		m.DeleteToLineStart()
	case kb.Matches(key, "tui.editor.deleteWordBackward"):
		m.DeleteWordBackward()
	case kb.Matches(key, "tui.editor.deleteWordForward"):
		m.DeleteWordForward()
	case kb.Matches(key, "tui.editor.deleteCharBackward"):
		m.DeleteBackward()
	case kb.Matches(key, "tui.editor.deleteCharForward"):
		m.DeleteForward()
	case kb.Matches(key, "tui.editor.yank"):
		m.Yank()
	case kb.Matches(key, "tui.editor.yankPop"):
		m.YankPop()
	case kb.Matches(key, "tui.editor.historyPrevious"):
		m.CancelAutocomplete()
		m.HistoryPrev()
	case kb.Matches(key, "tui.editor.historyNext"):
		m.CancelAutocomplete()
		m.HistoryNext()
	case kb.Matches(key, "tui.editor.cursorLineStart"):
		m.MoveLineStart()
	case kb.Matches(key, "tui.editor.cursorLineEnd"):
		m.MoveLineEnd()
	case kb.Matches(key, "tui.editor.cursorWordLeft"):
		m.MoveWordLeft()
	case kb.Matches(key, "tui.editor.cursorWordRight"):
		m.MoveWordRight()
	case kb.Matches(key, "tui.input.newLine"):
		if ke.Key == event.KeyEnter && ke.Mod == 0 &&
			kb.Matches(ui.Keystroke{Key: event.KeyEnter, Mod: event.ModShift}, "tui.input.submit") &&
			m.Cursor() > 0 && m.Text()[m.Cursor()-1] == '\\' {
			m.DeleteBackward()
			m.Submit()
			return
		}
		m.Newline()
	case kb.Matches(key, "tui.input.submit"):
		if m.Cursor() > 0 && m.Text()[m.Cursor()-1] == '\\' {
			m.DeleteBackward()
			m.Newline()
			return
		}
		m.Submit()
	case kb.Matches(key, "tui.editor.cursorUp"):
		m.CursorUp(max(1, p.lastRenderWidth()))
	case kb.Matches(key, "tui.editor.cursorDown"):
		m.CursorDown(max(1, p.lastRenderWidth()))
	case kb.Matches(key, "tui.editor.cursorRight"):
		m.MoveRight()
	case kb.Matches(key, "tui.editor.cursorLeft"):
		m.MoveLeft()
	case kb.Matches(key, "tui.editor.pageUp"):
		m.MoveVisual(max(1, p.lastRenderWidth()), -p.pageRows())
	case kb.Matches(key, "tui.editor.pageDown"):
		m.MoveVisual(max(1, p.lastRenderWidth()), p.pageRows())
	case kb.Matches(key, "tui.select.cancel"):
		m.Cancel()
		if p.OnCancel != nil {
			p.OnCancel()
		}
	default:
		if r, ok := ansi.TextRune(ke); ok {
			m.Insert(string(r))
		}
	}
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
