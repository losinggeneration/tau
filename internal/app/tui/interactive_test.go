package tui

import (
	"strings"
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
)

// TestInteractiveCommands_HelpContainsAllCommands verifies /help
// lists every registered command in the chat log.
func TestInteractiveCommands_HelpContainsAllCommands(t *testing.T) {
	app := newTestApp(t)
	cmdHelp(app, "")
	msgs := app.conv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	text := msgs[0].Text
	for _, cmd := range app.cmds.All() {
		if !strings.Contains(text, "/"+cmd.Name) {
			t.Errorf("/help missing /%s in:\n%s", cmd.Name, text)
		}
	}
}

// TestInteractiveCommands_HotkeysContainsKeybindings verifies
// /hotkeys shows all expected keybinding hints.
func TestInteractiveCommands_HotkeysContainsKeybindings(t *testing.T) {
	app := newTestApp(t)
	app.conv.Clear()
	cmdHotkeys(app, "")
	msgs := app.conv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	text := msgs[0].Text
	// Configurable actions are named with the registry's own key spelling —
	// the same strings a user writes in keybindings.json — so the hint and the
	// config file agree.
	required := []string{
		"Keybindings:",
		"ctrl+c",
		"ctrl+p",
		"tab",
		"enter",
		"shift-enter",
		"alt+backspace",
		"esc",
	}
	for _, want := range required {
		if !strings.Contains(text, want) {
			t.Errorf("/hotkeys missing %q in:\n%s", want, text)
		}
	}
}

// TestInteractiveCommands_CopyLast verifies /copy last attempts to
// copy the last assistant message. Since clipboard is not available
// in unit tests, we verify the chat-log error is surfaced.
func TestInteractiveCommands_CopyLastNoClipboard(t *testing.T) {
	app := newTestApp(t)
	app.conv.AddAssistantMessage("Hello from assistant")
	app.conv.Clear()
	app.conv.AddAssistantMessage("Hello again")
	cmdCopy(app, "last")
	msgs := app.conv.Messages()
	// cmdCopy should either surface a "copied" message or an error
	// about clipboard being unavailable.
	found := false
	for _, msg := range msgs {
		if strings.Contains(msg.Text, "copy:") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'copy:' in messages after /copy last, got: %+v", msgs)
	}
}

// TestInteractiveCommands_ShowModel verifies /show-model surfaces the
// current model in the chat log.
func TestInteractiveCommands_ShowModel(t *testing.T) {
	app := newTestApp(t)
	fs := app.sess.(*fakeSession)
	fs.stateModel = "anthropic/claude-sonnet-4-6"
	app.conv.Clear()
	cmdShowModel(app, "")
	msgs := app.conv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "claude-sonnet-4-6") {
		t.Errorf("/show-model should mention the model: %q", msgs[0].Text)
	}
}

// TestInteractiveCommands_StreamingState verifies setStreaming
// toggles the streaming indicator and that the footer reflects it.
func TestInteractiveCommands_StreamingState(t *testing.T) {
	app := newTestApp(t)
	app.footer = newFooterForTest()
	app.ctx = nil // ensure loopContext is nil-safe

	app.setStreaming(true)
	if !app.isStreaming() {
		t.Error("app should be streaming after setStreaming(true)")
	}
	if !app.footer.data.streaming {
		t.Error("footer should show streaming=true")
	}
	if app.footer.data.workingFrame == "" {
		t.Error("footer should have a non-empty working frame")
	}

	app.setStreaming(false)
	if app.isStreaming() {
		t.Error("app should not be streaming after setStreaming(false)")
	}
	if app.footer.data.streaming {
		t.Error("footer should show streaming=false")
	}
	if app.footer.data.workingFrame != "" {
		t.Error("footer should have empty working frame when not streaming")
	}
}

// TestInteractiveCommands_Interrupt verifies that consuming
// streaming for interrupt calls Interrupt on the session and
// clears streaming state.
func TestInteractiveCommands_Interrupt(t *testing.T) {
	app := newTestApp(t)
	app.footer = newFooterForTest()
	fs := app.sess.(*fakeSession)

	// Need to be streaming to interrupt
	app.setStreaming(true)
	if !app.consumeStreamingForInterrupt() {
		t.Fatal("consumeStreamingForInterrupt should return true when streaming")
	}
	if app.isStreaming() {
		t.Error("should not be streaming after interrupt")
	}
	fs.mu.Lock()
	if fs.interrupts != 1 {
		t.Errorf("Interrupt calls = %d, want 1", fs.interrupts)
	}
	fs.mu.Unlock()
}

// TestInteractiveCommands_InterruptWhenNotStreaming verifies that
// interrupt is a no-op when not streaming.
func TestInteractiveCommands_InterruptWhenNotStreaming(t *testing.T) {
	app := newTestApp(t)
	fs := app.sess.(*fakeSession)

	if app.consumeStreamingForInterrupt() {
		t.Error("consumeStreamingForInterrupt should return false when not streaming")
	}
	fs.mu.Lock()
	if fs.interrupts != 0 {
		t.Errorf("Interrupt calls = %d, want 0", fs.interrupts)
	}
	fs.mu.Unlock()
}

// TestInteractiveCommands_QueueFollowUp verifies that enqueueFollowUp
// adds to the pending queue and syncs the mirror.
func TestInteractiveCommands_QueueFollowUp(t *testing.T) {
	app := newTestApp(t)
	app.pending = NewPendingView()
	app.pendingQueue = nil

	app.enqueueFollowUp("task 1", nil)
	if len(app.pendingQueue) != 1 {
		t.Fatalf("pendingQueue len = %d, want 1", len(app.pendingQueue))
	}
	if app.pendingQueue[0] != "task 1" {
		t.Errorf("pendingQueue[0] = %q, want 'task 1'", app.pendingQueue[0])
	}

	app.enqueueFollowUp("task 2", nil)
	if len(app.pendingQueue) != 2 {
		t.Fatalf("pendingQueue len = %d, want 2", len(app.pendingQueue))
	}
}

// TestInteractiveCommands_DequeueFollowUp verifies dequeuing removes
// the first queued message.
func TestInteractiveCommands_DequeueFollowUp(t *testing.T) {
	app := newTestApp(t)
	app.pending = NewPendingView()
	app.pendingQueue = []string{"task 1", "task 2"}

	app.dequeueFollowUp(nil)
	if len(app.pendingQueue) != 1 {
		t.Fatalf("pendingQueue len = %d, want 1", len(app.pendingQueue))
	}
	if app.pendingQueue[0] != "task 2" {
		t.Errorf("pendingQueue[0] = %q, want 'task 2'", app.pendingQueue[0])
	}

	app.dequeueFollowUp(nil)
	if len(app.pendingQueue) != 0 {
		t.Fatalf("pendingQueue len = %d, want 0", len(app.pendingQueue))
	}

	// Dequeuing empty queue should be a no-op
	app.dequeueFollowUp(nil)
	if len(app.pendingQueue) != 0 {
		t.Errorf("pendingQueue len = %d, want 0", len(app.pendingQueue))
	}
}

// TestInteractiveCommands_Compact verifies /compact invokes the
// session's Compact method and surfaces a success message.
func TestInteractiveCommands_Compact(t *testing.T) {
	app := newTestApp(t)
	app.conv.Clear()
	cmdCompact(app, "")
	msgs := app.conv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "compact") {
		t.Errorf("/compact should mention 'compact': %q", msgs[0].Text)
	}
}

// TestInteractiveCommands_ExportStub verifies /export without args
// surfaces the coming-soon notice.
func TestInteractiveCommands_ExportStub(t *testing.T) {
	app := newTestApp(t)
	app.conv.Clear()
	cmdExportOrStub(app, "")
	msgs := app.conv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "export") {
		t.Errorf("export stub should mention 'export': %q", msgs[0].Text)
	}
}

// TestInteractiveCommands_ExportWithArgs verifies /export with a
// file path attempts to write the transcript.
func TestInteractiveCommands_ExportWithArgs(t *testing.T) {
	app := newTestApp(t)
	app.conv.Clear()
	app.conv.AddUserMessage("hello")
	app.conv.AddAssistantMessage("world")

	tmpPath := t.TempDir() + "/export.md"
	cmdExport(app, "markdown "+tmpPath)
	msgs := app.conv.Messages()
	found := false
	for _, msg := range msgs {
		if strings.Contains(msg.Text, "export:") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("export should mention 'export:': %+v", msgs)
	}
}

// TestInteractiveCommands_ForkStub verifies /fork without args
// surfaces the coming-soon notice.
func TestInteractiveCommands_ForkStub(t *testing.T) {
	app := newTestApp(t)
	app.conv.Clear()
	cmdForkOrStub(app, "")
	msgs := app.conv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "fork") {
		t.Errorf("fork stub should mention 'fork': %q", msgs[0].Text)
	}
}

// TestInteractiveCommands_TreeStub verifies /tree without args
// surfaces the coming-soon notice.
func TestInteractiveCommands_TreeStub(t *testing.T) {
	app := newTestApp(t)
	app.conv.Clear()
	cmdTreeOrStub(app, "")
	msgs := app.conv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "tree") {
		t.Errorf("tree stub should mention 'tree': %q", msgs[0].Text)
	}
}

// TestInteractiveCommands_ToolRunning verifies the tool-running
// indicator can be set and cleared.
func TestInteractiveCommands_ToolRunning(t *testing.T) {
	app := newTestApp(t)
	app.footer = newFooterForTest()

	app.setToolRunning(true)
	if !app.footer.data.toolRunning {
		t.Error("footer should show toolRunning=true")
	}

	app.setToolRunning(false)
	if app.footer.data.toolRunning {
		t.Error("footer should show toolRunning=false")
	}
}

// TestInteractiveCommands_FooterThinkingLevel verifies the footer
// displays the thinking level from session stats.
func TestInteractiveCommands_FooterThinkingLevel(t *testing.T) {
	app := newTestApp(t)
	app.footer = newFooterForTest()

	// Update the fake session's stats to include thinking level
	// (The fakeSession doesn't have a thinkingLevel field, but the
	// footer data does and is populated by refreshFooterStats.)
	// We'll set it directly for this test.
	app.footer.data.thinkingLevel = "high"
	// §10.3 only appends a thinking suffix for models that reason.
	app.footer.data.supportsReasoning = true
	output, _ := app.footer.render(120)
	if !strings.Contains(output, " • high") {
		t.Errorf("footer should contain ' • high': %q", output)
	}
}

// TestInteractiveCommands_FooterQueueCount verifies the footer
// displays the queue count.
func TestInteractiveCommands_FooterQueueCount(t *testing.T) {
	app := newTestApp(t)
	app.footer = newFooterForTest()
	app.footer.data.queueCount = 5
	output, _ := app.footer.render(80)
	if !strings.Contains(output, "5 queued") {
		t.Errorf("footer should contain '5 queued': %q", output)
	}
}

// TestInteractiveCommands_StatusAddAndClear verifies setStatus and
// clearStatus can add/remove entries from the status view.
func TestInteractiveCommands_StatusAddAndClear(t *testing.T) {
	app := newTestApp(t)
	app.status = tauui.NewStatusEntriesView(5)

	app.setStatus("github-ci", "building")
	if len(app.statusEntries) != 1 {
		t.Fatalf("statusEntries len = %d, want 1", len(app.statusEntries))
	}
	if app.statusEntries["github-ci"] != "building" {
		t.Errorf("statusEntries['github-ci'] = %q, want 'building'", app.statusEntries["github-ci"])
	}

	app.clearStatus("github-ci")
	if len(app.statusEntries) != 0 {
		t.Fatalf("statusEntries len = %d, want 0 after clear", len(app.statusEntries))
	}
}

// TestInteractiveCommands_StatusWithNewlines verifies that multiline
// status text is sanitized to a single line.
func TestInteractiveCommands_StatusWithNewlines(t *testing.T) {
	app := newTestApp(t)
	app.status = tauui.NewStatusEntriesView(5)

	app.setStatus("test", "line1\nline2\ttabbed")
	if app.statusEntries["test"] != "line1 line2 tabbed" {
		t.Errorf("sanitized status = %q, want 'line1 line2 tabbed'", app.statusEntries["test"])
	}
}

// TestInteractiveCommands_WidgetPlacement verifies that widgets can
// be added above and below the editor.
func TestInteractiveCommands_WidgetPlacement(t *testing.T) {
	app := newTestApp(t)
	app.widgetsAbove = NewWidgetContainer(widgetPlacementAbove)
	app.widgetsBelow = NewWidgetContainer(widgetPlacementBelow)

	app.setWidget("w1", []string{"above widget content"}, "above")
	if len(app.widgetEntriesAbove) != 1 {
		t.Fatalf("widgetEntriesAbove len = %d, want 1", len(app.widgetEntriesAbove))
	}

	app.setWidget("w2", []string{"below widget content"}, "below")
	if len(app.widgetEntriesBelow) != 1 {
		t.Fatalf("widgetEntriesBelow len = %d, want 1", len(app.widgetEntriesBelow))
	}

	// Remove a widget by passing nil
	app.setWidget("w1", nil, "above")
	if len(app.widgetEntriesAbove) != 0 {
		t.Errorf("widgetEntriesAbove len = %d, want 0 after nil", len(app.widgetEntriesAbove))
	}
}

// TestInteractiveCommands_ConfirmQuit verifies the confirm-to-quit
// flow with an empty editor.
func TestInteractiveCommands_ConfirmQuit(t *testing.T) {
	app := newTestApp(t)
	app.editor = newPromptEditorForTest()
	app.footer = newFooterForTest()
	ctx := testCtx()
	ctx.FocusedID = app.editor.ID()

	// First Ctrl+C with empty editor: should set confirm notice
	app.confirmOrQuit(ctx)
	if app.footer.data.notice == "" {
		t.Error("footer should have a confirm notice after first Ctrl+C with empty editor")
	}
}
