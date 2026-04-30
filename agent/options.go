package agent // import "github.com/losinggeneration/tau/agent"

import "github.com/losinggeneration/tau/ai"

// Option configures an agent.
type Option func(*Config)

// WithModel sets the model.
func WithModel(model *ai.Model) Option {
	return func(c *Config) {
		c.Model = model
	}
}

// WithSystemPrompt sets the system prompt.
func WithSystemPrompt(prompt string) Option {
	return func(c *Config) {
		c.SystemPrompt = prompt
	}
}

// WithInitialMessages seeds the agent transcript before the first run.
func WithInitialMessages(messages ...*Message) Option {
	return func(c *Config) {
		c.InitialMessages = append([]*Message(nil), messages...)
	}
}

// WithTools sets the tools.
func WithTools(tools ...Tool) Option {
	return func(c *Config) {
		c.Tools = append(c.Tools, tools...)
	}
}

// WithThinkingLevel sets the thinking level.
func WithThinkingLevel(level ThinkingLevel) Option {
	return func(c *Config) {
		c.ThinkingLevel = level
	}
}

// WithStreamHandler sets the stream handler.
func WithStreamHandler(handler StreamHandler) Option {
	return func(c *Config) {
		c.StreamHandler = handler
	}
}

// WithAPIKeyProvider sets the API key provider.
func WithAPIKeyProvider(provider APIKeyProvider) Option {
	return func(c *Config) {
		c.APIKeyProvider = provider
	}
}

// WithClientFactory sets a custom AI client factory for agent model calls.
func WithClientFactory(factory ClientFactory) Option {
	return func(c *Config) {
		c.ClientFactory = factory
	}
}

// WithStreamFn sets a custom low-level stream function.
func WithStreamFn(fn StreamFunc) Option {
	return func(c *Config) {
		c.StreamFn = fn
	}
}

// WithConvertToLlm sets the message conversion function used before each model call.
func WithConvertToLlm(fn ConvertToLlmFunc) Option {
	return func(c *Config) {
		c.ConvertToLlm = fn
	}
}

// WithTransformContext sets the context transform applied before ConvertToLlm.
func WithTransformContext(fn TransformContextFunc) Option {
	return func(c *Config) {
		c.TransformContext = fn
	}
}

// WithBeforeToolCall sets the hook invoked before a tool executes.
func WithBeforeToolCall(fn BeforeToolCallFunc) Option {
	return func(c *Config) {
		c.BeforeToolCall = fn
	}
}

// WithAfterToolCall sets the hook invoked after a tool executes.
func WithAfterToolCall(fn AfterToolCallFunc) Option {
	return func(c *Config) {
		c.AfterToolCall = fn
	}
}

// WithPayloadCallback sets a callback invoked before provider requests are sent.
func WithPayloadCallback(fn func(any) any) Option {
	return func(c *Config) {
		c.OnPayload = fn
	}
}

// WithResponseCallback sets a callback invoked after provider responses are received.
func WithResponseCallback(fn func(ai.ProviderResponse)) Option {
	return func(c *Config) {
		c.OnResponse = fn
	}
}

// WithToolExecution sets how multiple tool calls in one assistant turn execute.
func WithToolExecution(mode ToolExecutionMode) Option {
	return func(c *Config) {
		c.ToolExecution = mode
	}
}

// WithSteeringMode sets how queued steering messages are drained.
func WithSteeringMode(mode QueueMode) Option {
	return func(c *Config) {
		c.SteeringMode = mode
	}
}

// WithFollowUpMode sets how queued follow-up messages are drained.
func WithFollowUpMode(mode QueueMode) Option {
	return func(c *Config) {
		c.FollowUpMode = mode
	}
}

// WithSessionID forwards a session identifier to cache-aware providers.
func WithSessionID(sessionID string) Option {
	return func(c *Config) {
		c.SessionID = sessionID
	}
}

// WithThinkingBudgets sets optional per-level thinking token budgets.
func WithThinkingBudgets(budgets *ai.ThinkingBudgets) Option {
	return func(c *Config) {
		c.ThinkingBudgets = budgets
	}
}

// WithTransport sets the preferred AI transport.
func WithTransport(transport ai.Transport) Option {
	return func(c *Config) {
		c.Transport = transport
	}
}

// WithMaxRetryDelayMs sets the cap for provider-requested retry delays.
func WithMaxRetryDelayMs(delay int) Option {
	return func(c *Config) {
		c.MaxRetryDelayMs = delay
	}
}

// NewConfig creates a new config with options applied.
func NewConfig(opts ...Option) Config {
	cfg := Config{
		ThinkingLevel: ThinkingOff,
		ToolExecution: ToolExecutionParallel,
		SteeringMode:  QueueModeOneAtATime,
		FollowUpMode:  QueueModeOneAtATime,
		Transport:     ai.TransportSSE,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	return cfg
}
