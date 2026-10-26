package ai // import "github.com/losinggeneration/tau/ai"

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"golang.org/x/text/encoding/unicode"
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

func isClaude55Model(modelID string) bool {
	id := strings.ReplaceAll(strings.ToLower(modelID), ".", "-")
	return strings.Contains(id, "opus-5-5") || strings.Contains(id, "sonnet-5-5") || strings.Contains(id, "haiku-5-5")
}

// isAdaptiveThinkingModel checks if a model supports adaptive thinking.
// Adaptive thinking models include Opus 4.6+, Sonnet 4.6+, and Opus 4.7+.
func isAdaptiveThinkingModel(modelID string) bool {
	lowerID := strings.ToLower(modelID)
	return isClaude55Model(modelID) || strings.Contains(lowerID, "opus-4-6") ||
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
	case ReasoningMax:
		return "max"
	case ReasoningXHigh:
		if isClaude55Model(modelID) {
			return "xhigh"
		}
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
	tools         []Tool
	transportOpts *TransportOption
	// thinkingLevel is the managed effort stamped on streamed assistant messages.
	thinkingLevel string
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
		}),
	}, nil
}

// Complete implements Client.
func (c *anthropicClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

// Stream implements Client.
func (c *anthropicClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) (err error) {
	resolvedOptions, resolveErr := resolveRequestOptions(ctx, model, withCopilotHeaders(model, prompt, opts))
	if resolveErr != nil {
		return resolveErr
	}
	opts = resolvedOptions
	// Per-request state lives on a copy so a shared client can stream concurrently.
	request := *c
	return request.stream(ctx, model, prompt, timedEventHandler(handler), false, opts...)
}

func (c *anthropicClient) stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, fallback bool, opts ...Option) (err error) {
	if err := ValidateContextLimits(model, prompt); err != nil {
		return err
	}
	declared := providerContext(prompt).Tools
	_, nativeToolChanges := anthropicTranscript(model, prompt)
	var o options
	o.apply(opts)
	c.thinkingLevel = ""
	if anthropicManagedEffort(model) {
		c.thinkingLevel = anthropicEffort(model, &o)
		if c.thinkingLevel == "" {
			c.thinkingLevel = "high"
		}
	}

	// Determine if this is an OAuth token
	apiKey := o.apiKey
	if apiKey == "" {
		if headers := o.envHeaders(model.Provider); len(headers) != 0 {
			copyConfiguredHeaders(headers, o.headers)
			o.headers = headers
		} else {
			apiKey = o.envAPIKey(model.Provider)
		}
	}
	c.isOAuth = model.Provider != ProviderGitHubCopilot && isOAuthToken(apiKey)

	c.tools = declared

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

	hasAuth := apiKey != ""
	for name, value := range o.headers {
		if (strings.EqualFold(name, "authorization") || strings.EqualFold(name, "x-api-key") || strings.EqualFold(name, "cf-aig-authorization")) && strings.TrimSpace(value) != "" {
			hasAuth = true
		}
	}
	if !hasAuth {
		return fmt.Errorf("%s: missing API key", model.Provider)
	}

	// Build headers based on auth type
	var betas []string
	if anthropicManagedEffort(model) {
		betas = append(betas, anthropicMidConversationOutputConfigBeta, anthropicThinkingBindingBeta)
	}
	if nativeToolChanges {
		betas = append(betas, anthropicInlineToolsBeta)
	}
	headers := c.buildHeaders(apiKey, model, &o, betas...)

	transport := c.transport.withHeaders(headers)
	transport.timeout = o.timeout
	transport.providerEnv = o.providerEnv
	if baseURL := requestBaseURL(model, apiKey); baseURL != "" {
		transport.baseURL = baseURL
	}
	if refresh := cmp.Or(o.transportOpts, c.transportOpts); refresh != nil && apiKey != "" {
		transport.refreshCreds, transport.provider = refresh.RefreshCreds, refresh.Provider
	}

	resp, err := sendProviderRequest(ctx, &o, providerRequest{label: "anthropic", send: func() (*http.Response, error) {
		return transport.Do(ctx, http.MethodPost, "/v1/messages", reqBody)
	}})
	var rejected *providerStatusError
	if errors.As(err, &rejected) {
		body := rejected.body
		if !fallback && ctx.Err() == nil && rejected.status == http.StatusBadRequest && anthropicStrictLimitError(body) {
			retryPrompt := NormalizeContext(prompt)
			changed := false
			for m := range retryPrompt.Messages {
				tools := slices.Clone(retryPrompt.Messages[m].ToolsAdded)
				retryPrompt.Messages[m].ToolsAdded = tools
				for i, tool := range tools {
					config, err := constrainedSampling(tool)
					if err != nil {
						return err
					}
					if config != nil && config.Type == "json_schema" && config.Strict == "prefer" {
						_, strict, err := strictToolParameters(tool, model.Compat != nil && model.Compat.SupportsStrictTools, anthropicUnsupportedStrictKeyword)
						if err != nil {
							return err
						}
						if strict {
							tools[i].ConstrainedSampling = nil
							changed = true
						}
					}
				}
			}
			if changed {
				diagnostic := AssistantDiagnostic{Type: "anthropic_strict_tools_fallback", Timestamp: time.Now().UnixMilli(), Error: map[string]string{"message": string(body)}, Details: map[string]string{"reason": "strict_tool_limits_exceeded"}}
				return c.stream(ctx, model, retryPrompt, func(ctx context.Context, event StreamEvent) {
					switch e := event.(type) {
					case Done:
						e.Message.Diagnostics = append(e.Message.Diagnostics, diagnostic)
						event = e
					case StreamError:
						e.Msg.Diagnostics = append(e.Msg.Diagnostics, diagnostic)
						event = e
					}
					handler(ctx, event)
				}, true, opts...)
			}
		}
		return err
	}
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

	mediaType, _, mediaTypeErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaTypeErr == nil && mediaType == "text/event-stream" {
		return c.handleSSE(ctx, resp.Body, handler)
	}

	// Handle non-streaming response
	return c.handleJSON(ctx, resp.Body, handler)
}

// buildHeaders constructs the appropriate headers based on auth type and model.
func (c *anthropicClient) buildHeaders(apiKey string, model *Model, o *options, extraBetas ...string) map[string]string {
	headers := make(map[string]string)

	// Base headers
	headers["accept"] = "application/json"
	headers["anthropic-dangerous-direct-browser-access"] = "true"

	// Build beta features list
	var betaFeatures []string

	if len(c.tools) > 0 && model.Compat != nil && model.Compat.SupportsEagerToolInputStreaming != nil && !*model.Compat.SupportsEagerToolInputStreaming {
		betaFeatures = append(betaFeatures, "fine-grained-tool-streaming-2025-05-14")
	}

	activeThinking := o.thinkingEnabled || o.thinkingBudgetTokens > 0 || o.effort != "" ||
		(o.reasoning != "" && o.reasoning != ReasoningOff)
	needsInterleavedBeta := model.Reasoning && activeThinking &&
		(o.interleavedThinking == nil || *o.interleavedThinking) &&
		!isAdaptiveThinkingModel(model.ID) && !anthropicManagedEffort(model) &&
		(model.Compat == nil || !model.Compat.ForceAdaptiveThinking)
	if needsInterleavedBeta {
		betaFeatures = append(betaFeatures, "interleaved-thinking-2025-05-14")
	}
	betaFeatures = append(betaFeatures, extraBetas...)

	// OAuth: includes claude-code and oauth betas + Claude Code identity headers
	if c.isOAuth {
		betaFeatures = append([]string{
			"claude-code-20250219",
			"oauth-2025-04-20",
		}, betaFeatures...)

		headers["authorization"] = "Bearer " + apiKey
		headers["anthropic-beta"] = strings.Join(betaFeatures, ",")
		headers["user-agent"] = "claude-cli/2.1.75"
		headers["x-app"] = "cli"
	} else {
		// Standard or Copilot auth
		if len(betaFeatures) > 0 {
			headers["anthropic-beta"] = strings.Join(betaFeatures, ",")
		}

		if model.Provider == ProviderGitHubCopilot {
			headers["authorization"] = "Bearer " + apiKey
		} else if apiKey != "" {
			headers["x-api-key"] = apiKey
		}
	}

	if sessionID := cacheSessionID(o); sessionID != "" && !c.isOAuth && model.Provider != ProviderGitHubCopilot && model.Compat != nil && model.Compat.SendSessionAffinityHeaders {
		format := model.Compat.SessionAffinityFormat
		if format == "" && (model.Provider == ProviderOpenRouter || strings.Contains(model.BaseURL, "openrouter.ai")) {
			format = "openrouter"
		}
		header := "x-session-affinity"
		if format == "openrouter" {
			header = "x-session-id"
		}
		headers[header] = sessionID
	}

	// Merge user-provided headers
	copyConfiguredHeaders(headers, o.headers)

	return headers
}

// buildRequest builds the Anthropic API request body.
func (c *anthropicClient) buildRequest(model *Model, prompt *Context, o *options) (string, error) {
	prompt, nativeToolChanges := anthropicTranscript(model, prompt)
	req := anthropicRequest{
		Model:     model.ID,
		MaxTokens: getMaxTokens(o, model),
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
	if cacheRetention != CacheRetentionNone {
		cacheControl = &cacheControlParam{Type: "ephemeral"}
		if cacheRetention == CacheRetentionLong && (model.Compat == nil || model.Compat.SupportsLongCacheRetention == nil || *model.Compat.SupportsLongCacheRetention) {
			cacheControl.TTL = "1h"
		}
	}
	toolCacheControl := cacheControl
	if model.Compat != nil && model.Compat.SupportsCacheControlOnTools != nil && !*model.Compat.SupportsCacheControlOnTools {
		toolCacheControl = nil
	}
	convertTools := func(tools []Tool, cacheControl *cacheControlParam) ([]anthropicTool, error) {
		converted := make([]anthropicTool, len(tools))
		for i, t := range tools {
			name := t.Name
			if c.isOAuth {
				name = toClaudeCodeName(t.Name)
			}
			parameters, strict, err := strictToolParameters(t, model.Compat != nil && model.Compat.SupportsStrictTools, anthropicUnsupportedStrictKeyword)
			if err != nil {
				return nil, err
			}
			converted[i] = anthropicTool{Strict: strict, Name: name, Description: t.Description, InputSchema: parameters, EagerInputStreaming: model.Compat == nil || model.Compat.SupportsEagerToolInputStreaming == nil || *model.Compat.SupportsEagerToolInputStreaming}
			if i == len(tools)-1 {
				converted[i].CacheControl = cacheControl
			}
		}
		return converted, nil
	}

	var toolDefinitions func([]Tool) ([]anthropicTool, error)
	if nativeToolChanges {
		toolDefinitions = func(tools []Tool) ([]anthropicTool, error) { return convertTools(tools, nil) }
	}
	msgs, levels, err := c.convertMessages(transformedCtx, cacheControl, toolDefinitions)
	if err != nil {
		return "", err
	}
	req.Messages = msgs

	systemBlocks := make([]anthropicSystemBlock, 0, 2)
	if c.isOAuth {
		systemBlocks = append(systemBlocks, anthropicSystemBlock{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude.", CacheControl: cacheControl})
	}
	if prompt.SystemPrompt != "" {
		systemBlocks = append(systemBlocks, anthropicSystemBlock{Type: "text", Text: SanitizeSurrogates(prompt.SystemPrompt), CacheControl: cacheControl})
	}
	if len(systemBlocks) > 0 {
		req.System = systemBlocks
	}

	if len(prompt.Tools) > 0 {
		tools, err := convertTools(prompt.Tools, toolCacheControl)
		if err != nil {
			return "", err
		}
		req.Tools = tools
		if nativeToolChanges {
			req.Tools = append(req.Tools, anthropicDeferredToolPlaceholder)
		}
	}

	display := o.thinkingDisplay
	if display == "" {
		display = "summarized"
	}
	effort := anthropicEffort(model, o)
	// hasThinking is true when the caller expressed any opinion about
	// reasoning/thinking; Anthropic rejects temperature alongside thinking.
	hasThinking := o.thinkingEnabled || o.thinkingBudgetTokens > 0 || o.effort != "" ||
		(o.reasoning != "" && o.reasoning != ReasoningOff)
	switch {
	case anthropicManagedEffort(model):
		// Managed effort models always think adaptively so replayed blocks whose
		// prefix changed are dropped instead of failing the request.
		req.Thinking = &anthropicThinking{Type: "adaptive", Display: display, BlockBinding: &anthropicBlockBinding{PrefixMismatchBehavior: "drop_block"}}
		req.OutputConfig = &anthropicOutputConfig{Effort: "high"}
		active := effort
		if active == "" {
			active = "high"
		}
		req.Messages = insertAnthropicEffortMessages(req.Messages, levels, active)
	case hasThinking && (isAdaptiveThinkingModel(model.ID) || model.Compat != nil && model.Compat.ForceAdaptiveThinking):
		req.Thinking = &anthropicThinking{Type: "adaptive", Display: display}
		if effort != "" {
			req.OutputConfig = &anthropicOutputConfig{Effort: effort}
		}
	case hasThinking:
		budget := o.thinkingBudgetTokens
		if budget == 0 {
			if o.reasoning != "" && o.reasoning != ReasoningOff {
				budget = ThinkingBudgetForLevel(o.reasoning, o.thinkingBudgets)
			} else {
				budget = 1024
			}
		}
		req.MaxTokens, budget = fitThinkingBudget(model, prompt, req.MaxTokens, budget)
		req.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget, Display: display}
	case model.Reasoning && (o.reasoning == ReasoningOff || o.thinkingType == "disabled"):
		if off, exists := model.ThinkingLevelMap["off"]; !exists || off != nil {
			req.Thinking = &anthropicThinking{Type: "disabled"}
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

	supportsTemperature := model.Compat == nil || model.Compat.SupportsTemperature == nil || *model.Compat.SupportsTemperature
	if o.temperature != nil && !hasThinking && !anthropicManagedEffort(model) && supportsTemperature {
		req.Temperature = o.temperature
	}
	if userID, ok := o.metadata["user_id"].(string); ok {
		req.Metadata = map[string]any{"user_id": userID}
	}

	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

const (
	anthropicMidConversationOutputConfigBeta = "mid-conversation-output-config-2026-07-01"
	anthropicThinkingBindingBeta             = "thinking-binding-controls-2026-08-01"
	anthropicInlineToolsBeta                 = "inline-tools-2026-09-15"
)

// anthropicDeferredToolPlaceholder anchors the fixed request tool list when later
// tools are declared inline; Anthropic rejects a list where every tool is deferred.
var anthropicDeferredToolPlaceholder = anthropicTool{
	Name:         "__pi_deferred_placeholder__",
	Description:  "Reserved placeholder. Never available. Never call this.",
	InputSchema:  json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
	DeferLoading: true,
}

func anthropicManagedEffort(model *Model) bool {
	return model.Compat != nil && model.Compat.SupportsMidConvoEffort
}

// anthropicEffort resolves the native effort from WithEffort or the reasoning level.
func anthropicEffort(model *Model, o *options) string {
	if o.effort != "" {
		return o.effort
	}
	if o.reasoning == "" || o.reasoning == ReasoningOff {
		return ""
	}
	if mapped := model.ThinkingLevelMap[string(o.reasoning)]; mapped != nil {
		return *mapped
	}
	return mapReasoningLevelToAnthropicEffort(o.reasoning, model.ID)
}

// anthropicTranscript resolves the request prompt and tools. Models that accept later
// system messages keep them in place after the leading one; the rest see the replayed
// baseline. Tool changes are native when the leading message declares tools to anchor them.
func anthropicTranscript(model *Model, prompt *Context) (*Context, bool) {
	if model.Compat == nil || !model.Compat.SupportsMidConvoSystemMessages {
		return providerContext(prompt), false
	}
	messages := NormalizeContext(prompt).Messages
	request := &Context{Messages: messages}
	if len(messages) > 0 && messages[0].Role == "system" {
		request.SystemPrompt = providerContext(&Context{Messages: messages[:1]}).SystemPrompt
		request.Tools = messages[0].ToolsAdded
		request.Messages = messages[1:]
	}
	if model.Compat.SupportsMidConvoToolChanges && len(request.Tools) > 0 {
		return request, true
	}
	request.Tools = providerContext(&Context{Messages: messages}).Tools
	return request, false
}

// renderSystemMessageUpdate frames a later system message's section changes by name.
func renderSystemMessageUpdate(message Message) string {
	var parts []string
	if text := transcriptText(message.Content); text != "" {
		parts = append(parts, text)
	}
	names := make([]string, 0, len(message.Sections))
	for name := range message.Sections {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if value := message.Sections[name]; value == nil {
			parts = append(parts, fmt.Sprintf("Removed system prompt section %q.", name))
		} else {
			parts = append(parts, fmt.Sprintf("Updated system prompt section %q:\n\n%s", name, *value))
		}
	}
	return strings.Join(parts, "\n\n")
}

func isAnthropicEffort(value string) bool {
	switch value {
	case "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

// insertAnthropicEffortMessages restates each historical assistant turn's effort before it
// and the active effort at the end, so changing effort keeps the cached prefix valid.
func insertAnthropicEffortMessages(converted []anthropicMessage, levels map[int]string, active string) []anthropicMessage {
	messages := make([]anthropicMessage, 0, len(converted)+len(levels)+1)
	for i, message := range converted {
		if level, ok := levels[i]; ok {
			messages = append(messages, anthropicMessage{Role: "system", Content: []anthropicContentBlock{}, OutputConfig: &anthropicOutputConfig{Effort: level}})
		}
		messages = append(messages, message)
	}
	return append(messages, anthropicMessage{Role: "system", Content: []anthropicContentBlock{}, OutputConfig: &anthropicOutputConfig{Effort: active}})
}

func (c *anthropicClient) convertMessages(prompt *Context, cacheControl *cacheControlParam, toolDefinitions func([]Tool) ([]anthropicTool, error)) ([]anthropicMessage, map[int]string, error) {
	var msgs []anthropicMessage
	levels := make(map[int]string)
	// Later system messages wait until the next assistant message or the end: a
	// tool_result must directly follow its tool_use.
	var pending []anthropicMessage

	for i := 0; i < len(prompt.Messages); i++ {
		m := prompt.Messages[i]
		switch normalizeMessageRole(m.Role) {
		case "system":
			var blocks []anthropicContentBlock
			if text := renderSystemMessageUpdate(m); text != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: SanitizeSurrogates(text)})
			}
			if toolDefinitions != nil {
				redefined := make(map[string]bool, len(m.ToolsAdded))
				for _, tool := range m.ToolsAdded {
					redefined[tool.Name] = true
				}
				for _, tool := range m.ToolsRemoved {
					if redefined[tool.Name] {
						continue
					}
					name := tool.Name
					if c.isOAuth {
						name = toClaudeCodeName(name)
					}
					blocks = append(blocks, anthropicContentBlock{Type: "tool_removal", Tool: map[string]any{"type": "tool_reference", "name": name}})
				}
				definitions, err := toolDefinitions(m.ToolsAdded)
				if err != nil {
					return nil, nil, err
				}
				for _, definition := range definitions {
					blocks = append(blocks, anthropicContentBlock{Type: "tool_addition", Tool: map[string]any{"type": "tool_definition", "definition": definition}})
				}
			}
			if len(blocks) > 0 {
				pending = append(pending, anthropicMessage{Role: "system", Content: blocks})
			}
			continue
		case "toolResult":
			// Batch consecutive tool results
			content, next, err := c.convertToolResults(prompt.Messages, i)
			if err != nil {
				return nil, nil, err
			}
			msgs = append(msgs, anthropicMessage{Role: "user", Content: content})
			i = next - 1
			continue
		case "assistant":
			msgs = append(msgs, pending...)
			pending = nil
		}

		msg, err := c.convertMessage(m, cacheControl)
		if err != nil {
			return nil, nil, err
		}
		if msg == nil {
			continue
		}
		if msg.Role == "assistant" && m.API == APIAnthropicMessages && c.model != nil && m.Provider == c.model.Provider && isAnthropicEffort(m.ProviderThinkingLevel) {
			levels[len(msgs)] = m.ProviderThinkingLevel
		}
		msgs = append(msgs, *msg)
	}
	msgs = append(msgs, pending...)

	// Cache the conversation through the last user or system message.
	if cacheControl != nil && len(msgs) > 0 {
		lastMsg := &msgs[len(msgs)-1]
		if lastMsg.Role == "user" || lastMsg.Role == "system" {
			c.addCacheControlToLastBlock(lastMsg, cacheControl)
		}
	}

	return msgs, levels, nil
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
			case "text", "image", "tool_result", "tool_addition", "tool_removal":
				content[lastIdx].CacheControl = cacheControl
			}
		}
	case []anthropicToolResultBlock:
		if len(content) > 0 {
			content[len(content)-1].CacheControl = cacheControl
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

func (c *anthropicClient) convertToolResults(messages []Message, start int) ([]anthropicToolResultBlock, int, error) {
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

		results = append(results, result)
	}

	return results, i, nil
}

func (c *anthropicClient) convertMessage(m Message, cacheControl *cacheControlParam) (*anthropicMessage, error) {
	var (
		blocks []anthropicContentBlock
		err    error
	)
	role := normalizeMessageRole(m.Role)
	switch role {
	case "user":
		blocks, err = c.extractContent(m.Content)
	case "assistant":
		if contentBlocks, ok := m.Content.([]ContentBlock); ok {
			for _, b := range contentBlocks {
				if b.Type == ContentBlockThinking && !b.Redacted && strings.TrimSpace(b.Thinking) != "" && strings.TrimSpace(b.ThoughtSignature) == "" && c.model != nil && c.model.Compat != nil && c.model.Compat.AllowEmptySignature {
					blocks = append(blocks, anthropicContentBlock{Type: "thinking", Thinking: new(b.Thinking), Signature: new("")})
					continue
				}
				if b.Type == "toolCall" {
					name := b.Name
					if c.isOAuth {
						name = toClaudeCodeName(b.Name)
					}
					blocks = append(blocks, anthropicContentBlock{
						Type:  "tool_use",
						ID:    b.ID,
						Name:  name,
						Input: anthropicToolInput(b.Arguments),
					})
					continue
				}
				content, err := c.extractContent([]ContentBlock{b})
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, content...)
			}
		} else {
			blocks, err = c.extractContent(m.Content)
		}
	case "toolResult":
		// This case is handled in convertMessages for batching
		return nil, nil

	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	blocks = slices.DeleteFunc(blocks, func(block anthropicContentBlock) bool {
		return block.Type == "text" && strings.TrimSpace(block.Text) == ""
	})
	if len(blocks) == 0 {
		return nil, nil
	}
	return &anthropicMessage{Role: role, Content: blocks}, nil
}

func (c *anthropicClient) toolResultContent(content any) (any, error) {
	blocks, err := c.extractContent(content)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return "", nil
	}
	var texts []string
	for _, block := range blocks {
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	if len(texts) == len(blocks) {
		return strings.Join(texts, "\n"), nil
	}
	if len(texts) == 0 && slices.ContainsFunc(blocks, func(block anthropicContentBlock) bool { return block.Type == "image" }) {
		blocks = append([]anthropicContentBlock{{Type: "text", Text: "(see attached image)"}}, blocks...)
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
					Source: &anthropicImageSource{
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
				hasSignature := strings.TrimSpace(b.ThoughtSignature) != ""
				if strings.TrimSpace(b.Thinking) == "" && !hasSignature {
					continue
				}
				if !hasSignature {
					blocks = append(blocks, anthropicContentBlock{
						Type: "text",
						Text: b.Thinking,
					})
				} else {
					// Has signature -> emit thinking block with signature
					blocks = append(blocks, anthropicContentBlock{
						Type:      "thinking",
						Thinking:  new(b.Thinking),
						Signature: new(b.ThoughtSignature),
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
		Role:                  "assistant",
		API:                   APIAnthropicMessages,
		Provider:              c.model.Provider,
		Model:                 c.model.ID,
		Content:               []ContentBlock{},
		Timestamp:             timestamp(),
		Usage:                 Usage{},
		ProviderThinkingLevel: c.thinkingLevel,
	}
	handler(ctx, Start{Partial: assistant})

	blockIndexes := make(map[int]int)
	partialToolArgs := make(map[int]string)

	var sawStart, sawStop bool
	fail := func(err error) error {
		assistant.ErrorMessage = ""
		if ctx.Err() != nil {
			emitTerminalStreamError(ctx, handler, assistant, fmt.Errorf("request was aborted: %w", context.Cause(ctx)), StopReasonAborted)
			return nil
		}
		emitTerminalStreamError(ctx, handler, assistant, err, StopReasonError)

		return nil
	}

	reader := bufio.NewReader(body)
	if ctx.Err() != nil {
		return fail(nil)
	}
	// Peek retains short-body and read errors for normal event-reader handling.
	if prefix, err := reader.Peek(3); err == nil && bytes.Equal(prefix, []byte("\xef\xbb\xbf")) {
		if _, err := reader.Discard(3); err != nil {
			return fail(err)
		}
	}
	for {
		if ctx.Err() != nil {
			return fail(nil)
		}

		name, data, err := readAnthropicSSEData(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return fail(err)
		}

		// The UTF-8 decoder replaces each malformed sequence with U+FFFD
		// instead of failing, matching the provider's tolerance for bad
		// bytes.
		decoded, decodeErr := unicode.UTF8.NewDecoder().String(data)
		if decodeErr != nil {
			// Unreachable for a replacing decoder; keep the raw payload if
			// it ever fires rather than aborting the stream.
			decoded = data
		}
		data = decoded

		switch name {
		case "error":
			return fail(errors.New(data))
		case "", "message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop":
		default:
			continue
		}

		var event anthropicEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			if repaired := repairAnthropicJSONStrings(data); repaired != data {
				event = anthropicEvent{}
				err = json.Unmarshal([]byte(repaired), &event)
			}
			if err != nil {
				return fail(fmt.Errorf("could not parse anthropic SSE event: %w; data=%s", err, data))
			}
		}

		switch event.Type {
		case "error":
			return fail(errors.New(data))

		case "message_start":
			sawStart = true
			if event.Message != nil {
				assistant.ResponseID = event.Message.ID
				assistant.Usage = Usage{}
				event.Message.Usage.apply(&assistant.Usage)
				assistant.Usage.CalculateCost(c.model.Cost)
			}

		case "content_block_start":
			if event.ContentBlock == nil {
				continue
			}

			if event.ContentBlock.Type == "fallback" && len(assistant.Content) > 0 {
				return fail(errors.New("anthropic performed an unsupported mid-output model fallback"))
			}

			idx := len(assistant.Content)

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
					Thinking:         deref(event.ContentBlock.Thinking),
					ThoughtSignature: deref(event.ContentBlock.Signature),
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
				assistant.Content[idx].Arguments = anthropicToolInput(event.ContentBlock.Input)
				handler(ctx, ToolCallStart{ContentIndex: idx, Partial: assistant})
			default:
				continue
			}
			blockIndexes[event.Index] = idx

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
				if assistant.Content[idx].Type != "text" {
					continue
				}
				assistant.Content[idx].Text += event.Delta.Text
				handler(ctx, TextDelta{
					ContentIndex: idx,
					Delta:        event.Delta.Text,
					Partial:      assistant,
				})

			case "thinking_delta":
				if assistant.Content[idx].Type != "thinking" {
					continue
				}
				assistant.Content[idx].Thinking += event.Delta.Thinking
				handler(ctx, ThinkingDelta{
					ContentIndex: idx,
					Delta:        event.Delta.Thinking,
					Partial:      assistant,
				})

			case "input_json_delta":
				if assistant.Content[idx].Type != "toolCall" {
					continue
				}
				partialToolArgs[event.Index] += event.Delta.PartialJSON
				partial := partialToolArgs[event.Index]
				if !json.Valid([]byte(partial)) {
					if repaired := repairAnthropicJSONStrings(partial); json.Valid([]byte(repaired)) {
						partial = repaired
					}
				}
				assistant.Content[idx].Arguments = json.RawMessage(partial)
				handler(ctx, ToolCallDelta{
					ContentIndex: idx,
					Delta:        event.Delta.PartialJSON,
					Partial:      assistant,
				})

			case "signature_delta":
				if assistant.Content[idx].Type != "thinking" {
					continue
				}
				assistant.Content[idx].ThoughtSignature += event.Delta.Signature
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

			delete(blockIndexes, event.Index)
			delete(partialToolArgs, event.Index)

		case "message_delta":
			if event.Delta != nil && event.Delta.StopReason != "" {
				assistant.StopReason, assistant.ErrorMessage = anthropicStopReason(event.Delta.StopReason, event.Delta.StopDetails)
			}

			if event.Usage != nil {
				// Proxies may omit fields message_start already reported.
				event.Usage.apply(&assistant.Usage)
				assistant.Usage.CalculateCost(c.model.Cost)
			}

		case "message_stop":
			sawStop = true
		}
	}

	if ctx.Err() != nil {
		return fail(nil)
	}
	if sawStart && !sawStop {
		return fail(errors.New("anthropic stream ended before message_stop"))
	}
	finishStream(ctx, handler, assistant, assistant.StopReason != "", "anthropic stream ended without a stop reason")

	return nil
}

// anthropicStopReason maps a wire stop reason and, for errors, the message to
// report, like upstream mapStopReason.
func anthropicStopReason(reason string, details *anthropicStopDetails) (StopReason, string) {
	switch reason {
	case "end_turn", "pause_turn", "stop_sequence":
		return StopReasonStop, ""
	case "max_tokens":
		return StopReasonLength, ""
	case "tool_use":
		return StopReasonToolUse, ""
	case "refusal":
		if details != nil && details.Explanation != "" {
			return StopReasonError, details.Explanation
		}

		return StopReasonError, "The model refused to complete the request"
	case "sensitive":
		return StopReasonError, "Provider stopped with: sensitive"
	default:
		return StopReasonError, "unhandled stop reason: " + reason
	}
}

type anthropicStopDetails struct {
	Explanation string `json:"explanation"`
}

func (c *anthropicClient) handleJSON(ctx context.Context, body io.Reader, handler EventHandler) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	var resp anthropicResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	assistant := AssistantMessage{
		Role:                  "assistant",
		API:                   APIAnthropicMessages,
		Provider:              c.model.Provider,
		Model:                 c.model.ID,
		Content:               c.convertResponseBlocks(resp.Content),
		Timestamp:             timestamp(),
		ResponseID:            resp.ID,
		ProviderThinkingLevel: c.thinkingLevel,
	}
	resp.Usage.apply(&assistant.Usage)
	assistant.Usage.CalculateCost(c.model.Cost)
	if ctx.Err() == nil {
		assistant.StopReason, assistant.ErrorMessage = anthropicStopReason(resp.StopReason, resp.StopDetails)
	}
	finishStream(ctx, handler, assistant, true, "")

	return nil
}

func anthropicToolInput(input json.RawMessage) json.RawMessage {
	if len(input) == 0 || bytes.Equal(bytes.TrimSpace(input), []byte("null")) {
		return json.RawMessage("{}")
	}
	return input
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
				Type:             "thinking",
				Thinking:         b.Thinking,
				ThoughtSignature: b.Signature,
			})
		case "redacted_thinking":
			result = append(result, ContentBlock{
				Type:             "thinking",
				Thinking:         "[Reasoning redacted]",
				ThoughtSignature: b.Data,
				Redacted:         true,
			})
		case "tool_use":
			name := b.Name
			if c.isOAuth {
				name = fromClaudeCodeName(name, c.tools)
			}
			result = append(result, ContentBlock{
				Type:      "toolCall",
				ID:        b.ID,
				Name:      name,
				Arguments: anthropicToolInput(b.Input),
			})
		}
	}

	return result
}

// Anthropic API types

type anthropicRequest struct {
	Model        string                 `json:"model"`
	Messages     []anthropicMessage     `json:"messages"`
	System       any                    `json:"system,omitempty"` // string or []anthropicSystemBlock
	Tools        []anthropicTool        `json:"tools,omitempty"`
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	Metadata     map[string]any         `json:"metadata,omitempty"`
	MaxTokens    int                    `json:"max_tokens"`
	Stream       bool                   `json:"stream"`
	ToolChoice   any                    `json:"tool_choice,omitempty"`
	Temperature  *float64               `json:"temperature,omitempty"`
}

type anthropicSystemBlock struct {
	Type         string             `json:"type"`
	Text         string             `json:"text"`
	CacheControl *cacheControlParam `json:"cache_control,omitempty"`
}

type anthropicMessage struct {
	Role         string                 `json:"role"`
	Content      any                    `json:"content"` // string or []anthropicContentBlock
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicBlockBinding struct {
	PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
}

type anthropicContentBlock struct {
	Type         string                `json:"type"`
	Text         string                `json:"text,omitempty"`
	Source       *anthropicImageSource `json:"source,omitempty"`
	ID           string                `json:"id,omitempty"`
	Name         string                `json:"name,omitempty"`
	Input        json.RawMessage       `json:"input,omitempty"`
	Data         string                `json:"data,omitempty"` // For redacted_thinking
	Thinking     *string               `json:"thinking,omitempty"`
	Signature    *string               `json:"signature,omitempty"`
	Tool         map[string]any        `json:"tool,omitempty"` // For tool_addition and tool_removal
	CacheControl *cacheControlParam    `json:"cache_control,omitempty"`
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
	EagerInputStreaming bool               `json:"eager_input_streaming,omitempty"`
	Strict              bool               `json:"strict,omitempty"`
	Name                string             `json:"name"`
	Description         string             `json:"description"`
	InputSchema         json.RawMessage    `json:"input_schema"`
	DeferLoading        bool               `json:"defer_loading,omitempty"`
	CacheControl        *cacheControlParam `json:"cache_control,omitempty"`
}

type cacheControlParam struct {
	Type string `json:"type"`          // "ephemeral"
	TTL  string `json:"ttl,omitempty"` // e.g., "1h"
}

type anthropicThinking struct {
	Type         string                 `json:"type"` // "adaptive" | "enabled" | "disabled"
	BudgetTokens int                    `json:"budget_tokens,omitempty"`
	Display      string                 `json:"display,omitempty"` // "summarized" | "omitted"
	BlockBinding *anthropicBlockBinding `json:"block_binding,omitempty"`
}

type anthropicResponse struct {
	ID           string                   `json:"id"`
	Type         string                   `json:"type"`
	Role         string                   `json:"role"`
	Content      []anthropicResponseBlock `json:"content"`
	Model        string                   `json:"model"`
	StopReason   string                   `json:"stop_reason"`
	StopDetails  *anthropicStopDetails    `json:"stop_details"`
	StopSequence string                   `json:"stop_sequence"`
	Usage        anthropicEventUsage      `json:"usage"`
}

type anthropicResponseBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
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
	Type        string                `json:"type"`
	Text        string                `json:"text,omitempty"`
	Thinking    string                `json:"thinking,omitempty"`
	PartialJSON string                `json:"partial_json,omitempty"`
	StopReason  string                `json:"stop_reason,omitempty"`
	StopDetails *anthropicStopDetails `json:"stop_details,omitempty"`
	Signature   string                `json:"signature,omitempty"`
}

type anthropicEventUsage struct {
	InputTokens        *int `json:"input_tokens"`
	OutputTokens       *int `json:"output_tokens"`
	CacheCreationInput *int `json:"cache_creation_input_tokens"`
	CacheReadInput     *int `json:"cache_read_input_tokens"`
	CacheCreation      *struct {
		Ephemeral1h *int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// apply overwrites only the counts the event reports.
func (u anthropicEventUsage) apply(usage *Usage) {
	for _, field := range []struct {
		value  *int
		target *int
	}{
		{u.InputTokens, &usage.Input},
		{u.OutputTokens, &usage.Output},
		{u.CacheCreationInput, &usage.CacheWrite},
		{u.CacheReadInput, &usage.CacheRead},
	} {
		if field.value != nil {
			*field.target = *field.value
		}
	}
	if u.CacheCreation != nil && u.CacheCreation.Ephemeral1h != nil {
		usage.CacheWrite1h = *u.CacheCreation.Ephemeral1h
	}
}

// SSE helpers

// repairAnthropicJSONStrings escapes raw controls and invalid backslashes only
// inside strings. Other bytes, including raw tool-input numbers, stay intact.
func repairAnthropicJSONStrings(data string) string {
	var repaired strings.Builder
	var inString bool
	for i := 0; i < len(data); i++ {
		b := data[i]
		switch {
		case b == '"':
			inString = !inString
			repaired.WriteByte(b)
		case inString && b == '\\':
			repaired.WriteByte(b)
			if i+1 < len(data) && strings.ContainsRune(`"\/bfnrtu`, rune(data[i+1])) {
				i++
				repaired.WriteByte(data[i])
			} else {
				repaired.WriteByte('\\')
			}
		case inString && b < 0x20:
			const hex = "0123456789abcdef"
			repaired.WriteString(`\u00`)
			repaired.WriteByte(hex[b>>4])
			repaired.WriteByte(hex[b&0xf])
		default:
			repaired.WriteByte(b)
		}
	}
	return repaired.String()
}

func readLine(r *bufio.Reader) (string, error) {
	var buf bytes.Buffer

	for {
		b, err := r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && buf.Len() > 0 {
				return buf.String(), nil
			}
			return "", err
		}

		if b == '\r' {
			next, err := r.Peek(1)
			if err != nil {
				// CR completed the line. Peek retains its error for the next read.
				return buf.String(), nil
			}
			if len(next) > 0 && next[0] == '\n' {
				if _, err := r.ReadByte(); err != nil {
					return "", err
				}
			}
			break
		}
		if b == '\n' {
			break
		}

		buf.WriteByte(b)
	}

	return buf.String(), nil
}

func readAnthropicSSEData(reader *bufio.Reader) (string, string, error) {
	var (
		name string
		data []string
	)
	for {
		line, err := readLine(reader)
		if err != nil {
			if errors.Is(err, io.EOF) && (name != "" || len(data) > 0) {
				return name, strings.Join(data, "\n"), nil
			}
			return "", "", err
		}
		if line == "" {
			if name != "" || len(data) > 0 {
				return name, strings.Join(data, "\n"), nil
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			name = value
		case "data":
			data = append(data, value)
		}
	}
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

func anthropicStrictLimitError(body []byte) bool {
	var response struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return false
	}
	if response.Error.Type != "invalid_request_error" {
		return false
	}
	for _, message := range []string{"The compiled grammar is too large", "Too many strict tools (", "Schemas contains too many parameters with union types ("} {
		if strings.Contains(response.Error.Message, message) {
			return true
		}
	}
	return false
}
