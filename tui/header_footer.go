package tui

// HeaderFooter composes fixed header/footer/widget regions around a chat and
// editor viewport while preserving row order for cursor mapping.
type HeaderFooter struct {
	Header       RowRenderer
	Chat         RowRenderer
	Pending      RowRenderer
	Status       RowRenderer
	WidgetsAbove []RowRenderer
	Editor       RowRenderer
	WidgetsBelow []RowRenderer
	Footer       RowRenderer

	// Deprecated aliases retained for older callers.
	AboveEditor []RowRenderer
	BelowEditor []RowRenderer
}

func (h HeaderFooter) Render(width int) []string {
	var rows []string
	appendRows := func(r RowRenderer) {
		if r != nil {
			rows = append(rows, r.Render(width)...)
		}
	}
	appendRows(h.Header)
	appendRows(h.Chat)
	appendRows(h.Pending)
	appendRows(h.Status)
	above := h.WidgetsAbove
	if above == nil {
		above = h.AboveEditor
	}
	for _, widget := range above {
		appendRows(widget)
	}
	appendRows(h.Editor)
	below := h.WidgetsBelow
	if below == nil {
		below = h.BelowEditor
	}
	for _, widget := range below {
		appendRows(widget)
	}
	appendRows(h.Footer)
	return rows
}
