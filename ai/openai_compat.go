package ai

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// mapReasoningEffort applies the optional effort mapping.
func mapReasoningEffort(effort ReasoningLevel, effortMap map[string]string) string {
	if effortMap == nil {
		return string(effort)
	}
	if mapped, ok := effortMap[string(effort)]; ok {
		return mapped
	}
	return string(effort)
}

func hasOpenRouterRouting(routing *OpenRouterRouting) bool {
	if routing == nil {
		return false
	}

	return routing.AllowFallbacks != nil ||
		routing.RequireParameters != nil ||
		routing.DataCollection != "" ||
		routing.ZDR != nil ||
		routing.EnforceDistillableText != nil ||
		len(routing.Order) > 0 ||
		len(routing.Only) > 0 ||
		len(routing.Ignore) > 0 ||
		len(routing.Quantizations) > 0 ||
		routing.Sort != nil ||
		routing.MaxPrice != nil ||
		routing.PreferredMinThroughput != nil ||
		routing.PreferredMaxLatency != nil
}

// OpenAICompatProvider handles the shared OpenAI-compatible transport used by
// several provider families. Support for a provider family still depends on the
// public support taxonomy rather than this shared implementation alone.
type OpenAICompatProvider struct{}

// API implements Provider.
func (p *OpenAICompatProvider) API() string {
	return APIOpenAICompatible
}

// IsAlias implements Provider.
func (p *OpenAICompatProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *OpenAICompatProvider) NewClient(model *Model) (Client, error) {
	return &openAICompatClient{model: model}, nil
}

// openAICompatClient implements Client for OpenAI-compatible providers.
type openAICompatClient struct {
	model         *Model
	transportOpts *TransportOption
}

// SetTransportOption implements the SDK's transport option injection interface.
func (c *openAICompatClient) SetTransportOption(opts TransportOption) {
	c.transportOpts = &opts
}

// Capabilities implements CapableProvider.
// Compat providers default to OpenAI-compatible baseline capabilities;
// per-instance overrides can be declared in settings.json.
func (p *OpenAICompatProvider) Capabilities() Capabilities {
	return OpenAICompletionsCapabilities
}

// Complete implements Client.
func (c *openAICompatClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

// Stream implements Client.Streamer.
func (c *openAICompatClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) (err error) {
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

	if resolveOpenAIAPIKey(c.model, &o) == "" {
		return fmt.Errorf("%s: missing API key (set %s)", c.model.Provider, envVarNameForProvider(c.model.Provider))
	}

	transport, err := newOpenAIChatCompletionsTransport(c.model)
	if err != nil {
		return err
	}

	// Build request body
	body, err := buildOpenAICompatRequestBody(model, prompt, &o)
	if err != nil {
		return fmt.Errorf("%s: build request: %w", c.model.Provider, err)
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
		return fmt.Errorf("%s: marshal request: %w", c.model.Provider, err)
	}

	o.transportOpts = cmp.Or(o.transportOpts, c.transportOpts)
	resp, err := doOpenAIChatCompletions(ctx, model, &o, transport.withRequestOptions(c.model, &o), string(bodyBytes))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	// Stream response
	grammar, err := grammarInputProperties(prompt.Tools, model.GetCompat().SupportsOpenAIGrammarTools)
	if err != nil {
		return err
	}
	return streamOpenAIResponseWithAPI(ctx, model, resp.Body, handler, grammar, APIOpenAICompatible, c.model.Provider)
}

// streamOpenAICompatResponse streams the SSE response.
func streamOpenAICompatResponse(ctx context.Context, model *Model, body io.Reader, handler EventHandler, provider string) error {
	return streamOpenAIResponseWithAPI(ctx, model, body, handler, nil, APIOpenAICompatible, provider)
}

// mapOpenAICompatStopReason maps OpenAI finish_reason to StopReason, matching pi-mono's mapStopReason.
// P7-OC20: Unknown finish_reason values map to StopReasonError.
func mapOpenAICompatStopReason(reason string) (StopReason, string) {
	return mapOpenAIStopReason(reason)
}

// buildOpenAICompatRequestBody builds the request body.
func buildOpenAICompatRequestBody(model *Model, aiCtx *Context, o *options) (map[string]any, error) {
	req, err := buildOpenAIChatCompletionsRequest(model, aiCtx, o)
	if err != nil {
		return nil, err
	}
	return openAIRequestToMap(req)
}

// openAIRequestToMap renders the request as a map for payload callbacks.
func openAIRequestToMap(req *openAIRequest) (map[string]any, error) {
	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	if req.Tools != nil && len(req.Tools) == 0 {
		body["tools"] = []any{}
	}
	if req.ProviderOptions != nil {
		body["providerOptions"] = body["provider_options"]
	}
	return body, nil
}

func convertMessagesForOpenAICompat(aiCtx *Context, model *Model, compat *OpenAICompletionsCompat) []map[string]any {
	client := &openAIClient{model: model}
	messages, err := client.convertMessages(aiCtx, model, compat)
	if err != nil {
		return nil
	}
	body, err := openAIRequestToMap(&openAIRequest{Messages: messages})
	if err != nil {
		return nil
	}
	items, _ := body["messages"].([]any)
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if entry, ok := item.(map[string]any); ok {
			result = append(result, entry)
		}
	}
	return result
}

// convertMessageForOpenAICompat converts a single Message to OpenAI-compatible format.
func convertMessageForOpenAICompat(msg Message) map[string]any {
	ctx := &Context{Messages: []Message{msg}}
	messages := convertMessagesForOpenAICompat(ctx, &Model{ID: "compat-helper"}, &OpenAICompletionsCompat{})
	if len(messages) == 0 {
		return map[string]any{}
	}
	return messages[0]
}

// parseOpenAICompatUsage parses usage with detailed token counting.
// P7-OC04: Handles prompt_tokens_details.{cached_tokens, cache_write_tokens}
// P7-OC04: Handles completion_tokens_details.reasoning_tokens
func parseOpenAICompatUsage(raw *openAICompatUsage, model *Model) Usage {
	promptTokens := raw.PromptTokens
	completionTokens := raw.CompletionTokens

	// P7-OC04: Parse prompt_tokens_details
	cachedTokens := 0
	cacheWriteTokens := 0
	if raw.PromptTokensDetails != nil {
		cachedTokens = raw.PromptTokensDetails.CachedTokens
		cacheWriteTokens = raw.PromptTokensDetails.CacheWriteTokens
	}

	// P7-OC04: Parse completion_tokens_details
	reasoningTokens := 0
	if raw.CompletionTokensDetails != nil {
		reasoningTokens = raw.CompletionTokensDetails.ReasoningTokens
	}

	// Normalize to pi-ai semantics:
	// - cacheRead: hits from cache created by previous requests only
	// - cacheWrite: tokens written to cache in this request
	// Some providers report cached_tokens as (previous hits + current writes).
	// In that case, remove cacheWrite from cacheRead.
	cacheReadTokens := cacheWriteTokens
	if cacheReadTokens > 0 {
		cacheReadTokens = cachedTokens - cacheWriteTokens
		if cacheReadTokens < 0 {
			cacheReadTokens = 0
		}
	} else {
		cacheReadTokens = cachedTokens
	}

	// Compute non-cached input tokens
	input := promptTokens - cacheReadTokens - cacheWriteTokens
	if input < 0 {
		input = 0
	}

	// Add reasoning tokens to output (some providers don't include them in total)
	outputTokens := completionTokens + reasoningTokens

	usage := Usage{
		Input:      input,
		Output:     outputTokens,
		CacheRead:  cacheReadTokens,
		CacheWrite: cacheWriteTokens,
	}
	usage.CalculateCost(model.Cost)

	return usage
}
