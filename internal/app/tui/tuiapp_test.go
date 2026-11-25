package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tauui "github.com/losinggeneration/tau/tui"

	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/tui/widgets"
)

func TestConversationModelSnapshotRevisionsTrackMutations(t *testing.T) {
	conv := NewConversationModel()
	initial := conv.Snapshot()
	if initial.Revision != 0 || len(initial.Components) != 0 {
		t.Fatalf("initial snapshot = %#v, want empty revision 0", initial)
	}

	conv.AddUserMessage("hello")
	afterUser := conv.Snapshot()
	if afterUser.Revision <= initial.Revision || len(afterUser.Components) != 1 || afterUser.Components[0].Revision == 0 {
		t.Fatalf("after user snapshot = %#v", afterUser)
	}
	userID := afterUser.Components[0].ID
	userRev := afterUser.Components[0].Revision

	conv.BeginAssistantMessageWithID("a1")
	afterStart := conv.Snapshot()
	if afterStart.Revision <= afterUser.Revision || len(afterStart.Components) != 2 {
		t.Fatalf("assistant start snapshot = %#v", afterStart)
	}
	if afterStart.Components[0].ID != userID || afterStart.Components[0].Revision != userRev {
		t.Fatalf("unchanged user component revision changed: before=%#v after=%#v", afterUser.Components[0], afterStart.Components[0])
	}
	assistantRev := afterStart.Components[1].Revision

	conv.AppendAssistantText("hi")
	afterDelta := conv.Snapshot()
	if afterDelta.Revision <= afterStart.Revision || afterDelta.Components[1].Revision <= assistantRev {
		t.Fatalf("assistant delta did not advance revisions: before=%#v after=%#v", afterStart, afterDelta)
	}
	if afterDelta.Components[0].Revision != userRev {
		t.Fatalf("assistant delta changed user revision: %#v", afterDelta.Components[0])
	}

	conv.EndAssistantMessage()
	afterEnd := conv.Snapshot()
	if afterEnd.Revision <= afterDelta.Revision || afterEnd.Components[1].Streaming || afterEnd.Components[1].Revision <= afterDelta.Components[1].Revision {
		t.Fatalf("assistant end snapshot = %#v, before=%#v", afterEnd, afterDelta)
	}
}

func TestConversationModelClearThinkingAdvancesSnapshotRevision(t *testing.T) {
	conv := NewConversationModel()
	conv.AddSystem("history")
	conv.AddThinking()
	withThinking := conv.Snapshot()
	if len(withThinking.Components) != 2 || withThinking.Components[1].Role != RoleThinking {
		t.Fatalf("thinking snapshot = %#v", withThinking)
	}
	historyRev := withThinking.Components[0].Revision

	conv.ClearThinking()
	withoutThinking := conv.Snapshot()
	if withoutThinking.Revision <= withThinking.Revision {
		t.Fatalf("ClearThinking did not advance snapshot revision: before=%#v after=%#v", withThinking, withoutThinking)
	}
	if len(withoutThinking.Components) != 1 || withoutThinking.Components[0].Revision != historyRev {
		t.Fatalf("ClearThinking changed retained history component: %#v", withoutThinking)
	}
}

func TestConversationModelToolProgressAdvancesOnlyToolRevision(t *testing.T) {
	conv := NewConversationModel()
	conv.AddSystem("history")
	conv.AddToolCallWithID("tool-1", "search", `{"q":"tau"}`)
	before := conv.Snapshot()
	historyRev := before.Components[0].Revision
	toolRev := before.Components[1].Revision

	conv.AddToolProgress("tool-1", "running")
	afterProgress := conv.Snapshot()
	if afterProgress.Revision <= before.Revision || afterProgress.Components[1].Revision <= toolRev {
		t.Fatalf("tool progress did not advance tool revisions: before=%#v after=%#v", before, afterProgress)
	}
	if afterProgress.Components[0].Revision != historyRev {
		t.Fatalf("tool progress changed history revision: %#v", afterProgress.Components[0])
	}

	progressRev := afterProgress.Components[1].Revision
	conv.AddToolResultWithID("tool-1", "done", false)
	afterResult := conv.Snapshot()
	if afterResult.Revision <= afterProgress.Revision || afterResult.Components[1].Revision <= progressRev {
		t.Fatalf("tool result did not advance tool revision: before=%#v after=%#v", afterProgress, afterResult)
	}
}

func TestConversationModelProjectsStructuredToolExecutionComponent(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("tool-1", "bash", `{"command":"echo hi"}`)
	conv.AddToolProgress("tool-1", "running")
	conv.AddToolResultWithID("tool-1", "hi", false)

	snapshot := conv.Snapshot()
	if len(snapshot.Components) != 1 {
		t.Fatalf("components length = %d, want 1", len(snapshot.Components))
	}
	component := snapshot.Components[0]
	if component.Kind != ChatComponentToolExecution {
		t.Fatalf("component kind = %q, want %q", component.Kind, ChatComponentToolExecution)
	}
	tool := component.ToolExecution
	if tool.CallID != "tool-1" || tool.Name != "bash" || tool.Args != `{"command":"echo hi"}` || tool.Output != "hi" || tool.State != toolExecutionComplete || tool.IsError {
		t.Fatalf("tool payload = %#v", tool)
	}

	msg := component.Message()
	if msg.Role != RoleToolCall || msg.ToolCallID != "tool-1" || msg.ToolName != "bash" || msg.ToolArgs != `{"command":"echo hi"}` || msg.Text != "bash {\"command\":\"echo hi\"}\nhi" {
		t.Fatalf("legacy message shim = %#v", msg)
	}
}

func TestChatLogRendersStructuredToolExecutionComponent(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("tool-1", "bash", `{"command":"printf 'one\\ntwo\\n'"}`)
	conv.AddToolResultWithID("tool-1", "one\ntwo", false)
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 10})
	waitForChatLog(t, log)

	log.mu.Lock()
	var got []string
	for _, line := range log.lines {
		got = append(got, line.text)
	}
	log.mu.Unlock()
	joined := strings.Join(got, "\n")
	for _, want := range []string{" $ printf 'one\\ntwo\\n'", " one", " two"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rendered tool output missing %q from:\n%s", want, joined)
		}
	}
}

func TestChatLogSummarizesReadToolWithoutFileContents(t *testing.T) {
	var fileLines []string
	for i := 1; i <= 25; i++ {
		fileLines = append(fileLines, fmt.Sprintf("file line %02d", i))
	}

	conv := NewConversationModel()
	conv.AddToolCallWithID("tool-1", "read", `{"path":"large.txt","start_line":1,"limit":25}`)
	conv.AddToolResultWithID("tool-1", strings.Join(fileLines, "\n"), false)
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 30})
	waitForChatLog(t, log)

	log.mu.Lock()
	joined := strings.Join(renderLineTexts(log.lines), "\n")
	log.mu.Unlock()
	if !strings.Contains(joined, "read large.txt:1-25") {
		t.Fatalf("read call summary missing range:\n%s", joined)
	}
	if strings.Contains(joined, "file line 01") || strings.Contains(joined, "file line 20") {
		t.Fatalf("read tool should not echo file contents in chatlog:\n%s", joined)
	}
}

func TestChatLogTauLookRendersBashToolChrome(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("tool-1", "bash", `{"command":"echo hi"}`)
	conv.AddToolResultWithID("tool-1", "hi", false)
	log := newTestChatLog(t, conv)
	log.SetLookName("tau")
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 10})
	waitForChatLog(t, log)

	log.mu.Lock()
	joined := strings.Join(renderLineTexts(log.lines), "\n")
	log.mu.Unlock()
	for _, want := range []string{"╭ bash · complete", "╮", "│ $ echo hi", "├", "┤", "│ hi", "╰", "╯"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("tau-look bash chrome missing %q from:\n%s", want, joined)
		}
	}
}

func TestChatLogSetLookNameRerendersWhenThemeNameChangesWithinSameLook(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("tool-1", "bash", `{"command":"echo hi"}`)
	conv.AddToolResultWithID("tool-1", "hi", false)
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 10})
	waitForChatLog(t, log)

	log.mu.Lock()
	before := log.renderedRevision
	log.mu.Unlock()
	log.SetLookName("dark")
	waitForChatLog(t, log)

	log.mu.Lock()
	defer log.mu.Unlock()
	if log.renderedRevision <= before {
		t.Fatalf("theme name change did not schedule a chat rerender: before=%d after=%d", before, log.renderedRevision)
	}
	if log.themeName != "dark" {
		t.Fatalf("themeName = %q, want dark", log.themeName)
	}
}

func TestChatLogSetLookNameClearsRenderCache(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("tool-1", "bash", `{"command":"echo hi"}`)
	conv.AddToolResultWithID("tool-1", "hi", false)
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 10})
	waitForChatLog(t, log)

	log.mu.Lock()
	cacheEntries := len(log.renderCache)
	log.mu.Unlock()
	if cacheEntries == 0 {
		t.Fatal("test requires populated render cache")
	}

	log.SetLookName("dark")
	waitForChatLog(t, log)

	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.renderCacheOrder) == cacheEntries && log.metrics.CacheHits > 0 {
		t.Fatalf("theme change reused old render cache entries")
	}
}

func TestChatLogTauLookTruncatedBashOutputUsesToolBackground(t *testing.T) {
	var output []string
	for i := range bashToolPreviewLines + 1 {
		output = append(output, fmt.Sprintf("line-%02d", i))
	}
	conv := NewConversationModel()
	conv.AddToolCallWithID("tool-1", "bash", `{"command":"seq 25"}`)
	conv.AddToolResultWithID("tool-1", strings.Join(output, "\n"), false)
	log := newTestChatLog(t, conv)
	log.SetLookName("tau")
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 40})
	waitForChatLog(t, log)

	log.mu.Lock()
	defer log.mu.Unlock()
	for _, line := range log.lines {
		if strings.Contains(line.text, "bash output truncated") {
			if line.st.BG != log.pal().toolResult.BG {
				t.Fatalf("truncated bash line BG = %#v, want tool result BG %#v", line.st.BG, log.pal().toolResult.BG)
			}
			return
		}
	}
	t.Fatalf("missing truncated bash output row: %#v", renderLineTexts(log.lines))
}

func TestChatLogRendersEditToolDiffFromDetails(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("tool-1", "edit", `{"path":"test.junk","old_string":"old","new_string":"new"}`)
	diff := strings.Join([]string{
		" 26       in, _ := hex.DecodeString(test.in)",
		" 27       out1, out2 := splitPreMasterSecret(in)",
		" 28       s1 := hex.EncodeToString(out1)",
		" 29       s2 := hex.EncodeToString(out2)",
		"+30       // this is a test",
		" 30       if s1 != test.out1 || s2 != test.out2 {",
		" 31          t.Errorf(\"#%d: got\")",
		" 32       }",
		" 33    }",
	}, "\n")
	conv.AddToolResultWithDetails("tool-1", "", "Successfully replaced 1 block(s) in test.junk.", false, map[string]any{"diff": diff})
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 100, H: 30})
	waitForChatLog(t, log)

	log.mu.Lock()
	joined := strings.Join(renderLineTexts(log.lines), "\n")
	log.mu.Unlock()
	for _, want := range []string{"edit test.junk", "+30       // this is a test", "30       if s1 != test.out1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("edit diff missing %q from:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "Successfully replaced") || strings.Contains(joined, "tool edit") {
		t.Fatalf("edit diff render should replace generic tool output:\n%s", joined)
	}
}

func TestChatLogRendersWriteToolContentPreview(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("tool-1", "write", `{"path":"new.txt","content":"line 01\nline 02\nline 03\nline 04\nline 05\nline 06\nline 07\nline 08\nline 09\nline 10\nline 11\nline 12"}`)
	conv.AddToolResultWithID("tool-1", "Wrote 95 bytes to new.txt", false)
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 30})
	waitForChatLog(t, log)

	log.mu.Lock()
	joined := strings.Join(renderLineTexts(log.lines), "\n")
	log.mu.Unlock()
	for _, want := range []string{"write new.txt", "new file preview:", "line 01", "line 10", "... (2 more lines)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("write preview missing %q from:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "line 11") || strings.Contains(joined, "line 12") || strings.Contains(joined, "Wrote 95 bytes") || strings.Contains(joined, `"content"`) {
		t.Fatalf("write preview should replace generic JSON/result output and truncate content:\n%s", joined)
	}
}

func TestConversationModelStreamingSplit(t *testing.T) {
	m := NewConversationModel()

	// Committed user message.
	if idx := m.AddUserMessage("hello"); idx != 0 {
		t.Fatalf("expected user message at index 0, got %d", idx)
	}

	// Streaming assistant message is distinct from committed history.
	idx := m.BeginAssistantMessage()
	if idx != 1 {
		t.Fatalf("expected assistant at index 1, got %d", idx)
	}
	if !m.IsStreaming() {
		t.Fatal("expected IsStreaming true after BeginAssistantMessage")
	}

	m.AppendAssistantText("Hel")
	m.AppendAssistantText("lo!")
	if got := m.Messages()[1].Text; got != "Hello!" {
		t.Fatalf("expected streamed text 'Hello!', got %q", got)
	}

	m.EndAssistantMessage()
	if m.IsStreaming() {
		t.Fatal("expected IsStreaming false after EndAssistantMessage")
	}
}

func TestChatLogMinSizeTracksWrappedHeight(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("one two three four five six seven eight nine ten")
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("short reply")

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 20, H: 5})
	waitForChatLog(t, log)

	// MinSize().H must equal the wrapped line count (the ScrollView contract).
	want := log.LineCount()
	got := log.MinSize().H
	if got != want {
		t.Fatalf("MinSize().H (%d) must equal LineCount() (%d)", got, want)
	}
	if want <= 2 {
		t.Fatalf("expected wrapping to produce more than 2 lines at width 20, got %d", want)
	}
}

func TestChatLogRewrapsOnWidthChange(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("alpha beta gamma delta epsilon zeta eta theta")
	log := newTestChatLog(t, conv)

	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 5})
	waitForChatLog(t, log)
	narrow := log.LineCount()

	log.Layout(tauui.Rect{X: 0, Y: 0, W: 10, H: 5})
	waitForChatLog(t, log)
	wider := log.LineCount() // wider in line count because narrower columns wrap more

	if wider <= narrow {
		t.Fatalf("expected narrower width to produce more lines (%d > %d)", wider, narrow)
	}
}

func TestChatLogEmptyHasMinimumHeight(t *testing.T) {
	log := newTestChatLog(t, NewConversationModel())
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 20, H: 5})
	waitForChatLog(t, log)
	if got := log.MinSize().H; got < 1 {
		t.Fatalf("expected MinSize().H >= 1, got %d", got)
	}
}

func TestChatLogPiStyleRows(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("Could you list the files in the current directory?")
	conv.BeginAssistantMessage()
	conv.AddThinking()
	conv.AddToolCall("bash", `{"command":"ls -la","cwd":"/tmp"}`)
	conv.AddToolResult("total 3\nold\nagent\nai\ncoding-agent\nmom\npods\ntui\nweb-ui", false)
	conv.SetAssistantTextIfEmpty("Files/directories in the current directory:")
	conv.EndAssistantMessage()

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, log)

	log.mu.Lock()
	var got []string
	for _, ln := range log.lines {
		got = append(got, ln.text)
	}
	log.mu.Unlock()

	joined := strings.Join(got, "\n")
	for _, label := range []string{"you", "assistant", "tool call", "result"} {
		if strings.Contains(joined, label+"\n") || strings.Contains(joined, "\n"+label) {
			t.Fatalf("chat log should not render Tau role label %q in %q", label, joined)
		}
	}
	for _, want := range []string{
		" Could you list the files in the current directory?",
		" $ ls -la",
		" total 3",
		" web-ui",
		" Files/directories in the current directory:",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Pi-style row %q missing from:\n%s", want, joined)
		}
	}
	if got[len(got)-1] != " Files/directories in the current directory:" {
		t.Fatalf("final assistant response should render after tool result, got last row %q", got[len(got)-1])
	}
}

func TestChatLogUserMessageRendersMarkdown(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("# Heading\n**bold** and `code`")

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, log)

	log.mu.Lock()
	defer log.mu.Unlock()
	joined := strings.Join(renderLineTexts(log.lines), "\n")
	for _, want := range []string{"Heading", "bold", "code"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("markdown user content missing %q from:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, " # Heading") || strings.Contains(joined, "**bold**") {
		t.Fatalf("user message should be markdown-rendered, not literal:\n%q", joined)
	}
}

func TestChatLogLongUserMessageIsTruncated(t *testing.T) {
	conv := NewConversationModel()
	var b strings.Builder
	for i := range maxUserMessageRenderLines + 10 {
		fmt.Fprintf(&b, "line-%02d\n", i)
	}
	conv.AddUserMessage(b.String())

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 80})
	waitForChatLog(t, log)

	log.mu.Lock()
	rows := renderLineTexts(log.lines)
	log.mu.Unlock()

	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "... (user message truncated)") {
		t.Fatalf("long user message missing truncation marker:\n%s", joined)
	}
	if strings.Contains(joined, "line-29") {
		t.Fatalf("long user message rendered content beyond truncation:\n%s", joined)
	}
	if got, want := len(rows), maxUserMessageRenderLines+3; got != want {
		t.Fatalf("rendered row count = %d, want %d content/truncation/padding rows", got, want)
	}
}

func TestChatLogStreamingFencedBlockKeepsLiteralStateAcrossDeltas(t *testing.T) {
	conv := NewConversationModel()
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("```markdown\n# Literal Header\n")

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, log)

	conv.AppendAssistantText("**literal bold**\n")
	waitForChatLog(t, log)

	log.mu.Lock()
	defer log.mu.Unlock()
	joined := strings.Join(renderLineTexts(log.lines), "\n")
	for _, want := range []string{" ```markdown", " # Literal Header", " **literal bold**"} {
		if !strings.Contains(tauui.StripANSI(joined), want) {
			t.Fatalf("streaming fenced content missing %q from:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "\x1b[1m") || strings.Contains(joined, "\x1b[36m") {
		t.Fatalf("streaming fenced block content should not be markdown-styled:\n%q", joined)
	}
}

func TestChatLogMarkdownPreservesANSIControlSequencesForStyledPaint(t *testing.T) {
	conv := NewConversationModel()
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("The current directory is `/home/harley/Projects/ai/tau`.\n\n- `.agents/`\n- `go.mod`")
	conv.EndAssistantMessage()

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, log)

	log.mu.Lock()
	defer log.mu.Unlock()
	foundInlineCode := false
	for _, line := range log.lines {
		if strings.Contains(line.text, "\x1b[22;38;2;230;200;140m") {
			foundInlineCode = true
		}
	}
	if !foundInlineCode {
		t.Fatalf("rendered markdown did not preserve inline-code styling: %#v", log.lines)
	}
}

func TestChatLogFencedBlocksRenderLiteralContent(t *testing.T) {
	conv := NewConversationModel()
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("```markdown\n# Literal Header\n**literal bold**\n```\n\nOutside **bold**")
	conv.EndAssistantMessage()

	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, log)

	log.mu.Lock()
	defer log.mu.Unlock()
	joined := strings.Join(renderLineTexts(log.lines), "\n")
	for _, want := range []string{" ```markdown", " # Literal Header", " **literal bold**", " ```"} {
		if !strings.Contains(tauui.StripANSI(joined), want) {
			t.Fatalf("literal fenced content missing %q from:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "\x1b[7m") {
		t.Fatalf("fenced block should not render inverse styling in chat log:\n%q", joined)
	}
	if !strings.Contains(joined, "Outside \x1b[1mbold\x1b[0m") {
		t.Fatalf("outside markdown should still be styled:\n%q", joined)
	}
}

func TestChatLogPaintFillsEachRoleRow(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hello")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 10, H: 1})
	waitForChatLog(t, log)

	d := &recordingDrawer{}
	log.Paint(d, &tauui.Ctx{Theme: appTheme()})

	if len(d.fills) < 2 {
		t.Fatalf("expected chat rect fill plus row fill, got %d", len(d.fills))
	}
	row := d.fills[1]
	if row.rect != (tauui.Rect{X: 0, Y: 0, W: 10, H: 1}) {
		t.Fatalf("row fill rect = %#v, want full-width first row", row.rect)
	}
	if row.st.BG != defaultChatPalette().user.BG {
		t.Fatalf("user row BG = %#v, want %#v", row.st.BG, defaultChatPalette().user.BG)
	}
}

func TestChatLogPaintWithScrollOffsetShowsScrolledContent(t *testing.T) {
	conv := NewConversationModel()
	for i := range 10 {
		conv.AddSystem(fmt.Sprintf("line-%02d", i))
	}
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 20, H: 3})
	waitForChatLog(t, log)

	base := tauui.Style{}
	buf := tauui.NewBuffer(20, 5)
	buf.Clear(tauui.Cell{R: ' ', Style: base})
	rp := tauui.NewRenderPainter(buf, tauui.Rect{X: 0, Y: 0, W: 20, H: 5}, base)
	p := tauui.NewPainter(rp, base)
	d := tauui.NewDrawer(p)

	d.WithClip(tauui.Rect{X: 0, Y: 0, W: 20, H: 5}, func(cd tauui.Drawer) {
		cd.WithOffset(0, -15, func(od tauui.Drawer) {
			log.setVisibleRows(15, 20)
			defer log.setVisibleRows(0, 0)
			log.Paint(od, &tauui.Ctx{Theme: appTheme()})
		})
	})

	for y, want := range map[int]string{0: "line-07", 2: "line-08", 4: "line-09"} {
		if got := bufferRowString(buf, y, 20); !strings.Contains(got, want) {
			t.Fatalf("row %d = %q, want to contain %q", y, got, want)
		}
	}
}

func TestChatScrollViewportPaintOnlyDrawsVisibleRows(t *testing.T) {
	conv := NewConversationModel()
	for i := range 100 {
		conv.AddUserMessage(fmt.Sprintf("line-%03d", i))
	}
	log := newTestChatLog(t, conv)
	scroll := widgets.NewScrollView(widgets.ScrollViewOpts{Child: log, Scrollbar: widgets.ScrollbarAuto})
	viewport := &chatScrollViewport{ScrollView: scroll, chat: log}
	viewport.Layout(tauui.Rect{X: 0, Y: 0, W: 20, H: 5})
	waitForChatLog(t, log)
	viewport.Layout(tauui.Rect{X: 0, Y: 0, W: 20, H: 5})
	scroll.ScrollTo(&tauui.Ctx{Invalidate: func(tauui.Rect) {}}, max(0, log.LineCount()-5))

	d := &recordingDrawer{}
	viewport.Paint(d, &tauui.Ctx{Theme: appTheme()})

	// The first fill clears the full chat rect. Visible chat rows should be
	// bounded by viewport height, not by the 100-line transcript.
	rowFills := len(d.fills) - 1
	if rowFills != 5 {
		t.Fatalf("row fills = %d, want viewport height 5 (total fills %d)", rowFills, len(d.fills))
	}
}

func TestChatLogMouseDragSelectsTextAndCopiesOnRelease(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hello world")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 20, H: 3})
	waitForChatLog(t, log)

	var copied string
	var invalidated []tauui.Rect
	ctx := &tauui.Ctx{
		Invalidate:     func(r tauui.Rect) { invalidated = append(invalidated, r) },
		ClipboardWrite: func(s string) { copied = s },
	}

	if !log.Handle(tauui.MouseEvent{X: 1, Y: 1, Button: tauui.MouseButtonLeft, Action: tauui.MousePress}, ctx) {
		t.Fatal("mouse press was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: 6, Y: 1, Button: tauui.MouseButtonLeft, Action: tauui.MouseDrag}, ctx) {
		t.Fatal("mouse drag was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: 6, Y: 1, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("mouse release was not handled")
	}

	if copied != "hello" {
		t.Fatalf("copied selection = %q, want %q", copied, "hello")
	}
	if len(invalidated) != 3 {
		t.Fatalf("invalidations = %d, want one per selection change", len(invalidated))
	}
}

func TestChatLogDoubleClickSelectsWordAndCopiesOnRelease(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hello world")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 20, H: 3})
	waitForChatLog(t, log)

	var copied string
	ctx := &tauui.Ctx{
		Invalidate:     func(tauui.Rect) {},
		ClipboardWrite: func(s string) { copied = s },
	}

	if !log.Handle(tauui.MouseEvent{X: 3, Y: 1, Button: tauui.MouseButtonLeft, Action: tauui.MousePress, ClickCount: 2}, ctx) {
		t.Fatal("double-click press was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: 3, Y: 1, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("double-click release was not handled")
	}

	if copied != "hello" {
		t.Fatalf("copied double-click selection = %q, want %q", copied, "hello")
	}
}

func TestChatLogTripleClickSelectsLineAndCopiesOnRelease(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hello world")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 20, H: 3})
	waitForChatLog(t, log)

	var copied string
	ctx := &tauui.Ctx{
		Invalidate:     func(tauui.Rect) {},
		ClipboardWrite: func(s string) { copied = s },
	}

	if !log.Handle(tauui.MouseEvent{X: 5, Y: 1, Button: tauui.MouseButtonLeft, Action: tauui.MousePress, ClickCount: 3}, ctx) {
		t.Fatal("triple-click press was not handled")
	}
	if !log.Handle(tauui.MouseEvent{X: 5, Y: 1, Button: tauui.MouseButtonLeft, Action: tauui.MouseRelease}, ctx) {
		t.Fatal("triple-click release was not handled")
	}

	if strings.TrimRight(copied, " ") != "hello world" {
		t.Fatalf("copied triple-click selection = %q, want %q", copied, "hello world")
	}
}

func TestChatLogPaintHighlightsSelection(t *testing.T) {
	conv := NewConversationModel()
	conv.AddUserMessage("hello")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 20, H: 1})
	waitForChatLog(t, log)

	ctx := &tauui.Ctx{Theme: appTheme()}
	log.Handle(tauui.MouseEvent{X: 1, Y: 0, Button: tauui.MouseButtonLeft, Action: tauui.MousePress}, ctx)
	log.Handle(tauui.MouseEvent{X: 3, Y: 0, Button: tauui.MouseButtonLeft, Action: tauui.MouseDrag}, ctx)

	base := tauui.Style{}
	buf := tauui.NewBuffer(20, 1)
	buf.Clear(tauui.Cell{R: ' ', Style: base})
	rp := tauui.NewRenderPainter(buf, tauui.Rect{X: 0, Y: 0, W: 20, H: 1}, base)
	p := tauui.NewPainter(rp, base)
	d := tauui.NewDrawer(p)

	log.Paint(d, ctx)

	want := appTheme().Palette.Selection
	for x := 1; x < 3; x++ {
		if got := buf.At(x, 0).Style; got != want {
			t.Fatalf("selected cell %d style = %+v, want %+v", x, got, want)
		}
	}
}

func TestAppThemeUsesColorUnicodeChrome(t *testing.T) {
	theme := appTheme()

	if theme.Chrome.Border.Glyphs != tauui.BoxGlyphsLight {
		t.Fatalf("expected Unicode light-box border glyphs, got %#v", theme.Chrome.Border.Glyphs)
	}
	if theme.Chrome.FocusRing.Glyphs != tauui.BoxGlyphsLight {
		t.Fatalf("expected Unicode light-box focus glyphs, got %#v", theme.Chrome.FocusRing.Glyphs)
	}
	if theme.Base.FG.Kind() == tauui.ColorKindDefault || theme.Palette.Accent.FG.Kind() == tauui.ColorKindDefault {
		t.Fatalf("expected colored theme, got base FG kind %v and accent FG kind %v", theme.Base.FG.Kind(), theme.Palette.Accent.FG.Kind())
	}
}

func TestNewEnablesInputFeaturesForPromptEditing(t *testing.T) {
	app, err := New(Options{Session: &fakeSession{}})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	t.Cleanup(func() { _ = app.chat.Close() })

	if app.app == nil {
		t.Fatal("toolkit app was not initialized")
	}
	input := appInputOpts()
	if !input.Mouse {
		t.Fatal("mouse tracking should be enabled so wheel events reach the chat scroll view")
	}
	if !input.ModifiedKeys {
		t.Fatal("modified key reporting should be enabled so Shift+Enter can be distinguished from Enter")
	}
}

func TestEditorEnterKeymapSendsAndShiftEnterNewlines(t *testing.T) {
	km := appKeymap{}

	act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyEnter})
	if !ok || act != AppActionEditorSubmit {
		t.Fatalf("plain Enter in editor = (%v, %v), want AppActionEditorSubmit", act, ok)
	}

	act, ok = km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyEnter, Mod: tauui.ModShift})
	if !ok || act != tauui.ActionSubmit {
		t.Fatalf("Shift+Enter in editor = (%v, %v), want ActionSubmit for prompt editor newline", act, ok)
	}

	act, ok = km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyEnter, Mod: tauui.ModCtrl})
	if ok {
		t.Fatalf("unbound Ctrl+Enter in editor = (%v, %v)", act, ok)
	}

	act, ok = km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyEnter, Mod: tauui.ModAlt})
	if !ok || act != tauui.ActionSubmit {
		t.Fatalf("Alt+Enter in editor = (%v, %v), want ActionSubmit/newline because many terminals encode Shift+Enter as Alt+Enter", act, ok)
	}

	act, ok = km.Resolve(tauui.KeyCtxGlobal, tauui.Keystroke{Key: tauui.KeyEnter})
	if !ok || act != tauui.ActionActivate {
		t.Fatalf("plain Enter outside editor = (%v, %v), want ActionActivate", act, ok)
	}
}

func TestPlStringAndBool(t *testing.T) {
	if v, ok := plString(map[string]any{"delta": "hi"}, "delta"); !ok || v != "hi" {
		t.Fatalf("plString unexpected: %q %v", v, ok)
	}
	if _, ok := plString(map[string]any{"delta": 1}, "delta"); ok {
		t.Fatal("expected plString to reject non-string")
	}
	if v, ok := plBool(map[string]any{"is_error": true}, "is_error"); !ok || !v {
		t.Fatalf("plBool unexpected: %v %v", v, ok)
	}
}

func TestUsageEventRefreshesFooterStatsFromSessionState(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})
	fs := a.sess.(*fakeSession)
	fs.mu.Lock()
	fs.totalTokens = 42
	fs.contextWindow = 100
	fs.usedPct = 42
	fs.mu.Unlock()

	a.handleEvent(sdk.Event{Type: "usage", Payload: map[string]any{"this_call": map[string]any{"input": 999, "output": 999}}})

	if got := a.footer.data.totalTokens; got != 42 {
		t.Fatalf("footer total tokens = %d, want session Stats total 42", got)
	}
	if got := a.footer.data.contextWindow; got != 100 {
		t.Fatalf("footer context window = %d, want session Stats window 100", got)
	}
}

func TestMessageEndForUserDoesNotFinalizeAssistant(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})
	a.conv.BeginAssistantMessage()

	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "user"}})
	a.handleEvent(sdk.Event{Type: "text_delta", Payload: map[string]any{"delta": "hello"}})

	messages := a.conv.Messages()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1", len(messages))
	}
	if got := messages[0].Text; got != "hello" {
		t.Fatalf("assistant text = %q, want %q", got, "hello")
	}
	if !messages[0].Streaming {
		t.Fatal("user message_end should not finalize the assistant stream")
	}
}

func TestAssistantMessageEndUsesFinalContentWhenNoDeltasArrived(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "content": "full response"}})

	messages := a.conv.Messages()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1", len(messages))
	}
	if got := messages[0].Text; got != "full response" {
		t.Fatalf("assistant text = %q, want %q", got, "full response")
	}
	if messages[0].Streaming {
		t.Fatal("assistant message_end should finalize the assistant stream")
	}
}

func TestAssistantMessageEndDoesNotDuplicateStreamedDeltas(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "text_delta", Payload: map[string]any{"delta": "hello"}})
	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "content": "hello"}})

	messages := a.conv.Messages()
	if got := messages[0].Text; got != "hello" {
		t.Fatalf("assistant text = %q, want exactly %q", got, "hello")
	}
}

func TestDuplicateAssistantMessageEndDoesNotDuplicateCommittedMessage(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	payload := map[string]any{"role": "assistant", "message_id": "msg-1", "content": "hello"}
	a.handleEvent(sdk.Event{Type: "message_end", Payload: payload})
	a.handleEvent(sdk.Event{Type: "message_end", Payload: payload})

	messages := a.conv.Messages()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1: %#v", len(messages), messages)
	}
	if messages[0].Role != RoleAssistant || messages[0].Text != "hello" || messages[0].Streaming {
		t.Fatalf("message[0] = %#v, want committed assistant hello", messages[0])
	}
}

func TestDuplicateAssistantMessageEndWithoutIDDoesNotDuplicateCommittedMessage(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	payload := map[string]any{"role": "assistant", "content": "hello"}
	a.handleEvent(sdk.Event{Type: "message_end", Payload: payload})
	a.handleEvent(sdk.Event{Type: "message_end", Payload: payload})

	messages := a.conv.Messages()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1: %#v", len(messages), messages)
	}
	if messages[0].Role != RoleAssistant || messages[0].Text != "hello" || messages[0].Streaming {
		t.Fatalf("message[0] = %#v, want committed assistant hello", messages[0])
	}
}

func TestMessageStartCreatesAssistantStreamingRow(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "message_start", Payload: map[string]any{"role": "assistant", "message_id": "msg-1"}})

	messages := a.conv.Messages()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1: %#v", len(messages), messages)
	}
	if messages[0].Role != RoleAssistant || !messages[0].Streaming || messages[0].ID != "msg-1" {
		t.Fatalf("message[0] = %#v, want streaming assistant with sdk id", messages[0])
	}
}

func TestUserMessageEventsCommitUserRows(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "message_start", Payload: map[string]any{"role": "user", "message_id": "u1"}})
	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "user", "message_id": "u1", "content": "hello"}})

	messages := a.conv.Messages()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1: %#v", len(messages), messages)
	}
	if messages[0].Role != RoleUser || messages[0].Text != "hello" {
		t.Fatalf("message[0] = %#v, want event-sourced user message", messages[0])
	}
}

func TestToolLifecycleUpdatesOneLogicalToolExecution(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "tool_call", Payload: map[string]any{"tool_call_id": "tool-1", "name": "search", "input": `{"q":"tau"}`}})
	a.handleEvent(sdk.Event{Type: "tool_progress", Payload: map[string]any{"tool_call_id": "tool-1", "output": "running"}})
	a.handleEvent(sdk.Event{Type: "tool_result", Payload: map[string]any{"tool_call_id": "tool-1", "output": "done", "is_error": false}})

	messages := a.conv.Messages()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1: %#v", len(messages), messages)
	}
	msg := messages[0]
	if msg.Role != RoleToolCall || msg.ToolCallID != "tool-1" || msg.ToolName != "search" || msg.ToolArgs != `{"q":"tau"}` || msg.Text != "search {\"q\":\"tau\"}\ndone" {
		t.Fatalf("tool message = %#v, want one updated tool execution", msg)
	}
}

func TestAgentEndFinalizesAssistantWhenMessageEndMissing(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})
	a.setStreaming(true)

	a.handleEvent(sdk.Event{Type: "message_start", Payload: map[string]any{"role": "assistant", "message_id": "a1"}})
	a.handleEvent(sdk.Event{Type: "text_delta", Payload: map[string]any{"message_id": "a1", "delta": "partial"}})
	a.handleEvent(sdk.Event{Type: "agent_end"})

	messages := a.conv.Messages()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1: %#v", len(messages), messages)
	}
	if messages[0].Role != RoleAssistant || messages[0].Text != "partial" || messages[0].Streaming {
		t.Fatalf("message[0] = %#v, want committed assistant partial", messages[0])
	}
	if a.isStreaming() {
		t.Fatal("agent_end should clear App streaming state")
	}
}

func TestToolProgressChunksAppendUntilFinalResult(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "tool_call", Payload: map[string]any{"tool_call_id": "tool-1", "name": "search", "input": `{"q":"tau"}`}})
	a.handleEvent(sdk.Event{Type: "tool_progress", Payload: map[string]any{"tool_call_id": "tool-1", "chunk": "run"}})
	a.handleEvent(sdk.Event{Type: "tool_progress", Payload: map[string]any{"tool_call_id": "tool-1", "chunk": "ning"}})

	messages := a.conv.Messages()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1: %#v", len(messages), messages)
	}
	if got, want := messages[0].Text, "search {\"q\":\"tau\"}\nrunning"; got != want {
		t.Fatalf("tool progress text = %q, want %q", got, want)
	}
}

func TestThinkingIndicatorIsRemovedWhenVisibleOutputArrives(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})
	a.conv.BeginAssistantMessage()

	a.handleEvent(sdk.Event{Type: "thinking_delta", Payload: map[string]any{"delta": "hidden reasoning"}})
	if messages := a.conv.Messages(); len(messages) != 2 || messages[1].Role != RoleThinking {
		t.Fatalf("thinking indicator was not added: %#v", messages)
	}

	a.handleEvent(sdk.Event{Type: "text_delta", Payload: map[string]any{"delta": "hello"}})

	messages := a.conv.Messages()
	for _, msg := range messages {
		if msg.Role == RoleThinking {
			t.Fatalf("thinking indicator should be removed once visible output arrives: %#v", messages)
		}
	}
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want only assistant row: %#v", len(messages), messages)
	}
	if got := messages[0].Text; got != "hello" {
		t.Fatalf("assistant text = %q, want %q", got, "hello")
	}
}

func TestTUIStreamFrameStopsBeforeChangingFromThinkingToVisibleOutput(t *testing.T) {
	events := []sdk.Event{
		{Type: "thinking_delta", Payload: map[string]any{"delta": "one"}},
		{Type: "thinking_delta", Payload: map[string]any{"delta": "two"}},
		{Type: "text_delta", Payload: map[string]any{"delta": "hello"}},
		{Type: "message_end", Payload: map[string]any{"role": "assistant", "content": "hello"}},
	}

	if got := tuiStreamFrameCount(events, tuiStreamEventsPerFrame); got != 2 {
		t.Fatalf("first frame count = %d, want thinking-only frame of 2 events", got)
	}

	frame := coalesceTUIStreamEvents(events[:2])
	if len(frame) != 1 || frame[0].Type != "thinking_delta" {
		t.Fatalf("thinking frame should coalesce to one thinking_delta, got %#v", frame)
	}
}

func TestTUIStreamFrameDelaysVisibleOutputAfterThinking(t *testing.T) {
	lastThinking := time.Unix(10, 0)
	visible := []sdk.Event{{Type: "text_delta", Payload: map[string]any{"delta": "hello"}}}

	if got := tuiStreamFrameDelay(visible, lastThinking, lastThinking.Add(throbberInterval)); got != tuiThinkingMinimumDwell-throbberInterval {
		t.Fatalf("delay after one throbber tick = %s, want %s", got, tuiThinkingMinimumDwell-throbberInterval)
	}
	if got := tuiStreamFrameDelay(visible, lastThinking, lastThinking.Add(tuiThinkingMinimumDwell)); got != 0 {
		t.Fatalf("delay after dwell elapsed = %s, want 0", got)
	}
	if got := tuiStreamFrameDelay(visible, time.Time{}, lastThinking); got != 0 {
		t.Fatalf("delay without prior thinking = %s, want 0", got)
	}

	thinking := []sdk.Event{{Type: "thinking_delta", Payload: map[string]any{"delta": "still thinking"}}}
	if got := tuiStreamFrameDelay(thinking, lastThinking, lastThinking.Add(throbberInterval)); got != 0 {
		t.Fatalf("delay for more thinking = %s, want 0", got)
	}
	structural := []sdk.Event{{Type: "usage", Payload: map[string]any{"total_tokens": 10}}}
	if got := tuiStreamFrameDelay(structural, lastThinking, lastThinking.Add(throbberInterval)); got != 0 {
		t.Fatalf("delay for structural event = %s, want 0", got)
	}
}

func TestThinkingIndicatorIsRemovedWhenRunEndsWithoutVisibleOutput(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})
	a.conv.BeginAssistantMessage()

	a.handleEvent(sdk.Event{Type: "thinking_delta", Payload: map[string]any{"delta": "hidden reasoning"}})
	a.handleEvent(sdk.Event{Type: "agent_end"})

	messages := a.conv.Messages()
	for _, msg := range messages {
		if msg.Role == RoleThinking {
			t.Fatalf("thinking indicator should be removed when run ends: %#v", messages)
		}
	}
}

func TestAssistantMessageEndEmptyRemovesBlankRowAndCounts(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})
	a.conv.BeginAssistantMessage()

	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "content": ""}})
	a.handleEvent(sdk.Event{Type: "message_start", Payload: map[string]any{"role": "assistant"}})
	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "content": ""}})

	messages := a.conv.Messages()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want one summary row: %#v", len(messages), messages)
	}
	if messages[0].Role != RoleSystem {
		t.Fatalf("summary role = %q, want system", messages[0].Role)
	}
	if got := messages[0].Text; got != "Empty response from the server: 2" {
		t.Fatalf("summary text = %q", got)
	}
}

func TestToolOnlyAssistantMessageDoesNotDropFollowupAssistantText(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})
	// submit pre-creates the first assistant row before SDK events arrive.
	a.conv.BeginAssistantMessage()

	// First assistant message contains only a tool call, so its message_end
	// finalizes the pre-created empty row.
	a.handleEvent(sdk.Event{Type: "message_start", Payload: map[string]any{"role": "assistant"}})
	a.handleEvent(sdk.Event{Type: "tool_call", Payload: map[string]any{"name": "bash", "input": `{"command":"pwd && ls"}`}})
	a.handleEvent(sdk.Event{Type: "tool_result", Payload: map[string]any{"output": "file-a\nfile-b", "is_error": false}})
	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "content": ""}})

	// The agent then starts a second assistant message after the tool result.
	// The TUI must create a fresh streaming row; otherwise text_delta is lost.
	a.handleEvent(sdk.Event{Type: "message_start", Payload: map[string]any{"role": "assistant"}})
	a.handleEvent(sdk.Event{Type: "text_delta", Payload: map[string]any{"delta": "The directory contains file-a and file-b."}})
	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "content": "The directory contains file-a and file-b."}})

	messages := a.conv.Messages()
	if len(messages) < 3 {
		t.Fatalf("messages length = %d, want at least 3: %#v", len(messages), messages)
	}
	last := messages[len(messages)-1]
	if last.Role != RoleAssistant {
		t.Fatalf("last role = %q, want assistant; messages=%#v", last.Role, messages)
	}
	if got := last.Text; got != "The directory contains file-a and file-b." {
		t.Fatalf("follow-up assistant text = %q", got)
	}
	if last.Streaming {
		t.Fatal("follow-up assistant message should be finalized")
	}
}

func TestAgentEndClearsWorkingStatus(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{cwd: "/tmp/proj"})
	a.status = tauui.NewStatusEntriesView(5)

	a.setStreaming(true)
	before, _ := a.footer.render(80)
	if strings.Contains(before, "working") {
		t.Fatalf("streaming footer should not contain loader: %q", before)
	}
	if got := a.statusEntries[loaderStatusKey]; !strings.Contains(got, "⠋ "+defaultWorkingMessage) {
		t.Fatalf("streaming status missing throbber working indicator: %q", got)
	}

	// Provider/API failures can surface as an error event before the
	// session channel close fallback runs. The terminal agent_end event
	// is the point where the status region should stop claiming work is active.
	a.handleEvent(sdk.Event{Type: "error", Payload: map[string]any{"message": "API error: 529 overloaded"}})
	a.handleEvent(sdk.Event{Type: "agent_end"})

	if a.isStreaming() {
		t.Fatal("agent_end should clear App streaming state")
	}
	after, _ := a.footer.render(80)
	if strings.Contains(after, "working") {
		t.Fatalf("agent_end should keep working indicator out of footer, got footer %q", after)
	}
	if _, ok := a.statusEntries["agent"]; ok {
		t.Fatalf("agent_end should clear working status, got entries %v", a.statusEntries)
	}
}

func TestConsumeStreamingForInterruptClearsWorkingState(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{cwd: "/tmp/proj"})
	a.conv.BeginAssistantMessage()
	a.conv.AppendAssistantText("partial")
	a.setStreaming(true)

	if !a.consumeStreamingForInterrupt() {
		t.Fatal("consumeStreamingForInterrupt returned false for active stream")
	}
	if a.isStreaming() {
		t.Fatal("interrupt should clear App streaming state")
	}
	if a.conv.Messages()[0].Streaming {
		t.Fatal("interrupt should finalize assistant message")
	}
	fs := a.sess.(*fakeSession)
	fs.mu.Lock()
	interrupts := fs.interrupts
	fs.mu.Unlock()
	if interrupts != 1 {
		t.Fatalf("Interrupt calls = %d, want 1", interrupts)
	}
	after, _ := a.footer.render(80)
	if strings.Contains(after, "working") {
		t.Fatalf("interrupt should clear working indicator, got footer %q", after)
	}
}

func TestSetStreamingInvalidatesFooter(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{cwd: "/tmp/proj"})
	a.footer.Layout(tauui.Rect{X: 0, Y: 20, W: 80, H: 2})
	a.status = tauui.NewStatusEntriesView(5)

	var invalidated []tauui.Rect
	a.loopMu.Lock()
	a.loopCtx = &tauui.Ctx{Invalidate: func(r tauui.Rect) { invalidated = append(invalidated, r) }}
	a.loopMu.Unlock()

	a.setStreaming(true)
	// The loader now carries the configurable working message rather than a
	// hardcoded " working" literal.
	if got := a.statusEntries[loaderStatusKey]; !strings.Contains(got, defaultWorkingMessage) {
		t.Fatalf("setStreaming(true) status = %q, want the working loader", got)
	}
	if len(invalidated) == 0 {
		t.Fatal("setStreaming(true) should invalidate affected regions")
	}

	a.setStreaming(false)
	if _, ok := a.statusEntries["agent"]; ok {
		t.Fatalf("setStreaming(false) should clear working status, got %v", a.statusEntries)
	}
}

// TestConversationModelFiresListeners verifies that every mutating entry point
// notifies registered listeners. The chat log relies on this to invalidate
// itself when the underlying model changes while the scroll view is unfocused.
func TestConversationModelFiresListeners(t *testing.T) {
	conv := NewConversationModel()

	var calls int
	conv.AddListener(func() { calls++ })

	conv.AddSystem("hi")
	conv.AddUserMessage("hello")
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("world")
	conv.EndAssistantMessage()
	conv.AddToolCall("read_file", `{"path":"/tmp/x"}`)
	conv.AddToolResult("contents", false)
	conv.AddError("boom")

	if calls != 8 {
		t.Fatalf("expected 8 listener invocations, got %d", calls)
	}
}

// noopDrawer is a Drawer that discards all writes. It exists so tests can
// drive ChatLog.Paint without standing up a real cell buffer.
type noopDrawer struct{}

func (noopDrawer) FillRect(tauui.Rect, tauui.Style)          {}
func (noopDrawer) DrawText(tauui.Point, string, tauui.Style) {}
func (noopDrawer) DrawBorder(tauui.Rect, tauui.BoxStyle)     {}
func (noopDrawer) ClipRect() tauui.Rect                      { return tauui.Rect{} }
func (noopDrawer) WithClip(tauui.Rect, func(tauui.Drawer))   {}
func (noopDrawer) WithOffset(int, int, func(tauui.Drawer))   {}

type fillRecord struct {
	rect tauui.Rect
	st   tauui.Style
}

type textRecord struct {
	pos  tauui.Point
	text string
	st   tauui.Style
}

type recordingDrawer struct {
	fills []fillRecord
	texts []textRecord
}

func (r *recordingDrawer) FillRect(rect tauui.Rect, st tauui.Style) {
	r.fills = append(r.fills, fillRecord{rect: rect, st: st})
}

func (r *recordingDrawer) DrawText(p tauui.Point, s string, st tauui.Style) {
	r.texts = append(r.texts, textRecord{pos: p, text: s, st: st})
}
func (*recordingDrawer) DrawBorder(tauui.Rect, tauui.BoxStyle)          {}
func (*recordingDrawer) ClipRect() tauui.Rect                           { return tauui.Rect{} }
func (r *recordingDrawer) WithClip(_ tauui.Rect, fn func(tauui.Drawer)) { fn(r) }
func (r *recordingDrawer) WithOffset(_, _ int, fn func(tauui.Drawer))   { fn(r) }

func bufferRowString(buf *tauui.Buffer, y, width int) string {
	var b strings.Builder
	for x := range width {
		b.WriteRune(buf.At(x, y).R)
	}
	return b.String()
}

func newTestChatLog(t *testing.T, conv *ConversationModel) *ChatLog {
	t.Helper()
	log := NewChatLog(conv)
	log.setRenderNotifier(func() error {
		log.applyPreparedRender()
		return nil
	})
	t.Cleanup(func() { _ = log.Close() })
	return log
}

func waitForChatLog(t *testing.T, log *ChatLog) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		log.mu.Lock()
		ready := log.revision > 0 && log.renderedRevision == log.revision &&
			log.pending == nil && log.latestPrepared == nil && !log.notificationPending
		revision := log.revision
		renderedRevision := log.renderedRevision
		log.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("chat log did not finish rendering: revision=%d rendered=%d", revision, renderedRevision)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestChatLogListenerSchedulesWithoutSynchronousRewrap verifies that a model
// mutation only captures and schedules state on the listener path. The cached
// render and notifier remain unchanged until the worker finishes.
func TestChatLogListenerSchedulesWithoutSynchronousRewrap(t *testing.T) {
	conv := NewConversationModel()
	conv.AddSystem("first short message")
	conv.AddSystem("second short message")

	log := NewChatLog(conv)
	t.Cleanup(func() { _ = log.Close() })
	notifications := make(chan chatLogRenderUpdate, 4)
	log.setRenderNotifier(func() error {
		if update := log.applyPreparedRender(); update.installed {
			notifications <- update
		}
		return nil
	})
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 20})
	waitForChatLog(t, log)
	receiveRenderUpdate(t, notifications)

	started := make(chan struct{})
	release := make(chan struct{})
	baseWrap := log.wrapComponents
	log.mu.Lock()
	beforeLines := len(log.lines)
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
	waitForSignal(t, started, "listener-scheduled wrap start")

	log.mu.Lock()
	afterLines := len(log.lines)
	log.mu.Unlock()
	if afterLines != beforeLines {
		t.Fatalf("listener rewrapped synchronously: lines changed from %d to %d", beforeLines, afterLines)
	}
	select {
	case <-notifications:
		t.Fatal("render-ready notification arrived before wrapping completed")
	default:
	}

	close(release)
	waitForChatLog(t, log)
	select {
	case update := <-notifications:
		if !update.installed {
			t.Fatal("notifier ran before installing completed render state")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for render-ready notification")
	}
}

// TestChatLogMutationBeforeFirstLayoutIsRendered verifies that snapshots are
// not scheduled without a width, but the first Layout captures all content.
func TestChatLogMutationBeforeFirstLayoutIsRendered(t *testing.T) {
	conv := NewConversationModel()
	log := newTestChatLog(t, conv)

	conv.AddUserMessage("pre-layout content")
	log.mu.Lock()
	revision := log.revision
	pending := log.pending
	log.mu.Unlock()
	if revision != 0 || pending != nil {
		t.Fatalf("mutation without width scheduled work: revision=%d pending=%#v", revision, pending)
	}

	log.Layout(tauui.Rect{X: 0, Y: 0, W: 30, H: 10})
	waitForChatLog(t, log)
	log.mu.Lock()
	joined := ""
	for _, line := range log.lines {
		joined += line.text
	}
	log.mu.Unlock()
	if !strings.Contains(joined, "pre-layout content") {
		t.Fatalf("first layout did not render pre-layout mutation: %q", joined)
	}
}

func TestAnonymousFinalMessageDedupResetsForNewRun(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "content": "first"}})
	if a.conversationEvents == nil {
		a.conversationEvents = newConversationEventAdapter(a)
	}
	a.conversationEvents.beginRun()
	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "content": "second"}})

	messages := a.conv.Messages()
	if len(messages) != 2 {
		t.Fatalf("messages length = %d, want 2: %#v", len(messages), messages)
	}
	if messages[0].Text != "first" || messages[1].Text != "second" {
		t.Fatalf("messages = %#v, want first then second", messages)
	}
}

func TestMessageUpdateReplacesAssistantText(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "message_start", Payload: map[string]any{"role": "assistant"}})
	a.handleEvent(sdk.Event{Type: "message_update", Payload: map[string]any{"text": "hello"}})
	a.handleEvent(sdk.Event{Type: "message_update", Payload: map[string]any{"text": "hello world"}})
	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "content": "hello world"}})

	messages := a.conv.Messages()
	if len(messages) != 1 || messages[0].Text != "hello world" {
		t.Fatalf("messages = %#v, want one assistant with replacement text", messages)
	}
}

func TestToolCancelledEventMarksToolCancelled(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "tool_call", Payload: map[string]any{"tool_call_id": "tool-1", "name": "bash", "input": `{"command":"sleep 10"}`}})
	a.handleEvent(sdk.Event{Type: "tool_cancelled", Payload: map[string]any{"tool_call_id": "tool-1", "reason": "interrupted"}})

	snapshot := a.conv.Snapshot()
	if len(snapshot.Components) != 1 {
		t.Fatalf("components length = %d, want 1", len(snapshot.Components))
	}
	tool := snapshot.Components[0].ToolExecution
	if tool.State != toolExecutionCancelled || tool.Output != "interrupted" {
		t.Fatalf("tool payload = %#v, want cancelled/interrupted", tool)
	}
}

func TestToolResultWithoutIDUpdatesLastTool(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "tool_call", Payload: map[string]any{"name": "search", "input": `{"q":"tau"}`}})
	a.handleEvent(sdk.Event{Type: "tool_progress", Payload: map[string]any{"chunk": "run"}})
	a.handleEvent(sdk.Event{Type: "tool_result", Payload: map[string]any{"output": "done", "is_error": false}})

	messages := a.conv.Messages()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1: %#v", len(messages), messages)
	}
	if got, want := messages[0].Text, "search {\"q\":\"tau\"}\ndone"; got != want {
		t.Fatalf("tool text = %q, want %q", got, want)
	}
}
