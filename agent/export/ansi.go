package export

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ansiColors is the standard 16-color ANSI palette.
var ansiColors = [16]string{
	"#000000", // 0: black
	"#cc0000", // 1: red
	"#4e9a06", // 2: green
	"#c4a000", // 3: yellow
	"#3465a4", // 4: blue
	"#75507b", // 5: magenta
	"#06989a", // 6: cyan
	"#d3d7cf", // 7: white
	"#555753", // 8: bright black
	"#ef2929", // 9: bright red
	"#8ae234", // 10: bright green
	"#fce94f", // 11: bright yellow
	"#729fcf", // 12: bright blue
	"#ad7fa8", // 13: bright magenta
	"#34e2e2", // 14: bright cyan
	"#eeeeec", // 15: bright white
}

// color256ToHex converts a 256-color index to a hex color string.
func color256ToHex(index int) string {
	if index < 16 {
		return ansiColors[index]
	}
	if index < 232 {
		// 6×6×6 color cube
		ci := index - 16
		r := ci / 36
		g := (ci % 36) / 6
		b := ci % 6
		return fmt.Sprintf("#%02x%02x%02x", r*51, g*51, b*51)
	}
	// Grayscale ramp
	gray := 8 + (index-232)*10
	return fmt.Sprintf("#%02x%02x%02x", gray, gray, gray)
}

// escapeHTML escapes special HTML characters in text.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

type textStyle struct {
	bold      bool
	dim       bool
	italic    bool
	underline bool
	fgHex     string
	bgHex     string
}

func (ts textStyle) hasStyle() bool {
	return ts.bold || ts.dim || ts.italic || ts.underline || ts.fgHex != "" || ts.bgHex != ""
}

func (ts textStyle) toCSS() string {
	var parts []string
	if ts.fgHex != "" {
		parts = append(parts, "color:"+ts.fgHex)
	}
	if ts.bgHex != "" {
		parts = append(parts, "background-color:"+ts.bgHex)
	}
	if ts.bold {
		parts = append(parts, "font-weight:bold")
	}
	if ts.dim {
		parts = append(parts, "opacity:0.7")
	}
	if ts.italic {
		parts = append(parts, "font-style:italic")
	}
	if ts.underline {
		parts = append(parts, "text-decoration:underline")
	}
	return strings.Join(parts, ";")
}

// applySGR applies SGR parameter codes to the style, returning updated style.
func applySGR(params []int, style textStyle) textStyle {
	i := 0
	for i < len(params) {
		code := params[i]
		switch {
		case code == 0:
			style = textStyle{}
		case code == 1:
			style.bold = true
		case code == 2:
			style.dim = true
		case code == 3:
			style.italic = true
		case code == 4:
			style.underline = true
		case code == 7:
			// reverse — no-op per spec
		case code == 22:
			style.bold = false
			style.dim = false
		case code == 23:
			style.italic = false
		case code == 24:
			style.underline = false
		case code == 27:
			// no reverse — no-op
		case code >= 30 && code <= 37:
			style.fgHex = ansiColors[code-30]
		case code == 38:
			if i+1 < len(params) && params[i+1] == 5 && i+2 < len(params) {
				style.fgHex = color256ToHex(params[i+2])
				i += 2
			} else if i+1 < len(params) && params[i+1] == 2 && i+4 < len(params) {
				r, g, b := params[i+2], params[i+3], params[i+4]
				style.fgHex = fmt.Sprintf("#%02x%02x%02x", r, g, b)
				i += 4
			}
		case code == 39:
			style.fgHex = ""
		case code >= 40 && code <= 47:
			style.bgHex = ansiColors[code-40]
		case code == 48:
			if i+1 < len(params) && params[i+1] == 5 && i+2 < len(params) {
				style.bgHex = color256ToHex(params[i+2])
				i += 2
			} else if i+1 < len(params) && params[i+1] == 2 && i+4 < len(params) {
				r, g, b := params[i+2], params[i+3], params[i+4]
				style.bgHex = fmt.Sprintf("#%02x%02x%02x", r, g, b)
				i += 4
			}
		case code == 49:
			style.bgHex = ""
		case code >= 90 && code <= 97:
			style.fgHex = ansiColors[code-90+8]
		case code >= 100 && code <= 107:
			style.bgHex = ansiColors[code-100+8]
		}
		i++
	}
	return style
}

// ansiSGRRegex matches SGR sequences (ending with 'm').
var ansiSGRRegex = regexp.MustCompile(`\x1b\[([\d;]*)m`)

// ansiCSIRegex matches all CSI sequences (any final byte).
var ansiCSIRegex = regexp.MustCompile(`\x1b\[[\d;]*[a-zA-Z]`)

// AnsiToHTML converts a string containing ANSI escape sequences into HTML.
// Handles SGR (Select Graphic Rendition) codes for color/weight/style.
// Other CSI sequences (cursor movement, etc.) are stripped.
// Special HTML characters in the input are escaped.
func AnsiToHTML(s string) string {
	var buf strings.Builder
	style := textStyle{}
	inSpan := false
	lastIndex := 0

	// Find all CSI sequences to strip non-SGR ones alongside processing SGR.
	// Strategy: find all SGR matches with their positions, and also strip non-SGR CSI.
	// We'll process character by character using the SGR regex for matches,
	// and pre-strip non-SGR CSI sequences from the text segments in between.

	matches := ansiSGRRegex.FindAllStringIndex(s, -1)

	// Helper: emit text (with non-SGR CSI stripped and HTML escaped).
	emitText := func(text string) {
		// Strip any remaining non-SGR CSI sequences (e.g. cursor movement).
		stripped := ansiCSIRegex.ReplaceAllString(text, "")
		if stripped != "" {
			buf.WriteString(escapeHTML(stripped))
		}
	}

	for _, loc := range matches {
		start, end := loc[0], loc[1]

		// Emit text before this escape sequence.
		emitText(s[lastIndex:start])

		// Extract the SGR match.
		seq := s[start:end]
		// Parse params from the sequence: \x1b[ params m
		paramStr := seq[2 : len(seq)-1]
		var params []int
		if paramStr == "" {
			params = []int{0}
		} else {
			for _, p := range strings.Split(paramStr, ";") {
				n, err := strconv.Atoi(p)
				if err != nil {
					n = 0
				}
				params = append(params, n)
			}
		}

		// Close existing span if open.
		if inSpan {
			buf.WriteString("</span>")
			inSpan = false
		}

		// Apply SGR codes.
		style = applySGR(params, style)

		// Open new span if needed.
		if style.hasStyle() {
			buf.WriteString(`<span style="`)
			buf.WriteString(style.toCSS())
			buf.WriteString(`">`)
			inSpan = true
		}

		lastIndex = end
	}

	// Emit remaining text.
	emitText(s[lastIndex:])

	// Close trailing span.
	if inSpan {
		buf.WriteString("</span>")
	}

	return buf.String()
}
