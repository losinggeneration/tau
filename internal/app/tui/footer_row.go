package tui

import (
	"strings"

	tauui "github.com/losinggeneration/tau/tui"
)

// footerSpan is a run of footer text with its own style.
//
// The footer used to render one string per row and paint it in one style, which
// made the spec's threshold rule impossible to express: app.md §10.3 wants the
// context percentage warning- or error-coloured while the rest of the row stays
// dim, and a single style per row can only recolour everything.
type footerSpan struct {
	text  string
	style tauui.Style
}

// footerRow is a sequence of spans painted left to right.
type footerRow []footerSpan

// text returns the row's plain text, which is what width calculations and the
// non-view Render helper need.
func (r footerRow) text() string {
	var b strings.Builder
	for _, s := range r {
		b.WriteString(s.text)
	}
	return b.String()
}

// width returns the row's rendered cell width.
func (r footerRow) width() int { return tauui.TextWidth(r.text()) }

// truncate clips the row to width cells, dropping and shortening spans from the
// right. It returns a new row; the receiver is untouched.
//
// Truncation is span-aware rather than done on the joined string because
// cutting the joined text would lose the span boundaries the styles hang off.
func (r footerRow) truncate(width int) footerRow {
	if width <= 0 {
		return nil
	}
	if r.width() <= width {
		return r
	}
	out := make(footerRow, 0, len(r))
	used := 0
	for _, s := range r {
		remaining := width - used
		if remaining <= 0 {
			break
		}
		w := tauui.TextWidth(s.text)
		if w <= remaining {
			out = append(out, s)
			used += w
			continue
		}
		// This span is the one that straddles the edge; clip it and stop.
		out = append(out, footerSpan{text: tauui.TextTruncate(s.text, remaining, false), style: s.style})
		break
	}
	return out
}

// pad appends spaces in style until the row fills width. Used to right-align
// the model side of the stats row.
func (r footerRow) pad(width int, style tauui.Style) footerRow {
	gap := width - r.width()
	if gap <= 0 {
		return r
	}
	return append(r, footerSpan{text: strings.Repeat(" ", gap), style: style})
}

// append returns r with other's spans added.
func (r footerRow) append(other footerRow) footerRow {
	return append(r, other...)
}

// join concatenates rows.
func joinFooterRows(rows ...footerRow) footerRow {
	var out footerRow
	for _, r := range rows {
		out = append(out, r...)
	}
	return out
}

// paintFooterRow draws the spans left to right within rect, filling the whole
// row with base first so the row has a consistent background.
func paintFooterRow(d tauui.Drawer, rect tauui.Rect, row footerRow, base tauui.Style) {
	if rect.W <= 0 || rect.H <= 0 {
		return
	}
	d.FillRect(rect, base)
	x := rect.X
	for _, s := range row {
		if s.text == "" {
			continue
		}
		if x >= rect.X+rect.W {
			return
		}
		d.DrawText(tauui.Point{X: x, Y: rect.Y}, s.text, s.style)
		x += tauui.TextWidth(s.text)
	}
}
