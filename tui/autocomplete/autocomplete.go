package autocomplete

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/losinggeneration/tau/tui/fuzzy"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Item is an autocomplete entry.
type Item struct {
	Value       string
	Label       string
	Description string
	Scope       string
}

// SlashCommand describes a slash command and optional argument completion hook.
type SlashCommand struct {
	Name         string
	Description  string
	ArgumentHint string
	CompleteArgs func(ctx context.Context, prefix string) ([]Item, error)
}

// Suggestions are available completions and the prefix they replace.
type Suggestions struct {
	Items  []Item
	Prefix string
}

// FocusedIndex prefers an exact value, then the first case-sensitive prefix match.
func (s *Suggestions) FocusedIndex() int {
	if s == nil || s.Prefix == "" {
		return 0
	}
	prefixIndex := -1
	for i, item := range s.Items {
		if item.Value == s.Prefix {
			return i
		}
		if prefixIndex < 0 && strings.HasPrefix(item.Value, s.Prefix) {
			prefixIndex = i
		}
	}
	return max(prefixIndex, 0)
}

// ApplyResult is the updated editor text after applying a completion.
type ApplyResult struct {
	Lines      []string
	CursorLine int
	CursorCol  int
}

// CombinedProvider handles slash commands, command arguments, file paths, and
// @file attachments. It mirrors Pi's CombinedAutocompleteProvider, using a
// synchronous filesystem fallback rather than spawning fd.
type CombinedProvider struct {
	commands   []SlashCommand
	items      []Item
	basePath   string
	fuzzyFiles FileSearcher
}

// Option configures a CombinedProvider.
type Option func(*CombinedProvider)

// FileSearcher provides production non-subprocess fuzzy file discovery.
type FileSearcher interface {
	Search(ctx context.Context, basePath, query string, limit int) ([]Item, error)
}

// WithFileSearcher sets a recursive fuzzy file search implementation.
func WithFileSearcher(searcher FileSearcher) Option {
	return func(p *CombinedProvider) {
		p.fuzzyFiles = searcher
	}
}

// WithBasePath sets the filesystem root for relative path completions.
func WithBasePath(path string) Option {
	return func(p *CombinedProvider) {
		if path != "" {
			p.basePath = path
		}
	}
}

// WithCommands sets slash command completions.
func WithCommands(commands []SlashCommand) Option {
	return func(p *CombinedProvider) {
		p.commands = append([]SlashCommand(nil), commands...)
	}
}

// WithItems sets slash commands from static autocomplete items.
func WithItems(items []Item) Option {
	return func(p *CombinedProvider) {
		p.items = append([]Item(nil), items...)
	}
}

// NewCombinedProvider creates a provider rooted at the current working directory unless overridden.
func NewCombinedProvider(opts ...Option) (*CombinedProvider, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("autocomplete: get cwd: %w", err)
	}
	p := &CombinedProvider{basePath: cwd}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// GetSuggestions returns suggestions for the current cursor position. A nil
// result means no autocomplete should be shown.
func (p *CombinedProvider) GetSuggestions(ctx context.Context, lines []string, cursorLine, cursorCol int, force bool) (suggestions *Suggestions, err error) {
	defer func() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			suggestions = nil
			err = fmt.Errorf("autocomplete: cancelled: %w", ctxErr)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("autocomplete: cancelled: %w", err)
	}
	currentLine := lineAt(lines, cursorLine)
	textBeforeCursor := sliceString(currentLine, 0, cursorCol)

	if atPrefix := extractAtPrefix(textBeforeCursor); atPrefix != "" {
		parsed := parsePathPrefix(atPrefix)
		items := p.getRecursiveFileSuggestions(ctx, parsed.RawPrefix, parsed.IsQuotedPrefix)
		if len(items) == 0 {
			return nil, nil
		}
		return &Suggestions{Items: items, Prefix: atPrefix}, nil
	}

	commandText := strings.TrimLeftFunc(textBeforeCursor, isWhitespace)
	if !force && strings.HasPrefix(commandText, "/") {
		space := strings.Index(commandText, " ")
		if space == -1 || strings.Contains(commandText[1:space], ":") {
			prefix := commandText[1:]
			items := p.commandItems()
			filtered := filterAutocompleteItems(items, prefix)
			if len(filtered) == 0 {
				return nil, nil
			}
			return &Suggestions{Items: filtered, Prefix: commandText}, nil
		}

		commandName := commandText[1:space]
		argText := commandText[space+1:]
		for _, command := range p.commands {
			if command.Name != commandName || command.CompleteArgs == nil {
				continue
			}
			items, err := command.CompleteArgs(ctx, argText)
			if err != nil {
				return nil, fmt.Errorf("autocomplete: complete %s args: %w", commandName, err)
			}
			if len(items) == 0 {
				return nil, nil
			}
			return &Suggestions{Items: append([]Item(nil), items...), Prefix: argText}, nil
		}
		return nil, nil
	}

	pathPrefix, ok := extractPathPrefix(textBeforeCursor, force)
	if !ok {
		return nil, nil
	}
	items := p.getFileSuggestions(pathPrefix)
	if len(items) == 0 {
		return nil, nil
	}
	return &Suggestions{Items: items, Prefix: pathPrefix}, nil
}

// ApplyCompletion replaces prefix at the cursor with item and returns the new text/cursor.
func ApplyCompletion(lines []string, cursorLine, cursorCol int, item Item, prefix string) ApplyResult {
	newLines := append([]string(nil), lines...)
	for len(newLines) <= cursorLine {
		newLines = append(newLines, "")
	}
	currentLine := newLines[cursorLine]
	beforePrefix := sliceString(currentLine, 0, cursorCol-len(prefix))
	afterCursor := sliceString(currentLine, cursorCol, len(currentLine))

	isQuotedPrefix := strings.HasPrefix(prefix, "\"") || strings.HasPrefix(prefix, "@\"")
	if isQuotedPrefix && strings.HasSuffix(item.Value, "\"") && strings.HasPrefix(afterCursor, "\"") {
		afterCursor = afterCursor[1:]
	}

	isSlashCommand := strings.HasPrefix(prefix, "/") && strings.TrimFunc(beforePrefix, isWhitespace) == "" && !strings.Contains(prefix[1:], "/")
	if isSlashCommand {
		newLines[cursorLine] = beforePrefix + "/" + item.Value + " " + afterCursor
		return ApplyResult{Lines: newLines, CursorLine: cursorLine, CursorCol: len(beforePrefix) + len(item.Value) + 2}
	}

	if strings.HasPrefix(prefix, "@") {
		isDir := strings.HasSuffix(item.Label, "/")
		suffix := " "
		if isDir {
			suffix = ""
		}
		newLines[cursorLine] = beforePrefix + item.Value + suffix + afterCursor
		cursorOffset := cursorOffset(item, isDir)
		return ApplyResult{Lines: newLines, CursorLine: cursorLine, CursorCol: len(beforePrefix) + cursorOffset + len(suffix)}
	}

	newLines[cursorLine] = beforePrefix + item.Value + afterCursor
	isDir := strings.HasSuffix(item.Label, "/")
	return ApplyResult{Lines: newLines, CursorLine: cursorLine, CursorCol: len(beforePrefix) + cursorOffset(item, isDir)}
}

// ShouldTriggerFileCompletion excludes a bare slash command from forced file requests.
func (p *CombinedProvider) ShouldTriggerFileCompletion(lines []string, line, col int) bool {
	return ShouldTriggerFileCompletion(lines, line, col)
}

// ShouldTriggerFileCompletion reports whether Tab should trigger file completion.
func ShouldTriggerFileCompletion(lines []string, cursorLine, cursorCol int) bool {
	text := sliceString(lineAt(lines, cursorLine), 0, cursorCol)
	trimmed := strings.TrimFunc(text, isWhitespace)
	return !strings.HasPrefix(trimmed, "/") || strings.Contains(trimmed, " ")
}

func filterAutocompleteItems(items []Item, query string) []Item {
	query = strings.TrimFunc(query, isWhitespace)
	if query == "" {
		return append([]Item(nil), items...)
	}

	if !strings.Contains(query, ":") || strings.HasPrefix(query, "skill:") {
		text := func(item Item) string { return strings.TrimPrefix(item.Value, "skill:") }
		primary := fuzzy.Filter(items, query, text)
		matched := make(map[string]bool, len(primary))
		for _, item := range primary {
			matched[item.Value] = true
		}
		var remaining []Item
		for _, item := range items {
			if strings.HasPrefix(item.Value, "skill:") && !matched[item.Value] {
				remaining = append(remaining, item)
			}
		}
		return append(primary, fuzzy.Filter(remaining, query, func(item Item) string { return item.Value })...)
	}
	tokens := strings.Fields(query)
	scoped := false
	for _, token := range tokens {
		if strings.Contains(token, ":") {
			scoped = true
			break
		}
	}
	if !scoped {
		return fuzzy.Filter(items, query, func(item Item) string { return item.Value })
	}

	out := make([]Item, 0, len(items))
	for _, item := range items {
		if matchesScopedTokens(item, tokens) {
			out = append(out, item)
		}
	}
	return out
}

func matchesScopedTokens(item Item, tokens []string) bool {
	for _, token := range tokens {
		key, value, ok := strings.Cut(token, ":")
		if !ok {
			match := fuzzy.MatchString(token, strings.Join([]string{item.Value, item.Label, item.Description, item.Scope}, " "))
			if !match.Matches {
				return false
			}
			continue
		}
		if value == "" {
			continue
		}

		var haystack string
		switch strings.ToLower(key) {
		case "value", "name", "command", "cmd":
			haystack = item.Value
		case "label":
			haystack = item.Label
		case "description", "desc":
			haystack = item.Description
		case "scope", "group":
			haystack = item.Scope
		default:
			haystack = strings.Join([]string{item.Value, item.Label, item.Description, item.Scope}, " ")
		}
		if match := fuzzy.MatchString(value, haystack); !match.Matches {
			return false
		}
	}
	return true
}

func (p *CombinedProvider) commandItems() []Item {
	items := make([]Item, 0, len(p.commands)+len(p.items))
	for _, command := range p.commands {
		desc := command.Description
		if command.ArgumentHint != "" {
			if desc != "" {
				desc = command.ArgumentHint + " — " + desc
			} else {
				desc = command.ArgumentHint
			}
		}
		items = append(items, Item{Value: command.Name, Label: command.Name, Description: desc, Scope: "commands"})
	}
	items = append(items, p.items...)
	return items
}

func (p *CombinedProvider) getFileSuggestions(prefix string) []Item {
	parsed := parsePathPrefix(prefix)
	searchDir, searchPrefix, ok := p.searchLocation(parsed.RawPrefix)
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return nil
	}
	items := make([]Item, 0, len(entries))
	lower := cases.Lower(language.Und)
	searchPrefix = lower.String(searchPrefix)
	for _, entry := range entries {
		if !strings.HasPrefix(lower.String(entry.Name()), searchPrefix) {
			continue
		}
		isDir := entry.IsDir()
		if !isDir && entry.Type()&os.ModeSymlink != 0 {
			if info, err := os.Stat(filepath.Join(searchDir, entry.Name())); err == nil {
				isDir = info.IsDir()
			}
		}
		rel := buildRelativePath(parsed.RawPrefix, entry.Name())
		rel = filepath.ToSlash(rel)
		if isDir {
			rel += "/"
		}
		items = append(items, Item{Value: buildCompletionValue(rel, parsed), Label: entry.Name() + dirSuffix(isDir)})
	}
	sortItems(items)
	return items
}

func (p *CombinedProvider) getRecursiveFileSuggestions(ctx context.Context, query string, quoted bool) []Item {
	if ctx.Err() != nil {
		return nil
	}
	base := p.basePath
	var displayBase string
	if slash := strings.LastIndexByte(query, '/'); slash >= 0 {
		display := query[:slash+1]
		candidate := p.resolvePath(p.expandHome(display))
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			base, displayBase, query = candidate, display, query[slash+1:]
		}
	}
	searcher := p.fuzzyFiles
	if searcher == nil {
		searcher = WalkFileSearcher{}
	}
	items, err := searcher.Search(ctx, base, query, 20)
	if err != nil || ctx.Err() != nil {
		return nil
	}
	out := make([]Item, 0, len(items))
	for _, item := range items {
		isDir := strings.HasSuffix(item.Label, "/") || strings.HasSuffix(item.Value, "/")
		value := item.Value
		if value == "" {
			value = item.Label
		}
		if displayBase != "" {
			value = displayBase + value
			item.Description = displayBase + strings.TrimSuffix(value[len(displayBase):], "/")
		}
		value = buildCompletionValue(value, parsedPathPrefix{IsAtPrefix: true, IsQuotedPrefix: quoted})
		if isDir && !strings.HasSuffix(value, "/") && !strings.HasSuffix(value, "/\"") {
			value += "/"
		}
		item.Value = value
		out = append(out, item)
	}
	return out
}

func (p *CombinedProvider) searchLocation(raw string) (string, string, bool) {
	expanded := p.expandHome(raw)
	root := raw == "" || raw == "./" || raw == "../" || raw == "~" || raw == "~/" || raw == "/"
	if root || strings.HasSuffix(raw, "/") {
		return p.resolvePath(expanded), "", true
	}
	dir, file := filepath.Dir(expanded), filepath.Base(expanded)
	return p.resolvePath(dir), file, true
}

func (p *CombinedProvider) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(p.basePath, path)
}

func (p *CombinedProvider) expandHome(path string) string {
	if path == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
	}
	if strings.HasPrefix(path, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, path[2:])
		}
	}
	return path
}

func scoreEntry(filePath, query string, isDir bool) int {
	lower := cases.Lower(language.Und)
	name := lower.String(filepath.Base(filePath))
	q := lower.String(query)
	full := lower.String(filePath)
	score := 0
	switch {
	case name == q:
		score = 100
	case strings.HasPrefix(name, q):
		score = 80
	case strings.Contains(name, q):
		score = 50
	case strings.Contains(full, q):
		score = 30
	}
	if isDir && score > 0 {
		score += 10
	}
	return score
}

type parsedPathPrefix struct {
	RawPrefix                  string
	IsAtPrefix, IsQuotedPrefix bool
}

func parsePathPrefix(prefix string) parsedPathPrefix {
	switch {
	case strings.HasPrefix(prefix, "@\""):
		return parsedPathPrefix{RawPrefix: prefix[2:], IsAtPrefix: true, IsQuotedPrefix: true}
	case strings.HasPrefix(prefix, "\""):
		return parsedPathPrefix{RawPrefix: prefix[1:], IsQuotedPrefix: true}
	case strings.HasPrefix(prefix, "@"):
		return parsedPathPrefix{RawPrefix: prefix[1:], IsAtPrefix: true}
	default:
		return parsedPathPrefix{RawPrefix: prefix}
	}
}

func buildCompletionValue(path string, p parsedPathPrefix) string {
	prefix := ""
	if p.IsAtPrefix {
		prefix = "@"
	}
	if p.IsQuotedPrefix || strings.ContainsFunc(path, isAutocompleteSeparator) {
		return prefix + "\"" + path + "\""
	}
	return prefix + path
}

func buildRelativePath(prefix, name string) string {
	if strings.HasSuffix(prefix, "/") {
		return prefix + name
	}
	if strings.ContainsAny(prefix, `/\`) {
		dir := filepath.Dir(prefix)
		if dir == "." {
			if strings.HasPrefix(prefix, "./") {
				return "./" + name
			}
			return name
		}
		if dir == "/" {
			return "/" + name
		}
		rel := filepath.Join(dir, name)
		if strings.HasPrefix(prefix, "./") && !strings.HasPrefix(rel, "./") {
			rel = "./" + rel
		}
		return rel
	}
	if strings.HasPrefix(prefix, "~") {
		return "~/" + name
	}
	return name
}

func extractPathPrefix(text string, force bool) (string, bool) {
	if quoted := extractQuotedPrefix(text); quoted != "" {
		return quoted, true
	}
	idx := findLastDelimiter(text)
	prefix := text
	if idx >= 0 {
		prefix = text[idx+1:]
	}
	prefix = stripLeadingWrappers(prefix)
	last, _ := utf8.DecodeLastRuneInString(text)
	if force || strings.Contains(prefix, "/") || strings.HasPrefix(prefix, ".") || strings.HasPrefix(prefix, "~/") || (prefix == "" && text != "" && isAutocompleteSeparator(last)) {
		return prefix, true
	}
	return "", false
}

// HasTriggerPrefix checks editor trigger tokens after whitespace or prose punctuation.
func HasTriggerPrefix(text string, triggers []rune) bool {
	if quote := strings.LastIndexByte(text, '"'); quote > 0 && text[quote-1] == '@' && slices.Contains(triggers, '@') {
		start := quote - 1
		for start > 0 && wrapperCloser(text[start-1]) != 0 {
			start--
		}
		if start == 0 {
			return true
		}
		previous, _ := utf8.DecodeLastRuneInString(text[:start])
		if isAutocompleteSeparator(previous) {
			return true
		}
	}
	start := 0
	for index, r := range text {
		if isAutocompleteSeparator(r) {
			start = index + utf8.RuneLen(r)
		}
	}
	token := text[start:]
	for len(token) > 0 && wrapperCloser(token[0]) != 0 {
		token = token[1:]
	}
	first, _ := utf8.DecodeRuneInString(token)
	return len(token) > 0 && slices.Contains(triggers, first)
}

// HasAttachmentPrefix reports whether the text before a cursor ends in an active attachment token.
func HasAttachmentPrefix(text string) bool { return extractAtPrefix(text) != "" }

func extractAtPrefix(text string) string {
	if quoted := extractQuotedPrefix(text); strings.HasPrefix(quoted, "@\"") {
		return quoted
	}
	idx := findLastDelimiter(text)
	start := 0
	if idx >= 0 {
		start = idx + 1
	}
	token := stripLeadingWrappers(text[start:])
	if strings.HasPrefix(token, "@") {
		return token
	}
	return ""
}

func extractQuotedPrefix(text string) string {
	start := -1
	in := false
	for i, r := range text {
		if r == '"' {
			in = !in
			if in {
				start = i
			}
		}
	}
	if !in {
		return ""
	}
	if start > 0 && text[start-1] == '@' {
		if isTokenStart(text, start-1) {
			return text[start-1:]
		}
		return ""
	}
	if isTokenStart(text, start) {
		return text[start:]
	}
	return ""
}

func findLastDelimiter(text string) int {
	last := -1
	for i, r := range text {
		if strings.ContainsRune(" \t\"'=", r) || isAutocompleteSeparator(r) {
			last = i + utf8.RuneLen(r) - 1
		}
	}
	return last
}

func stripLeadingWrappers(token string) string {
	for len(token) > 0 {
		closer := wrapperCloser(token[0])
		if closer == 0 || strings.ContainsRune(token[1:], rune(closer)) {
			break
		}
		token = token[1:]
	}
	return token
}

func wrapperCloser(r byte) byte {
	switch r {
	case '(':
		return ')'
	case '[':
		return ']'
	case '{':
		return '}'
	case '<':
		return '>'
	case '`':
		return '`'
	default:
		return 0
	}
}

func isWhitespace(r rune) bool { return (unicode.IsSpace(r) && r != '\u0085') || r == '\ufeff' }

func isAutocompleteSeparator(r rune) bool {
	// CJK punctuation with the upstream Script_Extensions set, plus fullwidth prose punctuation.
	return isWhitespace(r) || strings.ContainsRune("·—‘’“”…、。〃〈〉《》「」『』【】〔〕〖〗〘〙〚〛〜〝〞〟〰〽゠・﹅﹆！（），．：；？［］｛｝｡｢｣､･𖿢", r)
}

func isTokenStart(text string, index int) bool {
	for index > 0 && wrapperCloser(text[index-1]) != 0 {
		index--
	}
	if index == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:index])
	return strings.ContainsRune(" \t\"'=", r) || isAutocompleteSeparator(r)
}

func lineAt(lines []string, n int) string {
	if n < 0 || n >= len(lines) {
		return ""
	}
	return lines[n]
}

func sliceString(s string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}

func cursorOffset(item Item, isDir bool) int {
	if isDir && strings.HasSuffix(item.Value, "\"") {
		return len(item.Value) - 1
	}
	return len(item.Value)
}

func dirSuffix(isDir bool) string {
	if isDir {
		return "/"
	}
	return ""
}

func sortItems(items []Item) {
	sort.Slice(items, func(i, j int) bool {
		ai, bi := strings.HasSuffix(items[i].Label, "/"), strings.HasSuffix(items[j].Label, "/")
		if ai != bi {
			return ai
		}
		return items[i].Label < items[j].Label
	})
}
