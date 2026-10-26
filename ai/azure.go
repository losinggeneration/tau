package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
)

// AzureProvider handles Azure OpenAI Responses API.
type AzureProvider struct{}

// API implements Provider.
func (p *AzureProvider) API() string {
	return APIAzureOpenAIResponses
}

// IsAlias implements Provider.
func (p *AzureProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *AzureProvider) NewClient(model *Model) (Client, error) {
	return &azureClient{model: model}, nil
}

// Capabilities implements CapableProvider.
func (p *AzureProvider) Capabilities() Capabilities {
	return AzureCapabilities
}

type azureClient struct {
	model *Model
}

const azureAPIVersion = "v1"

// Complete implements Client.
func (c *azureClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

// Stream implements Client.Streamer.
func (c *azureClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) (err error) {
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

	apiKey := o.apiKey
	if apiKey == "" {
		apiKey = o.envAPIKey(ProviderAzureOpenAI)
		if apiKey == "" {
			return errors.New("azure openai: missing API key (set AZURE_OPENAI_API_KEY)")
		}
	}

	baseURL, apiVersion, deploymentName, err := resolveAzureConfig(c.model, &o)
	if err != nil {
		return err
	}
	if deploymentName == "" {
		deploymentName = model.ID
	}

	body, err := buildAzureRequestBody(model, prompt, &o)
	if err != nil {
		return fmt.Errorf("azure openai: %w", err)
	}
	body["model"] = deploymentName
	if hasPayloadCallback(&o) {
		if next := invokePayloadCallback(&o, model, body); next != nil {
			if replacement, ok := next.(map[string]any); ok {
				body = replacement
			}
		}
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("azure openai: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/responses?api-version=%s", baseURL, apiVersion)
	resp, err := sendProviderRequest(ctx, &o, providerRequest{label: "azure openai", send: func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("api-key", apiKey)
		// Azure OpenAI gets the session id in the body as prompt_cache_key, so
		// session-affinity headers are opt-in.
		if model.GetCompat().SendSessionAffinityHeaders {
			for k, v := range sessionHeaders(model, &o) {
				req.Header.Set(k, v)
			}
		}
		for k, v := range o.headers {
			req.Header.Set(k, v)
		}
		return doHeaderRequest(req, o.timeout, "azure openai")
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

	output := AssistantMessage{
		Role:       "assistant",
		API:        APIAzureOpenAIResponses,
		Provider:   model.Provider,
		Model:      model.ID,
		Content:    []ContentBlock{},
		StopReason: StopReasonStop,
		Usage:      Usage{},
		Timestamp:  timestamp(),
	}
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

func resolveAzureConfig(model *Model, o *options) (baseURL, apiVersion, deploymentName string, err error) {
	if o != nil && strings.TrimSpace(o.azureAPIVersion) != "" {
		apiVersion = strings.TrimSpace(o.azureAPIVersion)
	} else {
		apiVersion = strings.TrimSpace(o.providerEnvValue("AZURE_OPENAI_API_VERSION"))
	}
	if apiVersion == "" {
		apiVersion = azureAPIVersion
	}

	if o != nil && strings.TrimSpace(o.azureBaseURL) != "" {
		baseURL = strings.TrimSpace(o.azureBaseURL)
	} else {
		baseURL = strings.TrimSpace(o.providerEnvValue("AZURE_OPENAI_BASE_URL"))
	}

	resourceName := strings.TrimSpace(o.providerEnvValue("AZURE_OPENAI_RESOURCE_NAME"))
	if o != nil && strings.TrimSpace(o.azureResourceName) != "" {
		resourceName = strings.TrimSpace(o.azureResourceName)
	}
	if baseURL == "" && resourceName != "" {
		baseURL = fmt.Sprintf("https://%s.openai.azure.com/openai/v1", resourceName)
	}
	if baseURL == "" {
		baseURL = model.BaseURL
	}
	if strings.TrimSpace(baseURL) == "" {
		return "", "", "", errors.New("azure openai: missing endpoint (set AZURE_OPENAI_BASE_URL, AZURE_OPENAI_RESOURCE_NAME, or model baseURL)")
	}
	if baseURL, err = normalizeAzureBaseURL(baseURL); err != nil {
		return "", "", "", err
	}

	if o != nil && strings.TrimSpace(o.azureDeploymentName) != "" {
		deploymentName = strings.TrimSpace(o.azureDeploymentName)
	}
	if deploymentName == "" {
		deploymentName = parseAzureDeploymentNameMap(o.providerEnvValue("AZURE_OPENAI_DEPLOYMENT_NAME_MAP"))[model.ID]
	}
	if deploymentName == "" {
		deploymentName = model.ID
	}

	return baseURL, apiVersion, deploymentName, nil
}

// normalizeAzureBaseURL points bare Azure and Foundry resource URLs at the
// /openai/v1 API root.
func normalizeAzureBaseURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" {
		return "", fmt.Errorf("azure openai: invalid base URL %q", raw)
	}
	host := u.Hostname()
	azureHost := strings.HasSuffix(host, ".openai.azure.com") ||
		strings.HasSuffix(host, ".cognitiveservices.azure.com") ||
		strings.HasSuffix(host, ".ai.azure.com")
	switch strings.TrimRight(u.Path, "/") {
	case "", "/openai", "/openai/v1/responses":
		if azureHost {
			u.Path = "/openai/v1"
			u.RawQuery = ""
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func parseAzureDeploymentNameMap(value string) map[string]string {
	out := map[string]string{}
	if value == "" {
		return out
	}

	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}
		modelID := strings.TrimSpace(parts[0])
		deploymentName := strings.TrimSpace(parts[1])
		if modelID == "" || deploymentName == "" {
			continue
		}
		out[modelID] = deploymentName
	}

	return out
}

// buildAzureRequestBody builds the Azure OpenAI Responses request body.
func buildAzureRequestBody(model *Model, aiCtx *Context, o *options) (map[string]any, error) {
	input, err := ConvertResponsesMessages(model, aiCtx, azureResponsesToolCallProviders, &ConvertResponsesMessagesOptions{
		IncludeSystemPrompt: true,
	})
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"model":  model.ID,
		"input":  input,
		"stream": true,
		"store":  false,
	}

	if o.temperature != nil {
		body["temperature"] = *o.temperature
	}

	if o.maxTokens > 0 {
		body["max_output_tokens"] = max(o.maxTokens, 16)
	}

	if o.topP != nil {
		body["top_p"] = *o.topP
	}

	if sessionID := cacheSessionID(o); sessionID != "" {
		body["prompt_cache_key"] = clampOpenAIPromptCacheKey(sessionID)
	}

	if len(o.metadata) > 0 {
		body["metadata"] = o.metadata
	}

	if len(aiCtx.Tools) > 0 {
		tools, err := responsesTools(model, aiCtx.Tools, supportsStrictMode(model, true))
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
		} else if off, ok := responsesOffEffort(model); ok {
			body["reasoning"] = map[string]any{"effort": off}
		}
	}

	// Last so model and request sampling parameters override named fields.
	maps.Copy(body, resolveSamplingParams(model, o))

	return body, nil
}

var azureResponsesToolCallProviders = map[string]struct{}{
	ProviderOpenAI:      {},
	ProviderOpenAICodex: {},
	ProviderOpenCode:    {},
	ProviderAzureOpenAI: {},
}

// streamAzureResponse streams Azure Responses API SSE events.
func streamAzureResponse(ctx context.Context, model *Model, body io.Reader, handler EventHandler, o *options) error {
	output := AssistantMessage{
		Role:       "assistant",
		API:        APIAzureOpenAIResponses,
		Provider:   model.Provider,
		Model:      model.ID,
		Content:    []ContentBlock{},
		StopReason: StopReasonStop,
		Usage:      Usage{},
		Timestamp:  timestamp(),
	}

	streamOpts := &ResponsesStreamOptions{
		ApplyServiceTierPricing: ServiceTierPricing,
	}
	if o != nil {
		streamOpts.ServiceTier = o.serviceTier
	}

	reader := newResponsesStreamReader(body)
	return ProcessResponsesStream(ctx, reader, &output, handler, model, streamOpts)
}
