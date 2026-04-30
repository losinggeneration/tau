package tui

import (
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/extensions"
)

// shortcutApp wires an extensions registry with one shortcut on the given key.
func shortcutApp(t *testing.T, key string) (*App, *Root, *int) {
	t.Helper()
	a := newAppWithSelector(t)
	a.extensions = extensions.New()
	fired := 0
	if err := a.extensions.RegisterShortcut(extensions.Shortcut{
		ID:      "ext.demo",
		Key:     key,
		Handler: func() { fired++ },
	}); err != nil {
		t.Fatalf("RegisterShortcut(%q): %v", key, err)
	}
	return a, &Root{app: a}, &fired
}

// §12: extension shortcuts are dispatched from the key path. They were
// registered and never consulted — TriggerShortcut had no production caller at
// all, so every extension shortcut was dead on arrival.
func TestExtensionShortcutRunsFromAKeystroke(t *testing.T) {
	a, root, fired := shortcutApp(t, "ctrl+g")
	km := appKeymap{app: a}

	act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyRune, Rune: 'g', Mod: tauui.ModCtrl})
	if !ok || act != AppActionExtensionShortcut {
		t.Fatalf("ctrl+g resolved to (%v, %v), want the extension-shortcut action", act, ok)
	}
	if !root.HandleAction(act, stubCtx()) {
		t.Fatal("extension shortcut action was not handled")
	}
	if *fired != 1 {
		t.Errorf("handler ran %d times, want 1", *fired)
	}
}

// The key spelling in the manifest and the key the terminal delivers are
// matched through the compiled keystroke, not by string equality.
func TestExtensionShortcutMatchesRegardlessOfKeySpelling(t *testing.T) {
	a, root, fired := shortcutApp(t, "Ctrl+G")
	km := appKeymap{app: a}

	act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyRune, Rune: 'g', Mod: tauui.ModCtrl})
	if !ok {
		t.Fatal("a shortcut registered as \"Ctrl+G\" did not match ctrl+g")
	}
	root.HandleAction(act, stubCtx())
	if *fired != 1 {
		t.Errorf("handler ran %d times, want 1", *fired)
	}
}

// §12 reserves a specific subset. An extension binding one of those keys must
// not take it: Ctrl+C has to keep exiting.
func TestExtensionShortcutCannotShadowAReservedKey(t *testing.T) {
	a, _, fired := shortcutApp(t, "ctrl+c")
	km := appKeymap{app: a}

	act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyCtrlC})
	if !ok {
		t.Fatal("ctrl+c resolved to nothing")
	}
	if act == AppActionExtensionShortcut {
		t.Fatal("an extension shortcut shadowed ctrl+c, which §12 reserves")
	}
	if act != AppActionExit {
		t.Errorf("ctrl+c resolved to %v, want app.exit", act)
	}
	if *fired != 0 {
		t.Errorf("reserved-key handler ran %d times, want 0", *fired)
	}
}

// The reserved list is a subset, not "everything the app binds". Ctrl+O
// (app.header.expand) is not on §12's list, so an extension may take it.
func TestExtensionShortcutMayShadowANonReservedAppKey(t *testing.T) {
	a, root, fired := shortcutApp(t, "ctrl+o")
	km := appKeymap{app: a}

	act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyRune, Rune: 'o', Mod: tauui.ModCtrl})
	if !ok {
		t.Fatal("ctrl+o resolved to nothing")
	}
	if act != AppActionExtensionShortcut {
		t.Fatalf("ctrl+o resolved to %v; app.header.expand is not reserved, so the extension wins", act)
	}
	root.HandleAction(act, stubCtx())
	if *fired != 1 {
		t.Errorf("handler ran %d times, want 1", *fired)
	}
}

// A key with no shortcut behind it must fall through to the app registry
// untouched.
func TestUnboundKeysAreNotClaimedByTheShortcutPath(t *testing.T) {
	a, _, _ := shortcutApp(t, "ctrl+g")
	km := appKeymap{app: a}

	act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyRune, Rune: 't', Mod: tauui.ModCtrl})
	if !ok || act != AppActionThinkingCycle {
		t.Errorf("ctrl+t resolved to (%v, %v), want app.thinking.cycle to still win", act, ok)
	}
}

// An app with no extensions registry must resolve exactly as before.
func TestNoExtensionsRegistryLeavesResolutionUnchanged(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyRune, Rune: 't', Mod: tauui.ModCtrl})
	if !ok || act != AppActionThinkingCycle {
		t.Errorf("ctrl+t resolved to (%v, %v) with no registry, want app.thinking.cycle", act, ok)
	}
}

// Dispatching with nothing pending must decline rather than swallow the key.
func TestExtensionShortcutActionDeclinesWithNothingPending(t *testing.T) {
	a := newAppWithSelector(t)
	root := &Root{app: a}

	if root.HandleAction(AppActionExtensionShortcut, stubCtx()) {
		t.Error("the shortcut action claimed a key with nothing pending")
	}
}

// The pending shortcut is consumed, not left to fire again on the next
// unrelated action.
func TestPendingShortcutIsConsumedOnce(t *testing.T) {
	a, root, fired := shortcutApp(t, "ctrl+g")
	km := appKeymap{app: a}

	act, _ := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyRune, Rune: 'g', Mod: tauui.ModCtrl})
	root.HandleAction(act, stubCtx())
	root.HandleAction(AppActionExtensionShortcut, stubCtx())

	if *fired != 1 {
		t.Errorf("handler ran %d times, want 1 — the pending shortcut was not cleared", *fired)
	}
}
