package ai // import "github.com/losinggeneration/tau/ai"

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Note: the path /v1/messages is appended at the call site, so the base URL
// must NOT include /v1 — otherwise the final URL would be .../v1/v1/messages.
const anthropicBaseURL = "https://api.anthropic.com"

// Claude Code tool names for OAuth token handling.
// Source: Claude Code 2.x canonical casing
var claudeCodeTools = []string{
	"Read",
	"Write",
	"Edit",
	"Bash",
	"Grep",
	"Glob",
	"AskUserQuestion",
	"EnterPlanMode",
	"ExitPlanMode",
	"KillShell",
	"NotebookEdit",
	"Skill",
	"Task",
	"TaskOutput",
	"TodoWrite",
	"WebFetch",
	"WebSearch",
}

// ccToolLookup maps lowercase tool names to their Claude Code canonical casing.
var ccToolLookup map[string]string

func init() {
	ccToolLookup = make(map[string]string, len(claudeCodeTools))
	for _, t := range claudeCodeTools {
		ccToolLookup[strings.ToLower(t)] = t
	}
}

// toClaudeCodeName converts a tool name to Claude Code canonical casing
// if it matches a known Claude Code tool (case-insensitive).
func toClaudeCodeName(name string) string {
	if canonical, ok := ccToolLookup[strings.ToLower(name)]; ok {
		return canonical
	}
	return name
}

// fromClaudeCodeName converts a Claude Code tool name back to the original
// tool name if a matching tool is found in the provided tools list.
func fromClaudeCodeName(name string, tools []Tool) string {
	if len(tools) == 0 {
		return name
	}

	lowerName := strings.ToLower(name)
	for _, tool := range tools {
		if strings.ToLower(tool.Name) == lowerName {
			return tool.Name
		}
	}
	return name
}

// isAdaptiveThinkingModel checks if a model supports adaptive thinking.
// Adaptive thinking models include Opus 4.6+, Sonnet 4.6+, and Opus 4.7+.
func isAdaptiveThinkingModel(modelID string) bool {
	lowerID := strings.ToLower(modelID)
	return strings.Contains(lowerID, "opus-4-6") ||
		strings.Contains(lowerID, "opus-4.6") ||
		strings.Contains(lowerID, "opus-4-7") ||
		strings.Contains(lowerID, "opus-4.7") ||
		strings.Contains(lowerID, "sonnet-4-6") ||
		strings.Contains(lowerID, "sonnet-4.6")
}

// mapReasoningLevelToAnthropicEffort translates the generic ReasoningLevel
// into Anthropic's adaptive-effort vocabulary (low|medium|high|xhigh|max).
// Anthropic does not accept "minimal" or "off"; the closest analogue is "low".
// On Opus 4.6+ models, ReasoningXHigh is mapped to "max" (the highest native
// effort); on Opus 4.7 it maps to "xhigh" to match Pi's vocabulary.
func mapReasoningLevelToAnthropicEffort(level ReasoningLevel, modelID string) string {
	switch level {
	case ReasoningMinimal:
		return "low"
	case ReasoningLow:
		return "low"
	case ReasoningMedium:
		return "medium"
	case ReasoningHigh:
		return "high"
	case ReasoningXHigh:
		lower := strings.ToLower(modelID)
		if strings.Contains(lower, "opus-4-7") || strings.Contains(lower, "opus-4.7") {
			return "xhigh"
		}
		if strings.Contains(lower, "opus-4-6") || strings.Contains(lower, "opus-4.6") {
			return "max"
		}
		return "high"
	default:
		return ""
	}
}

// isOAuthToken checks if the API key is an OAuth token.
func isOAuthToken(apiKey string) bool {
	return strings.Contains(apiKey, "sk-ant-oat")
}

type anthropicClient struct {
	model         *Model
	transport     *HTTPTransport
	isOAuth       bool
	interleaved   bool
	tools         []Tool
	transportOpts *TransportOption
}

// SetTransportOption implements the SDK's transport option injection interface.
func (c *anthropicClient) SetTransportOption(opts TransportOption) {
	c.transportOpts = &opts
}

// newAnthropicClient creates a new Anthropic client.
func newAnthropicClient(model *Model) (*anthropicClient, error) {
	baseURL := anthropicBaseURL
	if model != nil && model.BaseURL != "" {
		baseURL = model.BaseURL
	}

	return &anthropicClient{
		model: model,
		transport: NewTransport(baseURL, "", map[string][]string{
			"Content-Type":      {"application/json"},
			"anthropic-version": {"2023-06-01"},
			"anthropic-dangerous-direct-password-access": {"true"},
		}),
	}, nil
}

// Complete implements Client.
func (c *anthropicClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

// Stream implements Client.
func (c *anthropicClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) (err error) {
	var o options
	o.apply(opts)

	// Determine if this is an OAuth token
	apiKey := o.apiKey
	if apiKey == "" {
		apiKey = envGet(envVarNames(model.Provider)...)
	}
	c.isOAuth = isOAuthToken(apiKey)

	// Check for interleaved thinking option
	c.interleaved = o.interleavedThinking != nil && (*o.interleavedThinking || (isAdaptiveThinkingModel(model.ID) && *o.interleavedThinking))
	c.tools = prompt.Tools

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

	if apiKey == "" {
		return fmt.Errorf("%s: missing API key", model.Provider)
	}

	// Build headers based on auth type
	headers := c.buildHeaders(apiKey, model, &o)

	// Build transport with optional transport-level options (e.g., refresh callbacks)
	var transport *HTTPTransport
	if o.transportOpts != nil {
		// Convert map[string]string to http.Header
		httpHeaders := make(http.Header)
		for k, v := range headers {
			httpHeaders.Set(k, v)
		}
		transport = NewTransportWithOptions(c.transport.baseURL, apiKey, httpHeaders, TransportOptions{
			RefreshCreds: o.transportOpts.RefreshCreds,
			Provider:     o.transportOpts.Provider,
		})
	} else {
		transport = c.transport.withHeaders(headers)
	}

	resp, err := transport.Do(ctx, "POST", "/v1/messages", reqBody)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	if hasResponseCallback(&o) {
		invokeResponseCallback(&o, model, responseMetadata(resp))
	}

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("API error: %d", resp.StatusCode)
		}

		return fmt.Errorf("API error: %d %s", resp.StatusCode, string(body))
	}

	// Check for streaming
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return c.handleSSE(ctx, resp.Body, handler)
	}

	// Handle non-streaming response
	return c.handleJSON(ctx, resp.Body, handler)
}

// buildHeaders constructs the appropriate headers based on auth type and model.
func (c *anthropicClient) buildHeaders(apiKey string, model *Model, o *options) map[string]string {
	headers := make(map[string]string)

	// Base headers
	headers["x-api-key"] = apiKey
	headers["accept"] = "application/json"
	headers["anthropic-dangerous-direct-browser-access"] = "true"

	// Build beta features list
	var betaFeatures []string

	// Fine-grained tool streaming beta - always included for non-copilot
	if model.Provider != ProviderGitHubCopilot {
		betaFeatures = append(betaFeatures, "fine-grained-tool-streaming-2025-05-14")
	}

	// Interleaved thinking beta - only for non-adaptive models when enabled
	needsInterleavedBeta := c.interleaved && !isAdaptiveThinkingModel(model.ID)
	if needsInterleavedBeta {
		betaFeatures = append(betaFeatures, "interleaved-thinking-2025-05-14")
	}

	// OAuth: includes claude-code and oauth betas + Claude Code identity headers
	if c.isOAuth {
		betaFeatures = append([]string{
			"claude-code-20250219",
			"oauth-2025-04-20",
		}, betaFeatures...)

		headers["anthropic-beta"] = strings.Join(betaFeatures, ",")
		headers["user-agent"] = "claude-cli/2.1.75"
		headers["x-app"] = "cli"
	} else {
		// Standard or Copilot auth
		if len(betaFeatures) > 0 {
			headers["anthropic-beta"] = strings.Join(betaFeatures, ",")
		}

		// Copilot-specific headers
		if model.Provider == ProviderGitHubCopilot {
			// Copilot uses Bearer auth
			headers["authorization"] = "Bearer " + apiKey
		}
	}

	// Merge user-provided headers
	for k, v := range o.headers {
		headers[k] = v
	}

	return headers
}

// buildRequest builds the Anthropic API request body.
func (c *anthropicClient) buildRequest(model *Model, prompt *Context, o *options) (string, error) {
	req := anthropicRequest{
		Model:     model.ID,
		MaxTokens: model.MaxTokens,
		Stream:    true,
	}

	// Transform messages for cross-provider compatibility. This handles the
	// same-model vs cross-model split for tool-call IDs: foreign IDs (e.g.
	// OpenAI Responses' `call_id|item_id`, or Anthropic-compatible providers
	// whose tool IDs include characters outside the
	// ^[a-zA-Z0-9_-]{1,64}$ set) get normalized so both sides of the
	// tool_call/tool_result pairing survive the provider switch with matching
	// IDs. Without this, the target provider rejects the replay with
	// "tool result's tool id(...) not found".
	transformedCtx, err := TransformContext(prompt, model, normalizeAnthropicToolCallIDNormalizer)
	if err != nil {
		return "", fmt.Errorf("transform messages: %w", err)
	}

	// Dual-defense pass: even after the transform above, ensure the
	// toolResult ToolCallID and the assistant toolCall block ID agree. The
	// transform's toolCallIDMap records the assistant-side rename, but only
	// mutates toolResult.ToolCallID when an assistant turn was processed
	// earlier in the same TransformContext call. A second pass guarantees
	// the mapping is applied unconditionally, matching the openai.go
	// buildOpenAIChatCompletionsRequest behavior.
	for i := range transformedCtx.Messages {
		msg := &transformedCtx.Messages[i]
		if msg.ToolCallID != "" {
			msg.ToolCallID = normalizeAnthropicToolCallIDOnly(msg.ToolCallID)
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
				blocks[j].ID = normalizeAnthropicToolCallIDOnly(blocks[j].ID)
			}
		}
	}

	// Determine cache control settings
	cacheRetention := resolveCacheRetention(o.cacheRetention)
	var cacheControl *cacheControlParam
	if cacheRetention != "none" {
		ttl := ""
		if cacheRetention == "long" && !strings.Contains(model.BaseURL, "api.anthropic.com") {
			ttl = "1h"
		}
		cacheControl = &cacheControlParam{Type: "ephemeral"}
		if ttl != "" {
			cacheControl.TTL = ttl
		}
	}

	// Convert messages
	msgs, err := c.convertMessages(transformedCtx, cacheControl)
	if err != nil {
		return "", err
	}

	req.Messages = msgs

	// Add system prompt
	if prompt.SystemPrompt != "" {
		if c.isOAuth {
			// For OAuth tokens, include Claude Code identity in system
			systemBlocks := []anthropicSystemBlock{
				{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude."},
				{Type: "text", Text: prompt.SystemPrompt},
			}
			// Add cache control to system if enabled
			if cacheControl != nil {
				systemBlocks[0].CacheControl = cacheControl
				systemBlocks[1].CacheControl = cacheControl
			}
			req.System = systemBlocks
		} else {
			req.System = prompt.SystemPrompt
		}
	} else if c.isOAuth {
		// OAuth without system prompt still needs Claude Code identity
		systemBlocks := []anthropicSystemBlock{
			{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude."},
		}
		if cacheControl != nil {
			systemBlocks[0].CacheControl = cacheControl
		}
		req.System = systemBlocks
	}

	// Handle tools with cache_control on last tool
	if len(prompt.Tools) > 0 {
		req.Tools = make([]anthropicTool, len(prompt.Tools))
		for i, t := range prompt.Tools {
			name := t.Name
			if c.isOAuth {
				name = toClaudeCodeName(t.Name)
			}

			tool := anthropicTool{
				Name:        name,
				Description: t.Description,
				InputSchema: t.Parameters,
			}

			// Apply cache_control to the last tool for conversation caching
			if i == len(prompt.Tools)-1 && cacheControl != nil {
				tool.CacheControl = cacheControl
			}

			req.Tools[i] = tool
		}
	}

	// Handle thinking
	//
	// hasThinking is true when the caller expressed any opinion about
	// reasoning/thinking. The gate covers both the Anthropic-specific
	// options (thinkingEnabled, thinkingBudgetTokens, effort) and the
	// generic WithReasoning level. The result is reused below to suppress
	// the temperature field, which Anthropic rejects alongside thinking.
	hasThinking := o.thinkingEnabled || o.thinkingBudgetTokens > 0 || o.effort != "" ||
		(o.reasoning != "" && o.reasoning != ReasoningOff)
	if hasThinking {
		// Determine thinking type based on model capabilities
		thinkingConfig := &anthropicThinking{
			Enabled: true,
		}

		// Check if this is an adaptive thinking model (Opus 4.6+, Sonnet 4.6+)
		isAdaptiveModel := isAdaptiveThinkingModel(model.ID)

		// Resolve effort: explicit WithEffort wins; otherwise translate the
		// generic WithReasoning level into Anthropic's effort vocabulary.
		effort := o.effort
		if effort == "" && o.reasoning != "" && o.reasoning != ReasoningOff {
			effort = mapReasoningLevelToAnthropicEffort(o.reasoning, model.ID)
		}

		if isAdaptiveModel {
			// Adaptive thinking: Claude decides when and how much to think
			thinkingConfig.Type = "adaptive"
			// Default display to "summarized" if not specified
			display := o.thinkingDisplay
			if display == "" {
				display = "summarized"
			}
			thinkingConfig.Display = display

			// Set effort if either an explicit WithEffort or a translated
			// WithReasoning produced a value.
			if effort != "" {
				thinkingConfig.Effort = effort
			}
		} else {
			// Budget-based thinking for older models
			thinkingConfig.Type = "enabled"
			// Explicit WithThinkingBudgetTokens wins; otherwise derive a
			// default from the generic reasoning level.
			budget := o.thinkingBudgetTokens
			if budget == 0 && o.reasoning != "" && o.reasoning != ReasoningOff {
				budget = ThinkingBudgetForLevel(o.reasoning, nil)
			}
			thinkingConfig.BudgetTokens = budget
			// Default display to "summarized" if not specified
			display := o.thinkingDisplay
			if display == "" {
				display = "summarized"
			}
			thinkingConfig.Display = display
		}

		req.Thinking = thinkingConfig
	} else if o.thinkingType == "disabled" {
		req.Thinking = &anthropicThinking{
			Enabled: false,
			Type:    "disabled",
		}
	}

	// Handle tool choice
	if o.toolChoice != nil {
		switch choice := o.toolChoice.(type) {
		case string:
			req.ToolChoice = map[string]string{"type": choice}
		default:
			req.ToolChoice = choice
		}
	}

	// Handle temperature (incompatible with thinking)
	if o.temperature != nil && !hasThinking {
		req.Temperature = o.temperature
	}
	if len(o.metadata) > 0 {
		req.Metadata = o.metadata
	}

	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func (c *anthropicClient) convertMessages(prompt *Context, cacheControl *cacheControlParam) ([]anthropicMessage, error) {
	var msgs []anthropicMessage

	for i := 0; i < len(prompt.Messages); i++ {
		m := prompt.Messages[i]
		if normalizeMessageRole(m.Role) == "toolResult" {
			// Batch consecutive tool results
			content, next, err := c.convertToolResults(prompt.Messages, i, cacheControl)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, anthropicMessage{
				Role:    "user",
				Content: content,
			})
			i = next - 1
			continue
		}

		msg, err := c.convertMessage(m, cacheControl)
		if err != nil {
			return nil, err
		}

		if msg != nil {
			msgs = append(msgs, *msg)
		}
	}

	// GAP-019: Add cache_control to the last user message
	if cacheControl != nil && len(msgs) > 0 {
		lastMsg := &msgs[len(msgs)-1]
		if lastMsg.Role == "user" {
			c.addCacheControlToLastBlock(lastMsg, cacheControl)
		}
	}

	return msgs, nil
}

// addCacheControlToLastBlock adds cache_control to the last content block.
func (c *anthropicClient) addCacheControlToLastBlock(msg *anthropicMessage, cacheControl *cacheControlParam) {
	if msg.Content == nil {
		return
	}

	switch content := msg.Content.(type) {
	case string:
		// Convert string content to array with cache_control
		msg.Content = []anthropicContentBlock{
			{Type: "text", Text: content, CacheControl: cacheControl},
		}
	case []anthropicContentBlock:
		if len(content) > 0 {
			// Add cache_control to last block
			lastIdx := len(content) - 1
			switch content[lastIdx].Type {
			case "text", "image", "tool_result":
				content[lastIdx].CacheControl = cacheControl
			}
		}
	case []any:
		// Handle mixed array types
		if len(content) > 0 {
			lastIdx := len(content) - 1
			if block, ok := content[lastIdx].(anthropicContentBlock); ok {
				switch block.Type {
				case "text", "image", "tool_result":
					block.CacheControl = cacheControl
					content[lastIdx] = block
				}
			}
		}
	}
}

func (c *anthropicClient) convertToolResults(messages []Message, start int, cacheControl *cacheControlParam) ([]anthropicToolResultBlock, int, error) {
	var results []anthropicToolResultBlock

	i := start
	for ; i < len(messages); i++ {
		msg := messages[i]
		if normalizeMessageRole(msg.Role) != "toolResult" {
			break
		}

		content, err := c.toolResultContent(msg.Content)
		if err != nil {
			return nil, 0, err
		}

		result := anthropicToolResultBlock{
			Type:      "tool_result",
			ToolUseID: msg.ToolCallID,
			Content:   content,
			IsError:   msg.IsError,
		}

		// Add cache_control to the last tool result
		if i == len(messages)-1 || (i < len(messages)-1 && normalizeMessageRole(messages[i+1].Role) != "toolResult") {
			if cacheControl != nil {
				// Add to the content block
				if str, ok := result.Content.(string); ok {
					result.Content = []anthropicContentBlock{
						{Type: "text", Text: str, CacheControl: cacheControl},
					}
				} else if blocks, ok := result.Content.([]anthropicContentBlock); ok && len(blocks) > 0 {
					blocks[len(blocks)-1].CacheControl = cacheControl
				}
			}
		}

		results = append(results, result)
	}

	return results, i, nil
}

func (c *anthropicClient) convertMessage(m Message, cacheControl *cacheControlParam) (*anthropicMessage, error) {
	switch normalizeMessageRole(m.Role) {
	case "user":
		blocks, err := c.extractContent(m.Content)
		if err != nil {
			return nil, err
		}

		return &anthropicMessage{Role: "user", Content: blocks}, nil

	case "assistant":
		blocks, err := c.extractContent(m.Content)
		if err != nil {
			return nil, err
		}

		// Handle tool uses - add tool_use blocks for any toolCall content blocks
		if contentBlocks, ok := m.Content.([]ContentBlock); ok {
			for _, b := range contentBlocks {
				if b.Type == "toolCall" {
					name := b.Name
					if c.isOAuth {
						name = toClaudeCodeName(b.Name)
					}
					blocks = append(blocks, anthropicContentBlock{
						Type:  "tool_use",
						ID:    b.ID,
						Name:  name,
						Input: b.Arguments,
					})
				}
			}
		}

		msg := &anthropicMessage{Role: "assistant", Content: blocks}

		return msg, nil

	case "toolResult":
		// This case is handled in convertMessages for batching
		return nil, nil

	default:
		return nil, nil
	}
}

func (c *anthropicClient) toolResultContent(content any) (any, error) {
	blocks, err := c.extractContent(content)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return "", nil
	}
	if len(blocks) == 1 && blocks[0].Type == "text" {
		return blocks[0].Text, nil
	}

	return blocks, nil
}

func (c *anthropicClient) extractContent(content any) ([]anthropicContentBlock, error) {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil, nil
		}

		return []anthropicContentBlock{{Type: "text", Text: v}}, nil

	case []ContentBlock:
		var blocks []anthropicContentBlock

		for _, b := range v {
			switch b.Type {
			case "text":
				blocks = append(blocks, anthropicContentBlock{
					Type: "text",
					Text: b.Text,
				})
			case "image":
				blocks = append(blocks, anthropicContentBlock{
					Type: "image",
					Source: anthropicImageSource{
						Type:      "base64",
						MediaType: b.MimeType,
						Data:      b.Data,
					},
				})
			case "thinking":
				// Redacted thinking: pass the opaque payload back as redacted_thinking
				if b.Redacted {
					blocks = append(blocks, anthropicContentBlock{
						Type: "redacted_thinking",
						Data: b.ThoughtSignature,
					})
					continue
				}
				// Empty thinking content -> skip
				if strings.TrimSpace(b.Thinking) == "" {
					continue
				}
				// If no signature, convert to text block
				if b.ThoughtSignature == "" || strings.TrimSpace(b.ThoughtSignature) == "" {
					blocks = append(blocks, anthropicContentBlock{
						Type: "text",
						Text: b.Thinking,
					})
				} else {
					// Has signature -> emit thinking block with signature
					blocks = append(blocks, anthropicContentBlock{
						Type: "thinking",
						Text: b.Thinking,
						Data: b.ThoughtSignature, // Use Data for signature
					})
				}
			}
		}

		return blocks, nil

	default:
		return nil, nil
	}
}

func (c *anthropicClient) handleSSE(ctx context.Context, body io.Reader, handler EventHandler) error {
	assistant := AssistantMessage{
		Role:      "assistant",
		API:       APIAnthropicMessages,
		Provider:  c.model.Provider,
		Model:     c.model.ID,
		Content:   []ContentBlock{},
		Timestamp: timestamp(),
		Usage:     Usage{},
	}
	handler(ctx, Start{Partial: assistant})

	blockIndexes := make(map[int]int)
	partialToolArgs := make(map[int]string)
	// Track partial thinking signatures for signature_delta accumulation
	partialThinkSigs := make(map[int]string)

	for {
		line, err := readLine(body)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			emitTerminalStreamError(ctx, handler, assistant, err, StopReasonError)
			return nil
		}

		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		// Parse SSE event
		data, err := parseSSEData(line)
		if err != nil {
			continue
		}

		var event anthropicEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "message_start":
			if event.Message != nil {
				assistant.ResponseID = event.Message.ID
				assistant.Usage.Input = event.Message.Usage.InputTokens
				assistant.Usage.Output = event.Message.Usage.OutputTokens
				assistant.Usage.CacheWrite = event.Message.Usage.CacheCreationInput
				assistant.Usage.CacheRead = event.Message.Usage.CacheReadInput
				assistant.Usage.CalculateCost(c.model.Cost)
			}

		case "content_block_start":
			if event.ContentBlock == nil {
				continue
			}

			idx := len(assistant.Content)
			blockIndexes[event.Index] = idx

			switch event.ContentBlock.Type {
			case "text":
				assistant.Content = append(assistant.Content, ContentBlock{
					Type: "text",
					Text: event.ContentBlock.Text,
				})
				handler(ctx, TextStart{ContentIndex: idx, Partial: assistant})
			case "thinking":
				assistant.Content = append(assistant.Content, ContentBlock{
					Type:             "thinking",
					Thinking:         event.ContentBlock.Text,
					ThoughtSignature: "", // Initialize for signature_delta accumulation
				})
				handler(ctx, ThinkingStart{ContentIndex: idx, Partial: assistant})
			case "redacted_thinking":
				// Redacted thinking: capture the signature from data field
				assistant.Content = append(assistant.Content, ContentBlock{
					Type:             "thinking",
					Thinking:         "[Reasoning redacted]",
					ThoughtSignature: event.ContentBlock.Data,
					Redacted:         true,
				})
				handler(ctx, ThinkingStart{ContentIndex: idx, Partial: assistant})
				// Track signature for any following signature_delta events
				partialThinkSigs[idx] = event.ContentBlock.Data
			case "tool_use":
				// Convert tool name from Claude Code canonical to original if OAuth
				name := event.ContentBlock.Name
				if c.isOAuth {
					name = fromClaudeCodeName(name, c.tools)
				}
				assistant.Content = append(assistant.Content, ContentBlock{
					Type: "toolCall",
					ID:   event.ContentBlock.ID,
					Name: name,
				})
				if len(event.ContentBlock.Input) > 0 && string(event.ContentBlock.Input) != "{}" {
					assistant.Content[idx].Arguments = event.ContentBlock.Input
				}
				handler(ctx, ToolCallStart{ContentIndex: idx, Partial: assistant})
			}

		case "content_block_delta":
			if event.Delta == nil {
				continue
			}

			idx, ok := blockIndexes[event.Index]
			if !ok || idx >= len(assistant.Content) {
				continue
			}

			switch event.Delta.Type {
			case "text_delta":
				assistant.Content[idx].Text += event.Delta.Text
				handler(ctx, TextDelta{
					ContentIndex: idx,
					Delta:        event.Delta.Text,
					Partial:      assistant,
				})

			case "thinking_delta":
				assistant.Content[idx].Thinking += event.Delta.Thinking
				handler(ctx, ThinkingDelta{
					ContentIndex: idx,
					Delta:        event.Delta.Thinking,
					Partial:      assistant,
				})

			case "input_json_delta":
				partialToolArgs[event.Index] += event.Delta.PartialJSON
				assistant.Content[idx].Arguments = json.RawMessage(partialToolArgs[event.Index])
				handler(ctx, ToolCallDelta{
					ContentIndex: idx,
					Delta:        event.Delta.PartialJSON,
					Partial:      assistant,
				})

			case "signature_delta":
				// Accumulate thinking signature from signature_delta events
				if sig, ok := partialThinkSigs[event.Index]; ok {
					assistant.Content[idx].ThoughtSignature = sig + event.Delta.Signature
					partialThinkSigs[event.Index] = sig + event.Delta.Signature
				} else {
					// First signature_delta without prior partial
					assistant.Content[idx].ThoughtSignature = event.Delta.Signature
					partialThinkSigs[event.Index] = event.Delta.Signature
				}
			}

		case "content_block_stop":
			idx, ok := blockIndexes[event.Index]
			if !ok || idx >= len(assistant.Content) {
				continue
			}
			block := assistant.Content[idx]
			switch block.Type {
			case "text":
				handler(ctx, TextEnd{ContentIndex: idx, Content: block.Text, Partial: assistant})
			case "thinking":
				handler(ctx, ThinkingEnd{ContentIndex: idx, Content: block.Thinking, Partial: assistant})
			case "toolCall":
				if partial := partialToolArgs[event.Index]; partial != "" {
					block.Arguments = json.RawMessage(partial)
					assistant.Content[idx].Arguments = block.Arguments
				}
				handler(ctx, ToolCallEnd{
					ContentIndex: idx,
					ToolCall: ToolCall{
						ID:        block.ID,
						Name:      block.Name,
						Arguments: block.Arguments,
					},
					Partial: assistant,
				})
			}

		case "message_delta":
			if event.Delta != nil && event.Delta.StopReason != "" {
				assistant.StopReason = anthropicStopReason(event.Delta.StopReason)
			}

			if event.Usage != nil {
				assistant.Usage.Input = event.Usage.InputTokens
				assistant.Usage.Output = event.Usage.OutputTokens
				assistant.Usage.CacheWrite = event.Usage.CacheCreationInput
				assistant.Usage.CacheRead = event.Usage.CacheReadInput
				assistant.Usage.CalculateCost(c.model.Cost)
			}

		case "message_stop":
			handler(ctx, Done{
				Reason:  assistant.StopReason,
				Message: assistant,
			})
		}
	}

	return nil
}

func anthropicStopReason(reason string) StopReason {
	switch reason {
	case "end_turn":
		return StopReasonStop
	case "max_tokens":
		return StopReasonLength
	case "tool_use":
		return StopReasonToolUse
	case "error":
		return StopReasonError
	case "aborted":
		return StopReasonAborted
	default:
		return StopReason(reason)
	}
}

func (c *anthropicClient) handleJSON(ctx context.Context, body io.Reader, handler EventHandler) error {
	var resp anthropicResponse
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	assistant := AssistantMessage{
		Role:       "assistant",
		Content:    c.convertResponseBlocks(resp.Content),
		StopReason: anthropicStopReason(resp.StopReason),
		Timestamp:  timestamp(),
		Usage: Usage{
			Input:  resp.Usage.InputTokens,
			Output: resp.Usage.OutputTokens,
		},
	}
	assistant.Usage.CalculateCost(c.model.Cost)

	handler(ctx, Done{
		Reason:  assistant.StopReason,
		Message: assistant,
	})

	return nil
}

func (c *anthropicClient) convertResponseBlocks(blocks []anthropicResponseBlock) []ContentBlock {
	var result []ContentBlock

	for _, b := range blocks {
		switch b.Type {
		case "text":
			result = append(result, ContentBlock{
				Type: "text",
				Text: b.Text,
			})
		case "thinking":
			result = append(result, ContentBlock{
				Type:     "thinking",
				Thinking: b.Thinking,
			})
		case "tool_use":
			result = append(result, ContentBlock{
				Type:      "toolCall",
				ID:        b.ID,
				Name:      b.Name,
				Arguments: b.Input,
			})
		}
	}

	return result
}

// Anthropic API types

type anthropicRequest struct {
	Model       string             `json:"model"`
	Messages    []anthropicMessage `json:"messages"`
	System      any                `json:"system,omitempty"` // string or []anthropicSystemBlock
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Thinking    *anthropicThinking `json:"thinking,omitempty"`
	Metadata    map[string]any     `json:"metadata,omitempty"`
	MaxTokens   int                `json:"max_tokens"`
	Stream      bool               `json:"stream"`
	ToolChoice  any                `json:"tool_choice,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
}

type anthropicSystemBlock struct {
	Type         string             `json:"type"`
	Text         string             `json:"text"`
	CacheControl *cacheControlParam `json:"cache_control,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []anthropicContentBlock
}

type anthropicContentBlock struct {
	Type         string               `json:"type"`
	Text         string               `json:"text,omitempty"`
	Source       anthropicImageSource `json:"source,omitempty"`
	ID           string               `json:"id,omitempty"`
	Name         string               `json:"name,omitempty"`
	Input        json.RawMessage      `json:"input,omitempty"`
	Data         string               `json:"data,omitempty"` // For redacted_thinking
	CacheControl *cacheControlParam   `json:"cache_control,omitempty"`
}

type anthropicToolResultBlock struct {
	Type         string             `json:"type"`
	ToolUseID    string             `json:"tool_use_id"`
	Content      any                `json:"content"`
	IsError      bool               `json:"is_error,omitempty"`
	CacheControl *cacheControlParam `json:"cache_control,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicTool struct {
	Name         string             `json:"name"`
	Description  string             `json:"description"`
	InputSchema  json.RawMessage    `json:"input_schema"`
	CacheControl *cacheControlParam `json:"cache_control,omitempty"`
}

type cacheControlParam struct {
	Type string `json:"type"`          // "ephemeral"
	TTL  string `json:"ttl,omitempty"` // e.g., "1h"
}

type anthropicThinking struct {
	Enabled      bool   `json:"enabled"`
	Type         string `json:"type,omitempty"` // "adaptive" | "enabled" | "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"` // "summarized" | "omitted"
	Effort       string `json:"effort,omitempty"`  // "low" | "medium" | "high" | "xhigh" | "max"
}

type anthropicResponse struct {
	ID           string                   `json:"id"`
	Type         string                   `json:"type"`
	Role         string                   `json:"role"`
	Content      []anthropicResponseBlock `json:"content"`
	Model        string                   `json:"model"`
	StopReason   string                   `json:"stop_reason"`
	StopSequence string                   `json:"stop_sequence"`
	Usage        anthropicUsage           `json:"usage"`
}

type anthropicResponseBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicEvent struct {
	Type         string                  `json:"type"`
	Index        int                     `json:"index,omitempty"`
	Message      *anthropicStreamMessage `json:"message,omitempty"`
	ContentBlock *anthropicContentBlock  `json:"content_block,omitempty"`
	Delta        *anthropicDelta         `json:"delta,omitempty"`
	Usage        *anthropicEventUsage    `json:"usage,omitempty"`
	StopReason   string                  `json:"stop_reason,omitempty"`
}

type anthropicStreamMessage struct {
	ID    string              `json:"id"`
	Usage anthropicEventUsage `json:"usage"`
}

type anthropicDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
	Signature   string `json:"signature,omitempty"`
}

type anthropicEventUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	CacheCreationInput int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInput     int `json:"cache_read_input_tokens,omitempty"`
}

// SSE helpers

func readLine(r io.Reader) (string, error) {
	var buf bytes.Buffer

	for {
		b := make([]byte, 1)

		_, err := r.Read(b)
		if err != nil {
			return "", err
		}

		if b[0] == '\n' {
			break
		}

		if b[0] != '\r' {
			buf.Write(b)
		}
	}

	return buf.String(), nil
}

func parseSSEData(line string) (string, error) {
	if !strings.HasPrefix(line, "data:") {
		return "", errors.New("not SSE data")
	}

	return strings.TrimSpace(strings.TrimPrefix(line, "data:")), nil
}

// normalizeAnthropicToolCallIDNormalizer matches the ToolCallIDNormalizer
// signature so TransformContext can invoke it. The `targetModel` and
// `source` arguments are ignored — the anthropic-messages format only
// carries a single tool ID per call (no `call_id|item_id` split), and
// the only transformations required are provider-agnostic sanitization
// (strip composite-id prefix, replace disallowed characters, clamp to
// the 64-byte limit).
func normalizeAnthropicToolCallIDNormalizer(id string, _ *Model, _ *AssistantMessage) string {
	return normalizeAnthropicToolCallID(id)
}

// normalizeAnthropicToolCallID strips the `call_id|item_id` composite used
// by the OpenAI Responses API, then runs the resulting id through the same
// sanitizer the Responses-family normalizers use (sanitizeToolCallID with
// underscore substitution) so disallowed characters are consistently
// replaced with `_` across every Anthropic-compatible provider that follows
// the ^[a-zA-Z0-9_-]{1,64}$ rule. The function also clamps the result to
// the 64-byte limit. Both sides of the tool_call/tool_result pairing run
// through this same function, so the resulting IDs always match.
func normalizeAnthropicToolCallID(id string) string {
	if strings.Contains(id, "|") {
		parts := strings.SplitN(id, "|", 2)
		id = parts[0]
	}
	return normalizeAnthropicToolCallIDOnly(id)
}

// normalizeAnthropicToolCallIDOnly sanitizes a single tool-call ID. It is
// split out from normalizeAnthropicToolCallID so the dual-defense pass in
// buildRequest can apply the same per-side transformation without redoing
// the composite-id strip.
func normalizeAnthropicToolCallIDOnly(id string) string {
	return sanitizeToolCallID(id, '_')
}

func init() {
	Register(APIAnthropicMessages, &AnthropicProvider{})
}
