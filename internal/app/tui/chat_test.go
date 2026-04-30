package tui

import (
	"strings"
	"testing"

	"github.com/losinggeneration/rovel/ui/widgets"
	tauui "github.com/losinggeneration/tau/tui"
)

// The chat log is a full-width vertical flow. A scrollbar gutter would both
// violate that and — because it only appears once content overflows — change
// the chat's wrap width as the transcript crosses one screen, re-wrapping the
// whole history and flushing the width-keyed render cache.
func TestChatViewportKeepsFullWidthWhenContentOverflows(t *testing.T) {
	const width = 60

	conv := NewConversationModel()
	conv.AddUserMessage("short")
	chat := newTestChatLog(t, conv)

	scroll := widgets.NewScrollView(widgets.ScrollViewOpts{
		Child:     chat,
		Focusable: true,
		Scrollbar: widgets.ScrollbarHidden,
	})
	viewport := &chatScrollViewport{ScrollView: scroll, chat: chat}

	rect := tauui.Rect{X: 0, Y: 0, W: width, H: 10}
	viewport.Layout(rect)
	waitForChatLog(t, chat)
	fits := chat.Rect().W

	// Overflow the viewport many times over.
	for range 50 {
		conv.AddAssistantMessage(strings.Repeat("filler ", 12))
	}
	viewport.Layout(rect)
	waitForChatLog(t, chat)
	overflows := chat.Rect().W

	if fits != width {
		t.Fatalf("chat width = %d with content that fits, want the full %d", fits, width)
	}
	if overflows != width {
		t.Fatalf("chat width = %d once content overflows, want the full %d — a gutter was reserved", overflows, width)
	}
}

// Removing the bar must not remove the scrolling.
func TestChatViewportStillScrollsWithoutAScrollbar(t *testing.T) {
	conv := NewConversationModel()
	for range 50 {
		conv.AddAssistantMessage(strings.Repeat("filler ", 12))
	}
	chat := newTestChatLog(t, conv)

	scroll := widgets.NewScrollView(widgets.ScrollViewOpts{
		Child:     chat,
		Focusable: true,
		Scrollbar: widgets.ScrollbarHidden,
	})
	viewport := &chatScrollViewport{ScrollView: scroll, chat: chat}
	viewport.Layout(tauui.Rect{X: 0, Y: 0, W: 60, H: 10})
	waitForChatLog(t, chat)

	scroll.ScrollBottom(nil)
	bottom := scroll.ScrollPos()
	if bottom <= 0 {
		t.Fatalf("scroll position at bottom = %d, want a scrollable document", bottom)
	}

	scroll.ScrollBy(nil, -5)
	if got := scroll.ScrollPos(); got != bottom-5 {
		t.Fatalf("ScrollBy(-5) put position at %d, want %d", got, bottom-5)
	}

	scroll.ScrollTop(nil)
	if got := scroll.ScrollPos(); got != 0 {
		t.Fatalf("ScrollTop position = %d, want 0", got)
	}
}
