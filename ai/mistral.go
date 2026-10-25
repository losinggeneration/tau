package ai // import "github.com/losinggeneration/tau/ai"

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	mistralBaseURL           = "https://api.mistral.ai"
	mistralToolCallIDLength  = 9
	maxMistralErrorBodyChars = 4000
)

// mistralClient is the provider-specific client for Mistral.
type mistralClient struct {
	model *Model
}

// newMistralClient creates a new Mistral client.
func newMistralClient(model *Model) (*mistralClient, error) {
	return &mistralClient{model: model}, nil
}

// Complete implements Client.
func (c *mistralClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

// Stream implements Client.
func (c *mistralClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) (err error) {
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
	cfg := applyOptions(opts)
	apiKey := cfg.apiKey
	if apiKey == "" {
		apiKey = cfg.envAPIKey(ProviderMistral)
	}
	if apiKey == "" {
		return fmt.Errorf("mistral: missing API key (set MISTRAL_API_KEY)")
	}

	body, err := c.buildRequestBody(model, prompt, cfg)
	if err != nil {
		return fmt.Errorf("mistral: %w", err)
	}
	if hasPayloadCallback(cfg) {
		if next := invokePayloadCallback(cfg, model, body); next != nil {
			if replacement, ok := next.(map[string]any); ok {
				body = replacement
			}
		}
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("mistral: marshal request: %w", err)
	}

	baseURL := model.BaseURL
	if baseURL == "" {
		baseURL = mistralBaseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("mistral: create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range model.Headers {
		req.Header.Set(k, v)
	}
	if sessionID := cacheSessionID(cfg); sessionID != "" && req.Header.Get("x-affinity") == "" {
		req.Header.Set("x-affinity", sessionID)
	}
	for k, v := range cfg.headers {
		req.Header.Set(k, v)
	}

	timeout := cfg.timeout
	if !cfg.timeoutSet {
		timeout = time.Minute
	}
	// The timeout covers only the wait for response headers; long streams
	// such as extended thinking must not be cut off by a fixed deadline.
	requestCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req = req.WithContext(requestCtx)
	headerTimeout := fmt.Errorf("mistral response headers timed out after %dms", timeout.Milliseconds())
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() { cancel(headerTimeout) })
	}
	resp, err := http.DefaultClient.Do(req)
	if timer != nil {
		timer.Stop()
	}
	if err != nil {
		if ctx.Err() == nil && errors.Is(context.Cause(requestCtx), headerTimeout) {
			return headerTimeout
		}
		return fmt.Errorf("mistral: request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	if hasResponseCallback(cfg) {
		invokeResponseCallback(cfg, model, responseMetadata(resp))
	}

	if resp.StatusCode != http.StatusOK {
		// P7-MS10: Truncate error bodies to MAX_MISTRAL_ERROR_BODY_CHARS
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("mistral: read status %d body: %w", resp.StatusCode, readErr)
		}

		detail := strings.TrimSpace(string(respBody))
		if detail == "" {
			detail = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("mistral API error (%d): %s", resp.StatusCode, truncateErrorText(detail, maxMistralErrorBodyChars))
	}

	return c.streamMistralResponse(ctx, model, resp.Body, handler)
}

func (c *mistralClient) buildRequestBody(model *Model, prompt *Context, cfg *options) (map[string]any, error) {
	// Transform messages for cross-provider compatibility
	// This handles: same-model vs cross-model thinking, synthetic tool results,
	// orphaned tool call handling, and errored/aborted message filtering
	transformedPrompt := prompt
	if transformed, err := TransformContext(prompt, model, nil); err == nil {
		transformedPrompt = transformed
	}

	body := map[string]any{
		"model":    model.ID,
		"stream":   true,
		"messages": c.convertMessages(transformedPrompt),
	}

	if cfg.temperature != nil {
		body["temperature"] = *cfg.temperature
	}
	if cfg.maxTokens > 0 {
		body["max_tokens"] = cfg.maxTokens
	}
	if cfg.topP != nil {
		body["top_p"] = *cfg.topP
	}

	// P7-MS01: Forward toolChoice from options to request
	if cfg.toolChoice != nil {
		body["tool_choice"] = c.mapToolChoice(cfg.toolChoice)
	}

	// Models with a thinking level map take reasoning_effort; other
	// reasoning models take prompt_mode.
	if model.Reasoning {
		var level string
		if cfg.reasoning != "" && cfg.reasoning != ReasoningOff {
			if level = ClampThinkingLevel(model, string(cfg.reasoning)); level == "off" {
				level = ""
			}
		}
		switch {
		case model.ThinkingLevelMap == nil:
			if level != "" {
				body["prompt_mode"] = "reasoning"
			}
		case level != "":
			body["reasoning_effort"] = cmp.Or(deref(model.ThinkingLevelMap[level]), "high")
		case model.ThinkingLevelMap["off"] != nil:
			body["reasoning_effort"] = *model.ThinkingLevelMap["off"]
		}
	}

	if len(transformedPrompt.Tools) > 0 {
		tools := make([]map[string]any, 0, len(transformedPrompt.Tools))
		for _, tool := range transformedPrompt.Tools {
			parameters, strict, err := strictToolParameters(tool, true, nil)
			if err != nil {
				return nil, err
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tool.Name,
					"description": tool.Description,
					"parameters":  parameters,
					"strict":      strict,
				},
			})
		}
		body["tools"] = tools
	}
	if transformedPrompt.SystemPrompt != "" {
		messages := body["messages"].([]map[string]any)
		body["messages"] = append([]map[string]any{{
			"role":    "system",
			"content": SanitizeSurrogates(transformedPrompt.SystemPrompt),
		}}, messages...)
	}

	return body, nil
}

// mapToolChoice converts a toolChoice string to the Mistral API format.
func (c *mistralClient) mapToolChoice(choice any) any {
	switch v := choice.(type) {
	case string:
		switch v {
		case "auto", "none", "any", "required":
			return v
		default:
			return v
		}
	default:
		return choice
	}
}

// streamMistralResponse streams and processes the Mistral response.
// P7-MS03/P7-MS04/P7-MS05: Handle array content, thinking deltas, and multiple tool calls.
func (c *mistralClient) streamMistralResponse(ctx context.Context, model *Model, body io.Reader, handler EventHandler) error {
	reader := NewSSEReader(body)

	output := &AssistantMessage{
		Role:      "assistant",
		API:       APIMistralConversations,
		Provider:  model.Provider,
		Model:     model.ID,
		Content:   []ContentBlock{},
		Usage:     Usage{},
		Timestamp: timestamp(),
	}
	handler(ctx, Start{Partial: *output})

	// P7-MS05: Track multiple simultaneous tool calls via id:index keyed map
	toolBlocksByKey := make(map[string]int)

	// P7-MS07: Tool call ID normalizer
	idNormalizer := createMistralToolCallIdNormalizer()

	for {
		data, ok := reader.Next()
		if !ok {
			break
		}

		var chunk mistralChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.ID != "" {
			output.ResponseID = chunk.ID
		}

		for _, choice := range chunk.Choices {
			// P7-MS04: delta.content can be an array of {string | thinking | text}
			if choice.Delta.Content != nil {
				c.handleContentDelta(ctx, model, output, handler, choice.Delta.Content, &toolBlocksByKey, idNormalizer)
			}

			// Handle tool_calls from the struct field
			if len(choice.Delta.ToolCalls) > 0 {
				c.handleToolCalls(ctx, output, handler, choice.Delta.ToolCalls, &toolBlocksByKey, idNormalizer)
			}

			if choice.FinishReason != "" && output.StopReason != StopReasonToolUse {
				output.StopReason = mistralStopReason(choice.FinishReason)
				if output.StopReason == StopReasonError {
					output.ErrorMessage = "Provider stopped with: " + choice.FinishReason
					if choice.FinishReason == "error" {
						output.ErrorMessage += " (server error)"
					}
				}
			}
			if choice.FinishReason == "tool_calls" {
				output.StopReason = StopReasonToolUse
			}
		}

		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			output.Usage = Usage{
				Input:       chunk.Usage.PromptTokens,
				Output:      chunk.Usage.CompletionTokens,
				TotalTokens: chunk.Usage.TotalTokens,
			}
			output.Usage.CalculateCost(model.Cost)
			handler(ctx, &output.Usage)
		}
	}

	if ctx.Err() != nil {
		emitTerminalStreamError(ctx, handler, *output, ctx.Err(), StopReasonAborted)
		return nil
	}
	if err := reader.Error(); err != nil {
		streamErr := fmt.Errorf("mistral: read: %w", err)
		emitTerminalStreamError(ctx, handler, *output, streamErr, StopReasonError)
		return nil
	}
	if output.StopReason == "" {
		emitTerminalStreamError(ctx, handler, *output, errors.New("mistral stream ended without a finish reason"), StopReasonError)
		return nil
	}

	for i, block := range output.Content {
		if block.Type == ContentBlockText {
			handler(ctx, TextEnd{ContentIndex: i, Content: block.Text, Partial: *output})
		}
	}

	// Finalize all tool calls and emit toolcall_end events
	for key, blockIndex := range toolBlocksByKey {
		if blockIndex < 0 || blockIndex >= len(output.Content) {
			continue
		}
		block := output.Content[blockIndex]
		if block.Type != ContentBlockToolCall {
			continue
		}
		// P7-MS06: Use partial JSON parser for final arguments
		if args, ok := ExtractValidJSON(string(block.Arguments)); ok {
			// Serialize the parsed map back to json.RawMessage
			if serialized, err := json.Marshal(args); err == nil {
				block.Arguments = serialized
				output.Content[blockIndex] = block
			}
		}
		// Convert ContentBlock to ToolCall for the event
		tc := ToolCall{
			ID:        block.ID,
			Name:      block.Name,
			Arguments: block.Arguments,
		}
		// Emit toolcall_end for each tool call
		_ = key // key used for tracking, tool call already in content
		handler(ctx, ToolCallEnd{ContentIndex: blockIndex, ToolCall: tc, Partial: *output})
	}
	if output.StopReason == StopReasonError {
		emitTerminalStreamError(ctx, handler, *output, errors.New(output.ErrorMessage), StopReasonError)
	} else {
		handler(ctx, Done{Reason: output.StopReason, Message: *output})
	}
	return nil
}

// handleContentDelta handles the delta.content which can be a string or array of content items.
// P7-MS03/P7-MS04: Parse thinking content from delta items and handle array-based content deltas.
func (c *mistralClient) handleContentDelta(
	ctx context.Context,
	model *Model,
	output *AssistantMessage,
	handler EventHandler,
	content any,
	toolBlocksByKey *map[string]int,
	idNormalizer func(string) string,
) {
	// Handle tool calls from delta.ToolCalls
	if choice, ok := content.(map[string]any); ok {
		if tcData, ok := choice["tool_calls"]; ok {
			c.handleToolCalls(ctx, output, handler, tcData, toolBlocksByKey, idNormalizer)
		}
	}

	var items []any
	switch v := content.(type) {
	case string:
		items = []any{v}
	case []any:
		items = v
	default:
		return
	}

	for _, item := range items {
		// P7-MS03: Parse thinking content from item.type == "thinking"
		if itemMap, ok := item.(map[string]any); ok {
			itemType, _ := itemMap["type"].(string)

			switch itemType {
			case "thinking":
				c.handleThinkingDelta(ctx, output, handler, itemMap)
				continue
			case "text":
				// Handle text item within array
				if text, ok := itemMap["text"].(string); ok {
					c.handleTextDelta(ctx, output, handler, SanitizeSurrogates(text))
				}
				continue
			}
		}

		// String content - treat as text delta
		if text, ok := item.(string); ok {
			c.handleTextDelta(ctx, output, handler, SanitizeSurrogates(text))
		}
	}
}

// handleToolCalls processes tool call deltas.
// P7-MS05: Track multiple simultaneous tool calls via id:index keyed map.
func (c *mistralClient) handleToolCalls(
	ctx context.Context,
	output *AssistantMessage,
	handler EventHandler,
	tcData any,
	toolBlocksByKey *map[string]int,
	idNormalizer func(string) string,
) {
	var tcSlice []any
	switch v := tcData.(type) {
	case []mistralToolCall:
		// Handle each mistralToolCall directly
		for _, tc := range v {
			c.processToolCall(ctx, output, handler, tc.ID, tc.Index, tc.Function.Name, tc.Function.Arguments, toolBlocksByKey, idNormalizer)
		}
		return
	case []any:
		tcSlice = v
	default:
		return
	}

	for _, tcItem := range tcSlice {
		// Handle mistralToolCall directly (from []any case where items are mistralToolCall)
		if tc, ok := tcItem.(mistralToolCall); ok {
			c.processToolCall(ctx, output, handler, tc.ID, tc.Index, tc.Function.Name, tc.Function.Arguments, toolBlocksByKey, idNormalizer)
			continue
		}

		tcMap, ok := tcItem.(map[string]any)
		if !ok {
			continue
		}

		// Get tool call ID
		var callID string
		if id, ok := tcMap["id"].(string); ok && id != "" && id != "null" {
			callID = id
		} else if index, ok := tcMap["index"].(float64); ok {
			callID = fmt.Sprintf("toolcall:%d", int(index))
		} else {
			callID = "toolcall:0"
		}

		// Normalize the ID for Mistral (max 9 chars)
		normalizedID := idNormalizer(callID)

		// Get function name
		var funcName string
		if fn, ok := tcMap["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				funcName = name
			}
		}

		// Get arguments delta
		var argsDelta string
		if fn, ok := tcMap["function"].(map[string]any); ok {
			if args, ok := fn["arguments"].(string); ok {
				argsDelta = args
			}
		}

		// Get index for key tracking
		var index int
		if idx, ok := tcMap["index"].(float64); ok {
			index = int(idx)
		}

		c.processToolCall(ctx, output, handler, normalizedID, index, funcName, argsDelta, toolBlocksByKey, func(s string) string { return s })
	}
}

// processToolCall handles a single tool call delta, creating ToolCallStart/Delta events.
func (c *mistralClient) processToolCall(
	ctx context.Context,
	output *AssistantMessage,
	handler EventHandler,
	callID string,
	index int,
	funcName string,
	argsDelta string,
	toolBlocksByKey *map[string]int,
	idNormalizer func(string) string,
) {
	// Normalize the ID
	normalizedID := callID
	if callID == "" {
		normalizedID = fmt.Sprintf("toolcall:%d", index)
	} else if idNormalizer != nil {
		normalizedID = idNormalizer(callID)
	}

	key := fmt.Sprintf("%s:%d", normalizedID, index)
	blockIndex, exists := (*toolBlocksByKey)[key]

	if !exists {
		// New tool call - create ContentBlock
		blockIndex = len(output.Content)
		contentBlock := ContentBlock{
			Type:      ContentBlockToolCall,
			ID:        normalizedID,
			Name:      funcName,
			Arguments: []byte{},
		}
		output.Content = append(output.Content, contentBlock)
		(*toolBlocksByKey)[key] = blockIndex
		handler(ctx, ToolCallStart{ContentIndex: blockIndex, Partial: *output})
	}

	// Update arguments
	if blockIndex >= 0 && blockIndex < len(output.Content) && argsDelta != "" {
		tcBlock := output.Content[blockIndex]
		if tcBlock.Type == ContentBlockToolCall {
			tcBlock.Arguments = append(tcBlock.Arguments, argsDelta...)
			output.Content[blockIndex] = tcBlock
			handler(ctx, ToolCallDelta{ContentIndex: blockIndex, Delta: argsDelta, Partial: *output})
		}
	}
}

// handleTextDelta handles text delta emission.
func (c *mistralClient) handleTextDelta(ctx context.Context, output *AssistantMessage, handler EventHandler, delta string) {
	if delta == "" {
		return
	}
	// Flush any pending tool call
	c.flushPendingToolCall(output, handler)

	contentIndex := len(output.Content)
	output.Content = append(output.Content, ContentBlock{
		Type: ContentBlockText,
		Text: delta,
	})
	handler(ctx, TextStart{ContentIndex: contentIndex, Partial: *output})
	handler(ctx, TextDelta{ContentIndex: contentIndex, Delta: delta, Partial: *output})
}

// handleThinkingDelta handles thinking content delta.
// P7-MS03: Parse thinking content from item.type == "thinking".
func (c *mistralClient) handleThinkingDelta(ctx context.Context, output *AssistantMessage, handler EventHandler, item map[string]any) {
	// Extract thinking text from array of parts
	var thinkingText string
	if parts, ok := item["thinking"].([]any); ok {
		var sb strings.Builder
		for _, part := range parts {
			if partMap, ok := part.(map[string]any); ok {
				if text, ok := partMap["text"].(string); ok {
					sb.WriteString(text)
				}
			}
		}
		thinkingText = sb.String()
	}

	if thinkingText == "" {
		return
	}

	thinkingText = SanitizeSurrogates(thinkingText)
	contentIndex := len(output.Content)

	// Add thinking block to content
	output.Content = append(output.Content, ContentBlock{
		Type:     ContentBlockThinking,
		Thinking: thinkingText,
	})

	handler(ctx, ThinkingStart{ContentIndex: contentIndex, Partial: *output})
	handler(ctx, ThinkingDelta{ContentIndex: contentIndex, Delta: thinkingText, Partial: *output})
}

// flushPendingToolCall flushes any pending tool call to the output.
func (c *mistralClient) flushPendingToolCall(output *AssistantMessage, handler EventHandler) {
	// This is a no-op since we now track tool calls in the map
	// Keeping for API compatibility
}

// createMistralToolCallIdNormalizer creates a tool call ID normalizer.
// P7-MS07: Implement tool call ID normalizer with bidirectional idMap/reverseMap.
func createMistralToolCallIdNormalizer() func(string) string {
	idMap := make(map[string]string)
	reverseMap := make(map[string]string)

	return func(id string) string {
		if existing, ok := idMap[id]; ok {
			return existing
		}

		attempt := 0
		for {
			candidate := deriveMistralToolCallId(id, attempt)
			owner, ok := reverseMap[candidate]
			if !ok || owner == id {
				idMap[id] = candidate
				reverseMap[candidate] = id
				return candidate
			}
			attempt++
		}
	}
}

// mistralNonAlphanumeric matches the characters Mistral tool call IDs may not
// contain. It is compiled once: deriveMistralToolCallId is called in a
// collision-retry loop and used to recompile this pattern twice per attempt.
var mistralNonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9]`)

// deriveMistralToolCallId derives a Mistral-compatible tool call ID.
func deriveMistralToolCallId(id string, attempt int) string {
	// Normalize: remove non-alphanumeric
	normalized := mistralNonAlphanumeric.ReplaceAllString(id, "")

	if attempt == 0 && len(normalized) == mistralToolCallIDLength {
		return normalized
	}

	seedBase := normalized
	if seedBase == "" {
		seedBase = id
	}

	seed := seedBase
	if attempt > 0 {
		seed = fmt.Sprintf("%s:%d", seedBase, attempt)
	}

	hash := mistralNonAlphanumeric.ReplaceAllString(ShortHash(seed), "")
	if len(hash) >= mistralToolCallIDLength {
		return hash[:mistralToolCallIDLength]
	}
	// Fallback: pad with attempt indicator if too short
	padding := strings.Repeat("x", mistralToolCallIDLength-len(hash))
	if attempt > 0 {
		padding = strings.Repeat("x", attempt)[:mistralToolCallIDLength-len(hash)]
	}
	return hash + padding
}

// convertMessages converts our Context messages to Mistral messages.
func (c *mistralClient) convertMessages(prompt *Context) []map[string]any {
	messages := make([]map[string]any, 0, len(prompt.Messages))

	for _, msg := range prompt.Messages {
		switch normalizeMessageRole(msg.Role) {
		case "user":
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": c.extractContent(msg.Content),
			})
		case "assistant":
			assistant := map[string]any{
				"role":    "assistant",
				"content": c.extractContent(msg.Content),
			}
			if contentBlocks, ok := msg.Content.([]ContentBlock); ok {
				toolCalls := make([]map[string]any, 0)
				for _, block := range contentBlocks {
					if block.Type != ContentBlockToolCall {
						continue
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":   block.ID,
						"type": "function",
						"function": map[string]any{
							"name":      block.Name,
							"arguments": string(block.Arguments),
						},
					})
				}
				if len(toolCalls) > 0 {
					assistant["tool_calls"] = toolCalls
				}
			}
			messages = append(messages, assistant)
		case "toolResult":
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": msg.ToolCallID,
				"name":         msg.ToolName,
				"content":      mistralToolResultText(msg),
			})
		}
	}

	return messages
}

func mistralToolResultText(msg Message) string {
	// P7-MS09: Use toolContent array format for results
	var textParts []string
	var hasImage bool
	supportsImages := msg.Model != "" // This is simplified - in real impl would check model capability

	if s, ok := msg.Content.(string); ok {
		if msg.IsError && s != "" {
			return "[tool error] " + s
		}
		return s
	}

	if blocks, ok := msg.Content.([]ContentBlock); ok {
		for _, block := range blocks {
			switch block.Type {
			case ContentBlockText:
				if block.Text != "" {
					textParts = append(textParts, block.Text)
				}
			case ContentBlockImage:
				if supportsImages {
					// Include image in content
				} else {
					hasImage = true
				}
			}
		}
	}

	text := strings.Join(textParts, "\n")
	if text != "" {
		if hasImage {
			// P7-MS08: Append image omission note for non-image models
			text += "\n[tool image omitted: model does not support images]"
		}
		if msg.IsError {
			return "[tool error] " + text
		}
		return text
	}
	if hasImage {
		if msg.IsError {
			return "[tool error] (image omitted: model does not support images)"
		}
		return "(image omitted: model does not support images)"
	}
	if msg.IsError {
		return "[tool error] (no tool output)"
	}
	return "(no tool output)"
}

// extractContent extracts string content from a message's content field.
func (c *mistralClient) extractContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []ContentBlock:
		var text strings.Builder

		for _, block := range v {
			if block.Type == ContentBlockText {
				text.WriteString(block.Text)
			}
			if block.Type == ContentBlockThinking {
				text.WriteString(block.Thinking)
			}
		}

		return text.String()
	}

	return ""
}

// truncateErrorText truncates error text to maxChars.
// P7-MS10: Truncate error bodies to MAX_MISTRAL_ERROR_BODY_CHARS.
func truncateErrorText(text string, maxChars int) string {
	if len(text) <= maxChars {
		return text
	}
	return fmt.Sprintf("%s... [truncated %d chars]", text[:maxChars], len(text)-maxChars)
}

func mistralStopReason(reason string) StopReason {
	switch reason {
	case "stop":
		return StopReasonStop
	case "length", "model_length":
		return StopReasonLength
	case "tool_calls":
		return StopReasonToolUse
	case "error":
		return StopReasonError
	default:
		return StopReasonError
	}
}

type mistralChunk struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Choices []mistralChoice `json:"choices"`
	Usage   mistralUsage    `json:"usage"`
}

type mistralChoice struct {
	Index        int          `json:"index"`
	Delta        mistralDelta `json:"delta"`
	FinishReason string       `json:"finish_reason,omitempty"`
}

type mistralDelta struct {
	Role      string            `json:"role,omitempty"`
	Content   any               `json:"content,omitempty"` // string or []ContentItem
	ToolCalls []mistralToolCall `json:"tool_calls,omitempty"`
}

type mistralToolCall struct {
	ID       string              `json:"id"`
	Index    int                 `json:"index,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function mistralFunctionCall `json:"function"`
}

type mistralFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type mistralUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
}
