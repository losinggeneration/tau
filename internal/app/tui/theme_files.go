package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/losinggeneration/rovel/style"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/extensions"
)

// themeFile is the on-disk theme shape: { $schema?, name, appearance?, vars?,
// colors, export? }. Unknown top-level keys are rejected.
// Export colors are validated alongside terminal colors even though the TUI
// does not use them for rendering.
type themeFile struct {
	Schema     string                     `json:"$schema,omitempty"`
	Name       *string                    `json:"name"`
	Appearance string                     `json:"appearance,omitempty"`
	Vars       map[string]themeColorValue `json:"vars,omitempty"`
	Colors     map[string]themeColorValue `json:"colors"`
	Export     map[string]themeColorValue `json:"export,omitempty"`
}

// optionalThemeTokens maps tokens a theme file may omit to the token whose
// value they take. "" means the terminal default. The last four are tau
// extensions: an omitted row surface lets the terminal background show
// through.
var optionalThemeTokens = map[string]string{
	"scrollbarTrack":          "muted",
	"scrollbarThumb":          "text",
	"searchMatchBg":           "selectedBg",
	"searchMatchText":         "text",
	"thinkingMax":             "thinkingXhigh",
	"surface":                 "",
	"systemText":              "muted",
	"compactionFallbackBg":    "toolErrorBg",
	"compactionFallbackLabel": "error",
}

var themeExportTokens = map[string]bool{"pageBg": true, "cardBg": true, "infoBg": true}

// DecodeThemeColors validates a theme file against the theme schema and
// returns every colour token, with `vars` references and token fallbacks
// resolved.
func DecodeThemeColors(raw []byte) (map[string]string, error) {
	theme, err := DecodeTheme(raw)
	return theme.Values, err
}

// ThemeDefinition holds resolved file tokens and its declared appearance.
// Empty Appearance lets the theme's fixed colors or terminal decide.
type ThemeDefinition struct {
	// Dim names foreground tokens rendered with the terminal faint attribute.
	Dim        []string
	Values     map[string]string
	Appearance string
}

// ThemeLookup resolves discovered themes without losing file metadata.
type ThemeLookup func(name string) (ThemeDefinition, bool)

// DecodeTheme validates tokens and metadata through the same boundary used by
// DecodeThemeColors, retaining appearance for application and inspection.
func DecodeTheme(raw []byte) (ThemeDefinition, error) {
	values, appearance, err := decodeTheme(raw)
	return ThemeDefinition{Values: values, Appearance: appearance}, err
}

func decodeTheme(raw []byte) (map[string]string, string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, "", fmt.Errorf("theme: parse: %w", err)
	}
	for key, value := range fields {
		switch key {
		case "$schema", "name", "appearance", "vars", "colors", "export":
		default:
			return nil, "", fmt.Errorf("theme: unknown property %q", key)
		}
		if string(value) == "null" {
			return nil, "", fmt.Errorf("theme: %s must not be null", key)
		}
	}
	var file themeFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, "", fmt.Errorf("theme: parse: %w", err)
	}
	if file.Name == nil || *file.Name == "" {
		return nil, "", errors.New("theme: missing name")
	}
	name := *file.Name
	if strings.ContainsAny(name, "/\\") {
		return nil, "", fmt.Errorf("invalid theme name %q: theme names cannot contain path separators; \"/\" is reserved for automatic light/dark theme settings", name)
	}
	if _, declared := fields["appearance"]; declared && file.Appearance != "dark" && file.Appearance != "light" {
		return nil, "", fmt.Errorf("theme %q: invalid appearance %q", name, file.Appearance)
	}
	if file.Colors == nil {
		return nil, "", fmt.Errorf("theme %q: no colors object", name)
	}
	var missing, unknown []string
	for _, token := range specThemeTokenNames {
		if _, ok := file.Colors[token]; !ok {
			missing = append(missing, token)
		}
	}
	for token := range file.Colors {
		if _, optional := optionalThemeTokens[token]; !optional && !slices.Contains(specThemeTokenNames, token) {
			unknown = append(unknown, token)
		}
	}
	for token := range file.Export {
		if !themeExportTokens[token] {
			unknown = append(unknown, "export."+token)
		}
	}
	if len(missing) > 0 || len(unknown) > 0 {
		slices.Sort(missing)
		slices.Sort(unknown)
		var problems []string
		if len(missing) > 0 {
			problems = append(problems, "missing required color tokens: "+strings.Join(missing, ", "))
		}
		if len(unknown) > 0 {
			problems = append(problems, "unknown color tokens: "+strings.Join(unknown, ", "))
		}
		return nil, "", fmt.Errorf("invalid theme %q: %s", name, strings.Join(problems, "; "))
	}

	resolve := func(token string, color themeColorValue) (string, error) {
		value := string(color)
		seen := make(map[string]bool)
		for !isLiteralThemeColor(value) {
			next, exists := file.Vars[value]
			if !exists {
				if _, valid := parseThemeColor(value); valid {
					return value, nil
				}
				return "", fmt.Errorf("theme %q: variable reference not found for %s: %q", name, token, value)
			}
			if seen[value] {
				return "", fmt.Errorf("theme %q: circular variable reference %q", name, value)
			}
			seen[value] = true
			value = string(next)
		}
		if _, valid := parseThemeColor(value); !valid {
			return "", fmt.Errorf("theme %q: invalid color %q for %s", name, value, token)
		}
		return value, nil
	}
	for _, declarations := range []map[string]themeColorValue{file.Vars, file.Export} {
		for token, color := range declarations {
			if _, err := resolve(token, color); err != nil {
				return nil, "", err
			}
		}
	}
	out := make(map[string]string, len(file.Colors)+len(optionalThemeTokens))
	for token, color := range file.Colors {
		value, err := resolve(token, color)
		if err != nil {
			return nil, "", err
		}
		out[token] = value
	}
	for token, fallback := range optionalThemeTokens {
		if _, ok := out[token]; ok {
			continue
		}
		if fallback == "" {
			out[token] = "default"
			continue
		}
		out[token] = out[fallback]
	}
	return out, file.Appearance, nil
}

// isLiteralThemeColor reports whether a value is a colour rather than a
// variable reference: palette indices, the terminal default, hex,
// and OKLCH/OKHSL values.
func isLiteralThemeColor(value string) bool {
	lower := strings.ToLower(value)
	if value == "default" || strings.HasPrefix(value, "#") || strings.HasPrefix(lower, "oklch(") || strings.HasPrefix(lower, "okhsl(") {
		return true
	}
	_, err := strconv.Atoi(value)
	return err == nil
}

// resolveThemeSetting picks a theme name from a setting: "light/dark" names one theme for each terminal
// appearance, and any other value containing "/" selects the system theme.
func resolveThemeSetting(setting string, appearance ...string) string {
	light, dark, auto := strings.Cut(setting, "/")
	if !auto {
		return setting
	}
	light, dark = strings.TrimSpace(light), strings.TrimSpace(dark)
	if light == "" || dark == "" || strings.Contains(dark, "/") {
		return systemThemeName
	}
	hint := colorFgBgAppearance(os.Getenv("COLORFGBG"))
	if len(appearance) > 0 {
		hint = appearance[0]
	}
	if hint == "light" {
		return light
	}
	return dark
}

// colorFgBgAppearance classifies the background index in COLORFGBG ("fg;bg"
// or rxvt's "fg;xpm;bg") the way Vim does: 0-6 and 8 are dark, 7 and
// 9-15 are light. Anything else is unknown, which callers treat as dark.
func colorFgBgAppearance(value string) string {
	fields := strings.Split(value, ";")
	bg := strings.TrimSpace(fields[len(fields)-1])
	index, err := strconv.Atoi(bg)
	if err != nil || len(bg) > 2 || index < 0 || index > 15 {
		return ""
	}
	if index <= 6 || index == 8 {
		return "dark"
	}
	return "light"
}

// ThemeColorLookup resolves a theme name to its colour tokens. It is how
// discovered theme files reach the UI: the application layer owns discovery and
// precedence (see agent/artifacts), and hands the terminal UI a lookup rather
// than the registry, so this package stays independent of the resource loader.
//
// Returning false means "no such theme". The application uses the generated
// system theme unless the name identifies a built-in theme.
type ThemeColorLookup func(name string) (map[string]string, bool)

// themeDefinitionFor reserves generated system colors before resource lookup.
func (a *App) themeDefinitionFor(name string) (ThemeDefinition, bool) {
	if a == nil {
		return ThemeDefinition{}, false
	}
	if name == systemThemeName {
		saturation := 1.0
		if !a.terminalTheme.ready {
			saturation = 0
		}
		result := generateSystemTheme(a.terminalTheme.colors, saturation, a.terminalThemeAppearance())
		result.colors["surface"] = "default"
		result.colors["systemText"] = result.colors["muted"]
		if slices.Contains(result.dim, "muted") {
			result.dim = append(result.dim, "systemText")
		}
		result.colors["compactionFallbackBg"] = result.colors["toolErrorBg"]
		result.colors["compactionFallbackLabel"] = result.colors["error"]
		return ThemeDefinition{Values: result.colors, Appearance: result.appearance, Dim: result.dim}, true
	}
	if a.themeLookup != nil {
		if definition, ok := a.themeLookup(name); ok {
			return definition, true
		}
	}
	if a.themeColors != nil {
		if values, ok := a.themeColors(name); ok {
			return ThemeDefinition{Values: values}, true
		}
	}
	switch name {
	case "default", "tau", "dark", "light":
		return ThemeDefinition{}, true
	default:
		return ThemeDefinition{}, false
	}
}

func themeBackgroundToken(name string) bool {
	return strings.HasSuffix(name, "Bg") || name == "surface"
}

func (definition ThemeDefinition) appearance(terminal string) string {
	if definition.Appearance == "light" || definition.Appearance == "dark" {
		return definition.Appearance
	}
	var fg, bg float64
	var foregrounds, backgrounds int
	for token, raw := range definition.Values {
		// Tau-only extensions do not participate in upstream appearance inference.
		if !slices.Contains(specThemeTokenNames, token) && token != "thinkingMax" && token != "searchMatchBg" && token != "searchMatchText" && token != "scrollbarTrack" && token != "scrollbarThumb" {
			continue
		}
		color, ok := parseThemeColor(raw)
		if !ok || color.IsDefault() {
			continue
		}
		if index, indexed := color.Index(); indexed && index < 16 {
			continue
		}
		rgb := color.DisplayRGBA(style.RGBA{})
		lightness := rgbToOklab(tauui.RGBColor{R: rgb.R, G: rgb.G, B: rgb.B})[0]
		if m := oklchPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(raw))); m != nil {
			lightness = themeFloat(m[1])
			if m[2] != "" {
				lightness /= 100
			}
		}
		if themeBackgroundToken(token) {
			bg += lightness
			backgrounds++
		} else {
			fg += lightness
			foregrounds++
		}
	}
	if foregrounds > 0 {
		fg /= float64(foregrounds)
	}
	if backgrounds > 0 {
		bg /= float64(backgrounds)
	}
	switch {
	case foregrounds > 0 && backgrounds > 0:
		if bg < fg {
			return "dark"
		}
		return "light"
	case backgrounds > 0:
		if bg < 0.5 {
			return "dark"
		}
		return "light"
	case foregrounds > 0:
		if fg > 0.5 {
			return "dark"
		}
		return "light"
	default:
		return terminal
	}
}

func (definition ThemeDefinition) resolved(name string, terminal tauui.TerminalColors, hint string) extensions.Theme {
	appearance := definition.appearance(hint)
	fg, bg := "#e5e5e7", "#000000"
	if appearance == "light" {
		fg, bg = "#000000", "#ffffff"
	}
	if terminal.Foreground != nil {
		fg = systemHex(*terminal.Foreground)
	}
	if terminal.Background != nil {
		bg = systemHex(*terminal.Background)
	}
	colors := maps.Clone(definition.Values)
	for token, raw := range colors {
		color, ok := parseThemeColor(raw)
		if !ok || !color.IsDefault() {
			continue
		}
		if themeBackgroundToken(token) {
			colors[token] = bg
		} else {
			colors[token] = fg
		}
	}
	background, _ := parseThemeColor(bg)
	for _, token := range definition.Dim {
		if themeBackgroundToken(token) {
			continue
		}
		raw, exists := colors[token]
		if !exists {
			continue
		}
		color, ok := parseThemeColor(raw)
		if !ok {
			continue
		}
		colors[token] = faintInspectionColor(color, background)
	}
	return extensions.Theme{Name: name, Values: definition.Values, Colors: colors, Appearance: appearance, Dim: definition.Dim}
}

// faintInspectionColor mirrors the upstream 40% OKLCH mix toward terminal background.
func faintInspectionColor(fg, bg tauui.Color) string {
	toLCH := func(color tauui.Color) vec3 {
		rgb := color.DisplayRGBA(style.RGBA{})
		lab := rgbToOklab(tauui.RGBColor{R: rgb.R, G: rgb.G, B: rgb.B})
		return vec3{lab[0], math.Hypot(lab[1], lab[2]), math.Mod(math.Atan2(lab[2], lab[1])*180/math.Pi+360, 360)}
	}
	a, b := toLCH(fg), toLCH(bg)
	if a[1] < 1e-7 {
		a[2] = b[2]
	}
	if b[1] < 1e-7 {
		b[2] = a[2]
	}
	hueDelta := math.Mod(b[2]-a[2]+540, 360) - 180
	return systemHex(systemRGB(oklchToRGB(a[0]+(b[0]-a[0])*0.4, a[1]+(b[1]-a[1])*0.4, a[2]+hueDelta*0.4)))
}

type themeColorValue string

func (value *themeColorValue) UnmarshalJSON(raw []byte) error {
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return fmt.Errorf("decode theme color: %w", err)
		}
		if text == "" {
			text = "default"
		}
		*value = themeColorValue(text)
		return nil
	}
	index, err := strconv.Atoi(string(raw))
	if err != nil {
		return fmt.Errorf("decode theme palette index: %w", err)
	}
	if index < 0 || index > 255 {
		return fmt.Errorf("theme palette index %d is outside 0-255", index)
	}
	*value = themeColorValue(strconv.Itoa(index))
	return nil
}
