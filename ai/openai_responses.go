package ai

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
)

// ConvertResponsesMessagesOptions configures shared Responses message
// conversion.
type ConvertResponsesMessagesOptions struct {
	IncludeSystemPrompt bool
}

// ConvertResponsesToolsOptions configures shared Responses tool conversion.
type ConvertResponsesToolsOptions struct {
	Strict *bool
}

// =============================================================================
// OpenAI Responses Provider
// =============================================================================

// OpenAIResponsesProvider handles the OpenAI Responses API.
type OpenAIResponsesProvider struct{}

// API implements Provider.
func (p *OpenAIResponsesProvider) API() string {
	return APIOpenAIResponses
}

// IsAlias implements Provider.
func (p *OpenAIResponsesProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *OpenAIResponsesProvider) NewClient(model *Model) (Client, error) {
	return &openAIResponsesClient{model: model}, nil
}

type openAIResponsesClient struct {
	model *Model
}

// Complete implements Client.
func (c *openAIResponsesClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

// Stream implements Client.Streamer.
func (c *openAIResponsesClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) (err error) {
	resolvedOptions, resolveErr := resolveRequestOptions(ctx, model, withCopilotHeaders(model, prompt, opts))
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

	// Get API key
	apiKey := o.apiKey
	if apiKey == "" {
		apiKey = o.envAPIKey(model.Provider)
		if apiKey == "" {
			return errors.New("openai responses: missing API key")
		}
	}

	// Build URL
	baseURL := cmp.Or(requestBaseURL(model, apiKey), model.BaseURL, "https://api.openai.com/v1")
	baseURL = strings.TrimSuffix(baseURL, "/")

	url := baseURL + "/responses"

	// Build request body
	body, err := buildOpenAIResponsesBody(model, prompt, &o)
	if err != nil {
		return fmt.Errorf("openai responses: build request: %w", err)
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
		return fmt.Errorf("openai responses: marshal request: %w", err)
	}

	resp, err := sendProviderRequest(ctx, &o, providerRequest{label: "openai responses", send: func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		for k, v := range sessionHeaders(model, &o) {
			req.Header.Set(k, v)
		}
		for k, v := range o.headers {
			req.Header.Set(k, v)
		}
		return doHeaderRequest(req, o.timeout, "openai responses")
	}})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	if hasResponseCallback(&o) {
		invokeResponseCallback(&o, model, responseMetadata(resp))
	}

	// Initialize output message
	output := AssistantMessage{
		Role:      "assistant",
		Content:   []ContentBlock{},
		API:       model.API,
		Provider:  model.Provider,
		Model:     model.ID,
		Timestamp: timestamp(),
		Usage: Usage{
			Input:       0,
			Output:      0,
			CacheRead:   0,
			CacheWrite:  0,
			TotalTokens: 0,
			Cost:        Cost{Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0, Total: 0},
		},
		StopReason: StopReasonStop,
	}

	// Stream response using shared processor
	grammarInputs, err := responsesGrammarInputs(model, prompt.Tools)
	if err != nil {
		return err
	}
	streamOpts := &ResponsesStreamOptions{
		ServiceTier:             o.serviceTier,
		ApplyServiceTierPricing: ServiceTierPricing,
		GrammarInputs:           grammarInputs,
	}

	reader := newResponsesStreamReader(resp.Body)
	return ProcessResponsesStream(ctx, reader, &output, handler, model, streamOpts)
}

// =============================================================================
// Request Body Building
// =============================================================================

// buildOpenAIResponsesBody builds the OpenAI Responses API request body.
func buildOpenAIResponsesBody(model *Model, prompt *Context, o *options) (map[string]any, error) {
	messages, err := ConvertResponsesMessagesForOpenAI(model, prompt)
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"model":  model.ID,
		"input":  messages,
		"stream": true,
		"store":  false,
	}

	retention := resolveCacheRetention(o.cacheRetention)
	// Add session/caching
	if o.sessionID != "" && retention != CacheRetentionNone {
		body["prompt_cache_key"] = clampOpenAIPromptCacheKey(o.sessionID)
	}

	baseURL := cmp.Or(model.BaseURL, "https://api.openai.com/v1")
	apiKey := cmp.Or(o.apiKey, o.envAPIKey(model.Provider))
	chatGPTSignIn := model.Provider == ProviderOpenAI && baseURL == "https://api.openai.com/v1" && apiKey != "" && !strings.HasPrefix(apiKey, "sk-")
	longCache := model.Compat == nil || model.Compat.SupportsLongCacheRetention == nil || *model.Compat.SupportsLongCacheRetention
	explicitCache := model.Compat != nil && model.Compat.SupportsExplicitPromptCacheMode
	if !chatGPTSignIn {
		switch {
		case explicitCache && retention == CacheRetentionNone:
			body["prompt_cache_options"] = map[string]any{"mode": "explicit"}
		case retention == CacheRetentionLong && longCache:
			if explicitCache {
				body["prompt_cache_options"] = map[string]any{"ttl": "30m"}
			} else {
				body["prompt_cache_retention"] = "24h"
			}
		}
	}

	maxOutputTokens := model.Compat == nil || model.Compat.SupportsMaxOutputTokens == nil || *model.Compat.SupportsMaxOutputTokens
	if o.maxTokens > 0 && maxOutputTokens && !chatGPTSignIn {
		// Responses rejects output limits below 16 tokens.
		body["max_output_tokens"] = max(o.maxTokens, 16)
	}

	if o.temperature != nil && !chatGPTSignIn {
		body["temperature"] = *o.temperature
	}

	// Add service tier
	if o.serviceTier != "" {
		body["service_tier"] = o.serviceTier
	}

	// Add tools
	if len(prompt.Tools) > 0 {
		tools, err := responsesTools(model, prompt.Tools, supportsStrictMode(model, false))
		if err != nil {
			return nil, err
		}
		body["tools"] = tools
	}

	if model.Reasoning {
		effort := responsesReasoningEffort(model, o)
		if effort != "" || o.reasoningSummary != "" {
			body["reasoning"] = map[string]any{
				"effort":  defaultString(effort, "medium"),
				"summary": defaultString(o.reasoningSummary, "auto"),
			}
			body["include"] = []string{"reasoning.encrypted_content"}
		} else if off, ok := responsesOffEffort(model); ok && model.Provider != ProviderGitHubCopilot {
			body["reasoning"] = map[string]any{"effort": off}
		}
	}

	// Last so model and request sampling parameters override named fields.
	maps.Copy(body, resolveSamplingParams(model, o))

	return body, nil
}

// =============================================================================
// Message Conversion
// =============================================================================

// ConvertResponsesMessagesForOpenAI converts Context messages to OpenAI Responses format.
// This is the shared message conversion used by OpenAI Responses, Codex, and Azure.
func ConvertResponsesMessagesForOpenAI(model *Model, prompt *Context) ([]map[string]any, error) {
	return ConvertResponsesMessages(model, prompt, openAIResponsesToolCallProviders, &ConvertResponsesMessagesOptions{
		IncludeSystemPrompt: true,
	})
}

// ConvertResponsesMessages exposes the shared Responses-family message
// conversion with configurable tool-call provider normalization and system
// prompt inclusion.
func ConvertResponsesMessages(model *Model, prompt *Context, allowedToolCallProviders map[string]struct{}, opts *ConvertResponsesMessagesOptions) ([]map[string]any, error) {
	// Transform messages using TransformContext (handles thinking blocks, tool results, etc.)
	transformedPrompt := prompt
	if transformed, err := TransformContext(prompt, model, openAIResponsesToolCallIDNormalizerWithProviders(model, allowedToolCallProviders)); err == nil {
		transformedPrompt = transformed
	}

	grammar, err := responsesGrammarInputs(model, prompt.Tools)
	if err != nil {
		return nil, err
	}
	messages := []map[string]any{}

	// Add system prompt as developer/system message
	includeSystemPrompt := opts == nil || opts.IncludeSystemPrompt
	if includeSystemPrompt && transformedPrompt.SystemPrompt != "" {
		role := "system"
		if model.Reasoning {
			role = "developer"
		}
		messages = append(messages, map[string]any{
			"role":    role,
			"content": SanitizeSurrogates(transformedPrompt.SystemPrompt),
		})
	}

	msgIndex := 0
	for _, msg := range transformedPrompt.Messages {
		converted, err := convertMessageForOpenAIResponses(model, msg, msgIndex, grammar)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
		msgIndex++
	}
	return messages, nil
}

// convertMessageForOpenAIResponses converts a single Message to OpenAI Responses format.
// grammar maps grammar tools to their input property; their calls replay as
// custom tool calls.
func convertMessageForOpenAIResponses(model *Model, msg Message, msgIndex int, grammar map[string]string) ([]map[string]any, error) {
	switch msg.Role {
	case "user":
		return convertUserMessageForResponses(model, msg), nil

	case "assistant":
		return convertAssistantMessageForResponses(model, msg, msgIndex, grammar)

	case "tool", "toolResult":
		items := convertToolResultMessageForResponses(model, msg)
		if _, ok := grammar[msg.ToolName]; ok {
			for _, item := range items {
				item["type"] = "custom_tool_call_output"
			}
		}
		return items, nil

	default:
		return []map[string]any{}, nil
	}
}

// convertUserMessageForResponses converts a user message to OpenAI Responses format.
func convertUserMessageForResponses(model *Model, msg Message) []map[string]any {
	var content []map[string]any

	if contentStr, ok := msg.ContentString(); ok {
		content = append(content, map[string]any{
			"type": "input_text",
			"text": SanitizeSurrogates(contentStr),
		})
	} else {
		blocks := msg.ContentBlocks()
		for _, block := range blocks {
			switch block.Type {
			case ContentBlockText:
				content = append(content, map[string]any{
					"type": "input_text",
					"text": SanitizeSurrogates(block.Text),
				})
			case ContentBlockImage:
				content = append(content, map[string]any{
					"type":      "input_image",
					"detail":    "auto",
					"image_url": fmt.Sprintf("data:%s;base64,%s", block.MimeType, block.Data),
				})
			}
		}
	}

	// Filter images for non-image models
	if !model.SupportsInput(InputImage) {
		filtered := []map[string]any{}
		for _, c := range content {
			if c["type"] != "input_image" {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) == 0 {
			return []map[string]any{}
		}
		content = filtered
	}

	return []map[string]any{
		{
			"role":    "user",
			"content": content,
		},
	}
}

// convertAssistantMessageForResponses converts an assistant message to OpenAI Responses format.
func convertAssistantMessageForResponses(model *Model, msg Message, msgIndex int, grammar map[string]string) ([]map[string]any, error) {
	var items []map[string]any

	blocks := msg.ContentBlocks()
	for _, block := range blocks {
		switch block.Type {
		case ContentBlockThinking:
			// Include reasoning items if there's a signature
			if block.ThoughtSignature != "" {
				var reasoningItem map[string]any
				if err := json.Unmarshal([]byte(block.ThoughtSignature), &reasoningItem); err == nil {
					items = append(items, reasoningItem)
				}
			}

		case ContentBlockText:
			parsedSig := parseTextSignatureV1(block.TextSignature)
			msgID := ""
			phase := ""
			if parsedSig != nil {
				msgID = parsedSig.ID
				phase = parsedSig.Phase
			}

			if msgID == "" {
				msgID = fmt.Sprintf("msg_%d", msgIndex)
			} else if len(msgID) > 64 {
				msgID = fmt.Sprintf("msg_%s", ShortHash(msgID))
			}

			messageItem := map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{
						"type":        "output_text",
						"text":        SanitizeSurrogates(block.Text),
						"annotations": []any{},
					},
				},
				"status": "completed",
				"id":     msgID,
			}
			if phase != "" {
				messageItem["phase"] = phase
			}
			items = append(items, messageItem)

		case ContentBlockToolCall:
			// Parse tool call ID (format: call_id|item_id)
			parts := strings.SplitN(block.ID, "|", 2)
			callID := block.ID
			itemID := ""
			if len(parts) == 2 {
				callID = parts[0]
				itemID = parts[1]
			}

			isDifferentModel := msg.Model != "" &&
				msg.Model != model.ID &&
				msg.Provider == model.Provider &&
				msg.API == model.API

			// Different-model item ids trip pairing validation, and an id must
			// match its item type: fc_ for function calls, ctc_ for custom
			// ones. A call switches type when grammar tool support differs.
			property, custom := grammar[block.Name]
			prefix := "fc_"
			if custom {
				prefix = "ctc_"
			}
			if isDifferentModel || !strings.HasPrefix(itemID, prefix) {
				itemID = ""
			}

			call := map[string]any{
				"type":      "function_call",
				"call_id":   callID,
				"name":      block.Name,
				"arguments": string(block.Arguments),
			}
			if custom {
				input, err := grammarToolInput(block.Name, block.Arguments, property)
				if err != nil {
					return nil, err
				}
				delete(call, "arguments")
				call["type"] = "custom_tool_call"
				call["input"] = SanitizeSurrogates(input)
			}
			if itemID != "" {
				call["id"] = itemID
			}
			if msg.Provider == model.Provider && msg.API == model.API && msg.Model == model.ID && block.Namespace != nil {
				call["namespace"] = *block.Namespace
			}
			items = append(items, call)
		}
	}

	if len(items) == 0 {
		return []map[string]any{}, nil
	}

	return items, nil
}

// convertToolResultMessageForResponses converts a tool result message to OpenAI Responses format.
func convertToolResultMessageForResponses(model *Model, msg Message) []map[string]any {
	textParts := []string{}
	var hasImages bool

	blocks := msg.ContentBlocks()
	for _, block := range blocks {
		switch block.Type {
		case ContentBlockText:
			textParts = append(textParts, block.Text)
		case ContentBlockImage:
			hasImages = true
		}
	}

	hasText := len(textParts) > 0
	text := strings.Join(textParts, "\n")
	if !hasText {
		text = "(see attached image)"
	}

	// Get call_id from toolCallId
	callID := msg.ToolCallID
	if callID == "" && msg.ID != "" {
		// Extract call_id from composite ID
		callID, _, _ = strings.Cut(msg.ID, "|")
	}
	callID, _, _ = strings.Cut(callID, "|")
	if strings.TrimSpace(callID) == "" {
		return nil
	}

	// Handle images with text
	if hasImages && model.SupportsInput(InputImage) {
		content := []map[string]any{}
		if hasText {
			content = append(content, map[string]any{
				"type": "input_text",
				"text": SanitizeSurrogates(text),
			})
		}
		for _, block := range blocks {
			if block.Type == ContentBlockImage {
				content = append(content, map[string]any{
					"type":      "input_image",
					"detail":    "auto",
					"image_url": fmt.Sprintf("data:%s;base64,%s", block.MimeType, block.Data),
				})
			}
		}
		return []map[string]any{
			{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  content,
			},
		}
	}

	return []map[string]any{
		{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  SanitizeSurrogates(text),
		},
	}
}

// =============================================================================
// Tool Call ID Normalization
// =============================================================================

var openAIResponsesToolCallProviders = map[string]struct{}{
	ProviderOpenAI:      {},
	ProviderOpenAICodex: {},
	ProviderOpenCode:    {},
}

func openAIResponsesToolCallIDNormalizer(model *Model) ToolCallIDNormalizer {
	return openAIResponsesToolCallIDNormalizerWithProviders(model, openAIResponsesToolCallProviders)
}

func openAIResponsesToolCallIDNormalizerWithProviders(model *Model, allowedProviders map[string]struct{}) ToolCallIDNormalizer {
	return func(id string, targetModel *Model, source *AssistantMessage) string {
		normalizePart := func(part string) string {
			// Trimming after truncation is equivalent to the original
			// trim-truncate-trim: the leading trim only ever removed
			// characters past the 64-byte cut.
			return strings.TrimRight(sanitizeToolCallID(part, '_'), "_")
		}

		buildForeignResponsesItemID := func(itemID string) string {
			normalized := "fc_" + ShortHash(itemID)
			if len(normalized) > 64 {
				return normalized[:64]
			}
			return normalized
		}

		if _, ok := allowedProviders[targetModel.Provider]; !ok {
			return normalizePart(id)
		}
		if !strings.Contains(id, "|") {
			return normalizePart(id)
		}

		parts := strings.SplitN(id, "|", 2)
		callID := normalizePart(parts[0])
		itemID := parts[1]
		isForeignToolCall := source != nil && (source.Provider != targetModel.Provider || source.API != targetModel.API)
		if isForeignToolCall {
			itemID = buildForeignResponsesItemID(itemID)
		} else {
			itemID = normalizePart(itemID)
		}
		if !strings.HasPrefix(itemID, "fc_") {
			itemID = normalizePart("fc_" + itemID)
		}
		return callID + "|" + itemID
	}
}

// =============================================================================
// Tool Conversion
// =============================================================================

// responsesGrammarInputs maps the request's grammar tools to their input
// property when the model accepts OpenAI grammar tools.
func responsesGrammarInputs(model *Model, tools []Tool) (map[string]string, error) {
	return grammarInputProperties(tools, model.Compat != nil && model.Compat.SupportsOpenAIGrammarTools)
}

// supportsStrictMode reports the model's strict-mode compat flag, or fallback
// when the catalog leaves it unset.
func supportsStrictMode(model *Model, fallback bool) bool {
	if model.Compat != nil && model.Compat.SupportsStrictMode != nil {
		return *model.Compat.SupportsStrictMode
	}
	return fallback
}

// responsesTools converts request tools for the Responses family. Grammar
// tools become custom tools when the model accepts them, and strict is sent
// only when the provider supports strict mode.
func responsesTools(model *Model, tools []Tool, strictSupported bool) ([]map[string]any, error) {
	grammarSupported := model.Compat != nil && model.Compat.SupportsOpenAIGrammarTools
	result := make([]map[string]any, len(tools))
	for i, tool := range tools {
		grammar, err := resolveGrammarTool(tool, grammarSupported)
		if err != nil {
			return nil, err
		}
		if grammar != nil {
			result[i] = map[string]any{
				"type":        "custom",
				"name":        tool.Name,
				"description": tool.Description,
				"format":      map[string]any{"type": "grammar", "syntax": grammar.syntax, "definition": grammar.definition},
			}
			continue
		}
		parameters, strict, err := strictToolParameters(tool, strictSupported, nil)
		if err != nil {
			return nil, err
		}
		result[i] = map[string]any{
			"type":        "function",
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  parameters,
		}
		if strictSupported {
			result[i]["strict"] = strict
		}
	}
	return result, nil
}

// ConvertResponsesTools converts tools to OpenAI Responses format.
func ConvertResponsesTools(tools []Tool) []map[string]any {
	return ConvertResponsesToolsWithOptions(tools, nil)
}

// ConvertResponsesToolsWithOptions exposes shared Responses-family tool
// conversion with configurable strict-mode handling.
func ConvertResponsesToolsWithOptions(tools []Tool, opts *ConvertResponsesToolsOptions) []map[string]any {
	strict := false
	if opts != nil && opts.Strict != nil {
		strict = *opts.Strict
	}

	result := make([]map[string]any, len(tools))
	for i, tool := range tools {
		result[i] = map[string]any{
			"type":        "function",
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  tool.Parameters,
			"strict":      strict,
		}
	}
	return result
}

// =============================================================================
// Signature Utilities
// =============================================================================

// textSignatureV1 represents the V1 text signature format.
type textSignatureV1 struct {
	V     int    `json:"v"`
	ID    string `json:"id"`
	Phase string `json:"phase,omitempty"`
}

func encodeTextSignatureV1(id, phase string) string {
	payload := textSignatureV1{V: 1, ID: id}
	if phase == "commentary" || phase == "final_answer" {
		payload.Phase = phase
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(data)
}

// parseTextSignatureV1 parses a text signature V1 JSON string.
func parseTextSignatureV1(sig string) *textSignatureV1 {
	if sig == "" {
		return nil
	}
	if !strings.HasPrefix(sig, "{") {
		return &textSignatureV1{ID: sig}
	}
	var parsed textSignatureV1
	if err := json.Unmarshal([]byte(sig), &parsed); err != nil {
		return nil
	}
	if parsed.V != 1 || parsed.ID == "" {
		return nil
	}
	if parsed.Phase != "" && parsed.Phase != "commentary" && parsed.Phase != "final_answer" {
		parsed.Phase = ""
	}
	return &parsed
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
