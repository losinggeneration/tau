package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/tui/autocomplete"
	"github.com/losinggeneration/tau/tui/keybindings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestEditorPrimitiveTabRequestsCompletion(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.input.tab": {"f2"}}))
	for _, names := range [][]string{{"note.md"}, {"note.md", "notes.md"}} {
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range names {
				assert.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
			}
			var submitted bool
			p := &EditorPrimitive{OnSubmit: func(string) { submitted = true }}
			m := p.ensureModel()
			provider, err := autocomplete.NewCombinedProvider(autocomplete.WithBasePath(dir))
			assert.NoError(t, err)
			m.SetAutocompleteProvider(provider)
			text := "前文\n./no 后文"
			m.SetText(text)
			m.SetCursor(len(text) - len(" 后文"))
			p.HandleKey(KeyEvent{Key: KeyTab})
			assert.Equal(t, text, m.Text())
			assert.False(t, m.AutocompleteVisible())
			p.HandleKey(KeyEvent{Key: KeyF2})
			if len(names) == 1 {
				assert.Equal(t, "前文\n./note.md 后文", m.Text())
				assert.Equal(t, len("前文\n./note.md"), m.Cursor())
				assert.False(t, m.AutocompleteVisible())
				m.Undo()
				assert.Equal(t, text, m.Text())
			} else {
				assert.True(t, m.AutocompleteVisible())
				assert.Equal(t, text, m.Text())
				p.HandleKey(KeyEvent{Key: KeyF2})
				assert.Equal(t, "前文\n./note.md 后文", m.Text())
			}
			assert.False(t, submitted)
		})
	}
	p := &EditorPrimitive{}
	m := p.ensureModel()
	provider, err := autocomplete.NewCombinedProvider(autocomplete.WithCommands([]autocomplete.SlashCommand{{Name: "help"}}), autocomplete.WithBasePath(t.TempDir()))
	assert.NoError(t, err)
	m.SetAutocompleteProvider(provider)
	m.SetText("/hel")
	p.HandleKey(KeyEvent{Key: KeyF2})
	assert.True(t, m.AutocompleteVisible())
	assert.Equal(t, "/hel", m.Text())
	p.HandleKey(KeyEvent{Key: KeyF2})
	assert.Equal(t, "/help ", m.Text())
}

func TestEditorPrimitiveRefreshesOpenCompletionAfterEditsAndMovement(t *testing.T) {
	for _, mode := range []string{"regular", "forced"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"note.md", "notes.md"} {
				assert.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
			}
			p := &EditorPrimitive{}
			m := p.ensureModel()
			provider, err := autocomplete.NewCombinedProvider(autocomplete.WithBasePath(dir), autocomplete.WithCommands([]autocomplete.SlashCommand{{Name: "help"}, {Name: "history"}}))
			assert.NoError(t, err)
			m.SetAutocompleteProvider(provider)
			if mode == "regular" {
				m.SetText("/h")
			} else {
				m.SetText("./no")
			}
			p.HandleKey(KeyEvent{Key: KeyTab})
			assert.True(t, m.AutocompleteVisible())
			if mode == "regular" {
				p.HandleInput("e")
				assert.Equal(t, "/he", m.Text())
				assert.Len(t, m.VisibleAutocompleteItems(), 1)
				assert.Equal(t, "help", m.VisibleAutocompleteItems()[0].Value)
				p.HandleKey(KeyEvent{Key: KeyHome})
				assert.False(t, m.AutocompleteVisible())
			} else {
				p.HandleInput("te.")
				assert.Equal(t, "./note.", m.Text())
				assert.Len(t, m.VisibleAutocompleteItems(), 1)
				assert.Equal(t, "./note.md", m.VisibleAutocompleteItems()[0].Value)
				p.HandleKey(KeyEvent{Key: KeyHome})
				assert.True(t, m.AutocompleteVisible())
				assert.Len(t, m.VisibleAutocompleteItems(), 2)
				assert.Zero(t, m.Cursor())
			}
		})
	}
}

func TestEditorPrimitiveTriggersFirstLineSlashCompletion(t *testing.T) {
	for _, prefix := range []string{"", "  ", "\ufeff"} {
		t.Run(prefix, func(t *testing.T) {
			p := &EditorPrimitive{}
			m := p.ensureModel()
			provider, err := autocomplete.NewCombinedProvider(autocomplete.WithCommands([]autocomplete.SlashCommand{{Name: "help"}, {Name: "history"}}))
			assert.NoError(t, err)
			m.SetAutocompleteProvider(provider)
			m.SetText(prefix)
			p.HandleInput("/")
			assert.True(t, m.AutocompleteVisible())
			p.HandleInput("he")
			assert.True(t, m.AutocompleteVisible())
			assert.Len(t, m.VisibleAutocompleteItems(), 1)
			p.HandleInput("z")
			assert.False(t, m.AutocompleteVisible())
			p.HandleKey(KeyEvent{Key: KeyBackspace})
			assert.True(t, m.AutocompleteVisible())
			assert.Equal(t, prefix+"/he", m.Text())
			m.CancelAutocomplete()
			m.SetCursor(m.Cursor() - 1)
			p.HandleKey(KeyEvent{Key: KeyDelete})
			assert.True(t, m.AutocompleteVisible())
			assert.Equal(t, prefix+"/h", m.Text())
		})
	}
	for _, prefix := range []string{"前文\n", "ordinary ", "\u0085"} {
		p := &EditorPrimitive{}
		m := p.ensureModel()
		provider, err := autocomplete.NewCombinedProvider(autocomplete.WithCommands([]autocomplete.SlashCommand{{Name: "help"}}))
		assert.NoError(t, err)
		m.SetAutocompleteProvider(provider)
		m.SetText(prefix)
		p.HandleInput("/hel")
		assert.False(t, m.AutocompleteVisible())
	}
}

func TestEditorPrimitiveTriggersAttachmentCompletion(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"note.md", "other.md"} {
		assert.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	for _, prefix := range []string{"", "later\n", "文字（", "`"} {
		t.Run(prefix, func(t *testing.T) {
			p := &EditorPrimitive{}
			m := p.ensureModel()
			provider, err := autocomplete.NewCombinedProvider(autocomplete.WithBasePath(dir))
			assert.NoError(t, err)
			m.SetAutocompleteProvider(provider)
			m.SetText(prefix)
			p.HandleInput("@")
			assert.True(t, m.AutocompleteVisible())
			assert.Len(t, m.VisibleAutocompleteItems(), 2)
			p.HandleInput("note")
			assert.True(t, m.AutocompleteVisible())
			require.Len(t, m.VisibleAutocompleteItems(), 1)
			assert.Equal(t, "@note.md", m.VisibleAutocompleteItems()[0].Value)
			assert.Equal(t, prefix+"@note", m.Text())
			p.HandleInput("z")
			assert.False(t, m.AutocompleteVisible())
			p.HandleKey(KeyEvent{Key: KeyBackspace})
			assert.True(t, m.AutocompleteVisible())
			require.Len(t, m.VisibleAutocompleteItems(), 1)
		})
	}
	p := &EditorPrimitive{}
	m := p.ensureModel()
	provider, err := autocomplete.NewCombinedProvider(autocomplete.WithBasePath(dir))
	assert.NoError(t, err)
	m.SetAutocompleteProvider(provider)
	p.HandleInput("email@note")
	assert.False(t, m.AutocompleteVisible())
	assert.Equal(t, "email@note", m.Text())
}

type symbolSuggestionProvider struct {
	triggers []rune
	requests []string
}

func (p *symbolSuggestionProvider) TriggerCharacters() []rune { return p.triggers }
func (p *symbolSuggestionProvider) GetSuggestions(_ context.Context, lines []string, line, col int, _ bool) (*autocomplete.Suggestions, error) {
	p.requests = append(p.requests, lines[line][:col])
	return nil, nil
}

func TestEditorPrimitiveUsesProviderSymbolTriggers(t *testing.T) {
	for _, trigger := range []string{"@", "#", "$", "-"} {
		for _, before := range []string{"", "later\n", "查看，", "see ([", "`"} {
			t.Run(before+trigger, func(t *testing.T) {
				p := &EditorPrimitive{}
				provider := &symbolSuggestionProvider{triggers: []rune{'$', '-'}}
				m := p.ensureModel()
				m.SetAutocompleteProvider(provider)
				m.SetText(before)
				p.HandleInput(trigger)
				require.Equal(t, []string{strings.TrimPrefix(before, "later\n") + trigger}, provider.requests)
				p.HandleInput("r")
				assert.Len(t, provider.requests, 2)
			})
		}
	}
	for _, value := range []string{"email@src", "foo(@src", "问题#12", "=@src", "'@src", "./src", "$item"} {
		p := &EditorPrimitive{}
		provider := &symbolSuggestionProvider{}
		p.ensureModel().SetAutocompleteProvider(provider)
		p.HandleInput(value)
		assert.Empty(t, provider.requests, value)
	}
	p := &EditorPrimitive{}
	m := p.ensureModel()
	m.SetAutocompleteProvider(&symbolSuggestionProvider{triggers: []rune{'$'}})
	provider := &symbolSuggestionProvider{}
	m.SetAutocompleteProvider(provider)
	p.HandleInput("$item")
	assert.Empty(t, provider.requests)
}

func TestEditorPrimitiveSlashOnlyStartsCommandAtMessageStart(t *testing.T) {
	for _, prefix := range []string{"/inspect", "/inspect arg", "/inspect path", "first\n"} {
		t.Run(prefix, func(t *testing.T) {
			p := &EditorPrimitive{}
			provider := &symbolSuggestionProvider{}
			m := p.ensureModel()
			m.SetAutocompleteProvider(provider)
			m.SetText(prefix)
			p.HandleInput("/")
			assert.Empty(t, provider.requests)
			assert.Equal(t, prefix+"/", m.Text())
		})
	}
	p := &EditorPrimitive{}
	provider := &symbolSuggestionProvider{}
	m := p.ensureModel()
	m.SetAutocompleteProvider(provider)
	m.SetText(" \ufeff")
	p.HandleInput("/")
	assert.Equal(t, []string{" \ufeff/"}, provider.requests)
}

func TestEditorPrimitiveRetriesCompletionForCJKLetters(t *testing.T) {
	for _, letter := range []string{"说", "あ", "カ", "한", "ㄅ", "𠮷"} {
		t.Run(letter, func(t *testing.T) {
			p := &EditorPrimitive{}
			provider := &symbolSuggestionProvider{}
			m := p.ensureModel()
			m.SetAutocompleteProvider(provider)
			m.SetText("查看，@dir/")
			p.HandleInput(letter)
			assert.Equal(t, []string{"查看，@dir/" + letter}, provider.requests)
			assert.Equal(t, "查看，@dir/"+letter, m.Text())
		})
	}
	dir := t.TempDir()
	for _, name := range []string{"文档", "我的 文档", "资料，归档"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name, "说明.md"), nil, 0o600))
		t.Run(name, func(t *testing.T) {
			p := &EditorPrimitive{}
			m := p.ensureModel()
			provider, err := autocomplete.NewCombinedProvider(autocomplete.WithBasePath(dir))
			require.NoError(t, err)
			m.SetAutocompleteProvider(provider)
			prefix := "@文"
			if name != "文档" {
				prefix = "@\"" + name
			}
			m.SetText(prefix)
			value := "@" + name + "/"
			if name != "文档" {
				value = "@\"" + name + "/\""
			}
			m.SetAutocomplete(&autocomplete.Suggestions{Prefix: prefix, Items: []autocomplete.Item{{Value: value, Label: name + "/"}}})
			p.HandleKey(KeyEvent{Key: KeyTab})
			require.False(t, m.AutocompleteVisible())
			p.HandleInput("说")
			require.True(t, m.AutocompleteVisible())
			require.Len(t, m.VisibleAutocompleteItems(), 1)
			assert.Contains(t, m.VisibleAutocompleteItems()[0].Value, "说明.md")
			assert.Contains(t, m.Text(), "/说")
			assert.NotContains(t, m.Text(), "说明.md")
		})
	}
}

func TestEditorPrimitiveSymbolInsertionDoesNotRetrySlashContext(t *testing.T) {
	for _, symbol := range []rune{'@', '#', '$', '-'} {
		t.Run(string(symbol), func(t *testing.T) {
			p := &EditorPrimitive{}
			provider := &symbolSuggestionProvider{triggers: []rune{'$', '-'}}
			m := p.ensureModel()
			m.SetAutocompleteProvider(provider)
			m.SetText("/inspect")
			p.HandleInput(string(symbol))
			assert.Empty(t, provider.requests)
			assert.Equal(t, "/inspect"+string(symbol), m.Text())
		})
	}
	p := &EditorPrimitive{}
	provider := &symbolSuggestionProvider{triggers: []rune{'-'}}
	m := p.ensureModel()
	m.SetAutocompleteProvider(provider)
	m.SetText("/inspect ")
	p.HandleInput("-")
	assert.Equal(t, []string{"/inspect -"}, provider.requests)
}

func TestEditorPrimitiveNewlineDoesNotRefreshCompletion(t *testing.T) {
	p := &EditorPrimitive{}
	m := p.ensureModel()
	provider := &symbolSuggestionProvider{}
	m.SetAutocompleteProvider(provider)
	m.SetText("/he")
	m.SetAutocomplete(&autocomplete.Suggestions{Prefix: "/he", Items: []autocomplete.Item{{Value: "help"}}})
	p.HandleKey(KeyEvent{Key: KeyEnter, Mod: ModShift})
	assert.Equal(t, "/he\n", m.Text())
	assert.False(t, m.AutocompleteVisible())
	assert.Empty(t, provider.requests)
}

func TestEditorPrimitiveRetriesCompletionForSharedCJKCharacters(t *testing.T) {
	for _, letter := range []string{"ー", "ｰ", "゙", "ˇ", "〆", "㆐", "㋿", "𝍠", "🉐"} {
		t.Run(letter, func(t *testing.T) {
			p := &EditorPrimitive{}
			provider := &symbolSuggestionProvider{}
			m := p.ensureModel()
			m.SetAutocompleteProvider(provider)
			m.SetText("@dir/")
			p.HandleInput(letter)
			assert.Equal(t, []string{"@dir/" + letter}, provider.requests)
			assert.Equal(t, "@dir/"+letter, m.Text())
		})
	}
}
