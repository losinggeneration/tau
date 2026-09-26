package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdkpkg "github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

func TestWorkspaceOverlayListsFiltersAndSortsPinnedFirst(t *testing.T) {
	a := newTestApp(t)
	a.sess = &fakeSession{stateID: "active"}
	o := newWorkspaceOverlay(a, []sdkpkg.SessionInfo{
		{ID: "old", FirstPrompt: "alpha task", Created: "2024-01-01T00:00:00Z", Status: "waiting"},
		{ID: "pin", Name: "beta task", Created: "2023-01-01T00:00:00Z", Pinned: true, Status: "queued"},
	})

	rows := strings.Join(o.Render(120), "\n")
	if strings.Index(rows, "beta task") > strings.Index(rows, "alpha task") {
		t.Fatalf("pinned session not first:\n%s", rows)
	}
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'a'})
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'l'})
	rows = strings.Join(o.Render(120), "\n")
	if !strings.Contains(rows, "alpha task") || strings.Contains(rows, "beta task") {
		t.Fatalf("filtered rows =\n%s", rows)
	}
}

func TestWorkspaceOverlayPinRenameResumeAndNew(t *testing.T) {
	a := newTestApp(t)
	a.dialogPicker = tauui.NewDialogPicker()
	a.footer = newFooterForTest()
	a.sess = &fakeSession{stateID: "current", stateCWD: "/repo"}
	var pinned, renamed string
	a.pinSession = func(ctx context.Context, id string) error { pinned = id; return nil }
	a.renameSession = func(ctx context.Context, id, name string) error { renamed = id + ":" + name; return nil }
	var created, resumed bool
	a.sessionFactory = func(ctx context.Context, opts sdkpkg.SessionOptions) (sdkpkg.Session, error) {
		if opts.SessionID != "" {
			resumed = opts.SessionID == "target"
			return &fakeSession{stateID: opts.SessionID, stateCWD: opts.CWD}, nil
		}
		created = true
		return &fakeSession{stateID: "new", stateCWD: opts.CWD}, nil
	}
	o := newWorkspaceOverlay(a, []sdkpkg.SessionInfo{{ID: "target", Name: "target session", Created: "2025-01-01T00:00:00Z"}})

	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'p'})
	if pinned != "target" {
		t.Fatalf("pinned = %q, want target", pinned)
	}
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'r'})
	o.name = "renamed"
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	if renamed != "target:renamed" {
		t.Fatalf("renamed = %q, want target:renamed", renamed)
	}
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyEnter})
	if !resumed || a.sess.ID() != "target" {
		t.Fatalf("resume failed: resumed=%v active=%s", resumed, a.sess.ID())
	}
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'n'})
	if !created || a.sess.ID() != "new" {
		t.Fatalf("new failed: created=%v active=%s", created, a.sess.ID())
	}
}

func TestWorkspaceOverlayShowsLiveActiveStatus(t *testing.T) {
	a := newTestApp(t)
	a.footer = newFooterForTest()
	a.status = tauui.NewStatusEntriesView(5)
	a.sess = &fakeSession{stateID: "live", queueOverride: 2}

	o := newWorkspaceOverlay(a, []sdkpkg.SessionInfo{
		{ID: "live", FirstPrompt: "live task", Created: "2025-01-01T00:00:00Z", Status: "waiting"},
		{ID: "other", FirstPrompt: "other task", Created: "2024-01-01T00:00:00Z", Status: "complete"},
	})

	// The status column for the in-process session overlays its persisted
	// value with live queue state, mirroring the RPC workspace's
	// active-session status. The status column sits between the short ID
	// and the message count, so match on the rendered columns.
	liveRow := func() string {
		for _, row := range o.Render(160) {
			if strings.HasPrefix(strings.TrimSpace(row), "→") {
				return row
			}
		}
		return ""
	}()
	if !strings.Contains(liveRow, "queued") {
		t.Fatalf("live row should overlay queued status, got %q", liveRow)
	}

	// Streaming outranks the queue, matching the RPC status order.
	a.setStreaming(true)
	for _, row := range o.Render(160) {
		if strings.HasPrefix(strings.TrimSpace(row), "→") {
			if !strings.Contains(row, "running") {
				t.Fatalf("live row should overlay running while streaming, got %q", row)
			}
		}
	}

	// A non-active session keeps its persisted status; the other row's
	// status column still reads complete.
	for _, row := range o.Render(160) {
		if strings.Contains(row, "other task") {
			if strings.Contains(row, "queued") || strings.Contains(row, "running") {
				t.Fatalf("inactive row must not inherit live status, got %q", row)
			}
		}
	}
}

func TestCmdNameUsesRenameHook(t *testing.T) {
	a := newTestApp(t)
	a.sess = &fakeSession{stateID: "current"}
	var gotID, gotName string
	a.renameSession = func(ctx context.Context, id, name string) error {
		gotID, gotName = id, name
		return nil
	}
	cmdName(a, "  good name  ")
	if gotID != "current" || gotName != "good name" {
		t.Fatalf("rename = (%q,%q), want (current,good name)", gotID, gotName)
	}
}

func TestWorkspaceOverlayDeleteWithConfirmation(t *testing.T) {
	a := newTestApp(t)
	a.dialogPicker = tauui.NewDialogPicker()
	a.footer = newFooterForTest()
	a.sess = &fakeSession{stateID: "active"}
	var deleted []string
	a.deleteSession = func(ctx context.Context, id string) error {
		deleted = append(deleted, id)
		return nil
	}
	o := newWorkspaceOverlay(a, []sdkpkg.SessionInfo{
		{ID: "target", FirstPrompt: "doomed", Created: "2025-01-01T00:00:00Z"},
		{ID: "keep", FirstPrompt: "safe", Created: "2024-01-01T00:00:00Z"},
	})

	// d arms the confirmation; nothing is deleted until y.
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'd'})
	if !o.confirmDelete {
		t.Fatal("d did not arm delete confirmation")
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted before confirm: %v", deleted)
	}
	rows := strings.Join(o.Render(120), "\n")
	if !strings.Contains(rows, "Delete") || !strings.Contains(rows, "doomed") {
		t.Fatalf("confirm prompt missing session name:\n%s", rows)
	}

	// Any other key backs out without deleting.
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'n'})
	if o.confirmDelete {
		t.Fatal("confirmation stayed armed after another key")
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted after cancel: %v", deleted)
	}

	// d then y deletes and drops the row from the list.
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'd'})
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'y'})
	if len(deleted) != 1 || deleted[0] != "target" {
		t.Fatalf("deleted = %v, want [target]", deleted)
	}
	rows = strings.Join(o.Render(120), "\n")
	if strings.Contains(rows, "doomed") {
		t.Fatalf("deleted session still listed:\n%s", rows)
	}
	if !strings.Contains(rows, "safe") {
		t.Fatalf("surviving session lost:\n%s", rows)
	}
}

func TestWorkspaceOverlayDeleteRefusesActiveSession(t *testing.T) {
	a := newTestApp(t)
	a.footer = newFooterForTest()
	a.sess = &fakeSession{stateID: "live"}
	var deleted []string
	a.deleteSession = func(ctx context.Context, id string) error {
		deleted = append(deleted, id)
		return nil
	}
	o := newWorkspaceOverlay(a, []sdkpkg.SessionInfo{{ID: "live", FirstPrompt: "current", Created: "2025-01-01T00:00:00Z"}})

	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'd'})
	if o.confirmDelete {
		t.Fatal("active session armed for delete")
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted = %v, want none", deleted)
	}
	msgs := a.conv.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "cannot delete the active session") {
		t.Fatalf("messages = %+v, want active-session refusal", msgs)
	}
}

func TestWorkspaceOverlayDeleteReportsError(t *testing.T) {
	a := newTestApp(t)
	a.dialogPicker = tauui.NewDialogPicker()
	a.footer = newFooterForTest()
	a.sess = &fakeSession{stateID: "active"}
	a.deleteSession = func(ctx context.Context, id string) error {
		return errors.New("disk on fire")
	}
	o := newWorkspaceOverlay(a, []sdkpkg.SessionInfo{{ID: "target", FirstPrompt: "x", Created: "2025-01-01T00:00:00Z"}})

	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'd'})
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'y'})
	msgs := a.conv.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "disk on fire") {
		t.Fatalf("messages = %+v, want delete error", msgs)
	}
	rows := strings.Join(o.Render(120), "\n")
	if !strings.Contains(rows, "x") {
		t.Fatalf("session removed despite failed delete:\n%s", rows)
	}
}

func TestWorkspaceOverlayDeleteUnavailable(t *testing.T) {
	a := newTestApp(t)
	a.footer = newFooterForTest()
	a.sess = &fakeSession{stateID: "active"}
	o := newWorkspaceOverlay(a, []sdkpkg.SessionInfo{{ID: "target", FirstPrompt: "x", Created: "2025-01-01T00:00:00Z"}})

	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'd'})
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'y'})
	msgs := a.conv.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "deleting is unavailable") {
		t.Fatalf("messages = %+v, want unavailability notice", msgs)
	}
	if !strings.Contains(strings.Join(o.Render(120), "\n"), "x") {
		t.Fatal("session removed despite no delete hook")
	}
}

// Once the workspace dialog is focused, the toolkit delivers keys to the
// picker either as raw KeyEvents (Handle) or as actions translated to
// synthetic KeyEvents (HandleAction). Both must reach the overlay — this
// is the delivery path that lost plain runes while the editor kept focus.
func TestWorkspaceOverlayReceivesKeysThroughFocusedPicker(t *testing.T) {
	a := newTestApp(t)
	a.dialogPicker = tauui.NewDialogPicker()
	a.dialogPicker.SetOnCancel(a.dismissDialog)
	a.footer = newFooterForTest()
	a.sess = &fakeSession{stateID: "active"}
	var deleted []string
	a.deleteSession = func(ctx context.Context, id string) error {
		deleted = append(deleted, id)
		return nil
	}
	o := newWorkspaceOverlay(a, []sdkpkg.SessionInfo{
		{ID: "target", FirstPrompt: "doomed", Created: "2025-01-01T00:00:00Z"},
		{ID: "other", FirstPrompt: "survivor", Created: "2024-01-01T00:00:00Z"},
	})
	a.dialogPicker.SetComponent(o)
	a.dialogPicker.SetShown(true)

	// Raw rune delivery — the focused-view Handle path.
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'd'}, nil)
	if !o.confirmDelete {
		t.Fatal("raw key delivery did not arm delete confirmation")
	}

	// 'y' confirms through the same path.
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'y'}, nil)
	if len(deleted) != 1 || deleted[0] != "target" {
		t.Fatalf("deleted = %v, want [target]", deleted)
	}

	// Action delivery — the keymap bridge path (Activate → KeyEnter).
	// Re-arm the confirmation first, then Enter backs out: only y
	// confirms, so the resume key cannot commit a delete by accident.
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'd'}, nil)
	if !o.confirmDelete {
		t.Fatal("raw key delivery did not re-arm delete confirmation")
	}
	a.dialogPicker.HandleAction(int(tauui.ActionActivate), nil)
	if o.confirmDelete {
		t.Fatal("Enter action did not back out of the confirmation")
	}
	if len(deleted) != 1 {
		t.Fatalf("deleted = %v, want only the confirmed delete", deleted)
	}

	// Raw Esc on the focused picker cancels the dialog via OnCancel.
	a.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyEsc}, nil)
	if a.dialogPicker.Shown() {
		t.Fatal("Esc on the focused picker did not dismiss the dialog")
	}
}

// A long list must window around the focused row: rows scrolled off the
// window drop out of the render instead of piling past the dialog's
// visible height where the focused row becomes invisible.
func TestWorkspaceOverlayScrollsWindowWithFocus(t *testing.T) {
	a := newTestApp(t)
	a.sess = &fakeSession{stateID: "active"}
	sessions := make([]sdkpkg.SessionInfo, 0, 30)
	for i := range 30 {
		sessions = append(sessions, sdkpkg.SessionInfo{
			ID: fmt.Sprintf("id-%02d", i), FirstPrompt: fmt.Sprintf("session %02d", i),
			Created: "2025-01-01T00:00:00Z",
		})
	}
	o := newWorkspaceOverlay(a, sessions)

	for range 25 {
		o.HandleKey(tauui.KeyEvent{Key: tauui.KeyDown})
	}
	rows := strings.Join(o.Render(120), "\n")
	if !strings.Contains(rows, "session 25") {
		t.Fatalf("focused row not visible after scrolling down:\n%s", rows)
	}
	if strings.Contains(rows, "session 00") {
		t.Fatalf("first row should have scrolled off the window:\n%s", rows)
	}

	// Scrolling back up re-enters the top of the list.
	for range 25 {
		o.HandleKey(tauui.KeyEvent{Key: tauui.KeyUp})
	}
	rows = strings.Join(o.Render(120), "\n")
	if !strings.Contains(rows, "session 00") {
		t.Fatal("focused row not visible after scrolling back up")
	}
	if strings.Contains(rows, "session 25") {
		t.Fatal("bottom row should have scrolled off after returning to the top")
	}
}

// Filtering and deleting shrink the list under the scroll offset; the
// window must clamp instead of rendering an empty slice around a stale
// offset.
func TestWorkspaceOverlayScrollClampsWhenListShrinks(t *testing.T) {
	a := newTestApp(t)
	a.sess = &fakeSession{stateID: "active"}
	sessions := make([]sdkpkg.SessionInfo, 0, 20)
	for i := range 20 {
		sessions = append(sessions, sdkpkg.SessionInfo{
			ID: fmt.Sprintf("id-%02d", i), FirstPrompt: fmt.Sprintf("session %02d", i),
			Created: "2025-01-01T00:00:00Z",
		})
	}
	o := newWorkspaceOverlay(a, sessions)

	for range 19 {
		o.HandleKey(tauui.KeyEvent{Key: tauui.KeyDown})
	}
	// Delete shrinks the list by one; focus stays on the last row.
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'd'})
	o.HandleKey(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'y'})
	rows := strings.Join(o.Render(120), "\n")
	if !strings.Contains(rows, "session 18") {
		t.Fatalf("focused row lost after delete at list end:\n%s", rows)
	}

	// Filtering down to a handful of rows must still show the focused row.
	o.filter = "1"
	o.clamp()
	rows = strings.Join(o.Render(120), "\n")
	if !strings.Contains(rows, "→") {
		t.Fatalf("no focused row rendered after filter clamp:\n%s", rows)
	}
}
