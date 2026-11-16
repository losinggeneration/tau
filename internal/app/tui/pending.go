package tui

import (
	"strings"

	tauui "github.com/losinggeneration/tau/tui"
)

// pendingView renders queued steering/follow-up messages above the editor.
type pendingView struct {
	id       tauui.ID
	rect     tauui.Rect
	steering []string
	followUp []string
	hint     string
	maxRows  int
}

func NewPendingView() *pendingView {
	return &pendingView{id: tauui.NewID(), hint: "to edit all queued messages", maxRows: 6}
}

func (p *pendingView) SetItems(items []string) { p.SetQueues(nil, items) }

func (p *pendingView) SetQueues(steering, followUp []string) {
	p.steering = copyStrings(steering)
	p.followUp = copyStrings(followUp)
}

func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func (p *pendingView) Items() []string { return copyStrings(p.followUp) }

func (p *pendingView) ID() tauui.ID     { return p.id }
func (p *pendingView) Rect() tauui.Rect { return p.rect }
func (p *pendingView) Layout(r tauui.Rect) {
	p.rect = r
}
func (p *pendingView) Focusable() bool                         { return false }
func (p *pendingView) Handle(_ tauui.Event, _ *tauui.Ctx) bool { return false }

func (p *pendingView) rows(width int) []string {
	if len(p.steering)+len(p.followUp) == 0 {
		return nil
	}
	rows := []string{""}
	for _, s := range p.steering {
		rows = append(rows, " Steering: "+firstLineOf(s))
	}
	for _, s := range p.followUp {
		rows = append(rows, " Follow-up: "+firstLineOf(s))
	}
	rows = append(rows, " ↳ "+p.hint)
	if p.maxRows > 0 && len(rows) > p.maxRows {
		rows = rows[:p.maxRows]
		rows[len(rows)-1] = " ... (pending truncated)"
	}
	for i, row := range rows {
		rows[i] = tauui.TextTruncate(row, width, false)
	}
	return rows
}

func (p *pendingView) MinSize() tauui.Size {
	rows := p.rows(200)
	if len(rows) == 0 {
		return tauui.Size{}
	}
	maxW := 30
	for _, row := range rows {
		if w := tauui.TextWidth(row); w > maxW {
			maxW = w
		}
	}
	return tauui.Size{W: maxW, H: len(rows)}
}

func (p *pendingView) PreferredSize() tauui.Size { return p.MinSize() }

func (p *pendingView) Paint(d tauui.Drawer, ctx *tauui.Ctx) {
	if p == nil || p.rect.W <= 0 || p.rect.H <= 0 || ctx == nil {
		return
	}
	rows := p.rows(p.rect.W)
	if len(rows) == 0 {
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
	for i := 0; i < p.rect.H && i < len(rows); i++ {
		y := p.rect.Y + i
		d.FillRect(tauui.Rect{X: p.rect.X, Y: y, W: p.rect.W, H: 1}, surfSt)
		d.DrawText(tauui.Point{X: p.rect.X, Y: y}, rows[i], mutedSt)
	}
}

func firstLineOf(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimRight(s, " \t\r")
}
