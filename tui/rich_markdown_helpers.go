package tui

import (
	"strings"

	"github.com/losinggeneration/tau/tui/ansi"
)

func htmlLikeText(s string) string {
	s = strings.ReplaceAll(s, "<br>", "")
	s = strings.ReplaceAll(s, "<br/>", "")
	for {
		start := strings.IndexByte(s, '<')
		if start < 0 {
			return s
		}
		end := strings.IndexByte(s[start:], '>')
		if end < 0 {
			return s
		}
		end += start
		s = s[:start] + s[end+1:]
	}
}

const (
	ansiReset         = "\x1b[0m"
	ansiBold          = "\x1b[1m"
	ansiItalic        = "\x1b[3m"
	ansiUnderline     = "\x1b[4m"
	ansiStrikethrough = "\x1b[9m"
	ansiCyan          = "\x1b[36m"
	ansiInlineCode    = "\x1b[7m"
)

func renderInline(s string) string {
	s = replaceStrictStrikethrough(s)
	s = replaceStyled(s, "**", ansiBold)
	s = replaceStyled(s, "__", ansiUnderline)
	s = replaceStyled(s, "*", ansiItalic)
	s = replaceStyled(s, "_", ansiItalic)
	s = replaceStyled(s, "`", ansiInlineCode)
	s = renderMarkdownLinks(s)
	return s
}

func replaceStrictStrikethrough(s string) string {
	for {
		start := strings.Index(s, "~~")
		if start < 0 {
			return s
		}
		end := strings.Index(s[start+2:], "~~")
		if end < 0 {
			return s
		}
		end += start + 2
		inner := s[start+2 : end]
		if inner == "" || strings.HasPrefix(inner, " ") || strings.HasSuffix(inner, " ") || strings.HasPrefix(inner, "~") || strings.HasSuffix(inner, "~") {
			searchFrom := end + 2
			if searchFrom >= len(s) {
				return s
			}
			next := strings.Index(s[searchFrom:], "~~")
			if next < 0 {
				return s
			}
			end = searchFrom + next
			inner = s[start+2 : end]
			if inner == "" || strings.HasPrefix(inner, " ") || strings.HasSuffix(inner, " ") || strings.HasPrefix(inner, "~") || strings.HasSuffix(inner, "~") {
				return s
			}
		}
		s = s[:start] + ansiStrikethrough + inner + ansiReset + s[end+2:]
	}
}

func replaceStyled(s, marker, style string) string {
	for {
		start := strings.Index(s, marker)
		if start < 0 {
			return s
		}
		end := strings.Index(s[start+len(marker):], marker)
		if end < 0 {
			return s
		}
		end += start + len(marker)
		inner := s[start+len(marker) : end]
		if strings.TrimSpace(inner) == "" {
			return s
		}
		s = s[:start] + style + inner + ansiReset + s[end+len(marker):]
	}
}

func renderMarkdownLinks(s string) string {
	for {
		close := strings.Index(s, "](")
		if close < 0 {
			return renderBareAutolinks(s)
		}
		open := strings.LastIndex(s[:close], "[")
		if open < 0 {
			return renderBareAutolinks(s)
		}
		end := strings.Index(s[close+2:], ")")
		if end < 0 {
			return renderBareAutolinks(s)
		}
		end += close + 2
		label := s[open+1 : close]
		url := s[close+2 : end]
		repl := linkText(label, url, false)
		s = s[:open] + repl + s[end+1:]
	}
}

func renderBareAutolinks(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return s
	}
	out := s
	for _, field := range fields {
		trimmed := strings.TrimRight(field, ".,;:!?)"+"]")
		trail := field[len(trimmed):]
		if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") || looksLikeEmail(trimmed) {
			url := trimmed
			if looksLikeEmail(trimmed) && !strings.HasPrefix(trimmed, "mailto:") {
				url = "mailto:" + trimmed
			}
			repl := linkText(trimmed, url, true) + trail
			out = strings.Replace(out, field, repl, 1)
		}
	}
	return out
}

func linkText(label, url string, bare bool) string {
	if url == "" {
		return label
	}
	if GetTerminalCapabilities().Hyperlinks {
		return Hyperlink(label, url)
	}
	if bare {
		return label
	}
	return label + " (" + url + ")"
}

func looksLikeEmail(s string) bool {
	at := strings.Index(s, "@")
	return at > 0 && at < len(s)-1 && strings.Contains(s[at+1:], ".") && !strings.Contains(s, "://")
}

func renderTableRow(row []string, widths []int, align []string) string {
	parts := make([]string, len(widths))
	for i := range widths {
		cell := ""
		if i < len(row) {
			cell = renderInline(strings.TrimSpace(row[i]))
		}
		pad := widths[i] - TextWidth(cell)
		left, right := 0, pad
		if i < len(align) {
			switch align[i] {
			case "right":
				left, right = pad, 0
			case "center":
				left, right = pad/2, pad-pad/2
			}
		}
		parts[i] = " " + repeatSpace(left) + cell + repeatSpace(right) + " "
	}
	return "│" + strings.Join(parts, "│") + "│"
}

// wrapCell wraps cell content to width visible cells, breaking at word
// boundaries where possible and hard-breaking words wider than width.
func wrapCell(cell string, width int) []string {
	return ansi.WrapWords(cell, width)
}

// renderTableRule draws a horizontal table rule: a run of ─ per column
// joined by the junction glyph and closed by the given corner glyphs.
func renderTableRule(left, junction, right string, widths []int) string {
	parts := make([]string, len(widths))
	for i, w := range widths {
		parts[i] = strings.Repeat(borderHorizontal, w+2)
	}
	return left + strings.Join(parts, junction) + right
}

func renderTableDivider(widths []int) string {
	return renderTableRule(borderLeftTee, "┼", borderRightTee, widths)
}

// renderTableTop and renderTableBottom enclose a table with the same rounded
// corners the tau look's bordered chrome uses, so a table reads as another
// bordered surface rather than a bare grid.
func renderTableTop(widths []int) string {
	return renderTableRule(borderTopLeft, "┬", borderTopRight, widths)
}

func renderTableBottom(widths []int) string {
	return renderTableRule(borderBottomLeft, "┴", borderBottomRight, widths)
}
