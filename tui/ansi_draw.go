package tui

import (
	"strconv"
	"strings"

	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/rovel/style"
)

// DrawANSIText draws a string that may contain ANSI SGR styling by splitting it
// into styled spans and passing rovel-native Style values to DrawText.
func DrawANSIText(d Drawer, pos geom.Point, s string, base style.Style) {
	if d == nil || s == "" {
		return
	}

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
	for _, part := range strings.Split(params, ";") {
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
		case 36:
			current.FG = style.ColorCyan
		case 39:
			current.FG = base.FG
		}
	}
	return current
}
