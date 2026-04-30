package tui

import (
	"context"

	tauui "github.com/losinggeneration/tau/tui"
	baseui "github.com/losinggeneration/tau/ui"
)

func (a *App) Confirm(ctx context.Context, title, message string) (bool, error) {
	// Confirm/select/input need a focused dialog path; notifications/status are
	// the common pi command feedback path and are wired below.
	return false, baseui.ErrNoInput
}

func (a *App) Select(ctx context.Context, title string, options []string) (int, error) {
	return -1, baseui.ErrNoInput
}

func (a *App) Input(ctx context.Context, title, placeholder string) (string, error) {
	return "", baseui.ErrNoInput
}

func (a *App) Notify(level baseui.NotifyLevel, message string) {
	if a == nil || message == "" {
		return
	}
	a.post(func(_ *tauui.UpdateCtx) {
		if a.conv == nil {
			return
		}
		if level == baseui.NotifyError {
			a.conv.AddError(message)
		} else {
			a.conv.AddSystem(message)
		}
		a.refreshChat()
	})
}

func (a *App) SetStatus(key, message string) {
	if a == nil {
		return
	}
	a.post(func(_ *tauui.UpdateCtx) {
		a.setExtensionStatus(key, message)
	})
}

func (a *App) SetWidget(key string, lines []string) {
	if a == nil {
		return
	}
	a.post(func(_ *tauui.UpdateCtx) {
		a.setWidget(key, lines, "above")
	})
}

func (a *App) SetTitle(title string) {
	if a == nil || a.app == nil {
		return
	}
	a.post(func(_ *tauui.UpdateCtx) {
		_ = a.app.SetTitle(title)
	})
}

func (a *App) SetEditorText(text string) {
	if a == nil || a.extensions == nil {
		return
	}
	a.post(func(_ *tauui.UpdateCtx) {
		a.extensions.SetEditorText(text)
	})
}

func (a *App) SetToolsExpanded(expanded bool) {
	if a == nil || a.extensions == nil {
		return
	}
	apply := func() {
		for _, id := range a.chatToolIDs() {
			a.extensions.SetToolExpanded(id, expanded)
		}
		a.refreshChat()
	}
	if a.app == nil {
		apply()
		return
	}
	a.post(func(_ *tauui.UpdateCtx) { apply() })
}

func (a *App) chatToolIDs() []string {
	if a == nil || a.conv == nil {
		return nil
	}
	components := a.conv.Snapshot().Components
	ids := make([]string, 0, len(components))
	for _, component := range components {
		if component.Kind != ChatComponentToolExecution || component.ToolExecution.CallID == "" {
			continue
		}
		ids = append(ids, component.ToolExecution.CallID)
	}
	return ids
}
