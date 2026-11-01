package tui

import (
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
	return []string{renderInputRow(m, c.Prompt, c.Focused, width)}
}

func renderInputRow(m *inputmodel.Input, prompt string, focused bool, width int) string {
	avail := width - TextWidth(prompt)
	if avail <= 0 {
		return ANSISliceStrict(prompt, 0, width)
	}
	value := m.Value()
	before, after := splitAtByte(value, m.Cursor())
	cursorCell, afterRest := cursorCell(after)
	cursorWidth := max(1, TextWidth(cursorCell))
	cursorCol := TextWidth(before)
	totalWidth := TextWidth(value)
	start := 0
	if totalWidth >= avail {
		scrollWidth := avail
		if after == "" {
			scrollWidth--
		}
		half := scrollWidth / 2
		switch {
		case cursorCol < half:
		case cursorCol > totalWidth-half:
			start = max(0, totalWidth-scrollWidth)
		default:
			start = max(0, cursorCol-half)
		}
	}
	visibleBefore := ANSISliceStrict(before, start, cursorCol)
	used := TextWidth(visibleBefore)
	if used+cursorWidth > avail {
		cursorCell = " "
		cursorWidth = 1
	}
	remaining := max(0, avail-used-cursorWidth)
	visibleAfter := ANSISliceStrict(afterRest, 0, remaining)

	cursor := cursorCell
	if focused {
		cursor = cursorString(cursorCell, true)
	}
	line := prompt + visibleBefore + cursor + visibleAfter
	visibleText := prompt + visibleBefore + cursorCell + visibleAfter
	return padRightVisible(line, visibleText, width)
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

func NormalizeSingleLinePastePi(s string) string { return inputmodel.NormalizePaste(s) }

func (c *InputComponent) Handle(e Event, _ *Ctx) bool {
	switch e := e.(type) {
	case KeyEvent:
		return c.ensure().HandleKey(e)
	case PasteEvent:
		c.ensure().Paste(e.Text)
		return true
	default:
		return false
	}
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
