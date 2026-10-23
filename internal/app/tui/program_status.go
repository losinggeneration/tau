package tui

import (
	"strings"

	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

type programStatusReporter struct {
	active     bool
	compacting bool
	result     tauui.ProgramStatus
	resting    tauui.ProgramStatus
	blocked    tauui.ProgramStatus
	last       tauui.ProgramStatus
}

func (r *programStatusReporter) handleEvent(ev sdk.Event) {
	switch ev.Type {
	case "agent_start":
		r.active = true
		r.result = tauui.ProgramStatus{State: "done"}
	case "message_end":
		role, _ := plString(ev.Payload, "role")
		if role != "assistant" {
			return
		}
		reason, _ := plStringAny(ev.Payload, "finish_reason", "stop_reason", "stopReason")
		switch reason {
		case "error":
			message, _ := plStringAny(ev.Payload, "error_message", "errorMessage")
			r.result = tauui.ProgramStatus{State: "error", Message: statusErrorLine(message)}
		case "aborted":
			r.result = tauui.ProgramStatus{State: "idle"}
		default:
			r.result = tauui.ProgramStatus{State: "done"}
		}
	case "error":
		if r.result.State != "idle" {
			message, _ := plString(ev.Payload, "message")
			r.result = tauui.ProgramStatus{State: "error", Message: statusErrorLine(message)}
		}
	case "session_end":
		r.active = false
		r.resting = r.result
		if aborted, _ := plBool(ev.Payload, "aborted"); aborted {
			r.resting = tauui.ProgramStatus{State: "idle"}
		}
	case "compaction_start":
		r.compacting = true
	case "compaction_end":
		r.compacting = false
		aborted, _ := plBool(ev.Payload, "aborted")
		message, _ := plString(ev.Payload, "error_message")
		reason, _ := plString(ev.Payload, "reason")
		var outcome tauui.ProgramStatus
		switch {
		case aborted:
			outcome.State = "idle"
		case message != "":
			outcome = tauui.ProgramStatus{State: "error", Message: statusErrorLine(message)}
		case reason == "manual" || reason == "":
			outcome.State = "done"
		}
		if r.active {
			if aborted || message != "" {
				r.result = outcome
			}
		} else if outcome.State != "" {
			r.resting = outcome
		}
	}
}

func statusErrorLine(message string) string {
	line, _, _ := strings.Cut(message, "\n")
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return "Error"
}

func (r *programStatusReporter) current() tauui.ProgramStatus {
	switch {
	case r.blocked.State != "":
		return r.blocked
	case r.compacting:
		return tauui.ProgramStatus{State: "working", Message: "Compacting context"}
	case r.active:
		return tauui.ProgramStatus{State: "working"}
	case r.resting.State != "":
		return r.resting
	default:
		return tauui.ProgramStatus{State: "idle"}
	}
}

func (a *App) refreshProgramStatus() {
	if a.app == nil {
		return
	}
	status := a.programStatus.current()
	status.App = "tau"
	if (status.State == "working" || status.State == "done") && status.Message == "" && a.footer != nil && a.footer.data != nil {
		status.Message = a.footer.data.sessionName
	}
	if a.programStatus.last == status {
		return
	}
	if err := a.app.SetProgramStatus(status); err != nil {
		a.setStatus("program-status-error", err.Error())
		return
	}
	a.programStatus.last = status
}

func (a *App) resetProgramStatus() {
	blocked := a.programStatus.blocked
	a.programStatus = programStatusReporter{blocked: blocked}
	a.refreshProgramStatus()
}
