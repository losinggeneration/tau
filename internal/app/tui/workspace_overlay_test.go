package tui

import (
	"context"
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
