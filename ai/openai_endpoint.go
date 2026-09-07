package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"
)

// Endpoint resolution and transport shared by the two clients that speak the
// OpenAI /chat/completions protocol: openAIClient (APIOpenAICompletions) and
// openAICompatClient (APIOpenAICompatible). Both previously resolved base
// URLs, API keys and headers independently, which let the two paths drift.

// chatCompletionsPath is the endpoint both clients POST to.
const chatCompletionsPath = "/chat/completions"

var (
	// chatCompletionsMaxRetries bounds 5xx retries. With the doubling base
	// delay below the full schedule runs ~128s, so a hard-down provider
	// still fails in about two minutes.
	chatCompletionsMaxRetries = 10
	// chatCompletionsRetryBase is the first backoff delay; each retry doubles
	// it (125ms, 250ms, ... 64s).
	chatCompletionsRetryBase = 125 * time.Millisecond
)

// resolveOpenAIBaseURL returns the base URL for a model's chat/completions
// endpoint, preferring an explicit Model.BaseURL over the provider default.
func resolveOpenAIBaseURL(model *Model) (string, error) {
	baseURL := model.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURLs[model.Provider]
	}

	if baseURL == "" {
		return "", fmt.Errorf("%s: no base URL for provider", model.Provider)
	}

	return strings.TrimSuffix(baseURL, "/"), nil
}

// resolveOpenAIAPIKey returns the API key for a request, falling back to the
// provider's environment variable when the caller supplied none.
func resolveOpenAIAPIKey(model *Model, o *options) string {
	if o.apiKey != "" {
		return o.apiKey
	}

	return GetEnvAPIKey(model.Provider)
}

// providerRequestHeaders returns headers a specific provider requires on every
// chat/completions request.
func providerRequestHeaders(provider string) map[string]string {
	if provider == ProviderOpenRouter {
		// OpenRouter uses these to attribute and rank requests.
		return map[string]string{
			"Http-Referer": "https://github.com/losinggeneration/tau",
			"X-Title":      "Tau AI",
		}
	}

	return nil
}

// openAIRequestHeaders collects the per-request headers for a chat/completions
// call. Session-affinity headers are only forwarded when the model's compat
// opts in: pi-mono defaults this to false for openai-completions because
// sending session_id to opencode.ai/zen/go returns HTTP 500 on models like
// kimi-k2.6, glm-5.1 and glm-5.2. Caller-supplied headers win over both.
func openAIRequestHeaders(model *Model, o *options) map[string]string {
	headers := make(map[string]string, len(o.headers)+2)

	if model.GetCompat().SendSessionAffinityHeaders {
		maps.Copy(headers, sessionHeaders(o))
	}

	maps.Copy(headers, providerRequestHeaders(model.Provider))
	maps.Copy(headers, o.headers)

	return headers
}

// newOpenAIChatCompletionsTransport creates the base transport for a model's
// chat/completions endpoint. The API key is applied per-request by
// withRequestOptions rather than baked in here, because it can arrive from an
// Option or the credential store at call time.
func newOpenAIChatCompletionsTransport(model *Model) (*HTTPTransport, error) {
	baseURL, err := resolveOpenAIBaseURL(model)
	if err != nil {
		return nil, err
	}

	return NewTransport(baseURL, "", http.Header{
		"Content-Type": {"application/json"},
		"User-Agent":   {"tau-ai/0.1.0"},
	}), nil
}

// withRequestOptions layers a single request's API key, credential-refresh
// options and headers onto a base transport.
func (t *HTTPTransport) withRequestOptions(model *Model, o *options) *HTTPTransport {
	apiKey := resolveOpenAIAPIKey(model, o)
	headers := openAIRequestHeaders(model, o)

	if o.transportOpts != nil {
		return NewTransportWithOptions(t.baseURL, apiKey, t.headers, TransportOptions{
			RefreshCreds: o.transportOpts.RefreshCreds,
			Provider:     o.transportOpts.Provider,
		}).withHeaders(headers)
	}

	return t.withAPIKey(apiKey).withHeaders(headers)
}

// chatCompletionsRetryDelay returns the backoff before retry attempt+1.
func chatCompletionsRetryDelay(attempt int) time.Duration {
	return chatCompletionsRetryBase * time.Duration(1<<attempt)
}

// doOpenAIChatCompletions posts a chat/completions request and returns the
// response for streaming. Transient server failures (HTTP 5xx) are retried
// with exponential backoff for roughly two minutes before giving up; anything
// else fails immediately. On success the caller owns closing the body; on any
// error the body is already closed.
func doOpenAIChatCompletions(ctx context.Context, model *Model, o *options, transport *HTTPTransport, body string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := transport.Do(ctx, http.MethodPost, chatCompletionsPath, body)
		if err != nil {
			return nil, fmt.Errorf("%s: request: %w", model.Provider, err)
		}

		if hasResponseCallback(o) {
			invokeResponseCallback(o, model, responseMetadata(resp))
		}

		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		respBody, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			err := fmt.Errorf("read error response body: %w", readErr)
			if closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close error response body: %w", closeErr))
			}
			return nil, fmt.Errorf("%s: status %d: %w", model.Provider, resp.StatusCode, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("%s: close error response body: %w", model.Provider, closeErr)
		}

		// z.ai reports an exhausted usage window as a 429 whose message
		// carries the reset time in Beijing time; restate it in local time.
		if usageErr := parseZAIUsageLimitError(model.Provider, resp.StatusCode, respBody); usageErr != nil {
			return nil, usageErr
		}

		if attempt < chatCompletionsMaxRetries && resp.StatusCode >= http.StatusInternalServerError {
			if sleepErr := sleepWithContext(ctx, chatCompletionsRetryDelay(attempt)); sleepErr == nil {
				continue
			} else {
				return nil, fmt.Errorf("%s: request: %w", model.Provider, sleepErr)
			}
		}

		return nil, fmt.Errorf("%s: status %d: %s", model.Provider, resp.StatusCode, string(respBody))
	}
}
