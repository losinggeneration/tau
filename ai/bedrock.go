package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bedrockdocument "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

type bedrockClientImpl struct {
	model *Model
}

type bedrockStreamBlock struct {
	ContentBlock
	Index       int32
	PartialJSON string
}

// bedrockErrorPrefixes maps AWS Bedrock exception names to stable
// human-readable prefixes. The prefixes are matched by
// nonOverflowPatterns in ai/overflow.go to distinguish error categories.
// Mirrors pi-mono's BEDROCK_ERROR_PREFIXES in providers/amazon-bedrock.ts.
var bedrockErrorPrefixes = map[string]string{
	"InternalServerException":     "Internal server error",
	"ModelStreamErrorException":   "Model stream error",
	"ValidationException":         "Validation error",
	"ThrottlingException":         "Throttling error",
	"ServiceUnavailableException": "Service unavailable",
}

// formatBedrockError prefixes a Bedrock error with a stable category name so
// downstream consumers (retry, overflow detection) can match on simple
// strings. The raw message is preserved after the prefix.
func formatBedrockError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()

	// Walk the wrap chain to find a typed Bedrock exception name.
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		type namedError interface{ ErrorName() string }
		if ne, ok := cur.(namedError); ok {
			if prefix, found := bedrockErrorPrefixes[ne.ErrorName()]; found {
				return prefix + ": " + msg
			}
		}
	}

	// Fallback: scan the message for the exception class name. The AWS SDK
	// encodes the exception name in the error string when it cannot
	// preserve the typed error.
	for name, prefix := range bedrockErrorPrefixes {
		if strings.Contains(msg, name) {
			return prefix + ": " + msg
		}
	}

	return msg
}

func newBedrockClientForModel(model *Model) (*bedrockClientImpl, error) {
	if model == nil {
		return nil, errors.New("bedrock: model is nil")
	}
	return &bedrockClientImpl{model: model}, nil
}

// Complete implements Client.
//
// Note: bedrock intentionally does NOT delegate to the centralized
// completeViaStream helper in helpers.go. It has two behavioral
// divergences from the standard pattern that make delegation unsafe:
//   - It does not handle StreamError events (the handler only matches
//     Done), so a terminal StreamError would be silently dropped.
//   - On Stream returning a Go error, it returns a fully-populated
//     AssistantMessage with StopReason=Error and ErrorMessage set,
//     instead of returning (nil, err). This is the bedrock-specific
//     error model and changing it would be a behavior change for
//     existing callers. Bedrock can be migrated to the helper when its
//     error model is revisited; for now it keeps its inline handler.
func (c *bedrockClientImpl) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	var result *AssistantMessage
	err := c.Stream(ctx, model, prompt, func(_ context.Context, event StreamEvent) {
		if done, ok := event.(Done); ok {
			msg := done.Message
			result = &msg
		}
	}, opts...)
	if err != nil {
		return &AssistantMessage{
			Role:         "assistant",
			API:          APIBedrockConverseStream,
			Provider:     model.Provider,
			Model:        model.ID,
			StopReason:   StopReasonError,
			ErrorMessage: err.Error(),
			Timestamp:    time.Now().UnixMilli(),
		}, nil
	}
	if result == nil {
		return nil, errors.New("bedrock: stream completed without result")
	}
	return result, nil
}

func (c *bedrockClientImpl) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) error {
	if model == nil {
		return errors.New("bedrock: model is nil")
	}
	if prompt == nil {
		return errors.New("bedrock: prompt is nil")
	}

	o := applyOptions(opts)
	client, err := newBedrockRuntimeClient(ctx, o)
	if err != nil {
		return err
	}

	input, err := buildBedrockConverseInput(model, prompt, o)
	if err != nil {
		return err
	}
	if hasPayloadCallback(o) {
		if next := invokePayloadCallback(o, model, input); next != nil {
			converted, ok := next.(*bedrockruntime.ConverseStreamInput)
			if !ok {
				return fmt.Errorf("bedrock: onPayload returned %T, want *bedrockruntime.ConverseStreamInput", next)
			}
			input = converted
		}
	}

	output := AssistantMessage{
		Role:       "assistant",
		API:        APIBedrockConverseStream,
		Provider:   model.Provider,
		Model:      model.ID,
		Content:    []ContentBlock{},
		Usage:      Usage{},
		StopReason: StopReasonStop,
		Timestamp:  time.Now().UnixMilli(),
	}
	blocks := make([]*bedrockStreamBlock, 0)

	resp, err := client.ConverseStream(ctx, input)
	if err != nil {
		return fmt.Errorf("bedrock: failed to start stream: %s", formatBedrockError(err))
	}
	defer func() {
		_ = resp.GetStream().Close()
	}()

	if hasResponseCallback(o) {
		invokeResponseCallback(o, model, ProviderResponse{
			Status: 200,
		})
	}

	for event := range resp.GetStream().Events() {
		switch e := event.(type) {
		case *bedrocktypes.ConverseStreamOutputMemberMessageStart:
			if e.Value.Role != bedrocktypes.ConversationRoleAssistant {
				return fmt.Errorf("bedrock: unexpected message start role %q", e.Value.Role)
			}
			handler(ctx, Start{Partial: output})

		case *bedrocktypes.ConverseStreamOutputMemberContentBlockStart:
			handleBedrockContentBlockStart(ctx, &e.Value, &blocks, &output, handler)

		case *bedrocktypes.ConverseStreamOutputMemberContentBlockDelta:
			handleBedrockContentBlockDelta(ctx, &e.Value, &blocks, &output, handler)

		case *bedrocktypes.ConverseStreamOutputMemberContentBlockStop:
			handleBedrockContentBlockStop(ctx, &e.Value, blocks, &output, handler)

		case *bedrocktypes.ConverseStreamOutputMemberMessageStop:
			output.StopReason = mapBedrockStopReason(e.Value.StopReason)

		case *bedrocktypes.ConverseStreamOutputMemberMetadata:
			handleBedrockMetadata(&e.Value, model, &output)
		}

		select {
		case <-ctx.Done():
			err := ctx.Err()
			if err == nil {
				err = errors.New("request aborted")
			}
			emitTerminalStreamError(ctx, handler, output, err, StopReasonAborted)
			return nil
		default:
		}
	}

	if err := resp.GetStream().Err(); err != nil {
		emitTerminalStreamError(ctx, handler, output, fmt.Errorf("bedrock: stream error: %s", formatBedrockError(err)), StopReasonError)
		return nil
	}
	if output.StopReason == StopReasonError || output.StopReason == StopReasonAborted {
		emitTerminalStreamError(ctx, handler, output, errors.New(output.ErrorMessage), output.StopReason)
		return nil
	}
	handler(ctx, Done{Reason: output.StopReason, Message: output})
	return nil
}

func newBedrockRuntimeClient(ctx context.Context, o *options) (*bedrockruntime.Client, error) {
	loadOptions := make([]func(*awsconfig.LoadOptions) error, 0, 2)
	if region := resolveBedrockRegion(o); region != "" {
		loadOptions = append(loadOptions, awsconfig.WithRegion(region))
	}
	if o != nil && o.bedrockProfile != "" {
		loadOptions = append(loadOptions, awsconfig.WithSharedConfigProfile(o.bedrockProfile))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("bedrock: load config: %w", err)
	}

	client := bedrockruntime.NewFromConfig(cfg, func(options *bedrockruntime.Options) {
		if o == nil {
			return
		}
		if len(o.headers) > 0 || o.bedrockBearerToken != "" {
			options.HTTPClient = &http.Client{
				Transport: &bedrockHeaderRoundTripper{
					base:        http.DefaultTransport,
					headers:     cloneStringMap(o.headers),
					bearerToken: resolveBedrockBearerToken(o),
				},
			}
		}
	})

	return client, nil
}

func resolveBedrockRegion(o *options) string {
	if o != nil && o.bedrockRegion != "" {
		return o.bedrockRegion
	}
	if region := os.Getenv("AWS_REGION"); region != "" {
		return region
	}
	if region := os.Getenv("AWS_DEFAULT_REGION"); region != "" {
		return region
	}
	return "us-east-1"
}

func resolveBedrockBearerToken(o *options) string {
	if o != nil && o.bedrockBearerToken != "" {
		return o.bedrockBearerToken
	}
	return os.Getenv("AWS_BEARER_TOKEN_BEDROCK")
}

func buildBedrockConverseInput(model *Model, prompt *Context, o *options) (*bedrockruntime.ConverseStreamInput, error) {
	cacheRetention := resolveCacheRetention(o.cacheRetention)
	messages, err := convertBedrockMessages(prompt, model, cacheRetention)
	if err != nil {
		return nil, err
	}
	system, err := buildBedrockSystemPrompt(prompt.SystemPrompt, model, cacheRetention)
	if err != nil {
		return nil, err
	}

	inferenceConfig := &bedrocktypes.InferenceConfiguration{}
	if o.temperature != nil {
		temp := float32(*o.temperature)
		inferenceConfig.Temperature = &temp
	}
	if o.topP != nil {
		topP := float32(*o.topP)
		inferenceConfig.TopP = &topP
	}
	if maxTokens := getMaxTokens(o, model); maxTokens > 0 {
		mt := int32(maxTokens)
		inferenceConfig.MaxTokens = &mt
	}
	if inferenceConfig.Temperature == nil && inferenceConfig.TopP == nil && inferenceConfig.MaxTokens == nil {
		inferenceConfig = nil
	}

	additionalFields, err := buildBedrockAdditionalModelRequestFields(model, o)
	if err != nil {
		return nil, err
	}
	toolConfig, err := buildBedrockToolConfig(prompt.Tools, o.toolChoice)
	if err != nil {
		return nil, err
	}

	return &bedrockruntime.ConverseStreamInput{
		ModelId:                      aws.String(model.ID),
		Messages:                     messages,
		System:                       system,
		InferenceConfig:              inferenceConfig,
		ToolConfig:                   toolConfig,
		AdditionalModelRequestFields: bedrockDocumentOrNil(additionalFields),
		RequestMetadata:              cloneStringMap(o.bedrockRequestMetadata),
	}, nil
}

func convertBedrockMessages(prompt *Context, model *Model, cacheRetention CacheRetention) ([]bedrocktypes.Message, error) {
	transformed, err := TransformContext(prompt, model, normalizeToolCallIDForBedrock)
	if err != nil {
		return nil, err
	}

	result := make([]bedrocktypes.Message, 0, len(transformed.Messages))
	for i := 0; i < len(transformed.Messages); i++ {
		msg := transformed.Messages[i]
		role := normalizeMessageRole(msg.Role)

		switch role {
		case "user":
			content, err := convertBedrockUserContent(msg)
			if err != nil {
				return nil, err
			}
			if len(content) == 0 {
				continue
			}
			result = append(result, bedrocktypes.Message{
				Role:    bedrocktypes.ConversationRoleUser,
				Content: content,
			})

		case "assistant":
			content, err := convertBedrockAssistantContent(msg, model)
			if err != nil {
				return nil, err
			}
			if len(content) == 0 {
				continue
			}
			result = append(result, bedrocktypes.Message{
				Role:    bedrocktypes.ConversationRoleAssistant,
				Content: content,
			})

		case "toolResult":
			toolResults := make([]bedrocktypes.ContentBlock, 0, 2)
			for ; i < len(transformed.Messages) && normalizeMessageRole(transformed.Messages[i].Role) == "toolResult"; i++ {
				content, err := convertBedrockToolResult(transformed.Messages[i])
				if err != nil {
					return nil, err
				}
				toolResults = append(toolResults, content)
			}
			i--
			if len(toolResults) == 0 {
				continue
			}
			result = append(result, bedrocktypes.Message{
				Role:    bedrocktypes.ConversationRoleUser,
				Content: toolResults,
			})
		}
	}

	if cacheRetention != CacheRetentionNone && supportsBedrockPromptCaching(model) && len(result) > 0 {
		last := &result[len(result)-1]
		if last.Role == bedrocktypes.ConversationRoleUser {
			last.Content = append(last.Content, &bedrocktypes.ContentBlockMemberCachePoint{
				Value: bedrocktypes.CachePointBlock{
					Type: bedrocktypes.CachePointTypeDefault,
					Ttl:  bedrockCacheTTL(cacheRetention),
				},
			})
		}
	}

	return result, nil
}

func convertBedrockUserContent(msg Message) ([]bedrocktypes.ContentBlock, error) {
	switch c := msg.Content.(type) {
	case string:
		if c == "" {
			return nil, nil
		}
		return []bedrocktypes.ContentBlock{
			&bedrocktypes.ContentBlockMemberText{Value: SanitizeSurrogates(c)},
		}, nil
	case []ContentBlock:
		content := make([]bedrocktypes.ContentBlock, 0, len(c))
		for _, block := range c {
			switch block.Type {
			case ContentBlockText:
				if block.Text == "" {
					continue
				}
				content = append(content, &bedrocktypes.ContentBlockMemberText{Value: SanitizeSurrogates(block.Text)})
			case ContentBlockImage:
				image, err := buildBedrockImage(block.MimeType, block.Data)
				if err != nil {
					return nil, err
				}
				content = append(content, &bedrocktypes.ContentBlockMemberImage{Value: image})
			}
		}
		return content, nil
	default:
		return nil, nil
	}
}

func convertBedrockAssistantContent(msg Message, model *Model) ([]bedrocktypes.ContentBlock, error) {
	blocks := msg.ContentBlocks()
	content := make([]bedrocktypes.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case ContentBlockText:
			if strings.TrimSpace(block.Text) == "" {
				continue
			}
			content = append(content, &bedrocktypes.ContentBlockMemberText{Value: SanitizeSurrogates(block.Text)})
		case ContentBlockToolCall:
			var args any
			if len(block.Arguments) > 0 {
				if err := json.Unmarshal(block.Arguments, &args); err != nil {
					return nil, fmt.Errorf("bedrock: decode tool call arguments: %w", err)
				}
			}
			content = append(content, &bedrocktypes.ContentBlockMemberToolUse{
				Value: bedrocktypes.ToolUseBlock{
					ToolUseId: aws.String(block.ID),
					Name:      aws.String(block.Name),
					Input:     bedrockDocumentOrNil(args),
				},
			})
		case ContentBlockThinking:
			if strings.TrimSpace(block.Thinking) == "" && !block.Redacted {
				continue
			}
			if block.Redacted {
				content = append(content, &bedrocktypes.ContentBlockMemberReasoningContent{
					Value: &bedrocktypes.ReasoningContentBlockMemberRedactedContent{Value: []byte(block.ThoughtSignature)},
				})
				continue
			}
			value := &bedrocktypes.ReasoningTextBlock{
				Text: aws.String(SanitizeSurrogates(block.Thinking)),
			}
			if supportsBedrockThinkingSignature(model) && block.ThoughtSignature != "" {
				value.Signature = aws.String(block.ThoughtSignature)
			}
			if !supportsBedrockThinkingSignature(model) && block.ThoughtSignature == "" {
				content = append(content, &bedrocktypes.ContentBlockMemberText{Value: SanitizeSurrogates(block.Thinking)})
				continue
			}
			content = append(content, &bedrocktypes.ContentBlockMemberReasoningContent{
				Value: &bedrocktypes.ReasoningContentBlockMemberReasoningText{Value: *value},
			})
		}
	}
	return content, nil
}

func convertBedrockToolResult(msg Message) (bedrocktypes.ContentBlock, error) {
	blocks := msg.ContentBlocks()
	resultBlocks := make([]bedrocktypes.ToolResultContentBlock, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case ContentBlockText:
			resultBlocks = append(resultBlocks, &bedrocktypes.ToolResultContentBlockMemberText{
				Value: SanitizeSurrogates(block.Text),
			})
		case ContentBlockImage:
			image, err := buildBedrockImage(block.MimeType, block.Data)
			if err != nil {
				return nil, err
			}
			resultBlocks = append(resultBlocks, &bedrocktypes.ToolResultContentBlockMemberImage{Value: image})
		}
	}
	status := bedrocktypes.ToolResultStatusSuccess
	if msg.IsError {
		status = bedrocktypes.ToolResultStatusError
	}
	return &bedrocktypes.ContentBlockMemberToolResult{
		Value: bedrocktypes.ToolResultBlock{
			ToolUseId: aws.String(msg.ToolCallID),
			Content:   resultBlocks,
			Status:    status,
			Type:      aws.String("tool_result"),
		},
	}, nil
}

func buildBedrockSystemPrompt(systemPrompt string, model *Model, cacheRetention CacheRetention) ([]bedrocktypes.SystemContentBlock, error) {
	if systemPrompt == "" {
		return nil, nil
	}
	blocks := []bedrocktypes.SystemContentBlock{
		&bedrocktypes.SystemContentBlockMemberText{Value: SanitizeSurrogates(systemPrompt)},
	}
	if cacheRetention != CacheRetentionNone && supportsBedrockPromptCaching(model) {
		blocks = append(blocks, &bedrocktypes.SystemContentBlockMemberCachePoint{
			Value: bedrocktypes.CachePointBlock{
				Type: bedrocktypes.CachePointTypeDefault,
				Ttl:  bedrockCacheTTL(cacheRetention),
			},
		})
	}
	return blocks, nil
}

func buildBedrockToolConfig(tools []Tool, choice any) (*bedrocktypes.ToolConfiguration, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	if s, ok := choice.(string); ok && s == "none" {
		return nil, nil
	}

	out := &bedrocktypes.ToolConfiguration{
		Tools: make([]bedrocktypes.Tool, 0, len(tools)),
	}
	for _, tool := range tools {
		var schema any
		if len(tool.Parameters) > 0 {
			if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
				return nil, fmt.Errorf("bedrock: decode tool schema: %w", err)
			}
		}
		strict := false
		out.Tools = append(out.Tools, &bedrocktypes.ToolMemberToolSpec{
			Value: bedrocktypes.ToolSpecification{
				Name:        aws.String(tool.Name),
				Description: aws.String(tool.Description),
				InputSchema: &bedrocktypes.ToolInputSchemaMemberJson{Value: bedrockDocumentOrNil(schema)},
				Strict:      aws.Bool(strict),
			},
		})
	}

	switch v := choice.(type) {
	case string:
		switch v {
		case "auto":
			out.ToolChoice = &bedrocktypes.ToolChoiceMemberAuto{Value: bedrocktypes.AutoToolChoice{}}
		case "any", "required":
			out.ToolChoice = &bedrocktypes.ToolChoiceMemberAny{Value: bedrocktypes.AnyToolChoice{}}
		}
	case map[string]any:
		if kind, _ := v["type"].(string); kind == "tool" || kind == "function" {
			name := ""
			if kind == "tool" {
				name, _ = v["name"].(string)
			}
			if name == "" {
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
			}
			if name != "" {
				out.ToolChoice = &bedrocktypes.ToolChoiceMemberTool{Value: bedrocktypes.SpecificToolChoice{Name: aws.String(name)}}
			}
		}
	}

	return out, nil
}

func buildBedrockAdditionalModelRequestFields(model *Model, o *options) (any, error) {
	if model == nil || o == nil || o.reasoning == "" || o.reasoning == ReasoningOff || !model.Reasoning {
		return nil, nil
	}
	if !strings.Contains(strings.ToLower(model.ID), "claude") {
		return nil, nil
	}

	display := o.thinkingDisplay
	if display == "" {
		display = "summarized"
	}

	if supportsAdaptiveThinking(model.ID) {
		return map[string]any{
			"thinking": map[string]any{
				"type":    "adaptive",
				"display": display,
			},
			"output_config": map[string]any{
				"effort": mapBedrockThinkingEffort(o.reasoning, model.ID),
			},
		}, nil
	}

	budget := ThinkingBudgetForLevel(o.reasoning, o.bedrockThinkingBudgets)
	result := map[string]any{
		"thinking": map[string]any{
			"type":          "enabled",
			"budget_tokens": budget,
			"display":       display,
		},
	}
	if o.bedrockInterleavedThinking == nil || *o.bedrockInterleavedThinking {
		result["anthropic_beta"] = []string{"interleaved-thinking-2025-05-14"}
	}
	return result, nil
}

func supportsAdaptiveThinking(modelID string) bool {
	id := strings.ToLower(modelID)
	return strings.Contains(id, "opus-4-6") || strings.Contains(id, "opus-4.6") ||
		strings.Contains(id, "opus-4-7") || strings.Contains(id, "opus-4.7") ||
		strings.Contains(id, "sonnet-4-6") || strings.Contains(id, "sonnet-4.6")
}

func supportsBedrockPromptCaching(model *Model) bool {
	if model == nil {
		return false
	}
	id := strings.ToLower(model.ID)
	if !strings.Contains(id, "claude") {
		return os.Getenv("AWS_BEDROCK_FORCE_CACHE") == "1"
	}
	return strings.Contains(id, "-4-") || strings.Contains(id, "-4.") ||
		strings.Contains(id, "claude-3-7-sonnet") || strings.Contains(id, "claude-3-5-haiku")
}

func supportsBedrockThinkingSignature(model *Model) bool {
	if model == nil {
		return false
	}
	id := strings.ToLower(model.ID)
	return strings.Contains(id, "anthropic.claude") || strings.Contains(id, "anthropic/claude")
}

func mapBedrockThinkingEffort(level ReasoningLevel, modelID string) string {
	switch level {
	case ReasoningMinimal, ReasoningLow:
		return "low"
	case ReasoningMedium:
		return "medium"
	case ReasoningXHigh:
		lower := strings.ToLower(modelID)
		if strings.Contains(lower, "opus-4-6") || strings.Contains(lower, "opus-4.6") {
			return "max"
		}
		if strings.Contains(lower, "opus-4-7") || strings.Contains(lower, "opus-4.7") {
			return "xhigh"
		}
		return "high"
	default:
		return "high"
	}
}

func buildBedrockImage(mimeType string, data string) (bedrocktypes.ImageBlock, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return bedrocktypes.ImageBlock{}, fmt.Errorf("bedrock: decode image: %w", err)
	}

	var format bedrocktypes.ImageFormat
	switch mimeType {
	case "image/jpeg", "image/jpg":
		format = bedrocktypes.ImageFormatJpeg
	case "image/png":
		format = bedrocktypes.ImageFormatPng
	case "image/gif":
		format = bedrocktypes.ImageFormatGif
	case "image/webp":
		format = bedrocktypes.ImageFormatWebp
	default:
		return bedrocktypes.ImageBlock{}, fmt.Errorf("bedrock: unsupported image type %q", mimeType)
	}
	return bedrocktypes.ImageBlock{
		Format: format,
		Source: &bedrocktypes.ImageSourceMemberBytes{Value: raw},
	}, nil
}

func normalizeToolCallIDForBedrock(id string, _ *Model, _ *AssistantMessage) string {
	return sanitizeToolCallID(id, '_')
}

func bedrockCacheTTL(retention CacheRetention) bedrocktypes.CacheTTL {
	if retention == CacheRetentionLong {
		return bedrocktypes.CacheTTLOneHour
	}
	return ""
}

func handleBedrockContentBlockStart(ctx context.Context, event *bedrocktypes.ContentBlockStartEvent, blocks *[]*bedrockStreamBlock, output *AssistantMessage, handler EventHandler) {
	switch start := event.Start.(type) {
	case *bedrocktypes.ContentBlockStartMemberToolUse:
		block := &bedrockStreamBlock{
			ContentBlock: ContentBlock{
				Type:      ContentBlockToolCall,
				ID:        aws.ToString(start.Value.ToolUseId),
				Name:      aws.ToString(start.Value.Name),
				Arguments: json.RawMessage("{}"),
			},
			Index: aws.ToInt32(event.ContentBlockIndex),
		}
		*blocks = append(*blocks, block)
		output.Content = append(output.Content, block.ContentBlock)
		handler(ctx, ToolCallStart{ContentIndex: len(output.Content) - 1, Partial: *output})
	}
}

func handleBedrockContentBlockDelta(ctx context.Context, event *bedrocktypes.ContentBlockDeltaEvent, blocks *[]*bedrockStreamBlock, output *AssistantMessage, handler EventHandler) {
	index := findBedrockStreamBlock(*blocks, aws.ToInt32(event.ContentBlockIndex))
	switch delta := event.Delta.(type) {
	case *bedrocktypes.ContentBlockDeltaMemberText:
		if index == -1 {
			block := &bedrockStreamBlock{
				ContentBlock: ContentBlock{Type: ContentBlockText},
				Index:        aws.ToInt32(event.ContentBlockIndex),
			}
			*blocks = append(*blocks, block)
			output.Content = append(output.Content, block.ContentBlock)
			index = len(output.Content) - 1
			handler(ctx, TextStart{ContentIndex: index, Partial: *output})
		}
		(*blocks)[index].Text += delta.Value
		output.Content[index] = (*blocks)[index].ContentBlock
		handler(ctx, TextDelta{ContentIndex: index, Delta: delta.Value, Partial: *output})

	case *bedrocktypes.ContentBlockDeltaMemberToolUse:
		if index == -1 {
			return
		}
		(*blocks)[index].PartialJSON += aws.ToString(delta.Value.Input)
		args := ParseStreamingJSON((*blocks)[index].PartialJSON)
		(*blocks)[index].Arguments = mustMarshalStreamingJSON(args)
		output.Content[index] = (*blocks)[index].ContentBlock
		handler(ctx, ToolCallDelta{ContentIndex: index, Delta: aws.ToString(delta.Value.Input), Partial: *output})

	case *bedrocktypes.ContentBlockDeltaMemberReasoningContent:
		if index == -1 {
			block := &bedrockStreamBlock{
				ContentBlock: ContentBlock{Type: ContentBlockThinking},
				Index:        aws.ToInt32(event.ContentBlockIndex),
			}
			*blocks = append(*blocks, block)
			output.Content = append(output.Content, block.ContentBlock)
			index = len(output.Content) - 1
			handler(ctx, ThinkingStart{ContentIndex: index, Partial: *output})
		}
		switch reasoning := delta.Value.(type) {
		case *bedrocktypes.ReasoningContentBlockDeltaMemberText:
			(*blocks)[index].Thinking += reasoning.Value
			output.Content[index] = (*blocks)[index].ContentBlock
			handler(ctx, ThinkingDelta{ContentIndex: index, Delta: reasoning.Value, Partial: *output})
		case *bedrocktypes.ReasoningContentBlockDeltaMemberSignature:
			(*blocks)[index].ThoughtSignature += reasoning.Value
			output.Content[index] = (*blocks)[index].ContentBlock
		case *bedrocktypes.ReasoningContentBlockDeltaMemberRedactedContent:
			(*blocks)[index].Redacted = true
			(*blocks)[index].ThoughtSignature += string(reasoning.Value)
			output.Content[index] = (*blocks)[index].ContentBlock
		}
	}
}

func handleBedrockContentBlockStop(ctx context.Context, event *bedrocktypes.ContentBlockStopEvent, blocks []*bedrockStreamBlock, output *AssistantMessage, handler EventHandler) {
	index := findBedrockStreamBlock(blocks, aws.ToInt32(event.ContentBlockIndex))
	if index == -1 {
		return
	}
	block := blocks[index]
	output.Content[index] = block.ContentBlock
	switch block.Type {
	case ContentBlockText:
		handler(ctx, TextEnd{ContentIndex: index, Content: block.Text, Partial: *output})
	case ContentBlockThinking:
		handler(ctx, ThinkingEnd{ContentIndex: index, Content: block.Thinking, ContentSignature: block.ThoughtSignature, Partial: *output})
	case ContentBlockToolCall:
		handler(ctx, ToolCallEnd{ContentIndex: index, ToolCall: ToolCall{
			ID:        block.ID,
			Name:      block.Name,
			Arguments: block.Arguments,
		}, Partial: *output})
	}
}

func handleBedrockMetadata(event *bedrocktypes.ConverseStreamMetadataEvent, model *Model, output *AssistantMessage) {
	if event.Usage == nil {
		return
	}
	output.Usage.Input = int(aws.ToInt32(event.Usage.InputTokens))
	output.Usage.Output = int(aws.ToInt32(event.Usage.OutputTokens))
	output.Usage.CacheRead = int(aws.ToInt32(event.Usage.CacheReadInputTokens))
	output.Usage.CacheWrite = int(aws.ToInt32(event.Usage.CacheWriteInputTokens))
	output.Usage.TotalTokens = int(aws.ToInt32(event.Usage.TotalTokens))
	output.Usage.CalculateCost(model.Cost)
}

func mapBedrockStopReason(reason bedrocktypes.StopReason) StopReason {
	switch reason {
	case bedrocktypes.StopReasonEndTurn, bedrocktypes.StopReasonStopSequence:
		return StopReasonStop
	case bedrocktypes.StopReasonMaxTokens, bedrocktypes.StopReasonModelContextWindowExceeded:
		return StopReasonLength
	case bedrocktypes.StopReasonToolUse:
		return StopReasonToolUse
	default:
		return StopReasonError
	}
}

func findBedrockStreamBlock(blocks []*bedrockStreamBlock, streamIndex int32) int {
	for i, block := range blocks {
		if block.Index == streamIndex {
			return i
		}
	}
	return -1
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	return maps.Clone(in)
}

func bedrockDocumentOrNil(v any) bedrockdocument.Interface {
	if v == nil {
		return nil
	}
	return bedrockdocument.NewLazyDocument(v)
}

type bedrockHeaderRoundTripper struct {
	base        http.RoundTripper
	headers     map[string]string
	bearerToken string
}

func (rt *bedrockHeaderRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range rt.headers {
		clone.Header.Set(k, v)
	}
	if rt.bearerToken != "" {
		// Bearer-token auth bypasses SigV4. Strip the SigV4 headers that
		// aws-sdk-go-v2 added so the upstream only sees bearer auth.
		// Mirrors pi-mono's bearer-token middleware in providers/amazon-bedrock.ts.
		clone.Header.Del("Authorization")
		clone.Header.Del("X-Amz-Date")
		clone.Header.Del("X-Amz-Security-Token")
		clone.Header.Del("X-Amz-Content-Sha256")
		clone.Header.Set("Authorization", "Bearer "+rt.bearerToken)
	}
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}
