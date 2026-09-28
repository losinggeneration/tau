package ai // import "github.com/losinggeneration/tau/ai"

// Standard capability sets for first-class providers.
// Individual models may vary, but these represent what the provider
// API supports at the protocol level.

// AnthropicCapabilities is the baseline capability set for Anthropic providers.
// Claude models support: tools, streaming, vision, extended thinking, parallel tool calls.
var AnthropicCapabilities = Capabilities{
	Tools:             true,
	Streaming:         true,
	Vision:            true,
	Thinking:          true,
	ParallelToolCalls: true,
}

// ClaudeAgentCapabilities is the Claude Code Agent SDK surface. Vision is
// true: the provider converts image content blocks into base64 image blocks
// for streaming input (see claudeAgentBuildPromptBlocks).
var ClaudeAgentCapabilities = Capabilities{
	Tools:             true,
	Streaming:         true,
	Vision:            true,
	Thinking:          true,
	ParallelToolCalls: false,
}

// OpenAICompletionsCapabilities is the baseline for OpenAI-compatible providers.
// Supports: tools, streaming, vision (GPT-4o), thinking (o1/o3), parallel tool calls.
var OpenAICompletionsCapabilities = Capabilities{
	Tools:             true,
	Streaming:         true,
	Vision:            true,
	Thinking:          true,
	ParallelToolCalls: true,
}

// GoogleGenerativeAICapabilities is the baseline for Google Generative AI.
// Supports: tools, streaming, vision (Gemini 1.5+), no extended thinking.
var GoogleGenerativeAICapabilities = Capabilities{
	Tools:             true,
	Streaming:         true,
	Vision:            true,
	Thinking:          false, // Gemini doesn't have extended thinking like Claude
	ParallelToolCalls: true,
}

// GoogleGeminiCLICapabilities is the baseline for Google Gemini CLI / Antigravity.
var GoogleGeminiCLICapabilities = Capabilities{
	Tools:             true,
	Streaming:         true,
	Vision:            true,
	Thinking:          false,
	ParallelToolCalls: true,
}

// MistralCapabilities is the baseline for Mistral.
var MistralCapabilities = Capabilities{
	Tools:             true,
	Streaming:         true,
	Vision:            false, // Mistral models don't support vision
	Thinking:          false,
	ParallelToolCalls: true,
}

// BedrockCapabilities is the baseline for AWS Bedrock.
// Note: actual capabilities depend on the underlying model (Nova vs Claude).
// This represents the protocol-level support.
var BedrockCapabilities = Capabilities{
	Tools:             true,
	Streaming:         true,
	Vision:            true, // Nova and Claude support vision
	Thinking:          true, // Claude on Bedrock supports extended thinking
	ParallelToolCalls: true,
}

// AzureCapabilities is the baseline for Azure OpenAI.
var AzureCapabilities = Capabilities{
	Tools:             true,
	Streaming:         true,
	Vision:            true,
	Thinking:          true,
	ParallelToolCalls: true,
}

// CodexCapabilities is the baseline for OpenAI Codex.
var CodexCapabilities = Capabilities{
	Tools:             true,
	Streaming:         true,
	Vision:            true,
	Thinking:          true,
	ParallelToolCalls: true,
}
