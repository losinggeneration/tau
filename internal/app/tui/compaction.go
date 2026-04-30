package tui

import (
	"strings"

	sdk "github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

// Compaction indicator and summary (app.md §9: compaction_start/end "show a
// compaction indicator and commit a summary message").
//
// Compaction happens outside a Send, so the events do not arrive on a prompt's
// event channel. They come through sdk.EventObserver, which the session raises
// on whatever goroutine ran Compact — so everything here hops onto the app loop
// before touching UI state.

// compactionStatusKey is the compaction indicator's slot in the status region.
// Distinct from loaderStatusKey so a compaction during an active run does not
// clobber the run spinner (§9 reserves the region for active loaders, plural).
const compactionStatusKey = "compaction"

// compactionRunningMessage is the indicator text while a compaction runs.
const compactionRunningMessage = "Compacting..."

// wireSessionEvents subscribes to the session's out-of-band events. Sessions
// that predate sdk.EventObserver simply do not get an indicator, rather than
// failing to start.
func (a *App) wireSessionEvents() {
	if a == nil || a.sess == nil {
		return
	}
	observer, ok := a.sess.(sdk.EventObserver)
	if !ok {
		return
	}
	observer.SetEventObserver(a.onSessionEvent)
}

// onSessionEvent handles an out-of-band session event. Called on whatever
// goroutine raised it, so it only schedules; applySessionEvent does the work on
// the loop.
func (a *App) onSessionEvent(e sdk.Event) {
	a.post(func(_ *tauui.UpdateCtx) { a.applySessionEvent(e) })
}

// applySessionEvent applies an out-of-band session event. Runs on the app loop.
func (a *App) applySessionEvent(e sdk.Event) {
	switch e.Type {
	case "compaction_start":
		a.compactionRunning = true
		a.setStatus(compactionStatusKey, a.throbberFrame()+" "+compactionRunningMessage)
		a.startThrobber()
	case "compaction_end":
		summary, tokensBefore := compactionPayload(e.Payload)
		a.compactionRunning = false
		a.clearStatus(compactionStatusKey)
		if !a.isStreaming() {
			a.stopThrobber()
		}
		a.conv.AddCompactionSummary(summary, tokensBefore)
		a.refreshChat()
		a.refreshFooterStats(a.loopContext())
	}
}

// throbberFrame returns the current spinner frame, or a static bullet when the
// frame table is empty — the indicator should still appear rather than showing
// a bare message with nothing in front of it.
func (a *App) throbberFrame() string {
	if a == nil {
		return "•"
	}
	if frame := a.throbber.Frame(); frame != "" {
		return frame
	}
	return "•"
}

// compactionPayload pulls the summary and pre-compaction token count out of a
// compaction_end payload. The payload crosses the SDK boundary as a map, and a
// session that omits either field yields the zero value rather than an error:
// a missing token count costs a detail in the header, not the summary itself.
func compactionPayload(payload any) (summary string, tokensBefore int) {
	fields, ok := payload.(map[string]any)
	if !ok {
		return "", 0
	}
	if s, ok := fields["summary"].(string); ok {
		summary = strings.TrimSpace(s)
	}
	switch n := fields["tokens_before"].(type) {
	case int:
		tokensBefore = n
	case int64:
		tokensBefore = int(n)
	// A payload that has been through JSON arrives with float64 numbers.
	case float64:
		tokensBefore = int(n)
	}
	return summary, tokensBefore
}
