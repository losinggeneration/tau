package tui

import (
	"strings"
	"testing"

	"github.com/losinggeneration/tau/tui/ansi"
)

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Markdown golden tests: verify rendered output matches expected behaviors
// from the Pi markdown component spec.

func TestGoldenMarkdownEmptyRendersNoRows(t *testing.T) {
	m := NewMarkdown("")
	if lines := m.Render(80); len(lines) != 0 {
		t.Fatalf("empty markdown = %d rows, want 0", len(lines))
	}
}

func TestGoldenMarkdownWhitespaceOnlyRendersNoRows(t *testing.T) {
	m := NewMarkdown("   \n\t  \n  ")
	if lines := m.Render(80); len(lines) != 0 {
		t.Fatalf("whitespace-only markdown = %d rows, want 0", len(lines))
	}
}

func TestGoldenMarkdownTabsBecomeThreeSpaces(t *testing.T) {
	m := NewMarkdown("hello\tworld")
	lines := m.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	// Tab should be three spaces in text rendering
	if !contains(lines[0], "hello   world") {
		t.Fatalf("tab not expanded to 3 spaces: %q", lines[0])
	}
}

func TestGoldenMarkdownHeadingRenders(t *testing.T) {
	m := NewMarkdown("# Title")
	lines := m.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	// Heading should contain the title text
	found := false
	for _, line := range lines {
		if contains(line, "Title") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("heading text not found in output: %v", lines)
	}
}

func TestGoldenMarkdownCodeBlockRenders(t *testing.T) {
	m := NewMarkdown("```go\nfmt.Println(\"hello\")\n```")
	lines := m.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	// Code block should contain the code
	found := false
	for _, line := range lines {
		if contains(line, "Println") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("code content not found: %v", lines)
	}
}

func TestGoldenMarkdownInlineCodeRenders(t *testing.T) {
	m := NewMarkdown("Use `code` here")
	lines := m.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	found := false
	for _, line := range lines {
		if contains(line, "code") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("inline code not found: %v", lines)
	}
}

func TestGoldenMarkdownUnorderedListRenders(t *testing.T) {
	m := NewMarkdown("- item one\n- item two\n- item three")
	lines := m.Render(80)
	if len(lines) < 3 {
		t.Fatalf("expected >= 3 list rows, got %d", len(lines))
	}
}

func TestGoldenMarkdownOrderedListRenders(t *testing.T) {
	m := NewMarkdown("1. first\n2. second\n3. third")
	lines := m.Render(80)
	if len(lines) < 3 {
		t.Fatalf("expected >= 3 list rows, got %d", len(lines))
	}
}

func TestGoldenMarkdownBoldRenders(t *testing.T) {
	m := NewMarkdown("**bold text**")
	lines := m.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	found := false
	for _, line := range lines {
		if contains(line, "bold text") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("bold text not found: %v", lines)
	}
}

func TestGoldenMarkdownItalicRenders(t *testing.T) {
	m := NewMarkdown("*italic text*")
	lines := m.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	found := false
	for _, line := range lines {
		if contains(line, "italic text") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("italic text not found: %v", lines)
	}
}

func TestGoldenMarkdownStrikethroughRenders(t *testing.T) {
	m := NewMarkdown("~~struck text~~")
	lines := m.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	found := false
	for _, line := range lines {
		if contains(line, "struck text") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("strikethrough text not found: %v", lines)
	}
}

func TestGoldenMarkdownStrictStrikethroughNoSpaceAtStart(t *testing.T) {
	// ~~ text~~ should NOT parse as strikethrough (inner text starts with space)
	m := NewMarkdown("~~ text~~")
	lines := m.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	// The literal ~~ should appear in the output
	found := false
	for _, line := range lines {
		if contains(line, "~~") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("strict strikethrough should not parse ~~ text~~: %v", lines)
	}
}

func TestGoldenMarkdownBlockquoteRenders(t *testing.T) {
	m := NewMarkdown("> This is a quote")
	lines := m.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	found := false
	for _, line := range lines {
		if contains(line, "This is a quote") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("blockquote text not found: %v", lines)
	}
}

func TestGoldenMarkdownHorizontalRuleRenders(t *testing.T) {
	m := NewMarkdown("---")
	lines := m.Render(80)
	// Horizontal rule should produce at least one row
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
}

func TestGoldenMarkdownLinkRenders(t *testing.T) {
	m := NewMarkdown("[example](https://example.com)")
	lines := m.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	found := false
	for _, line := range lines {
		if contains(line, "example") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("link text not found: %v", lines)
	}
}

func TestGoldenMarkdownTableRenders(t *testing.T) {
	m := NewMarkdown("| A | B |\n|---|---|\n| 1 | 2 |")
	lines := m.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	// Table should contain cell content
	found := false
	for _, line := range lines {
		if contains(line, "A") && contains(line, "B") {
			found = true
			break
		}
	}
	if !found {
		// Column headers might be normalized; check for "1" and "2"
		for _, line := range lines {
			if contains(line, "1") && contains(line, "2") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("table content not found: %v", lines)
	}
}

func TestGoldenMarkdownPadsToFullWidth(t *testing.T) {
	m := NewMarkdown("hello")
	lines := m.Render(40)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	for i, line := range lines {
		if w := TextWidth(line); w != 40 {
			t.Fatalf("line %d width = %d, want 40 (full width)", i, w)
		}
	}
}

func TestGoldenMarkdownCachesByWidth(t *testing.T) {
	m := NewMarkdown("hello world")
	first := m.Render(60)
	second := m.Render(60)
	if len(first) != len(second) {
		t.Fatalf("cache mismatch: %d vs %d rows", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("line %d differs", i)
		}
	}
}

func TestGoldenMarkdownBareURLAutolinks(t *testing.T) {
	m := NewMarkdown("Visit https://example.com today")
	lines := m.Render(80)
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	found := false
	for _, line := range lines {
		if contains(line, "https://example.com") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("bare URL not autolinked: %v", lines)
	}
}

func TestGoldenMarkdownNestedListRenders(t *testing.T) {
	m := NewMarkdown("- outer\n  - inner\n- back")
	lines := m.Render(80)
	if len(lines) < 3 {
		t.Fatalf("expected >= 3 nested list rows, got %d", len(lines))
	}
}

func TestGoldenMarkdownMultipleParagraphsRenders(t *testing.T) {
	m := NewMarkdown("First paragraph.\n\nSecond paragraph.")
	lines := m.Render(80)
	if len(lines) < 2 {
		t.Fatalf("expected >= 2 paragraph rows, got %d", len(lines))
	}
}

func TestMarkdownHyperlinksCapabilityUsesOSC8(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Hyperlinks: true})
	defer ResetTerminalCapabilities()
	md := NewMarkdown("[click here](https://example.com)")
	joined := strings.Join(md.Render(80), "")
	if !strings.Contains(joined, "\x1b]8;;https://example.com\x1b\\click here\x1b]8;;\x1b\\") {
		t.Fatalf("missing OSC8 hyperlink: %q", joined)
	}
	if strings.Contains(joined, "(https://example.com)") {
		t.Fatalf("url duplicated in OSC8 mode: %q", joined)
	}
}

func TestMarkdownBareURLAndEmailNotDuplicated(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Hyperlinks: false})
	defer ResetTerminalCapabilities()
	md := NewMarkdown("Visit https://example.com or email user@example.com")
	plain := strings.Join(trimRows(md.Render(120)), " ")
	if count := strings.Count(plain, "https://example.com"); count != 1 {
		t.Fatalf("url count=%d output=%q", count, plain)
	}
	if strings.Contains(plain, "mailto:") {
		t.Fatalf("bare email rendered mailto: %q", plain)
	}
	if count := strings.Count(plain, "user@example.com"); count != 1 {
		t.Fatalf("email count=%d output=%q", count, plain)
	}
}

func TestMarkdownNestedListsPreserveMarkers(t *testing.T) {
	md := NewMarkdown("- Item 1\n  - Nested\n1. First\n   1. Inner")
	lines := trimRows(md.Render(80))
	assertContainsLine(t, lines, "- Item 1")
	assertContainsLine(t, lines, "  - Nested")
	assertContainsLine(t, lines, "1. First")
	assertContainsLine(t, lines, "   1. Inner")
}

func TestMarkdownTableRendersBordersAndRows(t *testing.T) {
	md := NewMarkdown("| Name | Age |\n| --- | --- |\n| Alice | 30 |\n| Bob | 25 |")
	lines := trimRows(md.Render(80))
	assertContainsSubstring(t, lines, "│ Name")
	assertContainsSubstring(t, lines, "Age │")
	assertContainsSubstring(t, lines, "├")
	assertContainsSubstring(t, lines, "┼")
	assertContainsSubstring(t, lines, "Alice")
	assertContainsSubstring(t, lines, "Bob")
	if lines[len(lines)-1] == "" {
		t.Fatalf("table ended with blank line: %q", lines)
	}
}

func TestMarkdownSpacingAfterBlocks(t *testing.T) {
	md := NewMarkdown("# Hello\n\nparagraph\n\n---\n\nnext\n\n```js\ncode\n```\n\nend")
	lines := stripRows(trimRows(md.Render(80)))
	want := []string{"Hello", "═════", "", "paragraph", "", "────────────────────────────────────────────────────────────────────────────────", "", "next", "", "```", "  code", "```", "", "end"}
	if len(lines) != len(want) {
		t.Fatalf("len=%d want %d lines=%q", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d=%q want %q all=%q", i, lines[i], want[i], lines)
		}
	}
}

func TestMarkdownBlockquoteWrapsWithBorder(t *testing.T) {
	md := NewMarkdown("> This is a very long blockquote line that should wrap to multiple lines")
	lines := trimRows(md.Render(30))
	count := 0
	for _, line := range lines {
		if line == "" {
			continue
		}
		count++
		if !strings.HasPrefix(line, "│ ") {
			t.Fatalf("blockquote line missing border: %q all=%q", line, lines)
		}
		if TextWidth(line) > 30 {
			t.Fatalf("blockquote line width=%d >30: %q", TextWidth(line), line)
		}
	}
	if count < 2 {
		t.Fatalf("expected wrapped blockquote, got %q", lines)
	}
}

func TestMarkdownInlineFormattingAndLinks(t *testing.T) {
	md := NewMarkdown("Use ~~old~~ **bold** `code` [site](https://example.com) and email user@example.com")
	joined := strings.Join(trimRows(md.Render(120)), " ")
	for _, want := range []string{"old", "bold", "code", "site (https://example.com)", "user@example.com"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("output %q missing %q", joined, want)
		}
	}
	for _, bad := range []string{"~~old~~", "**bold**", "`code`"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("output %q still contains delimiter %q", joined, bad)
		}
	}
}

func trimRows(rows []string) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = strings.TrimRight(row, " ")
	}
	return out
}

func stripRows(rows []string) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = ansi.Strip(row)
	}
	return out
}

func assertContainsLine(t *testing.T, lines []string, want string) {
	t.Helper()
	for _, line := range lines {
		if line == want {
			return
		}
	}
	t.Fatalf("missing line %q in %q", want, lines)
}

func assertContainsSubstring(t *testing.T, lines []string, want string) {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, want) {
			return
		}
	}
	t.Fatalf("missing substring %q in %q", want, lines)
}

func TestMarkdownStyledHeadingsInlineCodeAndStrictStrikethrough(t *testing.T) {
	md := NewMarkdown("# Title\nUse `code`, **bold**, *italic*, __under__, ~~gone~~, ~~ no ~~")
	joined := strings.Join(md.Render(120), "\n")
	for _, want := range []string{"\x1b[1m\x1b[36mTitle\x1b[0m", "\x1b[36m═════\x1b[0m", "\x1b[7mcode\x1b[0m", "\x1b[1mbold\x1b[0m", "\x1b[3mitalic\x1b[0m", "\x1b[4munder\x1b[0m", "\x1b[9mgone\x1b[0m", "~~ no ~~"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("output %q missing %q", joined, want)
		}
	}
}

func TestMarkdownFencedCodeAndLazyBlockquoteStyling(t *testing.T) {
	md := NewMarkdown("```go\nfmt.Println(1)\n```\n\n> quoted\nlazy continuation")
	lines := trimRows(md.Render(80))
	if !strings.Contains(lines[0], "\x1b[7m```") || !strings.Contains(lines[1], "\x1b[7m  fmt.Println(1)") {
		t.Fatalf("code block not styled: %q", lines)
	}
	assertContainsLine(t, lines, "│ quoted")
	assertContainsLine(t, lines, "│ lazy continuation")
}

func TestMarkdownOrderedListsAndANSIWidthPadding(t *testing.T) {
	md := NewMarkdown("1. **First**\n   2. `Second`")
	lines := md.Render(20)
	if len(lines) != 2 {
		t.Fatalf("lines=%q", lines)
	}
	for _, line := range lines {
		if TextWidth(line) != 20 {
			t.Fatalf("visible width=%d want 20 line=%q", TextWidth(line), line)
		}
	}
	plain := strings.Join(trimRows(lines), "\n")
	if !strings.Contains(plain, "1. \x1b[1mFirst\x1b[0m") || !strings.Contains(plain, "   2. \x1b[7mSecond\x1b[0m") {
		t.Fatalf("ordered list markers/styles missing: %q", plain)
	}
}

func TestMarkdownImageProtocolLinesSurviveWrapping(t *testing.T) {
	seq := "\x1b_Ga=T,f=100;abcdef\x1b\\"
	got := NewMarkdown("before\n" + seq + "\nafter").Render(6)
	if len(got) != 3 {
		t.Fatalf("rows=%#v", got)
	}
	if got[1] != seq {
		t.Fatalf("image row changed: %q want %q", got[1], seq)
	}
}

func TestMarkdownTableChromeEnclosesTable(t *testing.T) {
	src := "| a | b |\n|---|---|\n| 1 | 2 |"

	plain := trimRows(NewMarkdown(src).Render(80))
	joined := strings.Join(plain, "\n")
	if strings.Contains(joined, "╭") || strings.Contains(joined, "╰") {
		t.Fatalf("table chrome rendered without opting in: %q", plain)
	}

	md := NewMarkdown(src)
	md.SetTableChrome(true)
	chrome := trimRows(md.Render(80))
	if len(chrome) < 5 {
		t.Fatalf("expected top rule, header, divider, row, bottom rule; got %q", chrome)
	}
	if want := "╭───┬───╮"; chrome[0] != want {
		t.Fatalf("top rule = %q, want %q (all: %q)", chrome[0], want, chrome)
	}
	if want := "╰───┴───╯"; chrome[len(chrome)-1] != want {
		t.Fatalf("bottom rule = %q, want %q (all: %q)", chrome[len(chrome)-1], want, chrome)
	}
	if !contains(chrome[2], "┼") {
		t.Fatalf("header divider missing from %q", chrome)
	}
}

func TestMarkdownTableChromeMinSizeIncludesRules(t *testing.T) {
	src := "| a |\n|---|\n| 1 |"
	plain := NewMarkdown(src)
	plainSize := plain.MinSize()
	chrome := NewMarkdown(src)
	chrome.SetTableChrome(true)
	if got := chrome.MinSize().H; got != plainSize.H+2 {
		t.Fatalf("chrome height = %d, want %d (plain %d)", got, plainSize.H+2, plainSize.H)
	}
}

func TestMarkdownHeaderOnlyTableHasNoDanglingDivider(t *testing.T) {
	// A table with a delimiter row but no body rows (mid-stream) must not end
	// on a divider with nothing after it.
	plain := trimRows(NewMarkdown("| wip |\n|---|").Render(40))
	if len(plain) != 1 || !contains(plain[0], "wip") {
		t.Fatalf("plain header-only table = %q", plain)
	}
	if strings.Contains(strings.Join(plain, "\n"), "├") {
		t.Fatalf("header-only table ended with a dangling divider: %q", plain)
	}

	md := NewMarkdown("| wip |\n|---|")
	md.SetTableChrome(true)
	chrome := trimRows(md.Render(40))
	if len(chrome) != 3 || !strings.Contains(chrome[0], "╭") || !strings.Contains(chrome[2], "╰") {
		t.Fatalf("chrome header-only table = %q", chrome)
	}
	if strings.Contains(strings.Join(chrome, "\n"), "┼") {
		t.Fatalf("chrome header-only table kept the body divider: %q", chrome)
	}
}

func TestMarkdownTableWrapsCellsToFitWidth(t *testing.T) {
	src := "| Category | Count | Words |\n| --- | --- | --- |\n" +
		"| short | 54 | alpha beta gamma delta epsilon zeta eta theta iota kappa |"
	lines := trimRows(NewMarkdown(src).Render(30))
	rows := 0
	for _, line := range lines {
		if !strings.Contains(line, "│") {
			continue
		}
		rows++
		if w := TextWidth(line); w > 30 {
			t.Fatalf("table row width=%d >30: %q all=%q", w, line, lines)
		}
	}
	if rows < 4 {
		t.Fatalf("expected wrapped body rows, got %q", lines)
	}
	// Narrow columns keep their words intact; every word of the wide cell
	// survives wrapping.
	assertContainsSubstring(t, lines, "Category")
	assertContainsSubstring(t, lines, "Count")
	for _, word := range []string{"alpha", "kappa"} {
		assertContainsSubstring(t, lines, word)
	}
}

func TestMarkdownTableWrapsLongHeader(t *testing.T) {
	src := "| averylongheader that wraps | b |\n| --- | --- |\n| x | y |"
	lines := trimRows(NewMarkdown(src).Render(24))
	for _, line := range lines {
		if w := TextWidth(line); w > 24 {
			t.Fatalf("table row width=%d >24: %q all=%q", w, line, lines)
		}
	}
	assertContainsSubstring(t, lines, "averylongheader")
	assertContainsSubstring(t, lines, "that wraps")
}

func TestMarkdownTableChromeWrapsToFitWidth(t *testing.T) {
	src := "| Name | Value |\n|---|---|\n| k | one two three four five six seven eight nine ten |"
	md := NewMarkdown(src)
	md.SetTableChrome(true)
	lines := trimRows(md.Render(24))
	if !strings.Contains(lines[0], "╭") || !strings.Contains(lines[len(lines)-1], "╰") {
		t.Fatalf("chrome rules missing: %q", lines)
	}
	for _, line := range lines {
		if w := TextWidth(line); w > 24 {
			t.Fatalf("chrome row width=%d >24: %q all=%q", w, line, lines)
		}
	}
	assertContainsSubstring(t, lines, "ten")
}

func TestFitTableWidths(t *testing.T) {
	rows := [][]string{{"aaaa bbbb", "cc"}}
	natural := []int{9, 2}
	if got := fitTableWidths(rows, natural, 100); got[0] != 9 || got[1] != 2 {
		t.Fatalf("fitting table changed widths: %v", got)
	}
	// budget 7: mins [4,2] leave 1 cell of slack, going to the wider deficit.
	if got := fitTableWidths(rows, natural, 14); got[0] != 5 || got[1] != 2 {
		t.Fatalf("constrained widths = %v, want [5 2]", got)
	}
	// Below the longest-word minimums: scale proportionally, floored at 1.
	if got := fitTableWidths(rows, natural, 9); got[0] != 1 || got[1] != 1 {
		t.Fatalf("tiny widths = %v, want [1 1]", got)
	}
	if got := fitTableWidths(rows, natural, 0); got[0] != 9 || got[1] != 2 {
		t.Fatalf("zero avail changed widths: %v", got)
	}
}

func TestWrapCellPrefersWordBoundaries(t *testing.T) {
	got := wrapCell("alpha beta gamma", 10)
	if len(got) != 2 || got[0] != "alpha beta" || got[1] != "gamma" {
		t.Fatalf("word wrap = %q", got)
	}
	if got := wrapCell("abcdefghij", 4); len(got) != 3 || got[0] != "abcd" || got[2] != "ij" {
		t.Fatalf("hard wrap = %q", got)
	}
	got = wrapCell("\x1b[7malpha beta\x1b[0m", 6)
	if len(got) != 2 || got[0] != "\x1b[7malpha\x1b[0m" || got[1] != "\x1b[7mbeta\x1b[0m" {
		t.Fatalf("styled wrap = %q", got)
	}
}
