package agent

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/losinggeneration/tau/ai"
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
	if len(prompts[1]) != 4 {
		t.Fatalf("len(prompts[1]) = %d, want assistant tool-use, tool result, and steering before continuation", len(prompts[1]))
	}
	lastContent, ok := prompts[1][3].Content.([]ai.ContentBlock)
	if !ok {
		t.Fatalf("steering content type = %T, want []ai.ContentBlock", prompts[1][3].Content)
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

// TestBuggyCallbackDoesNotAbortRun verifies that the safe-callback wrappers
// prevent a buggy user-supplied
// callback from aborting the agent loop. A faulty transform that returns
// an error should be silently swallowed and the run should complete
// successfully.
func TestBuggyCallbackDoesNotAbortRun(t *testing.T) {
	client := stubClient{
		stream: func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
			final := ai.AssistantMessage{
				Role:       "assistant",
				Timestamp:  time.Now().UnixMilli(),
				Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "ok"}},
				StopReason: ai.StopReasonStop,
			}
			handler(ctx, ai.Done{Reason: ai.StopReasonStop, Message: final})
			return nil
		},
	}

	a, err := New(Config{
		Model:           &ai.Model{ID: "test", Provider: ai.ProviderOpenAI},
		ClientFactory:   func(model *ai.Model) (ai.Client, error) { return client, nil },
		InitialMessages: []*Message{NewUserMessage("hi")},
		ConvertToLlm: func(ctx context.Context, msgs []*Message) ([]ai.Message, error) {
			// Buggy callback that always errors. Should be wrapped by
			// safeConvertToLlm and return an empty list, not abort.
			return nil, errBuggyCallback
		},
		TransformContext: func(ctx context.Context, msgs []*Message) ([]*Message, error) {
			return nil, errBuggyCallback
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Abort()

	if err := a.Prompt(context.Background(), NewUserMessage("hi")); err != nil {
		t.Fatalf("Prompt: %v (callback should not abort the run)", err)
	}
	state := a.State()
	if state.ErrorMessage != "" {
		t.Fatalf("State.ErrorMessage = %q, want empty (callback error should not poison state)", state.ErrorMessage)
	}
}

var errBuggyCallback = errors.New("buggy callback intentionally errors")

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
