package ai

import (
	"regexp"
)

// Overflow detection patterns for different LLM providers.
//
// These patterns match error messages returned when input exceeds
// the model's context window.
//
// Provider-specific patterns (with example error messages):
//
//   - Anthropic: "prompt is too long: 213462 tokens > 200000 maximum"
//   - Anthropic: "413 {"error":{"type":"request_too_large","message":"Request exceeds the maximum size"}}"
//   - OpenAI: "Your input exceeds the context window of this model"
//   - Google: "The input token count (1196265) exceeds the maximum number of tokens allowed (1048575)"
//   - xAI: "This model's maximum prompt length is 131072 but the request contains 537812 tokens"
//   - Groq: "Please reduce the length of the messages or completion"
//   - OpenRouter: "This endpoint's maximum context length is X tokens. However, you requested about Y tokens"
//   - llama.cpp: "the request exceeds the available context size, try increasing it"
//   - LM Studio: "tokens to keep from the initial prompt is greater than the context length"
//   - GitHub Copilot: "prompt token count of X exceeds the limit of Y"
//   - MiniMax: "invalid params, context window exceeds limit"
//   - Kimi For Coding: "Your request exceeded model token limit: X (requested: Y)"
//   - Cerebras: "400/413 status code (no body)"
//   - Mistral: "Prompt contains X tokens ... too large for model with Y maximum context length"
//   - Ollama: Some deployments truncate silently, others return errors like "prompt too long; exceeded max context length by X tokens"
var overflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)prompt (?:is )?too long`),                                                                   // Anthropic and z.ai token overflow
	regexp.MustCompile(`(?i)prompt exceeds max length`),                                                                 // z.ai CN endpoint token overflow
	regexp.MustCompile(`(?i)request_too_large`),                                                                         // Anthropic request byte-size overflow (HTTP 413)
	regexp.MustCompile(`(?i)input is too long for requested model`),                                                     // Amazon Bedrock
	regexp.MustCompile(`(?i)exceeds the context window`),                                                                // OpenAI (Completions & Responses API)
	regexp.MustCompile(`(?i)exceeds (?:the )?(?:model'?s )?maximum context length(?: of [\d,]+ tokens?|\s*\([\d,]+\))`), // OpenAI-compatible proxies (LiteLLM)
	regexp.MustCompile(`(?i)input token count.*exceeds the maximum`),                                                    // Google (Gemini)
	regexp.MustCompile(`(?i)maximum prompt length is \d+`),                                                              // xAI (Grok)
	regexp.MustCompile(`(?i)reduce the length of the messages`),                                                         // Groq
	regexp.MustCompile(`(?i)maximum context length is \d+ tokens`),                                                      // OpenRouter (most backends)
	regexp.MustCompile(`(?i)exceeds (?:the )?maximum allowed input length of [\d,]+ tokens?`),                           // OpenRouter/Poolside
	regexp.MustCompile(`(?i)input \(\d+ tokens\) is longer than the model'?s context length \(\d+ tokens\)`),            // Together AI
	regexp.MustCompile(`(?i)exceeds the limit of \d+`),                                                                  // GitHub Copilot
	regexp.MustCompile(`(?i)exceeds the available context size`),                                                        // llama.cpp server
	regexp.MustCompile(`(?i)greater than the context length`),                                                           // LM Studio
	regexp.MustCompile(`(?i)context window exceeds limit`),                                                              // MiniMax
	regexp.MustCompile(`(?i)exceeded model token limit`),                                                                // Kimi For Coding
	regexp.MustCompile(`(?i)too large for model with \d+ maximum context length`),                                       // Mistral
	regexp.MustCompile(`(?i)prompt has [\d,]+ tokens?, but the configured context size is [\d,]+ tokens?`),              // DS4 server
	regexp.MustCompile(`(?i)model_context_window_exceeded`),                                                             // z.ai non-standard finish_reason surfaced as error text
	regexp.MustCompile(`(?i)prompt too long; exceeded (?:max )?context length`),                                         // Ollama explicit overflow error
	regexp.MustCompile(`(?i)range of input length should be`),                                                           // DashScope / Qwen Token Plan
	regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded`),                                                             // Generic fallback
	regexp.MustCompile(`(?i)too many tokens`),                                                                           // Generic fallback
	regexp.MustCompile(`(?i)token limit exceeded`),                                                                      // Generic fallback
}

// cerebrasBodylessOverflow matches Cerebras's 400/413 with no body. Other
// providers return bodyless 4xx for unrelated failures, so it only counts
// for Cerebras.
var cerebrasBodylessOverflow = regexp.MustCompile(`(?i)^4(?:00|13)\s*(?:status code)?\s*\(no body\)`)

// Non-overflow patterns - error messages matching these are excluded
// from overflow detection even if they also match an overflow pattern.
//
// Example: Bedrock formats throttling errors as "ThrottlingException: Too many tokens,
// please wait before trying again." which would match the "too many tokens" overflow
// pattern without this exclusion.
var nonOverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(Throttling error|Service unavailable):`), // AWS Bedrock non-overflow errors
	regexp.MustCompile(`(?i)rate limit`),                               // Generic rate limiting
	regexp.MustCompile(`(?i)too many requests`),                        // Generic HTTP 429 style
}

// OverflowUsage represents token usage information for overflow detection.
type OverflowUsage struct {
	Input    int // Input tokens
	Output   int // Output tokens
	CacheHit int // Cache read tokens (if supported)
}

// IsContextOverflow checks if an assistant message represents a context overflow error.
//
// This handles two cases:
//  1. Error-based overflow: Most providers return stopReason "error" with a
//     specific error message pattern.
//  2. Silent overflow: Some providers accept overflow requests and return
//     successfully. For these, check if usage.input exceeds the context window.
//
// Reliability by Provider:
//
// Reliable detection (returns error with detectable message):
//   - Anthropic: "prompt is too long: X tokens > Y maximum" or "request_too_large"
//   - OpenAI (Completions & Responses): "exceeds the context window"
//   - Google Gemini: "input token count exceeds the maximum"
//   - xAI (Grok): "maximum prompt length is X but request contains Y"
//   - Groq: "reduce the length of the messages"
//   - Cerebras: 400/413 status code (no body)
//   - Mistral: "Prompt contains X tokens ... too large for model with Y maximum context length"
//   - OpenRouter (all backends): "maximum context length is X tokens"
//   - llama.cpp: "exceeds the available context size"
//   - LM Studio: "greater than the context length"
//   - Kimi For Coding: "exceeded model token limit: X (requested: Y)"
//
// Unreliable detection:
//   - z.ai: Sometimes accepts overflow silently (detectable via usage.input > contextWindow),
//     sometimes returns rate limit errors. Pass contextWindow param to detect silent overflow.
//   - Ollama: May truncate input silently for some setups, but may also return explicit
//     overflow errors that match the patterns above. Silent truncation still cannot be
//     detected here because we do not know the expected token count.
func IsContextOverflow(provider, stopReason, errorMessage string, usage OverflowUsage, contextWindow int) bool {
	// Case 1: Check error message patterns
	if stopReason == "error" && errorMessage != "" {
		// Skip messages matching known non-overflow patterns (e.g. throttling / rate-limit)
		isNonOverflow := false

		for _, pattern := range nonOverflowPatterns {
			if pattern.MatchString(errorMessage) {
				isNonOverflow = true

				break
			}
		}

		if !isNonOverflow {
			for _, pattern := range overflowPatterns {
				if pattern.MatchString(errorMessage) {
					return true
				}
			}
			if provider == ProviderCerebras && cerebrasBodylessOverflow.MatchString(errorMessage) {
				return true
			}
		}
	}

	// Case 2: Silent overflow (z.ai style) - successful but usage exceeds context
	if contextWindow > 0 && stopReason == "stop" {
		inputTokens := usage.Input + usage.CacheHit
		if inputTokens > contextWindow {
			return true
		}
	}

	// Case 3: Length-stop overflow (Xiaomi MiMo style) - the server truncates
	// oversized input to the window, leaving no room for output.
	if contextWindow > 0 && stopReason == "length" && usage.Output == 0 {
		if float64(usage.Input+usage.CacheHit) >= float64(contextWindow)*0.99 {
			return true
		}
	}

	return false
}

// IsRecoverableLength reports whether a length stop ended below the intended
// output limit, which context pressure or provider truncation can cause.
// desiredMaxOutput must be the limit before any context-based clamping.
func IsRecoverableLength(stopReason string, output, desiredMaxOutput int) bool {
	return stopReason == "length" && desiredMaxOutput > 0 && output < desiredMaxOutput
}

// getOverflowPatterns returns a defensive copy of overflow detection patterns for tests.
func getOverflowPatterns() []*regexp.Regexp {
	result := make([]*regexp.Regexp, len(overflowPatterns))
	copy(result, overflowPatterns)

	return result
}
