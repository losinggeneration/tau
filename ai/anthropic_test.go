package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	msgs, _, err := client.convertMessages(&Context{
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
	}, nil, nil)
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

// TestAnthropicBuildRequestHonorsMaxTokensOption pins the compaction bug where
// every anthropic-messages request sent model.MaxTokens (e.g. 64K), ignoring
// the WithMaxTokens option every other provider honors. A summarization
// request near the context window then asks for tens of thousands of output
// tokens on top of a near-full prompt, and providers reject the request.
func TestAnthropicBuildRequestHonorsMaxTokensOption(t *testing.T) {
	client := &anthropicClient{
		model: &Model{
			ID:        "claude-sonnet-4-6",
			API:       APIAnthropicMessages,
			Provider:  ProviderAnthropic,
			MaxTokens: 64000,
		},
	}

	var o options
	o.maxTokens = 8192

	reqBody, err := client.buildRequest(client.model, &Context{Messages: []Message{}}, &o)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req anthropicRequest
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	if req.MaxTokens != 8192 {
		t.Fatalf("max_tokens = %d, want 8192 (WithMaxTokens must override model.MaxTokens)", req.MaxTokens)
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

func TestAnthropicToolChoiceReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		choice     any
	}{
		{name: "omitted"},
		{name: "auto", choice: "auto", want: `{"type":"auto"}`},
		{name: "any", choice: "any", want: `{"type":"any"}`},
		{name: "none", choice: "none", want: `{"type":"none"}`},
		{name: "specific tool", choice: map[string]any{"type": "tool", "name": "lookup"}, want: `{"type":"tool","name":"lookup"}`},
		{name: "parallel disabled", choice: map[string]any{"type": "tool", "name": "lookup", "disable_parallel_tool_use": true}, want: `{"type":"tool","name":"lookup","disable_parallel_tool_use":true}`},
		{name: "parallel allowed", choice: map[string]any{"type": "tool", "name": "lookup", "disable_parallel_tool_use": false}, want: `{"type":"tool","name":"lookup","disable_parallel_tool_use":false}`},
	} {
		for _, key := range []string{"test-key"} {
			for _, format := range []string{"JSON", "SSE"} {
				t.Run(tc.name+"/"+key+"/"+format, func(t *testing.T) {
					model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
					prompt := &Context{Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}, Messages: []Message{{Role: "user", Content: "question"}}}
					before, err := json.Marshal(prompt)
					require.NoError(t, err)
					modelBefore, err := json.Marshal(model)
					require.NoError(t, err)
					choiceBefore, err := json.Marshal(tc.choice)
					require.NoError(t, err)
					var requests, callbacks, responses, closes, terminals int
					var payload string
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
						wire, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						assert.Equal(t, payload, string(wire))
						var body map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(wire, &body))
						if tc.want == "" || requests == 3 {
							assert.NotContains(t, body, "tool_choice")
						} else {
							assert.JSONEq(t, tc.want, string(body["tool_choice"]))
						}
						var tools []map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(body["tools"], &tools))
						require.Len(t, tools, 1)
						assert.JSONEq(t, `"lookup"`, string(tools[0]["name"]))
						assert.JSONEq(t, `"claude-test"`, string(body["model"]))
						assert.JSONEq(t, `true`, string(body["stream"]))
						response := `{"id":"msg_choice","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
						contentType := "application/json"
						if format == "SSE" {
							contentType = "text/event-stream"
							response = "data: " + `{"type":"message_start","message":{"id":"msg_choice","usage":{"input_tokens":3}}}` + "\n\n" +
								"data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"answer"}}` + "\n\n" +
								"data: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
								"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
								"data: " + `{"type":"message_stop"}` + "\n\n"
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(response)), closes: &closes}}, nil
					})}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					opts := []Option{WithClient(client), WithAPIKey(key), WithPayloadCallback(func(value any) any { callbacks++; payload = value.(string); return nil }), WithResponseCallback(func(response ProviderResponse) { responses++; assert.Equal(t, http.StatusOK, response.Status) })}
					selected := append([]Option{}, opts...)
					if tc.choice != nil {
						selected = append(selected, WithToolChoice(tc.choice))
					}
					check := func(message AssistantMessage) {
						t.Helper()
						assert.Equal(t, StopReasonStop, message.StopReason)
						assert.Equal(t, "msg_choice", message.ResponseID)
						assert.Equal(t, model.ID, message.Model)
						assert.Equal(t, model.API, message.API)
						assert.Equal(t, model.Provider, message.Provider)
						assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
						assert.Equal(t, 3, message.Usage.Input)
						assert.Equal(t, 2, message.Usage.Output)
						assert.Equal(t, 5, message.Usage.TotalTokens)
						assert.Empty(t, message.ErrorMessage)
						assert.Empty(t, message.Diagnostics)
						assert.NotNil(t, message.DurationMs)
					}
					err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
						switch e := event.(type) {
						case Done:
							terminals++
							assert.Equal(t, StopReasonStop, e.Reason)
							check(e.Message)
						case StreamError:
							t.Fatalf("unexpected error terminal: %v", e.Err)
						}
					}, selected...)
					require.NoError(t, err)
					assert.Equal(t, 1, terminals)
					for _, options := range [][]Option{selected, opts} {
						message, err := CompleteWithModel(t.Context(), model, prompt, options...)
						require.NoError(t, err)
						require.NotNil(t, message)
						check(*message)
					}
					assert.Equal(t, []int{3, 3, 3, 3}, []int{requests, callbacks, responses, closes})
					after, err := json.Marshal(prompt)
					require.NoError(t, err)
					assert.Equal(t, string(before), string(after))
					modelAfter, err := json.Marshal(model)
					require.NoError(t, err)
					assert.Equal(t, string(modelBefore), string(modelAfter))
					choiceAfter, err := json.Marshal(tc.choice)
					require.NoError(t, err)
					assert.Equal(t, string(choiceBefore), string(choiceAfter))
				})
			}
		}
	}
}

func TestAnthropicThinkingDisplayReachesTransport(t *testing.T) {
	for _, mode := range []string{"budget", "adaptive", "managed", "disabled", "inactive"} {
		for _, display := range []string{"default", "summarized", "omitted", "last option wins"} {
			for _, format := range []string{"JSON", "SSE"} {
				t.Run(mode+"/"+display+"/"+format, func(t *testing.T) {
					model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", Reasoning: true, MaxTokens: 8192}
					if mode == "adaptive" {
						model.Compat = &CompatibilitySettings{ForceAdaptiveThinking: true}
					} else if mode == "managed" {
						model.Compat = &CompatibilitySettings{SupportsMidConvoEffort: true}
					}
					prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
					before, err := json.Marshal([]any{model, prompt})
					require.NoError(t, err)
					active := mode != "disabled" && mode != "inactive"
					var requests, callbacks, responses, closes, terminals, thinkingEnds int
					var payload string
					var expectedReplay string
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
						wire, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						assert.Equal(t, payload, string(wire))
						var body map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(wire, &body))
						wantDisplay := "summarized"
						if requests < 3 && (display == "omitted" || display == "last option wins") {
							wantDisplay = "omitted"
						}
						switch mode {
						case "budget":
							assert.JSONEq(t, fmt.Sprintf(`{"type":"enabled","budget_tokens":1024,"display":%q}`, wantDisplay), string(body["thinking"]))
						case "adaptive":
							assert.JSONEq(t, fmt.Sprintf(`{"type":"adaptive","display":%q}`, wantDisplay), string(body["thinking"]))
						case "managed":
							assert.JSONEq(t, fmt.Sprintf(`{"type":"adaptive","display":%q,"block_binding":{"prefix_mismatch_behavior":"drop_block"}}`, wantDisplay), string(body["thinking"]))
						case "disabled":
							assert.JSONEq(t, `{"type":"disabled"}`, string(body["thinking"]))
						case "inactive":
							assert.NotContains(t, body, "thinking")
						}
						assert.JSONEq(t, `"claude-test"`, string(body["model"]))
						assert.Equal(t, "true", string(body["stream"]))
						wantMax := "4096"
						if mode == "budget" {
							wantMax = "5120"
						}
						assert.Equal(t, wantMax, string(body["max_tokens"]))
						if active {
							assert.NotContains(t, body, "temperature")
						} else {
							assert.Equal(t, "0.4", string(body["temperature"]))
						}
						if mode == "managed" {
							assert.JSONEq(t, `{"effort":"high"}`, string(body["output_config"]))
						} else {
							assert.NotContains(t, body, "output_config")
						}
						expected := `[{"role":"user","content":[{"type":"text","text":"question"}]}]`
						if mode == "managed" {
							expected = `[{"role":"user","content":[{"type":"text","text":"question"}]},{"role":"system","content":[],"output_config":{"effort":"high"}}]`
						}
						if requests == 2 {
							expected = expectedReplay
						}
						assert.JSONEq(t, expected, string(body["messages"]))
						blocks := `[{"type":"text","text":"answer"}]`
						if active {
							blocks = `[{"type":"thinking","thinking":"","signature":"opaque-signature"},{"type":"text","text":"answer"}]`
						}
						response := `{"id":"msg_display","content":` + blocks + `,"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
						kind := "application/json"
						if format == "SSE" {
							kind = "text/event-stream"
							response = "data: " + `{"type":"message_start","message":{"id":"msg_display","usage":{"input_tokens":3}}}` + "\n\n"
							if active {
								response += "data: " + `{"type":"content_block_start","index":3,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
									"data: " + `{"type":"content_block_delta","index":3,"delta":{"type":"signature_delta","signature":"opaque-signature"}}` + "\n\n" +
									"data: " + `{"type":"content_block_stop","index":3}` + "\n\n"
							}
							response += "data: " + `{"type":"content_block_start","index":5,"content_block":{"type":"text","text":"answer"}}` + "\n\n" +
								"data: " + `{"type":"content_block_stop","index":5}` + "\n\n" +
								"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
								"data: " + `{"type":"message_stop"}` + "\n\n"
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {kind}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(response)), closes: &closes}}, nil
					})}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxTokens(4096), WithTemperature(0.4), WithPayloadCallback(func(value any) any { callbacks++; payload = value.(string); return nil }), WithResponseCallback(func(response ProviderResponse) { responses++; assert.Equal(t, http.StatusOK, response.Status) })}
					if active {
						opts = append(opts, WithThinkingEnabled(true))
					} else if mode == "disabled" {
						opts = append(opts, WithThinkingEnabled(false))
					}
					selected := append([]Option{}, opts...)
					switch display {
					case "summarized", "omitted":
						selected = append(selected, WithThinkingDisplay(display))
					case "last option wins":
						selected = append(selected, WithThinkingDisplay("summarized"), WithThinkingDisplay("omitted"))
					}
					check := func(message AssistantMessage) {
						t.Helper()
						want := []ContentBlock{{Type: ContentBlockText, Text: "answer"}}
						if active {
							want = append([]ContentBlock{{Type: ContentBlockThinking, ThoughtSignature: "opaque-signature"}}, want...)
						}
						assert.Equal(t, want, message.Content)
						assert.Equal(t, StopReasonStop, message.StopReason)
						assert.Equal(t, "msg_display", message.ResponseID)
						assert.Equal(t, model.ID, message.Model)
						assert.Equal(t, model.API, message.API)
						assert.Equal(t, model.Provider, message.Provider)
						assert.Equal(t, 3, message.Usage.Input)
						assert.Equal(t, 2, message.Usage.Output)
						assert.Equal(t, 5, message.Usage.TotalTokens)
						assert.Empty(t, message.ErrorMessage)
						assert.Empty(t, message.Diagnostics)
					}
					var first AssistantMessage
					err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
						switch e := event.(type) {
						case ThinkingEnd:
							thinkingEnds++
							assert.Empty(t, e.Content)
							assert.Equal(t, "opaque-signature", e.Partial.Content[0].ThoughtSignature)
						case Done:
							terminals++
							first = e.Message
							check(first)
						case StreamError:
							t.Errorf("unexpected error terminal: %v", e.Err)
						}
					}, selected...)
					require.NoError(t, err)
					assert.Equal(t, 1, terminals)
					wantThinkingEnds := 0
					if active && format == "SSE" {
						wantThinkingEnds = 1
					}
					assert.Equal(t, wantThinkingEnds, thinkingEnds)
					encoded, err := json.Marshal(first)
					require.NoError(t, err)
					var history Message
					require.NoError(t, json.Unmarshal(encoded, &history))
					replay := &Context{Messages: []Message{prompt.Messages[0], history, {Role: "user", Content: "next"}}}
					replayBefore, err := json.Marshal(replay)
					require.NoError(t, err)
					thinking := ""
					if active {
						thinking = `{"type":"thinking","thinking":"","signature":"opaque-signature"},`
					}
					expectedReplay = `[{"role":"user","content":[{"type":"text","text":"question"}]},`
					if mode == "managed" {
						expectedReplay += `{"role":"system","content":[],"output_config":{"effort":"high"}},`
					}
					expectedReplay += `{"role":"assistant","content":[` + thinking + `{"type":"text","text":"answer"}]},{"role":"user","content":[{"type":"text","text":"next"}]}`
					if mode == "managed" {
						expectedReplay += `,{"role":"system","content":[],"output_config":{"effort":"high"}}`
					}
					expectedReplay += `]`
					second, err := CompleteWithModel(t.Context(), model, replay, selected...)
					require.NoError(t, err)
					require.NotNil(t, second)
					check(*second)
					third, err := CompleteWithModel(t.Context(), model, prompt, opts...)
					require.NoError(t, err)
					require.NotNil(t, third)
					check(*third)
					assert.Equal(t, []int{3, 3, 3, 3}, []int{requests, callbacks, responses, closes})
					after, err := json.Marshal([]any{model, prompt})
					require.NoError(t, err)
					assert.Equal(t, string(before), string(after))
					replayAfter, err := json.Marshal(replay)
					require.NoError(t, err)
					assert.Equal(t, string(replayBefore), string(replayAfter))
					firstAfter, err := json.Marshal(first)
					require.NoError(t, err)
					assert.Equal(t, string(encoded), string(firstAfter))
				})
			}
		}
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

	reqBody, err := client.buildRequest(client.model, ctx, &options{cacheRetention: "short"})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req anthropicRequest
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	require.Len(t, req.Tools, 1)
	require.Equal(t, &cacheControlParam{Type: "ephemeral"}, req.Tools[0].CacheControl)
}

func TestAnthropicBuildRequestWithAdaptiveThinking(t *testing.T) {
	client := &anthropicClient{
		model: &Model{
			ID:        "claude-opus-4-7-20250514",
			Compat:    &CompatibilitySettings{ForceAdaptiveThinking: true},
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
					MaxTokens: 32000,
				},
			}

			if tt.wantType == "adaptive" {
				client.model.Compat = &CompatibilitySettings{ForceAdaptiveThinking: true}
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
				var effort string
				if req.OutputConfig != nil {
					effort = req.OutputConfig.Effort
				}
				if effort != tt.wantEffort {
					t.Errorf("output_config.effort = %q, want %q", effort, tt.wantEffort)
				}
				if tt.wantBudget > 0 && req.Thinking.BudgetTokens != tt.wantBudget {
					t.Errorf("thinking.budget_tokens = %d, want %d", req.Thinking.BudgetTokens, tt.wantBudget)
				}
			} else if req.Thinking != nil && req.Thinking.Type != "disabled" {
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
		provider string
		baseURL  string
		wantURL  string
	}{
		{"minimax", "https://api.minimax.io/anthropic", "https://api.minimax.io/anthropic/v1/messages"},
		{"minimax-cn", "https://api.minimaxi.com/anthropic", "https://api.minimaxi.com/anthropic/v1/messages"},
		{"kimi-coding", "https://api.kimi.com/coding", "https://api.kimi.com/coding/v1/messages"},
		{"vercel-ai-gateway", "https://ai-gateway.vercel.sh", "https://ai-gateway.vercel.sh/v1/messages"},
		{"opencode", "https://opencode.ai/zen", "https://opencode.ai/zen/v1/messages"},
		{"opencode-go", "https://opencode.ai/zen/go", "https://opencode.ai/zen/go/v1/messages"},
		{ProviderGitHubCopilot, "https://api.individual.githubcopilot.com", "https://api.individual.githubcopilot.com/v1/messages"},
		{ProviderAnthropic, "https://api.anthropic.com", "https://api.anthropic.com/v1/messages"},
		{"gateway-trailing-slash", "https://test.invalid/gateway/anthropic/", "https://test.invalid/gateway/anthropic/v1/messages"},
		{ProviderAnthropic, "", "https://api.anthropic.com/v1/messages"},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.baseURL, func(t *testing.T) {
			model := &Model{ID: "claude-endpoint", API: APIAnthropicMessages, Provider: tc.provider, BaseURL: tc.baseURL, MaxTokens: 4096}
			prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
			before, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			var requests, payloads, responses, closes, terminals int
			var payload string
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				assert.Equal(t, requests-1, closes)
				assert.Equal(t, http.MethodPost, req.Method)
				assert.Equal(t, tc.wantURL, req.URL.String())
				assert.Empty(t, req.URL.RawQuery)
				assert.Empty(t, req.URL.Fragment)
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, payload, string(wire))
				var request struct{ Messages json.RawMessage }
				require.NoError(t, json.Unmarshal(wire, &request))
				assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(request.Messages))
				body := "data: " + `{"type":"message_start","message":{"id":"msg_endpoint","usage":{"input_tokens":11,"output_tokens":7}}}` + "\n\n" +
					"data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"answer"}}` + "\n\n" +
					"data: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
					"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
					"data: " + `{"type":"message_stop"}` + "\n\n"
				kind := "text/event-stream"
				if requests == 2 {
					kind = "application/json"
					body = `{"id":"msg_endpoint","content":[{"type":"text","text":"answer"}],"usage":{"input_tokens":11,"output_tokens":7},"stop_reason":"end_turn"}`
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {kind}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(body)), closes: &closes}}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(value any) any {
				payloads++
				payload = value.(string)
				return nil
			}), WithResponseCallback(func(response ProviderResponse) {
				responses++
				assert.Equal(t, requests-1, closes)
				assert.Equal(t, http.StatusOK, response.Status)
			})}
			check := func(message AssistantMessage) {
				assert.Equal(t, "msg_endpoint", message.ResponseID)
				assert.Equal(t, model.ID, message.Model)
				assert.Equal(t, model.API, message.API)
				assert.Equal(t, model.Provider, message.Provider)
				assert.Equal(t, StopReasonStop, message.StopReason)
				assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
				assert.Equal(t, 11, message.Usage.Input)
				assert.Equal(t, 7, message.Usage.Output)
			}
			err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
				assert.Equal(t, 1, responses)
				assert.Zero(t, closes)
				switch e := event.(type) {
				case Done:
					terminals++
					check(e.Message)
				case StreamError:
					terminals++
					t.Errorf("unexpected terminal: %v", e.Err)
				}
			}, opts...)
			require.NoError(t, err)
			assert.Equal(t, 1, terminals)
			message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
			require.NoError(t, err)
			require.NotNil(t, message)
			check(*message)
			assert.Equal(t, []int{2, 2, 2, 2}, []int{requests, payloads, responses, closes})
			after, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
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

func TestAnthropicToolCallIDReplayReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name, id, same, cross string
	}{
		{name: "valid boundary", id: strings.Repeat("a", 64), same: strings.Repeat("a", 64), cross: strings.Repeat("a", 64)},
		{name: "over boundary", id: strings.Repeat("b", 65), same: strings.Repeat("b", 64), cross: strings.Repeat("b", 64)},
		{name: "punctuation", id: "call.abc+def/ghi=jkl", same: "call_abc_def_ghi_jkl", cross: "call_abc_def_ghi_jkl"},
		{name: "BMP characters", id: "call_é中_tail", same: "call____tail", cross: "call____tail"},
		{name: "composite", id: "call_abc|fc_def", same: "call_abc_fc_def", cross: "call_abc"},
	} {
		for _, mode := range []string{"same model", "cross model"} {
			for _, format := range []string{"JSON", "SSE"} {
				t.Run(tc.name+"/"+mode+"/"+format, func(t *testing.T) {
					model := &Model{ID: "source", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
					target := *model
					wantID := tc.same
					if mode == "cross model" {
						target.ID = "target"
						wantID = tc.cross
					}
					prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
					args := json.RawMessage(`{"n":9007199254740993}`)
					blocks := []ContentBlock{{Type: ContentBlockToolCall, ID: tc.id, Name: "lookup", Arguments: args}, {Type: ContentBlockToolCall, ID: "missing.id", Name: "lookup", Arguments: json.RawMessage(`{}`)}}
					wireBlocks := []map[string]any{{"type": "tool_use", "id": tc.id, "name": "lookup", "input": args}, {"type": "tool_use", "id": "missing.id", "name": "lookup", "input": json.RawMessage(`{}`)}}
					response, err := json.Marshal(map[string]any{"id": "msg_ids", "content": wireBlocks, "stop_reason": "tool_use", "usage": map[string]int{"input_tokens": 3, "output_tokens": 2}})
					require.NoError(t, err)
					kind := "application/json"
					if format == "SSE" {
						kind = "text/event-stream"
						var stream strings.Builder
						emit := func(event any) {
							data, err := json.Marshal(event)
							require.NoError(t, err)
							stream.WriteString("data: " + string(data) + "\n\n")
						}
						emit(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_ids", "usage": map[string]int{"input_tokens": 3}}})
						for i, block := range wireBlocks {
							emit(map[string]any{"type": "content_block_start", "index": i, "content_block": block})
							emit(map[string]any{"type": "content_block_stop", "index": i})
						}
						emit(map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}, "usage": map[string]int{"output_tokens": 2}})
						emit(map[string]string{"type": "message_stop"})
						response = []byte(stream.String())
					}
					var requests, callbacks, responses, closes, terminals int
					var payload string
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
						data, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						assert.Equal(t, payload, string(data))
						var body struct {
							Model    string
							Stream   bool
							Messages json.RawMessage
						}
						require.NoError(t, json.Unmarshal(data, &body))
						assert.True(t, body.Stream)
						responseKind, responseData := kind, string(response)
						if requests == 1 {
							assert.Equal(t, model.ID, body.Model)
							assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(body.Messages))
						} else {
							assert.Equal(t, target.ID, body.Model)
							assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]},{"role":"assistant","content":[{"type":"tool_use","id":"`+wantID+`","name":"lookup","input":`+string(args)+`},{"type":"tool_use","id":"missing_id","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+wantID+`","content":"actual result"},{"type":"tool_result","tool_use_id":"missing_id","content":"No result provided","is_error":true}]}]`, string(body.Messages))
							assert.Contains(t, string(body.Messages), string(args))
							responseKind, responseData = "application/json", `{"id":"msg_replayed","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {responseKind}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(responseData)), closes: &closes}}, nil
					})}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(value any) any { callbacks++; payload = value.(string); return nil }), WithResponseCallback(func(response ProviderResponse) { responses++; assert.Equal(t, http.StatusOK, response.Status) })}
					before, err := json.Marshal(prompt)
					require.NoError(t, err)
					var returned AssistantMessage
					err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
						switch e := event.(type) {
						case Done:
							terminals++
							assert.Equal(t, StopReasonToolUse, e.Reason)
							returned = e.Message
						case StreamError:
							t.Fatalf("unexpected terminal: %v", e.Err)
						}
					}, append(opts, WithClient(client))...)
					require.NoError(t, err)
					assert.Equal(t, 1, terminals)
					assert.Equal(t, blocks, returned.Content)
					assert.Equal(t, StopReasonToolUse, returned.StopReason)
					assert.Equal(t, "msg_ids", returned.ResponseID)
					assert.Equal(t, model.ID, returned.Model)
					assert.Equal(t, model.Provider, returned.Provider)
					assert.Equal(t, model.API, returned.API)
					assert.Equal(t, 3, returned.Usage.Input)
					assert.Equal(t, 2, returned.Usage.Output)
					assert.NotNil(t, returned.DurationMs)
					assert.Empty(t, returned.Diagnostics)
					after, err := json.Marshal(prompt)
					require.NoError(t, err)
					assert.Equal(t, string(before), string(after))
					encoded, err := json.Marshal(returned)
					require.NoError(t, err)
					var restored Message
					require.NoError(t, json.Unmarshal(encoded, &restored))
					prompt.Messages = append(prompt.Messages, restored, Message{Role: "toolResult", ToolCallID: tc.id, ToolName: "lookup", Content: "actual result"})
					before, err = json.Marshal(prompt)
					require.NoError(t, err)
					targetBefore, err := json.Marshal(target)
					require.NoError(t, err)
					targetClient, err := newAnthropicClient(&target)
					require.NoError(t, err)
					result, err := CompleteWithModel(t.Context(), &target, prompt, append(opts, WithClient(targetClient))...)
					require.NoError(t, err)
					require.NotNil(t, result)
					assert.Equal(t, StopReasonStop, result.StopReason)
					assert.Equal(t, "msg_replayed", result.ResponseID)
					assert.Equal(t, target.ID, result.Model)
					assert.Equal(t, target.Provider, result.Provider)
					assert.Equal(t, target.API, result.API)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, result.Content)
					assert.Equal(t, 3, result.Usage.Input)
					assert.Equal(t, 2, result.Usage.Output)
					assert.Empty(t, result.ErrorMessage)
					assert.Empty(t, result.Diagnostics)
					assert.NotNil(t, result.DurationMs)
					assert.Equal(t, []int{2, 2, 2, 2}, []int{requests, callbacks, responses, closes})
					after, err = json.Marshal(prompt)
					require.NoError(t, err)
					assert.Equal(t, string(before), string(after))
					targetAfter, err := json.Marshal(target)
					require.NoError(t, err)
					assert.Equal(t, string(targetBefore), string(targetAfter))
					returnedAfter, err := json.Marshal(returned)
					require.NoError(t, err)
					assert.Equal(t, string(encoded), string(returnedAfter))
				})
			}
		}
	}
}

func TestAnthropic55UsesAdaptiveNativeEffort(t *testing.T) {
	for _, id := range []string{"claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5", "claude-haiku-5.5"} {
		if !isAdaptiveThinkingModel(id) {
			t.Errorf("%s lacks adaptive thinking", id)
		}
		if effort := mapReasoningLevelToAnthropicEffort(ReasoningXHigh, id); effort != "xhigh" {
			t.Errorf("%s xhigh=%s", id, effort)
		}
	}
}

func TestAnthropicStrictToolPreference(t *testing.T) {
	for _, mode := range []string{"prefer", "require"} {
		for _, supported := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", mode, supported), func(t *testing.T) {
				var prompt Context
				if err := json.Unmarshal([]byte(`{"tools":[{"name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"}}},"constrainedSampling":{"type":"json_schema","strict":"`+mode+`"}}]}`), &prompt); err != nil {
					t.Fatal(err)
				}
				model := &Model{ID: "claude-test", Compat: &CompatibilitySettings{}}
				metadata, err := json.Marshal(map[string]any{"supportsStrictTools": supported})
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(metadata, model.Compat); err != nil {
					t.Fatal(err)
				}
				client := &anthropicClient{}
				body, err := client.buildRequest(model, &prompt, &options{})
				if !supported && mode == "require" {
					if err == nil {
						t.Fatal("required strict accepted without support")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var request struct {
					Tools []struct {
						Strict bool           `json:"strict"`
						Schema map[string]any `json:"input_schema"`
					} `json:"tools"`
				}
				if err := json.Unmarshal([]byte(body), &request); err != nil {
					t.Fatal(err)
				}
				if request.Tools[0].Strict != supported {
					t.Fatalf("strict=%t, want %t", request.Tools[0].Strict, supported)
				}
				if supported && request.Tools[0].Schema["additionalProperties"] != false {
					t.Fatalf("schema=%v", request.Tools[0].Schema)
				}
			})
		}
	}
}

func TestAnthropicStrictSchemaAdmissionReachesTransport(t *testing.T) {
	for _, schema := range []struct {
		name, parameters, converted, keyword string
	}{
		{
			name: "numeric bounds", keyword: "minimum",
			parameters: `{"type":"object","properties":{"timeout":{"type":"integer","minimum":1}}}`,
		},
		{
			name: "nested array minimum", keyword: "minItems",
			parameters: `{"type":"object","properties":{"options":{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"},"minItems":2}},"required":["tags"]}},"required":["options"]}`,
		},
		{
			name: "rejected format", keyword: "format",
			parameters: `{"type":"object","properties":{"expression":{"type":"string","format":"regex"}},"required":["expression"]}`,
		},
		{
			name:       "allowed constraints",
			parameters: `{"type":"object","properties":{"code":{"type":"string","minLength":1,"maxLength":1000,"pattern":"^[a-z]+$"},"url":{"type":"string","format":"uri"},"tags":{"type":"array","items":{"type":"string"},"minItems":1},"empty":{"type":"array","items":{"type":"string"},"minItems":0}},"required":["code","tags","empty"]}`,
			converted:  `{"type":"object","properties":{"code":{"type":"string","minLength":1,"maxLength":1000,"pattern":"^[a-z]+$"},"url":{"anyOf":[{"type":"string","format":"uri"},{"type":"null"}]},"tags":{"type":"array","items":{"type":"string"},"minItems":1},"empty":{"type":"array","items":{"type":"string"},"minItems":0}},"required":["code","empty","tags","url"],"additionalProperties":false}`,
		},
	} {
		for _, placement := range []string{"initial", "inline"} {
			for _, preference := range []string{"prefer", "require"} {
				t.Run(schema.name+"/"+placement+"/"+preference, func(t *testing.T) {
					model := &Model{ID: "claude-schema", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Compat: &CompatibilitySettings{SupportsStrictTools: true, SupportsMidConvoSystemMessages: true, SupportsMidConvoToolChanges: placement == "inline"}}
					tool := Tool{Name: "read", Description: "Read a value", Parameters: json.RawMessage(schema.parameters), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"` + preference + `"}`)}
					prompt := &Context{Tools: []Tool{tool}, Messages: []Message{{Role: "user", Content: "question"}}}
					if placement == "inline" {
						prompt = &Context{Messages: []Message{
							{Role: "system", ToolsAdded: []Tool{{Name: "anchor", Parameters: json.RawMessage(`{"type":"object"}`)}}},
							{Role: "user", Content: "question"},
							{Role: "system", ToolsAdded: []Tool{tool}},
						}}
					}
					before, err := json.Marshal([]any{model, prompt})
					require.NoError(t, err)
					var requests, callbacks, responses, closes, terminals int
					var payload string
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						wire, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						assert.Equal(t, payload, string(wire))
						assert.Equal(t, "test-key", req.Header.Get("x-api-key"))
						var body map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(wire, &body))
						parameters, strict := schema.parameters, ""
						if schema.keyword == "" {
							parameters, strict = schema.converted, `,"strict":true`
						}
						definition := `{"name":"read","description":"Read a value","eager_input_streaming":true,"input_schema":` + parameters + strict + `}`
						messages := `[{"role":"user","content":[{"type":"text","text":"question"}]}]`
						tools := "[" + definition + "]"
						if placement == "inline" {
							tools = `[{"name":"anchor","description":"","input_schema":{"type":"object"},"eager_input_streaming":true},{"name":"__pi_deferred_placeholder__","description":"Reserved placeholder. Never available. Never call this.","input_schema":{"type":"object","properties":{},"required":[]},"defer_loading":true}]`
							messages = `[{"role":"user","content":[{"type":"text","text":"question"}]},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":` + definition + `}}]}]`
							assert.Contains(t, req.Header.Get("anthropic-beta"), anthropicInlineToolsBeta)
						}
						assert.JSONEq(t, tools, string(body["tools"]))
						assert.JSONEq(t, messages, string(body["messages"]))
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(`{"id":"msg_schema","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)), closes: &closes}}, nil
					})}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxRetries(0), WithPayloadCallback(func(value any) any { callbacks++; payload = value.(string); return nil }), WithResponseCallback(func(response ProviderResponse) { responses++; assert.Equal(t, http.StatusOK, response.Status) })}
					blocked := schema.keyword != "" && preference == "require"
					check := func(message AssistantMessage) {
						t.Helper()
						assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
						assert.Equal(t, StopReasonStop, message.StopReason)
						assert.Equal(t, "msg_schema", message.ResponseID)
						assert.Equal(t, model.ID, message.Model)
						assert.Equal(t, model.API, message.API)
						assert.Equal(t, model.Provider, message.Provider)
						assert.Equal(t, 5, message.Usage.TotalTokens)
						assert.Empty(t, message.Diagnostics)
					}
					err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
						switch e := event.(type) {
						case Done:
							terminals++
							check(e.Message)
						case StreamError:
							t.Errorf("unexpected terminal: %v", e.Err)
						}
					}, opts...)
					if blocked {
						require.ErrorContains(t, err, `tool "read" requires JSON-schema constrained sampling`)
						require.ErrorContains(t, err, "unsupported schema keyword "+schema.keyword)
					} else {
						require.NoError(t, err)
					}
					message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
					if blocked {
						require.ErrorContains(t, err, "unsupported schema keyword "+schema.keyword)
						assert.Nil(t, message)
						assert.Equal(t, []int{0, 0, 0, 0, 0}, []int{requests, callbacks, responses, closes, terminals})
					} else {
						require.NoError(t, err)
						require.NotNil(t, message)
						check(*message)
						assert.Equal(t, []int{2, 2, 2, 2, 1}, []int{requests, callbacks, responses, closes, terminals})
					}
					after, err := json.Marshal([]any{model, prompt})
					require.NoError(t, err)
					assert.Equal(t, string(before), string(after))
					assert.Empty(t, client.tools)
				})
			}
		}
	}
}

func TestAnthropicPayloadReplacementSurvivesRetries(t *testing.T) {
	for _, mode := range []string{"replace", "observe", "non-string"} {
		t.Run(mode, func(t *testing.T) {
			model := &Model{ID: "claude-payload", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Compat: &CompatibilitySettings{SupportsStrictTools: true}}
			prompt := &Context{Tools: []Tool{
				{Name: "preferred", Description: "Look up a value", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"},"optional":{"type":"number"}},"required":["value"]}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)},
				{Name: "required", Parameters: json.RawMessage(`{"type":"object"}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"require"}`)},
			}}
			prompt.Messages = []Message{{Role: "user", Content: "question"}}
			before, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			var requests, callbacks, responses, closes, terminals int
			var wirePayload string
			var builtPayloads, wirePayloads, order []string
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				order = append(order, "send")
				assert.Equal(t, requests-1, closes)
				assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
				assert.Equal(t, "test-key", req.Header.Get("x-api-key"))
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, wirePayload, string(wire))
				wirePayloads = append(wirePayloads, string(wire))
				var body map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(wire, &body))
				assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(body["messages"]))
				preferred := `{"name":"preferred","description":"Look up a value","input_schema":{"type":"object","properties":{"value":{"type":"string"},"optional":{"anyOf":[{"type":"number"},{"type":"null"}]}},"required":["optional","value"],"additionalProperties":false},"strict":true,"eager_input_streaming":true}`
				if requests == 3 || requests == 4 {
					preferred = `{"name":"preferred","description":"Look up a value","input_schema":{"type":"object","properties":{"value":{"type":"string"},"optional":{"type":"number"}},"required":["value"]},"eager_input_streaming":true}`
				}
				assert.JSONEq(t, "["+preferred+`,{"name":"required","description":"","input_schema":{"type":"object","required":[],"additionalProperties":false},"strict":true,"eager_input_streaming":true}]`, string(body["tools"]))
				if mode == "replace" && requests < 5 {
					assert.Equal(t, "false", string(body["stream"]))
					assert.Equal(t, `"us"`, string(body["inference_geo"]))
				} else {
					assert.Equal(t, "true", string(body["stream"]))
					assert.NotContains(t, body, "inference_geo")
				}
				status := http.StatusOK
				response := `{"id":"msg_payload","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
				switch requests {
				case 1, 3:
					status = http.StatusServiceUnavailable
					response = `{"error":{"type":"overloaded_error","message":"busy"}}`
				case 2:
					status = http.StatusBadRequest
					response = `{"error":{"type":"invalid_request_error","message":"The compiled grammar is too large"}}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After-Ms": {"0"}, "X-Request-Id": {"accepted"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(response)), closes: &closes}}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxRetries(1), WithPayloadCallbackForModel(func(value any, got *Model) any {
				callbacks++
				order = append(order, "payload")
				assert.Same(t, model, got)
				built := value.(string)
				builtPayloads = append(builtPayloads, built)
				wirePayload = built
				var body map[string]json.RawMessage
				require.NoError(t, json.Unmarshal([]byte(built), &body))
				assert.Equal(t, "true", string(body["stream"]))
				assert.NotContains(t, body, "inference_geo")
				assert.Equal(t, []int{0, 2, 4}[callbacks-1], closes)
				if callbacks == 3 || mode == "observe" {
					return nil
				}
				if mode == "non-string" {
					return map[string]any{"stream": false, "inference_geo": "ignored"}
				}
				body["stream"] = json.RawMessage(`false`)
				body["inference_geo"] = json.RawMessage(`"us"`)
				replacement, err := json.Marshal(body)
				require.NoError(t, err)
				wirePayload = string(replacement)
				return wirePayload
			}), WithResponseCallbackForModel(func(response ProviderResponse, got *Model) {
				responses++
				order = append(order, "response")
				assert.Same(t, model, got)
				assert.Equal(t, http.StatusOK, response.Status)
				assert.Equal(t, "accepted", response.Headers["X-Request-Id"])
				assert.Equal(t, requests-1, closes)
				assert.Zero(t, terminals)
			})}
			check := func(message AssistantMessage) {
				t.Helper()
				assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
				assert.Equal(t, StopReasonStop, message.StopReason)
				assert.Equal(t, "msg_payload", message.ResponseID)
				assert.Equal(t, model.ID, message.Model)
				assert.Equal(t, model.API, message.API)
				assert.Equal(t, model.Provider, message.Provider)
				assert.Equal(t, 5, message.Usage.TotalTokens)
			}
			err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
				order = append(order, "event")
				done, ok := event.(Done)
				require.True(t, ok, "unexpected event %T", event)
				terminals++
				assert.Equal(t, 1, responses)
				assert.Equal(t, 3, closes)
				check(done.Message)
				require.Len(t, done.Message.Diagnostics, 1)
				assert.Equal(t, "anthropic_strict_tools_fallback", done.Message.Diagnostics[0].Type)
				assert.Equal(t, map[string]string{"reason": "strict_tool_limits_exceeded"}, done.Message.Diagnostics[0].Details)
			}, opts...)
			require.NoError(t, err)
			assert.Equal(t, []int{4, 2, 1, 4, 1}, []int{requests, callbacks, responses, closes, terminals})
			assert.Equal(t, []string{"payload", "send", "send", "payload", "send", "send", "response", "event"}, order)
			terminals = 0
			message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
			require.NoError(t, err)
			require.NotNil(t, message)
			check(*message)
			assert.Empty(t, message.Diagnostics)
			assert.Equal(t, []int{5, 3, 2, 5}, []int{requests, callbacks, responses, closes})
			require.Len(t, builtPayloads, 3)
			require.Len(t, wirePayloads, 5)
			assert.Equal(t, builtPayloads[0], builtPayloads[2])
			assert.NotEqual(t, builtPayloads[0], builtPayloads[1])
			assert.Equal(t, wirePayloads[0], wirePayloads[1])
			assert.Equal(t, wirePayloads[2], wirePayloads[3])
			assert.Equal(t, builtPayloads[2], wirePayloads[4])
			after, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
			assert.Empty(t, client.tools)
		})
	}
}

func TestAnthropicPreferredStrictFallsBackOncePerRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			Tools []struct {
				Strict bool `json:"strict"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		wantPreferred := calls%2 == 1
		if len(request.Tools) != 2 || request.Tools[0].Strict != wantPreferred || !request.Tools[1].Strict {
			t.Errorf("call %d: tools=%+v", calls, request.Tools)
		}
		w.Header().Set("Content-Type", "application/json")
		if wantPreferred {
			w.WriteHeader(http.StatusBadRequest)
			if _, err := io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Too many strict tools (21)"}}`); err != nil {
				t.Error(err)
			}
			return
		}
		if _, err := io.WriteString(w, `{"id":"answer","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: server.URL}
	if err := json.Unmarshal([]byte(`{"supportsStrictTools":true}`), &model.Compat); err != nil {
		t.Fatal(err)
	}
	client, err := newAnthropicClient(model)
	if err != nil {
		t.Fatal(err)
	}
	var prompt Context
	if err := json.Unmarshal([]byte(`{"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"preferred","parameters":{"type":"object"},"constrainedSampling":{"type":"json_schema","strict":"prefer"}},{"name":"required","parameters":{"type":"object"},"constrainedSampling":{"type":"json_schema","strict":"require"}}]}`), &prompt); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		var final AssistantMessage
		if err := client.Stream(t.Context(), model, &prompt, func(_ context.Context, event StreamEvent) {
			if done, ok := event.(Done); ok {
				final = done.Message
			}
		}, WithAPIKey("key")); err != nil {
			t.Fatal(err)
		}
		if len(final.Diagnostics) != 1 || final.Diagnostics[0].Type != "anthropic_strict_tools_fallback" {
			t.Fatalf("diagnostics=%v", final.Diagnostics)
		}
	}
	if calls != 4 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestAnthropicInlineStrictFallbackReachesTransport(t *testing.T) {
	for _, rejection := range []string{
		"The compiled grammar is too large",
		"Too many strict tools (32)",
		"Schemas contains too many parameters with union types (480 parameters with type arrays or anyOf)",
	} {
		for _, key := range []string{"test-key"} {
			for _, format := range []string{"JSON", "SSE"} {
				t.Run(rejection+"/"+key+"/"+format, func(t *testing.T) {
					model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Compat: &CompatibilitySettings{SupportsStrictTools: true, SupportsMidConvoSystemMessages: true, SupportsMidConvoToolChanges: true}}
					parameters := json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"},"optional":{"type":"number"}},"required":["value"]}`)
					required := Tool{Name: "read", Parameters: parameters, ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"require"}`)}
					preferred := required
					preferred.ConstrainedSampling = json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)
					prompt := &Context{Messages: []Message{
						{Role: "system", Content: "policy", ToolsAdded: []Tool{required}},
						{Role: "user", Content: "question"},
						{Role: "system", ToolsAdded: []Tool{preferred}},
						{Role: "system", ToolsAdded: []Tool{required}},
					}}
					before, err := json.Marshal(prompt)
					require.NoError(t, err)
					modelBefore, err := json.Marshal(model)
					require.NoError(t, err)
					var requests, callbacks, responses, closes, terminals int
					var callbackPayload string
					var payloads []map[string]any
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
						wire, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						assert.Equal(t, callbackPayload, string(wire))
						var body map[string]any
						require.NoError(t, json.Unmarshal(wire, &body))
						payloads = append(payloads, body)
						assert.Equal(t, model.ID, body["model"])
						assert.Equal(t, true, body["stream"])
						definitions := body["tools"].([]any)
						require.Len(t, definitions, 2)
						initial := definitions[0].(map[string]any)
						assert.Equal(t, "read", initial["name"])
						assert.Equal(t, true, initial["strict"])
						assert.Equal(t, map[string]any{"type": "ephemeral", "ttl": "1h"}, initial["cache_control"])
						placeholder := definitions[1].(map[string]any)
						assert.Equal(t, "__pi_deferred_placeholder__", placeholder["name"])
						assert.Equal(t, true, placeholder["defer_loading"])
						messages := body["messages"].([]any)
						require.Len(t, messages, 3)
						for i, raw := range messages[1:] {
							message := raw.(map[string]any)
							assert.Equal(t, "system", message["role"])
							blocks := message["content"].([]any)
							require.Len(t, blocks, 1)
							block := blocks[0].(map[string]any)
							assert.Equal(t, "tool_addition", block["type"])
							definition := block["tool"].(map[string]any)["definition"].(map[string]any)
							assert.Equal(t, "read", definition["name"])
							assert.NotContains(t, definition, "cache_control")
							wantSchema := `{"type":"object","properties":{"value":{"type":"string"},"optional":{"anyOf":[{"type":"number"},{"type":"null"}]}},"required":["optional","value"],"additionalProperties":false}`
							if requests == 2 && i == 0 {
								assert.NotContains(t, definition, "strict")
								wantSchema = string(parameters)
							} else {
								assert.Equal(t, true, definition["strict"])
								assert.Equal(t, initial["input_schema"], definition["input_schema"])
							}
							schema, err := json.Marshal(definition["input_schema"])
							require.NoError(t, err)
							assert.JSONEq(t, wantSchema, string(schema))
						}
						assert.Contains(t, req.Header.Get("anthropic-beta"), anthropicInlineToolsBeta)
						status, contentType := http.StatusOK, "application/json"
						response := `{"id":"msg_inline","content":[],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
						if format == "SSE" {
							contentType = "text/event-stream"
							response = "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_inline","usage":{"input_tokens":3}}}` + "\n\nevent: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\nevent: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
						}
						if requests == 1 {
							status, contentType = http.StatusBadRequest, "application/json"
							response = `{"error":{"type":"invalid_request_error","message":` + jsonQuote(rejection) + `}}`
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: &anthropicEOFBody{Reader: strings.NewReader(response), closes: &closes}}, nil
					})}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					opts := []Option{WithAPIKey(key), WithCacheRetention("long"), WithMaxRetries(0), WithPayloadCallback(func(payload any) any { callbacks++; callbackPayload = payload.(string); return nil }), WithResponseCallback(func(response ProviderResponse) { responses++; assert.Equal(t, http.StatusOK, response.Status) })}
					check := func(message AssistantMessage) {
						t.Helper()
						assert.Equal(t, StopReasonStop, message.StopReason)
						assert.Equal(t, "msg_inline", message.ResponseID)
						assert.Equal(t, model.ID, message.Model)
						assert.Equal(t, model.Provider, message.Provider)
						assert.Equal(t, model.API, message.API)
						assert.Equal(t, 5, message.Usage.TotalTokens)
					}
					err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
						switch e := event.(type) {
						case Done:
							terminals++
							check(e.Message)
							require.Len(t, e.Message.Diagnostics, 1)
							assert.Equal(t, "anthropic_strict_tools_fallback", e.Message.Diagnostics[0].Type)
							assert.Equal(t, map[string]string{"reason": "strict_tool_limits_exceeded"}, e.Message.Diagnostics[0].Details)
						case StreamError:
							t.Errorf("unexpected stream error: %v", e.Err)
						}
					}, opts...)
					require.NoError(t, err)
					assert.Equal(t, []int{2, 2, 1, 2, 1}, []int{requests, callbacks, responses, closes, terminals})
					message, err := client.Complete(t.Context(), model, prompt, opts...)
					require.NoError(t, err)
					require.NotNil(t, message)
					check(*message)
					assert.Empty(t, message.Diagnostics)
					assert.Equal(t, []int{3, 3, 2, 3}, []int{requests, callbacks, responses, closes})
					assert.Equal(t, payloads[0], payloads[2])
					assert.Equal(t, payloads[0]["tools"], payloads[1]["tools"])
					after, err := json.Marshal(prompt)
					require.NoError(t, err)
					assert.Equal(t, string(before), string(after))
					modelAfter, err := json.Marshal(model)
					require.NoError(t, err)
					assert.Equal(t, string(modelBefore), string(modelAfter))
				})
			}
		}
	}
}

func TestAnthropicStrictFallbackRequiresAdmittedPreference(t *testing.T) {
	for _, scenario := range []string{"required", "ordinary", "unsupported-schema", "unsupported-capability"} {
		t.Run(scenario, func(t *testing.T) {
			model := &Model{ID: "claude-admission", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Compat: &CompatibilitySettings{SupportsStrictTools: scenario != "unsupported-capability"}}
			tool := Tool{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"number"}},"required":["value"]}`)}
			wantTool := `{"name":"lookup","description":"","input_schema":{"type":"object","properties":{"value":{"type":"number"}},"required":["value"]},"eager_input_streaming":true}`
			switch scenario {
			case "required":
				tool.ConstrainedSampling = json.RawMessage(`{"type":"json_schema","strict":"require"}`)
				wantTool = `{"name":"lookup","description":"","input_schema":{"type":"object","properties":{"value":{"type":"number"}},"required":["value"],"additionalProperties":false},"strict":true,"eager_input_streaming":true}`
			case "unsupported-schema":
				tool.Parameters = json.RawMessage(`{"type":"object","properties":{"value":{"type":"number","minimum":1}},"required":["value"]}`)
				tool.ConstrainedSampling = json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)
				wantTool = `{"name":"lookup","description":"","input_schema":{"type":"object","properties":{"value":{"type":"number","minimum":1}},"required":["value"]},"eager_input_streaming":true}`
			case "unsupported-capability":
				tool.ConstrainedSampling = json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)
			}
			prompt := &Context{Tools: []Tool{tool}, Messages: []Message{{Role: "user", Content: "question"}}}
			before, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			const rejection = `{"error":{"type":"invalid_request_error","message":"The compiled grammar is too large"}}`
			var requests, callbacks, responses, closes, events int
			var payload string
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				assert.Equal(t, requests-1, closes)
				assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
				assert.Equal(t, "test-key", req.Header.Get("x-api-key"))
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, payload, string(wire))
				var body map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(wire, &body))
				assert.JSONEq(t, "["+wantTool+"]", string(body["tools"]))
				assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(body["messages"]))
				status, response := http.StatusBadRequest, rejection
				if requests > 2 {
					status, response = http.StatusOK, `{"id":"msg_admission","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After-Ms": {"0"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(response)), closes: &closes}}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxRetries(1), WithPayloadCallback(func(value any) any {
				callbacks++
				payload = value.(string)
				return nil
			}), WithResponseCallback(func(response ProviderResponse) {
				responses++
				assert.Equal(t, http.StatusOK, response.Status)
			})}
			checkRejection := func(err error) {
				t.Helper()
				var rejected *providerStatusError
				require.ErrorAs(t, err, &rejected)
				assert.Equal(t, http.StatusBadRequest, rejected.status)
				assert.Equal(t, rejection, string(rejected.body))
			}
			err = StreamWithModel(t.Context(), model, prompt, func(context.Context, StreamEvent) { events++ }, opts...)
			checkRejection(err)
			assert.Equal(t, []int{1, 1, 0, 1, 0}, []int{requests, callbacks, responses, closes, events})
			message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
			checkRejection(err)
			assert.Nil(t, message)
			assert.Equal(t, []int{2, 2, 0, 2, 0}, []int{requests, callbacks, responses, closes, events})
			message, err = CompleteWithModel(t.Context(), model, prompt, opts...)
			require.NoError(t, err)
			require.NotNil(t, message)
			assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
			assert.Equal(t, StopReasonStop, message.StopReason)
			assert.Equal(t, "msg_admission", message.ResponseID)
			assert.Equal(t, model.ID, message.Model)
			assert.Equal(t, model.API, message.API)
			assert.Equal(t, model.Provider, message.Provider)
			assert.Equal(t, 5, message.Usage.TotalTokens)
			assert.Empty(t, message.Diagnostics)
			assert.Equal(t, []int{3, 3, 1, 3, 0}, []int{requests, callbacks, responses, closes, events})
			after, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
			assert.Empty(t, client.tools)
		})
	}
}

func TestAnthropicStrictFallbackDoesNotRetryOtherErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "unrelated message", status: 400, body: `{"error":{"type":"invalid_request_error","message":"bad parameter"}}`},
		{name: "wrong error type", status: 400, body: `{"error":{"type":"authentication_error","message":"Too many strict tools (21)"}}`},
		{name: "server error", status: 500, body: `{"error":{"type":"invalid_request_error","message":"Too many strict tools (21)"}}`},
		{name: "forbidden", status: 403, body: `{"error":{"type":"invalid_request_error","message":"Too many strict tools (21)"}}`},
		{name: "malformed body", status: 400, body: `{"error":{"type":"invalid_request_error","message":"Too many strict tools (21)"}`},
		{name: "flat body", status: 400, body: `{"type":"invalid_request_error","message":"Too many strict tools (21)"}`},
		{name: "marker outside error", status: 400, body: `{"error":{"type":"invalid_request_error","message":"bad parameter"},"details":"Too many strict tools (21)"}`},
		{name: "null message", status: 400, body: `{"error":{"type":"invalid_request_error","message":null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &Model{ID: "claude-classifier", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Compat: &CompatibilitySettings{SupportsStrictTools: true}}
			prompt := &Context{Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)}}, Messages: []Message{{Role: "user", Content: "question"}}}
			before, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			var requests, payloads, responses, closes, events int
			var payload string
			reset := false
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				require.LessOrEqual(t, requests, 3, "unrecognized rejection must not downgrade")
				assert.Equal(t, requests-1, closes)
				assert.Equal(t, http.MethodPost, req.Method)
				assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
				assert.Equal(t, "test-key", req.Header.Get("x-api-key"))
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, payload, string(wire))
				var body map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(wire, &body))
				assert.JSONEq(t, `"claude-classifier"`, string(body["model"]))
				assert.Equal(t, "true", string(body["stream"]))
				assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(body["messages"]))
				assert.JSONEq(t, `[{"name":"lookup","description":"","input_schema":{"type":"object","properties":{"value":{"anyOf":[{"type":"string"},{"type":"null"}]}},"required":["value"],"additionalProperties":false},"strict":true,"eager_input_streaming":true}]`, string(body["tools"]))
				status, response := tc.status, tc.body
				if reset {
					status, response = http.StatusOK, `{"id":"msg_reset","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "X-Should-Retry": {"false"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(response)), closes: &closes}}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxRetries(1), WithPayloadCallbackForModel(func(value any, got *Model) any {
				payloads++
				assert.Same(t, model, got)
				payload = value.(string)
				return nil
			}), WithResponseCallbackForModel(func(response ProviderResponse, got *Model) {
				responses++
				assert.Same(t, model, got)
				assert.Equal(t, http.StatusOK, response.Status)
				assert.Equal(t, requests-1, closes)
			})}
			for call, entry := range []string{"Stream", "Complete"} {
				if entry == "Stream" {
					err = StreamWithModel(t.Context(), model, prompt, func(context.Context, StreamEvent) { events++ }, opts...)
				} else {
					var message *AssistantMessage
					message, err = CompleteWithModel(t.Context(), model, prompt, opts...)
					assert.Nil(t, message)
				}
				var rejected *providerStatusError
				require.ErrorAs(t, err, &rejected)
				assert.Equal(t, "anthropic", rejected.label)
				assert.Equal(t, tc.status, rejected.status)
				assert.Equal(t, tc.body, string(rejected.body))
				assert.EqualError(t, err, fmt.Sprintf("anthropic: status %d: %s", tc.status, tc.body))
				assert.Equal(t, []int{call + 1, call + 1, 0, call + 1, 0}, []int{requests, payloads, responses, closes, events})
			}
			reset = true
			message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
			require.NoError(t, err)
			require.NotNil(t, message)
			assert.Equal(t, StopReasonStop, message.StopReason)
			assert.Equal(t, "msg_reset", message.ResponseID)
			assert.Equal(t, model.ID, message.Model)
			assert.Equal(t, model.API, message.API)
			assert.Equal(t, model.Provider, message.Provider)
			assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
			assert.Equal(t, Usage{Input: 3, Output: 2, TotalTokens: 5}, message.Usage)
			assert.Empty(t, message.Diagnostics)
			assert.Empty(t, message.ErrorMessage)
			assert.Equal(t, []int{3, 3, 1, 3, 0}, []int{requests, payloads, responses, closes, events})
			after, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
			assert.Empty(t, client.tools)
			assert.Equal(t, http.Header{"Content-Type": {"application/json"}, "anthropic-version": {"2023-06-01"}}, client.transport.headers)
		})
	}
}

func TestAnthropicStrictFallbackReturnsSecondRejection(t *testing.T) {
	model := &Model{ID: "claude-rejected", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Compat: &CompatibilitySettings{SupportsStrictTools: true}}
	prompt := &Context{Tools: []Tool{
		{Name: "preferred", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)},
		{Name: "required", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"require"}`)},
	}, Messages: []Message{{Role: "user", Content: "question"}}}
	before, err := json.Marshal([]any{model, prompt})
	require.NoError(t, err)
	const (
		first        = `{"error":{"type":"invalid_request_error","message":"The compiled grammar is too large"}}`
		second       = `{"error":{"type":"invalid_request_error","message":"Too many strict tools (required grammar remains too large)"}}`
		strictSchema = `{"type":"object","properties":{"value":{"anyOf":[{"type":"string"},{"type":"null"}]}},"required":["value"],"additionalProperties":false}`
	)
	var requests, callbacks, responses, closes, events, attempt int
	var payload string
	reset := false
	previous := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = previous })
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		attempt++
		require.LessOrEqual(t, attempt, 2, "second rejection must end fallback")
		assert.Equal(t, requests-1, closes)
		wire, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		assert.Equal(t, payload, string(wire))
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(wire, &body))
		preferred := `{"name":"preferred","description":"","input_schema":` + strictSchema + `,"strict":true,"eager_input_streaming":true}`
		if attempt == 2 {
			preferred = `{"name":"preferred","description":"","input_schema":{"type":"object","properties":{"value":{"type":"string"}}},"eager_input_streaming":true}`
		}
		required := `{"name":"required","description":"","input_schema":` + strictSchema + `,"strict":true,"eager_input_streaming":true}`
		assert.JSONEq(t, `[`+preferred+`,`+required+`]`, string(body["tools"]))
		assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(body["messages"]))
		status, response := http.StatusBadRequest, first
		if attempt == 2 {
			response = second
		}
		if reset {
			status, response = http.StatusOK, `{"id":"msg_reset","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(response)), closes: &closes}}, nil
	})}
	client, err := newAnthropicClient(model)
	require.NoError(t, err)
	opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxRetries(1), WithPayloadCallback(func(value any) any {
		callbacks++
		payload = value.(string)
		return nil
	}), WithResponseCallback(func(response ProviderResponse) {
		responses++
		assert.Equal(t, http.StatusOK, response.Status)
	})}
	for call, entry := range []string{"Stream", "Complete"} {
		attempt = 0
		if entry == "Stream" {
			err = StreamWithModel(t.Context(), model, prompt, func(context.Context, StreamEvent) { events++ }, opts...)
		} else {
			var message *AssistantMessage
			message, err = CompleteWithModel(t.Context(), model, prompt, opts...)
			assert.Nil(t, message)
		}
		var rejected *providerStatusError
		require.ErrorAs(t, err, &rejected)
		assert.Equal(t, "anthropic", rejected.label)
		assert.Equal(t, http.StatusBadRequest, rejected.status)
		assert.Equal(t, second, string(rejected.body))
		assert.ErrorContains(t, err, second)
		assert.NotContains(t, err.Error(), "The compiled grammar is too large")
		assert.Equal(t, []int{2 * (call + 1), 2 * (call + 1), 0, 2 * (call + 1), 0}, []int{requests, callbacks, responses, closes, events})
	}
	reset, attempt = true, 0
	message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
	require.NoError(t, err)
	require.NotNil(t, message)
	assert.Equal(t, StopReasonStop, message.StopReason)
	assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
	assert.Equal(t, "msg_reset", message.ResponseID)
	assert.Equal(t, model.ID, message.Model)
	assert.Equal(t, model.API, message.API)
	assert.Equal(t, model.Provider, message.Provider)
	assert.Equal(t, 5, message.Usage.TotalTokens)
	assert.Empty(t, message.Diagnostics)
	assert.Equal(t, []int{5, 5, 1, 5, 0}, []int{requests, callbacks, responses, closes, events})
	after, err := json.Marshal([]any{model, prompt})
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	assert.Empty(t, client.tools)
}

func TestAnthropicStrictFallbackStopsOnRejectedBodyFailure(t *testing.T) {
	for _, failure := range []string{"read", "close", "read and close"} {
		t.Run(failure, func(t *testing.T) {
			model := &Model{ID: "claude-body", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Compat: &CompatibilitySettings{SupportsStrictTools: true}}
			prompt := &Context{Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)}}, Messages: []Message{{Role: "user", Content: "question"}}}
			before, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			const rejection = `{"error":{"type":"invalid_request_error","message":"The compiled grammar is too large"}}`
			readErr, closeErr := errors.New("rejected body read failed"), errors.New("rejected body close failed")
			var requests, callbacks, responses, closes, events int
			var payload string
			reset := false
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				assert.Equal(t, requests-1, closes)
				assert.NoError(t, req.Context().Err())
				assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, payload, string(wire))
				var body map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(wire, &body))
				assert.JSONEq(t, `[{"name":"lookup","description":"","input_schema":{"type":"object","properties":{"value":{"anyOf":[{"type":"string"},{"type":"null"}]}},"required":["value"],"additionalProperties":false},"strict":true,"eager_input_streaming":true}]`, string(body["tools"]))
				assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(body["messages"]))
				status, response := http.StatusBadRequest, rejection
				if reset {
					status, response = http.StatusOK, `{"id":"msg_body","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
				}
				reader := &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(response)), closes: &closes}
				if !reset {
					if failure != "close" {
						reader.Reader = io.MultiReader(reader.Reader, iotest.ErrReader(readErr))
					}
					if failure != "read" {
						reader.closeErr = closeErr
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "X-Should-Retry": {"true"}, "Retry-After-Ms": {"0"}}, Body: reader}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxRetries(1), WithPayloadCallback(func(value any) any {
				callbacks++
				payload = value.(string)
				return nil
			}), WithResponseCallback(func(response ProviderResponse) {
				responses++
				assert.Equal(t, http.StatusOK, response.Status)
			})}
			for call, entry := range []string{"Stream", "Complete"} {
				if entry == "Stream" {
					err = StreamWithModel(t.Context(), model, prompt, func(context.Context, StreamEvent) { events++ }, opts...)
				} else {
					var message *AssistantMessage
					message, err = CompleteWithModel(t.Context(), model, prompt, opts...)
					assert.Nil(t, message)
				}
				require.ErrorContains(t, err, "anthropic: status 400:")
				if failure != "close" {
					assert.ErrorIs(t, err, readErr)
					assert.Contains(t, err.Error(), "read error response body")
				}
				if failure != "read" {
					assert.ErrorIs(t, err, closeErr)
					assert.Contains(t, err.Error(), "close error response body")
				}
				var rejected *providerStatusError
				assert.False(t, errors.As(err, &rejected), "body failure must precede strict rejection classification")
				assert.Equal(t, []int{call + 1, call + 1, 0, call + 1, 0}, []int{requests, callbacks, responses, closes, events})
			}
			reset = true
			message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
			require.NoError(t, err)
			require.NotNil(t, message)
			assert.Equal(t, StopReasonStop, message.StopReason)
			assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
			assert.Equal(t, "msg_body", message.ResponseID)
			assert.Equal(t, model.ID, message.Model)
			assert.Equal(t, model.API, message.API)
			assert.Equal(t, model.Provider, message.Provider)
			assert.Equal(t, 5, message.Usage.TotalTokens)
			assert.Empty(t, message.Diagnostics)
			assert.Equal(t, []int{3, 3, 1, 3, 0}, []int{requests, callbacks, responses, closes, events})
			after, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
			assert.Empty(t, client.tools)
		})
	}
}

func TestAnthropicStrictFallbackHonorsCancellation(t *testing.T) {
	for _, boundary := range []string{"transport", "read", "active"} {
		t.Run(boundary, func(t *testing.T) {
			model := &Model{ID: "claude-cancel", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Compat: &CompatibilitySettings{SupportsStrictTools: true}}
			prompt := &Context{Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)}}, Messages: []Message{{Role: "user", Content: "question"}}}
			before, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			const rejection = `{"error":{"type":"invalid_request_error","message":"The compiled grammar is too large"}}`
			var requests, callbacks, responses, closes, attempt, events int
			var payload string
			var cancel context.CancelCauseFunc
			cause := errors.New("caller canceled strict request")
			reset := false
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				attempt++
				assert.Equal(t, requests-1, closes)
				assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, payload, string(wire))
				var body map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(wire, &body))
				wantTool := `[{"name":"lookup","description":"","input_schema":{"type":"object","properties":{"value":{"anyOf":[{"type":"string"},{"type":"null"}]}},"required":["value"],"additionalProperties":false},"strict":true,"eager_input_streaming":true}]`
				if attempt == 2 {
					assert.Equal(t, "active", boundary, "canceled request reached fallback transport")
					assert.NoError(t, req.Context().Err())
					wantTool = `[{"name":"lookup","description":"","input_schema":{"type":"object","properties":{"value":{"type":"string"}}},"eager_input_streaming":true}]`
				}
				assert.JSONEq(t, wantTool, string(body["tools"]))
				assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(body["messages"]))
				status, response := http.StatusOK, `{"id":"msg_cancel","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
				reader := &anthropicCancelBody{closes: &closes}
				if attempt == 1 && !reset {
					status, response = http.StatusBadRequest, rejection
					switch boundary {
					case "transport":
						cancel(cause)
					case "read":
						reader.beforeRead = func() { cancel(cause) }
					}
				}
				reader.Reader = strings.NewReader(response)
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After-Ms": {"0"}}, Body: reader}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxRetries(1), WithPayloadCallback(func(value any) any {
				callbacks++
				payload = value.(string)
				return nil
			}), WithResponseCallback(func(response ProviderResponse) {
				responses++
				assert.Equal(t, http.StatusOK, response.Status)
			})}
			for call, entry := range []string{"Stream", "Complete"} {
				ctx, stop := context.WithCancelCause(t.Context())
				t.Cleanup(func() { stop(nil) })
				cancel, attempt = stop, 0
				var message *AssistantMessage
				if entry == "Stream" {
					err = StreamWithModel(ctx, model, prompt, func(_ context.Context, event StreamEvent) {
						events++
						terminal, ok := event.(Done)
						require.True(t, ok, "unexpected event: %T", event)
						message = &terminal.Message
					}, opts...)
				} else {
					message, err = CompleteWithModel(ctx, model, prompt, opts...)
				}
				if boundary != "active" {
					require.ErrorIs(t, err, context.Canceled)
					assert.ErrorIs(t, context.Cause(ctx), cause)
					var rejected *providerStatusError
					require.ErrorAs(t, err, &rejected)
					assert.Equal(t, http.StatusBadRequest, rejected.status)
					assert.Equal(t, rejection, string(rejected.body))
					assert.Nil(t, message)
					assert.Equal(t, []int{call + 1, call + 1, 0, call + 1, 0}, []int{requests, callbacks, responses, closes, events})
				} else {
					require.NoError(t, err)
					require.NotNil(t, message)
					assert.Equal(t, StopReasonStop, message.StopReason)
					require.Len(t, message.Diagnostics, 1)
					assert.Equal(t, "anthropic_strict_tools_fallback", message.Diagnostics[0].Type)
					assert.Equal(t, []int{2 * (call + 1), 2 * (call + 1), call + 1, 2 * (call + 1), 1}, []int{requests, callbacks, responses, closes, events})
				}
				stop(nil)
			}
			reset, attempt = true, 0
			message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
			require.NoError(t, err)
			require.NotNil(t, message)
			assert.Equal(t, StopReasonStop, message.StopReason)
			assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
			assert.Equal(t, "msg_cancel", message.ResponseID)
			assert.Equal(t, model.ID, message.Model)
			assert.Equal(t, model.API, message.API)
			assert.Equal(t, model.Provider, message.Provider)
			assert.Equal(t, 5, message.Usage.TotalTokens)
			assert.Empty(t, message.Diagnostics)
			wantRequests, wantResponses := 3, 1
			if boundary == "active" {
				wantRequests, wantResponses = 5, 3
			}
			assert.Equal(t, []int{wantRequests, wantRequests, wantResponses, wantRequests}, []int{requests, callbacks, responses, closes})
			after, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
			assert.Empty(t, client.tools)
		})
	}
}

func TestAnthropicStrictFallbackStopsAtAcceptedSSE(t *testing.T) {
	for _, downgraded := range []bool{false, true} {
		for _, partial := range []bool{false, true} {
			for _, named := range []bool{false, true} {
				t.Run(fmt.Sprintf("downgraded=%t/partial=%t/named=%t", downgraded, partial, named), func(t *testing.T) {
					model := &Model{ID: "claude-accepted", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Compat: &CompatibilitySettings{SupportsStrictTools: true}}
					prompt := &Context{Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)}}, Messages: []Message{{Role: "user", Content: "question"}}}
					before, err := json.Marshal([]any{model, prompt})
					require.NoError(t, err)
					const rejection = `{"type":"error","error":{"type":"invalid_request_error","message":"The compiled grammar is too large"}}`
					failure := "data: " + rejection + "\n\n"
					if named {
						failure = "event: error\n" + failure
					}
					wantContent := []ContentBlock{}
					wantID, wantInput := "", 0
					wantEvents := []string{"start"}
					if partial {
						failure = "data: " + `{"type":"message_start","message":{"id":"msg_partial","usage":{"input_tokens":3}}}` + "\n\n" +
							"data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"partial"}}` + "\n\n" +
							"data: " + `{"type":"content_block_stop","index":0}` + "\n\n" + failure
						wantContent = []ContentBlock{{Type: ContentBlockText, Text: "partial"}}
						wantID, wantInput = "msg_partial", 3
						wantEvents = append(wantEvents, "text_start", "text_end")
					}
					wantEvents = append(wantEvents, "error")
					var requests, callbacks, responses, closes, attempt int
					var payload string
					var order []string
					reset := false
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						attempt++
						order = append(order, "request")
						assert.Equal(t, requests-1, closes)
						wire, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						assert.Equal(t, payload, string(wire))
						var body map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(wire, &body))
						wantTool := `[{"name":"lookup","description":"","input_schema":{"type":"object","properties":{"value":{"anyOf":[{"type":"string"},{"type":"null"}]}},"required":["value"],"additionalProperties":false},"strict":true,"eager_input_streaming":true}]`
						if downgraded && attempt == 2 && !reset {
							wantTool = `[{"name":"lookup","description":"","input_schema":{"type":"object","properties":{"value":{"type":"string"}}},"eager_input_streaming":true}]`
						}
						assert.JSONEq(t, wantTool, string(body["tools"]))
						assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(body["messages"]))
						status, contentType, response := http.StatusOK, "text/event-stream", failure
						if reset {
							contentType, response = "application/json", `{"id":"msg_reset","content":[],"stop_reason":"end_turn"}`
						} else if downgraded && attempt == 1 {
							status, contentType, response = http.StatusBadRequest, "application/json", rejection
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}, "Retry-After-Ms": {"0"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(response)), closes: &closes}}, nil
					})}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxRetries(1), WithPayloadCallback(func(value any) any {
						callbacks++
						order = append(order, "payload")
						payload = value.(string)
						return nil
					}), WithResponseCallback(func(response ProviderResponse) {
						responses++
						order = append(order, "response")
						assert.Equal(t, http.StatusOK, response.Status)
					})}
					check := func(message AssistantMessage) {
						t.Helper()
						assert.Equal(t, StopReasonError, message.StopReason)
						assert.Equal(t, rejection, message.ErrorMessage)
						assert.Equal(t, wantContent, message.Content)
						assert.Equal(t, wantID, message.ResponseID)
						assert.Equal(t, model.ID, message.Model)
						assert.Equal(t, model.API, message.API)
						assert.Equal(t, model.Provider, message.Provider)
						assert.Equal(t, wantInput, message.Usage.Input)
						assert.Zero(t, message.Usage.Output)
						if !downgraded {
							assert.Empty(t, message.Diagnostics)
							return
						}
						require.Len(t, message.Diagnostics, 1)
						diagnostic := message.Diagnostics[0]
						assert.Equal(t, "anthropic_strict_tools_fallback", diagnostic.Type)
						assert.Positive(t, diagnostic.Timestamp)
						assert.Equal(t, map[string]string{"message": rejection}, diagnostic.Error)
						assert.Equal(t, map[string]string{"reason": "strict_tool_limits_exceeded"}, diagnostic.Details)
					}
					var events []string
					err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
						switch e := event.(type) {
						case Start:
							events = append(events, "start")
						case TextStart:
							events = append(events, "text_start")
						case TextEnd:
							events = append(events, "text_end")
						case StreamError:
							events = append(events, "error")
							order = append(order, "terminal")
							assert.EqualError(t, e.Err, rejection)
							assert.Equal(t, string(StopReasonError), e.Reason)
							check(e.Msg)
						default:
							t.Errorf("unexpected event: %T", event)
						}
					}, opts...)
					require.NoError(t, err)
					assert.Equal(t, wantEvents, events)
					perCall := 1
					wantOrder := []string{"payload", "request"}
					if downgraded {
						perCall = 2
						wantOrder = append(wantOrder, "payload", "request")
					}
					assert.Equal(t, append(wantOrder, "response", "terminal"), order)
					assert.Equal(t, []int{perCall, perCall, 1, perCall}, []int{requests, callbacks, responses, closes})
					attempt = 0
					message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
					require.NoError(t, err)
					require.NotNil(t, message)
					check(*message)
					assert.Equal(t, []int{2 * perCall, 2 * perCall, 2, 2 * perCall}, []int{requests, callbacks, responses, closes})
					reset, attempt = true, 0
					message, err = CompleteWithModel(t.Context(), model, prompt, opts...)
					require.NoError(t, err)
					require.NotNil(t, message)
					assert.Equal(t, StopReasonStop, message.StopReason)
					assert.Equal(t, "msg_reset", message.ResponseID)
					assert.Empty(t, message.Diagnostics)
					assert.Equal(t, []int{2*perCall + 1, 2*perCall + 1, 3, 2*perCall + 1}, []int{requests, callbacks, responses, closes})
					after, err := json.Marshal([]any{model, prompt})
					require.NoError(t, err)
					assert.Equal(t, string(before), string(after))
					assert.Empty(t, client.tools)
				})
			}
		}
	}
}

func TestAnthropicStrictFallbackReportsOnlyAcceptedResponse(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(400)
			if _, err := io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"The compiled grammar is too large"}}`); err != nil {
				t.Error(err)
			}
			return
		}
		if _, err := io.WriteString(w, `{"content":[],"stop_reason":"end_turn"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	model := &Model{ID: "test", Provider: ProviderAnthropic, API: APIAnthropicMessages, BaseURL: server.URL, Compat: &CompatibilitySettings{SupportsStrictTools: true}}
	client, err := newAnthropicClient(model)
	if err != nil {
		t.Fatal(err)
	}
	responses := 0
	prompt := &Context{Tools: []Tool{{Name: "preferred", Parameters: json.RawMessage(`{"type":"object"}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)}}}
	err = client.Stream(t.Context(), model, prompt, func(context.Context, StreamEvent) {}, WithAPIKey("key"), WithResponseCallback(func(response ProviderResponse) {
		responses++
		if response.Status != 200 {
			t.Errorf("rejected response delivered: %d", response.Status)
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if responses != 1 {
		t.Fatalf("response callbacks=%d", responses)
	}
	if string(prompt.Tools[0].ConstrainedSampling) != `{"type":"json_schema","strict":"prefer"}` {
		t.Fatalf("fallback leaked into the caller's tools: %s", prompt.Tools[0].ConstrainedSampling)
	}
}

func TestAnthropicManagedEffortAndInlineDeclarationsMatchUpstream(t *testing.T) {
	var body map[string]any
	var beta string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		beta = r.Header.Get("anthropic-beta")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":10,"cache_creation_input_tokens":1000,"cache_creation":{"ephemeral_1h_input_tokens":400}}}}`+"\n\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`+"\n\n"+
			`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer server.Close()

	model := &Model{
		ID: "claude-opus-5-5", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: server.URL, MaxTokens: 1000, Reasoning: true,
		Cost:   CostProfile{Input: 10, Output: 50, CacheWrite: 12.5},
		Compat: &CompatibilitySettings{SupportsMidConvoEffort: true, SupportsMidConvoSystemMessages: true, SupportsMidConvoToolChanges: true},
	}
	read := Tool{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}
	grep := Tool{Name: "grep", Parameters: json.RawMessage(`{"type":"object"}`)}
	prompt := &Context{Messages: []Message{
		{Role: "system", Content: "base", ToolsAdded: []Tool{read}},
		{Role: "user", Content: "hi"},
		{Role: "assistant", API: APIAnthropicMessages, Provider: ProviderAnthropic, Model: model.ID, ProviderThinkingLevel: "low", Content: []ContentBlock{
			{Type: ContentBlockThinking, Thinking: "hmm", ThoughtSignature: "sig"}, {Type: ContentBlockText, Text: "hello"},
		}},
		{Role: "system", Sections: map[string]*string{"policy": new("strict")}, ToolsRemoved: []ToolReference{{Name: "read"}}, ToolsAdded: []Tool{grep}},
		{Role: "user", Content: "next"},
	}}
	client, err := newAnthropicClient(model)
	if err != nil {
		t.Fatal(err)
	}
	message, err := client.Complete(t.Context(), model, prompt, WithAPIKey("key"), WithReasoning(ReasoningMedium), WithTemperature(0.5), WithCacheRetention("long"))
	if err != nil {
		t.Fatal(err)
	}
	if message.ProviderThinkingLevel != "medium" {
		t.Fatalf("providerThinkingLevel = %q", message.ProviderThinkingLevel)
	}
	if message.Usage.CacheWrite1h != 400 || message.Usage.Cost.CacheWrite != (600*12.5+400*20)/1e6 {
		t.Fatalf("1h cache write usage = %+v", message.Usage)
	}
	for _, want := range []string{"mid-conversation-output-config-2026-07-01", "thinking-binding-controls-2026-08-01", "inline-tools-2026-09-15"} {
		if !strings.Contains(beta, want) {
			t.Fatalf("beta %q lacks %s", beta, want)
		}
	}
	got, err := json.Marshal(map[string]any{"system": body["system"], "tools": body["tools"], "messages": body["messages"], "thinking": body["thinking"], "output_config": body["output_config"], "temperature": body["temperature"]})
	if err != nil {
		t.Fatal(err)
	}
	cache := `"cache_control":{"ttl":"1h","type":"ephemeral"}`
	want := `{"messages":[` +
		`{"content":[{"text":"hi","type":"text"}],"role":"user"},` +
		`{"content":[],"output_config":{"effort":"low"},"role":"system"},` +
		`{"content":[{"signature":"sig","thinking":"hmm","type":"thinking"},{"text":"hello","type":"text"}],"role":"assistant"},` +
		`{"content":[{"text":"next","type":"text"}],"role":"user"},` +
		`{"content":[{"text":"Updated system prompt section \"policy\":\n\nstrict","type":"text"},{"tool":{"name":"read","type":"tool_reference"},"type":"tool_removal"},{` + cache + `,"tool":{"definition":{"description":"","eager_input_streaming":true,"input_schema":{"type":"object"},"name":"grep"},"type":"tool_definition"},"type":"tool_addition"}],"role":"system"},` +
		`{"content":[],"output_config":{"effort":"medium"},"role":"system"}],` +
		`"output_config":{"effort":"high"},` +
		`"system":[{` + cache + `,"text":"base","type":"text"}],` +
		`"temperature":null,` +
		`"thinking":{"block_binding":{"prefix_mismatch_behavior":"drop_block"},"display":"summarized","type":"adaptive"},` +
		`"tools":[{` + cache + `,"description":"","eager_input_streaming":true,"input_schema":{"type":"object"},"name":"read"},{"defer_loading":true,"description":"Reserved placeholder. Never available. Never call this.","input_schema":{"properties":{},"required":[],"type":"object"},"name":"__pi_deferred_placeholder__"}]}`
	if string(got) != want {
		t.Fatalf("request:\n%s\nwant:\n%s", got, want)
	}
}

func TestAnthropicNativeToolAdmissionReachesTransport(t *testing.T) {
	for _, scenario := range []string{"native", "system only", "tools only", "neither", "no compatibility", "no anchor"} {
		t.Run(scenario, func(t *testing.T) {
			model := &Model{ID: "claude-native", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
			if scenario != "no compatibility" {
				model.Compat = &CompatibilitySettings{
					SupportsMidConvoSystemMessages: scenario == "native" || scenario == "system only" || scenario == "no anchor",
					SupportsMidConvoToolChanges:    scenario == "native" || scenario == "tools only" || scenario == "no anchor",
				}
			}
			read := Tool{Name: "read", Description: "original", Parameters: json.RawMessage(`{"type":"object"}`)}
			baseline := Message{Role: "system", Content: "base"}
			if scenario != "no anchor" {
				baseline.ToolsAdded = []Tool{read}
			}
			initial := &Context{Messages: []Message{baseline, {Role: "user", Content: "question"}}}
			redefined := read
			redefined.Description = "replacement"
			updated := &Context{Messages: append(slices.Clone(initial.Messages), Message{Role: "system", Content: "update", ToolsRemoved: []ToolReference{{Name: "read"}}, ToolsAdded: []Tool{redefined, {Name: "lookup", Description: "late", Parameters: read.Parameters}}})}
			before, err := json.Marshal([]any{model, initial, updated})
			require.NoError(t, err)
			var requests, callbacks, responses, closes, terminals int
			var callbackPayload string
			var firstTools json.RawMessage
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, callbackPayload, string(wire))
				var body map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(wire, &body))
				definition := func(name, description string) string {
					return fmt.Sprintf(`{"name":%q,"description":%q,"input_schema":{"type":"object"},"eager_input_streaming":true}`, name, description)
				}
				wantTools := `[` + definition("read", "original") + `]`
				wantMessages := `[{"role":"user","content":[{"type":"text","text":"question"}]}]`
				wantSystem := `[{"type":"text","text":"base"}]`
				if scenario == "native" {
					wantTools = `[` + definition("read", "original") + `,{"name":"__pi_deferred_placeholder__","description":"Reserved placeholder. Never available. Never call this.","input_schema":{"type":"object","properties":{},"required":[]},"defer_loading":true}]`
					assert.Contains(t, req.Header.Get("anthropic-beta"), "inline-tools-2026-09-15")
				} else {
					assert.NotContains(t, req.Header.Get("anthropic-beta"), "inline-tools-2026-09-15")
				}
				if requests == 1 {
					firstTools = slices.Clone(body["tools"])
					if scenario == "no anchor" {
						assert.NotContains(t, body, "tools")
					}
				} else {
					if scenario == "native" {
						assert.JSONEq(t, string(firstTools), string(body["tools"]))
						wantMessages = `[{"role":"user","content":[{"type":"text","text":"question"}]},{"role":"system","content":[{"type":"text","text":"update"},{"type":"tool_addition","tool":{"type":"tool_definition","definition":` + definition("read", "replacement") + `}},{"type":"tool_addition","tool":{"type":"tool_definition","definition":` + definition("lookup", "late") + `}}]}]`
					} else {
						wantTools = `[` + definition("read", "replacement") + `,` + definition("lookup", "late") + `]`
						if scenario == "system only" || scenario == "no anchor" {
							wantMessages = `[{"role":"user","content":[{"type":"text","text":"question"}]},{"role":"system","content":[{"type":"text","text":"update"}]}]`
						} else {
							wantSystem = `[{"type":"text","text":"base\n\nupdate"}]`
						}
					}
				}
				if scenario != "no anchor" || requests == 2 {
					assert.JSONEq(t, wantTools, string(body["tools"]))
				}
				assert.JSONEq(t, wantMessages, string(body["messages"]))
				assert.JSONEq(t, wantSystem, string(body["system"]))
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: &anthropicEOFBody{Reader: strings.NewReader(`{"id":"msg_native","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`), closes: &closes}}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any { callbacks++; callbackPayload = payload.(string); return nil }), WithResponseCallback(func(_ ProviderResponse) { responses++ })}
			check := func(message AssistantMessage) {
				t.Helper()
				assert.Equal(t, StopReasonStop, message.StopReason)
				assert.Equal(t, "msg_native", message.ResponseID)
				assert.Equal(t, model.ID, message.Model)
				assert.Equal(t, model.API, message.API)
				assert.Equal(t, model.Provider, message.Provider)
				assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "ok"}}, message.Content)
				assert.Equal(t, 5, message.Usage.TotalTokens)
				assert.Empty(t, message.Diagnostics)
			}
			err = StreamWithModel(t.Context(), model, initial, func(_ context.Context, event StreamEvent) {
				switch e := event.(type) {
				case Done:
					terminals++
					check(e.Message)
				case StreamError:
					t.Errorf("unexpected terminal: %v", e.Err)
				}
			}, opts...)
			require.NoError(t, err)
			assert.Equal(t, 1, terminals)
			message, err := CompleteWithModel(t.Context(), model, updated, opts...)
			require.NoError(t, err)
			require.NotNil(t, message)
			check(*message)
			assert.Equal(t, []int{2, 2, 2, 2}, []int{requests, callbacks, responses, closes})
			after, err := json.Marshal([]any{model, initial, updated})
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}

func TestAnthropicManagedEffortReplayReachesTransport(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		for _, format := range []string{"JSON", "SSE"} {
			t.Run(effort+"/"+format, func(t *testing.T) {
				model := &Model{ID: "claude-managed", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Reasoning: true, Compat: &CompatibilitySettings{SupportsMidConvoEffort: true, ForceAdaptiveThinking: true}}
				modelBefore, err := json.Marshal(model)
				require.NoError(t, err)
				prompt := &Context{Messages: []Message{{Role: "user", Content: "one"}}}
				var requests, callbacks, responses, closes, starts, terminals int
				var callbackPayload string
				var payloads []map[string]any
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					wire, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, callbackPayload, string(wire))
					var body map[string]any
					require.NoError(t, json.Unmarshal(wire, &body))
					payloads = append(payloads, body)
					betas := req.Header.Get("anthropic-beta")
					if requests < 3 {
						assert.Contains(t, betas, "mid-conversation-output-config-2026-07-01")
						assert.Contains(t, betas, "thinking-binding-controls-2026-08-01")
					} else {
						assert.NotContains(t, betas, "mid-conversation-output-config-2026-07-01")
						assert.NotContains(t, betas, "thinking-binding-controls-2026-08-01")
					}
					contentType := "application/json"
					response := `{"id":"msg_effort","content":[{"type":"thinking","thinking":"reasoning","signature":"opaque-signature"},{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
					if format == "SSE" {
						contentType = "text/event-stream"
						response = "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_effort","usage":{"input_tokens":3}}}` + "\n\n" +
							"event: content_block_start\ndata: " + `{"type":"content_block_start","index":3,"content_block":{"type":"thinking","thinking":"reasoning","signature":"opaque-signature"}}` + "\n\n" +
							"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":3}` + "\n\n" +
							"event: content_block_start\ndata: " + `{"type":"content_block_start","index":5,"content_block":{"type":"text","text":"answer"}}` + "\n\n" +
							"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":5}` + "\n\n" +
							"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
							"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(response)), closes: &closes}}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any { callbacks++; callbackPayload = payload.(string); return nil }), WithResponseCallback(func(response ProviderResponse) { responses++; assert.Equal(t, http.StatusOK, response.Status) })}
				firstOptions := opts
				if effort != "" {
					firstOptions = append(firstOptions, WithEffort(effort))
				}
				wantEffort := effort
				if wantEffort == "" {
					wantEffort = "high"
				}
				check := func(message AssistantMessage, level string) {
					t.Helper()
					assert.Equal(t, StopReasonStop, message.StopReason)
					assert.Equal(t, "msg_effort", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, level, message.ProviderThinkingLevel)
					assert.Equal(t, 5, message.Usage.TotalTokens)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockThinking, Thinking: "reasoning", ThoughtSignature: "opaque-signature"}, {Type: ContentBlockText, Text: "answer"}}, message.Content)
					assert.Empty(t, message.Diagnostics)
				}
				var first AssistantMessage
				err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case Start:
						starts++
						assert.Equal(t, wantEffort, e.Partial.ProviderThinkingLevel)
					case Done:
						terminals++
						first = e.Message
						check(first, wantEffort)
					case StreamError:
						t.Errorf("unexpected stream error: %v", e.Err)
					}
				}, firstOptions...)
				require.NoError(t, err)
				wantStarts := 0
				if format == "SSE" {
					wantStarts = 1
				}
				assert.Equal(t, []int{1, 1, 1, 1, wantStarts, 1}, []int{requests, callbacks, responses, closes, starts, terminals})
				encoded, err := json.Marshal(first)
				require.NoError(t, err)
				var history Message
				require.NoError(t, json.Unmarshal(encoded, &history))
				assert.Equal(t, wantEffort, history.ProviderThinkingLevel)
				replay := &Context{Messages: []Message{prompt.Messages[0], history, {Role: "user", Content: "two"}}}
				before, err := json.Marshal(replay)
				require.NoError(t, err)
				nextEffort := "low"
				if wantEffort == "low" {
					nextEffort = "high"
				}
				second, err := client.Complete(t.Context(), model, replay, append(opts, WithEffort(nextEffort))...)
				require.NoError(t, err)
				require.NotNil(t, second)
				check(*second, nextEffort)
				require.Len(t, payloads, 2)
				firstMessages := payloads[0]["messages"].([]any)
				secondMessages := payloads[1]["messages"].([]any)
				require.Len(t, firstMessages, 2)
				require.Len(t, secondMessages, 5)
				assert.Equal(t, firstMessages, secondMessages[:len(firstMessages)])
				for i, body := range payloads {
					assert.Equal(t, map[string]any{"effort": "high"}, body["output_config"])
					assert.Equal(t, map[string]any{"type": "adaptive", "display": "summarized", "block_binding": map[string]any{"prefix_mismatch_behavior": "drop_block"}}, body["thinking"])
					level := wantEffort
					if i == 1 {
						level = nextEffort
					}
					messages := body["messages"].([]any)
					assert.Equal(t, map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"effort": level}}, messages[len(messages)-1])
				}
				assert.Equal(t, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "reasoning", "signature": "opaque-signature"}, map[string]any{"type": "text", "text": "answer"}}}, secondMessages[2])
				assert.Equal(t, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "two"}}}, secondMessages[3])
				unmanaged := *model
				unmanaged.Compat = &CompatibilitySettings{ForceAdaptiveThinking: true}
				third, err := client.Complete(t.Context(), &unmanaged, replay, append(opts, WithEffort("low"))...)
				require.NoError(t, err)
				require.NotNil(t, third)
				check(*third, "")
				require.Len(t, payloads, 3)
				assert.Equal(t, []any{firstMessages[0], secondMessages[2], secondMessages[3]}, payloads[2]["messages"])
				assert.Equal(t, map[string]any{"effort": "low"}, payloads[2]["output_config"])
				assert.Equal(t, map[string]any{"type": "adaptive", "display": "summarized"}, payloads[2]["thinking"])
				assert.Equal(t, []int{3, 3, 3, 3}, []int{requests, callbacks, responses, closes})
				after, err := json.Marshal(replay)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
				modelAfter, err := json.Marshal(model)
				require.NoError(t, err)
				assert.Equal(t, string(modelBefore), string(modelAfter))
				assert.Equal(t, []Message{{Role: "user", Content: "one"}}, prompt.Messages)
			})
		}
	}
}

func TestAnthropicHistoricalEffortAdmissionReachesTransport(t *testing.T) {
	for _, scenario := range []string{"same model", "other model", "missing API", "other API", "missing provider", "other provider", "legacy", "invalid level", "padded level", "filtered content", "error", "aborted"} {
		for _, format := range []string{"JSON", "SSE"} {
			t.Run(scenario+"/"+format, func(t *testing.T) {
				model := &Model{ID: "claude-managed", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Reasoning: true, Compat: &CompatibilitySettings{SupportsMidConvoEffort: true}}
				assistant := func(text, level string) Message {
					return Message{Role: "assistant", API: model.API, Provider: model.Provider, Model: model.ID, ProviderThinkingLevel: level, Content: []ContentBlock{{Type: ContentBlockText, Text: text}}, StopReason: "stop"}
				}
				subject := assistant("subject", "medium")
				switch scenario {
				case "other model":
					subject.Model = "previous-model"
				case "missing API":
					subject.API = ""
				case "other API":
					subject.API = APIOpenAIResponses
				case "missing provider":
					subject.Provider = ""
				case "other provider":
					subject.Provider = ProviderOpenAI
				case "legacy":
					subject.ProviderThinkingLevel = ""
				case "invalid level":
					subject.ProviderThinkingLevel = "HIGH"
				case "padded level":
					subject.ProviderThinkingLevel = " medium "
				case "filtered content":
					subject.Content = []ContentBlock{{Type: ContentBlockText, Text: " \t\n"}}
				case "error", "aborted":
					subject.StopReason = scenario
				}
				prompt := &Context{Messages: []Message{{Role: "user", Content: "one"}, assistant("before", "low"), {Role: "user", Content: "two"}, subject, {Role: "user", Content: "three"}, assistant("after", "high"), {Role: "user", Content: "four"}}}
				before, err := json.Marshal([]any{model, prompt})
				require.NoError(t, err)
				var requests, callbacks, responses, closes, terminals int
				var payload string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					wire, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, payload, string(wire))
					var body map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(wire, &body))
					want := `[{"role":"user","content":[{"type":"text","text":"one"}]},{"role":"system","content":[],"output_config":{"effort":"low"}},{"role":"assistant","content":[{"type":"text","text":"before"}]},{"role":"user","content":[{"type":"text","text":"two"}]},`
					if scenario == "same model" || scenario == "other model" {
						want += `{"role":"system","content":[],"output_config":{"effort":"medium"}},`
					}
					if scenario != "filtered content" && scenario != "error" && scenario != "aborted" {
						want += `{"role":"assistant","content":[{"type":"text","text":"subject"}]},`
					}
					want += `{"role":"user","content":[{"type":"text","text":"three"}]},{"role":"system","content":[],"output_config":{"effort":"high"}},{"role":"assistant","content":[{"type":"text","text":"after"}]},{"role":"user","content":[{"type":"text","text":"four"}]},`
					level := "max"
					if requests == 2 {
						level = "low"
					}
					want += fmt.Sprintf(`{"role":"system","content":[],"output_config":{"effort":%q}}]`, level)
					assert.JSONEq(t, want, string(body["messages"]))
					assert.JSONEq(t, `{"type":"adaptive","display":"summarized","block_binding":{"prefix_mismatch_behavior":"drop_block"}}`, string(body["thinking"]))
					assert.JSONEq(t, `{"effort":"high"}`, string(body["output_config"]))
					assert.Contains(t, req.Header.Get("anthropic-beta"), "mid-conversation-output-config-2026-07-01")
					assert.Contains(t, req.Header.Get("anthropic-beta"), "thinking-binding-controls-2026-08-01")
					response := `{"id":"msg_admission","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
					kind := "application/json"
					if format == "SSE" {
						kind = "text/event-stream"
						response = "data: " + `{"type":"message_start","message":{"id":"msg_admission","usage":{"input_tokens":3}}}` + "\n\n" +
							"data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"answer"}}` + "\n\n" +
							"data: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
							"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
							"data: " + `{"type":"message_stop"}` + "\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {kind}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(response)), closes: &closes}}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(value any) any { callbacks++; payload = value.(string); return nil }), WithResponseCallback(func(response ProviderResponse) { responses++; assert.Equal(t, http.StatusOK, response.Status) })}
				check := func(message AssistantMessage, level string) {
					t.Helper()
					assert.Equal(t, level, message.ProviderThinkingLevel)
					assert.Equal(t, StopReasonStop, message.StopReason)
					assert.Equal(t, "msg_admission", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
					assert.Equal(t, 3, message.Usage.Input)
					assert.Equal(t, 2, message.Usage.Output)
					assert.Equal(t, 5, message.Usage.TotalTokens)
					assert.Empty(t, message.ErrorMessage)
					assert.Empty(t, message.Diagnostics)
				}
				err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case Done:
						terminals++
						check(e.Message, "max")
					case StreamError:
						t.Errorf("unexpected error terminal: %v", e.Err)
					}
				}, append(opts, WithEffort("max"))...)
				require.NoError(t, err)
				assert.Equal(t, 1, terminals)
				message, err := CompleteWithModel(t.Context(), model, prompt, append(opts, WithEffort("low"))...)
				require.NoError(t, err)
				require.NotNil(t, message)
				check(*message, "low")
				assert.Equal(t, []int{2, 2, 2, 2}, []int{requests, callbacks, responses, closes})
				after, err := json.Marshal([]any{model, prompt})
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicSSETerminalOutcomesMatchUpstream(t *testing.T) {
	const (
		start = `data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}`
		text  = `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"partial"}}`
		stop  = `data: {"type":"message_stop"}`
	)
	delta := func(reason string) string {
		return `data: {"type":"message_delta","delta":{"stop_reason":"` + reason + `"}}`
	}
	for _, tc := range []struct {
		name, want string
		lines      []string
		reason     StopReason
		cancel     bool
	}{
		{name: "ok", lines: []string{start, text, delta("end_turn"), stop}, reason: StopReasonStop},
		{name: "pause turn", lines: []string{start, delta("pause_turn"), stop}, reason: StopReasonStop},
		{name: "stop sequence", lines: []string{start, delta("stop_sequence"), stop}, reason: StopReasonStop},
		{name: "no message_stop", lines: []string{start, text, delta("end_turn")}, reason: StopReasonError, want: "anthropic stream ended before message_stop"},
		{name: "no stop reason", lines: []string{start, stop}, reason: StopReasonError, want: "anthropic stream ended without a stop reason"},
		{name: "empty stream", reason: StopReasonError, want: "anthropic stream ended without a stop reason"},
		{name: "bad json", lines: []string{start, `data: {"type":`}, reason: StopReasonError, want: "could not parse anthropic SSE event"},
		{name: "error event", lines: []string{start, `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}, reason: StopReasonError, want: "Overloaded"},
		{name: "refusal", lines: []string{start, `data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"explanation":"nope"}}}`, stop}, reason: StopReasonError, want: "nope"},
		{name: "refusal default", lines: []string{start, delta("refusal"), stop}, reason: StopReasonError, want: "The model refused to complete the request"},
		{name: "sensitive", lines: []string{start, delta("sensitive"), stop}, reason: StopReasonError, want: "Provider stopped with: sensitive"},
		{name: "unknown", lines: []string{start, delta("mystery"), stop}, reason: StopReasonError, want: "unhandled stop reason: mystery"},
		{name: "canceled", lines: []string{start, text, delta("end_turn"), stop}, reason: StopReasonAborted, cancel: true, want: "request was aborted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &anthropicClient{model: &Model{ID: "claude-test", Provider: ProviderAnthropic}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			var terminal []StreamEvent
			err := client.handleSSE(ctx, strings.NewReader(strings.Join(tc.lines, "\n\n")+"\n\n"), func(_ context.Context, event StreamEvent) {
				switch event.(type) {
				case Done, StreamError:
					terminal = append(terminal, event)
				}
			})
			if err != nil {
				t.Fatalf("handleSSE: %v", err)
			}
			if len(terminal) != 1 {
				t.Fatalf("terminal events = %#v, want exactly one", terminal)
			}
			switch e := terminal[0].(type) {
			case Done:
				if tc.want != "" || e.Reason != tc.reason || e.Message.StopReason != tc.reason {
					t.Fatalf("done = %+v, want %s %q", e, tc.reason, tc.want)
				}
			case StreamError:
				if e.Msg.StopReason != tc.reason || !strings.Contains(e.Msg.ErrorMessage, tc.want) || tc.want == "" {
					t.Fatalf("error = %q %s, want %s %q", e.Msg.ErrorMessage, e.Msg.StopReason, tc.reason, tc.want)
				}
				if tc.name == "no message_stop" && (len(e.Msg.Content) != 1 || e.Msg.Content[0].Text != "partial") {
					t.Fatalf("partial content lost: %+v", e.Msg.Content)
				}
			}
		})
	}
}

func TestAnthropicStopReasonsReachTransport(t *testing.T) {
	for _, tc := range []struct {
		name, wire, details, diagnostic string
		reason                          StopReason
	}{
		{name: "end turn", wire: "end_turn", reason: StopReasonStop},
		{name: "pause turn", wire: "pause_turn", reason: StopReasonStop},
		{name: "stop sequence", wire: "stop_sequence", reason: StopReasonStop},
		{name: "token limit", wire: "max_tokens", reason: StopReasonLength},
		{name: "tool use", wire: "tool_use", reason: StopReasonToolUse},
		{name: "refusal explanation", wire: "refusal", details: `{"type":"refusal","category":"cyber","explanation":"blocked\n理由"}`, diagnostic: "blocked\n理由", reason: StopReasonError},
		{name: "refusal absent details", wire: "refusal", diagnostic: "The model refused to complete the request", reason: StopReasonError},
		{name: "refusal null details", wire: "refusal", details: `null`, diagnostic: "The model refused to complete the request", reason: StopReasonError},
		{name: "refusal empty explanation", wire: "refusal", details: `{"explanation":""}`, diagnostic: "The model refused to complete the request", reason: StopReasonError},
		{name: "refusal null explanation", wire: "refusal", details: `{"explanation":null}`, diagnostic: "The model refused to complete the request", reason: StopReasonError},
		{name: "sensitive", wire: "sensitive", details: `{"explanation":"not the sensitive diagnostic"}`, diagnostic: "Provider stopped with: sensitive", reason: StopReasonError},
		{name: "unknown", wire: "future_reason", diagnostic: "unhandled stop reason: future_reason", reason: StopReasonError},
	} {
		for _, format := range []string{"JSON", "SSE"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				var requests, callbacks, responses, closes, terminals int
				var payload string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					wire, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, payload, string(wire))
					stop := fmt.Sprintf(`"stop_reason":%q`, tc.wire)
					if tc.details != "" {
						stop += `,"stop_details":` + tc.details
					}
					body := `{"id":"msg_stop","content":[{"type":"text","text":"partial"}],` + stop + `,"usage":{"input_tokens":3,"output_tokens":2}}`
					contentType := "application/json"
					if format == "SSE" {
						contentType = "text/event-stream"
						body = "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_stop","usage":{"input_tokens":3}}}` + "\n\n" +
							"event: content_block_start\ndata: " + `{"type":"content_block_start","index":3,"content_block":{"type":"text","text":"partial"}}` + "\n\n" +
							"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":3}` + "\n\n" +
							"event: message_delta\ndata: " + `{"type":"message_delta","delta":{` + stop + `},"usage":{"output_tokens":2}}` + "\n\n" +
							"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(body)), closes: &closes}}, nil
				})}
				model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				modelBefore, err := json.Marshal(model)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithPayloadCallback(func(value any) any { callbacks++; payload = value.(string); return nil }), WithResponseCallback(func(response ProviderResponse) { responses++; assert.Equal(t, http.StatusOK, response.Status) })}
				check := func(message AssistantMessage) {
					t.Helper()
					assert.Equal(t, tc.reason, message.StopReason)
					assert.Equal(t, tc.diagnostic, message.ErrorMessage)
					assert.Equal(t, "msg_stop", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "partial"}}, message.Content)
					assert.Equal(t, 3, message.Usage.Input)
					assert.Equal(t, 2, message.Usage.Output)
					assert.Equal(t, 5, message.Usage.TotalTokens)
					assert.NotNil(t, message.DurationMs)
					assert.Empty(t, message.Diagnostics)
				}
				err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case Done:
						terminals++
						assert.Empty(t, tc.diagnostic)
						assert.Equal(t, tc.reason, e.Reason)
						check(e.Message)
					case StreamError:
						terminals++
						require.NotEmpty(t, tc.diagnostic)
						require.Error(t, e.Err)
						assert.Equal(t, tc.diagnostic, e.Err.Error())
						assert.Equal(t, string(tc.reason), e.Reason)
						check(e.Msg)
					}
				}, opts...)
				require.NoError(t, err)
				assert.Equal(t, 1, terminals)
				message, err := client.Complete(t.Context(), model, prompt, opts...)
				require.NoError(t, err)
				require.NotNil(t, message)
				check(*message)
				assert.Equal(t, []int{2, 2, 2, 2}, []int{requests, callbacks, responses, closes})
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
				modelAfter, err := json.Marshal(model)
				require.NoError(t, err)
				assert.Equal(t, string(modelBefore), string(modelAfter))
			})
		}
	}
}

func TestAnthropicFailedTurnReplayReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name, ending, diagnostic string
		reason                   StopReason
	}{
		{name: "provider error", diagnostic: "overloaded", reason: StopReasonError},
		{name: "truncated", diagnostic: "anthropic stream ended before message_stop", reason: StopReasonError},
		{name: "cancelled", diagnostic: "request was aborted: replay cancelled", reason: StopReasonAborted},
		{name: "JSON refusal", ending: "refusal", diagnostic: "The model refused to complete the request", reason: StopReasonError},
		{name: "JSON length retained", ending: "max_tokens", reason: StopReasonLength},
		{name: "JSON tool use retained", ending: "tool_use", reason: StopReasonToolUse},
	} {
		for _, mode := range []string{"same model", "cross model"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				model := &Model{ID: "source", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
				target := *model
				if mode == "cross model" {
					target.ID = "target"
				}
				const blocks = `[{"type":"thinking","thinking":"partial reason","signature":"opaque"},{"type":"text","text":"partial text"},{"type":"tool_use","id":"partial_call","name":"lookup","input":{"n":9007199254740993}}]`
				body, kind := `{"id":"msg_partial","content":`+blocks+`,"stop_reason":"`+tc.ending+`","usage":{"input_tokens":3,"output_tokens":2}}`, "application/json"
				if tc.ending == "" {
					kind = "text/event-stream"
					body = "data: " + `{"type":"message_start","message":{"id":"msg_partial","usage":{"input_tokens":3,"output_tokens":2}}}` + "\n\n"
					var content []json.RawMessage
					require.NoError(t, json.Unmarshal([]byte(blocks), &content))
					for i, block := range content {
						body += fmt.Sprintf("data: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":%s}\n\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i, block, i)
					}
					if tc.name == "provider error" {
						body += "data: " + `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}` + "\n\n"
					}
				}
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				cause := errors.New("replay cancelled")
				var requests, callbacks, responses, closes, terminals int
				var payload string
				var expectedReplay string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					data, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, payload, string(data))
					var request struct {
						Model    string
						Messages json.RawMessage
					}
					require.NoError(t, json.Unmarshal(data, &request))
					responseBody, responseKind := body, kind
					if requests == 1 {
						assert.Equal(t, model.ID, request.Model)
						assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(request.Messages))
					} else {
						assert.Equal(t, target.ID, request.Model)
						assert.JSONEq(t, expectedReplay, string(request.Messages))
						responseBody, responseKind = `{"id":"msg_retry","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":4}}`, "application/json"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {responseKind}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(responseBody)), closes: &closes}}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(value any) any { callbacks++; payload = value.(string); return nil }), WithResponseCallback(func(response ProviderResponse) { responses++; assert.Equal(t, http.StatusOK, response.Status) }), WithClient(client)}
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				modelBefore, err := json.Marshal(model)
				require.NoError(t, err)
				targetBefore, err := json.Marshal(target)
				require.NoError(t, err)
				var returned AssistantMessage
				err = StreamWithModel(ctx, model, prompt, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case ToolCallEnd:
						if tc.name == "cancelled" {
							cancel(cause)
						}
					case Done:
						terminals++
						assert.Empty(t, tc.diagnostic)
						assert.Equal(t, tc.reason, e.Reason)
						returned = e.Message
					case StreamError:
						terminals++
						assert.Contains(t, e.Err.Error(), tc.diagnostic)
						if tc.name == "cancelled" {
							assert.ErrorIs(t, e.Err, cause)
						}
						returned = e.Msg
					}
				}, opts...)
				require.NoError(t, err)
				assert.Equal(t, 1, terminals)
				assert.Equal(t, tc.reason, returned.StopReason)
				if tc.diagnostic == "" {
					assert.Empty(t, returned.ErrorMessage)
				} else {
					assert.Contains(t, returned.ErrorMessage, tc.diagnostic)
				}
				assert.Equal(t, []ContentBlock{{Type: ContentBlockThinking, Thinking: "partial reason", ThoughtSignature: "opaque"}, {Type: ContentBlockText, Text: "partial text"}, {Type: ContentBlockToolCall, ID: "partial_call", Name: "lookup", Arguments: json.RawMessage(`{"n":9007199254740993}`)}}, returned.Content)
				assert.Equal(t, "msg_partial", returned.ResponseID)
				assert.Equal(t, model.ID, returned.Model)
				assert.Equal(t, model.Provider, returned.Provider)
				assert.Equal(t, model.API, returned.API)
				assert.Equal(t, 3, returned.Usage.Input)
				assert.Equal(t, 2, returned.Usage.Output)
				encoded, err := json.Marshal(returned)
				require.NoError(t, err)
				var restored Message
				require.NoError(t, json.Unmarshal(encoded, &restored))
				prompt.Messages = append(prompt.Messages, Message{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockToolCall, ID: "prior_call", Name: "lookup", Arguments: json.RawMessage(`{}`)}}, StopReason: string(StopReasonToolUse), Model: model.ID, Provider: model.Provider, API: model.API}, restored, Message{Role: "user", Content: "retry"})
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				expectedReplay = `[{"role":"user","content":[{"type":"text","text":"question"}]},{"role":"assistant","content":[{"type":"tool_use","id":"prior_call","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"prior_call","content":"No result provided","is_error":true}]}`
				if tc.diagnostic == "" {
					thinking := `{"type":"thinking","thinking":"partial reason","signature":"opaque"}`
					if mode == "cross model" {
						thinking = `{"type":"text","text":"partial reason"}`
					}
					expectedReplay += `,{"role":"assistant","content":[` + thinking + `,{"type":"text","text":"partial text"},{"type":"tool_use","id":"partial_call","name":"lookup","input":{"n":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"partial_call","content":"No result provided","is_error":true}]}`
				}
				expectedReplay += `,{"role":"user","content":[{"type":"text","text":"retry"}]}]`
				targetClient, err := newAnthropicClient(&target)
				require.NoError(t, err)
				result, err := CompleteWithModel(t.Context(), &target, prompt, append(opts, WithClient(targetClient))...)
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Equal(t, StopReasonStop, result.StopReason)
				assert.Equal(t, "msg_retry", result.ResponseID)
				assert.Equal(t, target.ID, result.Model)
				assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, result.Content)
				assert.Empty(t, result.ErrorMessage)
				assert.Equal(t, 5, result.Usage.Input)
				assert.Equal(t, 4, result.Usage.Output)
				assert.Equal(t, []int{2, 2, 2, 2}, []int{requests, callbacks, responses, closes})
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
				returnedAfter, err := json.Marshal(returned)
				require.NoError(t, err)
				assert.Equal(t, string(encoded), string(returnedAfter))
				modelAfter, err := json.Marshal(model)
				require.NoError(t, err)
				assert.Equal(t, string(modelBefore), string(modelAfter))
				targetAfter, err := json.Marshal(target)
				require.NoError(t, err)
				assert.Equal(t, string(targetBefore), string(targetAfter))
			})
		}
	}
}

func TestAnthropicBudgetThinkingFitsResponseCeilingLikeUpstream(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		modelMax, window, base int
		reasoning              ReasoningLevel
		wantMax, wantBudget    int
	}{
		{name: "budget fills the model cap", modelMax: 8192, base: 8192, reasoning: ReasoningMedium, wantMax: 8192, wantBudget: 7168},
		{name: "explicit cap grows by the budget", modelMax: 64000, base: 4000, reasoning: ReasoningLow, wantMax: 6048, wantBudget: 2048},
		{name: "context room limits the ceiling", modelMax: 64000, window: 20002, base: 64000, reasoning: ReasoningMedium, wantMax: 15904, wantBudget: 8192},
		{name: "no answer room leaves no budget", modelMax: 64000, window: 5002, base: 64000, reasoning: ReasoningHigh, wantMax: 904, wantBudget: 0},
		{name: "exact answer reserve", modelMax: 64000, window: 5122, base: 64000, reasoning: ReasoningLow, wantMax: 1024, wantBudget: 0},
		{name: "exhausted context keeps one token", modelMax: 64000, window: 4098, base: 64000, reasoning: ReasoningLow, wantMax: 1, wantBudget: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &Model{ID: "claude-sonnet-4-5", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", Reasoning: true, MaxTokens: tc.modelMax, ContextWindow: tc.window}
			prompt := &Context{Messages: []Message{{Role: "user", Content: "1234567"}}}
			before, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			var requests, payloads, responses, closes, terminals int
			var payload string
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				assert.Equal(t, requests-1, closes)
				assert.Equal(t, http.MethodPost, req.Method)
				assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
				assert.Equal(t, "test-key", req.Header.Get("x-api-key"))
				assert.Equal(t, "2023-06-01", req.Header.Get("anthropic-version"))
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, payload, string(wire))
				var body map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(wire, &body))
				assert.JSONEq(t, `"claude-sonnet-4-5"`, string(body["model"]))
				assert.Equal(t, "true", string(body["stream"]))
				assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"1234567"}]}]`, string(body["messages"]))
				assert.NotContains(t, body, "output_config")
				if requests == 3 {
					assert.Equal(t, "400", string(body["max_tokens"]))
					assert.JSONEq(t, `{"type":"disabled"}`, string(body["thinking"]))
					assert.Equal(t, "0.4", string(body["temperature"]))
					assert.Empty(t, req.Header.Get("anthropic-beta"))
				} else {
					assert.Equal(t, fmt.Sprint(tc.wantMax), string(body["max_tokens"]))
					var thinking struct {
						Type    string `json:"type"`
						Budget  int    `json:"budget_tokens"`
						Display string `json:"display"`
					}
					require.NoError(t, json.Unmarshal(body["thinking"], &thinking))
					assert.Equal(t, "enabled", thinking.Type)
					assert.Equal(t, tc.wantBudget, thinking.Budget)
					assert.Equal(t, "summarized", thinking.Display)
					assert.NotContains(t, body, "temperature")
					assert.Equal(t, "interleaved-thinking-2025-05-14", req.Header.Get("anthropic-beta"))
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(`{"id":"msg_ceiling","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)), closes: &closes}}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithTemperature(0.4), WithPayloadCallbackForModel(func(value any, got *Model) any {
				payloads++
				assert.Same(t, model, got)
				payload = value.(string)
				return nil
			}), WithResponseCallback(func(response ProviderResponse) {
				responses++
				assert.Equal(t, http.StatusOK, response.Status)
				assert.Equal(t, requests-1, closes)
			})}
			budgetOpts := append(slices.Clone(opts), WithMaxTokens(tc.base), WithReasoning(tc.reasoning))
			check := func(message AssistantMessage) {
				t.Helper()
				assert.Equal(t, StopReasonStop, message.StopReason)
				assert.Equal(t, "msg_ceiling", message.ResponseID)
				assert.Equal(t, model.ID, message.Model)
				assert.Equal(t, model.API, message.API)
				assert.Equal(t, model.Provider, message.Provider)
				assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
				assert.Equal(t, Usage{Input: 3, Output: 2, TotalTokens: 5}, message.Usage)
				assert.Empty(t, message.Diagnostics)
				assert.Empty(t, message.ErrorMessage)
			}
			require.NoError(t, StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
				done, ok := event.(Done)
				require.True(t, ok, "unexpected %T", event)
				terminals++
				assert.Equal(t, 1, responses)
				check(done.Message)
			}, budgetOpts...))
			message, err := CompleteWithModel(t.Context(), model, prompt, budgetOpts...)
			require.NoError(t, err)
			require.NotNil(t, message)
			check(*message)
			message, err = CompleteWithModel(t.Context(), model, prompt, append(slices.Clone(opts), WithMaxTokens(400), WithReasoning(ReasoningOff))...)
			require.NoError(t, err)
			require.NotNil(t, message)
			check(*message)
			assert.Equal(t, []int{3, 3, 3, 3, 1}, []int{requests, payloads, responses, closes, terminals})
			assert.Equal(t, http.Header{"Content-Type": {"application/json"}, "anthropic-version": {"2023-06-01"}}, client.transport.headers)
			after, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}

func TestAnthropicHeaderTimeout(t *testing.T) {
	model := &Model{ID: "test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid"}
	client, err := newAnthropicClient(model)
	if err != nil {
		t.Fatal(err)
	}
	testHTTPProviderHeaderTimeout(t, client, model, `{"content":[],"stop_reason":"end_turn"}`)
}

func TestAnthropicAuthHeadersAndInjectedRefreshMatchUpstream(t *testing.T) {
	for _, credential := range []struct {
		name, provider, key, refreshed string
		header, value                  string
	}{
		{name: "api key", provider: ProviderAnthropic, key: "sk-ant-api-old", refreshed: "sk-ant-api-new", header: "x-api-key", value: "%s"},
		{name: "copilot", provider: ProviderGitHubCopilot, key: "copilot-old", refreshed: "copilot-new", header: "Authorization", value: "Bearer %s"},
	} {
		for _, outcome := range []string{"success", "close failure", "refresh failure", "second unauthorized"} {
			t.Run(credential.name+"/"+outcome, func(t *testing.T) {
				model := &Model{ID: "claude-refresh", API: APIAnthropicMessages, Provider: credential.provider, BaseURL: "https://test.invalid", MaxTokens: 4096}
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				before, err := json.Marshal([]any{model, prompt})
				require.NoError(t, err)
				closeErr, refreshErr := errors.New("unauthorized cleanup failed"), errors.New("credential exchange failed")
				var requests, payloads, responses, closes, refreshes, attempt int
				var payload string
				reset := false
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					attempt++
					assert.Equal(t, requests-1, closes)
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					assert.Equal(t, http.MethodPost, req.Method)
					assert.NoError(t, req.Context().Err())
					wire, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, payload, string(wire))
					var request struct{ Messages json.RawMessage }
					require.NoError(t, json.Unmarshal(wire, &request))
					assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(request.Messages))
					key := credential.key
					if attempt == 2 {
						key = credential.refreshed
					}
					assert.Equal(t, fmt.Sprintf(credential.value, key), req.Header.Get(credential.header))
					other := "Authorization"
					if credential.header == other {
						other = "x-api-key"
					}
					assert.Empty(t, req.Header.Get(other))
					assert.Empty(t, req.Header.Get("anthropic-dangerous-direct-password-access"))
					status := http.StatusOK
					body := `{"id":"msg_refresh","content":[{"type":"text","text":"answer"}],"usage":{"input_tokens":11,"output_tokens":7},"stop_reason":"end_turn"}`
					var bodyCloseErr error
					if !reset && (attempt == 1 || outcome == "second unauthorized") {
						status = http.StatusUnauthorized
						body = `{"error":{"type":"authentication_error","message":"first unauthorized"}}`
						if attempt == 2 {
							body = `{"error":{"type":"authentication_error","message":"refreshed unauthorized"}}`
						}
						if outcome == "close failure" {
							bodyCloseErr = closeErr
						}
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(body)), closes: &closes, closeErr: bodyCloseErr}}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				client.SetTransportOption(TransportOption{Provider: model.Provider, RefreshCreds: func(ctx context.Context, provider string) (string, error) {
					refreshes++
					assert.Equal(t, model.Provider, provider)
					assert.Equal(t, requests, closes)
					assert.Equal(t, 1, attempt)
					assert.NoError(t, ctx.Err())
					if outcome == "refresh failure" {
						return "", refreshErr
					}
					return credential.refreshed, nil
				}})
				opts := []Option{WithClient(client), WithAPIKey(credential.key), WithMaxRetries(0), WithCacheRetention("none"), WithPayloadCallback(func(value any) any {
					payloads++
					payload = value.(string)
					return nil
				}), WithResponseCallback(func(response ProviderResponse) {
					responses++
					assert.Equal(t, http.StatusOK, response.Status)
					assert.Equal(t, requests-1, closes)
				})}
				check := func(message *AssistantMessage) {
					require.NotNil(t, message)
					assert.Equal(t, "msg_refresh", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, StopReasonStop, message.StopReason)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
					assert.Equal(t, 11, message.Usage.Input)
					assert.Equal(t, 7, message.Usage.Output)
				}
				for _, api := range []string{"Stream", "Complete"} {
					attempt = 0
					var message *AssistantMessage
					if api == "Stream" {
						var events int
						err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
							events++
							terminal, ok := event.(Done)
							require.True(t, ok, "unexpected event %T", event)
							assert.Equal(t, 1, responses)
							assert.Equal(t, requests-1, closes)
							message = &terminal.Message
						}, opts...)
						if outcome == "success" {
							assert.Equal(t, 1, events)
						} else {
							assert.Zero(t, events)
						}
					} else {
						message, err = CompleteWithModel(t.Context(), model, prompt, opts...)
					}
					switch outcome {
					case "success":
						require.NoError(t, err)
						check(message)
					case "close failure":
						assert.Nil(t, message)
						assert.ErrorIs(t, err, closeErr)
						assert.ErrorContains(t, err, "close unauthorized response body")
					case "refresh failure":
						assert.Nil(t, message)
						assert.ErrorIs(t, err, refreshErr)
						assert.ErrorContains(t, err, "credential refresh failed")
					case "second unauthorized":
						assert.Nil(t, message)
						var rejected *providerStatusError
						require.ErrorAs(t, err, &rejected)
						assert.Equal(t, http.StatusUnauthorized, rejected.status)
						assert.Equal(t, `{"error":{"type":"authentication_error","message":"refreshed unauthorized"}}`, string(rejected.body))
					}
				}
				wantRequests, wantRefreshes, wantResponses := 4, 2, 0
				if outcome == "close failure" || outcome == "refresh failure" {
					wantRequests = 2
				}
				if outcome == "close failure" {
					wantRefreshes = 0
				}
				if outcome == "success" {
					wantResponses = 2
				}
				assert.Equal(t, []int{wantRequests, wantRequests, 2, wantRefreshes, wantResponses}, []int{requests, closes, payloads, refreshes, responses})
				reset, attempt = true, 0
				message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
				require.NoError(t, err)
				check(message)
				assert.Equal(t, []int{wantRequests + 1, wantRequests + 1, 3, wantRefreshes, wantResponses + 1}, []int{requests, closes, payloads, refreshes, responses})
				assert.Empty(t, client.transport.apiKey)
				assert.Empty(t, client.transport.headers.Get("x-api-key"))
				assert.Empty(t, client.transport.headers.Get("Authorization"))
				after, err := json.Marshal([]any{model, prompt})
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicSharedThinkingBudgets(t *testing.T) {
	model := &Model{ID: "claude-sonnet-4-5", MaxTokens: 32000}
	for _, level := range []ReasoningLevel{ReasoningLow, ReasoningXHigh, ReasoningMax, ReasoningOff} {
		t.Run(string(level), func(t *testing.T) {
			body, err := (&anthropicClient{}).buildRequest(model, &Context{}, applyOptions([]Option{WithMaxTokens(4000), WithReasoning(level), WithThinkingBudgets(&ThinkingBudgets{Low: new(0), High: new(3000)})}))
			require.NoError(t, err)
			var req anthropicRequest
			require.NoError(t, json.Unmarshal([]byte(body), &req))
			if level == ReasoningOff {
				require.Nil(t, req.Thinking)
				return
			}
			want := 3000
			if level == ReasoningLow {
				want = 0
			}
			require.NotNil(t, req.Thinking)
			require.Equal(t, want, req.Thinking.BudgetTokens)
			require.Equal(t, 4000+want, req.MaxTokens)
		})
	}
}

func TestAnthropicSessionAffinityReachesTransport(t *testing.T) {
	for _, scenario := range []struct {
		name, provider, baseURL, format, retention, key, session string
		enabled                                                  bool
		modelHeaders, requestHeaders, want                       map[string]string
	}{
		{name: "native default"},
		{name: "opt in", enabled: true, want: map[string]string{"x-session-affinity": "session"}},
		{name: "openai", enabled: true, format: "openai", want: map[string]string{"x-session-affinity": "session"}},
		{name: "openai nosession", enabled: true, format: "openai-nosession", want: map[string]string{"x-session-affinity": "session"}},
		{name: "explicit router", enabled: true, format: "openrouter", want: map[string]string{"x-session-id": "session"}},
		{name: "router provider", enabled: true, provider: ProviderOpenRouter, want: map[string]string{"x-session-id": "session"}},
		{name: "router URL", enabled: true, baseURL: "https://openrouter.ai/api", want: map[string]string{"x-session-id": "session"}},
		{name: "override router detection", enabled: true, provider: ProviderOpenRouter, format: "openai", want: map[string]string{"x-session-affinity": "session"}},
		{name: "router disabled", provider: ProviderOpenRouter, format: "openrouter"},
		{name: "none overrides environment", enabled: true, retention: "none"},
		{name: "empty session", enabled: true, session: "empty"},
		{name: "short overrides environment", enabled: true, retention: "short", want: map[string]string{"x-session-affinity": "session"}},
		{name: "unclamped identity", enabled: true, session: strings.Repeat("s", 65), want: map[string]string{"x-session-affinity": strings.Repeat("s", 65)}},
		{name: "configured precedence", enabled: true, modelHeaders: map[string]string{"X-Session-Affinity": "model", "X-Extra": "model-extra"}, requestHeaders: map[string]string{"x-session-affinity": "caller"}, want: map[string]string{"x-session-affinity": "caller", "x-extra": "model-extra"}},
		{name: "router configured precedence", enabled: true, format: "openrouter", modelHeaders: map[string]string{"X-Session-ID": "model"}, requestHeaders: map[string]string{"x-session-id": "caller"}, want: map[string]string{"x-session-id": "caller"}},
		{name: "explicit header with none", enabled: true, retention: "none", requestHeaders: map[string]string{"X-Session-Affinity": "caller"}, want: map[string]string{"x-session-affinity": "caller"}},
		{name: "Copilot excludes generated affinity", enabled: true, provider: ProviderGitHubCopilot},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("PI_CACHE_RETENTION", "long")
			original := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = original })
			var requests, callbacks, done int
			baseURL := scenario.baseURL
			if baseURL == "" {
				baseURL = "https://test.invalid"
			}
			key := scenario.key
			if key == "" {
				key = "test-key"
			}
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				require.Equal(t, baseURL+"/v1/messages", req.URL.String())
				for _, name := range []string{"x-session-id", "x-session-affinity", "session_id", "session-id", "x-client-request-id", "x-extra"} {
					assert.Equal(t, scenario.want[name], req.Header.Get(name), name)
				}
				if scenario.provider == ProviderGitHubCopilot {
					require.Equal(t, "Bearer "+key, req.Header.Get("Authorization"))
					require.Empty(t, req.Header.Get("x-api-key"))
				} else {
					require.Equal(t, key, req.Header.Get("x-api-key"))
					require.Empty(t, req.Header.Get("Authorization"))
				}
				var body map[string]any
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				require.Equal(t, "claude-test", body["model"])
				require.Equal(t, true, body["stream"])
				require.Len(t, body["messages"], 1)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"content":[],"stop_reason":"end_turn"}`))}, nil
			})}
			model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: scenario.provider, BaseURL: baseURL, Headers: scenario.modelHeaders}
			if model.Provider == "" {
				model.Provider = ProviderAnthropic
			}
			model.Compat = &CompatibilitySettings{SendSessionAffinityHeaders: scenario.enabled, SessionAffinityFormat: scenario.format}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			session := scenario.session
			if session == "" {
				session = "session"
			} else if session == "empty" {
				session = ""
			}
			modelBefore, err := json.Marshal(model)
			require.NoError(t, err)
			headersBefore, err := json.Marshal(scenario.requestHeaders)
			require.NoError(t, err)
			err = client.Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hello"}}}, func(_ context.Context, event StreamEvent) {
				if _, ok := event.(Done); ok {
					done++
				}
			}, WithAPIKey(key), WithSessionID(session), WithCacheRetention(scenario.retention), WithHeaders(scenario.requestHeaders), WithPayloadCallback(func(payload any) any {
				callbacks++
				require.IsType(t, "", payload)
				return nil
			}))
			require.NoError(t, err)
			require.Equal(t, 1, requests)
			require.Equal(t, 1, callbacks)
			require.Equal(t, 1, done)
			modelAfter, err := json.Marshal(model)
			require.NoError(t, err)
			headersAfter, err := json.Marshal(scenario.requestHeaders)
			require.NoError(t, err)
			require.Equal(t, string(modelBefore), string(modelAfter))
			require.Equal(t, string(headersBefore), string(headersAfter))
		})
	}
}

func TestAnthropicCachePlacementReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name, retention, env, compat, shape, key string
	}{
		{name: "default"},
		{name: "environment long", env: "long"},
		{name: "explicit long", retention: "long"},
		{name: "short overrides environment", retention: "short", env: "long"},
		{name: "none overrides environment", retention: "none", env: "long"},
		{name: "long unsupported", retention: "long", compat: `{"supportsLongCacheRetention":false}`},
		{name: "tools unsupported", retention: "long", compat: `{"supportsCacheControlOnTools":false}`},
		{name: "explicit capabilities", retention: "long", compat: `{"supportsLongCacheRetention":true,"supportsCacheControlOnTools":true}`},
		{name: "null capabilities", retention: "long", compat: `{"supportsLongCacheRetention":null,"supportsCacheControlOnTools":null}`},
		{name: "image result", shape: "image"},
		{name: "empty error result", shape: "empty"},
		{name: "legacy tool role", shape: "legacy"},
		{name: "historical results then user", shape: "user"},
		{name: "historical results then assistant", shape: "assistant"},
		{name: "multiple result groups", shape: "groups"},
		{name: "user image", shape: "user image"},
		{name: "native tool update", shape: "system", retention: "long", compat: `{"supportsMidConvoSystemMessages":true,"supportsMidConvoToolChanges":true}`},
		{name: "interleaved system update", shape: "interleaved system", compat: `{"supportsMidConvoSystemMessages":true,"supportsMidConvoToolChanges":true}`},
		{name: "system before missing result", shape: "system missing", compat: `{"supportsMidConvoSystemMessages":true,"supportsMidConvoToolChanges":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PI_CACHE_RETENTION", tc.env)
			model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Input: []InputType{InputText, InputImage}}
			if tc.compat != "" {
				require.NoError(t, json.Unmarshal([]byte(tc.compat), &model.Compat))
			}
			tools := []Tool{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}, {Name: "list", Parameters: json.RawMessage(`{"type":"object"}`)}}
			assistant := Message{Role: "assistant", API: model.API, Provider: model.Provider, Model: model.ID, Content: []ContentBlock{
				{Type: ContentBlockToolCall, ID: "a", Name: "read", Arguments: json.RawMessage(`{}`)},
				{Type: ContentBlockToolCall, ID: "b", Name: "list", Arguments: json.RawMessage(`{}`)},
			}}
			prompt := &Context{SystemPrompt: "base", Tools: tools, Messages: []Message{
				{Role: "user", Content: "hello"},
				assistant,
				{Role: "toolResult", ToolCallID: "a", ToolName: "read", Content: "first"},
				{Role: "toolResult", ToolCallID: "b", ToolName: "list", Content: []ContentBlock{{Type: ContentBlockText, Text: "last"}}},
			}}
			lastPath := "/messages/2/content/1"
			switch tc.shape {
			case "image":
				prompt.Messages[3].Content = []ContentBlock{{Type: ContentBlockText, Text: "image"}, {Type: ContentBlockImage, MimeType: "image/png", Data: "aGVsbG8="}}
			case "empty":
				prompt.Messages[3].Content, prompt.Messages[3].IsError = "", true
			case "legacy":
				prompt.Messages[3].Role = "tool"
			case "user", "user image":
				content := []ContentBlock{{Type: ContentBlockText, Text: "next"}}
				if tc.shape == "user image" {
					content = append(content, ContentBlock{Type: ContentBlockImage, MimeType: "image/png", Data: "aGVsbG8="})
					lastPath = "/messages/3/content/1"
				} else {
					lastPath = "/messages/3/content/0"
				}
				prompt.Messages = append(prompt.Messages, Message{Role: "user", Content: content})
			case "assistant":
				lastPath = ""
				prompt.Messages = append(prompt.Messages, Message{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockText, Text: "done"}}})
			case "groups":
				lastPath = "/messages/5/content/1"
				prompt.Messages = append(prompt.Messages, Message{Role: "user", Content: "next"}, assistant, prompt.Messages[2], prompt.Messages[3])
			case "system", "interleaved system", "system missing":
				lastPath = "/messages/3/content/1"
				update := Message{Role: "system", Content: "update", ToolsAdded: []Tool{{Name: "grep", Parameters: json.RawMessage(`{"type":"object"}`)}}}
				last := prompt.Messages[3]
				if tc.shape != "system" {
					prompt.Messages = prompt.Messages[:3]
				}
				prompt.Messages = append(prompt.Messages, update)
				if tc.shape == "interleaved system" {
					prompt.Messages = append(prompt.Messages, last)
				}
			}
			key := tc.key
			if key == "" {
				key = "test-key"
			}
			wantPaths := []string{"/system/0", "/tools/1"}
			if model.Compat != nil && model.Compat.SupportsCacheControlOnTools != nil && !*model.Compat.SupportsCacheControlOnTools {
				wantPaths = wantPaths[:1]
			}
			if lastPath != "" {
				wantPaths = append(wantPaths, lastPath)
			}
			if tc.retention == "none" {
				wantPaths = nil
			}
			wantCache := map[string]any{"type": "ephemeral"}
			if (tc.retention == "long" || tc.retention == "" && tc.env == "long") && (model.Compat == nil || model.Compat.SupportsLongCacheRetention == nil || *model.Compat.SupportsLongCacheRetention) {
				wantCache["ttl"] = "1h"
			}
			check := func(body map[string]any) {
				t.Helper()
				var paths []string
				var walk func(any, string)
				walk = func(value any, path string) {
					switch value := value.(type) {
					case map[string]any:
						for name, child := range value {
							if name == "cache_control" {
								paths = append(paths, path)
								assert.Equal(t, wantCache, child, path)
							} else {
								walk(child, path+"/"+name)
							}
						}
					case []any:
						for i, child := range value {
							walk(child, fmt.Sprintf("%s/%d", path, i))
						}
					}
				}
				walk(body, "")
				assert.ElementsMatch(t, wantPaths, paths)
				require.Equal(t, model.ID, body["model"])
				require.Equal(t, true, body["stream"])
				require.EqualValues(t, 4096, body["max_tokens"])
				messages := body["messages"].([]any)
				results := messages[2].(map[string]any)["content"].([]any)
				require.Len(t, results, 2)
				require.Equal(t, "a", results[0].(map[string]any)["tool_use_id"])
				require.Equal(t, "first", results[0].(map[string]any)["content"])
				require.Equal(t, "b", results[1].(map[string]any)["tool_use_id"])
				if tc.shape == "image" {
					blocks := results[1].(map[string]any)["content"].([]any)
					require.Len(t, blocks, 2)
					require.Equal(t, "image", blocks[1].(map[string]any)["type"])
				}
				if tc.shape == "user image" {
					blocks := messages[3].(map[string]any)["content"].([]any)
					require.Len(t, blocks, 2)
					require.Equal(t, "image", blocks[1].(map[string]any)["type"])
				}
				if tc.shape == "interleaved system" || tc.shape == "system missing" {
					require.Len(t, messages, 4)
					require.Equal(t, "user", messages[2].(map[string]any)["role"])
					system := messages[3].(map[string]any)
					require.Equal(t, "system", system["role"])
					blocks := system["content"].([]any)
					require.Len(t, blocks, 2)
					require.Equal(t, "update", blocks[0].(map[string]any)["text"])
					require.Equal(t, "tool_addition", blocks[1].(map[string]any)["type"])
					if tc.shape == "system missing" {
						require.Equal(t, true, results[1].(map[string]any)["is_error"])
					} else {
						require.Equal(t, "last", results[1].(map[string]any)["content"])
					}
				}
				if tc.shape == "legacy" {
					require.Equal(t, "last", results[1].(map[string]any)["content"])
					require.NotContains(t, results[1].(map[string]any), "is_error")
				}
				if tc.shape == "empty" {
					require.Equal(t, "", results[1].(map[string]any)["content"])
					require.Equal(t, true, results[1].(map[string]any)["is_error"])
				}
			}
			before, err := json.Marshal(prompt)
			require.NoError(t, err)
			var requests, callbacks, done int
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				require.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
				var body map[string]any
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				check(body)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"content":[],"stop_reason":"end_turn"}`))}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
				if _, ok := event.(Done); ok {
					done++
				}
			}, WithAPIKey(key), WithCacheRetention(tc.retention), WithPayloadCallback(func(payload any) any {
				callbacks++
				var body map[string]any
				require.NoError(t, json.Unmarshal([]byte(payload.(string)), &body))
				check(body)
				return nil
			}))
			require.NoError(t, err)
			require.Equal(t, 1, requests)
			require.Equal(t, 1, callbacks)
			require.Equal(t, 1, done)
			after, err := json.Marshal(prompt)
			require.NoError(t, err)
			require.JSONEq(t, string(before), string(after))
		})
	}
}

func TestAnthropicImageCapabilitiesReachTransport(t *testing.T) {
	for _, input := range [][]InputType{{InputText}, nil, {InputText, InputImage}} {
		for _, role := range []string{"toolResult", "tool"} {
			t.Run(fmt.Sprint(input)+"/"+role, func(t *testing.T) {
				model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Input: input}
				image := ContentBlock{Type: ContentBlockImage, MimeType: "image/png", Data: "aGVsbG8="}
				prompt := &Context{Messages: []Message{
					{Role: "user", Content: []ContentBlock{image, image}},
					{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockToolCall, ID: "call|foreign", Name: "read", Arguments: json.RawMessage(`{}`)}}},
					{Role: role, ToolCallID: "call|foreign", ToolName: "read", IsError: true, Content: []ContentBlock{image, image}},
				}}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				check := func(body map[string]any) {
					require.Equal(t, model.ID, body["model"])
					require.Equal(t, true, body["stream"])
					messages := body["messages"].([]any)
					require.Len(t, messages, 3)
					user := messages[0].(map[string]any)["content"].([]any)
					calls := messages[1].(map[string]any)["content"].([]any)
					results := messages[2].(map[string]any)["content"].([]any)
					require.Len(t, calls, 1)
					require.Len(t, results, 1)
					result := results[0].(map[string]any)
					require.Equal(t, calls[0].(map[string]any)["id"], result["tool_use_id"])
					require.Equal(t, true, result["is_error"])
					if model.SupportsInput(InputImage) {
						tool := result["content"].([]any)
						require.Len(t, tool, 3)
						require.Equal(t, map[string]any{"type": "text", "text": "(see attached image)"}, tool[0])
						for _, blocks := range [][]any{user, tool[1:]} {
							require.Len(t, blocks, 2)
							for _, block := range blocks {
								object := block.(map[string]any)
								require.Equal(t, "image", object["type"])
								source := object["source"].(map[string]any)
								require.Equal(t, image.Data, source["data"])
								require.Equal(t, image.MimeType, source["media_type"])
							}
						}
					} else {
						require.Len(t, user, 1)
						require.Equal(t, "text", user[0].(map[string]any)["type"])
						require.Equal(t, "(image omitted: model does not support images)", user[0].(map[string]any)["text"])
						require.Equal(t, "(tool image omitted: model does not support images)", result["content"])
					}
				}
				var requests, callbacks, done int
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					require.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					var body map[string]any
					require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
					check(body)
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"content":[],"stop_reason":"end_turn"}`))}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					if _, ok := event.(Done); ok {
						done++
					}
				}, WithAPIKey("test-key"), WithPayloadCallback(func(payload any) any {
					callbacks++
					var body map[string]any
					require.NoError(t, json.Unmarshal([]byte(payload.(string)), &body))
					check(body)
					return nil
				}))
				require.NoError(t, err)
				require.Equal(t, 1, requests)
				require.Equal(t, 1, callbacks)
				require.Equal(t, 1, done)
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				require.JSONEq(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicBlankThinkingReplayReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block ContentBlock
	}{
		{name: "blank", block: ContentBlock{Type: ContentBlockThinking, Thinking: " \t\n"}},
		{name: "legacy blank", block: ContentBlock{Type: ContentBlockThinking, Text: " \t\n"}},
		{name: "signed nonblank", block: ContentBlock{Type: ContentBlockThinking, Thinking: " reason ", ThoughtSignature: "signature"}},
	} {
		for _, source := range []string{"target", "other"} {
			t.Run(tc.name+"/"+source, func(t *testing.T) {
				model := &Model{ID: "target", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
				prompt := &Context{Messages: []Message{
					{Role: "user", Content: "question"},
					{Role: "assistant", Provider: model.Provider, API: model.API, Model: source, StopReason: string(StopReasonStop), Content: []ContentBlock{tc.block, {Type: ContentBlockText, Text: "answer"}}},
					{Role: "user", Content: "next"},
				}}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				check := func(body map[string]any) {
					messages := body["messages"].([]any)
					require.Len(t, messages, 3)
					assistant := messages[1].(map[string]any)
					require.Equal(t, "assistant", assistant["role"])
					blocks := assistant["content"].([]any)
					if tc.name == "signed nonblank" {
						require.Len(t, blocks, 2)
						if source == model.ID {
							require.Equal(t, map[string]any{"type": "thinking", "thinking": " reason ", "signature": "signature"}, blocks[0])
						} else {
							require.Equal(t, map[string]any{"type": "text", "text": " reason "}, blocks[0])
						}
					} else {
						require.Len(t, blocks, 1)
					}
					require.Equal(t, map[string]any{"type": "text", "text": "answer"}, blocks[len(blocks)-1])
				}
				var requests, callbacks, done int
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					require.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					var body map[string]any
					require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
					check(body)
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"content":[],"stop_reason":"end_turn"}`))}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					if _, ok := event.(Done); ok {
						done++
					}
				}, WithAPIKey("test-key"), WithPayloadCallback(func(payload any) any {
					callbacks++
					var body map[string]any
					require.NoError(t, json.Unmarshal([]byte(payload.(string)), &body))
					check(body)
					return nil
				}))
				require.NoError(t, err)
				require.Equal(t, 1, requests)
				require.Equal(t, 1, callbacks)
				require.Equal(t, 1, done)
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				require.JSONEq(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicSignedThinkingReplayReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name      string
		thinking  string
		signature string
		redacted  bool
	}{
		{name: "signed empty", signature: "signature"},
		{name: "signed blank", thinking: " \t\n", signature: "signature"},
		{name: "signed unicode blank", thinking: "\u00a0\u2003\u2028", signature: " signature "},
		{name: "signed nonblank", thinking: " reason ", signature: "signature"},
		{name: "unsigned empty"},
		{name: "unsigned blank", thinking: " \t\n"},
		{name: "blank signature empty", signature: " \t\n"},
		{name: "blank signature blank", thinking: " \t\n", signature: " \t\n"},
		{name: "unsigned nonblank", thinking: " reason "},
		{name: "blank signature nonblank", thinking: " reason ", signature: " \t\n"},
		{name: "redacted empty", signature: "encrypted", redacted: true},
	} {
		for _, compatibility := range []string{"unset", "off", "on"} {
			for _, entry := range []string{"Stream", "Complete"} {
				for _, source := range []string{"target", "other"} {
					for _, mode := range []string{"normal", "strict fallback"} {
						t.Run(tc.name+"/"+source+"/"+mode+"/"+compatibility+"/"+entry, func(t *testing.T) {
							model := &Model{ID: "target", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Compat: &CompatibilitySettings{SupportsStrictTools: true}}
							if compatibility != "unset" {
								require.NoError(t, json.Unmarshal([]byte(`{"supportsStrictTools":true,"allowEmptySignature":`+fmt.Sprint(compatibility == "on")+`}`), model.Compat))
							}
							prompt := &Context{Messages: []Message{
								{Role: "user", Content: []ContentBlock{{Type: ContentBlockText, Text: "question"}, {Type: ContentBlockThinking, Thinking: "user reasoning"}}},
								{Role: "assistant", Provider: model.Provider, API: model.API, Model: source, StopReason: string(StopReasonStop), Content: []ContentBlock{{Type: ContentBlockThinking, Thinking: tc.thinking, ThoughtSignature: tc.signature, Redacted: tc.redacted}, {Type: ContentBlockText, Text: "answer"}}},
								{Role: "user", Content: "next"},
							}}
							wantRequests := 1
							if mode == "strict fallback" {
								wantRequests = 2
								prompt.Tools = []Tool{{Name: "preferred", Parameters: json.RawMessage(`{"type":"object"}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)}}
							}
							before, err := json.Marshal(prompt)
							require.NoError(t, err)
							wantBlocks := []any{}
							if tc.redacted {
								if source == model.ID {
									wantBlocks = append(wantBlocks, map[string]any{"type": "redacted_thinking", "data": tc.signature})
								}
							} else if source == model.ID && strings.TrimSpace(tc.signature) != "" {
								wantBlocks = append(wantBlocks, map[string]any{"type": "thinking", "thinking": tc.thinking, "signature": tc.signature})
							} else if strings.TrimSpace(tc.thinking) != "" {
								if source == model.ID && compatibility == "on" {
									wantBlocks = append(wantBlocks, map[string]any{"type": "thinking", "thinking": tc.thinking, "signature": ""})
								} else {
									wantBlocks = append(wantBlocks, map[string]any{"type": "text", "text": tc.thinking})
								}
							}
							wantBlocks = append(wantBlocks, map[string]any{"type": "text", "text": "answer"})
							check := func(body map[string]any, attempt int) {
								require.Equal(t, []any{
									map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "question"}, map[string]any{"type": "text", "text": "user reasoning"}}},
									map[string]any{"role": "assistant", "content": wantBlocks},
									map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "next"}}},
								}, body["messages"])
								if mode == "strict fallback" {
									tools := body["tools"].([]any)
									require.Len(t, tools, 1)
									require.Equal(t, attempt == 1, tools[0].(map[string]any)["strict"] == true)
								}
							}
							var requests, callbacks, responses, closes, done int
							var callbackPayload string
							previous := http.DefaultClient
							t.Cleanup(func() { http.DefaultClient = previous })
							http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
								requests++
								require.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
								var body map[string]any
								wire, err := io.ReadAll(req.Body)
								require.NoError(t, err)
								assert.Equal(t, callbackPayload, string(wire))
								require.NoError(t, json.Unmarshal(wire, &body))
								check(body, requests)
								status := http.StatusOK
								response := `{"content":[],"stop_reason":"end_turn"}`
								if mode == "strict fallback" && requests == 1 {
									status = http.StatusBadRequest
									response = `{"error":{"type":"invalid_request_error","message":"Too many strict tools (21)"}}`
								}
								return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: &anthropicEOFBody{Reader: strings.NewReader(response), closes: &closes}}, nil
							})}
							client, err := newAnthropicClient(model)
							require.NoError(t, err)
							opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithResponseCallback(func(_ ProviderResponse) { responses++ }), WithPayloadCallback(func(payload any) any {
								callbacks++
								callbackPayload = payload.(string)
								var body map[string]any
								require.NoError(t, json.Unmarshal([]byte(payload.(string)), &body))
								check(body, callbacks)
								return nil
							})}
							checkMessage := func(message AssistantMessage) {
								t.Helper()
								assert.Equal(t, StopReasonStop, message.StopReason)
								assert.Equal(t, model.ID, message.Model)
								assert.Equal(t, model.API, message.API)
								assert.Equal(t, model.Provider, message.Provider)
								if mode == "strict fallback" {
									require.Len(t, message.Diagnostics, 1)
									assert.Equal(t, "anthropic_strict_tools_fallback", message.Diagnostics[0].Type)
								} else {
									assert.Empty(t, message.Diagnostics)
								}
							}
							if entry == "Stream" {
								err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
									switch e := event.(type) {
									case Done:
										done++
										checkMessage(e.Message)
									case StreamError:
										t.Errorf("unexpected stream error: %v", e.Err)
									}
								}, opts...)
							} else {
								var message *AssistantMessage
								message, err = client.Complete(t.Context(), model, prompt, opts...)
								require.NoError(t, err)
								require.NotNil(t, message)
								checkMessage(*message)
								done++
							}
							require.NoError(t, err)
							require.Equal(t, wantRequests, requests)
							require.Equal(t, wantRequests, callbacks)
							require.Equal(t, 1, done)
							require.Equal(t, 1, responses)
							require.Equal(t, wantRequests, closes)
							after, err := json.Marshal(prompt)
							require.NoError(t, err)
							require.JSONEq(t, string(before), string(after))
						})
					}
				}
			}
		}
	}
}

func TestAnthropicThinkingStartText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		want  string
	}{
		{name: "missing"},
		{name: "null", field: `,"thinking":null`},
		{name: "empty", field: `,"thinking":""`},
		{name: "nonempty", field: `,"thinking":" initial "`, want: " initial "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &anthropicClient{model: &Model{}}
			stream := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"" + tc.field + "}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"opaque\"}}\n\n" +
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n"
			var starts, ends, done int
			err := client.handleSSE(t.Context(), strings.NewReader(stream), func(_ context.Context, event StreamEvent) {
				switch e := event.(type) {
				case ThinkingStart:
					starts++
					require.Equal(t, tc.want, e.Partial.Content[0].Thinking)
				case ThinkingEnd:
					ends++
					require.Equal(t, tc.want, e.Content)
				case Done:
					done++
					require.Equal(t, []ContentBlock{{Type: ContentBlockThinking, Thinking: tc.want, ThoughtSignature: "opaque"}}, e.Message.Content)
				}
			})
			require.NoError(t, err)
			require.Equal(t, 1, starts)
			require.Equal(t, 1, ends)
			require.Equal(t, 1, done)
		})
	}
}

func TestAnthropicJSONThinkingRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire map[string]any
		want ContentBlock
	}{
		{name: "signed nonblank", wire: map[string]any{"type": "thinking", "thinking": " reason ", "signature": " opaque "}, want: ContentBlock{Type: ContentBlockThinking, Thinking: " reason ", ThoughtSignature: " opaque "}},
		{name: "signed empty", wire: map[string]any{"type": "thinking", "thinking": "", "signature": "opaque"}, want: ContentBlock{Type: ContentBlockThinking, ThoughtSignature: "opaque"}},
		{name: "signed blank", wire: map[string]any{"type": "thinking", "thinking": " \t\n\u2003", "signature": "opaque"}, want: ContentBlock{Type: ContentBlockThinking, Thinking: " \t\n\u2003", ThoughtSignature: "opaque"}},
		{name: "redacted", wire: map[string]any{"type": "redacted_thinking", "data": " encrypted "}, want: ContentBlock{Type: ContentBlockThinking, Thinking: "[Reasoning redacted]", ThoughtSignature: " encrypted ", Redacted: true}},
		{name: "redacted empty", wire: map[string]any{"type": "redacted_thinking", "data": ""}, want: ContentBlock{Type: ContentBlockThinking, Thinking: "[Reasoning redacted]", Redacted: true}},
		{name: "unsigned nonblank", wire: map[string]any{"type": "thinking", "thinking": " reason "}, want: ContentBlock{Type: ContentBlockThinking, Thinking: " reason "}},
		{name: "unsigned blank", wire: map[string]any{"type": "thinking", "thinking": " \t\n"}, want: ContentBlock{Type: ContentBlockThinking, Thinking: " \t\n"}},
	} {
		for _, target := range []string{"source", "other"} {
			for _, mode := range []string{"Stream", "Complete"} {
				t.Run(tc.name+"/"+target+"/"+mode, func(t *testing.T) {
					model := &Model{ID: "source", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
					tool := map[string]any{"type": "tool_use", "id": "call_1", "name": "lookup", "input": map[string]any{"key": "value"}}
					text := map[string]any{"type": "text", "text": "answer"}
					response, err := json.Marshal(map[string]any{
						"id": "msg_json", "content": []any{tc.wire, text, tool}, "stop_reason": "tool_use",
						"usage": map[string]any{"input_tokens": 11, "output_tokens": 7, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 5},
					})
					require.NoError(t, err)
					var requests, callbacks, done int
					var bodies []map[string]any
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						require.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
						var body map[string]any
						require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
						bodies = append(bodies, body)
						content := string(response)
						if requests == 2 {
							content = `{"content":[],"stop_reason":"end_turn"}`
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(content))}, nil
					})}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
						callbacks++
						var body map[string]any
						require.NoError(t, json.Unmarshal([]byte(payload.(string)), &body))
						bodies = append(bodies, body)
						return nil
					})}
					prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}, Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}}}`)}}}
					var message *AssistantMessage
					if mode == "Complete" {
						message, err = client.Complete(t.Context(), model, prompt, opts...)
					} else {
						err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
							switch e := event.(type) {
							case Done:
								done++
								message = &e.Message
							case StreamError:
								t.Errorf("unexpected stream error: %v", e)
							}
						}, opts...)
						require.Equal(t, 1, done)
					}
					require.NoError(t, err)
					require.NotNil(t, message)
					assert.Equal(t, []ContentBlock{tc.want, {Type: ContentBlockText, Text: "answer"}, {Type: ContentBlockToolCall, ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"key":"value"}`)}}, message.Content)
					require.Equal(t, "msg_json", message.ResponseID)
					require.Equal(t, model.ID, message.Model)
					require.Equal(t, model.API, message.API)
					require.Equal(t, model.Provider, message.Provider)
					require.Equal(t, StopReasonToolUse, message.StopReason)
					require.Equal(t, 11, message.Usage.Input)
					require.Equal(t, 7, message.Usage.Output)
					require.Equal(t, 3, message.Usage.CacheRead)
					require.Equal(t, 5, message.Usage.CacheWrite)
					prompt.Messages = append(prompt.Messages,
						Message{Role: "assistant", Content: message.Content, Provider: message.Provider, API: message.API, Model: message.Model, StopReason: string(message.StopReason)},
						Message{Role: "toolResult", ToolCallID: "call_1", ToolName: "lookup", Content: "result"})
					before, err := json.Marshal(prompt)
					require.NoError(t, err)
					nextModel := *model
					nextModel.ID = target
					done = 0
					err = client.Stream(t.Context(), &nextModel, prompt, func(_ context.Context, event StreamEvent) {
						if _, ok := event.(Done); ok {
							done++
						}
					}, opts...)
					require.NoError(t, err)
					require.Equal(t, 2, requests)
					require.Equal(t, 2, callbacks)
					require.Equal(t, 1, done)
					require.Len(t, bodies, 4)
					wantBlocks := []any{}
					if target == model.ID && (tc.want.Redacted || tc.want.ThoughtSignature != "") {
						if tc.want.Redacted && tc.want.ThoughtSignature == "" {
							wantBlocks = append(wantBlocks, map[string]any{"type": "redacted_thinking"})
						} else {
							wantBlocks = append(wantBlocks, tc.wire)
						}
					} else if !tc.want.Redacted && strings.TrimSpace(tc.want.Thinking) != "" {
						wantBlocks = append(wantBlocks, map[string]any{"type": "text", "text": tc.want.Thinking})
					}
					wantBlocks = append(wantBlocks, text, tool)
					for _, body := range bodies[2:] {
						require.Equal(t, target, body["model"])
						assert.Equal(t, []any{
							map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "question"}}},
							map[string]any{"role": "assistant", "content": wantBlocks},
							map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "result"}}},
						}, body["messages"])
					}
					after, err := json.Marshal(prompt)
					require.NoError(t, err)
					require.JSONEq(t, string(before), string(after))
				})
			}
		}
	}
}

func TestAnthropicThinkingStartSignatureReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		block   map[string]any
		deltas  []string
		index   int
		initial string
		want    ContentBlock
	}{
		{name: "initial only", block: map[string]any{"type": "thinking", "thinking": "reason", "signature": " initial "}, initial: " initial ", want: ContentBlock{Type: ContentBlockThinking, Thinking: "reason", ThoughtSignature: " initial "}},
		{name: "initial and deltas", block: map[string]any{"type": "thinking", "thinking": "reason", "signature": "initial"}, deltas: []string{"-one", "-two"}, initial: "initial", want: ContentBlock{Type: ContentBlockThinking, Thinking: "reason", ThoughtSignature: "initial-one-two"}},
		{name: "empty signed", block: map[string]any{"type": "thinking", "thinking": "", "signature": "initial"}, initial: "initial", want: ContentBlock{Type: ContentBlockThinking, ThoughtSignature: "initial"}},
		{name: "blank signed", block: map[string]any{"type": "thinking", "thinking": " \t\u2003", "signature": "initial"}, deltas: []string{"-tail"}, initial: "initial", want: ContentBlock{Type: ContentBlockThinking, Thinking: " \t\u2003", ThoughtSignature: "initial-tail"}},
		{name: "missing signature", block: map[string]any{"type": "thinking", "thinking": "reason"}, deltas: []string{"one", "two"}, want: ContentBlock{Type: ContentBlockThinking, Thinking: "reason", ThoughtSignature: "onetwo"}},
		{name: "null signature", block: map[string]any{"type": "thinking", "thinking": "reason", "signature": nil}, deltas: []string{"one", "two"}, want: ContentBlock{Type: ContentBlockThinking, Thinking: "reason", ThoughtSignature: "onetwo"}},
		{name: "empty signature", block: map[string]any{"type": "thinking", "thinking": "reason", "signature": ""}, deltas: []string{"one", "two"}, want: ContentBlock{Type: ContentBlockThinking, Thinking: "reason", ThoughtSignature: "onetwo"}},
		{name: "unsigned", block: map[string]any{"type": "thinking", "thinking": "reason"}, want: ContentBlock{Type: ContentBlockThinking, Thinking: "reason"}},
		{name: "unsigned blank", block: map[string]any{"type": "thinking", "thinking": " \t\u2003"}, want: ContentBlock{Type: ContentBlockThinking, Thinking: " \t\u2003"}},
		{name: "redacted", block: map[string]any{"type": "redacted_thinking", "data": "encrypted"}, initial: "encrypted", want: ContentBlock{Type: ContentBlockThinking, Thinking: "[Reasoning redacted]", ThoughtSignature: "encrypted", Redacted: true}},
		{name: "redacted and deltas", block: map[string]any{"type": "redacted_thinking", "data": "encrypted"}, deltas: []string{"-one", "-two"}, initial: "encrypted", want: ContentBlock{Type: ContentBlockThinking, Thinking: "[Reasoning redacted]", ThoughtSignature: "encrypted-one-two", Redacted: true}},
		{name: "sparse redacted and deltas", index: 7, block: map[string]any{"type": "redacted_thinking", "data": "encrypted"}, deltas: []string{"-one", "-two"}, initial: "encrypted", want: ContentBlock{Type: ContentBlockThinking, Thinking: "[Reasoning redacted]", ThoughtSignature: "encrypted-one-two", Redacted: true}},
	} {
		for _, target := range []string{"source", "other"} {
			t.Run(tc.name+"/"+target, func(t *testing.T) {
				var stream strings.Builder
				emit := func(event map[string]any) {
					data, err := json.Marshal(event)
					require.NoError(t, err)
					stream.WriteString("data: " + string(data) + "\n\n")
				}
				emit(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_sse", "usage": map[string]any{"input_tokens": 11}}})
				emit(map[string]any{"type": "content_block_start", "index": tc.index, "content_block": tc.block})
				for _, delta := range tc.deltas {
					emit(map[string]any{"type": "content_block_delta", "index": tc.index, "delta": map[string]any{"type": "signature_delta", "signature": delta}})
				}
				emit(map[string]any{"type": "content_block_stop", "index": tc.index})
				emit(map[string]any{"type": "content_block_start", "index": tc.index + 1, "content_block": map[string]any{"type": "text", "text": "answer"}})
				emit(map[string]any{"type": "content_block_stop", "index": tc.index + 1})
				emit(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 7}})
				emit(map[string]any{"type": "message_stop"})
				var requests, callbacks, starts, ends, done int
				var bodies []map[string]any
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					require.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					var body map[string]any
					require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
					bodies = append(bodies, body)
					if requests == 1 {
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream.String()))}, nil
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"content":[],"stop_reason":"end_turn"}`))}, nil
				})}
				model := &Model{ID: "source", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
					callbacks++
					var body map[string]any
					require.NoError(t, json.Unmarshal([]byte(payload.(string)), &body))
					bodies = append(bodies, body)
					return nil
				})}
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				var message *AssistantMessage
				err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case ThinkingStart:
						starts++
						assert.Equal(t, tc.initial, e.Partial.Content[e.ContentIndex].ThoughtSignature)
					case ThinkingEnd:
						ends++
						assert.Equal(t, tc.want, e.Partial.Content[e.ContentIndex])
					case Done:
						done++
						message = &e.Message
					case StreamError:
						t.Errorf("unexpected stream error: %v", e)
					}
				}, opts...)
				require.NoError(t, err)
				require.Equal(t, 1, starts)
				require.Equal(t, 1, ends)
				require.Equal(t, 1, done)
				require.NotNil(t, message)
				assert.Equal(t, []ContentBlock{tc.want, {Type: ContentBlockText, Text: "answer"}}, message.Content)
				require.Equal(t, "msg_sse", message.ResponseID)
				require.Equal(t, model.ID, message.Model)
				require.Equal(t, model.API, message.API)
				require.Equal(t, model.Provider, message.Provider)
				require.Equal(t, StopReasonStop, message.StopReason)
				require.Equal(t, 11, message.Usage.Input)
				require.Equal(t, 7, message.Usage.Output)
				prompt.Messages = append(prompt.Messages,
					Message{Role: "assistant", Content: message.Content, Provider: message.Provider, API: message.API, Model: message.Model},
					Message{Role: "user", Content: "next"})
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				nextModel := *model
				nextModel.ID = target
				done = 0
				err = client.Stream(t.Context(), &nextModel, prompt, func(_ context.Context, event StreamEvent) {
					if _, ok := event.(Done); ok {
						done++
					}
				}, opts...)
				require.NoError(t, err)
				require.Equal(t, 2, requests)
				require.Equal(t, 2, callbacks)
				require.Equal(t, 1, done)
				require.Len(t, bodies, 4)
				var wantBlocks []any
				if target == model.ID && tc.want.Redacted {
					wantBlocks = append(wantBlocks, map[string]any{"type": "redacted_thinking", "data": tc.want.ThoughtSignature})
				} else if target == model.ID && tc.want.ThoughtSignature != "" {
					wantBlocks = append(wantBlocks, map[string]any{"type": "thinking", "thinking": tc.want.Thinking, "signature": tc.want.ThoughtSignature})
				} else if !tc.want.Redacted && strings.TrimSpace(tc.want.Thinking) != "" {
					wantBlocks = append(wantBlocks, map[string]any{"type": "text", "text": tc.want.Thinking})
				}
				wantBlocks = append(wantBlocks, map[string]any{"type": "text", "text": "answer"})
				for _, body := range bodies[2:] {
					require.Equal(t, target, body["model"])
					assert.Equal(t, []any{
						map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "question"}}},
						map[string]any{"role": "assistant", "content": wantBlocks},
						map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "next"}}},
					}, body["messages"])
				}
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				require.JSONEq(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicAssistantOrderReachesTransport(t *testing.T) {
	for _, format := range []string{"json", "sse"} {
		for _, layout := range []string{"interleaved", "tools last"} {
			for _, mode := range []string{"same model", "cross model", "strict fallback"} {
				t.Run(format+"/"+layout+"/"+mode, func(t *testing.T) {
					model := &Model{ID: "source", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Compat: &CompatibilitySettings{SupportsStrictTools: true}}
					first := map[string]any{"type": "tool_use", "id": "call_1", "name": "read", "input": map[string]any{"path": "one"}}
					second := map[string]any{"type": "tool_use", "id": "call_2", "name": "read", "input": map[string]any{"path": "two"}}
					thinking := map[string]any{"type": "thinking", "thinking": "reason", "signature": "opaque"}
					text := map[string]any{"type": "text", "text": "between"}
					tail := map[string]any{"type": "text", "text": "tail"}
					wire := []any{first, thinking, text, second, tail}
					want := []ContentBlock{
						{Type: ContentBlockToolCall, ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"path":"one"}`)},
						{Type: ContentBlockThinking, Thinking: "reason", ThoughtSignature: "opaque"},
						{Type: ContentBlockText, Text: "between"},
						{Type: ContentBlockToolCall, ID: "call_2", Name: "read", Arguments: json.RawMessage(`{"path":"two"}`)},
						{Type: ContentBlockText, Text: "tail"},
					}
					if layout == "tools last" {
						wire = []any{thinking, text, tail, first, second}
						want = []ContentBlock{want[1], want[2], want[4], want[0], want[3]}
					}
					response, err := json.Marshal(map[string]any{"id": "msg_order", "content": wire, "stop_reason": "tool_use", "usage": map[string]any{"input_tokens": 11, "output_tokens": 7}})
					require.NoError(t, err)
					contentType := "application/json"
					if format == "sse" {
						var stream strings.Builder
						emit := func(event any) {
							data, err := json.Marshal(event)
							require.NoError(t, err)
							stream.WriteString("data: " + string(data) + "\n\n")
						}
						emit(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_order", "usage": map[string]any{"input_tokens": 11}}})
						for i, block := range wire {
							emit(map[string]any{"type": "content_block_start", "index": i, "content_block": block})
							emit(map[string]any{"type": "content_block_stop", "index": i})
						}
						emit(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 7}})
						emit(map[string]any{"type": "message_stop"})
						response = []byte(stream.String())
						contentType = "text/event-stream"
					}
					var requests, callbacks, done int
					var bodies []map[string]any
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						require.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
						var body map[string]any
						require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
						bodies = append(bodies, body)
						status, kind, data := http.StatusOK, contentType, string(response)
						if requests > 1 {
							kind, data = "application/json", `{"content":[],"stop_reason":"end_turn"}`
						}
						if mode == "strict fallback" && requests == 2 {
							status, data = http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"Too many strict tools (21)"}}`
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(data))}, nil
					})}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					callback := WithPayloadCallback(func(payload any) any {
						callbacks++
						var body map[string]any
						require.NoError(t, json.Unmarshal([]byte(payload.(string)), &body))
						bodies = append(bodies, body)
						return nil
					})
					prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}, Tools: []Tool{{Name: "read", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)}}}
					message, err := client.Complete(t.Context(), model, prompt, WithAPIKey("test-key"), WithCacheRetention("none"), callback)
					require.NoError(t, err)
					require.NotNil(t, message)
					require.Equal(t, want, message.Content)
					require.Equal(t, "msg_order", message.ResponseID)
					require.Equal(t, model.ID, message.Model)
					require.Equal(t, model.API, message.API)
					require.Equal(t, model.Provider, message.Provider)
					require.Equal(t, StopReasonToolUse, message.StopReason)
					require.Equal(t, 11, message.Usage.Input)
					require.Equal(t, 7, message.Usage.Output)
					prompt.Messages = append(prompt.Messages,
						Message{Role: "assistant", Content: message.Content, Provider: message.Provider, API: message.API, Model: message.Model, StopReason: string(message.StopReason)},
						Message{Role: "toolResult", ToolCallID: "call_1", ToolName: "read", Content: "one result"},
						Message{Role: "toolResult", ToolCallID: "call_2", ToolName: "read", Content: "two result"})
					transformed, err := TransformContext(prompt, model, normalizeAnthropicToolCallIDNormalizer)
					require.NoError(t, err)
					require.Equal(t, want, transformed.Messages[1].Content)
					before, err := json.Marshal(prompt)
					require.NoError(t, err)
					nextModel := *model
					key := "test-key"
					if mode == "cross model" {
						nextModel.ID = "other"
						thinking = map[string]any{"type": "text", "text": "reason"}
					}
					wantRequests := 2
					if mode == "strict fallback" {
						wantRequests = 3
					}
					wantBlocks := []any{first, thinking, text, second, tail}
					if layout == "tools last" {
						wantBlocks = []any{thinking, text, tail, first, second}
					}
					err = client.Stream(t.Context(), &nextModel, prompt, func(_ context.Context, event StreamEvent) {
						switch e := event.(type) {
						case Done:
							done++
						case StreamError:
							t.Errorf("unexpected stream error: %v", e)
						}
					}, WithAPIKey(key), WithCacheRetention("none"), callback)
					require.NoError(t, err)
					require.Equal(t, wantRequests, requests)
					require.Equal(t, wantRequests, callbacks)
					require.Equal(t, 1, done)
					require.Len(t, bodies, 2*wantRequests)
					for i, body := range bodies[2:] {
						require.Equal(t, nextModel.ID, body["model"])
						assert.Equal(t, []any{
							map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "question"}}},
							map[string]any{"role": "assistant", "content": wantBlocks},
							map[string]any{"role": "user", "content": []any{
								map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "one result"},
								map[string]any{"type": "tool_result", "tool_use_id": "call_2", "content": "two result"},
							}},
						}, body["messages"])
						tools := body["tools"].([]any)
						require.Len(t, tools, 1)
						assert.Equal(t, mode != "strict fallback" || i < 2, tools[0].(map[string]any)["strict"] == true)
					}
					after, err := json.Marshal(prompt)
					require.NoError(t, err)
					require.JSONEq(t, string(before), string(after))
				})
			}
		}
	}
}

func TestAnthropicMissingToolInputReachesTransport(t *testing.T) {
	for _, format := range []string{"json", "sse"} {
		for _, input := range []string{"missing", "null", "{}", `{"value":9007199254740993}`} {
			t.Run(format+"/"+input, func(t *testing.T) {
				model := &Model{ID: "source", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
				block := `{"type":"tool_use","id":"call_1","name":"read"`
				published := json.RawMessage("{}")
				wantInput := "{}"
				if input != "missing" {
					block += `,"input":` + input
					if input != "null" {
						published = json.RawMessage(input)
						wantInput = input
					}
				}
				block += "}"
				contentType := "application/json"
				response := `{"id":"msg_input","content":[` + block + `],"stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":7}}`
				if format == "sse" {
					contentType = "text/event-stream"
					response = "data: " + `{"type":"message_start","message":{"id":"msg_input","usage":{"input_tokens":11}}}` + "\n\n" +
						"data: " + `{"type":"content_block_start","index":0,"content_block":` + block + "}\n\n" +
						"data: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
						"data: " + `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}` + "\n\n" +
						"data: " + `{"type":"message_stop"}` + "\n\n"
				}
				var requests, callbacks, done int
				var payloads []string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					require.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					body, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					payloads = append(payloads, string(body))
					kind, data := contentType, response
					if requests > 1 {
						kind, data = "application/json", `{"content":[],"stop_reason":"end_turn"}`
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(data))}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				callback := WithPayloadCallback(func(payload any) any {
					callbacks++
					payloads = append(payloads, payload.(string))
					return nil
				})
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}, Tools: []Tool{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}}
				message, err := client.Complete(t.Context(), model, prompt, WithAPIKey("test-key"), WithCacheRetention("none"), callback)
				require.NoError(t, err)
				require.NotNil(t, message)
				require.Equal(t, []ContentBlock{{Type: ContentBlockToolCall, ID: "call_1", Name: "read", Arguments: published}}, message.Content)
				require.Equal(t, "msg_input", message.ResponseID)
				require.Equal(t, model.ID, message.Model)
				require.Equal(t, model.API, message.API)
				require.Equal(t, model.Provider, message.Provider)
				require.Equal(t, StopReasonToolUse, message.StopReason)
				require.Equal(t, 11, message.Usage.Input)
				require.Equal(t, 7, message.Usage.Output)
				prompt.Messages = append(prompt.Messages,
					Message{Role: "assistant", Content: message.Content, Provider: message.Provider, API: message.API, Model: message.Model, StopReason: string(message.StopReason)},
					Message{Role: "toolResult", ToolCallID: "call_1", ToolName: "read", Content: "result"})
				transformed, err := TransformContext(prompt, model, normalizeAnthropicToolCallIDNormalizer)
				require.NoError(t, err)
				require.Equal(t, message.Content, transformed.Messages[1].Content)
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case Done:
						done++
					case StreamError:
						t.Errorf("unexpected stream error: %v", e)
					}
				}, WithAPIKey("test-key"), WithCacheRetention("none"), callback)
				require.NoError(t, err)
				require.Equal(t, 2, requests)
				require.Equal(t, 2, callbacks)
				require.Equal(t, 1, done)
				require.Len(t, payloads, 4)
				for _, payload := range payloads[2:] {
					var body struct {
						Messages json.RawMessage `json:"messages"`
					}
					require.NoError(t, json.Unmarshal([]byte(payload), &body))
					var messages []struct {
						Content []struct {
							Input json.RawMessage `json:"input"`
						} `json:"content"`
					}
					require.NoError(t, json.Unmarshal(body.Messages, &messages))
					require.Len(t, messages, 3)
					require.Len(t, messages[1].Content, 1)
					assert.Equal(t, wantInput, string(messages[1].Content[0].Input))
					assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":`+wantInput+`}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"result"}]}]`, string(body.Messages))
				}
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				require.JSONEq(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicBlankTextReplayReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    string
		content any
		want    string
	}{
		{name: "user empty string", role: "user", content: ""},
		{name: "user blank string", role: "user", content: " \t\n\u2003"},
		{name: "user nil", role: "user"},
		{name: "user empty array", role: "user", content: []ContentBlock{}},
		{name: "user empty text", role: "user", content: []ContentBlock{{Type: ContentBlockText}}},
		{name: "user blank text", role: "user", content: []ContentBlock{{Type: ContentBlockText, Text: " \t\n\u2003"}}},
		{name: "user nonblank string", role: "user", content: " keep \n", want: `{"role":"user","content":[{"type":"text","text":" keep \n"}]}`},
		{name: "user mixed text", role: "user", content: []ContentBlock{{Type: ContentBlockText}, {Type: ContentBlockText, Text: " keep \n"}, {Type: ContentBlockText, Text: "\u2003"}}, want: `{"role":"user","content":[{"type":"text","text":" keep \n"}]}`},
		{name: "user image", role: "user", content: []ContentBlock{{Type: ContentBlockText, Text: "\t"}, {Type: ContentBlockImage, MimeType: "image/png", Data: "aW1hZ2U="}}, want: `{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}`},
		{name: "assistant empty text", role: "assistant", content: []ContentBlock{{Type: ContentBlockText}}},
		{name: "assistant blank text", role: "assistant", content: []ContentBlock{{Type: ContentBlockText, Text: " \t\n\u2003"}}},
		{name: "assistant mixed text", role: "assistant", content: []ContentBlock{{Type: ContentBlockText}, {Type: ContentBlockText, Text: " keep \n"}, {Type: ContentBlockText, Text: "\u2003"}}, want: `{"role":"assistant","content":[{"type":"text","text":" keep \n"}]}`},
		{name: "assistant nonblank text", role: "assistant", content: []ContentBlock{{Type: ContentBlockText, Text: " keep \n"}}, want: `{"role":"assistant","content":[{"type":"text","text":" keep \n"}]}`},
		{name: "assistant signed empty thinking", role: "assistant", content: []ContentBlock{{Type: ContentBlockThinking, ThoughtSignature: "sig"}}, want: `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig"}]}`},
		{name: "assistant signed blank thinking", role: "assistant", content: []ContentBlock{{Type: ContentBlockThinking, Thinking: " \t", ThoughtSignature: "sig"}}, want: `{"role":"assistant","content":[{"type":"thinking","thinking":" \t","signature":"sig"}]}`},
		{name: "assistant unsigned blank thinking", role: "assistant", content: []ContentBlock{{Type: ContentBlockThinking, Thinking: " \t"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &Model{ID: "source", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Input: []InputType{InputText, InputImage}}
			prompt := &Context{Messages: []Message{
				{Role: "user", Content: "question"},
				{Role: tc.role, Content: tc.content, Provider: model.Provider, API: model.API, Model: model.ID},
				{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockToolCall, ID: "call_1", Name: "read", Arguments: json.RawMessage("{}")}}, Provider: model.Provider, API: model.API, Model: model.ID},
				{Role: "toolResult", ToolCallID: "call_1", ToolName: "read", Content: []ContentBlock{{Type: ContentBlockText, Text: " \t\n\u2003"}}},
			}}
			transformed, err := TransformContext(prompt, model, normalizeAnthropicToolCallIDNormalizer)
			require.NoError(t, err)
			if tc.name != "assistant unsigned blank thinking" {
				require.Len(t, transformed.Messages, 4)
				require.Equal(t, tc.content, transformed.Messages[1].Content)
			}
			before, err := json.Marshal(prompt)
			require.NoError(t, err)
			want := `[{"role":"user","content":[{"type":"text","text":"question"}]},`
			if tc.want != "" {
				want += tc.want + ","
			}
			want += `{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":" \t\n\u2003"}]}]`
			var requests, callbacks, done int
			var payloads []string
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				require.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				payloads = append(payloads, string(body))
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"content":[],"stop_reason":"end_turn"}`))}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
				switch e := event.(type) {
				case Done:
					done++
				case StreamError:
					t.Errorf("unexpected stream error: %v", e)
				}
			}, WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
				callbacks++
				payloads = append(payloads, payload.(string))
				return nil
			}))
			require.NoError(t, err)
			require.Equal(t, 1, requests)
			require.Equal(t, 1, callbacks)
			require.Equal(t, 1, done)
			require.Len(t, payloads, 2)
			for _, payload := range payloads {
				var body struct {
					Messages json.RawMessage `json:"messages"`
				}
				require.NoError(t, json.Unmarshal([]byte(payload), &body))
				assert.JSONEq(t, want, string(body.Messages))
			}
			after, err := json.Marshal(prompt)
			require.NoError(t, err)
			require.JSONEq(t, string(before), string(after))
		})
	}
}

func TestAnthropicOverlappingRequestsKeepResponseState(t *testing.T) {
	for _, scenario := range []struct {
		name, outerKey, innerKey, innerTool, outerName, innerName string
	}{
		{name: "separate keys and loadouts", outerKey: "outer-key", innerKey: "inner-key", innerTool: "lookup", outerName: "Read", innerName: "Read"},
	} {
		for _, format := range []string{"JSON", "SSE"} {
			t.Run(scenario.name+"/"+format, func(t *testing.T) {
				model := &Model{ID: "claude-overlap", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Compat: &CompatibilitySettings{SupportsMidConvoEffort: true}}
				outer := &Context{Tools: []Tool{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}, Messages: []Message{{Role: "user", Content: "outer"}}}
				inner := &Context{Tools: []Tool{{Name: scenario.innerTool, Parameters: json.RawMessage(`{"type":"object"}`)}}, Messages: []Message{{Role: "user", Content: "inner"}}}
				before, err := json.Marshal([]any{model, outer, inner})
				require.NoError(t, err)
				var requests, callbacks, responses, terminals int
				var closes [2]int
				var payload string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					i := requests
					requests++
					wire, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, payload, string(wire))
					var body map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(wire, &body))
					key, tool, question, effort := scenario.outerKey, "read", "outer", "low"
					if i == 1 {
						key, tool, question, effort = scenario.innerKey, scenario.innerTool, "inner", "high"
					}
					assert.Equal(t, key, req.Header.Get("x-api-key"))
					assert.Empty(t, req.Header.Get("Authorization"))
					assert.NotContains(t, body, "system")
					assert.JSONEq(t, fmt.Sprintf(`[{"name":%q,"description":"","input_schema":{"type":"object"},"eager_input_streaming":true}]`, tool), string(body["tools"]))
					assert.JSONEq(t, fmt.Sprintf(`[{"role":"user","content":[{"type":"text","text":%q}]},{"role":"system","content":[],"output_config":{"effort":%q}}]`, question, effort), string(body["messages"]))
					block := `{"type":"tool_use","id":"call_overlap","name":"Read","input":{"n":9007199254740993}}`
					data := `{"id":"msg_` + question + `","content":[` + block + `],"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":2}}`
					kind := "application/json"
					if format == "SSE" {
						kind = "text/event-stream"
						data = "data: " + `{"type":"message_start","message":{"id":"msg_` + question + `","usage":{"input_tokens":3}}}` + "\n\n" +
							"data: " + `{"type":"content_block_start","index":0,"content_block":` + block + `}` + "\n\n" +
							"data: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
							"data: " + `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}` + "\n\n" +
							"data: " + `{"type":"message_stop"}` + "\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {kind}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(data)), closes: &closes[i]}}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				check := func(message AssistantMessage, name, effort, id string) {
					t.Helper()
					assert.Equal(t, []ContentBlock{{Type: ContentBlockToolCall, ID: "call_overlap", Name: name, Arguments: json.RawMessage(`{"n":9007199254740993}`)}}, message.Content)
					assert.Equal(t, effort, message.ProviderThinkingLevel)
					assert.Equal(t, id, message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, StopReasonToolUse, message.StopReason)
					assert.Equal(t, 5, message.Usage.TotalTokens)
					assert.Empty(t, message.Diagnostics)
				}
				opts := []Option{WithClient(client), WithCacheRetention("none"), WithPayloadCallback(func(value any) any { callbacks++; payload = value.(string); return nil })}
				innerOptions := append(slices.Clone(opts), WithAPIKey(scenario.innerKey), WithEffort("high"), WithResponseCallback(func(response ProviderResponse) {
					responses++
					assert.Equal(t, http.StatusOK, response.Status)
					assert.Equal(t, [2]int{}, closes)
				}))
				outerOptions := append(slices.Clone(opts), WithAPIKey(scenario.outerKey), WithEffort("low"), WithResponseCallback(func(response ProviderResponse) {
					responses++
					assert.Equal(t, http.StatusOK, response.Status)
					assert.Equal(t, 1, requests)
					message, err := CompleteWithModel(t.Context(), model, inner, innerOptions...)
					require.NoError(t, err)
					require.NotNil(t, message)
					check(*message, scenario.innerName, "high", "msg_inner")
					assert.Equal(t, [2]int{0, 1}, closes)
				}))
				err = StreamWithModel(t.Context(), model, outer, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case ToolCallStart:
						assert.Equal(t, scenario.outerName, e.Partial.Content[0].Name)
					case Done:
						terminals++
						check(e.Message, scenario.outerName, "low", "msg_outer")
					case StreamError:
						t.Errorf("unexpected terminal: %v", e.Err)
					}
				}, outerOptions...)
				require.NoError(t, err)
				assert.Equal(t, []int{2, 2, 2, 1}, []int{requests, callbacks, responses, terminals})
				assert.Equal(t, [2]int{1, 1}, closes)
				assert.Empty(t, client.tools)
				assert.Empty(t, client.thinkingLevel)
				after, err := json.Marshal([]any{model, outer, inner})
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicSSEBlockRoutingReachesTransport(t *testing.T) {
	for _, target := range []string{"text", "thinking", "redacted_thinking", "tool_use"} {
		for _, delta := range []string{"text_delta", "thinking_delta", "signature_delta", "input_json_delta"} {
			for _, route := range []string{"known", "unsupported", "absent"} {
				t.Run(target+"/"+delta+"/"+route, func(t *testing.T) {
					model := &Model{ID: "source", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
					want := ContentBlock{Type: ContentBlockText, Text: "answer ok"}
					block := map[string]any{"type": target, "text": "answer"}
					valid := map[string]any{"type": "text_delta", "text": " ok"}
					deltaEvent, startEvent, endEvent := "text_delta", "text_start", "text_end"
					stop := "end_turn"
					switch target {
					case "thinking", "redacted_thinking":
						block = map[string]any{"type": target, "thinking": "reason", "signature": "opaque", "data": "opaque"}
						want = ContentBlock{Type: ContentBlockThinking, Thinking: "reason ok", ThoughtSignature: "opaque"}
						if target == "redacted_thinking" {
							want.Thinking, want.Redacted = "[Reasoning redacted] ok", true
						}
						valid = map[string]any{"type": "thinking_delta", "thinking": " ok"}
						deltaEvent, startEvent, endEvent = "thinking_delta", "thinking_start", "thinking_end"
					case "tool_use":
						block = map[string]any{"type": target, "id": "call_1", "name": "read", "input": map[string]any{}}
						want = ContentBlock{Type: ContentBlockToolCall, ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"n":9007199254740993}`)}
						valid = map[string]any{"type": "input_json_delta", "partial_json": string(want.Arguments)}
						deltaEvent, startEvent, endEvent = "tool_delta", "tool_start", "tool_end"
						stop = "tool_use"
					}
					injected := map[string]any{"type": delta}
					switch delta {
					case "text_delta":
						injected["text"] = " injected"
					case "thinking_delta":
						injected["thinking"] = " injected"
					case "signature_delta":
						injected["signature"] = ":injected"
					case "input_json_delta":
						injected["partial_json"] = " "
					}
					accepted := route == "known" && (delta == "text_delta" && target == "text" || delta == "input_json_delta" && target == "tool_use" || (delta == "thinking_delta" || delta == "signature_delta") && want.Type == ContentBlockThinking)
					if accepted {
						switch delta {
						case "text_delta":
							want.Text = "answer injected ok"
						case "thinking_delta":
							want.Thinking = strings.TrimSuffix(want.Thinking, " ok") + " injected ok"
						case "signature_delta":
							want.ThoughtSignature += ":injected"
						case "input_json_delta":
							want.Arguments = append(json.RawMessage(" "), want.Arguments...)
						}
					}
					var stream strings.Builder
					emit := func(event any) {
						data, err := json.Marshal(event)
						require.NoError(t, err)
						stream.WriteString("data: " + string(data) + "\n\n")
					}
					emit(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_route", "usage": map[string]any{"input_tokens": 11}}})
					index := 11
					if route == "unsupported" {
						index = 2
						emit(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "server_tool_use", "id": "server_1", "name": "web_search", "input": map[string]any{}}})
					} else if route == "absent" {
						index = 99
					}
					emit(map[string]any{"type": "content_block_start", "index": 11, "content_block": block})
					emit(map[string]any{"type": "content_block_delta", "index": index, "delta": injected})
					if route != "known" {
						emit(map[string]any{"type": "content_block_stop", "index": index})
					}
					emit(map[string]any{"type": "content_block_delta", "index": 11, "delta": valid})
					emit(map[string]any{"type": "content_block_stop", "index": 11})
					emit(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": 7}})
					emit(map[string]any{"type": "message_stop"})
					var requests, callbacks int
					var payloads []string
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						require.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
						body, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						payloads = append(payloads, string(body))
						kind, data := "text/event-stream", stream.String()
						if requests > 1 {
							kind, data = "application/json", `{"content":[],"stop_reason":"end_turn"}`
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(data))}, nil
					})}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}, Tools: []Tool{{Name: "read", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}}
					opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
						callbacks++
						payloads = append(payloads, payload.(string))
						return nil
					})}
					var events []string
					var message AssistantMessage
					err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
						var label string
						var contentIndex int
						switch e := event.(type) {
						case Start:
							label = "start"
						case TextStart:
							label, contentIndex = "text_start", e.ContentIndex
						case TextDelta:
							label, contentIndex = "text_delta", e.ContentIndex
						case TextEnd:
							label, contentIndex = "text_end", e.ContentIndex
						case ThinkingStart:
							label, contentIndex = "thinking_start", e.ContentIndex
						case ThinkingDelta:
							label, contentIndex = "thinking_delta", e.ContentIndex
						case ThinkingEnd:
							label, contentIndex = "thinking_end", e.ContentIndex
						case ToolCallStart:
							label, contentIndex = "tool_start", e.ContentIndex
						case ToolCallDelta:
							label, contentIndex = "tool_delta", e.ContentIndex
						case ToolCallEnd:
							label, contentIndex = "tool_end", e.ContentIndex
							assert.Equal(t, want.Arguments, e.ToolCall.Arguments)
						case Done:
							label, message = "done", e.Message
						case StreamError:
							t.Errorf("unexpected stream error: %v", e)
						default:
							t.Errorf("unexpected event: %T", event)
						}
						assert.Zero(t, contentIndex)
						events = append(events, label)
					}, opts...)
					require.NoError(t, err)
					wantEvents := []string{"start", startEvent}
					if accepted && delta != "signature_delta" {
						wantEvents = append(wantEvents, deltaEvent)
					}
					wantEvents = append(wantEvents, deltaEvent, endEvent, "done")
					assert.Equal(t, wantEvents, events)
					assert.Equal(t, []ContentBlock{want}, message.Content)
					assert.Equal(t, "msg_route", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, 11, message.Usage.Input)
					assert.Equal(t, 7, message.Usage.Output)
					wantStop := StopReasonStop
					if target == "tool_use" {
						wantStop = StopReasonToolUse
					}
					assert.Equal(t, wantStop, message.StopReason)
					prompt.Messages = append(prompt.Messages, Message{Role: "assistant", Content: message.Content, Provider: message.Provider, API: message.API, Model: message.Model})
					if target == "tool_use" {
						prompt.Messages = append(prompt.Messages, Message{Role: "toolResult", ToolCallID: "call_1", ToolName: "read", Content: "result"})
					} else {
						prompt.Messages = append(prompt.Messages, Message{Role: "user", Content: "next"})
					}
					before, err := json.Marshal(prompt)
					require.NoError(t, err)
					result, err := client.Complete(t.Context(), model, prompt, opts...)
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, StopReasonStop, result.StopReason)
					require.Equal(t, 2, requests)
					require.Equal(t, 2, callbacks)
					require.Len(t, payloads, 4)
					wire := map[string]any{"type": "text", "text": want.Text}
					tail := map[string]any{"type": "text", "text": "next"}
					switch target {
					case "thinking":
						wire = map[string]any{"type": "thinking", "thinking": want.Thinking, "signature": want.ThoughtSignature}
					case "redacted_thinking":
						wire = map[string]any{"type": "redacted_thinking", "data": want.ThoughtSignature}
					case "tool_use":
						wire = map[string]any{"type": "tool_use", "id": "call_1", "name": "read", "input": json.RawMessage(`{"n":9007199254740993}`)}
						tail = map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "result"}
					}
					wantMessages, err := json.Marshal([]any{
						map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "question"}}},
						map[string]any{"role": "assistant", "content": []any{wire}},
						map[string]any{"role": "user", "content": []any{tail}},
					})
					require.NoError(t, err)
					for _, payload := range payloads[2:] {
						var body struct {
							Messages json.RawMessage `json:"messages"`
						}
						require.NoError(t, json.Unmarshal([]byte(payload), &body))
						assert.JSONEq(t, string(wantMessages), string(body.Messages))
						if target == "tool_use" {
							assert.Contains(t, string(body.Messages), `{"n":9007199254740993}`)
						}
					}
					after, err := json.Marshal(prompt)
					require.NoError(t, err)
					require.JSONEq(t, string(before), string(after))
				})
			}
		}
	}
}

func TestAnthropicReturnedModelKeepsRequestedIdentityAndPricing(t *testing.T) {
	for _, format := range []string{"json", "sse"} {
		for _, returned := range []string{"source", "allowed-fallback", "proxy-alias"} {
			t.Run(format+"/"+returned, func(t *testing.T) {
				model := &Model{
					ID: "source", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096,
					Cost:   CostProfile{Input: 10, Output: 20},
					Compat: &CompatibilitySettings{AllowedFallbackModels: []FallbackModel{{Provider: ProviderAnthropic, Model: "allowed-fallback", Cost: CostProfile{Input: 1, Output: 2}}}},
				}
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				before, err := json.Marshal([]any{model, prompt})
				require.NoError(t, err)
				var requests, payloads, responses, closes, terminals int
				var payload string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, requests-1, closes)
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					assert.Equal(t, "test-key", req.Header.Get("x-api-key"))
					assert.NotContains(t, req.Header.Get("anthropic-beta"), "server-side-fallback")
					wire, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, payload, string(wire))
					var body map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(wire, &body))
					assert.JSONEq(t, `"source"`, string(body["model"]))
					assert.Equal(t, "true", string(body["stream"]))
					assert.NotContains(t, body, "fallbacks")
					want := `[{"role":"user","content":[{"type":"text","text":"question"}]}]`
					responseModel := returned
					if requests == 2 {
						responseModel = "source"
						want = `[{"role":"user","content":[{"type":"text","text":"question"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"reasoning","signature":"opaque-signature"},{"type":"text","text":"answer"}]},{"role":"user","content":[{"type":"text","text":"next"}]}]`
					}
					assert.JSONEq(t, want, string(body["messages"]))
					contentType := "application/json"
					response := fmt.Sprintf(`{"id":"msg_identity","model":%q,"content":[{"type":"thinking","thinking":"reasoning","signature":"opaque-signature"},{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20}}`, responseModel)
					if format == "sse" {
						contentType = "text/event-stream"
						response = fmt.Sprintf("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_identity\",\"model\":%q,\"usage\":{\"input_tokens\":100}}}\n\n", responseModel) +
							"data: {\"type\":\"content_block_start\",\"index\":4,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"reasoning\",\"signature\":\"opaque-signature\"}}\n\n" +
							"data: {\"type\":\"content_block_stop\",\"index\":4}\n\n" +
							"data: {\"type\":\"content_block_start\",\"index\":7,\"content_block\":{\"type\":\"text\",\"text\":\"answer\"}}\n\n" +
							"data: {\"type\":\"content_block_stop\",\"index\":7}\n\n" +
							"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n" +
							"data: {\"type\":\"message_stop\"}\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(response)), closes: &closes}}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(value any) any { payloads++; payload = value.(string); return nil }), WithResponseCallback(func(response ProviderResponse) {
					responses++
					assert.Equal(t, http.StatusOK, response.Status)
					assert.Equal(t, requests-1, closes)
				})}
				check := func(message AssistantMessage) {
					t.Helper()
					assert.Equal(t, "source", message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, "msg_identity", message.ResponseID)
					assert.Equal(t, StopReasonStop, message.StopReason)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockThinking, Thinking: "reasoning", ThoughtSignature: "opaque-signature"}, {Type: ContentBlockText, Text: "answer"}}, message.Content)
					assert.Equal(t, Usage{Input: 100, Output: 20, TotalTokens: 120, Cost: Cost{Input: 0.001, Output: 0.0004, Total: 0.0014}}, message.Usage)
					assert.Empty(t, message.Diagnostics)
				}
				var message *AssistantMessage
				err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case ThinkingStart:
						assert.Equal(t, "source", e.Partial.Model)
						assert.Equal(t, Usage{Input: 100, TotalTokens: 100, Cost: Cost{Input: 0.001, Total: 0.001}}, e.Partial.Usage)
					case Done:
						terminals++
						assert.Equal(t, 1, responses)
						message = &e.Message
					case StreamError:
						t.Errorf("unexpected stream error: %v", e.Err)
					}
				}, opts...)
				require.NoError(t, err)
				require.NotNil(t, message)
				check(*message)
				encoded, err := json.Marshal(message)
				require.NoError(t, err)
				var restored Message
				require.NoError(t, json.Unmarshal(encoded, &restored))
				replay := &Context{Messages: []Message{prompt.Messages[0], restored, {Role: "user", Content: "next"}}}
				replayBefore, err := json.Marshal(replay)
				require.NoError(t, err)
				replayed, err := CompleteWithModel(t.Context(), model, replay, opts...)
				require.NoError(t, err)
				require.NotNil(t, replayed)
				check(*replayed)
				assert.Equal(t, []int{2, 2, 2, 2, 1}, []int{requests, payloads, responses, closes, terminals})
				after, err := json.Marshal([]any{model, prompt})
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
				replayAfter, err := json.Marshal(replay)
				require.NoError(t, err)
				assert.Equal(t, string(replayBefore), string(replayAfter))
				encodedAfter, err := json.Marshal(message)
				require.NoError(t, err)
				assert.Equal(t, string(encoded), string(encodedAfter))
			})
		}
	}
}

func TestAnthropicFallbackReachesTransport(t *testing.T) {
	for _, prefix := range []string{"none", "text", "empty_text", "thinking", "tool_use", "unknown_control"} {
		for _, ending := range []string{"complete", "truncated"} {
			t.Run(prefix+"/"+ending, func(t *testing.T) {
				model := &Model{ID: "source", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
				var stream strings.Builder
				emit := func(event any) {
					data, err := json.Marshal(event)
					require.NoError(t, err)
					stream.WriteString("data: " + string(data) + "\n\n")
				}
				emit(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_fallback", "usage": map[string]any{"input_tokens": 11}}})
				want := []ContentBlock{}
				wantEvents := []string{"start"}
				if prefix != "none" {
					block := map[string]any{"type": "text", "text": "partial"}
					want = []ContentBlock{{Type: ContentBlockText, Text: "partial"}}
					start, end := "text_start", "text_end"
					switch prefix {
					case "empty_text":
						block["text"] = ""
						want[0].Text = ""
					case "thinking":
						block = map[string]any{"type": "thinking", "thinking": "reason", "signature": "opaque"}
						want = []ContentBlock{{Type: ContentBlockThinking, Thinking: "reason", ThoughtSignature: "opaque"}}
						start, end = "thinking_start", "thinking_end"
					case "tool_use":
						block = map[string]any{"type": "tool_use", "id": "call_1", "name": "read", "input": json.RawMessage(`{"n":9007199254740993}`)}
						want = []ContentBlock{{Type: ContentBlockToolCall, ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"n":9007199254740993}`)}}
						start, end = "tool_start", "tool_end"
					}
					emit(map[string]any{"type": "content_block_start", "index": 4, "content_block": block})
					emit(map[string]any{"type": "content_block_stop", "index": 4})
					wantEvents = append(wantEvents, start, end)
				}
				kind := "fallback"
				if prefix == "unknown_control" {
					kind = "server_tool_use"
				}
				emit(map[string]any{"type": "content_block_start", "index": 7, "content_block": map[string]any{"type": kind, "from": map[string]any{"model": "source"}, "to": map[string]any{"model": "other"}}})
				emit(map[string]any{"type": "content_block_stop", "index": 7})
				midFallback := prefix != "none" && prefix != "unknown_control"
				if ending == "complete" {
					emit(map[string]any{"type": "content_block_start", "index": 9, "content_block": map[string]any{"type": "text", "text": "after"}})
					emit(map[string]any{"type": "content_block_stop", "index": 9})
					emit(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 7}})
					emit(map[string]any{"type": "message_stop"})
					if !midFallback {
						want = append(want, ContentBlock{Type: ContentBlockText, Text: "after"})
						wantEvents = append(wantEvents, "text_start", "text_end")
					}
				}
				wantReason, wantError, wantOutput := StopReasonStop, "", 7
				if midFallback {
					wantReason, wantError, wantOutput = StopReasonError, "anthropic performed an unsupported mid-output model fallback", 0
				} else if ending == "truncated" {
					wantReason, wantError, wantOutput = StopReasonError, "anthropic stream ended before message_stop", 0
				}
				if wantError == "" {
					wantEvents = append(wantEvents, "done")
				} else {
					wantEvents = append(wantEvents, "error")
				}
				var requests, callbacks int
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream.String()))}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
					callbacks++
					var body struct {
						Messages json.RawMessage `json:"messages"`
					}
					require.NoError(t, json.Unmarshal([]byte(payload.(string)), &body))
					assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(body.Messages))
					return nil
				})}
				check := func(message AssistantMessage) {
					assert.Equal(t, want, message.Content)
					assert.Equal(t, wantReason, message.StopReason)
					assert.Equal(t, wantError, message.ErrorMessage)
					assert.Equal(t, "msg_fallback", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, 11, message.Usage.Input)
					assert.Equal(t, wantOutput, message.Usage.Output)
				}
				var events []string
				err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case Start:
						events = append(events, "start")
					case TextStart:
						events = append(events, "text_start")
					case TextEnd:
						events = append(events, "text_end")
					case ThinkingStart:
						events = append(events, "thinking_start")
					case ThinkingEnd:
						events = append(events, "thinking_end")
					case ToolCallStart:
						events = append(events, "tool_start")
					case ToolCallEnd:
						events = append(events, "tool_end")
					case Done:
						events = append(events, "done")
						check(e.Message)
					case StreamError:
						events = append(events, "error")
						assert.EqualError(t, e.Err, wantError)
						check(e.Msg)
					default:
						t.Errorf("unexpected event: %T", event)
					}
				}, opts...)
				require.NoError(t, err)
				assert.Equal(t, wantEvents, events)
				message, err := client.Complete(t.Context(), model, prompt, opts...)
				require.NoError(t, err)
				require.NotNil(t, message)
				check(*message)
				assert.Equal(t, 2, requests)
				assert.Equal(t, 2, callbacks)
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				assert.JSONEq(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicClosedBlocksReachesTransport(t *testing.T) {
	for _, target := range []string{"text", "thinking", "redacted_thinking", "tool_use"} {
		for _, mode := range []string{"live", "late", "duplicate_stop", "reuse"} {
			t.Run(target+"/"+mode, func(t *testing.T) {
				model := &Model{ID: "source", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
				block := map[string]any{"type": "text", "text": "answer"}
				want := ContentBlock{Type: ContentBlockText, Text: "answer ok"}
				delta := map[string]any{"type": "text_delta", "text": " ok"}
				start, change, end := "text_start", "text_delta", "text_end"
				switch target {
				case "thinking", "redacted_thinking":
					block = map[string]any{"type": target, "thinking": "reason", "signature": "opaque", "data": "opaque"}
					want = ContentBlock{Type: ContentBlockThinking, Thinking: "reason ok", ThoughtSignature: "opaque:signed"}
					if target == "redacted_thinking" {
						want.Thinking, want.Redacted = "[Reasoning redacted] ok", true
					}
					delta = map[string]any{"type": "thinking_delta", "thinking": " ok"}
					start, change, end = "thinking_start", "thinking_delta", "thinking_end"
				case "tool_use":
					block = map[string]any{"type": target, "id": "call_1", "name": "read", "input": map[string]any{}}
					want = ContentBlock{Type: ContentBlockToolCall, ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"n":9007199254740993}`)}
					delta = map[string]any{"type": "input_json_delta", "partial_json": string(want.Arguments)}
					start, change, end = "tool_start", "tool_delta", "tool_end"
				}
				var stream strings.Builder
				emit := func(event any) {
					data, err := json.Marshal(event)
					require.NoError(t, err)
					stream.WriteString("data: " + string(data) + "\n\n")
				}
				startBlock := func() { emit(map[string]any{"type": "content_block_start", "index": 11, "content_block": block}) }
				stopBlock := func() { emit(map[string]any{"type": "content_block_stop", "index": 11}) }
				addDelta := func() {
					emit(map[string]any{"type": "content_block_delta", "index": 11, "delta": delta})
					if want.Type == ContentBlockThinking {
						emit(map[string]any{"type": "content_block_delta", "index": 11, "delta": map[string]any{"type": "signature_delta", "signature": ":signed"}})
					}
				}
				emit(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_closed", "usage": map[string]any{"input_tokens": 11}}})
				startBlock()
				if mode == "live" {
					addDelta()
				}
				if mode == "live" {
					switch want.Type {
					case ContentBlockText:
						want.Text += " ok"
					case ContentBlockThinking:
						want.Thinking += " ok"
						want.ThoughtSignature += ":signed"
					case ContentBlockToolCall:
						delta["partial_json"] = " "
						want.Arguments = append(want.Arguments, ' ')
					}
				}
				addDelta()
				stopBlock()
				wantBlocks := []ContentBlock{want}
				wantEvents := []string{"start", start + "/0", change + "/0"}
				if mode == "live" {
					wantEvents = append(wantEvents, change+"/0")
				}
				wantEvents = append(wantEvents, end+"/0")
				switch mode {
				case "late":
					addDelta()
				case "duplicate_stop":
					stopBlock()
				case "reuse":
					startBlock()
					addDelta()
					stopBlock()
					wantBlocks = append(wantBlocks, want)
					wantEvents = append(wantEvents, start+"/1", change+"/1", end+"/1")
				}
				emit(map[string]any{"type": "content_block_start", "index": 27, "content_block": map[string]any{"type": "text", "text": "tail"}})
				emit(map[string]any{"type": "content_block_delta", "index": 27, "delta": map[string]any{"type": "text_delta", "text": " ok"}})
				emit(map[string]any{"type": "content_block_stop", "index": 27})
				wantEvents = append(wantEvents, fmt.Sprintf("text_start/%d", len(wantBlocks)), fmt.Sprintf("text_delta/%d", len(wantBlocks)), fmt.Sprintf("text_end/%d", len(wantBlocks)), "done")
				wantBlocks = append(wantBlocks, ContentBlock{Type: ContentBlockText, Text: "tail ok"})
				emit(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 7}})
				emit(map[string]any{"type": "message_stop"})
				checkRequest := func(payload string) {
					var body struct {
						Messages json.RawMessage `json:"messages"`
					}
					require.NoError(t, json.Unmarshal([]byte(payload), &body))
					assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(body.Messages))
				}
				var requests, callbacks int
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					body, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					checkRequest(string(body))
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream.String()))}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
					callbacks++
					checkRequest(payload.(string))
					return nil
				})}
				check := func(message AssistantMessage) {
					assert.Equal(t, wantBlocks, message.Content)
					assert.Equal(t, StopReasonStop, message.StopReason)
					assert.Empty(t, message.ErrorMessage)
					assert.Equal(t, "msg_closed", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, 11, message.Usage.Input)
					assert.Equal(t, 7, message.Usage.Output)
				}
				var events []string
				err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					var label string
					var index int
					switch e := event.(type) {
					case Start:
						events = append(events, "start")
						return
					case TextStart:
						label, index = "text_start", e.ContentIndex
					case TextDelta:
						label, index = "text_delta", e.ContentIndex
					case TextEnd:
						label, index = "text_end", e.ContentIndex
						require.Less(t, index, len(wantBlocks))
						assert.Equal(t, wantBlocks[index].Text, e.Content)
					case ThinkingStart:
						label, index = "thinking_start", e.ContentIndex
					case ThinkingDelta:
						label, index = "thinking_delta", e.ContentIndex
					case ThinkingEnd:
						label, index = "thinking_end", e.ContentIndex
						require.Less(t, index, len(wantBlocks))
						require.Len(t, e.Partial.Content, index+1)
						assert.Equal(t, wantBlocks[index], e.Partial.Content[index])
					case ToolCallStart:
						label, index = "tool_start", e.ContentIndex
					case ToolCallDelta:
						label, index = "tool_delta", e.ContentIndex
					case ToolCallEnd:
						label, index = "tool_end", e.ContentIndex
						require.Less(t, index, len(wantBlocks))
						assert.Equal(t, wantBlocks[index].Arguments, e.ToolCall.Arguments)
					case Done:
						events = append(events, "done")
						check(e.Message)
						return
					default:
						t.Errorf("unexpected event: %T", event)
						return
					}
					events = append(events, fmt.Sprintf("%s/%d", label, index))
				}, opts...)
				require.NoError(t, err)
				assert.Equal(t, wantEvents, events)
				message, err := client.Complete(t.Context(), model, prompt, opts...)
				require.NoError(t, err)
				require.NotNil(t, message)
				check(*message)
				assert.Equal(t, 2, requests)
				assert.Equal(t, 2, callbacks)
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				assert.JSONEq(t, string(before), string(after))
			})
		}
	}
}

type anthropicCancelBody struct {
	*strings.Reader
	beforeRead func()
	closes     *int
}

func (b *anthropicCancelBody) Read(p []byte) (int, error) {
	if b.beforeRead != nil {
		b.beforeRead()
		b.beforeRead = nil
	}
	return b.Reader.Read(p)
}

func (b *anthropicCancelBody) Close() error {
	(*b.closes)++
	return nil
}

func TestAnthropicAcceptedBodyCloseFailureReachesTransport(t *testing.T) {
	for _, format := range []string{"json", "sse"} {
		for _, outcome := range []string{"success", "refusal", "malformed"} {
			t.Run(format+"/"+outcome, func(t *testing.T) {
				model := &Model{ID: "claude-close", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				before, err := json.Marshal([]any{model, prompt})
				require.NoError(t, err)
				closeErr := errors.New("accepted body close failed")
				var requests, payloads, responses, closes, terminals int
				var payload string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, requests-1, closes)
					wire, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, payload, string(wire))
					var request struct{ Messages json.RawMessage }
					require.NoError(t, json.Unmarshal(wire, &request))
					assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(request.Messages))
					reason := "end_turn"
					if outcome == "refusal" {
						reason = "refusal"
					}
					body := `{"id":"msg_close","content":[{"type":"text","text":"answer"}],"usage":{"input_tokens":11,"output_tokens":7},"stop_reason":"` + reason + `","stop_details":{"explanation":"declined"}}`
					kind := "application/json"
					if format == "sse" {
						kind = "text/event-stream"
						body = "data: " + `{"type":"message_start","message":{"id":"msg_close","usage":{"input_tokens":11,"output_tokens":7}}}` + "\n\n" +
							"data: " + `{"type":"content_block_start","index":3,"content_block":{"type":"text","text":"answer"}}` + "\n\n" +
							"data: " + `{"type":"content_block_stop","index":3}` + "\n\n" +
							"data: " + `{"type":"message_delta","delta":{"stop_reason":"` + reason + `","stop_details":{"explanation":"declined"}}}` + "\n\n" +
							"data: " + `{"type":"message_stop"}` + "\n\n"
					}
					if outcome == "malformed" {
						if format == "json" {
							body = `{"id":]}`
						} else {
							body += "data: {\"type\":]}\n\n"
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {kind}, "X-Should-Retry": {"true"}, "Retry-After-Ms": {"0"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(body)), closes: &closes, closeErr: closeErr}}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxRetries(2), WithPayloadCallback(func(value any) any {
					payloads++
					payload = value.(string)
					return nil
				}), WithResponseCallback(func(response ProviderResponse) {
					responses++
					assert.Equal(t, http.StatusOK, response.Status)
					assert.Equal(t, requests-1, closes)
				}), WithClient(client)}
				err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					assert.Equal(t, 1, responses)
					assert.Zero(t, closes)
					var message AssistantMessage
					switch e := event.(type) {
					case Done:
						terminals++
						assert.Equal(t, "success", outcome)
						assert.Equal(t, StopReasonStop, e.Reason)
						message = e.Message
					case StreamError:
						terminals++
						assert.NotErrorIs(t, e.Err, closeErr)
						message = e.Msg
						assert.Equal(t, StopReasonError, message.StopReason)
						if outcome == "refusal" {
							assert.EqualError(t, e.Err, "declined")
						} else {
							assert.Contains(t, e.Err.Error(), "could not parse anthropic SSE event")
						}
					default:
						return
					}
					assert.Equal(t, "msg_close", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
					assert.Equal(t, 11, message.Usage.Input)
					assert.Equal(t, 7, message.Usage.Output)
				}, opts...)
				assert.ErrorIs(t, err, closeErr)
				wantTerminals := 1
				if format == "json" && outcome == "malformed" {
					wantTerminals = 0
					var syntaxErr *json.SyntaxError
					assert.ErrorAs(t, err, &syntaxErr)
					assert.Contains(t, err.Error(), "decode response")
				}
				assert.Equal(t, wantTerminals, terminals)
				message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
				assert.ErrorIs(t, err, closeErr)
				assert.Nil(t, message)
				if format == "json" && outcome == "malformed" {
					var syntaxErr *json.SyntaxError
					assert.ErrorAs(t, err, &syntaxErr)
				}
				assert.Equal(t, []int{2, 2, 2, 2}, []int{requests, payloads, responses, closes})
				after, err := json.Marshal([]any{model, prompt})
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicJSONCancellationReachesTransport(t *testing.T) {
	for _, format := range []string{"json", "sse"} {
		for _, outcome := range []string{"end_turn", "refusal"} {
			for _, boundary := range []string{"active", "response", "read"} {
				t.Run(format+"/"+outcome+"/"+boundary, func(t *testing.T) {
					model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
					prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
					before, err := json.Marshal(prompt)
					require.NoError(t, err)
					const jsonBody = `{"id":"msg_cancel","content":[{"type":"text","text":"answer"}],"usage":{"input_tokens":11,"output_tokens":7},"stop_reason":"%s","stop_details":{"explanation":"declined"}}`
					body, contentType := fmt.Sprintf(jsonBody, outcome), "application/json"
					if format == "sse" {
						body = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cancel\",\"usage\":{\"input_tokens\":11}}}\n\n" +
							"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"answer\"}}\n\n" +
							"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
							fmt.Sprintf("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q,\"stop_details\":{\"explanation\":\"declined\"}},\"usage\":{\"output_tokens\":7}}\n\n", outcome) +
							"data: {\"type\":\"message_stop\"}\n\n"
						contentType = "text/event-stream"
					}
					var requests, callbacks, responses, closes int
					var cancel context.CancelCauseFunc
					cause := fmt.Errorf("caller stopped reading")
					checkRequest := func(payload string) {
						var request struct {
							Messages json.RawMessage `json:"messages"`
						}
						require.NoError(t, json.Unmarshal([]byte(payload), &request))
						assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(request.Messages))
					}
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
						payload, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						checkRequest(string(payload))
						reader := &anthropicCancelBody{Reader: strings.NewReader(body), closes: &closes}
						if boundary == "read" {
							reader.beforeRead = func() { cancel(cause) }
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: reader}, nil
					})}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
						callbacks++
						checkRequest(payload.(string))
						return nil
					}), WithResponseCallback(func(response ProviderResponse) {
						responses++
						if boundary == "response" {
							cancel(cause)
						}
					})}
					check := func(message AssistantMessage) {
						assert.Equal(t, model.ID, message.Model)
						assert.Equal(t, model.API, message.API)
						assert.Equal(t, model.Provider, message.Provider)
						if boundary != "active" {
							assert.Equal(t, StopReasonAborted, message.StopReason)
							assert.Contains(t, message.ErrorMessage, cause.Error())
						} else if outcome == "refusal" {
							assert.Equal(t, StopReasonError, message.StopReason)
							assert.Equal(t, "declined", message.ErrorMessage)
						} else {
							assert.Equal(t, StopReasonStop, message.StopReason)
							assert.Empty(t, message.ErrorMessage)
						}
						if format == "json" || boundary == "active" {
							assert.Equal(t, "msg_cancel", message.ResponseID)
							assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
							assert.Equal(t, 11, message.Usage.Input)
							assert.Equal(t, 7, message.Usage.Output)
						}
					}
					for _, api := range []string{"Stream", "Complete"} {
						ctx, stop := context.WithCancelCause(t.Context())
						cancel = stop
						var message *AssistantMessage
						if api == "Complete" {
							message, err = client.Complete(ctx, model, prompt, opts...)
						} else {
							var terminals int
							err = client.Stream(ctx, model, prompt, func(_ context.Context, event StreamEvent) {
								switch e := event.(type) {
								case Done:
									terminals++
									assert.Equal(t, "active", boundary)
									assert.Equal(t, "end_turn", outcome)
									message = &e.Message
								case StreamError:
									terminals++
									message = &e.Msg
									if boundary != "active" {
										assert.ErrorIs(t, e.Err, cause)
									}
								}
							}, opts...)
							require.Equal(t, 1, terminals)
						}
						require.NoError(t, err)
						require.NotNil(t, message)
						check(*message)
						stop(nil)
					}
					assert.Equal(t, 2, requests)
					assert.Equal(t, 2, callbacks)
					assert.Equal(t, 2, responses)
					assert.Equal(t, 2, closes)
					after, err := json.Marshal(prompt)
					require.NoError(t, err)
					assert.JSONEq(t, string(before), string(after))
				})
			}
		}
	}
}

type anthropicEOFBody struct {
	io.Reader
	closes   *int
	closeErr error
}

func (b *anthropicEOFBody) Close() error {
	(*b.closes)++
	return b.closeErr
}

func TestAnthropicSSELineEndingsReachTransport(t *testing.T) {
	for _, ending := range []struct{ name, value string }{{"lf", "\n"}, {"crlf", "\r\n"}, {"cr", "\r"}} {
		for _, tail := range []string{"blank", "newline", "unterminated", "missing stop", "malformed"} {
			for _, readerMode := range []string{"regular", "data with EOF"} {
				t.Run(ending.name+"/"+tail+"/"+readerMode, func(t *testing.T) {
					lines := []string{
						`event: message_start`,
						`data: {"type":"message_start","message":{"id":"msg_eof","usage":{"input_tokens":11}}}`,
						"",
						`data: {"type":"content_block_start","index":11,"content_block":{"type":"text","text":"answer"}}`,
						"",
						`data: {"type":"content_block_stop","index":11}`,
						"",
						`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
						"",
					}
					if tail == "malformed" {
						lines = append(lines, `data: {"type":"message_stop"`)
					} else if tail != "missing stop" {
						lines = append(lines, `data: {"type":"message_stop"}`)
					}
					body := strings.Join(lines, ending.value)
					if tail == "blank" {
						body += ending.value + ending.value
					} else if tail == "newline" {
						body += ending.value
					}
					var requests, callbacks, closes int
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					checkPayload := func(payload string) {
						var request struct{ Messages json.RawMessage }
						require.NoError(t, json.Unmarshal([]byte(payload), &request))
						assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(request.Messages))
					}
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
						payload, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						checkPayload(string(payload))
						var reader io.Reader = strings.NewReader(body)
						if readerMode == "data with EOF" {
							reader = iotest.OneByteReader(iotest.DataErrReader(reader))
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &anthropicEOFBody{Reader: reader, closes: &closes}}, nil
					})}
					model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
					before, err := json.Marshal(prompt)
					require.NoError(t, err)
					opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
						callbacks++
						checkPayload(payload.(string))
						return nil
					})}
					for _, api := range []string{"Stream", "Complete"} {
						var message *AssistantMessage
						if api == "Complete" {
							message, err = client.Complete(t.Context(), model, prompt, opts...)
						} else {
							var terminals, starts, ends int
							err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
								switch e := event.(type) {
								case TextStart:
									starts++
									assert.Equal(t, 0, e.ContentIndex)
								case TextEnd:
									ends++
									assert.Equal(t, "answer", e.Content)
								case Done:
									terminals++
									message = &e.Message
								case StreamError:
									terminals++
									message = &e.Msg
								}
							}, opts...)
							assert.Equal(t, 1, terminals)
							assert.Equal(t, 1, starts)
							assert.Equal(t, 1, ends)
						}
						require.NoError(t, err)
						require.NotNil(t, message)
						assert.Equal(t, "msg_eof", message.ResponseID)
						assert.Equal(t, model.ID, message.Model)
						assert.Equal(t, model.API, message.API)
						assert.Equal(t, model.Provider, message.Provider)
						assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
						assert.Equal(t, 11, message.Usage.Input)
						assert.Equal(t, 7, message.Usage.Output)
						switch tail {
						case "missing stop":
							assert.Equal(t, StopReasonError, message.StopReason)
							assert.Equal(t, "anthropic stream ended before message_stop", message.ErrorMessage)
						case "malformed":
							assert.Equal(t, StopReasonError, message.StopReason)
							assert.Contains(t, message.ErrorMessage, "could not parse anthropic SSE event")
						default:
							assert.Equal(t, StopReasonStop, message.StopReason)
							assert.Empty(t, message.ErrorMessage)
						}
					}
					assert.Equal(t, 2, requests)
					assert.Equal(t, 2, callbacks)
					assert.Equal(t, 2, closes)
					after, err := json.Marshal(prompt)
					require.NoError(t, err)
					assert.JSONEq(t, string(before), string(after))
				})
			}
		}
	}
}

func TestAnthropicDefaultThinkingBudgetReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name, modelID, thinking string
		opts                    []Option
		cap, maxTokens, budget  int
	}{
		{name: "enabled default", opts: []Option{WithThinkingEnabled(true)}, thinking: "enabled", maxTokens: 5024, budget: 1024},
		{name: "enabled zero token option", opts: []Option{WithThinkingEnabled(true), WithThinkingBudgetTokens(0)}, thinking: "enabled", maxTokens: 5024, budget: 1024},
		{name: "explicit budget", opts: []Option{WithThinkingEnabled(true), WithThinkingBudgetTokens(3000)}, thinking: "enabled", maxTokens: 7000, budget: 3000},
		{name: "generic reasoning", opts: []Option{WithThinkingEnabled(true), WithReasoning(ReasoningMedium)}, thinking: "enabled", maxTokens: 12192, budget: 8192},
		{name: "configured zero", opts: []Option{WithThinkingEnabled(true), WithReasoning(ReasoningLow), WithThinkingBudgets(&ThinkingBudgets{Low: new(0)})}, thinking: "enabled", maxTokens: 4000},
		{name: "configured budget", opts: []Option{WithThinkingEnabled(true), WithReasoning(ReasoningLow), WithThinkingBudgets(&ThinkingBudgets{Low: new(2000)})}, thinking: "enabled", maxTokens: 6000, budget: 2000},
		{name: "model cap", opts: []Option{WithThinkingEnabled(true), WithMaxTokens(1200)}, cap: 1536, thinking: "enabled", maxTokens: 1536, budget: 512},
		{name: "adaptive", modelID: "claude-opus-4-6", opts: []Option{WithThinkingEnabled(true)}, thinking: "adaptive", maxTokens: 4000},
		{name: "no thinking", maxTokens: 4000},
		{name: "explicit off", opts: []Option{WithReasoning(ReasoningOff)}, maxTokens: 4000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &Model{ID: "claude-sonnet-4-5", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 32000}
			if tc.modelID != "" {
				model.ID = tc.modelID
			}
			if tc.thinking == "adaptive" {
				model.Compat = &CompatibilitySettings{ForceAdaptiveThinking: true}
			}
			if tc.cap > 0 {
				model.MaxTokens = tc.cap
			}
			prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
			before, err := json.Marshal(prompt)
			require.NoError(t, err)
			checkPayload := func(payload string) {
				var request anthropicRequest
				require.NoError(t, json.Unmarshal([]byte(payload), &request))
				assert.Equal(t, model.ID, request.Model)
				assert.True(t, request.Stream)
				assert.Equal(t, tc.maxTokens, request.MaxTokens)
				if tc.thinking == "" {
					assert.Nil(t, request.Thinking)
					require.NotNil(t, request.Temperature)
					assert.Equal(t, 0.4, *request.Temperature)
				} else {
					require.NotNil(t, request.Thinking)
					assert.Equal(t, tc.thinking, request.Thinking.Type)
					assert.Equal(t, tc.budget, request.Thinking.BudgetTokens)
					assert.Equal(t, "summarized", request.Thinking.Display)
					assert.Nil(t, request.Temperature)
				}
				var wire struct{ Messages json.RawMessage }
				require.NoError(t, json.Unmarshal([]byte(payload), &wire))
				assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(wire.Messages))
			}
			var requests, callbacks int
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
				payload, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				checkPayload(string(payload))
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_budget","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7}}`))}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxTokens(4000), WithTemperature(0.4), WithPayloadCallback(func(payload any) any {
				callbacks++
				checkPayload(payload.(string))
				return nil
			})}
			opts = append(opts, tc.opts...)
			for _, api := range []string{"Stream", "Complete"} {
				var message *AssistantMessage
				if api == "Complete" {
					message, err = client.Complete(t.Context(), model, prompt, opts...)
				} else {
					var terminals int
					err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
						switch e := event.(type) {
						case Done:
							terminals++
							message = &e.Message
						case StreamError:
							terminals++
							message = &e.Msg
						}
					}, opts...)
					assert.Equal(t, 1, terminals)
				}
				require.NoError(t, err)
				require.NotNil(t, message)
				assert.Equal(t, StopReasonStop, message.StopReason)
				assert.Empty(t, message.ErrorMessage)
				assert.Equal(t, "msg_budget", message.ResponseID)
				assert.Equal(t, model.ID, message.Model)
				assert.Equal(t, model.API, message.API)
				assert.Equal(t, model.Provider, message.Provider)
				assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
				assert.Equal(t, 11, message.Usage.Input)
				assert.Equal(t, 7, message.Usage.Output)
			}
			assert.Equal(t, 2, requests)
			assert.Equal(t, 2, callbacks)
			after, err := json.Marshal(prompt)
			require.NoError(t, err)
			assert.JSONEq(t, string(before), string(after))
		})
	}
}

func TestAnthropicToolResultContentReachesTransport(t *testing.T) {
	image := ContentBlock{Type: ContentBlockImage, MimeType: "image/png", Data: "aGVsbG8="}
	imageJSON := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}`
	for _, tc := range []struct {
		name, want string
		content    any
		textOnly   bool
	}{
		{name: "string", content: "result", want: `"result"`},
		{name: "empty string", content: "", want: `""`},
		{name: "nil", want: `""`},
		{name: "single text", content: []ContentBlock{{Type: ContentBlockText, Text: "result"}}, want: `"result"`},
		{name: "multiple text", content: []ContentBlock{{Type: ContentBlockText, Text: "first"}, {Type: ContentBlockText, Text: "second"}}, want: `"first\nsecond"`},
		{name: "empty texts", content: []ContentBlock{{Type: ContentBlockText}, {Type: ContentBlockText}}, want: `"\n"`},
		{name: "whitespace", content: []ContentBlock{{Type: ContentBlockText, Text: " \t"}, {Type: ContentBlockText, Text: "\n"}}, want: `" \t\n\n"`},
		{name: "image only", content: []ContentBlock{image}, want: `[{"type":"text","text":"(see attached image)"},` + imageJSON + `]`},
		{name: "multiple images", content: []ContentBlock{image, image}, want: `[{"type":"text","text":"(see attached image)"},` + imageJSON + `,` + imageJSON + `]`},
		{name: "image and empty text", content: []ContentBlock{image, {Type: ContentBlockText}}, want: `[` + imageJSON + `,{"type":"text"}]`},
		{name: "mixed order", content: []ContentBlock{{Type: ContentBlockText, Text: "before"}, image, {Type: ContentBlockText, Text: "after"}}, want: `[{"type":"text","text":"before"},` + imageJSON + `,{"type":"text","text":"after"}]`},
		{name: "unsupported images", content: []ContentBlock{image, image}, textOnly: true, want: `"(tool image omitted: model does not support images)"`},
		{name: "unsupported mixed", content: []ContentBlock{{Type: ContentBlockText, Text: "before"}, image, {Type: ContentBlockText, Text: "after"}}, textOnly: true, want: `"before\n(tool image omitted: model does not support images)\nafter"`},
	} {
		for _, role := range []string{"toolResult", "tool"} {
			t.Run(tc.name+"/"+role, func(t *testing.T) {
				model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Input: []InputType{InputText, InputImage}}
				if tc.textOnly {
					model.Input = []InputType{InputText}
				}
				prompt := &Context{Messages: []Message{
					{Role: "user", Content: "question"},
					{Role: "assistant", API: model.API, Provider: model.Provider, Model: model.ID, Content: []ContentBlock{
						{Type: ContentBlockToolCall, ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"n":9007199254740993}`)},
						{Type: ContentBlockToolCall, ID: "call_2", Name: "read", Arguments: json.RawMessage(`{}`)},
					}},
					{Role: role, ToolCallID: "call_1", ToolName: "read", IsError: true, Content: tc.content},
					{Role: role, ToolCallID: "call_2", ToolName: "read", Content: "last"},
				}}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				var callbackPayload string
				check := func(payload string) {
					t.Helper()
					var body struct {
						Model    string
						Stream   bool
						Messages json.RawMessage
					}
					require.NoError(t, json.Unmarshal([]byte(payload), &body))
					assert.Equal(t, model.ID, body.Model)
					assert.True(t, body.Stream)
					want := `[{"role":"user","content":[{"type":"text","text":"question"}]},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":{"n":9007199254740993}},{"type":"tool_use","id":"call_2","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":` + tc.want + `,"is_error":true},{"type":"tool_result","tool_use_id":"call_2","content":"last","cache_control":{"type":"ephemeral"}}]}]`
					assert.JSONEq(t, want, string(body.Messages))
					assert.Contains(t, payload, `9007199254740993`)
				}
				var requests, callbacks int
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					payload, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, callbackPayload, string(payload))
					check(string(payload))
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_result","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7}}`))}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithCacheRetention("short"), WithPayloadCallback(func(payload any) any {
					callbacks++
					callbackPayload = payload.(string)
					check(callbackPayload)
					return nil
				})}
				for _, api := range []string{"Stream", "Complete"} {
					var message *AssistantMessage
					if api == "Complete" {
						message, err = client.Complete(t.Context(), model, prompt, opts...)
					} else {
						var terminals int
						err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
							switch e := event.(type) {
							case Done:
								terminals++
								message = &e.Message
							case StreamError:
								terminals++
								message = &e.Msg
							}
						}, opts...)
						assert.Equal(t, 1, terminals)
					}
					require.NoError(t, err)
					require.NotNil(t, message)
					assert.Equal(t, StopReasonStop, message.StopReason)
					assert.Empty(t, message.ErrorMessage)
					assert.Equal(t, "msg_result", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
					assert.Equal(t, 11, message.Usage.Input)
					assert.Equal(t, 7, message.Usage.Output)
				}
				assert.Equal(t, 2, requests)
				assert.Equal(t, 2, callbacks)
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				assert.JSONEq(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicResponseToolInputPublication(t *testing.T) {
	for _, format := range []string{"sse", "json"} {
		for _, input := range []string{"missing", "null", "{}", "{ }", `{"value":9007199254740993}`} {
			t.Run(format+"/"+input, func(t *testing.T) {
				model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
				block := `{"type":"tool_use","id":"call_1","name":"read"`
				want := input
				if input != "missing" {
					block += `,"input":` + input
				}
				if input == "missing" || input == "null" {
					want = "{}"
				}
				block += "}"
				response := `{"id":"msg_input","content":[` + block + `],"stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":7}}`
				kind := "application/json"
				if format == "sse" {
					kind = "text/event-stream"
					response = "data: " + `{"type":"message_start","message":{"id":"msg_input","usage":{"input_tokens":11}}}` + "\n\n" +
						"data: " + `{"type":"content_block_start","index":3,"content_block":` + block + "}\n\n" +
						"data: " + `{"type":"content_block_stop","index":3}` + "\n\n" +
						"data: " + `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}` + "\n\n" +
						"data: " + `{"type":"message_stop"}` + "\n\n"
				}
				var requests, callbacks int
				var callbackPayload string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					payload, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, callbackPayload, string(payload))
					if requests == 3 {
						var body struct {
							Messages []struct {
								Content []struct{ Input json.RawMessage }
							}
						}
						require.NoError(t, json.Unmarshal(payload, &body))
						require.Len(t, body.Messages, 3)
						require.Len(t, body.Messages[1].Content, 1)
						assert.JSONEq(t, want, string(body.Messages[1].Content[0].Input))
						if strings.Contains(want, "9007199254740993") {
							assert.Contains(t, string(payload), "9007199254740993")
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"content":[],"stop_reason":"end_turn"}`))}, nil
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(response))}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}, Tools: []Tool{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
					callbacks++
					callbackPayload = payload.(string)
					return nil
				})}
				check := func(content []ContentBlock) {
					t.Helper()
					require.Len(t, content, 1)
					assert.Equal(t, ContentBlockToolCall, content[0].Type)
					assert.Equal(t, "call_1", content[0].ID)
					assert.Equal(t, "read", content[0].Name)
					assert.Equal(t, want, string(content[0].Arguments))
				}
				var message *AssistantMessage
				var starts, ends, terminals int
				err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case ToolCallStart:
						starts++
						assert.Equal(t, 0, e.ContentIndex)
						check(e.Partial.Content)
					case ToolCallEnd:
						ends++
						check(e.Partial.Content)
						assert.Equal(t, want, string(e.ToolCall.Arguments))
					case Done:
						terminals++
						message = &e.Message
					case StreamError:
						terminals++
						t.Errorf("unexpected stream error: %v", e)
					}
				}, opts...)
				require.NoError(t, err)
				assert.Equal(t, 1, terminals)
				if format == "sse" {
					assert.Equal(t, 1, starts)
					assert.Equal(t, 1, ends)
				}
				for _, api := range []string{"Stream", "Complete"} {
					if api == "Complete" {
						message, err = client.Complete(t.Context(), model, prompt, opts...)
						require.NoError(t, err)
					}
					require.NotNil(t, message)
					check(message.Content)
					assert.Equal(t, "msg_input", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, StopReasonToolUse, message.StopReason)
					assert.Equal(t, 11, message.Usage.Input)
					assert.Equal(t, 7, message.Usage.Output)
				}
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
				prompt.Messages = append(prompt.Messages, Message{Role: "assistant", Content: message.Content, API: message.API, Provider: message.Provider, Model: message.Model}, Message{Role: "toolResult", ToolCallID: "call_1", ToolName: "read", Content: "result"})
				before, err = json.Marshal(prompt)
				require.NoError(t, err)
				_, err = client.Complete(t.Context(), model, prompt, opts...)
				require.NoError(t, err)
				after, err = json.Marshal(prompt)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
				assert.Equal(t, 3, requests)
				assert.Equal(t, 3, callbacks)
			})
		}
	}
}

func TestAnthropicSSEDataFramingReachesTransport(t *testing.T) {
	for _, ending := range []struct{ name, value string }{{"lf", "\n"}, {"crlf", "\r\n"}, {"cr", "\r"}} {
		for _, framing := range []string{"single line", "multiple lines", "comments and fields", "empty data field", "unterminated", "separate JSON values"} {
			t.Run(ending.name+"/"+framing, func(t *testing.T) {
				payloads := []string{
					`{"type":"message_start","message":{"id":"msg_framing","usage":{"input_tokens":11}}}`,
					`{"type":"content_block_start","index":3,"content_block":{"type":"text","text":"answer"}}`,
					`{"type":"content_block_stop","index":3}`,
					`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
					`{"type":"message_stop"}`,
				}
				var lines []string
				for _, payload := range payloads {
					if framing == "single line" || framing == "separate JSON values" {
						lines = append(lines, "data: "+payload)
					} else {
						split := strings.Index(payload, ",")
						if split == -1 {
							split = len(payload) - 1
						}
						lines = append(lines, "data: "+payload[:split])
						if framing == "comments and fields" {
							lines = append(lines, ": heartbeat", "id: ignored", "retry: 12", "event: message_delta")
						}
						if framing == "empty data field" {
							lines = append(lines, "data")
						}
						lines = append(lines, "data:"+payload[split:])
					}
					if framing == "separate JSON values" && len(lines) == 1 {
						lines = append(lines, `data: {"type":"ping"}`)
					}
					lines = append(lines, "")
				}
				body := strings.Join(lines, ending.value) + ending.value
				if framing == "unterminated" {
					body = strings.TrimRight(body, "\r\n")
				}
				var requests, callbacks, closes int
				var callbackPayload string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					payload, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, callbackPayload, string(payload))
					var wire struct{ Messages json.RawMessage }
					require.NoError(t, json.Unmarshal(payload, &wire))
					assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(wire.Messages))
					reader := iotest.OneByteReader(iotest.DataErrReader(strings.NewReader(body)))
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &anthropicEOFBody{Reader: reader, closes: &closes}}, nil
				})}
				model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
					callbacks++
					callbackPayload = payload.(string)
					return nil
				})}
				for _, api := range []string{"Stream", "Complete"} {
					var message *AssistantMessage
					if api == "Complete" {
						message, err = client.Complete(t.Context(), model, prompt, opts...)
					} else {
						var terminals, starts, ends int
						err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
							switch e := event.(type) {
							case TextStart:
								starts++
								assert.Equal(t, 0, e.ContentIndex)
							case TextEnd:
								ends++
								assert.Equal(t, "answer", e.Content)
							case Done:
								terminals++
								message = &e.Message
							case StreamError:
								terminals++
								message = &e.Msg
							}
						}, opts...)
						assert.Equal(t, 1, terminals)
						if framing != "separate JSON values" {
							assert.Equal(t, 1, starts)
							assert.Equal(t, 1, ends)
						}
					}
					require.NoError(t, err)
					require.NotNil(t, message)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					if framing == "separate JSON values" {
						assert.Equal(t, StopReasonError, message.StopReason)
						assert.Contains(t, message.ErrorMessage, "could not parse anthropic SSE event")
						assert.Empty(t, message.Content)
					} else {
						assert.Equal(t, StopReasonStop, message.StopReason)
						assert.Empty(t, message.ErrorMessage)
						assert.Equal(t, "msg_framing", message.ResponseID)
						assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
						assert.Equal(t, 11, message.Usage.Input)
						assert.Equal(t, 7, message.Usage.Output)
					}
				}
				assert.Equal(t, 2, requests)
				assert.Equal(t, 2, callbacks)
				assert.Equal(t, 2, closes)
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicSSENamedEventsReachTransport(t *testing.T) {
	for _, tc := range []struct {
		name, tail, wantError string
		wantFailure           bool
	}{
		{name: "unnamed control"},
		{name: "known named control", tail: "event: message_delta\ndata: {\"type\":\"ping\"}\n\n"},
		{name: "typed error control", tail: "data: {\"type\":\"error\",\"error\":{\"message\":\"busy\"}}\n\n", wantFailure: true, wantError: `{"type":"error","error":{"message":"busy"}}`},
		{name: "named JSON error", tail: "event: error\ndata: {\"error\":\"busy\"}\n\n", wantFailure: true, wantError: `{"error":"busy"}`},
		{name: "named plaintext error", tail: "event:error\ndata: busy\n\n", wantFailure: true, wantError: "busy"},
		{name: "named empty error", tail: "event: error\n\n", wantFailure: true},
		{name: "named EOF error", tail: "event: error", wantFailure: true},
		{name: "named multiline EOF error", tail: "event: error\ndata: busy\ndata:  retry", wantFailure: true, wantError: "busy\n retry"},
		{name: "last name wins", tail: "event: ignored\nevent: error\ndata: busy\n\n", wantFailure: true, wantError: "busy"},
		{name: "unknown malformed event", tail: "event: extension\ndata: not JSON\n\n"},
		{name: "unknown typed error", tail: "event: extension\ndata: {\"type\":\"error\"}\n\n"},
		{name: "name resets at blank line", tail: "event: extension\n\ndata: {\"type\":\"error\"}\n\n", wantFailure: true, wantError: `{"type":"error"}`},
		{name: "empty name restores unnamed", tail: "event: extension\nevent\ndata: {\"type\":\"error\"}\n\n", wantFailure: true, wantError: `{"type":"error"}`},
		{name: "only one optional space", tail: "event:  error\ndata: not JSON\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_named","usage":{"input_tokens":11}}}` + "\n\n" +
				"event: content_block_start\ndata: " + `{"type":"content_block_start","index":3,"content_block":{"type":"text","text":"answer"}}` + "\n\n" +
				"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":3}` + "\n\n" +
				"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n" +
				"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n" + tc.tail
			var requests, callbacks, closes int
			var callbackPayload string
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				payload, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, callbackPayload, string(payload))
				assert.Contains(t, string(payload), `"text":"question"`)
				reader := iotest.OneByteReader(iotest.DataErrReader(strings.NewReader(body)))
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &anthropicEOFBody{Reader: reader, closes: &closes}}, nil
			})}
			model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
			before, err := json.Marshal(prompt)
			require.NoError(t, err)
			opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
				callbacks++
				callbackPayload = payload.(string)
				return nil
			})}
			for _, api := range []string{"Stream", "Complete"} {
				var message *AssistantMessage
				if api == "Complete" {
					message, err = client.Complete(t.Context(), model, prompt, opts...)
				} else {
					var terminals int
					err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
						switch e := event.(type) {
						case TextStart:
							assert.Equal(t, 0, e.ContentIndex)
						case Done:
							terminals++
							assert.False(t, tc.wantFailure)
							message = &e.Message
						case StreamError:
							terminals++
							assert.True(t, tc.wantFailure)
							message = &e.Msg
						}
					}, opts...)
					assert.Equal(t, 1, terminals)
				}
				require.NoError(t, err)
				require.NotNil(t, message)
				wantReason := StopReasonStop
				if tc.wantFailure {
					wantReason = StopReasonError
				}
				assert.Equal(t, wantReason, message.StopReason)
				assert.Equal(t, tc.wantError, message.ErrorMessage)
				assert.Equal(t, "msg_named", message.ResponseID)
				assert.Equal(t, model.ID, message.Model)
				assert.Equal(t, model.API, message.API)
				assert.Equal(t, model.Provider, message.Provider)
				assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
				assert.Equal(t, 11, message.Usage.Input)
				assert.Equal(t, 7, message.Usage.Output)
			}
			assert.Equal(t, 2, requests)
			assert.Equal(t, 2, callbacks)
			assert.Equal(t, 2, closes)
			after, err := json.Marshal(prompt)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}

func TestAnthropicSSEJSONRepairReachesTransport(t *testing.T) {
	var controls strings.Builder
	for b := byte(0); b < 32; b++ {
		if b != '\r' && b != '\n' {
			controls.WriteByte(b)
		}
	}
	for _, tc := range []struct {
		name, target, token, want string
		wantFailure               bool
	}{
		{name: "valid text", token: `"answer 世界"`, want: "answer 世界"},
		{name: "valid escapes", token: `"\"\\\/\b\f\n\r\t\u0041"`, want: "\"\\/\b\f\n\r\tA"},
		{name: "raw tab", token: "\"col1\tcol2\"", want: "col1\tcol2"},
		{name: "raw controls", token: "\"" + controls.String() + "\"", want: controls.String()},
		{name: "raw newline across data fields", token: "\"line1\nline2\"", want: "line1\nline2"},
		{name: "invalid escape", token: `"A\H"`, want: `A\H`},
		{name: "invalid escape and control", token: "\"A\\H\tend\"", want: "A\\H\tend"},
		{name: "valid escapes during repair", token: `"\u0041\n\\\H"`, want: "A\n\\\\H"},
		{name: "backslash before raw control", token: "\"A\\\tend\"", want: "A\\\tend"},
		{name: "escaped backslash before quote", token: `"A\\\"B\H"`, want: "A\\\"B\\H"},
		{name: "thinking repair", target: "thinking", token: `"A\H"`, want: `A\H`},
		{name: "signature repair", target: "signature", token: "\"sig\ttail\"", want: "sig\ttail"},
		{name: "tool raw bigint control", target: "tool", token: `"valid\\path"`, want: `valid\path`},
		{name: "tool raw bigint repair", target: "tool", token: `"A\H"`, want: `A\H`},
		{name: "invalid unicode remains error", token: `"\uZZZZ"`, wantFailure: true},
		{name: "incomplete unicode remains error", token: `"\u12"`, wantFailure: true},
		{name: "missing quote remains error", token: `"unterminated`, wantFailure: true},
		{name: "trailing backslash remains error", token: `"unterminated\`, wantFailure: true},
		{name: "nonstring type remains error", token: `42`, wantFailure: true},
		{name: "structure remains error after repair", token: `"A\H",`, wantFailure: true},
		{name: "outside control remains error", token: "\x00\"answer\"", wantFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, field, delta := "text", "text", "text_delta"
			switch tc.target {
			case "thinking":
				kind, field, delta = "thinking", "thinking", "thinking_delta"
			case "signature":
				kind, field, delta = "thinking", "signature", "signature_delta"
			case "tool":
				kind = "tool_use"
			}
			start := `{"type":"content_block_start","index":3,"content_block":{"type":"` + kind + `","` + field + `":"prefix"}}`
			payload := `{"type":"content_block_delta","index":3,"delta":{"type":"` + delta + `","` + field + `":` + tc.token + `}}`
			rawInput := `{"n":9007199254740993, "s":` + tc.token + `}`
			if tc.target == "tool" {
				start = `{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"call_1","name":"read","input":` + rawInput + `}}`
				payload = `{"type":"ping"}`
			}
			var body strings.Builder
			for _, item := range []struct{ name, data string }{
				{"message_start", `{"type":"message_start","message":{"id":"msg_repair","usage":{"input_tokens":11}}}`},
				{"content_block_start", start},
				{"content_block_delta", payload},
				{"content_block_stop", `{"type":"content_block_stop","index":3}`},
				{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`},
				{"message_stop", `{"type":"message_stop"}`},
			} {
				body.WriteString("event: " + item.name + "\ndata: " + strings.ReplaceAll(item.data, "\n", "\ndata: ") + "\n\n")
			}
			var requests, callbacks, closes int
			var callbackPayload string
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, callbackPayload, string(wire))
				assert.Contains(t, string(wire), `"text":"question"`)
				reader := iotest.OneByteReader(iotest.DataErrReader(strings.NewReader(body.String())))
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &anthropicEOFBody{Reader: reader, closes: &closes}}, nil
			})}
			model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
			before, err := json.Marshal(prompt)
			require.NoError(t, err)
			opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
				callbacks++
				callbackPayload = payload.(string)
				return nil
			})}
			check := func(message AssistantMessage) {
				assert.Equal(t, "msg_repair", message.ResponseID)
				assert.Equal(t, model.ID, message.Model)
				assert.Equal(t, model.API, message.API)
				assert.Equal(t, model.Provider, message.Provider)
				assert.Equal(t, 11, message.Usage.Input)
				require.Len(t, message.Content, 1)
				block := message.Content[0]
				if tc.wantFailure {
					assert.Equal(t, StopReasonError, message.StopReason)
					assert.Contains(t, message.ErrorMessage, "could not parse anthropic SSE event")
					assert.Contains(t, message.ErrorMessage, payload)
					assert.Equal(t, "prefix", block.Text)
					assert.Zero(t, message.Usage.Output)
					return
				}
				assert.Equal(t, StopReasonStop, message.StopReason)
				assert.Empty(t, message.ErrorMessage)
				assert.Equal(t, 7, message.Usage.Output)
				switch tc.target {
				case "thinking":
					assert.Equal(t, "prefix"+tc.want, block.Thinking)
				case "signature":
					assert.Equal(t, "prefix"+tc.want, block.ThoughtSignature)
				case "tool":
					assert.Equal(t, "call_1", block.ID)
					assert.Equal(t, "read", block.Name)
					var input struct {
						N json.Number
						S string
					}
					require.NoError(t, json.Unmarshal(block.Arguments, &input))
					assert.Equal(t, "9007199254740993", input.N.String())
					assert.Equal(t, tc.want, input.S)
					if tc.name == "tool raw bigint control" {
						assert.Equal(t, rawInput, string(block.Arguments))
					}
				default:
					assert.Equal(t, "prefix"+tc.want, block.Text)
				}
			}
			var terminals, ends int
			err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
				switch e := event.(type) {
				case TextDelta:
					assert.Equal(t, tc.want, e.Delta)
					assert.Equal(t, 0, e.ContentIndex)
				case ThinkingDelta:
					assert.Equal(t, tc.want, e.Delta)
					assert.Equal(t, 0, e.ContentIndex)
				case TextEnd, ThinkingEnd, ToolCallEnd:
					ends++
				case Done:
					terminals++
					assert.False(t, tc.wantFailure)
					check(e.Message)
				case StreamError:
					terminals++
					assert.True(t, tc.wantFailure)
					check(e.Msg)
				}
			}, opts...)
			require.NoError(t, err)
			assert.Equal(t, 1, terminals)
			if tc.wantFailure {
				assert.Zero(t, ends)
			} else {
				assert.Equal(t, 1, ends)
			}
			message, err := client.Complete(t.Context(), model, prompt, opts...)
			require.NoError(t, err)
			require.NotNil(t, message)
			check(*message)
			assert.Equal(t, 2, requests)
			assert.Equal(t, 2, callbacks)
			assert.Equal(t, 2, closes)
			after, err := json.Marshal(prompt)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}

func TestAnthropicSSEFailureDiagnosticPrecedence(t *testing.T) {
	readErr := errors.New("body read failed")
	cancelErr := errors.New("caller cancelled after refusal")
	const stop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, outcome := range []string{"refusal", "sensitive", "end_turn"} {
		for _, tc := range []struct {
			name, tail, want    string
			readFailure, cancel bool
		}{
			{name: "completed", tail: stop},
			{name: "named error", tail: "event: error\ndata: busy\n\n", want: "busy"},
			{name: "empty named error", tail: "event: error\n\n"},
			{name: "typed error", tail: "data: {\"type\":\"error\"}\n\n", want: `{"type":"error"}`},
			{name: "parse error", tail: "data: not JSON\n\n", want: "could not parse anthropic SSE event:"},
			{name: "read error", readFailure: true, want: readErr.Error()},
			{name: "truncated", want: "anthropic stream ended before message_stop"},
			{name: "cancel at EOF", cancel: true, want: "request was aborted: " + cancelErr.Error()},
			{name: "cancel after stop", cancel: true, want: "request was aborted: " + cancelErr.Error()},
			{name: "cancel before stop", tail: stop, cancel: true, want: "request was aborted: " + cancelErr.Error()},
			{name: "cancel before named error", tail: "event: error\ndata: busy\n\n", cancel: true, want: "request was aborted: " + cancelErr.Error()},
			{name: "cancel before read error", readFailure: true, cancel: true, want: "request was aborted: " + cancelErr.Error()},
		} {
			t.Run(outcome+"/"+tc.name, func(t *testing.T) {
				prefix := "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_precedence","usage":{"input_tokens":11}}}` + "\n\n" +
					"event: content_block_start\ndata: " + `{"type":"content_block_start","index":3,"content_block":{"type":"text","text":"answer"}}` + "\n\n" +
					"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":3}` + "\n\n" +
					fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q,\"stop_details\":{\"explanation\":\"declined\"}},\"usage\":{\"output_tokens\":7}}\n\n", outcome)
				if tc.name == "cancel after stop" {
					prefix += stop
				}
				var requests, callbacks, responses, closes int
				var cancel context.CancelCauseFunc
				var callbackPayload string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					payload, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, callbackPayload, string(payload))
					assert.Contains(t, string(payload), `"text":"question"`)
					readers := []io.Reader{strings.NewReader(prefix)}
					if tc.cancel {
						readers = append(readers, &anthropicCancelBody{Reader: strings.NewReader(""), beforeRead: func() { cancel(cancelErr) }})
					}
					readers = append(readers, strings.NewReader(tc.tail))
					if tc.readFailure {
						readers = append(readers, iotest.ErrReader(readErr))
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &anthropicEOFBody{Reader: io.MultiReader(readers...), closes: &closes}}, nil
				})}
				model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
					callbacks++
					callbackPayload = payload.(string)
					return nil
				}), WithResponseCallback(func(_ ProviderResponse) { responses++ })}
				want, reason := tc.want, StopReasonError
				if tc.cancel {
					reason = StopReasonAborted
				} else if tc.name == "completed" {
					switch outcome {
					case "refusal":
						want = "declined"
					case "sensitive":
						want = "Provider stopped with: sensitive"
					default:
						reason = StopReasonStop
					}
				}
				for _, api := range []string{"Stream", "Complete"} {
					ctx, stop := context.WithCancelCause(t.Context())
					cancel = stop
					var message *AssistantMessage
					if api == "Complete" {
						message, err = client.Complete(ctx, model, prompt, opts...)
					} else {
						var terminals, ends int
						err = client.Stream(ctx, model, prompt, func(_ context.Context, event StreamEvent) {
							switch e := event.(type) {
							case TextStart:
								assert.Equal(t, 0, e.ContentIndex)
							case TextEnd:
								ends++
								assert.Equal(t, "answer", e.Content)
							case Done:
								terminals++
								assert.Equal(t, StopReasonStop, reason)
								message = &e.Message
							case StreamError:
								terminals++
								assert.NotEqual(t, StopReasonStop, reason)
								assert.Equal(t, string(reason), e.Reason)
								require.Error(t, e.Err)
								assert.Equal(t, e.Err.Error(), e.Msg.ErrorMessage)
								if tc.cancel {
									assert.ErrorIs(t, e.Err, cancelErr)
								} else if tc.readFailure {
									assert.ErrorIs(t, e.Err, readErr)
								}
								message = &e.Msg
							}
						}, opts...)
						assert.Equal(t, 1, terminals)
						assert.Equal(t, 1, ends)
					}
					require.NoError(t, err)
					require.NotNil(t, message)
					assert.Equal(t, reason, message.StopReason)
					if tc.name == "parse error" {
						assert.Contains(t, message.ErrorMessage, want)
						assert.Contains(t, message.ErrorMessage, "data=not JSON")
					} else {
						assert.Equal(t, want, message.ErrorMessage)
					}
					assert.Equal(t, "msg_precedence", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
					assert.Equal(t, 11, message.Usage.Input)
					assert.Equal(t, 7, message.Usage.Output)
					stop(nil)
				}
				assert.Equal(t, 2, requests)
				assert.Equal(t, 2, callbacks)
				assert.Equal(t, 2, responses)
				assert.Equal(t, 2, closes)
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicSSECRBeforeReadError(t *testing.T) {
	readErr := errors.New("body failed after event boundary")
	for _, ending := range []struct{ name, value string }{{"lf", "\n"}, {"crlf", "\r\n"}, {"cr", "\r"}} {
		for _, tail := range []string{"block stop", "usage delta", "named error"} {
			for _, readFailure := range []bool{false, true} {
				for _, reads := range []string{"buffered", "one byte", "data with error"} {
					t.Run(fmt.Sprintf("%s/%s/readFailure=%t/%s", ending.name, tail, readFailure, reads), func(t *testing.T) {
						body := "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_cr","usage":{"input_tokens":11}}}` + "\n\n" +
							"event: content_block_start\ndata: " + `{"type":"content_block_start","index":3,"content_block":{"type":"text","text":"answer"}}` + "\n\n" +
							"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n"
						if tail != "block stop" {
							body += "event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":3}` + "\n\n"
						}
						switch tail {
						case "block stop":
							body += "event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":3}` + "\n\n"
						case "usage delta":
							body += "event: message_delta\ndata: " + `{"type":"message_delta","usage":{"output_tokens":13}}` + "\n\n"
						case "named error":
							body += "event: error\ndata: busy\n\n"
						}
						body = strings.ReplaceAll(body, "\n", ending.value)
						var requests, callbacks, responses, closes int
						var callbackPayload string
						previous := http.DefaultClient
						t.Cleanup(func() { http.DefaultClient = previous })
						http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
							requests++
							payload, err := io.ReadAll(req.Body)
							require.NoError(t, err)
							assert.Equal(t, callbackPayload, string(payload))
							assert.Contains(t, string(payload), `"text":"question"`)
							var reader io.Reader = strings.NewReader(body)
							if readFailure {
								reader = io.MultiReader(reader, iotest.ErrReader(readErr))
							}
							switch reads {
							case "one byte":
								reader = iotest.OneByteReader(reader)
							case "data with error":
								reader = iotest.DataErrReader(reader)
							}
							return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &anthropicEOFBody{Reader: reader, closes: &closes}}, nil
						})}
						model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
						client, err := newAnthropicClient(model)
						require.NoError(t, err)
						prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
						before, err := json.Marshal(prompt)
						require.NoError(t, err)
						opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
							callbacks++
							callbackPayload = payload.(string)
							return nil
						}), WithResponseCallback(func(_ ProviderResponse) { responses++ })}
						want := "anthropic stream ended before message_stop"
						if readFailure {
							want = readErr.Error()
						}
						if tail == "named error" {
							want = "busy"
						}
						for _, api := range []string{"Stream", "Complete"} {
							var message *AssistantMessage
							if api == "Complete" {
								message, err = client.Complete(t.Context(), model, prompt, opts...)
							} else {
								var terminals, starts, ends int
								err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
									switch e := event.(type) {
									case TextStart:
										starts++
										assert.Equal(t, 0, e.ContentIndex)
									case TextEnd:
										ends++
										assert.Equal(t, 0, e.ContentIndex)
										assert.Equal(t, "answer", e.Content)
									case Done:
										terminals++
										t.Error("unexpected successful terminal")
									case StreamError:
										terminals++
										require.Error(t, e.Err)
										assert.Equal(t, want, e.Err.Error())
										if readFailure && tail != "named error" {
											assert.ErrorIs(t, e.Err, readErr)
										}
										message = &e.Msg
									}
								}, opts...)
								assert.Equal(t, 1, terminals)
								assert.Equal(t, 1, starts)
								assert.Equal(t, 1, ends)
							}
							require.NoError(t, err)
							require.NotNil(t, message)
							assert.Equal(t, StopReasonError, message.StopReason)
							assert.Equal(t, want, message.ErrorMessage)
							assert.Equal(t, "msg_cr", message.ResponseID)
							assert.Equal(t, model.ID, message.Model)
							assert.Equal(t, model.API, message.API)
							assert.Equal(t, model.Provider, message.Provider)
							assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
							assert.Equal(t, 11, message.Usage.Input)
							output := 7
							if tail == "usage delta" {
								output = 13
							}
							assert.Equal(t, output, message.Usage.Output)
						}
						assert.Equal(t, 2, requests)
						assert.Equal(t, 2, callbacks)
						assert.Equal(t, 2, responses)
						assert.Equal(t, 2, closes)
						after, err := json.Marshal(prompt)
						require.NoError(t, err)
						assert.Equal(t, string(before), string(after))
					})
				}
			}
		}
	}
}

func TestAnthropicStreamedToolStringRepair(t *testing.T) {
	for _, tc := range []struct {
		name           string
		parts          []string
		want           string
		invalid        bool
		outerMalformed bool
	}{
		{name: "valid bigint", parts: []string{`{"n":9007199254740993, "s":"answer"}`}, want: `{"n":9007199254740993, "s":"answer"}`},
		{name: "valid escapes", parts: []string{`{"s":"\u0041\n\\\""}`}, want: `{"s":"\u0041\n\\\""}`},
		{name: "raw tab", parts: []string{"{\"n\":9007199254740993, \"s\":\"col1\tcol2\"}"}, want: `{"n":9007199254740993, "s":"col1\u0009col2"}`},
		{name: "raw controls", parts: []string{"{\"s\":\"a\x00\n\r\b\f\x1fz\"}"}, want: `{"s":"a\u0000\u000a\u000d\u0008\u000c\u001fz"}`},
		{name: "invalid escape", parts: []string{`{"s":"A\H"}`}, want: `{"s":"A\\H"}`},
		{name: "invalid escape and tab", parts: []string{"{\"s\":\"A\\H\tend\"}"}, want: `{"s":"A\\H\u0009end"}`},
		{name: "backslash before control", parts: []string{"{\"s\":\"A\\\tend\"}"}, want: `{"s":"A\\\u0009end"}`},
		{name: "split invalid escape", parts: []string{`{"n":9007199254740993,"s":"A\`, `H"}`}, want: `{"n":9007199254740993,"s":"A\\H"}`},
		{name: "split valid escape", parts: []string{`{"s":"A\`, `nB"}`}, want: `{"s":"A\nB"}`},
		{name: "split unicode", parts: []string{`{"s":"\u00`, `41"}`}, want: `{"s":"\u0041"}`},
		{name: "split quote", parts: []string{`{"s":"A\`, `"B\H"}`}, want: `{"s":"A\"B\\H"}`},
		{name: "outer and nested repair", parts: []string{"{\"n\":9007199254740993,\"s\":\"A\\H\tend\"}"}, want: `{"n":9007199254740993,"s":"A\\H\u0009end"}`, outerMalformed: true},
		{name: "repair followed by more input", parts: []string{`{"s":"A\H"}`, " \n"}, want: "{\"s\":\"A\\\\H\"} \n"},
		{name: "empty delta", parts: []string{""}, want: "", invalid: true},
		{name: "malformed unicode unchanged", parts: []string{`{"s":"\uZZZZ"}`}, want: `{"s":"\uZZZZ"}`, invalid: true},
		{name: "incomplete unchanged", parts: []string{`{"s":"A\H`}, want: `{"s":"A\H`, invalid: true},
		{name: "structural error unchanged", parts: []string{`{"s":"A\H",}`}, want: `{"s":"A\H",}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body strings.Builder
			appendEvent := func(value string) { body.WriteString("data: " + value + "\n\n") }
			appendEvent(`{"type":"message_start","message":{"id":"msg_nested","usage":{"input_tokens":11}}}`)
			appendEvent(`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"call_1","name":"read","input":{}}}`)
			for _, part := range tc.parts {
				encoded, err := json.Marshal(part)
				require.NoError(t, err)
				if tc.outerMalformed {
					encoded = []byte(strings.ReplaceAll(strings.ReplaceAll(string(encoded), `\\H`, `\H`), `\t`, "\t"))
				}
				appendEvent(`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":` + string(encoded) + `}}`)
			}
			appendEvent(`{"type":"content_block_stop","index":3}`)
			appendEvent(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`)
			appendEvent(`{"type":"message_stop"}`)
			var requests, callbacks, responses, closes int
			var callbackPayload string
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, callbackPayload, string(wire))
				response := body.String()
				if requests == 3 {
					var payload struct {
						Messages []struct {
							Role    string
							Content []struct {
								Type, ID  string
								Input     json.RawMessage
								ToolUseID string `json:"tool_use_id"`
							}
						}
					}
					require.NoError(t, json.Unmarshal(wire, &payload))
					require.Len(t, payload.Messages, 3)
					require.Len(t, payload.Messages[1].Content, 1)
					assert.Equal(t, "assistant", payload.Messages[1].Role)
					assert.Equal(t, "call_1", payload.Messages[1].Content[0].ID)
					assert.JSONEq(t, tc.want, string(payload.Messages[1].Content[0].Input))
					if strings.Contains(tc.want, "9007199254740993") {
						assert.Contains(t, string(wire), "9007199254740993")
					}
					require.Len(t, payload.Messages[2].Content, 1)
					assert.Equal(t, "call_1", payload.Messages[2].Content[0].ToolUseID)
					response = "data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\ndata: " + `{"type":"message_stop"}` + "\n\n"
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(response)), closes: &closes}}, nil
			})}
			model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
			before, err := json.Marshal(prompt)
			require.NoError(t, err)
			opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any { callbacks++; callbackPayload = payload.(string); return nil }), WithResponseCallback(func(_ ProviderResponse) { responses++ })}
			checkArgs := func(args json.RawMessage) {
				t.Helper()
				assert.Equal(t, tc.want, string(args))
				assert.Equal(t, !tc.invalid, json.Valid(args))
			}
			var starts, deltas, ends, terminals int
			err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
				switch e := event.(type) {
				case ToolCallStart:
					starts++
					assert.Equal(t, 0, e.ContentIndex)
					require.Len(t, e.Partial.Content, 1)
					assert.Equal(t, "{}", string(e.Partial.Content[0].Arguments))
				case ToolCallDelta:
					require.Less(t, deltas, len(tc.parts))
					assert.Equal(t, tc.parts[deltas], e.Delta)
					deltas++
					assert.Equal(t, 0, e.ContentIndex)
					require.Len(t, e.Partial.Content, 1)
					if deltas == len(tc.parts) {
						checkArgs(e.Partial.Content[0].Arguments)
						if !tc.invalid {
							_, err := json.Marshal(e.Partial)
							assert.NoError(t, err)
						}
					}
				case ToolCallEnd:
					ends++
					assert.Equal(t, 0, e.ContentIndex)
					checkArgs(e.ToolCall.Arguments)
					require.Len(t, e.Partial.Content, 1)
					checkArgs(e.Partial.Content[0].Arguments)
				case Done:
					terminals++
					require.Len(t, e.Message.Content, 1)
					checkArgs(e.Message.Content[0].Arguments)
				case StreamError:
					t.Errorf("unexpected stream error: %v", e.Err)
				}
			}, opts...)
			require.NoError(t, err)
			assert.Equal(t, []int{1, len(tc.parts), 1, 1}, []int{starts, deltas, ends, terminals})
			message, err := client.Complete(t.Context(), model, prompt, opts...)
			require.NoError(t, err)
			require.NotNil(t, message)
			require.Len(t, message.Content, 1)
			checkArgs(message.Content[0].Arguments)
			assert.Equal(t, "msg_nested", message.ResponseID)
			assert.Equal(t, model.ID, message.Model)
			assert.Equal(t, model.API, message.API)
			assert.Equal(t, model.Provider, message.Provider)
			assert.Equal(t, StopReasonToolUse, message.StopReason)
			assert.Equal(t, 11, message.Usage.Input)
			assert.Equal(t, 7, message.Usage.Output)
			count := 2
			if !tc.invalid {
				encoded, err := json.Marshal(message)
				require.NoError(t, err)
				assert.Contains(t, string(encoded), `"arguments":`)
				replay := &Context{Messages: []Message{prompt.Messages[0], {Role: "assistant", API: model.API, Provider: model.Provider, Model: model.ID, Content: message.Content}, {Role: "toolResult", ToolCallID: "call_1", ToolName: "read", Content: "result"}}}
				source, err := json.Marshal(replay)
				require.NoError(t, err)
				_, err = client.Complete(t.Context(), model, replay, opts...)
				require.NoError(t, err)
				after, err := json.Marshal(replay)
				require.NoError(t, err)
				assert.Equal(t, string(source), string(after))
				count++
			}
			assert.Equal(t, []int{count, count, count, count}, []int{requests, callbacks, responses, closes})
			after, err := json.Marshal(prompt)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}

func TestAnthropicSSEInitialBOMReachesTransport(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	readErr := errors.New("body failed at BOM boundary")
	for _, tc := range []struct {
		name, prefix, first, text, diagnostic string
		noStart                               bool
		readFailure                           bool
	}{
		{name: "empty body", first: "short", diagnostic: "anthropic stream ended without a stop reason"},
		{name: "one byte BOM prefix", first: "short", prefix: "\xef", diagnostic: "anthropic stream ended without a stop reason"},
		{name: "two byte BOM prefix", first: "short", prefix: "\xef\xbb", diagnostic: "anthropic stream ended without a stop reason"},
		{name: "BOM only", first: "short", prefix: bom, diagnostic: "anthropic stream ended without a stop reason"},
		{name: "empty read failure", first: "short", diagnostic: readErr.Error(), readFailure: true},
		{name: "short read failure", first: "short", prefix: "\xef\xbb", diagnostic: readErr.Error(), readFailure: true},
		{name: "BOM read failure", first: "short", prefix: bom, diagnostic: readErr.Error(), readFailure: true},
		{name: "read failure after events", first: "data", prefix: bom, text: "answer", diagnostic: readErr.Error(), readFailure: true},
		{name: "no BOM", first: "data", text: "answer"},
		{name: "initial data", prefix: bom, first: "data", text: "answer"},
		{name: "initial event", prefix: bom, first: "event", text: "answer", noStart: true},
		{name: "initial error", prefix: bom, first: "error", diagnostic: "provider diagnostic"},
		{name: "initial comment", prefix: bom, first: "comment", text: "answer"},
		{name: "initial blank", prefix: bom, first: "blank", text: "answer"},
		{name: "second BOM retained", prefix: bom + bom, first: "data", text: "answer", noStart: true},
		{name: "BOM after blank retained", prefix: "\n" + bom, first: "data", text: "answer", noStart: true},
		{name: "BOM after comment retained", prefix: ": comment\n" + bom, first: "data", text: "answer", noStart: true},
		{name: "BOM in content retained", prefix: bom, first: "data", text: bom + "answer" + bom},
	} {
		for _, mode := range []string{"buffered", "one byte", "data with EOF"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				start := `data: {"type":"message_start","message":{"id":"msg_bom","usage":{"input_tokens":11}}}`
				var lines []string
				switch tc.first {
				case "event":
					lines = append(lines, "event: unknown", start)
				case "error":
					lines = append(lines, "event: error", "data: provider diagnostic")
				case "comment":
					lines = append(lines, ": comment", start)
				case "blank":
					lines = append(lines, "", start)
				default:
					lines = append(lines, start)
				}
				text, err := json.Marshal(tc.text)
				require.NoError(t, err)
				lines = append(lines, "", `data: {"type":"content_block_start","index":3,"content_block":{"type":"text","text":`+string(text)+`}}`, "",
					`data: {"type":"content_block_stop","index":3}`, "", `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`, "", `data: {"type":"message_stop"}`, "", "")
				response := tc.prefix + strings.Join(lines, "\n")
				if tc.first == "short" {
					response = tc.prefix
				}
				var requests, callbacks, responses, closes int
				var callbackPayload string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					wire, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, callbackPayload, string(wire))
					var reader io.Reader = strings.NewReader(response)
					if tc.readFailure {
						reader = io.MultiReader(reader, iotest.ErrReader(readErr))
					}
					switch mode {
					case "one byte":
						reader = iotest.OneByteReader(reader)
					case "data with EOF":
						reader = iotest.DataErrReader(reader)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &anthropicEOFBody{Reader: reader, closes: &closes}}, nil
				})}
				model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any {
					callbacks++
					callbackPayload = payload.(string)
					return nil
				}), WithResponseCallback(func(_ ProviderResponse) { responses++ })}
				check := func(message AssistantMessage) {
					t.Helper()
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					if tc.diagnostic != "" {
						assert.Equal(t, StopReasonError, message.StopReason)
						assert.Equal(t, tc.diagnostic, message.ErrorMessage)
					} else {
						assert.Equal(t, StopReasonStop, message.StopReason)
						assert.Empty(t, message.ErrorMessage)
					}
					if tc.noStart {
						assert.Empty(t, message.ResponseID)
						assert.Zero(t, message.Usage.Input)
					} else if tc.first == "error" || tc.first == "short" {
						assert.Empty(t, message.Content)
						assert.Empty(t, message.ResponseID)
						assert.Zero(t, message.Usage.Output)
						return
					} else {
						assert.Equal(t, "msg_bom", message.ResponseID)
						assert.Equal(t, 11, message.Usage.Input)
					}
					assert.Equal(t, 7, message.Usage.Output)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: tc.text}}, message.Content)
				}
				var terminals, ends int
				err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case TextEnd:
						ends++
						assert.Equal(t, 0, e.ContentIndex)
						assert.Equal(t, tc.text, e.Content)
					case Done:
						terminals++
						check(e.Message)
					case StreamError:
						terminals++
						check(e.Msg)
						assert.EqualError(t, e.Err, e.Msg.ErrorMessage)
						if tc.readFailure {
							assert.ErrorIs(t, e.Err, readErr)
						}
					}
				}, opts...)
				require.NoError(t, err)
				assert.Equal(t, 1, terminals)
				if tc.first == "error" || tc.first == "short" {
					assert.Zero(t, ends)
				} else {
					assert.Equal(t, 1, ends)
				}
				message, err := client.Complete(t.Context(), model, prompt, opts...)
				require.NoError(t, err)
				require.NotNil(t, message)
				check(*message)
				assert.Equal(t, []int{2, 2, 2, 2}, []int{requests, callbacks, responses, closes})
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicEagerToolInputReachesTransport(t *testing.T) {
	for _, compatibility := range []string{"nil", "unset", "on", "off"} {
		for _, layout := range []string{"none", "flat", "native", "removed", "fallback", "override", "suppress"} {
			for _, auth := range []string{"key", "copilot"} {
				for _, responseKind := range []string{"JSON", "SSE"} {
					for _, entry := range []string{"Stream", "Complete"} {
						t.Run(compatibility+"/"+layout+"/"+auth+"/"+responseKind+"/"+entry, func(t *testing.T) {
							model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
							if compatibility != "nil" || layout == "native" || layout == "removed" || layout == "fallback" {
								model.Compat = &CompatibilitySettings{}
							}
							if compatibility == "on" || compatibility == "off" {
								require.NoError(t, json.Unmarshal([]byte(`{"supportsEagerToolInputStreaming":`+fmt.Sprint(compatibility == "on")+`}`), model.Compat))
							}
							key := "test-key"
							if auth == "copilot" {
								model.Provider = ProviderGitHubCopilot
							}
							tool := Tool{Name: "read", Parameters: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`)}
							prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
							if layout != "none" {
								prompt.Tools = []Tool{tool}
							}
							if layout == "native" || layout == "removed" {
								model.Compat.SupportsMidConvoSystemMessages = true
								model.Compat.SupportsMidConvoToolChanges = true
								prompt.Tools = nil
								update := Message{Role: "system", ToolsRemoved: []ToolReference{{Name: "read"}}}
								if layout == "native" {
									update.ToolsAdded = []Tool{tool, {Name: "lookup", Parameters: tool.Parameters}}
								}
								prompt.Messages = []Message{{Role: "system", Content: "policy", ToolsAdded: []Tool{tool}}, {Role: "user", Content: "question"}, update}
							}
							wantRequests := 1
							if layout == "fallback" {
								model.Compat.SupportsStrictTools = true
								prompt.Tools[0].ConstrainedSampling = json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)
								wantRequests = 2
							}
							modelBefore, err := json.Marshal(model)
							require.NoError(t, err)
							before, err := json.Marshal(prompt)
							require.NoError(t, err)
							var requests, callbacks, responses, closes, terminals int
							var callbackPayload string
							checkDefinition := func(def map[string]any, attempt int) {
								t.Helper()
								if compatibility == "off" {
									assert.NotContains(t, def, "eager_input_streaming")
								} else {
									assert.Equal(t, true, def["eager_input_streaming"])
								}
								wantSchema := `{"type":"object","properties":{"n":{"type":"integer"}}}`
								if layout == "fallback" && attempt == 1 {
									wantSchema = `{"type":"object","properties":{"n":{"anyOf":[{"type":"integer"},{"type":"null"}]}},"required":["n"],"additionalProperties":false}`
								}
								schema, err := json.Marshal(def["input_schema"])
								require.NoError(t, err)
								assert.JSONEq(t, wantSchema, string(schema))
								if layout == "fallback" {
									assert.Equal(t, attempt == 1, def["strict"] == true)
								}
							}
							previous := http.DefaultClient
							t.Cleanup(func() { http.DefaultClient = previous })
							http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
								requests++
								assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
								wire, err := io.ReadAll(req.Body)
								require.NoError(t, err)
								assert.Equal(t, callbackPayload, string(wire))
								var body map[string]any
								require.NoError(t, json.Unmarshal(wire, &body))
								if layout == "none" {
									assert.NotContains(t, body, "tools")
								} else {
									defs := body["tools"].([]any)
									wantDefs := 1
									if layout == "native" || layout == "removed" {
										wantDefs = 2
									}
									require.Len(t, defs, wantDefs)
									def := defs[0].(map[string]any)
									checkDefinition(def, requests)
									assert.Equal(t, "read", def["name"])
									assert.Equal(t, map[string]any{"type": "ephemeral"}, def["cache_control"])
									if wantDefs == 2 {
										placeholder := defs[1].(map[string]any)
										assert.Equal(t, "__pi_deferred_placeholder__", placeholder["name"])
										assert.Equal(t, true, placeholder["defer_loading"])
										assert.NotContains(t, placeholder, "eager_input_streaming")
										assert.NotContains(t, placeholder, "cache_control")
										messages := body["messages"].([]any)
										update := messages[len(messages)-1].(map[string]any)
										blocks := update["content"].([]any)
										if layout == "native" {
											require.Len(t, blocks, 2)
											for _, raw := range blocks {
												block := raw.(map[string]any)
												assert.Equal(t, "tool_addition", block["type"])
												checkDefinition(block["tool"].(map[string]any)["definition"].(map[string]any), requests)
											}
										} else {
											require.Len(t, blocks, 1)
											assert.Equal(t, "tool_removal", blocks[0].(map[string]any)["type"])
										}
									}
								}
								var wantBetas []string
								if compatibility == "off" && layout != "none" && layout != "removed" {
									wantBetas = append(wantBetas, "fine-grained-tool-streaming-2025-05-14")
								}
								if layout == "native" || layout == "removed" {
									wantBetas = append(wantBetas, anthropicInlineToolsBeta)
								}
								if layout == "override" {
									wantBetas = []string{"caller-beta"}
								}
								if layout == "suppress" {
									wantBetas = nil
								}
								assert.Equal(t, strings.Join(wantBetas, ","), req.Header.Get("anthropic-beta"))
								contentType := "application/json"
								status := http.StatusOK
								response := `{"id":"msg_eager","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
								if responseKind == "SSE" {
									contentType = "text/event-stream"
									response = "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_eager","usage":{"input_tokens":3}}}` + "\n\nevent: content_block_start\ndata: " + `{"type":"content_block_start","index":4,"content_block":{"type":"text","text":"ok"}}` + "\n\nevent: content_block_stop\ndata: " + `{"type":"content_block_stop","index":4}` + "\n\nevent: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\nevent: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
								}
								if layout == "fallback" && requests == 1 {
									status = http.StatusBadRequest
									contentType = "application/json"
									response = `{"error":{"type":"invalid_request_error","message":"Too many strict tools (21)"}}`
								}
								return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: &anthropicEOFBody{Reader: strings.NewReader(response), closes: &closes}}, nil
							})}
							client, err := newAnthropicClient(model)
							require.NoError(t, err)
							opts := []Option{WithAPIKey(key), WithCacheRetention("short"), WithPayloadCallback(func(payload any) any { callbacks++; callbackPayload = payload.(string); return nil }), WithResponseCallback(func(_ ProviderResponse) { responses++ })}
							if layout == "override" {
								opts = append(opts, WithHeaders(map[string]string{"anthropic-beta": "caller-beta"}))
							}
							if layout == "suppress" {
								opts = append(opts, WithHeaders(map[string]string{"anthropic-beta": ""}))
							}
							checkMessage := func(message AssistantMessage) {
								t.Helper()
								assert.Equal(t, StopReasonStop, message.StopReason)
								assert.Equal(t, "msg_eager", message.ResponseID)
								assert.Equal(t, model.ID, message.Model)
								assert.Equal(t, model.Provider, message.Provider)
								assert.Equal(t, model.API, message.API)
								assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "ok"}}, message.Content)
								assert.Equal(t, 5, message.Usage.TotalTokens)
								if layout == "fallback" {
									require.Len(t, message.Diagnostics, 1)
									assert.Equal(t, "anthropic_strict_tools_fallback", message.Diagnostics[0].Type)
								} else {
									assert.Empty(t, message.Diagnostics)
								}
							}
							if entry == "Stream" {
								err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
									switch e := event.(type) {
									case Done:
										terminals++
										checkMessage(e.Message)
									case StreamError:
										t.Errorf("unexpected stream error: %v", e.Err)
									}
								}, opts...)
							} else {
								var message *AssistantMessage
								message, err = client.Complete(t.Context(), model, prompt, opts...)
								require.NoError(t, err)
								require.NotNil(t, message)
								terminals++
								checkMessage(*message)
							}
							require.NoError(t, err)
							assert.Equal(t, []int{wantRequests, wantRequests, 1, wantRequests, 1}, []int{requests, callbacks, responses, closes, terminals})
							after, err := json.Marshal(prompt)
							require.NoError(t, err)
							assert.Equal(t, string(before), string(after))
							modelAfter, err := json.Marshal(model)
							require.NoError(t, err)
							assert.Equal(t, string(modelBefore), string(modelAfter))
						})
					}
				}
			}
		}
	}
}

func TestAnthropicHeaderOwnedAuthReachesTransport(t *testing.T) {
	for _, name := range envVarNames(ProviderAnthropic) {
		t.Setenv(name, "")
	}
	for _, tc := range []struct {
		name                  string
		modelHeaders, headers map[string]string
		key                   string
		valid                 bool
	}{
		{name: "model bearer", modelHeaders: map[string]string{"aUtHoRiZaTiOn": "Bearer gateway-token"}, valid: true},
		{name: "caller bearer", headers: map[string]string{"AUTHORIZATION": "Bearer gateway-token"}, valid: true},
		{name: "model api key", modelHeaders: map[string]string{"X-API-KEY": "gateway-key"}, valid: true},
		{name: "caller api key", headers: map[string]string{"x-api-key": "gateway-key"}, valid: true},
		{name: "model cloudflare", modelHeaders: map[string]string{"Cf-Aig-Authorization": "Bearer gateway-token"}, valid: true},
		{name: "caller cloudflare", headers: map[string]string{"cf-aig-authorization": "Bearer gateway-token"}, valid: true},
		{name: "caller replaces model", modelHeaders: map[string]string{"Authorization": "Bearer model-token"}, headers: map[string]string{"authorization": "Bearer caller-token"}, valid: true},
		{name: "caller suppresses model", modelHeaders: map[string]string{"Authorization": "Bearer model-token"}, headers: map[string]string{"authorization": ""}},
		{name: "caller blanks model", modelHeaders: map[string]string{"X-Api-Key": "model-key"}, headers: map[string]string{"x-api-key": " \t "}},
		{name: "blank bearer", headers: map[string]string{"Authorization": " \t "}},
		{name: "empty cloudflare", headers: map[string]string{"cf-aig-authorization": ""}},
		{name: "unrelated header", headers: map[string]string{"X-Other": "not-auth"}},
		{name: "blank cloudflare", headers: map[string]string{"Cf-Aig-Authorization": " \t "}},
		{name: "empty api key", headers: map[string]string{"X-Api-Key": ""}},
		{name: "suppressed bearer with gateway", modelHeaders: map[string]string{"Authorization": "Bearer model-token", "cf-aig-authorization": "Bearer gateway-token"}, headers: map[string]string{"authorization": ""}, valid: true},
		{name: "OAuth shaped header stays header owned", headers: map[string]string{"Authorization": "Bearer sk-ant-oat-gateway"}, valid: true},
		{name: "absent"},
		{name: "api key control", key: "test-key", valid: true},
	} {
		for _, fallback := range []bool{false, true} {
			for _, responseKind := range []string{"JSON", "SSE"} {
				for _, entry := range []string{"Stream", "Complete"} {
					t.Run(fmt.Sprintf("%s/fallback=%t/%s/%s", tc.name, fallback, responseKind, entry), func(t *testing.T) {
						model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", Headers: tc.modelHeaders, MaxTokens: 4096}
						prompt := &Context{SystemPrompt: "policy", Messages: []Message{{Role: "user", Content: "question"}}, Tools: []Tool{{Name: "read", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}}
						wantRequests := 1
						if fallback {
							model.Compat = &CompatibilitySettings{SupportsStrictTools: true}
							prompt.Tools[0].ConstrainedSampling = json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)
							wantRequests = 2
						}
						if !tc.valid {
							wantRequests = 0
						}
						before, err := json.Marshal([]any{model, prompt, tc.headers})
						require.NoError(t, err)
						var requests, callbacks, responses, closes, terminals int
						var callbackPayload string
						previous := http.DefaultClient
						t.Cleanup(func() { http.DefaultClient = previous })
						http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
							requests++
							assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
							assert.Equal(t, "2023-06-01", req.Header.Get("anthropic-version"))
							expected := make(http.Header)
							if tc.key != "" {
								expected.Set("x-api-key", tc.key)
							}
							for k, v := range tc.modelHeaders {
								expected.Set(k, v)
							}
							for k, v := range tc.headers {
								expected.Set(k, v)
							}
							for _, name := range []string{"Authorization", "x-api-key", "cf-aig-authorization"} {
								assert.Equal(t, expected.Get(name), req.Header.Get(name), name)
							}
							wire, err := io.ReadAll(req.Body)
							require.NoError(t, err)
							assert.Equal(t, callbackPayload, string(wire))
							var body map[string]any
							require.NoError(t, json.Unmarshal(wire, &body))
							assert.Equal(t, true, body["stream"])
							assert.Equal(t, model.ID, body["model"])
							assert.NotContains(t, req.Header.Get("anthropic-beta"), "oauth-2025-04-20")
							assert.Empty(t, req.Header.Get("x-app"))
							assert.Equal(t, []any{map[string]any{"type": "text", "text": "policy"}}, body["system"])
							assert.Equal(t, "read", body["tools"].([]any)[0].(map[string]any)["name"])
							if fallback {
								assert.Equal(t, requests == 1, body["tools"].([]any)[0].(map[string]any)["strict"] == true)
							}
							status, contentType := http.StatusOK, "application/json"
							response := `{"id":"msg_auth","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
							if responseKind == "SSE" {
								contentType = "text/event-stream"
								response = "data: " + `{"type":"message_start","message":{"id":"msg_auth","usage":{"input_tokens":3}}}` + "\n\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}` + "\n\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\ndata: " + `{"type":"message_stop"}` + "\n\n"
							}
							if fallback && requests == 1 {
								status, contentType = http.StatusBadRequest, "application/json"
								response = `{"error":{"type":"invalid_request_error","message":"Too many strict tools (21)"}}`
							}
							return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: &anthropicEOFBody{Reader: strings.NewReader(response), closes: &closes}}, nil
						})}
						client, err := newAnthropicClient(model)
						require.NoError(t, err)
						opts := []Option{WithAPIKey(tc.key), WithHeaders(tc.headers), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any { callbacks++; callbackPayload = payload.(string); return nil }), WithResponseCallback(func(_ ProviderResponse) { responses++ })}
						checkMessage := func(message AssistantMessage) {
							t.Helper()
							assert.Equal(t, StopReasonStop, message.StopReason)
							assert.Equal(t, "msg_auth", message.ResponseID)
							assert.Equal(t, model.ID, message.Model)
							assert.Equal(t, model.API, message.API)
							assert.Equal(t, model.Provider, message.Provider)
							assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "ok"}}, message.Content)
							assert.Equal(t, 5, message.Usage.TotalTokens)
							if fallback {
								require.Len(t, message.Diagnostics, 1)
								assert.Equal(t, "anthropic_strict_tools_fallback", message.Diagnostics[0].Type)
							} else {
								assert.Empty(t, message.Diagnostics)
							}
						}
						if entry == "Stream" {
							err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
								switch e := event.(type) {
								case Done:
									terminals++
									checkMessage(e.Message)
								case StreamError:
									t.Errorf("unexpected stream error: %v", e.Err)
								}
							}, opts...)
						} else {
							var message *AssistantMessage
							message, err = client.Complete(t.Context(), model, prompt, opts...)
							if tc.valid && message != nil {
								terminals++
								checkMessage(*message)
							} else {
								assert.Nil(t, message)
							}
						}
						if tc.valid {
							assert.NoError(t, err)
							assert.Equal(t, []int{wantRequests, wantRequests, 1, wantRequests, 1}, []int{requests, callbacks, responses, closes, terminals})
						} else {
							require.ErrorContains(t, err, "missing API key")
							assert.Equal(t, []int{0, 1, 0, 0, 0}, []int{requests, callbacks, responses, closes, terminals})
						}
						after, err := json.Marshal([]any{model, prompt, tc.headers})
						require.NoError(t, err)
						assert.Equal(t, string(before), string(after))
					})
				}
			}
		}
	}
}

func TestAnthropicRejectionKeepsStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		if _, err := io.WriteString(w, `{"error":{"message":"denied"}}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: server.URL}
	client, err := newAnthropicClient(model)
	require.NoError(t, err)
	err = client.Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "x"}}}, func(context.Context, StreamEvent) {}, WithAPIKey("key"), WithMaxRetries(0))
	var rejected *providerStatusError
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, http.StatusForbidden, rejected.status)
}

func TestAnthropicThinkingOffReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reasoning bool
		compat    *CompatibilitySettings
		levels    map[string]*string
	}{
		{name: "budget", reasoning: true},
		{name: "adaptive", reasoning: true, compat: &CompatibilitySettings{ForceAdaptiveThinking: true}},
		{name: "off unsupported", reasoning: true, levels: map[string]*string{"off": nil}},
		{name: "off mapped", reasoning: true, levels: map[string]*string{"off": new("none")}},
		{name: "non reasoning"},
		{name: "managed", reasoning: true, compat: &CompatibilitySettings{SupportsMidConvoEffort: true}},
	} {
		for _, mode := range []string{"unset", "off", "disabled", "enabled", "simple unset", "simple off", "simple high", "disabled then enabled", "enabled then disabled"} {
			for _, entry := range []string{"Stream", "Complete"} {
				t.Run(tc.name+"/"+mode+"/"+entry, func(t *testing.T) {
					model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", Reasoning: tc.reasoning, MaxTokens: 32000, Compat: tc.compat, ThinkingLevelMap: tc.levels}
					prompt := &Context{SystemPrompt: "policy", Messages: []Message{{Role: "user", Content: "question"}}}
					before, err := json.Marshal([]any{model, prompt})
					require.NoError(t, err)
					var requests, payloads, responses, closes, terminals int
					var callbackPayload string
					opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxTokens(4000), WithTemperature(0.4), WithPayloadCallback(func(payload any) any { payloads++; callbackPayload = payload.(string); return nil }), WithResponseCallback(func(_ ProviderResponse) { responses++ })}
					switch mode {
					case "off", "simple off":
						opts = append(opts, WithReasoning(ReasoningOff))
					case "disabled":
						opts = append(opts, WithThinkingEnabled(false))
					case "enabled":
						opts = append(opts, WithThinkingEnabled(true))
					case "simple high":
						opts = append(opts, WithReasoning(ReasoningHigh))
					case "disabled then enabled":
						opts = append(opts, WithThinkingEnabled(false), WithThinkingEnabled(true))
					case "enabled then disabled":
						opts = append(opts, WithThinkingEnabled(true), WithThinkingEnabled(false))
					}
					var wantThinking string
					managed := tc.compat != nil && tc.compat.SupportsMidConvoEffort
					active := mode == "enabled" || mode == "simple high" || mode == "disabled then enabled"
					wantMax := 4000
					var wantBudget int
					if managed {
						wantThinking = `{"type":"adaptive","display":"summarized","block_binding":{"prefix_mismatch_behavior":"drop_block"}}`
					} else if active {
						if tc.compat != nil && tc.compat.ForceAdaptiveThinking {
							wantThinking = `{"type":"adaptive","display":"summarized"}`
						} else {
							wantBudget = 1024
							if mode == "simple high" {
								wantBudget = 16384
							}
							wantMax += wantBudget
							wantThinking = fmt.Sprintf(`{"type":"enabled","display":"summarized","budget_tokens":%d}`, wantBudget)
						}
					} else if tc.reasoning && mode != "unset" {
						if value, exists := tc.levels["off"]; !exists || value != nil {
							wantThinking = `{"type":"disabled"}`
						}
					}
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
						assert.Equal(t, "test-key", req.Header.Get("x-api-key"))
						assert.Equal(t, "2023-06-01", req.Header.Get("anthropic-version"))
						wire, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						assert.Equal(t, callbackPayload, string(wire))
						var body map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(wire, &body))
						if wantThinking == "" {
							assert.NotContains(t, body, "thinking")
						} else {
							assert.JSONEq(t, wantThinking, string(body["thinking"]))
						}
						assert.JSONEq(t, fmt.Sprintf("%q", model.ID), string(body["model"]))
						assert.Equal(t, "true", string(body["stream"]))
						assert.Equal(t, fmt.Sprint(wantMax), string(body["max_tokens"]))
						if active || managed {
							assert.NotContains(t, body, "temperature")
						} else {
							assert.Equal(t, "0.4", string(body["temperature"]))
						}
						if managed || active && tc.compat != nil && tc.compat.ForceAdaptiveThinking && mode == "simple high" {
							assert.JSONEq(t, `{"effort":"high"}`, string(body["output_config"]))
						} else {
							assert.NotContains(t, body, "output_config")
						}
						if !managed {
							assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(body["messages"]))
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: &anthropicEOFBody{Reader: strings.NewReader(`{"id":"msg_off","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`), closes: &closes}}, nil
					})}
					var message *AssistantMessage
					handler := func(_ context.Context, event StreamEvent) {
						switch e := event.(type) {
						case Done:
							terminals++
							message = &e.Message
						case StreamError:
							t.Errorf("unexpected stream error: %v", e.Err)
						}
					}
					if strings.HasPrefix(mode, "simple ") {
						simple := ExtractSimpleStreamOptions(opts)
						if entry == "Stream" {
							err = StreamSimpleWithModel(t.Context(), model, prompt, handler, &simple)
						} else {
							message, err = CompleteSimpleWithModel(t.Context(), model, prompt, &simple)
						}
					} else {
						client, clientErr := newAnthropicClient(model)
						require.NoError(t, clientErr)
						if entry == "Stream" {
							err = client.Stream(t.Context(), model, prompt, handler, opts...)
						} else {
							message, err = client.Complete(t.Context(), model, prompt, opts...)
						}
					}
					require.NoError(t, err)
					require.NotNil(t, message)
					if entry == "Complete" {
						terminals++
					}
					assert.Equal(t, []int{1, 1, 1, 1, 1}, []int{requests, payloads, responses, closes, terminals})
					assert.Equal(t, "msg_off", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, StopReasonStop, message.StopReason)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
					assert.Equal(t, 5, message.Usage.TotalTokens)
					assert.Empty(t, message.Diagnostics)
					after, err := json.Marshal([]any{model, prompt})
					require.NoError(t, err)
					assert.Equal(t, string(before), string(after))
				})
			}
		}
	}
}

func TestAnthropicConfiguredHeadersStayRequestLocal(t *testing.T) {
	for _, auth := range []string{"api key", "copilot"} {
		t.Run(auth, func(t *testing.T) {
			model := &Model{ID: "claude-headers", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Reasoning: true, Headers: map[string]string{
				"ANTHROPIC-BETA": "model-beta,model-beta, , other-model-beta",
				"USER-AGENT":     "model-client", "X-APP": "model-app",
			}}
			key := "test-key"
			if auth == "copilot" {
				model.Provider = ProviderGitHubCopilot
			}
			prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
			replacement := map[string]string{"anthropic-beta": " caller-beta,caller-beta, , other-caller-beta ", "user-agent": "caller-client", "x-app": "caller-app"}
			suppression := map[string]string{"Anthropic-Beta": "", "User-Agent": "", "X-App": ""}
			before, err := json.Marshal([]any{model, prompt, replacement, suppression})
			require.NoError(t, err)
			var requests, payloads, responses, closes, terminals int
			var payload string
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				assert.Equal(t, requests-1, closes)
				assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
				assert.Equal(t, "2023-06-01", req.Header.Get("anthropic-version"))
				want := replacement
				if requests == 2 {
					want = suppression
				} else if requests == 3 {
					want = model.Headers
				}
				for name, value := range want {
					assert.Equal(t, []string{value}, req.Header.Values(name), name)
				}
				if auth == "api key" {
					assert.Equal(t, key, req.Header.Get("x-api-key"))
					assert.Empty(t, req.Header.Get("authorization"))
				} else {
					assert.Equal(t, "Bearer "+key, req.Header.Get("authorization"))
					assert.Empty(t, req.Header.Get("x-api-key"))
				}
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, payload, string(wire))
				var body map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(wire, &body))
				assert.Equal(t, "true", string(body["stream"]))
				assert.NotContains(t, body, "betas")
				assert.JSONEq(t, `{"type":"enabled","budget_tokens":1024,"display":"summarized"}`, string(body["thinking"]))
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(`{"id":"msg_headers","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)), closes: &closes}}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithClient(client), WithAPIKey(key), WithCacheRetention("none"), WithThinkingBudgetTokens(1024), WithPayloadCallbackForModel(func(value any, got *Model) any {
				payloads++
				assert.Same(t, model, got)
				payload = value.(string)
				return nil
			}), WithResponseCallback(func(response ProviderResponse) {
				responses++
				assert.Equal(t, http.StatusOK, response.Status)
				assert.Equal(t, requests-1, closes)
			})}
			check := func(message AssistantMessage) {
				t.Helper()
				assert.Equal(t, StopReasonStop, message.StopReason)
				assert.Equal(t, "msg_headers", message.ResponseID)
				assert.Equal(t, model.ID, message.Model)
				assert.Equal(t, model.API, message.API)
				assert.Equal(t, model.Provider, message.Provider)
				assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
				assert.Equal(t, 5, message.Usage.TotalTokens)
				assert.Empty(t, message.Diagnostics)
			}
			err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
				done, ok := event.(Done)
				require.True(t, ok, "unexpected event %T", event)
				terminals++
				assert.Equal(t, 1, responses)
				check(done.Message)
			}, append(opts, WithHeaders(replacement))...)
			require.NoError(t, err)
			for _, headers := range []map[string]string{suppression, nil} {
				message, err := CompleteWithModel(t.Context(), model, prompt, append(opts, WithHeaders(headers))...)
				require.NoError(t, err)
				require.NotNil(t, message)
				check(*message)
			}
			assert.Equal(t, []int{3, 3, 3, 3, 1}, []int{requests, payloads, responses, closes, terminals})
			assert.Equal(t, http.Header{"Content-Type": {"application/json"}, "anthropic-version": {"2023-06-01"}}, client.transport.headers)
			after, err := json.Marshal([]any{model, prompt, replacement, suppression})
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}

func TestAnthropicInterleavedBetaReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name      string
		id        string
		reasoning bool
		compat    *CompatibilitySettings
		opts      []Option
		want      bool
		header    *string
	}{
		{name: "enabled default", reasoning: true, opts: []Option{WithThinkingEnabled(true)}, want: true},
		{name: "enabled explicit", reasoning: true, opts: []Option{WithThinkingEnabled(true), WithInterleavedThinking(true)}, want: true},
		{name: "enabled suppressed", reasoning: true, opts: []Option{WithThinkingEnabled(true), WithInterleavedThinking(false)}},
		{name: "unset", reasoning: true},
		{name: "unset explicit beta", reasoning: true, opts: []Option{WithInterleavedThinking(true)}},
		{name: "disabled explicit beta", reasoning: true, opts: []Option{WithThinkingEnabled(false), WithInterleavedThinking(true)}},
		{name: "off explicit beta", reasoning: true, opts: []Option{WithReasoning(ReasoningOff), WithInterleavedThinking(true)}},
		{name: "nonreasoning explicit beta", opts: []Option{WithThinkingEnabled(true), WithInterleavedThinking(true)}},
		{name: "adaptive id without flag", id: "claude-sonnet-4-6", reasoning: true, opts: []Option{WithThinkingEnabled(true), WithInterleavedThinking(true)}, want: true},
		{name: "adaptive flag", reasoning: true, compat: &CompatibilitySettings{ForceAdaptiveThinking: true}, opts: []Option{WithThinkingEnabled(true), WithInterleavedThinking(true)}},
		{name: "managed", reasoning: true, compat: &CompatibilitySettings{SupportsMidConvoEffort: true}, opts: []Option{WithThinkingEnabled(true), WithInterleavedThinking(true)}},
		{name: "generic reasoning", reasoning: true, opts: []Option{WithReasoning(ReasoningHigh)}, want: true},
		{name: "budget", reasoning: true, opts: []Option{WithThinkingBudgetTokens(1024)}, want: true},
		{name: "effort", reasoning: true, opts: []Option{WithEffort("high")}, want: true},
		{name: "caller replacement", reasoning: true, opts: []Option{WithThinkingEnabled(true)}, header: new("caller-beta")},
		{name: "caller suppression", reasoning: true, opts: []Option{WithThinkingEnabled(true)}, header: new("")},
	} {
		for _, auth := range []string{"api key", "copilot"} {
			for _, responseKind := range []string{"JSON", "SSE"} {
				for _, entry := range []string{"Stream", "Complete"} {
					t.Run(tc.name+"/"+auth+"/"+responseKind+"/"+entry, func(t *testing.T) {
						model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 32000, Reasoning: tc.reasoning, Compat: tc.compat}
						if tc.id != "" {
							model.ID = tc.id
						}
						key := "test-key"
						if auth == "copilot" {
							model.Provider = ProviderGitHubCopilot
						}
						prompt := &Context{SystemPrompt: "policy", Messages: []Message{{Role: "user", Content: "question"}}}
						before, err := json.Marshal([]any{model, prompt})
						require.NoError(t, err)
						var requests, payloads, responses, closes, terminals int
						var callbackPayload string
						previous := http.DefaultClient
						t.Cleanup(func() { http.DefaultClient = previous })
						http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
							requests++
							assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
							var betas []string
							if tc.want {
								betas = append(betas, "interleaved-thinking-2025-05-14")
							}
							if tc.compat != nil && tc.compat.SupportsMidConvoEffort {
								betas = append(betas, anthropicMidConversationOutputConfigBeta, anthropicThinkingBindingBeta)
							}
							wantBeta := strings.Join(betas, ",")
							if tc.header != nil {
								wantBeta = *tc.header
							}
							assert.Equal(t, wantBeta, req.Header.Get("anthropic-beta"))
							if auth == "api key" {
								assert.Equal(t, key, req.Header.Get("x-api-key"))
							} else {
								assert.Equal(t, "Bearer "+key, req.Header.Get("authorization"))
							}
							wire, err := io.ReadAll(req.Body)
							require.NoError(t, err)
							assert.Equal(t, callbackPayload, string(wire))
							var body map[string]json.RawMessage
							require.NoError(t, json.Unmarshal(wire, &body))
							assert.Equal(t, "true", string(body["stream"]))
							assert.JSONEq(t, fmt.Sprintf("%q", model.ID), string(body["model"]))
							contentType := "application/json"
							response := `{"id":"msg_beta","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
							if responseKind == "SSE" {
								contentType = "text/event-stream"
								response = "data: " + `{"type":"message_start","message":{"id":"msg_beta","usage":{"input_tokens":3}}}` + "\n\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}` + "\n\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\ndata: " + `{"type":"message_stop"}` + "\n\n"
							}
							return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: &anthropicEOFBody{Reader: strings.NewReader(response), closes: &closes}}, nil
						})}
						opts := append([]Option{WithAPIKey(key), WithCacheRetention("none"), WithMaxTokens(4000), WithPayloadCallback(func(payload any) any { payloads++; callbackPayload = payload.(string); return nil }), WithResponseCallback(func(_ ProviderResponse) { responses++ })}, tc.opts...)
						if tc.header != nil {
							opts = append(opts, WithHeaders(map[string]string{"Anthropic-Beta": *tc.header}))
						}
						client, err := newAnthropicClient(model)
						require.NoError(t, err)
						var message *AssistantMessage
						if entry == "Stream" {
							err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
								switch e := event.(type) {
								case Done:
									terminals++
									message = &e.Message
								case StreamError:
									t.Errorf("unexpected stream error: %v", e.Err)
								}
							}, opts...)
						} else {
							message, err = client.Complete(t.Context(), model, prompt, opts...)
							terminals++
						}
						require.NoError(t, err)
						require.NotNil(t, message)
						assert.Equal(t, []int{1, 1, 1, 1, 1}, []int{requests, payloads, responses, closes, terminals})
						assert.Equal(t, "msg_beta", message.ResponseID)
						assert.Equal(t, model.ID, message.Model)
						assert.Equal(t, model.Provider, message.Provider)
						assert.Equal(t, model.API, message.API)
						assert.Equal(t, StopReasonStop, message.StopReason)
						assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "ok"}}, message.Content)
						assert.Equal(t, 5, message.Usage.TotalTokens)
						assert.Empty(t, message.Diagnostics)
						after, err := json.Marshal([]any{model, prompt})
						require.NoError(t, err)
						assert.Equal(t, string(before), string(after))
					})
				}
			}
		}
	}
}

func TestAnthropicMetadataReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		want     string
		retry    bool
	}{
		{name: "absent"},
		{name: "empty", metadata: map[string]any{}},
		{name: "string", metadata: map[string]any{"user_id": "user-123"}, want: `{"user_id":"user-123"}`},
		{name: "empty string", metadata: map[string]any{"user_id": ""}, want: `{"user_id":""}`},
		{name: "unicode string", metadata: map[string]any{"user_id": "  用户\n"}, want: `{"user_id":"  用户\n"}`},
		{name: "null", metadata: map[string]any{"user_id": nil}},
		{name: "number", metadata: map[string]any{"user_id": 42}},
		{name: "boolean", metadata: map[string]any{"user_id": true}},
		{name: "object", metadata: map[string]any{"user_id": map[string]any{"private": "not-for-provider"}}},
		{name: "array", metadata: map[string]any{"user_id": []string{"not-for-provider"}}},
		{name: "unsupported only", metadata: map[string]any{"trace": "not-for-provider"}},
		{name: "string with extras on retry", metadata: map[string]any{"user_id": "user-123", "trace": "not-for-provider"}, want: `{"user_id":"user-123"}`, retry: true},
		{name: "unencodable extras", metadata: map[string]any{"user_id": "user-123", "callback": func() {}}, want: `{"user_id":"user-123"}`},
	} {
		for _, responseKind := range []string{"JSON", "SSE"} {
			for _, entry := range []string{"Stream", "Complete"} {
				t.Run(tc.name+"/"+responseKind+"/"+entry, func(t *testing.T) {
					model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
					prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
					wantRequests := 1
					if tc.retry {
						wantRequests = 2
						model.Compat = &CompatibilitySettings{SupportsStrictTools: true}
						prompt.Tools = []Tool{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)}}
					}
					var requests, payloads, responses, closes, terminals int
					var callbackPayload string
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
						assert.Equal(t, "test-key", req.Header.Get("x-api-key"))
						wire, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						assert.Equal(t, callbackPayload, string(wire))
						var body map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(wire, &body))
						if tc.want == "" {
							assert.NotContains(t, body, "metadata")
						} else {
							assert.JSONEq(t, tc.want, string(body["metadata"]))
						}
						assert.Equal(t, "true", string(body["stream"]))
						assert.JSONEq(t, `"claude-test"`, string(body["model"]))
						status, contentType := http.StatusOK, "application/json"
						response := `{"id":"msg_metadata","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
						if responseKind == "SSE" {
							contentType = "text/event-stream"
							response = "data: " + `{"type":"message_start","message":{"id":"msg_metadata","usage":{"input_tokens":3}}}` + "\n\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}` + "\n\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\ndata: " + `{"type":"message_stop"}` + "\n\n"
						}
						if tc.retry && requests == 1 {
							status, contentType = http.StatusBadRequest, "application/json"
							response = `{"error":{"type":"invalid_request_error","message":"Too many strict tools (21)"}}`
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: &anthropicEOFBody{Reader: strings.NewReader(response), closes: &closes}}, nil
					})}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithMetadata(tc.metadata), WithPayloadCallback(func(payload any) any { payloads++; callbackPayload = payload.(string); return nil }), WithResponseCallback(func(_ ProviderResponse) { responses++ })}
					var message *AssistantMessage
					if entry == "Stream" {
						err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
							switch e := event.(type) {
							case Done:
								terminals++
								message = &e.Message
							case StreamError:
								t.Errorf("unexpected stream error: %v", e.Err)
							}
						}, opts...)
					} else {
						message, err = client.Complete(t.Context(), model, prompt, opts...)
						terminals++
					}
					require.NoError(t, err)
					require.NotNil(t, message)
					assert.Equal(t, []int{wantRequests, wantRequests, 1, wantRequests, 1}, []int{requests, payloads, responses, closes, terminals})
					assert.Equal(t, "msg_metadata", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, StopReasonStop, message.StopReason)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "ok"}}, message.Content)
					assert.Equal(t, 5, message.Usage.TotalTokens)
					if tc.retry {
						require.Len(t, message.Diagnostics, 1)
						assert.Equal(t, "anthropic_strict_tools_fallback", message.Diagnostics[0].Type)
					}
					assert.Equal(t, len(tc.metadata), len(applyOptions(opts).metadata))
					for name, value := range tc.metadata {
						if name != "callback" {
							assert.Equal(t, value, applyOptions(opts).metadata[name])
						}
					}
				})
			}
		}
	}
}

func TestAnthropicNeverPresentsAsClaudeCode(t *testing.T) {
	for _, provider := range []string{ProviderAnthropic, ProviderGitHubCopilot} {
		for _, key := range []string{"test-key", "sk-ant-oat-test"} {
			for _, entry := range []string{"Stream", "Complete"} {
				t.Run(provider+"/"+key+"/"+entry, func(t *testing.T) {
					refused := provider == ProviderAnthropic && key == "sk-ant-oat-test"
					model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: provider, BaseURL: "https://test.invalid", MaxTokens: 4096}
					prompt := &Context{SystemPrompt: "policy", Messages: []Message{{Role: "user", Content: "question"}}, Tools: []Tool{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}}
					before, err := json.Marshal(prompt)
					require.NoError(t, err)
					var requests, payloads, closes, terminals int
					var callbackPayload string
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						wire, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						assert.Equal(t, callbackPayload, string(wire))
						assert.NotContains(t, string(wire), "Claude Code")
						var body map[string]any
						require.NoError(t, json.Unmarshal(wire, &body))
						assert.Equal(t, []any{map[string]any{"type": "text", "text": "policy"}}, body["system"])
						assert.Equal(t, "read", body["tools"].([]any)[0].(map[string]any)["name"])
						assert.Empty(t, req.Header.Get("x-app"))
						assert.NotContains(t, req.Header.Get("User-Agent"), "claude-cli/")
						assert.NotContains(t, req.Header.Get("anthropic-beta"), "claude-code")
						assert.NotContains(t, req.Header.Get("anthropic-beta"), "oauth-2025-04-20")
						if provider == ProviderGitHubCopilot {
							assert.Equal(t, "Bearer "+key, req.Header.Get("Authorization"))
							assert.Empty(t, req.Header.Get("x-api-key"))
							assert.Equal(t, "user", req.Header.Get("X-Initiator"))
							assert.Equal(t, "conversation-edits", req.Header.Get("Openai-Intent"))
						} else {
							assert.Equal(t, key, req.Header.Get("x-api-key"))
							assert.Empty(t, req.Header.Get("Authorization"))
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: &anthropicEOFBody{Reader: strings.NewReader(`{"id":"msg_shape","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":2}}`), closes: &closes}}, nil
					})}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					opts := []Option{WithAPIKey(key), WithCacheRetention("none"), WithPayloadCallback(func(payload any) any { payloads++; callbackPayload = payload.(string); return nil })}
					var message *AssistantMessage
					if entry == "Stream" {
						err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
							if done, ok := event.(Done); ok {
								terminals++
								message = &done.Message
							}
						}, opts...)
					} else {
						message, err = client.Complete(t.Context(), model, prompt, opts...)
						if err == nil {
							terminals++
						}
					}
					if refused {
						require.ErrorIs(t, err, errAnthropicSubscriptionToken)
						assert.Nil(t, message)
						assert.Equal(t, []int{0, 0, 0, 0}, []int{requests, payloads, closes, terminals}, "a subscription token never reaches the API")
						return
					}
					require.NoError(t, err)
					require.NotNil(t, message)
					assert.Equal(t, []int{1, 1, 1, 1}, []int{requests, payloads, closes, terminals})
					assert.Equal(t, StopReasonToolUse, message.StopReason)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockToolCall, ID: "call_1", Name: "Read", Arguments: json.RawMessage(`{}`)}}, message.Content, "tool names come back as the model sent them")
					after, err := json.Marshal(prompt)
					require.NoError(t, err)
					assert.Equal(t, string(before), string(after))
				})
			}
		}
	}
}

func TestAnthropicSSEUTF8ReachesTransport(t *testing.T) {
	for _, tc := range []struct{ name, wire, want string }{
		{"valid multibyte", "é水😀", "é水😀"},
		{"replacement character", "�", "�"},
		{"embedded BOM", "\xef\xbb\xbf", "\ufeff"},
		{"invalid lead", "\xff", "�"},
		{"adjacent invalid bytes", "\xff\xff", "��"},
		{"truncated scalar", "\xe2\x82", "�"},
		{"bad continuation", "\xe2\x82\xff", "��"},
		{"encoded surrogate", "\xed\xa0\x80", "���"},
		{"above unicode ceiling", "\xf4\x90\x80\x80", "����"},
		{"overlong scalar", "\xc0\xaf", "��"},
		{"recovery around valid scalar", "\xe2\x82é\xff", "�é�"},
	} {
		for _, mode := range []string{"buffered", "one byte", "data with EOF"} {
			for _, shape := range []string{"content", "named error"} {
				t.Run(tc.name+"/"+mode+"/"+shape, func(t *testing.T) {
					var body strings.Builder
					emit := func(event string) { body.WriteString("data: " + event + "\n\n") }
					emit(`{"type":"message_start","message":{"id":"msg_utf8","usage":{"input_tokens":3}}}`)
					emit(`{"type":"content_block_start","index":3,"content_block":{"type":"text","text":"` + tc.wire + `"}}`)
					emit(`{"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"` + tc.wire + `"}}`)
					emit(`{"type":"content_block_stop","index":3}`)
					emit(`{"type":"content_block_start","index":8,"content_block":{"type":"tool_use","id":"call_1","name":"read","input":{"n":9007199254740993,"text":"` + tc.wire + `"}}}`)
					emit(`{"type":"content_block_stop","index":8}`)
					emit(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`)
					emit(`{"type":"message_stop"}`)
					if shape == "named error" {
						body.WriteString("event: error\ndata: diagnostic " + tc.wire)
					}
					var requests, payloads, responses, closes, terminals int
					var callbackPayload string
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						wire, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						assert.Equal(t, callbackPayload, string(wire))
						var reader io.Reader = strings.NewReader(body.String())
						switch mode {
						case "one byte":
							reader = iotest.OneByteReader(reader)
						case "data with EOF":
							reader = iotest.DataErrReader(reader)
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &anthropicEOFBody{Reader: reader, closes: &closes}}, nil
					})}
					model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
					client, err := newAnthropicClient(model)
					require.NoError(t, err)
					prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
					before, err := json.Marshal(prompt)
					require.NoError(t, err)
					opts := []Option{WithAPIKey("test-key"), WithPayloadCallback(func(payload any) any { payloads++; callbackPayload = payload.(string); return nil }), WithResponseCallback(func(_ ProviderResponse) { responses++ })}
					check := func(message AssistantMessage) {
						t.Helper()
						assert.Equal(t, "msg_utf8", message.ResponseID)
						assert.Equal(t, 5, message.Usage.TotalTokens)
						if shape == "named error" {
							assert.Equal(t, StopReasonError, message.StopReason)
							assert.Equal(t, "diagnostic "+tc.want, message.ErrorMessage)
						} else {
							assert.Equal(t, StopReasonToolUse, message.StopReason)
							assert.Empty(t, message.ErrorMessage)
						}
						require.Len(t, message.Content, 2)
						assert.Equal(t, tc.want+tc.want, message.Content[0].Text)
						assert.Equal(t, `{"n":9007199254740993,"text":"`+tc.want+`"}`, string(message.Content[1].Arguments))
						_, err := json.Marshal(message)
						assert.NoError(t, err)
					}
					err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
						switch e := event.(type) {
						case TextStart:
							assert.Equal(t, tc.want, e.Partial.Content[0].Text)
						case TextDelta:
							assert.Equal(t, tc.want, e.Delta)
						case TextEnd:
							assert.Equal(t, tc.want+tc.want, e.Content)
						case ToolCallEnd:
							assert.Equal(t, `{"n":9007199254740993,"text":"`+tc.want+`"}`, string(e.ToolCall.Arguments))
						case Done:
							terminals++
							check(e.Message)
						case StreamError:
							terminals++
							check(e.Msg)
						}
					}, opts...)
					require.NoError(t, err)
					message, err := client.Complete(t.Context(), model, prompt, opts...)
					require.NoError(t, err)
					require.NotNil(t, message)
					check(*message)
					assert.Equal(t, []int{2, 2, 2, 2, 1}, []int{requests, payloads, responses, closes, terminals})
					after, err := json.Marshal(prompt)
					require.NoError(t, err)
					assert.Equal(t, string(before), string(after))
				})
			}
		}
	}
}

func TestAnthropicPricingTiersRecalculateThroughTransport(t *testing.T) {
	baseStart := Cost{Input: 0.00008, CacheRead: 0.00003, CacheWrite: 0.00012, Total: 0.00023}
	for _, tc := range []struct {
		name, delta                  string
		startRead, read, write, long int
		startCost, cost              Cost
	}{
		{"exact first threshold excludes output", `"output_tokens":1000`, 30, 30, 30, 10, baseStart, Cost{Input: 0.00008, Output: 0.003, CacheRead: 0.00003, CacheWrite: 0.00012, Total: 0.00323}},
		{"read crosses first threshold", `"output_tokens":1000,"cache_read_input_tokens":31`, 30, 31, 30, 10, baseStart, Cost{Input: 0.0002, Output: 0.007, CacheRead: 0.000062, CacheWrite: 0.00026, Total: 0.007522}},
		{"write crosses first threshold", `"output_tokens":1000,"cache_creation_input_tokens":31,"cache_creation":{"ephemeral_1h_input_tokens":11}`, 30, 30, 31, 11, baseStart, Cost{Input: 0.0002, Output: 0.007, CacheRead: 0.00006, CacheWrite: 0.00027, Total: 0.00753}},
		{"exact second threshold", `"output_tokens":1000,"cache_read_input_tokens":130`, 30, 130, 30, 10, baseStart, Cost{Input: 0.0002, Output: 0.007, CacheRead: 0.00026, CacheWrite: 0.00026, Total: 0.00772}},
		{"highest unsorted tier", `"output_tokens":1000,"cache_read_input_tokens":131`, 30, 131, 30, 10, baseStart, Cost{Input: 0.00044, Output: 0.013, CacheRead: 0.000393, CacheWrite: 0.00054, Total: 0.014373}},
		{"correction returns to base rates", `"output_tokens":1000,"cache_read_input_tokens":29`, 131, 29, 30, 10, Cost{Input: 0.00044, CacheRead: 0.000393, CacheWrite: 0.00054, Total: 0.001373}, Cost{Input: 0.00008, Output: 0.003, CacheRead: 0.000029, CacheWrite: 0.00012, Total: 0.003229}},
	} {
		for _, format := range []string{"SSE", "JSON"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Cost: CostProfile{
					Input: 2, Output: 3, CacheRead: 1, CacheWrite: 4,
					Tiers: []CostTier{
						{InputTokensAbove: 200, Input: 11, Output: 13, CacheRead: 3, CacheWrite: 16},
						{InputTokensAbove: 100, Input: 5, Output: 7, CacheRead: 2, CacheWrite: 8},
					},
				}}
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				before, err := json.Marshal([]any{model, prompt})
				require.NoError(t, err)
				var requests, payloads, responses, closes, starts, terminals int
				var payload string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					wire, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, payload, string(wire))
					assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
					contentType := "application/json"
					body := fmt.Sprintf(`{"id":"msg_tier","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":40,"output_tokens":1000,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"cache_creation":{"ephemeral_1h_input_tokens":%d}}}`, tc.read, tc.write, tc.long)
					if format == "SSE" {
						contentType = "text/event-stream"
						body = fmt.Sprintf("event: message_start\ndata: "+`{"type":"message_start","message":{"id":"msg_tier","usage":{"input_tokens":40,"output_tokens":0,"cache_read_input_tokens":%d,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_1h_input_tokens":10}}}}`+"\n\n", tc.startRead) +
							"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"answer"}}` + "\n\n" +
							"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
							"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{` + tc.delta + `}}` + "\n\n" +
							"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(body)), closes: &closes}}, nil
				})}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				opts := []Option{WithClient(client), WithAPIKey("test-key"), WithPayloadCallback(func(value any) any { payloads++; payload = value.(string); return nil }), WithResponseCallback(func(_ ProviderResponse) { responses++ })}
				check := func(message AssistantMessage) {
					t.Helper()
					assert.Equal(t, "msg_tier", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, model.API, message.API)
					assert.Equal(t, model.Provider, message.Provider)
					assert.Equal(t, StopReasonStop, message.StopReason)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
					assert.Equal(t, Usage{Input: 40, Output: 1000, CacheRead: tc.read, CacheWrite: tc.write, CacheWrite1h: tc.long, TotalTokens: 1040 + tc.read + tc.write, Cost: tc.cost}, message.Usage)
				}
				err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case TextStart:
						starts++
						assert.Equal(t, Usage{Input: 40, CacheRead: tc.startRead, CacheWrite: 30, CacheWrite1h: 10, TotalTokens: 70 + tc.startRead, Cost: tc.startCost}, e.Partial.Usage)
					case Done:
						terminals++
						assert.Equal(t, 1, responses)
						check(e.Message)
					case StreamError:
						t.Errorf("unexpected terminal error: %v", e.Err)
					}
				}, opts...)
				require.NoError(t, err)
				message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
				require.NoError(t, err)
				require.NotNil(t, message)
				check(*message)
				assert.Equal(t, []int{2, 2, 2, 2, 1}, []int{requests, payloads, responses, closes, terminals})
				wantStarts := 0
				if format == "SSE" {
					wantStarts = 1
				}
				assert.Equal(t, wantStarts, starts)
				after, err := json.Marshal([]any{model, prompt})
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicSSEUsagePresenceReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		clear       bool
	}{
		{name: "absent"},
		{name: "null", usage: `,"usage":null`},
		{name: "empty", usage: `,"usage":{}`},
		{name: "null fields", usage: `,"usage":{"input_tokens":null,"output_tokens":null,"cache_read_input_tokens":null,"cache_creation_input_tokens":null,"cache_creation":{"ephemeral_1h_input_tokens":null}}`},
		{name: "explicit zero", usage: `,"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_1h_input_tokens":0}}`, clear: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &Model{ID: "claude-usage", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Cost: CostProfile{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}}
			prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
			before, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			var requests, payloads, responses, closes, starts, ends, terminals int
			var payload string
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				assert.Equal(t, requests-1, closes)
				assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, payload, string(wire))
				var body map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(wire, &body))
				assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(body["messages"]))
				bodyText := strings.Join([]string{
					`data: {"type":"message_start","message":{"id":"msg_usage","usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":1000,"cache_creation":{"ephemeral_1h_input_tokens":400}}}}`,
					`data: {"type":"message_delta","usage":{"output_tokens":5}}`,
					`data: {"type":"message_delta","delta":{}` + tc.usage + `}`,
					`data: {"type":"content_block_start","index":7,"content_block":{"type":"text","text":"answer"}}`,
					`data: {"type":"content_block_stop","index":7}`,
					`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
					`data: {"type":"message_stop"}`,
				}, "\n\n") + "\n\n"
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(bodyText)), closes: &closes}}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithAPIKey("test-key"), WithCacheRetention("none"), WithPayloadCallback(func(value any) any { payloads++; payload = value.(string); return nil }), WithResponseCallback(func(response ProviderResponse) {
				responses++
				assert.Equal(t, http.StatusOK, response.Status)
				assert.Equal(t, requests-1, closes)
			}), WithClient(client)}
			checkUsage := func(usage Usage) {
				t.Helper()
				if tc.clear {
					assert.Equal(t, Usage{}, usage)
					return
				}
				assert.Equal(t, []int{10, 5, 2, 1000, 400, 1017}, []int{usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite, usage.CacheWrite1h, usage.TotalTokens})
				assert.InDelta(t, 0.00005, usage.Cost.Input, 1e-12)
				assert.InDelta(t, 0.000125, usage.Cost.Output, 1e-12)
				assert.InDelta(t, 0.000001, usage.Cost.CacheRead, 1e-12)
				assert.InDelta(t, 0.00775, usage.Cost.CacheWrite, 1e-12)
				assert.InDelta(t, 0.007926, usage.Cost.Total, 1e-12)
			}
			check := func(message AssistantMessage) {
				t.Helper()
				checkUsage(message.Usage)
				assert.Equal(t, "msg_usage", message.ResponseID)
				assert.Equal(t, model.ID, message.Model)
				assert.Equal(t, model.API, message.API)
				assert.Equal(t, model.Provider, message.Provider)
				assert.Equal(t, StopReasonStop, message.StopReason)
				assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
				assert.Empty(t, message.Diagnostics)
			}
			err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
				assert.Equal(t, 1, responses)
				switch e := event.(type) {
				case Start:
					starts++
				case TextStart:
					checkUsage(e.Partial.Usage)
				case TextEnd:
					ends++
					checkUsage(e.Partial.Usage)
				case Done:
					terminals++
					check(e.Message)
				default:
					t.Errorf("unexpected event %T", event)
				}
			}, opts...)
			require.NoError(t, err)
			message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
			require.NoError(t, err)
			require.NotNil(t, message)
			check(*message)
			assert.Equal(t, []int{2, 2, 2, 2, 1, 1, 1}, []int{requests, payloads, responses, closes, starts, ends, terminals})
			after, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}

func TestAnthropicCacheUsageReachesTransport(t *testing.T) {
	const initial = `"input_tokens":10,"cache_read_input_tokens":2,"cache_creation_input_tokens":1000`
	for _, tc := range []struct {
		name, start, delta, final string
		write, long               int
		cost                      float64
	}{
		{"start breakdown", initial + `,"cache_creation":{"ephemeral_1h_input_tokens":400}`, `"output_tokens":5`, initial + `,"output_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":400}`, 1000, 400, 0.00775},
		{"delta only", `"input_tokens":0,"output_tokens":0`, initial + `,"output_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":1000}`, initial + `,"output_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":1000}`, 1000, 1000, 0.01},
		{"delta replaces breakdown", initial + `,"cache_creation":{"ephemeral_1h_input_tokens":1000}`, `"output_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":400}`, initial + `,"output_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":400}`, 1000, 400, 0.00775},
		{"zero breakdown", initial + `,"cache_creation":{"ephemeral_1h_input_tokens":400}`, `"output_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":0}`, initial + `,"output_tokens":5`, 1000, 0, 0.00625},
		{"null breakdown", initial + `,"cache_creation":{"ephemeral_1h_input_tokens":400}`, `"output_tokens":5,"cache_creation":null`, initial + `,"output_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":400}`, 1000, 400, 0.00775},
		{"null nested count", initial + `,"cache_creation":{"ephemeral_1h_input_tokens":400}`, `"output_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":null}`, initial + `,"output_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":400}`, 1000, 400, 0.00775},
		{"null counts", initial + `,"cache_creation":{"ephemeral_1h_input_tokens":400}`, `"output_tokens":5,"input_tokens":null,"cache_creation_input_tokens":null,"cache_read_input_tokens":null,"cache_creation":{}`, initial + `,"output_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":400}`, 1000, 400, 0.00775},
		{"no breakdown", initial, `"output_tokens":5`, initial + `,"output_tokens":5`, 1000, 0, 0.00625},
		{"clear writes", initial + `,"cache_creation":{"ephemeral_1h_input_tokens":400}`, `"output_tokens":5,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_1h_input_tokens":0}`, `"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":2,"cache_creation_input_tokens":0`, 0, 0, 0},
	} {
		for _, format := range []string{"SSE", "JSON"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				var requests, callbacks, responses, closes, terminals int
				var payload string
				previous := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = previous })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					wire, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, payload, string(wire))
					body := `{"id":"msg_cache","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{` + tc.final + `}}`
					contentType := "application/json"
					if format == "SSE" {
						contentType = "text/event-stream"
						body = "data: " + `{"type":"message_start","message":{"id":"msg_cache","usage":{` + tc.start + `}}}` + "\n\n" +
							"data: " + `{"type":"content_block_start","index":3,"content_block":{"type":"text","text":"answer"}}` + "\n\n" +
							"data: " + `{"type":"content_block_stop","index":3}` + "\n\n" +
							"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{` + tc.delta + `}}` + "\n\n" +
							"data: " + `{"type":"message_stop"}` + "\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(body)), closes: &closes}}, nil
				})}
				model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096, Cost: CostProfile{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}}
				client, err := newAnthropicClient(model)
				require.NoError(t, err)
				prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
				before, err := json.Marshal(prompt)
				require.NoError(t, err)
				opts := []Option{WithAPIKey("test-key"), WithPayloadCallback(func(value any) any { callbacks++; payload = value.(string); return nil }), WithResponseCallback(func(_ ProviderResponse) { responses++ })}
				check := func(message AssistantMessage) {
					t.Helper()
					assert.Equal(t, "msg_cache", message.ResponseID)
					assert.Equal(t, StopReasonStop, message.StopReason)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
					assert.Equal(t, 10, message.Usage.Input)
					assert.Equal(t, 5, message.Usage.Output)
					assert.Equal(t, 2, message.Usage.CacheRead)
					assert.Equal(t, tc.write, message.Usage.CacheWrite)
					assert.Equal(t, tc.long, message.Usage.CacheWrite1h)
					assert.Equal(t, 17+tc.write, message.Usage.TotalTokens)
					assert.InDelta(t, 0.00005, message.Usage.Cost.Input, 1e-12)
					assert.InDelta(t, 0.000125, message.Usage.Cost.Output, 1e-12)
					assert.InDelta(t, 0.000001, message.Usage.Cost.CacheRead, 1e-12)
					assert.InDelta(t, tc.cost, message.Usage.Cost.CacheWrite, 1e-12)
					assert.InDelta(t, 0.000176+tc.cost, message.Usage.Cost.Total, 1e-12)
				}
				err = client.Stream(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
					switch e := event.(type) {
					case Done:
						terminals++
						check(e.Message)
					case StreamError:
						t.Errorf("unexpected terminal error: %v", e.Err)
					}
				}, opts...)
				require.NoError(t, err)
				message, err := client.Complete(t.Context(), model, prompt, opts...)
				require.NoError(t, err)
				require.NotNil(t, message)
				check(*message)
				assert.Equal(t, []int{2, 2, 2, 2, 1}, []int{requests, callbacks, responses, closes, terminals})
				after, err := json.Marshal(prompt)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
			})
		}
	}
}

func TestAnthropicTemperatureCompatibilityReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name, compat string
		omit         bool
	}{
		{name: "claude-opus-4-7", omit: true},
		{name: "claude-opus-4-8", omit: true},
		{name: "claude-opus-4-6"},
		{name: "claude-sonnet-4-6"},
		{name: "custom absent"},
		{name: "custom empty", compat: `{}`},
		{name: "custom null", compat: `{"supportsTemperature":null}`},
		{name: "custom true", compat: `{"supportsTemperature":true}`},
		{name: "custom false", compat: `{"supportsTemperature":false}`, omit: true},
		{name: "custom managed", compat: `{"supportsTemperature":true,"supportsMidConvoEffort":true}`, omit: true},
	} {
		for _, temperature := range []*float64{nil, new(0.0), new(1.0)} {
			for _, entry := range []string{"Stream", "Complete"} {
				label := "unset"
				if temperature != nil {
					label = fmt.Sprint(*temperature)
				}
				t.Run(tc.name+"/"+label+"/"+entry, func(t *testing.T) {
					model := &Model{ID: "vendor--claude-opus-4-7", API: APIAnthropicMessages, Provider: ProviderAnthropic, Reasoning: true, MaxTokens: 32000}
					if strings.HasPrefix(tc.name, "claude-") {
						catalog, err := GetModel(ProviderAnthropic, tc.name)
						require.NoError(t, err)
						copy := *catalog
						model = &copy
					} else if tc.compat != "" {
						require.NoError(t, json.Unmarshal([]byte(tc.compat), &model.Compat))
					}
					model.BaseURL = "https://test.invalid"
					prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
					before, err := json.Marshal([]any{model, prompt})
					require.NoError(t, err)
					var requests, payloads, responses, closes, terminals int
					var payload string
					previous := http.DefaultClient
					t.Cleanup(func() { http.DefaultClient = previous })
					http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						requests++
						assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
						wire, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						assert.Equal(t, payload, string(wire))
						var body map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(wire, &body))
						if tc.omit || temperature == nil {
							assert.NotContains(t, body, "temperature")
						} else {
							assert.Equal(t, fmt.Sprint(*temperature), string(body["temperature"]))
						}
						if tc.name == "custom managed" {
							assert.JSONEq(t, `{"type":"adaptive","display":"summarized","block_binding":{"prefix_mismatch_behavior":"drop_block"}}`, string(body["thinking"]))
							assert.JSONEq(t, `{"effort":"high"}`, string(body["output_config"]))
						} else {
							assert.JSONEq(t, `{"type":"disabled"}`, string(body["thinking"]))
							assert.NotContains(t, body, "output_config")
						}
						assert.JSONEq(t, fmt.Sprintf("%q", model.ID), string(body["model"]))
						assert.Equal(t, "true", string(body["stream"]))
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: &anthropicEOFBody{Reader: strings.NewReader(`{"id":"msg_temperature","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn"}`), closes: &closes}}, nil
					})}
					simple := &SimpleStreamOptions{
						APIKey: "test-key", CacheRetention: "none", Temperature: temperature,
						OnPayload:  func(value any) any { payloads++; payload = value.(string); return nil },
						OnResponse: func(_ ProviderResponse) { responses++ },
					}
					var message *AssistantMessage
					if entry == "Stream" {
						err = StreamSimpleWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
							switch e := event.(type) {
							case Done:
								terminals++
								message = &e.Message
							case StreamError:
								t.Errorf("unexpected stream error: %v", e.Err)
							}
						}, simple)
					} else {
						message, err = CompleteSimpleWithModel(t.Context(), model, prompt, simple)
					}
					require.NoError(t, err)
					require.NotNil(t, message)
					assert.Equal(t, "msg_temperature", message.ResponseID)
					assert.Equal(t, model.ID, message.Model)
					assert.Equal(t, StopReasonStop, message.StopReason)
					assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
					assert.Equal(t, []int{1, 1, 1, 1}, []int{requests, payloads, responses, closes})
					if entry == "Stream" {
						assert.Equal(t, 1, terminals)
					}
					after, err := json.Marshal([]any{model, prompt})
					require.NoError(t, err)
					assert.Equal(t, string(before), string(after))
				})
			}
		}
	}
}

func TestAnthropicResponseMediaTypeReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
		sse               bool
	}{
		{"SSE control", "text/event-stream", true},
		{"SSE parameters", "text/event-stream; charset=utf-8", true},
		{"SSE mixed case", "Text/Event-Stream; Charset=UTF-8", true},
		{"SSE whitespace", " \tTEXT/EVENT-STREAM \t; charset=utf-8", true},
		{"JSON control", "application/json", false},
		{"JSON quoted parameter", `application/json; profile="text/event-stream"`, false},
		{"JSON subtype suffix", "text/event-stream+json", false},
		{"JSON subtype prefix", "x-text/event-stream", false},
		{"JSON missing header", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &Model{ID: "claude-media-type", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
			prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
			before, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			var requests, payloads, responses, closes, starts, terminals int
			var payload string
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				assert.Equal(t, requests-1, closes)
				assert.Equal(t, "https://test.invalid/v1/messages", req.URL.String())
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, payload, string(wire))
				var request struct {
					Model    string
					Messages json.RawMessage
				}
				require.NoError(t, json.Unmarshal(wire, &request))
				assert.Equal(t, model.ID, request.Model)
				assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(request.Messages))
				body := `{"id":"msg_media","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
				if tc.sse {
					body = "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_media","usage":{"input_tokens":3,"output_tokens":2}}}` + "\n\n" +
						"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"answer"}}` + "\n\n" +
						"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
						"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
						"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {tc.contentType}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(strings.NewReader(body)), closes: &closes}}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithClient(client), WithAPIKey("test-key"), WithMaxRetries(1), WithCacheRetention("none"), WithPayloadCallback(func(value any) any { payloads++; payload = value.(string); return nil }), WithResponseCallback(func(response ProviderResponse) {
				responses++
				assert.Equal(t, http.StatusOK, response.Status)
				assert.Equal(t, tc.contentType, response.Headers["Content-Type"])
				assert.Equal(t, requests-1, closes)
			})}
			check := func(message AssistantMessage) {
				t.Helper()
				assert.Equal(t, "msg_media", message.ResponseID)
				assert.Equal(t, model.ID, message.Model)
				assert.Equal(t, model.API, message.API)
				assert.Equal(t, model.Provider, message.Provider)
				assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
				assert.Equal(t, Usage{Input: 3, Output: 2, TotalTokens: 5}, message.Usage)
				assert.Equal(t, StopReasonStop, message.StopReason)
				assert.Empty(t, message.ErrorMessage)
			}
			err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
				assert.Equal(t, 1, responses)
				switch e := event.(type) {
				case Start:
					starts++
				case Done:
					terminals++
					check(e.Message)
				case StreamError:
					t.Errorf("unexpected error terminal: %v", e.Err)
				}
			}, opts...)
			assert.NoError(t, err)
			message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
			assert.NoError(t, err)
			if assert.NotNil(t, message) {
				check(*message)
			}
			wantStarts := 0
			if tc.sse {
				wantStarts = 1
			}
			assert.Equal(t, []int{2, 2, 2, 2, wantStarts, 1}, []int{requests, payloads, responses, closes, starts, terminals})
			after, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
			assert.Empty(t, client.tools)
			assert.Equal(t, http.Header{"Content-Type": {"application/json"}, "anthropic-version": {"2023-06-01"}}, client.transport.headers)
		})
	}
}

func TestAnthropicJSONResponseCompletionReachesTransport(t *testing.T) {
	for _, tail := range []string{"whitespace", "second value", "junk", "read failure"} {
		t.Run(tail, func(t *testing.T) {
			model := &Model{ID: "claude-json-tail", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
			prompt := &Context{Messages: []Message{{Role: "user", Content: "question"}}}
			before, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			readErr := errors.New("response body failed after JSON value")
			var requests, payloads, responses, closes, terminals int
			var payload string
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				assert.Equal(t, requests-1, closes)
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, payload, string(wire))
				var request struct {
					Model    string
					Messages json.RawMessage
				}
				require.NoError(t, json.Unmarshal(wire, &request))
				assert.Equal(t, model.ID, request.Model)
				assert.JSONEq(t, `[{"role":"user","content":[{"type":"text","text":"question"}]}]`, string(request.Messages))
				body := `{"id":"msg_json_tail","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
				var reader io.Reader = strings.NewReader(body)
				if requests < 3 {
					switch tail {
					case "whitespace":
						reader = io.MultiReader(reader, strings.NewReader(" \r\n\t"))
					case "second value":
						reader = io.MultiReader(reader, strings.NewReader(`{"error":"unexpected extra response"}`))
					case "junk":
						reader = io.MultiReader(reader, strings.NewReader("not JSON"))
					case "read failure":
						reader = io.MultiReader(reader, iotest.ErrReader(readErr))
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}, "X-Should-Retry": {"true"}, "Retry-After-Ms": {"0"}}, Body: &anthropicEOFBody{Reader: iotest.OneByteReader(reader), closes: &closes}}, nil
			})}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithClient(client), WithAPIKey("test-key"), WithCacheRetention("none"), WithMaxRetries(2), WithPayloadCallback(func(value any) any { payloads++; payload = value.(string); return nil }), WithResponseCallback(func(response ProviderResponse) {
				responses++
				assert.Equal(t, http.StatusOK, response.Status)
				assert.Equal(t, requests-1, closes)
			})}
			checkMessage := func(message *AssistantMessage) {
				t.Helper()
				require.NotNil(t, message)
				assert.Equal(t, "msg_json_tail", message.ResponseID)
				assert.Equal(t, model.ID, message.Model)
				assert.Equal(t, model.API, message.API)
				assert.Equal(t, model.Provider, message.Provider)
				assert.Equal(t, []ContentBlock{{Type: ContentBlockText, Text: "answer"}}, message.Content)
				assert.Equal(t, Usage{Input: 3, Output: 2, TotalTokens: 5}, message.Usage)
				assert.Equal(t, StopReasonStop, message.StopReason)
				assert.Empty(t, message.ErrorMessage)
			}
			checkError := func(err error) {
				t.Helper()
				if tail == "whitespace" {
					require.NoError(t, err)
					return
				}
				assert.Error(t, err)
				if tail == "read failure" {
					assert.ErrorIs(t, err, readErr)
				}
				if tail == "junk" {
					var syntaxErr *json.SyntaxError
					assert.ErrorAs(t, err, &syntaxErr)
				}
			}
			err = StreamWithModel(t.Context(), model, prompt, func(_ context.Context, event StreamEvent) {
				assert.Equal(t, 1, responses)
				assert.Zero(t, closes)
				switch e := event.(type) {
				case Done:
					terminals++
					checkMessage(&e.Message)
				default:
					t.Errorf("unexpected event: %T", event)
				}
			}, opts...)
			checkError(err)
			wantTerminals := 0
			if tail == "whitespace" {
				wantTerminals = 1
			}
			assert.Equal(t, wantTerminals, terminals)
			message, err := CompleteWithModel(t.Context(), model, prompt, opts...)
			checkError(err)
			if tail == "whitespace" {
				checkMessage(message)
			} else {
				assert.Nil(t, message)
			}
			message, err = CompleteWithModel(t.Context(), model, prompt, opts...)
			require.NoError(t, err)
			checkMessage(message)
			assert.Equal(t, []int{3, 3, 3, 3}, []int{requests, payloads, responses, closes})
			after, err := json.Marshal([]any{model, prompt})
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
			assert.Empty(t, client.tools)
			assert.Equal(t, http.Header{"Content-Type": {"application/json"}, "anthropic-version": {"2023-06-01"}}, client.transport.headers)
		})
	}
}

func TestAnthropicEnvironmentBearerReachesTransport(t *testing.T) {
	for _, tc := range []struct {
		name, token, envKey, key, header, wantKey, wantBearer string
	}{
		{name: "bearer only", token: "sk-ant-oat-bearer", wantBearer: "Bearer sk-ant-oat-bearer"},
		{name: "bearer beats environment key", token: "bearer", envKey: "environment", wantBearer: "Bearer bearer"},
		{name: "explicit beats bearer", token: "bearer", key: "explicit", wantKey: "explicit"},
		{name: "caller header wins", token: "bearer", header: "Bearer caller", wantBearer: "Bearer caller"},
		{name: "environment key fallback", envKey: "environment", wantKey: "environment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_AUTH_TOKEN", tc.token)
			t.Setenv("ANTHROPIC_API_KEY", tc.envKey)
			t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			var requests int
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				assert.Equal(t, tc.wantKey, req.Header.Get("x-api-key"))
				assert.Equal(t, tc.wantBearer, req.Header.Get("Authorization"))
				assert.NotContains(t, req.Header.Get("anthropic-beta"), "oauth-2025-04-20")
				assert.Empty(t, req.Header.Get("x-app"))
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_auth","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))}, nil
			})}
			model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid", MaxTokens: 4096}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithAPIKey(tc.key)}
			if tc.header != "" {
				opts = append(opts, WithHeaders(map[string]string{"AUTHORIZATION": tc.header}))
			}
			message, err := client.Complete(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "question"}}}, opts...)
			require.NoError(t, err)
			assert.Equal(t, StopReasonStop, message.StopReason)
			assert.Equal(t, 1, requests)
		})
	}
}

func TestAnthropicEnvironmentBearerDoesNotRefreshAnAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	previous := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = previous })
	var requests, refreshes int
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		assert.Equal(t, "Bearer bearer", req.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"unauthorized"}}`))}, nil
	})}
	model := &Model{ID: "claude-test", API: APIAnthropicMessages, Provider: ProviderAnthropic, BaseURL: "https://test.invalid"}
	client, err := newAnthropicClient(model)
	require.NoError(t, err)
	_, err = client.Complete(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "question"}}}, WithMaxRetries(0), WithTransportOption(TransportOption{Provider: ProviderAnthropic, RefreshCreds: func(context.Context, string) (string, error) { refreshes++; return "", nil }}))
	require.Error(t, err)
	assert.Equal(t, 1, requests)
	assert.Zero(t, refreshes)
}

func TestAnthropicFederationRequestsAvoidOAuthShapingAndRespectKeys(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	env := federationOptions(t).providerEnv
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			exchanges, messages := 0, 0
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/v1/oauth/token" {
					exchanges++
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"federation-access","expires_in":3600}`))}, nil
				}
				messages++
				if explicit {
					require.Equal(t, "explicit-key", req.Header.Get("x-api-key"))
					require.Empty(t, req.Header.Get("Authorization"))
				} else {
					require.Equal(t, "Bearer federation-access", req.Header.Get("Authorization"))
					require.Empty(t, req.Header.Get("x-api-key"))
					require.Contains(t, req.Header.Get("anthropic-beta"), "oauth-2025-04-20")
				}
				require.NotContains(t, req.Header.Get("anthropic-beta"), "claude-code")
				require.NotContains(t, req.Header.Get("anthropic-beta"), "oidc-federation")
				return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"synthetic rejection"}}`))}, nil
			})}
			model := &Model{ID: "claude-test", Provider: ProviderAnthropic, API: APIAnthropicMessages, BaseURL: "https://api.anthropic.invalid"}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			opts := []Option{WithProviderEnv(env), WithMaxRetries(0)}
			if explicit {
				opts = append(opts, WithAPIKey("explicit-key"))
			}
			for range 2 {
				err = client.Stream(t.Context(), model, &Context{}, func(context.Context, StreamEvent) {}, opts...)
				require.ErrorContains(t, err, "synthetic rejection")
			}
			require.Equal(t, 2, messages)
			if explicit {
				require.Zero(t, exchanges)
			} else {
				require.Equal(t, 1, exchanges)
			}
		})
	}
}

func TestAnthropicFederationAdmissionAndHeaderPrecedence(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
		t.Setenv(name, "")
	}
	env := federationOptions(t).providerEnv
	for _, tc := range []struct {
		name, provider string
		option         Option
		wantRequests   int
	}{
		{"header", ProviderAnthropic, WithHeaders(map[string]string{"Authorization": "Bearer explicit"}), 1},
		{"disabled", ProviderAnthropic, WithoutEnvCredentials(), 0},
		{"other provider", "other-anthropic-compatible", WithMaxRetries(0), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			requests := 0
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				require.Equal(t, "/v1/messages", req.URL.Path)
				require.Equal(t, "Bearer explicit", req.Header.Get("Authorization"))
				return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"synthetic rejection"}}`))}, nil
			})}
			model := &Model{ID: "claude-test", Provider: tc.provider, API: APIAnthropicMessages, BaseURL: "https://api.anthropic.invalid"}
			client, err := newAnthropicClient(model)
			require.NoError(t, err)
			err = client.Stream(t.Context(), model, &Context{}, func(context.Context, StreamEvent) {}, WithProviderEnv(env), tc.option, WithMaxRetries(0))
			require.Error(t, err)
			require.Equal(t, tc.wantRequests, requests)
		})
	}
}

func TestAnthropicAdaptiveThinkingUsesCompatibilityNotModelID(t *testing.T) {
	for _, id := range []string{"claude-opus-4-6", "claude-opus-4-7", "vendor--claude-opus-latest"} {
		for _, adaptive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", id, adaptive), func(t *testing.T) {
				model := &Model{ID: id, API: APIAnthropicMessages, Provider: ProviderAnthropic, Reasoning: true, MaxTokens: 32000, Compat: &CompatibilitySettings{ForceAdaptiveThinking: adaptive}}
				client := &anthropicClient{model: model}
				opts := &options{thinkingEnabled: true, reasoning: ReasoningHigh}
				body, err := client.buildRequest(model, &Context{}, opts)
				require.NoError(t, err)
				var request anthropicRequest
				require.NoError(t, json.Unmarshal([]byte(body), &request))
				require.NotNil(t, request.Thinking)
				if adaptive {
					assert.Equal(t, "adaptive", request.Thinking.Type)
					assert.NotNil(t, request.OutputConfig)
				} else {
					assert.Equal(t, "enabled", request.Thinking.Type)
					assert.Positive(t, request.Thinking.BudgetTokens)
					assert.Nil(t, request.OutputConfig)
				}
				betas := client.buildHeaders("key", model, opts)["anthropic-beta"]
				assert.Equal(t, !adaptive, strings.Contains(betas, "interleaved-thinking-2025-05-14"))
			})
		}
	}
}
