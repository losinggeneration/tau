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

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
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

	body := requireBody(t)(buildCodexRequestBody(model, prompt, &options{
		sessionID:        "sess_123",
		serviceTier:      "priority",
		textVerbosity:    "high",
		reasoningEffort:  "minimal",
		reasoningSummary: "detailed",
		maxTokens:        8192,
	}))

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
	original := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = original })
	var requests int
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		require.Equal(t, "acct_123", req.Header.Get("chatgpt-account-id"))
		require.Equal(t, "sess_123", req.Header.Get("session-id"))
		require.NotContains(t, req.Header, "Session_id")
		require.Equal(t, "sess_123", req.Header.Get("x-client-request-id"))
		require.Equal(t, codexSSEBeta, req.Header.Get("OpenAI-Beta"))
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	resp, err := doCodexSSERequest(t.Context(), &Model{BaseURL: "https://test.invalid"}, []byte(`{"ok":true}`), "jwt.token.sig", "acct_123", &options{sessionID: "sess_123"})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, requests)
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
		0,
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

func TestCodexWebSocketUsageLimitIsTyped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		var request map[string]any
		if err := conn.ReadJSON(&request); err != nil {
			t.Error(err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_at":1787198689,"resets_in_seconds":490120}}`)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`))
	var terminal *StreamError
	err := (&codexClient{}).Stream(t.Context(), &Model{ID: "codex", BaseURL: server.URL}, &Context{}, func(_ context.Context, event StreamEvent) {
		if event, ok := event.(StreamError); ok {
			terminal = &event
		}
	}, WithAPIKey("header."+claims+".signature"), WithTransport(TransportAuto), WithTimeout(0))
	require.NoError(t, err)
	require.NotNil(t, terminal)

	var usageErr *CodexUsageLimitError
	require.ErrorAs(t, terminal.Err, &usageErr)
	require.Equal(t, http.StatusTooManyRequests, usageErr.StatusCode)
	resetAt, ok := UsageLimitResetAt(terminal.Err)
	require.True(t, ok)
	require.True(t, resetAt.Equal(time.Unix(1787198689, 0)))
}

func TestCodexHeaderPrecedence(t *testing.T) {
	for _, headers := range []http.Header{
		buildCodexSSEHeaders(map[string]string{"originator": "model", "User-Agent": "model-agent"}, map[string]string{"Originator": "caller", "user-agent": "caller-agent", "Authorization": "wrong", "chatgpt-account-id": "wrong"}, "account", "key", ""),
		buildCodexWebSocketHeaders(map[string]string{"originator": "model", "User-Agent": "model-agent"}, map[string]string{"Originator": "caller", "user-agent": "caller-agent", "Authorization": "wrong", "chatgpt-account-id": "wrong"}, "account", "key", "request"),
	} {
		if headers.Get("originator") != "caller" || headers.Get("User-Agent") != "caller-agent" {
			t.Errorf("defaults replaced explicit headers: %v", headers)
		}
		if headers.Get("Authorization") != "Bearer key" || headers.Get("chatgpt-account-id") != "account" {
			t.Errorf("credentials not authoritative: %v", headers)
		}
	}
}

func TestCodexUsesSharedReasoningLevel(t *testing.T) {
	model := &Model{ID: "gpt-6-luna", Reasoning: true, ThinkingLevelMap: map[string]*string{"low": new("minimal")}}
	config := applyOptions([]Option{WithReasoning(ReasoningLow)})
	body := requireBody(t)(buildCodexRequestBody(model, &Context{}, config))
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "minimal" {
		t.Fatalf("reasoning=%v", body)
	}
}

func TestCodexSSEConfiguredRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		status, retries, want int
	}{
		{"zero network retries", 0, 0, 1},
		{"zero status retries", http.StatusServiceUnavailable, 0, 1},
		{"one retry", http.StatusServiceUnavailable, 1, 2},
		{"permanent failure", http.StatusBadRequest, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := errors.New("network failure")
			calls := 0
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if tc.status == 0 {
					return nil, failure
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After-Ms": []string{"0"}}, Body: io.NopCloser(strings.NewReader("failed"))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = previous })
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			_, err := doCodexSSERequest(ctx, &Model{BaseURL: "https://test.invalid"}, nil, "key", "account", applyOptions([]Option{WithMaxRetries(tc.retries)}))
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("retry budget ignored: %v", err)
			}
			if tc.status == 0 && !errors.Is(err, failure) {
				t.Fatalf("network cause lost: %v", err)
			}
			if calls != tc.want {
				t.Fatalf("requests=%d want %d", calls, tc.want)
			}
		})
	}
}

func TestCodexSSEServerRetryDelayLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    []Option
		limited bool
	}{
		{name: "default", limited: true},
		{name: "explicit", opts: []Option{WithMaxRetryDelayMs(1)}, limited: true},
		{name: "disabled", opts: []Option{WithMaxRetryDelayMs(0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader("busy"))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = previous })
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			_, err := doCodexSSERequest(ctx, &Model{BaseURL: "https://test.invalid"}, nil, "key", "account", applyOptions(append([]Option{WithMaxRetries(1)}, tc.opts...)))
			if tc.limited {
				if err == nil || !strings.Contains(err.Error(), "retry delay") || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("delay cap ignored: %v", err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("wait not canceled: %v", err)
			}
		})
	}
}

func TestCodexSSEHeaderTimeout(t *testing.T) {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := doCodexSSERequest(ctx, &Model{BaseURL: "https://test.invalid"}, nil, "key", "account", applyOptions([]Option{WithMaxRetries(0), WithTimeout(time.Millisecond)}))
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("header timeout ignored: %v", err)
	}
}

type codexTestBody struct {
	io.Reader
	ctx               context.Context
	delay             time.Duration
	readErr, closeErr error
	closed            bool
}

func (b *codexTestBody) Read(p []byte) (int, error) {
	if b.delay > 0 {
		timer := time.NewTimer(b.delay)
		defer timer.Stop()
		b.delay = 0
		select {
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		case <-timer.C:
		}
	}
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.Reader.Read(p)
}
func (b *codexTestBody) Close() error { b.closed = true; return b.closeErr }

func TestCodexSSEHeaderTimeoutDoesNotLimitBody(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			var body *codexTestBody
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if timeout == 0 {
					timer := time.NewTimer(3 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-req.Context().Done():
						return nil, req.Context().Err()
					case <-timer.C:
					}
				}
				body = &codexTestBody{Reader: strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"), ctx: req.Context(), delay: 10 * time.Millisecond}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = previous })
			payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`))
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			message, err := (&codexClient{}).Complete(ctx, &Model{ID: "codex", BaseURL: "https://test.invalid"}, &Context{}, WithAPIKey("header."+payload+".signature"), WithMaxRetries(0), WithTimeout(timeout))
			if err != nil {
				t.Fatal(err)
			}
			if message.StopReason != StopReasonStop {
				t.Fatalf("terminal outcome=%#v", message)
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

func TestCodexSSEErrorBodyCleanupIsNotRetried(t *testing.T) {
	readErr := errors.New("read failed")
	closeErr := errors.New("close failed")
	body := &codexTestBody{readErr: readErr, closeErr: closeErr}
	calls := 0
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body.ctx = req.Context()
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: body}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	_, err := doCodexSSERequest(t.Context(), &Model{BaseURL: "https://test.invalid"}, nil, "key", "account", applyOptions([]Option{WithTimeout(time.Second)}))
	if !errors.Is(err, readErr) || !errors.Is(err, closeErr) || calls != 1 || !body.closed || body.ctx.Err() == nil {
		t.Fatalf("cleanup failure lost or retried: requests=%d closed=%v err=%v", calls, body.closed, err)
	}
}

func TestCodexRetryDelay(t *testing.T) {
	if delay := codexRetryDelay(1000); delay <= 0 || delay > 16*time.Second {
		t.Fatalf("backoff overflowed: %s", delay)
	}
}

func TestCodexWebSocketCancellationUnblocksIO(t *testing.T) {
	for _, phase := range []string{"handshake", "write", "response"} {
		t.Run(phase, func(t *testing.T) {
			ready := make(chan struct{})
			release := make(chan struct{})
			serverDone := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(serverDone)
				if phase == "handshake" {
					close(ready)
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					close(ready)
					return
				}
				defer func() {
					if err := conn.Close(); err != nil {
						t.Error(err)
					}
				}()
				if phase == "write" {
					if _, _, err := conn.NextReader(); err != nil {
						t.Error(err)
					}
					close(ready)
					<-release
					return
				}
				var request map[string]any
				if err := conn.ReadJSON(&request); err != nil {
					t.Error(err)
					close(ready)
					return
				}
				close(ready)
				<-release
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type result struct {
				started bool
				err     error
			}
			done := make(chan result, 1)
			go func() {
				body := map[string]any{"model": "codex"}
				if phase == "write" {
					body["input"] = strings.Repeat("x", 16<<20)
				}
				_, started, err := streamCodexWebSocket(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil, body, 0)
				done <- result{started: started, err: err}
			}()
			<-ready
			cancel()
			timer := time.NewTimer(100 * time.Millisecond)
			defer timer.Stop()
			select {
			case result := <-done:
				close(release)
				<-serverDone
				if !errors.Is(result.err, context.Canceled) {
					t.Fatalf("cancellation cause lost: %v", result.err)
				}
				if result.started != (phase == "response") {
					t.Fatalf("started=%v phase=%s", result.started, phase)
				}
			case <-timer.C:
				close(release)
				<-done
				<-serverDone
				t.Fatal("cancellation did not unblock websocket I/O")
			}
		})
	}
}

func TestCodexWebSocketTimeoutIsHeaderOnly(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		timeout, headerDelay, bodyDelay time.Duration
		stall                           bool
	}{
		{name: "disabled", headerDelay: 5 * time.Millisecond},
		{name: "body unbounded", timeout: 5 * time.Millisecond, bodyDelay: 15 * time.Millisecond},
		{name: "header deadline", timeout: 5 * time.Millisecond, stall: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			serverDone := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(serverDone)
				if tc.stall {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				if err := sleepWithContext(r.Context(), tc.headerDelay); err != nil {
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() {
					if err := conn.Close(); err != nil {
						t.Error(err)
					}
				}()
				var request map[string]any
				if err := conn.ReadJSON(&request); err != nil {
					t.Error(err)
					return
				}
				if err := sleepWithContext(r.Context(), tc.bodyDelay); err != nil {
					return
				}
				if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			defer close(release)
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`))
			message, err := (&codexClient{}).Complete(ctx, &Model{ID: "codex", BaseURL: server.URL}, &Context{}, WithAPIKey("header."+claims+".signature"), WithTransport(TransportWS), WithTimeout(tc.timeout))
			if err != nil {
				t.Fatal(err)
			}
			<-serverDone
			if ctx.Err() != nil {
				t.Fatal("provider header timeout ignored or leaked into body")
			}
			if tc.stall {
				if message.StopReason != StopReasonError || !strings.Contains(message.ErrorMessage, "headers timed out") {
					t.Fatalf("header deadline outcome=%#v", message)
				}
			} else if message.StopReason != StopReasonStop {
				t.Fatalf("completion=%#v", message)
			}
		})
	}
}

func TestCodexWebSocketCancellationDoesNotFallback(t *testing.T) {
	for _, transport := range []Transport{TransportWS, TransportAuto} {
		t.Run(string(transport), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			serverDone := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(serverDone)
				cancel()
				timer := time.NewTimer(100 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-r.Context().Done():
				case <-timer.C:
					t.Error("cancellation did not close handshake connection")
				}
			}))
			defer server.Close()
			previous := http.DefaultClient
			calls := 0
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected SSE request")
			})}
			t.Cleanup(func() { http.DefaultClient = previous })
			claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`))
			var terminal *StreamError
			err := (&codexClient{}).Stream(ctx, &Model{ID: "codex", BaseURL: server.URL}, &Context{}, func(_ context.Context, event StreamEvent) {
				if event, ok := event.(StreamError); ok {
					terminal = &event
				}
			}, WithAPIKey("header."+claims+".signature"), WithTransport(transport), WithTimeout(0))
			<-serverDone
			if err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatalf("SSE requests after cancellation=%d", calls)
			}
			if terminal == nil || terminal.Reason != string(StopReasonAborted) || !errors.Is(terminal.Err, context.Canceled) {
				t.Fatalf("terminal=%#v", terminal)
			}
		})
	}
}

func TestCodexCacheKeyReachesTransport(t *testing.T) {
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
				require.Empty(t, req.Header.Values("session_id"))
				wantAffinity := scenario.want
				if scenario.retention == "none" {
					wantAffinity = ""
				}
				if req.Header.Get("session-id") != wantAffinity || req.Header.Get("x-client-request-id") != wantAffinity {
					t.Errorf("affinity changed: %q / %q, want %q", req.Header.Get("session-id"), req.Header.Get("x-client-request-id"), wantAffinity)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}, nil
			})}
			model := &Model{ID: "test", API: APIOpenAICodexResponses, Provider: ProviderOpenAICodex, BaseURL: "https://test.invalid/v1"}
			key := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`)) + ".signature"
			err := (&codexClient{model: model}).Stream(t.Context(), model, &Context{Messages: []Message{{Role: "user", Content: "hello"}}}, func(_ context.Context, event StreamEvent) {
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

func TestCodexWebSocketCacheIdentity(t *testing.T) {
	for _, session := range []string{"", "session", strings.Repeat("界", 63) + "😀z"} {
		for _, retention := range []string{"short", "none"} {
			o := &options{sessionID: session, cacheRetention: retention}
			headers := buildCodexWebSocketHeaders(nil, nil, "account", "key", cacheSessionID(o))
			require.Empty(t, headers.Values("session_id"))
			got := headers.Get("session-id")
			if got == "" || headers.Get("x-client-request-id") != got {
				t.Fatalf("missing or inconsistent WebSocket identity: %q", got)
			}
			if session == "" || retention == "none" {
				if _, err := uuid.Parse(got); err != nil {
					t.Fatalf("disabled/absent cache did not generate request ID: %v", err)
				}
			} else {
				runes := []rune(session)
				if want := string(runes[:min(len(runes), 64)]); got != want {
					t.Fatalf("WebSocket identity = %q, want %q", got, want)
				}
			}
			if o.sessionID != session {
				t.Fatal("caller session changed")
			}
		}
	}
}

func TestCodexSessionHeaderPrecedence(t *testing.T) {
	for _, transport := range []Transport{TransportSSE, TransportWS} {
		for _, session := range []string{"", "session", strings.Repeat("x", 67)} {
			t.Run(string(transport)+"/"+session, func(t *testing.T) {
				modelHeaders := map[string]string{"Session-ID": "model-hyphen", "Session_id": "model-underscore", "X-Client-Request-Id": "model-request"}
				optionHeaders := map[string]string{"session-id": "caller-hyphen", "session_id": "caller-underscore", "x-client-request-id": "caller-request"}
				var headers http.Header
				if transport == TransportSSE {
					headers = buildCodexSSEHeaders(modelHeaders, optionHeaders, "account", "key", session)
					require.Equal(t, "text/event-stream", headers.Get("Accept"))
					require.Equal(t, "application/json", headers.Get("Content-Type"))
					require.Equal(t, codexSSEBeta, headers.Get("OpenAI-Beta"))
				} else {
					headers = buildCodexWebSocketHeaders(modelHeaders, optionHeaders, "account", "key", session)
					require.Empty(t, headers.Values("Accept"))
					require.Empty(t, headers.Values("Content-Type"))
					require.Equal(t, codexWebsocketBeta, headers.Get("OpenAI-Beta"))
				}
				require.Equal(t, "caller-underscore", headers.Get("session_id"))
				switch {
				case session != "":
					require.Equal(t, clampOpenAIPromptCacheKey(session), headers.Get("session-id"))
					require.Equal(t, headers.Get("session-id"), headers.Get("x-client-request-id"))
				case transport == TransportSSE:
					require.Equal(t, "caller-hyphen", headers.Get("session-id"))
					require.Equal(t, "caller-request", headers.Get("x-client-request-id"))
				default:
					_, err := uuid.Parse(headers.Get("session-id"))
					require.NoError(t, err)
					require.Equal(t, headers.Get("session-id"), headers.Get("x-client-request-id"))
				}
				require.Equal(t, "Bearer key", headers.Get("Authorization"))
				require.Equal(t, "account", headers.Get("chatgpt-account-id"))
				require.Equal(t, "model-underscore", modelHeaders["Session_id"])
				require.Equal(t, "caller-underscore", optionHeaders["session_id"])
			})
		}
	}
}
