package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/agent/plugins"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/extensions"
)

// fakeMessageRenderer replaces the chat log's default projection for the
// registered custom message type. The Render implementation returns rows the
// chat log can install verbatim.
type fakeMessageRenderer struct {
	customType string
	rows       []string
	calls      int
	lastWid    int
}

func (f *fakeMessageRenderer) CustomType() string { return f.customType }

func (f *fakeMessageRenderer) Render(_ ChatComponent, width int) ([]string, bool) {
	f.calls++
	f.lastWid = width
	return f.rows, true
}

// fakeToolRenderer replaces the tool execution projection for a
// given tool name.
type fakeToolRenderer struct {
	tool  string
	rows  []string
	calls int
}

func (f *fakeToolRenderer) ToolName() string { return f.tool }

func (f *fakeToolRenderer) RenderShell(_ ToolExecutionComponent, width int) ([]string, bool) {
	f.calls++
	out := make([]string, len(f.rows))
	for i, r := range f.rows {
		if width > 0 {
			out[i] = r + strings.Repeat(" ", max(0, width-len(r)))
		} else {
			out[i] = r
		}
	}
	return out, true
}

func TestPluginRenderers_RegisterMessageAndLookup(t *testing.T) {
	r := newPluginRenderers()
	ren := &fakeMessageRenderer{customType: "weather", rows: []string{"hello"}}
	r.RegisterMessage(ren)
	got := r.LookupMessage("weather")
	if got != ren {
		t.Fatalf("LookupMessage(weather) = %v, want registered renderer", got)
	}
	if r.LookupMessage("nonexistent") != nil {
		t.Fatalf("expected nil for unregistered custom type")
	}
}

func TestPluginRenderers_RegisterToolAndLookup(t *testing.T) {
	r := newPluginRenderers()
	tr := &fakeToolRenderer{tool: "bash", rows: []string{"$ ls"}}
	r.RegisterTool(tr)
	got := r.LookupTool("bash")
	if got != tr {
		t.Fatalf("LookupTool(bash) = %v, want registered renderer", got)
	}
	if r.LookupTool("nope") != nil {
		t.Fatalf("expected nil for unregistered tool")
	}
}

func TestPluginRenderers_ApplyForMessageComponent(t *testing.T) {
	r := newPluginRenderers()
	ren := &fakeMessageRenderer{customType: "weather", rows: []string{"hello", "world"}}
	r.RegisterMessage(ren)

	comp := ChatComponent{Kind: ChatComponentCustom, Custom: CustomComponent{SourceID: "weather"}}
	rows, replace := r.applyPluginRenderers(comp, 40)
	if !replace {
		t.Fatalf("expected replace=true")
	}
	if len(rows) != 2 || rows[0] != "hello" || rows[1] != "world" {
		t.Fatalf("rows = %v, want [hello world]", rows)
	}
	if ren.calls != 1 {
		t.Fatalf("renderer calls = %d, want 1", ren.calls)
	}
	if ren.lastWid != 40 {
		t.Fatalf("renderer width = %d, want 40", ren.lastWid)
	}
}

func TestPluginRenderers_ApplyForToolComponent(t *testing.T) {
	r := newPluginRenderers()
	tr := &fakeToolRenderer{tool: "bash", rows: []string{"$ ls"}}
	r.RegisterTool(tr)

	comp := ChatComponent{
		Kind:          ChatComponentToolExecution,
		ToolExecution: ToolExecutionComponent{Name: "bash"},
	}
	rows, replace := r.applyPluginRenderers(comp, 30)
	if !replace {
		t.Fatalf("expected replace=true")
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if tr.calls != 1 {
		t.Fatalf("tool renderer calls = %d, want 1", tr.calls)
	}
}

func TestPluginRenderers_ApplyFallsBackWhenUnmatched(t *testing.T) {
	r := newPluginRenderers()
	comp := ChatComponent{Kind: ChatComponentUser, Role: RoleUser}
	rows, replace := r.applyPluginRenderers(comp, 40)
	if replace || rows != nil {
		t.Fatalf("expected fallback (nil,false); got (%v,%v)", rows, replace)
	}
}

func TestChatLog_ApplyPluginComponentReplacesDefaultRows(t *testing.T) {
	conv := NewConversationModel()
	conv.AddMessage("weather", "hello world")
	c := NewChatLog(conv)
	t.Cleanup(func() { _ = c.Close() })

	r := newPluginRenderers()
	ren := &fakeMessageRenderer{customType: "weather", rows: []string{"replaced"}}
	r.RegisterMessage(ren)
	c.setPluginRenderers(r)

	// Manually run a single-component render through the cached path. The
	// plugin renderer should win because the component's custom type is
	// registered.
	snap := conv.Snapshot()
	if len(snap.Components) == 0 {
		t.Fatalf("expected at least one component")
	}
	lines, tailLine, _ := c.renderComponent(context.Background(), snap.Components[0], 40)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	if lines[0].text != "replaced" && !strings.HasPrefix(lines[0].text, "replaced") {
		t.Fatalf("line text = %q, want prefix 'replaced'", lines[0].text)
	}
	if tailLine < 1 {
		t.Fatalf("tailLine = %d, want >= 1", tailLine)
	}
}

func TestChatLog_NoRendererFallsBackToDefault(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hello")
	c := NewChatLog(conv)
	t.Cleanup(func() { _ = c.Close() })

	r := newPluginRenderers()
	c.setPluginRenderers(r)

	snap := conv.Snapshot()
	if len(snap.Components) == 0 {
		t.Fatalf("expected at least one component")
	}
	lines, _, _ := c.renderComponent(context.Background(), snap.Components[0], 40)
	if len(lines) == 0 {
		t.Fatalf("expected default fallback to produce rows")
	}
	// Default user rows have BG style; verify the line was
	// produced via the default path (text not empty).
	if lines[0].text == "" {
		t.Fatalf("expected non-empty default fallback row")
	}
}

func TestExtensions_WidgetsByID(t *testing.T) {
	r := extensions.New()
	if err := r.RegisterWidget(extensions.Widget{
		ID:        "header-a",
		Placement: extensions.Header,
		Render:    func(int) []string { return []string{"row-a"} },
	}); err != nil {
		t.Fatalf("RegisterWidget(header-a): %v", err)
	}
	if err := r.RegisterWidget(extensions.Widget{
		ID:        "footer-a",
		Placement: extensions.Footer,
		Render:    func(int) []string { return []string{"row-f"} },
	}); err != nil {
		t.Fatalf("RegisterWidget(footer-a): %v", err)
	}

	hdr := r.WidgetsByID(extensions.Header, 80)
	if len(hdr) != 1 {
		t.Fatalf("header widgets = %d, want 1", len(hdr))
	}
	if rows := hdr["header-a"]; len(rows) != 1 || rows[0] != "row-a" {
		t.Fatalf("header-a rows = %v, want [row-a]", rows)
	}

	ftr := r.WidgetsByID(extensions.Footer, 80)
	if len(ftr) != 1 {
		t.Fatalf("footer widgets = %d, want 1", len(ftr))
	}
	if rows := ftr["footer-a"]; len(rows) != 1 || rows[0] != "row-f" {
		t.Fatalf("footer-a rows = %v, want [row-f]", rows)
	}

	ids := r.WidgetIDs(extensions.Header)
	if len(ids) != 1 || ids[0] != "header-a" {
		t.Fatalf("header ids = %v, want [header-a]", ids)
	}
}

// TestPluginExtensionWiring_EditorTextExercisesLiveEditor verifies
// the App's plugin extension Registry is wired so a plugin's
// SetEditorText and InsertEditorText calls flow into the live
// prompt editor. The plugin surface in extensions.Registry owns the
// schema; the App provides the adapter that bridges the schema to
// the editor widget.
func TestPluginExtensionWiring_EditorTextExercisesLiveEditor(t *testing.T) {
	app := newAppWithSelector(t)
	app.extensions = extensions.New()

	app.extensions.SetEditorText("plugin wrote this")
	app.applyEditorTextFromExtension(app.extensions.EditorText())
	if got := app.editor.Text(); got != "plugin wrote this" {
		t.Errorf("editor text after SetEditorText = %q, want 'plugin wrote this'", got)
	}

	// Insert at position 0 should prepend; insert at end appends.
	app.extensions.SetEditorText("base")
	app.applyInsertEditorTextFromExtension(0, "X-")
	if got := app.editor.Text(); got != "X-base" {
		t.Errorf("editor text after Insert(0) = %q, want 'X-base'", got)
	}
	app.applyInsertEditorTextFromExtension(len(app.editor.Text()), "-Y")
	if got := app.editor.Text(); got != "X-base-Y" {
		t.Errorf("editor text after Insert(end) = %q, want 'X-base-Y'", got)
	}

	// Negative positions clamp to start; out-of-range positions
	// clamp to end.
	app.applyInsertEditorTextFromExtension(-3, "!")
	if got := app.editor.Text(); !strings.HasPrefix(got, "!") {
		t.Errorf("editor text after Insert(-3) = %q, want leading '!'", got)
	}
}

// TestPluginExtensionWiring_PasteProviderRoutesThroughEditor
// verifies the App installs a live PasteProvider on the plugin
// Registry so plugin-initiated pastes flow through the editor
// rather than no-oping.
func TestPluginExtensionWiring_PasteProviderRoutesThroughEditor(t *testing.T) {
	app := newAppWithSelector(t)
	app.extensions = extensions.New()
	app.extensions.SetPasteProvider(&appEditorPasteProvider{app: app})

	// Set the editor to a known baseline so we can assert the
	// paste appended correctly.
	ctx := stubCtx()
	app.editor.SetText(ctx, "")

	text, cursor := app.extensions.InsertPaste("hello world")
	if text != "hello world" {
		t.Errorf("InsertPaste text = %q, want 'hello world'", text)
	}
	if cursor != len("hello world") {
		t.Errorf("InsertPaste cursor = %d, want %d", cursor, len("hello world"))
	}
	if got := app.editor.Text(); got != "hello world" {
		t.Errorf("editor after InsertPaste = %q, want 'hello world'", got)
	}

	// ReplacePaste should overwrite the editor contents.
	text, cursor = app.extensions.ReplacePaste("replaced")
	if text != "replaced" || cursor != len("replaced") {
		t.Errorf("ReplacePaste = (%q, %d), want ('replaced', %d)", text, cursor, len("replaced"))
	}
	if got := app.editor.Text(); got != "replaced" {
		t.Errorf("editor after ReplacePaste = %q, want 'replaced'", got)
	}

	// The Registry records the most recent payload.
	if last := app.extensions.LastPaste(); last != "replaced" {
		t.Errorf("LastPaste = %q, want 'replaced'", last)
	}
}

func TestPluginExtensionWiring_InsertPasteUsesEditorPasteSemantics(t *testing.T) {
	app := newAppWithSelector(t)
	app.extensions = extensions.New()
	app.extensions.SetPasteProvider(&appEditorPasteProvider{app: app})

	largePaste := strings.Repeat("pasted line\n", 16)
	text, cursor := app.extensions.InsertPaste(largePaste)

	if !strings.Contains(text, "[paste #1 +17 lines]") {
		t.Fatalf("InsertPaste text = %q, want large-paste marker", text)
	}
	if cursor != len(text) {
		t.Fatalf("InsertPaste cursor = %d, want marker text length %d", cursor, len(text))
	}
	if got := app.editor.Model().ExpandedText(); got != largePaste {
		t.Fatalf("ExpandedText = %q, want original large paste", got)
	}
}

func TestPluginExtensionWiring_PasteDataURLInsertsImageAttachment(t *testing.T) {
	app := newAppWithSelector(t)
	dir := t.TempDir()
	app.footer = NewFooter(&footerData{cwd: dir})
	app.extensions = extensions.New()
	app.extensions.SetPasteProvider(&appEditorPasteProvider{app: app})

	payload := "data:image/png;base64," + testPNGBase64
	text, _ := app.extensions.InsertPaste(payload)
	if text != "@pasted-image.png" {
		t.Fatalf("editor text = %q, want @pasted-image.png", text)
	}
	if _, err := os.Stat(filepath.Join(dir, "pasted-image.png")); err != nil {
		t.Fatalf("pasted image not saved: %v", err)
	}
}

// plugin Registry tracks pastes a plugin handled without routing
// through the editor.
func TestPluginExtensionWiring_PasteHandledCounter(t *testing.T) {
	r := extensions.New()
	if got := r.PastesHandled(); got != 0 {
		t.Errorf("initial PastesHandled = %d, want 0", got)
	}
	r.NotifyPasteHandled()
	r.NotifyPasteHandled()
	if got := r.PastesHandled(); got != 2 {
		t.Errorf("PastesHandled = %d, want 2", got)
	}
}

// TestPluginExtensionWiring_ToolExpansionForwardsToRegistry
// verifies the chat log's tool-expansion hook is wired so plugin
// observers see the same state the chat log renders.
func TestPluginExtensionWiring_ToolExpansionForwardsToRegistry(t *testing.T) {
	app := newAppWithSelector(t)
	app.chat = NewChatLog(app.conv)
	t.Cleanup(func() { _ = app.chat.Close() })
	app.extensions = extensions.New()
	// Wire the chat log's tool-expansion hook into the extensions
	// registry so SetToolExpanded forwards the toggle. The App's
	// constructor wires this once; the test re-creates both
	// objects so the wiring has to be redone to match.
	app.chat.SetToolExpansionHandler(func(id string, expanded bool) {
		if app.extensions == nil {
			return
		}
		app.extensions.SetToolExpanded(id, expanded)
	})

	// Default is unexpanded; SetToolExpanded forwards through
	// the hook, the hook writes to the Registry.
	if app.extensions.ToolExpanded("call-1") {
		t.Error("ToolExpanded should default to false")
	}
	if !app.chat.SetToolExpanded("call-1", true) {
		t.Error("SetToolExpanded returned false; no handler wired")
	}
	if !app.extensions.ToolExpanded("call-1") {
		t.Error("plugin Registry should observe expanded=true after chat toggle")
	}
	if !app.chat.SetToolExpanded("call-1", false) {
		t.Error("SetToolExpanded returned false on second toggle")
	}
	if app.extensions.ToolExpanded("call-1") {
		t.Error("plugin Registry should observe expanded=false after collapse")
	}

	// Direct Registry write is also visible (the chat log
	// consults the Registry through SetToolExpanded/SetToolExpanded).
	app.extensions.SetToolExpanded("call-1", true)
	if !app.extensions.ToolExpanded("call-1") {
		t.Error("ToolExpanded should be true after Registry write")
	}
}

func TestPluginUISetToolsExpandedExpandsKnownToolCalls(t *testing.T) {
	app := newAppWithSelector(t)
	app.extensions = extensions.New()
	app.conv.AddToolCallWithID("call-1", "bash", `{"command":"echo hi"}`)
	app.conv.AddToolCallWithID("call-2", "read", `{"path":"README.md"}`)

	app.SetToolsExpanded(true)

	for _, id := range []string{"call-1", "call-2"} {
		if !app.extensions.ToolExpanded(id) {
			t.Fatalf("ToolExpanded(%s) = false, want true", id)
		}
	}
}

func TestTerminalInputMapping(t *testing.T) {
	input := plugins.TerminalInput{
		Key:  terminalInputKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'x', Mod: tauui.ModCtrl | tauui.ModAlt}),
		Text: terminalInputText(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'x'}),
		Mod:  terminalInputMod(tauui.ModCtrl | tauui.ModAlt),
	}
	if input.Key != "rune" || input.Text != "x" || input.Mod != "ctrl+alt" {
		t.Fatalf("terminal input mapping = %#v, want rune/x/ctrl+alt", input)
	}
}

// TestPluginExtensionWiring_ThemeRoundTrip verifies the plugin
// Registry's Set/Theme round-trips through the App's applyTheme
// helper so the toolkit repaints with the new palette.
func TestPluginExtensionWiring_ThemeRoundTrip(t *testing.T) {
	app := newAppWithSelector(t)
	app.editor = newPromptEditorForTest()
	footerData := &footerData{}
	app.footer = NewFooter(footerData)
	app.extensions = extensions.New()
	app.extensions.SetTheme(extensions.Theme{Name: "dark", Values: map[string]string{}})

	// The Registry records the theme; the App consumes it.
	if got := app.extensions.Theme().Name; got != "dark" {
		t.Errorf("Registry theme after SetTheme = %q, want 'dark'", got)
	}
	app.ensureExtensionTheme(app.extensions.Theme().Name)
	// The footer data tracks the active theme name.
	if app.footer != nil && app.footer.data != nil && app.footer.data.theme != "dark" {
		t.Errorf("footer theme after ensureExtensionTheme = %q, want 'dark'", app.footer.data.theme)
	}
}

// TestPluginExtensionWiring_ContextBackgroundSafeForPlugins
// verifies the App exposes a context safe to pass to SDK calls from
// background plugin goroutines.
func TestPluginExtensionWiring_ContextBackgroundSafeForPlugins(t *testing.T) {
	app := newAppWithSelector(t)
	ctx := app.contextBackground()
	if ctx == nil {
		t.Fatal("contextBackground returned nil")
	}
	if err := ctx.Err(); err != nil {
		t.Errorf("contextBackground err = %v, want nil", err)
	}
	// A cancelled context surfaces immediately.
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cancelCtx.Err(); err == nil {
		t.Error("cancelled context should have non-nil err")
	}
}

// TestPluginExtensionWiring_ExtensionsConfigForHelper verifies the
// extensionsConfigFor helper produces a Registry with the default
// theme seeded, matching what the App constructor does.
func TestPluginExtensionWiring_ExtensionsConfigForHelper(t *testing.T) {
	r := extensionsConfigFor()
	if r == nil {
		t.Fatal("extensionsConfigFor returned nil")
	}
	if name := r.Theme().Name; name != "default" {
		t.Errorf("seeded theme = %q, want 'default'", name)
	}
}
