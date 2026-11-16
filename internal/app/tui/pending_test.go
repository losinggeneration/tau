package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tauui "github.com/losinggeneration/tau/tui"

	sdkpkg "github.com/losinggeneration/tau/sdk"
)

// TestPendingView_CollapsesWhenEmpty checks that an empty pending
// view reports MinSize 0x0 and Paint is a no-op (the VStack should
// treat the region as absent).
func TestPendingView_CollapsesWhenEmpty(t *testing.T) {
	p := NewPendingView()
	if sz := p.MinSize(); sz.W != 0 || sz.H != 0 {
		t.Errorf("empty pending view should have zero MinSize, got %+v", sz)
	}
}

// TestPendingView_GrowsWhenPopulated checks that setting items gives
// the view a real size and that Paint emits a header + bullets.
func TestPendingView_GrowsWhenPopulated(t *testing.T) {
	p := NewPendingView()
	p.SetItems([]string{"first", "second", "third"})
	sz := p.MinSize()
	if sz.W == 0 || sz.H == 0 {
		t.Errorf("populated pending view should have non-zero MinSize, got %+v", sz)
	}
	if sz.H < 2 {
		t.Errorf("populated pending view should be at least 2 rows (header + 1 item), got %d", sz.H)
	}
	// Cap at maxRows (6): spacer, three items, edit hint.
	if sz.H > 6 {
		t.Errorf("populated pending view should not exceed maxRows=6, got %d", sz.H)
	}
}

// TestPendingView_OverflowMarker ensures the view's height is capped
// at maxRows even with many items (so a 7-item queue doesn't dominate
// the screen). The "… (N more)" marker is rendered inside the
// existing cap, replacing the last item slot.
func TestPendingView_OverflowMarker(t *testing.T) {
	p := NewPendingView()
	p.SetItems([]string{"a", "b", "c", "d", "e", "f", "g"})
	sz := p.MinSize()
	if sz.H != 6 {
		t.Errorf("overflow view should be capped at maxRows=6, got %d", sz.H)
	}
}

// TestPendingView_TruncatesMultiline ensures a queued message with
// embedded newlines is rendered as a single bullet row showing only
// the first line.
func TestPendingView_TruncatesMultiline(t *testing.T) {
	view := NewPendingView()
	view.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 4})
	view.SetItems([]string{"line1\nline2\nline3"})

	if got := firstLineOf("line1\nline2\nline3"); got != "line1" {
		t.Errorf("firstLineOf should return only the first line, got %q", got)
	}
	if got := firstLineOf("trailing whitespace   \nnext"); got != "trailing whitespace" {
		t.Errorf("firstLineOf should trim trailing whitespace, got %q", got)
	}
	if got := firstLineOf("no-newline"); got != "no-newline" {
		t.Errorf("firstLineOf should pass through single-line input, got %q", got)
	}
}

// TestPendingView_DetectsItemChanges ensures that adding/removing
// items causes MinSize to change so the VStack picks up the size
// delta. This is the contract the App relies on for layout
// invalidation in refreshPending.
func TestPendingView_DetectsItemChanges(t *testing.T) {
	p := NewPendingView()
	if p.MinSize() != (tauui.Size{}) {
		t.Errorf("empty view should report zero MinSize")
	}
	p.SetItems([]string{"x"})
	if p.MinSize().H == 0 {
		t.Errorf("after SetItems, MinSize.H should be non-zero")
	}
	p.SetItems(nil)
	if p.MinSize() != (tauui.Size{}) {
		t.Errorf("after SetItems(nil), MinSize should collapse to zero")
	}
}

func TestRoutePromptWhileStreamingSteersCurrentTask(t *testing.T) {
	a := newTestApp(t)
	fs := a.sess.(*fakeSession)
	a.setStreaming(true)

	a.routePrompt("guide the in-flight task", testCtx())

	var steers []string
	for i := 0; i < 100; i++ {
		fs.mu.Lock()
		steers = append([]string(nil), fs.steers...)
		followUps := append([]string(nil), fs.followUps...)
		fs.mu.Unlock()
		if len(steers) > 0 {
			if steers[0] != "guide the in-flight task" {
				t.Fatalf("expected session.steers=[guide the in-flight task], got %v", steers)
			}
			if len(followUps) != 0 {
				t.Fatalf("plain Enter while streaming must not queue a completion-only follow-up, got %v", followUps)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("expected session.steers=[guide the in-flight task], got %v", steers)
}

func TestRoutePromptWhileStreamingShowsSteeringPending(t *testing.T) {
	a := newTestApp(t)
	a.pending = NewPendingView()
	a.setStreaming(true)

	a.routePrompt("guide the in-flight task", testCtx())

	rows := a.pending.rows(80)
	if len(rows) == 0 {
		t.Fatal("pending view should show queued steering")
	}
	want := " Steering: guide the in-flight task"
	found := false
	for _, row := range rows {
		if row == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("pending rows missing %q: %#v", want, rows)
	}
}

func TestSubmitWhileStreamingMirrorsSteeringInSubmitContext(t *testing.T) {
	a := newTestApp(t)
	a.editor = newPromptEditorForTest()
	a.pending = NewPendingView()
	a.setStreaming(true)
	ctx := testCtx()
	ctx.FocusedID = a.editor.ID()

	a.editor.SetText(ctx, "first steering message")
	if !a.trySubmit(ctx) {
		t.Fatal("trySubmit should consume focused editor submit")
	}

	if got := a.editor.Text(); got != "" {
		t.Fatalf("editor text = %q, want cleared", got)
	}
	assertPendingRow(t, a.pending.rows(80), " Steering: first steering message")
}

func assertPendingRow(t *testing.T, rows []string, want string) {
	t.Helper()
	for _, row := range rows {
		if row == want {
			return
		}
	}
	t.Fatalf("pending rows missing %q: %#v", want, rows)
}

func TestFollowUpActionWhileStreamingQueuesAndClearsEditor(t *testing.T) {
	a := newTestApp(t)
	a.editor = newPromptEditorForTest()
	a.pending = NewPendingView()
	a.setStreaming(true)
	ctx := testCtx()
	a.editor.SetText(ctx, "summarize after finishing")

	a.handleFollowUpAction(ctx)

	fs := a.sess.(*fakeSession)
	if len(fs.followUps) != 1 || fs.followUps[0] != "summarize after finishing" {
		t.Fatalf("expected one explicit follow-up, got %v", fs.followUps)
	}
	if got := a.editor.Text(); got != "" {
		t.Fatalf("editor text = %q, want cleared after queueing", got)
	}
}

func TestFollowUpActionWhileIdleSubmitsImmediately(t *testing.T) {
	a := newTestApp(t)
	a.editor = newPromptEditorForTest()
	a.footer = newFooterForTest()
	a.pending = NewPendingView()
	ctx, cancel := context.WithCancel(context.Background())
	a.ctx = ctx
	t.Cleanup(cancel)
	tuiCtx := testCtx()
	tuiCtx.FocusedID = a.editor.ID()
	a.editor.SetText(tuiCtx, "start now")

	a.handleFollowUpAction(tuiCtx)

	fs := a.sess.(*fakeSession)
	for i := 0; i < 100; i++ {
		fs.mu.Lock()
		sent := append([]string(nil), fs.sentTexts...)
		queued := append([]string(nil), fs.followUps...)
		fs.mu.Unlock()
		if len(sent) > 0 {
			if sent[0] != "start now" {
				t.Fatalf("sent text = %q, want start now", sent[0])
			}
			if len(queued) != 0 {
				t.Fatalf("idle follow-up action must not leave a dormant follow-up, got %v", queued)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("idle follow-up action did not start a prompt")
}

// TestEnqueueFollowUp_RecordsOnSession is the integration test for
// the explicit follow-up queue wiring: enqueueFollowUp sends the right
// payload, grows the App mirror, and refreshes the pending view.
func TestEnqueueFollowUp_RecordsOnSession(t *testing.T) {
	a := newTestApp(t)
	fs := a.sess.(*fakeSession)

	// Stand in a minimal tauui.Ctx so refreshPending doesn't nil-deref.
	a.pending = NewPendingView()
	a.pendingQueue = nil

	a.enqueueFollowUp("first follow-up", testCtx())

	if len(fs.followUps) != 1 || fs.followUps[0] != "first follow-up" {
		t.Errorf("expected session.followUps=[first follow-up], got %v", fs.followUps)
	}
	if got := a.pendingQueue; len(got) != 1 || got[0] != "first follow-up" {
		t.Errorf("expected pendingQueue=[first follow-up], got %v", got)
	}
	if a.pending.MinSize().H == 0 {
		t.Errorf("pending view should claim layout space after enqueue, got MinSize=%+v", a.pending.MinSize())
	}

	// A second enqueue appends to the mirror and the session.
	a.enqueueFollowUp("second follow-up", testCtx())
	if len(fs.followUps) != 2 {
		t.Errorf("expected session.followUps len=2, got %d", len(fs.followUps))
	}
	if got := a.pendingQueue; len(got) != 2 {
		t.Errorf("expected pendingQueue len=2, got %d", len(got))
	}
}

// TestEnqueueFollowUp_NoFollowUpper covers the no-implementation
// branch. We swap the App's session for a struct that satisfies
// sdk.Session but does NOT have a FollowUp method (so it doesn't
// satisfy sdk.FollowUpper), then verify the chat log gets an error
// and the queue stays empty.
//
// Building a session that satisfies sdk.Session from scratch is a
// lot of boilerplate, so we use a different strategy: define a
// local type alias for *fakeSession that explicitly does not
// implement FollowUpper. In Go this is impossible with embedding
// (the embedded method is promoted), so we use a thin wrapper
// struct that holds a *fakeSession and forwards the Session
// methods explicitly, omitting FollowUp. This satisfies the
// compiler because sdk.Session doesn't require FollowUp.
func TestEnqueueFollowUp_NoFollowUpper(t *testing.T) {
	a := newTestApp(t)
	fs := &fakeSession{}
	a.sess = &sessionNoFollowUp{inner: fs}
	a.pending = NewPendingView()

	a.enqueueFollowUp("queued text", testCtx())

	if len(a.pendingQueue) != 0 {
		t.Errorf("pendingQueue should stay empty when session lacks FollowUpper, got %v", a.pendingQueue)
	}
	if a.pending.MinSize().H != 0 {
		t.Errorf("pending view should not claim space when queue stays empty, got MinSize=%+v", a.pending.MinSize())
	}
	// The error message should be present in the chat log.
	found := false
	for _, m := range a.conv.Messages() {
		if m.Role == RoleError {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected an error message in the chat log when FollowUpper is missing, got %d messages", len(a.conv.Messages()))
	}
}

// Dequeue takes every queued message out of the session and puts it back in
// the editor ahead of the current draft, so queued work can be edited or
// held back instead of only dropping a row from the display.
func TestRestoreQueuedMessagesMovesQueueIntoEditor(t *testing.T) {
	a := newTestApp(t)
	fs := &fakeSession{steers: []string{"steer one"}, followUps: []string{"follow one", "follow two"}}
	a.sess = fs
	a.pending = NewPendingView()
	a.editor = newPromptEditorForTest()
	ctx := testCtx()
	a.editor.SetText(ctx, "draft")
	a.steeringQueue = []string{"steer one"}
	a.pendingQueue = []string{"follow one", "follow two"}
	a.refreshPending(ctx)
	if rows := a.pending.rows(80); !strings.Contains(rows[len(rows)-1], "alt+up to edit all queued messages") {
		t.Fatalf("pending hint = %q, want the bound dequeue key", rows[len(rows)-1])
	}

	a.restoreQueuedMessages(ctx)

	if got, want := a.editor.Text(), "steer one\n\nfollow one\n\nfollow two\n\ndraft"; got != want {
		t.Errorf("editor = %q, want %q", got, want)
	}
	if steering, followUp := fs.QueueSizes(); steering+followUp != 0 {
		t.Errorf("session queue = %d steering, %d follow-up; want empty", steering, followUp)
	}
	if len(a.steeringQueue)+len(a.pendingQueue) != 0 || a.pending.MinSize().H != 0 {
		t.Errorf("pending mirror not cleared: %v %v", a.steeringQueue, a.pendingQueue)
	}
}

func TestRestoreQueuedMessagesEmptyQueueSaysSo(t *testing.T) {
	a := newTestApp(t)
	a.pending = NewPendingView()
	a.editor = newPromptEditorForTest()

	a.restoreQueuedMessages(testCtx())

	msgs := a.conv.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "No queued messages") {
		t.Errorf("messages = %+v, want a no-queued-messages notice", msgs)
	}
	if a.editor.Text() != "" {
		t.Errorf("editor = %q, want unchanged", a.editor.Text())
	}
}

func TestSyncPendingFromSessionMirrorsSteeringAndFollowUpCounts(t *testing.T) {
	a := newTestApp(t)
	a.pending = NewPendingView()
	fs := a.sess.(*fakeSession)
	fs.steeringOverride = 2
	fs.queueOverride = 1

	a.syncPendingFromSession(nil)

	rows := a.pending.rows(80)
	want := []string{
		" Steering: queued message",
		" Steering: queued message",
		" Follow-up: queued message",
	}
	for _, expected := range want {
		found := false
		for _, row := range rows {
			if row == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("pending rows missing %q: %#v", expected, rows)
		}
	}
}

// TestRefreshPending_HandlesNilContext verifies refreshPending
// tolerates a nil tauui.Ctx (the no-paint-context path used by tests
// and during early construction).
func TestRefreshPending_HandlesNilContext(t *testing.T) {
	a := newTestApp(t)
	a.pending = NewPendingView()
	a.pendingQueue = []string{"x"}

	// Should not panic on a nil ctx.
	a.refreshPending(nil)

	if a.pending.MinSize().H == 0 {
		t.Errorf("pending view should still claim space after refresh, got %+v", a.pending.MinSize())
	}
}

// sessionNoFollowUp is a thin wrapper around *fakeSession that
// satisfies sdk.Session but explicitly does NOT have a FollowUp
// method (so the type assertion `a.sess.(sdk.FollowUpper)` in
// enqueueFollowUp fails). The forwarded methods are the minimum
// the test path exercises: State and SetModel. The unused
// parameters are zero-valued rather than forwarded because the
// test doesn't read them; if a future test needs to assert on
// them, swap the body to delegate to inner.
type sessionNoFollowUp struct {
	inner *fakeSession
}

func (s *sessionNoFollowUp) Send(_ context.Context, _ sdkpkg.Message) (<-chan sdkpkg.Event, error) {
	return nil, nil
}

func (s *sessionNoFollowUp) SendText(_ context.Context, _ string) (<-chan sdkpkg.Event, error) {
	return nil, nil
}
func (s *sessionNoFollowUp) Interrupt() error                             { return nil }
func (s *sessionNoFollowUp) Compact(_ context.Context, _ ...string) error { return nil }
func (s *sessionNoFollowUp) State() sdkpkg.SessionState                   { return s.inner.State() }
func (s *sessionNoFollowUp) ID() string                                   { return s.inner.ID() }
func (s *sessionNoFollowUp) IsPersisted() bool                            { return false }
func (s *sessionNoFollowUp) Close(_ context.Context) error                { return nil }
func (s *sessionNoFollowUp) SetThinkingLevel(_ string) error              { return nil }
func (s *sessionNoFollowUp) SetModel(_ string) error                      { return nil }
func (s *sessionNoFollowUp) SlashCommands() []sdkpkg.SlashCommandInfo {
	return nil
}
