package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/losinggeneration/rovel/backend"
	"github.com/losinggeneration/rovel/backend/headless"
	"github.com/losinggeneration/tau/ai"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/extensions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemThemeStartsPendingAndCannotBeShadowed(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}, ThemeColors: func(string) (map[string]string, bool) { return map[string]string{"text": "#ff0000"}, true }})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	assert.Equal(t, "system", a.footer.data.theme)
	assert.Equal(t, "system", a.availableThemes()[0])
	assert.Equal(t, tauui.ColorDefault, a.chat.ThemeTokens().Text)
	assert.Equal(t, tauui.ColorDefault, a.chat.ThemeTokens().Surface)
	assert.Equal(t, "default", a.extensions.Theme().Values["error"])
	a.applyTerminalThemeColors(tauui.TerminalColors{})
	assert.Equal(t, "1", a.extensions.Theme().Values["error"])
}

func TestTerminalThemeMergesPartialReportsAndProtectsExtensionTheme(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	a.applyTerminalThemeColors(systemTestInput(t))
	first := a.chat.ThemeTokens()
	var publications int
	a.extensions.OnThemeChanged(func(theme extensions.Theme) { publications++; a.applyThemeFromExtension(theme) })
	a.applyTerminalThemeColors(tauui.TerminalColors{})
	a.applyTerminalThemeColors(systemTestInput(t))
	assert.Zero(t, publications, "identical reports and timeouts must not repaint")
	assert.Equal(t, first, a.chat.ThemeTokens())
	a.applyTerminalThemeColors(tauui.TerminalColors{Background: new(tauui.RGBColor{R: 250, G: 250, B: 250})})
	require.Equal(t, 1, publications)
	require.NotNil(t, a.terminalTheme.colors.Foreground)
	require.NotNil(t, a.terminalTheme.colors.Palette)
	assert.Equal(t, *systemTestInput(t).Foreground, *a.terminalTheme.colors.Foreground)
	assert.NotEqual(t, first, a.chat.ThemeTokens())
	a.extensions.SetTheme(extensions.Theme{Name: "system", Values: map[string]string{"text": "#123456"}})
	custom := a.chat.ThemeTokens()
	a.applyTerminalThemeColors(systemTestInput(t))
	a.applyTerminalThemeScheme("dark")
	assert.Equal(t, custom, a.chat.ThemeTokens(), "even an in-memory theme named system must survive terminal updates")
	a.applyTheme("system")
	assert.NotEqual(t, custom, a.chat.ThemeTokens(), "explicit selection restores system ownership")
	a.applyTerminalThemeColors(tauui.TerminalColors{Background: new(tauui.RGBColor{R: 250, G: 250, B: 250})})
	assert.NotEqual(t, custom, a.chat.ThemeTokens())
}

func TestThemePairUsesReportedColorsBeforeSchemeAndEnvironment(t *testing.T) {
	t.Setenv("COLORFGBG", "0;15")
	a, err := New(Options{Session: &fakeSession{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	a.applyTheme("light/dark")
	assert.Equal(t, "light", a.footer.data.theme)
	a.applyTerminalThemeScheme("dark")
	assert.Equal(t, "dark", a.footer.data.theme)
	a.applyTerminalThemeColors(tauui.TerminalColors{Background: new(tauui.RGBColor{R: 255, G: 255, B: 255})})
	assert.Equal(t, "light", a.footer.data.theme)
	a.applyTerminalThemeScheme("dark")
	assert.Equal(t, "light", a.footer.data.theme)
	a.applyTheme("tau")
	a.applyTerminalThemeColors(systemTestInput(t))
	assert.Equal(t, "tau", a.footer.data.theme)
}

func TestTerminalThemeQueriesOnLoopAndRequeriesOnSchemeChange(t *testing.T) {
	for _, setting := range []string{"system", "missing"} {
		t.Run(setting, func(t *testing.T) {
			t.Setenv("TAU_PROGRAM_STATUS", "")
			t.Setenv("PI_PROGRAM_STATUS", "0")
			a := newTestApp(t)
			a.footer = newFooterForTest()
			a.extensions = extensions.New()
			a.extensions.OnThemeChanged(func(theme extensions.Theme) { a.applyThemeFromExtension(theme) })
			a.applyTheme(setting)
			assert.Equal(t, "system", a.terminalTheme.active)
			be := headless.New(tauui.Size{W: 20, H: 4})
			t.Cleanup(be.Close)
			toolkit, err := tauui.New(tauui.Opts{
				Root: tauui.NewText("theme"), Backend: be,
				OnTerminalColorScheme: a.applyTerminalThemeScheme,
				OnTerminalReply: func(reply tauui.TerminalReplyEvent) {
					if reply.Sequence != "\x1b]12;finished\a" {
						return
					}
					assert.Equal(t, "light", a.terminalThemeAppearance())
					assert.Equal(t, tauui.ColorDefault, a.chat.ThemeTokens().Surface)
					toolkitOutput := string(be.Output())
					assert.Equal(t, 2, strings.Count(toolkitOutput, "\x1b]10;?\a"))
					assert.Contains(t, toolkitOutput, "\x1b[?2031h")
					a.app.Quit()
				},
			})
			require.NoError(t, err)
			a.app = toolkit
			require.NoError(t, toolkit.Post(func(*tauui.UpdateCtx) { a.startTerminalTheme() }))
			be.SendEvent(tauui.TerminalReplyEvent{Sequence: "\x1b]11;#ffffff\a"})
			be.SendEvent(tauui.TerminalReplyEvent{Sequence: "\x1b[?1;2c"})
			be.SendEvent(tauui.TerminalReplyEvent{Sequence: "\x1b[?997;1n"})
			be.SendEvent(tauui.TerminalReplyEvent{Sequence: "\x1b[?1;2c"})
			be.SendEvent(tauui.TerminalReplyEvent{Sequence: "\x1b]12;finished\a"})
			require.NoError(t, toolkit.Run())
			assert.Contains(t, string(be.Output()), "\x1b[?2031l")
		})
	}
}

func TestTerminalThemeQueryFailureLeavesIndexedFallback(t *testing.T) {
	a := newTestApp(t)
	a.extensions = extensions.New()
	a.applyTheme("system")
	toolkit, err := tauui.New(tauui.Opts{Root: tauui.NewText("theme"), Backend: themeQueryFailureBackend{headless.New(tauui.Size{W: 20, H: 4})}})
	require.NoError(t, err)
	a.app = toolkit
	require.NoError(t, toolkit.Start())
	defer func() { require.NoError(t, toolkit.Stop()) }()
	a.startTerminalTheme()
	assert.Equal(t, "1", a.extensions.Theme().Values["error"])
}

type themeQueryFailureBackend struct{ *headless.Backend }

func (b themeQueryFailureBackend) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "\x1b]10;?") {
		return 0, errors.New("query failed")
	}
	return b.Backend.Write(p)
}

func TestFileThemeMetadataTracksTerminalWithoutChangingNativeDefaults(t *testing.T) {
	a := newTestApp(t)
	a.extensions = extensions.New()
	values := map[string]string{}
	for _, token := range specThemeTokenNames {
		values[token] = "default"
	}
	a.themeLookup = func(name string) (ThemeDefinition, bool) { return ThemeDefinition{Values: values}, name == "custom" }
	a.applyTheme("custom")
	assert.Equal(t, "dark", a.extensions.Theme().Appearance)
	assert.Equal(t, "#e5e5e7", a.extensions.Theme().Colors["text"])
	assert.Equal(t, tauui.ColorDefault, a.chat.ThemeTokens().Text)
	a.applyTerminalThemeColors(tauui.TerminalColors{Foreground: new(tauui.RGBColor{R: 1, G: 2, B: 3}), Background: new(tauui.RGBColor{R: 250, G: 250, B: 250})})
	theme := a.extensions.Theme()
	assert.Equal(t, "light", theme.Appearance)
	assert.Equal(t, "#010203", theme.Colors["text"])
	assert.Equal(t, "#fafafa", theme.Colors["userMessageBg"])
	assert.Equal(t, "default", theme.Values["text"])
	assert.Equal(t, tauui.ColorDefault, a.chat.ThemeTokens().Text)
	assert.Equal(t, "default", values["text"], "lookup maps are never rewritten")
	a.themeLookup = func(string) (ThemeDefinition, bool) { return ThemeDefinition{Values: values, Appearance: "dark"}, true }
	a.applyTheme("custom")
	assert.Equal(t, "dark", a.extensions.Theme().Appearance)
}

func TestSystemThemeFaintTokensReachNativeStylesAndInspection(t *testing.T) {
	a := newTestApp(t)
	a.extensions = extensions.New()
	a.applyTheme("system")
	tokens := a.chat.ThemeTokens()
	assert.NotZero(t, tokens.chatPalette().thinking.Attr&tauui.AttrDim)
	assert.NotZero(t, tokens.chatPalette().dim.Attr&tauui.AttrDim)
	assert.NotZero(t, tokens.nativeTheme().Palette.Placeholder.Attr&tauui.AttrDim)
	assert.Zero(t, tokens.nativeTheme().Palette.Accent.Attr&tauui.AttrDim)
	assert.Zero(t, tokens.chatPalette().assistant.Attr&tauui.AttrDim, "same terminal-default color must not make primary text faint")
	theme := a.extensions.Theme()
	assert.Contains(t, theme.Dim, "muted")
	assert.Equal(t, "default", theme.Values["muted"])
	assert.Equal(t, "#727273", theme.Colors["muted"])
	assert.Equal(t, "#e5e5e7", theme.Colors["text"])
	a.applyTerminalThemeColors(tauui.TerminalColors{Background: new(tauui.RGBColor{})})
	tokens = a.chat.ThemeTokens()
	assert.Zero(t, tokens.chatPalette().thinking.Attr&tauui.AttrDim)
	assert.Empty(t, a.extensions.Theme().Dim)
}

func TestExtensionFaintMetadataSurvivesApplication(t *testing.T) {
	a := newTestApp(t)
	a.applyThemeFromExtension(extensions.Theme{Name: "memory", Values: map[string]string{"thinkingText": "default"}, Dim: []string{"thinkingText"}})
	assert.NotZero(t, a.chat.pal().thinking.Attr&tauui.AttrDim)
	a.applyTheme("default")
	assert.Zero(t, a.chat.pal().thinking.Attr&tauui.AttrDim)
}

func TestUnavailableThemeFallsBackToSystemAndRecoversOnReload(t *testing.T) {
	for _, setting := range []string{"missing", "a/b/c", "/dark", "missing/missing"} {
		t.Run(setting, func(t *testing.T) {
			sess := &settingsSession{fakeSession: &fakeSession{}, settings: &ai.Settings{Theme: setting}}
			available := false
			a, err := New(Options{Session: sess, ThemeLookup: func(name string) (ThemeDefinition, bool) {
				return ThemeDefinition{Values: map[string]string{"text": "#123456"}, Appearance: "light"}, available && name == "missing"
			}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
			assert.Equal(t, "system", a.footer.data.theme)
			assert.Equal(t, "system", a.extensions.Theme().Name)
			assert.True(t, a.terminalTheme.autoSync)
			assert.Equal(t, "default", a.extensions.Theme().Values["error"], "startup fallback is pending grayscale")
			a.applyTerminalThemeColors(tauui.TerminalColors{})
			assert.Equal(t, "1", a.extensions.Theme().Values["error"], "timeout uses the system indexed fallback")
			a.applyTerminalThemeColors(systemTestInput(t))
			a.applyTheme("system")
			expected := a.chat.ThemeTokens()
			a.applyTheme(setting)
			assert.Equal(t, expected, a.chat.ThemeTokens())
			a.applyTerminalThemeColors(tauui.TerminalColors{Background: new(tauui.RGBColor{R: 250, G: 250, B: 250})})
			assert.Equal(t, "system", a.extensions.Theme().Name)
			assert.Equal(t, "light", a.extensions.Theme().Appearance)
			assert.NotEqual(t, expected, a.chat.ThemeTokens())
			if setting == "missing" {
				available = true
				a.reapplySessionSettings()
				assert.Equal(t, "missing", a.extensions.Theme().Name)
				assert.Equal(t, "#123456", a.extensions.Theme().Values["text"])
				assert.False(t, a.terminalTheme.autoSync)
				available = false
				a.reapplySessionSettings()
				assert.Equal(t, "system", a.extensions.Theme().Name)
				assert.True(t, a.terminalTheme.autoSync)
			}
			assert.Equal(t, setting, sess.settings.Theme, "fallback never rewrites settings")
		})
	}
}

func TestThemeFallbackPreservesNativeAndLegacyLookups(t *testing.T) {
	a := newTestApp(t)
	a.extensions = extensions.New()
	for _, name := range []string{"default", "tau", "dark", "light"} {
		a.applyTheme(name)
		assert.Equal(t, name, a.extensions.Theme().Name)
		assert.Equal(t, themeTokensFor(name), a.chat.ThemeTokens())
		assert.False(t, a.terminalTheme.autoSync)
	}
	a.themeColors = func(name string) (map[string]string, bool) {
		return map[string]string{"text": "#123456"}, name == "legacy"
	}
	a.applyTheme("legacy")
	assert.Equal(t, "legacy", a.extensions.Theme().Name)
	assert.Equal(t, "#123456", a.extensions.Theme().Values["text"])
	assert.False(t, a.terminalTheme.autoSync)
	a.applyTheme("missing")
	assert.Equal(t, "system", a.extensions.Theme().Name)
	assert.True(t, a.terminalTheme.autoSync)
}

type themeSuspendBackend struct {
	*headless.Backend
	signals chan backend.LifecycleSignal
}

func (b *themeSuspendBackend) Signals() <-chan backend.LifecycleSignal { return b.signals }
func (b *themeSuspendBackend) Suspend() error                          { return nil }

func TestTerminalThemeRefreshesOnResumeWithoutReplacingExtensionTheme(t *testing.T) {
	for _, selection := range []string{"system", "light/dark", "in-memory"} {
		t.Run(selection, func(t *testing.T) {
			t.Setenv("TAU_PROGRAM_STATUS", "")
			t.Setenv("PI_PROGRAM_STATUS", "0")
			a, err := New(Options{Session: &fakeSession{}, ThemeLookup: func(name string) (ThemeDefinition, bool) {
				if name == "light" {
					return ThemeDefinition{Values: map[string]string{"text": "#000000"}}, true
				}
				return ThemeDefinition{}, false
			}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
			a.applyTerminalThemeColors(systemTestInput(t))
			if selection == "in-memory" {
				a.extensions.SetTheme(extensions.Theme{Name: "system", Values: map[string]string{"text": "#123456"}})
			} else {
				a.applyTheme(selection)
			}
			before := a.chat.ThemeTokens()
			be := &themeSuspendBackend{Backend: headless.New(tauui.Size{W: 20, H: 4}), signals: make(chan backend.LifecycleSignal, 1)}
			t.Cleanup(be.Close)
			toolkit, err := tauui.New(tauui.Opts{
				Root: tauui.NewText("resume"), Backend: be, OnResume: func() {
					a.startTerminalTheme()
					assert.Equal(t, before, a.chat.ThemeTokens(), "resume retains cached colors until fresh replies arrive")
					be.SendEvent(tauui.TerminalReplyEvent{Sequence: "\x1b]10;#000000\a"})
					be.SendEvent(tauui.TerminalReplyEvent{Sequence: "\x1b]11;#ffffff\a"})
					be.SendEvent(tauui.TerminalReplyEvent{Sequence: "\x1b[?1;2c"})
					be.SendEvent(tauui.TerminalReplyEvent{Sequence: "finished"})
				},
				OnTerminalReply: func(reply tauui.TerminalReplyEvent) {
					if reply.Sequence != "finished" {
						return
					}
					assert.Equal(t, 2, strings.Count(string(be.Output()), "\x1b]10;?\a"))
					assert.Equal(t, "light", a.terminalThemeAppearance())
					if selection == "in-memory" {
						assert.Equal(t, before, a.chat.ThemeTokens())
						assert.Equal(t, "#123456", a.extensions.Theme().Values["text"])
						assert.False(t, a.terminalTheme.autoSync)
					} else {
						assert.NotEqual(t, before, a.chat.ThemeTokens())
						if selection == "light/dark" {
							assert.Equal(t, "light", a.extensions.Theme().Name)
						}
					}
					a.app.Quit()
				},
			})
			require.NoError(t, err)
			a.app = toolkit
			require.NoError(t, toolkit.Post(func(*tauui.UpdateCtx) {
				a.startTerminalTheme()
				be.signals <- backend.SignalSuspend
			}))
			require.NoError(t, toolkit.Run())
		})
	}
}
