package tui

import (
	"slices"
	"sort"
	"strings"

	"github.com/losinggeneration/tau/ai"
	tauui "github.com/losinggeneration/tau/tui"

	"github.com/losinggeneration/tau/tui/settingslist"
)

// settingsOverlay hosts a settingslist rendered as the dialog picker.
// The picker hosts the same settings model, theme, thinking level,
// auto-compaction, and queue display settings the TUI supports; the
// per-row value is sourced from the App's runtime state and the change
// callback writes back to whichever subsystem owns the setting.
//
// Wiring the existing settingslist package into the App was the gap
// the parity checklist flagged: the package existed and was tested,
// but it had no consumer. The overlay below gives it one and exposes
// the App's settings in a single searchable/fuzzy menu.
type settingsOverlay struct {
	list  *settingslist.List
	items []settingslist.Item
}

// newSettingsOverlay constructs a settingsOverlay whose value list is
// sourced from app state. The returned overlay is ready to be hosted
// by the dialog picker; the search field is enabled so the user can
// fuzzy-filter settings as they type.
func newSettingsOverlay(app *App) *settingsOverlay {
	items := buildSettingsItems(app)
	o := &settingsOverlay{items: items}
	o.list = settingslist.New(
		items,
		12,
		settingslist.DefaultTheme(),
		func(id, newValue string) {
			if app == nil {
				return
			}
			applySettingsChange(app, id, newValue)
		},
		func() {
			if app != nil {
				app.dismissDialog()
			}
		},
		settingslist.Options{EnableSearch: true},
	)
	return o
}

// HandleInput accepts simple test input and forwards it as normalized key
// events. Production picker input uses HandleKey.
func (o *settingsOverlay) HandleInput(data string) {
	if o == nil || o.list == nil {
		return
	}
	o.list.HandleInput(data)
}

func (o *settingsOverlay) HandleKey(ke tauui.KeyEvent) {
	if o == nil || o.list == nil {
		return
	}
	o.list.HandleKey(ke)
}

// Paste inserts clipboard text into the settings search.
func (o *settingsOverlay) Paste(value string) {
	if o != nil && o.list != nil {
		o.list.Paste(value)
	}
}

// Render implements the settingslist.Component contract so it can be
// hosted by the overlay picker.
func (o *settingsOverlay) Render(width int) []string {
	if o == nil || o.list == nil {
		return nil
	}
	return o.list.Render(width)
}

// SearchEnabled reports whether the search input is active. The
// dialog picker uses this to size itself.
func (o *settingsOverlay) SearchEnabled() bool {
	return true
}

func buildSettingsItems(app *App) []settingslist.Item {
	if app == nil {
		return nil
	}
	// Several rows read their current value from the footer's data record.
	// Reading it unguarded means /settings panics whenever the footer is not
	// yet wired, which is a crash for a menu that only needed to show a blank.
	data := app.footer.dataOrEmpty()
	items := []settingslist.Item{
		{
			ID:           "settings.model",
			Label:        "Model",
			Description:  "Active model id (provider/id).",
			CurrentValue: app.currentModel,
			Values:       app.switchableModelIDs(),
		},
		{
			ID:           "settings.thinkingLevel",
			Label:        "Thinking level",
			Description:  "Reasoning effort; saved per active model.",
			CurrentValue: data.thinkingLevel,
			Values:       ai.ThinkingLevels,
		},
		{
			ID:           "settings.autoCompaction",
			Label:        "Auto-compact",
			Description:  "Trigger automatic compaction when context is full.",
			CurrentValue: boolOnOff(data.autoCompaction),
			Values:       []string{"off", "on"},
		},
		{
			ID:           "settings.theme",
			Label:        "Theme",
			Description:  "Color theme for the terminal UI.",
			CurrentValue: app.terminalTheme.setting,
			Submenu:      app.themeSubmenuFactory(),
		},
		{
			ID:           "settings.layout",
			Label:        "Layout",
			Description:  "fixed: pinned header/editor, chat scrolls alone. document: one scrolling document.",
			CurrentValue: string(app.LayoutMode()),
			Values:       []string{string(LayoutFixed), string(LayoutDocument)},
		},
		{
			ID:           "settings.streamingFooter",
			Label:        "Footer notice",
			Description:  "Right-side notice when an action is in progress.",
			CurrentValue: data.notice,
		},
	}
	return items
}

// themeSubmenuFactory previews navigation and restores the original on cancel.
func (a *App) themeSubmenuFactory() func(current string, done func(selected string, ok bool)) settingslist.KeyComponent {
	return func(current string, done func(selected string, ok bool)) settingslist.KeyComponent {
		original := current
		if a != nil && a.terminalTheme.setting != "" {
			original = a.terminalTheme.setting
		}
		restore := func() { a.applyTheme(original) }
		if a != nil && a.terminalTheme.inMemory && a.extensions != nil {
			theme := a.extensions.Theme()
			original = theme.Name
			restore = func() { a.extensions.SetTheme(theme) }
		}
		if a != nil {
			a.terminalTheme.restorePreview = restore
		}
		menu := &themeSettingsMenu{app: a, themes: a.availableThemes(), done: func(value string) {
			a.terminalTheme.restorePreview = nil
			done(value, true)
		}, cancel: func() {
			a.cancelThemePreview()
			done("", false)
		}}
		menu.light, menu.dark, _ = strings.Cut(original, "/")
		menu.light, menu.dark = strings.TrimSpace(menu.light), strings.TrimSpace(menu.dark)
		if menu.light == "" || menu.dark == "" || strings.Contains(menu.dark, "/") {
			menu.light = original
			if !slices.Contains(menu.themes, original) {
				menu.light = systemThemeName
			}
			menu.dark = menu.light
			menu.showSingle(menu.light)
		} else {
			menu.showAutomatic()
		}
		return menu
	}
}

type themeSettingsMenu struct {
	*settingslist.List
	app         *App
	themes      []string
	light, dark string
	done        func(string)
	cancel      func()
}

func (m *themeSettingsMenu) choices() []settingslist.Item {
	items := make([]settingslist.Item, 0, len(m.themes))
	for _, name := range m.themes {
		items = append(items, settingslist.Item{ID: name, Label: name, CurrentValue: name, Values: []string{name}})
	}
	return items
}

func (m *themeSettingsMenu) showSingle(current string) {
	items := m.choices()
	automatic := settingslist.Item{ID: "/", Label: "automatic", Description: "Use separate themes for light and dark terminal appearance", Values: []string{"/"}}
	index := 0
	if len(items) > 0 && items[0].ID == systemThemeName {
		index = 1
	}
	items = slices.Insert(items, index, automatic)
	preview := func(value string) {
		if value == "/" {
			value = m.light + "/" + m.dark
		}
		m.app.applyTheme(value)
	}
	m.List = settingslist.New(items, 8, settingslist.DefaultTheme(), func(id, value string) {
		if id == "/" {
			preview(id)
			m.showAutomatic()
		} else {
			m.done(value)
		}
	}, m.cancel, settingslist.Options{SelectedID: current, OnSelectionChange: func(item settingslist.Item) { preview(item.ID) }})
}

func (m *themeSettingsMenu) showAutomatic() {
	picker := func(side string) func(string, func(string, bool)) settingslist.KeyComponent {
		return func(current string, done func(string, bool)) settingslist.KeyComponent {
			return settingslist.New(m.choices(), 8, settingslist.DefaultTheme(), func(_, value string) {
				if side == "light" {
					m.light = value
				} else {
					m.dark = value
				}
				m.app.applyTheme(m.light + "/" + m.dark)
				done(value, true)
			}, func() {
				m.app.applyTheme(m.light + "/" + m.dark)
				done("", false)
			}, settingslist.Options{SelectedID: current, OnSelectionChange: func(item settingslist.Item) { m.app.applyTheme(item.ID) }})
		}
	}
	items := []settingslist.Item{
		{ID: "light", Label: "Light theme", Description: "Theme to use when the terminal is light", CurrentValue: m.light, Submenu: picker("light")},
		{ID: "dark", Label: "Dark theme", Description: "Theme to use when the terminal is dark", CurrentValue: m.dark, Submenu: picker("dark")},
		{ID: "apply", Label: "Apply", Description: "Save and go back", Values: []string{"save and go back"}},
		{ID: "single", Label: "Change mode", Description: "Switch to one theme for light and dark", Values: []string{"switch to single theme"}},
	}
	m.List = settingslist.New(items, 8, settingslist.DefaultTheme(), func(id, _ string) {
		switch id {
		case "apply":
			m.done(m.light + "/" + m.dark)
		case "single":
			current := m.dark
			if m.app.terminalThemeAppearance() == "light" {
				current = m.light
			}
			m.app.applyTheme(current)
			m.showSingle(current)
		}
	}, m.cancel, settingslist.Options{})
}

// availableThemes returns the list of theme names the App can
// switch to. The default theme is always present; theme plugins
// can extend this list by registering themes via the extensions
// API. Used by the settings submenu and the /theme command.
func (a *App) availableThemes() []string {
	if a == nil {
		return nil
	}
	names := []string{systemThemeName, "default", "tau", "dark", "light"}
	if a.themeNames != nil {
		for _, name := range a.themeNames() {
			if name != "" && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	if a.extensions != nil {
		th := a.extensions.Theme()
		if th.Name != "" && !slices.Contains(names, th.Name) {
			// Replace the default with the configured theme name
			// so the user sees the current theme reflected.
			names = append(names, th.Name)
		}
	}
	return names
}

// applySettingsChange writes a confirmed new value back into the App
// state. The dispatch picks the subsystem that owns the setting
// (model switch, thinking level toggle, auto-compaction flag, footer
// notice) and pushes the value through it. Unknown ids no-op.
func applySettingsChange(app *App, id, value string) {
	if app == nil || id == "" {
		return
	}
	switch id {
	case "settings.model":
		if value == "" || value == app.currentModel {
			return
		}
		app.applyModelFromArgs(stripProviderPrefix(value))
		if app.footer != nil && app.footer.data != nil {
			app.footer.data.model = app.sess.State().Model()
		}
		app.refreshFooterStats(app.loopContext())
	case "settings.thinkingLevel":
		if app.sess == nil {
			return
		}
		if err := app.sess.SetThinkingLevel(value); err != nil {
			app.conv.AddError("settings.thinkingLevel: " + err.Error())
			app.refreshChat()
			return
		}
		if app.footer != nil && app.footer.data != nil {
			app.footer.data.thinkingLevel = value
		}
		// Remember the effort the user was working at, so app.thinking.toggle
		// can come back to it. Recorded here rather than in the toggle handler
		// so every way of choosing a level feeds the same memory.
		app.rememberThinkingLevel(value)
		// Persist the level against the active model so starting or switching
		// back to it restores the effort.
		if err := app.persistModelThinkingSetting(app.currentModel, value); err != nil {
			app.conv.AddError("settings.thinkingLevel: " + err.Error())
		}
		// The editor's rules are the standing thinking-level indicator.
		app.refreshEditorChrome()
	case "settings.autoCompaction":
		enabled := strings.EqualFold(value, "on") || value == "true"
		if app.footer != nil && app.footer.data != nil {
			app.footer.data.autoCompaction = enabled
		}
	case "settings.layout":
		mode, ok := ParseLayoutMode(value)
		if !ok {
			app.conv.AddError("settings.layout: unknown mode " + value)
			app.refreshChat()
			return
		}
		app.SetLayoutMode(mode)
		if err := app.persistLayoutSetting(mode); err != nil && app.conv != nil {
			app.conv.AddError("settings.layout: selected " + value + " but failed to save setting: " + err.Error())
			app.refreshChat()
		}
	case "settings.theme":
		if app.footer != nil && app.footer.data != nil {
			app.footer.data.theme = value
		}
		// applyTheme resolves the theme's colours and pushes them to both the
		// chat log and the extensions registry, so nothing extra is needed
		// here — forwarding the bare name first would briefly repaint plugin
		// widgets against an empty palette.
		app.applyTheme(value)
		if err := app.persistThemeSetting(value); err != nil && app.conv != nil {
			app.conv.AddError("settings.theme: selected " + value + " but failed to save setting: " + err.Error())
		}
	case "settings.streamingFooter":
		if app.footer != nil && app.footer.data != nil {
			app.footer.data.notice = value
		}
	}
	if ctx := app.loopContext(); ctx != nil {
		if ctx.InvalidateLayout != nil {
			ctx.InvalidateLayout()
		}
		if ctx.InvalidateAll != nil {
			ctx.InvalidateAll()
		}
		if ctx.Invalidate != nil {
			if f := app.footer; f != nil {
				if r := f.Rect(); r.W > 0 && r.H > 0 {
					ctx.Invalidate(r)
				}
			}
		}
	}
	app.refreshChat()
}

func (a *App) persistThemeSetting(value string) error {
	if a == nil || a.settingsPath == "" {
		return nil
	}
	return ai.UpdateSettings(a.ctxOrBackground(), a.settingsPath, map[string]any{"theme": value})
}

// persistLayoutSetting writes the chosen layout back to settings.json so the
// choice survives a restart. Without this the setting would be readable but
// only ever writable by hand, and a switch made in the UI would silently
// revert on the next launch.
func (a *App) persistLayoutSetting(mode LayoutMode) error {
	if a == nil || a.settingsPath == "" {
		return nil
	}
	return ai.UpdateSettings(a.ctxOrBackground(), a.settingsPath, map[string]any{"layoutMode": string(mode)})
}

// persistModelThinkingSetting records the session's thinking level against the
// active model in settings.json, so the level follows the model across
// sessions and model switches instead of being one global knob.
func (a *App) persistModelThinkingSetting(modelKey, level string) error {
	if a == nil || a.settingsPath == "" || modelKey == "" {
		return nil
	}
	return ai.UpdateSettings(a.ctxOrBackground(), a.settingsPath, map[string]any{
		"models": map[string]any{modelKey: map[string]any{"thinkingLevel": level}},
	})
}

// applySavedThinkingLevel applies the per-model thinking level configured in
// settings for key, if any. Model switches otherwise inherit the previous
// model's level, which silently mismatches models configured for different
// efforts. Returns whether a level was applied.
func (a *App) applySavedThinkingLevel(key string) bool {
	if a == nil || a.sess == nil || a.settingsPath == "" {
		return false
	}
	settings, err := ai.LoadSettings(a.settingsPath)
	if err != nil {
		a.conv.AddError("thinking level: load settings: " + err.Error())
		return false
	}
	return a.applyThinkingLevel(settings.ThinkingLevelFor(key))
}

// applyThinkingLevel sets the session thinking level when it changes.
func (a *App) applyThinkingLevel(level string) bool {
	if level == "" || level == a.currentThinkingLevel() {
		return false
	}
	if err := a.sess.SetThinkingLevel(level); err != nil {
		a.conv.AddError("thinking level: " + err.Error())
		return false
	}
	if a.footer != nil && a.footer.data != nil {
		a.footer.data.thinkingLevel = level
	}
	a.refreshEditorChrome()
	a.conv.AddSystem("thinking level: " + level)
	return true
}

// switchableModelIDs returns the list of bare model ids the user can
// switch to via the settings menu. Used as the Values cycle list for
// the model setting.
func (a *App) switchableModelIDs() []string {
	if a == nil {
		return nil
	}
	models := a.availableModels()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if m == nil || m.ID == "" {
			continue
		}
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids
}

func stripProviderPrefix(value string) string {
	if idx := strings.Index(value, "/"); idx > 0 {
		return value[idx+1:]
	}
	return value
}

func boolOnOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// cmdSettingsOverlay is the /settings command handler. It opens
// the dialog picker with the settings overlay as the hosted
// component so the user gets a searchable/fuzzy settings menu
// wired to the runtime config (model, thinking level,
// auto-compaction, footer notice).
func cmdSettingsOverlay(app *App, _ string) {
	if app == nil {
		return
	}
	if app.dialogPicker == nil {
		return
	}
	app.closeDialogComponent()
	overlay := newSettingsOverlay(app)
	app.dialogPicker.SetComponent(overlay)
	app.dialogPicker.SetOnCancel(func() { overlay.HandleKey(tauui.KeyEvent{Key: tauui.KeyEsc}) })
	app.dialogPicker.SetHeader("/settings")
	app.dialogPicker.Reset()
	app.dialogPicker.SetShown(true)
	if ctx := app.loopContext(); ctx != nil && ctx.InvalidateLayout != nil {
		ctx.InvalidateLayout()
	}
}
