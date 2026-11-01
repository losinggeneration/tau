package tui

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/render"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/extensions"
	"github.com/losinggeneration/tau/tui/keybindings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsPersistenceMergesConcurrentChoicesAndPreservesUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"models":{"p/m":{"future":9007199254740993}},"terminal":{"future":true},"extension":false}`), 0o600))
	app := &App{settingsPath: path, ctx: t.Context()}
	var workers sync.WaitGroup
	for _, save := range []func() error{
		func() error { return app.persistThemeSetting("tau") },
		func() error { return app.persistLayoutSetting(LayoutMode("compact")) },
		func() error { return app.persistModelThinkingSetting("p/m", "high") },
	} {
		workers.Go(func() {
			if err := save(); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"models":{"p/m":{"thinkingLevel":"high","future":9007199254740993}},"terminal":{"future":true},"extension":false,"theme":"tau","layoutMode":"compact"}`, string(data))
	assert.Contains(t, string(data), "9007199254740993")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	app.ctx = ctx
	require.ErrorIs(t, app.persistThemeSetting("different"), context.Canceled)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(data), string(after))
}

type settingsCursorDrawer struct {
	rovel.Drawer
	cursor  rovel.Point
	visible bool
}

func (d *settingsCursorDrawer) SetCursor(pos rovel.Point) { d.cursor = pos; d.visible = true }

func TestSettingsOverlayPaintsSearchCursor(t *testing.T) {
	a := newAppWithSelector(t)
	o := newSettingsOverlay(a)
	o.Paste("abcdefghijklmnop")
	o.HandleInput("\x01\x1b[C\x1b[C\x1b[C\x1b[C\x1b[C\x1b[C\x1b[C\x1b[C")
	picker := a.dialogPicker
	picker.SetComponent(o)
	picker.SetShown(true)
	picker.Layout(tauui.Rect{X: 1, Y: 1, W: 8, H: 3})
	for _, focused := range []bool{true, false} {
		buf := render.NewBuffer(12, 5)
		drawer := &settingsCursorDrawer{Drawer: rovel.NewDrawer(rovel.NewPainter(render.NewPainter(buf, tauui.Rect{W: 12, H: 5}, tauui.Style{}), tauui.Style{}))}
		ctx := stubCtx()
		if focused {
			ctx.FocusedID = picker.ID()
		}
		picker.Paint(drawer, ctx)
		assert.Equal(t, focused, drawer.visible)
		if focused {
			assert.Equal(t, tauui.Point{X: 6, Y: 1}, drawer.cursor)
		}
		for x, r := range "> fghijk" {
			assert.Equal(t, r, buf.At(x+1, 1).R)
		}
		assert.Equal(t, !focused, buf.At(6, 1).Style.Attr&tauui.AttrReverse != 0)
	}
}

func TestSettingsOverlayLoadsToolkitBindingsFromNativeConfig(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keybindings.json"), []byte(`{"tui.editor.cursorLineStart":["ctrl+o"],"tui.select.cancel":["ctrl+q"],"tui.select.down":["ctrl+n"]}`), 0o600))
	a, err := New(Options{Session: &fakeSession{}, SettingsPath: filepath.Join(dir, "settings.json")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	cmdSettingsOverlay(a, "")
	o, ok := a.dialogPicker.Component().(*settingsOverlay)
	require.True(t, ok)
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'n', Mod: tauui.ModCtrl}, stubCtx())
	assert.Equal(t, 1, o.list.SelectedIndex())
	o.Paste("level")
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'o', Mod: tauui.ModCtrl}, stubCtx())
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'X'}, stubCtx())
	assert.Equal(t, "Xlevel", o.list.Search())
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyEsc}, stubCtx())
	assert.True(t, a.dialogPicker.Shown(), "replaced Escape must not close settings")
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'q', Mod: tauui.ModCtrl}, stubCtx())
	assert.False(t, a.dialogPicker.Shown())
}

func TestSettingsOverlayOwnsRawKeysBeforeNativeActions(t *testing.T) {
	a := newAppWithSelector(t)
	cmdSettingsOverlay(a, "")
	o := a.dialogPicker.Component().(*settingsOverlay)
	km := appKeymap{app: a}
	for _, ctx := range []tauui.KeyContext{tauui.KeyCtxTextInput, tauui.KeyCtxGlobal} {
		for _, key := range []tauui.KeyEvent{
			{Key: tauui.KeyRune, Rune: ' '},
			{Key: tauui.KeyEnter},
			{Key: tauui.KeyDown},
			{Key: tauui.KeyEsc},
			{Key: tauui.KeyRune, Rune: 'o', Mod: tauui.ModCtrl},
			{Key: tauui.KeyCtrlC},
		} {
			_, resolved := km.Resolve(ctx, tauui.Keystroke{Key: key.Key, Rune: key.Rune, Mod: key.Mod})
			assert.False(t, resolved, "settings must receive raw key %+v in context %v", key, ctx)
		}
	}
	o.Paste("thinking")
	key := tauui.KeyEvent{Key: tauui.KeyRune, Rune: ' '}
	_, resolved := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: key.Key, Rune: key.Rune, Mod: key.Mod})
	require.False(t, resolved)
	a.dialogPicker.Handle(key, stubCtx())
	assert.Equal(t, "thinking ", o.list.Search())
	action, ok := km.Resolve(tauui.KeyCtxGlobal, ctrl('v'))
	assert.True(t, ok)
	assert.Equal(t, AppActionPaste, action)
}

func TestSettingsOverlayOwnsClipboardWhenEditorStillFocused(t *testing.T) {
	a := newAppWithSelector(t)
	cmdSettingsOverlay(a, "")
	o := a.dialogPicker.Component().(*settingsOverlay)
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	var reads int
	ctx.ClipboardRead = func() { reads++ }
	require.True(t, a.requestClipboardPaste(ctx))
	assert.Equal(t, 1, reads)
	require.True(t, (&Root{app: a}).Handle(tauui.ClipboardResponseEvent{Text: "thinking\nlevel"}, ctx))
	assert.Equal(t, "thinkinglevel", o.list.Search())
	assert.Empty(t, a.editor.Text())
}

func TestSettingsThemeNavigationPreviewsAndCancelRestoresWithoutSaving(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}, SettingsPath: filepath.Join(t.TempDir(), "settings.json")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	applySettingsChange(a, "settings.theme", "dark")
	original, err := os.ReadFile(a.settingsPath)
	require.NoError(t, err)
	o := newSettingsOverlay(a)
	o.Paste("theme")
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	rows := strings.Join(o.Render(80), "\n")
	assert.Contains(t, rows, "> dark")
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyDown})
	assert.Equal(t, "light", a.terminalTheme.active)
	assert.Equal(t, "light", a.footer.data.theme)
	after, err := os.ReadFile(a.settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(original), string(after))
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEsc})
	assert.Equal(t, "dark", a.terminalTheme.active)
	assert.Equal(t, "dark", a.footer.data.theme)
	after, err = os.ReadFile(a.settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(original), string(after))
	assert.Contains(t, strings.Join(o.Render(80), "\n"), "dark")
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyDown})
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	assert.Equal(t, "light", a.terminalTheme.active)
	after, err = os.ReadFile(a.settingsPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"theme":"light"}`, string(after))
}

func TestSettingsThemeCancelRestoresExtensionOwnedPalette(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	custom := extensions.Theme{Name: "custom", Values: map[string]string{"text": "#123456"}, Appearance: "light", Dim: []string{"muted"}}
	a.extensions.SetTheme(custom)
	original := a.chat.ThemeTokens()
	require.True(t, a.terminalTheme.inMemory)
	o := newSettingsOverlay(a)
	o.Paste("theme")
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	assert.Contains(t, strings.Join(o.Render(80), "\n"), "> custom")
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyDown})
	assert.False(t, a.terminalTheme.inMemory)
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEsc})
	assert.True(t, a.terminalTheme.inMemory)
	assert.Equal(t, custom, a.extensions.Theme())
	assert.Equal(t, original, a.chat.ThemeTokens())
	assert.Equal(t, "custom", a.footer.data.theme)
	a.applyTerminalThemeColors(systemTestInput(t))
	assert.Equal(t, original, a.chat.ThemeTokens())
}

func TestSettingsHostedEscapeCancelsThemePreviewBeforeClosing(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	a.applyTheme("dark")
	cmdSettingsOverlay(a, "")
	o := a.dialogPicker.Component().(*settingsOverlay)
	o.Paste("theme")
	for _, key := range []tauui.Key{tauui.KeyEnter, tauui.KeyDown, tauui.KeyEsc} {
		require.True(t, a.dialogPicker.Handle(tauui.KeyEvent{Key: key}, stubCtx()))
	}
	assert.True(t, a.dialogPicker.Shown())
	assert.Equal(t, "dark", a.terminalTheme.active)
	require.True(t, a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyEsc}, stubCtx()))
	assert.False(t, a.dialogPicker.Shown())
}

func TestReplacingSettingsDialogRollsBackThemePreview(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		t.Run(strconv.FormatBool(confirm), func(t *testing.T) {
			a, err := New(Options{Session: &fakeSession{}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
			a.applyTheme("dark")
			cmdSettingsOverlay(a, "")
			o := a.dialogPicker.Component().(*settingsOverlay)
			o.Paste("theme")
			o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
			o.HandleKey(tauui.KeyEvent{Key: tauui.KeyDown})
			require.Equal(t, "light", a.terminalTheme.active)
			if confirm {
				o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
			}
			cmdSettingsOverlay(a, "")
			want := "dark"
			if confirm {
				want = "light"
			}
			assert.Equal(t, want, a.terminalTheme.active)
			assert.Equal(t, want, a.footer.data.theme)
			a.dismissDialog()
			assert.Equal(t, want, a.terminalTheme.active)
		})
	}
}

func TestSettingsAutomaticThemeEditsPreviewAndRequireApply(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}, SettingsPath: filepath.Join(t.TempDir(), "settings.json")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	a.terminalTheme.scheme = "light"
	applySettingsChange(a, "settings.theme", "light/dark")
	original, err := os.ReadFile(a.settingsPath)
	require.NoError(t, err)
	o := newSettingsOverlay(a)
	o.Paste("theme")
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	require.Contains(t, strings.Join(o.Render(80), "\n"), "Light theme")
	// Open the light-theme picker, preview a different theme, then cancel it.
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyUp})
	assert.Equal(t, "dark", a.terminalTheme.active)
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEsc})
	assert.Equal(t, "light/dark", a.terminalTheme.setting)
	assert.Equal(t, "light", a.terminalTheme.active)
	// A confirmed picker choice changes the pair but not the saved setting.
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyUp})
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	assert.Equal(t, "dark/dark", a.terminalTheme.setting)
	after, err := os.ReadFile(a.settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(original), string(after))
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyDown})
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyDown})
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	after, err = os.ReadFile(a.settingsPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"theme":"dark/dark"}`, string(after))
	assert.Contains(t, strings.Join(o.Render(80), "\n"), "dark/dark")
}

func TestSettingsAutomaticThemeCanCancelAndChangeModes(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}, SettingsPath: filepath.Join(t.TempDir(), "settings.json")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	applySettingsChange(a, "settings.theme", "dark")
	original, err := os.ReadFile(a.settingsPath)
	require.NoError(t, err)
	o := newSettingsOverlay(a)
	o.Paste("theme")
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	require.Contains(t, strings.Join(o.Render(80), "\n"), "automatic")
	// Automatic mode is between the system theme and fixed themes.
	for range 3 {
		o.HandleKey(tauui.KeyEvent{Key: tauui.KeyUp})
	}
	require.Equal(t, "dark/dark", a.terminalTheme.setting)
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	require.Contains(t, strings.Join(o.Render(80), "\n"), "Light theme")
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEsc})
	assert.Equal(t, "dark", a.terminalTheme.setting)
	// Automatic mode can return to single mode without committing either mode.
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	for range 3 {
		o.HandleKey(tauui.KeyEvent{Key: tauui.KeyUp})
	}
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	for range 3 {
		o.HandleKey(tauui.KeyEvent{Key: tauui.KeyDown})
	}
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	assert.Contains(t, strings.Join(o.Render(80), "\n"), "> dark")
	assert.Equal(t, "dark", a.terminalTheme.setting)
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEsc})
	after, err := os.ReadFile(a.settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(original), string(after))
}

func TestSettingsAutomaticThemeDarkChoiceAndOuterCancel(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}, SettingsPath: filepath.Join(t.TempDir(), "settings.json")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	a.terminalTheme.scheme = "dark"
	applySettingsChange(a, "settings.theme", "light/dark")
	original, err := os.ReadFile(a.settingsPath)
	require.NoError(t, err)
	cmdSettingsOverlay(a, "")
	o := a.dialogPicker.Component().(*settingsOverlay)
	o.Paste("theme")
	for _, key := range []tauui.Key{tauui.KeyEnter, tauui.KeyDown, tauui.KeyEnter, tauui.KeyDown, tauui.KeyEnter} {
		require.True(t, a.dialogPicker.Handle(tauui.KeyEvent{Key: key}, stubCtx()))
	}
	assert.Equal(t, "light/light", a.terminalTheme.setting)
	assert.Equal(t, "light", a.terminalTheme.active)
	require.True(t, a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyEsc}, stubCtx()))
	assert.True(t, a.dialogPicker.Shown())
	assert.Equal(t, "light/dark", a.terminalTheme.setting)
	assert.Equal(t, "dark", a.terminalTheme.active)
	after, err := os.ReadFile(a.settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(original), string(after))
	// Replacing settings during a second preview also restores the original pair.
	for _, key := range []tauui.Key{tauui.KeyEnter, tauui.KeyDown, tauui.KeyEnter, tauui.KeyDown} {
		require.True(t, a.dialogPicker.Handle(tauui.KeyEvent{Key: key}, stubCtx()))
	}
	require.Equal(t, "light", a.terminalTheme.active)
	cmdSettingsOverlay(a, "")
	assert.Equal(t, "light/dark", a.terminalTheme.setting)
	assert.Equal(t, "dark", a.terminalTheme.active)
}

func TestSettingsThemeChoicesIncludeReloadedResources(t *testing.T) {
	names := []string{"system", "dark", "project-theme", "project-theme", ""}
	a, err := New(Options{Session: &fakeSession{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	a.themeNames = func() []string { return names }
	assert.Equal(t, []string{"system", "default", "tau", "dark", "light", "project-theme"}, a.availableThemes())
	o := newSettingsOverlay(a)
	o.Paste("theme")
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	assert.Contains(t, strings.Join(o.Render(80), "\n"), "project-theme")
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEsc})
	names = []string{"reloaded-theme"}
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	rows := strings.Join(o.Render(80), "\n")
	assert.Contains(t, rows, "reloaded-theme")
	assert.NotContains(t, rows, "project-theme")
}
