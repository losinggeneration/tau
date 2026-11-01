package keybindings

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
)

func TestActionRouterResolvesThroughManager(t *testing.T) {
	manager := NewDefault(Config{"tui.input.submit": {"ctrl+x"}})
	router := NewActionRouter(manager)

	got, ok := router.Resolve(ui.Keystroke{Key: event.KeyRune, Rune: 'x', Mod: event.ModCtrl})
	if !ok || got != "tui.input.submit" {
		t.Fatalf("Resolve(ctrl+x) = %q, %v; want tui.input.submit, true", got, ok)
	}
}

func TestLoadConfigSupportsWrappedKeybindingsAndDedupe(t *testing.T) {
	cfg, err := LoadConfig(strings.NewReader(`{"keybindings":{"tui.input.submit":["ctrl+x","ctrl+x"]}}`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg["tui.input.submit"]; len(got) != 1 || got[0] != "ctrl+x" {
		t.Fatalf("config = %#v, want deduped ctrl+x", cfg)
	}
}

func TestConflictDiagnosticsAndReservedActions(t *testing.T) {
	manager := NewDefault(Config{
		"tui.input.submit":   {"ctrl+x"},
		"tui.select.confirm": {"ctrl+x"},
	})
	diagnostics := manager.ConflictDiagnostics()
	if len(diagnostics) != 1 || diagnostics[0] != "ctrl+x is assigned to tui.input.submit, tui.select.confirm" {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if err := CanRegisterExtensionBinding("tui.input.submit"); err == nil {
		t.Fatalf("reserved binding accepted")
	}
	if err := CanRegisterExtensionBinding("plugin.example.action"); err != nil {
		t.Fatalf("plugin action rejected: %v", err)
	}
}

// Reserved actions include message copy from the current upstream conflict policy.
var specReservedActions = []string{
	"app.interrupt",
	"app.clear",
	"app.exit",
	"app.suspend",
	"app.thinking.cycle",
	"app.thinking.toggle",
	"app.model.cycleForward",
	"app.model.cycleBackward",
	"app.model.select",
	"app.tools.expand",
	"app.editor.external",
	"app.message.followUp",
	"app.message.copy",

	"tui.input.submit",
	"tui.select.confirm",
	"tui.select.cancel",
	"tui.input.copy",
	"tui.editor.deleteToLineEnd",
}

func TestReservedActionsAreExactlyTheSpecSubset(t *testing.T) {
	want := slices.Clone(specReservedActions)
	slices.Sort(want)

	got := ReservedActions()
	if !slices.Equal(got, want) {
		t.Errorf("reserved actions = %v\nwant %v", got, want)
	}
}

// The old implementation reserved every id in TUIDefinitions. That forbade
// extension shortcuts the spec permits — cursor movement, selection paging —
// while leaving every app action unprotected.
func TestUnreservedToolkitActionsAreNotProtected(t *testing.T) {
	for _, id := range []string{
		"tui.editor.cursorUp",
		"tui.editor.cursorWordLeft",
		"tui.select.up",
		"tui.select.pageDown",
		"tui.editor.undo",
		"tui.input.tab",
	} {
		if IsReservedAction(id) {
			t.Errorf("%q is reserved; §12 protects a subset, and this is not in it", id)
		}
		if err := CanRegisterExtensionBinding(id); err != nil {
			t.Errorf("extension binding for %q rejected: %v", id, err)
		}
	}
}

func TestApplicationActionsAreProtected(t *testing.T) {
	for _, id := range []string{"app.exit", "app.interrupt", "app.model.select"} {
		if !IsReservedAction(id) {
			t.Errorf("%q is not reserved; §12 protects it", id)
		}
	}
	// Picker actions are deliberately shadowable.
	if IsReservedAction("app.scopedModels.provider") {
		t.Error("a picker action is reserved; §12 says picker actions may be shadowed")
	}
}

// ClaimsReservedKey is the form the rule actually takes: extension shortcuts run
// before app keybindings, so the gate has to be about keys, not ids.
func TestClaimsReservedKeyFollowsTheBindings(t *testing.T) {
	manager := New(map[string]Definition{
		"app.exit":            {DefaultKeys: []string{"ctrl+c"}},
		"app.header.expand":   {DefaultKeys: []string{"ctrl+o"}},
		"tui.editor.cursorUp": {DefaultKeys: []string{"up"}},
	}, nil)

	cases := map[string]struct {
		k    ui.Keystroke
		want bool
	}{
		"ctrl+c is app.exit":                 {ui.Keystroke{Key: event.KeyCtrlC}, true},
		"ctrl+o is not reserved":             {ui.Keystroke{Key: event.KeyRune, Rune: 'o', Mod: event.ModCtrl}, false},
		"up is an unreserved toolkit action": {ui.Keystroke{Key: event.KeyUp}, false},
		"ctrl+g is unbound":                  {ui.Keystroke{Key: event.KeyRune, Rune: 'g', Mod: event.ModCtrl}, false},
	}
	for name, tc := range cases {
		if got := manager.ClaimsReservedKey(tc.k); got != tc.want {
			t.Errorf("%s: ClaimsReservedKey = %v, want %v", name, got, tc.want)
		}
	}
}

// A user rebinding a reserved action moves the protection with it.
func TestClaimsReservedKeyFollowsUserRebinds(t *testing.T) {
	manager := New(map[string]Definition{
		"app.exit": {DefaultKeys: []string{"ctrl+c"}},
	}, Config{"app.exit": {"ctrl+q"}})

	if manager.ClaimsReservedKey(ui.Keystroke{Key: event.KeyCtrlC}) {
		t.Error("ctrl+c still protected after the user rebound app.exit away from it")
	}
	if !manager.ClaimsReservedKey(ui.Keystroke{Key: event.KeyRune, Rune: 'q', Mod: event.ModCtrl}) {
		t.Error("ctrl+q is not protected after the user bound app.exit to it")
	}
}

// A nil manager must not report everything as reserved, which would disable
// extension shortcuts wholesale.
func TestNilManagerClaimsNothing(t *testing.T) {
	var manager *Manager
	if manager.ClaimsReservedKey(ui.Keystroke{Key: event.KeyCtrlC}) {
		t.Error("a nil manager claimed a reserved key")
	}
}

func TestLoadConfigAcceptsScalarBindingsAndSkipsInvalidEntries(t *testing.T) {
	const body = `{"tui.input.submit":"ctrl+x","tui.select.up":["ctrl+k","ctrl+k"],"tui.select.down":[],"number":42,"null":null,"object":{},"mixed":["enter",false],"$schema":"https://example.test/schema"}`
	for _, raw := range []string{body, "\ufeff" + body, `{"keybindings":` + body + `}`} {
		cfg, err := LoadConfig(strings.NewReader(raw))
		require.NoError(t, err)
		assert.Equal(t, Config{"tui.input.submit": {"ctrl+x"}, "tui.select.up": {"ctrl+k"}, "tui.select.down": {}}, cfg)
		manager := NewDefault(cfg)
		assert.True(t, manager.Matches(ui.Keystroke{Key: event.KeyRune, Rune: 'x', Mod: event.ModCtrl}, "tui.input.submit"))
		assert.Empty(t, manager.Keys("tui.select.down"))
		assert.Equal(t, []string{"escape", "ctrl+c"}, manager.Keys("tui.select.cancel"))
	}
}

func TestLoadConfigRejectsTrailingJSON(t *testing.T) {
	for _, raw := range []string{`{} {}`, `{} garbage`, `{"tui.input.submit":`} {
		_, err := LoadConfig(strings.NewReader(raw))
		assert.Error(t, err)
	}
}

func TestLoadConfigIgnoresInvalidKeyNamesWithoutDisablingDefaults(t *testing.T) {
	for _, value := range []string{`"not-a-key"`, `"ctrl+ctrl+x"`, `["ctrl+x","not-a-key"]`, `["ctrl+x",null]`, `""`} {
		cfg, err := LoadConfig(strings.NewReader(`{"tui.input.submit":` + value + `,"tui.select.up":"ctrl+k"}`))
		require.NoError(t, err)
		assert.Equal(t, Config{"tui.select.up": {"ctrl+k"}}, cfg)
		assert.Equal(t, []string{"enter"}, NewDefault(cfg).Keys("tui.input.submit"))
	}
	cfg, err := LoadConfig(strings.NewReader(`{"tui.input.submit":["ctrl+x","shift+enter"],"tui.select.down":[]}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"ctrl+x", "shift+enter"}, cfg["tui.input.submit"])
	assert.Empty(t, NewDefault(cfg).Keys("tui.select.down"))
}

func TestLoadConfigMigratesLegacyActionNames(t *testing.T) {
	for _, tc := range []struct{ legacy, current string }{
		{"cursorWordLeft", "tui.editor.cursorWordLeft"}, {"undo", "tui.editor.undo"}, {"newLine", "tui.input.newLine"}, {"selectCancel", "tui.select.cancel"}, {"cycleModelBackward", "app.model.cycleBackward"}, {"deleteSessionNoninvasive", "app.session.deleteNoninvasive"},
	} {
		cfg, err := LoadConfig(strings.NewReader(`{"` + tc.legacy + `":"ctrl+k"}`))
		require.NoError(t, err)
		assert.Equal(t, Config{tc.current: {"ctrl+k"}}, cfg)
		cfg, err = LoadConfig(strings.NewReader(`{"` + tc.legacy + `":"ctrl+k","` + tc.current + `":[]}`))
		require.NoError(t, err)
		assert.Equal(t, Config{tc.current: {}}, cfg)
		cfg, err = LoadConfig(strings.NewReader(`{"` + tc.legacy + `":"ctrl+k","` + tc.current + `":false}`))
		require.NoError(t, err)
		assert.Empty(t, cfg)
	}
	cfg, err := LoadConfig(strings.NewReader(`{"customAction":"ctrl+k","app.message.copy":"ctrl+y"}`))
	require.NoError(t, err)
	assert.Equal(t, Config{"customAction": {"ctrl+k"}, "app.message.copy": {"ctrl+y"}}, cfg)
}
