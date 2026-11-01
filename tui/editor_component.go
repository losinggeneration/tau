package tui

import (
	"strconv"
	"strings"

	"github.com/losinggeneration/tau/tui/autocomplete"
	editormodel "github.com/losinggeneration/tau/tui/editor"
)

// EditorComponent renders a Pi-style framed multiline editor around the
// existing editor model.
type EditorComponent struct {
	Model          *editormodel.Editor
	PaddingX       int
	Focused        bool
	MaxVisibleRows int
	TerminalRows   int
}

func NewEditorComponent(model *editormodel.Editor) *EditorComponent {
	if model == nil {
		model = editormodel.New()
	}
	return &EditorComponent{Model: model, Focused: true}
}

func (c *EditorComponent) ensure() *editormodel.Editor {
	if c.Model == nil {
		c.Model = editormodel.New()
	}
	return c.Model
}

func (c *EditorComponent) Render(width int) []string {
	if c == nil || width <= 0 {
		return nil
	}
	m := c.ensure()
	padX := c.PaddingX
	if padX < 0 {
		padX = 0
	}
	if padX*2 >= width {
		padX = max(0, (width-1)/2)
	}
	contentW := width - 2*padX
	if contentW < 1 {
		contentW = 1
	}
	layoutW := contentW
	if padX == 0 && layoutW > 1 {
		layoutW--
	}
	visible := c.maxVisibleRows()
	content := editorContentRows(m, layoutW, contentW, padX, c.Focused && !m.AutocompleteVisible())
	if len(content) == 0 {
		content = []string{padRightNoEllipsis(repeatSpace(padX)+cursorString(" ", c.Focused)+repeatSpace(padX), width)}
	}
	m.EnsureCursorVisible(visible, layoutW)
	start := m.ScrollOffset()
	if start > len(content) {
		start = len(content)
	}
	end := start + visible
	if end > len(content) {
		end = len(content)
	}
	top := strings.Repeat("─", width)
	bottom := top
	if start > 0 {
		top = scrollRule(width, "↑", start)
	}
	if end < len(content) {
		bottom = scrollRule(width, "↓", len(content)-end)
	}
	out := []string{top}
	out = append(out, content[start:end]...)
	out = append(out, bottom)
	if m.AutocompleteVisible() {
		out = append(out, renderAutocomplete(m, contentW, padX)...)
	}
	return out
}

func (c *EditorComponent) maxVisibleRows() int {
	if c.MaxVisibleRows > 0 {
		return c.MaxVisibleRows
	}
	if c.TerminalRows > 0 {
		return max(5, c.TerminalRows*3/10)
	}
	return 5
}

func editorContentRows(m *editormodel.Editor, layoutW, contentW, padX int, focused bool) []string {
	lines := m.Lines()
	pos := m.Position()
	out := []string{}
	prefix := repeatSpace(padX)
	for i, line := range lines {
		visual := editormodel.VisualLines(line, layoutW)
		if len(visual) == 0 {
			visual = []string{""}
		}
		segmentStart := 0
		for j, vline := range visual {
			lineWidth := TextWidth(vline)
			cell := vline
			if i == pos.Line && pos.Col >= segmentStart && (pos.Col < segmentStart+lineWidth || j == len(visual)-1 && pos.Col == segmentStart+lineWidth) {
				col := pos.Col - segmentStart
				before := ANSISliceStrict(vline, 0, col)
				after := strings.TrimPrefix(vline, before)
				cur, rest := cursorCell(after)
				cell = before + cursorString(cur, focused) + rest
				visibleCell := before + cur + rest
				out = append(out, padRightVisible(prefix+cell, prefix+visibleCell, padX*2+contentW))
				segmentStart += lineWidth
				continue
			}
			out = append(out, padRightNoEllipsis(prefix+padRightNoEllipsis(cell, contentW), padX*2+contentW))
			segmentStart += lineWidth
		}
	}
	return out
}

func cursorString(cell string, focused bool) string {
	if !focused {
		return cell
	}
	return CursorMarker + fakeCursorStart + cell + fakeCursorEnd
}

func scrollRule(width int, arrow string, count int) string {
	width = max(0, width)
	label := " " + arrow + " " + strconv.Itoa(count) + " more "
	if labelWidth := TextWidth(label); labelWidth+2 <= width {
		left := (width - labelWidth) / 2
		return strings.Repeat("─", left) + label + strings.Repeat("─", width-left-labelWidth)
	}
	indicator := "───" + label
	if remaining := width - TextWidth(indicator); remaining >= 0 {
		return indicator + strings.Repeat("─", remaining)
	}
	ellipsis := "..."[:min(3, width)]
	return ANSISliceStrict(indicator, 0, width-len(ellipsis)) + ellipsis
}

func renderAutocomplete(m *editormodel.Editor, contentW, padX int) []string {
	items := m.AutocompleteItems()
	selectItems := make([]SelectItem, 0, len(items))
	for _, item := range items {
		selectItems = append(selectItems, SelectItem{Value: item.Value, Label: autocompleteLabel(item), Description: item.Description})
	}
	list := NewSelectList(selectItems, SelectListLayoutOptions{MaxVisible: m.AutocompleteMaxVisible()}, SelectListTheme{})
	list.SetSelectedIndex(m.AutocompleteIndex())
	rows := list.Render(contentW)
	prefix := repeatSpace(padX)
	for i, row := range rows {
		rows[i] = padRightNoEllipsis(prefix+row, contentW+padX*2)
	}
	return rows
}

func autocompleteLabel(item autocomplete.Item) string {
	if item.Label != "" {
		return item.Label
	}
	return item.Value
}
