package tui

import (
	"fmt"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// MarkdownTheme supplies foreground colors and attributes for semantic markdown
// roles. Backgrounds are deliberately inherited from the containing message.
type MarkdownTheme struct {
	Heading, Link, LinkURL, Code, CodeBlock, CodeBlockBorder Style
	Quote, QuoteBorder, HR, ListBullet                       Style
	// Syntax colors fenced code in a known language. Nil leaves code blocks
	// in the CodeBlock style.
	Syntax *SyntaxTheme
}

// SyntaxTheme colors highlighted code by token kind. Text styles everything
// the other kinds don't cover.
type SyntaxTheme struct {
	Text, Comment, Keyword, Function, Variable, String Style
	Number, Type, Operator, Punctuation                Style
}

func (t *SyntaxTheme) style(kind chroma.TokenType) Style {
	switch {
	case kind == chroma.KeywordType, kind == chroma.NameBuiltin, kind == chroma.NameClass:
		return t.Type
	case kind == chroma.KeywordConstant:
		return t.Number
	case kind.InCategory(chroma.Keyword):
		return t.Keyword
	case kind == chroma.NameFunction, kind == chroma.NameFunctionMagic:
		return t.Function
	case kind == chroma.NameTag:
		return t.Punctuation
	case kind == chroma.NameAttribute, kind.InSubCategory(chroma.NameVariable):
		return t.Variable
	case kind.InSubCategory(chroma.LiteralString):
		return t.String
	case kind.InSubCategory(chroma.LiteralNumber):
		return t.Number
	case kind.InCategory(chroma.Comment):
		return t.Comment
	case kind.InCategory(chroma.Operator):
		return t.Operator
	case kind.InCategory(chroma.Punctuation):
		return t.Punctuation
	}
	return t.Text
}

// HighlightCode returns code split into ANSI-styled lines, choosing the lexer
// by language name, alias or file name. It returns nil when no lexer matches,
// so callers keep their plain code style rather than guessing a language.
func HighlightCode(code, lang string, theme SyntaxTheme) []string {
	lexer := lexers.Get(lang)
	if lang == "" || lexer == nil {
		return nil
	}
	iterator, err := chroma.Coalesce(lexer).Tokenise(nil, code)
	if err != nil {
		return nil
	}
	lines := []string{""}
	for _, token := range iterator.Tokens() {
		for i, part := range strings.Split(token.Value, "\n") {
			if i > 0 {
				lines = append(lines, "")
			}
			if part != "" {
				lines[len(lines)-1] += markdownStyle(part, theme.style(token.Type))
			}
		}
	}
	// Lexers end input with a newline the caller didn't write.
	if len(lines) > 1 && lines[len(lines)-1] == "" && !strings.HasSuffix(code, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
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
