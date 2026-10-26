package tui

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/losinggeneration/rovel/backend/headless"
	"github.com/losinggeneration/rovel/geom"
	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/stretchr/testify/require"
)

func TestHistorySearchItemsBackwardStartFromCurrentHistoryPosition(t *testing.T) {
	entries := []string{"one", "two", "three", "four"}
	items := historySearchItems(entries, "", historySearchBackward, 2)
	got := dialogItemValues(items)
	want := []string{"two", "one", "four", "three"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order=%v, want %v", got, want)
	}
}

func TestHistorySearchItemsForwardStartFromCurrentHistoryPosition(t *testing.T) {
	entries := []string{"one", "two", "three", "four"}
	items := historySearchItems(entries, "", historySearchForward, 2)
	got := dialogItemValues(items)
	want := []string{"three", "four", "one", "two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order=%v, want %v", got, want)
	}
}

func TestHistorySearchItemsFilterAndDisplayFirstLine(t *testing.T) {
	entries := []string{"alpha\nsecond line", "beta", "alphabet"}
	items := historySearchItems(entries, "alpha", historySearchBackward, len(entries))
	if len(items) != 2 {
		t.Fatalf("items=%#v, want 2", items)
	}
	if items[0].Name != "alphabet" || items[0].Value != "alphabet" {
		t.Fatalf("first item=%#v", items[0])
	}
	if items[1].Name != "alpha" || items[1].Value != "alpha\nsecond line" {
		t.Fatalf("second item=%#v", items[1])
	}
}

func TestOpenHistorySearchAppliesFocusedItemToEditor(t *testing.T) {
	a := &App{
		conv:         NewConversationModel(),
		editor:       newPromptEditorForTest(),
		dialogPicker: tauui.NewDialogPicker(),
	}
	a.dialogPicker.SetOnCancel(a.dismissDialog)
	a.editor.Model().AddHistory("first")
	a.editor.Model().AddHistory("second")
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	a.loopCtx = ctx

	if !a.openHistorySearch(historySearchBackward, ctx) {
		t.Fatalf("openHistorySearch returned false")
	}
	if !a.dialogPicker.Shown() {
		t.Fatalf("dialog picker not shown")
	}
	a.dialogPicker.HandleAction(int(tauui.ActionActivate), ctx)
	if got := a.editor.Text(); got != "second" {
		t.Fatalf("editor text=%q, want selected history entry", got)
	}
	if a.dialogPicker.Shown() {
		t.Fatalf("dialog picker still shown after selection")
	}
}

func TestOpenHistorySearchSeedsFilterWithCurrentEntry(t *testing.T) {
	a := &App{
		conv:         NewConversationModel(),
		editor:       newPromptEditorForTest(),
		dialogPicker: tauui.NewDialogPicker(),
	}
	a.dialogPicker.SetOnCancel(a.dismissDialog)
	a.editor.Model().AddHistory("find this one")
	a.editor.Model().AddHistory("alpha")
	a.editor.Model().AddHistory("beta")
	a.editor.Model().HistoryPrev()
	if a.editor.Model().Text() != "beta" {
		t.Fatalf("seeded text=%q, want latest history entry", a.editor.Model().Text())
	}

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	a.loopCtx = ctx

	if !a.openHistorySearch(historySearchBackward, ctx) {
		t.Fatalf("openHistorySearch returned false")
	}
	if got := a.dialogPicker.Filter(); got != "beta" {
		t.Fatalf("picker filter=%q, want seeded with current entry text", got)
	}
}

func TestHistorySearchSeedUsesDraftWhenNotInHistory(t *testing.T) {
	if got := historySearchSeed(historySearchBackward, 0, []string{"alpha", "beta"}, "fresh prompt"); got != "fresh prompt" {
		t.Fatalf("seed=%q, want draft text", got)
	}
}

func dialogItemValues(items []tauui.DialogPickerItem) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.Value
	}
	return out
}

// pagerSession serves History from an in-memory transcript, oldest first.
type pagerSession struct {
	fakeSession
	entries []sdk.HistoryEntry
	reads   int
}

func (p *pagerSession) History(before string, limit int) ([]sdk.HistoryEntry, string, error) {
	p.reads++
	end := len(p.entries)
	if before != "" {
		for i, e := range p.entries {
			if e.EntryID == before {
				end = i
			}
		}
	}
	start := max(0, end-limit)
	next := ""
	if start > 0 {
		next = p.entries[start].EntryID
	}
	return p.entries[start:end], next, nil
}

func newPagerSession(n int) *pagerSession {
	p := &pagerSession{}
	for i := range n {
		p.entries = append(p.entries, sdk.HistoryEntry{
			EntryID: fmt.Sprintf("e%d", i),
			Message: sdk.Message{Role: "assistant", Text: fmt.Sprintf("m%d", i)},
		})
	}
	return p
}

// A long session holds only its newest page; older pages come back from the
// session when the viewport reaches the top, and leave memory again once they
// are a window's worth above the rendered window.
func TestHistoryPagesInAndDropsOut(t *testing.T) {
	sess := newPagerSession(3 * chatLogWindowComponents)
	a := newTestApp(t)
	a.sess = sess
	a.chat.setRenderNotifier(func() error {
		a.chat.applyPreparedRender()
		return nil
	})

	a.seedChatFromSession(sess, "")
	held := a.conv.Snapshot()
	require.Len(t, held.Components, chatLogWindowComponents)
	require.Equal(t, fmt.Sprintf("m%d", 2*chatLogWindowComponents), held.Components[0].Text)
	require.Equal(t, fmt.Sprintf("e%d", 2*chatLogWindowComponents), a.historyNext)

	loadPage := func() {
		t.Helper()
		be := headless.New(geom.Size{W: 20, H: 4})
		defer be.Close()
		var err error
		a.app, err = tauui.New(tauui.Opts{Root: tauui.NewText("test"), Backend: be})
		require.NoError(t, err)
		var poll func(*tauui.UpdateCtx)
		poll = func(*tauui.UpdateCtx) {
			if !a.historyLoading {
				a.app.Quit()
				return
			}
			go func() { _ = a.app.Post(poll) }()
		}
		require.NoError(t, a.app.Post(func(u *tauui.UpdateCtx) {
			a.loadOlderHistory()
			poll(u)
		}))
		require.NoError(t, a.app.Run())
	}

	first := held.Components[0].ID
	loadPage()
	held = a.conv.Snapshot()
	require.Len(t, held.Components, 2*chatLogWindowComponents)
	require.Equal(t, fmt.Sprintf("m%d", chatLogWindowComponents), held.Components[0].Text)
	require.Equal(t, first, a.conv.SnapshotFrom(0).Components[0].ID, "held components keep their positions")
	require.Equal(t, fmt.Sprintf("e%d", chatLogWindowComponents), a.historyNext)

	loadPage()
	require.Equal(t, "m0", a.conv.Snapshot().Components[0].Text)
	require.Empty(t, a.historyNext, "the start of the history ends paging")
	reads := sess.reads
	a.loadOlderHistory()
	require.Equal(t, reads, sess.reads, "nothing left to page")

	// Rendering the default tail leaves two pages above the window: they are
	// dropped back to the session, cut at an entry boundary.
	a.chat.Layout(tauui.Rect{W: 40, H: 10})
	waitForChatLog(t, a.chat)
	a.dropOffscreenHistory()
	held = a.conv.Snapshot()
	require.Len(t, held.Components, chatLogWindowComponents)
	require.Equal(t, fmt.Sprintf("m%d", 2*chatLogWindowComponents), held.Components[0].Text)
	require.Equal(t, fmt.Sprintf("e%d", 2*chatLogWindowComponents), a.historyNext)
}

// A page read that a transcript replacement overtook must not land in the new
// transcript.
func TestHistoryPageDiscardedAfterReplacement(t *testing.T) {
	sess := newPagerSession(2 * chatLogWindowComponents)
	a := newTestApp(t)
	a.sess = sess
	a.seedChatFromSession(sess, "")

	be := headless.New(geom.Size{W: 20, H: 4})
	defer be.Close()
	var err error
	a.app, err = tauui.New(tauui.Opts{Root: tauui.NewText("test"), Backend: be})
	require.NoError(t, err)
	require.NoError(t, a.app.Post(func(*tauui.UpdateCtx) {
		a.loadOlderHistory()
		a.replaceConversation(NewConversationModel())
		time.AfterFunc(50*time.Millisecond, func() { _ = a.app.Post(func(*tauui.UpdateCtx) { a.app.Quit() }) })
	}))
	require.NoError(t, a.app.Run())
	require.True(t, a.conv.IsEmpty())
	require.False(t, a.historyLoading)
}
