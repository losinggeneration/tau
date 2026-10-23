package tui

import (
	"os"
	"reflect"
	"strings"
	"time"

	tauui "github.com/losinggeneration/tau/tui"
)

const systemThemeName = "system"

type terminalThemeState struct {
	fileError  string
	setting    string
	active     string
	colors     tauui.TerminalColors
	scheme     string
	ready      bool
	started    bool
	autoSync   bool
	inMemory   bool
	publishing bool
}

func (a *App) terminalThemeAppearance() string {
	state := &a.terminalTheme
	if state.colors.Background != nil {
		return terminalAppearance(*state.colors.Background, state.colors.Foreground)
	}
	if state.scheme != "" {
		return state.scheme
	}
	if appearance := colorFgBgAppearance(os.Getenv("COLORFGBG")); appearance != "" {
		return appearance
	}
	return "dark"
}

func (a *App) resolvedThemeName() string {
	setting := a.terminalTheme.setting
	if setting == "" {
		return systemThemeName
	}
	return resolveThemeSetting(setting, a.terminalThemeAppearance())
}

func (a *App) setThemeNotifications() {
	if a.app == nil {
		return
	}
	if err := a.app.SetTerminalColorSchemeNotifications(a.terminalTheme.autoSync); err != nil {
		a.conv.AddError("theme notifications: " + err.Error())
	}
}

func (a *App) startTerminalTheme() {
	a.terminalTheme.started = true
	a.setThemeNotifications()
	a.refreshNativeTheme()
	a.queryTerminalThemeColors()
}

func (a *App) queryTerminalThemeColors() {
	if a.app == nil || !a.terminalTheme.started {
		return
	}
	if err := a.app.QueryTerminalColors(100*time.Millisecond, a.applyTerminalThemeColors); err != nil {
		// Unsupported or failed color queries use the same fallback as a timeout.
		a.applyTerminalThemeColors(tauui.TerminalColors{})
	}
}

func (a *App) applyTerminalThemeColors(reported tauui.TerminalColors) {
	state := &a.terminalTheme
	next := state.colors
	if reported.Foreground != nil {
		next.Foreground = new(*reported.Foreground)
	}
	if reported.Background != nil {
		next.Background = new(*reported.Background)
	}
	if reported.Palette != nil {
		next.Palette = new(*reported.Palette)
	}
	if state.ready && reflect.DeepEqual(next, state.colors) {
		return
	}
	state.colors = next
	state.ready = true
	a.reapplyTerminalTheme()
}

func (a *App) applyTerminalThemeScheme(scheme string) {
	state := &a.terminalTheme
	if !state.autoSync || (scheme != "dark" && scheme != "light") {
		return
	}
	previous := a.terminalThemeAppearance()
	state.scheme = scheme
	if previous != a.terminalThemeAppearance() {
		a.reapplyTerminalTheme()
	}
	a.queryTerminalThemeColors()
}

func (a *App) reapplyTerminalTheme() {
	state := &a.terminalTheme
	if state.inMemory {
		return
	}
	name := a.resolvedThemeName()
	a.renderTheme(name)
}

func (a *App) applyTheme(setting string) {
	if a == nil {
		return
	}
	state := &a.terminalTheme
	state.setting = strings.TrimSpace(setting)
	state.inMemory = false
	name := a.resolvedThemeName()
	a.renderTheme(name)
	a.queryTerminalThemeColors()
}

func (a *App) renderTheme(name string) {
	definition, ok := a.themeDefinitionFor(name)
	if !ok {
		name = systemThemeName
		definition, _ = a.themeDefinitionFor(name)
	}
	_, _, pair := strings.Cut(a.terminalTheme.setting, "/")
	a.terminalTheme.autoSync = name == systemThemeName || pair
	a.setThemeNotifications()
	a.terminalTheme.active = name
	if a.footer != nil && a.footer.data != nil {
		a.footer.data.theme = name
	}
	if a.extensions != nil {
		a.terminalTheme.publishing = true
		a.extensions.SetTheme(definition.resolved(name, a.terminalTheme.colors, a.terminalThemeAppearance()))
		a.terminalTheme.publishing = false
	}
	if a.chat != nil {
		a.chat.setThemeDefinition(name, definition)
	}
	a.refreshEditorChrome()
	a.refreshNativeTheme()
	if ctx := a.loopContext(); ctx != nil {
		if ctx.InvalidateLayout != nil {
			ctx.InvalidateLayout()
		}
		if ctx.InvalidateAll != nil {
			ctx.InvalidateAll()
		}
	}
	a.refreshChat()
}

func (a *App) refreshNativeTheme() {
	if a.app == nil || !a.terminalTheme.started || a.chat == nil {
		return
	}
	tokens := a.chat.ThemeTokens()
	a.app.SetTheme(tokens.nativeTheme())
}
