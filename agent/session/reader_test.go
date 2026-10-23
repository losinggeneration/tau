package session

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// forgetFixture writes m1 <- m2 <- m3 <- m4 with a compaction anchored at m3
// and returns the reader plus the entries.
func forgetFixture(t *testing.T) (*Reader, []Entry, Entry) {
	t.Helper()
	w, err := CreateSessionFile(t.TempDir(), NewHeader("/test", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	var msgs []Entry
	var parent *string
	for i, role := range []string{"user", "assistant", "user", "assistant"} {
		m := NewMessageEntry(parent, json.RawMessage(`{"role":"`+role+`","content":"`+string(rune('1'+i))+`"}`), nil)
		if err := w.Append(m); err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			c := NewCompactionEntry("sum", m.ID, 1, 1, false)
			if err := w.Append(c); err != nil {
				t.Fatal(err)
			}
			msgs = append(msgs, c)
		}
		msgs = append(msgs, m)
		parent = &m.ID
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := ReadFile(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	// msgs: m1 m2 compaction m3 m4
	return r, []Entry{msgs[0], msgs[1], msgs[3], msgs[4]}, msgs[2]
}

func TestForgetSet(t *testing.T) {
	r, m, compaction := forgetFixture(t)

	tests := []struct {
		name       string
		from       string
		wantIDs    []string
		wantParent string
	}{
		{"last message", m[3].ID, []string{m[3].ID}, m[2].ID},
		{"compaction anchor drops the compaction", m[2].ID, []string{m[2].ID, m[3].ID, compaction.ID}, m[1].ID},
		{"compaction summary", compaction.ID, []string{m[2].ID, m[3].ID, compaction.ID}, m[1].ID},
		{"first message", m[0].ID, []string{m[0].ID, m[1].ID, m[2].ID, m[3].ID, compaction.ID}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, parent, err := r.ForgetSet(tt.from)
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(ids)
			slices.Sort(tt.wantIDs)
			if !slices.Equal(ids, tt.wantIDs) || parent != tt.wantParent {
				t.Errorf("ForgetSet = %v, %q; want %v, %q", ids, parent, tt.wantIDs, tt.wantParent)
			}
		})
	}

	if _, _, err := r.ForgetSet("missing"); err == nil {
		t.Error("ForgetSet(missing) succeeded")
	}
}

func TestAllEntriesPlacesCompactionBeforeAnchor(t *testing.T) {
	r, m, compaction := forgetFixture(t)
	var got []string
	for _, e := range r.AllEntries() {
		got = append(got, e.ID)
	}
	want := []string{m[0].ID, m[1].ID, compaction.ID, m[2].ID, m[3].ID}
	if !slices.Equal(got, want) {
		t.Errorf("AllEntries = %v, want %v", got, want)
	}
}

func TestContextEditsPreserveHistoryAndBranchIsolation(t *testing.T) {
	first := NewMessageEntry(nil, json.RawMessage(`{"role":"assistant","content":"original","model":"test"}`), nil)
	edit := NewEntry(EntryTypeContextEdit, &first.ID)
	edit.TargetID = &first.ID
	edit.Replacement = json.RawMessage(`{"content":"replacement"}`)
	r := &Reader{entries: []Entry{first, edit}}
	context := r.ToAgentMessages()
	if len(context) != 1 || context[0].Content[0].Text != "replacement" || context[0].Model != "test" {
		t.Fatalf("context=%+v", context)
	}
	if r.AllMessages()[0].Content[0].Text != "original" {
		t.Fatal("display history was edited")
	}
	omit := NewEntry(EntryTypeContextEdit, &edit.ID)
	omit.TargetID = &first.ID
	omit.Replacement = json.RawMessage(`null`)
	r.entries = append(r.entries, omit)
	if len(r.ToAgentMessages()) != 0 {
		t.Fatal("omitted entry remains in context")
	}
	branch := NewMessageEntry(&first.ID, json.RawMessage(`{"role":"user","content":"branch"}`), nil)
	r.entries = append(r.entries, branch)
	if got := r.ToAgentMessages(); len(got) != 2 || got[0].Content[0].Text != "original" {
		t.Fatalf("edit leaked across branch: %+v", got)
	}
}

func TestContextEditSurvivesFileReload(t *testing.T) {
	writer, err := CreateSessionFile(t.TempDir(), NewHeader("/test", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	first := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"original"}`), nil)
	edit := NewEntry(EntryTypeContextEdit, &first.ID)
	edit.TargetID = &first.ID
	edit.Replacement = json.RawMessage(`null`)
	for _, entry := range []Entry{first, edit} {
		if err := writer.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := ReadFile(writer.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.ToAgentMessages()) != 0 || len(reader.AllMessages()) != 1 {
		t.Fatal("reload lost context/display distinction")
	}
}

func TestLinkedCompactionDoesNotApplyToSiblingWithSharedAnchor(t *testing.T) {
	first := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"shared"}`), nil)
	compact := NewCompactionEntry("other branch", first.ID, 10, 2, false)
	compact.ParentID = &first.ID
	sibling := NewMessageEntry(&first.ID, json.RawMessage(`{"role":"user","content":"sibling"}`), nil)
	reader := &Reader{header: NewHeader("/test", nil, nil), entries: []Entry{first, compact, sibling}}
	manager := NewManager("/test")
	manager.LoadEntries(reader)
	for name, entries := range map[string][]Entry{"reader": reader.ContextEntries(), "manager": manager.ContextEntries(), "display": reader.AllEntries()} {
		if len(entries) != 2 || entries[0].ID != first.ID || entries[1].ID != sibling.ID {
			t.Errorf("%s context includes sibling compaction: %+v", name, entries)
		}
	}
	// A descendant of the compaction must still see its summary.
	descendant := NewMessageEntry(&compact.ID, json.RawMessage(`{"role":"user","content":"after compaction"}`), nil)
	reader.entries = append(reader.entries, descendant)
	manager.LoadEntries(reader)
	for name, entries := range map[string][]Entry{"reader": reader.ContextEntries(), "manager": manager.ContextEntries(), "display": reader.AllEntries()} {
		if len(entries) != 3 || entries[0].ID != compact.ID || entries[1].ID != first.ID || entries[2].ID != descendant.ID {
			t.Errorf("%s lost active compaction: %+v", name, entries)
		}
	}
}

func TestForgetSetDoesNotDeleteSiblingCompaction(t *testing.T) {
	first := NewMessageEntry(nil, json.RawMessage(`{"role":"user","content":"shared"}`), nil)
	compact := NewCompactionEntry("sibling summary", first.ID, 10, 2, false)
	compact.ParentID = &first.ID
	sibling := NewMessageEntry(&first.ID, json.RawMessage(`{"role":"user","content":"sibling"}`), nil)
	reader := &Reader{entries: []Entry{first, compact, sibling}}
	if _, _, err := reader.ForgetSet(compact.ID); err == nil {
		t.Fatal("accepted compaction from sibling branch")
	}
	ids, _, err := reader.ForgetSet(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(ids, compact.ID) {
		t.Fatal("deleting shared anchor also deletes sibling compaction")
	}
}

func TestLinkedCompactionIsCurrentPositionBeforeNextMessage(t *testing.T) {
	manager := NewManager("/test")
	first := manager.AppendMessage(json.RawMessage(`{"role":"user","content":"first"}`), nil)
	compact := manager.AppendCompaction("summary", first, 10, 2)
	reader := manager.ToReader("")
	if got := reader.CurrentPosition(); got != compact {
		t.Fatalf("position = %q, want compaction %q", got, compact)
	}
	entries := reader.ContextEntries()
	if len(entries) != 2 || entries[0].ID != compact || entries[1].ID != first {
		t.Fatalf("context = %+v", entries)
	}
}

func TestCompactionRetainsEarlierSystemDeclarations(t *testing.T) {
	system := NewMessageEntry(nil, json.RawMessage(`{"role":"system","content":"policy","sections":{"mode":"strict"},"toolsAdded":[{"name":"read","parameters":{}}]}`), nil)
	old := NewMessageEntry(&system.ID, json.RawMessage(`{"role":"user","content":"old"}`), nil)
	kept := NewMessageEntry(&old.ID, json.RawMessage(`{"role":"user","content":"kept"}`), nil)
	compact := NewCompactionEntry("summary", kept.ID, 10, 2, false)
	reader := &Reader{header: NewHeader("/test", nil, nil), entries: []Entry{system, old, kept, compact}}
	manager := NewManager("/test")
	manager.LoadEntries(reader)
	for name, entries := range map[string][]Entry{"reader": reader.ContextEntries(), "manager": manager.ContextEntries()} {
		if len(entries) != 3 || entries[0].ID != system.ID || entries[1].ID != compact.ID || entries[2].ID != kept.ID {
			t.Errorf("%s lost baseline: %+v", name, entries)
		}
	}
	edit := NewEntry(EntryTypeContextEdit, &kept.ID)
	edit.TargetID = &system.ID
	edit.Replacement = json.RawMessage(`null`)
	reader.entries = append(reader.entries, edit)
	manager.LoadEntries(reader)
	for name, entries := range map[string][]Entry{"reader": reader.ContextEntries(), "manager": manager.ContextEntries()} {
		if len(entries) != 2 || entries[0].ID != compact.ID || entries[1].ID != kept.ID {
			t.Errorf("%s resurrected omitted system: %+v", name, entries)
		}
	}
}

func TestToolNamespaceSurvivesPersistedHistory(t *testing.T) {
	w, err := CreateSessionFile(t.TempDir(), NewHeader("/test", nil, nil))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	entry := NewMessageEntry(nil, json.RawMessage(`{"role":"assistant","provider":"openai","api":"openai-responses","model":"gpt","content":[{"type":"toolCall","id":"call|fc_1","name":"lookup","arguments":{},"namespace":"dynamic_tools"}]}`), nil)
	require.NoError(t, w.Append(entry))
	require.NoError(t, w.Close())
	r, err := ReadFile(w.Path())
	require.NoError(t, err)
	for _, messages := range [][]*Message{r.ToAgentMessages(), r.AllMessages()} {
		require.Len(t, messages, 1)
		data, err := json.Marshal(messages[0].Content)
		require.NoError(t, err)
		require.Contains(t, string(data), `"namespace":"dynamic_tools"`)
	}
}
