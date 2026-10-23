package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tauui "github.com/losinggeneration/tau/tui"
)

func TestParseThemeColorReadsOKLCHAndOKHSL(t *testing.T) {
	for raw, want := range map[string]tauui.Color{
		"okhsl(29.23 100% 56.8%)":          tauui.ColorRGB(255, 0, 0),
		"OKHSL(29.23deg 1 0.568)":          tauui.ColorRGB(255, 0, 0),
		"oklch(0.627955 0.257683 29.2339)": tauui.ColorRGB(255, 0, 0),
		"oklch(100% 0.3 150)":              tauui.ColorRGB(255, 255, 255),
		"oklch(0 0.3 150deg)":              tauui.ColorRGB(0, 0, 0),
		"okhsl(0 0% 100%)":                 tauui.ColorRGB(255, 255, 255),
		"okhsl(231 68% 47%)":               tauui.ColorRGB(47, 120, 153),
	} {
		got, ok := parseThemeColor(raw)
		require.True(t, ok, raw)
		assert.Equal(t, want, got, raw)
	}
	for _, raw := range []string{"okhsl(250 160% 55%)", "okhsl(250 50% 101%)", "oklch(120% 0.1 20)", "oklch(50% -0.1 20)", "oklch(50% 0.1)", "oklch(1e999 0 0)"} {
		_, ok := parseThemeColor(raw)
		assert.False(t, ok, raw)
	}
}

func TestNativeThemeUsesActiveTokens(t *testing.T) {
	tokens := defaultThemeTokens()
	tokens.Surface = tauui.ColorDefault
	tokens.Text = tauui.ColorRGB(0x12, 0x34, 0x56)
	theme := tokens.nativeTheme()
	assert.Equal(t, tokens.Surface, theme.Base.BG)
	assert.Equal(t, tokens.Text, theme.Base.FG)
	assert.Equal(t, tokens.Muted, theme.Palette.Placeholder.FG)
	assert.Equal(t, tokens.SelectedBg, theme.Palette.Selection.BG)
	assert.Equal(t, tokens.Border, theme.Palette.Border.FG)
	assert.Equal(t, tokens.Accent, theme.Palette.Accent.FG)
}

func TestFaintForegroundDoesNotLeakToOtherRoles(t *testing.T) {
	require.LessOrEqual(t, len(nativeTokenNames), 64, "faint token bitset must cover every native token")
	base := defaultThemeTokens()
	tokens := base.withDim([]string{"toolOutput", "toolDiffContext", "border", "thinkingHigh", "dim", "muted", "syntaxComment", "mdQuote", "selectedBg"})
	p := tokens.chatPalette()
	assert.NotZero(t, p.toolResult.Attr&tauui.AttrDim)
	assert.Zero(t, p.toolCall.Attr&tauui.AttrDim)
	assert.NotZero(t, p.borderStyle(tauui.DynamicBorderNeutral, tokens.Surface).Attr&tauui.AttrDim)
	assert.Zero(t, p.borderStyle(tauui.DynamicBorderSuccess, tokens.Surface).Attr&tauui.AttrDim)
	assert.NotZero(t, tokens.editorBorderStyle("high").Attr&tauui.AttrDim)
	assert.Zero(t, tokens.editorBorderStyle("low").Attr&tauui.AttrDim)
	footer := &Footer{tokens: func() themeTokens { return tokens }, data: &footerData{contextWindow: 100, contextUsedPct: 95}}
	assert.NotZero(t, footer.baseStyle(nil).Attr&tauui.AttrDim)
	assert.Zero(t, footer.contextStyle(footer.baseStyle(nil)).Attr&tauui.AttrDim)
	assert.Zero(t, tokens.nativeTheme().Palette.Selection.Attr&tauui.AttrDim, "background tokens cannot add faint")
	assert.NotZero(t, tokens.foreground("mdQuote", tokens.MdQuote, tauui.Style{}).Attr&tauui.AttrDim)
	assert.NotZero(t, tokens.foreground("syntaxComment", tokens.SyntaxComment, tauui.Style{}).Attr&tauui.AttrDim)
	assert.Equal(t, tokens, tokens.withDim([]string{"dim", "muted", "border", "thinkingHigh", "toolOutput", "toolDiffContext", "syntaxComment", "mdQuote", "muted"}))
}

func TestEditorBorderUsesEveryThinkingToken(t *testing.T) {
	tokens := defaultThemeTokens()
	tokens.Border = tauui.ColorRGB(1, 2, 3)
	tokens.ThinkingOff = tauui.ColorRGB(4, 5, 6)
	tokens = tokens.withDim([]string{"thinkingOff", "thinkingMax"})
	for level, want := range map[string]tauui.Color{
		"": tokens.ThinkingOff, "off": tokens.ThinkingOff, "unknown": tokens.ThinkingOff,
		"minimal": tokens.ThinkingMinimal, "low": tokens.ThinkingLow, "medium": tokens.ThinkingMedium,
		"high": tokens.ThinkingHigh, "xhigh": tokens.ThinkingXHigh, " MAX ": tokens.ThinkingMax,
	} {
		st := tokens.editorBorderStyle(level)
		assert.Equal(t, want, st.FG, level)
		assert.Equal(t, tokens.Surface, st.BG, level)
		assert.Equal(t, level == "" || level == "off" || level == "unknown" || level == " MAX ", st.Attr&tauui.AttrDim != 0, level)
	}
}
