package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResponsesReasoningUsesSharedLevelAndModelMapping(t *testing.T) {
	model := &Model{ID: "reasoner", Reasoning: true, ThinkingLevelMap: map[string]*string{"low": new("minimal")}}
	config := applyOptions([]Option{WithReasoning(ReasoningLow)})
	body, err := buildOpenAIResponsesBody(model, &Context{}, config)
	if err != nil {
		t.Fatal(err)
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "minimal" {
		t.Fatalf("reasoning ignored: %v", body)
	}
	config = applyOptions([]Option{WithReasoning(ReasoningLow), WithReasoningEffort("high")})
	body, err = buildOpenAIResponsesBody(model, &Context{}, config)
	if err != nil {
		t.Fatal(err)
	}
	if body["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatal("explicit effort did not win")
	}
}

func TestResponsesReasoningOffAndSamplingMatchUpstream(t *testing.T) {
	builders := map[string]func(*Model, *options) map[string]any{
		"responses": func(model *Model, o *options) map[string]any {
			body, err := buildOpenAIResponsesBody(model, &Context{}, o)
			if err != nil {
				t.Fatal(err)
			}
			return body
		},
		"azure": func(model *Model, o *options) map[string]any {
			return requireBody(t)(buildAzureRequestBody(model, &Context{}, o))
		},
		"codex": func(model *Model, o *options) map[string]any {
			return requireBody(t)(buildCodexRequestBody(model, &Context{}, o))
		},
	}
	for name, build := range builders {
		mapped := &Model{ID: "reasoner", Reasoning: true, ThinkingLevelMap: map[string]*string{"off": new("minimal")}}
		for _, opts := range [][]Option{{WithReasoning(ReasoningOff)}, nil} {
			body := build(mapped, applyOptions(opts))
			reasoning, _ := body["reasoning"].(map[string]any)
			if len(reasoning) != 1 || reasoning["effort"] != "minimal" {
				t.Fatalf("%s off reasoning = %v, want only the mapped off effort", name, body["reasoning"])
			}
			if name != "codex" && body["include"] != nil {
				t.Fatalf("%s off requested encrypted reasoning", name)
			}
		}
		unsupported := &Model{ID: "reasoner", Reasoning: true, ThinkingLevelMap: map[string]*string{"off": nil}}
		if body := build(unsupported, applyOptions([]Option{WithReasoning(ReasoningOff)})); body["reasoning"] != nil {
			t.Fatalf("%s sent reasoning for a model without an off level: %v", name, body["reasoning"])
		}
		body := build(&Model{ID: "reasoner", Reasoning: true}, applyOptions([]Option{WithReasoning(ReasoningHigh)}))
		if reasoning := body["reasoning"].(map[string]any); reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
			t.Fatalf("%s high reasoning = %v", name, reasoning)
		}

		sampled := &Model{ID: "reasoner", SamplingParams: map[string]any{"temperature": 0.2, "top_k": 40}}
		body = build(sampled, applyOptions([]Option{WithTemperature(0.9), WithSamplingParams(map[string]any{"min_p": 0.1})}))
		if name == "codex" {
			if body["top_k"] != nil {
				t.Fatalf("codex applied sampling parameters: %v", body)
			}
			continue
		}
		if body["temperature"] != 0.2 || body["top_k"] != 40 || body["min_p"] != 0.1 {
			t.Fatalf("%s sampling = %v", name, body)
		}
	}
	copilot := &Model{ID: "reasoner", Provider: ProviderGitHubCopilot, Reasoning: true}
	body, err := buildOpenAIResponsesBody(copilot, &Context{}, applyOptions(nil))
	if err != nil || body["reasoning"] != nil {
		t.Fatalf("copilot off reasoning = %v (%v)", body["reasoning"], err)
	}
}

func TestResponsesConstrainedSamplingMatchesUpstream(t *testing.T) {
	lookup := func(strict string) Tool {
		return Tool{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`), ConstrainedSampling: json.RawMessage(`{"type":"json_schema","strict":"` + strict + `"}`)}
	}
	builders := map[string]func(*Model, *Context) (map[string]any, error){
		"responses": func(model *Model, prompt *Context) (map[string]any, error) {
			return buildOpenAIResponsesBody(model, prompt, &options{})
		},
		"azure": func(model *Model, prompt *Context) (map[string]any, error) {
			return buildAzureRequestBody(model, prompt, &options{})
		},
		"codex": func(model *Model, prompt *Context) (map[string]any, error) {
			return buildCodexRequestBody(model, prompt, &options{})
		},
	}
	defaultStrict := map[string]bool{"responses": false, "azure": true, "codex": true}
	for name, build := range builders {
		for _, compat := range []*CompatibilitySettings{nil, {SupportsStrictMode: new(true)}, {SupportsStrictMode: new(false)}} {
			supported := defaultStrict[name]
			if compat != nil {
				supported = *compat.SupportsStrictMode
			}
			model := &Model{ID: "gpt", Compat: compat}
			body, err := build(model, &Context{Tools: []Tool{lookup("prefer")}})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			tool := body["tools"].([]map[string]any)[0]
			strict, present := tool["strict"]
			if present != supported || supported && strict != true {
				t.Fatalf("%s supported=%v: preferred tool = %v", name, supported, tool)
			}
			_, err = build(model, &Context{Tools: []Tool{lookup("require")}})
			if supported == (err != nil) {
				t.Fatalf("%s supported=%v: required strictness err = %v", name, supported, err)
			}
		}
		// Unsupported schemas fail closed when required and fall back when preferred.
		model := &Model{ID: "gpt", Compat: &CompatibilitySettings{SupportsStrictMode: new(true)}}
		loose := lookup("require")
		loose.Parameters = json.RawMessage(`{"type":"object","additionalProperties":true}`)
		if _, err := build(model, &Context{Tools: []Tool{loose}}); err == nil {
			t.Fatalf("%s: required unsupported schema accepted", name)
		}
		loose.ConstrainedSampling = json.RawMessage(`{"type":"json_schema","strict":"prefer"}`)
		body, err := build(model, &Context{Tools: []Tool{loose}})
		if err != nil || body["tools"].([]map[string]any)[0]["strict"] != false {
			t.Fatalf("%s: preferred unsupported schema = %v, %v", name, body["tools"], err)
		}
	}

	code := Tool{Name: "code", Description: "run", Parameters: json.RawMessage(`{"type":"object","properties":{"source":{"type":"string"}},"required":["source"]}`), ConstrainedSampling: json.RawMessage(`{"type":"grammar","variants":{"openai_lark":"start: x"}}`)}
	prompt := &Context{Tools: []Tool{code}, Messages: []Message{
		{Role: "assistant", Provider: ProviderOpenAI, API: APIOpenAIResponses, Model: "gpt", Content: []ContentBlock{
			{Type: ContentBlockToolCall, ID: "call_1|ctc_1", Name: "code", Arguments: json.RawMessage(`{"source":"x"}`)},
		}},
		{Role: "toolResult", ToolCallID: "call_1|ctc_1", ToolName: "code", Content: []ContentBlock{{Type: ContentBlockText, Text: "ok"}}},
	}}
	plain := &Model{ID: "gpt", Provider: ProviderOpenAI, API: APIOpenAIResponses}
	body := requireBody(t)(buildOpenAIResponsesBody(plain, prompt, &options{}))
	if tool := body["tools"].([]map[string]any)[0]; tool["type"] != "function" {
		t.Fatalf("grammar tool without grammar support = %v", tool)
	}
	input := body["input"].([]map[string]any)
	if input[0]["type"] != "function_call" || input[0]["id"] != nil || input[1]["type"] != "function_call_output" {
		t.Fatalf("replay without grammar support = %v", input)
	}
	grammarModel := &Model{ID: "gpt", Provider: ProviderOpenAI, API: APIOpenAIResponses, Compat: &CompatibilitySettings{SupportsOpenAIGrammarTools: true}}
	body = requireBody(t)(buildOpenAIResponsesBody(grammarModel, prompt, &options{}))
	tool := body["tools"].([]map[string]any)[0]
	if !reflect.DeepEqual(tool, map[string]any{"type": "custom", "name": "code", "description": "run", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: x"}}) {
		t.Fatalf("custom tool = %v", tool)
	}
	input = body["input"].([]map[string]any)
	if !reflect.DeepEqual(input[0], map[string]any{"type": "custom_tool_call", "id": "ctc_1", "call_id": "call_1", "name": "code", "input": "x"}) || input[1]["type"] != "custom_tool_call_output" || input[1]["output"] != "ok" {
		t.Fatalf("custom replay = %v", input)
	}
	prompt.Messages[0].Content.([]ContentBlock)[0].Arguments = json.RawMessage(`{"other":"x"}`)
	if _, err := buildOpenAIResponsesBody(grammarModel, prompt, &options{}); err == nil {
		t.Fatal("grammar call without its input property replayed")
	}
}

func TestProcessResponsesStreamCustomToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"code","input":""}}`,
		`data: {"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"print("}`,
		`data: {"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"\"hi\")"}`,
		`data: {"type":"response.custom_tool_call_input.done","output_index":0,"input":"print(\"hi\")"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"code","input":"print(\"hi\")"}}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
	}, "\n\n") + "\n\n"
	var (
		deltas strings.Builder
		end    *ToolCallEnd
		done   *Done
	)
	output := &AssistantMessage{Role: "assistant"}
	err := ProcessResponsesStream(t.Context(), newResponsesStreamReader(strings.NewReader(stream)), output, func(_ context.Context, event StreamEvent) {
		switch e := event.(type) {
		case ToolCallDelta:
			deltas.WriteString(e.Delta)
		case ToolCallEnd:
			end = &e
		case Done:
			done = &e
		}
	}, &Model{ID: "gpt"}, &ResponsesStreamOptions{GrammarInputs: map[string]string{"code": "source"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"source":"print(\"hi\")"}`
	if deltas.String() != want || end == nil || string(end.ToolCall.Arguments) != want || end.ToolCall.ID != "call_1|ctc_1" {
		t.Fatalf("deltas = %s, end = %+v", deltas.String(), end)
	}
	if done == nil || done.Message.StopReason != StopReasonToolUse || string(done.Message.Content[0].Arguments) != want {
		t.Fatalf("done = %+v", done)
	}

	changed := strings.Replace(stream, `"input":"print(\"hi\")"}}`, `"input":"other"}}`, 1)
	var failed *StreamError
	output = &AssistantMessage{Role: "assistant"}
	if err := ProcessResponsesStream(t.Context(), newResponsesStreamReader(strings.NewReader(changed)), output, func(_ context.Context, event StreamEvent) {
		if e, ok := event.(StreamError); ok {
			failed = &e
		}
	}, &Model{ID: "gpt"}, nil); err != nil {
		t.Fatal(err)
	}
	if failed == nil || !strings.Contains(failed.Err.Error(), "changed after it was closed") {
		t.Fatalf("changed input after close = %+v", failed)
	}
}

func requireBody(t *testing.T) func(map[string]any, error) map[string]any {
	t.Helper()
	return func(body map[string]any, err error) map[string]any {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
}

func requireItems(t *testing.T) func([]map[string]any, error) []map[string]any {
	t.Helper()
	return func(items []map[string]any, err error) []map[string]any {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
}

func TestResponsesHeaderTimeout(t *testing.T) {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := (&openAIResponsesClient{}).Complete(ctx, &Model{ID: "test", BaseURL: "https://test.invalid"}, &Context{}, WithAPIKey("key"), WithTimeout(time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || !strings.Contains(err.Error(), "response headers timed out") {
		t.Fatalf("header timeout ignored or lost: %v", err)
	}
}

func TestResponsesHeaderTimeoutDoesNotLimitBody(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Millisecond} {
		for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
			t.Run(fmt.Sprintf("%s/%d", timeout, status), func(t *testing.T) {
				var body *codexTestBody
				previous := http.DefaultClient
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if timeout == 0 {
						timer := time.NewTimer(10 * time.Millisecond)
						defer timer.Stop()
						select {
						case <-req.Context().Done():
							return nil, req.Context().Err()
						case <-timer.C:
						}
					}
					payload := "rejected"
					if status == http.StatusOK {
						payload = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
					}
					body = &codexTestBody{Reader: strings.NewReader(payload), ctx: req.Context(), delay: 10 * time.Millisecond}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
				})}
				t.Cleanup(func() { http.DefaultClient = previous })
				message, err := (&openAIResponsesClient{}).Complete(t.Context(), &Model{ID: "test", BaseURL: "https://test.invalid"}, &Context{}, WithAPIKey("key"), WithTimeout(timeout))
				if status == http.StatusOK {
					if err != nil || message == nil || message.StopReason != StopReasonStop {
						t.Fatalf("body timed out: message=%#v err=%v", message, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "status 400: rejected") {
					t.Fatalf("error body timed out: %v", err)
				}
				if body == nil || !body.closed {
					t.Fatal("body not closed")
				}
				if timeout > 0 && body.ctx.Err() == nil {
					t.Fatal("request context not released")
				}
			})
		}
	}
}

func TestResponsesBodyCancellationAndCleanupCauses(t *testing.T) {
	readFailure := errors.New("read failed")
	closeFailure := errors.New("close failed")
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		for _, cancelBody := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/cancel=%v", status, cancelBody), func(t *testing.T) {
				body := &codexTestBody{Reader: strings.NewReader("rejected"), readErr: readFailure, closeErr: closeFailure}
				if cancelBody {
					body.delay = time.Hour
				}
				previous := http.DefaultClient
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					body.ctx = req.Context()
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
				})}
				t.Cleanup(func() { http.DefaultClient = previous })
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer cancel()
				var terminal *StreamError
				err := (&openAIResponsesClient{}).Stream(ctx, &Model{ID: "test", BaseURL: "https://test.invalid"}, &Context{}, func(_ context.Context, event StreamEvent) {
					if failure, ok := event.(StreamError); ok {
						terminal = &failure
					}
				}, WithAPIKey("key"), WithTimeout(time.Millisecond))
				if !body.closed || body.ctx.Err() == nil || !errors.Is(err, closeFailure) {
					t.Fatalf("cleanup lost: closed=%v err=%v", body.closed, err)
				}
				cause := readFailure
				if cancelBody {
					cause = context.DeadlineExceeded
					if ctx.Err() == nil {
						t.Fatal("provider timeout interrupted body")
					}
				}
				if status == http.StatusOK {
					if terminal == nil || !errors.Is(terminal.Err, cause) {
						t.Fatalf("terminal cause lost: %#v", terminal)
					}
				} else if !errors.Is(err, cause) {
					t.Fatalf("body cause lost: %v", err)
				}
			})
		}
	}
}

func TestResponsesFamilyNamespaceReplayIsolation(t *testing.T) {
	for _, family := range []string{"responses", "azure", "codex"} {
		t.Run(family, func(t *testing.T) {
			model := &Model{ID: "gpt", Provider: family, API: family}
			for _, source := range []string{"same", "model", "provider", "api", "unknown"} {
				message := Message{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockToolCall, ID: "call|fc_1", Name: "lookup", Arguments: json.RawMessage(`{}`), Namespace: new("dynamic_tools")}}}
				message.Model, message.Provider, message.API = model.ID, model.Provider, model.API
				switch source {
				case "model":
					message.Model = "other"
				case "provider":
					message.Provider = "other"
				case "api":
					message.API = "other"
				case "unknown":
					message.Model, message.Provider, message.API = "", "", ""
				}
				prompt := &Context{Messages: []Message{message}}
				var body map[string]any
				var err error
				switch family {
				case "responses":
					body, err = buildOpenAIResponsesBody(model, prompt, &options{})
				case "azure":
					body, err = buildAzureRequestBody(model, prompt, &options{})
				case "codex":
					body, err = buildCodexRequestBody(model, prompt, &options{})
				}
				require.NoError(t, err)
				var call map[string]any
				for _, item := range body["input"].([]map[string]any) {
					if item["type"] == "function_call" {
						call = item
						break
					}
				}
				require.NotNil(t, call)
				if source == "same" {
					require.Equal(t, "dynamic_tools", call["namespace"])
				} else {
					require.NotContains(t, call, "namespace")
				}
				data, err := json.Marshal(prompt)
				require.NoError(t, err)
				require.Contains(t, string(data), `"namespace":"dynamic_tools"`)
			}
		})
	}
}

func TestResponsesCacheKeyReachesTransport(t *testing.T) {
	for _, scenario := range []struct{ name, session, retention, want string }{
		{name: "empty"},
		{name: "short", session: "session", want: "session"},
		{name: "boundary", session: strings.Repeat("界", 64), want: strings.Repeat("界", 64)},
		{name: "ascii overflow", session: strings.Repeat("x", 67), want: strings.Repeat("x", 64)},
		{name: "unicode overflow", session: strings.Repeat("界", 63) + "😀z", want: strings.Repeat("界", 63) + "😀"},
		{name: "combining marks", session: strings.Repeat("e\u0301", 33), want: strings.Repeat("e\u0301", 32)},
		{name: "disabled", session: strings.Repeat("x", 67), retention: "none"},
		{name: "explicit short", session: strings.Repeat("x", 67), retention: "short", want: strings.Repeat("x", 64)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("PI_CACHE_RETENTION", "long")
			original := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = original })
			var requests, callbacks, done int
			check := func(body map[string]any) {
				if scenario.want == "" {
					if _, ok := body["prompt_cache_key"]; ok {
						t.Errorf("unexpected cache key: %v", body["prompt_cache_key"])
					}
				} else if body["prompt_cache_key"] != scenario.want {
					t.Errorf("cache key = %v, want %q", body["prompt_cache_key"], scenario.want)
				}
			}
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				check(body)
				wantAffinity := scenario.session
				if scenario.retention == "none" {
					wantAffinity = ""
				}
				if req.Header.Get("session_id") != wantAffinity || req.Header.Get("x-client-request-id") != wantAffinity {
					t.Errorf("affinity changed: %q / %q, want %q", req.Header.Get("session_id"), req.Header.Get("x-client-request-id"), wantAffinity)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}, nil
			})}
			model := &Model{ID: "test", API: APIOpenAIResponses, Provider: ProviderOpenAI, BaseURL: "https://test.invalid/v1"}
			key := "test-key"

			err := (&openAIResponsesClient{model: model}).Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hello"}}}, func(_ context.Context, event StreamEvent) {
				if _, ok := event.(Done); ok {
					done++
				}
			}, WithAPIKey(key), WithSessionID(scenario.session), WithCacheRetention(scenario.retention), WithTransport(TransportSSE), WithAzureBaseURL("https://test.invalid/v1"), WithPayloadCallback(func(payload any) any {
				callbacks++
				body, ok := payload.(map[string]any)
				if !ok {
					t.Fatalf("payload type = %T", payload)
				}
				check(body)
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if requests != 1 || callbacks != 1 || done != 1 {
				t.Fatalf("requests=%d callbacks=%d done=%d", requests, callbacks, done)
			}
		})
	}
}

func TestResponsesCachePolicyReachesTransport(t *testing.T) {
	for _, scenario := range []struct {
		name, baseURL, retention, environment, key, provider, session string
		compatJSON, catalogID, wantRetention                          string
		wantOptions                                                   map[string]any
		keyFromEnvironment                                            bool
	}{
		{name: "default short"},
		{name: "environment long", environment: "long", wantRetention: "24h"},
		{name: "proxy long", baseURL: "https://proxy.example.com/v1", retention: "long", wantRetention: "24h"},
		{name: "default endpoint long", retention: "long", wantRetention: "24h"},
		{name: "long without session", retention: "long", session: "", wantRetention: "24h"},
		{name: "explicit short", retention: "short", environment: "long"},
		{name: "disabled", retention: "none", environment: "long"},
		{name: "long unsupported", retention: "long", compatJSON: `{"supportsLongCacheRetention":false}`},
		{name: "proxy long supported", baseURL: "https://proxy.example.com/v1", retention: "long", compatJSON: `{"supportsLongCacheRetention":true}`, wantRetention: "24h"},
		{name: "explicit mode disabled", retention: "none", environment: "long", compatJSON: `{"supportsExplicitPromptCacheMode":true}`, wantOptions: map[string]any{"mode": "explicit"}},
		{name: "explicit mode short", retention: "short", environment: "long", compatJSON: `{"supportsExplicitPromptCacheMode":true}`},
		{name: "explicit mode long", retention: "long", compatJSON: `{"supportsExplicitPromptCacheMode":true}`, wantOptions: map[string]any{"ttl": "30m"}},
		{name: "explicit mode unsupported long", retention: "long", compatJSON: `{"supportsExplicitPromptCacheMode":true,"supportsLongCacheRetention":false}`},
		{name: "explicit mode none with unsupported long", retention: "none", compatJSON: `{"supportsExplicitPromptCacheMode":true,"supportsLongCacheRetention":false}`, wantOptions: map[string]any{"mode": "explicit"}},
		{name: "explicit mode false", retention: "long", compatJSON: `{"supportsExplicitPromptCacheMode":false}`, wantRetention: "24h"},
		{name: "catalog traditional long", retention: "long", catalogID: "gpt-4o-mini", wantRetention: "24h"},
		{name: "catalog explicit long", retention: "long", catalogID: "gpt-6-luna", wantOptions: map[string]any{"ttl": "30m"}},
		{name: "catalog explicit none", retention: "none", catalogID: "gpt-6-luna", wantOptions: map[string]any{"mode": "explicit"}},
		{name: "ChatGPT long", baseURL: "https://api.openai.com/v1", retention: "long", key: "chatgpt-access-token"},
		{name: "ChatGPT explicit long", baseURL: "https://api.openai.com/v1", retention: "long", key: "chatgpt-access-token", compatJSON: `{"supportsExplicitPromptCacheMode":true}`},
		{name: "ChatGPT explicit none", baseURL: "https://api.openai.com/v1", retention: "none", key: "chatgpt-access-token", compatJSON: `{"supportsExplicitPromptCacheMode":true}`},
		{name: "environment ChatGPT token", baseURL: "https://api.openai.com/v1", retention: "long", key: "chatgpt-access-token", keyFromEnvironment: true},
		{name: "default endpoint ChatGPT token", retention: "long", key: "chatgpt-access-token"},
		{name: "proxy non-sk key", baseURL: "https://proxy.example.com/v1", retention: "long", key: "gateway-key", wantRetention: "24h"},
		{name: "other provider non-sk key", baseURL: "https://api.openai.com/v1", provider: "gateway", retention: "long", key: "gateway-key", wantRetention: "24h"},
	} {
		if scenario.name != "long without session" {
			scenario.session = "cache-session"
		}
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("PI_CACHE_RETENTION", scenario.environment)
			model := &Model{ID: "test", API: APIOpenAIResponses, Provider: ProviderOpenAI, BaseURL: scenario.baseURL}
			if scenario.catalogID != "" {
				catalogModel, err := GetModel(ProviderOpenAI, scenario.catalogID)
				require.NoError(t, err)
				copy := *catalogModel
				model = &copy
			}
			if scenario.provider != "" {
				model.Provider = scenario.provider
			}
			if scenario.compatJSON != "" {
				require.NoError(t, json.Unmarshal([]byte(scenario.compatJSON), &model.Compat))
			}
			var requests, callbacks, done int
			check := func(body map[string]any) {
				if scenario.wantRetention == "" {
					require.NotContains(t, body, "prompt_cache_retention")
				} else {
					require.Equal(t, scenario.wantRetention, body["prompt_cache_retention"])
				}
				if scenario.wantOptions == nil {
					require.NotContains(t, body, "prompt_cache_options")
				} else {
					require.Equal(t, scenario.wantOptions, body["prompt_cache_options"])
				}
				if scenario.retention == "none" || scenario.session == "" {
					require.NotContains(t, body, "prompt_cache_key")
				} else {
					require.Equal(t, scenario.session, body["prompt_cache_key"])
				}
				require.Equal(t, false, body["store"])
			}
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				var body map[string]any
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				check(body)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}, nil
			})}
			key := scenario.key
			if key == "" {
				key = "sk-test"
			}
			if scenario.keyFromEnvironment {
				t.Setenv("OPENAI_API_KEY", key)
				key = ""
			}
			err := (&openAIResponsesClient{model: model}).Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hello"}}}, func(_ context.Context, event StreamEvent) {
				if _, ok := event.(Done); ok {
					done++
				}
			}, WithAPIKey(key), WithSessionID(scenario.session), WithCacheRetention(scenario.retention), WithPayloadCallback(func(payload any) any {
				callbacks++
				body, ok := payload.(map[string]any)
				require.True(t, ok)
				check(body)
				return nil
			}))
			require.NoError(t, err)
			require.Equal(t, 1, requests)
			require.Equal(t, 1, callbacks)
			require.Equal(t, 1, done)
		})
	}
}

func TestResponsesRequestLimitsReachesTransport(t *testing.T) {
	for _, scenario := range []struct {
		name, compatJSON, baseURL, provider, key, environmentKey string
		maxTokens, wantMax                                       int
		temperature, wantTemperature                             *float64
		modelSampling, requestSampling                           map[string]any
	}{
		{name: "default compatibility", maxTokens: 1000, wantMax: 1000, temperature: new(0.5), wantTemperature: new(0.5)},
		{name: "empty compatibility", compatJSON: `{}`, maxTokens: 1000, wantMax: 1000},
		{name: "explicit support", compatJSON: `{"supportsMaxOutputTokens":true}`, maxTokens: 1000, wantMax: 1000},
		{name: "null support defaults true", compatJSON: `{"supportsMaxOutputTokens":null}`, maxTokens: 1000, wantMax: 1000},
		{name: "unsupported", compatJSON: `{"supportsMaxOutputTokens":false}`, maxTokens: 1000, temperature: new(0.5), wantTemperature: new(0.5)},
		{name: "one token", maxTokens: 1, wantMax: 16},
		{name: "below minimum", maxTokens: 15, wantMax: 16},
		{name: "minimum", maxTokens: 16, wantMax: 16},
		{name: "above minimum", maxTokens: 17, wantMax: 17},
		{name: "unset maximum"},
		{name: "negative maximum", maxTokens: -1},
		{name: "zero temperature", temperature: new(0.0), wantTemperature: new(0.0)},
		{name: "ChatGPT temperature only", key: "chatgpt-access-token", temperature: new(0.5)},
		{name: "ChatGPT zero temperature only", key: "chatgpt-access-token", temperature: new(0.0)},
		{name: "ChatGPT named fields", baseURL: "https://api.openai.com/v1", key: "chatgpt-access-token", maxTokens: 1000, temperature: new(0.5)},
		{name: "ChatGPT default endpoint", key: "chatgpt-access-token", maxTokens: 1, temperature: new(0.0)},
		{name: "ChatGPT environment credential", environmentKey: "chatgpt-access-token", maxTokens: 1000, temperature: new(0.5)},
		{name: "API key overrides ChatGPT environment", key: "sk-test", environmentKey: "chatgpt-access-token", maxTokens: 1000, wantMax: 1000, temperature: new(0.5), wantTemperature: new(0.5)},
		{name: "ChatGPT overrides API key environment", key: "chatgpt-access-token", environmentKey: "sk-test", maxTokens: 1000, temperature: new(0.5)},
		{name: "proxy non-sk key", baseURL: "https://gateway.example.com/v1", key: "gateway-key", maxTokens: 1, wantMax: 16, temperature: new(0.5), wantTemperature: new(0.5)},
		{name: "other provider non-sk key", provider: "gateway", baseURL: "https://api.openai.com/v1", key: "gateway-key", maxTokens: 1000, wantMax: 1000, temperature: new(0.5), wantTemperature: new(0.5)},
		{name: "model sampling overrides named limits", maxTokens: 1000, temperature: new(0.5), modelSampling: map[string]any{"max_output_tokens": 7, "temperature": 0.2}, wantMax: 7, wantTemperature: new(0.2)},
		{name: "request sampling overrides model", maxTokens: 1, temperature: new(0.5), modelSampling: map[string]any{"max_output_tokens": 7, "temperature": 0.2}, requestSampling: map[string]any{"max_output_tokens": 9, "temperature": 0.3}, wantMax: 9, wantTemperature: new(0.3)},
		{name: "sampling overrides unsupported", compatJSON: `{"supportsMaxOutputTokens":false}`, maxTokens: 1000, requestSampling: map[string]any{"max_output_tokens": 19}, wantMax: 19},
		{name: "sampling overrides ChatGPT exclusions", key: "chatgpt-access-token", maxTokens: 1000, temperature: new(0.5), requestSampling: map[string]any{"max_output_tokens": 21, "temperature": 0.3}, wantMax: 21, wantTemperature: new(0.3)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", scenario.environmentKey)
			model := &Model{ID: "test", API: APIOpenAIResponses, Provider: ProviderOpenAI, BaseURL: scenario.baseURL, SamplingParams: scenario.modelSampling}
			if scenario.provider != "" {
				model.Provider = scenario.provider
			}
			if scenario.compatJSON != "" {
				require.NoError(t, json.Unmarshal([]byte(scenario.compatJSON), &model.Compat))
			}
			check := func(body map[string]any) {
				if scenario.wantMax == 0 {
					require.NotContains(t, body, "max_output_tokens")
				} else {
					require.EqualValues(t, scenario.wantMax, body["max_output_tokens"])
				}
				if scenario.wantTemperature == nil {
					require.NotContains(t, body, "temperature")
				} else {
					require.Equal(t, *scenario.wantTemperature, body["temperature"])
				}
				require.Equal(t, false, body["store"])
			}
			var requests, callbacks, done int
			previous := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = previous })
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				var body map[string]any
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				check(body)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}, nil
			})}
			key := scenario.key
			if key == "" && scenario.environmentKey == "" {
				key = "sk-test"
			}
			opts := []Option{WithAPIKey(key), WithMaxTokens(scenario.maxTokens), WithSamplingParams(scenario.requestSampling), WithPayloadCallback(func(payload any) any {
				callbacks++
				body, ok := payload.(map[string]any)
				require.True(t, ok)
				check(body)
				return nil
			})}
			if scenario.temperature != nil {
				opts = append(opts, WithTemperature(*scenario.temperature))
			}
			err := (&openAIResponsesClient{model: model}).Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hello"}}}, func(_ context.Context, event StreamEvent) {
				if _, ok := event.(Done); ok {
					done++
				}
			}, opts...)
			require.NoError(t, err)
			require.Equal(t, 1, requests)
			require.Equal(t, 1, callbacks)
			require.Equal(t, 1, done)
		})
	}
}
