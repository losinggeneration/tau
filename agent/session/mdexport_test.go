package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderMarkdown_Header(t *testing.T) {
	r := buildTestReader(t, simpleUserMsg(t, "hello"))
	out, err := RenderMarkdown(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	md := string(out)

	if !strings.Contains(md, "# Session:") {
		t.Errorf("expected '# Session:' heading, got:\n%s", md)
	}
	if !strings.Contains(md, "**Created:**") {
		t.Errorf("expected '**Created:**' in output")
	}
	if !strings.Contains(md, "**CWD:**") {
		t.Errorf("expected '**CWD:**' in output")
	}
	if !strings.Contains(md, "/test/cwd") {
		t.Errorf("expected CWD value '/test/cwd' in output")
	}
}

func TestRenderMarkdown_RendersAllEntryTypes(t *testing.T) {
	userMsg := simpleUserMsg(t, "hello there")

	assistantEntry := msgWithToolCall(t, "bash", "call-1", "output text")

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

	r := buildTestReader(t, userMsg, assistantEntry, compaction, branchSum, sessionInfo, modelChange, thinkingChange, customMsg)
	out, err := RenderMarkdown(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	md := string(out)

	checks := []struct {
		name    string
		snippet string
	}{
		{"user message heading", "### User"},
		{"user message content", "hello there"},
		{"assistant message heading", "### Assistant"},
		{"tool call label", "**Tool:** `bash`"},
		{"tool result label", "**Tool Result:**"},
		{"tool result content", "output text"},
		{"compaction heading", "### Compaction"},
		{"compaction token counts", "1000 → 200"},
		{"compaction summary", "compact summary text"},
		{"branch summary heading", "### Branch Summary"},
		{"branch from id", "deadbeef"},
		{"branch summary text", "branch text"},
		{"session info rename", "**Renamed:** My Test Session"},
		{"model change", "**Model:** anthropic/claude-3-opus"},
		{"thinking level", "**Thinking level:** high"},
		{"custom message heading", "### Custom (my_plugin)"},
		{"custom message content", "custom plugin content"},
	}
	for _, c := range checks {
		if !strings.Contains(md, c.snippet) {
			t.Errorf("%s: expected %q in output\n---\n%s", c.name, c.snippet, md)
		}
	}
}

func TestRenderMarkdown_StripsAnsi(t *testing.T) {
	ansiRed := "\x1b[31merror\x1b[0m"
	entry := msgWithToolCall(t, "bash", "call-1", ansiRed)

	r := buildTestReader(t, entry)
	out, err := RenderMarkdown(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	md := string(out)

	if strings.Contains(md, "\x1b") {
		t.Errorf("output contains raw ANSI escape character")
	}
	if strings.Contains(md, "\x1b[") {
		t.Errorf("output contains ANSI escape sequence")
	}
	if !strings.Contains(md, "error") {
		t.Errorf("expected word 'error' to be preserved after ANSI stripping")
	}
}

func TestRenderMarkdown_SkipsPositionChangeAndCustom(t *testing.T) {
	posChange := NewPositionChangeEntry("some-target-id")
	custom := NewCustomEntry("my_plugin", json.RawMessage(`{"secret":"data"}`))

	r := buildTestReader(t, posChange, custom)
	out, err := RenderMarkdown(r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	md := string(out)

	if strings.Contains(md, "some-target-id") {
		t.Errorf("position_change content should not appear in export")
	}
	if strings.Contains(md, "secret") {
		t.Errorf("custom entry data should not appear in export")
	}
}

// msgAsEntry wraps a message map in a proper message Entry with the given parent.
func msgAsEntry(t *testing.T, parentID *string, content string) (Entry, *string) {
	t.Helper()
	msg := map[string]string{"role": "user", "content": content}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	e := NewMessageEntry(nil, data, nil)
	e.ParentID = parentID
	id := e.ID
	return e, &id
}

func TestRenderMarkdown_ActiveBranchOnly(t *testing.T) {
	// Build a session with two branches:
	//   A (user: "main message") --> B (user: "active path") --> C (user: "final active")
	//                           └--> branch_summary ("abandoned message")
	//
	// After the branch_summary entry, we add C with parent B so the active branch
	// is A -> B -> C. The branch_summary is NOT on that chain, so it should be
	// excluded from the markdown output.
	dir := t.TempDir()
	header := NewHeader("/test/cwd", nil, nil)
	w, err := CreateSessionFile(dir, header)
	if err != nil {
		t.Fatalf("CreateSessionFile: %v", err)
	}

	var aID, bID, cID string

	// Entry A: root message
	a, aPtr := msgAsEntry(t, nil, "main message")
	aID = *aPtr
	if err := w.Append(a); err != nil {
		t.Fatalf("Append A: %v", err)
	}

	// Entry B: continues active path
	b, bPtr := msgAsEntry(t, &aID, "active path")
	bID = *bPtr
	if err := w.Append(b); err != nil {
		t.Fatalf("Append B: %v", err)
	}

	// Branch summary entry: parent is B, but we DON'T continue this chain.
	// This makes it an abandoned branch — not reachable from the active tip.
	branchSummary := NewBranchSummaryEntry(aID, "this branch was abandoned", false)
	branchSummary.ParentID = &bID
	if err := w.Append(branchSummary); err != nil {
		t.Fatalf("Append branch summary: %v", err)
	}

	// Entry C: continues the active path by pointing back to B (same parent as
	// the branch summary). This makes C the active tip, and the branch_summary
	// entry above is not on the active branch path.
	c, cPtr := msgAsEntry(t, &bID, "final active message")
	cID = *cPtr
	if err := w.Append(c); err != nil {
		t.Fatalf("Append C: %v", err)
	}

	// A position_change entry at the end tells CurrentPosition() that C is the
	// active head. Without it, CurrentPosition() would return C's ID anyway
	// since it's the last message entry.
	posChange := NewPositionChangeEntry(cID)
	posChange.ParentID = &cID
	if err := w.Append(posChange); err != nil {
		t.Fatalf("Append position_change: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := ReadFile(w.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Verify the active branch is A -> B -> C.
	active := r.ActiveBranch()
	if len(active) != 3 {
		t.Fatalf("expected 3 entries on active branch, got %d: %v", len(active), active)
	}

	out, err := RenderMarkdown(r, Options{})
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	md := string(out)

	// Active branch content MUST appear.
	if !strings.Contains(md, "main message") {
		t.Errorf("expected active branch content 'main message' in output")
	}
	if !strings.Contains(md, "active path") {
		t.Errorf("expected active branch content 'active path' in output")
	}
	if !strings.Contains(md, "final active message") {
		t.Errorf("expected active branch content 'final active message' in output")
	}

	// Abandoned branch content MUST NOT appear.
	if strings.Contains(md, "this branch was abandoned") {
		t.Errorf("abandoned branch content 'this branch was abandoned' should not appear in markdown export")
	}
	if strings.Contains(md, "abandoned") {
		t.Errorf("abandoned branch text should not appear in markdown export")
	}
}
