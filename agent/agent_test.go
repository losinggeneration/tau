package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/losinggeneration/tau/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateLoopConfigCarriesRuntimeToolsForClaudeAgent(t *testing.T) {
	tool := Tool{Name: "echo", Parameters: map[string]any{"type": "object"}}
	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "sonnet", API: ai.APIClaudeAgent, Provider: ai.ProviderClaudeAgent}),
		WithTools(tool),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cfg := agent.createLoopConfig(false, nil, "")
	if len(cfg.Tools) != 1 || cfg.Tools[0].Name != "echo" {
		t.Fatalf("cfg.Tools = %#v, want echo tool", cfg.Tools)
	}
}

func TestContinueRejectsEmptyContextAndAssistantLastWithoutQueuedMessages(t *testing.T) {
	a, err := New(NewConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Continue(context.Background()); !errors.Is(err, ErrNoMessagesToContinue) {
		t.Fatalf("Continue empty err = %v, want %v", err, ErrNoMessagesToContinue)
	}

	a.ReplaceMessages([]*Message{{Role: RoleAssistant, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}, Timestamp: time.Now()}})
	if err := a.Continue(context.Background()); !errors.Is(err, ErrCannotContinueAssistant) {
		t.Fatalf("Continue assistant-last err = %v, want %v", err, ErrCannotContinueAssistant)
	}
}

func TestWaitForIdleWaitsForAgentEndListenerSettlement(t *testing.T) {
	client := stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
		final := ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}}, StopReason: ai.StopReasonStop, Timestamp: time.Now().UnixMilli()}
		handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: final.Timestamp}})
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
		return nil
	}}
	a, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) { return client, nil }),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	listenerDone := make(chan struct{})
	a.Subscribe(func(event Event, signal <-chan struct{}) {
		if _, ok := event.(AgentEnd); ok {
			time.Sleep(25 * time.Millisecond)
			close(listenerDone)
		}
	})

	runDone := make(chan error, 1)
	go func() { runDone <- a.Prompt(context.Background(), NewUserMessage("hi")) }()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt did not complete")
	}

	a.WaitForIdle()
	select {
	case <-listenerDone:
	default:
		t.Fatal("WaitForIdle returned before AgentEnd listener settled")
	}
}

func TestContinueConsumesSteeringBeforeFollowUp(t *testing.T) {
	var promptTexts []string
	client := stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
		if len(prompt.Messages) > 0 {
			if blocks := prompt.Messages[len(prompt.Messages)-1].ContentBlocks(); len(blocks) > 0 {
				promptTexts = append(promptTexts, blocks[0].Text)
			}
		}
		final := ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}}, StopReason: ai.StopReasonStop, Timestamp: time.Now().UnixMilli()}
		handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: final.Timestamp}})
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
		return nil
	}}
	a, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) { return client, nil }),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.ReplaceMessages([]*Message{{Role: RoleAssistant, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "prior"}}, Timestamp: time.Now()}})
	a.Steer(NewUserMessage("steer first"))
	a.FollowUp(NewUserMessage("follow later"))

	if err := a.Continue(context.Background()); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if len(promptTexts) != 2 || promptTexts[0] != "steer first" || promptTexts[1] != "follow later" {
		t.Fatalf("promptTexts = %v, want [steer first follow later]", promptTexts)
	}
	if a.HasQueuedMessages() {
		t.Fatal("queues should be drained after steering runs before follow-up")
	}
}

func TestAbortCancelsProviderContextAndEmitsFinalAgentEnd(t *testing.T) {
	streamEntered := make(chan struct{})
	ctxCancelled := make(chan struct{})
	client := stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
		close(streamEntered)
		<-ctx.Done()
		close(ctxCancelled)
		return ctx.Err()
	}}
	a, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) { return client, nil }),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var events []string
	a.Subscribe(func(event Event, signal <-chan struct{}) { events = append(events, eventLabel(event)) })

	done := make(chan error, 1)
	go func() { done <- a.Prompt(context.Background(), NewUserMessage("hi")) }()
	select {
	case <-streamEntered:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	a.Abort()
	select {
	case <-ctxCancelled:
	case <-time.After(time.Second):
		t.Fatal("provider context was not cancelled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("prompt did not finish after abort")
	}
	if len(events) == 0 || events[len(events)-1] != "agent_end" {
		t.Fatalf("events = %v, want final agent_end", events)
	}
}

func TestDefaultConvertToLlmMapsToolRoleToToolResult(t *testing.T) {
	now := time.Now()

	messages := []*Message{
		{
			Role:       RoleTool,
			ToolCallID: "call_1",
			ToolName:   "read",
			Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
			Timestamp:  now,
		},
	}

	converted, err := defaultConvertToLlm(context.Background(), messages)
	if err != nil {
		t.Fatalf("defaultConvertToLlm: %v", err)
	}
	if len(converted) != 1 {
		t.Fatalf("converted len = %d, want 1", len(converted))
	}

	if converted[0].Role != "toolResult" {
		t.Fatalf("role = %q, want toolResult", converted[0].Role)
	}

	if converted[0].Timestamp != now.UnixMilli() {
		t.Fatalf("timestamp = %d, want %d", converted[0].Timestamp, now.UnixMilli())
	}
}

func TestMessageToAIMapsToolRoleToToolResult(t *testing.T) {
	now := time.Now()
	msg := &Message{
		Role:       RoleTool,
		ToolCallID: "call_1",
		ToolName:   "bash",
		Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
		Timestamp:  now,
	}

	converted := messageToAI(msg)
	if converted.Role != "toolResult" {
		t.Fatalf("role = %q, want toolResult", converted.Role)
	}

	if converted.Timestamp != now.UnixMilli() {
		t.Fatalf("timestamp = %d, want %d", converted.Timestamp, now.UnixMilli())
	}
}

type errorClient struct {
	err error
}

func (e errorClient) Stream(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
	return e.err
}

func (e errorClient) Complete(ctx context.Context, model *ai.Model, prompt *ai.Context, opts ...ai.Option) (*ai.AssistantMessage, error) {
	return nil, e.err
}

func TestPromptAppendsFailureAssistantMessageOnRunError(t *testing.T) {
	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return errorClient{err: context.Canceled}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = agent.Prompt(context.Background(), NewUserMessage("hi"))
	if err == nil {
		t.Fatal("expected prompt error")
	}

	msgs := agent.Messages()
	if len(msgs) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(msgs))
	}
	if msgs[1].Role != RoleAssistant {
		t.Fatalf("last role = %q, want assistant", msgs[1].Role)
	}
	if !msgs[1].IsError {
		t.Fatal("expected synthesized failure assistant message to be marked as error")
	}
	if msgs[1].StopReason != ai.StopReasonAborted {
		t.Fatalf("stop reason = %q, want %q", msgs[1].StopReason, ai.StopReasonAborted)
	}
	if msgs[1].ErrorMessage != context.Canceled.Error() {
		t.Fatalf("error message = %q, want %q", msgs[1].ErrorMessage, context.Canceled.Error())
	}
	if got := msgs[1].Content[0].Text; got != "" {
		t.Fatalf("content = %q, want empty failure text block", got)
	}

	state := agent.State()
	if !errors.Is(state.Error, context.Canceled) {
		t.Fatalf("state.Error = %v, want %v", state.Error, context.Canceled)
	}
	if state.ErrorMessage != context.Canceled.Error() {
		t.Fatalf("state.ErrorMessage = %q, want %q", state.ErrorMessage, context.Canceled.Error())
	}
	if state.IsStreaming {
		t.Fatal("state.IsStreaming = true, want false after failed prompt completes")
	}
	if state.StreamingMessage != nil {
		t.Fatalf("state.StreamingMessage = %#v, want nil after failed prompt completes", state.StreamingMessage)
	}
	if len(state.PendingToolIDs) != 0 {
		t.Fatalf("len(state.PendingToolIDs) = %d, want 0 after failed prompt completes", len(state.PendingToolIDs))
	}
}

func TestPromptUpdatesStreamingStateDuringAssistantStream(t *testing.T) {
	release := make(chan struct{})

	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					partial := ai.AssistantMessage{
						Role:      "assistant",
						Timestamp: time.Now().UnixMilli(),
						Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "hello"}},
					}
					handler(ctx, ai.Start{Partial: partial})
					handler(ctx, ai.TextDelta{ContentIndex: 0, Delta: "hello", Partial: partial})

					<-release

					final := partial
					final.StopReason = ai.StopReasonStop
					handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
					return nil
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	updateCh := make(chan State, 1)
	messageEndCh := make(chan State, 1)
	agentEndCh := make(chan State, 1)
	agent.Subscribe(func(event Event, signal <-chan struct{}) {
		switch event.(type) {
		case MessageUpdate:
			select {
			case updateCh <- agent.State():
			default:
			}
		case MessageEnd:
			state := agent.State()
			if len(state.Messages) > 0 && state.Messages[len(state.Messages)-1].Role == RoleAssistant {
				select {
				case messageEndCh <- state:
				default:
				}
			}
		case AgentEnd:
			select {
			case agentEndCh <- agent.State():
			default:
			}
		}
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- agent.Prompt(context.Background(), NewUserMessage("hi"))
	}()

	updateState := <-updateCh
	if !updateState.IsStreaming {
		t.Fatal("state.IsStreaming = false, want true during message update")
	}
	if len(updateState.Messages) != 1 {
		t.Fatalf("len(state.Messages) = %d, want 1 before assistant message completes", len(updateState.Messages))
	}
	if updateState.StreamingMessage == nil {
		t.Fatal("state.StreamingMessage = nil, want partial assistant message during stream")
	}
	if got := updateState.StreamingMessage.Content[0].Text; got != "hello" {
		t.Fatalf("streaming content = %q, want hello", got)
	}

	close(release)

	if err := <-errCh; err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	messageEndState := <-messageEndCh
	if !messageEndState.IsStreaming {
		t.Fatal("state.IsStreaming = false, want true while message_end listeners run")
	}
	if messageEndState.StreamingMessage != nil {
		t.Fatalf("state.StreamingMessage = %#v, want nil after assistant message_end", messageEndState.StreamingMessage)
	}
	if len(messageEndState.Messages) != 2 {
		t.Fatalf("len(state.Messages) = %d, want 2 after assistant message_end", len(messageEndState.Messages))
	}

	agentEndState := <-agentEndCh
	if !agentEndState.IsStreaming {
		t.Fatal("state.IsStreaming = false, want true while agent_end listeners run")
	}

	finalState := agent.State()
	if finalState.IsStreaming {
		t.Fatal("state.IsStreaming = true, want false after prompt completes")
	}
	if finalState.StreamingMessage != nil {
		t.Fatalf("state.StreamingMessage = %#v, want nil after prompt completes", finalState.StreamingMessage)
	}
	if len(finalState.Messages) != 2 {
		t.Fatalf("len(state.Messages) = %d, want 2 after prompt completes", len(finalState.Messages))
	}
}

func TestPromptPendingToolIDsTrackCurrentlyExecutingParallelTools(t *testing.T) {
	fastCall, err := CreateToolCall("fast", map[string]any{})
	if err != nil {
		t.Fatalf("CreateToolCall(fast): %v", err)
	}
	slowCall, err := CreateToolCall("slow", map[string]any{})
	if err != nil {
		t.Fatalf("CreateToolCall(slow): %v", err)
	}

	fastDone := make(chan struct{})
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})

	var streamCalls int
	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithToolExecution(ToolExecutionParallel),
		WithTools(
			Tool{
				Name:       "fast",
				Parameters: map[string]any{"type": "object"},
				Execute: func(ctx ToolContext) (*ToolResult, error) {
					close(fastDone)
					return &ToolResult{Content: "fast", Details: map[string]any{}}, nil
				},
			},
			Tool{
				Name:       "slow",
				Parameters: map[string]any{"type": "object"},
				Execute: func(ctx ToolContext) (*ToolResult, error) {
					close(slowStarted)
					<-releaseSlow
					return &ToolResult{Content: "slow", Details: map[string]any{}}, nil
				},
			},
		),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					streamCalls++

					switch streamCalls {
					case 1:
						partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
						handler(ctx, ai.Start{Partial: partial})

						partial.Content = []ai.ContentBlock{
							{Type: ai.ContentBlockToolCall, ID: fastCall.ID, Name: fastCall.Name, Arguments: fastCall.Arguments},
							{Type: ai.ContentBlockToolCall, ID: slowCall.ID, Name: slowCall.Name, Arguments: slowCall.Arguments},
						}
						handler(ctx, ai.ToolCallStart{ContentIndex: 0, Partial: partial})
						handler(ctx, ai.ToolCallStart{ContentIndex: 1, Partial: partial})

						final := partial
						final.StopReason = ai.StopReasonToolUse
						handler(ctx, ai.Done{Reason: ai.StopReasonToolUse, Message: final})
						return nil
					case 2:
						partial := ai.AssistantMessage{
							Role:      "assistant",
							Timestamp: time.Now().UnixMilli(),
							Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
						}
						handler(ctx, ai.Start{Partial: partial})

						final := partial
						final.StopReason = ai.StopReasonStop
						handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
						return nil
					default:
						t.Fatalf("unexpected extra Stream call %d", streamCalls)
						return nil
					}
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- agent.Prompt(context.Background(), NewUserMessage("hi"))
	}()

	<-slowStarted
	<-fastDone

	deadline := time.Now().Add(time.Second)
	for {
		state := agent.State()
		if len(state.PendingToolIDs) == 1 {
			if _, ok := state.PendingToolIDs[slowCall.ID]; !ok {
				t.Fatalf("pending tool IDs missing slow call %q", slowCall.ID)
			}
			if _, ok := state.PendingToolIDs[fastCall.ID]; ok {
				t.Fatalf("pending tool IDs unexpectedly still contains completed fast call %q", fastCall.ID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("len(state.PendingToolIDs) = %d, want 1 while only slow tool is still running", len(state.PendingToolIDs))
		}
		time.Sleep(time.Millisecond)
	}

	close(releaseSlow)

	if err := <-errCh; err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	finalState := agent.State()
	if len(finalState.PendingToolIDs) != 0 {
		t.Fatalf("len(finalState.PendingToolIDs) = %d, want 0 after prompt completes", len(finalState.PendingToolIDs))
	}
}

func TestPromptPrepareArgumentsFeedsHooksEventsAndExecution(t *testing.T) {
	toolCall, err := CreateToolCall("read", map[string]any{})
	if err != nil {
		t.Fatalf("CreateToolCall(read): %v", err)
	}

	var (
		beforeArgs    map[string]any
		executedArgs  map[string]any
		toolStartArgs map[string]any
		streamCalls   int
	)

	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithTools(Tool{
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
				args["path"] = "/tmp/from-prepare.txt"
				return args
			},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				executedArgs = maps.Clone(ctx.Args)
				return &ToolResult{Content: ctx.Args["path"].(string), Details: map[string]any{}}, nil
			},
		}),
		WithBeforeToolCall(func(ctx context.Context, toolCtx ToolCallContext) (BeforeToolCallResult, error) {
			beforeArgs = maps.Clone(toolCtx.Args)
			return BeforeToolCallResult{}, nil
		}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					streamCalls++

					switch streamCalls {
					case 1:
						partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
						handler(ctx, ai.Start{Partial: partial})

						partial.Content = []ai.ContentBlock{
							{Type: ai.ContentBlockToolCall, ID: toolCall.ID, Name: toolCall.Name, Arguments: toolCall.Arguments},
						}
						handler(ctx, ai.ToolCallStart{ContentIndex: 0, Partial: partial})

						final := partial
						final.StopReason = ai.StopReasonToolUse
						handler(ctx, ai.Done{Reason: ai.StopReasonToolUse, Message: final})
						return nil
					case 2:
						partial := ai.AssistantMessage{
							Role:      "assistant",
							Timestamp: time.Now().UnixMilli(),
							Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
						}
						handler(ctx, ai.Start{Partial: partial})

						final := partial
						final.StopReason = ai.StopReasonStop
						handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
						return nil
					default:
						t.Fatalf("unexpected extra Stream call %d", streamCalls)
						return nil
					}
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	agent.Subscribe(func(event Event, signal <-chan struct{}) {
		start, ok := event.(ToolStart)
		if !ok {
			return
		}
		toolStartArgs = maps.Clone(start.Args)
	})

	if err := agent.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if got := beforeArgs["path"]; got != "/tmp/from-prepare.txt" {
		t.Fatalf("before hook path = %v, want /tmp/from-prepare.txt", got)
	}
	// ToolStart fires BEFORE prepareToolCall, with raw (unvalidated) arguments.
	// This matches the TypeScript reference behavior: tool_execution_start carries
	// the parsed but un-`prepareArguments`'d args. The test tool call had empty args `{}`,
	// so toolStartArgs should reflect that pre-prepare state (nil or empty path).
	if toolStartArgs != nil {
		if _, hasPath := toolStartArgs["path"]; hasPath {
			t.Fatal("ToolStart args unexpectedly included prepared path")
		}
	}
	if got := executedArgs["path"]; got != "/tmp/from-prepare.txt" {
		t.Fatalf("executed path = %v, want /tmp/from-prepare.txt", got)
	}

	msgs := agent.Messages()
	if msgs[0].Role != RoleSystem {
		t.Fatal("missing canonical tool baseline")
	}
	msgs = msgs[1:]
	if len(msgs) != 4 {
		t.Fatalf("len(messages) = %d, want 4", len(msgs))
	}
	if msgs[2].Role != RoleTool {
		t.Fatalf("tool result role = %q, want %q", msgs[2].Role, RoleTool)
	}
	if got := msgs[2].Content[0].Text; got != "/tmp/from-prepare.txt" {
		t.Fatalf("tool result content = %q, want /tmp/from-prepare.txt", got)
	}
}

func TestPromptStructuredToolUpdatePreservesContentBlocksInStateAndEvent(t *testing.T) {
	toolCall, err := CreateToolCall("inspect", map[string]any{})
	if err != nil {
		t.Fatalf("CreateToolCall(inspect): %v", err)
	}

	partialBlocks := []ai.ContentBlock{
		{Type: ai.ContentBlockText, Text: "preview"},
		{Type: ai.ContentBlockImage, Data: "abc123", MimeType: "image/png"},
	}

	var streamCalls int
	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithTools(Tool{
			Name:       "inspect",
			Parameters: map[string]any{"type": "object"},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				ctx.OnUpdate(&ToolResult{
					ContentBlocks: partialBlocks,
					Details:       map[string]any{"step": 1},
				})
				return &ToolResult{
					Content: "final",
					Details: map[string]any{},
				}, nil
			},
		}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					streamCalls++

					switch streamCalls {
					case 1:
						partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
						handler(ctx, ai.Start{Partial: partial})

						partial.Content = []ai.ContentBlock{
							{Type: ai.ContentBlockToolCall, ID: toolCall.ID, Name: toolCall.Name, Arguments: toolCall.Arguments},
						}
						handler(ctx, ai.ToolCallStart{ContentIndex: 0, Partial: partial})

						final := partial
						final.StopReason = ai.StopReasonToolUse
						handler(ctx, ai.Done{Reason: ai.StopReasonToolUse, Message: final})
						return nil
					case 2:
						partial := ai.AssistantMessage{
							Role:      "assistant",
							Timestamp: time.Now().UnixMilli(),
							Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
						}
						handler(ctx, ai.Start{Partial: partial})

						final := partial
						final.StopReason = ai.StopReasonStop
						handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
						return nil
					default:
						t.Fatalf("unexpected extra Stream call %d", streamCalls)
						return nil
					}
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	updateCh := make(chan ToolUpdate, 1)
	stateCh := make(chan State, 1)
	agent.Subscribe(func(event Event, signal <-chan struct{}) {
		update, ok := event.(ToolUpdate)
		if !ok {
			return
		}
		select {
		case updateCh <- update:
		default:
		}
		select {
		case stateCh <- agent.State():
		default:
		}
	})

	if err := agent.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	update := <-updateCh
	if update.PartialResult == nil || update.PartialResult.Message == nil {
		t.Fatal("expected structured partial result message")
	}
	if len(update.PartialResult.Message.Content) != 2 {
		t.Fatalf("len(partial message content) = %d, want 2", len(update.PartialResult.Message.Content))
	}
	if got := update.PartialResult.Message.Content[0].Text; got != "preview" {
		t.Fatalf("partial text = %q, want preview", got)
	}
	if got := update.PartialResult.Message.Content[1].Type; got != ai.ContentBlockImage {
		t.Fatalf("partial content[1].Type = %q, want %q", got, ai.ContentBlockImage)
	}

	updateState := <-stateCh
	if updateState.StreamingMessage != nil {
		t.Fatalf("state.StreamingMessage = %#v, want nil during tool update parity behavior", updateState.StreamingMessage)
	}

	msgs := agent.Messages()
	if msgs[0].Role != RoleSystem {
		t.Fatal("missing canonical tool baseline")
	}
	msgs = msgs[1:]
	if len(msgs) != 4 {
		t.Fatalf("len(messages) = %d, want 4", len(msgs))
	}
	if got := msgs[2].Content[0].Text; got != "final" {
		t.Fatalf("final tool result content = %q, want final", got)
	}
}

func TestPromptBlockedToolCallProducesBlockedResultWithoutExecution(t *testing.T) {
	toolCall, err := CreateToolCall("blocked", map[string]any{})
	if err != nil {
		t.Fatalf("CreateToolCall(blocked): %v", err)
	}

	var (
		executed    bool
		streamCalls int
		toolStarts  []ToolStart
		toolEnds    []ToolEnd
	)

	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithTools(Tool{
			Name:       "blocked",
			Parameters: map[string]any{"type": "object"},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				executed = true
				return &ToolResult{Content: "unexpected", Details: map[string]any{}}, nil
			},
		}),
		WithBeforeToolCall(func(ctx context.Context, toolCtx ToolCallContext) (BeforeToolCallResult, error) {
			return BeforeToolCallResult{Block: true, Reason: "denied"}, nil
		}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					streamCalls++

					switch streamCalls {
					case 1:
						partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
						handler(ctx, ai.Start{Partial: partial})

						partial.Content = []ai.ContentBlock{
							{Type: ai.ContentBlockToolCall, ID: toolCall.ID, Name: toolCall.Name, Arguments: toolCall.Arguments},
						}
						handler(ctx, ai.ToolCallStart{ContentIndex: 0, Partial: partial})

						final := partial
						final.StopReason = ai.StopReasonToolUse
						handler(ctx, ai.Done{Reason: ai.StopReasonToolUse, Message: final})
						return nil
					case 2:
						partial := ai.AssistantMessage{
							Role:      "assistant",
							Timestamp: time.Now().UnixMilli(),
							Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
						}
						handler(ctx, ai.Start{Partial: partial})

						final := partial
						final.StopReason = ai.StopReasonStop
						handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
						return nil
					default:
						t.Fatalf("unexpected extra Stream call %d", streamCalls)
						return nil
					}
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	agent.Subscribe(func(event Event, signal <-chan struct{}) {
		switch e := event.(type) {
		case ToolStart:
			toolStarts = append(toolStarts, e)
		case ToolEnd:
			toolEnds = append(toolEnds, e)
		}
	})

	if err := agent.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if executed {
		t.Fatal("blocked tool was executed")
	}
	if len(toolStarts) != 1 {
		t.Fatalf("len(toolStarts) = %d, want 1", len(toolStarts))
	}
	if got := toolStarts[0].Name; got != "blocked" {
		t.Fatalf("tool start name = %q, want blocked", got)
	}
	if len(toolEnds) != 1 {
		t.Fatalf("len(toolEnds) = %d, want 1", len(toolEnds))
	}
	if !toolEnds[0].IsError {
		t.Fatal("blocked tool end should be marked as error")
	}
	if toolEnds[0].Result == nil || toolEnds[0].Result.Content != "denied" {
		t.Fatalf("blocked tool result = %#v, want denied", toolEnds[0].Result)
	}

	msgs := agent.Messages()
	if msgs[0].Role != RoleSystem {
		t.Fatal("missing canonical tool baseline")
	}
	msgs = msgs[1:]
	if len(msgs) != 4 {
		t.Fatalf("len(messages) = %d, want 4", len(msgs))
	}
	if msgs[2].Role != RoleTool {
		t.Fatalf("tool result role = %q, want %q", msgs[2].Role, RoleTool)
	}
	if !msgs[2].IsError {
		t.Fatal("blocked tool message should be marked as error")
	}
	if got := msgs[2].Content[0].Text; got != "denied" {
		t.Fatalf("blocked tool message content = %q, want denied", got)
	}

	state := agent.State()
	if state.StreamingMessage != nil {
		t.Fatal("state.StreamingMessage should be cleared after blocked tool turn completes")
	}
	if len(state.PendingToolIDs) != 0 {
		t.Fatalf("len(state.PendingToolIDs) = %d, want 0 after blocked tool completes", len(state.PendingToolIDs))
	}
}

func TestPromptAfterToolCallOverridesFinalToolMessage(t *testing.T) {
	toolCall, err := CreateToolCall("override", map[string]any{"path": "file.txt"})
	if err != nil {
		t.Fatalf("CreateToolCall(override): %v", err)
	}

	overrideError := true
	var (
		hookArgs    map[string]any
		hookContent string
		streamCalls int
		toolEnds    []ToolEnd
	)

	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithTools(Tool{
			Name:       "override",
			Parameters: map[string]any{"type": "object"},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				return &ToolResult{
					Content: "original",
					Details: map[string]any{"source": "tool"},
				}, nil
			},
		}),
		WithAfterToolCall(func(ctx context.Context, toolCtx ToolCallContext, result *ToolResult) (AfterToolCallResult, error) {
			hookArgs = maps.Clone(toolCtx.Args)
			hookContent = result.Content
			return AfterToolCallResult{
				ContentBlocks: []ai.ContentBlock{
					{Type: ai.ContentBlockText, Text: "override"},
					{Type: ai.ContentBlockImage, Data: "abc123", MimeType: "image/png"},
				},
				Details: map[string]any{"source": "after"},
				IsError: &overrideError,
			}, nil
		}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					streamCalls++

					switch streamCalls {
					case 1:
						partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
						handler(ctx, ai.Start{Partial: partial})

						partial.Content = []ai.ContentBlock{
							{Type: ai.ContentBlockToolCall, ID: toolCall.ID, Name: toolCall.Name, Arguments: toolCall.Arguments},
						}
						handler(ctx, ai.ToolCallStart{ContentIndex: 0, Partial: partial})

						final := partial
						final.StopReason = ai.StopReasonToolUse
						handler(ctx, ai.Done{Reason: ai.StopReasonToolUse, Message: final})
						return nil
					case 2:
						partial := ai.AssistantMessage{
							Role:      "assistant",
							Timestamp: time.Now().UnixMilli(),
							Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
						}
						handler(ctx, ai.Start{Partial: partial})

						final := partial
						final.StopReason = ai.StopReasonStop
						handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
						return nil
					default:
						t.Fatalf("unexpected extra Stream call %d", streamCalls)
						return nil
					}
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	agent.Subscribe(func(event Event, signal <-chan struct{}) {
		end, ok := event.(ToolEnd)
		if !ok {
			return
		}
		toolEnds = append(toolEnds, end)
	})

	if err := agent.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if got := hookArgs["path"]; got != "file.txt" {
		t.Fatalf("after hook args[path] = %v, want file.txt", got)
	}
	if hookContent != "original" {
		t.Fatalf("after hook saw content = %q, want original", hookContent)
	}
	if len(toolEnds) != 1 {
		t.Fatalf("len(toolEnds) = %d, want 1", len(toolEnds))
	}
	if toolEnds[0].Result == nil {
		t.Fatal("tool end result = nil, want overridden result")
	}
	if !toolEnds[0].IsError {
		t.Fatal("tool end should reflect after-hook error override")
	}
	if len(toolEnds[0].Result.ContentBlocks) != 2 {
		t.Fatalf("len(tool end content blocks) = %d, want 2", len(toolEnds[0].Result.ContentBlocks))
	}
	if got := toolEnds[0].Result.ContentBlocks[0].Text; got != "override" {
		t.Fatalf("tool end text = %q, want override", got)
	}
	details := detailsMap(t, toolEnds[0].Result.Details)
	if got := details["source"]; got != "after" {
		t.Fatalf("tool end detail[source] = %v, want after", got)
	}

	msgs := agent.Messages()
	if msgs[0].Role != RoleSystem {
		t.Fatal("missing canonical tool baseline")
	}
	msgs = msgs[1:]
	if len(msgs) != 4 {
		t.Fatalf("len(messages) = %d, want 4", len(msgs))
	}
	if got := len(msgs[2].Content); got != 2 {
		t.Fatalf("len(tool message content) = %d, want 2", got)
	}
	if got := msgs[2].Content[0].Text; got != "override" {
		t.Fatalf("tool message text = %q, want override", got)
	}
	if got := msgs[2].Content[1].Type; got != ai.ContentBlockImage {
		t.Fatalf("tool message content[1].Type = %q, want %q", got, ai.ContentBlockImage)
	}
	if !msgs[2].IsError {
		t.Fatal("tool message should reflect after-hook error override")
	}
}

func TestPromptInjectsQueuedSteeringBeforeFirstAssistantTurn(t *testing.T) {
	var prompts [][]ai.Message

	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					prompts = append(prompts, append([]ai.Message(nil), prompt.Messages...))

					partial := ai.AssistantMessage{
						Role:      "assistant",
						Timestamp: time.Now().UnixMilli(),
						Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
					}
					handler(ctx, ai.Start{Partial: partial})

					final := partial
					final.StopReason = ai.StopReasonStop
					handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
					return nil
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	agent.Steer(NewUserMessage("queued steering"))

	if err := agent.Prompt(context.Background(), NewUserMessage("primary prompt")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if len(prompts) != 1 {
		t.Fatalf("len(prompts) = %d, want 1 when queued steering is injected before the first turn", len(prompts))
	}
	if len(prompts[0]) != 2 {
		t.Fatalf("len(prompts[0]) = %d, want 2 first-turn prompt messages", len(prompts[0]))
	}
	content, ok := prompts[0][0].Content.([]ai.ContentBlock)
	if !ok {
		t.Fatalf("prompt content type = %T, want []ai.ContentBlock", prompts[0][0].Content)
	}
	if got := content[0].Text; got != "primary prompt" {
		t.Fatalf("first prompt content = %q, want %q", got, "primary prompt")
	}
	steeringContent, ok := prompts[0][1].Content.([]ai.ContentBlock)
	if !ok {
		t.Fatalf("steering content type = %T, want []ai.ContentBlock", prompts[0][1].Content)
	}
	if got := steeringContent[0].Text; got != "queued steering" {
		t.Fatalf("first-turn steering content = %q, want %q", got, "queued steering")
	}

	state := agent.State()
	if len(state.Messages) != 3 {
		t.Fatalf("len(state.Messages) = %d, want 3 after single-turn steering injection", len(state.Messages))
	}
	if agent.HasQueuedMessages() {
		t.Fatal("HasQueuedMessages = true, want steering queue drained during the first turn")
	}
}

func TestPromptRunsQueuedFollowUpsAfterAssistantStopsOneAtATime(t *testing.T) {
	var prompts [][]ai.Message

	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					prompts = append(prompts, append([]ai.Message(nil), prompt.Messages...))

					partial := ai.AssistantMessage{
						Role:      "assistant",
						Timestamp: time.Now().UnixMilli(),
						Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
					}
					handler(ctx, ai.Start{Partial: partial})

					final := partial
					final.StopReason = ai.StopReasonStop
					handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
					return nil
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	agent.FollowUp(NewUserMessage("follow-up one"))
	agent.FollowUp(NewUserMessage("follow-up two"))

	if err := agent.Prompt(context.Background(), NewUserMessage("primary prompt")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if len(prompts) != 3 {
		t.Fatalf("len(prompts) = %d, want 3 after queued follow-ups trigger two additional turns", len(prompts))
	}
	if len(prompts[0]) != 1 {
		t.Fatalf("len(prompts[0]) = %d, want 1 first-turn prompt message", len(prompts[0]))
	}
	firstContent, ok := prompts[0][0].Content.([]ai.ContentBlock)
	if !ok {
		t.Fatalf("first prompt content type = %T, want []ai.ContentBlock", prompts[0][0].Content)
	}
	if got := firstContent[0].Text; got != "primary prompt" {
		t.Fatalf("first prompt content = %q, want %q", got, "primary prompt")
	}
	if len(prompts[1]) != 3 {
		t.Fatalf("len(prompts[1]) = %d, want 3 before first follow-up turn", len(prompts[1]))
	}
	secondContent, ok := prompts[1][2].Content.([]ai.ContentBlock)
	if !ok {
		t.Fatalf("second-turn follow-up content type = %T, want []ai.ContentBlock", prompts[1][2].Content)
	}
	if got := secondContent[0].Text; got != "follow-up one" {
		t.Fatalf("second-turn follow-up content = %q, want %q", got, "follow-up one")
	}
	if len(prompts[2]) != 5 {
		t.Fatalf("len(prompts[2]) = %d, want 5 before second follow-up turn", len(prompts[2]))
	}
	thirdContent, ok := prompts[2][4].Content.([]ai.ContentBlock)
	if !ok {
		t.Fatalf("third-turn follow-up content type = %T, want []ai.ContentBlock", prompts[2][4].Content)
	}
	if got := thirdContent[0].Text; got != "follow-up two" {
		t.Fatalf("third-turn follow-up content = %q, want %q", got, "follow-up two")
	}

	state := agent.State()
	if len(state.Messages) != 6 {
		t.Fatalf("len(state.Messages) = %d, want 6 after follow-up-triggered turns", len(state.Messages))
	}
	if agent.HasQueuedMessages() {
		t.Fatal("HasQueuedMessages = true, want follow-up queue drained after all turns")
	}
}

func TestPromptAdmitsSteeringBeforeToolContinuation(t *testing.T) {
	steeringQueued := make(chan struct{})
	var steeringOnce sync.Once
	var prompts [][]ai.Message
	var streamCalls int

	toolCall, err := CreateToolCall("slow", map[string]any{})
	if err != nil {
		t.Fatalf("CreateToolCall: %v", err)
	}

	var agent *Agent
	agent, err = New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithTools(Tool{
			Name:       "slow",
			Parameters: map[string]any{"type": "object"},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				steeringOnce.Do(func() {
					agent.Steer(NewUserMessage("guide the in-flight task"))
					close(steeringQueued)
				})
				return &ToolResult{Content: "tool result"}, nil
			},
		}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					streamCalls++
					prompts = append(prompts, append([]ai.Message(nil), prompt.Messages...))

					partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
					handler(ctx, ai.Start{Partial: partial})
					switch streamCalls {
					case 1:
						partial.Content = []ai.ContentBlock{{Type: ai.ContentBlockToolCall, ID: toolCall.ID, Name: toolCall.Name, Arguments: toolCall.Arguments}}
						handler(ctx, ai.ToolCallStart{ContentIndex: 0, Partial: partial})
						final := partial
						final.StopReason = ai.StopReasonToolUse
						handler(ctx, ai.Done{Reason: ai.StopReasonToolUse, Message: final})
					default:
						partial.Content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}
						final := partial
						final.StopReason = ai.StopReasonStop
						handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
					}
					return nil
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := agent.Prompt(context.Background(), NewUserMessage("primary prompt")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	select {
	case <-steeringQueued:
	default:
		t.Fatal("tool did not queue steering guidance")
	}
	if len(prompts) != 2 {
		t.Fatalf("len(prompts) = %d, want 2: initial tool turn and one guided continuation", len(prompts))
	}
	if len(prompts[1]) != 5 {
		t.Fatalf("len(prompts[1]) = %d, want assistant tool-use, tool result, and steering before continuation", len(prompts[1]))
	}
	lastContent, ok := prompts[1][4].Content.([]ai.ContentBlock)
	if !ok {
		t.Fatalf("steering content type = %T, want []ai.ContentBlock", prompts[1][4].Content)
	}
	if got := lastContent[0].Text; got != "guide the in-flight task" {
		t.Fatalf("continuation last message = %q, want queued steering guidance before continuation", got)
	}
	if agent.HasQueuedMessages() {
		t.Fatal("steering queue should be drained into the tool continuation")
	}
}

func TestPromptDrainsQueuedSteeringInSingleFirstTurnWhenModeAll(t *testing.T) {
	var prompts [][]ai.Message

	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithSteeringMode(QueueModeAll),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					prompts = append(prompts, append([]ai.Message(nil), prompt.Messages...))

					partial := ai.AssistantMessage{
						Role:      "assistant",
						Timestamp: time.Now().UnixMilli(),
						Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
					}
					handler(ctx, ai.Start{Partial: partial})

					final := partial
					final.StopReason = ai.StopReasonStop
					handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
					return nil
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	agent.Steer(NewUserMessage("queued steering one"))
	agent.Steer(NewUserMessage("queued steering two"))

	if err := agent.Prompt(context.Background(), NewUserMessage("primary prompt")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if len(prompts) != 1 {
		t.Fatalf("len(prompts) = %d, want 1 after queued steering drains in the first turn", len(prompts))
	}
	if len(prompts[0]) != 3 {
		t.Fatalf("len(prompts[0]) = %d, want 3 once both steering messages run on the first turn", len(prompts[0]))
	}

	firstSteering, ok := prompts[0][1].Content.([]ai.ContentBlock)
	if !ok {
		t.Fatalf("first steering content type = %T, want []ai.ContentBlock", prompts[0][1].Content)
	}
	if got := firstSteering[0].Text; got != "queued steering one" {
		t.Fatalf("first-turn first steering content = %q, want %q", got, "queued steering one")
	}

	secondSteering, ok := prompts[0][2].Content.([]ai.ContentBlock)
	if !ok {
		t.Fatalf("second steering content type = %T, want []ai.ContentBlock", prompts[0][2].Content)
	}
	if got := secondSteering[0].Text; got != "queued steering two" {
		t.Fatalf("first-turn second steering content = %q, want %q", got, "queued steering two")
	}

	state := agent.State()
	if len(state.Messages) != 4 {
		t.Fatalf("len(state.Messages) = %d, want 4 after both steering messages and one assistant turn", len(state.Messages))
	}
	if agent.HasQueuedMessages() {
		t.Fatal("HasQueuedMessages = true, want steering queue drained after the first turn")
	}
}

func TestNewSeedsInitialMessages(t *testing.T) {
	msg := NewUserMessage("seeded")

	agent, err := New(NewConfig(
		WithInitialMessages(msg),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := agent.Messages()
	if len(got) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(got))
	}
	if got[0] != msg {
		t.Fatalf("message pointer = %p, want %p", got[0], msg)
	}
}

func TestSettersReplaceMutableState(t *testing.T) {
	agent, err := New(NewConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	model := &ai.Model{ID: "model-1"}
	tool := Tool{Name: "echo"}
	msg := NewUserMessage("hello")

	agent.SetModel(model)
	agent.SetTools(tool)
	agent.ReplaceMessages([]*Message{msg})

	state := agent.State()
	if state.Model != model {
		t.Fatalf("state.Model = %p, want %p", state.Model, model)
	}
	if len(state.Tools) != 1 || state.Tools[0].Name != "echo" {
		t.Fatalf("state.Tools = %#v, want single echo tool", state.Tools)
	}
	if len(state.Messages) != 1 || state.Messages[0] != msg {
		t.Fatalf("state.Messages = %#v, want single seeded message", state.Messages)
	}
}

func TestSetModelUsesFuturePrompt(t *testing.T) {
	var usedModel *ai.Model

	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "before", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			usedModel = model
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					handler(ctx, ai.Start{Partial: ai.AssistantMessage{
						Role:      "assistant",
						Timestamp: time.Now().UnixMilli(),
						Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
					}})
					handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{
						Role:       "assistant",
						Timestamp:  time.Now().UnixMilli(),
						Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
						StopReason: ai.StopReasonStop,
					}})
					return nil
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	after := &ai.Model{ID: "after", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}
	agent.SetModel(after)

	if err := agent.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if usedModel != after {
		t.Fatalf("usedModel = %p, want %p", usedModel, after)
	}
}

func TestSetModelWakesUsageLimitRetry(t *testing.T) {
	for _, provider := range []string{ai.ProviderOpenAI, ai.ProviderAnthropic} {
		for _, timing := range []string{"waiting", "wait_event", "in_flight"} {
			t.Run(provider+"/"+timing, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					before := &ai.Model{ID: "before", Provider: ai.ProviderOpenAI}
					after := &ai.Model{ID: "after", Provider: provider}
					var models, clients []*ai.Model
					var waits int
					var a *Agent
					var err error
					a, err = New(NewConfig(WithModel(before), WithClientFactory(func(model *ai.Model) (ai.Client, error) {
						clients = append(clients, model)
						return stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
							models = append(models, model)
							if len(models) == 1 {
								if timing == "in_flight" {
									a.SetModel(after)
								}
								return &ai.CodexUsageLimitError{StatusCode: 429, ResetsAt: time.Now().Add(time.Hour)}
							}
							for _, message := range prompt.Messages {
								require.False(t, message.IsError)
							}
							handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}}, StopReason: ai.StopReasonStop}})
							return nil
						}}, nil
					})))
					require.NoError(t, err)
					a.Subscribe(func(event Event, _ <-chan struct{}) {
						if _, ok := event.(UsageLimitWait); ok {
							waits++
							if timing == "wait_event" {
								a.SetModel(after)
							}
						}
					})
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					done := make(chan error, 1)
					go func() { done <- a.Prompt(ctx, NewUserMessage("hi")) }()
					synctest.Wait()
					if timing == "waiting" {
						a.SetModel(&ai.Model{ID: before.ID, Provider: before.Provider})
						synctest.Wait()
						require.Len(t, models, 1, "reselecting the same model must not retry")
						a.SetModel(after)
						synctest.Wait()
					}
					select {
					case err := <-done:
						require.NoError(t, err)
					default:
						cancel()
						<-done
						t.Fatal("model change left the quota countdown running")
					}
					require.Equal(t, []*ai.Model{before, after}, models)
					require.Equal(t, models, clients)
					require.Equal(t, 1, waits)
					require.Equal(t, "ok", messageText(a.Messages()[len(a.Messages())-1]))
				})
			})
		}
	}
}

func TestWithStreamFnOverridesClientCreation(t *testing.T) {
	var sawStreamFn bool

	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithStreamFn(func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			sawStreamFn = true
			handler(ctx, ai.Start{Partial: ai.AssistantMessage{
				Role:      "assistant",
				Timestamp: time.Now().UnixMilli(),
				Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
			}})
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{
				Role:       "assistant",
				Timestamp:  time.Now().UnixMilli(),
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
				StopReason: ai.StopReasonStop,
			}})
			return nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := agent.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !sawStreamFn {
		t.Fatal("expected custom stream function to be used")
	}
}

func TestSignalRemainsOpenAfterSuccessfulRun(t *testing.T) {
	var signal <-chan struct{}

	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					handler(ctx, ai.Start{Partial: ai.AssistantMessage{
						Role:      "assistant",
						Timestamp: time.Now().UnixMilli(),
						Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
					}})
					handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{
						Role:       "assistant",
						Timestamp:  time.Now().UnixMilli(),
						Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
						StopReason: ai.StopReasonStop,
					}})
					return nil
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	agent.Subscribe(func(event Event, runSignal <-chan struct{}) {
		if _, ok := event.(AgentStart); ok {
			signal = runSignal
		}
	})

	if err := agent.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if signal == nil {
		t.Fatal("expected run signal")
	}

	select {
	case <-signal:
		t.Fatal("signal closed after successful run, want open unless aborted")
	default:
	}
}

func TestPromptDrainsQueuedFollowUpsInSingleTurnWhenModeAll(t *testing.T) {
	var prompts [][]ai.Message

	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithFollowUpMode(QueueModeAll),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					prompts = append(prompts, append([]ai.Message(nil), prompt.Messages...))

					partial := ai.AssistantMessage{
						Role:      "assistant",
						Timestamp: time.Now().UnixMilli(),
						Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
					}
					handler(ctx, ai.Start{Partial: partial})

					final := partial
					final.StopReason = ai.StopReasonStop
					handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
					return nil
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	agent.FollowUp(NewUserMessage("follow-up one"))
	agent.FollowUp(NewUserMessage("follow-up two"))

	if err := agent.Prompt(context.Background(), NewUserMessage("primary prompt")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if len(prompts) != 2 {
		t.Fatalf("len(prompts) = %d, want 2 after queued follow-ups drain in one continuation turn", len(prompts))
	}
	if len(prompts[1]) != 4 {
		t.Fatalf("len(prompts[1]) = %d, want 4 once both follow-ups run on the second turn", len(prompts[1]))
	}

	firstFollowUp, ok := prompts[1][2].Content.([]ai.ContentBlock)
	if !ok {
		t.Fatalf("first follow-up content type = %T, want []ai.ContentBlock", prompts[1][2].Content)
	}
	if got := firstFollowUp[0].Text; got != "follow-up one" {
		t.Fatalf("second-turn first follow-up content = %q, want %q", got, "follow-up one")
	}

	secondFollowUp, ok := prompts[1][3].Content.([]ai.ContentBlock)
	if !ok {
		t.Fatalf("second follow-up content type = %T, want []ai.ContentBlock", prompts[1][3].Content)
	}
	if got := secondFollowUp[0].Text; got != "follow-up two" {
		t.Fatalf("second-turn second follow-up content = %q, want %q", got, "follow-up two")
	}

	state := agent.State()
	if len(state.Messages) != 5 {
		t.Fatalf("len(state.Messages) = %d, want 5 after both follow-up messages and two assistant turns", len(state.Messages))
	}
	if agent.HasQueuedMessages() {
		t.Fatal("HasQueuedMessages = true, want follow-up queue drained after the single continuation turn")
	}
}

func TestSubscribeUnsubscribeUsesStableHandle(t *testing.T) {
	agent, err := New(NewConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var calls int
	unsubscribe := agent.Subscribe(func(event Event, signal <-chan struct{}) {
		calls++
	})

	agent.emit(AgentStart{})
	unsubscribe()
	agent.emit(AgentStart{})

	if calls != 1 {
		t.Fatalf("listener calls = %d, want 1", calls)
	}
}

func TestDefaultConvertToLlmFiltersMessages(t *testing.T) {
	// Verify that the exported DefaultConvertToLlm matches the constructor
	// default behavior: filters to RoleUser, RoleAssistant, RoleTool.
	now := time.Now()

	// Create a message with a custom role that should be filtered out
	customMsg := &Message{
		Role:      "custom_role",
		Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "filtered out"}},
		Timestamp: now,
	}
	userMsg := &Message{
		Role:      RoleUser,
		Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "hello"}},
		Timestamp: now,
	}
	toolMsg := &Message{
		Role:       RoleTool,
		ToolCallID: "call_1",
		ToolName:   "bash",
		Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}},
		Timestamp:  now,
	}

	converted, err := DefaultConvertToLlm(context.Background(), []*Message{customMsg, userMsg, toolMsg})
	if err != nil {
		t.Fatalf("DefaultConvertToLlm: %v", err)
	}
	if len(converted) != 2 {
		t.Fatalf("converted len = %d, want 2 (custom_role should be filtered)", len(converted))
	}
	if converted[0].Role != "user" {
		t.Fatalf("first message role = %q, want user", converted[0].Role)
	}
	if converted[1].Role != "toolResult" {
		t.Fatalf("second message role = %q, want toolResult", converted[1].Role)
	}
}

func TestWaitForIdleReturnsAfterRunCompletion(t *testing.T) {
	// Verify WaitForIdle returns after a run completes.
	agent, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{
				stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
					handler(ctx, ai.Start{Partial: ai.AssistantMessage{
						Role:      "assistant",
						Timestamp: time.Now().UnixMilli(),
						Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
					}})
					handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{
						Role:       "assistant",
						Timestamp:  time.Now().UnixMilli(),
						Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
						StopReason: ai.StopReasonStop,
					}})
					return nil
				},
			}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var listenerCalls int
	agent.Subscribe(func(event Event, signal <-chan struct{}) {
		listenerCalls++
	})

	if err := agent.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// WaitForIdle should return (not block forever)
	done := make(chan struct{})
	go func() {
		agent.WaitForIdle()
		close(done)
	}()

	select {
	case <-done:
		// Good: WaitForIdle returned
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForIdle blocked indefinitely")
	}

	// Listener should have been called (sync emission)
	if listenerCalls == 0 {
		t.Fatal("expected listener to be called synchronously")
	}
}

// A /compact issued while a turn is still in flight must take effect for the
// remaining model calls of that turn: the SDK swaps agent state via
// ReplaceMessages mid-run, so the running loop has to notice and rebase its
// working context onto the replacement instead of continuing from the stale
// pre-compaction snapshot.
func TestPromptMidRunReplaceMessagesRebasesContext(t *testing.T) {
	toolCall, err := CreateToolCall("compact_now", map[string]any{})
	if err != nil {
		t.Fatalf("CreateToolCall: %v", err)
	}

	var streamCalls int
	var secondPrompt *ai.Context
	var a *Agent
	a, err = New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithTools(Tool{
			Name:       "compact_now",
			Parameters: map[string]any{"type": "object"},
			Execute: func(ctx ToolContext) (*ToolResult, error) {
				a.ReplaceMessages([]*Message{{
					Role:      RoleSystem,
					Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "Session Summary: compacted away"}},
					Timestamp: time.Now(),
				}})
				return &ToolResult{Content: "compacted"}, nil
			},
		}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) {
			return stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
				streamCalls++
				partial := ai.AssistantMessage{Role: "assistant", Timestamp: time.Now().UnixMilli()}
				switch streamCalls {
				case 1:
					handler(ctx, ai.Start{Partial: partial})
					partial.Content = []ai.ContentBlock{{
						Type:      ai.ContentBlockToolCall,
						ID:        toolCall.ID,
						Name:      toolCall.Name,
						Arguments: toolCall.Arguments,
					}}
					final := partial
					final.StopReason = ai.StopReasonToolUse
					handler(ctx, ai.Done{Reason: ai.StopReasonToolUse, Message: final})
					return nil
				case 2:
					secondPrompt = prompt
					handler(ctx, ai.Start{Partial: partial})
					partial.Content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}
					final := partial
					final.StopReason = ai.StopReasonStop
					handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
					return nil
				default:
					t.Errorf("unexpected stream call %d", streamCalls)
					return nil
				}
			}}, nil
		}),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := a.Prompt(context.Background(), NewUserMessage("original user instructions")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if secondPrompt == nil {
		t.Fatal("model was not called a second time")
	}
	if !strings.Contains(secondPrompt.SystemPrompt, "Session Summary: compacted away") {
		t.Fatalf("second model call does not carry the compaction summary in its system prompt: %q", secondPrompt.SystemPrompt)
	}
	if n := len(secondPrompt.Messages); n == 0 || secondPrompt.Messages[n-1].Role != "user" {
		t.Fatalf("second model call must end with a user message after compaction (providers reject empty input): %#v", secondPrompt.Messages)
	}
	for _, m := range secondPrompt.Messages {
		if m.Role == "toolResult" {
			t.Fatalf("second model call received an orphan tool result: %#v", m)
		}
		if content, ok := m.ContentString(); ok && strings.Contains(content, "original user instructions") {
			t.Fatalf("second model call still sees the pre-compaction user message: %#v", m)
		}
	}
}

func TestContinueResumesAfterFailedAssistantMessage(t *testing.T) {
	var lastRoles []string
	client := stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
		lastRoles = nil
		for _, m := range prompt.Messages {
			lastRoles = append(lastRoles, m.Role)
		}
		final := ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}}, StopReason: ai.StopReasonStop, Timestamp: time.Now().UnixMilli()}
		handler(ctx, ai.Start{Partial: ai.AssistantMessage{Role: "assistant", Timestamp: final.Timestamp}})
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
		return nil
	}}
	a, err := New(NewConfig(
		WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI}),
		WithClientFactory(func(model *ai.Model) (ai.Client, error) { return client, nil }),
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.ReplaceMessages([]*Message{
		NewUserMessage("do the thing"),
		{Role: RoleAssistant, StopReason: ai.StopReasonAborted, IsError: true, Timestamp: time.Now()},
		{Role: RoleAssistant, StopReason: ai.StopReasonError, IsError: true, Timestamp: time.Now()},
	})

	if err := a.Continue(context.Background()); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if len(lastRoles) != 1 || lastRoles[0] != "user" {
		t.Fatalf("prompt roles = %v, want [user]", lastRoles)
	}
	msgs := a.Messages()
	if len(msgs) != 2 || msgs[1].StopReason != ai.StopReasonStop {
		t.Fatalf("messages after retry = %d, want user + successful assistant", len(msgs))
	}
}

func TestPeekQueuedMessagesDoesNotConsumeQueues(t *testing.T) {
	a, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	a.Steer(NewUserMessage("steer"))
	a.FollowUp(NewUserMessage("follow"))
	steering, followUp := a.PeekQueuedMessages()
	if len(steering) != 1 || len(followUp) != 1 {
		t.Fatal("queued messages missing")
	}
	steering[0] = nil
	followUp[0] = nil
	steering, followUp = a.PeekQueuedMessages()
	if steering[0] == nil || followUp[0] == nil {
		t.Fatal("peek returned queue backing slices")
	}
	if s, f := a.QueueSizes(); s != 1 || f != 1 {
		t.Fatalf("peek consumed queues: %d %d", s, f)
	}
}

func TestSettledEventObservesIdleState(t *testing.T) {
	var a *Agent
	observed := false
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", Content: []ai.ContentBlock{{Type: "text", Text: "ok"}}}})
		return nil
	}}
	var err error
	a, err = New(Config{Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }, EventDispatcher: func(ctx context.Context, name string, _ []byte, _ string) EventDispatchResult {
		if name == "agent_settled" {
			observed = true
			if a.State().IsStreaming {
				t.Error("settled while busy")
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
	if !observed {
		t.Fatal("missing settled event")
	}
}

func TestSetThinkingBudgetsAppliesToFutureLoop(t *testing.T) {
	a, err := New(NewConfig(WithThinkingLevel(ThinkingMedium), WithThinkingBudgets(&ai.ThinkingBudgets{Medium: new(1234)})))
	if err != nil {
		t.Fatal(err)
	}
	first := a.createLoopConfig(false, nil, "")
	a.SetThinkingBudgets(&ai.ThinkingBudgets{Medium: new(4321)})
	next := a.createLoopConfig(false, nil, "")
	if got := ai.ThinkingBudgetForLevel(ai.ReasoningLevel(first.ThinkingLevel), first.ThinkingBudgets); got != 1234 {
		t.Fatalf("previous loop budget = %d", got)
	}
	if got := ai.ThinkingBudgetForLevel(ai.ReasoningLevel(next.ThinkingLevel), next.ThinkingBudgets); got != 4321 {
		t.Fatalf("next loop budget = %d", got)
	}
}

func TestConcurrentPromptsAdmitOnlyOneRun(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	a, err := New(Config{Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return client, nil }})
	if err != nil {
		t.Fatal(err)
	}
	// Both public preflight checks can pass before either run acquires ownership.
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- a.runPromptMessages(t.Context(), []*Message{NewUserMessage("hi")}, false, nil, "")
		}()
	}
	close(start)
	<-entered
	select {
	case err := <-results:
		if !errors.Is(err, ErrAlreadyProcessing) {
			t.Errorf("second run: %v", err)
		}
	case <-entered:
		t.Error("two runs entered provider simultaneously")
	}
	close(release)
	if err := <-results; err != nil {
		t.Error(err)
	}
}

func TestRuntimeSnapshotsSynchronizeWithSetters(t *testing.T) {
	a, err := New(NewConfig())
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Go(func() {
		for range 100 {
			a.SetModel(&ai.Model{ID: "test"})
			a.SetSystemPrompt("updated")
			a.SetTools(Tool{Name: "test"})
			a.SetThinkingLevel(ThinkingHigh)
		}
	})
	workers.Go(func() {
		for range 100 {
			a.createContextSnapshot()
			a.createLoopConfig(false, nil, "")
		}
	})
	workers.Wait()
}

func TestResetRetainsReplayedTranscriptBaseline(t *testing.T) {
	a, err := New(NewConfig())
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.UnixMilli(123)
	a.ReplaceMessages([]*Message{
		{Role: RoleSystem, Timestamp: timestamp, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "base"}}, Sections: map[string]*string{"policy": new("old")}, ToolsAdded: []ai.Tool{{Name: "old"}}},
		NewUserMessage("hello"),
		{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "extra"}}, Sections: map[string]*string{"policy": new("new")}, ToolsRemoved: []ai.ToolReference{{Name: "old"}}, ToolsAdded: []ai.Tool{{Name: "new"}}},
	})
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	messages := a.Messages()
	if len(messages) != 1 || !messages[0].Timestamp.Equal(timestamp) {
		t.Fatalf("baseline timestamp lost: %+v", messages)
	}
	if len(messages) != 1 || messages[0].Role != RoleSystem || messageText(messages[0]) != "base\n\nextra" || messages[0].Sections["policy"] == nil || *messages[0].Sections["policy"] != "new" || len(messages[0].ToolsAdded) != 1 || messages[0].ToolsAdded[0].Name != "new" {
		t.Fatalf("baseline=%+v", messages)
	}
}

func TestWaitForIdleIncludesSettledHandlers(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	a, err := New(Config{EventDispatcher: func(ctx context.Context, name string, _ []byte, _ string) EventDispatchResult {
		if name == "agent_settled" {
			close(entered)
			<-release
		}
		return EventDispatchResult{}
	}})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() {
		runDone <- a.runWithLifecycle(context.Background(), func(context.Context) error { return nil })
	}()
	<-entered
	if a.State().IsStreaming {
		t.Error("settled handler must observe idle processing state")
	}
	idle := make(chan struct{})
	go func() { a.WaitForIdle(); close(idle) }()
	select {
	case <-idle:
		close(release)
		<-runDone
		t.Fatal("idle returned before settled handler completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("idle did not return after settlement")
	}
}

func TestWaitForIdleIncludesRunStartedDuringSettlement(t *testing.T) {
	var a *Agent
	var once sync.Once
	nextStarted := make(chan struct{})
	releaseNext := make(chan struct{})
	nextDone := make(chan error, 1)
	var err error
	a, err = New(Config{EventDispatcher: func(ctx context.Context, name string, _ []byte, _ string) EventDispatchResult {
		if name == "agent_settled" {
			once.Do(func() {
				go func() {
					nextDone <- a.runWithLifecycle(t.Context(), func(context.Context) error {
						close(nextStarted)
						<-releaseNext
						return nil
					})
				}()
				<-nextStarted
			})
		}
		return EventDispatchResult{}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.runWithLifecycle(t.Context(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	idle := make(chan struct{})
	go func() { a.WaitForIdle(); close(idle) }()
	select {
	case <-idle:
		close(releaseNext)
		<-nextDone
		t.Fatal("idle returned while settlement-triggered run was active")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseNext)
	if err := <-nextDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("idle did not return after second run settled")
	}
}

func TestResetDoesNotMutateActiveRun(t *testing.T) {
	a, err := New(NewConfig())
	if err != nil {
		t.Fatal(err)
	}
	a.ReplaceMessages([]*Message{NewUserMessage("keep history")})
	a.Steer(NewUserMessage("keep queued"))
	_, run, err := a.beginRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer a.finishRun(run, nil)
	if err := a.Reset(); !errors.Is(err, ErrAlreadyProcessing) {
		t.Fatalf("Reset error = %v, want ErrAlreadyProcessing", err)
	}
	if state := a.State(); !state.IsStreaming || len(state.Messages) != 1 || messageText(state.Messages[0]) != "keep history" {
		t.Fatalf("reset mutated active run: %+v", state)
	}
	if steering, _ := a.QueueSizes(); steering != 1 {
		t.Fatal("reset discarded active run queue")
	}
}

func TestNewUsesLeadingTranscriptPromptInsteadOfScalar(t *testing.T) {
	a, err := New(NewConfig(WithSystemPrompt("stale scalar"), WithInitialMessages(&Message{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "canonical"}}})))
	if err != nil {
		t.Fatal(err)
	}
	prompt, _ := liftSystemMessagesIntoPrompt(a.createContextSnapshot().SystemPrompt, a.Messages())
	if prompt != "canonical" {
		t.Fatalf("effective prompt = %q, want canonical", prompt)
	}
}

func TestDefaultConvertToLlmCustomMessages(t *testing.T) {
	custom := &Message{Role: "custom", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "extension context"}}, Data: map[string]any{"customType": "notice"}}
	excluded := *custom
	excluded.ExcludeFromContext = true
	messages, err := DefaultConvertToLlm(t.Context(), []*Message{custom, &excluded})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != "user" || messages[0].ContentBlocks()[0].Text != "extension context" {
		t.Fatalf("custom conversion = %+v", messages)
	}
	if custom.Role != "custom" {
		t.Fatal("conversion mutated custom role")
	}
}

func TestContinueRejectsSystemOnlyTranscript(t *testing.T) {
	a, err := New(NewConfig())
	if err != nil {
		t.Fatal(err)
	}
	a.ReplaceMessages([]*Message{{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "baseline"}}}})
	if err := a.Continue(t.Context()); !errors.Is(err, ErrNoMessagesToContinue) {
		t.Fatalf("Continue error = %v, want ErrNoMessagesToContinue", err)
	}
}

func TestEmitSnapshotsListenersBeforeCallbacks(t *testing.T) {
	a, err := New(NewConfig())
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	var unsubscribe func()
	unsubscribe = a.Subscribe(func(Event, <-chan struct{}) {
		calls = append(calls, "first")
		unsubscribe()
	})
	a.Subscribe(func(Event, <-chan struct{}) { calls = append(calls, "second") })
	a.Subscribe(func(Event, <-chan struct{}) { calls = append(calls, "third") })
	a.emit(AgentStart{})
	if !slices.Equal(calls, []string{"first", "second", "third"}) {
		t.Fatalf("listener calls = %v", calls)
	}
}

func TestAppendCustomMessageDefersUntilToolResultsAndDoesNotTriggerTurn(t *testing.T) {
	var a *Agent
	var requests int
	custom := &Message{Role: RoleCustom, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "context only"}}}
	tool := Tool{Name: "work", Parameters: map[string]any{"type": "object"}, Execute: func(tc ToolContext) (*ToolResult, error) {
		if err := a.AppendCustomMessage(tc.Context, custom); err != nil {
			return nil, err
		}
		if slices.Contains(a.Messages(), custom) {
			t.Error("custom message appended before tool result")
		}
		return &ToolResult{Content: "result"}, nil
	}}
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		requests++
		final := ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "done"}}}
		if requests == 1 {
			final.StopReason = ai.StopReasonToolUse
			final.Content = []ai.ContentBlock{{Type: ai.ContentBlockToolCall, ID: "work-1", Name: "work", Arguments: []byte(`{}`)}}
		} else {
			var texts []string
			for _, msg := range prompt.Messages {
				for _, block := range msg.ContentBlocks() {
					texts = append(texts, block.Text)
				}
			}
			if !slices.Contains(texts, "context only") || !slices.Contains(texts, "from turn_end") {
				t.Errorf("next request missing custom context: %v", texts)
			}
		}
		handler(ctx, ai.Done{Reason: final.StopReason, Message: final})
		return nil
	}}
	var err error
	a, err = New(NewConfig(WithModel(&ai.Model{ID: "test", API: ai.APIOpenAICompletions}), WithTools(tool), WithClientFactory(func(*ai.Model) (ai.Client, error) { return client, nil })))
	if err != nil {
		t.Fatal(err)
	}
	a.Subscribe(func(event Event, _ <-chan struct{}) {
		if end, ok := event.(TurnEnd); ok && len(end.ToolResult) > 0 {
			if err := a.AppendCustomMessage(t.Context(), &Message{Role: RoleCustom, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "from turn_end"}}}); err != nil {
				t.Error(err)
			}
		}
	})
	if err := a.Prompt(t.Context(), NewUserMessage("start")); err != nil {
		t.Fatal(err)
	}
	messages := a.Messages()
	if messages[0].Role != RoleSystem {
		t.Fatal("missing canonical tool baseline")
	}
	messages = messages[1:]
	if requests != 2 || len(messages) != 6 || messages[2].Role != RoleTool || messages[3] != custom {
		t.Fatalf("requests=%d messages=%+v", requests, messages)
	}
	if err := a.AppendCustomMessage(t.Context(), &Message{Role: RoleCustom}); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(a.Messages()) != 8 {
		t.Fatal("idle append started a request or lost message")
	}
	if err := a.AppendCustomMessage(t.Context(), NewUserMessage("invalid")); err == nil {
		t.Fatal("accepted non-custom message")
	}
}

func TestAppendCustomMessageCancellationAndRunFailure(t *testing.T) {
	a, err := New(NewConfig())
	if err != nil {
		t.Fatal(err)
	}
	custom := &Message{Role: RoleCustom, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "retained"}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.AppendCustomMessage(ctx, custom); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled append: %v", err)
	}
	if len(a.Messages()) != 0 {
		t.Fatal("canceled append changed history")
	}
	var roles []MessageRole
	a.Subscribe(func(event Event, _ <-chan struct{}) {
		if end, ok := event.(MessageEnd); ok {
			roles = append(roles, end.Message.Role)
		}
	})
	failure := errors.New("startup failed")
	err = a.runWithLifecycle(t.Context(), func(ctx context.Context) error {
		if err := a.AppendCustomMessage(ctx, custom); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("run error: %v", err)
	}
	messages := a.Messages()
	if len(messages) != 2 || messages[1] != custom || !slices.Equal(roles, []MessageRole{RoleAssistant, RoleCustom}) {
		t.Fatalf("messages=%+v emitted=%v", messages, roles)
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	a.flushCustomMessages()
	if len(a.Messages()) != 0 {
		t.Fatal("custom message delivered twice")
	}
}

func TestQueueCustomMessageRequiresActiveRun(t *testing.T) {
	a, err := New(NewConfig())
	if err != nil {
		t.Fatal(err)
	}
	message := &Message{Role: RoleCustom, CustomType: "queued"}
	if queued, err := a.QueueCustomMessage(t.Context(), message); err != nil || queued {
		t.Fatalf("idle admission: %v, %v", queued, err)
	}
	if queued, err := a.QueueCustomMessage(t.Context(), nil); err == nil || queued {
		t.Fatal("accepted nil message")
	}
	if queued, err := a.QueueCustomMessage(t.Context(), NewUserMessage("invalid")); err == nil || queued {
		t.Fatal("accepted user message")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if queued, err := a.QueueCustomMessage(ctx, message); !errors.Is(err, context.Canceled) || queued {
		t.Fatalf("canceled admission: %v, %v", queued, err)
	}
	var published int
	unsubscribe := a.Subscribe(func(event Event, _ <-chan struct{}) {
		if end, ok := event.(MessageEnd); ok && end.Message == message {
			published++
		}
	})
	defer unsubscribe()
	err = a.runWithLifecycle(t.Context(), func(ctx context.Context) error {
		queued, err := a.QueueCustomMessage(ctx, message)
		if err != nil {
			return err
		}
		if !queued {
			return errors.New("active run rejected custom message")
		}
		if published != 0 {
			return errors.New("custom message published before turn boundary")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if published != 1 {
		t.Fatalf("published %d times", published)
	}
	a.flushCustomMessages()
	if published != 1 {
		t.Fatal("published twice")
	}
}

func TestReplaceMessagesUsesTranscriptPromptInsteadOfStaleScalar(t *testing.T) {
	a, err := New(NewConfig(WithSystemPrompt("stale scalar")))
	if err != nil {
		t.Fatal(err)
	}
	a.ReplaceMessages([]*Message{{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "restored policy"}}}, NewUserMessage("hello")})
	prompt, _ := liftSystemMessagesIntoPrompt(a.createContextSnapshot().SystemPrompt, a.Messages())
	if prompt != "restored policy" {
		t.Fatalf("effective prompt=%q", prompt)
	}
}

func TestResetDoesNotResurrectRemovedRegistryTool(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprint(restored), func(t *testing.T) {
			tool := Tool{Name: "read", Parameters: map[string]any{"type": "object"}, Execute: func(ToolContext) (*ToolResult, error) { return &ToolResult{Content: "ok"}, nil }}
			a, err := New(NewConfig(WithTools(tool)))
			if err != nil {
				t.Fatal(err)
			}
			messages := []*Message{{Role: RoleSystem, ToolsRemoved: []ai.ToolReference{{Name: "read"}}}}
			if restored {
				messages = append(messages, &Message{Role: RoleSystem, ToolsAdded: []ai.Tool{{Name: "read"}}})
			}
			a.ReplaceMessages(messages)
			for _, reset := range []bool{false, true} {
				if reset {
					if err := a.Reset(); err != nil {
						t.Fatal(err)
					}
				}
				snapshot := a.createContextSnapshot()
				call := prepareToolCall(t.Context(), snapshot, nil, ai.ToolCall{Name: "read", Arguments: json.RawMessage(`{}`)}, AgentLoopConfig{})
				if (call.immediate == nil) != restored {
					t.Errorf("reset=%v restored=%v prepared=%+v", reset, restored, call)
				}
				client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
					baseline := ai.CurrentSystemMessage(ai.NormalizeContext(prompt).Messages)
					visible := baseline != nil && len(baseline.ToolsAdded) > 0
					if visible != restored {
						t.Errorf("reset=%v visible=%v want %v", reset, visible, restored)
					}
					handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
					return nil
				}}
				_, err := streamAssistantResponse(t.Context(), snapshot, AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{ID: "test"}, "", nil, func(Event) {})
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestExcludedLeadingSystemDoesNotReplaceConfiguredPrompt(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%v", replace), func(t *testing.T) {
			messages := []*Message{{Role: RoleSystem, ExcludeFromContext: true, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "display only"}}}, NewUserMessage("hello")}
			config := NewConfig(WithSystemPrompt("configured policy"))
			if !replace {
				config.InitialMessages = messages
			}
			a, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			if replace {
				a.ReplaceMessages(messages)
			}
			client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
				baseline := ai.CurrentSystemMessage(ai.NormalizeContext(prompt).Messages)
				if baseline == nil || baseline.Content != "configured policy" {
					t.Errorf("excluded entry replaced configured prompt: %+v", baseline)
				}
				handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
				return nil
			}}
			_, err = streamAssistantResponse(t.Context(), a.createContextSnapshot(), AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{ID: "test"}, a.createContextSnapshot().SystemPrompt, nil, func(Event) {})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDefaultConvertToLlmSkipsNilTranscriptEntries(t *testing.T) {
	messages, err := DefaultConvertToLlm(t.Context(), []*Message{nil, NewUserMessage("kept"), nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("converted messages: %+v", messages)
	}
}

func TestDefaultConvertToLlmHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := DefaultConvertToLlm(ctx, []*Message{NewUserMessage("kept")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("conversion ignored cancellation: %v", err)
	}
}

func TestStateReportsReplayedSystemPrompt(t *testing.T) {
	a, err := New(NewConfig(WithSystemPrompt("configured")))
	if err != nil {
		t.Fatal(err)
	}
	a.ReplaceMessages([]*Message{
		{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "canonical"}}, Sections: map[string]*string{"policy": new("current section")}},
		{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "excluded"}}, ExcludeFromContext: true},
	})
	if got := a.State().SystemPrompt; got != "canonical\n\ncurrent section" {
		t.Fatalf("state prompt = %q", got)
	}
	if got := a.createContextSnapshot().SystemPrompt; got != "" {
		t.Fatalf("request scalar duplicates replayed transcript: %q", got)
	}
}

func TestContinueIgnoresTrailingNilTranscriptEntries(t *testing.T) {
	a, err := New(NewConfig())
	if err != nil {
		t.Fatal(err)
	}
	a.ReplaceMessages([]*Message{{Role: RoleAssistant, StopReason: ai.StopReasonStop}, nil})
	if err := a.Continue(t.Context()); !errors.Is(err, ErrCannotContinueAssistant) {
		t.Fatalf("Continue error=%v", err)
	}
	a.ReplaceMessages([]*Message{nil})
	if err := a.Continue(t.Context()); !errors.Is(err, ErrNoMessagesToContinue) {
		t.Fatalf("Continue nil error=%v", err)
	}
}

func TestDefaultConvertToLlmRetainsPublicToolResultRole(t *testing.T) {
	messages, err := DefaultConvertToLlm(t.Context(), []*Message{{Role: "toolResult", ToolCallID: "call", ToolName: "lookup", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "result"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != "toolResult" || messages[0].ToolCallID != "call" {
		t.Fatalf("tool result lost: %+v", messages)
	}
}

func TestSetSystemPromptReplacesReplayedPrompt(t *testing.T) {
	config := NewConfig()
	section := "old section"
	original := &Message{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "old prompt"}}, Sections: map[string]*string{"section": &section}, ToolsAdded: []ai.Tool{{Name: "retained"}}}
	config.InitialMessages = []*Message{original}
	a, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	a.SetSystemPrompt("replacement")
	if got := a.State().SystemPrompt; got != "replacement" {
		t.Fatalf("prompt = %q", got)
	}
	if got := ai.CurrentSystemPrompt(ConvertMessagesToAI(a.Messages())); got != "replacement" {
		t.Fatalf("transcript prompt = %q", got)
	}
	if original.Content[0].Text != "old prompt" || len(original.Sections) != 1 {
		t.Fatal("setter mutated caller-owned history")
	}
	if messages := a.Messages(); len(messages[0].ToolsAdded) != 1 || messages[0].ToolsAdded[0].Name != "retained" {
		t.Fatal("prompt replacement discarded tool declarations")
	}
	a.SetSystemPrompt("")
	if got := a.State().SystemPrompt; got != "" {
		t.Fatalf("cleared prompt = %q", got)
	}
}

func TestPromptReplacementTranscriptSurvivesReconstructionAndReset(t *testing.T) {
	a, err := New(NewConfig(WithSystemPrompt("configured")))
	if err != nil {
		t.Fatal(err)
	}
	a.SetSystemPrompt("replacement")
	for _, prompt := range []string{"replacement", ""} {
		a.SetSystemPrompt(prompt)
		if err := a.Reset(); err != nil {
			t.Fatal(err)
		}
		restored, err := New(NewConfig(WithSystemPrompt("fallback"), WithInitialMessages(a.Messages()...)))
		if err != nil {
			t.Fatal(err)
		}
		if got := restored.State().SystemPrompt; got != prompt {
			t.Fatalf("restored prompt = %q, want %q", got, prompt)
		}
		if restored.createContextSnapshot().SystemPrompt != "" {
			t.Fatal("restoration resurrected scalar fallback")
		}
	}
}

func TestToolRegistryChangesCommitAtRequestBoundary(t *testing.T) {
	var want []string
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		current := ai.CurrentSystemMessage(ai.NormalizeContext(prompt).Messages)
		var names []string
		if current != nil {
			for _, tool := range current.ToolsAdded {
				names = append(names, tool.Name)
			}
		}
		if !slices.Equal(names, want) {
			t.Errorf("request tools = %v, want %v", names, want)
		}
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}}}})
		return nil
	}}
	a, err := New(NewConfig(WithTools(Tool{Name: "old"}, Tool{Name: "undeclared"}), WithInitialMessages(&Message{Role: RoleSystem, ToolsAdded: []ai.Tool{{Name: "old"}}}), WithModel(&ai.Model{ID: "test"}), WithClientFactory(func(*ai.Model) (ai.Client, error) { return client, nil })))
	if err != nil {
		t.Fatal(err)
	}
	a.SetTools(Tool{Name: "new", Description: "new declaration"}, Tool{Name: "undeclared"})
	want = []string{"new"}
	if err := a.Prompt(t.Context(), NewUserMessage("first")); err != nil {
		t.Fatal(err)
	}
	current := ai.CurrentSystemMessage(ConvertMessagesToAI(a.Messages()))
	if current == nil || len(current.ToolsAdded) != 1 || current.ToolsAdded[0].Description != "new declaration" {
		t.Fatalf("uncommitted declarations: %+v", current)
	}
	a.AddTools(Tool{Name: "added"})
	want = []string{"new", "added"}
	if err := a.Prompt(t.Context(), NewUserMessage("second")); err != nil {
		t.Fatal(err)
	}
	var declarations int
	for _, message := range a.Messages() {
		if message.Role == RoleSystem && (len(message.ToolsAdded) > 0 || len(message.ToolsRemoved) > 0) {
			declarations++
		}
	}
	if declarations != 3 {
		t.Fatalf("declaration entries = %d, want 3", declarations)
	}
	if err := a.Prompt(t.Context(), NewUserMessage("unchanged")); err != nil {
		t.Fatal(err)
	}
	var after int
	for _, message := range a.Messages() {
		if message.Role == RoleSystem {
			after++
		}
	}
	if after != declarations {
		t.Fatal("unchanged registry emitted duplicate delta")
	}
}

func TestSystemSectionsCommitAtRequestBoundary(t *testing.T) {
	var prompts []string
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		prompts = append(prompts, ai.CurrentSystemPrompt(ai.NormalizeContext(prompt).Messages))
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}}}})
		return nil
	}}
	a, err := New(NewConfig(WithSystemPrompt("base"), WithModel(&ai.Model{ID: "test"}), WithClientFactory(func(*ai.Model) (ai.Client, error) { return client, nil })))
	if err != nil {
		t.Fatal(err)
	}
	sections := func() int {
		var n int
		for _, message := range a.Messages() {
			if message.Role == RoleSystem && len(message.Sections) > 0 {
				n++
			}
		}
		return n
	}
	a.SetSystemSection("servers", new("listed"))
	if err := a.Prompt(t.Context(), NewUserMessage("first")); err != nil {
		t.Fatal(err)
	}
	a.SetSystemSection("servers", new("listed"))
	if err := a.Prompt(t.Context(), NewUserMessage("unchanged")); err != nil {
		t.Fatal(err)
	}
	if sections() != 1 {
		t.Fatalf("section entries = %d, want 1", sections())
	}
	a.SetSystemSection("servers", nil)
	a.SetSystemSection("absent", nil)
	if err := a.Prompt(t.Context(), NewUserMessage("removed")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"base\n\nlisted", "base\n\nlisted", "base"}; !slices.Equal(prompts, want) {
		t.Fatalf("request prompts = %q, want %q", prompts, want)
	}
	if sections() != 2 {
		t.Fatalf("section entries = %d, want 2", sections())
	}
}

func TestRegistryReloadRefreshesRestoredDeclaration(t *testing.T) {
	tool := Tool{Name: "read", Description: "current schema"}
	a, err := New(NewConfig(WithTools(tool), WithInitialMessages(&Message{Role: RoleSystem, ToolsAdded: []ai.Tool{{Name: "read", Description: "restored schema"}}})))
	if err != nil {
		t.Fatal(err)
	}
	a.SetTools(tool)
	update, err := a.prepareRequest(t.Context(), PrepareRequestContext{Context: a.createContextSnapshot()})
	if err != nil {
		t.Fatal(err)
	}
	current := ai.CurrentSystemMessage(ConvertMessagesToAI(append(update.Context.Messages, update.Messages...)))
	if current.ToolsAdded[0].Description != tool.Description {
		t.Fatalf("stale declaration: %+v", current.ToolsAdded[0])
	}
}

func TestPrepareRequestRetainsExplicitToolRegistry(t *testing.T) {
	config := NewConfig(WithTools(Tool{Name: "registered"}))
	config.PrepareRequest = func(_ context.Context, request PrepareRequestContext) (*AgentRequestUpdate, error) {
		snapshot := *request.Context
		snapshot.Tools = []Tool{{Name: "request-only"}}
		return &AgentRequestUpdate{Context: &snapshot}, nil
	}
	a, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	update, err := a.prepareRequest(t.Context(), PrepareRequestContext{Context: a.createContextSnapshot()})
	if err != nil {
		t.Fatal(err)
	}
	if len(update.Context.Tools) != 1 || update.Context.Tools[0].Name != "request-only" {
		t.Fatalf("registry override lost: %+v", update.Context.Tools)
	}
}

func TestRegistryDeltaIncludedInRunCompletion(t *testing.T) {
	a, err := New(NewConfig(WithTools(Tool{Name: "old"}), WithModel(&ai.Model{ID: "test"}), WithClientFactory(func(*ai.Model) (ai.Client, error) {
		return stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}}}})
			return nil
		}}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	a.SetTools(Tool{Name: "new"})
	var completed []*Message
	a.Subscribe(func(event Event, _ <-chan struct{}) {
		if end, ok := event.(AgentEnd); ok {
			completed = end.Messages
		}
	})
	if err := a.Prompt(t.Context(), NewUserMessage("run")); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(completed, func(message *Message) bool {
		return message.Role == RoleSystem && len(message.ToolsAdded) > 0 && message.ToolsAdded[0].Name == "new"
	}) {
		t.Fatalf("completion omitted declaration: %+v", completed)
	}
}

func TestCanonicalConstructionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		config   Config
		prompt   string
		tools    int
		messages int
	}{
		{name: "shorthand", config: NewConfig(WithSystemPrompt("policy"), WithTools(Tool{Name: "read"})), prompt: "policy", tools: 1, messages: 1},
		{name: "empty", config: NewConfig(), messages: 0},
		{name: "leading empty", config: NewConfig(WithSystemPrompt("ignored"), WithTools(Tool{Name: "read"}), WithInitialMessages(&Message{Role: RoleSystem})), messages: 1},
		{name: "transcript only", config: NewConfig(WithInitialMessages(&Message{Role: RoleSystem, Sections: map[string]*string{"policy": new("transcript")}, ToolsAdded: []ai.Tool{{Name: "not registered"}}})), prompt: "transcript", tools: 1, messages: 1},
		{name: "initial state", config: Config{SystemPrompt: "ignored", Tools: []Tool{{Name: "ignored"}}, InitialState: &InitialState{SystemPrompt: "state", Tools: []Tool{{Name: "state"}}}}, prompt: "state", tools: 1, messages: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := New(tc.config)
			if err != nil {
				t.Fatal(err)
			}
			if got := a.State().SystemPrompt; got != tc.prompt {
				t.Fatalf("prompt=%q, want %q", got, tc.prompt)
			}
			messages := a.Messages()
			if len(messages) != tc.messages {
				t.Fatalf("messages=%d, want %d", len(messages), tc.messages)
			}
			baseline := ai.CurrentSystemMessage(ConvertMessagesToAI(messages))
			var tools int
			if baseline != nil {
				tools = len(baseline.ToolsAdded)
			}
			if tools != tc.tools {
				t.Fatalf("declarations=%d, want %d", tools, tc.tools)
			}
			if got := a.createContextSnapshot().SystemPrompt; got != "" {
				t.Fatalf("noncanonical scalar=%q", got)
			}
		})
	}
}

func TestRegistryReconnectionPreservesUnavailableAndRemovedTools(t *testing.T) {
	a, err := New(NewConfig(WithInitialMessages(&Message{Role: RoleSystem, ToolsAdded: []ai.Tool{{Name: "late", Description: "restored"}}, ToolsRemoved: []ai.ToolReference{{Name: "removed"}}})))
	if err != nil {
		t.Fatal(err)
	}
	baseline := ai.CurrentSystemMessage(ConvertMessagesToAI(a.Messages()))
	if len(baseline.ToolsAdded) != 1 {
		t.Fatal("unavailable declaration lost")
	}
	a.AddTools(Tool{Name: "late", Description: "reconnected"}, Tool{Name: "removed", Exposure: ToolHidden})
	update, err := a.prepareRequest(t.Context(), PrepareRequestContext{Context: a.createContextSnapshot()})
	if err != nil {
		t.Fatal(err)
	}
	effective := ai.CurrentSystemMessage(ConvertMessagesToAI(append(update.Context.Messages, update.Messages...)))
	if len(effective.ToolsAdded) != 1 || effective.ToolsAdded[0].Description != "reconnected" {
		t.Fatalf("incorrect reconnection: %+v", effective)
	}
}

func TestRegistryReconnectionDoesNotResurrectRemovedDeclaration(t *testing.T) {
	a, err := New(NewConfig(WithInitialMessages(&Message{Role: RoleSystem, ToolsRemoved: []ai.ToolReference{{Name: "removed"}}})))
	if err != nil {
		t.Fatal(err)
	}
	a.SetTools(Tool{Name: "removed"})
	update, err := a.prepareRequest(t.Context(), PrepareRequestContext{Context: a.createContextSnapshot()})
	if err != nil {
		t.Fatal(err)
	}
	effective := ai.CurrentSystemMessage(ConvertMessagesToAI(append(update.Context.Messages, update.Messages...)))
	if len(effective.ToolsAdded) != 0 {
		t.Fatalf("registry resurrected removed declaration: %+v", effective)
	}
}

func TestLeadingEmptyBaselineDoesNotInheritRegistry(t *testing.T) {
	a, err := New(NewConfig(WithTools(Tool{Name: "read"}), WithInitialMessages(&Message{Role: RoleSystem, Timestamp: time.UnixMilli(0)})))
	if err != nil {
		t.Fatal(err)
	}
	client := stubClient{stream: func(ctx context.Context, _ *ai.Model, prompt *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
		baseline := ai.CurrentSystemMessage(ai.NormalizeContext(prompt).Messages)
		if baseline != nil && len(baseline.ToolsAdded) > 0 {
			t.Errorf("empty baseline inherited registry: %+v", baseline.ToolsAdded)
		}
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
		return nil
	}}
	_, err = streamAssistantResponse(t.Context(), a.createContextSnapshot(), AgentLoopConfig{ConvertToLlm: DefaultConvertToLlm}, client, &ai.Model{ID: "test"}, "", nil, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPersistedRestorationDoesNotInheritDirectReplacementFallback(t *testing.T) {
	a, err := New(NewConfig(WithSystemPrompt("old policy")))
	if err != nil {
		t.Fatal(err)
	}
	a.RestoreMessages([]*Message{NewUserMessage("selected branch")})
	if got := a.State().SystemPrompt; got != "" {
		t.Fatalf("persisted branch inherited policy: %q", got)
	}
	a.RestoreMessages(nil)
	if len(a.Messages()) != 0 || a.State().SystemPrompt != "" {
		t.Fatal("empty branch inherited state")
	}
}

func TestSetToolsPrunesPendingActivation(t *testing.T) {
	a, err := New(NewConfig(WithTools(Tool{Name: "removed"}, Tool{Name: "hidden"}, Tool{Name: "kept"})))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetActiveTools("removed", "hidden", "kept"); err != nil {
		t.Fatal(err)
	}
	a.SetTools(Tool{Name: "kept"}, Tool{Name: "hidden", Exposure: ToolHidden}, Tool{Name: "added"})
	if got := a.ActiveTools(); !slices.Equal(got, []string{"kept"}) {
		t.Fatalf("pending loadout retained removed tools: %v", got)
	}
	update, err := a.prepareRequest(t.Context(), PrepareRequestContext{Context: a.createContextSnapshot()})
	if err != nil {
		t.Fatal(err)
	}
	baseline := ai.CurrentSystemMessage(ConvertMessagesToAI(append(update.Context.Messages, update.Messages...)))
	if baseline == nil || len(baseline.ToolsAdded) != 1 || baseline.ToolsAdded[0].Name != "kept" {
		t.Fatalf("registry replacement changed explicit loadout: %+v", baseline)
	}
}

func TestRunSetupFailureRecordsFailureTimeErrorMessage(t *testing.T) {
	setupErr := errors.New("client setup failed")
	var failedAt time.Time
	a, err := New(Config{Model: &ai.Model{ID: "test", API: "test-api", Provider: "test-provider"}, ClientFactory: func(*ai.Model) (ai.Client, error) {
		time.Sleep(5 * time.Millisecond)
		failedAt = time.Now()
		return nil, setupErr
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.runPromptMessages(t.Context(), []*Message{NewUserMessage("hi")}, false, nil, ""); !errors.Is(err, setupErr) {
		t.Fatalf("run err = %v", err)
	}
	messages := a.State().Messages
	last := messages[len(messages)-1]
	if last.Role != RoleAssistant || last.StopReason != ai.StopReasonError || !strings.Contains(last.ErrorMessage, "client setup failed") ||
		last.Provider != "test-provider" || last.Timestamp.Before(failedAt) {
		t.Fatalf("failure message = %+v, failed at %v", last, failedAt)
	}
}

func TestRequestOptionsAreReplacedForFutureRuns(t *testing.T) {
	model := &ai.Model{ID: "options", Provider: "local", API: "local", MaxTokens: 64}
	var got []ai.SimpleStreamOptions
	options := []ai.Option{ai.WithMaxRetries(0), ai.WithTimeout(0), ai.WithMaxRetryDelayMs(17)}
	config := NewConfig(WithModel(model), WithRequestOptions(options...), WithStreamFn(func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
		got = append(got, ai.ExtractSimpleStreamOptions(opts))
		handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}}}})
		return nil
	}))
	options[0] = ai.WithMaxRetries(9)
	a, err := New(config)
	require.NoError(t, err)
	require.NoError(t, a.Prompt(t.Context(), NewUserMessage("first")))
	replacement := []ai.Option{ai.WithMaxRetryDelayMs(0)}
	a.SetRequestOptions(replacement...)
	replacement[0] = ai.WithMaxRetryDelayMs(99)
	require.NoError(t, a.Prompt(t.Context(), NewUserMessage("second")))
	require.Len(t, got, 2)
	require.NotNil(t, got[0].MaxRetries)
	assert.Zero(t, *got[0].MaxRetries)
	require.NotNil(t, got[0].Timeout)
	assert.Zero(t, *got[0].Timeout)
	assert.Equal(t, 17, got[0].MaxRetryDelayMs)
	assert.Nil(t, got[1].MaxRetries)
	assert.Nil(t, got[1].Timeout)
	assert.True(t, got[1].MaxRetryDelaySet)
	assert.Zero(t, got[1].MaxRetryDelayMs)
}

func TestSettlementPreservesRunCancellationAndValues(t *testing.T) {
	type contextKey struct{}
	for _, abort := range []bool{false, true} {
		t.Run(fmt.Sprint(abort), func(t *testing.T) {
			ctx := context.WithValue(t.Context(), contextKey{}, "run")
			var a *Agent
			var observed context.Context
			var err error
			a, err = New(Config{EventDispatcher: func(ctx context.Context, name string, _ []byte, _ string) EventDispatchResult {
				if name == "agent_settled" {
					observed = ctx
					if ctx.Value(contextKey{}) != "run" {
						t.Error("settlement lost run value")
					}
					if abort != errors.Is(ctx.Err(), context.Canceled) {
						t.Errorf("settlement cancellation = %v, abort = %v", ctx.Err(), abort)
					}
				}
				return EventDispatchResult{}
			}})
			if err != nil {
				t.Fatal(err)
			}
			err = a.runWithLifecycle(ctx, func(ctx context.Context) error {
				if abort {
					a.Abort()
					return ctx.Err()
				}
				return nil
			})
			if abort != errors.Is(err, context.Canceled) {
				t.Errorf("run = %v, abort = %v", err, abort)
			}
			if observed == nil || !errors.Is(observed.Err(), context.Canceled) {
				t.Error("run context was not released after settlement")
			}
		})
	}
}

func TestPromptPreparesPhysicalClientWithoutChangingSelectedModel(t *testing.T) {
	selected := &ai.Model{ID: "virtual", Provider: "virtual"}
	physical := &ai.Model{ID: "physical", Provider: "physical", API: "physical"}
	var constructed []string
	a, err := New(Config{
		Model: selected,
		PrepareRequest: func(_ context.Context, request PrepareRequestContext) (*AgentRequestUpdate, error) {
			if request.Model != selected {
				t.Errorf("selected model=%+v", request.Model)
			}
			return &AgentRequestUpdate{Model: physical}, nil
		},
		ClientFactory: func(model *ai.Model) (ai.Client, error) {
			constructed = append(constructed, model.ID)
			if model != physical {
				return nil, errors.New("virtual client must not be constructed")
			}
			return stubClient{stream: func(ctx context.Context, model *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
				if model != physical {
					t.Errorf("stream model=%+v", model)
				}
				handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
				return nil
			}}, nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, a.Prompt(t.Context(), NewUserMessage("route")))
	state := a.State()
	require.Equal(t, []string{"physical"}, constructed)
	require.Same(t, selected, state.Model)
	require.Len(t, state.Messages, 2)
	assert.Equal(t, physical.ID, state.Messages[1].Model)
	assert.Equal(t, physical.Provider, state.Messages[1].Provider)
}

func TestCallbackFailureStopsBeforeProviderAndSettles(t *testing.T) {
	for _, callback := range []string{"conversion", "transform", "prompt transform", "credentials"} {
		for _, entry := range []string{"prompt", "continue"} {
			if callback == "prompt transform" && entry == "continue" {
				continue
			}
			t.Run(callback+"/"+entry, func(t *testing.T) {
				failure := errors.New("callback rejected request")
				var streams, callbacks int
				var recovered bool
				config := Config{
					Model:           &ai.Model{ID: "test", Provider: "callback-provider"},
					InitialMessages: []*Message{NewUserMessage("history")},
					ClientFactory: func(*ai.Model) (ai.Client, error) {
						return stubClient{stream: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
							streams++
							handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
							return nil
						}}, nil
					},
				}
				transform := func(ctx context.Context, messages []*Message) ([]*Message, error) {
					callbacks++
					if !recovered {
						return []*Message{NewUserMessage("partial must not be sent")}, failure
					}
					return messages, nil
				}
				var options []PromptOption
				switch callback {
				case "conversion":
					config.ConvertToLlm = func(ctx context.Context, messages []*Message) ([]ai.Message, error) {
						callbacks++
						if !recovered {
							return []ai.Message{{Role: "user", Content: "partial must not be sent"}}, failure
						}
						return DefaultConvertToLlm(ctx, messages)
					}
				case "transform":
					config.TransformContext = transform
				case "prompt transform":
					options = []PromptOption{WithPromptContext(transform)}
					config.TransformContext = func(_ context.Context, messages []*Message) ([]*Message, error) {
						if !recovered {
							t.Error("secondary transform admitted after primary failure")
						}
						return messages, nil
					}
				case "credentials":
					config.APIKeyProvider = func(_ context.Context, provider string) (string, error) {
						callbacks++
						assert.Equal(t, "callback-provider", provider)
						if !recovered {
							return "partial-must-not-be-used", failure
						}
						return "key", nil
					}
				}
				a, err := New(config)
				require.NoError(t, err)
				if entry == "continue" {
					err = a.Continue(t.Context())
				} else {
					err = a.Prompt(t.Context(), NewUserMessage("prompt"), options...)
				}
				require.ErrorIs(t, err, failure)
				assert.Zero(t, streams)
				assert.Equal(t, 1, callbacks)
				a.WaitForIdle()
				state := a.State()
				assert.ErrorIs(t, state.Error, failure)
				assert.False(t, state.IsStreaming)
				require.NotEmpty(t, state.Messages)
				last := state.Messages[len(state.Messages)-1]
				assert.True(t, last.IsError)
				assert.Equal(t, ai.StopReasonError, last.StopReason)
				assert.Contains(t, last.ErrorMessage, failure.Error())
				recovered = true
				require.NoError(t, a.Continue(t.Context()))
				assert.Equal(t, 1, streams)
				assert.NoError(t, a.State().Error)
			})
		}
	}
}

func TestCallbackCancellationRetainsCauseWithoutProviderRequest(t *testing.T) {
	for _, callback := range []string{"conversion", "transform", "credentials"} {
		t.Run(callback, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.Join(context.Canceled, errors.New("callback cancelled request"))
			config := Config{Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) {
				return stubClient{stream: func(context.Context, *ai.Model, *ai.Context, ai.EventHandler, ...ai.Option) error {
					t.Error("cancelled callback admitted a provider request")
					return nil
				}}, nil
			}}
			switch callback {
			case "conversion":
				config.ConvertToLlm = func(ctx context.Context, _ []*Message) ([]ai.Message, error) {
					cancel(cause)
					return nil, context.Cause(ctx)
				}
			case "transform":
				config.TransformContext = func(ctx context.Context, _ []*Message) ([]*Message, error) {
					cancel(cause)
					return nil, context.Cause(ctx)
				}
			case "credentials":
				config.APIKeyProvider = func(ctx context.Context, _ string) (string, error) { cancel(cause); return "", context.Cause(ctx) }
			}
			a, err := New(config)
			require.NoError(t, err)
			require.ErrorIs(t, a.Prompt(ctx, NewUserMessage("prompt")), cause)
			a.WaitForIdle()
			state := a.State()
			assert.ErrorIs(t, state.Error, cause)
			assert.False(t, state.IsStreaming)
			require.NotEmpty(t, state.Messages)
			assert.Equal(t, ai.StopReasonAborted, state.Messages[len(state.Messages)-1].StopReason)
		})
	}
}

func TestRunFailurePublishesOneCompleteLifecycle(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		for _, stage := range []string{"startup", "setup", "conversion", "stream", "cancel"} {
			t.Run(entry+"/"+stage, func(t *testing.T) {
				failure := errors.New("request rejected")
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				var events []Event
				var pluginEvents []string
				config := Config{
					Model: &ai.Model{ID: "test", API: "local", Provider: "local"}, InitialMessages: []*Message{NewUserMessage("history")},
					ClientFactory: func(*ai.Model) (ai.Client, error) { return nil, failure },
					EventDispatcher: func(_ context.Context, name string, _ []byte, _ string) EventDispatchResult {
						pluginEvents = append(pluginEvents, name)
						return EventDispatchResult{}
					},
				}
				if stage == "startup" {
					config.GetStartupMessages = func(context.Context) ([]*Message, error) { return nil, failure }
				}
				if stage == "conversion" || stage == "cancel" {
					config.ConvertToLlm = func(context.Context, []*Message) ([]ai.Message, error) {
						if stage == "cancel" {
							failure = errors.Join(context.Canceled, failure)
							cancel(failure)
						}
						return nil, failure
					}
					config.ClientFactory = func(*ai.Model) (ai.Client, error) {
						return stubClient{stream: func(context.Context, *ai.Model, *ai.Context, ai.EventHandler, ...ai.Option) error {
							t.Error("failed request reached stream")
							return nil
						}}, nil
					}
				}
				if stage == "stream" {
					config.ClientFactory = func(*ai.Model) (ai.Client, error) {
						return stubClient{stream: func(context.Context, *ai.Model, *ai.Context, ai.EventHandler, ...ai.Option) error { return failure }}, nil
					}
				}
				a, err := New(config)
				require.NoError(t, err)
				a.Subscribe(func(event Event, signal <-chan struct{}) {
					events = append(events, event)
					require.NotNil(t, signal)
					assert.True(t, a.State().IsStreaming, "listeners must settle before idle")
				})
				if entry == "prompt" {
					err = a.Prompt(ctx, NewUserMessage("prompt"))
				} else {
					err = a.Continue(ctx)
				}
				require.ErrorIs(t, err, failure)
				var types []string
				for _, event := range events {
					types = append(types, fmt.Sprintf("%T", event))
				}
				var want []string
				if stage != "startup" {
					want = append(want, "agent.AgentStart", "agent.TurnStart")
					if entry == "prompt" {
						want = append(want, "agent.MessageStart", "agent.MessageEnd")
					}
				}
				want = append(want, "agent.MessageStart", "agent.MessageEnd", "agent.TurnEnd", "agent.AgentEnd")
				require.Equal(t, want, types)
				end := events[len(events)-1].(AgentEnd)
				require.Len(t, end.Messages, 1)
				message := end.Messages[0]
				assert.Same(t, message, events[len(events)-4].(MessageStart).Message)
				assert.Same(t, message, events[len(events)-3].(MessageEnd).Message)
				assert.Same(t, message, events[len(events)-2].(TurnEnd).Message)
				assert.Equal(t, 1, len(slices.DeleteFunc(slices.Clone(a.Messages()), func(m *Message) bool { return m != message })))
				terminal := slices.DeleteFunc(slices.Clone(pluginEvents), func(name string) bool {
					return name != "message_start" && name != "message_end" && name != "turn_end" && name != "agent_end" && name != "agent_settled"
				})
				var wantPlugin []string
				if stage != "startup" && entry == "prompt" {
					wantPlugin = append(wantPlugin, "message_start", "message_end")
				}
				wantPlugin = append(wantPlugin, "message_start", "message_end", "turn_end", "agent_end", "agent_settled")
				assert.Equal(t, wantPlugin, terminal)
				a.WaitForIdle()
				assert.False(t, a.State().IsStreaming)
				assert.ErrorIs(t, a.State().Error, failure)
			})
		}
	}
}

func TestRunFailureWaitsForTerminalListener(t *testing.T) {
	failure := errors.New("setup failed")
	a, err := New(Config{Model: &ai.Model{ID: "test"}, ClientFactory: func(*ai.Model) (ai.Client, error) { return nil, failure }})
	require.NoError(t, err)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	a.Subscribe(func(event Event, _ <-chan struct{}) {
		if _, ok := event.(AgentEnd); ok {
			close(entered)
			<-release
		}
	})
	done := make(chan error, 1)
	go func() { done <- a.Prompt(t.Context(), NewUserMessage("prompt")) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("failure terminal listener not reached")
	}
	assert.True(t, a.State().IsStreaming)
	assert.ErrorIs(t, a.State().Error, failure)
	select {
	case err := <-done:
		t.Fatalf("run completed before terminal listener: %v", err)
	default:
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		require.ErrorIs(t, err, failure)
	case <-time.After(time.Second):
		t.Fatal("failure run did not settle")
	}
	a.WaitForIdle()
	assert.False(t, a.State().IsStreaming)
}

func TestRunStreamFailureRetainsPublishedTerminal(t *testing.T) {
	for _, entry := range []string{"prompt", "continue", "cancelled prompt", "cancelled continue"} {
		for _, reason := range []ai.StopReason{ai.StopReasonError, ai.StopReasonAborted, ai.StopReasonStop} {
			t.Run(entry+"/"+string(reason), func(t *testing.T) {
				cleanupErr := errors.New("stream cleanup failed")
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				cause := errors.New("caller cancelled stream")
				cancelled := strings.HasPrefix(entry, "cancelled")
				var events []Event
				model := &ai.Model{ID: "physical", API: "local", Provider: "local"}
				a, err := New(Config{Model: model, InitialMessages: []*Message{NewUserMessage("history")}, StreamFn: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
					message := ai.AssistantMessage{Role: "assistant", StopReason: reason, ErrorMessage: "provider outcome", Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "partial response"}}, Usage: ai.Usage{Input: 7, Output: 3}, Timestamp: 1234567890}
					if reason == ai.StopReasonStop {
						handler(ctx, ai.Done{Message: message})
					} else {
						handler(ctx, ai.StreamError{Msg: message})
					}
					if cancelled {
						cancel(cause)
						return errors.Join(cleanupErr, ctx.Err(), context.Cause(ctx))
					}
					return cleanupErr
				}})
				require.NoError(t, err)
				a.Subscribe(func(event Event, _ <-chan struct{}) {
					events = append(events, event)
					assert.True(t, a.State().IsStreaming)
				})
				if strings.HasSuffix(entry, "prompt") {
					err = a.Prompt(ctx, NewUserMessage("hello"))
				} else {
					err = a.Continue(ctx)
				}
				require.ErrorIs(t, err, cleanupErr)
				if cancelled {
					require.ErrorIs(t, err, cause)
					require.ErrorIs(t, err, context.Canceled)
				}
				var assistants []*Message
				var turns []TurnEnd
				var ends []AgentEnd
				for _, event := range events {
					switch e := event.(type) {
					case MessageEnd:
						if e.Message.Role == RoleAssistant {
							assistants = append(assistants, e.Message)
						}
					case TurnEnd:
						turns = append(turns, e)
					case AgentEnd:
						ends = append(ends, e)
					}
				}
				require.Len(t, turns, 1)
				require.Len(t, ends, 1)
				if reason == ai.StopReasonStop {
					require.Len(t, assistants, 2, "a successful reply must not hide a later cleanup failure")
					require.Contains(t, assistants[1].ErrorMessage, cleanupErr.Error())
				} else {
					require.Len(t, assistants, 1, "a published failure must not be synthesized again")
					require.Same(t, assistants[0], turns[0].Message)
					require.Equal(t, []*Message{assistants[0]}, ends[0].Messages)
				}
				var retained int
				for _, message := range a.Messages() {
					if message.Role == RoleAssistant {
						retained++
					}
				}
				require.Equal(t, len(assistants), retained)
				message := assistants[0]
				require.Equal(t, reason, message.StopReason)
				require.Equal(t, "partial response", message.Content[0].Text)
				require.Equal(t, 7, message.Usage.Input)
				require.Equal(t, 3, message.Usage.Output)
				require.NotNil(t, message.DurationMs)
				require.Equal(t, model.ID, message.Model)
				require.ErrorIs(t, a.State().Error, cleanupErr)
				require.False(t, a.State().IsStreaming)
			})
		}
	}
}

func TestFailedRunRetainsCancellationAndSettlement(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		for _, stage := range []string{"startup", "conversion", "stream"} {
			for _, outcome := range []string{"callback error", "context error", "cause", "deadline"} {
				t.Run(entry+"/"+stage+"/"+outcome, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						cause := errors.New("run owner stopped request")
						failure := errors.New("callback cleanup failed")
						ctx, cancel := context.WithCancelCause(t.Context())
						defer cancel(nil)
						if outcome == "deadline" {
							var stop context.CancelFunc
							ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
							defer stop()
						}
						fail := func(runCtx context.Context) error {
							if outcome == "deadline" {
								<-runCtx.Done()
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
						var events []Event
						var settled []bool
						config := Config{
							Model: &ai.Model{ID: "local"}, InitialMessages: []*Message{NewUserMessage("history")},
							StreamFn: func(runCtx context.Context, _ *ai.Model, _ *ai.Context, _ ai.EventHandler, _ ...ai.Option) error {
								if stage != "stream" {
									t.Error("cancelled callback admitted provider")
								}
								return fail(runCtx)
							},
							EventDispatcher: func(_ context.Context, name string, payload []byte, _ string) EventDispatchResult {
								if name == "agent_settled" {
									var value struct{ Aborted bool }
									require.NoError(t, json.Unmarshal(payload, &value))
									settled = append(settled, value.Aborted)
								}
								return EventDispatchResult{}
							},
						}
						if stage == "startup" {
							config.GetStartupMessages = func(runCtx context.Context) ([]*Message, error) { return nil, fail(runCtx) }
						}
						if stage == "conversion" {
							config.ConvertToLlm = func(runCtx context.Context, _ []*Message) ([]ai.Message, error) { return nil, fail(runCtx) }
						}
						a, err := New(config)
						require.NoError(t, err)
						a.Subscribe(func(event Event, _ <-chan struct{}) { events = append(events, event) })
						if entry == "prompt" {
							err = a.Prompt(ctx, NewUserMessage("hello"))
						} else {
							err = a.Continue(ctx)
						}
						assert.ErrorIs(t, err, cause)
						assert.ErrorIs(t, err, ctx.Err())
						if outcome == "callback error" || outcome == "deadline" {
							assert.ErrorIs(t, err, failure)
						}
						assert.ErrorIs(t, a.State().Error, cause)
						assert.ErrorIs(t, a.State().Error, ctx.Err())
						assert.Equal(t, []bool{true}, settled)
						var failures []*Message
						for _, event := range events {
							if end, ok := event.(MessageEnd); ok && end.Message.Role == RoleAssistant {
								failures = append(failures, end.Message)
							}
						}
						require.Len(t, failures, 1)
						assert.Equal(t, ai.StopReasonAborted, failures[0].StopReason)
						assert.Contains(t, failures[0].ErrorMessage, cause.Error())
						assert.Same(t, failures[0], events[len(events)-2].(TurnEnd).Message)
						assert.Equal(t, []*Message{failures[0]}, events[len(events)-1].(AgentEnd).Messages)
						assert.False(t, a.State().IsStreaming)
					})
				})
			}
		}
	}
}

func TestNilErrorTransformCancellationStopsComposedPreparation(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	cause := errors.New("prompt transform withdrew request")
	var calls []string
	a, err := New(Config{
		Model: &ai.Model{ID: "local"},
		TransformContext: func(_ context.Context, messages []*Message) ([]*Message, error) {
			calls = append(calls, "secondary")
			return messages, nil
		},
		ConvertToLlm: func(_ context.Context, messages []*Message) ([]ai.Message, error) {
			calls = append(calls, "conversion")
			return ConvertMessagesToAI(messages), nil
		},
		StreamFn: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
			calls = append(calls, "stream")
			handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
			return nil
		},
	})
	require.NoError(t, err)
	var events []Event
	a.Subscribe(func(event Event, _ <-chan struct{}) { events = append(events, event) })
	err = a.Prompt(ctx, NewUserMessage("hello"), WithPromptContext(func(_ context.Context, messages []*Message) ([]*Message, error) {
		calls = append(calls, "primary")
		cancel(cause)
		return messages, nil
	}))
	require.ErrorIs(t, err, cause)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, []string{"primary"}, calls)
	state := a.State()
	require.ErrorIs(t, state.Error, cause)
	require.False(t, state.IsStreaming)
	require.Len(t, state.Messages, 2)
	failure := state.Messages[1]
	require.Equal(t, ai.StopReasonAborted, failure.StopReason)
	require.Same(t, failure, events[len(events)-2].(TurnEnd).Message)
	require.Equal(t, []*Message{failure}, events[len(events)-1].(AgentEnd).Messages)
}

func TestStartupCancellationSettlesWithoutAdmittingReturnedMessages(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		for _, stage := range []string{"supplier", "before_agent_start", "input"} {
			t.Run(entry+"/"+stage, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				cause := errors.New("startup owner cancelled")
				var calls []string
				config := Config{
					Model: &ai.Model{ID: "local"}, InitialMessages: []*Message{NewUserMessage("history")},
					GetStartupMessages: func(context.Context) ([]*Message, error) {
						calls = append(calls, "supplier")
						if stage == "supplier" {
							cancel(cause)
						}
						return []*Message{NewUserMessage("pending")}, nil
					},
					EventDispatcher: func(_ context.Context, name string, _ []byte, _ string) EventDispatchResult {
						if name == "before_agent_start" || name == "input" {
							calls = append(calls, name)
							if name == stage {
								cancel(cause)
							}
						}
						if name == "input" {
							return EventDispatchResult{Blocked: true}
						}
						return EventDispatchResult{}
					},
					StreamFn: func(context.Context, *ai.Model, *ai.Context, ai.EventHandler, ...ai.Option) error {
						t.Error("cancelled startup admitted provider")
						return nil
					},
				}
				a, err := New(config)
				require.NoError(t, err)
				var events []Event
				a.Subscribe(func(event Event, _ <-chan struct{}) { events = append(events, event) })
				if entry == "prompt" {
					err = a.Prompt(ctx, NewUserMessage("hello"))
				} else {
					err = a.Continue(ctx)
				}
				require.ErrorIs(t, err, context.Canceled)
				require.ErrorIs(t, err, cause)
				want := []string{"supplier"}
				if stage != "supplier" {
					want = append(want, "before_agent_start")
				}
				if stage == "input" {
					want = append(want, "input")
				}
				require.Equal(t, want, calls)
				state := a.State()
				require.False(t, state.IsStreaming)
				require.ErrorIs(t, state.Error, cause)
				require.Len(t, state.Messages, 2)
				failure := state.Messages[1]
				require.Equal(t, ai.StopReasonAborted, failure.StopReason)
				require.True(t, failure.IsError)
				require.Same(t, failure, events[len(events)-4].(MessageStart).Message)
				require.Same(t, failure, events[len(events)-3].(MessageEnd).Message)
				require.Same(t, failure, events[len(events)-2].(TurnEnd).Message)
				require.Equal(t, []*Message{failure}, events[len(events)-1].(AgentEnd).Messages)
				var ends int
				for _, event := range events {
					if _, ok := event.(AgentEnd); ok {
						ends++
					}
				}
				require.Equal(t, 1, ends)
			})
		}
	}
}

func TestCancelledStatefulQueueSuppliersRetainMessages(t *testing.T) {
	for _, queue := range []string{"steering", "follow-up"} {
		t.Run(queue, func(t *testing.T) {
			a, err := New(Config{Model: &ai.Model{ID: "local"}})
			require.NoError(t, err)
			queued := NewUserMessage("retained")
			a.Steer(queued)
			a.FollowUp(queued)
			config := a.createLoopConfig(false, nil, "")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			var messages []*Message
			if queue == "steering" {
				messages, err = config.GetSteeringMessages(ctx)
			} else {
				messages, err = config.GetFollowUpMessages(ctx)
			}
			require.ErrorIs(t, err, context.Canceled)
			require.Empty(t, messages)
			require.Equal(t, []*Message{queued}, a.steeringQueue)
			require.Equal(t, []*Message{queued}, a.followUpQueue)
		})
	}
}

func TestCancelledContinueRetainsQueuedAssistantContinuation(t *testing.T) {
	for _, queue := range []string{"steering", "follow-up"} {
		t.Run(queue, func(t *testing.T) {
			original := &Message{Role: RoleAssistant, StopReason: ai.StopReasonStop}
			a, err := New(Config{Model: &ai.Model{ID: "local"}, InitialMessages: []*Message{NewUserMessage("history"), original}})
			require.NoError(t, err)
			queued := NewUserMessage("retained")
			if queue == "steering" {
				a.Steer(queued)
			} else {
				a.FollowUp(queued)
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			cause := errors.New("continuation withdrawn")
			cancel(cause)
			err = a.Continue(ctx)
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorIs(t, err, cause)
			if queue == "steering" {
				require.Equal(t, []*Message{queued}, a.steeringQueue)
			} else {
				require.Equal(t, []*Message{queued}, a.followUpQueue)
			}
			require.Len(t, a.Messages(), 2)
			require.Same(t, original, a.Messages()[1])
			require.False(t, a.State().IsStreaming)
		})
	}
}

func TestTurnCancellationRetainsStatefulQueues(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		t.Run(entry, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("turn cancelled before queue poll")
			var a *Agent
			var streams int
			queued := NewUserMessage("queued for later")
			var err error
			a, err = New(Config{
				Model: &ai.Model{ID: "local"}, InitialMessages: []*Message{NewUserMessage("history")},
				StreamFn: func(callCtx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
					streams++
					handler(callCtx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
					return nil
				},
				FinishTurn: func(context.Context, AgentTurnContext) (TurnDecision, error) {
					a.Steer(queued)
					a.FollowUp(queued)
					cancel(cause)
					return "", nil
				},
			})
			require.NoError(t, err)
			var events []Event
			a.Subscribe(func(event Event, _ <-chan struct{}) { events = append(events, event) })
			if entry == "prompt" {
				err = a.Prompt(ctx, NewUserMessage("hello"))
			} else {
				err = a.Continue(ctx)
			}
			require.ErrorIs(t, err, cause)
			require.ErrorIs(t, err, context.Canceled)
			require.Equal(t, 1, streams)
			require.Equal(t, []*Message{queued}, a.steeringQueue)
			require.Equal(t, []*Message{queued}, a.followUpQueue)
			require.False(t, a.State().IsStreaming)
			require.ErrorIs(t, a.State().Error, cause)
			var ends, aborted int
			for _, event := range events {
				switch e := event.(type) {
				case AgentEnd:
					ends++
				case MessageEnd:
					require.NotSame(t, queued, e.Message)
					if e.Message.StopReason == ai.StopReasonAborted {
						aborted++
					}
				}
			}
			require.Equal(t, 1, ends)
			require.Equal(t, 1, aborted)
		})
	}
}

func TestNextTurnCancellationRejectsPreparedMessages(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		t.Run(entry, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("next turn preparation cancelled")
			returned := NewUserMessage("must not enter history")
			var streams int
			a, err := New(Config{
				Model: &ai.Model{ID: "local"}, InitialMessages: []*Message{NewUserMessage("history")},
				StreamFn: func(callCtx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
					streams++
					handler(callCtx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
					return nil
				},
				FinishTurn: func(context.Context, AgentTurnContext) (TurnDecision, error) { return TurnContinue, nil },
				PrepareNextTurn: func(callCtx context.Context, _ AgentTurnContext) (*AgentTurnUpdate, error) {
					cancel(cause)
					<-callCtx.Done()
					return &AgentTurnUpdate{Messages: []*Message{returned}}, nil
				},
			})
			require.NoError(t, err)
			var events []Event
			a.Subscribe(func(event Event, _ <-chan struct{}) { events = append(events, event) })
			if entry == "prompt" {
				err = a.Prompt(ctx, NewUserMessage("hello"))
			} else {
				err = a.Continue(ctx)
			}
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorIs(t, err, cause)
			require.Equal(t, 1, streams)
			require.False(t, a.State().IsStreaming)
			require.ErrorIs(t, a.State().Error, cause)
			for _, message := range a.Messages() {
				require.NotSame(t, returned, message)
			}
			var ends, starts, aborted int
			for _, event := range events {
				switch e := event.(type) {
				case TurnStart:
					starts++
				case AgentEnd:
					ends++
				case MessageEnd:
					require.NotSame(t, returned, e.Message)
					if e.Message.StopReason == ai.StopReasonAborted {
						aborted++
					}
				}
			}
			require.Equal(t, 1, starts)
			require.Equal(t, 1, ends)
			require.Equal(t, 1, aborted)
		})
	}
}

func TestBeforeSettleCancellationRejectsMessages(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		t.Run(entry, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("settlement cancelled")
			var streams int
			a, err := New(Config{
				Model: &ai.Model{ID: "local"}, InitialMessages: []*Message{NewUserMessage("history")},
				StreamFn: func(callCtx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
					streams++
					handler(callCtx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
					return nil
				},
				EventDispatcher: func(callCtx context.Context, name string, _ []byte, _ string) EventDispatchResult {
					if name != "agent_before_settle" {
						return EventDispatchResult{}
					}
					cancel(cause)
					<-callCtx.Done()
					return EventDispatchResult{Responses: []EventDispatchResponse{{ResponseData: []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"must not enter history"}]}]}`)}}}
				},
			})
			require.NoError(t, err)
			var events []Event
			a.Subscribe(func(event Event, _ <-chan struct{}) { events = append(events, event) })
			if entry == "prompt" {
				err = a.Prompt(ctx, NewUserMessage("hello"))
			} else {
				err = a.Continue(ctx)
			}
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorIs(t, err, cause)
			require.Equal(t, 1, streams)
			require.False(t, a.State().IsStreaming)
			require.ErrorIs(t, a.State().Error, cause)
			for _, message := range a.Messages() {
				require.NotEqual(t, "must not enter history", agentInputText(message))
			}
			var ends, starts, aborted int
			for _, event := range events {
				switch e := event.(type) {
				case TurnStart:
					starts++
				case AgentEnd:
					ends++
				case MessageEnd:
					require.NotEqual(t, "must not enter history", agentInputText(e.Message))
					if e.Message.StopReason == ai.StopReasonAborted {
						aborted++
					}
				}
			}
			require.Equal(t, 1, starts)
			require.Equal(t, 1, ends)
			require.Equal(t, 1, aborted)
		})
	}
}

func TestPostResponseCancellationRetainsAssistantWithoutToolResults(t *testing.T) {
	for _, entry := range []string{"prompt", "continue"} {
		for _, stage := range []string{"after_provider_response", "tool_call", "user_bash"} {
			t.Run(entry+"/"+stage, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				cause := errors.New("post-response cancelled")
				var executions int
				a, err := New(Config{
					Model: &ai.Model{ID: "local"}, InitialMessages: []*Message{NewUserMessage("history")},
					Tools: []Tool{{Name: "bash", Parameters: map[string]any{}, Execute: func(ToolContext) (*ToolResult, error) { executions++; return &ToolResult{Content: "executed"}, nil }}},
					StreamFn: func(callCtx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
						handler(callCtx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonToolUse, Content: []ai.ContentBlock{{Type: ai.ContentBlockToolCall, ID: "bash-1", Name: "bash", Arguments: []byte(`{}`)}}}})
						return nil
					},
					EventDispatcher: func(callCtx context.Context, name string, _ []byte, _ string) EventDispatchResult {
						if name != stage {
							return EventDispatchResult{}
						}
						cancel(cause)
						<-callCtx.Done()
						return EventDispatchResult{Blocked: true, Responses: []EventDispatchResponse{{ResponseData: []byte(`{"result":{"output":"rejected override","exitCode":0}}`)}}}
					},
				})
				require.NoError(t, err)
				var events []Event
				a.Subscribe(func(event Event, _ <-chan struct{}) { events = append(events, event) })
				if entry == "prompt" {
					err = a.Prompt(ctx, NewUserMessage("hello"))
				} else {
					err = a.Continue(ctx)
				}
				require.ErrorIs(t, err, context.Canceled)
				require.ErrorIs(t, err, cause)
				require.Equal(t, 0, executions)
				require.False(t, a.State().IsStreaming)
				require.ErrorIs(t, a.State().Error, cause)
				for _, message := range a.Messages() {
					require.NotEqual(t, RoleTool, message.Role)
				}
				var assistants []*Message
				var ends int
				for _, event := range events {
					switch e := event.(type) {
					case ToolStart, ToolEnd:
						t.Fatalf("unexpected tool lifecycle: %T", event)
					case MessageEnd:
						if e.Message.Role == RoleAssistant {
							assistants = append(assistants, e.Message)
						}
					case AgentEnd:
						ends++
					}
				}
				require.Len(t, assistants, 2)
				require.Equal(t, ai.StopReasonToolUse, assistants[0].StopReason)
				require.Equal(t, ai.StopReasonAborted, assistants[1].StopReason)
				require.Equal(t, 1, ends)
			})
		}
	}
}

func TestListenerPanicFailsRunAndSettles(t *testing.T) {
	cases := []struct {
		name      string
		panicsOn  func(Event) bool
		agentEnds int
	}{
		{"assistant message end", func(e Event) bool {
			end, ok := e.(MessageEnd)
			return ok && end.Message.Role == RoleAssistant
		}, 1},
		{"agent end", func(e Event) bool { _, ok := e.(AgentEnd); return ok }, 2},
		{"every event", func(Event) bool { return true }, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := New(Config{Model: &ai.Model{ID: "m", API: "local", Provider: "local"}, StreamFn: func(ctx context.Context, _ *ai.Model, _ *ai.Context, handler ai.EventHandler, _ ...ai.Option) error {
				handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}}}})
				return nil
			}})
			require.NoError(t, err)
			var (
				panicked  bool
				agentEnds int
				later     []Event
			)
			a.Subscribe(func(event Event, _ <-chan struct{}) {
				if _, ok := event.(AgentEnd); ok {
					agentEnds++
				}
				if tc.panicsOn(event) && (!panicked || tc.name == "every event") {
					panicked = true
					panic("listener broke")
				}
			})
			a.Subscribe(func(event Event, _ <-chan struct{}) { later = append(later, event) })

			err = a.Prompt(t.Context(), NewUserMessage("prompt"))
			require.ErrorIs(t, err, ErrListenerPanic)
			assert.ErrorContains(t, err, "listener broke")
			assert.NotErrorIs(t, err, context.Canceled)
			a.WaitForIdle()
			state := a.State()
			assert.False(t, state.IsStreaming)
			assert.ErrorIs(t, state.Error, ErrListenerPanic)
			assert.Equal(t, tc.agentEnds, agentEnds)
			last := state.Messages[len(state.Messages)-1]
			assert.Equal(t, RoleAssistant, last.Role)
			assert.Equal(t, ai.StopReasonError, last.StopReason)
			assert.Contains(t, last.ErrorMessage, "listener broke")
			// A panic stops that event's delivery to later listeners.
			if tc.name == "every event" {
				assert.Empty(t, later)
			} else {
				assert.NotEmpty(t, later)
			}

			// The agent stays usable for a later run.
			panicked, agentEnds = true, 0
			if tc.name != "every event" {
				require.NoError(t, a.Prompt(t.Context(), NewUserMessage("again")))
				assert.Equal(t, 1, agentEnds)
			}
		})
	}
}

func TestListenerPanicOnIdleAppendReturnsError(t *testing.T) {
	a, err := New(Config{Model: &ai.Model{ID: "m"}})
	require.NoError(t, err)
	a.Subscribe(func(Event, <-chan struct{}) { panic(errors.New("listener broke")) })
	err = a.AppendCustomMessage(t.Context(), &Message{Role: RoleCustom, CustomType: "note"})
	require.ErrorIs(t, err, ErrListenerPanic)
	assert.ErrorContains(t, err, "listener broke")
	assert.Len(t, a.State().Messages, 1)
}

func TestProviderEnvironmentFailureRejectsRequest(t *testing.T) {
	cause := errors.New("scoped credential rejected")
	requests := 0
	a, err := New(Config{
		Model:       &ai.Model{ID: "test", Provider: "test"},
		ProviderEnv: func(context.Context, string) (map[string]string, error) { return nil, cause },
		ClientFactory: func(*ai.Model) (ai.Client, error) {
			return stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
				requests++
				handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
				return nil
			}}, nil
		},
	})
	require.NoError(t, err)
	err = a.Prompt(t.Context(), NewUserMessage("hello"))
	require.ErrorIs(t, err, cause)
	require.Zero(t, requests)
}

func TestRequestCredentialsOverrideSeparateLookups(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fails), func(t *testing.T) {
			cause := errors.New("snapshot unavailable")
			calls, requests := 0, 0
			a, err := New(Config{
				Model:          &ai.Model{ID: "test", Provider: "test"},
				APIKeyProvider: func(context.Context, string) (string, error) { t.Error("separate key lookup"); return "", cause },
				ProviderEnv: func(context.Context, string) (map[string]string, error) {
					t.Error("separate environment lookup")
					return nil, cause
				},
				RequestCredentials: func(ctx context.Context, provider string) (string, map[string]string, error) {
					calls++
					require.Equal(t, "test", provider)
					if fails {
						return "", nil, cause
					}
					return "snapshot", map[string]string{"VERSION": "snapshot"}, nil
				},
				ClientFactory: func(*ai.Model) (ai.Client, error) {
					return stubClient{stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
						requests++
						actual := ai.ExtractSimpleStreamOptions(opts)
						require.Equal(t, "snapshot", actual.APIKey)
						require.Equal(t, "snapshot", actual.Env["VERSION"])
						handler(ctx, ai.Done{Message: ai.AssistantMessage{Role: "assistant", StopReason: ai.StopReasonStop}})
						return nil
					}}, nil
				},
			})
			require.NoError(t, err)
			err = a.Prompt(t.Context(), NewUserMessage("hello"))
			require.Equal(t, 1, calls)
			if fails {
				require.ErrorIs(t, err, cause)
				require.Zero(t, requests)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, requests)
			}
		})
	}
}

func TestRequestCredentialsRetainCancellationCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	cause := errors.New("credential resolution cancelled")
	calls := 0
	a, err := New(Config{
		Model: &ai.Model{ID: "test", Provider: "test"},
		RequestCredentials: func(context.Context, string) (string, map[string]string, error) {
			cancel(cause)
			return "key", nil, nil
		},
		ClientFactory: func(*ai.Model) (ai.Client, error) {
			return stubClient{stream: func(context.Context, *ai.Model, *ai.Context, ai.EventHandler, ...ai.Option) error {
				calls++
				return nil
			}}, nil
		},
	})
	require.NoError(t, err)
	err = a.Prompt(ctx, NewUserMessage("hello"))
	require.ErrorIs(t, err, cause)
	require.Zero(t, calls)
}
