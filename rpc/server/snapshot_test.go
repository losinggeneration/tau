package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	sessionpkg "github.com/losinggeneration/tau/agent/session"
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

	h.send(t, "snapshot", `{"type":"get_snapshot","args":{}}`)
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

func TestSnapshotDropsLiveStateWhenOverFrame(t *testing.T) {
	cursor := strings.Repeat("<", 64<<10)
	snapshot, size := snapshotAtLimits(t, cursor)
	if size >= coord.MaxFrameBytes {
		t.Fatalf("snapshot encodes to %d bytes", size)
	}
	live := snapshot.Live
	if !live.Truncated || live.Messages == nil || len(live.Messages)+len(live.Tools)+len(live.Bash) != 0 {
		t.Fatalf("oversized snapshot kept live state: truncated=%v messages=%d tools=%d bash=%d", live.Truncated, len(live.Messages), len(live.Tools), len(live.Bash))
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
	for i := range 3 * historyPageEntries {
		msg := sessionpkg.NewMessageEntry(parent, json.RawMessage(fmt.Sprintf(`{"role":"user","content":"m%d"}`, i)), nil)
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
	for i := range 4 {
		if len(page.Entries) == 0 || len(page.Entries) > historyPageEntries {
			t.Fatalf("page %d has %d entries", i, len(page.Entries))
		}
		var ids []string
		for _, entry := range page.Entries {
			ids = append(ids, entry.EntryID)
		}
		got = append(ids, got...)
		if page.Before == "" {
			break
		}
		if i == 3 {
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
}
