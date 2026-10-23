package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func systemTestRGB(t *testing.T, hex string) tauui.RGBColor {
	t.Helper()
	value, err := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	require.NoError(t, err)
	return tauui.RGBColor{R: uint8(value >> 16), G: uint8(value >> 8), B: uint8(value)}
}

func systemTestInput(t *testing.T) tauui.TerminalColors {
	t.Helper()
	var palette [16]tauui.RGBColor
	for i, hex := range []string{"21222c", "ff5555", "50fa7b", "f1fa8c", "bd93f9", "ff79c6", "8be9fd", "f8f8f2", "6272a4", "ff6e6e", "69ff94", "ffffa5", "d6acff", "ff92df", "a4ffff", "ffffff"} {
		palette[i] = systemTestRGB(t, hex)
	}
	return tauui.TerminalColors{Background: new(systemTestRGB(t, "282a36")), Foreground: new(systemTestRGB(t, "f8f8f2")), Palette: &palette}
}

func TestGenerateSystemThemeContrastAndRoles(t *testing.T) {
	for name, input := range map[string]tauui.TerminalColors{
		"dracula":        systemTestInput(t),
		"solarized":      {Background: new(systemTestRGB(t, "fdf6e3")), Foreground: new(systemTestRGB(t, "657b83"))},
		"backgroundOnly": {Background: new(systemTestRGB(t, "1e1e1e"))},
		"midGray":        {Background: new(systemTestRGB(t, "808080")), Foreground: new(systemTestRGB(t, "ffffff"))},
	} {
		t.Run(name, func(t *testing.T) {
			result := generateSystemTheme(input, 1, "")
			resolve := func(token string) tauui.RGBColor {
				raw, ok := result.colors[token]
				require.True(t, ok, token)
				if raw == "default" {
					require.NotNil(t, input.Foreground)
					return *input.Foreground
				}
				return systemTestRGB(t, raw)
			}
			for _, surface := range []tauui.RGBColor{*input.Background, resolve("selectedBg")} {
				assert.GreaterOrEqual(t, wcagContrast(resolve("text"), surface), 4.5)
			}
			assert.GreaterOrEqual(t, wcagContrast(resolve("toolTitle"), resolve("toolErrorBg")), 4.5)
			if name != "midGray" {
				offset := func(token string) float64 {
					return math.Abs(rgbToOklab(resolve(token))[0] - rgbToOklab(*input.Background)[0])
				}
				assert.Greater(t, offset("text"), offset("muted"))
				assert.Greater(t, offset("muted"), offset("dim"))
			}
			for _, panel := range systemPanels {
				assert.Less(t, wcagContrast(resolve(panel), *input.Background), 2.0, panel)
				assert.Equal(t, result.appearance == "dark", rgbToOklab(resolve(panel))[0] > rgbToOklab(*input.Background)[0], panel)
			}
			for _, token := range specThemeTokenNames {
				_, ok := result.colors[token]
				assert.True(t, ok, token)
			}
			if name == "dracula" {
				assert.Equal(t, "default", result.colors["text"])
			}
			if name == "solarized" {
				assert.NotEqual(t, "default", result.colors["text"])
			}
		})
	}
}

func TestGenerateSystemThemePreservesPastelChroma(t *testing.T) {
	var palette [16]tauui.RGBColor
	for i, hex := range []string{"51576d", "e78284", "a6d189", "e5c890", "8caaee", "f4b8e4", "81c8be", "b5bfe2", "626880", "e67172", "8ec772", "d9ba73", "7b9ef0", "f2a4db", "5abfb5", "a5adce"} {
		palette[i] = systemTestRGB(t, hex)
	}
	input := tauui.TerminalColors{Background: new(systemTestRGB(t, "303446")), Foreground: new(systemTestRGB(t, "c6d0f5")), Palette: &palette}
	result := generateSystemTheme(input, 1, "")
	chroma := func(color tauui.RGBColor) float64 { lab := rgbToOklab(color); return math.Hypot(lab[1], lab[2]) }
	accent := systemTestRGB(t, result.colors["accent"])
	assert.Less(t, rgbToOklab(accent)[0], rgbToOklab(palette[5])[0]-0.05)
	assert.LessOrEqual(t, chroma(accent), chroma(palette[5])*1.03)
	for _, panel := range []string{"userMessageBg", "customMessageBg"} {
		assert.LessOrEqual(t, chroma(systemTestRGB(t, result.colors[panel])), 0.1)
	}
	grayscale := generateSystemTheme(input, 0, "")
	assert.Less(t, chroma(systemTestRGB(t, grayscale.colors["error"])), 0.005)
}

func TestGenerateSystemThemeIndexedAndPending(t *testing.T) {
	result := generateSystemTheme(tauui.TerminalColors{}, 1, "light")
	assert.Equal(t, "light", result.appearance)
	assert.Equal(t, "1", result.colors["error"])
	assert.Equal(t, "default", result.colors["text"])
	assert.Equal(t, "default", result.colors["userMessageBg"])
	assert.Equal(t, "2", result.colors["syntaxString"])
	assert.Equal(t, "5", result.colors["syntaxNumber"])
	assert.Contains(t, result.dim, "muted")
	assert.NotContains(t, result.dim, "text")
	assert.Equal(t, "default", generateSystemTheme(tauui.TerminalColors{}, 0, "").colors["error"])
}

func TestTerminalAppearanceUsesReadableDirection(t *testing.T) {
	for _, tc := range []struct{ bg, fg, want string }{
		{"000000", "ffffff", "dark"},
		{"ffffff", "000000", "light"},
		{"808080", "ffffff", "light"},
		{"1e1e1e", "000000", "dark"},
		{"fdf6e3", "657b83", "light"},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			assert.Equal(t, tc.want, terminalAppearance(systemTestRGB(t, tc.bg), new(systemTestRGB(t, tc.fg))))
		})
	}
}
