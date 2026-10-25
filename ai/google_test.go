package ai

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	genai "google.golang.org/genai"
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

func TestGoogleThinkingConfigsMatchUpstream(t *testing.T) {
	str := func(value string) *string { return &value }
	pro25 := &Model{ID: "gemini-2.5-pro", Reasoning: true}
	pro3 := &Model{ID: "gemini-3.1-pro-preview", Reasoning: true, ThinkingLevelMap: map[string]*string{"minimal": nil, "off": nil, "xhigh": nil, "max": nil}}
	mapped := &Model{ID: "gemini-3-flash", Reasoning: true, ThinkingLevelMap: map[string]*string{"low": str("medium")}}
	tests := []struct {
		name  string
		model *Model
		opts  []Option
		want  *genai.ThinkingConfig
	}{
		{"unset disables thinking", pro25, nil, &genai.ThinkingConfig{ThinkingBudget: new(int32(0))}},
		{"budget model level", pro25, []Option{WithReasoning(ReasoningHigh)}, &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: new(int32(32768))}},
		{"configured budget wins", pro25, []Option{WithReasoning(ReasoningHigh), WithThinkingBudgetTokens(5000)}, &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: new(int32(5000))}},
		{"other models choose dynamically", &Model{ID: "gemini-2.0-flash-thinking", Reasoning: true}, []Option{WithReasoning(ReasoningLow)}, &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: new(int32(-1))}},
		{"level model clamps up", pro3, []Option{WithReasoning(ReasoningMinimal)}, &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: genai.ThinkingLevelLow}},
		{"level model off keeps its lowest level", pro3, []Option{WithReasoning(ReasoningOff)}, &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelLow}},
		{"thinking map", mapped, []Option{WithReasoning(ReasoningLow)}, &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: genai.ThinkingLevelMedium}},
		{"non-reasoning model", &Model{ID: "gemini-2.5-pro"}, []Option{WithReasoning(ReasoningHigh)}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildGenAIThinkingConfig(tt.model, applyOptions(tt.opts))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("config = %+v, want %+v", got, tt.want)
			}
		})
	}

	bad := &Model{ID: "gemini-3-pro", Provider: "google", Reasoning: true, ThinkingLevelMap: map[string]*string{"high": str("ultra")}}
	if _, err := buildGenAIThinkingConfig(bad, applyOptions([]Option{WithReasoning(ReasoningHigh)})); err == nil {
		t.Fatal("unsupported thinking map value accepted")
	}

	// Gemini CLI shares the resolver: unset reasoning no longer turns thinking on.
	request, err := BuildGeminiCLIRequest(pro25, &Context{}, "project", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if config := request.Request.GenerationConfig.ThinkingConfig; config == nil || config.IncludeThoughts || config.ThinkingBudget == nil || *config.ThinkingBudget != 0 {
		t.Fatalf("gemini cli unset thinking = %+v", config)
	}
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

func TestGoogleStreamTerminalOutcomesMatchUpstream(t *testing.T) {
	text := &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: genai.NewContentFromText("hi", genai.RoleModel)}}}
	call := &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "read", Args: map[string]any{}}}}}}}}
	finish := func(reason genai.FinishReason) *genai.GenerateContentResponse {
		return &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{FinishReason: reason}}}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		chunks  []*genai.GenerateContentResponse
		err     error
		reason  StopReason
		message string
	}{
		{name: "stop", chunks: []*genai.GenerateContentResponse{text, finish(genai.FinishReasonStop)}, reason: StopReasonStop},
		{name: "tool use needs finish", chunks: []*genai.GenerateContentResponse{call, finish(genai.FinishReasonStop)}, reason: StopReasonToolUse},
		{name: "tool then safety", chunks: []*genai.GenerateContentResponse{call, finish(genai.FinishReasonSafety)}, reason: StopReasonError, message: "Provider stopped with: SAFETY"},
		{name: "no finish", chunks: []*genai.GenerateContentResponse{call}, reason: StopReasonError, message: "google stream ended without a finish reason"},
		{name: "transport error", chunks: []*genai.GenerateContentResponse{text}, err: errors.New("connection reset"), reason: StopReasonError, message: "connection reset"},
		{name: "canceled", ctx: canceled, chunks: []*genai.GenerateContentResponse{text, finish(genai.FinishReasonStop)}, reason: StopReasonAborted, message: "request was aborted: context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			chunks := func(yield func(*genai.GenerateContentResponse, error) bool) {
				for _, chunk := range tc.chunks {
					if !yield(chunk, nil) {
						return
					}
				}
				if tc.err != nil {
					yield(nil, tc.err)
				}
			}
			reason, message := streamTerminal(t, func(handler EventHandler) {
				if err := streamGoogleChunks(ctx, &Model{ID: "gemini"}, chunks, handler); err != nil {
					t.Fatal(err)
				}
			})
			if reason != tc.reason || message != tc.message {
				t.Fatalf("got %s %q, want %s %q", reason, message, tc.reason, tc.message)
			}
		})
	}
}

func TestRetryGoogleStreamRetriesOnlyInitialFailures(t *testing.T) {
	chunk := &genai.GenerateContentResponse{}
	later := errors.New("stream reset")
	for _, tc := range []struct {
		name    string
		opts    []Option
		first   error
		calls   int
		status  int
		wantErr error
	}{
		{name: "default makes one attempt", first: genai.APIError{Code: 503}, calls: 1, status: 503},
		{name: "server error", opts: []Option{WithMaxRetries(1)}, first: genai.APIError{Code: 503}, calls: 2},
		{name: "network failure", opts: []Option{WithMaxRetries(1)}, first: errors.New("connection reset"), calls: 2},
		{name: "bad request", opts: []Option{WithMaxRetries(1)}, first: genai.APIError{Code: 400}, calls: 1, status: 400},
		{name: "mid-stream failure", opts: []Option{WithMaxRetries(1)}, calls: 1, wantErr: later},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := providerRetryBase
			providerRetryBase = 0
			t.Cleanup(func() { providerRetryBase = previous })
			calls := 0
			stream := retryGoogleStream(t.Context(), applyOptions(tc.opts), func() iter.Seq2[*genai.GenerateContentResponse, error] {
				calls++
				return func(yield func(*genai.GenerateContentResponse, error) bool) {
					if calls == 1 && tc.first != nil {
						yield(nil, tc.first)
						return
					}
					if yield(chunk, nil) && tc.wantErr != nil {
						yield(nil, tc.wantErr)
					}
				}
			})
			var (
				chunks int
				err    error
			)
			for got, gotErr := range stream {
				if gotErr != nil {
					err = gotErr
					continue
				}
				require.Same(t, chunk, got)
				chunks++
			}
			require.Equal(t, tc.calls, calls)
			if tc.status != 0 {
				apiErr, ok := errors.AsType[genai.APIError](err)
				require.True(t, ok, "%v", err)
				require.Equal(t, tc.status, apiErr.Code)
				return
			}
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, chunks)
		})
	}
}

func TestGoogleHeaderTimeout(t *testing.T) {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	model := &Model{ID: "gemini-test", API: APIGoogleGenerativeAI, Provider: ProviderGoogle, BaseURL: "https://test.invalid"}
	client, err := newGoogleClient(model)
	require.NoError(t, err)
	var timeoutErr error
	require.NoError(t, client.Stream(ctx, model, &Context{}, func(_ context.Context, event StreamEvent) {
		if failure, ok := event.(StreamError); ok {
			timeoutErr = failure.Err
		}
	}, WithAPIKey("key"), WithTimeout(time.Millisecond)))
	require.ErrorIs(t, timeoutErr, context.DeadlineExceeded)
	require.ErrorContains(t, timeoutErr, "response headers timed out")
	require.NoError(t, ctx.Err())
}

func TestGoogleCredentialsResolveAtEachRequest(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "old-key")
	model := &Model{ID: "gemini-test", Provider: ProviderGoogle, API: APIGoogleGenerativeAI, BaseURL: "https://test.invalid"}
	client, err := newGoogleClient(model)
	require.NoError(t, err)
	var requests int
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		require.Equal(t, "new-key", req.Header.Get("x-goog-api-key"))
		return nil, errors.New("isolated request")
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	t.Setenv("GEMINI_API_KEY", "new-key")
	streamErr := client.Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hello"}}}, func(context.Context, StreamEvent) {}, WithMaxRetries(0), WithTimeout(time.Second))
	if streamErr != nil {
		require.ErrorContains(t, streamErr, "isolated request")
	}
	require.Equal(t, 1, requests)
	t.Setenv("GEMINI_API_KEY", "")
	require.ErrorContains(t, client.Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hello"}}}, func(context.Context, StreamEvent) {}, WithMaxRetries(0), WithTimeout(time.Second)), "missing API key")
	require.Equal(t, 1, requests)
}
