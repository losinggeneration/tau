package tui

import (
	"io"
	"sync"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/backend"
	"github.com/losinggeneration/rovel/geom"
	roverlay "github.com/losinggeneration/rovel/ui/overlay"
)

// Placement determines how an overlay is positioned relative to the screen.
type Placement = rovel.Placement

// OverlayOpts configures an overlay shown above the root view.
type OverlayOpts struct {
	Root      View
	Modal     bool
	Place     Placement
	OnDismiss func()

	// MaxHeight clamps the overlay placement height before layout.
	MaxHeight int

	// Visible, when set, controls dynamic overlay visibility. Hidden dynamic
	// overlays are dismissed from the underlying stack and restored when visible
	// again via SetHidden(false).
	Visible func() bool

	// Margin clamps custom placement away from screen edges by this many cells.
	Margin int

	// Offset adjusts resolved placement by cells before margin clamping.
	Offset geom.Point

	// NonCapturing renders the overlay without moving wrapper focus to it. In
	// the underlying backend this maps to a non-modal overlay.
	NonCapturing bool
}

// Overlay is a handle for an active overlay.
//
// ShowOverlay returns the handle immediately and the app loop populates it
// later, so inner/id/hidden are written on the loop goroutine and read from
// whichever goroutine holds the handle. That cross-goroutine hand-off is the
// documented usage, so the mutable trio is guarded by mu. opts and app are set
// before the handle escapes its constructor and are never reassigned, so they
// need no lock.
type Overlay struct {
	app  *App
	opts OverlayOpts

	mu     sync.Mutex
	inner  *rovel.Overlay
	id     ID
	hidden bool
}

// setActive records the overlay the app loop just created for this handle.
func (o *Overlay) setActive(inner *rovel.Overlay) {
	o.mu.Lock()
	o.inner = inner
	o.id = inner.ID()
	o.hidden = false
	o.mu.Unlock()
}

// setDismissed records that the overlay is no longer on the rovel stack while
// retaining enough state to show it again.
func (o *Overlay) setDismissed() {
	o.mu.Lock()
	o.hidden = true
	o.inner = nil
	o.mu.Unlock()
}

// snapshot reads the loop-populated state under the lock.
func (o *Overlay) snapshot() (inner *rovel.Overlay, id ID, hidden bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.inner, o.id, o.hidden
}

// ID returns the overlay id.
func (o *Overlay) ID() ID {
	if o == nil {
		return 0
	}
	inner, id, _ := o.snapshot()
	if inner != nil {
		return inner.ID()
	}

	return id
}

// Root returns the overlay root view.
func (o *Overlay) Root() View {
	if o == nil {
		return nil
	}
	if inner, _, _ := o.snapshot(); inner != nil {
		return inner.Root()
	}

	return o.opts.Root
}

// Modal reports whether the overlay captures focus/input.
func (o *Overlay) Modal() bool {
	if o == nil {
		return false
	}
	if inner, _, _ := o.snapshot(); inner != nil {
		return inner.Modal()
	}

	return o.opts.Modal && !o.opts.NonCapturing
}

// Rect returns the overlay's most recent layout rect.
func (o *Overlay) Rect() Rect {
	if o == nil {
		return Rect{}
	}
	inner, _, _ := o.snapshot()
	if inner == nil {
		return Rect{}
	}

	r := inner.Rect()
	if o.opts.MaxHeight > 0 && r.H > o.opts.MaxHeight {
		r.H = o.opts.MaxHeight
	}
	return r
}

// Hide dismisses this overlay.
func (o *Overlay) Hide() {
	if o == nil || o.app == nil {
		return
	}

	o.SetHidden(true)
}

// SetHidden hides or shows the overlay. Hidden overlays are dismissed from the
// underlying rovel stack but retain enough wrapper state to be shown again.
func (o *Overlay) SetHidden(hidden bool) {
	if o == nil || o.app == nil {
		return
	}

	if hidden || (o.opts.Visible != nil && !o.opts.Visible()) {
		id := o.ID()
		_ = o.app.Post(func(ctx *UpdateCtx) {
			if id != 0 {
				ctx.DismissOverlayByID(id)
			}
			o.setDismissed()
			o.app.forgetOverlay(id)
		})

		return
	}

	o.opts.Place = o.resolvedPlacement()
	_ = o.app.Post(func(ctx *UpdateCtx) {
		inner := ctx.ShowOverlay(rovel.OverlayOpts{
			Root:      o.opts.Root,
			Modal:     o.opts.Modal && !o.opts.NonCapturing,
			Place:     o.opts.Place,
			OnDismiss: o.opts.OnDismiss,
		})
		o.setActive(inner)
		o.app.rememberOverlay(o)
	})
}

// Hidden reports whether the handle no longer refers to an active overlay.
func (o *Overlay) Hidden() bool {
	if o == nil || o.app == nil {
		return true
	}

	_, _, hidden := o.snapshot()

	return hidden || !o.app.HasOverlay(o.ID())
}

// Focus requests focus for the overlay root.
func (o *Overlay) Focus() {
	if o == nil || o.app == nil || o.Root() == nil {
		return
	}

	o.app.SetFocus(o.Root().ID())
}

// Unfocus clears focus from the overlay root if it currently owns focus.
func (o *Overlay) Unfocus() {
	if o == nil || o.app == nil {
		return
	}

	o.app.SetFocus(0)
}

func (o *Overlay) Focused() bool {
	if o == nil || o.Root() == nil || o.app == nil {
		return false
	}

	return o.app.IsFocused(o.Root().ID())
}

// ShowOverlay schedules an overlay to be displayed and returns a handle that
// becomes populated when the app loop processes the request.
func (a *App) ShowOverlay(opts OverlayOpts) *Overlay {
	h := &Overlay{app: a, opts: opts}
	if h.opts.Visible != nil && !h.opts.Visible() {
		h.hidden = true
		return h
	}
	h.opts.Place = h.resolvedPlacement()

	_ = a.Post(func(ctx *UpdateCtx) {
		inner := ctx.ShowOverlay(rovel.OverlayOpts{
			Root:      h.opts.Root,
			Modal:     h.opts.Modal && !h.opts.NonCapturing,
			Place:     h.opts.Place,
			OnDismiss: h.opts.OnDismiss,
		})
		h.setActive(inner)
		a.rememberOverlay(h)
	})

	return h
}

// HideOverlay dismisses an overlay by id. A zero id dismisses the top overlay.
func (a *App) HideOverlay(id ID) {
	_ = a.Post(func(ctx *UpdateCtx) {
		if id == 0 {
			dismissed := ctx.DismissOverlay()
			if dismissed != nil {
				a.forgetOverlay(dismissed.ID())
			}

			return
		}

		ctx.DismissOverlayByID(id)
		if overlay, ok := a.overlayByID(id); ok {
			overlay.setDismissed()
		}
		a.forgetOverlay(id)
	})
}

// HasOverlay reports whether the wrapper knows about an active overlay.
func (a *App) HasOverlay(id ID) bool {
	a.overlayMu.Lock()
	defer a.overlayMu.Unlock()

	if id == 0 {
		return len(a.overlays) > 0
	}

	_, ok := a.overlays[id]

	return ok
}

// SetFocus schedules focus to move to the view id.
func (a *App) SetFocus(id ID) {
	a.mu.Lock()
	a.focusedID = id
	a.mu.Unlock()

	_ = a.Post(func(ctx *UpdateCtx) {
		ctx.RequestFocus(id)
	})
}

// RequestRender schedules a render. A forced render invalidates the full app.
func (a *App) RequestRender(force bool) error {
	if force {
		return a.PostInvalidateAll()
	}

	return a.Post(func(*UpdateCtx) {})
}

func (a *App) rememberOverlay(o *Overlay) {
	if o == nil || o.ID() == 0 {
		return
	}

	a.overlayMu.Lock()
	defer a.overlayMu.Unlock()
	a.overlays[o.ID()] = o
}

func (a *App) overlayByID(id ID) (*Overlay, bool) {
	a.overlayMu.Lock()
	defer a.overlayMu.Unlock()

	o, ok := a.overlays[id]

	return o, ok
}

func (a *App) forgetOverlay(id ID) {
	a.overlayMu.Lock()
	defer a.overlayMu.Unlock()
	delete(a.overlays, id)
}

func (o *Overlay) resolvedPlacement() Placement {
	place := o.opts.Place
	if place == nil {
		place = roverlay.Centered{}
	}
	if o.opts.MaxHeight <= 0 && o.opts.Margin <= 0 && o.opts.Offset == (geom.Point{}) {
		return place
	}
	return placementWrapper{base: place, maxHeight: o.opts.MaxHeight, margin: o.opts.Margin, offset: o.opts.Offset}
}

type placementWrapper struct {
	base      Placement
	maxHeight int
	margin    int
	offset    geom.Point
}

func (p placementWrapper) Resolve(root View, screen geom.Size) geom.Rect {
	r := p.base.Resolve(root, screen)
	if p.maxHeight > 0 && r.H > p.maxHeight {
		r.H = p.maxHeight
	}
	r.X += p.offset.X
	r.Y += p.offset.Y
	margin := p.margin
	if margin < 0 {
		margin = 0
	}
	if margin*2 > screen.W {
		margin = screen.W / 2
	}
	if r.W > screen.W-margin*2 {
		r.W = screen.W - margin*2
	}
	if r.X < margin {
		r.X = margin
	}
	if r.X+r.W > screen.W-margin {
		r.X = screen.W - margin - r.W
	}
	if margin*2 > screen.H {
		margin = screen.H / 2
	}
	if r.H > screen.H-margin*2 {
		r.H = screen.H - margin*2
	}
	if r.Y < margin {
		r.Y = margin
	}
	if r.Y+r.H > screen.H-margin {
		r.Y = screen.H - margin - r.H
	}
	if r.X < 0 {
		r.X = 0
	}
	if r.Y < 0 {
		r.Y = 0
	}
	return r
}

// AnchorName identifies Pi-style overlay anchor locations.
type AnchorName string

const (
	AnchorCenter      AnchorName = "center"
	AnchorTopLeft     AnchorName = "top-left"
	AnchorTop         AnchorName = "top"
	AnchorTopRight    AnchorName = "top-right"
	AnchorLeft        AnchorName = "left"
	AnchorRight       AnchorName = "right"
	AnchorBottomLeft  AnchorName = "bottom-left"
	AnchorBottom      AnchorName = "bottom"
	AnchorBottomRight AnchorName = "bottom-right"
)

// EdgeAnchored places overlays at the common Pi anchor points with optional
// fixed dimensions before margin/offset wrapping.
type EdgeAnchored struct {
	Anchor AnchorName
	W      int
	H      int
	Width  SizeValue
	Height SizeValue
	MinW   int
	MinH   int
}

type SizeValue struct {
	Cells   int
	Percent int
}

func Cells(n int) SizeValue   { return SizeValue{Cells: n} }
func Percent(n int) SizeValue { return SizeValue{Percent: n} }

func (p EdgeAnchored) Resolve(root View, screen geom.Size) geom.Rect {
	sz := root.MinSize()
	if p.W > 0 {
		sz.W = p.W
	}
	if p.H > 0 {
		sz.H = p.H
	}
	if w := p.Width.Resolve(screen.W); w > 0 {
		sz.W = w
	}
	if h := p.Height.Resolve(screen.H); h > 0 {
		sz.H = h
	}
	if p.MinW > 0 && sz.W < p.MinW {
		sz.W = p.MinW
	}
	if p.MinH > 0 && sz.H < p.MinH {
		sz.H = p.MinH
	}
	if sz.W <= 0 {
		sz.W = min(80, screen.W)
	}
	if sz.W > screen.W {
		sz.W = screen.W
	}
	if sz.H > screen.H {
		sz.H = screen.H
	}
	x := (screen.W - sz.W) / 2
	y := (screen.H - sz.H) / 2
	switch p.Anchor {
	case AnchorTopLeft:
		x, y = 0, 0
	case AnchorTop:
		y = 0
	case AnchorTopRight:
		x, y = screen.W-sz.W, 0
	case AnchorLeft:
		x = 0
	case AnchorRight:
		x = screen.W - sz.W
	case AnchorBottomLeft:
		x, y = 0, screen.H-sz.H
	case AnchorBottom:
		y = screen.H - sz.H
	case AnchorBottomRight:
		x, y = screen.W-sz.W, screen.H-sz.H
	}
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return geom.Rect{X: x, Y: y, W: sz.W, H: sz.H}
}

func (v SizeValue) Resolve(total int) int {
	if v.Cells > 0 {
		return v.Cells
	}
	if v.Percent <= 0 || total <= 0 {
		return 0
	}
	if v.Percent > 100 {
		v.Percent = 100
	}
	return total * v.Percent / 100
}

// HasDynamicVisibility reports whether this overlay is controlled by a
// visibility predicate.
func (o *Overlay) HasDynamicVisibility() bool { return o != nil && o.opts.Visible != nil }

// RefreshVisibility reevaluates a dynamic visibility predicate and hides or
// shows the overlay accordingly.
func (o *Overlay) RefreshVisibility() {
	if o == nil || o.opts.Visible == nil {
		return
	}
	o.SetHidden(!o.opts.Visible())
}

// RenderOverlayRows defensively truncates overlay-rendered rows to width while
// preserving terminal image protocol rows.
func RenderOverlayRows(root RowRenderer, width int) []string {
	if root == nil || width <= 0 {
		return nil
	}
	rows := root.Render(width)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if IsImageLine(row) {
			out = append(out, row)
			continue
		}
		out = append(out, Truncate(row, width, false))
	}
	return out
}

// stdoutBackend mirrors all ANSI writes to stdout while delegating lifecycle
// and input to the wrapped backend.
type stdoutBackend struct {
	backend.Backend
	stdout io.Writer
	mu     sync.Mutex
}

var (
	_ backend.Backend             = (*stdoutBackend)(nil)
	_ backend.ANSITransport       = (*stdoutBackend)(nil)
	_ backend.CapabilityReporter  = (*stdoutBackend)(nil)
	_ backend.InputFeatureEnabler = (*stdoutBackend)(nil)
)

func (b *stdoutBackend) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if transport, ok := b.Backend.(backend.ANSITransport); ok {
		_, err := transport.Write(p)
		if err != nil {
			return 0, err
		}
	}

	if b.stdout == nil {
		return len(p), nil
	}

	return b.stdout.Write(p)
}

func (b *stdoutBackend) Flush() error {
	if transport, ok := b.Backend.(backend.ANSITransport); ok {
		return transport.Flush()
	}

	return nil
}

func (b *stdoutBackend) InputCapabilities() backend.InputCapabilities {
	if reporter, ok := b.Backend.(backend.CapabilityReporter); ok {
		return reporter.InputCapabilities()
	}

	return backend.InputCapabilities{}
}

func (b *stdoutBackend) SetInputFeatures(f backend.InputFeatures) error {
	if enabler, ok := b.Backend.(backend.InputFeatureEnabler); ok {
		return enabler.SetInputFeatures(f)
	}

	return nil
}

var _ = geom.Rect{}
