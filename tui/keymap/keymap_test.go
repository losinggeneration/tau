package keymap

import (
	"testing"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
)

func TestEditorKeymap_Arrows(t *testing.T) {
	km := EditorKeymap{}

	tests := []struct {
		name string
		key  event.Key
		want ui.Action
	}{
		{"Left", event.KeyLeft, ui.ActionMoveLeft},
		{"Right", event.KeyRight, ui.ActionMoveRight},
		{"Up", event.KeyUp, ui.ActionMoveUp},
		{"Down", event.KeyDown, ui.ActionMoveDown},
		{"Home", event.KeyHome, ui.ActionHome},
		{"End", event.KeyEnd, ui.ActionEnd},
		{"PageUp", event.KeyPageUp, ui.ActionPageUp},
		{"PageDown", event.KeyPageDown, ui.ActionPageDown},
		{"Backspace", event.KeyBackspace, ui.ActionDeleteBackward},
		{"Delete", event.KeyDelete, ui.ActionDeleteForward},
		{"Enter", event.KeyEnter, ui.ActionSubmit},
		{"Tab", event.KeyTab, ui.ActionFocusNext},
		{"ShiftTab", event.KeyShiftTab, ui.ActionFocusPrev},
		{"Esc", event.KeyEsc, ui.ActionCancel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := km.Resolve(ui.KeyCtxGlobal, ui.Keystroke{Key: tt.key})
			if !ok {
				t.Errorf("Resolve(%s) = !ok, want true", tt.name)
				return
			}

			if got != tt.want {
				t.Errorf("Resolve(%s) = %d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

func TestEditorKeymap_CtrlCombos(t *testing.T) {
	km := EditorKeymap{}

	tests := []struct {
		name string
		rune rune
		want ui.Action
	}{
		{"Ctrl+A", 'a', ui.ActionHome},
		{"Ctrl+B", 'b', ui.ActionMoveLeft},
		{"Ctrl+D", 'd', ui.ActionDeleteForward},
		{"Ctrl+E", 'e', ui.ActionEnd},
		{"Ctrl+F", 'f', ui.ActionMoveRight},
		{"Ctrl+K", 'k', ActionDeleteToLineEnd},
		{"Ctrl+U", 'u', ActionDeleteToLineStart},
		{"Ctrl+W", 'w', ActionDeleteWordBackward},
		{"Ctrl+Y", 'y', ActionYank},
		{"Ctrl+-", '-', ActionUndo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := km.Resolve(ui.KeyCtxGlobal, ui.Keystroke{
				Key:  event.KeyRune,
				Rune: tt.rune,
				Mod:  event.ModCtrl,
			})
			if !ok {
				t.Errorf("Resolve(%s) = !ok, want true", tt.name)
				return
			}

			if got != tt.want {
				t.Errorf("Resolve(%s) = %d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

// Ctrl+Z is not undo here: the app layer binds it to app.suspend, and undo's
// conventional binding is Ctrl+- (see tui.editor.undo in tui/keybindings).
// Keeping Ctrl+Z in this table would claim a key the app has already given to
// suspend, and the two would fight the moment this keymap is composed under
// an app resolver.
func TestEditorKeymap_CtrlZIsNotUndo(t *testing.T) {
	km := EditorKeymap{}

	got, ok := km.Resolve(ui.KeyCtxTextInput, ui.Keystroke{
		Key:  event.KeyRune,
		Rune: 'z',
		Mod:  event.ModCtrl,
	})
	if ok && got == ActionUndo {
		t.Fatal("Ctrl+Z resolves to undo; it belongs to app.suspend")
	}
}

func TestEditorKeymap_PiEditorAlternateBindings(t *testing.T) {
	km := EditorKeymap{}

	tests := []struct {
		name string
		key  event.Key
		rune rune
		mod  event.ModMask
		want ui.Action
	}{
		{"AltLeft", event.KeyLeft, 0, event.ModAlt, ActionMoveWordLeft},
		{"CtrlLeft", event.KeyLeft, 0, event.ModCtrl, ActionMoveWordLeft},
		{"AltB", event.KeyRune, 'b', event.ModAlt, ActionMoveWordLeft},
		{"AltRight", event.KeyRight, 0, event.ModAlt, ActionMoveWordRight},
		{"CtrlRight", event.KeyRight, 0, event.ModCtrl, ActionMoveWordRight},
		{"AltF", event.KeyRune, 'f', event.ModAlt, ActionMoveWordRight},
		{"AltBackspace", event.KeyBackspace, 0, event.ModAlt, ActionDeleteWordBackward},
		{"AltD", event.KeyRune, 'd', event.ModAlt, ActionDeleteWordForward},
		{"AltDelete", event.KeyDelete, 0, event.ModAlt, ActionDeleteWordForward},
		{"AltY", event.KeyRune, 'y', event.ModAlt, ActionYankPop},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := km.Resolve(ui.KeyCtxTextInput, ui.Keystroke{Key: tt.key, Rune: tt.rune, Mod: tt.mod})
			if !ok {
				t.Fatalf("Resolve(%s) = !ok, want true", tt.name)
			}

			if got != tt.want {
				t.Fatalf("Resolve(%s) = %d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

func TestEditorKeymap_CtrlC_CopyInTextInput(t *testing.T) {
	km := EditorKeymap{}

	got, ok := km.Resolve(ui.KeyCtxTextInput, ui.Keystroke{Key: event.KeyCtrlC})
	if !ok || got != ui.ActionCopy {
		t.Errorf("Resolve(Ctrl+C, TextInput) = (%v, %v), want (ActionCopy, true)", got, ok)
	}
}

func TestEditorKeymap_CtrlC_CancelInGlobal(t *testing.T) {
	km := EditorKeymap{}

	got, ok := km.Resolve(ui.KeyCtxGlobal, ui.Keystroke{Key: event.KeyCtrlC})
	if !ok || got != ui.ActionCancel {
		t.Errorf("Resolve(Ctrl+C, Global) = (%v, %v), want (ActionCancel, true)", got, ok)
	}
}

func TestEditorKeymap_UnboundReturnsFalse(t *testing.T) {
	km := EditorKeymap{}

	got, ok := km.Resolve(ui.KeyCtxGlobal, ui.Keystroke{Key: event.KeyRune, Rune: 'x'})
	if ok || got != ui.ActionNone {
		t.Errorf("Resolve(unbound) = (%v, %v), want (ActionNone, false)", got, ok)
	}
}

func TestManager_DefaultOverrideDisableDedupeAndConflicts(t *testing.T) {
	m := NewManager(map[ui.Action][]Binding{
		ui.ActionSubmit: {B(KeyCtxGlobal, event.KeyEnter, 0, 0), B(KeyCtxGlobal, event.KeyEnter, 0, 0)},
		ui.ActionCancel: {B(KeyCtxGlobal, event.KeyEsc, 0, 0)},
	})

	if keys := m.Keys(ui.ActionSubmit); len(keys) != 1 {
		t.Fatalf("deduped default keys len = %d, want 1", len(keys))
	}

	got, ok := m.Resolve(KeyCtxGlobal, ui.Keystroke{Key: event.KeyEnter})
	if !ok || got != ui.ActionSubmit {
		t.Fatalf("default Resolve = (%v, %v), want submit/true", got, ok)
	}

	m.Set(ui.ActionSubmit, nil)
	got, ok = m.Resolve(KeyCtxGlobal, ui.Keystroke{Key: event.KeyEnter})
	if ok || got != ui.ActionNone {
		t.Fatalf("disabled Resolve = (%v, %v), want none/false", got, ok)
	}

	binding := B(KeyCtxGlobal, event.KeyRune, 'x', 0)
	m.Set(ui.ActionSubmit, []Binding{binding, binding})
	if keys := m.Keys(ui.ActionSubmit); len(keys) != 1 {
		t.Fatalf("deduped user keys len = %d, want 1", len(keys))
	}
	got, ok = m.Resolve(KeyCtxGlobal, ui.Keystroke{Key: event.KeyRune, Rune: 'x'})
	if !ok || got != ui.ActionSubmit {
		t.Fatalf("override Resolve = (%v, %v), want submit/true", got, ok)
	}

	m.Set(ui.ActionCancel, []Binding{binding})
	conflicts := m.Conflicts()
	if len(conflicts) != 1 {
		t.Fatalf("Conflicts len = %d, want 1", len(conflicts))
	}
	if len(conflicts[0].Actions) != 2 {
		t.Fatalf("conflict actions = %v, want 2 actions", conflicts[0].Actions)
	}
}

func TestReservedActions(t *testing.T) {
	reserved := ReservedActions()
	for _, action := range []ui.Action{ui.ActionCancel, ui.ActionSubmit, ActionDeleteToLineEnd} {
		if !reserved[action] {
			t.Fatalf("action %v is not reserved", action)
		}
	}
}

func TestKeymapAdapter_UsesProvidedContext(t *testing.T) {
	km := contextKeymap{}

	got, ok := tuiKeymapAdapter(km, ui.KeyCtxTextInput, ui.Keystroke{Key: event.KeyEnter})
	if !ok || got != ui.ActionSubmit {
		t.Fatalf("text input resolve = (%v, %v), want submit/true", got, ok)
	}

	got, ok = tuiKeymapAdapter(km, ui.KeyCtxGlobal, ui.Keystroke{Key: event.KeyEnter})
	if !ok || got != ui.ActionActivate {
		t.Fatalf("global resolve = (%v, %v), want activate/true", got, ok)
	}
}

type contextKeymap struct{}

func (contextKeymap) Resolve(ctx ui.KeyContext, _ ui.Keystroke) (ui.Action, bool) {
	if ctx == ui.KeyCtxTextInput {
		return ui.ActionSubmit, true
	}

	return ui.ActionActivate, true
}

func tuiKeymapAdapter(km ui.Keymap, ctx ui.KeyContext, k ui.Keystroke) (ui.Action, bool) {
	return km.Resolve(ctx, k)
}

func TestKeystrokeOf(t *testing.T) {
	e := event.KeyEvent{Key: event.KeyRune, Rune: 'q', Mod: event.ModCtrl}
	ks := KeystrokeOf(e)

	if ks.Key != event.KeyRune || ks.Rune != 'q' || ks.Mod != event.ModCtrl {
		t.Errorf("KeystrokeOf mismatch: got %+v", ks)
	}
}

func TestKeyContext_ConstantsDistinct(t *testing.T) {
	if KeyCtxGlobal == KeyCtxTextInput || KeyCtxGlobal == KeyCtxOverlay || KeyCtxTextInput == KeyCtxOverlay {
		t.Error("KeyContext constants should be distinct")
	}
}
