package tui

import (
	"encoding/json"
	"testing"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/tui/extensions"
)

func TestResolverRendersUnregisteredHistoricalTool(t *testing.T) {
	registry := extensions.New()
	registry.RegisterToolRendererResolver(func(name string, next func() extensions.Renderer) extensions.Renderer {
		if name != "removed_tool" {
			return next()
		}
		return func(payload any) []string {
			tool := payload.(ToolExecutionComponent)
			if tool.DurationMs == nil || *tool.DurationMs != 12.5 {
				t.Fatal("resolver lost recorded timing")
			}
			return []string{"historical output"}
		}
	})
	renderers := newPluginRenderers()
	renderers.ApplyExtensionsRegistry(registry)
	override := renderers.toolOverrides(ToolExecutionComponent{Name: "removed_tool", State: toolExecutionComplete, DurationMs: new(12.5)}, 40)
	if !override.hasShell || len(override.shell) != 1 || override.shell[0] != "historical output" {
		t.Fatalf("unregistered renderer ignored: %+v", override)
	}
	if got := renderers.toolOverrides(ToolExecutionComponent{Name: "unknown"}, 40); got.hasShell {
		t.Fatal("missing renderer did not fall back")
	}
}

func TestRestoredRichResultReachesRenderer(t *testing.T) {
	conv := NewConversationModel()
	appendSDKMessageToConversation(conv, sdk.Message{Role: "tool", ToolCallID: "call", ToolName: "removed_tool", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "partial"}, {Type: ai.ContentBlockImage, Data: "aW1hZ2U="}}, StructuredContent: json.RawMessage(`{"answer":42}`), ToolUsage: &ai.Usage{Input: 4}, NestedCalls: []ai.NestedToolCall{{ID: "child"}}, IsError: true})
	registry := extensions.New()
	called := false
	registry.RegisterToolRendererResolver(func(name string, next func() extensions.Renderer) extensions.Renderer {
		if name != "removed_tool" {
			return next()
		}
		return func(payload any) []string {
			called = true
			tool := payload.(ToolExecutionComponent)
			if len(tool.Content) != 2 || string(tool.StructuredContent) != `{"answer":42}` || tool.ToolUsage == nil || tool.ToolUsage.Input != 4 || len(tool.NestedCalls) != 1 || !tool.IsError {
				t.Fatalf("renderer lost rich error result: %+v", tool)
			}
			return []string{"rendered rich result"}
		}
	})
	renderers := newPluginRenderers()
	renderers.ApplyExtensionsRegistry(registry)
	override := renderers.toolOverrides(conv.Snapshot().Components[0].ToolExecution, 60)
	if !called || !override.hasShell {
		t.Fatal("historical rich result bypassed renderer")
	}
}
