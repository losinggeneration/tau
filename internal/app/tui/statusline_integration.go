package tui

import (
	"path/filepath"
	"strings"

	"github.com/losinggeneration/tau/tui/statusline"
)

// statuslineStateFromFooter derives a statusline.State view from the
// app's footer data and a few live app flags. It lets the app lean on
// the tui/statusline helper for the "running/queued/tools/tok/$ …"
// footer line rather than duplicating that format locally.
//
// The conversion is intentionally lossless-but-additive: anything
// already in footerData maps cleanly, while running/abortable/started
// (which aren't on footerData today) come from the app directly. Callers
// that don't have an *app handle can still use the helper by passing a
// zero-value footer and overriding fields after the fact.
func statuslineStateFromFooter(d *footerData) statusline.State {
	s := statusline.State{Abortable: true}
	if d == nil {
		return s
	}
	if d.model != "" {
		// footerData stores "provider/model" in a single field
		// (set by app.go from ModelSpec). Split on the first '/'
		// when possible so the statusline helper can render the
		// provider/model pair the way the spec expects.
		if i := strings.IndexByte(d.model, '/'); i > 0 && i < len(d.model)-1 {
			s.Provider = d.model[:i]
			s.Model = d.model[i+1:]
		} else {
			s.Model = d.model
		}
	}
	if d.branch != "" {
		s.Branch = d.branch
	}
	if d.cwd != "" {
		s.Session = filepath.Base(d.cwd)
	}
	s.Pending = d.queueCount
	s.ToolsEnabled = d.providerCount
	s.Running = d.streaming || d.toolRunning
	if d.thinkingLevel != "" && d.thinkingLevel != "off" {
		s.Thinking = true
	}
	if d.contextWindow > 0 || d.contextUsed > 0 {
		s.Cost = statusline.Cost{
			InputTokens:  d.contextUsed,
			OutputTokens: d.totalTokens,
		}
	}
	return s
}

// renderStatuslineFooter returns the statusline helper's footer
// representation (running/queued/tools/tok/$ ...) for the given width.
// It is exposed for tests and for overlays that want a compact status
// bar (e.g. confirm-to-quit, splash) without spinning up the full
// Footer widget.
func renderStatuslineFooter(d *footerData, width int) string {
	return statusline.RenderFooter(statuslineStateFromFooter(d), width)
}

// renderStatuslineHeader returns the statusline helper's header
// representation (session · branch · provider/model · thinking) for
// the given width.
func renderStatuslineHeader(d *footerData, width int) string {
	return statusline.RenderHeader(statuslineStateFromFooter(d), width)
}
