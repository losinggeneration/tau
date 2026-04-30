package ai // import "github.com/losinggeneration/tau/ai"

// Capabilities describes what a provider supports. First-class providers
// have these compiled in; compat providers declare them in settings.json.
type Capabilities struct {
	// Tools indicates the provider supports tool/function calling.
	Tools bool `json:"tools"`
	// Streaming indicates the provider supports streaming responses.
	Streaming bool `json:"streaming"`
	// Vision indicates the provider supports image inputs.
	Vision bool `json:"vision"`
	// Thinking indicates the provider supports extended thinking/reasoning.
	Thinking bool `json:"thinking"`
	// ParallelToolCalls indicates the provider supports multiple tool calls
	// in a single assistant turn.
	ParallelToolCalls bool `json:"parallel_tool_calls"`
}

// CapableProvider extends Provider with capability metadata.
type CapableProvider interface {
	Provider
	// Capabilities returns the provider's supported features.
	Capabilities() Capabilities
}
