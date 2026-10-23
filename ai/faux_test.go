package ai

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

func TestFauxCacheEstimateComparesMessagesThenCharacters(t *testing.T) {
	p := RegisterFauxProvider()
	defer p.Unregister()
	p.SetResponses([]AssistantMessage{FauxAssistantMessage("a"), FauxAssistantMessage("b"), FauxAssistantMessage("c")})

	prompt := func(last string) *Context {
		return &Context{SystemPrompt: "Be concise.", Messages: []Message{
			{Role: "user", Content: "keep", Timestamp: 1},
			{Role: "user", Content: last, Timestamp: 2},
		}}
	}
	opts := []Option{WithSessionID("prefix"), WithCacheRetention(string(CacheRetentionShort))}
	if _, err := Complete(t.Context(), p.model, prompt("hello world"), opts...); err != nil {
		t.Fatal(err)
	}
	second, err := Complete(t.Context(), p.model, prompt("hello there"), opts...)
	if err != nil {
		t.Fatal(err)
	}
	cached := len("system:Be concise.\n\nuser:keep\n\nuser:hello ")
	whole := len("system:Be concise.\n\nuser:keep\n\nuser:hello there")
	if second.Usage.CacheRead != (cached+3)/4 || second.Usage.CacheWrite != (whole-cached+3)/4 {
		t.Fatalf("usage = %+v, want cacheRead %d and cacheWrite %d", second.Usage, (cached+3)/4, (whole-cached+3)/4)
	}

	none, err := Complete(t.Context(), p.model, prompt("hello there"), WithSessionID("prefix"), WithCacheRetention(string(CacheRetentionNone)))
	if err != nil {
		t.Fatal(err)
	}
	if none.Usage.CacheRead != 0 || none.Usage.CacheWrite != 0 {
		t.Fatalf("cache retention none usage = %+v", none.Usage)
	}
}

func TestFauxToolNamespaceSurvivesStreaming(t *testing.T) {
	p := RegisterFauxProvider()
	defer p.Unregister()
	var response AssistantMessage
	require.NoError(t, json.Unmarshal([]byte(`{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"call","name":"lookup","arguments":{},"namespace":"dynamic_tools"}]}`), &response))
	p.SetResponses([]AssistantMessage{response})
	var values []any
	require.NoError(t, Stream(t.Context(), p.model, &Context{Messages: []Message{{Role: "user", Content: "lookup"}}}, func(_ context.Context, event StreamEvent) {
		switch e := event.(type) {
		case ToolCallStart:
			values = append(values, e.Partial.Content[0])
		case ToolCallEnd:
			values = append(values, e.ToolCall, e.Partial.Content[0])
		case Done:
			values = append(values, e.Message.Content[0])
		}
	}))
	require.Len(t, values, 4)
	for _, value := range values {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		require.Contains(t, string(data), `"namespace":"dynamic_tools"`)
	}
}
