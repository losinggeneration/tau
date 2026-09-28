package tui

import (
	"strings"
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/widgets"
)

// newAutoscrollFixture builds the fixed-shell chat stack the way New does: a
// ChatLog inside a ScrollView wrapped by the chatScrollViewport, with the
// App wired to the viewport's user-scroll observer. The returned App is
// minimal (no session, no editor), which is enough to drive
// handleChatRenderUpdate the way the render-ready callback does.
func newAutoscrollFixture(t *testing.T) (*App, *chatScrollViewport, *ChatLog, *ConversationModel) {
	t.Helper()

	conv := NewConversationModel()
	for i := range 40 {
		conv.AddAssistantMessage(strings.Repeat("filler ", 8) + strings.Repeat("n", i))
	}
	chat := newTestChatLog(t, conv)

	scroll := widgets.NewScrollView(widgets.ScrollViewOpts{
		Child:     chat,
		Focusable: true,
		Scrollbar: widgets.ScrollbarHidden,
	})
	viewport := &chatScrollViewport{ScrollView: scroll, chat: chat}
	a := &App{scroll: scroll, chat: chat, conv: conv}
	viewport.onUserScroll = a.chatUserScrolled

	rect := tauui.Rect{X: 0, Y: 0, W: 40, H: 10}
	viewport.Layout(rect)
	waitForChatLog(t, chat)
	// Re-layout once the completed height is known, matching the app's
	// render-ready layout invalidation pass.
	viewport.Layout(rect)

	if max := chat.MinSize().H - rect.H; max <= 0 {
		t.Fatalf("fixture is not scrollable: content=%d viewport=%d", chat.MinSize().H, rect.H)
	}
	return a, viewport, chat, conv
}

func wheelEvent(button tauui.MouseButton) tauui.MouseEvent {
	return tauui.MouseEvent{Button: button, Action: tauui.MousePress, X: 1, Y: 1}
}

// deliverCurrentRender simulates the render-ready callback for a completed,
// current render: follow intent is armed by mutations (refreshChat) and
// consumed here against the viewport's real geometry.
func deliverCurrentRender(t *testing.T, a *App, viewport *chatScrollViewport) {
	t.Helper()
	a.refreshChat()
	a.handleChatRenderUpdate(chatLogRenderUpdate{
		installed: true,
		current:   true,
		rect:      viewport.Rect(),
	}, func(tauui.Rect) {}, nil)
}

// A user scrolling away from the end of the buffer must stop follow-the-tail:
// streaming renders keep arriving, and none of them may pull the viewport to
// the bottom. Scrolling back to the end resumes the follow.
func TestUserScrollSuspendsAutoscrollUntilBottom(t *testing.T) {
	a, viewport, chat, conv := newAutoscrollFixture(t)
	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}

	deliverCurrentRender(t, a, viewport)
	if !viewport.atBottom() {
		t.Fatal("autoscroll did not pin the viewport to the end of the buffer")
	}

	if !viewport.Handle(wheelEvent(tauui.MouseButtonWheelUp), ctx) {
		t.Fatal("wheel up was not handled by the chat viewport")
	}
	if !a.autoScrollOff {
		t.Fatal("scrolling away from the end did not suspend autoscroll")
	}

	// Stream more content while the user reads. Renders must not move the
	// viewport, not even the ones that catch up to the newest revision.
	pinned := viewport.ScrollPos()
	conv.AddAssistantMessage(strings.Repeat("streamed line\n", 12))
	waitForChatLog(t, chat)
	viewport.Layout(viewport.Rect())
	deliverCurrentRender(t, a, viewport)
	if got := viewport.ScrollPos(); got != pinned {
		t.Fatalf("current render moved the viewport from %d to %d while autoscroll was suspended", pinned, got)
	}

	// Scrolling back down to the end resumes the follow...
	for !viewport.atBottom() {
		viewport.Handle(wheelEvent(tauui.MouseButtonWheelDown), ctx)
	}
	if a.autoScrollOff {
		t.Fatal("returning to the end of the buffer did not resume autoscroll")
	}

	// ...so the next render pins the view to the new content.
	conv.AddAssistantMessage(strings.Repeat("tail line\n", 20))
	waitForChatLog(t, chat)
	viewport.Layout(viewport.Rect())
	deliverCurrentRender(t, a, viewport)
	if !viewport.atBottom() {
		t.Fatalf("autoscroll did not resume following after the user returned to the end: pos=%d", viewport.ScrollPos())
	}
}

// Replacing the transcript wholesale (new session, resume, branch switch)
// drops whatever the user was scrolled to along with the old content, so the
// runtime state resumes following for the new transcript.
func TestTranscriptReplacementResumesAutoscroll(t *testing.T) {
	a, viewport, _, _ := newAutoscrollFixture(t)
	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}

	viewport.ScrollBottom(ctx)
	viewport.Handle(wheelEvent(tauui.MouseButtonWheelUp), ctx)
	if !a.autoScrollOff {
		t.Fatal("setup: user scroll did not suspend autoscroll")
	}

	a.replaceConversation(NewConversationModel())
	if a.autoScrollOff {
		t.Fatal("transcript replacement did not resume autoscroll")
	}
}

// Keyboard scroll actions on the focused viewport follow the same rule as the
// wheel: moving away suspends, landing on the end resumes.
func TestUserScrollActionsSuspendAndResumeAutoscroll(t *testing.T) {
	a, viewport, _, _ := newAutoscrollFixture(t)
	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}
	viewport.ScrollBottom(ctx)

	if !viewport.HandleAction(tauui.ActionMoveUp, ctx) {
		t.Fatal("MoveUp action was not handled by the chat viewport")
	}
	if !a.autoScrollOff {
		t.Fatal("MoveUp away from the end did not suspend autoscroll")
	}
	if viewport.HandleAction(tauui.ActionEnd, ctx) {
		if a.autoScrollOff {
			t.Fatal("End action back to the buffer's end did not resume autoscroll")
		}
	} else {
		t.Fatal("End action was not handled by the chat viewport")
	}
}

// Only user input flips the autoscroll state. Programmatic scrolls drive the
// embedded ScrollView directly, so a follow (or any other app-driven anchor)
// cannot be mistaken for the user changing their mind.
func TestProgrammaticScrollDoesNotChangeAutoscrollState(t *testing.T) {
	a, viewport, _, _ := newAutoscrollFixture(t)
	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}

	if a.autoScrollOff {
		t.Fatal("autoscroll starts suspended")
	}
	// Start from the end of the buffer, where a wheel up actually moves.
	viewport.ScrollBottom(ctx)
	viewport.Handle(wheelEvent(tauui.MouseButtonWheelUp), ctx)
	if !a.autoScrollOff {
		t.Fatal("user scroll did not suspend autoscroll")
	}

	// An app-driven scroll (layout switch, transcript reset) moves the
	// position without going through the wrapper's handlers, so the
	// suspension survives it. Only the paths that explicitly re-anchor
	// clear the state themselves.
	viewport.ScrollBottom(ctx)
	if !a.autoScrollOff {
		t.Fatal("programmatic scroll cleared the autoscroll suspension")
	}

	// A non-scroll mouse event (a selection click) moves nothing, so the
	// state must not change there either.
	a.autoScrollOff = false
	viewport.Handle(tauui.MouseEvent{Button: tauui.MouseButtonLeft, Action: tauui.MousePress, X: 1, Y: 1}, ctx)
	if a.autoScrollOff {
		t.Fatal("non-scroll mouse event suspended autoscroll")
	}
}

// jumpToChatBottom backs the status line and app.chat.scrollBottom (Ctrl+End):
// it must land the viewport on the end of the buffer and re-arm
// follow-the-tail, including dropping the new-message count the status line
// was advertising.
func TestJumpToChatBottomScrollsAndResumes(t *testing.T) {
	a, viewport, _, _ := newAutoscrollFixture(t)
	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}

	viewport.ScrollBottom(ctx)
	viewport.Handle(wheelEvent(tauui.MouseButtonWheelUp), ctx)
	if !a.autoScrollOff {
		t.Fatal("setup: user scroll did not suspend autoscroll")
	}
	a.noteNewMessage()
	a.noteNewMessage()
	if a.pendingNewMessages != 2 {
		t.Fatalf("setup: pendingNewMessages = %d, want 2", a.pendingNewMessages)
	}

	a.jumpToChatBottom(ctx)

	if !viewport.atBottom() {
		t.Errorf("jump left the viewport at %d, want the end of the buffer", viewport.ScrollPos())
	}
	if a.autoScrollOff {
		t.Error("jump did not resume autoscroll")
	}
	if a.pendingNewMessages != 0 {
		t.Errorf("pendingNewMessages = %d after the jump, want 0", a.pendingNewMessages)
	}
}

// In the document layout the document itself owns the scroll position, so the
// same action must re-pin the document to its bottom. A viewport shorter than
// the editor region alone guarantees the document is scrollable without
// waiting on async chat renders.
func TestJumpToChatBottomDocumentLayout(t *testing.T) {
	a := newToastFixture(t)
	a.SetLayoutMode(LayoutDocument)
	doc := a.root.document
	doc.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 5})
	doc.SetScrollBack(3)
	if doc.AtBottom() {
		t.Fatal("setup: document fixture is not scrollable")
	}

	a.jumpToChatBottom(testCtx())

	if !doc.AtBottom() {
		t.Errorf("jump left the document at scrollBack %d, want 0", doc.ScrollBack())
	}
}
