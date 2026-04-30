package tui

import (
	"strings"
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
)

func TestPromptEditor_IsTopLevelToolkitComponent(t *testing.T) {
	a := newAppWithSelector(t)
	if a.editor == nil {
		t.Fatal("prompt editor is nil")
	}
	if a.editor.Widget() == nil {
		t.Fatal("prompt editor did not expose wrapped toolkit editor widget")
	}
}

// TestPromptEditor_SetTextAndText verifies that the prompt editor's
// SetText / Text round-trip round-trips the user input verbatim.
// This is the foundation of the prompt editor's user-visible contract.
func TestPromptEditor_SetTextAndText(t *testing.T) {
	a := newAppWithSelector(t)
	a.editor.SetText(stubCtx(), "hello world")
	if got := a.editor.Text(); got != "hello world" {
		t.Errorf("editor.Text() = %q, want %q", got, "hello world")
	}
}

// TestPromptEditor_SetTextReplaces verifies that SetText replaces
// the editor content wholesale. The Pi prompt editor contract
// requires that internal edits from the selector never leak into
// future SetText calls.
func TestPromptEditor_SetTextReplaces(t *testing.T) {
	a := newAppWithSelector(t)
	a.editor.SetText(stubCtx(), "alpha")
	a.editor.SetText(stubCtx(), "beta")
	if got := a.editor.Text(); got != "beta" {
		t.Errorf("SetText did not replace; got %q, want %q", got, "beta")
	}
}

// TestPromptEditor_ClearViaSetEmpty verifies the empty-string
// SetText clears the editor. The submit pipeline uses this pattern
// after dispatching the prompt.
func TestPromptEditor_ClearViaSetEmpty(t *testing.T) {
	a := newAppWithSelector(t)
	a.editor.SetText(stubCtx(), "non-empty")
	a.editor.SetText(stubCtx(), "")
	if got := a.editor.Text(); got != "" {
		t.Errorf("editor.Text() should be empty after SetText(\"\"); got %q", got)
	}
}

// TestPromptEditor_RefreshSelectorUsesEditorText verifies the
// authoritative pipeline: the editor's text drives the inline
// selector's filtering. This is the L923 contract that the slash
// popup responds to the editor's current content.
func TestPromptEditor_RefreshSelectorUsesEditorText(t *testing.T) {
	a := newAppWithSelector(t)
	a.editor.SetText(stubCtx(), "/help")
	a.refreshSelector(a.editor.Text(), stubCtx())
	if !a.selector.Shown() {
		t.Fatal("selector should be visible after typing /help")
	}
	if a.selectorMode != selectorModeCommands {
		t.Errorf("selector mode = %d, want commands", a.selectorMode)
	}
}

// TestPromptEditor_NewlineIsLiteralCharacter verifies that the
// SetText path stores embedded newlines so that Submit can ship
// multi-line input. The Pi prompt editor contract allows literal
// newlines via Shift+Enter; SetText is the test-side shorthand.
func TestPromptEditor_NewlineIsLiteralCharacter(t *testing.T) {
	a := newAppWithSelector(t)
	a.editor.SetText(stubCtx(), "first\nsecond")
	got := a.editor.Text()
	if !strings.Contains(got, "\n") {
		t.Fatalf("editor should preserve newline, got %q", got)
	}
	if len(strings.Split(got, "\n")) != 2 {
		t.Errorf("expected 2 lines after newline split, got %q", got)
	}
}

// TestPromptEditor_UnicodeRoundTrip verifies the editor stores and
// returns unicode (CJK, accented, emoji) verbatim. This is the
// spec's correctness requirement for non-ASCII input.
func TestPromptEditor_UnicodeRoundTrip(t *testing.T) {
	a := newAppWithSelector(t)
	a.editor.SetText(stubCtx(), "héllo 世界 🌍")
	if got := a.editor.Text(); got != "héllo 世界 🌍" {
		t.Errorf("unicode round-trip failed; got %q", got)
	}
}

// TestPromptEditor_EmptyOnFreshApp verifies a freshly created App
// has an empty editor buffer; this is the spec's baseline state.
func TestPromptEditor_EmptyOnFreshApp(t *testing.T) {
	a := newAppWithSelector(t)
	if a.editor.Text() != "" {
		t.Errorf("fresh editor should be empty, got %q", a.editor.Text())
	}
}

func TestPromptEditorPiStyleChrome(t *testing.T) {
	editor := tauui.NewPromptEditor()
	frame := editor
	frame.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 5})

	buf := tauui.NewBuffer(40, 5)
	clip := tauui.Rect{X: 0, Y: 0, W: 40, H: 5}
	painter := tauui.NewRenderPainter(buf, clip, tauui.Style{})
	drawer := tauui.NewDrawer(tauui.NewPainter(painter, tauui.Style{}))
	frame.Paint(drawer, &tauui.Ctx{Theme: appTheme()})

	rows := make([]string, 0, 5)
	for y := range 5 {
		var row strings.Builder
		for x := range 40 {
			cell := buf.At(x, y)
			if cell == nil || cell.R == 0 {
				row.WriteByte(' ')
			} else {
				row.WriteRune(cell.R)
			}
		}
		rows = append(rows, row.String())
	}
	rendered := strings.Join(rows, "\n")

	if strings.Contains(rendered, "Prompt") {
		t.Fatalf("pi-style editor chrome should not render Tau's titled prompt border: %q", rendered)
	}
	if strings.Contains(rendered, "│") || strings.Contains(rendered, "┌") || strings.Contains(rendered, "└") {
		t.Fatalf("pi-style editor chrome should not render box side/corner glyphs: %q", rendered)
	}
	if !strings.Contains(rows[0], "──") || !strings.Contains(rows[4], "──") {
		t.Fatalf("pi-style editor chrome should render horizontal top and bottom rules: %q", rendered)
	}
	if got := editor.Widget().Rect(); got.X != 1 || got.Y != 1 || got.W != 38 || got.H != 3 {
		t.Fatalf("editor inner rect = %+v, want x=1 y=1 w=38 h=3", got)
	}
}
