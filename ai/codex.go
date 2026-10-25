package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	defaultCodexBaseURL = "https://chatgpt.com/backend-api"
	codexSSEBeta        = "responses=experimental"
	codexWebsocketBeta  = "responses_websockets=2026-02-06"
	codexJWTClaimPath   = "https://api.openai.com/auth"
)

var (
	codexMaxRetries     = 3
	codexBaseRetryDelay = time.Second
	codexRetryableError = regexp.MustCompile(`rate.?limit|overloaded|service.?unavailable|upstream.?connect|connection.?refused`)
)

// CodexUsageLimitError is returned when the OpenAI Codex API reports that
// the account's usage limit has been reached (error type
// "usage_limit_reached"). It preserves the structured details from the API
// response so callers can inspect when the quota resets.
type CodexUsageLimitError struct {
	// StatusCode is the HTTP status code of the response (typically 429).
	StatusCode int
	// Message is the provider-supplied error message.
	Message string
	// PlanType is the subscription plan associated with the account.
	PlanType string
	// ResetsAt is when the usage limit resets, in local time.
	ResetsAt time.Time
	// ResetsIn is the provider-reported duration until the usage limit resets.
	ResetsIn time.Duration
}

// Error implements error.
func (e *CodexUsageLimitError) Error() string {
	message := e.Message
	if message == "" {
		message = "The usage limit has been reached"
	}

	return fmt.Sprintf("openai codex: status %d: %s. Available again in %s at %s",
		e.StatusCode, message, e.ResetsIn, e.ResetsAt.Format(time.DateTime))
}

// codexErrorResponse mirrors the error envelope returned by the Codex API.
type codexErrorResponse struct {
	Error struct {
		Type            string `json:"type"`
		Message         string `json:"message"`
		PlanType        string `json:"plan_type"`
		ResetsAt        int64  `json:"resets_at"`
		ResetsInSeconds int64  `json:"resets_in_seconds"`
	} `json:"error"`
}

// parseCodexUsageLimitError returns a *CodexUsageLimitError when body is a
// "usage_limit_reached" error response, and nil otherwise.
func parseCodexUsageLimitError(statusCode int, body []byte) *CodexUsageLimitError {
	var parsed codexErrorResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	if parsed.Error.Type != "usage_limit_reached" {
		return nil
	}

	usageErr := &CodexUsageLimitError{
		StatusCode: statusCode,
		Message:    parsed.Error.Message,
		PlanType:   parsed.Error.PlanType,
		ResetsIn:   time.Duration(parsed.Error.ResetsInSeconds) * time.Second,
	}
	if parsed.Error.ResetsAt != 0 {
		usageErr.ResetsAt = time.Unix(parsed.Error.ResetsAt, 0).In(time.Local)
	}

	return usageErr
}

// CodexProvider handles OpenAI Codex API (GitHub Copilot).
type CodexProvider struct{}

// API implements Provider.
func (p *CodexProvider) API() string {
	return APIOpenAICodexResponses
}

// IsAlias implements Provider.
func (p *CodexProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *CodexProvider) NewClient(model *Model) (Client, error) {
	return &codexClient{model: model}, nil
}

// Capabilities implements CapableProvider.
func (p *CodexProvider) Capabilities() Capabilities {
	return CodexCapabilities
}

type codexClient struct {
	model *Model
}

// Complete implements Client.
func (c *codexClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

// Stream implements Client.Streamer.
func (c *codexClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) (err error) {
	resolvedOptions, resolveErr := resolveRequestOptions(ctx, model, opts)
	if resolveErr != nil {
		return resolveErr
	}
	opts = resolvedOptions
	handler = timedEventHandler(handler)
	if err := ValidateContextLimits(model, prompt); err != nil {
		return err
	}
	prompt = providerContext(prompt)
	var o options
	o.apply(opts)

	apiKey := o.apiKey
	if apiKey == "" {
		apiKey = o.envAPIKey(model.Provider)
		if apiKey == "" {
			return errors.New("openai codex: missing API key")
		}
	}

	accountID, err := extractCodexAccountID(apiKey)
	if err != nil {
		return fmt.Errorf("openai codex: extract account id: %w", err)
	}

	body, err := buildCodexRequestBody(model, prompt, &o)
	if err != nil {
		return fmt.Errorf("openai codex: %w", err)
	}
	if hasPayloadCallback(&o) {
		if next := invokePayloadCallback(&o, model, body); next != nil {
			if replacement, ok := next.(map[string]any); ok {
				body = replacement
			}
		}
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("openai codex: marshal request: %w", err)
	}

	grammarInputs, err := responsesGrammarInputs(model, prompt.Tools)
	if err != nil {
		return err
	}
	streamOpts := &ResponsesStreamOptions{
		ServiceTier:             o.serviceTier,
		ApplyServiceTierPricing: ServiceTierPricing,
		GrammarInputs:           grammarInputs,
		ResolveServiceTier:      ResolveCodexServiceTier,
	}

	transport := o.preferredTransport
	if transport == "" {
		transport = TransportSSE
	}

	if transport != TransportSSE {
		websocketURL, wsURLErr := resolveCodexWebSocketURL(model.BaseURL)
		if wsURLErr == nil {
			timeout := o.timeout
			if !o.timeoutSet {
				timeout = 15 * time.Second
			}
			payload, started, wsErr := streamCodexWebSocket(
				ctx,
				websocketURL,
				buildCodexWebSocketHeaders(model.Headers, o.headers, accountID, apiKey, cacheSessionID(&o)),
				body,
				timeout,
			)
			if wsErr == nil {
				return streamCodexResponse(ctx, model, strings.NewReader(payload), handler, streamOpts)
			}
			if started || transport == TransportWS || ctx.Err() != nil {
				reason := StopReasonError
				if ctx.Err() != nil {
					reason = StopReasonAborted
				}
				emitTerminalStreamError(ctx, handler, AssistantMessage{
					Role:      "assistant",
					API:       APIOpenAICodexResponses,
					Provider:  model.Provider,
					Model:     model.ID,
					Timestamp: timestamp(),
				}, wsErr, reason)
				return nil
			}
		} else if transport == TransportWS {
			return fmt.Errorf("openai codex: websocket url: %w", wsURLErr)
		}
	}

	resp, err := doCodexSSERequest(ctx, model, bodyBytes, apiKey, accountID, &o)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("openai codex: close response body: %w", closeErr)
		}
	}()

	return streamCodexResponse(ctx, model, resp.Body, handler, streamOpts)
}

func doCodexSSERequest(ctx context.Context, model *Model, body []byte, apiKey, accountID string, o *options) (resp *http.Response, err error) {
	url := resolveCodexURL(model.BaseURL)
	headers := buildCodexSSEHeaders(model.Headers, o.headers, accountID, apiKey, cacheSessionID(o))
	bodyText := string(body)

	retries := codexMaxRetries
	if o.maxRetries != nil {
		retries = *o.maxRetries
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("openai codex: request: %w", err)
		}
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(bodyText))
		if reqErr != nil {
			return nil, fmt.Errorf("openai codex: create request: %w", reqErr)
		}
		req.Header = headers.Clone()

		resp, err = doHeaderRequest(req, o.timeout, "openai codex")
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("openai codex: request: %w", ctx.Err())
			}
			if attempt < retries {
				if sleepErr := sleepWithContext(ctx, codexRetryDelay(attempt)); sleepErr == nil {
					continue
				} else {
					return nil, fmt.Errorf("openai codex: request: %w", sleepErr)
				}
			}
			return nil, fmt.Errorf("openai codex: request: %w", err)
		}

		if hasResponseCallback(o) {
			invokeResponseCallback(o, model, responseMetadata(resp))
		}

		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		respBody, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			err := fmt.Errorf("read error response body: %w", readErr)
			if closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close error response body: %w", closeErr))
			}
			return nil, fmt.Errorf("openai codex: status %d: %w", resp.StatusCode, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("openai codex: close error response body: %w", closeErr)
		}

		errorText := string(respBody)
		if usageErr := parseCodexUsageLimitError(resp.StatusCode, respBody); usageErr != nil {
			return nil, usageErr
		}
		if attempt < retries && isRetryableCodexError(resp.StatusCode, errorText) {
			delay := codexRetryDelay(attempt)
			if serverDelay, ok := retryAfterDelay(resp.Header); ok {
				maxDelay := o.maxRetryDelayMs
				if !o.maxRetryDelaySet {
					maxDelay = 60000
				}
				if maxDelay > 0 && float64(serverDelay)/float64(time.Millisecond) > float64(maxDelay) {
					return nil, fmt.Errorf("openai codex: server requested %s retry delay (max: %dms)", serverDelay, maxDelay)
				}
				delay = serverDelay
			}
			if sleepErr := sleepWithContext(ctx, delay); sleepErr == nil {
				continue
			} else {
				return nil, fmt.Errorf("openai codex: request: %w", sleepErr)
			}
		}

		return nil, fmt.Errorf("openai codex: status %d: %s", resp.StatusCode, errorText)
	}
}

func buildBaseCodexHeaders(modelHeaders, optionHeaders map[string]string, accountID, apiKey string) http.Header {
	headers := make(http.Header)
	headers.Set("originator", "pi")
	headers.Set("User-Agent", fmt.Sprintf("tau (%s %s)", runtime.GOOS, runtime.GOARCH))
	for k, v := range modelHeaders {
		headers.Set(k, v)
	}
	for k, v := range optionHeaders {
		headers.Set(k, v)
	}

	headers.Set("Authorization", "Bearer "+apiKey)
	headers.Set("chatgpt-account-id", accountID)

	return headers
}

func buildCodexSSEHeaders(modelHeaders, optionHeaders map[string]string, accountID, apiKey, sessionID string) http.Header {
	headers := buildBaseCodexHeaders(modelHeaders, optionHeaders, accountID, apiKey)
	headers.Set("OpenAI-Beta", codexSSEBeta)
	headers.Set("Accept", "text/event-stream")
	headers.Set("Content-Type", "application/json")
	if sessionID != "" {
		sessionID = clampOpenAIPromptCacheKey(sessionID)
		headers.Set("session-id", sessionID)
		headers.Set("x-client-request-id", sessionID)
	}
	return headers
}

func buildCodexWebSocketHeaders(modelHeaders, optionHeaders map[string]string, accountID, apiKey, requestID string) http.Header {
	requestID = clampOpenAIPromptCacheKey(requestID)
	if requestID == "" {
		requestID = createCodexRequestID()
	}
	headers := buildBaseCodexHeaders(modelHeaders, optionHeaders, accountID, apiKey)
	headers.Set("OpenAI-Beta", codexWebsocketBeta)
	headers.Set("x-client-request-id", requestID)
	headers.Set("session-id", requestID)
	headers.Del("Accept")
	headers.Del("Content-Type")
	return headers
}

func createCodexRequestID() string {
	return uuid.NewString()
}

func resolveCodexURL(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		raw = defaultCodexBaseURL
	}
	normalized := strings.TrimRight(raw, "/")
	switch {
	case strings.HasSuffix(normalized, "/codex/responses"):
		return normalized
	case strings.HasSuffix(normalized, "/codex"):
		return normalized + "/responses"
	default:
		return normalized + "/codex/responses"
	}
}

func resolveCodexWebSocketURL(baseURL string) (string, error) {
	parsed, err := url.Parse(resolveCodexURL(baseURL))
	if err != nil {
		return "", err
	}

	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	case "wss", "ws":
	default:
		return "", fmt.Errorf("unsupported codex websocket scheme %q", parsed.Scheme)
	}

	return parsed.String(), nil
}

func streamCodexWebSocket(ctx context.Context, websocketURL string, headers http.Header, body map[string]any, timeout time.Duration) (payload string, started bool, err error) {
	requestCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() {
			cancel(fmt.Errorf("openai codex: websocket response headers timed out: %w", context.DeadlineExceeded))
		})
		defer timer.Stop()
	}
	var cleanup func() error
	defer func() {
		if cleanup != nil {
			if closeErr := cleanup(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				err = errors.Join(err, fmt.Errorf("openai codex: websocket close: %w", closeErr))
			}
		}
		if cause := context.Cause(requestCtx); cause != nil {
			err = errors.Join(err, cause)
		}
	}()
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		closed := make(chan error, 1)
		stop := context.AfterFunc(requestCtx, func() { closed <- conn.Close() })
		cleanup = func() error {
			if stop() {
				return conn.Close()
			}
			return <-closed
		}
		return conn, nil
	}}
	conn, resp, err := dialer.DialContext(requestCtx, websocketURL, headers)
	if timer != nil {
		timer.Stop()
	}
	if resp != nil && resp.Body != nil {
		defer func() {
			if closeErr := resp.Body.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("openai codex: close websocket handshake body: %w", closeErr))
			}
		}()
	}
	if err != nil {
		return "", false, fmt.Errorf("openai codex: websocket connect: %w", err)
	}

	request := map[string]any{"type": "response.create"}
	for k, v := range body {
		request[k] = v
	}
	if writeErr := conn.WriteJSON(request); writeErr != nil {
		return "", false, fmt.Errorf("openai codex: websocket write: %w", writeErr)
	}
	started = true

	var builder strings.Builder
	sawCompletion := false

	for {
		if ctx.Err() != nil {
			return builder.String(), started, ctx.Err()
		}

		_, message, readErr := conn.ReadMessage()
		if readErr != nil {
			if sawCompletion && websocket.IsCloseError(readErr, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				break
			}
			return builder.String(), started, fmt.Errorf("openai codex: websocket read: %w", readErr)
		}

		trimmed := strings.TrimSpace(string(message))
		if trimmed == "" {
			continue
		}

		builder.WriteString("data: ")
		builder.WriteString(trimmed)
		builder.WriteString("\n")

		var event struct {
			Type string `json:"type"`
		}
		if jsonErr := json.Unmarshal(message, &event); jsonErr == nil {
			switch event.Type {
			case "response.completed", "response.done", "response.incomplete", "response.failed", "error":
				sawCompletion = true
			}
		}

		if sawCompletion {
			builder.WriteString("\n")
			return builder.String(), started, nil
		}
	}

	return builder.String(), started, nil
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func codexRetryDelay(attempt int) time.Duration {
	return codexBaseRetryDelay * time.Duration(1<<min(attempt, 4))
}

func isRetryableCodexError(statusCode int, body string) bool {
	switch statusCode {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return codexRetryableError.MatchString(strings.ToLower(body))
	}
}

func extractCodexAccountID(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("invalid JWT token")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return "", fmt.Errorf("unmarshal JWT payload: %w", err)
	}

	claim, ok := payload[codexJWTClaimPath].(map[string]any)
	if !ok {
		return "", errors.New("missing auth claim")
	}

	accountID, ok := claim["chatgpt_account_id"].(string)
	if !ok || accountID == "" {
		return "", errors.New("missing chatgpt_account_id claim")
	}

	return accountID, nil
}

// buildCodexRequestBody builds the Codex API request body.
func buildCodexRequestBody(model *Model, aiCtx *Context, o *options) (map[string]any, error) {
	transformedCtx := aiCtx
	if transformed, err := TransformContext(aiCtx, model, openAIResponsesToolCallIDNormalizer(model)); err == nil {
		transformedCtx = transformed
	}

	grammar, err := responsesGrammarInputs(model, aiCtx.Tools)
	if err != nil {
		return nil, err
	}
	input := make([]map[string]any, 0, len(transformedCtx.Messages)+1)
	for i, msg := range transformedCtx.Messages {
		items, err := convertMessageForOpenAIResponses(model, msg, i, grammar)
		if err != nil {
			return nil, err
		}
		input = append(input, items...)
	}

	body := map[string]any{
		"model":               model.ID,
		"store":               false,
		"stream":              true,
		"input":               input,
		"text":                map[string]any{"verbosity": defaultString(o.textVerbosity, "medium")},
		"include":             []string{"reasoning.encrypted_content"},
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
	}

	if transformedCtx.SystemPrompt != "" {
		body["instructions"] = transformedCtx.SystemPrompt
	}
	if o.temperature != nil {
		body["temperature"] = *o.temperature
	}
	if o.topP != nil {
		body["top_p"] = *o.topP
	}
	if sessionID := cacheSessionID(o); sessionID != "" {
		body["prompt_cache_key"] = clampOpenAIPromptCacheKey(sessionID)
	}
	if len(o.metadata) > 0 {
		body["metadata"] = o.metadata
	}
	if o.serviceTier != "" {
		body["service_tier"] = o.serviceTier
	}
	if len(aiCtx.Tools) > 0 {
		tools, err := responsesTools(model, aiCtx.Tools, supportsStrictMode(model, true))
		if err != nil {
			return nil, err
		}
		body["tools"] = tools
	}
	switch effort := responsesReasoningEffort(model, o); {
	case effort == "none":
		if off, ok := responsesOffEffort(model); ok {
			body["reasoning"] = map[string]any{"effort": off, "summary": defaultString(o.reasoningSummary, "auto")}
		}
	case effort != "" || o.reasoningSummary != "":
		body["reasoning"] = map[string]any{
			"effort":  clampCodexReasoningEffort(model.ID, defaultString(effort, "medium")),
			"summary": defaultString(o.reasoningSummary, "auto"),
		}
	case model.Reasoning:
		if off, ok := responsesOffEffort(model); ok {
			body["reasoning"] = map[string]any{"effort": off}
		}
	}

	return body, nil
}

func clampCodexReasoningEffort(modelID, effort string) string {
	id := modelID
	if strings.Contains(id, "/") {
		parts := strings.Split(id, "/")
		id = parts[len(parts)-1]
	}

	switch {
	case (strings.HasPrefix(id, "gpt-5.2") || strings.HasPrefix(id, "gpt-5.3") || strings.HasPrefix(id, "gpt-5.4")) && effort == "minimal":
		return "low"
	case id == "gpt-5.1" && effort == "xhigh":
		return "high"
	case id == "gpt-5.1-codex-mini":
		switch effort {
		case "high", "xhigh":
			return "high"
		default:
			return "medium"
		}
	default:
		return effort
	}
}

// convertMessageForCodex converts a Message to Codex format.
func convertMessageForCodex(msg Message) map[string]any {
	role := normalizeMessageRole(msg.Role)
	if role == "toolResult" {
		role = "tool"
	}

	var content any

	switch c := msg.Content.(type) {
	case string:
		content = c
	case []ContentBlock:
		if normalizeMessageRole(msg.Role) == "toolResult" {
			content = []map[string]any{{
				"type":         "function_call_output",
				"tool_call_id": msg.ToolCallID,
				"output":       codexToolResultOutput(c),
			}}
			break
		}

		blocks := make([]map[string]any, 0, len(c))
		for _, block := range c {
			switch block.Type {
			case "text":
				blocks = append(blocks, map[string]any{
					"type": "input_text",
					"text": block.Text,
				})
			case "toolCall":
				var args any
				if block.Arguments != nil {
					if err := json.Unmarshal(block.Arguments, &args); err != nil {
						args = string(block.Arguments)
					}
				}

				blocks = append(blocks, map[string]any{
					"type":      "function_call",
					"id":        block.ID,
					"name":      block.Name,
					"arguments": args,
				})
			}
		}

		content = blocks
	}

	return map[string]any{
		"role":    role,
		"content": content,
	}
}

func codexToolResultOutput(blocks []ContentBlock) any {
	if len(blocks) == 1 && blocks[0].Type == ContentBlockText {
		return blocks[0].Text
	}

	items := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case ContentBlockText:
			items = append(items, map[string]any{
				"type": "text",
				"text": block.Text,
			})
		case ContentBlockImage:
			items = append(items, map[string]any{
				"type":      "image",
				"mime_type": block.MimeType,
				"data":      block.Data,
			})
		}
	}
	if len(items) == 0 {
		return ""
	}
	return items
}

// streamCodexResponse streams the SSE response using shared processing.
func streamCodexResponse(ctx context.Context, model *Model, body io.Reader, handler EventHandler, opts *ResponsesStreamOptions) error {
	payload, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("openai codex: read response: %w", err)
	}

	// Codex is the one Responses provider that must see the whole payload
	// before parsing: it rewrites service-tier fields across the SSE text.
	serviceTier := ""
	if opts != nil {
		serviceTier = opts.ServiceTier
	}

	normalized := normalizeCodexResponsesPayload(payload, serviceTier)
	reader := newResponsesStreamReader(strings.NewReader(normalized))

	output := &AssistantMessage{
		Role:       "assistant",
		API:        APIOpenAICodexResponses,
		Provider:   model.Provider,
		Model:      model.ID,
		Content:    []ContentBlock{},
		StopReason: StopReasonStop,
		Usage:      Usage{},
		Timestamp:  timestamp(),
	}

	return ProcessResponsesStream(ctx, reader, output, handler, model, opts)
}

func normalizeCodexResponsesPayload(payload []byte, requestedServiceTier string) string {
	lines := strings.Split(string(payload), "\n")
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, "data: ") {
			lines[i] = line
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "" || data == "[DONE]" {
			lines[i] = line
			continue
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			lines[i] = line
			continue
		}

		typeName, _ := event["type"].(string)
		switch typeName {
		case "response.done", "response.completed", "response.incomplete":
			event["type"] = "response.completed"
			resp, _ := event["response"].(map[string]any)
			if resp == nil {
				resp = map[string]any{}
				event["response"] = resp
			}
			status, _ := resp["status"].(string)
			if status == "" {
				if typeName == "response.incomplete" {
					resp["status"] = "incomplete"
				} else {
					resp["status"] = "completed"
				}
			}
			serviceTier, _ := resp["service_tier"].(string)
			if serviceTier == "default" && (requestedServiceTier == "flex" || requestedServiceTier == "priority") {
				resp["service_tier"] = requestedServiceTier
			}
		}

		normalized, err := json.Marshal(event)
		if err != nil {
			lines[i] = line
			continue
		}
		lines[i] = "data: " + string(normalized)
	}

	return strings.Join(lines, "\n")
}
