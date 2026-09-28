package ai

import (
	"fmt"
	"testing"
	"time"
)

func TestUsageLimitResetAtCodex(t *testing.T) {
	resetsAt := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	err := &CodexUsageLimitError{StatusCode: 429, ResetsAt: resetsAt, ResetsIn: 2 * time.Hour}

	got, ok := UsageLimitResetAt(err)
	if !ok {
		t.Fatal("expected ok for codex usage limit error")
	}
	if !got.Equal(resetsAt) {
		t.Fatalf("resetAt = %v, want %v", got, resetsAt)
	}
}

func TestUsageLimitResetAtCodexResetsInOnly(t *testing.T) {
	err := &CodexUsageLimitError{StatusCode: 429, ResetsIn: time.Minute}

	got, ok := UsageLimitResetAt(err)
	if !ok {
		t.Fatal("expected ok when only ResetsIn is set")
	}
	if diff := time.Until(got); diff < 59*time.Second || diff > time.Minute+2*time.Second {
		t.Fatalf("resetAt = %v, want ~now+%v", got, time.Minute)
	}
}

func TestUsageLimitResetAtCodexNoResetInfo(t *testing.T) {
	err := &CodexUsageLimitError{StatusCode: 429}

	if _, ok := UsageLimitResetAt(err); ok {
		t.Fatal("expected !ok when neither ResetsAt nor ResetsIn is set")
	}
}

func TestUsageLimitResetAtZAI(t *testing.T) {
	resetsAt := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	err := &ZAIUsageLimitError{StatusCode: 429, ResetsAt: resetsAt}

	got, ok := UsageLimitResetAt(err)
	if !ok {
		t.Fatal("expected ok for z.ai usage limit error")
	}
	if !got.Equal(resetsAt) {
		t.Fatalf("resetAt = %v, want %v", got, resetsAt)
	}
}

func TestUsageLimitResetAtWrappedError(t *testing.T) {
	resetsAt := time.Now().Add(time.Hour)
	wrapped := fmt.Errorf("openai codex: request: %w", &CodexUsageLimitError{StatusCode: 429, ResetsAt: resetsAt})

	got, ok := UsageLimitResetAt(wrapped)
	if !ok {
		t.Fatal("expected ok for wrapped usage limit error")
	}
	if !got.Equal(resetsAt) {
		t.Fatalf("resetAt = %v, want %v", got, resetsAt)
	}
}

func TestUsageLimitResetAtOtherErrors(t *testing.T) {
	for _, err := range []error{
		nil,
		fmt.Errorf("provider failed"),
		&CodexUsageLimitError{StatusCode: 429},
	} {
		if _, ok := UsageLimitResetAt(err); ok {
			t.Fatalf("expected !ok for %v", err)
		}
	}
}
