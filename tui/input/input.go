package input

import (
	"unicode"
	"unicode/utf8"

	"github.com/losinggeneration/rovel/text"
	"github.com/losinggeneration/tau/tui/killring"
	"github.com/losinggeneration/tau/tui/undostack"
)

type State struct {
	Text   string
	Cursor int
}

type Input struct {
	text               string
	cursor             int
	kill               killring.Ring
	undo               *undostack.Stack[State]
	last               string
	yankStart, yankEnd int
	selectionStart     int
	selectionEnd       int
	submitted          string
	cancelled          bool
}

func New() *Input {
	return &Input{undo: undostack.New(func(s State) State { return s }), yankStart: -1, yankEnd: -1, selectionStart: -1, selectionEnd: -1}
}

func (i *Input) SetValue(s string) {
	i.text = s
	i.cursor = len(s)
	i.last = ""
	i.yankStart = -1
	i.yankEnd = -1
	i.clearSelection()
}
func (i *Input) Value() string     { return i.text }
func (i *Input) Cursor() int       { return i.cursor }
func (i *Input) Submitted() string { return i.submitted }
func (i *Input) Cancelled() bool   { return i.cancelled }
func (i *Input) SetCursor(pos int) {
	if pos < 0 {
		pos = 0
	}
	if pos > len(i.text) {
		pos = len(i.text)
	}
	i.cursor = text.ClampCluster(i.text, pos)
	i.breakChains()
}

func (i *Input) Insert(s string) {
	if s == "" {
		return
	}
	if i.hasSelection() {
		i.pushUndo()
		i.deleteSelectionNoUndo()
	} else if i.shouldPushInsertUndo(s) {
		i.pushUndo()
	}
	i.text = i.text[:i.cursor] + s + i.text[i.cursor:]
	i.cursor += len(s)
	if isCoalescableTyping(s) {
		i.last = "insert"
	} else {
		i.last = ""
	}
	i.yankStart = -1
}
func (i *Input) Submit() { i.submitted = i.text }
func (i *Input) Cancel() { i.cancelled = true }
func (i *Input) Undo() {
	st, ok := i.undo.Pop()
	if !ok {
		return
	}
	i.text = st.Text
	i.cursor = st.Cursor
	i.breakChains()
}

func (i *Input) MoveLeft() {
	i.cursor = text.PrevCluster(i.text, i.cursor)
	i.breakChains()
}

func (i *Input) MoveRight() {
	i.cursor = text.NextCluster(i.text, i.cursor)
	i.breakChains()
}
func (i *Input) Home() { i.cursor = 0; i.breakChains() }
func (i *Input) End()  { i.cursor = len(i.text); i.breakChains() }
func (i *Input) SetSelection(start, end int) {
	start = i.clampByte(start)
	end = i.clampByte(end)
	if start == end {
		i.clearSelection()
		return
	}
	if start > end {
		start, end = end, start
	}
	i.selectionStart = start
	i.selectionEnd = end
}

func (i *Input) Selection() (int, int, bool) {
	if !i.hasSelection() {
		return 0, 0, false
	}
	return i.selectionStart, i.selectionEnd, true
}

func (i *Input) CopySelection() string {
	if !i.hasSelection() {
		return ""
	}
	return i.text[i.selectionStart:i.selectionEnd]
}

func (i *Input) CutSelection() string {
	if !i.hasSelection() {
		return ""
	}
	i.pushUndo()
	deleted := i.deleteSelectionNoUndo()
	i.kill.Push(deleted, killring.PushOptions{})
	i.last = "kill"
	return deleted
}

func (i *Input) DeleteBackward() {
	if i.cursor == 0 {
		return
	}
	i.pushUndo()
	old := i.cursor
	i.MoveLeftNoBreak()
	deleted := i.text[i.cursor:old]
	i.text = i.text[:i.cursor] + i.text[old:]
	i.kill.Push(deleted, killring.PushOptions{Prepend: true, Accumulate: i.last == "kill"})
	i.last = "kill"
	i.yankStart = -1
}

func (i *Input) DeleteForward() {
	if i.cursor >= len(i.text) {
		return
	}
	i.pushUndo()
	next := text.NextCluster(i.text, i.cursor)
	deleted := i.text[i.cursor:next]
	i.text = i.text[:i.cursor] + i.text[next:]
	i.kill.Push(deleted, killring.PushOptions{Accumulate: i.last == "kill"})
	i.last = "kill"
	i.yankStart = -1
}

func (i *Input) DeleteWordBackward() {
	if i.cursor == 0 {
		return
	}
	i.pushUndo()
	old := i.cursor
	for i.cursor > 0 {
		r, _ := utf8.DecodeLastRuneInString(i.text[:i.cursor])
		if !unicode.IsSpace(r) {
			break
		}
		i.MoveLeftNoBreak()
	}
	for i.cursor > 0 {
		r, _ := utf8.DecodeLastRuneInString(i.text[:i.cursor])
		if unicode.IsSpace(r) {
			break
		}
		i.MoveLeftNoBreak()
	}
	deleted := i.text[i.cursor:old]
	i.text = i.text[:i.cursor] + i.text[old:]
	i.kill.Push(deleted, killring.PushOptions{Prepend: true, Accumulate: i.last == "kill"})
	i.last = "kill"
	i.yankStart = -1
}

func (i *Input) DeleteWordForward() {
	if i.cursor >= len(i.text) {
		return
	}
	i.pushUndo()
	start := i.cursor
	// Skip leading whitespace
	for i.cursor < len(i.text) {
		r, _ := utf8.DecodeRuneInString(i.text[i.cursor:])
		if !unicode.IsSpace(r) {
			break
		}
		_, sz := utf8.DecodeRuneInString(i.text[i.cursor:])
		i.cursor += sz
	}
	// Walk forward through word chars
	for i.cursor < len(i.text) {
		r, _ := utf8.DecodeRuneInString(i.text[i.cursor:])
		if unicode.IsSpace(r) {
			break
		}
		_, sz := utf8.DecodeRuneInString(i.text[i.cursor:])
		i.cursor += sz
	}
	end := i.cursor
	deleted := i.text[start:end]
	i.text = i.text[:start] + i.text[end:]
	i.cursor = start
	i.kill.Push(deleted, killring.PushOptions{Accumulate: i.last == "kill"})
	i.last = "kill"
	i.yankStart = -1
}

func (i *Input) DeleteToStart() {
	if i.cursor == 0 {
		return
	}
	i.pushUndo()
	deleted := i.text[:i.cursor]
	i.text = i.text[i.cursor:]
	i.cursor = 0
	i.kill.Push(deleted, killring.PushOptions{Prepend: true, Accumulate: i.last == "kill"})
	i.last = "kill"
	i.yankStart = -1
}

func (i *Input) DeleteToEnd() {
	if i.cursor >= len(i.text) {
		return
	}
	i.pushUndo()
	deleted := i.text[i.cursor:]
	i.text = i.text[:i.cursor]
	i.kill.Push(deleted, killring.PushOptions{Accumulate: i.last == "kill"})
	i.last = "kill"
	i.yankStart = -1
}

func (i *Input) Yank() {
	text, ok := i.kill.Peek()
	if !ok {
		return
	}
	i.pushUndo()
	start := i.cursor
	i.text = i.text[:i.cursor] + text + i.text[i.cursor:]
	i.cursor += len(text)
	i.yankStart = start
	i.yankEnd = i.cursor
	i.last = "yank"
}

func (i *Input) YankPop() {
	if i.last != "yank" || i.yankStart < 0 || i.kill.Len() < 2 {
		return
	}
	i.pushUndo()
	i.text = i.text[:i.yankStart] + i.text[i.yankEnd:]
	i.cursor = i.yankStart
	i.kill.Rotate()
	text, _ := i.kill.Peek()
	i.text = i.text[:i.cursor] + text + i.text[i.cursor:]
	i.cursor += len(text)
	i.yankEnd = i.cursor
	i.last = "yank"
}

func (i *Input) Paste(s string) {
	s = NormalizePaste(s)
	if s == "" {
		return
	}
	i.pushUndo()
	if i.hasSelection() {
		i.deleteSelectionNoUndo()
	}
	i.text = i.text[:i.cursor] + s + i.text[i.cursor:]
	i.cursor += len(s)
	i.last = "paste"
	i.yankStart = -1
}

func (i *Input) MoveWordLeft() {
	for i.cursor > 0 {
		r, _ := utf8.DecodeLastRuneInString(i.text[:i.cursor])
		if !unicode.IsSpace(r) {
			break
		}
		i.MoveLeftNoBreak()
	}
	for i.cursor > 0 {
		r, _ := utf8.DecodeLastRuneInString(i.text[:i.cursor])
		if unicode.IsSpace(r) {
			break
		}
		i.MoveLeftNoBreak()
	}
	i.breakChains()
}

func (i *Input) MoveWordRight() {
	for i.cursor < len(i.text) {
		r, sz := utf8.DecodeRuneInString(i.text[i.cursor:])
		if !unicode.IsSpace(r) {
			break
		}
		i.cursor += sz
	}
	for i.cursor < len(i.text) {
		r, sz := utf8.DecodeRuneInString(i.text[i.cursor:])
		if unicode.IsSpace(r) {
			break
		}
		i.cursor += sz
	}
	i.breakChains()
}

func NormalizePaste(s string) string {
	out := make([]rune, 0, len(s))
	lastWasSpace := false
	for _, r := range s {
		switch r {
		case '\r', '\n', '\t':
			if !lastWasSpace {
				out = append(out, ' ')
				lastWasSpace = true
			}
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		out = append(out, r)
		lastWasSpace = r == ' '
	}
	return string(out)
}

func (i *Input) MoveLeftNoBreak() {
	i.cursor = text.PrevCluster(i.text, i.cursor)
}
func (i *Input) pushUndo() { i.undo.Push(State{i.text, i.cursor}) }
func (i *Input) breakChains() {
	if i.last != "kill" {
		i.last = ""
	}
	i.yankStart = -1
	i.yankEnd = -1
	i.clearSelection()
}

func (i *Input) shouldPushInsertUndo(s string) bool {
	return i.last != "insert" || !isCoalescableTyping(s)
}

func isCoalescableTyping(s string) bool {
	r, sz := utf8.DecodeRuneInString(s)
	return r != utf8.RuneError && sz == len(s) && !unicode.IsSpace(r)
}

func (i *Input) clampByte(pos int) int {
	if pos < 0 {
		pos = 0
	}
	if pos > len(i.text) {
		pos = len(i.text)
	}
	return text.ClampCluster(i.text, pos)
}

func (i *Input) hasSelection() bool {
	return i.selectionStart >= 0 && i.selectionEnd > i.selectionStart && i.selectionEnd <= len(i.text)
}

func (i *Input) clearSelection() {
	i.selectionStart = -1
	i.selectionEnd = -1
}

func (i *Input) deleteSelectionNoUndo() string {
	if !i.hasSelection() {
		return ""
	}
	deleted := i.text[i.selectionStart:i.selectionEnd]
	i.text = i.text[:i.selectionStart] + i.text[i.selectionEnd:]
	i.cursor = i.selectionStart
	i.clearSelection()
	return deleted
}
