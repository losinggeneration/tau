package tui

import (
	"strconv"
	"strings"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/style"
)

// DrawANSIText draws a string that may contain ANSI SGR styling by splitting it
// into styled spans and passing rovel-native Style values to DrawText.
func DrawANSIText(d Drawer, pos geom.Point, s string, base style.Style) {
	if d == nil || s == "" {
		return
	}

	s = resolveFakeCursors(s)
	current := base
	x := pos.X
	start := 0
	flush := func(end int) {
		if end <= start {
			return
		}
		chunk := s[start:end]
		d.DrawText(geom.Point{X: x, Y: pos.Y}, chunk, current)
		x += TextWidth(chunk)
	}

	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], CursorMarker) {
			flush(i)
			if cursor, ok := d.(rovel.CursorDrawer); ok {
				cursor.SetCursor(geom.Point{X: x, Y: pos.Y})
			}
			i += len(CursorMarker)
			start = i
			continue
		}

		if s[i] != 0x1b || i+1 >= len(s) || s[i+1] != '[' {
			i++
			continue
		}

		end := i + 2
		for end < len(s) && s[end] != 'm' {
			end++
		}
		if end >= len(s) {
			i++
			continue
		}

		flush(i)
		current = applySGR(current, base, s[i+2:end])
		i = end + 1
		start = i
	}
	flush(len(s))
}

func applySGR(current, base style.Style, params string) style.Style {
	if params == "" {
		return base
	}
	parts := strings.Split(params, ";")
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if part == "" {
			part = "0"
		}
		code, err := strconv.Atoi(part)
		if err != nil {
			continue
		}
		switch code {
		case 0:
			current = base
		case 1:
			current.Attr |= style.AttrBold
		case 2:
			current.Attr |= style.AttrDim
		case 3:
			current.Attr |= style.AttrItalic
		case 4:
			current.Attr |= style.AttrUnderline
		case 7:
			current.Attr |= style.AttrReverse
		case 9:
			// Rovel currently has no strikethrough attribute. Underline is a
			// visible fallback that keeps the span distinguished in cell renderers.
			current.Attr |= style.AttrUnderline
		case 22:
			current.Attr &^= style.AttrBold | style.AttrDim
		case 23:
			current.Attr &^= style.AttrItalic
		case 24:
			current.Attr &^= style.AttrUnderline
		case 27:
			current.Attr &^= style.AttrReverse
		case 29:
			current.Attr &^= style.AttrUnderline
		case 30, 31, 32, 33, 34, 35, 36, 37:
			current.FG = style.ColorBasic(uint8(code - 30))
		case 90, 91, 92, 93, 94, 95, 96, 97:
			current.FG = style.ColorBasic(uint8(code - 90 + 8))
		case 38:
			var skip int
			current.FG, skip = extendedSGRColor(current.FG, parts[i+1:])
			i += skip
		case 39:
			current.FG = style.ColorDefault
		case 40, 41, 42, 43, 44, 45, 46, 47:
			current.BG = style.ColorBasic(uint8(code - 40))
		case 100, 101, 102, 103, 104, 105, 106, 107:
			current.BG = style.ColorBasic(uint8(code - 100 + 8))
		case 48:
			var skip int
			current.BG, skip = extendedSGRColor(current.BG, parts[i+1:])
			i += skip
		case 49:
			// The containing surface owns the background, not the terminal.
			current.BG = base.BG
		}
	}
	return current
}

// extendedSGRColor parses the 5;n or 2;r;g;b tail of SGR 38/48 and returns
// how many parameters it consumed. Malformed values keep the current color.
func extendedSGRColor(current style.Color, rest []string) (style.Color, int) {
	switch {
	case len(rest) >= 2 && rest[0] == "5":
		if value, err := strconv.ParseUint(rest[1], 10, 8); err == nil {
			current = style.ColorIndex(uint8(value))
		}
		return current, 2
	case len(rest) >= 4 && rest[0] == "2":
		red, redErr := strconv.ParseUint(rest[1], 10, 8)
		green, greenErr := strconv.ParseUint(rest[2], 10, 8)
		blue, blueErr := strconv.ParseUint(rest[3], 10, 8)
		if redErr == nil && greenErr == nil && blueErr == nil {
			current = style.ColorRGB(uint8(red), uint8(green), uint8(blue))
		}
		return current, 4
	}
	return current, 0
}
