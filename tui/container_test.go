package tui

import (
	"testing"

	"github.com/losinggeneration/rovel"
)

// fakeView is a minimal View implementation for testing containers.
type fakeView struct {
	id        ID
	minSize   Size
	rect      Rect
	paintN    int
	handleN   int
	handleRet bool
}

func newFake(id ID) *fakeView {
	return &fakeView{id: id, minSize: Size{W: 1, H: 1}}
}

func (f *fakeView) ID() ID                 { return f.id }
func (f *fakeView) Rect() Rect             { return f.rect }
func (f *fakeView) MinSize() Size          { return f.minSize }
func (f *fakeView) Layout(r Rect)          { f.rect = r }
func (f *fakeView) Focusable() bool        { return false }
func (f *fakeView) Paint(d Drawer, _ *Ctx) { f.paintN++ }
func (f *fakeView) Handle(e Event, _ *Ctx) bool {
	f.handleN++
	return f.handleRet
}

func TestContainer_AddChild(t *testing.T) {
	c := NewContainer()
	a := newFake(NewID())
	b := newFake(NewID())

	c.AddChild(a)
	c.AddChild(b)

	if got := c.ChildCount(); got != 2 {
		t.Errorf("ChildCount() = %d, want 2", got)
	}

	if c.ChildAt(0) != a {
		t.Errorf("ChildAt(0) = %v, want %v", c.ChildAt(0), a)
	}

	if c.ChildAt(1) != b {
		t.Errorf("ChildAt(1) = %v, want %v", c.ChildAt(1), b)
	}
}

func TestContainer_AddChild_Nil(t *testing.T) {
	c := NewContainer()
	c.AddChild(nil) // must not panic

	if got := c.ChildCount(); got != 0 {
		t.Errorf("ChildCount() = %d, want 0", got)
	}
}

func TestContainer_RemoveChild_Known(t *testing.T) {
	c := NewContainer()
	a := newFake(NewID())
	b := newFake(NewID())
	c.AddChild(a)
	c.AddChild(b)

	c.RemoveChild(a)

	if got := c.ChildCount(); got != 1 {
		t.Errorf("ChildCount() = %d, want 1", got)
	}

	if c.ChildAt(0) != b {
		t.Errorf("ChildAt(0) = %v, want %v", c.ChildAt(0), b)
	}
}

func TestContainer_RemoveChild_Unknown(t *testing.T) {
	c := NewContainer()
	a := newFake(NewID())
	b := newFake(NewID())
	c.AddChild(a)

	c.RemoveChild(b) // unknown — must be a no-op

	if got := c.ChildCount(); got != 1 {
		t.Errorf("ChildCount() = %d, want 1", got)
	}

	if c.ChildAt(0) != a {
		t.Errorf("ChildAt(0) = %v, want %v", c.ChildAt(0), a)
	}
}

func TestContainer_RemoveChild_Nil(t *testing.T) {
	c := NewContainer()
	a := newFake(NewID())
	c.AddChild(a)

	c.RemoveChild(nil) // must not panic

	if got := c.ChildCount(); got != 1 {
		t.Errorf("ChildCount() = %d, want 1", got)
	}
}

func TestContainer_Clear(t *testing.T) {
	c := NewContainer()
	c.AddChild(newFake(NewID()))
	c.AddChild(newFake(NewID()))
	c.AddChild(newFake(NewID()))

	c.Clear()

	if got := c.ChildCount(); got != 0 {
		t.Errorf("ChildCount() = %d, want 0", got)
	}
}

func TestContainer_Children_ReturnsCopy(t *testing.T) {
	c := NewContainer()
	a := newFake(NewID())
	c.AddChild(a)

	got := c.Children()
	if len(got) != 1 || got[0] != a {
		t.Fatalf("Children() = %v, want [%v]", got, a)
	}

	// Mutating the returned slice must not affect the container.
	got[0] = nil
	if c.ChildAt(0) != a {
		t.Errorf("mutating returned slice affected container")
	}
}

func TestContainer_MinSize_Empty(t *testing.T) {
	c := NewContainer()
	got := c.MinSize()
	if got != (Size{W: 1, H: 1}) {
		t.Errorf("MinSize() empty = %v, want {1 1}", got)
	}
}

func TestContainer_MinSize_Children(t *testing.T) {
	c := NewContainer()
	a := &fakeView{id: NewID(), minSize: Size{W: 5, H: 2}}
	b := &fakeView{id: NewID(), minSize: Size{W: 3, H: 4}}
	c.AddChild(a)
	c.AddChild(b)

	// Width = max(5, 3) = 5, height = 2 + 4 = 6.
	got := c.MinSize()
	if got.W != 5 {
		t.Errorf("MinSize().W = %d, want 5", got.W)
	}

	if got.H != 6 {
		t.Errorf("MinSize().H = %d, want 6", got.H)
	}
}

func TestContainer_MinSize_WithGap(t *testing.T) {
	c := NewContainer()
	c.SetGap(nil, 1)
	a := &fakeView{id: NewID(), minSize: Size{W: 1, H: 2}}
	b := &fakeView{id: NewID(), minSize: Size{W: 1, H: 3}}
	c.AddChild(a)
	c.AddChild(b)

	// 2 + 1 (gap) + 3 = 6
	got := c.MinSize()
	if got.H != 6 {
		t.Errorf("MinSize().H = %d, want 6", got.H)
	}
}

func TestContainer_Layout_Cascades(t *testing.T) {
	c := NewContainer()
	a := &fakeView{id: NewID(), minSize: Size{W: 1, H: 1}}
	b := &fakeView{id: NewID(), minSize: Size{W: 1, H: 1}}
	c.AddChild(a)
	c.AddChild(b)

	c.Layout(Rect{X: 0, Y: 0, W: 10, H: 10})

	if a.rect.Y != 0 {
		t.Errorf("a.rect.Y = %d, want 0", a.rect.Y)
	}

	if b.rect.Y == a.rect.Y {
		t.Errorf("b should be below a; b.rect.Y = %d, a.rect.Y = %d", b.rect.Y, a.rect.Y)
	}
}

func TestContainer_Layout_EmptyIsNoOp(t *testing.T) {
	c := NewContainer()
	// Layout on an empty container must not panic and must record the rect.
	c.Layout(Rect{X: 0, Y: 0, W: 10, H: 5})

	if c.Rect() != (Rect{X: 0, Y: 0, W: 10, H: 5}) {
		t.Errorf("Rect() = %v, want {0 0 10 5}", c.Rect())
	}
}

func TestContainer_Visible_Toggle(t *testing.T) {
	c := NewContainer()
	if !c.Visible() {
		t.Errorf("new container should be visible by default")
	}

	c.SetVisible(false)
	if c.Visible() {
		t.Errorf("container should be hidden after SetVisible(false)")
	}
}

func TestContainer_ChildAt_OutOfRange(t *testing.T) {
	c := NewContainer()
	if got := c.ChildAt(0); got != nil {
		t.Errorf("ChildAt(0) on empty = %v, want nil", got)
	}

	if got := c.ChildAt(-1); got != nil {
		t.Errorf("ChildAt(-1) = %v, want nil", got)
	}

	a := newFake(NewID())
	c.AddChild(a)
	if got := c.ChildAt(1); got != nil {
		t.Errorf("ChildAt(1) on 1-child = %v, want nil", got)
	}
}

func TestContainer_Rect_BeforeLayout(t *testing.T) {
	c := NewContainer()
	if c.Rect() != (Rect{}) {
		t.Errorf("Rect() before Layout = %v, want zero", c.Rect())
	}
}

func TestContainer_Handle_PropagatesToChildren(t *testing.T) {
	c := NewContainer()
	a := &fakeView{id: NewID(), handleRet: false}
	b := &fakeView{id: NewID(), handleRet: true}
	c.AddChild(a)
	c.AddChild(b)

	event := KeyEvent{Key: KeyRune, Rune: 'x'}

	if !c.Handle(event, nil) {
		t.Errorf("Handle = false, want true (b handles)")
	}

	if a.handleN != 1 {
		t.Errorf("a.handleN = %d, want 1", a.handleN)
	}

	if b.handleN != 1 {
		t.Errorf("b.handleN = %d, want 1", b.handleN)
	}
}

func TestContainer_Handle_StopsAtFirstConsumer(t *testing.T) {
	c := NewContainer()
	a := &fakeView{id: NewID(), handleRet: true}
	b := &fakeView{id: NewID(), handleRet: true}
	c.AddChild(a)
	c.AddChild(b)

	event := KeyEvent{Key: KeyRune, Rune: 'x'}

	if !c.Handle(event, nil) {
		t.Errorf("Handle = false, want true")
	}

	if a.handleN != 1 {
		t.Errorf("a.handleN = %d, want 1", a.handleN)
	}

	if b.handleN != 0 {
		t.Errorf("b.handleN = %d, want 0 (a consumed)", b.handleN)
	}
}

func TestContainer_Handle_NoneConsume(t *testing.T) {
	c := NewContainer()
	a := &fakeView{id: NewID(), handleRet: false}
	c.AddChild(a)

	event := KeyEvent{Key: KeyRune, Rune: 'x'}

	if c.Handle(event, nil) {
		t.Errorf("Handle = true, want false")
	}
}

func TestContainer_Invalidate_NoCtx(t *testing.T) {
	c := NewContainer()
	c.Layout(Rect{X: 0, Y: 0, W: 5, H: 5})

	// Should not panic.
	c.Invalidate(nil)
}

func TestContainer_Invalidate_NoRect(t *testing.T) {
	c := NewContainer()
	// No layout yet — invalidate must be a no-op, not panic.
	c.Invalidate(&Ctx{})
}

// Test the container invariants:
//   - renders children vertically in insertion order
//   - empty container renders no rows
func TestContainer_Spec_EmptyRendersNothing(t *testing.T) {
	c := NewContainer()
	rec := &recordingDrawer{}
	c.Paint(rec, &Ctx{})

	if len(rec.lines) != 0 {
		t.Errorf("empty container painted %d lines, want 0", len(rec.lines))
	}
}

func TestContainer_Spec_VerticalInsertionOrder(t *testing.T) {
	c := NewContainer()
	a := &recordingView{id: NewID(), label: "A"}
	b := &recordingView{id: NewID(), label: "B"}
	c.AddChild(a)
	c.AddChild(b)

	c.Layout(Rect{X: 0, Y: 0, W: 80, H: 24})

	// Children should be laid out in insertion order: a above b.
	if a.rect.Y >= b.rect.Y {
		t.Errorf("a.rect.Y (%d) should be < b.rect.Y (%d)", a.rect.Y, b.rect.Y)
	}
}

// recordingDrawer captures DrawText/FillRect calls for assertions.
type recordingDrawer struct {
	lines []string
}

func (r *recordingDrawer) FillRect(_ Rect, _ Style) {}
func (r *recordingDrawer) DrawText(p Point, s string, _ Style) {
	r.lines = append(r.lines, s)
}
func (r *recordingDrawer) DrawBorder(_ Rect, _ rovel.BoxStyle) {}
func (r *recordingDrawer) ClipRect() Rect                      { return Rect{} }
func (r *recordingDrawer) WithClip(_ Rect, fn func(Drawer)) {
	fn(r)
}

func (r *recordingDrawer) WithOffset(_ int, _ int, fn func(Drawer)) {
	fn(r)
}

// recordingView is a View that records its layout rect.
type recordingView struct {
	id    ID
	rect  Rect
	label string
}

func (r *recordingView) ID() ID        { return r.id }
func (r *recordingView) Rect() Rect    { return r.rect }
func (r *recordingView) MinSize() Size { return Size{W: 1, H: 1} }
func (r *recordingView) Layout(rect Rect) {
	r.rect = rect
}
func (r *recordingView) Focusable() bool             { return false }
func (r *recordingView) Paint(_ Drawer, _ *Ctx)      {}
func (r *recordingView) Handle(_ Event, _ *Ctx) bool { return false }

// ensure geom package is used to avoid unused import in this file.
var _ rovel.Size
