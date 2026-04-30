package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/ai"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/widgets"
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
	a.cycleThinkingLevel() // xhigh -> off (wrap)
	if got := a.currentThinkingLevel(); got != "off" {
		t.Fatalf("cycling past the top gave %q, want off", got)
	}

	a.toggleThinkingLevel()
	if got := a.currentThinkingLevel(); got != "xhigh" {
		t.Errorf("toggle gave %q, want the cycled-to xhigh rather than the default", got)
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

// The toast text format is the user-facing contract; lock it so a
// localisation or refactor cannot silently change "New messages (N)".
func TestNewMessagesToastText(t *testing.T) {
	a := newToastFixture(t)
	a.pendingNewMessages = 1
	if got := a.newMessagesToastText(); got != "New messages (1)" {
		t.Errorf("text at count=1 = %q, want %q", got, "New messages (1)")
	}
	a.pendingNewMessages = 42
	if got := a.newMessagesToastText(); got != "New messages (42)" {
		t.Errorf("text at count=42 = %q, want %q", got, "New messages (42)")
	}
}

// With the editor pinned to a known row, the toast should sit one row above
// it, centred horizontally across the root, and exactly the text's width
// tall (one row).
func TestNewMessagesToastRectCentredAboveEditor(t *testing.T) {
	a := newToastFixture(t)
	a.editor.Layout(tauui.Rect{X: 0, Y: 10, W: 80, H: 1})
	a.pendingNewMessages = 1

	rect := a.newMessagesToastRect()
	text := a.newMessagesToastText()
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

// The toast must not paint (and the rect must be zero) when the counter is
// zero — there is nothing to advertise.
func TestNewMessagesToastRectHiddenWhenCountZero(t *testing.T) {
	a := newToastFixture(t)
	a.editor.Layout(tauui.Rect{X: 0, Y: 10, W: 80, H: 1})

	if rect := a.newMessagesToastRect(); rect != (tauui.Rect{}) {
		t.Errorf("rect = %+v with zero count, want zero rect", rect)
	}
}

// A very narrow terminal must clamp the toast width so the centred x never
// goes negative.
func TestNewMessagesToastRectClampsToTerminalWidth(t *testing.T) {
	a := newToastFixture(t)
	a.editor.Layout(tauui.Rect{X: 0, Y: 5, W: 5, H: 1})
	// Re-layout the Root at a narrow width.
	rootRect := tauui.Rect{X: 0, Y: 0, W: 10, H: 20}
	a.root.AppLayout.Layout(rootRect)
	a.pendingNewMessages = 1

	rect := a.newMessagesToastRect()
	if rect.W > rootRect.W {
		t.Errorf("rect.W = %d exceeds root.W = %d", rect.W, rootRect.W)
	}
	if rect.X < rootRect.X || rect.X+rect.W > rootRect.X+rootRect.W {
		t.Errorf("rect = %+v not contained in root = %+v", rect, rootRect)
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
