package tui

import (
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
)

func TestScopedModelsOverlayToggleFromAllCreatesExplicitSingleModel(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	overlay := newScopedModelsOverlay(app)
	if overlay == nil || len(overlay.items) == 0 {
		t.Skip("no authenticated models available in test environment")
	}

	key := overlay.items[0].Key
	if !app.scopedModelEnabled(key) {
		t.Fatalf("nil scoped model set should treat %q as enabled", key)
	}
	overlay.toggleFocused()
	if len(app.scopedModelKeys) != 1 || app.scopedModelKeys[0] != key {
		t.Fatalf("scopedModelKeys after first toggle = %#v, want [%q]", app.scopedModelKeys, key)
	}
	if !app.scopedModelEnabled(key) {
		t.Fatalf("explicit scoped model should be enabled after toggle")
	}

	overlay.toggleFocused()
	if len(app.scopedModelKeys) != 0 {
		t.Fatalf("scopedModelKeys after removing only model = %#v, want empty explicit list", app.scopedModelKeys)
	}
	if app.scopedModelEnabled(key) {
		t.Fatalf("empty explicit scoped model set should disable %q", key)
	}
}

func TestCmdScopedModelsOverlayOpensDialogComponent(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil

	cmdScopedModelsOverlay(app, "")

	if !app.dialogPicker.Shown() {
		t.Fatalf("/scoped-models should show the dialog picker")
	}
	if app.dialogPicker.Component() == nil {
		t.Fatalf("/scoped-models should install a component")
	}
	if got := app.dialogPicker.Header(); got != "/scoped-models" {
		t.Fatalf("dialog header = %q, want /scoped-models", got)
	}
}

func TestScopedModelsSpaceTogglesWithoutDismissingDialog(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	overlay := newScopedModelsOverlay(app)
	if overlay == nil || len(overlay.items) == 0 {
		t.Skip("no authenticated models available in test environment")
	}
	app.dialogPicker.SetComponent(overlay)
	app.dialogPicker.SetShown(true)

	ctx := scopedModelDialogTestCtx(app)
	handled := app.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: ' '}, ctx)
	if !handled {
		t.Fatalf("space should be handled by the scoped-models dialog")
	}
	if !app.dialogPicker.Shown() {
		t.Fatalf("space dismissed scoped-models dialog; want it to remain open")
	}
	if got := app.editor.Text(); got != "" {
		t.Fatalf("space leaked into editor text = %q, want empty", got)
	}
	if len(app.scopedModelKeys) != 1 || app.scopedModelKeys[0] != overlay.items[0].Key {
		t.Fatalf("space did not toggle focused model, scopedModelKeys=%#v", app.scopedModelKeys)
	}
}

func TestScopedModelsEnterTogglesFocusedModel(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	overlay := newScopedModelsOverlay(app)
	if overlay == nil || len(overlay.items) == 0 {
		t.Skip("no authenticated models available in test environment")
	}
	app.dialogPicker.SetComponent(overlay)
	app.dialogPicker.SetShown(true)

	ctx := scopedModelDialogTestCtx(app)
	if !app.dialogPicker.HandleAction(int(tauui.ActionActivate), ctx) {
		t.Fatalf("Enter/activate should be handled by the scoped-models dialog")
	}
	if len(app.scopedModelKeys) != 1 || app.scopedModelKeys[0] != overlay.items[0].Key {
		t.Fatalf("Enter did not toggle focused model, scopedModelKeys=%#v", app.scopedModelKeys)
	}
}

func TestScopedModelsSpaceKeymapRoutesToDialogWhenEditorStillFocused(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	overlay := newScopedModelsOverlay(app)
	if overlay == nil || len(overlay.items) == 0 {
		t.Skip("no authenticated models available in test environment")
	}
	app.dialogPicker.SetComponent(overlay)
	app.dialogPicker.SetShown(true)

	act, ok := appKeymap{app: app}.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyRune, Rune: ' '})
	if !ok || act != AppActionSelectorActivate {
		t.Fatalf("space with focused editor and scoped-models dialog = (%v, %v), want AppActionSelectorActivate/true", act, ok)
	}
}

func TestScopedModelsEnterKeymapRoutesToDialogWhenEditorStillFocused(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	overlay := newScopedModelsOverlay(app)
	if overlay == nil || len(overlay.items) == 0 {
		t.Skip("no authenticated models available in test environment")
	}
	app.dialogPicker.SetComponent(overlay)
	app.dialogPicker.SetShown(true)

	act, ok := appKeymap{app: app}.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyEnter})
	if !ok || act != AppActionSelectorActivate {
		t.Fatalf("Enter with focused editor and scoped-models dialog = (%v, %v), want AppActionSelectorActivate/true", act, ok)
	}
}

func scopedModelDialogTestCtx(app *App) *tauui.Ctx {
	ctx := stubCtx()
	ctx.FocusedID = app.dialogPicker.ID()
	ctx.Invalidate = func(tauui.Rect) {}
	return ctx
}
