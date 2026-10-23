package ai // import "github.com/losinggeneration/tau/ai"

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strings"

	genai "google.golang.org/genai"
)

// googleClient is the provider-specific client for Google Generative AI.
type googleClient struct {
	model  *Model
	apiKey string
}

// newGoogleClient creates a new Google Generative AI client.
func newGoogleClient(model *Model) (*googleClient, error) {
	return &googleClient{
		model:  model,
		apiKey: getEnv("GEMINI_API_KEY"),
	}, nil
}

// Complete implements Client.
func (c *googleClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

// Stream implements Client.
func (c *googleClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) error {
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
		apiKey = c.apiKey
	}
	if apiKey == "" {
		return fmt.Errorf("google: missing API key (set GEMINI_API_KEY)")
	}

	requestPayload := googleRequestPayload{
		Model:   model.ID,
		Prompt:  prompt,
		Options: cfg,
	}
	if hasPayloadCallback(cfg) {
		if next := invokePayloadCallback(cfg, model, requestPayload); next != nil {
			if replacement, ok := next.(googleRequestPayload); ok {
				if replacement.Prompt != nil {
					prompt = replacement.Prompt
				}
				if replacement.Options != nil {
					cfg = replacement.Options
				}
			}
		}
	}

	// Create client using the SDK pattern that honors model.headers, model.baseUrl, apiVersion
	client, err := c.createClient(ctx, apiKey, cfg)
	if err != nil {
		return fmt.Errorf("failed to create Google GenAI client: %w", err)
	}

	// Apply TransformContext first for cross-provider compatibility
	// This handles: same-model vs cross-model thinking, synthetic tool results,
	// orphaned tool call handling, and errored/aborted message filtering
	transformedPrompt := prompt
	if transformed, err := TransformContext(prompt, model, NormalizeToolCallIDForGoogle); err == nil {
		transformedPrompt = transformed
	}
	contents := c.convertMessages(transformedPrompt)

	// Build generation config
	config := &genai.GenerateContentConfig{
		MaxOutputTokens: int32(getMaxTokens(cfg, model)),
	}

	// Add system prompt
	if transformedPrompt.SystemPrompt != "" {
		config.SystemInstruction = genai.NewContentFromParts([]*genai.Part{
			genai.NewPartFromText(SanitizeSurrogates(transformedPrompt.SystemPrompt)),
		}, "")
	}

	// Add tools if present with tool config for tool choice
	if len(transformedPrompt.Tools) > 0 {
		declarations, strict, err := googleFunctionDeclarations(model, transformedPrompt.Tools, googleJSONSchemaParameters)
		if err != nil {
			return err
		}
		functions := make([]*genai.FunctionDeclaration, len(declarations))
		for i, declaration := range declarations {
			functions[i] = &genai.FunctionDeclaration{
				Name:                 transformedPrompt.Tools[i].Name,
				Description:          transformedPrompt.Tools[i].Description,
				ParametersJsonSchema: declaration["parametersJsonSchema"],
			}
		}
		config.Tools = []*genai.Tool{{FunctionDeclarations: functions}}
		if mode := googleFunctionCallingMode(cfg.toolChoice, strict); mode != "" {
			config.ToolConfig = &genai.ToolConfig{
				FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigMode(mode)},
			}
		}
	}

	thinkingConfig, err := buildGenAIThinkingConfig(model, cfg)
	if err != nil {
		return err
	}
	config.ThinkingConfig = thinkingConfig

	// Add temperature if set
	if cfg.temperature != nil {
		t := float32(*cfg.temperature)
		config.Temperature = &t
	}

	// Add topP if set
	if cfg.topP != nil {
		p := float32(*cfg.topP)
		config.TopP = &p
	}

	return streamGoogleChunks(ctx, model, retryGoogleStream(ctx, cfg, func() iter.Seq2[*genai.GenerateContentResponse, error] {
		return client.Models.GenerateContentStream(ctx, model.ID, contents, config)
	}), handler)
}

// retryGoogleStream retries a stream whose first result is a retryable
// failure, matching upstream's retryGoogleRequest. Later failures are not retried.
func retryGoogleStream(ctx context.Context, cfg *options, start func() iter.Seq2[*genai.GenerateContentResponse, error]) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		retries := 0
		if cfg.maxRetries != nil {
			retries = *cfg.maxRetries
		}
		for attempt := 0; ; attempt++ {
			next, stop := iter.Pull2(start())
			chunk, err, ok := next()
			if ok && err != nil {
				stop()
				var status int
				if apiErr, isAPI := errors.AsType[genai.APIError](err); isAPI {
					status = apiErr.Code
				}
				retry, waitErr := waitProviderRetry(ctx, "google", cfg, attempt, retries, status, nil, providerBackoff(attempt), err)
				if waitErr != nil {
					yield(nil, waitErr)
					return
				}
				if retry {
					continue
				}
				yield(nil, err)
				return
			}
			defer stop()
			for ok && yield(chunk, err) {
				chunk, err, ok = next()
			}
			return
		}
	}
}

// streamGoogleChunks converts a GenAI response stream into Tau stream events.
func streamGoogleChunks(ctx context.Context, model *Model, chunks iter.Seq2[*genai.GenerateContentResponse, error], handler EventHandler) error {
	// Set up streaming with shared context for tool call handling
	var (
		sharedCtx = &googleStreamingCtx{
			model:           model,
			toolCallCounter: 0,
			lastThoughtSig:  "",
			lastTextSig:     "",
		}
		currentBlock *ContentBlock
		output       = AssistantMessage{
			Role:       "assistant",
			API:        APIGoogleGenerativeAI,
			Provider:   model.Provider,
			Model:      model.ID,
			Content:    []ContentBlock{},
			StopReason: StopReasonStop,
			Timestamp:  timestamp(),
			Usage:      Usage{},
		}
	)
	handler(ctx, Start{Partial: output})

	// Iterate over the stream - errors come from the iterator itself

	var finished bool
	for chunk, err := range chunks {
		if err != nil {
			reason := StopReasonError
			if ctx.Err() != nil {
				reason = StopReasonAborted
			}
			emitTerminalStreamError(ctx, handler, output, err, reason)
			return nil
		}

		// Handle candidates
		for _, cand := range chunk.Candidates {
			var parts []*genai.Part
			if cand.Content != nil {
				parts = cand.Content.Parts
			}

			for _, part := range parts {
				// Handle text content
				if part.Text != "" {
					blockType := ContentBlockText
					eventTypeThinking := part.Thought
					if currentBlock == nil || currentBlock.Type != blockType || (blockType == ContentBlockText && currentBlock.Thinking != "" && !eventTypeThinking) || (eventTypeThinking && currentBlock.Type != ContentBlockThinking) {
						if currentBlock != nil {
							endGoogleBlock(ctx, handler, output, len(output.Content)-1, *currentBlock)
						}
						if eventTypeThinking {
							output.Content = append(output.Content, ContentBlock{Type: ContentBlockThinking})
							currentBlock = &output.Content[len(output.Content)-1]
							handler(ctx, ThinkingStart{ContentIndex: len(output.Content) - 1, Partial: output})
						} else {
							output.Content = append(output.Content, ContentBlock{Type: ContentBlockText})
							currentBlock = &output.Content[len(output.Content)-1]
							handler(ctx, TextStart{ContentIndex: len(output.Content) - 1, Partial: output})
						}
					}

					if eventTypeThinking {
						currentBlock.Thinking += part.Text
						// P7-GG08: Retain thoughtSignature on thinking blocks (ThoughtSignature is []byte)
						currentBlock.ThoughtSignature = retainThoughtSignature(currentBlock.ThoughtSignature, string(part.ThoughtSignature))
						handler(ctx, ThinkingDelta{
							ContentIndex: len(output.Content) - 1,
							Delta:        part.Text,
							Partial:      output,
						})
					} else {
						currentBlock.Text += part.Text
						// P7-GG08: Retain textSignature on text blocks (ThoughtSignature is []byte)
						currentBlock.TextSignature = retainThoughtSignature(currentBlock.TextSignature, string(part.ThoughtSignature))
						handler(ctx, TextDelta{
							ContentIndex: len(output.Content) - 1,
							Delta:        part.Text,
							Partial:      output,
						})
					}
				}

				// Handle function calls
				if part.FunctionCall != nil {
					if currentBlock != nil {
						endGoogleBlock(ctx, handler, output, len(output.Content)-1, *currentBlock)
						currentBlock = nil
					}

					sharedCtx.toolCallCounter++
					args, err := json.Marshal(part.FunctionCall.Args)
					if err != nil {
						return fmt.Errorf("google: marshal args: %w", err)
					}

					// P7-GG06: Generate unique ID if not provided or if it's a duplicate
					// Format: {name}_{Date.now()}_{counter}
					providedID := part.FunctionCall.ID
					needsNewID := providedID == "" || HasDuplicateToolCallID(output.Content, providedID)
					var toolCallID string
					if needsNewID {
						if requiresToolCallID(model.ID) {
							// For models requiring tool call IDs, use normalized format
							toolCallID = NormalizeToolCallIDForGoogle(fmt.Sprintf("%s_%d_%d",
								part.FunctionCall.Name, timestamp(), sharedCtx.toolCallCounter), model, nil)
						} else {
							toolCallID = fmt.Sprintf("%s_%d_%d", part.FunctionCall.Name, timestamp(), sharedCtx.toolCallCounter)
						}
					} else {
						toolCallID = providedID
					}

					toolCall := ToolCall{
						ID:        toolCallID,
						Name:      part.FunctionCall.Name,
						Arguments: args,
					}
					output.Content = append(output.Content, ContentBlock{
						Type:             ContentBlockToolCall,
						ID:               toolCall.ID,
						Name:             toolCall.Name,
						Arguments:        toolCall.Arguments,
						ThoughtSignature: string(part.ThoughtSignature), // P7-GG07: Set thoughtSignature on tool call block
					})
					idx := len(output.Content) - 1
					handler(ctx, ToolCallStart{ContentIndex: idx, Partial: output})
					handler(ctx, ToolCallDelta{
						ContentIndex: idx,
						Delta:        string(args),
						Partial:      output,
					})
					handler(ctx, ToolCallEnd{
						ContentIndex: idx,
						ToolCall:     toolCall,
						Partial:      output,
					})
				}
			}

			if cand.FinishReason != "" {
				finished = true
				output.StopReason, output.ErrorMessage = googleFinish(string(cand.FinishReason), output.Content)
			}
		}

		if chunk.ResponseID != "" {
			output.ResponseID = chunk.ResponseID
		}

		// Handle usage metadata
		if chunk.UsageMetadata != nil {
			output.Usage = Usage{
				Input:      int(chunk.UsageMetadata.PromptTokenCount) - int(chunk.UsageMetadata.CachedContentTokenCount),
				Output:     int(chunk.UsageMetadata.CandidatesTokenCount) + int(chunk.UsageMetadata.ThoughtsTokenCount),
				CacheRead:  int(chunk.UsageMetadata.CachedContentTokenCount),
				CacheWrite: 0,
			}
			output.Usage.CalculateCost(model.Cost)
			handler(ctx, &output.Usage)
		}
	}

	if currentBlock != nil {
		endGoogleBlock(ctx, handler, output, len(output.Content)-1, *currentBlock)
	}

	finishStream(ctx, handler, output, finished, "google stream ended without a finish reason")

	return nil
}

func buildGenAIThinkingConfig(model *Model, cfg *options) (*genai.ThinkingConfig, error) {
	thinking, ok, err := resolveGoogleThinking(model, cfg)
	if err != nil || !ok {
		return nil, err
	}
	config := &genai.ThinkingConfig{IncludeThoughts: thinking.enabled}
	if level, ok := googleThinkingLevelToGenAI(thinking.level); ok {
		config.ThinkingLevel = level
	} else if thinking.budget != nil {
		config.ThinkingBudget = new(int32(*thinking.budget))
	}
	return config, nil
}

func googleThinkingLevelToGenAI(level GoogleThinkingLevel) (genai.ThinkingLevel, bool) {
	switch level {
	case GoogleThinkingLevelMinimal:
		return genai.ThinkingLevelMinimal, true
	case GoogleThinkingLevelLow:
		return genai.ThinkingLevelLow, true
	case GoogleThinkingLevelMedium:
		return genai.ThinkingLevelMedium, true
	case GoogleThinkingLevelHigh:
		return genai.ThinkingLevelHigh, true
	default:
		return "", false
	}
}

type googleRequestPayload struct {
	Model   string
	Prompt  *Context
	Options *options
}

func endGoogleBlock(ctx context.Context, handler EventHandler, output AssistantMessage, idx int, block ContentBlock) {
	switch block.Type {
	case ContentBlockThinking:
		handler(ctx, ThinkingEnd{ContentIndex: idx, Content: block.Thinking, Partial: output})
	case ContentBlockText:
		handler(ctx, TextEnd{ContentIndex: idx, Content: block.Text, Partial: output})
	}
}

// convertMessages converts our Context messages to Google Contents.
func (c *googleClient) convertMessages(prompt *Context) []*genai.Content {
	contents := make([]*genai.Content, 0, len(prompt.Messages))

	for _, msg := range prompt.Messages {
		role := c.convertRole(normalizeMessageRole(msg.Role))
		parts := c.convertContent(msg)
		if len(parts) == 0 {
			continue
		}

		if role == "user" && normalizeMessageRole(msg.Role) == "toolResult" && len(contents) > 0 {
			last := contents[len(contents)-1]
			if last.Role == "user" && googleHasFunctionResponse(last) {
				last.Parts = append(last.Parts, parts...)
				continue
			}
		}

		contents = append(contents, genai.NewContentFromParts(parts, role))
	}

	return contents
}

// convertRole converts our role to Google's role.
func (c *googleClient) convertRole(role string) genai.Role {
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

// convertContent converts our content to Google Parts.
func (c *googleClient) convertContent(msg Message) []*genai.Part {
	parts := make([]*genai.Part, 0)
	role := normalizeMessageRole(msg.Role)

	switch v := msg.Content.(type) {
	case string:
		if v != "" {
			parts = append(parts, genai.NewPartFromText(v))
		}
	case []ContentBlock:
		if role == "toolResult" {
			return googleToolResultParts(msg, v)
		}

		for _, block := range v {
			switch block.Type {
			case ContentBlockText:
				parts = append(parts, genai.NewPartFromText(block.Text))
			case ContentBlockToolCall:
				// Assistant tool use - convert to function response
				// Arguments is json.RawMessage, need to unmarshal to map[string]any
				var args map[string]any
				if err := json.Unmarshal(block.Arguments, &args); err != nil {
					args = nil
				}

				part := genai.NewPartFromFunctionCall(block.Name, args)

				// Thought signature: prefer the resolved one (validated base64,
				// same provider/model). For Gemini 3 unsigned function calls,
				// use the skip-validator sentinel.
				signature := ResolveThoughtSignature(true, block.ThoughtSignature)
				if signature == "" && isGemini3Model(c.model.ID) {
					signature = SkipThoughtSignatureValidator
				}
				if signature != "" {
					part.ThoughtSignature = []byte(signature)
				}

				parts = append(parts, part)
			}
		}
	}

	return parts
}

// googleStreamingCtx holds state used during streaming for Google Generative AI.
type googleStreamingCtx struct {
	model           *Model
	toolCallCounter int64
	lastThoughtSig  string
	lastTextSig     string
}

// createClient creates a Google GenAI client that honors model headers and base URL.
// This matches pi-mono's behavior of merging model.headers with options.headers.
func (c *googleClient) createClient(ctx context.Context, apiKey string, cfg *options) (*genai.Client, error) {
	optionsHeaders := cfg.headers
	httpOpts := &genai.HTTPOptions{}

	// Honor model.baseUrl if present
	if c.model.BaseURL != "" {
		httpOpts.BaseURL = c.model.BaseURL
		httpOpts.APIVersion = "" // baseUrl already includes version path
	}

	// Merge model.headers and optionsHeaders
	if c.model.Headers != nil || len(optionsHeaders) > 0 {
		headers := make(map[string]string)
		for k, v := range c.model.Headers {
			headers[k] = v
		}
		copyConfiguredHeaders(headers, optionsHeaders)
		if len(headers) > 0 {
			httpOpts.Headers = http.Header{}
			for k, v := range headers {
				httpOpts.Headers.Set(k, v)
			}
		}
	}

	config := &genai.ClientConfig{APIKey: apiKey, HTTPOptions: *httpOpts}
	if cfg.timeout > 0 {
		base := cmp.Or(http.DefaultClient.Transport, http.DefaultTransport)
		config.HTTPClient = &http.Client{Transport: &headerTimeoutTransport{base: base, timeout: cfg.timeout, label: "google"}}
	}
	return genai.NewClient(ctx, config)
}

func googleToolResultParts(msg Message, blocks []ContentBlock) []*genai.Part {
	var textParts []string
	for _, block := range blocks {
		if block.Type == ContentBlockText && block.Text != "" {
			textParts = append(textParts, block.Text)
		}
	}

	response := map[string]any{"output": strings.Join(textParts, "\n")}
	if msg.IsError {
		response = map[string]any{"error": strings.Join(textParts, "\n")}
	}

	return []*genai.Part{
		genai.NewPartFromFunctionResponse(msg.ToolName, response),
	}
}

func googleHasFunctionResponse(content *genai.Content) bool {
	if content == nil {
		return false
	}
	for _, part := range content.Parts {
		if part.FunctionResponse != nil {
			return true
		}
	}
	return false
}
