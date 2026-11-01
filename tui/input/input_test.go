package input

import (
	"testing"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/tau/tui/keybindings"
	"github.com/losinggeneration/tau/tui/killring"
)

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
	i.Undo()
	if got := i.Value(); got != "hello" {
		t.Fatalf("first undo = %q, want hello", got)
	}
	i.Undo()
	if got := i.Value(); got != "" {
		t.Fatalf("second undo = %q, want empty", got)
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

func TestInputTypingUndoIncludesWhitespaceWithFollowingWord(t *testing.T) {
	for _, separator := range []string{" ", "\t", "\u00a0", "\ufeff"} {
		t.Run(separator, func(t *testing.T) {
			i := New()
			for _, part := range []string{"hel", "lo", separator, "wor", "ld"} {
				i.Insert(part)
			}
			i.Undo()
			if i.Value() != "hello" {
				t.Fatalf("first undo = %q", i.Value())
			}
			i.Undo()
			if i.Value() != "" {
				t.Fatalf("second undo = %q", i.Value())
			}
		})
	}
}

func TestInputCharacterDeletesDoNotKillAndMovementBreaksKills(t *testing.T) {
	for _, action := range []string{"backward", "forward", "move", "undo", "boundary"} {
		t.Run(action, func(t *testing.T) {
			i := New()
			i.SetValue("one two")
			i.DeleteWordBackward()
			switch action {
			case "backward":
				i.DeleteBackward()
			case "forward":
				i.SetCursor(0)
				i.DeleteForward()
				i.End()
			case "move":
				i.MoveLeft()
				i.MoveRight()
			case "undo":
				i.Undo()
			case "boundary":
				i.DeleteForward()
			}
			if got := i.kill.Entries(); len(got) != 1 || got[0] != "two" {
				t.Fatalf("character edit changed ring: %v", got)
			}
			i.DeleteWordBackward()
			if got := i.kill.Entries(); len(got) != 2 || got[0] != "two" {
				t.Fatalf("kill chain was retained: %v", got)
			}
		})
	}
}

func TestInputEditingUsesConfiguredBindings(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	for _, tc := range []struct {
		action, want string
		cursor       int
	}{
		{"undo", "old", 3},
		{"deleteCharBackward", "onetwo", 3},
		{"deleteCharForward", "one wo", 4},
		{"deleteWordBackward", "two", 0},
		{"deleteWordForward", "one ", 4},
		{"deleteToLineStart", "two", 0},
		{"deleteToLineEnd", "one ", 4},
		{"yank", "one Ytwo", 5},
		{"yankPop", "one two", 4},
		{"cursorLeft", "one two", 3},
		{"cursorRight", "one two", 5},
		{"cursorLineStart", "one two", 0},
		{"cursorLineEnd", "one two", 7},
		{"cursorWordLeft", "one two", 0},
		{"cursorWordRight", "one two", 7},
	} {
		t.Run(tc.action, func(t *testing.T) {
			keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.editor." + tc.action: {"f2"}}))
			in := New()
			in.SetValue("one two")
			in.undo.Push(State{"old", 3})
			in.SetCursor(4)
			in.kill.Push("Y", killring.PushOptions{})
			if !in.HandleKey(event.KeyEvent{Key: event.KeyF2}) {
				t.Fatal("configured key not handled")
			}
			if in.Value() != tc.want || in.Cursor() != tc.cursor {
				t.Fatalf("value=%q cursor=%d want %q %d", in.Value(), in.Cursor(), tc.want, tc.cursor)
			}
		})
	}
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.editor.cursorLineStart": {}}))
	in := New()
	in.SetValue("value")
	if in.HandleKey(event.KeyEvent{Key: event.KeyRune, Rune: 'a', Mod: event.ModCtrl}) || in.Cursor() != 5 {
		t.Fatal("disabled binding still active")
	}
	if in.HandleKey(event.KeyEvent{Key: event.KeyLeft, Mod: event.ModShift}) || in.Cursor() != 5 {
		t.Fatal("unbound modified arrow moved cursor")
	}
}

func TestInputPasteRemovesLineBreaksAndExpandsEachTab(t *testing.T) {
	for _, tc := range []struct{ paste, want string }{
		{"a\r\nb\rc\nd", "abcd"}, {"a\t\tb", "a        b"}, {" a \n\t b ", " a      b "}, {"界\n👩‍💻\t", "界👩‍💻    "},
	} {
		t.Run(tc.paste, func(t *testing.T) {
			in := New()
			in.SetValue("left:right")
			in.SetCursor(5)
			in.Paste(tc.paste)
			if got := in.Value(); got != "left:"+tc.want+"right" {
				t.Fatalf("paste=%q want=%q", got, "left:"+tc.want+"right")
			}
			if in.Cursor() != 5+len(tc.want) {
				t.Fatalf("cursor=%d", in.Cursor())
			}
			in.Undo()
			if in.Value() != "left:right" || in.Cursor() != 5 {
				t.Fatalf("undo=%q cursor=%d", in.Value(), in.Cursor())
			}
		})
	}
}

func TestInputEmptyNormalizedPasteBreaksTypingWithoutReplacingSelection(t *testing.T) {
	for _, payload := range []string{"", "\r\n", "\x00\x07"} {
		t.Run(payload, func(t *testing.T) {
			in := New()
			in.Insert("first")
			in.Paste(payload)
			in.Insert("second")
			in.Undo()
			if in.Value() != "first" {
				t.Fatalf("undo typing=%q", in.Value())
			}
			in.Undo()
			if in.Value() != "first" {
				t.Fatalf("undo paste=%q", in.Value())
			}
			in.Undo()
			if in.Value() != "" {
				t.Fatalf("undo first=%q", in.Value())
			}
			in.SetValue("selected")
			in.SetSelection(0, len(in.Value()))
			in.Paste(payload)
			if in.Value() != "selected" {
				t.Fatalf("empty paste replaced selection: %q", in.Value())
			}
		})
	}
}
