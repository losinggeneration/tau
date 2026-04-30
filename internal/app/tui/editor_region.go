package tui

import (
	tauui "github.com/losinggeneration/tau/tui"
)

// EditorDialogRegion is the application editor slot. Most of the time it
// lays out and paints the prompt editor frame together with the autocomplete
// selector that belongs to it. When a true dialog picker is visible, it
// replaces the editor in the same region instead of being inserted as an extra
// row elsewhere in the stack. This matches the Pi application layout contract
// for selector/dialog surfaces.
//
// The autocomplete selector is editor chrome, not a below-editor widget: the
// spec places it directly under the editor's bottom rule sharing the editor's
// content column, and it must stay adjacent to the editor no matter what else
// occupies the widget regions. A dialog suppresses it — a dialog owns its own
// input, so an inline completion list under it would be answering a prompt
// that is no longer on screen.
type EditorDialogRegion struct {
	id       tauui.ID
	rect     tauui.Rect
	editor   *tauui.PromptEditor
	dialog   *tauui.DialogPicker
	selector tauui.View
}

func NewEditorDialogRegion(editor *tauui.PromptEditor, dialog *tauui.DialogPicker, selector tauui.View) *EditorDialogRegion {
	return &EditorDialogRegion{id: tauui.NewID(), editor: editor, dialog: dialog, selector: selector}
}

func (e *EditorDialogRegion) ID() tauui.ID { return e.id }

func (e *EditorDialogRegion) Rect() tauui.Rect { return e.rect }

func (e *EditorDialogRegion) active() tauui.View {
	if e == nil {
		return nil
	}
	if e.dialog != nil && e.dialog.Shown() {
		return e.dialog
	}
	if e.editor == nil {
		return nil
	}
	return e.editor
}

// dialogShown reports whether the dialog has taken over the region.
func (e *EditorDialogRegion) dialogShown() bool {
	return e != nil && e.dialog != nil && e.dialog.Shown()
}

// selectorView returns the autocomplete selector when it should participate in
// this region's layout.
func (e *EditorDialogRegion) selectorView() tauui.View {
	if e == nil || e.selector == nil || e.dialogShown() {
		return nil
	}
	if e.selector.MinSize().H <= 0 {
		return nil
	}
	return e.selector
}

// selectorRect places the selector directly below the editor's bottom rule,
// indented to the editor's content column.
func (e *EditorDialogRegion) selectorRect(r tauui.Rect, editorH, selectorH int) tauui.Rect {
	pad := 0
	if e.editor != nil {
		pad = e.editor.Padding()
	}
	rect := tauui.Rect{X: r.X + pad, Y: r.Y + editorH, W: r.W - 2*pad, H: selectorH}
	if rect.W < 1 {
		rect.X = r.X
		rect.W = r.W
	}
	return rect
}

func (e *EditorDialogRegion) Layout(r tauui.Rect) {
	e.rect = r
	active := e.active()
	if e.editor != nil && e.editor != active {
		e.editor.Layout(tauui.Rect{})
	}
	if e.dialog != nil && e.dialog != active {
		e.dialog.Layout(tauui.Rect{})
	}

	selector := e.selectorView()
	if selector == nil {
		if e.selector != nil {
			e.selector.Layout(tauui.Rect{})
		}
		if active != nil {
			active.Layout(r)
		}
		return
	}

	selectorH := min(selector.MinSize().H, r.H)
	editorRect := r
	editorRect.H = max(0, r.H-selectorH)
	if active != nil {
		active.Layout(editorRect)
	}
	selector.Layout(e.selectorRect(r, editorRect.H, selectorH))
}

func (e *EditorDialogRegion) MinSize() tauui.Size {
	active := e.active()
	if active == nil {
		return tauui.Size{}
	}
	size := active.MinSize()
	if selector := e.selectorView(); selector != nil {
		selectorSize := selector.MinSize()
		size.H += selectorSize.H
		size.W = max(size.W, selectorSize.W)
	}
	return size
}

func (e *EditorDialogRegion) PreferredSize() tauui.Size { return e.MinSize() }

func (e *EditorDialogRegion) Paint(d tauui.Drawer, ctx *tauui.Ctx) {
	if active := e.active(); active != nil {
		active.Paint(d, ctx)
	}
	if selector := e.selectorView(); selector != nil {
		selector.Paint(d, ctx)
	}
}

func (e *EditorDialogRegion) Handle(ev tauui.Event, ctx *tauui.Ctx) bool {
	if selector := e.selectorView(); selector != nil && selector.Handle(ev, ctx) {
		return true
	}
	if active := e.active(); active != nil {
		return active.Handle(ev, ctx)
	}
	return false
}

func (e *EditorDialogRegion) HandleAction(act tauui.Action, ctx *tauui.Ctx) bool {
	if selector := e.selectorView(); selector != nil && handleViewAction(selector, act, ctx) {
		return true
	}
	active := e.active()
	if active == nil {
		return false
	}
	return handleViewAction(active, act, ctx)
}

// handleViewAction dispatches an action to a view that may declare either
// signature the toolkit accepts.
func handleViewAction(v tauui.View, act tauui.Action, ctx *tauui.Ctx) bool {
	if h, ok := v.(interface {
		HandleAction(tauui.Action, *tauui.Ctx) bool
	}); ok {
		return h.HandleAction(act, ctx)
	}
	if h, ok := v.(interface {
		HandleAction(int, *tauui.Ctx) bool
	}); ok {
		return h.HandleAction(int(act), ctx)
	}
	return false
}

func (e *EditorDialogRegion) Focusable() bool { return false }

func (e *EditorDialogRegion) Children() []tauui.View {
	children := make([]tauui.View, 0, 3)
	if e.editor != nil {
		children = append(children, e.editor)
	}
	if e.dialog != nil {
		children = append(children, e.dialog)
	}
	if e.selector != nil {
		children = append(children, e.selector)
	}
	return children
}
