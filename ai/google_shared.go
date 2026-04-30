package ai // import "github.com/losinggeneration/tau/ai"

import (
	"encoding/json"
	"regexp"
	"strings"
)

// SkipThoughtSignatureValidator is the sentinel value that tells the Gemini
// API to skip thought signature validation. Used for unsigned function call
// parts (e.g. replayed from providers without thought signatures like Claude
// via Antigravity).
// See: https://ai.google.dev/gemini-api/docs/thought-signatures
const SkipThoughtSignatureValidator = "skip_thought_signature_validator"

// base64SignaturePattern matches valid base64-encoded thought signatures.
var base64SignaturePattern = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)

// IsValidThoughtSignature reports whether a thought signature is a valid
// base64 string.
func IsValidThoughtSignature(signature string) bool {
	if signature == "" {
		return false
	}
	if len(signature)%4 != 0 {
		return false
	}
	return base64SignaturePattern.MatchString(signature)
}

// ResolveThoughtSignature keeps a signature only when it comes from the same
// provider/model and is valid base64.
func ResolveThoughtSignature(isSameProviderAndModel bool, signature string) string {
	if !isSameProviderAndModel {
		return ""
	}
	if !IsValidThoughtSignature(signature) {
		return ""
	}
	return signature
}

// IsThinkingPart reports whether a Google response part should be treated as
// thinking content.
func IsThinkingPart(thought bool) bool {
	return thought
}

// requiresToolCallID returns true if the model requires explicit tool call IDs.
func requiresToolCallID(modelID string) bool {
	return strings.HasPrefix(modelID, "claude-") || strings.HasPrefix(modelID, "gpt-oss-")
}

// RequiresToolCallID reports whether a Google-family model requires explicit
// tool call IDs in function call / response parts.
func RequiresToolCallID(modelID string) bool {
	return requiresToolCallID(modelID)
}

// supportsMultimodalFunctionResponse returns true if the model supports
// multimodal function responses (Gemini 3+).
func supportsMultimodalFunctionResponse(modelID string) bool {
	lower := strings.ToLower(modelID)
	if strings.HasPrefix(lower, "gemini") || strings.HasPrefix(lower, "gemini-live") {
		return isGeminiMajorVersionAtLeast(modelID, 3)
	}
	return true
}

func isGeminiMajorVersionAtLeast(modelID string, minVersion int) bool {
	lower := strings.ToLower(modelID)
	prefixes := []string{"gemini-live-", "gemini-"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			rest := strings.TrimPrefix(lower, prefix)
			version := 0
			for _, r := range rest {
				if r < '0' || r > '9' {
					break
				}
				version = version*10 + int(r-'0')
			}
			return version >= minVersion
		}
	}
	return false
}

// isGemini3Model returns true if the model is a Gemini 3 family model.
func isGemini3Model(modelID string) bool {
	lower := strings.ToLower(modelID)
	return strings.Contains(lower, "gemini-3")
}

// isGemini3ProModel returns true if the model is a Gemini 3 Pro model.
func isGemini3ProModel(modelID string) bool {
	lower := strings.ToLower(modelID)
	return strings.Contains(lower, "gemini-3") && strings.Contains(lower, "-pro")
}

// isGemini3FlashModel returns true if the model is a Gemini 3 Flash model.
func isGemini3FlashModel(modelID string) bool {
	lower := strings.ToLower(modelID)
	return strings.Contains(lower, "gemini-3") && strings.Contains(lower, "-flash")
}

// isGemma4Model returns true if the model is a Gemma 4 model.
func isGemma4Model(modelID string) bool {
	lower := strings.ToLower(modelID)
	return strings.Contains(lower, "gemma-4") || strings.Contains(lower, "gemma4")
}

func disabledGoogleThinkingLevel(modelID string) (GoogleThinkingLevel, bool) {
	switch {
	case isGemini3ProModel(modelID):
		return GoogleThinkingLevelLow, true
	case isGemini3FlashModel(modelID), isGemma4Model(modelID):
		return GoogleThinkingLevelMinimal, true
	default:
		return GoogleThinkingLevelUnspecified, false
	}
}

// retainThoughtSignature preserves the last non-empty signature for a block.
// Some backends only send thoughtSignature on the first delta; later deltas may omit it.
func retainThoughtSignature(existing, incoming string) string {
	if incoming != "" {
		return incoming
	}
	return existing
}

// RetainThoughtSignature preserves the last non-empty signature for a block.
func RetainThoughtSignature(existing, incoming string) string {
	return retainThoughtSignature(existing, incoming)
}

// NormalizeToolCallIDForGoogle normalizes a tool call ID for Google API compatibility.
// Models that require tool call IDs (claude-*, gpt-oss-*) need IDs that match
// Google's pattern: ^[a-zA-Z0-9_-]{1,64}$.
// Implements ToolCallIDNormalizer for use with TransformContext.
func NormalizeToolCallIDForGoogle(id string, model *Model, source *AssistantMessage) string {
	// Google drops disallowed runes rather than substituting for them.
	return sanitizeToolCallID(id, 0)
}

// toolChoiceMode returns the Google FunctionCallingConfig mode for choice.
//   - nil returns "" (caller should skip toolConfig).
//   - string returns the value as-is ("auto", "none", "any", "required").
//   - structured object ({"type":"function",...} or {"type":"tool",...})
//     returns "any", since specifying a particular function is semantically
//     "must call one of these named functions" (mapToolChoice maps this to
//     the "ANY" enum).
//   - any other type returns "" (silent skip).
//
// Used as a gate by Vertex (and any other provider that needs to know whether
// the caller expressed an opinion about tool selection).
func toolChoiceMode(choice any) string {
	switch v := choice.(type) {
	case nil:
		return ""
	case string:
		return v
	case map[string]any:
		return "any"
	default:
		return ""
	}
}

// toolChoiceFunctions returns the function names extracted from a structured
// tool choice of the form:
//   - {"type":"function","function":{"name":"..."}}  (OpenAI-style)
//   - {"type":"tool","name":"..."}                  (Anthropic-style)
//
// Returns nil when choice is a string, nil, or a structured shape with no
// recognizable name. Vertex's functionCallingConfig only accepts a single
// function name; multi-function choices are not yet supported here.
func toolChoiceFunctions(choice any) []string {
	m, ok := choice.(map[string]any)
	if !ok {
		return nil
	}
	kind, _ := m["type"].(string)
	var name string
	switch kind {
	case "function":
		if fn, ok := m["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
		}
	case "tool":
		name, _ = m["name"].(string)
	}
	if name == "" {
		return nil
	}
	return []string{name}
}

func mapToolChoice(choice any) string {
	switch toolChoiceMode(choice) {
	case "none":
		return "NONE"
	case "any":
		return "ANY"
	default:
		return "AUTO"
	}
}

// MapGoogleToolChoice maps a tool choice value to the Google mode string.
func MapGoogleToolChoice(choice any) string {
	return mapToolChoice(choice)
}

// buildGoogleContentBlock creates a content block from a Message.
// Returns role, parts, and whether the content should be skipped.
// modelID is used to determine Gemini 3 sentinel handling for unsigned function calls.
func buildGoogleContentBlock(msg Message, isSameProviderModel bool, modelID string) (role string, parts []map[string]any, skip bool) {
	normalizedRole := normalizeMessageRole(msg.Role)

	switch normalizedRole {
	case "user":
		role = "user"
	case "assistant":
		role = "model"
	case "toolResult":
		role = "user"
	default:
		role = "user"
	}

	if normalizedRole == "toolResult" {
		// Tool results are handled specially
		return role, nil, false
	}

	switch c := msg.Content.(type) {
	case string:
		if c != "" {
			parts = append(parts, map[string]any{"text": c})
		}
	case []ContentBlock:
		for _, block := range c {
			if block.Type == ContentBlockText && block.Text != "" {
				// Filter empty text blocks - they can cause issues with some models (e.g. Claude via Antigravity)
				parts = append(parts, map[string]any{"text": block.Text})
			} else if block.Type == ContentBlockToolCall {
				// Assistant tool use - convert to function call
				// Arguments is json.RawMessage, need to unmarshal to map[string]any
				var args map[string]any
				if err := json.Unmarshal(block.Arguments, &args); err != nil {
					args = nil
				}
				// Thought signature: prefer the resolved one (validated base64,
				// same provider/model). For Gemini 3 unsigned function calls,
				// use the skip-validator sentinel.
				signature := ResolveThoughtSignature(isSameProviderModel, block.ThoughtSignature)
				if signature == "" && isGemini3Model(modelID) {
					signature = SkipThoughtSignatureValidator
				}
				part := map[string]any{
					"functionCall": map[string]any{
						"name": block.Name,
						"args": args,
					},
				}
				if signature != "" {
					part["thoughtSignature"] = signature
				}
				parts = append(parts, part)
			}
		}
	}

	return role, parts, len(parts) == 0
}

// googleToolResultPart creates a Google function response part from a tool result message.
// This is shared between Google Generative AI and Vertex.
func googleToolResultPart(msg Message, blocks []ContentBlock) map[string]any {
	var textParts []string
	for _, block := range blocks {
		if block.Type == ContentBlockText && block.Text != "" {
			textParts = append(textParts, block.Text)
		}
	}

	responseKey := "output"
	if msg.IsError {
		responseKey = "error"
	}

	return map[string]any{
		"functionResponse": map[string]any{
			"name": msg.ToolName,
			"response": map[string]any{
				responseKey: strings.Join(textParts, "\n"),
			},
		},
	}
}

// googleStopReason maps Google's finish reason to our StopReason type.
// This is shared between Google Generative AI and Vertex.
// Maps all safety/reasoning-related finish reasons to "error" per pi-mono reference.
func googleStopReason(reason string) StopReason {
	switch reason {
	case "STOP":
		return StopReasonStop
	case "MAX_TOKENS":
		return StopReasonLength
	case "ABORTED":
		return StopReasonAborted
	// All other finish reasons are treated as errors:
	// BLOCKLIST, PROHIBITED_CONTENT, SPII, SAFETY, IMAGE_SAFETY,
	// IMAGE_PROHIBITED_CONTENT, IMAGE_RECITATION, IMAGE_OTHER,
	// RECITATION, FINISH_REASON_UNSPECIFIED, OTHER, LANGUAGE,
	// MALFORMED_FUNCTION_CALL, UNEXPECTED_TOOL_CALL, NO_IMAGE,
	// ERROR
	default:
		return StopReasonError
	}
}

// MapGoogleStopReason maps Google's finish reason to the shared StopReason.
func MapGoogleStopReason(reason string) StopReason {
	return googleStopReason(reason)
}

// HasDuplicateToolCallID checks if a tool call ID already exists in the content.
// This implements pi-mono's dedup check: output.content.some((b) => b.type === "toolCall" && b.id === providedId)
func HasDuplicateToolCallID(content []ContentBlock, id string) bool {
	for _, block := range content {
		if block.Type == ContentBlockToolCall && block.ID == id {
			return true
		}
	}
	return false
}

// ConvertTools converts tools to Gemini function declarations format.
// By default uses parametersJsonSchema which supports full JSON Schema.
// Set useParameters to true to use the legacy parameters field instead.
// This matches the TypeScript convertTools function from providers/google-shared.ts.
func ConvertTools(tools []Tool, useParameters bool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}

	declarations := make([]map[string]any, len(tools))
	for i, tool := range tools {
		decl := map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
		}
		if useParameters {
			// Legacy parameters field
			var params map[string]any
			if err := json.Unmarshal(tool.Parameters, &params); err == nil {
				decl["parameters"] = params
			}
		} else {
			// Full JSON Schema support
			var params map[string]any
			if err := json.Unmarshal(tool.Parameters, &params); err == nil {
				decl["parametersJsonSchema"] = params
			}
		}
		declarations[i] = decl
	}

	return []map[string]any{
		{"functionDeclarations": declarations},
	}
}
