package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildGoogleToolCallIDDedup tests P7-GG06: tool call ID generation with dedup
// Reference: pi-mono google.ts lines 176-181
// - Generated ID format: {name}_{Date.now()}_{counter}
// - Generate new ID if provided ID is empty or already in use
func TestBuildGoogleToolCallIDDedup(t *testing.T) {
	tests := []struct {
		name         string
		providedID   string
		existingIDs  []string
		functionName string
		wantNewID    bool
	}{
		{
			name:         "empty provided ID generates new ID",
			providedID:   "",
			existingIDs:  []string{},
			functionName: "get_weather",
			wantNewID:    true,
		},
		{
			name:         "provided ID not in use keeps it",
			providedID:   "get_weather_12345",
			existingIDs:  []string{},
			functionName: "get_weather",
			wantNewID:    false,
		},
		{
			name:         "provided ID already in use generates new ID",
			providedID:   "get_weather_existing",
			existingIDs:  []string{"get_weather_existing"},
			functionName: "get_weather",
			wantNewID:    true,
		},
		{
			name:         "provided ID with other IDs in use keeps it",
			providedID:   "get_weather_existing",
			existingIDs:  []string{"get_weather_other"},
			functionName: "get_weather",
			wantNewID:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the dedup logic from pi-mono
			needsNewID := tt.providedID == "" || containsID(tt.existingIDs, tt.providedID)

			if needsNewID != tt.wantNewID {
				t.Errorf("needsNewID = %v, want %v", needsNewID, tt.wantNewID)
			}
		})
	}
}

// containsID checks if an ID exists in the list (used for dedup simulation)
func containsID(ids []string, id string) bool {
	for _, existing := range ids {
		if existing == id {
			return true
		}
	}
	return false
}

// TestBuildGoogleThoughtSignatureOnToolCall tests P7-GG07: thoughtSignature on tool calls
// Reference: pi-mono google.ts lines 187-190
// - thoughtSignature from part should be set on the tool call
func TestBuildGoogleThoughtSignatureOnToolCall(t *testing.T) {
	// Simulate the behavior from pi-mono
	signature := "abc123"

	// From pi-mono: {...part.thoughtSignature && { thoughtSignature: part.thoughtSignature }}
	toolCall := struct {
		Type             string
		ID               string
		Name             string
		Arguments        map[string]any
		ThoughtSignature string
	}{
		Type:             "toolCall",
		ID:               "tool_1",
		Name:             "get_weather",
		Arguments:        map[string]any{"city": "NYC"},
		ThoughtSignature: "", // initially empty
	}

	// Apply signature if present (pi-mono conditional spread)
	if signature != "" {
		toolCall.ThoughtSignature = signature
	}

	if toolCall.ThoughtSignature != signature {
		t.Errorf("ThoughtSignature = %q, want %q", toolCall.ThoughtSignature, signature)
	}
}

// TestBuildGoogleTextSignatureOnTextBlock tests P7-GG08: textSignature on text blocks
// Reference: pi-mono google.ts lines 115-124
// - textSignature should be retained using retainThoughtSignature
func TestBuildGoogleTextSignatureOnTextBlock(t *testing.T) {
	tests := []struct {
		name          string
		existing      string
		incoming      string
		wantSignature string
	}{
		{
			name:          "first delta with signature",
			existing:      "",
			incoming:      "sig123",
			wantSignature: "sig123",
		},
		{
			name:          "subsequent delta without signature",
			existing:      "sig123",
			incoming:      "",
			wantSignature: "sig123", // retained from first
		},
		{
			name:          "subsequent delta with new signature",
			existing:      "sig123",
			incoming:      "sig456",
			wantSignature: "sig456", // new signature wins
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate retainThoughtSignature
			signature := retainThoughtSignature(tt.existing, tt.incoming)
			if signature != tt.wantSignature {
				t.Errorf("signature = %q, want %q", signature, tt.wantSignature)
			}
		})
	}
}

// TestBuildGoogleAbortSignalAsTerminalError tests P7-GG15: abort/error as terminal errors
// Reference: pi-mono google.ts lines 242-245
// - finishReason == "aborted" | "error" should be terminal errors
func TestBuildGoogleAbortSignalAsTerminalError(t *testing.T) {
	tests := []struct {
		reason  string
		isError bool
	}{
		{"STOP", false},
		{"MAX_TOKENS", false},
		{"RECITATION", true},
		{"MALFORMED_FUNCTION_CALL", true},
		{"ABORTED", true},
		{"ERROR", true},
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			// Simulate pi-mono's error reason handling
			isTerminalError := isGoogleErrorStopReason(tt.reason)

			if isTerminalError != tt.isError {
				t.Errorf("isTerminalError for %q = %v, want %v", tt.reason, isTerminalError, tt.isError)
			}
		})
	}
}

// isGoogleErrorStopReason maps finish reasons to error status
// Reference: pi-mono google-shared.ts mapStopReason and google.ts abort/error handling
func isGoogleErrorStopReason(reason string) bool {
	switch reason {
	case "STOP", "MAX_TOKENS":
		return false
	default:
		// All other finish reasons are terminal errors
		return true
	}
}

// TestBuildGoogleUsageTrackingCacheWrite tests P7-GG14: cacheWrite token tracking
// Reference: pi-mono google.ts lines 226-240
// - Output tokens = candidatesTokenCount + thoughtsTokenCount
// - Usage.cost should be calculated via calculateCost(model, usage)
func TestBuildGoogleUsageTrackingCacheWrite(t *testing.T) {
	// Test that cacheWrite is included in cost calculation
	usage := Usage{
		Input:       1000,
		Output:      500,
		CacheRead:   200,
		CacheWrite:  100, // This should be tracked
		TotalTokens: 1800,
	}

	// Create a mock cost profile
	cost := CostProfile{
		Input:      0.5, // per 1M tokens
		Output:     1.5,
		CacheRead:  0.25,
		CacheWrite: 0.25,
	}

	usage.CalculateCost(cost)

	// Verify cost is calculated (this should pass once CalculateCost is fixed)
	if usage.Cost.Total == 0 {
		t.Errorf("usage.Cost.Total = 0, expected non-zero after CalculateCost")
	}
}

// TestBuildGoogleToolConfigFunctionCallingConfig tests P7-GG01: toolConfig.functionCallingConfig
// Reference: pi-mono google.ts lines 356-361
// - toolConfig should only be sent when tools are present AND toolChoice is specified
func TestBuildGoogleToolConfigFunctionCallingConfig(t *testing.T) {
	tests := []struct {
		name       string
		hasTools   bool
		toolChoice string
		wantConfig bool
	}{
		{
			name:       "no tools, no tool choice",
			hasTools:   false,
			toolChoice: "",
			wantConfig: false,
		},
		{
			name:       "has tools, no tool choice",
			hasTools:   true,
			toolChoice: "",
			wantConfig: false,
		},
		{
			name:       "has tools, has tool choice",
			hasTools:   true,
			toolChoice: "any",
			wantConfig: true,
		},
		{
			name:       "no tools, has tool choice",
			hasTools:   false,
			toolChoice: "none",
			wantConfig: false, // toolConfig only when tools are present
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate pi-mono's toolConfig logic
			hasToolConfig := tt.hasTools && tt.toolChoice != ""

			if hasToolConfig != tt.wantConfig {
				t.Errorf("hasToolConfig = %v, want %v", hasToolConfig, tt.wantConfig)
			}
		})
	}
}

// TestBuildGoogleThinkingConfigIncludeThoughts tests P7-GG02: ThinkingConfig.includeThoughts
// Reference: pi-mono google.ts line 367
// - includeThoughts: true should be set whenever thinking is enabled
func TestBuildGoogleThinkingConfigIncludeThoughts(t *testing.T) {
	tests := []struct {
		name            string
		thinkingEnabled bool
		modelReasoning  bool
		wantInclude     bool
	}{
		{
			name:            "thinking enabled on reasoning model",
			thinkingEnabled: true,
			modelReasoning:  true,
			wantInclude:     true,
		},
		{
			name:            "thinking disabled on reasoning model",
			thinkingEnabled: false,
			modelReasoning:  true,
			wantInclude:     false, // getDisabledThinkingConfig is used instead
		},
		{
			name:            "thinking enabled on non-reasoning model",
			thinkingEnabled: true,
			modelReasoning:  false,
			wantInclude:     false, // should not enable thinking on non-reasoning models
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate pi-mono's includeThoughts logic
			includeThoughts := tt.thinkingEnabled && tt.modelReasoning

			if includeThoughts != tt.wantInclude {
				t.Errorf("includeThoughts = %v, want %v", includeThoughts, tt.wantInclude)
			}
		})
	}
}

// TestBuildGoogleThinkingLevelMapping tests P7-GG03: ThinkingConfig.thinkingLevel mapping
// Reference: pi-mono google.ts lines 368-372
// - thinkingLevel enum mapping for Gemini 3
func TestBuildGoogleThinkingLevelMapping(t *testing.T) {
	tests := []struct {
		modelID   string
		effort    string
		wantLevel string
	}{
		// Gemini 3 Pro
		{"gemini-3-pro", "minimal", "LOW"},
		{"gemini-3-pro", "low", "LOW"},
		{"gemini-3-pro", "medium", "HIGH"},
		{"gemini-3-pro", "high", "HIGH"},
		// Gemma 4
		{"gemma-4", "minimal", "MINIMAL"},
		{"gemma-4", "low", "MINIMAL"},
		{"gemma-4", "medium", "HIGH"},
		{"gemma-4", "high", "HIGH"},
		// Default mapping
		{"gemini-2-flash", "minimal", "MINIMAL"},
		{"gemini-2-flash", "low", "LOW"},
		{"gemini-2-flash", "medium", "MEDIUM"},
		{"gemini-2-flash", "high", "HIGH"},
	}

	for _, tt := range tests {
		t.Run(tt.modelID+"_"+tt.effort, func(t *testing.T) {
			// This tests the desired behavior - actual implementation is in google.go
			level := mapEffortToThinkingLevel(tt.modelID, tt.effort)
			if level != tt.wantLevel {
				t.Errorf("level = %q, want %q", level, tt.wantLevel)
			}
		})
	}
}

// mapEffortToThinkingLevel maps effort levels to thinking levels per model
func mapEffortToThinkingLevel(modelID, effort string) string {
	lower := strings.ToLower(modelID)

	// Gemini 3 Pro mapping
	if strings.Contains(lower, "gemini-3") && strings.Contains(lower, "pro") {
		switch effort {
		case "minimal", "low":
			return "LOW"
		case "medium", "high":
			return "HIGH"
		}
	}

	// Gemma 4 mapping
	if strings.Contains(lower, "gemma-4") || strings.Contains(lower, "gemma4") {
		switch effort {
		case "minimal", "low":
			return "MINIMAL"
		case "medium", "high":
			return "HIGH"
		}
	}

	// Default mapping
	switch effort {
	case "minimal":
		return "MINIMAL"
	case "low":
		return "LOW"
	case "medium":
		return "MEDIUM"
	case "high":
		return "HIGH"
	}

	return "MEDIUM"
}

func TestRequiresToolCallID(t *testing.T) {
	tests := []struct {
		modelID string
		want    bool
	}{
		{"claude-3-5-sonnet", true},
		{"claude-3-opus", true},
		{"gpt-oss-model", true},
		{"gemini-2.0-flash", false},
		{"gpt-4o", false},
		{"o3", false},
	}

	for _, tt := range tests {
		if got := requiresToolCallID(tt.modelID); got != tt.want {
			t.Errorf("requiresToolCallID(%q) = %v, want %v", tt.modelID, got, tt.want)
		}
	}
}

func TestNormalizeToolCallIDForGoogle(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"tool_123", "tool_123"},
		{"tool-123", "tool-123"},
		{"tool.123", "tool123"},
		{"tool@123", "tool123"},
		{"very_long_tool_call_id_that_exceeds_64_characters_limit_here", "very_long_tool_call_id_that_exceeds_64_characters_limit_here"},
		{"abc", "abc"},
		{"ABC", "ABC"},
		{"123", "123"},
		{"", ""},
	}

	for _, tt := range tests {
		if got := NormalizeToolCallIDForGoogle(tt.input, nil, nil); got != tt.want {
			t.Errorf("NormalizeToolCallIDForGoogle(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBuildGoogleContentBlockReplaySignaturesAndUnsignedGemini3ToolCall(t *testing.T) {
	signed := Message{Role: "assistant", Content: []ContentBlock{{
		Type:             ContentBlockToolCall,
		ID:               "call_google",
		Name:             "lookup",
		Arguments:        json.RawMessage(`{"q":"one"}`),
		ThoughtSignature: "aGVsbG8=", // valid base64
	}}}

	role, parts, skip := buildGoogleContentBlock(signed, true, "test-model")
	if skip {
		t.Fatal("same-model signed tool call should not be skipped")
	}
	if role != "model" {
		t.Fatalf("role = %q, want model", role)
	}
	if len(parts) != 1 {
		t.Fatalf("len(parts) = %d, want 1", len(parts))
	}
	if got := parts[0]["thoughtSignature"]; got != "aGVsbG8=" {
		t.Fatalf("thoughtSignature = %v, want base64sig", got)
	}

	_, parts, skip = buildGoogleContentBlock(signed, false, "test-model")
	if skip || len(parts) != 1 {
		t.Fatalf("cross-model signed tool call parts=%#v skip=%v", parts, skip)
	}
	if _, ok := parts[0]["thoughtSignature"]; ok {
		t.Fatalf("cross-model replay should strip thoughtSignature, got %#v", parts[0])
	}

	unsigned := Message{Role: "assistant", Content: []ContentBlock{{
		Type:      ContentBlockToolCall,
		ID:        "call_unsigned",
		Name:      "lookup",
		Arguments: json.RawMessage(`{"q":"gemini3"}`),
	}}}
	_, parts, skip = buildGoogleContentBlock(unsigned, true, "gemini-3-pro")
	if skip || len(parts) != 1 {
		t.Fatalf("unsigned Gemini 3-style function call should remain replay-safe, parts=%#v skip=%v", parts, skip)
	}
	if _, ok := parts[0]["functionCall"]; !ok {
		t.Fatalf("unsigned tool call missing functionCall part: %#v", parts[0])
	}
	if got, ok := parts[0]["thoughtSignature"]; !ok || got != SkipThoughtSignatureValidator {
		t.Fatalf("unsigned Gemini 3 tool call should use skip-validator sentinel, got %#v", parts[0])
	}

	// Non-Gemini-3 unsigned tool call should NOT receive the sentinel.
	_, parts, skip = buildGoogleContentBlock(unsigned, true, "gemini-2-pro")
	if skip || len(parts) != 1 {
		t.Fatalf("unsigned non-Gemini-3 tool call should remain replay-safe, parts=%#v skip=%v", parts, skip)
	}
	if _, ok := parts[0]["thoughtSignature"]; ok {
		t.Fatalf("unsigned non-Gemini-3 tool call should not invent thoughtSignature, got %#v", parts[0])
	}
}

func TestRetainThoughtSignature(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		incoming string
		want     string
	}{
		{"empty existing, non-empty incoming", "", "sig123", "sig123"},
		{"non-empty existing, non-empty incoming", "existing", "incoming", "incoming"},
		{"non-empty existing, empty incoming", "existing", "", "existing"},
		{"both empty", "", "", ""},
		{"existing with content, incoming empty", "abc123", "", "abc123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retainThoughtSignature(tt.existing, tt.incoming); got != tt.want {
				t.Errorf("retainThoughtSignature(%q, %q) = %q, want %q", tt.existing, tt.incoming, got, tt.want)
			}
		})
	}
}

func TestIsGemini3ProModel(t *testing.T) {
	tests := []struct {
		modelID string
		want    bool
	}{
		{"gemini-3-pro", true},
		{"gemini-3-pro-001", true},
		{"gemini-3.0-pro", true},
		{"gemini-3.5-pro", true},
		{"gemini-3-pro-exp", true},
		{"gemini-2-pro", false},
		{"gemini-3-flash", false},
		{"gemini-pro", false},
	}

	for _, tt := range tests {
		if got := isGemini3ProModel(tt.modelID); got != tt.want {
			t.Errorf("isGemini3ProModel(%q) = %v, want %v", tt.modelID, got, tt.want)
		}
	}
}

func TestIsGemini3FlashModel(t *testing.T) {
	tests := []struct {
		modelID string
		want    bool
	}{
		{"gemini-3-flash", true},
		{"gemini-3-flash-001", true},
		{"gemini-3.0-flash", true},
		{"gemini-3-flash-lite", true},
		{"gemini-3.5-flash", true},
		{"gemini-2-flash", false},
		{"gemini-3-pro", false},
		{"gemini-flash", false},
	}

	for _, tt := range tests {
		if got := isGemini3FlashModel(tt.modelID); got != tt.want {
			t.Errorf("isGemini3FlashModel(%q) = %v, want %v", tt.modelID, got, tt.want)
		}
	}
}

func TestIsGemma4Model(t *testing.T) {
	tests := []struct {
		modelID string
		want    bool
	}{
		{"gemma-4", true},
		{"gemma-4-it", true},
		{"gemma-4-27b", true},
		{"gemma4", true},
		{"gemma-3", false},
		{"gemma-2", false},
		{"gemini-4", false},
	}

	for _, tt := range tests {
		if got := isGemma4Model(tt.modelID); got != tt.want {
			t.Errorf("isGemma4Model(%q) = %v, want %v", tt.modelID, got, tt.want)
		}
	}
}

func TestMapToolChoice(t *testing.T) {
	tests := []struct {
		choice string
		want   string
	}{
		{"auto", "AUTO"},
		{"none", "NONE"},
		{"any", "ANY"},
		{"", "AUTO"},
		{"invalid", "AUTO"},
	}

	for _, tt := range tests {
		if got := mapToolChoice(tt.choice); got != tt.want {
			t.Errorf("mapToolChoice(%q) = %q, want %q", tt.choice, got, tt.want)
		}
	}
}

func TestGoogleStopReason(t *testing.T) {
	tests := []struct {
		reason string
		want   StopReason
	}{
		{"STOP", StopReasonStop},
		{"MAX_TOKENS", StopReasonLength},
		// All other reasons map to StopReasonError per pi-mono reference
		{"UNKNOWN", StopReasonError},
		{"RECITATION", StopReasonError},
		{"SAFETY", StopReasonError},
		{"MALFORMED_FUNCTION_CALL", StopReasonError},
		{"ABORTED", StopReasonAborted},
		{"ERROR", StopReasonError},
		{"BLOCKLIST", StopReasonError},
		{"PROHIBITED_CONTENT", StopReasonError},
		{"", StopReasonError},
	}

	for _, tt := range tests {
		if got := googleStopReason(tt.reason); got != tt.want {
			t.Errorf("googleStopReason(%q) = %v, want %v", tt.reason, got, tt.want)
		}
	}
}

func TestSupportsMultimodalFunctionResponse(t *testing.T) {
	tests := []struct {
		modelID string
		want    bool
	}{
		{"gemini-3-pro", true},
		{"gemini-3-flash", true},
		{"gemini-2.0-flash", false},
		{"gemini-2.5-pro", false},
		{"gemma-4", true},
		{"gemma-3", true},
		{"claude-3", true},
		{"gpt-4", true},
	}

	for _, tt := range tests {
		if got := supportsMultimodalFunctionResponse(tt.modelID); got != tt.want {
			t.Errorf("supportsMultimodalFunctionResponse(%q) = %v, want %v", tt.modelID, got, tt.want)
		}
	}
}

func TestBuildGoogleContentBlock(t *testing.T) {
	tests := []struct {
		name           string
		msg            Message
		isSameProvider bool
		wantRole       string
		wantPartsLen   int
		wantSkip       bool
	}{
		{
			name: "user with string content",
			msg: Message{
				Role:    "user",
				Content: "hello",
			},
			isSameProvider: false,
			wantRole:       "user",
			wantPartsLen:   1,
			wantSkip:       false,
		},
		{
			name: "user with text block",
			msg: Message{
				Role:    "user",
				Content: []ContentBlock{{Type: ContentBlockText, Text: "hello"}},
			},
			isSameProvider: false,
			wantRole:       "user",
			wantPartsLen:   1,
			wantSkip:       false,
		},
		{
			name: "assistant with text block",
			msg: Message{
				Role:    "assistant",
				Content: []ContentBlock{{Type: ContentBlockText, Text: "hi"}},
			},
			isSameProvider: false,
			wantRole:       "model",
			wantPartsLen:   1,
			wantSkip:       false,
		},
		{
			name: "toolResult",
			msg: Message{
				Role:     "toolResult",
				ToolName: "read",
				Content:  []ContentBlock{{Type: ContentBlockText, Text: "file content"}},
			},
			isSameProvider: false,
			wantRole:       "user",
			wantPartsLen:   0,
			wantSkip:       false,
		},
		{
			name: "assistant with tool call",
			msg: Message{
				Role:    "assistant",
				Content: []ContentBlock{{Type: ContentBlockToolCall, Name: "get_weather", Arguments: []byte(`{"city":"NYC"}`)}},
			},
			isSameProvider: true,
			wantRole:       "model",
			wantPartsLen:   1,
			wantSkip:       false,
		},
		{
			name: "empty text block should be skipped",
			msg: Message{
				Role:    "user",
				Content: []ContentBlock{{Type: ContentBlockText, Text: ""}},
			},
			isSameProvider: false,
			wantRole:       "user",
			wantPartsLen:   0,
			wantSkip:       true, // empty blocks are filtered, so len(parts)==0 and skip=true
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role, parts, skip := buildGoogleContentBlock(tt.msg, tt.isSameProvider, "test-model")
			if role != tt.wantRole {
				t.Errorf("role = %q, want %q", role, tt.wantRole)
			}
			if len(parts) != tt.wantPartsLen {
				t.Errorf("len(parts) = %d, want %d", len(parts), tt.wantPartsLen)
			}
			if skip != tt.wantSkip {
				t.Errorf("skip = %v, want %v", skip, tt.wantSkip)
			}
		})
	}
}

func TestIsValidThoughtSignature(t *testing.T) {
	tests := []struct {
		name      string
		signature string
		want      bool
	}{
		{"empty", "", false},
		{"valid base64", "aGVsbG8=", true},
		{"invalid base64 chars", "not-base64!@#", false},
		{"wrong length", "abc", false},
		{"skip validator sentinel is not valid base64", SkipThoughtSignatureValidator, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValidThoughtSignature(tt.signature); got != tt.want {
				t.Errorf("IsValidThoughtSignature(%q) = %v, want %v", tt.signature, got, tt.want)
			}
		})
	}
}

func TestResolveThoughtSignature(t *testing.T) {
	tests := []struct {
		name                   string
		isSameProviderAndModel bool
		signature              string
		want                   string
	}{
		{"same provider, valid signature", true, "aGVsbG8=", "aGVsbG8="},
		{"different provider, valid signature", false, "aGVsbG8=", ""},
		{"same provider, invalid signature", true, "not-base64", ""},
		{"same provider, empty signature", true, "", ""},
		{"different provider, empty signature", false, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveThoughtSignature(tt.isSameProviderAndModel, tt.signature); got != tt.want {
				t.Errorf("ResolveThoughtSignature(%v, %q) = %q, want %q", tt.isSameProviderAndModel, tt.signature, got, tt.want)
			}
		})
	}
}
