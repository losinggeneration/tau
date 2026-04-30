package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/ai"
	tauui "github.com/losinggeneration/tau/tui"
)

func renderedThinkingLines(t *testing.T, configure func(*ChatLog), build func(*ConversationModel)) []string {
	t.Helper()
	conv := NewConversationModel()
	build(conv)
	log := newTestChatLog(t, conv)
	if configure != nil {
		configure(log)
	}
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 60, H: 100})
	waitForChatLog(t, log)
	log.mu.Lock()
	defer log.mu.Unlock()
	return renderLineTexts(log.lines)
}

func TestThinkingUsesConfiguredHiddenLabel(t *testing.T) {
	lines := renderedThinkingLines(
		t,
		func(log *ChatLog) { log.SetThinkingDisplay(false, "Pondering…") },
		func(conv *ConversationModel) { conv.AddHistoricalThinking("t1", "") },
	)
	if countLinesContaining(lines, "Pondering…") == 0 {
		t.Fatalf("configured hidden-thinking label not used: %#v", lines)
	}
	if countLinesContaining(lines, defaultHiddenThinkingLabel) != 0 {
		t.Fatalf("default label leaked past the override: %#v", lines)
	}
}

func TestThinkingFallsBackToDefaultLabel(t *testing.T) {
	lines := renderedThinkingLines(
		t,
		func(log *ChatLog) { log.SetThinkingDisplay(false, "   ") },
		func(conv *ConversationModel) { conv.AddHistoricalThinking("t1", "") },
	)
	if countLinesContaining(lines, defaultHiddenThinkingLabel) == 0 {
		t.Fatalf("blank label should fall back to %q: %#v", defaultHiddenThinkingLabel, lines)
	}
}

// hideThinkingBlock removes the block entirely rather than replacing it with a
// placeholder — a user who turned it off does not want a row about it.
func TestHideThinkingBlockDropsTheComponent(t *testing.T) {
	// Restored history collapses thinking to the placeholder, so the baseline
	// to remove is that row rather than the reasoning text.
	build := func(conv *ConversationModel) {
		conv.AddHistoricalThinking("t1", "secret reasoning")
		conv.AddAssistantMessage("visible answer")
	}

	shown := renderedThinkingLines(t, nil, build)
	if countLinesContaining(shown, defaultHiddenThinkingLabel) == 0 {
		t.Fatalf("baseline should render a thinking row: %#v", shown)
	}

	hidden := renderedThinkingLines(
		t,
		func(log *ChatLog) { log.SetThinkingDisplay(true, "") },
		build,
	)
	if countLinesContaining(hidden, "secret reasoning") != 0 {
		t.Fatalf("hideThinkingBlock left thinking text in the log: %#v", hidden)
	}
	if countLinesContaining(hidden, defaultHiddenThinkingLabel) != 0 {
		t.Fatalf("hideThinkingBlock left a placeholder row: %#v", hidden)
	}
	if countLinesContaining(hidden, "visible answer") == 0 {
		t.Fatalf("hideThinkingBlock removed more than the thinking block: %#v", hidden)
	}
	if len(hidden) >= len(shown) {
		t.Fatalf("hideThinkingBlock did not shrink the log: %d rows vs %d", len(hidden), len(shown))
	}
}

// Live streaming thinking still renders its text, so the markdown path is
// exercised and hiding still removes it.
func TestStreamingThinkingRendersItsText(t *testing.T) {
	build := func(conv *ConversationModel) {
		conv.AddMessage(RoleThinking, "considering **options**")
	}

	shown := renderedThinkingLines(t, nil, build)
	if countLinesContaining(shown, "considering") == 0 {
		t.Fatalf("thinking text should render: %#v", shown)
	}

	hidden := renderedThinkingLines(
		t,
		func(log *ChatLog) { log.SetThinkingDisplay(true, "") },
		build,
	)
	if countLinesContaining(hidden, "considering") != 0 {
		t.Fatalf("hideThinkingBlock left streaming thinking text: %#v", hidden)
	}
}

// A thinking-display change has to drop the render cache, or the log keeps
// painting the pre-change rows.
//
// Asserting on the *output* rather than on len(renderCache): SetThinkingDisplay
// clears the cache and then schedules a re-render, so a test that reads the map
// straight afterwards races that goroutine and fails whenever the render lands
// first. Stale rows are the failure this guards against anyway.
func TestSetThinkingDisplayInvalidatesRenderCache(t *testing.T) {
	conv := NewConversationModel()
	conv.AddHistoricalThinking("t1", "reasoning")
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 20})
	waitForChatLog(t, log)

	// The painted rows, not Render(): Render re-renders from the conversation
	// and so never consults the cache, which is exactly what the cache would
	// serve stale.
	paintedRows := func() string {
		log.mu.Lock()
		defer log.mu.Unlock()
		return strings.Join(renderLineTexts(log.lines), "\n")
	}

	if before := paintedRows(); !strings.Contains(before, defaultHiddenThinkingLabel) {
		t.Fatalf("setup: block should carry the default label:\n%s", before)
	}

	log.SetThinkingDisplay(false, "Pondering…")
	waitForChatLog(t, log)

	after := paintedRows()
	if strings.Contains(after, defaultHiddenThinkingLabel) || !strings.Contains(after, "Pondering…") {
		t.Errorf("painted rows kept the old label; the cache was not dropped:\n%s", after)
	}
}

// The transcript export and the live log must agree on the placeholder.
func TestTranscriptUsesSharedHiddenThinkingLabel(t *testing.T) {
	conv := NewConversationModel()
	conv.AddHistoricalThinking("t1", "")
	got := FormatTranscriptMarkdown(conv.Snapshot(), TranscriptFormatOptions{IncludeThinking: true})
	if !strings.Contains(got, defaultHiddenThinkingLabel) {
		t.Fatalf("transcript does not use the shared label %q:\n%s", defaultHiddenThinkingLabel, got)
	}
	if strings.Contains(got, "Thinking hidden") {
		t.Fatalf("transcript still carries the old hardcoded label:\n%s", got)
	}
}

// These settings parse into ai.Settings but each has to be handed to a specific
// widget; without that they silently do nothing.
func TestApplyDisplaySettingsReachesComponents(t *testing.T) {
	a := newTestApp(t)
	a.editor = tauui.NewPromptEditor()

	a.applyDisplaySettings(&ai.Settings{
		HideThinkingBlock:      true,
		EditorPaddingX:         4,
		AutocompleteMaxVisible: 7,
	})

	a.chat.mu.Lock()
	hidden := a.chat.hideThinkingBlock
	a.chat.mu.Unlock()
	if !hidden {
		t.Fatalf("hideThinkingBlock did not reach the chat log")
	}
	if got := a.editor.Padding(); got != 4 {
		t.Fatalf("editor padding = %d, want the configured 4", got)
	}
	if got := a.editor.Model().AutocompleteMaxVisible(); got != 7 {
		t.Fatalf("autocomplete max visible = %d, want the configured 7", got)
	}
}

// Zero values must not clobber component defaults.
func TestApplyDisplaySettingsIgnoresUnsetValues(t *testing.T) {
	a := newTestApp(t)
	a.editor = tauui.NewPromptEditor()
	defaultPadding := a.editor.Padding()

	a.applyDisplaySettings(&ai.Settings{})

	if got := a.editor.Padding(); got != defaultPadding {
		t.Fatalf("editor padding = %d after empty settings, want the default %d", got, defaultPadding)
	}
}

// End-to-end: a real settings.json must drive the components.
func TestNewAppliesDisplaySettingsFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body, err := json.Marshal(map[string]any{
		"theme":             "tau",
		"hideThinkingBlock": true,
		"editorPaddingX":    3,
	})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	settings, err := ai.LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if !settings.HideThinkingBlock || settings.EditorPaddingX != 3 {
		t.Fatalf("settings did not round-trip: %+v", settings)
	}

	a := newTestApp(t)
	a.editor = tauui.NewPromptEditor()
	a.applyDisplaySettings(settings)

	if got := a.editor.Padding(); got != 3 {
		t.Fatalf("editorPaddingX from file = %d, want 3", got)
	}
}
