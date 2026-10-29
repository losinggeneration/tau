package tui

import (
	"fmt"
	"math"
	"slices"
	"strconv"

	tauui "github.com/losinggeneration/tau/tui"
)

// A terminal palette anchors hue, saturation, and absolute chroma at its own lightness.
type systemFamily struct {
	hue, minimum, maximum float64
	slot                  int
}

var systemFamilies = map[string]systemFamily{
	"neutral":            {231.49, 0.02, 0.08, 8},
	"blue":               {231.49, 0.1, 0.68, 4},
	"green":              {158.68, 0.1, 0.76, 2},
	"red":                {20, 0.1, 0.92, 1},
	"yellow":             {82.36, 0.5, 1, 3},
	"orange":             {52, 0.12, 0.85, 3},
	"violet":             {295, 0.2, 0.6, 5},
	"calamine":           {202.43, 0.1, 0.74, 6},
	"thinkingSlate":      {231.49, 0.08, 0.2, 4},
	"thinkingBlue":       {231.49, 0.2, 0.45, 4},
	"thinkingPeriwinkle": {263.25, 0.3, 0.6, 6},
	"thinkingViolet":     {295, 0.4, 0.75, 5},
	"thinkingMagenta":    {337.5, 0.5, 0.85, 13},
	"thinkingRed":        {20, 0.95, 1, 1},
}

var systemTokenFamilies = map[string]string{
	"selectedBg":         "blue",
	"searchMatchBg":      "orange",
	"userMessageBg":      "blue",
	"customMessageBg":    "violet",
	"toolPendingBg":      "neutral",
	"toolSuccessBg":      "green",
	"toolErrorBg":        "red",
	"text":               "neutral",
	"userMessageText":    "neutral",
	"customMessageText":  "neutral",
	"toolTitle":          "neutral",
	"syntaxOperator":     "neutral",
	"syntaxPunctuation":  "neutral",
	"muted":              "neutral",
	"dim":                "neutral",
	"thinkingText":       "neutral",
	"toolOutput":         "neutral",
	"mdLinkUrl":          "neutral",
	"mdQuote":            "neutral",
	"mdQuoteBorder":      "neutral",
	"mdHr":               "neutral",
	"mdCodeBlockBorder":  "neutral",
	"toolDiffContext":    "neutral",
	"syntaxComment":      "neutral",
	"scrollbarTrack":     "neutral",
	"scrollbarThumb":     "neutral",
	"searchMatchText":    "neutral",
	"borderMuted":        "neutral",
	"accent":             "violet",
	"borderAccent":       "violet",
	"customMessageLabel": "violet",
	"mdCode":             "violet",
	"mdListBullet":       "violet",
	"syntaxType":         "violet",
	"border":             "blue",
	"mdLink":             "blue",
	"syntaxKeyword":      "blue",
	"syntaxVariable":     "calamine",
	"success":            "green",
	"mdCodeBlock":        "green",
	"toolDiffAdded":      "green",
	"bashMode":           "green",
	"syntaxNumber":       "green",
	"error":              "red",
	"toolDiffRemoved":    "red",
	"warning":            "yellow",
	"mdHeading":          "yellow",
	"syntaxFunction":     "yellow",
	"syntaxString":       "orange",
	"thinkingOff":        "neutral",
	"thinkingMinimal":    "thinkingSlate",
	"thinkingLow":        "thinkingBlue",
	"thinkingMedium":     "thinkingPeriwinkle",
	"thinkingHigh":       "thinkingViolet",
	"thinkingXhigh":      "thinkingMagenta",
	"thinkingMax":        "thinkingRed",
}
var systemTokenSlots = map[string]int{"syntaxString": 2, "syntaxNumber": 5, "searchMatchBg": 3}

type systemCurve struct {
	coefficients [6]float64
	reachable    [2]float64
}

var systemLevels = map[string][2]systemCurve{
	"panel":       {{[6]float64{0.29131, -0.39746, 2.33185, -0.85524, -1.2076, 0.86276}, [2]float64{0, 0.979}}, {[6]float64{-3.74073, 27.94549, -78.44258, 112.6798, -79.60015, 22.11277}, [2]float64{0.348, 1}}},
	"track":       {{[6]float64{0.39028, -0.23015, 0.83573, 2.43829, -4.38292, 2.01582}, [2]float64{0, 0.946}}, {[6]float64{-5.24921, 38.37322, -107.28833, 152.10005, -106.17127, 29.18061}, [2]float64{0.368, 1}}},
	"thinking0":   {{[6]float64{0.52988, -0.05809, -0.30924, 4.63567, -6.52933, 2.89108}, [2]float64{0, 0.873}}, {[6]float64{-28.27749, 182.85284, -469.62416, 603.15916, -384.59976, 97.35147}, [2]float64{0.51, 1}}},
	"thinking1":   {{[6]float64{0.55278, -0.03667, -0.45659, 4.95347, -6.90265, 3.0706}, [2]float64{0, 0.858}}, {[6]float64{-37.10484, 235.86282, -596.62344, 754.3633, -474.00763, 118.3551}, [2]float64{0.535, 1}}},
	"thinking2":   {{[6]float64{0.57486, -0.01765, -0.58987, 5.25227, -7.27175, 3.25532}, [2]float64{0, 0.842}}, {[6]float64{-59.89653, 377.05024, -945.07843, 1182.03145, -734.96375, 181.68658}, [2]float64{0.556, 1}}},
	"thinking3":   {{[6]float64{0.59621, -0.00062, -0.71148, 5.53588, -7.6392, 3.44606}, [2]float64{0, 0.827}}, {[6]float64{-72.07122, 445.84082, -1099.57352, 1353.88793, -829.53392, 202.26164}, [2]float64{0.58, 1}}},
	"thinking4":   {{[6]float64{0.61691, 0.01462, -0.82288, 5.80651, -8.00641, 3.64333}, [2]float64{0, 0.811}}, {[6]float64{-110.14338, 674.21488, -1645.75941, 2004.32367, -1215.15899, 293.3183}, [2]float64{0.6, 1}}},
	"thinking5":   {{[6]float64{0.63702, 0.02826, -0.92498, 6.06465, -8.37246, 3.84651}, [2]float64{0, 0.795}}, {[6]float64{-175.47701, 1063.54495, -2570.70594, 3098.80776, -1860.15527, 444.76392}, [2]float64{0.62, 1}}},
	"thinking6":   {{[6]float64{0.65658, 0.04044, -1.01835, 6.30989, -8.73529, 4.05439}, [2]float64{0, 0.779}}, {[6]float64{-183.81712, 1094.70055, -2602.68539, 3088.71276, -1826.91131, 430.75931}, [2]float64{0.643, 1}}},
	"subtle":      {{[6]float64{0.56762, -0.02475, -0.5383, 5.12628, -7.10931, 3.17324}, [2]float64{0, 0.848}}, {[6]float64{-232.85459, 1376.54473, -3249.11801, 3827.91186, -2248.29472, 526.55751}, [2]float64{0.657, 1}}},
	"thumb":       {{[6]float64{0.60323, 0.00278, -0.73328, 5.57157, -7.68067, 3.46933}, [2]float64{0, 0.823}}, {[6]float64{-82.89897, 511.01355, -1255.98095, 1540.76821, -940.68087, 228.58523}, [2]float64{0.586, 1}}},
	"readable":    {{[6]float64{0.66937, 0.04704, -1.06871, 6.43941, -8.9332, 4.17229}, [2]float64{0, 0.77}}, {[6]float64{-1554.52576, 8733.56817, -19604.93507, 21977.72696, -12300.99599, 2749.81288}, [2]float64{0.751, 1}}},
	"emphasis":    {{[6]float64{0.7303, 0.07695, -1.31626, 7.1681, -10.14436, 4.92846}, [2]float64{0, 0.712}}, {[6]float64{-4948.31942, 26870.91986, -58334.48399, 63280.17197, -34298.01053, 7430.30146}, [2]float64{0.811, 1}}},
	"textOnPanel": {{[6]float64{0.86713, 0.05232, -0.89428, 4.79014, -5.5432, 1.75023}, [2]float64{0, 0.542}}, {[6]float64{-8570.89457, 43954.60805, -90084.00702, 92220.6791, -47152.15802, 9632.27113}, [2]float64{0.867, 1}}},
	"text":        {{[6]float64{0.89242, 0.02311, -0.44862, 2.34417, -0.06084, -2.63844}, [2]float64{0, 0.5}}, {[6]float64{-2004.67048, 6664.47299, -6060.70202, -1792.61209, 5133.82359, -1939.85583}, [2]float64{0.894, 1}}},
}

var (
	systemPanels           = []string{"userMessageBg", "toolPendingBg", "toolSuccessBg", "toolErrorBg", "selectedBg", "searchMatchBg", "customMessageBg"}
	systemForegroundTokens = []string{"text", "userMessageText", "toolTitle"}
)

type systemRule struct {
	token string
	on    []string
	level string
}

var (
	systemRules      = buildSystemRules()
	systemSolveOrder = systemDependencyOrder()
)

func buildSystemRules() []systemRule {
	var rules []systemRule
	each := func(tokens, on []string, level string) {
		for _, token := range tokens {
			rules = append(rules, systemRule{token, on, level})
		}
	}
	tool := []string{"toolPendingBg", "toolSuccessBg", "toolErrorBg"}
	messages := []string{"userMessageBg", "customMessageBg"}
	surfaces := func(base []string, extra ...string) []string { return append(slices.Clone(base), extra...) }
	background := []string{"background"}
	each(systemPanels, background, "panel")
	each([]string{"text"}, background, "text")
	each([]string{"text"}, []string{"selectedBg"}, "textOnPanel")
	each([]string{"userMessageText"}, []string{"userMessageBg"}, "textOnPanel")
	each([]string{"toolTitle"}, tool, "textOnPanel")
	each([]string{"accent", "success", "error", "warning"}, surfaces(tool, "background", "selectedBg"), "readable")
	each([]string{"muted"}, surfaces(tool, "background", "selectedBg", "customMessageBg"), "readable")
	each([]string{"dim"}, surfaces(tool, "background", "selectedBg", "customMessageBg"), "subtle")
	each([]string{"thinkingText"}, background, "readable")
	each([]string{"customMessageText"}, surfaces(tool, "customMessageBg"), "readable")
	each([]string{"customMessageLabel"}, surfaces(tool, "background", "customMessageBg", "selectedBg"), "readable")
	each([]string{"toolOutput"}, surfaces(tool, "background"), "readable")
	each([]string{"mdHeading", "mdLink", "mdLinkUrl", "mdCode", "mdQuote", "mdCodeBlockBorder", "mdListBullet"}, surfaces(messages, "background"), "readable")
	all := surfaces(messages, tool...)
	all = surfaces(all, "background")
	each([]string{"mdCodeBlock"}, all, "readable")
	each([]string{"toolDiffAdded", "toolDiffRemoved", "toolDiffContext"}, surfaces(tool, "background"), "readable")
	each([]string{"syntaxComment", "syntaxKeyword", "syntaxFunction", "syntaxVariable", "syntaxString", "syntaxNumber", "syntaxType", "syntaxOperator", "syntaxPunctuation"}, all, "readable")
	each([]string{"searchMatchText"}, []string{"searchMatchBg"}, "readable")
	each([]string{"bashMode", "border", "borderAccent"}, background, "readable")
	each([]string{"borderMuted"}, background, "subtle")
	each([]string{"mdQuoteBorder", "mdHr"}, all, "readable")
	each([]string{"scrollbarTrack"}, background, "track")
	each([]string{"scrollbarThumb"}, []string{"scrollbarTrack"}, "thumb")
	for i, token := range []string{"thinkingOff", "thinkingMinimal", "thinkingLow", "thinkingMedium", "thinkingHigh", "thinkingXhigh", "thinkingMax"} {
		each([]string{token}, background, "thinking"+strconv.Itoa(i))
	}
	return rules
}

func systemDependencyOrder() []string {
	var order []string
	var visit func(string)
	visit = func(token string) {
		if slices.Contains(order, token) {
			return
		}
		for _, rule := range systemRules {
			if rule.token == token {
				for _, surface := range rule.on {
					if surface != "background" {
						visit(surface)
					}
				}
			}
		}
		order = append(order, token)
	}
	for _, rule := range systemRules {
		visit(rule.token)
	}
	return order
}

type systemThemeResult struct {
	colors     map[string]string
	dim        []string
	appearance string
}

func relativeLuminance(color tauui.RGBColor) float64 {
	return 0.2126*srgbToLinear(float64(color.R)/255) + 0.7152*srgbToLinear(float64(color.G)/255) + 0.0722*srgbToLinear(float64(color.B)/255)
}

func wcagContrast(a, b tauui.RGBColor) float64 {
	first, second := relativeLuminance(a), relativeLuminance(b)
	return (max(first, second) + 0.05) / (min(first, second) + 0.05)
}

func terminalAppearance(background tauui.RGBColor, foreground *tauui.RGBColor) string {
	white, black := wcagContrast(tauui.RGBColor{R: 255, G: 255, B: 255}, background), wcagContrast(tauui.RGBColor{}, background)
	if foreground != nil {
		difference := rgbToOklab(*foreground)[0] - rgbToOklab(background)[0]
		if math.Abs(difference) > 0.05 {
			if difference > 0 && white >= 4.5 {
				return "dark"
			}
			if difference < 0 && black >= 4.5 {
				return "light"
			}
		}
	}
	if white >= black {
		return "dark"
	}
	return "light"
}

func bellWeight(lightness float64) float64 {
	gaussian := func(x float64) float64 { v := x - 0.5; return math.Exp(-v * v / (2 * 0.25 * 0.25)) }
	return (gaussian(lightness) - gaussian(0)) / (1 - gaussian(0))
}

func (f systemFamily) saturationCurve(lightness float64) float64 {
	floor := 1.0
	if f.maximum > 0 {
		floor = f.minimum / f.maximum
	}
	return floor + (1-floor)*bellWeight(lightness)
}

func systemLevelTarget(level, appearance string, surface float64) (float64, bool) {
	index := 0
	if appearance == "light" {
		index = 1
	}
	curve := systemLevels[level][index]
	if surface < curve.reachable[0] || surface > curve.reachable[1] {
		return 0, false
	}
	var sum float64
	for power, coefficient := range curve.coefficients {
		sum += coefficient * math.Pow(surface, float64(power))
	}
	return sum, true
}

type systemSource struct{ h, s, l, chroma float64 }

func systemSourceOf(color tauui.RGBColor) systemSource {
	h, s, l := rgbToOkhsl(color)
	lab := rgbToOklab(color)
	return systemSource{h, s, l, math.Hypot(lab[1], lab[2])}
}

func systemRGB(rgb [3]uint8) tauui.RGBColor { return tauui.RGBColor{R: rgb[0], G: rgb[1], B: rgb[2]} }

func systemHex(color tauui.RGBColor) string {
	return fmt.Sprintf("#%02x%02x%02x", color.R, color.G, color.B)
}

func anchoredSystemColor(source systemSource, family systemFamily, lightness, saturation float64) tauui.RGBColor {
	anchor := family.saturationCurve(source.l)
	falloff := 1.0
	if anchor > 0 {
		falloff = min(1, family.saturationCurve(lightness)/anchor)
	}
	color := systemRGB(okhslToRGB(source.h, source.s*falloff*saturation, lightness))
	cap := source.chroma * falloff * saturation
	lab := rgbToOklab(color)
	if math.Hypot(lab[1], lab[2]) <= cap {
		return color
	}
	return systemRGB(oklchToRGB(lab[0], cap, source.h))
}

func withSystemTextContrast(color tauui.RGBColor, surfaces []tauui.RGBColor, appearance string) tauui.RGBColor {
	meets := func(candidate tauui.RGBColor) bool {
		for _, surface := range surfaces {
			if wcagContrast(candidate, surface) < 4.5 {
				return false
			}
		}
		return true
	}
	if meets(color) {
		return color
	}
	h, s, l := rgbToOkhsl(color)
	at := func(lightness float64) tauui.RGBColor { return systemRGB(okhslToRGB(h, s, lightness)) }
	extreme := 0.0
	if appearance == "dark" {
		extreme = 1
	}
	if !meets(at(extreme)) {
		return at(extreme)
	}
	low, high := l, extreme
	for range 20 {
		middle := (low + high) / 2
		if meets(at(middle)) {
			high = middle
		} else {
			low = middle
		}
	}
	return at(high)
}

func generateSystemTheme(input tauui.TerminalColors, saturation float64, hint string) systemThemeResult {
	saturation = min(1, max(0, saturation))
	result := systemThemeResult{colors: make(map[string]string), appearance: hint}
	if input.Background == nil {
		for token, familyName := range systemTokenFamilies {
			result.colors[token] = "default"
			if slices.Contains(systemPanels, token) {
				continue
			}
			if familyName != "neutral" && saturation > 0 {
				slot := systemFamilies[familyName].slot
				if override, ok := systemTokenSlots[token]; ok {
					slot = override
				}
				result.colors[token] = strconv.Itoa(slot)
			}
			if familyName == "neutral" && !slices.Contains(systemForegroundTokens, token) {
				result.dim = append(result.dim, token)
			}
		}
		slices.Sort(result.dim)
		return result
	}
	background := *input.Background
	appearance := terminalAppearance(background, input.Foreground)
	result.appearance = appearance
	lighter := appearance == "dark"
	extreme := 0.0
	if lighter {
		extreme = 1
	}
	backgroundL := rgbToOklab(background)[0]
	var palette [16]systemSource
	if input.Palette != nil {
		for i, color := range input.Palette {
			palette[i] = systemSourceOf(color)
		}
	}
	paint := func(token string, labL float64) tauui.RGBColor {
		lightness := oklabToOkhslLightness(labL)
		family := systemFamilies[systemTokenFamilies[token]]
		if input.Palette == nil {
			return systemRGB(okhslToRGB(family.hue, (family.minimum+(family.maximum-family.minimum)*bellWeight(lightness))*saturation, lightness))
		}
		slot := family.slot
		if override, ok := systemTokenSlots[token]; ok {
			slot = override
		}
		return anchoredSystemColor(palette[slot], family, lightness, saturation)
	}
	target := func(level string, surfaceL, t float64) (float64, bool) {
		reached, ok := systemLevelTarget(level, appearance, surfaceL)
		if !ok && t == 0 {
			return 0, false
		}
		if !ok {
			reached = extreme
		}
		floorLevel := "readable"
		if !lighter {
			floorLevel = "subtle"
		}
		floor, ok := systemLevelTarget(floorLevel, appearance, surfaceL)
		if !ok {
			floor = extreme
		}
		distance, floorDistance := reached-surfaceL, floor-surfaceL
		compressed := distance
		if math.Abs(distance) > math.Abs(floorDistance) {
			compressed -= (distance - floorDistance) * min(t, 1)
		}
		return surfaceL + compressed*(1-max(0, t-1)), true
	}
	extremeText := tauui.RGBColor{}
	if lighter {
		extremeText = tauui.RGBColor{R: 255, G: 255, B: 255}
	}
	readable := func(color tauui.RGBColor) bool { return wcagContrast(extremeText, color) >= 4.5 }
	limitPanel := func(token string, l float64) tauui.RGBColor {
		color := paint(token, l)
		if readable(color) {
			return color
		}
		low, high := backgroundL, l
		for range 20 {
			middle := (low + high) / 2
			if readable(paint(token, middle)) {
				low = middle
			} else {
				high = middle
			}
		}
		return paint(token, low)
	}
	solve := func(t float64) map[string]tauui.RGBColor {
		colors := map[string]tauui.RGBColor{"background": background}
		for _, token := range systemSolveOrder {
			l := 1.0
			if lighter {
				l = 0
			}
			for _, rule := range systemRules {
				if rule.token != token {
					continue
				}
				for _, surface := range rule.on {
					color, ok := colors[surface]
					if !ok {
						color = background
					}
					value, ok := target(rule.level, rgbToOklab(color)[0], t)
					if !ok || value < 0 || value > 1 {
						return nil
					}
					if lighter {
						l = max(l, value)
					} else {
						l = min(l, value)
					}
				}
			}
			if slices.Contains(systemPanels, token) {
				colors[token] = limitPanel(token, l)
			} else {
				colors[token] = paint(token, l)
			}
		}
		return colors
	}
	relaxation := 0.0
	solved := solve(0)
	if solved == nil {
		low, high := 0.0, 2.0
		solved = solve(high)
		for range 20 {
			middle := (low + high) / 2
			if attempt := solve(middle); attempt != nil {
				high, solved = middle, attempt
			} else {
				low = middle
			}
		}
		relaxation = high
	}
	for token := range systemTokenFamilies {
		result.colors[token] = "default"
		if color, ok := solved[token]; ok {
			result.colors[token] = systemHex(color)
		}
	}
	for _, token := range systemForegroundTokens {
		var surfaces []tauui.RGBColor
		for _, rule := range systemRules {
			if rule.token == token {
				for _, surface := range rule.on {
					color, ok := solved[surface]
					if !ok {
						color = background
					}
					surfaces = append(surfaces, color)
				}
			}
		}
		text, hasText := solved[token]
		if input.Foreground != nil {
			needed := 1.0
			if lighter {
				needed = 0
			}
			reachable := true
			for _, surface := range surfaces {
				value, ok := target("emphasis", rgbToOklab(surface)[0], relaxation)
				if !ok || value < 0 || value > 1 {
					reachable = false
					break
				}
				if lighter {
					needed = max(needed, value)
				} else {
					needed = min(needed, value)
				}
			}
			if reachable {
				foregroundL := rgbToOklab(*input.Foreground)[0]
				if (lighter && foregroundL >= needed) || (!lighter && foregroundL <= needed) {
					result.colors[token] = "default"
					continue
				}
				text = anchoredSystemColor(systemSourceOf(*input.Foreground), systemFamilies["neutral"], oklabToOkhslLightness(needed), saturation)
				hasText = true
			}
		}
		if hasText {
			result.colors[token] = systemHex(withSystemTextContrast(text, surfaces, appearance))
		}
	}
	return result
}
