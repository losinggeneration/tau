package tui

import (
	"strings"
	"testing"
)

// TestWidgetContainer_AboveEmptyCollapses locks in the default app chrome:
// with no above-editor widgets there should be no spacer row between the chat
// log and prompt editor.
func TestWidgetContainer_AboveEmptyCollapses(t *testing.T) {
	w := NewWidgetContainer(widgetPlacementAbove)
	if sz := w.MinSize(); sz.W != 0 || sz.H != 0 {
		t.Errorf("empty above widget container should collapse, got %+v", sz)
	}
}

func TestWidgetContainer_BelowCollapsesWhenEmpty(t *testing.T) {
	w := NewWidgetContainer(widgetPlacementBelow)
	if sz := w.MinSize(); sz.W != 0 || sz.H != 0 {
		t.Errorf("empty below widget container should have zero MinSize, got %+v", sz)
	}
}

// TestWidgetContainer_GrowsWhenPopulated verifies that setting
// widgets gives the container a real size and that MinSize tracks
// the line count (with the per-widget line cap applied).
func TestWidgetContainer_GrowsWhenPopulated(t *testing.T) {
	w := NewWidgetContainer(widgetPlacementAbove)
	w.SetWidgets(map[string][]string{
		"diff": {"line1", "line2", "line3"},
	})
	sz := w.MinSize()
	if sz.W == 0 || sz.H == 0 {
		t.Errorf("populated widget container should have non-zero MinSize, got %+v", sz)
	}
	// 1 spacer + 3 body lines = 4 rows.
	if sz.H != 4 {
		t.Errorf("populated widget container should be 4 rows, got %d", sz.H)
	}
}

// TestWidgetContainer_CapAtMaxLines ensures a chatty widget can't
// push the editor off-screen. The per-widget line cap kicks in at
// MaxWidgetLines (10); widgets longer than that get a truncation
// marker row instead of the dropped lines.
func TestWidgetContainer_CapAtMaxLines(t *testing.T) {
	w := NewWidgetContainer(widgetPlacementAbove)
	lines := make([]string, 50)
	for i := range lines {
		lines[i] = "line"
	}
	w.SetWidgets(map[string][]string{
		"diff": lines,
	})
	sz := w.MinSize()
	// 1 spacer + MaxWidgetLines body + 1 truncation marker = 12.
	want := 1 + MaxWidgetLines + 1
	if sz.H != want {
		t.Errorf("widget container should be %d rows after cap, got %d", want, sz.H)
	}
}

// TestWidgetContainer_InterWidgetSeparator ensures widgets render in flow with
// no automatic key headers or blank separators: spacer + one line per widget.
func TestWidgetContainer_InterWidgetSeparator(t *testing.T) {
	w := NewWidgetContainer(widgetPlacementAbove)
	w.SetWidgets(map[string][]string{
		"a": {"x"},
		"b": {"y"},
	})
	sz := w.MinSize()
	if sz.H != 3 {
		t.Errorf("two-widget container should be 3 rows, got %d", sz.H)
	}
}

// TestWidgetContainer_NoSeparatorForSingle verifies a single
// widget doesn't get a trailing separator.
func TestWidgetContainer_NoSeparatorForSingle(t *testing.T) {
	w := NewWidgetContainer(widgetPlacementAbove)
	w.SetWidgets(map[string][]string{
		"a": {"x", "y"},
	})
	sz := w.MinSize()
	// 1 header + 2 body = 3. No separator.
	if sz.H != 3 {
		t.Errorf("single-widget container should be 3 rows, got %d", sz.H)
	}
}

// TestWidgetContainer_DetectsWidgetChanges ensures add/remove
// changes the MinSize so the VStack picks up the size delta.
func TestWidgetContainer_DetectsWidgetChanges(t *testing.T) {
	w := NewWidgetContainer(widgetPlacementAbove)
	if w.MinSize().H != 0 {
		t.Errorf("empty above container should collapse")
	}
	w.SetWidgets(map[string][]string{"a": {"x"}})
	if w.MinSize().H == 0 {
		t.Errorf("after SetWidgets, MinSize.H should be non-zero")
	}
	w.SetWidgets(nil)
	if w.MinSize().H != 0 {
		t.Errorf("after SetWidgets(nil), above container should collapse")
	}
}

// TestSetWidget_RoutesByPlacement verifies that the placement
// string picks the right container. setWidget on "above" should
// touch widgetEntriesAbove and widgetsAbove, leaving
// widgetEntriesBelow and widgetsBelow untouched (and vice versa).
func TestSetWidget_RoutesByPlacement(t *testing.T) {
	a := newTestApp(t)
	a.widgetsAbove = NewWidgetContainer(widgetPlacementAbove)
	a.widgetsBelow = NewWidgetContainer(widgetPlacementBelow)
	a.widgetEntriesAbove = nil
	a.widgetEntriesBelow = nil

	a.setWidget("diff", []string{"line1", "line2"}, "above")
	if got := a.widgetEntriesAbove["diff"]; len(got) != 2 || got[0] != "line1" {
		t.Errorf("setWidget(above) didn't populate above entry, got %v", got)
	}
	if got := a.widgetEntriesBelow["diff"]; got != nil {
		t.Errorf("setWidget(above) should not populate below entry, got %v", got)
	}

	a.setWidget("hint", []string{"lineA"}, "below")
	if got := a.widgetEntriesBelow["hint"]; len(got) != 1 || got[0] != "lineA" {
		t.Errorf("setWidget(below) didn't populate below entry, got %v", got)
	}
	if _, present := a.widgetEntriesAbove["hint"]; present {
		t.Errorf("setWidget(below) should not populate above entry, got %v", a.widgetEntriesAbove)
	}
}

// TestSetWidget_NilLinesRemoves ensures passing nil lines (or an
// empty slice) removes the widget rather than rendering a blank
// entry. This matches the spec's "Content omitted on wire when
// clearing" behavior.
func TestSetWidget_NilLinesRemoves(t *testing.T) {
	a := newTestApp(t)
	a.widgetsAbove = NewWidgetContainer(widgetPlacementAbove)
	a.widgetEntriesAbove = nil

	a.setWidget("diff", []string{"x"}, "above")
	if _, ok := a.widgetEntriesAbove["diff"]; !ok {
		t.Fatalf("setup: widget not added")
	}
	a.setWidget("diff", nil, "above")
	if _, ok := a.widgetEntriesAbove["diff"]; ok {
		t.Errorf("setWidget(nil) should remove the entry, got %v", a.widgetEntriesAbove)
	}
}

// TestSetWidget_EmptyPlacementNoop guards against extension bugs
// that pass an unrecognized placement string. The widget should
// not be silently dropped on the wrong side.
func TestSetWidget_EmptyPlacementNoop(t *testing.T) {
	a := newTestApp(t)
	a.widgetsAbove = NewWidgetContainer(widgetPlacementAbove)
	a.widgetsBelow = NewWidgetContainer(widgetPlacementBelow)
	a.widgetEntriesAbove = nil
	a.widgetEntriesBelow = nil

	a.setWidget("diff", []string{"x"}, "")
	if len(a.widgetEntriesAbove) != 0 || len(a.widgetEntriesBelow) != 0 {
		t.Errorf("empty placement should be a no-op, got above=%v below=%v",
			a.widgetEntriesAbove, a.widgetEntriesBelow)
	}
}

// TestSetWidget_CallerCanKeepMutatingInput verifies the Set
// defensively copies the caller's slice. If a caller passes a
// slice they intend to reuse, the container should not see later
// mutations.
func TestSetWidget_CallerCanKeepMutatingInput(t *testing.T) {
	a := newTestApp(t)
	a.widgetsAbove = NewWidgetContainer(widgetPlacementAbove)
	a.widgetEntriesAbove = nil

	lines := []string{"first", "second"}
	a.setWidget("diff", lines, "above")
	// Mutate the caller's slice.
	lines[0] = "MUTATED"
	if got := a.widgetEntriesAbove["diff"][0]; got != "first" {
		t.Errorf("container should have copied the input, got %q", got)
	}
}

// TestWidgetContainer_HandlesLongLines verifies that a single
// widget line longer than the rendered width is truncated by the
// toolkit's text helper rather than wrapping (we don't wrap; we
// truncate). This is more of a paint-path test, but MinSize
// shouldn't go infinite on a long line.
func TestWidgetContainer_HandlesLongLines(t *testing.T) {
	w := NewWidgetContainer(widgetPlacementAbove)
	w.SetWidgets(map[string][]string{
		"diff": {strings.Repeat("a", 1000)},
	})
	if sz := w.MinSize(); sz.W > 250 {
		t.Errorf("MinSize.W should be clamped, got %d", sz.W)
	}
}
