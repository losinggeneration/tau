package session

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func indexEntryIDs(entries []Entry) []string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

// The index must page the branch backward in AllEntries order, pick up
// appends incrementally, and start over when the file is rewritten.
func TestIndexHistoryPagesBranch(t *testing.T) {
	w, err := CreateSessionFile(t.TempDir(), NewHeader("/test", nil, nil))
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	var ids []string
	var parent *string
	appendMsg := func(i int) {
		t.Helper()
		m := NewMessageEntry(parent, json.RawMessage(fmt.Sprintf(`{"role":"user","content":"m%d"}`, i)), nil)
		require.NoError(t, w.Append(m))
		ids = append(ids, m.ID)
		parent = &m.ID
	}
	for i := range 6 {
		appendMsg(i)
	}
	// A compaction keeping m3..m5; its summary shows right before m3. Later
	// messages chain to the compaction entry.
	c := NewCompactionEntry("sum", ids[3], 1, 1, false)
	c.ParentID = parent
	require.NoError(t, w.Append(c))
	parent = &c.ID
	appendMsg(6)
	require.NoError(t, w.Append(NewModelChangeEntry("p", "m")))
	head := ids[6]

	var x Index
	page, next, err := x.History(w.Path(), head, "", 3)
	require.NoError(t, err)
	require.Equal(t, []string{ids[4], ids[5], ids[6]}, indexEntryIDs(page))
	require.Equal(t, ids[4], next)

	page, next, err = x.History(w.Path(), head, next, 3)
	require.NoError(t, err)
	require.Equal(t, []string{ids[2], c.ID, ids[3]}, indexEntryIDs(page), "the summary pages with the entry it anchors to")
	require.Equal(t, ids[2], next)

	page, next, err = x.History(w.Path(), head, next, 3)
	require.NoError(t, err)
	require.Equal(t, []string{ids[0], ids[1]}, indexEntryIDs(page))
	require.Empty(t, next, "the start of the branch ends paging")

	// Appends are indexed incrementally.
	appendMsg(7)
	page, _, err = x.History(w.Path(), ids[7], "", 1)
	require.NoError(t, err)
	require.Equal(t, []string{ids[7]}, indexEntryIDs(page))

	// A rewrite replaces the file; the index starts over and drops deleted
	// entries, which ends the chain there.
	require.NoError(t, w.MarkDeleted([]string{ids[5]}))
	page, next, err = x.History(w.Path(), ids[7], "", 10)
	require.NoError(t, err)
	require.Equal(t, []string{ids[6], ids[7]}, indexEntryIDs(page))
	require.Empty(t, next)
}
