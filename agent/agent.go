package agent // import "github.com/losinggeneration/tau/agent"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
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

	// messagesVersion increments on every external transcript replacement
	// (ReplaceMessages/Reset) so a running loop can detect and rebase onto it.
	messagesVersion uint64

	toolRegistryBaseline []Tool
	toolRegistryChanged  bool
	pendingActiveTools   []string
	pendingSections      map[string]*string
	activeToolsChanged   bool

	// Event listeners
	listeners      []eventListener
	nextListenerID int

	// Queues
	steeringQueue         []*Message
	followUpQueue         []*Message
	pendingCustomMessages []*Message

	// Control
	modelChanged chan struct{}
	activeRun    *activeRun
	settlingRuns map[*activeRun]struct{}
	steeringMode QueueMode
	followUpMode QueueMode
}

// activeRun represents an ongoing agent run.
type activeRun struct {
	ctx       context.Context
	promise   chan struct{}
	abortCtrl context.CancelFunc
	failCtrl  context.CancelCauseFunc
	abortCh   chan struct{}
	aborted   bool
	// listenerErrs holds recovered listener panics; the first one ends the run.
	listenerErrs []error
}

type runExecutor func(ctx context.Context) error

type eventListener struct {
	id int
	fn func(event Event, signal <-chan struct{})
}

// New creates a new agent with the given configuration.
func New(config Config) (*Agent, error) {
	if initial := config.InitialState; initial != nil {
		config.Model = initial.Model
		config.SystemPrompt = initial.SystemPrompt
		config.InitialMessages = initial.Messages
		config.Tools = initial.Tools
		config.ThinkingLevel = initial.ThinkingLevel
	}
	if len(config.InitialMessages) > 0 && config.InitialMessages[0] != nil && config.InitialMessages[0].Role == RoleSystem && !config.InitialMessages[0].ExcludeFromContext {
		config.SystemPrompt = ""
	}

	if (config.SystemPrompt != "" || len(config.Tools) > 0) && (len(config.InitialMessages) == 0 || config.InitialMessages[0] == nil || config.InitialMessages[0].Role != RoleSystem || config.InitialMessages[0].ExcludeFromContext) {
		declarations, err := visibleToolDeclarations(config.Tools)
		if err != nil {
			return nil, fmt.Errorf("initial tool declarations: %w", err)
		}
		baseline := &Message{Role: RoleSystem, Timestamp: time.UnixMilli(0), Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: config.SystemPrompt}}, ToolsAdded: declarations}
		config.InitialMessages = append([]*Message{baseline}, config.InitialMessages...)
		config.SystemPrompt = ""
	}

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
		modelChanged: make(chan struct{}),
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

// defaultConvertToLlm converts agent messages to LLM format (filters out non-LLM message types).
func defaultConvertToLlm(ctx context.Context, messages []*Message) ([]ai.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]ai.Message, 0, len(messages))
	for _, msg := range messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if msg == nil || msg.ExcludeFromContext {
			continue
		}
		switch msg.Role {
		case RoleCustom:
			converted := messageToAI(msg)
			converted.Role = "user"
			result = append(result, converted)
		case RoleUser, RoleAssistant, RoleTool, "toolResult", RoleSystem:
			result = append(result, messageToAI(msg))
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

// emit sends an event to all listeners. A panicking listener stops delivery of
// this event to later listeners.
func (a *Agent) emit(event Event) error {
	a.mu.RLock()
	listeners := slices.Clone(a.listeners)
	var signal <-chan struct{}
	if a.activeRun != nil {
		signal = a.activeRun.abortCh
	}
	handler := a.handler
	a.mu.RUnlock()

	if handler != nil {
		if err := callListener(func() { handler(event) }); err != nil {
			return err
		}
	}

	for _, listener := range listeners {
		if err := callListener(func() { listener.fn(event, signal) }); err != nil {
			return err
		}
	}
	return nil
}

func callListener(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("%w: %w", ErrListenerPanic, e)
			} else {
				err = fmt.Errorf("%w: %v", ErrListenerPanic, r)
			}
		}
	}()
	fn()
	return nil
}

// recordListenerFailure ends the active run at its next cancellation boundary.
// Outside a run, the caller's own publication path reports the error instead.
func (a *Agent) recordListenerFailure(err error) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	run := a.activeRun
	if run == nil {
		return false
	}
	run.listenerErrs = append(run.listenerErrs, err)
	if len(run.listenerErrs) == 1 {
		run.failCtrl(err)
	}
	return true
}

func (a *Agent) runListenerErrs(run *activeRun) []error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return slices.Clone(run.listenerErrs)
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

// Continue continues the agent from its current state, resuming a run that
// was interrupted or failed.
func (a *Agent) Continue(ctx context.Context) error {
	a.mu.Lock()
	if a.activeRun != nil {
		a.mu.Unlock()
		return ErrAlreadyProcessing
	}

	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return errors.Join(err, context.Cause(ctx))
	}

	// Failed/aborted assistant messages are never replayed to the model, so
	// a run that ended on one resumes from the message before it.
	for n := len(a.state.Messages); n > 0; n = len(a.state.Messages) {
		last := a.state.Messages[n-1]
		if last == nil {
			a.state.Messages = a.state.Messages[:n-1]
			continue
		}
		if last.Role != RoleAssistant || (last.StopReason != ai.StopReasonError && last.StopReason != ai.StopReasonAborted) {
			break
		}
		a.state.Messages = a.state.Messages[:n-1]
	}

	if len(a.state.Messages) == 0 || !slices.ContainsFunc(a.state.Messages, func(message *Message) bool {
		return message != nil && message.Role != RoleSystem
	}) {
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

// AppendCustomMessage appends context without starting a provider request. During a
// run, delivery waits until the current assistant turn and its tools finish.
func (a *Agent) AppendCustomMessage(ctx context.Context, message *Message) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("append custom message: %w", err)
	}
	if message == nil || message.Role != RoleCustom {
		return errors.New("append custom message: custom role required")
	}
	a.mu.Lock()
	if a.activeRun != nil {
		a.pendingCustomMessages = append(a.pendingCustomMessages, message)
		a.mu.Unlock()
		return nil
	}
	a.state.Messages = append(a.state.Messages, message)
	a.messagesVersion++
	a.mu.Unlock()
	if err := a.emitCustomMessages([]*Message{message}); err != nil {
		return fmt.Errorf("append custom message: %w", err)
	}
	return nil
}

// QueueCustomMessage admits context only while a run owns the turn boundary.
// A false result leaves delivery to the caller's idle publication path.
func (a *Agent) QueueCustomMessage(ctx context.Context, message *Message) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("queue custom message: %w", err)
	}
	if message == nil || message.Role != RoleCustom {
		return false, errors.New("queue custom message: custom role required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeRun == nil {
		return false, nil
	}
	a.pendingCustomMessages = append(a.pendingCustomMessages, message)
	return true, nil
}

func (a *Agent) flushCustomMessages() {
	a.mu.Lock()
	messages := a.takeCustomMessagesLocked()
	a.mu.Unlock()
	if err := a.emitCustomMessages(messages); err != nil {
		a.recordListenerFailure(err)
	}
}

func (a *Agent) takeCustomMessagesLocked() []*Message {
	messages := a.pendingCustomMessages
	a.pendingCustomMessages = nil
	if len(messages) > 0 {
		a.state.Messages = append(a.state.Messages, messages...)
		a.messagesVersion++
	}
	return messages
}

func (a *Agent) emitCustomMessages(messages []*Message) error {
	var errs []error
	for _, message := range messages {
		errs = append(errs, a.emit(MessageStart{Message: message}), a.emit(MessageEnd{Message: message}))
	}
	return errors.Join(errs...)
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

// PeekQueuedMessages returns queue snapshots without consuming pending messages.
func (a *Agent) PeekQueuedMessages() (steering, followUp []*Message) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return slicesClone(a.steeringQueue), slicesClone(a.followUpQueue)
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
	for {
		a.mu.RLock()
		run := a.activeRun
		if run == nil {
			for settling := range a.settlingRuns {
				run = settling
				break
			}
		}
		a.mu.RUnlock()
		if run == nil {
			return
		}
		<-run.promise
	}
}

// Reset clears transcript state, runtime state, and queued messages when idle.
func (a *Agent) Reset() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeRun != nil {
		return ErrAlreadyProcessing
	}
	var transcript []ai.Message
	for _, message := range a.state.Messages {
		if message != nil && message.Role == RoleSystem && !message.ExcludeFromContext {
			transcript = append(transcript, messageToAI(message))
		}
	}
	a.state.Messages = nil
	if baseline := ai.CurrentSystemMessage(transcript); baseline != nil {
		// Keep removals relative to the separate executor registry across reset.
		removed := make(map[string]bool)
		for _, message := range transcript {
			for _, tool := range message.ToolsRemoved {
				removed[tool.Name] = true
			}
			for _, tool := range message.ToolsAdded {
				delete(removed, tool.Name)
			}
		}
		var removals []ai.ToolReference
		for name := range removed {
			removals = append(removals, ai.ToolReference{Name: name})
		}
		slices.SortFunc(removals, func(a, b ai.ToolReference) int { return strings.Compare(a.Name, b.Name) })
		a.state.Messages = []*Message{{Role: RoleSystem, Content: []ai.ContentBlock{{Type: ai.ContentBlockText, Text: baseline.Content.(string)}}, Sections: baseline.Sections, ToolsAdded: baseline.ToolsAdded, ToolsRemoved: removals, Timestamp: time.UnixMilli(baseline.Timestamp)}}
	}
	a.messagesVersion++
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolIDs = make(map[string]struct{})
	a.state.Error = nil
	a.state.ErrorMessage = ""
	a.steeringQueue = nil
	a.followUpQueue = nil
	a.pendingCustomMessages = nil
	return nil
}

// State returns the current agent state.
func (a *Agent) State() State {
	a.mu.RLock()
	defer a.mu.RUnlock()
	state := a.state.Clone()
	transcript := ai.NormalizeContext(&ai.Context{SystemPrompt: state.SystemPrompt, Messages: ConvertMessagesToAI(state.Messages)})
	state.SystemPrompt = ai.CurrentSystemPrompt(transcript.Messages)
	return *state
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

func (a *Agent) runWithLifecycle(ctx context.Context, exec runExecutor) (runErr error) {
	runCtx, run, err := a.beginRun(ctx)
	if err != nil {
		return err
	}

	defer func() {
		runErr = errors.Join(runErr, a.finishRun(run, runErr))
	}()

	runErr = exec(runCtx)
	if listenerErrs := a.runListenerErrs(run); len(listenerErrs) > 0 {
		// A listener failure cancels the run to stop the loop; that
		// cancellation is not a user abort, so only other loop errors are kept.
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			listenerErrs = append(listenerErrs, runErr)
		}
		runErr = errors.Join(listenerErrs...)
	} else if runErr != nil {
		if err := runCtx.Err(); err != nil {
			runErr = errors.Join(runErr, err, context.Cause(runCtx))
		}
	}
	if runErr == nil {
		return nil
	}

	failed := len(a.runListenerErrs(run))
	a.emitRunFailure(runCtx, runErr)
	if later := a.runListenerErrs(run)[failed:]; len(later) > 0 {
		runErr = errors.Join(append([]error{runErr}, later...)...)
	}
	return runErr
}

func (a *Agent) beginRun(ctx context.Context) (context.Context, *activeRun, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeRun != nil {
		return nil, nil, ErrAlreadyProcessing
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	run := &activeRun{
		ctx:       runCtx,
		promise:   make(chan struct{}),
		abortCtrl: func() { cancel(nil) },
		failCtrl:  cancel,
		abortCh:   make(chan struct{}),
	}

	a.activeRun = run
	a.state.IsStreaming = true
	a.state.StreamingMessage = nil
	a.state.Error = nil
	a.state.ErrorMessage = ""
	a.state.PendingToolIDs = make(map[string]struct{})

	return runCtx, run, nil
}

func (a *Agent) finishRun(run *activeRun, runErr error) (err error) {
	a.mu.Lock()
	messages := a.takeCustomMessagesLocked()
	if a.settlingRuns == nil {
		a.settlingRuns = make(map[*activeRun]struct{})
	}
	a.settlingRuns[run] = struct{}{}
	a.activeRun = nil
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolIDs = make(map[string]struct{})
	if runErr != nil {
		a.state.Error = runErr
		a.state.ErrorMessage = runErr.Error()
	}
	sessionID := a.state.SessionID
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.settlingRuns, run)
		close(run.promise)
		a.mu.Unlock()
	}()
	defer run.abortCtrl()
	if err = a.emitCustomMessages(messages); err != nil {
		a.mu.Lock()
		a.state.Error = errors.Join(a.state.Error, err)
		a.state.ErrorMessage = a.state.Error.Error()
		a.mu.Unlock()
	}
	if a.config.EventDispatcher != nil {
		payload, marshalErr := json.Marshal(map[string]any{"aborted": errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded)})
		if marshalErr != nil {
			a.mu.Lock()
			a.state.Error = marshalErr
			a.state.ErrorMessage = marshalErr.Error()
			a.mu.Unlock()
			return err
		}
		a.config.EventDispatcher(run.ctx, "agent_settled", payload, sessionID)
	}
	return err
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
	a.mu.RLock()
	defer a.mu.RUnlock()
	return &AgentContext{
		Model:           a.state.Model,
		SystemPrompt:    a.state.SystemPrompt,
		Messages:        slicesClone(a.state.Messages),
		MessagesVersion: a.messagesVersion,
		Tools:           append([]Tool(nil), a.state.Tools...),
		SessionCWD:      a.state.SessionCWD,
		Capabilities:    a.state.Capabilities,
	}
}

func (a *Agent) createLoopConfig(skipInitialSteeringPoll bool, transform TransformContextFunc, systemAddendum string) AgentLoopConfig {
	a.mu.RLock()
	defer a.mu.RUnlock()
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
		deferFailureEnd:       true,
		GetStartupMessages:    a.config.GetStartupMessages,
		RequestOptions:        slices.Clone(a.config.RequestOptions),
		FlushContextMessages:  a.flushCustomMessages,
		PrepareNextTurn:       a.config.PrepareNextTurn,
		OnProviderStreamEvent: a.config.OnProviderStreamEvent,
		PausedBy:              a.config.PausedBy,
		FinishTurn:            a.config.FinishTurn,
		PrepareRequest:        a.prepareRequest,
		Model:                 a.state.Model,
		modelSelection:        a.modelSelection,
		ClientFactory:         a.config.ClientFactory,
		StreamFn:              a.config.StreamFn,
		ConvertToLlm:          a.convertToLlm,
		SystemAddendum:        mergedAddendum,
		GetAPIKey:             a.config.APIKeyProvider,
		ProviderEnv:           a.config.ProviderEnv,
		RequestCredentials:    a.config.RequestCredentials,
		TransformContext:      composeTransforms(transform, a.config.TransformContext),
		OnPayload:             a.config.OnPayload,
		OnResponse:            a.config.OnResponse,
		ToolExecution:         a.config.ToolExecution,
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
		GetSteeringMessages: func(ctx context.Context) ([]*Message, error) {
			if skipInitial {
				skipInitial = false
				return nil, nil
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return a.drainSteering(), nil
		},
		GetFollowUpMessages: func(ctx context.Context) ([]*Message, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return a.drainFollowUp(), nil
		},
		RebasedMessages: func(seenVersion uint64) ([]*Message, uint64, bool) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.messagesVersion == seenVersion {
				return nil, seenVersion, false
			}
			return slicesClone(a.state.Messages), a.messagesVersion, true
		},
		EventDispatcher: a.config.EventDispatcher,
		Tools:           append([]Tool(nil), a.state.Tools...),
		SessionCWD:      a.state.SessionCWD,
		Capabilities:    a.state.Capabilities,
	}
}

// emitRunFailure emits run failure events and adds the failure message to state.Messages.
func (a *Agent) emitRunFailure(ctx context.Context, err error) {
	stopReason := ai.StopReasonError
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		stopReason = ai.StopReasonAborted
	}
	var apiName, providerName, modelID string
	a.mu.RLock()
	if a.state.Model != nil {
		apiName = a.state.Model.API
		providerName = a.state.Model.Provider
		modelID = a.state.Model.ID
	}
	config := AgentLoopConfig{EventDispatcher: a.config.EventDispatcher, SessionID: a.state.SessionID}
	a.mu.RUnlock()

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
	a.state.Error = err
	a.state.ErrorMessage = err.Error()
	a.mu.Unlock()

	emit := withPluginEventDispatch(ctx, config, a.processEvent)
	var published *publishedStreamFailure
	if errors.As(err, &published) {
		msg = published.message
	} else {
		emit(MessageStart{Message: msg})
		emit(MessageEnd{Message: msg})
	}
	emit(TurnEnd{Message: msg})
	if config.EventDispatcher != nil {
		config.EventDispatcher(ctx, "turn_end", nil, config.SessionID)
	}
	emit(AgentEnd{Messages: []*Message{msg}})
	if config.EventDispatcher != nil {
		config.EventDispatcher(ctx, "agent_end", nil, config.SessionID)
	}
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

		if err := ctx.Err(); err != nil {
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

	if err := a.emit(event); err != nil {
		a.recordListenerFailure(err)
	}
}

// AddTools adds tools to the agent.
func (a *Agent) AddTools(tools ...Tool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.markToolRegistryChange()
	a.state.Tools = append(a.state.Tools, tools...)
}

// SetModel replaces the active model for future turns and wakes quota retries.
func (a *Agent) SetModel(model *ai.Model) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !sameModelSelection(a.state.Model, model) {
		close(a.modelChanged)
		a.modelChanged = make(chan struct{})
	}
	a.state.Model = model
}

func (a *Agent) modelSelection() (*ai.Model, <-chan struct{}) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.state.Model, a.modelChanged
}

// SetTools replaces the tool list for future turns.
func (a *Agent) SetTools(tools ...Tool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.markToolRegistryChange()
	a.state.Tools = append([]Tool(nil), tools...)
	if a.activeToolsChanged {
		a.pendingActiveTools = slices.DeleteFunc(a.pendingActiveTools, func(name string) bool {
			return !slices.ContainsFunc(tools, func(tool Tool) bool { return tool.Name == name && modelDeclarable(tool) })
		})
	}
}

func (a *Agent) markToolRegistryChange() {
	if !a.toolRegistryChanged {
		a.toolRegistryBaseline = slices.Clone(a.state.Tools)
		a.toolRegistryChanged = true
	}
}

func (a *Agent) prepareRequest(ctx context.Context, request PrepareRequestContext) (*AgentRequestUpdate, error) {
	var update *AgentRequestUpdate
	if a.config.PrepareRequest != nil {
		var err error
		update, err = a.config.PrepareRequest(ctx, request)
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current := request.Context
	if update != nil && update.Context != nil {
		current = update.Context
	}
	a.mu.Lock()
	tools := slices.Clone(a.state.Tools)
	if update != nil && update.Context != nil {
		tools = slices.Clone(current.Tools)
	}
	var delta *Message
	if a.toolRegistryChanged {
		before, err := visibleToolDeclarations(a.toolRegistryBaseline)
		if err != nil {
			a.mu.Unlock()
			return nil, fmt.Errorf("previous tool registry: %w", err)
		}
		after, err := requestToolDeclarations(tools, current.Messages)
		if err != nil {
			a.mu.Unlock()
			return nil, fmt.Errorf("updated tool registry: %w", err)
		}
		delta = &Message{Role: RoleSystem, Timestamp: time.Now()}
		for _, old := range before {
			if !slices.ContainsFunc(after, func(tool ai.Tool) bool { return tool.Name == old.Name }) {
				delta.ToolsRemoved = append(delta.ToolsRemoved, ai.ToolReference{Name: old.Name})
			}
		}
		hasHistory := hasToolDeclarationHistory(current.Messages)
		baseline := ai.CurrentSystemMessage(ConvertMessagesToAI(current.Messages))
		removed := make(map[string]bool)
		for _, message := range current.Messages {
			if message == nil || message.Role != RoleSystem || message.ExcludeFromContext {
				continue
			}
			for _, tool := range message.ToolsRemoved {
				removed[tool.Name] = true
			}
			for _, tool := range message.ToolsAdded {
				delete(removed, tool.Name)
			}
		}
		for _, next := range after {
			if removed[next.Name] {
				continue
			}
			index := slices.IndexFunc(before, func(tool ai.Tool) bool { return tool.Name == next.Name })
			declaredIndex := -1
			if baseline != nil {
				declaredIndex = slices.IndexFunc(baseline.ToolsAdded, func(tool ai.Tool) bool { return tool.Name == next.Name })
			}
			if !hasHistory || index < 0 || (declaredIndex >= 0 && !reflect.DeepEqual(baseline.ToolsAdded[declaredIndex], next)) {
				delta.ToolsAdded = append(delta.ToolsAdded, next)
			}
		}
	}

	if a.activeToolsChanged {
		// An explicit loadout replaces registration defaults from this boundary.
		delta = &Message{Role: RoleSystem, Timestamp: time.Now()}
		baseline := ai.CurrentSystemMessage(ConvertMessagesToAI(current.Messages))
		if baseline != nil {
			for _, old := range baseline.ToolsAdded {
				if !slices.Contains(a.pendingActiveTools, old.Name) {
					delta.ToolsRemoved = append(delta.ToolsRemoved, ai.ToolReference{Name: old.Name})
				}
			}
		}
		for _, name := range a.pendingActiveTools {
			for _, tool := range tools {
				if tool.Name == name && modelDeclarable(tool) {
					declaration, err := agentToolToAITool(tool)
					if err != nil {
						a.mu.Unlock()
						return nil, fmt.Errorf("activate tool: %w", err)
					}
					delta.ToolsAdded = append(delta.ToolsAdded, declaration)
				}
			}
		}
		a.activeToolsChanged = false
		a.pendingActiveTools = nil
	}
	if len(a.pendingSections) > 0 {
		baseline := ai.CurrentSystemMessage(ConvertMessagesToAI(current.Messages))
		for name, text := range a.pendingSections {
			var have *string
			if baseline != nil {
				have = baseline.Sections[name]
			}
			if (have == nil && text == nil) || (have != nil && text != nil && *have == *text) {
				continue
			}
			if delta == nil {
				delta = &Message{Role: RoleSystem, Timestamp: time.Now()}
			}
			if delta.Sections == nil {
				delta.Sections = map[string]*string{}
			}
			delta.Sections[name] = text
		}
		a.pendingSections = nil
	}
	a.toolRegistryChanged = false
	a.toolRegistryBaseline = nil
	a.mu.Unlock()
	snapshot := *current
	snapshot.Tools = tools
	if update == nil {
		update = &AgentRequestUpdate{}
	}
	if delta != nil && (len(delta.ToolsAdded) > 0 || len(delta.ToolsRemoved) > 0 || len(delta.Sections) > 0) {
		update.Messages = append(update.Messages, delta)
	}
	update.Context = &snapshot
	return update, nil
}

func visibleToolDeclarations(tools []Tool) ([]ai.Tool, error) {
	var declarations []ai.Tool
	for _, tool := range tools {
		if (tool.DefaultActive != nil && !*tool.DefaultActive) || (tool.Exposure != "" && tool.Exposure != ToolDirect && tool.Exposure != ToolModelOnly) {
			continue
		}
		declaration, err := agentToolToAITool(tool)
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", tool.Name, err)
		}
		declarations = append(declarations, declaration)
	}
	return declarations, nil
}

// SetSystemSection stages a named system prompt section for the next request.
// A nil text removes the section. Unchanged values publish nothing.
func (a *Agent) SetSystemSection(name string, text *string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingSections == nil {
		a.pendingSections = map[string]*string{}
	}
	a.pendingSections[name] = text
}

// SetSystemPrompt sets the system prompt.
func (a *Agent) SetSystemPrompt(prompt string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, message := range a.state.Messages {
		if message == nil || message.Role != RoleSystem || message.ExcludeFromContext {
			continue
		}
		replacement := *message
		replacement.Content = nil
		replacement.Sections = nil
		a.state.Messages[i] = &replacement
	}
	a.state.SystemPrompt = ""
	a.state.Messages = append(a.state.Messages, &Message{
		Role:      RoleSystem,
		Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: prompt}},
		Timestamp: time.Now(),
	})
	a.messagesVersion++
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
	if len(a.state.Messages) > 0 && a.state.Messages[0] != nil && a.state.Messages[0].Role == RoleSystem {
		a.state.SystemPrompt = ai.CurrentSystemPrompt(ConvertMessagesToAI(a.state.Messages[:1]))
	}
	a.state.Messages = slicesClone(messages)
	if len(messages) > 0 && messages[0] != nil && messages[0].Role == RoleSystem && !messages[0].ExcludeFromContext {
		a.state.SystemPrompt = ""
	}
	a.messagesVersion++
}

// RestoreMessages installs a persisted branch without inheriting in-memory prompt state.
func (a *Agent) RestoreMessages(messages []*Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Messages = slices.Clone(messages)
	a.state.SystemPrompt = ""
	a.messagesVersion++
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

// SetThinkingBudgets replaces per-level thinking budgets for future turns.
func (a *Agent) SetThinkingBudgets(budgets *ai.ThinkingBudgets) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config.ThinkingBudgets = budgets
}

// SetTransport sets the preferred provider transport for subsequent runs.
func (a *Agent) SetTransport(transport ai.Transport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config.Transport = transport
}

// SetRequestOptions replaces provider options for subsequent agent runs.
func (a *Agent) SetRequestOptions(opts ...ai.Option) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config.RequestOptions = slices.Clone(opts)
}
