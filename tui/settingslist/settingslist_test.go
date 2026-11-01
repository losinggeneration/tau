package settingslist

import (
	"strings"
	"testing"

	"github.com/losinggeneration/rovel/event"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/keybindings"
)

func TestListCyclesValuesAndReportsChange(t *testing.T) {
	var gotID, gotVal string
	l := New([]Item{{ID: "theme", Label: "Theme", CurrentValue: "dark", Values: []string{"dark", "light"}}}, 5, DefaultTheme(), func(id, v string) { gotID = id; gotVal = v }, func() {}, Options{})
	l.HandleInput("\r")
	if gotID != "theme" || gotVal != "light" {
		t.Fatalf("change=%s %s", gotID, gotVal)
	}
	if l.Items()[0].CurrentValue != "light" {
		t.Fatalf("value not updated")
	}
}

func TestListWrapNavigation(t *testing.T) {
	l := New([]Item{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}, 5, DefaultTheme(), nil, nil, Options{})
	l.HandleInput("\x1b[A")
	if l.SelectedIndex() != 1 {
		t.Fatalf("up wrap selected=%d", l.SelectedIndex())
	}
	l.HandleInput("\x1b[B")
	if l.SelectedIndex() != 0 {
		t.Fatalf("down wrap selected=%d", l.SelectedIndex())
	}
}

func TestListSearchFiltersAndResetsSelection(t *testing.T) {
	l := New([]Item{{ID: "a", Label: "Alpha"}, {ID: "b", Label: "Beta"}, {ID: "g", Label: "Gamma"}}, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	l.HandleInput("g")
	lines := l.Render(80)
	found := false
	for _, line := range lines {
		if line == "> g"+strings.Repeat(" ", 77) {
			continue
		}
		if line == "> Gamma  " {
			found = true
		}
		if line == "Alpha" {
			t.Fatalf("unfiltered alpha rendered: %v", lines)
		}
	}
	if !found {
		t.Fatalf("filtered item not rendered: %v", lines)
	}
	if l.Search() != "g" || l.SelectedIndex() != 0 {
		t.Fatalf("search=%q selected=%d", l.Search(), l.SelectedIndex())
	}
}

func TestListRenderNoBlankLineAfterSearchRow(t *testing.T) {
	l := New([]Item{{ID: "a", Label: "Alpha"}, {ID: "b", Label: "Beta"}}, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	rows := l.Render(80)
	if len(rows) < 2 || rows[1] == "" {
		t.Fatalf("blank line after search row: %#v", rows)
	}
}

func TestListValueChangeKeepsSelection(t *testing.T) {
	l := New([]Item{
		{ID: "a", Label: "A", Values: []string{"1", "2"}},
		{ID: "b", Label: "B", CurrentValue: "x", Values: []string{"x", "y"}},
	}, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	l.HandleInput("\x1b[B") // down to B
	if l.SelectedIndex() != 1 {
		t.Fatalf("after down = %d, want 1", l.SelectedIndex())
	}
	l.HandleInput("\r") // cycle B's value
	if l.SelectedIndex() != 1 {
		t.Fatalf("selection reset to %d after value change, want 1", l.SelectedIndex())
	}
	if l.Items()[1].CurrentValue != "y" {
		t.Fatalf("value not cycled: %q", l.Items()[1].CurrentValue)
	}
}

func TestListCancel(t *testing.T) {
	cancelled := false
	l := New(nil, 5, DefaultTheme(), nil, func() { cancelled = true }, Options{})
	l.HandleInput("\x1b")
	if !cancelled {
		t.Fatalf("cancel not called")
	}
}

func TestListEmptyAndNoMatchesRender(t *testing.T) {
	l := New(nil, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	if got := l.Render(80); len(got) < 1 || got[0] == "" || got[0][:2] != "> " {
		t.Fatalf("empty render=%v", got)
	}
	l = New([]Item{{ID: "a", Label: "Alpha"}}, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	l.HandleInput("z")
	found := false
	for _, line := range l.Render(80) {
		if line == "  No matching settings" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no matches not rendered")
	}
}

type sub struct{ done func(string, bool) }

func (s *sub) Render(width int) []string { return []string{"submenu"} }
func (s *sub) HandleKey(ke event.KeyEvent) {
	if ke.Key == event.KeyRune && ke.Rune == 'x' {
		s.done("new", true)
	}
}

func TestListSubmenuDelegatesAndRestores(t *testing.T) {
	var changed string
	l := New([]Item{{ID: "m", Label: "Model", CurrentValue: "old", Submenu: func(cur string, done func(string, bool)) KeyComponent {
		if cur != "old" {
			t.Fatalf("cur=%q", cur)
		}
		return &sub{done: done}
	}}}, 5, DefaultTheme(), func(id, v string) { changed = id + ":" + v }, nil, Options{})
	l.HandleInput("\r")
	if got := l.Render(80); len(got) != 1 || got[0] != "submenu" {
		t.Fatalf("submenu render=%v", got)
	}
	l.HandleInput("x")
	if changed != "m:new" {
		t.Fatalf("changed=%q", changed)
	}
	if l.Items()[0].CurrentValue != "new" {
		t.Fatalf("value not set")
	}
}

// Tests ported from Pi's settings-list behavior spec.

func TestPiSettingsListEmptyShowsNoSettings(t *testing.T) {
	l := New(nil, 5, DefaultTheme(), nil, nil, Options{})
	lines := l.Render(80)
	found := false
	for _, line := range lines {
		if contains(line, "No settings available") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("empty list should show 'No settings available': %v", lines)
	}
}

func TestPiSettingsListEmptySearchShowsNoMatching(t *testing.T) {
	l := New([]Item{
		{ID: "a", Label: "Alpha", Description: "First"},
		{ID: "b", Label: "Beta", Description: "Second"},
	}, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	l.HandleInput("z")
	lines := l.Render(80)
	found := false
	for _, line := range lines {
		if contains(line, "No matching settings") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("empty search should show 'No matching settings': %v", lines)
	}
}

func TestPiSettingsListUpDownWrap(t *testing.T) {
	l := New([]Item{
		{ID: "a", Label: "Alpha"},
		{ID: "b", Label: "Beta"},
		{ID: "c", Label: "Gamma"},
	}, 5, DefaultTheme(), nil, nil, Options{})
	if l.SelectedIndex() != 0 {
		t.Fatal("initial selection should be 0")
	}
	l.HandleInput("\x1b[B") // down
	if l.SelectedIndex() != 1 {
		t.Fatalf("after down = %d, want 1", l.SelectedIndex())
	}
	l.HandleInput("\x1b[B") // down
	l.HandleInput("\x1b[B") // down → wraps to 0
	if l.SelectedIndex() != 0 {
		t.Fatalf("after down x3 = %d, want 0 (wrap)", l.SelectedIndex())
	}
	l.HandleInput("\x1b[A") // up → wraps to last
	if l.SelectedIndex() != 2 {
		t.Fatalf("after up from first = %d, want 2 (wrap)", l.SelectedIndex())
	}
}

func TestPiSettingsListValueItemCycles(t *testing.T) {
	changed := false
	var changedID, changedVal string
	l := New([]Item{
		{ID: "theme", Label: "Theme", CurrentValue: "dark", Values: []string{"dark", "light", "auto"}},
	}, 5, DefaultTheme(), func(id, val string) {
		changed = true
		changedID = id
		changedVal = val
	}, nil, Options{})
	l.HandleInput("\r") // enter activates
	if !changed || changedID != "theme" || changedVal != "light" {
		t.Fatalf("cycle: changed=%v id=%q val=%q, want theme light", changed, changedID, changedVal)
	}
	// Cycle again to auto
	l.HandleInput("\r")
	if l.Items()[0].CurrentValue != "auto" {
		t.Fatalf("after second cycle = %q, want auto", l.Items()[0].CurrentValue)
	}
	// Cycle again to dark (wrap)
	l.HandleInput("\r")
	if l.Items()[0].CurrentValue != "dark" {
		t.Fatalf("after third cycle = %q, want dark (wrap)", l.Items()[0].CurrentValue)
	}
}

func TestPiSettingsListEscapeCancels(t *testing.T) {
	cancelled := false
	l := New([]Item{{ID: "a", Label: "Alpha"}}, 5, DefaultTheme(), nil, func() {
		cancelled = true
	}, Options{})
	l.HandleInput("\x1b") // escape
	if !cancelled {
		t.Fatal("escape did not cancel")
	}
}

func TestPiSettingsListCtrlCCancels(t *testing.T) {
	cancelled := false
	l := New([]Item{{ID: "a", Label: "Alpha"}}, 5, DefaultTheme(), nil, func() {
		cancelled = true
	}, Options{})
	l.HandleInput("\x03") // ctrl+c
	if !cancelled {
		t.Fatal("ctrl+c did not cancel")
	}
}

func TestPiSettingsListSearchFiltersByLabel(t *testing.T) {
	l := New([]Item{
		{ID: "a", Label: "Alpha Setting", Description: "First"},
		{ID: "b", Label: "Beta Option", Description: "Second"},
		{ID: "c", Label: "Gamma Value", Description: "Third"},
	}, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	l.HandleInput("b")
	items := l.FilteredItems()
	if len(items) != 1 || items[0].ID != "b" {
		t.Fatalf("search 'b' = %+v, want only Beta", items)
	}
}

func TestPiSettingsListSelectedDescriptionWraps(t *testing.T) {
	l := New([]Item{
		{ID: "a", Label: "Test", Description: "This is a long description that should wrap below the list when rendered"},
	}, 5, DefaultTheme(), nil, nil, Options{})
	lines := l.Render(40)
	// Should contain the description somewhere
	found := false
	for _, line := range lines {
		if contains(line, "description") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("description not found in output: %v", lines)
	}
}

func TestPiSettingsListHintLineAppearsAtBottom(t *testing.T) {
	l := New([]Item{
		{ID: "a", Label: "Alpha"},
	}, 5, DefaultTheme(), nil, nil, Options{})
	lines := l.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	lastLine := lines[len(lines)-1]
	// The last line should be a hint (Enter/Space to change, Esc to cancel)
	if !contains(lastLine, "Esc") {
		t.Fatalf("hint line at bottom = %q, should mention Esc", lastLine)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestListSearchUsesFuzzyFilteringAgainstLabels(t *testing.T) {
	l := New([]Item{{ID: "theme", Label: "Theme"}, {ID: "tools", Label: "Tools"}, {ID: "tokens", Label: "Token budget"}}, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	l.HandleInput("t")
	l.HandleInput("b")
	items := l.FilteredItems()
	if len(items) != 1 || items[0].ID != "tokens" {
		t.Fatalf("filtered items = %+v, want token budget via fuzzy tb", items)
	}
	if l.SelectedIndex() != 0 {
		t.Fatalf("selection = %d, want reset to zero", l.SelectedIndex())
	}
}

func TestListSelectedDescriptionWrapsAndHintAlwaysAppears(t *testing.T) {
	l := New([]Item{{ID: "theme", Label: "Theme", Description: "Choose the color theme used by the terminal interface."}}, 5, DefaultTheme(), nil, nil, Options{})
	lines := l.Render(24)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Choose the color") || !strings.Contains(joined, "terminal interface.") {
		t.Fatalf("description not wrapped/rendered: %#v", lines)
	}
	if !strings.Contains(joined, "Enter/Space to change") {
		t.Fatalf("hint missing: %#v", lines)
	}
}

func TestListSearchInputAndBackspace(t *testing.T) {
	l := New([]Item{{ID: "alpha", Label: "Alpha"}, {ID: "beta", Label: "Beta"}}, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	l.HandleInput("b")
	l.HandleInput("e")
	if l.Search() != "be" {
		t.Fatalf("search = %q, want be", l.Search())
	}
	l.HandleInput("\x7f")
	if l.Search() != "b" {
		t.Fatalf("search after backspace = %q, want b", l.Search())
	}
	items := l.FilteredItems()
	if len(items) != 1 || items[0].ID != "beta" {
		t.Fatalf("filtered items = %+v, want beta", items)
	}
}

func TestListValueCyclingWrapsToFirstValue(t *testing.T) {
	var got string
	l := New([]Item{{ID: "mode", Label: "Mode", CurrentValue: "c", Values: []string{"a", "b", "c"}}}, 5, DefaultTheme(), func(_, v string) { got = v }, nil, Options{})
	l.HandleInput(" ")
	if got != "a" || l.Items()[0].CurrentValue != "a" {
		t.Fatalf("cycled to %q/%q, want a", got, l.Items()[0].CurrentValue)
	}
}

func TestSearchInsertsShiftedCharacter(t *testing.T) {
	l := New([]Item{{ID: "plus", Label: "+"}, {ID: "equals", Label: "="}}, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	l.HandleKey(event.KeyEvent{Key: event.KeyRune, Rune: '=', ShiftedRune: '+', Mod: event.ModShift})
	if l.search != "+" {
		t.Fatalf("search=%q", l.search)
	}
}

func TestListSearchPreservesUnicodeFormatCharacters(t *testing.T) {
	value := "👩\u200d💻ش\u200cم\u00a0\U000e0100"
	l := New([]Item{{ID: "joined", Label: value}, {ID: "plain", Label: "other"}}, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	for _, r := range value {
		l.HandleKey(event.KeyEvent{Key: event.KeyRune, Rune: r})
	}
	if l.Search() != value {
		t.Fatalf("search=%q want %q", l.Search(), value)
	}
	items := l.FilteredItems()
	if len(items) != 1 || items[0].ID != "joined" {
		t.Fatalf("filtered=%+v", items)
	}
	for _, key := range []event.KeyEvent{{Key: event.KeyRune, Rune: 27}, {Key: event.KeyRune, Rune: 128}, {Key: event.KeyRune, Rune: '\u200d', Mod: event.ModCtrl}, {Key: event.KeyRune, Rune: '\u200d', Mod: event.ModAlt}} {
		l.HandleKey(key)
	}
	if l.Search() != value {
		t.Fatalf("control/shortcut changed search=%q", l.Search())
	}
}

func TestListSpaceEditsActiveSearchWithoutChangingValue(t *testing.T) {
	var changes int
	l := New([]Item{{ID: "match", Label: "Auto compact", CurrentValue: "off", Values: []string{"off", "on"}}, {ID: "other", Label: "Autocomplete"}}, 5, DefaultTheme(), func(string, string) { changes++ }, nil, Options{EnableSearch: true})
	l.HandleInput(" ")
	if changes != 1 {
		t.Fatalf("empty search Space changes=%d", changes)
	}
	for _, r := range "Auto compact" {
		l.HandleKey(event.KeyEvent{Key: event.KeyRune, Rune: r})
	}
	if l.Search() != "Auto compact" || changes != 1 {
		t.Fatalf("search=%q changes=%d", l.Search(), changes)
	}
	items := l.FilteredItems()
	if len(items) != 1 || items[0].ID != "match" {
		t.Fatalf("filtered=%+v", items)
	}
}

func TestListSearchBackspaceRemovesWholeGrapheme(t *testing.T) {
	for _, value := range []string{"e\u0301", "👩\u200d💻", "🇬🇧", "👍🏽"} {
		l := New(nil, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
		for _, r := range "x" + value {
			l.HandleKey(event.KeyEvent{Key: event.KeyRune, Rune: r})
		}
		l.HandleKey(event.KeyEvent{Key: event.KeyBackspace})
		if l.Search() != "x" {
			t.Fatalf("backspace %q = %q", value, l.Search())
		}
	}
}

func TestListStringInputDecodesChunksAndModifiedText(t *testing.T) {
	l := New([]Item{{ID: "symbol", Label: "+界e\u0301"}}, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	l.HandleInput("\x1b[43u界e\u0301")
	if l.Search() != "+界e\u0301" {
		t.Fatalf("search=%q", l.Search())
	}
	l.HandleInput("\x1b[")
	l.HandleInput("127u")
	if l.Search() != "+界" {
		t.Fatalf("decoded backspace search=%q", l.Search())
	}
	l.HandleInput("\x1b]10;rgb:00/00/00\x07")
	if l.Search() != "+界" {
		t.Fatalf("reply changed search=%q", l.Search())
	}
	for _, b := range []byte("👩\u200d💻") {
		l.HandleInput(string([]byte{b}))
	}
	if l.Search() != "+界👩\u200d💻" {
		t.Fatalf("split UTF-8 search=%q", l.Search())
	}
}

func TestListSearchUsesSingleLineEditingModel(t *testing.T) {
	l := New([]Item{{ID: "a", Label: "alpha beta"}, {ID: "b", Label: "beta alpha"}}, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	l.HandleInput("alpha beta")
	l.HandleInput("\x01")
	l.HandleInput("X")
	if l.Search() != "Xalpha beta" {
		t.Fatalf("home insert=%q", l.Search())
	}
	l.HandleInput("\x1b[45;5u")
	if l.Search() != "alpha beta" {
		t.Fatalf("undo=%q", l.Search())
	}
	l.HandleInput("\x05")
	l.HandleInput("\x17")
	if l.Search() != "alpha " {
		t.Fatalf("word kill=%q", l.Search())
	}
	l.HandleInput("\x19")
	if l.Search() != "alpha beta" {
		t.Fatalf("yank=%q", l.Search())
	}
	l.HandleInput("\x01")
	l.HandleInput("\x1b[200~new\nquery\x1b[201~")
	if l.Search() != "newqueryalpha beta" {
		t.Fatalf("paste=%q", l.Search())
	}
	l.HandleInput("\x1b[45;5u")
	if l.Search() != "alpha beta" {
		t.Fatalf("paste undo=%q", l.Search())
	}
	if len(l.FilteredItems()) != 2 {
		t.Fatalf("filtered=%+v", l.FilteredItems())
	}
}

func TestListSearchRendersScrollingCursor(t *testing.T) {
	l := New(nil, 5, DefaultTheme(), nil, nil, Options{EnableSearch: true})
	l.Paste("abcdefghijklmnop")
	for _, tc := range []struct {
		key, want, before string
		width             int
	}{
		{"", "> lmnop ", "> lmnop", 8},
		{"\x01", "> abcdef", "> ", 8},
		{"\x1b[C\x1b[C\x1b[C\x1b[C\x1b[C\x1b[C\x1b[C\x1b[C", "> fghijk", "> fgh", 8},
	} {
		l.HandleInput(tc.key)
		row := l.Render(tc.width)[0]
		if got := tauui.StripANSI(row); got != tc.want {
			t.Fatalf("row=%q want %q", got, tc.want)
		}
		if !strings.HasPrefix(row, tc.before+tauui.CursorMarker) {
			t.Fatalf("cursor in %q, want after %q", row, tc.before)
		}
	}
	if l.Search() != "abcdefghijklmnop" {
		t.Fatalf("render changed search=%q", l.Search())
	}
	l.HandleInput("\x15\x0b")
	l.Paste("界x")
	l.HandleInput("\x01")
	for _, width := range []int{1, 2, 3, 4, 8} {
		row := l.Render(width)[0]
		if tauui.TextWidth(row) > width {
			t.Fatalf("width %d row=%q", width, row)
		}
	}
}

func TestListUsesConfiguredNavigationAndSearchBindings(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{
		"tui.select.up": {"ctrl+p"}, "tui.select.down": {"ctrl+n"},
		"tui.select.confirm": {"ctrl+s"}, "tui.select.cancel": {"ctrl+q"},
		"tui.editor.cursorLineStart": {"ctrl+o"},
	}))
	var changes, cancels int
	l := New([]Item{{ID: "a", Label: "Alpha", Values: []string{"off", "on"}}, {ID: "b", Label: "Beta", Values: []string{"off", "on"}}}, 5, DefaultTheme(), func(string, string) { changes++ }, func() { cancels++ }, Options{EnableSearch: true})
	l.HandleInput("\x0e")
	if l.SelectedIndex() != 1 {
		t.Fatal("configured down ignored")
	}
	l.HandleInput("\x1b[B\r\x1b")
	if l.SelectedIndex() != 1 || changes != 0 || cancels != 0 {
		t.Fatal("replaced defaults still active")
	}
	l.HandleInput("\x13")
	if changes != 1 || l.Items()[1].CurrentValue != "off" {
		t.Fatalf("confirm changes=%d items=%+v", changes, l.Items())
	}
	l.HandleInput("\x10")
	if l.SelectedIndex() != 0 {
		t.Fatal("configured up ignored")
	}
	l.Paste("Beta")
	l.HandleInput("\x0fX")
	if l.Search() != "XBeta" {
		t.Fatalf("search=%q", l.Search())
	}
	l.HandleInput("\x11")
	if cancels != 1 {
		t.Fatalf("cancels=%d", cancels)
	}
}
