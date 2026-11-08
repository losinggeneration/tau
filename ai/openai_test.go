package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// zaiTestModel returns a model configured like the generated "zai/glm-5.2"
// entry: OpenAI-completions API, ZAI thinking format.
func zaiTestModel() *Model {
	return &Model{
		ID:        "glm-5.2",
		API:       APIOpenAICompletions,
		Provider:  ProviderZAI,
		BaseURL:   "https://api.z.ai/api/coding/paas/v4",
		Reasoning: true,
		Compat: &CompatibilitySettings{
			MaxTokensField: "max_tokens",
			ThinkingFormat: ThinkingFormatZAI,
			ZAIToolStream:  true,
		},
	}
}

// opencodeGoKimiK26TestModel returns a model configured like the generated
// "opencode-go/kimi-k2.6" entry after the deepseek thinking-format fix.
func opencodeGoKimiK26TestModel() *Model {
	return &Model{
		ID:        "kimi-k2.6",
		API:       APIOpenAICompletions,
		Provider:  ProviderOpenCodeGo,
		BaseURL:   "https://opencode.ai/zen/go/v1",
		Reasoning: true,
		Compat: &CompatibilitySettings{
			MaxTokensField:          "max_tokens",
			ThinkingFormat:          ThinkingFormatDeepSeek,
			SupportsReasoningEffort: new(false),
		},
	}
}

func newOpenAIClientForTest(t *testing.T, model *Model) *openAIClient {
	t.Helper()
	c, err := newOpenAIClient(model)
	if err != nil {
		t.Fatalf("newOpenAIClient: %v", err)
	}
	return c
}

// TestOpenAIBuildRequestZAIThinkingObject verifies z.ai gets upstream's
// thinking object: explicitly disabled with no reasoning (z.ai otherwise
// thinks and GLM-5.x returns reasoning-only turns), and enabled without
// clearing earlier thinking when a level is requested.
func TestOpenAIBuildRequestZAIThinkingObject(t *testing.T) {
	client := newOpenAIClientForTest(t, zaiTestModel())
	for _, tc := range []struct {
		reasoning ReasoningLevel
		want      string
	}{
		{"", `{"type":"disabled"}`},
		{ReasoningOff, `{"type":"disabled"}`},
		{ReasoningHigh, `{"clear_thinking":false,"type":"enabled"}`},
	} {
		reqBody, err := client.buildRequest(client.model, &Context{Messages: []Message{{Role: "user", Content: "hi"}}}, &options{reasoning: tc.reasoning})
		if err != nil {
			t.Fatalf("buildRequest: %v", err)
		}
		var req map[string]json.RawMessage
		if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if string(req["thinking"]) != tc.want || req["enable_thinking"] != nil {
			t.Fatalf("reasoning %q body = %s, want thinking %s", tc.reasoning, reqBody, tc.want)
		}
	}
}

// TestOpenAIBuildRequestKimiK26DeepSeekDisabledThinking verifies that
// opencode-go/kimi-k2.6 (deepseek thinking format) sends
// `thinking: { type: "disabled" }` when no reasoning effort is set, matching
// pi-mono. pi-mono's generator marks kimi-k2.6 with thinkingFormat:"deepseek"
// because the OpenCode kimi backend rejects string thinking values or combined
// reasoning_effort.
func TestOpenAIBuildRequestKimiK26DeepSeekDisabledThinking(t *testing.T) {
	client := newOpenAIClientForTest(t, opencodeGoKimiK26TestModel())
	reqBody, err := client.buildRequest(client.model, &Context{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, &options{})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req map[string]json.RawMessage
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	raw, ok := req["thinking"]
	if !ok {
		t.Fatalf("thinking not set; want {type:disabled}. body: %s", reqBody)
	}
	var think struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &think); err != nil {
		t.Fatalf("thinking not an object: %v", err)
	}
	if think.Type != "disabled" {
		t.Fatalf("thinking.type = %q, want %q", think.Type, "disabled")
	}
	// supportsReasoningEffort is false, so reasoning_effort must NOT be sent.
	if _, ok := req["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort should be absent for kimi-k2.6; body: %s", reqBody)
	}
}

// TestOpenAIBuildRequestKimiK26DeepSeekEnabledThinking verifies the on path
// sends thinking: { type: "enabled" } and no reasoning_effort.
func TestOpenAIBuildRequestKimiK26DeepSeekEnabledThinking(t *testing.T) {
	client := newOpenAIClientForTest(t, opencodeGoKimiK26TestModel())
	o := options{reasoning: ReasoningHigh}
	reqBody, err := client.buildRequest(client.model, &Context{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, &o)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req map[string]json.RawMessage
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	raw, ok := req["thinking"]
	if !ok {
		t.Fatalf("thinking not set; want {type:enabled}. body: %s", reqBody)
	}
	var think struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &think); err != nil {
		t.Fatalf("thinking not an object: %v", err)
	}
	if think.Type != "enabled" {
		t.Fatalf("thinking.type = %q, want %q", think.Type, "enabled")
	}
	if _, ok := req["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort should be absent for kimi-k2.6; body: %s", reqBody)
	}
}

// TestOpenAIBuildRequestDefaultOpenAIDoesNotSendThinking guards against
// regressions where the new disable branch leaks into the default/OpenAI
// thinking format (which should send neither enable_thinking nor thinking
// when no reasoning is requested).
func TestOpenAIBuildRequestDefaultOpenAIDoesNotSendThinking(t *testing.T) {
	model := &Model{
		ID:       "gpt-4o",
		API:      APIOpenAICompletions,
		Provider: ProviderOpenAI,
		BaseURL:  "https://api.openai.com/v1",
	}
	client := newOpenAIClientForTest(t, model)
	reqBody, err := client.buildRequest(model, &Context{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, &options{})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if strings.Contains(reqBody, "enable_thinking") {
		t.Fatalf("default OpenAI request should not contain enable_thinking: %s", reqBody)
	}
	if strings.Contains(reqBody, "\"thinking\"") {
		t.Fatalf("default OpenAI request should not contain a thinking object: %s", reqBody)
	}
}

// TestOpenAIBuildRequestNormalizesAssistantToolCallIDs reproduces the
// cross-provider switch failure (gpt-5.5 → opencode-go/kimi-k3): tool call
// IDs recorded by the OpenAI Responses API use the composite
// `call_id|item_id` format. buildOpenAIChatCompletionsRequest normalized the
// toolResult ToolCallID but left the assistant toolCall block ID untouched,
// so the request paired tool_calls[].id "call_x|fc_y" with a tool message
// tool_call_id "call_x" — which the provider rejects with
// "tool_call_id ... is not found".
func TestOpenAIBuildRequestNormalizesAssistantToolCallIDs(t *testing.T) {
	compositeID := "call_abc123|fc_def456"
	ctx := &Context{
		Messages: []Message{
			{Role: "user", Content: "run the tool"},
			{
				Role:     "assistant",
				Provider: ProviderOpenAI,
				API:      APIOpenAIResponses,
				Model:    "gpt-5.5",
				Content: []ContentBlock{
					{
						Type:      ContentBlockToolCall,
						ID:        compositeID,
						Name:      "bash",
						Arguments: json.RawMessage(`{"command":"ls"}`),
					},
				},
			},
			{
				Role:       "toolResult",
				ToolCallID: compositeID,
				ToolName:   "bash",
				Content:    "ok",
			},
			{Role: "user", Content: "now switch models"},
		},
	}

	client := newOpenAIClientForTest(t, opencodeGoKimiK26TestModel())
	reqBody, err := client.buildRequest(client.model, ctx, &options{})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		t.Fatalf("unmarshal request: %v\nbody: %s", err, reqBody)
	}

	var toolCallIDs, toolResultIDs []string
	for _, m := range req.Messages {
		for _, tc := range m.ToolCalls {
			toolCallIDs = append(toolCallIDs, tc.ID)
		}
		if m.Role == "tool" {
			toolResultIDs = append(toolResultIDs, m.ToolCallID)
		}
	}

	if len(toolCallIDs) != 1 || len(toolResultIDs) != 1 {
		t.Fatalf("want 1 tool call and 1 tool result, got %v / %v\nbody: %s", toolCallIDs, toolResultIDs, reqBody)
	}
	if toolCallIDs[0] != toolResultIDs[0] {
		t.Fatalf("tool_call_id mismatch: tool_calls[].id = %q, tool message tool_call_id = %q\nbody: %s",
			toolCallIDs[0], toolResultIDs[0], reqBody)
	}
	if strings.Contains(toolCallIDs[0], "|") {
		t.Fatalf("tool call ID still in composite Responses format: %q", toolCallIDs[0])
	}
}

// TestStreamOpenAIResponseAccumulatesTextAcrossChunks verifies that
// streamOpenAIResponse accumulates streamed text deltas into the final
// AssistantMessage.Content and emits a single Done with the full text,
// rather than emitting Done with empty Content per chunk (the prior bug
// that made reasoning-only models like opencode-go kimi-k2.6 / glm-5.x
// appear empty in the TUI).
func TestStreamOpenAIResponseAccumulatesTextAcrossChunks(t *testing.T) {
	// Build a fake SSE stream: role chunk, several text deltas, finish chunk.
	chunks := []string{
		`{"id":"r1","model":"kimi-k2.6","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"id":"r1","model":"kimi-k2.6","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`{"id":"r1","model":"kimi-k2.6","choices":[{"index":0,"delta":{"content":"!"}}]}`,
		`{"id":"r1","model":"kimi-k2.6","choices":[{"index":0,"delta":{"content":" How"}}]}`,
		`{"id":"r1","model":"kimi-k2.6","choices":[{"index":0,"delta":{"content":" can I help?"}}]}`,
		`{"id":"r1","model":"kimi-k2.6","choices":[{"index":0,"finish_reason":"stop","delta":{}}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`,
	}
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString("data: ")
		sb.WriteString(c)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")

	model := &Model{
		ID:        "kimi-k2.6",
		API:       APIOpenAICompletions,
		Provider:  ProviderOpenCodeGo,
		BaseURL:   "https://opencode.ai/zen/go/v1",
		Reasoning: true,
		Compat: &CompatibilitySettings{
			MaxTokensField: "max_tokens",
			ThinkingFormat: ThinkingFormatDeepSeek,
		},
	}

	var textDeltas []string
	var doneCount int
	var finalContent string
	var finalStopReason StopReason
	err := streamOpenAIResponse(context.Background(), model, strings.NewReader(sb.String()), func(_ context.Context, ev StreamEvent) {
		switch e := ev.(type) {
		case TextDelta:
			textDeltas = append(textDeltas, e.Delta)
		case Done:
			doneCount++
			finalStopReason = e.Reason
			finalContent = ""
			for _, b := range e.Message.Content {
				if b.Type == ContentBlockText {
					finalContent += b.Text
				}
			}
		}
	})
	if err != nil {
		t.Fatalf("streamOpenAIResponse: %v", err)
	}
	if doneCount != 1 {
		t.Fatalf("Done emitted %d times, want 1 (per-chunk Done was the accumulation bug)", doneCount)
	}
	wantText := "Hello! How can I help?"
	if finalContent != wantText {
		t.Fatalf("final text = %q, want %q", finalContent, wantText)
	}
	if finalStopReason != StopReasonStop {
		t.Fatalf("stop reason = %q, want %q", finalStopReason, StopReasonStop)
	}
	if strings.Join(textDeltas, "") != wantText {
		t.Fatalf("text deltas = %q, want %q", strings.Join(textDeltas, ""), wantText)
	}
}

// TestStreamOpenAIResponseAccumulatesThinkingThenText verifies that when a
// reasoning model emits reasoning_content followed by visible content, the
// final AssistantMessage.Content has both a thinking block and a text block.
func TestStreamOpenAIResponseAccumulatesThinkingThenText(t *testing.T) {
	chunks := []string{
		`{"id":"r1","model":"glm-5.2","choices":[{"index":0,"delta":{"reasoning_content":"Let me think."}}]}`,
		`{"id":"r1","model":"glm-5.2","choices":[{"index":0,"delta":{"reasoning_content":" Done."}}]}`,
		`{"id":"r1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"Hi!"}}]}`,
		`{"id":"r1","model":"glm-5.2","choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`,
	}
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString("data: ")
		sb.WriteString(c)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")

	model := &Model{
		ID:        "glm-5.2",
		API:       APIOpenAICompletions,
		Provider:  ProviderZAI,
		BaseURL:   "https://api.z.ai/api/coding/paas/v4",
		Reasoning: true,
		Compat: &CompatibilitySettings{
			MaxTokensField: "max_tokens",
			ThinkingFormat: ThinkingFormatZAI,
		},
	}

	var done *Done
	err := streamOpenAIResponse(context.Background(), model, strings.NewReader(sb.String()), func(_ context.Context, ev StreamEvent) {
		if d, ok := ev.(Done); ok {
			done = &d
		}
	})
	if err != nil {
		t.Fatalf("streamOpenAIResponse: %v", err)
	}
	if done == nil {
		t.Fatal("Done not emitted")
	}
	if len(done.Message.Content) != 2 {
		t.Fatalf("Content blocks = %d, want 2", len(done.Message.Content))
	}
	if done.Message.Content[0].Type != ContentBlockThinking {
		t.Fatalf("block 0 type = %q, want %q", done.Message.Content[0].Type, ContentBlockThinking)
	}
	if done.Message.Content[0].Thinking != "Let me think. Done." {
		t.Fatalf("thinking = %q", done.Message.Content[0].Thinking)
	}
	if done.Message.Content[1].Type != ContentBlockText {
		t.Fatalf("block 1 type = %q, want %q", done.Message.Content[1].Type, ContentBlockText)
	}
	if done.Message.Content[1].Text != "Hi!" {
		t.Fatalf("text = %q, want %q", done.Message.Content[1].Text, "Hi!")
	}
}

// TestOpenAIBuildRequestOmitsSessionAffinityHeadersByDefault verifies the
// session_id/x-client-request-id HTTP headers are NOT added to the request
// when SendSessionAffinityHeaders is false (the default, matching pi-mono).
// opencode.ai/zen/go returns HTTP 500 when the session_id header is present
// on models like kimi-k2.6 / glm-5.x.
func TestOpenAIBuildRequestOmitsSessionAffinityHeadersByDefault(t *testing.T) {
	model := zaiTestModel() // SendSessionAffinityHeaders unset => false
	client := newOpenAIClientForTest(t, model)

	o := options{}
	o.sessionID = "test-session-id"
	o.cacheRetention = "short" // ensure cacheSessionID is non-empty

	reqBody, err := client.buildRequest(model, &Context{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, &o)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if strings.Contains(reqBody, "session_id") || strings.Contains(reqBody, "x-client-request-id") {
		// The request body never contains these anyway; this is a header test.
	}
	// Inspect transport headers by simulating what openAIClient.Stream does:
	// headers := sessionHeaders(model, &o) gated by model.GetCompat().SendSessionAffinityHeaders.
	gotHeaders := map[string]string{}
	if model.GetCompat().SendSessionAffinityHeaders {
		for k, v := range sessionHeaders(model, &o) {
			gotHeaders[k] = v
		}
	}
	if len(gotHeaders) != 0 {
		t.Fatalf("expected no session-affinity headers when SendSessionAffinityHeaders=false; got %v", gotHeaders)
	}
}

// TestOpenAIBuildRequestSendsSessionAffinityHeadersWhenEnabled verifies the
// session_id/x-client-request-id headers ARE returned when a model opts in
// (matching pi-mono's cloudflare-ai-gateway models that set
// sendSessionAffinityHeaders: true).
func TestOpenAIBuildRequestSendsSessionAffinityHeadersWhenEnabled(t *testing.T) {
	model := &Model{
		ID:        "r2",
		API:       APIOpenAICompletions,
		Provider:  ProviderCloudflareWorkersAI,
		BaseURL:   "https://api.cloudflare.com/client/v4/accounts/x/ai",
		Reasoning: false,
		Compat: &CompatibilitySettings{
			MaxTokensField:             "max_tokens",
			SendSessionAffinityHeaders: true,
		},
	}
	_ = newOpenAIClientForTest(t, model)

	o := options{}
	o.sessionID = "test-session-id"
	o.cacheRetention = "short"

	if !model.GetCompat().SendSessionAffinityHeaders {
		t.Fatal("SendSessionAffinityHeaders should be true")
	}
	hdrs := sessionHeaders(&Model{}, &o)
	if hdrs["session_id"] != "test-session-id" {
		t.Fatalf("session_id header = %q, want %q", hdrs["session_id"], "test-session-id")
	}
	if hdrs["x-client-request-id"] != "test-session-id" {
		t.Fatalf("x-client-request-id header = %q, want %q", hdrs["x-client-request-id"], "test-session-id")
	}
}

// --- P7-OC01: reasoning_details parsing for thoughtSignature on tool calls ---

func TestStreamOpenAICompatParsesReasoningDetailsForThoughtSignature(t *testing.T) {
	// Simulate a stream with a tool call followed by reasoning_details
	chunks := []string{
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"read","arguments":""}}]}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.encrypted","id":"call_abc","data":"encrypted_data_123"}]}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	stream := strings.NewReader(strings.Join(chunks, "\n"))

	var events []StreamEvent
	var doneEvents []Done
	err := streamOpenAICompatResponse(context.Background(), &Model{ID: "o3", Provider: ProviderOpenAI}, stream, func(_ context.Context, event StreamEvent) {
		events = append(events, event)
		if d, ok := event.(Done); ok {
			doneEvents = append(doneEvents, d)
		}
	}, ProviderOpenAI)
	if err != nil {
		t.Fatalf("streamOpenAICompatResponse: %v", err)
	}
	if len(doneEvents) == 0 {
		t.Fatal("no Done event received")
	}

	msg := doneEvents[0].Message
	// Find the tool call block
	var foundTC *ContentBlock
	for i := range msg.Content {
		if msg.Content[i].Type == ContentBlockToolCall {
			foundTC = &msg.Content[i]
			break
		}
	}
	if foundTC == nil {
		t.Fatal("no tool call block found in output")
	}
	if foundTC.ThoughtSignature == "" {
		t.Fatal("ThoughtSignature is empty, want reasoning_details data")
	}

	assertStreamGolden(t, events, streamGolden{StopReason: StopReasonToolUse})

	// Verify the signature is valid JSON with expected fields
	var detail map[string]any
	if err := json.Unmarshal([]byte(foundTC.ThoughtSignature), &detail); err != nil {
		t.Fatalf("ThoughtSignature is not valid JSON: %v", err)
	}
	if detail["type"] != "reasoning.encrypted" {
		t.Fatalf("detail.type = %v, want reasoning.encrypted", detail["type"])
	}
	if detail["id"] != "call_abc" {
		t.Fatalf("detail.id = %v, want call_abc", detail["id"])
	}
	if detail["data"] != "encrypted_data_123" {
		t.Fatalf("detail.data = %v, want encrypted_data_123", detail["data"])
	}
}

func TestConvertMessagesForOpenAICompatForwardsReasoningDetails(t *testing.T) {
	// When an assistant message has tool calls with thoughtSignature,
	// the converted message should include reasoning_details
	sig := `{"type":"reasoning.encrypted","id":"call_abc","data":"enc_data"}`
	blocks := []ContentBlock{
		{Type: ContentBlockToolCall, ID: "call_abc", Name: "read", Arguments: json.RawMessage(`{"path":"/tmp"}`), ThoughtSignature: sig},
	}
	msg := Message{Role: "assistant", Content: blocks}
	aiCtx := &Context{Messages: []Message{msg}}
	compat := &OpenAICompletionsCompat{}

	messages := convertMessagesForOpenAICompat(aiCtx, &Model{ID: "o3", Provider: ProviderOpenAI}, compat)

	// Find the assistant message
	var assistantMsg map[string]any
	for _, m := range messages {
		if m["role"] == "assistant" {
			assistantMsg = m
			break
		}
	}
	if assistantMsg == nil {
		t.Fatal("no assistant message found")
	}

	rd, ok := assistantMsg["reasoning_details"].([]any)
	if !ok || len(rd) == 0 {
		t.Fatal("reasoning_details not found or empty in assistant message")
	}

	detail, ok := rd[0].(map[string]any)
	if !ok {
		t.Fatalf("reasoning_details[0] type = %T, want map[string]any", rd[0])
	}
	if detail["type"] != "reasoning.encrypted" {
		t.Fatalf("detail.type = %v, want reasoning.encrypted", detail["type"])
	}
}

// --- P7-OC03: developer role when model.reasoning && compat.supportsDeveloperRole ---

func TestBuildOpenAICompatRequestBodyUsesDeveloperRoleForReasoningModels(t *testing.T) {
	model := &Model{
		ID:        "o3",
		API:       APIOpenAICompletions,
		Provider:  ProviderOpenAI,
		Reasoning: true,
	}
	body := requireBody(t)(buildOpenAICompatRequestBody(model, &Context{
		SystemPrompt: "You are helpful.",
		Messages:     []Message{{Role: "user", Content: "hello"}},
	}, &options{}))

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("messages = %v", body["messages"])
	}
	if messages[0].(map[string]any)["role"] != "developer" {
		t.Fatalf("first message role = %v, want developer", messages[0].(map[string]any)["role"])
	}
}

func TestBuildOpenAICompatRequestBodyUsesSystemRoleForNonReasoningModels(t *testing.T) {
	model := &Model{
		ID:       "gpt-4o",
		API:      APIOpenAICompletions,
		Provider: ProviderOpenAI,
	}
	body := requireBody(t)(buildOpenAICompatRequestBody(model, &Context{
		SystemPrompt: "You are helpful.",
		Messages:     []Message{{Role: "user", Content: "hello"}},
	}, &options{}))

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("messages = %v", body["messages"])
	}
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("first message role = %v, want system", messages[0].(map[string]any)["role"])
	}
}

// --- P7-OC06: store: false when compat.supportsStore ---

func TestBuildOpenAICompatRequestBodySetsStoreFalse(t *testing.T) {
	model := &Model{ID: "gpt-4o", Provider: ProviderOpenAI}
	body := requireBody(t)(buildOpenAICompatRequestBody(model, &Context{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, &options{}))

	store, ok := body["store"]
	if !ok {
		t.Fatal("store field not set")
	}
	if store != false {
		t.Fatalf("store = %v, want false", store)
	}
}

// --- P7-OC07: stream_options: { include_usage: true } ---

func TestBuildOpenAICompatRequestBodyIncludesStreamOptions(t *testing.T) {
	model := &Model{ID: "gpt-4o", Provider: ProviderOpenAI}
	body := requireBody(t)(buildOpenAICompatRequestBody(model, &Context{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, &options{}))

	so, ok := body["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options type = %T", body["stream_options"])
	}
	if so["include_usage"] != true {
		t.Fatal("stream_options.include_usage = false, want true")
	}
}

// --- P7-OC08: max_completion_tokens vs max_tokens ---

func TestBuildOpenAICompatRequestBodyUsesMaxCompletionTokens(t *testing.T) {
	model := &Model{ID: "gpt-4o", Provider: ProviderOpenAI}
	body := requireBody(t)(buildOpenAICompatRequestBody(model, &Context{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, &options{maxTokens: 4096}))

	if _, ok := body["max_completion_tokens"]; !ok {
		t.Fatal("max_completion_tokens not set")
	}
	if body["max_completion_tokens"] != float64(4096) {
		t.Fatalf("max_completion_tokens = %v, want 4096", body["max_completion_tokens"])
	}
	if _, ok := body["max_tokens"]; ok {
		t.Fatal("max_tokens should not be set when using max_completion_tokens")
	}
}

// --- P7-OC09: strict: false on tool definitions ---

func TestBuildOpenAICompatRequestBodySetsStrictFalseOnTools(t *testing.T) {
	model := &Model{ID: "gpt-4o", Provider: ProviderOpenAI}
	body := requireBody(t)(buildOpenAICompatRequestBody(model, &Context{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools: []Tool{{
			Name:        "read",
			Description: "Read a file",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
	}, &options{}))

	tools, ok := body["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools = %v", body["tools"])
	}
	if strict := tools[0].(map[string]any)["function"].(map[string]any)["strict"]; strict != false {
		t.Fatalf("tools[0].function.strict = %v, want false", strict)
	}
}

// --- P7-OC16: tool result name field ---

func TestConvertMessagesForOpenAICompatIncludesToolResultName(t *testing.T) {
	aiCtx := &Context{
		Messages: []Message{
			{Role: "assistant", Content: []ContentBlock{
				{Type: ContentBlockToolCall, ID: "call_1", Name: "read", Arguments: json.RawMessage(`{}`)},
			}},
			{Role: "toolResult", ToolCallID: "call_1", ToolName: "read", Content: "file content"},
		},
	}
	compat := &OpenAICompletionsCompat{RequiresToolResultName: true}

	messages := convertMessagesForOpenAICompat(aiCtx, &Model{ID: "test"}, compat)

	// Find the tool result message
	var toolMsg map[string]any
	for _, m := range messages {
		if m["role"] == "tool" {
			toolMsg = m
			break
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool result message found")
	}
	if toolMsg["name"] != "read" {
		t.Fatalf("tool result name = %v, want read", toolMsg["name"])
	}
}

// --- P7-OC18: empty text block filtering ---

func TestConvertMessagesForOpenAICompatFiltersEmptyTextBlocks(t *testing.T) {
	aiCtx := &Context{
		Messages: []Message{
			{Role: "assistant", Content: []ContentBlock{
				{Type: ContentBlockText, Text: ""},
				{Type: ContentBlockText, Text: "real content"},
			}},
		},
	}
	compat := &OpenAICompletionsCompat{}

	messages := convertMessagesForOpenAICompat(aiCtx, &Model{ID: "test"}, compat)

	// Find the assistant message
	var assistantMsg map[string]any
	for _, m := range messages {
		if m["role"] == "assistant" {
			assistantMsg = m
			break
		}
	}
	if assistantMsg == nil {
		t.Fatal("no assistant message found")
	}
	content, ok := assistantMsg["content"].(string)
	if !ok {
		t.Fatalf("content type = %T", assistantMsg["content"])
	}
	if content != "real content" {
		t.Fatalf("content = %v, want real content", content)
	}
}

// --- P7-OC19: choice.usage fallback ---

func TestStreamOpenAICompatHandlesStringContentDelta(t *testing.T) {
	chunks := []string{
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	stream := strings.NewReader(strings.Join(chunks, "\n"))

	var textDeltas []string
	var doneEvents []Done
	err := streamOpenAICompatResponse(context.Background(), &Model{ID: "glm-5.2", Provider: ProviderZAI}, stream, func(_ context.Context, event StreamEvent) {
		switch e := event.(type) {
		case TextDelta:
			textDeltas = append(textDeltas, e.Delta)
		case Done:
			doneEvents = append(doneEvents, e)
		}
	}, ProviderZAI)
	if err != nil {
		t.Fatalf("streamOpenAICompatResponse: %v", err)
	}
	if len(textDeltas) != 1 || textDeltas[0] != "hello" {
		t.Fatalf("textDeltas = %v, want [hello]", textDeltas)
	}
	if len(doneEvents) == 0 {
		t.Fatal("no Done event received")
	}
	msg := doneEvents[0].Message
	if len(msg.Content) != 1 || msg.Content[0].Type != ContentBlockText || msg.Content[0].Text != "hello" {
		t.Fatalf("message content = %#v, want text hello", msg.Content)
	}
}

func TestStreamOpenAICompatFallsBackToChoiceUsage(t *testing.T) {
	// Simulate a stream where usage is in choice.usage, not top-level
	chunks := []string{
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"hello"}]},"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	stream := strings.NewReader(strings.Join(chunks, "\n"))

	var doneEvents []Done
	err := streamOpenAICompatResponse(context.Background(), &Model{ID: "gpt-4o", Provider: ProviderOpenAI, Cost: CostProfile{Input: 1, Output: 2}}, stream, func(_ context.Context, event StreamEvent) {
		if d, ok := event.(Done); ok {
			doneEvents = append(doneEvents, d)
		}
	}, ProviderOpenAI)
	if err != nil {
		t.Fatalf("streamOpenAICompatResponse: %v", err)
	}
	if len(doneEvents) == 0 {
		t.Fatal("no Done event received")
	}

	msg := doneEvents[0].Message
	if msg.Usage.Input != 10 {
		t.Fatalf("usage.input = %d, want 10", msg.Usage.Input)
	}
	if msg.Usage.Output != 5 {
		t.Fatalf("usage.output = %d, want 5", msg.Usage.Output)
	}
}

// --- P7-OC20: unknown finish_reason maps to StopReasonError ---

func TestMapOpenAICompatStopReason(t *testing.T) {
	tests := []struct {
		reason          string
		wantStop        StopReason
		wantErrContains string
	}{
		{"stop", StopReasonStop, ""},
		{"end", StopReasonStop, ""},
		{"length", StopReasonLength, ""},
		{"tool_calls", StopReasonToolUse, ""},
		{"function_call", StopReasonToolUse, ""},
		{"content_filter", StopReasonError, "content_filter"},
		{"network_error", StopReasonError, "network_error"},
		{"unknown_reason", StopReasonError, "unknown_reason"},
		{"", StopReasonError, ""},
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			stopReason, errMsg := mapOpenAICompatStopReason(tt.reason)
			if stopReason != tt.wantStop {
				t.Errorf("stopReason = %v, want %v", stopReason, tt.wantStop)
			}
			if tt.wantErrContains != "" && !strings.Contains(errMsg, tt.wantErrContains) {
				t.Errorf("errMsg = %q, want to contain %q", errMsg, tt.wantErrContains)
			}
		})
	}
}

func TestStreamOpenAICompatMapsUnknownFinishReasonToError(t *testing.T) {
	chunks := []string{
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"hi"}]}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"finish_reason":"content_filter"}]}`,
		`data: [DONE]`,
	}
	stream := strings.NewReader(strings.Join(chunks, "\n"))

	var errorEvents []StreamError
	err := streamOpenAICompatResponse(context.Background(), &Model{ID: "gpt-4o", Provider: ProviderOpenAI}, stream, func(_ context.Context, event StreamEvent) {
		switch e := event.(type) {
		case Done:
			t.Fatalf("finish_reason error ended with Done: %+v", e)
		case StreamError:
			errorEvents = append(errorEvents, e)
		}
	}, ProviderOpenAI)
	if err != nil {
		t.Fatalf("streamOpenAICompatResponse: %v", err)
	}
	if len(errorEvents) != 1 {
		t.Fatalf("error events = %d, want 1", len(errorEvents))
	}

	msg := errorEvents[0].Msg
	if msg.StopReason != StopReasonError {
		t.Fatalf("stopReason = %v, want error", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "content_filter") {
		t.Fatalf("errorMessage = %q, want to contain content_filter", msg.ErrorMessage)
	}
}

// --- P7-OC05: synthetic assistant after tool result ---

func TestConvertMessagesForOpenAICompatInjectsSyntheticAssistant(t *testing.T) {
	aiCtx := &Context{
		Messages: []Message{
			{Role: "assistant", Content: []ContentBlock{
				{Type: ContentBlockToolCall, ID: "call_1", Name: "read", Arguments: json.RawMessage(`{}`)},
			}},
			{Role: "toolResult", ToolCallID: "call_1", Content: "result"},
			{Role: "user", Content: "next question"},
		},
	}
	compat := &OpenAICompletionsCompat{RequiresAssistantAfterToolResult: true}

	messages := convertMessagesForOpenAICompat(aiCtx, &Model{ID: "test"}, compat)

	// Find the tool result message and verify an assistant bridge follows it
	toolResultIdx := -1
	for i, m := range messages {
		if m["role"] == "tool" {
			toolResultIdx = i
		}
	}
	if toolResultIdx < 0 {
		t.Fatal("no tool result message found")
	}
	if toolResultIdx+1 >= len(messages) {
		t.Fatal("no message after tool result")
	}
	if messages[toolResultIdx+1]["role"] != "assistant" {
		t.Fatalf("message after tool result role = %v, want assistant", messages[toolResultIdx+1]["role"])
	}
}

// --- P7-OC15: thinking blocks converted to text when requiresThinkingAsText ---

func TestConvertMessagesForOpenAICompatConvertsThinkingToText(t *testing.T) {
	aiCtx := &Context{
		Messages: []Message{
			{Role: "assistant", Content: []ContentBlock{
				{Type: ContentBlockThinking, Thinking: "I should think about this"},
				{Type: ContentBlockText, Text: "Here is my answer"},
			}},
		},
	}
	compat := &OpenAICompletionsCompat{RequiresThinkingAsText: true}

	messages := convertMessagesForOpenAICompat(aiCtx, &Model{ID: "test"}, compat)

	var assistantMsg map[string]any
	for _, m := range messages {
		if m["role"] == "assistant" {
			assistantMsg = m
			break
		}
	}
	if assistantMsg == nil {
		t.Fatal("no assistant message found")
	}
	content, ok := assistantMsg["content"].(string)
	if !ok {
		t.Fatalf("content type = %T", assistantMsg["content"])
	}
	if !strings.Contains(content, "I should think about this") {
		t.Fatalf("content = %q, want to contain thinking text", content)
	}
	if !strings.Contains(content, "Here is my answer") {
		t.Fatalf("content = %q, want to contain answer text", content)
	}
}

// --- toolChoice forwarding ---

func TestBuildOpenAICompatRequestBodyForwardsToolChoice(t *testing.T) {
	model := &Model{ID: "gpt-4o", Provider: ProviderOpenAI}
	body := requireBody(t)(buildOpenAICompatRequestBody(model, &Context{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools: []Tool{{
			Name:        "read",
			Description: "Read",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
	}, &options{toolChoice: "auto"}))

	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto", body["tool_choice"])
	}
}

// --- hasToolHistory ---

func TestHasToolHistory(t *testing.T) {
	tests := []struct {
		name     string
		messages []Message
		want     bool
	}{
		{
			name:     "empty",
			messages: nil,
			want:     false,
		},
		{
			name:     "no tools",
			messages: []Message{{Role: "user", Content: "hi"}},
			want:     false,
		},
		{
			name: "has tool result",
			messages: []Message{
				{Role: "toolResult", ToolCallID: "call_1", Content: "result"},
			},
			want: true,
		},
		{
			name: "has tool call in assistant",
			messages: []Message{
				{Role: "assistant", Content: []ContentBlock{
					{Type: ContentBlockToolCall, ID: "call_1", Name: "read", Arguments: json.RawMessage(`{}`)},
				}},
			},
			want: true,
		},
		{
			name: "assistant without tool calls",
			messages: []Message{
				{Role: "assistant", Content: []ContentBlock{
					{Type: ContentBlockText, Text: "hello"},
				}},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasToolHistory(tt.messages)
			if got != tt.want {
				t.Errorf("hasToolHistory() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildOpenAICompatRequestBodySendsEmptyToolsForToolHistory(t *testing.T) {
	model := &Model{ID: "gpt-4o", Provider: ProviderOpenAI}
	body := requireBody(t)(buildOpenAICompatRequestBody(model, &Context{
		Messages: []Message{
			{Role: "assistant", Content: []ContentBlock{
				{Type: ContentBlockToolCall, ID: "call_1", Name: "read", Arguments: json.RawMessage(`{}`)},
			}},
			{Role: "toolResult", ToolCallID: "call_1", Content: "result"},
			{Role: "user", Content: "follow up"},
		},
	}, &options{}))

	tools, ok := body["tools"]
	if !ok {
		t.Fatal("tools field not set for tool history context")
	}
	toolsSlice, ok := tools.([]any)
	if !ok {
		t.Fatalf("tools type = %T", tools)
	}
	if len(toolsSlice) != 0 {
		t.Fatalf("tools length = %d, want 0 (empty array)", len(toolsSlice))
	}
}

// --- P7-OC04: detailed usage parsing ---

func TestParseOpenAICompatUsageWithDetailedTokens(t *testing.T) {
	raw := &openAICompatUsage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		PromptTokensDetails: &openAICompatPromptTokensDetails{
			CachedTokens:     30,
			CacheWriteTokens: 10,
		},
		CompletionTokensDetails: &openAICompatCompletionTokensDetails{
			ReasoningTokens: 20,
		},
	}

	usage := parseOpenAICompatUsage(raw, &Model{Cost: CostProfile{Input: 1, Output: 2}})

	// cacheRead = cached_tokens - cacheWrite = 30 - 10 = 20
	if usage.CacheRead != 20 {
		t.Fatalf("cacheRead = %d, want 20", usage.CacheRead)
	}
	if usage.CacheWrite != 10 {
		t.Fatalf("cacheWrite = %d, want 10", usage.CacheWrite)
	}
	// input = prompt_tokens - cacheRead - cacheWrite = 100 - 20 - 10 = 70
	if usage.Input != 70 {
		t.Fatalf("input = %d, want 70", usage.Input)
	}
	// output = completion_tokens + reasoning_tokens = 50 + 20 = 70
	if usage.Output != 70 {
		t.Fatalf("output = %d, want 70", usage.Output)
	}
}

// --- P7-OC02: alternate reasoning field names ---

func TestStreamOpenAICompatHandlesAlternateReasoningFields(t *testing.T) {
	// Test that reasoning_content field is handled as thinking
	chunks := []string{
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"reasoning_content":"let me think"}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"answer"}]}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	stream := strings.NewReader(strings.Join(chunks, "\n"))

	var thinkingDeltas []string
	var doneEvents []Done
	err := streamOpenAICompatResponse(context.Background(), &Model{ID: "deepseek-r1", Provider: ProviderOpenRouter}, stream, func(_ context.Context, event StreamEvent) {
		switch e := event.(type) {
		case ThinkingDelta:
			thinkingDeltas = append(thinkingDeltas, e.Delta)
		case Done:
			doneEvents = append(doneEvents, e)
		}
	}, ProviderOpenRouter)
	if err != nil {
		t.Fatalf("streamOpenAICompatResponse: %v", err)
	}

	if len(thinkingDeltas) == 0 || thinkingDeltas[0] != "let me think" {
		t.Fatalf("thinkingDeltas = %v, want [let me think]", thinkingDeltas)
	}
	if len(doneEvents) == 0 {
		t.Fatal("no Done event")
	}

	// Verify thinking block is in the output content
	msg := doneEvents[0].Message
	var hasThinking, hasText bool
	for _, b := range msg.Content {
		if b.Type == ContentBlockThinking {
			hasThinking = true
		}
		if b.Type == ContentBlockText {
			hasText = true
		}
	}
	if !hasThinking {
		t.Fatal("no thinking block in output")
	}
	if !hasText {
		t.Fatal("no text block in output")
	}
}

// --- P7-OC14: OpenRouter provider routing ---

func TestBuildOpenAICompatRequestBodyOpenRouterProviderRouting(t *testing.T) {
	allowFallbacks := false
	model := &Model{
		ID:       "anthropic/claude-sonnet-4-20250514",
		Provider: ProviderOpenRouter,
		BaseURL:  "https://openrouter.ai/api/v1",
		Compat: &CompatibilitySettings{
			OpenRouterRouting: &OpenRouterRouting{
				Only:           []string{"anthropic"},
				AllowFallbacks: &allowFallbacks,
			},
		},
	}
	body := requireBody(t)(buildOpenAICompatRequestBody(model, &Context{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, &options{}))

	provider, ok := body["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider type = %T", body["provider"])
	}
	if only, _ := provider["only"].([]any); len(only) != 1 || only[0] != "anthropic" {
		t.Fatalf("provider.only = %v, want [anthropic]", provider["only"])
	}
	if provider["allow_fallbacks"] != false {
		t.Fatalf("provider = %v, want allow_fallbacks false", provider)
	}
}

// --- base openai.go client: stream_options and toolChoice ---

func TestOpenAIBuildRequestIncludesStreamOptions(t *testing.T) {
	client := &openAIClient{model: &Model{ID: "gpt-4o"}}
	body, err := client.buildRequest(&Model{ID: "gpt-4o"}, &Context{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}, &options{})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req openAIRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Fatal("stream_options.include_usage not set")
	}
}

func TestOpenAIBuildRequestForwardsToolChoice(t *testing.T) {
	client := &openAIClient{model: &Model{ID: "gpt-4o"}}
	body, err := client.buildRequest(&Model{ID: "gpt-4o"}, &Context{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Tools: []Tool{{
			Name:        "read",
			Description: "Read",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
	}, &options{toolChoice: "auto"})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req openAIRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.ToolChoice != "auto" {
		t.Fatalf("tool_choice = %v, want auto", req.ToolChoice)
	}
}

// TestResolveOpenAIBaseURLPrefersModel verifies an explicit Model.BaseURL wins
// over the provider default and that trailing slashes are trimmed so callers
// can concatenate a path unconditionally.
func TestResolveOpenAIBaseURLPrefersModel(t *testing.T) {
	tests := []struct {
		name  string
		model *Model
		want  string
	}{
		{
			name:  "explicit base URL wins",
			model: &Model{Provider: ProviderGroq, BaseURL: "https://proxy.internal/v1"},
			want:  "https://proxy.internal/v1",
		},
		{
			name:  "trailing slash trimmed",
			model: &Model{Provider: ProviderGroq, BaseURL: "https://proxy.internal/v1/"},
			want:  "https://proxy.internal/v1",
		},
		{
			name:  "falls back to provider default",
			model: &Model{Provider: ProviderGroq},
			want:  "https://api.groq.com/openai/v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveOpenAIBaseURL(tt.model)
			if err != nil {
				t.Fatalf("resolveOpenAIBaseURL: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveOpenAIBaseURL = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveOpenAIBaseURLUnknownProvider verifies an unresolvable provider
// reports an error naming the provider rather than returning an empty URL that
// would produce a request to a bare path.
func TestResolveOpenAIBaseURLUnknownProvider(t *testing.T) {
	_, err := resolveOpenAIBaseURL(&Model{Provider: "definitely-not-a-provider"})
	if err == nil {
		t.Fatal("resolveOpenAIBaseURL(unknown) expected error, got nil")
	}
}

// TestCompatProvidersResolveBaseURL guards the regression that motivated
// unifying the two base-URL tables: the openai-compatible client used to carry
// its own six-entry table, so every other compat provider failed with
// "missing base URL" even though DefaultBaseURLs knew the answer.
func TestCompatProvidersResolveBaseURL(t *testing.T) {
	for _, p := range OpenAICompatibleProviders() {
		t.Run(p, func(t *testing.T) {
			got, err := resolveOpenAIBaseURL(&Model{Provider: p})
			if err != nil {
				t.Fatalf("resolveOpenAIBaseURL(%s): %v", p, err)
			}
			if got == "" {
				t.Errorf("resolveOpenAIBaseURL(%s) = empty", p)
			}
		})
	}
}

// TestOpenAIRequestHeadersSessionAffinityGating verifies session-affinity
// headers are only forwarded when the model's compat opts in. Sending
// session_id to opencode.ai/zen/go returns HTTP 500 on some models, so the
// gate is load-bearing rather than cosmetic.
func TestOpenAIRequestHeadersSessionAffinityGating(t *testing.T) {
	var o options
	o.apply([]Option{WithSessionID("session-abc")})

	optedOut := &Model{
		Provider: ProviderGroq,
		Compat:   &CompatibilitySettings{SendSessionAffinityHeaders: false},
	}
	for k := range sessionHeaders(optedOut, &o) {
		if _, ok := openAIRequestHeaders(optedOut, &o)[k]; ok {
			t.Errorf("header %q forwarded despite SendSessionAffinityHeaders=false", k)
		}
	}

	optedIn := &Model{
		Provider: ProviderGroq,
		Compat:   &CompatibilitySettings{SendSessionAffinityHeaders: true},
	}
	got := openAIRequestHeaders(optedIn, &o)
	want := sessionHeaders(optedIn, &o)
	if len(want) == 0 {
		t.Skip("no session headers configured for this option set")
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("header %q = %q, want %q", k, got[k], v)
		}
	}
}

// TestOpenAIRequestHeadersOpenRouterRanking verifies OpenRouter's attribution
// headers reach both clients. They previously existed only on the
// openai-compatible path, so the openai-completions client silently omitted
// them.
func TestOpenAIRequestHeadersOpenRouterRanking(t *testing.T) {
	var o options

	got := openAIRequestHeaders(&Model{Provider: ProviderOpenRouter}, &o)
	if got["X-Title"] == "" {
		t.Error("X-Title header missing for OpenRouter")
	}
	if got["Http-Referer"] == "" {
		t.Error("Http-Referer header missing for OpenRouter")
	}

	other := openAIRequestHeaders(&Model{Provider: ProviderGroq}, &o)
	if _, ok := other["X-Title"]; ok {
		t.Error("X-Title header leaked to a non-OpenRouter provider")
	}
}

// TestOpenAIRequestHeadersCallerWins verifies caller-supplied headers override
// both provider and session-affinity headers.
func TestOpenAIRequestHeadersCallerWins(t *testing.T) {
	var o options
	o.apply([]Option{WithHeaders(map[string]string{"X-Title": "custom"})})

	got := openAIRequestHeaders(&Model{Provider: ProviderOpenRouter}, &o)
	if got["X-Title"] != "custom" {
		t.Errorf("X-Title = %q, want %q", got["X-Title"], "custom")
	}
}

// This file tests the ai package from within the same package,
// so types are used directly without the ai. prefix.

func TestOpenAIResponsesProviderIsNotAlias(t *testing.T) {
	// Verify that openai-responses is no longer an alias
	provider, ok := Get(APIOpenAIResponses)
	if !ok {
		t.Fatal("openai-responses provider not registered")
	}

	if provider.IsAlias() {
		t.Error("openai-responses provider should not be an alias")
	}
}

func TestOpenAIResponsesProviderNewClient(t *testing.T) {
	provider := OpenAIResponsesProvider{}

	if provider.API() != APIOpenAIResponses {
		t.Errorf("expected API %q, got %q", APIOpenAIResponses, provider.API())
	}

	if provider.IsAlias() {
		t.Error("OpenAIResponsesProvider.IsAlias() should return false")
	}

	// Create a mock model for testing
	model := Model{
		ID:       "gpt-4o",
		Provider: ProviderOpenAI,
		API:      APIOpenAIResponses,
		BaseURL:  "https://api.openai.com/v1",
	}

	client, err := provider.NewClient(&model)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	if client == nil {
		t.Fatal("client should not be nil")
	}
}

func TestBuildOpenAIResponsesBody(t *testing.T) {
	model := &Model{
		ID:       "gpt-4o",
		Provider: ProviderOpenAI,
		API:      APIOpenAIResponses,
		BaseURL:  "https://api.openai.com/v1",
	}

	prompt := &Context{
		SystemPrompt: "You are a helpful assistant.",
		Messages: []Message{
			{
				Role:    "user",
				Content: "Hello, how are you?",
			},
		},
	}

	// Create a mock options struct
	o := &options{}

	body, err := buildOpenAIResponsesBody(model, prompt, o)
	if err != nil {
		t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
	}

	// Verify basic body structure
	if body["model"] != "gpt-4o" {
		t.Errorf("expected model %q, got %v", "gpt-4o", body["model"])
	}

	if body["stream"] != true {
		t.Error("stream should be true")
	}

	if body["store"] != false {
		t.Error("store should be false")
	}

	// Verify input/messages were converted
	input, ok := body["input"].([]map[string]any)
	if !ok {
		t.Fatal("input should be a slice of maps")
	}

	if len(input) < 2 {
		t.Fatalf("expected at least 2 messages (system + user), got %d", len(input))
	}

	// Check system prompt
	systemMsg, ok := input[0]["content"].(string)
	if !ok {
		t.Fatalf("system message should have string content, got %T", input[0]["content"])
	}
	if systemMsg != "You are a helpful assistant." {
		t.Errorf("system prompt mismatch: got %q", systemMsg)
	}

	// Check user message
	userMsg := input[1]
	if userMsg["role"] != "user" {
		t.Errorf("expected user role, got %v", userMsg["role"])
	}
}

func TestBuildOpenAIResponsesBodyWithTools(t *testing.T) {
	model := &Model{
		ID:       "gpt-4o",
		Provider: ProviderOpenAI,
		API:      APIOpenAIResponses,
	}

	prompt := &Context{
		Messages: []Message{
			{Role: "user", Content: "What's the weather?"},
		},
		Tools: []Tool{
			{
				Name:        "get_weather",
				Description: "Get the current weather",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}}}`),
			},
		},
	}

	o := &options{}
	body, err := buildOpenAIResponsesBody(model, prompt, o)
	if err != nil {
		t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
	}

	tools, ok := body["tools"].([]map[string]any)
	if !ok {
		t.Fatal("tools should be present in body")
	}

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	if tools[0]["name"] != "get_weather" {
		t.Errorf("expected tool name 'get_weather', got %v", tools[0]["name"])
	}
}

func TestBuildOpenAIResponsesBodyWithServiceTier(t *testing.T) {
	model := &Model{
		ID:       "gpt-4o",
		Provider: ProviderOpenAI,
		API:      APIOpenAIResponses,
	}

	prompt := &Context{
		Messages: []Message{
			{Role: "user", Content: "Hello"},
		},
	}

	o := &options{}
	o.serviceTier = "flex"

	body, err := buildOpenAIResponsesBody(model, prompt, o)
	if err != nil {
		t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
	}

	if body["service_tier"] != "flex" {
		t.Errorf("expected service_tier 'flex', got %v", body["service_tier"])
	}
}

func TestBuildOpenAIResponsesBodyWithMaxTokens(t *testing.T) {
	model := &Model{
		ID:       "gpt-4o",
		Provider: ProviderOpenAI,
		API:      APIOpenAIResponses,
	}

	prompt := &Context{
		Messages: []Message{
			{Role: "user", Content: "Count to 100"},
		},
	}

	o := &options{}
	o.maxTokens = 1000

	body, err := buildOpenAIResponsesBody(model, prompt, o)
	if err != nil {
		t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
	}

	if body["max_output_tokens"] != 1000 {
		t.Errorf("expected max_output_tokens 1000, got %v", body["max_output_tokens"])
	}
}

func TestBuildOpenAIResponsesBodyWithReasoningEffortAndSummary(t *testing.T) {
	model := &Model{
		ID:        "gpt-4o",
		Provider:  ProviderOpenAI,
		API:       APIOpenAIResponses,
		Reasoning: true,
	}

	prompt := &Context{
		Messages: []Message{
			{Role: "user", Content: "Solve this problem"},
		},
	}

	o := &options{}
	o.reasoningEffort = "high"
	o.reasoningSummary = "detailed"

	body, err := buildOpenAIResponsesBody(model, prompt, o)
	if err != nil {
		t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
	}

	// Verify reasoning config
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatal("reasoning should be present in body")
	}

	if reasoning["effort"] != "high" {
		t.Errorf("expected reasoning effort 'high', got %v", reasoning["effort"])
	}

	if reasoning["summary"] != "detailed" {
		t.Errorf("expected reasoning summary 'detailed', got %v", reasoning["summary"])
	}

	// Verify include for encrypted content
	include, ok := body["include"].([]string)
	if !ok {
		t.Fatal("include should be present")
	}

	found := false
	for _, v := range include {
		if v == "reasoning.encrypted_content" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'reasoning.encrypted_content' in include")
	}
}

func TestBuildOpenAIResponsesBodyDefaultsMissingReasoningFields(t *testing.T) {
	model := &Model{
		ID:        "gpt-4o",
		Provider:  ProviderOpenAI,
		API:       APIOpenAIResponses,
		Reasoning: true,
	}

	prompt := &Context{
		Messages: []Message{{Role: "user", Content: "Explain this"}},
	}

	t.Run("summary only defaults effort to medium", func(t *testing.T) {
		o := &options{}
		o.reasoningSummary = "concise"

		body, err := buildOpenAIResponsesBody(model, prompt, o)
		if err != nil {
			t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
		}

		reasoning := body["reasoning"].(map[string]any)
		if reasoning["effort"] != "medium" {
			t.Fatalf("expected default effort 'medium', got %v", reasoning["effort"])
		}
		if reasoning["summary"] != "concise" {
			t.Fatalf("expected summary 'concise', got %v", reasoning["summary"])
		}
	})

	t.Run("effort only defaults summary to auto", func(t *testing.T) {
		o := &options{}
		o.reasoningEffort = "low"

		body, err := buildOpenAIResponsesBody(model, prompt, o)
		if err != nil {
			t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
		}

		reasoning := body["reasoning"].(map[string]any)
		if reasoning["effort"] != "low" {
			t.Fatalf("expected effort 'low', got %v", reasoning["effort"])
		}
		if reasoning["summary"] != "auto" {
			t.Fatalf("expected default summary 'auto', got %v", reasoning["summary"])
		}
	})
}

func TestBuildOpenAIResponsesBodyUsesResolvedCacheRetentionForDirectOpenAI(t *testing.T) {
	t.Setenv("PI_CACHE_RETENTION", "long")

	model := &Model{
		ID:        "gpt-4o",
		Provider:  ProviderOpenAI,
		API:       APIOpenAIResponses,
		Reasoning: true,
	}

	prompt := &Context{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	}

	o := &options{}
	o.sessionID = "session-123"

	body, err := buildOpenAIResponsesBody(model, prompt, o)
	if err != nil {
		t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
	}

	if got := body["prompt_cache_key"]; got != "session-123" {
		t.Fatalf("expected prompt_cache_key to be session-123, got %v", got)
	}
	if got := body["prompt_cache_retention"]; got != "24h" {
		t.Fatalf("expected prompt_cache_retention to be 24h, got %v", got)
	}
}

func TestConvertResponsesTools(t *testing.T) {
	tools := []Tool{
		{
			Name:        "tool1",
			Description: "First tool",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		},
		{
			Name:        "tool2",
			Description: "Second tool",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		},
	}

	result := ConvertResponsesTools(tools)

	if len(result) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(result))
	}

	for i, tool := range result {
		if tool["type"] != "function" {
			t.Errorf("tool %d: expected type 'function', got %v", i, tool["type"])
		}

		expectedName := tools[i].Name
		if tool["name"] != expectedName {
			t.Errorf("tool %d: expected name %q, got %v", i, expectedName, tool["name"])
		}
		if strict, ok := tool["strict"].(bool); !ok || strict {
			t.Errorf("tool %d: expected strict=false, got %v", i, tool["strict"])
		}
	}
}

func TestConvertUserMessageForResponses(t *testing.T) {
	model := &Model{
		ID:       "gpt-4o",
		Provider: ProviderOpenAI,
		API:      APIOpenAIResponses,
		Input:    []InputType{InputText, InputImage},
	}

	// Test string content
	msg := Message{
		Role:    "user",
		Content: "Hello, world!",
	}

	result := convertUserMessageForResponses(model, msg)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}

	if result[0]["role"] != "user" {
		t.Errorf("expected role 'user', got %v", result[0]["role"])
	}

	content := result[0]["content"].([]map[string]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}

	if content[0]["type"] != "input_text" {
		t.Errorf("expected type 'input_text', got %v", content[0]["type"])
	}

	if content[0]["text"] != "Hello, world!" {
		t.Errorf("expected text 'Hello, world!', got %v", content[0]["text"])
	}
}

func TestConvertAssistantMessageForResponses(t *testing.T) {
	model := &Model{
		ID:       "gpt-4o",
		Provider: ProviderOpenAI,
		API:      APIOpenAIResponses,
	}

	// Test text block
	msg := Message{
		Role: "assistant",
		Content: []ContentBlock{
			{
				Type: ContentBlockText,
				Text: "Hello!",
			},
		},
	}

	result := requireItems(t)(convertAssistantMessageForResponses(model, msg, 0, nil))
	if len(result) != 1 {
		t.Fatalf("expected 1 message item, got %d", len(result))
	}

	if result[0]["type"] != "message" {
		t.Errorf("expected type 'message', got %v", result[0]["type"])
	}

	if result[0]["role"] != "assistant" {
		t.Errorf("expected role 'assistant', got %v", result[0]["role"])
	}
}

func TestConvertAssistantMessageForResponsesPreservesTextSignaturePhaseAndLegacyIDs(t *testing.T) {
	model := &Model{
		ID:       "gpt-4o",
		Provider: ProviderOpenAI,
		API:      APIOpenAIResponses,
	}

	t.Run("json signature preserves id and phase", func(t *testing.T) {
		msg := Message{
			Role: "assistant",
			Content: []ContentBlock{{
				Type:          ContentBlockText,
				Text:          "Final answer",
				TextSignature: `{"v":1,"id":"msg_abc123","phase":"final_answer"}`,
			}},
		}

		result := requireItems(t)(convertAssistantMessageForResponses(model, msg, 2, nil))
		if got := result[0]["id"]; got != "msg_abc123" {
			t.Fatalf("expected id msg_abc123, got %v", got)
		}
		if got := result[0]["phase"]; got != "final_answer" {
			t.Fatalf("expected phase final_answer, got %v", got)
		}
	})

	t.Run("legacy plain-string signature is still treated as message id", func(t *testing.T) {
		msg := Message{
			Role: "assistant",
			Content: []ContentBlock{{
				Type:          ContentBlockText,
				Text:          "Legacy answer",
				TextSignature: "legacy-msg-id",
			}},
		}

		result := requireItems(t)(convertAssistantMessageForResponses(model, msg, 3, nil))
		if got := result[0]["id"]; got != "legacy-msg-id" {
			t.Fatalf("expected legacy id to round-trip, got %v", got)
		}
	})

	t.Run("cross-model replay omits responses item id for tool calls", func(t *testing.T) {
		msg := Message{
			Role:     "assistant",
			Provider: ProviderOpenAI,
			API:      APIOpenAIResponses,
			Model:    "gpt-4.1",
			Content: []ContentBlock{{
				Type:      ContentBlockToolCall,
				ID:        "call_123|fc_original",
				Name:      "lookup",
				Arguments: json.RawMessage(`{"city":"Boston"}`),
			}},
		}

		result := requireItems(t)(convertAssistantMessageForResponses(model, msg, 4, nil))
		if len(result) != 1 {
			t.Fatalf("expected 1 message item, got %d", len(result))
		}
		if got := result[0]["call_id"]; got != "call_123" {
			t.Fatalf("expected call_id call_123, got %v", got)
		}
		if _, ok := result[0]["id"]; ok {
			t.Fatalf("expected cross-model replay to omit item id, got %v", result[0]["id"])
		}
	})
}

func TestConvertAssistantMessageForResponsesReplayFixturePreservesAndStripsIDs(t *testing.T) {
	model := &Model{ID: "gpt-4o", Provider: ProviderOpenAI, API: APIOpenAIResponses}

	sameModel := Message{
		Role:     "assistant",
		Provider: ProviderOpenAI,
		API:      APIOpenAIResponses,
		Model:    "gpt-4o",
		Content: []ContentBlock{
			{Type: ContentBlockThinking, ThoughtSignature: `{"type":"reasoning.encrypted","id":"rs_1","data":"enc"}`},
			{Type: ContentBlockText, Text: "Answer", TextSignature: `{"v":1,"id":"msg_same","phase":"final_answer"}`},
			{Type: ContentBlockToolCall, ID: "call_same|fc_same", Name: "lookup", Arguments: json.RawMessage(`{"q":"one"}`)},
		},
	}
	items := requireItems(t)(convertAssistantMessageForResponses(model, sameModel, 7, nil))
	requireResponsesItem(t, items, "reasoning.encrypted", "rs_1")
	message := requireResponsesItem(t, items, "message", "msg_same")
	if got := message["phase"]; got != "final_answer" {
		t.Fatalf("phase = %v, want final_answer", got)
	}
	tool := requireResponsesItem(t, items, "function_call", "fc_same")
	if got := tool["call_id"]; got != "call_same" {
		t.Fatalf("call_id = %v, want call_same", got)
	}

	differentModel := sameModel
	differentModel.Model = "gpt-4.1"
	items = requireItems(t)(convertAssistantMessageForResponses(model, differentModel, 8, nil))
	tool = requireResponsesItem(t, items, "function_call", "")
	if _, ok := tool["id"]; ok {
		t.Fatalf("different-model replay should strip function_call item id, got %v", tool["id"])
	}
	if got := tool["call_id"]; got != "call_same" {
		t.Fatalf("call_id = %v, want call_same", got)
	}
}

func requireResponsesItem(t *testing.T, items []map[string]any, typ, id string) map[string]any {
	t.Helper()
	for _, item := range items {
		if item["type"] != typ {
			continue
		}
		if id != "" && item["id"] != id {
			continue
		}
		return item
	}
	t.Fatalf("missing Responses item type=%q id=%q in %#v", typ, id, items)
	return nil
}

func TestConvertToolResultMessageForResponses(t *testing.T) {
	model := &Model{
		ID:       "gpt-4o",
		Provider: ProviderOpenAI,
		API:      APIOpenAIResponses,
	}

	// Test tool result message
	msg := Message{
		Role:       "toolResult",
		ToolCallID: "call_123",
		Content: []ContentBlock{
			{
				Type: ContentBlockText,
				Text: "The weather is sunny.",
			},
		},
	}

	result := convertToolResultMessageForResponses(model, msg)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}

	if result[0]["type"] != "function_call_output" {
		t.Errorf("expected type 'function_call_output', got %v", result[0]["type"])
	}

	if result[0]["call_id"] != "call_123" {
		t.Errorf("expected call_id 'call_123', got %v", result[0]["call_id"])
	}

	t.Run("joins multiple text blocks with newlines", func(t *testing.T) {
		msg := Message{
			Role:       "toolResult",
			ToolCallID: "call_456",
			Content: []ContentBlock{
				{Type: ContentBlockText, Text: "first line"},
				{Type: ContentBlockText, Text: "second line"},
			},
		}

		result := convertToolResultMessageForResponses(model, msg)
		if got := result[0]["output"]; got != "first line\nsecond line" {
			t.Fatalf("expected newline-joined output, got %v", got)
		}
	})

	t.Run("composite tool call id emits bare call id", func(t *testing.T) {
		msg := Message{
			Role:       "toolResult",
			ToolCallID: "call_789|fc_123",
			Content: []ContentBlock{
				{Type: ContentBlockText, Text: "result"},
			},
		}

		result := convertToolResultMessageForResponses(model, msg)
		if got := result[0]["call_id"]; got != "call_789" {
			t.Fatalf("expected bare call_id, got %v", got)
		}
	})

	t.Run("empty non-image output falls back to placeholder text", func(t *testing.T) {
		msg := Message{
			Role:       "toolResult",
			ToolCallID: "call_987",
			Content:    []ContentBlock{},
		}

		result := convertToolResultMessageForResponses(model, msg)
		if got := result[0]["output"]; got != "(see attached image)" {
			t.Fatalf("expected placeholder output, got %v", got)
		}
	})

	t.Run("missing tool call id is omitted", func(t *testing.T) {
		msg := Message{
			Role:    "toolResult",
			Content: []ContentBlock{{Type: ContentBlockText, Text: "legacy restored result"}},
		}

		result := convertToolResultMessageForResponses(model, msg)
		if len(result) != 0 {
			t.Fatalf("expected malformed tool result to be omitted, got %#v", result)
		}
	})
}

func TestBuildCodexRequestBodyOmitsRestoredToolResultWithEmptyCallID(t *testing.T) {
	model := &Model{ID: "gpt-5.3-codex", Provider: ProviderOpenAICodex, API: APIOpenAICodexResponses}
	prompt := &Context{
		Messages: []Message{
			{Role: "user", Content: "Use the tool"},
			{Role: "assistant", Provider: ProviderOpenAICodex, API: APIOpenAICodexResponses, Model: "gpt-5.3-codex", Content: []ContentBlock{{Type: ContentBlockToolCall, ID: "call_123", Name: "read", Arguments: json.RawMessage(`{"path":"a.txt"}`)}}},
			{Role: "toolResult", Content: []ContentBlock{{Type: ContentBlockText, Text: "legacy restored result without metadata"}}},
			{Role: "user", Content: "continue"},
		},
	}

	body := requireBody(t)(buildCodexRequestBody(model, prompt, &options{}))
	input := body["input"].([]map[string]any)
	for i, item := range input {
		if item["type"] != "function_call_output" {
			continue
		}
		if got, _ := item["call_id"].(string); got == "" {
			t.Fatalf("input[%d].call_id is empty in %#v", i, input)
		}
	}
}

func TestParseTextSignatureV1(t *testing.T) {
	// Valid signature
	sig := `{"v":1,"id":"msg_abc123"}`
	parsed := parseTextSignatureV1(sig)
	if parsed == nil {
		t.Fatal("expected valid signature to parse")
	}
	if parsed.ID != "msg_abc123" {
		t.Errorf("expected id 'msg_abc123', got %q", parsed.ID)
	}

	// Valid signature with phase
	sig = `{"v":1,"id":"msg_abc123","phase":"final_answer"}`
	parsed = parseTextSignatureV1(sig)
	if parsed == nil {
		t.Fatal("expected valid signature to parse")
	}
	if parsed.Phase != "final_answer" {
		t.Errorf("expected phase 'final_answer', got %q", parsed.Phase)
	}

	// Legacy plain-string signature should still parse as an ID.
	parsed = parseTextSignatureV1("legacy-msg-id")
	if parsed == nil || parsed.ID != "legacy-msg-id" {
		t.Fatalf("expected legacy plain-string signature to parse, got %+v", parsed)
	}

	// Invalid signatures
	invalid := []string{
		`{"v":0,"id":"test"}`,
		`{"v":1}`,
		`{"id":"test"}`,
		`{"v":1,"id":123}`,
	}

	for _, sig := range invalid {
		parsed := parseTextSignatureV1(sig)
		if parsed != nil {
			t.Errorf("expected nil for invalid signature %q, got %+v", sig, parsed)
		}
	}
}

func TestConvertResponsesMessagesForOpenAI(t *testing.T) {
	model := &Model{
		ID:        "gpt-4o",
		Provider:  ProviderOpenAI,
		API:       APIOpenAIResponses,
		Reasoning: true,
	}

	prompt := &Context{
		SystemPrompt: "System prompt here",
		Messages: []Message{
			{
				Role:    "user",
				Content: "Hello",
			},
			{
				Role: "assistant",
				Content: []ContentBlock{
					{Type: ContentBlockText, Text: "Hi there!"},
				},
			},
		},
	}

	messages, err := ConvertResponsesMessagesForOpenAI(model, prompt)
	if err != nil {
		t.Fatalf("ConvertResponsesMessagesForOpenAI failed: %v", err)
	}

	if len(messages) < 3 {
		t.Fatalf("expected at least 3 messages (system + user + assistant), got %d", len(messages))
	}

	// Check system prompt becomes developer for reasoning models
	if messages[0]["role"] != "developer" {
		t.Errorf("expected system message to become 'developer', got %v", messages[0]["role"])
	}

	// Check user message
	if messages[1]["role"] != "user" {
		t.Errorf("expected user role, got %v", messages[1]["role"])
	}

	// Check assistant message
	if messages[2]["role"] != "assistant" {
		t.Errorf("expected assistant role, got %v", messages[2]["role"])
	}
}

// TestGitHubCopilotOpenAIResponsesModels verifies Copilot OpenAI Responses models.
func TestGitHubCopilotOpenAIResponsesModels(t *testing.T) {
	// Verify gpt-5.4-nano is a Copilot model using OpenAI Responses API
	model, err := GetAvailableModel("github-copilot", "gpt-5.4-nano")
	if err != nil {
		t.Fatalf("GetAvailableModel(github-copilot, gpt-5.4-nano): %v", err)
	}

	if model.Provider != ProviderGitHubCopilot {
		t.Errorf("Provider = %q, want %q", model.Provider, ProviderGitHubCopilot)
	}

	if model.API != APIOpenAIResponses {
		t.Errorf("API = %q, want %q", model.API, APIOpenAIResponses)
	}
}

// TestGitHubCopilotOpenAIResponsesOmitsReasoningNone verifies that Copilot
// OpenAI Responses models don't send "reasoning: {effort: none}" when no
// reasoning is requested.
func TestGitHubCopilotOpenAIResponsesOmitsReasoningNone(t *testing.T) {
	model := &Model{
		ID:        "gpt-5.4-nano",
		Provider:  ProviderGitHubCopilot,
		API:       APIOpenAIResponses,
		Reasoning: true,
	}

	prompt := &Context{
		Messages: []Message{
			{Role: "user", Content: "Hello"},
		},
	}

	// No reasoning options set
	o := &options{}

	body, err := buildOpenAIResponsesBody(model, prompt, o)
	if err != nil {
		t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
	}

	// Copilot should NOT have reasoning: {effort: none} because we skip
	// the else branch when model.Provider == ProviderGitHubCopilot
	if _, hasReasoning := body["reasoning"]; hasReasoning {
		reasoning := body["reasoning"].(map[string]any)
		if effort, ok := reasoning["effort"].(string); ok && effort == "none" {
			t.Error("Copilot should not send reasoning: {effort: none}")
		}
	}
}

// TestGitHubCopilotOpenAIResponsesWithReasoningEffort verifies that Copilot
// OpenAI Responses models work correctly when reasoning effort is specified.
func TestGitHubCopilotOpenAIResponsesWithReasoningEffort(t *testing.T) {
	model := &Model{
		ID:        "gpt-5.4-nano",
		Provider:  ProviderGitHubCopilot,
		API:       APIOpenAIResponses,
		Reasoning: true,
	}

	prompt := &Context{
		Messages: []Message{
			{Role: "user", Content: "Hello"},
		},
	}

	o := &options{}
	o.reasoningEffort = "high"
	o.reasoningSummary = "concise"

	body, err := buildOpenAIResponsesBody(model, prompt, o)
	if err != nil {
		t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
	}

	// When reasoning options are set, Copilot should include them
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatal("reasoning should be present when effort/summary are set")
	}

	if reasoning["effort"] != "high" {
		t.Errorf("effort = %v, want high", reasoning["effort"])
	}

	if reasoning["summary"] != "concise" {
		t.Errorf("summary = %v, want concise", reasoning["summary"])
	}

	// Should include encrypted content
	include, ok := body["include"].([]string)
	if !ok {
		t.Fatal("include should be present")
	}

	found := false
	for _, v := range include {
		if v == "reasoning.encrypted_content" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'reasoning.encrypted_content' in include")
	}
}

// TestNonCopilotOpenAIResponsesAddsReasoningNone verifies that non-Copilot
// OpenAI Responses models DO send "reasoning: {effort: none}" when no
// reasoning is requested (this is the default behavior for official OpenAI).
func TestNonCopilotOpenAIResponsesAddsReasoningNone(t *testing.T) {
	model := &Model{
		ID:        "gpt-5.4",
		Provider:  ProviderOpenAI,
		API:       APIOpenAIResponses,
		Reasoning: true,
	}

	prompt := &Context{
		Messages: []Message{
			{Role: "user", Content: "Hello"},
		},
	}

	// No reasoning options set
	o := &options{}

	body, err := buildOpenAIResponsesBody(model, prompt, o)
	if err != nil {
		t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
	}

	// Non-Copilot models SHOULD have reasoning: {effort: none}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatal("reasoning should be present for non-Copilot models")
	}

	if effort, ok := reasoning["effort"].(string); !ok || effort != "none" {
		t.Errorf("expected reasoning.effort = none, got %v", effort)
	}
}

// TestGitHubCopilotOpenAIResponsesCacheRetention verifies Copilot models
// handle cache retention correctly.
func TestGitHubCopilotOpenAIResponsesCacheRetention(t *testing.T) {
	model := &Model{
		ID:        "gpt-5.4-nano",
		Provider:  ProviderGitHubCopilot,
		API:       APIOpenAIResponses,
		BaseURL:   "https://api.individual.githubcopilot.com",
		Reasoning: true,
	}

	prompt := &Context{
		Messages: []Message{
			{Role: "user", Content: "Hello"},
		},
	}

	o := &options{}
	o.sessionID = "session-123"
	o.cacheRetention = "short"

	body, err := buildOpenAIResponsesBody(model, prompt, o)
	if err != nil {
		t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
	}

	// Should have session-based cache key
	if cacheKey := body["prompt_cache_key"]; cacheKey != "session-123" {
		t.Errorf("prompt_cache_key = %v, want session-123", cacheKey)
	}

	// Copilot endpoint should not have 24h retention (only official OpenAI does)
	if retention := body["prompt_cache_retention"]; retention != nil {
		t.Errorf("prompt_cache_retention = %v, want nil for non-OpenAI endpoint", retention)
	}
}

// TestGitHubCopilotOpenAIResponsesCacheRetentionNone verifies cache is
// disabled when cacheRetention is "none".
func TestGitHubCopilotOpenAIResponsesCacheRetentionNone(t *testing.T) {
	model := &Model{
		ID:       "gpt-5.4-nano",
		Provider: ProviderGitHubCopilot,
		API:      APIOpenAIResponses,
	}

	prompt := &Context{
		Messages: []Message{
			{Role: "user", Content: "Hello"},
		},
	}

	o := &options{}
	o.sessionID = "session-123"
	o.cacheRetention = "none"

	body, err := buildOpenAIResponsesBody(model, prompt, o)
	if err != nil {
		t.Fatalf("buildOpenAIResponsesBody failed: %v", err)
	}

	// Should NOT have cache key when retention is none
	if cacheKey := body["prompt_cache_key"]; cacheKey != nil {
		t.Errorf("prompt_cache_key = %v, want nil for cacheRetention=none", cacheKey)
	}
}

// TestGitHubCopilotOpenAIResponsesProviderSupport verifies the support
// status for Copilot OpenAI Responses models.
func TestGitHubCopilotOpenAIResponsesProviderSupport(t *testing.T) {
	status, ok := ProviderSupport(ProviderGitHubCopilot)
	if !ok {
		t.Fatal("ProviderSupport(github-copilot) returned false")
	}

	// Verify GitHub Copilot is properly supported
	if !status.HasImplementation {
		t.Error("GitHub Copilot should have an implementation")
	}

	// GitHub Copilot should be registered
	if !status.IsRegistered {
		t.Error("GitHub Copilot should be registered")
	}
}

// TestProcessResponsesStreamStartEvent tests that response.created generates a start event.
func TestProcessResponsesStreamStartEvent(t *testing.T) {
	// Simulate SSE data for a simple text response
	sseData := `data: {"type":"response.created","response":{"id":"resp_abc123","status":"in_progress"}}
data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_001"}}
data: {"type":"response.content_part.added","part":{"type":"output_text","text":"Hello"}}
data: {"type":"response.output_text.delta","delta":" world"}
data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_001","content":[{"type":"output_text","text":"Hello world"}],"status":"completed"}}
data: {"type":"response.completed","response":{"id":"resp_abc123","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":0}}}}

`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o", Cost: CostProfile{Input: 1000, Output: 2000}}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var events []StreamEvent
	handler := func(ctx context.Context, event StreamEvent) {
		events = append(events, event)
	}

	err := ProcessResponsesStream(context.Background(), reader, output, handler, model, nil)
	if err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	// Verify events
	if len(events) == 0 {
		t.Fatal("expected at least one event")
	}

	// Check for start event
	startSeen := false
	doneSeen := false
	textDeltaSeen := false

	for _, e := range events {
		switch e.(type) {
		case Start:
			startSeen = true
		case Done:
			doneSeen = true
		case TextDelta:
			textDeltaSeen = true
		}
	}

	if !startSeen {
		t.Error("expected a Start event")
	}
	if !doneSeen {
		t.Error("expected a Done event")
	}
	if !textDeltaSeen {
		t.Error("expected a TextDelta event")
	}
}

// TestProcessResponsesStreamToolCall tests tool call processing.
func TestProcessResponsesStreamToolCall(t *testing.T) {
	sseData := `data: {"type":"response.created","response":{"id":"resp_abc123","status":"in_progress"}}
data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_001","call_id":"call_123","name":"get_weather","arguments":""}}
data: {"type":"response.function_call_arguments.delta","delta":"{\"location\":"}
data: {"type":"response.function_call_arguments.delta","delta":"\"Boston\"}"}
data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_001","call_id":"call_123","name":"get_weather","arguments":"{\"location\":\"Boston\"}"}}
data: {"type":"response.completed","response":{"id":"resp_abc123","status":"completed"}}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o"}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var events []StreamEvent
	handler := func(ctx context.Context, event StreamEvent) {
		events = append(events, event)
	}

	err := ProcessResponsesStream(context.Background(), reader, output, handler, model, nil)
	if err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	// Check for tool call events
	toolCallStartSeen := false
	toolCallEndSeen := false
	toolCallDeltaSeen := false

	for _, e := range events {
		switch e := e.(type) {
		case ToolCallStart:
			toolCallStartSeen = true
			if e.ContentIndex < 0 {
				t.Error("expected non-negative content index for tool call start")
			}
		case ToolCallDelta:
			toolCallDeltaSeen = true
			if e.Delta == "" {
				t.Error("expected non-empty delta")
			}
		case ToolCallEnd:
			toolCallEndSeen = true
			if e.ToolCall.Name != "get_weather" {
				t.Errorf("expected tool call name 'get_weather', got %q", e.ToolCall.Name)
			}
		}
	}

	if !toolCallStartSeen {
		t.Error("expected a ToolCallStart event")
	}
	if !toolCallEndSeen {
		t.Error("expected a ToolCallEnd event")
	}
	if !toolCallDeltaSeen {
		t.Error("expected a ToolCallDelta event")
	}

	assertStreamGolden(t, events, streamGolden{
		EventTypes: []string{"Start", "ToolCallStart", "ToolCallDelta", "ToolCallDelta", "ToolCallEnd", "Done"},
		StopReason: StopReasonToolUse,
	})
}

// TestProcessResponsesStreamThinking tests reasoning/thinking processing.
func TestProcessResponsesStreamThinking(t *testing.T) {
	sseData := `data: {"type":"response.created","response":{"id":"resp_abc123"}}
data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_001"}}
data: {"type":"response.reasoning_summary_part.added","item":{"type":"reasoning_summary","summary":[{"type":"summary","text":"Thinking part 1"}]}}
data: {"type":"response.reasoning_summary_text.delta","delta":"Thinking part 1"}
data: {"type":"response.reasoning_summary_part.done","item":{"type":"reasoning_summary","summary":[{"type":"summary","text":"Thinking part 1"}]}}
data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_001","summary":[{"type":"summary","text":"Thinking part 1"}]}}
data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_001"}}
data: {"type":"response.output_text.delta","delta":"Final answer"}
data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_001","content":[{"type":"output_text","text":"Final answer"}]}}
data: {"type":"response.completed","response":{"id":"resp_abc123","status":"completed"}}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o", Reasoning: true}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var events []StreamEvent
	handler := func(ctx context.Context, event StreamEvent) {
		events = append(events, event)
	}

	err := ProcessResponsesStream(context.Background(), reader, output, handler, model, nil)
	if err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	// Check for thinking events
	thinkingStartSeen := false
	thinkingEndSeen := false

	for _, e := range events {
		switch v := e.(type) {
		case ThinkingStart:
			thinkingStartSeen = true
		case ThinkingEnd:
			thinkingEndSeen = true
			if v.Content == "" {
				t.Error("expected non-empty thinking content")
			}
		}
	}

	if !thinkingStartSeen {
		t.Error("expected a ThinkingStart event")
	}
	if !thinkingEndSeen {
		t.Error("expected a ThinkingEnd event")
	}
}

func TestProcessResponsesStreamThinkingFinalSummarySetsThoughtSignature(t *testing.T) {
	sseData := `data: {"type":"response.created","response":{"id":"resp_abc123"}}

data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_001"}}

data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_001","summary":[{"type":"summary","text":"First thought"},{"type":"summary","text":"Second thought"}]}}

data: {"type":"response.completed","response":{"id":"resp_abc123","status":"completed"}}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o", Reasoning: true}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	err := ProcessResponsesStream(context.Background(), reader, output, func(context.Context, StreamEvent) {}, model, nil)
	if err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	if len(output.Content) != 1 {
		t.Fatalf("len(output.Content) = %d, want 1", len(output.Content))
	}

	block := output.Content[0]
	if block.Type != ContentBlockThinking {
		t.Fatalf("block.Type = %q, want %q", block.Type, ContentBlockThinking)
	}
	if block.Thinking != "First thought\n\nSecond thought" {
		t.Fatalf("block.Thinking = %q, want joined summary text", block.Thinking)
	}
	if block.ThoughtSignature == "" {
		t.Fatal("block.ThoughtSignature = empty, want serialized reasoning item")
	}

	var signature responsesItem
	if err := json.Unmarshal([]byte(block.ThoughtSignature), &signature); err != nil {
		t.Fatalf("unmarshal thought signature: %v", err)
	}
	if signature.ID != "rs_001" {
		t.Fatalf("signature.ID = %q, want rs_001", signature.ID)
	}
	if len(signature.Summary) != 2 || signature.Summary[0].Text != "First thought" || signature.Summary[1].Text != "Second thought" {
		t.Fatalf("signature.Summary = %+v, want final summary texts", signature.Summary)
	}
}

func TestProcessResponsesStreamMessageDoneSetsTextSignatureAndPhase(t *testing.T) {
	sseData := `data: {"type":"response.created","response":{"id":"resp_abc123"}}

data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_001"}}

data: {"type":"response.output_text.delta","delta":"Final answer"}

data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_001","phase":"final_answer","content":[{"type":"output_text","text":"Final answer"}],"status":"completed"}}

data: {"type":"response.completed","response":{"id":"resp_abc123","status":"completed"}}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o"}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	err := ProcessResponsesStream(context.Background(), reader, output, func(context.Context, StreamEvent) {}, model, nil)
	if err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	if len(output.Content) != 1 {
		t.Fatalf("len(output.Content) = %d, want 1", len(output.Content))
	}

	block := output.Content[0]
	if block.Type != ContentBlockText {
		t.Fatalf("block.Type = %q, want %q", block.Type, ContentBlockText)
	}
	if block.Text != "Final answer" {
		t.Fatalf("block.Text = %q, want Final answer", block.Text)
	}
	if block.TextSignature != encodeTextSignatureV1("msg_001", "final_answer") {
		t.Fatalf("block.TextSignature = %q, want encoded id+phase", block.TextSignature)
	}
}

// TestProcessResponsesStreamServiceTierPricing tests service tier pricing application.
func TestProcessResponsesStreamServiceTierPricing(t *testing.T) {
	sseData := `data: {"type":"response.created","response":{"id":"resp_abc123"}}
data: {"type":"response.completed","response":{"id":"resp_abc123","status":"completed","service_tier":"flex","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":0}}}}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o", Cost: CostProfile{Input: 1000, Output: 2000}}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var usageSeen bool
	var finalUsage *Usage

	handler := func(ctx context.Context, event StreamEvent) {
		if u, ok := event.(*Usage); ok {
			usageSeen = true
			finalUsage = u
		}
	}

	streamOpts := &ResponsesStreamOptions{
		ServiceTier:             "flex",
		ApplyServiceTierPricing: ServiceTierPricing,
	}

	err := ProcessResponsesStream(context.Background(), reader, output, handler, model, streamOpts)
	if err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	if !usageSeen {
		t.Error("expected a usage event")
	}

	if finalUsage != nil && finalUsage.Cost.Total == 0 {
		t.Error("expected non-zero cost after service tier pricing")
	}

	// Verify the flex multiplier (0.5x) was applied
	// Base cost: 100 * 0.001 + 50 * 0.002 = 0.1 + 0.1 = 0.2
	// With 0.5x multiplier: 0.1
	if finalUsage != nil && finalUsage.Cost.Total != 0.1 {
		t.Errorf("expected cost 0.1 with flex multiplier, got %f", finalUsage.Cost.Total)
	}
}

// TestProcessResponsesStreamPriorityTier tests priority tier pricing (2x).
func TestProcessResponsesStreamPriorityTier(t *testing.T) {
	sseData := `data: {"type":"response.created","response":{"id":"resp_abc123"}}
data: {"type":"response.completed","response":{"id":"resp_abc123","status":"completed","service_tier":"priority","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":0}}}}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o", Cost: CostProfile{Input: 1000, Output: 2000}}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var finalUsage *Usage

	handler := func(ctx context.Context, event StreamEvent) {
		if u, ok := event.(*Usage); ok {
			finalUsage = u
		}
	}

	streamOpts := &ResponsesStreamOptions{
		ServiceTier:             "priority",
		ApplyServiceTierPricing: ServiceTierPricing,
	}

	err := ProcessResponsesStream(context.Background(), reader, output, handler, model, streamOpts)
	if err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	// Base cost: 100 * 0.001 + 50 * 0.002 = 0.1 + 0.1 = 0.2
	// With 2x multiplier: 0.4
	if finalUsage != nil && finalUsage.Cost.Total != 0.4 {
		t.Errorf("expected cost 0.4 with priority multiplier, got %f", finalUsage.Cost.Total)
	}
}

// TestProcessResponsesStreamToolUseStopReason tests that stopReason becomes toolUse when there are tool calls.
func TestProcessResponsesStreamToolUseStopReason(t *testing.T) {
	sseData := `data: {"type":"response.created","response":{"id":"resp_abc123"}}
data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_001","call_id":"call_123","name":"tool","arguments":"{}"}}
data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_001","call_id":"call_123","name":"tool","arguments":"{}"}}
data: {"type":"response.completed","response":{"id":"resp_abc123","status":"completed"}}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o"}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var doneEvent Done

	handler := func(ctx context.Context, event StreamEvent) {
		if d, ok := event.(Done); ok {
			doneEvent = d
		}
	}

	err := ProcessResponsesStream(context.Background(), reader, output, handler, model, nil)
	if err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	if doneEvent.Reason != StopReasonToolUse {
		t.Errorf("expected stop reason 'toolUse', got %q", doneEvent.Reason)
	}
}

// TestProcessResponsesStreamErrorEvent tests that error events generate StreamError.
func TestProcessResponsesStreamErrorEvent(t *testing.T) {
	sseData := `data: {"type":"error","code":"rate_limit_exceeded","message":"Rate limit exceeded"}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o"}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var streamErrSeen bool

	handler := func(ctx context.Context, event StreamEvent) {
		if _, ok := event.(StreamError); ok {
			streamErrSeen = true
		}
	}

	err := ProcessResponsesStream(context.Background(), reader, output, handler, model, nil)
	_ = err // Note: error return is nil here since we handle it via StreamError

	if !streamErrSeen {
		t.Error("expected a StreamError event for error type")
	}
}

func TestProcessResponsesStreamEmptyErrorEventIncludesRawPayload(t *testing.T) {
	sseData := `data: {"type":"error"}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-5.5"}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var streamErr StreamError
	handler := func(ctx context.Context, event StreamEvent) {
		if e, ok := event.(StreamError); ok {
			streamErr = e
		}
	}

	if err := ProcessResponsesStream(context.Background(), reader, output, handler, model, nil); err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	want := `error event with empty code/message: {"type":"error"}`
	if streamErr.Msg.ErrorMessage != want {
		t.Fatalf("errorMessage = %q, want %q", streamErr.Msg.ErrorMessage, want)
	}
	if !strings.Contains(streamErr.Error(), want) {
		t.Fatalf("stream error = %q, want to contain %q", streamErr.Error(), want)
	}
}

func TestProcessResponsesStreamNestedErrorEventMessage(t *testing.T) {
	sseData := `data: {"type":"error","error":{"code":"server_error","message":"upstream reset"}}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-5.5"}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var streamErr StreamError
	handler := func(ctx context.Context, event StreamEvent) {
		if e, ok := event.(StreamError); ok {
			streamErr = e
		}
	}

	if err := ProcessResponsesStream(context.Background(), reader, output, handler, model, nil); err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	want := "error code server_error: upstream reset"
	if streamErr.Msg.ErrorMessage != want {
		t.Fatalf("errorMessage = %q, want %q", streamErr.Msg.ErrorMessage, want)
	}
}

// TestProcessResponsesStreamFailedEvent tests that response.failed generates StreamError.
func TestProcessResponsesStreamFailedEvent(t *testing.T) {
	sseData := `data: {"type":"response.failed","response":{"id":"resp_abc123","status":"failed","error":{"code":"internal_error","message":"Internal server error"},"incomplete_details":{"reason":"Server overloaded"}}}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o"}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var streamErrSeen bool

	handler := func(ctx context.Context, event StreamEvent) {
		if _, ok := event.(StreamError); ok {
			streamErrSeen = true
		}
	}

	err := ProcessResponsesStream(context.Background(), reader, output, handler, model, nil)
	_ = err // Handled via StreamError

	if !streamErrSeen {
		t.Error("expected a StreamError event for response.failed")
	}
}

// TestProcessResponsesStreamRefusal tests refusal content handling.
func TestProcessResponsesStreamRefusal(t *testing.T) {
	sseData := `data: {"type":"response.created","response":{"id":"resp_abc123"}}
data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_001"}}
data: {"type":"response.content_part.added","part":{"type":"output_text","text":"I can help with"}}
data: {"type":"response.refusal.delta","delta":" that."}
data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_001","content":[{"type":"refusal","refusal":"that."}]}}
data: {"type":"response.completed","response":{"id":"resp_abc123","status":"completed"}}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o"}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var textContent string

	handler := func(ctx context.Context, event StreamEvent) {
		if e, ok := event.(TextEnd); ok {
			textContent = e.Content
		}
	}

	err := ProcessResponsesStream(context.Background(), reader, output, handler, model, nil)
	if err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	// Refusal content should be included in text
	if textContent != "I can help with that." {
		t.Errorf("expected text content 'I can help with that.', got %q", textContent)
	}
}

// TestProcessResponsesStreamIncompleteStatus tests incomplete status mapping.
func TestProcessResponsesStreamIncompleteStatus(t *testing.T) {
	sseData := `data: {"type":"response.created","response":{"id":"resp_abc123"}}
data: {"type":"response.completed","response":{"id":"resp_abc123","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))
	model := &Model{ID: "gpt-4o"}
	output := &AssistantMessage{Role: "assistant", Content: []ContentBlock{}}

	var doneEvent Done

	handler := func(ctx context.Context, event StreamEvent) {
		if d, ok := event.(Done); ok {
			doneEvent = d
		}
	}

	err := ProcessResponsesStream(context.Background(), reader, output, handler, model, nil)
	if err != nil {
		t.Fatalf("ProcessResponsesStream failed: %v", err)
	}

	if doneEvent.Reason != StopReasonLength {
		t.Errorf("expected stop reason 'length' for incomplete status, got %q", doneEvent.Reason)
	}
}

// TestResponsesStreamReaderLineReading tests the SSE reader, including that
// the blank lines SSE uses to separate events are skipped.
func TestResponsesStreamReaderLineReading(t *testing.T) {
	sseData := "data: {\"type\":\"event1\"}\n\ndata: {\"type\":\"event2\"}\ndata: {\"type\":\"event3\"}\n"

	reader := newResponsesStreamReader(strings.NewReader(sseData))

	for _, want := range []string{`{"type":"event1"}`, `{"type":"event2"}`, `{"type":"event3"}`} {
		got, ok := reader.Next()
		if !ok {
			t.Fatalf("Next() ended early, wanted %q", want)
		}
		if got != want {
			t.Errorf("Next() = %q, want %q", got, want)
		}
	}

	if _, ok := reader.Next(); ok {
		t.Error("expected end of stream")
	}
	if err := reader.Error(); err != nil {
		t.Errorf("Error() = %v, want nil on clean EOF", err)
	}
}

// TestResponsesStreamReaderCRLFFragmentation tests CRLF line handling.
func TestResponsesStreamReaderCRLFFragmentation(t *testing.T) {
	sseData := "data: {\"type\":\"event1\"}\r\ndata: {\"type\":\"event2\"}\r\n"

	reader := newResponsesStreamReader(strings.NewReader(sseData))

	for _, want := range []string{`{"type":"event1"}`, `{"type":"event2"}`} {
		got, ok := reader.Next()
		if !ok {
			t.Fatalf("Next() ended early, wanted %q", want)
		}
		// The \r must not survive into the payload.
		if got != want {
			t.Errorf("Next() = %q, want %q", got, want)
		}
	}
}

// TestServiceTierPricingFlex tests flex tier pricing (0.5x).
func TestServiceTierPricingFlex(t *testing.T) {
	usage := &Usage{
		Input:  100,
		Output: 50,
		Cost: Cost{
			Input:  0.1,
			Output: 0.1,
			Total:  0.2,
		},
	}

	ServiceTierPricing(usage, "flex")

	if usage.Cost.Total != 0.1 {
		t.Errorf("expected total cost 0.1 with flex multiplier, got %f", usage.Cost.Total)
	}
}

// TestServiceTierPricingPriority tests priority tier pricing (2x).
func TestServiceTierPricingPriority(t *testing.T) {
	usage := &Usage{
		Input:  100,
		Output: 50,
		Cost: Cost{
			Input:  0.1,
			Output: 0.1,
			Total:  0.2,
		},
	}

	ServiceTierPricing(usage, "priority")

	if usage.Cost.Total != 0.4 {
		t.Errorf("expected total cost 0.4 with priority multiplier, got %f", usage.Cost.Total)
	}
}

// TestServiceTierPricingDefault tests default tier (1x).
func TestServiceTierPricingDefault(t *testing.T) {
	usage := &Usage{
		Input:  100,
		Output: 50,
		Cost: Cost{
			Input:  0.1,
			Output: 0.1,
			Total:  0.2,
		},
	}

	ServiceTierPricing(usage, "")

	if usage.Cost.Total != 0.2 {
		t.Errorf("expected total cost 0.2 with default multiplier, got %f", usage.Cost.Total)
	}
}

// TestServiceTierPricingNilUsage tests nil usage handling.
func TestServiceTierPricingNilUsage(t *testing.T) {
	// Should not panic
	ServiceTierPricing(nil, "flex")
}

// TestNewResponsesStreamReaderEmpty tests empty reader handling.
func TestNewResponsesStreamReaderEmpty(t *testing.T) {
	reader := newResponsesStreamReader(strings.NewReader(""))

	line, ok := reader.Next()
	if ok {
		t.Error("expected end of stream on empty reader")
	}
	if line != "" {
		t.Errorf("expected empty payload, got %q", line)
	}
}

// TestNewResponsesStreamReaderPartialJSON tests that the reader passes
// malformed payloads through rather than dropping the rest of the stream —
// deciding what is valid JSON is the caller's job.
func TestNewResponsesStreamReaderPartialJSON(t *testing.T) {
	sseData := `data: invalid json
data: {"type":"valid"}
`

	reader := newResponsesStreamReader(strings.NewReader(sseData))

	got, ok := reader.Next()
	if !ok {
		t.Fatal("expected the malformed payload to be yielded, not skipped")
	}
	if got != "invalid json" {
		t.Errorf("Next() = %q, want %q", got, "invalid json")
	}

	got, ok = reader.Next()
	if !ok {
		t.Fatal("expected the stream to continue after a malformed payload")
	}
	if got != `{"type":"valid"}` {
		t.Errorf("Next() = %q, want %q", got, `{"type":"valid"}`)
	}
}

// TestParseStreamingJSON tests partial JSON parsing.
func TestParseStreamingJSON(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`{"key": "value"}`, `{"key":"value"}`},
		{`{"key": "`, `{}`},
		{`{"nested": {"inner": "`, `{}`},
		{`[1, 2, 3]`, `{}`},
		{`not json at all`, `{}`},
		{`{"key": "value", "arr": [1,`, `{}`},
	}

	for _, tt := range tests {
		result := ParseStreamingJSON(tt.input)
		if tt.expected == "{}" {
			if len(result) != 0 {
				t.Errorf("ParseStreamingJSON(%q) = %#v, expected empty object", tt.input, result)
			}
			continue
		}
		if len(result) == 0 {
			t.Errorf("ParseStreamingJSON(%q) = empty object, expected non-empty JSON", tt.input)
		}
	}
}

// TestToolCallID tests composite tool call ID building.
func TestToolCallID(t *testing.T) {
	tests := []struct {
		callID, itemID, expected string
	}{
		{"call_123", "fc_456", "call_123|fc_456"},
		{"call_123", "", "call_123"},
		{"", "fc_456", "fc_456"},
		{"", "", ""},
	}

	for _, tt := range tests {
		result := toolCallID(tt.callID, tt.itemID)
		if result != tt.expected {
			t.Errorf("toolCallID(%q, %q) = %q, expected %q", tt.callID, tt.itemID, result, tt.expected)
		}
	}
}

// TestMapResponsesStopReason tests stop reason mapping.
func TestMapResponsesStopReason(t *testing.T) {
	tests := []struct {
		status, reason string
		expected       StopReason
		message        string
	}{
		{status: "completed", expected: StopReasonStop},
		{status: "incomplete", reason: "max_output_tokens", expected: StopReasonLength},
		{status: "incomplete", reason: "content_filter", expected: StopReasonError, message: "Response incomplete: content_filter"},
		{status: "incomplete", expected: StopReasonError, message: "Response incomplete without a provider reason"},
		{status: "failed", expected: StopReasonError},
		{status: "cancelled", expected: StopReasonError},
		{status: "in_progress", expected: StopReasonStop},
		{status: "queued", expected: StopReasonStop},
		{status: "", expected: StopReasonStop},
	}

	for _, tt := range tests {
		result, message := mapResponsesStopReason(tt.status, tt.reason)
		if result != tt.expected || message != tt.message {
			t.Errorf("mapResponsesStopReason(%q, %q) = %q %q, expected %q %q", tt.status, tt.reason, result, message, tt.expected, tt.message)
		}
	}
}

// TestBuildResponseErrorMessage tests error message construction.
func TestBuildResponseErrorMessage(t *testing.T) {
	tests := []struct {
		resp     responsesResponseBody
		expected string
	}{
		{
			responsesResponseBody{Error: responsesError{Code: "err_code", Message: "Error message"}},
			"err_code: Error message",
		},
		{
			responsesResponseBody{Error: responsesError{Message: "Error only"}},
			"Error only",
		},
		{
			responsesResponseBody{Incomplete: responsesIncomplete{Reason: "max tokens"}},
			"incomplete: max tokens",
		},
		{
			responsesResponseBody{},
			"unknown error",
		},
	}

	for _, tt := range tests {
		result := buildResponseErrorMessage(tt.resp)
		if result != tt.expected {
			t.Errorf("buildResponseErrorMessage() = %q, expected %q", result, tt.expected)
		}
	}
}

// TestHasToolCalls tests tool call detection.
func TestHasToolCalls(t *testing.T) {
	tests := []struct {
		content  []ContentBlock
		expected bool
	}{
		{[]ContentBlock{{Type: ContentBlockText, Text: "Hello"}}, false},
		{[]ContentBlock{{Type: ContentBlockToolCall, Name: "tool"}}, true},
		{[]ContentBlock{{Type: ContentBlockThinking}}, false},
		{[]ContentBlock{
			{Type: ContentBlockText, Text: "Hello"},
			{Type: ContentBlockToolCall, Name: "tool"},
		}, true},
		{[]ContentBlock{}, false},
	}

	for _, tt := range tests {
		result := hasToolCalls(tt.content)
		if result != tt.expected {
			t.Errorf("hasToolCalls() = %v, expected %v", result, tt.expected)
		}
	}
}

func TestOpenAISamplingPrecedence(t *testing.T) {
	model := &Model{ID: "test", Reasoning: true, SamplingParams: map[string]any{"temperature": 0.2, "top_p": 0.3, "min_p": 0.05}, SamplingParamsByThinkingLevel: map[string]map[string]any{"off": {"temperature": 0.7}, "high": {"top_k": 20}}}
	body := func(opts ...Option) map[string]any {
		t.Helper()
		request, err := buildOpenAIChatCompletionsRequest(model, &Context{}, applyOptions(opts))
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	// Sampling parameters land after the named fields, so they win over the
	// request temperature, and unmodeled keys pass through.
	got := body(WithTemperature(0.9))
	if got["temperature"] != 0.7 || got["top_p"] != 0.3 || got["min_p"] != 0.05 || got["top_k"] != nil {
		t.Fatalf("off-level sampling = %v", got)
	}
	got = body(WithReasoning(ReasoningHigh), WithSamplingParams(map[string]any{"temperature": 0.1, "repetition_penalty": 1.1}))
	if got["temperature"] != 0.1 || got["top_k"] != float64(20) || got["repetition_penalty"] != 1.1 {
		t.Fatalf("request sampling = %v", got)
	}
	request, err := buildOpenAIChatCompletionsRequest(model, &Context{}, applyOptions([]Option{WithSamplingParams(map[string]any{"top_k": 5})}))
	if err != nil {
		t.Fatal(err)
	}
	if mapped := requireBody(t)(openAIRequestToMap(request)); mapped["top_k"] != float64(5) {
		t.Fatalf("map body = %v", mapped)
	}
}

func TestOpenAICompletionToolStrictPreference(t *testing.T) {
	disabled := &Model{ID: "test", API: APIOpenAICompletions, Provider: ProviderOpenAI, Compat: &CompatibilitySettings{SupportsStrictMode: new(false)}}
	tool := Tool{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)}
	body, err := newOpenAIClientForTest(t, disabled).buildRequest(disabled, &Context{Tools: []Tool{tool}}, &options{})
	if err != nil || strings.Contains(body, `"strict"`) {
		t.Fatalf("catalog-disabled strict mode sent strict: %v %s", err, body)
	}
	tool.ConstrainedSampling = json.RawMessage(`{"type":"json_schema","strict":"require"}`)
	if _, err := newOpenAIClientForTest(t, disabled).buildRequest(disabled, &Context{Tools: []Tool{tool}}, &options{}); err == nil {
		t.Fatal("required strict sampling accepted where the catalog disables strict mode")
	}
	for _, supported := range []*bool{nil, new(true)} {
		model := &Model{ID: "test", API: APIOpenAICompletions, Provider: ProviderOpenAI, Compat: &CompatibilitySettings{SupportsStrictMode: supported}}
		for _, mode := range []string{"prefer", "require"} {
			client := newOpenAIClientForTest(t, model)
			body, err := client.buildRequest(model, &Context{Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"` + mode + `"}`)}}}, &options{})
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				Tools []struct {
					Function struct {
						Strict     bool           `json:"strict"`
						Parameters map[string]any `json:"parameters"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.Unmarshal([]byte(body), &request); err != nil {
				t.Fatal(err)
			}
			if !request.Tools[0].Function.Strict {
				t.Fatalf("%s strict=false", mode)
			}
		}
	}
}

func TestAzureChatCompletionsResolveEndpointAndDeployment(t *testing.T) {
	var path, sentModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		sentModel = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("AZURE_OPENAI_BASE_URL", "")
	t.Setenv("AZURE_OPENAI_RESOURCE_NAME", "")
	t.Setenv("AZURE_OPENAI_DEPLOYMENT_NAME_MAP", "deepseek-v4-pro=my-deepseek")
	model := &Model{ID: "deepseek-v4-pro", API: APIOpenAICompletions, Provider: "azure"}
	client, err := GetClient(model)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hi"}}}, func(context.Context, StreamEvent) {}); err == nil || !strings.Contains(err.Error(), "missing endpoint") {
		t.Fatalf("unconfigured endpoint error = %v", err)
	}
	t.Setenv("AZURE_OPENAI_BASE_URL", server.URL+"/openai/v1")
	message, err := client.Complete(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/openai/v1/chat/completions" || sentModel != "my-deepseek" || message.Model != "deepseek-v4-pro" {
		t.Fatalf("path=%q model=%q message=%+v", path, sentModel, message)
	}
	path = ""
	configured := &Model{ID: "deepseek-v4-pro", API: APIOpenAICompletions, Provider: "azure", BaseURL: "http://127.0.0.1:1/unused"}
	client, err = GetClient(configured)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Complete(t.Context(), configured, &Context{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil || path != "/openai/v1/chat/completions" {
		t.Fatalf("environment endpoint did not override the model base URL: path=%q err=%v", path, err)
	}
}

func TestOpenAICompletionsTerminalOutcomesMatchUpstream(t *testing.T) {
	chunk := func(delta, finish string) string {
		reason := "null"
		if finish != "" {
			reason = `"` + finish + `"`
		}
		return `data: {"id":"c1","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + reason + `}]}`
	}
	text := chunk(`{"content":"hi"}`, "")
	tool := chunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]}`, "")
	for _, tc := range []struct {
		name, want string
		lines      []string
		noFinish   bool
		cancel     bool
		reason     StopReason
	}{
		{name: "ok", lines: []string{text, chunk(`{}`, "stop")}, reason: StopReasonStop},
		{name: "missing finish", lines: []string{text}, reason: StopReasonError, want: "stream ended without finish_reason"},
		{name: "content filter", lines: []string{text, chunk(`{}`, "content_filter")}, reason: StopReasonError, want: "Provider finish_reason: content_filter"},
		{name: "unsupported finish stop", lines: []string{text}, noFinish: true, reason: StopReasonStop},
		{name: "unsupported finish tool", lines: []string{tool}, noFinish: true, reason: StopReasonToolUse},
		{name: "canceled", lines: []string{text, chunk(`{}`, "stop")}, cancel: true, reason: StopReasonAborted, want: "request was aborted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &Model{ID: "m", API: APIOpenAICompletions, Provider: "custom"}
			if tc.noFinish {
				model.Compat = &CompatibilitySettings{SupportsFinishReason: new(false)}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			var terminal []StreamEvent
			err := streamOpenAIResponse(ctx, model, strings.NewReader(strings.Join(append(tc.lines, "data: [DONE]"), "\n\n")+"\n\n"), func(_ context.Context, ev StreamEvent) {
				switch ev.(type) {
				case Done, StreamError:
					terminal = append(terminal, ev)
				}
			})
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			if len(terminal) != 1 {
				t.Fatalf("terminal = %#v", terminal)
			}
			switch e := terminal[0].(type) {
			case Done:
				if tc.want != "" || e.Reason != tc.reason {
					t.Fatalf("done %s, want %s %q", e.Reason, tc.reason, tc.want)
				}
			case StreamError:
				if tc.want == "" || e.Msg.StopReason != tc.reason || !strings.Contains(e.Msg.ErrorMessage, tc.want) {
					t.Fatalf("error %s %q, want %s %q", e.Msg.StopReason, e.Msg.ErrorMessage, tc.reason, tc.want)
				}
			}
		})
	}
}

func TestOpenAICompletionsThinkingFormatsMatchUpstream(t *testing.T) {
	str := func(value string) *string { return &value }
	tests := []struct {
		name      string
		compat    *CompatibilitySettings
		levels    map[string]*string
		reasoning ReasoningLevel
		maxTokens int
		want      map[string]string
	}{
		{name: "together on", compat: &CompatibilitySettings{ThinkingFormat: "together", SupportsReasoningEffort: new(true)}, reasoning: ReasoningHigh, want: map[string]string{"reasoning": `{"enabled":true}`, "reasoning_effort": `"high"`}},
		{name: "together off", compat: &CompatibilitySettings{ThinkingFormat: "together"}, reasoning: ReasoningOff, want: map[string]string{"reasoning": `{"enabled":false}`}},
		{
			name: "baseten vars", compat: &CompatibilitySettings{ThinkingFormat: "baseten", SupportsReasoningEffort: new(true), ChatTemplateArgs: json.RawMessage(`{"enable_thinking":{"$var":"thinking.enabled"},"effort":{"$var":"thinking.effort","omitWhenOff":true},"budget":{"$var":"thinking.budget"},"fixed":1}`)},
			levels: map[string]*string{"low": str("light")}, reasoning: ReasoningLow, maxTokens: 3000,
			want: map[string]string{"chat_template_args": `{"budget":1976,"effort":"light","enable_thinking":true,"fixed":1}`, "reasoning_effort": `"light"`},
		},
		{
			name: "baseten off", compat: &CompatibilitySettings{ThinkingFormat: "baseten", SupportsReasoningEffort: new(true), ChatTemplateArgs: json.RawMessage(`{"enable_thinking":{"$var":"thinking.enabled"},"effort":{"$var":"thinking.effort","omitWhenOff":true}}`)},
			levels: map[string]*string{"off": str("none")}, reasoning: ReasoningOff,
			want: map[string]string{"chat_template_args": `{"enable_thinking":false}`, "reasoning_effort": `"none"`},
		},
		{name: "chat template budget field", compat: &CompatibilitySettings{ThinkingFormat: "chat-template", ThinkingTokenBudgetField: "thinking_budget_tokens", ChatTemplateKwargs: json.RawMessage(`{"reasoning":{"$var":"thinking.enabled"}}`)}, reasoning: ReasoningMedium, want: map[string]string{"chat_template_kwargs": `{"reasoning":true}`, "thinking_budget_tokens": "8192"}},
		{name: "ant-ling unmapped", compat: &CompatibilitySettings{ThinkingFormat: "ant-ling"}, reasoning: ReasoningHigh, want: map[string]string{"reasoning": ""}},
		{name: "ant-ling mapped", compat: &CompatibilitySettings{ThinkingFormat: "ant-ling"}, levels: map[string]*string{"high": str("deep")}, reasoning: ReasoningHigh, want: map[string]string{"reasoning": `{"effort":"deep"}`}},
		{name: "string thinking off", compat: &CompatibilitySettings{ThinkingFormat: "string-thinking"}, reasoning: ReasoningOff, want: map[string]string{"thinking": `"none"`}},
		{name: "openrouter mapped off", compat: &CompatibilitySettings{ThinkingFormat: "openrouter"}, levels: map[string]*string{"off": str("minimal")}, want: map[string]string{"reasoning": `{"effort":"minimal"}`}},
		{name: "deepseek unsupported off", compat: &CompatibilitySettings{ThinkingFormat: "deepseek"}, levels: map[string]*string{"off": nil}, reasoning: ReasoningOff, want: map[string]string{"thinking": ""}},
		{name: "openai off value", compat: &CompatibilitySettings{SupportsReasoningEffort: new(true)}, levels: map[string]*string{"off": str("none")}, reasoning: ReasoningOff, want: map[string]string{"reasoning_effort": `"none"`}},
		{name: "openai clamps to supported level", compat: &CompatibilitySettings{SupportsReasoningEffort: new(true)}, levels: map[string]*string{"minimal": nil}, reasoning: ReasoningMinimal, want: map[string]string{"reasoning_effort": `"low"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := &Model{ID: "m", Provider: "custom", API: APIOpenAICompletions, BaseURL: "https://example.com/v1", Reasoning: true, MaxTokens: 32000, Compat: tc.compat, ThinkingLevelMap: tc.levels}
			o := &options{reasoning: tc.reasoning, maxTokens: tc.maxTokens}
			body, err := newOpenAIClientForTest(t, model).buildRequest(model, &Context{}, o)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &fields); err != nil {
				t.Fatal(err)
			}
			for field, want := range tc.want {
				if string(fields[field]) != want {
					t.Errorf("%s = %s, want %q in %s", field, fields[field], want, body)
				}
			}
		})
	}
}

func TestOpenAICompletionsReplaysThinkingInItsStreamedField(t *testing.T) {
	model := &Model{ID: "local", API: APIOpenAICompletions, Provider: "custom", BaseURL: "http://localhost:8080/v1", Reasoning: true}
	stream := "data: {\"choices\":[{\"delta\":{\"reasoning\":\"step one\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n"
	var done *Done
	if err := streamOpenAIResponse(context.Background(), model, strings.NewReader(stream), func(_ context.Context, event StreamEvent) {
		if d, ok := event.(Done); ok {
			done = &d
		}
	}); err != nil || done == nil {
		t.Fatalf("stream: %v %v", err, done)
	}
	if done.Message.Content[0].ThoughtSignature != "reasoning" {
		t.Fatalf("thinking block = %+v, want the reasoning field recorded", done.Message.Content[0])
	}

	reply := Message{Role: "assistant", Provider: model.Provider, Model: model.ID, API: model.API, Content: done.Message.Content}
	body, err := newOpenAIClientForTest(t, model).buildRequest(model, &Context{Messages: []Message{{Role: "user", Content: "q"}, reply}}, &options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"content":"answer","reasoning":"step one"`) {
		t.Fatalf("replay = %s, want the answer as content and thinking under reasoning", body)
	}

	deepseek := &Model{ID: "deepseek-reasoner", API: APIOpenAICompletions, Provider: ProviderDeepSeek, BaseURL: "https://api.deepseek.com", Reasoning: true}
	body, err = newOpenAIClientForTest(t, deepseek).buildRequest(deepseek, &Context{Messages: []Message{{Role: "user", Content: "q"}, {Role: "assistant", Provider: ProviderDeepSeek, Model: "deepseek-reasoner", API: APIOpenAICompletions, Content: []ContentBlock{{Type: ContentBlockText, Text: "a"}}}}}, &options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"content":"a","reasoning_content":""`) {
		t.Fatalf("deepseek replay = %s, want an empty reasoning_content", body)
	}
}

func TestOpenAICompletionsGrammarToolsMatchUpstream(t *testing.T) {
	code := Tool{Name: "code", Description: "run", Parameters: json.RawMessage(`{"type":"object","properties":{"source":{"type":"string"}},"required":["source"]}`), ConstrainedSampling: json.RawMessage(`{"type":"grammar","variants":{"openai_regex":"x+"}}`)}
	prompt := &Context{Tools: []Tool{code}, Messages: []Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockToolCall, ID: "call_1", Name: "code", Arguments: json.RawMessage(`{"source":"xx"}`)}}},
		{Role: "toolResult", ToolCallID: "call_1", ToolName: "code", Content: []ContentBlock{{Type: ContentBlockText, Text: "ok"}}},
	}}
	model := &Model{ID: "gpt", API: APIOpenAICompletions, Provider: ProviderOpenAI, Compat: &CompatibilitySettings{SupportsOpenAIGrammarTools: true}}
	body, err := newOpenAIClientForTest(t, model).buildRequest(model, prompt, &options{})
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Tools    []map[string]any `json:"tools"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatal(err)
	}
	wantTool := map[string]any{"type": "custom", "custom": map[string]any{"name": "code", "description": "run", "format": map[string]any{"type": "grammar", "grammar": map[string]any{"syntax": "regex", "definition": "x+"}}}}
	if !reflect.DeepEqual(request.Tools[0], wantTool) {
		t.Fatalf("tool = %v", request.Tools[0])
	}
	wantCall := []any{map[string]any{"id": "call_1", "type": "custom", "custom": map[string]any{"name": "code", "input": "xx"}}}
	if !reflect.DeepEqual(request.Messages[1]["tool_calls"], wantCall) {
		t.Fatalf("replayed call = %v", request.Messages[1]["tool_calls"])
	}

	plain := &Model{ID: "gpt", API: APIOpenAICompletions, Provider: ProviderOpenAI}
	body, err = newOpenAIClientForTest(t, plain).buildRequest(plain, prompt, &options{})
	if err != nil || !strings.Contains(body, `"type":"function"`) || strings.Contains(body, `"custom"`) {
		t.Fatalf("grammar tool without grammar support = %s, %v", body, err)
	}

	stream := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2","type":"custom","custom":{"name":"code","input":"x"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"custom":{"input":"\"y"}}]}}]}`,
		`data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	var (
		deltas strings.Builder
		done   *Done
	)
	err = streamOpenAIResponseWithAPI(t.Context(), model, strings.NewReader(stream), func(_ context.Context, event StreamEvent) {
		switch e := event.(type) {
		case ToolCallDelta:
			deltas.WriteString(e.Delta)
		case Done:
			done = &e
		}
	}, map[string]string{"code": "source"}, model.API, model.Provider)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"source":"x\"y"}`
	if deltas.String() != want || done == nil || string(done.Message.Content[0].Arguments) != want || done.Message.Content[0].Name != "code" {
		t.Fatalf("deltas = %s, done = %+v", deltas.String(), done)
	}
}

func TestOpenAISharedThinkingBudgets(t *testing.T) {
	model := &Model{ID: "local", Reasoning: true, MaxTokens: 16000}
	for _, tc := range []struct {
		level ReasoningLevel
		want  int
	}{{ReasoningLow, 0}, {ReasoningXHigh, 3000}, {ReasoningMax, 3000}, {ReasoningOff, 0}} {
		t.Run(string(tc.level), func(t *testing.T) {
			opts := applyOptions([]Option{WithReasoning(tc.level), WithThinkingBudgets(&ThinkingBudgets{Low: new(0), High: new(3000)})})
			effort := string(tc.level)
			if tc.level == ReasoningOff {
				effort = ""
			}
			if got := openAIThinkingBudget(&openAIRequest{MaxTokens: 8000}, model, opts, effort); got != tc.want {
				t.Fatalf("budget=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestOpenAISharedThinkingBudgetRequest(t *testing.T) {
	model := &Model{ID: "local", Reasoning: true, MaxTokens: 16000, Compat: &CompatibilitySettings{SupportsThinkingTokenBudget: true}}
	for _, tc := range []struct {
		name  string
		extra []Option
		want  int
	}{
		{"shared", nil, 3000},
		{"answer room", []Option{WithMaxTokens(2000)}, 976},
		{"token override", []Option{WithThinkingBudgetTokens(1000)}, 1000},
		{"off", []Option{WithReasoning(ReasoningOff)}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := []Option{WithReasoning(ReasoningHigh), WithThinkingBudgets(&ThinkingBudgets{High: new(3000)})}
			body, err := (&openAIClient{}).buildRequest(model, &Context{}, applyOptions(append(opts, tc.extra...)))
			require.NoError(t, err)
			var req struct {
				Budget int `json:"thinking_token_budget"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &req))
			require.Equal(t, tc.want, req.Budget)
		})
	}
}

func TestCompletionsPromptCachePolicy(t *testing.T) {
	for _, client := range []string{"direct", "compatible"} {
		for _, scenario := range []struct {
			name, baseURL, provider, retention, env, session, key, ttl string
			long                                                       *bool
		}{
			{name: "default short", baseURL: "https://api.openai.com/v1", session: "session", key: "session"},
			{name: "provider default endpoint", session: "session", key: "session"},
			{name: "long", baseURL: "https://api.openai.com/v1", retention: "long", session: "session", key: "session", ttl: "24h"},
			{name: "none", baseURL: "https://api.openai.com/v1", retention: "none", env: "long", session: "session"},
			{name: "environment", baseURL: "https://api.openai.com/v1", env: "long", session: "session", key: "session", ttl: "24h"},
			{name: "explicit short overrides environment", baseURL: "https://api.openai.com/v1", env: "long", retention: "short", session: "session", key: "session"},
			{name: "proxy rejects long", baseURL: "https://proxy.example/v1", long: new(false), retention: "long", session: "session"},
			{name: "proxy accepts long", baseURL: "https://proxy.example/v1", long: new(true), retention: "long", session: "session", key: "session", ttl: "24h"},
			{name: "proxy short", baseURL: "https://proxy.example/v1", long: new(true), retention: "short", session: "session"},
			{name: "no session", baseURL: "https://api.openai.com/v1", retention: "long", ttl: "24h"},
			{name: "unicode key", baseURL: "https://api.openai.com/v1", session: strings.Repeat("界", 65), key: strings.Repeat("界", 64)},
			{name: "unsupported detected provider", baseURL: "https://api.together.xyz/v1", provider: ProviderTogether, retention: "long", session: "session"},
		} {
			t.Run(client+"/"+scenario.name, func(t *testing.T) {
				t.Setenv("PI_CACHE_RETENTION", scenario.env)
				model := &Model{ID: "model", API: APIOpenAICompletions, Provider: ProviderOpenAI, BaseURL: scenario.baseURL, Compat: &CompatibilitySettings{SupportsLongCacheRetention: scenario.long}}
				if scenario.provider != "" {
					model.Provider = scenario.provider
				}
				prompt := &Context{Messages: []Message{{Role: "user", Content: "hello"}}}
				o := applyOptions([]Option{WithCacheRetention(scenario.retention), WithSessionID(scenario.session)})
				var body map[string]any
				if client == "direct" {
					encoded, err := (&openAIClient{}).buildRequest(model, prompt, o)
					require.NoError(t, err)
					require.NoError(t, json.Unmarshal([]byte(encoded), &body))
				} else {
					var err error
					body, err = buildOpenAICompatRequestBody(model, prompt, o)
					require.NoError(t, err)
				}
				if scenario.key == "" {
					require.NotContains(t, body, "prompt_cache_key")
				} else {
					require.Equal(t, scenario.key, body["prompt_cache_key"])
				}
				if scenario.ttl == "" {
					require.NotContains(t, body, "prompt_cache_retention")
				} else {
					require.Equal(t, scenario.ttl, body["prompt_cache_retention"])
				}
				if model.GetCompat().SupportsStore {
					require.Equal(t, false, body["store"])
				} else {
					require.NotContains(t, body, "store")
				}
			})
		}
	}
}

func TestCompletionsCacheMarkersFollowFinalRequest(t *testing.T) {
	for _, scenario := range []struct {
		name, provider, id, format, retention, env, ttl, tail string
		long                                                  *bool
		marked                                                bool
	}{
		{name: "detected OpenRouter", provider: ProviderOpenRouter, id: "anthropic/model", marked: true},
		{name: "disabled OpenRouter", provider: ProviderOpenRouter, id: "anthropic/model", retention: "none"},
		{name: "custom format", format: "anthropic", marked: true},
		{name: "unknown format", format: "other"},
		{name: "explicit format disables detection", provider: ProviderOpenRouter, id: "anthropic/model", format: "none"},
		{name: "long markers", format: "anthropic", retention: "long", marked: true, ttl: "1h"},
		{name: "environment markers", format: "anthropic", env: "long", marked: true, ttl: "1h"},
		{name: "unsupported long markers", format: "anthropic", retention: "long", long: new(false), marked: true},
		{name: "tool result", format: "anthropic", tail: "tool", marked: true},
		{name: "image tail", format: "anthropic", tail: "image", marked: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("PI_CACHE_RETENTION", scenario.env)
			model := &Model{ID: scenario.id, API: APIOpenAICompletions, Provider: scenario.provider, BaseURL: "https://proxy.example/v1", Input: []InputType{InputText, InputImage}, Compat: &CompatibilitySettings{CacheControlFormat: scenario.format, SupportsLongCacheRetention: scenario.long}}
			prompt := &Context{SystemPrompt: "policy", Messages: []Message{{Role: "user", Content: "hello"}}, Tools: []Tool{{Name: "first", Parameters: json.RawMessage(`{"type":"object"}`)}, {Name: "last", Parameters: json.RawMessage(`{"type":"object"}`)}}}
			if scenario.tail == "tool" {
				prompt.Messages = append(prompt.Messages, Message{Role: "assistant", API: model.API, Provider: model.Provider, Model: model.ID, Content: []ContentBlock{{Type: ContentBlockToolCall, ID: "call", Name: "last", Arguments: json.RawMessage(`{}`)}}}, Message{Role: "toolResult", ToolCallID: "call", ToolName: "last", Content: []ContentBlock{{Type: ContentBlockText, Text: "done"}}})
			}
			if scenario.tail == "image" {
				prompt.Messages = append(prompt.Messages, Message{Role: "user", Content: []ContentBlock{{Type: ContentBlockImage, MimeType: "image/png", Data: "AA=="}}})
			}
			before, err := json.Marshal(prompt)
			require.NoError(t, err)
			body, err := buildOpenAICompatRequestBody(model, prompt, applyOptions([]Option{WithCacheRetention(scenario.retention)}))
			require.NoError(t, err)
			after, err := json.Marshal(prompt)
			require.NoError(t, err)
			require.Equal(t, string(before), string(after))
			messages := body["messages"].([]any)
			tools := body["tools"].([]any)
			marker := map[string]any{"type": "ephemeral"}
			if scenario.ttl != "" {
				marker["ttl"] = scenario.ttl
			}
			last := len(messages) - 1
			if scenario.tail == "image" {
				last--
			}
			if scenario.marked {
				require.IsType(t, []any{}, messages[0].(map[string]any)["content"])
				require.Equal(t, marker, messages[0].(map[string]any)["content"].([]any)[0].(map[string]any)["cache_control"])
				require.Equal(t, marker, tools[1].(map[string]any)["cache_control"])
				require.IsType(t, []any{}, messages[last].(map[string]any)["content"])
				require.Equal(t, marker, messages[last].(map[string]any)["content"].([]any)[0].(map[string]any)["cache_control"])
				if scenario.tail == "tool" {
					require.Equal(t, "hello", messages[1].(map[string]any)["content"])
				}
			} else {
				encoded, err := json.Marshal(body)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), "cache_control")
			}
			require.NotContains(t, tools[0].(map[string]any), "cache_control")
		})
	}
}

func TestCompletionsCachePolicyReachesStreamTransport(t *testing.T) {
	for _, api := range []string{APIOpenAICompletions, APIOpenAICompatible} {
		for _, retention := range []string{"none", "long"} {
			t.Run(api+"/"+retention, func(t *testing.T) {
				t.Setenv("PI_CACHE_RETENTION", "long")
				var requests, callbacks, done int
				original := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = original })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					require.Equal(t, "/v1/chat/completions", req.URL.Path)
					var body map[string]any
					require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
					require.Equal(t, false, body["store"])
					if retention == "long" {
						require.Equal(t, "session", body["prompt_cache_key"])
						require.Equal(t, "24h", body["prompt_cache_retention"])
						require.Equal(t, "1h", body["tools"].([]any)[0].(map[string]any)["cache_control"].(map[string]any)["ttl"])
					} else {
						require.NotContains(t, body, "prompt_cache_key")
						require.NotContains(t, body, "prompt_cache_retention")
						require.NotContains(t, body["tools"].([]any)[0].(map[string]any), "cache_control")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))}, nil
				})}
				model := &Model{ID: "custom", API: api, Provider: ProviderOpenAI, BaseURL: "https://api.openai.com/v1", Compat: &CompatibilitySettings{CacheControlFormat: "anthropic"}}
				var client Client
				if api == APIOpenAICompletions {
					var err error
					client, err = newOpenAIClient(model)
					require.NoError(t, err)
				} else {
					client = &openAICompatClient{model: model}
				}
				err := client.Stream(t.Context(), model, &Context{SystemPrompt: "policy", Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []Tool{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}}, func(_ context.Context, event StreamEvent) {
					if _, ok := event.(Done); ok {
						done++
					}
				}, WithAPIKey("test-key"), WithSessionID("session"), WithCacheRetention(retention), WithPayloadCallback(func(payload any) any {
					callbacks++
					encoded, err := json.Marshal(payload)
					require.NoError(t, err)
					if retention == "none" {
						require.NotContains(t, string(encoded), "cache_control")
					} else {
						require.Contains(t, string(encoded), "cache_control")
					}
					return nil
				}))
				require.NoError(t, err)
				require.Equal(t, 1, requests)
				require.Equal(t, 1, callbacks)
				require.Equal(t, 1, done)
			})
		}
	}
}

func TestClampOpenAIPromptCacheKey(t *testing.T) {
	for _, key := range []string{"", "session", strings.Repeat("界", 64), strings.Repeat("界", 63) + "😀z", strings.Repeat("e\u0301", 33)} {
		chars := []rune(key)
		want := string(chars[:min(len(chars), 64)])
		require.Equal(t, want, clampOpenAIPromptCacheKey(key))
	}
}
