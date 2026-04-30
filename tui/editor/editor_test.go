package editor

import (
	"testing"

	"github.com/losinggeneration/tau/tui/autocomplete"
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
	if got := e.DeleteWordForward(); got != "one " {
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
	if e.Text() != "two!" {
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

func TestPiEditorSubmitEmptyDoesNothing(t *testing.T) {
	called := false
	e := New()
	e.SetOnSubmit(func(string) { called = true })
	e.Submit()
	if called {
		t.Fatal("submit on empty should not call callback")
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
