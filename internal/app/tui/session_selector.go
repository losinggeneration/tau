package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

// SelectSession shows a focused picker for choosing a saved session. It uses
// the shared DialogPicker widget so the picker owns its search line, separator,
// filtered result list, and selected-row marker.
func SelectSession(ctx context.Context, sessions []sdk.SessionInfo) (string, error) {
	picker := tauui.NewDialogPicker()
	picker.SetHeader("/resume")
	picker.SetVisible(20)
	picker.SetItems(sessionInfoDialogPickerItems(sessions, ""))
	picker.SetShown(true)

	root := &sessionSelectorRoot{id: tauui.NewID(), picker: picker, sessions: append([]sdk.SessionInfo(nil), sessions...)}
	app, err := tauui.New(tauui.Opts{
		Root:           root,
		Title:          "tau — select session",
		Theme:          appTheme(),
		Keymap:         sessionSelectorKeymap{},
		Mouse:          true,
		BracketedPaste: boolPtr(true),
		ClearOnExit:    true,
	})
	if err != nil {
		return "", err
	}
	root.app = app
	if err := app.Post(func(ctx *tauui.UpdateCtx) {
		if ctx == nil || ctx.RequestFocus == nil || root.picker == nil {
			return
		}
		ctx.RequestFocus(root.picker.ID())
		if ctx.InvalidateAll != nil {
			ctx.InvalidateAll()
		}
	}); err != nil {
		return "", fmt.Errorf("focus session selector: %w", err)
	}
	picker.SetOnSelect(func(_ int, item tauui.DialogPickerItem) {
		root.selected = item.Value
		if root.selected == "" {
			root.selected = item.Name
		}
		app.Quit()
	})
	picker.SetOnChange(func(filter string) {
		picker.SetItems(sessionInfoDialogPickerItems(sessions, filter))
	})
	picker.SetOnCancel(func() {
		app.Quit()
	})

	defer quitOnContextCancel(ctx, app.Quit)()
	if err := app.Run(); err != nil {
		return "", err
	}
	return root.selected, nil
}

func sessionInfoDialogPickerItems(sessions []sdk.SessionInfo, filter string) []tauui.DialogPickerItem {
	sessions = append([]sdk.SessionInfo(nil), sessions...)
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].Created > sessions[j].Created
	})

	lower := strings.ToLower(strings.TrimSpace(filter))
	items := make([]tauui.DialogPickerItem, 0, len(sessions))
	for _, sess := range sessions {
		if !sessionInfoMatches(sess, lower) {
			continue
		}
		items = append(items, tauui.DialogPickerItem{
			Name:        sessionInfoDisplayPrompt(sess),
			Description: sessionInfoDisplayMeta(sess),
			Value:       sess.ID,
		})
	}
	return items
}

type sessionSelectorRoot struct {
	id       tauui.ID
	picker   *tauui.DialogPicker
	app      *tauui.App
	sessions []sdk.SessionInfo
	selected string
}

func (r *sessionSelectorRoot) ID() tauui.ID { return r.id }

func (r *sessionSelectorRoot) Rect() tauui.Rect {
	if r.picker == nil {
		return tauui.Rect{}
	}
	return r.picker.Rect()
}

func (r *sessionSelectorRoot) MinSize() tauui.Size {
	if r.picker == nil {
		return tauui.Size{}
	}
	return r.picker.MinSize()
}

func (r *sessionSelectorRoot) Layout(rect tauui.Rect) {
	if r.picker != nil {
		r.picker.Layout(rect)
	}
}

func (r *sessionSelectorRoot) Paint(d tauui.Drawer, ctx *tauui.Ctx) {
	if r.picker != nil {
		r.picker.Paint(d, ctx)
	}
}

func (r *sessionSelectorRoot) Handle(e tauui.Event, ctx *tauui.Ctx) bool {
	if r.picker == nil {
		return false
	}
	return r.picker.Handle(e, ctx)
}

func (r *sessionSelectorRoot) Children() []tauui.View {
	if r.picker == nil {
		return nil
	}
	return []tauui.View{r.picker}
}

func (r *sessionSelectorRoot) HandleAction(act tauui.Action, ctx *tauui.Ctx) bool {
	// Ctrl+Z suspends the selector. Raw mode delivers it as a keystroke, so
	// without this case it would be swallowed. rovel's suspend restores the
	// terminal, stops the process, and repaints on resume by itself.
	if act == AppActionSuspend {
		if ctx == nil || ctx.Suspend == nil {
			return false
		}
		return ctx.Suspend() == nil
	}
	if r.picker == nil {
		return false
	}
	if act == tauui.ActionQuit {
		if r.app != nil {
			r.app.Quit()
		}
		return true
	}
	return r.picker.HandleAction(int(act), ctx)
}

type sessionSelectorKeymap struct{}

func (sessionSelectorKeymap) Resolve(ctx tauui.KeyContext, ks tauui.Keystroke) (tauui.Action, bool) {
	switch ks.Key {
	case tauui.KeyEsc, tauui.KeyCtrlC:
		return tauui.ActionQuit, true
	case tauui.KeyRune:
		if ks.Mod == 0 && ks.Rune == ' ' {
			return tauui.ActionNone, true
		}
		// Raw mode turns ISIG off, so Ctrl+Z arrives as a keystroke rather
		// than SIGTSTP; resolve it to suspend so the picker can stop into the
		// background like any other terminal program.
		if ks.Mod == tauui.ModCtrl && (ks.Rune == 'z' || ks.Rune == 'Z') {
			return AppActionSuspend, true
		}
	}
	return tauui.StandardKeymap{}.Resolve(ctx, ks)
}
