package tui

import (
	"context"
	"testing"

	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

func TestSessionSelectorRootHasDistinctIDFromPicker(t *testing.T) {
	picker := tauui.NewDialogPicker()
	root := &sessionSelectorRoot{id: tauui.NewID(), picker: picker}

	if root.ID() == 0 {
		t.Fatal("session selector root ID should be non-zero")
	}
	if root.ID() == picker.ID() {
		t.Fatalf("session selector root ID duplicates picker ID %v", picker.ID())
	}

	children := root.Children()
	if len(children) != 1 || children[0] != picker {
		t.Fatalf("session selector root children = %#v, want picker child", children)
	}
}

func TestSessionSelectorRootPaintDoesNotRequestFocusOrPost(t *testing.T) {
	picker := tauui.NewDialogPicker()
	picker.SetShown(true)
	root := &sessionSelectorRoot{id: tauui.NewID(), picker: picker}

	var focusRequests, posts int
	root.app = &tauui.App{}
	ctx := stubCtx()
	ctx.RequestFocus = func(tauui.ID) { focusRequests++ }

	root.Paint(noopDrawer{}, ctx)
	root.Paint(noopDrawer{}, ctx)

	_ = posts
	if focusRequests != 0 {
		t.Fatalf("Paint requested focus %d times; focus must be scheduled outside Paint", focusRequests)
	}
}

func TestSessionInfoDialogPickerItemsFiltersAndSorts(t *testing.T) {
	sessions := []sdk.SessionInfo{
		{ID: "old", Name: "alpha", Created: "2024-01-01T00:00:00Z"},
		{ID: "new", Name: "beta", Created: "2025-01-01T00:00:00Z"},
	}

	items := sessionInfoDialogPickerItems(sessions, "")
	if len(items) != 2 {
		t.Fatalf("items len = %d, want 2", len(items))
	}
	if items[0].Value != "new" || items[1].Value != "old" {
		t.Fatalf("items order = %#v, want newest first", items)
	}

	items = sessionInfoDialogPickerItems(sessions, "alpha")
	if len(items) != 1 || items[0].Value != "old" {
		t.Fatalf("filtered items = %#v, want old", items)
	}
}

func TestSessionSelectorItemsCachesSessionsWhileFiltering(t *testing.T) {
	calls := 0
	a := &App{
		sessionLister: func(context.Context) ([]sdk.SessionInfo, error) {
			calls++
			return []sdk.SessionInfo{
				{ID: "old", Name: "alpha", Created: "2024-01-01T00:00:00Z"},
				{ID: "new", Name: "beta", Created: "2025-01-01T00:00:00Z"},
			}, nil
		},
	}

	if got := a.sessionSelectorItems(""); len(got) != 2 {
		t.Fatalf("initial items len = %d, want 2", len(got))
	}
	if got := a.sessionSelectorItems("alpha"); len(got) != 1 || got[0].Value != "old" {
		t.Fatalf("filtered items = %#v, want old", got)
	}
	if calls != 1 {
		t.Fatalf("session lister calls = %d, want 1 while filtering", calls)
	}

	a.clearSessionSelectorCache()
	if got := a.sessionSelectorItems("beta"); len(got) != 1 || got[0].Value != "new" {
		t.Fatalf("filtered after clear = %#v, want new", got)
	}
	if calls != 2 {
		t.Fatalf("session lister calls after clear = %d, want 2", calls)
	}
}

func TestSessionSelectorKeymapOnlyEnterActivates(t *testing.T) {
	km := sessionSelectorKeymap{}
	if act, ok := km.Resolve(tauui.KeyCtxGlobal, tauui.Keystroke{Key: tauui.KeyEnter}); !ok || act != tauui.ActionActivate {
		t.Fatalf("Enter resolve = (%v, %v), want ActionActivate/true", act, ok)
	}
	if act, ok := km.Resolve(tauui.KeyCtxGlobal, tauui.Keystroke{Key: tauui.KeyRune, Rune: ' '}); !ok || act != tauui.ActionNone {
		t.Fatalf("Space resolve = (%v, %v), want ActionNone/true", act, ok)
	}
	if act, ok := km.Resolve(tauui.KeyCtxGlobal, tauui.Keystroke{Key: tauui.KeyTab}); !ok || act == tauui.ActionActivate {
		t.Fatalf("Tab resolve = (%v, %v), want non-activate action", act, ok)
	}
}

// Ctrl+Z suspends the selector too. Raw mode means it arrives as a keystroke,
// not SIGTSTP, so without this binding it is swallowed — the same suppression
// the main TUI had.
func TestSessionSelectorCtrlZSuspends(t *testing.T) {
	km := sessionSelectorKeymap{}
	for _, r := range []rune{'z', 'Z'} {
		ks := tauui.Keystroke{Key: tauui.KeyRune, Rune: r, Mod: tauui.ModCtrl}
		if act, ok := km.Resolve(tauui.KeyCtxGlobal, ks); !ok || act != AppActionSuspend {
			t.Fatalf("ctrl+%c resolve = (%v, %v), want suspend", r, act, ok)
		}
	}
}

// The root hands suspend to the toolkit and repaints on resume.
func TestSessionSelectorRootSuspendsOnAction(t *testing.T) {
	root := &sessionSelectorRoot{picker: tauui.NewDialogPicker()}

	var suspended bool
	ctx := &tauui.Ctx{Suspend: func() error { suspended = true; return nil }}
	if !root.HandleAction(AppActionSuspend, ctx) {
		t.Fatal("suspend action was not handled")
	}
	if !suspended {
		t.Error("suspend did not call Ctx.Suspend")
	}
}

func TestQuitOnContextCancelStopBeforeCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var quits int
	stop := quitOnContextCancel(ctx, func() { quits++ })
	stop()
	cancel()
	if quits != 0 {
		t.Fatalf("quit called after stop: %d", quits)
	}
}
