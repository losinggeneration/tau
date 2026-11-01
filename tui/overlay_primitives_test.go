package tui

import (
	"strings"
	"testing"

	"github.com/losinggeneration/tau/tui/autocomplete"
	"github.com/losinggeneration/tau/tui/keybindings"
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
	assert.Equal(t, "a    b after", p.Value)
	p.HandleKey(KeyEvent{Key: KeyHome})
	p.HandleKey(KeyEvent{Key: KeyDelete})
	p.Paste("x")
	assert.Equal(t, "x    b after", p.Value)
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
				assert.Equal(t, "ab界Xhelloworlde\u0301", value())
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

func TestEditorPrimitiveUsesConfiguredEditingKeys(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	for _, tc := range []struct {
		id, old, want string
		cursor        int
	}{
		{"cursorLeft", "left", "one two", 3},
		{"cursorRight", "right", "one two", 5},
		{"cursorWordLeft", "alt+b", "one two", 0},
		{"cursorWordRight", "alt+f", "one two", 7},
		{"cursorLineStart", "home", "one two", 0},
		{"cursorLineEnd", "end", "one two", 7},
		{"deleteCharBackward", "backspace", "onetwo", 3},
		{"deleteCharForward", "delete", "one wo", 4},
		{"deleteWordBackward", "ctrl+w", "two", 0},
		{"deleteWordForward", "alt+d", "one ", 4},
		{"deleteToLineStart", "ctrl+u", "two", 0},
		{"deleteToLineEnd", "ctrl+k", "one ", 4},
		{"yank", "ctrl+y", "one savedtwo", 9},
		{"yankPop", "alt+y", "one oldertwo", 9},
		{"undo", "ctrl+-", "", 0},
	} {
		t.Run(tc.id, func(t *testing.T) {
			keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.editor." + tc.id: {"f2"}}))
			p := &EditorPrimitive{}
			m := p.ensureModel()
			m.SetText("older")
			m.DeleteToLineStart()
			m.MoveLeft()
			m.SetText("saved")
			m.DeleteToLineStart()
			m.SetText("one two")
			m.SetCursor(4)
			if tc.id == "yankPop" {
				m.Yank()
			}
			p.HandleKey(KeyEvent{Key: KeyF2})
			assert.Equal(t, tc.want, m.Text())
			assert.Equal(t, tc.cursor, m.Cursor())
			keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.editor." + tc.id: {}}))
			m.SetText("one two")
			m.SetCursor(4)
			old, err := keybindings.CompileKey(tc.old)
			if !assert.NoError(t, err) {
				return
			}
			p.HandleKey(KeyEvent{Key: old.Key, Rune: old.Rune, Mod: old.Mod})
			assert.Equal(t, "one two", m.Text())
			assert.Equal(t, 4, m.Cursor(), "disabled old key")
		})
	}
	for _, tc := range []struct {
		id          string
		start, want int
	}{
		{"cursorUp", 4, 0}, {"cursorDown", 0, 4}, {"pageUp", 4, 0}, {"pageDown", 0, 4},
	} {
		t.Run(tc.id, func(t *testing.T) {
			keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.editor." + tc.id: {"f2"}}))
			p := &EditorPrimitive{MaxRows: 1}
			p.ensureModel().SetText("abcdef")
			p.ensureModel().SetCursor(tc.start)
			p.Render(4)
			p.HandleKey(KeyEvent{Key: KeyF2})
			assert.Equal(t, tc.want, p.Model.Cursor())
			assert.Equal(t, "abcdef", p.Model.Text())
		})
	}
}

func TestOverlayPrimitivesUseConfiguredSubmitAndCancel(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{
		"tui.input.submit": {"f2"}, "tui.input.newLine": {"f3"}, "tui.select.cancel": {"f4"},
	}))
	for _, kind := range []string{"input", "editor"} {
		t.Run(kind, func(t *testing.T) {
			var submitted []string
			var canceled int
			input := &InputPrimitive{Value: "hello"}
			editor := &EditorPrimitive{}
			editor.ensureModel().SetText("hello")
			input.OnSubmit = func(s string) { submitted = append(submitted, s) }
			editor.OnSubmit = input.OnSubmit
			input.OnCancel = func() { canceled++ }
			editor.OnCancel = input.OnCancel
			handle := input.HandleKey
			if kind == "editor" {
				handle = editor.HandleKey
			}
			handle(KeyEvent{Key: KeyEnter})
			handle(KeyEvent{Key: KeyEsc})
			assert.Empty(t, submitted)
			assert.Zero(t, canceled)
			handle(KeyEvent{Key: KeyF2})
			assert.Equal(t, []string{"hello"}, submitted)
			handle(KeyEvent{Key: KeyF4})
			assert.Equal(t, 1, canceled)
			if kind == "editor" {
				handle(KeyEvent{Key: KeyF3})
				assert.Equal(t, "\n", editor.Model.Text())
			}
		})
	}
}

func TestEditorPrimitiveCompletionUsesConfiguredSelectionKeys(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{
		"tui.select.up": {"f2"}, "tui.select.down": {"f3"}, "tui.select.cancel": {"f4"}, "tui.input.tab": {"f5"}, "tui.select.confirm": {"f6"},
	}))
	p := &EditorPrimitive{}
	m := p.ensureModel()
	m.SetText("o")
	suggestions := &autocomplete.Suggestions{Prefix: "o", Items: []autocomplete.Item{{Value: "one"}, {Value: "other"}}}
	m.SetAutocomplete(suggestions)
	p.HandleKey(KeyEvent{Key: KeyF3})
	assert.Equal(t, 1, m.AutocompleteIndex())
	p.HandleKey(KeyEvent{Key: KeyF2})
	assert.Zero(t, m.AutocompleteIndex())
	p.HandleKey(KeyEvent{Key: KeyF4})
	assert.False(t, m.AutocompleteVisible())
	for _, key := range []Key{KeyF5, KeyF6} {
		m.SetText("o")
		m.SetAutocomplete(suggestions)
		p.HandleKey(KeyEvent{Key: key})
		assert.False(t, m.AutocompleteVisible())
		assert.Equal(t, "one", strings.TrimSpace(m.Text()))
	}
}

func TestEditorPrimitiveBackslashSubmitUsesCursorAndPreservesState(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(nil))
	for _, tc := range []struct {
		text      string
		cursor    int
		want      string
		submitted bool
	}{
		{"before\\after", 7, "before\nafter", false},
		{"before\\", 3, "", true},
	} {
		t.Run(tc.text, func(t *testing.T) {
			var submitted []string
			p := &EditorPrimitive{OnSubmit: func(s string) { submitted = append(submitted, s) }}
			m := p.ensureModel()
			m.SetText(tc.text)
			m.SetCursor(tc.cursor)
			p.HandleKey(KeyEvent{Key: KeyEnter})
			assert.Equal(t, tc.want, m.Text())
			if tc.submitted {
				assert.Equal(t, []string{tc.text}, submitted)
			} else {
				assert.Empty(t, submitted)
				m.Undo()
				m.Undo()
				assert.Equal(t, tc.text, m.Text())
				assert.Equal(t, tc.cursor, m.Cursor())
			}
		})
	}
	p := &EditorPrimitive{}
	m := p.ensureModel()
	m.Paste(strings.Repeat("x", 1200))
	marker := m.Text()
	m.Insert("\\")
	p.HandleKey(KeyEvent{Key: KeyEnter})
	assert.Equal(t, strings.Repeat("x", 1200)+"\n", m.ExpandedText())
	m.Undo()
	m.Undo()
	assert.Equal(t, marker+"\\", m.Text())
	assert.Equal(t, strings.Repeat("x", 1200)+"\\", m.ExpandedText())
}

func TestEditorPrimitiveBackslashEnterSubmitsWithSwappedBindings(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{
		"tui.input.submit": {"shift+enter"}, "tui.input.newLine": {"enter"},
	}))
	var submitted []string
	p := &EditorPrimitive{OnSubmit: func(s string) { submitted = append(submitted, s) }}
	p.ensureModel().SetText("hello\\tail")
	p.Model.SetCursor(6)
	p.HandleKey(KeyEvent{Key: KeyEnter})
	assert.Equal(t, []string{"hellotail"}, submitted)
	assert.Empty(t, p.Model.Text())
	p.Model.SetText("hello")
	p.HandleKey(KeyEvent{Key: KeyEnter})
	assert.Equal(t, "hello\n", p.Model.Text())
	assert.Len(t, submitted, 1)
}

func TestEditorPrimitiveExplicitHistoryCancelsCompletionAndRestoresDraft(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{
		"tui.editor.historyPrevious": {"f2"}, "tui.editor.historyNext": {"f3"},
	}))
	p := &EditorPrimitive{}
	m := p.ensureModel()
	m.AddHistory("old\nentry")
	m.AddHistory("new\nentry")
	m.SetText("draft\nbody")
	m.SetCursor(8)
	m.SetAutocomplete(&autocomplete.Suggestions{Prefix: "body", Items: []autocomplete.Item{{Value: "completion"}}})
	for _, step := range []struct {
		key  Key
		text string
	}{{KeyF2, "new\nentry"}, {KeyF2, "old\nentry"}, {KeyF3, "new\nentry"}, {KeyF3, "draft\nbody"}} {
		p.HandleKey(KeyEvent{Key: step.key})
		assert.Equal(t, step.text, m.Text())
		assert.False(t, m.AutocompleteVisible())
	}
	assert.Equal(t, 8, m.Cursor())
	m.Undo()
	assert.Equal(t, "draft\nbody", m.Text())
	assert.Equal(t, 8, m.Cursor())
	keybindings.SetDefaultManager(keybindings.NewDefault(nil))
	p.HandleKey(KeyEvent{Key: KeyF2})
	assert.Equal(t, "draft\nbody", m.Text())
}

func TestEditorPrimitiveArrowHistoryRequiresBoundaryAndStart(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(nil))
	p := &EditorPrimitive{}
	m := p.ensureModel()
	m.AddHistory("history\nentry")
	m.SetText("draft")
	p.Render(20)
	p.HandleKey(KeyEvent{Key: KeyUp})
	assert.Equal(t, "draft", m.Text())
	assert.Zero(t, m.Cursor())
	p.HandleKey(KeyEvent{Key: KeyUp})
	assert.Equal(t, "history\nentry", m.Text())
	assert.Zero(t, m.Cursor())
	p.HandleKey(KeyEvent{Key: KeyDown})
	assert.Equal(t, "history\nentry", m.Text())
	p.HandleKey(KeyEvent{Key: KeyDown})
	assert.Equal(t, "draft", m.Text())
	assert.Zero(t, m.Cursor())
	p.HandleKey(KeyEvent{Key: KeyDown})
	assert.Equal(t, "draft", m.Text())
	assert.Equal(t, 5, m.Cursor())
}

func TestEditorPrimitiveSlashConfirmationSubmitsOnlyThroughSubmitKey(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	for _, tc := range []struct {
		prefix, value string
		key           Key
		config        keybindings.Config
		wantSubmit    bool
	}{
		{"/he", "/help", KeyEnter, nil, true},
		{"/he", "/help", KeyTab, nil, false},
		{"@fi", "@file", KeyEnter, nil, false},
		{"/he", "/help", KeyF2, keybindings.Config{"tui.select.confirm": {"f2"}}, false},
		{"/he", "/help", KeyF2, keybindings.Config{"tui.select.confirm": {"f2"}, "tui.input.submit": {"f2"}}, true},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			keybindings.SetDefaultManager(keybindings.NewDefault(tc.config))
			var submitted []string
			p := &EditorPrimitive{OnSubmit: func(s string) { submitted = append(submitted, s) }}
			m := p.ensureModel()
			m.SetText(tc.prefix)
			m.SetAutocomplete(&autocomplete.Suggestions{Prefix: tc.prefix, Items: []autocomplete.Item{{Value: strings.TrimPrefix(tc.value, "/")}}})
			p.HandleKey(KeyEvent{Key: tc.key})
			assert.False(t, m.AutocompleteVisible())
			if tc.wantSubmit {
				assert.Equal(t, []string{tc.value}, submitted)
				assert.Empty(t, m.Text())
			} else {
				assert.Empty(t, submitted)
				assert.Equal(t, tc.value+" ", m.Text())
				m.Undo()
				assert.Equal(t, tc.prefix, m.Text())
			}
		})
	}
}

func TestEditorPrimitiveSubmitsWhitespaceAsEmptyAfterReset(t *testing.T) {
	p := &EditorPrimitive{}
	p.ensureModel().SetText(" \t\n")
	calls := 0
	p.OnSubmit = func(value string) { calls++; assert.Empty(t, value); assert.Empty(t, p.ensureModel().Text()) }
	p.HandleKey(KeyEvent{Key: KeyEnter})
	assert.Equal(t, 1, calls)
	p.HandleInput("\x1b[45;5u")
	assert.Empty(t, p.ensureModel().Text())
}
