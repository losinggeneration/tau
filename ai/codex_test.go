package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCodexCapabilities(t *testing.T) {
	if !CodexCapabilities.Vision {
		t.Fatal("CodexCapabilities.Vision = false, want true")
	}
	if !CodexCapabilities.Thinking {
		t.Fatal("CodexCapabilities.Thinking = false, want true")
	}
}

func TestConvertMessageForCodexToolResultUsesMessageMetadata(t *testing.T) {
	msg := Message{
		Role:       "toolResult",
		ToolCallID: "call_1",
		ToolName:   "read",
		Content: []ContentBlock{
			{Type: ContentBlockText, Text: "file contents"},
		},
	}

	converted := convertMessageForCodex(msg)

	if converted["role"] != "tool" {
		t.Fatalf("role = %v, want tool", converted["role"])
	}

	content, ok := converted["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content type = %T", converted["content"])
	}
	if len(content) != 1 {
		t.Fatalf("len(content) = %d, want 1", len(content))
	}
	if content[0]["type"] != "function_call_output" {
		t.Fatalf("type = %v, want function_call_output", content[0]["type"])
	}
	if content[0]["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id = %v, want call_1", content[0]["tool_call_id"])
	}
	if content[0]["output"] != "file contents" {
		t.Fatalf("output = %v, want file contents", content[0]["output"])
	}
}

func TestBuildCodexRequestBodyMatchesResponsesParity(t *testing.T) {
	model := &Model{ID: "gpt-5.3-codex", Provider: ProviderOpenAICodex, API: APIOpenAICodexResponses, Reasoning: true}
	prompt := &Context{
		SystemPrompt: "You are helpful.",
		Messages:     []Message{{Role: "user", Content: "Hello"}},
		Tools: []Tool{{
			Name:        "read_file",
			Description: "Read a file",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
	}

	body := buildCodexRequestBody(model, prompt, &options{
		sessionID:        "sess_123",
		serviceTier:      "priority",
		textVerbosity:    "high",
		reasoningEffort:  "minimal",
		reasoningSummary: "detailed",
		maxTokens:        8192,
	})

	if got := body["store"]; got != false {
		t.Fatalf("store = %v, want false", got)
	}
	if got := body["prompt_cache_key"]; got != "sess_123" {
		t.Fatalf("prompt_cache_key = %v, want sess_123", got)
	}
	if got := body["service_tier"]; got != "priority" {
		t.Fatalf("service_tier = %v, want priority", got)
	}
	text, ok := body["text"].(map[string]any)
	if !ok {
		t.Fatalf("text type = %T", body["text"])
	}
	if got := text["verbosity"]; got != "high" {
		t.Fatalf("text.verbosity = %v, want high", got)
	}
	include, ok := body["include"].([]string)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v, want reasoning.encrypted_content", body["include"])
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning type = %T", body["reasoning"])
	}
	if got := reasoning["effort"]; got != "low" {
		t.Fatalf("reasoning.effort = %v, want low", got)
	}
	if got := reasoning["summary"]; got != "detailed" {
		t.Fatalf("reasoning.summary = %v, want detailed", got)
	}
	if _, ok := body["input"]; !ok {
		t.Fatal("input missing from body")
	}
	if _, ok := body["messages"]; ok {
		t.Fatalf("messages = %v, want omitted", body["messages"])
	}
	if _, ok := body["max_output_tokens"]; ok {
		t.Fatalf("max_output_tokens = %v, want omitted because the Codex endpoint rejects it", body["max_output_tokens"])
	}
	tools, ok := body["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want one converted tool", body["tools"])
	}
	if strict, ok := tools[0]["strict"].(bool); !ok || strict {
		t.Fatalf("tools[0].strict = %v, want false", tools[0]["strict"])
	}
}

func TestExtractCodexAccountID(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct_123"}}`))
	token := "header." + payload + ".signature"

	accountID, err := extractCodexAccountID(token)
	if err != nil {
		t.Fatalf("extractCodexAccountID: %v", err)
	}
	if accountID != "acct_123" {
		t.Fatalf("accountID = %q, want acct_123", accountID)
	}
}

func TestNormalizeCodexResponsesPayloadNormalizesTerminalEvents(t *testing.T) {
	payload := []byte(strings.Join([]string{
		`data: {"type":"response.done","response":{"id":"resp_1","status":"completed","service_tier":"default"}}`,
		`data: {"type":"response.incomplete","response":{"id":"resp_2"}}`,
		"",
	}, "\n"))

	normalized := normalizeCodexResponsesPayload(payload, "flex")
	lines := strings.Split(strings.TrimSpace(normalized), "\n")
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2", len(lines))
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], "data: ")), &first); err != nil {
		t.Fatalf("unmarshal first: %v", err)
	}
	if got := first["type"]; got != "response.completed" {
		t.Fatalf("first.type = %v, want response.completed", got)
	}
	firstResp := first["response"].(map[string]any)
	if got := firstResp["service_tier"]; got != "flex" {
		t.Fatalf("first.response.service_tier = %v, want flex", got)
	}

	var second map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &second); err != nil {
		t.Fatalf("unmarshal second: %v", err)
	}
	if got := second["type"]; got != "response.completed" {
		t.Fatalf("second.type = %v, want response.completed", got)
	}
	secondResp := second["response"].(map[string]any)
	if got := secondResp["status"]; got != "incomplete" {
		t.Fatalf("second.response.status = %v, want incomplete", got)
	}
}

func TestStreamCodexResponseUsesRequestedServiceTierForDefaultResponseTier(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_123"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_123","status":"completed","service_tier":"default","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":0}}}}`,
		"",
	}, "\n"))

	var done Done
	err := streamCodexResponse(
		context.Background(),
		&Model{ID: "gpt-5.3-codex", Provider: ProviderOpenAICodex, Cost: CostProfile{Input: 1000, Output: 2000}},
		stream,
		func(_ context.Context, event StreamEvent) {
			if e, ok := event.(Done); ok {
				done = e
			}
		},
		&ResponsesStreamOptions{ServiceTier: "flex", ApplyServiceTierPricing: ServiceTierPricing},
	)
	if err != nil {
		t.Fatalf("streamCodexResponse: %v", err)
	}
	if done.Message.Usage.Cost.Total != 0.1 {
		t.Fatalf("usage cost total = %v, want 0.1", done.Message.Usage.Cost.Total)
	}
}

func TestDoCodexSSERequestRetriesRetryableStatus(t *testing.T) {
	oldRetries, oldDelay := codexMaxRetries, codexBaseRetryDelay
	codexMaxRetries = 1
	codexBaseRetryDelay = time.Millisecond
	defer func() {
		codexMaxRetries = oldRetries
		codexBaseRetryDelay = oldDelay
	}()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `rate limit exceeded`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()

	resp, err := doCodexSSERequest(context.Background(), &Model{BaseURL: server.URL}, []byte(`{"ok":true}`), "jwt.token.sig", "acct_123", &options{})
	if err != nil {
		t.Fatalf("doCodexSSERequest: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestParseCodexUsageLimitError(t *testing.T) {
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_at":1787198689,"eligible_promo":null,"resets_in_seconds":490120}}`)

	usageErr := parseCodexUsageLimitError(http.StatusTooManyRequests, body)
	if usageErr == nil {
		t.Fatal("parseCodexUsageLimitError = nil, want error")
	}
	if usageErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d, want %d", usageErr.StatusCode, http.StatusTooManyRequests)
	}
	if usageErr.Message != "The usage limit has been reached" {
		t.Fatalf("Message = %q, want %q", usageErr.Message, "The usage limit has been reached")
	}
	if usageErr.PlanType != "plus" {
		t.Fatalf("PlanType = %q, want plus", usageErr.PlanType)
	}
	if want := time.Unix(1787198689, 0).In(time.Local); !usageErr.ResetsAt.Equal(want) {
		t.Fatalf("ResetsAt = %v, want %v", usageErr.ResetsAt, want)
	}
	if want := 490120 * time.Second; usageErr.ResetsIn != want {
		t.Fatalf("ResetsIn = %v, want %v", usageErr.ResetsIn, want)
	}

	wantMessage := "openai codex: status 429: The usage limit has been reached. Available again in 136h8m40s at " +
		time.Unix(1787198689, 0).In(time.Local).Format(time.DateTime)
	if usageErr.Error() != wantMessage {
		t.Fatalf("Error() = %q, want %q", usageErr.Error(), wantMessage)
	}
}

func TestParseCodexUsageLimitErrorIgnoresOtherErrors(t *testing.T) {
	cases := map[string][]byte{
		"invalid json":  []byte(`rate limit exceeded`),
		"other type":    []byte(`{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`),
		"wrong shape":   []byte(`{"message":"The usage limit has been reached"}`),
		"empty payload": []byte(`{}`),
	}

	for name, body := range cases {
		if usageErr := parseCodexUsageLimitError(http.StatusTooManyRequests, body); usageErr != nil {
			t.Fatalf("%s: parseCodexUsageLimitError = %v, want nil", name, usageErr)
		}
	}
}

func TestDoCodexSSERequestReturnsUsageLimitError(t *testing.T) {
	oldRetries, oldDelay := codexMaxRetries, codexBaseRetryDelay
	codexMaxRetries = 3
	codexBaseRetryDelay = time.Millisecond
	defer func() {
		codexMaxRetries = oldRetries
		codexBaseRetryDelay = oldDelay
	}()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_at":1787198689,"eligible_promo":null,"resets_in_seconds":490120}}`)
	}))
	defer server.Close()

	_, err := doCodexSSERequest(context.Background(), &Model{BaseURL: server.URL}, []byte(`{"ok":true}`), "jwt.token.sig", "acct_123", &options{})
	if err == nil {
		t.Fatal("doCodexSSERequest error = nil, want usage limit error")
	}

	var usageErr *CodexUsageLimitError
	if !errors.As(err, &usageErr) {
		t.Fatalf("error type = %T, want *CodexUsageLimitError (%v)", err, err)
	}
	if usageErr.ResetsIn != 490120*time.Second {
		t.Fatalf("ResetsIn = %v, want %v", usageErr.ResetsIn, 490120*time.Second)
	}
	if !strings.Contains(err.Error(), "The usage limit has been reached. Available again in ") {
		t.Fatalf("error = %q, want formatted usage limit message", err)
	}

	// Usage limit errors are terminal: retrying cannot succeed, so the
	// request must not be retried.
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestDoCodexSSERequestSetsSessionHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("chatgpt-account-id"); got != "acct_123" {
			t.Fatalf("chatgpt-account-id = %q, want acct_123", got)
		}
		if got := r.Header.Get("session_id"); got != "sess_123" {
			t.Fatalf("session_id = %q, want sess_123", got)
		}
		if got := r.Header.Get("x-client-request-id"); got != "sess_123" {
			t.Fatalf("x-client-request-id = %q, want sess_123", got)
		}
		if got := r.Header.Get("OpenAI-Beta"); got != codexSSEBeta {
			t.Fatalf("OpenAI-Beta = %q, want %q", got, codexSSEBeta)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()

	resp, err := doCodexSSERequest(context.Background(), &Model{BaseURL: server.URL}, []byte(`{"ok":true}`), "jwt.token.sig", "acct_123", &options{sessionID: "sess_123"})
	if err != nil {
		t.Fatalf("doCodexSSERequest: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
}

func TestStreamCodexWebSocketReadsCompletionEvents(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()

		var request map[string]any
		if err := conn.ReadJSON(&request); err != nil {
			t.Fatalf("ReadJSON: %v", err)
		}
		if got := request["type"]; got != "response.create" {
			t.Fatalf("request.type = %v, want response.create", got)
		}
		if got := r.Header.Get("chatgpt-account-id"); got != "acct_123" {
			t.Fatalf("chatgpt-account-id = %q, want acct_123", got)
		}
		if err := conn.WriteJSON(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_1", "status": "completed"},
		}); err != nil {
			t.Fatalf("WriteJSON: %v", err)
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	payload, started, err := streamCodexWebSocket(
		context.Background(),
		wsURL,
		buildCodexWebSocketHeaders(nil, nil, "acct_123", "token", "req_123"),
		map[string]any{"model": "gpt-5.3-codex"},
	)
	if err != nil {
		t.Fatalf("streamCodexWebSocket: %v", err)
	}
	if !started {
		t.Fatal("started = false, want true")
	}
	if !strings.Contains(payload, `"type":"response.completed"`) {
		t.Fatalf("payload = %q, want completion event", payload)
	}
}
