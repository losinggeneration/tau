package ai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
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
	headers := sessionHeaders(&options{sessionID: "sess_123"})
	if headers["session_id"] != "sess_123" {
		t.Fatalf("session_id = %q, want sess_123", headers["session_id"])
	}
	if headers["x-client-request-id"] != "sess_123" {
		t.Fatalf("x-client-request-id = %q, want sess_123", headers["x-client-request-id"])
	}

	if got := sessionHeaders(&options{
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

	opts := BuildBaseOptions(&Model{MaxTokens: 48000}, &SimpleStreamOptions{
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
	if got.MaxTokens != 32000 {
		t.Fatalf("MaxTokens = %d, want 32000 default clamp", got.MaxTokens)
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
