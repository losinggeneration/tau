package tui

import (
	"context"
	"fmt"
	"testing"

	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

func forkPickerEntries() []sdk.ForkEntry {
	return []sdk.ForkEntry{
		{EntryID: "e1", Role: "user", Text: "first prompt"},
		{EntryID: "e2", Role: "assistant", Text: "agent reply\nsecond line"},
		{EntryID: "e3", Role: "user", Text: "second prompt"},
	}
}

func TestForkSelectorStartsAtLatestPrompt(t *testing.T) {
	a := newAppWithSelector(t)
	a.sess = &forkEntrySession{fakeSession: &fakeSession{stateID: "old"}, entries: forkPickerEntries()}
	a.sessionFactory = func(_ context.Context, _ sdk.SessionOptions) (sdk.Session, error) {
		return &fakeSession{stateID: "new"}, nil
	}

	a.openForkSelector(stubCtx())

	if a.selectorMode != selectorModeFork || !a.selector.Shown() {
		t.Fatalf("fork selector shown=%v mode=%d", a.selector.Shown(), a.selectorMode)
	}
	items := a.selector.Items()
	if len(items) != 2 || items[0].Value != "e1" || items[1].Value != "e3" {
		t.Fatalf("prompt items = %#v", items)
	}
	if got := a.selector.Focused(); got != 1 {
		t.Fatalf("focused = %d, want latest prompt at 1", got)
	}
}

func TestForkSelectorLongHistoryStartsAtBottom(t *testing.T) {
	entries := make([]sdk.ForkEntry, 100)
	for i := range entries {
		entries[i] = sdk.ForkEntry{EntryID: fmt.Sprintf("e%d", i), Role: "user", Text: fmt.Sprintf("prompt %d", i)}
	}
	a := newAppWithSelector(t)
	a.sess = &forkEntrySession{fakeSession: &fakeSession{stateID: "old"}, entries: entries}
	a.sessionFactory = func(_ context.Context, _ sdk.SessionOptions) (sdk.Session, error) {
		return &fakeSession{stateID: "new"}, nil
	}

	a.openForkSelector(stubCtx())
	if got := a.selector.Focused(); got != 99 {
		t.Fatalf("focused = %d, want 99", got)
	}
}

func TestForkSelectorTabTogglesAllMessagesAndInvalidates(t *testing.T) {
	a := newAppWithSelector(t)
	a.sess = &forkEntrySession{fakeSession: &fakeSession{stateID: "old"}, entries: forkPickerEntries()}
	a.sessionFactory = func(_ context.Context, _ sdk.SessionOptions) (sdk.Session, error) {
		return &fakeSession{stateID: "new"}, nil
	}
	var invalidations int
	var invalidateAll int
	ctx := stubCtx()
	ctx.Invalidate = func(tauui.Rect) { invalidations++ }
	ctx.InvalidateAll = func() { invalidateAll++ }
	a.openForkSelector(ctx)
	invalidations = 0
	invalidateAll = 0

	if !a.applySelectorFromAction(ctx) {
		t.Fatal("Tab was not handled")
	}
	items := a.selector.Items()
	if len(items) != 3 || items[1].Value != "e2" {
		t.Fatalf("all-message items = %#v", items)
	}
	if invalidations+invalidateAll == 0 {
		t.Fatal("Tab did not invalidate the selector")
	}
	if invalidateAll == 0 {
		t.Fatal("Tab did not invalidate the full changed selector viewport")
	}
	item, ok := a.selector.FocusedItem()
	if !ok || item.Value != "e3" {
		t.Fatalf("selection after toggle = %#v, %v; want e3", item, ok)
	}
}

func TestForkSelectorUsesSharedViewportAndStaysAboveFooter(t *testing.T) {
	entries := make([]sdk.ForkEntry, 100)
	for i := range entries {
		entries[i] = sdk.ForkEntry{EntryID: fmt.Sprintf("e%d", i), Role: "user", Text: fmt.Sprintf("prompt %d", i)}
	}
	a, err := New(Options{
		Session: &forkEntrySession{fakeSession: &fakeSession{stateID: "old"}, entries: entries},
		SessionFactory: func(_ context.Context, _ sdk.SessionOptions) (sdk.Session, error) {
			return &fakeSession{stateID: "new"}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.chat.Close() })

	ctx := stubCtx()
	a.editor.SetText(ctx, "/fork ")
	a.root.Layout(tauui.Rect{W: 80, H: 24})

	if got := a.selector.Visible(); got != tauui.DefaultSelectorVisible {
		t.Fatalf("visible rows = %d, want shared selector default %d", got, tauui.DefaultSelectorVisible)
	}
	selectorBottom := a.selector.Rect().Y + a.selector.Rect().H
	if selectorBottom > a.footer.Rect().Y {
		t.Fatalf("selector bottom %d overlaps footer at %d", selectorBottom, a.footer.Rect().Y)
	}
}

func TestForkSelectorEnterForksAtSelection(t *testing.T) {
	a := newAppWithSelector(t)
	a.sess = &forkEntrySession{fakeSession: &fakeSession{stateID: "old"}, entries: forkPickerEntries()}
	var got sdk.SessionOptions
	a.sessionFactory = func(_ context.Context, opts sdk.SessionOptions) (sdk.Session, error) {
		got = opts
		return &fakeSession{stateID: "new"}, nil
	}
	a.loopCtx = stubCtx()
	a.openForkSelector(stubCtx())

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	if !a.activateAndSend(ctx) {
		t.Fatal("Enter was not handled")
	}
	if got.ForkFrom != "old" || got.ForkFromEntry != "e3" {
		t.Fatalf("fork options = %+v", got)
	}
}

func TestForkCommandEmptyArgsForksLatest(t *testing.T) {
	a := newTestApp(t)
	a.sess = &forkEntrySession{fakeSession: &fakeSession{stateID: "old"}, entries: forkPickerEntries()}
	var got sdk.SessionOptions
	a.sessionFactory = func(_ context.Context, opts sdk.SessionOptions) (sdk.Session, error) {
		got = opts
		return &fakeSession{stateID: "new"}, nil
	}

	cmdForkOrStub(a, "")
	if got.ForkFrom != "old" || got.ForkFromEntry != "" {
		t.Fatalf("options = %+v", got)
	}
}

type forkEntrySession struct {
	*fakeSession
	entries []sdk.ForkEntry
}

func (f *forkEntrySession) GetForkEntries() ([]sdk.ForkEntry, error) {
	return f.entries, nil
}

type forgetSession struct {
	*fakeSession
	entries []sdk.ForkEntry
	forgot  string
	kept    []sdk.Message
}

func (f *forgetSession) GetForgetEntries() ([]sdk.ForkEntry, error) { return f.entries, nil }

func (f *forgetSession) Forget(entryID string) error {
	f.forgot = entryID
	f.fakeSession.messages = f.kept
	return nil
}

func TestForgetCommandRequiresSelection(t *testing.T) {
	a := newAppWithSelector(t)
	entries := append([]sdk.ForkEntry{{EntryID: "c0", Role: "compaction", Text: "summary text"}}, forkPickerEntries()...)
	sess := &forgetSession{
		fakeSession: &fakeSession{stateID: "s", messages: []sdk.Message{{Role: "user", Text: "stale"}}},
		entries:     entries,
		kept:        []sdk.Message{{Role: "user", Text: "first prompt"}, {Role: "assistant", Text: "agent reply"}},
	}
	a.sess = sess
	a.loopCtx = stubCtx()
	a.conv.AddUserMessage("stale")

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	a.editor.SetText(ctx, "/forget")
	if !a.trySubmit(ctx) {
		t.Fatal("/forget was not handled")
	}
	if sess.forgot != "" {
		t.Fatalf("/forget without a selection forgot %q", sess.forgot)
	}
	if a.selectorMode != selectorModeForget || !a.selector.Shown() {
		t.Fatalf("forget selector shown=%v mode=%d", a.selector.Shown(), a.selectorMode)
	}
	items := a.selector.Items()
	if len(items) != 3 || items[0].Description != "summary" || items[2].Value != "e3" {
		t.Fatalf("items = %#v", items)
	}
	if got := a.selector.Focused(); got != 2 {
		t.Fatalf("focused = %d, want bottom entry 2", got)
	}

	a.handleSelectorAction(int(tauui.ActionMoveUp))
	if !a.activateAndSend(ctx) {
		t.Fatal("Enter was not handled")
	}
	if sess.forgot != "e1" {
		t.Fatalf("forgot %q, want e1", sess.forgot)
	}
	if a.selector.Shown() || a.editor.Text() != "" {
		t.Fatalf("selector shown=%v editor=%q after forget", a.selector.Shown(), a.editor.Text())
	}
	var texts []string
	for _, m := range a.conv.Messages() {
		texts = append(texts, m.Text)
	}
	if want := []string{"first prompt", "agent reply"}; fmt.Sprint(texts) != fmt.Sprint(want) {
		t.Fatalf("chat = %q, want %q", texts, want)
	}
}

func TestForgetSelectorWithoutEntriesReportsError(t *testing.T) {
	a := newAppWithSelector(t)
	a.sess = &forgetSession{fakeSession: &fakeSession{stateID: "s"}}

	a.openForgetSelector(stubCtx())

	if a.selector.Shown() || a.selectorMode != selectorModeNone {
		t.Fatalf("selector shown=%v mode=%d", a.selector.Shown(), a.selectorMode)
	}
	msgs := a.conv.Messages()
	if len(msgs) == 0 || msgs[len(msgs)-1].Text != "forget: no messages to forget" {
		t.Fatalf("messages = %#v", msgs)
	}
}
