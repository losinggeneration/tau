package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/losinggeneration/rovel"
	"github.com/losinggeneration/rovel/backend/headless"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/autocomplete"
	"github.com/losinggeneration/tau/tui/extensions"
	"github.com/losinggeneration/tau/tui/keybindings"
	"github.com/losinggeneration/tau/tui/keymap"
	"github.com/losinggeneration/tau/tui/widgets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// convText joins the transcript so a test can assert an error reached the user.
func convText(a *App) string {
	var b strings.Builder
	for _, m := range a.conv.Messages() {
		b.WriteString(m.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// thinkingApp returns an app whose footer already reports a reasoning-capable
// model at the given level, which is the state the thinking actions read.
func thinkingApp(t *testing.T, level string) (*App, *fakeSession) {
	t.Helper()
	a := newTestApp(t)
	sess, ok := a.sess.(*fakeSession)
	if !ok {
		t.Fatalf("newTestApp session is %T, want *fakeSession", a.sess)
	}
	if a.footer == nil {
		a.footer = newFooterForTest()
	}
	a.footer.data.supportsReasoning = true
	a.footer.data.thinkingLevel = level
	return a, sess
}

// §12 app.thinking.cycle: one rung up the ladder each press, wrapping past the
// top back to off.
func TestThinkingCycleWalksTheLadderAndWraps(t *testing.T) {
	a, sess := thinkingApp(t, "off")

	for _, want := range append(ai.ThinkingLevels[1:], "off") {
		if !a.cycleThinkingLevel() {
			t.Fatalf("cycle from %q reported no change", a.currentThinkingLevel())
		}
		if got := a.currentThinkingLevel(); got != want {
			t.Fatalf("cycled to %q, want %q", got, want)
		}
	}

	// Every rung must have reached the session, not just the footer.
	want := append(append([]string(nil), ai.ThinkingLevels[1:]...), "off")
	got := sess.thinkingLevelsSeen()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("session saw %v, want %v", got, want)
	}
}

// The ladder order is ai.ThinkingLevels, so the cycle and the /settings row
// cannot drift apart.
func TestThinkingCycleUsesTheSharedLadderOrder(t *testing.T) {
	a, _ := thinkingApp(t, "low")
	a.cycleThinkingLevel()

	i := 0
	for n, level := range ai.ThinkingLevels {
		if level == "low" {
			i = n
		}
	}
	if got, want := a.currentThinkingLevel(), ai.ThinkingLevels[i+1]; got != want {
		t.Errorf("cycle from low reached %q, want the next shared-ladder rung %q", got, want)
	}
}

// §12 app.thinking.toggle: off and back on, returning to the level the user was
// working at rather than a fixed default.
func TestThinkingToggleRestoresTheLastNonOffLevel(t *testing.T) {
	a, _ := thinkingApp(t, "high")

	a.toggleThinkingLevel()
	if got := a.currentThinkingLevel(); got != "off" {
		t.Fatalf("toggle from high gave %q, want off", got)
	}

	a.toggleThinkingLevel()
	if got := a.currentThinkingLevel(); got != "high" {
		t.Errorf("toggle back gave %q, want the remembered high", got)
	}
}

// With nothing remembered, toggling on picks the documented default rather than
// silently staying off.
func TestThinkingToggleFromOffUsesTheDefaultLevel(t *testing.T) {
	a, _ := thinkingApp(t, "off")

	a.toggleThinkingLevel()
	if got := a.currentThinkingLevel(); got != ai.DefaultThinkingLevel {
		t.Errorf("toggle from a cold off gave %q, want %q", got, ai.DefaultThinkingLevel)
	}
}

// Cycling all the way round to off must still leave the toggle able to come
// back to the level the user was working at. This is the case the toggle's own
// bookkeeping cannot cover — by the time it runs, the level is already off —
// which is why the memory lives in the shared apply path.
func TestThinkingToggleRestoresALevelReachedByCycling(t *testing.T) {
	a, _ := thinkingApp(t, "high")

	a.cycleThinkingLevel() // high -> xhigh
	a.cycleThinkingLevel() // xhigh -> max
	a.cycleThinkingLevel() // max -> off
	if got := a.currentThinkingLevel(); got != "off" {
		t.Fatalf("cycling past the top gave %q, want off", got)
	}

	a.toggleThinkingLevel()
	if got := a.currentThinkingLevel(); got != "max" {
		t.Errorf("toggle gave %q, want the cycled-to max rather than the default", got)
	}
}

// End-to-end: a level chosen in /settings survives a toggle round-trip. (The
// memory here is redundant with the toggle's own bookkeeping; the case that
// needs the shared apply path is the cycle-to-off wrap above.)
func TestThinkingToggleRestoresALevelChosenInSettings(t *testing.T) {
	a, _ := thinkingApp(t, "off")

	applySettingsChange(a, "settings.thinkingLevel", "minimal")
	a.toggleThinkingLevel() // -> off
	a.toggleThinkingLevel() // -> back

	if got := a.currentThinkingLevel(); got != "minimal" {
		t.Errorf("toggle gave %q, want the minimal chosen in /settings", got)
	}
}

// A model with no reasoning support must not have its thinking level changed:
// the footer would then advertise an effort the model never applies.
func TestThinkingActionsNoOpWithoutReasoningSupport(t *testing.T) {
	a, sess := thinkingApp(t, "off")
	a.footer.data.supportsReasoning = false

	if a.cycleThinkingLevel() {
		t.Error("cycle acted on a model without reasoning support")
	}
	if a.toggleThinkingLevel() {
		t.Error("toggle acted on a model without reasoning support")
	}
	if got := sess.thinkingLevelsSeen(); len(got) != 0 {
		t.Errorf("session was told %v despite no reasoning support", got)
	}
}

// A session that rejects the level must surface the error and leave the footer
// alone rather than reporting a level that was never applied.
func TestThinkingCycleReportsSessionRejection(t *testing.T) {
	a, sess := thinkingApp(t, "off")
	sess.setThinkingErr = errors.New("model does not support xhigh")

	a.cycleThinkingLevel()

	if got := a.currentThinkingLevel(); got != "off" {
		t.Errorf("footer moved to %q after the session refused", got)
	}
	if joined := convText(a); !strings.Contains(joined, "does not support xhigh") {
		t.Errorf("session rejection was not surfaced: %q", joined)
	}
}

// The registry must route the shipped defaults to the new actions; the whole
// point of registering an id is that its key reaches a handler.
func TestNewAppActionsResolveFromTheirDefaultKeys(t *testing.T) {
	km := appKeymap{app: newAppWithSelector(t)}
	cases := []struct {
		name string
		ks   tauui.Keystroke
		want tauui.Action
	}{
		{"alt+m", tauui.Keystroke{Key: tauui.KeyRune, Rune: 'm', Mod: tauui.ModAlt}, AppActionModelSelect},
		{"ctrl+t", tauui.Keystroke{Key: tauui.KeyRune, Rune: 't', Mod: tauui.ModCtrl}, AppActionThinkingCycle},
		{"alt+t", tauui.Keystroke{Key: tauui.KeyRune, Rune: 't', Mod: tauui.ModAlt}, AppActionThinkingToggle},
		{"ctrl+v", tauui.Keystroke{Key: tauui.KeyRune, Rune: 'v', Mod: tauui.ModCtrl}, AppActionPaste},
	}
	for _, tc := range cases {
		got, ok := km.Resolve(tauui.KeyCtxTextInput, tc.ks)
		if !ok {
			t.Errorf("%s resolved to nothing", tc.name)
			continue
		}
		if got != tc.want {
			t.Errorf("%s resolved to %v, want %v", tc.name, got, tc.want)
		}
	}
}

// /hotkeys is the only place a user can discover these keys, so every globally
// bindable action has to be listed. app.header.expand was already missing when
// the thinking and model-select actions were added.
func TestHotkeysListEveryGlobalAction(t *testing.T) {
	listed := make(map[string]bool, len(hotkeyHintOrder))
	for _, id := range hotkeyHintOrder {
		listed[id] = true
	}
	for id := range appActions() {
		if _, dialogOnly := scopedModelsActionIDs[id]; dialogOnly {
			continue
		}
		if !listed[id] {
			t.Errorf("action %q is bindable but /hotkeys never lists it", id)
		}
	}
	for _, id := range hotkeyHintOrder {
		if _, ok := appActions()[id]; !ok {
			t.Errorf("/hotkeys lists %q, which is not a dispatchable action", id)
		}
	}
}

// app.suspend ships on Ctrl+Z, the conventional suspend key. The terminal is
// in raw mode (ISIG off), so Ctrl+Z arrives as a keystroke rather than
// SIGTSTP; resolving it to app.suspend is the only way Ctrl+Z can suspend
// the TUI into the background. Undo is not lost: it is bound to Ctrl+-
// (tui.editor.undo), which is the readline convention.
func TestSuspendBindsCtrlZ(t *testing.T) {
	if _, ok := appActions()[actionIDSuspend]; !ok {
		t.Fatal("app.suspend has no action mapping")
	}
	keys := defaultAppKeybindings().Keys(actionIDSuspend)
	if len(keys) != 1 || keys[0] != "ctrl+z" {
		t.Errorf("app.suspend ships bound to %v; want [ctrl+z]", keys)
	}
}

// The suspend handler asks the toolkit to suspend and repaints on resume.
func TestSuspendAsksTheToolkitAndRepaints(t *testing.T) {
	a := newTestApp(t)
	root := &Root{app: a}

	var suspended, repainted bool
	ctx := &tauui.Ctx{
		Suspend:       func() error { suspended = true; return nil },
		InvalidateAll: func() { repainted = true },
	}
	if !root.HandleAction(AppActionSuspend, ctx) {
		t.Fatal("suspend action was not handled")
	}
	if !suspended {
		t.Error("suspend did not call Ctx.Suspend")
	}
	if !repainted {
		t.Error("suspend did not repaint on resume")
	}
}

// A backend without suspend support must decline rather than pretend.
func TestSuspendDeclinesWhenUnsupported(t *testing.T) {
	a := newTestApp(t)
	root := &Root{app: a}

	if root.HandleAction(AppActionSuspend, &tauui.Ctx{}) {
		t.Error("suspend claimed the key with no backend support")
	}
}

// A failed suspend leaves the terminal in an unknown state; the user has to be
// told rather than left wondering why nothing happened.
func TestSuspendSurfacesFailure(t *testing.T) {
	a := newTestApp(t)
	root := &Root{app: a}

	ctx := &tauui.Ctx{Suspend: func() error { return errors.New("no tty") }}
	if !root.HandleAction(AppActionSuspend, ctx) {
		t.Fatal("suspend action was not handled")
	}
	if joined := convText(a); !strings.Contains(joined, "no tty") {
		t.Errorf("suspend failure was not surfaced: %q", joined)
	}
}

// app.model.select opens the same picker /model opens, so the key and the
// command cannot drift apart.
func TestModelSelectOpensTheModelPicker(t *testing.T) {
	a := newAppWithSelector(t)
	// cmdModel rewrites the editor through the loop context, so the picker
	// needs one captured before the action runs.
	ctx := stubCtx()
	a.loopCtx = ctx
	root := &Root{app: a}

	if !root.HandleAction(AppActionModelSelect, ctx) {
		t.Fatal("model select was not handled")
	}
	if got := a.editor.Text(); got != "/model " {
		t.Errorf("editor text = %q, want the model picker prefix %q", got, "/model ")
	}
}

// renderAppToBuffer paints the App's Root into a virtual cell buffer
// for golden comparisons. Returns nil if App.Root hasn't been built,
// so callers can gracefully skip when running without the toolkit
// loop.
func renderAppToBuffer(a *App, w, h int) *tauui.Buffer {
	if a.root == nil {
		return nil
	}
	buf := tauui.NewBuffer(w, h)
	clip := tauui.Rect{X: 0, Y: 0, W: w, H: h}
	painter := tauui.NewRenderPainter(buf, clip, tauui.Style{})
	drawer := tauui.NewDrawer(tauui.NewPainter(painter, tauui.Style{}))
	a.root.Layout(tauui.Rect{X: 0, Y: 0, W: w, H: h})
	a.root.Paint(drawer, &tauui.Ctx{Theme: appTheme()})
	return buf
}

// bufferRows returns each row's visible text, trimming trailing
// whitespace so tests stay robust across width changes.
func bufferRows(buf *tauui.Buffer, w, h int) []string {
	rows := make([]string, 0, h)
	for y := 0; y < h; y++ {
		var row strings.Builder
		for x := 0; x < w; x++ {
			cell := buf.At(x, y)
			if cell == nil || cell.R == 0 {
				row.WriteByte(' ')
			} else {
				row.WriteRune(cell.R)
			}
		}
		rows = append(rows, strings.TrimRight(row.String(), " "))
	}
	return rows
}

// TestAppGolden_RegionContractOnSlidingBuffer is the canonical
// app-level golden fixture: it confirms that the App root layout
// exposes the four-region contract (header, chat, editor, footer)
// by exercising the paint path against a buffer and verifying each
// region anchors to the expected edge. Tests skip when the test
// helper did not assemble the Root (no toolkit loop running).
func TestAppGolden_RegionContractOnSlidingBuffer(t *testing.T) {
	a := newAppWithSelector(t)
	a.editor.SetText(stubCtx(), "> hi")

	for _, h := range []int{20, 24, 30, 40} {
		buf := renderAppToBuffer(a, 80, h)
		if buf == nil {
			t.Skipf("App.Root not wired in test helper; skipping render at height %d", h)
		}
		rows := bufferRows(buf, 80, h)
		if len(rows) != h {
			t.Fatalf("expected %d rendered rows, got %d", h, len(rows))
		}
		if rows[0] == "" {
			t.Errorf("header row (0) is empty at height %d", h)
		}
		if rows[len(rows)-1] == "" {
			t.Errorf("footer row empty at height %d", h)
		}
	}
}

// TestAppGolden_SelectorRegionAppearsOnSlash verifies that the
// four-region contract still produces a non-trivial paint when the
// inline selector is visible. The selector pane is a fifth region
// wedged between the editor and footer.
func TestAppGolden_SelectorRegionAppearsOnSlash(t *testing.T) {
	a := newAppWithSelector(t)
	a.refreshSelector("/", stubCtx())

	buf := renderAppToBuffer(a, 80, 24)
	if buf == nil {
		t.Skip("App.Root not wired in test helper; skipping render")
	}
	if !a.selector.Shown() {
		t.Fatal("selector should be visible after refreshSelector('/')")
	}
	if buf.At(0, 0) == nil {
		t.Errorf("top-left cell of render buffer should not be nil")
	}
}

// ===== "New messages" toast =====

// newToastFixture builds an App with a real editor and a Root large enough to
// give the toast a known coordinate system. The Root carries a single Editor
// region (everything else is nil and skipped by NewAppLayout), so the
// VStack's measured height collapses to the editor's MinSize and the editor
// sits at the top of the layout — which is enough to drive the rect
// computation deterministically. The chat log is attached for the
// ThemeTokens lookup the paint path performs, but is not laid out.
//
// The document layout is built alongside the AppLayout so SetLayoutMode can
// be exercised in tests without panicking on a nil document.
func newToastFixture(t *testing.T) *App {
	t.Helper()
	a := newAppWithSelector(t)
	editor := newPromptEditorForTest()
	a.editor = editor
	editor.SetBorderStyle(tauui.Style{FG: a.chat.ThemeTokens().Border, BG: a.chat.ThemeTokens().Surface})
	rootRect := tauui.Rect{X: 0, Y: 0, W: 80, H: 20}
	appLayout := tauui.NewAppLayout(tauui.AppLayoutOpts{Editor: a.editor})
	appLayout.Layout(rootRect)
	documentLayout := tauui.NewDocumentLayout(tauui.AppLayoutOpts{Editor: a.editor, Chat: a.chat})
	documentLayout.Layout(rootRect)
	root := &Root{AppLayout: appLayout, app: a, document: documentLayout, layout: appLayout}
	a.root = root
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()
	return a
}

// setAutoScrollOff is the single mutation point for autoScrollOff. Transitions
// to false must also drop the pendingNewMessages counter — otherwise the
// "New messages" toast would survive a return-to-bottom and misreport the
// state.
func TestSetAutoScrollOffResetsCountOnReturnToBottom(t *testing.T) {
	a := newToastFixture(t)

	a.setAutoScrollOff(true)
	a.pendingNewMessages = 3
	if !a.autoScrollOff {
		t.Fatal("setup: autoScrollOff should be true")
	}

	a.setAutoScrollOff(false)
	if a.autoScrollOff {
		t.Error("autoScrollOff should be false after setAutoScrollOff(false)")
	}
	if a.pendingNewMessages != 0 {
		t.Errorf("pendingNewMessages = %d after return-to-bottom, want 0", a.pendingNewMessages)
	}

	// Idempotent: setting false again should not race or invalidate.
	a.setAutoScrollOff(false)
	if a.pendingNewMessages != 0 {
		t.Error("idempotent setAutoScrollOff(false) touched the counter")
	}
}

// chatScrolledAway must mirror autoScrollOff so the conversation event adapter
// has a single predicate to ask. The document-layout path is not wired yet
// (chatUserScrolled never fires there), so the predicate stays false in
// document mode and the toast never paints there.
func TestChatScrolledAwayMirrorsAutoScrollOff(t *testing.T) {
	a := newToastFixture(t)

	if a.chatScrolledAway() {
		t.Error("chatScrolledAway() = true on a fresh app")
	}
	a.setAutoScrollOff(true)
	if !a.chatScrolledAway() {
		t.Error("chatScrolledAway() = false after setAutoScrollOff(true)")
	}
}

// noteNewMessage bumps the counter only when the user is scrolled away. At
// the bottom the user is following the tail, so new rows are not "new since
// scrolling" and should not contribute to the toast count.
func TestNoteNewMessageOnlyWhileScrolledAway(t *testing.T) {
	a := newToastFixture(t)

	a.noteNewMessage()
	if a.pendingNewMessages != 0 {
		t.Errorf("pendingNewMessages = %d before any scroll-away; want 0", a.pendingNewMessages)
	}

	a.setAutoScrollOff(true)
	for i := 1; i <= 3; i++ {
		a.noteNewMessage()
		if a.pendingNewMessages != i {
			t.Errorf("after %d noteNewMessage calls, pendingNewMessages = %d, want %d", i, a.pendingNewMessages, i)
		}
	}
}

// The chat status line text format is the user-facing contract; lock it so a
// localisation or refactor cannot silently change it. The line reads
// "scroll to bottom (<keys>)", prefixed by the new-message count when any
// arrived while the user was scrolled away.
func TestChatStatusText(t *testing.T) {
	a := newToastFixture(t)

	// Not scrolled away: nothing to advertise, even with a stale count.
	a.pendingNewMessages = 3
	if got := a.chatStatusText(); got != "" {
		t.Errorf("text while following the tail = %q, want empty", got)
	}

	a.setAutoScrollOff(true)
	a.pendingNewMessages = 0
	if got := a.chatStatusText(); got != "scroll to bottom (ctrl+end)" {
		t.Errorf("text scrolled away, no new messages = %q, want %q", got, "scroll to bottom (ctrl+end)")
	}

	a.pendingNewMessages = 42
	if got := a.chatStatusText(); got != "New messages (42) · scroll to bottom (ctrl+end)" {
		t.Errorf("text scrolled away with count = %q, want %q", got, "New messages (42) · scroll to bottom (ctrl+end)")
	}
}

// With the editor pinned to a known row, the status line should sit one row
// above it, centred horizontally across the root, and exactly the text's
// width tall (one row).
func TestChatStatusRectCentredAboveEditor(t *testing.T) {
	a := newToastFixture(t)
	a.editor.Layout(tauui.Rect{X: 0, Y: 10, W: 80, H: 1})
	a.setAutoScrollOff(true)

	rect := a.chatStatusRect()
	text := a.chatStatusText()
	wantW := tauui.TextWidth(text)

	if rect.H != 1 {
		t.Errorf("rect.H = %d, want 1", rect.H)
	}
	if rect.W != wantW {
		t.Errorf("rect.W = %d, want %d (text width)", rect.W, wantW)
	}
	if rect.Y != a.editor.Rect().Y-1 {
		t.Errorf("rect.Y = %d, want editor.Y-1 = %d", rect.Y, a.editor.Rect().Y-1)
	}
	wantX := a.root.Rect().X + (a.root.Rect().W-rect.W)/2
	if rect.X != wantX {
		t.Errorf("rect.X = %d, want %d (centred across root width %d)", rect.X, wantX, a.root.Rect().W)
	}
}

// The status line must not paint (and the rect must be zero) while the user
// follows the tail — there is nothing to advertise.
func TestChatStatusRectHiddenWhileFollowing(t *testing.T) {
	a := newToastFixture(t)
	a.editor.Layout(tauui.Rect{X: 0, Y: 10, W: 80, H: 1})
	a.pendingNewMessages = 2

	if rect := a.chatStatusRect(); rect != (tauui.Rect{}) {
		t.Errorf("rect = %+v while following the tail, want zero rect", rect)
	}
}

// A very narrow terminal must clamp the status width so the centred x never
// goes negative.
func TestChatStatusRectClampsToTerminalWidth(t *testing.T) {
	a := newToastFixture(t)
	a.editor.Layout(tauui.Rect{X: 0, Y: 5, W: 5, H: 1})
	// Re-layout the Root at a narrow width.
	rootRect := tauui.Rect{X: 0, Y: 0, W: 10, H: 20}
	a.root.AppLayout.Layout(rootRect)
	a.setAutoScrollOff(true)

	rect := a.chatStatusRect()
	if rect.W > rootRect.W {
		t.Errorf("rect.W = %d exceeds root.W = %d", rect.W, rootRect.W)
	}
	if rect.X < rootRect.X || rect.X+rect.W > rootRect.X+rootRect.W {
		t.Errorf("rect = %+v not contained in root = %+v", rect, rootRect)
	}
}

// The status line advertises how to jump back to the bottom, so a left press
// on it must jump — the Root intercepts the click before it can reach the
// view underneath and start a chat-log selection. Presses elsewhere, and
// non-press events on the line itself, must not.
func TestChatStatusClickJumpsToBottom(t *testing.T) {
	a, viewport, _, _ := newAutoscrollFixture(t)
	// Layer the fixture's app into a real Root so the click path is the
	// production one: Root.Handle intercepts on the status rect.
	editorRect := tauui.Rect{X: 0, Y: 10, W: 40, H: 1}
	a.editor = newPromptEditorForTest()
	a.editor.Layout(editorRect)
	appLayout := tauui.NewAppLayout(tauui.AppLayoutOpts{Editor: a.editor})
	appLayout.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 20})
	a.root = &Root{AppLayout: appLayout, app: a, layout: appLayout}

	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}
	viewport.ScrollBottom(ctx)
	viewport.Handle(wheelEvent(tauui.MouseButtonWheelUp), ctx)
	if !a.autoScrollOff {
		t.Fatal("setup: user scroll did not suspend autoscroll")
	}
	rect := a.chatStatusRect()
	if rect.W <= 0 {
		t.Fatal("setup: status line has no rect while scrolled away")
	}

	// A release (or a wheel event) on the line is not a click.
	if a.root.Handle(tauui.MouseEvent{Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease, X: rect.X, Y: rect.Y}, ctx) {
		t.Error("release on the status line was claimed by the click handler")
	}
	if !a.autoScrollOff {
		t.Fatal("release on the status line resumed autoscroll")
	}

	// A press outside the line belongs to whatever sits under it.
	if a.root.Handle(tauui.MouseEvent{Button: tauui.MouseButtonLeft, Action: tauui.MousePress, X: 1, Y: rect.Y}, ctx) {
		t.Error("press left of the status line was claimed by the click handler")
	}
	if !a.autoScrollOff {
		t.Fatal("press outside the status line resumed autoscroll")
	}

	// The click itself.
	if !a.root.Handle(tauui.MouseEvent{Button: tauui.MouseButtonLeft, Action: tauui.MousePress, X: rect.X + rect.W/2, Y: rect.Y}, ctx) {
		t.Error("press on the status line was not handled")
	}
	if a.autoScrollOff {
		t.Error("clicking the status line did not resume autoscroll")
	}
	if !viewport.atBottom() {
		t.Errorf("clicking the status line left the viewport at %d, want the end", viewport.ScrollPos())
	}
}

// A return-to-bottom event (user scroll) must drop the counter so the toast
// disappears on the next paint. This is the integration with chatUserScrolled
// exercised through the existing chatviewport fixture.
func TestUserScrollResetsCountOnReturnToBottom(t *testing.T) {
	a, viewport, _, _ := newAutoscrollFixture(t)
	a.setAutoScrollOff(true)
	a.pendingNewMessages = 5

	a.chatUserScrolled(true) // user scrolled back to the end

	if a.autoScrollOff {
		t.Error("autoScrollOff should be false after returning to the bottom")
	}
	if a.pendingNewMessages != 0 {
		t.Errorf("pendingNewMessages = %d after return-to-bottom, want 0", a.pendingNewMessages)
	}
	_ = viewport
}

// The transcript-replace path (/resume, /branch) drops whatever the user was
// scrolled to along with the old content. setAutoScrollOff should re-arm
// the counter as part of that re-anchor.
func TestTranscriptReplacementResetsCount(t *testing.T) {
	a := newToastFixture(t)
	// replaceConversationWithMode only re-anchors autoscroll when clearChat
	// is true AND a.scroll is non-nil — that's the production wiring
	// (re-anchoring without a scroll view has nothing to drive). Mirror that
	// here so the test exercises the real code path.
	scroll := widgets.NewScrollView(widgets.ScrollViewOpts{Child: a.chat})
	a.scroll = scroll
	a.setAutoScrollOff(true)
	a.pendingNewMessages = 4

	a.replaceConversation(NewConversationModel())

	if a.autoScrollOff {
		t.Error("autoScrollOff should be false after transcript replacement")
	}
	if a.pendingNewMessages != 0 {
		t.Errorf("pendingNewMessages = %d after transcript replace, want 0", a.pendingNewMessages)
	}
}

// Switch layout modes must also drop the counter. The document layout has
// no scroll-away tracking, so leaving a counter behind would mean it never
// decrements.
func TestLayoutSwitchResetsCount(t *testing.T) {
	a := newToastFixture(t)
	a.setAutoScrollOff(true)
	a.pendingNewMessages = 7

	a.SetLayoutMode(LayoutDocument)

	if a.pendingNewMessages != 0 {
		t.Errorf("pendingNewMessages = %d after switch to document, want 0", a.pendingNewMessages)
	}
}

// Session-advertised commands with the same name as a TUI-local command
// must not reach the popup, /help, or autocomplete twice: local dispatch
// already wins, so the duplicate entry is display noise (e.g. /sessions
// listed once as the workspace and again as the shell builtin).
func TestExternalCommandsSkipLocalDuplicates(t *testing.T) {
	app := newTestApp(t)
	fs := app.sess.(*fakeSession)
	fs.mu.Lock()
	fs.slashInfos = []sdkSlashInfo{
		{Name: "sessions", Description: "shell-oriented builtin", Source: "builtin"},
		{Name: "skill:x", Description: "skill command", Source: "skill"},
	}
	fs.mu.Unlock()

	got := app.externalCommands()
	if len(got) != 1 {
		t.Fatalf("externalCommands = %+v, want only the non-local entry", got)
	}
	if got[0].Name != "skill:x" {
		t.Fatalf("externalCommands[0] = %q, want skill:x", got[0].Name)
	}
}

type bashSession struct {
	*fakeSession
	calls chan bashCall
}

type bashCall struct {
	ctx      context.Context
	command  string
	excluded bool
}

func (s *bashSession) ExecuteBash(ctx context.Context, command string, options sdk.BashOptions) (sdk.BashExecution, error) {
	s.calls <- bashCall{ctx: ctx, command: command, excluded: options.ExcludeFromContext}
	<-ctx.Done()
	return sdk.BashExecution{Command: command, Cancelled: true}, nil
}

func TestParseUserBash(t *testing.T) {
	for _, tc := range []struct {
		text, command string
		excluded, ok  bool
	}{
		{"!ls -la", "ls -la", false, true},
		{"  !! git status ", "git status", true, true},
		{"!", "", false, false},
		{"!!  ", "", true, false},
		{"ls !x", "", false, false},
	} {
		command, excluded, ok := parseUserBash(tc.text)
		if command != tc.command || excluded != tc.excluded || ok != tc.ok {
			t.Errorf("%q: got %q %v %v", tc.text, command, excluded, ok)
		}
	}
}

func TestSubmitUserBashRunsThroughSessionAndEscCancels(t *testing.T) {
	app := newTestApp(t)
	fake := app.sess.(*fakeSession)
	sess := &bashSession{fakeSession: fake, calls: make(chan bashCall, 1)}
	app.sess = sess
	editor := newPromptEditorForTest()
	app.editor = editor
	app.footer = newFooterForTest()
	ctx := testCtx()
	ctx.FocusedID = editor.ID()
	editor.SetText(ctx, "!!git status")
	if !app.trySubmit(ctx) {
		t.Fatal("shell input not consumed")
	}
	call := <-sess.calls
	if call.command != "git status" || !call.excluded {
		t.Fatalf("call = %+v", call)
	}
	tool := app.conv.Snapshot().Components[0].ToolExecution
	if tool.Name != "bash" || tool.State != toolExecutionExecuting || !strings.Contains(tool.Args, `"exclude_from_context":true`) {
		t.Fatalf("running row = %+v", tool)
	}
	editor.SetText(ctx, "!ls")
	app.trySubmit(ctx)
	if !strings.Contains(convText(app), "already running") {
		t.Fatal("second shell was admitted")
	}
	fake.mu.Lock()
	sent := len(fake.sentTexts)
	fake.mu.Unlock()
	if sent != 0 {
		t.Fatal("shell input reached the model")
	}
	if !(&Root{app: app}).HandleAction(tauui.ActionCancel, ctx) {
		t.Fatal("Esc not handled")
	}
	<-call.ctx.Done()
}

func TestNewScopesModelCyclingToConfiguredPatterns(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	var keys []string
	for _, candidate := range newTestApp(t).allScopedModelKeys() {
		if strings.HasPrefix(candidate, ai.ProviderOpenAI+"/") {
			keys = append(keys, candidate)
		}
	}
	require.GreaterOrEqual(t, len(keys), 2, "an OpenAI key should authenticate catalog models")

	sess := &fakeSession{stateModel: keys[1]}
	app, err := New(Options{Session: sess, ModelScope: []string{keys[0] + ":high", keys[1], "missing/*"}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = app.chat.Close() })
	assert.Equal(t, []string{keys[0], keys[1]}, app.scopedModelKeys)

	require.True(t, app.cycleScopedModel())
	assert.Equal(t, []string{keys[0]}, sess.setModels)
	assert.Equal(t, []string{"high"}, sess.thinkingLevels, "a scoped level applies when cycling to its model")

	unscoped, err := New(Options{Session: &fakeSession{}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = unscoped.chat.Close() })
	assert.Nil(t, unscoped.scopedModelKeys)
}

func TestEnableSkillCommandsFalseHidesOnlySkillCompletions(t *testing.T) {
	app := newTestApp(t)
	fs := app.sess.(*fakeSession)
	fs.mu.Lock()
	fs.slashInfos = []sdkSlashInfo{
		{Name: "review", Description: "prompt", Source: "prompt"},
		{Name: "skill:x", Description: "skill command", Source: "skill"},
	}
	fs.mu.Unlock()
	names := func() []string {
		var out []string
		for _, cmd := range app.externalCommands() {
			out = append(out, cmd.Name)
		}
		return out
	}

	app.applyDisplaySettings(&ai.Settings{})
	assert.Equal(t, []string{"review", "skill:x"}, names())

	disabled, err := ai.ParseSettings([]byte(`{"enableSkillCommands":false}`))
	require.NoError(t, err)
	app.applyDisplaySettings(disabled)
	assert.Equal(t, []string{"review"}, names())
	assert.True(t, app.sessionSlashCommandExists("skill:x"), "typed skill commands still reach the session")
}

type settingsSession struct {
	*fakeSession
	settings *ai.Settings
	steering string
}

func (s *settingsSession) Settings() *ai.Settings { return s.settings }

func (s *settingsSession) SetSteeringMode(mode string) error {
	s.steering = mode
	return nil
}

func (s *settingsSession) SetFollowUpMode(string) error { return nil }

func TestSessionSettingsDriveStartupAndSuccessfulReload(t *testing.T) {
	startup, err := ai.ParseSettings([]byte(`{"hideThinkingBlock":true,"enableSkillCommands":false}`))
	require.NoError(t, err)
	sess := &settingsSession{fakeSession: &fakeSession{}, settings: startup}
	a, err := New(Options{Session: sess})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.chat.Close() })
	assert.True(t, a.hideSkillCommands, "startup ignored merged session settings")

	a.conversationEvents = newConversationEventAdapter(a)
	a.conversationEvents.handle(sdk.Event{Type: "command_result", Payload: map[string]any{"command": "reload", "output": "boom", "is_error": true}})
	assert.True(t, a.hideSkillCommands, "rejected reload changed settings")
	sess.settings = &ai.Settings{}
	a.conversationEvents.handle(sdk.Event{Type: "command_result", Payload: map[string]any{"command": "reload", "output": "reloaded"}})
	assert.False(t, a.hideSkillCommands, "successful reload did not reapply settings")
	a.chat.mu.Lock()
	hidden := a.chat.hideThinkingBlock
	a.chat.mu.Unlock()
	assert.False(t, hidden, "reload did not reapply thinking display")
}

func TestReloadPreservesSessionLayout(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	for _, mode := range []string{"", "fixed", "document"} {
		for _, newSession := range []bool{false, true} {
			t.Run(fmt.Sprintf("layout=%s/new=%t", mode, newSession), func(t *testing.T) {
				sess := &settingsSession{fakeSession: &fakeSession{stateCWD: t.TempDir()}, settings: &ai.Settings{LayoutMode: mode, QuietStartup: true}}
				a, err := New(Options{
					Session:      sess,
					SettingsPath: filepath.Join(t.TempDir(), "settings.json"),
					SessionFactory: func(context.Context, sdk.SessionOptions) (sdk.Session, error) {
						return sess, nil
					},
				})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
				if newSession {
					cmdNew(a, "")
				}
				rect := tauui.Rect{W: 80, H: 24}
				a.root.Layout(rect)
				before := a.root.Editor.Rect()
				beforeMode := a.LayoutMode()
				wantMode, ok := ParseLayoutMode(mode)
				require.True(t, ok)
				assert.Equal(t, wantMode, beforeMode, "startup ignored the configured layout")
				footerBefore := a.root.Footer.Rect()
				a.conversationEvents.handle(sdk.Event{Type: "command_result", Payload: map[string]any{"command": "reload", "output": "reloaded settings, artifacts, and plugins"}})
				a.root.Layout(rect)
				assert.Equal(t, beforeMode, a.LayoutMode(), "reload switched layout models")
				assert.Equal(t, before.Y, a.root.Editor.Rect().Y, "reload moved the editor vertically")
				assert.Equal(t, footerBefore.Y, a.root.Footer.Rect().Y, "reload moved the footer vertically")
			})
		}
	}
}

func TestReloadResetsRemovedDisplaySettingsAndAppliesTheme(t *testing.T) {
	startup, err := ai.ParseSettings([]byte(`{"editorPaddingX":3,"autocompleteMaxVisible":9,"steeringMode":"one-at-a-time","theme":"light"}`))
	require.NoError(t, err)
	sess := &settingsSession{fakeSession: &fakeSession{}, settings: startup}
	a, err := New(Options{Session: sess})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.chat.Close() })
	assert.Equal(t, 3, a.editor.Padding())
	assert.Equal(t, 9, a.editor.Model().AutocompleteMaxVisible())
	assert.Equal(t, "one-at-a-time", sess.steering)
	assert.Equal(t, "light", a.footer.data.theme)

	sess.settings = &ai.Settings{}
	a.conversationEvents = newConversationEventAdapter(a)
	a.conversationEvents.handle(sdk.Event{Type: "command_result", Payload: map[string]any{"command": "reload", "output": "reloaded"}})
	assert.Equal(t, 1, a.editor.Padding(), "removed editorPaddingX kept its old value")
	assert.Equal(t, 5, a.editor.Model().AutocompleteMaxVisible(), "removed autocompleteMaxVisible kept its old value")
	assert.Equal(t, "all", sess.steering, "removed steeringMode did not return to the default")
	assert.Equal(t, "system", a.footer.data.theme, "reload did not apply the theme")

	zero, err := ai.ParseSettings([]byte(`{"editorPaddingX":0}`))
	require.NoError(t, err)
	a.applyDisplaySettings(zero)
	assert.Equal(t, 0, a.editor.Padding(), "explicit zero padding ignored")
}

func TestEditorPaintTracksThinkingTokensAcrossThemeChanges(t *testing.T) {
	a := newEditorChromeApp(t)
	a.themeLookup = func(name string) (ThemeDefinition, bool) {
		if name != "custom" {
			return ThemeDefinition{}, false
		}
		return ThemeDefinition{Values: map[string]string{"thinkingOff": "#123456", "thinkingMax": "#abcdef", "border": "#654321"}, Dim: []string{"thinkingOff"}}, true
	}
	a.applyTheme("custom")
	for _, tc := range []struct {
		level string
		color tauui.Color
		dim   bool
	}{
		{"off", tauui.ColorRGB(0x12, 0x34, 0x56), true},
		{"max", tauui.ColorRGB(0xab, 0xcd, 0xef), false},
	} {
		a.footer.data.thinkingLevel = tc.level
		a.refreshEditorChrome()
		st := paintedRuleStyle(t, a.editor)
		assert.Equal(t, tc.color, st.FG, tc.level)
		assert.Equal(t, tc.dim, st.Attr&tauui.AttrDim != 0, tc.level)
	}
	a.applyThemeFromExtension(extensions.Theme{Name: "in-memory", Values: map[string]string{"thinkingMax": "#102030"}, Dim: []string{"thinkingMax"}})
	st := paintedRuleStyle(t, a.editor)
	assert.Equal(t, tauui.ColorRGB(0x10, 0x20, 0x30), st.FG)
	assert.NotZero(t, st.Attr&tauui.AttrDim)
	a.applyTheme("custom")
	st = paintedRuleStyle(t, a.editor)
	assert.Equal(t, tauui.ColorRGB(0xab, 0xcd, 0xef), st.FG)
	assert.Zero(t, st.Attr&tauui.AttrDim)
}

func TestRootWheelFallsBackToTranscript(t *testing.T) {
	a, viewport, _, _ := newAutoscrollFixture(t)
	editor := tauui.NewPromptEditor()
	layout := tauui.NewAppLayout(tauui.AppLayoutOpts{Chat: viewport, Editor: editor})
	root := &Root{AppLayout: layout, layout: layout, app: a}
	a.root = root
	a.editor = editor
	root.Layout(tauui.Rect{W: 40, H: 15})
	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}
	viewport.ScrollBottom(ctx)
	before := viewport.ScrollPos()
	e := wheelEvent(tauui.MouseButtonWheelUp)
	e.Y = editor.Rect().Y
	e.WheelDelta = 2
	require.True(t, root.Handle(e, ctx))
	assert.Equal(t, before-6, viewport.ScrollPos())
	assert.True(t, a.autoScrollOff)
	// A wheel already delivered to the transcript must not be applied twice.
	before = viewport.ScrollPos()
	e.Y = viewport.Rect().Y
	require.True(t, root.Handle(e, ctx))
	assert.Equal(t, before-6, viewport.ScrollPos())
}

type wheelOverlayProbe struct {
	tauui.View
	mouseEvents int
	keyEvents   int
	focusedID   tauui.ID
}

func (v *wheelOverlayProbe) Focusable() bool { return true }
func (v *wheelOverlayProbe) MouseOpaque()    {}
func (v *wheelOverlayProbe) Handle(e tauui.Event, ctx *tauui.Ctx) bool {
	switch e.(type) {
	case tauui.MouseEvent:
		v.mouseEvents++
	case tauui.KeyEvent:
		v.keyEvents++
		v.focusedID = ctx.FocusedID
	}
	return v.View.Handle(e, ctx)
}

func TestOverlayWheelRoutingThroughToolkit(t *testing.T) {
	for _, layoutMode := range []LayoutMode{LayoutFixed, LayoutDocument} {
		for _, location := range []string{"chat", "editor", "outside", "scrolling-overlay"} {
			t.Run(string(layoutMode)+"/"+location, func(t *testing.T) {
				a, viewport, chat, _ := newAutoscrollFixture(t)
				editor := tauui.NewPromptEditor()
				fixed := tauui.NewAppLayout(tauui.AppLayoutOpts{Chat: viewport, Editor: editor})
				root := &Root{AppLayout: fixed, layout: fixed, app: a}
				a.root = root
				a.editor = editor

				if layoutMode == LayoutDocument {
					root.document = tauui.NewDocumentLayout(tauui.AppLayoutOpts{Chat: chat, Editor: editor})
					root.layout = root.document
				}
				be := headless.New(geom.Size{W: 40, H: 15})
				defer be.Close()
				overlay := &wheelOverlayProbe{View: tauui.NewPromptEditor()}
				overlayScroll := widgets.NewScrollView(widgets.ScrollViewOpts{Child: tauui.NewText(strings.Repeat("overlay row\n", 40)), Scrollbar: widgets.ScrollbarHidden})
				if location == "scrolling-overlay" {
					overlay.View = overlayScroll
				}
				var before int
				checked := false
				var err error
				a.app, err = tauui.New(tauui.Opts{Root: root, Backend: be, OnTerminalReply: func(tauui.TerminalReplyEvent) {
					checked = true
					if location == "scrolling-overlay" {
						assert.Equal(t, 6, overlayScroll.ScrollPos())
						if layoutMode == LayoutDocument {
							assert.Equal(t, before, root.document.ScrollBack())
						} else {
							assert.Equal(t, before, viewport.ScrollPos())
						}
					} else if layoutMode == LayoutDocument {
						assert.Equal(t, before+6, root.document.ScrollBack())
					} else {
						assert.Equal(t, before-6, viewport.ScrollPos())
						assert.True(t, a.autoScrollOff)
					}
					wantMouse := 1
					if location == "outside" {
						wantMouse = 0
					}
					assert.Equal(t, wantMouse, overlay.mouseEvents)
					assert.Equal(t, 1, overlay.keyEvents, "wheel reached focused overlay as raw key input")
					assert.Equal(t, overlay.ID(), overlay.focusedID)
					a.app.Quit()
				}})
				require.NoError(t, err)
				require.NoError(t, a.app.Post(func(ctx *tauui.UpdateCtx) {
					viewport.ScrollBottom(&tauui.Ctx{Invalidate: func(tauui.Rect) {}})
					if layoutMode == LayoutDocument {
						before = root.document.ScrollBack()
					} else {
						before = viewport.ScrollPos()
					}
					anchor := tauui.AnchorTopLeft
					if location == "editor" {
						anchor = tauui.AnchorBottomLeft
					}
					ctx.ShowOverlay(rovel.OverlayOpts{Root: overlay, Modal: true, Place: tauui.EdgeAnchored{Anchor: anchor, W: 10, H: 3}})
					e := wheelEvent(tauui.MouseButtonWheelUp)
					e.WheelDelta = 2
					if location == "editor" {
						e.Y = 14
					}
					if location == "outside" {
						e.X = 30
					}
					if location == "scrolling-overlay" {
						e.Button = tauui.MouseButtonWheelDown
					}
					be.SendEvent(e)
					be.SendEvent(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'z'})
					be.SendEvent(tauui.TerminalReplyEvent{Sequence: "wheel-check"})
				}))
				require.NoError(t, a.app.Run())
				assert.True(t, checked)
			})
		}
	}
}

func TestWheelSettingsReloadThroughToolkit(t *testing.T) {
	for _, mode := range []LayoutMode{LayoutFixed, LayoutDocument} {
		t.Run(string(mode), func(t *testing.T) {
			a, viewport, chat, _ := newAutoscrollFixture(t)
			editor := tauui.NewPromptEditor()
			fixed := tauui.NewAppLayout(tauui.AppLayoutOpts{Chat: viewport, Editor: editor})
			root := &Root{AppLayout: fixed, layout: fixed, app: a}
			a.root = root
			a.editor = editor
			if mode == LayoutDocument {
				root.document = tauui.NewDocumentLayout(tauui.AppLayoutOpts{Chat: chat, Editor: editor})
				root.layout = root.document
			}
			settings, err := ai.ParseSettings([]byte(fmt.Sprintf(`{"fullscreenWheelScrollLines":3,"layoutMode":%q}`, mode)))
			require.NoError(t, err)
			sess := &settingsSession{fakeSession: &fakeSession{}, settings: settings}
			a.sess = sess
			a.conversationEvents = newConversationEventAdapter(a)
			be := headless.New(geom.Size{W: 40, H: 15})
			defer be.Close()
			position := func() int {
				if mode == LayoutDocument {
					return root.document.ScrollBack()
				}
				return -viewport.ScrollPos()
			}
			var before, phase int
			a.app, err = tauui.New(tauui.Opts{Root: root, Backend: be, WheelScrollLines: new(settings.ResolveFullscreenWheelScrollLines()), OnTerminalReply: func(tauui.TerminalReplyEvent) {
				switch phase {
				case 0:
					assert.Equal(t, before+6, position())
					a.conversationEvents.handle(sdk.Event{Type: "command_result", Payload: map[string]any{"command": "reload", "is_error": true}})
				case 1:
					assert.Equal(t, before+6, position(), "failed reload changed wheel count")
					sess.settings, err = ai.ParseSettings([]byte(fmt.Sprintf(`{"fullscreenWheelScrollLines":2,"layoutMode":%q}`, mode)))
					require.NoError(t, err)
					a.conversationEvents.handle(sdk.Event{Type: "command_result", Payload: map[string]any{"command": "reload"}})
				case 2:
					assert.Equal(t, before+20, position(), "Alt or reload applied twice")
					sess.settings = &ai.Settings{LayoutMode: string(mode)}
					a.conversationEvents.handle(sdk.Event{Type: "command_result", Payload: map[string]any{"command": "reload"}})
				case 3:
					assert.Equal(t, before+2, position(), "removed setting did not reset to auto")
					a.app.Quit()
					phase++
					return
				}
				phase++
				before = position()
				e := wheelEvent(tauui.MouseButtonWheelUp)
				e.WheelDelta = 2
				if phase == 2 {
					e.Mod = tauui.ModAlt
				}
				be.SendEvent(e)
				be.SendEvent(tauui.TerminalReplyEvent{Sequence: "wheel-settings"})
			}})
			require.NoError(t, err)
			require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) {
				viewport.ScrollBottom(nil)
				before = position()
				e := wheelEvent(tauui.MouseButtonWheelUp)
				e.WheelDelta = 2
				be.SendEvent(e)
				be.SendEvent(tauui.TerminalReplyEvent{Sequence: "wheel-settings"})
			}))
			require.NoError(t, a.app.Run())
			assert.Equal(t, 4, phase)
		})
	}
}

// Rows the chat window loads (or drops) above the viewed content must not
// slide that content under a reader in the top-anchored fixed shell.
func TestChatRenderUpdateKeepsFixedViewOnWindowGrowth(t *testing.T) {
	a, viewport, _, _ := newAutoscrollFixture(t)
	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}
	viewport.ScrollTo(ctx, 5)

	a.handleChatRenderUpdate(chatLogRenderUpdate{installed: true, rect: viewport.Rect(), topDelta: 3}, func(tauui.Rect) {}, nil)
	if got := viewport.ScrollPos(); got != 8 {
		t.Fatalf("scroll pos after 3 rows added above = %d, want 8", got)
	}
	a.handleChatRenderUpdate(chatLogRenderUpdate{installed: true, rect: viewport.Rect(), topDelta: -2}, func(tauui.Rect) {}, nil)
	if got := viewport.ScrollPos(); got != 6 {
		t.Fatalf("scroll pos after 2 rows dropped above = %d, want 6", got)
	}
}

func TestPausedEventShowsPauseUntilResponseStarts(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "paused", Payload: map[string]any{"target": "nightly"}})
	if got := a.workingMessage(); !strings.HasPrefix(got, `paused by flag "nightly" since `) {
		t.Fatalf("workingMessage = %q, want pause notice", got)
	}

	a.handleEvent(sdk.Event{Type: "message_start", Payload: map[string]any{"role": "assistant"}})
	if got := a.workingMessage(); got != defaultWorkingMessage {
		t.Fatalf("workingMessage after resume = %q, want %q", got, defaultWorkingMessage)
	}
}

func TestReloadCleanupFailureAppliesAdmittedSessionSettings(t *testing.T) {
	startup, err := ai.ParseSettings([]byte(`{"hideThinkingBlock":true,"enableSkillCommands":false}`))
	require.NoError(t, err)
	sess := &settingsSession{fakeSession: &fakeSession{}, settings: startup}
	app, err := New(Options{Session: sess})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, app.chat.Close()) })
	require.True(t, app.hideSkillCommands)
	sess.settings = &ai.Settings{}
	app.conversationEvents = newConversationEventAdapter(app)
	app.conversationEvents.handle(sdk.Event{Type: "command_result", Payload: map[string]any{"command": "reload", "output": "old plugin cleanup failed", "is_error": true}})
	require.False(t, app.hideSkillCommands)
	app.chat.mu.Lock()
	hidden := app.chat.hideThinkingBlock
	app.chat.mu.Unlock()
	require.False(t, hidden)
}

func TestNativeEditorKeysReachModelOperations(t *testing.T) {
	cases := []struct {
		key    string
		want   string
		cursor int
	}{
		{"ctrl+k", "one ", 4},
		{"ctrl+u", "two", 0},
		{"ctrl+w", "two", 0},
		{"alt+d", "one ", 4},
		{"alt+delete", "one ", 4},
		{"ctrl+-", "", 0},
		{"alt+b", "one two", 0},
		{"alt+left", "one two", 0},
		{"ctrl+left", "one two", 0},
		{"alt+f", "one two", 7},
		{"alt+right", "one two", 7},
		{"ctrl+right", "one two", 7},
		{"ctrl+a", "one two", 0},
		{"ctrl+e", "one two", 7},
		{"ctrl+b", "one two", 3},
		{"ctrl+f", "one two", 5},
		{"ctrl+d", "one wo", 4},
		{"ctrl+y", "one savedtwo", 9},
		{"alt+y", "one oldertwo", 9},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			a := newAppWithSelector(t)
			m := a.editor.Model()
			m.SetText("older")
			m.DeleteToLineStart()
			m.MoveLeft()
			m.SetText("saved")
			m.DeleteToLineStart()
			m.SetText("one two")
			m.SetCursor(4)
			if tc.key == "alt+y" {
				m.Yank()
			}
			ks, err := keybindings.CompileKey(tc.key)
			require.NoError(t, err)
			act, ok := (appKeymap{app: a}).Resolve(tauui.KeyCtxTextInput, ks)
			require.True(t, ok, "key must resolve")
			require.True(t, a.editor.HandleAction(act, &tauui.Ctx{FocusedID: a.editor.ID()}), "editor must handle action %v", act)
			assert.Equal(t, tc.want, m.Text())
			assert.Equal(t, tc.cursor, m.Cursor())
		})
	}
}

func TestNativeExtensionInsertAppliesOnce(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	a.extensions.SetEditorText("base")
	var changes []string
	a.editor.SetOnChange(func(s string, _ *tauui.Ctx) { changes = append(changes, s) })
	a.extensions.InsertEditorText(0, "X-")
	assert.Equal(t, "X-base", a.editor.Text())
	assert.Equal(t, a.editor.Text(), a.extensions.EditorText())
	assert.Equal(t, []string{"X-base"}, changes)
	a.editor.Model().Undo()
	assert.Equal(t, "base", a.editor.Text())
}

func TestNativeSubmitStoresExpandedHistoryAndDiscardsUndo(t *testing.T) {
	app := newTestApp(t)
	app.editor = newPromptEditorForTest()
	app.footer = newFooterForTest()
	ctx := testCtx()
	ctx.FocusedID = app.editor.ID()
	var handled string
	app.cmds.Register(Command{Name: "capture", Handler: func(_ *App, args string) { handled = args }})
	app.editor.Model().Insert(" /capture ")
	body := strings.Repeat("pasted line\n", 20)
	app.editor.Model().Paste(body)
	var changes []string
	app.editor.SetOnChange(func(text string, _ *tauui.Ctx) { changes = append(changes, text) })
	require.True(t, app.trySubmit(ctx))
	want := "/capture " + strings.TrimSpace(body)
	assert.Equal(t, strings.TrimSpace(body), handled)
	assert.Equal(t, []string{want}, app.editor.Model().HistoryEntries())
	assert.Empty(t, app.editor.Text())
	assert.Empty(t, app.editor.Model().LargePastes())
	assert.Equal(t, []string{""}, changes)
	app.editor.Model().Undo()
	assert.Empty(t, app.editor.Text())
	app.editor.Model().HistoryPrev()
	assert.Equal(t, want, app.editor.Text())
}

func TestNativeSubmitWhitespaceClearsWithoutRoutingOrHistory(t *testing.T) {
	app := newTestApp(t)
	app.editor = newPromptEditorForTest()
	ctx := testCtx()
	ctx.FocusedID = app.editor.ID()
	app.editor.Model().Paste(strings.Repeat(" \n", 20))
	require.True(t, app.trySubmit(ctx))
	assert.Empty(t, app.editor.Text())
	assert.Empty(t, app.editor.Model().HistoryEntries())
	assert.Empty(t, app.editor.Model().LargePastes())
	app.editor.Model().Undo()
	assert.Empty(t, app.editor.Text())
	fs := app.sess.(*fakeSession)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	assert.Empty(t, fs.sentTexts)
}

func TestNativeEditorCharacterJumpUsesDecodedTextWithoutEditing(t *testing.T) {
	a := newAppWithSelector(t)
	m := a.editor.Model()
	m.SetText("o+界o\nend+")
	m.SetCursor(0)
	ctx := &tauui.Ctx{FocusedID: a.editor.ID()}
	var changes int
	a.editor.SetOnChange(func(string, *tauui.Ctx) { changes++ })
	handle := func(key tauui.KeyEvent) {
		act, ok := (appKeymap{app: a}).Resolve(tauui.KeyCtxTextInput, keymap.KeystrokeOf(key))
		if ok {
			require.True(t, a.editor.HandleAction(act, ctx))
			return
		}
		require.True(t, a.editor.Handle(key, ctx))
	}
	handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: ']', Mod: tauui.ModCtrl})
	handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'o'})
	assert.Equal(t, len("o+界"), m.Cursor())
	assert.Zero(t, changes)
	handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: ']', Mod: tauui.ModCtrl | tauui.ModAlt})
	handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: '+', Mod: tauui.ModShift})
	assert.Equal(t, 1, m.Cursor())
	assert.Zero(t, changes)
	assert.Equal(t, "o+界o\nend+", m.Text())
	handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: ']', Mod: tauui.ModCtrl})
	handle(tauui.KeyEvent{Key: tauui.KeyLeft})
	handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'X'})
	assert.Equal(t, "Xo+界o\nend+", m.Text())
	assert.Equal(t, 1, changes)
}

func TestNativeSubmitReplacesBackslashAtCursorWithoutLosingPaste(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(nil))
	a := newAppWithSelector(t)
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	m := a.editor.Model()
	m.Paste(strings.Repeat("pasted body\n", 20))
	m.Insert("\\tail")
	before := m.Text()
	m.SetCursor(len(before) - len("tail"))
	var changes []string
	a.editor.SetOnChange(func(s string, _ *tauui.Ctx) { changes = append(changes, s) })
	action, ok := (appKeymap{app: a}).Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyEnter})
	require.True(t, ok)
	require.False(t, a.editor.HandleAction(action, ctx))
	require.True(t, (&Root{app: a}).HandleAction(action, ctx))
	assert.Equal(t, strings.Replace(before, "\\tail", "\ntail", 1), m.Text())
	assert.Equal(t, strings.Repeat("pasted body\n", 20)+"\ntail", m.ExpandedText())
	require.NotEmpty(t, changes)
	assert.Equal(t, m.Text(), changes[len(changes)-1])
	require.True(t, a.editor.HandleAction(keymap.ActionUndo, ctx))
	require.True(t, a.editor.HandleAction(keymap.ActionUndo, ctx))
	assert.Equal(t, before, m.Text())
	assert.Equal(t, strings.Repeat("pasted body\n", 20)+"\\tail", m.ExpandedText())
}

func TestNativeBackslashEnterWithSwappedBindings(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{
		"tui.input.submit": {"shift+enter"}, "tui.input.newLine": {"enter"},
	}))
	a := newAppWithSelector(t)
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	var called []string
	a.cmds.Register(Command{Name: "check", Handler: func(_ *App, args string) { called = append(called, args) }})
	a.editor.SetText(ctx, "/check \\argument")
	a.editor.Model().SetCursor(len("/check \\"))
	action, ok := (appKeymap{app: a}).Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyEnter})
	require.True(t, ok)
	if !a.editor.HandleAction(action, ctx) {
		require.True(t, (&Root{app: a}).HandleAction(action, ctx))
	}
	assert.Equal(t, []string{"argument"}, called)
	assert.Empty(t, a.editor.Text())
	a.editor.SetText(ctx, "hello")
	action, ok = (appKeymap{app: a}).Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyEnter})
	require.True(t, ok)
	require.True(t, a.editor.HandleAction(action, ctx))
	assert.Equal(t, "hello\n", a.editor.Text())
}

func TestNativeExplicitHistoryBindingPrecedesModelCycle(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{
		"tui.editor.historyPrevious": {"ctrl+p"}, "tui.editor.historyNext": {"ctrl+n"},
	}))
	a := newAppWithSelector(t)
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	m := a.editor.Model()
	m.AddHistory("old\nentry")
	m.AddHistory("new\nentry")
	a.editor.SetText(ctx, "draft\nbody")
	m.SetCursor(8)
	var changes []string
	a.editor.SetOnChange(func(s string, _ *tauui.Ctx) { changes = append(changes, s) })
	km := appKeymap{app: a}
	for _, step := range []struct {
		key  rune
		text string
	}{{'p', "new\nentry"}, {'p', "old\nentry"}, {'n', "new\nentry"}, {'n', "draft\nbody"}} {
		action, ok := km.Resolve(tauui.KeyCtxTextInput, ctrl(step.key))
		require.True(t, ok)
		require.True(t, a.editor.HandleAction(action, ctx), "history key must be consumed by editor, not model cycling")
		assert.Equal(t, step.text, m.Text())
	}
	assert.Equal(t, 8, m.Cursor())
	assert.Len(t, changes, 4)
	keybindings.SetDefaultManager(keybindings.NewDefault(nil))
	action, ok := km.Resolve(tauui.KeyCtxTextInput, ctrl('p'))
	require.True(t, ok)
	assert.Equal(t, AppActionModelCycleForward, action)
}

func TestNativeNonemptyCtrlDRespectsDeleteBinding(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	keybindings.SetDefaultManager(keybindings.NewDefault(keybindings.Config{"tui.editor.deleteCharForward": {"f2"}}))
	a := newAppWithSelector(t)
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	a.editor.SetText(ctx, "abc")
	a.editor.Model().SetCursor(1)
	km := appKeymap{app: a}
	_, ok := km.Resolve(tauui.KeyCtxTextInput, ctrl('d'))
	assert.False(t, ok, "disabled Ctrl+D must not delete from a nonempty draft")
	action, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyF2})
	require.True(t, ok)
	require.True(t, a.editor.HandleAction(action, ctx))
	assert.Equal(t, "ac", a.editor.Text())
	keybindings.SetDefaultManager(keybindings.NewDefault(nil))
	action, ok = km.Resolve(tauui.KeyCtxTextInput, ctrl('d'))
	require.True(t, ok)
	assert.Equal(t, tauui.ActionDeleteFwd, action)
	a.editor.SetText(ctx, "")
	action, ok = km.Resolve(tauui.KeyCtxTextInput, ctrl('d'))
	require.True(t, ok)
	assert.Equal(t, AppActionQuitNow, action)
}

func TestFileSelectorGuardUsesProviderTokenBoundaries(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"@", true},
		{"see @file", true},
		{"see，@file", true},
		{"see\u3000@file", true},
		{"see (@\"my dir/ma", true},
		{"see <@file", true},
		{"see `@file", true},
		{"old line\n(@file", true},
		{"see @\"my @dir/ma", true},
		{"email@example", false},
		{"ordinary text", false},
		{"see @file done", false},
		{"see @\"closed\"", false},
		{"name(@\"dir/ma", false},
		{"name，ordinary", false},
	} {
		t.Run(tc.text, func(t *testing.T) {
			if got := shouldCheckFileSelector(tc.text, len(tc.text)); got != tc.want {
				t.Fatalf("text=%q got=%t want=%t", tc.text, got, tc.want)
			}
		})
	}
}

func TestNativeFileCompletionAfterWrappedUnicodeQuotedPrefix(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Mkdir(filepath.Join(dir, "my dir"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "my dir", "说明.md"), []byte("body"), 0o644))
	a := newAppWithSelector(t)
	a.sess = &fakeSession{stateCWD: dir}
	for _, before := range []string{"查看，(", "前文\n查看\u3000<"} {
		text := before + "@\"my dir/说\" 后文"
		col := len(before + "@\"my dir/说")
		a.editor.Model().SetText(text)
		a.editor.Model().SetCursor(col)
		a.refreshSelector(text, nil)
		if !a.selector.Shown() || a.selectorMode != selectorModeFiles || len(a.completionSelector.Items) != 1 {
			t.Fatalf("text=%q shown=%t completion=%+v", text, a.selector.Shown(), a.completionSelector)
		}
		a.loopMu.Lock()
		a.loopCtx = stubCtx()
		a.loopMu.Unlock()
		a.applySelector(0, a.selector.Items()[0])
		got, cursor := a.editor.Text(), a.editor.Model().Cursor()
		want := before + "@\"my dir/说明.md\"  后文"
		if got != want || cursor != len(before+"@\"my dir/说明.md\" ") {
			t.Fatalf("text=%q got=%q cursor=%d", text, got, cursor)
		}
	}
}

func TestNativeFileCompletionPreservesPasteAndUndo(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "说明.md"), []byte("body"), 0o644))
	a := newAppWithSelector(t)
	a.sess = &fakeSession{stateCWD: dir}
	ctx := stubCtx()
	a.loopMu.Lock()
	a.loopCtx = ctx
	a.loopMu.Unlock()
	m := a.editor.Model()
	body := strings.Repeat("paste body\n", 20)
	m.Paste(body)
	m.Insert("\n查看 @说 后文")
	before := m.Text()
	m.SetCursor(len(before) - len(" 后文"))
	a.refreshSelector(before, ctx)
	require.True(t, a.selector.Shown())
	require.Len(t, a.selector.Items(), 1)
	var changes []string
	a.editor.SetOnChange(func(s string, _ *tauui.Ctx) { changes = append(changes, s) })
	var moves []tauui.EditorCursorMoveEvent
	a.editor.SetOnCursorMove(func(e tauui.EditorCursorMoveEvent, _ *tauui.Ctx) { moves = append(moves, e) })
	a.applySelector(0, a.selector.Items()[0])
	want := strings.Replace(before, "@说", "@说明.md ", 1)
	assert.Equal(t, want, m.Text())
	assert.Equal(t, body+"\n查看 @说明.md  后文", m.ExpandedText())
	assert.Equal(t, len(want)-len(" 后文"), m.Cursor())
	assert.Equal(t, []string{want}, changes)
	require.Len(t, moves, 1)
	assert.Equal(t, m.Cursor(), moves[0].Cursor)
	m.Undo()
	assert.Equal(t, before, m.Text())
	assert.Equal(t, body+"\n查看 @说 后文", m.ExpandedText())
	m.Paste(body)
	assert.Contains(t, m.Text(), "[paste #2 ")
}

func TestNativeArgumentCompletionUsesProviderOnIndentedLogicalLine(t *testing.T) {
	a := newAppWithSelector(t)
	ctx := stubCtx()
	a.loopMu.Lock()
	a.loopCtx = ctx
	a.loopMu.Unlock()
	var called []string
	a.cmds.Register(Command{Name: "inspect", CompleteArgs: func(_ *App, prefix string) []autocomplete.Item {
		called = append(called, prefix)
		return []autocomplete.Item{{Value: "目标", Label: "目标"}}
	}})
	for _, indent := range []string{"  ", "\ufeff", "\u00a0"} {
		called = nil
		text := indent + "/inspect 目 后文\n后行"
		a.editor.Model().SetText(text)
		a.editor.Model().SetCursor(len(text) - len(" 后文\n后行"))
		a.refreshSelector(text, ctx)
		require.True(t, a.selector.Shown())
		require.Equal(t, []string{"目"}, called)
		a.applySelector(0, a.selector.Items()[0])
		assert.Equal(t, indent+"/inspect 目标 后文\n后行", a.editor.Text())
		assert.Equal(t, len(indent+"/inspect 目标"), a.editor.Model().Cursor())
		a.editor.Model().Undo()
		assert.Equal(t, text, a.editor.Text())
	}
	called = nil
	text := "\u0085/inspect 目"
	a.editor.Model().SetText(text)
	a.refreshSelector(text, ctx)
	assert.False(t, a.selector.Shown())
	assert.Empty(t, called)
}

func TestNativeSlashSelectorUsesProviderRankingAndCursor(t *testing.T) {
	a := newAppWithSelector(t)
	ctx := stubCtx()
	a.loopMu.Lock()
	a.loopCtx = ctx
	a.loopMu.Unlock()
	a.testExternalCommands = []ExternalCommand{{Name: "skill:zzhelp", Description: "skill"}, {Name: "heavenly", Description: "ordinary"}}
	for _, tc := range []struct {
		text   string
		cursor int
	}{{"/hly", len("/hly")}, {"  /hly 后文\n后行", len("  /hly")}} {
		a.editor.Model().SetText(tc.text)
		a.editor.Model().SetCursor(tc.cursor)
		a.refreshSelector(tc.text, ctx)
		require.True(t, a.selector.Shown())
		require.Equal(t, selectorModeCommands, a.selectorMode)
		require.Equal(t, "heavenly", a.selector.Items()[0].Name)
		a.applySelector(0, a.selector.Items()[0])
		want := tc.text[:tc.cursor-len("/hly")] + "/heavenly " + tc.text[tc.cursor:]
		assert.Equal(t, want, a.editor.Text())
		assert.Equal(t, tc.cursor-len("/hly")+len("/heavenly "), a.editor.Model().Cursor())
		a.editor.Model().Undo()
		assert.Equal(t, tc.text, a.editor.Text())
	}
}

func TestNativeAttachmentCompletionIgnoresQuotesOnPreviousLines(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "目标.md"), []byte("body"), 0o600))
	a := newAppWithSelector(t)
	a.sess = &fakeSession{stateCWD: dir}
	ctx := stubCtx()
	a.loopMu.Lock()
	a.loopCtx = ctx
	a.loopMu.Unlock()
	for _, before := range []string{"an unmatched \"quote\n", "an unmatched @\"attachment\n"} {
		text := before + "查看 @\"目 后文"
		a.editor.Model().SetText(text)
		a.editor.Model().SetCursor(len(text) - len(" 后文"))
		a.refreshSelector(text, ctx)
		require.True(t, a.selector.Shown())
		require.Len(t, a.selector.Items(), 1)
		a.applySelector(0, a.selector.Items()[0])
		assert.Equal(t, before+"查看 @\"目标.md\"  后文", a.editor.Text())
		assert.Equal(t, len(before+"查看 @\"目标.md\" "), a.editor.Model().Cursor())
		a.editor.Model().Undo()
		assert.Equal(t, text, a.editor.Text())
	}
}

func TestNativeArgumentCompletionFocusPrefersExactValue(t *testing.T) {
	a := newAppWithSelector(t)
	a.cmds.Register(Command{Name: "inspect", CompleteArgs: func(_ *App, _ string) []autocomplete.Item {
		return []autocomplete.Item{{Value: "argument", Label: "argument"}, {Value: "arg", Label: "arg"}}
	}})
	a.editor.Model().SetText("/inspect arg")
	a.refreshSelector(a.editor.Text(), stubCtx())
	require.True(t, a.selector.Shown())
	assert.Equal(t, 1, a.selector.Focused())
}

func TestNativeTabRequestsCompletionWithoutVisibleSelector(t *testing.T) {
	for _, names := range [][]string{{"note.md"}, {"note.md", "notes.md"}} {
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range names {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("body"), 0o600))
			}
			a := newAppWithSelector(t)
			a.sess = &fakeSession{stateCWD: dir}
			ctx := stubCtx()
			ctx.FocusedID = a.editor.ID()
			a.loopMu.Lock()
			a.loopCtx = ctx
			a.loopMu.Unlock()
			text := "前文\n./no 后文"
			a.editor.Model().SetText(text)
			a.editor.Model().SetCursor(len(text) - len(" 后文"))
			action, ok := (appKeymap{app: a}).Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyTab})
			require.True(t, ok)
			require.Equal(t, AppActionEditorComplete, action)
			require.True(t, (&Root{app: a}).HandleAction(action, ctx))
			if len(names) == 1 {
				assert.Equal(t, "前文\n./note.md 后文", a.editor.Text())
				assert.Equal(t, len("前文\n./note.md"), a.editor.Model().Cursor())
				assert.False(t, a.selectorVisible())
				a.editor.Model().Undo()
				assert.Equal(t, text, a.editor.Text())
			} else {
				assert.Equal(t, text, a.editor.Text())
				require.True(t, a.selector.Shown())
				require.Equal(t, selectorModeFiles, a.selectorMode)
				require.Len(t, a.selector.Items(), 2)
				require.True(t, (&Root{app: a}).HandleAction(action, ctx))
				assert.Equal(t, "前文\n./note.md 后文", a.editor.Text())
			}
		})
	}
}

func TestNativeTabRequestsSlashListInsteadOfFiles(t *testing.T) {
	a := newAppWithSelector(t)
	ctx := stubCtx()
	a.editor.Model().SetText("/hel")
	require.True(t, a.applySelectorFromAction(ctx))
	require.True(t, a.selector.Shown())
	assert.Equal(t, selectorModeCommands, a.selectorMode)
	assert.Equal(t, "/hel", a.editor.Text())
}

func TestNativeForcedAttachmentTabPreservesPasteAndDirectoryCursor(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "目标 文件夹"), 0o700))
	a := newAppWithSelector(t)
	a.sess = &fakeSession{stateCWD: dir}
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	m := a.editor.Model()
	body := strings.Repeat("paste body\n", 20)
	m.Paste(body)
	m.Insert("\n@\"目 后文")
	text := m.Text()
	m.SetCursor(len(text) - len(" 后文"))
	require.True(t, a.applySelectorFromAction(ctx))
	want := strings.Replace(text, "@\"目", "@\"目标 文件夹/\"", 1)
	assert.Equal(t, want, m.Text())
	assert.Equal(t, len(want)-len("\" 后文"), m.Cursor())
	assert.Equal(t, body+"\n@\"目标 文件夹/\" 后文", m.ExpandedText())
	m.Undo()
	assert.Equal(t, text, m.Text())
	assert.Equal(t, body+"\n@\"目 后文", m.ExpandedText())
}

func TestNativeTabOnlyOffersSlashCommandsOnFirstLine(t *testing.T) {
	for _, text := range []string{"前文\n/hel", "前文\n\ufeff/hel"} {
		t.Run(text, func(t *testing.T) {
			a := newAppWithSelector(t)
			a.editor.Model().SetText(text)
			cursor := a.editor.Model().Cursor()
			require.True(t, a.applySelectorFromAction(stubCtx()))
			assert.False(t, a.selector.Shown())
			assert.Equal(t, text, a.editor.Text())
			assert.Equal(t, cursor, a.editor.Model().Cursor())
		})
	}
}

func TestNativeAutomaticCommandCompletionStaysOnFirstLine(t *testing.T) {
	a := newAppWithSelector(t)
	calls := 0
	a.cmds.Register(Command{Name: "inspect", CompleteArgs: func(_ *App, _ string) []autocomplete.Item {
		calls++
		return []autocomplete.Item{{Value: "argument", Label: "argument"}}
	}})
	for _, text := range []string{"前文\n/hel", "前文\n  /hel", "前文\n/inspect arg", "前文\n\ufeff/inspect arg"} {
		t.Run(text, func(t *testing.T) {
			a.editor.Model().SetText(text)
			a.refreshSelector(text, stubCtx())
			assert.False(t, a.selector.Shown())
			assert.Equal(t, text, a.editor.Text())
			assert.Zero(t, calls)
		})
	}
}

func TestNativeCursorMovementRefreshesOpenRegularCompletion(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "note.md"), nil, 0o600))
	for _, text := range []string{"/inspect arg", "查看 @note"} {
		t.Run(text, func(t *testing.T) {
			a, err := New(Options{Session: &fakeSession{stateCWD: dir}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
			a.cmds.Register(Command{Name: "inspect", CompleteArgs: func(_ *App, _ string) []autocomplete.Item {
				return []autocomplete.Item{{Value: "argument", Label: "argument"}}
			}})
			ctx := stubCtx()
			ctx.FocusedID = a.editor.ID()
			a.editor.Model().SetText(text)
			a.refreshSelector(text, ctx)
			require.True(t, a.selector.Shown())
			require.True(t, a.editor.HandleAction(tauui.ActionHome, ctx))
			assert.False(t, a.selector.Shown())
			assert.Equal(t, text, a.editor.Text())
			assert.Zero(t, a.editor.Model().Cursor())
		})
	}
}

func TestNativeCursorMovementChangesArgumentPickerToCommands(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	a.cmds.Register(Command{Name: "inspect", CompleteArgs: func(_ *App, _ string) []autocomplete.Item {
		return []autocomplete.Item{{Value: "argument", Label: "argument"}}
	}})
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	a.editor.Model().SetText("/inspect ")
	a.refreshSelector(a.editor.Text(), ctx)
	require.True(t, a.selector.Shown())
	assert.Equal(t, "argument", a.selector.Items()[0].Name)
	require.True(t, a.editor.HandleAction(tauui.ActionMoveLeft, ctx))
	require.True(t, a.selector.Shown())
	assert.Equal(t, selectorModeCommands, a.selectorMode)
	assert.Equal(t, "inspect", a.selector.Items()[0].Name)
	assert.Equal(t, "/inspect ", a.editor.Text())
}

func TestNativeForcedCompletionRefreshDoesNotApplySingleResult(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"note.md", "notes.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	a, err := New(Options{Session: &fakeSession{stateCWD: dir}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	a.editor.Model().SetText("./no")
	require.True(t, a.requestTabCompletion(ctx))
	require.True(t, a.selector.Shown())
	require.Len(t, a.selector.Items(), 2)
	require.True(t, a.editor.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 't'}, ctx))
	require.True(t, a.selector.Shown())
	assert.Equal(t, "./not", a.editor.Text())
	require.True(t, a.editor.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'e'}, ctx))
	require.True(t, a.selector.Shown())
	require.True(t, a.editor.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: '.'}, ctx))
	require.True(t, a.selector.Shown())
	require.Len(t, a.selector.Items(), 1)
	assert.Equal(t, "./note.", a.editor.Text())
	assert.Equal(t, "./note.md", a.completionSelector.Items[0].Value)
	require.True(t, a.editor.HandleAction(tauui.ActionHome, ctx))
	require.True(t, a.selector.Shown())
	assert.Equal(t, "./note.", a.editor.Text())
	assert.Zero(t, a.editor.Model().Cursor())
	require.Len(t, a.selector.Items(), 2)
	require.True(t, a.editor.HandleAction(tauui.ActionEnd, ctx))
	require.True(t, a.selector.Shown())
	require.Len(t, a.selector.Items(), 1)
	require.True(t, a.editor.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'z'}, ctx))
	assert.False(t, a.selector.Shown())
	assert.Equal(t, "./note.z", a.editor.Text())
	assert.False(t, a.forcedCompletion)
}

func TestNativeArgumentCompletionRejectsChangedDraft(t *testing.T) {
	for _, mode := range []string{"text", "cursor"} {
		t.Run(mode, func(t *testing.T) {
			a := newAppWithSelector(t)
			a.editor.Model().SetText("/inspect arg")
			a.cmds.Register(Command{Name: "inspect", CompleteArgs: func(_ *App, _ string) []autocomplete.Item {
				if mode == "text" {
					a.editor.Model().SetText("replacement draft")
				} else {
					a.editor.Model().SetCursor(0)
				}
				return []autocomplete.Item{{Value: "stale", Label: "stale"}}
			}})
			a.refreshSelector(a.editor.Text(), stubCtx())
			assert.False(t, a.selector.Shown())
			if mode == "text" {
				assert.Equal(t, "replacement draft", a.editor.Text())
			} else {
				assert.Equal(t, "/inspect arg", a.editor.Text())
				assert.Zero(t, a.editor.Model().Cursor())
			}
		})
	}
}

func TestNativeArgumentCompletionKeepsNewerRequest(t *testing.T) {
	a := newAppWithSelector(t)
	a.editor.Model().SetText("/inspect arg")
	nested := false
	a.cmds.Register(Command{Name: "inspect", CompleteArgs: func(_ *App, _ string) []autocomplete.Item {
		if !nested {
			nested = true
			a.refreshSelector(a.editor.Text(), stubCtx())
			return []autocomplete.Item{{Value: "stale", Label: "stale"}}
		}
		return []autocomplete.Item{{Value: "latest", Label: "latest"}}
	}})
	a.refreshSelector(a.editor.Text(), stubCtx())
	require.True(t, a.selector.Shown())
	require.Len(t, a.selector.Items(), 1)
	assert.Equal(t, "latest", a.selector.Items()[0].Name)
}

func TestNativeCompletionRequestsOncePerEdit(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.chat.Close()) })
	calls := 0
	a.cmds.Register(Command{Name: "inspect", CompleteArgs: func(_ *App, _ string) []autocomplete.Item {
		calls++
		return []autocomplete.Item{{Value: "argument", Label: "argument"}}
	}})
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	a.editor.Model().SetText("/inspect a")
	a.refreshSelector(a.editor.Text(), ctx)
	require.Equal(t, 1, calls)
	require.True(t, a.editor.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'r'}, ctx))
	assert.Equal(t, 2, calls)
	assert.Equal(t, "/inspect ar", a.editor.Text())
}

func TestNativeAttachmentCompletionRequiresEditorTokenBoundary(t *testing.T) {
	for _, prefix := range []string{"=", "'", "\"", "foo(", "文字"} {
		t.Run(prefix, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "note.md"), nil, 0o600))
			a := newAppWithSelector(t)
			a.sess = &fakeSession{stateCWD: dir}
			a.editor.Model().SetText(prefix + "@no")
			assert.False(t, shouldCheckFileSelector(a.editor.Text(), a.editor.Model().Cursor()))
			a.refreshSelector(a.editor.Text(), stubCtx())
			assert.False(t, a.selector.Shown())
			assert.Equal(t, prefix+"@no", a.editor.Text())
		})
	}
}

func TestNativeArgumentRefreshClearsOldListAfterCallbackEdit(t *testing.T) {
	for _, mode := range []string{"text", "cursor"} {
		t.Run(mode, func(t *testing.T) {
			a := newAppWithSelector(t)
			m := a.editor.Model()
			m.SetText("/inspect arg")
			edit := false
			a.cmds.Register(Command{Name: "inspect", CompleteArgs: func(_ *App, _ string) []autocomplete.Item {
				if edit {
					if mode == "text" {
						m.SetText("replacement draft")
					} else {
						m.SetCursor(0)
					}
				}
				return []autocomplete.Item{{Value: "old", Label: "old"}}
			}})
			a.refreshSelector(m.Text(), stubCtx())
			require.True(t, a.selector.Shown())
			edit = true
			a.refreshSelector(m.Text(), stubCtx())
			assert.False(t, a.selector.Shown())
			if mode == "text" {
				assert.Equal(t, "replacement draft", m.Text())
			} else {
				assert.Zero(t, m.Cursor())
			}
		})
	}
}

func TestNativeSubmissionTrimsJavaScriptWhitespace(t *testing.T) {
	for _, draft := range []string{"\ufeff /capture value \ufeff", "\ufeff\u00a0"} {
		t.Run(draft, func(t *testing.T) {
			a := newTestApp(t)
			a.editor = newPromptEditorForTest()
			a.footer = newFooterForTest()
			ctx := testCtx()
			ctx.FocusedID = a.editor.ID()
			var handled []string
			a.cmds.Register(Command{Name: "capture", Handler: func(_ *App, args string) { handled = append(handled, args) }})
			a.editor.SetText(ctx, draft)
			require.True(t, a.trySubmit(ctx))
			assert.Empty(t, a.editor.Text())
			if strings.Contains(draft, "capture") {
				assert.Equal(t, []string{"value"}, handled)
				assert.Equal(t, []string{"/capture value"}, a.editor.Model().HistoryEntries())
			} else {
				assert.Empty(t, handled)
				assert.Empty(t, a.editor.Model().HistoryEntries())
			}
			a.editor.Model().Undo()
			assert.Empty(t, a.editor.Text())
		})
	}
	a := newTestApp(t)
	a.editor = newPromptEditorForTest()
	a.editor.Model().SetText("\u0085")
	assert.Equal(t, "\u0085", a.editorTextForSubmit())
}

type shutdownSession struct {
	*fakeSession
	closeSession func(context.Context) error
}

func (s *shutdownSession) Close(ctx context.Context) error { return s.closeSession(ctx) }

func TestRunRestoresTerminalBeforeClosingSessionWithLiveContext(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		be := headless.New(tauui.Size{W: 40, H: 12})
		type contextKey struct{}
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "kept"))
		closeErr := errors.New("session cleanup failed")
		closed := 0
		sess := &shutdownSession{fakeSession: &fakeSession{}, closeSession: func(cleanup context.Context) error {
			closed++
			assert.True(t, be.Restored(), "session shutdown ran while terminal was active")
			assert.NoError(t, cleanup.Err())
			assert.Equal(t, "kept", cleanup.Value(contextKey{}))
			_, bounded := cleanup.Deadline()
			assert.True(t, bounded)
			return closeErr
		}}
		app, err := New(Options{Session: sess})
		require.NoError(t, err)
		app.app, err = tauui.New(tauui.Opts{Root: app.root, Backend: be, OnTerminalReply: func(tauui.TerminalReplyEvent) {
			if cancelParent {
				cancel()
			} else {
				app.quit()
			}
		}})
		require.NoError(t, err)
		be.SendEvent(tauui.TerminalReplyEvent{Sequence: "done"})
		err = app.Run(ctx)
		cancel()
		assert.ErrorIs(t, err, closeErr)
		assert.Equal(t, 1, closed)
		assert.True(t, be.Restored())
	}
}

func TestRunQuitRemovesUnsentSessionImages(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "session-data")
	app, err := New(Options{Session: &fakeSession{stateDataDir: dataDir}})
	require.NoError(t, err)
	be := headless.New(tauui.Size{W: 40, H: 12})
	app.app, err = tauui.New(tauui.Opts{Root: app.root, Backend: be, OnTerminalReply: func(tauui.TerminalReplyEvent) { app.quit() }})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "pasted-image.png"), []byte{1}, 0o600))
	be.SendEvent(tauui.TerminalReplyEvent{Sequence: "done"})
	require.NoError(t, app.Run(context.Background()))
	_, err = os.Stat(dataDir)
	assert.True(t, os.IsNotExist(err), "unsent session data remains: %v", err)
}
