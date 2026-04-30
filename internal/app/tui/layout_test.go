package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/ai"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/settingslist"
)

func TestParseLayoutMode(t *testing.T) {
	cases := map[string]struct {
		want LayoutMode
		ok   bool
	}{
		"":          {DefaultLayoutMode, true},
		"  ":        {DefaultLayoutMode, true},
		"default":   {DefaultLayoutMode, true},
		"fixed":     {LayoutFixed, true},
		"FIXED":     {LayoutFixed, true},
		"shell":     {LayoutFixed, true},
		"document":  {LayoutDocument, true},
		" Document": {LayoutDocument, true},
		"doc":       {LayoutDocument, true},
		"flow":      {LayoutDocument, true},
		"sideways":  {DefaultLayoutMode, false},
	}
	for raw, want := range cases {
		got, ok := ParseLayoutMode(raw)
		if got != want.want || ok != want.ok {
			t.Errorf("ParseLayoutMode(%q) = (%q, %v), want (%q, %v)", raw, got, ok, want.want, want.ok)
		}
	}
}

// Both layouts are built over the same region views, so a switch must be a
// swap — not a rebuild that would drop editor text or transcript state.
func TestSetLayoutModeSwapsWithoutRebuildingRegions(t *testing.T) {
	a := newLayoutTestApp(t)

	if got := a.LayoutMode(); got != LayoutFixed {
		t.Fatalf("initial layout = %q, want %q", got, LayoutFixed)
	}
	fixedChat := a.root.AppLayout.Chat
	docChat := a.root.document.Chat

	a.SetLayoutMode(LayoutDocument)
	if got := a.LayoutMode(); got != LayoutDocument {
		t.Fatalf("layout = %q after switching, want %q", got, LayoutDocument)
	}
	if a.root.AppLayout.Chat != fixedChat || a.root.document.Chat != docChat {
		t.Error("switching rebuilt a region view; it should only swap which layout is active")
	}

	a.SetLayoutMode(LayoutFixed)
	if got := a.LayoutMode(); got != LayoutFixed {
		t.Fatalf("layout = %q after switching back, want %q", got, LayoutFixed)
	}
}

func TestToggleLayoutModeFlipsBothWays(t *testing.T) {
	a := newLayoutTestApp(t)

	if got := a.ToggleLayoutMode(); got != LayoutDocument {
		t.Fatalf("first toggle = %q, want %q", got, LayoutDocument)
	}
	if got := a.ToggleLayoutMode(); got != LayoutFixed {
		t.Fatalf("second toggle = %q, want %q", got, LayoutFixed)
	}
}

// Focus traversal must follow the active model, or Tab and the editor stop
// working after a switch.
func TestRootChildrenFollowTheActiveLayout(t *testing.T) {
	a := newLayoutTestApp(t)

	a.SetLayoutMode(LayoutDocument)
	docChildren := a.root.Children()
	if len(docChildren) == 0 {
		t.Fatal("document layout exposed no children for focus traversal")
	}
	if !containsView(docChildren, a.root.document.Chat) {
		t.Error("document layout children do not include its chat region")
	}

	a.SetLayoutMode(LayoutFixed)
	if !containsView(a.root.Children(), a.root.AppLayout.Chat) {
		t.Error("fixed layout children do not include its chat region")
	}
}

// The document model must place the editor on screen no matter how tall the
// transcript is; that is the property that makes it usable.
func TestDocumentModeKeepsTheEditorOnScreen(t *testing.T) {
	a := newLayoutTestApp(t)
	a.SetLayoutMode(LayoutDocument)

	rect := tauui.Rect{X: 0, Y: 0, W: 80, H: 24}
	// Lay out once first so the chat log's render worker knows its width;
	// waiting on a log that has never been sized would wait forever.
	a.root.Layout(rect)

	for _, msgs := range []int{0, 5, 200} {
		for range msgs {
			a.conv.AddUserMessage(strings.Repeat("filler ", 12))
		}
		if msgs > 0 {
			waitForChatLog(t, a.chat)
		}
		a.root.Layout(rect)

		editorRect := a.root.document.Editor.Rect()
		if editorRect.H > 0 && (editorRect.Y < 0 || editorRect.Y+editorRect.H > rect.H) {
			t.Fatalf("%d messages: editor at Y=%d h=%d is off screen",
				msgs, editorRect.Y, editorRect.H)
		}
	}
}

// The setting has to reach the layout, or configuring it does nothing — the
// same class of bug as the display settings that were parsed and dropped.
func TestLayoutModeSettingIsApplied(t *testing.T) {
	a := newLayoutTestApp(t)

	a.applyDisplaySettings(&ai.Settings{LayoutMode: "document"})
	if got := a.LayoutMode(); got != LayoutDocument {
		t.Fatalf("layout = %q after settings applied, want %q", got, LayoutDocument)
	}

	a.applyDisplaySettings(&ai.Settings{LayoutMode: "fixed"})
	if got := a.LayoutMode(); got != LayoutFixed {
		t.Fatalf("layout = %q after settings applied, want %q", got, LayoutFixed)
	}
}

// An unset layoutMode must not silently move an existing user off the default.
func TestUnsetLayoutModeKeepsTheDefault(t *testing.T) {
	a := newLayoutTestApp(t)
	a.applyDisplaySettings(&ai.Settings{})
	if got := a.LayoutMode(); got != DefaultLayoutMode {
		t.Fatalf("layout = %q with no setting, want the default %q", got, DefaultLayoutMode)
	}
}

// A typo should be reported rather than silently ignored.
func TestUnknownLayoutModeIsReported(t *testing.T) {
	a := newLayoutTestApp(t)
	a.conv.Clear()

	a.applyDisplaySettings(&ai.Settings{LayoutMode: "sideways"})

	if got := a.LayoutMode(); got != DefaultLayoutMode {
		t.Errorf("layout = %q for an unknown mode, want the default", got)
	}
	var found bool
	for _, m := range a.conv.Messages() {
		if strings.Contains(m.Text, "layoutMode") && strings.Contains(m.Text, "sideways") {
			found = true
		}
	}
	if !found {
		t.Errorf("unknown layoutMode was not reported: %#v", a.conv.Messages())
	}
}

func TestCmdLayoutTogglesAndReports(t *testing.T) {
	a := newLayoutTestApp(t)
	a.conv.Clear()

	cmdLayout(a, "")
	if got := a.LayoutMode(); got != LayoutDocument {
		t.Fatalf("/layout did not toggle: mode = %q", got)
	}
	if last := lastMessageText(a); !strings.Contains(last, "document") {
		t.Errorf("/layout did not report the new mode: %q", last)
	}

	cmdLayout(a, "fixed")
	if got := a.LayoutMode(); got != LayoutFixed {
		t.Fatalf("/layout fixed did not switch: mode = %q", got)
	}

	cmdLayout(a, "sideways")
	if last := lastMessageText(a); !strings.Contains(last, "unknown mode") {
		t.Errorf("/layout with a bad argument did not complain: %q", last)
	}
	if got := a.LayoutMode(); got != LayoutFixed {
		t.Errorf("a bad /layout argument changed the mode to %q", got)
	}
}

func lastMessageText(a *App) string {
	msgs := a.conv.Messages()
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1].Text
}

func containsView(views []tauui.View, want tauui.View) bool {
	for _, v := range views {
		if v == want {
			return true
		}
	}
	return false
}

// newLayoutTestApp builds an App with both layout models wired over the same
// region views, mirroring what New does, without standing up the toolkit loop.
func newLayoutTestApp(t *testing.T) *App {
	t.Helper()
	a := newTestApp(t)
	a.editor = tauui.NewPromptEditor()
	// newTestApp's chat log has no render notifier, so prepared renders are
	// never installed and waitForChatLog would spin until its deadline.
	_ = a.chat.Close()
	a.chat = newTestChatLog(t, a.conv)

	header := NewHeader("test")
	pending := NewPendingView()
	status := tauui.NewStatusEntriesView(5)
	widgetsAbove := NewWidgetContainer(widgetPlacementAbove)
	widgetsBelow := NewWidgetContainer(widgetPlacementBelow)
	footer := NewFooter(&footerData{})
	editorRegion := NewEditorDialogRegion(a.editor, tauui.NewDialogPicker(), nil)

	regions := tauui.AppLayoutOpts{
		Header:       header,
		Pending:      pending,
		Status:       status,
		WidgetsAbove: widgetsAbove,
		Editor:       editorRegion,
		WidgetsBelow: widgetsBelow,
		Footer:       footer,
	}

	fixedRegions := regions
	fixedRegions.Chat = a.chat
	documentRegions := regions
	documentRegions.Chat = a.chat

	root := &Root{
		AppLayout: tauui.NewAppLayout(fixedRegions),
		app:       a,
		document:  tauui.NewDocumentLayout(documentRegions),
	}
	root.layout = root.AppLayout
	a.root = root
	return a
}

// A layout chosen at runtime must survive a restart. Comparing the two models
// means flipping back and forth over days; a choice that silently reverted on
// the next launch would be worse than no toggle at all.
func TestLayoutSwitchPersistsToSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"tau"}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	a := newLayoutTestApp(t)
	a.settingsPath = path
	a.conv.Clear()

	cmdLayout(a, "document")

	settings, err := ai.LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if settings.LayoutMode != string(LayoutDocument) {
		t.Errorf("settings.layoutMode = %q after /layout document, want %q",
			settings.LayoutMode, LayoutDocument)
	}
	// Unrelated settings must survive the write.
	if settings.Theme != "tau" {
		t.Errorf("persisting the layout clobbered the theme: %q", settings.Theme)
	}

	cmdLayout(a, "fixed")
	settings, err = ai.LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if settings.LayoutMode != string(LayoutFixed) {
		t.Errorf("settings.layoutMode = %q after switching back, want %q",
			settings.LayoutMode, LayoutFixed)
	}
}

// Asking which model is active must not write anything.
func TestLayoutStatusDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"tau"}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	a := newLayoutTestApp(t)
	a.settingsPath = path
	cmdLayout(a, "status")

	settings, err := ai.LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if settings.LayoutMode != "" {
		t.Errorf("/layout status wrote layoutMode = %q; it should only report", settings.LayoutMode)
	}
}

// The settings overlay is the discoverable path — a switch buried behind a
// slash command only helps someone who already knows it exists.
func TestSettingsOverlayExposesTheLayout(t *testing.T) {
	a := newLayoutTestApp(t)
	items := buildSettingsItems(a)

	var layout *settingslist.Item
	for i := range items {
		if items[i].ID == "settings.layout" {
			layout = &items[i]
		}
	}
	if layout == nil {
		t.Fatalf("no layout row in /settings; rows: %v", settingsItemIDs(items))
	}
	if layout.CurrentValue != string(LayoutFixed) {
		t.Errorf("layout row shows %q, want the active mode %q", layout.CurrentValue, LayoutFixed)
	}
	if len(layout.Values) != 2 {
		t.Errorf("layout row offers %v, want both models", layout.Values)
	}
}

// Choosing a layout in /settings must actually switch it, not just record it.
func TestSettingsOverlayLayoutChangeApplies(t *testing.T) {
	a := newLayoutTestApp(t)

	applySettingsChange(a, "settings.layout", string(LayoutDocument))
	if got := a.LayoutMode(); got != LayoutDocument {
		t.Fatalf("layout = %q after the settings change, want %q", got, LayoutDocument)
	}

	applySettingsChange(a, "settings.layout", string(LayoutFixed))
	if got := a.LayoutMode(); got != LayoutFixed {
		t.Fatalf("layout = %q after switching back, want %q", got, LayoutFixed)
	}
}

func settingsItemIDs(items []settingslist.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// Root claims mouse events so it can offer the wheel to the document. It must
// then deliver everything else exactly where rovel's own hit test would have —
// otherwise clicking the editor or the chat stops working.
func TestRootDeliversMouseToTheDeepestRegion(t *testing.T) {
	a := newLayoutTestApp(t)
	rect := tauui.Rect{X: 0, Y: 0, W: 80, H: 24}
	a.root.Layout(rect)

	for _, region := range a.root.Children() {
		r := region.Rect()
		if r.W <= 0 || r.H <= 0 {
			continue
		}
		got := deepestViewAt(a.root.Children(), r.X, r.Y)
		if got == nil {
			t.Errorf("no view found at the top-left of region %T", region)
			continue
		}
		// The result must be the region itself or something inside it.
		if got != region && !viewWithin(region, got) {
			t.Errorf("point in %T dispatched to unrelated %T", region, got)
		}
	}

	if got := deepestViewAt(a.root.Children(), -5, -5); got != nil {
		t.Errorf("a point outside every region resolved to %T, want nil", got)
	}
}

// viewWithin reports whether target is v or one of its descendants.
func viewWithin(v, target tauui.View) bool {
	if v == target {
		return true
	}
	parent, ok := v.(interface{ Children() []tauui.View })
	if !ok {
		return false
	}
	for _, child := range parent.Children() {
		if viewWithin(child, target) {
			return true
		}
	}
	return false
}

// The wheel scrolls the document, and only in document mode — in the fixed
// shell the chat's own ScrollView owns it.
func TestWheelScrollsOnlyInDocumentMode(t *testing.T) {
	a := newLayoutTestApp(t)
	rect := tauui.Rect{X: 0, Y: 0, W: 80, H: 24}

	for range 200 {
		a.conv.AddUserMessage("filler line for scroll depth")
	}
	a.root.Layout(rect)
	waitForChatLog(t, a.chat)
	a.root.Layout(rect)

	wheelUp := tauui.MouseEvent{Button: tauui.MouseButtonWheelUp, Action: tauui.MousePress, X: 5, Y: 5}

	// Fixed shell: Root must not scroll the document.
	a.root.Handle(wheelUp, nil)
	if a.root.document.ScrollBack() != 0 {
		t.Errorf("wheel moved the document while the fixed shell was active: %d",
			a.root.document.ScrollBack())
	}

	a.SetLayoutMode(LayoutDocument)
	a.root.Layout(rect)
	if !a.root.Handle(wheelUp, nil) {
		t.Fatal("wheel was not handled in document mode")
	}
	if a.root.document.ScrollBack() == 0 {
		t.Error("wheel did not scroll the document")
	}
}
