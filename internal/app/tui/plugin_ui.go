package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/losinggeneration/rovel/event"
	tauui "github.com/losinggeneration/tau/tui"
	baseui "github.com/losinggeneration/tau/ui"
)

type extensionDialogResult struct {
	index int
	text  string
	err   error
}

type extensionDialog struct {
	ctx    context.Context
	once   sync.Once
	result chan extensionDialogResult
}

func (d *extensionDialog) finish(result extensionDialogResult) {
	d.once.Do(func() {
		if err := d.ctx.Err(); err != nil {
			result = extensionDialogResult{index: -1, err: err}
		}
		d.result <- result
	})
}

func (a *App) Confirm(ctx context.Context, title, message string) (bool, error) {
	result := a.requestExtensionDialog(ctx, title+"\n"+message, []string{"Yes", "No"}, "permission", "")
	return result.index == 0, result.err
}

func (a *App) Select(ctx context.Context, title string, options []string) (int, error) {
	result := a.requestExtensionDialog(ctx, title, slices.Clone(options), "question", "")
	return result.index, result.err
}

func (a *App) Input(ctx context.Context, title, placeholder string) (string, error) {
	result := a.requestExtensionDialog(ctx, title, nil, "input", placeholder)
	if result.err == nil && result.index < 0 {
		return "", baseui.ErrNoInput
	}
	return result.text, result.err
}

// stopExtensionDialogs runs after the toolkit loop exits. Closing the admission
// channel also releases requests whose posted opener never ran.
func (a *App) stopExtensionDialogs() {
	a.dialogMu.Lock()
	defer a.dialogMu.Unlock()
	if a.dialogStopped == nil {
		a.dialogStopped = make(chan struct{})
	}
	select {
	case <-a.dialogStopped:
	default:
		close(a.dialogStopped)
	}
	if a.extensionDialog != nil {
		a.extensionDialog.finish(extensionDialogResult{index: -1, err: tauui.ErrClosed})
		a.dismissDialogCtx(nil)
	}
}

func (a *App) requestExtensionDialog(ctx context.Context, title string, options []string, kind, placeholder string) extensionDialogResult {
	cancelled := extensionDialogResult{index: -1}
	if err := ctx.Err(); err != nil {
		cancelled.err = err
		return cancelled
	}
	if a == nil || a.app == nil || a.dialogPicker == nil {
		cancelled.err = baseui.ErrNoInput
		return cancelled
	}
	a.dialogMu.Lock()
	if a.dialogStopped == nil {
		a.dialogStopped = make(chan struct{})
	}
	stopped := a.dialogStopped
	a.dialogMu.Unlock()
	select {
	case <-stopped:
		cancelled.err = tauui.ErrClosed
		return cancelled
	default:
	}
	request := &extensionDialog{ctx: ctx, result: make(chan extensionDialogResult, 1)}
	if err := a.app.Post(func(u *tauui.UpdateCtx) {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-stopped:
			return
		default:
		}
		a.closeDialogComponent()
		a.extensionDialog = request
		a.dismissSelector()
		picker := a.dialogPicker
		picker.Reset()
		titleLines := strings.Split(title, "\n")
		for i, line := range titleLines {
			titleLines[i] = sanitizeStatus(line)
		}
		picker.SetHeader(strings.Join(titleLines, "\n"))
		picker.SetOnCancel(a.dismissDialog)
		finish := func(result extensionDialogResult) {
			if a.extensionDialog != request {
				return
			}
			if err := ctx.Err(); err != nil {
				result = extensionDialogResult{index: -1, err: err}
			}
			request.finish(result)
			a.dismissDialog()
		}
		if kind == "input" {
			input := &tauui.InputPrimitive{Prompt: "> ", OnSubmit: func(value string) { finish(extensionDialogResult{text: value}) }, OnCancel: func() { finish(extensionDialogResult{index: -1}) }}
			picker.SetComponent(&extensionInput{title: title, placeholder: placeholder, input: input})
		} else {
			update := func(filter string) {
				var items []tauui.DialogPickerItem
				for i, option := range options {
					if strings.Contains(strings.ToLower(option), strings.ToLower(filter)) {
						items = append(items, tauui.DialogPickerItem{Name: sanitizeStatus(option), Value: strconv.Itoa(i)})
					}
				}
				picker.SetItems(items)
			}
			update("")
			picker.SetOnChange(update)
			picker.SetOnSelect(func(_ int, item tauui.DialogPickerItem) {
				index, err := strconv.Atoi(item.Value)
				if err != nil {
					finish(extensionDialogResult{index: -1, err: fmt.Errorf("select dialog index: %w", err)})
					return
				}
				finish(extensionDialogResult{index: index})
			})
		}
		a.dialogInputActive.Store(kind == "input")
		picker.SetShown(true)
		statusKind := kind
		if kind == "input" {
			statusKind = "question"
		}
		a.programStatus.blocked = tauui.ProgramStatus{State: "blocked", Kind: statusKind, Message: title}
		a.refreshProgramStatus()
		u.InvalidateLayout()
		u.InvalidateAll()
		// The picker must enter the rebuilt view tree before it can receive focus.
		if err := a.app.Post(func(*tauui.UpdateCtx) {
			if a.extensionDialog == request {
				a.app.SetFocus(picker.ID())
			}
		}); err != nil {
			request.finish(extensionDialogResult{index: -1, err: err})
		}
	}); err != nil {
		cancelled.err = fmt.Errorf("open extension dialog: %w", err)
		return cancelled
	}
	select {
	case result := <-request.result:
		return result
	case <-ctx.Done():
		cancelled.err = ctx.Err()
	case <-stopped:
		cancelled.err = tauui.ErrClosed
	}
	request.finish(cancelled)
	if err := a.app.Post(func(*tauui.UpdateCtx) {
		if a.extensionDialog == request {
			a.dismissDialog()
		}
	}); err != nil && !errors.Is(err, tauui.ErrClosed) {
		cancelled.err = errors.Join(cancelled.err, fmt.Errorf("close extension dialog: %w", err))
		return cancelled
	}
	return <-request.result
}

type extensionInput struct {
	title, placeholder string
	input              *tauui.InputPrimitive
}

func (d *extensionInput) Render(width int) []string {
	rows := strings.Split(d.title, "\n")
	for i, row := range rows {
		rows[i] = sanitizeStatus(row)
	}
	if d.placeholder != "" {
		rows = append(rows, sanitizeStatus(d.placeholder))
	}
	rows = append(rows, d.input.Render(width)...)
	return append(rows, "Enter to submit, Esc to cancel")
}

func (d *extensionInput) HandleKey(key event.KeyEvent) { d.input.HandleKey(key) }
func (d *extensionInput) Paste(value string)           { d.input.Paste(value) }

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
