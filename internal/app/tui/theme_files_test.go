package tui

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/agent/artifacts"
	tauui "github.com/losinggeneration/tau/tui"
)

func TestDecodeThemeColorsRejectsInvalidValues(t *testing.T) {
	for _, raw := range [][]byte{
		completeTheme(t, "bad", map[string]any{"text": "#invalid"}, nil),
		completeTheme(t, "bad", map[string]any{"text": "a"}, map[string]any{"vars": map[string]any{"a": "b", "b": "a"}}),
	} {
		if _, err := DecodeThemeColors(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	values, err := DecodeThemeColors(completeTheme(t, "good", map[string]any{"text": "a"}, map[string]any{"vars": map[string]any{"a": "b", "b": "#123456"}}))
	if err != nil || values["text"] != "#123456" {
		t.Fatalf("values=%v err=%v", values, err)
	}
}

func TestThemeRejectsInvalidIdentity(t *testing.T) {
	for _, raw := range [][]byte{
		completeTheme(t, "../outside", nil, nil),
		completeTheme(t, "theme", nil, map[string]any{"appearance": "blue"}),
	} {
		if _, err := DecodeThemeColors(raw); err == nil {
			t.Fatalf("invalid identity accepted: %s", raw)
		}
	}
}

func TestDecodeThemeColorsAcceptsPaletteAndTerminalDefault(t *testing.T) {
	values, err := DecodeThemeColors(completeTheme(t, "palette",
		map[string]any{"accent": "accent", "text": 255, "userMessageBg": "terminal"},
		map[string]any{"vars": map[string]any{"accent": 42, "terminal": ""}, "export": map[string]any{"pageBg": 0}}))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"accent": "42", "text": "255", "userMessageBg": "default"} {
		if values[name] != want {
			t.Errorf("%s=%q, want %q", name, values[name], want)
		}
	}
	for _, value := range []any{-1, 256, 1.5, true, nil, map[string]any{}} {
		if _, err := DecodeThemeColors(completeTheme(t, "invalid", map[string]any{"text": value}, nil)); err == nil {
			t.Errorf("accepted %v", value)
		}
	}
}

func TestDecodeThemeColorsValidatesUnusedDeclarations(t *testing.T) {
	for _, extra := range []map[string]any{
		{"vars": map[string]any{"unused": "#invalid"}},
		{"vars": map[string]any{"a": "b", "b": "a"}},
		{"export": map[string]any{"pageBg": "unknown"}},
	} {
		if _, err := DecodeThemeColors(completeTheme(t, "bad", nil, extra)); err == nil {
			t.Errorf("accepted invalid declaration: %v", extra)
		}
	}
}

// completeTheme returns a theme file that defines every required token, with
// the given colors replacing the filler.
func completeTheme(t *testing.T, name string, colors map[string]any, extra map[string]any) []byte {
	t.Helper()
	all := make(map[string]any, len(specThemeTokenNames))
	for _, token := range specThemeTokenNames {
		all[token] = "#000000"
	}
	maps.Copy(all, colors)
	file := map[string]any{"name": name, "colors": all}
	maps.Copy(file, extra)
	raw, err := json.Marshal(file)
	require.NoError(t, err)
	return raw
}

func TestDecodeThemeColorsFollowsPiSchema(t *testing.T) {
	values, err := DecodeThemeColors(completeTheme(t, "full", map[string]any{"thinkingXhigh": "#abcdef", "error": "#ff0000"}, nil))
	require.NoError(t, err)
	assert.Equal(t, "#abcdef", values["thinkingMax"], "thinkingMax falls back to thinkingXhigh")
	assert.Equal(t, "#000000", values["searchMatchBg"], "searchMatchBg falls back to selectedBg")
	assert.Equal(t, "default", values["surface"], "an omitted tau surface lets the terminal background show")
	assert.Equal(t, "#ff0000", values["compactionFallbackLabel"])

	partial := []byte(`{"name":"partial","colors":{"text":"#ffffff"}}`)
	_, err = DecodeThemeColors(partial)
	require.ErrorContains(t, err, "missing required color tokens: accent, bashMode, border")

	for name, raw := range map[string][]byte{
		"unknown token":     completeTheme(t, "bad", map[string]any{"userMessageBackground": "#000000"}, nil),
		"unknown key":       completeTheme(t, "bad", nil, map[string]any{"palette": map[string]any{}}),
		"unknown export":    completeTheme(t, "bad", nil, map[string]any{"export": map[string]any{"footerBg": "#000000"}}),
		"missing reference": completeTheme(t, "bad", map[string]any{"text": "brand"}, nil),
		"tau var syntax":    completeTheme(t, "bad", map[string]any{"text": "$brand"}, map[string]any{"vars": map[string]any{"brand": "#123456"}}),
		"missing name":      []byte(`{"colors":{}}`),
	} {
		_, err := DecodeThemeColors(raw)
		assert.Error(t, err, name)
	}
	_, err = DecodeThemeColors(completeTheme(t, "bad", map[string]any{"text": "brand"}, nil))
	require.ErrorContains(t, err, "variable reference not found")
}

func TestBuiltinThemesAreUpstreamPalettes(t *testing.T) {
	for _, entry := range artifacts.BuiltinThemes() {
		values, err := DecodeThemeColors(entry.Raw)
		require.NoError(t, err, entry.SourcePath)
		for _, token := range specThemeTokenNames {
			_, ok := parseThemeColor(values[token])
			require.True(t, ok, "%s %s=%q", entry.SourcePath, token, values[token])
		}
	}
	light, err := DecodeThemeColors(artifacts.BuiltinThemes()[1].Raw)
	require.NoError(t, err)
	text, ok := parseThemeColor(light["text"])
	require.True(t, ok)
	assert.Equal(t, tauui.ColorRGB(0x3b, 0x3f, 0x41), text, "light text is okhsl(225 5%% 27%%)")
}

func TestResolveThemeSettingUsesColorFgBg(t *testing.T) {
	for _, tc := range []struct{ setting, colorfgbg, want string }{
		{"midnight", "0;15", "midnight"},
		{"light/dark", "0;15", "light"},
		{" paper / ink ", "15;0", "ink"},
		{"light/dark", "0;default;7", "light"},
		{"light/dark", "15;8", "dark"},
		{"light/dark", "", "dark"},
		{"light/dark", "0;42", "dark"},
		{"a/b/c", "0;15", "system"},
		{"/dark", "0;15", "system"},
	} {
		t.Setenv("COLORFGBG", tc.colorfgbg)
		assert.Equal(t, tc.want, resolveThemeSetting(tc.setting), "%+v", tc)
	}
}

func TestDecodeThemeColorsRejectsInvalidMetadataAndTrailingJSON(t *testing.T) {
	for _, field := range []string{"$schema", "appearance", "vars", "export"} {
		t.Run(field, func(t *testing.T) {
			_, err := DecodeThemeColors(completeTheme(t, "invalid", nil, map[string]any{field: nil}))
			require.Error(t, err)
		})
	}
	for label, extra := range map[string]map[string]any{
		"empty appearance": {"appearance": ""},
		"case alias":       {"NAME": "alias"},
	} {
		t.Run(label, func(t *testing.T) {
			_, err := DecodeThemeColors(completeTheme(t, "invalid", nil, extra))
			require.Error(t, err)
		})
	}
	raw := append(completeTheme(t, "invalid", nil, nil), []byte(` {"name":"second"}`)...)
	_, err := DecodeThemeColors(raw)
	require.Error(t, err)
}

func TestDecodeThemePreservesAndInfersAppearance(t *testing.T) {
	for _, entry := range artifacts.BuiltinThemes() {
		theme, err := DecodeTheme(entry.Raw)
		require.NoError(t, err)
		assert.Equal(t, strings.TrimPrefix(entry.SourcePath, "builtin:"), theme.Appearance)
		var file map[string]any
		require.NoError(t, json.Unmarshal(entry.Raw, &file))
		delete(file, "appearance")
		raw, err := json.Marshal(file)
		require.NoError(t, err)
		theme, err = DecodeTheme(raw)
		require.NoError(t, err)
		assert.Equal(t, strings.TrimPrefix(entry.SourcePath, "builtin:"), theme.appearance("dark"))
	}
	for _, tc := range []struct{ name, fg, bg, declared, want string }{
		{"explicit", "#ffffff", "#000000", "light", "light"},
		{"foreground only", "#000000", "default", "", "light"},
		{"background only", "default", "#ffffff", "", "light"},
		{"palette only", "7", "0", "", "light"},
		{"fixed palette", "16", "231", "", "light"},
		{"oklch", "oklch(70% 0 0)", "oklch(20% 0 0)", "", "dark"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			colors := map[string]any{}
			for _, name := range specThemeTokenNames {
				if themeBackgroundToken(name) {
					colors[name] = tc.bg
				} else {
					colors[name] = tc.fg
				}
			}
			extra := map[string]any{}
			if tc.declared != "" {
				extra["appearance"] = tc.declared
			}
			theme, err := DecodeTheme(completeTheme(t, "test", colors, extra))
			require.NoError(t, err)
			assert.Equal(t, tc.want, theme.appearance("light"))
		})
	}
}

func TestThemeConcreteDefaultsDoNotReplaceNativeDefaults(t *testing.T) {
	definition := ThemeDefinition{Values: map[string]string{"text": "default", "userMessageBg": "default", "accent": "4"}, Appearance: "dark"}
	theme := definition.resolved("custom", tauui.TerminalColors{}, "light")
	assert.Equal(t, "dark", theme.Appearance)
	assert.Equal(t, "#e5e5e7", theme.Colors["text"])
	assert.Equal(t, "#000000", theme.Colors["userMessageBg"])
	definition.Appearance = "light"
	theme = definition.resolved("custom", tauui.TerminalColors{}, "dark")
	assert.Equal(t, "#000000", theme.Colors["text"])
	assert.Equal(t, "#ffffff", theme.Colors["userMessageBg"])
	foreground := tauui.RGBColor{R: 200, G: 210, B: 220}
	background := tauui.RGBColor{R: 10, G: 20, B: 30}
	theme = definition.resolved("custom", tauui.TerminalColors{Foreground: &foreground, Background: &background}, "dark")
	assert.Equal(t, "#c8d2dc", theme.Colors["text"])
	assert.Equal(t, "#0a141e", theme.Colors["userMessageBg"])
	assert.Equal(t, "4", theme.Colors["accent"], "palette identities remain terminal controlled")
	assert.Equal(t, "default", theme.Values["text"])
	base := defaultThemeTokens()
	tokens := base.withOverrides(theme.Values)
	assert.Equal(t, tauui.ColorDefault, tokens.Text)
	assert.Equal(t, tauui.ColorDefault, tokens.UserMessageBg)
	assert.Equal(t, "default", definition.Values["text"])
}

func TestFaintInspectionColorsMixInOKLCHWithoutChangingValues(t *testing.T) {
	definition := ThemeDefinition{Values: map[string]string{"text": "#ffffff", "muted": "#ffffff", "toolOutput": "default", "userMessageBg": "default", "accent": "#ff0000"}, Appearance: "dark", Dim: []string{"muted", "toolOutput", "userMessageBg", "missing"}}
	theme := definition.resolved("faint", tauui.TerminalColors{}, "dark")
	assert.Equal(t, "#808080", theme.Colors["muted"], "40% OKLCH mix differs from sRGB #999999")
	assert.Equal(t, "#727273", theme.Colors["toolOutput"])
	assert.Equal(t, "#ffffff", theme.Colors["text"])
	assert.Equal(t, "#000000", theme.Colors["userMessageBg"], "backgrounds never become faint")
	assert.Equal(t, "#ff0000", theme.Colors["accent"])
	assert.Equal(t, "default", definition.Values["toolOutput"])
	assert.Equal(t, "#ffffff", definition.Values["muted"])
	_, ok := theme.Colors["missing"]
	assert.False(t, ok)
}
