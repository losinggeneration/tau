package tui

import (
	"strings"
	"testing"

	sdk "github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
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
		name       string
		payload    any
		wantText   string
		wantTokens int
	}{
		{"native ints", map[string]any{"summary": "s", "tokens_before": 42}, "s", 42},
		{"json floats", map[string]any{"summary": "s", "tokens_before": float64(42)}, "s", 42},
		{"int64", map[string]any{"summary": "s", "tokens_before": int64(42)}, "s", 42},
		{"missing count", map[string]any{"summary": "s"}, "s", 0},
		{"missing summary", map[string]any{"tokens_before": 42}, "", 42},
		{"not a map", "nonsense", "", 0},
		{"nil", nil, "", 0},
	}
	for _, tc := range cases {
		summary, tokens := compactionPayload(tc.payload)
		if summary != tc.wantText || tokens != tc.wantTokens {
			t.Errorf("%s: got (%q, %d), want (%q, %d)", tc.name, summary, tokens, tc.wantText, tc.wantTokens)
		}
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
