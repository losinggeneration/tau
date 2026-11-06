package editor

import (
	"testing"

	"github.com/losinggeneration/rovel/render"
	"github.com/losinggeneration/rovel/text"
	"github.com/stretchr/testify/assert"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
)

func TestWidgetUpDownHistoryAtVisualBoundaries(t *testing.T) {
	m := New()
	m.AddHistory("one")
	m.AddHistory("two")
	m.SetText("draft")
	m.SetCursor(0)
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

func TestWidgetInsertsShiftedProducedCharacters(t *testing.T) {
	w := NewWidget()
	ctx := &rovel.Ctx{FocusedID: w.ID()}
	for _, key := range []event.KeyEvent{
		{Key: event.KeyRune, Rune: '=', ShiftedRune: '+', Mod: event.ModShift},
		{Key: event.KeyRune, Rune: 'c', ShiftedRune: 'C', Mod: event.ModShift},
		{Key: event.KeyRune, Rune: 'E', Mod: event.ModShift},
	} {
		if !w.Handle(key, ctx) {
			t.Fatalf("key not inserted: %+v", key)
		}
	}
	if got := w.Model().Text(); got != "+CE" {
		t.Fatalf("text=%q", got)
	}
	for _, mod := range []event.ModMask{event.ModCtrl, event.ModAlt, event.ModShift | event.ModCtrl} {
		if w.Handle(event.KeyEvent{Key: event.KeyRune, Rune: '=', ShiftedRune: '+', Mod: mod}, ctx) {
			t.Fatalf("modified key inserted: %v", mod)
		}
	}
}

type editorCursorDrawer struct {
	rovel.CellDrawer
	pos     rovel.Point
	visible bool
}

func (d *editorCursorDrawer) SetCursor(pos rovel.Point) { d.pos = pos; d.visible = true }

func TestWidgetPaintUsesPhysicalCursorAndPreservesSelection(t *testing.T) {
	for _, mode := range []string{"focused", "unfocused", "readonly", "selection", "scrolled"} {
		t.Run(mode, func(t *testing.T) {
			w := NewWidget()
			w.Layout(rovel.Rect{X: 2, Y: 1, W: 5, H: 2})
			w.Model().SetText("界abc\nsecond\nthird")
			w.Model().SetCursor(len("界"))
			if mode == "readonly" {
				w.SetReadOnly(true)
			}
			if mode == "selection" {
				w.Model().SetSelection(0, len("界"))
			}
			if mode == "scrolled" {
				w.Model().SetCursor(len(w.Model().Text()))
			}
			buf := render.NewBuffer(10, 5)
			d := &editorCursorDrawer{CellDrawer: rovel.NewDrawer(rovel.NewPainter(render.NewPainter(buf, rovel.Rect{W: 10, H: 5}, rovel.Style{}), rovel.Style{})).(rovel.CellDrawer)}
			ctx := &rovel.Ctx{Theme: rovel.DefaultTheme(), FocusedID: w.ID()}
			if mode == "unfocused" {
				ctx.FocusedID = 0
			}
			w.Paint(d, ctx)
			wantVisible := mode != "unfocused" && mode != "readonly"
			if d.visible != wantVisible {
				t.Fatalf("cursor visible=%t want %t", d.visible, wantVisible)
			}
			if wantVisible && !(rovel.Rect{X: 2, Y: 1, W: 5, H: 2}).Contains(d.pos) {
				t.Fatalf("cursor outside editor: %+v", d.pos)
			}
			if mode == "focused" && d.pos != (rovel.Point{X: 4, Y: 1}) {
				t.Fatalf("cursor=%+v", d.pos)
			}
			if mode == "selection" && buf.At(2, 1).Style != ctx.Theme.Palette.Selection {
				t.Fatalf("lost selection: %+v", buf.At(2, 1))
			}
			if mode == "focused" && buf.At(4, 1).Style != ctx.Theme.Base {
				t.Fatalf("fake cursor remains: %+v", buf.At(4, 1))
			}
		})
	}
}

func TestWidgetSetTextPreservesUnicodeGraphemes(t *testing.T) {
	for _, value := range []string{"e\u0301", "👩\u200d💻", "✈\ufe0f", "ش\u200cم", "字\U000e0100"} {
		t.Run(value, func(t *testing.T) {
			w := NewWidget()
			var notified string
			w.SetOnChange(func(s string, _ *rovel.Ctx) { notified = s })
			w.SetText(nil, value)
			if got := w.Model().Text(); got != value {
				t.Fatalf("stored %q, want %q", got, value)
			}
			if notified != value {
				t.Fatalf("notified %q, want %q", notified, value)
			}
			w.Model().Undo()
			if got := w.Model().Text(); got != "" {
				t.Fatalf("undo = %q", got)
			}
		})
	}
}

func TestWidgetEditorActionsRespectFocusReadOnlyAndNotify(t *testing.T) {
	for _, act := range []ui.Action{ActionDeleteToLineEnd, ActionDeleteToLineStart, ActionDeleteWordBackward, ActionDeleteWordForward, ActionUndo, ActionYank, ActionYankPop} {
		w := NewWidget()
		m := w.Model()
		m.SetText("saved")
		m.DeleteToLineStart()
		m.SetText("one two")
		m.SetCursor(4)
		ctx := &rovel.Ctx{FocusedID: w.ID()}
		before, cursor := m.Text(), m.Cursor()
		w.SetReadOnly(true)
		if w.HandleAction(act, ctx) || m.Text() != before || m.Cursor() != cursor {
			t.Fatalf("read-only action %v edited text", act)
		}
		w.SetReadOnly(false)
		ctx.FocusedID = 0
		if w.HandleAction(act, ctx) || m.Text() != before || m.Cursor() != cursor {
			t.Fatalf("unfocused action %v edited text", act)
		}
	}
	w := NewWidget()
	w.Model().SetText("one two")
	ctx := &rovel.Ctx{FocusedID: w.ID()}
	var changes []string
	var moves []CursorMoveEvent
	w.SetOnChange(func(s string, _ *rovel.Ctx) { changes = append(changes, s) })
	w.SetOnCursorMove(func(e CursorMoveEvent, _ *rovel.Ctx) { moves = append(moves, e) })
	w.HandleAction(ActionMoveWordLeft, ctx)
	w.HandleAction(ActionDeleteWordForward, ctx)
	if len(changes) != 1 || changes[0] != "one " {
		t.Fatalf("changes=%v", changes)
	}
	if len(moves) != 1 || moves[0].Cursor != 4 || moves[0].Reason != CursorMoveNavigate {
		t.Fatalf("moves=%v", moves)
	}
	w.HandleAction(ActionUndo, ctx)
	if len(changes) != 2 || changes[1] != "one two" {
		t.Fatalf("undo changes=%v", changes)
	}
}

func TestWidgetResetNotifiesOnceAndDiscardsUndo(t *testing.T) {
	w := NewWidget()
	w.Model().SetText("draft")
	var changed []string
	var moved []CursorMoveEvent
	w.SetOnChange(func(value string, _ *rovel.Ctx) { changed = append(changed, value) })
	w.SetOnCursorMove(func(move CursorMoveEvent, _ *rovel.Ctx) { moved = append(moved, move) })
	w.Reset(nil)
	if len(changed) != 1 || changed[0] != "" {
		t.Fatalf("changes=%v", changed)
	}
	if len(moved) != 1 || moved[0].Cursor != 0 || moved[0].Reason != CursorMoveSetText {
		t.Fatalf("cursor events=%v", moved)
	}
	w.Model().Undo()
	if w.Text() != "" {
		t.Fatalf("undo restored submitted draft %q", w.Text())
	}
}

func TestWidgetTypingPreservesUnicodeFormatCharacters(t *testing.T) {
	value := "👩\u200d💻ش\u200cم\u00a0\u2028\U000e0100"
	w := NewWidget()
	ctx := &rovel.Ctx{FocusedID: w.ID()}
	for _, r := range value {
		if !w.Handle(event.KeyEvent{Key: event.KeyRune, Rune: r}, ctx) {
			t.Fatalf("rejected U+%04X", r)
		}
	}
	if w.Text() != value {
		t.Fatalf("typed=%q want %q", w.Text(), value)
	}
	for _, r := range []rune{0, 7, 27, 127, 128, 159} {
		if w.Handle(event.KeyEvent{Key: event.KeyRune, Rune: r}, ctx) {
			t.Fatalf("accepted control U+%04X", r)
		}
	}
}

func TestVisualCursorUsesFollowingRowAtWrappedBoundary(t *testing.T) {
	for _, tc := range []struct {
		value            string
		cursor, row, col int
	}{
		{"abcdef", 3, 1, 0}, {"abcdef", 6, 1, 3}, {"abc\ndef", 3, 0, 3}, {"abc\ndef", 4, 1, 0}, {"界ab界c", len("界a"), 1, 0},
	} {
		row, col := visualCursor(editorVisualRows(tc.value, 3), tc.cursor, 3)
		if row != tc.row || col != tc.col {
			t.Fatalf("%q cursor %d = %d,%d want %d,%d", tc.value, tc.cursor, row, col, tc.row, tc.col)
		}
	}
}

func TestWidgetPageActionsMoveAcrossVisualRows(t *testing.T) {
	w := NewWidget()
	w.Layout(rovel.Rect{W: 3, H: 2})
	m := w.Model()
	m.SetText("abcdefghijklmnopqr")
	m.SetCursor(1)
	ctx := &rovel.Ctx{FocusedID: w.ID()}
	var moves []CursorMoveEvent
	w.SetOnCursorMove(func(ev CursorMoveEvent, _ *rovel.Ctx) { moves = append(moves, ev) })
	if !w.HandleAction(ui.ActionPageDown, ctx) {
		t.Fatal("page down unhandled")
	}
	if m.Cursor() != 7 {
		t.Fatalf("page down cursor=%d", m.Cursor())
	}
	w.HandleAction(ui.ActionPageDown, ctx)
	if m.Cursor() != 13 {
		t.Fatalf("second page cursor=%d", m.Cursor())
	}
	w.HandleAction(ui.ActionPageDown, ctx)
	if m.Cursor() != 16 {
		t.Fatalf("clamped page cursor=%d", m.Cursor())
	}
	w.HandleAction(ui.ActionPageUp, ctx)
	if m.Cursor() != 10 {
		t.Fatalf("page up cursor=%d", m.Cursor())
	}
	if len(moves) != 4 {
		t.Fatalf("cursor notifications=%+v", moves)
	}
	if m.Text() != "abcdefghijklmnopqr" {
		t.Fatalf("text changed=%q", m.Text())
	}
}

func TestWidgetCharacterJumpConsumesNextTextAndCancelsOnAction(t *testing.T) {
	w := NewWidget()
	m := w.Model()
	m.SetText("aba")
	m.SetCursor(0)
	ctx := &rovel.Ctx{FocusedID: w.ID()}
	var changes int
	var moves []CursorMoveEvent
	w.SetOnChange(func(string, *rovel.Ctx) { changes++ })
	w.SetOnCursorMove(func(ev CursorMoveEvent, _ *rovel.Ctx) { moves = append(moves, ev) })
	assert.True(t, w.Handle(event.KeyEvent{Key: event.KeyRune, Rune: ']', Mod: event.ModCtrl}, ctx))
	assert.True(t, w.Handle(event.KeyEvent{Key: event.KeyRune, Rune: 'a'}, ctx))
	assert.Equal(t, 2, m.Cursor())
	assert.Equal(t, "aba", m.Text())
	assert.Zero(t, changes)
	assert.Len(t, moves, 1)
	assert.True(t, w.Handle(event.KeyEvent{Key: event.KeyRune, Rune: ']', Mod: event.ModCtrl}, ctx))
	assert.True(t, w.HandleAction(ui.ActionMoveLeft, ctx))
	assert.True(t, w.Handle(event.KeyEvent{Key: event.KeyRune, Rune: 'X'}, ctx))
	assert.Equal(t, "aXba", m.Text())
	assert.Equal(t, 1, changes)
}

func TestWidgetArrowHistoryRequiresStartAndMovesToLineEdges(t *testing.T) {
	m := New()
	m.AddHistory("history")
	m.SetText("draft")
	w := NewWidgetOpts(WidgetOpts{Model: m})
	w.Layout(rovel.Rect{W: 20, H: 5})
	ctx := &rovel.Ctx{FocusedID: w.ID()}
	assert.True(t, w.HandleAction(ui.ActionMoveUp, ctx))
	assert.Equal(t, "draft", m.Text())
	assert.Zero(t, m.Cursor())
	assert.True(t, w.HandleAction(ui.ActionMoveUp, ctx))
	assert.Equal(t, "history", m.Text())
	assert.True(t, w.HandleAction(ui.ActionMoveDown, ctx))
	assert.Equal(t, "draft", m.Text())
	assert.Zero(t, m.Cursor())
	assert.True(t, w.HandleAction(ui.ActionMoveDown, ctx))
	assert.Equal(t, "draft", m.Text())
	assert.Equal(t, 5, m.Cursor())
	m.SetText("first\nsecond")
	m.SetCursor(8)
	assert.True(t, w.HandleAction(ui.ActionMoveDown, ctx))
	assert.Equal(t, len(m.Text()), m.Cursor())
	m.SetText("abcde")
	m.SetCursor(3)
	w.Layout(rovel.Rect{W: 2, H: 5})
	assert.True(t, w.HandleAction(ui.ActionMoveUp, ctx))
	assert.Equal(t, 1, m.Cursor())
	assert.Equal(t, "abcde", m.Text())
	assert.True(t, w.HandleAction(ui.ActionMoveUp, ctx))
	assert.Zero(t, m.Cursor())
	assert.Equal(t, "abcde", m.Text())
}

func TestWidgetReadOnlyNavigationDoesNotReplaceTextWithHistory(t *testing.T) {
	for _, act := range []ui.Action{ui.ActionMoveUp, ui.ActionMoveDown, ActionHistoryPrevious, ActionHistoryNext} {
		for _, browsing := range []bool{false, true} {
			m := New()
			m.AddHistory("older")
			m.AddHistory("newer")
			m.SetText("draft")
			m.SetCursor(0)
			if browsing {
				m.HistoryPrev()
			}
			w := NewWidgetOpts(WidgetOpts{Model: m})
			w.Layout(rovel.Rect{W: 20, H: 5})
			w.SetReadOnly(true)
			ctx := &rovel.Ctx{FocusedID: w.ID()}
			var changes int
			w.SetOnChange(func(string, *rovel.Ctx) { changes++ })
			before := m.Text()
			index := m.HistoryIndex()
			w.HandleAction(act, ctx)
			assert.Equal(t, before, m.Text(), "action %v browsing %t", act, browsing)
			assert.Equal(t, index, m.HistoryIndex())
			assert.Zero(t, changes)
		}
	}
	m := New()
	m.SetText("first\nsecond")
	w := NewWidgetOpts(WidgetOpts{Model: m})
	w.Layout(rovel.Rect{W: 20, H: 5})
	w.SetReadOnly(true)
	ctx := &rovel.Ctx{FocusedID: w.ID()}
	assert.True(t, w.HandleAction(ui.ActionMoveUp, ctx))
	assert.Equal(t, "first\nsecond", m.Text())
	assert.Less(t, m.Cursor(), len(m.Text()))
}

func TestWidgetProgrammaticInsertionNotifiesOnce(t *testing.T) {
	m := New()
	m.SetText("界 suffix")
	m.SetCursor(len("界"))
	w := NewWidgetOpts(WidgetOpts{Model: m})
	ctx, _ := widgetMouseCtx(w)
	var changes []string
	var moves []CursorMoveEvent
	w.SetOnChange(func(s string, _ *rovel.Ctx) { changes = append(changes, s) })
	w.SetOnCursorMove(func(move CursorMoveEvent, _ *rovel.Ctx) { moves = append(moves, move) })
	w.InsertTextAtCursor(ctx, "\r\n🙂\t")
	assert.Equal(t, "界\n🙂     suffix", m.Text())
	assert.Equal(t, []string{m.Text()}, changes)
	assert.Equal(t, []CursorMoveEvent{{Cursor: len("界\n🙂    "), Reason: CursorMoveEdit}}, moves)
	w.InsertTextAtCursor(ctx, "")
	assert.Len(t, changes, 1)
	assert.Len(t, moves, 1)
	m.Undo()
	assert.Equal(t, "界 suffix", m.Text())
	assert.Equal(t, len("界"), m.Cursor())
}

func TestWidgetPaintPreservesUnicodeGraphemes(t *testing.T) {
	const value = "ASCII 日 👩‍💻 ♥️ á"
	for _, width := range []int{24, 10, 2, 1} {
		for _, selected := range []bool{false, true} {
			w := NewWidget()
			w.Layout(rovel.Rect{X: 2, Y: 1, W: width, H: 20})
			w.Model().SetText(value)
			if selected {
				w.Model().SetSelection(0, len(value))
			}
			buf := render.NewBuffer(width+4, 22)
			d := &editorCursorDrawer{CellDrawer: rovel.NewDrawer(rovel.NewPainter(render.NewPainter(buf, rovel.Rect{W: width + 4, H: 22}, rovel.Style{}), rovel.Style{})).(rovel.CellDrawer)}
			ctx := &rovel.Ctx{Theme: rovel.DefaultTheme(), FocusedID: w.ID()}
			w.Paint(d, ctx)
			for y, row := range editorVisualRows(value, width) {
				x := 0
				for off := 0; off < len(row.text); {
					next := text.NextCluster(row.text, off)
					cluster := row.text[off:next]
					cw := text.Width(cluster)
					if cw > 0 && x+cw <= width {
						cell := buf.At(2+x, 1+y)
						assert.Equal(t, cluster, cell.Text(), "width=%d selected=%t column=%d", width, selected, x)
						if selected {
							assert.Equal(t, ctx.Theme.Palette.Selection, cell.Style)
						}
					}
					x += cw
					off = next
				}
			}
			if width == 24 {
				assert.Equal(t, "👩‍💻", buf.At(11, 1).Text())
				assert.Equal(t, "♥️", buf.At(14, 1).Text())
				assert.Equal(t, "á", buf.At(17, 1).Text())
				assert.Equal(t, rovel.Point{X: 18, Y: 1}, d.pos)
			}
			for y := range 20 {
				assert.Equal(t, " ", buf.At(1, y+1).Text())
				assert.Equal(t, " ", buf.At(width+2, y+1).Text())
			}
			assert.Equal(t, value, w.Model().Text())
		}
	}
}
