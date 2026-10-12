package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/losinggeneration/tau/ai"
)

func TestExecuteParallelRunsToolsConcurrently(t *testing.T) {
	var running atomic.Int32
	var sawParallel atomic.Bool

	sleepTool := Tool{
		Name:       "sleep",
		Parameters: map[string]any{},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			if running.Add(1) > 1 {
				sawParallel.Store(true)
			}
			time.Sleep(50 * time.Millisecond)
			running.Add(-1)
			return &ToolResult{Content: "ok", Details: map[string]any{}}, nil
		},
	}

	ctx := &AgentContext{
		Tools: []Tool{sleepTool},
	}

	calls := []ai.ToolCall{
		mustCreateToolCall(t, "sleep"),
		mustCreateToolCall(t, "sleep"),
	}

	start := time.Now()
	results := executeParallel(context.Background(), ctx, nil, calls, AgentLoopConfig{ToolExecution: ToolExecutionParallel}, func(Event) {})
	elapsed := time.Since(start)

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if !sawParallel.Load() {
		t.Fatal("expected tool executions to overlap")
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("parallel execution took too long: %v", elapsed)
	}
}

func mustCreateToolCall(t *testing.T, name string) ai.ToolCall {
	t.Helper()

	call, err := CreateToolCall(name, map[string]any{})
	if err != nil {
		t.Fatalf("CreateToolCall(%q): %v", name, err)
	}

	if call.ID == "" {
		t.Fatalf("CreateToolCall(%q) returned empty ID", name)
	}

	return call
}

func mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func TestPrepareToolCallBlockReason(t *testing.T) {
	assistantMsg := &Message{Role: RoleAssistant, Content: []ai.ContentBlock{}}
	prepared := prepareToolCall(context.Background(), &AgentContext{
		Tools: []Tool{{
			Name:       "blocked",
			Parameters: map[string]any{"type": "object"},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				return &ToolResult{Content: "unexpected", Details: map[string]any{}}, nil
			},
		}},
	}, assistantMsg, ai.ToolCall{ID: "t1", Name: "blocked", Arguments: []byte(`{}`)}, AgentLoopConfig{
		BeforeToolCall: func(ctx context.Context, toolCtx ToolCallContext) (BeforeToolCallResult, error) {
			return BeforeToolCallResult{Block: true, Reason: "denied"}, nil
		},
	})

	if prepared.immediate == nil {
		t.Fatal("expected blocked call to return immediate result")
	}
	if prepared.immediate.Content != "denied" {
		t.Fatalf("immediate content = %q, want denied", prepared.immediate.Content)
	}
}

func TestPrepareToolCallBlockDefaultReasonMatchesParity(t *testing.T) {
	assistantMsg := &Message{Role: RoleAssistant, Content: []ai.ContentBlock{}}
	prepared := prepareToolCall(context.Background(), &AgentContext{
		Tools: []Tool{{
			Name:       "blocked",
			Parameters: map[string]any{"type": "object"},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				return &ToolResult{Content: "unexpected", Details: map[string]any{}}, nil
			},
		}},
	}, assistantMsg, ai.ToolCall{ID: "t1", Name: "blocked", Arguments: []byte(`{}`)}, AgentLoopConfig{
		BeforeToolCall: func(ctx context.Context, toolCtx ToolCallContext) (BeforeToolCallResult, error) {
			return BeforeToolCallResult{Block: true}, nil
		},
	})

	if prepared.immediate == nil {
		t.Fatal("expected blocked call to return immediate result")
	}
	if !prepared.immediate.IsError {
		t.Fatal("expected blocked call to be an error result")
	}
	if prepared.immediate.Content != "Tool execution was blocked" {
		t.Fatalf("immediate content = %q, want %q", prepared.immediate.Content, "Tool execution was blocked")
	}
}

func TestPrepareToolCallValidatesArgumentsAgainstSchema(t *testing.T) {
	assistantMsg := &Message{Role: RoleAssistant, Content: []ai.ContentBlock{}}
	prepared := prepareToolCall(context.Background(), &AgentContext{
		Tools: []Tool{{
			Name: "read",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				return &ToolResult{Content: "unexpected", Details: map[string]any{}}, nil
			},
		}},
	}, assistantMsg, ai.ToolCall{ID: "t1", Name: "read", Arguments: []byte(`{}`)}, AgentLoopConfig{})

	if prepared.immediate == nil {
		t.Fatal("expected invalid arguments to produce immediate error")
	}
	if !prepared.immediate.IsError {
		t.Fatal("expected validation failure to be marked as error")
	}
}

func TestPrepareToolCallValidatesPreparedArguments(t *testing.T) {
	assistantMsg := &Message{Role: RoleAssistant, Content: []ai.ContentBlock{}}
	prepared := prepareToolCall(context.Background(), &AgentContext{
		Tools: []Tool{{
			Name: "read",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
			PrepareArguments: func(args map[string]any) map[string]any {
				if args == nil {
					args = map[string]any{}
				}
				args["path"] = "/tmp/file.txt"
				return args
			},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				return &ToolResult{Content: "unexpected", Details: map[string]any{}}, nil
			},
		}},
	}, assistantMsg, ai.ToolCall{ID: "t1", Name: "read", Arguments: []byte(`{}`)}, AgentLoopConfig{})

	if prepared.immediate != nil {
		t.Fatalf("expected prepared arguments to satisfy schema, got immediate result: %#v", prepared.immediate)
	}
	if prepared.args["path"] != "/tmp/file.txt" {
		t.Fatalf("path = %v, want /tmp/file.txt", prepared.args["path"])
	}
}

func TestFinalizeToolResultCreatesMessage(t *testing.T) {
	result := finalizeToolResult(ai.ToolCall{ID: "call", Name: "echo"}, &ToolResult{
		Content: "done",
		Details: map[string]any{},
	}, func(Event) {})

	if result.Message == nil {
		t.Fatal("expected tool result message to be attached")
	}
	if got := result.Message.ToolCallID; got != "call" {
		t.Fatalf("toolCallID = %q, want call", got)
	}
	if got := result.Message.ToolName; got != "echo" {
		t.Fatalf("toolName = %q, want echo", got)
	}
	if got := result.Message.Content[0].Text; got != "done" {
		t.Fatalf("content = %q, want done", got)
	}
}

func TestFinalizeToolResultUsesStructuredContent(t *testing.T) {
	result := finalizeToolResult(ai.ToolCall{ID: "call", Name: "echo"}, &ToolResult{
		ContentBlocks: []ai.ContentBlock{
			{Type: ai.ContentBlockText, Text: "hello"},
			{Type: ai.ContentBlockImage, Data: "abc", MimeType: "image/png"},
		},
		Details: map[string]any{},
	}, func(Event) {})

	if len(result.Message.Content) != 2 {
		t.Fatalf("len(content) = %d, want 2", len(result.Message.Content))
	}
	if result.Message.Content[1].Type != ai.ContentBlockImage {
		t.Fatalf("content[1].Type = %q, want %q", result.Message.Content[1].Type, ai.ContentBlockImage)
	}
}

func TestExecutePreparedToolEmitsToolUpdate(t *testing.T) {
	prepared := preparedToolCall{
		toolCall: ai.ToolCall{ID: "t1", Name: "stream"},
		tool: &Tool{
			Name: "stream",
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				ctx.OnUpdate(&ToolResult{
					Content: "partial",
					Details: map[string]any{"step": 1},
				})
				return &ToolResult{
					Content: "done",
					Details: map[string]any{},
				}, nil
			},
		},
		args: map[string]any{"x": 1},
	}

	var sawUpdate bool
	result := executePreparedTool(context.Background(), &AgentContext{}, nil, prepared, AgentLoopConfig{}, func(event Event) {
		update, ok := event.(ToolUpdate)
		if !ok {
			return
		}
		sawUpdate = true
		if update.ToolCallID != "t1" {
			t.Fatalf("toolCallID = %q, want t1", update.ToolCallID)
		}
		if update.PartialResult == nil || update.PartialResult.Message == nil {
			t.Fatal("expected partial result message")
		}
	})

	if !sawUpdate {
		t.Fatal("expected tool update event")
	}
	if result.Content != "done" {
		t.Fatalf("result.Content = %q, want done", result.Content)
	}
}

func TestExecutePreparedToolPassesContext(t *testing.T) {
	ctx := context.Background()
	prepared := preparedToolCall{
		toolCall: ai.ToolCall{ID: "t1", Name: "signal"},
		tool: &Tool{
			Name: "signal",
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				if ctx.Context == nil {
					t.Fatal("expected context to be passed to tool")
				}
				return &ToolResult{Content: "ok", Details: map[string]any{}}, nil
			},
		},
		args: map[string]any{},
	}

	result := executePreparedTool(ctx, &AgentContext{}, nil, prepared, AgentLoopConfig{}, func(Event) {})
	if result.Content != "ok" {
		t.Fatalf("result.Content = %q, want ok", result.Content)
	}
}

func TestPrepareToolCallBeforeHookErrorBecomesImmediateErrorResult(t *testing.T) {
	assistantMsg := &Message{Role: RoleAssistant, Content: []ai.ContentBlock{}}
	prepared := prepareToolCall(context.Background(), &AgentContext{Tools: []Tool{{
		Name:       "read",
		Parameters: map[string]any{"type": "object"},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			return &ToolResult{Content: "unexpected"}, nil
		},
	}}}, assistantMsg, ai.ToolCall{ID: "t1", Name: "read", Arguments: []byte(`{}`)}, AgentLoopConfig{
		BeforeToolCall: func(ctx context.Context, toolCtx ToolCallContext) (BeforeToolCallResult, error) {
			return BeforeToolCallResult{}, fmt.Errorf("hook failed")
		},
	})
	if prepared.immediate == nil || !prepared.immediate.IsError {
		t.Fatalf("prepared.immediate = %#v, want error result", prepared.immediate)
	}
	if !strings.Contains(prepared.immediate.Content, "hook failed") {
		t.Fatalf("content = %q, want hook failed", prepared.immediate.Content)
	}
}

func TestExecutePreparedToolExecutionErrorBecomesErrorResult(t *testing.T) {
	assistantMsg := &Message{Role: RoleAssistant, Content: []ai.ContentBlock{}}
	prepared := preparedToolCall{
		toolCall: ai.ToolCall{ID: "t1", Name: "read", Arguments: []byte(`{}`)},
		tool: &Tool{
			Name:       "read",
			Parameters: map[string]any{"type": "object"},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				return nil, fmt.Errorf("tool exploded")
			},
		},
		args: map[string]any{},
	}
	result := executePreparedTool(context.Background(), &AgentContext{}, assistantMsg, prepared, AgentLoopConfig{}, func(Event) {})
	if result == nil || !result.IsError || result.Content != "tool exploded" {
		t.Fatalf("result = %#v, want tool exploded error result", result)
	}
}

func TestExecutePreparedToolAfterHookErrorBecomesErrorResult(t *testing.T) {
	assistantMsg := &Message{Role: RoleAssistant, Content: []ai.ContentBlock{}}
	prepared := preparedToolCall{
		toolCall: ai.ToolCall{ID: "t1", Name: "read", Arguments: []byte(`{}`)},
		tool: &Tool{
			Name:       "read",
			Parameters: map[string]any{"type": "object"},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				return &ToolResult{Content: "ok"}, nil
			},
		},
		args: map[string]any{},
	}
	result := executePreparedTool(context.Background(), &AgentContext{}, assistantMsg, prepared, AgentLoopConfig{
		AfterToolCall: func(ctx context.Context, toolCtx ToolCallContext, result *ToolResult) (AfterToolCallResult, error) {
			return AfterToolCallResult{}, fmt.Errorf("after failed")
		},
	}, func(Event) {})
	if result == nil || !result.IsError || result.Content != "after failed" {
		t.Fatalf("result = %#v, want after failed error result", result)
	}
}

func TestExecutePreparedToolAfterHookOverrides(t *testing.T) {
	overrideError := true
	prepared := preparedToolCall{
		toolCall: ai.ToolCall{ID: "t1", Name: "override"},
		tool: &Tool{
			Name: "override",
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				return &ToolResult{
					Content: "original",
					Details: map[string]any{"a": 1},
				}, nil
			},
		},
		args: map[string]any{},
	}

	result := executePreparedTool(context.Background(), &AgentContext{}, nil, prepared, AgentLoopConfig{
		AfterToolCall: func(ctx context.Context, toolCtx ToolCallContext, result *ToolResult) (AfterToolCallResult, error) {
			return AfterToolCallResult{
				ContentBlocks: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "override"}},
				Details:       map[string]any{"b": 2},
				IsError:       &overrideError,
			}, nil
		},
	}, func(Event) {})

	if !result.IsError {
		t.Fatal("expected after hook to override isError")
	}
	if len(result.ContentBlocks) != 1 || result.ContentBlocks[0].Text != "override" {
		t.Fatal("expected after hook to override content blocks")
	}
	details := detailsMap(t, result.Details)
	if details["b"] != 2 {
		t.Fatal("expected after hook to replace details")
	}
	if _, ok := details["a"]; ok {
		t.Fatal("expected original details to be replaced")
	}
}

func TestPrepareToolCallNotFound(t *testing.T) {
	assistantMsg := &Message{Role: RoleAssistant, Content: []ai.ContentBlock{}}
	prepared := prepareToolCall(context.Background(), &AgentContext{}, assistantMsg, ai.ToolCall{ID: "missing", Name: "missing"}, AgentLoopConfig{})
	if prepared.immediate == nil {
		t.Fatal("expected immediate error result")
	}
	if prepared.immediate.Content != "Tool missing not found" {
		t.Fatalf("content = %q, want %q", prepared.immediate.Content, "Tool missing not found")
	}
}

func TestExecuteSequentialEmitsToolStartWithRawArgsBeforePrepare(t *testing.T) {
	// Verify that ToolStart is emitted BEFORE prepareToolCall,
	// and that Args in the event are the raw parsed args (not post-validated).
	// This matches the TypeScript reference behavior.
	var toolStartReceived bool
	var toolStartArgs map[string]any

	toolCall, err := CreateToolCall("read", map[string]any{"path": "f"})
	if err != nil {
		t.Fatalf("CreateToolCall: %v", err)
	}

	// Use mustMarshal to create raw JSON bytes matching the actual streaming format
	toolCall.Arguments = mustMarshal(map[string]any{"path": "f"})

	calls := []ai.ToolCall{toolCall}

	_ = executeSequential(context.Background(), &AgentContext{
		Tools: []Tool{{
			Name: "read",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				return &ToolResult{Content: "ok"}, nil
			},
		}},
	}, &Message{Role: RoleAssistant, Content: []ai.ContentBlock{}}, calls, AgentLoopConfig{}, func(event Event) {
		if start, ok := event.(ToolStart); ok {
			toolStartReceived = true
			toolStartArgs = start.Args
		}
	})

	if !toolStartReceived {
		t.Fatal("expected ToolStart event")
	}
	if toolStartArgs == nil {
		t.Fatal("expected toolStartArgs to be non-nil (raw parsed args)")
	}
	if got := toolStartArgs["path"]; got != "f" {
		t.Fatalf("toolStartArgs[path] = %v, want %q", got, "f")
	}
}

func TestExecuteParallelEmitsToolStartBeforePrepareAndSourceOrderToolEnd(t *testing.T) {
	// Verify that:
	// 1. ToolStart is emitted for ALL tools before ANY prepare calls
	// 2. ToolEnd is emitted in original source order (not completion order)
	// This matches the TypeScript reference behavior.

	var toolStartSeq []string
	var toolEndSeq []string
	var mu sync.Mutex
	record := func(seq *[]string, name string) {
		mu.Lock()
		*seq = append(*seq, name)
		mu.Unlock()
	}

	slowTool := Tool{
		Name:       "slow",
		Parameters: map[string]any{"type": "object"},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			time.Sleep(50 * time.Millisecond)
			return &ToolResult{Content: "slow done"}, nil
		},
	}
	fastTool := Tool{
		Name:       "fast",
		Parameters: map[string]any{"type": "object"},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			return &ToolResult{Content: "fast done"}, nil
		},
	}

	slowCall := ai.ToolCall{ID: "s1", Name: "slow", Arguments: mustMarshal(map[string]any{})}
	fastCall := ai.ToolCall{ID: "f1", Name: "fast", Arguments: mustMarshal(map[string]any{})}

	_ = executeParallel(
		context.Background(), &AgentContext{
			Tools: []Tool{slowTool, fastTool},
		}, &Message{Role: RoleAssistant, Content: []ai.ContentBlock{}},
		[]ai.ToolCall{slowCall, fastCall},
		AgentLoopConfig{},
		func(event Event) {
			if start, ok := event.(ToolStart); ok {
				record(&toolStartSeq, start.Name)
			}
			if end, ok := event.(ToolEnd); ok {
				name := end.Name
				if name == "" {
					name = end.ToolCallID
				}
				record(&toolEndSeq, name)
			}
		},
	)

	// ToolStart should fire for both tools before any preparation
	if len(toolStartSeq) != 2 {
		t.Fatalf("toolStartSeq = %v, want [slow fast]", toolStartSeq)
	}
	// ToolEnd should be in source order (slow first, then fast)
	// even though fast completes first
	if len(toolEndSeq) != 2 {
		t.Fatalf("toolEndSeq = %v, want [slow fast]", toolEndSeq)
	}
	if toolEndSeq[0] != "slow" || toolEndSeq[1] != "fast" {
		t.Fatalf("ToolEnd not in source order: %v", toolEndSeq)
	}
}

func TestExecuteParallelImmediatePreflightFailureEmitsBeforeSlowToolCompletes(t *testing.T) {
	blockedTool := Tool{
		Name:       "blocked",
		Parameters: map[string]any{"type": "object"},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			return &ToolResult{Content: "unexpected"}, nil
		},
	}
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	slowTool := Tool{
		Name:       "slow",
		Parameters: map[string]any{"type": "object"},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			close(slowStarted)
			<-releaseSlow
			return &ToolResult{Content: "slow done"}, nil
		},
	}

	blockedCall := ai.ToolCall{ID: "b1", Name: "blocked", Arguments: mustMarshal(map[string]any{})}
	slowCall := ai.ToolCall{ID: "s1", Name: "slow", Arguments: mustMarshal(map[string]any{})}

	toolEndCh := make(chan string, 2)
	done := make(chan []*ToolResult, 1)
	go func() {
		results := executeParallel(
			context.Background(), &AgentContext{
				Tools: []Tool{blockedTool, slowTool},
			}, &Message{Role: RoleAssistant, Content: []ai.ContentBlock{}},
			[]ai.ToolCall{blockedCall, slowCall},
			AgentLoopConfig{
				BeforeToolCall: func(ctx context.Context, toolCtx ToolCallContext) (BeforeToolCallResult, error) {
					if toolCtx.ToolName == "blocked" {
						return BeforeToolCallResult{Block: true}, nil
					}
					return BeforeToolCallResult{}, nil
				},
			},
			func(event Event) {
				if end, ok := event.(ToolEnd); ok {
					toolEndCh <- end.Name
				}
			},
		)
		done <- results
	}()

	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow tool did not start")
	}

	select {
	case got := <-toolEndCh:
		if got != "blocked" {
			t.Fatalf("first ToolEnd = %q, want blocked", got)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked preflight failure was not emitted before slow tool completed")
	}

	close(releaseSlow)
	select {
	case results := <-done:
		if len(results) != 2 {
			t.Fatalf("len(results) = %d, want 2", len(results))
		}
		if !results[0].IsError || results[0].Content != "Tool execution was blocked" {
			t.Fatalf("blocked result = %#v", results[0])
		}
		if results[1].Content != "slow done" {
			t.Fatalf("slow result content = %q, want slow done", results[1].Content)
		}
	case <-time.After(time.Second):
		t.Fatal("parallel execution did not complete")
	}
}

func TestPrepareToolCallBeforeToolCallReceivesValidatedArgs(t *testing.T) {
	// Verify that BeforeToolCall receives args AFTER PrepareArguments + validation.
	// This matches the TypeScript reference behavior.
	assistantMsg := &Message{Role: RoleAssistant, Content: []ai.ContentBlock{}}

	var receivedArgs map[string]any
	prepared := prepareToolCall(context.Background(), &AgentContext{
		Tools: []Tool{{
			Name: "read",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
			PrepareArguments: func(args map[string]any) map[string]any {
				// Inject path if missing
				if args == nil {
					args = map[string]any{}
				}
				if _, ok := args["path"]; !ok {
					args["path"] = "/default.txt"
				}
				return args
			},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				return &ToolResult{Content: "ok"}, nil
			},
		}},
	}, assistantMsg, ai.ToolCall{ID: "t1", Name: "read", Arguments: []byte(`{}`)}, AgentLoopConfig{
		BeforeToolCall: func(ctx context.Context, toolCtx ToolCallContext) (BeforeToolCallResult, error) {
			receivedArgs = toolCtx.Args
			return BeforeToolCallResult{}, nil
		},
	})

	if prepared.immediate != nil {
		t.Fatalf("unexpected immediate error: %v", prepared.immediate)
	}
	// BeforeToolCall should receive the path that was injected by PrepareArguments
	// and then validated by the schema
	if receivedArgs == nil {
		t.Fatal("BeforeToolCall was not called")
	}
	if path := receivedArgs["path"]; path != "/default.txt" {
		t.Fatalf("BeforeToolCall received path = %v, want %q", path, "/default.txt")
	}
}

func TestRunAgentLoopStopsAfterEmptyAssistantResponse(t *testing.T) {
	var streamCalls int
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			streamCalls++
			final := ai.AssistantMessage{
				Role:       "assistant",
				Content:    []ai.ContentBlock{},
				StopReason: ai.StopReasonStop,
				Timestamp:  time.Now().UnixMilli(),
			}
			handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: final.Timestamp}})
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	result, err := RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI}},
		AgentLoopConfig{
			Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:  DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
			GetFollowUpMessages: func(ctx context.Context) ([]*Message, error) {
				return []*Message{NewUserMessage("do not send")}, nil
			},
		},
		func(Event) {},
	)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}
	if streamCalls != 1 {
		t.Fatalf("stream calls = %d, want 1", streamCalls)
	}
	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want user + empty assistant", len(result))
	}
	if result[1].StopReason != ai.StopReasonError {
		t.Fatalf("empty response stop reason = %q, want error", result[1].StopReason)
	}
	if !strings.Contains(result[1].ErrorMessage, "empty response") {
		t.Fatalf("empty response error message = %q", result[1].ErrorMessage)
	}
}

func TestRunAgentLoopProviderErrorStillEmitsFinalAgentEnd(t *testing.T) {
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			return fmt.Errorf("provider failed")
		},
	}

	var events []string
	result, err := RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI}},
		AgentLoopConfig{
			Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:  DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
		},
		func(event Event) { events = append(events, eventLabel(event)) },
	)
	if err == nil {
		t.Fatal("expected provider error")
	}
	if len(result) != 1 || result[0].Role != RoleUser {
		t.Fatalf("result = %#v, want original user prompt only", result)
	}
	if len(events) == 0 || events[len(events)-1] != "agent_end" {
		t.Fatalf("last event = %q in %v, want agent_end", events[len(events)-1], events)
	}
}

func TestRunAgentLoopEmitsEventsInReferenceOrderAcrossToolTurn(t *testing.T) {
	toolCall, err := CreateToolCall("lookup", map[string]any{"q": "status"})
	if err != nil {
		t.Fatalf("CreateToolCall: %v", err)
	}

	var streamCalls int
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			streamCalls++

			switch streamCalls {
			case 1:
				partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
				handler(ctx, ai.Start{Partial: partial})

				partial.Content = []ai.ContentBlock{{
					Type:      ai.ContentBlockToolCall,
					ID:        toolCall.ID,
					Name:      toolCall.Name,
					Arguments: toolCall.Arguments,
				}}
				handler(ctx, ai.ToolCallStart{ContentIndex: 0, Partial: partial})

				final := partial
				final.StopReason = ai.StopReasonToolUse
				handler(ctx, ai.Done{Reason: ai.StopReasonToolUse, Message: final})
				return nil
			case 2:
				partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
				handler(ctx, ai.Start{Partial: partial})

				partial.Content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}
				handler(ctx, ai.TextDelta{ContentIndex: 0, Delta: "done", Partial: partial})

				final := partial
				final.StopReason = ai.StopReasonStop
				handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
				return nil
			default:
				t.Fatalf("unexpected extra Stream call %d", streamCalls)
				return nil
			}
		},
	}

	var events []string
	result, err := RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{
			Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			Tools: []Tool{{
				Name:       "lookup",
				Parameters: map[string]any{"type": "object"},
				Execute: func(ctx ToolContext) (*ToolResult, error) {
					return &ToolResult{Content: "ok", Details: map[string]any{}}, nil
				},
			}},
		},
		AgentLoopConfig{
			Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:  DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
		},
		func(event Event) {
			events = append(events, eventLabel(event))
		},
	)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}
	if len(result) != 4 {
		t.Fatalf("len(result) = %d, want 4", len(result))
	}

	want := []string{
		"agent_start",
		"turn_start",
		"message_start:user",
		"message_end:user",
		"message_start:assistant",
		"message_update:assistant",
		"message_end:assistant",
		"tool_start:lookup",
		"tool_end:lookup",
		"message_start:tool",
		"message_end:tool",
		"turn_end:assistant",
		"turn_start",
		"message_start:assistant",
		"message_update:assistant",
		"message_end:assistant",
		"turn_end:assistant",
		"agent_end",
	}

	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d\n got: %v\nwant: %v", len(events), len(want), events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q\nfull sequence: %v", i, events[i], want[i], events)
		}
	}
}

func TestRunAgentLoopResolvesDynamicAPIKeyForEachAssistantTurn(t *testing.T) {
	toolCall, err := CreateToolCall("lookup", map[string]any{"q": "status"})
	if err != nil {
		t.Fatalf("CreateToolCall: %v", err)
	}

	var streamCalls int
	var apiKeys []string
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			streamCalls++
			apiKeys = append(apiKeys, ai.ExtractSimpleStreamOptions(opts).APIKey)

			switch streamCalls {
			case 1:
				final := ai.AssistantMessage{
					Role: "assistant",
					Content: []ai.ContentBlock{{
						Type:      ai.ContentBlockToolCall,
						ID:        toolCall.ID,
						Name:      toolCall.Name,
						Arguments: toolCall.Arguments,
					}},
					StopReason: ai.StopReasonToolUse,
					Timestamp:  time.Now().UnixMilli(),
				}
				handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: final.Timestamp}})
				handler(ctx, ai.ToolCallStart{ContentIndex: 0, Partial: final})
				handler(ctx, ai.Done{Reason: ai.StopReasonToolUse, Message: final})
				return nil
			case 2:
				final := ai.AssistantMessage{
					Role:       "assistant",
					Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
					StopReason: ai.StopReasonStop,
					Timestamp:  time.Now().UnixMilli(),
				}
				handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: final.Timestamp}})
				handler(ctx, ai.TextDelta{ContentIndex: 0, Delta: "done", Partial: final})
				handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
				return nil
			default:
				t.Fatalf("unexpected extra Stream call %d", streamCalls)
				return nil
			}
		},
	}

	var keyCalls int
	_, err = RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{
			Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			Tools: []Tool{{
				Name:       "lookup",
				Parameters: map[string]any{"type": "object"},
				Execute: func(ctx ToolContext) (*ToolResult, error) {
					return &ToolResult{Content: "ok"}, nil
				},
			}},
		},
		AgentLoopConfig{
			Model:        &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm: DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) {
				return client, nil
			},
			GetAPIKey: func(ctx context.Context, provider string) (string, error) {
				if provider != ai.ProviderOpenAI {
					t.Fatalf("provider = %q, want %q", provider, ai.ProviderOpenAI)
				}
				keyCalls++
				return fmt.Sprintf("turn-key-%d", keyCalls), nil
			},
			APIKey: "static-fallback",
		},
		func(event Event) {},
	)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}
	if keyCalls != 2 {
		t.Fatalf("GetAPIKey calls = %d, want 2", keyCalls)
	}
	wantKeys := []string{"turn-key-1", "turn-key-2"}
	if !reflect.DeepEqual(apiKeys, wantKeys) {
		t.Fatalf("stream API keys = %v, want %v", apiKeys, wantKeys)
	}
}

func TestRunAgentLoopStopsAfterTerminalAssistantErrorBeforeFollowUps(t *testing.T) {
	var followUpPolls int
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			handler(ctx, ai.StreamError{
				Reason: "error",
				Msg: ai.AssistantMessage{
					Role:         "assistant",
					StopReason:   ai.StopReasonError,
					ErrorMessage: "boom",
					Timestamp:    time.Now().UnixMilli(),
				},
			})
			return nil
		},
	}

	var events []string
	result, err := RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{
			Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
		},
		AgentLoopConfig{
			Model:        &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm: DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) {
				return client, nil
			},
			GetFollowUpMessages: func(ctx context.Context) ([]*Message, error) {
				followUpPolls++
				return []*Message{NewUserMessage("should not run")}, nil
			},
		},
		func(event Event) {
			events = append(events, eventLabel(event))
		},
	)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}
	if followUpPolls != 0 {
		t.Fatalf("follow-up polls = %d, want 0 after terminal assistant error", followUpPolls)
	}
	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2", len(result))
	}
	if got := result[1].StopReason; got != ai.StopReasonError {
		t.Fatalf("result[1].StopReason = %q, want %q", got, ai.StopReasonError)
	}

	want := []string{
		"agent_start",
		"turn_start",
		"message_start:user",
		"message_end:user",
		"message_start:assistant",
		"message_end:assistant",
		"turn_end:assistant",
		"agent_end",
	}

	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d\n got: %v\nwant: %v", len(events), len(want), events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q\nfull sequence: %v", i, events[i], want[i], events)
		}
	}
}

func TestRunAgentLoopContinueEmitsLifecycleWithoutPromptReplay(t *testing.T) {
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			final := ai.AssistantMessage{
				Role:       "assistant",
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "continued"}},
				StopReason: ai.StopReasonStop,
				Timestamp:  time.Now().UnixMilli(),
			}
			handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: final.Timestamp}})
			handler(ctx, ai.TextDelta{ContentIndex: 0, Delta: "continued", Partial: final})
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	var events []string
	result, err := RunAgentLoopContinue(
		context.Background(),
		&AgentContext{
			Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			Messages: []*Message{
				NewUserMessage("hi"),
				{
					Role:       RoleTool,
					ToolCallID: "tool-1",
					ToolName:   "lookup",
					Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
					Timestamp:  time.Now(),
				},
			},
		},
		AgentLoopConfig{
			Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:  DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
		},
		func(event Event) {
			events = append(events, eventLabel(event))
		},
	)
	if err != nil {
		t.Fatalf("RunAgentLoopContinue: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if got := result[0].Role; got != RoleAssistant {
		t.Fatalf("result[0].Role = %q, want %q", got, RoleAssistant)
	}

	want := []string{
		"agent_start",
		"turn_start",
		"message_start:assistant",
		"message_update:assistant",
		"message_end:assistant",
		"turn_end:assistant",
		"agent_end",
	}

	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d\n got: %v\nwant: %v", len(events), len(want), events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q\nfull sequence: %v", i, events[i], want[i], events)
		}
	}
}

func eventLabel(event Event) string {
	switch e := event.(type) {
	case AgentStart:
		return "agent_start"
	case AgentEnd:
		return "agent_end"
	case TurnStart:
		return "turn_start"
	case TurnEnd:
		if e.Message == nil {
			return "turn_end"
		}
		return fmt.Sprintf("turn_end:%s", e.Message.Role)
	case MessageStart:
		if e.Message == nil {
			return "message_start"
		}
		return fmt.Sprintf("message_start:%s", e.Message.Role)
	case MessageUpdate:
		if e.Message == nil {
			return "message_update"
		}
		return fmt.Sprintf("message_update:%s", e.Message.Role)
	case MessageEnd:
		if e.Message == nil {
			return "message_end"
		}
		return fmt.Sprintf("message_end:%s", e.Message.Role)
	case ToolStart:
		return fmt.Sprintf("tool_start:%s", e.Name)
	case ToolEnd:
		if e.Name != "" {
			return fmt.Sprintf("tool_end:%s", e.Name)
		}
		if e.Result != nil && e.Result.Message != nil && e.Result.Message.ToolName != "" {
			return fmt.Sprintf("tool_end:%s", e.Result.Message.ToolName)
		}
		return "tool_end"
	case UsageLimitWait:
		return "usage_limit_wait"
	default:
		return fmt.Sprintf("%T", event)
	}
}

func BenchmarkExecuteParallel(b *testing.B) {
	ctx := &AgentContext{
		Tools: []Tool{{
			Name: "noop",
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				return &ToolResult{Content: "ok", Details: map[string]any{}}, nil
			},
		}},
	}

	call, err := CreateToolCall("noop", map[string]any{})
	if err != nil {
		b.Fatal(err)
	}

	calls := []ai.ToolCall{call, {ID: fmt.Sprintf("%s-2", call.ID), Name: "noop", Arguments: call.Arguments}}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		executeParallel(context.Background(), ctx, nil, calls, AgentLoopConfig{ToolExecution: ToolExecutionParallel}, func(Event) {})
	}
}

func TestExecuteToolCallsWithFilterRespectsParallelToolCallsCapability(t *testing.T) {
	var startTime atomic.Int64
	var order []int

	tool := Tool{
		Name:       "order",
		Parameters: map[string]any{},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			idx := int(startTime.Add(1) - 1)
			time.Sleep(50 * time.Millisecond)
			order = append(order, idx)
			return &ToolResult{Content: "ok"}, nil
		},
	}

	agentCtx := &AgentContext{
		Tools: []Tool{tool},
		Capabilities: ai.Capabilities{
			ParallelToolCalls: false,
		},
	}

	calls := []ai.ToolCall{
		mustCreateToolCall(t, "order"),
		mustCreateToolCall(t, "order"),
	}

	// Config requests parallel execution, but provider does not support it.
	// Tools must run sequentially.
	results := executeToolCallsWithFilter(context.Background(), agentCtx, nil, calls,
		AgentLoopConfig{ToolExecution: ToolExecutionParallel}, func(Event) {})

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}

	// Sequential execution means the second tool started after the first finished,
	// so order should be [0, 1].
	if len(order) != 2 || order[0] != 0 || order[1] != 1 {
		t.Errorf("execution order = %v, want [0, 1]; tools should run sequentially", order)
	}
}

func TestAIThinkingBudget(t *testing.T) {
	minimal := 10
	low := 20
	medium := 30
	high := 40
	budgets := &ai.ThinkingBudgets{
		Minimal: &minimal,
		Low:     &low,
		Medium:  &medium,
		High:    &high,
	}

	tests := []struct {
		name  string
		level ThinkingLevel
		want  int
	}{
		{name: "minimal", level: ThinkingMinimal, want: 10},
		{name: "low", level: ThinkingLow, want: 20},
		{name: "medium", level: ThinkingMedium, want: 30},
		{name: "high", level: ThinkingHigh, want: 40},
		{name: "xhigh unsupported", level: ThinkingXHigh, want: 0},
		{name: "off", level: ThinkingOff, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := aiThinkingBudget(tt.level, budgets); got != tt.want {
				t.Fatalf("aiThinkingBudget(%q) = %d, want %d", tt.level, got, tt.want)
			}
		})
	}

	if got := aiThinkingBudget(ThinkingMedium, nil); got != 0 {
		t.Fatalf("aiThinkingBudget(nil budgets) = %d, want 0", got)
	}
}

func TestRunAgentLoopFinalizesPluginBlockedToolCallsAsNormalResults(t *testing.T) {
	// Verify that plugin-blocked tool calls are finalized as normal tool results:
	// ToolEnd + MessageStart/MessageEnd + tool_result dispatch + message appended to context.
	toolCall, err := CreateToolCall("lookup", map[string]any{"q": "test"})
	if err != nil {
		t.Fatalf("CreateToolCall: %v", err)
	}

	var events []string
	var streamCalls int
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			streamCalls++
			if streamCalls == 1 {
				final := ai.AssistantMessage{
					Role: "assistant",
					Content: []ai.ContentBlock{{
						Type:      ai.ContentBlockToolCall,
						ID:        toolCall.ID,
						Name:      toolCall.Name,
						Arguments: toolCall.Arguments,
					}},
					StopReason: ai.StopReasonToolUse,
					Timestamp:  time.Now().UnixMilli(),
				}
				handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: final.Timestamp}})
				handler(ctx, ai.ToolCallStart{ContentIndex: 0, Partial: final})
				handler(ctx, ai.Done{Reason: ai.StopReasonToolUse, Message: final})
				return nil
			}
			final := ai.AssistantMessage{
				Role:       "assistant",
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
				StopReason: ai.StopReasonStop,
				Timestamp:  time.Now().UnixMilli(),
			}
			handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: final.Timestamp}})
			handler(ctx, ai.TextDelta{ContentIndex: 0, Delta: "done", Partial: final})
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	result, err := RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{
			Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			Tools: []Tool{{
				Name:       "lookup",
				Parameters: map[string]any{"type": "object"},
				Execute: func(ctx ToolContext) (*ToolResult, error) {
					t.Fatal("blocked tool should not execute")
					return &ToolResult{Content: "ok"}, nil
				},
			}},
		},
		AgentLoopConfig{
			Model:        &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm: DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) {
				return client, nil
			},
			// Plugin intercepts tool_call event and blocks it
			EventDispatcher: func(name string, payload []byte, sessionID string) EventDispatchResult {
				return EventDispatchResult{Blocked: name == "tool_call"}
			},
		},
		func(event Event) {
			events = append(events, eventLabel(event))
		},
	)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}

	// Verify events: should have tool_start, tool_end, message_start:tool, message_end:tool
	found := func(prefix string) bool {
		for _, e := range events {
			if e == prefix || strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	}
	if !found("tool_start:lookup") {
		t.Fatalf("missing tool_start event, got: %v", events)
	}
	if !found("tool_end") {
		t.Fatalf("missing tool_end event, got: %v", events)
	}
	if !found("message_start:tool") {
		t.Fatalf("missing message_start:tool event, got: %v", events)
	}
	if !found("message_end:tool") {
		t.Fatalf("missing message_end:tool event, got: %v", events)
	}

	// Verify tool result message in context
	if len(result) != 4 {
		t.Fatalf("len(result) = %d, want 4 (user + assistant + tool + assistant)", len(result))
	}
	if result[2].Role != RoleTool {
		t.Fatalf("result[2].Role = %q, want tool", result[2].Role)
	}
	if got := result[2].Content[0].Text; got != "Tool execution was blocked by a plugin" {
		t.Fatalf("tool result content = %q", got)
	}
}

func TestTurnEndContainsToolResultMessagesWithMessageFieldSet(t *testing.T) {
	// Verify TurnEnd events carry ToolResult objects where the Message field is populated.
	// ToolEnd mirrors TypeScript and fires before message_start/message_end, but
	// TurnEnd should still expose finalized tool result messages after execution.
	toolCall, err := CreateToolCall("lookup", map[string]any{"q": "status"})
	if err != nil {
		t.Fatalf("CreateToolCall: %v", err)
	}

	var streamCalls int
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			streamCalls++
			if streamCalls > 1 {
				// Second turn: plain assistant response
				partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
				handler(ctx, ai.Start{Partial: partial})
				partial.Content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}
				handler(ctx, ai.TextDelta{ContentIndex: 0, Delta: "done", Partial: partial})
				final := partial
				final.StopReason = ai.StopReasonStop
				handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
				return nil
			}

			// First turn: tool call response
			partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
			handler(ctx, ai.Start{Partial: partial})
			partial.Content = []ai.ContentBlock{{
				Type:      ai.ContentBlockToolCall,
				ID:        toolCall.ID,
				Name:      toolCall.Name,
				Arguments: toolCall.Arguments,
			}}
			handler(ctx, ai.ToolCallStart{ContentIndex: 0, Partial: partial})
			final := partial
			final.StopReason = ai.StopReasonToolUse
			handler(ctx, ai.Done{Reason: ai.StopReasonToolUse, Message: final})
			return nil
		},
	}

	var turnEndEvent Event
	_, err = RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{
			Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			Tools: []Tool{{
				Name:       "lookup",
				Parameters: map[string]any{"type": "object"},
				Execute: func(ctx ToolContext) (*ToolResult, error) {
					return &ToolResult{Content: "result: ok", Details: map[string]any{}}, nil
				},
			}},
		},
		AgentLoopConfig{
			Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:  DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
		},
		func(event Event) {
			if te, ok := event.(TurnEnd); ok && len(te.ToolResult) > 0 {
				turnEndEvent = event
			}
		},
	)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}

	if turnEndEvent == nil {
		t.Fatal("expected TurnEnd event with tool results")
	}

	te := turnEndEvent.(TurnEnd)
	if len(te.ToolResult) != 1 {
		t.Fatalf("turn_end toolResults len = %d, want 1", len(te.ToolResult))
	}

	// Critical: ToolResult.Message must be populated for consumers to access the tool result message
	if te.ToolResult[0].Message == nil {
		t.Fatal("ToolResult.Message is nil — TurnEnd should expose finalized tool result messages")
	}

	// Verify the message content matches what the tool produced
	msg := te.ToolResult[0].Message
	if msg.Role != RoleTool {
		t.Errorf("tool result message role = %q, want %q", msg.Role, RoleTool)
	}
	if msg.ToolCallID != toolCall.ID {
		t.Errorf("tool result message ToolCallID = %q, want %q", msg.ToolCallID, toolCall.ID)
	}
	if msg.ToolName != "lookup" {
		t.Errorf("tool result message ToolName = %q, want %q", msg.ToolName, "lookup")
	}

	// Check content (tool returned "result: ok")
	foundOK := false
	for _, block := range msg.Content {
		if block.Text == "result: ok" {
			foundOK = true
			break
		}
	}
	if !foundOK {
		t.Error("tool result message content does not contain tool return value")
	}
}

func TestTransformContextIsCalledBeforeConvertToLlmInFullLoop(t *testing.T) {
	// Verify TransformContext runs before ConvertToLlm and can inspect/modify messages.
	var transformCalls, convertCalls int
	transform := func(ctx context.Context, messages []*Message) ([]*Message, error) {
		transformCalls++
		msgs := make([]*Message, 0, len(messages)+1)
		msgs = append(msgs, NewUserMessage("system: extra"))
		msgs = append(msgs, messages...)
		return msgs, nil
	}

	convert := func(ctx context.Context, messages []*Message) ([]ai.Message, error) {
		convertCalls++
		return DefaultConvertToLlm(ctx, messages)
	}

	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
			handler(ctx, ai.Start{Partial: partial})
			partial.Content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}
			handler(ctx, ai.TextDelta{ContentIndex: 0, Delta: "done", Partial: partial})
			final := partial
			final.StopReason = ai.StopReasonStop
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	_, err := RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{
			Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			Tools: []Tool{},
		},
		AgentLoopConfig{
			Model:            &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			TransformContext: transform,
			ConvertToLlm:     convert,
			ClientFactory:    func(model *ai.Model) (ai.Client, error) { return client, nil },
		},
		func(Event) {},
	)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}

	if transformCalls == 0 {
		t.Error("TransformContext was not called")
	}
	if convertCalls == 0 {
		t.Error("ConvertToLlm was not called")
	}

	if transformCalls > 0 && convertCalls > 0 && transformCalls > convertCalls {
		t.Error("TransformContext should be called before ConvertToLlm")
	}
}

func TestExecutePreparedToolAfterHookOverridesWithTextContent(t *testing.T) {
	// Verify afterToolCall can override result content with plain text (not just blocks).
	// This matches the TypeScript reference: afterResult.content ?? result.content.
	prepared := preparedToolCall{
		toolCall: ai.ToolCall{ID: "t1", Name: "text-override"},
		tool: &Tool{
			Name: "text-override",
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				return &ToolResult{
					Content:       "original content",
					ContentBlocks: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "original blocks"}},
					Details:       map[string]any{"key": "original"},
				}, nil
			},
		},
		args: map[string]any{},
	}

	result := executePreparedTool(context.Background(), &AgentContext{}, nil, prepared, AgentLoopConfig{
		AfterToolCall: func(ctx context.Context, toolCtx ToolCallContext, result *ToolResult) (AfterToolCallResult, error) {
			return AfterToolCallResult{
				Content: "overridden text content",
				Details: map[string]any{"key": "overridden"},
				IsError: func() *bool { v := false; return &v }(),
			}, nil
		},
	}, func(Event) {})

	// Content should be overridden with text (not blocks)
	if result.Content != "overridden text content" {
		t.Errorf("content = %q, want %q", result.Content, "overridden text content")
	}
	// ContentBlocks should be cleared when Content is set
	if len(result.ContentBlocks) != 0 {
		t.Errorf("contentBlocks len = %d, want 0 (cleared when Content is set)", len(result.ContentBlocks))
	}
	// Details should be replaced
	details := detailsMap(t, result.Details)
	if details["key"] != "overridden" {
		t.Errorf("details[key] = %v, want overridden", details["key"])
	}
	if _, ok := details["original"]; ok {
		t.Error("expected original details to be replaced")
	}
	// IsError should be overridden
	if result.IsError {
		t.Error("expected isError = false")
	}
}

// TestEventPayloadMessage verifies the JSON payload built for message_user /
// message_assistant / message_system plugin events has the expected shape
// (snake_case fields matching the plugin proto, content flattened to a
// string when there's a single text block, structured blocks otherwise).
func TestEventPayloadMessage(t *testing.T) {
	tests := []struct {
		name    string
		msg     *Message
		wantKey string
	}{
		{
			name: "user single text block",
			msg: &Message{
				Role:    RoleUser,
				Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "hello"}},
			},
			wantKey: "hello",
		},
		{
			name: "assistant multi-block content",
			msg: &Message{
				Role: RoleAssistant,
				Content: []ai.ContentBlock{
					{Type: ai.ContentBlockText, Text: "thinking"},
					{Type: ai.ContentBlockToolCall, ID: "call_1", Name: "read_file"},
				},
			},
			wantKey: "thinking",
		},
		{
			name: "tool message with tool_call_id",
			msg: &Message{
				Role:       RoleTool,
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "file contents"}},
				ToolCallID: "call_42",
				ToolName:   "read_file",
			},
			wantKey: "call_42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := eventPayloadMessage(tt.msg)
			if data == nil {
				t.Fatal("eventPayloadMessage returned nil")
			}
			var p map[string]any
			if err := json.Unmarshal(data, &p); err != nil {
				t.Fatalf("payload is not valid JSON: %v\n%s", err, data)
			}
			if _, ok := p["role"]; !ok {
				t.Errorf("payload missing 'role' field: %s", data)
			}
			if got := p["tool_call_id"]; tt.wantKey != "" && got != tt.wantKey && !containsAll(p, tt.wantKey) {
				// For non-tool cases, just sanity-check that the expected
				// text shows up somewhere in the payload.
				if !containsAll(p, tt.wantKey) {
					t.Errorf("payload = %s, want substring %q", data, tt.wantKey)
				}
			}
		})
	}
}

// TestEventPayloadToolCall verifies the JSON payload for tool_call events
// includes the tool name, call ID, and raw arguments.
func TestEventPayloadToolCall(t *testing.T) {
	args := json.RawMessage(`{"path":"/tmp/x"}`)
	tc := ai.ToolCall{ID: "call_1", Name: "read_file", Arguments: args}
	data := eventPayloadToolCall(tc)
	if data == nil {
		t.Fatal("eventPayloadToolCall returned nil")
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, data)
	}
	if p["tool_name"] != "read_file" {
		t.Errorf("tool_name = %v, want read_file", p["tool_name"])
	}
	if p["tool_call_id"] != "call_1" {
		t.Errorf("tool_call_id = %v, want call_1", p["tool_call_id"])
	}
	arguments, ok := p["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("arguments not a map: %v", p["arguments"])
	}
	if arguments["path"] != "/tmp/x" {
		t.Errorf("arguments.path = %v, want /tmp/x", arguments["path"])
	}
}

// TestEventPayloadToolResult verifies the JSON payload for tool_result
// events includes the result string and isError flag.
func TestEventPayloadToolResult(t *testing.T) {
	result := &ToolResult{
		Content: "file contents",
		IsError: false,
		Message: &Message{ToolCallID: "call_99", ToolName: "read_file"},
	}
	data := eventPayloadToolResult(result)
	if data == nil {
		t.Fatal("eventPayloadToolResult returned nil")
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, data)
	}
	if p["result"] != "file contents" {
		t.Errorf("result = %v, want 'file contents'", p["result"])
	}
	if p["is_error"] != false {
		t.Errorf("is_error = %v, want false", p["is_error"])
	}
	if p["tool_call_id"] != "call_99" {
		t.Errorf("tool_call_id = %v, want call_99", p["tool_call_id"])
	}
}

// TestEventPayloadToolResultNilSafe verifies nil inputs don't crash.
func TestEventPayloadToolResultNilSafe(t *testing.T) {
	if got := eventPayloadToolResult(nil); got != nil {
		t.Errorf("eventPayloadToolResult(nil) = %v, want nil", got)
	}
	if got := eventPayloadMessage(nil); got != nil {
		t.Errorf("eventPayloadMessage(nil) = %v, want nil", got)
	}
}

// containsAll reports whether any string in payloadMap equals want.
func containsAll(payloadMap map[string]any, want string) bool {
	if s, ok := payloadMap["content"].(string); ok && s == want {
		return true
	}
	if arr, ok := payloadMap["content"].([]any); ok {
		for _, b := range arr {
			if m, ok := b.(map[string]any); ok {
				if m["text"] == want {
					return true
				}
			}
		}
	}
	return false
}

// TestEventPayloadModel verifies the model_select payload carries the
// model's ID, provider, and API. Also covers the nil-safe path.
func TestEventPayloadModel(t *testing.T) {
	model := &ai.Model{ID: "claude-opus-4-7", Provider: "anthropic", API: "anthropicMessages"}
	data := eventPayloadModel(model)
	if data == nil {
		t.Fatal("eventPayloadModel returned nil for non-nil model")
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, data)
	}
	if p["id"] != "claude-opus-4-7" {
		t.Errorf("id = %v, want claude-opus-4-7", p["id"])
	}
	if p["provider"] != "anthropic" {
		t.Errorf("provider = %v, want anthropic", p["provider"])
	}
	if p["api"] != "anthropicMessages" {
		t.Errorf("api = %v, want anthropicMessages", p["api"])
	}

	if got := eventPayloadModel(nil); got != nil {
		t.Errorf("eventPayloadModel(nil) = %v, want nil", got)
	}
}

// --- Tests for plugin event dispatches: user_bash, before_provider_request,
// after_provider_response plugin event dispatches.

func TestDispatchUserBashEvent(t *testing.T) {
	var events []string
	callCount := 0
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			callCount++
			handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}})
			if callCount == 1 {
				// First turn: return a bash tool call.
				handler(ctx, ai.Done{Reason: ai.StopReasonToolUse, Message: ai.AssistantMessage{
					Role:      "assistant",
					Timestamp: time.Now().UnixMilli(),
					Content: []ai.ContentBlock{
						{Type: "toolCall", ID: "tc-1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
					},
					StopReason: ai.StopReasonToolUse,
				}})
				return nil
			}
			// Second turn: return the final answer.
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{
				Role:       "assistant",
				Timestamp:  time.Now().UnixMilli(),
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
				StopReason: ai.StopReasonStop,
			}})
			return nil
		},
	}

	tool := Tool{
		Name:       "bash",
		Parameters: map[string]any{},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			return &ToolResult{Content: "listed"}, nil
		},
	}

	a, err := New(Config{
		Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
		ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
		Tools:         []Tool{tool},
		EventDispatcher: func(name string, payload []byte, sessionID string) EventDispatchResult {
			events = append(events, name)
			return EventDispatchResult{}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Abort()

	if err := a.Prompt(context.Background(), NewUserMessage("list files")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// user_bash must be dispatched alongside tool_call.
	var sawUserBash, sawToolCall bool
	for _, e := range events {
		if e == "user_bash" {
			sawUserBash = true
		}
		if e == "tool_call" {
			sawToolCall = true
		}
	}
	if !sawUserBash {
		t.Errorf("user_bash event not dispatched; events = %v", events)
	}
	if !sawToolCall {
		t.Errorf("tool_call event not dispatched; events = %v", events)
	}
}

func TestDispatchBeforeAndAfterProviderRequestEvents(t *testing.T) {
	var events []string
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}})
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{
				Role:       "assistant",
				Timestamp:  time.Now().UnixMilli(),
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
				StopReason: ai.StopReasonStop,
			}})
			return nil
		},
	}

	a, err := New(Config{
		Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
		ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
		EventDispatcher: func(name string, payload []byte, sessionID string) EventDispatchResult {
			events = append(events, name)
			return EventDispatchResult{}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Abort()

	if err := a.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// before_provider_request must fire before after_provider_response.
	beforeIdx, afterIdx := -1, -1
	for i, e := range events {
		if e == "before_provider_request" && beforeIdx == -1 {
			beforeIdx = i
		}
		if e == "after_provider_response" && afterIdx == -1 {
			afterIdx = i
		}
	}
	if beforeIdx == -1 {
		t.Errorf("before_provider_request not dispatched; events = %v", events)
	}
	if afterIdx == -1 {
		t.Errorf("after_provider_response not dispatched; events = %v", events)
	}
	if beforeIdx != -1 && afterIdx != -1 && beforeIdx >= afterIdx {
		t.Errorf("before_provider_request (idx %d) should fire before after_provider_response (idx %d); events = %v",
			beforeIdx, afterIdx, events)
	}
}

type stubClient struct {
	stream func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error
}

func (s stubClient) Stream(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
	return s.stream(ctx, model, prompt, handler, opts...)
}

func (s stubClient) Complete(ctx context.Context, model *ai.Model, prompt *ai.Context, opts ...ai.Option) (*ai.AssistantMessage, error) {
	return nil, nil
}

func TestStreamAssistantResponseNoStartDoneEmitsMessageStartAndEndOnce(t *testing.T) {
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			final := ai.AssistantMessage{
				Role:       "assistant",
				Timestamp:  time.Now().UnixMilli(),
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done without start"}},
				StopReason: ai.StopReasonStop,
			}
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	agentCtx := &AgentContext{Messages: []*Message{NewUserMessage("hi")}}
	var starts, ends int
	msg, err := streamAssistantResponse(
		context.Background(),
		agentCtx,
		AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm},
		client,
		&ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
		"",
		nil,
		func(event Event) {
			switch event.(type) {
			case MessageStart:
				starts++
			case MessageEnd:
				ends++
			}
		},
	)
	if err != nil {
		t.Fatalf("streamAssistantResponse: %v", err)
	}
	if msg == nil || len(msg.Content) != 1 || msg.Content[0].Text != "done without start" {
		t.Fatalf("msg = %#v, want final assistant text", msg)
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("message starts/ends = %d/%d, want 1/1", starts, ends)
	}
}

func TestStreamAssistantResponseEmitsUpdatesForAllAssistantEvents(t *testing.T) {
	now := time.Now().UnixMilli()
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			partial := ai.AssistantMessage{
				Role:      "assistant",
				Timestamp: now,
				Content:   []ai.ContentBlock{},
			}
			handler(ctx, ai.Start{Partial: partial})

			partial.Content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: ""}}
			handler(ctx, ai.TextStart{ContentIndex: 0, Partial: partial})

			partial.Content[0].Text = "hello"
			handler(ctx, ai.TextDelta{ContentIndex: 0, Delta: "hello", Partial: partial})
			handler(ctx, ai.TextEnd{ContentIndex: 0, Content: "hello", Partial: partial})

			final := partial
			final.StopReason = ai.StopReasonStop
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	var updateEvents []ai.StreamEvent
	agentCtx := &AgentContext{Messages: []*Message{NewUserMessage("hi")}}
	msg, err := streamAssistantResponse(
		context.Background(),
		agentCtx,
		AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm},
		client,
		&ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
		"",
		nil,
		func(event Event) {
			update, ok := event.(MessageUpdate)
			if ok {
				updateEvents = append(updateEvents, update.AssistantMessageEvent)
			}
		},
	)
	if err != nil {
		t.Fatalf("streamAssistantResponse: %v", err)
	}
	if msg == nil {
		t.Fatal("expected message")
	}
	if len(updateEvents) != 3 {
		t.Fatalf("len(updateEvents) = %d, want 3", len(updateEvents))
	}
	if _, ok := updateEvents[0].(ai.TextStart); !ok {
		t.Fatalf("updateEvents[0] = %T, want ai.TextStart", updateEvents[0])
	}
	if _, ok := updateEvents[1].(ai.TextDelta); !ok {
		t.Fatalf("updateEvents[1] = %T, want ai.TextDelta", updateEvents[1])
	}
	if _, ok := updateEvents[2].(ai.TextEnd); !ok {
		t.Fatalf("updateEvents[2] = %T, want ai.TextEnd", updateEvents[2])
	}
}

func TestStreamAssistantResponseUsesStreamErrorMessage(t *testing.T) {
	now := time.Now().UnixMilli()
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			handler(ctx, ai.StreamError{
				Reason: "error",
				Msg: ai.AssistantMessage{
					Role:         "assistant",
					StopReason:   ai.StopReasonError,
					ErrorMessage: "boom",
					Timestamp:    now,
				},
			})
			return nil
		},
	}

	var ends int
	agentCtx := &AgentContext{Messages: []*Message{NewUserMessage("hi")}}
	msg, err := streamAssistantResponse(
		context.Background(),
		agentCtx,
		AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm},
		client,
		&ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
		"",
		nil,
		func(event Event) {
			if _, ok := event.(MessageEnd); ok {
				ends++
			}
		},
	)
	if err != nil {
		t.Fatalf("streamAssistantResponse: %v", err)
	}
	if msg == nil {
		t.Fatal("expected message")
	}
	if !msg.IsError {
		t.Fatalf("expected error message, got %+v", msg)
	}
	if ends != 1 {
		t.Fatalf("message end count = %d, want 1", ends)
	}
}

// --- Tests for the channel-based EventStream form (return a stream of
// events and the final message list).

func TestAgentLoopReturnsEventStream(t *testing.T) {
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			final := ai.AssistantMessage{
				Role:       "assistant",
				Timestamp:  time.Now().UnixMilli(),
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "hi"}},
				StopReason: ai.StopReasonStop,
			}
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	agentCtx := &AgentContext{
		Model:    &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
		Messages: []*Message{NewUserMessage("hello")},
	}
	config := AgentLoopConfig{
		ConvertToLlm:  DefaultConvertToLlm,
		ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
	}

	stream := AgentLoop([]*Message{NewUserMessage("hello")}, agentCtx, config, nil)
	if stream == nil {
		t.Fatal("AgentLoop returned nil")
	}
	if stream.Events == nil || stream.Done == nil || stream.Err == nil {
		t.Fatal("EventStream channels must all be non-nil")
	}

	// Drain events (should not block: every event must be read; we collect
	// only to count them).
	var events int
	for range stream.Events {
		events++
	}

	// Run completed. Exactly one of Done or Err received a value; the
	// other is closed. Check Err first to surface any unexpected error;
	// a closed Err channel returns nil which we must distinguish from
	// a real nil error.
	select {
	case err, ok := <-stream.Err:
		if ok {
			t.Fatalf("unexpected error: %v", err)
		}
		// Err was closed without a value: success.
	case msgs, ok := <-stream.Done:
		if !ok {
			t.Fatal("Done channel was closed without a value")
		}
		if msgs == nil {
			t.Fatal("expected non-nil messages")
		}
	}
	if events == 0 {
		t.Fatal("expected at least one event from the loop")
	}
}

func TestAgentLoopContinueReturnsEventStream(t *testing.T) {
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			final := ai.AssistantMessage{
				Role:       "assistant",
				Timestamp:  time.Now().UnixMilli(),
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "continue"}},
				StopReason: ai.StopReasonStop,
			}
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	agentCtx := &AgentContext{
		Model:    &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
		Messages: []*Message{NewUserMessage("first")},
	}
	config := AgentLoopConfig{
		ConvertToLlm:  DefaultConvertToLlm,
		ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
	}

	stream := AgentLoopContinue(agentCtx, config, nil)
	if stream == nil {
		t.Fatal("AgentLoopContinue returned nil")
	}

	for range stream.Events {
		// drain
	}

	// Run completed. Check Err first to surface any error; a closed Err
	// channel returns nil (the success case).
	select {
	case err, ok := <-stream.Err:
		if ok {
			t.Fatalf("unexpected error: %v", err)
		}
	case msgs, ok := <-stream.Done:
		if !ok {
			t.Fatal("Done channel was closed without a value")
		}
		if msgs == nil {
			t.Fatal("expected non-nil messages")
		}
	}
}

func TestAgentLoopSignalCancelsRun(t *testing.T) {
	// Stream that blocks until the loop is cancelled.
	streamStarted := make(chan struct{})
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}})
			close(streamStarted)
			<-ctx.Done()
			return ctx.Err()
		},
	}

	signal := make(chan struct{})
	agentCtx := &AgentContext{
		Model:    &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
		Messages: []*Message{NewUserMessage("hi")},
	}
	config := AgentLoopConfig{
		ConvertToLlm:  DefaultConvertToLlm,
		ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
	}

	stream := AgentLoop([]*Message{NewUserMessage("hi")}, agentCtx, config, signal)

	// Drain events in a goroutine so the producer can make progress and
	// reach the stream function. We only need to know that the run
	// terminated; we don't care which events arrived.
	drained := make(chan struct{})
	go func() {
		for range stream.Events {
		}
		close(drained)
	}()

	// Wait for the stream function to be reached, then signal cancellation.
	<-streamStarted
	close(signal)

	// The drain goroutine will exit once the producer closes Events.
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("events channel was not closed after signal cancellation")
	}

	// Either Done (with the partial assistant message and error) or Err
	// is acceptable; the contract is that the loop terminates. The
	// "ok" return distinguishes a real channel close from a value
	// receive; both are acceptable termination signals.
	select {
	case <-stream.Done:
	case err, ok := <-stream.Err:
		if ok && err == nil {
			t.Fatal("expected non-nil error from cancelled run")
		}
	}
}

func TestAgentLoopNoEventDropsWhenConsumerReads(t *testing.T) {
	// Verifies the fix to the original non-blocking-send bug: events are
	// not dropped when the consumer is actively reading.
	const eventCount = 50

	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			partial := ai.AssistantMessage{
				Role:      "assistant",
				Timestamp: time.Now().UnixMilli(),
				Content:   []ai.ContentBlock{},
			}
			handler(ctx, ai.Start{Partial: partial})
			for i := 0; i < eventCount; i++ {
				partial.Content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "x"}}
				handler(ctx, ai.TextDelta{ContentIndex: 0, Delta: "x", Partial: partial})
			}
			partial.StopReason = ai.StopReasonStop
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: partial})
			return nil
		},
	}

	agentCtx := &AgentContext{
		Model:    &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
		Messages: []*Message{NewUserMessage("hi")},
	}
	config := AgentLoopConfig{
		ConvertToLlm:  DefaultConvertToLlm,
		ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
	}

	stream := AgentLoop([]*Message{NewUserMessage("hi")}, agentCtx, config, nil)

	got := 0
	for range stream.Events {
		got++
	}
	// Expect Start + 50 deltas + Done = 52.
	if got < eventCount+2 {
		t.Fatalf("events received = %d, want at least %d (events were dropped)", got, eventCount+2)
	}
}

// --- Tests for the safe-callback wrappers (callbacks return safe
// fallbacks on internal error).

func TestSafeConvertToLlmReturnsEmptyOnError(t *testing.T) {
	wrapped := safeConvertToLlm(func(ctx context.Context, msgs []*Message) ([]ai.Message, error) {
		return nil, errors.New("boom")
	})
	got, err := wrapped(context.Background(), []*Message{NewUserMessage("hi")})
	if err != nil {
		t.Fatalf("safeConvertToLlm returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("safeConvertToLlm = %v, want empty slice", got)
	}
}

func TestSafeConvertToLlmPassthroughOnSuccess(t *testing.T) {
	want := []ai.Message{{Role: "user", Content: "hi"}}
	wrapped := safeConvertToLlm(func(ctx context.Context, msgs []*Message) ([]ai.Message, error) {
		return want, nil
	})
	got, err := wrapped(context.Background(), nil)
	if err != nil {
		t.Fatalf("safeConvertToLlm returned error: %v", err)
	}
	if len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("safeConvertToLlm = %v, want passthrough", got)
	}
}

func TestSafeTransformContextReturnsOriginalOnError(t *testing.T) {
	original := []*Message{NewUserMessage("hi"), NewUserMessage("there")}
	wrapped := safeTransformContext(func(ctx context.Context, msgs []*Message) ([]*Message, error) {
		return nil, errors.New("boom")
	})
	got, err := wrapped(context.Background(), original)
	if err != nil {
		t.Fatalf("safeTransformContext returned error: %v", err)
	}
	if len(got) != len(original) {
		t.Fatalf("safeTransformContext = %v, want original messages (len %d)", got, len(original))
	}
}

func TestSafeAPIKeyProviderReturnsEmptyOnError(t *testing.T) {
	wrapped := safeAPIKeyProvider(func(ctx context.Context, provider string) (string, error) {
		return "should-be-discarded", errors.New("boom")
	})
	got, err := wrapped(context.Background(), "test")
	if err != nil {
		t.Fatalf("safeAPIKeyProvider returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("safeAPIKeyProvider = %q, want empty string", got)
	}
}

func TestSafeMessageSupplierReturnsEmptyOnError(t *testing.T) {
	wrapped := safeMessageSupplier(func(ctx context.Context) ([]*Message, error) {
		return []*Message{NewUserMessage("should-be-discarded")}, errors.New("boom")
	})
	got, err := wrapped(context.Background())
	if err != nil {
		t.Fatalf("safeMessageSupplier returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("safeMessageSupplier = %v, want empty slice", got)
	}
}

// TestBeforeAgentStart_SystemPromptChained ensures that a plugin's
// before_agent_start response rewriting the system prompt is honored by
// the loop, and that two rewrites chain in dispatch order (last writer's
// value drives the loop). The shim relies on this to inject extensions
// into the system prompt.
//
// Note: each plugin handler receives the ORIGINAL system-prompt payload;
// chaining across plugins is "apply responses in dispatch order, last
// rewrite wins." Within a single pi-ext shim, pi extensions chain through
// pi's own runtime before the single response crosses the RPC boundary.
func TestBeforeAgentStart_SystemPromptChained(t *testing.T) {
	var capturedPrompt string
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			capturedPrompt = prompt.SystemPrompt
			final := ai.AssistantMessage{
				Role:       "assistant",
				Content:    []ai.ContentBlock{},
				StopReason: ai.StopReasonStop,
				Timestamp:  time.Now().UnixMilli(),
			}
			handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: final.Timestamp}})
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	// Two synthetic plugins: A appends "[A]" to whatever system-prompt it
	// receives, B appends "[B]". They both see the same original payload.
	// After chaining, the loop should receive "<base>[B]" (B's rewrite
	// applied last).
	makeResponse := func(appendage string) func(name string, payload []byte, sessionID string) EventDispatchResult {
		return func(name string, payload []byte, _ string) EventDispatchResult {
			if name != "before_agent_start" {
				return EventDispatchResult{}
			}
			var in struct {
				SystemPrompt string `json:"systemPrompt"`
			}
			if err := json.Unmarshal(payload, &in); err != nil {
				return EventDispatchResult{}
			}
			out, _ := json.Marshal(struct {
				SystemPrompt string `json:"systemPrompt"`
			}{SystemPrompt: in.SystemPrompt + appendage})
			return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: out}}}
		}
	}

	dispatcher := func(name string, payload []byte, sessionID string) EventDispatchResult {
		if name != "before_agent_start" {
			return EventDispatchResult{}
		}
		// Two "plugin" responses, in dispatch order: A then B.
		ra := makeResponse("[A]")(name, payload, sessionID)
		rb := makeResponse("[B]")(name, payload, sessionID)
		all := EventDispatchResult{}
		all.Responses = append(all.Responses, ra.Responses...)
		all.Responses = append(all.Responses, rb.Responses...)
		return all
	}

	if _, err := RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI}},
		AgentLoopConfig{
			Model:           &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:    DefaultConvertToLlm,
			ClientFactory:   func(model *ai.Model) (ai.Client, error) { return client, nil },
			SystemPrompt:    "base",
			EventDispatcher: dispatcher,
		},
		func(Event) {},
	); err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}

	want := "base[B]"
	if capturedPrompt != want {
		t.Errorf("captured system prompt = %q, want %q", capturedPrompt, want)
	}
}

// TestBeforeAgentStart_NoRewritesLeavesPromptAlone ensures a dispatcher
// that doesn't return a before_agent_start rewrite does not touch the
// system prompt.
func TestBeforeAgentStart_NoRewritesLeavesPromptAlone(t *testing.T) {
	var capturedPrompt string
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			capturedPrompt = prompt.SystemPrompt
			final := ai.AssistantMessage{
				Role:       "assistant",
				Content:    []ai.ContentBlock{},
				StopReason: ai.StopReasonStop,
				Timestamp:  time.Now().UnixMilli(),
			}
			handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: final.Timestamp}})
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	if _, err := RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI}},
		AgentLoopConfig{
			Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:  DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
			SystemPrompt:  "original",
			EventDispatcher: func(name string, _ []byte, _ string) EventDispatchResult {
				return EventDispatchResult{}
			},
		},
		func(Event) {},
	); err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}
	if capturedPrompt != "original" {
		t.Errorf("captured prompt = %q, want %q", capturedPrompt, "original")
	}
}

// TestBeforeAgentStart_InvalidResponseDataIgnored ensures a malformed
// response_data doesn't break the loop. The chain is applied defensively.
func TestBeforeAgentStart_InvalidResponseDataIgnored(t *testing.T) {
	var capturedPrompt string
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			capturedPrompt = prompt.SystemPrompt
			final := ai.AssistantMessage{
				Role:       "assistant",
				Content:    []ai.ContentBlock{},
				StopReason: ai.StopReasonStop,
				Timestamp:  time.Now().UnixMilli(),
			}
			handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: final.Timestamp}})
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	dispatcher := func(name string, _ []byte, _ string) EventDispatchResult {
		if name != "before_agent_start" {
			return EventDispatchResult{}
		}
		return EventDispatchResult{
			Responses: []EventDispatchResponse{
				{ResponseData: []byte(`not-json`)},
				{ResponseData: []byte(`{"systemPrompt":"kept"}`)},
			},
		}
	}

	if _, err := RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI}},
		AgentLoopConfig{
			Model:           &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:    DefaultConvertToLlm,
			ClientFactory:   func(model *ai.Model) (ai.Client, error) { return client, nil },
			SystemPrompt:    "base",
			EventDispatcher: dispatcher,
		},
		func(Event) {},
	); err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}
	if capturedPrompt != "kept" {
		t.Errorf("captured prompt = %q, want %q", capturedPrompt, "kept")
	}
}

// After a mid-run ReplaceMessages (compaction), the retained tail can begin
// with tool results whose assistant tool-call message was summarized away.
// Providers reject transcripts containing such orphan results, so the rebase
// must drop them while keeping results whose tool call survived.
func TestDropOrphanToolResultsRemovesResultsWithoutToolCall(t *testing.T) {
	keptCall, err := CreateToolCall("kept_tool", map[string]any{})
	if err != nil {
		t.Fatalf("CreateToolCall(kept_tool): %v", err)
	}
	lostCall, err := CreateToolCall("lost_tool", map[string]any{})
	if err != nil {
		t.Fatalf("CreateToolCall(lost_tool): %v", err)
	}

	summary := &Message{
		Role:      RoleSystem,
		Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "Session Summary"}},
		Timestamp: time.Now(),
	}
	assistantWithCall := &Message{
		Role: RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:      ai.ContentBlockToolCall,
			ID:        keptCall.ID,
			Name:      keptCall.Name,
			Arguments: keptCall.Arguments,
		}},
		Timestamp: time.Now(),
	}

	messages := []*Message{
		summary,
		NewToolMessage(lostCall.ID, "lost_tool", "orphaned result", false),
		assistantWithCall,
		NewToolMessage(keptCall.ID, "kept_tool", "kept result", false),
	}

	got := dropOrphanToolResults(messages)

	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (summary, assistant, kept result): %#v", len(got), got)
	}
	if got[0] != summary || got[1] != assistantWithCall {
		t.Fatalf("got = %#v, want summary then assistant preserved in order", got)
	}
	if got[2].ToolCallID != keptCall.ID {
		t.Fatalf("got[2].ToolCallID = %q, want %q", got[2].ToolCallID, keptCall.ID)
	}
}

// Resuming a session whose compaction cut mid-turn loads a context that
// starts with a tool result whose tool call was summarized away. The send
// path must drop it: providers reject transcripts with orphan tool results.
func TestStreamAssistantResponseDropsOrphanToolResultsFromInitialContext(t *testing.T) {
	orphanCall, err := CreateToolCall("orphan_tool", map[string]any{})
	if err != nil {
		t.Fatalf("CreateToolCall: %v", err)
	}
	keptCall, err := CreateToolCall("kept_tool", map[string]any{})
	if err != nil {
		t.Fatalf("CreateToolCall: %v", err)
	}

	var got *ai.Context
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			got = prompt
			partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
			handler(ctx, ai.Start{Partial: partial})
			partial.Content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}
			final := partial
			final.StopReason = ai.StopReasonStop
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	assistantWithCall := &Message{
		Role: RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:      ai.ContentBlockToolCall,
			ID:        keptCall.ID,
			Name:      keptCall.Name,
			Arguments: keptCall.Arguments,
		}},
		Timestamp: time.Now(),
	}

	_, err = RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("continue after resume")},
		&AgentContext{
			Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			Messages: []*Message{
				NewToolMessage(orphanCall.ID, "orphan_tool", "result before the cut", false),
				assistantWithCall,
				NewToolMessage(keptCall.ID, "kept_tool", "kept result", false),
				NewUserMessage("continue after resume"),
			},
		},
		AgentLoopConfig{
			Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:  DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
		},
		func(Event) {},
	)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}
	if got == nil {
		t.Fatal("model was not called")
	}

	roles := make(map[string]bool)
	for _, m := range got.Messages {
		roles[m.Role] = true
		if m.Role == "toolResult" && strings.Contains(fmt.Sprintf("%v", m.Content), "result before the cut") {
			t.Fatalf("orphan tool result reached the provider: %#v", m)
		}
	}
	if !roles["toolResult"] {
		t.Fatal("kept tool result was dropped along with the orphan")
	}
}

// After a mid-turn compaction, the rebased transcript can begin with an
// assistant tool-call turn (the summary is lifted into the system prompt,
// orphan results are dropped, and no user message remains ahead of it).
// Providers reject a request whose first message is not from the user —
// zai/GLM returns error 1214 ("The messages parameter is illegal") — so the
// send path must prepend a synthetic user turn, like it already does for the
// summary-only case.
func TestStreamAssistantResponsePrependsUserWhenContextStartsWithAssistant(t *testing.T) {
	call, err := CreateToolCall("read", map[string]any{"path": "/tmp"})
	if err != nil {
		t.Fatalf("CreateToolCall: %v", err)
	}

	var got *ai.Context
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			got = prompt
			partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
			handler(ctx, ai.Start{Partial: partial})
			partial.Content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}
			final := partial
			final.StopReason = ai.StopReasonStop
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	summary := &Message{
		Role:      RoleSystem,
		Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "Session Summary: cut mid-turn"}},
		Timestamp: time.Now(),
	}
	assistantWithCall := &Message{
		Role: RoleAssistant,
		Content: []ai.ContentBlock{{
			Type:      ai.ContentBlockToolCall,
			ID:        call.ID,
			Name:      call.Name,
			Arguments: call.Arguments,
		}},
		Timestamp: time.Now(),
	}

	_, err = RunAgentLoopContinue(
		context.Background(),
		&AgentContext{
			Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			Messages: []*Message{
				summary,
				assistantWithCall,
				NewToolMessage(call.ID, "read", "file contents", false),
			},
		},
		AgentLoopConfig{
			Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:  DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
		},
		func(Event) {},
	)
	if err != nil {
		t.Fatalf("RunAgentLoopContinue: %v", err)
	}
	if got == nil {
		t.Fatal("model was not called")
	}
	if len(got.Messages) == 0 {
		t.Fatal("provider request had no messages")
	}
	if got.Messages[0].Role != "user" {
		t.Fatalf("first provider message role = %q, want user (assistant-first transcripts are rejected by providers)", got.Messages[0].Role)
	}
}

func TestRunAgentLoopUsageLimitWaitsAndRetries(t *testing.T) {
	oldBuffer := usageLimitRetryBuffer
	usageLimitRetryBuffer = time.Millisecond
	t.Cleanup(func() { usageLimitRetryBuffer = oldBuffer })

	var streamCalls int
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			streamCalls++
			if streamCalls == 1 {
				return &ai.CodexUsageLimitError{
					StatusCode: 429,
					Message:    "The usage limit has been reached",
					ResetsAt:   time.Now().Add(10 * time.Millisecond),
					ResetsIn:   10 * time.Millisecond,
				}
			}
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{
				Role:       "assistant",
				Timestamp:  time.Now().UnixMilli(),
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
				StopReason: ai.StopReasonStop,
			}})
			return nil
		},
	}

	var events []string
	result, err := RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI}},
		AgentLoopConfig{
			Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:  DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
		},
		func(event Event) { events = append(events, eventLabel(event)) },
	)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}
	if streamCalls != 2 {
		t.Fatalf("stream calls = %d, want 2 (initial + retry)", streamCalls)
	}
	if len(result) != 3 {
		t.Fatalf("len(result) = %d, want user + usage-limit error + assistant", len(result))
	}
	errMsg := result[1]
	if !errMsg.IsError || errMsg.Role != RoleAssistant {
		t.Fatalf("result[1] = %#v, want error assistant message", errMsg)
	}
	if !strings.Contains(errMsg.ErrorMessage, "usage limit") {
		t.Fatalf("error message = %q, want usage limit text", errMsg.ErrorMessage)
	}
	if !errMsg.ExcludeFromContext {
		t.Fatal("error message must be excluded from provider context")
	}
	if result[2].IsError || messageText(result[2]) != "ok" {
		t.Fatalf("result[2] = %#v, want successful assistant reply", result[2])
	}
	if strings.Count(strings.Join(events, ","), "message_end:assistant") != 2 {
		t.Fatalf("events = %v, want two assistant message_end events (error + retry)", events)
	}
	if !slices.Contains(events, "usage_limit_wait") {
		t.Fatalf("events = %v, want usage_limit_wait", events)
	}
}

func TestRunAgentLoopUsageLimitRetryExcludesErrorFromProviderContext(t *testing.T) {
	oldBuffer := usageLimitRetryBuffer
	usageLimitRetryBuffer = time.Millisecond
	t.Cleanup(func() { usageLimitRetryBuffer = oldBuffer })

	var sawMessages []ai.Message
	var streamCalls int
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			streamCalls++
			sawMessages = append(sawMessages, prompt.Messages...)
			if streamCalls == 1 {
				return &ai.ZAIUsageLimitError{StatusCode: 429, ResetsAt: time.Now().Add(10 * time.Millisecond)}
			}
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{
				Role:       "assistant",
				Timestamp:  time.Now().UnixMilli(),
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
				StopReason: ai.StopReasonStop,
			}})
			return nil
		},
	}

	if _, err := RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI}},
		AgentLoopConfig{
			Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:  DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
		},
		func(Event) {},
	); err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}

	for _, msg := range sawMessages[len(sawMessages)-1:] {
		if msg.IsError {
			t.Fatalf("retry request contained the usage-limit error message: %#v", msg)
		}
	}
}

func TestRunAgentLoopUsageLimitAbortDuringWait(t *testing.T) {
	oldBuffer := usageLimitRetryBuffer
	usageLimitRetryBuffer = time.Millisecond
	t.Cleanup(func() { usageLimitRetryBuffer = oldBuffer })

	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			return &ai.CodexUsageLimitError{StatusCode: 429, ResetsAt: time.Now().Add(time.Hour)}
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	result, err := RunAgentLoop(
		ctx,
		[]*Message{NewUserMessage("hi")},
		&AgentContext{Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI}},
		AgentLoopConfig{
			Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:  DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
		},
		func(Event) {},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline exceeded", err)
	}
	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want user + usage-limit error", len(result))
	}
}

func TestRunAgentLoopStreamUsageLimitWaitsAndRetries(t *testing.T) {
	oldBuffer := usageLimitRetryBuffer
	usageLimitRetryBuffer = time.Millisecond
	t.Cleanup(func() { usageLimitRetryBuffer = oldBuffer })

	var streamCalls int
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			streamCalls++
			if streamCalls == 1 {
				limitErr := &ai.ClaudeAgentUsageLimitError{Message: "session usage limit", ResetsAt: time.Now().Add(10 * time.Millisecond)}
				handler(ctx, ai.StreamError{Err: limitErr, Msg: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonError, ErrorMessage: limitErr.Error()}})
				return nil
			}
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{
				Role:       "assistant",
				Timestamp:  time.Now().UnixMilli(),
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
				StopReason: ai.StopReasonStop,
			}})
			return nil
		},
	}

	var events []string
	result, err := RunAgentLoop(
		context.Background(),
		[]*Message{NewUserMessage("hi")},
		&AgentContext{Model: &ai.Model{ID: "test", Provider: ai.ProviderOpenAI}},
		AgentLoopConfig{
			Model:         &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
			ConvertToLlm:  DefaultConvertToLlm,
			ClientFactory: func(model *ai.Model) (ai.Client, error) { return client, nil },
		},
		func(event Event) { events = append(events, eventLabel(event)) },
	)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}
	if streamCalls != 2 {
		t.Fatalf("stream calls = %d, want 2 (initial + retry)", streamCalls)
	}
	if len(result) != 3 {
		t.Fatalf("len(result) = %d, want user + usage-limit error + assistant", len(result))
	}
	errMsg := result[1]
	if !errMsg.IsError || errMsg.Role != RoleAssistant {
		t.Fatalf("result[1] = %#v, want error assistant message", errMsg)
	}
	if !strings.Contains(errMsg.ErrorMessage, "usage limit") {
		t.Fatalf("error message = %q, want usage limit text", errMsg.ErrorMessage)
	}
	if !errMsg.ExcludeFromContext {
		t.Fatal("error message must be excluded from provider context")
	}
	if result[2].IsError || messageText(result[2]) != "ok" {
		t.Fatalf("result[2] = %#v, want successful assistant reply", result[2])
	}
	if strings.Count(strings.Join(events, ","), "message_end:assistant") != 2 {
		t.Fatalf("events = %v, want two assistant message_end events (error + retry)", events)
	}
	if !slices.Contains(events, "usage_limit_wait") {
		t.Fatalf("events = %v, want usage_limit_wait", events)
	}
}
