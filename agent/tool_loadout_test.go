package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/ai"
)

func TestDeferredActivationSurvivesReconnection(t *testing.T) {
	a, err := New(NewConfig(WithTools(Tool{Name: "lookup", Exposure: ToolDeferred})))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetActiveTools("lookup"); err != nil {
		t.Fatal(err)
	}
	update, err := a.prepareRequest(t.Context(), PrepareRequestContext{Context: a.createContextSnapshot()})
	if err != nil {
		t.Fatal(err)
	}
	messages := append(update.Context.Messages, update.Messages...)
	a.RestoreMessages(messages)
	a.SetTools()
	a.SetTools(Tool{Name: "lookup", Exposure: ToolDeferred})
	update, err = a.prepareRequest(t.Context(), PrepareRequestContext{Context: a.createContextSnapshot()})
	if err != nil {
		t.Fatal(err)
	}
	current := *update.Context
	current.Messages = append(current.Messages, update.Messages...)
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		baseline := ai.CurrentSystemMessage(ai.NormalizeContext(prompt).Messages)
		if baseline == nil || len(baseline.ToolsAdded) != 1 || baseline.ToolsAdded[0].Name != "lookup" {
			t.Errorf("loaded deferred declaration lost: %+v", baseline)
		}
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err = streamAssistantResponse(t.Context(), &current, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{ID: "test"}, "", nil, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
}

func TestToolDiscoveryAndReachPermissions(t *testing.T) {
	tools := []Tool{
		{Name: "mcp__docs__lookup", Exposure: ToolDeferred, Description: "Search manuals", Namespace: &ToolNamespace{Name: "mcp__docs", Aliases: []string{"documents"}, Description: "Reference documentation"}},
		{Name: "private", Exposure: ToolHidden, Description: "Search manuals"},
		{Name: "discover", Exposure: ToolModelOnly, ReachesTools: func(map[string]any) []string { return []string{"mcp__docs"} }},
	}
	current := &AgentContext{Tools: tools}
	matches, err := SearchTools(t.Context(), tools, "manuals", "documents", 8)
	if err != nil || len(matches) != 1 || matches[0].Name != tools[0].Name {
		t.Fatalf("search: %+v, %v", matches, err)
	}
	caller := tools[2]
	reachable := ReachableTools(current, caller, nil)
	if len(reachable) != 1 || reachable[0].Name != tools[0].Name {
		t.Fatalf("reach permissions: %+v", reachable)
	}
	calls := 0
	tools[0].Execute = func(ToolContext) (*ToolResult, error) {
		calls++
		return &ToolResult{StructuredContent: json.RawMessage(`42`)}, nil
	}
	result := RunToolCall(t.Context(), &AgentContext{Tools: ReachableTools(current, caller, nil)}, nil, ai.ToolCall{Name: "private", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if !result.IsError || calls != 0 {
		t.Fatalf("unauthorized nested call: %+v", result)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := SearchTools(ctx, tools, "manuals", "", 8); err == nil {
		t.Fatal("cancelled search succeeded")
	}
}

func TestNestedCallerRegistryAndReadiness(t *testing.T) {
	var calls, ready int
	target := Tool{Parameters: map[string]any{"type": "object"}, Name: "docs_read", Exposure: ToolDeferred, Namespace: &ToolNamespace{Name: "docs"}, WaitReady: func(context.Context) error { ready++; return nil }, Execute: func(ToolContext) (*ToolResult, error) { calls++; return &ToolResult{Content: "ok"}, nil }}
	caller := Tool{Parameters: map[string]any{"type": "object"}, Name: "replacement", ReachesTools: func(map[string]any) []string { return []string{"docs"} }, Execute: func(ctx ToolContext) (*ToolResult, error) {
		result := ctx.ExecuteTool(ai.ToolCall{Name: "docs_read", Arguments: json.RawMessage(`{}`)})
		denied := ctx.ExecuteTool(ai.ToolCall{Name: "secret", Arguments: json.RawMessage(`{}`)})
		if !denied.IsError {
			t.Error("hidden tool leaked into caller registry")
		}
		return result, nil
	}}
	current := &AgentContext{Tools: []Tool{caller, target, {Name: "secret", Exposure: ToolHidden}}}
	result := RunToolCall(t.Context(), current, nil, ai.ToolCall{Name: "replacement", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if result.IsError || calls != 1 || ready != 2 {
		t.Fatalf("nested execution: %+v, calls=%d ready=%d", result, calls, ready)
	}
}

func TestExactActivationOverridesRegistryDelta(t *testing.T) {
	a, err := New(NewConfig(WithTools(Tool{Name: "old"})))
	if err != nil {
		t.Fatal(err)
	}
	a.SetTools(Tool{Name: "old"}, Tool{Name: "new"})
	if err := a.SetActiveTools("old"); err != nil {
		t.Fatal(err)
	}
	update, err := a.prepareRequest(t.Context(), PrepareRequestContext{Context: a.createContextSnapshot()})
	if err != nil {
		t.Fatal(err)
	}
	baseline := ai.CurrentSystemMessage(ConvertMessagesToAI(append(update.Context.Messages, update.Messages...)))
	if baseline == nil || len(baseline.ToolsAdded) != 1 || baseline.ToolsAdded[0].Name != "old" {
		t.Fatalf("not exact: %+v", baseline)
	}
}

func TestLoadedDeferredSchemaRefresh(t *testing.T) {
	a, err := New(NewConfig(WithTools(Tool{Name: "lookup", Exposure: ToolDeferred})))
	if err != nil {
		t.Fatal(err)
	}
	a.RestoreMessages([]*Message{{Role: RoleSystem, ToolsAdded: []ai.Tool{{Name: "lookup", Description: "old"}}}})
	a.SetTools(Tool{Name: "lookup", Exposure: ToolDeferred, Description: "new"})
	update, err := a.prepareRequest(t.Context(), PrepareRequestContext{Context: a.createContextSnapshot()})
	if err != nil {
		t.Fatal(err)
	}
	baseline := ai.CurrentSystemMessage(ConvertMessagesToAI(append(update.Context.Messages, update.Messages...)))
	if baseline.ToolsAdded[0].Description != "new" {
		t.Fatalf("stale schema: %+v", baseline.ToolsAdded)
	}
}

func TestActivationFailureRetainsRegistryUpdate(t *testing.T) {
	a, err := New(NewConfig(WithTools(Tool{Name: "old"})))
	if err != nil {
		t.Fatal(err)
	}
	a.SetTools(Tool{Name: "old"}, Tool{Name: "broken", Exposure: ToolDeferred, Parameters: map[string]any{"bad": func() {}}})
	if err := a.SetActiveTools("broken"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.prepareRequest(t.Context(), PrepareRequestContext{Context: a.createContextSnapshot()}); err == nil {
		t.Fatal("invalid schema accepted")
	}
	if !a.toolRegistryChanged || !a.activeToolsChanged {
		t.Fatal("failed request consumed staged changes")
	}
}

func TestInactiveDefaultsAndCancelledEmptySearch(t *testing.T) {
	tools := []Tool{{Name: "disabled", DefaultActive: new(false)}}
	if got := CallableTools(&AgentContext{Tools: tools}); len(got) != 0 {
		t.Fatalf("inactive default callable: %+v", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := SearchTools(ctx, nil, "docs", "missing", 8); !errors.Is(err, context.Canceled) {
		t.Fatalf("empty search ignored cancellation: %v", err)
	}
}

func TestSearchIncludesSchemaWordsAndNamespaceIdentifiers(t *testing.T) {
	tools := []Tool{{Name: "findAccount", Exposure: ToolDeferred, Parameters: map[string]any{"type": "object", "properties": map[string]any{"billingAddress": map[string]any{"type": "string", "description": "Customer postal destination"}}}, Namespace: &ToolNamespace{Name: "mcp__accounts"}}}
	matches, err := SearchTools(t.Context(), tools, "postal", "mcp__accounts", 8)
	if err != nil || len(matches) != 1 {
		t.Fatalf("schema metadata not searchable: %+v, %v", matches, err)
	}
	if len(ReachableTools(&AgentContext{Tools: tools}, Tool{ReachesTools: func(map[string]any) []string { return []string{"mcp__accounts.find_account"} }}, nil)) != 1 {
		t.Fatal("namespace identifier did not authorize tool")
	}
}

func TestCancellableReachCallback(t *testing.T) {
	current := &AgentContext{Tools: []Tool{{Name: "lookup", Exposure: ToolDeferred}}}
	caller := Tool{ReachTools: func(ctx context.Context, args map[string]any) ([]string, error) { return []string{"all"}, ctx.Err() }}
	tools, err := ReachableToolsContext(t.Context(), current, caller, nil)
	if err != nil || len(tools) != 1 {
		t.Fatalf("reach callback failed: %+v, %v", tools, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = ReachableToolsContext(ctx, current, caller, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reach: %v", err)
	}
}

func TestSearchRanksWholeStemmedTermsWithLengthNormalization(t *testing.T) {
	tools := []Tool{
		{Name: "noise", Exposure: ToolDeferred, Description: strings.Repeat("issue ", 2) + strings.Repeat("unrelated ", 300)},
		{Name: "listIssues", Exposure: ToolDeferred, Description: "Find project issues"},
		{Name: "tissue", Exposure: ToolDeferred, Description: "paper tissues"},
	}
	found, err := SearchTools(t.Context(), tools, "the issue", "", 3)
	if err != nil || len(found) != 2 || found[0].Name != "listIssues" {
		t.Fatalf("ranked search: %+v, %v", found, err)
	}
	found, err = SearchTools(t.Context(), tools, "list", "", 3)
	if err != nil || len(found) != 1 || found[0].Name != "listIssues" {
		t.Fatalf("camel case search: %+v, %v", found, err)
	}
}

func TestReachReadinessCancellationPreventsExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var executions int
	target := Tool{Name: "docs_read", Exposure: ToolDeferred, Namespace: &ToolNamespace{Name: "docs"}, Parameters: map[string]any{"type": "object"}, WaitReady: func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	}, Execute: func(ToolContext) (*ToolResult, error) { executions++; return &ToolResult{}, nil }}
	caller := Tool{Name: "replacement_search", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"docs"} }, Execute: func(ToolContext) (*ToolResult, error) { executions++; return &ToolResult{}, nil }}
	result := RunToolCall(ctx, &AgentContext{Tools: []Tool{caller, target}}, nil, ai.ToolCall{Name: caller.Name, Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if !errors.Is(result.Rejection, context.Canceled) || executions != 0 {
		t.Fatalf("cancelled readiness executed work: %+v, executions=%d", result, executions)
	}
}
