package tui

import (
	"unicode"

	"github.com/losinggeneration/rovel/text"
	inputmodel "github.com/losinggeneration/tau/tui/input"
)

const (
	ansiInverseOn  = "\x1b[7m"
	ansiInverseOff = "\x1b[27m"
)

// InputComponent renders a Pi-compatible single-line input row backed by the
// existing input model.
type InputComponent struct {
	id      ID
	Model   *inputmodel.Input
	Prompt  string
	Focused bool
	rect    Rect
}

func NewInputComponent() *InputComponent {
	return &InputComponent{id: NewID(), Model: inputmodel.New(), Prompt: "> ", Focused: true}
}

func (c *InputComponent) ensure() *inputmodel.Input {
	if c.Model == nil {
		c.Model = inputmodel.New()
	}
	if c.Prompt == "" {
		c.Prompt = "> "
	}
	return c.Model
}

func (c *InputComponent) SetValue(s string) { c.ensure().SetValue(s) }
func (c *InputComponent) Value() string     { return c.ensure().Value() }
func (c *InputComponent) SetFocused(v bool) { c.Focused = v }

func (c *InputComponent) Render(width int) []string {
	if c == nil || width <= 0 {
		return nil
	}
	m := c.ensure()
	prompt := c.Prompt
	avail := width - TextWidth(prompt)
	if avail < 1 {
		avail = 1
	}
	value := m.Value()
	before, after := splitAtByte(value, m.Cursor())
	cursorCell, afterRest := cursorCell(after)
	beforeW := TextWidth(before)
	start := 0
	if beforeW >= avail {
		start = beforeW - avail + 1
	}
	visibleBefore := ANSISliceStrict(before, start, beforeW)
	used := TextWidth(visibleBefore)
	remaining := avail - used - 1
	if remaining < 0 {
		remaining = 0
	}
	visibleAfter := ANSISliceStrict(afterRest, 0, remaining)
	cursor := cursorCell
	if c.Focused {
		cursor = cursorString(cursorCell, true)
	}
	line := prompt + visibleBefore + cursor + visibleAfter
	visibleText := prompt + visibleBefore + cursorCell + visibleAfter
	return []string{padRightVisible(line, visibleText, width)}
}

func padRightVisible(rendered, visible string, width int) string {
	if width <= 0 {
		return ""
	}
	w := TextWidth(visible)
	if w >= width {
		return rendered
	}
	return rendered + repeatSpace(width-w)
}

func cursorCell(s string) (string, string) {
	if s == "" {
		return " ", ""
	}
	next := text.NextCluster(s, 0)
	if next <= 0 || next > len(s) {
		return " ", ""
	}
	return s[:next], s[next:]
}

func NormalizeSingleLinePastePi(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '\r', '\n':
			continue
		case '\t':
			out = append(out, ' ', ' ', ' ', ' ')
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

func (c *InputComponent) Handle(e Event, _ *Ctx) bool {
	ke, ok := e.(KeyEvent)
	if !ok {
		return false
	}
	m := c.ensure()
	switch ke.Key {
	case KeyRune:
		if r, ok := ke.TextRune(); ok {
			m.Insert(string(r))
			return true
		}
	case KeyLeft:
		m.MoveLeft()
		return true
	case KeyRight:
		m.MoveRight()
		return true
	case KeyHome:
		m.Home()
		return true
	case KeyEnd:
		m.End()
		return true
	case KeyBackspace:
		m.DeleteBackward()
		return true
	case KeyDelete:
		m.DeleteForward()
		return true
	}
	return false
}

func (c *InputComponent) ID() ID {
	if c.id == 0 {
		c.id = NewID()
	}
	return c.id
}
func (c *InputComponent) Rect() Rect      { return c.rect }
func (c *InputComponent) Layout(r Rect)   { c.rect = r }
func (c *InputComponent) MinSize() Size   { return Size{W: 1, H: 1} }
func (c *InputComponent) Focusable() bool { return true }
func (c *InputComponent) Paint(d Drawer, ctx *Ctx) {
	for i, row := range c.Render(c.rect.W) {
		drawCursorRow(d, Point{X: c.rect.X, Y: c.rect.Y + i}, row, Style{}, ctx != nil && ctx.FocusedID == c.ID())
	}
}
