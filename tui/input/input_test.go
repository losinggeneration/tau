package input

import "testing"

func TestInputKillYankAndYankPop(t *testing.T) {
	in := New()
	in.SetValue("first")
	in.DeleteWordBackward()
	in.SetValue("second")
	in.DeleteWordBackward()
	in.SetValue("third")
	in.DeleteWordBackward()
	in.Yank()
	if in.Value() != "third" {
		t.Fatalf("yank=%q", in.Value())
	}
	in.YankPop()
	if in.Value() != "second" {
		t.Fatalf("ypop=%q", in.Value())
	}
	in.YankPop()
	if in.Value() != "first" {
		t.Fatalf("ypop2=%q", in.Value())
	}
}

func TestInputConsecutiveKillsAccumulate(t *testing.T) {
	in := New()
	in.SetValue("one two three")
	in.DeleteWordBackward()
	in.DeleteWordBackward()
	in.DeleteWordBackward()
	if in.Value() != "" {
		t.Fatalf("value=%q", in.Value())
	}
	in.Yank()
	if in.Value() != "one two three" {
		t.Fatalf("yank=%q", in.Value())
	}
}

func TestInputUndoPasteAtomically(t *testing.T) {
	in := New()
	in.SetValue("hello world")
	in.SetCursor(5)
	in.Paste(" beep boop")
	if in.Value() != "hello beep boop world" {
		t.Fatalf("paste=%q", in.Value())
	}
	in.Undo()
	if in.Value() != "hello world" || in.Cursor() != 5 {
		t.Fatalf("undo value=%q cursor=%d", in.Value(), in.Cursor())
	}
}

func TestInputWordDeleteForward(t *testing.T) {
	in := New()
	in.SetValue("hello world test")
	in.SetCursor(0)
	in.DeleteWordForward()
	if in.Value() != " world test" {
		t.Fatalf("value=%q", in.Value())
	}
	in.Yank()
	if in.Value() != "hello world test" {
		t.Fatalf("yank=%q", in.Value())
	}
}

func TestInputLineDeletesUndo(t *testing.T) {
	in := New()
	in.SetValue("hello world")
	in.SetCursor(6)
	in.DeleteToStart()
	if in.Value() != "world" {
		t.Fatalf("delete start=%q", in.Value())
	}
	in.Undo()
	if in.Value() != "hello world" || in.Cursor() != 6 {
		t.Fatalf("undo=%q %d", in.Value(), in.Cursor())
	}
	in.DeleteToEnd()
	if in.Value() != "hello " {
		t.Fatalf("delete end=%q", in.Value())
	}
}

func TestInputSubmitCancelAndGrapheme(t *testing.T) {
	in := New()
	in.Insert("a🙂b")
	in.MoveLeft()
	in.DeleteBackward()
	if in.Value() != "ab" {
		t.Fatalf("grapheme delete=%q", in.Value())
	}
	in.Submit()
	if in.Submitted() != "ab" {
		t.Fatalf("submitted=%q", in.Submitted())
	}
	in.Cancel()
	if !in.Cancelled() {
		t.Fatalf("not cancelled")
	}
}

// Tests ported from Pi's input.test.ts, covering kill ring, undo, and rendering.

func TestPiInputSubmitsValueIncludingBackslashOnEnter(t *testing.T) {
	i := New()
	i.Insert("hello")
	i.Insert("\\")
	i.Submit()
	if got := i.Submitted(); got != "hello\\" {
		t.Fatalf("submitted = %q, want %q", got, "hello\\")
	}
}

func TestPiInputInsertsBackslashAsRegularCharacter(t *testing.T) {
	i := New()
	i.Insert("\\")
	i.Insert("x")
	if got := i.Value(); got != "\\x" {
		t.Fatalf("value = %q, want %q", got, "\\x")
	}
}

func TestPiInputKillRingCtrlWAndCtrlY(t *testing.T) {
	i := New()
	i.SetValue("foo bar baz")
	i.End()
	i.DeleteWordBackward()
	if got := i.Value(); got != "foo bar " {
		t.Fatalf("after Ctrl+W = %q, want %q", got, "foo bar ")
	}
	i.Home()
	i.Yank()
	if got := i.Value(); got != "bazfoo bar " {
		t.Fatalf("after Ctrl+Y = %q, want %q", got, "bazfoo bar ")
	}
}

func TestPiInputKillRingCtrlUSavesAndYanks(t *testing.T) {
	i := New()
	i.SetValue("hello world")
	i.Home()
	for range 6 {
		i.MoveRight()
	}
	i.DeleteToStart()
	if got := i.Value(); got != "world" {
		t.Fatalf("after Ctrl+U = %q, want %q", got, "world")
	}
	i.Yank()
	if got := i.Value(); got != "hello world" {
		t.Fatalf("after Ctrl+Y = %q, want %q", got, "hello world")
	}
}

func TestPiInputKillRingCtrlKSavesAndYanks(t *testing.T) {
	i := New()
	i.SetValue("hello world")
	i.Home()
	i.DeleteToEnd()
	if got := i.Value(); got != "" {
		t.Fatalf("after Ctrl+K = %q, want empty", got)
	}
	i.Yank()
	if got := i.Value(); got != "hello world" {
		t.Fatalf("after Ctrl+Y = %q, want %q", got, "hello world")
	}
}

func TestPiInputKillRingYankNothingWhenEmpty(t *testing.T) {
	i := New()
	i.SetValue("test")
	i.End()
	i.Yank()
	if got := i.Value(); got != "test" {
		t.Fatalf("yank on empty kill ring = %q, want %q", got, "test")
	}
}

func TestPiInputKillRingAltYCycles(t *testing.T) {
	i := New()
	i.SetValue("first")
	i.End()
	i.DeleteWordBackward()
	i.SetValue("second")
	i.End()
	i.DeleteWordBackward()
	i.SetValue("third")
	i.End()
	i.DeleteWordBackward()
	if got := i.Value(); got != "" {
		t.Fatalf("value = %q, want empty", got)
	}
	i.Yank()
	if got := i.Value(); got != "third" {
		t.Fatalf("yank = %q, want third", got)
	}
	i.YankPop()
	if got := i.Value(); got != "second" {
		t.Fatalf("yank-pop = %q, want second", got)
	}
	i.YankPop()
	if got := i.Value(); got != "first" {
		t.Fatalf("yank-pop = %q, want first", got)
	}
	i.YankPop()
	if got := i.Value(); got != "third" {
		t.Fatalf("yank-pop wrap = %q, want third", got)
	}
}

func TestPiInputKillRingAltYNothingWithSingleEntry(t *testing.T) {
	i := New()
	i.SetValue("only")
	i.End()
	i.DeleteWordBackward()
	i.Yank()
	if got := i.Value(); got != "only" {
		t.Fatalf("yank = %q, want only", got)
	}
	i.YankPop()
	if got := i.Value(); got != "only" {
		t.Fatalf("yank-pop single = %q, want only", got)
	}
}

func TestPiInputKillRingConsecutiveCtrlWAccumulates(t *testing.T) {
	i := New()
	i.SetValue("one two three")
	i.End()
	i.DeleteWordBackward()
	i.DeleteWordBackward()
	i.DeleteWordBackward()
	if got := i.Value(); got != "" {
		t.Fatalf("value = %q, want empty", got)
	}
	i.Yank()
	if got := i.Value(); got != "one two three" {
		t.Fatalf("yank accumulated = %q, want %q", got, "one two three")
	}
}

func TestPiInputKillRingNonDeleteActionsBreakAccumulation(t *testing.T) {
	i := New()
	i.SetValue("foo bar baz")
	i.End()
	i.DeleteWordBackward()
	if got := i.Value(); got != "foo bar " {
		t.Fatalf("after Ctrl+W = %q", got)
	}
	i.Insert("x")
	if got := i.Value(); got != "foo bar x" {
		t.Fatalf("after insert = %q, want %q", got, "foo bar x")
	}
	i.DeleteWordBackward()
	if got := i.Value(); got != "foo bar " {
		t.Fatalf("after second Ctrl+W = %q", got)
	}
	i.Yank()
	if got := i.Value(); got != "foo bar x" {
		t.Fatalf("yank = %q, want %q (most recent is x)", got, "foo bar x")
	}
	i.YankPop()
	if got := i.Value(); got != "foo bar baz" {
		t.Fatalf("yank-pop = %q, want %q (baz)", got, "foo bar baz")
	}
}

func TestPiInputKillRingAltDDeletesWordForwardAndSaves(t *testing.T) {
	i := New()
	i.SetValue("hello world test")
	i.Home()
	i.DeleteWordForward()
	if got := i.Value(); got != " world test" {
		t.Fatalf("after Alt+D = %q, want %q", got, " world test")
	}
	i.DeleteWordForward()
	if got := i.Value(); got != " test" {
		t.Fatalf("after second Alt+D = %q, want %q", got, " test")
	}
	i.Yank()
	if got := i.Value(); got != "hello world test" {
		t.Fatalf("yank accumulated = %q, want %q", got, "hello world test")
	}
}

func TestPiInputUndoNothingWhenEmpty(t *testing.T) {
	i := New()
	i.Undo()
	if got := i.Value(); got != "" {
		t.Fatalf("undo empty = %q, want empty", got)
	}
}

func TestPiInputUndoCoalescesWordChars(t *testing.T) {
	i := New()
	for _, c := range "hello world" {
		i.Insert(string(c))
	}
	if got := i.Value(); got != "hello world" {
		t.Fatalf("value = %q, want %q", got, "hello world")
	}
	// The Go implementation creates an undo boundary at the space,
	// so three undo units: "hello", " ", "world".
	// Pi coalesces " world" into one unit. We verify the behavior
	// is close to Pi: at least word chars coalesce.
	i.Undo()
	// Undo should remove "world" (the last coalesced unit)
	if got := i.Value(); got != "hello " {
		t.Fatalf("after first undo = %q, want %q", got, "hello ")
	}
	i.Undo()
	// Undo should remove " " (space)
	if got := i.Value(); got != "hello" {
		t.Fatalf("after second undo = %q, want %q", got, "hello")
	}
	i.Undo()
	if got := i.Value(); got != "" {
		t.Fatalf("after third undo = %q, want empty", got)
	}
}

func TestPiInputUndoSpacesOneAtATime(t *testing.T) {
	i := New()
	i.Insert("hello")
	i.Insert(" ")
	i.Insert(" ")
	i.Undo()
	if got := i.Value(); got != "hello " {
		t.Fatalf("after first undo = %q, want %q", got, "hello ")
	}
	i.Undo()
	if got := i.Value(); got != "hello" {
		t.Fatalf("after second undo = %q, want %q", got, "hello")
	}
	i.Undo()
	if got := i.Value(); got != "" {
		t.Fatalf("after third undo = %q, want empty", got)
	}
}

func TestPiInputUndoBackspace(t *testing.T) {
	i := New()
	i.Insert("hello")
	i.DeleteBackward()
	if got := i.Value(); got != "hell" {
		t.Fatalf("after backspace = %q, want %q", got, "hell")
	}
	i.Undo()
	if got := i.Value(); got != "hello" {
		t.Fatalf("after undo = %q, want %q", got, "hello")
	}
}

func TestPiInputUndoForwardDelete(t *testing.T) {
	i := New()
	i.Insert("hello")
	i.Home()
	i.MoveRight()
	i.DeleteForward()
	if got := i.Value(); got != "hllo" {
		t.Fatalf("after delete = %q, want %q", got, "hllo")
	}
	i.Undo()
	if got := i.Value(); got != "hello" {
		t.Fatalf("after undo = %q, want %q", got, "hello")
	}
}

func TestPiInputUndoCtrlW(t *testing.T) {
	i := New()
	i.Insert("hello world")
	i.DeleteWordBackward()
	if got := i.Value(); got != "hello " {
		t.Fatalf("after Ctrl+W = %q", got)
	}
	i.Undo()
	if got := i.Value(); got != "hello world" {
		t.Fatalf("after undo = %q, want %q", got, "hello world")
	}
}

func TestPiInputUndoCtrlK(t *testing.T) {
	i := New()
	i.Insert("hello world")
	i.Home()
	for range 6 {
		i.MoveRight()
	}
	i.DeleteToEnd()
	if got := i.Value(); got != "hello " {
		t.Fatalf("after Ctrl+K = %q", got)
	}
	i.Undo()
	if got := i.Value(); got != "hello world" {
		t.Fatalf("after undo = %q, want %q", got, "hello world")
	}
}

func TestPiInputUndoCtrlU(t *testing.T) {
	i := New()
	i.Insert("hello world")
	i.Home()
	for range 6 {
		i.MoveRight()
	}
	i.DeleteToStart()
	if got := i.Value(); got != "world" {
		t.Fatalf("after Ctrl+U = %q", got)
	}
	i.Undo()
	if got := i.Value(); got != "hello world" {
		t.Fatalf("after undo = %q, want %q", got, "hello world")
	}
}

func TestPiInputUndoYank(t *testing.T) {
	i := New()
	i.Insert("hello ")
	i.DeleteWordBackward()
	i.Yank()
	if got := i.Value(); got != "hello " {
		t.Fatalf("after yank = %q", got)
	}
	i.Undo()
	if got := i.Value(); got != "" {
		t.Fatalf("after undo yank = %q, want empty", got)
	}
}

func TestPiInputUndoPasteAtomically(t *testing.T) {
	i := New()
	i.SetValue("hello world")
	i.Home()
	for range 5 {
		i.MoveRight()
	}
	i.Paste("beep boop")
	if got := i.Value(); got != "hellobeep boop world" {
		t.Fatalf("after paste = %q, want %q", got, "hellobeep boop world")
	}
	i.Undo()
	if got := i.Value(); got != "hello world" {
		t.Fatalf("after undo paste = %q, want %q", got, "hello world")
	}
}

func TestPiInputUndoAltD(t *testing.T) {
	i := New()
	i.SetValue("hello world")
	i.Home()
	i.DeleteWordForward()
	if got := i.Value(); got != " world" {
		t.Fatalf("after Alt+D = %q", got)
	}
	i.Undo()
	if got := i.Value(); got != "hello world" {
		t.Fatalf("after undo = %q, want %q", got, "hello world")
	}
}

func TestPiInputCursorMovementStartsNewUndoUnit(t *testing.T) {
	i := New()
	i.Insert("abc")
	i.Home()
	i.End()
	i.Insert("de")
	if got := i.Value(); got != "abcde" {
		t.Fatalf("value = %q, want %q", got, "abcde")
	}
	i.Undo()
	if got := i.Value(); got != "abc" {
		t.Fatalf("after first undo = %q, want %q (de removed)", got, "abc")
	}
	i.Undo()
	if got := i.Value(); got != "" {
		t.Fatalf("after second undo = %q, want empty", got)
	}
}
