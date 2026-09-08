package tui

import (
	"strings"
	"testing"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/render"
	"github.com/losinggeneration/rovel/style"
	"github.com/losinggeneration/rovel/ui"
)

// TestSelectorDefaults verifies the constructor's defaults: no items,
// focus at zero, and the default visible count of 5.
func TestSelectorDefaults(t *testing.T) {
	s := NewSelector()
	if s.Visible() != DefaultSelectorVisible {
		t.Errorf("Visible = %d, want %d", s.Visible(), DefaultSelectorVisible)
	}
	if len(s.Items()) != 0 {
		t.Errorf("Items = %d, want 0", len(s.Items()))
	}
	if s.Focused() != 0 {
		t.Errorf("Focused = %d, want 0", s.Focused())
	}
}

// TestSelectorSetItemsResetsFocused verifies that replacing the item list
// resets the focused index to the first item.
func TestSelectorSetItemsResetsFocused(t *testing.T) {
	s := NewSelector()
	s.SetItems([]SelectorItem{
		{Name: "a"}, {Name: "b"}, {Name: "c"},
	})
	s.SetFocused(2)
	if s.Focused() != 2 {
		t.Fatalf("Focused = %d, want 2", s.Focused())
	}
	s.SetItems([]SelectorItem{{Name: "x"}, {Name: "y"}})
	if s.Focused() != 0 {
		t.Errorf("after replace Focused = %d, want 0", s.Focused())
	}
}

// TestSelectorSetVisibleClamp verifies non-positive values fall back
// to the default, and positive values override.
func TestSelectorSetVisibleClamp(t *testing.T) {
	s := NewSelector()
	s.SetVisible(0)
	if s.Visible() != DefaultSelectorVisible {
		t.Errorf("Visible after 0 = %d, want default %d", s.Visible(), DefaultSelectorVisible)
	}
	s.SetVisible(-3)
	if s.Visible() != DefaultSelectorVisible {
		t.Errorf("Visible after -3 = %d, want default %d", s.Visible(), DefaultSelectorVisible)
	}
	s.SetVisible(7)
	if s.Visible() != 7 {
		t.Errorf("Visible after 7 = %d, want 7", s.Visible())
	}
}

// TestSelectorNavigation verifies Up/Down clamp at the list edges and emit invalidations.
func TestSelectorNavigation(t *testing.T) {
	invalidated := 0
	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) { invalidated++ }}
	s := NewSelector()
	s.SetItems([]SelectorItem{
		{Name: "a"}, {Name: "b"}, {Name: "c"},
	})

	s.HandleAction(int(ui.ActionMoveDown), ctx)
	if s.Focused() != 1 {
		t.Errorf("after Down focused = %d, want 1", s.Focused())
	}
	s.HandleAction(int(ui.ActionMoveDown), ctx)
	s.HandleAction(int(ui.ActionMoveDown), ctx) // clamps at end
	if s.Focused() != 2 {
		t.Errorf("after Down x3 focused = %d, want 2", s.Focused())
	}
	s.SetFocused(0)
	s.HandleAction(int(ui.ActionMoveUp), ctx)
	if s.Focused() != 0 {
		t.Errorf("after Up from first focused = %d, want 0", s.Focused())
	}
	if invalidated < 2 {
		t.Errorf("expected at least 2 invalidations, got %d", invalidated)
	}
}

func TestSelectorPageUpDownNavigate(t *testing.T) {
	invalidated := 0
	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) { invalidated++ }}
	s := NewSelector()
	s.SetVisible(3)
	s.SetItems([]SelectorItem{
		{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"},
	})
	s.SetFocused(4)

	if !s.HandleAction(int(ui.ActionPageUp), ctx) {
		t.Fatal("HandleAction(PageUp) returned false")
	}
	if s.Focused() != 1 {
		t.Fatalf("after PageUp focused = %d, want 1", s.Focused())
	}
	if !s.HandleAction(int(ui.ActionPageUp), ctx) {
		t.Fatal("HandleAction(PageUp at start) returned false")
	}
	if s.Focused() != 0 {
		t.Fatalf("after PageUp at start focused = %d, want 0", s.Focused())
	}
	if !s.HandleAction(int(ui.ActionPageDown), ctx) {
		t.Fatal("HandleAction(PageDown) returned false")
	}
	if s.Focused() != 3 {
		t.Fatalf("after PageDown focused = %d, want 3", s.Focused())
	}
	if !s.HandleAction(int(ui.ActionPageDown), ctx) {
		t.Fatal("HandleAction(PageDown at end) returned false")
	}
	if s.Focused() != 4 {
		t.Fatalf("after PageDown at end focused = %d, want 4", s.Focused())
	}
	if invalidated < 4 {
		t.Fatalf("invalidations = %d, want at least 4", invalidated)
	}
}

// TestSelectorEnterFiresOnSelect verifies the ActionActivate path.
func TestSelectorEnterFiresOnSelect(t *testing.T) {
	var gotIdx int
	var gotItem SelectorItem
	called := 0
	s := NewSelector()
	s.SetItems([]SelectorItem{
		{Name: "help", Description: "Show help"},
		{Name: "clear", Description: "Clear chat"},
	})
	s.SetOnSelect(func(idx int, item SelectorItem) {
		gotIdx = idx
		gotItem = item
		called++
	})
	s.SetFocused(1)
	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	if !s.HandleAction(int(ui.ActionActivate), ctx) {
		t.Fatal("HandleAction(ActionActivate) returned false")
	}
	if called != 1 {
		t.Fatalf("onSelect called %d times, want 1", called)
	}
	if gotIdx != 1 {
		t.Errorf("gotIdx = %d, want 1", gotIdx)
	}
	if gotItem.Name != "clear" {
		t.Errorf("gotItem = %q, want %q", gotItem.Name, "clear")
	}
}

// TestSelectorTabFiresOnSelect via ActionSelect verifies the dedicated
// Tab action routes to the same onSelect callback.
func TestSelectorTabFiresOnSelect(t *testing.T) {
	called := 0
	s := NewSelector()
	s.SetItems([]SelectorItem{{Name: "x"}})
	s.SetOnSelect(func(int, SelectorItem) { called++ })
	ctx := &rovel.Ctx{Invalidate: func(geom.Rect) {}}
	if !s.HandleActionWithTab(int(ActionSelect), ctx) {
		t.Fatal("HandleActionWithTab(ActionSelect) returned false")
	}
	if called != 1 {
		t.Errorf("onSelect called %d times, want 1", called)
	}
}

// TestSelectorEscFiresOnClose verifies the raw Esc path.
func TestSelectorEscFiresOnClose(t *testing.T) {
	closed := false
	s := NewSelector()
	s.SetOnClose(func() { closed = true })
	if !s.Handle(rovel.KeyEvent{Key: rovel.KeyEsc}, nil) {
		t.Fatal("Handle(Esc) returned false")
	}
	if !closed {
		t.Error("onClose not invoked")
	}
}

// TestSelectorMinSize verifies the dimensions reflect the configured
// visible count, not the item list length.
func TestSelectorMinSize(t *testing.T) {
	s := NewSelector()
	s.SetShown(true)
	s.SetItems([]SelectorItem{
		{Name: "a", Description: "short"},
		{Name: "longer-name", Description: "a longer description"},
	})
	s.SetVisible(5)
	sz := s.MinSize()
	// Two items fit without clipping, so no scroll indicator is reserved.
	if sz.H != 2 {
		t.Errorf("MinSize.H = %d, want 2", sz.H)
	}
	if sz.W < 30 {
		t.Errorf("MinSize.W = %d, want >= 30", sz.W)
	}
	if sz.W > 80 {
		t.Errorf("MinSize.W = %d, want <= 80", sz.W)
	}
}

// TestSelectorMinSizeWhenClipped verifies the selector reserves rows for the
// visible item window plus a scroll indicator only when clipped.
func TestSelectorMinSizeWhenClipped(t *testing.T) {
	s := NewSelector()
	s.SetShown(true)
	s.SetVisible(2)
	s.SetItems([]SelectorItem{{Name: "a"}, {Name: "b"}, {Name: "c"}})
	sz := s.MinSize()
	if sz.H != 3 {
		t.Errorf("MinSize.H when clipped = %d, want 3", sz.H)
	}
	if sz.W < 30 {
		t.Errorf("MinSize.W = %d, want >= 30", sz.W)
	}
}

// TestSelectorHiddenHasZeroMinSize verifies a hidden selector reports
// zero dimensions so a parent VStack can hide it without removing it
// from the layout.
func TestSelectorHiddenHasZeroMinSize(t *testing.T) {
	s := NewSelector()
	s.SetItems([]SelectorItem{{Name: "a"}, {Name: "b"}})
	s.SetShown(false)
	sz := s.MinSize()
	if sz.W != 0 || sz.H != 0 {
		t.Errorf("hidden MinSize = %dx%d, want 0x0", sz.W, sz.H)
	}
	if s.Focusable() {
		t.Error("hidden selector should not be focusable")
	}
}

// TestSelectorShownHasFullMinSize verifies showing restores the
// reported dimensions and focus.
func TestSelectorShownHasFullMinSize(t *testing.T) {
	s := NewSelector()
	s.SetItems([]SelectorItem{
		{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"},
	})
	s.SetShown(true)
	sz := s.MinSize()
	if sz.H == 0 {
		t.Errorf("shown MinSize.H = 0, want > 0")
	}
	if !s.Focusable() {
		t.Error("shown selector should be focusable")
	}
}

// TestSelectorPaintRendersItemsWithoutChrome is the visual regression test: it
// paints into a real cell buffer and asserts items and the focus marker appear,
// while selector-owned header/separator chrome and unclipped counters do not.
func TestSelectorPaintRendersItemsWithoutChrome(t *testing.T) {
	theme := rovel.DefaultTheme()
	s := NewSelector()
	s.SetItems([]SelectorItem{
		{Name: "settings", Description: "Open settings menu"},
		{Name: "model", Description: "Select model (opens selector UI)"},
		{Name: "scoped-models", Description: "Enable/disable models for Ctrl+P cycling"},
		{Name: "export", Description: "Export session (HTML default, or specify path: .html/.jsonl)"},
		{Name: "import", Description: "Import and resume a session from a JSONL file"},
	})
	s.SetShown(true)
	s.SetFocused(0)
	s.Layout(geom.Rect{X: 0, Y: 0, W: 80, H: s.MinSize().H})
	paint(s, theme)

	rendered := renderBuffer(80, s.MinSize().H)
	if strings.Contains(rendered, "(1/5)") {
		t.Errorf("rendered output contains unclipped counter:\n%s", rendered)
	}
	firstLine := strings.SplitN(rendered, "\n", 2)[0]
	if strings.HasPrefix(strings.TrimSpace(firstLine), "/") {
		t.Errorf("rendered output contains header row:\n%s", rendered)
	}
	if strings.Contains(rendered, "─") {
		t.Errorf("rendered output contains separator:\n%s", rendered)
	}
	if !strings.Contains(rendered, "settings") {
		t.Errorf("rendered output missing first item name")
	}
	if !strings.Contains(rendered, "Open settings menu") {
		t.Errorf("rendered output missing first item description")
	}
	if !strings.Contains(rendered, "model") {
		t.Errorf("rendered output missing second item name")
	}
	if !strings.Contains(rendered, "→") {
		t.Errorf("rendered output missing focus arrow")
	}
}

// TestSelectorReusedForModelPicker verifies the same component drives
// the model picker. Only model rows should be painted when the list fits.
func TestSelectorPaintRendersScrollIndicatorWhenClipped(t *testing.T) {
	theme := rovel.DefaultTheme()
	s := NewSelector()
	s.SetVisible(2)
	s.SetItems([]SelectorItem{{Name: "a"}, {Name: "b"}, {Name: "c"}})
	s.SetShown(true)
	s.SetFocused(1)
	s.Layout(geom.Rect{X: 0, Y: 0, W: 30, H: s.MinSize().H})
	paint(s, theme)
	rendered := renderBuffer(30, s.MinSize().H)
	if !strings.Contains(rendered, "(2/3)") {
		t.Errorf("rendered output missing clipped scroll indicator:\n%s", rendered)
	}
	lines := strings.Split(rendered, "\n")
	if got, want := lines[2][:7], "  (2/3)"; got != want {
		t.Errorf("scroll indicator prefix = %q, want %q\n%s", got, want, rendered)
	}
}

func TestSelectorReusedForModelPicker(t *testing.T) {
	theme := rovel.DefaultTheme()
	s := NewSelector()
	models := []SelectorItem{
		{Name: "claude-3-5-haiku-20241022", Description: "anthropic"},
		{Name: "claude-3-5-haiku-latest", Description: "anthropic"},
		{Name: "claude-3-5-sonnet-20240620", Description: "anthropic"},
		{Name: "claude-3-5-sonnet-20241022", Description: "anthropic"},
		{Name: "claude-3-7-sonnet-20250219", Description: "anthropic"},
	}
	s.SetItems(models)
	s.SetShown(true)
	s.SetFocused(0)
	s.Layout(geom.Rect{X: 0, Y: 0, W: 80, H: s.MinSize().H})
	paint(s, theme)
	rendered := renderBuffer(80, s.MinSize().H)
	if strings.Contains(rendered, "/model") {
		t.Errorf("model picker rendered selector-owned /model header:\n%s", rendered)
	}
	if strings.Contains(rendered, "─") {
		t.Errorf("model picker rendered selector-owned separator:\n%s", rendered)
	}
	firstLine := strings.SplitN(rendered, "\n", 2)[0]
	if !strings.Contains(firstLine, "→") || !strings.Contains(firstLine, "claude-3-5-haiku-20241022") {
		t.Errorf("model picker first row = %q, want focused model item", firstLine)
	}
	if !strings.Contains(rendered, "claude-3-5-haiku-20241022") {
		t.Errorf("model picker missing first model")
	}
	if !strings.Contains(rendered, "anthropic") {
		t.Errorf("model picker missing provider column")
	}
	if strings.Contains(rendered, "(1/5)") {
		t.Errorf("model picker rendered unclipped counter")
	}
}

// paint drives a view through a real paint pass into a 80x24 cell
// buffer, then snapshots the buffer into a string. The shared helper
// lets selector/container tests verify visual output without spinning
// up the full toolkit loop.
var sharedPaintBuf *render.Buffer

func paint(v rovel.View, theme rovel.Theme) {
	sz := v.Rect()
	if sz.W <= 0 || sz.H <= 0 {
		return
	}
	if sharedPaintBuf == nil || sharedPaintBuf.W != sz.W || sharedPaintBuf.H != sz.H {
		sharedPaintBuf = render.NewBuffer(sz.W, sz.H)
	} else {
		for y := 0; y < sz.H; y++ {
			for x := 0; x < sz.W; x++ {
				sharedPaintBuf.At(x, y).R = 0
				sharedPaintBuf.At(x, y).Style = style.Style{}
			}
		}
	}
	clip := geom.Rect{X: 0, Y: 0, W: sz.W, H: sz.H}
	p := render.NewPainter(sharedPaintBuf, clip, style.Style{})
	d := rovel.NewDrawer(rovel.NewPainter(p, style.Style{}))
	v.Paint(d, &rovel.Ctx{Theme: theme})
}

func renderBuffer(w, h int) string {
	if sharedPaintBuf == nil {
		return ""
	}
	var sb strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			cell := sharedPaintBuf.At(x, y)
			if cell == nil || cell.R == 0 {
				sb.WriteByte(' ')
			} else {
				sb.WriteRune(cell.R)
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func TestSelectorPaintShowsDescriptionWhenLongNameCanYield(t *testing.T) {
	sel := NewSelector()
	sel.SetItems([]SelectorItem{{
		Name:        strings.Repeat("long ", 40),
		Description: "12 3d",
	}})
	sel.SetShown(true)
	sel.Layout(Rect{X: 0, Y: 0, W: 30, H: 1})

	rec := &recordingDrawer{}
	sel.Paint(rec, &Ctx{Theme: DefaultThemeModern()})

	foundMeta := false
	foundEllipsis := false
	for _, line := range rec.lines {
		if line == "12 3d" {
			foundMeta = true
		}
		if strings.HasSuffix(line, "…") {
			foundEllipsis = true
		}
	}
	if !foundMeta || !foundEllipsis {
		t.Fatalf("selector should draw truncated name with ellipsis and metadata; drew %#v", rec.lines)
	}
}

func TestDialogPickerPaintShowsDescriptionForLongName(t *testing.T) {
	picker := NewDialogPicker()
	picker.SetItems([]DialogPickerItem{{
		Name:        strings.Repeat("long ", 40),
		Description: "12 3d",
	}})
	picker.SetShown(true)
	picker.Layout(Rect{X: 0, Y: 0, W: 30, H: 5})

	rec := &recordingDrawer{}
	picker.Paint(rec, &Ctx{Theme: DefaultThemeModern()})

	foundMeta := false
	foundEllipsis := false
	for _, line := range rec.lines {
		if line == "12 3d" {
			foundMeta = true
		}
		if strings.HasSuffix(line, "…") {
			foundEllipsis = true
		}
	}
	if !foundMeta || !foundEllipsis {
		t.Fatalf("dialog picker should draw truncated name with ellipsis and metadata; drew %#v", rec.lines)
	}
}

func TestSelectorPaintKeepsDescriptionNextToName(t *testing.T) {
	sel := NewSelector()
	sel.SetItems([]SelectorItem{
		{Name: "/skill:odin", Description: strings.Repeat("very long description ", 20)},
		{Name: "/skill:tdd", Description: "short"},
	})
	sel.SetShown(true)
	sel.Layout(Rect{X: 0, Y: 0, W: 40, H: 2})

	rec := &recordingDrawer{}
	sel.Paint(rec, &Ctx{Theme: DefaultThemeModern()})

	var names, descs int
	for _, line := range rec.lines {
		switch {
		case strings.HasPrefix(line, "/skill:"):
			names++
		case strings.HasPrefix(line, "very long"):
			descs++
			if !strings.HasSuffix(line, "…") {
				t.Fatalf("long description should be truncated with an ellipsis: %q", line)
			}
		case line == "short":
			descs++
		}
	}
	if names != 2 || descs != 2 {
		t.Fatalf("expected both names and both descriptions drawn, drew %#v", rec.lines)
	}
}
