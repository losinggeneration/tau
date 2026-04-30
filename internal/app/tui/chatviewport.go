package tui

import (
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/widgets"
)

// chatScrollViewport separates the viewport's minimum allocation from the
// ChatLog's full content height. ScrollView.MinSize normally forwards its
// child's height, which makes VStack treat a long transcript as non-shrinkable
// and consume the rows reserved for the prompt and footer. The embedded
// ScrollView still reads ChatLog.MinSize directly for overflow, scrollbar, and
// scroll-range calculations; only the parent-facing minimum is bounded.
type chatScrollViewport struct {
	*widgets.ScrollView
	chat *ChatLog

	// onUserScroll, when non-nil, is invoked after a user-driven event (wheel,
	// scroll keys, scrollbar drag) moves the viewport, reporting whether the
	// viewport came to rest on the end of the scroll buffer. The App uses that
	// to suspend follow-the-tail autoscroll while the user reads away from the
	// end and resume it when they return. Programmatic scrolls never pass
	// through this wrapper's handlers, so they cannot be mistaken for user
	// intent.
	onUserScroll func(atBottom bool)
}

func (v *chatScrollViewport) MinSize() tauui.Size {
	size := v.ScrollView.MinSize()
	size.H = 1
	return size
}

func (v *chatScrollViewport) Layout(r tauui.Rect) {
	if v != nil && v.chat != nil {
		start := 0
		if v.ScrollView != nil {
			start = v.ScrollPos()
		}
		v.chat.setVisibleRows(start, start+r.H)
	}
	v.ScrollView.Layout(r)
}

// Paint tells ChatLog which content rows are actually visible before delegating
// to ScrollView. The toolkit ScrollView offsets and clips its child, but a
// child Paint still has to iterate all of its own rows. Once the transcript is
// taller than the viewport that makes each frame proportional to total history
// length. Supplying the visible row window keeps repaint cost bounded by the
// terminal height while preserving ScrollView's existing scrollbar and input
// behavior.
func (v *chatScrollViewport) Paint(d tauui.Drawer, ctx *tauui.Ctx) {
	if v == nil || v.ScrollView == nil {
		return
	}
	if v.chat == nil {
		v.ScrollView.Paint(d, ctx)
		return
	}

	rect := v.Rect()
	start := v.ScrollPos()
	end := start + rect.H
	v.chat.setVisibleRows(start, end)
	defer v.chat.setVisibleRows(0, 0)

	v.ScrollView.Paint(d, ctx)
}

// Handle observes user-driven scroll changes before delegating to ScrollView.
// Wheel events, arrow/page/home/end keys, and scrollbar drags all move the
// position from inside ScrollView.Handle, so diffing the position across the
// delegation is the single place that catches every user scroll source. The
// App installs onUserScroll to suspend autoscroll while the user reads away
// from the end of the buffer and resume it when they scroll back down.
func (v *chatScrollViewport) Handle(e tauui.Event, ctx *tauui.Ctx) bool {
	if v == nil || v.ScrollView == nil {
		return false
	}
	before := v.ScrollPos()
	handled := v.ScrollView.Handle(e, ctx)
	v.notifyUserScroll(before)
	return handled
}

// HandleAction mirrors Handle for semantic actions (MoveUp/MoveDown/PageUp/
// PageDown/Home/End), which the toolkit dispatches to the focused view while
// the chat viewport holds focus.
func (v *chatScrollViewport) HandleAction(act tauui.Action, ctx *tauui.Ctx) bool {
	if v == nil || v.ScrollView == nil {
		return false
	}
	before := v.ScrollPos()
	handled := v.ScrollView.HandleAction(act, ctx)
	v.notifyUserScroll(before)
	return handled
}

// notifyUserScroll reports the viewport's resting position to the App when a
// user-driven event moved it. A no-op when nothing moved, so scrolling against
// an edge (wheel down while already at the end) leaves the autoscroll state
// untouched.
func (v *chatScrollViewport) notifyUserScroll(before int) {
	if after := v.ScrollPos(); after != before && v.onUserScroll != nil {
		v.onUserScroll(v.atBottom())
	}
}

// atBottom reports whether the viewport rests on the end of the scroll buffer.
// ScrollView clamps its offset to contentHeight-viewportHeight, so reaching
// that bound — or a buffer no taller than the viewport — is "at the bottom",
// the state that resumes autoscroll.
func (v *chatScrollViewport) atBottom() bool {
	if v == nil || v.ScrollView == nil || v.chat == nil {
		return true
	}
	rect := v.Rect()
	if rect.H <= 0 {
		return true
	}
	if maxScroll := v.chat.MinSize().H - rect.H; maxScroll > 0 {
		return v.ScrollPos() >= maxScroll
	}
	return true
}

// chatUserScrolled is the App's onUserScroll observer. Scrolling away from the
// end of the buffer suspends follow-the-tail autoscroll; arriving back at the
// end resumes it. This is runtime UI state, not configuration: the user's
// scroll position is the only authority on whether streaming messages should
// keep pulling the viewport to the bottom. Runs on the app loop, from
// ScrollView event dispatch.
func (a *App) chatUserScrolled(atBottom bool) {
	if a == nil {
		return
	}
	a.setAutoScrollOff(!atBottom)
}
