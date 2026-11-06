package tui

import (
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/ui/layout"
)

// Container owns an ordered list of child views and renders them vertically,
// in insertion order.
//
// Container is implemented as a thin wrapper around layout.VStack that adds
// RemoveChild and Clear operations. HStack and VStack are layout primitives;
// Container is the user-facing concept. The two overlap in practice — a
// Container is a VStack with explicit add/remove/clear semantics on its
// child set.
type Container struct {
	stack   *layout.VStack
	visible bool
}

// NewContainer returns an empty Container.
func NewContainer() *Container {
	return &Container{stack: layout.NewVStack(), visible: true}
}

// NewContainerWith returns a Container pre-populated with the given children.
func NewContainerWith(children ...View) *Container {
	c := NewContainer()
	for _, child := range children {
		c.AddChild(child)
	}

	return c
}

// AddChild appends a child. Per spec, no validation is performed.
func (c *Container) AddChild(child View) {
	if child == nil {
		return
	}

	c.stack.Add(child)
}

// RemoveChild removes the first occurrence of child from the container's
// ordered list. Removing an unknown child is a no-op per spec.
func (c *Container) RemoveChild(child View) {
	if child == nil {
		return
	}

	for _, ch := range c.stack.Children() {
		if ch == child {
			c.removeByChild(ch)
			return
		}
	}
}

// Clear removes every child.
func (c *Container) Clear() {
	c.stack = layout.NewVStack()
}

// Children returns a copy of the ordered child list.
func (c *Container) Children() []View {
	return c.stack.Children()
}

// ChildCount returns the number of children.
func (c *Container) ChildCount() int {
	return len(c.stack.Children())
}

// ChildAt returns the child at index i. Returns nil if out of range.
func (c *Container) ChildAt(i int) View {
	ch := c.stack.Children()
	if i < 0 || i >= len(ch) {
		return nil
	}

	return ch[i]
}

// SetGap sets the inter-child gap in rows. Default is 0. Per spec, gap is
// optional behavior; we expose it for parity with HStack/VStack.
func (c *Container) SetGap(ctx *Ctx, gap int) {
	if gap < 0 {
		gap = 0
	}

	c.stack.SetGap(ctx, gap)
}

// ID implements View.
func (c *Container) ID() ID { return c.stack.ID() }

// Rect implements View.
func (c *Container) Rect() Rect { return c.stack.Rect() }

// MinSize implements View. Returns {1 1} for empty containers (VStack
// returns {0 0} which is the spec's empty default).
func (c *Container) MinSize() Size {
	if len(c.stack.Children()) == 0 {
		return Size{W: 1, H: 1}
	}

	return c.stack.MinSize()
}

// Layout positions the container and cascades to children.
func (c *Container) Layout(r Rect) { c.stack.Layout(r) }

// Render renders row-rendering children vertically in insertion order. Children
// that do not expose RowRenderer are skipped because the row-rendering contract
// is optional for rovel-native View implementations.
func (c *Container) Render(width int) []string {
	if c == nil || !c.visible || width <= 0 {
		return nil
	}
	var out []string
	for _, child := range c.stack.Children() {
		out = append(out, RenderRows(child, width)...)
	}
	return out
}

// Paint renders the container and its children. Per spec, an empty container
// renders no rows. Hidden containers also render no rows.
func (c *Container) Paint(d Drawer, ctx *Ctx) {
	if !c.visible {
		return
	}

	if len(c.stack.Children()) == 0 {
		return
	}

	c.stack.Paint(d, ctx)
}

// Handle forwards events to children in insertion order. The first child
// that returns true wins. VStack only handles Tab/Shift+Tab for focus
// navigation, so we walk children directly for general event propagation.
func (c *Container) Handle(e Event, ctx *Ctx) bool {
	for _, ch := range c.stack.Children() {
		if ch.Handle(e, ctx) {
			return true
		}
	}

	// Also let VStack handle its built-in Tab/Shift+Tab focus navigation.
	if ctx != nil {
		return c.stack.Handle(e, ctx)
	}

	return false
}

// Focusable returns true so the user can land focus on the container itself.
func (c *Container) Focusable() bool { return true }

// SetVisible toggles rendering. Hidden containers are not painted but still
// participate in layout.
func (c *Container) SetVisible(v bool) { c.visible = v }

// Visible reports the current visibility flag.
func (c *Container) Visible() bool { return c.visible }

// Invalidate marks the container's rect for repaint and propagates invalidation
// to children where possible.
func (c *Container) Invalidate(ctx *Ctx) {
	for _, child := range c.stack.Children() {
		switch v := child.(type) {
		case interface{ Invalidate(*Ctx) }:
			v.Invalidate(ctx)
		case interface{ Invalidate() }:
			v.Invalidate()
		}
	}

	if ctx == nil || ctx.Invalidate == nil {
		return
	}

	r := c.Rect()
	if r.W <= 0 || r.H <= 0 {
		return
	}

	ctx.Invalidate(r)
}

// removeByChild removes the first child matching v by walking Children() and
// rebuilding the VStack without it. VStack doesn't expose Remove directly, so
// we rebuild from a filtered slice.
//
// This is O(n) and allocates a new VStack on each call. For a chat-style UI
// where children are added frequently and rarely removed, that is fine. For
// workloads with many removals, replace this with a custom VStack that
// supports Remove.
func (c *Container) removeByChild(v View) {
	cur := c.stack.Children()
	filtered := make([]layout.Child, 0, len(cur))
	for _, ch := range cur {
		if ch == v {
			continue
		}
		// Preserve any SizePolicy attached to the child.
		for _, orig := range c.stack.Children() {
			if orig == ch {
				_ = orig
			}
		}

		// Re-add with default SizePolicy — VStack's Add() wraps with NewChild(v).
		filtered = append(filtered, layout.NewChild(ch))
	}

	// Rebuild the VStack from the filtered set. We can't mutate the existing
	// VStack's internal slice, so we replace it.
	c.stack = layout.NewVStackWithChildren(filtered)
}

var _ = geom.Rect{}
