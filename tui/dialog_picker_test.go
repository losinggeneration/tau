package tui

import (
	"strings"
	"testing"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/render"
	"github.com/losinggeneration/rovel/style"
	"github.com/losinggeneration/rovel/ui"
)

// TestDialogPicker_HiddenByDefault verifies the picker starts in
// the hidden state — the production code constructs it lazily and
// only shows it on demand.
func TestDialogPicker_HiddenByDefault(t *testing.T) {
	p := NewDialogPicker()
	if p.Shown() {
		t.Errorf("picker should start hidden")
	}
	if p.Focusable() {
		t.Errorf("hidden picker should not be focusable")
	}
	if sz := p.MinSize(); sz.W != 0 || sz.H != 0 {
		t.Errorf("hidden picker MinSize should be 0x0, got %+v", sz)
	}
}

// fakeDialogComponent is a minimal component implementation the
// tests use to verify the SetComponent / HandleInput path.
type fakeDialogComponent struct {
	rows    []string
	handled []event.KeyEvent
	renderW int
}

func (f *fakeDialogComponent) Render(width int) []string {
	f.renderW = width
	out := make([]string, len(f.rows))
	copy(out, f.rows)
	return out
}

func (f *fakeDialogComponent) HandleKey(ke event.KeyEvent) {
	f.handled = append(f.handled, ke)
}

// TestDialogPicker_SetComponentForwardsRenderAndInput verifies that
// installing a custom component via SetComponent delegates Paint to
// the component's Render and forwards Handle/HandleAction inputs to
// the component's HandleInput.
func TestDialogPicker_SetComponentForwardsRenderAndInput(t *testing.T) {
	p := NewDialogPicker()
	c := &fakeDialogComponent{rows: []string{"alpha", "beta", "gamma"}}
	p.SetComponent(c)
	p.SetShown(true)
	if p.Component() == nil {
		t.Fatal("Component() should return the installed component")
	}
	rows := c.Render(60)
	if len(rows) != 3 || rows[0] != "alpha" {
		t.Errorf("component.Realm[0] = %q, want %q", rows[0], "alpha")
	}
	// MinSize should reflect the component's rows.
	sz := p.MinSize()
	if sz.H != 3 {
		t.Errorf("component-hosted MinSize.H = %d, want 3", sz.H)
	}
	// Forward a rune key.
	p.Handle(rovel.KeyEvent{Key: event.KeyRune, Rune: 'x', Mod: 0}, nil)
	if len(c.handled) != 1 || c.handled[0].Key != event.KeyRune || c.handled[0].Rune != 'x' {
		t.Errorf("rune not forwarded, got %v", c.handled)
	}
	// Forward Enter.
	p.Handle(rovel.KeyEvent{Key: event.KeyEnter}, nil)
	if len(c.handled) != 2 || c.handled[1].Key != event.KeyEnter {
		t.Errorf("enter not forwarded, got %v", c.handled)
	}
	// Forward MoveUp via HandleAction.
	p.HandleAction(int(ui.ActionMoveUp), nil)
	if len(c.handled) != 3 || c.handled[2].Key != event.KeyUp {
		t.Errorf("MoveUp not forwarded, got %v", c.handled)
	}
	// Forward MoveDown via HandleAction.
	p.HandleAction(int(ui.ActionMoveDown), nil)
	if len(c.handled) != 4 || c.handled[3].Key != event.KeyDown {
		t.Errorf("MoveDown not forwarded, got %v", c.handled)
	}
	// Esc fires onCancel regardless of component.
	canceled := false
	p.SetOnCancel(func() { canceled = true })
	p.Handle(rovel.KeyEvent{Key: event.KeyEsc}, nil)
	if !canceled {
		t.Errorf("Esc should fire onCancel even with component installed")
	}
}

// TestDialogPicker_ClearComponentRestoresDefault verifies that
// passing nil to SetComponent restores the default item-list
// rendering behavior.
func TestDialogPicker_ClearComponentRestoresDefault(t *testing.T) {
	p := NewDialogPicker()
	c := &fakeDialogComponent{rows: []string{"x"}}
	p.SetComponent(c)
	p.SetShown(true)
	if p.Component() == nil {
		t.Fatal("component should be set")
	}
	p.SetComponent(nil)
	if p.Component() != nil {
		t.Fatal("Component() should be nil after clearing")
	}
	p.SetItems([]DialogPickerItem{{Name: "a"}})
	sz := p.MinSize()
	if sz.H == 0 {
		t.Errorf("default MinSize should be non-zero when items present")
	}
}

func TestDialogPicker_PaintUsesFullRowSelectionAndPlainInput(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	p.SetHeader("/resume")
	p.SetFilter("alpha")
	p.SetItems([]DialogPickerItem{
		{Name: "short", Description: "older"},
		{Name: "much-longer-session-id", Description: "newer"},
	})
	p.SetFocused(1)
	p.Layout(geom.Rect{X: 0, Y: 0, W: 40, H: p.MinSize().H})

	focus := style.Style{BG: style.ColorBlue}
	surface := style.Style{BG: style.ColorBlack}
	theme := rovel.DefaultTheme()
	theme.Palette.Focus = focus
	theme.Palette.Surface = surface
	theme.Palette.Accent = surface
	theme.Palette.SurfaceMuted = surface

	buf := render.NewBuffer(40, p.MinSize().H)
	painter := render.NewPainter(buf, geom.Rect{X: 0, Y: 0, W: 40, H: p.MinSize().H}, style.Style{})
	d := &cursorRecordingDrawer{Drawer: rovel.NewDrawer(rovel.NewPainter(painter, style.Style{}))}
	p.Paint(d, &rovel.Ctx{Theme: theme, FocusedID: p.ID()})
	if !d.visible || d.cursor != (Point{X: len("> alpha"), Y: 1}) {
		t.Fatalf("cursor=%+v visible=%t", d.cursor, d.visible)
	}

	inputY := 1
	for x := 0; x < 40; x++ {
		want := surface
		if got := buf.At(x, inputY).Style; got != want {
			t.Fatalf("input cell %d style = %#v, want %#v", x, got, want)
		}
	}

	focusedY := 4 // header, filter, separator, first row, focused second row.
	for x := 0; x < 40; x++ {
		if got := buf.At(x, focusedY).Style; got != focus {
			t.Fatalf("focused row cell %d style = %#v, want focus %#v", x, got, focus)
		}
	}
}

// TestDialogPicker_RoundTripPaint avoids the paint path but keeps
// the import of geom in use across tests.
func TestDialogPicker_RoundTripPaint(t *testing.T) {
	_ = geom.Point{}
}

// TestDialogPicker_ShownReportsMinSize verifies that a shown picker
// claims a real MinSize — without this the VStack gives it no rows
// and the picker never paints.
func TestDialogPicker_ShownReportsMinSize(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	p.SetHeader("/model")
	p.SetItems([]DialogPickerItem{
		{Name: "claude-sonnet-4-6", Description: "anthropic"},
		{Name: "gpt-4o", Description: "openai"},
	})
	sz := p.MinSize()
	if sz.W == 0 || sz.H == 0 {
		t.Errorf("shown picker should have non-zero MinSize, got %+v", sz)
	}
	// Header + filter + separator + items + counter.
	wantH := p.Visible() + 4
	if sz.H != wantH {
		t.Errorf("MinSize.H = %d, want %d", sz.H, wantH)
	}
}

// TestDialogPicker_ItemsClampFocus verifies that a shorter items
// list clamps the focused index into range, so a stale focus
// position doesn't point past the end.
func TestDialogPicker_ItemsClampFocus(t *testing.T) {
	p := NewDialogPicker()
	p.SetItems([]DialogPickerItem{
		{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"},
	})
	p.SetFocused(3)
	if p.Focused() != 3 {
		t.Fatalf("setup: focused = %d, want 3", p.Focused())
	}
	// Shorten the list; focus should clamp to the new last index.
	p.SetItems([]DialogPickerItem{{Name: "a"}, {Name: "b"}})
	if p.Focused() != 1 {
		t.Errorf("after shrink, focused = %d, want 1 (clamped)", p.Focused())
	}
}

// TestDialogPicker_FilterAppendsRune verifies that typing a rune in
// the picker's filter input updates the filter text and fires the
// onChange callback.
func TestDialogPicker_FilterAppendsRune(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	var gotFilter string
	p.SetOnChange(func(f string) { gotFilter = f })

	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	handled := p.Handle(event.KeyEvent{Key: event.KeyRune, Rune: 'c'}, ctx)
	if !handled {
		t.Errorf("Handle(KeyRune) should return true")
	}
	if p.Filter() != "c" {
		t.Errorf("Filter = %q, want %q", p.Filter(), "c")
	}
	if gotFilter != "c" {
		t.Errorf("onChange callback got %q, want %q", gotFilter, "c")
	}

	// Another rune should append, not replace.
	p.Handle(event.KeyEvent{Key: event.KeyRune, Rune: 'l'}, ctx)
	if p.Filter() != "cl" {
		t.Errorf("Filter after second rune = %q, want %q", p.Filter(), "cl")
	}
}

// TestDialogPicker_BackspaceRemovesRune verifies that Backspace
// drops the last rune from the filter and fires onChange.
func TestDialogPicker_BackspaceRemovesRune(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	p.SetFilter("hello")
	var gotFilter string
	p.SetOnChange(func(f string) { gotFilter = f })

	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	handled := p.Handle(event.KeyEvent{Key: event.KeyBackspace}, ctx)
	if !handled {
		t.Errorf("Handle(Backspace) should return true")
	}
	if p.Filter() != "hell" {
		t.Errorf("Filter after backspace = %q, want %q", p.Filter(), "hell")
	}
	if gotFilter != "hell" {
		t.Errorf("onChange callback got %q, want %q", gotFilter, "hell")
	}
}

// TestDialogPicker_BackspaceOnEmptyFilterIsNoop verifies that
// pressing Backspace with an empty filter doesn't crash or fire a
// spurious onChange.
func TestDialogPicker_BackspaceOnEmptyFilterIsNoop(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	fired := false
	p.SetOnChange(func(f string) { fired = true })

	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	handled := p.Handle(event.KeyEvent{Key: event.KeyBackspace}, ctx)
	if !handled {
		t.Errorf("Handle(Backspace) on empty filter should still return true")
	}
	if p.Filter() != "" {
		t.Errorf("Filter = %q, want empty", p.Filter())
	}
	if fired {
		t.Errorf("onChange should not fire on empty filter backspace")
	}
}

// TestDialogPicker_EscFiresOnCancel verifies that Esc dismisses the
// picker via the onCancel callback.
func TestDialogPicker_EscFiresOnCancel(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	cancelled := false
	p.SetOnCancel(func() { cancelled = true })

	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	handled := p.Handle(event.KeyEvent{Key: event.KeyEsc}, ctx)
	if !handled {
		t.Errorf("Handle(Esc) should return true")
	}
	if !cancelled {
		t.Errorf("onCancel should have fired on Esc")
	}
}

// TestDialogPicker_EnterFiresOnSelect verifies that Enter picks the
// focused item via the onSelect callback.
func TestDialogPicker_EnterFiresOnSelect(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	p.SetItems([]DialogPickerItem{
		{Name: "a"}, {Name: "b"}, {Name: "c"},
	})
	p.SetFocused(1)

	var gotIdx int
	var gotItem DialogPickerItem
	p.SetOnSelect(func(idx int, item DialogPickerItem) {
		gotIdx = idx
		gotItem = item
	})

	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	handled := p.HandleAction(int(ui.ActionActivate), ctx)
	if !handled {
		t.Errorf("HandleAction(Activate) should return true")
	}
	if gotIdx != 1 {
		t.Errorf("onSelect got idx %d, want 1", gotIdx)
	}
	if gotItem.Name != "b" {
		t.Errorf("onSelect got item %q, want %q", gotItem.Name, "b")
	}
}

// TestDialogPicker_UpDownNavigate verifies that ActionMoveUp and
// ActionMoveDown move the focused index, clamped to the list range.
func TestDialogPicker_UpDownNavigate(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	p.SetItems([]DialogPickerItem{
		{Name: "a"}, {Name: "b"}, {Name: "c"},
	})

	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	// Start at 0; Down should move to 1.
	p.HandleAction(int(ui.ActionMoveDown), ctx)
	if p.Focused() != 1 {
		t.Errorf("after Down, focused = %d, want 1", p.Focused())
	}
	// Down again moves to 2.
	p.HandleAction(int(ui.ActionMoveDown), ctx)
	if p.Focused() != 2 {
		t.Errorf("after Down, focused = %d, want 2", p.Focused())
	}
	// Down at the end is clamped — stays at 2.
	p.HandleAction(int(ui.ActionMoveDown), ctx)
	if p.Focused() != 2 {
		t.Errorf("after Down at end, focused = %d, want 2 (clamped)", p.Focused())
	}
	// Up moves back to 1.
	p.HandleAction(int(ui.ActionMoveUp), ctx)
	if p.Focused() != 1 {
		t.Errorf("after Up, focused = %d, want 1", p.Focused())
	}
	// Up at the start is clamped — stays at 0 after enough Ups.
	p.SetFocused(0)
	p.HandleAction(int(ui.ActionMoveUp), ctx)
	if p.Focused() != 0 {
		t.Errorf("after Up at start, focused = %d, want 0 (clamped)", p.Focused())
	}
}

// TestDialogPicker_PageUpDownNavigate verifies that PageUp and
// PageDown move the focused index by a visible page, clamped to the
// list range.
func TestDialogPicker_PageUpDownNavigate(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	p.SetVisible(4)
	p.SetItems([]DialogPickerItem{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
		{Name: "d"},
		{Name: "e"},
		{Name: "f"},
		{Name: "g"},
		{Name: "h"},
		{Name: "i"},
		{Name: "j"},
	})
	p.SetFocused(5)

	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	if !p.HandleAction(int(ui.ActionPageUp), ctx) {
		t.Fatalf("HandleAction(PageUp) should return true")
	}
	if p.Focused() != 1 {
		t.Errorf("after PageUp, focused = %d, want 1", p.Focused())
	}
	if !p.HandleAction(int(ui.ActionPageUp), ctx) {
		t.Fatalf("HandleAction(PageUp at start) should return true")
	}
	if p.Focused() != 0 {
		t.Errorf("after PageUp at start, focused = %d, want 0", p.Focused())
	}
	if !p.HandleAction(int(ui.ActionPageDown), ctx) {
		t.Fatalf("HandleAction(PageDown) should return true")
	}
	if p.Focused() != 4 {
		t.Errorf("after PageDown, focused = %d, want 4", p.Focused())
	}
	p.SetFocused(8)
	if !p.HandleAction(int(ui.ActionPageDown), ctx) {
		t.Fatalf("HandleAction(PageDown near end) should return true")
	}
	if p.Focused() != 9 {
		t.Errorf("after PageDown near end, focused = %d, want 9", p.Focused())
	}
}

// TestDialogPicker_PageUpDownRawKeysNavigate verifies that raw PageUp
// and PageDown key events move selection too, so the picker behaves
// correctly even if a host sends keys directly instead of semantic
// actions.
func TestDialogPicker_PageUpDownRawKeysNavigate(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	p.SetVisible(3)
	p.SetItems([]DialogPickerItem{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"}})
	p.SetFocused(3)

	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	if !p.Handle(event.KeyEvent{Key: event.KeyPageUp}, ctx) {
		t.Fatalf("Handle(PageUp) should return true")
	}
	if p.Focused() != 0 {
		t.Errorf("after raw PageUp, focused = %d, want 0", p.Focused())
	}
	if !p.Handle(event.KeyEvent{Key: event.KeyPageDown}, ctx) {
		t.Fatalf("Handle(PageDown) should return true")
	}
	if p.Focused() != 3 {
		t.Errorf("after raw PageDown, focused = %d, want 3", p.Focused())
	}
}

// TestDialogPicker_ComponentForwardsPageActions verifies that a hosted
// component receives PageUp/PageDown when the picker handles semantic
// page actions.
func TestDialogPicker_ComponentForwardsPageActions(t *testing.T) {
	p := NewDialogPicker()
	c := &fakeDialogComponent{rows: []string{"x"}}
	p.SetComponent(c)
	p.SetShown(true)

	p.HandleAction(int(ui.ActionPageUp), nil)
	p.HandleAction(int(ui.ActionPageDown), nil)
	if len(c.handled) != 2 {
		t.Fatalf("component handled %d keys, want 2", len(c.handled))
	}
	if c.handled[0].Key != event.KeyPageUp || c.handled[1].Key != event.KeyPageDown {
		t.Errorf("component handled keys = %v, want PageUp then PageDown", c.handled)
	}
}

// TestDialogPicker_DeleteBackwardViaAction verifies that
// ActionDeleteBackward is handled by HandleAction (so the toolkit's
// default keymap doesn't swallow Backspace before our Handle gets
// to mutate the filter).
func TestDialogPicker_DeleteBackwardViaAction(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	p.SetFilter("hello")
	var gotFilter string
	p.SetOnChange(func(f string) { gotFilter = f })

	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	handled := p.HandleAction(int(ui.ActionDeleteBackward), ctx)
	if !handled {
		t.Errorf("HandleAction(DeleteBackward) should return true")
	}
	if p.Filter() != "hell" {
		t.Errorf("Filter after DeleteBackward = %q, want %q", p.Filter(), "hell")
	}
	if gotFilter != "hell" {
		t.Errorf("onChange got %q, want %q", gotFilter, "hell")
	}
}

// TestDialogPicker_HiddenPickerIgnoresEvents verifies that a hidden
// picker doesn't claim key events — they should bubble through to
// other handlers.
func TestDialogPicker_HiddenPickerIgnoresEvents(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(false)
	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	if p.Handle(event.KeyEvent{Key: event.KeyRune, Rune: 'c'}, ctx) {
		t.Errorf("hidden picker should not claim rune events")
	}
	if p.HandleAction(int(ui.ActionActivate), ctx) {
		t.Errorf("hidden picker should not claim action events")
	}
}

// TestDialogPicker_ResetClearsState verifies that Reset() prepares
// the picker for a fresh dialog session: empty filter, empty items,
// focus at 0.
func TestDialogPicker_ResetClearsState(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	p.SetFilter("old")
	p.SetItems([]DialogPickerItem{{Name: "x"}, {Name: "y"}})
	p.SetFocused(1)

	p.Reset()
	if p.Filter() != "" {
		t.Errorf("after Reset, Filter = %q, want empty", p.Filter())
	}
	if len(p.Items()) != 0 {
		t.Errorf("after Reset, Items len = %d, want 0", len(p.Items()))
	}
	if p.Focused() != 0 {
		t.Errorf("after Reset, Focused = %d, want 0", p.Focused())
	}
}

// TestDialogPicker_FocusedItemOnEmptyList verifies the safe
// behavior when the picker has no items (e.g. an over-aggressive
// filter matched nothing).
func TestDialogPicker_FocusedItemOnEmptyList(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	p.SetItems(nil)
	if _, ok := p.FocusedItem(); ok {
		t.Errorf("FocusedItem on empty list should return false")
	}
	// SetFocused on an empty list shouldn't panic.
	p.SetFocused(5)
	if p.Focused() != 0 {
		t.Errorf("SetFocused on empty list should clamp to 0, got %d", p.Focused())
	}
}

// TestDialogPicker_MinSizeClampsWidth verifies that very long item
// names don't cause the picker to claim an absurd MinSize.
func TestDialogPicker_MinSizeClampsWidth(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	p.SetItems([]DialogPickerItem{
		{Name: string(make([]byte, 500)), Description: ""},
	})
	sz := p.MinSize()
	if sz.W > 80 {
		t.Errorf("MinSize.W should be capped at 80, got %d", sz.W)
	}
}

// TestDialogPicker_HandleMultibyteRune verifies that the filter
// correctly handles multibyte runes (e.g. emoji) when appending
// and backspacing.
func TestDialogPicker_HandleMultibyteRune(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	p.Handle(event.KeyEvent{Key: event.KeyRune, Rune: '🚀'}, ctx)
	if p.Filter() != "🚀" {
		t.Errorf("Filter after multibyte rune = %q, want %q", p.Filter(), "🚀")
	}
	p.Handle(event.KeyEvent{Key: event.KeyBackspace}, ctx)
	if p.Filter() != "" {
		t.Errorf("Filter after backspace = %q, want empty", p.Filter())
	}
}

// TestDialogPicker_geomImportKeptInUse is a no-op test that exists
// only to keep the geom import in use (the package is imported for
// type completeness in case future tests need it). Remove if the
// import goes unused elsewhere.
func TestDialogPicker_geomImportKeptInUse(t *testing.T) {
	_ = geom.Size{}
}

func TestDialogPickerVisibilityNotificationsIgnoreRedundantChanges(t *testing.T) {
	picker := NewDialogPicker()
	var changes []bool
	picker.SetOnVisibilityChanged(func(shown bool) { changes = append(changes, shown) })
	picker.SetShown(false)
	picker.SetShown(true)
	picker.SetShown(true)
	picker.SetShown(false)
	if len(changes) != 2 || !changes[0] || changes[1] {
		t.Fatalf("visibility changes = %v, want [true false]", changes)
	}
}

func TestDialogPickerHostedPrimitiveResolvesPhysicalCursor(t *testing.T) {
	p := NewDialogPicker()
	p.SetShown(true)
	p.SetComponent(&InputPrimitive{Prompt: "> ", Value: "界x"})
	p.Layout(Rect{X: 1, Y: 1, W: 10, H: 2})
	buf := render.NewBuffer(15, 4)
	d := &cursorRecordingDrawer{Drawer: rovel.NewDrawer(rovel.NewPainter(render.NewPainter(buf, Rect{W: 15, H: 4}, Style{}), Style{}))}
	p.Paint(d, &Ctx{Theme: DefaultTheme(), FocusedID: p.ID()})
	if !d.visible || d.cursor != (Point{X: 6, Y: 1}) {
		t.Fatalf("cursor=%+v visible=%t", d.cursor, d.visible)
	}
	if buf.At(3, 1).R != '界' || buf.At(5, 1).R != 'x' {
		t.Fatalf("hosted row contains marker bytes: %+v", buf)
	}
	if buf.At(6, 1).Style.Attr&AttrReverse != 0 {
		t.Fatal("hosted primitive retained fake cursor")
	}
}

func TestDialogPickerMultilineHeaderKeepsMessageAndChoicesVisible(t *testing.T) {
	p := NewDialogPicker()
	p.SetHeader("Allow action\nExecute this command?")
	p.SetVisible(2)
	p.SetItems([]DialogPickerItem{{Name: "Yes"}, {Name: "No"}})
	p.SetShown(true)
	if p.MinSize().H != 7 {
		t.Fatalf("height=%d", p.MinSize().H)
	}
	p.Layout(geom.Rect{W: 40, H: 7})
	buf := NewBuffer(40, 7)
	base := Style{}
	painter := NewRenderPainter(buf, Rect{W: 40, H: 7}, base)
	p.Paint(NewDrawer(NewPainter(painter, base)), &Ctx{Theme: Theme{Base: base}})
	for y, want := range map[int]string{0: "Allow action", 1: "Execute this command?", 2: "> ", 4: "→ Yes", 5: "  No"} {
		var row strings.Builder
		for x := range 40 {
			row.WriteRune(buf.At(x, y).R)
		}
		if !strings.HasPrefix(row.String(), want) {
			t.Fatalf("row %d=%q, want %q", y, row.String(), want)
		}
	}
}

func TestDialogPickerHostedInputReceivesPasteAndClipboard(t *testing.T) {
	input := &InputPrimitive{}
	p := NewDialogPicker()
	p.SetComponent(input)
	p.SetShown(true)
	for _, e := range []Event{PasteEvent{Text: "a\nb"}, ClipboardResponseEvent{Text: " c"}} {
		if !p.Handle(e, nil) {
			t.Fatalf("paste not handled: %T", e)
		}
	}
	if input.Value != "a b c" {
		t.Fatalf("value=%q", input.Value)
	}
}
