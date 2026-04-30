package ai

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestFauxProviderSupportsMultipleModels(t *testing.T) {
	p := RegisterFauxProvider(WithFauxModels([]FauxModelDefinition{
		{ID: "faux-fast", Name: "Faux Fast", Reasoning: false},
		{ID: "faux-thinker", Name: "Faux Thinker", Reasoning: true},
	}))
	defer p.Unregister()
	p.SetResponseSteps([]FauxResponseStep{
		func(_ *Context, _ []Option, _ *fauxState, model *Model) (AssistantMessage, error) {
			return FauxAssistantMessage(model.ID), nil
		},
		func(_ *Context, _ []Option, _ *fauxState, model *Model) (AssistantMessage, error) {
			return FauxAssistantMessage(model.ID), nil
		},
	})

	if got := len(p.Models()); got != 2 {
		t.Fatalf("len(Models()) = %d, want 2", got)
	}
	if p.GetModel("faux-fast") == nil || p.GetModel("faux-thinker") == nil {
		t.Fatal("GetModel returned nil for registered faux models")
	}

	ctx := &Context{Messages: []Message{{Role: "user", Content: "hi", Timestamp: 1}}}

	fast, err := Complete(context.Background(), p.GetModel("faux-fast"), ctx)
	if err != nil {
		t.Fatalf("Complete fast: %v", err)
	}
	thinker, err := Complete(context.Background(), p.GetModel("faux-thinker"), ctx)
	if err != nil {
		t.Fatalf("Complete thinker: %v", err)
	}

	if fast.Content[0].Text != "faux-fast" {
		t.Fatalf("fast content = %q, want faux-fast", fast.Content[0].Text)
	}
	if thinker.Content[0].Text != "faux-thinker" {
		t.Fatalf("thinker content = %q, want faux-thinker", thinker.Content[0].Text)
	}
}

func TestFauxProviderEstimatesUsageAndCaching(t *testing.T) {
	p := RegisterFauxProvider()
	defer p.Unregister()
	p.SetResponses([]AssistantMessage{
		FauxAssistantMessage("first"),
		FauxAssistantMessage("second"),
	})

	ctx := &Context{
		SystemPrompt: "Be concise.",
		Messages:     []Message{{Role: "user", Content: "hello", Timestamp: 1}},
	}

	first, err := Complete(context.Background(), p.model, ctx, WithSessionID("session-1"), WithCacheRetention(string(CacheRetentionShort)))
	if err != nil {
		t.Fatalf("Complete first: %v", err)
	}
	if first.Usage.Input == 0 || first.Usage.Output == 0 {
		t.Fatalf("first usage = %+v, want non-zero input/output", first.Usage)
	}
	if first.Usage.CacheWrite == 0 {
		t.Fatalf("first cacheWrite = %d, want > 0", first.Usage.CacheWrite)
	}

	ctx.Messages = append(ctx.Messages,
		Message{Role: "assistant", Content: first.Content, Timestamp: first.Timestamp},
		Message{Role: "user", Content: "follow up", Timestamp: 2},
	)

	second, err := Complete(context.Background(), p.model, ctx, WithSessionID("session-1"), WithCacheRetention(string(CacheRetentionShort)))
	if err != nil {
		t.Fatalf("Complete second: %v", err)
	}
	if second.Usage.CacheRead == 0 {
		t.Fatalf("second cacheRead = %d, want > 0", second.Usage.CacheRead)
	}
}

func TestFauxProviderAbortsMidStream(t *testing.T) {
	p := RegisterFauxProvider(WithFauxTokensPerSecond(100), WithFauxTokenSize(3, 3))
	defer p.Unregister()
	p.SetResponses([]AssistantMessage{
		FauxAssistantMessage("abcdefghijklmnopqrstuvwxyz"),
	})

	ctx, cancel := context.WithCancel(context.Background())
	var (
		textDeltaCount int
		events         []StreamEvent
	)
	err := Stream(ctx, p.model, &Context{Messages: []Message{{Role: "user", Content: "hi", Timestamp: 1}}}, func(_ context.Context, event StreamEvent) {
		events = append(events, event)
		if _, ok := event.(TextDelta); ok {
			textDeltaCount++
			cancel()
		}
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if textDeltaCount == 0 {
		t.Fatal("expected at least one text delta before abort")
	}

	last, ok := events[len(events)-1].(StreamError)
	if !ok {
		t.Fatalf("last event type = %T, want StreamError", events[len(events)-1])
	}
	if last.Msg.StopReason != StopReasonAborted {
		t.Fatalf("stopReason = %q, want %q", last.Msg.StopReason, StopReasonAborted)
	}
}

func TestFauxProviderToolCallEndIncludesArguments(t *testing.T) {
	p := RegisterFauxProvider(WithFauxTokenSize(1, 1))
	defer p.Unregister()
	p.SetResponses([]AssistantMessage{
		FauxAssistantMessage([]ContentBlock{
			FauxToolCall("echo", json.RawMessage(`{"text":"hi","count":12}`), WithFauxToolCallID("tool-1")),
		}, WithFauxStopReason(StopReasonToolUse)),
	})

	var toolEnd ToolCallEnd
	err := Stream(context.Background(), p.model, &Context{Messages: []Message{{Role: "user", Content: "hi", Timestamp: 1}}}, func(_ context.Context, event StreamEvent) {
		if e, ok := event.(ToolCallEnd); ok {
			toolEnd = e
		}
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if string(toolEnd.ToolCall.Arguments) != `{"count":12,"text":"hi"}` && string(toolEnd.ToolCall.Arguments) != `{"text":"hi","count":12}` {
		t.Fatalf("toolEnd.ToolCall.Arguments = %s", string(toolEnd.ToolCall.Arguments))
	}
}

func TestRegisterFauxProviderCanUnregister(t *testing.T) {
	p := RegisterFauxProvider(WithFauxAPI("faux:test"))
	if _, ok := Get("faux:test"); !ok {
		t.Fatal("provider not registered")
	}
	p.Unregister()
	if _, ok := Get("faux:test"); ok {
		t.Fatal("provider still registered after Unregister")
	}
}

func TestFauxProviderFactoryErrorBecomesTerminalError(t *testing.T) {
	p := RegisterFauxProvider()
	defer p.Unregister()
	p.SetResponseSteps([]FauxResponseStep{
		func(_ *Context, _ []Option, _ *fauxState, _ *Model) (AssistantMessage, error) {
			return AssistantMessage{}, errors.New("boom")
		},
	})

	var streamErr StreamError
	err := Stream(context.Background(), p.model, &Context{Messages: []Message{{Role: "user", Content: "hi", Timestamp: 1}}}, func(_ context.Context, event StreamEvent) {
		if e, ok := event.(StreamError); ok {
			streamErr = e
		}
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if streamErr.Msg.ErrorMessage != "boom" {
		t.Fatalf("errorMessage = %q, want boom", streamErr.Msg.ErrorMessage)
	}
}

func TestFauxProviderRespectsPacing(t *testing.T) {
	p := RegisterFauxProvider(WithFauxTokensPerSecond(1000), WithFauxTokenSize(3, 3))
	defer p.Unregister()
	p.SetResponses([]AssistantMessage{FauxAssistantMessage("abcdefghijkl")})

	start := time.Now()
	err := Stream(context.Background(), p.model, &Context{Messages: []Message{{Role: "user", Content: "hi", Timestamp: 1}}}, func(_ context.Context, event StreamEvent) {
		_ = event
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if time.Since(start) <= 0 {
		t.Fatal("expected non-zero streaming duration")
	}
}
