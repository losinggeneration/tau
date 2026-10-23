package ai // import "github.com/losinggeneration/tau/ai"

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	ProviderLlamaCpp:   DefaultLlamaServerURL + "/v1",
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
	// Azure endpoints come from the environment first; Stream resolves them per request.
	if baseURL == "" && model.Provider != ProviderAzureOpenAI {
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

	// Build request
	reqBody, err := c.buildRequest(model, prompt, &o)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	transport := c.transport
	if model.Provider == ProviderAzureOpenAI {
		baseURL, _, deployment, err := resolveAzureConfig(model, &o)
		if err != nil {
			return err
		}
		azure := *transport
		azure.baseURL = strings.TrimSuffix(baseURL, "/")
		transport = &azure
		if reqBody, err = withRequestModel(reqBody, deployment); err != nil {
			return err
		}
	}

	if hasPayloadCallback(&o) {
		if next := invokePayloadCallback(&o, model, reqBody); next != nil {
			if s, ok := next.(string); ok {
				reqBody = s
			}
		}
	}

	o.transportOpts = cmp.Or(o.transportOpts, c.transportOpts)
	resp, err := doOpenAIChatCompletions(ctx, model, &o, transport.withRequestOptions(model, &o), reqBody)
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
	grammar, err := grammarInputProperties(prompt.Tools, model.GetCompat().SupportsOpenAIGrammarTools)
	if err != nil {
		return err
	}
	return streamOpenAIResponseWithAPI(ctx, c.model, resp.Body, handler, grammar, c.model.API, c.model.Provider)
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

	if err := applyOpenAIRequestTools(req, transformedCtx, compat); err != nil {
		return nil, err
	}
	if err := applyOpenAIRequestOptions(req, model, o, compat); err != nil {
		return nil, err
	}
	applyCompletionsCacheControl(req, compat, resolveCacheRetention(o.cacheRetention))
	return req, nil
}

func applyOpenAIRequestTools(req *openAIRequest, prompt *Context, compat *OpenAICompletionsCompat) error {
	if len(prompt.Tools) > 0 {
		req.Tools = make([]toolDefinition, len(prompt.Tools))
		for i, t := range prompt.Tools {
			grammar, err := resolveGrammarTool(t, compat.SupportsOpenAIGrammarTools)
			if err != nil {
				return err
			}
			if grammar != nil {
				custom := &customToolDefinition{Name: t.Name, Description: t.Description, Format: customToolGrammar{Type: "grammar"}}
				custom.Format.Grammar.Syntax = grammar.syntax
				custom.Format.Grammar.Definition = grammar.definition
				req.Tools[i] = toolDefinition{Type: "custom", Custom: custom}
				continue
			}
			def := toolDefinition{
				Type:     "function",
				Function: toolToFunctionDefinition(t),
			}
			parameters, strict, err := strictToolParameters(t, compat.SupportsStrictMode, nil)
			if err != nil {
				return err
			}
			def.Function.Parameters = parameters
			if compat.SupportsStrictMode {
				def.Function.Strict = new(strict)
			}
			req.Tools[i] = def
		}
		if compat.ZAIToolStream {
			req.ToolStream = true
		}
		return nil
	}

	if hasToolHistory(prompt.Messages) {
		req.Tools = []toolDefinition{}
	}
	return nil
}

func applyOpenAIRequestOptions(req *openAIRequest, model *Model, o *options, compat *OpenAICompletionsCompat) error {
	req.Temperature = o.temperature
	req.TopP = o.topP
	req.SamplingParams = resolveSamplingParams(model, o)

	if o.toolChoice != nil {
		req.ToolChoice = o.toolChoice
	}

	applyOpenAIMaxTokens(req, model, o, compat)
	if err := applyOpenAIReasoningOptions(req, model, o, compat); err != nil {
		return err
	}

	if compat.SupportsStore {
		req.Store = new(false)
	}

	retention := resolveCacheRetention(o.cacheRetention)
	baseURL := model.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURLs[model.Provider]
	}
	if strings.Contains(baseURL, "api.openai.com") && retention != CacheRetentionNone || retention == CacheRetentionLong && compat.SupportsLongCacheRetention {
		req.PromptCacheKey = clampOpenAIPromptCacheKey(o.sessionID)
	}
	if retention == CacheRetentionLong && compat.SupportsLongCacheRetention {
		req.PromptCacheRetention = "24h"
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
	return nil
}

// applyOpenAIReasoningOptions sends the request's thinking level in the
// model's thinking format, mirroring upstream openai-completions.
func applyOpenAIReasoningOptions(req *openAIRequest, model *Model, o *options, compat *OpenAICompletionsCompat) error {
	if !model.Reasoning {
		return nil
	}
	var effort string
	if o.reasoning != "" && o.reasoning != ReasoningOff {
		if effort = ClampThinkingLevel(model, string(o.reasoning)); effort == "off" {
			effort = ""
		}
	}
	// A bare WithThinkingEnabled turns thinking on without choosing a level.
	enabled := effort != "" || o.thinkingEnabled && o.reasoning == ""
	offValue, offMapped := model.ThinkingLevelMap["off"]
	offSupported := !offMapped || offValue != nil
	// mapped is the model's value for the effort, falling back to the effort
	// itself; ok is false when the thinking map marks it unsupported.
	mapped := func() (string, bool) {
		if value, exists := model.ThinkingLevelMap[effort]; exists {
			if value == nil {
				return "", false
			}
			return *value, true
		}
		return mapReasoningEffort(ReasoningLevel(effort), compat.ReasoningEffortMap), true
	}
	mappedOr := func() string {
		if value, ok := mapped(); ok {
			return value
		}
		return effort
	}
	// leveled is the mapped effort, or the mapped off value when reasoning
	// is off; ok is false when neither yields a string.
	leveled := func() (string, bool) {
		if effort != "" {
			return mapped()
		}
		if offValue != nil {
			return *offValue, true
		}
		return "", false
	}
	budget := openAIThinkingBudget(req, model, o, effort)

	switch compat.ThinkingFormat {
	case ThinkingFormatZAI:
		if enabled {
			req.Thinking = map[string]any{"type": "enabled", "clear_thinking": false}
		} else {
			req.Thinking = map[string]any{"type": "disabled"}
		}
		if value, ok := mapped(); ok && effort != "" && compat.SupportsReasoningEffort {
			req.ReasoningEffort = value
		}
	case ThinkingFormatQwen:
		req.EnableThinking = boolPtr(enabled)
		if effort != "" && compat.SupportsReasoningEffort {
			req.ReasoningEffort = mappedOr()
		}
	case ThinkingFormatQwenChatTmpl:
		req.ChatTemplateKwargs = map[string]any{"enable_thinking": enabled, "preserve_thinking": true}
	case ThinkingFormatChatTemplate, ThinkingFormatBaseten:
		raw := json.RawMessage(nil)
		if model.Compat != nil {
			raw = model.Compat.ChatTemplateKwargs
			if compat.ThinkingFormat == ThinkingFormatBaseten {
				raw = model.Compat.ChatTemplateArgs
			}
		}
		values, err := resolveChatTemplateValues(raw, effort, budget, leveled)
		if err != nil {
			return err
		}
		if compat.ThinkingFormat == ThinkingFormatChatTemplate {
			req.ChatTemplateKwargs = values
			break
		}
		req.ChatTemplateArgs = values
		if value, ok := leveled(); ok && compat.SupportsReasoningEffort {
			req.ReasoningEffort = value
		}
	case ThinkingFormatDeepSeek:
		if enabled {
			req.Thinking = map[string]any{"type": "enabled"}
		} else if offSupported {
			req.Thinking = map[string]any{"type": "disabled"}
		}
		if effort != "" && compat.SupportsReasoningEffort {
			req.ReasoningEffort = mappedOr()
		}
	case ThinkingFormatOpenRouter:
		if effort != "" {
			req.Reasoning = map[string]any{"effort": mappedOr()}
		} else if offSupported {
			req.Reasoning = map[string]any{"effort": cmp.Or(deref(offValue), "none")}
		}
	case ThinkingFormatAntLing:
		if value := model.ThinkingLevelMap[effort]; effort != "" && value != nil {
			req.Reasoning = map[string]any{"effort": *value}
		}
	case ThinkingFormatTogether:
		req.Reasoning = map[string]any{"enabled": enabled}
		if effort != "" && compat.SupportsReasoningEffort {
			req.ReasoningEffort = mappedOr()
		}
	case ThinkingFormatString:
		if effort != "" {
			req.Thinking = mappedOr()
		} else if offSupported {
			req.Thinking = cmp.Or(deref(offValue), "none")
		}
	default:
		if effort != "" && compat.SupportsReasoningEffort {
			req.ReasoningEffort = mappedOr()
		} else if offValue != nil && compat.SupportsReasoningEffort {
			req.ReasoningEffort = *offValue
		}
	}

	if field := thinkingTokenBudgetField(model.Compat); field != "" && budget > 0 {
		req.ThinkingBudget = map[string]int{field: budget}
	}
	return nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// thinkingTokenBudgetField is the top-level request field that caps
// reasoning tokens, or "" when the model has none.
func thinkingTokenBudgetField(compat *CompatibilitySettings) string {
	switch {
	case compat == nil:
		return ""
	case compat.ThinkingTokenBudgetField != "":
		return compat.ThinkingTokenBudgetField
	case compat.SupportsThinkingTokenBudget:
		return "thinking_token_budget"
	default:
		return ""
	}
}

// openAIThinkingBudget caps the level's thinking budget so the answer keeps
// room under the request's token ceiling; zero means none applies.
func openAIThinkingBudget(req *openAIRequest, model *Model, o *options, effort string) int {
	if effort == "" {
		return 0
	}
	budget := o.thinkingBudgetTokens
	if budget <= 0 {
		budget = ThinkingBudgetForLevel(ReasoningLevel(effort), o.thinkingBudgets)
	}
	ceiling := cmp.Or(req.MaxTokens, req.MaxCompletionTokens, model.MaxTokens)
	return min(budget, max(0, ceiling-minAnswerTokens))
}

// resolveChatTemplateValues fills configured chat-template values, replacing
// {"$var": ...} references with the request's thinking state.
func resolveChatTemplateValues(raw json.RawMessage, effort string, budget int, leveled func() (string, bool)) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("parse chat template values: %w", err)
	}
	resolved := make(map[string]any, len(values))
	for key, value := range values {
		ref, ok := value.(map[string]any)
		if !ok {
			resolved[key] = value
			continue
		}
		if omit, _ := ref["omitWhenOff"].(bool); omit && effort == "" {
			continue
		}
		switch ref["$var"] {
		case "thinking.enabled":
			resolved[key] = effort != ""
		case "thinking.budget":
			if budget > 0 {
				resolved[key] = budget
			}
		case "thinking.effort":
			if value, ok := leveled(); ok {
				resolved[key] = value
			}
		default:
			return nil, fmt.Errorf("chat template value %q: unknown $var %v", key, ref["$var"])
		}
	}
	if len(resolved) == 0 {
		return nil, nil
	}
	return resolved, nil
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

func clampOpenAIPromptCacheKey(key string) string {
	chars := []rune(key)
	return string(chars[:min(len(chars), 64)])
}

func applyCompletionsCacheControl(req *openAIRequest, compat *OpenAICompletionsCompat, retention CacheRetention) {
	if compat.CacheControlFormat != "anthropic" || retention == CacheRetentionNone {
		return
	}
	control := &cacheControlParam{Type: "ephemeral"}
	if retention == CacheRetentionLong && compat.SupportsLongCacheRetention {
		control.TTL = "1h"
	}
	mark := func(message *openAIMessage) bool {
		switch content := message.Content.(type) {
		case string:
			if content != "" {
				message.Content = []map[string]any{{"type": "text", "text": content, "cache_control": control}}
				return true
			}
		case []map[string]any:
			for i := len(content) - 1; i >= 0; i-- {
				if content[i]["type"] == "text" {
					content[i]["cache_control"] = control
					return true
				}
			}
		}
		return false
	}
	for i := range req.Messages {
		if req.Messages[i].Role == "system" || req.Messages[i].Role == "developer" {
			mark(&req.Messages[i])
			break
		}
	}
	if len(req.Tools) > 0 {
		req.Tools[len(req.Tools)-1].CacheControl = control
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		switch req.Messages[i].Role {
		case "user", "assistant", "tool":
			if mark(&req.Messages[i]) {
				return
			}
		}
	}
}

func (c *openAIClient) convertMessages(prompt *Context, model *Model, compat *OpenAICompletionsCompat) ([]openAIMessage, error) {
	var msgs []openAIMessage
	grammar, err := grammarInputProperties(prompt.Tools, compat.SupportsOpenAIGrammarTools)
	if err != nil {
		return nil, err
	}

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

		msg, err := c.convertMessage(m, model, compat, grammar)
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
				toolResult, _ := c.convertMessage(toolMsg, model, compat, grammar)
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

// grammar maps grammar tools to their input property; their calls replay as
// custom tool calls.
func (c *openAIClient) convertMessage(m Message, model *Model, compat *OpenAICompletionsCompat, grammar map[string]string) (*openAIMessage, error) {
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
				if property, ok := grammar[b.Name]; ok {
					input, err := grammarToolInput(b.Name, b.Arguments, property)
					if err != nil {
						return nil, err
					}
					tc = toolCall{ID: b.ID, Type: "custom", Custom: &customToolInput{Name: b.Name, Input: SanitizeSurrogates(input)}}
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

		// Replay thinking in the reasoning field it streamed in.
		if len(thinkingBlocks) > 0 && !compat.RequiresThinkingAsText && len(reasonings) == 0 {
			field := thinkingBlocks[0].ThoughtSignature
			if model.Provider == ProviderOpenCodeGo && field == "reasoning" {
				field = "reasoning_content"
			}
			thinking := make([]string, 0, len(thinkingBlocks))
			for _, block := range thinkingBlocks {
				thinking = append(thinking, block.Thinking)
			}
			joined := strings.Join(thinking, "\n")
			switch field {
			case "reasoning_content":
				msg.ReasoningContent = &joined
			case "reasoning":
				msg.Reasoning = &joined
			case "reasoning_text":
				msg.ReasoningText = &joined
			}
		}
		if compat.RequiresReasoningContentOnAssistantMessages && model.Reasoning && msg.ReasoningContent == nil {
			msg.ReasoningContent = new("")
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
	return streamOpenAIResponseWithAPI(ctx, model, body, handler, nil, model.API, model.Provider)
}

// grammar maps grammar tools to the argument their custom tool input is
// stored in.
func streamOpenAIResponseWithAPI(ctx context.Context, model *Model, body io.Reader, handler EventHandler, grammar map[string]string, api, provider string) error {
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

	var hasFinishReason bool
	currentText := ""
	currentThinking := ""
	// currentThinkingField names the delta field the reasoning arrived in, so
	// replay can send it back the same way.
	var currentThinkingField string
	currentToolCall := (*ToolCall)(nil)
	currentToolCallSignature := ""
	// currentCustomInput is set while a custom (grammar) tool call streams.
	var currentCustomInput *grammarInputBuffer
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
		output.Content = append(output.Content, ContentBlock{Type: ContentBlockThinking, Thinking: currentThinking, ThoughtSignature: currentThinkingField})
		handler(ctx, ThinkingEnd{ContentIndex: contentIndex, Content: currentThinking, Partial: *output})
		contentIndex++
		currentThinking = ""
	}

	flushToolCall := func() {
		if currentToolCall == nil {
			return
		}
		if currentCustomInput != nil {
			if delta := currentCustomInput.finish(); delta != "" {
				currentToolCall.Arguments = currentCustomInput.arguments()
				handler(ctx, ToolCallDelta{ContentIndex: contentIndex, Delta: delta, Partial: *output})
			}
			currentCustomInput = nil
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
				currentThinkingField = choice.Delta.ReasoningField
				if model.Provider == ProviderOpenCodeGo && currentThinkingField == "reasoning" {
					currentThinkingField = "reasoning_content"
				}
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
						if tc.Custom != nil && tc.Function == (functionCall{}) {
							currentToolCall.Name = tc.Custom.Name
							property := grammar[tc.Custom.Name]
							if property == "" {
								property = "input"
							}
							currentCustomInput = newGrammarInputBuffer(property)
							currentToolCall.Arguments = currentCustomInput.arguments()
						}
						handler(ctx, ToolCallStart{ContentIndex: contentIndex, Partial: *output})
					}
					if currentCustomInput != nil && tc.Custom != nil && tc.Custom.Input != "" {
						delta, err := currentCustomInput.append(currentCustomInput.input+tc.Custom.Input, false)
						if err != nil {
							return fmt.Errorf("custom tool input: %w", err)
						}
						currentToolCall.Arguments = currentCustomInput.arguments()
						handler(ctx, ToolCallDelta{ContentIndex: contentIndex, Delta: delta, Partial: *output})
					} else if tc.Function.Arguments != "" {
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
				hasFinishReason = true
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

	if !hasFinishReason && !compat.SupportsFinishReason {
		hasFinishReason = true
		output.StopReason = StopReasonStop
		if hasToolCalls(output.Content) {
			output.StopReason = StopReasonToolUse
		}
	}
	finishStream(ctx, handler, *output, hasFinishReason, "stream ended without finish_reason")

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
	PromptCacheKey       string           `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string           `json:"prompt_cache_retention,omitempty"`
	Temperature          *float64         `json:"temperature,omitempty"`
	TopP                 *float64         `json:"top_p,omitempty"`
	Model                string           `json:"model"`
	Messages             []openAIMessage  `json:"messages"`
	Tools                []toolDefinition `json:"tools,omitempty"`
	Metadata             map[string]any   `json:"metadata,omitempty"`
	Store                *bool            `json:"store,omitempty"`
	MaxTokens            int              `json:"max_tokens,omitempty"`
	MaxCompletionTokens  int              `json:"max_completion_tokens,omitempty"`
	ReasoningEffort      string           `json:"reasoning_effort,omitempty"`
	EnableThinking       *bool            `json:"enable_thinking,omitempty"`
	Thinking             any              `json:"thinking,omitempty"`
	ChatTemplateKwargs   map[string]any   `json:"chat_template_kwargs,omitempty"`
	ChatTemplateArgs     map[string]any   `json:"chat_template_args,omitempty"`
	Reasoning            map[string]any   `json:"reasoning,omitempty"`
	Provider             any              `json:"provider,omitempty"`
	ProviderOptions      map[string]any   `json:"provider_options,omitempty"`
	ToolChoice           any              `json:"tool_choice,omitempty"`
	ToolStream           bool             `json:"tool_stream,omitempty"`
	Stream               bool             `json:"stream"`
	StreamOptions        *streamOptions   `json:"stream_options,omitempty"`
	// ThinkingBudget is sent under a compat-configured top-level field.
	ThinkingBudget map[string]int `json:"-"`
	// SamplingParams are merged into the body last, so they override the
	// named fields.
	SamplingParams map[string]any `json:"-"`
}

func (r openAIRequest) MarshalJSON() ([]byte, error) {
	type plain openAIRequest
	data, err := json.Marshal(plain(r))
	if err != nil || len(r.SamplingParams) == 0 && len(r.ThinkingBudget) == 0 {
		return data, err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	extra := make(map[string]any, len(r.ThinkingBudget)+len(r.SamplingParams))
	for key, value := range r.ThinkingBudget {
		extra[key] = value
	}
	maps.Copy(extra, r.SamplingParams)
	for key, value := range extra {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("sampling parameter %q: %w", key, err)
		}
		body[key] = encoded
	}
	return json.Marshal(body)
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
	ReasoningContent *string          `json:"reasoning_content,omitempty"`
	Reasoning        *string          `json:"reasoning,omitempty"`
	ReasoningText    *string          `json:"reasoning_text,omitempty"`
}

type toolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function functionCall     `json:"function,omitzero"`
	Custom   *customToolInput `json:"custom,omitempty"`
}

// customToolInput is a custom (grammar) tool call; streamed deltas carry
// input fragments.
type customToolInput struct {
	Name  string `json:"name,omitempty"`
	Input string `json:"input"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolDefinition struct {
	CacheControl *cacheControlParam    `json:"cache_control,omitempty"`
	Type         string                `json:"type"`
	Function     functionDefinition    `json:"function,omitzero"`
	Custom       *customToolDefinition `json:"custom,omitempty"`
}

type customToolDefinition struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Format      customToolGrammar `json:"format"`
}

type customToolGrammar struct {
	Type    string `json:"type"`
	Grammar struct {
		Syntax     string `json:"syntax"`
		Definition string `json:"definition"`
	} `json:"grammar"`
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
	ReasoningField   string                   `json:"-"` // which of those fields carried it
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
		d.ReasoningContent, d.ReasoningField = tmp.ReasoningContent, "reasoning_content"
	case tmp.Reasoning != "":
		d.ReasoningContent, d.ReasoningField = tmp.Reasoning, "reasoning"
	case tmp.ReasoningText != "":
		d.ReasoningContent, d.ReasoningField = tmp.ReasoningText, "reasoning_text"
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

// withRequestModel replaces the request's model, as Azure deployments may be
// named differently from the catalog model.
func withRequestModel(body, model string) (string, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		return "", fmt.Errorf("decode request: %w", err)
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return "", fmt.Errorf("encode model: %w", err)
	}
	request["model"] = encoded
	updated, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}
	return string(updated), nil
}
