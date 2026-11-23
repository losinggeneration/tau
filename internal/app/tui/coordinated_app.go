package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	rpcclient "github.com/losinggeneration/tau/rpc/client"
	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
	baseui "github.com/losinggeneration/tau/ui"
)

func (a *App) registerInstanceCommands() {
	if a.instanceAction == nil {
		return
	}
	for _, entry := range []struct{ name, action, help string }{
		{"instances", "instances", "List and switch running instances"},
		{"instance-new", "new", "Start an independent worker in this or another directory"},
		{"instance-stop", "stop", "Stop this worker for all clients"},
	} {
		a.cmds.Register(Command{Name: entry.name, Description: entry.help, Handler: func(a *App, arg string) {
			if entry.action == "stop" && a.coordinated == nil {
				a.setStatus("coordinator", "No worker attached.")
				return
			}
			a.leaveInstance(entry.action, arg)
		}})
	}
}

func (a *App) leaveInstance(action, arg string) {
	if a.waitingForCoordinatedSend() {
		return
	}
	draft := a.editor.Model().ExpandedText()
	if action == "resume" {
		draft = ""
	}
	a.instanceAction(action, arg, draft)
	a.quit()
}

// Don't save or clear the draft as we leave while it may already have been
// accepted by the worker. The send's completion reconciles the draft first.
func (a *App) waitingForCoordinatedSend() bool {
	if !a.coordinatedPendingSend {
		return false
	}
	a.setStatus("coordinator-send", "Waiting for the worker to acknowledge the message…")
	return true
}

func (a *App) sendCoordinatedDraft(text string, followUp bool) {
	if text == "" || a.coordinatedPendingSend {
		return
	}
	a.coordinatedPendingSend = true
	original := a.editor.Text()
	sess := a.coordinated
	cwd := a.fileSelectorRoot()
	sessionID := a.coordinatedSessionID
	if sessionID == "" {
		sessionID = sess.ID()
	}
	behavior := "steer"
	if followUp {
		behavior = "follow_up"
	}
	go func() {
		expanded, images, err := expandTextFileReferences(text, cwd)
		if err == nil {
			msg := sdk.Message{Role: "user", Text: expanded, Images: images}
			err = sess.sendDraft(a.ctx, sessionID, msg, behavior)
		}
		a.post(func(*tauui.UpdateCtx) {
			a.coordinatedPendingSend = false
			a.clearStatus("coordinator-send")
			if err != nil {
				a.conv.AddError("Send: " + err.Error())
				if errors.Is(err, rpcclient.ErrOutcomeUnknown) {
					a.conv.AddSystem("Draft kept. Check the shared transcript and queue before sending it again.")
				}
				a.refreshChat()
				return
			}
			a.clearStatus("startup")
			ctx := a.loopContext()
			a.editor.Model().AddHistory(text)
			if a.editor.Text() == original {
				a.editor.Reset(ctx)
				a.clearPendingAttachments(ctx)
				a.dismissSelector()
			}
		})
	}()
}

// restoreCoordinatedQueue takes the queue back from the worker without blocking
// the app loop. Holding the pending-send guard keeps this client from leaving
// the instance before the removed text reaches the editor.
func (a *App) restoreCoordinatedQueue() {
	if a.waitingForCoordinatedSend() {
		return
	}
	a.coordinatedPendingSend = true
	sess := a.coordinated
	sessionID := a.coordinatedSessionID
	if sessionID == "" {
		sessionID = sess.ID()
	}
	go func() {
		queued, kept, err := sess.takeQueuedText(a.ctx, sessionID)
		a.post(func(*tauui.UpdateCtx) { a.finishCoordinatedRestore(queued, kept, err) })
	}()
}

// finishCoordinatedRestore leaves the pending view to the worker's queue_update,
// which stays correct if another client queued a message in the meantime.
func (a *App) finishCoordinatedRestore(queued []string, kept int, err error) {
	a.coordinatedPendingSend = false
	a.clearStatus("coordinator-send")
	if err != nil {
		a.conv.AddError("Restore queued messages: " + err.Error())
		if errors.Is(err, rpcclient.ErrOutcomeUnknown) {
			a.conv.AddSystem("The queue may have been cleared. Check the shared transcript and queue before resending.")
		}
		a.refreshChat()
		return
	}
	a.restoreQueuedText(a.loopContext(), queued, kept)
}

func (a *App) forwardCoordinatedEvents(ctx context.Context, events <-chan sdk.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			applied := make(chan struct{})
			if err := a.app.Post(func(*tauui.UpdateCtx) {
				a.applyCoordinatedEvent(ev)
				close(applied)
			}); err != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-applied:
			}
		}
	}
}

func (a *App) applyCoordinatedEvent(ev sdk.Event) {
	if ev.Type != CoordinatedResetEvent && ev.SessionID != "" && a.coordinatedSessionID != "" && ev.SessionID != a.coordinatedSessionID {
		return
	}
	if _, ok := payloadValue(ev.Payload, "stats").(sdk.SessionStats); ok {
		a.refreshCoordinatedModel()
	}
	switch ev.Type {
	case CoordinatedResetEvent:
		reset, ok := ev.Payload.(CoordinatedReset)
		if !ok {
			return
		}
		changedSession := a.coordinatedSessionID != "" && a.coordinatedSessionID != reset.SessionID
		a.coordinatedSessionID = reset.SessionID
		if changedSession {
			a.clearSessionSelectorCache()
			a.pendingModelKey = ""
			a.clearPendingAttachments(a.loopContext())
		}
		a.coordinatedBash = ""
		a.replaceConversation(NewConversationModel())
		appendHistoryToConversation(a.conv, reset.History)
		a.historyNext = reset.HistoryBefore
		a.coordinatedEntries = make(map[string]struct{}, len(reset.History))
		for _, e := range reset.History {
			a.coordinatedEntries[e.EntryID] = struct{}{}
		}
		for _, row := range reset.Live.Messages {
			if row.Role == "assistant" {
				a.ensureAssistantMessage(row.ID)
				a.conv.SetAssistantText(row.Text)
				if row.Text == "" && row.Thinking != "" {
					a.conv.AddThinking(a.throbberFrame())
				}
			}
		}
		for _, tool := range reset.Live.Tools {
			a.conv.AddToolCallWithID(tool.ID, tool.Name, tool.Input)
			a.conv.AddToolProgress(tool.ID, tool.Progress)
		}
		for _, bash := range reset.Live.Bash {
			a.applyCoordinatedBash(bash.ID, bash.Text)
		}
		a.setToolRunning(len(reset.Live.Tools) > 0)
		if reset.Live.Truncated {
			a.conv.AddSystem("Some earlier streaming output was too large to restore.")
		}
		streaming := reset.Snapshot != nil && reset.Snapshot.State.IsStreaming
		a.setStreaming(streaming)
		if streaming && a.throbberTimer == nil {
			a.startThrobber()
		}
		a.programStatus.compacting = reset.Snapshot != nil && reset.Snapshot.State.IsCompacting
		a.refreshCompactionIndicator()
		a.refreshProgramStatus()
		if a.app != nil {
			_ = a.app.SetTitle(appTerminalTitle(reset.SessionID, streaming))
		}
		a.refreshCoordinatedModel()
		a.syncPendingFromSession(a.loopContext())
		a.refreshChat()
		a.refreshCoordinatedDialogs()
	case CoordinatedStatusEvent:
		state, _ := plString(ev.Payload, "state")
		switch state {
		case CoordinatedConnected:
			a.clearStatus("coordinator")
		case CoordinatedExited:
			a.programStatus.compacting = false
			a.setStreaming(false)
			a.refreshCompactionIndicator()
			a.refreshProgramStatus()
			a.setToolRunning(false)
			a.coordinatedBash = ""
			a.syncPendingFromSession(a.loopContext())
			a.refreshCoordinatedDialogs()
			a.setStatus("coordinator", "Worker exited. Press F8 or use /instances to switch.")
		default:
			a.setStatus("coordinator", "Connection to worker lost; reconnecting…")
		}
	case "queue_update":
		a.syncPendingFromSession(a.loopContext())
	case "ui_confirm", "ui_select", "ui_input", "ui_resolved":
		a.refreshCoordinatedDialogs()
	case "session_start", "model_change", "thinking_level_change":
		a.refreshCoordinatedModel()
	case "session_end", "session_shutdown":
		a.setStreaming(false)
	case "bash_execution_update":
		id, _ := plString(ev.Payload, "id")
		delta, _ := plString(ev.Payload, "delta")
		a.applyCoordinatedBash(id, delta)
	case "bash_execution_end":
		id, _ := plString(ev.Payload, "id")
		if a.coordinatedBash == id && id != "" {
			a.coordinatedBash = ""
			if idx := a.conv.findTool(id); idx >= 0 {
				a.conv.AddToolResultWithID(id, a.conv.committed[idx].toolOutput, false)
				a.refreshChat()
			}
		}
	default:
		if entry, _ := plString(ev.Payload, "entry_id"); entry != "" {
			if _, shown := a.coordinatedEntries[entry]; shown {
				return
			}
		}
		a.applySessionEvent(ev)
	}
}

func (a *App) refreshCoordinatedModel() {
	if a.footer == nil {
		return
	}
	state := a.coordinated.State()
	a.currentModel = state.Model()
	if cwd := state.CWD(); cwd != "" && a.footer.data.cwd != cwd {
		a.footer.data.cwd = cwd
		a.footer.data.branch = detectGitBranch(cwd)
	}
	a.refreshFooterStats(a.loopContext())
}

func (a *App) abortCoordinatedBash() {
	go func() {
		if err := a.coordinated.abortBash(); err != nil {
			a.post(func(*tauui.UpdateCtx) { a.setStatus("coordinator-action", "Abort bash: "+err.Error()) })
		}
	}()
}

func (a *App) applyCoordinatedBash(id, chunk string) {
	if id == "" || chunk == "" {
		return
	}
	if a.conv.findTool(id) < 0 {
		a.conv.AddToolCallWithID(id, "bash", "")
	}
	a.coordinatedBash = id
	a.conv.AddToolProgress(id, chunk)
	a.refreshChat()
}

func (a *App) refreshCoordinatedDialogs() {
	requests := a.coordinated.PendingUIRequests()
	if a.coordinatedDialog != "" {
		for _, r := range requests {
			if r.ID == a.coordinatedDialog && a.dialogPicker.Shown() {
				return
			}
		}
		a.coordinatedDialog = ""
		a.coordinatedReply = ""
		a.clearStatus("approval")
		a.dismissDialog()
	}
	if len(requests) == 0 || a.dialogPicker.Shown() {
		return
	}
	req := requests[0]
	a.coordinatedDialog = req.ID
	a.dismissSelector()
	p := a.dialogPicker
	p.Reset()
	title := sanitizeStatus(req.Title)
	if req.Message != "" {
		title += "\n" + sanitizeStatus(req.Message)
	}
	p.SetHeader(title)
	reply := func(response baseui.UIResponse) {
		if a.coordinatedDialog != req.ID || a.coordinatedReply == req.ID {
			return
		}
		a.coordinatedReply = req.ID
		go func() {
			_, err := a.coordinated.RespondUI(a.ctx, response)
			if err != nil {
				a.post(func(*tauui.UpdateCtx) {
					if a.coordinatedDialog == req.ID {
						a.coordinatedReply = ""
						a.setStatus("approval", fmt.Sprintf("Approval: %v", err))
					}
				})
			}
		}()
	}
	cancel := func() { reply(baseui.NewUIResponseCancelled(req.ID, req.Method)) }
	p.SetOnCancel(cancel)
	switch req.Method {
	case baseui.UIMethodInput:
		input := &tauui.InputPrimitive{Prompt: "> ", OnSubmit: func(s string) { reply(baseui.NewUIResponseValue(req.ID, req.Method, s)) }, OnCancel: cancel}
		p.SetComponent(&extensionInput{title: title, placeholder: req.Placeholder, input: input})
		a.dialogInputActive.Store(true)
	case baseui.UIMethodConfirm:
		p.SetItems([]tauui.DialogPickerItem{{Name: "No", Value: "no"}, {Name: "Yes", Value: "yes"}})
		p.SetOnSelect(func(_ int, item tauui.DialogPickerItem) {
			reply(baseui.NewUIResponseConfirmed(req.ID, req.Method, item.Value == "yes"))
		})
	case baseui.UIMethodSelect:
		update := func(filter string) {
			items := make([]tauui.DialogPickerItem, 0, len(req.Options))
			for _, option := range req.Options {
				if strings.Contains(strings.ToLower(option), strings.ToLower(filter)) {
					items = append(items, tauui.DialogPickerItem{Name: sanitizeStatus(option), Value: option})
				}
			}
			p.SetItems(items)
		}
		update("")
		p.SetOnChange(update)
		p.SetOnSelect(func(_ int, item tauui.DialogPickerItem) {
			reply(baseui.NewUIResponseValue(req.ID, req.Method, item.Value))
		})
	}
	p.SetShown(true)
	if ctx := a.loopContext(); ctx != nil {
		if ctx.InvalidateLayout != nil {
			ctx.InvalidateLayout()
		}
		if ctx.InvalidateAll != nil {
			ctx.InvalidateAll()
		}
	}
	a.post(func(*tauui.UpdateCtx) {
		if a.coordinatedDialog == req.ID && p.Shown() {
			a.app.SetFocus(p.ID())
		}
	})
}
