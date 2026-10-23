package tui

import (
	"fmt"
	"sort"
)

// StatusEntriesView renders keyed transient status entries. Empty entry maps
// collapse to 0x0 so app layouts can include it unconditionally.
type StatusEntriesView struct {
	id      ID
	rect    Rect
	entries map[string]string
	maxRows int
}

func NewStatusEntriesView(maxRows int) *StatusEntriesView {
	if maxRows <= 0 {
		maxRows = 5
	}
	return &StatusEntriesView{id: NewID(), maxRows: maxRows}
}

func (s *StatusEntriesView) SetEntries(entries map[string]string) {
	if len(entries) == 0 {
		s.entries = nil
		return
	}
	out := make(map[string]string, len(entries))
	for k, v := range entries {
		out[k] = v
	}
	s.entries = out
}

func (s *StatusEntriesView) Entries() map[string]string {
	out := make(map[string]string, len(s.entries))
	for k, v := range s.entries {
		out[k] = v
	}
	return out
}

func (s *StatusEntriesView) MaxRows() int {
	if s == nil {
		return 0
	}
	return s.maxRows
}

func (s *StatusEntriesView) ID() ID                  { return s.id }
func (s *StatusEntriesView) Rect() Rect              { return s.rect }
func (s *StatusEntriesView) Layout(r Rect)           { s.rect = r }
func (s *StatusEntriesView) Focusable() bool         { return false }
func (s *StatusEntriesView) Handle(Event, *Ctx) bool { return false }
func (s *StatusEntriesView) PreferredSize() Size     { return s.MinSize() }

func (s *StatusEntriesView) MinSize() Size {
	if len(s.entries) == 0 {
		return Size{}
	}
	rows := len(s.entries)
	if rows > s.maxRows {
		rows = s.maxRows
	}
	maxW := 16
	for k, v := range s.entries {
		if w := TextWidth(k+": "+v) + 2; w > maxW {
			maxW = w
		}
	}
	if maxW > 200 {
		maxW = 200
	}
	return Size{W: maxW, H: rows}
}

func (s *StatusEntriesView) Paint(d Drawer, ctx *Ctx) {
	if s == nil || len(s.entries) == 0 || ctx == nil {
		return
	}
	r := s.rect
	if r.W <= 0 || r.H <= 0 {
		return
	}

	baseSt := ctx.Theme.Base
	if baseSt == (Style{}) {
		baseSt = ctx.Theme.Palette.Surface
	}
	mutedSt := ctx.Theme.Palette.TextMuted
	if mutedSt == (Style{}) {
		mutedSt = baseSt
	}
	mutedSt.BG = baseSt.BG
	accentSt := ctx.Theme.Palette.Accent
	if accentSt == (Style{}) {
		accentSt = baseSt
	}
	accentSt.BG = baseSt.BG

	keys := make([]string, 0, len(s.entries))
	for k := range s.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	rows := len(keys)
	if rows > r.H {
		rows = r.H
	}
	more := len(keys) - rows
	for i := range rows {
		rowY := r.Y + i
		if rowY >= r.Y+r.H {
			break
		}
		d.FillRect(Rect{X: r.X, Y: rowY, W: r.W, H: 1}, baseSt)
		key := keys[i]
		value := s.entries[key]
		keyStr := key + ":"
		d.DrawText(Point{X: r.X, Y: rowY}, TextTruncate(keyStr, r.W, false), accentSt)
		keyW := TextWidth(keyStr) + 1
		if keyW >= r.W {
			continue
		}
		textAvail := r.W - keyW
		if textAvail > 0 {
			d.DrawText(Point{X: r.X + keyW, Y: rowY}, TextTruncate(" "+value, textAvail, false), mutedSt)
		}
	}
	if more > 0 && rows < r.H {
		rowY := r.Y + rows
		d.FillRect(Rect{X: r.X, Y: rowY, W: r.W, H: 1}, baseSt)
		d.DrawText(Point{X: r.X, Y: rowY}, TextTruncate(fmt.Sprintf("… (+%d more)", more), r.W, false), mutedSt)
	}
}
