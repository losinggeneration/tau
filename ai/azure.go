package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
	var o options
	o.apply(opts)

	apiKey := o.apiKey
	if apiKey == "" {
		apiKey = os.Getenv("AZURE_OPENAI_API_KEY")
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

	body := buildAzureRequestBody(model, prompt, &o)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("azure openai: create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("api-key", apiKey)
	// pi-mono does not send session-affinity HTTP headers to Azure OpenAI
	// (it puts the session id in the body as prompt_cache_key instead).
	if model.GetCompat().SendSessionAffinityHeaders {
		for k, v := range sessionHeaders(&o) {
			req.Header.Set(k, v)
		}
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("azure openai: request: %w", err)
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
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("azure openai: status %d", resp.StatusCode)
		}

		return fmt.Errorf("azure openai: status %d: %s", resp.StatusCode, string(body))
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
	streamOpts := &ResponsesStreamOptions{
		ServiceTier:             o.serviceTier,
		ApplyServiceTierPricing: ServiceTierPricing,
	}

	reader := newResponsesStreamReader(resp.Body)
	return ProcessResponsesStream(ctx, reader, &output, handler, model, streamOpts)
}

func resolveAzureConfig(model *Model, o *options) (baseURL, apiVersion, deploymentName string, err error) {
	if o != nil && strings.TrimSpace(o.azureAPIVersion) != "" {
		apiVersion = strings.TrimSpace(o.azureAPIVersion)
	} else {
		apiVersion = strings.TrimSpace(os.Getenv("AZURE_OPENAI_API_VERSION"))
	}
	if apiVersion == "" {
		apiVersion = azureAPIVersion
	}

	if o != nil && strings.TrimSpace(o.azureBaseURL) != "" {
		baseURL = strings.TrimSpace(o.azureBaseURL)
	} else {
		baseURL = strings.TrimSpace(os.Getenv("AZURE_OPENAI_BASE_URL"))
	}

	resourceName := strings.TrimSpace(os.Getenv("AZURE_OPENAI_RESOURCE_NAME"))
	if o != nil && strings.TrimSpace(o.azureResourceName) != "" {
		resourceName = strings.TrimSpace(o.azureResourceName)
	}
	if baseURL == "" && resourceName != "" {
		baseURL = fmt.Sprintf("https://%s.openai.azure.com/openai/v1", resourceName)
	}
	if baseURL == "" {
		baseURL = model.BaseURL
	}
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "", "", "", errors.New("azure openai: missing endpoint (set AZURE_OPENAI_BASE_URL, AZURE_OPENAI_RESOURCE_NAME, or model baseURL)")
	}

	if o != nil && strings.TrimSpace(o.azureDeploymentName) != "" {
		deploymentName = strings.TrimSpace(o.azureDeploymentName)
	}
	if deploymentName == "" {
		deploymentName = parseAzureDeploymentNameMap(os.Getenv("AZURE_OPENAI_DEPLOYMENT_NAME_MAP"))[model.ID]
	}
	if deploymentName == "" {
		deploymentName = model.ID
	}

	return baseURL, apiVersion, deploymentName, nil
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
func buildAzureRequestBody(model *Model, aiCtx *Context, o *options) map[string]any {
	transformedCtx := aiCtx
	input, err := ConvertResponsesMessages(model, aiCtx, azureResponsesToolCallProviders, &ConvertResponsesMessagesOptions{
		IncludeSystemPrompt: true,
	})
	if err != nil {
		input = nil
	} else {
		transformedCtx = &Context{
			SystemPrompt: aiCtx.SystemPrompt,
			Messages:     nil,
			Tools:        aiCtx.Tools,
		}
		if transformed, transformErr := TransformContext(aiCtx, model, openAIResponsesToolCallIDNormalizerWithProviders(model, azureResponsesToolCallProviders)); transformErr == nil {
			transformedCtx = transformed
		}
	}

	body := map[string]any{
		"model":  model.ID,
		"input":  input,
		"stream": true,
	}

	if o.temperature != nil {
		body["temperature"] = *o.temperature
	}

	if o.maxTokens > 0 {
		body["max_output_tokens"] = o.maxTokens
	}

	if o.topP != nil {
		body["top_p"] = *o.topP
	}

	if sessionID := cacheSessionID(o); sessionID != "" {
		body["prompt_cache_key"] = sessionID
	}

	if len(o.metadata) > 0 {
		body["metadata"] = o.metadata
	}

	if len(transformedCtx.Tools) > 0 {
		body["tools"] = ConvertResponsesTools(transformedCtx.Tools)
	}

	if model.Reasoning {
		reasoning := map[string]any{"effort": "none"}
		if o.reasoning != "" && o.reasoning != ReasoningOff {
			reasoning["effort"] = string(o.reasoning)
			body["include"] = []string{"reasoning.encrypted_content"}
		}
		body["reasoning"] = reasoning
	}

	return body
}

var azureResponsesToolCallProviders = map[string]struct{}{
	ProviderOpenAI:      {},
	ProviderOpenAICodex: {},
	ProviderOpenCode:    {},
	ProviderAzureOpenAI: {},
}

func convertAzureResponsesMessage(model *Model, msg Message) []map[string]any {
	return convertMessageForOpenAIResponses(model, msg, 0)
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
