package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	sdk "github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/extensions"
)

// §9: compaction_end commits a summary message. It used to be unreachable —
// the SDK dispatched compaction events to the plugin manager only.
func TestCompactionEndCommitsASummaryMessage(t *testing.T) {
	conv := NewConversationModel()
	conv.AddCompactionSummary("Discussed the footer grammar and the theme tokens.", 12000)

	components := conv.Snapshot().Components
	if len(components) != 1 {
		t.Fatalf("got %d components, want 1", len(components))
	}
	c := components[0]
	if c.Kind != ChatComponentCompactionSummary {
		t.Errorf("kind = %q, want %q", c.Kind, ChatComponentCompactionSummary)
	}
	if !strings.Contains(c.Text, "footer grammar") {
		t.Errorf("summary text = %q, want the summary body", c.Text)
	}
	if c.Custom.Type != compactionCustomType {
		t.Errorf("custom type = %q, want %q so extension renderers can key off it", c.Custom.Type, compactionCustomType)
	}
	if !strings.Contains(c.Custom.Title, "12k") {
		t.Errorf("title = %q, want the reclaimed token count", c.Custom.Title)
	}
}

// A compaction with no summary must not leave an empty box in the transcript.
func TestEmptyCompactionSummaryIsDropped(t *testing.T) {
	conv := NewConversationModel()
	conv.AddCompactionSummary("   \n ", 500)
	if got := len(conv.Snapshot().Components); got != 0 {
		t.Errorf("empty summary produced %d components, want 0", got)
	}
}

// A session that reports no token count still gets a usable header.
func TestCompactionTitleWithoutATokenCount(t *testing.T) {
	if got := compactionTitle(0); got != "Context compacted" {
		t.Errorf("title = %q, want the bare header", got)
	}
	if got := compactionTitle(2500); !strings.Contains(got, "2.5k") {
		t.Errorf("title = %q, want the formatted count", got)
	}
}

// The summary renders with its `[compaction]` provenance tag. The palette has
// carried a customLabel style since the theme work and nothing used it.
func TestCompactionSummaryRendersALabelRow(t *testing.T) {
	conv := NewConversationModel()
	conv.AddCompactionSummary("A summary body.", 1000)
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 60, H: 20})
	waitForChatLog(t, log)

	log.mu.Lock()
	rows := renderLineTexts(log.lines)
	styles := make([]tauui.Style, len(log.lines))
	for i, l := range log.lines {
		styles[i] = l.st
	}
	log.mu.Unlock()

	joined := strings.Join(rows, "\n")
	for _, want := range []string{compactionLabel, "Context compacted", "A summary body."} {
		if !strings.Contains(joined, want) {
			t.Errorf("rendered summary missing %q:\n%s", want, joined)
		}
	}

	var labelStyled bool
	for i, row := range rows {
		if strings.Contains(row, compactionLabel) {
			if styles[i].Attr&tauui.AttrBold == 0 {
				t.Errorf("label row %q is not drawn in the customLabel style", row)
			}
			labelStyled = true
		}
	}
	if !labelStyled {
		t.Error("no label row was rendered")
	}
}

// The transcript export keeps the summary; §9 makes it part of history, not
// chrome that vanishes on export.
func TestCompactionSummarySurvivesTranscriptExport(t *testing.T) {
	conv := NewConversationModel()
	conv.AddCompactionSummary("Kept context.", 400)

	out := FormatTranscriptMarkdown(conv.Snapshot(), TranscriptFormatOptions{})
	if !strings.Contains(out, "Kept context.") {
		t.Errorf("transcript dropped the summary:\n%s", out)
	}
}

// The compaction indicator lives under its own status key so a compaction
// during an active run does not clobber the run spinner.
func TestCompactionIndicatorUsesItsOwnStatusKey(t *testing.T) {
	a := newTestApp(t)
	a.status = tauui.NewStatusEntriesView(5)
	a.setStatus(loaderStatusKey, "⠋ Working...")

	a.applySessionEvent(sdk.Event{Type: "compaction_start"})

	if got := a.statusEntries[compactionStatusKey]; !strings.Contains(got, "Compacting") {
		t.Errorf("compaction status = %q, want the compaction indicator", got)
	}
	if got := a.statusEntries[loaderStatusKey]; !strings.Contains(got, "Working") {
		t.Errorf("run loader = %q, want it untouched by the compaction indicator", got)
	}

	a.applySessionEvent(sdk.Event{
		Type:    "compaction_end",
		Payload: map[string]any{"summary": "done", "tokens_before": 900},
	})
	if _, ok := a.statusEntries[compactionStatusKey]; ok {
		t.Error("compaction indicator outlived the compaction")
	}
	if got := a.statusEntries[loaderStatusKey]; !strings.Contains(got, "Working") {
		t.Errorf("run loader = %q, want it still present", got)
	}
}

// The payload crosses the SDK boundary as a map and may have been through JSON,
// which turns every number into a float64.
func TestCompactionPayloadDecoding(t *testing.T) {
	cases := []struct {
		name          string
		payload       any
		wantText      string
		wantTokens    int
		wantFallback  bool
		wantFallbackE string
	}{
		{"native ints", map[string]any{"summary": "s", "tokens_before": 42}, "s", 42, false, ""},
		{"json floats", map[string]any{"summary": "s", "tokens_before": float64(42)}, "s", 42, false, ""},
		{"int64", map[string]any{"summary": "s", "tokens_before": int64(42)}, "s", 42, false, ""},
		{"missing count", map[string]any{"summary": "s"}, "s", 0, false, ""},
		{"missing summary", map[string]any{"tokens_before": 42}, "", 42, false, ""},
		{"fallback error", map[string]any{"summary": "s", "tokens_before": 42, "fallback": true, "fallback_error": "provider 400"}, "s", 42, true, "provider 400"},
		{"not a map", "nonsense", "", 0, false, ""},
		{"nil", nil, "", 0, false, ""},
	}
	for _, tc := range cases {
		summary, tokens, fallback, fallbackErr := compactionPayload(tc.payload)
		if summary != tc.wantText || tokens != tc.wantTokens || fallback != tc.wantFallback || fallbackErr != tc.wantFallbackE {
			t.Errorf("%s: got (%q, %d, %v, %q), want (%q, %d, %v, %q)", tc.name, summary, tokens, fallback, fallbackErr, tc.wantText, tc.wantTokens, tc.wantFallback, tc.wantFallbackE)
		}
	}
}

// When the model-driven summary fails, the SDK flags the compaction_end
// payload; the transcript must show the summary came from the local fallback
// rather than presenting it as model output.
func TestCompactionEndMarksFallbackSummary(t *testing.T) {
	a := newTestApp(t)

	a.applySessionEvent(sdk.Event{
		Type: "compaction_end",
		Payload: map[string]any{
			"summary":        "user: hi",
			"tokens_before":  10,
			"fallback":       true,
			"fallback_error": "provider 400: prompt is too long",
		},
	})

	components := a.conv.Snapshot().Components
	if len(components) != 1 {
		t.Fatalf("got %d components, want 1", len(components))
	}
	c := components[0]
	if !c.Fallback {
		t.Error("component not flagged as a fallback summary")
	}
	if !strings.Contains(c.Custom.Title, "Local fallback summary") {
		t.Errorf("title = %q, want the local fallback header", c.Custom.Title)
	}
	text := c.Text
	if !strings.Contains(text, "model call failed") {
		t.Errorf("summary text = %q, want the fallback marker", text)
	}
	if !strings.Contains(text, "prompt is too long") {
		t.Errorf("summary text = %q, want the fallback cause", text)
	}
	if !strings.Contains(text, "user: hi") {
		t.Errorf("summary text = %q, want the summary body preserved", text)
	}
}

// A fallback compaction renders on the fallback tint, not the regular
// compaction box, and the transcript export carries the fallback title.
func TestFallbackCompactionSummaryRendersAndExportsWithFallbackStyling(t *testing.T) {
	conv := NewConversationModel()
	conv.AddFallbackCompactionSummary("user: hi", "provider 400", 10_000)
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 60, H: 30})
	waitForChatLog(t, log)

	log.mu.Lock()
	rows := renderLineTexts(log.lines)
	styles := make([]tauui.Style, len(log.lines))
	for i, l := range log.lines {
		styles[i] = l.st
	}
	log.mu.Unlock()

	pal := defaultChatPalette()
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, compactionLabel) {
		t.Errorf("rendered fallback summary missing %q:\n%s", compactionLabel, joined)
	}
	if !strings.Contains(joined, "Local fallback summary") {
		t.Errorf("rendered fallback summary missing the fallback header:\n%s", joined)
	}
	var labelStyled, boxStyled bool
	for i, row := range rows {
		if strings.Contains(row, compactionLabel) {
			if styles[i] != pal.compactionFallbackLabel {
				t.Errorf("label row %q style = %#v, want the fallback label style %#v", row, styles[i], pal.compactionFallbackLabel)
			}
			labelStyled = true
		}
		if strings.Contains(row, "Local fallback summary") {
			if styles[i].BG != pal.compactionFallback.BG {
				t.Errorf("title row %q BG = %#v, want the fallback box BG %#v", row, styles[i].BG, pal.compactionFallback.BG)
			}
			boxStyled = true
		}
	}
	if !labelStyled || !boxStyled {
		t.Errorf("fallback rows not rendered (label=%v box=%v)", labelStyled, boxStyled)
	}

	out := FormatTranscriptMarkdown(conv.Snapshot(), TranscriptFormatOptions{})
	if !strings.Contains(out, "Local fallback summary") {
		t.Errorf("transcript export dropped the fallback title:\n%s", out)
	}
	if !strings.Contains(out, "model call failed") {
		t.Errorf("transcript export dropped the fallback cause:\n%s", out)
	}
}

// Compaction owns the same throbber lifecycle as the agent loader. Starting
// compaction must therefore animate even when no agent run is active, and its
// label should match the requested compact loader copy.
func TestCompactionIndicatorAdvancesWithSharedThrobber(t *testing.T) {
	a := newTestApp(t)
	a.status = tauui.NewStatusEntriesView(5)

	a.applySessionEvent(sdk.Event{Type: "compaction_start"})
	first := a.statusEntries[compactionStatusKey]
	if !strings.Contains(first, compactionRunningMessage) {
		t.Fatalf("compaction status = %q, want %q", first, compactionRunningMessage)
	}
	if strings.Contains(first, "context") {
		t.Fatalf("compaction status = %q, should use compact loader copy without extra context", first)
	}

	a.advanceThrobberFrame()
	second := a.statusEntries[compactionStatusKey]
	if second == first {
		t.Fatalf("compaction throbber did not advance: first=%q second=%q", first, second)
	}
	if !strings.Contains(second, compactionRunningMessage) {
		t.Fatalf("advanced compaction status = %q, want %q", second, compactionRunningMessage)
	}
}

// A session predating sdk.EventObserver must not stop the app from starting.
func TestWiringSessionEventsToleratesAnOlderSession(t *testing.T) {
	a := newTestApp(t)
	a.sess = &sessionNoFollowUp{}
	a.wireSessionEvents() // must not panic
}

// A failed compaction clears the indicator without committing a summary. A
// manual failure is reported by /compact itself; an automatic one is reported
// here, since nothing else will.
func TestCompactionEndReportsFailedAttempts(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    int
	}{
		{"manual abort", map[string]any{"reason": "manual", "aborted": true}, 0},
		{"manual error", map[string]any{"reason": "manual", "error_message": "boom"}, 0},
		{"automatic abort", map[string]any{"reason": "threshold", "aborted": true}, 1},
		{"automatic error", map[string]any{"reason": "threshold", "error_message": "boom"}, 1},
	}
	for _, tc := range cases {
		a := newTestApp(t)
		a.applySessionEvent(sdk.Event{Type: "compaction_start"})
		a.applySessionEvent(sdk.Event{Type: "compaction_end", Payload: tc.payload})
		_, running := a.statusEntries[compactionStatusKey]
		assert.False(t, running, tc.name)
		assert.False(t, a.compactionRunning, tc.name)
		assert.Len(t, a.conv.Snapshot().Components, tc.want, tc.name)
	}
}

func TestFailCompactBeforeCompactionEnd(t *testing.T) {
	for _, tc := range []struct {
		name             string
		startedIndicator bool
		switchSession    bool
		wantRunning      bool
	}{
		{name: "synthetic indicator clears", startedIndicator: true},
		{name: "running compaction keeps indicator", wantRunning: true},
		{name: "replaced session clears stale indicator", startedIndicator: true, switchSession: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			a.status = tauui.NewStatusEntriesView(5)
			sess := a.sess
			a.applySessionEvent(sdk.Event{Type: "compaction_start"})
			if tc.switchSession {
				a.sess = &fakeSession{}
				a.wireSessionEvents()
			}

			a.failCompact(sess, tc.startedIndicator, errors.New("boom"))

			_, shown := a.statusEntries[compactionStatusKey]
			assert.Equal(t, tc.wantRunning, shown)
			assert.Equal(t, tc.wantRunning, a.compactionRunning)
			assert.Equal(t, tc.wantRunning, a.programStatus.compacting)
			msgs := a.conv.Messages()
			if assert.NotEmpty(t, msgs) {
				assert.Equal(t, RoleError, msgs[len(msgs)-1].Role)
				assert.Contains(t, msgs[len(msgs)-1].Text, "compact: boom")
			}
		})
	}
}

func TestCompactionStatusPaintUsesActiveThemeWithoutChangingEditor(t *testing.T) {
	a := newEditorChromeApp(t)
	a.status = tauui.NewStatusEntriesView(5)
	a.footer.data.thinkingLevel = "max"
	a.applySessionEvent(sdk.Event{Type: "compaction_start"})
	defer a.applySessionEvent(sdk.Event{Type: "compaction_end", Payload: map[string]any{"aborted": true}})
	for _, values := range []map[string]string{
		{"muted": "#123456", "accent": "#abcdef", "thinkingMax": "#102030"},
		{"muted": "#654321", "accent": "#fedcba", "thinkingMax": "#304050"},
	} {
		a.applyThemeFromExtension(extensions.Theme{Name: "custom", Values: values, Dim: []string{"muted", "thinkingMax"}})
		tokens := a.chat.ThemeTokens()
		theme := tokens.nativeTheme()
		a.status.Layout(tauui.Rect{W: 40, H: 1})
		buf := tauui.NewBuffer(40, 1)
		painter := tauui.NewRenderPainter(buf, tauui.Rect{W: 40, H: 1}, theme.Base)
		a.status.Paint(tauui.NewDrawer(tauui.NewPainter(painter, theme.Base)), &tauui.Ctx{Theme: theme})
		assert.Equal(t, tokens.Accent, buf.At(0, 0).Style.FG)
		// The status value follows the key, separator, and one leading space.
		message := buf.At(len(compactionStatusKey)+3, 0).Style
		assert.Equal(t, tokens.Muted, message.FG)
		assert.NotZero(t, message.Attr&tauui.AttrDim)
		assert.Equal(t, tokens.Surface, message.BG)
		editor := paintedRuleStyle(t, a.editor)
		assert.Equal(t, tokens.ThinkingMax, editor.FG)
		assert.NotZero(t, editor.Attr&tauui.AttrDim)
	}
}
