package ai // import "github.com/losinggeneration/tau/ai"

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// Default endpoint for Gemini CLI (production)
	defaultGeminiCLIEndpoint = "https://cloudcode-pa.googleapis.com"

	// Antigravity endpoints (sandbox)
	antigravityDailyEndpoint    = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	antigravityAutopushEndpoint = "https://autopush-cloudcode-pa.sandbox.googleapis.com"

	// Retry configuration
	maxRetries               = 3
	baseDelayMs              = 1000
	maxEmptyStreamRetries    = 2
	emptyStreamBaseDelayMs   = 500
	claudeThinkingBetaHeader = "interleaved-thinking-2025-05-14"

	// Default Antigravity version
	defaultAntigravityVersion = "1.21.9"
)

// Google thinking level for Gemini 3 models.
type GoogleThinkingLevel string

const (
	GoogleThinkingLevelUnspecified GoogleThinkingLevel = "THINKING_LEVEL_UNSPECIFIED"
	GoogleThinkingLevelMinimal     GoogleThinkingLevel = "MINIMAL"
	GoogleThinkingLevelLow         GoogleThinkingLevel = "LOW"
	GoogleThinkingLevelMedium      GoogleThinkingLevel = "MEDIUM"
	GoogleThinkingLevelHigh        GoogleThinkingLevel = "HIGH"
)

// googleGeminiCLIOptions extends the standard options with Gemini CLI-specific fields.
type googleGeminiCLIOptions struct {
	thinking *geminiThinkingConfig
}

type geminiThinkingConfig struct {
	enabled      bool
	budgetTokens int
	level        GoogleThinkingLevel
}

// GeminiCLICredentials represents the JSON-encoded API key format for Gemini CLI.
type GeminiCLICredentials struct {
	Token     string `json:"token"`
	ProjectID string `json:"projectId"`
}

// CloudCodeAssistRequest represents the request body for the Cloud Code Assist API.
type CloudCodeAssistRequest struct {
	Project     string                      `json:"project"`
	Model       string                      `json:"model"`
	Request     cloudCodeAssistInnerRequest `json:"request"`
	RequestType string                      `json:"requestType,omitempty"`
	UserAgent   string                      `json:"userAgent,omitempty"`
	RequestID   string                      `json:"requestId,omitempty"`
}

type cloudCodeAssistInnerRequest struct {
	Contents          []cloudCodeContent `json:"contents"`
	SessionID         string             `json:"sessionId,omitempty"`
	SystemInstruction *systemInstruction `json:"systemInstruction,omitempty"`
	GenerationConfig  *generationConfig  `json:"generationConfig,omitempty"`
	Tools             []any              `json:"tools,omitempty"`
	ToolConfig        *toolConfig        `json:"toolConfig,omitempty"`
}

type systemInstruction struct {
	Role  string                  `json:"role,omitempty"`
	Parts []systemInstructionPart `json:"parts"`
}

type systemInstructionPart struct {
	Text string `json:"text"`
}

type generationConfig struct {
	MaxOutputTokens *int            `json:"maxOutputTokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	ThinkingConfig  *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type thinkingConfig struct {
	IncludeThoughts bool                `json:"includeThoughts,omitempty"`
	ThinkingBudget  int                 `json:"thinkingBudget,omitempty"`
	ThinkingLevel   GoogleThinkingLevel `json:"thinkingLevel,omitempty"`
}

type toolConfig struct {
	FunctionCallingConfig functionCallingConfig `json:"functionCallingConfig"`
}

type functionCallingConfig struct {
	Mode string `json:"mode"`
}

type cloudCodeContent struct {
	Role  string                 `json:"role,omitempty"`
	Parts []cloudCodeContentPart `json:"parts"`
}

type cloudCodeContentPart struct {
	Text         string            `json:"text,omitempty"`
	Thought      string            `json:"thought,omitempty"`
	FunctionCall *functionCallPart `json:"functionCall,omitempty"`
}

type functionCallPart struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
	ID   string         `json:"id,omitempty"`
}

// CloudCodeAssistResponseChunk represents a chunk from the SSE stream.
type CloudCodeAssistResponseChunk struct {
	Response *struct {
		Candidates []struct {
			Content *struct {
				Role  string                  `json:"role"`
				Parts []cloudCodeResponsePart `json:"parts,omitempty"`
			} `json:"content,omitempty"`
			FinishReason string `json:"finishReason,omitempty"`
		} `json:"candidates,omitempty"`
		UsageMetadata *usageMetadata `json:"usageMetadata,omitempty"`
		ModelVersion  string         `json:"modelVersion,omitempty"`
		ResponseID    string         `json:"responseId,omitempty"`
	} `json:"response,omitempty"`
	TraceID string `json:"traceId,omitempty"`
}

type cloudCodeResponsePart struct {
	Text             string `json:"text,omitempty"`
	Thought          bool   `json:"thought,omitempty"`
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
	FunctionCall     *struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
		ID   string         `json:"id,omitempty"`
	} `json:"functionCall,omitempty"`
}

type usageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount    int `json:"candidatesTokenCount,omitempty"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount         int `json:"totalTokenCount,omitempty"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
}

// googleGeminiCLIClient implements the Gemini CLI streaming client.
type googleGeminiCLIClient struct {
	model *Model
}

// IsAlias implements Client.
func (c *googleGeminiCLIClient) IsAlias() bool { return false }

// newGoogleGeminiCLIClient creates a new Google Gemini CLI client.
func newGoogleGeminiCLIClient(model *Model) (*googleGeminiCLIClient, error) {
	return &googleGeminiCLIClient{model: model}, nil
}

// Complete implements Client.
func (c *googleGeminiCLIClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

// Stream implements Client.
func (c *googleGeminiCLIClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) error {
	cfg := applyOptions(opts)

	// Parse OAuth credentials from apiKey (JSON-encoded: {token, projectId})
	apiKeyRaw := cfg.apiKey
	if apiKeyRaw == "" {
		return fmt.Errorf("google gemini cli: missing API key (set GOOGLE_CLOUD_CODE_ASSIST_TOKEN or use WithAPIKey with OAuth credentials)")
	}

	var creds GeminiCLICredentials
	if err := json.Unmarshal([]byte(apiKeyRaw), &creds); err != nil {
		return fmt.Errorf("google gemini cli: invalid credentials format: %w", err)
	}

	if creds.ProjectID == "" && cfg.geminiCLIProjectID != "" {
		creds.ProjectID = cfg.geminiCLIProjectID
	}

	if creds.Token == "" || creds.ProjectID == "" {
		return fmt.Errorf("google gemini cli: missing token or projectId in credentials")
	}

	isAntigravity := model.Provider == ProviderGoogleAntigravity || model.Provider == ProviderAntigravity

	// Determine endpoints based on provider
	baseURL := strings.TrimSpace(model.BaseURL)
	var endpoints []string
	if baseURL != "" {
		endpoints = []string{baseURL}
	} else if isAntigravity {
		endpoints = []string{antigravityDailyEndpoint, antigravityAutopushEndpoint, defaultGeminiCLIEndpoint}
	} else {
		endpoints = []string{defaultGeminiCLIEndpoint}
	}

	// Build request
	geminiOpts := &googleGeminiCLIOptions{}
	if cfg.thinkingEnabled || cfg.thinkingBudgetTokens > 0 || cfg.reasoning != ReasoningOff {
		geminiOpts.thinking = &geminiThinkingConfig{
			enabled:      true,
			budgetTokens: cfg.thinkingBudgetTokens,
		}
	}

	requestBody := buildGeminiCLIRequest(model, prompt, creds.ProjectID, geminiOpts, isAntigravity, cfg)

	// Allow payload callback to modify request
	if hasPayloadCallback(cfg) {
		if next := invokePayloadCallback(cfg, model, requestBody); next != nil {
			if replacement, ok := next.(CloudCodeAssistRequest); ok {
				requestBody = &replacement
			}
		}
	}

	// Build headers
	headers := buildGeminiCLIHeaders(isAntigravity, model, cfg)

	requestBodyJSON, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("google gemini cli: marshal request: %w", err)
	}

	// Make request with retry logic
	var response *http.Response
	var lastErr error
	var requestURL string
	endpointIndex := 0

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		endpoint := endpoints[endpointIndex]
		requestURL = endpoint + "/v1internal:streamGenerateContent?alt=sse"

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, strings.NewReader(string(requestBodyJSON)))
		if err != nil {
			return fmt.Errorf("google gemini cli: create request: %w", err)
		}

		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("google gemini cli: request failed: %w", err)
			if attempt < maxRetries {
				delayMs := baseDelayMs * int(math.Pow(2, float64(attempt)))
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(delayMs) * time.Millisecond):
					continue
				}
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			response = resp
			break
		}

		// Read error body
		body, readErr := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			if readErr != nil {
				readErr = fmt.Errorf("read error body: %w; close error body: %w", readErr, closeErr)
			} else {
				readErr = fmt.Errorf("close error body: %w", closeErr)
			}
		}
		if readErr != nil {
			lastErr = fmt.Errorf("google gemini cli: API error (%d): %w", resp.StatusCode, readErr)
			continue
		}
		errorText := string(body)

		// On 403/404, cascade to next endpoint immediately
		if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) && endpointIndex < len(endpoints)-1 {
			endpointIndex++
			continue
		}

		// Check if retryable
		if attempt < maxRetries && isRetryableError(resp.StatusCode, errorText) {
			if endpointIndex < len(endpoints)-1 {
				endpointIndex++
			}

			serverDelay := extractRetryDelay(errorText, resp.Header)
			var delayMs int
			if serverDelay > 0 {
				delayMs = serverDelay
			} else {
				delayMs = baseDelayMs * int(math.Pow(2, float64(attempt)))
			}

			// Check max retry delay
			if cfg.maxRetryDelayMs > 0 && serverDelay > cfg.maxRetryDelayMs {
				delaySeconds := int(math.Ceil(float64(serverDelay) / 1000))
				maxDelaySeconds := int(math.Ceil(float64(cfg.maxRetryDelayMs) / 1000))
				return fmt.Errorf("google gemini cli: server requested %ds retry delay (max: %ds): %s", delaySeconds, maxDelaySeconds, extractErrorMessage(errorText))
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(delayMs) * time.Millisecond):
				continue
			}
		}

		lastErr = fmt.Errorf("google gemini cli: API error (%d): %s", resp.StatusCode, extractErrorMessage(errorText))

		if resp.StatusCode == http.StatusOK {
			break
		}
	}

	if response == nil || response.StatusCode != http.StatusOK {
		return lastErr
	}

	// Invoke response callback
	if hasResponseCallback(cfg) {
		respHeaders := make(map[string]string)
		for k, values := range response.Header {
			if len(values) > 0 {
				respHeaders[k] = values[0]
			}
		}
		invokeResponseCallback(cfg, model, ProviderResponse{
			Status:  response.StatusCode,
			Headers: respHeaders,
		})
	}

	// Stream response
	return streamGeminiCLIResponse(ctx, model, response, handler, cfg)
}

// buildGeminiCLIHeaders builds request headers.
func buildGeminiCLIHeaders(isAntigravity bool, model *Model, cfg *options) map[string]string {
	headers := make(map[string]string)

	if isAntigravity {
		version := os.Getenv("PI_AI_ANTIGRAVITY_VERSION")
		if strings.TrimSpace(version) == "" {
			version = defaultAntigravityVersion
		}
		headers["User-Agent"] = fmt.Sprintf("antigravity/%s darwin/arm64", version)
	} else {
		headers["User-Agent"] = "google-cloud-sdk vscode_cloudshelleditor/0.1"
		headers["X-Goog-Api-Client"] = "gl-node/22.17.0"
		headers["Client-Metadata"] = `{"ideType":"IDE_UNSPECIFIED","platform":"PLATFORM_UNSPECIFIED","pluginType":"GEMINI"}`
	}

	// Add Claude thinking beta header for Claude models on Antigravity
	if isAntigravity && strings.HasPrefix(model.ID, "claude-") && model.Reasoning {
		headers["anthropic-beta"] = claudeThinkingBetaHeader
	}

	// Copy custom headers
	for k, v := range cfg.headers {
		headers[k] = v
	}

	return headers
}

// buildGeminiCLIRequest builds the Cloud Code Assist request body.
func buildGeminiCLIRequest(model *Model, prompt *Context, projectID string, opts *googleGeminiCLIOptions, isAntigravity bool, cfg *options) *CloudCodeAssistRequest {
	// Apply TransformContext for cross-provider compatibility
	transformedPrompt := prompt
	if transformed, err := TransformContext(prompt, model, nil); err == nil {
		transformedPrompt = transformed
	}

	contents := convertGeminiCLIMessages(transformedPrompt)

	genConfig := &generationConfig{}
	if cfg.temperature != nil {
		genConfig.Temperature = cfg.temperature
	}
	maxTokens := getMaxTokens(cfg, model)
	if maxTokens > 0 {
		genConfig.MaxOutputTokens = &maxTokens
	}

	// Build thinking config
	if model.Reasoning {
		if opts.thinking != nil && opts.thinking.enabled {
			genConfig.ThinkingConfig = &thinkingConfig{
				IncludeThoughts: true,
			}
			if cfg.googleThinkingLevel != "" {
				genConfig.ThinkingConfig.ThinkingLevel = cfg.googleThinkingLevel
			} else if opts.thinking.level != "" {
				genConfig.ThinkingConfig.ThinkingLevel = opts.thinking.level
			} else if opts.thinking.budgetTokens > 0 {
				genConfig.ThinkingConfig.ThinkingBudget = opts.thinking.budgetTokens
			}
		} else if opts.thinking != nil && !opts.thinking.enabled {
			// Disable thinking for reasoning models
			genConfig.ThinkingConfig = getDisabledThinkingConfig(model.ID)
		}
	}

	innerReq := cloudCodeAssistInnerRequest{
		Contents: contents,
	}

	// Session ID from options
	if cfg.sessionID != "" {
		innerReq.SessionID = cfg.sessionID
	}

	// System instruction
	if prompt.SystemPrompt != "" {
		innerReq.SystemInstruction = &systemInstruction{
			Parts: []systemInstructionPart{
				{Text: SanitizeSurrogates(prompt.SystemPrompt)},
			},
		}
	}

	if genConfig.MaxOutputTokens != nil || genConfig.Temperature != nil || genConfig.ThinkingConfig != nil {
		innerReq.GenerationConfig = genConfig
	}

	// Tools
	if len(prompt.Tools) > 0 {
		tools := convertGeminiCLITools(prompt.Tools, strings.HasPrefix(model.ID, "claude-"))
		innerReq.Tools = tools

		if toolChoiceMode(cfg.toolChoice) != "" {
			innerReq.ToolConfig = &toolConfig{
				FunctionCallingConfig: functionCallingConfig{
					Mode: mapGeminiCLIToolChoice(cfg.toolChoice),
				},
			}
		}
	}

	// Antigravity-specific system instruction
	if isAntigravity && innerReq.SystemInstruction != nil {
		existingParts := innerReq.SystemInstruction.Parts
		antigravityInstruction := getAntigravitySystemInstruction()
		innerReq.SystemInstruction = &systemInstruction{
			Role: "user",
			Parts: []systemInstructionPart{
				{Text: antigravityInstruction},
				{Text: fmt.Sprintf("Please ignore following [ignore]%s[/ignore]", antigravityInstruction)},
			},
		}
		innerReq.SystemInstruction.Parts = append(innerReq.SystemInstruction.Parts, existingParts...)
	}

	requestType := "generate"
	userAgent := ""
	requestPrefix := "generate"
	if isAntigravity {
		requestType = "agent"
		userAgent = "antigravity"
		requestPrefix = "agent"
	}

	// Build request ID
	requestID := fmt.Sprintf("%s-%d-%s", requestPrefix, time.Now().UnixMilli(), rand.Text())

	return &CloudCodeAssistRequest{
		Project:     projectID,
		Model:       model.ID,
		Request:     innerReq,
		RequestType: requestType,
		UserAgent:   userAgent,
		RequestID:   requestID,
	}
}

// getAntigravitySystemInstruction returns the Antigravity system instruction.
func getAntigravitySystemInstruction() string {
	return "You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team working on Advanced Agentic Coding." +
		"You are pair programming with a USER to solve their coding task. The task may require creating a new codebase, modifying or debugging an existing codebase, or simply answering a question." +
		"**Absolute paths only**" +
		"**Proactiveness**"
}

// getDisabledThinkingConfig returns the thinking config to disable thinking for specific models.
func getDisabledThinkingConfig(modelID string) *thinkingConfig {
	if level, ok := disabledGoogleThinkingLevel(modelID); ok {
		return &thinkingConfig{ThinkingLevel: level}
	}
	return &thinkingConfig{ThinkingBudget: 0}
}

// mapGeminiCLIToolChoice maps tool choice option to API string.
func mapGeminiCLIToolChoice(choice any) string {
	switch toolChoiceMode(choice) {
	case "auto":
		return "MODE_UNSPECIFIED"
	case "none":
		return "NONE"
	case "any":
		return "ANY"
	default:
		return "MODE_UNSPECIFIED"
	}
}

// streamGeminiCLIResponse streams the SSE response.
func streamGeminiCLIResponse(ctx context.Context, model *Model, response *http.Response, handler EventHandler, cfg *options) (err error) {
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("google gemini cli: close response body: %w", closeErr))
		}
	}()

	output := AssistantMessage{
		Role:       "assistant",
		API:        APIGoogleGeminiCLI,
		Provider:   model.Provider,
		Model:      model.ID,
		Content:    []ContentBlock{},
		StopReason: StopReasonStop,
		Timestamp:  timestamp(),
		Usage:      Usage{},
	}

	started := false
	ensureStarted := func() {
		if !started {
			handler(ctx, Start{Partial: output})
			started = true
		}
	}

	var currentBlock *ContentBlock
	blocks := output.Content
	blockIndex := func() int { return len(blocks) - 1 }

	reader := NewSSEReader(response.Body)

	for {
		jsonStr, ok := reader.Next()
		if !ok {
			break
		}

		var chunk CloudCodeAssistResponseChunk
		if err := json.Unmarshal([]byte(jsonStr), &chunk); err != nil {
			continue
		}

		respData := chunk.Response
		if respData == nil {
			continue
		}

		// Track response ID
		if respData.ResponseID != "" && output.ResponseID == "" {
			output.ResponseID = respData.ResponseID
		}

		candidate := respData.Candidates
		if len(candidate) == 0 || candidate[0].Content == nil {
			continue
		}

		cand := candidate[0]
		if cand.Content.Parts == nil {
			continue
		}

		for _, part := range cand.Content.Parts {
			isThinking := part.Thought

			if part.Text != "" {

				if currentBlock == nil ||
					(isThinking && currentBlock.Type != ContentBlockThinking) ||
					(!isThinking && currentBlock.Type != ContentBlockText) {
					if currentBlock != nil {
						endGeminiCLIBlock(ctx, handler, output, blockIndex(), *currentBlock)
					}
					if isThinking {
						currentBlock = &ContentBlock{Type: ContentBlockThinking, Thinking: ""}
						output.Content = append(output.Content, *currentBlock)
						ensureStarted()
						handler(ctx, ThinkingStart{ContentIndex: blockIndex(), Partial: output})
					} else {
						currentBlock = &ContentBlock{Type: ContentBlockText, Text: ""}
						output.Content = append(output.Content, *currentBlock)
						ensureStarted()
						handler(ctx, TextStart{ContentIndex: blockIndex(), Partial: output})
					}
				}

				if currentBlock.Type == ContentBlockThinking {
					currentBlock.Thinking += part.Text
					if part.ThoughtSignature != "" {
						currentBlock.ThoughtSignature = retainThoughtSignature(currentBlock.ThoughtSignature, part.ThoughtSignature)
					}
					handler(ctx, ThinkingDelta{
						ContentIndex: blockIndex(),
						Delta:        part.Text,
						Partial:      output,
					})
				} else {
					currentBlock.Text += part.Text
					if part.ThoughtSignature != "" {
						currentBlock.TextSignature = retainThoughtSignature(currentBlock.TextSignature, part.ThoughtSignature)
					}
					handler(ctx, TextDelta{
						ContentIndex: blockIndex(),
						Delta:        part.Text,
						Partial:      output,
					})
				}
			}

			if part.FunctionCall != nil {

				if currentBlock != nil {
					endGeminiCLIBlock(ctx, handler, output, blockIndex(), *currentBlock)
					currentBlock = nil
				}

				providedID := part.FunctionCall.ID
				needsNewID := providedID == "" || HasDuplicateToolCallID(output.Content, providedID)
				toolCallID := providedID
				if needsNewID {
					toolCallID = fmt.Sprintf("%s_%d_%s", part.FunctionCall.Name, time.Now().UnixMilli()%1000000, rand.Text())
				}

				args, _ := json.Marshal(part.FunctionCall.Args)
				toolCallBlock := ToolCall{
					ID:        toolCallID,
					Name:      part.FunctionCall.Name,
					Arguments: args,
				}
				contentBlock := ContentBlock{
					Type:      ContentBlockToolCall,
					ID:        toolCallID,
					Name:      part.FunctionCall.Name,
					Arguments: args,
				}
				output.Content = append(output.Content, contentBlock)

				ensureStarted()
				handler(ctx, ToolCallStart{ContentIndex: blockIndex(), Partial: output})
				handler(ctx, ToolCallDelta{
					ContentIndex: blockIndex(),
					Delta:        string(args),
					Partial:      output,
				})
				handler(ctx, ToolCallEnd{
					ContentIndex: blockIndex(),
					ToolCall:     toolCallBlock,
					Partial:      output,
				})
			}
		}

		// Handle finish reason
		if cand.FinishReason != "" {
			output.StopReason = googleStopReason(cand.FinishReason)
			if hasToolCalls(output.Content) {
				output.StopReason = StopReasonToolUse
			}
		}

		// Handle usage metadata
		if respData.UsageMetadata != nil {
			metadata := respData.UsageMetadata
			promptTokens := metadata.PromptTokenCount
			cacheReadTokens := metadata.CachedContentTokenCount
			output.Usage = Usage{
				Input:       promptTokens - cacheReadTokens,
				Output:      metadata.CandidatesTokenCount + metadata.ThoughtsTokenCount,
				CacheRead:   cacheReadTokens,
				CacheWrite:  0,
				TotalTokens: metadata.TotalTokenCount,
			}
			output.Usage.CalculateCost(model.Cost)
		}
	}

	// End current block
	if currentBlock != nil {
		endGeminiCLIBlock(ctx, handler, output, blockIndex(), *currentBlock)
	}

	if !started {
		return fmt.Errorf("google gemini cli: empty response stream")
	}

	handler(ctx, Done{
		Reason:  output.StopReason,
		Message: output,
	})

	return nil
}

// endGeminiCLIBlock ends the current content block.
func endGeminiCLIBlock(ctx context.Context, handler EventHandler, output AssistantMessage, idx int, block ContentBlock) {
	switch block.Type {
	case ContentBlockThinking:
		handler(ctx, ThinkingEnd{ContentIndex: idx, Content: block.Thinking, Partial: output})
	case ContentBlockText:
		handler(ctx, TextEnd{ContentIndex: idx, Content: block.Text, Partial: output})
	}
}

// convertGeminiCLIMessages converts Context messages to Cloud Code content format.
func convertGeminiCLIMessages(prompt *Context) []cloudCodeContent {
	contents := make([]cloudCodeContent, 0, len(prompt.Messages))

	for _, msg := range prompt.Messages {
		role := mapMessageRoleToGeminiCLI(normalizeMessageRole(msg.Role))
		parts := convertGeminiCLIContent(msg)
		if len(parts) == 0 {
			continue
		}

		// For tool results, append to previous user message with function response
		if role == "user" && normalizeMessageRole(msg.Role) == "toolResult" && len(contents) > 0 {
			last := &contents[len(contents)-1]
			for _, part := range parts {
				if part.FunctionCall != nil {
					last.Parts = append(last.Parts, part)
				}
			}
			continue
		}

		contents = append(contents, cloudCodeContent{
			Role:  role,
			Parts: parts,
		})
	}

	return contents
}

// mapMessageRoleToGeminiCLI maps our role to Cloud Code role.
func mapMessageRoleToGeminiCLI(role string) string {
	switch role {
	case "system":
		return "system"
	case "user":
		return "user"
	case "assistant":
		return "model"
	default:
		return "user"
	}
}

// convertGeminiCLIContent converts message content to Cloud Code parts.
func convertGeminiCLIContent(msg Message) []cloudCodeContentPart {
	parts := make([]cloudCodeContentPart, 0)
	role := normalizeMessageRole(msg.Role)

	switch v := msg.Content.(type) {
	case string:
		if v != "" {
			parts = append(parts, cloudCodeContentPart{Text: v})
		}
	case []ContentBlock:
		if role == "toolResult" {
			return convertGeminiCLIToolResult(msg, v)
		}

		for _, block := range v {
			switch block.Type {
			case ContentBlockText:
				if block.Text != "" {
					parts = append(parts, cloudCodeContentPart{Text: block.Text})
				}
			case ContentBlockThinking:
				if block.Thinking != "" {
					parts = append(parts, cloudCodeContentPart{
						Text:    block.Thinking,
						Thought: "thinking",
					})
				}
			case ContentBlockToolCall:
				// Assistant tool calls become function call parts
				var args map[string]any
				_ = json.Unmarshal(block.Arguments, &args)
				parts = append(parts, cloudCodeContentPart{
					FunctionCall: &functionCallPart{
						Name: block.Name,
						Args: args,
						ID:   block.ID,
					},
				})
			}
		}
	}

	return parts
}

// convertGeminiCLIToolResult converts tool result messages.
func convertGeminiCLIToolResult(msg Message, blocks []ContentBlock) []cloudCodeContentPart {
	parts := make([]cloudCodeContentPart, 0)

	var texts []string
	for _, block := range blocks {
		if block.Type == ContentBlockText && block.Text != "" {
			texts = append(texts, block.Text)
		}
	}

	response := map[string]any{"output": strings.Join(texts, "\n")}
	if msg.IsError {
		response = map[string]any{"error": strings.Join(texts, "\n")}
	}

	parts = append(parts, cloudCodeContentPart{
		FunctionCall: &functionCallPart{
			Name: msg.ToolName,
			Args: response,
			ID:   msg.ToolCallID,
		},
	})

	return parts
}

// convertGeminiCLITools converts tools to Cloud Code format.
func convertGeminiCLITools(tools []Tool, useParameters bool) []any {
	result := make([]any, 0, len(tools))

	for _, tool := range tools {
		funcDecl := make(map[string]any)
		funcDecl["name"] = tool.Name
		if tool.Description != "" {
			funcDecl["description"] = tool.Description
		}
		if tool.Parameters != nil && useParameters {
			// Claude models on Cloud Code need legacy "parameters" field
			var schema map[string]any
			if err := json.Unmarshal(tool.Parameters, &schema); err == nil {
				funcDecl["parameters"] = schema
			}
		}
		result = append(result, map[string]any{"functionDeclarations": []map[string]any{funcDecl}})
	}

	return result
}

// extractRetryDelay extracts retry delay from error response (in milliseconds).
// Checks headers first (retry-after, x-ratelimit-reset, x-ratelimit-reset-after),
// then parses body patterns like:
// - "Your quota will reset after 39s"
// - "Your quota will reset after 18h31m10s"
// - "Please retry in Xs" or "Please retry in Xms"
// - "retryDelay": "34.074824224s" (JSON field)
func extractRetryDelay(errorText string, headers http.Header) int {
	normalizeDelay := func(ms int) int {
		if ms > 0 && ms < 10*24*3600*1000 { // Cap at ~10 days to prevent overflow
			return ms + 1000
		}
		return 0
	}

	if headers != nil {
		// Retry-After header (seconds or RFC1123 date)
		if retryAfter := headers.Get("retry-after"); retryAfter != "" {
			if seconds, err := strconv.ParseFloat(retryAfter, 64); err == nil {
				return normalizeDelay(int(seconds * 1000))
			}
			// Try as date
			if date, err := time.Parse(time.RFC1123, retryAfter); err == nil {
				return normalizeDelay(int(time.Until(date).Milliseconds()))
			}
		}

		// X-Ratelimit-Reset (Unix timestamp)
		if reset := headers.Get("x-ratelimit-reset"); reset != "" {
			if seconds, err := strconv.ParseInt(reset, 10, 64); err == nil {
				return normalizeDelay(int(time.Until(time.Unix(seconds, 0)).Milliseconds()))
			}
		}

		// X-Ratelimit-Reset-After (seconds)
		if resetAfter := headers.Get("x-ratelimit-reset-after"); resetAfter != "" {
			if seconds, err := strconv.ParseFloat(resetAfter, 64); err == nil {
				return normalizeDelay(int(seconds * 1000))
			}
		}
	}

	// Pattern 1: "Your quota will reset after ..." (formats: "18h31m10s", "10m15s", "6s", "39s")
	if matches := regexp.MustCompile(`(?i)reset after (?:(\d+)h)?(?:(\d+)m)?(\d+(?:\.\d+)?)s`).FindStringSubmatch(errorText); len(matches) > 0 {
		hours := 0
		minutes := 0
		if matches[1] != "" {
			hours, _ = strconv.Atoi(matches[1])
		}
		if matches[2] != "" {
			minutes, _ = strconv.Atoi(matches[2])
		}
		seconds, _ := strconv.ParseFloat(matches[3], 64)
		totalMs := ((hours*60+minutes)*60 + int(seconds+0.5)) * 1000
		return normalizeDelay(totalMs)
	}

	// Pattern 2: "Please retry in X[ms|s]"
	if matches := regexp.MustCompile(`(?i)Please retry in ([0-9.]+)(ms|s)`).FindStringSubmatch(errorText); len(matches) > 0 {
		value, _ := strconv.ParseFloat(matches[1], 64)
		multiplier := 1.0
		if strings.ToLower(matches[2]) == "s" {
			multiplier = 1000
		}
		return normalizeDelay(int(value * multiplier))
	}

	// Pattern 3: "retryDelay": "34.074824224s" (JSON field in error details)
	if matches := regexp.MustCompile(`"retryDelay":\s*"([0-9.]+)(ms|s)"`).FindStringSubmatch(errorText); len(matches) > 0 {
		value, _ := strconv.ParseFloat(matches[1], 64)
		multiplier := 1.0
		if strings.ToLower(matches[2]) == "s" {
			multiplier = 1000
		}
		return normalizeDelay(int(value*multiplier + 0.5))
	}

	return 0
}

// ExtractRetryDelay exposes Gemini CLI retry-delay parsing for parity with the
// TS helper export.
func ExtractRetryDelay(errorText string, headers http.Header) int {
	return extractRetryDelay(errorText, headers)
}

// BuildGeminiCLIRequest exposes the Gemini CLI request builder for parity with
// the TS helper export.
func BuildGeminiCLIRequest(model *Model, prompt *Context, projectID string, cfg *options, isAntigravity bool) *CloudCodeAssistRequest {
	if cfg == nil {
		cfg = &options{}
	}

	opts := &googleGeminiCLIOptions{}
	if cfg.reasoning != "" && cfg.reasoning != ReasoningOff {
		opts.thinking = &geminiThinkingConfig{
			enabled:      true,
			budgetTokens: cfg.thinkingBudgetTokens,
			level:        cfg.googleThinkingLevel,
		}
	}
	if cfg.reasoning == ReasoningOff {
		opts.thinking = &geminiThinkingConfig{enabled: false}
	}

	return buildGeminiCLIRequest(model, prompt, projectID, opts, isAntigravity, cfg)
}

// isRetryableError determines if an error is retryable.
func isRetryableError(status int, errorText string) bool {
	if status == http.StatusTooManyRequests || status == http.StatusInternalServerError ||
		status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout {
		return true
	}
	return regexp.MustCompile(`(?i)rate limit|resource exhausted|overloaded|service unavailable|other side closed`).MatchString(errorText)
}

// extractErrorMessage extracts a clean error message from API error response.
func extractErrorMessage(errorText string) string {
	// Try to parse JSON error
	var errResp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(errorText), &errResp); err == nil {
		if errResp.Error.Message != "" {
			return errResp.Error.Message
		}
	}
	return errorText
}
