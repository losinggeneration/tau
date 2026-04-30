package tui

// FixedLineRender renders one terminal row for FixedLineView. The returned style
// is used both to fill the row and draw the text.
type FixedLineRender func(width int) (string, Style)

// FixedLineView is a one-row component whose content is produced by a callback.
// It is useful for app-specific header/footer/status chrome that should render
// through the top-level toolkit instead of reimplementing retained view plumbing.
type FixedLineView struct {
	id     ID
	rect   Rect
	render FixedLineRender
}

func NewFixedLineView(render FixedLineRender) *FixedLineView {
	return &FixedLineView{id: NewID(), render: render}
}

func (v *FixedLineView) SetRender(render FixedLineRender) { v.render = render }
func (v *FixedLineView) Render(width int) (string, Style) {
	if v == nil || v.render == nil {
		return "", Style{}
	}
	return v.render(width)
}
func (v *FixedLineView) ID() ID                  { return v.id }
func (v *FixedLineView) Rect() Rect              { return v.rect }
func (v *FixedLineView) Layout(r Rect)           { v.rect = r }
func (v *FixedLineView) MinSize() Size           { return Size{W: 1, H: 1} }
func (v *FixedLineView) PreferredSize() Size     { return v.MinSize() }
func (v *FixedLineView) Focusable() bool         { return false }
func (v *FixedLineView) Handle(Event, *Ctx) bool { return false }

func (v *FixedLineView) Paint(d Drawer, ctx *Ctx) {
	if v == nil || v.rect.W <= 0 || v.rect.H <= 0 || v.render == nil {
		return
	}
	s, st := v.render(v.rect.W)
	d.FillRect(v.rect, st)
	d.DrawText(Point{X: v.rect.X, Y: v.rect.Y}, s, st)
	_ = ctx
}
