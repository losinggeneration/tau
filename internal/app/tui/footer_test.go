package tui

import (
	"strings"
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
)

// TestFormatTokenCount locks in the k/M/G compact formatting used
// by the footer's context-usage display.
func TestFormatTokenCount(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1k"},
		{1500, "1.5k"},
		{12000, "12k"},
		{12500, "12.5k"},
		{999_999, "1000k"},
		{1_000_000, "1M"},
		{1_500_000, "1.5M"},
		{12_000_000, "12M"},
		{1_000_000_000, "1G"},
		{-1, "0"},
	}
	for _, tc := range cases {
		got := formatTokenCount(tc.in)
		if got != tc.want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSanitizeStatus covers the single-line status sanitization
// used by extension status entries: control chars and newlines are
// collapsed so the bar stays one row.
func TestSanitizeStatus(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"newlines", "line1\nline2\nline3", "line1 line2 line3"},
		{"crlf", "a\r\nb", "a b"},
		{"tabs", "a\tb\t\tc", "a b c"},
		{"deletes_ctrl", "a\x00b\x01c", "abc"},
		{"deletes_DEL", "a\x7fb", "ab"},
		{"collapses_whitespace", "  hello   world  ", "hello world"},
		{"empty_after_sanitize", "\x00\x01\t\n", ""},
		{"unicode_preserved", "café", "café"},
		{"strips_sgr_escape", "café \x1b[31mred\x1b[0m", "café red"},
		{"strips_osc_escape", "before \x1b]0;title\x07 after", "before after"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeStatus(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeStatus(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFooterRender exercises the field-stack rendering with various
// footer data permutations. The render must include the new fields
// (branch, used/window/pct, providers, extension statuses,
// auto-compaction) when populated, and omit them when empty.
func TestFooterStatsModelRowDetailedUsage(t *testing.T) {
	f := NewFooter(&footerData{
		cwd:              "/tmp",
		model:            "anthropic/claude-sonnet-4-6",
		inputTokens:      1200,
		outputTokens:     345,
		cacheReadTokens:  2000,
		cacheWriteTokens: 50,
		costUSD:          0.0375,
		contextWindow:    200_000,
		contextUsedPct:   72.3,
	})
	got, _ := f.render(120)
	for _, want := range []string{"↑1.2k", "↓345", "R2k", "W50", "$0.0375", "72%/200k", "anthropic/claude"} {
		if !strings.Contains(got, want) {
			t.Fatalf("footer detailed usage missing %q:\n%s", want, got)
		}
	}
}

func TestFooterApplyUsagePayload(t *testing.T) {
	a := &App{footer: NewFooter(&footerData{})}
	a.applyUsagePayload(map[string]any{
		"this_call": map[string]any{
			"input":      float64(100),
			"output":     float64(20),
			"cacheRead":  float64(5),
			"cacheWrite": float64(3),
			"cost": map[string]any{
				"total": float64(0.012),
			},
		},
	})
	data := a.footer.data
	if data.inputTokens != 100 || data.outputTokens != 20 || data.cacheReadTokens != 5 || data.cacheWriteTokens != 3 || data.costUSD != 0.012 {
		t.Fatalf("footer usage = input:%d output:%d read:%d write:%d cost:%g", data.inputTokens, data.outputTokens, data.cacheReadTokens, data.cacheWriteTokens, data.costUSD)
	}
}

// TestFooterRender exercises the field-stack rendering with various
// footer data permutations. The render must include the new fields
// (branch, used/window/pct, providers, extension statuses,
// auto-compaction) when populated, and omit them when empty.
// The threshold recolours only the context span. Restyling the whole row —
// what the previous implementation did — makes the cwd, token counts and model
// all turn red at 90%, which is noise rather than a signal.
func TestFooterContextThresholdStylesOnlyTheUsageSpan(t *testing.T) {
	ctx := &tauui.Ctx{Theme: appTheme()}

	cases := []struct {
		pct  float64
		want tauui.Color
	}{
		{50, tauui.ColorBrightBlack},
		{75, tauui.ColorBrightYellow},
		{95, tauui.ColorBrightRed},
	}
	for _, tc := range cases {
		f := NewFooter(&footerData{cwd: ".", contextWindow: 100, contextUsedPct: tc.pct, model: "m"})
		rows := f.styledRows(ctx, 120)
		if len(rows) < 2 {
			t.Fatalf("%v%%: footer produced %d rows", tc.pct, len(rows))
		}

		var usage, other []footerSpan
		for _, span := range rows[1] {
			if strings.Contains(span.text, "%/") {
				usage = append(usage, span)
			} else if strings.TrimSpace(span.text) != "" {
				other = append(other, span)
			}
		}
		if len(usage) != 1 {
			t.Fatalf("%v%%: found %d context spans, want exactly 1 (row: %q)", tc.pct, len(usage), rows[1].text())
		}
		if usage[0].style.FG != tc.want {
			t.Errorf("%v%%: context span FG = %v, want %v", tc.pct, usage[0].style.FG, tc.want)
		}
		for _, span := range other {
			if span.style.FG == tauui.ColorBrightYellow || span.style.FG == tauui.ColorBrightRed {
				t.Errorf("%v%%: span %q was recoloured with the threshold; only the usage span should be",
					tc.pct, span.text)
			}
		}
	}
}

func TestFooterCwdRowIncludesSessionName(t *testing.T) {
	f := NewFooter(&footerData{cwd: "/tmp/project", branch: "main", sessionName: "visual pass"})
	got, _ := f.render(120)
	firstRow := strings.Split(got, "\n")[0]
	// §10.3: "<cwd> (<branch>) • <name>" — no "git:" prefix, no " · " joins.
	for _, want := range []string{"/tmp/project", "(main)", "• visual pass"} {
		if !strings.Contains(firstRow, want) {
			t.Fatalf("cwd row missing %q: %q", want, firstRow)
		}
	}
}

func TestFooterRender(t *testing.T) {
	st := func() tauui.Style { return tauui.Style{FG: tauui.ColorWhite, BG: tauui.ColorBrightBlack} }
	_ = st // silence unused warning; style is captured by Paint but
	// not asserted on here because rendering layout is exercised
	// via Paint, which isn't called from these unit tests.

	t.Run("minimal", func(t *testing.T) {
		f := NewFooter(&footerData{cwd: "/tmp"})
		got, _ := f.render(120)
		if !strings.HasPrefix(got, " /tmp") {
			t.Errorf("minimal footer should start with cwd: %q", got)
		}
		// No branch, no stats → just " <cwd>" and a stats row carrying only
		// the model side. "idle" was a tau invention with no spec counterpart;
		// an empty stats side now renders as empty rather than as a word.
		if strings.Contains(got, "idle") {
			t.Errorf("footer should not invent an 'idle' field: %q", got)
		}
		if !strings.Contains(got, "no-model") {
			t.Errorf("a footer with no model should say so: %q", got)
		}
	})

	t.Run("full", func(t *testing.T) {
		f := NewFooter(&footerData{
			cwd:            "/home/user/proj",
			branch:         "main",
			model:          "anthropic/claude-sonnet-4-6",
			contextUsed:    1200,
			contextWindow:  200_000,
			contextUsedPct: 1,
			providerCount:  3,
			autoCompaction: true,
			extensionStatuses: map[string]string{
				"ctx": "on",
			},
			streaming: true,
		})
		got, _ := f.render(120)
		// Must include every populated field, in the expected order:
		// §10.3 shape: "<cwd> (<branch>)" / stats + "P%/W (auto)" + model / statuses
		wantSubs := []string{
			"proj",       // cwd basename
			"(main)",     // branch, parenthesised
			"anthropic/", // model (truncated long ids are fine)
			"1%/200k",    // context usage
			"(auto)",     // auto-compaction, suffixed on the usage
			"on",         // extension status value (keys are not shown)
		}
		for _, s := range wantSubs {
			if !strings.Contains(got, s) {
				t.Errorf("full footer missing %q:\n%s", s, got)
			}
		}
	})

	t.Run("notice_overrides", func(t *testing.T) {
		f := NewFooter(&footerData{
			cwd:    "/tmp",
			branch: "main",
			model:  "anthropic/claude-sonnet-4-6",
			notice: "press ctrl+c to exit",
		})
		got, _ := f.render(120)
		if !strings.Contains(got, "press ctrl+c to exit") {
			t.Errorf("notice should appear in footer: %q", got)
		}
		// Notice path should suppress auto-compact (which would be
		// shown if it were set) and right-field stats.
		if strings.Contains(got, "providers") {
			t.Errorf("notice path should suppress right-field stats: %q", got)
		}
	})

	t.Run("extension_status_sanitized", func(t *testing.T) {
		f := NewFooter(&footerData{
			cwd: "/tmp",
			extensionStatuses: map[string]string{
				"x": "multi\nline\tvalue",
			},
		})
		got, _ := f.render(120)
		// §10.3 renders the sanitized value only; keys order the row but are
		// not shown.
		if !strings.Contains(got, "multi line value") {
			t.Errorf("extension status should be sanitized: %q", got)
		}
		if strings.Count(got, "\n") != 2 {
			t.Errorf("footer with extension status should render three rows: %q", got)
		}
	})

	t.Run("falls_back_when_window_unknown", func(t *testing.T) {
		// contextUsed set but contextWindow == 0 → no ContextWindow
		// yet (e.g. before the first Stats() call) → fall back to
		// the cumulative token display rather than rendering "0/0".
		f := NewFooter(&footerData{
			cwd:         "/tmp",
			contextUsed: 42,
			totalTokens: 42,
		})
		got, _ := f.render(120)
		if strings.Contains(got, "0/0") {
			t.Errorf("should not render 0/0 when window is unknown: %q", got)
		}
		if !strings.Contains(got, "?/42") {
			t.Errorf("should render unknown-window context display: %q", got)
		}
	})
}

// TestFooterRenderTruncatesLongCwd ensures the cwd is truncated to 20
// columns like before; the new fields shouldn't change that.
func TestFooterRenderTruncatesLongCwd(t *testing.T) {
	f := NewFooter(&footerData{cwd: "/home/user/some/very/long/path/that/exceeds/twenty/chars"})
	got, _ := f.render(120)
	// Basename of the path is "chars"; the 20-col truncate of the
	// basename leaves it intact because it's shorter than 20.
	if !strings.HasPrefix(got, " /home/user/some/very/long/path") {
		t.Errorf("footer should start with cwd path: %q", got)
	}
}

// TestDetectGitBranch covers the subprocess helper. It's a smoke
// test — the project itself is a git repo, so the call should
// succeed and return "code-agent" (the current branch).
func TestDetectGitBranch(t *testing.T) {
	branch := detectGitBranch(".")
	if branch == "" {
		t.Skip("git not available or not in a git repo")
	}
	if branch == "HEAD" {
		t.Errorf("detectGitBranch returned detached HEAD marker")
	}
}

// §10.3: the footer has no enclosing border and no background block; its rows
// are dim text. It used to paint SurfaceMuted across every row, which read as a
// status bar rather than as chrome.
func TestFooterDoesNotPaintAStatusBarBackground(t *testing.T) {
	f := NewFooter(&footerData{cwd: "/tmp/project", model: "anthropic/x"})
	f.SetThemeTokens(func() themeTokens { return defaultThemeTokens() })
	f.Layout(tauui.Rect{X: 0, Y: 10, W: 80, H: 2})

	d := &recordingDrawer{}
	f.Paint(d, &tauui.Ctx{Theme: appTheme()})

	surface := defaultThemeTokens().Surface
	for _, fill := range d.fills {
		if fill.st.BG != surface {
			t.Errorf("footer filled a row with %v, want the plain surface %v — no status-bar block",
				fill.st.BG, surface)
		}
	}
	if len(d.texts) == 0 {
		t.Fatal("footer painted no text")
	}
	dim := defaultThemeTokens().Dim
	for _, txt := range d.texts {
		if strings.TrimSpace(txt.text) == "" {
			continue
		}
		if txt.st.FG != dim {
			t.Errorf("footer text %q drawn in %v, want the dim token %v", txt.text, txt.st.FG, dim)
		}
	}
}

func TestFooterCwdRowFollowsTheSpecGrammar(t *testing.T) {
	cases := []struct {
		name string
		data footerData
		want string
	}{
		{"cwd only", footerData{cwd: "/tmp/p"}, " /tmp/p"},
		{"with branch", footerData{cwd: "/tmp/p", branch: "main"}, " /tmp/p (main)"},
		{"with name", footerData{cwd: "/tmp/p", sessionName: "vis"}, " /tmp/p • vis"},
		{"both", footerData{cwd: "/tmp/p", branch: "main", sessionName: "vis"}, " /tmp/p (main) • vis"},
	}
	for _, tc := range cases {
		f := NewFooter(&tc.data)
		got := f.renderRows(120)[0]
		if got != tc.want {
			t.Errorf("%s: cwd row = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The theme name is not a §10.3 field; it was a tau addition that cost row
// width on every session and is discoverable through /settings.
func TestFooterCwdRowOmitsTheThemeName(t *testing.T) {
	f := NewFooter(&footerData{cwd: "/tmp/p", theme: "tau"})
	if row := f.renderRows(120)[0]; strings.Contains(row, "theme") || strings.Contains(row, "tau") {
		t.Errorf("cwd row carries the theme name: %q", row)
	}
}

func TestFooterStatsRowFollowsTheSpecGrammar(t *testing.T) {
	f := NewFooter(&footerData{
		cwd:              "/tmp",
		inputTokens:      1200,
		outputTokens:     340,
		cacheReadTokens:  50,
		cacheWriteTokens: 20,
		contextWindow:    200000,
		contextUsedPct:   6,
		model:            "anthropic/sonnet",
	})
	row := f.renderRows(200)[1]

	for _, want := range []string{"↑1.2k", "↓340", "R50", "W20", "6%/200k", "anthropic/sonnet"} {
		if !strings.Contains(row, want) {
			t.Errorf("stats row missing %q: %q", want, row)
		}
	}
}

// An OAuth subscription marks the cost " (sub)" rather than reporting a dollar
// figure the user is not actually being charged.
func TestFooterMarksSubscriptionCost(t *testing.T) {
	f := NewFooter(&footerData{cwd: "/tmp", costUSD: 0.42, costKnown: true, oauthSubscription: true})
	if row := f.renderRows(200)[1]; !strings.Contains(row, "(sub)") {
		t.Errorf("subscription cost not marked: %q", row)
	}

	plain := NewFooter(&footerData{cwd: "/tmp", costUSD: 0.42, costKnown: true})
	if row := plain.renderRows(200)[1]; strings.Contains(row, "(sub)") {
		t.Errorf("non-subscription cost marked as one: %q", row)
	}
}

// A missing model must say "no-model" rather than leaving a blank right side,
// which reads as "the footer is broken" instead of "nothing is selected".
func TestFooterReportsNoModel(t *testing.T) {
	f := NewFooter(&footerData{cwd: "/tmp"})
	if row := f.renderRows(120)[1]; !strings.Contains(row, "no-model") {
		t.Errorf("stats row does not report a missing model: %q", row)
	}
}

// The provider prefix appears only when there is more than one provider to
// disambiguate between.
func TestFooterProviderPrefixOnlyWhenAmbiguous(t *testing.T) {
	multi := NewFooter(&footerData{cwd: "/tmp", model: "anthropic/x", provider: "anthropic", providerCount: 3})
	if row := multi.renderRows(120)[1]; !strings.Contains(row, "(anthropic) ") {
		t.Errorf("multi-provider footer lacks the provider prefix: %q", row)
	}

	single := NewFooter(&footerData{cwd: "/tmp", model: "anthropic/x", provider: "anthropic", providerCount: 1})
	if row := single.renderRows(120)[1]; strings.Contains(row, "(anthropic) ") {
		t.Errorf("single-provider footer should not prefix the model: %q", row)
	}
}

// Narrow terminals truncate the stats first and drop the model side rather
// than clipping it into nonsense.
func TestFooterDegradesGracefullyWhenNarrow(t *testing.T) {
	f := NewFooter(&footerData{
		cwd:           "/a/very/long/working/directory/path",
		branch:        "some-long-branch-name",
		inputTokens:   123456,
		contextWindow: 200000,
		model:         "anthropic/claude-sonnet-4-6",
	})
	for _, width := range []int{120, 60, 30, 12, 4, 1} {
		for i, row := range f.renderRows(width) {
			if got := tauui.TextWidth(row); got > width {
				t.Errorf("width %d: row %d is %d cells wide: %q", width, i, got, row)
			}
		}
	}
}

// §9 reserves the status region for active loaders. Extension statuses belong
// to footer row 3 — which previously never rendered, because nothing in
// production wrote to it.
func TestExtensionStatusesGoToTheFooterNotTheStatusRegion(t *testing.T) {
	a := newTestApp(t)
	a.footer = newFooterForTest()
	a.status = tauui.NewStatusEntriesView(5)

	a.setExtensionStatus("lint", "3 warnings")

	if got := a.footer.data.extensionStatuses["lint"]; got != "3 warnings" {
		t.Errorf("footer extension status = %q, want it recorded", got)
	}
	if _, ok := a.statusEntries["lint"]; ok {
		t.Error("extension status leaked into the status region, which is for loaders")
	}
	if rows := a.footer.MinSize().H; rows != 3 {
		t.Errorf("footer height = %d, want 3 once a status row exists", rows)
	}

	a.setExtensionStatus("lint", "")
	if len(a.footer.data.extensionStatuses) != 0 {
		t.Error("clearing an extension status left it behind")
	}
	if rows := a.footer.MinSize().H; rows != 2 {
		t.Errorf("footer height = %d after clearing, want it back to 2", rows)
	}
}

// The run indicator stays in the status region under its own key, so it cannot
// be clobbered by an extension using the same name.
func TestRunIndicatorStaysInTheStatusRegion(t *testing.T) {
	a := newTestApp(t)
	a.footer = newFooterForTest()
	a.status = tauui.NewStatusEntriesView(5)

	a.setStatus(loaderStatusKey, "⠋ Working...")

	if got := a.statusEntries[loaderStatusKey]; !strings.Contains(got, defaultWorkingMessage) {
		t.Errorf("status region = %q, want the run loader", got)
	}
	if len(a.footer.data.extensionStatuses) != 0 {
		t.Error("the run loader leaked into the footer's extension row")
	}
}

// §9 gives the run message a default and lets an extension override it; it was
// previously the hardcoded literal " working".
func TestWorkingMessageDefaultAndOverride(t *testing.T) {
	a := newTestApp(t)

	if got := a.workingMessage(); got != defaultWorkingMessage {
		t.Errorf("default working message = %q, want %q", got, defaultWorkingMessage)
	}

	a.SetWorkingMessage("Compiling...")
	if got := a.workingMessage(); got != "Compiling..." {
		t.Errorf("overridden working message = %q, want the override", got)
	}

	a.SetWorkingMessage("")
	if got := a.workingMessage(); got != defaultWorkingMessage {
		t.Errorf("cleared working message = %q, want the default back", got)
	}
}

func TestProviderFromModelSpec(t *testing.T) {
	cases := map[string]string{
		"anthropic/claude-sonnet-4-6": "anthropic",
		"openai/gpt-5":                "openai",
		"bare-model-id":               "",
		"":                            "",
	}
	for spec, want := range cases {
		if got := providerFromModelSpec(spec); got != want {
			t.Errorf("providerFromModelSpec(%q) = %q, want %q", spec, got, want)
		}
	}
}
