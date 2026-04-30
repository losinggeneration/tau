package tui

import "testing"

// fixedHeightView is a region of a known height that records where it was
// placed, so layout assertions read as coordinates rather than pixels.
type fixedHeightView struct {
	id   ID
	rect Rect
	h    int
}

func newFixedHeightView(h int) *fixedHeightView {
	return &fixedHeightView{id: NewID(), h: h}
}

func (v *fixedHeightView) ID() ID                  { return v.id }
func (v *fixedHeightView) Rect() Rect              { return v.rect }
func (v *fixedHeightView) Layout(r Rect)           { v.rect = r }
func (v *fixedHeightView) MinSize() Size           { return Size{W: 1, H: v.h} }
func (v *fixedHeightView) PreferredSize() Size     { return v.MinSize() }
func (v *fixedHeightView) Focusable() bool         { return false }
func (v *fixedHeightView) Handle(Event, *Ctx) bool { return false }
func (v *fixedHeightView) Paint(Drawer, *Ctx)      {}

// A document shorter than the terminal flows from the top: content sits at the
// top edge and the editor floats directly beneath it. This is the visible
// difference from the fixed shell, which pins the editor to the bottom.
func TestDocumentLayoutShortDocumentFlowsFromTheTop(t *testing.T) {
	header := newFixedHeightView(3)
	chat := newFixedHeightView(2)
	editor := newFixedHeightView(3)
	footer := newFixedHeightView(2)

	d := NewDocumentLayout(AppLayoutOpts{Header: header, Chat: chat, Editor: editor, Footer: footer})
	d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})

	if got := d.ContentHeight(); got != 10 {
		t.Fatalf("content height = %d, want 10", got)
	}
	if header.rect.Y != 0 {
		t.Errorf("header Y = %d, want 0 (document starts at the top)", header.rect.Y)
	}
	if chat.rect.Y != 3 {
		t.Errorf("chat Y = %d, want 3", chat.rect.Y)
	}
	if editor.rect.Y != 5 {
		t.Errorf("editor Y = %d, want 5 (directly under the content, not at the bottom edge)", editor.rect.Y)
	}
	if footer.rect.Y != 8 {
		t.Errorf("footer Y = %d, want 8", footer.rect.Y)
	}
}

// Once the document outgrows the terminal it anchors at the bottom: the footer
// ends on the last row and the header scrolls off the top.
func TestDocumentLayoutAnchorsAtTheBottomWhenItOverflows(t *testing.T) {
	header := newFixedHeightView(5)
	chat := newFixedHeightView(100)
	editor := newFixedHeightView(3)
	footer := newFixedHeightView(2)

	d := NewDocumentLayout(AppLayoutOpts{Header: header, Chat: chat, Editor: editor, Footer: footer})
	d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})

	if got := d.ContentHeight(); got != 110 {
		t.Fatalf("content height = %d, want 110", got)
	}
	if got := footer.rect.Y + footer.rect.H; got != 24 {
		t.Errorf("document bottom = %d, want it flush with the viewport bottom (24)", got)
	}
	if header.rect.Y >= 0 {
		t.Errorf("header Y = %d, want it scrolled above the viewport", header.rect.Y)
	}
	if editor.rect.Y+editor.rect.H > 24 {
		t.Errorf("editor bottom = %d, want it on screen", editor.rect.Y+editor.rect.H)
	}
}

// The editor and footer must stay visible however long the transcript grows —
// that is what makes the bottom anchor usable rather than just a scrollback.
func TestDocumentLayoutKeepsTheEditorVisibleAsChatGrows(t *testing.T) {
	chat := newFixedHeightView(0)
	editor := newFixedHeightView(3)
	footer := newFixedHeightView(2)
	d := NewDocumentLayout(AppLayoutOpts{Chat: chat, Editor: editor, Footer: footer})

	for _, lines := range []int{0, 10, 50, 500, 5000} {
		chat.h = lines
		d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})

		if editor.rect.Y < 0 || editor.rect.Y+editor.rect.H > 24 {
			t.Fatalf("chat=%d: editor at Y=%d h=%d is off screen", lines, editor.rect.Y, editor.rect.H)
		}
		if footer.rect.Y+footer.rect.H > 24 {
			t.Fatalf("chat=%d: footer bottom %d is below the viewport", lines, footer.rect.Y+footer.rect.H)
		}
	}
}

func TestDocumentLayoutScrollBackMovesTheWindow(t *testing.T) {
	chat := newFixedHeightView(100)
	footer := newFixedHeightView(2)
	d := NewDocumentLayout(AppLayoutOpts{Chat: chat, Footer: footer})
	d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})

	if !d.AtBottom() {
		t.Fatal("a fresh layout should be anchored at the bottom")
	}
	bottomY := chat.rect.Y

	d.ScrollBy(10)
	d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})
	if d.ScrollBack() != 10 {
		t.Fatalf("scroll back = %d, want 10", d.ScrollBack())
	}
	if chat.rect.Y != bottomY+10 {
		t.Errorf("chat Y = %d, want %d (scrolling up moves content down)", chat.rect.Y, bottomY+10)
	}
	if d.AtBottom() {
		t.Error("AtBottom is true after scrolling up")
	}
}

// Scrolling must not run off either end of the document.
func TestDocumentLayoutClampsScrolling(t *testing.T) {
	chat := newFixedHeightView(100)
	d := NewDocumentLayout(AppLayoutOpts{Chat: chat})
	d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})

	d.ScrollBy(-50)
	if d.ScrollBack() != 0 {
		t.Errorf("scrolling below the bottom gave %d, want 0", d.ScrollBack())
	}

	d.ScrollBy(10_000)
	if want := 100 - 24; d.ScrollBack() != want {
		t.Errorf("scrolling past the top gave %d, want %d", d.ScrollBack(), want)
	}
}

// A transcript that shrinks (a /new session, say) must not leave the viewport
// stranded above a document that no longer reaches it.
func TestDocumentLayoutReclampsWhenTheDocumentShrinks(t *testing.T) {
	chat := newFixedHeightView(200)
	d := NewDocumentLayout(AppLayoutOpts{Chat: chat})
	d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})
	d.ScrollBy(150)
	if d.ScrollBack() != 150 {
		t.Fatalf("setup: scroll back = %d, want 150", d.ScrollBack())
	}

	chat.h = 10
	d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})

	if d.ScrollBack() != 0 {
		t.Errorf("scroll back = %d after the document shrank below one screen, want 0", d.ScrollBack())
	}
	if chat.rect.Y != 0 {
		t.Errorf("chat Y = %d, want the short document back at the top", chat.rect.Y)
	}
}

// Nil regions are skipped rather than counted as zero-height entries.
func TestDocumentLayoutSkipsNilRegions(t *testing.T) {
	chat := newFixedHeightView(4)
	d := NewDocumentLayout(AppLayoutOpts{Chat: chat})
	d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})

	if got := len(d.Children()); got != 1 {
		t.Fatalf("children = %d, want only the chat region", got)
	}
	if got := d.ContentHeight(); got != 4 {
		t.Errorf("content height = %d, want 4", got)
	}
}

// A zero-sized viewport must not panic or produce nonsense offsets.
func TestDocumentLayoutHandlesADegenerateViewport(t *testing.T) {
	chat := newFixedHeightView(50)
	d := NewDocumentLayout(AppLayoutOpts{Chat: chat})

	d.Layout(Rect{X: 0, Y: 0, W: 0, H: 0})
	if got := d.ContentHeight(); got != 0 {
		t.Errorf("content height = %d for a zero viewport, want 0", got)
	}
	d.Paint(nil, nil) // must not panic
}

// The document is the scrolling surface, so it has to own the wheel itself —
// the fixed shell gets this from the ScrollView wrapping its chat.
func TestDocumentLayoutScrollsOnWheel(t *testing.T) {
	chat := newFixedHeightView(100)
	d := NewDocumentLayout(AppLayoutOpts{Chat: chat})
	d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})

	up := MouseEvent{Button: MouseButtonWheelUp, Action: MousePress}
	if !d.Handle(up, nil) {
		t.Fatal("wheel up was not handled")
	}
	if d.ScrollBack() != documentWheelLines {
		t.Errorf("scroll back = %d after one wheel notch, want %d", d.ScrollBack(), documentWheelLines)
	}

	down := MouseEvent{Button: MouseButtonWheelDown, Action: MousePress}
	if !d.Handle(down, nil) {
		t.Fatal("wheel down was not handled")
	}
	if !d.AtBottom() {
		t.Errorf("scroll back = %d after scrolling back down, want 0", d.ScrollBack())
	}
}

func TestDocumentLayoutScrollsOnPageKeys(t *testing.T) {
	chat := newFixedHeightView(200)
	d := NewDocumentLayout(AppLayoutOpts{Chat: chat})
	d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})

	if !d.Handle(KeyEvent{Key: KeyPageUp}, nil) {
		t.Fatal("page up was not handled")
	}
	if got, want := d.ScrollBack(), 23; got != want {
		t.Errorf("scroll back = %d after page up, want %d", got, want)
	}
	if !d.Handle(KeyEvent{Key: KeyPageDown}, nil) {
		t.Fatal("page down was not handled")
	}
	if !d.AtBottom() {
		t.Errorf("scroll back = %d after page down, want 0", d.ScrollBack())
	}
}

// At the ends, the event must fall through rather than be swallowed — reporting
// "handled" when nothing moved hides the key from everything else.
func TestDocumentLayoutDoesNotSwallowScrollsAtTheEnds(t *testing.T) {
	chat := newFixedHeightView(100)
	d := NewDocumentLayout(AppLayoutOpts{Chat: chat})
	d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})

	if d.Handle(MouseEvent{Button: MouseButtonWheelDown, Action: MousePress}, nil) {
		t.Error("wheel down at the bottom reported handled; nothing moved")
	}

	d.SetScrollBack(1 << 30) // clamped to the top
	if d.Handle(MouseEvent{Button: MouseButtonWheelUp, Action: MousePress}, nil) {
		t.Error("wheel up at the top reported handled; nothing moved")
	}
}

// Arrow keys belong to the focused editor; scrolling must not steal them.
func TestDocumentLayoutLeavesEditingKeysAlone(t *testing.T) {
	chat := newFixedHeightView(100)
	d := NewDocumentLayout(AppLayoutOpts{Chat: chat})
	d.Layout(Rect{X: 0, Y: 0, W: 40, H: 24})

	for _, key := range []Key{KeyUp, KeyDown, KeyLeft, KeyRight, KeyHome, KeyEnd, KeyBackspace} {
		if d.Handle(KeyEvent{Key: key}, nil) {
			t.Errorf("document layout consumed %v; it belongs to the editor", key)
		}
	}
}
