package tui

import (
	"strings"
	"testing"
	"time"

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
