package tui

import (
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
)

func newEditorChromeApp(t *testing.T) *App {
	t.Helper()
	a := newTestApp(t)
	a.editor = tauui.NewPromptEditor()
	a.footer = NewFooter(&footerData{})
	return a
}

// The editor's rules are the standing thinking-level indicator, so changing the
// level has to reach the editor and not just the footer label.
func TestRefreshEditorChromeTracksThinkingLevel(t *testing.T) {
	a := newEditorChromeApp(t)
	tokens := a.chat.ThemeTokens()

	a.footer.data.thinkingLevel = "off"
	a.refreshEditorChrome()
	off := paintedRuleStyle(t, a.editor)
	if off.FG != tokens.ThinkingOff {
		t.Fatalf("thinking off: rule FG = %#v, want the thinkingOff token %#v", off.FG, tokens.ThinkingOff)
	}

	a.footer.data.thinkingLevel = "high"
	a.refreshEditorChrome()
	high := paintedRuleStyle(t, a.editor)
	if high.FG != tokens.ThinkingHigh {
		t.Fatalf("thinking high: rule FG = %#v, want the thinkingHigh token %#v", high.FG, tokens.ThinkingHigh)
	}
	if high.FG == off.FG {
		t.Fatalf("thinking level change left the editor rule unchanged (%#v)", high.FG)
	}
}

// A theme switch must move the editor too, not only the chat log.
func TestRefreshEditorChromeFollowsThemeTokens(t *testing.T) {
	a := newEditorChromeApp(t)
	a.footer.data.thinkingLevel = "off"

	a.chat.SetTheme("default", map[string]string{"thinkingOff": "#0a0b0c"})
	a.refreshEditorChrome()

	if got, want := paintedRuleStyle(t, a.editor).FG, tauui.ColorRGB(0x0a, 0x0b, 0x0c); got != want {
		t.Fatalf("rule FG = %#v, want the overridden thinkingOff token %#v", got, want)
	}
}

// Root.Layout is where the editor learns the terminal height; without it the
// 30% cap has nothing to bound.
func TestRootLayoutPublishesTerminalHeightToEditor(t *testing.T) {
	a := newEditorChromeApp(t)
	a.editor.SetText(nil, longPrompt(200))

	root := &Root{
		AppLayout: tauui.NewAppLayout(tauui.AppLayoutOpts{Editor: a.editor}),
		app:       a,
	}
	root.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 60})

	got := a.editor.MinSize().H
	if want := 60 * tauui.PromptEditorMaxHeightPercent / 100; got != want {
		t.Fatalf("editor height = %d after layout on a 60-row terminal, want the cap %d", got, want)
	}
}

func longPrompt(lines int) string {
	out := make([]byte, 0, lines*5)
	for range lines {
		out = append(out, "line\n"...)
	}
	return string(out)
}

// paintedRuleStyle paints the editor and returns the style its top rule used.
func paintedRuleStyle(t *testing.T, editor *tauui.PromptEditor) tauui.Style {
	t.Helper()
	const w, h = 20, 7
	editor.Layout(tauui.Rect{X: 0, Y: 0, W: w, H: h})
	buf := tauui.NewBuffer(w, h)
	p := tauui.NewRenderPainter(buf, tauui.Rect{W: w, H: h}, tauui.Style{})
	editor.Paint(tauui.NewDrawer(tauui.NewPainter(p, tauui.Style{})), &tauui.Ctx{Theme: tauui.DefaultThemeModern()})
	cell := buf.At(0, 0)
	if cell == nil {
		t.Fatalf("editor painted no top-left cell")
	}
	return cell.Style
}

func TestRefreshSelectorPlainTypingSkipsCompletionWork(t *testing.T) {
	sess := &fakeSession{}
	a := newTestApp(t)
	a.sess = sess
	a.editor = newPromptEditorForTest()
	a.selector = tauui.NewSelector()
	a.dialogPicker = tauui.NewDialogPicker()

	ctx := stubCtx()
	for _, text := range []string{"h", "he", "hel", "hello world"} {
		a.refreshSelector(text, ctx)
	}

	if sess.slashCalls != 0 {
		t.Fatalf("plain typing called SlashCommands %d times; want 0", sess.slashCalls)
	}
	if a.selector.Shown() {
		t.Fatal("selector shown for plain typing")
	}
}

func TestRefreshSelectorAtCompletionUsesSessionCommandsOnce(t *testing.T) {
	dir := t.TempDir()
	sess := &fakeSession{stateCWD: dir}
	a := newTestApp(t)
	a.sess = sess
	a.editor = newPromptEditorForTest()
	a.selector = tauui.NewSelector()
	a.dialogPicker = tauui.NewDialogPicker()

	a.editor.Model().SetText("@")
	a.refreshSelector("@", stubCtx())

	if sess.slashCalls != 1 {
		t.Fatalf("@ completion called SlashCommands %d times; want 1", sess.slashCalls)
	}
}

func TestEditorDialogRegionSwitchesBetweenEditorAndDialog(t *testing.T) {
	editor := tauui.NewPromptEditor()
	frame := editor
	dialog := tauui.NewDialogPicker()
	region := NewEditorDialogRegion(frame, dialog, nil)

	if got, want := region.MinSize(), frame.MinSize(); got != want {
		t.Fatalf("hidden dialog MinSize = %+v, want editor frame %+v", got, want)
	}

	r := tauui.Rect{X: 2, Y: 3, W: 50, H: 7}
	region.Layout(r)
	if got := frame.Rect(); got != r {
		t.Fatalf("hidden dialog should layout editor frame at region rect: got %+v want %+v", got, r)
	}

	dialog.SetHeader("/settings")
	dialog.SetShown(true)
	if got, want := region.MinSize(), dialog.MinSize(); got != want {
		t.Fatalf("shown dialog MinSize = %+v, want dialog %+v", got, want)
	}
	region.Layout(r)
	if got := dialog.Rect(); got != r {
		t.Fatalf("shown dialog should layout picker at region rect: got %+v want %+v", got, r)
	}
	if got := frame.Rect(); got == r {
		t.Fatalf("shown dialog should replace editor frame, but frame still owns active rect %+v", got)
	}
}

// shownSelector returns a selector with items, so it reports a non-zero
// MinSize and participates in layout.
func shownSelector(t *testing.T) *tauui.Selector {
	t.Helper()
	s := tauui.NewSelector()
	s.SetItems([]tauui.SelectorItem{{Name: "/help"}, {Name: "/model"}})
	s.SetShown(true)
	if s.MinSize().H <= 0 {
		t.Fatalf("selector fixture reports zero height; it cannot exercise placement")
	}
	return s
}

// Autocomplete is editor chrome: it must sit directly under the bottom rule and
// share the editor's content column, not float in the widget regions.
func TestEditorDialogRegionPlacesSelectorBelowEditor(t *testing.T) {
	editor := tauui.NewPromptEditor()
	selector := shownSelector(t)
	region := NewEditorDialogRegion(editor, tauui.NewDialogPicker(), selector)

	editorH := editor.MinSize().H
	selectorH := selector.MinSize().H
	if got, want := region.MinSize().H, editorH+selectorH; got != want {
		t.Fatalf("region MinSize.H = %d, want editor+selector (%d)", got, want)
	}

	r := tauui.Rect{X: 2, Y: 3, W: 50, H: editorH + selectorH}
	region.Layout(r)

	if got := editor.Rect().H; got != editorH {
		t.Fatalf("editor height = %d, want %d — the selector must not eat editor rows", got, editorH)
	}
	sel := selector.Rect()
	if want := r.Y + editorH; sel.Y != want {
		t.Fatalf("selector Y = %d, want %d (immediately below the editor's bottom rule)", sel.Y, want)
	}
	if want := r.X + editor.Padding(); sel.X != want {
		t.Fatalf("selector X = %d, want %d (aligned to the editor content column)", sel.X, want)
	}
	if want := r.W - 2*editor.Padding(); sel.W != want {
		t.Fatalf("selector W = %d, want %d (editor content width)", sel.W, want)
	}
}

// A dialog owns its own input, so an inline completion list underneath would be
// answering a prompt that is no longer on screen.
func TestEditorDialogRegionSuppressesSelectorUnderDialog(t *testing.T) {
	editor := tauui.NewPromptEditor()
	selector := shownSelector(t)
	dialog := tauui.NewDialogPicker()
	region := NewEditorDialogRegion(editor, dialog, selector)

	dialog.SetHeader("/settings")
	dialog.SetShown(true)

	if got, want := region.MinSize(), dialog.MinSize(); got != want {
		t.Fatalf("region MinSize = %+v under a dialog, want the dialog's %+v", got, want)
	}
	region.Layout(tauui.Rect{X: 0, Y: 0, W: 50, H: dialog.MinSize().H})
	if got := selector.Rect(); got != (tauui.Rect{}) {
		t.Fatalf("selector still laid out at %+v under a dialog, want collapsed", got)
	}
}

// A hidden selector must not reserve a row.
func TestEditorDialogRegionIgnoresHiddenSelector(t *testing.T) {
	editor := tauui.NewPromptEditor()
	selector := tauui.NewSelector()
	region := NewEditorDialogRegion(editor, tauui.NewDialogPicker(), selector)

	if got, want := region.MinSize(), editor.MinSize(); got != want {
		t.Fatalf("region MinSize = %+v with a hidden selector, want the editor's %+v", got, want)
	}
	r := tauui.Rect{X: 0, Y: 0, W: 50, H: editor.MinSize().H}
	region.Layout(r)
	if got := editor.Rect(); got != r {
		t.Fatalf("editor rect = %+v, want the whole region %+v", got, r)
	}
}
