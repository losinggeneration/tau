package keybindings

import (
	"testing"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
)

func TestDefaultBindingsMatchPiActions(t *testing.T) {
	m := NewDefault(nil)
	cases := []struct {
		key ui.Keystroke
		id  string
	}{
		{ui.Keystroke{Key: event.KeyUp}, "tui.editor.cursorUp"},
		{ui.Keystroke{Key: event.KeyRune, Rune: 'b', Mod: event.ModCtrl}, "tui.editor.cursorLeft"},
		{ui.Keystroke{Key: event.KeyRune, Rune: 'b', Mod: event.ModAlt}, "tui.editor.cursorWordLeft"},
		{ui.Keystroke{Key: event.KeyRune, Rune: 'f', Mod: event.ModAlt}, "tui.editor.cursorWordRight"},
		{ui.Keystroke{Key: event.KeyRune, Rune: 'd', Mod: event.ModCtrl}, "tui.editor.deleteCharForward"},
		{ui.Keystroke{Key: event.KeyRune, Rune: 'w', Mod: event.ModCtrl}, "tui.editor.deleteWordBackward"},
		{ui.Keystroke{Key: event.KeyRune, Rune: 'd', Mod: event.ModAlt}, "tui.editor.deleteWordForward"},
		{ui.Keystroke{Key: event.KeyRune, Rune: 'y', Mod: event.ModAlt}, "tui.editor.yankPop"},
		{ui.Keystroke{Key: event.KeyRune, Rune: '-', Mod: event.ModCtrl}, "tui.editor.undo"},
		{ui.Keystroke{Key: event.KeyEnter}, "tui.input.submit"},
		{ui.Keystroke{Key: event.KeyTab}, "tui.input.tab"},
		{ui.Keystroke{Key: event.KeyEsc}, "tui.select.cancel"},
	}
	for _, tc := range cases {
		if !m.Matches(tc.key, tc.id) {
			t.Fatalf("%+v did not match %s keys=%v", tc.key, tc.id, m.Keys(tc.id))
		}
	}
}

func TestUserBindingsOverrideDisableDedupe(t *testing.T) {
	m := NewDefault(Config{"tui.input.submit": {"ctrl+s", "ctrl+s"}, "tui.select.cancel": {}})
	if !m.Matches(ui.Keystroke{Key: event.KeyRune, Rune: 's', Mod: event.ModCtrl}, "tui.input.submit") {
		t.Fatalf("override missing")
	}
	if m.Matches(ui.Keystroke{Key: event.KeyEnter}, "tui.input.submit") {
		t.Fatalf("default should be replaced")
	}
	if len(m.Keys("tui.input.submit")) != 1 {
		t.Fatalf("dedupe failed")
	}
	if m.Matches(ui.Keystroke{Key: event.KeyEsc}, "tui.select.cancel") {
		t.Fatalf("empty override should disable")
	}
}

func TestConflictsOnlyUserBindings(t *testing.T) {
	m := NewDefault(Config{"tui.input.submit": {"ctrl+x"}, "tui.input.copy": {"ctrl+x"}, "unknown": {"ctrl+x"}})
	c := m.Conflicts()
	if len(c) != 1 || c[0].Key != "ctrl+x" || len(c[0].Keybindings) != 2 {
		t.Fatalf("conflicts=%+v", c)
	}
}

func TestBindingsReturnedAsCopies(t *testing.T) {
	m := NewDefault(Config{"tui.input.submit": {"ctrl+s"}})
	keys := m.Keys("tui.input.submit")
	keys[0] = "changed"
	if m.Keys("tui.input.submit")[0] != "ctrl+s" {
		t.Fatalf("keys aliased")
	}
	user := m.UserBindings()
	user["tui.input.submit"][0] = "changed"
	if m.UserBindings()["tui.input.submit"][0] != "ctrl+s" {
		t.Fatalf("user aliased")
	}
}

func TestDefaultManagerGlobalGetSet(t *testing.T) {
	orig := DefaultManager()
	custom := NewDefault(Config{"tui.input.submit": {"ctrl+s"}})
	SetDefaultManager(custom)
	if DefaultManager() != custom {
		t.Fatalf("default manager not replaced")
	}
	SetDefaultManager(nil)
	if DefaultManager() == nil {
		t.Fatalf("nil manager installed")
	}
	SetDefaultManager(orig)
}

// A binding with a modifier must not match the unmodified key.
//
// The alternate-encoding clauses used to test only Key and ignore Mod, so
// "alt+backspace" matched a bare Backspace — which stole Backspace from the
// prompt editor entirely.
func TestModifiedBindingDoesNotMatchTheBareKey(t *testing.T) {
	m := New(map[string]Definition{
		"app.message.dequeue": {DefaultKeys: []string{"alt+backspace"}},
	}, nil)

	bare := ui.Keystroke{Key: event.KeyBackspace}
	if m.Matches(bare, "app.message.dequeue") {
		t.Error("bare Backspace matched an alt+backspace binding")
	}

	alt := ui.Keystroke{Key: event.KeyBackspace, Mod: event.ModAlt}
	if !m.Matches(alt, "app.message.dequeue") {
		t.Error("alt+backspace did not match its own binding")
	}
}

// The equivalences themselves must survive: terminals spell Backspace as either
// the dedicated key or Ctrl+H, and Ctrl+C as either form.
func TestAlternateEncodingsStillMatch(t *testing.T) {
	m := New(map[string]Definition{
		"back": {DefaultKeys: []string{"backspace"}},
		"quit": {DefaultKeys: []string{"ctrl+c"}},
	}, nil)

	cases := []struct {
		name   string
		key    ui.Keystroke
		action string
	}{
		{"backspace key", ui.Keystroke{Key: event.KeyBackspace}, "back"},
		{"ctrl+h", ui.Keystroke{Key: event.KeyRune, Rune: 'h', Mod: event.ModCtrl}, "back"},
		{"ctrl+c key", ui.Keystroke{Key: event.KeyCtrlC}, "quit"},
		{"ctrl+c rune", ui.Keystroke{Key: event.KeyRune, Rune: 'c', Mod: event.ModCtrl}, "quit"},
	}
	for _, tc := range cases {
		if !m.Matches(tc.key, tc.action) {
			t.Errorf("%s did not match the %q binding", tc.name, tc.action)
		}
	}

	// Ctrl+H folds onto Backspace, so alt+ctrl+h is alt+backspace — and still
	// must not match a bare backspace binding.
	altCtrlH := ui.Keystroke{Key: event.KeyRune, Rune: 'h', Mod: event.ModCtrl | event.ModAlt}
	if m.Matches(altCtrlH, "back") {
		t.Error("alt+ctrl+h matched a bare backspace binding")
	}
}

// TestPiKeybindingsNoEvictionOnRebind ports Pi's keybindings.test.ts
// "does not evict selector confirm when input submit is rebound".
func TestPiKeybindingsNoEvictSelectorWhenInputRebound(t *testing.T) {
	m := NewDefault(Config{
		"tui.input.submit": {"enter", "ctrl+enter"},
	})
	if got, want := m.Keys("tui.input.submit"), []string{"enter", "ctrl+enter"}; !sliceEq(got, want) {
		t.Fatalf("tui.input.submit keys = %#v, want %#v", got, want)
	}
	if got, want := m.Keys("tui.select.confirm"), []string{"enter"}; !sliceEq(got, want) {
		t.Fatalf("tui.select.confirm keys = %#v, want %#v", got, want)
	}
}

// TestPiKeybindingsNoEvictCursorWhenSelectRebindsKey ports Pi's
// "does not evict cursor bindings when another action reuses the same key".
func TestPiKeybindingsNoEvictCursorWhenSelectRebindsKey(t *testing.T) {
	m := NewDefault(Config{
		"tui.select.up": {"up", "ctrl+p"},
	})
	if got, want := m.Keys("tui.select.up"), []string{"up", "ctrl+p"}; !sliceEq(got, want) {
		t.Fatalf("tui.select.up keys = %#v, want %#v", got, want)
	}
	if got, want := m.Keys("tui.editor.cursorUp"), []string{"up"}; !sliceEq(got, want) {
		t.Fatalf("tui.editor.cursorUp keys = %#v, want %#v", got, want)
	}
}

// TestPiKeybindingsReportsDirectUserConflictsWithoutEvictingDefaults
// ports Pi's "still reports direct user binding conflicts without evicting defaults".
func TestPiKeybindingsReportsDirectUserConflictsWithoutEvictingDefaults(t *testing.T) {
	m := NewDefault(Config{
		"tui.input.submit":   {"ctrl+x"},
		"tui.select.confirm": {"ctrl+x"},
	})
	conflicts := m.Conflicts()
	if len(conflicts) != 1 || conflicts[0].Key != "ctrl+x" {
		t.Fatalf("conflicts = %+v, want one conflict on ctrl+x", conflicts)
	}
	if got, want := conflicts[0].Keybindings, []string{"tui.input.submit", "tui.select.confirm"}; !sliceEq(got, want) {
		t.Fatalf("conflict keybindings = %#v, want %#v", got, want)
	}
	if got, want := m.Keys("tui.editor.cursorLeft"), []string{"left", "ctrl+b"}; !sliceEq(got, want) {
		t.Fatalf("tui.editor.cursorLeft keys = %#v, want %#v (should not be evicted)", got, want)
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPiKeybindingsUndefinedUserUsesDefaultAndEmptyDisables(t *testing.T) {
	m := NewDefault(Config{"tui.input.submit": {}, "unknown.action": {"ctrl+x"}})
	if m.Matches(ui.Keystroke{Key: event.KeyEnter}, "tui.input.submit") {
		t.Fatalf("empty user binding should disable submit")
	}
	if !m.Matches(ui.Keystroke{Key: event.KeyTab}, "tui.input.tab") {
		t.Fatalf("undefined user binding should leave default tab binding active")
	}
	if got := m.Keys("unknown.action"); len(got) != 0 {
		t.Fatalf("unknown user binding resolved to %#v, want empty", got)
	}
}

func TestPiKeybindingsConflictsIgnoreDefaultClaims(t *testing.T) {
	m := NewDefault(Config{"tui.input.submit": {"up"}})
	if conflicts := m.Conflicts(); len(conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want no default/user conflict diagnostics", conflicts)
	}
	up := ui.Keystroke{Key: event.KeyUp}
	if !m.Matches(up, "tui.input.submit") || !m.Matches(up, "tui.editor.cursorUp") {
		t.Fatalf("expected user submit and default cursorUp to both resolve up")
	}
}

func TestPiKeybindingsDuplicateUserKeysDedupedInOrder(t *testing.T) {
	m := NewDefault(Config{"tui.select.cancel": {"ctrl+c", "escape", "ctrl+c", "escape"}})
	got := m.Keys("tui.select.cancel")
	want := []string{"ctrl+c", "escape"}
	if len(got) != len(want) {
		t.Fatalf("keys = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %#v, want %#v", got, want)
		}
	}
}

func TestPiKeybindingsConflictActionsAreSorted(t *testing.T) {
	m := NewDefault(Config{"tui.select.cancel": {"ctrl+x"}, "tui.input.copy": {"ctrl+x"}, "tui.input.submit": {"ctrl+x"}})
	conflicts := m.Conflicts()
	if len(conflicts) != 1 || conflicts[0].Key != "ctrl+x" {
		t.Fatalf("conflicts = %+v", conflicts)
	}
	want := []string{"tui.input.copy", "tui.input.submit", "tui.select.cancel"}
	for i := range want {
		if conflicts[0].Keybindings[i] != want[i] {
			t.Fatalf("conflict actions = %#v, want %#v", conflicts[0].Keybindings, want)
		}
	}
}
