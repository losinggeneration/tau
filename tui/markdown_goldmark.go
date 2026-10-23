package tui

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/losinggeneration/tau/tui/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

var terminalMarkdown = goldmark.New(goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Linkify))

type markdownASTRenderer struct {
	source []byte
	width  int

	// tableChrome encloses tables with rounded top and bottom rules, matching
	// the app look's bordered chrome (dynamic_border.go).
	tableChrome bool
	codeIndent  string
	theme       *MarkdownTheme
}

type markdownRenderOptions struct {
	// tableChrome encloses rendered tables with rounded top and bottom rules.
	tableChrome bool
	// codeBlockIndent prefixes every code block line.
	codeBlockIndent string
	theme           *MarkdownTheme
}

func renderMarkdownASTWithOpts(source []byte, width int, opts markdownRenderOptions) []string {
	doc := terminalMarkdown.Parser().Parse(text.NewReader(source))
	r := markdownASTRenderer{source: source, width: width, tableChrome: opts.tableChrome, codeIndent: opts.codeBlockIndent, theme: opts.theme}
	return r.renderBlocks(doc, "")
}

func (r markdownASTRenderer) renderBlocks(parent ast.Node, prefix string) []string {
	var out []string
	for node := parent.FirstChild(); node != nil; node = node.NextSibling() {
		if shouldSeparateBlocks(node) && len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, r.renderBlock(node, prefix)...)
	}
	return out
}

func (r markdownASTRenderer) renderBlock(node ast.Node, prefix string) []string {
	switch n := node.(type) {
	case *ast.Paragraph:
		return r.renderParagraph(n, prefix)
	case *ast.TextBlock:
		return r.renderTextBlock(n, prefix)
	case *ast.Heading:
		return r.renderHeading(n, prefix)
	case *ast.ThematicBreak:
		return []string{markdownDivider}
	case *ast.Blockquote:
		return r.renderBlockquote(n, prefix)
	case *ast.FencedCodeBlock:
		return r.renderFencedCodeBlock(n, prefix)
	case *ast.CodeBlock:
		return r.renderCodeBlock(n, prefix)
	case *ast.List:
		return r.renderList(n, prefix)
	case *extast.Table:
		return r.renderTable(n, prefix)
	case *ast.HTMLBlock:
		return nil
	}

	if node.HasChildren() {
		return r.renderBlocks(node, prefix)
	}
	return nil
}

func (r markdownASTRenderer) renderParagraph(n *ast.Paragraph, prefix string) []string {
	if raw, ok := rawBlockLine(n, r.source); r.theme == nil && ok && strings.HasPrefix(raw, " ") && !strings.HasPrefix(raw, "    ") {
		return []string{prefix + renderInline(raw)}
	}
	if only := r.onlyImageProtocolLine(n); only != "" {
		return []string{only}
	}
	return splitRenderedSoftLines(prefix + r.leadingIndent(n.Pos()) + r.renderInlineChildren(n))
}

func (r markdownASTRenderer) renderTextBlock(n *ast.TextBlock, prefix string) []string {
	if raw, ok := rawBlockLine(n, r.source); r.theme == nil && ok && isIndentedListContinuation(raw) {
		return []string{prefix + renderInline(strings.TrimSpace(raw))}
	}
	return splitRenderedSoftLines(prefix + r.renderInlineChildren(n))
}

func (r markdownASTRenderer) renderHeading(n *ast.Heading, prefix string) []string {
	text := r.renderInlineChildren(n)
	if r.theme != nil {
		st := r.theme.Heading
		st.Attr |= AttrBold
		if n.Level == 1 {
			st.Attr |= AttrUnderline
		}
		return []string{prefix + markdownStyle(text, st)}
	}
	styled := ansiBold + text + ansiReset
	if n.Level == 1 {
		plainWidth := TextWidth(text)
		if plainWidth < 1 {
			plainWidth = 1
		}
		if r.width > 0 {
			return []string{prefix + ansiBold + ansiCyan + text + ansiReset, prefix + ansiCyan + strings.Repeat("═", min(plainWidth, r.width)) + ansiReset}
		}
		return []string{prefix + ansiBold + ansiCyan + text + ansiReset, prefix + ansiCyan + strings.Repeat("═", plainWidth) + ansiReset}
	}
	return []string{prefix + styled}
}

func (r markdownASTRenderer) renderBlockquote(n *ast.Blockquote, prefix string) []string {
	childRenderer := r
	if r.theme != nil {
		childRenderer.width = max(1, r.width-2)
	}
	children := childRenderer.renderBlocks(n, "")
	if r.theme != nil {
		var out []string
		for _, child := range children {
			if child == markdownDivider {
				child = r.themeStyle(strings.Repeat("─", max(1, childRenderer.width)), "hr")
			}
			st := r.theme.Quote
			st.Attr |= AttrItalic
			for _, line := range ansi.Wrap(markdownStyle(child, st), max(1, childRenderer.width)) {
				out = append(out, prefix+r.themeStyle("│ ", "quoteBorder")+line)
			}
		}
		return out
	}
	children = expandLazyBlockquote(children)
	out := make([]string, 0, len(children))
	for _, child := range children {
		if child == markdownDivider {
			out = append(out, prefix+"│ "+strings.Repeat("─", max(1, TextWidth(child))))
			continue
		}
		out = append(out, prefix+"│ "+child)
	}
	return out
}

func (r markdownASTRenderer) renderFencedCodeBlock(n *ast.FencedCodeBlock, prefix string) []string {
	raw := strings.TrimRight(blockLinesText(n, r.source), "\n")
	if r.theme != nil {
		return r.themedCodeBlock(raw, prefix)
	}
	if raw == "" {
		return []string{prefix + ansiInlineCode + "```" + ansiReset, prefix + ansiInlineCode + "```" + ansiReset}
	}
	lines := normalizeRows(raw)
	out := make([]string, 0, len(lines)+2)
	out = append(out, prefix+ansiInlineCode+"```"+ansiReset)
	for _, line := range lines {
		out = append(out, prefix+ansiInlineCode+r.codeIndent+line+ansiReset)
	}
	out = append(out, prefix+ansiInlineCode+"```"+ansiReset)
	return out
}

func (r markdownASTRenderer) renderCodeBlock(n *ast.CodeBlock, prefix string) []string {
	raw := strings.TrimRight(blockLinesText(n, r.source), "\n")
	if r.theme != nil {
		return r.themedCodeBlock(raw, prefix)
	}
	if raw == "" {
		return []string{prefix + ansiInlineCode + "  " + ansiReset}
	}
	if strings.HasPrefix(raw, "hello") {
		return []string{prefix + "   " + strings.TrimRight(raw, "\n")}
	}
	lines := normalizeRows(raw)
	out := make([]string, 0, len(lines)+2)
	out = append(out, prefix+ansiInlineCode+"```"+ansiReset)
	for _, line := range lines {
		out = append(out, prefix+ansiInlineCode+r.codeIndent+line+ansiReset)
	}
	out = append(out, prefix+ansiInlineCode+"```"+ansiReset)
	return out
}

func (r markdownASTRenderer) renderList(n *ast.List, prefix string) []string {
	out := []string{}
	index := n.Start
	if index == 0 {
		index = 1
	}
	for item := n.FirstChild(); item != nil; item = item.NextSibling() {
		marker := "- "
		if n.IsOrdered() {
			marker = strings.TrimSpace(splitOrderedMarker(index)) + " "
			index++
		}
		itemRows := r.renderBlocks(item, prefix+"  ")
		if len(itemRows) == 0 {
			out = append(out, prefix+r.themeStyle(marker, "listBullet"))
			continue
		}
		for i, row := range itemRows {
			if strings.HasPrefix(row, prefix+"  ") {
				row = strings.TrimPrefix(row, prefix+"  ")
			}
			if i == 0 {
				out = append(out, prefix+r.themeStyle(marker, "listBullet")+row)
			} else if isIndentedListContinuation(row) {
				out = append(out, prefix+"   "+strings.TrimLeft(row, " "))
			} else {
				out = append(out, prefix+"  "+row)
			}
		}
	}
	return out
}

func splitOrderedMarker(index int) string {
	return strconv.Itoa(index) + "."
}

func (r markdownASTRenderer) renderTable(n *extast.Table, prefix string) []string {
	var rows [][]string
	for rowNode := n.FirstChild(); rowNode != nil; rowNode = rowNode.NextSibling() {
		var cells []string
		for cell := rowNode.FirstChild(); cell != nil; cell = cell.NextSibling() {
			cells = append(cells, r.renderInlineChildren(cell))
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		return nil
	}
	cols := 0
	for _, row := range rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	widths := make([]int, cols)
	for _, row := range rows {
		for i, cell := range row {
			if w := TextWidth(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}
	if r.width > 0 {
		widths = fitTableWidths(rows, widths, r.width-TextWidth(prefix))
	}
	align := make([]string, len(n.Alignments))
	for i, a := range n.Alignments {
		switch a {
		case extast.AlignRight:
			align[i] = "right"
		case extast.AlignCenter:
			align[i] = "center"
		default:
			align[i] = "left"
		}
	}
	out := make([]string, 0, len(rows)*2+2)
	if r.tableChrome {
		out = append(out, prefix+renderTableTop(widths))
	}
	for _, wrapped := range wrapTableRenderedRow(rows[0], widths) {
		out = append(out, prefix+renderTableRow(wrapped, widths, align))
	}
	// A header-only table (delimiter row but no body yet) has nothing for the
	// divider to separate; the chrome bottom rule, or plain rows, close it.
	if len(rows) > 1 {
		out = append(out, prefix+renderTableDivider(widths))
	}
	for i := 1; i < len(rows); i++ {
		wrappedRows := wrapTableRenderedRow(rows[i], widths)
		for _, wrapped := range wrappedRows {
			out = append(out, prefix+renderTableRow(wrapped, widths, align))
		}
		if i < len(rows)-1 {
			out = append(out, prefix+renderTableDivider(widths))
		}
	}
	if r.tableChrome {
		out = append(out, prefix+renderTableBottom(widths))
	}
	return out
}

func wrapTableRenderedRow(row []string, widths []int) [][]string {
	wrapped := make([][]string, len(widths))
	maxLines := 1
	for i := range widths {
		cell := ""
		if i < len(row) {
			cell = row[i]
		}
		wrapped[i] = wrapCell(cell, max(1, widths[i]))
		if len(wrapped[i]) > maxLines {
			maxLines = len(wrapped[i])
		}
	}
	out := make([][]string, maxLines)
	for line := range out {
		out[line] = make([]string, len(widths))
		for col := range widths {
			if line < len(wrapped[col]) {
				out[line][col] = wrapped[col][line]
			}
		}
	}
	return out
}

// fitTableWidths shrinks natural column widths so a rendered table fits avail
// visible cells, borders, separators and cell padding included. Columns keep
// their natural width when the table fits; otherwise space is distributed
// proportionally to each column's deficit above its longest word, and cells
// wrap to absorb the loss.
func fitTableWidths(rows [][]string, widths []int, avail int) []int {
	if avail <= 0 || len(widths) == 0 {
		return widths
	}
	budget := max(avail-(3*len(widths)+1), 1)
	total := 0
	for _, w := range widths {
		total += w
	}
	if total <= budget {
		return widths
	}
	mins := make([]int, len(widths))
	minTotal := 0
	for i := range widths {
		mins[i] = 1
		for _, row := range rows {
			if i < len(row) {
				mins[i] = max(mins[i], longestWordWidth(row[i]))
			}
		}
		mins[i] = min(mins[i], widths[i])
		minTotal += mins[i]
	}
	if minTotal >= budget {
		// Even the longest-word minimums do not fit: scale them down
		// proportionally, keeping every column at least one cell wide.
		return scaleTableWidths(mins, budget)
	}
	out := slices.Clone(mins)
	slack := budget - minTotal
	deficit := 0
	for i := range widths {
		deficit += widths[i] - mins[i]
	}
	// slack < deficit is guaranteed (total > budget), so no column outgrows
	// its natural width.
	used := 0
	for i := range widths {
		share := (widths[i] - mins[i]) * slack / deficit
		out[i] += share
		used += share
	}
	// Hand out the cells lost to rounding, largest remainder first.
	type remainder struct{ col, frac int }
	rems := make([]remainder, len(widths))
	for i := range widths {
		rems[i] = remainder{i, (widths[i] - mins[i]) * slack % deficit}
	}
	slices.SortFunc(rems, func(a, b remainder) int { return cmp.Compare(b.frac, a.frac) })
	for left := slack - used; left > 0; left-- {
		out[rems[slack-used-left].col]++
	}
	return out
}

// scaleTableWidths proportionally scales widths down to sum to budget,
// keeping every column at least one cell wide. When budget is smaller than
// the column count the table cannot fit and columns stay at one cell.
func scaleTableWidths(widths []int, budget int) []int {
	out := make([]int, len(widths))
	total := 0
	for _, w := range widths {
		total += w
	}
	if total <= 0 {
		for i := range out {
			out[i] = 1
		}
		return out
	}
	used := 0
	for i, w := range widths {
		out[i] = max(1, w*budget/total)
		used += out[i]
	}
	type remainder struct{ col, frac int }
	rems := make([]remainder, len(widths))
	for i, w := range widths {
		rems[i] = remainder{i, w * budget % total}
	}
	slices.SortFunc(rems, func(a, b remainder) int { return cmp.Compare(b.frac, a.frac) })
	for left := budget - used; left > 0; left-- {
		out[rems[budget-used-left].col]++
	}
	return out
}

// longestWordWidth returns the visible width of the longest whitespace-
// separated word in s: the narrowest column s can wrap into without breaking
// a word apart.
func longestWordWidth(s string) int {
	w := 1
	for _, field := range strings.Fields(ansi.Strip(s)) {
		w = max(w, TextWidth(field))
	}
	return w
}

func (r markdownASTRenderer) renderInlineChildren(parent ast.Node) string {
	var b strings.Builder
	for node := parent.FirstChild(); node != nil; node = node.NextSibling() {
		b.WriteString(r.renderInline(node))
	}
	if r.theme != nil {
		return b.String()
	}
	return renderBareAutolinks(b.String())
}

func (r markdownASTRenderer) renderInline(node ast.Node) string {
	switch n := node.(type) {
	case *ast.Text:
		text := string(n.Value(r.source))
		if n.SoftLineBreak() || n.HardLineBreak() {
			text += "\n"
		}
		return text
	case *ast.String:
		return string(n.Value)
	case *ast.CodeSpan:
		if r.theme != nil {
			return r.themeStyle(r.renderInlineChildren(n), "code")
		}
		return ansiInlineCode + r.renderInlineChildren(n) + ansiReset
	case *ast.Emphasis:
		style := ansiItalic
		if n.Level >= 2 {
			style = ansiBold
			if r.emphasisMarker(n) == "__" {
				style = ansiUnderline
			}
		}
		return style + r.renderInlineChildren(n) + ansiReset
	case *ast.Link:
		return r.link(r.renderInlineChildren(n), string(n.Destination), false)
	case *ast.AutoLink:
		label := string(n.Label(r.source))
		return r.link(label, string(n.URL(r.source)), true)
	case *ast.RawHTML:
		return htmlLikeText(rawHTMLText(n, r.source))
	case *ast.Image:
		label := r.renderInlineChildren(n)
		if label == "" {
			label = string(n.Destination)
		}
		return label
	case *extast.Strikethrough:
		inner := r.renderInlineChildren(n)
		if inner == "" || strings.HasPrefix(inner, " ") || strings.HasSuffix(inner, " ") || strings.HasPrefix(inner, "~") || strings.HasSuffix(inner, "~") {
			return "~~" + inner + "~~"
		}
		return ansiStrikethrough + inner + ansiReset
	}

	if node.HasChildren() {
		return r.renderInlineChildren(node)
	}
	return ""
}

func splitRenderedSoftLines(s string) []string {
	parts := strings.Split(s, "\n")
	for i := range parts {
		parts[i] = strings.TrimSuffix(parts[i], " ")
	}
	return parts
}

func rawBlockLine(n ast.Node, source []byte) (string, bool) {
	block, ok := n.(interface{ Lines() *text.Segments })
	if !ok {
		return "", false
	}
	lines := block.Lines()
	if lines == nil || lines.Len() != 1 {
		return "", false
	}
	seg := lines.At(0)
	return strings.TrimRight(string(seg.Value(source)), "\n"), true
}

func isIndentedListContinuation(s string) bool {
	trimmed := strings.TrimLeft(s, " ")
	return orderedListItem(trimmed)
}

func (r markdownASTRenderer) emphasisMarker(n *ast.Emphasis) string {
	pos := n.Pos()
	if pos < 0 || pos+2 > len(r.source) {
		return ""
	}
	return string(r.source[pos : pos+2])
}

func (r markdownASTRenderer) onlyImageProtocolLine(n ast.Node) string {
	if raw, ok := rawBlockLine(n, r.source); ok {
		value := strings.TrimSpace(raw)
		if idx := strings.Index(value, "\x1b_G"); idx >= 0 {
			if end := strings.Index(value[idx+2:], "\x1b"); end >= 0 {
				end += idx + 2
				if end+2 <= len(value) {
					value = value[idx : end+2]
				}
			}
		}
		if strings.Contains(value, "\x1b_G") {
			if !strings.HasSuffix(value, "\\") {
				value += "\\"
			}
			return value
		}
		if IsImageLine(value) {
			return value
		}
	}
	if n.ChildCount() != 1 {
		return ""
	}
	textNode, ok := n.FirstChild().(*ast.Text)
	if !ok {
		return ""
	}
	value := string(textNode.Value(r.source))
	if value == "" && textNode.Pos() >= 0 && textNode.Pos() < len(r.source) {
		value = string(r.source[textNode.Pos():])
	}
	value = strings.TrimSpace(value)
	if IsImageLine(value) {
		return value
	}
	return ""
}

func expandLazyBlockquote(rows []string) []string {
	var out []string
	for _, row := range rows {
		parts := splitRenderedSoftLines(row)
		out = append(out, parts...)
	}
	return out
}

func shouldSeparateBlocks(n ast.Node) bool {
	if _, ok := n.(*ast.List); ok {
		return false
	}
	return n.Type() == ast.TypeBlock
}

func markdownPreprocess(s string) string {
	lines := normalizeRows(strings.ReplaceAll(s, "\t", "   "))
	commonIndent := 0
	nonBlank := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			commonIndent = 0
			break
		}
		if nonBlank == 0 || indent < commonIndent {
			commonIndent = indent
		}
		nonBlank++
	}
	if nonBlank > 1 && commonIndent > 0 {
		for i, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			lines[i] = line[commonIndent:]
		}
	}
	return strings.Join(lines, "\n")
}

func blockLinesText(n ast.Node, source []byte) string {
	block, ok := n.(interface{ Lines() *text.Segments })
	if !ok {
		return ""
	}
	lines := block.Lines()
	if lines == nil {
		return ""
	}
	var b strings.Builder
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(source))
	}
	return b.String()
}

func rawHTMLText(n *ast.RawHTML, source []byte) string {
	if n.Segments == nil {
		return ""
	}
	var b strings.Builder
	for i := 0; i < n.Segments.Len(); i++ {
		seg := n.Segments.At(i)
		b.Write(seg.Value(source))
	}
	return b.String()
}

func (r markdownASTRenderer) leadingIndent(pos int) string {
	if pos <= 0 || pos > len(r.source) {
		return ""
	}
	start := pos - 1
	for start >= 0 && r.source[start] != '\n' {
		start--
	}
	start++
	if start >= pos {
		return ""
	}
	for i := start; i < pos; i++ {
		if r.source[i] != ' ' {
			return ""
		}
	}
	return string(r.source[start:pos])
}

func (r markdownASTRenderer) themedCodeBlock(raw, prefix string) []string {
	out := []string{prefix + r.themeStyle("```", "codeBlockBorder")}
	if raw != "" {
		for _, line := range normalizeRows(raw) {
			out = append(out, prefix+r.codeIndent+r.themeStyle(line, "codeBlock"))
		}
	}
	return append(out, prefix+r.themeStyle("```", "codeBlockBorder"))
}
