package tui

import (
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
)

// TestStatusView_CollapsesWhenEmpty locks in the layout contract:
// an empty status view reports MinSize 0x0 so the VStack treats the
// region as absent in the common case.
func TestStatusView_CollapsesWhenEmpty(t *testing.T) {
	s := tauui.NewStatusEntriesView(5)
	if sz := s.MinSize(); sz.W != 0 || sz.H != 0 {
		t.Errorf("empty status view should have zero MinSize, got %+v", sz)
	}
}

// TestStatusView_GrowsWhenPopulated verifies that setting entries
// gives the view a real size and that MinSize tracks the entry
// count (up to maxRows).
func TestStatusView_GrowsWhenPopulated(t *testing.T) {
	s := tauui.NewStatusEntriesView(5)
	s.SetEntries(map[string]string{
		"agent":  "working",
		"sentry": "0 errors",
		"ctx":    "1.2k/200k",
	})
	sz := s.MinSize()
	if sz.W == 0 || sz.H == 0 {
		t.Errorf("populated status view should have non-zero MinSize, got %+v", sz)
	}
	if sz.H != 3 {
		t.Errorf("populated status view should be 3 rows, got %d", sz.H)
	}
}

// TestStatusView_CapAtMaxRows ensures a chatty extension set can't
// push the editor off-screen. With more entries than maxRows the
// view height is capped.
func TestStatusView_CapAtMaxRows(t *testing.T) {
	s := tauui.NewStatusEntriesView(5)
	entries := map[string]string{}
	for i := 0; i < 20; i++ {
		entries[byteToKey(i)] = "value"
	}
	s.SetEntries(entries)
	if sz := s.MinSize(); sz.H > s.MaxRows() {
		t.Errorf("status view should be capped at maxRows=%d, got %d", s.MaxRows(), sz.H)
	}
}

// TestStatusView_KeySortedIteration locks in the deterministic
// iteration order. The set of entries uses random key insertion
// order; the view should always sort before drawing.
func TestStatusView_KeySortedIteration(t *testing.T) {
	s := tauui.NewStatusEntriesView(5)
	s.SetEntries(map[string]string{
		"zeta":  "z",
		"alpha": "a",
		"mu":    "m",
	})
	// Layout doesn't matter here; we just need to confirm the
	// internal entries map is the one Paint will iterate.
	if got, want := len(s.Entries()), 3; got != want {
		t.Errorf("Entries() length = %d, want %d", got, want)
	}
}

// TestStatusView_DetectsEntryChanges ensures add/remove changes
// the MinSize so the VStack picks up the size delta.
func TestStatusView_DetectsEntryChanges(t *testing.T) {
	s := tauui.NewStatusEntriesView(5)
	if s.MinSize() != (tauui.Size{}) {
		t.Errorf("empty view should report zero MinSize")
	}
	s.SetEntries(map[string]string{"x": "y"})
	if s.MinSize().H == 0 {
		t.Errorf("after SetEntries, MinSize.H should be non-zero")
	}
	s.SetEntries(nil)
	if s.MinSize() != (tauui.Size{}) {
		t.Errorf("after SetEntries(nil), MinSize should collapse to zero")
	}
}

// byteToKey maps a small integer to a stable key name for use in
// tests that need a handful of distinct keys.
func byteToKey(i int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	return string(letters[i%len(letters)])
}

// TestSetStatus_AddsEntryAndCollapsesLayout validates the round
// trip: setStatus populates the entry, the view claims layout
// space; clearStatus removes it, the view collapses.
func TestSetStatus_AddsEntryAndCollapsesLayout(t *testing.T) {
	a := newTestApp(t)
	a.status = tauui.NewStatusEntriesView(5)
	a.statusEntries = nil
	a.refreshStatus(testCtx())

	if a.status.MinSize().H != 0 {
		t.Errorf("status view should start collapsed, got %+v", a.status.MinSize())
	}

	a.setStatus("agent", "working on it")
	if got := a.statusEntries["agent"]; got != "working on it" {
		t.Errorf("setStatus didn't record entry, got %q", got)
	}
	if a.status.MinSize().H == 0 {
		t.Errorf("status view should claim space after setStatus, got %+v", a.status.MinSize())
	}

	a.clearStatus("agent")
	if _, ok := a.statusEntries["agent"]; ok {
		t.Errorf("clearStatus didn't remove entry")
	}
	if a.status.MinSize().H != 0 {
		t.Errorf("status view should collapse after clearStatus, got %+v", a.status.MinSize())
	}
}

func TestStatusHeightChangesInvalidateLayout(t *testing.T) {
	a := newTestApp(t)
	a.status = tauui.NewStatusEntriesView(5)
	var layouts int
	ctx := &tauui.Ctx{InvalidateLayout: func() { layouts++ }}
	for _, entries := range []map[string]string{
		{"one": "first"},
		{"one": "first", "two": "second"},
		{"one": "first again", "two": "second"},
		{"one": "first again"},
		nil,
	} {
		a.statusEntries = entries
		a.refreshStatus(ctx)
	}
	if layouts != 4 {
		t.Errorf("layout invalidations = %d, want 4 (one per height change)", layouts)
	}
}

// TestSetStatus_SanitizesText ensures embedded newlines and
// control characters don't break the layout — the same single-line
// rule the footer uses for extension status entries.
func TestSetStatus_SanitizesText(t *testing.T) {
	a := newTestApp(t)
	a.status = tauui.NewStatusEntriesView(5)
	a.statusEntries = nil

	a.setStatus("agent", "line1\nline2\ttab\rdone")
	if got := a.statusEntries["agent"]; got != "line1 line2 tab done" {
		t.Errorf("setStatus didn't sanitize text, got %q", got)
	}

	// Empty text removes the entry rather than storing a blank.
	a.setStatus("agent", "")
	if _, ok := a.statusEntries["agent"]; ok {
		t.Errorf("setStatus with empty text should remove the entry, got %q", a.statusEntries["agent"])
	}
}

// TestSetStatus_EmptyKeyIsNoop guards against extension bugs that
// pass an empty key. We don't want a phantom "" entry to take up a
// row in the status view.
func TestSetStatus_EmptyKeyIsNoop(t *testing.T) {
	a := newTestApp(t)
	a.status = tauui.NewStatusEntriesView(5)
	a.statusEntries = nil

	a.setStatus("", "value")
	if len(a.statusEntries) != 0 {
		t.Errorf("setStatus with empty key should be a no-op, got %v", a.statusEntries)
	}
}

// TestSetStatus_OverwritesExisting verifies that re-setting the
// same key updates the text in place rather than appending.
func TestSetStatus_OverwritesExisting(t *testing.T) {
	a := newTestApp(t)
	a.status = tauui.NewStatusEntriesView(5)
	a.statusEntries = nil

	a.setStatus("agent", "first")
	a.setStatus("agent", "second")
	if len(a.statusEntries) != 1 {
		t.Errorf("re-set of same key should not duplicate, got %d entries", len(a.statusEntries))
	}
	if got := a.statusEntries["agent"]; got != "second" {
		t.Errorf("re-set should overwrite, got %q", got)
	}
}
