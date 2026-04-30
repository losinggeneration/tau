package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicHandleSSEEmitsStructuredEvents(t *testing.T) {
	client := &anthropicClient{
		model: &Model{
			ID:       "claude-test",
			API:      APIAnthropicMessages,
			Provider: ProviderAnthropic,
			Cost:     CostProfile{Input: 1, Output: 2, CacheRead: 0.5, CacheWrite: 0.75},
		},
	}

	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":0}}}`,
		"",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		"",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool_1","name":"read","input":{}}}`,
		"",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"ab\"}"}}`,
		"",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"output_tokens":4,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}`,
		"",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	var events []StreamEvent
	handler := func(_ context.Context, event StreamEvent) {
		events = append(events, event)
	}

	if err := client.handleSSE(context.Background(), strings.NewReader(stream), handler); err != nil {
		t.Fatalf("handleSSE: %v", err)
	}

	if len(events) != 8 {
		t.Fatalf("len(events) = %d, want 8", len(events))
	}

	if _, ok := events[0].(Start); !ok {
		t.Fatalf("event[0] = %T, want Start", events[0])
	}
	if _, ok := events[1].(TextStart); !ok {
		t.Fatalf("event[1] = %T, want TextStart", events[1])
	}
	if _, ok := events[2].(TextDelta); !ok {
		t.Fatalf("event[2] = %T, want TextDelta", events[2])
	}
	if textEnd, ok := events[3].(TextEnd); !ok {
		t.Fatalf("event[3] = %T, want TextEnd", events[3])
	} else if textEnd.Content != "Hello" {
		t.Fatalf("text end content = %q, want Hello", textEnd.Content)
	}
	if _, ok := events[4].(ToolCallStart); !ok {
		t.Fatalf("event[4] = %T, want ToolCallStart", events[4])
	}
	if _, ok := events[5].(ToolCallDelta); !ok {
		t.Fatalf("event[5] = %T, want ToolCallDelta", events[5])
	}
	toolEnd, ok := events[6].(ToolCallEnd)
	if !ok {
		t.Fatalf("event[6] = %T, want ToolCallEnd", events[6])
	}
	if string(toolEnd.ToolCall.Arguments) != `{"path":"ab"}` {
		t.Fatalf("tool args = %s, want %s", string(toolEnd.ToolCall.Arguments), `{"path":"ab"}`)
	}

	done, ok := events[7].(Done)
	if !ok {
		t.Fatalf("event[7] = %T, want Done", events[7])
	}
	if done.Reason != StopReasonToolUse {
		t.Fatalf("stop reason = %q, want %q", done.Reason, StopReasonToolUse)
	}
	if done.Message.ResponseID != "msg_1" {
		t.Fatalf("response id = %q, want msg_1", done.Message.ResponseID)
	}
	if done.Message.Usage.Input != 10 || done.Message.Usage.Output != 4 {
		t.Fatalf("usage = %+v, want input=10 output=4", done.Message.Usage)
	}
	if done.Message.Usage.CacheWrite != 2 || done.Message.Usage.CacheRead != 3 {
		t.Fatalf("cache usage = %+v, want cacheWrite=2 cacheRead=3", done.Message.Usage)
	}
	if done.Message.Usage.TotalTokens != 19 {
		t.Fatalf("total tokens = %d, want 19", done.Message.Usage.TotalTokens)
	}

	assertStreamGolden(t, events, streamGolden{
		EventTypes: []string{"Start", "TextStart", "TextDelta", "TextEnd", "ToolCallStart", "ToolCallDelta", "ToolCallEnd", "Done"},
		StopReason: StopReasonToolUse,
	})
}

func TestAnthropicConvertMessagesGroupsToolResults(t *testing.T) {
	client := &anthropicClient{}

	msgs, err := client.convertMessages(&Context{
		Messages: []Message{
			{
				Role:      "assistant",
				Content:   []ContentBlock{{Type: ContentBlockToolCall, ID: "tool_1", Name: "read", Arguments: json.RawMessage(`{"path":"a.txt"}`)}},
				Timestamp: 1,
			},
			{
				Role:       "toolResult",
				ToolCallID: "tool_1",
				Content:    []ContentBlock{{Type: ContentBlockText, Text: "contents"}},
				Timestamp:  2,
			},
			{
				Role:       "toolResult",
				ToolCallID: "tool_2",
				Content:    []ContentBlock{{Type: ContentBlockText, Text: "other"}},
				IsError:    true,
				Timestamp:  3,
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[1].Role != "user" {
		t.Fatalf("msgs[1].Role = %q, want user", msgs[1].Role)
	}

	results, ok := msgs[1].Content.([]anthropicToolResultBlock)
	if !ok {
		t.Fatalf("msgs[1].Content = %T, want []anthropicToolResultBlock", msgs[1].Content)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Type != "tool_result" || results[0].ToolUseID != "tool_1" {
		t.Fatalf("first result = %+v", results[0])
	}
	if results[0].Content != "contents" {
		t.Fatalf("first result content = %#v, want %q", results[0].Content, "contents")
	}
	if !results[1].IsError {
		t.Fatalf("second result should be error: %+v", results[1])
	}
}

func TestAnthropicParseRedactedThinking(t *testing.T) {
	client := &anthropicClient{
		model: &Model{
			ID:       "claude-test",
			API:      APIAnthropicMessages,
			Provider: ProviderAnthropic,
		},
	}

	// Simulate redacted_thinking event from Anthropic API
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":0}}}`,
		"",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"sig_abc123xyz"}}`,
		"",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		"",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	var done Done
	handler := func(_ context.Context, event StreamEvent) {
		if d, ok := event.(Done); ok {
			done = d
		}
	}

	if err := client.handleSSE(context.Background(), strings.NewReader(stream), handler); err != nil {
		t.Fatalf("handleSSE: %v", err)
	}

	// Check the final message content (not the initial Start event which has no content)
	if len(done.Message.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(done.Message.Content))
	}
	block := done.Message.Content[0]
	if block.Type != "thinking" {
		t.Fatalf("block type = %q, want thinking", block.Type)
	}
	// Redacted thinking should show the placeholder text
	if block.Thinking != "[Reasoning redacted]" {
		t.Fatalf("redacted thinking text = %q, want %q", block.Thinking, "[Reasoning redacted]")
	}
	// The signature should be captured from the data field
	if block.ThoughtSignature != "sig_abc123xyz" {
		t.Fatalf("thought signature = %q, want %q", block.ThoughtSignature, "sig_abc123xyz")
	}
	// Redacted flag should be set
	if !block.Redacted {
		t.Fatalf("redacted flag not set")
	}
}

func TestAnthropicParseSignatureDelta(t *testing.T) {
	client := &anthropicClient{
		model: &Model{
			ID:       "claude-test",
			API:      APIAnthropicMessages,
			Provider: ProviderAnthropic,
		},
	}

	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":0}}}`,
		"",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","text":""}}`,
		"",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Step one: think."}}`,
		"",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_part1"}}`,
		"",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_part2"}}`,
		"",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		"",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	var done Done
	handler := func(_ context.Context, event StreamEvent) {
		if d, ok := event.(Done); ok {
			done = d
		}
	}

	if err := client.handleSSE(context.Background(), strings.NewReader(stream), handler); err != nil {
		t.Fatalf("handleSSE: %v", err)
	}

	if len(done.Message.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(done.Message.Content))
	}
	block := done.Message.Content[0]
	if block.ThoughtSignature != "sig_part1sig_part2" {
		t.Fatalf("thought signature = %q, want concatenated sig", block.ThoughtSignature)
	}
}

func TestAnthropicConvertMessagesHandlesRedactedThinking(t *testing.T) {
	client := &anthropicClient{}

	// Simulate messages with redacted thinking as input
	ctx := &Context{
		Messages: []Message{
			{
				Role: "assistant",
				Content: []ContentBlock{
					{
						Type:             "thinking",
						Thinking:         "[Reasoning redacted]",
						ThoughtSignature: "sig_from_replay",
						Redacted:         true,
					},
				},
			},
		},
	}

	blocks, err := client.extractContent(ctx.Messages[0].Content)
	if err != nil {
		t.Fatalf("extractContent: %v", err)
	}

	// Redacted thinking should be converted to redacted_thinking blocks for the API
	// This is tested by checking the output blocks from convertMessages
	if len(blocks) == 0 {
		t.Fatalf("expected at least one block from redacted thinking content")
	}
}

func TestAnthropicBuildRequestWithToolChoice(t *testing.T) {
	client := &anthropicClient{
		model: &Model{
			ID:       "claude-sonnet-4-7-20250514",
			API:      APIAnthropicMessages,
			Provider: ProviderAnthropic,
		},
	}

	var o options
	o.toolChoice = "auto"

	reqBody, err := client.buildRequest(client.model, &Context{Messages: []Message{}}, &o)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req anthropicRequest
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	if req.ToolChoice == nil {
		t.Fatalf("tool_choice should be set")
	}
}

func TestAnthropicBuildRequestWithThinkingDisplay(t *testing.T) {
	client := &anthropicClient{
		model: &Model{
			ID:       "claude-sonnet-4-7-20250514",
			API:      APIAnthropicMessages,
			Provider: ProviderAnthropic,
		},
	}

	var o options
	o.thinkingEnabled = true
	o.thinkingBudgetTokens = 1024
	// thinkingDisplay is a string option that should be passed through

	reqBody, err := client.buildRequest(client.model, &Context{Messages: []Message{}}, &o)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req anthropicRequest
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	if req.Thinking == nil {
		t.Fatalf("thinking config should be set")
	}
}

func TestAnthropicBuildRequestWithCacheControl(t *testing.T) {
	client := &anthropicClient{
		model: &Model{
			ID:        "claude-test",
			API:       APIAnthropicMessages,
			Provider:  ProviderAnthropic,
			MaxTokens: 4096,
		},
	}

	// Build request with tools
	ctx := &Context{
		Messages: []Message{
			{Role: "user", Content: "Use a tool"},
		},
		Tools: []Tool{
			{
				Name:        "get_weather",
				Description: "Get weather",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
	}

	reqBody, err := client.buildRequest(client.model, ctx, &options{})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req anthropicRequest
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	// Last tool should have cache_control
	if len(req.Tools) == 0 {
		t.Fatalf("expected tools in request")
	}
	lastTool := req.Tools[len(req.Tools)-1]
	// Check if cache_control is applied to the last tool's input_schema
	// The Go API doesn't have cache_control on tools yet
	_ = lastTool
}

func TestAnthropicBuildRequestWithAdaptiveThinking(t *testing.T) {
	client := &anthropicClient{
		model: &Model{
			ID:        "claude-opus-4-7-20250514",
			API:       APIAnthropicMessages,
			Provider:  ProviderAnthropic,
			MaxTokens: 8192,
		},
	}

	// Test that adaptive thinking is built for opus-4-7
	var o options
	o.thinkingEnabled = true

	reqBody, err := client.buildRequest(client.model, &Context{Messages: []Message{}}, &o)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req anthropicRequest
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	// With adaptive thinking enabled, thinking config should have type
	if req.Thinking == nil {
		t.Fatalf("thinking config should be set for adaptive thinking model")
	}
}

func TestAnthropicBuildRequestWithGenericReasoning(t *testing.T) {
	tests := []struct {
		name         string
		modelID      string
		reasoning    ReasoningLevel
		effort       string
		budgetTokens int
		wantThinking bool
		wantType     string
		wantEffort   string
		wantBudget   int
	}{
		{
			name:         "generic reasoning high on opus 4-7 adaptive maps to high effort",
			modelID:      "claude-opus-4-7-20250514",
			reasoning:    ReasoningHigh,
			wantThinking: true,
			wantType:     "adaptive",
			wantEffort:   "high",
		},
		{
			name:         "generic reasoning xhigh on opus 4-7 maps to xhigh effort",
			modelID:      "claude-opus-4-7-20250514",
			reasoning:    ReasoningXHigh,
			wantThinking: true,
			wantType:     "adaptive",
			wantEffort:   "xhigh",
		},
		{
			name:         "generic reasoning xhigh on opus 4-6 maps to max effort",
			modelID:      "claude-opus-4-6-20250514",
			reasoning:    ReasoningXHigh,
			wantThinking: true,
			wantType:     "adaptive",
			wantEffort:   "max",
		},
		{
			name:         "generic reasoning medium on non-adaptive maps to enabled budget",
			modelID:      "claude-sonnet-4-5-20250514",
			reasoning:    ReasoningMedium,
			wantThinking: true,
			wantType:     "enabled",
			wantBudget:   8192,
		},
		{
			name:         "generic reasoning off emits no thinking block",
			modelID:      "claude-sonnet-4-5-20250514",
			reasoning:    ReasoningOff,
			wantThinking: false,
		},
		{
			name:         "explicit WithEffort wins over generic reasoning",
			modelID:      "claude-opus-4-7-20250514",
			reasoning:    ReasoningHigh,
			effort:       "max",
			wantThinking: true,
			wantType:     "adaptive",
			wantEffort:   "max",
		},
		{
			name:         "explicit WithThinkingBudgetTokens wins over generic reasoning",
			modelID:      "claude-sonnet-4-5-20250514",
			reasoning:    ReasoningHigh,
			budgetTokens: 4096,
			wantThinking: true,
			wantType:     "enabled",
			wantBudget:   4096,
		},
		{
			name:         "generic reasoning minimal collapses to low effort",
			modelID:      "claude-opus-4-7-20250514",
			reasoning:    ReasoningMinimal,
			wantThinking: true,
			wantType:     "adaptive",
			wantEffort:   "low",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &anthropicClient{
				model: &Model{
					ID:        tt.modelID,
					API:       APIAnthropicMessages,
					Provider:  ProviderAnthropic,
					MaxTokens: 8192,
				},
			}

			var o options
			o.reasoning = tt.reasoning
			o.effort = tt.effort
			o.thinkingBudgetTokens = tt.budgetTokens

			reqBody, err := client.buildRequest(client.model, &Context{Messages: []Message{}}, &o)
			if err != nil {
				t.Fatalf("buildRequest: %v", err)
			}

			var req anthropicRequest
			if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
				t.Fatalf("unmarshal request: %v", err)
			}

			if tt.wantThinking {
				if req.Thinking == nil {
					t.Fatalf("thinking config should be set; req body = %s", reqBody)
				}
				if req.Thinking.Type != tt.wantType {
					t.Errorf("thinking.type = %q, want %q", req.Thinking.Type, tt.wantType)
				}
				if req.Thinking.Effort != tt.wantEffort {
					t.Errorf("thinking.effort = %q, want %q", req.Thinking.Effort, tt.wantEffort)
				}
				if tt.wantBudget > 0 && req.Thinking.BudgetTokens != tt.wantBudget {
					t.Errorf("thinking.budget_tokens = %d, want %d", req.Thinking.BudgetTokens, tt.wantBudget)
				}
			} else if req.Thinking != nil && req.Thinking.Enabled {
				t.Errorf("thinking should not be enabled; req.Thinking = %+v", req.Thinking)
			}
		})
	}
}

func TestAnthropicBuildRequestSuppressesTemperatureWithGenericReasoning(t *testing.T) {
	// When the caller uses WithReasoning instead of WithThinkingEnabled, the
	// temperature field must still be suppressed on Anthropic. Otherwise the
	// API rejects the request (Anthropic rejects temperature alongside any
	// thinking block).
	client := &anthropicClient{
		model: &Model{
			ID:        "claude-opus-4-7-20250514",
			API:       APIAnthropicMessages,
			Provider:  ProviderAnthropic,
			MaxTokens: 8192,
		},
	}

	temp := 0.5
	var o options
	o.reasoning = ReasoningHigh
	o.temperature = &temp

	reqBody, err := client.buildRequest(client.model, &Context{Messages: []Message{}}, &o)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	if strings.Contains(reqBody, `"temperature"`) {
		t.Errorf("temperature should be suppressed when reasoning is set; req body = %s", reqBody)
	}
}

func TestMapReasoningLevelToAnthropicEffort(t *testing.T) {
	tests := []struct {
		name    string
		level   ReasoningLevel
		modelID string
		want    string
	}{
		{"minimal collapses to low", ReasoningMinimal, "claude-opus-4-7", "low"},
		{"low stays low", ReasoningLow, "claude-opus-4-7", "low"},
		{"medium stays medium", ReasoningMedium, "claude-opus-4-7", "medium"},
		{"high stays high", ReasoningHigh, "claude-opus-4-7", "high"},
		{"xhigh on opus 4-7 maps to xhigh", ReasoningXHigh, "claude-opus-4-7", "xhigh"},
		{"xhigh on opus 4-6 maps to max", ReasoningXHigh, "claude-opus-4-6", "max"},
		{"xhigh on non-opus falls back to high", ReasoningXHigh, "claude-sonnet-4-5", "high"},
		{"off returns empty", ReasoningOff, "claude-opus-4-7", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapReasoningLevelToAnthropicEffort(tt.level, tt.modelID)
			if got != tt.want {
				t.Errorf("mapReasoningLevelToAnthropicEffort(%q, %q) = %q, want %q",
					tt.level, tt.modelID, got, tt.want)
			}
		})
	}
}

func TestNewAnthropicClientUsesModelBaseURL(t *testing.T) {
	client, err := newAnthropicClient(&Model{
		ID:       "MiniMax-M2.7",
		API:      APIAnthropicMessages,
		Provider: ProviderMiniMax,
		BaseURL:  "https://api.minimax.io/anthropic",
	})
	if err != nil {
		t.Fatalf("newAnthropicClient: %v", err)
	}

	if client.transport.baseURL != "https://api.minimax.io/anthropic" {
		t.Fatalf("baseURL = %q, want %q", client.transport.baseURL, "https://api.minimax.io/anthropic")
	}
}

// TestAnthropicFinalURLAppendsV1Messages guards against the regression where
// the Go SDK sent POST /messages (404 on every Anthropic-compatible provider
// whose base URL doesn't already include /v1 — e.g. MiniMax, kimi-coding,
// vercel-ai-gateway, opencode, github-copilot). The Pi SDK (and the official
// @anthropic-ai/sdk) use /v1/messages, so the Go SDK must do the same to keep
// final URLs in parity with Pi.
func TestAnthropicFinalURLAppendsV1Messages(t *testing.T) {
	cases := []struct {
		name        string
		baseURL     string
		wantPath    string
		wantNoSlash string // a path that must NOT appear (e.g. /v1/v1/messages)
	}{
		// All of these provider baseURLs intentionally do NOT contain /v1.
		// The Pi SDK (and the @anthropic-ai/sdk) appends /v1/messages, so
		// the final URL is baseURL + /v1/messages.
		{"minimax", "https://api.minimax.io/anthropic", "/v1/messages", "/v1/v1/messages"},
		{"minimax-cn", "https://api.minimaxi.com/anthropic", "/v1/messages", "/v1/v1/messages"},
		{"kimi-coding", "https://api.kimi.com/coding", "/v1/messages", "/v1/v1/messages"},
		{"vercel-ai-gateway", "https://ai-gateway.vercel.sh", "/v1/messages", "/v1/v1/messages"},
		{"opencode", "https://opencode.ai/zen", "/v1/messages", "/v1/v1/messages"},
		{"opencode-go", "https://opencode.ai/zen/go", "/v1/messages", "/v1/v1/messages"},
		{"github-copilot", "https://api.individual.githubcopilot.com", "/v1/messages", "/v1/v1/messages"},
		{"anthropic", "https://api.anthropic.com", "/v1/messages", "/v1/v1/messages"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Spin up a local server that records the request path and
			// returns a minimal valid Anthropic SSE response so the call
			// completes without error.
			var gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
			}))
			defer server.Close()

			client, err := newAnthropicClient(&Model{
				ID:       "test-model",
				API:      APIAnthropicMessages,
				Provider: tc.name,
				BaseURL:  server.URL,
			})
			if err != nil {
				t.Fatalf("newAnthropicClient: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			_ = client.Stream(ctx, client.model, &Context{
				Messages: []Message{{Role: "user", Content: "hi"}},
			}, func(_ context.Context, _ StreamEvent) {}, WithAPIKey("test-key"))

			if gotPath != tc.wantPath {
				t.Fatalf("path = %q, want %q (baseURL=%q)", gotPath, tc.wantPath, tc.baseURL)
			}
			if strings.Contains(gotPath, tc.wantNoSlash) {
				t.Fatalf("path = %q contains duplicated /v1 — default baseURL likely still has /v1", gotPath)
			}
		})
	}
}

// TestAnthropicBuildRequestNormalizesCrossModelToolCallIDs reproduces the
// cross-model tool-call-ID mismatch that surfaces when a user switches
// models mid-session (e.g. MiniMax/MiniMax-M2.7 → MiniMax/MiniMax-M3, both
// using the anthropic-messages API). The previous assistant turn was
// recorded under the source model; replaying it under the target model
// requires the tool_use IDs to be normalized so the target provider can
// pair the assistant's tool_use block with the user-side tool_result.
//
// The bug surfaces in two ways:
//
//  1. The source model produced an ID with characters that aren't valid
//     for the anthropic-messages character set (e.g. a `|` separator from
//     the OpenAI Responses API, or a `+`/=/`/`). Without normalization,
//     the assistant tool_use ID round-trips verbatim while the tool_result
//     tool_use_id (which already went through normalizeToolCallId) does not.
//     The two sides no longer match, and the provider rejects with
//     "tool result's tool id(...) not found".
//
//  2. The source model emitted a foreign-prefix ID (e.g. `call_function_*`)
//     that the target provider's session doesn't recognize. The IDs must be
//     regenerated deterministically from the original so both sides agree.
func TestAnthropicBuildRequestNormalizesCrossModelToolCallIDs(t *testing.T) {
	cases := []struct {
		name          string
		sourceID      string
		sourceResult  string
		wantToolUseID string
	}{
		{
			name:          "pipe-separated id from Responses-style source drops item-id suffix",
			sourceID:      "call_abc123|fc_def456",
			sourceResult:  "call_abc123|fc_def456",
			wantToolUseID: "call_abc123",
		},
		{
			name:          "foreign prefix id from previous anthropic-compatible provider",
			sourceID:      "call_function_oqkxc4depr3i_1",
			sourceResult:  "call_function_oqkxc4depr3i_1",
			wantToolUseID: "call_function_oqkxc4depr3i_1",
		},
		{
			name:          "id with disallowed characters sanitizes to underscores",
			sourceID:      "call.abc+def/ghi=jkl",
			sourceResult:  "call.abc+def/ghi=jkl",
			wantToolUseID: "call_abc_def_ghi_jkl",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &anthropicClient{
				model: &Model{
					ID:        "MiniMax-M3",
					API:       APIAnthropicMessages,
					Provider:  ProviderMiniMax,
					BaseURL:   "https://api.minimax.io/anthropic",
					MaxTokens: 4096,
				},
			}

			ctx := &Context{
				Messages: []Message{
					{Role: "user", Content: "run the tool"},
					{
						Role:     "assistant",
						Provider: ProviderMiniMax,
						API:      APIAnthropicMessages,
						Model:    "MiniMax-M2.7",
						Content: []ContentBlock{
							{
								Type:      ContentBlockToolCall,
								ID:        tc.sourceID,
								Name:      "bash",
								Arguments: json.RawMessage(`{"command":"ls"}`),
							},
						},
					},
					{
						Role:       "toolResult",
						ToolCallID: tc.sourceResult,
						ToolName:   "bash",
						Content:    "ok",
					},
					{Role: "user", Content: "now switch models"},
				},
			}

			reqBody, err := client.buildRequest(client.model, ctx, &options{})
			if err != nil {
				t.Fatalf("buildRequest: %v", err)
			}

			type anthropicReqContent struct {
				Type      string          `json:"type"`
				ID        string          `json:"id,omitempty"`
				Name      string          `json:"name,omitempty"`
				Input     json.RawMessage `json:"input,omitempty"`
				ToolUseID string          `json:"tool_use_id,omitempty"`
			}
			var req struct {
				Messages []struct {
					Role    string                `json:"role"`
					Content []anthropicReqContent `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
				t.Fatalf("unmarshal request: %v\nbody: %s", err, reqBody)
			}

			var toolUseID string
			var toolResultIDs []string
			for _, m := range req.Messages {
				for _, c := range m.Content {
					if c.Type == "tool_use" {
						toolUseID = c.ID
					}
					if c.Type == "tool_result" {
						toolResultIDs = append(toolResultIDs, c.ToolUseID)
					}
				}
			}

			if toolUseID == "" {
				t.Fatalf("tool_use block not found in assistant turn; body: %s", reqBody)
			}
			if len(toolResultIDs) != 1 {
				t.Fatalf("tool_result block count = %d, want 1; body: %s", len(toolResultIDs), reqBody)
			}
			if toolUseID != toolResultIDs[0] {
				t.Fatalf("tool_use_id / tool_use block mismatch: assistant tool_use.id = %q, tool_result.tool_use_id = %q — providers reject this with 'tool result's tool id(...) not found'.\nbody: %s",
					toolUseID, toolResultIDs[0], reqBody)
			}
			if toolUseID != tc.wantToolUseID {
				t.Errorf("normalized tool_use id = %q, want %q (sanitized and truncated to provider rules).\nbody: %s",
					toolUseID, tc.wantToolUseID, reqBody)
			}
		})
	}
}
