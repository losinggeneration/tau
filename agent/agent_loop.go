package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/losinggeneration/tau/ai"
)

func aiThinkingBudget(level ThinkingLevel, budgets *ai.ThinkingBudgets) int {
	if budgets == nil {
		return 0
	}

	switch level {
	case ThinkingMinimal:
		if budgets.Minimal != nil {
			return *budgets.Minimal
		}
	case ThinkingLow:
		if budgets.Low != nil {
			return *budgets.Low
		}
	case ThinkingMedium:
		if budgets.Medium != nil {
			return *budgets.Medium
		}
	case ThinkingHigh:
		if budgets.High != nil {
			return *budgets.High
		}
	}

	return 0
}

func buildAIOptions(config AgentLoopConfig, model *ai.Model) []ai.Option {
	opts := []ai.Option{}
	if config.SessionID != "" {
		opts = append(opts, ai.WithSessionID(config.SessionID))
	}

	if config.ThinkingLevel != "" && config.ThinkingLevel != ThinkingOff {
		opts = append(opts, ai.WithReasoning(ai.ReasoningLevel(config.ThinkingLevel)))
		if budget := aiThinkingBudget(config.ThinkingLevel, config.ThinkingBudgets); budget > 0 {
			opts = append(opts, ai.WithThinkingEnabled(true), ai.WithThinkingBudgetTokens(budget))
		}
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
	client       ai.Client
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
	err := c.streamFn(ctx, model, prompt, func(_ context.Context, event ai.StreamEvent) {
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

	client, err := getLoopClient(model, config)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
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
		client:       client,
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
		if len(agentCtx.Messages) == 0 {
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

func executeLoopInvocation(
	ctx context.Context,
	invocation *loopInvocation,
	config AgentLoopConfig,
	emit EventSink,
) ([]*Message, error) {
	emit = withPluginEventDispatch(config, emit)
	dispatchEvent := func(name string, payload []byte) EventDispatchResult {
		if config.EventDispatcher != nil {
			return config.EventDispatcher(name, payload, config.SessionID)
		}
		return EventDispatchResult{}
	}

	// before_agent_start carries the current system prompt; subscribed plugins
	// may return response_data {"systemPrompt": "..."} to rewrite it. Rewrites
	// chain in dispatch order: each plugin's replacement becomes the base for
	// the next applied rewrite, and the final value drives the loop. (pi
	// semantics; within a pi-ext shim process, chained extensions are resolved
	// there before the single response crosses the RPC boundary.)
	if beforePayload, err := json.Marshal(map[string]any{"systemPrompt": invocation.systemPrompt}); err == nil {
		result := dispatchEvent("before_agent_start", beforePayload)
		for _, resp := range result.Responses {
			var data struct {
				SystemPrompt string `json:"systemPrompt"`
			}
			if len(resp.ResponseData) == 0 || json.Unmarshal(resp.ResponseData, &data) != nil {
				continue
			}
			if data.SystemPrompt != "" {
				invocation.systemPrompt = data.SystemPrompt
			}
		}
	} else {
		dispatchEvent("before_agent_start", nil)
	}
	emit(AgentStart{})
	dispatchEvent("agent_start", nil)
	emit(TurnStart{})
	dispatchEvent("turn_start", nil)

	for _, prompt := range invocation.newMessages {
		emit(MessageStart{Message: prompt})
		emit(MessageEnd{Message: prompt})
		// Fire the `input` event for plugin introspection. Plugins can
		// block or rewrite user input here; the loop does not yet act on
		// the block verdict, so the dispatch is informational.
		dispatchEvent("input", eventPayloadMessage(prompt))
	}

	result, err := runLoop(
		ctx,
		invocation.currentCtx,
		invocation.newMessages,
		config,
		invocation.client,
		invocation.systemPrompt,
		invocation.opts,
		emit,
	)
	if err != nil {
		// Direct loop callers still receive a complete lifecycle. Higher-level
		// Agent methods may additionally synthesize assistant error messages, but
		// AgentEnd must remain the final emitted loop event on this path.
		emit(AgentEnd{Messages: result})
		dispatchEvent("agent_end", nil)
		return result, err
	}

	emit(AgentEnd{Messages: result})
	dispatchEvent("agent_end", nil)
	return result, nil
}

func withPluginEventDispatch(config AgentLoopConfig, emit EventSink) EventSink {
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
		config.EventDispatcher(name, payload, config.SessionID)
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
		Role       string `json:"role"`
		Content    any    `json:"content"`
		ToolCallID string `json:"tool_call_id,omitempty"`
		ToolName   string `json:"tool_name,omitempty"`
		IsError    bool   `json:"is_error,omitempty"`
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
		ToolName      string            `json:"tool_name"`
		ToolCallID    string            `json:"tool_call_id"`
		Result        string            `json:"result"`
		ContentBlocks []ai.ContentBlock `json:"content_blocks,omitempty"`
		IsError       bool              `json:"is_error"`
	}
	p := payload{
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
	client ai.Client,
	systemPrompt string,
	opts []ai.Option,
	emit EventSink,
) ([]*Message, error) {
	if newMessages == nil {
		newMessages = []*Message{}
	}

	dispatchEvent := func(name string, payload []byte) EventDispatchResult {
		if config.EventDispatcher != nil {
			return config.EventDispatcher(name, payload, config.SessionID)
		}
		return EventDispatchResult{}
	}

	firstTurn := true
	consecutiveEmptyResponses := 0
	msgVersion := currentCtx.MessagesVersion

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

			// Get the model
			model := config.Model
			if model == nil {
				model = currentCtx.Model
			}
			// Fire the `model_select` event so plugins can observe which
			// model is driving the current turn.
			dispatchEvent("model_select", eventPayloadModel(model))

			// Fire `before_provider_request` so plugins can observe or
			// transform the request. This is informational; the block verdict
			// is not yet honored. The on-payload callback (ai.WithPayloadCallback)
			// is the canonical transformation seam.
			dispatchEvent("before_provider_request", eventPayloadModel(model))

			// Stream assistant response
			message, err := streamAssistantResponse(ctx, currentCtx, config, client, model, systemPrompt, opts, emit)
			if err != nil {
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

			if isEmptyAssistantResponse(message) {
				consecutiveEmptyResponses++
				if consecutiveEmptyResponses >= maxConsecutiveEmptyResponses {
					message.IsError = true
					message.StopReason = ai.StopReasonError
					message.ErrorMessage = fmt.Sprintf("empty response from server (%d consecutive)", consecutiveEmptyResponses)
				}
			} else {
				consecutiveEmptyResponses = 0
			}

			newMessages = append(newMessages, message)

			if message.StopReason == ai.StopReasonError || message.StopReason == ai.StopReasonAborted {
				emit(TurnEnd{
					Message:    message,
					ToolCalls:  nil,
					ToolResult: nil,
				})
				return newMessages, nil
			}

			// Check for tool calls
			toolCalls := extractToolCalls(message)
			hasMoreToolCalls = len(toolCalls) > 0

			// Dispatch tool_call events and check for blocks
			blockedToolCalls := make(map[int]bool)
			for i := range toolCalls {
				blocked := dispatchEvent("tool_call", eventPayloadToolCall(toolCalls[i])).Blocked
				blockedToolCalls[i] = blocked

				// Fire the bash-specific `user_bash` plugin event for tools
				// named "bash". Plugins can observe or transform the command.
				// The block verdict is not yet honored; this is informational.
				// The canonical tool_call event above remains the authoritative
				// plugin signal for any tool, including bash.
				if toolCalls[i].Name == "bash" {
					dispatchEvent("user_bash", eventPayloadToolCall(toolCalls[i]))
				}
			}

			// Execute non-blocked tools
			var toolResults []*ToolResult
			if hasMoreToolCalls {
				// Build list of non-blocked tool calls
				var nonBlockedCalls []ai.ToolCall
				for i, tc := range toolCalls {
					if !blockedToolCalls[i] {
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
						emit(ToolEnd{
							ToolCallID: tc.ID,
							Name:       tc.Name,
							Result:     blockedResult,
							IsError:    true,
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

			emit(TurnEnd{
				Message:    message,
				ToolCalls:  toolCalls,
				ToolResult: toolResults,
			})
			dispatchEvent("turn_end", nil)

			// Steering guidance is admitted at each turn boundary. Follow-ups
			// remain completion-only and are checked after the tool-driven run
			// would otherwise stop.
			pendingMessages, err = getSteeringMessages(ctx, config)
			if err != nil {
				return newMessages, err
			}
		}

		// Agent would stop here. Check for follow-up messages.
		followUpMessages, err := getFollowUpMessages(ctx, config)
		if err != nil {
			return newMessages, err
		}
		if len(followUpMessages) > 0 {
			// Set as pending so inner loop processes them
			pendingMessages = followUpMessages

			continue
		}

		// No more messages, exit
		break
	}

	return newMessages, nil
}

const maxConsecutiveEmptyResponses = 1

// getSteeringMessages gets steering messages if configured.
func getSteeringMessages(ctx context.Context, config AgentLoopConfig) ([]*Message, error) {
	if config.GetSteeringMessages != nil {
		return config.GetSteeringMessages(ctx)
	}

	return nil, nil
}

// getFollowUpMessages gets follow-up messages if configured.
func getFollowUpMessages(ctx context.Context, config AgentLoopConfig) ([]*Message, error) {
	if config.GetFollowUpMessages != nil {
		return config.GetFollowUpMessages(ctx)
	}

	return nil, nil
}

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
	// Apply context transform if configured
	messages := agentCtx.Messages

	if config.TransformContext != nil {
		var err error
		messages, err = config.TransformContext(ctx, messages)
		if err != nil {
			return nil, fmt.Errorf("transform context: %w", err)
		}
	}

	// System messages in the transcript are contextual instructions/summaries,
	// not ordinary chat turns. Several providers either reject or silently drop
	// role=system entries in the message array, so lift them into the provider's
	// dedicated system prompt before conversion. This is especially important for
	// compaction: after /compact the live history is a single "Session Summary:"
	// system message, and the next provider request must receive that summary.
	requestSystemPrompt, requestMessages := liftSystemMessagesIntoPrompt(systemPrompt, dropOrphanToolResults(messages))

	// Convert to LLM-compatible messages
	llmMessages, err := config.ConvertToLlm(ctx, requestMessages)
	if err != nil {
		return nil, fmt.Errorf("convert to llm: %w", err)
	}

	// Build LLM context
	aiCtx := &ai.Context{
		SystemPrompt: requestSystemPrompt,
		Messages:     llmMessages,
	}

	// Collect tools from context
	var tools []ai.Tool
	if len(agentCtx.Tools) > 0 {
		tools = make([]ai.Tool, len(agentCtx.Tools))
		for i, t := range agentCtx.Tools {
			var params json.RawMessage
			if t.Parameters != nil {
				data, err := json.Marshal(t.Parameters)
				if err != nil {
					return nil, fmt.Errorf("marshal tool params: %w", err)
				}
				params = data
			}
			tools[i] = ai.Tool{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			}
		}

		aiCtx.Tools = tools
	}

	var (
		mu             sync.Mutex
		partialMessage *Message
		finalMessage   *Message
	)

	addedPartial := false

	// Create event handler
	handler := func(c context.Context, event ai.StreamEvent) {
		mu.Lock()
		defer mu.Unlock()

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
			if e.Message.Content != nil {
				msg := messageFromAI(&e.Message)
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
			msg := messageFromAI(&e.Msg)
			if msg.Role == "" {
				msg.Role = RoleAssistant
			}
			if msg.Timestamp.IsZero() {
				msg.Timestamp = time.Now()
			}
			if e.Msg.StopReason == "" {
				msg.IsError = true
			}
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
	if config.GetAPIKey != nil {
		provider := ""
		if model != nil {
			provider = model.Provider
		}
		key, err := config.GetAPIKey(ctx, provider)
		if err != nil {
			return nil, fmt.Errorf("get api key: %w", err)
		}
		if key != "" {
			apiKey = key
		}
	}
	if apiKey != "" {
		streamOpts = append(append([]ai.Option(nil), opts...), ai.WithAPIKey(apiKey))
	}

	// Stream from the model
	err = client.Stream(ctx, model, aiCtx, handler, streamOpts...)
	if err != nil {
		return nil, err
	}

	if finalMessage != nil {
		return finalMessage, nil
	}

	// Return the last message in context
	if len(agentCtx.Messages) > 0 {
		return agentCtx.Messages[len(agentCtx.Messages)-1], nil
	}

	return nil, nil
}

// dropOrphanToolResults removes tool results whose tool call is not present
// earlier in the list. After a mid-run ReplaceMessages (compaction), a result
// can outlive the assistant tool-call message that produced it; providers
// reject transcripts containing such orphan results.
func dropOrphanToolResults(messages []*Message) []*Message {
	toolCallIDs := make(map[string]bool)
	kept := make([]*Message, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		if msg.Role == RoleAssistant {
			for _, block := range msg.Content {
				if block.Type == ai.ContentBlockToolCall && block.ID != "" {
					toolCallIDs[block.ID] = true
				}
			}
		}
		if msg.Role == RoleTool && !toolCallIDs[msg.ToolCallID] {
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
		text := strings.TrimSpace(messageText(msg))
		if text == "" {
			continue
		}
		systemParts = append(systemParts, text)
	}
	if len(systemParts) == 0 {
		return systemPrompt, messages
	}

	parts := make([]string, 0, 1+len(systemParts))
	if trimmed := strings.TrimSpace(systemPrompt); trimmed != "" {
		parts = append(parts, trimmed)
	}
	parts = append(parts, systemParts...)
	// A compaction that lands mid-turn leaves only the summary. Providers
	// reject a request with no conversational input, so ask the model to
	// resume from the summary.
	if len(out) == 0 {
		out = append(out, NewUserMessage("Continue from where you left off."))
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

func isEmptyAssistantResponse(message *Message) bool {
	if message == nil || message.Role != RoleAssistant {
		return false
	}
	if message.ErrorMessage != "" || message.StopReason == ai.StopReasonError || message.StopReason == ai.StopReasonAborted {
		return false
	}
	for _, block := range message.Content {
		switch block.Type {
		case ai.ContentBlockText:
			if strings.TrimSpace(block.Text) != "" {
				return false
			}
		case ai.ContentBlockThinking:
			// Thinking-only responses are not actionable for the next turn.
			if block.Name != "" || block.ID != "" || len(block.Arguments) > 0 || block.Data != "" || block.ThoughtSignature != "" || block.Redacted || block.TextSignature != "" {
				return false
			}
		case "":
			if block.Text != "" || block.Thinking != "" || block.Name != "" || block.ID != "" || len(block.Arguments) > 0 || block.Data != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// extractToolCalls extracts tool calls from an assistant message.
func extractToolCalls(message *Message) []ai.ToolCall {
	var toolCalls []ai.ToolCall

	for _, block := range message.Content {
		if block.Type == "toolCall" {
			toolCalls = append(toolCalls, ai.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
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
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Result:     result,
			IsError:    result != nil && result.IsError,
		})
		result = finalizeToolResultMessage(tc, result, emit)

		results = append(results, result)
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

	type completedResult struct {
		index  int
		result *ToolResult
	}

	pending := make([]pendingResult, 0, len(toolCalls))

	// Emit tool_execution_start for ALL tools before ANY prepare calls.
	// This matches the TypeScript reference behavior: preflight all starts,
	// then prepare all, then execute non-blocked tools concurrently.
	for _, tc := range toolCalls {
		var rawArgs map[string]any
		if tc.Arguments != nil {
			_ = json.Unmarshal(tc.Arguments, &rawArgs)
		}
		emit(ToolStart{
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Args:       rawArgs,
		})
	}

	// Now prepare all tool calls (may be immediate for validation failures).
	// Immediate preflight failures are emitted as soon as preparation finishes,
	// before any runnable tool is allowed to block finalization. The result slice
	// is still returned in assistant source order below.
	for _, tc := range toolCalls {
		prepared := prepareToolCall(ctx, agentCtx, assistantMsg, tc, config)
		entry := pendingResult{prepared: prepared}
		if prepared.immediate != nil {
			emit(ToolEnd{
				ToolCallID: prepared.toolCall.ID,
				Name:       prepared.toolCall.Name,
				Result:     prepared.immediate,
				IsError:    prepared.immediate.IsError,
			})
			entry.result = finalizeToolResultMessage(prepared.toolCall, prepared.immediate, emit)
		}
		pending = append(pending, entry)
	}

	completed := make(chan completedResult, len(toolCalls))

	var wg sync.WaitGroup
	for i := range pending {
		if pending[i].prepared.immediate != nil {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result := executePreparedTool(ctx, agentCtx, assistantMsg, pending[i].prepared, config, emit)
			pending[i].result = result
			completed <- completedResult{index: i, result: result}
		}(i)
	}

	go func() {
		wg.Wait()
		close(completed)
	}()

	for range completed {
		// drain completion channel to ensure all goroutines finish
	}

	// Finalize runnable tools in original source order (not completion order).
	// Immediate preflight failures were already emitted above and carry an
	// attached message, so finalizeToolResultMessage is idempotent for them.
	results := make([]*ToolResult, 0, len(pending))
	for _, p := range pending {
		result := p.result
		if p.prepared.immediate == nil {
			// Emit ToolEnd in source order, then emit the tool result message.
			// This matches TypeScript emitToolCallOutcome: tool_execution_end,
			// then message_start/message_end for runnable tools.
			emit(ToolEnd{
				ToolCallID: p.prepared.toolCall.ID,
				Name:       p.prepared.toolCall.Name,
				Result:     result,
				IsError:    result != nil && result.IsError,
			})
		}
		results = append(results, finalizeToolResultMessage(p.prepared.toolCall, result, emit))
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

func prepareToolCall(ctx context.Context, agentCtx *AgentContext, assistantMsg *Message, toolCall ai.ToolCall, config AgentLoopConfig) preparedToolCall {
	prepared := preparedToolCall{toolCall: toolCall}

	for i := range agentCtx.Tools {
		if agentCtx.Tools[i].Name == toolCall.Name {
			prepared.tool = &agentCtx.Tools[i]
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
			Content: err.Error(),
			IsError: true,
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
			prepared.immediate = &ToolResult{
				Content: fmt.Sprintf("before tool call: %v", err),
				IsError: true,
			}
			return prepared
		}
		if beforeResult.Block {
			reason := beforeResult.Reason
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			prepared.immediate = &ToolResult{
				Content: reason,
				IsError: true,
			}
		}
	}

	return prepared
}

func executePreparedTool(ctx context.Context, agentCtx *AgentContext, assistantMsg *Message, prepared preparedToolCall, config AgentLoopConfig, emit EventSink) *ToolResult {
	result, err := prepared.tool.Execute(ToolContext{
		Context:    ctx,
		ToolCallID: prepared.toolCall.ID,
		Name:       prepared.toolCall.Name,
		Args:       prepared.args,
		OnUpdate: func(partial *ToolResult) {
			if partial == nil {
				return
			}
			emitToolUpdate(prepared.toolCall, prepared.args, partial, emit)
		},
		SessionCWD:   agentCtx.SessionCWD,
		Capabilities: agentCtx.Capabilities,
	})
	if err != nil {
		result = &ToolResult{
			Content: err.Error(),
			IsError: true,
		}
	}
	if result == nil {
		result = &ToolResult{Content: ""}
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
		// Apply override (matches TypeScript: content ?? result.content)
		if len(afterResult.ContentBlocks) > 0 {
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
		// If hook threw, override with error result
		if afterErr != nil {
			result = &ToolResult{
				Content: afterErr.Error(),
				IsError: true,
			}
		}
	}

	return result
}

func finalizeToolResult(toolCall ai.ToolCall, toolResult *ToolResult, emit EventSink) *ToolResult {
	if toolResult == nil {
		toolResult = &ToolResult{Content: ""}
	}

	emit(ToolEnd{
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
		Role:       RoleTool,
		ToolCallID: toolCall.ID,
		ToolName:   toolCall.Name,
		Content:    partial.NormalizedContent(),
		IsError:    partial.IsError,
		Timestamp:  time.Now(),
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
		Name:        tool.Name,
		Description: tool.Description,
		Parameters:  params,
	}, nil
}

// messageFromAI converts an AI AssistantMessage to an agent Message.
func messageFromAI(msg *ai.AssistantMessage) *Message {
	isError := msg.StopReason == ai.StopReasonError || msg.StopReason == ai.StopReasonAborted
	return &Message{
		Role:         RoleAssistant,
		Content:      msg.Content,
		IsError:      isError,
		API:          msg.API,
		Provider:     msg.Provider,
		Model:        msg.Model,
		StopReason:   msg.StopReason,
		Usage:        msg.Usage,
		ErrorMessage: msg.ErrorMessage,
		ResponseID:   msg.ResponseID,
		Timestamp:    time.UnixMilli(msg.Timestamp),
	}
}

// messageToAI converts an agent Message to an AI Message.
func messageToAI(msg *Message) ai.Message {
	role := string(msg.Role)
	if msg.Role == RoleTool {
		role = "toolResult"
	}

	return ai.Message{
		Role:       role,
		Content:    msg.Content,
		ToolCallID: msg.ToolCallID,
		ToolName:   msg.ToolName,
		IsError:    msg.IsError,
		Timestamp:  msg.Timestamp.UnixMilli(),
	}
}

// ConvertMessagesToAI converts agent messages to AI messages.
func ConvertMessagesToAI(messages []*Message) []ai.Message {
	result := make([]ai.Message, 0, len(messages))
	for _, m := range messages {
		if m.ExcludeFromContext {
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
