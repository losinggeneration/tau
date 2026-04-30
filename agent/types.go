package agent // import "github.com/losinggeneration/tau/agent"

import (
	"context"
	"slices"
	"time"

	"github.com/losinggeneration/tau/ai"
)

// Tool represents an agent tool.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	Execute     ToolFunc

	// Label is the display name shown in UI. If empty, Name is used.
	Label string

	// PrepareArguments is an optional function that preprocesses arguments
	// before validation and execution. It can be used for argument coercion,
	// adding defaults, or enriching arguments with additional context.
	PrepareArguments func(args map[string]any) map[string]any
}

// ToolFunc is the function signature for tool execution.
type ToolFunc func(ctx ToolContext) (*ToolResult, error)

// ToolContext contains context for tool execution.
type ToolContext struct {
	Context    context.Context
	ToolCallID string
	Name       string
	Args       map[string]any
	OnUpdate   func(*ToolResult)

	// SessionCWD is the session's current working directory.
	// All tools that touch the filesystem resolve relative paths against this.
	SessionCWD string

	// Capabilities describes what the active provider supports.
	Capabilities ai.Capabilities
}

// ToolResult represents the result of tool execution.
type ToolResult struct {
	Content       string
	ContentBlocks []ai.ContentBlock
	IsError       bool
	Details       any
	// ExcludeFromContext keeps the result in events/session history but omits it from future LLM context.
	ExcludeFromContext bool
	// Message is the corresponding tool result message, set after execution.
	Message *Message
}

// NormalizedContent returns the structured content blocks for the tool result.
func (r *ToolResult) NormalizedContent() []ai.ContentBlock {
	if r == nil {
		return nil
	}
	if len(r.ContentBlocks) > 0 {
		return r.ContentBlocks
	}
	return []ai.ContentBlock{{Type: ai.ContentBlockText, Text: r.Content}}
}

// ToolExecutionMode defines how multiple tool calls are executed.
// Matches pi-mono's ToolExecutionMode = "sequential" | "parallel".
type ToolExecutionMode = string

const (
	ToolExecutionParallel   ToolExecutionMode = "parallel"
	ToolExecutionSequential ToolExecutionMode = "sequential"
)

// ThinkingLevel represents how much reasoning the agent should use.
// Matches pi-mono's ThinkingLevel = "off" | "minimal" | "low" | "medium" | "high" | "xhigh".
type ThinkingLevel = string

const (
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
)

// EventDispatchResponse is a single listener's response to a dispatched
// event. ResponseData carries event-specific result data as raw JSON (e.g.
// {"systemPrompt": "..."} for before_agent_start rewrites).
type EventDispatchResponse struct {
	Block        bool
	BlockReason  string
	ResponseData []byte
}

// EventDispatchResult is the aggregate outcome of dispatching an event to
// all subscribed listeners. Blocked is true when a listener blocked a
// cancellable event; Responses carries each listener's response in dispatch
// order (registration order).
type EventDispatchResult struct {
	Blocked   bool
	Responses []EventDispatchResponse
}

// Config contains agent configuration.
type Config struct {
	Model           *ai.Model
	SystemPrompt    string
	InitialMessages []*Message
	Tools           []Tool
	ThinkingLevel   ThinkingLevel
	StreamHandler   StreamHandler
	APIKeyProvider  APIKeyProvider
	ClientFactory   ClientFactory
	StreamFn        StreamFunc

	// SystemAddendum is additional context appended to the system prompt.
	// Used for skill content injection.
	SystemAddendum string

	// Optional configuration
	ConvertToLlm     ConvertToLlmFunc
	TransformContext TransformContextFunc
	BeforeToolCall   BeforeToolCallFunc
	AfterToolCall    AfterToolCallFunc
	OnPayload        func(any) any
	OnResponse       func(ai.ProviderResponse)
	ToolExecution    ToolExecutionMode
	SteeringMode     QueueMode
	FollowUpMode     QueueMode
	SessionID        string
	ThinkingBudgets  *ai.ThinkingBudgets
	Transport        ai.Transport
	MaxRetryDelayMs  int

	// EventDispatcher sends events to external listeners (e.g., plugins).
	// The function receives the event name, optional JSON payload, and session ID.
	// It returns the dispatch outcome: Blocked reports a block verdict (for
	// cancellable events such as tool_call), and Responses carries each
	// subscribed listener's response in dispatch order for events whose
	// responses are chained (e.g. before_agent_start system-prompt rewrites).
	EventDispatcher func(name string, payload []byte, sessionID string) EventDispatchResult

	// SessionCWD is the session's current working directory.
	// All tools that touch the filesystem resolve relative paths against this.
	SessionCWD string

	// Capabilities describes what the active provider supports.
	// Tools check this to determine feature availability (e.g., vision for images).
	Capabilities ai.Capabilities
}

// BeforeToolCallFunc is called before a tool is executed.
// Return a result with Block=true to prevent execution.
// ctx includes the AssistantMessage and raw ToolCall for full context.
type BeforeToolCallFunc func(ctx context.Context, toolCtx ToolCallContext) (BeforeToolCallResult, error)

// BeforeToolCallResult is the result of a beforeToolCall hook.
type BeforeToolCallResult struct {
	Block  bool
	Reason string
}

// AfterToolCallFunc is called after a tool finishes execution.
// Return an AfterToolCallResult to override parts of the result.
// ctx includes the AssistantMessage and raw ToolCall for full context.
type AfterToolCallFunc func(ctx context.Context, toolCtx ToolCallContext, result *ToolResult) (AfterToolCallResult, error)

// AfterToolCallResult is the result of an afterToolCall hook.
type AfterToolCallResult struct {
	// ContentBlocks overrides the result content with structured blocks.
	ContentBlocks []ai.ContentBlock
	// Content overrides the result content as plain text (falls back to result.Content if no blocks).
	Content string
	// Details sets additional structured details.
	Details any
	// IsError marks the result as an error.
	IsError *bool
}

// QueueMode defines how queued messages are drained.
// Matches pi-mono's QueueMode = "all" | "one-at-a-time".
type QueueMode = string

const (
	QueueModeAll        QueueMode = "all"
	QueueModeOneAtATime QueueMode = "one-at-a-time"
)

// StreamHandler handles agent events during streaming.
type StreamHandler func(event Event)

// APIKeyProvider returns an API key for a provider.
type APIKeyProvider func(ctx context.Context, provider string) (string, error)

// ClientFactory builds an AI client for a model.
type ClientFactory func(model *ai.Model) (ai.Client, error)

// StreamFunc allows callers to override the low-level stream implementation.
// This mirrors the TypeScript streamFn hook while keeping Go signatures explicit.
type StreamFunc func(ctx context.Context, model *ai.Model, prompt *ai.Context, handler ai.EventHandler, opts ...ai.Option) error

// ConvertToLlmFunc converts agent messages into AI-provider messages.
type ConvertToLlmFunc func(ctx context.Context, messages []*Message) ([]ai.Message, error)

// TransformContextFunc rewrites the message context before model conversion.
type TransformContextFunc func(ctx context.Context, messages []*Message) ([]*Message, error)

// MessageSupplierFunc returns queued messages when the loop asks for them.
type MessageSupplierFunc func(ctx context.Context) ([]*Message, error)

// Message represents an agent message.
type Message struct {
	Role               MessageRole
	Content            []ai.ContentBlock
	Data               any
	ToolCallID         string
	ToolName           string
	IsError            bool
	API                string
	Provider           string
	Model              string
	StopReason         ai.StopReason
	Usage              ai.Usage
	ErrorMessage       string
	ResponseID         string
	ExcludeFromContext bool
	Timestamp          time.Time
}

// MessageRole represents the role of a message.
type MessageRole string

const (
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
	RoleSystem    MessageRole = "system"
)

// NewUserMessage creates a user message.
func NewUserMessage(content string) *Message {
	return &Message{
		Role:      RoleUser,
		Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: content}},
		Timestamp: time.Now(),
	}
}

// NewAssistantMessage creates an assistant message.
func NewAssistantMessage(content []ai.ContentBlock) *Message {
	return &Message{
		Role:      RoleAssistant,
		Content:   content,
		Timestamp: time.Now(),
	}
}

// NewToolMessage creates a tool result message.
func NewToolMessage(callID, name, content string, isError bool) *Message {
	return &Message{
		Role:       RoleTool,
		ToolCallID: callID,
		ToolName:   name,
		Content:    []ai.ContentBlock{{Type: ai.ContentBlockText, Text: content}},
		IsError:    isError,
		Timestamp:  time.Now(),
	}
}

// State represents the current agent state.
type State struct {
	Model         *ai.Model
	SystemPrompt  string
	Messages      []*Message
	Tools         []Tool
	ThinkingLevel ThinkingLevel

	// PendingToolIDs contains tool call IDs that are currently executing.
	// IDs appear after tool_execution_start and are removed before the run
	// becomes idle again.
	PendingToolIDs map[string]struct{}

	// Error preserves the most recent failed or aborted run error as a Go error.
	// This is cleared when a new run starts.
	Error error

	// ErrorMessage is the stable string form of Error for public consumers.
	// This is cleared when a new run starts and populated when a run fails or is
	// aborted.
	ErrorMessage string

	// SessionID is a unique identifier for this agent session.
	SessionID string

	// SessionCWD is the session's current working directory.
	// All tools that touch the filesystem resolve relative paths against this.
	SessionCWD string

	// Capabilities describes what the active provider supports.
	Capabilities ai.Capabilities

	// IsStreaming is true while the agent is processing a prompt or continuation.
	// This remains true until awaited agent_end listeners settle.
	IsStreaming bool

	// StreamingMessage is the partial assistant message for the current streamed
	// response, if any.
	StreamingMessage *Message
}

// Clone creates a copy of the state.
func (s *State) Clone() *State {
	messages := make([]*Message, len(s.Messages))
	copy(messages, s.Messages)

	pending := make(map[string]struct{}, len(s.PendingToolIDs))
	for id := range s.PendingToolIDs {
		pending[id] = struct{}{}
	}

	return &State{
		Model:            s.Model,
		SystemPrompt:     s.SystemPrompt,
		Messages:         messages,
		Tools:            append([]Tool(nil), s.Tools...),
		ThinkingLevel:    s.ThinkingLevel,
		PendingToolIDs:   pending,
		Error:            s.Error,
		ErrorMessage:     s.ErrorMessage,
		SessionID:        s.SessionID,
		SessionCWD:       s.SessionCWD,
		Capabilities:     s.Capabilities,
		IsStreaming:      s.IsStreaming,
		StreamingMessage: s.StreamingMessage,
	}
}

// PendingToolCalls returns the tool call IDs currently executing.
// This provides parity with pi-mono's `pendingToolCalls: ReadonlySet<string>`.
// The returned slice is a snapshot of current state; further changes to
// PendingToolIDs will not be reflected.
func (s *State) PendingToolCalls() []string {
	if len(s.PendingToolIDs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(s.PendingToolIDs))
	for id := range s.PendingToolIDs {
		ids = append(ids, id)
	}
	// Sort for deterministic iteration order
	slices.Sort(ids)
	return ids
}

// Event represents an agent event.
type Event interface {
	isEvent()
}

// AgentStart indicates the agent has started processing.
type AgentStart struct{}

func (AgentStart) isEvent() {}

// AgentEnd indicates the agent has finished processing.
type AgentEnd struct {
	Messages []*Message
}

func (AgentEnd) isEvent() {}

// MessageStart indicates a message is starting.
type MessageStart struct {
	Message *Message
}

func (MessageStart) isEvent() {}

// MessageUpdate indicates a message is being updated (streaming).
type MessageUpdate struct {
	Message               *Message
	AssistantMessageEvent ai.StreamEvent
}

func (MessageUpdate) isEvent() {}

// MessageEnd indicates a message is complete.
type MessageEnd struct {
	Message *Message
}

func (MessageEnd) isEvent() {}

// TurnStart indicates the start of a new turn.
type TurnStart struct{}

func (TurnStart) isEvent() {}

// TurnEnd indicates the end of a turn.
type TurnEnd struct {
	Message    *Message
	ToolCalls  []ai.ToolCall
	ToolResult []*ToolResult
}

func (TurnEnd) isEvent() {}

// ToolStart indicates a tool is starting execution.
type ToolStart struct {
	ToolCallID string
	Name       string
	Args       map[string]any
}

func (ToolStart) isEvent() {}

// ToolExecutionStart is an alias for ToolStart providing pi-mono compatibility.
// pi-mono emits "tool_execution_start" while Go emits "tool_start".
// Both types are identical; the alias is provided for consumers that expect
// the pi-mono naming convention.
type ToolExecutionStart = ToolStart

// ToolUpdate indicates a tool emitted a partial update while running.
type ToolUpdate struct {
	ToolCallID    string
	Name          string
	Args          map[string]any
	PartialResult *ToolResult
}

func (ToolUpdate) isEvent() {}

// ToolExecutionUpdate is an alias for ToolUpdate providing pi-mono compatibility.
// pi-mono emits "tool_execution_update" while Go emits "tool_update".
// Both types are identical; the alias is provided for consumers that expect
// the pi-mono naming convention.
type ToolExecutionUpdate = ToolUpdate

// ToolEnd indicates a tool has finished.
type ToolEnd struct {
	ToolCallID string
	Name       string
	Result     *ToolResult
	IsError    bool
}

func (ToolEnd) isEvent() {}

// ToolExecutionEnd is an alias for ToolEnd providing pi-mono compatibility.
// pi-mono emits "tool_execution_end" while Go emits "tool_end".
// Both types are identical; the alias is provided for consumers that expect
// the pi-mono naming convention.
type ToolExecutionEnd = ToolEnd

// AgentContext is the shared context for an agent loop.
// Passed to both runAgentLoop and runAgentLoopContinue.
type AgentContext struct {
	Model        *ai.Model
	Tools        []Tool
	SystemPrompt string
	Messages     []*Message

	// SessionCWD is the session's current working directory.
	// All tools that touch the filesystem resolve relative paths against this.
	SessionCWD string

	// Capabilities describes what the active provider supports.
	Capabilities ai.Capabilities
}

// AgentLoopConfig contains the configuration for the low-level agent loop.
type AgentLoopConfig struct {
	// Required configuration
	Model        *ai.Model
	ConvertToLlm ConvertToLlmFunc
	Tools        []Tool

	// Optional: system prompt (can also be set in context)
	SystemPrompt string

	// Optional: additional context appended to the system prompt.
	// Used for skill content injection.
	SystemAddendum string

	// Resolves API key dynamically for each LLM call.
	// Important for short-lived OAuth tokens that may expire during tool execution.
	// Return nil when no key is available.
	GetAPIKey func(ctx context.Context, provider string) (string, error)

	// Optional custom client factory. When nil, ai.GetClient is used.
	ClientFactory ClientFactory

	// Optional low-level stream function override.
	StreamFn StreamFunc

	// Called after the current assistant turn finishes executing its tool calls.
	// If messages are returned, they are added to the context before the next LLM call.
	// Use this for "steering" the agent while it's working.
	GetSteeringMessages MessageSupplierFunc

	// Called when the agent has no more tool calls and no steering messages.
	// If messages are returned, they're added to the context and the agent
	// continues with another turn.
	// Use this for follow-up messages that should wait until the agent finishes.
	GetFollowUpMessages MessageSupplierFunc

	// Optional transform applied to the context before ConvertToLlm.
	// Use this for operations that work at the AgentMessage level:
	// - Context window management (pruning old messages)
	// - Injecting context from external sources
	TransformContext TransformContextFunc

	// Tool execution mode.
	// - "sequential": execute tool calls one by one
	// - "parallel": preflight tool calls sequentially, then execute allowed tools concurrently
	ToolExecution ToolExecutionMode

	// Called before a tool is executed, after arguments have been validated.
	// Return {Block: true} to prevent execution.
	BeforeToolCall func(ctx context.Context, toolCtx ToolCallContext) (BeforeToolCallResult, error)

	// Called after a tool finishes executing.
	// Return AfterToolCallResult to override parts of the executed tool result.
	AfterToolCall func(ctx context.Context, toolCtx ToolCallContext, result *ToolResult) (AfterToolCallResult, error)

	// Called before provider requests are sent; may replace the payload.
	OnPayload func(any) any

	// Called after provider responses are received.
	OnResponse func(ai.ProviderResponse)

	// Thinking/reasoning level for models that support it.
	ThinkingLevel ThinkingLevel

	// Transport to use for API calls.
	Transport ai.Transport

	// Thinking budgets per level.
	ThinkingBudgets *ai.ThinkingBudgets

	// API key (alternative to GetAPIKey for static keys).
	APIKey string

	// Session identifier forwarded to providers for cache/session affinity.
	SessionID string

	// Max retry delay in milliseconds.
	MaxRetryDelayMs int

	// EventDispatcher sends events to external listeners (e.g., plugins).
	// The function receives the event name, optional JSON payload, and session ID.
	// It returns the dispatch outcome: Blocked reports a block verdict (for
	// cancellable events such as tool_call), and Responses carries each
	// subscribed listener's response in dispatch order for events whose
	// responses are chained (e.g. before_agent_start system-prompt rewrites).
	EventDispatcher func(name string, payload []byte, sessionID string) EventDispatchResult

	// SessionCWD is the session's current working directory.
	// All tools that touch the filesystem resolve relative paths against this.
	SessionCWD string

	// Capabilities describes what the active provider supports.
	// Tools check this to determine feature availability (e.g., vision for images).
	Capabilities ai.Capabilities
}

// ToolCallContext contains context for tool call hooks.
// Matches pi-mono's BeforeToolCallContext and AfterToolCallContext.
type ToolCallContext struct {
	ToolCallID       string
	ToolName         string
	Args             map[string]any
	Context          *AgentContext
	AssistantMessage *Message    // The assistant message containing this tool call
	ToolCall         ai.ToolCall // The raw tool call block from the assistant message
}
