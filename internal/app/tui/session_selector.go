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
	return selectTerminalItem(ctx, "/resume", "tau — select session", func(filter string) []tauui.DialogPickerItem {
		return sessionInfoDialogPickerItems(sessions, filter)
	}, nil)
}

func selectTerminalItem(ctx context.Context, header, title string, items func(string) []tauui.DialogPickerItem, start func(*sessionSelectorRoot) func()) (string, error) {
	root, err := runTerminalSelector(ctx, terminalSelector{header: header, title: title, items: items, start: start})
	if err != nil {
		return "", err
	}
	return root.selected, nil
}

// terminalSelector configures a standalone picker. filter and focus restore
// a previous position; keys and onAction add picker-specific shortcuts.
type terminalSelector struct {
	header, title string
	items         func(string) []tauui.DialogPickerItem
	start         func(*sessionSelectorRoot) func()
	filter        string
	focus         func([]tauui.DialogPickerItem) int
	keys          func(tauui.Keystroke) (tauui.Action, bool)
	onAction      func(*sessionSelectorRoot, tauui.Action, *tauui.Ctx) bool
	canSelect     func(tauui.DialogPickerItem) bool
}

func runTerminalSelector(ctx context.Context, s terminalSelector) (*sessionSelectorRoot, error) {
	items := s.items
	picker := tauui.NewDialogPicker()
	picker.SetHeader(s.header)
	picker.SetVisible(20)
	picker.SetFilter(s.filter)
	initial := items(s.filter)
	picker.SetItems(initial)
	if s.focus != nil {
		picker.SetFocused(s.focus(initial))
	}
	picker.SetShown(true)

	root := &sessionSelectorRoot{id: tauui.NewID(), picker: picker, onAction: s.onAction}
	app, err := tauui.New(tauui.Opts{
		Root:           root,
		Title:          s.title,
		Theme:          appTheme(),
		Keymap:         sessionSelectorKeymap{keys: s.keys},
		Mouse:          true,
		BracketedPaste: boolPtr(true),
		ClearOnExit:    true,
	})
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("focus selector: %w", err)
	}
	picker.SetOnSelect(func(_ int, item tauui.DialogPickerItem) {
		if s.canSelect != nil && !s.canSelect(item) {
			return
		}
		root.selected = item.Value
		if root.selected == "" {
			root.selected = item.Name
		}
		app.Quit()
	})
	picker.SetOnChange(func(filter string) {
		picker.SetItems(items(filter))
	})
	picker.SetOnCancel(func() {
		app.Quit()
	})

	if s.start != nil {
		defer s.start(root)()
	}
	defer quitOnContextCancel(ctx, app.Quit)()
	if err := app.Run(); err != nil {
		return nil, err
	}
	return root, nil
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
	selected string
	onAction func(*sessionSelectorRoot, tauui.Action, *tauui.Ctx) bool
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
	if r.onAction != nil && r.onAction(r, act, ctx) {
		return true
	}
	if act == tauui.ActionQuit {
		if r.app != nil {
			r.app.Quit()
		}
		return true
	}
	return r.picker.HandleAction(int(act), ctx)
}

type sessionSelectorKeymap struct {
	keys func(tauui.Keystroke) (tauui.Action, bool)
}

func (k sessionSelectorKeymap) Resolve(ctx tauui.KeyContext, ks tauui.Keystroke) (tauui.Action, bool) {
	if k.keys != nil {
		if act, ok := k.keys(ks); ok {
			return act, true
		}
	}
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
