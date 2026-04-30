package agent // import "github.com/losinggeneration/tau/agent"

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/losinggeneration/tau/ai"
)

// Agent is the main agent runtime.
type Agent struct {
	config Config

	// State
	state   State
	handler StreamHandler
	mu      sync.RWMutex

	// Event listeners
	listeners      []eventListener
	nextListenerID int

	// Queues
	steeringQueue []*Message
	followUpQueue []*Message

	// Control
	activeRun    *activeRun
	steeringMode QueueMode
	followUpMode QueueMode
}

// activeRun represents an ongoing agent run.
type activeRun struct {
	promise   chan struct{}
	abortCtrl context.CancelFunc
	abortCh   chan struct{}
	aborted   bool
}

type runExecutor func(ctx context.Context) error

type eventListener struct {
	id int
	fn func(event Event, signal <-chan struct{})
}

// New creates a new agent with the given configuration.
func New(config Config) (*Agent, error) {
	// Set defaults
	if config.ToolExecution == "" {
		config.ToolExecution = ToolExecutionParallel
	}
	if config.SteeringMode == "" {
		config.SteeringMode = QueueModeOneAtATime
	}
	if config.FollowUpMode == "" {
		config.FollowUpMode = QueueModeOneAtATime
	}

	// Set default ConvertToLlm if not provided
	if config.ConvertToLlm == nil {
		config.ConvertToLlm = defaultConvertToLlm
	}

	return &Agent{
		config: config,
		state: State{
			Model:          config.Model,
			SystemPrompt:   config.SystemPrompt,
			Messages:       slicesClone(config.InitialMessages),
			Tools:          append([]Tool(nil), config.Tools...),
			ThinkingLevel:  config.ThinkingLevel,
			PendingToolIDs: make(map[string]struct{}),
			SessionID:      config.SessionID,
			SessionCWD:     config.SessionCWD,
			Capabilities:   config.Capabilities,
		},
		handler:      config.StreamHandler,
		steeringMode: config.SteeringMode,
		followUpMode: config.FollowUpMode,
	}, nil
}

// convertToLlm is the function used to convert agent messages to LLM format.
// Stored as a field so it can be accessed within the agent.
func (a *Agent) convertToLlm(ctx context.Context, messages []*Message) ([]ai.Message, error) {
	if a.config.ConvertToLlm != nil {
		return a.config.ConvertToLlm(ctx, messages)
	}
	return defaultConvertToLlm(ctx, messages)
}

// safeConvertToLlm wraps a ConvertToLlmFunc so that internal errors return
// an empty message list (the spec's safe fallback for the conversion
// callback). The agent loop relies on this so a buggy user-supplied
// converter cannot abort the run.
func safeConvertToLlm(convert ConvertToLlmFunc) ConvertToLlmFunc {
	if convert == nil {
		return nil
	}
	return func(ctx context.Context, messages []*Message) ([]ai.Message, error) {
		result, err := convert(ctx, messages)
		if err != nil {
			return []ai.Message{}, nil
		}
		return result, nil
	}
}

// safeTransformContext wraps a TransformContextFunc so that internal
// errors return the original messages (the spec's safe fallback for the
// context-transform callback). The agent loop relies on this so a buggy
// user-supplied transform cannot abort the run.
func safeTransformContext(transform TransformContextFunc) TransformContextFunc {
	if transform == nil {
		return nil
	}
	return func(ctx context.Context, messages []*Message) ([]*Message, error) {
		result, err := transform(ctx, messages)
		if err != nil {
			return messages, nil
		}
		return result, nil
	}
}

// safeAPIKeyProvider wraps a dynamic API-key provider so that internal
// errors return the empty string (the spec's safe fallback for an
// absent key). The agent loop relies on this so a buggy user-supplied
// provider cannot abort the run.
func safeAPIKeyProvider(provider func(ctx context.Context, providerName string) (string, error)) func(ctx context.Context, providerName string) (string, error) {
	if provider == nil {
		return nil
	}
	return func(ctx context.Context, providerName string) (string, error) {
		key, err := provider(ctx, providerName)
		if err != nil {
			return "", nil
		}
		return key, nil
	}
}

// safeMessageSupplier wraps a MessageSupplierFunc (steering or follow-up)
// so that internal errors return an empty list (the spec's safe fallback
// for queue providers). The agent loop relies on this so a buggy
// user-supplied supplier cannot abort the run.
func safeMessageSupplier(supplier MessageSupplierFunc) MessageSupplierFunc {
	if supplier == nil {
		return nil
	}
	return func(ctx context.Context) ([]*Message, error) {
		messages, err := supplier(ctx)
		if err != nil {
			return nil, nil
		}
		return messages, nil
	}
}

// defaultConvertToLlm converts agent messages to LLM format (filters out non-LLM message types).
func defaultConvertToLlm(_ context.Context, messages []*Message) ([]ai.Message, error) {
	result := make([]ai.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.ExcludeFromContext {
			continue
		}
		role := string(msg.Role)
		if msg.Role == RoleTool {
			role = "toolResult"
		}
		switch msg.Role {
		case RoleUser, RoleAssistant, RoleTool, RoleSystem:
			result = append(result, ai.Message{
				Role:       role,
				Content:    msg.Content,
				ToolCallID: msg.ToolCallID,
				ToolName:   msg.ToolName,
				IsError:    msg.IsError,
				Timestamp:  msg.Timestamp.UnixMilli(),
			})
		}
	}
	return result, nil
}

// Subscribe adds a listener for agent events.
// Returns an unsubscribe function.
func (a *Agent) Subscribe(listener func(event Event, signal <-chan struct{})) (unsubscribe func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextListenerID++
	id := a.nextListenerID
	a.listeners = append(a.listeners, eventListener{id: id, fn: listener})
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		for i, l := range a.listeners {
			if l.id == id {
				a.listeners = append(a.listeners[:i], a.listeners[i+1:]...)
				return
			}
		}
	}
}

// emit sends an event to all listeners.
func (a *Agent) emit(event Event) {
	a.mu.RLock()
	listeners := a.listeners
	signal := a.currentAbortSignal()
	handler := a.handler
	a.mu.RUnlock()

	if handler != nil {
		handler(event)
	}

	for _, listener := range listeners {
		listener.fn(event, signal)
	}
}

// currentAbortSignal returns the current abort signal if a run is active.
func (a *Agent) currentAbortSignal() <-chan struct{} {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.activeRun != nil {
		return a.activeRun.abortCh
	}
	return nil
}

type promptOptions struct {
	transform      TransformContextFunc
	systemAddendum string
}

// PromptOption configures a single Prompt call.
type PromptOption func(*promptOptions)

// WithPromptContext applies an additional context transform for one Prompt call.
func WithPromptContext(transform TransformContextFunc) PromptOption {
	return func(opts *promptOptions) { opts.transform = transform }
}

// WithPromptSystemAddendum appends additional system context for one Prompt call.
func WithPromptSystemAddendum(addendum string) PromptOption {
	return func(opts *promptOptions) { opts.systemAddendum = addendum }
}

func applyPromptOptions(opts []PromptOption) promptOptions {
	var out promptOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&out)
		}
	}
	return out
}

// Prompt sends a message to the agent and processes the response.
func (a *Agent) Prompt(ctx context.Context, msg *Message, opts ...PromptOption) error {
	po := applyPromptOptions(opts)
	return a.PromptMessages(ctx, []*Message{msg}, optsFromPromptOptions(po)...)
}

func optsFromPromptOptions(po promptOptions) []PromptOption {
	return []PromptOption{
		WithPromptContext(po.transform),
		WithPromptSystemAddendum(po.systemAddendum),
	}
}

// PromptMessages sends a batch of messages to the agent and processes the response.
func (a *Agent) PromptMessages(ctx context.Context, msgs []*Message, opts ...PromptOption) error {
	po := applyPromptOptions(opts)
	a.mu.Lock()
	if a.activeRun != nil {
		a.mu.Unlock()
		return ErrAlreadyProcessingPrompt
	}
	a.mu.Unlock()

	return a.runPromptMessages(ctx, msgs, false, po.transform, po.systemAddendum)
}

// Continue continues the agent from its current state.
func (a *Agent) Continue(ctx context.Context) error {
	a.mu.Lock()
	if a.activeRun != nil {
		a.mu.Unlock()
		return ErrAlreadyProcessing
	}

	if len(a.state.Messages) == 0 {
		a.mu.Unlock()
		return ErrNoMessagesToContinue
	}
	lastMsg := a.state.Messages[len(a.state.Messages)-1]

	if lastMsg.Role == RoleAssistant {
		// Check steering queue first
		if len(a.steeringQueue) > 0 {
			messages := a.drainSteering()
			a.mu.Unlock()
			return a.runPromptMessages(ctx, messages, true, nil, "")
		}

		// Check follow-up queue
		if len(a.followUpQueue) > 0 {
			messages := a.drainFollowUp()
			a.mu.Unlock()
			return a.runPromptMessages(ctx, messages, false, nil, "")
		}

		a.mu.Unlock()
		return ErrCannotContinueAssistant
	}

	a.mu.Unlock()

	return a.runContinuation(ctx)
}

// steer queues a message to be injected after the current assistant turn finishes.
func (a *Agent) Steer(msg *Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue = append(a.steeringQueue, msg)
}

// followUp queues a message to run only after the agent would otherwise stop.
func (a *Agent) FollowUp(msg *Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQueue = append(a.followUpQueue, msg)
}

// ClearSteeringQueue removes all queued steering messages.
func (a *Agent) ClearSteeringQueue() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue = nil
}

// ClearFollowUpQueue removes all queued follow-up messages.
func (a *Agent) ClearFollowUpQueue() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQueue = nil
}

// ClearAllQueues removes all queued messages.
func (a *Agent) ClearAllQueues() {
	a.ClearSteeringQueue()
	a.ClearFollowUpQueue()
}

// HasQueuedMessages returns true when either queue contains pending messages.
func (a *Agent) HasQueuedMessages() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.steeringQueue) > 0 || len(a.followUpQueue) > 0
}

// QueueSizes returns the current length of the steering and follow-up queues.
// Used by the SDK to write queue_update entries to the session log.
func (a *Agent) QueueSizes() (steering, followUp int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.steeringQueue), len(a.followUpQueue)
}

// Signal returns the active abort signal for the current run, if any.
func (a *Agent) Signal() <-chan struct{} {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.activeRun != nil {
		return a.activeRun.abortCh
	}
	return nil
}

// Abort stops the current run.
func (a *Agent) Abort() {
	a.mu.Lock()
	if a.activeRun != nil {
		a.activeRun.aborted = true
		select {
		case <-a.activeRun.abortCh:
		default:
			close(a.activeRun.abortCh)
		}
		a.activeRun.abortCtrl()
	}
	a.mu.Unlock()
}

// WaitForIdle blocks until the current run finishes.
func (a *Agent) WaitForIdle() {
	a.mu.RLock()
	promise := a.activeRun
	a.mu.RUnlock()

	if promise != nil {
		<-promise.promise
	}
}

// Reset clears transcript state, runtime state, and queued messages.
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Messages = nil
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolIDs = make(map[string]struct{})
	a.state.Error = nil
	a.state.ErrorMessage = ""
	a.steeringQueue = nil
	a.followUpQueue = nil
}

// State returns the current agent state.
func (a *Agent) State() State {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return *a.state.Clone()
}

// Messages returns a copy of messages.
func (a *Agent) Messages() []*Message {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return slicesClone(a.state.Messages)
}

// SetSteeringMode sets how queued steering messages are drained.
func (a *Agent) SetSteeringMode(mode QueueMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringMode = mode
}

// SetFollowUpMode sets how queued follow-up messages are drained.
func (a *Agent) SetFollowUpMode(mode QueueMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpMode = mode
}

// drainSteering drains the steering queue based on the current mode.
func (a *Agent) drainSteering() []*Message {
	if a.steeringMode == QueueModeAll {
		messages := a.steeringQueue
		a.steeringQueue = nil
		return messages
	}
	if len(a.steeringQueue) > 0 {
		msg := a.steeringQueue[0]
		a.steeringQueue = a.steeringQueue[1:]
		return []*Message{msg}
	}
	return nil
}

// drainFollowUp drains the follow-up queue based on the current mode.
func (a *Agent) drainFollowUp() []*Message {
	if a.followUpMode == QueueModeAll {
		messages := a.followUpQueue
		a.followUpQueue = nil
		return messages
	}
	if len(a.followUpQueue) > 0 {
		msg := a.followUpQueue[0]
		a.followUpQueue = a.followUpQueue[1:]
		return []*Message{msg}
	}
	return nil
}

// runPromptMessages runs the low-level agent loop with new prompt messages.
func (a *Agent) runPromptMessages(
	ctx context.Context,
	messages []*Message,
	skipInitialSteeringPoll bool,
	transform TransformContextFunc,
	systemAddendum string,
) error {
	return a.runWithLifecycle(ctx, func(runCtx context.Context) error {
		_, err := RunAgentLoop(
			runCtx,
			messages,
			a.createContextSnapshot(),
			a.createLoopConfig(skipInitialSteeringPoll, transform, systemAddendum),
			a.processEvent,
		)
		return err
	})
}

func (a *Agent) runContinuation(ctx context.Context) error {
	return a.runWithLifecycle(ctx, func(runCtx context.Context) error {
		_, err := RunAgentLoopContinue(
			runCtx,
			a.createContextSnapshot(),
			a.createLoopConfig(false, nil, ""),
			a.processEvent,
		)
		return err
	})
}

func (a *Agent) runWithLifecycle(ctx context.Context, exec runExecutor) error {
	runCtx, run := a.beginRun(ctx)

	var runErr error
	defer func() {
		a.finishRun(run, runErr)
	}()

	runErr = exec(runCtx)
	if runErr != nil {
		a.emitRunFailure(runErr)
	}

	return runErr
}

func (a *Agent) beginRun(ctx context.Context) (context.Context, *activeRun) {
	runCtx, abortCtrl := context.WithCancel(ctx)
	run := &activeRun{
		promise:   make(chan struct{}),
		abortCtrl: abortCtrl,
		abortCh:   make(chan struct{}),
	}

	a.mu.Lock()
	a.activeRun = run
	a.state.IsStreaming = true
	a.state.StreamingMessage = nil
	a.state.Error = nil
	a.state.ErrorMessage = ""
	a.state.PendingToolIDs = make(map[string]struct{})
	a.mu.Unlock()

	return runCtx, run
}

func (a *Agent) finishRun(run *activeRun, runErr error) {
	a.mu.Lock()
	a.activeRun = nil
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolIDs = make(map[string]struct{})
	if runErr != nil {
		a.state.Error = runErr
		a.state.ErrorMessage = runErr.Error()
	}
	a.mu.Unlock()

	close(run.promise)
	run.abortCtrl()
}

// slicesClone clones a slice of messages.
func slicesClone[T any](s []T) []T {
	if s == nil {
		return nil
	}
	result := make([]T, len(s))
	copy(result, s)
	return result
}

func (a *Agent) createContextSnapshot() *AgentContext {
	return &AgentContext{
		Model:        a.state.Model,
		SystemPrompt: a.state.SystemPrompt,
		Messages:     slicesClone(a.state.Messages),
		Tools:        append([]Tool(nil), a.state.Tools...),
		SessionCWD:   a.state.SessionCWD,
		Capabilities: a.state.Capabilities,
	}
}

func (a *Agent) createLoopConfig(skipInitialSteeringPoll bool, transform TransformContextFunc, systemAddendum string) AgentLoopConfig {
	skipInitial := skipInitialSteeringPoll
	mergedAddendum := a.config.SystemAddendum
	if systemAddendum != "" {
		if mergedAddendum != "" {
			mergedAddendum += "\n\n" + systemAddendum
		} else {
			mergedAddendum = systemAddendum
		}
	}

	return AgentLoopConfig{
		Model:            a.state.Model,
		ClientFactory:    a.config.ClientFactory,
		StreamFn:         a.config.StreamFn,
		ConvertToLlm:     safeConvertToLlm(a.convertToLlm),
		SystemAddendum:   mergedAddendum,
		GetAPIKey:        safeAPIKeyProvider(a.config.APIKeyProvider),
		TransformContext: safeTransformContext(composeTransforms(transform, a.config.TransformContext)),
		OnPayload:        a.config.OnPayload,
		OnResponse:       a.config.OnResponse,
		ToolExecution:    a.config.ToolExecution,
		BeforeToolCall: func(callCtx context.Context, ctx ToolCallContext) (BeforeToolCallResult, error) {
			if a.config.BeforeToolCall == nil {
				return BeforeToolCallResult{}, nil
			}
			return a.config.BeforeToolCall(callCtx, ctx)
		},
		AfterToolCall: func(callCtx context.Context, ctx ToolCallContext, result *ToolResult) (AfterToolCallResult, error) {
			if a.config.AfterToolCall == nil {
				return AfterToolCallResult{}, nil
			}
			return a.config.AfterToolCall(callCtx, ctx, result)
		},
		ThinkingLevel:   a.state.ThinkingLevel,
		Transport:       a.config.Transport,
		ThinkingBudgets: a.config.ThinkingBudgets,
		SessionID:       a.state.SessionID,
		APIKey:          "",
		MaxRetryDelayMs: a.config.MaxRetryDelayMs,
		GetSteeringMessages: safeMessageSupplier(func(ctx context.Context) ([]*Message, error) {
			if skipInitial {
				skipInitial = false
				return nil, nil
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.drainSteering(), nil
		}),
		GetFollowUpMessages: safeMessageSupplier(func(ctx context.Context) ([]*Message, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.drainFollowUp(), nil
		}),
		EventDispatcher: a.config.EventDispatcher,
		Tools:           append([]Tool(nil), a.state.Tools...),
		SessionCWD:      a.state.SessionCWD,
		Capabilities:    a.state.Capabilities,
	}
}

// emitRunFailure emits run failure events and adds the failure message to state.Messages.
func (a *Agent) emitRunFailure(err error) {
	stopReason := ai.StopReasonError
	if errors.Is(err, context.Canceled) {
		stopReason = ai.StopReasonAborted
	}
	var apiName, providerName, modelID string
	if a.state.Model != nil {
		apiName = a.state.Model.API
		providerName = a.state.Model.Provider
		modelID = a.state.Model.ID
	}

	msg := &Message{
		Role:         RoleAssistant,
		Content:      []ai.ContentBlock{{Type: ai.ContentBlockText, Text: ""}},
		IsError:      true,
		API:          apiName,
		Provider:     providerName,
		Model:        modelID,
		StopReason:   stopReason,
		ErrorMessage: err.Error(),
		Timestamp:    time.Now(),
	}

	a.mu.Lock()
	a.state.Messages = append(a.state.Messages, msg)
	a.state.Error = err
	a.state.ErrorMessage = err.Error()
	a.mu.Unlock()

	a.processEvent(AgentEnd{Messages: []*Message{msg}})
}

func composeTransforms(primary, secondary TransformContextFunc) TransformContextFunc {
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}

	return func(ctx context.Context, messages []*Message) ([]*Message, error) {
		transformed, err := primary(ctx, messages)
		if err != nil {
			return nil, err
		}

		return secondary(ctx, transformed)
	}
}

func (a *Agent) processEvent(event Event) {
	a.mu.Lock()
	switch e := event.(type) {
	case AgentStart:
	case AgentEnd:
		_ = e
	case MessageStart:
		if e.Message != nil && e.Message.Role == RoleAssistant {
			a.state.StreamingMessage = e.Message
		}
	case MessageUpdate:
		a.state.StreamingMessage = e.Message
	case MessageEnd:
		if e.Message != nil {
			if e.Message.Role == RoleAssistant && isEmptyAssistantResponse(e.Message) {
				break
			}
			a.state.Messages = append(a.state.Messages, e.Message)
			if e.Message.Role == RoleAssistant {
				a.state.StreamingMessage = nil
			}
		}
	case ToolStart:
		a.state.PendingToolIDs[e.ToolCallID] = struct{}{}
	case ToolUpdate:
	case ToolEnd:
		delete(a.state.PendingToolIDs, e.ToolCallID)
	}
	a.mu.Unlock()

	a.emit(event)
}

// AddTools adds tools to the agent.
func (a *Agent) AddTools(tools ...Tool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Tools = append(a.state.Tools, tools...)
}

// SetModel replaces the active model for future turns.
func (a *Agent) SetModel(model *ai.Model) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Model = model
}

// SetTools replaces the tool list for future turns.
func (a *Agent) SetTools(tools ...Tool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Tools = append([]Tool(nil), tools...)
}

// SetSystemPrompt sets the system prompt.
func (a *Agent) SetSystemPrompt(prompt string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.SystemPrompt = prompt
}

// SetThinkingLevel sets the thinking level.
func (a *Agent) SetThinkingLevel(level ThinkingLevel) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.ThinkingLevel = level
}

// ReplaceMessages replaces the transcript stored on the agent.
func (a *Agent) ReplaceMessages(messages []*Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Messages = slicesClone(messages)
}

// Complete is a single-call helper that runs the agent with a prompt and waits
// for completion. It matches pi-mono's complete() ergonomics.
func Complete(ctx context.Context, prompt string, tools ...Tool) (*State, error) {
	config := NewConfig()
	config.Tools = tools
	config.StreamHandler = nil // suppress streaming events for Complete

	agent, err := New(config)
	if err != nil {
		return nil, fmt.Errorf("agent.Complete: %w", err)
	}

	msg := NewUserMessage(prompt)
	if err := agent.Prompt(ctx, msg); err != nil {
		return nil, fmt.Errorf("agent.Complete: %w", err)
	}

	state := agent.State()
	return &state, nil
}
