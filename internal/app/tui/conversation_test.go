package tui

import (
	"fmt"
	"testing"
)

func TestShellProgressRetainsFragmentedANSI(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("call", "bash", `{"command":"pwd","exclude_from_context":true}`)
	for _, chunk := range []string{"\x1b[", "31mred", "\x1b", "[0m"} {
		conv.AddToolProgress("call", chunk)
	}
	tool := conv.Snapshot().Components[0].ToolExecution
	if tool.Output != "\x1b[31mred\x1b[0m" || !tool.ExcludeFromContext {
		t.Fatalf("fragmented progress corrupted: %+v", tool)
	}
	conv.addToolResult("call", "bash", "red", false, nil, new(1.0), true)
	tool = conv.Snapshot().Components[0].ToolExecution
	if tool.Output != "red" || !tool.ExcludeFromContext {
		t.Fatalf("final result retained stream fragments: %+v", tool)
	}
}

// SnapshotFrom and SnapshotTail must return exactly the tail of the full
// projection, including a streaming draft and transient items, and release
// the projection memo of items that fall below the window.
func TestConversationSnapshotFromMatchesFullTail(t *testing.T) {
	conv := NewConversationModel()
	for i := range 5 {
		conv.AddSystem(fmt.Sprintf("s%d", i))
	}
	conv.BeginAssistantMessage()
	conv.AppendAssistantText("draft")
	conv.AddThinking()
	full := conv.Snapshot().Components

	check := func(name string, got ChatSnapshot, want int) {
		t.Helper()
		want = min(max(want, 0), len(full))
		if got.Offset != want || len(got.Components) != len(full)-want {
			t.Fatalf("%s: offset %d with %d components, want %d with %d", name, got.Offset, len(got.Components), want, len(full)-want)
		}
		for i, c := range got.Components {
			if c.ID != full[want+i].ID {
				t.Fatalf("%s: component %d = %q, want %q", name, i, c.ID, full[want+i].ID)
			}
		}
	}
	for _, start := range []int{0, 2, 5, 6, len(full), -2, 100} {
		check(fmt.Sprintf("from %d", start), conv.SnapshotFrom(start), start)
	}
	for _, n := range []int{0, 2, len(full), 100} {
		check(fmt.Sprintf("tail %d", n), conv.SnapshotTail(n), len(full)-n)
	}

	conv.SnapshotFrom(3)
	conv.mu.Lock()
	defer conv.mu.Unlock()
	for i := range 3 {
		if conv.committed[i].projection != nil {
			t.Fatalf("item %d below the window kept its projection memo", i)
		}
	}
	if conv.committed[3].projection == nil {
		t.Fatal("item inside the window lost its projection memo")
	}
}

// Dropping history and paging it back keeps every held component at the same
// position, and the boundary lands on the first item of a persisted entry.
func TestConversationDropAndPrependKeepPositions(t *testing.T) {
	conv := NewConversationModel()
	conv.AddPersistedUserMessage("e1", "first", nil)
	conv.AddSystem("notice")
	conv.AddPersistedUserMessage("e2", "second", nil)
	conv.AddToolCallWithID("c1", "bash", `{"command":"ls"}`)
	conv.committed[3].entryID = "e2"
	conv.AddAssistantMessage("tail")
	full := conv.Snapshot()

	if _, _, ok := conv.HistoryBoundary(0); ok {
		t.Fatal("nothing precedes position 0, so there is no boundary")
	}
	boundary, entryID, ok := conv.HistoryBoundary(4)
	if !ok || boundary != 2 || entryID != "e2" {
		t.Fatalf("boundary = %d %q %v, want the first item of e2 at 2", boundary, entryID, ok)
	}

	conv.DropBefore(boundary)
	after := conv.Snapshot()
	if after.Base != 2 || after.Offset != 2 || len(after.Components) != 3 || after.Components[0].ID != full.Components[2].ID {
		t.Fatalf("after drop: base %d offset %d, %d components", after.Base, after.Offset, len(after.Components))
	}
	if got := conv.SnapshotFrom(3).Components[0].ID; got != full.Components[3].ID {
		t.Fatalf("position 3 = %q after the drop, want %q", got, full.Components[3].ID)
	}

	older := NewConversationModel()
	older.AddPersistedUserMessage("e0", "zeroth", nil)
	older.AddPersistedUserMessage("e1", "first", nil)
	conv.PrependHistory(older)
	back := conv.Snapshot()
	if back.Base != 0 || len(back.Components) != 5 || back.Components[0].Text != "zeroth" {
		t.Fatalf("after prepend: base %d, %d components", back.Base, len(back.Components))
	}
	if back.Components[0].ID == full.Components[0].ID {
		t.Fatal("paged-in history reused an ID the renderer may have cached")
	}
	if got := conv.SnapshotFrom(3).Components[0].ID; got != full.Components[3].ID {
		t.Fatalf("position 3 = %q after the prepend, want %q", got, full.Components[3].ID)
	}
}
