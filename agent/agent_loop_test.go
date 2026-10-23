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
	"testing/synctest"
	"time"

	"github.com/losinggeneration/tau/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestExecuteParallelEmitsCompletionOrderToolEnd(t *testing.T) {
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
	if len(toolEndSeq) != 2 {
		t.Fatalf("toolEndSeq = %v, want [slow fast]", toolEndSeq)
	}
	if toolEndSeq[0] != "fast" || toolEndSeq[1] != "slow" {
		t.Fatalf("ToolEnd not in completion order: %v", toolEndSeq)
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

func TestRunAgentLoopDeliversFollowUpAfterEmptyAssistantResponse(t *testing.T) {
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
				if streamCalls > 1 {
					return nil, nil
				}
				return []*Message{NewUserMessage("follow up")}, nil
			},
		},
		func(Event) {},
	)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}
	if streamCalls != 2 {
		t.Fatalf("stream calls = %d, want the follow-up request", streamCalls)
	}
	if len(result) != 4 {
		t.Fatalf("len(result) = %d, want user, empty assistant, follow-up, assistant", len(result))
	}
	if result[1].StopReason != ai.StopReasonStop || result[1].IsError || result[1].ErrorMessage != "" {
		t.Fatalf("empty response = %+v, want a normal stop", result[1])
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
			EventDispatcher: func(ctx context.Context, name string, payload []byte, sessionID string) EventDispatchResult {
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
		EventDispatcher: func(ctx context.Context, name string, payload []byte, sessionID string) EventDispatchResult {
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
		EventDispatcher: func(ctx context.Context, name string, payload []byte, sessionID string) EventDispatchResult {
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
			capturedPrompt = ai.CurrentSystemMessage(ai.NormalizeContext(prompt).Messages).Content.(string)
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
	makeResponse := func(appendage string) func(ctx context.Context, name string, payload []byte, sessionID string) EventDispatchResult {
		return func(ctx context.Context, name string, payload []byte, _ string) EventDispatchResult {
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

	dispatcher := func(ctx context.Context, name string, payload []byte, sessionID string) EventDispatchResult {
		if name != "before_agent_start" {
			return EventDispatchResult{}
		}
		// Two "plugin" responses, in dispatch order: A then B.
		ra := makeResponse("[A]")(ctx, name, payload, sessionID)
		rb := makeResponse("[B]")(ctx, name, payload, sessionID)
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
			EventDispatcher: func(ctx context.Context, name string, _ []byte, _ string) EventDispatchResult {
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
			capturedPrompt = ai.CurrentSystemMessage(ai.NormalizeContext(prompt).Messages).Content.(string)
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

	dispatcher := func(ctx context.Context, name string, _ []byte, _ string) EventDispatchResult {
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

func TestFinishTurnEndsBeforePollingQueues(t *testing.T) {
	calls := 0
	polls := 0
	var order []string
	client := stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
		calls++
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}, StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := RunAgentLoop(context.Background(), []*Message{NewUserMessage("hi")}, &AgentContext{}, AgentLoopConfig{
		Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, ConvertToLlm: DefaultConvertToLlm,
		GetSteeringMessages: func(context.Context) ([]*Message, error) { polls++; return nil, nil },
		GetFollowUpMessages: func(context.Context) ([]*Message, error) { t.Fatal("follow-up polled after end"); return nil, nil },
		FinishTurn: func(ctx context.Context, turn AgentTurnContext) (TurnDecision, error) {
			order = append(order, "finish")
			return TurnEndRun, nil
		},
	}, func(event Event) {
		if _, ok := event.(TurnEnd); ok {
			order = append(order, "end")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || polls != 1 || !reflect.DeepEqual(order, []string{"finish", "end"}) {
		t.Fatalf("calls=%d polls=%d order=%v", calls, polls, order)
	}
}

func TestFinishTurnContinueGuaranteesOnePreparedRequest(t *testing.T) {
	calls := 0
	prepared := 0
	client := stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
		calls++
		if model.ID != "prepared" {
			t.Fatalf("model=%s", model.ID)
		}
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}, StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := RunAgentLoop(context.Background(), []*Message{NewUserMessage("hi")}, &AgentContext{}, AgentLoopConfig{
		Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, ConvertToLlm: DefaultConvertToLlm,
		PrepareRequest: func(ctx context.Context, request PrepareRequestContext) (*AgentRequestUpdate, error) {
			prepared++
			return &AgentRequestUpdate{Model: &ai.Model{ID: "prepared"}}, nil
		},
		FinishTurn: func(ctx context.Context, turn AgentTurnContext) (TurnDecision, error) {
			if calls == 1 {
				return TurnContinue, nil
			}
			return TurnDefault, nil
		},
	}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || prepared != 2 {
		t.Fatalf("calls=%d prepared=%d", calls, prepared)
	}
}

func TestUserBashVetoPreventsToolExecution(t *testing.T) {
	calls := 0
	executed := false
	client := stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
		calls++
		msg := ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}, StopReason: ai.StopReasonStop}
		if calls == 1 {
			msg.Content = []ai.ContentBlock{{Type: "toolCall", ID: "bash-1", Name: "bash", Arguments: json.RawMessage(`{"command":"echo unsafe"}`)}}
			msg.StopReason = ai.StopReasonToolUse
		}
		handler(ctx, ai.Done{Message: msg})
		return nil
	}}
	a, err := New(Config{
		Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil },
		Tools: []Tool{{Name: "bash", Parameters: map[string]any{}, Execute: func(ToolContext) (*ToolResult, error) { executed = true; return &ToolResult{}, nil }}},
		EventDispatcher: func(ctx context.Context, name string, payload []byte, sessionID string) EventDispatchResult {
			return EventDispatchResult{Blocked: name == "user_bash"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatal(err)
	}
	if executed {
		t.Fatal("bash executed despite veto")
	}
}

func TestToolDurationExcludesHookOverrides(t *testing.T) {
	result := executePreparedTool(context.Background(), &AgentContext{}, &Message{}, preparedToolCall{
		toolCall: ai.ToolCall{ID: "timed", Name: "test"},
		tool:     &Tool{Execute: func(ToolContext) (*ToolResult, error) { return &ToolResult{Content: "ok"}, nil }},
	}, AgentLoopConfig{AfterToolCall: func(context.Context, ToolCallContext, *ToolResult) (AfterToolCallResult, error) {
		return AfterToolCallResult{}, errors.New("hook failed")
	}}, func(Event) {})
	if result.DurationMs == nil || *result.DurationMs < 0 {
		t.Fatalf("missing execution timing: %+v", result)
	}
	result = finalizeToolResultMessage(ai.ToolCall{ID: "timed", Name: "test"}, result, func(Event) {})
	if result.Message.DurationMs == nil || *result.Message.DurationMs != *result.DurationMs {
		t.Fatal("timing not propagated to message")
	}
	blocked := finalizeToolResultMessage(ai.ToolCall{ID: "blocked"}, &ToolResult{IsError: true}, func(Event) {})
	if blocked.Message.DurationMs != nil {
		t.Fatal("unexecuted tool has timing")
	}
}

func TestFinishTurnCannotContinueProviderError(t *testing.T) {
	calls := 0
	finished := 0
	client := stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
		calls++
		handler(ctx, ai.StreamError{Msg: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonError, ErrorMessage: "failed"}})
		return nil
	}}
	_, err := RunAgentLoop(context.Background(), []*Message{NewUserMessage("hi")}, &AgentContext{}, AgentLoopConfig{
		Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, ConvertToLlm: DefaultConvertToLlm,
		FinishTurn:          func(context.Context, AgentTurnContext) (TurnDecision, error) { finished++; return TurnContinue, nil },
		GetFollowUpMessages: func(context.Context) ([]*Message, error) { t.Fatal("error polled queue"); return nil, nil },
	}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || finished != 1 {
		t.Fatalf("calls=%d finished=%d", calls, finished)
	}
}

func TestFailedTurnsFinishThenEndWithoutPollingQueues(t *testing.T) {
	for _, reason := range []ai.StopReason{ai.StopReasonError, ai.StopReasonAborted} {
		var order []string
		polls := 0
		client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
			handler(ctx, ai.StreamError{Reason: string(reason), Msg: ai.AssistantMessage{Role: "assistant", StopReason: reason, ErrorMessage: "failed"}})
			return nil
		}}
		_, err := RunAgentLoop(context.Background(), []*Message{NewUserMessage("hi")}, &AgentContext{}, AgentLoopConfig{
			Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, ConvertToLlm: DefaultConvertToLlm,
			GetSteeringMessages: func(context.Context) ([]*Message, error) { polls++; return nil, nil },
			GetFollowUpMessages: func(context.Context) ([]*Message, error) { polls++; return nil, nil },
			FinishTurn: func(_ context.Context, turn AgentTurnContext) (TurnDecision, error) {
				order = append(order, "finish:"+string(turn.Message.StopReason))
				return TurnContinue, nil
			},
			EventDispatcher: func(ctx context.Context, name string, _ []byte, _ string) EventDispatchResult {
				if name == "turn_end" {
					order = append(order, "dispatch")
				}
				return EventDispatchResult{}
			},
		}, func(event Event) {
			if _, ok := event.(TurnEnd); ok {
				order = append(order, "end")
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"finish:" + string(reason), "end", "dispatch"}
		if polls != 1 || !reflect.DeepEqual(order, want) {
			t.Fatalf("%s: polls=%d order=%v, want only the initial poll and %v", reason, polls, order, want)
		}
	}
}

func TestFinishTurnTreatsEmptyResponseAsNormalTurn(t *testing.T) {
	calls := 0
	var finished []*Message
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		calls++
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	a, err := New(Config{
		Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil },
		FinishTurn: func(_ context.Context, turn AgentTurnContext) (TurnDecision, error) {
			finished = append(finished, turn.Message)
			if calls == 1 {
				return TurnContinue, nil
			}
			return TurnDefault, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(finished) != 2 {
		t.Fatalf("calls=%d finished=%d, want one forced continuation", calls, len(finished))
	}
	for _, message := range finished {
		if message.IsError || message.StopReason != ai.StopReasonStop || message.ErrorMessage != "" {
			t.Fatalf("empty response finished as %+v", message)
		}
	}
	state := a.State()
	if len(state.Messages) != 3 || state.StreamingMessage != nil {
		t.Fatalf("messages=%d streaming=%v, want prompt and both replies", len(state.Messages), state.StreamingMessage)
	}
}

func TestPrepareNextTurnAdmitsSteeringQueuedDuringPreparation(t *testing.T) {
	var (
		requests [][]string
		queued   []*Message
		polls    int
	)
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		var texts []string
		for _, message := range prompt.Messages {
			texts = append(texts, fmt.Sprint(message.Content))
		}
		requests = append(requests, texts)
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: "text", Text: "ok"}}, StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := RunAgentLoop(context.Background(), []*Message{NewUserMessage("hi")}, &AgentContext{}, AgentLoopConfig{
		Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, ConvertToLlm: DefaultConvertToLlm,
		GetSteeringMessages: func(context.Context) ([]*Message, error) {
			polls++
			drained := queued
			queued = nil
			return drained, nil
		},
		FinishTurn: func(context.Context, AgentTurnContext) (TurnDecision, error) {
			if len(requests) == 1 {
				return TurnContinue, nil
			}
			return TurnDefault, nil
		},
		PrepareNextTurn: func(context.Context, AgentTurnContext) (*AgentTurnUpdate, error) {
			queued = append(queued, NewUserMessage("steer"))
			return nil, nil
		},
	}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || !strings.Contains(strings.Join(requests[1], ","), "steer") {
		t.Fatalf("requests=%v, want steering queued during preparation in the second request", requests)
	}
}

func TestToolExposureRejectsUnreachableModelCalls(t *testing.T) {
	for _, exposure := range []ToolExposure{ToolCodemode, ToolDeferred, ToolHidden, "invalid"} {
		tool := Tool{Name: "restricted", Exposure: exposure, Parameters: map[string]any{}, Execute: func(ToolContext) (*ToolResult, error) { return &ToolResult{Content: "unexpected"}, nil }}
		prepared := prepareToolCall(context.Background(), &AgentContext{Tools: []Tool{tool}}, &Message{}, ai.ToolCall{ID: "call", Name: tool.Name, Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
		if prepared.immediate == nil || !prepared.immediate.IsError {
			t.Fatalf("model reached %s tool", exposure)
		}
	}
}

func TestStructuredOutputPreservedAndClearedByContentOverride(t *testing.T) {
	tool := &Tool{Name: "structured", OutputSchema: map[string]any{"type": "array"}, Execute: func(ToolContext) (*ToolResult, error) {
		return &ToolResult{Content: "ok", StructuredContent: json.RawMessage(`[1,2]`)}, nil
	}}
	prepared := preparedToolCall{tool: tool, toolCall: ai.ToolCall{ID: "call", Name: tool.Name}}
	result := executePreparedTool(context.Background(), &AgentContext{}, &Message{}, prepared, AgentLoopConfig{}, func(Event) {})
	if result.IsError || string(result.StructuredContent) != `[1,2]` {
		t.Fatalf("result=%+v", result)
	}
	result = executePreparedTool(context.Background(), &AgentContext{}, &Message{}, prepared, AgentLoopConfig{AfterToolCall: func(context.Context, ToolCallContext, *ToolResult) (AfterToolCallResult, error) {
		return AfterToolCallResult{Content: "changed"}, nil
	}}, func(Event) {})
	if result.StructuredContent != nil {
		t.Fatal("stale structured result retained")
	}
	tool.Execute = func(ToolContext) (*ToolResult, error) {
		return &ToolResult{StructuredContent: json.RawMessage(`42`)}, nil
	}
	result = executePreparedTool(context.Background(), &AgentContext{}, &Message{}, prepared, AgentLoopConfig{}, func(Event) {})
	if !result.IsError {
		t.Fatal("invalid output schema accepted")
	}
}

func TestPrepareNextTurnAndRawProviderEvents(t *testing.T) {
	calls := 0
	observed := 0
	client := stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
		calls++
		if calls == 2 && model.ID != "next" {
			t.Fatal("next-turn model not applied")
		}
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: "text", Text: "ok"}}, StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := RunAgentLoop(context.Background(), []*Message{NewUserMessage("hi")}, &AgentContext{}, AgentLoopConfig{
		Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, ConvertToLlm: DefaultConvertToLlm,
		OnProviderStreamEvent: func(context.Context, ai.StreamEvent) { observed++ },
		FinishTurn: func(context.Context, AgentTurnContext) (TurnDecision, error) {
			if calls == 1 {
				return TurnContinue, nil
			}
			return TurnDefault, nil
		},
		PrepareNextTurn: func(context.Context, AgentTurnContext) (*AgentTurnUpdate, error) {
			return &AgentTurnUpdate{Model: &ai.Model{ID: "next"}, Messages: []*Message{NewUserMessage("next")}}, nil
		},
	}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || observed != 2 {
		t.Fatalf("calls=%d observed=%d", calls, observed)
	}
}

func TestInputHookTransformsBeforeProvider(t *testing.T) {
	var received string
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		received = fmt.Sprint(prompt.Messages[len(prompt.Messages)-1].Content)
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: "text", Text: "ok"}}}})
		return nil
	}}
	a, err := New(Config{Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, EventDispatcher: func(ctx context.Context, name string, _ []byte, _ string) EventDispatchResult {
		if name == "input" {
			return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: []byte(`{"action":"transform","text":"rewritten"}`)}}}
		}
		return EventDispatchResult{}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(context.Background(), NewUserMessage("original")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(received, "rewritten") || strings.Contains(received, "original") {
		t.Fatalf("input ignored: %s", received)
	}
}

func TestBeforeSettleAdmitsAnotherTurn(t *testing.T) {
	calls := 0
	settlements := 0
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		calls++
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: "text", Text: "ok"}}}})
		return nil
	}}
	a, err := New(Config{Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, EventDispatcher: func(ctx context.Context, name string, _ []byte, _ string) EventDispatchResult {
		if name == "agent_before_settle" {
			settlements++
			if settlements == 1 {
				return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: []byte(`{"continue":true}`)}}}
			}
		}
		return EventDispatchResult{}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || settlements != 2 {
		t.Fatalf("calls %d settlements %d", calls, settlements)
	}
}

func TestRunToolCallExecutesHiddenToolWithoutTranscriptEvents(t *testing.T) {
	tool := Tool{Name: "hidden", Exposure: ToolHidden, Parameters: map[string]any{"type": "object"}, Execute: func(ctx ToolContext) (*ToolResult, error) { return &ToolResult{Content: "nested"}, nil }}
	result := RunToolCall(context.Background(), &AgentContext{Tools: []Tool{tool}}, nil, ai.ToolCall{ID: "parent/1", Name: "hidden", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if result.IsError || result.Content != "nested" {
		t.Fatalf("nested call: %#v", result)
	}
}

func TestToolProgressAfterCompletionIsIgnored(t *testing.T) {
	var update func(*ToolResult)
	updates := 0
	tool := Tool{Execute: func(ctx ToolContext) (*ToolResult, error) {
		update = ctx.OnUpdate
		ctx.OnUpdate(&ToolResult{Content: "during"})
		return &ToolResult{}, nil
	}}
	executePreparedTool(context.Background(), &AgentContext{}, nil, preparedToolCall{tool: &tool}, AgentLoopConfig{}, func(Event) { updates++ })
	before := updates
	update(&ToolResult{Content: "late"})
	if updates != before {
		t.Fatal("late progress emitted")
	}
}

func TestRunToolCallWithoutExecutorReturnsError(t *testing.T) {
	result := RunToolCall(context.Background(), &AgentContext{Tools: []Tool{{Name: "unavailable", Parameters: map[string]any{"type": "object"}}}}, nil, ai.ToolCall{Name: "unavailable", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if !result.IsError || result.DurationMs != nil {
		t.Fatalf("unavailable tool: %#v", result)
	}
}

func TestContextHookCannotDiscardTranscriptSystemChanges(t *testing.T) {
	model := &ai.Model{ID: "test"}
	seen := false
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		for _, msg := range ai.NormalizeContext(prompt).Messages {
			if msg.Sections["policy"] != nil && *msg.Sections["policy"] == "retain" {
				seen = true
			}
		}
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := streamAssistantResponse(context.Background(), &AgentContext{Messages: []*Message{{Role: RoleSystem, Sections: map[string]*string{"policy": new("retain")}}, NewUserMessage("hi")}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm, EventDispatcher: func(ctx context.Context, name string, payload []byte, _ string) EventDispatchResult {
		if name != "context" {
			return EventDispatchResult{}
		}
		var data struct {
			Messages []ai.Message `json:"messages"`
		}
		if err := json.Unmarshal(payload, &data); err != nil {
			t.Fatal(err)
		}
		for _, msg := range data.Messages {
			if msg.Role == "system" {
				t.Error("context hook received system state")
			}
		}
		return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: json.RawMessage(`{"messages":[{"role":"user","content":"rewritten"}]}`)}}}
	}}, client, model, "base", nil, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("context hook discarded system changes")
	}
}

func TestRunToolCallForwardsProgressWithoutLifecycleEvents(t *testing.T) {
	var late func(*ToolResult)
	var updates []string
	tool := Tool{Name: "nested", Parameters: map[string]any{"type": "object"}, Execute: func(ctx ToolContext) (*ToolResult, error) {
		late = ctx.OnUpdate
		ctx.OnUpdate(&ToolResult{Content: "progress"})
		return &ToolResult{Content: "done"}, nil
	}}
	result := RunToolCall(context.Background(), &AgentContext{Tools: []Tool{tool}}, nil, ai.ToolCall{Name: "nested", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{}, func(result *ToolResult) { updates = append(updates, result.Content) })
	late(&ToolResult{Content: "late"})
	if result.IsError || result.Content != "done" || !slices.Equal(updates, []string{"progress"}) {
		t.Fatalf("result=%+v updates=%v", result, updates)
	}
}

func TestStreamAssistantResponseRejectsImageLimitBeforeProvider(t *testing.T) {
	called := false
	client := stubClient{stream: func(context.Context, *ai.Model, *ai.Context, ai.EventHandler, ...ai.Option) error {
		called = true
		return nil
	}}
	model := &ai.Model{InputLimits: &ai.ModelInputLimits{Images: &ai.ModelImageInputLimits{MaxPerRequest: 1}}}
	image := ai.ContentBlock{Type: ai.ContentBlockImage}
	_, err := streamAssistantResponse(context.Background(), &AgentContext{Messages: []*Message{{Role: RoleUser, Content: []ai.ContentBlock{image}}, {Role: RoleUser, Content: []ai.ContentBlock{image}}}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, model, "", nil, func(Event) {})
	if err == nil || called {
		t.Fatalf("err=%v provider called=%v", err, called)
	}
}

func TestStreamAssistantResponseRequiresTerminalEvent(t *testing.T) {
	client := stubClient{stream: func(context.Context, *ai.Model, *ai.Context, ai.EventHandler, ...ai.Option) error { return nil }}
	_, err := streamAssistantResponse(context.Background(), &AgentContext{Messages: []*Message{NewUserMessage("hi")}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{}, "", nil, func(Event) {})
	if err == nil {
		t.Fatal("unterminated stream accepted as the prior user message")
	}
}

func TestStreamAssistantResponseNormalizesTerminalOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event ai.StreamEvent
		want  ai.StopReason
	}{
		{name: "empty done", event: ai.Done{Reason: ai.StopReasonLength}, want: ai.StopReasonLength},
		{name: "bare error", event: ai.StreamError{Err: errors.New("server_busy")}, want: ai.StopReasonError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
				handler(ctx, tc.event)
				return nil
			}}
			message, err := streamAssistantResponse(context.Background(), &AgentContext{Messages: []*Message{NewUserMessage("hi")}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{}, "", nil, func(Event) {})
			if err != nil {
				t.Fatal(err)
			}
			if message.Role != RoleAssistant || message.StopReason != tc.want {
				t.Fatalf("outcome=%+v", message)
			}
			if tc.name == "bare error" && message.ErrorMessage != "server_busy" {
				t.Fatalf("error=%q", message.ErrorMessage)
			}
		})
	}
}

func TestConvertMessagesToAIPreservesTranscriptChanges(t *testing.T) {
	message := &Message{Role: RoleSystem, Sections: map[string]*string{"policy": new("keep")}, ToolsAdded: []ai.Tool{{Name: "read"}}, ToolsRemoved: []ai.ToolReference{{Name: "old"}}}
	converted := ConvertMessagesToAI([]*Message{message})
	if len(converted) != 1 || !reflect.DeepEqual(converted[0].Sections, message.Sections) || !reflect.DeepEqual(converted[0].ToolsAdded, message.ToolsAdded) || !reflect.DeepEqual(converted[0].ToolsRemoved, message.ToolsRemoved) {
		t.Fatalf("transcript changes lost: %+v", converted)
	}
}

func TestExcludedTranscriptChangesDoNotReachProvider(t *testing.T) {
	seen := false
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		for _, message := range ai.NormalizeContext(prompt).Messages {
			if message.Sections["excluded"] != nil {
				seen = true
			}
		}
		handler(ctx, ai.Done{Reason: ai.StopReasonStop})
		return nil
	}}
	_, err := streamAssistantResponse(context.Background(), &AgentContext{Messages: []*Message{{Role: RoleSystem, ExcludeFromContext: true, Sections: map[string]*string{"excluded": new("hidden")}}, NewUserMessage("hi")}}, AgentLoopConfig{ConvertToLlm: ConvertAllMessagesForTest}, client, &ai.Model{}, "", nil, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Fatal("excluded transcript state reached provider")
	}
}

func ConvertAllMessagesForTest(_ context.Context, messages []*Message) ([]ai.Message, error) {
	return ConvertMessagesToAI(messages), nil
}

func TestTruncatedAssistantToolCallsNeverExecute(t *testing.T) {
	calls := 0
	tool := Tool{Name: "write", Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { calls++; return &ToolResult{Content: "written"}, nil }}
	results := executeSequential(context.Background(), &AgentContext{Tools: []Tool{tool}}, &Message{Role: RoleAssistant, StopReason: ai.StopReasonLength}, []ai.ToolCall{{ID: "call", Name: "write", Arguments: json.RawMessage(`{}`)}}, AgentLoopConfig{}, func(Event) {})
	if calls != 0 || len(results) != 1 || !results[0].IsError || results[0].Terminate {
		t.Fatalf("truncated call executed: calls=%d results=%+v", calls, results)
	}
}

func TestCancellationAfterBeforeToolHookPreventsExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	tool := Tool{Name: "write", Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { calls++; return &ToolResult{Content: "written"}, nil }}
	result := RunToolCall(ctx, &AgentContext{Tools: []Tool{tool}}, &Message{Role: RoleAssistant}, ai.ToolCall{Name: "write", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{BeforeToolCall: func(context.Context, ToolCallContext) (BeforeToolCallResult, error) {
		cancel()
		return BeforeToolCallResult{}, nil
	}})
	if calls != 0 || !result.IsError || result.DurationMs != nil {
		t.Fatalf("canceled hook admitted execution: calls=%d result=%+v", calls, result)
	}
}

func TestParallelAbortStopsPreflightAndPreservesArtifactOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tool := Tool{Name: "write", Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { t.Error("canceled tool executed"); return nil, nil }}
	var events []string
	results := executeParallel(ctx, &AgentContext{Tools: []Tool{tool}}, nil, []ai.ToolCall{{ID: "first", Name: "write", Arguments: json.RawMessage(`{}`)}, {ID: "trailing", Name: "write", Arguments: json.RawMessage(`{}`)}}, AgentLoopConfig{BeforeToolCall: func(context.Context, ToolCallContext) (BeforeToolCallResult, error) {
		cancel()
		return BeforeToolCallResult{}, nil
	}}, func(event Event) {
		switch e := event.(type) {
		case ToolStart:
			events = append(events, "start:"+e.ToolCallID)
		case ToolEnd:
			if e.DurationMs != nil {
				t.Error("cancelled preflight acquired execution timing")
			}
			events = append(events, "end:"+e.ToolCallID)
		case MessageEnd:
			events = append(events, "message:"+e.Message.ToolCallID)
		}
	})
	for _, result := range results {
		if result.DurationMs != nil {
			t.Error("unexecuted result acquired persisted timing")
		}
	}
	if len(results) != 1 || !results[0].IsError || !slices.Equal(events, []string{"start:first", "end:first", "message:first"}) {
		t.Fatalf("aborted batch: events=%v results=%+v", events, results)
	}
}

func TestFinalizedAssistantRecordsRequestedThinkingLevel(t *testing.T) {
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		handler(ctx, ai.Done{Reason: ai.StopReasonStop})
		return nil
	}}
	message, err := streamAssistantResponse(context.Background(), &AgentContext{Messages: []*Message{NewUserMessage("hi")}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm, ThinkingLevel: ThinkingOff}, client, &ai.Model{API: "api", Provider: "provider", ID: "model"}, "", nil, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if message.ThinkingLevel != ThinkingOff {
		t.Fatalf("thinking level=%q", message.ThinkingLevel)
	}
	if message.API != "api" || message.Provider != "provider" || message.Model != "model" {
		t.Fatalf("response did not record the model that answered: %+v", message)
	}
}

func TestDefaultConversionPreservesAssistantSourceMetadata(t *testing.T) {
	message := &Message{Role: RoleAssistant, Provider: "source-provider", API: "source-api", Model: "source-model", StopReason: ai.StopReasonLength, DurationMs: new(4.5)}
	converted, err := DefaultConvertToLlm(context.Background(), []*Message{message})
	if err != nil {
		t.Fatal(err)
	}
	if len(converted) != 1 || converted[0].Provider != message.Provider || converted[0].API != message.API || converted[0].Model != message.Model || converted[0].StopReason != string(message.StopReason) || converted[0].DurationMs == nil {
		t.Fatalf("source metadata lost: %+v", converted)
	}
}

func TestBuildAIOptionsExplicitlyForwardsOffThinking(t *testing.T) {
	model := &ai.Model{Reasoning: true, SamplingParamsByThinkingLevel: map[string]map[string]any{"off": {"temperature": 0.4}}}
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, options ...ai.Option) error {
		if len(options) == 0 {
			t.Error("explicit off reasoning was not forwarded")
		}
		handler(ctx, ai.Done{Reason: ai.StopReasonStop})
		return nil
	}}
	_, err := streamAssistantResponse(context.Background(), &AgentContext{Messages: []*Message{NewUserMessage("hi")}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, model, "", buildAIOptions(AgentLoopConfig{ThinkingLevel: ThinkingOff}, model), func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUserBashOverridePreventsToolExecution(t *testing.T) {
	for _, raw := range []string{`{"result":{"output":"remote output","exitCode":0}}`, `{"operations":{}}`, `{"result":{}}`, `{`} {
		t.Run(raw, func(t *testing.T) {
			calls, executions := 0, 0
			client := stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
				calls++
				message := ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}
				if calls == 1 {
					message.Content = []ai.ContentBlock{{Type: ai.ContentBlockToolCall, ID: "bash-1", Name: "bash", Arguments: json.RawMessage(`{"command":"unsafe"}`)}}
					message.StopReason = ai.StopReasonToolUse
				}
				handler(ctx, ai.Done{Message: message})
				return nil
			}}
			a, err := New(Config{Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, Tools: []Tool{{Name: "bash", Parameters: map[string]any{}, Execute: func(ToolContext) (*ToolResult, error) { executions++; return &ToolResult{}, nil }}}, EventDispatcher: func(ctx context.Context, name string, _ []byte, _ string) EventDispatchResult {
				if name == "user_bash" {
					return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: []byte(raw)}}}
				}
				return EventDispatchResult{}
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Prompt(t.Context(), NewUserMessage("hi")); err != nil {
				t.Fatal(err)
			}
			if executions != 0 {
				t.Fatal("override allowed local shell execution")
			}
			var result *Message
			for _, message := range a.Messages() {
				if message.Role == RoleTool {
					result = message
				}
			}
			if result == nil {
				t.Fatal("missing tool outcome")
			}
			if raw == `{"result":{"output":"remote output","exitCode":0}}` {
				if result.IsError || result.Content[0].Text != "remote output" {
					t.Fatalf("result: %+v", result)
				}
			} else if !result.IsError {
				t.Fatal("malformed override was not an error")
			}
		})
	}
}

func TestProviderTranscriptChangesRetainSourceOrder(t *testing.T) {
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		var sequence []string
		for _, message := range ai.NormalizeContext(prompt).Messages {
			if len(message.ToolsAdded) > 0 {
				sequence = append(sequence, "add:"+message.ToolsAdded[0].Name)
			}
			if len(message.ToolsRemoved) > 0 {
				sequence = append(sequence, "remove:"+message.ToolsRemoved[0].Name)
			}
			if message.Role == "user" {
				sequence = append(sequence, "user")
			}
		}
		if !slices.Equal(sequence, []string{"add:first", "user", "remove:first", "add:second", "user"}) {
			t.Errorf("transcript reordered: %v", sequence)
		}
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := streamAssistantResponse(t.Context(), &AgentContext{Tools: []Tool{{Name: "first"}, {Name: "second"}}, Messages: []*Message{{Role: RoleSystem, ToolsAdded: []ai.Tool{{Name: "first"}}}, NewUserMessage("before"), {Role: RoleSystem, ToolsRemoved: []ai.ToolReference{{Name: "first"}}}, {Role: RoleSystem, ToolsAdded: []ai.Tool{{Name: "second"}}}, NewUserMessage("after")}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{}, "", nil, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCustomRoleSkipsUserInputAndStartsCustomRun(t *testing.T) {
	var inputCalled, customStartup bool
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		if len(prompt.Messages) != 1 || prompt.Messages[0].Role != "user" {
			t.Fatalf("custom provider prompt = %+v", prompt.Messages)
		}
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}}})
		return nil
	}}
	_, err := RunAgentLoop(t.Context(), []*Message{{Role: RoleCustom, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "extension context"}}}}, &AgentContext{}, AgentLoopConfig{
		Model: &ai.Model{ID: "test"}, ConvertToLlm: DefaultConvertToLlm, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil },
		EventDispatcher: func(ctx context.Context, name string, payload []byte, _ string) EventDispatchResult {
			if name == "input" {
				inputCalled = true
			}
			if name == "before_agent_start" {
				var startup struct {
					MessageType string `json:"messageType"`
				}
				if err := json.Unmarshal(payload, &startup); err != nil {
					t.Fatal(err)
				}
				customStartup = startup.MessageType == "custom"
			}
			return EventDispatchResult{}
		},
	}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if inputCalled || !customStartup {
		t.Fatalf("custom startup=%v input=%v", customStartup, inputCalled)
	}
}

func TestSequentialToolEndIncludesExecutionDuration(t *testing.T) {
	tool := Tool{Name: "echo", Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { return &ToolResult{Content: "ok"}, nil }}
	var ended *ToolEnd
	results := executeSequential(t.Context(), &AgentContext{Tools: []Tool{tool}}, nil, []ai.ToolCall{{ID: "call", Name: "echo", Arguments: json.RawMessage(`{}`)}}, AgentLoopConfig{}, func(event Event) {
		if end, ok := event.(ToolEnd); ok {
			ended = &end
		}
	})
	if ended == nil || ended.DurationMs == nil || len(results) != 1 || results[0].DurationMs == nil {
		t.Fatalf("end=%+v results=%+v", ended, results)
	}
	if *ended.DurationMs != *results[0].DurationMs {
		t.Fatal("event duration differs from persisted result")
	}
}

func TestStartupSystemSectionsSurviveToolContinuation(t *testing.T) {
	var requests int
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		requests++
		var found bool
		for _, message := range prompt.Messages {
			if section := message.Sections["startup-policy"]; section != nil && *section == "retain" {
				found = true
			}
		}
		if !found {
			t.Errorf("request %d lost startup system section", requests)
		}
		final := ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}}
		if requests == 1 {
			final.StopReason = ai.StopReasonToolUse
			final.Content = []ai.ContentBlock{{Type: ai.ContentBlockToolCall, ID: "call", Name: "work", Arguments: json.RawMessage(`{}`)}}
		}
		handler(ctx, ai.Done{Message: final, Reason: final.StopReason})
		return nil
	}}
	tool := Tool{Name: "work", Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { return &ToolResult{Content: "done"}, nil }}
	_, err := RunAgentLoop(t.Context(), []*Message{{Role: RoleCustom, CustomType: "trigger"}}, &AgentContext{Tools: []Tool{tool}}, AgentLoopConfig{
		Model: &ai.Model{ID: "test"}, ConvertToLlm: DefaultConvertToLlm, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil },
		EventDispatcher: func(ctx context.Context, name string, _ []byte, _ string) EventDispatchResult {
			if name == "before_agent_start" {
				return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: []byte(`{"messages":[{"role":"system","sections":{"startup-policy":"retain"}}]}`)}}}
			}
			return EventDispatchResult{}
		},
	}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestRequestPreservesCanonicalSystemEntryOrder(t *testing.T) {
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		messages := ai.NormalizeContext(prompt).Messages
		var system []ai.Message
		for _, message := range messages {
			if message.Role == "system" {
				system = append(system, message)
			}
		}
		if len(system) != 3 || system[0].Content != "base" || system[1].ContentBlocks()[0].Text != "first" || system[2].ContentBlocks()[0].Text != "second" {
			t.Fatalf("reordered system entries: %+v", system)
		}
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := streamAssistantResponse(t.Context(), &AgentContext{Messages: []*Message{
		{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "first"}}, Sections: map[string]*string{"policy": new("one")}},
		NewUserMessage("hi"),
		{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "second"}}, Sections: map[string]*string{"policy": new("two")}},
	}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{ID: "test"}, "base", nil, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExcludedSystemMessageDoesNotEnterRequestPrompt(t *testing.T) {
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		baseline := ai.CurrentSystemMessage(ai.NormalizeContext(prompt).Messages)
		if baseline != nil && strings.Contains(baseline.Content.(string), "omit this") {
			t.Fatalf("excluded system text leaked: %+v", baseline)
		}
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := streamAssistantResponse(t.Context(), &AgentContext{Messages: []*Message{
		{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "omit this"}}, ExcludeFromContext: true}, NewUserMessage("hi"),
	}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{ID: "test"}, "base", nil, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTranscriptToolRemovalControlsExecution(t *testing.T) {
	for _, mode := range []string{"sequential", "parallel"} {
		for _, scenario := range []struct {
			name         string
			changes      []*Message
			wantCalls    int
			unregistered bool
			exposure     ToolExposure
		}{
			{name: "unregistered declaration", changes: []*Message{{Role: RoleSystem, ToolsAdded: []ai.Tool{{Name: "read"}}}}, unregistered: true},
			{name: "deferred declaration", changes: []*Message{{Role: RoleSystem, ToolsAdded: []ai.Tool{{Name: "read"}}}}, exposure: ToolDeferred, wantCalls: 1},
			{name: "removed", changes: []*Message{{Role: RoleSystem, ToolsRemoved: []ai.ToolReference{{Name: "read"}}}}},
			{name: "restored", changes: []*Message{{Role: RoleSystem, ToolsRemoved: []ai.ToolReference{{Name: "read"}}}, {Role: RoleSystem, ToolsAdded: []ai.Tool{{Name: "read"}}}}, wantCalls: 1},
			{name: "excluded removal", changes: []*Message{{Role: RoleSystem, ExcludeFromContext: true, ToolsRemoved: []ai.ToolReference{{Name: "read"}}}}, wantCalls: 1},
		} {
			t.Run(mode+"/"+scenario.name, func(t *testing.T) {
				calls := 0
				tool := Tool{Name: "read", Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { calls++; return &ToolResult{Content: "ok"}, nil }}
				tool.Exposure = scenario.exposure
				ctx := &AgentContext{Tools: []Tool{tool}, Messages: scenario.changes}
				if scenario.unregistered {
					ctx.Tools = nil
				}
				execute := executeSequential
				if mode == "parallel" {
					execute = executeParallel
				}
				results := execute(context.Background(), ctx, &Message{Role: RoleAssistant}, []ai.ToolCall{{ID: "call", Name: "read", Arguments: json.RawMessage(`{}`)}}, AgentLoopConfig{}, func(Event) {})
				if calls != scenario.wantCalls || len(results) != 1 || results[0].IsError != (scenario.wantCalls == 0) {
					t.Fatalf("calls=%d results=%+v", calls, results)
				}
			})
		}
	}
}

func TestRequestKeepsPlainSystemTextAfterRichEntries(t *testing.T) {
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		baseline := ai.CurrentSystemMessage(ai.NormalizeContext(prompt).Messages)
		if baseline == nil || baseline.Content != "base\n\nfirst\n\nlast" {
			t.Fatalf("system text reordered: %+v", baseline)
		}
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := streamAssistantResponse(t.Context(), &AgentContext{Messages: []*Message{
		{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "first"}}, Sections: map[string]*string{"policy": new("one")}},
		NewUserMessage("hello"),
		{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "last"}}},
	}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{ID: "test"}, "base", nil, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRestoredToolDeclarationsRespectRegistryExposure(t *testing.T) {
	for _, exposure := range []ToolExposure{"", ToolDirect, ToolModelOnly, ToolHidden, ToolDeferred, ToolCodemode, "unregistered"} {
		t.Run(string(exposure), func(t *testing.T) {
			tool := Tool{Name: "restored", Exposure: exposure, Parameters: map[string]any{"type": "object"}}
			tools := []Tool{tool}
			if exposure == "unregistered" {
				tools = nil
			}
			client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
				baseline := ai.CurrentSystemMessage(ai.NormalizeContext(prompt).Messages)
				visible := baseline != nil && len(baseline.ToolsAdded) > 0
				wantVisible := exposure == "" || exposure == ToolDirect || exposure == ToolModelOnly || exposure == ToolDeferred || exposure == ToolCodemode
				if visible != wantVisible {
					t.Errorf("visible=%v, want %v: %+v", visible, wantVisible, baseline)
				}
				handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
				return nil
			}}
			restored := &Message{Role: RoleSystem, ToolsAdded: []ai.Tool{{Name: "restored", Description: "persisted declaration"}}}
			_, err := streamAssistantResponse(t.Context(), &AgentContext{Tools: tools, Messages: []*Message{restored, NewUserMessage("hello")}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{ID: "test"}, "", nil, func(Event) {})
			if len(restored.ToolsAdded) != 1 || restored.ToolsAdded[0].Description != "persisted declaration" {
				t.Fatal("request filtering mutated stored declarations")
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSystemContextHookCannotExposeUnavailableTools(t *testing.T) {
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		baseline := ai.CurrentSystemMessage(ai.NormalizeContext(prompt).Messages)
		if baseline == nil || len(baseline.ToolsAdded) != 0 {
			t.Fatalf("hook exposed unavailable tools: %+v", baseline)
		}
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := streamAssistantResponse(t.Context(), &AgentContext{Tools: []Tool{{Name: "hidden", Exposure: ToolHidden}}, Messages: []*Message{NewUserMessage("hello")}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm, EventDispatcher: func(ctx context.Context, name string, _ []byte, _ string) EventDispatchResult {
		if name == "context_with_system" {
			return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: []byte(`{"messages":[{"role":"system","toolsAdded":[{"name":"hidden"},{"name":"unregistered"}]},{"role":"user","content":"hello"}]}`)}}}
		}
		return EventDispatchResult{}
	}}, client, &ai.Model{ID: "test"}, "", nil, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContextHookPreservesInterleavedSystemEntries(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		response []byte
		want     []string
	}{
		{name: "identity", want: []string{"first", "user", "second", "user"}},
		{name: "omit", response: []byte(`{"messages":[]}`), want: []string{"first", "second"}},
		{name: "shorten", response: []byte(`{"messages":[{"role":"user","content":"replacement"}]}`), want: []string{"first", "user", "second"}},
		{name: "extend", response: []byte(`{"messages":[{"role":"user","content":"one"},{"role":"user","content":"two"},{"role":"user","content":"three"}]}`), want: []string{"first", "user", "second", "user", "user"}},
		{name: "reject system injection", response: []byte(`{"messages":[{"role":"system","sections":{"policy":"injected"}},{"role":"user","content":"replacement"}]}`), want: []string{"first", "user", "second"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
				var order []string
				for _, message := range ai.NormalizeContext(prompt).Messages {
					if message.Role == "system" && message.Sections["policy"] != nil {
						order = append(order, *message.Sections["policy"])
					}
					if message.Role == "user" {
						order = append(order, "user")
					}
				}
				if !slices.Equal(order, scenario.want) {
					t.Errorf("hook reordered transcript: %v, want %v", order, scenario.want)
				}
				handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
				return nil
			}}
			_, err := streamAssistantResponse(t.Context(), &AgentContext{Messages: []*Message{
				{Role: RoleSystem, Sections: map[string]*string{"policy": new("first")}},
				NewUserMessage("old"),
				{Role: RoleSystem, Sections: map[string]*string{"policy": new("second")}},
				NewUserMessage("new"),
			}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm, EventDispatcher: func(ctx context.Context, name string, payload []byte, _ string) EventDispatchResult {
				if name == "context" {
					response := scenario.response
					if response == nil {
						response = payload
					}
					return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: response}}}
				}
				return EventDispatchResult{}
			}}, client, &ai.Model{ID: "test"}, "", nil, func(Event) {})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestToolOutputValidationUsesFinalHookResult(t *testing.T) {
	for _, scenario := range []struct {
		name                 string
		initial, replacement json.RawMessage
		wantError            bool
	}{
		{name: "hook invalidates output", initial: json.RawMessage(`[1]`), replacement: json.RawMessage(`{"wrong":true}`), wantError: true},
		{name: "hook repairs output", initial: json.RawMessage(`{"wrong":true}`), replacement: json.RawMessage(`[1]`)},
		{name: "arbitrary JSON without schema", initial: json.RawMessage(`[1]`), replacement: json.RawMessage(`42`)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			tool := Tool{Name: "structured", Parameters: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "array"}, Execute: func(ToolContext) (*ToolResult, error) {
				return &ToolResult{Content: "output", StructuredContent: scenario.initial}, nil
			}}
			if scenario.name == "arbitrary JSON without schema" {
				tool.OutputSchema = nil
			}
			result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{tool}}, nil, ai.ToolCall{Name: "structured", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{AfterToolCall: func(context.Context, ToolCallContext, *ToolResult) (AfterToolCallResult, error) {
				return AfterToolCallResult{StructuredContent: scenario.replacement}, nil
			}})
			if result.IsError != scenario.wantError {
				t.Fatalf("final result error=%v, want %v: %+v", result.IsError, scenario.wantError, result)
			}
			expected := scenario.replacement
			if scenario.wantError {
				expected = nil
			}
			if string(result.StructuredContent) != string(expected) {
				t.Fatalf("final structured output=%s, want %s", result.StructuredContent, expected)
			}
		})
	}
}

func TestOutputSchemaFailureReplacesRichContent(t *testing.T) {
	tool := Tool{Name: "structured", Parameters: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "array"}, Execute: func(ToolContext) (*ToolResult, error) {
		return &ToolResult{ContentBlocks: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "claimed success"}}, StructuredContent: json.RawMessage(`42`)}, nil
	}}
	result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{tool}}, nil, ai.ToolCall{Name: "structured", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if !result.IsError || len(result.NormalizedContent()) != 1 || !strings.Contains(result.NormalizedContent()[0].Text, "does not match schema") {
		t.Fatalf("schema diagnostic hidden by stale content: %+v", result)
	}
}

func TestConvertMessagesToAISkipsNilTranscriptEntries(t *testing.T) {
	messages := ConvertMessagesToAI([]*Message{nil, NewUserMessage("kept"), nil})
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("converted messages: %+v", messages)
	}
}

func TestBeforeAgentStartOverrideProjectsHistoricalSystems(t *testing.T) {
	for _, forced := range []string{"forced policy", "", "system-aware replacement"} {
		t.Run(fmt.Sprintf("forced=%q", forced), func(t *testing.T) {
			section := "historical section"
			history := &Message{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "historical policy"}}, Sections: map[string]*string{"policy": &section}, ToolsAdded: []ai.Tool{{Name: "available"}}, Timestamp: time.UnixMilli(1700000000123)}
			var requests int
			client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
				requests++
				normalized := ai.NormalizeContext(prompt)
				current := ai.CurrentSystemMessage(normalized.Messages)
				expected := forced
				if forced == "system-aware replacement" {
					expected = "authoritative hook"
				}
				if current == nil || current.Content != expected || len(current.Sections) != 0 || len(current.ToolsAdded) != 1 || current.Timestamp != 1700000000123 {
					t.Errorf("forced request = %+v, want exact %q and retained tools/timestamp", current, forced)
				}
				handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
				return nil
			}}
			_, err := RunAgentLoop(t.Context(), []*Message{NewUserMessage("hello")}, &AgentContext{Messages: []*Message{history}, Tools: []Tool{{Name: "available"}}}, AgentLoopConfig{
				Model: &ai.Model{ID: "test"}, ConvertToLlm: DefaultConvertToLlm, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, EventDispatcher: func(ctx context.Context, name string, data []byte, _ string) EventDispatchResult {
					if name == "context" {
						return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: []byte(`{"messages":[{"role":"user","content":"ordinary hook"}]}`)}}}
					}
					if name == "context_with_system" {
						var payload struct {
							Messages []ai.Message `json:"messages"`
						}
						if err := json.Unmarshal(data, &payload); err != nil {
							t.Fatal(err)
						}
						current := ai.CurrentSystemMessage(payload.Messages)
						if current == nil || current.Content != forced || len(current.Sections) != 0 {
							t.Fatalf("system-aware hook did not see forced projection: %+v", current)
						}
						if forced == "system-aware replacement" {
							payload.Messages[0].Content = "authoritative hook"
							updated, err := json.Marshal(payload)
							if err != nil {
								t.Fatal(err)
							}
							return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: updated}}}
						}
						return EventDispatchResult{}
					}
					if name != "before_agent_start" {
						return EventDispatchResult{}
					}
					data, err := json.Marshal(map[string]string{"systemPrompt": forced})
					if err != nil {
						t.Fatal(err)
					}
					return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: data}}}
				},
			}, func(Event) {})
			if err != nil {
				t.Fatal(err)
			}
			if requests != 1 || history.Content[0].Text != "historical policy" || *history.Sections["policy"] != section {
				t.Fatalf("request count %d or history mutated: %+v", requests, history)
			}
		})
	}
}

func TestBeforeAgentStartReceivesReplayedPrompt(t *testing.T) {
	section := "section policy"
	var received string
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := RunAgentLoop(t.Context(), []*Message{NewUserMessage("hello")}, &AgentContext{Messages: []*Message{
		{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "historical policy"}}, Sections: map[string]*string{"policy": &section}},
		{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "excluded policy"}}, ExcludeFromContext: true},
	}}, AgentLoopConfig{Model: &ai.Model{ID: "test"}, SystemPrompt: "configured", ConvertToLlm: DefaultConvertToLlm, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, EventDispatcher: func(ctx context.Context, name string, data []byte, _ string) EventDispatchResult {
		if name == "before_agent_start" {
			var payload struct {
				SystemPrompt string `json:"systemPrompt"`
			}
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatal(err)
			}
			received = payload.SystemPrompt
		}
		return EventDispatchResult{}
	}}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if received != "configured\n\nhistorical policy\n\nsection policy" {
		t.Fatalf("startup prompt = %q", received)
	}
}

func TestContinueContextIgnoresNilTranscriptEntries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []*Message
		want     error
	}{
		{"only nil", []*Message{nil}, ErrCannotContinueNoMessages},
		{"assistant then nil", []*Message{{Role: RoleAssistant}, nil}, ErrCannotContinueFromAssistant},
		{"user then nil", []*Message{NewUserMessage("hello"), nil}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := prepareLoopContext(&AgentContext{Messages: tc.messages}, loopStartContinue, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("continue error=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestDropOrphanToolResultsHandlesPublicRole(t *testing.T) {
	orphan := &Message{Role: "toolResult", ToolCallID: "missing"}
	assistant := &Message{Role: RoleAssistant, Content: []ai.ContentBlock{{Type: ai.ContentBlockToolCall, ID: "call", Name: "lookup"}}}
	result := &Message{Role: "toolResult", ToolCallID: "call"}
	messages := []*Message{orphan, assistant, result}
	kept := dropOrphanToolResults(messages)
	if len(kept) != 2 || kept[0] != assistant || kept[1] != result {
		t.Fatalf("public tool-result pairing lost: %+v", kept)
	}
	if len(messages) != 3 || messages[0] != orphan {
		t.Fatal("source history mutated")
	}
}

func TestPlainSystemProjectionPreservesTextBlocks(t *testing.T) {
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		got := ai.CurrentSystemPrompt(ai.NormalizeContext(prompt).Messages)
		if got != " configured \n\n first \nsecond " {
			t.Errorf("projected prompt = %q", got)
		}
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := streamAssistantResponse(t.Context(), &AgentContext{Messages: []*Message{
		{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: " first "}, {Type: ai.ContentBlockThinking, Thinking: "private reasoning"}, {Type: ai.ContentBlockText, Text: "second "}}}, NewUserMessage("hello"),
	}}, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{ID: "test"}, " configured ", nil, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAfterToolCallCanClearContent(t *testing.T) {
	for _, preserveStructured := range []bool{false, true} {
		t.Run(fmt.Sprintf("preserve structured=%v", preserveStructured), func(t *testing.T) {
			tool := Tool{Name: "clear", Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) {
				return &ToolResult{Content: "stale text", ContentBlocks: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "stale blocks"}}, StructuredContent: json.RawMessage(`{"stale":true}`)}, nil
			}}
			result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{tool}}, nil, ai.ToolCall{Name: "clear", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{AfterToolCall: func(context.Context, ToolCallContext, *ToolResult) (AfterToolCallResult, error) {
				replacement := AfterToolCallResult{ContentBlocks: []ai.ContentBlock{}}
				if preserveStructured {
					replacement.StructuredContent = json.RawMessage(`42`)
				}
				return replacement, nil
			}})
			if result.IsError || result.Content != "" || result.ContentBlocks == nil || len(result.NormalizedContent()) != 0 {
				t.Fatalf("content not cleared: %+v", result)
			}
			if preserveStructured {
				if string(result.StructuredContent) != "42" {
					t.Fatalf("replacement lost: %s", result.StructuredContent)
				}
			} else if result.StructuredContent != nil {
				t.Fatalf("stale structured output retained: %s", result.StructuredContent)
			}
			finalized := finalizeToolResultMessage(ai.ToolCall{ID: "call", Name: "clear"}, result, func(Event) {})
			if len(finalized.Message.Content) != 0 {
				t.Fatalf("cleared content resurrected in transcript: %+v", finalized.Message)
			}
		})
	}
}

func TestRunToolCallUsesExplicitRegistryInsteadOfModelDeclarations(t *testing.T) {
	removal := &Message{Role: RoleSystem, ToolsRemoved: []ai.ToolReference{{Name: "nested"}}}
	tool := Tool{Name: "nested", Exposure: ToolHidden, Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { return &ToolResult{Content: "authorized"}, nil }}
	current := &AgentContext{Messages: []*Message{removal}, Tools: []Tool{tool}}
	var before, after bool
	result := RunToolCall(t.Context(), current, nil, ai.ToolCall{Name: "nested", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{
		BeforeToolCall: func(_ context.Context, call ToolCallContext) (BeforeToolCallResult, error) {
			before = true
			if call.Context != current {
				t.Fatal("hook lost original context")
			}
			return BeforeToolCallResult{}, nil
		},
		AfterToolCall: func(_ context.Context, call ToolCallContext, _ *ToolResult) (AfterToolCallResult, error) {
			after = true
			if call.Context != current {
				t.Fatal("hook lost original context")
			}
			return AfterToolCallResult{}, nil
		},
	})
	if result.IsError || result.Content != "authorized" || !before || !after {
		t.Fatalf("explicit registry rejected: %+v, before=%v after=%v", result, before, after)
	}
	if current.Tools[0].Exposure != ToolHidden || len(current.Messages[0].ToolsRemoved) != 1 {
		t.Fatal("caller state mutated")
	}
	prepared := prepareToolCall(t.Context(), current, nil, ai.ToolCall{Name: "nested", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if prepared.immediate == nil || !prepared.immediate.IsError {
		t.Fatal("model call bypassed transcript removal")
	}
}

func TestTranscriptLoadoutDoesNotInheritUndeclaredRegistryTools(t *testing.T) {
	var executed bool
	registry := []Tool{{Name: "declared", Parameters: map[string]any{"type": "object"}}, {Name: "registry-only", Execute: func(ToolContext) (*ToolResult, error) {
		executed = true
		return &ToolResult{Content: "unexpected"}, nil
	}}}
	history := []*Message{{Role: RoleSystem, ToolsAdded: []ai.Tool{{Name: "declared"}}}, NewUserMessage("hello")}
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		baseline := ai.CurrentSystemMessage(ai.NormalizeContext(prompt).Messages)
		if baseline == nil || len(baseline.ToolsAdded) != 1 || baseline.ToolsAdded[0].Name != "declared" {
			t.Errorf("registry leaked into canonical loadout: %+v", baseline)
		}
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	agentCtx := &AgentContext{Messages: history, Tools: registry}
	_, err := streamAssistantResponse(t.Context(), agentCtx, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{ID: "test"}, "", nil, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	prepared := prepareToolCall(t.Context(), agentCtx, &Message{Role: RoleAssistant}, ai.ToolCall{ID: "call", Name: "registry-only", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if prepared.immediate == nil || !prepared.immediate.IsError || executed {
		t.Fatal("undeclared registry tool was authorized")
	}
	if len(history[0].ToolsAdded) != 1 {
		t.Fatal("request altered transcript")
	}
}

func TestNestedExecutionRejectionsDifferFromErrorValues(t *testing.T) {
	failure := errors.New("transport closed")
	tools := []Tool{
		{Name: "value", Parameters: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) {
			return &ToolResult{IsError: true, StructuredContent: json.RawMessage(`{"error":"not found"}`)}, nil
		}},
		{Name: "reject", Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { return nil, failure }},
	}
	current := &AgentContext{Tools: tools}
	value := RunToolCall(t.Context(), current, nil, ai.ToolCall{Name: "value", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if !value.IsError || value.Rejection != nil || len(value.StructuredContent) == 0 {
		t.Fatalf("error value rejected: %+v", value)
	}
	rejected := RunToolCall(t.Context(), current, nil, ai.ToolCall{Name: "reject", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if !errors.Is(rejected.Rejection, failure) {
		t.Fatalf("execution cause lost: %+v", rejected)
	}
	denied := RunToolCall(t.Context(), current, nil, ai.ToolCall{Name: "private", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if denied.Rejection == nil {
		t.Fatal("missing executor returned usable error value")
	}
}

func TestNestedExecutionSettlesWithCaller(t *testing.T) {
	var late func(ai.ToolCall) *ToolResult
	caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"*"} }, Execute: func(ctx ToolContext) (*ToolResult, error) {
		late = ctx.ExecuteTool
		return &ToolResult{Content: "done"}, nil
	}}
	calls := 0
	target := Tool{Name: "target", Exposure: ToolDeferred, Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { calls++; return &ToolResult{}, nil }}
	RunToolCall(t.Context(), &AgentContext{Tools: []Tool{caller, target}}, nil, ai.ToolCall{Name: "caller", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	result := late(ai.ToolCall{Name: "target", Arguments: json.RawMessage(`{}`)})
	if calls != 0 || result.Rejection == nil {
		t.Fatalf("late nested execution escaped settlement: %+v, calls=%d", result, calls)
	}
}

func TestCallerWaitsForReachableTools(t *testing.T) {
	ready := false
	caller := Tool{Name: "replacement", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"docs"} }, Execute: func(ToolContext) (*ToolResult, error) {
		if !ready {
			t.Error("discovery started before reachable server ready")
		}
		return &ToolResult{}, nil
	}}
	target := Tool{Name: "lookup", Exposure: ToolDeferred, Namespace: &ToolNamespace{Name: "docs"}, WaitReady: func(context.Context) error { ready = true; return nil }}
	result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{caller, target}}, nil, ai.ToolCall{Name: "replacement", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if result.Rejection != nil {
		t.Fatal(result.Rejection)
	}
}

func TestNestedRecordsAndUsageAreBounded(t *testing.T) {
	target := Tool{Name: "lookup", Exposure: ToolDeferred, Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) {
		return &ToolResult{Usage: &ai.Usage{Input: 1, TotalTokens: 1}}, nil
	}}
	caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"*"} }, Execute: func(ctx ToolContext) (*ToolResult, error) {
		for range 140 {
			ctx.ExecuteTool(ai.ToolCall{Name: "lookup", Arguments: json.RawMessage(`{}`)})
		}
		return &ToolResult{}, nil
	}}
	result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{caller, target}}, nil, ai.ToolCall{ID: "outer", Name: "caller", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if len(result.NestedCalls) != 128 || result.Usage == nil || result.Usage.Input != 140 {
		t.Fatalf("unbounded records or incomplete usage: %+v", result)
	}
	for _, record := range result.NestedCalls {
		if record.Status != "ok" || record.ID == "" {
			t.Fatalf("unsettled record: %+v", record)
		}
	}
}

func TestCallerJoinsCancelledNestedExecution(t *testing.T) {
	entered := make(chan struct{})
	finished := make(chan struct{})
	updates := 0
	target := Tool{Name: "wait", Exposure: ToolDeferred, Parameters: map[string]any{"type": "object"}, Execute: func(ctx ToolContext) (*ToolResult, error) {
		close(entered)
		<-ctx.Context.Done()
		ctx.OnUpdate(&ToolResult{Content: "late"})
		return nil, ctx.Context.Err()
	}}
	caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"*"} }, Execute: func(ctx ToolContext) (*ToolResult, error) {
		go func() {
			defer close(finished)
			ctx.ExecuteTool(ai.ToolCall{Name: "wait", Arguments: json.RawMessage(`{}`)})
		}()
		<-entered
		return &ToolResult{}, nil
	}}
	result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{caller, target}}, nil, ai.ToolCall{Name: "caller", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{}, func(partial *ToolResult) {
		for _, record := range partial.NestedCalls {
			if record.Status != "running" {
				t.Errorf("late progress: %+v", partial)
			}
		}
		updates++
	})
	<-finished
	if len(result.NestedCalls) != 1 || result.NestedCalls[0].Status != "cancelled" || updates != 1 {
		t.Fatalf("cancellation did not settle: %+v, updates=%d", result, updates)
	}
}

func TestFinalizeToolResultRetainsNestedRecords(t *testing.T) {
	result := finalizeToolResultMessage(ai.ToolCall{ID: "outer", Name: "caller"}, &ToolResult{NestedCalls: []NestedToolCall{{ID: "outer/1", Name: "lookup", Status: "ok"}}}, func(Event) {})
	if len(result.Message.NestedCalls) != 1 || result.Message.NestedCalls[0].ID != "outer/1" {
		t.Fatalf("lost nested records: %+v", result.Message)
	}
}

func TestNestedProgressRetainsCallerContentAndRecords(t *testing.T) {
	target := Tool{Name: "lookup", Parameters: map[string]any{"type": "object"}, Exposure: ToolDeferred, Execute: func(ctx ToolContext) (*ToolResult, error) {
		ctx.OnUpdate(&ToolResult{Content: "child output"})
		return &ToolResult{}, nil
	}}
	caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"all"} }, Execute: func(ctx ToolContext) (*ToolResult, error) {
		ctx.OnUpdate(&ToolResult{Content: "parent output"})
		ctx.ExecuteTool(ai.ToolCall{Name: "lookup", Arguments: json.RawMessage(`{}`)})
		return &ToolResult{}, nil
	}}
	var snapshots []*ToolResult
	result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{caller, target}}, nil, ai.ToolCall{Name: "caller", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{}, func(partial *ToolResult) { snapshots = append(snapshots, partial) })
	if result.Rejection != nil {
		t.Fatal(result.Rejection)
	}
	if len(snapshots) < 2 {
		t.Fatalf("missing progress: %+v", snapshots)
	}
	last := snapshots[len(snapshots)-1]
	if last.Content != "parent output" || len(last.NestedCalls) != 1 || last.Message == nil || len(last.Message.NestedCalls) != 1 {
		t.Fatalf("missing parent content or live records: %+v", last)
	}
}

func TestReachFailurePreventsCallerExecution(t *testing.T) {
	failure := errors.New("reach unavailable")
	caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachTools: func(context.Context, map[string]any) ([]string, error) { return nil, failure }, Execute: func(ToolContext) (*ToolResult, error) { t.Fatal("caller executed after failed reach"); return nil, nil }}
	result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{caller}}, nil, ai.ToolCall{Name: "caller", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
	if !errors.Is(result.Rejection, failure) {
		t.Fatalf("lost reach failure: %+v", result)
	}
}

func TestNestedProgressIncludesRunningCalls(t *testing.T) {
	var snapshots [][]NestedToolCall
	target := Tool{Name: "lookup", Exposure: ToolDeferred, Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { return &ToolResult{}, nil }}
	caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"all"} }, Execute: func(ctx ToolContext) (*ToolResult, error) {
		ctx.ExecuteTool(ai.ToolCall{ID: "child", Name: "lookup", Arguments: json.RawMessage(`{}`)})
		return &ToolResult{}, nil
	}}
	result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{caller, target}}, nil, ai.ToolCall{Name: "caller", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{}, func(partial *ToolResult) { snapshots = append(snapshots, slices.Clone(partial.NestedCalls)) })
	if result.Rejection != nil {
		t.Fatal(result.Rejection)
	}
	if len(snapshots) < 2 || len(snapshots[0]) != 1 || snapshots[0][0].Status != "running" || snapshots[len(snapshots)-1][0].Status != "ok" {
		t.Fatalf("missing lifecycle progress: %+v", snapshots)
	}
}

func TestAfterToolFailureRetainsNestedAccounting(t *testing.T) {
	failure := errors.New("hook failed")
	target := Tool{Name: "lookup", Exposure: ToolDeferred, Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { return &ToolResult{Usage: &ai.Usage{Input: 3}}, nil }}
	caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"all"} }, Execute: func(ctx ToolContext) (*ToolResult, error) {
		ctx.ExecuteTool(ai.ToolCall{Name: "lookup", Arguments: json.RawMessage(`{}`)})
		return &ToolResult{Usage: &ai.Usage{Input: 2}}, nil
	}}
	result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{caller, target}}, nil, ai.ToolCall{Name: "caller", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{AfterToolCall: func(_ context.Context, call ToolCallContext, _ *ToolResult) (AfterToolCallResult, error) {
		if call.ToolName == "caller" {
			return AfterToolCallResult{}, failure
		}
		return AfterToolCallResult{}, nil
	}})
	if !errors.Is(result.Rejection, failure) || len(result.NestedCalls) != 1 || result.Usage == nil || result.Usage.Input != 5 {
		t.Fatalf("hook discarded execution accounting: %+v", result)
	}
}

func TestAfterToolUsageReplacesAggregate(t *testing.T) {
	target := Tool{Name: "lookup", Exposure: ToolDeferred, Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { return &ToolResult{Usage: &ai.Usage{Input: 3}}, nil }}
	caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"all"} }, Execute: func(ctx ToolContext) (*ToolResult, error) {
		ctx.ExecuteTool(ai.ToolCall{Name: "lookup", Arguments: json.RawMessage(`{}`)})
		return &ToolResult{Usage: &ai.Usage{Input: 2}}, nil
	}}
	result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{caller, target}}, nil, ai.ToolCall{Name: "caller", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{AfterToolCall: func(_ context.Context, call ToolCallContext, result *ToolResult) (AfterToolCallResult, error) {
		if call.ToolName == "caller" {
			if result.Usage.Input != 5 {
				t.Errorf("hook received incomplete usage: %+v", result.Usage)
			}
			return AfterToolCallResult{Usage: &ai.Usage{Input: 7}}, nil
		}
		return AfterToolCallResult{}, nil
	}})
	if result.Usage == nil || result.Usage.Input != 7 || len(result.NestedCalls) != 1 {
		t.Fatalf("usage replacement or records lost: %+v", result)
	}
}

func TestExecutionRejectionRetainsNestedAccounting(t *testing.T) {
	cause := errors.New("execution failed")
	tool := Tool{Name: "caller", Execute: func(ToolContext) (*ToolResult, error) {
		return &ToolResult{Usage: &ai.Usage{Input: 7}, NestedCalls: []NestedToolCall{{ID: "child", Name: "lookup", Status: "ok"}}}, cause
	}}
	result := executePreparedTool(t.Context(), &AgentContext{}, nil, preparedToolCall{tool: &tool, toolCall: ai.ToolCall{ID: "parent", Name: tool.Name}}, AgentLoopConfig{}, func(Event) {})
	if !errors.Is(result.Rejection, cause) || result.Usage == nil || result.Usage.Input != 7 || len(result.NestedCalls) != 1 {
		t.Fatalf("accounting lost: %+v", result)
	}
}

func TestRunAgentLoopClampsResponseBudgetToContextRoom(t *testing.T) {
	var maxTokens []int
	client := stubClient{
		stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			maxTokens = append(maxTokens, ai.ExtractSimpleStreamOptions(opts).MaxTokens)
			final := ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}}, StopReason: ai.StopReasonStop, Timestamp: time.Now().UnixMilli()}
			handler(ctx, ai.Start{Partial: final})
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}
	model := &ai.Model{ID: "test", Provider: ai.ProviderOpenAI, ContextWindow: 10000, MaxTokens: 8000}
	prompt := strings.Repeat("x", 3500)
	_, err := RunAgentLoop(context.Background(), []*Message{NewUserMessage(prompt)}, &AgentContext{Model: model}, AgentLoopConfig{
		Model:         model,
		ConvertToLlm:  DefaultConvertToLlm,
		ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil },
	}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{10000 - 1000 - 4096}; !reflect.DeepEqual(maxTokens, want) {
		t.Fatalf("maxTokens = %v, want %v", maxTokens, want)
	}
}

func TestLoopDispatchCarriesContextThroughToolLifecycle(t *testing.T) {
	type contextKey struct{}
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), contextKey{}, "operation"), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	seen := make(map[string]int)
	var requests int
	tool := Tool{Name: "inspect", Parameters: map[string]any{}, Execute: func(tc ToolContext) (*ToolResult, error) {
		if tc.Context.Value(contextKey{}) != "operation" {
			t.Error("tool lost operation context")
		}
		return &ToolResult{Content: "result"}, nil
	}}
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		requests++
		message := ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}}
		if requests == 1 {
			message.StopReason = ai.StopReasonToolUse
			message.Content = []ai.ContentBlock{{Type: ai.ContentBlockToolCall, ID: "call", Name: "inspect", Arguments: json.RawMessage(`{}`)}}
		}
		handler(ctx, ai.Done{Message: message})
		return nil
	}}
	_, err := RunAgentLoop(ctx, []*Message{NewUserMessage("hello")}, &AgentContext{Tools: []Tool{tool}}, AgentLoopConfig{
		Model: &ai.Model{ID: "test"}, ConvertToLlm: DefaultConvertToLlm,
		ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, SessionID: "session",
		EventDispatcher: func(hookCtx context.Context, name string, _ []byte, id string) EventDispatchResult {
			seen[name]++
			if hookCtx.Value(contextKey{}) != "operation" || id != "session" {
				t.Errorf("%s lost context or session", name)
			}
			if got, ok := hookCtx.Deadline(); !ok || got != deadline {
				t.Errorf("%s lost deadline", name)
			}
			return EventDispatchResult{}
		},
	}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"before_agent_start", "agent_start", "turn_start", "input", "message_start", "message_end", "model_select", "before_provider_request", "context", "context_with_system", "after_provider_response", "tool_call", "tool_execution_start", "tool_execution_end", "tool_result", "turn_end", "agent_before_settle", "agent_end"} {
		if seen[name] == 0 {
			t.Errorf("missing %s", name)
		}
	}
}

func TestPrepareRequestSelectsClientBeforeProviderSetup(t *testing.T) {
	for _, continuation := range []bool{false, true} {
		t.Run(fmt.Sprint(continuation), func(t *testing.T) {
			initial := &ai.Model{ID: "unroutable", Provider: "unroutable"}
			selected := &ai.Model{ID: "physical", Provider: "physical"}
			setupErr := errors.New("unrouted provider cannot be constructed")
			var created, observed []string
			var completed []*Message
			var polls, preparations, streams int
			late := NewUserMessage("late steering")
			projected := NewUserMessage("canonical projection")
			client := stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
				streams++
				if model != selected || ai.ExtractSimpleStreamOptions(opts).Reasoning != ai.ReasoningHigh {
					t.Errorf("model=%+v options=%+v", model, ai.ExtractSimpleStreamOptions(opts))
				}
				if streams == 1 && (len(prompt.Messages) != 1 || !strings.Contains(fmt.Sprint(prompt.Messages[0].Content), "canonical projection")) {
					t.Errorf("first request=%+v", prompt.Messages)
				}
				if streams == 2 && !strings.Contains(fmt.Sprint(prompt.Messages), "late steering") {
					t.Errorf("second request lost queued steering: %+v", prompt.Messages)
				}
				handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
				return nil
			}}
			config := AgentLoopConfig{
				ConvertToLlm: DefaultConvertToLlm,
				ClientFactory: func(model *ai.Model) (ai.Client, error) {
					created = append(created, model.ID)
					if model != selected {
						return nil, setupErr
					}
					return client, nil
				},
				PrepareRequest: func(_ context.Context, request PrepareRequestContext) (*AgentRequestUpdate, error) {
					preparations++
					observed = append(observed, request.Model.ID)
					if preparations == 1 {
						if len(completed) == 0 || request.Context.Messages[len(request.Context.Messages)-1] != completed[len(completed)-1] {
							t.Error("preparation ran before message publication")
						}
						return &AgentRequestUpdate{Model: selected, ThinkingLevel: new(ThinkingHigh), Context: &AgentContext{Messages: []*Message{projected}}}, nil
					}
					if request.ThinkingLevel != ThinkingHigh || request.Context.Messages[0] != projected {
						t.Errorf("replacement did not persist: %+v", request)
					}
					return nil, nil
				},
				GetSteeringMessages: func(context.Context) ([]*Message, error) {
					polls++
					if preparations == 1 {
						return []*Message{late}, nil
					}
					return nil, nil
				},
			}
			snapshot := &AgentContext{Model: initial, Messages: []*Message{NewUserMessage("previous")}}
			sink := func(event Event) {
				if e, ok := event.(MessageEnd); ok {
					completed = append(completed, e.Message)
				}
			}
			// Continuation has no new startup message to publish.
			if continuation {
				completed = append(completed, snapshot.Messages...)
			}
			var err error
			if continuation {
				_, err = RunAgentLoopContinue(t.Context(), snapshot, config, sink)
			} else {
				_, err = RunAgentLoop(t.Context(), []*Message{NewUserMessage("prompt")}, snapshot, config, sink)
			}
			if err != nil || !reflect.DeepEqual(created, []string{"physical"}) || !reflect.DeepEqual(observed, []string{"unroutable", "physical"}) || streams != 2 || polls != 3 {
				t.Fatalf("err=%v created=%v observed=%v streams=%d polls=%d", err, created, observed, streams, polls)
			}
		})
	}
}

func TestPrepareRequestSupersedesNextTurnClientSelection(t *testing.T) {
	var created []string
	var streams int
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		streams++
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err := RunAgentLoop(t.Context(), []*Message{NewUserMessage("prompt")}, &AgentContext{}, AgentLoopConfig{
		Model: &ai.Model{ID: "initial"}, ConvertToLlm: DefaultConvertToLlm,
		ClientFactory: func(model *ai.Model) (ai.Client, error) {
			created = append(created, model.ID)
			if model.ID == "intermediate" {
				return nil, errors.New("intermediate provider must not be constructed")
			}
			return client, nil
		},
		PrepareRequest: func(_ context.Context, request PrepareRequestContext) (*AgentRequestUpdate, error) {
			if streams == 1 {
				if request.Model.ID != "intermediate" {
					t.Errorf("request model=%s", request.Model.ID)
				}
				return &AgentRequestUpdate{Model: &ai.Model{ID: "final"}}, nil
			}
			return nil, nil
		},
		PrepareNextTurn: func(context.Context, AgentTurnContext) (*AgentTurnUpdate, error) {
			return &AgentTurnUpdate{Model: &ai.Model{ID: "intermediate"}}, nil
		},
		FinishTurn: func(context.Context, AgentTurnContext) (TurnDecision, error) {
			if streams == 1 {
				return TurnContinue, nil
			}
			return TurnEndRun, nil
		},
	}, func(Event) {})
	if err != nil || !reflect.DeepEqual(created, []string{"initial", "final"}) || streams != 2 {
		t.Fatalf("err=%v created=%v streams=%d", err, created, streams)
	}
}

func TestPrepareRequestFailureDoesNotConstructProvider(t *testing.T) {
	failure := errors.New("request preparation rejected")
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			ctx, stop := context.WithCancelCause(t.Context())
			defer stop(nil)
			var created int
			_, err := RunAgentLoop(ctx, []*Message{NewUserMessage("prompt")}, &AgentContext{}, AgentLoopConfig{
				Model: &ai.Model{ID: "initial"}, ConvertToLlm: DefaultConvertToLlm,
				ClientFactory: func(*ai.Model) (ai.Client, error) { created++; return stubClient{}, nil },
				PrepareRequest: func(context.Context, PrepareRequestContext) (*AgentRequestUpdate, error) {
					if cancel {
						stop(failure)
						return &AgentRequestUpdate{Model: &ai.Model{ID: "cancelled"}}, nil
					}
					return nil, failure
				},
			}, func(Event) {})
			if !errors.Is(err, failure) || created != 0 {
				t.Fatalf("err=%v created=%d", err, created)
			}
		})
	}
}

func TestPreparedClientFailureRetainsPublishedPromptAndCause(t *testing.T) {
	setupErr := errors.New("prepared client failed")
	var events []string
	var selected string
	result, err := RunAgentLoop(t.Context(), []*Message{NewUserMessage("prompt")}, &AgentContext{}, AgentLoopConfig{
		Model: &ai.Model{ID: "initial"}, ConvertToLlm: DefaultConvertToLlm,
		PrepareRequest: func(context.Context, PrepareRequestContext) (*AgentRequestUpdate, error) {
			return &AgentRequestUpdate{Model: &ai.Model{ID: "selected"}}, nil
		},
		ClientFactory: func(model *ai.Model) (ai.Client, error) {
			selected = model.ID
			return nil, setupErr
		},
	}, func(event Event) { events = append(events, eventLabel(event)) })
	if !errors.Is(err, setupErr) || selected != "selected" || len(result) != 1 || messageText(result[0]) != "prompt" {
		t.Fatalf("err=%v selected=%s result=%+v", err, selected, result)
	}
	want := []string{"agent_start", "turn_start", "message_start:user", "message_end:user", "agent_end"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestLoopMessageSupplierFailureStopsProviderAdmission(t *testing.T) {
	for _, queue := range []string{"steering", "follow-up"} {
		t.Run(queue, func(t *testing.T) {
			failure := errors.New("queue supplier failed")
			var streams, ends int
			client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
				streams++
				handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
				return nil
			}}
			config := AgentLoopConfig{Model: &ai.Model{ID: "test"}, ConvertToLlm: DefaultConvertToLlm, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }}
			supplier := func(context.Context) ([]*Message, error) {
				return []*Message{NewUserMessage("partial must not be sent")}, failure
			}
			wantStreams := 0
			if queue == "steering" {
				config.GetSteeringMessages = supplier
			} else {
				config.GetFollowUpMessages = supplier
				wantStreams = 1
			}
			_, err := RunAgentLoop(t.Context(), []*Message{NewUserMessage("prompt")}, &AgentContext{}, config, func(event Event) {
				if _, ok := event.(AgentEnd); ok {
					ends++
				}
			})
			if !errors.Is(err, failure) || streams != wantStreams || ends != 1 {
				t.Fatalf("err=%v streams=%d want=%d ends=%d", err, streams, wantStreams, ends)
			}
		})
	}
}

func TestRunAgentLoopRetainsPublishedFailureOnStreamError(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		t.Run(entry, func(t *testing.T) {
			failure := errors.New("stream cleanup failed")
			var events []Event
			config := AgentLoopConfig{Model: &ai.Model{ID: "local"}, ConvertToLlm: DefaultConvertToLlm, StreamFn: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
				handler(ctx, ai.StreamError{Msg: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonError, ErrorMessage: "provider rejected", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "partial"}}}})
				return failure
			}}
			sink := func(event Event) { events = append(events, event) }
			current := &AgentContext{Messages: []*Message{NewUserMessage("history")}}
			var messages []*Message
			var err error
			if entry == "prompt" {
				messages, err = RunAgentLoop(t.Context(), []*Message{NewUserMessage("hello")}, current, config, sink)
			} else {
				messages, err = RunAgentLoopContinue(t.Context(), current, config, sink)
			}
			if !errors.Is(err, failure) {
				t.Fatalf("error = %v, want cleanup error", err)
			}
			var published *Message
			var ends int
			for _, event := range events {
				if e, ok := event.(MessageEnd); ok && e.Message.Role == RoleAssistant {
					if published != nil {
						t.Fatal("duplicate assistant failure")
					}
					published = e.Message
				}
				if _, ok := event.(AgentEnd); ok {
					ends++
				}
			}
			if published == nil || ends != 1 || len(messages) == 0 || messages[len(messages)-1] != published {
				t.Fatalf("returned messages must retain the published failure: published=%p, messages=%v, ends=%d", published, messages, ends)
			}
			end := events[len(events)-1].(AgentEnd)
			if len(end.Messages) != len(messages) || end.Messages[len(end.Messages)-1] != published {
				t.Fatal("agent end must retain returned messages")
			}
		})
	}
}

func TestDirectLoopFailureRetainsCancellation(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		for _, stage := range []string{"startup", "steering", "request", "client", "transform", "conversion", "credentials", "stream", "published failure", "finish", "follow-up", "next turn"} {
			for _, outcome := range []string{"callback error", "context error", "cause", "deadline"} {
				t.Run(entry+"/"+stage+"/"+outcome, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						cause := errors.New("direct loop owner stopped request")
						failure := errors.New("callback cleanup failed")
						ctx, cancel := context.WithCancelCause(t.Context())
						defer cancel(nil)
						if outcome == "deadline" {
							var stop context.CancelFunc
							ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
							defer stop()
						}
						var failures, streams int
						fail := func() error {
							failures++
							if outcome == "deadline" {
								<-ctx.Done()
							} else {
								cancel(cause)
							}
							switch outcome {
							case "context error":
								return ctx.Err()
							case "cause":
								return context.Cause(ctx)
							default:
								return failure
							}
						}
						config := AgentLoopConfig{
							Model: &ai.Model{ID: "local"}, ConvertToLlm: DefaultConvertToLlm,
							StreamFn: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
								streams++
								if stage == "stream" {
									return fail()
								}
								if stage == "published failure" {
									handler(ctx, ai.StreamError{Msg: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonError, ErrorMessage: "provider rejected"}})
									return fail()
								}
								handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
								return nil
							},
						}
						switch stage {
						case "startup":
							config.GetStartupMessages = func(context.Context) ([]*Message, error) { return nil, fail() }
						case "steering":
							config.GetSteeringMessages = func(context.Context) ([]*Message, error) { return nil, fail() }
						case "request":
							config.PrepareRequest = func(context.Context, PrepareRequestContext) (*AgentRequestUpdate, error) { return nil, fail() }
						case "client":
							config.StreamFn = nil
							config.ClientFactory = func(*ai.Model) (ai.Client, error) { return nil, fail() }
						case "transform":
							config.TransformContext = func(context.Context, []*Message) ([]*Message, error) { return nil, fail() }
						case "conversion":
							config.ConvertToLlm = func(context.Context, []*Message) ([]ai.Message, error) { return nil, fail() }
						case "credentials":
							config.GetAPIKey = func(context.Context, string) (string, error) { return "", fail() }
						case "finish":
							config.FinishTurn = func(context.Context, AgentTurnContext) (TurnDecision, error) { return "", fail() }
						case "follow-up":
							config.GetFollowUpMessages = func(context.Context) ([]*Message, error) { return nil, fail() }
						case "next turn":
							config.FinishTurn = func(context.Context, AgentTurnContext) (TurnDecision, error) { return TurnContinue, nil }
							config.PrepareNextTurn = func(context.Context, AgentTurnContext) (*AgentTurnUpdate, error) { return nil, fail() }
						}
						var events []Event
						sink := func(event Event) { events = append(events, event) }
						current := &AgentContext{Messages: []*Message{NewUserMessage("history")}}
						var messages []*Message
						var err error
						if entry == "prompt" {
							messages, err = RunAgentLoop(ctx, []*Message{NewUserMessage("hello")}, current, config, sink)
						} else {
							messages, err = RunAgentLoopContinue(ctx, current, config, sink)
						}
						require.ErrorIs(t, err, cause)
						require.ErrorIs(t, err, ctx.Err())
						if outcome == "callback error" || outcome == "deadline" {
							require.ErrorIs(t, err, failure)
						}
						require.Equal(t, 1, failures)
						wantStreams := 0
						switch stage {
						case "stream", "published failure", "finish", "follow-up", "next turn":
							wantStreams = 1
						}
						require.Equal(t, wantStreams, streams)
						if stage == "startup" {
							require.Empty(t, events)
							require.Empty(t, messages)
							return
						}
						var ends int
						var published *Message
						for _, event := range events {
							if _, ok := event.(AgentEnd); ok {
								ends++
							}
							if end, ok := event.(MessageEnd); ok && end.Message.Role == RoleAssistant {
								require.Nil(t, published, "assistant must publish once")
								published = end.Message
							}
						}
						require.Equal(t, 1, ends)
						require.Equal(t, messages, events[len(events)-1].(AgentEnd).Messages)
						if stage == "published failure" {
							var wrapped *publishedStreamFailure
							require.ErrorAs(t, err, &wrapped)
							require.Same(t, published, wrapped.message)
							require.Same(t, published, messages[len(messages)-1])
							require.Equal(t, ai.StopReasonError, published.StopReason)
						}
					})
				})
			}
		}
	}
}

func TestDirectLoopCancellationDoesNotRewriteSuccessfulPublication(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		for _, outcome := range []string{"success", "published error", "callback error"} {
			t.Run(entry+"/"+outcome, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				cause := errors.New("publication owner stopped request")
				failure := errors.New("uncancelled callback failed")
				config := AgentLoopConfig{
					Model: &ai.Model{ID: "local"}, ConvertToLlm: DefaultConvertToLlm,
					StreamFn: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
						switch outcome {
						case "callback error":
							return failure
						case "published error":
							handler(ctx, ai.StreamError{Msg: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonError, ErrorMessage: "provider rejected"}})
						default:
							handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
						}
						return nil
					},
				}
				var events []Event
				sink := func(event Event) {
					events = append(events, event)
					if _, ok := event.(AgentEnd); ok && outcome != "callback error" {
						cancel(cause)
					}
				}
				current := &AgentContext{Messages: []*Message{NewUserMessage("history")}}
				var messages []*Message
				var err error
				if entry == "prompt" {
					messages, err = RunAgentLoop(ctx, []*Message{NewUserMessage("hello")}, current, config, sink)
				} else {
					messages, err = RunAgentLoopContinue(ctx, current, config, sink)
				}
				if outcome == "callback error" {
					require.Same(t, failure, err)
					require.NoError(t, ctx.Err())
					return
				}
				require.NoError(t, err)
				require.ErrorIs(t, context.Cause(ctx), cause)
				require.NotEmpty(t, messages)
				last := messages[len(messages)-1]
				wantStop := ai.StopReasonStop
				if outcome == "published error" {
					wantStop = ai.StopReasonError
				}
				require.Equal(t, wantStop, last.StopReason)
				require.Same(t, last, events[len(events)-1].(AgentEnd).Messages[len(messages)-1])
			})
		}
	}
}

func TestBuildAIOptionsRetainsThinkingBudgets(t *testing.T) {
	budgets := &ai.ThinkingBudgets{Low: new(0), High: new(3000)}
	for _, level := range []ThinkingLevel{ThinkingOff, ThinkingLow, ThinkingXHigh, ThinkingMax} {
		t.Run(string(level), func(t *testing.T) {
			opts := buildAIOptions(AgentLoopConfig{ThinkingLevel: level, ThinkingBudgets: budgets}, &ai.Model{})
			got := ai.ExtractSimpleStreamOptions(opts)
			require.Equal(t, budgets, got.ThinkingBudgets)
			require.Equal(t, ai.ReasoningLevel(level), got.Reasoning)
		})
	}
}

func TestLoopThinkingBudgetsSurviveRequestRouting(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		t.Run(entry, func(t *testing.T) {
			budgets := &ai.ThinkingBudgets{Low: new(0), High: new(3000)}
			var requests int
			config := AgentLoopConfig{
				Model: &ai.Model{ID: "virtual"}, ThinkingLevel: ThinkingLow, ThinkingBudgets: budgets, ConvertToLlm: DefaultConvertToLlm,
				PrepareRequest: func(context.Context, PrepareRequestContext) (*AgentRequestUpdate, error) {
					return &AgentRequestUpdate{Model: &ai.Model{ID: "physical"}, ThinkingLevel: new(ThinkingXHigh)}, nil
				},
				StreamFn: func(ctx context.Context, model *ai.Model, _ *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					requests++
					got := ai.ExtractSimpleStreamOptions(opts)
					require.Equal(t, "physical", model.ID)
					require.Equal(t, ai.ReasoningXHigh, got.Reasoning)
					require.Equal(t, budgets, got.ThinkingBudgets)
					handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
					return nil
				},
			}
			current := &AgentContext{Messages: []*Message{NewUserMessage("history")}}
			var err error
			if entry == "prompt" {
				_, err = RunAgentLoop(t.Context(), []*Message{NewUserMessage("hello")}, current, config, func(Event) {})
			} else {
				_, err = RunAgentLoopContinue(t.Context(), current, config, func(Event) {})
			}
			require.NoError(t, err)
			require.Equal(t, 1, requests)
		})
	}
}

func TestAgentToolNamespaceSurvivesConversionAndExtraction(t *testing.T) {
	var output ai.AssistantMessage
	require.NoError(t, json.Unmarshal([]byte(`{"role":"assistant","provider":"openai","api":"openai-responses","model":"gpt","content":[{"type":"toolCall","id":"call|fc_1","name":"lookup","arguments":{},"namespace":"dynamic_tools"}]}`), &output))
	message := messageFromAI(&output)
	converted, err := DefaultConvertToLlm(t.Context(), []*Message{message})
	require.NoError(t, err)
	require.Len(t, converted, 1)
	for _, value := range []any{message.Content[0], converted[0].ContentBlocks()[0], extractToolCalls(message)[0]} {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		require.Contains(t, string(data), `"namespace":"dynamic_tools"`)
	}
}

func TestLoopStreamRetainsFirstTerminal(t *testing.T) {
	for _, entry := range []string{"prompt", "continue", "complete"} {
		for _, firstKind := range []string{"done", "error", "aborted", "usage limit"} {
			for _, withStart := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/start=%t", entry, firstKind, withStart), func(t *testing.T) {
					first := ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "first"}}, StopReason: ai.StopReasonStop, Usage: ai.Usage{Input: 7}, Timestamp: 1234567890, DurationMs: new(12.5)}
					var terminal ai.StreamEvent
					var limit error
					switch firstKind {
					case "done":
						terminal = ai.Done{Message: first}
					case "error", "aborted":
						first.StopReason = ai.StopReason(firstKind)
						first.ErrorMessage = "first failure"
						terminal = ai.StreamError{Msg: first, Err: errors.New("first failure")}
					case "usage limit":
						limit = &ai.ClaudeAgentUsageLimitError{Message: "wait", ResetsAt: time.Now().Add(time.Hour)}
						terminal = ai.StreamError{Err: limit}
					}
					late := ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "late"}}, StopReason: ai.StopReasonStop, Usage: ai.Usage{Input: 99}}
					var raw int
					stream := func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
						if withStart {
							handler(ctx, ai.Start{Partial: first})
						}
						handler(ctx, terminal)
						handler(ctx, ai.TextDelta{Partial: late, Delta: "late"})
						handler(ctx, ai.Start{Partial: late})
						handler(ctx, ai.Done{Message: late})
						handler(ctx, ai.StreamError{Msg: late, Err: errors.New("late failure")})
						return nil
					}
					if entry == "complete" {
						if limit != nil {
							return
						}
						got, err := (streamFuncClient{streamFn: stream}).Complete(t.Context(), &ai.Model{}, &ai.Context{})
						require.NoError(t, err)
						require.Equal(t, first, *got)
						return
					}
					var events []Event
					current := &AgentContext{Messages: []*Message{NewUserMessage("history")}}
					config := AgentLoopConfig{Model: &ai.Model{ID: "test"}, ConvertToLlm: DefaultConvertToLlm, StreamFn: stream, OnProviderStreamEvent: func(context.Context, ai.StreamEvent) { raw++ }}
					// Exercise usage-limit settlement without waiting for its retry window.
					if limit != nil {
						_, err := streamAssistantResponse(t.Context(), current, config, streamFuncClient{streamFn: stream}, config.Model, "", nil, func(e Event) { events = append(events, e) })
						require.ErrorIs(t, err, limit)
						want := 1
						if withStart {
							want++
						}
						require.Len(t, current.Messages, want)
						return
					}
					var messages []*Message
					var err error
					if entry == "prompt" {
						messages, err = RunAgentLoop(t.Context(), []*Message{NewUserMessage("hello")}, current, config, func(e Event) { events = append(events, e) })
					} else {
						messages, err = RunAgentLoopContinue(t.Context(), current, config, func(e Event) { events = append(events, e) })
					}
					require.NoError(t, err)
					var starts, ends, updates int
					var published *Message
					for _, event := range events {
						switch e := event.(type) {
						case MessageStart:
							if e.Message.Role == RoleAssistant {
								starts++
							}
						case MessageEnd:
							if e.Message.Role == RoleAssistant {
								ends++
								published = e.Message
							}
						case MessageUpdate:
							updates++
						}
					}
					require.Equal(t, 1, starts)
					require.Equal(t, 1, ends)
					require.Zero(t, updates)
					require.Equal(t, "first", messageText(published))
					require.Equal(t, first.StopReason, published.StopReason)
					require.Equal(t, first.Usage, published.Usage)
					require.Same(t, published, messages[len(messages)-1])
					require.Same(t, published, events[len(events)-1].(AgentEnd).Messages[len(messages)-1])
					wantRaw := 5
					if withStart {
						wantRaw++
					}
					require.Equal(t, wantRaw, raw)
				})
			}
		}
	}
}

func TestRequestPreparationStopsAfterNilErrorCancellation(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		for _, stage := range []string{"client", "transform", "conversion", "context", "context_with_system", "credentials"} {
			for _, outcome := range []string{"cancel", "deadline"} {
				t.Run(entry+"/"+stage+"/"+outcome, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						cause := errors.New("request preparation withdrawn")
						ctx, cancel := context.WithCancelCause(t.Context())
						defer cancel(nil)
						if outcome == "deadline" {
							var stop context.CancelFunc
							ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
							defer stop()
						}
						var calls []string
						visit := func(name string) {
							calls = append(calls, name)
							if name == stage {
								if outcome == "deadline" {
									<-ctx.Done()
								} else {
									cancel(cause)
								}
							}
						}
						client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
							visit("stream")
							handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
							return nil
						}}
						config := AgentLoopConfig{
							Model:         &ai.Model{ID: "local"},
							ClientFactory: func(*ai.Model) (ai.Client, error) { visit("client"); return client, nil },
							TransformContext: func(_ context.Context, messages []*Message) ([]*Message, error) {
								visit("transform")
								return messages, nil
							},
							ConvertToLlm: func(ctx context.Context, messages []*Message) ([]ai.Message, error) {
								visit("conversion")
								return ConvertMessagesToAI(messages), nil
							},
							GetAPIKey: func(context.Context, string) (string, error) { visit("credentials"); return "key", nil },
							EventDispatcher: func(_ context.Context, name string, _ []byte, _ string) EventDispatchResult {
								if name == "context" || name == "context_with_system" {
									visit(name)
								}
								return EventDispatchResult{}
							},
						}
						var events []Event
						sink := func(event Event) { events = append(events, event) }
						current := &AgentContext{Messages: []*Message{NewUserMessage("history")}}
						var messages []*Message
						var err error
						if entry == "prompt" {
							messages, err = RunAgentLoop(ctx, []*Message{NewUserMessage("hello")}, current, config, sink)
						} else {
							messages, err = RunAgentLoopContinue(ctx, current, config, sink)
						}
						require.ErrorIs(t, err, cause)
						require.ErrorIs(t, err, ctx.Err())
						order := []string{"client", "transform", "conversion", "context", "context_with_system", "credentials", "stream"}
						require.Equal(t, order[:slices.Index(order, stage)+1], calls)
						var ends int
						for _, event := range events {
							if end, ok := event.(MessageEnd); ok {
								require.NotEqual(t, RoleAssistant, end.Message.Role)
							}
							if _, ok := event.(AgentEnd); ok {
								ends++
							}
						}
						require.Equal(t, 1, ends)
						require.Equal(t, messages, events[len(events)-1].(AgentEnd).Messages)
					})
				})
			}
		}
	}
}

func TestStartupCancellationDiscardsNilErrorCallbackResults(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		for _, stage := range []string{"supplier", "before_agent_start", "input_transform", "input_handled", "input_blocked"} {
			for _, outcome := range []string{"cancel", "deadline"} {
				t.Run(entry+"/"+stage+"/"+outcome, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						cause := errors.New("startup withdrawn")
						ctx, cancel := context.WithCancelCause(t.Context())
						defer cancel(nil)
						if outcome == "deadline" {
							var stop context.CancelFunc
							ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
							defer stop()
						}
						var calls []string
						visit := func(name string) {
							calls = append(calls, name)
							if name == stage {
								if outcome == "deadline" {
									<-ctx.Done()
								} else {
									cancel(cause)
								}
							}
						}
						prompt := NewUserMessage("original")
						startup := NewUserMessage("pending")
						var events []Event
						config := AgentLoopConfig{
							Model: &ai.Model{ID: "local"}, ConvertToLlm: DefaultConvertToLlm,
							GetStartupMessages: func(context.Context) ([]*Message, error) {
								visit("supplier")
								return []*Message{startup}, nil
							},
							EventDispatcher: func(_ context.Context, name string, _ []byte, _ string) EventDispatchResult {
								switch name {
								case "before_agent_start":
									visit(name)
								case "input":
									if strings.HasPrefix(stage, "input_") {
										visit(stage)
									} else {
										visit("input")
									}
									if stage == "input_blocked" {
										return EventDispatchResult{Blocked: true}
									}
									action := "transform"
									if stage == "input_handled" {
										action = "handled"
									}
									return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: json.RawMessage(`{"action":"` + action + `","text":"replacement"}`)}}}
								}
								return EventDispatchResult{}
							},
							GetSteeringMessages: func(context.Context) ([]*Message, error) { visit("steering"); return nil, nil },
							StreamFn: func(context.Context, *ai.Model, *ai.Context, ai.EventHandler, ...ai.Option) error {
								t.Error("cancelled startup reached stream")
								return nil
							},
						}
						current := &AgentContext{Messages: []*Message{NewUserMessage("history")}}
						sink := func(event Event) { events = append(events, event) }
						var err error
						if entry == "prompt" {
							_, err = RunAgentLoop(ctx, []*Message{prompt}, current, config, sink)
						} else {
							_, err = RunAgentLoopContinue(ctx, current, config, sink)
						}
						require.ErrorIs(t, err, cause)
						require.ErrorIs(t, err, ctx.Err())
						want := []string{"supplier"}
						if stage != "supplier" {
							want = append(want, "before_agent_start")
						}
						if strings.HasPrefix(stage, "input_") {
							want = append(want, stage)
						}
						require.Equal(t, want, calls)
						require.Equal(t, "original", agentInputText(prompt))
						require.Equal(t, "pending", agentInputText(startup))
						var types []string
						for _, event := range events {
							types = append(types, fmt.Sprintf("%T", event))
						}
						if strings.HasPrefix(stage, "input_") {
							require.Equal(t, []string{"agent.AgentStart", "agent.TurnStart"}, types)
						} else {
							require.Empty(t, types)
						}
					})
				})
			}
		}
	}
}

func TestAlreadyCancelledStartupDoesNotAdmitCallbacks(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		t.Run(entry, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			cause := errors.New("startup cancelled before entry")
			cancel(cause)
			config := AgentLoopConfig{
				Model: &ai.Model{ID: "local"}, ConvertToLlm: DefaultConvertToLlm,
				GetStartupMessages: func(context.Context) ([]*Message, error) {
					t.Error("cancelled invocation admitted startup supplier")
					return nil, nil
				},
				EventDispatcher: func(context.Context, string, []byte, string) EventDispatchResult {
					t.Error("cancelled invocation admitted plugin dispatcher")
					return EventDispatchResult{}
				},
			}
			sink := func(Event) { t.Error("cancelled invocation published startup events") }
			current := &AgentContext{Messages: []*Message{NewUserMessage("history")}}
			var err error
			if entry == "prompt" {
				_, err = RunAgentLoop(ctx, []*Message{NewUserMessage("hello")}, current, config, sink)
			} else {
				_, err = RunAgentLoopContinue(ctx, current, config, sink)
			}
			require.ErrorIs(t, err, cause)
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestQueueCancellationDiscardsSupplierResults(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		for _, stage := range []string{"initial steering", "turn steering", "late steering", "follow-up"} {
			for _, outcome := range []string{"cancel", "deadline"} {
				for _, result := range []string{"empty", "message"} {
					t.Run(entry+"/"+stage+"/"+outcome+"/"+result, func(t *testing.T) {
						synctest.Test(t, func(t *testing.T) {
							ctx, cancel := context.WithCancelCause(t.Context())
							defer cancel(nil)
							cause := errors.New("queue owner withdrew admission")
							if outcome == "deadline" {
								var stop context.CancelFunc
								ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
								defer stop()
							}
							var polls, streams, preparations, followUps, settlements int
							queued := NewUserMessage("must not publish")
							withdraw := func() ([]*Message, error) {
								if outcome == "deadline" {
									<-ctx.Done()
								} else {
									cancel(cause)
								}
								if result == "message" {
									return []*Message{queued}, nil
								}
								return nil, nil
							}
							config := AgentLoopConfig{
								Model: &ai.Model{ID: "local"}, ConvertToLlm: DefaultConvertToLlm,
								GetSteeringMessages: func(context.Context) ([]*Message, error) {
									polls++
									if stage == "initial steering" && polls == 1 || stage == "turn steering" && polls == 2 || stage == "late steering" && polls == 3 {
										return withdraw()
									}
									return nil, nil
								},
								GetFollowUpMessages: func(context.Context) ([]*Message, error) {
									followUps++
									if stage == "follow-up" {
										return withdraw()
									}
									return nil, nil
								},
								FinishTurn: func(context.Context, AgentTurnContext) (TurnDecision, error) {
									if stage == "late steering" && streams == 1 {
										return TurnContinue, nil
									}
									return "", nil
								},
								PrepareNextTurn: func(context.Context, AgentTurnContext) (*AgentTurnUpdate, error) { preparations++; return nil, nil },
								EventDispatcher: func(_ context.Context, name string, _ []byte, _ string) EventDispatchResult {
									if name == "agent_before_settle" {
										settlements++
									}
									return EventDispatchResult{}
								},
								StreamFn: func(callCtx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
									streams++
									handler(callCtx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
									return nil
								},
							}
							var events []Event
							sink := func(event Event) { events = append(events, event) }
							current := &AgentContext{Messages: []*Message{NewUserMessage("history")}}
							var messages []*Message
							var err error
							if entry == "prompt" {
								messages, err = RunAgentLoop(ctx, []*Message{NewUserMessage("hello")}, current, config, sink)
							} else {
								messages, err = RunAgentLoopContinue(ctx, current, config, sink)
							}
							require.ErrorIs(t, err, cause)
							require.ErrorIs(t, err, ctx.Err())
							wantStreams, wantPolls := 1, 2
							if stage == "initial steering" {
								wantStreams, wantPolls = 0, 1
							}
							if stage == "late steering" {
								wantPolls = 3
							}
							require.Equal(t, wantStreams, streams)
							require.Equal(t, wantPolls, polls)
							wantPreparations, wantFollowUps := 0, 0
							if stage == "late steering" {
								wantPreparations, wantFollowUps = 1, 1
							}
							if stage == "follow-up" {
								wantFollowUps = 1
							}
							require.Equal(t, wantPreparations, preparations)
							require.Equal(t, wantFollowUps, followUps)
							require.Zero(t, settlements)
							for _, msg := range messages {
								require.NotSame(t, queued, msg)
							}
							var ends int
							for _, event := range events {
								switch e := event.(type) {
								case MessageStart:
									require.NotSame(t, queued, e.Message)
								case MessageEnd:
									require.NotSame(t, queued, e.Message)
								case AgentEnd:
									ends++
									require.Equal(t, messages, e.Messages)
								}
							}
							require.Equal(t, 1, ends)
						})
					})
				}
			}
		}
	}
}

func TestCancelledQueuePollDoesNotInvokeSupplier(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, queue := range []string{"steering", "follow-up"} {
		t.Run(queue, func(t *testing.T) {
			for _, configured := range []bool{false, true} {
				var config AgentLoopConfig
				if configured {
					supplier := func(context.Context) ([]*Message, error) { t.Error("admitted cancelled supplier"); return nil, nil }
					config.GetSteeringMessages, config.GetFollowUpMessages = supplier, supplier
				}
				var messages []*Message
				var err error
				if queue == "steering" {
					messages, err = getSteeringMessages(ctx, config)
				} else {
					messages, err = getFollowUpMessages(ctx, config)
				}
				require.ErrorIs(t, err, context.Canceled)
				require.Empty(t, messages)
			}
		})
	}
}

func TestPreparationCancellationDiscardsReturnedUpdates(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		for _, stage := range []string{"request", "next turn"} {
			for _, outcome := range []string{"cancel", "deadline"} {
				for _, result := range []string{"nil", "empty", "message"} {
					t.Run(entry+"/"+stage+"/"+outcome+"/"+result, func(t *testing.T) {
						synctest.Test(t, func(t *testing.T) {
							ctx, cancel := context.WithCancelCause(t.Context())
							defer cancel(nil)
							cause := errors.New("preparation admission withdrawn")
							if outcome == "deadline" {
								var stop context.CancelFunc
								ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
								defer stop()
							}
							withdraw := func() {
								if outcome == "deadline" {
									<-ctx.Done()
								} else {
									cancel(cause)
								}
							}
							returned := NewUserMessage("rejected preparation message")
							projection := &AgentContext{SystemPrompt: "rejected projection"}
							var messages []*Message
							if result == "message" {
								messages = []*Message{returned}
							}
							var streams, clients, requests, preparations, starts int
							config := AgentLoopConfig{
								Model: &ai.Model{ID: "local"}, ConvertToLlm: DefaultConvertToLlm,
								ClientFactory: func(*ai.Model) (ai.Client, error) {
									clients++
									return stubClient{stream: func(callCtx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
										streams++
										handler(callCtx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
										return nil
									}}, nil
								},
								PrepareRequest: func(context.Context, PrepareRequestContext) (*AgentRequestUpdate, error) {
									requests++
									if stage != "request" {
										return nil, nil
									}
									withdraw()
									if result == "nil" {
										return nil, nil
									}
									return &AgentRequestUpdate{Messages: messages, Context: projection, Model: &ai.Model{ID: "rejected"}, ThinkingLevel: new(ThinkingHigh)}, nil
								},
								PrepareNextTurn: func(context.Context, AgentTurnContext) (*AgentTurnUpdate, error) {
									preparations++
									withdraw()
									if result == "nil" {
										return nil, nil
									}
									return &AgentTurnUpdate{Messages: messages, Context: projection, Model: &ai.Model{ID: "rejected"}, ThinkingLevel: new(ThinkingHigh)}, nil
								},
								FinishTurn: func(context.Context, AgentTurnContext) (TurnDecision, error) { return TurnContinue, nil },
							}
							var events []Event
							sink := func(event Event) { events = append(events, event) }
							current := &AgentContext{Messages: []*Message{NewUserMessage("history")}}
							var got []*Message
							var err error
							if entry == "prompt" {
								got, err = RunAgentLoop(ctx, []*Message{NewUserMessage("hello")}, current, config, sink)
							} else {
								got, err = RunAgentLoopContinue(ctx, current, config, sink)
							}
							require.ErrorIs(t, err, ctx.Err())
							require.ErrorIs(t, err, cause)
							wantStreams, wantPreparations := 0, 0
							if stage == "next turn" {
								wantStreams, wantPreparations = 1, 1
							}
							require.Equal(t, wantStreams, streams)
							require.Equal(t, wantStreams, clients)
							require.Equal(t, wantPreparations, preparations)
							require.Equal(t, 1, requests)
							for _, message := range got {
								require.NotSame(t, returned, message)
							}
							var ends int
							for _, event := range events {
								switch e := event.(type) {
								case MessageStart:
									require.NotSame(t, returned, e.Message)
								case MessageEnd:
									require.NotSame(t, returned, e.Message)
								case TurnStart:
									starts++
								case AgentEnd:
									ends++
									require.Equal(t, got, e.Messages)
								}
							}
							require.Equal(t, 1, starts)
							require.Equal(t, 1, ends)
							require.Empty(t, projection.Messages)
						})
					})
				}
			}
		}
	}
}

func TestBeforeSettleCancellationRejectsResponses(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		for _, outcome := range []string{"cancel", "deadline"} {
			for _, response := range []string{"nil", "empty", "continue", "messages", "malformed"} {
				t.Run(entry+"/"+outcome+"/"+response, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						ctx, cancel := context.WithCancelCause(t.Context())
						defer cancel(nil)
						cause := errors.New("settlement admission withdrawn")
						if outcome == "deadline" {
							var stop context.CancelFunc
							ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
							defer stop()
						}
						var data []byte
						switch response {
						case "empty":
							data = []byte(`{}`)
						case "continue":
							data = []byte(`{"continue":true}`)
						case "messages":
							data = []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"rejected settlement"}]}]}`)
						case "malformed":
							data = []byte(`{`)
						}
						var streams, settlements, preparations int
						config := AgentLoopConfig{
							Model: &ai.Model{ID: "local"}, ConvertToLlm: DefaultConvertToLlm,
							StreamFn: func(callCtx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
								streams++
								handler(callCtx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
								return nil
							},
							PrepareNextTurn: func(context.Context, AgentTurnContext) (*AgentTurnUpdate, error) { preparations++; return nil, nil },
							EventDispatcher: func(_ context.Context, name string, _ []byte, _ string) EventDispatchResult {
								if name != "agent_before_settle" {
									return EventDispatchResult{}
								}
								settlements++
								if outcome == "deadline" {
									<-ctx.Done()
								} else {
									cancel(cause)
								}
								return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: data}}}
							},
						}
						var events []Event
						sink := func(event Event) { events = append(events, event) }
						current := &AgentContext{Messages: []*Message{NewUserMessage("history")}}
						var messages []*Message
						var err error
						if entry == "prompt" {
							messages, err = RunAgentLoop(ctx, []*Message{NewUserMessage("hello")}, current, config, sink)
						} else {
							messages, err = RunAgentLoopContinue(ctx, current, config, sink)
						}
						var starts, ends, assistants int
						for _, event := range events {
							switch e := event.(type) {
							case TurnStart:
								starts++
							case AgentEnd:
								ends++
								require.Equal(t, messages, e.Messages)
							case MessageStart:
								require.NotEqual(t, "rejected settlement", agentInputText(e.Message))
							case MessageEnd:
								require.NotEqual(t, "rejected settlement", agentInputText(e.Message))
								if e.Message.Role == RoleAssistant {
									assistants++
									require.Equal(t, ai.StopReasonStop, e.Message.StopReason)
								}
							}
						}
						require.Equal(t, 0, preparations)
						require.Equal(t, 1, starts)
						require.Equal(t, 1, ends)
						require.Equal(t, 1, assistants)
						require.Equal(t, 1, settlements)
						require.Equal(t, 1, streams)
						for _, message := range messages {
							require.NotEqual(t, "rejected settlement", agentInputText(message))
						}
						require.ErrorIs(t, err, ctx.Err())
						require.ErrorIs(t, err, cause)
						require.NotContains(t, err.Error(), "parse settlement")
					})
				})
			}
		}
	}
}

func TestPostResponseCancellationStopsToolAdmission(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		for _, stage := range []string{"after_provider_response", "tool_call", "user_bash"} {
			for _, outcome := range []string{"cancel", "deadline"} {
				for _, verdict := range []string{"block", "override"} {
					t.Run(entry+"/"+stage+"/"+outcome+"/"+verdict, func(t *testing.T) {
						synctest.Test(t, func(t *testing.T) {
							ctx, cancel := context.WithCancelCause(t.Context())
							defer cancel(nil)
							cause := errors.New("tool admission withdrawn")
							if outcome == "deadline" {
								var stop context.CancelFunc
								ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
								defer stop()
							}
							var streams, executions, finishes int
							var dispatches []string
							config := AgentLoopConfig{
								Model: &ai.Model{ID: "local"}, ConvertToLlm: DefaultConvertToLlm,
								StreamFn: func(callCtx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
									streams++
									handler(callCtx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonToolUse, Content: []ai.ContentBlock{
										{Type: ai.ContentBlockToolCall, ID: "bash-1", Name: "bash", Arguments: []byte(`{}`)},
										{Type: ai.ContentBlockToolCall, ID: "bash-2", Name: "bash", Arguments: []byte(`{}`)},
									}}})
									return nil
								},
								FinishTurn: func(context.Context, AgentTurnContext) (TurnDecision, error) { finishes++; return TurnEndRun, nil },
								EventDispatcher: func(_ context.Context, name string, _ []byte, _ string) EventDispatchResult {
									dispatches = append(dispatches, name)
									if name != stage {
										return EventDispatchResult{}
									}
									if outcome == "deadline" {
										<-ctx.Done()
									} else {
										cancel(cause)
									}
									if verdict == "block" {
										return EventDispatchResult{Blocked: true}
									}
									return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: []byte(`{"result":{"output":"must not publish","exitCode":0}}`)}}}
								},
							}
							current := &AgentContext{Messages: []*Message{NewUserMessage("history")}, Tools: []Tool{{Name: "bash", Parameters: map[string]any{}, Execute: func(ToolContext) (*ToolResult, error) { executions++; return &ToolResult{Content: "executed"}, nil }}}}
							var events []Event
							sink := func(event Event) { events = append(events, event) }
							var messages []*Message
							var err error
							if entry == "prompt" {
								messages, err = RunAgentLoop(ctx, []*Message{NewUserMessage("hello")}, current, config, sink)
							} else {
								messages, err = RunAgentLoopContinue(ctx, current, config, sink)
							}
							require.ErrorIs(t, err, ctx.Err())
							require.ErrorIs(t, err, cause)
							require.Equal(t, 0, finishes)
							require.Equal(t, 0, executions)
							require.Equal(t, 1, streams)
							var assistants, ends int
							for _, event := range events {
								switch e := event.(type) {
								case ToolStart, ToolEnd:
									t.Fatalf("tool lifecycle admitted after cancellation: %T", event)
								case MessageEnd:
									require.NotEqual(t, RoleTool, e.Message.Role)
									if e.Message.Role == RoleAssistant {
										assistants++
										require.Equal(t, ai.StopReasonToolUse, e.Message.StopReason)
									}
								case AgentEnd:
									ends++
									require.Equal(t, messages, e.Messages)
								}
							}
							require.Equal(t, 1, assistants)
							require.Equal(t, 1, ends)
							require.Equal(t, RoleAssistant, messages[len(messages)-1].Role)
							index := slices.Index(dispatches, stage)
							require.NotEqual(t, -1, index)
							require.Equal(t, []string{"agent_end"}, dispatches[index+1:])
						})
					})
				}
			}
		}
	}
}

func TestArgumentPreparationCancellationStopsAdmission(t *testing.T) {
	for _, mode := range []string{"sequential", "parallel", "direct", "nested"} {
		for _, outcome := range []string{"cancel", "deadline"} {
			for _, returned := range []string{"valid", "invalid", "unencodable", "invalid schema"} {
				t.Run(mode+"/"+outcome+"/"+returned, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						ctx, cancel := context.WithCancelCause(t.Context())
						defer cancel(nil)
						cause := errors.New("argument preparation withdrawn")
						if outcome == "deadline" {
							var stop context.CancelFunc
							ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
							defer stop()
						}
						var preparations, hooks, executions, readiness, reaches, afters int
						tool := Tool{
							Name: "target", Parameters: map[string]any{"type": "object", "required": []string{"path"}, "properties": map[string]any{"path": map[string]any{"type": "string"}}},
							PrepareArguments: func(map[string]any) map[string]any {
								preparations++
								if outcome == "deadline" {
									<-ctx.Done()
								} else {
									cancel(cause)
								}
								synctest.Wait()
								switch returned {
								case "invalid":
									return map[string]any{}
								case "unencodable":
									return map[string]any{"path": make(chan int)}
								default:
									return map[string]any{"path": "not admitted"}
								}
							},
							WaitReady:  func(context.Context) error { readiness++; return nil },
							ReachTools: func(context.Context, map[string]any) ([]string, error) { reaches++; return nil, nil },
							Execute:    func(ToolContext) (*ToolResult, error) { executions++; return &ToolResult{Content: "executed"}, nil },
						}
						if returned == "invalid schema" {
							tool.Parameters["type"] = "not a schema type"
						}
						config := AgentLoopConfig{
							BeforeToolCall: func(_ context.Context, call ToolCallContext) (BeforeToolCallResult, error) {
								if call.ToolName == "target" {
									hooks++
								}
								return BeforeToolCallResult{}, nil
							},
							AfterToolCall: func(_ context.Context, call ToolCallContext, _ *ToolResult) (AfterToolCallResult, error) {
								if call.ToolName == "target" {
									afters++
								}
								return AfterToolCallResult{}, nil
							},
						}
						current := &AgentContext{Tools: []Tool{tool}}
						calls := []ai.ToolCall{{ID: "first", Name: "target", Arguments: json.RawMessage(`{}`)}, {ID: "later", Name: "target", Arguments: json.RawMessage(`{}`)}}
						var results []*ToolResult
						var events []Event
						emit := func(event Event) { events = append(events, event) }
						switch mode {
						case "sequential":
							results = executeSequential(ctx, current, nil, calls, config, emit)
						case "parallel":
							results = executeParallel(ctx, current, nil, calls, config, emit)
						case "direct":
							results = []*ToolResult{RunToolCall(ctx, current, nil, calls[0], config)}
						case "nested":
							caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"*"} }, Execute: func(call ToolContext) (*ToolResult, error) {
								results = append(results, call.ExecuteTool(calls[0]))
								return &ToolResult{}, nil
							}}
							current.Tools = append(current.Tools, caller)
							outer := RunToolCall(ctx, current, nil, ai.ToolCall{ID: "outer", Name: "caller", Arguments: json.RawMessage(`{}`)}, config)
							require.Len(t, outer.NestedCalls, 1)
							require.Equal(t, "cancelled", outer.NestedCalls[0].Status)
						}
						require.Equal(t, 1, preparations)
						require.Zero(t, hooks, "cancelled preparation admitted BeforeToolCall")
						require.Zero(t, executions)
						if mode == "nested" {
							require.Equal(t, 1, readiness, "outer call checks reachable readiness before cancellation")
						} else {
							require.Zero(t, readiness)
						}
						require.Zero(t, reaches)
						require.Zero(t, afters)
						require.Len(t, results, 1)
						result := results[0]
						require.True(t, result.IsError)
						require.ErrorIs(t, result.Rejection, ctx.Err())
						require.ErrorIs(t, result.Rejection, cause)
						require.Equal(t, ctx.Err().Error(), result.Content)
						require.Nil(t, result.DurationMs)
						if mode == "sequential" || mode == "parallel" {
							var order []string
							for _, event := range events {
								switch e := event.(type) {
								case ToolStart:
									order = append(order, "start:"+e.ToolCallID)
								case ToolEnd:
									order = append(order, "end:"+e.ToolCallID)
									require.Same(t, result, e.Result)
								case MessageEnd:
									order = append(order, "message:"+e.Message.ToolCallID)
									require.Same(t, result.Message, e.Message)
								}
							}
							require.Equal(t, []string{"start:first", "end:first", "message:first"}, order)
						}
					})
				})
			}
		}
	}
}

func TestToolPreflightRejectionRetainsErrorIdentities(t *testing.T) {
	for _, mode := range []string{"sequential", "parallel", "direct", "nested"} {
		for _, outcome := range []string{"error", "cancel allow", "cancel block", "cancel error", "deadline allow", "deadline block", "deadline error", "already cancelled"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancelCause(t.Context())
					defer cancel(nil)
					cause := errors.New("tool permission withdrawn")
					hookErr := errors.New("permission callback failed")
					if strings.HasPrefix(outcome, "deadline") {
						var stop context.CancelFunc
						ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
						defer stop()
					}
					var hooks, preparations, readiness, reaches, executions, afters int
					tool := Tool{
						Name: "target", Parameters: map[string]any{"type": "object"},
						PrepareArguments: func(args map[string]any) map[string]any { preparations++; return args },
						WaitReady:        func(context.Context) error { readiness++; return nil },
						ReachTools:       func(context.Context, map[string]any) ([]string, error) { reaches++; return nil, nil },
						Execute:          func(ToolContext) (*ToolResult, error) { executions++; return &ToolResult{}, nil },
					}
					config := AgentLoopConfig{
						BeforeToolCall: func(_ context.Context, call ToolCallContext) (BeforeToolCallResult, error) {
							if call.ToolName != "target" {
								return BeforeToolCallResult{}, nil
							}
							hooks++
							if strings.HasPrefix(outcome, "deadline") {
								<-ctx.Done()
							} else if strings.HasPrefix(outcome, "cancel") {
								cancel(cause)
							}
							synctest.Wait()
							if strings.HasSuffix(outcome, "error") {
								return BeforeToolCallResult{Block: true, Terminate: true}, fmt.Errorf("permission check: %w", hookErr)
							}
							return BeforeToolCallResult{Block: strings.HasSuffix(outcome, "block"), Terminate: true, Reason: "denied"}, nil
						},
						AfterToolCall: func(_ context.Context, call ToolCallContext, _ *ToolResult) (AfterToolCallResult, error) {
							if call.ToolName == "target" {
								afters++
							}
							return AfterToolCallResult{}, nil
						},
					}
					current := &AgentContext{Tools: []Tool{tool}}
					call := ai.ToolCall{ID: "first", Name: "target", Arguments: json.RawMessage(`{}`)}
					if outcome == "already cancelled" && mode != "nested" {
						cancel(cause)
					}
					var results []*ToolResult
					var order []string
					emit := func(event Event) {
						switch e := event.(type) {
						case ToolStart:
							order = append(order, "start:"+e.ToolCallID)
						case ToolEnd:
							order = append(order, "end:"+e.ToolCallID)
							require.Nil(t, e.DurationMs)
						case MessageEnd:
							order = append(order, "message:"+e.Message.ToolCallID)
						}
					}
					calls := []ai.ToolCall{call}
					if outcome != "error" {
						later := call
						later.ID = "later"
						calls = append(calls, later)
					}
					switch mode {
					case "sequential":
						results = executeSequential(ctx, current, nil, calls, config, emit)
					case "parallel":
						results = executeParallel(ctx, current, nil, calls, config, emit)
					case "direct":
						results = []*ToolResult{RunToolCall(ctx, current, nil, call, config)}
					case "nested":
						caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"target"} }, Execute: func(tc ToolContext) (*ToolResult, error) {
							if outcome == "already cancelled" {
								cancel(cause)
								synctest.Wait()
							}
							results = append(results, tc.ExecuteTool(call))
							return &ToolResult{}, nil
						}}
						current.Tools = append(current.Tools, caller)
						outer := RunToolCall(ctx, current, nil, ai.ToolCall{ID: "outer", Name: "caller", Arguments: json.RawMessage(`{}`)}, config)
						require.Len(t, outer.NestedCalls, 1)
						status := "cancelled"
						if outcome == "error" {
							status = "error"
						}
						assert.Equal(t, status, outer.NestedCalls[0].Status)
					}
					require.Len(t, results, 1)
					result := results[0]
					require.True(t, result.IsError)
					require.False(t, result.Terminate)
					require.Nil(t, result.DurationMs)
					if strings.HasSuffix(outcome, "error") {
						assert.ErrorIs(t, result.Rejection, hookErr)
						assert.Equal(t, "before tool call: permission check: permission callback failed", result.Content)
					} else {
						assert.Equal(t, ctx.Err().Error(), result.Content)
					}
					if outcome != "error" {
						assert.ErrorIs(t, result.Rejection, ctx.Err())
						assert.ErrorIs(t, result.Rejection, cause)
					}
					expectedHooks := 1
					if outcome == "already cancelled" {
						expectedHooks = 0
					}
					require.Equal(t, expectedHooks, hooks)
					require.Equal(t, expectedHooks, preparations)
					expectedReady := 0
					if mode == "nested" {
						expectedReady = 1
					}
					require.Equal(t, expectedReady, readiness)
					require.Zero(t, reaches)
					require.Zero(t, executions)
					require.Zero(t, afters)
					if mode == "sequential" || mode == "parallel" {
						require.Equal(t, []string{"start:first", "end:first", "message:first"}, order)
					}
				})
			})
		}
	}
}

func TestToolReadinessCancellationStopsAdmission(t *testing.T) {
	for _, mode := range []string{"sequential", "parallel", "direct", "nested"} {
		for _, stage := range []string{"own readiness", "reach", "legacy reach", "first reachable readiness", "last reachable readiness"} {
			for _, outcome := range []string{"cancel", "deadline"} {
				for _, returned := range []string{"nil", "error"} {
					if stage == "legacy reach" && returned == "error" {
						continue
					}
					t.Run(mode+"/"+stage+"/"+outcome+"/"+returned, func(t *testing.T) {
						synctest.Test(t, func(t *testing.T) {
							ctx, cancel := context.WithCancelCause(t.Context())
							defer cancel(nil)
							cause := errors.New("tool readiness withdrawn")
							failure := errors.New("admission callback failed")
							if outcome == "deadline" {
								var stop context.CancelFunc
								ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
								defer stop()
							}
							withdraw := func() error {
								if outcome == "deadline" {
									<-ctx.Done()
								} else {
									cancel(cause)
								}
								synctest.Wait()
								if returned == "error" {
									return failure
								}
								return nil
							}
							armed := mode != "nested"
							var ready, reaches, childReady, executions, afters int
							tool := Tool{
								Name: "target", Parameters: map[string]any{"type": "object"},
								WaitReady: func(context.Context) error {
									if !armed {
										return nil
									}
									ready++
									if stage == "own readiness" {
										return withdraw()
									}
									return nil
								},
								ReachTools: func(context.Context, map[string]any) ([]string, error) {
									reaches++
									if stage == "reach" {
										return []string{"child0", "child1"}, withdraw()
									}
									return []string{"child0", "child1"}, nil
								},
								Execute: func(ToolContext) (*ToolResult, error) { executions++; return &ToolResult{Content: "not admitted"}, nil },
							}
							if stage == "legacy reach" {
								tool.ReachTools = nil
								tool.ReachesTools = func(map[string]any) []string {
									reaches++
									require.NoError(t, withdraw())
									return []string{"child0", "child1"}
								}
							}
							tools := []Tool{tool}
							for i := range 2 {
								tools = append(tools, Tool{Name: fmt.Sprintf("child%d", i), Parameters: map[string]any{"type": "object"}, WaitReady: func(context.Context) error {
									childReady++
									if (stage == "first reachable readiness" && i == 0) || (stage == "last reachable readiness" && i == 1) {
										return withdraw()
									}
									return nil
								}})
							}
							current := &AgentContext{Tools: tools}
							config := AgentLoopConfig{AfterToolCall: func(_ context.Context, call ToolCallContext, _ *ToolResult) (AfterToolCallResult, error) {
								if call.ToolName == "target" {
									afters++
								}
								return AfterToolCallResult{}, nil
							}}
							call := ai.ToolCall{ID: "first", Name: "target", Arguments: json.RawMessage(`{}`)}
							var results []*ToolResult
							var order []string
							emit := func(event Event) {
								switch e := event.(type) {
								case ToolStart:
									order = append(order, "start:"+e.ToolCallID)
								case ToolEnd:
									order = append(order, "end:"+e.ToolCallID)
									assert.Nil(t, e.DurationMs)
								case MessageEnd:
									order = append(order, "message:"+e.Message.ToolCallID)
								}
							}
							switch mode {
							case "sequential":
								results = executeSequential(ctx, current, nil, []ai.ToolCall{call}, config, emit)
							case "parallel":
								results = executeParallel(ctx, current, nil, []ai.ToolCall{call}, config, emit)
							case "direct":
								results = []*ToolResult{RunToolCall(ctx, current, nil, call, config)}
							case "nested":
								caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"target", "child0", "child1"} }, Execute: func(tc ToolContext) (*ToolResult, error) {
									armed = true
									results = append(results, tc.ExecuteTool(call))
									return &ToolResult{}, nil
								}}
								for i := 1; i < len(current.Tools); i++ {
									wait := current.Tools[i].WaitReady
									current.Tools[i].WaitReady = func(c context.Context) error {
										if !armed {
											return nil
										}
										return wait(c)
									}
								}
								current.Tools = append(current.Tools, caller)
								outer := RunToolCall(ctx, current, nil, ai.ToolCall{ID: "outer", Name: "caller", Arguments: json.RawMessage(`{}`)}, config)
								require.Len(t, outer.NestedCalls, 1)
								assert.Equal(t, "cancelled", outer.NestedCalls[0].Status)
								assert.Nil(t, outer.NestedCalls[0].DurationMs)
							}
							require.Len(t, results, 1)
							result := results[0]
							assert.True(t, result.IsError)
							assert.False(t, result.Terminate)
							assert.Nil(t, result.DurationMs)
							assert.ErrorIs(t, result.Rejection, ctx.Err())
							assert.ErrorIs(t, result.Rejection, cause)
							if returned == "error" {
								assert.ErrorIs(t, result.Rejection, failure)
								prefix := "tool readiness: "
								switch stage {
								case "reach":
									prefix = "evaluate tool reach: "
								case "first reachable readiness":
									prefix = "reachable tool child0 readiness: "
								case "last reachable readiness":
									prefix = "reachable tool child1 readiness: "
								}
								assert.Equal(t, prefix+failure.Error(), result.Content)
							} else {
								assert.Equal(t, ctx.Err().Error(), result.Content)
							}
							assert.Equal(t, 1, ready)
							expectedReach := 1
							if stage == "own readiness" {
								expectedReach = 0
							}
							assert.Equal(t, expectedReach, reaches)
							var expectedChildren int
							if stage == "first reachable readiness" {
								expectedChildren = 1
							} else if stage == "last reachable readiness" {
								expectedChildren = 2
							}
							assert.Equal(t, expectedChildren, childReady)
							assert.Zero(t, executions)
							assert.Zero(t, afters)
							if mode == "sequential" || mode == "parallel" {
								assert.Equal(t, []string{"start:first", "end:first", "message:first"}, order)
							}
						})
					})
				}
			}
		}
	}
}

func TestPreparedToolEntryCancellationRetainsCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("prepared execution withdrawn")
	cancel(cause)
	result := executePreparedTool(ctx, nil, nil, preparedToolCall{}, AgentLoopConfig{}, func(Event) { t.Error("cancelled execution emitted event") })
	require.ErrorIs(t, result.Rejection, context.Canceled)
	require.ErrorIs(t, result.Rejection, cause)
	require.Nil(t, result.DurationMs)
}

func TestAfterToolFailureDiscardsReturnedOverrides(t *testing.T) {
	for _, mode := range []string{"sequential", "parallel", "direct", "nested"} {
		for _, outcome := range []string{"ordinary", "cancel", "deadline"} {
			for _, returned := range []string{"empty", "override"} {
				t.Run(mode+"/"+outcome+"/"+returned, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						ctx, cancel := context.WithCancelCause(t.Context())
						defer cancel(nil)
						cause := errors.New("after hook withdrawn")
						failure := errors.New("after hook failed")
						if outcome == "deadline" {
							var stop context.CancelFunc
							ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
							defer stop()
						}
						var executions, hooks int
						leaf := Tool{Name: "leaf", Exposure: ToolDeferred, Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) {
							return &ToolResult{Usage: &ai.Usage{Input: 3}}, nil
						}}
						target := Tool{Name: "target", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"leaf"} }, Execute: func(tc ToolContext) (*ToolResult, error) {
							executions++
							child := tc.ExecuteTool(ai.ToolCall{ID: "leaf-call", Name: "leaf", Arguments: json.RawMessage(`{}`)})
							require.NoError(t, child.Rejection)
							time.Sleep(2 * time.Millisecond)
							return &ToolResult{Content: "executed", StructuredContent: json.RawMessage(`{"ok":true}`), Details: "executed details", Usage: &ai.Usage{Input: 2}}, nil
						}}
						current := &AgentContext{Tools: []Tool{target, leaf}}
						config := AgentLoopConfig{AfterToolCall: func(_ context.Context, call ToolCallContext, result *ToolResult) (AfterToolCallResult, error) {
							if call.ToolName != "target" {
								return AfterToolCallResult{}, nil
							}
							hooks++
							require.Equal(t, 5, result.Usage.Input)
							require.Len(t, result.NestedCalls, 1)
							if outcome == "cancel" {
								cancel(cause)
							} else if outcome == "deadline" {
								<-ctx.Done()
							}
							synctest.Wait()
							time.Sleep(5 * time.Second)
							if returned == "override" {
								return AfterToolCallResult{ContentBlocks: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "rejected override"}}, StructuredContent: json.RawMessage(`{"replacement":true}`), Details: "rejected details", IsError: new(false), Terminate: new(true), Usage: &ai.Usage{Input: 999}}, failure
							}
							return AfterToolCallResult{}, failure
						}}
						call := ai.ToolCall{ID: "target-call", Name: "target", Arguments: json.RawMessage(`{}`)}
						var result *ToolResult
						var order []string
						emit := func(event Event) {
							switch e := event.(type) {
							case ToolStart:
								order = append(order, "start:"+e.ToolCallID)
							case ToolEnd:
								order = append(order, "end:"+e.ToolCallID)
								assert.Equal(t, new(2.0), e.DurationMs)
							case MessageEnd:
								order = append(order, "message:"+e.Message.ToolCallID)
							}
						}
						switch mode {
						case "sequential":
							result = executeSequential(ctx, current, nil, []ai.ToolCall{call}, config, emit)[0]
						case "parallel":
							result = executeParallel(ctx, current, nil, []ai.ToolCall{call}, config, emit)[0]
						case "direct":
							result = RunToolCall(ctx, current, nil, call, config)
						case "nested":
							caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"target", "leaf"} }, Execute: func(tc ToolContext) (*ToolResult, error) {
								result = tc.ExecuteTool(call)
								return &ToolResult{}, nil
							}}
							current.Tools = append(current.Tools, caller)
							outer := RunToolCall(ctx, current, nil, ai.ToolCall{ID: "outer", Name: "caller", Arguments: json.RawMessage(`{}`)}, config)
							require.Len(t, outer.NestedCalls, 1)
							status := "error"
							if outcome != "ordinary" {
								status = "cancelled"
							}
							assert.Equal(t, status, outer.NestedCalls[0].Status)
							assert.Equal(t, new(2.0), outer.NestedCalls[0].DurationMs)
							assert.Equal(t, &ai.Usage{Input: 5}, outer.Usage)
						}
						assert.Equal(t, 1, executions)
						assert.Equal(t, 1, hooks)
						assert.ErrorIs(t, result.Rejection, failure)
						if outcome != "ordinary" {
							assert.ErrorIs(t, result.Rejection, ctx.Err())
							assert.ErrorIs(t, result.Rejection, cause)
						}
						assert.Equal(t, failure.Error(), result.Content)
						assert.True(t, result.IsError)
						assert.False(t, result.Terminate)
						assert.Nil(t, result.ContentBlocks)
						assert.Nil(t, result.StructuredContent)
						assert.Nil(t, result.Details)
						assert.Equal(t, &ai.Usage{Input: 5}, result.Usage)
						require.Len(t, result.NestedCalls, 1)
						assert.Equal(t, "ok", result.NestedCalls[0].Status)
						assert.Equal(t, "leaf-call", result.NestedCalls[0].ID)
						assert.Equal(t, new(2.0), result.DurationMs)
						if mode == "sequential" || mode == "parallel" {
							assert.Equal(t, []string{"start:target-call", "end:target-call", "message:target-call"}, order)
							require.NotNil(t, result.Message)
							assert.Equal(t, result.Usage, result.Message.ToolUsage)
							assert.Equal(t, result.NestedCalls, result.Message.NestedCalls)
						}
					})
				})
			}
		}
	}
}

func TestAfterToolCancellationWithoutErrorKeepsResult(t *testing.T) {
	for _, outcome := range []string{"cancel", "deadline"} {
		for _, returned := range []string{"empty", "override"} {
			t.Run(outcome+"/"+returned, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancelCause(t.Context())
					defer cancel(nil)
					cause := errors.New("completed after hook withdrawn")
					if outcome == "deadline" {
						var stop context.CancelFunc
						ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
						defer stop()
					}
					tool := Tool{Name: "completed", Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) {
						time.Sleep(time.Millisecond)
						return &ToolResult{Content: "executed error value", IsError: true, Usage: &ai.Usage{Input: 2}}, nil
					}}
					result := RunToolCall(ctx, &AgentContext{Tools: []Tool{tool}}, nil, ai.ToolCall{Name: tool.Name, Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{AfterToolCall: func(context.Context, ToolCallContext, *ToolResult) (AfterToolCallResult, error) {
						if outcome == "deadline" {
							<-ctx.Done()
						} else {
							cancel(cause)
						}
						synctest.Wait()
						if returned == "override" {
							return AfterToolCallResult{Content: "accepted", Usage: &ai.Usage{Input: 7}, IsError: new(false), Terminate: new(true)}, nil
						}
						return AfterToolCallResult{}, nil
					}})
					assert.NoError(t, result.Rejection)
					assert.Equal(t, new(1.0), result.DurationMs)
					if returned == "override" {
						assert.Equal(t, "accepted", result.Content)
						assert.Equal(t, &ai.Usage{Input: 7}, result.Usage)
						assert.False(t, result.IsError)
						assert.True(t, result.Terminate)
					} else {
						assert.Equal(t, "executed error value", result.Content)
						assert.Equal(t, &ai.Usage{Input: 2}, result.Usage)
						assert.True(t, result.IsError)
						assert.False(t, result.Terminate)
					}
				})
			})
		}
	}
}

func TestExecutionFailureRetainsCancellationAndAccounting(t *testing.T) {
	for _, mode := range []string{"sequential", "parallel", "direct", "nested"} {
		for _, outcome := range []string{"ordinary", "cancel", "deadline"} {
			for _, returned := range []string{"nil", "accounted"} {
				t.Run(mode+"/"+outcome+"/"+returned, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						ctx, cancel := context.WithCancelCause(t.Context())
						defer cancel(nil)
						cause := errors.New("execution withdrawn")
						failure := errors.New("execution failed")
						if outcome == "deadline" {
							var stop context.CancelFunc
							ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
							defer stop()
						}
						usage := &ai.Usage{Input: 3}
						if returned == "accounted" {
							usage.Input += 2
						}
						check := func(result *ToolResult) {
							t.Helper()
							assert.ErrorIs(t, result.Rejection, failure)
							if outcome != "ordinary" {
								assert.ErrorIs(t, result.Rejection, ctx.Err())
								assert.ErrorIs(t, result.Rejection, cause)
							}
							assert.Equal(t, failure.Error(), result.Content)
							assert.True(t, result.IsError)
							assert.False(t, result.Terminate)
							assert.Nil(t, result.ContentBlocks)
							assert.Nil(t, result.StructuredContent)
							assert.Nil(t, result.Details)
							assert.Equal(t, usage, result.Usage)
							require.Len(t, result.NestedCalls, 1)
							assert.Equal(t, "ok", result.NestedCalls[0].Status)
							assert.Equal(t, "leaf-call", result.NestedCalls[0].ID)
						}
						var executions, hooks int
						leaf := Tool{Name: "leaf", Exposure: ToolDeferred, Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { return &ToolResult{Usage: &ai.Usage{Input: 3}}, nil }}
						target := Tool{Name: "target", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"leaf"} }, Execute: func(tc ToolContext) (*ToolResult, error) {
							executions++
							child := tc.ExecuteTool(ai.ToolCall{ID: "leaf-call", Name: "leaf", Arguments: json.RawMessage(`{}`)})
							require.NoError(t, child.Rejection)
							time.Sleep(2 * time.Millisecond)
							if outcome == "cancel" {
								cancel(cause)
							} else if outcome == "deadline" {
								<-ctx.Done()
							}
							synctest.Wait()
							if returned == "nil" {
								return nil, failure
							}
							return &ToolResult{Content: "stale", ContentBlocks: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "stale"}}, StructuredContent: json.RawMessage(`{"stale":true}`), Details: "stale", Terminate: true, Usage: &ai.Usage{Input: 2}}, failure
						}}
						current := &AgentContext{Tools: []Tool{target, leaf}}
						config := AgentLoopConfig{AfterToolCall: func(_ context.Context, call ToolCallContext, result *ToolResult) (AfterToolCallResult, error) {
							if call.ToolName == "target" {
								hooks++
								check(result)
								time.Sleep(5 * time.Second)
							}
							return AfterToolCallResult{}, nil
						}}
						call := ai.ToolCall{ID: "target-call", Name: "target", Arguments: json.RawMessage(`{}`)}
						var result *ToolResult
						var order []string
						emit := func(event Event) {
							switch e := event.(type) {
							case ToolStart:
								order = append(order, "start:"+e.ToolCallID)
							case ToolEnd:
								order = append(order, "end:"+e.ToolCallID)
							case MessageEnd:
								order = append(order, "message:"+e.Message.ToolCallID)
							}
						}
						switch mode {
						case "sequential":
							result = executeSequential(ctx, current, nil, []ai.ToolCall{call}, config, emit)[0]
						case "parallel":
							result = executeParallel(ctx, current, nil, []ai.ToolCall{call}, config, emit)[0]
						case "direct":
							result = RunToolCall(ctx, current, nil, call, config)
						case "nested":
							caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"target", "leaf"} }, Execute: func(tc ToolContext) (*ToolResult, error) {
								result = tc.ExecuteTool(call)
								return &ToolResult{}, nil
							}}
							current.Tools = append(current.Tools, caller)
							outer := RunToolCall(ctx, current, nil, ai.ToolCall{ID: "outer", Name: "caller", Arguments: json.RawMessage(`{}`)}, config)
							require.Len(t, outer.NestedCalls, 1)
							status := "error"
							if outcome != "ordinary" {
								status = "cancelled"
							}
							assert.Equal(t, status, outer.NestedCalls[0].Status)
							assert.Equal(t, result.DurationMs, outer.NestedCalls[0].DurationMs)
							assert.Equal(t, usage, outer.Usage)
						}
						check(result)
						assert.Equal(t, 1, executions)
						assert.Equal(t, 1, hooks)
						duration := 2.0
						if outcome == "deadline" {
							duration = 1000
						}
						assert.Equal(t, new(duration), result.DurationMs)
						if mode == "sequential" || mode == "parallel" {
							assert.Equal(t, []string{"start:target-call", "end:target-call", "message:target-call"}, order)
							require.NotNil(t, result.Message)
							assert.Equal(t, result.DurationMs, result.Message.DurationMs)
							assert.Equal(t, usage, result.Message.ToolUsage)
							assert.Equal(t, result.NestedCalls, result.Message.NestedCalls)
						}
					})
				})
			}
		}
	}
}

func TestExecutionCancellationWithoutErrorKeepsResult(t *testing.T) {
	for _, outcome := range []string{"cancel", "deadline"} {
		for _, returned := range []string{"nil", "success", "error value"} {
			t.Run(outcome+"/"+returned, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancelCause(t.Context())
					defer cancel(nil)
					cause := errors.New("completed execution withdrawn")
					if outcome == "deadline" {
						var stop context.CancelFunc
						ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
						defer stop()
					}
					var hooks int
					tool := Tool{Name: "completed", Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) {
						time.Sleep(time.Millisecond)
						if outcome == "deadline" {
							<-ctx.Done()
						} else {
							cancel(cause)
						}
						synctest.Wait()
						if returned == "nil" {
							return nil, nil
						}
						return &ToolResult{Content: "completed", StructuredContent: json.RawMessage(`null`), Details: "retained", IsError: returned == "error value", Usage: &ai.Usage{Input: 2}}, nil
					}}
					result := RunToolCall(ctx, &AgentContext{Tools: []Tool{tool}}, nil, ai.ToolCall{Name: tool.Name, Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{AfterToolCall: func(context.Context, ToolCallContext, *ToolResult) (AfterToolCallResult, error) {
						hooks++
						return AfterToolCallResult{}, nil
					}})
					assert.Equal(t, 1, hooks)
					assert.NoError(t, result.Rejection)
					assert.Equal(t, returned == "error value", result.IsError)
					duration := 1.0
					if outcome == "deadline" {
						duration = 1000
					}
					assert.Equal(t, new(duration), result.DurationMs)
					if returned == "nil" {
						assert.Empty(t, result.Content)
						assert.Nil(t, result.Usage)
					} else {
						assert.Equal(t, "completed", result.Content)
						assert.Equal(t, json.RawMessage(`null`), result.StructuredContent)
						assert.Equal(t, "retained", result.Details)
						assert.Equal(t, &ai.Usage{Input: 2}, result.Usage)
					}
				})
			})
		}
	}
}

func TestOutputSchemaRejectionDiscardsFinalRichResult(t *testing.T) {
	for _, mode := range []string{"sequential", "parallel", "direct", "nested"} {
		for _, invalid := range []string{"mismatch", "malformed JSON", "invalid schema"} {
			t.Run(mode+"/"+invalid, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var hooks int
					leaf := Tool{Name: "leaf", Exposure: ToolDeferred, Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) {
						return &ToolResult{Usage: &ai.Usage{Input: 3}}, nil
					}}
					target := Tool{Name: "target", Parameters: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "array"}, ReachesTools: func(map[string]any) []string { return []string{"leaf"} }, Execute: func(tc ToolContext) (*ToolResult, error) {
						child := tc.ExecuteTool(ai.ToolCall{ID: "leaf-call", Name: "leaf", Arguments: json.RawMessage(`{}`)})
						require.NoError(t, child.Rejection)
						time.Sleep(2 * time.Millisecond)
						return &ToolResult{Content: "executed", StructuredContent: json.RawMessage(`[1]`), Usage: &ai.Usage{Input: 2}}, nil
					}}
					if invalid == "invalid schema" {
						target.OutputSchema = map[string]any{"type": "not-a-type"}
					}
					config := AgentLoopConfig{AfterToolCall: func(_ context.Context, call ToolCallContext, result *ToolResult) (AfterToolCallResult, error) {
						if call.ToolName != "target" {
							return AfterToolCallResult{}, nil
						}
						hooks++
						assert.NoError(t, result.Rejection)
						assert.Equal(t, json.RawMessage(`[1]`), result.StructuredContent)
						assert.Equal(t, &ai.Usage{Input: 5}, result.Usage)
						time.Sleep(5 * time.Second)
						output := json.RawMessage(`42`)
						if invalid == "malformed JSON" {
							output = json.RawMessage(`{`)
						}
						return AfterToolCallResult{ContentBlocks: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "claimed success"}}, StructuredContent: output, Details: "stale details", Terminate: new(true), Usage: &ai.Usage{Input: 7}}, nil
					}}
					current := &AgentContext{Tools: []Tool{target, leaf}}
					call := ai.ToolCall{ID: "target-call", Name: "target", Arguments: json.RawMessage(`{}`)}
					var result *ToolResult
					var order []string
					emit := func(event Event) {
						switch e := event.(type) {
						case ToolStart:
							order = append(order, "start:"+e.ToolCallID)
						case ToolEnd:
							order = append(order, "end:"+e.ToolCallID)
						case MessageEnd:
							order = append(order, "message:"+e.Message.ToolCallID)
						}
					}
					switch mode {
					case "sequential":
						result = executeSequential(t.Context(), current, nil, []ai.ToolCall{call}, config, emit)[0]
					case "parallel":
						result = executeParallel(t.Context(), current, nil, []ai.ToolCall{call}, config, emit)[0]
					case "direct":
						result = RunToolCall(t.Context(), current, nil, call, config)
					case "nested":
						caller := Tool{Name: "caller", Parameters: map[string]any{"type": "object"}, ReachesTools: func(map[string]any) []string { return []string{"target", "leaf"} }, Execute: func(tc ToolContext) (*ToolResult, error) {
							result = tc.ExecuteTool(call)
							return &ToolResult{}, nil
						}}
						current.Tools = append(current.Tools, caller)
						outer := RunToolCall(t.Context(), current, nil, ai.ToolCall{Name: "caller", Arguments: json.RawMessage(`{}`)}, config)
						require.Len(t, outer.NestedCalls, 1)
						assert.Equal(t, "error", outer.NestedCalls[0].Status)
						assert.Equal(t, result.DurationMs, outer.NestedCalls[0].DurationMs)
						assert.Equal(t, &ai.Usage{Input: 7}, outer.Usage)
					}
					assert.Equal(t, 1, hooks)
					require.Error(t, result.Rejection)
					assert.Equal(t, result.Rejection.Error(), result.Content)
					assert.True(t, result.IsError)
					assert.False(t, result.Terminate)
					assert.Nil(t, result.StructuredContent)
					assert.Nil(t, result.Details)
					assert.Nil(t, result.ContentBlocks)
					assert.Equal(t, []ai.ContentBlock{{Type: ai.ContentBlockText, Text: result.Content}}, result.NormalizedContent())
					assert.Equal(t, &ai.Usage{Input: 7}, result.Usage)
					require.Len(t, result.NestedCalls, 1)
					assert.Equal(t, "leaf-call", result.NestedCalls[0].ID)
					assert.Equal(t, "ok", result.NestedCalls[0].Status)
					assert.Equal(t, new(2.0), result.DurationMs)
					if mode == "sequential" || mode == "parallel" {
						assert.Equal(t, []string{"start:target-call", "end:target-call", "message:target-call"}, order)
						require.NotNil(t, result.Message)
						assert.Nil(t, result.Message.StructuredContent)
						assert.Nil(t, result.Message.Details)
						assert.Equal(t, result.NormalizedContent(), result.Message.Content)
						assert.Equal(t, result.DurationMs, result.Message.DurationMs)
						assert.Equal(t, result.Usage, result.Message.ToolUsage)
					}
				})
			})
		}
	}
}

func TestOutputSchemaKeepsExplicitErrorsAndUnconstrainedJSON(t *testing.T) {
	for _, value := range []string{`null`, `42`, `"text"`, `true`, `[]`, `{}`} {
		for _, outcome := range []string{"success without schema", "explicit error"} {
			t.Run(outcome+"/"+value, func(t *testing.T) {
				want := &ToolResult{Content: "retained", StructuredContent: json.RawMessage(value), Details: "retained details", Terminate: true, Usage: &ai.Usage{Input: 2}, IsError: outcome == "explicit error"}
				tool := Tool{Name: "target", Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { return want, nil }}
				if want.IsError {
					tool.OutputSchema = map[string]any{"type": "not-a-type"}
				}
				result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{tool}}, nil, ai.ToolCall{Name: tool.Name, Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
				assert.Same(t, want, result)
				assert.NoError(t, result.Rejection)
				assert.Equal(t, json.RawMessage(value), result.StructuredContent)
				assert.Equal(t, "retained details", result.Details)
				assert.True(t, result.Terminate)
				assert.Equal(t, outcome == "explicit error", result.IsError)
				assert.Equal(t, &ai.Usage{Input: 2}, result.Usage)
				require.NotNil(t, result.DurationMs)
			})
		}
	}
}
