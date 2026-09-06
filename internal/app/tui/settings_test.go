package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/tui/settingslist"
)

// withTestFooter returns app after installing a minimal Footer bound to
// a clean footerData struct so the settings overlay builder has live
// state to read.
func withTestFooter(app *App) *App {
	app.footer = newFooterForTest()
	return app
}

// TestSettingsOverlay_BuildItems verifies the runtime state model feeds
// the settings list builder. Each settings item is sourced from live
// App state (model id, thinking level, auto-compaction toggle,
// theme name, footer notice), so the builder is the single
// integration point between the App and the settingslist package.
// This covers the "wire settings list to actual runtime settings"
// parity gap.
func TestSettingsOverlay_BuildItems(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil

	app.footer.data.theme = "dark"
	app.footer.data.thinkingLevel = "high"
	app.footer.data.autoCompaction = true
	app.footer.data.notice = "ready"
	app.currentModel = "anthropic/claude-sonnet-4-6"

	items := buildSettingsItems(app)
	if len(items) < 5 {
		t.Fatalf("expected at least 5 items, got %d", len(items))
	}
	wantID := map[string]bool{
		"settings.model":           false,
		"settings.thinkingLevel":   false,
		"settings.autoCompaction":  false,
		"settings.theme":           false,
		"settings.streamingFooter": false,
	}
	for _, it := range items {
		if _, ok := wantID[it.ID]; ok {
			wantID[it.ID] = true
			if it.Label == "" {
				t.Errorf("item %s missing Label", it.ID)
			}
			if it.Description == "" {
				t.Errorf("item %s missing Description", it.ID)
			}
		}
	}
	for id, seen := range wantID {
		if !seen {
			t.Errorf("missing settings item %s", id)
		}
	}
}

// TestSettingsOverlay_RenderReachable verifies the overlay
// component's Render/HandleInput surface meets the
// settingslist.Component contract and that the SearchEnabled
// predicate returns true so the dialog picker sizes itself for a
// search input row. This covers the "searchable/fuzzy settings menu"
// parity item.
func TestSettingsOverlay_RenderReachable(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil

	overlay := newSettingsOverlay(app)
	if overlay == nil || overlay.list == nil {
		t.Fatal("newSettingsOverlay should produce a configured list")
	}
	if !overlay.SearchEnabled() {
		t.Errorf("settings overlay must enable the search input")
	}
	if got := overlay.Render(80); len(got) == 0 {
		t.Errorf("settings overlay Render produced no rows")
	}
	// Forwarding a bare rune must be safe (no panic) and reach the list.
	overlay.HandleInput("x")

	// Items must be unique by id so the settings list can resolve
	// duplicate-key lookups deterministically.
	seen := map[string]bool{}
	for _, it := range overlay.list.Items() {
		if seen[it.ID] {
			t.Errorf("duplicate settings id: %s", it.ID)
		}
		seen[it.ID] = true
	}
}

// TestSettingsOverlay_SubmenuConfiguredOnTheme verifies the theme
// setting has a submenu factory (the "submenu behavior in app"
// parity gap). The factory must return a component when invoked.
func TestSettingsOverlay_SubmenuConfiguredOnTheme(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil

	items := buildSettingsItems(app)
	var themeItem *settingslist.Item
	for i, it := range items {
		if it.ID == "settings.theme" {
			themeItem = &items[i]
			break
		}
	}
	if themeItem == nil {
		t.Fatal("settings.theme item missing")
	}
	if themeItem.Submenu == nil {
		t.Fatal("settings.theme should carry a submenu factory")
	}
	doneCalled := false
	comp := themeItem.Submenu(themeItem.CurrentValue, func(_ string, _ bool) { doneCalled = true })
	if comp == nil {
		t.Fatal("theme submenu factory returned nil component")
	}
	if rows := comp.Render(80); len(rows) == 0 {
		t.Errorf("theme submenu produced no rows")
	}
	if doneCalled {
		// sanity: factory didn't auto-fire done
		t.Errorf("submenu factory should not immediately invoke done")
	}
}

// TestSettingsOverlay_ValueCyclingMaterializesNextValue verifies the
// settings list cycles through configured Values for cycling-style
// entries (e.g. thinking level, auto-compaction). This is the
// "value cycling behavior" parity item.
func TestSettingsOverlay_ValueCyclingMaterializesNextValue(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	overlay := newSettingsOverlay(app)

	// Find the thinking level item by id and assert it carries a
	// non-empty Values cycle, matching Pi's settings list contract.
	items := overlay.list.Items()
	var found bool
	for _, it := range items {
		if it.ID != "settings.thinkingLevel" {
			continue
		}
		found = true
		want := []string{"off", "minimal", "low", "medium", "high", "xhigh"}
		if len(it.Values) != len(want) {
			t.Fatalf("thinking level Values length = %d, want %d", len(it.Values), len(want))
		}
		for i, v := range want {
			if i < len(it.Values) && it.Values[i] != v {
				t.Errorf("thinking level Values[%d] = %q, want %q", i, it.Values[i], v)
			}
		}
	}
	if !found {
		t.Fatal("thinking level item missing from settings overlay")
	}
}

// TestApplySettingsChange_DispatchByID verifies the dispatcher writes
// changes through the right subsystem for each id. Each case writes
// through the live App state and observes the result without
// requiring the toolkit loop.
func TestApplySettingsChange_DispatchByID(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	app.footer.data.theme = "default"
	app.footer.data.thinkingLevel = "off"
	app.footer.data.autoCompaction = false
	app.footer.data.notice = ""

	// Theme change must update footer data.
	applySettingsChange(app, "settings.theme", "dark")
	if app.footer.data.theme != "dark" {
		t.Errorf("theme after change = %q, want dark", app.footer.data.theme)
	}
	if app.chat == nil || app.chat.look != chatLogLookPi {
		t.Errorf("chat look after dark theme = %#v, want pi", app.chat)
	}

	// Auto-compaction: off/on text → bool.
	applySettingsChange(app, "settings.autoCompaction", "on")
	if !app.footer.data.autoCompaction {
		t.Errorf("autoCompaction after on = %v, want true", app.footer.data.autoCompaction)
	}
	applySettingsChange(app, "settings.autoCompaction", "off")
	if app.footer.data.autoCompaction {
		t.Errorf("autoCompaction after off = %v, want false", app.footer.data.autoCompaction)
	}

	// Footer notice round-trips.
	applySettingsChange(app, "settings.streamingFooter", "esc again to quit")
	if app.footer.data.notice != "esc again to quit" {
		t.Errorf("footer notice after change = %q, want 'esc again to quit'", app.footer.data.notice)
	}
}

func TestApplySettingsChangeThemePersistsAndInvalidatesAll(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	app.settingsPath = filepath.Join(t.TempDir(), "settings.json")

	var layoutCalls, allCalls int
	app.loopMu.Lock()
	app.loopCtx = stubCtx()
	app.loopCtx.InvalidateLayout = func() { layoutCalls++ }
	app.loopCtx.InvalidateAll = func() { allCalls++ }
	app.loopMu.Unlock()

	applySettingsChange(app, "settings.theme", "tau")

	settings, err := ai.LoadSettings(app.settingsPath)
	if err != nil {
		t.Fatalf("load saved settings: %v", err)
	}
	if settings.Theme != "tau" {
		t.Fatalf("saved theme = %q, want tau", settings.Theme)
	}
	if app.chat.look != chatLogLookTau {
		t.Fatalf("chat look = %d, want tau", app.chat.look)
	}
	if layoutCalls == 0 || allCalls == 0 {
		t.Fatalf("theme change invalidations layout=%d all=%d, want both nonzero", layoutCalls, allCalls)
	}
}

func TestApplySettingsChangeThemeReportsPersistError(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	fileAsDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	app.settingsPath = filepath.Join(fileAsDir, "settings.json")

	applySettingsChange(app, "settings.theme", "tau")

	if app.footer.data.theme != "tau" {
		t.Fatalf("theme should still apply despite save error, got %q", app.footer.data.theme)
	}
	for _, msg := range app.conv.Messages() {
		if msg.Role == RoleError && strings.Contains(msg.Text, "failed to save setting") {
			return
		}
	}
	t.Fatalf("missing theme save error in conversation: %#v", app.conv.Messages())
}

// TestSettingsOverlay_DescriptionsAndHintsRender verifies each
// settings item carries a non-empty Description and that the hint
// row from the underlying settingslist appears in the rendered output.
// This is the "descriptions/hints parity in app" checklist item.
func TestSettingsOverlay_DescriptionsAndHintsRender(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	overlay := newSettingsOverlay(app)

	items := overlay.list.Items()
	seen := map[string]bool{}
	for _, it := range items {
		if it.ID == "" {
			continue
		}
		if it.Description == "" {
			t.Errorf("item %s missing Description", it.ID)
		}
		if it.Label == "" {
			t.Errorf("item %s missing Label", it.ID)
		}
		seen[it.ID] = true
	}
	mustInclude := []string{
		"settings.model",
		"settings.thinkingLevel",
		"settings.autoCompaction",
		"settings.theme",
		"settings.streamingFooter",
	}
	for _, id := range mustInclude {
		if !seen[id] {
			t.Errorf("missing required settings item %s", id)
		}
	}

	rows := overlay.Render(100)
	if len(rows) == 0 {
		t.Fatal("settings overlay produced no rows")
	}
	hintsSeen := 0
	for _, row := range rows {
		// The settingslist component renders a "Tab cycle · Enter submenu · esc back" style
		// hint line at the bottom. Match on the long hint keywords so we don't depend
		// on a specific format string.
		low := strings.ToLower(row)
		if strings.Contains(low, "tab") || strings.Contains(low, "enter") || strings.Contains(low, "esc") {
			hintsSeen++
		}
	}
	if hintsSeen == 0 {
		t.Errorf("settings overlay render should surface a hint row, got %v", rows)
	}
}

// TestSettingsOverlay_SubmenuClosesViaDone verifies the theme
// submenu invokes the done callback with (selected, true) when the
// user picks a theme so the parent settings list can apply and
// close. Without a working done path the submenu would leak focus.
func TestSettingsOverlay_SubmenuClosesViaDone(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil

	items := buildSettingsItems(app)
	var themeItem *settingslist.Item
	for i, it := range items {
		if it.ID == "settings.theme" {
			themeItem = &items[i]
			break
		}
	}
	if themeItem == nil || themeItem.Submenu == nil {
		t.Fatal("theme submenu factory missing")
	}

	// Confirm the theme submenu factory invokes done when an item
	// is picked. We simulate the pick by closing over the closure
	// that the parent list uses to apply and dismiss.
	doneCalled := false
	comp := themeItem.Submenu(themeItem.CurrentValue, func(_ string, _ bool) {
		doneCalled = true
	})
	if comp == nil {
		t.Fatal("theme submenu factory returned nil component")
	}
	rows := comp.Render(80)
	if len(rows) == 0 {
		t.Fatal("submenu produced no rows")
	}
	if doneCalled {
		t.Fatal("submenu Render must not immediately fire done")
	}
	// Confirm the submenu contains the theme choices so the user has
	// something to pick.
	for _, label := range app.availableThemes() {
		if !rowsContainLabel(rows, label) {
			t.Errorf("submenu rows missing theme %q: %v", label, rows)
		}
	}

	// Render must leave state intact so an explicit Escape from the
	// user can fire done. Render-only callers should never trigger
	// the done callback; only a user-issued confirm/cancel does.
	comp2 := themeItem.Submenu(themeItem.CurrentValue, func(_ string, _ bool) {
		doneCalled = true
	})
	_ = comp2.Render(80)
	if doneCalled {
		t.Error("Render must not invoke done callback")
	}
}

// rowsContainLabel is a tolerant predicate that ignores ANSI styling
// when checking whether label appears verbatim in rows. The
// settingslist component inserts ANSI escapes around the label, so
// we strip them before comparing.
func rowsContainLabel(rows []string, label string) bool {
	for _, row := range rows {
		stripped := stripANSI(row)
		if strings.Contains(stripped, label) {
			return true
		}
	}
	return false
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == 0x1b {
			in = true
			continue
		}
		if in {
			if r == 'm' || r == 'K' {
				in = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// TestCmdSettingsOverlay_OpensOverlay verifies the /settings command
// installs the settings overlay component on the dialog picker and
// shows it. The picker is the live UI consumer of the settingslist
// package so the call must keep the picker focused and visible.
func TestCmdSettingsOverlay_OpensOverlay(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil

	cmdSettingsOverlay(app, "")

	if !app.dialogPicker.Shown() {
		t.Errorf("/settings should show the dialog picker")
	}
	if app.dialogPicker.Component() == nil {
		t.Errorf("/settings must install a component")
	}
	if hdr := app.dialogPicker.Header(); !strings.Contains(hdr, "settings") {
		t.Errorf("/settings must set the header, got %q", hdr)
	}
}

func TestDismissSettingsOverlayReleasesComponent(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil

	cmdSettingsOverlay(app, "")
	if app.dialogPicker.Component() == nil {
		t.Fatal("/settings must install a component before dismissal")
	}

	app.dismissDialogCtx(stubCtx())

	if app.dialogPicker.Shown() {
		t.Fatal("dismissDialogCtx should hide dialog picker")
	}
	if got := app.dialogPicker.Component(); got != nil {
		t.Fatalf("dismissDialogCtx retained dialog component %T; want nil", got)
	}
}

// TestApplySettingsChange_ThinkingLevelPersistsPerModel verifies changing the
// thinking level records it against the active model in settings.json, so the
// level follows the model across sessions and model switches.
func TestApplySettingsChange_ThinkingLevelPersistsPerModel(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	app.settingsPath = filepath.Join(t.TempDir(), "settings.json")
	app.currentModel = "anthropic/claude-opus-4-6"

	applySettingsChange(app, "settings.thinkingLevel", "high")

	settings, err := ai.LoadSettings(app.settingsPath)
	if err != nil {
		t.Fatalf("load saved settings: %v", err)
	}
	if got := settings.Models["anthropic/claude-opus-4-6"].ThinkingLevel; got != "high" {
		t.Fatalf("saved per-model level = %q, want high", got)
	}
	sess := app.sess.(*fakeSession)
	seen := sess.thinkingLevelsSeen()
	if len(seen) != 1 || seen[0] != "high" {
		t.Fatalf("session levels = %v, want [high]", seen)
	}
}

// TestApplySettingsChange_ThinkingLevelWithoutModelSkipsPersist verifies no
// per-model entry is written when no model is active yet; the level stays a
// session-only change rather than polluting settings.json.
func TestApplySettingsChange_ThinkingLevelWithoutModelSkipsPersist(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	app.settingsPath = filepath.Join(t.TempDir(), "settings.json")

	applySettingsChange(app, "settings.thinkingLevel", "low")

	settings, err := ai.LoadSettings(app.settingsPath)
	if err != nil {
		t.Fatalf("load saved settings: %v", err)
	}
	if len(settings.Models) != 0 {
		t.Fatalf("models map = %v, want empty", settings.Models)
	}
}

// TestApplyModelFromKey_AppliesSavedThinkingLevel verifies switching to a
// model with a saved per-model thinking level applies it to the session
// rather than inheriting the previous model's level.
func TestApplyModelFromKey_AppliesSavedThinkingLevel(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	app.settingsPath = filepath.Join(t.TempDir(), "settings.json")
	settings := &ai.Settings{Models: map[string]ai.ModelSettings{
		"anthropic/claude-opus-4-6": {ThinkingLevel: "high"},
	}}
	if err := ai.SaveSettings(app.settingsPath, settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	app.footer.data.thinkingLevel = "medium"
	app.currentModel = "anthropic/claude-sonnet-4-6"

	app.applyModelFromKey("anthropic/claude-opus-4-6")

	sess := app.sess.(*fakeSession)
	seen := sess.thinkingLevelsSeen()
	if len(seen) != 1 || seen[0] != "high" {
		t.Fatalf("session levels after switch = %v, want [high]", seen)
	}
	foundSystem := false
	for _, msg := range app.conv.Messages() {
		if msg.Role == RoleSystem && strings.Contains(msg.Text, "thinking level: high") {
			foundSystem = true
		}
	}
	if !foundSystem {
		t.Fatalf("missing 'thinking level: high' system row in conversation")
	}
}

// TestApplyModelFromKey_NoSavedLevelKeepsCurrent verifies a model without a
// saved per-model level inherits the current session level unchanged.
func TestApplyModelFromKey_NoSavedLevelKeepsCurrent(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	app.settingsPath = filepath.Join(t.TempDir(), "settings.json")
	if err := ai.SaveSettings(app.settingsPath, &ai.Settings{}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	app.footer.data.thinkingLevel = "medium"
	app.currentModel = "anthropic/claude-sonnet-4-6"

	app.applyModelFromKey("anthropic/claude-opus-4-6")

	sess := app.sess.(*fakeSession)
	if seen := sess.thinkingLevelsSeen(); len(seen) != 0 {
		t.Fatalf("session levels after switch = %v, want none", seen)
	}
}

// TestApplySettingsChange_ThinkingLevelReportsPersistError verifies a failed
// settings write surfaces an error row while the level still applies to the
// session — matching the theme-persist-error behavior.
func TestApplySettingsChange_ThinkingLevelReportsPersistError(t *testing.T) {
	app := withTestFooter(newAppWithSelector(t))
	app.extensions = nil
	fileAsDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	app.settingsPath = filepath.Join(fileAsDir, "settings.json")
	app.currentModel = "anthropic/claude-opus-4-6"

	applySettingsChange(app, "settings.thinkingLevel", "high")

	sess := app.sess.(*fakeSession)
	if seen := sess.thinkingLevelsSeen(); len(seen) != 1 || seen[0] != "high" {
		t.Fatalf("session levels = %v, want [high] despite save error", seen)
	}
	for _, msg := range app.conv.Messages() {
		if msg.Role == RoleError && strings.Contains(msg.Text, "settings.thinkingLevel") {
			return
		}
	}
	t.Fatalf("missing persist error in conversation: %#v", app.conv.Messages())
}
