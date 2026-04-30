package tui

import (
	tauui "github.com/losinggeneration/tau/tui"
)

// MaxWidgetLines is the per-widget line cap enforced by the widget view.
const MaxWidgetLines = 10

type widgetPlacement int

const (
	widgetPlacementAbove widgetPlacement = iota
	widgetPlacementBelow
)

type widgetEntry struct {
	key   string
	lines []string
}

// widgetContainer renders extension string-array widgets. Above-editor widgets
// reserve a leading spacer row only when they have content; both placements
// collapse when empty.
type widgetContainer struct {
	id        tauui.ID
	rect      tauui.Rect
	widgets   map[string][]string
	entries   []widgetEntry
	placement widgetPlacement
}

func NewWidgetContainer(p widgetPlacement) *widgetContainer {
	return &widgetContainer{id: tauui.NewID(), placement: p}
}

func (w *widgetContainer) SetWidgets(widgets map[string][]string) {
	if len(widgets) == 0 {
		w.widgets = nil
		w.entries = nil
		return
	}
	entries := make([]widgetEntry, 0, len(widgets))
	for k, v := range widgets {
		entries = append(entries, widgetEntry{key: k, lines: copyStrings(v)})
	}
	w.SetWidgetEntries(entries)
}

func (w *widgetContainer) SetWidgetEntries(entries []widgetEntry) {
	if len(entries) == 0 {
		w.widgets = nil
		w.entries = nil
		return
	}
	w.widgets = make(map[string][]string, len(entries))
	w.entries = make([]widgetEntry, 0, len(entries))
	for _, e := range entries {
		if e.key == "" {
			continue
		}
		lines := copyStrings(e.lines)
		w.widgets[e.key] = lines
		w.entries = append(w.entries, widgetEntry{key: e.key, lines: lines})
	}
	if len(w.entries) == 0 {
		w.widgets = nil
	}
}

func (w *widgetContainer) Widgets() map[string][]string {
	out := make(map[string][]string, len(w.widgets))
	for k, v := range w.widgets {
		out[k] = copyStrings(v)
	}
	return out
}

// RenderPluginWidgetRows returns width-safe rows matching the live widget body
// rendering: one-cell left padding, insertion order supplied by the caller's
// map iteration, and the spec truncation marker. Prefer live widgetContainer for
// deterministic insertion order.
func RenderPluginWidgetRows(widgets map[string][]string, width int) []string {
	if width <= 0 || len(widgets) == 0 {
		return nil
	}
	out := []string{}
	for _, lines := range widgets {
		out = append(out, renderWidgetLines(lines, width)...)
	}
	return out
}

func (w *widgetContainer) ID() tauui.ID     { return w.id }
func (w *widgetContainer) Rect() tauui.Rect { return w.rect }
func (w *widgetContainer) Layout(r tauui.Rect) {
	w.rect = r
}
func (w *widgetContainer) Focusable() bool { return false }
func (w *widgetContainer) Handle(_ tauui.Event, _ *tauui.Ctx) bool {
	return false
}

func (w *widgetContainer) MinSize() tauui.Size {
	rows := 0
	if w.placement == widgetPlacementAbove && len(w.entries) > 0 {
		rows = 1
	}
	maxW := 1
	for _, e := range w.entries {
		height := len(e.lines)
		if height > MaxWidgetLines {
			height = MaxWidgetLines + 1
		}
		rows += height
		for _, line := range e.lines {
			if width := tauui.TextWidth(line) + 1; width > maxW {
				maxW = width
			}
		}
	}
	if rows == 0 {
		return tauui.Size{}
	}
	if maxW > 200 {
		maxW = 200
	}
	return tauui.Size{W: maxW, H: rows}
}

func (w *widgetContainer) PreferredSize() tauui.Size { return w.MinSize() }

func (w *widgetContainer) Paint(d tauui.Drawer, ctx *tauui.Ctx) {
	if w == nil || w.rect.W <= 0 || w.rect.H <= 0 || ctx == nil {
		return
	}
	surfSt := ctx.Theme.Palette.Surface
	if surfSt == (tauui.Style{}) {
		surfSt = ctx.Theme.Base
	}
	mutedSt := ctx.Theme.Palette.SurfaceMuted
	if mutedSt == (tauui.Style{}) {
		mutedSt = surfSt
	}

	rowY := w.rect.Y
	if w.placement == widgetPlacementAbove && len(w.entries) > 0 {
		d.FillRect(tauui.Rect{X: w.rect.X, Y: rowY, W: w.rect.W, H: 1}, surfSt)
		rowY++
	}
	for _, e := range w.entries {
		for _, row := range renderWidgetLines(e.lines, w.rect.W) {
			if rowY >= w.rect.Y+w.rect.H {
				return
			}
			d.FillRect(tauui.Rect{X: w.rect.X, Y: rowY, W: w.rect.W, H: 1}, surfSt)
			d.DrawText(tauui.Point{X: w.rect.X, Y: rowY}, row, mutedSt)
			rowY++
		}
	}
}

func renderWidgetLines(lines []string, width int) []string {
	show := len(lines)
	truncated := show > MaxWidgetLines
	if truncated {
		show = MaxWidgetLines
	}
	out := make([]string, 0, show+1)
	for i := 0; i < show; i++ {
		out = append(out, tauui.TextTruncate(" "+lines[i], width, false))
	}
	if truncated {
		out = append(out, tauui.TextTruncate(" ... (widget truncated)", width, false))
	}
	return out
}
