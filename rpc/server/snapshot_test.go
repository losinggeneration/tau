package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	sessionpkg "github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/rpc/coord"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/ui"
)

type worstCaseSnapshot struct {
	Live       liveSnapshot       `json:"live"`
	History    historyPage        `json:"history"`
	UIRequests []ui.SharedRequest `json:"ui_requests"`
}

// snapshotAtLimits fills every snapshot bound with characters that JSON
// escapes sixfold, then returns the decoded snapshot and its encoded size.
func snapshotAtLimits(t *testing.T, before string) (worstCaseSnapshot, int) {
	t.Helper()
	const dialogs = 16
	escaped := func(n int) string { return strings.Repeat("<", n) }

	entries := make([]sdk.HistoryEntry, historyPageEntries)
	for i := range entries {
		entry := sdk.HistoryEntry{EntryID: fmt.Sprintf("entry-%02d", i), Message: sdk.Message{Role: "assistant"}}
		empty, err := json.Marshal(historyItem{EntryID: entry.EntryID, Message: &entry.Message})
		if err != nil {
			t.Fatal(err)
		}
		entry.Message.Text = escaped((12<<10 - len(empty)) / 6)
		entries[i] = entry
	}
	rt := newWorkerRuntime()
	rt.sess.historyFn = func() ([]sdk.HistoryEntry, string, error) { return entries, before, nil }

	var asking sync.WaitGroup
	t.Cleanup(asking.Wait)
	h := startWorker(t, rt, nil)
	sessionUI := (<-rt.opts).UI
	observer := <-rt.sess.observerReady
	h.send(t, "ready", `{"type":"get_state"}`)
	h.response(t, "ready")

	big := escaped(2 * liveFieldBytes)
	id := func(kind string, i int) string { return fmt.Sprintf("%s%d", kind, i) + escaped(254) }
	for i := range liveSlots {
		for _, e := range []sdk.Event{
			{Type: "message_start", Payload: map[string]any{"message_id": id("m", i), "role": big}},
			{Type: "text_delta", Payload: map[string]any{"message_id": id("m", i), "delta": big}},
			{Type: "thinking_delta", Payload: map[string]any{"message_id": id("m", i), "delta": big}},
			{Type: "tool_call", Payload: map[string]any{"tool_call_id": id("t", i), "name": big, "input": big}},
			{Type: "tool_progress", Payload: map[string]any{"tool_call_id": id("t", i), "chunk": big}},
			{Type: "bash_execution_update", Payload: map[string]any{"id": id("b", i), "delta": big}},
		} {
			observer(e)
			h.nextEvent(t)
		}
	}

	// Titles fill the dialog descriptor limit once escaped.
	title := escaped(1340)
	for range dialogs {
		asking.Go(func() {
			if _, err := sessionUI.Input(t.Context(), title, ""); err == nil {
				t.Error("dialog resolved without a reply")
			}
		})
	}
	for seen := 0; seen < dialogs; {
		if f := h.nextEvent(t); strings.Contains(string(f.Event), `"type":"ui_input"`) {
			seen++
		}
	}

	h.send(t, "snapshot", `{"type":"get_snapshot","args":{"preview":true}}`)
	r := h.response(t, "snapshot")
	if !r.OK {
		t.Fatalf("snapshot at limits failed: %+v", r.Error)
	}
	data, err := json.Marshal(r.Result)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot worstCaseSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.History.Entries) != historyPageEntries || snapshot.History.Before != before || len(snapshot.UIRequests) != dialogs {
		t.Fatalf("snapshot lost history or dialogs: %d entries, cursor %d bytes, %d dialogs",
			len(snapshot.History.Entries), len(snapshot.History.Before), len(snapshot.UIRequests))
	}
	for _, entry := range snapshot.History.Entries {
		if entry.Omitted || entry.Message == nil {
			t.Fatalf("history entry %s omitted", entry.EntryID)
		}
	}
	return snapshot, len(data)
}

func TestSnapshotFitsFrameAtLiveLimits(t *testing.T) {
	snapshot, size := snapshotAtLimits(t, "older")
	if size < 768<<10 || size >= coord.MaxFrameBytes {
		t.Fatalf("snapshot encodes to %d bytes, want a near-limit result under %d", size, coord.MaxFrameBytes)
	}
	live := snapshot.Live
	if live.Truncated || len(live.Messages) != liveSlots || len(live.Tools) != liveSlots || len(live.Bash) != liveSlots {
		t.Fatalf("live state dropped: truncated=%v messages=%d tools=%d bash=%d", live.Truncated, len(live.Messages), len(live.Tools), len(live.Bash))
	}
	for _, item := range live.Messages {
		if !item.Truncated || len(item.Role) != liveFieldBytes || len(item.Text) != liveFieldBytes || len(item.Thinking) != liveFieldBytes {
			t.Fatalf("message %q not filled to the field limit", item.ID[:2])
		}
	}
	for _, item := range live.Tools {
		if len(item.Name) != liveFieldBytes || len(item.Input) != liveFieldBytes || len(item.Progress) != liveFieldBytes {
			t.Fatalf("tool %q not filled to the field limit", item.ID[:2])
		}
	}
}

func TestSnapshotKeepsLivePreviewsThatFitFrame(t *testing.T) {
	cursor := strings.Repeat("<", 64<<10)
	snapshot, size := snapshotAtLimits(t, cursor)
	if size >= coord.MaxFrameBytes {
		t.Fatalf("snapshot encodes to %d bytes", size)
	}
	live := snapshot.Live
	kept := len(live.Messages) + len(live.Tools) + len(live.Bash)
	if !live.Truncated || live.Tools == nil || live.Bash == nil || kept == 0 || kept == 3*liveSlots {
		t.Fatalf("oversized snapshot live state: truncated=%v messages=%d tools=%d bash=%d", live.Truncated, len(live.Messages), len(live.Tools), len(live.Bash))
	}
	if live.Messages[len(live.Messages)-1].ID[:2] != fmt.Sprint("m", liveSlots-1) {
		t.Fatalf("kept message %q, want the newest", live.Messages[len(live.Messages)-1].ID[:2])
	}
}

func TestWorkerHistoryPagesAcrossCompactions(t *testing.T) {
	cwd, dir := t.TempDir(), t.TempDir()
	storage, err := sessionpkg.NewStorageAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	header := sessionpkg.NewHeader(cwd, nil, nil)
	writer, err := storage.Create(header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if writer != nil {
			if err := writer.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	var parent *string
	var want []string
	// Each entry fits on its own, but a full page by count would exceed the byte limit.
	text := strings.Repeat("x", 16<<10)
	for i := range 3 * historyPageEntries {
		msg := sessionpkg.NewMessageEntry(parent, json.RawMessage(fmt.Sprintf(`{"role":"user","content":%q}`, text)), nil)
		if err := writer.Append(msg); err != nil {
			t.Fatal(err)
		}
		parent = &msg.ID
		if i == historyPageEntries || i == 2*historyPageEntries {
			summary := sessionpkg.NewCompactionEntry("summary", msg.ID, 100, 20, false)
			summary.ParentID = parent
			if err := writer.Append(summary); err != nil {
				t.Fatal(err)
			}
			parent = &summary.ID
			want = append(want, summary.ID)
		}
		want = append(want, msg.ID)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	writer = nil
	rt, err := sdk.NewRuntime(sdk.RuntimeOptions{SessionsDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	h := startWorkerInstance(t, rt, nil, coord.Instance{ID: "instance", CWD: cwd, SessionID: header.ID})
	h.send(t, "snapshot", `{"type":"get_snapshot"}`)
	resp := h.response(t, "snapshot")
	if !resp.OK {
		t.Fatalf("get_snapshot: %+v", resp.Error)
	}
	var snap struct{ History historyPage }
	decodeResult(t, resp, &snap)
	var got []string
	page := snap.History
	const maxPages = 12
	for i := range maxPages {
		if len(page.Entries) == 0 || len(page.Entries) > historyPageEntries {
			t.Fatalf("page %d has %d entries", i, len(page.Entries))
		}
		var ids []string
		for _, entry := range page.Entries {
			if entry.Omitted || entry.Message == nil || (entry.Message.Role == "user" && entry.Message.Text != text) {
				t.Fatalf("page %d lost entry %q", i, entry.EntryID)
			}
			ids = append(ids, entry.EntryID)
		}
		got = append(ids, got...)
		if page.Before == "" {
			break
		}
		if i == maxPages-1 {
			t.Fatal("paging did not reach the start")
		}
		h.send(t, "older", fmt.Sprintf(`{"type":"get_history","args":{"session_id":%q,"before":%q}}`, header.ID, page.Before))
		resp := h.response(t, "older")
		if !resp.OK {
			t.Fatalf("get_history page %d: %+v", i+1, resp.Error)
		}
		decodeResult(t, resp, &page)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("reconstructed history has wrong order or entries: got %v, want %v", got, want)
	}

	var first sdk.Message
	if err := json.Unmarshal(fetchContent(t, h, fmt.Sprintf(`"session_id":%q,"entry_id":%q`, header.ID, want[0])), &first); err != nil {
		t.Fatal(err)
	}
	if first.Role != "user" || first.Text != text {
		t.Fatalf("saved entry content = role %q, %d text bytes", first.Role, len(first.Text))
	}
	h.send(t, "missing", fmt.Sprintf(`{"type":"get_entry_content","args":{"session_id":%q,"entry_id":"missing","offset":0}}`, header.ID))
	if resp := h.response(t, "missing"); resp.OK || resp.Error.Code != coord.CodeNotFound {
		t.Fatalf("missing entry response: %+v", resp.Error)
	}
}

type statsSnapshotSession struct {
	*workerSession
	stats sdk.SessionStats
}

func (s *statsSnapshotSession) Stats() sdk.SessionStats { return s.stats }

func TestWorkerSnapshotUsesSessionStats(t *testing.T) {
	rt := newWorkerRuntime()
	stats := sdk.SessionStats{SessionID: "session", Model: "demo/reasoner", ThinkingLevel: "medium", TotalTokens: 500, ContextWindow: 10000, ContextUsedPct: 5, AutoCompaction: true}
	sess := &statsSnapshotSession{rt.sess, stats}
	rt.newFn = func(sdk.SessionOptions) (sdk.Session, error) { return sess, nil }
	h := startWorker(t, rt, nil)
	h.send(t, "snapshot", `{"type":"get_snapshot"}`)
	var out struct {
		State struct {
			ThinkingLevel string           `json:"thinking_level"`
			Stats         sdk.SessionStats `json:"stats"`
		}
	}
	decodeResult(t, h.response(t, "snapshot"), &out)
	if out.State.ThinkingLevel != stats.ThinkingLevel || out.State.Stats != stats {
		t.Fatalf("snapshot state = %+v, want stats %+v", out.State, stats)
	}
}

func TestWorkerSnapshotTracksCompaction(t *testing.T) {
	rt := newWorkerRuntime()
	h := startWorker(t, rt, nil)
	h.send(t, "ready", `{"type":"get_state"}`)
	if r := h.response(t, "ready"); !r.OK {
		t.Fatalf("ready probe: %+v", r)
	}
	observe := <-rt.sess.observerReady
	for _, phase := range []struct {
		event      string
		compacting bool
	}{
		{"compaction_start", true},
		{"compaction_end", false},
	} {
		observe(sdk.Event{Type: phase.event, SessionID: "session"})
		event := h.nextEvent(t)
		h.send(t, phase.event, `{"type":"get_snapshot"}`)
		var snapshot struct {
			Position coord.Position
			State    struct {
				IsCompacting bool `json:"is_compacting"`
			}
		}
		decodeResult(t, h.response(t, phase.event), &snapshot)
		if event.Position == nil || snapshot.Position != *event.Position || snapshot.State.IsCompacting != phase.compacting {
			t.Fatalf("after %s: snapshot at %+v reports compacting=%v, want %+v and compacting=%v", phase.event, snapshot.Position, snapshot.State.IsCompacting, event.Position, phase.compacting)
		}
	}
}

func TestWorkerSnapshotPreservesLargeBashResult(t *testing.T) {
	rt := newWorkerRuntime()
	output := strings.Repeat("line of bash output\n", 1400)
	rt.sess.historyFn = func() ([]sdk.HistoryEntry, string, error) {
		return []sdk.HistoryEntry{
			{EntryID: "prompt", Message: sdk.Message{Role: "user", Text: "test"}},
			{EntryID: "result", Message: sdk.Message{Role: "toolResult", ToolCallID: "bash-call", ToolName: "bash", Text: output}},
			{EntryID: "reply", Message: sdk.Message{Role: "assistant", Text: "done"}},
		}, "", nil
	}
	h := startWorker(t, rt, nil)
	h.send(t, "snapshot", `{"type":"get_snapshot"}`)
	var out worstCaseSnapshot
	decodeResult(t, h.response(t, "snapshot"), &out)
	if len(out.History.Entries) != 3 {
		t.Fatalf("history has %d entries, want 3", len(out.History.Entries))
	}
	item := out.History.Entries[1]
	if item.Omitted || item.Message == nil || item.Message.ToolCallID != "bash-call" || item.Message.Text != output {
		t.Fatalf("bash result lost: omitted=%v, message present=%v", item.Omitted, item.Message != nil)
	}
}

func TestWorkerSnapshotKeepsOversizedToolResultMetadata(t *testing.T) {
	rt := newWorkerRuntime()
	output := strings.Repeat("x", historyPageBytes+1)
	rt.sess.historyFn = func() ([]sdk.HistoryEntry, string, error) {
		return []sdk.HistoryEntry{
			{EntryID: "call", Message: sdk.Message{Role: "assistant", Text: "running bash"}},
			{EntryID: "result", Message: sdk.Message{Role: "toolResult", ToolCallID: "bash-call", ToolName: "bash", IsError: true, Text: output}},
			{EntryID: "oversized", Message: sdk.Message{Role: "assistant", Text: output}},
			{EntryID: "reply", Message: sdk.Message{Role: "assistant", Text: "done"}},
		}, "older", nil
	}
	h := startWorker(t, rt, nil)
	h.send(t, "snapshot", `{"type":"get_snapshot","args":{"preview":true}}`)
	var out worstCaseSnapshot
	decodeResult(t, h.response(t, "snapshot"), &out)
	page := out.History
	if len(page.Entries) != 4 || page.Before != "older" {
		t.Fatalf("history has %d entries and cursor %q, want 4 entries and cursor older", len(page.Entries), page.Before)
	}
	item := page.Entries[1]
	msg := item.Message
	if item.EntryID != "result" || item.Omitted || !item.Truncated || msg == nil || msg.Role != "toolResult" || msg.ToolCallID != "bash-call" || msg.ToolName != "bash" || !msg.IsError || msg.Text != output[:historyPreviewBytes] {
		t.Fatal("oversized tool result did not retain its completion metadata and output preview")
	}
	if item := page.Entries[2]; item.EntryID != "oversized" || item.Omitted || !item.Truncated || item.Message == nil || item.Message.Text != output[:historyPreviewBytes] {
		t.Fatal("oversized assistant entry was not previewed")
	}
	if item := page.Entries[3]; item.EntryID != "reply" || item.Omitted || item.Message == nil || item.Message.Text != "done" {
		t.Fatal("reply after oversized entries was lost")
	}
}

func imageMessage() sdk.Message {
	return sdk.Message{
		Role: "user", Text: "look",
		Content: []ai.ContentBlock{
			{Type: ai.ContentBlockText, Text: "look"},
			{Type: ai.ContentBlockImage, MimeType: "image/png", Data: "cG5n"},
			{Type: ai.ContentBlockImage, MimeType: "image/jpeg", Data: "anBlZw=="},
		},
		Images: []ai.ContentBlock{
			{Type: ai.ContentBlockImage, MimeType: "image/png", Data: "cG5n"},
			{Type: ai.ContentBlockImage, MimeType: "image/jpeg", Data: "anBlZw=="},
		},
	}
}

func TestReadHistoryStripsImageData(t *testing.T) {
	withContent := imageMessage()
	onlyImages := sdk.Message{Role: "user", Images: []ai.ContentBlock{{Type: ai.ContentBlockImage, MimeType: "image/gif", Data: "Z2lm"}}}
	rt := newWorkerRuntime()
	rt.sess.historyFn = func() ([]sdk.HistoryEntry, string, error) {
		return []sdk.HistoryEntry{{EntryID: "content", Message: withContent}, {EntryID: "images", Message: onlyImages}}, "", nil
	}
	page, err := readHistory(rt.sess, "", previewHistory)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 || page.Entries[0].Truncated || page.Entries[1].Truncated {
		t.Fatalf("history = %+v", page.Entries)
	}
	msg := page.Entries[0].Message
	if msg.Images != nil || len(msg.Content) != 3 || msg.Content[0].Text != "look" {
		t.Fatalf("content images kept duplicates or lost text: %+v", msg)
	}
	for i, want := range []string{"image/png", "image/jpeg"} {
		if b := msg.Content[i+1]; b.Type != ai.ContentBlockImage || b.MimeType != want || b.Data != "" {
			t.Fatalf("image %d placeholder = %+v", i, b)
		}
	}
	if b := page.Entries[1].Message.Images; len(b) != 1 || b[0].MimeType != "image/gif" || b[0].Data != "" {
		t.Fatalf("images placeholder = %+v", b)
	}
	if withContent.Content[1].Data == "" || onlyImages.Images[0].Data == "" {
		t.Fatal("stripping image data changed the session's message")
	}
}

func TestReadHistoryPreviewKeepsToolCalls(t *testing.T) {
	text := strings.Repeat("é", historyPageBytes)
	msg := sdk.Message{Role: "assistant", Text: text, Content: []ai.ContentBlock{
		{Type: ai.ContentBlockText, Text: text},
		{Type: ai.ContentBlockImage, MimeType: "image/png", Data: strings.Repeat("A", 1<<20)},
		{Type: "toolCall", ID: "call", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
	}}
	rt := newWorkerRuntime()
	rt.sess.historyFn = func() ([]sdk.HistoryEntry, string, error) {
		return []sdk.HistoryEntry{{EntryID: "big", Message: msg}}, "", nil
	}
	page, err := readHistory(rt.sess, "", previewHistory)
	if err != nil {
		t.Fatal(err)
	}
	item := page.Entries[0]
	if !item.Truncated || item.Omitted || item.Message == nil {
		t.Fatalf("entry not previewed: %+v", item)
	}
	p := item.Message
	if !utf8.ValidString(p.Text) || len(p.Text) == 0 || len(p.Text) > historyPreviewBytes || len(p.Content) != 3 {
		t.Fatalf("preview text %d bytes, %d blocks", len(p.Text), len(p.Content))
	}
	if b := p.Content[1]; b.MimeType != "image/png" || b.Data != "" {
		t.Fatalf("image placeholder = %+v", b)
	}
	if b := p.Content[2]; b.ID != "call" || b.Name != "bash" || string(b.Arguments) != `{"command":"ls"}` {
		t.Fatalf("tool call = %+v", b)
	}
}

// fetchContent pages get_entry_content from offset 0 and returns the
// reassembled bytes.
func fetchContent(t *testing.T, h *workerHarness, args string) []byte {
	t.Helper()
	var data []byte
	var revision string
	for i := 0; ; i++ {
		id := fmt.Sprintf("content-%d", i)
		h.send(t, id, fmt.Sprintf(`{"type":"get_entry_content","args":{%s,"offset":%d,"revision":%q}}`, args, len(data), revision))
		resp := h.response(t, id)
		if !resp.OK {
			t.Fatalf("get_entry_content at %d: %+v", len(data), resp.Error)
		}
		var chunk entryContent
		decodeResult(t, resp, &chunk)
		if len(chunk.Data) > entryContentChunkBytes || (chunk.More && len(chunk.Data) == 0) {
			t.Fatalf("chunk of %d bytes, more=%v", len(chunk.Data), chunk.More)
		}
		if chunk.More && (chunk.Revision == "" || revision != "" && revision != chunk.Revision) {
			t.Fatalf("revision changed from %q to %q", revision, chunk.Revision)
		}
		revision = chunk.Revision
		data = append(data, chunk.Data...)
		if !chunk.More {
			return data
		}
	}
}

func TestWorkerEntryContentPagesSavedEntry(t *testing.T) {
	rt := newWorkerRuntime()
	msg := imageMessage()
	msg.Text = strings.Repeat("x", 3*entryContentChunkBytes)
	rt.sess.entryFn = func(id string) (sdk.HistoryEntry, error) {
		if id != "entry" {
			return sdk.HistoryEntry{}, sdk.ErrHistoryEntryNotFound
		}
		return sdk.HistoryEntry{EntryID: id, Message: msg}, nil
	}
	h := startWorker(t, rt, nil)

	var got sdk.Message
	if err := json.Unmarshal(fetchContent(t, h, `"session_id":"session","entry_id":"entry"`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != msg.Text || got.Images != nil || len(got.Content) != 3 || got.Content[1].Data != "" || got.Content[2].MimeType != "image/jpeg" {
		t.Fatalf("saved entry content = role %q, %d text bytes, images %v, content %+v", got.Role, len(got.Text), got.Images, got.Content[1:])
	}
	if image := fetchContent(t, h, `"session_id":"session","entry_id":"entry","image_index":1`); string(image) != "anBlZw==" {
		t.Fatalf("image 1 = %q", image)
	}

	for _, tc := range []struct{ name, args, code string }{
		{"missing", `"session_id":"session","entry_id":"gone","offset":0`, coord.CodeNotFound},
		{"no image", `"session_id":"session","entry_id":"entry","image_index":2,"offset":0`, coord.CodeNotFound},
		{"other session", `"session_id":"other","entry_id":"entry","offset":0`, coord.CodeResyncRequired},
		{"negative offset", `"session_id":"session","entry_id":"entry","offset":-1`, coord.CodeInvalidFrame},
		{"missing revision", `"session_id":"session","entry_id":"entry","image_index":0,"offset":5`, coord.CodeResyncRequired},
		{"live image", `"session_id":"session","entry_id":"entry","live_kind":"tool","image_index":0,"offset":0`, coord.CodeInvalidFrame},
		{"bad kind", `"session_id":"session","entry_id":"entry","live_kind":"other","offset":0`, coord.CodeInvalidFrame},
		{"no live item", `"session_id":"session","entry_id":"entry","live_kind":"bash","offset":0`, coord.CodeNotFound},
	} {
		h.send(t, tc.name, `{"type":"get_entry_content","args":{`+tc.args+`}}`)
		if resp := h.response(t, tc.name); resp.OK || resp.Error == nil || resp.Error.Code != tc.code {
			t.Errorf("%s: response %+v, want code %s", tc.name, resp.Error, tc.code)
		}
	}
}

func TestWorkerEntryContentServesFullLiveItem(t *testing.T) {
	rt := newWorkerRuntime()
	h := startWorker(t, rt, nil)
	observer := <-rt.sess.observerReady
	h.send(t, "ready", `{"type":"get_state"}`)
	h.response(t, "ready")

	input := strings.Repeat("i", 300<<10)
	observer(sdk.Event{Type: "tool_call", Payload: map[string]any{"tool_call_id": "call", "name": "write", "input": input}})
	h.nextEvent(t)
	observer(sdk.Event{Type: "tool_progress", Payload: map[string]any{"tool_call_id": "call", "chunk": "half"}})
	h.nextEvent(t)

	var item liveItem
	if err := json.Unmarshal(fetchContent(t, h, `"session_id":"session","entry_id":"call","live_kind":"tool"`), &item); err != nil {
		t.Fatal(err)
	}
	if item.ID != "call" || item.Name != "write" || item.Input != input || item.Progress != "half" || item.Truncated {
		t.Fatalf("live tool = id %q name %q, %d input bytes, progress %q, truncated %v", item.ID, item.Name, len(item.Input), item.Progress, item.Truncated)
	}

	h.send(t, "snapshot", `{"type":"get_snapshot"}`)
	var snap worstCaseSnapshot
	decodeResult(t, h.response(t, "snapshot"), &snap)
	if tools := snap.Live.Tools; len(tools) != 1 || len(tools[0].Input) != liveFieldBytes || !tools[0].Truncated {
		t.Fatalf("snapshot live tools = %+v", tools)
	}
}

func TestWorkerEntryContentIsolatesReadsAndInvalidatesOnContextEdit(t *testing.T) {
	rt := newWorkerRuntime()
	h := startWorker(t, rt, nil)
	observe := <-rt.sess.observerReady
	h.send(t, "ready", `{"type":"get_state"}`)
	h.response(t, "ready")

	begin := func(id, input string) entryContent {
		t.Helper()
		observe(sdk.Event{Type: "tool_call", Payload: map[string]any{"tool_call_id": "call", "name": "write", "input": input}})
		h.nextEvent(t)
		h.send(t, id, `{"type":"get_entry_content","args":{"session_id":"session","entry_id":"call","live_kind":"tool"}}`)
		var part entryContent
		decodeResult(t, h.response(t, id), &part)
		if !part.More || part.Revision == "" {
			t.Fatalf("initial read didn't return a revision: %q", part.Revision)
		}
		return part
	}
	next := func(id string, part entryContent) Response {
		t.Helper()
		h.send(t, id, fmt.Sprintf(`{"type":"get_entry_content","args":{"session_id":"session","entry_id":"call","live_kind":"tool","revision":%q,"offset":%d}}`, part.Revision, len(part.Data)))
		return h.response(t, id)
	}
	original := strings.Repeat("a", 300<<10)
	a := begin("first-read", original)
	b := begin("second-read", strings.Repeat("b", 300<<10))
	if a.Revision == b.Revision {
		t.Fatal("two reads share a revision")
	}
	var tail entryContent
	decodeResult(t, next("first-tail", a), &tail)
	var item liveItem
	if err := json.Unmarshal(append(a.Data, tail.Data...), &item); err != nil || item.Input != original {
		t.Fatalf("first read changed after another client started reading: %v", err)
	}
	observe(sdk.Event{Type: "context_edited", SessionID: "session"})
	h.nextEvent(t)
	if resp := next("second-tail", b); resp.OK || resp.Error == nil || resp.Error.Code != coord.CodeResyncRequired {
		t.Fatalf("read survived context edit: %+v", resp)
	}
}

func TestLiveTextKeepsBoundedTail(t *testing.T) {
	var text liveText
	text.set("start")
	for range 5 {
		text.add(strings.Repeat("a", liveContentBytes/2))
	}
	text.add("ü-end")
	if len(text.buf) > 2*liveContentBytes {
		t.Fatalf("buffer grew to %d bytes", len(text.buf))
	}
	full, cut := text.tail(liveContentBytes)
	if !cut || len(full) != liveContentBytes || !strings.HasSuffix(full, "ü-end") {
		t.Fatalf("tail = %d bytes, cut %v", len(full), cut)
	}
	preview, cut := text.tail(5)
	if !cut || preview != "-end" {
		t.Fatalf("preview = %q, cut %v", preview, cut)
	}
	text.set("fresh")
	if s, cut := text.tail(liveContentBytes); s != "fresh" || cut {
		t.Fatalf("set kept old content: %q %v", s, cut)
	}
}

func TestLiveStateTracksUsageLimitWait(t *testing.T) {
	const wait = `{"type":"usage_limit_wait","provider":"openai","resets_at":"2026-11-23T12:00:00Z"}`
	for _, tc := range []struct{ name, kind, event string }{
		{"retry starts", "message_start", `{"message_id":"m2","role":"assistant"}`},
		{"run ends", "agent_end", `{}`},
		{"session ends", "session_end", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var l liveState
			if err := l.update("usage_limit_wait", json.RawMessage(wait)); err != nil {
				t.Fatal(err)
			}
			got := l.snapshot().UsageLimitWait
			if got == nil || got.Provider != "openai" || got.ResetsAt != "2026-11-23T12:00:00Z" {
				t.Fatalf("wait = %+v", got)
			}
			if err := l.update(tc.kind, json.RawMessage(tc.event)); err != nil {
				t.Fatal(err)
			}
			if got := l.snapshot().UsageLimitWait; got != nil {
				t.Fatalf("wait survived %s: %+v", tc.kind, got)
			}
		})
	}
}

func TestHistoryPreviewKeepsErrorMessage(t *testing.T) {
	msg := sdk.Message{Role: "assistant", IsError: true, ErrorMessage: "status 429"}
	if got := historyPreview(msg).ErrorMessage; got != "status 429" {
		t.Fatalf("preview error = %q", got)
	}
}
