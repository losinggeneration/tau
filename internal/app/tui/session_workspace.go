package tui

import (
	"fmt"
	"strings"
)

func cmdName(app *App, args string) {
	if app == nil || app.conv == nil || app.sess == nil {
		return
	}
	name := strings.TrimSpace(args)
	if name == "" {
		app.conv.AddError("name: usage: /name <name>")
		app.refreshChat()
		return
	}
	if app.renameSession == nil {
		app.conv.AddError("name: session rename is unavailable")
		app.refreshChat()
		return
	}
	id := app.sess.ID()
	if id == "" {
		app.conv.AddError("name: no active session")
		app.refreshChat()
		return
	}
	if err := app.renameSession(app.ctxOrBackground(), id, name); err != nil {
		app.conv.AddError("name: " + err.Error())
		app.refreshChat()
		return
	}
	if app.footer != nil && app.footer.data != nil {
		app.footer.data.sessionName = name
	}
	app.conv.AddSystem("name: " + name)
	app.refreshChat()
}

func cmdSessionWorkspace(app *App, _ string) {
	if app == nil || app.dialogPicker == nil {
		return
	}
	if app.sessionLister == nil {
		app.conv.AddError("session: session listing is unavailable")
		app.refreshChat()
		return
	}
	sessions, err := app.sessionLister(app.ctxOrBackground())
	if err != nil {
		app.conv.AddError("session: " + err.Error())
		app.refreshChat()
		return
	}
	app.closeDialogComponent()
	overlay := newWorkspaceOverlay(app, sessions)
	app.dialogPicker.SetComponent(overlay)
	app.dialogPicker.SetHeader("/session")
	app.dialogPicker.SetOnCancel(app.dismissDialog)
	app.dialogPicker.Reset()
	app.dialogPicker.SetShown(true)
	// The overlay owns text input (filter, rename, delete confirm), so
	// it needs focus: with the editor still focused, the keymap bridge
	// only forwards Tab/Enter/arrows/Space and every other rune lands
	// in the prompt. Mirrors openHistorySearch.
	if app.app != nil {
		app.app.SetFocus(app.dialogPicker.ID())
	}
	if ctx := app.loopContext(); ctx != nil {
		if ctx.InvalidateLayout != nil {
			ctx.InvalidateLayout()
		}
		if ctx.InvalidateAll != nil {
			ctx.InvalidateAll()
		}
	}
}

func (a *App) RenameSession(id, name string) error {
	if a == nil || a.renameSession == nil {
		return fmt.Errorf("session rename is unavailable")
	}
	return a.renameSession(a.ctxOrBackground(), id, name)
}

func (a *App) PinSession(id string) error {
	if a == nil || a.pinSession == nil {
		return fmt.Errorf("session pinning is unavailable")
	}
	return a.pinSession(a.ctxOrBackground(), id)
}

func (a *App) UnpinSession(id string) error {
	if a == nil || a.unpinSession == nil {
		return fmt.Errorf("session pinning is unavailable")
	}
	return a.unpinSession(a.ctxOrBackground(), id)
}
