package tui

import (
	"strings"

	tauui "github.com/losinggeneration/tau/tui"
)

// LayoutMode selects how the eight application regions are arranged.
//
// Both models are supported rather than one replacing the other, because they
// trade off differently and the choice is a matter of taste as much as
// conformance:
//
//   - LayoutFixed pins the header to the top and the editor/footer to the
//     bottom, giving the remaining space to a chat region that scrolls in its
//     own viewport. Chrome is always where you left it, and scrolling the
//     transcript never moves the prompt.
//   - LayoutDocument concatenates every region into one tall document and
//     shows its bottom, the way terminal scrollback works. The header scrolls
//     away instead of costing rows forever, a short session sits at the top of
//     the screen rather than being pushed to the bottom, and there is a single
//     scroll position for everything.
//
// app.md §9 specifies the document model. The fixed shell is retained as a
// supported alternative, not as an unfixed deviation.
type LayoutMode string

const (
	// LayoutFixed is the fixed shell.
	LayoutFixed LayoutMode = "fixed"
	// LayoutDocument is the bottom-anchored vertical document.
	LayoutDocument LayoutMode = "document"
)

// DefaultLayoutMode is what an unset setting resolves to. The fixed shell stays
// the default until the document model has been lived with.
const DefaultLayoutMode = LayoutFixed

// ParseLayoutMode resolves a settings value to a mode. Unknown values fall back
// to the default rather than erroring: the value arrives from settings.json,
// and a typo should not stop the UI from starting.
func ParseLayoutMode(raw string) (LayoutMode, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "default":
		return DefaultLayoutMode, true
	case string(LayoutFixed), "shell":
		return LayoutFixed, true
	case string(LayoutDocument), "doc", "flow":
		return LayoutDocument, true
	default:
		return DefaultLayoutMode, false
	}
}

// LayoutMode returns the active mode.
func (a *App) LayoutMode() LayoutMode {
	if a == nil || a.root == nil || a.root.layout == nil {
		return DefaultLayoutMode
	}
	if a.root.layout == tauui.View(a.root.document) {
		return LayoutDocument
	}
	return LayoutFixed
}

// SetLayoutMode switches the active layout.
//
// Both models are already built over the same region views, so this swaps a
// pointer and relayouts. Nothing is reconstructed, which is what makes flipping
// between them mid-session cheap enough to compare them directly.
//
// The chat region differs between the two: the fixed shell scrolls a viewport
// around the log, while the document lays the log out at full height. Switching
// therefore has to re-anchor the transcript, or the view lands at whatever
// offset the other model happened to be holding.
func (a *App) SetLayoutMode(mode LayoutMode) {
	if a == nil || a.root == nil {
		return
	}
	if a.LayoutMode() == mode {
		return
	}

	switch mode {
	case LayoutDocument:
		a.root.layout = a.root.document
		a.root.document.SetScrollBack(0)
		// Entering the document layout resets the counter too: the document
		// has no scroll-away tracking, so any value left from the fixed shell
		// would never decrement.
		a.resetNewMessageCount()
	default:
		a.root.layout = a.root.AppLayout
		if a.scroll != nil {
			a.scroll.ScrollBottom(a.loopContext())
			// Re-anchoring to the end of the transcript is a programmatic
			// follow, so any autoscroll suspension carried over from the
			// other layout resumes with it. setAutoScrollOff also drops
			// pendingNewMessages so the toast doesn't survive a switch that
			// was meant to re-pin the view.
			a.setAutoScrollOff(false)
		}
	}

	if ctx := a.loopContext(); ctx != nil {
		if ctx.InvalidateLayout != nil {
			ctx.InvalidateLayout()
		}
		if ctx.Invalidate != nil && a.root.Rect().W > 0 {
			ctx.Invalidate(a.root.Rect())
		}
	}
}

// ToggleLayoutMode flips between the two models and returns the new one.
func (a *App) ToggleLayoutMode() LayoutMode {
	if a.LayoutMode() == LayoutDocument {
		a.SetLayoutMode(LayoutFixed)
		return LayoutFixed
	}
	a.SetLayoutMode(LayoutDocument)
	return LayoutDocument
}

// cmdLayout reports or switches the layout model. With no argument it toggles,
// so the two can be compared with repeated invocations.
//
// A switch is persisted to settings.json. Comparing the models means flipping
// back and forth over days, and a choice that silently reverted on the next
// launch would be worse than no toggle at all. `/layout status` only reports,
// so it never writes.
func cmdLayout(app *App, args string) {
	if app == nil {
		return
	}
	arg := strings.ToLower(strings.TrimSpace(args))

	var mode LayoutMode
	switch arg {
	case "status", "show":
		mode = app.LayoutMode()
		app.conv.AddSystem("layout: " + string(mode) + " (" + layoutModeSummary(mode) + ")")
		app.refreshChat()
		return
	case "":
		mode = app.ToggleLayoutMode()
	default:
		parsed, ok := ParseLayoutMode(arg)
		if !ok {
			app.conv.AddError("layout: unknown mode " + arg + "; expected fixed or document")
			app.refreshChat()
			return
		}
		mode = parsed
		app.SetLayoutMode(mode)
	}

	app.conv.AddSystem("layout: " + string(mode) + " (" + layoutModeSummary(mode) + ")")
	if err := app.persistLayoutSetting(mode); err != nil {
		app.conv.AddError("layout: switched to " + string(mode) + " but failed to save it: " + err.Error())
	}
	app.refreshChat()
}

func layoutModeSummary(mode LayoutMode) string {
	if mode == LayoutDocument {
		return "one scrolling document; header scrolls away"
	}
	return "fixed header and editor; chat scrolls on its own"
}
