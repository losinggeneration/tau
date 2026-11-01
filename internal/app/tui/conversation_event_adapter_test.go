package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/keybindings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
)

func TestUsageLimitWaitShowsErrorRowAndCountdown(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{
		"role":          "assistant",
		"content":       "",
		"error_message": "openai codex: status 429: The usage limit has been reached. Available again in 2h20m18s",
	}})
	until := time.Now().Add(2*time.Hour + 20*time.Minute).Round(time.Second)
	a.handleEvent(sdk.Event{Type: "usage_limit_wait", Payload: map[string]any{
		"provider":  "openai",
		"resets_at": until.Format(time.RFC3339),
	}})

	var errRows int
	for _, msg := range a.conv.Messages() {
		if msg.IsError && strings.Contains(msg.Text, "status 429") {
			errRows++
		}
	}
	if errRows != 1 {
		t.Fatalf("error rows containing 429 text = %d, want 1: %#v", errRows, a.conv.Messages())
	}

	got := a.workingMessage()
	if !strings.HasPrefix(got, "openai usage limit, retrying at ") || !strings.Contains(got, "(in 2h20m") {
		t.Fatalf("workingMessage = %q, want usage-limit countdown", got)
	}

	// The retry's first streamed message restores the ordinary run message.
	a.handleEvent(sdk.Event{Type: "message_start", Payload: map[string]any{"role": "assistant"}})
	if got := a.workingMessage(); got != defaultWorkingMessage {
		t.Fatalf("workingMessage after retry = %q, want %q", got, defaultWorkingMessage)
	}
}

func TestUsageLimitWaitErrorEventDoesNotDuplicateRow(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{
		"role":          "assistant",
		"content":       "",
		"error_message": "provider failed",
	}})
	a.handleEvent(sdk.Event{Type: "error", Payload: map[string]any{"message": "provider failed"}})

	var errRows int
	for _, msg := range a.conv.Messages() {
		if msg.IsError && msg.Text == "provider failed" {
			errRows++
		}
	}
	if errRows != 1 {
		t.Fatalf("error rows = %d, want 1 (message_end and error event carry the same text): %#v", errRows, a.conv.Messages())
	}
}

func TestUsageLimitWaitCountdownAfterResetShowsRetryingNow(t *testing.T) {
	a := newTestApp(t)

	a.setUsageLimitWait("openai", time.Now().Add(-time.Second))
	if got := a.workingMessage(); got != "openai usage limit, retrying now" {
		t.Fatalf("workingMessage = %q, want retrying now", got)
	}

	a.clearUsageLimitWait()
	if got := a.workingMessage(); got != defaultWorkingMessage {
		t.Fatalf("workingMessage after clear = %q, want %q", got, defaultWorkingMessage)
	}
}

func TestToolResultPreservesExecutionDuration(t *testing.T) {
	for _, wire := range []any{12.5, new(12.5)} {
		app := newTestApp(t)
		app.handleEvent(sdk.Event{Type: "tool_call", Payload: map[string]any{"tool_call_id": "call", "name": "bash", "input": `{"command":"pwd"}`}})
		app.handleEvent(sdk.Event{Type: "tool_result", Payload: map[string]any{"tool_call_id": "call", "output": "ok", "durationMs": wire}})
		snapshot := app.conv.Snapshot()
		if len(snapshot.Components) != 1 || snapshot.Components[0].ToolExecution.DurationMs == nil || *snapshot.Components[0].ToolExecution.DurationMs != 12.5 {
			t.Fatalf("duration lost: %+v", snapshot)
		}
	}
}

func TestToolResultWithoutExecutionOmitsDuration(t *testing.T) {
	app := newTestApp(t)
	app.handleEvent(sdk.Event{Type: "tool_call", Payload: map[string]any{"tool_call_id": "blocked", "name": "bash"}})
	app.handleEvent(sdk.Event{Type: "tool_result", Payload: map[string]any{"tool_call_id": "blocked", "output": "blocked", "is_error": true, "durationMs": (*float64)(nil)}})
	if got := app.conv.Snapshot().Components[0].ToolExecution.DurationMs; got != nil {
		t.Fatalf("fabricated duration: %v", *got)
	}
}

func TestExcludedShellHeaderSurvivesProgressAndResult(t *testing.T) {
	app := newTestApp(t)
	app.handleEvent(sdk.Event{Type: "tool_call", Payload: map[string]any{"tool_call_id": "call", "name": "bash", "input": `{"command":"pwd","exclude_from_context":true}`}})
	check := func() {
		t.Helper()
		if !app.conv.Snapshot().Components[0].ToolExecution.ExcludeFromContext {
			t.Fatal("excluded shell lost its dim-header flag")
		}
	}
	check()
	app.handleEvent(sdk.Event{Type: "tool_progress", Payload: map[string]any{"tool_call_id": "call", "chunk": "working"}})
	check()
	app.handleEvent(sdk.Event{Type: "tool_result", Payload: map[string]any{"tool_call_id": "call", "output": "ok", "excludeFromContext": true}})
	check()
}

func TestLiveRichToolResultMatchesRestoration(t *testing.T) {
	app := newTestApp(t)
	blocks := []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "first"}, {Type: ai.ContentBlockImage, MimeType: "image/png", Data: "aW1hZ2U="}, {Type: ai.ContentBlockText, Text: "second"}}
	structured := json.RawMessage(`{"answer":42}`)
	records := []ai.NestedToolCall{{ID: "child"}}
	usage := &ai.Usage{Input: 4}
	app.handleEvent(sdk.Event{Type: "tool_call", Payload: map[string]any{"tool_call_id": "call", "name": "lookup"}})
	app.handleEvent(sdk.Event{Type: "tool_result", Payload: map[string]any{"tool_call_id": "call", "content": blocks, "structuredContent": structured, "nestedCalls": records, "toolUsage": usage}})
	tool := app.conv.Snapshot().Components[0].ToolExecution
	if len(tool.Content) != 3 || string(tool.StructuredContent) != string(structured) || len(tool.NestedCalls) != 1 || tool.ToolUsage == nil {
		t.Fatalf("lost rich live result: %+v", tool)
	}
	images := app.conv.Snapshot().Components[0].Images
	if tool.Output != "first\nsecond" || len(images) != 1 || images[0].Data != "aW1hZ2U=" || images[0].MIMEType != "image/png" {
		t.Fatalf("lost item boundaries: %q %+v", tool.Output, images)
	}
}

// Live items carry the session entry that recorded them: the user row its own
// entry, an assistant message's text and the tool executions it requested the
// assistant entry. That is what lets the model drop history at a message.
func TestMessageEndTagsItemsWithSessionEntry(t *testing.T) {
	a := newTestApp(t)
	a.footer = NewFooter(&footerData{})

	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "user", "content": "go", "entry_id": "eU"}})
	a.handleEvent(sdk.Event{Type: "message_start", Payload: map[string]any{"role": "assistant", "message_id": "m1"}})
	a.handleEvent(sdk.Event{Type: "text_delta", Payload: map[string]any{"delta": "running it", "message_id": "m1"}})
	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "assistant", "content": "running it", "message_id": "m1", "entry_id": "eA"}})
	a.handleEvent(sdk.Event{Type: "tool_call", Payload: map[string]any{"tool_call_id": "c1", "name": "bash", "input": `{"command":"ls"}`}})
	a.handleEvent(sdk.Event{Type: "tool_result", Payload: map[string]any{"tool_call_id": "c1", "output": "ok"}})
	// A tool call that starts before its message finishes cannot be attributed.
	a.handleEvent(sdk.Event{Type: "message_start", Payload: map[string]any{"role": "assistant", "message_id": "m2"}})
	a.handleEvent(sdk.Event{Type: "tool_call", Payload: map[string]any{"tool_call_id": "c2", "name": "bash", "input": `{"command":"pwd"}`}})

	var got []string
	for _, item := range a.conv.committed {
		got = append(got, fmt.Sprintf("%d=%s", item.kind, item.entryID))
	}
	want := []string{
		fmt.Sprintf("%d=eU", conversationItemUser),
		fmt.Sprintf("%d=eA", conversationItemAssistant),
		fmt.Sprintf("%d=eA", conversationItemToolExecution),
		fmt.Sprintf("%d=", conversationItemToolExecution),
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("tags = %v, want %v", got, want)
	}
}

func TestSuccessfulReloadUpdatesLiveKeybindings(t *testing.T) {
	original := keybindings.DefaultManager()
	t.Cleanup(func() { keybindings.SetDefaultManager(original) })
	dir := t.TempDir()
	path := filepath.Join(dir, "keybindings.json")
	write := func(body string) { t.Helper(); require.NoError(t, os.WriteFile(path, []byte(body), 0o600)) }
	write(`{"app.model.cycleForward":"ctrl+g","tui.select.cancel":"ctrl+q"}`)
	app, err := New(Options{Session: &fakeSession{}, SettingsPath: filepath.Join(dir, "settings.json")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, app.chat.Close()) })
	km := appKeymap{app: app, manager: app.keybindings}
	write(`{"cycleModelForward":"ctrl+j","selectCancel":"ctrl+l"}`)
	app.conversationEvents.handle(sdk.Event{Type: "command_result", Payload: map[string]any{"command": "reload", "is_error": true, "output": "rejected"}})
	act, ok := km.Resolve(tauui.KeyCtxTextInput, ctrl('g'))
	require.True(t, ok)
	assert.Equal(t, AppActionModelCycleForward, act)
	assert.True(t, keybindings.DefaultManager().Matches(ctrl('q'), "tui.select.cancel"))
	app.conversationEvents.handle(sdk.Event{Type: "command_result", Payload: map[string]any{"command": "reload"}})
	act, ok = km.Resolve(tauui.KeyCtxTextInput, ctrl('j'))
	assert.True(t, ok)
	assert.Equal(t, AppActionModelCycleForward, act)
	act, ok = km.Resolve(tauui.KeyCtxTextInput, ctrl('g'))
	assert.False(t, ok && act == AppActionModelCycleForward)
	assert.Contains(t, strings.Join(app.keybindingHints(), "\n"), "ctrl+j")
	cmdSettingsOverlay(app, "")
	app.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'q', Mod: tauui.ModCtrl}, stubCtx())
	assert.True(t, app.dialogPicker.Shown())
	app.dialogPicker.Handle(tauui.KeyEvent{Key: tauui.KeyRune, Rune: 'l', Mod: tauui.ModCtrl}, stubCtx())
	assert.False(t, app.dialogPicker.Shown())
	for _, invalid := range []bool{true, false} {
		if invalid {
			write(`{`)
		} else {
			require.NoError(t, os.Remove(path))
		}
		app.conversationEvents.handle(sdk.Event{Type: "command_result", Payload: map[string]any{"command": "reload"}})
		act, ok = km.Resolve(tauui.KeyCtxTextInput, ctrl('p'))
		assert.True(t, ok)
		assert.Equal(t, AppActionModelCycleForward, act)
		assert.True(t, keybindings.DefaultManager().Matches(tauui.Keystroke{Key: tauui.KeyEsc}, "tui.select.cancel"))
	}
	assert.Contains(t, fmt.Sprint(app.conv.Messages()), "keybindings: using defaults")
}
