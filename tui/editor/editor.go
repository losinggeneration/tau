package editor

import (
	"context"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/losinggeneration/rovel/text"
	"github.com/losinggeneration/tau/tui/autocomplete"
	"github.com/losinggeneration/tau/tui/killring"
	"github.com/losinggeneration/tau/tui/undostack"
)

type State struct {
	Text   string
	Cursor int
}
type Position struct{ Line, Col int }

type SuggestionProvider interface {
	GetSuggestions(ctx context.Context, lines []string, cursorLine, cursorCol int, force bool) (*autocomplete.Suggestions, error)
}

type Editor struct {
	text                 string
	cursor               int
	submit               func(string)
	cancelled            bool
	kill                 killring.Ring
	undo                 *undostack.Stack[State]
	last                 string
	yankStart, yankEnd   int
	selectionStart       int
	selectionEnd         int
	history              []string
	historyIndex         int
	historyDraft         string
	historyBrowsing      bool
	desiredCol           int
	desiredColValid      bool
	autocomplete         *autocomplete.Suggestions
	autocompleteIndex    int
	autocompleteVisible  int
	autocompleteProvider SuggestionProvider
	autocompleteSeq      int
	scrollOffset         int
	largePasteThreshold  int
	largePastes          map[string]string
	nextPaste            int
}

func New() *Editor {
	return &Editor{undo: undostack.New(func(s State) State { return s }), yankStart: -1, yankEnd: -1, selectionStart: -1, selectionEnd: -1, historyIndex: -1, desiredCol: -1, autocompleteVisible: 5, largePasteThreshold: 1000, largePastes: map[string]string{}}
}

func (e *Editor) SetText(s string) {
	e.setText(s)
	e.resetHistoryBrowse()
}
func (e *Editor) Text() string { return e.text }
func (e *Editor) Cursor() int  { return e.cursor }
func (e *Editor) SetCursor(pos int) {
	if pos < 0 {
		pos = 0
	}
	if pos > len(e.text) {
		pos = len(e.text)
	}
	e.cursor = text.ClampCluster(e.text, pos)
	e.breakChains()
}
func (e *Editor) SetOnSubmit(fn func(string)) { e.submit = fn }
func (e *Editor) Cancelled() bool             { return e.cancelled }
func (e *Editor) SetLargePasteThreshold(n int) {
	if n < 0 {
		n = 0
	}
	e.largePasteThreshold = n
}

func (e *Editor) LargePastes() map[string]string {
	out := map[string]string{}
	for k, v := range e.largePastes {
		out[k] = v
	}
	return out
}

func (e *Editor) AddHistory(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	e.history = append(e.history, s)
	e.historyIndex = len(e.history)
	e.historyDraft = ""
	e.historyBrowsing = false
}

func (e *Editor) HistoryEntries() []string {
	return append([]string(nil), e.history...)
}

func (e *Editor) HistoryIndex() int {
	if e.historyIndex < 0 || e.historyIndex > len(e.history) {
		return len(e.history)
	}
	return e.historyIndex
}

func (e *Editor) HistoryPrev() {
	if len(e.history) == 0 {
		return
	}
	if e.historyIndex < 0 || e.historyIndex > len(e.history) {
		e.historyIndex = len(e.history)
	}
	if !e.historyBrowsing {
		e.historyDraft = e.text
		e.historyBrowsing = true
	}
	if e.historyIndex > 0 {
		e.historyIndex--
		e.setText(e.history[e.historyIndex])
	}
}

func (e *Editor) HistoryNext() {
	if len(e.history) == 0 || !e.historyBrowsing {
		return
	}
	if e.historyIndex < len(e.history)-1 {
		e.historyIndex++
		e.setText(e.history[e.historyIndex])
		return
	}
	e.historyIndex = len(e.history)
	draft := e.historyDraft
	e.historyDraft = ""
	e.historyBrowsing = false
	e.setText(draft)
}

func (e *Editor) SetAutocomplete(s *autocomplete.Suggestions) {
	e.autocomplete = s
	e.autocompleteIndex = 0
}

func (e *Editor) SetAutocompleteProvider(provider SuggestionProvider) {
	e.autocompleteProvider = provider
}

func (e *Editor) UpdateAutocomplete(ctx context.Context, force bool) error {
	if e.autocompleteProvider == nil {
		e.autocomplete = nil
		return nil
	}
	e.autocompleteSeq++
	seq := e.autocompleteSeq
	pos := e.Position()
	suggestions, err := e.autocompleteProvider.GetSuggestions(ctx, e.Lines(), pos.Line, pos.Col, force)
	if seq != e.autocompleteSeq {
		return nil
	}
	if err != nil {
		return err
	}
	e.SetAutocomplete(suggestions)
	return nil
}
func (e *Editor) AutocompleteIndex() int { return e.autocompleteIndex }
func (e *Editor) AutocompleteVisible() bool {
	return e.autocomplete != nil && len(e.autocomplete.Items) > 0
}

func (e *Editor) SetAutocompleteMaxVisible(n int) {
	if n <= 0 {
		n = 1
	}
	e.autocompleteVisible = n
}

func (e *Editor) AutocompleteMaxVisible() int {
	if e.autocompleteVisible <= 0 {
		return 5
	}
	return e.autocompleteVisible
}

func (e *Editor) VisibleAutocompleteItems() []autocomplete.Item {
	if !e.AutocompleteVisible() {
		return nil
	}
	maxVisible := e.AutocompleteMaxVisible()
	start := e.autocompleteIndex - maxVisible/2
	if start < 0 {
		start = 0
	}
	if start+maxVisible > len(e.autocomplete.Items) {
		start = len(e.autocomplete.Items) - maxVisible
	}
	if start < 0 {
		start = 0
	}
	end := start + maxVisible
	if end > len(e.autocomplete.Items) {
		end = len(e.autocomplete.Items)
	}
	return append([]autocomplete.Item(nil), e.autocomplete.Items[start:end]...)
}
func (e *Editor) CancelAutocomplete() { e.autocomplete = nil }
func (e *Editor) AutocompleteNext() {
	if e.autocomplete != nil && len(e.autocomplete.Items) > 0 {
		e.autocompleteIndex = (e.autocompleteIndex + 1) % len(e.autocomplete.Items)
	}
}

func (e *Editor) AutocompletePrev() {
	if e.autocomplete != nil && len(e.autocomplete.Items) > 0 {
		e.autocompleteIndex = (e.autocompleteIndex + len(e.autocomplete.Items) - 1) % len(e.autocomplete.Items)
	}
}

func (e *Editor) ApplyAutocomplete() {
	if e.autocomplete == nil || len(e.autocomplete.Items) == 0 {
		return
	}
	lines := e.Lines()
	pos := e.Position()
	res := autocomplete.ApplyCompletion(lines, pos.Line, pos.Col, e.autocomplete.Items[e.autocompleteIndex], e.autocomplete.Prefix)
	e.SetText(joinLines(res.Lines))
	e.SetPosition(Position{res.CursorLine, res.CursorCol})
	e.autocomplete = nil
}

func (e *Editor) Insert(s string) {
	if s == "" {
		return
	}
	e.resetHistoryBrowse()
	if e.hasSelection() {
		e.pushUndo()
		e.deleteSelectionNoUndo()
	} else if e.shouldPushInsertUndo(s) {
		e.pushUndo()
	}
	e.text = e.text[:e.cursor] + s + e.text[e.cursor:]
	e.cursor += len(s)
	if isCoalescableTyping(s) {
		e.last = "insert"
	} else {
		e.last = ""
	}
	e.yankStart = -1
	e.desiredColValid = false
}

func (e *Editor) Paste(s string) {
	if s == "" {
		return
	}
	e.resetHistoryBrowse()
	s = normalizePasteText(s)
	if s == "" {
		return
	}
	if needsLeadingPasteSpace(e.text, e.cursor, s) {
		s = " " + s
	}
	if isLargePaste(s) || (e.largePasteThreshold > 0 && len(s) > e.largePasteThreshold) {
		e.nextPaste++
		id := e.nextPaste
		marker := pasteMarker(id, s)
		e.largePastes[marker] = s
		e.insertAtomic(marker, "paste")
		return
	}
	e.insertAtomic(s, "paste")
}
func (e *Editor) Newline() { e.Insert("\n") }
func (e *Editor) Submit() {
	text := strings.TrimSpace(e.ExpandedText())
	if text == "" {
		return
	}
	e.text = ""
	e.cursor = 0
	e.largePastes = map[string]string{}
	e.undo = undostack.New(func(s State) State { return s })
	e.breakChains()
	if e.submit != nil {
		e.submit(text)
	}
}
func (e *Editor) Cancel() { e.cancelled = true }
func (e *Editor) Undo() {
	st, ok := e.undo.Pop()
	if !ok {
		return
	}
	e.text = st.Text
	e.cursor = st.Cursor
	e.breakChains()
}

func (e *Editor) MoveLeft() {
	e.cursor = text.PrevCluster(e.text, e.cursor)
	e.breakChains()
}

func (e *Editor) MoveRight() {
	e.cursor = text.NextCluster(e.text, e.cursor)
	e.breakChains()
}

func (e *Editor) MoveLineStart() {
	for e.cursor > 0 && e.text[e.cursor-1] != '\n' {
		e.moveLeftNoBreak()
	}
	e.breakChains()
}

func (e *Editor) MoveLineEnd() {
	for e.cursor < len(e.text) {
		r, sz := utf8.DecodeRuneInString(e.text[e.cursor:])
		if r == '\n' {
			break
		}
		e.cursor += sz
	}
	e.breakChains()
}

func (e *Editor) MoveUp() {
	p := e.Position()
	if p.Line == 0 {
		return
	}
	desired := e.verticalDesiredCol(p.Col)
	e.SetPosition(Position{p.Line - 1, desired})
	e.desiredCol = desired
	e.desiredColValid = true
}

func (e *Editor) MoveDown() {
	p := e.Position()
	if p.Line >= len(e.Lines())-1 {
		return
	}
	desired := e.verticalDesiredCol(p.Col)
	e.SetPosition(Position{p.Line + 1, desired})
	e.desiredCol = desired
	e.desiredColValid = true
}

func (e *Editor) MoveVisualUp(width int) {
	e.moveVisual(width, -1)
}

func (e *Editor) MoveVisualDown(width int) {
	e.moveVisual(width, 1)
}

func (e *Editor) AtVisualTop(width int) bool {
	line, visualLine, _, _ := e.visualPosition(width)
	return line == 0 && visualLine == 0
}

func (e *Editor) AtVisualBottom(width int) bool {
	line, visualLine, _, segments := e.visualPosition(width)
	return line == len(segments)-1 && visualLine == len(segments[line])-1
}

func (e *Editor) PageUp(n int) {
	if n <= 0 {
		n = 10
	}
	for range n {
		e.MoveUp()
	}
}

func (e *Editor) PageDown(n int) {
	if n <= 0 {
		n = 10
	}
	for range n {
		e.MoveDown()
	}
}

func (e *Editor) DeleteBackward() string {
	if e.cursor == 0 {
		return ""
	}
	e.resetHistoryBrowse()
	e.pushUndo()
	old := e.cursor
	e.moveLeftNoBreak()
	deleted := e.text[e.cursor:old]
	e.text = e.text[:e.cursor] + e.text[old:]
	e.kill.Push(deleted, killring.PushOptions{Prepend: true, Accumulate: e.last == "kill"})
	e.last = "kill"
	return deleted
}

func (e *Editor) DeleteForward() string {
	if e.cursor >= len(e.text) {
		return ""
	}
	e.resetHistoryBrowse()
	e.pushUndo()
	next := text.NextCluster(e.text, e.cursor)
	deleted := e.text[e.cursor:next]
	e.text = e.text[:e.cursor] + e.text[next:]
	e.kill.Push(deleted, killring.PushOptions{Accumulate: e.last == "kill"})
	e.last = "kill"
	return deleted
}

func (e *Editor) DeleteToLineStart() string {
	if e.cursor == 0 {
		return ""
	}
	e.resetHistoryBrowse()
	e.pushUndo()
	old := e.cursor
	for e.cursor > 0 && e.text[e.cursor-1] != '\n' {
		e.moveLeftNoBreak()
	}
	deleted := e.text[e.cursor:old]
	e.text = e.text[:e.cursor] + e.text[old:]
	e.kill.Push(deleted, killring.PushOptions{Prepend: true, Accumulate: e.last == "kill"})
	e.last = "kill"
	return deleted
}

func (e *Editor) DeleteToLineEnd() string {
	if e.cursor >= len(e.text) {
		return ""
	}
	e.resetHistoryBrowse()
	e.pushUndo()
	start := e.cursor
	for e.cursor < len(e.text) {
		r, sz := utf8.DecodeRuneInString(e.text[e.cursor:])
		if r == '\n' {
			break
		}
		e.cursor += sz
	}
	end := e.cursor
	deleted := e.text[start:end]
	e.text = e.text[:start] + e.text[end:]
	e.cursor = start
	e.kill.Push(deleted, killring.PushOptions{Accumulate: e.last == "kill"})
	e.last = "kill"
	return deleted
}

func (e *Editor) DeleteWordBackward() string {
	if e.cursor == 0 {
		return ""
	}
	e.resetHistoryBrowse()
	e.pushUndo()
	old := e.cursor
	for e.cursor > 0 {
		r, _ := utf8.DecodeLastRuneInString(e.text[:e.cursor])
		if !unicode.IsSpace(r) {
			break
		}
		e.moveLeftNoBreak()
	}
	for e.cursor > 0 {
		r, _ := utf8.DecodeLastRuneInString(e.text[:e.cursor])
		if unicode.IsSpace(r) {
			break
		}
		e.moveLeftNoBreak()
	}
	deleted := e.text[e.cursor:old]
	e.text = e.text[:e.cursor] + e.text[old:]
	e.kill.Push(deleted, killring.PushOptions{Prepend: true, Accumulate: e.last == "kill"})
	e.last = "kill"
	return deleted
}

func (e *Editor) DeleteWordForward() string {
	if e.cursor >= len(e.text) {
		return ""
	}
	e.resetHistoryBrowse()
	e.pushUndo()
	start := e.cursor
	for e.cursor < len(e.text) {
		r, sz := utf8.DecodeRuneInString(e.text[e.cursor:])
		e.cursor += sz
		if unicode.IsSpace(r) {
			break
		}
	}
	end := e.cursor
	deleted := e.text[start:end]
	e.text = e.text[:start] + e.text[end:]
	e.cursor = start
	e.kill.Push(deleted, killring.PushOptions{Accumulate: e.last == "kill"})
	e.last = "kill"
	return deleted
}

func (e *Editor) Yank() {
	text, ok := e.kill.Peek()
	if !ok {
		return
	}
	e.resetHistoryBrowse()
	e.pushUndo()
	start := e.cursor
	e.text = e.text[:e.cursor] + text + e.text[e.cursor:]
	e.cursor += len(text)
	e.yankStart = start
	e.yankEnd = e.cursor
	e.last = "yank"
}

func (e *Editor) YankPop() {
	if e.last != "yank" || e.yankStart < 0 || e.kill.Len() < 2 {
		return
	}
	e.pushUndo()
	e.text = e.text[:e.yankStart] + e.text[e.yankEnd:]
	e.cursor = e.yankStart
	e.kill.Rotate()
	text, _ := e.kill.Peek()
	e.text = e.text[:e.cursor] + text + e.text[e.cursor:]
	e.cursor += len(text)
	e.yankEnd = e.cursor
	e.last = "yank"
}

func (e *Editor) MoveWordLeft() {
	for e.cursor > 0 {
		r, _ := utf8.DecodeLastRuneInString(e.text[:e.cursor])
		if !unicode.IsSpace(r) {
			break
		}
		e.moveLeftNoBreak()
	}
	for e.cursor > 0 {
		r, _ := utf8.DecodeLastRuneInString(e.text[:e.cursor])
		if unicode.IsSpace(r) {
			break
		}
		e.moveLeftNoBreak()
	}
	e.breakChains()
}

func (e *Editor) MoveWordRight() {
	for e.cursor < len(e.text) {
		r, sz := utf8.DecodeRuneInString(e.text[e.cursor:])
		if !unicode.IsSpace(r) {
			break
		}
		e.cursor += sz
	}
	for e.cursor < len(e.text) {
		r, sz := utf8.DecodeRuneInString(e.text[e.cursor:])
		if unicode.IsSpace(r) {
			break
		}
		e.cursor += sz
	}
	e.breakChains()
}

func (e *Editor) JumpForward(target rune) bool {
	for pos := e.cursor; pos < len(e.text); {
		r, sz := utf8.DecodeRuneInString(e.text[pos:])
		pos += sz
		if r == target {
			e.cursor = pos - sz
			e.breakChains()
			return true
		}
	}
	return false
}

func (e *Editor) JumpBackward(target rune) bool {
	for pos := e.cursor; pos > 0; {
		r, sz := utf8.DecodeLastRuneInString(e.text[:pos])
		pos -= sz
		if r == target {
			e.cursor = pos
			e.breakChains()
			return true
		}
	}
	return false
}
func (e *Editor) ScrollOffset() int { return e.scrollOffset }
func (e *Editor) SetScrollOffset(offset int) {
	if offset < 0 {
		offset = 0
	}
	e.scrollOffset = offset
}

func (e *Editor) EnsureCursorVisible(maxRows, wrapWidth int) {
	if maxRows <= 0 {
		e.scrollOffset = 0
		return
	}
	idx := e.visualCursorIndex(wrapWidth)
	if idx < e.scrollOffset {
		e.scrollOffset = idx
	}
	if idx >= e.scrollOffset+maxRows {
		e.scrollOffset = idx - maxRows + 1
	}
	if e.scrollOffset < 0 {
		e.scrollOffset = 0
	}
}

func (e *Editor) ExpandedText() string {
	out := e.text
	for marker, body := range e.largePastes {
		out = strings.ReplaceAll(out, marker, body)
	}
	return out
}

func (e *Editor) Lines() []string {
	var out []string
	start := 0
	for i, c := range e.text {
		if c == '\n' {
			out = append(out, e.text[start:i])
			start = i + 1
		}
	}
	out = append(out, e.text[start:])
	return out
}

func (e *Editor) Position() Position {
	line, start := 0, 0
	for idx, r := range e.text {
		if idx >= e.cursor {
			break
		}
		if r == '\n' {
			line++
			start = idx + 1
		}
	}
	col := text.ColumnOf(e.text[start:e.cursor], e.cursor-start)
	return Position{line, col}
}

func (e *Editor) SetPosition(p Position) {
	lines := e.Lines()
	if p.Line < 0 {
		p.Line = 0
	}
	if p.Line >= len(lines) {
		p.Line = len(lines) - 1
	}
	off := 0
	for l := 0; l < p.Line; l++ {
		off += len(lines[l]) + 1
	}
	line := lines[p.Line]
	off += text.OffsetAtColumnBias(line, p.Col, text.BiasLeft)
	e.cursor = off
	e.breakChains()
}

func (e *Editor) SetSelection(start, end int) {
	start = e.clampByte(start)
	end = e.clampByte(end)
	if start == end {
		e.clearSelection()
		return
	}
	if start > end {
		start, end = end, start
	}
	e.selectionStart = start
	e.selectionEnd = end
}

func (e *Editor) Selection() (int, int, bool) {
	if !e.hasSelection() {
		return 0, 0, false
	}
	return e.selectionStart, e.selectionEnd, true
}

func (e *Editor) CopySelection() string {
	if !e.hasSelection() {
		return ""
	}
	return e.text[e.selectionStart:e.selectionEnd]
}

func (e *Editor) CutSelection() string {
	if !e.hasSelection() {
		return ""
	}
	e.resetHistoryBrowse()
	e.pushUndo()
	deleted := e.deleteSelectionNoUndo()
	e.kill.Push(deleted, killring.PushOptions{})
	e.last = "kill"
	return deleted
}

func (e *Editor) moveLeftNoBreak() {
	e.cursor = text.PrevCluster(e.text, e.cursor)
}

func (e *Editor) setText(s string) {
	e.text = s
	e.cursor = len(s)
	e.scrollOffset = 0
	e.last = ""
	e.yankStart = -1
	e.yankEnd = -1
	e.clearSelection()
	e.desiredColValid = false
}

func (e *Editor) resetHistoryBrowse() {
	e.historyIndex = len(e.history)
	e.historyDraft = ""
	e.historyBrowsing = false
}

func (e *Editor) pushUndo() { e.undo.Push(State{e.text, e.cursor}) }
func (e *Editor) breakChains() {
	if e.last != "kill" {
		e.last = ""
	}
	e.yankStart = -1
	e.yankEnd = -1
	e.clearSelection()
	e.desiredColValid = false
}

func (e *Editor) insertAtomic(s, last string) {
	e.resetHistoryBrowse()
	e.pushUndo()
	if e.hasSelection() {
		e.deleteSelectionNoUndo()
	}
	e.text = e.text[:e.cursor] + s + e.text[e.cursor:]
	e.cursor += len(s)
	e.last = last
	e.yankStart = -1
	e.desiredColValid = false
}

func (e *Editor) shouldPushInsertUndo(s string) bool {
	return e.last != "insert" || !isCoalescableTyping(s)
}

func isCoalescableTyping(s string) bool {
	r, sz := utf8.DecodeRuneInString(s)
	return r != utf8.RuneError && sz == len(s) && !unicode.IsSpace(r)
}

func (e *Editor) verticalDesiredCol(current int) int {
	if e.desiredColValid {
		return e.desiredCol
	}
	return current
}

func (e *Editor) clampByte(pos int) int {
	if pos < 0 {
		pos = 0
	}
	if pos > len(e.text) {
		pos = len(e.text)
	}
	for pos > 0 && pos < len(e.text) && !utf8.RuneStart(e.text[pos]) {
		pos--
	}
	return pos
}

func (e *Editor) hasSelection() bool {
	return e.selectionStart >= 0 && e.selectionEnd > e.selectionStart && e.selectionEnd <= len(e.text)
}

func (e *Editor) clearSelection() {
	e.selectionStart = -1
	e.selectionEnd = -1
}

func (e *Editor) deleteSelectionNoUndo() string {
	if !e.hasSelection() {
		return ""
	}
	deleted := e.text[e.selectionStart:e.selectionEnd]
	e.text = e.text[:e.selectionStart] + e.text[e.selectionEnd:]
	e.cursor = e.selectionStart
	e.clearSelection()
	return deleted
}

func (e *Editor) moveVisual(width, delta int) {
	if width <= 0 {
		width = 1
	}
	line, visualLine, visualCol, segments := e.visualPosition(width)
	flatIndex := 0
	for i := 0; i < line; i++ {
		flatIndex += len(segments[i])
	}
	flatIndex += visualLine
	target := flatIndex + delta
	if target < 0 || target >= totalSegments(segments) {
		return
	}
	desired := e.verticalDesiredCol(visualCol)
	targetLine, targetVisualLine := locateSegment(segments, target)
	segmentLen := segments[targetLine][targetVisualLine]
	col := desired
	if col > segmentLen {
		col = segmentLen
	}
	e.setVisualPosition(width, targetLine, targetVisualLine, col)
	e.desiredCol = desired
	e.desiredColValid = true
}

func (e *Editor) visualPosition(width int) (line, visualLine, visualCol int, segments [][]int) {
	pos := e.Position()
	lines := e.Lines()
	segments = make([][]int, len(lines))
	for i, l := range lines {
		segments[i] = visualSegments(l, width)
	}
	line = pos.Line
	col := pos.Col
	for visualLine < len(segments[line])-1 && col > segments[line][visualLine] {
		col -= segments[line][visualLine]
		visualLine++
	}
	visualCol = col
	if maxCol := segments[line][visualLine]; visualCol > maxCol {
		visualCol = maxCol
	}
	return line, visualLine, visualCol, segments
}

func (e *Editor) visualCursorIndex(width int) int {
	line, visualLine, _, segments := e.visualPosition(width)
	idx := 0
	for i := 0; i < line; i++ {
		idx += len(segments[i])
	}
	return idx + visualLine
}

func (e *Editor) setVisualPosition(width, line, visualLine, visualCol int) {
	lines := e.Lines()
	if line < 0 {
		line = 0
	}
	if line >= len(lines) {
		line = len(lines) - 1
	}
	segments := visualSegments(lines[line], width)
	if visualLine < 0 {
		visualLine = 0
	}
	if visualLine >= len(segments) {
		visualLine = len(segments) - 1
	}
	col := 0
	for i := 0; i < visualLine; i++ {
		col += segments[i]
	}
	if visualCol > segments[visualLine] {
		visualCol = segments[visualLine]
	}
	e.SetPosition(Position{line, col + visualCol})
}

func visualSegments(line string, width int) []int {
	if width <= 0 {
		width = 1
	}
	wrapped := text.WrapLines(line, text.WrapOptions{Width: width, Mode: text.WrapCluster})
	segments := make([]int, 0, len(wrapped))
	for _, line := range wrapped {
		segments = append(segments, line.Cols)
	}
	if len(segments) == 0 {
		return []int{0}
	}
	return segments
}

func totalSegments(segments [][]int) int {
	total := 0
	for _, line := range segments {
		total += len(line)
	}
	return total
}

func locateSegment(segments [][]int, target int) (int, int) {
	for line, lineSegments := range segments {
		if target < len(lineSegments) {
			return line, target
		}
		target -= len(lineSegments)
	}
	return len(segments) - 1, len(segments[len(segments)-1]) - 1
}

func VisualLines(value string, width int) []string {
	if width <= 0 {
		width = 1
	}
	wrapped := text.WrapLines(value, text.WrapOptions{Width: width, Mode: text.WrapCluster})
	out := make([]string, 0, len(wrapped))
	for _, line := range wrapped {
		out = append(out, value[line.StartByte:line.EndByte])
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func normalizePasteText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || !unicode.IsControl(r) {
			out = append(out, r)
		}
	}
	return string(out)
}

func needsLeadingPasteSpace(text string, cursor int, paste string) bool {
	if cursor <= 0 || paste == "" || strings.HasPrefix(paste, " ") || strings.HasPrefix(paste, "\n") {
		return false
	}
	prev, _ := utf8.DecodeLastRuneInString(text[:cursor])
	return unicode.IsLetter(prev) || unicode.IsDigit(prev) || prev == '_' || prev == '.' || prev == '/'
}

func isLargePaste(s string) bool {
	return strings.Count(s, "\n")+1 > 10 || len(s) > 1000
}

func pasteMarker(id int, s string) string {
	lines := strings.Count(s, "\n") + 1
	if lines > 1 {
		return "[paste #" + strconv.Itoa(id) + " +" + strconv.Itoa(lines) + " lines]"
	}
	return "[paste #" + strconv.Itoa(id) + " " + strconv.Itoa(len(s)) + " chars]"
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}
