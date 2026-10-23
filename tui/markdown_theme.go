package tui

import (
	"fmt"
	"strings"
)

// MarkdownTheme supplies foreground colors and attributes for semantic markdown
// roles. Backgrounds are deliberately inherited from the containing message.
type MarkdownTheme struct {
	Heading, Link, LinkURL, Code, CodeBlock, CodeBlockBorder Style
	Quote, QuoteBorder, HR, ListBullet                       Style
}

func (t *MarkdownTheme) role(name string) Style {
	switch name {
	case "heading":
		return t.Heading
	case "link":
		return t.Link
	case "linkURL":
		return t.LinkURL
	case "code":
		return t.Code
	case "codeBlock":
		return t.CodeBlock
	case "codeBlockBorder":
		return t.CodeBlockBorder
	case "quote":
		return t.Quote
	case "quoteBorder":
		return t.QuoteBorder
	case "hr":
		return t.HR
	case "listBullet":
		return t.ListBullet
	}
	return Style{}
}

func markdownStyle(text string, st Style) string {
	color := "39"
	if r, g, b, ok := st.FG.RGB(); ok {
		color = fmt.Sprintf("38;2;%d;%d;%d", r, g, b)
	} else if index, ok := st.FG.Index(); ok {
		color = fmt.Sprintf("38;5;%d", index)
	} else if index, ok := st.FG.BasicIndex(); ok {
		if index < 8 {
			color = fmt.Sprint(30 + index)
		} else {
			color = fmt.Sprint(90 + index - 8)
		}
	}
	prefix := "\x1b[22;" + color + "m"
	for _, attr := range []struct {
		mask AttrMask
		seq  string
	}{{AttrBold, ansiBold}, {AttrDim, "\x1b[2m"}, {AttrItalic, ansiItalic}, {AttrUnderline, ansiUnderline}} {
		if st.Attr&attr.mask != 0 {
			prefix += attr.seq
		}
	}
	// Child spans reset to the containing role, not the outer message palette.
	return prefix + strings.ReplaceAll(text, ansiReset, ansiReset+prefix) + ansiReset
}

func (m *Markdown) themeStyle(text, role string) string {
	if m.theme == nil {
		return text
	}
	return markdownStyle(text, m.theme.role(role))
}

func (r markdownASTRenderer) themeStyle(text, role string) string {
	if r.theme == nil {
		return text
	}
	return markdownStyle(text, r.theme.role(role))
}

func (r markdownASTRenderer) link(label, url string, bare bool) string {
	if r.theme == nil {
		return linkText(label, url, bare)
	}
	st := r.theme.Link
	st.Attr |= AttrUnderline
	label = markdownStyle(label, st)
	if GetTerminalCapabilities().Hyperlinks {
		return Hyperlink(label, url)
	}
	if bare {
		return label
	}
	return label + r.themeStyle(" ("+url+")", "linkURL")
}
