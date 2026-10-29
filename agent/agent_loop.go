package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/losinggeneration/tau/ai"
	"github.com/xeipuuv/gojsonschema"
)

func buildAIOptions(config AgentLoopConfig, model *ai.Model) []ai.Option {
	opts := slices.Clone(config.RequestOptions)
	if config.SessionID != "" {
		opts = append(opts, ai.WithSessionID(config.SessionID))
	}

	if config.ThinkingLevel != "" {
		opts = append(opts, ai.WithReasoning(ai.ReasoningLevel(config.ThinkingLevel)))
	}

	if config.ThinkingBudgets != nil {
		opts = append(opts, ai.WithThinkingBudgets(config.ThinkingBudgets))
	}

	if config.Transport != "" {
		opts = append(opts, ai.WithTransport(config.Transport))
	}
	if config.MaxRetryDelayMs > 0 {
		opts = append(opts, ai.WithMaxRetryDelayMs(config.MaxRetryDelayMs))
	}
	if config.OnPayload != nil {
		opts = append(opts, ai.WithPayloadCallback(config.OnPayload))
	}
	if config.OnResponse != nil {
		opts = append(opts, ai.WithResponseCallback(config.OnResponse))
	}

	return opts
}

// EventSink is a callback for agent events.
type EventSink func(event Event)

type loopStartMode int

const (
	loopStartWithPrompts loopStartMode = iota
	loopStartContinue
)

type loopInvocation struct {
	currentCtx   *AgentContext
	newMessages  []*Message
	systemPrompt string
	opts         []ai.Option
}

type streamFuncClient struct {
	streamFn StreamFunc
}

func (c streamFuncClient) Stream(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error {
	return c.streamFn(ctx, model, prompt, handler, opts...)
}

func (c streamFuncClient) Complete(ctx context.Context, model *ai.Model, prompt *ai.Context, opts ...ai.Option) (*ai.AssistantMessage, error) {
	var result *ai.AssistantMessage
	var mu sync.Mutex
	err := c.streamFn(ctx, model, prompt, func(_ context.Context, event ai.StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		if result != nil {
			return
		}
		switch e := event.(type) {
		case ai.Done:
			msg := e.Message
			result = &msg
		case ai.StreamError:
			msg := e.Msg
			result = &msg
		}
	}, opts...)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("stream function did not produce a terminal assistant message")
	}

	return result, nil
}

// RunAgentLoop starts an agent loop with a new prompt message.
// The prompt is added to the context and events are emitted for it.
func RunAgentLoop(
	ctx context.Context,
	prompts []*Message,
	agentCtx *AgentContext,
	config AgentLoopConfig,
	emit EventSink,
) ([]*Message, error) {
	invocation, err := prepareLoopInvocation(ctx, agentCtx, config, loopStartWithPrompts, prompts)
	if err != nil {
		return nil, err
	}

	return executeLoopInvocation(ctx, invocation, config, emit)
}

// RunAgentLoopContinue continues an agent loop from the current context
// without adding a new message. Used for retries where the context
// already has user message or tool results.
func RunAgentLoopContinue(
	ctx context.Context,
	agentCtx *AgentContext,
	config AgentLoopConfig,
	emit EventSink,
) ([]*Message, error) {
	invocation, err := prepareLoopInvocation(ctx, agentCtx, config, loopStartContinue, nil)
	if err != nil {
		return nil, err
	}

	return executeLoopInvocation(ctx, invocation, config, emit)
}

func prepareLoopInvocation(
	ctx context.Context,
	agentCtx *AgentContext,
	config AgentLoopConfig,
	mode loopStartMode,
	prompts []*Message,
) (*loopInvocation, error) {
	currentCtx, newMessages, err := prepareLoopContext(agentCtx, mode, prompts)
	if err != nil {
		return nil, err
	}

	model := config.Model
	if model == nil {
		model = currentCtx.Model
	}

	systemPrompt := config.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = currentCtx.SystemPrompt
	}
	// Append system addendum (e.g., skill content)
	if config.SystemAddendum != "" {
		if systemPrompt != "" {
			systemPrompt = systemPrompt + "\n\n" + config.SystemAddendum
		} else {
			systemPrompt = config.SystemAddendum
		}
	}

	return &loopInvocation{
		currentCtx:   currentCtx,
		newMessages:  newMessages,
		systemPrompt: systemPrompt,
		opts:         buildAIOptions(config, model),
	}, nil
}

func prepareLoopContext(agentCtx *AgentContext, mode loopStartMode, prompts []*Message) (*AgentContext, []*Message, error) {
	switch mode {
	case loopStartWithPrompts:
		newMessages := append([]*Message(nil), prompts...)
		return &AgentContext{
			Model:           agentCtx.Model,
			Tools:           agentCtx.Tools,
			SystemPrompt:    agentCtx.SystemPrompt,
			Messages:        append(append([]*Message(nil), agentCtx.Messages...), prompts...),
			MessagesVersion: agentCtx.MessagesVersion,
			SessionCWD:      agentCtx.SessionCWD,
			Capabilities:    agentCtx.Capabilities,
		}, newMessages, nil
	case loopStartContinue:
		messages := slices.DeleteFunc(slices.Clone(agentCtx.Messages), func(message *Message) bool { return message == nil })
		snapshot := *agentCtx
		snapshot.Messages = messages
		agentCtx = &snapshot
		if len(agentCtx.Messages) == 0 || !slices.ContainsFunc(agentCtx.Messages, func(message *Message) bool {
			return message != nil && message.Role != RoleSystem
		}) {
			return nil, nil, ErrCannotContinueNoMessages
		}

		lastMsg := agentCtx.Messages[len(agentCtx.Messages)-1]
		if lastMsg.Role == RoleAssistant {
			return nil, nil, ErrCannotContinueFromAssistant
		}

		return agentCtx, nil, nil
	default:
		return nil, nil, fmt.Errorf("unknown loop start mode: %d", mode)
	}
}

func agentInputText(message *Message) string {
	var text strings.Builder
	for _, block := range message.Content {
		if block.Type == ai.ContentBlockText {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

func executeLoopInvocation(
	ctx context.Context,
	invocation *loopInvocation,
	config AgentLoopConfig,
	emit EventSink,
) (messages []*Message, runErr error) {
	defer func() {
		// Stateful runs join cancellation at their lifecycle boundary.
		if runErr != nil && !config.deferFailureEnd {
			if err := ctx.Err(); err != nil {
				runErr = errors.Join(runErr, err, context.Cause(ctx))
			}
		}
	}()

	emit = withPluginEventDispatch(ctx, config, emit)
	dispatchEvent := func(name string, payload []byte) EventDispatchResult {
		if config.EventDispatcher != nil {
			return config.EventDispatcher(ctx, name, payload, config.SessionID)
		}
		return EventDispatchResult{}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var startupMessages []*Message
	if config.GetStartupMessages != nil {
		pending, err := config.GetStartupMessages(ctx)
		if err != nil {
			return nil, fmt.Errorf("prepare startup messages: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		startupMessages = append(startupMessages, pending...)
	}
	// Startup prompt overrides affect requests only. System-aware context hooks
	// retain authority over the projected prompt and declarations.
	promptContext := ai.NormalizeContext(&ai.Context{SystemPrompt: invocation.systemPrompt, Messages: ConvertMessagesToAI(invocation.currentCtx.Messages)})
	startup := map[string]any{"systemPrompt": ai.CurrentSystemPrompt(promptContext.Messages), "messageType": "user"}
	if len(invocation.newMessages) > 0 {
		startup["prompt"] = agentInputText(invocation.newMessages[0])
		if invocation.newMessages[0].Role == RoleCustom || invocation.newMessages[0].Data != nil {
			delete(startup, "prompt")
			startup["messageType"] = "custom"
			startup["message"] = invocation.newMessages[0]
		}
	}
	if beforePayload, err := json.Marshal(startup); err == nil {
		result := dispatchEvent("before_agent_start", beforePayload)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, resp := range result.Responses {
			var data struct {
				SystemPrompt *string    `json:"systemPrompt"`
				Messages     []*Message `json:"messages"`
			}
			if len(resp.ResponseData) == 0 {
				continue
			}
			if err := json.Unmarshal(resp.ResponseData, &data); err != nil {
				continue
			}
			if data.SystemPrompt != nil {
				config.forcedSystemPrompt = data.SystemPrompt
			}
			if len(data.Messages) > 0 {
				startupMessages = append(startupMessages, data.Messages...)
			}
		}
	} else {
		return nil, fmt.Errorf("marshal startup hook: %w", err)
	}
	invocation.newMessages = append(invocation.newMessages, startupMessages...)
	invocation.currentCtx.Messages = append(invocation.currentCtx.Messages, startupMessages...)
	emit(AgentStart{})
	dispatchEvent("agent_start", nil)
	emit(TurnStart{})
	dispatchEvent("turn_start", nil)

	var admitted []*Message
	for _, prompt := range invocation.newMessages {
		if prompt.Role == RoleCustom || prompt.Data != nil {
			admitted = append(admitted, prompt)
			emit(MessageStart{Message: prompt})
			emit(MessageEnd{Message: prompt})
			continue
		}
		inputPayload, err := json.Marshal(map[string]any{"text": agentInputText(prompt), "source": "user"})
		if err != nil {
			return nil, fmt.Errorf("marshal input: %w", err)
		}
		response := dispatchEvent("input", inputPayload)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		handled := response.Blocked
		for _, item := range response.Responses {
			if len(item.ResponseData) == 0 {
				continue
			}
			var result struct {
				Action string            `json:"action"`
				Text   string            `json:"text"`
				Images []ai.ContentBlock `json:"images"`
			}
			if err := json.Unmarshal(item.ResponseData, &result); err != nil {
				return nil, fmt.Errorf("parse input hook: %w", err)
			}
			switch result.Action {
			case "handled":
				handled = true
			case "transform":
				prompt.Content = append([]ai.ContentBlock{{Type: ai.ContentBlockText, Text: result.Text}}, result.Images...)
			}
		}
		if handled {
			invocation.currentCtx.Messages = slices.DeleteFunc(invocation.currentCtx.Messages, func(message *Message) bool { return message == prompt })
			continue
		}
		admitted = append(admitted, prompt)
		emit(MessageStart{Message: prompt})
		emit(MessageEnd{Message: prompt})
	}
	if len(invocation.newMessages) > 0 && len(admitted) == 0 {
		emit(AgentEnd{})
		dispatchEvent("agent_end", nil)
		return nil, nil
	}
	invocation.newMessages = admitted

	result, err := runLoop(
		ctx,
		invocation.currentCtx,
		invocation.newMessages,
		config,
		invocation.systemPrompt,
		invocation.opts,
		emit,
	)
	if err != nil {
		if !config.deferFailureEnd {
			emit(AgentEnd{Messages: result})
			dispatchEvent("agent_end", nil)
		}
		return result, err
	}

	emit(AgentEnd{Messages: result})
	dispatchEvent("agent_end", nil)
	return result, nil
}

func withPluginEventDispatch(ctx context.Context, config AgentLoopConfig, emit EventSink) EventSink {
	return func(event Event) {
		emit(event)
		if config.EventDispatcher == nil {
			return
		}
		name := pluginEventName(event)
		if name == "" {
			return
		}
		payload, err := json.Marshal(event)
		if err != nil {
			return
		}
		config.EventDispatcher(ctx, name, payload, config.SessionID)
	}
}

// eventPayloadMessage builds the JSON payload for message_user /
// message_assistant / message_system plugin events. The shape matches the
// plugin proto (snake_case fields) so plugin authors can consume the
// payload directly without remapping.
//
// Returns nil on marshal error; the caller passes nil to dispatchEvent
// which the EventDispatcher treats as "no payload" — never crashing the
// loop on a wire-format bug.
func eventPayloadMessage(msg *Message) []byte {
	if msg == nil {
		return nil
	}
	type contentBlock struct {
		Type      string          `json:"type"`
		Text      string          `json:"text,omitempty"`
		ID        string          `json:"id,omitempty"`
		Name      string          `json:"name,omitempty"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	type payload struct {
		DurationMs *float64 `json:"durationMs,omitempty"`
		Role       string   `json:"role"`
		Content    any      `json:"content"`
		ToolCallID string   `json:"tool_call_id,omitempty"`
		ToolName   string   `json:"tool_name,omitempty"`
		IsError    bool     `json:"is_error,omitempty"`
	}
	var content any
	if len(msg.Content) == 1 && msg.Content[0].Type == ai.ContentBlockText {
		content = msg.Content[0].Text
	} else {
		blocks := make([]contentBlock, 0, len(msg.Content))
		for _, b := range msg.Content {
			blocks = append(blocks, contentBlock{
				Type:      string(b.Type),
				Text:      b.Text,
				ID:        b.ID,
				Name:      b.Name,
				Arguments: b.Arguments,
			})
		}
		content = blocks
	}
	p := payload{
		DurationMs: msg.DurationMs,
		Role:       string(msg.Role),
		Content:    content,
		ToolCallID: msg.ToolCallID,
		ToolName:   msg.ToolName,
		IsError:    msg.IsError,
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return data
}

// eventPayloadModel builds the JSON payload for model_select events. The
// payload carries the model ID, provider, and API so plugins can record
// which model the current turn is using. Returns nil for nil input.
func eventPayloadModel(model *ai.Model) []byte {
	if model == nil {
		return nil
	}
	type payload struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
		API      string `json:"api"`
	}
	data, err := json.Marshal(payload{
		ID:       model.ID,
		Provider: model.Provider,
		API:      model.API,
	})
	if err != nil {
		return nil
	}
	return data
}

// eventPayloadToolCall builds the JSON payload for tool_call events. The
// tool-call ID, name, and raw arguments are forwarded verbatim so plugins
// that re-marshal the arguments (e.g., for UI rendering) don't lose any
// precision.
func eventPayloadToolCall(tc ai.ToolCall) []byte {
	type payload struct {
		ToolName   string          `json:"tool_name"`
		ToolCallID string          `json:"tool_call_id"`
		Arguments  json.RawMessage `json:"arguments,omitempty"`
	}
	data, err := json.Marshal(payload{
		ToolName:   tc.Name,
		ToolCallID: tc.ID,
		Arguments:  tc.Arguments,
	})
	if err != nil {
		return nil
	}
	return data
}

// eventPayloadToolResult builds the JSON payload for tool_result events.
// The result.Content is sent as a string; ContentBlocks are included only
// when non-empty so the wire shape stays clean for plain text results.
// toolCallID and toolName are extracted from the result message; raw
// arguments are not carried (plugins that need them re-subscribe to the
// matching tool_call event by ID).
func eventPayloadToolResult(result *ToolResult) []byte {
	if result == nil {
		return nil
	}
	var toolCallID, toolName string
	if result.Message != nil {
		toolCallID = result.Message.ToolCallID
		toolName = result.Message.ToolName
	}
	type payload struct {
		DurationMs    *float64          `json:"durationMs,omitempty"`
		ToolName      string            `json:"tool_name"`
		ToolCallID    string            `json:"tool_call_id"`
		Result        string            `json:"result"`
		ContentBlocks []ai.ContentBlock `json:"content_blocks,omitempty"`
		IsError       bool              `json:"is_error"`
	}
	p := payload{
		DurationMs: result.DurationMs,
		ToolName:   toolName,
		ToolCallID: toolCallID,
		Result:     result.Content,
		IsError:    result.IsError,
	}
	if len(result.ContentBlocks) > 0 {
		p.ContentBlocks = result.ContentBlocks
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return data
}

func pluginEventName(event Event) string {
	switch event.(type) {
	case MessageStart:
		return "message_start"
	case MessageUpdate:
		return "message_update"
	case MessageEnd:
		return "message_end"
	case ToolStart:
		return "tool_execution_start"
	case ToolUpdate:
		return "tool_execution_update"
	case ToolEnd:
		return "tool_execution_end"
	default:
		return ""
	}
}

// recordResponseModel names the model that answered when the provider left
// it out, so sessions and routers can tell which physical model replied.
func recordResponseModel(msg *Message, model *ai.Model) {
	if model == nil || msg.Provider != "" || msg.Model != "" {
		return
	}
	msg.API, msg.Provider, msg.Model = model.API, model.Provider, model.ID
}

func getLoopClient(model *ai.Model, config AgentLoopConfig) (ai.Client, error) {
	if model == nil {
		return nil, fmt.Errorf("model is required: no model configured in agent context or loop config")
	}
	if config.StreamFn != nil {
		return streamFuncClient{streamFn: config.StreamFn}, nil
	}
	if config.ClientFactory != nil {
		return config.ClientFactory(model)
	}

	return ai.GetClient(model)
}

// runLoop is the main loop logic shared by RunAgentLoop and RunAgentLoopContinue.
func runLoop(
	ctx context.Context,
	currentCtx *AgentContext,
	newMessages []*Message,
	config AgentLoopConfig,
	systemPrompt string,
	opts []ai.Option,
	emit EventSink,
) ([]*Message, error) {
	if newMessages == nil {
		newMessages = []*Message{}
	}

	dispatchEvent := func(name string, payload []byte) EventDispatchResult {
		if config.EventDispatcher != nil {
			return config.EventDispatcher(ctx, name, payload, config.SessionID)
		}
		return EventDispatchResult{}
	}

	if config.Model == nil {
		config.Model = currentCtx.Model
	}
	var (
		client      ai.Client
		clientModel *ai.Model
	)

	firstTurn := true
	msgVersion := currentCtx.MessagesVersion
	var explicitContinuation bool
	var lastTurn AgentTurnContext

	// Get pending messages from steering at start
	pendingMessages, err := getSteeringMessages(ctx, config)
	if err != nil {
		return newMessages, err
	}

	// Outer loop: continues when queued follow-up messages arrive after agent would stop
	for {
		hasMoreToolCalls := true

		// Inner loop: process tool calls and steering messages
		for hasMoreToolCalls || len(pendingMessages) > 0 {
			if !firstTurn {
				if config.PrepareNextTurn != nil {
					update, err := config.PrepareNextTurn(ctx, lastTurn)
					if err != nil {
						return newMessages, fmt.Errorf("prepare next turn: %w", err)
					}
					if err := ctx.Err(); err != nil {
						return newMessages, err
					}
					if update != nil {
						if update.Context != nil {
							currentCtx = update.Context
							systemPrompt = currentCtx.SystemPrompt
						}
						if update.Model != nil {
							config.Model = update.Model
						}
						if update.ThinkingLevel != nil {
							config.ThinkingLevel = *update.ThinkingLevel
						}
						opts = buildAIOptions(config, config.Model)
						pendingMessages = append(update.Messages, pendingMessages...)
					}
				}
				// Preparation can run long (compaction), so admit steering queued
				// meanwhile. Polling only when the turn-end poll came back empty
				// keeps one-at-a-time delivery from admitting two messages.
				if len(pendingMessages) == 0 {
					pendingMessages, err = getSteeringMessages(ctx, config)
					if err != nil {
						return newMessages, err
					}
				}
				emit(TurnStart{})
				dispatchEvent("turn_start", nil)
			} else {
				firstTurn = false
			}

			// A mid-run ReplaceMessages (e.g. /compact issued while this turn
			// is streaming) swaps the agent's transcript. Rebase the working
			// context onto the replacement so the next model call starts from
			// it instead of continuing from the stale pre-replacement snapshot.
			if config.RebasedMessages != nil {
				if msgs, version, ok := config.RebasedMessages(msgVersion); ok {
					currentCtx.Messages = msgs
					msgVersion = version
				}
			}

			// Process pending messages (inject before next assistant response)
			if len(pendingMessages) > 0 {
				for _, msg := range pendingMessages {
					emit(MessageStart{Message: msg})
					emit(MessageEnd{Message: msg})
					currentCtx.Messages = append(currentCtx.Messages, msg)
					newMessages = append(newMessages, msg)

					// Dispatch message events
					switch msg.Role {
					case RoleUser:
						dispatchEvent("message_user", eventPayloadMessage(msg))
					case RoleAssistant:
						dispatchEvent("message_assistant", eventPayloadMessage(msg))
					case RoleSystem:
						dispatchEvent("message_system", eventPayloadMessage(msg))
					}
				}
			}

			if err := ctx.Err(); err != nil {
				return newMessages, err
			}
			if config.PrepareRequest != nil {
				requestContext := *currentCtx
				requestContext.SystemPrompt = systemPrompt
				update, err := config.PrepareRequest(ctx, PrepareRequestContext{Context: &requestContext, Model: config.Model, ThinkingLevel: config.ThinkingLevel})
				if err != nil {
					return newMessages, fmt.Errorf("prepare request: %w", err)
				}
				if err := ctx.Err(); err != nil {
					return newMessages, err
				}
				if update != nil {
					if update.Context != nil {
						currentCtx = update.Context
						systemPrompt = currentCtx.SystemPrompt
					}
					if update.Model != nil {
						config.Model = update.Model
					}
					if update.ThinkingLevel != nil {
						config.ThinkingLevel = *update.ThinkingLevel
					}
					for _, message := range update.Messages {
						if message == nil {
							continue
						}
						emit(MessageStart{Message: message})
						emit(MessageEnd{Message: message})
						currentCtx.Messages = append(currentCtx.Messages, message)
						newMessages = append(newMessages, message)
						if message.Role == RoleSystem {
							dispatchEvent("message_system", eventPayloadMessage(message))
						}
					}
					opts = buildAIOptions(config, config.Model)
				}
			}
			if err := context.Cause(ctx); err != nil {
				return newMessages, err
			}
			model := config.Model
			if client == nil || clientModel != model {
				client, err = getLoopClient(model, config)
				if err != nil {
					return newMessages, fmt.Errorf("create client: %w", err)
				}
				clientModel = model
			}
			if err := ctx.Err(); err != nil {
				return newMessages, err
			}
			// Fire the `model_select` event so plugins can observe which
			// model is driving the current turn.
			dispatchEvent("model_select", eventPayloadModel(model))

			// Fire `before_provider_request` so plugins can observe or
			// transform the request. This is informational; the block verdict
			// is not yet honored. The on-payload callback (ai.WithPayloadCallback)
			// is the canonical transformation seam.
			dispatchEvent("before_provider_request", eventPayloadModel(model))

			// Stream assistant response. A usage-limit failure (429 carrying a
			// reset time) is surfaced in the transcript and retried once the
			// window passes instead of ending the run like Pi does. That's
			// intentional, so keep it when porting upstream agent-loop changes.
			message, err := streamAssistantResponse(ctx, currentCtx, config, client, model, systemPrompt, opts, emit)
			for err != nil {
				resetAt, ok := ai.UsageLimitResetAt(err)
				if !ok {
					break
				}

				errMsg := &Message{
					Role:               RoleAssistant,
					Content:            []ai.ContentBlock{{Type: ai.ContentBlockText, Text: ""}},
					IsError:            true,
					API:                model.API,
					Provider:           model.Provider,
					Model:              model.ID,
					StopReason:         ai.StopReasonError,
					ErrorMessage:       err.Error(),
					Timestamp:          time.Now(),
					ExcludeFromContext: true,
				}
				emit(MessageStart{Message: errMsg})
				emit(MessageEnd{Message: errMsg})
				newMessages = append(newMessages, errMsg)

				emit(UsageLimitWait{Provider: model.Provider, ResetsAt: resetAt})

				if waitErr := sleepUntil(ctx, resetAt.Add(usageLimitRetryBuffer)); waitErr != nil {
					return newMessages, waitErr
				}

				message, err = streamAssistantResponse(ctx, currentCtx, config, client, model, systemPrompt, opts, emit)
			}
			if err != nil {
				if message != nil {
					newMessages = append(newMessages, message)
				}
				return newMessages, err
			}

			// Fire `after_provider_response` once the response is received.
			// The on-response callback (ai.WithResponseCallback) is the
			// canonical transformation seam; this is the plugin dispatch
			// for symmetry.
			dispatchEvent("after_provider_response", eventPayloadMessage(message))

			if message == nil {
				return newMessages, nil
			}

			newMessages = append(newMessages, message)

			if message.StopReason == ai.StopReasonError || message.StopReason == ai.StopReasonAborted {
				if config.FinishTurn != nil {
					if _, err := config.FinishTurn(ctx, AgentTurnContext{Message: message, Context: currentCtx, NewMessages: newMessages}); err != nil {
						return newMessages, fmt.Errorf("finish turn: %w", err)
					}
				}
				emit(TurnEnd{Message: message})
				dispatchEvent("turn_end", nil)
				if config.FlushContextMessages != nil {
					config.FlushContextMessages()
				}
				return newMessages, nil
			}

			// Check for tool calls
			toolCalls := extractToolCalls(message)
			hasMoreToolCalls = len(toolCalls) > 0

			// Dispatch tool_call events and check for blocks
			blockedToolCalls := make(map[int]bool)
			overriddenToolCalls := make(map[int]*ToolResult)
			for i := range toolCalls {
				if err := ctx.Err(); err != nil {
					return newMessages, err
				}
				blocked := dispatchEvent("tool_call", eventPayloadToolCall(toolCalls[i])).Blocked
				if err := ctx.Err(); err != nil {
					return newMessages, err
				}
				blockedToolCalls[i] = blocked

				if toolCalls[i].Name == "bash" {
					result := dispatchEvent("user_bash", eventPayloadToolCall(toolCalls[i]))
					if err := ctx.Err(); err != nil {
						return newMessages, err
					}
					blockedToolCalls[i] = blockedToolCalls[i] || result.Blocked
					for _, response := range result.Responses {
						blockedToolCalls[i] = blockedToolCalls[i] || response.Block
						if len(response.ResponseData) == 0 {
							continue
						}
						if overriddenToolCalls[i] != nil {
							overriddenToolCalls[i] = &ToolResult{Content: "multiple user bash overrides", IsError: true}
							break
						}
						overriddenToolCalls[i] = userBashOverride(response.ResponseData)
					}
				}

			}

			// Execute non-blocked tools
			var toolResults []*ToolResult
			if hasMoreToolCalls {
				// Build list of non-blocked tool calls
				var nonBlockedCalls []ai.ToolCall
				for i, tc := range toolCalls {
					if !blockedToolCalls[i] && overriddenToolCalls[i] == nil {
						nonBlockedCalls = append(nonBlockedCalls, tc)
					} else {
						// Finalize blocked result as a normal tool result.
						// Emit ToolStart before finalization (matches reference: start fires
						// first, then execution is short-circuited for blocked tools).
						var rawArgs map[string]any
						if tc.Arguments != nil {
							_ = json.Unmarshal(tc.Arguments, &rawArgs)
						}
						emit(ToolStart{
							ToolCallID: tc.ID,
							Name:       tc.Name,
							Args:       rawArgs,
						})
						blockedResult := &ToolResult{
							Content: "Tool execution was blocked by a plugin",
							IsError: true,
						}
						if override := overriddenToolCalls[i]; override != nil && !blockedToolCalls[i] {
							blockedResult = override
						}
						emit(ToolEnd{
							ToolCallID: tc.ID,
							Name:       tc.Name,
							Result:     blockedResult,
							IsError:    blockedResult.IsError,
						})
						toolResult := finalizeToolResultMessage(tc, blockedResult, emit)
						currentCtx.Messages = append(currentCtx.Messages, toolResult.Message)
						newMessages = append(newMessages, toolResult.Message)
						dispatchEvent("tool_result", eventPayloadToolResult(toolResult))
						toolResults = append(toolResults, toolResult)
					}
				}

				// Execute non-blocked tools
				if len(nonBlockedCalls) > 0 {
					results := executeToolCallsWithFilter(ctx, currentCtx, message, nonBlockedCalls, config, emit)
					for _, result := range results {
						currentCtx.Messages = append(currentCtx.Messages, result.Message)
						newMessages = append(newMessages, result.Message)
						dispatchEvent("tool_result", eventPayloadToolResult(result))
						toolResults = append(toolResults, result)
					}
				}
			}

			if len(toolResults) > 0 {
				allTerminate := true
				for _, result := range toolResults {
					allTerminate = allTerminate && result.Terminate
				}
				if allTerminate {
					hasMoreToolCalls = false
				}
			}
			lastTurn = AgentTurnContext{Message: message, ToolResults: toolResults, Context: currentCtx, NewMessages: newMessages}
			var decision TurnDecision
			if config.FinishTurn != nil {
				decision, err = config.FinishTurn(ctx, AgentTurnContext{Message: message, ToolResults: toolResults, Context: currentCtx, NewMessages: newMessages})
				if err != nil {
					return newMessages, fmt.Errorf("finish turn: %w", err)
				}
			}
			emit(TurnEnd{
				Message:    message,
				ToolCalls:  toolCalls,
				ToolResult: toolResults,
			})
			dispatchEvent("turn_end", nil)
			if config.FlushContextMessages != nil {
				config.FlushContextMessages()
			}
			if decision == TurnEndRun {
				return newMessages, nil
			}
			explicitContinuation = decision == TurnContinue

			// Steering guidance is admitted at each turn boundary. Follow-ups
			// remain completion-only and are checked after the tool-driven run
			// would otherwise stop.
			pendingMessages, err = getSteeringMessages(ctx, config)
			if err != nil {
				return newMessages, err
			}
			if hasMoreToolCalls || len(pendingMessages) > 0 {
				explicitContinuation = false
			}
		}

		// Agent would stop here. Check for follow-up messages.
		followUpMessages, err := getFollowUpMessages(ctx, config)
		if err != nil {
			return newMessages, err
		}
		if len(followUpMessages) > 0 {
			explicitContinuation = false
			// Set as pending so inner loop processes them
			pendingMessages = followUpMessages

			continue
		}

		if explicitContinuation {
			explicitContinuation = false
			continue
		}
		payload, err := json.Marshal(map[string]any{"messages": currentCtx.Messages})
		if err != nil {
			return newMessages, fmt.Errorf("marshal settlement: %w", err)
		}
		settlement := dispatchEvent("agent_before_settle", payload)
		if err := ctx.Err(); err != nil {
			return newMessages, err
		}
		for _, response := range settlement.Responses {
			if len(response.ResponseData) == 0 {
				continue
			}
			var update struct {
				Continue bool       `json:"continue"`
				Messages []*Message `json:"messages"`
			}
			if err := json.Unmarshal(response.ResponseData, &update); err != nil {
				return newMessages, fmt.Errorf("parse settlement: %w", err)
			}
			pendingMessages = append(pendingMessages, update.Messages...)
			explicitContinuation = explicitContinuation || update.Continue
		}
		if len(pendingMessages) > 0 || explicitContinuation {
			explicitContinuation = false
			continue
		}
		break
	}

	return newMessages, nil
}

// getSteeringMessages gets steering messages if configured.
func getSteeringMessages(ctx context.Context, config AgentLoopConfig) ([]*Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.GetSteeringMessages != nil {
		messages, err := config.GetSteeringMessages(ctx)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return messages, nil
	}

	return nil, nil
}

// getFollowUpMessages gets follow-up messages if configured.
func getFollowUpMessages(ctx context.Context, config AgentLoopConfig) ([]*Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.GetFollowUpMessages != nil {
		messages, err := config.GetFollowUpMessages(ctx)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return messages, nil
	}

	return nil, nil
}

// usageLimitRetryBuffer waits slightly past the provider-reported reset time
// so a request landing exactly at the reset doesn't bounce off a stale quota.
var usageLimitRetryBuffer = 5 * time.Second

// pausePollInterval is how often a paused loop rechecks its pause flag.
var pausePollInterval = 15 * time.Second

// waitWhilePaused holds the next provider request while config.PausedBy
// reports a pause flag.
func waitWhilePaused(ctx context.Context, config AgentLoopConfig, emit EventSink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if config.PausedBy == nil {
		return nil
	}
	target := config.PausedBy()
	if target == "" {
		return nil
	}
	emit(Paused{Target: target})
	ticker := time.NewTicker(pausePollInterval)
	defer ticker.Stop()
	for config.PausedBy() != "" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

func sleepUntil(ctx context.Context, until time.Time) error {
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type publishedStreamFailure struct {
	message *Message
	err     error
}

func (e *publishedStreamFailure) Error() string { return e.err.Error() }
func (e *publishedStreamFailure) Unwrap() error { return e.err }

// streamAssistantResponse streams an assistant response from the LLM.
func streamAssistantResponse(
	ctx context.Context,
	agentCtx *AgentContext,
	config AgentLoopConfig,
	client ai.Client,
	model *ai.Model,
	systemPrompt string,
	opts []ai.Option,
	emit EventSink,
) (*Message, error) {
	if err := waitWhilePaused(ctx, config, emit); err != nil {
		return nil, err
	}
	// Apply context transform if configured
	messages := agentCtx.Messages

	if config.TransformContext != nil {
		var err error
		messages, err = config.TransformContext(ctx, messages)
		if err != nil {
			return nil, fmt.Errorf("transform context: %w", err)
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var plainMessages, requestMessages []*Message
	var retainedSystem bool
	for _, message := range dropOrphanToolResults(messages) {
		if message == nil || message.ExcludeFromContext {
			continue
		}
		if message.Role == RoleSystem && !retainedSystem && len(message.Sections)+len(message.ToolsAdded)+len(message.ToolsRemoved) == 0 {
			plainMessages = append(plainMessages, message)
			continue
		}
		requestMessages = append(requestMessages, message)
		if message.Role == RoleSystem {
			retainedSystem = true
		}
		if message.Role != RoleSystem {
			plainMessages = append(plainMessages, message)
		}
	}
	requestSystemPrompt, ordinaryMessages := liftSystemMessagesIntoPrompt(systemPrompt, plainMessages)

	if len(ordinaryMessages) > 0 && ordinaryMessages[0].Role == RoleUser {
		firstOrdinary := slices.IndexFunc(requestMessages, func(message *Message) bool { return message.Role != RoleSystem })
		if firstOrdinary < 0 || requestMessages[firstOrdinary].Role != RoleUser {
			requestMessages = append([]*Message{ordinaryMessages[0]}, requestMessages...)
		}
	}

	// Convert to LLM-compatible messages
	llmMessages, err := config.ConvertToLlm(ctx, requestMessages)
	if err != nil {
		return nil, fmt.Errorf("convert to llm: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Build LLM context
	aiCtx := &ai.Context{
		SystemPrompt: requestSystemPrompt,
		Messages:     llmMessages,
	}

	// Collect tools from context
	var tools []ai.Tool
	if len(agentCtx.Tools) > 0 {
		tools = make([]ai.Tool, 0, len(agentCtx.Tools))
		for _, t := range agentCtx.Tools {
			if !modelDeclarable(t) {
				continue
			}
			var params json.RawMessage
			if t.Parameters != nil {
				data, err := json.Marshal(t.Parameters)
				if err != nil {
					return nil, fmt.Errorf("marshal tool params: %w", err)
				}
				params = data
			}
			tools = append(tools, ai.Tool{
				ConstrainedSampling: t.ConstrainedSampling,
				Name:                t.Name,
				Description:         t.Description,
				Parameters:          params,
			})
		}

		if !hasToolDeclarationHistory(agentCtx.Messages) {
			initial, err := visibleToolDeclarations(agentCtx.Tools)
			if err != nil {
				return nil, fmt.Errorf("initial tool declarations: %w", err)
			}
			aiCtx.Tools = initial
		}
	}

	if config.EventDispatcher != nil {
		for _, name := range []string{"context", "context_with_system"} {
			if name == "context_with_system" && config.forcedSystemPrompt != nil {
				normalized := ai.NormalizeContext(aiCtx)
				head := ai.Message{Role: "system", Content: *config.forcedSystemPrompt}
				if current := ai.CurrentSystemMessage(normalized.Messages); current != nil {
					head.ToolsAdded = current.ToolsAdded
					head.Timestamp = current.Timestamp
				}
				ordinary := slices.DeleteFunc(normalized.Messages, func(message ai.Message) bool { return message.Role == "system" })
				aiCtx = &ai.Context{Messages: append([]ai.Message{head}, ordinary...)}
			}
			normalized := ai.NormalizeContext(aiCtx)
			visible := normalized.Messages
			if name == "context" {
				visible = nil
				for _, message := range normalized.Messages {
					if message.Role != "system" {
						visible = append(visible, message)
					}
				}
			}
			payload, err := json.Marshal(map[string]any{"messages": visible})
			if err != nil {
				return nil, fmt.Errorf("marshal %s: %w", name, err)
			}
			result := config.EventDispatcher(ctx, name, payload, config.SessionID)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for _, response := range result.Responses {
				if len(response.ResponseData) == 0 {
					continue
				}
				var update struct {
					Messages []ai.Message `json:"messages"`
				}
				if err := json.Unmarshal(response.ResponseData, &update); err != nil {
					return nil, fmt.Errorf("parse %s: %w", name, err)
				}
				if update.Messages != nil {
					if name == "context_with_system" {
						aiCtx = &ai.Context{Messages: update.Messages}
					} else {
						// Only context_with_system may replace system instructions or tool state.
						ordinary := slices.DeleteFunc(slices.Clone(update.Messages), func(message ai.Message) bool { return message.Role == "system" })
						merged := make([]ai.Message, 0, len(normalized.Messages)+len(ordinary))
						next := 0
						for _, message := range normalized.Messages {
							if message.Role == "system" {
								merged = append(merged, message)
							} else if next < len(ordinary) {
								merged = append(merged, ordinary[next])
								next++
							}
						}
						merged = append(merged, ordinary[next:]...)
						aiCtx = &ai.Context{Messages: merged}
					}
				}
			}
		}
	}

	// Transcript restoration and hooks may declare tools, but cannot grant registry exposure.
	visibleTools := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		visibleTools[tool.Name] = struct{}{}
	}
	aiCtx.Messages = slices.Clone(aiCtx.Messages)
	for i := range aiCtx.Messages {
		message := &aiCtx.Messages[i]
		if message.Role != "system" {
			continue
		}
		message.ToolsAdded = slices.DeleteFunc(slices.Clone(message.ToolsAdded), func(tool ai.Tool) bool {
			_, visible := visibleTools[tool.Name]
			return !visible
		})
	}

	var (
		mu             sync.Mutex
		partialMessage *Message
		finalMessage   *Message
		settled        bool
	)

	addedPartial := false
	started := time.Now()

	// Create event handler
	handler := func(c context.Context, event ai.StreamEvent) {
		if config.OnProviderStreamEvent != nil {
			config.OnProviderStreamEvent(c, event)
		}
		mu.Lock()
		defer mu.Unlock()
		if settled {
			return
		}

		switch e := event.(type) {
		case ai.Start:
			partialMessage = &Message{
				Role:      RoleAssistant,
				Content:   e.Partial.Content,
				Timestamp: time.UnixMilli(e.Partial.Timestamp),
			}
			agentCtx.Messages = append(agentCtx.Messages, partialMessage)
			addedPartial = true

			emit(MessageStart{Message: partialMessage})

		case ai.TextStart:
			if partialMessage == nil {
				return
			}
			partialMessage.Content = e.Partial.Content
			emit(MessageUpdate{Message: partialMessage, AssistantMessageEvent: e})

		case ai.TextDelta:
			if partialMessage == nil {
				return
			}
			partialMessage.Content = e.Partial.Content
			emit(MessageUpdate{Message: partialMessage, AssistantMessageEvent: e})

		case ai.TextEnd:
			if partialMessage == nil {
				return
			}
			partialMessage.Content = e.Partial.Content
			emit(MessageUpdate{Message: partialMessage, AssistantMessageEvent: e})

		case ai.ThinkingStart:
			if partialMessage == nil {
				return
			}
			partialMessage.Content = e.Partial.Content
			emit(MessageUpdate{Message: partialMessage, AssistantMessageEvent: e})

		case ai.ThinkingDelta:
			if partialMessage == nil {
				return
			}
			partialMessage.Content = e.Partial.Content
			emit(MessageUpdate{Message: partialMessage, AssistantMessageEvent: e})

		case ai.ThinkingEnd:
			if partialMessage == nil {
				return
			}
			partialMessage.Content = e.Partial.Content
			emit(MessageUpdate{Message: partialMessage, AssistantMessageEvent: e})

		case ai.ToolCallStart:
			if partialMessage == nil {
				return
			}
			partialMessage.Content = e.Partial.Content
			emit(MessageUpdate{Message: partialMessage, AssistantMessageEvent: e})

		case ai.ToolCallDelta:
			if partialMessage == nil {
				return
			}
			partialMessage.Content = e.Partial.Content
			emit(MessageUpdate{Message: partialMessage, AssistantMessageEvent: e})

		case ai.ToolCallEnd:
			if partialMessage == nil {
				return
			}
			partialMessage.Content = e.Partial.Content
			emit(MessageUpdate{Message: partialMessage, AssistantMessageEvent: e})

		case ai.Done:
			settled = true
			{
				if e.Message.StopReason == "" {
					e.Message.StopReason = e.Reason
				}
				msg := messageFromAI(&e.Message)
				recordResponseModel(msg, model)
				msg.ThinkingLevel = config.ThinkingLevel
				if msg.ThinkingLevel == "" {
					msg.ThinkingLevel = ThinkingOff
				}
				if msg.DurationMs == nil {
					msg.DurationMs = new(float64(time.Since(started)) / float64(time.Millisecond))
				}
				finalMessage = msg
				if addedPartial {
					agentCtx.Messages[len(agentCtx.Messages)-1] = msg
				} else {
					agentCtx.Messages = append(agentCtx.Messages, msg)
					emit(MessageStart{Message: msg})
				}

				emit(MessageEnd{Message: msg})
			}

		case ai.StreamError:
			settled = true
			// Usage limits go back to the caller's wait-and-retry instead of
			// becoming a terminal error.
			if _, ok := ai.UsageLimitResetAt(e.Err); ok {
				err = e.Err
				return
			}
			if e.Msg.StopReason == "" {
				e.Msg.StopReason = ai.StopReasonError
				if errors.Is(e.Err, context.Canceled) || errors.Is(e.Err, context.DeadlineExceeded) {
					e.Msg.StopReason = ai.StopReasonAborted
				}
			}
			if e.Msg.ErrorMessage == "" && e.Err != nil {
				e.Msg.ErrorMessage = e.Err.Error()
			}
			msg := messageFromAI(&e.Msg)
			recordResponseModel(msg, model)
			msg.ThinkingLevel = config.ThinkingLevel
			if msg.ThinkingLevel == "" {
				msg.ThinkingLevel = ThinkingOff
			}
			if msg.DurationMs == nil {
				msg.DurationMs = new(float64(time.Since(started)) / float64(time.Millisecond))
			}
			if msg.Role == "" {
				msg.Role = RoleAssistant
			}
			if msg.Timestamp.IsZero() {
				msg.Timestamp = time.Now()
			}
			msg.IsError = e.Msg.StopReason == ai.StopReasonError || e.Msg.StopReason == ai.StopReasonAborted
			finalMessage = msg

			if addedPartial {
				agentCtx.Messages[len(agentCtx.Messages)-1] = msg
			} else {
				agentCtx.Messages = append(agentCtx.Messages, msg)
				emit(MessageStart{Message: msg})
				addedPartial = true
			}
			emit(MessageEnd{Message: msg})
		}
	}

	// Resolve API key for every LLM call. This matches the TypeScript
	// streamAssistantResponse behavior and is important for short-lived OAuth
	// tokens that may expire while tools are running between turns.
	streamOpts := opts
	apiKey := config.APIKey

	var env map[string]string
	provider := ""
	if model != nil {
		provider = model.Provider
	}
	if config.RequestCredentials != nil {
		key, variables, err := config.RequestCredentials(ctx, provider)
		if err != nil {
			return nil, fmt.Errorf("get request credentials: %w", err)
		}
		if key != "" {
			apiKey = key
		}
		env = variables
	} else {
		if config.GetAPIKey != nil {
			key, err := config.GetAPIKey(ctx, provider)
			if err != nil {
				return nil, fmt.Errorf("get api key: %w", err)
			}
			if key != "" {
				apiKey = key
			}
		}
		if config.ProviderEnv != nil {
			variables, err := config.ProviderEnv(ctx, provider)
			if err != nil {
				return nil, fmt.Errorf("get provider environment: %w", err)
			}
			env = variables
		}
	}
	if apiKey != "" {
		streamOpts = append(slices.Clone(streamOpts), ai.WithAPIKey(apiKey))
	}
	if env != nil {
		streamOpts = append(slices.Clone(streamOpts), ai.WithProviderEnv(env))
	}

	// Like upstream streamSimple, the response budget defaults to the model cap
	// and never exceeds the room the prompt leaves in the context window.
	if model != nil {
		budget := ai.ExtractSimpleStreamOptions(streamOpts).MaxTokens
		if budget <= 0 {
			budget = model.MaxTokens
		}
		if budget > 0 {
			streamOpts = append(slices.Clone(streamOpts), ai.WithMaxTokens(ai.ClampMaxTokensToContext(model, aiCtx, budget)))
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Stream from the model
	if err := ai.ValidateContextLimits(model, aiCtx); err != nil {
		return nil, fmt.Errorf("validate model request: %w", err)
	}
	if streamErr := client.Stream(ctx, model, aiCtx, handler, streamOpts...); streamErr != nil {
		if finalMessage != nil && finalMessage.IsError {
			return finalMessage, &publishedStreamFailure{message: finalMessage, err: streamErr}
		}
		return nil, streamErr
	}
	if err != nil {
		return nil, err
	}

	if finalMessage != nil {
		return finalMessage, nil
	}

	return nil, errors.New("assistant stream ended without a terminal event")
}

// dropOrphanToolResults removes tool results whose tool call is not present
// earlier in the list. After a mid-run ReplaceMessages (compaction), a result
// can outlive the assistant tool-call message that produced it; providers
// reject transcripts containing such orphan results.
func dropOrphanToolResults(messages []*Message) []*Message {
	toolCallIDs := make(map[string]bool)
	kept := make([]*Message, 0, len(messages))
	for _, msg := range messages {
		if msg == nil || msg.ExcludeFromContext {
			continue
		}
		if msg.Role == RoleAssistant {
			for _, block := range msg.Content {
				if block.Type == ai.ContentBlockToolCall && block.ID != "" {
					toolCallIDs[block.ID] = true
				}
			}
		}
		if (msg.Role == RoleTool || msg.Role == "toolResult") && !toolCallIDs[msg.ToolCallID] {
			continue
		}
		kept = append(kept, msg)
	}
	return kept
}

func liftSystemMessagesIntoPrompt(systemPrompt string, messages []*Message) (string, []*Message) {
	if len(messages) == 0 {
		return systemPrompt, messages
	}

	var systemParts []string
	out := make([]*Message, 0, len(messages))
	for _, msg := range messages {
		if msg == nil || msg.Role != RoleSystem {
			out = append(out, msg)
			continue
		}
		text := ai.CurrentSystemPrompt([]ai.Message{messageToAI(msg)})
		if text == "" {
			continue
		}
		systemParts = append(systemParts, text)
	}

	// A compaction that lands mid-turn leaves a transcript that is
	// summary-only or begins with an assistant tool-call turn (the orphaned
	// results ahead of it are dropped, and there is no user message before
	// it). Providers reject both shapes — an empty messages array, or a first
	// message that is not from the user (zai/GLM error 1214) — so ask the
	// model to resume from the summary.
	needsLeadingUser := len(out) == 0 || (out[0].Role != RoleUser && out[0].Role != RoleCustom)
	if len(systemParts) == 0 && !needsLeadingUser {
		return systemPrompt, messages
	}

	parts := make([]string, 0, 1+len(systemParts))
	if systemPrompt != "" {
		parts = append(parts, systemPrompt)
	}
	parts = append(parts, systemParts...)
	if needsLeadingUser {
		out = append([]*Message{NewUserMessage("Continue from where you left off.")}, out...)
	}
	return strings.Join(parts, "\n\n"), out
}

func messageText(msg *Message) string {
	if msg == nil {
		return ""
	}
	var b strings.Builder
	for _, block := range msg.Content {
		switch block.Type {
		case ai.ContentBlockText:
			b.WriteString(block.Text)
		case ai.ContentBlockThinking:
			if block.Thinking != "" {
				b.WriteString(block.Thinking)
			} else {
				b.WriteString(block.Text)
			}
		}
	}
	return b.String()
}

// extractToolCalls extracts tool calls from an assistant message.
func extractToolCalls(message *Message) []ai.ToolCall {
	var toolCalls []ai.ToolCall

	for _, block := range message.Content {
		if block.Type == "toolCall" {
			toolCalls = append(toolCalls, ai.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Namespace: block.Namespace,
				Arguments: block.Arguments,
			})
		}
	}

	return toolCalls
}

// executeToolCallsWithFilter executes only the specified tool calls (filtered list).
func executeToolCallsWithFilter(
	ctx context.Context,
	agentCtx *AgentContext,
	assistantMsg *Message,
	toolCalls []ai.ToolCall,
	config AgentLoopConfig,
	emit EventSink,
) []*ToolResult {
	if len(toolCalls) == 0 {
		return nil
	}

	// Respect provider capability: if parallel tool calls are not supported,
	// run sequentially regardless of config. This matches the auth/providers spec:
	// "skip parallel tool calls if unsupported".
	sequential := config.ToolExecution == ToolExecutionSequential ||
		!agentCtx.Capabilities.ParallelToolCalls

	if sequential {
		return executeSequential(ctx, agentCtx, assistantMsg, toolCalls, config, emit)
	}

	return executeParallel(ctx, agentCtx, assistantMsg, toolCalls, config, emit)
}

func parseArgsErr(args []byte) (map[string]any, error) {
	return parseArguments(args)
}

func executeSequential(
	ctx context.Context,
	agentCtx *AgentContext,
	assistantMsg *Message,
	toolCalls []ai.ToolCall,
	config AgentLoopConfig,
	emit EventSink,
) []*ToolResult {
	results := make([]*ToolResult, 0, len(toolCalls))

	for _, tc := range toolCalls {
		// Emit tool_execution_start before prepareToolCall, with raw arguments.
		// This matches the TypeScript reference behavior where start event carries
		// the original streaming JSON bytes.
		var rawArgs map[string]any
		if tc.Arguments != nil {
			_ = json.Unmarshal(tc.Arguments, &rawArgs)
		}
		emit(ToolStart{
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Args:       rawArgs,
		})

		prepared := prepareToolCall(ctx, agentCtx, assistantMsg, tc, config)

		var result *ToolResult
		if prepared.immediate != nil {
			result = prepared.immediate
		} else {
			result = executePreparedTool(ctx, agentCtx, assistantMsg, prepared, config, emit)
		}

		emit(ToolEnd{
			DurationMs: result.DurationMs,
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Result:     result,
			IsError:    result != nil && result.IsError,
		})
		result = finalizeToolResultMessage(tc, result, emit)

		results = append(results, result)
		if ctx.Err() != nil {
			break
		}
	}

	return results
}

func executeParallel(
	ctx context.Context,
	agentCtx *AgentContext,
	assistantMsg *Message,
	toolCalls []ai.ToolCall,
	config AgentLoopConfig,
	emit EventSink,
) []*ToolResult {
	type pendingResult struct {
		prepared preparedToolCall
		result   *ToolResult
	}
	pending := make([]pendingResult, 0, len(toolCalls))
	for _, call := range toolCalls {
		var rawArgs map[string]any
		if call.Arguments != nil {
			if err := json.Unmarshal(call.Arguments, &rawArgs); err != nil {
				rawArgs = nil
			}
		}
		emit(ToolStart{ToolCallID: call.ID, Name: call.Name, Args: rawArgs})
		prepared := prepareToolCall(ctx, agentCtx, assistantMsg, call, config)
		entry := pendingResult{prepared: prepared, result: prepared.immediate}
		if entry.result != nil {
			emit(ToolEnd{ToolCallID: call.ID, Name: call.Name, Result: entry.result, IsError: entry.result.IsError})
		}
		pending = append(pending, entry)
		if ctx.Err() != nil {
			break
		}
	}
	var wg sync.WaitGroup
	for i := range pending {
		if pending[i].result != nil {
			continue
		}
		wg.Go(func() {
			entry := &pending[i]
			entry.result = executePreparedTool(ctx, agentCtx, assistantMsg, entry.prepared, config, emit)
			call := entry.prepared.toolCall
			emit(ToolEnd{DurationMs: entry.result.DurationMs, ToolCallID: call.ID, Name: call.Name, Result: entry.result, IsError: entry.result.IsError})
		})
	}
	wg.Wait()
	// Completion events precede source-ordered transcript artifacts.
	results := make([]*ToolResult, 0, len(pending))
	for _, entry := range pending {
		results = append(results, finalizeToolResultMessage(entry.prepared.toolCall, entry.result, emit))
	}
	return results
}

func parseArguments(args json.RawMessage) (map[string]any, error) {
	if len(args) == 0 {
		return nil, nil
	}

	var result map[string]any
	if err := json.Unmarshal(args, &result); err != nil {
		return nil, fmt.Errorf("parse arguments: %w", err)
	}

	return result, nil
}

func marshalPreparedArguments(args map[string]any) (json.RawMessage, error) {
	if args == nil {
		return nil, nil
	}

	data, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("marshal prepared arguments: %w", err)
	}

	return data, nil
}

type preparedToolCall struct {
	toolCall  ai.ToolCall
	tool      *Tool
	args      map[string]any
	immediate *ToolResult
}

// hasToolDeclarationHistory reports whether the transcript declares tools.
// Legacy transcripts without tool declarations still use the registry as their loadout.
func hasToolDeclarationHistory(messages []*Message) bool {
	return slices.ContainsFunc(messages, func(message *Message) bool {
		return message != nil && message.Role == RoleSystem && !message.ExcludeFromContext && (message.Timestamp.Equal(time.UnixMilli(0)) || len(message.ToolsAdded) > 0 || len(message.ToolsRemoved) > 0)
	})
}

// prepareToolCall validates a call against the model's declarations. Passing
// authorizedTools supplies a caller-approved registry that skips those checks.
func prepareToolCall(ctx context.Context, agentCtx *AgentContext, assistantMsg *Message, toolCall ai.ToolCall, config AgentLoopConfig, authorizedTools ...[]Tool) (prepared preparedToolCall) {
	prepared = preparedToolCall{toolCall: toolCall}
	defer func() {
		if prepared.immediate != nil && prepared.immediate.Rejection == nil {
			prepared.immediate.Rejection = errors.New(prepared.immediate.Content)
		}
	}()
	if err := ctx.Err(); err != nil {
		prepared.immediate = &ToolResult{Content: err.Error(), IsError: true, Rejection: errors.Join(err, context.Cause(ctx))}
		return prepared
	}
	if assistantMsg != nil && assistantMsg.StopReason == ai.StopReasonLength {
		prepared.immediate = &ToolResult{Content: "Assistant output was truncated. Re-issue the tool call with complete arguments.", IsError: true}
		return prepared
	}

	tools := agentCtx.Tools
	if len(authorizedTools) > 0 {
		tools = authorizedTools[0]
	}
	for i := range tools {
		if tools[i].Name == toolCall.Name {
			prepared.tool = &tools[i]
			break
		}
	}

	if prepared.tool == nil {
		prepared.immediate = &ToolResult{
			Content: fmt.Sprintf("Tool %s not found", toolCall.Name),
			IsError: true,
			Details: map[string]any{},
		}
		return prepared
	}

	declared := true
	var declarationHistory []*Message
	if len(authorizedTools) == 0 {
		declarationHistory = agentCtx.Messages
		declared = !hasToolDeclarationHistory(declarationHistory)
	}
	for _, message := range declarationHistory {
		if message == nil || message.Role != RoleSystem || message.ExcludeFromContext {
			continue
		}
		for _, removed := range message.ToolsRemoved {
			if removed.Name == toolCall.Name {
				declared = false
			}
		}
		for _, added := range message.ToolsAdded {
			if added.Name == toolCall.Name {
				declared = true
			}
		}
	}
	if !declared {
		prepared.immediate = &ToolResult{Content: fmt.Sprintf("Tool %s was removed from the transcript", toolCall.Name), IsError: true}
		return prepared
	}

	if prepared.tool.Execute == nil {
		prepared.immediate = &ToolResult{Content: fmt.Sprintf("Tool %s has no executor", toolCall.Name), IsError: true}
		return prepared
	}

	if len(authorizedTools) == 0 && (!modelDeclarable(*prepared.tool) || ((prepared.tool.Exposure == ToolDeferred || prepared.tool.Exposure == ToolCodemode) && !hasToolDeclarationHistory(declarationHistory))) {
		prepared.immediate = &ToolResult{Content: "Tool is not exposed to the model", IsError: true}
		return prepared
	}

	args, err := parseArgsErr(toolCall.Arguments)
	if err != nil {
		prepared.immediate = &ToolResult{
			Content: fmt.Sprintf("Failed to parse arguments: %v", err),
			IsError: true,
			Details: map[string]any{},
		}
		return prepared
	}

	if prepared.tool.PrepareArguments != nil {
		args = prepared.tool.PrepareArguments(args)
	}
	if err := ctx.Err(); err != nil {
		prepared.immediate = &ToolResult{Content: err.Error(), IsError: true, Rejection: errors.Join(err, context.Cause(ctx))}
		return prepared
	}

	aiTool, err := agentToolToAITool(*prepared.tool)
	if err != nil {
		prepared.immediate = &ToolResult{
			Content: fmt.Sprintf("invalid tool schema: %v", err),
			IsError: true,
		}
		return prepared
	}
	preparedArgs, err := marshalPreparedArguments(args)
	if err != nil {
		prepared.immediate = &ToolResult{
			Content: fmt.Sprintf("failed to normalize arguments: %v", err),
			IsError: true,
		}
		return prepared
	}
	validatedArgs, err := ai.ValidateToolArguments(&aiTool, ai.ToolCall{
		ID:        toolCall.ID,
		Name:      toolCall.Name,
		Arguments: preparedArgs,
	})
	if err != nil {
		prepared.immediate = &ToolResult{
			Content:   err.Error(),
			Rejection: err,
			IsError:   true,
		}
		return prepared
	}
	// Set prepared.args to validated args BEFORE calling BeforeToolCall.
	// This matches the TypeScript reference behavior where beforeToolCall receives
	// validated args (after prepareArguments + schema validation).
	prepared.args = validatedArgs

	if config.BeforeToolCall != nil {
		beforeResult, err := config.BeforeToolCall(ctx, ToolCallContext{
			ToolCallID:       toolCall.ID,
			ToolName:         toolCall.Name,
			Args:             prepared.args,
			Context:          agentCtx,
			AssistantMessage: assistantMsg,
			ToolCall:         toolCall,
		})
		if err != nil {
			rejection := fmt.Errorf("before tool call: %w", err)
			prepared.immediate = &ToolResult{
				Content:   rejection.Error(),
				Rejection: errors.Join(rejection, ctx.Err(), context.Cause(ctx)),
				IsError:   true,
			}
			return prepared
		}
		if beforeResult.Block {
			reason := beforeResult.Reason
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			prepared.immediate = &ToolResult{
				Content:   reason,
				IsError:   true,
				Terminate: beforeResult.Terminate,
			}
		}
	}

	if err := ctx.Err(); err != nil {
		prepared.immediate = &ToolResult{Content: err.Error(), IsError: true, Rejection: errors.Join(err, context.Cause(ctx))}
	}
	return prepared
}

// RunToolCall executes a nested call through validation and hooks without transcript events.
func RunToolCall(ctx context.Context, agentCtx *AgentContext, assistant *Message, call ai.ToolCall, config AgentLoopConfig, onUpdate ...func(*ToolResult)) *ToolResult {
	if err := ctx.Err(); err != nil {
		return &ToolResult{Content: err.Error(), IsError: true, Rejection: errors.Join(err, context.Cause(ctx))}
	}
	prepared := prepareToolCall(ctx, agentCtx, assistant, call, config, agentCtx.Tools)
	if prepared.immediate != nil {
		return prepared.immediate
	}
	return executePreparedTool(ctx, agentCtx, assistant, prepared, config, func(event Event) {
		update, ok := event.(ToolExecutionUpdate)
		if !ok {
			return
		}
		for _, sink := range onUpdate {
			if sink != nil {
				sink(update.PartialResult)
			}
		}
	})
}

func executePreparedTool(ctx context.Context, agentCtx *AgentContext, assistantMsg *Message, prepared preparedToolCall, config AgentLoopConfig, emit EventSink) *ToolResult {
	if err := ctx.Err(); err != nil {
		return &ToolResult{Content: err.Error(), IsError: true, Rejection: errors.Join(err, context.Cause(ctx))}
	}
	if prepared.tool.WaitReady != nil {
		if err := prepared.tool.WaitReady(ctx); err != nil {
			return &ToolResult{Content: fmt.Sprintf("tool readiness: %v", err), IsError: true, Rejection: errors.Join(fmt.Errorf("tool readiness: %w", err), ctx.Err(), context.Cause(ctx))}
		}
	}
	if err := ctx.Err(); err != nil {
		return &ToolResult{Content: err.Error(), IsError: true, Rejection: errors.Join(err, context.Cause(ctx))}
	}
	nestedContext := *agentCtx
	var reachErr error
	nestedContext.Tools, reachErr = ReachableToolsContext(ctx, agentCtx, *prepared.tool, prepared.args)
	if reachErr != nil {
		return &ToolResult{Content: reachErr.Error(), IsError: true, Rejection: errors.Join(reachErr, ctx.Err(), context.Cause(ctx))}
	}
	if err := ctx.Err(); err != nil {
		return &ToolResult{Content: err.Error(), IsError: true, Rejection: errors.Join(err, context.Cause(ctx))}
	}
	for _, tool := range nestedContext.Tools {
		if tool.WaitReady != nil {
			if err := tool.WaitReady(ctx); err != nil {
				return &ToolResult{Content: fmt.Sprintf("reachable tool %s readiness: %v", tool.Name, err), IsError: true, Rejection: errors.Join(fmt.Errorf("reachable tool %s readiness: %w", tool.Name, err), ctx.Err(), context.Cause(ctx))}
			}
		}
		if err := ctx.Err(); err != nil {
			return &ToolResult{Content: err.Error(), IsError: true, Rejection: errors.Join(err, context.Cause(ctx))}
		}
	}
	nestedCtx, cancelNested := context.WithCancel(ctx)
	defer cancelNested()
	var nestedMu sync.Mutex
	var nestedWG sync.WaitGroup
	nestedClosed := false
	var nestedCalls []NestedToolCall
	type runningCall struct {
		sequence uint64
		record   NestedToolCall
	}
	var running []runningCall
	snapshotNested := func() []NestedToolCall {
		records := slices.Clone(nestedCalls)
		for _, call := range running {
			records = append(records, call.record)
		}
		return slices.Clone(records[max(0, len(records)-128):])
	}
	var nestedUsage ai.Usage
	var nestedCount uint64
	started := time.Now()
	var progressMu sync.Mutex
	settled := false
	var callerProgress ToolResult
	publishNested := func() {
		nestedMu.Lock()
		records := snapshotNested()
		nestedMu.Unlock()
		progressMu.Lock()
		defer progressMu.Unlock()
		if settled || nestedCtx.Err() != nil {
			return
		}
		partial := callerProgress
		partial.NestedCalls = records
		emitToolUpdate(prepared.toolCall, prepared.args, &partial, emit)
	}
	executeNested := func(callCtx context.Context, call ai.ToolCall) *ToolResult {
		runCtx, cancelRun := context.WithCancel(nestedCtx)
		defer cancelRun()
		defer context.AfterFunc(callCtx, cancelRun)()
		nestedMu.Lock()
		if nestedClosed {
			nestedMu.Unlock()
			return &ToolResult{Content: "caller execution has ended", IsError: true, Rejection: context.Canceled}
		}
		nestedCount++
		sequence := nestedCount
		if call.ID == "" {
			call.ID = fmt.Sprintf("%s/%d", prepared.toolCall.ID, nestedCount)
		}
		nestedWG.Add(1)
		if len(running) == 128 {
			running = slices.Delete(running, 0, 1)
		}
		running = append(running, runningCall{sequence: sequence, record: NestedToolCall{ID: call.ID, Name: call.Name, Status: "running"}})
		nestedMu.Unlock()
		publishNested()
		defer nestedWG.Done()
		nestedResult := RunToolCall(runCtx, &nestedContext, assistantMsg, call, config, func(*ToolResult) { publishNested() })
		record := NestedToolCall{ID: call.ID, Name: call.Name, Status: "ok", DurationMs: nestedResult.DurationMs}
		if nestedResult.IsError {
			record.Status = "error"
			record.Error = string([]rune(nestedResult.Content)[:min(500, len([]rune(nestedResult.Content)))])
		}
		if errors.Is(nestedResult.Rejection, context.Canceled) || errors.Is(nestedResult.Rejection, context.DeadlineExceeded) {
			record.Status = "cancelled"
		}
		nestedMu.Lock()
		// ponytail: retain the last 128 completions; persist full diagnostics if needed.
		if len(nestedCalls) == 128 {
			nestedCalls = slices.Delete(nestedCalls, 0, 1)
		}
		running = slices.DeleteFunc(running, func(call runningCall) bool { return call.sequence == sequence })
		nestedCalls = append(nestedCalls, record)
		if nestedResult.Usage != nil {
			addToolUsage(&nestedUsage, *nestedResult.Usage)
		}
		nestedMu.Unlock()
		publishNested()
		return nestedResult
	}
	result, err := prepared.tool.Execute(ToolContext{
		ExecuteTool:        func(call ai.ToolCall) *ToolResult { return executeNested(nestedCtx, call) },
		ExecuteToolContext: executeNested,
		Tools:              nestedContext.Tools,
		Model:              agentCtx.Model,
		Context:            ctx,
		ToolCallID:         prepared.toolCall.ID,
		Name:               prepared.toolCall.Name,
		Args:               prepared.args,
		OnUpdate: func(partial *ToolResult) {
			if partial == nil {
				return
			}
			nestedMu.Lock()
			records := snapshotNested()
			nestedMu.Unlock()
			progressMu.Lock()
			defer progressMu.Unlock()
			if settled || ctx.Err() != nil {
				return
			}
			callerProgress = *partial
			callerProgress.ContentBlocks = slices.Clone(partial.ContentBlocks)
			snapshot := callerProgress
			snapshot.NestedCalls = append(slices.Clone(partial.NestedCalls), records...)
			emitToolUpdate(prepared.toolCall, prepared.args, &snapshot, emit)
		},
		SessionCWD:   agentCtx.SessionCWD,
		Capabilities: agentCtx.Capabilities,
	})
	nestedMu.Lock()
	nestedClosed = true
	cancelNested()
	nestedMu.Unlock()
	nestedWG.Wait()
	progressMu.Lock()
	settled = true
	progressMu.Unlock()
	durationMs := float64(time.Since(started)) / float64(time.Millisecond)
	if err != nil {
		rejected := &ToolResult{Content: err.Error(), Rejection: errors.Join(err, ctx.Err(), context.Cause(ctx)), IsError: true}
		if result != nil {
			rejected.Usage = result.Usage
			rejected.NestedCalls = slices.Clone(result.NestedCalls)
		}
		result = rejected
	}
	if result == nil {
		result = &ToolResult{Content: ""}
	}

	if len(nestedCalls) > 0 {
		result.NestedCalls = append(result.NestedCalls, nestedCalls...)
	}
	if len(result.NestedCalls) > 128 {
		result.NestedCalls = slices.Clone(result.NestedCalls[len(result.NestedCalls)-128:])
	}
	if nestedUsage != (ai.Usage{}) {
		if result.Usage == nil {
			result.Usage = new(ai.Usage)
		} else {
			result.Usage = new(*result.Usage)
		}
		addToolUsage(result.Usage, nestedUsage)
	}

	if config.AfterToolCall != nil {
		afterResult, afterErr := config.AfterToolCall(ctx, ToolCallContext{
			ToolCallID:       prepared.toolCall.ID,
			ToolName:         prepared.toolCall.Name,
			Args:             prepared.args,
			Context:          agentCtx,
			AssistantMessage: assistantMsg,
			ToolCall:         prepared.toolCall,
		}, result)
		if afterErr != nil {
			result = &ToolResult{
				NestedCalls: result.NestedCalls,
				Usage:       result.Usage,
				Content:     afterErr.Error(),
				Rejection:   errors.Join(afterErr, ctx.Err(), context.Cause(ctx)),
				IsError:     true,
			}
		} else {
			if afterResult.ContentBlocks != nil || afterResult.Content != "" {
				result.StructuredContent = nil
			}
			if afterResult.StructuredContent != nil {
				result.StructuredContent = afterResult.StructuredContent
			}
			if afterResult.Usage != nil {
				result.Usage = afterResult.Usage
			}
			if afterResult.Terminate != nil {
				result.Terminate = *afterResult.Terminate
			}
			// Apply override (matches TypeScript: content ?? result.content)
			if afterResult.ContentBlocks != nil {
				result.ContentBlocks = afterResult.ContentBlocks
				result.Content = ""
			} else if afterResult.Content != "" {
				result.Content = afterResult.Content
				result.ContentBlocks = nil
			}
			if afterResult.Details != nil {
				result.Details = afterResult.Details
			}
			if afterResult.IsError != nil {
				result.IsError = *afterResult.IsError
			}
		}
	}

	if !result.IsError && prepared.tool.OutputSchema != nil {
		validation, err := gojsonschema.Validate(gojsonschema.NewGoLoader(prepared.tool.OutputSchema), gojsonschema.NewBytesLoader(result.StructuredContent))
		if err != nil {
			err = fmt.Errorf("validate tool output: %w", err)
		} else if !validation.Valid() {
			err = fmt.Errorf("tool output does not match schema: %v", validation.Errors())
		}
		if err != nil {
			result = &ToolResult{
				NestedCalls: result.NestedCalls,
				Usage:       result.Usage,
				Content:     err.Error(),
				Rejection:   err,
				IsError:     true,
			}
		}
	}

	result.DurationMs = &durationMs
	return result
}

func finalizeToolResult(toolCall ai.ToolCall, toolResult *ToolResult, emit EventSink) *ToolResult {
	if toolResult == nil {
		toolResult = &ToolResult{Content: ""}
	}

	emit(ToolEnd{
		DurationMs: toolResult.DurationMs,
		ToolCallID: toolCall.ID,
		Name:       toolCall.Name,
		Result:     toolResult,
		IsError:    toolResult.IsError,
	})

	return finalizeToolResultMessage(toolCall, toolResult, emit)
}

func finalizeToolResultMessage(toolCall ai.ToolCall, toolResult *ToolResult, emit EventSink) *ToolResult {
	if toolResult == nil {
		toolResult = &ToolResult{Content: ""}
	}

	// Skip if already finalized.
	if toolResult.Message != nil {
		return toolResult
	}

	toolMsg := &Message{
		NestedCalls:        toolResult.NestedCalls,
		StructuredContent:  toolResult.StructuredContent,
		ToolUsage:          toolResult.Usage,
		Details:            toolResult.Details,
		DurationMs:         toolResult.DurationMs,
		Role:               RoleTool,
		ToolCallID:         toolCall.ID,
		ToolName:           toolCall.Name,
		Content:            toolResult.NormalizedContent(),
		IsError:            toolResult.IsError,
		ExcludeFromContext: toolResult.ExcludeFromContext,
		Timestamp:          time.Now(),
	}

	emit(MessageStart{Message: toolMsg})
	emit(MessageEnd{Message: toolMsg})
	toolResult.Message = toolMsg

	return toolResult
}

func emitToolUpdate(toolCall ai.ToolCall, args map[string]any, partial *ToolResult, emit EventSink) {
	if emit == nil || partial == nil {
		return
	}

	partialMsg := &Message{
		NestedCalls: slices.Clone(partial.NestedCalls),
		Role:        RoleTool,
		ToolCallID:  toolCall.ID,
		ToolName:    toolCall.Name,
		Content:     partial.NormalizedContent(),
		IsError:     partial.IsError,
		Timestamp:   time.Now(),
	}
	partial.Message = partialMsg

	emit(ToolUpdate{
		ToolCallID:    toolCall.ID,
		Name:          toolCall.Name,
		Args:          args,
		PartialResult: partial,
	})
}

func agentToolToAITool(tool Tool) (ai.Tool, error) {
	var params json.RawMessage
	if tool.Parameters != nil {
		data, err := json.Marshal(tool.Parameters)
		if err != nil {
			return ai.Tool{}, err
		}
		params = data
	}

	return ai.Tool{
		ConstrainedSampling: tool.ConstrainedSampling,
		Name:                tool.Name,
		Description:         tool.Description,
		Parameters:          params,
	}, nil
}

// messageFromAI converts an AI AssistantMessage to an agent Message.
func messageFromAI(msg *ai.AssistantMessage) *Message {
	isError := msg.StopReason == ai.StopReasonError || msg.StopReason == ai.StopReasonAborted
	return &Message{
		Diagnostics:           msg.Diagnostics,
		DurationMs:            msg.DurationMs,
		Role:                  RoleAssistant,
		Content:               msg.Content,
		IsError:               isError,
		API:                   msg.API,
		Provider:              msg.Provider,
		Model:                 msg.Model,
		StopReason:            msg.StopReason,
		Usage:                 msg.Usage,
		ErrorMessage:          msg.ErrorMessage,
		ResponseID:            msg.ResponseID,
		ProviderThinkingLevel: msg.ProviderThinkingLevel,
		Timestamp:             time.UnixMilli(msg.Timestamp),
	}
}

// messageToAI converts an agent Message to an AI Message.
func messageToAI(msg *Message) ai.Message {
	role := string(msg.Role)
	if msg.Role == RoleTool {
		role = "toolResult"
	}

	return ai.Message{
		Sections:              msg.Sections,
		ToolsAdded:            msg.ToolsAdded,
		ToolsRemoved:          msg.ToolsRemoved,
		Provider:              msg.Provider,
		API:                   msg.API,
		Model:                 msg.Model,
		StopReason:            string(msg.StopReason),
		ProviderThinkingLevel: msg.ProviderThinkingLevel,
		Diagnostics:           msg.Diagnostics,
		DurationMs:            msg.DurationMs,
		Role:                  role,
		Content:               msg.Content,
		ToolCallID:            msg.ToolCallID,
		ToolName:              msg.ToolName,
		IsError:               msg.IsError,
		Timestamp:             msg.Timestamp.UnixMilli(),
	}
}

// ConvertMessagesToAI converts agent messages to AI messages.
func ConvertMessagesToAI(messages []*Message) []ai.Message {
	result := make([]ai.Message, 0, len(messages))
	for _, m := range messages {
		if m == nil || m.ExcludeFromContext {
			continue
		}
		result = append(result, messageToAI(m))
	}

	return result
}

// DefaultConvertToLlm provides a default implementation of ConvertToLlm.
// This matches the TypeScript reference behavior: filters to RoleUser, RoleAssistant,
// and RoleTool (mapped to "toolResult"). Other roles are excluded.
// For converting all messages without filtering, use ConvertMessagesToAI instead.
func DefaultConvertToLlm(ctx context.Context, messages []*Message) ([]ai.Message, error) {
	return defaultConvertToLlm(ctx, messages)
}

// CreateToolCall creates a new tool call with a generated ID.
func CreateToolCall(name string, args map[string]any) (ai.ToolCall, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return ai.ToolCall{}, err
	}

	return ai.ToolCall{
		ID:        uuid.New().String(),
		Name:      name,
		Arguments: argsJSON,
	}, nil
}

// EventStream exposes a channel-based view of an agent loop run. The
// producer goroutine inside the loop pushes events into Events as they
// occur, then sends the final message list to Done or an error to Err.
// Exactly one of Done or Err receives a value; the other channel is closed
// without a value.
//
// Events is unbuffered. The consumer must read events as they arrive. If
// the consumer abandons the stream without reading, the loop will block on
// the next emit and remain running until the abort signal fires (via
// signal or context cancellation).
type EventStream struct {
	Events <-chan Event
	Done   <-chan []*Message
	Err    <-chan error
}

// AgentLoop starts an agent loop with new prompt messages. It returns an
// EventStream that emits events as they occur and the final message list
// when the loop completes. The signal channel, if non-nil, cancels the
// loop when it fires.
func AgentLoop(
	prompts []*Message,
	agentCtx *AgentContext,
	config AgentLoopConfig,
	signal <-chan struct{},
) *EventStream {
	return runEventLoop(func(ctx context.Context, emit EventSink) ([]*Message, error) {
		return RunAgentLoop(ctx, prompts, agentCtx, config, emit)
	}, signal)
}

// AgentLoopContinue continues an agent loop from the current context. It
// returns an EventStream that emits events as they occur and the final
// message list when the loop completes. The signal channel, if non-nil,
// cancels the loop when it fires.
func AgentLoopContinue(
	agentCtx *AgentContext,
	config AgentLoopConfig,
	signal <-chan struct{},
) *EventStream {
	return runEventLoop(func(ctx context.Context, emit EventSink) ([]*Message, error) {
		return RunAgentLoopContinue(ctx, agentCtx, config, emit)
	}, signal)
}

// runEventLoop is the shared implementation behind AgentLoop and
// AgentLoopContinue. It wires the signal channel to context cancellation
// and runs the loop in a goroutine, sending events to the returned
// EventStream.
func runEventLoop(
	run func(ctx context.Context, emit EventSink) ([]*Message, error),
	signal <-chan struct{},
) *EventStream {
	ctx, cancel := context.WithCancel(context.Background())

	if signal != nil {
		go func() {
			select {
			case <-signal:
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	events := make(chan Event)
	done := make(chan []*Message, 1)
	errs := make(chan error, 1)

	go func() {
		defer cancel()
		defer close(events)
		defer close(done)
		defer close(errs)

		emit := func(event Event) {
			select {
			case events <- event:
			case <-ctx.Done():
				// consumer abandoned or signal fired; stop emitting
			}
		}

		messages, err := run(ctx, emit)
		if err != nil {
			errs <- err
			return
		}
		done <- messages
	}()

	return &EventStream{
		Events: events,
		Done:   done,
		Err:    errs,
	}
}

func userBashOverride(raw []byte) *ToolResult {
	result, err := ParseUserBashOverride(raw)
	if err != nil {
		return &ToolResult{Content: err.Error(), IsError: true}
	}
	return &ToolResult{Content: result.Output, IsError: result.ExitCode != 0}
}

// ErrInvalidUserBashOverride reports a user_bash response that cannot replace execution.
var ErrInvalidUserBashOverride = errors.New("user bash override requires output and exitCode; remote operations are not supported")

// ParseUserBashOverride decodes a user_bash result override. Serialized
// operations cannot carry executable callbacks, so they are rejected rather
// than silently falling back to local execution.
func ParseUserBashOverride(raw []byte) (BashResult, error) {
	var override struct {
		Result *struct {
			Output   *string `json:"output"`
			ExitCode *int    `json:"exitCode"`
		} `json:"result"`
		Operations json.RawMessage `json:"operations"`
	}
	if err := json.Unmarshal(raw, &override); err != nil {
		return BashResult{}, fmt.Errorf("parse user bash override: %w", err)
	}
	if override.Result == nil || override.Result.Output == nil || override.Result.ExitCode == nil || len(override.Operations) > 0 {
		return BashResult{}, ErrInvalidUserBashOverride
	}
	return BashResult{Output: *override.Result.Output, ExitCode: *override.Result.ExitCode, CancelReason: BashCancelNone}, nil
}

func addToolUsage(total *ai.Usage, next ai.Usage) {
	total.Input += next.Input
	total.Output += next.Output
	total.CacheRead += next.CacheRead
	total.CacheWrite += next.CacheWrite
	total.TotalTokens += next.TotalTokens
	total.Cost.Input += next.Cost.Input
	total.Cost.Output += next.Cost.Output
	total.Cost.CacheRead += next.Cost.CacheRead
	total.Cost.CacheWrite += next.Cost.CacheWrite
	total.Cost.Total += next.Cost.Total
}
