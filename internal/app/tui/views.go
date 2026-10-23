package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	tauui "github.com/losinggeneration/tau/tui"
)

var ansiEscapeRE = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\)|[PX^_].*?\x1b\\|[@-Z\\-_])`)

// footerData is the read model the Footer pulls from each paint. It carries
// persistent session information only; active-run loaders render in the status
// region, not in the footer.
type footerData struct {
	cwd         string
	branch      string
	sessionName string
	model       string

	contextUsed      int
	contextWindow    int
	contextUsedPct   float64
	inputTokens      int
	outputTokens     int
	cacheReadTokens  int
	cacheWriteTokens int
	costUSD          float64
	// totalTokens is retained as a fallback for sessions that only expose
	// aggregate usage.
	totalTokens   int
	providerCount int
	// provider names the active model's provider. The footer prefixes the
	// model with it only when more than one provider is available, per §10.3;
	// the previous version showed a provider *count* instead.
	provider string
	// supportsReasoning gates the thinking suffix: §10.3 appends it only for
	// models that actually reason, rather than hiding it when the level is off.
	supportsReasoning bool
	thinkingLevel     string
	// oauthSubscription marks cost as covered by a subscription, rendered as
	// the " (sub)" suffix.
	oauthSubscription bool
	queueCount        int
	toolRunning       bool
	autoCompaction    bool

	extensionStatuses map[string]string
	streaming         bool
	workingFrame      string
	notice            string
	theme             string
}

// Footer renders the bottom region as persistent session status rows:
// cwd/session, stats/model, and optional extension statuses.
type Footer struct {
	id   tauui.ID
	rect tauui.Rect
	data *footerData

	// tokens resolves the active theme's colours. Nil falls back to terminal
	// defaults, which keeps the Footer usable in tests without a live App.
	tokens func() themeTokens
}

func NewFooter(data *footerData) *Footer {
	if data == nil {
		data = &footerData{}
	}
	return &Footer{id: tauui.NewID(), data: data}
}

// dataOrEmpty returns the footer's data record, or an empty one when the footer
// (or its data) is absent. Callers that only need to read a value should use
// this rather than dereferencing through a possibly-nil footer.
func (f *Footer) dataOrEmpty() footerData {
	if f == nil || f.data == nil {
		return footerData{}
	}
	return *f.data
}

// SetThemeTokens installs the theme lookup so footer text tracks the active
// theme's dim/warning/error colours.
func (f *Footer) SetThemeTokens(fn func() themeTokens) {
	if f != nil {
		f.tokens = fn
	}
}

func (f *Footer) ID() tauui.ID                        { return f.id }
func (f *Footer) Rect() tauui.Rect                    { return f.rect }
func (f *Footer) Layout(r tauui.Rect)                 { f.rect = r }
func (f *Footer) Focusable() bool                     { return false }
func (f *Footer) Handle(tauui.Event, *tauui.Ctx) bool { return false }
func (f *Footer) PreferredSize() tauui.Size           { return f.MinSize() }

func (f *Footer) MinSize() tauui.Size {
	h := 2
	if f != nil && f.data != nil && len(f.extensionStatusParts()) > 0 {
		h = 3
	}
	return tauui.Size{W: 1, H: h}
}

// Paint renders the footer rows as dim text.
//
// It deliberately does not fill the region with a surface colour. The footer
// used to paint SurfaceMuted across every row, which read as a status bar;
// app.md §10.3 specifies rows that are "visually dim except threshold/error/
// warning spans", with no enclosing border and no background block.
func (f *Footer) Paint(d tauui.Drawer, ctx *tauui.Ctx) {
	if f == nil || f.rect.W <= 0 || f.rect.H <= 0 {
		return
	}
	base := f.baseStyle(ctx)
	rows := f.styledRows(ctx, f.rect.W)
	for i := range f.rect.H {
		rect := tauui.Rect{X: f.rect.X, Y: f.rect.Y + i, W: f.rect.W, H: 1}
		var row footerRow
		if i < len(rows) {
			row = rows[i]
		}
		paintFooterRow(d, rect, row, base)
	}
}

// baseStyle is the dim text style the footer renders in.
func (f *Footer) baseStyle(ctx *tauui.Ctx) tauui.Style {
	st := tauui.Style{FG: tauui.ColorBrightBlack}
	if ctx != nil && ctx.Theme.Base != (tauui.Style{}) {
		st = ctx.Theme.Base.WithFG(tauui.ColorBrightBlack)
	}
	if f != nil && f.tokens != nil {
		tokens := f.tokens()
		st = tokens.foreground("dim", tokens.Dim, tauui.Style{BG: tokens.Surface})
	}
	return st
}

// contextStyle returns the style for the context-usage span: warning above 70%,
// error above 90%, otherwise the row's dim style. Only that span is recoloured
// — the previous implementation restyled the entire row.
func (f *Footer) contextStyle(base tauui.Style) tauui.Style {
	if f == nil || f.data == nil || f.data.contextWindow <= 0 {
		return base
	}
	warn, err := tauui.ColorBrightYellow, tauui.ColorBrightRed
	if f.tokens != nil {
		tokens := f.tokens()
		warn, err = tokens.Warning, tokens.Error
		switch {
		case f.data.contextUsedPct >= 90:
			return tokens.foreground("error", tokens.Error, base)
		case f.data.contextUsedPct >= 70:
			return tokens.foreground("warning", tokens.Warning, base)
		}
	}
	switch {
	case f.data.contextUsedPct >= 90:
		return base.WithFG(err)
	case f.data.contextUsedPct >= 70:
		return base.WithFG(warn)
	}
	return base
}

// render is retained for tests and non-view helpers. It returns the footer rows
// joined with newlines.
func (f *Footer) render(width int) (string, tauui.Style) {
	return strings.Join(f.renderRows(width), "\n"), tauui.Style{FG: tauui.ColorWhite}
}

// renderRows returns the plain text of each row.
func (f *Footer) renderRows(width int) []string {
	rows := f.styledRows(nil, width)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.text())
	}
	if len(out) == 0 {
		return []string{"", ""}
	}
	return out
}

// styledRows builds the footer's two or three rows.
func (f *Footer) styledRows(ctx *tauui.Ctx, width int) []footerRow {
	if f == nil || f.data == nil || width <= 0 {
		return []footerRow{nil, nil}
	}
	base := f.baseStyle(ctx)
	rows := []footerRow{
		f.cwdRow(base).truncate(width),
		f.statsModelRow(base, width),
	}
	if status := f.extensionStatusRow(base); len(status) > 0 {
		rows = append(rows, status.truncate(width))
	}
	return rows
}

// cwdRow renders the working directory, git branch, and session name.
//
// Format per app.md §10.3: "<cwd> (<branch>) • <name>". The previous version
// joined fields with " · " and prefixed the branch "git:", and carried a
// "theme:" field the spec does not have — the active theme is discoverable
// through /settings.
func (f *Footer) cwdRow(base tauui.Style) footerRow {
	d := f.data
	cwd := d.cwd
	if cwd == "" {
		cwd = "."
	} else {
		cwd = shortenHome(cwd)
	}
	text := " " + cwd
	if d.branch != "" {
		text += " (" + d.branch + ")"
	}
	if d.sessionName != "" {
		text += " • " + d.sessionName
	}
	return footerRow{{text: text, style: base}}
}

// statsModelRow renders cumulative usage on the left and the model on the
// right, right-aligned when it fits.
func (f *Footer) statsModelRow(base tauui.Style, width int) footerRow {
	d := f.data
	if d.notice != "" {
		// A notice takes the whole row, right-aligned.
		notice := footerRow{{text: d.notice + " ", style: base}}
		return footerRow{{text: " ", style: base}}.
			pad(width-notice.width(), base).
			append(notice).
			truncate(width)
	}

	left := f.statsSpans(base)
	right := f.modelSpans(base)

	leftW, rightW := left.width(), right.width()
	// Too narrow for both: truncate the stats first, then drop the model side
	// entirely rather than clipping it to something unreadable.
	if leftW+rightW > width {
		if rightW < width {
			return joinFooterRows(left.truncate(width-rightW).pad(width-rightW, base), right)
		}
		return left.truncate(width)
	}
	return joinFooterRows(left.pad(width-rightW, base), right)
}

// statsSpans builds the token/cost/context side.
func (f *Footer) statsSpans(base tauui.Style) footerRow {
	d := f.data
	row := footerRow{{text: " ", style: base}}
	add := func(text string) {
		if text == "" {
			return
		}
		if row.width() > 1 {
			row = append(row, footerSpan{text: " ", style: base})
		}
		row = append(row, footerSpan{text: text, style: base})
	}

	if d.inputTokens > 0 {
		add("↑" + formatTokenCount(d.inputTokens))
	}
	if d.outputTokens > 0 {
		add("↓" + formatTokenCount(d.outputTokens))
	}
	if d.cacheReadTokens > 0 {
		add("R" + formatTokenCount(d.cacheReadTokens))
	}
	if d.cacheWriteTokens > 0 {
		add("W" + formatTokenCount(d.cacheWriteTokens))
	}
	if d.costUSD > 0 {
		cost := formatCostUSD(d.costUSD)
		if d.oauthSubscription {
			cost += " (sub)"
		}
		add(cost)
	}

	// queued/tool-running are tau additions, not spec fields. They are kept
	// because they carry information nothing else surfaces, and placed inside
	// the spec's left-hand run rather than replacing any part of it.
	if d.queueCount > 0 {
		add(fmt.Sprintf("%d queued", d.queueCount))
	}
	if d.toolRunning {
		add("tool running")
	}

	// The context span carries its own threshold style; everything else on the
	// row stays dim.
	if usage := f.contextUsageText(); usage != "" {
		if row.width() > 1 {
			row = append(row, footerSpan{text: " ", style: base})
		}
		row = append(row, footerSpan{text: usage, style: f.contextStyle(base)})
	} else if d.totalTokens > 0 && row.width() <= 1 {
		add("↑" + formatTokenCount(d.totalTokens))
	}
	return row
}

// contextUsageText renders context usage as "P%/W" or "?/W", with " (auto)"
// appended when auto-compaction is enabled (app.md §10.3). Auto-compaction was
// previously a separate "auto-compact" field.
func (f *Footer) contextUsageText() string {
	d := f.data
	var text string
	switch {
	case d.contextWindow > 0:
		text = fmt.Sprintf("%d%%/%s", int(d.contextUsedPct), formatTokenCount(d.contextWindow))
	case d.contextUsed > 0:
		text = "?/" + formatTokenCount(d.contextUsed)
	default:
		return ""
	}
	if d.autoCompaction {
		text += " (auto)"
	}
	return text
}

// modelSpans builds the right-hand model side: an optional "(provider) "
// prefix, the model id or "no-model", and the thinking level for reasoning
// models.
func (f *Footer) modelSpans(base tauui.Style) footerRow {
	d := f.data
	text := d.model
	if text == "" {
		text = "no-model"
	}
	if d.providerCount > 1 && d.provider != "" {
		text = "(" + d.provider + ") " + text
	}
	if d.supportsReasoning {
		if d.thinkingLevel == "" || d.thinkingLevel == "off" {
			text += " • thinking off"
		} else {
			text += " • " + d.thinkingLevel
		}
	}
	return footerRow{{text: text + " ", style: base}}
}

// extensionStatusRow renders the optional third row: extension statuses sorted
// by key, each sanitised to a single line, joined with spaces.
//
// This row previously never appeared in a real session — footerData's status
// map was written only by tests, while production statuses went to the status
// region, which app.md §9 reserves for active loaders.
func (f *Footer) extensionStatusRow(base tauui.Style) footerRow {
	parts := f.extensionStatusParts()
	if len(parts) == 0 {
		return nil
	}
	return footerRow{{text: " " + strings.Join(parts, " "), style: base}}
}

func (f *Footer) extensionStatusParts() []string {
	if f == nil || f.data == nil || len(f.data.extensionStatuses) == 0 {
		return nil
	}
	keys := make([]string, 0, len(f.data.extensionStatuses))
	for k := range f.data.extensionStatuses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if v := sanitizeStatus(f.data.extensionStatuses[k]); v != "" {
			parts = append(parts, v)
		}
	}
	return parts
}

func shortenHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	prefix := home + string(filepath.Separator)
	if strings.HasPrefix(path, prefix) {
		return "~" + string(filepath.Separator) + strings.TrimPrefix(path, prefix)
	}
	return path
}

func formatTokenCount(n int) string {
	if n < 0 {
		return "0"
	}
	const (
		k = 1_000
		m = 1_000_000
		g = 1_000_000_000
	)
	switch {
	case n >= g:
		return fmt.Sprintf("%sG", trimZero(fmt.Sprintf("%.1f", float64(n)/float64(g))))
	case n >= m:
		return fmt.Sprintf("%sM", trimZero(fmt.Sprintf("%.1f", float64(n)/float64(m))))
	case n >= k:
		return fmt.Sprintf("%sk", trimZero(fmt.Sprintf("%.1f", float64(n)/float64(k))))
	default:
		return fmt.Sprintf("%d", n)
	}
}

func formatCostUSD(v float64) string {
	if v <= 0 {
		return "$0"
	}
	return "$" + strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", v), "0"), ".")
}

func trimZero(s string) string {
	if len(s) >= 3 && strings.HasSuffix(s, ".0") {
		return s[:len(s)-2]
	}
	return s
}

func sanitizeStatus(s string) string {
	s = ansiEscapeRE.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
