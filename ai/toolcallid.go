package ai

import "strings"

// Providers constrain tool call IDs differently. Google, Bedrock and the
// Responses family all require ^[a-zA-Z0-9_-]{1,64}$, and each carried its own
// copy of the same character loop — differing only in what it did with a
// disallowed rune.
//
// Two normalizers deliberately stay separate rather than folding in here:
//
//   - normalizeToolCallId (openai.go) maps only |, +, / and =, leaving other
//     characters intact, and caps OpenAI at 40. That is a different policy,
//     not a different disposal, and unifying it would rewrite IDs that
//     currently round-trip untouched.
//   - createMistralToolCallIdNormalizer (mistral.go) derives fixed-length IDs
//     with collision retry and per-conversation state. That is generation
//     rather than sanitization.

// toolCallIDMaxLen is the length Google, Bedrock and the Responses family all
// clamp tool call IDs to.
const toolCallIDMaxLen = 64

// isToolCallIDRune reports whether r is allowed in a tool call ID.
func isToolCallIDRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '_' || r == '-'
}

// sanitizeToolCallID rewrites id so every rune is allowed, then truncates to
// toolCallIDMaxLen. Disallowed runes are replaced with sub, or dropped when
// sub is 0 — Google drops them, while Bedrock and the Responses family
// substitute an underscore.
func sanitizeToolCallID(id string, sub rune) string {
	var b strings.Builder
	b.Grow(len(id))

	for _, r := range id {
		switch {
		case isToolCallIDRune(r):
			b.WriteRune(r)
		case sub != 0:
			b.WriteRune(sub)
		}
	}

	out := b.String()
	if len(out) > toolCallIDMaxLen {
		out = out[:toolCallIDMaxLen]
	}

	return out
}
