package editor

import (
	"testing"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
)

func TestWidgetUpDownHistoryAtVisualBoundaries(t *testing.T) {
	m := New()
	m.AddHistory("one")
	m.AddHistory("two")
	m.SetText("draft")
	w := NewWidgetOpts(WidgetOpts{Model: m})
	w.Layout(rovel.Rect{W: 20, H: 5})
	ctx := &rovel.Ctx{FocusedID: w.ID()}

	if !w.HandleAction(ui.ActionMoveUp, ctx) {
		t.Fatalf("up not handled")
	}
	if m.Text() != "two" {
		t.Fatalf("after up text=%q", m.Text())
	}
	if !w.HandleAction(ui.ActionMoveDown, ctx) {
		t.Fatalf("down not handled")
	}
	if m.Text() != "draft" {
		t.Fatalf("after down text=%q", m.Text())
	}
}

func TestWidgetUpDownMovesWithinMultilineBeforeHistory(t *testing.T) {
	m := New()
	m.AddHistory("history")
	m.SetText("first\nsecond")
	m.SetPosition(Position{1, 0})
	w := NewWidgetOpts(WidgetOpts{Model: m})
	w.Layout(rovel.Rect{W: 20, H: 5})
	ctx := &rovel.Ctx{FocusedID: w.ID()}

	if !w.HandleAction(ui.ActionMoveUp, ctx) {
		t.Fatalf("up not handled")
	}
	if m.Text() != "first\nsecond" {
		t.Fatalf("up should move cursor, not history: %q", m.Text())
	}
	if p := m.Position(); p.Line != 0 {
		t.Fatalf("line after up=%d", p.Line)
	}

	if !w.HandleAction(ui.ActionMoveUp, ctx) {
		t.Fatalf("second up not handled")
	}
	if m.Text() != "history" {
		t.Fatalf("second up should recall history: %q", m.Text())
	}
}

func widgetMouseCtx(w *Widget) (*rovel.Ctx, *string) {
	var copied string
	ctx := &rovel.Ctx{
		FocusedID:  w.ID(),
		Theme:      rovel.DefaultTheme(),
		Invalidate: func(rovel.Rect) {},
		ClipboardWrite: func(s string) {
			copied = s
		},
	}
	return ctx, &copied
}

func TestWidgetMouseClickPositionsCursor(t *testing.T) {
	m := New()
	m.SetText("hello world")
	w := NewWidgetOpts(WidgetOpts{Model: m})
	w.Layout(rovel.Rect{X: 2, Y: 3, W: 20, H: 3})
	ctx, _ := widgetMouseCtx(w)

	if !w.Handle(event.MouseEvent{X: 2 + 6, Y: 3, Button: event.MouseButtonLeft, Action: event.MousePress}, ctx) {
		t.Fatalf("mouse press not handled")
	}
	if got := m.Cursor(); got != 6 {
		t.Fatalf("cursor after click = %d, want 6", got)
	}
	if start, end, ok := m.Selection(); ok || start != 0 || end != 0 {
		t.Fatalf("click should leave no selection, got (%d,%d,%v)", start, end, ok)
	}
}

func TestWidgetMouseClickOutsideRectIgnored(t *testing.T) {
	m := New()
	m.SetText("hi")
	w := NewWidgetOpts(WidgetOpts{Model: m})
	w.Layout(rovel.Rect{X: 0, Y: 0, W: 10, H: 1})
	ctx, _ := widgetMouseCtx(w)

	// Y is in viewport coordinates; a click below the rect's H must not move
	// the cursor into nonsense territory.
	if w.Handle(event.MouseEvent{X: 5, Y: 99, Button: event.MouseButtonLeft, Action: event.MousePress}, ctx) {
		t.Fatalf("click outside rect should not be handled")
	}
}

func TestWidgetMouseDragSelectsAndCopiesOnRelease(t *testing.T) {
	m := New()
	m.SetText("hello world")
	w := NewWidgetOpts(WidgetOpts{Model: m})
	w.Layout(rovel.Rect{X: 0, Y: 0, W: 20, H: 1})
	ctx, copied := widgetMouseCtx(w)

	press := event.MouseEvent{X: 0, Y: 0, Button: event.MouseButtonLeft, Action: event.MousePress}
	if !w.Handle(press, ctx) {
		t.Fatalf("press not handled")
	}
	drag := event.MouseEvent{X: 5, Y: 0, Button: event.MouseButtonLeft, Action: event.MouseDrag}
	if !w.Handle(drag, ctx) {
		t.Fatalf("drag not handled")
	}
	start, end, ok := m.Selection()
	if !ok || start != 0 || end != 5 {
		t.Fatalf("drag selection = (%d,%d,%v), want (0,5,true)", start, end, ok)
	}
	release := event.MouseEvent{X: 5, Y: 0, Button: event.MouseButtonLeft, Action: event.MouseRelease}
	if !w.Handle(release, ctx) {
		t.Fatalf("release not handled")
	}
	if *copied != "hello" {
		t.Fatalf("clipboard after release = %q, want hello", *copied)
	}
	if got := m.Cursor(); got != 5 {
		t.Fatalf("cursor after release = %d, want 5", got)
	}
}

func TestWidgetMouseClickWithoutDragDoesNotCopy(t *testing.T) {
	m := New()
	m.SetText("hello")
	w := NewWidgetOpts(WidgetOpts{Model: m})
	w.Layout(rovel.Rect{X: 0, Y: 0, W: 10, H: 1})
	ctx, copied := widgetMouseCtx(w)

	press := event.MouseEvent{X: 2, Y: 0, Button: event.MouseButtonLeft, Action: event.MousePress}
	release := event.MouseEvent{X: 2, Y: 0, Button: event.MouseButtonLeft, Action: event.MouseRelease}
	if !w.Handle(press, ctx) {
		t.Fatalf("press not handled")
	}
	if !w.Handle(release, ctx) {
		t.Fatalf("release not handled")
	}
	if *copied != "" {
		t.Fatalf("click without drag should not copy, got %q", *copied)
	}
	if _, _, ok := m.Selection(); ok {
		t.Fatalf("click without drag should leave no selection")
	}
}

func TestWidgetMouseDoubleClickSelectsWord(t *testing.T) {
	m := New()
	m.SetText("hello world")
	w := NewWidgetOpts(WidgetOpts{Model: m})
	w.Layout(rovel.Rect{X: 0, Y: 0, W: 20, H: 1})
	ctx, copied := widgetMouseCtx(w)

	press := event.MouseEvent{X: 7, Y: 0, Button: event.MouseButtonLeft, Action: event.MousePress, ClickCount: 2}
	if !w.Handle(press, ctx) {
		t.Fatalf("double-click press not handled")
	}
	start, end, ok := m.Selection()
	if !ok || start != 6 || end != 11 {
		t.Fatalf("word selection = (%d,%d,%v), want (6,11,true)", start, end, ok)
	}
	release := event.MouseEvent{X: 7, Y: 0, Button: event.MouseButtonLeft, Action: event.MouseRelease, ClickCount: 2}
	if !w.Handle(release, ctx) {
		t.Fatalf("release not handled")
	}
	if *copied != "world" {
		t.Fatalf("clipboard after dbl-click release = %q, want world", *copied)
	}
}

func TestWidgetMouseTripleClickSelectsLine(t *testing.T) {
	m := New()
	m.SetText("first\nsecond")
	w := NewWidgetOpts(WidgetOpts{Model: m})
	w.Layout(rovel.Rect{X: 0, Y: 0, W: 20, H: 5})
	ctx, copied := widgetMouseCtx(w)

	press := event.MouseEvent{X: 2, Y: 1, Button: event.MouseButtonLeft, Action: event.MousePress, ClickCount: 3}
	if !w.Handle(press, ctx) {
		t.Fatalf("triple-click press not handled")
	}
	start, end, ok := m.Selection()
	if !ok || start != 6 || end != 12 {
		t.Fatalf("line selection = (%d,%d,%v), want (6,12,true)", start, end, ok)
	}
	release := event.MouseEvent{X: 2, Y: 1, Button: event.MouseButtonLeft, Action: event.MouseRelease, ClickCount: 3}
	if !w.Handle(release, ctx) {
		t.Fatalf("release not handled")
	}
	if *copied != "second" {
		t.Fatalf("clipboard after triple-click release = %q, want second", *copied)
	}
}

func TestWidgetMouseShiftClickExtendsSelection(t *testing.T) {
	m := New()
	m.SetText("hello world")
	m.SetSelection(0, 5)
	w := NewWidgetOpts(WidgetOpts{Model: m})
	w.Layout(rovel.Rect{X: 0, Y: 0, W: 20, H: 1})
	ctx, copied := widgetMouseCtx(w)

	press := event.MouseEvent{X: 11, Y: 0, Button: event.MouseButtonLeft, Action: event.MousePress, Mod: event.ModShift}
	if !w.Handle(press, ctx) {
		t.Fatalf("shift-click not handled")
	}
	start, end, ok := m.Selection()
	if !ok || start != 0 || end != 11 {
		t.Fatalf("shift-click selection = (%d,%d,%v), want (0,11,true)", start, end, ok)
	}
	release := event.MouseEvent{X: 11, Y: 0, Button: event.MouseButtonLeft, Action: event.MouseRelease, Mod: event.ModShift}
	if !w.Handle(release, ctx) {
		t.Fatalf("release not handled")
	}
	if *copied != "hello world" {
		t.Fatalf("clipboard = %q, want hello world", *copied)
	}
}

func TestWidgetMouseIgnoresOtherButtons(t *testing.T) {
	m := New()
	m.SetText("hello")
	w := NewWidgetOpts(WidgetOpts{Model: m})
	w.Layout(rovel.Rect{X: 0, Y: 0, W: 10, H: 1})
	ctx, _ := widgetMouseCtx(w)

	for _, btn := range []event.MouseButton{event.MouseButtonMiddle, event.MouseButtonRight, event.MouseButtonWheelUp, event.MouseButtonWheelDown} {
		if w.Handle(event.MouseEvent{X: 1, Y: 0, Button: btn, Action: event.MousePress}, ctx) {
			t.Fatalf("non-left button %d should not be handled", btn)
		}
	}
}
