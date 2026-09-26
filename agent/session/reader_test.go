package session

import (
	"encoding/json"
	"slices"
	"testing"
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
