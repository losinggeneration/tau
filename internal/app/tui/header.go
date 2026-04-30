package tui

import (
	"strings"
	"sync"

	tauui "github.com/losinggeneration/tau/tui"
)

// Header renders the startup banner as the layout's first region (app.md §9).
//
// Shape, compact:
//
//	<blank>
//	 tau 1.2.3                     ← logo bold accent, version dim
//	 Enter send · / commands · …   ← hints joined by dim separators
//	 ctrl+o for startup help       ← how to expand
//	 <blank>
//	 Type a message, or …          ← dim onboarding sentence
//	<blank>
//
// Expanded keeps the logo line, replaces the joined hints with one per line,
// then the blank and the same onboarding sentence.
//
// **Deviation, agreed 2026-08-10 (Harley):** the banner hides once the
// transcript has content, in *both* layout models. In the fixed shell it has to
// — a permanent seven-row banner would eat a third of a short terminal forever.
// In the document model it would scroll away on its own, so hiding is redundant
// there but harmless, and one behaviour is simpler than two. The spec implies a
// persistent region; this is a deliberate, recorded difference.
type Header struct {
	id   tauui.ID
	rect tauui.Rect

	mu       sync.Mutex
	version  string
	expanded bool
	hidden   bool
	// quiet is the quietStartup setting: the header has no visible rows at
	// all, independently of the transcript-driven hide.
	quiet bool
	// custom replaces the built-in content when an extension supplies its own
	// header (§8a setHeader). The surrounding spacer rows go with it: they are
	// part of the built-in header, not of the region.
	custom []string

	// hints are the compact keybinding hints, resolved from the live
	// keybinding registry so a rebind shows up here.
	hints func() []string
	// messages are startup diagnostics shown under the onboarding sentence.
	messages []string
	// tokens resolves theme colours; nil falls back to terminal defaults.
	tokens func() themeTokens
}

// NewHeader returns a header for the given version string.
func NewHeader(version string) *Header {
	return &Header{id: tauui.NewID(), version: strings.TrimSpace(version)}
}

func (h *Header) ID() tauui.ID                        { return h.id }
func (h *Header) Rect() tauui.Rect                    { return h.rect }
func (h *Header) Layout(r tauui.Rect)                 { h.rect = r }
func (h *Header) Focusable() bool                     { return false }
func (h *Header) Handle(tauui.Event, *tauui.Ctx) bool { return false }
func (h *Header) PreferredSize() tauui.Size           { return h.MinSize() }

// SetHintProvider installs the source for the compact keybinding hints.
func (h *Header) SetHintProvider(fn func() []string) {
	h.mu.Lock()
	h.hints = fn
	h.mu.Unlock()
}

// SetThemeTokens installs the theme lookup.
func (h *Header) SetThemeTokens(fn func() themeTokens) {
	h.mu.Lock()
	h.tokens = fn
	h.mu.Unlock()
}

// SetMessages installs startup diagnostics below the built-in help text.
func (h *Header) SetMessages(lines []string) {
	h.mu.Lock()
	h.messages = append([]string(nil), lines...)
	h.mu.Unlock()
}

// SetHidden hides or shows the header. The app hides it once the transcript has
// content, so the banner does not persist past the first exchange.
func (h *Header) SetHidden(hidden bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hidden == hidden {
		return false
	}
	h.hidden = hidden
	return true
}

// SetQuietStartup suppresses the header entirely (§9 quiet startup: "the
// built-in header has no visible rows unless an extension supplies a custom
// header"). Reports whether the value changed.
func (h *Header) SetQuietStartup(quiet bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.quiet == quiet {
		return false
	}
	h.quiet = quiet
	return true
}

// Hidden reports whether the header is currently suppressed.
func (h *Header) Hidden() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hidden
}

// SetExpanded switches between the compact and expanded forms.
func (h *Header) SetExpanded(expanded bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.expanded == expanded {
		return false
	}
	h.expanded = expanded
	return true
}

// Expanded reports whether the expanded form is showing.
func (h *Header) Expanded() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.expanded
}

// SetCustom replaces the built-in header content (§8a setHeader). Passing nil
// restores the built-in header.
func (h *Header) SetCustom(lines []string) {
	h.mu.Lock()
	h.custom = append([]string(nil), lines...)
	if len(lines) == 0 {
		h.custom = nil
	}
	h.mu.Unlock()
}

// headerLine is one rendered row and its style role.
type headerLine struct {
	text string
	// accent marks the logo line, which is bold accent-styled; everything else
	// is dim (§9).
	accent bool
}

// lines builds the header's rows for the current state.
func (h *Header) lines() []headerLine {
	h.mu.Lock()
	version, expanded, hidden, custom := h.version, h.expanded, h.hidden, h.custom
	quiet, hintFn := h.quiet, h.hints
	messages := append([]string(nil), h.messages...)
	h.mu.Unlock()

	if hidden || quiet {
		return nil
	}
	if len(custom) > 0 {
		// A custom header replaces the content, spacers included: those belong
		// to the built-in header, not to the region.
		out := make([]headerLine, 0, len(custom))
		for _, line := range custom {
			out = append(out, headerLine{text: " " + line})
		}
		return out
	}

	brand := "tau"
	if version != "" {
		brand += " " + version
	}

	var hints []string
	if hintFn != nil {
		hints = hintFn()
	}
	if len(hints) == 0 {
		hints = []string{"Enter send", "Shift+Enter newline", "/ commands", "Ctrl+C quit"}
	}

	out := []headerLine{{text: ""}, {text: " " + brand, accent: true}}
	if expanded {
		for _, hint := range hints {
			out = append(out, headerLine{text: "   " + hint})
		}
	} else {
		out = append(out, headerLine{text: " " + strings.Join(hints, " · ")})
		out = append(out, headerLine{text: " " + headerExpandHint})
	}
	out = append(out,
		headerLine{text: ""},
		headerLine{text: " Type a message, or use /help for commands and resources."},
	)
	for _, msg := range messages {
		for _, line := range strings.Split(msg, "\n") {
			if strings.TrimSpace(line) != "" {
				out = append(out, headerLine{text: " " + line})
			}
		}
	}
	out = append(out, headerLine{text: ""})
	return out
}

// headerExpandHint tells the user how to expand startup help. §9 requires the
// compact form to carry this line; it was missing entirely before.
const headerExpandHint = "ctrl+o for startup help · /hotkeys for all keybindings"

func (h *Header) MinSize() tauui.Size {
	lines := h.lines()
	if len(lines) == 0 {
		return tauui.Size{}
	}
	return tauui.Size{W: 1, H: len(lines)}
}

func (h *Header) Paint(d tauui.Drawer, ctx *tauui.Ctx) {
	if h == nil || h.rect.W <= 0 || h.rect.H <= 0 {
		return
	}
	h.mu.Lock()
	tokenFn := h.tokens
	h.mu.Unlock()

	base := tauui.Style{FG: tauui.ColorBrightBlack}
	accent := tauui.Style{FG: tauui.ColorWhite, Attr: tauui.AttrBold}
	if tokenFn != nil {
		tokens := tokenFn()
		base = tauui.Style{FG: tokens.Dim, BG: tokens.Surface}
		accent = tauui.Style{FG: tokens.Accent, BG: tokens.Surface, Attr: tauui.AttrBold}
	} else if ctx != nil && ctx.Theme.Base != (tauui.Style{}) {
		base = ctx.Theme.Base.WithFG(tauui.ColorBrightBlack)
		accent = ctx.Theme.Base.WithAttr(tauui.AttrBold)
	}

	lines := h.lines()
	for i := range h.rect.H {
		y := h.rect.Y + i
		st := base
		text := ""
		if i < len(lines) {
			if lines[i].accent {
				st = accent
			}
			text = lines[i].text
		}
		d.FillRect(tauui.Rect{X: h.rect.X, Y: y, W: h.rect.W, H: 1}, base)
		if text != "" {
			d.DrawText(tauui.Point{X: h.rect.X, Y: y}, tauui.TextTruncate(text, h.rect.W, false), st)
		}
	}
}

// Render returns the header's plain text, for tests and transcript helpers.
func (h *Header) Render(width int) (string, tauui.Style) {
	lines := h.lines()
	rows := make([]string, 0, len(lines))
	for _, line := range lines {
		rows = append(rows, tauui.TextTruncate(line.text, width, false))
	}
	return strings.Join(rows, "\n"), tauui.Style{}
}

// headerHints returns the compact keybinding hints, resolved from the live
// registry so a user rebind is reflected rather than the shipped default.
func (a *App) headerHints() []string {
	manager := a.keybindings
	if manager == nil {
		manager = defaultAppKeybindings()
	}
	hints := []string{"Enter send", "Shift+Enter newline", "/ commands"}
	if keys := manager.Keys(actionIDExit); len(keys) > 0 {
		hints = append(hints, strings.Join(keys, "/")+" quit")
	}
	return hints
}

// refreshHeaderVisibility hides the banner once the transcript has content and
// restores it when the transcript is cleared (a /new session).
//
// Agreed deviation: this applies in both layout models. See the Header doc.
func (a *App) refreshHeaderVisibility() {
	if a == nil || a.header == nil || a.conv == nil {
		return
	}
	if !a.header.SetHidden(!a.conv.IsEmpty()) {
		return
	}
	if ctx := a.loopContext(); ctx != nil && ctx.InvalidateLayout != nil {
		ctx.InvalidateLayout()
	}
}

// ToggleHeaderExpanded switches the startup help between compact and expanded.
func (a *App) ToggleHeaderExpanded() {
	if a == nil || a.header == nil {
		return
	}
	if !a.header.SetExpanded(!a.header.Expanded()) {
		return
	}
	if ctx := a.loopContext(); ctx != nil && ctx.InvalidateLayout != nil {
		ctx.InvalidateLayout()
	}
}
