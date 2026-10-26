package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithHeadersAndResponseCallback(t *testing.T) {
	called := false
	headers := map[string]string{"X-Test": "1"}
	metadata := map[string]any{"user_id": "u1"}

	ctx := WithOptions(
		context.Background(),
		WithTransport(TransportWS),
		WithSessionID("session-1"),
		WithCacheRetention(string(CacheRetentionLong)),
		WithMaxRetryDelayMs(2500),
		WithMetadata(metadata),
		WithHeaders(headers),
		WithPayloadCallback(func(payload any) any {
			return payload
		}),
		WithResponseCallback(func(resp ProviderResponse) {
			called = true
			if resp.Status != 201 {
				t.Fatalf("status = %d, want 201", resp.Status)
			}
		}),
	)

	opts := OptionsFromContext(ctx)
	if opts.headers["X-Test"] != "1" {
		t.Fatalf("header = %q, want 1", opts.headers["X-Test"])
	}
	if opts.preferredTransport != TransportWS {
		t.Fatalf("transport = %q, want %q", opts.preferredTransport, TransportWS)
	}
	if opts.sessionID != "session-1" {
		t.Fatalf("sessionID = %q, want session-1", opts.sessionID)
	}
	if opts.cacheRetention != string(CacheRetentionLong) {
		t.Fatalf("cacheRetention = %q, want %q", opts.cacheRetention, CacheRetentionLong)
	}
	if opts.maxRetryDelayMs != 2500 {
		t.Fatalf("maxRetryDelayMs = %d, want 2500", opts.maxRetryDelayMs)
	}
	if opts.metadata["user_id"] != "u1" {
		t.Fatalf("metadata[user_id] = %v, want u1", opts.metadata["user_id"])
	}
	if opts.onPayload == nil {
		t.Fatal("expected payload callback")
	}
	if got := opts.onPayload(map[string]any{"ok": true}); got == nil {
		t.Fatal("expected payload callback result")
	}

	if opts.onResponse == nil {
		t.Fatal("expected response callback")
	}

	opts.onResponse(ProviderResponse{Status: 201})
	if !called {
		t.Fatal("expected response callback to be invoked")
	}
}

func TestResolveCacheRetentionDefaults(t *testing.T) {
	t.Setenv("PI_CACHE_RETENTION", "")
	if got := resolveCacheRetention(""); got != CacheRetentionShort {
		t.Fatalf("resolveCacheRetention(\"\") = %q, want %q", got, CacheRetentionShort)
	}

	t.Setenv("PI_CACHE_RETENTION", string(CacheRetentionLong))
	if got := resolveCacheRetention(""); got != CacheRetentionLong {
		t.Fatalf("resolveCacheRetention(\"\") with env = %q, want %q", got, CacheRetentionLong)
	}

	if got := resolveCacheRetention(string(CacheRetentionNone)); got != CacheRetentionNone {
		t.Fatalf("resolveCacheRetention(none) = %q, want %q", got, CacheRetentionNone)
	}
}

func TestSessionHeadersRespectsCacheRetention(t *testing.T) {
	headers := sessionHeaders(&Model{}, &options{sessionID: "sess_123"})
	if headers["session_id"] != "sess_123" {
		t.Fatalf("session_id = %q, want sess_123", headers["session_id"])
	}
	if headers["x-client-request-id"] != "sess_123" {
		t.Fatalf("x-client-request-id = %q, want sess_123", headers["x-client-request-id"])
	}

	if got := sessionHeaders(&Model{}, &options{
		sessionID:      "sess_123",
		cacheRetention: string(CacheRetentionNone),
	}); got != nil {
		t.Fatalf("sessionHeaders(cacheRetention=none) = %#v, want nil", got)
	}
}

func TestOpenAIStreamOptionsCallbacksApplyToLiveRequest(t *testing.T) {
	t.Helper()

	var (
		requestBody      string
		responseCallback ProviderResponse
		responseCalled   bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll(request body): %v", err)
		}
		requestBody = string(body)

		if got := r.Header.Get("Authorization"); got != "Bearer runtime-key" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer runtime-key")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", "req_123")
		if _, err := io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n"); err != nil {
			t.Fatalf("WriteString(chunk): %v", err)
		}
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			t.Fatalf("WriteString(done): %v", err)
		}
	}))
	defer server.Close()

	client := &openAIClient{
		model: &Model{ID: "gpt-4o", API: APIOpenAICompletions, Provider: ProviderOpenAI},
		transport: &HTTPTransport{
			httpClient: server.Client(),
			baseURL:    server.URL,
			headers:    make(http.Header),
		},
	}

	var doneEvents []Done
	err := client.Stream(
		context.Background(),
		client.model,
		&Context{
			Messages: []Message{{Role: "user", Content: "original"}},
		},
		func(_ context.Context, event StreamEvent) {
			if done, ok := event.(Done); ok {
				doneEvents = append(doneEvents, done)
			}
		},
		WithAPIKey("runtime-key"),
		WithPayloadCallback(func(payload any) any {
			body, ok := payload.(string)
			if !ok {
				t.Fatalf("payload type = %T, want string", payload)
			}
			return strings.Replace(body, "original", "mutated", 1)
		}),
		WithResponseCallback(func(resp ProviderResponse) {
			responseCalled = true
			responseCallback = resp
		}),
	)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if !strings.Contains(requestBody, "mutated") {
		t.Fatalf("request body = %q, want payload callback mutation", requestBody)
	}
	if strings.Contains(requestBody, "original") {
		t.Fatalf("request body = %q, want original payload replaced", requestBody)
	}
	if !responseCalled {
		t.Fatal("expected response callback to be invoked")
	}
	if responseCallback.Status != http.StatusOK {
		t.Fatalf("response status = %d, want %d", responseCallback.Status, http.StatusOK)
	}
	if got := responseCallback.Headers["X-Request-Id"]; got != "req_123" {
		t.Fatalf("response header X-Request-Id = %q, want req_123", got)
	}
	if len(doneEvents) != 1 {
		t.Fatalf("len(doneEvents) = %d, want 1", len(doneEvents))
	}
	if got := doneEvents[0].Reason; got != StopReasonStop {
		t.Fatalf("done reason = %q, want %q", got, StopReasonStop)
	}
}

func TestClampReasoning(t *testing.T) {
	tests := []struct {
		input    ReasoningLevel
		expected ReasoningLevel
	}{
		{ReasoningOff, ReasoningOff},
		{ReasoningMinimal, ReasoningMinimal},
		{ReasoningLow, ReasoningLow},
		{ReasoningMedium, ReasoningMedium},
		{ReasoningHigh, ReasoningHigh},
		{ReasoningXHigh, ReasoningHigh}, // xhigh collapses to high
	}

	for _, tt := range tests {
		t.Run(string(tt.input), func(t *testing.T) {
			got := ClampReasoning(tt.input)
			if got != tt.expected {
				t.Errorf("ClampReasoning(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestThinkingBudgetForLevelDefaults(t *testing.T) {
	tests := []struct {
		level ReasoningLevel
		want  int
	}{
		{ReasoningOff, 0},
		{ReasoningMinimal, 1024},
		{ReasoningLow, 2048},
		{ReasoningMedium, 8192},
		{ReasoningHigh, 16384},
		{ReasoningXHigh, 16384}, // xhigh uses high budget
	}

	for _, tt := range tests {
		t.Run(string(tt.level), func(t *testing.T) {
			got := ThinkingBudgetForLevel(tt.level, nil)
			if got != tt.want {
				t.Errorf("ThinkingBudgetForLevel(%q, nil) = %d, want %d", tt.level, got, tt.want)
			}
		})
	}
}

func TestThinkingBudgetForLevelCustomBudgets(t *testing.T) {
	custom := &ThinkingBudgets{
		Medium: new(9999),
		High:   new(33333),
	}

	if got := ThinkingBudgetForLevel(ReasoningMedium, custom); got != 9999 {
		t.Errorf("ThinkingBudgetForLevel(medium, custom) = %d, want 9999", got)
	}
	if got := ThinkingBudgetForLevel(ReasoningHigh, custom); got != 33333 {
		t.Errorf("ThinkingBudgetForLevel(high, custom) = %d, want 33333", got)
	}
	// Unset levels should still use defaults
	if got := ThinkingBudgetForLevel(ReasoningMinimal, custom); got != 1024 {
		t.Errorf("ThinkingBudgetForLevel(minimal, custom) = %d, want 1024", got)
	}
}

func TestAdjustMaxTokensForThinking(t *testing.T) {
	tests := []struct {
		name           string
		baseMaxTokens  int
		modelMaxTokens int
		level          ReasoningLevel
		customBudgets  *ThinkingBudgets
		wantMaxTokens  int
		wantBudget     int
	}{
		{
			name:           "medium level within model limit",
			baseMaxTokens:  4096,
			modelMaxTokens: 32768,
			level:          ReasoningMedium,
			wantMaxTokens:  12288, // 4096 + 8192, capped at 32768
			wantBudget:     8192,
		},
		{
			name:           "high level exceeds model limit",
			baseMaxTokens:  20000,
			modelMaxTokens: 16384,
			level:          ReasoningHigh,
			wantMaxTokens:  16384, // min(20000+16384, 16384)
			wantBudget:     15360, // reserve a 1024-token output floor when capped at model max
		},
		{
			name:           "small base max tokens triggers floor",
			baseMaxTokens:  512,
			modelMaxTokens: 4096,
			level:          ReasoningLow,
			wantMaxTokens:  2560, // min(512+2048, 4096) = min(2560, 4096) = 2560
			wantBudget:     2048, // 2560 > 4096 (false) && 2560 > 2048 (false), no floor adjustment
		},
		{
			name:           "very small max triggers floor adjustment",
			baseMaxTokens:  256,
			modelMaxTokens: 2048,
			level:          ReasoningMinimal,
			wantMaxTokens:  1280, // min(256+1024, 2048)
			wantBudget:     1024, // 1280 > 1024, no floor adjustment
		},
		{
			name:           "xhigh collapses to high",
			baseMaxTokens:  1000,
			modelMaxTokens: 50000,
			level:          ReasoningXHigh,
			wantMaxTokens:  17384, // 1000 + 16384 (high budget)
			wantBudget:     16384,
		},
		{
			name:           "custom budgets override defaults",
			baseMaxTokens:  500,
			modelMaxTokens: 10000,
			level:          ReasoningMedium,
			customBudgets:  &ThinkingBudgets{Medium: new(500)},
			wantMaxTokens:  1000, // 500 + 500
			wantBudget:     500,
		},
		{
			name:           "off level produces zero budget",
			baseMaxTokens:  4096,
			modelMaxTokens: 32768,
			level:          ReasoningOff,
			wantMaxTokens:  4096,
			wantBudget:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maxTokens, budget := AdjustMaxTokensForThinking(tt.baseMaxTokens, tt.modelMaxTokens, tt.level, tt.customBudgets)
			if maxTokens != tt.wantMaxTokens {
				t.Errorf("AdjustMaxTokensForThinking(...).maxTokens = %d, want %d", maxTokens, tt.wantMaxTokens)
			}
			if budget != tt.wantBudget {
				t.Errorf("AdjustMaxTokensForThinking(...).budget = %d, want %d", budget, tt.wantBudget)
			}
		})
	}
}

func TestClampReasoningForModel(t *testing.T) {
	xhighModel := &Model{ID: "gpt-5.2", Provider: ProviderOpenAI}
	unsupportedModel := &Model{ID: "gpt-4o", Provider: ProviderOpenAI}

	tests := []struct {
		name  string
		level ReasoningLevel
		model *Model
		want  ReasoningLevel
	}{
		{"xhigh with xhigh-capable model passes through", ReasoningXHigh, xhighModel, ReasoningXHigh},
		{"xhigh with nil model collapses to high", ReasoningXHigh, nil, ReasoningHigh},
		{"xhigh with unsupported model collapses to high", ReasoningXHigh, unsupportedModel, ReasoningHigh},
		{"high passes through regardless of model", ReasoningHigh, xhighModel, ReasoningHigh},
		{"high passes through with nil model", ReasoningHigh, nil, ReasoningHigh},
		{"off passes through with nil model", ReasoningOff, nil, ReasoningOff},
		{"medium passes through regardless of model", ReasoningMedium, unsupportedModel, ReasoningMedium},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClampReasoningForModel(tt.level, tt.model)
			if got != tt.want {
				t.Errorf("ClampReasoningForModel(%q, %v) = %q, want %q", tt.level, tt.model, got, tt.want)
			}
		})
	}
}

func TestAdjustMaxTokensForThinkingForModel(t *testing.T) {
	xhighModel := &Model{ID: "gpt-5.2", Provider: ProviderOpenAI}
	unsupportedModel := &Model{ID: "gpt-4o", Provider: ProviderOpenAI}

	tests := []struct {
		name           string
		baseMaxTokens  int
		modelMaxTokens int
		level          ReasoningLevel
		model          *Model
		wantMaxTokens  int
		wantBudget     int
	}{
		{
			name:           "xhigh with xhigh-capable model preserves level",
			baseMaxTokens:  4096,
			modelMaxTokens: 32768,
			level:          ReasoningXHigh,
			model:          xhighModel,
			wantMaxTokens:  20480, // 4096 + 16384 (high budget)
			wantBudget:     16384,
		},
		{
			name:           "xhigh with unsupported model collapses to high",
			baseMaxTokens:  4096,
			modelMaxTokens: 32768,
			level:          ReasoningXHigh,
			model:          unsupportedModel,
			wantMaxTokens:  20480,
			wantBudget:     16384,
		},
		{
			name:           "xhigh with nil model collapses to high",
			baseMaxTokens:  4096,
			modelMaxTokens: 32768,
			level:          ReasoningXHigh,
			model:          nil,
			wantMaxTokens:  20480,
			wantBudget:     16384,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maxTokens, budget := AdjustMaxTokensForThinkingForModel(tt.baseMaxTokens, tt.modelMaxTokens, tt.level, tt.model, nil)
			if maxTokens != tt.wantMaxTokens {
				t.Errorf("AdjustMaxTokensForThinkingForModel(...).maxTokens = %d, want %d", maxTokens, tt.wantMaxTokens)
			}
			if budget != tt.wantBudget {
				t.Errorf("AdjustMaxTokensForThinkingForModel(...).budget = %d, want %d", budget, tt.wantBudget)
			}
		})
	}
}

func TestBuildBaseOptionsParityFields(t *testing.T) {
	temp := 0.25
	headers := map[string]string{"X-Test": "1"}
	metadata := map[string]any{"trace_id": "abc123"}
	payloadCalled := false
	responseCalled := false

	opts := BuildBaseOptions(&Model{MaxTokens: 48000}, nil, &SimpleStreamOptions{
		Temperature:     &temp,
		Reasoning:       ReasoningMedium,
		APIKey:          "runtime-key",
		CacheRetention:  string(CacheRetentionLong),
		SessionID:       "sess_123",
		Headers:         headers,
		MaxRetryDelayMs: 2500,
		Metadata:        metadata,
		OnPayload: func(payload any) any {
			payloadCalled = true
			return payload
		},
		OnResponse: func(resp ProviderResponse) {
			responseCalled = true
		},
	})

	got := ExtractSimpleStreamOptions(opts)
	if got.Temperature == nil || *got.Temperature != temp {
		t.Fatalf("Temperature = %v, want %v", got.Temperature, temp)
	}
	if got.MaxTokens != 48000 {
		t.Fatalf("MaxTokens = %d, want the model cap", got.MaxTokens)
	}
	if got.Reasoning != ReasoningMedium {
		t.Fatalf("Reasoning = %q, want %q", got.Reasoning, ReasoningMedium)
	}
	if got.APIKey != "runtime-key" {
		t.Fatalf("APIKey = %q, want runtime-key", got.APIKey)
	}
	if got.CacheRetention != string(CacheRetentionLong) {
		t.Fatalf("CacheRetention = %q, want %q", got.CacheRetention, CacheRetentionLong)
	}
	if got.SessionID != "sess_123" {
		t.Fatalf("SessionID = %q, want sess_123", got.SessionID)
	}
	if !reflect.DeepEqual(got.Headers, headers) {
		t.Fatalf("Headers = %#v, want %#v", got.Headers, headers)
	}
	if got.MaxRetryDelayMs != 2500 {
		t.Fatalf("MaxRetryDelayMs = %d, want 2500", got.MaxRetryDelayMs)
	}
	if !reflect.DeepEqual(got.Metadata, metadata) {
		t.Fatalf("Metadata = %#v, want %#v", got.Metadata, metadata)
	}
	if got.OnPayload == nil {
		t.Fatal("OnPayload = nil, want callback")
	}
	if got.OnResponse == nil {
		t.Fatal("OnResponse = nil, want callback")
	}
	_ = got.OnPayload(map[string]any{"ok": true})
	got.OnResponse(ProviderResponse{Status: 200})
	if !payloadCalled {
		t.Fatal("expected payload callback to run")
	}
	if !responseCalled {
		t.Fatal("expected response callback to run")
	}
}

func TestWithToolChoice(t *testing.T) {
	tests := []struct {
		name    string
		choice  string
		wantErr bool
	}{
		{"auto", "auto", false},
		{"none", "none", false},
		{"any", "any", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := WithOptions(context.Background(), WithToolChoice(tt.choice))
			opts := OptionsFromContext(ctx)
			if opts.toolChoice != tt.choice {
				t.Errorf("WithToolChoice(%q) = %q, want %q", tt.choice, opts.toolChoice, tt.choice)
			}
		})
	}
}

func TestBuildBaseOptionsPreservesExplicitReasoningOff(t *testing.T) {
	got := applyOptions(BuildBaseOptions(&Model{MaxTokens: 100}, nil, &SimpleStreamOptions{Reasoning: ReasoningOff}))
	if got.reasoning != ReasoningOff {
		t.Fatalf("reasoning=%q, want explicit off", got.reasoning)
	}
}

func TestClampMaxTokensToContextUsesCharacterEstimateAndReserve(t *testing.T) {
	prompt := &Context{SystemPrompt: strings.Repeat("s", 70), Messages: []Message{
		{Role: "user", Content: []ContentBlock{{Type: ContentBlockText, Text: strings.Repeat("u", 35)}, {Type: ContentBlockImage}}},
		{Role: "assistant", Content: []ContentBlock{{Type: ContentBlockThinking, Thinking: strings.Repeat("t", 7)}, {Type: ContentBlockToolCall, Name: "ab", Arguments: json.RawMessage(`{"x":1}`)}}},
		{Role: "toolResult", Content: "ééééééé"},
	}}
	// system 20, user ceil(4835/3.5)=1382, assistant ceil(16/3.5)=5, tool result 2.
	if got := EstimateContextTokens(prompt); got != 1409 {
		t.Fatalf("estimate = %d", got)
	}
	model := &Model{ContextWindow: 10000, MaxTokens: 8000}
	if got := ClampMaxTokensToContext(model, prompt, 8000); got != 10000-1409-4096 {
		t.Fatalf("clamped = %d", got)
	}
	if got := ClampMaxTokensToContext(model, prompt, 100); got != 100 {
		t.Fatalf("small request = %d", got)
	}
	if got := ClampMaxTokensToContext(&Model{ContextWindow: 5000}, prompt, 8000); got != 1 {
		t.Fatalf("exhausted context = %d", got)
	}
	if got := ClampMaxTokensToContext(&Model{}, prompt, 0); got != 1 {
		t.Fatalf("unknown window = %d", got)
	}
	if got := applyOptions(BuildBaseOptions(model, prompt, nil)).maxTokens; got != 10000-1409-4096 {
		t.Fatalf("simple default = %d", got)
	}
}

func TestProviderRetryOptionsPreserveExplicitZeroThroughSimpleOptions(t *testing.T) {
	opts := []Option{WithTimeout(0), WithMaxRetries(0), WithMaxRetryDelayMs(0)}
	simple := ExtractSimpleStreamOptions(opts)
	require.NotNil(t, simple.Timeout)
	require.NotNil(t, simple.MaxRetries)
	assert.True(t, simple.MaxRetryDelaySet)
	rebuilt := applyOptions(BuildBaseOptions(&Model{MaxTokens: 32}, &Context{}, &simple))
	assert.True(t, rebuilt.timeoutSet)
	assert.Zero(t, rebuilt.timeout)
	require.NotNil(t, rebuilt.maxRetries)
	assert.Zero(t, *rebuilt.maxRetries)
	assert.True(t, rebuilt.maxRetryDelaySet)
	assert.Zero(t, rebuilt.maxRetryDelayMs)
}

func TestSimpleOptionsRoundTripTransport(t *testing.T) {
	simple := ExtractSimpleStreamOptions([]Option{WithTransport(TransportWS)})
	assert.Equal(t, TransportWS, simple.Transport)
	assert.Equal(t, TransportWS, applyOptions(BuildBaseOptions(&Model{MaxTokens: 32}, &Context{}, &simple)).preferredTransport)
	assert.Empty(t, ExtractSimpleStreamOptions(nil).Transport)
}

func TestSimpleThinkingBudgetsRoundTrip(t *testing.T) {
	budgets := &ThinkingBudgets{Low: new(0), High: new(3456)}
	for _, level := range []ReasoningLevel{ReasoningOff, ReasoningLow, ReasoningXHigh, ReasoningMax} {
		t.Run(string(level), func(t *testing.T) {
			got := ExtractSimpleStreamOptions(BuildBaseOptions(&Model{MaxTokens: 16000}, nil, &SimpleStreamOptions{Reasoning: level, ThinkingBudgets: budgets}))
			require.Equal(t, budgets, got.ThinkingBudgets)
			require.Equal(t, level, got.Reasoning)
		})
	}
	require.Nil(t, ExtractSimpleStreamOptions(nil).ThinkingBudgets)
}

func TestSessionAffinityFormatsReachTransport(t *testing.T) {
	for _, api := range []string{APIOpenAICompletions, APIOpenAICompatible, APIOpenAIResponses, APIAzureOpenAIResponses} {
		for _, scenario := range []struct {
			name, format, provider, baseURL, session, retention string
			disabled, overrides                                 bool
		}{
			{name: "default", session: "session"},
			{name: "explicit openai", format: "openai", session: "session"},
			{name: "no session", format: "openai-nosession", session: "session"},
			{name: "openrouter", format: "openrouter", session: "session"},
			{name: "detected provider", provider: ProviderOpenRouter, session: "session"},
			{name: "detected URL", baseURL: "https://openrouter.ai/api/v1", session: "session"},
			{name: "override detection", provider: ProviderOpenRouter, format: "openai-nosession", session: "session"},
			{name: "disabled", format: "openrouter", session: "session", disabled: true},
			{name: "none", format: "openai-nosession", session: "session", retention: "none"},
			{name: "empty", format: "openrouter"},
			{name: "full affinity ID", format: "openai-nosession", session: strings.Repeat("s", 70)},
			{name: "explicit headers", format: "openai", session: "session", overrides: true},
		} {
			t.Run(api+"/"+scenario.name, func(t *testing.T) {
				t.Setenv("PI_CACHE_RETENTION", "long")
				model := &Model{ID: "test", API: api, Provider: ProviderOpenAI, BaseURL: "https://test.invalid/v1"}
				if scenario.provider != "" {
					model.Provider = scenario.provider
				}
				if scenario.baseURL != "" {
					model.BaseURL = scenario.baseURL
				}
				require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(`{"sendSessionAffinityHeaders":%t,"sessionAffinityFormat":%q}`, !scenario.disabled, scenario.format)), &model.Compat))
				want := make(map[string]string)
				format := scenario.format
				if format == "" {
					format = "openai"
					if model.Provider == ProviderOpenRouter || strings.Contains(model.BaseURL, "openrouter.ai") {
						format = "openrouter"
					}
				}
				if scenario.session != "" && scenario.retention != "none" && (!scenario.disabled || api == APIOpenAIResponses) {
					if format == "openrouter" {
						want["x-session-id"] = scenario.session
					} else {
						want["x-client-request-id"] = scenario.session
						if format == "openai" {
							want["session_id"] = scenario.session
						}
						if api == APIOpenAICompletions || api == APIOpenAICompatible {
							want["x-session-affinity"] = scenario.session
						}
					}
				}
				opts := []Option{WithAPIKey("sk-test"), WithSessionID(scenario.session), WithCacheRetention(scenario.retention)}
				if scenario.overrides {
					model.Headers = map[string]string{"X-Client-Request-Id": "model-request"}
					opts = append(opts, WithHeaders(map[string]string{"session_id": "explicit-session", "x-client-request-id": "explicit-request", "x-session-affinity": "explicit-affinity", "x-session-id": "explicit-router"}))
					want = map[string]string{"session_id": "explicit-session", "x-client-request-id": "explicit-request", "x-session-affinity": "explicit-affinity", "x-session-id": "explicit-router"}
				}
				var requests, callbacks, done int
				original := http.DefaultClient
				t.Cleanup(func() { http.DefaultClient = original })
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					for _, header := range []string{"session_id", "x-client-request-id", "x-session-affinity", "x-session-id"} {
						assert.Equal(t, want[header], req.Header.Get(header), header)
					}
					var body map[string]any
					require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
					require.NotContains(t, body, "session_id")
					if scenario.retention == "none" || scenario.session == "" {
						require.NotContains(t, body, "prompt_cache_key")
					} else {
						wantKey := clampOpenAIPromptCacheKey(scenario.session)
						assert.Equal(t, wantKey, body["prompt_cache_key"])
					}
					response := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
					if api == APIOpenAICompletions || api == APIOpenAICompatible {
						response = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
				})}
				var client Client
				switch api {
				case APIOpenAICompletions:
					var err error
					client, err = newOpenAIClient(model)
					require.NoError(t, err)
				case APIOpenAICompatible:
					client = &openAICompatClient{model: model}
				case APIOpenAIResponses:
					client = &openAIResponsesClient{model: model}
				case APIAzureOpenAIResponses:
					client = &azureClient{model: model}
					opts = append(opts, WithAzureBaseURL(model.BaseURL))
				}
				opts = append(opts, WithPayloadCallback(func(any) any { callbacks++; return nil }))
				require.NoError(t, client.Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hello"}}}, func(_ context.Context, event StreamEvent) {
					if _, ok := event.(Done); ok {
						done++
					}
				}, opts...))
				require.Equal(t, 1, requests)
				require.Equal(t, 1, callbacks)
				require.Equal(t, 1, done)
			})
		}
	}
}

func TestThinkingDisableOptionsStayProviderScoped(t *testing.T) {
	for _, api := range []string{APIAnthropicMessages, APIOpenAICompletions, APIBedrockConverseStream} {
		for _, level := range []ReasoningLevel{"", ReasoningOff, ReasoningHigh} {
			t.Run(api+"/"+string(level), func(t *testing.T) {
				model := &Model{API: api, MaxTokens: 4000}
				got := applyOptions(BuildBaseOptions(model, nil, &SimpleStreamOptions{Reasoning: level}))
				assert.Equal(t, level, got.reasoning)
				assert.False(t, got.thinkingEnabled)
				if api == APIAnthropicMessages && level == "" {
					assert.Equal(t, "disabled", got.thinkingType)
				} else {
					assert.Empty(t, got.thinkingType)
				}
			})
		}
	}
	got := applyOptions([]Option{WithThinkingEnabled(false), WithThinkingEnabled(true)})
	assert.True(t, got.thinkingEnabled)
	assert.Empty(t, got.thinkingType)
	got = applyOptions([]Option{WithThinkingEnabled(true), WithThinkingEnabled(false)})
	assert.False(t, got.thinkingEnabled)
	assert.Equal(t, "disabled", got.thinkingType)
}

func TestWithoutEnvCredentialsAtProviderBoundary(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "MISTRAL_API_KEY", "AZURE_OPENAI_API_KEY", "GOOGLE_CLOUD_API_KEY", "VERTEXAI_API_KEY", "GOOGLE_ACCESS_TOKEN", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "OPENROUTER_API_KEY", "AWS_BEARER_TOKEN_BEDROCK", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		t.Setenv(name, "ambient-secret")
	}
	t.Setenv("GOOGLE_CLOUD_PROJECT", "project")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "us-central1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	failure := errors.New("request reached isolated transport")
	var requests int
	previousTransport, previousClient := http.DefaultTransport, http.DefaultClient
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		assert.NotContains(t, req.Header.Get("Authorization"), "ambient-secret")
		assert.NotContains(t, req.Header.Get("x-api-key"), "ambient-secret")
		assert.NotContains(t, req.Header.Get("x-goog-api-key"), "ambient-secret")
		return nil, failure
	})
	http.DefaultTransport, http.DefaultClient = transport, &http.Client{Transport: transport}
	t.Cleanup(func() { http.DefaultTransport, http.DefaultClient = previousTransport, previousClient })
	for _, tc := range []struct{ provider, api string }{
		{ProviderAnthropic, APIAnthropicMessages},
		{ProviderOpenAI, APIOpenAICompletions},
		{ProviderOpenAI, APIOpenAICompatible},
		{ProviderOpenAI, APIOpenAIResponses},
		{ProviderOpenAICodex, APIOpenAICodexResponses},
		{ProviderGoogle, APIGoogleGenerativeAI},
		{ProviderMistral, APIMistralConversations},
		{ProviderAzureOpenAI, APIAzureOpenAIResponses},
		{ProviderGoogleVertex, APIGoogleVertex},
		{ProviderAmazonBedrock, APIBedrockConverseStream},
		{ProviderOpenAI, APIPiMessages},
	} {
		t.Run(tc.api, func(t *testing.T) {
			model := &Model{ID: "local-test", Provider: tc.provider, API: tc.api, BaseURL: "https://test.invalid"}
			client, err := GetClient(model)
			require.NoError(t, err)
			for _, explicit := range []string{"", "explicit-key"} {
				for _, complete := range []bool{false, true} {
					before := requests
					key := explicit
					if explicit != "" && tc.api == APIOpenAICodexResponses {
						key = "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"synthetic-account"}}`)) + ".s"
					}
					opts := []Option{WithoutEnvCredentials(), WithMaxRetries(0), WithAPIKey(key)}
					if complete {
						var result *AssistantMessage
						result, err = client.Complete(t.Context(), model, &Context{}, opts...)
						if err == nil && result != nil && result.StopReason == StopReasonError {
							err = errors.New(result.ErrorMessage)
						}
					} else {
						err = client.Stream(t.Context(), model, &Context{}, func(context.Context, StreamEvent) {}, opts...)
					}
					if explicit == "" {
						require.Error(t, err)
						if tc.api != APIOpenAICompletions {
							assert.Equal(t, before, requests, "implicit credentials must not reach transport")
						}
					} else {
						assert.Greater(t, requests, before, "explicit credentials must still reach transport")
					}
				}
			}
		})
	}
	for _, tc := range []struct{ operation, api string }{{"image", APIOpenRouterImages}, {"classifier", APIOpenAIDecisions}} {
		t.Run(tc.operation, func(t *testing.T) {
			model := &Model{ID: "local-test", Type: tc.operation, Provider: ProviderOpenRouter, API: tc.api, BaseURL: "https://test.invalid"}
			before := requests
			var err error
			if tc.operation == "image" {
				_, err = GenerateImages(t.Context(), model, nil, WithoutEnvCredentials())
			} else {
				_, err = Classify(t.Context(), model, ClassifierContext{Questions: map[string]ClassifierQuestion{"ok": {Type: "boolean", Instructions: "ok?"}}}, WithoutEnvCredentials())
			}
			require.Error(t, err)
			assert.Equal(t, before, requests)
		})
	}
	model := &Model{ID: "test", API: APIAnthropicMessages}
	simple := ExtractSimpleStreamOptions([]Option{WithoutEnvCredentials()})
	require.True(t, simple.NoEnvCredentials)
	require.True(t, applyOptions(BuildBaseOptions(model, &Context{}, &simple)).noEnvCredentials)
}

func TestProviderEnvironmentOwnsOverridesAndSimpleRoundTrip(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-key")
	env := map[string]string{"OPENAI_API_KEY": "scoped-key"}
	option := WithProviderEnv(env)
	env["OPENAI_API_KEY"] = "mutated"
	o := applyOptions([]Option{option})
	require.Equal(t, "scoped-key", o.envAPIKey(ProviderOpenAI))
	o.providerEnv["OPENAI_API_KEY"] = "changed-options"
	require.Equal(t, "scoped-key", applyOptions([]Option{option}).envAPIKey(ProviderOpenAI))
	simple := ExtractSimpleStreamOptions([]Option{option})
	roundTrip := applyOptions(BuildBaseOptions(&Model{Provider: ProviderOpenAI}, &Context{}, &simple))
	simple.Env["OPENAI_API_KEY"] = "changed-simple"
	require.Equal(t, "scoped-key", roundTrip.envAPIKey(ProviderOpenAI))
	require.Equal(t, "ambient-key", applyOptions([]Option{WithProviderEnv(map[string]string{"OPENAI_API_KEY": ""})}).envAPIKey(ProviderOpenAI))
	require.Empty(t, applyOptions([]Option{option, WithoutEnvCredentials()}).envAPIKey(ProviderOpenAI))
}

func TestProviderEnvironmentResolvesKeysHeadersAndConfiguredReferences(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-token")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-key")
	env := map[string]string{"ANTHROPIC_AUTH_TOKEN": "scoped-token", "ANTHROPIC_API_KEY": "scoped-key", "CONFIG_SECRET": "scoped-config"}
	o := applyOptions([]Option{WithProviderEnv(env)})
	require.Equal(t, "scoped-key", o.envAPIKey(ProviderAnthropic))
	require.Equal(t, "Bearer scoped-token", o.envHeaders(ProviderAnthropic)["Authorization"])
	model := &Model{Provider: ProviderAnthropic, Headers: map[string]string{"X-Configured": "$CONFIG_SECRET"}, requestConfig: &modelRequestConfig{resolver: &configValueResolver{}, apiKey: "${CONFIG_SECRET}"}}
	resolved, err := resolveRequestOptions(t.Context(), model, []Option{WithProviderEnv(env)})
	require.NoError(t, err)
	actual := applyOptions(resolved)
	require.Equal(t, "scoped-config", actual.apiKey)
	require.Equal(t, "scoped-config", actual.headers["X-Configured"])
	require.Equal(t, "ambient-key", GetEnvAPIKey(ProviderAnthropic))
	require.Equal(t, "Bearer ambient-token", GetEnvHeaders(ProviderAnthropic)["Authorization"])
}

func TestProviderEnvironmentCredentialsReachTransport(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-key")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
	oldClient, oldTransport := http.DefaultClient, http.DefaultTransport
	t.Cleanup(func() { http.DefaultClient, http.DefaultTransport = oldClient, oldTransport })
	for _, tc := range []struct{ provider, api, variable, header, want string }{
		{ProviderOpenAI, APIOpenAICompletions, "OPENAI_API_KEY", "Authorization", "Bearer scoped-key"},
		{ProviderOpenAI, APIOpenAICompatible, "OPENAI_API_KEY", "Authorization", "Bearer scoped-key"},
		{ProviderOpenAI, APIOpenAIResponses, "OPENAI_API_KEY", "Authorization", "Bearer scoped-key"},
		{ProviderAnthropic, APIAnthropicMessages, "ANTHROPIC_API_KEY", "x-api-key", "scoped-key"},
		{ProviderAnthropic, APIAnthropicMessages, "ANTHROPIC_AUTH_TOKEN", "Authorization", "Bearer scoped-key"},
	} {
		t.Run(tc.api+tc.variable, func(t *testing.T) {
			requests := 0
			failure := errors.New("isolated request")
			transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				require.Equal(t, tc.want, req.Header.Get(tc.header))
				require.NotContains(t, req.Header.Get(tc.header), "ambient")
				return nil, failure
			})
			http.DefaultTransport, http.DefaultClient = transport, &http.Client{Transport: transport}
			model := &Model{ID: "test", Provider: tc.provider, API: tc.api, BaseURL: "https://provider.invalid"}
			client, err := GetClient(model)
			require.NoError(t, err)
			for _, complete := range []bool{false, true} {
				opts := []Option{WithProviderEnv(map[string]string{tc.variable: "scoped-key"}), WithMaxRetries(0)}
				if complete {
					_, err = client.Complete(t.Context(), model, &Context{}, opts...)
				} else {
					err = client.Stream(t.Context(), model, &Context{}, func(context.Context, StreamEvent) {}, opts...)
				}
				require.Error(t, err)
			}
			require.Equal(t, 2, requests)
		})
	}
}

func TestProviderEnvironmentCloudEndpointAndBearerSelection(t *testing.T) {
	o := applyOptions([]Option{WithProviderEnv(map[string]string{
		"AZURE_OPENAI_RESOURCE_NAME": "scoped-resource", "AZURE_OPENAI_API_VERSION": "scoped-version",
		"AZURE_OPENAI_DEPLOYMENT_NAME_MAP": "test=scoped-deployment",
		"GOOGLE_CLOUD_PROJECT":             "scoped-project", "GOOGLE_CLOUD_LOCATION": "global", "GOOGLE_ACCESS_TOKEN": "scoped-bearer",
		"AWS_REGION": "scoped-region", "AWS_BEARER_TOKEN_BEDROCK": "scoped-bedrock",
	})})
	base, version, deployment, err := resolveAzureConfig(&Model{ID: "test"}, o)
	require.NoError(t, err)
	require.Contains(t, base, "scoped-resource.openai.azure.com")
	require.Equal(t, "scoped-version", version)
	require.Equal(t, "scoped-deployment", deployment)
	t.Setenv("GOOGLE_CLOUD_API_KEY", "")
	t.Setenv("VERTEXAI_API_KEY", "")
	endpoint, auth, err := resolveVertexEndpoint(t.Context(), &Model{ID: "test"}, o)
	require.NoError(t, err)
	require.Contains(t, endpoint, "projects/scoped-project/locations/global")
	require.Equal(t, "scoped-bearer", auth.BearerToken)
	require.Equal(t, "scoped-region", resolveBedrockRegion(o))
	require.Equal(t, "scoped-bedrock", resolveBedrockBearerToken(o))
}

func TestProviderEnvironmentExpandsScopedCloudflareEndpoint(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "ambient-account")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "ambient-gateway")
	o := applyOptions([]Option{WithProviderEnv(map[string]string{"CLOUDFLARE_ACCOUNT_ID": "scoped-account", "CLOUDFLARE_GATEWAY_ID": "scoped-gateway"})})
	endpoint, err := expandBaseURLWithEnv("https://gateway.invalid/{CLOUDFLARE_ACCOUNT_ID}/{CLOUDFLARE_GATEWAY_ID}", o.providerEnvValue)
	require.NoError(t, err)
	require.Equal(t, "https://gateway.invalid/scoped-account/scoped-gateway", endpoint)
}
