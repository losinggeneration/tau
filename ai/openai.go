package ai // import "github.com/losinggeneration/tau/ai"

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// DefaultBaseURLs maps providers to their default API endpoints.
var DefaultBaseURLs = map[string]string{
	ProviderOpenAI:     "https://api.openai.com/v1",
	ProviderGroq:       "https://api.groq.com/openai/v1",
	ProviderCerebras:   "https://api.cerebras.ai/v1",
	ProviderxAI:        "https://api.x.ai/v1",
	ProviderOpenRouter: "https://openrouter.ai/api/v1",
	ProviderOllama:     "http://localhost:11434/v1",
	ProviderLMStudio:   "http://localhost:1234/v1",
	ProviderVLLM:       "http://localhost:8000/v1",
	// Additional providers matching pi parity
	ProviderDeepSeek:            "https://api.deepseek.com/v1",
	ProviderFireworks:           "https://api.fireworks.ai/v1",
	ProviderMoonshot:            "https://api.moonshot.cn/v1",
	ProviderMoonshotAI:          "https://api.moonshot.cn/v1",
	ProviderMoonshotAICN:        "https://api.moonshot.cn/v1",
	ProviderKimiCoding:          "https://api.moonshot.cn/v1", // Kimi uses Moonshot API
	ProviderOpenCode:            "https://api.opencode.ai/v1",
	ProviderOpenCodeGo:          "https://opencode.ai/zen/go/v1",
	ProviderZAI:                 "https://api.z.ai/v1",
	ProviderMiniMax:             "https://api.minimax.chat/v1",
	ProviderCloudflare:          "https://api.cloudflare.com/client/v4/workers/ai",
	ProviderCloudflareWorkersAI: "https://api.cloudflare.com/client/v4/workers/ai",
	ProviderCloudflareAIGateway: "https://gateway.ai.cloudflare.com/v1",
	ProviderXiaomi:              "https://api.mimi.cn/v1",
	ProviderHuggingFace:         "https://api-inference.huggingface.co/v1",
	ProviderNVIDIA:              "https://integrate.api.nvidia.com/v1",
	ProviderTogether:            "https://api.together.xyz/v1",
	ProviderAntLing:             "https://api.ant-ling.com/v1",
	ProviderZAICodingCN:         "https://api.zai-coding.cn/v1",
	ProviderXiaomiTokenPlanCN:   "https://api.mimi.cn/v1",
	ProviderXiaomiTokenPlanAMS:  "https://api.mimi.cn/v1",
	ProviderXiaomiTokenPlanSGP:  "https://api.mimi.cn/v1",
}

type openAIClient struct {
	model         *Model
	transport     *HTTPTransport
	transportOpts *TransportOption
}

// SetTransportOption implements the SDK's transport option injection interface.
// This allows the SDK to pass credential refresh callbacks for 401 retry support.
func (c *openAIClient) SetTransportOption(opts TransportOption) {
	c.transportOpts = &opts
}

// newOpenAIClient creates a new OpenAI-compatible client.
func newOpenAIClient(model *Model) (*openAIClient, error) {
	baseURL := model.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURLs[model.Provider]
		if baseURL == "" {
			return nil, fmt.Errorf("no base URL for provider: %s", model.Provider)
		}
	}

	// Note: API key is now passed via WithAPIKey option or from credential store
	// at call time. The transport is created without an API key.
	return &openAIClient{
		model: model,
		transport: NewTransport(baseURL, "", map[string][]string{
			"Content-Type": {"application/json"},
			"User-Agent":   {"tau-ai/0.1.0"},
		}),
	}, nil
}

// Complete implements Client.
func (c *openAIClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

// Stream implements Client.
func (c *openAIClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) (err error) {
	var o options
	o.apply(opts)

	// Build request
	reqBody, err := c.buildRequest(model, prompt, &o)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	if hasPayloadCallback(&o) {
		if next := invokePayloadCallback(&o, model, reqBody); next != nil {
			if s, ok := next.(string); ok {
				reqBody = s
			}
		}
	}

	resp, err := doOpenAIChatCompletions(ctx, model, &o, c.transport.withRequestOptions(model, &o), reqBody)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	// Handle SSE stream. streamOpenAIResponse accumulates text, thinking,
	// and tool-call deltas across chunks into a single AssistantMessage and
	// emits Start/Delta/End/Done events with populated Partial.Content.
	// (The earlier per-chunk handleChunk emitted Done with an empty AssistantMessage
	// on every chunk, so agent_loop's `partialMessage.Content = e.Partial.Content`
	// overwrote any accumulated text — reasoning-only models like opencode-go
	// kimi-k2.6 / glm-5.x therefore appeared to return empty content.)
	return streamOpenAIResponse(ctx, c.model, resp.Body, handler)
}

// hasToolHistory checks if conversation messages contain tool calls or tool results.
func hasToolHistory(messages []Message) bool {
	for _, msg := range messages {
		if normalizeMessageRole(msg.Role) == messageRoleToolResult {
			return true
		}
		if msg.Role != "assistant" {
			continue
		}
		for _, block := range msg.ContentBlocks() {
			if block.Type == ContentBlockToolCall {
				return true
			}
		}
	}
	return false
}

// normalizeToolCallId normalizes tool call IDs to match provider requirements.
func normalizeToolCallId(id string, provider string) string {
	// Handle pipe-separated IDs from OpenAI Responses API
	if strings.Contains(id, "|") {
		parts := strings.SplitN(id, "|", 2)
		id = parts[0]
	}
	// Sanitize to allowed chars and truncate
	id = strings.ReplaceAll(id, "|", "_")
	id = strings.ReplaceAll(id, "+", "_")
	id = strings.ReplaceAll(id, "/", "_")
	id = strings.ReplaceAll(id, "=", "_")

	if provider == ProviderOpenAI {
		if len(id) > 40 {
			id = id[:40]
		}
	} else if len(id) > 64 {
		id = id[:64]
	}
	return id
}

func (c *openAIClient) buildRequest(model *Model, prompt *Context, o *options) (string, error) {
	req, err := buildOpenAIChatCompletionsRequest(model, prompt, o)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func buildOpenAIChatCompletionsRequest(model *Model, prompt *Context, o *options) (*openAIRequest, error) {
	compat := model.GetCompat()

	// Transform messages for cross-provider compatibility. This handles same-model
	// vs cross-model thinking, synthetic tool results, orphaned tool call handling,
	// and errored/aborted message filtering.
	transformedCtx, err := TransformContext(prompt, model, nil)
	if err != nil {
		return nil, fmt.Errorf("transform messages: %w", err)
	}

	// Normalize tool call IDs on both sides of the pairing: the toolResult
	// ToolCallID and the assistant toolCall block ID. Normalizing only one
	// side breaks the pairing for foreign IDs (e.g. the Responses API's
	// composite "call_id|item_id" format recorded before a provider switch),
	// which providers reject with "tool_call_id ... is not found".
	for i := range transformedCtx.Messages {
		msg := &transformedCtx.Messages[i]
		if msg.ToolCallID != "" {
			msg.ToolCallID = normalizeToolCallId(msg.ToolCallID, model.Provider)
		}
		if msg.Role != "assistant" {
			continue
		}
		blocks, ok := msg.Content.([]ContentBlock)
		if !ok {
			continue
		}
		for j := range blocks {
			if blocks[j].Type == ContentBlockToolCall && blocks[j].ID != "" {
				blocks[j].ID = normalizeToolCallId(blocks[j].ID, model.Provider)
			}
		}
	}

	client := &openAIClient{model: model}
	msgs, err := client.convertMessages(transformedCtx, model, compat)
	if err != nil {
		return nil, err
	}
	maybeAddOpenRouterAnthropicCacheControlToMessages(model, msgs)

	req := &openAIRequest{
		Model:    model.ID,
		Messages: msgs,
		Stream:   true,
	}
	if compat.SupportsUsageInStreaming {
		req.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	// Preserve the historical OpenAI provider behavior: the first-party
	// completions client always asks for usage in stream chunks.
	if model.API == APIOpenAICompletions || model.Provider == ProviderOpenAI {
		req.StreamOptions = &streamOptions{IncludeUsage: true}
	}

	applyOpenAIRequestTools(req, transformedCtx, compat)
	applyOpenAIRequestOptions(req, model, o, compat)
	return req, nil
}

func applyOpenAIRequestTools(req *openAIRequest, prompt *Context, compat *OpenAICompletionsCompat) {
	if len(prompt.Tools) > 0 {
		req.Tools = make([]toolDefinition, len(prompt.Tools))
		for i, t := range prompt.Tools {
			def := toolDefinition{
				Type:     "function",
				Function: toolToFunctionDefinition(t),
			}
			if compat.SupportsStrictMode {
				def.Function.Strict = new(false)
			}
			req.Tools[i] = def
		}
		if compat.ZAIToolStream {
			req.ToolStream = true
		}
		return
	}

	if hasToolHistory(prompt.Messages) {
		req.Tools = []toolDefinition{}
	}
}

func applyOpenAIRequestOptions(req *openAIRequest, model *Model, o *options, compat *OpenAICompletionsCompat) {
	if o.toolChoice != nil {
		req.ToolChoice = o.toolChoice
	}

	applyOpenAIReasoningOptions(req, model, o, compat)
	applyOpenAIMaxTokens(req, model, o, compat)

	if compat.SupportsStore {
		store := cacheSessionID(o) != ""
		req.Store = &store
	}

	if hasOpenRouterRouting(compat.OpenRouterRouting) && (model.Provider == ProviderOpenRouter || strings.Contains(model.BaseURL, "openrouter.ai")) {
		req.Provider = compat.OpenRouterRouting
	}

	if strings.Contains(model.BaseURL, "ai-gateway.vercel.sh") && compat.VercelGatewayRouting != nil {
		req.ProviderOptions = map[string]any{"gateway": compat.VercelGatewayRouting}
	}

	if len(o.metadata) > 0 {
		req.Metadata = o.metadata
	}
}

func applyOpenAIReasoningOptions(req *openAIRequest, model *Model, o *options, compat *OpenAICompletionsCompat) {
	if model.Reasoning && o.reasoning != "" && o.reasoning != ReasoningOff {
		switch compat.ThinkingFormat {
		case ThinkingFormatZAI:
			req.EnableThinking = boolPtr(true)
			if compat.SupportsReasoningEffort {
				req.ReasoningEffort = mapReasoningEffort(o.reasoning, compat.ReasoningEffortMap)
			}
		case ThinkingFormatQwen:
			req.EnableThinking = boolPtr(true)
		case ThinkingFormatQwenChatTmpl:
			req.ChatTemplate_kwargs = map[string]bool{"enable_thinking": true, "preserve_thinking": true}
		case ThinkingFormatOpenRouter:
			req.Reasoning = &openAIRouterReasoning{Effort: mapReasoningEffort(o.reasoning, compat.ReasoningEffortMap)}
		case ThinkingFormatDeepSeek:
			req.Thinking = &openAIThinking{Type: "enabled"}
		default:
			if compat.SupportsReasoningEffort {
				req.ReasoningEffort = mapReasoningEffort(o.reasoning, compat.ReasoningEffortMap)
			}
		}
		return
	}

	if o.thinkingEnabled && o.reasoning == "" {
		switch compat.ThinkingFormat {
		case ThinkingFormatZAI:
			req.Thinking = &openAIThinking{Type: "enabled"}
		case ThinkingFormatQwen:
			req.EnableThinking = boolPtr(true)
		case ThinkingFormatQwenChatTmpl:
			req.ChatTemplate_kwargs = map[string]bool{"enable_thinking": true}
		}
		return
	}

	if !model.Reasoning {
		return
	}

	switch compat.ThinkingFormat {
	case ThinkingFormatZAI, ThinkingFormatQwen:
		req.EnableThinking = boolPtr(false)
	case ThinkingFormatDeepSeek:
		req.Thinking = &openAIThinking{Type: "disabled"}
	case ThinkingFormatOpenRouter:
		// OpenRouter treats an omitted reasoning object as provider default; keep
		// the previous compat behavior by explicitly disabling it for reasoning
		// models when the caller didn't request reasoning.
		req.Reasoning = &openAIRouterReasoning{Effort: "none"}
	}
}

func applyOpenAIMaxTokens(req *openAIRequest, model *Model, o *options, compat *OpenAICompletionsCompat) {
	maxTokens := o.maxTokens
	if maxTokens == 0 {
		maxTokens = model.MaxTokens
	}
	if maxTokens == 0 {
		return
	}
	if compat.MaxTokensField == "max_tokens" {
		req.MaxTokens = maxTokens
		return
	}
	req.MaxCompletionTokens = maxTokens
}

// toolToFunctionDefinition converts a Tool to a functionDefinition.
func toolToFunctionDefinition(t Tool) functionDefinition {
	return functionDefinition{
		Name:        t.Name,
		Description: t.Description,
		Parameters:  t.Parameters,
	}
}

// maybeAddOpenRouterAnthropicCacheControlToMessages adds cache_control for OpenRouter Anthropic models.
// This version works with openAIMessage type.
func maybeAddOpenRouterAnthropicCacheControlToMessages(model *Model, messages []openAIMessage) {
	if model.Provider != ProviderOpenRouter || !strings.HasPrefix(model.ID, "anthropic/") {
		return
	}

	// Find last user/assistant message with text content and add cache_control
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}

		// If content is a string, convert to array with cache_control
		if contentStr, ok := msg.Content.(string); ok && contentStr != "" {
			messages[i].Content = []map[string]any{
				{
					"type":          "text",
					"text":          contentStr,
					"cache_control": map[string]string{"type": "ephemeral"},
				},
			}
			return
		}

		// If content is an array, find last text part
		if contentArr, ok := msg.Content.([]map[string]any); ok {
			for j := len(contentArr) - 1; j >= 0; j-- {
				part := contentArr[j]
				if partType, ok := part["type"].(string); ok && partType == "text" {
					part["cache_control"] = map[string]string{"type": "ephemeral"}
					return
				}
			}
		}
	}
}

func (c *openAIClient) convertMessages(prompt *Context, model *Model, compat *OpenAICompletionsCompat) ([]openAIMessage, error) {
	var msgs []openAIMessage

	// Add system prompt (GAP-001: developer role vs system)
	if prompt.SystemPrompt != "" {
		role := "system"
		if model.Reasoning && compat.SupportsDeveloperRole {
			role = "developer"
		}
		msgs = append(msgs, openAIMessage{
			Role:    role,
			Content: prompt.SystemPrompt,
		})
	}

	// Track last role for requiresAssistantAfterToolResult (GAP-007)
	var lastRole string

	// Convert messages with batching of consecutive tool results (GAP-003)
	for i := 0; i < len(prompt.Messages); i++ {
		m := prompt.Messages[i]

		// GAP-007: Insert synthetic assistant message when required
		if compat.RequiresAssistantAfterToolResult && lastRole == "toolResult" && normalizeMessageRole(m.Role) == "user" {
			msgs = append(msgs, openAIMessage{
				Role:    "assistant",
				Content: "I have processed the tool results.",
			})
		}

		msg, err := c.convertMessage(m, model, compat)
		if err != nil {
			return nil, err
		}

		if msg != nil {
			msgs = append(msgs, *msg)
			lastRole = normalizeMessageRole(m.Role)
		}

		// GAP-003 & GAP-015: Batch consecutive tool results with image handling
		if normalizeMessageRole(m.Role) == "toolResult" {
			imageBlocks := c.extractImageBlocksFromToolResult(m)

			// Collect consecutive tool results
			j := i + 1
			for ; j < len(prompt.Messages) && normalizeMessageRole(prompt.Messages[j].Role) == "toolResult"; j++ {
				toolMsg := prompt.Messages[j]
				toolResult, _ := c.convertMessage(toolMsg, model, compat)
				if toolResult != nil {
					msgs = append(msgs, *toolResult)
				}
				imageBlocks = append(imageBlocks, c.extractImageBlocksFromToolResult(toolMsg)...)
			}

			// Handle images from tool results
			if len(imageBlocks) > 0 && model.SupportsInput(InputImage) {
				if compat.RequiresAssistantAfterToolResult {
					msgs = append(msgs, openAIMessage{
						Role:    "assistant",
						Content: "I have processed the tool results.",
					})
				}
				msgs = append(msgs, openAIMessage{
					Role:    "user",
					Content: imageBlocks,
				})
				lastRole = "user"
			}

			i = j - 1
		}
	}

	return msgs, nil
}

// extractImageBlocksFromToolResult extracts image blocks from a tool result message.
func (c *openAIClient) extractImageBlocksFromToolResult(m Message) []map[string]any {
	var imageBlocks []map[string]any

	if blocks, ok := m.Content.([]ContentBlock); ok {
		for _, b := range blocks {
			if b.Type == "image" {
				imageBlocks = append(imageBlocks, map[string]any{
					"type": "image_url",
					"image_url": map[string]string{
						"url": fmt.Sprintf("data:%s;base64,%s", b.MimeType, b.Data),
					},
				})
			}
		}
	}

	return imageBlocks
}

func (c *openAIClient) convertMessage(m Message, model *Model, compat *OpenAICompletionsCompat) (*openAIMessage, error) {
	switch normalizeMessageRole(m.Role) {
	case "system":
		content, err := c.extractContent(m.Content, model)
		if err != nil {
			return nil, err
		}

		return &openAIMessage{Role: "system", Content: content}, nil

	case "user":
		content, err := c.extractContent(m.Content, model)
		if err != nil {
			return nil, err
		}

		return &openAIMessage{Role: "user", Content: content}, nil

	case "assistant":
		blocks, ok := m.Content.([]ContentBlock)
		if !ok {
			return nil, nil
		}

		// GAP-023 & GAP-024: Handle empty content and null content
		content := c.extractAssistantContent(blocks, compat)
		msg := &openAIMessage{Role: "assistant", Content: content}

		// Filter out empty text/thinking blocks (GAP-023)
		textBlocks := make([]ContentBlock, 0)
		thinkingBlocks := make([]ContentBlock, 0)
		toolCalls := make([]toolCall, 0)
		var reasonings []map[string]any

		for _, b := range blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					textBlocks = append(textBlocks, b)
				}
			case "thinking":
				if strings.TrimSpace(b.Thinking) != "" {
					thinkingBlocks = append(thinkingBlocks, b)
				}
			case "toolCall":
				tc := toolCall{
					ID:       b.ID,
					Type:     "function",
					Function: functionCall{Name: b.Name, Arguments: string(b.Arguments)},
				}
				toolCalls = append(toolCalls, tc)

				// GAP-009: Collect reasoning_details for tool calls with thought signatures
				if b.ThoughtSignature != "" {
					if detail, err := parseThoughtSignature(b.ThoughtSignature); err == nil && detail != nil {
						reasonings = append(reasonings, detail)
					}
				}
			}
		}

		// GAP-008: Convert thinking blocks to text when required
		if compat.RequiresThinkingAsText && len(thinkingBlocks) > 0 {
			var sb strings.Builder
			for _, tb := range thinkingBlocks {
				sb.WriteString(tb.Thinking)
				sb.WriteString("\n\n")
			}
			textBlocks = append([]ContentBlock{{Type: "text", Text: sb.String()}}, textBlocks...)
			thinkingBlocks = nil
		}

		// Add text content
		if len(textBlocks) > 0 {
			var sb strings.Builder
			for _, tb := range textBlocks {
				sb.WriteString(tb.Text)
			}
			msg.Content = sb.String()
		} else if compat.RequiresAssistantAfterToolResult {
			msg.Content = ""
		} else {
			msg.Content = nil
		}

		// Add thinking content using signature field (GAP-002)
		if len(thinkingBlocks) > 0 && !compat.RequiresThinkingAsText {
			sig := thinkingBlocks[0].ThoughtSignature
			if sig != "" {
				msg.Content = sig // Use signature field for reasoning content
			}
		}

		// Add tool calls
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls

			// GAP-009: Add reasoning_details
			if len(reasonings) > 0 {
				msg.ReasoningDetails = reasonings
			}
		}

		// GAP-024: Skip messages with no content and no tool calls
		if msg.Content == nil && len(msg.ToolCalls) == 0 {
			return nil, nil
		}

		return msg, nil

	case "toolResult":
		content, toolResultMsg := c.convertToolResultMessage(m, compat)
		if toolResultMsg != nil {
			return toolResultMsg, nil
		}

		return &openAIMessage{
			Role:       "tool",
			ToolCallID: m.ToolCallID,
			Content:    content,
		}, nil

	default:
		return nil, nil
	}
}

// extractAssistantContent extracts text content from assistant message blocks.
// Handles compat.RequiresAssistantAfterToolResult for null content.
func (c *openAIClient) extractAssistantContent(blocks []ContentBlock, compat *OpenAICompletionsCompat) any {
	var textParts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			textParts = append(textParts, b.Text)
		}
	}

	if len(textParts) > 0 {
		return strings.Join(textParts, "")
	}

	if compat.RequiresAssistantAfterToolResult {
		return ""
	}

	return nil
}

// convertToolResultMessage converts a tool result message, handling provider-specific requirements.
func (c *openAIClient) convertToolResultMessage(m Message, compat *OpenAICompletionsCompat) (string, *openAIMessage) {
	// Extract text content
	var textResult string
	var hasImages bool

	if blocks, ok := m.Content.([]ContentBlock); ok {
		var sb strings.Builder
		for _, b := range blocks {
			switch b.Type {
			case "text":
				sb.WriteString(b.Text)
			case "image":
				hasImages = true
			}
		}
		textResult = sb.String()
	} else if str, ok := m.Content.(string); ok {
		textResult = str
	}

	// GAP-015: Handle images separately
	if hasImages {
		// Return placeholder text - images handled in main loop
		return "(see attached image)", nil
	}

	// GAP-014: Add name field if required
	if compat.RequiresToolResultName && m.ToolName != "" {
		return textResult, &openAIMessage{
			Role:       "tool",
			ToolCallID: m.ToolCallID,
			Name:       m.ToolName,
			Content:    textResult,
		}
	}

	return textResult, nil
}

// parseThoughtSignature parses a thought signature into reasoning detail format.
func parseThoughtSignature(sig string) (map[string]any, error) {
	// Try to parse as JSON first
	var detail map[string]any
	if err := json.Unmarshal([]byte(sig), &detail); err == nil {
		if detail["type"] == "reasoning.encrypted" && detail["id"] != nil && detail["data"] != nil {
			return detail, nil
		}
	}
	return nil, errors.New("invalid signature format")
}

func (c *openAIClient) extractContent(content any, model *Model) (any, error) {
	switch v := content.(type) {
	case string:
		return v, nil
	case []ContentBlock:
		var sb strings.Builder
		var imageBlocks []map[string]any

		for _, b := range v {
			switch b.Type {
			case "text":
				sb.WriteString(b.Text)
			case "image":
				// GAP-022: Filter images when model doesn't support them
				if model.SupportsInput(InputImage) {
					imageBlocks = append(imageBlocks, map[string]any{
						"type": "image_url",
						"image_url": map[string]string{
							"url": fmt.Sprintf("data:%s;base64,%s", b.MimeType, b.Data),
						},
					})
				}
			}
		}

		text := sb.String()
		if len(imageBlocks) > 0 {
			result := make([]any, 0, len(imageBlocks)+1)
			if text != "" {
				result = append(result, map[string]any{"type": "text", "text": text})
			}
			for _, block := range imageBlocks {
				result = append(result, block)
			}
			return result, nil
		}

		return text, nil
	default:
		return "", nil
	}
}

func (c *openAIClient) handleChunk(ctx context.Context, chunk openAIChunk, handler EventHandler) error {
	var data bytes.Buffer
	if err := data.WriteByte('\n'); err != nil {
		return fmt.Errorf("write stream chunk prefix: %w", err)
	}
	if _, err := data.WriteString("data: "); err != nil {
		return fmt.Errorf("write stream chunk prefix: %w", err)
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("marshal stream chunk: %w", err)
	}
	if _, err := data.Write(encoded); err != nil {
		return fmt.Errorf("write stream chunk: %w", err)
	}
	return streamOpenAIResponse(ctx, c.model, &data, handler)
}

// streamOpenAIResponse accumulates text, thinking, and tool-call deltas across
// streamed SSE chunks into a single AssistantMessage, mirroring the working
// streamOpenAICompatResponse in openai_compat.go and pi-mono's openai-completions
// streaming. It emits Start/Delta/End events with Partial.Content populated so
// agent_loop's `partialMessage.Content = e.Partial.Content` keeps the visible
// text, and emits a single Done at end-of-stream with the full Content.
//
// The earlier per-chunk handleChunk emitted Done with an empty AssistantMessage
// on every chunk, so reasoning-only models (opencode-go kimi-k2.6, glm-5.x)
// appeared empty in the TUI even though the upstream returned content.
func streamOpenAIResponse(ctx context.Context, model *Model, body io.Reader, handler EventHandler) error {
	return streamOpenAIResponseWithAPI(ctx, model, body, handler, model.API, model.Provider)
}

func streamOpenAIResponseWithAPI(ctx context.Context, model *Model, body io.Reader, handler EventHandler, api, provider string) error {
	reader := NewSSEReader(body)

	output := &AssistantMessage{
		Role:       "assistant",
		API:        api,
		Provider:   provider,
		Model:      model.ID,
		Content:    []ContentBlock{},
		StopReason: StopReasonStop,
		Usage:      Usage{},
		Timestamp:  timestamp(),
	}

	handler(ctx, Start{Partial: *output})

	compat := model.GetCompat()
	if api == APIOpenAICompatible {
		compat.ReasoningDetails = true
	}

	currentText := ""
	currentThinking := ""
	currentToolCall := (*ToolCall)(nil)
	currentToolCallSignature := ""
	contentIndex := 0

	flushText := func() {
		if currentText == "" {
			return
		}
		output.Content = append(output.Content, ContentBlock{Type: ContentBlockText, Text: currentText})
		handler(ctx, TextEnd{Content: currentText, ContentIndex: contentIndex, Partial: *output})
		contentIndex++
		currentText = ""
	}

	flushThinking := func() {
		if currentThinking == "" {
			return
		}
		output.Content = append(output.Content, ContentBlock{Type: ContentBlockThinking, Thinking: currentThinking})
		handler(ctx, ThinkingEnd{ContentIndex: contentIndex, Content: currentThinking, Partial: *output})
		contentIndex++
		currentThinking = ""
	}

	flushToolCall := func() {
		if currentToolCall == nil {
			return
		}
		output.Content = append(output.Content, ContentBlock{
			Type:             ContentBlockToolCall,
			ID:               currentToolCall.ID,
			Name:             currentToolCall.Name,
			Arguments:        currentToolCall.Arguments,
			ThoughtSignature: currentToolCallSignature,
		})
		handler(ctx, ToolCallEnd{ContentIndex: contentIndex, ToolCall: *currentToolCall, Partial: *output})
		contentIndex++
		currentToolCall = nil
		currentToolCallSignature = ""
	}

	for {
		dataStr, ok := reader.Next()
		if !ok {
			break
		}

		var chunk openAIChunk
		if err := json.Unmarshal([]byte(dataStr), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			emitTerminalStreamError(ctx, handler, AssistantMessage{
				Role:         "assistant",
				API:          api,
				Provider:     provider,
				Model:        model.ID,
				StopReason:   StopReasonError,
				ErrorMessage: chunk.Error.Message,
				Timestamp:    timestamp(),
			}, errors.New(chunk.Error.Message), StopReasonError)
			return nil
		}
		if chunk.ID != "" {
			output.ResponseID = chunk.ID
		}
		if chunk.Usage.hasTokens() {
			output.Usage = parseOpenAIUsage(&chunk.Usage, model)
		}

		for _, choice := range chunk.Choices {
			// Text content (streaming delta)
			for _, item := range choice.Delta.Content {
				if item.Text == "" {
					continue
				}
				if currentText == "" {
					// Starting a new text block: flush any pending thinking/tool call.
					if currentThinking != "" {
						flushThinking()
					}
					if currentToolCall != nil {
						flushToolCall()
					}
					handler(ctx, TextStart{ContentIndex: contentIndex, Partial: *output})
				}
				currentText += item.Text
				handler(ctx, TextDelta{ContentIndex: contentIndex, Delta: item.Text, Partial: *output})
			}

			// Non-streaming message fallback (some providers send a single
			// complete message chunk instead of streaming deltas).
			for _, msg := range choice.Messages {
				if currentText == "" {
					if currentThinking != "" {
						flushThinking()
					}
					if currentToolCall != nil {
						flushToolCall()
					}
					handler(ctx, TextStart{ContentIndex: contentIndex, Partial: *output})
				}
				currentText += msg.Content
				handler(ctx, TextDelta{ContentIndex: contentIndex, Delta: msg.Content, Partial: *output})
			}

			// Reasoning/thinking content (GAP-002: multiple field names)
			if choice.Delta.ReasoningContent != "" {
				if currentThinking == "" {
					if currentText != "" {
						flushText()
					}
					if currentToolCall != nil {
						flushToolCall()
					}
					handler(ctx, ThinkingStart{ContentIndex: contentIndex, Partial: *output})
				}
				currentThinking += choice.Delta.ReasoningContent
				handler(ctx, ThinkingDelta{ContentIndex: contentIndex, Delta: choice.Delta.ReasoningContent, Partial: *output})
			}

			// Reasoning_details for tool call signatures (GAP-009)
			if len(choice.Delta.ReasoningDetails) > 0 && compat.ReasoningDetails {
				applyOpenAIReasoningDetails(output, currentToolCall, &currentToolCallSignature, choice.Delta.ReasoningDetails)
			}

			// Tool calls
			if choice.Delta.ToolCalls != nil {
				for _, tc := range choice.Delta.ToolCalls {
					if currentToolCall == nil || (tc.ID != "" && currentToolCall.ID != tc.ID) {
						if currentToolCall != nil {
							flushToolCall()
						}
						if currentText != "" {
							flushText()
						}
						if currentThinking != "" {
							flushThinking()
						}
						currentToolCall = &ToolCall{
							ID:        tc.ID,
							Name:      tc.Function.Name,
							Arguments: json.RawMessage{},
						}
						handler(ctx, ToolCallStart{ContentIndex: contentIndex, Partial: *output})
					}
					if tc.Function.Arguments != "" {
						currentToolCall.Arguments = append(currentToolCall.Arguments, tc.Function.Arguments...)
						handler(ctx, ToolCallDelta{ContentIndex: contentIndex, Delta: tc.Function.Arguments, Partial: *output})
					}
				}
			}

			// Check for tool calls in the message (set stop reason)
			if choice.Delta.ToolCalls != nil {
				output.StopReason = StopReasonToolUse
			}

			if choice.FinishReason != "" {
				stopReason, errMsg := mapOpenAIStopReason(choice.FinishReason)
				output.StopReason = stopReason
				if errMsg != "" {
					output.ErrorMessage = errMsg
				}
			}

			if choice.Usage != nil {
				output.Usage = parseOpenAICompatUsage(choice.Usage, model)
			}
		}
	}

	// A mid-stream read failure ends the loop the same way a clean EOF does,
	// so it has to be reported explicitly rather than emitting a normal Done.
	if err := reader.Error(); err != nil {
		emitTerminalStreamError(ctx, handler, *output, fmt.Errorf("openai: read stream: %w", err), StopReasonError)
		return nil
	}

	// Flush remaining state and emit terminal Done.
	if currentText != "" {
		flushText()
	}
	if currentThinking != "" {
		flushThinking()
	}
	if currentToolCall != nil {
		flushToolCall()
	}

	handler(ctx, Done{Reason: output.StopReason, Message: *output})
	return nil
}

// OpenAI-compatible API types.
type (
	openAICompatUsage                   = usage
	openAICompatPromptTokensDetails     = openAIPromptTokensDetails
	openAICompatCompletionTokensDetails = openAICompletionTokensDetails
)

type openAICompatContentDelta []openAICompatContent

func (c *openAICompatContentDelta) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*c = nil
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		if text == "" {
			*c = nil
			return nil
		}
		*c = []openAICompatContent{{Type: "text", Text: text}}
		return nil
	}

	var items []openAICompatContent
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("content delta: %w", err)
	}
	*c = items
	return nil
}

type openAICompatContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type openAIRequest struct {
	Model               string                 `json:"model"`
	Messages            []openAIMessage        `json:"messages"`
	Tools               []toolDefinition       `json:"tools,omitempty"`
	Metadata            map[string]any         `json:"metadata,omitempty"`
	Store               *bool                  `json:"store,omitempty"`
	MaxTokens           int                    `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                    `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string                 `json:"reasoning_effort,omitempty"`
	EnableThinking      *bool                  `json:"enable_thinking,omitempty"`
	Thinking            *openAIThinking        `json:"thinking,omitempty"`
	ChatTemplate_kwargs map[string]bool        `json:"chat_template_kwargs,omitempty"`
	Reasoning           *openAIRouterReasoning `json:"reasoning,omitempty"`
	Provider            any                    `json:"provider,omitempty"`
	ProviderOptions     map[string]any         `json:"provider_options,omitempty"`
	ToolChoice          any                    `json:"tool_choice,omitempty"`
	ToolStream          bool                   `json:"tool_stream,omitempty"`
	Stream              bool                   `json:"stream"`
	StreamOptions       *streamOptions         `json:"stream_options,omitempty"`
}

// openAIRouterReasoning represents OpenRouter's reasoning object.
type openAIRouterReasoning struct {
	Effort string `json:"effort,omitempty"`
}

// openAIThinking represents the DeepSeek-style thinking object
// { type: "enabled" | "disabled" } accepted by z.ai / opencode kimi backends.
type openAIThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// boolPtr returns a pointer to v, used so *bool request fields can be
// serialized as an explicit false (a bare bool with omitempty would omit it).
func boolPtr(v bool) *bool { return &v }

// streamOptions for requesting usage in streaming responses.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role             string           `json:"role"`
	Content          any              `json:"content,omitempty"`
	Name             string           `json:"name,omitempty"` // For tool results when required
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ToolCalls        []toolCall       `json:"tool_calls,omitempty"`
	ReasoningDetails []map[string]any `json:"reasoning_details,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolDefinition struct {
	Type     string             `json:"type"`
	Function functionDefinition `json:"function"`
}

type functionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

type openAIChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []choice     `json:"choices"`
	Usage   usage        `json:"usage,omitempty"`
	Error   *openAIError `json:"error,omitempty"`
}

type choice struct {
	Index        int                `json:"index"`
	Delta        delta              `json:"delta"`
	Messages     []msg              `json:"messages,omitempty"`
	FinishReason string             `json:"finish_reason"`
	Usage        *openAICompatUsage `json:"usage,omitempty"`
}

type delta struct {
	Content          openAICompatContentDelta `json:"content,omitempty"`
	ReasoningContent string                   `json:"-"` // reasoning_content, reasoning, reasoning_text
	ReasoningDetails []reasoningDetail        `json:"reasoning_details,omitempty"`
	ToolCalls        []toolCall               `json:"tool_calls,omitempty"`
}

type reasoningDetail struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Data string `json:"data,omitempty"`
}

type msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type usage struct {
	PromptTokens            int                            `json:"prompt_tokens"`
	CompletionTokens        int                            `json:"completion_tokens"`
	TotalTokens             int                            `json:"total_tokens"`
	Input                   int                            `json:"input,omitempty"`
	Output                  int                            `json:"output,omitempty"`
	PromptTokensDetails     *openAIPromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *openAICompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type openAIPromptTokensDetails struct {
	CachedTokens     int `json:"cached_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

type openAICompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func (u usage) hasTokens() bool {
	return u.PromptTokens > 0 || u.CompletionTokens > 0 || u.TotalTokens > 0 || u.Input > 0 || u.Output > 0
}

func parseOpenAIUsage(raw *usage, model *Model) Usage {
	compatRaw := &openAICompatUsage{
		PromptTokens:            raw.PromptTokens,
		CompletionTokens:        raw.CompletionTokens,
		TotalTokens:             raw.TotalTokens,
		PromptTokensDetails:     raw.PromptTokensDetails,
		CompletionTokensDetails: raw.CompletionTokensDetails,
	}
	if compatRaw.PromptTokens == 0 {
		compatRaw.PromptTokens = raw.Input
	}
	if compatRaw.CompletionTokens == 0 {
		compatRaw.CompletionTokens = raw.Output
	}
	return parseOpenAICompatUsage(compatRaw, model)
}

func mapOpenAIStopReason(reason string) (StopReason, string) {
	switch reason {
	case "":
		return StopReasonError, ""
	case "stop", "end":
		return StopReasonStop, ""
	case "length":
		return StopReasonLength, ""
	case "function_call", "tool_calls":
		return StopReasonToolUse, ""
	case "content_filter":
		return StopReasonError, "Provider finish_reason: content_filter"
	case "network_error":
		return StopReasonError, "Provider finish_reason: network_error"
	default:
		return StopReasonError, fmt.Sprintf("Provider finish_reason: %s", reason)
	}
}

func applyOpenAIReasoningDetails(output *AssistantMessage, currentToolCall *ToolCall, currentToolCallSignature *string, details []reasoningDetail) {
	for _, detail := range details {
		if detail.Type != "reasoning.encrypted" || detail.ID == "" || detail.Data == "" {
			continue
		}
		sigJSON, err := json.Marshal(detail)
		if err != nil {
			continue
		}
		if currentToolCall != nil && currentToolCall.ID == detail.ID && currentToolCallSignature != nil {
			*currentToolCallSignature = string(sigJSON)
		}
		for j := range output.Content {
			if output.Content[j].Type == ContentBlockToolCall && output.Content[j].ID == detail.ID {
				output.Content[j].ThoughtSignature = string(sigJSON)
				break
			}
		}
	}
}

// UnmarshalJSON implements custom unmarshaling for delta to handle reasoning fields.
func (d *delta) UnmarshalJSON(data []byte) error {
	type deltaAlias delta
	var tmp struct {
		deltaAlias
		ReasoningContent string `json:"reasoning_content,omitempty"`
		Reasoning        string `json:"reasoning,omitempty"`
		ReasoningText    string `json:"reasoning_text,omitempty"`
	}

	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}

	d.Content = tmp.Content
	d.ToolCalls = tmp.ToolCalls
	d.ReasoningDetails = tmp.ReasoningDetails

	switch {
	case tmp.ReasoningContent != "":
		d.ReasoningContent = tmp.ReasoningContent
	case tmp.Reasoning != "":
		d.ReasoningContent = tmp.Reasoning
	case tmp.ReasoningText != "":
		d.ReasoningContent = tmp.ReasoningText
	}

	return nil
}

// Use getEnvAPIKey from env.go for API key retrieval.

func init() {
	Register(APIOpenAICompletions, &OpenAICompletionsProvider{})
}

// AddUserMessage adds a user message to the context.
func (c *Context) AddUserMessage(content string) {
	c.Messages = append(c.Messages, Message{
		Role:      "user",
		Content:   content,
		Timestamp: timestamp(),
	})
}

// AddAssistantMessage adds an assistant message to the context.
func (c *Context) AddAssistantMessage(content []ContentBlock, stopReason StopReason) {
	c.Messages = append(c.Messages, Message{
		Role:      "assistant",
		Content:   content,
		Timestamp: timestamp(),
	})
}

// AddToolResult adds a tool result to the context.
func (c *Context) AddToolResult(callID, name, result string, isError bool) {
	c.Messages = append(c.Messages, Message{
		Role:       "toolResult",
		ToolCallID: callID,
		ToolName:   name,
		Content:    result,
		IsError:    isError,
		Timestamp:  timestamp(),
	})
}

func timestamp() int64 {
	return time.Now().UnixMilli()
}

// AppendMessages appends messages to the context.
func (c *Context) AppendMessages(msgs ...Message) {
	c.Messages = append(c.Messages, msgs...)
}
