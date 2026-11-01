package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPrimitivesInsertShiftedCharacter(t *testing.T) {
	input := &InputPrimitive{}
	editor := &EditorPrimitive{}
	key := KeyEvent{Key: KeyRune, Rune: '=', ShiftedRune: '+', Mod: ModShift}
	input.HandleKey(key)
	editor.HandleKey(key)
	assert.Equal(t, "+", input.Value)
	assert.Equal(t, "+", editor.ensureModel().Text())
	key.Mod |= ModCtrl
	input.HandleKey(key)
	editor.HandleKey(key)
	assert.Equal(t, "+", input.Value)
	assert.Equal(t, "+", editor.ensureModel().Text())
}

func TestInputPrimitivePasteNormalizesAndReplacesSelection(t *testing.T) {
	p := &InputPrimitive{Value: "before after"}
	p.SetSelection(0, 6)
	p.Paste("a\r\n\tb\x00\x07")
	assert.Equal(t, "a b after", p.Value)
	p.HandleKey(KeyEvent{Key: KeyHome})
	p.HandleKey(KeyEvent{Key: KeyDelete})
	p.Paste("x")
	assert.Equal(t, "x b after", p.Value)
}

func TestPrimitiveStringInputReachesEditingActions(t *testing.T) {
	for _, kind := range []string{"input", "editor"} {
		t.Run(kind, func(t *testing.T) {
			input := &InputPrimitive{Value: "one two"}
			editor := &EditorPrimitive{}
			editor.ensureModel().SetText("one two")
			handle := input.HandleInput
			value := func() string { return input.Value }
			if kind == "editor" {
				handle = editor.HandleInput
				value = func() string { return editor.ensureModel().Text() }
			}
			handle("\x17")
			assert.Equal(t, "one ", value())
			handle("\x19")
			assert.Equal(t, "one two", value())
			handle("\x1b[45;5u")
			assert.Equal(t, "one ", value())
			handle("\x15")
			assert.Empty(t, value())
			handle("\x19")
			assert.Equal(t, "one ", value())
			handle("\x1by")
			assert.Equal(t, "two", value())
			handle("\x01")
			handle("\x1bd")
			assert.Empty(t, value())
			handle("\x19")
			assert.Equal(t, "two", value())
			handle("\x1b[H")
			handle("\x1b[3~")
			assert.Equal(t, "wo", value())
			handle("\x1b[F")
			handle("\x1b[1;5D")
			handle("\x04")
			assert.Equal(t, "o", value())
		})
	}
}

func TestPrimitiveStringInputPreservesChunksAndPasteBoundaries(t *testing.T) {
	for _, kind := range []string{"input", "editor"} {
		t.Run(kind, func(t *testing.T) {
			input := &InputPrimitive{}
			editor := &EditorPrimitive{}
			handle := input.HandleInput
			value := func() string { return input.Value }
			undo := func() { input.HandleInput("\x1b[45;5u") }
			var submitted []string
			input.OnSubmit = func(text string) { submitted = append(submitted, text) }
			editor.OnSubmit = input.OnSubmit
			if kind == "editor" {
				handle = editor.HandleInput
				value = func() string { return editor.ensureModel().Text() }
				undo = func() { editor.HandleInput("\x1b[45;5u") }
			}
			handle("ab界e\u0301")
			assert.Equal(t, "ab界e\u0301", value())
			handle("\x1b[")
			assert.Equal(t, "ab界e\u0301", value())
			handle("D")
			handle("X")
			assert.Equal(t, "ab界Xe\u0301", value())
			handle("\x1b[")
			handle("20")
			for _, b := range []byte("0~hello\nworld\x1b[201~") {
				handle(string([]byte{b}))
			}
			assert.Empty(t, submitted)
			if kind == "editor" {
				assert.Equal(t, "ab界Xhello\nworlde\u0301", value())
			} else {
				assert.Equal(t, "ab界Xhello worlde\u0301", value())
			}
			undo()
			assert.Equal(t, "ab界Xe\u0301", value())
			handle("\x1b]10;rgb:00/00/00\x07")
			assert.Equal(t, "ab界Xe\u0301", value())
		})
	}
}

func TestPrimitiveStringInputPreservesSplitUTF8(t *testing.T) {
	input := &InputPrimitive{}
	editor := &EditorPrimitive{}
	for _, b := range []byte("界e\u0301") {
		chunk := string([]byte{b})
		input.HandleInput(chunk)
		editor.HandleInput(chunk)
	}
	assert.Equal(t, "界e\u0301", input.Value)
	assert.Equal(t, input.Value, editor.ensureModel().Text())
}

func TestPrimitiveTypingPreservesUnicodeFormatCharacters(t *testing.T) {
	value := "👩\u200d💻ش\u200cم\u00a0\u2028\U000e0100"
	input := &InputPrimitive{}
	editor := &EditorPrimitive{}
	input.HandleInput(value)
	editor.HandleInput(value)
	assert.Equal(t, value, input.Value)
	assert.Equal(t, value, editor.ensureModel().Text())
	for _, mod := range []ModMask{ModCtrl, ModAlt} {
		input.HandleKey(KeyEvent{Key: KeyRune, Rune: '\u200d', Mod: mod})
		editor.HandleKey(KeyEvent{Key: KeyRune, Rune: '\u200d', Mod: mod})
	}
	assert.Equal(t, value, input.Value)
	assert.Equal(t, value, editor.ensureModel().Text())
}

func TestEditorPrimitiveWrapBoundaryRendersOneCursor(t *testing.T) {
	p := &EditorPrimitive{}
	p.HandleInput("abcdef")
	p.ensureModel().SetCursor(3)
	rows := p.Render(3)
	assert.Equal(t, []string{"abc", CursorMarker + "def"}, rows)
	assert.Equal(t, 1, strings.Count(strings.Join(rows, ""), CursorMarker))
}

func TestEditorPrimitivePagesThroughWrappedRows(t *testing.T) {
	p := &EditorPrimitive{MaxRows: 2}
	m := p.ensureModel()
	m.SetText("abcdefghijklmnopqr")
	m.SetCursor(1)
	p.Render(3)
	p.HandleInput("\x1b[6~")
	assert.Equal(t, 7, m.Cursor())
	p.HandleInput("\x1b[6~")
	assert.Equal(t, 13, m.Cursor())
	p.HandleInput("\x1b[6~")
	assert.Equal(t, 16, m.Cursor())
	p.HandleInput("\x1b[5~")
	assert.Equal(t, 10, m.Cursor())
	assert.Equal(t, "abcdefghijklmnopqr", m.Text())
	p.MaxRows = 0
	p.HandleInput("\x1b[5~")
	assert.Equal(t, 1, m.Cursor())
	p.HandleInput("\x1b[6~")
	assert.Equal(t, 16, m.Cursor())
}

func TestInputPrimitiveBoundsRowsAndCentersViewport(t *testing.T) {
	for _, tc := range []struct {
		prompt, value, want string
		width, cursor       int
	}{
		{"> ", "abc", ">", 1, 0},
		{"> ", "界x", ">  ", 3, 0},
		{"> ", "界x", "> 界", 4, 0},
		{"> ", "abcdefghijklmnop", "> fghijk", 8, 8},
		{"", "abcdefghijklmnop", "fghijk", 6, 8},
		{"> ", "abcdefghijklmnop", "> lmnop ", 8, 16},
	} {
		p := &InputPrimitive{Prompt: tc.prompt, Value: tc.value}
		p.ensureModel().SetCursor(tc.cursor)
		rows := p.Render(tc.width)
		assert.Len(t, rows, 1)
		assert.LessOrEqual(t, TextWidth(rows[0]), tc.width)
		assert.Equal(t, tc.want, StripANSI(rows[0]))
		assert.Equal(t, tc.value, p.Value)
	}
}

func TestEditorPrimitiveCharacterJumpMode(t *testing.T) {
	p := &EditorPrimitive{}
	m := p.ensureModel()
	m.SetText("o+界o\nend+")
	m.SetCursor(0)
	p.HandleInput("\x1b[93;5u")
	p.HandleInput("o")
	assert.Equal(t, len("o+界"), m.Cursor())
	p.HandleInput("\x1b[93;7u")
	p.HandleInput("+")
	assert.Equal(t, 1, m.Cursor())
	p.HandleInput("\x1b[93;5u")
	p.HandleInput("z")
	assert.Equal(t, 1, m.Cursor())
	assert.Equal(t, "o+界o\nend+", m.Text())
	p.HandleInput("\x1b[93;5u")
	p.HandleInput("\x1b[93;5u")
	p.HandleInput("X")
	assert.Equal(t, "oX+界o\nend+", m.Text())
	p.HandleInput("\x1b[93;5u")
	p.HandleInput("\x1b[D")
	p.HandleInput("Y")
	assert.Equal(t, "oYX+界o\nend+", m.Text())
	p.HandleInput("\x1b[93;5u")
	p.HandleInput("\x1b[43;2u")
	assert.Equal(t, len("oYX"), m.Cursor())
}
