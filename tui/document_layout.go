package tui

// DocumentLayout composes the eight interactive application regions as a single
// vertical terminal document rather than a fixed shell.
//
// The difference from AppLayout is where the slack goes. AppLayout pins the
// header to the top and the editor/footer to the bottom, giving all remaining
// space to a chat region that scrolls inside its own viewport. DocumentLayout
// gives every region its natural height, concatenates them into one tall
// document, and shows a window onto it — anchored at the bottom, so the editor
// and footer stay visible while everything above scrolls off the top exactly as
// terminal scrollback does.
//
// Consequences worth knowing before choosing between them:
//
//   - The header scrolls away once the transcript is long enough, instead of
//     occupying rows forever or having to hide itself.
//   - When the document is shorter than the terminal, content sits at the top
//     and the editor floats directly beneath it rather than being pinned to the
//     bottom edge.
//   - There is one scroll position for the whole document, not a separate one
//     for the chat.
//
// Regions are painted clipped to the viewport, so a region straddling the top
// edge renders its visible tail rather than being dropped.
type DocumentLayout struct {
	id   ID
	rect Rect

	Header       View
	Chat         View
	Pending      View
	Status       View
	WidgetsAbove View
	Editor       View
	WidgetsBelow View
	Footer       View

	// scrollBack is how many rows above the bottom the viewport is pinned.
	// Zero keeps the document anchored at the bottom, which is the normal
	// state; scrolling up raises it.
	scrollBack int

	// contentHeight is the last measured document height, kept so scrolling
	// can be clamped without re-measuring every region.
	contentHeight int
}

// NewDocumentLayout returns a DocumentLayout over the canonical regions. Nil
// regions are skipped, matching AppLayout.
func NewDocumentLayout(opts AppLayoutOpts) *DocumentLayout {
	return &DocumentLayout{
		id:           NewID(),
		Header:       opts.Header,
		Chat:         opts.Chat,
		Pending:      opts.Pending,
		Status:       opts.Status,
		WidgetsAbove: opts.WidgetsAbove,
		Editor:       opts.Editor,
		WidgetsBelow: opts.WidgetsBelow,
		Footer:       opts.Footer,
	}
}

// regions returns the non-nil regions in document order.
func (d *DocumentLayout) regions() []View {
	all := []View{
		d.Header, d.Chat, d.Pending, d.Status,
		d.WidgetsAbove, d.Editor, d.WidgetsBelow, d.Footer,
	}
	out := make([]View, 0, len(all))
	for _, v := range all {
		if v != nil {
			out = append(out, v)
		}
	}
	return out
}

func (d *DocumentLayout) ID() ID     { return d.id }
func (d *DocumentLayout) Rect() Rect { return d.rect }

// MinSize reports the terminal-sized window, not the document height. The
// document is deliberately allowed to exceed the viewport — that is the whole
// point — so reporting its true height would make an enclosing layout try to
// grow to fit it.
func (d *DocumentLayout) MinSize() Size { return Size{W: 1, H: 1} }

// PreferredSize matches MinSize for the same reason.
func (d *DocumentLayout) PreferredSize() Size { return d.MinSize() }

func (d *DocumentLayout) Focusable() bool { return false }

// documentWheelLines is how far one wheel notch moves the document, matching
// the fixed shell's ScrollView so the two models scroll at the same rate.
const documentWheelLines = 3

// Handle drives the document's scroll position.
//
// The fixed shell gets this for free: its chat sits in a ScrollView that owns
// wheel and page keys. The document has no such wrapper — it *is* the scrolling
// surface — so it has to handle them itself. Without this the scroll API
// existed but nothing could reach it, and the document was frozen at its
// bottom.
func (d *DocumentLayout) Handle(e Event, ctx *Ctx) bool {
	switch ev := e.(type) {
	case MouseEvent:
		lines := ev.ScrollLines()
		switch ev.Button {
		case MouseButtonWheelUp:
			return d.scroll(ctx, lines)
		case MouseButtonWheelDown:
			return d.scroll(ctx, -lines)
		}
	case KeyEvent:
		// Page keys only; line-wise arrows belong to the editor, which is
		// focused, and stealing them would break cursor movement.
		page := max(d.rect.H-1, 1)
		switch ev.Key {
		case KeyPageUp:
			return d.scroll(ctx, page)
		case KeyPageDown:
			return d.scroll(ctx, -page)
		}
	}
	return false
}

// scroll moves the viewport and repaints, reporting whether anything moved.
// Returning false when already at an end lets the event fall through instead of
// being silently swallowed at the edges.
func (d *DocumentLayout) scroll(ctx *Ctx, delta int) bool {
	before := d.scrollBack
	d.ScrollBy(delta)
	if d.scrollBack == before {
		return false
	}
	if ctx != nil {
		if ctx.InvalidateLayout != nil {
			ctx.InvalidateLayout()
		}
		if ctx.Invalidate != nil && d.rect.W > 0 && d.rect.H > 0 {
			ctx.Invalidate(d.rect)
		}
	}
	return true
}

// ContentHeight returns the height of the whole document as last measured.
func (d *DocumentLayout) ContentHeight() int { return d.contentHeight }

// ScrollBack returns how many rows above the bottom the viewport sits.
func (d *DocumentLayout) ScrollBack() int { return d.scrollBack }

// ScrollBy moves the viewport by delta rows, positive scrolling up into
// history. The result is clamped so the viewport can neither pass the top of
// the document nor drift below its bottom.
func (d *DocumentLayout) ScrollBy(delta int) {
	d.SetScrollBack(d.scrollBack + delta)
}

// SetScrollBack pins the viewport rows above the bottom, clamped to the
// document.
func (d *DocumentLayout) SetScrollBack(rows int) {
	maxBack := max(d.contentHeight-d.rect.H, 0)
	d.scrollBack = min(max(rows, 0), maxBack)
}

// AtBottom reports whether the viewport is pinned to the end of the document.
// Callers use it to decide whether new content should keep the view anchored.
func (d *DocumentLayout) AtBottom() bool { return d.scrollBack == 0 }

// Layout measures every region at the full width and positions it in document
// order, shifting the whole run up so the bottom of the document lands on the
// bottom of the viewport.
//
// Regions keep their natural height here. The chat region in particular must be
// the raw log rather than a scroll view: a viewport-sized chat would report the
// terminal height and the document could never grow past one screen.
func (d *DocumentLayout) Layout(r Rect) {
	d.rect = r
	regions := d.regions()
	if r.W <= 0 || r.H <= 0 || len(regions) == 0 {
		d.contentHeight = 0
		return
	}

	heights := make([]int, len(regions))
	total := 0
	for i, v := range regions {
		h := max(v.MinSize().H, 0)
		heights[i] = h
		total += h
	}
	d.contentHeight = total

	// Re-clamp: the document may have shrunk since the last scroll.
	d.SetScrollBack(d.scrollBack)

	// offset is how many rows of the document sit above the viewport. A
	// document shorter than the viewport has none, and flows from the top —
	// the editor sits under the content rather than at the screen's bottom
	// edge, which is the visible difference from the fixed shell.
	offset := 0
	if total > r.H {
		offset = total - r.H - d.scrollBack
	}

	y := r.Y - offset
	for i, v := range regions {
		v.Layout(Rect{X: r.X, Y: y, W: r.W, H: heights[i]})
		y += heights[i]
	}
}

// Paint draws each region clipped to the viewport, skipping those scrolled
// entirely out of view.
func (d *DocumentLayout) Paint(dr Drawer, ctx *Ctx) {
	if d.rect.W <= 0 || d.rect.H <= 0 {
		return
	}
	viewportBottom := d.rect.Y + d.rect.H
	for _, v := range d.regions() {
		rr := v.Rect()
		if rr.H <= 0 || rr.Y >= viewportBottom || rr.Y+rr.H <= d.rect.Y {
			continue
		}
		// Clip rather than skip: a region straddling the top edge must render
		// its visible tail, which is the normal state for the chat log.
		dr.WithClip(d.rect, func(dr Drawer) {
			v.Paint(dr, ctx)
		})
	}
}

// Children exposes the regions for focus traversal and hit testing.
func (d *DocumentLayout) Children() []View { return d.regions() }
