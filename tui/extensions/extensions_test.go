package extensions

import (
	"testing"

	"github.com/losinggeneration/rovel/event"
	"github.com/losinggeneration/rovel/ui"
	"github.com/losinggeneration/tau/tui/keybindings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCustomRenderers(t *testing.T) {
	r := New()
	if err := r.RegisterMessageRenderer("custom", func(any) []string { return []string{"msg"} }); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterToolRenderer("tool", func(any) []string { return []string{"tool"} }); err != nil {
		t.Fatal(err)
	}
	if lines, ok := r.RenderMessage("custom", nil); !ok || lines[0] != "msg" {
		t.Fatalf("message=%v %v", lines, ok)
	}
	if lines, ok := r.RenderTool("tool", nil); !ok || lines[0] != "tool" {
		t.Fatalf("tool=%v %v", lines, ok)
	}
	if err := r.RegisterMessageRenderer("custom", func(any) []string { return nil }); err == nil {
		t.Fatalf("duplicate accepted")
	}
}

func TestShortcutsConflictAndTrigger(t *testing.T) {
	r := New()
	called := 0
	if err := r.RegisterShortcut(Shortcut{ID: "plugin.a", Key: "ctrl+k", Handler: func() { called++ }}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterShortcut(Shortcut{ID: "plugin.b", Key: "ctrl+k", Handler: func() {}}); err == nil {
		t.Fatalf("conflict accepted")
	}
	if !r.TriggerShortcut("ctrl+k") || called != 1 {
		t.Fatalf("trigger failed called=%d", called)
	}
	if err := r.RegisterShortcut(Shortcut{ID: "tui.input.submit", Key: "ctrl+x", Handler: func() {}}); err == nil {
		t.Fatalf("reserved shortcut accepted")
	}
}

func TestWidgetsThemeEditorAndExpansion(t *testing.T) {
	r := New()
	_ = r.RegisterWidget(Widget{ID: "w", Placement: Footer, Render: func(width int) []string { return []string{"footer"} }})
	got := r.Widgets(Footer, 80)
	if len(got) != 1 || got[0][0] != "footer" {
		t.Fatalf("widgets=%v", got)
	}
	r.SetTheme(Theme{Name: "dark", Values: map[string]string{"fg": "white"}})
	th := r.Theme()
	th.Values["fg"] = "mutated"
	if r.Theme().Values["fg"] != "white" {
		t.Fatalf("theme aliased")
	}
	r.SetEditorText("hello")
	r.InsertEditorText(5, " world")
	if r.EditorText() != "hello world" {
		t.Fatalf("editor=%q", r.EditorText())
	}
	r.SetToolExpanded("call", true)
	if !r.ToolExpanded("call") {
		t.Fatalf("expanded false")
	}
}

// TestExtensionIntegration_MessageRenderer verifies that a registered
// message renderer is invoked and its output returned.
func TestExtensionIntegration_MessageRenderer(t *testing.T) {
	r := New()
	err := r.RegisterMessageRenderer("assistant", func(payload any) []string {
		return []string{"custom: " + toString(payload)}
	})
	if err != nil {
		t.Fatalf("RegisterMessageRenderer: %v", err)
	}

	// Render should call the registered function
	rows, ok := r.RenderMessage("assistant", "hello")
	if !ok {
		t.Fatal("RenderMessage should return true for registered kind")
	}
	if len(rows) != 1 || rows[0] != "custom: hello" {
		t.Fatalf("RenderMessage = %v, want [custom: hello]", rows)
	}

	// Unknown kind should return false
	_, ok = r.RenderMessage("unknown", nil)
	if ok {
		t.Error("RenderMessage should return false for unregistered kind")
	}
}

// TestExtensionIntegration_ToolRenderer verifies that a registered
// tool renderer is invoked and its output returned.
func TestExtensionIntegration_ToolRenderer(t *testing.T) {
	r := New()
	err := r.RegisterToolRenderer("bash", func(payload any) []string {
		return []string{"→ bash executed", "output: " + toString(payload)}
	})
	if err != nil {
		t.Fatalf("RegisterToolRenderer: %v", err)
	}

	rows, ok := r.RenderTool("bash", "ls -la")
	if !ok {
		t.Fatal("RenderTool should return true for registered name")
	}
	if len(rows) != 2 {
		t.Fatalf("RenderTool rows = %d, want 2", len(rows))
	}
	if rows[0] != "→ bash executed" {
		t.Fatalf("RenderTool[0] = %q, want '→ bash executed'", rows[0])
	}

	// Unknown tool should return false
	_, ok = r.RenderTool("unknown", nil)
	if ok {
		t.Error("RenderTool should return false for unregistered name")
	}
}

// TestExtensionIntegration_ShortcutRegistration verifies that shortcuts
// can be registered and triggered, and that reserved action IDs are
// rejected.
func TestExtensionIntegration_ShortcutRegistration(t *testing.T) {
	// Init the default keybindings manager so CanRegisterExtensionBinding works
	keybindings.SetDefaultManager(keybindings.DefaultManager())

	r := New()
	triggered := false
	err := r.RegisterShortcut(Shortcut{
		ID:          "ext.custom.action",
		Key:         "ctrl+shift+f",
		Description: "Custom find",
		Handler:     func() { triggered = true },
	})
	if err != nil {
		t.Fatalf("RegisterShortcut: %v", err)
	}

	// Trigger the shortcut by key
	if !r.TriggerShortcut("ctrl+shift+f") {
		t.Error("TriggerShortcut should return true for registered key")
	}
	if !triggered {
		t.Error("shortcut handler should have been called")
	}

	// Triggering an unregistered key should return false
	if r.TriggerShortcut("ctrl+shift+x") {
		t.Error("TriggerShortcut should return false for unregistered key")
	}
}

// TestExtensionIntegration_ShortcutConflict verifies that registering
// two shortcuts with the same key fails.
func TestExtensionIntegration_ShortcutConflict(t *testing.T) {
	keybindings.SetDefaultManager(keybindings.DefaultManager())

	r := New()
	err := r.RegisterShortcut(Shortcut{
		ID:          "ext.first",
		Key:         "ctrl+shift+a",
		Description: "First shortcut",
		Handler:     func() {},
	})
	if err != nil {
		t.Fatalf("first RegisterShortcut: %v", err)
	}

	// Second shortcut with same key should fail
	err = r.RegisterShortcut(Shortcut{
		ID:          "ext.second",
		Key:         "ctrl+shift+a",
		Description: "Second shortcut",
		Handler:     func() {},
	})
	if err == nil {
		t.Error("second RegisterShortcut with same key should fail")
	}
}

// TestExtensionIntegration_WidgetRegistration verifies that widgets
// can be registered and rendered by placement.
func TestExtensionIntegration_WidgetRegistration(t *testing.T) {
	r := New()
	err := r.RegisterWidget(Widget{
		ID:        "status-bar",
		Placement: Footer,
		Render:    func(width int) []string { return []string{"status: ok"} },
	})
	if err != nil {
		t.Fatalf("RegisterWidget: %v", err)
	}

	// Get widgets for the Footer placement
	widgets := r.Widgets(Footer, 80)
	if len(widgets) != 1 {
		t.Fatalf("Widgets(Footer) = %d, want 1", len(widgets))
	}
	if len(widgets[0]) != 1 || widgets[0][0] != "status: ok" {
		t.Fatalf("Widgets[0] = %v, want [status: ok]", widgets[0])
	}

	// Header placement should have no widgets
	widgets = r.Widgets(Header, 80)
	if len(widgets) != 0 {
		t.Fatalf("Widgets(Header) = %d, want 0", len(widgets))
	}
}

// TestExtensionIntegration_ThemeManagement verifies that theme values
// can be set and queried.
func TestExtensionIntegration_ThemeManagement(t *testing.T) {
	r := New()

	// Set a theme
	r.SetTheme(Theme{
		Name: "dark",
		Values: map[string]string{
			"primary":   "#00ff00",
			"secondary": "#0088ff",
		},
	})

	// Query the theme
	theme := r.Theme()
	if theme.Name != "dark" {
		t.Errorf("Theme().Name = %q, want 'dark'", theme.Name)
	}
	if theme.Values["primary"] != "#00ff00" {
		t.Errorf("Theme().Values['primary'] = %q, want '#00ff00'", theme.Values["primary"])
	}

	// Verify the returned theme is a copy (not the internal map)
	theme.Values["primary"] = "#modified"
	theme2 := r.Theme()
	if theme2.Values["primary"] == "#modified" {
		t.Error("Theme() should return a copy, not the internal map")
	}
}

// TestExtensionIntegration_EditorTextManipulation verifies that the
// editor text API can set, get, and insert text.
func TestExtensionIntegration_EditorTextManipulation(t *testing.T) {
	r := New()

	// Set and get text
	r.SetEditorText("hello world")
	if got := r.EditorText(); got != "hello world" {
		t.Errorf("EditorText() = %q, want 'hello world'", got)
	}

	// Insert at position
	r.InsertEditorText(5, " beautiful")
	if got := r.EditorText(); got != "hello beautiful world" {
		t.Errorf("EditorText() after insert = %q, want 'hello beautiful world'", got)
	}

	// Insert at position 0
	r.SetEditorText("world")
	r.InsertEditorText(0, "hello ")
	if got := r.EditorText(); got != "hello world" {
		t.Errorf("EditorText() after prepend = %q, want 'hello world'", got)
	}

	// Insert at end (beyond length should clamp)
	r.SetEditorText("hello")
	r.InsertEditorText(100, " world")
	if got := r.EditorText(); got != "hello world" {
		t.Errorf("EditorText() after append = %q, want 'hello world'", got)
	}

	// Insert at negative position should clamp to 0
	r.SetEditorText("hello")
	r.InsertEditorText(-5, "x")
	if got := r.EditorText(); got != "xhello" {
		t.Errorf("EditorText() after negative pos = %q, want 'xhello'", got)
	}
}

// TestExtensionIntegration_ToolExpansionState verifies that tool
// expansion state can be set and queried.
func TestExtensionIntegration_ToolExpansionState(t *testing.T) {
	r := New()

	// Default should be false (unexpanded)
	if r.ToolExpanded("tool-1") {
		t.Error("ToolExpanded should default to false")
	}

	// Set expanded
	r.SetToolExpanded("tool-1", true)
	if !r.ToolExpanded("tool-1") {
		t.Error("ToolExpanded should be true after SetToolExpanded(true)")
	}

	// Set collapsed
	r.SetToolExpanded("tool-1", false)
	if r.ToolExpanded("tool-1") {
		t.Error("ToolExpanded should be false after SetToolExpanded(false)")
	}

	// Different tools should have independent state
	r.SetToolExpanded("tool-2", true)
	if !r.ToolExpanded("tool-2") {
		t.Error("ToolExpanded('tool-2') should be true")
	}
	if r.ToolExpanded("tool-1") {
		t.Error("ToolExpanded('tool-1') should still be false")
	}
}

// TestExtensionIntegration_DuplicateRegistration verifies that
// registering the same renderer/widget/shortcut twice fails.
func TestExtensionIntegration_DuplicateRegistration(t *testing.T) {
	keybindings.SetDefaultManager(keybindings.DefaultManager())

	r := New()

	// Duplicate message renderer
	err := r.RegisterMessageRenderer("assistant", func(any) []string { return nil })
	if err != nil {
		t.Fatalf("first RegisterMessageRenderer: %v", err)
	}
	err = r.RegisterMessageRenderer("assistant", func(any) []string { return nil })
	if err == nil {
		t.Error("duplicate RegisterMessageRenderer should fail")
	}

	// Duplicate tool renderer
	err = r.RegisterToolRenderer("bash", func(any) []string { return nil })
	if err != nil {
		t.Fatalf("first RegisterToolRenderer: %v", err)
	}
	err = r.RegisterToolRenderer("bash", func(any) []string { return nil })
	if err == nil {
		t.Error("duplicate RegisterToolRenderer should fail")
	}

	// Duplicate widget
	err = r.RegisterWidget(Widget{ID: "w1", Placement: Footer, Render: func(int) []string { return nil }})
	if err != nil {
		t.Fatalf("first RegisterWidget: %v", err)
	}
	err = r.RegisterWidget(Widget{ID: "w1", Placement: Footer, Render: func(int) []string { return nil }})
	if err == nil {
		t.Error("duplicate RegisterWidget should fail")
	}

	// Duplicate shortcut
	err = r.RegisterShortcut(Shortcut{ID: "ext.dup", Key: "ctrl+d", Description: "d", Handler: func() {}})
	if err != nil {
		t.Fatalf("first RegisterShortcut: %v", err)
	}
	err = r.RegisterShortcut(Shortcut{ID: "ext.dup", Key: "ctrl+e", Description: "e", Handler: func() {}})
	if err == nil {
		t.Error("duplicate RegisterShortcut (same ID) should fail")
	}
}

// toString is a test helper that converts any payload to a string.
func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestToolRendererResolversWrapInRegistrationOrder(t *testing.T) {
	r := New()
	if err := r.RegisterToolRenderer("known", func(any) []string { return []string{"base"} }); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"first", "second"} {
		r.RegisterToolRendererResolver(func(name string, next func() Renderer) Renderer {
			base := next()
			if name == "unknown" && base == nil {
				base = func(any) []string { return []string{"fallback"} }
			}
			if base == nil {
				return nil
			}
			return func(payload any) []string { return append([]string{label}, base(payload)...) }
		})
	}
	for _, name := range []string{"known", "unknown"} {
		rows, ok := r.RenderTool(name, nil)
		if !ok || len(rows) != 3 || rows[0] != "first" || rows[1] != "second" {
			t.Fatalf("%s chain: %v", name, rows)
		}
	}
	if r.ToolRendererFor("missing") != nil {
		t.Fatal("unknown tool without a matching resolver did not fall back")
	}
}

func TestThemeMetadataMapsAreOwnedSnapshots(t *testing.T) {
	r := New()
	source := Theme{Name: "custom", Appearance: "light", Dim: []string{"muted"}, Values: map[string]string{"text": "default"}, Colors: map[string]string{"text": "#010203"}}
	r.OnThemeChanged(func(theme Theme) {
		if theme.Appearance != "light" {
			t.Errorf("appearance=%q", theme.Appearance)
		}
		theme.Values["text"] = "callback mutation"
		theme.Colors["text"] = "callback mutation"
		theme.Dim[0] = "callback mutation"
	})
	r.SetTheme(source)
	source.Values["text"] = "source mutation"
	source.Colors["text"] = "source mutation"
	source.Dim[0] = "source mutation"
	snapshot := r.Theme()
	if snapshot.Values["text"] != "default" || snapshot.Colors["text"] != "#010203" || snapshot.Dim[0] != "muted" {
		t.Fatalf("theme aliased: %+v", snapshot)
	}
	snapshot.Colors["text"] = "snapshot mutation"
	snapshot.Dim[0] = "snapshot mutation"
	if r.Theme().Colors["text"] != "#010203" || r.Theme().Dim[0] != "muted" {
		t.Fatal("colors snapshot aliased")
	}
	r.OnThemeChanged(nil)
	r.SetTheme(Theme{Name: "plain"})
	if r.Theme().Colors != nil || r.Theme().Appearance != "" {
		t.Fatal("replacement retained old metadata")
	}
}

func TestShortcutForShiftedProducedSymbol(t *testing.T) {
	r := New()
	err := r.RegisterShortcut(Shortcut{ID: "plugin.plus", Key: "ctrl++", Handler: func() {}})
	require.NoError(t, err)
	shortcut, ok := r.ShortcutForKeystroke(ui.Keystroke{Key: event.KeyRune, Rune: '=', ShiftedRune: '+', Mod: event.ModShift | event.ModCtrl})
	require.True(t, ok)
	assert.Equal(t, "plugin.plus", shortcut.ID)
}

func TestInsertEditorTextClampsToGraphemeBoundary(t *testing.T) {
	for _, value := range []string{"界", "e\u0301", "👩\u200d💻"} {
		for pos := 1; pos < len(value); pos++ {
			r := New()
			r.SetEditorText(value)
			var notified string
			var insertedAt int
			r.OnEditorTextChanged(func(s string) { notified = s })
			r.OnInsertEditorTextChanged(func(pos int, _ string) { insertedAt = pos })
			r.InsertEditorText(pos, "X")
			if got := r.EditorText(); got != "X"+value {
				t.Fatalf("insert %d in %q = %q", pos, value, got)
			}
			if notified != r.EditorText() || insertedAt != 0 {
				t.Fatalf("callbacks = %q at %d", notified, insertedAt)
			}
		}
	}
}
