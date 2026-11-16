package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/keybindings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ctrl(r rune) tauui.Keystroke {
	return tauui.Keystroke{Key: tauui.KeyRune, Rune: r, Mod: tauui.ModCtrl}
}

// app.chat.scrollBottom ships on Ctrl+End. The editor owns plain End (end of
// line), so the chord has to resolve in the text-input context too — the
// editor has focus virtually always, and jumping to the bottom of the chat
// while typing is the common case.
func TestChatScrollBottomResolvesInBothContexts(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}
	ks := tauui.Keystroke{Key: tauui.KeyEnd, Mod: tauui.ModCtrl}

	for _, ctx := range []tauui.KeyContext{tauui.KeyCtxGlobal, tauui.KeyCtxTextInput} {
		got, ok := km.Resolve(ctx, ks)
		if !ok || got != AppActionChatScrollBottom {
			t.Errorf("ctrl+end in ctx %v = (%v, %v), want AppActionChatScrollBottom", ctx, got, ok)
		}
	}
	// Plain End stays with the editor's ActionEnd.
	if got, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyEnd}); ok && got == AppActionChatScrollBottom {
		t.Error("plain End was claimed by chat scroll; it must stay the editor's line-end")
	}
}

// Every registered action id must have a Go action behind it and a definition
// in front of it. A mismatch means a key that resolves to nothing, or a
// documented binding no handler can receive.
func TestAppActionRegistryIsConsistent(t *testing.T) {
	defs := appKeybindingDefinitions()
	actions := appActions()

	for id := range defs {
		if _, ok := actions[id]; !ok {
			t.Errorf("action id %q has a keybinding definition but no dispatchable action", id)
		}
	}
	for id := range actions {
		if _, ok := defs[id]; !ok {
			t.Errorf("action id %q is dispatchable but has no keybinding definition", id)
		}
	}
}

// The defaults must reproduce the bindings the hardcoded keymap used, or this
// change silently moved everyone's keys.
func TestDefaultBindingsMatchTheKeysTheyReplaced(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	cases := []struct {
		name string
		ks   tauui.Keystroke
		want tauui.Action
	}{
		{"ctrl+c", tauui.Keystroke{Key: tauui.KeyCtrlC}, AppActionExit},
		{"ctrl+c as rune", ctrl('c'), AppActionExit},
		{"ctrl+p", ctrl('p'), AppActionModelCycleForward},
		{
			"ctrl+shift+p",
			tauui.Keystroke{Key: tauui.KeyRune, Rune: 'p', Mod: tauui.ModCtrl | tauui.ModShift},
			AppActionModelCycleBackward,
		},
	}
	for _, tc := range cases {
		got, ok := km.Resolve(tauui.KeyCtxTextInput, tc.ks)
		if !ok || got != tc.want {
			t.Errorf("%s = (%v, %v), want (%v, true)", tc.name, got, ok, tc.want)
		}
	}
}

// The old keymap accepted an uppercase rune for Ctrl+<letter> because
// terminals disagree about how they report it. That tolerance must survive.
func TestUppercaseRunesStillResolve(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	got, ok := km.Resolve(tauui.KeyCtxTextInput, ctrl('P'))
	if !ok || got != AppActionModelCycleForward {
		t.Fatalf("Ctrl+P (uppercase rune) = (%v, %v), want model cycle forward", got, ok)
	}
}

// Ctrl+P is bound to two actions. Which one wins depends on whether the
// scoped-models dialog is open; resolving it to the wrong one — or to neither —
// is the failure mode a naive first-match router produces.
func TestSharedKeyResolvesByContext(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	inDialog := func(id string) bool { _, ok := scopedModelsActionIDs[id]; return ok }
	got, ok := km.resolveAppAction(ctrl('p'), inDialog)
	if !ok || got != AppActionScopedModelsProvider {
		t.Errorf("ctrl+p inside the dialog = (%v, %v), want scoped-models provider", got, ok)
	}

	outside := func(id string) bool { _, ok := scopedModelsActionIDs[id]; return !ok }
	got, ok = km.resolveAppAction(ctrl('p'), outside)
	if !ok || got != AppActionModelCycleForward {
		t.Errorf("ctrl+p outside the dialog = (%v, %v), want model cycle forward", got, ok)
	}
}

// Alt+Up is app.message.dequeue in the editor, matching Pi. It used to be
// Alt+Backspace, which the editor also binds to delete-word-backward.
func TestDequeueBindsAltUpInTheEditor(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	got, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyUp, Mod: tauui.ModAlt})
	if !ok || got != AppActionMessageDequeue {
		t.Errorf("alt+up in the editor = (%v, %v), want dequeue", got, ok)
	}
	if got, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyBackspace, Mod: tauui.ModAlt}); ok && got == AppActionMessageDequeue {
		t.Error("alt+backspace still resolves to dequeue; it belongs to the editor's word delete")
	}
}

// Ctrl+Z suspends the app. Raw mode turns ISIG off, so Ctrl+Z arrives as a
// keystroke, not SIGTSTP; without this binding it falls through every layer
// and is swallowed. It must resolve in every context — editor, global, and
// overlay — because suspending from a dialog or while focus is elsewhere is
// still the conventional behavior.
func TestCtrlZSuspendsInEveryContext(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	for _, ctx := range []tauui.KeyContext{tauui.KeyCtxTextInput, tauui.KeyCtxGlobal, tauui.KeyCtxOverlay} {
		got, ok := km.Resolve(ctx, ctrl('z'))
		if !ok || got != AppActionSuspend {
			t.Errorf("ctrl+z in context %d = (%v, %v), want suspend", ctx, got, ok)
		}
	}
}

// Terminals disagree about whether Ctrl+Z arrives with a lowercase or
// uppercase rune; both spellings must suspend, matching how the other
// Ctrl+letter bindings tolerate the uppercase form.
func TestUppercaseCtrlZStillSuspends(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	got, ok := km.Resolve(tauui.KeyCtxTextInput, ctrl('Z'))
	if !ok || got != AppActionSuspend {
		t.Fatalf("Ctrl+Z (uppercase rune) = (%v, %v), want suspend", got, ok)
	}
}

// The point of the registry: a user file rebinds an action, and the new key
// works while the old one stops.
func TestUserConfigRebindsAnAction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keybindings.json")
	if err := os.WriteFile(path, []byte(`{"keybindings":{"app.model.cycleForward":["ctrl+n"]}}`), 0o600); err != nil {
		t.Fatalf("write keybindings: %v", err)
	}

	manager, err := newAppKeybindingManager(path)
	if err != nil {
		t.Fatalf("newAppKeybindingManager: %v", err)
	}
	a := newAppWithSelector(t)
	km := appKeymap{app: a, manager: manager}

	got, ok := km.Resolve(tauui.KeyCtxTextInput, ctrl('n'))
	if !ok || got != AppActionModelCycleForward {
		t.Errorf("rebound ctrl+n = (%v, %v), want model cycle forward", got, ok)
	}
	if got, ok := km.Resolve(tauui.KeyCtxTextInput, ctrl('p')); ok && got == AppActionModelCycleForward {
		t.Error("ctrl+p still cycles the model after being rebound away")
	}
}

// An empty binding list disables the action (spec §12).
func TestEmptyBindingDisablesAnAction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keybindings.json")
	if err := os.WriteFile(path, []byte(`{"keybindings":{"app.model.cycleForward":[]}}`), 0o600); err != nil {
		t.Fatalf("write keybindings: %v", err)
	}

	manager, err := newAppKeybindingManager(path)
	if err != nil {
		t.Fatalf("newAppKeybindingManager: %v", err)
	}
	a := newAppWithSelector(t)
	km := appKeymap{app: a, manager: manager}

	if got, ok := km.Resolve(tauui.KeyCtxTextInput, ctrl('p')); ok && got == AppActionModelCycleForward {
		t.Errorf("disabled action still resolves: (%v, %v)", got, ok)
	}
}

// A missing file is the normal case, not an error.
func TestMissingKeybindingsFileIsNotAnError(t *testing.T) {
	manager, err := newAppKeybindingManager(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing keybindings file reported an error: %v", err)
	}
	if manager == nil {
		t.Fatal("missing keybindings file produced a nil manager")
	}
	if !manager.Matches(tauui.Keystroke{Key: tauui.KeyCtrlC}, actionIDExit) {
		t.Error("defaults were not applied when the config file is absent")
	}
}

// A malformed file must report, not panic and not silently vanish — the caller
// surfaces the error so the user knows their config was ignored.
func TestMalformedKeybindingsFileReportsAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keybindings.json")
	if err := os.WriteFile(path, []byte(`{ not json`), 0o600); err != nil {
		t.Fatalf("write keybindings: %v", err)
	}

	manager, err := newAppKeybindingManager(path)
	if err == nil {
		t.Fatal("malformed keybindings file did not report an error")
	}
	if manager == nil {
		t.Fatal("malformed keybindings file produced a nil manager instead of defaults")
	}
	if !manager.Matches(tauui.Keystroke{Key: tauui.KeyCtrlC}, actionIDExit) {
		t.Error("fallback manager does not carry the defaults")
	}
}

// The app registry must not be resolved against the toolkit's default manager:
// that one has no app.* ids at all, so every app key would fall through.
func TestZeroValueKeymapUsesAppDefaultsNotToolkitDefaults(t *testing.T) {
	// Premise: the toolkit registry knows nothing about app.exit, so routing
	// through it would drop Ctrl+C entirely.
	if keybindings.DefaultManager().Matches(tauui.Keystroke{Key: tauui.KeyCtrlC}, actionIDExit) {
		t.Fatal("toolkit DefaultManager now binds app.exit; this test's premise is stale")
	}

	a := newAppWithSelector(t)
	km := appKeymap{app: a} // no manager
	got, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyCtrlC})
	if !ok || got != AppActionExit {
		t.Fatalf("zero-value keymap resolved ctrl+c to (%v, %v), want app exit", got, ok)
	}
}

// A hint that still shows the shipped default after a rebind is a lie, and
// /hotkeys is the one place a user goes to check what their keys are.
func TestHotkeysFollowUserRebinds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keybindings.json")
	if err := os.WriteFile(path, []byte(`{"keybindings":{"app.model.cycleForward":["ctrl+n"]}}`), 0o600); err != nil {
		t.Fatalf("write keybindings: %v", err)
	}
	manager, err := newAppKeybindingManager(path)
	if err != nil {
		t.Fatalf("newAppKeybindingManager: %v", err)
	}

	app := newTestApp(t)
	app.keybindings = manager
	app.conv.Clear()
	cmdHotkeys(app, "")

	text := app.conv.Messages()[0].Text
	if !strings.Contains(text, "ctrl+n") {
		t.Errorf("/hotkeys does not show the rebound key:\n%s", text)
	}
	if strings.Contains(text, "ctrl+p ") {
		t.Errorf("/hotkeys still advertises the replaced default:\n%s", text)
	}
}

// An action bound to nothing must not be listed with a blank key column.
func TestHotkeysOmitDisabledActions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keybindings.json")
	if err := os.WriteFile(path, []byte(`{"keybindings":{"app.model.cycleForward":[]}}`), 0o600); err != nil {
		t.Fatalf("write keybindings: %v", err)
	}
	manager, err := newAppKeybindingManager(path)
	if err != nil {
		t.Fatalf("newAppKeybindingManager: %v", err)
	}

	app := newTestApp(t)
	app.keybindings = manager
	app.conv.Clear()
	cmdHotkeys(app, "")

	text := app.conv.Messages()[0].Text
	if strings.Contains(text, "Cycle to the next model") {
		t.Errorf("/hotkeys lists a disabled action:\n%s", text)
	}
}

func TestKeybindingsPathSitsBesideSettings(t *testing.T) {
	if got := appKeybindingsPath("/home/u/.tau/agent/settings.json"); got != "/home/u/.tau/agent/keybindings.json" {
		t.Errorf("appKeybindingsPath = %q", got)
	}
	if got := appKeybindingsPath(""); got != "" {
		t.Errorf("empty settings path should yield no keybindings path, got %q", got)
	}
}

// Backspace must reach the editor. It briefly did not: the app registry binds
// alt+backspace to app.message.dequeue, and the toolkit's keystroke matcher
// ignored modifiers on its backspace equivalence, so a bare Backspace resolved
// to dequeue and the editor never saw the key.
func TestPlainBackspaceReachesTheEditor(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyBackspace})
	if ok && act == AppActionMessageDequeue {
		t.Fatal("plain Backspace resolves to dequeue; the editor never receives it")
	}
}

// Ctrl+H is how some terminals spell Backspace, so it must reach the editor too.
func TestCtrlHReachesTheEditor(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	ks := tauui.Keystroke{Key: tauui.KeyRune, Rune: 'h', Mod: tauui.ModCtrl}
	if act, ok := km.Resolve(tauui.KeyCtxTextInput, ks); ok && act == AppActionMessageDequeue {
		t.Fatal("Ctrl+H resolves to dequeue; it is Backspace and belongs to the editor")
	}
}

// Editing keys must never be captured by an app action. This is the general
// version of the bug: any typing or editing key that resolves to an app action
// is a key the editor cannot see.
func TestEditingKeysAreNotCapturedByAppActions(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}
	appActionIDs := appActions()

	keys := map[string]tauui.Keystroke{
		"backspace": {Key: tauui.KeyBackspace},
		"delete":    {Key: tauui.KeyDelete},
		"left":      {Key: tauui.KeyLeft},
		"right":     {Key: tauui.KeyRight},
		"home":      {Key: tauui.KeyHome},
		"end":       {Key: tauui.KeyEnd},
		"space":     {Key: tauui.KeyRune, Rune: ' '},
		"letter a":  {Key: tauui.KeyRune, Rune: 'a'},
		"digit 1":   {Key: tauui.KeyRune, Rune: '1'},
	}
	for name, ks := range keys {
		act, ok := km.Resolve(tauui.KeyCtxTextInput, ks)
		if !ok {
			continue
		}
		for id, appAct := range appActionIDs {
			if act == appAct {
				t.Errorf("%s resolves to the app action %q; the editor will never see it", name, id)
			}
		}
	}
}

func TestAppKeymapRoutesShiftedProducedSymbol(t *testing.T) {
	manager := keybindings.New(appKeybindingDefinitions(), keybindings.Config{actionIDModelCycleForward: {"ctrl++"}})
	km := appKeymap{app: newAppWithSelector(t), manager: manager}
	got, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyRune, Rune: '=', ShiftedRune: '+', Mod: tauui.ModCtrl | tauui.ModShift})
	require.True(t, ok)
	assert.Equal(t, AppActionModelCycleForward, got)
}

func TestNativeEditorHonorsToolkitRebindAndDisable(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.editor.deleteToLineEnd": {"f2"}, "tui.editor.deleteCharBackward": {}}))
	a := newAppWithSelector(t)
	m := a.editor.Model()
	m.SetText("one two")
	m.SetCursor(4)
	km := appKeymap{app: a}
	action, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyF2})
	require.True(t, ok)
	require.True(t, a.editor.HandleAction(action, &tauui.Ctx{FocusedID: a.editor.ID()}))
	assert.Equal(t, "one ", m.Text())
	_, ok = km.Resolve(tauui.KeyCtxTextInput, ctrl('k'))
	assert.False(t, ok)
	_, ok = km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyBackspace})
	assert.False(t, ok, "disabled key must not fall back to standard keymap")
}

func TestNativeEditorUsesConfiguredSubmitAndNewline(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{
		"tui.input.submit": {"f2"}, "tui.input.newLine": {"f3"},
	}))
	a := newAppWithSelector(t)
	km := appKeymap{app: a}
	action, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyF2})
	assert.True(t, ok)
	assert.Equal(t, AppActionEditorSubmit, action)
	action, ok = km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyF3})
	assert.True(t, ok)
	assert.Equal(t, tauui.ActionSubmit, action)
	require.True(t, a.editor.HandleAction(action, &tauui.Ctx{FocusedID: a.editor.ID()}))
	assert.Equal(t, "\n", a.editor.Text())
	for _, key := range []tauui.Keystroke{{Key: tauui.KeyEnter}, {Key: tauui.KeyEnter, Mod: tauui.ModShift}} {
		_, ok = km.Resolve(tauui.KeyCtxTextInput, key)
		assert.False(t, ok, "removed Enter binding %+v", key)
	}
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.input.submit": {}, "tui.input.newLine": {}}))
	_, ok = km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyEnter})
	assert.False(t, ok)
}

func TestInlineSelectorUsesConfiguredToolkitKeys(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{
		"tui.select.up": {"f2"}, "tui.select.down": {"f3"}, "tui.select.pageUp": {"f4"}, "tui.select.pageDown": {"f5"},
		"tui.select.confirm": {"f6"}, "tui.select.cancel": {"f7"}, "tui.input.tab": {"f8"},
		"tui.editor.cursorUp": {}, "tui.editor.cursorDown": {}, "tui.editor.pageUp": {}, "tui.editor.pageDown": {}, "tui.input.submit": {},
	}))
	a := newAppWithSelector(t)
	a.refreshSelector("/", nil)
	require.True(t, a.selectorVisible())
	km := appKeymap{app: a}
	for _, tc := range []struct {
		key  tauui.Key
		want tauui.Action
	}{
		{tauui.KeyF2, AppActionSelectorUp},
		{tauui.KeyF3, AppActionSelectorDown},
		{tauui.KeyF4, AppActionSelectorPageUp},
		{tauui.KeyF5, AppActionSelectorPageDown},
		{tauui.KeyF6, AppActionSelectorActivate},
		{tauui.KeyF7, tauui.ActionCancel},
		{tauui.KeyF8, AppActionEditorComplete},
	} {
		action, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tc.key})
		assert.True(t, ok)
		assert.Equal(t, tc.want, action)
	}
	for _, key := range []tauui.Key{tauui.KeyUp, tauui.KeyDown, tauui.KeyPageUp, tauui.KeyPageDown, tauui.KeyEnter, tauui.KeyTab} {
		action, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: key})
		assert.False(t, ok, "disabled selector key %v action %v", key, action)
	}
	require.True(t, a.routeSelectorDown(stubCtx()))
	assert.Equal(t, 1, a.selector.Focused())
	require.True(t, (&Root{app: a}).HandleAction(tauui.ActionCancel, stubCtx()))
	assert.False(t, a.selectorVisible())
}

func TestNativeStartupAppliesScalarAndLegacyBindings(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keybindings.json"), []byte("\ufeff"+`{"cycleModelForward":"ctrl+j","app.model.cycleForward":"ctrl+g","cursorLineStart":"ctrl+o","selectCancel":"ctrl+q","tui.select.down":false}`), 0o600))
	a, err := New(Options{Session: &fakeSession{}, SettingsPath: filepath.Join(dir, "settings.json")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	act, ok := (appKeymap{app: a, manager: a.keybindings}).Resolve(tauui.KeyCtxTextInput, ctrl('g'))
	assert.True(t, ok)
	assert.Equal(t, AppActionModelCycleForward, act)
	cmdSettingsOverlay(a, "")
	o, ok := a.dialogPicker.Component().(*settingsOverlay)
	require.True(t, ok)
	o.Paste("level")
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'o', Mod: tauui.ModCtrl}, stubCtx())
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'X'}, stubCtx())
	assert.Equal(t, "Xlevel", o.list.Search())
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyEsc}, stubCtx())
	assert.True(t, a.dialogPicker.Shown())
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'q', Mod: tauui.ModCtrl}, stubCtx())
	assert.False(t, a.dialogPicker.Shown())
}
