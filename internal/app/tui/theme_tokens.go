package tui

import (
	"strconv"
	"strings"

	tauui "github.com/losinggeneration/tau/tui"
)

// themeTokens is the app-level colour vocabulary named by the visual spec.
// Views resolve every colour through a token instead of a literal so switching
// themes is one swap and no surface is left behind on the previous palette.
//
// Tokens are colours, not styles: attributes (bold, italic) describe what a
// component is doing with a row, not what the theme is, so they stay with the
// component. The rovel Palette underneath remains the toolkit-level vocabulary
// for generic widgets; these tokens are the tau-specific roles it has no name
// for (per-tool-state backgrounds, per-thinking-level accents).
// The field names below match the spec's token names (app.md §11) so a theme
// file written against the spec applies. Two fields are tau extensions, marked
// as such; everything else is one of the 51 required tokens.
type themeTokens struct {
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

	// BashMode tints shell execution chrome: the bash tool's border title and
	// its command line. Pi also uses it for the editor border while the prompt
	// is in shell-input mode; tau has no such mode yet, so the editor resolves
	// its border from the thinking level alone (see editorBorderColor).
	BashMode tauui.Color

	// SystemText is a tau extension for system/notification rows. The spec
	// renders those with `muted`; tau tints them so a notice is
	// distinguishable from dimmed chrome at a glance.
	SystemText tauui.Color
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

		BashMode:   tauui.ColorRGB(220, 180, 80),
		SystemText: tauui.ColorRGB(190, 170, 220),
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

		"bashmode": &t.BashMode,

		// tau extensions, outside the spec's required set.
		"surface":    &t.Surface,
		"systemtext": &t.SystemText,
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

// parseThemeColor accepts "#rgb", "#rrggbb", "rgb(r,g,b)", a 0-255 palette
// index, or "default" for the terminal's own colour.
func parseThemeColor(raw string) (tauui.Color, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return tauui.ColorDefault, false
	}
	if s == "default" {
		return tauui.ColorDefault, true
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
		user:       tauui.Style{FG: t.UserMessageText, BG: t.UserMessageBg},
		assistant:  tauui.Style{FG: t.Text, BG: t.Surface},
		thinking:   tauui.Style{FG: t.ThinkingText, BG: t.Surface, Attr: tauui.AttrItalic},
		toolCall:   tauui.Style{FG: t.ToolTitle, BG: t.ToolPendingBg, Attr: tauui.AttrBold},
		toolResult: tauui.Style{FG: t.ToolOutput, BG: t.ToolSuccessBg},
		toolError:  tauui.Style{FG: t.Error, BG: t.ToolErrorBg},
		errorRole:  tauui.Style{FG: t.Error, BG: t.Surface},
		system:     tauui.Style{FG: t.SystemText, BG: t.Surface},
		dim:        tauui.Style{FG: t.Muted, BG: t.Surface},
		spacer:     tauui.Style{FG: t.Text, BG: t.Surface},

		// Custom messages get their own background rather than borrowing the
		// tool-success one. A compaction summary is not a successful tool call,
		// and reusing that green said it was.
		custom:      tauui.Style{FG: t.CustomMessageText, BG: t.CustomMessageBg},
		customLabel: tauui.Style{FG: t.CustomMessageLabel, BG: t.CustomMessageBg, Attr: tauui.AttrBold},

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

// editorBorderColor resolves the prompt editor's rule colour from the session's
// thinking level. An unset or "off" level uses the neutral border token, so the
// coloured rule reads as "reasoning is on" at a glance.
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
	default:
		return t.Border
	}
}
