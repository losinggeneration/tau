package tui

import "strings"

// DynamicBorderState is the semantic state a bordered section is in. It selects
// the border colour through a DynamicBorderStyleFunc rather than being drawn
// differently, so every bordered surface signals state the same way.
type DynamicBorderState uint8

const (
	DynamicBorderNeutral DynamicBorderState = iota
	DynamicBorderRunning
	DynamicBorderSuccess
	DynamicBorderError
	DynamicBorderCancelled

	// DynamicBorderStateCount bounds the state range, so applications can
	// resolve every state's colour into a fixed table up front.
	DynamicBorderStateCount
)

// The colour a state resolves to is the application's decision, not the
// component's: tau maps these onto its theme tokens. The component only fixes
// what the states are, so every bordered surface agrees on the vocabulary.

// DynamicBorder renders a titled box as terminal rows. It is deliberately a
// row producer rather than a View: its consumers (the chat log's tool chrome,
// bordered loaders) already own row lists and paint them themselves, and a
// View would force them through a second layout pass.
//
// Per the visual spec this is for bash execution, bordered loaders,
// selectors/dialogs and explicit sections — it is NOT the shell for ordinary
// user, assistant or tool messages, which render flat.
type DynamicBorder struct {
	// PaddingX overrides the default one-cell horizontal inset.
	PaddingX *int
	// Title appears inline in the top edge. It is truncated to fit and may be
	// empty for an untitled box.
	Title string

	// Width is the total width of the box including both vertical edges.
	Width int
}

// borderGlyphs are the rounded box-drawing characters the dynamic border uses.
// They are fixed rather than themed: the border's variable is its colour, and
// a section that changed shape between states would read as a different
// component rather than the same one in a new state.
const (
	borderTopLeft     = "╭"
	borderTopRight    = "╮"
	borderBottomLeft  = "╰"
	borderBottomRight = "╯"
	borderLeftTee     = "├"
	borderRightTee    = "┤"
	borderVertical    = "│"
	borderHorizontal  = "─"
)

// InnerWidth is the usable text width between the edges and their padding.
func (b DynamicBorder) InnerWidth() int {
	return max(1, b.Width-2-2*b.paddingX())
}

func (b DynamicBorder) paddingX() int {
	if b.PaddingX == nil {
		return 1
	}
	return min(max(0, *b.PaddingX), max(0, (b.Width-2)/2))
}

// Top renders the top edge, with Title inlined when there is room for it.
func (b DynamicBorder) Top() string {
	if b.Width <= 0 {
		return ""
	}
	if b.Width == 1 {
		return borderTopLeft
	}
	title := TextTruncate(b.Title, max(0, b.Width-2), false)
	used := TextWidth(title)
	return borderTopLeft + title + strings.Repeat(borderHorizontal, max(0, b.Width-2-used)) + borderTopRight
}

// Rule renders an internal divider, used to separate a section's header from
// its body.
func (b DynamicBorder) Rule() string {
	if b.Width <= 0 {
		return ""
	}
	if b.Width == 1 {
		return borderLeftTee
	}
	return borderLeftTee + strings.Repeat(borderHorizontal, max(0, b.Width-2)) + borderRightTee
}

// Bottom renders the bottom edge.
func (b DynamicBorder) Bottom() string {
	if b.Width <= 0 {
		return ""
	}
	if b.Width == 1 {
		return borderBottomLeft
	}
	return borderBottomLeft + strings.Repeat(borderHorizontal, max(0, b.Width-2)) + borderBottomRight
}

// Line renders one content row, padded so the right edge lands at Width.
func (b DynamicBorder) Line(text string) string {
	if b.Width <= 0 {
		return ""
	}
	if b.Width == 1 {
		return borderVertical
	}
	if b.Width == 2 {
		// Two edges and nothing between them: there is no content column, and
		// the usual leading-pad layout would overflow by one cell.
		return borderVertical + borderVertical
	}
	inner := TextTruncate(text, max(0, b.Width-2-2*b.paddingX()), false)
	line := borderVertical + strings.Repeat(" ", b.paddingX()) + inner
	if width := TextWidth(line); width < b.Width-1 {
		line += strings.Repeat(" ", b.Width-1-width)
	}
	return line + borderVertical
}

// Wrap renders a complete box around rows, wrapping each to InnerWidth.
func (b DynamicBorder) Wrap(rows []string) []string {
	if b.Width <= 0 {
		return nil
	}
	out := make([]string, 0, len(rows)+2)
	out = append(out, b.Top())
	for _, row := range rows {
		wrapped := WrapLines(row, WrapOptions{Width: b.InnerWidth(), Mode: WrapWord})
		if len(wrapped) == 0 {
			out = append(out, b.Line(""))
			continue
		}
		for _, wl := range wrapped {
			out = append(out, b.Line(row[wl.StartByte:wl.EndByte]))
		}
	}
	return append(out, b.Bottom())
}
