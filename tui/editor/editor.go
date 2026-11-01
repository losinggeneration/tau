package editor

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/text"
	"github.com/losinggeneration/rovel/ui"
	"github.com/losinggeneration/tau/tui/ansi"
	"github.com/losinggeneration/tau/tui/autocomplete"
	"github.com/losinggeneration/tau/tui/keybindings"
	"github.com/losinggeneration/tau/tui/killring"
	"github.com/losinggeneration/tau/tui/undostack"
)

type State struct {
	Text   string
	Cursor int
}
type Position struct{ Line, Col int }

type editorSnapshot struct {
	State
	pastes       map[string]string
	pasteCounter int
}

func cloneEditorSnapshot(s editorSnapshot) editorSnapshot {
	s.pastes = maps.Clone(s.pastes)
	return s
}

type SuggestionProvider interface {
	GetSuggestions(ctx context.Context, lines []string, cursorLine, cursorCol int, force bool) (*autocomplete.Suggestions, error)
}

type Editor struct {
	text                 string
	cursor               int
	submit               func(string)
	cancelled            bool
	kill                 killring.Ring
	undo                 *undostack.Stack[editorSnapshot]
	last                 string
	yankStart, yankEnd   int
	selectionStart       int
	selectionEnd         int
	history              []string
	historyIndex         int
	historyDraft         State
	historyBrowsing      bool
	desiredCol           int
	desiredColValid      bool
	autocomplete         *autocomplete.Suggestions
	autocompleteIndex    int
	autocompleteVisible  int
	autocompleteProvider SuggestionProvider
	autocompleteSeq      int
	autocompleteCancel   context.CancelFunc
	autocompleteForce    bool
	autocompleteTriggers []rune
	scrollOffset         int
	largePasteThreshold  int
	largePastes          map[string]string
	nextPaste            int
	jumpDirection        int
}

func New() *Editor {
	return &Editor{undo: undostack.New(cloneEditorSnapshot), yankStart: -1, yankEnd: -1, selectionStart: -1, selectionEnd: -1, historyIndex: -1, desiredCol: -1, autocompleteVisible: 5, largePasteThreshold: 1000, largePastes: map[string]string{}}
}

func (e *Editor) SetText(s string) {
	e.CancelJump()
	e.CancelAutocomplete()
	s = normalizeText(s)
	if e.text != s {
		e.pushUndo()
	}
	clear(e.largePastes)
	e.nextPaste = 0
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
	e.cursor = e.clampSegment(pos)
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
	s = strings.TrimFunc(s, isWhitespace)
	if s == "" || (len(e.history) > 0 && e.history[len(e.history)-1] == s) {
		return
	}
	e.history = append(e.history, s)
	if len(e.history) > 100 {
		e.history = append([]string(nil), e.history[len(e.history)-100:]...)
	}
	e.historyIndex = len(e.history)
	e.historyDraft = State{}
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
	e.last = ""
	if len(e.history) == 0 {
		return
	}
	if e.historyIndex < 0 || e.historyIndex > len(e.history) {
		e.historyIndex = len(e.history)
	}
	if !e.historyBrowsing {
		e.pushUndo()
		e.historyDraft = State{e.text, e.cursor}
		e.historyBrowsing = true
	}
	if e.historyIndex > 0 {
		e.historyIndex--
		e.setText(e.history[e.historyIndex])
		e.cursor = 0
	}
}

func (e *Editor) HistoryNext() {
	e.last = ""
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
	e.historyDraft = State{}
	e.historyBrowsing = false
	e.setText(draft.Text)
	e.cursor = draft.Cursor
}

func (e *Editor) SetAutocomplete(s *autocomplete.Suggestions) {
	e.autocomplete = s
	e.autocompleteIndex = s.FocusedIndex()
}

func (e *Editor) SetAutocompleteProvider(provider SuggestionProvider) {
	e.CancelAutocomplete()
	e.autocompleteProvider = provider
	e.autocompleteTriggers = []rune{'@', '#'}
	if custom, ok := provider.(interface{ TriggerCharacters() []rune }); ok {
		for _, r := range custom.TriggerCharacters() {
			if r > 0xffff || r == '/' || (unicode.IsSpace(r) && r != '\u0085') || r == '\ufeff' || slices.Contains(e.autocompleteTriggers, r) {
				continue
			}
			e.autocompleteTriggers = append(e.autocompleteTriggers, r)
		}
	}
}

func (e *Editor) UpdateAutocomplete(ctx context.Context, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.autocompleteProvider == nil {
		e.autocomplete = nil
		return nil
	}
	if e.autocompleteCancel != nil {
		e.autocompleteCancel()
	}
	requestCtx, cancel := context.WithCancel(ctx)
	e.autocompleteCancel = cancel
	defer cancel()
	e.autocompleteSeq++
	seq := e.autocompleteSeq
	pos := e.completionPosition()
	value, cursor := e.text, e.cursor
	suggestions, err := e.autocompleteProvider.GetSuggestions(requestCtx, e.Lines(), pos.Line, pos.Col, force)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if seq != e.autocompleteSeq {
		return nil
	}
	if e.text != value || e.cursor != cursor {
		e.CancelAutocomplete()
		return nil
	}
	e.SetAutocomplete(suggestions)
	e.autocompleteForce = force
	return nil
}

// RefreshAutocomplete updates an open list without applying a single result.
func (e *Editor) RefreshAutocomplete(ctx context.Context) error {
	if !e.AutocompleteVisible() {
		return nil
	}
	if err := e.UpdateAutocomplete(ctx, e.autocompleteForce); err != nil {
		return fmt.Errorf("refresh completion: %w", err)
	}
	return nil
}

// IsAtCommandStart reports whether only the initial slash and indentation precede the cursor.
func (e *Editor) IsAtCommandStart() bool {
	pos := e.completionPosition()
	if pos.Line != 0 {
		return false
	}
	before := strings.TrimFunc(e.Lines()[0][:pos.Col], isWhitespace)
	return before == "" || before == "/"
}

// Shared CJK Script_Extensions characters are absent from Go's script tables.
var sharedCJKCharacters = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0xb7, Hi: 0xb7, Stride: 1},
		{Lo: 0x2c7, Hi: 0x2c7, Stride: 1},
		{Lo: 0x2c9, Hi: 0x2cb, Stride: 1},
		{Lo: 0x2d9, Hi: 0x2d9, Stride: 1},
		{Lo: 0x305, Hi: 0x305, Stride: 1},
		{Lo: 0x323, Hi: 0x323, Stride: 1},
		{Lo: 0x2ff0, Hi: 0x2fff, Stride: 1},
		{Lo: 0x3001, Hi: 0x3003, Stride: 1},
		{Lo: 0x3006, Hi: 0x3006, Stride: 1},
		{Lo: 0x3008, Hi: 0x3011, Stride: 1},
		{Lo: 0x3013, Hi: 0x301f, Stride: 1},
		{Lo: 0x302a, Hi: 0x302d, Stride: 1},
		{Lo: 0x3030, Hi: 0x3035, Stride: 1},
		{Lo: 0x3037, Hi: 0x3037, Stride: 1},
		{Lo: 0x303c, Hi: 0x303f, Stride: 1},
		{Lo: 0x3099, Hi: 0x309c, Stride: 1},
		{Lo: 0x30a0, Hi: 0x30a0, Stride: 1},
		{Lo: 0x30fb, Hi: 0x30fc, Stride: 1},
		{Lo: 0x3190, Hi: 0x319f, Stride: 1},
		{Lo: 0x31c0, Hi: 0x31e5, Stride: 1},
		{Lo: 0x31ef, Hi: 0x31ef, Stride: 1},
		{Lo: 0x3220, Hi: 0x3247, Stride: 1},
		{Lo: 0x3280, Hi: 0x32b0, Stride: 1},
		{Lo: 0x32c0, Hi: 0x32cb, Stride: 1},
		{Lo: 0x32ff, Hi: 0x32ff, Stride: 1},
		{Lo: 0x3358, Hi: 0x3370, Stride: 1},
		{Lo: 0x337b, Hi: 0x337f, Stride: 1},
		{Lo: 0x33e0, Hi: 0x33fe, Stride: 1},
		{Lo: 0xa700, Hi: 0xa707, Stride: 1},
		{Lo: 0xfe45, Hi: 0xfe46, Stride: 1},
		{Lo: 0xff61, Hi: 0xff65, Stride: 1},
		{Lo: 0xff70, Hi: 0xff70, Stride: 1},
		{Lo: 0xff9e, Hi: 0xff9f, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x1d360, Hi: 0x1d371, Stride: 1},
		{Lo: 0x1f250, Hi: 0x1f251, Stride: 1},
	},
}

// IsAutocompleteTrigger reports whether a typed character can start or retry completion.
func (e *Editor) IsAutocompleteTrigger(r rune) bool {
	if slices.Contains(e.autocompleteTriggers, r) {
		pos := e.completionPosition()
		return autocomplete.HasTriggerPrefix(e.Lines()[pos.Line][:pos.Col], e.autocompleteTriggers)
	}
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Bopomofo, sharedCJKCharacters) || r == '/' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune(".-_", r)
}

// TriggerAutocomplete requests regular completion in command or symbol context.
func (e *Editor) TriggerAutocomplete(ctx context.Context) error {
	pos := e.completionPosition()
	before := e.Lines()[pos.Line][:pos.Col]
	slash := strings.TrimLeftFunc(before, isWhitespace)
	if (pos.Line != 0 || !strings.HasPrefix(slash, "/")) && !autocomplete.HasTriggerPrefix(before, e.autocompleteTriggers) {
		return nil
	}
	if err := e.UpdateAutocomplete(ctx, false); err != nil {
		return fmt.Errorf("trigger completion: %w", err)
	}
	return nil
}

// CompleteOnTab requests suggestions and applies a single forced file result.
func (e *Editor) CompleteOnTab(ctx context.Context) error {
	pos := e.completionPosition()
	lines := e.Lines()
	before := strings.TrimLeftFunc(lines[pos.Line][:pos.Col], isWhitespace)
	force := pos.Line != 0 || !strings.HasPrefix(before, "/") || strings.Contains(before, " ")
	if force {
		if admission, ok := e.autocompleteProvider.(interface{ ShouldTriggerFileCompletion([]string, int, int) bool }); ok && !admission.ShouldTriggerFileCompletion(lines, pos.Line, pos.Col) {
			return ctx.Err()
		}
	}
	value, cursor := e.text, e.cursor
	seq := e.autocompleteSeq + 1
	if err := e.UpdateAutocomplete(ctx, force); err != nil {
		return fmt.Errorf("complete on Tab: %w", err)
	}
	if e.autocompleteSeq == seq && e.text == value && e.cursor == cursor && force && e.autocomplete != nil && len(e.autocomplete.Items) == 1 {
		e.ApplyAutocomplete()
	}
	return nil
}

// AutocompletePrefix returns the prefix replaced by the active completion.
func (e *Editor) AutocompletePrefix() string {
	if e.autocomplete == nil {
		return ""
	}
	return e.autocomplete.Prefix
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

func (e *Editor) CancelAutocomplete() {
	if e.autocompleteCancel != nil {
		e.autocompleteCancel()
		e.autocompleteCancel = nil
	}
	e.autocompleteSeq++
	e.autocomplete = nil
}

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

func (e *Editor) completionPosition() Position {
	before := e.text[:e.cursor]
	return Position{strings.Count(before, "\n"), len(before) - strings.LastIndex(before, "\n") - 1}
}

func (e *Editor) ApplyAutocomplete() {
	if e.autocomplete == nil || len(e.autocomplete.Items) == 0 {
		return
	}
	lines := e.Lines()
	pos := e.completionPosition()
	apply := autocomplete.ApplyCompletion
	if provider, ok := e.autocompleteProvider.(interface {
		ApplyCompletion([]string, int, int, autocomplete.Item, string) autocomplete.ApplyResult
	}); ok {
		apply = provider.ApplyCompletion
	}
	res := apply(lines, pos.Line, pos.Col, e.autocomplete.Items[e.autocompleteIndex], e.autocomplete.Prefix)
	e.pushUndo()
	e.setText(joinLines(res.Lines))
	cursor := res.CursorCol
	for _, line := range res.Lines[:res.CursorLine] {
		cursor += len(line) + 1
	}
	e.SetCursor(cursor)
	e.CancelAutocomplete()
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
	e.last = "insert"
	e.yankStart = -1
	e.desiredColValid = false
}

// InsertTextAtCursor inserts normalized text as one undoable edit, without paste markers.
func (e *Editor) InsertTextAtCursor(s string) {
	if s == "" {
		return
	}
	e.CancelAutocomplete()
	e.insertAtomic(normalizeText(s), "")
}

func (e *Editor) Paste(s string) {
	e.CancelAutocomplete()
	e.resetHistoryBrowse()
	s = normalizePasteText(s)
	if s == "" {
		e.pushUndo()
		e.last = ""
		return
	}
	if needsLeadingPasteSpace(e.text, e.cursor, s) {
		s = " " + s
	}
	if isLargePaste(s) || (e.largePasteThreshold > 0 && len(utf16.Encode([]rune(s))) > e.largePasteThreshold) {
		id := e.nextPaste + 1
		marker := pasteMarker(id, s)
		e.insertAtomic(marker, "paste")
		e.nextPaste = id
		e.largePastes[marker] = s
		return
	}
	e.insertAtomic(s, "paste")
}
func (e *Editor) Newline() { e.CancelAutocomplete(); e.insertAtomic("\n", "") }
func (e *Editor) Submit() {
	text := strings.TrimFunc(e.ExpandedText(), isWhitespace)
	e.Reset()
	if e.submit != nil {
		e.submit(text)
	}
}

// Reset discards the draft and its undo state while retaining history and kills.
func (e *Editor) Reset() {
	e.CancelJump()
	e.text = ""
	e.cursor = 0
	clear(e.largePastes)
	e.nextPaste = 0
	e.CancelAutocomplete()
	e.resetHistoryBrowse()
	e.scrollOffset = 0
	e.undo.Clear()
	e.breakChains()
}

func (e *Editor) Cancel() { e.cancelled = true }
func (e *Editor) Undo() {
	e.resetHistoryBrowse()
	st, ok := e.undo.Pop()
	if !ok {
		return
	}
	e.text = st.Text
	e.cursor = st.Cursor
	e.largePastes = st.pastes
	e.nextPaste = st.pasteCounter
	e.breakChains()
}

func (e *Editor) MoveLeft() {
	e.cursor = e.previousSegment(e.cursor)
	e.breakChains()
}

func (e *Editor) MoveRight() {
	e.cursor = e.nextSegment(e.cursor)
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

// CursorUp moves by a visual row or browses history from the top-left edge.
func (e *Editor) CursorUp(width int) {
	if !e.AtVisualTop(width) {
		e.MoveVisualUp(width)
		return
	}
	if e.text == "" || e.historyBrowsing || e.Position().Col == 0 {
		e.HistoryPrev()
		return
	}
	e.MoveLineStart()
}

// CursorDown moves by a visual row or returns from history at the bottom edge.
func (e *Editor) CursorDown(width int) {
	if !e.AtVisualBottom(width) {
		e.MoveVisualDown(width)
		return
	}
	if e.historyBrowsing {
		e.HistoryNext()
		return
	}
	e.MoveLineEnd()
}

func (e *Editor) MoveVisualUp(width int) {
	e.MoveVisual(width, -1)
}

func (e *Editor) MoveVisualDown(width int) {
	e.MoveVisual(width, 1)
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
	e.last = ""
	if e.cursor == 0 {
		return ""
	}
	e.resetHistoryBrowse()
	e.pushUndo()
	old := e.cursor
	e.moveLeftNoBreak()
	deleted := e.text[e.cursor:old]
	e.text = e.text[:e.cursor] + e.text[old:]
	e.removePaste(deleted)
	return deleted
}

func (e *Editor) DeleteForward() string {
	e.last = ""
	if e.cursor >= len(e.text) {
		return ""
	}
	e.resetHistoryBrowse()
	e.pushUndo()
	next := e.nextSegment(e.cursor)
	deleted := e.text[e.cursor:next]
	e.text = e.text[:e.cursor] + e.text[next:]
	return deleted
}

func (e *Editor) DeleteToLineStart() string {
	if e.cursor == 0 {
		return ""
	}
	e.resetHistoryBrowse()
	e.pushUndo()
	old := e.cursor
	if e.text[e.cursor-1] == '\n' {
		e.cursor--
	} else {
		for e.cursor > 0 && e.text[e.cursor-1] != '\n' {
			e.moveLeftNoBreak()
		}
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
	if e.text[e.cursor] == '\n' {
		e.cursor++
	} else {
		for e.cursor < len(e.text) && e.text[e.cursor] != '\n' {
			_, sz := utf8.DecodeRuneInString(e.text[e.cursor:])
			e.cursor += sz
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

func (e *Editor) DeleteWordBackward() string {
	if e.cursor == 0 {
		return ""
	}
	e.resetHistoryBrowse()
	e.pushUndo()
	old := e.cursor
	e.moveWordLeftNoBreak()
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
	e.moveWordRightNoBreak()
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
	e.moveWordLeftNoBreak()
	e.breakChains()
}

func (e *Editor) moveWordLeftNoBreak() {
	if e.cursor > 0 && e.text[e.cursor-1] == '\n' {
		e.cursor--
		return
	}
	for e.cursor > 0 && e.text[e.cursor-1] != '\n' {
		r, _ := utf8.DecodeLastRuneInString(e.text[:e.cursor])
		if !unicode.IsSpace(r) {
			break
		}
		e.moveLeftNoBreak()
	}
	if start, end, ok := e.markerAt(e.cursor - 1); ok && end == e.cursor {
		e.cursor = start
		return
	}
	for e.cursor > 0 && e.text[e.cursor-1] != '\n' {
		if _, _, ok := e.markerAt(e.cursor - 1); ok {
			break
		}
		r, _ := utf8.DecodeLastRuneInString(e.text[:e.cursor])
		if unicode.IsSpace(r) {
			break
		}
		e.moveLeftNoBreak()
	}
}

func (e *Editor) MoveWordRight() {
	e.moveWordRightNoBreak()
	e.breakChains()
}

func (e *Editor) moveWordRightNoBreak() {
	if e.cursor < len(e.text) && e.text[e.cursor] == '\n' {
		e.cursor++
		return
	}
	for e.cursor < len(e.text) && e.text[e.cursor] != '\n' {
		r, sz := utf8.DecodeRuneInString(e.text[e.cursor:])
		if !unicode.IsSpace(r) {
			break
		}
		e.cursor += sz
	}
	if start, end, ok := e.markerAt(e.cursor); ok && start == e.cursor {
		e.cursor = end
		return
	}
	for e.cursor < len(e.text) && e.text[e.cursor] != '\n' {
		if _, _, ok := e.markerAt(e.cursor); ok {
			break
		}
		r, sz := utf8.DecodeRuneInString(e.text[e.cursor:])
		if unicode.IsSpace(r) {
			break
		}
		e.cursor += sz
	}
}

// HandleJumpKey consumes jump hotkeys and the next printable target.
func (e *Editor) HandleJumpKey(key event.KeyEvent) bool {
	direction := 0
	kb := keybindings.DefaultManager()
	stroke := ui.KeystrokeOf(key)
	switch {
	case kb.Matches(stroke, "tui.editor.jumpForward"):
		direction = 1
	case kb.Matches(stroke, "tui.editor.jumpBackward"):
		direction = -1
	}
	pending := e.jumpDirection
	e.jumpDirection = 0
	if direction != 0 {
		if pending == 0 {
			e.jumpDirection = direction
		}
		return true
	}
	if pending == 0 {
		return false
	}
	target, ok := ansi.TextRune(key)
	if !ok {
		return false
	}
	if pending > 0 {
		e.JumpForward(target)
	} else {
		e.JumpBackward(target)
	}
	return true
}

// CancelJump leaves any pending one-key character jump.
func (e *Editor) CancelJump() { e.jumpDirection = 0 }

func (e *Editor) JumpForward(target rune) bool {
	_, size := utf8.DecodeRuneInString(e.text[e.cursor:])
	for pos := e.cursor + size; pos < len(e.text); {
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

var pasteMarkerPattern = regexp.MustCompile(`\[paste #([0-9]+)(?: (?:[0-9]+ chars|\+[0-9]+ lines))?\]`)

func (e *Editor) removePaste(deleted string) {
	match := pasteMarkerPattern.FindStringSubmatch(deleted)
	if match == nil {
		return
	}
	target, err := strconv.Atoi(match[1])
	if err != nil {
		return
	}
	renumber := func(marker string) string {
		span := pasteMarkerPattern.FindStringSubmatchIndex(marker)
		id, err := strconv.Atoi(marker[span[2]:span[3]])
		if err != nil || id <= target {
			return marker
		}
		return marker[:span[2]] + strconv.Itoa(id-1) + marker[span[3]:]
	}
	pastes := make(map[string]string, len(e.largePastes))
	for marker, body := range e.largePastes {
		id := pasteMarkerPattern.FindStringSubmatch(marker)[1]
		if id != match[1] {
			pastes[renumber(marker)] = body
		}
	}
	e.largePastes = pastes
	e.nextPaste--
	before := pasteMarkerPattern.ReplaceAllStringFunc(e.text[:e.cursor], renumber)
	after := pasteMarkerPattern.ReplaceAllStringFunc(e.text[e.cursor:], renumber)
	e.text = before + after
	e.cursor = len(before)
}

func (e *Editor) ExpandedText() string {
	bodies := make(map[string]string, len(e.largePastes))
	var ids []string
	for marker, body := range e.largePastes {
		match := pasteMarkerPattern.FindStringSubmatch(marker)
		if match == nil {
			continue
		}
		ids = append(ids, match[1])
		bodies[match[1]] = body
	}
	slices.SortFunc(ids, func(a, b string) int {
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	})
	out := e.text
	for _, id := range ids {
		prefix := "[paste #" + id
		out = pasteMarkerPattern.ReplaceAllStringFunc(out, func(marker string) string {
			if strings.HasPrefix(marker, prefix+" ") || marker == prefix+"]" {
				return bodies[id]
			}
			return marker
		})
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
	e.cursor = e.clampSegment(off)
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

func (e *Editor) markerAt(pos int) (int, int, bool) {
	if pos < 0 || pos >= len(e.text) || len(e.largePastes) == 0 {
		return 0, 0, false
	}
	for _, span := range pasteMarkerPattern.FindAllStringSubmatchIndex(e.text, -1) {
		if pos < span[0] {
			break
		}
		if pos >= span[1] {
			continue
		}
		prefix := "[paste #" + e.text[span[2]:span[3]]
		for marker := range e.largePastes {
			if strings.HasPrefix(marker, prefix+" ") || marker == prefix+"]" {
				return span[0], span[1], true
			}
		}
	}
	return 0, 0, false
}

func (e *Editor) clampSegment(pos int) int {
	pos = text.ClampCluster(e.text, pos)
	if start, _, ok := e.markerAt(pos); ok {
		return start
	}
	return pos
}

func (e *Editor) previousSegment(pos int) int {
	if start, _, ok := e.markerAt(pos - 1); ok {
		return start
	}
	return text.PrevCluster(e.text, pos)
}

func (e *Editor) nextSegment(pos int) int {
	if _, end, ok := e.markerAt(pos); ok {
		return end
	}
	return text.NextCluster(e.text, pos)
}

func (e *Editor) moveLeftNoBreak() {
	e.cursor = e.previousSegment(e.cursor)
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
	e.historyDraft = State{}
	e.historyBrowsing = false
}

func (e *Editor) pushUndo() {
	e.undo.Push(editorSnapshot{State: State{e.text, e.cursor}, pastes: e.largePastes, pasteCounter: e.nextPaste})
}

func (e *Editor) breakChains() {
	e.last = ""
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

func isWhitespace(r rune) bool { return (unicode.IsSpace(r) && r != '\u0085') || r == '\ufeff' }

func (e *Editor) shouldPushInsertUndo(s string) bool {
	return e.last != "insert" || strings.ContainsFunc(s, isWhitespace)
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
	return text.ClampCluster(e.text, pos)
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

// MoveVisual moves by delta wrapped rows while retaining the desired display column.
func (e *Editor) MoveVisual(width, delta int) {
	if width <= 0 {
		width = 1
	}
	line, visualLine, visualCol, segments := e.visualPosition(width)
	flatIndex := 0
	for i := 0; i < line; i++ {
		flatIndex += len(segments[i])
	}
	flatIndex += visualLine
	target := max(0, min(flatIndex+delta, totalSegments(segments)-1))
	if target == flatIndex {
		return
	}
	desired := e.verticalDesiredCol(visualCol)
	for target < totalSegments(segments) {
		targetLine, targetVisualLine := locateSegment(segments, target)
		segmentLen := segments[targetLine][targetVisualLine]
		if targetVisualLine < len(segments[targetLine])-1 {
			segmentLen = max(0, segmentLen-1)
		}
		col := min(desired, segmentLen)
		lineStart := 0
		lines := e.Lines()
		for i := 0; i < targetLine; i++ {
			lineStart += len(lines[i]) + 1
		}
		segmentCol := 0
		for i := 0; i < targetVisualLine; i++ {
			segmentCol += segments[targetLine][i]
		}
		segmentStart := lineStart + text.OffsetAtColumnBias(lines[targetLine], segmentCol, text.BiasLeft)
		candidate := lineStart + text.OffsetAtColumnBias(lines[targetLine], segmentCol+col, text.BiasLeft)
		if start, _, ok := e.markerAt(candidate); ok && start < segmentStart && delta > 0 {
			target++
			continue
		}
		e.setVisualPosition(width, targetLine, targetVisualLine, col)
		break
	}

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
	for visualLine < len(segments[line])-1 && col >= segments[line][visualLine] {
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

func normalizeText(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), "\t", "    ")
}

var pasteCtrlSequence = regexp.MustCompile(`\x1b\[[0-9]+;5u`)

func normalizePasteText(s string) string {
	// tmux can encode control letters inside bracketed paste as CSI-u.
	s = pasteCtrlSequence.ReplaceAllStringFunc(s, func(sequence string) string {
		code, err := strconv.Atoi(sequence[2 : len(sequence)-3])
		if err != nil {
			return sequence
		}
		switch {
		case code >= 'a' && code <= 'z':
			return string(rune(code - 'a' + 1))
		case code >= 'A' && code <= 'Z':
			return string(rune(code - 'A' + 1))
		default:
			return sequence
		}
	})
	s = normalizeText(s)
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || !unicode.IsControl(r) {
			out = append(out, r)
		}
	}
	return string(out)
}

func needsLeadingPasteSpace(text string, cursor int, paste string) bool {
	if cursor <= 0 || paste == "" || !strings.ContainsRune("/~.", rune(paste[0])) {
		return false
	}
	prev := text[cursor-1]
	return prev >= 'a' && prev <= 'z' || prev >= 'A' && prev <= 'Z' || prev >= '0' && prev <= '9' || prev == '_'
}

func isLargePaste(s string) bool {
	return strings.Count(s, "\n")+1 > 10 || len(utf16.Encode([]rune(s))) > 1000
}

func pasteMarker(id int, s string) string {
	lines := strings.Count(s, "\n") + 1
	if lines > 10 {
		return "[paste #" + strconv.Itoa(id) + " +" + strconv.Itoa(lines) + " lines]"
	}
	return "[paste #" + strconv.Itoa(id) + " " + strconv.Itoa(len(utf16.Encode([]rune(s)))) + " chars]"
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
