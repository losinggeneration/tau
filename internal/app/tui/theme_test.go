package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/extensions"
)

func TestDecodeThemeColorsReadsTheSpecShape(t *testing.T) {
	raw := []byte(`{
	  "$schema": "https://example.invalid/theme.json",
	  "name": "midnight",
	  "colors": {
	    "userMessageBg": "#102030",
	    "toolTitle": "rgb(1,2,3)",
	    "accent": "42"
	  },
	  "export": { "pageBg": "#000000" }
	}`)

	colors, err := DecodeThemeColors(raw)
	if err != nil {
		t.Fatalf("DecodeThemeColors: %v", err)
	}
	want := map[string]string{
		"userMessageBg": "#102030",
		"toolTitle":     "rgb(1,2,3)",
		"accent":        "42",
	}
	for token, value := range want {
		if colors[token] != value {
			t.Errorf("colors[%q] = %q, want %q", token, colors[token], value)
		}
	}
	// `export` is for HTML export and must not leak into the terminal tokens.
	if _, ok := colors["pageBg"]; ok {
		t.Error("export colours leaked into the terminal token set")
	}
}

// `vars` is the spec's named-value mechanism; a theme that routes its palette
// through it must still produce concrete colours.
func TestDecodeThemeColorsResolvesVars(t *testing.T) {
	raw := []byte(`{
	  "name": "varsy",
	  "vars": { "brand": "#ff8800" },
	  "colors": { "accent": "$brand", "border": "{brand}", "text": "brand", "muted": "#111111" }
	}`)

	colors, err := DecodeThemeColors(raw)
	if err != nil {
		t.Fatalf("DecodeThemeColors: %v", err)
	}
	for _, token := range []string{"accent", "border", "text"} {
		if colors[token] != "#ff8800" {
			t.Errorf("colors[%q] = %q, want the var resolved to #ff8800", token, colors[token])
		}
	}
	if colors["muted"] != "#111111" {
		t.Errorf("literal colour was rewritten: %q", colors["muted"])
	}
}

func TestDecodeThemeColorsRejectsUnusableFiles(t *testing.T) {
	if _, err := DecodeThemeColors([]byte(`{ not json`)); err == nil {
		t.Error("malformed JSON accepted")
	}
	if _, err := DecodeThemeColors([]byte(`{"name":"empty"}`)); err == nil {
		t.Error("theme with no colors object accepted")
	}
}

// The end-to-end point of C2: a discovered theme file must repaint the UI.
// Before this wiring the name reached the app and the colours did not.
func TestDiscoveredThemeFileRepaintsTheChatLog(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"name":   "midnight",
		"colors": map[string]string{"userMessageBg": "#123456"},
	})
	if err != nil {
		t.Fatalf("marshal theme: %v", err)
	}

	a := newTestApp(t)
	a.themeColors = func(name string) (map[string]string, bool) {
		if name != "midnight" {
			return nil, false
		}
		colors, err := DecodeThemeColors(raw)
		if err != nil {
			t.Fatalf("DecodeThemeColors: %v", err)
		}
		return colors, true
	}

	a.applyTheme("midnight")

	if got, want := a.chat.pal().user.BG, tauui.ColorRGB(0x12, 0x34, 0x56); got != want {
		t.Fatalf("user message background = %#v, want the theme file's %#v", got, want)
	}
}

// A theme name with no file behind it keeps the built-in tokens rather than
// blanking the palette.
func TestUnknownThemeNameKeepsBuiltInTokens(t *testing.T) {
	a := newTestApp(t)
	a.themeColors = func(string) (map[string]string, bool) { return nil, false }

	before := a.chat.pal().user.BG
	a.applyTheme("no-such-theme")

	if got := a.chat.pal().user.BG; got != before {
		t.Fatalf("unknown theme changed the palette: %#v -> %#v", before, got)
	}
}

// Themes must reach plugin widgets too, not just the chat log — the registry
// previously received the name with an empty values map.
func TestThemeColorsReachTheExtensionsRegistry(t *testing.T) {
	a := newTestApp(t)
	a.extensions = extensions.New()
	a.themeColors = func(string) (map[string]string, bool) {
		return map[string]string{"accent": "#abcdef"}, true
	}

	a.applyTheme("midnight")

	got := a.extensions.Theme()
	if got.Name != "midnight" {
		t.Errorf("extensions theme name = %q, want midnight", got.Name)
	}
	if got.Values["accent"] != "#abcdef" {
		t.Errorf("extensions theme values = %#v, want the accent forwarded", got.Values)
	}
}

// A nil lookup is the ordinary case for hosts without a resource loader; it
// must degrade to the built-in themes rather than panicking.
func TestNilThemeLookupIsSafe(t *testing.T) {
	a := newTestApp(t)
	a.themeColors = nil

	a.applyTheme("tau")

	if got := a.chat.ThemeTokens().Border; got != tauThemeTokens().Border {
		t.Fatalf("built-in tau theme did not apply with a nil lookup: %#v", got)
	}
}

// Custom messages must not borrow the tool-success background: a compaction
// summary is not a successful tool call.
func TestCustomMessageBoxUsesItsOwnToken(t *testing.T) {
	tokens := defaultThemeTokens()
	p := tokens.chatPalette()

	if p.custom.BG == p.toolResult.BG {
		t.Fatal("custom message background is still the tool-result background")
	}
	if p.custom.BG != tokens.CustomMessageBg || p.custom.FG != tokens.CustomMessageText {
		t.Fatalf("custom style = %#v, want the customMessage tokens", p.custom)
	}
}

// The rendered rows, not just the palette, must pick up the custom background.
func TestCustomComponentRendersOnTheCustomBackground(t *testing.T) {
	conv := NewConversationModel()
	conv.AddMessage(Role("compaction_summary"), "compacted 12000 tokens")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 20})
	waitForChatLog(t, log)

	want := defaultThemeTokens().CustomMessageBg
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, line := range log.lines {
		if strings.Contains(line.text, "compacted") {
			if line.st.BG != want {
				t.Fatalf("custom row BG = %#v, want %#v", line.st.BG, want)
			}
			return
		}
	}
	t.Fatal("custom message did not render")
}

func TestParseThemeColorAcceptsSpecFormats(t *testing.T) {
	cases := []struct {
		raw  string
		want tauui.Color
	}{
		{"#ff8800", tauui.ColorRGB(255, 136, 0)},
		{"#F80", tauui.ColorRGB(255, 136, 0)},
		{"rgb(255, 136, 0)", tauui.ColorRGB(255, 136, 0)},
		{"  #FF8800  ", tauui.ColorRGB(255, 136, 0)},
		{"12", tauui.ColorIndex(12)},
		{"default", tauui.ColorDefault},
	}
	for _, tc := range cases {
		got, ok := parseThemeColor(tc.raw)
		if !ok {
			t.Fatalf("parseThemeColor(%q) reported failure", tc.raw)
		}
		if got != tc.want {
			t.Fatalf("parseThemeColor(%q) = %#v, want %#v", tc.raw, got, tc.want)
		}
	}
}

func TestParseThemeColorRejectsMalformedValues(t *testing.T) {
	for _, raw := range []string{"", "#ff88", "#gggggg", "rgb(1,2)", "rgb(1,2,300)", "256", "chartreuse"} {
		if _, ok := parseThemeColor(raw); ok {
			t.Fatalf("parseThemeColor(%q) accepted a malformed value", raw)
		}
	}
}

// A malformed value must cost exactly one token: theme values arrive from user
// settings and plugins, and one typo should not strand the UI on a half-applied
// palette.
func TestThemeTokensWithOverridesSkipsUnknownAndMalformed(t *testing.T) {
	base := defaultThemeTokens()
	got := base.withOverrides(map[string]string{
		"userMessageBg": "#102030",
		"toolSuccessBg": "not-a-color",
		"nonsenseToken": "#ffffff",
	})

	if got.UserMessageBg != tauui.ColorRGB(16, 32, 48) {
		t.Fatalf("UserMessageBg = %#v, want the override applied", got.UserMessageBg)
	}
	if got.ToolSuccessBg != base.ToolSuccessBg {
		t.Fatalf("ToolSuccessBg = %#v, want the base value kept when the override is malformed", got.ToolSuccessBg)
	}
	if got.Text != base.Text {
		t.Fatalf("Text = %#v, want untouched tokens preserved", got.Text)
	}
}

func TestThemeTokenNamesAcceptSpellingVariants(t *testing.T) {
	want := tauui.ColorRGB(1, 2, 3)
	for _, name := range []string{"userMessageBg", "user-message-bg", "USER_MESSAGE_BG", " usermessagebg "} {
		tokens := defaultThemeTokens()
		got := tokens.withOverrides(map[string]string{name: "rgb(1,2,3)"})
		if got.UserMessageBg != want {
			t.Fatalf("override under %q did not apply: got %#v", name, got.UserMessageBg)
		}
	}
}

// Every token app.md §11 marks required must be addressable. A missing one is
// not a cosmetic gap: withOverrides silently skips names it does not know, so
// the whole failure is invisible — a conformant theme file loads and nothing
// changes colour.
func TestTokenTargetsCoverSpecNames(t *testing.T) {
	tokens := defaultThemeTokens()
	targets := tokens.tokenTargets()

	var missing []string
	for _, name := range specThemeTokenNames {
		if _, ok := targets[normalizeTokenName(name)]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d/%d spec tokens are not addressable: %v",
			len(missing), len(specThemeTokenNames), missing)
	}
}

// The spec fixes the required set at 51. Guarding the count catches a token
// dropped from specThemeTokenNames, which would make the test above pass
// vacuously.
func TestSpecTokenListMatchesTheSpecCount(t *testing.T) {
	const specRequiredTokens = 51
	if len(specThemeTokenNames) != specRequiredTokens {
		t.Fatalf("specThemeTokenNames has %d entries, want the spec's %d",
			len(specThemeTokenNames), specRequiredTokens)
	}
	seen := map[string]bool{}
	for _, name := range specThemeTokenNames {
		key := normalizeTokenName(name)
		if seen[key] {
			t.Errorf("token %q listed twice", name)
		}
		seen[key] = true
	}
}

// A theme file written against the spec must actually repaint the UI. This is
// the end-to-end version of the coverage test: set every required token to a
// distinct colour and confirm none is left on its default.
func TestSpecConformantThemeAppliesEveryToken(t *testing.T) {
	values := map[string]string{}
	for i, name := range specThemeTokenNames {
		// Distinct, valid, and different from any default.
		values[name] = fmt.Sprintf("#%02x%02x%02x", 1, i+1, 2)
	}

	base := defaultThemeTokens()
	applied := base.withOverrides(values)

	targets := applied.tokenTargets()
	baseTargets := base.tokenTargets()
	for _, name := range specThemeTokenNames {
		key := normalizeTokenName(name)
		got, ok := targets[key]
		if !ok {
			t.Fatalf("token %q not addressable", name)
		}
		if *got == *baseTargets[key] {
			t.Errorf("token %q kept its default after the theme set it", name)
		}
	}
}

func TestChatPaletteDerivesEveryRoleFromTokens(t *testing.T) {
	tokens := defaultThemeTokens()
	p := tokens.chatPalette()

	checks := []struct {
		name string
		got  tauui.Style
		want tauui.Style
	}{
		{"user", p.user, tauui.Style{FG: tokens.UserMessageText, BG: tokens.UserMessageBg}},
		{"toolCall", p.toolCall, tauui.Style{FG: tokens.ToolTitle, BG: tokens.ToolPendingBg, Attr: tauui.AttrBold}},
		{"toolResult", p.toolResult, tauui.Style{FG: tokens.ToolOutput, BG: tokens.ToolSuccessBg}},
		{"toolError", p.toolError, tauui.Style{FG: tokens.Error, BG: tokens.ToolErrorBg}},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Fatalf("palette.%s = %#v, want %#v", c.name, c.got, c.want)
		}
	}
	if p.bashMode != tokens.BashMode {
		t.Fatalf("palette.bashMode = %#v, want the bashMode token", p.bashMode)
	}
}

func TestEditorBorderColorTracksThinkingLevel(t *testing.T) {
	tokens := defaultThemeTokens()
	cases := map[string]tauui.Color{
		"":        tokens.Border,
		"off":     tokens.Border,
		"minimal": tokens.ThinkingMinimal,
		"low":     tokens.ThinkingLow,
		"medium":  tokens.ThinkingMedium,
		"high":    tokens.ThinkingHigh,
		"xhigh":   tokens.ThinkingXHigh,
		"HIGH":    tokens.ThinkingHigh,
	}
	for level, want := range cases {
		if got := tokens.editorBorderColor(level); got != want {
			t.Fatalf("editorBorderColor(%q) = %#v, want %#v", level, got, want)
		}
	}
}

// Distinct thinking levels must be distinguishable, otherwise the editor rule
// carries no information.
func TestThinkingLevelColorsAreDistinct(t *testing.T) {
	tokens := defaultThemeTokens()
	seen := map[tauui.Color]string{}
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh"} {
		color := tokens.editorBorderColor(level)
		if prev, ok := seen[color]; ok {
			t.Fatalf("thinking levels %q and %q resolve to the same colour %#v", prev, level, color)
		}
		seen[color] = level
	}
}

func TestSetThemeAppliesTokenOverridesAndRerenders(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hello")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 20})
	waitForChatLog(t, log)

	log.SetTheme("default", map[string]string{"userMessageBg": "#123456"})
	waitForChatLog(t, log)

	if got, want := log.pal().user.BG, tauui.ColorRGB(0x12, 0x34, 0x56); got != want {
		t.Fatalf("user BG = %#v, want the overridden token %#v", got, want)
	}
	if got := log.ThemeTokens().UserMessageBg; got != tauui.ColorRGB(0x12, 0x34, 0x56) {
		t.Fatalf("ThemeTokens().UserMessageBg = %#v, want the override", got)
	}

	log.mu.Lock()
	defer log.mu.Unlock()
	for _, line := range log.lines {
		if strings.Contains(line.text, "hello") {
			if line.st.BG != tauui.ColorRGB(0x12, 0x34, 0x56) {
				t.Fatalf("rendered user row BG = %#v, want the theme change to reach painted rows", line.st.BG)
			}
			return
		}
	}
	t.Fatalf("missing user row: %#v", renderLineTexts(log.lines))
}

// A theme switch must drop the render cache: entries are keyed by revision and
// width, neither of which changes when only the colours do.
func TestSetThemeInvalidatesRenderCache(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hello")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 20})
	waitForChatLog(t, log)

	log.SetTheme("tau", nil)

	log.mu.Lock()
	cached := len(log.renderCache)
	log.mu.Unlock()
	if cached != 0 {
		t.Fatalf("render cache holds %d entries after a theme change, want it dropped", cached)
	}
}
