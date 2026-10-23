package tui

import (
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	tauui "github.com/losinggeneration/tau/tui"
)

// themeTokens is the app-level colour vocabulary named by the visual spec.
// Views resolve every colour through a token instead of a literal so switching
// themes is one swap and no surface is left behind on the previous palette.
//
// Component attributes stay with the component; faint foreground metadata is
// retained alongside colors so attribute-only changes invalidate render caches.
// The field names below match the spec's token names (app.md §11) so a theme
// file written against the spec applies. Two fields are tau extensions, marked
// as such; everything else is one of the 51 required tokens.
type themeTokens struct {
	// A stable token bitset keeps attribute-only changes in the cache identity.
	dimTokens uint64

	// Surface is a tau extension. The spec assumes the terminal's own
	// background shows through; tau paints an explicit background on every
	// chat row so provenance cues span the full width, which needs a colour
	// to paint. Themes that do not set it inherit the default.
	Surface tauui.Color

	Accent       tauui.Color
	Border       tauui.Color
	BorderAccent tauui.Color
	BorderMuted  tauui.Color
	Success      tauui.Color
	Error        tauui.Color
	Warning      tauui.Color
	Muted        tauui.Color
	Dim          tauui.Color
	Text         tauui.Color

	ThinkingText tauui.Color
	SelectedBg   tauui.Color

	UserMessageBg   tauui.Color
	UserMessageText tauui.Color

	CustomMessageBg    tauui.Color
	CustomMessageText  tauui.Color
	CustomMessageLabel tauui.Color

	ToolPendingBg tauui.Color
	ToolSuccessBg tauui.Color
	ToolErrorBg   tauui.Color
	ToolTitle     tauui.Color
	ToolOutput    tauui.Color

	MdHeading         tauui.Color
	MdLink            tauui.Color
	MdLinkURL         tauui.Color
	MdCode            tauui.Color
	MdCodeBlock       tauui.Color
	MdCodeBlockBorder tauui.Color
	MdQuote           tauui.Color
	MdQuoteBorder     tauui.Color
	MdHr              tauui.Color
	MdListBullet      tauui.Color

	ToolDiffAdded   tauui.Color
	ToolDiffRemoved tauui.Color
	ToolDiffContext tauui.Color

	SyntaxComment     tauui.Color
	SyntaxKeyword     tauui.Color
	SyntaxFunction    tauui.Color
	SyntaxVariable    tauui.Color
	SyntaxString      tauui.Color
	SyntaxNumber      tauui.Color
	SyntaxType        tauui.Color
	SyntaxOperator    tauui.Color
	SyntaxPunctuation tauui.Color

	ThinkingOff     tauui.Color
	ThinkingMinimal tauui.Color
	ThinkingLow     tauui.Color
	ThinkingMedium  tauui.Color
	ThinkingHigh    tauui.Color
	ThinkingXHigh   tauui.Color
	ThinkingMax     tauui.Color

	// BashMode tints shell execution chrome: the bash tool's border title and
	// its command line. Pi also uses it for the editor border while the prompt
	// is in shell-input mode; tau has no such mode yet, so the editor resolves
	// its border from the thinking level alone (see editorBorderColor).
	BashMode tauui.Color

	// SystemText is a tau extension for system/notification rows. The spec
	// renders those with `muted`; tau tints them so a notice is
	// distinguishable from dimmed chrome at a glance.
	SystemText tauui.Color

	// CompactionFallbackBg and CompactionFallbackLabel are tau extensions for
	// a compaction that degraded to a local summary after the model call
	// failed: the custom-message hues pushed toward red, so the degraded
	// provenance is visible without reading the body. Text stays
	// CustomMessageText.
	CompactionFallbackBg    tauui.Color
	CompactionFallbackLabel tauui.Color
}

var nativeTokenNames = func() []string {
	tokens := defaultThemeTokens()
	return slices.Sorted(maps.Keys(tokens.tokenTargets()))
}()

func (t *themeTokens) withDim(names []string) themeTokens {
	out := *t
	out.dimTokens = 0
	for _, name := range names {
		name = normalizeTokenName(name)
		if name == "surface" || strings.HasSuffix(name, "bg") {
			continue
		}
		if index, found := slices.BinarySearch(nativeTokenNames, name); found {
			out.dimTokens |= 1 << index
		}
	}
	return out
}

func (t *themeTokens) foreground(name string, color tauui.Color, base tauui.Style) tauui.Style {
	base.FG = color
	base.Attr &^= tauui.AttrDim
	if index, found := slices.BinarySearch(nativeTokenNames, normalizeTokenName(name)); found && t.dimTokens&(1<<index) != 0 {
		base.Attr |= tauui.AttrDim
	}
	return base
}

// defaultThemeTokens is the dark token set every unnamed/unknown theme
// resolves to. The values match the literals the chat log carried before the
// tokens existed, so the default look is unchanged by the indirection.
func defaultThemeTokens() themeTokens {
	return themeTokens{
		Surface: tauui.ColorRGB(30, 30, 30),

		Accent:       tauui.ColorRGB(110, 170, 240),
		Border:       tauui.ColorRGB(80, 80, 80),
		BorderAccent: tauui.ColorRGB(110, 170, 240),
		BorderMuted:  tauui.ColorRGB(55, 55, 55),
		Success:      tauui.ColorRGB(120, 210, 140),
		Error:        tauui.ColorRGB(255, 150, 150),
		Warning:      tauui.ColorRGB(225, 190, 110),
		Muted:        tauui.ColorRGB(150, 150, 150),
		Dim:          tauui.ColorRGB(120, 120, 120),
		Text:         tauui.ColorRGB(220, 220, 220),

		ThinkingText: tauui.ColorRGB(170, 170, 170),
		SelectedBg:   tauui.ColorRGB(45, 60, 75),

		UserMessageBg:   tauui.ColorRGB(30, 68, 84),
		UserMessageText: tauui.ColorRGB(235, 250, 255),

		CustomMessageBg:    tauui.ColorRGB(45, 40, 60),
		CustomMessageText:  tauui.ColorRGB(220, 215, 235),
		CustomMessageLabel: tauui.ColorRGB(180, 165, 220),

		ToolPendingBg: tauui.ColorRGB(28, 72, 42),
		ToolSuccessBg: tauui.ColorRGB(28, 72, 42),
		ToolErrorBg:   tauui.ColorRGB(72, 28, 28),
		ToolTitle:     tauui.ColorRGB(245, 245, 245),
		ToolOutput:    tauui.ColorRGB(215, 215, 215),

		MdHeading:         tauui.ColorRGB(130, 190, 250),
		MdLink:            tauui.ColorRGB(110, 190, 220),
		MdLinkURL:         tauui.ColorRGB(120, 140, 170),
		MdCode:            tauui.ColorRGB(230, 200, 140),
		MdCodeBlock:       tauui.ColorRGB(215, 215, 215),
		MdCodeBlockBorder: tauui.ColorRGB(80, 80, 80),
		MdQuote:           tauui.ColorRGB(170, 170, 170),
		MdQuoteBorder:     tauui.ColorRGB(100, 100, 100),
		MdHr:              tauui.ColorRGB(90, 90, 90),
		MdListBullet:      tauui.ColorRGB(150, 170, 200),

		ToolDiffAdded:   tauui.ColorRGB(170, 255, 170),
		ToolDiffRemoved: tauui.ColorRGB(255, 170, 170),
		ToolDiffContext: tauui.ColorRGB(160, 160, 160),

		SyntaxComment:     tauui.ColorRGB(120, 140, 120),
		SyntaxKeyword:     tauui.ColorRGB(200, 140, 220),
		SyntaxFunction:    tauui.ColorRGB(130, 190, 250),
		SyntaxVariable:    tauui.ColorRGB(220, 220, 220),
		SyntaxString:      tauui.ColorRGB(170, 210, 140),
		SyntaxNumber:      tauui.ColorRGB(230, 180, 130),
		SyntaxType:        tauui.ColorRGB(230, 210, 140),
		SyntaxOperator:    tauui.ColorRGB(190, 190, 190),
		SyntaxPunctuation: tauui.ColorRGB(170, 170, 170),

		ThinkingOff:     tauui.ColorRGB(80, 80, 80),
		ThinkingMinimal: tauui.ColorRGB(90, 110, 130),
		ThinkingLow:     tauui.ColorRGB(90, 140, 190),
		ThinkingMedium:  tauui.ColorRGB(130, 130, 210),
		ThinkingHigh:    tauui.ColorRGB(180, 120, 210),
		ThinkingXHigh:   tauui.ColorRGB(215, 110, 175),
		ThinkingMax:     tauui.ColorRGB(240, 80, 140),

		BashMode:   tauui.ColorRGB(220, 180, 80),
		SystemText: tauui.ColorRGB(190, 170, 220),

		CompactionFallbackBg:    tauui.ColorRGB(66, 34, 56),
		CompactionFallbackLabel: tauui.ColorRGB(228, 130, 185),
	}
}

// tauThemeTokens is the branded token set selected by `theme: tau`. It shares
// the default hues but warms the chrome the tau look draws borders with, so the
// bordered tool components read as deliberate rather than as the dark theme
// with boxes bolted on.
func tauThemeTokens() themeTokens {
	t := defaultThemeTokens()
	t.Border = tauui.ColorRGB(110, 95, 70)
	t.ToolPendingBg = tauui.ColorRGB(34, 60, 46)
	t.ToolSuccessBg = tauui.ColorRGB(26, 66, 40)
	t.ToolTitle = tauui.ColorRGB(250, 240, 220)
	return t
}

// themeTokensFor resolves a theme name to its token set. Unknown names fall
// back to the default tokens rather than erroring: theme names arrive from
// user settings and from plugins, and an unrecognised one should degrade to a
// readable UI, not a broken one.
func themeTokensFor(name string) themeTokens {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "tau":
		return tauThemeTokens()
	default:
		return defaultThemeTokens()
	}
}

// specThemeTokenNames is the full set of colour tokens app.md §11 requires a
// theme file to define. It is listed separately from tokenTargets so a test can
// assert the two agree: tokenTargets is what actually applies, this is what the
// spec says must apply, and a token in one but not the other is the bug that
// makes a conformant theme file silently do nothing.
var specThemeTokenNames = []string{
	"accent", "border", "borderAccent", "borderMuted",
	"success", "error", "warning", "muted", "dim", "text",
	"thinkingText", "selectedBg",
	"userMessageBg", "userMessageText",
	"customMessageBg", "customMessageText", "customMessageLabel",
	"toolPendingBg", "toolSuccessBg", "toolErrorBg", "toolTitle", "toolOutput",
	"mdHeading", "mdLink", "mdLinkUrl", "mdCode", "mdCodeBlock",
	"mdCodeBlockBorder", "mdQuote", "mdQuoteBorder", "mdHr", "mdListBullet",
	"toolDiffAdded", "toolDiffRemoved", "toolDiffContext",
	"syntaxComment", "syntaxKeyword", "syntaxFunction", "syntaxVariable",
	"syntaxString", "syntaxNumber", "syntaxType", "syntaxOperator",
	"syntaxPunctuation",
	"thinkingOff", "thinkingMinimal", "thinkingLow", "thinkingMedium",
	"thinkingHigh", "thinkingXhigh",
	"bashMode",
}

// tokenTargets maps token names to the fields they set. Keys are normalised
// (see normalizeTokenName), so a theme file may spell a token `userMessageBg`,
// `user-message-bg`, or `USER_MESSAGE_BG` and hit the same field.
//
// The names are the spec's, not tau's internal ones. That distinction is the
// whole point: a theme authored against app.md has to apply here, and the
// previous vocabulary (`errorText`, `diffAddedText`, `textMuted`, …) meant such
// a file matched almost nothing and was silently discarded token by token.
func (t *themeTokens) tokenTargets() map[string]*tauui.Color {
	return map[string]*tauui.Color{
		"accent":       &t.Accent,
		"border":       &t.Border,
		"borderaccent": &t.BorderAccent,
		"bordermuted":  &t.BorderMuted,
		"success":      &t.Success,
		"error":        &t.Error,
		"warning":      &t.Warning,
		"muted":        &t.Muted,
		"dim":          &t.Dim,
		"text":         &t.Text,

		"thinkingtext": &t.ThinkingText,
		"selectedbg":   &t.SelectedBg,

		"usermessagebg":   &t.UserMessageBg,
		"usermessagetext": &t.UserMessageText,

		"custommessagebg":    &t.CustomMessageBg,
		"custommessagetext":  &t.CustomMessageText,
		"custommessagelabel": &t.CustomMessageLabel,

		"toolpendingbg": &t.ToolPendingBg,
		"toolsuccessbg": &t.ToolSuccessBg,
		"toolerrorbg":   &t.ToolErrorBg,
		"tooltitle":     &t.ToolTitle,
		"tooloutput":    &t.ToolOutput,

		"mdheading":         &t.MdHeading,
		"mdlink":            &t.MdLink,
		"mdlinkurl":         &t.MdLinkURL,
		"mdcode":            &t.MdCode,
		"mdcodeblock":       &t.MdCodeBlock,
		"mdcodeblockborder": &t.MdCodeBlockBorder,
		"mdquote":           &t.MdQuote,
		"mdquoteborder":     &t.MdQuoteBorder,
		"mdhr":              &t.MdHr,
		"mdlistbullet":      &t.MdListBullet,

		"tooldiffadded":   &t.ToolDiffAdded,
		"tooldiffremoved": &t.ToolDiffRemoved,
		"tooldiffcontext": &t.ToolDiffContext,

		"syntaxcomment":     &t.SyntaxComment,
		"syntaxkeyword":     &t.SyntaxKeyword,
		"syntaxfunction":    &t.SyntaxFunction,
		"syntaxvariable":    &t.SyntaxVariable,
		"syntaxstring":      &t.SyntaxString,
		"syntaxnumber":      &t.SyntaxNumber,
		"syntaxtype":        &t.SyntaxType,
		"syntaxoperator":    &t.SyntaxOperator,
		"syntaxpunctuation": &t.SyntaxPunctuation,

		"thinkingoff":     &t.ThinkingOff,
		"thinkingminimal": &t.ThinkingMinimal,
		"thinkinglow":     &t.ThinkingLow,
		"thinkingmedium":  &t.ThinkingMedium,
		"thinkinghigh":    &t.ThinkingHigh,
		"thinkingxhigh":   &t.ThinkingXHigh,
		"thinkingmax":     &t.ThinkingMax,

		"bashmode": &t.BashMode,

		// tau extensions, outside the spec's required set.
		"surface":                 &t.Surface,
		"systemtext":              &t.SystemText,
		"compactionfallbackbg":    &t.CompactionFallbackBg,
		"compactionfallbacklabel": &t.CompactionFallbackLabel,
	}
}

// withOverrides returns a copy of t with any recognised token names replaced by
// the parsed colour. Unknown names and unparseable values are skipped so a
// malformed plugin theme degrades one token at a time instead of failing the
// whole switch.
func (t *themeTokens) withOverrides(values map[string]string) themeTokens {
	if len(values) == 0 {
		return *t
	}
	out := *t
	targets := out.tokenTargets()
	for name, raw := range values {
		dst, ok := targets[normalizeTokenName(name)]
		if !ok {
			continue
		}
		if color, ok := parseThemeColor(raw); ok {
			*dst = color
		}
	}
	return out
}

// normalizeTokenName folds the spellings a settings file or plugin may use
// (userMessageBg, user-message-bg, user_message_bg) onto one key.
func normalizeTokenName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if r == '-' || r == '_' || r == '.' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

const themeNumber = `[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:e[+-]?\d+)?`

var (
	oklchPattern = regexp.MustCompile(`^oklch\(\s*(` + themeNumber + `)(%)?\s+(` + themeNumber + `)\s+(` + themeNumber + `)(?:deg)?\s*\)$`)
	okhslPattern = regexp.MustCompile(`^okhsl\(\s*(` + themeNumber + `)(?:deg)?\s+(` + themeNumber + `)(%)?\s+(` + themeNumber + `)(%)?\s*\)$`)
)

// parseThemeColor accepts "#rgb", "#rrggbb", "oklch(L C H)", "okhsl(H S L)",
// "rgb(r,g,b)", a 0-255 palette index, or "default" for the terminal's own
// colour.
func parseThemeColor(raw string) (tauui.Color, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return tauui.ColorDefault, false
	}
	if s == "default" {
		return tauui.ColorDefault, true
	}
	if m := oklchPattern.FindStringSubmatch(s); m != nil {
		l, c, h := themeFloat(m[1]), themeFloat(m[3]), themeFloat(m[4])
		if m[2] != "" {
			l /= 100
		}
		if !finite(l, c, h) || l < 0 || l > 1 || c < 0 {
			return tauui.ColorDefault, false
		}
		rgb := oklchToRGB(l, c, h)
		return tauui.ColorRGB(rgb[0], rgb[1], rgb[2]), true
	}
	if m := okhslPattern.FindStringSubmatch(s); m != nil {
		h, sat, l := themeFloat(m[1]), themeFloat(m[2]), themeFloat(m[4])
		if m[3] != "" {
			sat /= 100
		}
		if m[5] != "" {
			l /= 100
		}
		if !finite(h, sat, l) || sat < 0 || sat > 1 || l < 0 || l > 1 {
			return tauui.ColorDefault, false
		}
		rgb := okhslToRGB(h, sat, l)
		return tauui.ColorRGB(rgb[0], rgb[1], rgb[2]), true
	}
	if rest, ok := strings.CutPrefix(s, "#"); ok {
		return parseHexColor(rest)
	}
	if rest, ok := strings.CutPrefix(s, "rgb("); ok {
		rest, ok = strings.CutSuffix(rest, ")")
		if !ok {
			return tauui.ColorDefault, false
		}
		parts := strings.Split(rest, ",")
		if len(parts) != 3 {
			return tauui.ColorDefault, false
		}
		var rgb [3]uint8
		for i, part := range parts {
			v, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || v < 0 || v > 255 {
				return tauui.ColorDefault, false
			}
			rgb[i] = uint8(v)
		}
		return tauui.ColorRGB(rgb[0], rgb[1], rgb[2]), true
	}
	if index, err := strconv.Atoi(s); err == nil && index >= 0 && index <= 255 {
		return tauui.ColorIndex(uint8(index)), true
	}
	return tauui.ColorDefault, false
}

// themeFloat parses a value the colour patterns already matched as a number.
func themeFloat(text string) float64 {
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return math.NaN()
	}
	return v
}

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func parseHexColor(hex string) (tauui.Color, bool) {
	switch len(hex) {
	case 3:
		var rgb [3]uint8
		for i := range 3 {
			v, err := strconv.ParseUint(hex[i:i+1], 16, 8)
			if err != nil {
				return tauui.ColorDefault, false
			}
			// #abc expands to #aabbcc.
			rgb[i] = uint8(v)*16 + uint8(v)
		}
		return tauui.ColorRGB(rgb[0], rgb[1], rgb[2]), true
	case 6:
		var rgb [3]uint8
		for i := range 3 {
			v, err := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
			if err != nil {
				return tauui.ColorDefault, false
			}
			rgb[i] = uint8(v)
		}
		return tauui.ColorRGB(rgb[0], rgb[1], rgb[2]), true
	default:
		return tauui.ColorDefault, false
	}
}

// chatPalette projects the tokens onto the per-role row styles the chat log
// paints with. Every style carries a background because Paint fills each row
// before drawing text, so the provenance cue spans the full terminal width.
func (t *themeTokens) chatPalette() chatPalette {
	return chatPalette{
		tokens:     *t,
		user:       t.foreground("UserMessageText", t.UserMessageText, tauui.Style{BG: t.UserMessageBg}),
		assistant:  t.foreground("Text", t.Text, tauui.Style{BG: t.Surface}),
		thinking:   t.foreground("ThinkingText", t.ThinkingText, tauui.Style{BG: t.Surface, Attr: tauui.AttrItalic}),
		toolCall:   t.foreground("ToolTitle", t.ToolTitle, tauui.Style{BG: t.ToolPendingBg, Attr: tauui.AttrBold}),
		toolResult: t.foreground("ToolOutput", t.ToolOutput, tauui.Style{BG: t.ToolSuccessBg}),
		toolError:  t.foreground("Error", t.Error, tauui.Style{BG: t.ToolErrorBg}),
		errorRole:  t.foreground("Error", t.Error, tauui.Style{BG: t.Surface}),
		system:     t.foreground("SystemText", t.SystemText, tauui.Style{BG: t.Surface}),
		dim:        t.foreground("Muted", t.Muted, tauui.Style{BG: t.Surface}),
		spacer:     t.foreground("Text", t.Text, tauui.Style{BG: t.Surface}),

		// Custom messages get their own background rather than borrowing the
		// tool-success one. A compaction summary is not a successful tool call,
		// and reusing that green said it was.
		custom:      t.foreground("CustomMessageText", t.CustomMessageText, tauui.Style{BG: t.CustomMessageBg}),
		customLabel: t.foreground("CustomMessageLabel", t.CustomMessageLabel, tauui.Style{BG: t.CustomMessageBg, Attr: tauui.AttrBold}),

		compactionFallback:      t.foreground("CustomMessageText", t.CustomMessageText, tauui.Style{BG: t.CompactionFallbackBg}),
		compactionFallbackLabel: t.foreground("CompactionFallbackLabel", t.CompactionFallbackLabel, tauui.Style{BG: t.CompactionFallbackBg, Attr: tauui.AttrBold}),

		bashMode:    t.BashMode,
		diffAdded:   t.ToolDiffAdded,
		diffRemoved: t.ToolDiffRemoved,
		diffContext: t.ToolDiffContext,

		// Resolved up front so the render worker never has to take a lock to
		// colour a border; it runs per bordered row.
		borderColors: [tauui.DynamicBorderStateCount]tauui.Color{
			tauui.DynamicBorderNeutral:   t.dynamicBorderColor(tauui.DynamicBorderNeutral),
			tauui.DynamicBorderRunning:   t.dynamicBorderColor(tauui.DynamicBorderRunning),
			tauui.DynamicBorderSuccess:   t.dynamicBorderColor(tauui.DynamicBorderSuccess),
			tauui.DynamicBorderError:     t.dynamicBorderColor(tauui.DynamicBorderError),
			tauui.DynamicBorderCancelled: t.dynamicBorderColor(tauui.DynamicBorderCancelled),
		},
	}
}

// dynamicBorderColor resolves the glyph colour for a bordered surface in a
// given state. This is the "colour function" the dynamic border is specified
// with: the component fixes the state vocabulary, the theme decides what each
// state looks like, so bash chrome and any future bordered section agree.
func (t *themeTokens) dynamicBorderColor(state tauui.DynamicBorderState) tauui.Color {
	switch state {
	case tauui.DynamicBorderRunning:
		return t.Accent
	case tauui.DynamicBorderSuccess:
		return t.Success
	case tauui.DynamicBorderError:
		return t.Error
	case tauui.DynamicBorderCancelled:
		return t.Warning
	default:
		return t.Border
	}
}

// editorBorderColor resolves the prompt editor's rule color from the session's
// thinking level. Unset and unrecognized levels use thinkingOff.
func (t *themeTokens) editorBorderColor(thinkingLevel string) tauui.Color {
	switch strings.ToLower(strings.TrimSpace(thinkingLevel)) {
	case "minimal":
		return t.ThinkingMinimal
	case "low":
		return t.ThinkingLow
	case "medium":
		return t.ThinkingMedium
	case "high":
		return t.ThinkingHigh
	case "xhigh":
		return t.ThinkingXHigh
	case "max":
		return t.ThinkingMax
	default:
		return t.ThinkingOff
	}
}

func (t *themeTokens) editorBorderStyle(level string) tauui.Style {
	name := "thinkingOff"
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "minimal", "low", "medium", "high", "xhigh", "max":
		name = "thinking" + strings.ToLower(strings.TrimSpace(level))
	}
	return t.foreground(name, t.editorBorderColor(level), tauui.Style{BG: t.Surface})
}

// nativeTheme keeps editor content, pickers, and clear cells on the same palette
// as the transcript without changing the application's Unicode chrome.
func (t *themeTokens) nativeTheme() tauui.Theme {
	theme := appTheme()
	base := t.foreground("text", t.Text, tauui.Style{BG: t.Surface})
	theme.Base = base
	theme.Palette.Surface = base
	theme.Palette.SurfaceMuted = base
	theme.Palette.Text = base
	theme.Palette.TextMuted = t.foreground("Muted", t.Muted, base)
	theme.Palette.Border = t.foreground("Border", t.Border, base)
	theme.Palette.BorderMuted = t.foreground("BorderMuted", t.BorderMuted, base)
	theme.Palette.Focus = t.foreground("Accent", t.Accent, base)
	theme.Palette.Selection = base.WithBG(t.SelectedBg)
	theme.Palette.Accent = t.foreground("Accent", t.Accent, base)
	theme.Palette.Placeholder = t.foreground("Muted", t.Muted, base)
	theme.Palette.Success = t.foreground("Success", t.Success, base)
	theme.Palette.Warning = t.foreground("Warning", t.Warning, base)
	theme.Palette.Danger = t.foreground("Error", t.Error, base)
	theme.Palette.Disabled = t.foreground("Dim", t.Dim, base)
	return theme
}

func (t *themeTokens) markdownTheme() tauui.MarkdownTheme {
	return tauui.MarkdownTheme{
		Heading:         t.foreground("mdHeading", t.MdHeading, tauui.Style{}),
		Link:            t.foreground("mdLink", t.MdLink, tauui.Style{}),
		LinkURL:         t.foreground("mdLinkUrl", t.MdLinkURL, tauui.Style{}),
		Code:            t.foreground("mdCode", t.MdCode, tauui.Style{}),
		CodeBlock:       t.foreground("mdCodeBlock", t.MdCodeBlock, tauui.Style{}),
		CodeBlockBorder: t.foreground("mdCodeBlockBorder", t.MdCodeBlockBorder, tauui.Style{}),
		Quote:           t.foreground("mdQuote", t.MdQuote, tauui.Style{}),
		QuoteBorder:     t.foreground("mdQuoteBorder", t.MdQuoteBorder, tauui.Style{}),
		HR:              t.foreground("mdHr", t.MdHr, tauui.Style{}),
		ListBullet:      t.foreground("mdListBullet", t.MdListBullet, tauui.Style{}),
	}
}
