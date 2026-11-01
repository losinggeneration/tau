package editor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/tau/tui/autocomplete"
	"github.com/losinggeneration/tau/tui/keybindings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEditorInsertNewlineAndSubmit(t *testing.T) {
	e := New()
	var submitted string
	e.SetOnSubmit(func(s string) { submitted = s })
	e.Insert("hello")
	e.Newline()
	e.Insert("world")
	if e.Text() != "hello\nworld" {
		t.Fatalf("text=%q", e.Text())
	}
	e.Submit()
	if submitted != "hello\nworld" {
		t.Fatalf("submitted=%q", submitted)
	}
}

func TestEditorGraphemeSafeMoveAndDelete(t *testing.T) {
	e := New()
	e.SetText("a🙂b")
	e.MoveLeft()
	if e.DeleteBackward() != "🙂" {
		t.Fatalf("text=%q cursor=%d", e.Text(), e.Cursor())
	}
	if e.Text() != "ab" {
		t.Fatalf("text=%q", e.Text())
	}
}

func TestEditorLineMovement(t *testing.T) {
	e := New()
	e.SetText("abc\ndef")
	e.SetCursor(5)
	e.MoveLineStart()
	if e.Cursor() != 4 {
		t.Fatalf("start cursor=%d", e.Cursor())
	}
	e.MoveLineEnd()
	if e.Cursor() != 7 {
		t.Fatalf("end cursor=%d", e.Cursor())
	}
}

func TestEditorWordMovementAndDeletion(t *testing.T) {
	e := New()
	e.SetText("one two three")
	e.MoveWordLeft()
	if e.Cursor() != 8 {
		t.Fatalf("word left cursor=%d", e.Cursor())
	}
	if got := e.DeleteWordBackward(); got != "two " {
		t.Fatalf("deleted=%q text=%q", got, e.Text())
	}
	e.SetCursor(0)
	if got := e.DeleteWordForward(); got != "one" {
		t.Fatalf("forward deleted=%q text=%q", got, e.Text())
	}
}

func TestEditorDeleteLineSegments(t *testing.T) {
	e := New()
	e.SetText("hello world")
	e.SetCursor(6)
	if got := e.DeleteToLineStart(); got != "hello " {
		t.Fatalf("deleted start=%q", got)
	}
	if e.Text() != "world" {
		t.Fatalf("text=%q", e.Text())
	}
	if got := e.DeleteToLineEnd(); got != "world" {
		t.Fatalf("deleted end=%q", got)
	}
}

func TestEditorLinesAndCancel(t *testing.T) {
	e := New()
	e.SetText("a\nb\n")
	lines := e.Lines()
	if len(lines) != 3 || lines[2] != "" {
		t.Fatalf("lines=%v", lines)
	}
	e.Cancel()
	if !e.Cancelled() {
		t.Fatalf("not cancelled")
	}
}

func TestEditorVerticalMovementPreservesStickyDesiredColumn(t *testing.T) {
	e := New()
	e.SetText("abcd\nef\nghijk")
	e.SetPosition(Position{0, 3})
	e.MoveDown()
	if p := e.Position(); p.Line != 1 || p.Col != 2 {
		t.Fatalf("p=%+v", p)
	}
	e.MoveDown()
	if p := e.Position(); p.Line != 2 || p.Col != 3 {
		t.Fatalf("p2=%+v", p)
	}
}

func TestEditorKillYankYankPopUndo(t *testing.T) {
	e := New()
	e.SetText("first")
	e.DeleteWordBackward()
	e.SetText("second")
	e.DeleteWordBackward()
	e.Yank()
	if e.Text() != "second" {
		t.Fatalf("yank=%q", e.Text())
	}
	e.YankPop()
	if e.Text() != "first" {
		t.Fatalf("ypop=%q", e.Text())
	}
	e.Undo()
	if e.Text() != "second" {
		t.Fatalf("undo=%q", e.Text())
	}
}

func TestEditorPasteLargeMarker(t *testing.T) {
	e := New()
	e.SetLargePasteThreshold(3)
	e.Paste("abcdef")
	if e.Text() != "[paste #1 6 chars]" {
		t.Fatalf("text=%q", e.Text())
	}
	if e.LargePastes()["[paste #1 6 chars]"] != "abcdef" {
		t.Fatalf("large paste missing")
	}
}

func TestEditorHistory(t *testing.T) {
	e := New()
	e.AddHistory("one")
	e.AddHistory("two")
	e.HistoryPrev()
	if e.Text() != "two" {
		t.Fatalf("prev=%q", e.Text())
	}
	e.HistoryPrev()
	if e.Text() != "one" {
		t.Fatalf("prev2=%q", e.Text())
	}
	e.HistoryNext()
	if e.Text() != "two" {
		t.Fatalf("next=%q", e.Text())
	}
	e.HistoryNext()
	if e.Text() != "" {
		t.Fatalf("next empty=%q", e.Text())
	}
}

func TestEditorHistoryRestoresDraft(t *testing.T) {
	e := New()
	e.AddHistory("one")
	e.AddHistory("two")
	e.SetText("draft")
	e.HistoryPrev()
	if e.Text() != "two" {
		t.Fatalf("prev=%q", e.Text())
	}
	e.HistoryNext()
	if e.Text() != "draft" {
		t.Fatalf("draft=%q", e.Text())
	}
}

func TestEditorHistoryEditResetsBrowsing(t *testing.T) {
	e := New()
	e.AddHistory("one")
	e.AddHistory("two")
	e.SetText("draft")
	e.HistoryPrev()
	e.Insert("!")
	e.HistoryNext()
	if e.Text() != "!two" {
		t.Fatalf("text=%q", e.Text())
	}
	e.HistoryPrev()
	if e.Text() != "two" {
		t.Fatalf("prev after edit=%q", e.Text())
	}
}

func TestEditorHistoryRecallStartsAtTopOfMultilineEntry(t *testing.T) {
	e := New()
	e.AddHistory("one\ntwo\nthree\nfour\nfive\nsix")
	e.SetScrollOffset(3)
	e.HistoryPrev()
	if got := e.ScrollOffset(); got != 0 {
		t.Fatalf("scroll offset=%d, want recalled history to start at top", got)
	}
}

func TestEditorVisualBoundaries(t *testing.T) {
	e := New()
	e.SetText("abcdef\ng")
	e.SetPosition(Position{0, 0})
	if !e.AtVisualTop(3) || e.AtVisualBottom(3) {
		t.Fatalf("top/bottom at start = %v/%v", e.AtVisualTop(3), e.AtVisualBottom(3))
	}
	e.SetPosition(Position{0, 4})
	if e.AtVisualTop(3) || e.AtVisualBottom(3) {
		t.Fatalf("top/bottom mid wrap = %v/%v", e.AtVisualTop(3), e.AtVisualBottom(3))
	}
	e.SetPosition(Position{1, 1})
	if !e.AtVisualBottom(3) || e.AtVisualTop(3) {
		t.Fatalf("top/bottom at end = %v/%v", e.AtVisualTop(3), e.AtVisualBottom(3))
	}
}

func TestEditorAutocompleteApply(t *testing.T) {
	e := New()
	e.SetText("/he")
	e.SetAutocomplete(&autocomplete.Suggestions{Prefix: "/he", Items: []autocomplete.Item{{Value: "help", Label: "help"}, {Value: "history", Label: "history"}}})
	e.AutocompleteNext()
	if e.AutocompleteIndex() != 1 {
		t.Fatalf("idx=%d", e.AutocompleteIndex())
	}
	e.AutocompletePrev()
	e.ApplyAutocomplete()
	if e.Text() != "/help " {
		t.Fatalf("text=%q", e.Text())
	}
	if e.Position().Col != 6 {
		t.Fatalf("pos=%+v", e.Position())
	}
}

func TestEditorVisualMovementAndScrollOffset(t *testing.T) {
	e := New()
	e.SetText("abcdef\ng")
	e.SetPosition(Position{0, 5})
	e.MoveVisualUp(3)
	if p := e.Position(); p.Line != 0 || p.Col != 2 {
		t.Fatalf("visual up pos=%+v", p)
	}
	e.MoveVisualDown(3)
	e.MoveVisualDown(3)
	if p := e.Position(); p.Line != 1 || p.Col != 1 {
		t.Fatalf("visual down pos=%+v", p)
	}
	e.SetScrollOffset(2)
	e.EnsureCursorVisible(2, 3)
	if e.ScrollOffset() != 2 {
		t.Fatalf("scroll offset=%d", e.ScrollOffset())
	}
}

func TestEditorJumpForwardBackward(t *testing.T) {
	e := New()
	e.SetText("alpha beta gamma")
	e.SetCursor(0)
	if !e.JumpForward('g') || e.Cursor() != len("alpha beta ") {
		t.Fatalf("jump forward cursor=%d", e.Cursor())
	}
	if !e.JumpBackward('b') || e.Cursor() != len("alpha ") {
		t.Fatalf("jump backward cursor=%d", e.Cursor())
	}
}

func TestEditorVisibleAutocompleteItems(t *testing.T) {
	e := New()
	e.SetAutocompleteMaxVisible(3)
	e.SetAutocomplete(&autocomplete.Suggestions{Prefix: "@", Items: []autocomplete.Item{{Value: "a"}, {Value: "b"}, {Value: "c"}, {Value: "d"}, {Value: "e"}}})
	e.AutocompleteNext()
	e.AutocompleteNext()
	e.AutocompleteNext()
	items := e.VisibleAutocompleteItems()
	if len(items) != 3 || items[0].Value != "c" || items[2].Value != "e" {
		t.Fatalf("visible items=%+v", items)
	}
}

// Tests ported from Pi's editor.test.ts, covering key invariants.

func TestPiEditorInitialState(t *testing.T) {
	e := New()
	if e.Text() != "" {
		t.Fatalf("initial text = %q, want empty", e.Text())
	}
	if pos := e.Position(); pos.Line != 0 || pos.Col != 0 {
		t.Fatalf("initial pos = %+v, want {0,0}", pos)
	}
	if len(e.Lines()) != 1 {
		t.Fatalf("initial lines = %d, want 1 (empty line)", len(e.Lines()))
	}
}

func TestPiEditorInsertText(t *testing.T) {
	e := New()
	e.Insert("hello")
	if e.Text() != "hello" {
		t.Fatalf("text = %q, want hello", e.Text())
	}
	if pos := e.Position(); pos.Line != 0 || pos.Col != 5 {
		t.Fatalf("pos = %+v, want {0,5}", pos)
	}
}

func TestPiEditorMoveLeftAndRight(t *testing.T) {
	e := New()
	e.Insert("abc")
	e.MoveLeft()
	if pos := e.Position(); pos.Col != 2 {
		t.Fatalf("after left pos = %+v, want col 2", pos)
	}
	e.MoveLeft()
	e.MoveLeft()
	if pos := e.Position(); pos.Col != 0 {
		t.Fatalf("after left x2 pos = %+v, want col 0", pos)
	}
	e.MoveLeft() // should not go negative
	if pos := e.Position(); pos.Col != 0 {
		t.Fatalf("after left at start pos = %+v, want col 0", pos)
	}
	e.MoveRight()
	if pos := e.Position(); pos.Col != 1 {
		t.Fatalf("after right pos = %+v, want col 1", pos)
	}
}

func TestPiEditorNewline(t *testing.T) {
	e := New()
	e.Insert("hello")
	e.Newline()
	e.Insert("world")
	if e.Text() != "hello\nworld" {
		t.Fatalf("text = %q, want hello\\nworld", e.Text())
	}
	pos := e.Position()
	if pos.Line != 1 || pos.Col != 5 {
		t.Fatalf("pos = %+v, want {1,5}", pos)
	}
}

func TestPiEditorDeleteBackward(t *testing.T) {
	e := New()
	e.Insert("hello")
	e.DeleteBackward()
	if e.Text() != "hell" {
		t.Fatalf("after delete = %q, want hell", e.Text())
	}
}

func TestPiEditorDeleteForward(t *testing.T) {
	e := New()
	e.Insert("hello")
	e.MoveLineStart()
	e.DeleteForward()
	if e.Text() != "ello" {
		t.Fatalf("after forward delete = %q, want ello", e.Text())
	}
}

func TestPiEditorMoveLineStartEnd(t *testing.T) {
	e := New()
	e.Insert("hello world")
	e.MoveLineStart()
	if pos := e.Position(); pos.Col != 0 {
		t.Fatalf("after line start = %+v, want col 0", pos)
	}
	e.MoveLineEnd()
	if pos := e.Position(); pos.Col != 11 {
		t.Fatalf("after line end = %+v, want col 11", pos)
	}
}

func TestPiEditorUndoEmptyStack(t *testing.T) {
	e := New()
	e.Undo()
	if e.Text() != "" {
		t.Fatalf("undo on empty = %q, want empty", e.Text())
	}
}

func TestPiEditorUndoBackspace(t *testing.T) {
	e := New()
	e.Insert("hello")
	e.DeleteBackward()
	e.Undo()
	if e.Text() != "hello" {
		t.Fatalf("after undo backspace = %q, want hello", e.Text())
	}
}

func TestPiEditorUndoForwardDelete(t *testing.T) {
	e := New()
	e.Insert("hello")
	e.MoveLineStart()
	e.DeleteForward()
	e.Undo()
	if e.Text() != "hello" {
		t.Fatalf("after undo forward delete = %q, want hello", e.Text())
	}
}

func TestPiEditorSubmitClears(t *testing.T) {
	submitted := ""
	e := New()
	e.SetOnSubmit(func(text string) { submitted = text })
	e.Insert("hello world")
	e.Submit()
	if submitted != "hello world" {
		t.Fatalf("submitted = %q, want hello world", submitted)
	}
	if e.Text() != "" {
		t.Fatalf("after submit text = %q, want empty", e.Text())
	}
}

func TestEditorSubmitEmptyResetsBeforeCallingBack(t *testing.T) {
	for _, draft := range []string{"", " \t\n", "[paste #1 4 chars]"} {
		t.Run(draft, func(t *testing.T) {
			e := New()
			e.SetText(draft)
			e.largePastes["[paste #1 4 chars]"] = " \n\t "
			e.nextPaste = 1
			e.SetAutocomplete(&autocomplete.Suggestions{Items: []autocomplete.Item{{Value: "completion"}}})
			e.SetScrollOffset(10)
			calls := 0
			e.SetOnSubmit(func(value string) {
				calls++
				assert.Empty(t, value)
				assert.Empty(t, e.Text())
				assert.Zero(t, e.Cursor())
				assert.Empty(t, e.LargePastes())
				assert.Zero(t, e.nextPaste)
				assert.False(t, e.AutocompleteVisible())
				assert.Zero(t, e.ScrollOffset())
				assert.Zero(t, e.undo.Len())
			})
			e.Submit()
			assert.Equal(t, 1, calls)
			e.Undo()
			assert.Empty(t, e.Text())
		})
	}
}

func TestPiEditorHistoryPrevNext(t *testing.T) {
	e := New()
	e.AddHistory("first")
	e.AddHistory("second")
	e.HistoryPrev()
	if e.Text() != "second" {
		t.Fatalf("history prev = %q, want second", e.Text())
	}
	e.HistoryPrev()
	if e.Text() != "first" {
		t.Fatalf("history prev x2 = %q, want first", e.Text())
	}
	e.HistoryNext()
	if e.Text() != "second" {
		t.Fatalf("history next = %q, want second", e.Text())
	}
	e.HistoryNext()
	if e.Text() != "" {
		t.Fatalf("history next at end = %q, want empty", e.Text())
	}
}

func TestPiEditorKillRingCtrlWAndYank(t *testing.T) {
	e := New()
	e.Insert("foo bar baz")
	e.MoveLineEnd()
	e.DeleteWordBackward()
	if e.Text() != "foo bar " {
		t.Fatalf("after Ctrl+W = %q, want %q", e.Text(), "foo bar ")
	}
	e.MoveLineStart()
	e.Yank()
	if e.Text() != "bazfoo bar " {
		t.Fatalf("after yank = %q, want %q", e.Text(), "bazfoo bar ")
	}
}

func TestPiEditorKillRingCtrlKAndYank(t *testing.T) {
	e := New()
	e.Insert("hello world")
	e.MoveLineStart()
	e.DeleteToLineEnd()
	if e.Text() != "" {
		t.Fatalf("after Ctrl+K = %q, want empty", e.Text())
	}
	e.Yank()
	if e.Text() != "hello world" {
		t.Fatalf("after yank = %q, want hello world", e.Text())
	}
}

func TestPiEditorPasteLargeCreatesMarker(t *testing.T) {
	e := New()
	large := ""
	for i := 0; i < 15; i++ {
		large += "line\n"
	}
	e.Paste(large)
	text := e.Text()
	if !contains(text, "[paste #") {
		t.Fatalf("large paste should create marker: %q", text)
	}
	// Expanded text should contain original paste
	if e.ExpandedText() != large {
		t.Fatalf("expanded text != original paste")
	}
}

func TestPiEditorPasteSmallInsertsDirectly(t *testing.T) {
	e := New()
	e.Paste("hello")
	if e.Text() != "hello" {
		t.Fatalf("small paste = %q, want hello", e.Text())
	}
	if contains(e.Text(), "[paste") {
		t.Fatalf("small paste should not have marker: %q", e.Text())
	}
}

func TestPiEditorPasteNormalizesTabs(t *testing.T) {
	e := New()
	e.Paste("a\tb")
	// Editor paste normalizes tabs to four spaces
	if e.Text() != "a    b" {
		t.Fatalf("paste tabs = %q, want %q", e.Text(), "a    b")
	}
}

func TestPiEditorCtrlCIgnored(t *testing.T) {
	e := New()
	e.Insert("hello")
	// Ctrl+C (cancel) should set cancelled flag but not modify text
	e.Cancel()
	if !e.Cancelled() {
		t.Fatal("cancel not set")
	}
	if e.Text() != "hello" {
		t.Fatalf("text after cancel = %q, want hello", e.Text())
	}
}

func TestPiEditorLinesAndPosition(t *testing.T) {
	e := New()
	e.SetText("line1\nline2\nline3")
	lines := e.Lines()
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	if lines[0] != "line1" || lines[1] != "line2" || lines[2] != "line3" {
		t.Fatalf("lines = %v", lines)
	}
	e.SetPosition(Position{Line: 1, Col: 3})
	pos := e.Position()
	if pos.Line != 1 || pos.Col != 3 {
		t.Fatalf("pos = %+v, want {1,3}", pos)
	}
}

func TestPiEditorUndoCoalescesWordChars(t *testing.T) {
	e := New()
	e.Insert("h")
	e.Insert("e")
	e.Insert("l")
	e.Insert("l")
	e.Insert("o")
	e.Undo()
	// Undo should restore to empty (word chars coalesced)
	if e.Text() != "" {
		t.Fatalf("after undo = %q, want empty (coalesced)", e.Text())
	}
}

func TestPiEditorVisualLineWrapping(t *testing.T) {
	// VisualLines should split long lines at width
	lines := VisualLines("abcdefghij", 4)
	if len(lines) != 3 {
		t.Fatalf("visual lines = %d, want 3 (10/4)", len(lines))
	}
	if lines[0] != "abcd" || lines[1] != "efgh" || lines[2] != "ij" {
		t.Fatalf("visual lines = %v, want [abcd efgh ij]", lines)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestEditorHistoryCapsAndDeduplicatesConsecutiveEntries(t *testing.T) {
	e := New()
	for i := range 105 {
		e.AddHistory(strconv.Itoa(i))
		e.AddHistory(" " + strconv.Itoa(i) + " ")
	}
	entries := e.HistoryEntries()
	require.Len(t, entries, 100)
	assert.Equal(t, "5", entries[0])
	assert.Equal(t, "104", entries[99])
	entries[0] = "changed"
	assert.Equal(t, "5", e.HistoryEntries()[0])
	e.AddHistory("103")
	assert.Equal(t, []string{"103", "104", "103"}, e.HistoryEntries()[97:])
	e.AddHistory(" \n ")
	require.Len(t, e.HistoryEntries(), 100)
	for range 101 {
		e.HistoryPrev()
	}
	assert.Equal(t, "6", e.Text())
}

func TestEditorHistoryRestoresDraftCursorAndUndo(t *testing.T) {
	e := New()
	e.AddHistory("old\nentry")
	e.AddHistory("new\nentry")
	e.SetText("draft 😀 tail")
	e.SetCursor(len("draft 😀"))
	e.HistoryPrev()
	assert.Equal(t, "new\nentry", e.Text())
	assert.Zero(t, e.Cursor())
	e.HistoryPrev()
	assert.Equal(t, "old\nentry", e.Text())
	assert.Zero(t, e.Cursor())
	e.HistoryNext()
	assert.Equal(t, "new\nentry", e.Text())
	assert.Equal(t, len(e.Text()), e.Cursor())
	e.HistoryNext()
	assert.Equal(t, "draft 😀 tail", e.Text())
	assert.Equal(t, len("draft 😀"), e.Cursor())
	e.HistoryPrev()
	e.HistoryPrev()
	e.Undo()
	assert.Equal(t, "draft 😀 tail", e.Text())
	assert.Equal(t, len("draft 😀"), e.Cursor())
	assert.False(t, e.historyBrowsing)
	e.HistoryNext()
	assert.Equal(t, "draft 😀 tail", e.Text())
}

func TestEditorHistoryUndoRestoresPrebrowseEditChain(t *testing.T) {
	e := New()
	e.AddHistory("entry")
	e.Insert("draft")
	e.HistoryPrev()
	e.HistoryNext()
	e.Insert("!")
	e.Undo()
	assert.Equal(t, "draft", e.Text())
	e.Undo()
	assert.Equal(t, "draft", e.Text(), "browsing takes just one snapshot")
	e.Undo()
	assert.Empty(t, e.Text(), "the edit before history remains undoable")
}

func TestEditorTypingUndoIncludesWhitespaceWithFollowingWord(t *testing.T) {
	for _, separator := range []string{" ", "\t", "\u00a0", "\ufeff"} {
		t.Run(separator, func(t *testing.T) {
			e := New()
			for _, part := range []string{"hel", "lo", separator, "wor", "ld"} {
				e.Insert(part)
			}
			e.Undo()
			assert.Equal(t, "hello", e.Text())
			e.Undo()
			assert.Empty(t, e.Text())
		})
	}
	e := New()
	e.Insert("first")
	e.Newline()
	e.Insert("next")
	e.Undo()
	assert.Equal(t, "first\n", e.Text())
	e.Undo()
	assert.Equal(t, "first", e.Text())
}

func TestEditorCharacterDeletesDoNotKillAndMovementBreaksKills(t *testing.T) {
	for _, action := range []string{"backward", "forward", "move", "undo", "boundary"} {
		t.Run(action, func(t *testing.T) {
			e := New()
			e.SetText("one two")
			e.DeleteWordBackward()
			switch action {
			case "backward":
				e.DeleteBackward()
			case "forward":
				e.SetCursor(0)
				e.DeleteForward()
				e.SetCursor(len(e.Text()))
			case "move":
				e.MoveLeft()
				e.MoveRight()
			case "undo":
				e.Undo()
			case "boundary":
				e.DeleteForward()
			}
			assert.Equal(t, []string{"two"}, e.kill.Entries())
			e.DeleteWordBackward()
			require.Len(t, e.kill.Entries(), 2)
			assert.Equal(t, "two", e.kill.Entries()[0])
		})
	}
}

func TestEditorLineKillsMergeNewlinesAndAccumulate(t *testing.T) {
	for _, direction := range []string{"backward", "forward"} {
		t.Run(direction, func(t *testing.T) {
			e := New()
			original := "α\n\n🙂omega"
			e.SetText(original)
			var deleted []string
			if direction == "backward" {
				e.SetCursor(len("α\n\n"))
				deleted = []string{e.DeleteToLineStart(), e.DeleteToLineStart(), e.DeleteToLineStart()}
				assert.Equal(t, []string{"\n", "\n", "α"}, deleted)
				assert.Equal(t, "🙂omega", e.Text())
				assert.Equal(t, 0, e.Cursor())
				assert.Equal(t, []string{"α\n\n"}, e.kill.Entries())
				assert.Empty(t, e.DeleteToLineStart())
			} else {
				e.SetCursor(len("α"))
				deleted = []string{e.DeleteToLineEnd(), e.DeleteToLineEnd(), e.DeleteToLineEnd()}
				assert.Equal(t, []string{"\n", "\n", "🙂omega"}, deleted)
				assert.Equal(t, "α", e.Text())
				assert.Equal(t, len("α"), e.Cursor())
				assert.Equal(t, []string{"\n\n🙂omega"}, e.kill.Entries())
				assert.Empty(t, e.DeleteToLineEnd())
			}
			assert.Equal(t, 4, e.undo.Len())
			e.Yank()
			assert.Equal(t, original, e.Text())
			e.Undo()
			for range 3 {
				e.Undo()
			}
			assert.Equal(t, original, e.Text())
			if direction == "backward" {
				assert.Equal(t, len("α\n\n"), e.Cursor())
			} else {
				assert.Equal(t, len("α"), e.Cursor())
			}
		})
	}
}

func TestEditorWordActionsRespectLogicalLines(t *testing.T) {
	for _, tc := range []struct {
		name, text     string
		cursor, target int
		deleted        string
		forward        bool
	}{
		{"backward newline", "α word\nnext", len("α word\n"), len("α word"), "\n", false},
		{"backward space only line", "word\n  next", len("word\n  "), len("word\n"), "  ", false},
		{"forward newline", "first\n🙂next", len("first"), len("first\n"), "\n", true},
		{"forward word", "one two", 0, len("one"), "one", true},
		{"forward leading space", "one   two", len("one"), len("one   two"), "   two", true},
		{"forward space only line", "first  \nnext", len("first"), len("first  "), "  ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New()
			e.SetText(tc.text)
			e.SetCursor(tc.cursor)
			if tc.forward {
				e.MoveWordRight()
			} else {
				e.MoveWordLeft()
			}
			assert.Equal(t, tc.target, e.Cursor())
			e.SetCursor(tc.cursor)
			var deleted string
			if tc.forward {
				deleted = e.DeleteWordForward()
			} else {
				deleted = e.DeleteWordBackward()
			}
			assert.Equal(t, tc.deleted, deleted)
			assert.Equal(t, tc.text[:min(tc.cursor, tc.target)]+tc.text[max(tc.cursor, tc.target):], e.Text())
			assert.Equal(t, min(tc.cursor, tc.target), e.Cursor())
			e.Yank()
			assert.Equal(t, tc.text, e.Text())
			e.Undo()
			e.Undo()
			assert.Equal(t, tc.text, e.Text())
			assert.Equal(t, tc.cursor, e.Cursor())
		})
	}
}

func TestEditorPasteDecodesCSIuBeforeNormalizing(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"one\x1b[106;5utwo", "one\ntwo"},
		{"one\x1b[74;5utwo", "one\ntwo"},
		{"one\x1b[109;5u\x1b[106;5utwo", "one\ntwo"},
		{"a\x1b[105;5ub\x1b[97;5uc", "a    bc"},
		{"a\x1b[57;5ub", "a[57;5ub"},
		{"a\x1b[999999999999999999999999999999999;5ub", "a[999999999999999999999999999999999;5ub"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			e := New()
			e.Paste(tc.input)
			assert.Equal(t, tc.want, e.Text())
			e.Undo()
			assert.Empty(t, e.Text())
		})
	}
}

func TestEditorPasteSpacesOnlyPathsAfterASCIIWords(t *testing.T) {
	for _, tc := range []struct{ before, paste, want string }{
		{"hello", "world", "helloworld"},
		{"hello", "/file", "hello /file"},
		{"hello", "~/file", "hello ~/file"},
		{"hello", "./file", "hello ./file"},
		{"é", "/file", "é/file"},
		{"hello/", "/file", "hello//file"},
		{"hello.", "/file", "hello./file"},
		{"x_", ".file", "x_ .file"},
		{"x1", "/file", "x1 /file"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			e := New()
			e.SetText(tc.before)
			e.Paste(tc.paste)
			assert.Equal(t, tc.want, e.Text())
			e.Undo()
			assert.Equal(t, tc.before, e.Text())
		})
	}
	e := New()
	e.SetText("hello suffix")
	e.SetCursor(5)
	e.Paste("/file")
	assert.Equal(t, "hello /file suffix", e.Text())
}

func TestEditorPasteCancelsVisibleAutocomplete(t *testing.T) {
	e := New()
	e.SetAutocomplete(&autocomplete.Suggestions{Items: []autocomplete.Item{{Value: "hello"}}})
	require.True(t, e.AutocompleteVisible())
	e.Paste("word")
	assert.False(t, e.AutocompleteVisible())
	assert.Equal(t, "word", e.Text())
}

func TestEditorLargePasteUsesUTF16CountsAndLineThreshold(t *testing.T) {
	for _, tc := range []struct{ name, body, marker string }{
		{"exact BMP threshold", strings.Repeat("é", 1000), ""},
		{"over BMP threshold", strings.Repeat("é", 1001), "[paste #1 1001 chars]"},
		{"exact astral threshold", strings.Repeat("🙂", 500), ""},
		{"over astral threshold", strings.Repeat("🙂", 501), "[paste #1 1002 chars]"},
		{"long two lines", strings.Repeat("a", 1000) + "\nb", "[paste #1 1002 chars]"},
		{"ten lines", strings.Repeat("a\n", 9) + "a", ""},
		{"eleven lines", strings.Repeat("a\n", 10) + "a", "[paste #1 +11 lines]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New()
			e.Paste(tc.body)
			if tc.marker == "" {
				assert.Equal(t, tc.body, e.Text())
				assert.Empty(t, e.LargePastes())
			} else {
				assert.Equal(t, tc.marker, e.Text())
				assert.Equal(t, map[string]string{tc.marker: tc.body}, e.LargePastes())
			}
			assert.Equal(t, tc.body, e.ExpandedText())
			e.Undo()
			assert.Empty(t, e.Text())
		})
	}
}

func TestEditorUndoRestoresPasteRegistryAndCounter(t *testing.T) {
	e := New()
	e.SetLargePasteThreshold(3)
	e.Paste("first")
	first := e.Text()
	e.Paste("second")
	assert.Len(t, e.LargePastes(), 2)
	e.Undo()
	assert.Equal(t, first, e.Text())
	assert.Equal(t, map[string]string{first: "first"}, e.LargePastes())
	assert.Equal(t, 1, e.nextPaste)
	e.Paste("third")
	assert.Equal(t, first+"[paste #2 5 chars]", e.Text())
	e.Undo()
	e.Undo()
	assert.Empty(t, e.Text())
	assert.Empty(t, e.LargePastes())
	assert.Zero(t, e.nextPaste)
	e.Paste("again")
	assert.Equal(t, "[paste #1 5 chars]", e.Text())
	e.Insert("x")
	e.largePastes[e.Text()[:len(e.Text())-1]] = "mutated"
	e.Undo()
	assert.Equal(t, "again", e.ExpandedText())
}

func TestEditorExpandsPasteMarkersInCreationOrder(t *testing.T) {
	e := New()
	e.SetLargePasteThreshold(3)
	e.Paste("first")
	first := e.Text()
	e.Paste("second")
	second := "[paste #2 6 chars]"
	body := "literal $& $` $' " + second
	e.largePastes[first] = body
	e.largePastes[second] = "nested " + first
	for range 20 {
		assert.Equal(t, "literal $& $` $' nested "+first+"nested "+first, e.ExpandedText())
	}
	e.text = "[paste #1] / [paste #2 999 chars] / [paste #3]"
	assert.Equal(t, "literal $& $` $' nested "+first+" / nested "+first+" / [paste #3]", e.ExpandedText())
}

func TestEditorSetTextNormalizesAndRestoresPasteOnUndo(t *testing.T) {
	e := New()
	e.SetLargePasteThreshold(3)
	e.Paste("original")
	marker := e.Text()
	e.SetAutocomplete(&autocomplete.Suggestions{Items: []autocomplete.Item{{Value: "hello"}}})
	e.SetText("a\r\nb\rc\td")
	assert.Equal(t, "a\nb\nc    d", e.Text())
	assert.False(t, e.AutocompleteVisible())
	assert.Empty(t, e.LargePastes())
	assert.Zero(t, e.nextPaste)
	e.Undo()
	assert.Equal(t, marker, e.Text())
	assert.Equal(t, "original", e.ExpandedText())
	assert.Equal(t, 1, e.nextPaste)
	e.Undo()
	assert.Empty(t, e.Text())
	assert.Empty(t, e.LargePastes())
}

func TestEditorSetTextDoesNotSnapshotUnchangedNormalizedText(t *testing.T) {
	e := New()
	e.Insert("a\nb")
	e.SetText("a\r\nb")
	e.Undo()
	assert.Empty(t, e.Text())
}

func TestEditorSubmitResetsTransientStateAndRetainsKillRing(t *testing.T) {
	e := New()
	e.SetLargePasteThreshold(3)
	e.SetText("killed")
	e.DeleteWordBackward()
	e.AddHistory("history")
	e.Paste("body")
	e.HistoryPrev()
	e.SetAutocomplete(&autocomplete.Suggestions{Items: []autocomplete.Item{{Value: "completion"}}})
	e.SetScrollOffset(10)
	var submitted string
	e.SetOnSubmit(func(value string) { submitted = value })
	e.Submit()
	assert.Equal(t, "history", submitted)
	assert.Empty(t, e.Text())
	assert.Zero(t, e.Cursor())
	assert.Empty(t, e.LargePastes())
	assert.Zero(t, e.nextPaste)
	assert.False(t, e.AutocompleteVisible())
	assert.False(t, e.historyBrowsing)
	assert.Zero(t, e.ScrollOffset())
	assert.Zero(t, e.undo.Len())
	assert.Equal(t, []string{"killed"}, e.kill.Entries())
	e.Paste("again")
	assert.Equal(t, "[paste #1 5 chars]", e.Text())
}

func TestEditorCompletionPreservesPasteStorageAndUndo(t *testing.T) {
	e := New()
	e.SetLargePasteThreshold(3)
	e.Paste("body")
	marker := e.Text()
	e.Insert(" /he")
	before := e.Text()
	e.SetAutocomplete(&autocomplete.Suggestions{Prefix: "/he", Items: []autocomplete.Item{{Value: "/help"}}})
	e.ApplyAutocomplete()
	assert.Equal(t, marker+" /help", e.Text())
	assert.Equal(t, "body /help", e.ExpandedText())
	assert.Equal(t, map[string]string{marker: "body"}, e.LargePastes())
	assert.Equal(t, 1, e.nextPaste)
	assert.False(t, e.AutocompleteVisible())
	e.Undo()
	assert.Equal(t, before, e.Text())
	assert.Equal(t, "body /he", e.ExpandedText())
	e.Undo()
	assert.Equal(t, marker, e.Text())
	assert.Equal(t, "body", e.ExpandedText())
}

func TestEditorAutocompleteUsesByteCoordinates(t *testing.T) {
	e := New()
	var prefix string
	provider, err := autocomplete.NewCombinedProvider(autocomplete.WithCommands([]autocomplete.SlashCommand{{Name: "open", CompleteArgs: func(_ context.Context, s string) ([]autocomplete.Item, error) {
		prefix = s
		return []autocomplete.Item{{Value: "界面", Label: "界面"}}, nil
	}}}))
	require.NoError(t, err)
	e.SetAutocompleteProvider(provider)
	e.SetText("/open 界")
	require.NoError(t, e.UpdateAutocomplete(context.Background(), false))
	assert.Equal(t, "界", prefix)
	e.ApplyAutocomplete()
	assert.Equal(t, "/open 界面", e.Text())
	assert.Equal(t, len(e.Text()), e.Cursor())
	for _, leading := range []string{"界", "👩\u200d💻", "e\u0301", "first\n界"} {
		t.Run(leading, func(t *testing.T) {
			e.SetText(leading + " @sr tail")
			e.SetCursor(len(leading + " @sr"))
			e.SetAutocomplete(&autocomplete.Suggestions{Prefix: "@sr", Items: []autocomplete.Item{{Value: "@资料/", Label: "资料/"}}})
			e.ApplyAutocomplete()
			assert.Equal(t, leading+" @资料/ tail", e.Text())
			assert.Equal(t, len(leading+" @资料/"), e.Cursor())
			e.Undo()
			assert.Equal(t, leading+" @sr tail", e.Text())
			assert.Equal(t, len(leading+" @sr"), e.Cursor())
		})
	}
}

func TestEditorTreatsKnownPasteMarkersAsAtomicSegments(t *testing.T) {
	e := New()
	e.SetLargePasteThreshold(3)
	e.Insert("A")
	e.Paste("body")
	e.Insert("B")
	marker := "[paste #1 4 chars]"
	original := e.Text()
	e.SetCursor(0)
	e.MoveRight()
	assert.Equal(t, 1, e.Cursor())
	e.MoveRight()
	assert.Equal(t, 1+len(marker), e.Cursor())
	e.MoveRight()
	assert.Equal(t, len(original), e.Cursor())
	e.MoveLeft()
	e.MoveLeft()
	assert.Equal(t, 1, e.Cursor())
	e.MoveLeft()
	assert.Zero(t, e.Cursor())
	e.SetCursor(5)
	assert.Equal(t, 1, e.Cursor())
	e.SetPosition(Position{0, 5})
	assert.Equal(t, 1, e.Cursor())
	assert.Equal(t, marker, e.DeleteForward())
	assert.Equal(t, "AB", e.Text())
	e.Undo()
	assert.Equal(t, original, e.Text())
	assert.Equal(t, "AbodyB", e.ExpandedText())
	e.SetCursor(1 + len(marker))
	assert.Equal(t, marker, e.DeleteBackward())
	assert.Equal(t, "AB", e.Text())
	e.Undo()
	assert.Equal(t, original, e.Text())
	assert.Equal(t, "AbodyB", e.ExpandedText())
	e.SetCursor(0)
	e.MoveWordRight()
	assert.Equal(t, 1, e.Cursor())
	e.MoveWordRight()
	assert.Equal(t, 1+len(marker), e.Cursor())
	e.MoveWordLeft()
	assert.Equal(t, 1, e.Cursor())
	e.MoveWordLeft()
	assert.Zero(t, e.Cursor())
	e.SetCursor(1)
	assert.Equal(t, marker, e.DeleteWordForward())
	assert.Equal(t, "AB", e.Text())
	e.Undo()
	e.SetCursor(1 + len(marker))
	assert.Equal(t, marker, e.DeleteWordBackward())
	assert.Equal(t, "AB", e.Text())
}

func TestEditorMarkerAtomicityRequiresStoredID(t *testing.T) {
	e := New()
	e.SetLargePasteThreshold(3)
	e.Paste("body")
	for _, marker := range []string{"[paste #1]", "[paste #1 999 chars]"} {
		e.setText(marker)
		e.SetCursor(0)
		e.MoveRight()
		assert.Equal(t, len(marker), e.Cursor())
	}
	e.setText("[paste #99]")
	e.SetCursor(0)
	e.MoveRight()
	assert.Equal(t, 1, e.Cursor())
	assert.Equal(t, "p", e.DeleteForward())
	assert.Equal(t, "[aste #99]", e.Text())
}

func TestEditorBackspaceRenumbersPasteRegistryAndUndo(t *testing.T) {
	e := New()
	e.SetLargePasteThreshold(3)
	e.Paste("alpha")
	e.SetCursor(0)
	e.Paste("beta")
	e.SetCursor(0)
	e.Paste("gamma")
	original := e.Text()
	registry := e.LargePastes()
	e.SetCursor(len(original))
	assert.Equal(t, "[paste #1 5 chars]", e.DeleteBackward())
	assert.Equal(t, "[paste #2 5 chars][paste #1 4 chars]", e.Text())
	assert.Equal(t, len(e.Text()), e.Cursor())
	assert.Equal(t, "gammabeta", e.ExpandedText())
	assert.Equal(t, map[string]string{"[paste #1 4 chars]": "beta", "[paste #2 5 chars]": "gamma"}, e.LargePastes())
	assert.Equal(t, 2, e.nextPaste)
	e.Undo()
	assert.Equal(t, original, e.Text())
	assert.Equal(t, registry, e.LargePastes())
	assert.Equal(t, 3, e.nextPaste)
	e.DeleteBackward()
	e.Paste("delta")
	assert.Equal(t, "gammabetadelta", e.ExpandedText())
	assert.Equal(t, 3, e.nextPaste)
}

func TestEditorBackspaceRenumbersMarkerLabelsAndCursor(t *testing.T) {
	e := New()
	e.SetLargePasteThreshold(3)
	for range 10 {
		e.Paste("body")
	}
	e.setText("[paste #10]\n[paste #2 999 chars][paste #1] tail [paste #10 +20 lines][paste #99]")
	e.SetCursor(len("[paste #10]\n[paste #2 999 chars][paste #1]"))
	assert.Equal(t, "[paste #1]", e.DeleteBackward())
	assert.Equal(t, "[paste #9]\n[paste #1 999 chars] tail [paste #9 +20 lines][paste #98]", e.Text())
	assert.Equal(t, len("[paste #9]\n[paste #1 999 chars]"), e.Cursor())
	assert.Equal(t, 9, len(e.LargePastes()))
	assert.Equal(t, 9, e.nextPaste)
	assert.Equal(t, "body\nbody tail body[paste #98]", e.ExpandedText())
}

func TestEditorForwardAndWordDeletionKeepPasteIDsForYank(t *testing.T) {
	for _, remove := range []func(*Editor) string{(*Editor).DeleteForward, (*Editor).DeleteWordForward} {
		e := New()
		e.SetLargePasteThreshold(3)
		e.Paste("body")
		e.SetCursor(0)
		assert.Equal(t, "[paste #1 4 chars]", remove(e))
		assert.Equal(t, map[string]string{"[paste #1 4 chars]": "body"}, e.LargePastes())
		assert.Equal(t, 1, e.nextPaste)
	}
	e := New()
	e.SetLargePasteThreshold(3)
	e.Paste("body")
	e.SetCursor(0)
	e.DeleteWordForward()
	e.Yank()
	assert.Equal(t, "body", e.ExpandedText())
}

func TestEditorResetDiscardsDraftAndUndoButRetainsHistoryAndKills(t *testing.T) {
	e := New()
	e.AddHistory("saved")
	e.SetText("killed")
	e.DeleteWordBackward()
	e.Paste(strings.Repeat("line\n", 20))
	e.SetScrollOffset(12)
	e.SetAutocomplete(&autocomplete.Suggestions{Items: []autocomplete.Item{{Value: "value"}}})
	e.Reset()
	assert.Empty(t, e.Text())
	assert.Zero(t, e.Cursor())
	assert.Empty(t, e.LargePastes())
	assert.Zero(t, e.nextPaste)
	assert.False(t, e.AutocompleteVisible())
	assert.Zero(t, e.ScrollOffset())
	assert.False(t, e.historyBrowsing)
	e.Undo()
	assert.Empty(t, e.Text())
	e.HistoryPrev()
	assert.Equal(t, "saved", e.Text())
	e.Reset()
	e.Yank()
	assert.Equal(t, "killed", e.Text())
}

func TestEditorHistoryRetainsDraftPasteRegistryAndCounter(t *testing.T) {
	for _, restore := range []string{"next", "undo"} {
		t.Run(restore, func(t *testing.T) {
			e := New()
			e.AddHistory("older")
			e.AddHistory("newer")
			first := strings.Repeat("a", 1001)
			second := strings.Repeat("b", 1002)
			e.Paste(first)
			e.Insert(" tail")
			e.SetCursor(len(e.Text()) - 2)
			draft, cursor, registry := e.Text(), e.Cursor(), e.LargePastes()
			e.HistoryPrev()
			e.HistoryPrev()
			assert.Equal(t, registry, e.LargePastes())
			assert.Equal(t, 1, e.nextPaste)
			if restore == "undo" {
				e.Undo()
			} else {
				e.HistoryNext()
				e.HistoryNext()
			}
			assert.Equal(t, draft, e.Text())
			assert.Equal(t, cursor, e.Cursor())
			assert.Equal(t, first+" tail", e.ExpandedText())
			assert.Equal(t, registry, e.LargePastes())
			assert.Equal(t, 1, e.nextPaste)
			e.SetCursor(len(e.Text()))
			e.Paste(second)
			assert.Equal(t, 2, e.nextPaste)
			assert.Equal(t, first+" tail"+second, e.ExpandedText())
			e.Undo()
			assert.Equal(t, draft, e.Text())
			assert.Equal(t, registry, e.LargePastes())
			assert.Equal(t, 1, e.nextPaste)
		})
	}
}

func TestEditorWrappedBoundaryBelongsToFollowingVisualLine(t *testing.T) {
	for _, value := range []string{"abcdef", "界ab界"} {
		e := New()
		e.SetText(value)
		e.SetCursor(3)
		if value == "界ab界" {
			e.SetCursor(len("界a"))
		}
		assert.False(t, e.AtVisualTop(3), value)
		assert.True(t, e.AtVisualBottom(3), value)
		e.MoveVisualUp(3)
		assert.Zero(t, e.Cursor(), value)
		e.MoveVisualDown(3)
		if value == "abcdef" {
			assert.Equal(t, 3, e.Cursor())
		} else {
			assert.Equal(t, len("界a"), e.Cursor())
		}
	}
}

func TestEditorVisualMovementClampsWithinNonfinalSegment(t *testing.T) {
	e := New()
	e.SetText("ab\nabcdef")
	e.SetPosition(Position{0, 2})
	e.MoveVisualDown(2)
	assert.Equal(t, Position{1, 1}, e.Position())
	assert.False(t, e.AtVisualBottom(2))
	e.MoveVisualDown(2)
	assert.Equal(t, Position{1, 3}, e.Position())
	e.MoveVisualDown(2)
	assert.Equal(t, Position{1, 6}, e.Position())
	e.MoveVisualUp(2)
	assert.Equal(t, Position{1, 3}, e.Position())
}

func TestEditorVisualMovementSkipsPasteContinuationRows(t *testing.T) {
	for _, col := range []int{3, 10} {
		t.Run(strconv.Itoa(col), func(t *testing.T) {
			e := New()
			e.Insert("abcdefgh")
			e.Paste(strings.Repeat("x", 2000))
			e.Insert("tail\n123456789012345678")
			e.SetPosition(Position{0, col})
			if col == 10 {
				e.SetPosition(Position{1, 10})
			}
			if col == 3 {
				e.MoveVisualDown(20)
				assert.Equal(t, Position{1, 3}, e.Position())
				e.MoveVisualUp(20)
				assert.Equal(t, Position{0, 8}, e.Position())
				e.MoveLeft()
				assert.Equal(t, Position{0, 7}, e.Position())
			} else {
				e.MoveVisualUp(20)
				assert.Equal(t, Position{0, 30}, e.Position())
				e.MoveVisualUp(20)
				assert.Equal(t, Position{0, 8}, e.Position())
				e.MoveVisualDown(20)
				assert.Equal(t, Position{0, 30}, e.Position())
			}
		})
	}
}

func TestEditorVisualMovementSkipsMultipleMarkerRowsAndKeepsColumn(t *testing.T) {
	e := New()
	e.Insert("abcdefgh")
	e.Paste(strings.Repeat("x", 2000))
	e.Insert("\n12345678")
	e.SetCursor(3)
	e.MoveVisualDown(8)
	assert.Equal(t, 8, e.Cursor())
	e.MoveVisualDown(8)
	assert.Equal(t, Position{1, 3}, e.Position())
	e.MoveVisualUp(8)
	assert.Equal(t, Position{0, 8}, e.Position())
	e.MoveVisualUp(8)
	assert.Equal(t, Position{0, 3}, e.Position())
}

func TestEditorSelectionClampsToGraphemeBoundaries(t *testing.T) {
	for _, cluster := range []string{"e\u0301", "👩\u200d💻", "👍🏽", "🇬🇧"} {
		for offset := 1; offset < len(cluster); offset++ {
			e := New()
			e.SetText("a" + cluster + "b")
			e.SetSelection(1+offset, 1+len(cluster))
			assert.Equal(t, cluster, e.CopySelection(), "%q offset %d", cluster, offset)
			assert.Equal(t, cluster, e.CutSelection())
			assert.Equal(t, "ab", e.Text())
			e.Undo()
			assert.Equal(t, "a"+cluster+"b", e.Text())
		}
	}
}

func TestEditorJumpForwardSkipsCurrentOccurrence(t *testing.T) {
	e := New()
	e.SetText("o界o\nnext o")
	e.SetCursor(0)
	assert.True(t, e.JumpForward('o'))
	assert.Equal(t, len("o界"), e.Cursor())
	assert.True(t, e.JumpForward('o'))
	assert.Equal(t, len("o界o\nnext "), e.Cursor())
	before := e.Cursor()
	assert.False(t, e.JumpForward('o'))
	assert.Equal(t, before, e.Cursor())
	assert.True(t, e.JumpBackward('o'))
	assert.Equal(t, len("o界"), e.Cursor())
	assert.True(t, e.JumpBackward('o'))
	assert.Zero(t, e.Cursor())
	assert.False(t, e.JumpBackward('o'))
	assert.Zero(t, e.Cursor())
	e.SetCursor(len("o"))
	assert.False(t, e.JumpForward('界'))
	assert.Equal(t, len("o"), e.Cursor())
}

func TestEditorJumpKeyCancellationAndReset(t *testing.T) {
	e := New()
	e.SetText("a+a")
	e.SetCursor(0)
	forward := event.KeyEvent{Key: event.KeyRune, Rune: ']', Mod: event.ModCtrl}
	backward := forward
	backward.Mod |= event.ModAlt
	assert.True(t, e.HandleJumpKey(forward))
	assert.True(t, e.HandleJumpKey(backward))
	assert.False(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyRune, Rune: 'a'}))
	assert.Zero(t, e.Cursor())
	assert.True(t, e.HandleJumpKey(forward))
	assert.False(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyEsc}))
	assert.False(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyRune, Rune: 'a'}))
	assert.True(t, e.HandleJumpKey(forward))
	e.SetText("a+a")
	e.SetCursor(0)
	assert.False(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyRune, Rune: 'a'}))
	assert.True(t, e.HandleJumpKey(forward))
	e.Reset()
	assert.False(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyRune, Rune: 'a'}))
	e.SetText("a+a")
	e.SetCursor(0)
	e.HandleJumpKey(forward)
	assert.True(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyRune, Rune: '=', ShiftedRune: '+', Mod: event.ModShift}))
	assert.Equal(t, 1, e.Cursor())
}

func TestEditorJumpUsesConfiguredBindings(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{
		"tui.editor.jumpForward": {"f2"}, "tui.editor.jumpBackward": {"f3"},
	}))
	e := New()
	e.SetText("x界x界")
	e.SetCursor(0)
	assert.False(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyRune, Rune: ']', Mod: event.ModCtrl}))
	assert.True(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyF2}))
	assert.True(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyRune, Rune: '界'}))
	assert.Equal(t, 1, e.Cursor())
	e.SetCursor(len("x界x"))
	assert.True(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyF3}))
	assert.True(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyRune, Rune: '界'}))
	assert.Equal(t, 1, e.Cursor())
	assert.True(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyF2}))
	assert.True(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyF3}))
	assert.False(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyRune, Rune: 'x'}))
	assert.Equal(t, 1, e.Cursor())
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.editor.jumpForward": {}}))
	assert.False(t, e.HandleJumpKey(event.KeyEvent{Key: event.KeyRune, Rune: ']', Mod: event.ModCtrl}))
}

type suggestionFunc func(context.Context, []string, int, int, bool) (*autocomplete.Suggestions, error)

func (f suggestionFunc) GetSuggestions(ctx context.Context, lines []string, line, col int, force bool) (*autocomplete.Suggestions, error) {
	return f(ctx, lines, line, col, force)
}

func TestEditorAutocompleteRejectsInvalidatedRequest(t *testing.T) {
	for _, mode := range []string{"text", "cursor", "cancel", "paste", "provider", "context", "nested"} {
		t.Run(mode, func(t *testing.T) {
			e := New()
			e.SetText("/he")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.SetAutocompleteProvider(suggestionFunc(func(_ context.Context, _ []string, _, _ int, _ bool) (*autocomplete.Suggestions, error) {
				switch mode {
				case "text":
					e.SetText("/other")
				case "cursor":
					e.SetCursor(0)
				case "cancel":
					e.CancelAutocomplete()
				case "paste":
					e.Paste("new")
				case "provider":
					e.SetAutocompleteProvider(nil)
				case "context":
					cancel()
				case "nested":
					e.SetAutocompleteProvider(suggestionFunc(func(context.Context, []string, int, int, bool) (*autocomplete.Suggestions, error) {
						return &autocomplete.Suggestions{Prefix: "/he", Items: []autocomplete.Item{{Value: "latest"}}}, nil
					}))
					require.NoError(t, e.UpdateAutocomplete(ctx, false))
				}
				return &autocomplete.Suggestions{Prefix: "/he", Items: []autocomplete.Item{{Value: "stale"}}}, nil
			}))
			err := e.UpdateAutocomplete(ctx, false)
			if mode == "context" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			if mode == "nested" {
				require.True(t, e.AutocompleteVisible())
				assert.Equal(t, "latest", e.VisibleAutocompleteItems()[0].Value)
			} else {
				assert.False(t, e.AutocompleteVisible())
			}
		})
	}
}

func TestEditorAutocompleteDoesNotCallProviderAfterCancellation(t *testing.T) {
	e := New()
	e.SetText("/he")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	e.SetAutocompleteProvider(suggestionFunc(func(context.Context, []string, int, int, bool) (*autocomplete.Suggestions, error) {
		called = true
		return nil, nil
	}))
	require.ErrorIs(t, e.UpdateAutocomplete(ctx, false), context.Canceled)
	assert.False(t, called)
	assert.False(t, e.AutocompleteVisible())
}

func TestEditorEmptyPasteCancelsCompletionAndBreaksTyping(t *testing.T) {
	for _, payload := range []string{"", "\x00\x07"} {
		t.Run(payload, func(t *testing.T) {
			e := New()
			e.Insert("first")
			e.SetAutocomplete(&autocomplete.Suggestions{Items: []autocomplete.Item{{Value: "completion"}}})
			e.Paste(payload)
			assert.False(t, e.AutocompleteVisible())
			assert.Equal(t, "first", e.Text())
			e.Insert("second")
			e.Undo()
			assert.Equal(t, "first", e.Text())
			e.Undo()
			assert.Equal(t, "first", e.Text())
			e.Undo()
			assert.Empty(t, e.Text())
		})
	}
	e := New()
	e.AddHistory("history")
	e.SetText("draft")
	e.HistoryPrev()
	e.Paste("")
	assert.False(t, e.historyBrowsing)
	assert.Equal(t, "history", e.Text())
}

func TestEditorAutocompleteFocusPrefersExactThenPrefixValue(t *testing.T) {
	for _, tc := range []struct {
		prefix string
		values []string
		want   int
	}{{"arg", []string{"arg-more", "arg"}, 1}, {"arg", []string{"other", "argument"}, 1}, {"Arg", []string{"other", "argument"}, 0}, {"", []string{"other", "argument"}, 0}, {"missing", []string{"other", "argument"}, 0}} {
		e := New()
		s := &autocomplete.Suggestions{Prefix: tc.prefix}
		for _, value := range tc.values {
			s.Items = append(s.Items, autocomplete.Item{Value: value, Label: "arg"})
		}
		e.SetAutocomplete(s)
		assert.Equal(t, tc.want, e.AutocompleteIndex(), "prefix=%q", tc.prefix)
	}
}

func TestEditorTabCompletionAdmissionAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		text         string
		cursor       int
		force, admit bool
	}{{"/hel", 4, false, true}, {"\ufeff/hel", 7, false, true}, {"/help arg", 9, true, true}, {"/help ", 6, true, true}, {"later\n/hel", 10, true, true}, {"later\n./no", 10, true, true}} {
		t.Run(tc.text, func(t *testing.T) {
			e := New()
			e.SetText(tc.text)
			e.SetCursor(tc.cursor)
			calls := 0
			e.SetAutocompleteProvider(suggestionFunc(func(_ context.Context, lines []string, line, col int, force bool) (*autocomplete.Suggestions, error) {
				calls++
				assert.Equal(t, tc.force, force)
				assert.Equal(t, strings.Split(tc.text, "\n"), lines)
				assert.Equal(t, strings.Count(tc.text[:tc.cursor], "\n"), line)
				assert.Equal(t, tc.cursor-strings.LastIndexByte(tc.text[:tc.cursor], '\n')-1, col)
				return nil, nil
			}))
			require.NoError(t, e.CompleteOnTab(context.Background()))
			assert.Equal(t, tc.admit, calls == 1)
		})
	}
	for _, mode := range []string{"nested", "cancel", "error"} {
		t.Run(mode, func(t *testing.T) {
			e := New()
			e.SetText("./no")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("provider failed")
			e.SetAutocompleteProvider(suggestionFunc(func(context.Context, []string, int, int, bool) (*autocomplete.Suggestions, error) {
				switch mode {
				case "nested":
					e.SetAutocompleteProvider(suggestionFunc(func(context.Context, []string, int, int, bool) (*autocomplete.Suggestions, error) {
						return &autocomplete.Suggestions{Prefix: "./no", Items: []autocomplete.Item{{Value: "./new"}}}, nil
					}))
					require.NoError(t, e.UpdateAutocomplete(ctx, true))
				case "cancel":
					cancel()
				case "error":
					return nil, failure
				}
				return &autocomplete.Suggestions{Prefix: "./no", Items: []autocomplete.Item{{Value: "./stale"}}}, nil
			}))
			err := e.CompleteOnTab(ctx)
			switch mode {
			case "nested":
				require.NoError(t, err)
				require.True(t, e.AutocompleteVisible())
				assert.Equal(t, "./new", e.VisibleAutocompleteItems()[0].Value)
			case "cancel":
				require.ErrorIs(t, err, context.Canceled)
			case "error":
				require.ErrorIs(t, err, failure)
			}
			assert.Equal(t, "./no", e.Text())
		})
	}
}

func TestEditorTabDoesNotApplyOldListAfterProviderMovesCursor(t *testing.T) {
	e := New()
	e.SetText("./no")
	e.SetAutocompleteProvider(suggestionFunc(func(context.Context, []string, int, int, bool) (*autocomplete.Suggestions, error) {
		e.SetCursor(0)
		return &autocomplete.Suggestions{Prefix: "./no", Items: []autocomplete.Item{{Value: "./new"}}}, nil
	}))
	e.SetAutocomplete(&autocomplete.Suggestions{Prefix: "./no", Items: []autocomplete.Item{{Value: "./old"}}})
	require.NoError(t, e.CompleteOnTab(t.Context()))
	assert.Equal(t, "./no", e.Text())
	assert.Zero(t, e.Cursor())
}

func TestEditorRefreshDropsListWhenProviderChangesPosition(t *testing.T) {
	for _, mode := range []string{"text", "cursor"} {
		t.Run(mode, func(t *testing.T) {
			e := New()
			e.SetText("./no")
			e.SetAutocompleteProvider(suggestionFunc(func(context.Context, []string, int, int, bool) (*autocomplete.Suggestions, error) {
				if mode == "text" {
					e.Insert("t")
				} else {
					e.SetCursor(0)
				}
				return &autocomplete.Suggestions{Prefix: "./no", Items: []autocomplete.Item{{Value: "./new"}}}, nil
			}))
			e.SetAutocomplete(&autocomplete.Suggestions{Prefix: "./no", Items: []autocomplete.Item{{Value: "./old"}}})
			require.NoError(t, e.RefreshAutocomplete(t.Context()))
			assert.False(t, e.AutocompleteVisible())
			if mode == "text" {
				assert.Equal(t, "./not", e.Text())
			} else {
				assert.Equal(t, "./no", e.Text())
				assert.Zero(t, e.Cursor())
			}
		})
	}
}

func TestEditorRefreshPreservesProviderErrorIdentity(t *testing.T) {
	e := New()
	e.SetText("./no")
	failure := errors.New("completion unavailable")
	calls := 0
	e.SetAutocompleteProvider(suggestionFunc(func(context.Context, []string, int, int, bool) (*autocomplete.Suggestions, error) {
		calls++
		return nil, failure
	}))
	require.NoError(t, e.RefreshAutocomplete(t.Context()))
	assert.Zero(t, calls)
	e.SetAutocomplete(&autocomplete.Suggestions{Prefix: "./no", Items: []autocomplete.Item{{Value: "./old"}}})
	require.ErrorIs(t, e.RefreshAutocomplete(t.Context()), failure)
	assert.Equal(t, 1, calls)
	assert.Equal(t, "./no", e.Text())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, e.RefreshAutocomplete(ctx), context.Canceled)
	assert.Equal(t, 1, calls)
}

func TestEditorNewlineCancelsCompletionAndPendingResult(t *testing.T) {
	for _, mode := range []string{"visible", "pending"} {
		t.Run(mode, func(t *testing.T) {
			e := New()
			e.SetText("/he")
			if mode == "visible" {
				e.SetAutocomplete(&autocomplete.Suggestions{Prefix: "/he", Items: []autocomplete.Item{{Value: "help"}}})
				e.Newline()
			} else {
				e.SetAutocompleteProvider(suggestionFunc(func(context.Context, []string, int, int, bool) (*autocomplete.Suggestions, error) {
					e.Newline()
					return &autocomplete.Suggestions{Prefix: "/he", Items: []autocomplete.Item{{Value: "help"}}}, nil
				}))
				require.NoError(t, e.UpdateAutocomplete(t.Context(), false))
			}
			assert.False(t, e.AutocompleteVisible())
			assert.Equal(t, "/he\n", e.Text())
			e.Undo()
			assert.Equal(t, "/he", e.Text())
			assert.False(t, e.AutocompleteVisible())
		})
	}
}

func TestEditorAutocompleteCancelsProviderContext(t *testing.T) {
	for _, mode := range []string{"cancel", "paste", "text", "provider", "nested"} {
		t.Run(mode, func(t *testing.T) {
			e := New()
			e.SetText("/he")
			nested := false
			e.SetAutocompleteProvider(suggestionFunc(func(ctx context.Context, _ []string, _, _ int, _ bool) (*autocomplete.Suggestions, error) {
				if nested {
					return &autocomplete.Suggestions{Prefix: "/he", Items: []autocomplete.Item{{Value: "latest"}}}, nil
				}
				switch mode {
				case "cancel":
					e.CancelAutocomplete()
				case "paste":
					e.Paste("x")
				case "text":
					e.SetText("replacement")
				case "provider":
					e.SetAutocompleteProvider(nil)
				case "nested":
					nested = true
					require.NoError(t, e.UpdateAutocomplete(t.Context(), false))
				}
				assert.ErrorIs(t, ctx.Err(), context.Canceled)
				return &autocomplete.Suggestions{Prefix: "/he", Items: []autocomplete.Item{{Value: "stale"}}}, nil
			}))
			require.NoError(t, e.UpdateAutocomplete(t.Context(), false))
			if mode == "nested" {
				require.True(t, e.AutocompleteVisible())
				assert.Equal(t, "latest", e.VisibleAutocompleteItems()[0].Value)
			} else {
				assert.False(t, e.AutocompleteVisible())
			}
		})
	}
}

func TestEditorApplyingCompletionCancelsProviderRequest(t *testing.T) {
	e := New()
	e.SetText("@no")
	e.SetAutocompleteProvider(suggestionFunc(func(ctx context.Context, _ []string, _, _ int, _ bool) (*autocomplete.Suggestions, error) {
		e.ApplyAutocomplete()
		assert.ErrorIs(t, ctx.Err(), context.Canceled)
		return &autocomplete.Suggestions{Prefix: "@no", Items: []autocomplete.Item{{Value: "@stale", Label: "stale"}}}, nil
	}))
	e.SetAutocomplete(&autocomplete.Suggestions{Prefix: "@no", Items: []autocomplete.Item{{Value: "@note.md", Label: "note.md"}}})
	require.NoError(t, e.UpdateAutocomplete(t.Context(), false))
	assert.False(t, e.AutocompleteVisible())
	assert.Equal(t, "@note.md ", e.Text())
	e.Undo()
	assert.Equal(t, "@no", e.Text())
}

type admissionSuggestionProvider struct {
	suggestionFunc
	allow  bool
	checks int
}

func (p *admissionSuggestionProvider) ShouldTriggerFileCompletion(_ []string, _, _ int) bool {
	p.checks++
	return p.allow
}

func TestEditorTabUsesProviderFileAdmission(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(strconv.FormatBool(allow), func(t *testing.T) {
			e := New()
			e.SetText("plain")
			calls := 0
			provider := &admissionSuggestionProvider{allow: allow, suggestionFunc: func(_ context.Context, _ []string, _, _ int, force bool) (*autocomplete.Suggestions, error) {
				calls++
				assert.True(t, force)
				return &autocomplete.Suggestions{Prefix: "plain", Items: []autocomplete.Item{{Value: "plain.md", Label: "plain.md"}}}, nil
			}}
			e.SetAutocompleteProvider(provider)
			require.NoError(t, e.CompleteOnTab(t.Context()))
			assert.Equal(t, 1, provider.checks)
			if allow {
				assert.Equal(t, 1, calls)
				assert.Equal(t, "plain.md", e.Text())
			} else {
				assert.Zero(t, calls)
				assert.Equal(t, "plain", e.Text())
			}
		})
	}
	e := New()
	e.SetText("first\n/hel")
	calls := 0
	e.SetAutocompleteProvider(suggestionFunc(func(_ context.Context, _ []string, _, _ int, force bool) (*autocomplete.Suggestions, error) {
		calls++
		assert.True(t, force)
		return &autocomplete.Suggestions{Prefix: "/hel", Items: []autocomplete.Item{{Value: "hello", Label: "hello"}}}, nil
	}))
	require.NoError(t, e.CompleteOnTab(t.Context()))
	assert.Equal(t, 1, calls)
	assert.Equal(t, "first\n/hello ", e.Text())
}

type applicationSuggestionProvider struct {
	suggestionFunc
	apply func([]string, int, int, autocomplete.Item, string) autocomplete.ApplyResult
}

func (p applicationSuggestionProvider) ApplyCompletion(lines []string, line, col int, item autocomplete.Item, prefix string) autocomplete.ApplyResult {
	return p.apply(lines, line, col, item, prefix)
}

func TestEditorUsesProviderCompletionApplication(t *testing.T) {
	for _, tab := range []bool{false, true} {
		t.Run(strconv.FormatBool(tab), func(t *testing.T) {
			e := New()
			e.Paste(strings.Repeat("x", 1100))
			marker := e.Text()
			e.Insert("\nask #na suffix")
			e.SetCursor(len(marker) + len("\nask #na"))
			before, cursor := e.Text(), e.Cursor()
			calls := 0
			item := autocomplete.Item{Value: "internal-id", Label: "display name"}
			e.SetAutocompleteProvider(applicationSuggestionProvider{suggestionFunc: func(_ context.Context, lines []string, line, col int, force bool) (*autocomplete.Suggestions, error) {
				assert.Equal(t, tab, force)
				return &autocomplete.Suggestions{Prefix: "#na", Items: []autocomplete.Item{item}}, nil
			}, apply: func(lines []string, line, col int, selected autocomplete.Item, prefix string) autocomplete.ApplyResult {
				calls++
				assert.Equal(t, []string{marker, "ask #na suffix"}, lines)
				assert.Equal(t, 1, line)
				assert.Equal(t, len("ask #na"), col)
				assert.Equal(t, item, selected)
				assert.Equal(t, "#na", prefix)
				lines[1] = "ask 名前 suffix"
				return autocomplete.ApplyResult{Lines: lines, CursorLine: 1, CursorCol: len("ask 名前")}
			}})
			if tab {
				require.NoError(t, e.CompleteOnTab(t.Context()))
			} else {
				require.NoError(t, e.UpdateAutocomplete(t.Context(), false))
				e.ApplyAutocomplete()
			}
			assert.Equal(t, 1, calls)
			assert.Equal(t, marker+"\nask 名前 suffix", e.Text())
			assert.Equal(t, len(marker)+len("\nask 名前"), e.Cursor())
			assert.Equal(t, strings.Repeat("x", 1100)+"\nask 名前 suffix", e.ExpandedText())
			assert.False(t, e.AutocompleteVisible())
			e.Undo()
			assert.Equal(t, before, e.Text())
			assert.Equal(t, cursor, e.Cursor())
			assert.Equal(t, strings.Repeat("x", 1100)+"\nask #na suffix", e.ExpandedText())
		})
	}
}

func TestEditorHistoryBoundaryBreaksTypingUndo(t *testing.T) {
	for _, mode := range []string{"empty previous", "empty next", "next from draft"} {
		t.Run(mode, func(t *testing.T) {
			e := New()
			if mode == "next from draft" {
				e.AddHistory("entry")
			}
			e.Insert("before")
			if mode == "empty previous" {
				e.HistoryPrev()
			} else {
				e.HistoryNext()
			}
			e.Insert("after")
			e.Undo()
			assert.Equal(t, "before", e.Text())
			e.Undo()
			assert.Empty(t, e.Text())
		})
	}
}

func TestEditorCompletionRetriesSharedCJKCharacters(t *testing.T) {
	e := New()
	for _, r := range "ˇˉ˙\u0305\u0323⿰〆〓〪〱〷〼゙゛ー㆐㇀㇯㈠㉇㊀㋀㋿㍘㍻㏠꜀ｰﾞ𝍠🉐" {
		assert.True(t, e.IsAutocompleteTrigger(r), "U+%04X", r)
	}
	for _, r := range "éΩ🙂⁂" {
		assert.False(t, e.IsAutocompleteTrigger(r), "U+%04X", r)
	}
}

func TestEditorHistoryTrimsJavaScriptWhitespace(t *testing.T) {
	e := New()
	e.AddHistory("\ufeff first \ufeff")
	e.AddHistory("first")
	e.AddHistory("\ufeff \u00a0")
	e.AddHistory("\u0085")
	assert.Equal(t, []string{"first", "\u0085"}, e.HistoryEntries())
	e.HistoryPrev()
	assert.Equal(t, "\u0085", e.Text())
	e.HistoryPrev()
	assert.Equal(t, "first", e.Text())
}

func TestEditorSubmitTrimsJavaScriptWhitespace(t *testing.T) {
	for _, tc := range []struct{ draft, want string }{{"\ufeff message \ufeff", "message"}, {"\ufeff\u00a0", ""}, {"\u0085", "\u0085"}} {
		t.Run(tc.draft, func(t *testing.T) {
			e := New()
			e.SetText(tc.draft)
			calls := 0
			e.SetOnSubmit(func(value string) { calls++; assert.Equal(t, tc.want, value); assert.Empty(t, e.Text()) })
			e.Submit()
			assert.Equal(t, 1, calls)
			assert.Empty(t, e.Text())
		})
	}
}

func TestEditorProgrammaticInsertionNormalizesAndStaysAtomic(t *testing.T) {
	for _, tc := range []struct{ insert, want string }{
		{"/tmp/image.png", "/tmp/image.png"}, {"line1\nline2\nline3", "line1\nline2\nline3"}, {"界\r\n🙂\r\tend", "界\n🙂\n    end"},
	} {
		t.Run(tc.insert, func(t *testing.T) {
			e := New()
			e.SetText("hello world")
			e.SetCursor(len("hello"))
			e.Insert("X")
			before := e.Text()
			cursor := e.Cursor()
			e.SetAutocomplete(&autocomplete.Suggestions{Prefix: "X", Items: []autocomplete.Item{{Value: "choice"}}})
			e.InsertTextAtCursor(tc.insert)
			assert.Equal(t, "helloX"+tc.want+" world", e.Text())
			assert.Equal(t, cursor+len(tc.want), e.Cursor())
			assert.False(t, e.AutocompleteVisible())
			e.Insert("Y")
			e.Undo()
			assert.Equal(t, "helloX"+tc.want+" world", e.Text())
			e.Undo()
			assert.Equal(t, before, e.Text())
			assert.Equal(t, cursor, e.Cursor())
			e.Undo()
			assert.Equal(t, "hello world", e.Text())
		})
	}
}

func TestEditorProgrammaticInsertionRetainsPasteStorageAndExitsHistory(t *testing.T) {
	e := New()
	e.SetLargePasteThreshold(3)
	e.Paste("stored body")
	marker := e.Text()
	pastes := e.LargePastes()
	e.AddHistory("history")
	e.HistoryPrev()
	require.True(t, e.historyBrowsing)
	e.InsertTextAtCursor("[image]")
	assert.False(t, e.historyBrowsing)
	assert.Equal(t, pastes, e.LargePastes())
	e.Undo()
	assert.Equal(t, "history", e.Text())
	assert.Equal(t, pastes, e.LargePastes())
	e.HistoryNext()
	assert.Equal(t, "history", e.Text())
	e.setText(marker)
	e.SetCursor(len(marker))
	e.InsertTextAtCursor("/image")
	assert.Equal(t, "stored body/image", e.ExpandedText())
	assert.Equal(t, 1, e.nextPaste)
	e.Undo()
	assert.Equal(t, "stored body", e.ExpandedText())
	e.Paste("next body")
	assert.Contains(t, e.Text(), "[paste #2")
}

func TestEditorEmptyProgrammaticInsertionLeavesStateAlone(t *testing.T) {
	e := New()
	e.Insert("a")
	e.SetAutocomplete(&autocomplete.Suggestions{Prefix: "a", Items: []autocomplete.Item{{Value: "abc"}}})
	e.InsertTextAtCursor("")
	assert.True(t, e.AutocompleteVisible())
	e.Insert("b")
	e.Undo()
	assert.Empty(t, e.Text())
}
