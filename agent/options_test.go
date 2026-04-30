package agent

import (
	"context"
	"testing"

	"github.com/losinggeneration/tau/ai"
)

func TestNewConfigDefaults(t *testing.T) {
	cfg := NewConfig()

	// Default thinking level is "off" per P7-AG06 to match pi-mono
	if cfg.ThinkingLevel != ThinkingOff {
		t.Fatalf("thinking level = %q, want %q", cfg.ThinkingLevel, ThinkingOff)
	}
	if cfg.ToolExecution != ToolExecutionParallel {
		t.Fatalf("tool execution = %q, want %q", cfg.ToolExecution, ToolExecutionParallel)
	}
	if cfg.SteeringMode != QueueModeOneAtATime {
		t.Fatalf("steering mode = %q, want %q", cfg.SteeringMode, QueueModeOneAtATime)
	}
	if cfg.FollowUpMode != QueueModeOneAtATime {
		t.Fatalf("follow-up mode = %q, want %q", cfg.FollowUpMode, QueueModeOneAtATime)
	}
	if cfg.Transport != ai.TransportSSE {
		t.Fatalf("transport = %q, want %q", cfg.Transport, ai.TransportSSE)
	}
}

func TestOptionsApplyExtendedConfig(t *testing.T) {
	convert := func(ctx context.Context, messages []*Message) ([]ai.Message, error) { return nil, nil }
	transform := func(ctx context.Context, messages []*Message) ([]*Message, error) { return messages, nil }
	before := func(ctx context.Context, toolCtx ToolCallContext) (BeforeToolCallResult, error) {
		return BeforeToolCallResult{Block: true}, nil
	}
	after := func(ctx context.Context, toolCtx ToolCallContext, result *ToolResult) (AfterToolCallResult, error) {
		return AfterToolCallResult{}, nil
	}
	apiKeyProvider := func(ctx context.Context, provider string) (string, error) { return "key", nil }
	clientFactory := func(model *ai.Model) (ai.Client, error) { return nil, nil }
	handler := func(event Event) {}
	payload := func(v any) any { return v }
	response := func(ai.ProviderResponse) {}
	budgets := &ai.ThinkingBudgets{}

	cfg := NewConfig(
		WithModel(&ai.Model{ID: "test"}),
		WithSystemPrompt("system"),
		WithTools(Tool{Name: "read"}),
		WithThinkingLevel(ThinkingHigh),
		WithStreamHandler(handler),
		WithAPIKeyProvider(apiKeyProvider),
		WithClientFactory(clientFactory),
		WithConvertToLlm(convert),
		WithTransformContext(transform),
		WithBeforeToolCall(before),
		WithAfterToolCall(after),
		WithPayloadCallback(payload),
		WithResponseCallback(response),
		WithToolExecution(ToolExecutionSequential),
		WithSteeringMode(QueueModeAll),
		WithFollowUpMode(QueueModeAll),
		WithSessionID("session-123"),
		WithThinkingBudgets(budgets),
		WithTransport(ai.TransportWS),
		WithMaxRetryDelayMs(1500),
	)

	if cfg.Model == nil || cfg.Model.ID != "test" {
		t.Fatalf("model = %+v", cfg.Model)
	}
	if cfg.SystemPrompt != "system" {
		t.Fatalf("system prompt = %q, want system", cfg.SystemPrompt)
	}
	if len(cfg.Tools) != 1 || cfg.Tools[0].Name != "read" {
		t.Fatalf("tools = %+v", cfg.Tools)
	}
	if cfg.ThinkingLevel != ThinkingHigh {
		t.Fatalf("thinking level = %q, want %q", cfg.ThinkingLevel, ThinkingHigh)
	}
	if cfg.StreamHandler == nil || cfg.APIKeyProvider == nil || cfg.ClientFactory == nil {
		t.Fatal("expected stream handler, API key provider, and client factory to be set")
	}
	if cfg.ConvertToLlm == nil || cfg.TransformContext == nil {
		t.Fatal("expected conversion hooks to be set")
	}
	if cfg.BeforeToolCall == nil || cfg.AfterToolCall == nil {
		t.Fatal("expected tool hooks to be set")
	}
	if cfg.OnPayload == nil || cfg.OnResponse == nil {
		t.Fatal("expected provider callbacks to be set")
	}
	if cfg.ToolExecution != ToolExecutionSequential {
		t.Fatalf("tool execution = %q, want %q", cfg.ToolExecution, ToolExecutionSequential)
	}
	if cfg.SteeringMode != QueueModeAll || cfg.FollowUpMode != QueueModeAll {
		t.Fatalf("queue modes = %q/%q, want %q/%q", cfg.SteeringMode, cfg.FollowUpMode, QueueModeAll, QueueModeAll)
	}
	if cfg.SessionID != "session-123" {
		t.Fatalf("session id = %q, want session-123", cfg.SessionID)
	}
	if cfg.ThinkingBudgets != budgets {
		t.Fatal("thinking budgets pointer was not preserved")
	}
	if cfg.Transport != ai.TransportWS {
		t.Fatalf("transport = %q, want %q", cfg.Transport, ai.TransportWS)
	}
	if cfg.MaxRetryDelayMs != 1500 {
		t.Fatalf("max retry delay = %d, want 1500", cfg.MaxRetryDelayMs)
	}
}
