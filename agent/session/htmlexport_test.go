package session

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// buildTestReader creates a session file in a temp dir, appends the given
// entries, and returns a *Reader for the resulting file.
func buildTestReader(t *testing.T, entries ...Entry) *Reader {
	t.Helper()
	dir := t.TempDir()
	header := NewHeader("/test/cwd", nil, nil)
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}
	// Chain entries with parent IDs so ActiveBranch() can traverse them
	var prevID *string
	for i := range entries {
		if prevID != nil {
			entries[i].ParentID = prevID
		}
		if err := w.Append(entries[i]); err != nil {
			t.Fatalf("Append entry %d: %v", i, err)
		}
		prevID = &entries[i].ID
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r, err := ReadFile(w.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return r
}

// buildTestReaderAt creates a session file in the given dir (not a temp dir)
// so the path is controlled.
func buildTestReaderAt(t *testing.T, dir string, entries ...Entry) *Reader {
	t.Helper()
	header := NewHeader("/test/cwd", nil, nil)
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}
	// Chain entries with parent IDs so ActiveBranch() can traverse them
	var prevID *string
	for i := range entries {
		if prevID != nil {
			entries[i].ParentID = prevID
		}
		if err := w.Append(entries[i]); err != nil {
			t.Fatalf("Append entry %d: %v", i, err)
		}
		prevID = &entries[i].ID
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r, err := ReadFile(w.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return r
}

// simpleUserMsg builds a minimal user message entry.
func simpleUserMsg(t *testing.T, content string) Entry {
	t.Helper()
	msg := map[string]string{"role": "user", "content": content}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	return NewMessageEntry(nil, data, nil)
}

// msgWithToolCall builds an assistant message that has a tool_call and tool_result.
func msgWithToolCall(t *testing.T, toolName, toolCallID, resultContent string) Entry {
	t.Helper()
	msg := map[string]any{
		"role":    "assistant",
		"content": "",
		"tool_calls": []any{
			map[string]any{
				"id":    toolCallID,
				"name":  toolName,
				"input": map[string]any{"cmd": "ls"},
			},
		},
		"tool_results": []any{
			map[string]any{
				"tool_call_id": toolCallID,
				"content":      resultContent,
			},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal tool message: %v", err)
	}
	return NewMessageEntry(nil, data, nil)
}

func TestRenderHTML_IncludesEmbeddedStylesheet(t *testing.T) {
	r := buildTestReader(t, simpleUserMsg(t, "hello"))
	out, err := RenderHTML(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)
	if count := strings.Count(html, "<style>"); count != 1 {
		t.Errorf("expected exactly one <style> block, got: %d", count)
	}
	// style.css contains "font-family" near the top — sanity check
	if !strings.Contains(html, "font-family") {
		t.Errorf("embedded stylesheet missing (no 'font-family' found)")
	}
	// Old exporter had minified CSS with "border-left:4px solid #3b82f6" (no spaces).
	// The new exporter uses the external style.css which has proper formatting.
	if strings.Contains(html, "border-left:4px solid #3b82f6") {
		t.Errorf("found old inline CSS marker; embedded stylesheet not being used")
	}
}

func TestRenderHTML_RendersBranchSummaryCollapsedByDefault(t *testing.T) {
	dir := t.TempDir()
	_ = dir // unused; using buildTestReader
	msg := simpleUserMsg(t, "first message")
	branch := NewBranchSummaryEntry("deadbeef-1234-5678-abcd-ef0123456789", "summary of old branch", false)

	r := buildTestReader(t, msg, branch)
	out, err := RenderHTML(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)

	if !strings.Contains(html, `class="branch-summary"`) {
		t.Errorf("expected branch-summary details block, not found")
	}
	// Must NOT have open attribute when NoCollapse is false
	if strings.Contains(html, `class="branch-summary" open`) {
		t.Errorf("branch-summary should be collapsed by default (no 'open' attribute)")
	}
}

func TestRenderHTML_NoCollapseOpensBranchSummary(t *testing.T) {
	msg := simpleUserMsg(t, "first message")
	branch := NewBranchSummaryEntry("deadbeef-1234-5678-abcd-ef0123456789", "summary of old branch", false)

	r := buildTestReader(t, msg, branch)
	out, err := RenderHTML(r, Options{NoCollapse: true})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)

	if !strings.Contains(html, `class="branch-summary" open`) {
		t.Errorf("NoCollapse=true: expected branch-summary with open attribute")
	}
}

func TestRenderHTML_RendersAllEntryTypes(t *testing.T) {
	userMsg := simpleUserMsg(t, "hello there")

	compaction := NewCompactionEntry("compact summary text", "some-id", 1000, 200, false)

	branchSum := NewBranchSummaryEntry("deadbeef-1234-5678-abcd-ef0123456789", "branch text", false)

	sessionInfo := NewSessionInfoEntry("My Test Session")

	modelChange := NewModelChangeEntry("anthropic", "claude-3-opus")

	thinkingChange := NewThinkingLevelChangeEntry("high")

	customMsg := NewCustomMessageEntry(
		"my_plugin",
		json.RawMessage(`{}`),
		json.RawMessage(`{"role":"user","content":"custom plugin content"}`),
	)

	r := buildTestReader(t, userMsg, compaction, branchSum, sessionInfo, modelChange, thinkingChange, customMsg)
	out, err := RenderHTML(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)

	checks := []struct {
		name    string
		snippet string
	}{
		{"message", `class="message user"`},
		{"compaction", `class="compaction"`},
		{"compaction summary text", "compact summary text"},
		{"branch-summary", `class="branch-summary"`},
		{"branch summary text", "branch text"},
		{"session-info rename", `Renamed to &#34;My Test Session&#34;`},
		{"model-change provider", "anthropic"},
		{"thinking-change level", "high"},
		{"custom-message", "custom plugin content"},
	}
	for _, c := range checks {
		if !strings.Contains(html, c.snippet) {
			t.Errorf("%s: expected %q in output", c.name, c.snippet)
		}
	}
}

func TestRenderHTML_AnsiInBashResult(t *testing.T) {
	ansiRed := "\x1b[31merror\x1b[0m"
	entry := msgWithToolCall(t, "bash", "call-1", ansiRed)

	r := buildTestReader(t, entry)
	out, err := RenderHTML(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)

	// AnsiToHTML converts \x1b[31m → <span style="color:#cc0000">
	if !strings.Contains(html, `color:#cc0000`) {
		t.Errorf("expected ANSI red converted to CSS color in tool-result, html snippet:\n%s", html[strings.Index(html, "tool-result"):min(len(html), strings.Index(html, "tool-result")+500)])
	}
	if !strings.Contains(html, "error") {
		t.Errorf("expected 'error' text in output")
	}
}

func TestRenderHTML_EscapesPlainCodeBlock(t *testing.T) {
	xss := "<script>alert(1)</script>"
	entry := msgWithToolCall(t, "read", "call-2", xss)

	r := buildTestReader(t, entry)
	out, err := RenderHTML(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)

	// Should escape the angle brackets
	if strings.Contains(html, "<script>") {
		t.Errorf("XSS: <script> tag was not escaped in tool-result")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Errorf("expected HTML-escaped script tag in output")
	}
}

func TestRenderHTML_SkipsPositionChangeAndCustom(t *testing.T) {
	posChange := NewPositionChangeEntry("some-target-id")
	custom := NewCustomEntry("my_plugin", json.RawMessage(`{"secret":"data"}`))

	r := buildTestReader(t, posChange, custom)
	out, err := RenderHTML(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)

	// Custom entry data should not appear
	if strings.Contains(html, "secret") {
		t.Errorf("custom entry data should not appear in export")
	}
	// position_change new_position target should not appear as rendered content
	if strings.Contains(html, "some-target-id") {
		t.Errorf("position_change content should not appear in export")
	}
}

func TestRenderHTML_MetadataAndTitle(t *testing.T) {
	r := buildTestReader(t, simpleUserMsg(t, "hi"))
	out, err := RenderHTML(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)

	// Should have a title tag
	if !strings.Contains(html, "<title>") {
		t.Errorf("expected <title> tag")
	}
	// Should have metadata div
	if !strings.Contains(html, `class="metadata"`) {
		t.Errorf("expected metadata div")
	}
	// Should have CWD
	if !strings.Contains(html, "/test/cwd") {
		t.Errorf("expected CWD in metadata")
	}
}

// min is a helper for older Go compatibility in tests.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestRenderHTML_AbandonedBranchesCollapsedByDefault(t *testing.T) {
	// Build a two-branch session:
	//   msg1 (active branch root)
	//     ├── msg2 (continues active)
	//     └── branchSummary (abandoned branch — not on active path)
	//
	// ActiveBranch() returns [msg1, msg2]; branchSummary is abandoned.
	msg1 := simpleUserMsg(t, "first message")
	msg2 := simpleUserMsg(t, "second message — this is the active path")
	abandonedSummary := NewBranchSummaryEntry(msg1.ID, "summary of the abandoned branch", false)

	// Use buildTestReaderAt so we control parent IDs precisely.
	// Chain msg1→msg2 (active), and give abandonedSummary no parent so it's not
	// on the active chain.
	r := buildTestReaderAtBranch(t, t.TempDir(),
		// active branch entries (chained)
		[]Entry{msg1, msg2},
		// abandoned branch entry (not on active chain)
		[][]Entry{{abandonedSummary}},
	)
	out, err := RenderHTML(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)

	// Active branch content must appear
	if !strings.Contains(html, "first message") {
		t.Errorf("active branch msg1 missing from export")
	}
	if !strings.Contains(html, "second message") {
		t.Errorf("active branch msg2 missing from export")
	}

	// Abandoned branch summary content must appear (exported but collapsed)
	if !strings.Contains(html, "summary of the abandoned branch") {
		t.Errorf("abandoned branch summary missing from export")
	}

	// The abandoned branches wrapper must be collapsed (no "open" on the outer details)
	if !strings.Contains(html, `class="abandoned-branches"`) {
		t.Errorf("expected abandoned-branches wrapper in export")
	}
	if strings.Contains(html, `class="abandoned-branches" open`) {
		t.Errorf("abandoned-branches should be collapsed by default (no 'open')")
	}
}

func TestRenderHTML_AbandonedBranchesExpandedWithNoCollapse(t *testing.T) {
	msg1 := simpleUserMsg(t, "first message")
	msg2 := simpleUserMsg(t, "second message — active path")
	abandonedSummary := NewBranchSummaryEntry(msg1.ID, "abandoned content", false)

	r := buildTestReaderAtBranch(t, t.TempDir(),
		[]Entry{msg1, msg2},
		[][]Entry{{abandonedSummary}},
	)
	out, err := RenderHTML(r, Options{NoCollapse: true})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)

	// With NoCollapse, individual branch summaries must have open attribute
	if !strings.Contains(html, `class="branch-summary" open`) {
		t.Errorf("NoCollapse=true: expected branch-summary with 'open' attribute")
	}
}

func TestRenderHTML_NoAbandonedBranchesOmitsWrapper(t *testing.T) {
	msg := simpleUserMsg(t, "single branch message")

	r := buildTestReader(t, msg)
	out, err := RenderHTML(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)

	if strings.Contains(html, `class="abandoned-branches"`) {
		t.Errorf("no abandoned branches: wrapper should not appear")
	}
}

// buildTestReaderAtBranch builds a session file with multiple entry groups.
// Each group's entries are chained by ParentID internally, but there is no
// parent link between groups, so entries in later groups are not reachable from
// the active branch traversal.
func buildTestReaderAtBranch(t *testing.T, dir string, activeBranch []Entry, otherGroups [][]Entry) *Reader {
	t.Helper()
	header := NewHeader("/test/cwd", nil, nil)
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}

	// Write active branch entries, chaining them by ParentID
	var prevID *string
	for i := range activeBranch {
		if prevID != nil {
			activeBranch[i].ParentID = prevID
		}
		if err := w.Append(activeBranch[i]); err != nil {
			t.Fatalf("Append active branch entry %d: %v", i, err)
		}
		prevID = &activeBranch[i].ID
	}

	// Write other groups without parent links (abandoned branches)
	for _, group := range otherGroups {
		for _, e := range group {
			if err := w.Append(e); err != nil {
				t.Fatalf("Append other group entry: %v", err)
			}
		}
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r, err := ReadFile(w.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return r
}

// Ensure buildTestReaderAt is used (suppress unused import warning).
var _ = filepath.Join
