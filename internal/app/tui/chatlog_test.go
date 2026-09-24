package tui

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/widgets"
)

// findStyledRow returns the index of the first rendered chat-log row that
// carries the given ANSI escape sequence. The chat log only renders SGR
// sequences in inline-code, bold, italic, underline, and strikethrough spans,
// so any presence of "\x1b[" inside c.lines confirms an assistant message
// with styled markdown.
func findStyledRow(t *testing.T, log *ChatLog) int {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	for i, line := range log.lines {
		if strings.Contains(line.text, "\x1b[") {
			return i
		}
	}
	t.Fatalf("no rendered row carried ANSI escapes: %#v", log.lines)
	return -1
}

// TestChatLogMouseDragSelectsStyledTextAndCopiesOnRelease verifies that a
// mouse-drag selection over an assistant message containing styled markdown
// (inline code, bold) lands on the same visible characters the user pointed
// at and that the clipboard payload carries no shell escape sequences.
func TestChatLogMouseDragSelectsStyledTextAndCopiesOnRelease(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hi")
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("Inline `code` here")
	conv.EndAssistantMessage()

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, log)

	styledRow := findStyledRow(t, log)

	var copied string
	ctx := &tauui.Ctx{
		Invalidate:     func(tauui.Rect) {},
		ClipboardWrite: func(s string) { copied = s },
	}
	if !log.Handle(tauui.MouseEvent{X: 1, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MousePress}, ctx) {
		t.Fatal("mouse press was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: 17, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MouseDrag}, ctx) {
		t.Fatal("mouse drag was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: 17, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("mouse release was not handled")
	}

	if strings.Contains(copied, "\x1b[") {
		t.Fatalf("copied text contains ANSI escapes: %q", copied)
	}
	if want := "Inline code"; !strings.Contains(copied, want) {
		t.Fatalf("copied text = %q, want substring %q", copied, want)
	}
}

// TestChatLogDoubleClickSelectsStyledWordAndCopiesOnRelease verifies that a
// double-click on a styled word (e.g. inline code) selects that word rather
// than the surrounding escape sequences and that the clipboard receives clean
// text.
func TestChatLogDoubleClickSelectsStyledWordAndCopiesOnRelease(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hi")
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("Inline `code` here")
	conv.EndAssistantMessage()

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, log)

	styledRow := findStyledRow(t, log)

	var copied string
	ctx := &tauui.Ctx{
		Invalidate:     func(tauui.Rect) {},
		ClipboardWrite: func(s string) { copied = s },
	}

	log.mu.Lock()
	var visibleCol int
	for i, line := range log.lines {
		if i != styledRow {
			continue
		}
		stripped := tauui.StripANSI(line.text)
		idx := strings.Index(stripped, "code")
		if idx < 0 {
			log.mu.Unlock()
			t.Fatalf("styled row %d missing word: %q", i, stripped)
		}
		visibleCol = idx + 1
	}
	log.mu.Unlock()

	x := visibleCol
	if !log.Handle(tauui.MouseEvent{X: x, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MousePress, ClickCount: 2}, ctx) {
		t.Fatal("double-click press was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: x, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("double-click release was not handled")
	}

	if strings.Contains(copied, "\x1b[") {
		t.Fatalf("copied text contains ANSI escapes: %q", copied)
	}
	if copied != "code" {
		t.Fatalf("copied double-click selection = %q, want %q", copied, "code")
	}
}

// TestChatLogTripleClickSelectsStyledLineAndCopiesOnRelease verifies that a
// triple-click on a styled row returns the full visible line minus the leading
// padding space, with no shell escape sequences leaking into the clipboard.
func TestChatLogTripleClickSelectsStyledLineAndCopiesOnRelease(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hi")
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("Inline `code` here")
	conv.EndAssistantMessage()

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, log)

	styledRow := findStyledRow(t, log)

	var copied string
	ctx := &tauui.Ctx{
		Invalidate:     func(tauui.Rect) {},
		ClipboardWrite: func(s string) { copied = s },
	}
	if !log.Handle(tauui.MouseEvent{X: 5, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MousePress, ClickCount: 3}, ctx) {
		t.Fatal("triple-click press was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: 5, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("triple-click release was not handled")
	}

	if strings.Contains(copied, "\x1b[") {
		t.Fatalf("copied text contains ANSI escapes: %q", copied)
	}
	if want := "Inline code here"; !strings.Contains(copied, want) {
		t.Fatalf("copied triple-click selection = %q, want substring %q", copied, want)
	}
}

// TestChatLogSelectionPreservesPlainTextWithoutRegression guards the
// existing behaviour for chat lines that carry no styling, ensuring the
// visible-cell rewrite still copies exactly the characters the user dragged
// across.
func TestChatLogSelectionPreservesPlainTextWithoutRegression(t *testing.T) {
	conv := NewConversationModel()
	conv.AddAssistantMessage("plain assistant line")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, log)

	// The chat log renders an empty spacer above every assistant row, so the
	// actual message line sits at index 1.
	contentRow := 1

	var copied string
	ctx := &tauui.Ctx{
		Invalidate:     func(tauui.Rect) {},
		ClipboardWrite: func(s string) { copied = s },
	}
	if !log.Handle(tauui.MouseEvent{X: 1, Y: contentRow, Button: tauui.MouseButtonLeft, Action: tauui.MousePress}, ctx) {
		t.Fatal("mouse press was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: 6, Y: contentRow, Button: tauui.MouseButtonLeft, Action: tauui.MouseDrag}, ctx) {
		t.Fatal("mouse drag was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: 6, Y: contentRow, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("mouse release was not handled")
	}
	if copied != "plain" {
		t.Fatalf("copied selection = %q, want %q", copied, "plain")
	}
}

// TestChatLogSelectionAcrossStyledBoundaryJoinsSpans verifies that selecting
// from before an inline-code span into it (and out the other side) returns
// the expected visible characters with no leftover shell escapes.
func TestChatLogSelectionAcrossStyledBoundaryJoinsSpans(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hi")
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("Plain `styled` text")
	conv.EndAssistantMessage()

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, log)

	styledRow := findStyledRow(t, log)
	var visibleWidth int
	log.mu.Lock()
	for i, line := range log.lines {
		if i == styledRow {
			visibleWidth = tauui.TextWidth(line.text)
		}
	}
	log.mu.Unlock()
	if visibleWidth == 0 {
		t.Fatal("could not measure styled row visible width")
	}

	var copied string
	ctx := &tauui.Ctx{
		Invalidate:     func(tauui.Rect) {},
		ClipboardWrite: func(s string) { copied = s },
	}
	if !log.Handle(tauui.MouseEvent{X: 0, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MousePress}, ctx) {
		t.Fatal("mouse press was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: visibleWidth, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MouseDrag}, ctx) {
		t.Fatal("mouse drag was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: visibleWidth, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("mouse release was not handled")
	}

	if strings.Contains(copied, "\x1b[") {
		t.Fatalf("copied text contains ANSI escapes: %q", copied)
	}
	if want := "Plain styled text"; !strings.Contains(copied, want) {
		t.Fatalf("copied selection = %q, want substring %q", copied, want)
	}
}

// TestChatLogSelectionClampsToVisibleWidth ensures that clicks past the end of
// the visible row clamp to the last visible cell rather than some position
// inside the trailing ANSI escapes.
func TestChatLogSelectionClampsToVisibleWidth(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hi")
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("`x`")
	conv.EndAssistantMessage()

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, log)

	styledRow := findStyledRow(t, log)

	log.mu.Lock()
	width := tauui.TextWidth(log.lines[styledRow].text)
	log.mu.Unlock()

	var copied string
	ctx := &tauui.Ctx{
		Invalidate:     func(tauui.Rect) {},
		ClipboardWrite: func(s string) { copied = s },
	}
	if !log.Handle(tauui.MouseEvent{X: width, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MousePress}, ctx) {
		t.Fatal("mouse press was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: width + 50, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MouseDrag}, ctx) {
		t.Fatal("mouse drag was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: width + 50, Y: styledRow, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("mouse release was not handled")
	}
	if strings.Contains(copied, "\x1b[") {
		t.Fatalf("copied text contains ANSI escapes: %q", copied)
	}
}

func chatLogContentRowBounds(t *testing.T, log *ChatLog, component int) (first, last int) {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	if component < 0 || component >= len(log.renderedState.spans) {
		t.Fatalf("component %d not in rendered state", component)
	}
	span := log.renderedState.spans[component]
	if span.start < 0 || span.end > len(log.lines) {
		t.Fatalf("span %+v outside rendered lines", span)
	}
	first, last = -1, -1
	for i := span.start; i < span.end; i++ {
		if strings.TrimSpace(tauui.StripANSI(log.lines[i].text)) != "" {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		t.Fatalf("component %d rendered no content rows", component)
	}
	return first, last
}

func dragSelectChatLogRows(t *testing.T, log *ChatLog, y0, y1 int, copied *string) {
	t.Helper()
	ctx := &tauui.Ctx{
		Invalidate:     func(tauui.Rect) {},
		ClipboardWrite: func(s string) { *copied = s },
	}
	if !log.Handle(tauui.MouseEvent{X: 0, Y: y0, Button: tauui.MouseButtonLeft, Action: tauui.MousePress}, ctx) {
		t.Fatal("mouse press was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: 500, Y: y1, Button: tauui.MouseButtonLeft, Action: tauui.MouseDrag}, ctx) {
		t.Fatal("mouse drag was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: 500, Y: y1, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("mouse release was not handled")
	}
}

// TestChatLogWholeMessageSelectionCopiesUnwrappedText pins that a drag across
// every rendered row of one message copies the message's own text. Copying the
// word-wrapped render rows instead leaks hard line breaks at the terminal's
// wrap width, which mangles wrapped paragraphs, code, and tables.
func TestChatLogWholeMessageSelectionCopiesUnwrappedText(t *testing.T) {
	const body = "This is a long assistant paragraph that word wraps across several terminal rows when the chat width is narrow."
	conv := NewConversationModel()
	conv.AddAssistantMessage(body)
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 30, H: 20})
	waitForChatLog(t, log)

	first, last := chatLogContentRowBounds(t, log, 0)
	if first == last {
		t.Fatal("test message did not wrap onto multiple rows; test is invalid")
	}

	var copied string
	dragSelectChatLogRows(t, log, first, last, &copied)
	if copied != body {
		t.Fatalf("copied whole-message selection = %q, want the unwrapped message text %q", copied, body)
	}
}

// TestChatLogPartialWrappedSelectionCopiesVisibleRows pins that selecting only
// part of a wrapped message keeps copying exactly the visible cells: the
// whole-message copy path must not fire for partial selections.
func TestChatLogPartialWrappedSelectionCopiesVisibleRows(t *testing.T) {
	conv := NewConversationModel()
	conv.AddAssistantMessage("one two three four five six seven eight nine ten eleven twelve")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 20, H: 20})
	waitForChatLog(t, log)

	first, last := chatLogContentRowBounds(t, log, 0)
	if first == last {
		t.Fatal("test message did not wrap onto multiple rows; test is invalid")
	}

	log.mu.Lock()
	firstRowText := tauui.StripANSI(log.lines[first].text)
	log.mu.Unlock()

	var copied string
	ctx := &tauui.Ctx{
		Invalidate:     func(tauui.Rect) {},
		ClipboardWrite: func(s string) { copied = s },
	}
	if !log.Handle(tauui.MouseEvent{X: 1, Y: first, Button: tauui.MouseButtonLeft, Action: tauui.MousePress}, ctx) ||
		!log.Handle(tauui.MouseEvent{X: 500, Y: first, Button: tauui.MouseButtonLeft, Action: tauui.MouseDrag}, ctx) ||
		!log.Handle(tauui.MouseEvent{X: 500, Y: first, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("selection events were not handled")
	}
	if want := strings.TrimPrefix(firstRowText, " "); copied != want {
		t.Fatalf("copied partial selection = %q, want the visible first row %q", copied, want)
	}
}

// TestChatLogWholeTranscriptSelectionJoinsComponentTexts pins the multi-message
// drag: selecting several complete messages copies each message's text,
// joined the way /copy all formats them, instead of interleaved wrapped rows.
func TestChatLogWholeTranscriptSelectionJoinsComponentTexts(t *testing.T) {
	const firstBody = "First assistant message with more than enough words to wrap across several rows."
	const secondBody = "Second assistant message that also wraps comfortably across multiple rows here."
	conv := NewConversationModel()
	conv.AddAssistantMessage(firstBody)
	conv.AddAssistantMessage(secondBody)
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 30, H: 20})
	waitForChatLog(t, log)

	firstStart, _ := chatLogContentRowBounds(t, log, 0)
	_, secondEnd := chatLogContentRowBounds(t, log, 1)

	var copied string
	dragSelectChatLogRows(t, log, firstStart, secondEnd, &copied)
	if want := firstBody + "\n\n" + secondBody; copied != want {
		t.Fatalf("copied multi-message selection = %q, want %q", copied, want)
	}
}

// TestChatLogPartialSelectionAcrossWrappedRowsJoinsSoftWrap pins the partial
// copy path: dragging from mid-row through the end of a wrapped message must
// not put a hard break at the terminal's wrap width. Soft-wrapped rows join
// with the space the wrap consumed; only real source line breaks survive.
func TestChatLogPartialSelectionAcrossWrappedRowsJoinsSoftWrap(t *testing.T) {
	const body = "alpha beta gamma delta epsilon zeta eta theta iota kappa"
	conv := NewConversationModel()
	conv.AddAssistantMessage(body)
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 20, H: 20})
	waitForChatLog(t, log)

	first, last := chatLogContentRowBounds(t, log, 0)
	if first == last {
		t.Fatal("test message did not wrap onto multiple rows; test is invalid")
	}

	// Press at the third content cell so the selection is partial (the
	// whole-message copy path must not take it), then drag through the row end.
	var copied string
	ctx := &tauui.Ctx{
		Invalidate:     func(tauui.Rect) {},
		ClipboardWrite: func(s string) { copied = s },
	}
	if !log.Handle(tauui.MouseEvent{X: 3, Y: first, Button: tauui.MouseButtonLeft, Action: tauui.MousePress}, ctx) ||
		!log.Handle(tauui.MouseEvent{X: 500, Y: last, Button: tauui.MouseButtonLeft, Action: tauui.MouseDrag}, ctx) ||
		!log.Handle(tauui.MouseEvent{X: 500, Y: last, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("selection events were not handled")
	}

	if strings.Contains(copied, "\n") {
		t.Fatalf("copied partial selection carries hard breaks at wrap points: %q", copied)
	}
	if want := body[2:]; copied != want {
		t.Fatalf("copied partial selection = %q, want soft-wrapped join %q", copied, want)
	}
}

func TestChatLogRapidUpdatesCoalesceToLatestSnapshot(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("initial")
	conv.AddSystem("status: initial")
	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })

	var wraps atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	baseWrap := log.wrapComponents
	log.mu.Lock()
	log.wrapMessagesFunc = func(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
		if wraps.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return chatLogRenderState{}
			}
		}
		return baseWrap(ctx, components, width)
	}
	log.mu.Unlock()
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})

	log.Layout(tauui.Rect{W: 40, H: 10})
	waitForSignal(t, started, "first wrap start")
	conv.UpsertSystemByPrefix("status:", "status: intermediate one")
	conv.UpsertSystemByPrefix("status:", "status: intermediate two")
	conv.UpsertSystemByPrefix("status:", "status: latest state")
	close(release)
	waitForChatLog(t, log)

	if got := wraps.Load(); got != 2 {
		t.Fatalf("wrap calls = %d, want canceled initial wrap plus latest full wrap", got)
	}
	if got := chatLogText(log); !strings.Contains(got, "latest state") {
		t.Fatalf("final render does not contain latest snapshot: %q", got)
	}
}

func TestChatLogOldWidthResultCannotReplaceNewWidth(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("alpha beta gamma delta epsilon zeta eta theta")
	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })

	var wraps atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	baseWrap := log.wrapComponents
	log.mu.Lock()
	log.wrapMessagesFunc = func(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
		if wraps.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return chatLogRenderState{}
			}
		}
		return baseWrap(ctx, components, width)
	}
	log.mu.Unlock()

	updates := make(chan chatLogRenderUpdate, 4)
	log.setRenderNotifier(func() error {
		if update := log.applyPreparedRender(); update.installed {
			updates <- update
		}
		return nil
	})

	log.Layout(tauui.Rect{W: 40, H: 10})
	waitForSignal(t, started, "old-width wrap start")
	log.Layout(tauui.Rect{W: 10, H: 10})
	close(release)
	waitForChatLog(t, log)

	log.mu.Lock()
	wrapWidth := log.wrapWidth
	desiredWidth := log.desiredWidth
	log.mu.Unlock()
	if wrapWidth != 10 || desiredWidth != 10 {
		t.Fatalf("completed/desired widths = %d/%d, want 10/10", wrapWidth, desiredWidth)
	}
	if got := wraps.Load(); got != 2 {
		t.Fatalf("wrap calls = %d, want old and latest width", got)
	}
	if got := len(updates); got != 1 {
		t.Fatalf("installed updates = %d, want only the current-width result", got)
	}
}

func TestChatLogCachedMethodsDoNotWaitForWorker(t *testing.T) {
	conv := NewConversationModel()
	conv.AddSystem("cached")
	conv.AddSystem("second cached row")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{W: 40, H: 10})
	waitForChatLog(t, log)

	started := make(chan struct{})
	release := make(chan struct{})
	baseWrap := log.wrapComponents
	log.mu.Lock()
	cachedCount := log.lineCount
	log.wrapMessagesFunc = func(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
		close(started)
		select {
		case <-release:
			return baseWrap(ctx, components, width)
		case <-ctx.Done():
			return chatLogRenderState{}
		}
	}
	log.mu.Unlock()

	conv.Clear()
	waitForSignal(t, started, "blocked wrap start")

	returned := make(chan struct{})
	go func() {
		if got := log.LineCount(); got != cachedCount {
			t.Errorf("LineCount while pending = %d, want cached %d", got, cachedCount)
		}
		if got := log.MinSize().H; got != cachedCount {
			t.Errorf("MinSize while pending = %d, want cached %d", got, cachedCount)
		}
		log.Paint(noopDrawer{}, &tauui.Ctx{Theme: appTheme()})
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Paint/MinSize/LineCount blocked waiting for render worker")
	}
	close(release)
	waitForChatLog(t, log)
}

func TestChatLogStreamingAppendRewrapsOnlyAssistantMessage(t *testing.T) {
	conv := NewConversationModel()
	for i := range 100 {
		conv.AddSystem("history row " + strings.Repeat("x", i%7))
	}
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("stream")

	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	var fullWraps atomic.Int32
	baseWrap := log.wrapComponents
	log.mu.Lock()
	log.wrapMessagesFunc = func(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
		fullWraps.Add(1)
		return baseWrap(ctx, components, width)
	}
	log.mu.Unlock()
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})
	log.Layout(tauui.Rect{W: 40, H: 10})
	waitForChatLog(t, log)

	conv.AppendAssistantText("ing text")
	conv.AppendAssistantText(" arrives")
	conv.AppendAssistantText(" incrementally")
	waitForChatLog(t, log)

	if got := fullWraps.Load(); got > 1 {
		t.Fatalf("full transcript wraps = %d, want at most initial render", got)
	}
	if got := chatLogText(log); !strings.Contains(got, "streaming text arrives incrementally") {
		t.Fatalf("incremental assistant text missing from render: %q", got)
	}
}

func TestChatLogStreamingDeltaIsVisibleBeforeMessageEnd(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("question")
	conv.BeginAssistantMessage()
	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })

	posts := make(chan struct{}, 8)
	log.setRenderNotifier(func() error {
		posts <- struct{}{}
		return nil
	})
	applyNext := func() chatLogRenderUpdate {
		t.Helper()
		waitForSignal(t, posts, "streaming render post")
		update := log.applyPreparedRender()
		if !update.installed {
			t.Fatal("posted streaming render did not install")
		}
		return update
	}

	log.Layout(tauui.Rect{W: 40, H: 10})
	applyNext()
	conv.AppendAssistantText("first visible delta")
	update := applyNext()
	if !update.current {
		t.Fatal("single streaming delta did not catch up to current revision")
	}
	if !conv.IsStreaming() {
		t.Fatal("test finalized assistant before checking streaming visibility")
	}
	if got := chatLogText(log); !strings.Contains(got, "first visible delta") {
		t.Fatalf("streaming delta remained hidden until message end: %q", got)
	}

	conv.AppendAssistantText(" and another")
	applyNext()
	if got := chatLogText(log); !strings.Contains(got, "first visible delta and another") {
		t.Fatalf("second streaming delta was not painted incrementally: %q", got)
	}
}

func TestChatLogIncrementalSpanPreservesFollowingToolRows(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("run a command")
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("Before the tool")

	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	var fullWraps atomic.Int32
	baseWrap := log.wrapComponents
	log.mu.Lock()
	log.wrapMessagesFunc = func(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
		fullWraps.Add(1)
		return baseWrap(ctx, components, width)
	}
	log.mu.Unlock()
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})
	log.Layout(tauui.Rect{W: 24, H: 10})
	waitForChatLog(t, log)

	conv.AddToolCall("bash", `{"command":"ls"}`)
	waitForChatLog(t, log)
	if got := fullWraps.Load(); got != 1 {
		t.Fatalf("full wraps after append-only tool insertion = %d, want only initial render", got)
	}

	conv.AppendAssistantText(" and this appended explanation is long enough to add wrapped rows")
	waitForChatLog(t, log)
	if got := fullWraps.Load(); got != 1 {
		t.Fatalf("assistant append after tool insertion caused full wrap: got %d", got)
	}

	got := chatLogText(log)
	assistantAt := strings.Index(got, "Before the tool")
	toolAt := strings.Index(got, "$ ls")
	if assistantAt < 0 || toolAt < 0 || assistantAt >= toolAt {
		t.Fatalf("incremental splice lost or reordered following tool row: %q", got)
	}
}

func TestChatLogIncrementalRenderMatchesFullRender(t *testing.T) {
	log := NewChatLog(NewConversationModel())
	t.Cleanup(func() { _ = log.Close() })

	previousMessages := []Message{
		{Role: RoleUser, Text: "run a command"},
		{Role: RoleAssistant, Text: "A completed paragraph.\nAn active tail", Streaming: true},
		{Role: RoleToolCall, Text: `bash {"command":"ls"}`, ToolName: "bash", ToolArgs: `{"command":"ls"}`},
	}
	const width = 24
	previous := log.wrapMessages(context.Background(), previousMessages, width)

	messages := append([]Message(nil), previousMessages...)
	messages[1].Text += " that now wraps onto another row"
	incremental, ok := log.tryIncrementalRender(&previous, messages, width)
	if !ok {
		t.Fatal("append-only streaming assistant update did not use incremental render")
	}
	full := log.wrapMessages(context.Background(), messages, width)
	if !reflect.DeepEqual(incremental, full) {
		t.Fatalf("incremental render differs from full render:\nincremental=%#v\nfull=%#v", incremental, full)
	}
}

func TestChatLogAssistantFinalizationDoesNotFullRewrap(t *testing.T) {
	conv := NewConversationModel()
	for i := range 2000 {
		conv.AddSystem("history row " + strings.Repeat("x", i%17))
	}
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("final answer")

	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	var fullWraps atomic.Int32
	baseWrap := log.wrapComponents
	log.mu.Lock()
	log.wrapMessagesFunc = func(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
		fullWraps.Add(1)
		return baseWrap(ctx, components, width)
	}
	log.mu.Unlock()
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})
	log.Layout(tauui.Rect{W: 60, H: 20})
	waitForChatLog(t, log)

	conv.EndAssistantMessage()
	waitForChatLog(t, log)

	if got := fullWraps.Load(); got > 1 {
		t.Fatalf("assistant finalization caused full transcript wrap: got %d, want at most initial render", got)
	}
	if got := chatLogText(log); !strings.Contains(got, "final answer") {
		t.Fatalf("finalized assistant text missing from render: %q", got)
	}
}

func TestChatLogSingleMessageReplaceDoesNotFullRewrapHistory(t *testing.T) {
	conv := NewConversationModel()
	for i := range 2000 {
		conv.AddSystem("history row " + strings.Repeat("x", i%17))
	}
	conv.AddSystem("status: initial")

	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	var fullWraps atomic.Int32
	baseWrap := log.wrapComponents
	log.mu.Lock()
	log.wrapMessagesFunc = func(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
		fullWraps.Add(1)
		return baseWrap(ctx, components, width)
	}
	log.mu.Unlock()
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})
	log.Layout(tauui.Rect{W: 60, H: 20})
	waitForChatLog(t, log)

	conv.UpsertSystemByPrefix("status:", "status: changed without full history wrap")
	waitForChatLog(t, log)

	if got := fullWraps.Load(); got > 1 {
		t.Fatalf("single message replacement caused full transcript wrap: got %d, want at most initial render", got)
	}
	if got := chatLogText(log); !strings.Contains(got, "changed without full history wrap") {
		t.Fatalf("updated system text missing from render: %q", got)
	}
}

func TestChatLogToolProgressDoesNotFullRewrapHistory(t *testing.T) {
	conv := NewConversationModel()
	for i := range 2000 {
		conv.AddSystem("history row " + strings.Repeat("x", i%17))
	}
	conv.AddToolCallWithID("tool-1", "search", `{"q":"tau"}`)

	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	var fullWraps atomic.Int32
	baseWrap := log.wrapComponents
	log.mu.Lock()
	log.wrapMessagesFunc = func(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
		fullWraps.Add(1)
		return baseWrap(ctx, components, width)
	}
	log.mu.Unlock()
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})
	log.Layout(tauui.Rect{W: 60, H: 20})
	waitForChatLog(t, log)

	conv.AddToolProgress("tool-1", "running")
	conv.AddToolResultWithID("tool-1", "done", false)
	waitForChatLog(t, log)

	if got := fullWraps.Load(); got > 1 {
		t.Fatalf("tool progress/result caused full transcript wrap: got %d, want at most initial render", got)
	}
	if got := chatLogText(log); !strings.Contains(got, "done") {
		t.Fatalf("tool result missing from render: %q", got)
	}
}

func TestChatLogComponentCacheReusesUnchangedComponents(t *testing.T) {
	conv := NewConversationModel()
	conv.AddSystem("first row")
	conv.AddSystem("status: old")
	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })

	first := conv.Snapshot()
	state := log.wrapComponents(context.Background(), first.Components, 40)
	if len(log.renderCache) != 2 {
		t.Fatalf("render cache entries = %d, want 2", len(log.renderCache))
	}

	conv.UpsertSystemByPrefix("status:", "status: new")
	second := conv.Snapshot()
	if second.Components[0].Revision != first.Components[0].Revision || second.Components[1].Revision == first.Components[1].Revision {
		t.Fatalf("unexpected component revisions: before=%#v after=%#v", first.Components, second.Components)
	}
	updated, ok := log.tryIncrementalComponentRender(&state, second, second.Components, 40)
	if !ok {
		t.Fatal("component revision update did not use incremental render")
	}
	if len(log.renderCache) != 3 {
		t.Fatalf("render cache entries = %d, want unchanged component reused and changed component added", len(log.renderCache))
	}
	if got := strings.Join(renderLineTexts(updated.lines), "\n"); !strings.Contains(got, "status: new") {
		t.Fatalf("updated render missing changed component: %q", got)
	}
}

func TestChatLogComponentCacheIsKeyedByWidth(t *testing.T) {
	conv := NewConversationModel()
	conv.AddSystem("alpha beta gamma delta epsilon")
	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	snapshot := conv.Snapshot()

	log.wrapComponents(context.Background(), snapshot.Components, 40)
	log.wrapComponents(context.Background(), snapshot.Components, 10)

	if len(log.renderCache) != 2 {
		t.Fatalf("render cache entries = %d, want separate entries per width", len(log.renderCache))
	}
}

func TestChatLogComponentAssistantFinalizationDoesNotRewrapComponent(t *testing.T) {
	log := NewChatLog(NewConversationModel())
	t.Cleanup(func() { _ = log.Close() })
	previousComponents := []ChatComponent{componentFromMessage(Message{ID: "a1", Role: RoleAssistant, Text: "final", Streaming: true}, 1)}
	previous := log.wrapComponents(context.Background(), previousComponents, 40)
	components := []ChatComponent{componentFromMessage(Message{ID: "a1", Role: RoleAssistant, Text: "final", Streaming: false}, 2)}

	updated, ok := log.tryIncrementalComponentRender(&previous, ChatSnapshot{Revision: 2, Components: components}, components, 40)
	if !ok {
		t.Fatal("assistant finalization did not use component incremental path")
	}
	if !reflect.DeepEqual(updated.lines, previous.lines) {
		t.Fatalf("metadata-only finalization changed rendered lines: before=%#v after=%#v", previous.lines, updated.lines)
	}
}

func renderLineTexts(lines []renderLine) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = line.text
	}
	return out
}

// paintChatLogRows paints log into a fresh cell buffer and returns the visible
// rows as strings. Asserting on painted cells — rather than on the pre-paint
// row strings — matters for whitespace bugs: rovel's cell renderer drops
// zero-width control characters (including tab), so a raw tab survives in the
// row string while vanishing from the screen.
func paintChatLogRows(t *testing.T, log *ChatLog, width, height int) []string {
	t.Helper()
	base := tauui.Style{}
	buf := tauui.NewBuffer(width, height)
	buf.Clear(tauui.Cell{R: ' ', Style: base})
	rp := tauui.NewRenderPainter(buf, tauui.Rect{X: 0, Y: 0, W: width, H: height}, base)
	d := tauui.NewDrawer(tauui.NewPainter(rp, base))
	log.Paint(d, &tauui.Ctx{Theme: appTheme()})
	rows := make([]string, height)
	for y := range height {
		rows[y] = bufferRowString(buf, y, width)
	}
	return rows
}

func layoutAndPaintChatLog(t *testing.T, log *ChatLog, width int) []string {
	t.Helper()
	log.Layout(tauui.Rect{X: 0, Y: 0, W: width, H: 1})
	waitForChatLog(t, log)
	height := max(1, log.LineCount())
	log.Layout(tauui.Rect{X: 0, Y: 0, W: width, H: height})
	waitForChatLog(t, log)
	return paintChatLogRows(t, log, width, height)
}

// TestChatLogBashOutputPreservesTabAndSpaceIndent guards the plain-look bash
// renderer: tab-indented output must render as the toolkit's three-space
// expansion, and space-indented output must keep its leading spaces. The chat
// row adds one cell of padding, so "\ttabbed" paints with four leading spaces
// and "  spaced" with three.
func TestChatLogBashOutputPreservesTabAndSpaceIndent(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("tool-1", "bash", `{"command":"printf 'top\\n\\ttabbed\\n  spaced\\n'"}`)
	conv.AddToolResultWithID("tool-1", "top\n\ttabbed\n  spaced", false)
	log := newTestChatLog(t, conv)

	rows := layoutAndPaintChatLog(t, log, 40)
	joined := strings.Join(rows, "\n")
	for _, want := range []string{" top", "    tabbed", "   spaced"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("plain bash output missing %q (indent collapsed):\n%s", want, joined)
		}
	}
}

// TestChatLogTauBashChromePreservesTabAndSpaceIndent is the bordered-look
// variant of the bash indent guard: the same output must keep its indentation
// inside the dynamic border's content column.
func TestChatLogTauBashChromePreservesTabAndSpaceIndent(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("tool-1", "bash", `{"command":"printf 'top\\n\\ttabbed\\n  spaced\\n'"}`)
	conv.AddToolResultWithID("tool-1", "top\n\ttabbed\n  spaced", false)
	log := newTestChatLog(t, conv)
	log.SetLookName("tau")

	rows := layoutAndPaintChatLog(t, log, 40)
	joined := strings.Join(rows, "\n")
	// Border content column starts after "│ ": pad(1) + tab(3) for tabbed,
	// pad(1) + two spaces for spaced.
	for _, want := range []string{"│ top", "│    tabbed", "│   spaced"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("tau bash chrome missing %q (indent collapsed):\n%s", want, joined)
		}
	}
}

// TestChatLogEditDiffPreservesTabAndSpaceIndent guards the diff renderer:
// unified-diff context/added/removed lines keep their indentation, with tabs
// expanded like every other chat row.
func TestChatLogEditDiffPreservesTabAndSpaceIndent(t *testing.T) {
	diff := strings.Join([]string{
		"-old line",
		"+\tnew tabbed",
		"   context spaces",
	}, "\n")
	conv := NewConversationModel()
	conv.AddToolCallWithID("tool-1", "edit", `{"path":"f.go","old_string":"old line","new_string":"new tabbed"}`)
	conv.AddToolResultWithDetails("tool-1", "", "ok", false, map[string]any{"diff": diff})
	log := newTestChatLog(t, conv)

	rows := layoutAndPaintChatLog(t, log, 40)
	joined := strings.Join(rows, "\n")
	for _, want := range []string{
		" -old line",
		" +   new tabbed", // chat pad + marker + tab→3 spaces
		"    context spaces",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("edit diff missing %q (indent collapsed):\n%s", want, joined)
		}
	}
}

// TestChatLogAssistantFencedCodePreservesIndent guards the streaming/every-line
// fenced-code path: fenced content is rendered as literal wrapped lines, and
// its indentation must survive painting.
func TestChatLogAssistantFencedCodePreservesIndent(t *testing.T) {
	conv := NewConversationModel()
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("```\n\ttabbed\n  spaced\n```")
	conv.EndAssistantMessage()
	log := newTestChatLog(t, conv)

	rows := layoutAndPaintChatLog(t, log, 40)
	joined := strings.Join(rows, "\n")
	for _, want := range []string{"    tabbed", "   spaced"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("fenced code missing %q (indent collapsed):\n%s", want, joined)
		}
	}
}

func TestChatLogThinkingRemovalDoesNotFullRewrapHistory(t *testing.T) {
	conv := NewConversationModel()
	for i := range 2000 {
		conv.AddSystem("history row " + strings.Repeat("x", i%17))
	}
	conv.BeginAssistantMessage()
	conv.AddThinking()

	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	var fullWraps atomic.Int32
	baseWrap := log.wrapComponents
	log.mu.Lock()
	log.wrapMessagesFunc = func(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
		fullWraps.Add(1)
		return baseWrap(ctx, components, width)
	}
	log.mu.Unlock()
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})
	log.Layout(tauui.Rect{W: 60, H: 20})
	waitForChatLog(t, log)

	conv.ClearThinking()
	waitForChatLog(t, log)

	if got := fullWraps.Load(); got > 1 {
		t.Fatalf("thinking removal caused full transcript wrap: got %d, want at most initial render", got)
	}
	if got := chatLogText(log); strings.Contains(got, "Thinking") {
		t.Fatalf("thinking row remained after removal: %q", got)
	}
}

func TestChatLogIncrementalRenderFallsBackForStructuralChanges(t *testing.T) {
	log := NewChatLog(NewConversationModel())
	t.Cleanup(func() { _ = log.Close() })

	previousMessages := []Message{
		{Role: RoleUser, Text: "question"},
		{Role: RoleAssistant, Text: "streaming", Streaming: true},
	}
	const width = 30
	previous := log.wrapMessages(context.Background(), previousMessages, width)

	tests := []struct {
		name     string
		messages []Message
		width    int
	}{
		{
			name: "width change",
			messages: []Message{
				{Role: RoleUser, Text: "question"},
				{Role: RoleAssistant, Text: "streaming more", Streaming: true},
			},
			width: width - 1,
		},
		{
			name: "multiple message changes",
			messages: []Message{
				{Role: RoleUser, Text: "changed question"},
				{Role: RoleAssistant, Text: "streaming more", Streaming: true},
			},
			width: width,
		},
		{
			name: "middle message insertion",
			messages: []Message{
				{Role: RoleUser, Text: "question"},
				{Role: RoleToolCall, ToolName: "bash", ToolArgs: `{"command":"ls"}`},
				{Role: RoleAssistant, Text: "streaming", Streaming: true},
			},
			width: width,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := log.tryIncrementalRender(&previous, tt.messages, tt.width); ok {
				t.Fatal("structural change unexpectedly used incremental render")
			}
		})
	}
}

func TestChatLogAppendOnlyMessageDoesNotFullRewrapHistory(t *testing.T) {
	conv := NewConversationModel()
	for i := range 2000 {
		conv.AddSystem("history row " + strings.Repeat("x", i%17))
	}

	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	var fullWraps atomic.Int32
	baseWrap := log.wrapComponents
	log.mu.Lock()
	log.wrapMessagesFunc = func(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
		fullWraps.Add(1)
		return baseWrap(ctx, components, width)
	}
	log.mu.Unlock()
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})
	log.Layout(tauui.Rect{W: 60, H: 20})
	waitForChatLog(t, log)

	conv.AddUserMessage("new prompt should append without rewrapping all history")
	waitForChatLog(t, log)

	if got := fullWraps.Load(); got > 1 {
		t.Fatalf("full transcript wraps = %d, want at most initial render", got)
	}
	if got := chatLogText(log); !strings.Contains(got, "new prompt should append") {
		t.Fatalf("appended message missing from render: %q", got)
	}
}

func TestChatLogZeroWidthForcesFreshSnapshotWhenShownAgain(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("before hiding")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{W: 30, H: 5})
	waitForChatLog(t, log)

	log.Layout(tauui.Rect{W: 0, H: 5})
	conv.AddSystem("changed while hidden")
	log.Layout(tauui.Rect{W: 30, H: 5})
	waitForChatLog(t, log)

	if got := chatLogText(log); !strings.Contains(got, "changed while hidden") {
		t.Fatalf("restored width reused stale render: %q", got)
	}
}

func TestChatLogRenderUpdateReportsHeightChanges(t *testing.T) {
	conv := NewConversationModel()
	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	updates := make(chan chatLogRenderUpdate, 4)
	log.setRenderNotifier(func() error {
		if update := log.applyPreparedRender(); update.installed {
			updates <- update
		}
		return nil
	})

	log.Layout(tauui.Rect{W: 40, H: 10})
	first := receiveRenderUpdate(t, updates)
	if !first.layoutChanged {
		t.Fatal("initial completed width should request layout")
	}

	conv.AddSystem("same single completed row")
	sameHeight := receiveRenderUpdate(t, updates)
	if !sameHeight.layoutChanged {
		t.Fatal("system/status rows include a leading spacer, so first row should change height")
	}

	conv.AddSystem("second row")
	taller := receiveRenderUpdate(t, updates)
	if !taller.layoutChanged {
		t.Fatal("completed height change did not request layout")
	}
}

func TestChatLogCloseIsIdempotentAndStopsScheduling(t *testing.T) {
	conv := NewConversationModel()
	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})
	log.Layout(tauui.Rect{W: 20, H: 5})
	waitForChatLog(t, log)

	log.mu.Lock()
	revision := log.revision
	log.mu.Unlock()

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			if err := log.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	closed := make(chan struct{})
	go func() {
		wg.Wait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("concurrent Close calls did not return")
	}

	conv.AddSystem("after close")
	log.Layout(tauui.Rect{W: 10, H: 5})
	log.MarkDirty()
	log.mu.Lock()
	gotRevision := log.revision
	pending := log.pending
	log.mu.Unlock()
	if gotRevision != revision || pending != nil {
		t.Fatalf("closed ChatLog scheduled work: revision=%d want=%d pending=%#v", gotRevision, revision, pending)
	}
}

func TestRefreshChatDefersScrollUntilRenderCompletion(t *testing.T) {
	conv := NewConversationModel()
	for i := range 10 {
		conv.AddUserMessage(strings.Repeat("x", i+1))
	}
	log := newTestChatLog(t, conv)
	scroll := widgets.NewScrollView(widgets.ScrollViewOpts{Child: log})
	scroll.Layout(tauui.Rect{W: 20, H: 3})
	waitForChatLog(t, log)
	// Re-layout once completed height is available, matching the app's
	// render-ready layout invalidation pass.
	scroll.Layout(tauui.Rect{W: 20, H: 3})

	invalidations := 0
	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) { invalidations++ }}
	scroll.ScrollBottom(ctx)
	if scroll.ScrollPos() == 0 {
		t.Fatal("test setup did not produce scrollable content")
	}
	scroll.ScrollTop(ctx)
	invalidations = 0

	a := &App{scroll: scroll}
	a.loopMu.Lock()
	a.loopCtx = ctx
	a.loopMu.Unlock()
	a.refreshChat()
	if got := scroll.ScrollPos(); got != 0 {
		t.Fatalf("refreshChat scrolled stale content to %d", got)
	}
	if invalidations != 0 {
		t.Fatalf("refreshChat invalidated stale viewport %d times", invalidations)
	}
	if !a.followChat {
		t.Fatal("refreshChat did not preserve follow-latest intent")
	}
}

func TestChatOverflowKeepsPromptAndFooterOnScreen(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.chat.Close() })

	// Apply worker results synchronously in this headless test, then drive the
	// layout pass explicitly as the production App.Post callback would.
	a.chat.setRenderNotifier(func() error {
		a.chat.applyPreparedRender()
		return nil
	})
	screen := tauui.Rect{W: 40, H: 14}
	a.root.Layout(screen)
	waitForChatLog(t, a.chat)
	a.root.Layout(screen)

	for i := range 30 {
		a.conv.AddSystem("overflow row " + strings.Repeat("x", i%5))
	}
	waitForChatLog(t, a.chat)
	a.root.Layout(screen)
	// Overflow enables the scrollbar and removes one content column, which
	// schedules one final rewrap at the narrower width.
	waitForChatLog(t, a.chat)
	a.root.Layout(screen)

	if contentH, viewportH := a.chat.MinSize().H, a.scroll.Rect().H; contentH <= viewportH {
		t.Fatalf("test setup did not overflow: content=%d viewport=%d", contentH, viewportH)
	}
	if got, want := a.editor.Rect().H, a.editor.MinSize().H; got != want {
		t.Fatalf("prompt height after chat overflow = %d, want %d", got, want)
	}
	if bottom := a.editor.Rect().Y + a.editor.Rect().H; bottom > screen.Y+screen.H {
		t.Fatalf("prompt extends below screen: bottom=%d screenBottom=%d", bottom, screen.Y+screen.H)
	}
	if rect := a.footer.Rect(); rect.H != a.footer.MinSize().H || rect.Y+rect.H > screen.Y+screen.H {
		t.Fatalf("footer pushed off-screen after chat overflow: rect=%+v screen=%+v", rect, screen)
	}

	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}
	a.scroll.ScrollBottom(ctx)
	if got := a.scroll.ScrollPos(); got <= 0 {
		t.Fatalf("overflowing chat did not become scrollable: position=%d", got)
	}
}

func TestConversationModelSubscribeUnregistersIdempotently(t *testing.T) {
	conv := NewConversationModel()
	var calls int
	unsubscribe := conv.Subscribe(func() { calls++ })
	conv.AddSystem("before")
	unsubscribe()
	unsubscribe()
	conv.AddSystem("after")
	if calls != 1 {
		t.Fatalf("listener calls = %d, want 1 before unsubscribe", calls)
	}
}

func TestAppRegistersOnlyChatLogConversationListener(t *testing.T) {
	a, err := New(Options{Session: &fakeSession{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.chat.Close() })

	a.conv.mu.Lock()
	listeners := len(a.conv.listeners)
	a.conv.mu.Unlock()
	if listeners != 1 {
		t.Fatalf("conversation listeners = %d, want only ChatLog subscription", listeners)
	}
}

func TestChatLogNewSnapshotCancelsStaleFullRender(t *testing.T) {
	conv := NewConversationModel()
	conv.AddSystem("initial")
	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })

	started := make(chan struct{})
	canceled := make(chan struct{})
	baseWrap := log.wrapComponents
	var wraps atomic.Int32
	log.mu.Lock()
	log.wrapMessagesFunc = func(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
		if wraps.Add(1) == 1 {
			close(started)
			select {
			case <-ctx.Done():
				close(canceled)
				return chatLogRenderState{}
			case <-time.After(time.Second):
				return chatLogRenderState{}
			}
		}
		return baseWrap(ctx, components, width)
	}
	log.mu.Unlock()
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})

	log.Layout(tauui.Rect{W: 40, H: 10})
	waitForSignal(t, started, "first full render start")
	conv.AddSystem("newer snapshot")
	waitForSignal(t, canceled, "stale full render cancellation")
	waitForChatLog(t, log)

	if got := chatLogText(log); !strings.Contains(got, "newer snapshot") {
		t.Fatalf("latest snapshot was not rendered after cancellation: %q", got)
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func receiveRenderUpdate(t *testing.T, updates <-chan chatLogRenderUpdate) chatLogRenderUpdate {
	t.Helper()
	select {
	case update := <-updates:
		return update
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for render update")
		return chatLogRenderUpdate{}
	}
}

func chatLogText(log *ChatLog) string {
	log.mu.Lock()
	defer log.mu.Unlock()
	var b strings.Builder
	for _, line := range log.lines {
		b.WriteString(line.text)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestChatLogLargeHistoryRendersFully pins that a long transcript is rendered
// in full. It previously also asserted the placeholder-virtualization path was
// not taken; that path has since been removed, since it was never enabled and
// the cost it existed to avoid was a quadratic text wrapper, now fixed.
func TestChatLogLargeHistoryRendersFully(t *testing.T) {
	conv := NewConversationModel()
	for i := range 1500 {
		conv.AddSystem("history row " + strings.Repeat("x", i%11))
	}
	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})
	log.setVisibleRows(0, 20)
	log.Layout(tauui.Rect{W: 60, H: 20})
	waitForChatLog(t, log)

	log.mu.Lock()
	lineCount := log.lineCount
	text := strings.Join(renderLineTexts(log.lines), "\n")
	log.mu.Unlock()

	if lineCount < 1500 {
		t.Fatalf("line count = %d, want full rendered history", lineCount)
	}
	if !strings.Contains(text, "history row xxxxxxxxxx") {
		t.Fatalf("full history render missing expected content")
	}
}

// benchConvOfLines fills a conversation until it wraps to at least the given
// number of rendered lines at width 80.
func benchConvOfLines(lines int) *ConversationModel {
	conv := NewConversationModel()
	rows := 0
	i := 0
	conv.Batch(func() {
		for rows < lines {
			switch i % 3 {
			case 0:
				conv.AddUserMessage(fmt.Sprintf("please do task %d", i))
				rows += 4
			case 1:
				conv.AddAssistantMessage(strings.Repeat(fmt.Sprintf("reasoning about task %d. ", i), 6))
				rows += 3
			default:
				conv.AddToolCallWithID(fmt.Sprintf("call-%d", i), "bash", `{"command":"ls -la"}`)
				conv.AddToolResultWithID(fmt.Sprintf("call-%d", i), strings.Repeat("output line\n", 6), false)
				rows += 8
			}
			i++
		}
	})
	return conv
}

func benchRenderedChatLog(tb testing.TB, conv *ConversationModel) *ChatLog {
	tb.Helper()
	chat := NewChatLog(conv)
	chat.setRenderNotifier(func() error { return nil })
	chat.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 40})
	benchSettle(tb, chat)
	return chat
}

func benchSettle(tb testing.TB, chat *ChatLog) {
	tb.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		chat.mu.Lock()
		has := chat.latestPrepared != nil
		idle := chat.pending == nil && chat.latestPrepared == nil && chat.renderedRevision == chat.revision
		chat.mu.Unlock()
		if has {
			chat.applyPreparedRender()
			continue
		}
		if idle {
			return
		}
		time.Sleep(20 * time.Microsecond)
	}
	tb.Fatal("chat log never settled")
}

var benchSizes = []int{82, 162, 246, 500, 1000, 2000}

// BenchmarkChatLogScheduleSnapshot measures the synchronous work every
// conversation mutation performs on the app loop before the worker sees it.
func BenchmarkChatLogScheduleSnapshot(b *testing.B) {
	for _, lines := range benchSizes {
		b.Run(fmt.Sprintf("lines=%d", lines), func(b *testing.B) {
			conv := benchConvOfLines(lines)
			chat := benchRenderedChatLog(b, conv)
			b.Cleanup(func() { _ = chat.Close() })
			b.ReportAllocs()
			for b.Loop() {
				chat.scheduleCurrentSnapshot()
			}
		})
	}
}

// BenchmarkChatLogFullWrap measures a cold full wrap of the whole transcript.
func BenchmarkChatLogFullWrap(b *testing.B) {
	for _, lines := range benchSizes {
		b.Run(fmt.Sprintf("lines=%d", lines), func(b *testing.B) {
			conv := benchConvOfLines(lines)
			chat := benchRenderedChatLog(b, conv)
			b.Cleanup(func() { _ = chat.Close() })
			components := conv.Snapshot().Components
			b.ReportAllocs()
			for b.Loop() {
				_ = chat.wrapComponents(context.Background(), components, 80)
			}
		})
	}
}

// BenchmarkChatLogWarmWrap measures a re-wrap where every component is already
// in the render cache — the path a prepend or any non-incremental reschedule
// takes once the transcript has been rendered before.
func BenchmarkChatLogWarmWrap(b *testing.B) {
	for _, lines := range benchSizes {
		b.Run(fmt.Sprintf("lines=%d", lines), func(b *testing.B) {
			conv := benchConvOfLines(lines)
			chat := benchRenderedChatLog(b, conv)
			b.Cleanup(func() { _ = chat.Close() })
			components := conv.Snapshot().Components
			_ = chat.wrapComponents(context.Background(), components, 80)
			b.ReportAllocs()
			for b.Loop() {
				_ = chat.wrapComponents(context.Background(), components, 80)
			}
		})
	}
}

// BenchmarkChatLogPaint measures one repaint of a single viewport-sized window.
func BenchmarkChatLogPaint(b *testing.B) {
	for _, lines := range benchSizes {
		b.Run(fmt.Sprintf("lines=%d", lines), func(b *testing.B) {
			conv := benchConvOfLines(lines)
			chat := benchRenderedChatLog(b, conv)
			b.Cleanup(func() { _ = chat.Close() })
			chat.setVisibleRows(0, 40)
			d := &benchDrawer{}
			ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}
			b.ReportAllocs()
			for b.Loop() {
				chat.Paint(d, ctx)
			}
		})
	}
}

// TestAppendAndStreamingAvoidFullRewrap pins the property that keeps chat cost
// independent of transcript length: appending a message and streaming a text
// delta must both take the incremental path. If either falls back to a full
// wrap, every keystroke of a long session re-renders the whole transcript.
func TestAppendAndStreamingAvoidFullRewrap(t *testing.T) {
	for _, lines := range []int{82, 246, 1000} {
		conv := benchConvOfLines(lines)
		chat := benchRenderedChatLog(t, conv)

		before := chat.Metrics()
		conv.AddAssistantMessage("appended")
		benchSettle(t, chat)
		afterAppend := chat.Metrics()
		if got := afterAppend.FullRenderCount - before.FullRenderCount; got != 0 {
			t.Errorf("lines=%d: appending a message triggered %d full re-wrap(s), want 0", lines, got)
		}
		if got := afterAppend.IncrementalRenderCount - before.IncrementalRenderCount; got == 0 {
			t.Errorf("lines=%d: appending a message did not use the incremental path", lines)
		}

		conv.BeginAssistantMessage()
		benchSettle(t, chat)
		beforeDelta := chat.Metrics()
		conv.AppendAssistantText("token ")
		benchSettle(t, chat)
		afterDelta := chat.Metrics()
		if got := afterDelta.FullRenderCount - beforeDelta.FullRenderCount; got != 0 {
			t.Errorf("lines=%d: a streaming delta triggered %d full re-wrap(s), want 0", lines, got)
		}
		if got := afterDelta.IncrementalRenderCount - beforeDelta.IncrementalRenderCount; got == 0 {
			t.Errorf("lines=%d: a streaming delta did not use the incremental path", lines)
		}

		// A delta must re-wrap only the draft component, never the history.
		if got := afterDelta.CacheMisses - beforeDelta.CacheMisses; got > 1 {
			t.Errorf("lines=%d: a streaming delta caused %d cache misses, want at most 1 (the draft)", lines, got)
		}
		_ = chat.Close()
	}
}

type benchDrawer struct{}

func (d *benchDrawer) FillRect(tauui.Rect, tauui.Style)             {}
func (d *benchDrawer) DrawText(tauui.Point, string, tauui.Style)    {}
func (d *benchDrawer) DrawBorder(tauui.Rect, tauui.BoxStyle)        {}
func (d *benchDrawer) ClipRect() tauui.Rect                         { return tauui.Rect{W: 80, H: 40} }
func (d *benchDrawer) WithClip(_ tauui.Rect, fn func(tauui.Drawer)) { fn(d) }
func (d *benchDrawer) WithOffset(_, _ int, fn func(tauui.Drawer))   { fn(d) }

// TestConversationProjectionInvalidatesOnMutation guards the per-item
// projection memo: every mutation assigns a new revision, so the memo must
// never serve stale text for a tool execution that has since produced output,
// completed, or been cancelled.
func TestConversationProjectionInvalidatesOnMutation(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("call-1", "bash", `{"command":"ls"}`)

	first := conv.Snapshot().Components
	if len(first) != 1 {
		t.Fatalf("components = %d, want 1", len(first))
	}
	if strings.Contains(first[0].Text, "partial output") {
		t.Fatalf("unexpected output in fresh tool call: %q", first[0].Text)
	}

	conv.AddToolProgress("call-1", "partial output")
	afterProgress := conv.Snapshot().Components
	if !strings.Contains(afterProgress[0].Text, "partial output") {
		t.Fatalf("projection after progress = %q, want it to include the streamed output", afterProgress[0].Text)
	}
	if afterProgress[0].Revision == first[0].Revision {
		t.Fatal("tool progress did not change the component revision; the projection memo cannot invalidate")
	}

	conv.AddToolResultWithID("call-1", "final output", false)
	afterResult := conv.Snapshot().Components
	if !strings.Contains(afterResult[0].Text, "final output") {
		t.Fatalf("projection after result = %q, want the final output", afterResult[0].Text)
	}
	if got := afterResult[0].ToolExecution.State; got != toolExecutionComplete {
		t.Fatalf("tool state = %v, want complete", got)
	}

	conv.CancelTool("call-1", "user cancelled")
	afterCancel := conv.Snapshot().Components
	if got := afterCancel[0].ToolExecution.State; got != toolExecutionCancelled {
		t.Fatalf("tool state = %v, want cancelled", got)
	}
	if !afterCancel[0].ToolExecution.IsError {
		t.Fatal("cancelled tool should project as an error")
	}
	if got := afterCancel[0].Message().Role; got != RoleError {
		t.Fatalf("cancelled tool message role = %q, want %q", got, RoleError)
	}
}

// TestConversationProjectionMatchesFreshBuild checks the memo against an
// unmemoized projection for every item kind, so a cached component can never
// differ from one built from scratch.
func TestConversationProjectionMatchesFreshBuild(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hello")
	conv.AddAssistantMessage("hi there")
	conv.AddSystem("a system notice")
	conv.AddError("something failed")
	conv.AddHistoricalThinking("think-1", "private reasoning")
	conv.AddToolCallWithID("call-1", "bash", `{"command":"ls"}`)
	conv.AddToolResultWithID("call-1", "file-a\nfile-b", false)

	// Two projections in a row: the second is served entirely from the memo.
	first := conv.Snapshot().Components
	second := conv.Snapshot().Components
	if len(first) != len(second) {
		t.Fatalf("component counts differ: %d vs %d", len(first), len(second))
	}

	conv.mu.Lock()
	defer conv.mu.Unlock()
	for i := range conv.committed {
		want := conv.committed[i].buildComponent()
		got := second[i]
		if got.ID != want.ID || got.Kind != want.Kind || got.Role != want.Role ||
			got.Text != want.Text || got.Revision != want.Revision || got.Collapsed != want.Collapsed ||
			got.ToolExecution != want.ToolExecution {
			t.Fatalf("memoized component[%d] = %#v, want %#v", i, got, want)
		}
	}
}

// TestRenderComponentCacheNotMutatedByWrap guards the read-only contract on the
// slice renderComponent returns: wraps alias the cached lines instead of
// copying them, so any caller writing through the result would corrupt every
// later render of that component.
func TestRenderComponentCacheNotMutatedByWrap(t *testing.T) {
	conv := NewConversationModel()
	// Consecutive user messages exercise the spacer path, which prepends a row
	// to the component's lines.
	conv.AddUserMessage("first user message")
	conv.AddUserMessage("second user message")
	conv.AddAssistantMessage("an assistant reply")
	conv.AddToolCallWithID("call-1", "bash", `{"command":"ls"}`)
	conv.AddToolResultWithID("call-1", "file-a\nfile-b", false)

	chat := newTestChatLog(t, conv)
	chat.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, chat)

	components := conv.Snapshot().Components
	first := chat.wrapComponents(context.Background(), components, 80)
	firstRows := renderLineTexts(first.lines)

	// Snapshot the cache, then wrap repeatedly. Nothing may change.
	cacheBefore := snapshotRenderCache(chat)
	for range 3 {
		next := chat.wrapComponents(context.Background(), components, 80)
		if got, want := renderLineTexts(next.lines), firstRows; !slices.Equal(got, want) {
			t.Fatalf("re-wrap produced different rows\n got: %q\nwant: %q", got, want)
		}
	}
	cacheAfter := snapshotRenderCache(chat)
	for key, before := range cacheBefore {
		after, ok := cacheAfter[key]
		if !ok {
			continue
		}
		if !slices.Equal(before, after) {
			t.Fatalf("render cache entry %v mutated by wrapping\n before: %q\n after:  %q", key, before, after)
		}
	}
}

func snapshotRenderCache(chat *ChatLog) map[renderCacheKey][]string {
	chat.mu.Lock()
	defer chat.mu.Unlock()
	out := make(map[renderCacheKey][]string, len(chat.renderCache))
	for key, entry := range chat.renderCache {
		out[key] = renderLineTexts(entry.lines)
	}
	return out
}

func TestChatLogRenderRowsUsesCachedTranscript(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hello")
	conv.AddAssistantMessage("world")
	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})
	log.Layout(tauui.Rect{W: 20, H: 5})
	waitForChatLog(t, log)

	rows := tauui.RenderRows(log, 12)
	if len(rows) == 0 {
		t.Fatal("RenderRows returned no rows")
	}
	for i, row := range rows {
		if got := tauui.TextWidth(row); got != 12 {
			t.Fatalf("row %d width = %d, want 12: %q", i, got, row)
		}
	}
	joined := stringsJoinRows(rows)
	if !rowsContainString(joined, "hello") || !rowsContainString(joined, "world") {
		t.Fatalf("rendered rows missing transcript content: %#v", rows)
	}
}

func TestChatLogRenderRowsDoesNotScheduleOrWait(t *testing.T) {
	conv := NewConversationModel()
	conv.AddSystem("cached")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{W: 30, H: 5})
	waitForChatLog(t, log)

	started := make(chan struct{})
	release := make(chan struct{})
	baseWrap := log.wrapComponents
	log.mu.Lock()
	log.wrapMessagesFunc = func(ctx context.Context, components []ChatComponent, width int) chatLogRenderState {
		close(started)
		select {
		case <-release:
			return baseWrap(ctx, components, width)
		case <-ctx.Done():
			return chatLogRenderState{}
		}
	}
	log.mu.Unlock()

	conv.AddSystem("pending")
	waitForChatLog(t, log)
	rows := tauui.RenderRows(log, 10)
	if len(rows) == 0 || !rowsContainString(stringsJoinRows(rows), "cached") {
		t.Fatalf("RenderRows should return cached transcript while worker is pending: %#v", rows)
	}
	close(release)
	waitForChatLog(t, log)
}

func stringsJoinRows(rows []string) string {
	var out string
	for _, row := range rows {
		out += row + "\n"
	}
	return out
}

func rowsContainString(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return sub == ""
}

func waitForPreparedChatLog(t *testing.T, log *ChatLog) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		log.mu.Lock()
		ready := log.latestPrepared != nil && log.pending == nil
		revision := log.revision
		renderedRevision := log.renderedRevision
		log.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("chat log did not prepare render: revision=%d rendered=%d", revision, renderedRevision)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestChatLogAssistantMarkdownTableRenders pins that GitHub-style markdown
// tables streamed as an assistant message render as bordered tables, not as
// literal pipe-delimited paragraphs.
func TestChatLogAssistantMarkdownTableRenders(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("show me a table")
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("| Category | Count | Words |\n")
	conv.AppendAssistantText("|---|---|---|\n")
	conv.AppendAssistantText("| something | 0 | stuff |\n")
	conv.AppendAssistantText("| other | 12 | more stuff |")

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{W: 60, H: 20})
	waitForChatLog(t, log)

	got := chatLogText(log)
	if !strings.Contains(got, "│") {
		t.Fatalf("markdown table did not render with borders:\n%s", got)
	}
	if strings.Contains(got, "|---|") {
		t.Fatalf("table delimiter row rendered literally:\n%s", got)
	}
}

// TestChatLogIncrementalRenderMatchesFullRenderWithTable verifies the
// incremental streaming tail render agrees with the full render when a table
// grows across deltas.
func TestChatLogIncrementalRenderMatchesFullRenderWithTable(t *testing.T) {
	log := NewChatLog(NewConversationModel())
	t.Cleanup(func() { _ = log.Close() })

	previousMessages := []Message{
		{Role: RoleUser, Text: "table please"},
		{Role: RoleAssistant, Text: "Intro:\n\n| a | b |\n|---|---|\n| 1 |", Streaming: true},
	}
	const width = 40
	previous := log.wrapMessages(context.Background(), previousMessages, width)

	messages := append([]Message(nil), previousMessages...)
	messages[1].Text += " two |\n| 3 | three |"
	incremental, ok := log.tryIncrementalRender(&previous, messages, width)
	if !ok {
		t.Fatal("append-only streaming assistant update with table did not use incremental render")
	}
	full := log.wrapMessages(context.Background(), messages, width)
	if !reflect.DeepEqual(incremental, full) {
		t.Fatalf("incremental render differs from full render:\nincremental=%#v\nfull=%#v", incremental, full)
	}
	if !strings.Contains(strings.Join(renderLineTexts(full.lines), "\n"), "│ a") {
		t.Fatalf("table did not render with borders:\n%s", strings.Join(renderLineTexts(full.lines), "\n"))
	}
}

func TestGroupAssistantMarkdownBlocks(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  []string
	}{
		{
			name:  "plain lines stay individual",
			lines: []string{"one", "", "two"},
			want:  []string{"one", "", "two"},
		},
		{
			name:  "table groups into one block",
			lines: []string{"| a | b |", "|---|---|", "| 1 | 2 |"},
			want:  []string{"| a | b |\n|---|---|\n| 1 | 2 |"},
		},
		{
			name:  "table ends at blank line",
			lines: []string{"| a | b |", "|---|---|", "| 1 | 2 |", "", "after"},
			want:  []string{"| a | b |\n|---|---|\n| 1 | 2 |", "", "after"},
		},
		{
			name:  "table ends at pipe-less line",
			lines: []string{"| a | b |", "|---|---|", "no pipes here", "still none"},
			want:  []string{"| a | b |\n|---|---|", "no pipes here", "still none"},
		},
		{
			name:  "table ends at fence line",
			lines: []string{"| a | b |", "|---|---|", "```go", "code"},
			want:  []string{"| a | b |\n|---|---|", "```go", "code"},
		},
		{
			name:  "no delimiter means no table",
			lines: []string{"| a | b |", "| 1 | 2 |"},
			want:  []string{"| a | b |", "| 1 | 2 |"},
		},
		{
			name:  "header as last line is not a table",
			lines: []string{"intro", "| a | b |"},
			want:  []string{"intro", "| a | b |"},
		},
		{
			name:  "lines inside a fence never group",
			lines: []string{"```text", "| a | b |", "|---|---|", "| 1 | 2 |", "```"},
			want:  []string{"```text", "| a | b |", "|---|---|", "| 1 | 2 |", "```"},
		},
		{
			name:  "table after a fence groups",
			lines: []string{"```text", "x", "```", "| a | b |", "|---|---|", "| 1 | 2 |"},
			want:  []string{"```text", "x", "```", "| a | b |\n|---|---|\n| 1 | 2 |"},
		},
		{
			name:  "alignment delimiters accepted",
			lines: []string{"| a | b | c |", "|:--|--:|:-:|", "| 1 | 2 | 3 |"},
			want:  []string{"| a | b | c |\n|:--|--:|:-:|\n| 1 | 2 | 3 |"},
		},
		{
			name:  "delimiter without leading or trailing pipe",
			lines: []string{"a | b", "--- | ---", "1 | 2"},
			want:  []string{"a | b\n--- | ---\n1 | 2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := groupAssistantMarkdownBlocks(tt.lines)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("groupAssistantMarkdownBlocks(%q) = %q, want %q", tt.lines, got, tt.want)
			}
		})
	}
}

func TestIsAssistantTableDelimiter(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"|---|---|", true},
		{"| --- | --- |", true},
		{"|:--|--:|:-:|", true},
		{"---|---", true},
		{":-|-:", true},
		{"|---|", true},
		{"| - |", true},
		{"", false},
		{"---", false},         // setext underline / thematic break
		{"|a|b|", false},       // letters
		{"| -- x -- |", false}, // letter in cell
		{"||", false},          // no cells
		{"|::|", false},        // no dashes
		{"|a-b|", false},       // letter
		{"|---|---", true},     // missing trailing pipe still delimits
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			if got := isAssistantTableDelimiter(tt.line); got != tt.want {
				t.Fatalf("isAssistantTableDelimiter(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestAssistantTableTailByteStartsAtTableHeader(t *testing.T) {
	// The streaming tail must begin at the table's header line, not its last
	// row, so a newly arrived delimiter row can turn the header paragraph into
	// a table on the next delta.
	log := NewChatLog(NewConversationModel())
	t.Cleanup(func() { _ = log.Close() })
	body := "intro\n| a | b |\n|---|---|\n| 1 | 2 |"
	_, tailStart, tailByte := log.renderAssistantText(context.Background(), body, true, 60)
	if want := "intro\n"; body[:tailByte] != want {
		t.Fatalf("tail prefix = %q, want %q", body[:tailByte], want)
	}
	// Rows before the tail: the spacer and the intro paragraph.
	if want := 2; tailStart != want {
		t.Fatalf("tailStart = %d, want %d", tailStart, want)
	}
}

// TestChatLogTableStreamingMatchesFullRender drives a long streaming sequence
// mixing tables (including inline styles in cells), fenced code, and blank
// lines, asserting every incremental render equals the full render and never
// falls back.
func TestChatLogTableStreamingMatchesFullRender(t *testing.T) {
	for _, look := range []struct{ name, theme string }{
		{name: "pi", theme: ""},
		{name: "tau", theme: "tau"},
	} {
		t.Run(look.name, func(t *testing.T) {
			runTableStreamingEquivalence(t, look.theme)
		})
	}
}

func runTableStreamingEquivalence(t *testing.T, theme string) {
	t.Helper()
	log := NewChatLog(NewConversationModel())
	t.Cleanup(func() { _ = log.Close() })
	if theme != "" {
		log.SetTheme(theme, nil)
	}

	deltas := []string{
		"Before the table.\n\n",
		"| Col A | Col B |\n",
		"|---|---|\n",
		"| one | two |\n",
		"| three | four |\n",
		"| five `code` | **six** |\n",
		"\nAfter the table.\n\n",
		"```go\n",
		"func main() {}\n",
		"```\n",
		"| x | y |\n",
		"|---|---|\n",
		"| tail | table |\n",
	}
	const width = 50
	full := ""
	previous := log.wrapMessages(context.Background(), []Message{{Role: RoleUser, Text: "q"}}, width)
	for _, delta := range deltas {
		full += delta
		messages := []Message{
			{Role: RoleUser, Text: "q"},
			{Role: RoleAssistant, Text: full, Streaming: true},
		}
		incremental, ok := log.tryIncrementalRender(&previous, messages, width)
		if !ok {
			t.Fatalf("delta %q fell back to a full render", delta)
		}
		fullState := log.wrapMessages(context.Background(), messages, width)
		if !reflect.DeepEqual(incremental, fullState) {
			t.Fatalf("incremental render differs from full render after delta %q:\nincremental=%#v\nfull=%#v", delta, incremental, fullState)
		}
		previous = incremental
	}
	rows := tauui.StripANSI(strings.Join(renderLineTexts(previous.lines), "\n"))
	for _, want := range []string{"│ Col A", "│ five code", "After the table.", "│ tail"} {
		if !strings.Contains(rows, want) {
			t.Fatalf("rendered rows missing %q:\n%s", want, rows)
		}
	}
	if theme == "tau" != strings.Contains(rows, "╭") {
		t.Fatalf("table chrome mismatch for theme %q:\n%s", theme, rows)
	}
}

// TestChatLogTableChromeFollowsTheme pins that the tau theme encloses chat
// markdown tables in rounded chrome while other themes keep the plain grid.
func TestChatLogTableChromeFollowsTheme(t *testing.T) {
	const tableBody = "| a | b |\n|---|---|\n| 1 | 2 |"
	for _, tc := range []struct {
		theme    string
		enclosed bool
	}{
		{theme: "", enclosed: false},
		{theme: "pi", enclosed: false},
		{theme: "tau", enclosed: true},
	} {
		t.Run("theme="+tc.theme, func(t *testing.T) {
			log := NewChatLog(NewConversationModel())
			t.Cleanup(func() { _ = log.Close() })
			log.SetTheme(tc.theme, nil)
			messages := []Message{
				{Role: RoleUser, Text: "table please"},
				{Role: RoleAssistant, Text: tableBody},
			}
			rows := strings.Join(renderLineTexts(log.wrapMessages(context.Background(), messages, 60).lines), "\n")
			if strings.Contains(rows, "╭") != tc.enclosed {
				t.Fatalf("theme %q table chrome = %v, want %v:\n%s", tc.theme, !tc.enclosed, tc.enclosed, rows)
			}
			if strings.Contains(rows, "╰") != tc.enclosed {
				t.Fatalf("theme %q bottom rule mismatch:\n%s", tc.theme, rows)
			}
			// The grid itself renders regardless of chrome.
			if !strings.Contains(rows, "│ a") || !strings.Contains(rows, "┼") {
				t.Fatalf("theme %q lost the table grid:\n%s", tc.theme, rows)
			}
		})
	}
}

// clickChatLogRow simulates a plain single click (press + release, no drag) on
// content row y. The release deliberately carries no ClickCount: rovel only
// populates it on press, so the release-side count is zero in production.
func clickChatLogRow(t *testing.T, log *ChatLog, y int) {
	t.Helper()
	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}
	if !log.Handle(tauui.MouseEvent{X: 2, Y: y, Button: tauui.MouseButtonLeft, Action: tauui.MousePress, ClickCount: 1}, ctx) {
		t.Fatal("mouse press was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: 2, Y: y, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("mouse release was not handled")
	}
}

func chatLogJoinedRows(t *testing.T, log *ChatLog) string {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	return strings.Join(renderLineTexts(log.lines), "\n")
}

// TestChatLogUserMessageClickTogglesExpansion verifies that clicking a
// truncated user message expands it in place and clicking it again restores
// the capped preview.
func TestChatLogUserMessageClickTogglesExpansion(t *testing.T) {
	conv := NewConversationModel()
	var b strings.Builder
	for i := range maxUserMessageRenderLines + 10 {
		fmt.Fprintf(&b, "line-%02d\n", i)
	}
	conv.AddUserMessage(b.String())

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 200})
	waitForChatLog(t, log)

	if joined := chatLogJoinedRows(t, log); !strings.Contains(joined, userMessageTruncatedText) || strings.Contains(joined, "line-29") {
		t.Fatalf("collapsed preview wrong:\n%s", joined)
	}

	// Dragging a selection across the message must not toggle it.
	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}
	if !log.Handle(tauui.MouseEvent{X: 2, Y: 5, Button: tauui.MouseButtonLeft, Action: tauui.MousePress}, ctx) ||
		!log.Handle(tauui.MouseEvent{X: 12, Y: 5, Button: tauui.MouseButtonLeft, Action: tauui.MouseDrag}, ctx) ||
		!log.Handle(tauui.MouseEvent{X: 12, Y: 5, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("drag selection was not handled")
	}
	if joined := chatLogJoinedRows(t, log); !strings.Contains(joined, userMessageTruncatedText) {
		t.Fatalf("drag selection unexpectedly toggled expansion:\n%s", joined)
	}

	clickChatLogRow(t, log, 5)
	waitForChatLog(t, log)

	if joined := chatLogJoinedRows(t, log); !strings.Contains(joined, "line-29") || !strings.Contains(joined, collapseHintText) || strings.Contains(joined, userMessageTruncatedText) {
		t.Fatalf("expanded message wrong:\n%s", joined)
	}

	clickChatLogRow(t, log, 5)
	waitForChatLog(t, log)

	if joined := chatLogJoinedRows(t, log); !strings.Contains(joined, userMessageTruncatedText) || strings.Contains(joined, "line-29") {
		t.Fatalf("re-collapsed preview wrong:\n%s", joined)
	}
}

// TestChatLogUserMessageClickShortMessageDoesNotExpand verifies that clicking
// a user message within the render cap is a no-op: nothing is hidden, so no
// collapse hint may appear.
func TestChatLogUserMessageClickShortMessageDoesNotExpand(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("short message")

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 20})
	waitForChatLog(t, log)

	revision := log.Revision()
	clickChatLogRow(t, log, 1)

	if got := log.Revision(); got != revision {
		t.Fatalf("revision = %d after clicking short message, want unchanged %d", got, revision)
	}
	if joined := chatLogJoinedRows(t, log); strings.Contains(joined, collapseHintText) {
		t.Fatalf("short message grew a collapse hint:\n%s", joined)
	}
}

// TestChatLogUserMessageExpansionSurvivesStreaming verifies the expanded state
// is keyed per message: an assistant streaming update after an expansion must
// not collapse the expanded user message back to its preview.
func TestChatLogUserMessageExpansionSurvivesStreaming(t *testing.T) {
	conv := NewConversationModel()
	var b strings.Builder
	for i := range maxUserMessageRenderLines + 10 {
		fmt.Fprintf(&b, "line-%02d\n", i)
	}
	conv.AddUserMessage(b.String())

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 200})
	waitForChatLog(t, log)

	clickChatLogRow(t, log, 5)
	waitForChatLog(t, log)

	conv.BeginAssistantMessage()
	conv.AppendAssistantText("streaming reply")
	waitForChatLog(t, log)

	if joined := chatLogJoinedRows(t, log); !strings.Contains(joined, "line-29") {
		t.Fatalf("streaming update collapsed the expanded user message:\n%s", joined)
	}
	if !strings.Contains(chatLogJoinedRows(t, log), "streaming reply") {
		t.Fatal("assistant stream text missing")
	}
}

// chatLogComponentRow returns a content row inside the rendered span of the
// first component of the given kind, for tests that click a specific component.
func chatLogComponentRow(t *testing.T, log *ChatLog, kind ChatComponentKind) int {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	for i := range log.renderedState.components {
		if log.renderedState.components[i].Kind == kind {
			span := log.renderedState.spans[i]
			return span.start + (span.end-span.start)/2
		}
	}
	t.Fatalf("no component of kind %q in the rendered state", kind)
	return 0
}

func bashOutputOfLines(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "out-%02d\n", i)
	}
	return b.String()
}

// TestChatLogBashToolClickTogglesExpansion verifies that clicking a bash box
// whose output is over the preview cap reveals the full output plus a collapse
// hint, and clicking again restores the capped preview.
func TestChatLogBashToolClickTogglesExpansion(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("call-1", "bash", `{"command":"ls"}`)
	conv.AddToolResultWithID("call-1", bashOutputOfLines(bashToolPreviewLines+10), false)

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 200})
	waitForChatLog(t, log)

	toolRow := chatLogComponentRow(t, log, ChatComponentToolExecution)
	if joined := chatLogJoinedRows(t, log); !strings.Contains(joined, bashTruncatedText) || strings.Contains(joined, "out-29") {
		t.Fatalf("collapsed bash preview wrong:\n%s", joined)
	}

	clickChatLogRow(t, log, toolRow)
	waitForChatLog(t, log)

	if joined := chatLogJoinedRows(t, log); !strings.Contains(joined, "out-29") || !strings.Contains(joined, collapseHintText) || strings.Contains(joined, bashTruncatedText) {
		t.Fatalf("expanded bash output wrong:\n%s", joined)
	}

	clickChatLogRow(t, log, toolRow)
	waitForChatLog(t, log)

	if joined := chatLogJoinedRows(t, log); !strings.Contains(joined, bashTruncatedText) || strings.Contains(joined, "out-29") {
		t.Fatalf("re-collapsed bash preview wrong:\n%s", joined)
	}
}

// TestChatLogShortBashToolClickDoesNotExpand verifies that clicking a bash box
// within the preview cap is a no-op.
func TestChatLogShortBashToolClickDoesNotExpand(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("call-1", "bash", `{"command":"ls"}`)
	conv.AddToolResultWithID("call-1", "one\ntwo", false)

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 20})
	waitForChatLog(t, log)

	revision := log.Revision()
	clickChatLogRow(t, log, chatLogComponentRow(t, log, ChatComponentToolExecution))

	if got := log.Revision(); got != revision {
		t.Fatalf("revision = %d after clicking short bash box, want unchanged %d", got, revision)
	}
	if joined := chatLogJoinedRows(t, log); strings.Contains(joined, collapseHintText) {
		t.Fatalf("short bash box grew a collapse hint:\n%s", joined)
	}
}

// TestChatLogEditDiffClickTogglesExpansion verifies that clicking an edit box
// whose compact diff dropped rows reveals the full diff, and clicking again
// restores the compact window.
func TestChatLogEditDiffClickTogglesExpansion(t *testing.T) {
	var lines []string
	for i := range 10 {
		lines = append(lines, fmt.Sprintf(" ctx-%02d", i))
	}
	lines = append(lines, "-old", "+new")
	for i := range 10 {
		lines = append(lines, fmt.Sprintf(" ctx-b%02d", i))
	}
	diff := strings.Join(lines, "\n")

	conv := NewConversationModel()
	conv.AddToolCallWithID("call-1", "edit", `{"path":"f.go"}`)
	conv.AddToolResultWithDetails("call-1", "", "ok", false, map[string]any{"diff": diff})
	if !compactEditDiffTruncated(diff) {
		t.Fatal("test diff is not compacted; test is invalid")
	}

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 200})
	waitForChatLog(t, log)

	if joined := chatLogJoinedRows(t, log); strings.Contains(joined, "ctx-00") || !strings.Contains(joined, "ctx-06") || !strings.Contains(joined, "     ...") {
		t.Fatalf("expected compacted diff with hidden rows:\n%s", joined)
	}

	clickChatLogRow(t, log, chatLogComponentRow(t, log, ChatComponentToolExecution))
	waitForChatLog(t, log)

	if joined := chatLogJoinedRows(t, log); !strings.Contains(joined, "ctx-b09") || !strings.Contains(joined, collapseHintText) {
		t.Fatalf("expanded diff missing tail rows or collapse hint:\n%s", joined)
	}

	clickChatLogRow(t, log, chatLogComponentRow(t, log, ChatComponentToolExecution))
	waitForChatLog(t, log)

	if joined := chatLogJoinedRows(t, log); !strings.Contains(joined, "     ...") || strings.Contains(joined, "ctx-b09") {
		t.Fatalf("re-collapsed diff wrong:\n%s", joined)
	}
}

// TestChatLogWritePreviewClickTogglesExpansion verifies the same toggle for a
// write box whose content preview is capped.
func TestChatLogWritePreviewClickTogglesExpansion(t *testing.T) {
	var b strings.Builder
	for i := range writeContentPreviewLines + 5 {
		fmt.Fprintf(&b, "content-%02d\n", i)
	}
	conv := NewConversationModel()
	conv.AddToolCallWithID("call-1", "write", `{"path":"f.go"}`)
	conv.AddToolResultWithName("call-1", "write", "ok", false)
	// The write preview comes from args; re-add the call with full content.
	conv.AddToolCallWithID("call-2", "write", fmt.Sprintf(`{"path":"g.go","content":%q}`, strings.TrimRight(b.String(), "\n")))

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 200})
	waitForChatLog(t, log)

	rows := chatLogJoinedRows(t, log)
	if !strings.Contains(rows, "content-09") || strings.Contains(rows, "content-14") || !strings.Contains(rows, "... (5 more lines)") {
		t.Fatalf("collapsed write preview wrong:\n%s", rows)
	}

	// Two write boxes exist; click the second (call-2). Its span is the second
	// tool-execution component.
	log.mu.Lock()
	var secondToolRow int
	found := 0
	for i := range log.renderedState.components {
		if log.renderedState.components[i].Kind == ChatComponentToolExecution {
			found++
			if found == 2 {
				span := log.renderedState.spans[i]
				secondToolRow = span.start + (span.end-span.start)/2
			}
		}
	}
	log.mu.Unlock()
	if found < 2 {
		t.Fatalf("found %d tool components, want 2", found)
	}

	clickChatLogRow(t, log, secondToolRow)
	waitForChatLog(t, log)

	if joined := chatLogJoinedRows(t, log); !strings.Contains(joined, "content-14") || !strings.Contains(joined, collapseHintText) {
		t.Fatalf("expanded write preview wrong:\n%s", joined)
	}
}

// TestChatLogToolClickNotifiesExpansionHandler verifies the plugin-facing hook
// observes the same toggle the chat log renders, keyed by call ID.
func TestChatLogToolClickNotifiesExpansionHandler(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("call-1", "bash", `{"command":"ls"}`)
	conv.AddToolResultWithID("call-1", bashOutputOfLines(bashToolPreviewLines+5), false)

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 200})
	waitForChatLog(t, log)

	var gotID string
	var gotExpanded bool
	log.SetToolExpansionHandler(func(id string, expanded bool) { gotID, gotExpanded = id, expanded })

	clickChatLogRow(t, log, chatLogComponentRow(t, log, ChatComponentToolExecution))
	waitForChatLog(t, log)

	if gotID != "call-1" || !gotExpanded {
		t.Fatalf("expansion handler observed (%q, %v), want (call-1, true)", gotID, gotExpanded)
	}
}
