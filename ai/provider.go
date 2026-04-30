package ai // import "github.com/losinggeneration/tau/ai"

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// Provider creates clients for specific API types.
type Provider interface {
	// API returns the API type this provider implements.
	API() string

	// IsAlias returns true if this provider is an alias to another provider
	// implementation (e.g. "openai-responses" resolved to the OpenAI completions
	// client path). Dedicated implementations return false. Aliases should be
	// replaced by real implementations to ensure API-level routing is precise.
	IsAlias() bool

	// NewClient creates a new client for the given model.
	NewClient(model *Model) (Client, error)
}

// Registry manages providers.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	sources   map[string]string
	order     []string
}

// Global registry of providers.
var globalRegistry = &Registry{
	providers: make(map[string]Provider),
	sources:   make(map[string]string),
	order:     make([]string, 0),
}

// Register registers a provider for an API type.
func (r *Registry) Register(api string, p Provider) {
	r.RegisterWithSource(api, p, "")
}

// RegisterWithSource registers a provider for an API type and associates it
// with an optional source identifier for grouped unregistration.
func (r *Registry) RegisterWithSource(api string, p Provider, sourceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.providers == nil {
		r.providers = make(map[string]Provider)
	}
	if r.sources == nil {
		r.sources = make(map[string]string)
	}
	if _, exists := r.providers[api]; !exists {
		r.order = append(r.order, api)
	}

	r.providers[api] = p
	if sourceID != "" {
		r.sources[api] = sourceID
	} else {
		delete(r.sources, api)
	}
}

// Get retrieves a provider for an API type.
func (r *Registry) Get(api string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.providers[api]

	return p, ok
}

// Unregister removes a provider for an API type.
func (r *Registry) Unregister(api string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.providers, api)
	delete(r.sources, api)
	r.removeOrderLocked(api)
}

// UnregisterSource removes all providers registered under the given source ID.
func (r *Registry) UnregisterSource(sourceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for api, source := range r.sources {
		if source == sourceID {
			delete(r.providers, api)
			delete(r.sources, api)
			r.removeOrderLocked(api)
		}
	}
}

// Clear removes all registered providers from the registry.
func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()

	clear(r.providers)
	clear(r.sources)
	r.order = nil
}

func (r *Registry) removeOrderLocked(api string) {
	for i, orderedAPI := range r.order {
		if orderedAPI == api {
			r.order = append(r.order[:i], r.order[i+1:]...)
			return
		}
	}
}

func (r *Registry) orderedAPIsLocked() []string {
	apis := make([]string, 0, len(r.providers))
	seen := make(map[string]struct{}, len(r.providers))
	for _, api := range r.order {
		if _, ok := r.providers[api]; ok {
			apis = append(apis, api)
			seen[api] = struct{}{}
		}
	}
	if len(apis) != len(r.providers) {
		missing := make([]string, 0, len(r.providers)-len(apis))
		for api := range r.providers {
			if _, ok := seen[api]; !ok {
				missing = append(missing, api)
			}
		}
		slices.Sort(missing)
		apis = append(apis, missing...)
	}
	return apis
}

// APIs returns the currently registered API identifiers in insertion order.
func (r *Registry) APIs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.orderedAPIsLocked()
}

// Providers returns the currently registered providers in API insertion order.
func (r *Registry) Providers() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	apis := r.orderedAPIsLocked()
	providers := make([]Provider, 0, len(apis))
	for _, api := range apis {
		providers = append(providers, r.providers[api])
	}

	return providers
}

// Register registers a provider globally.
func Register(api string, p Provider) {
	globalRegistry.Register(api, p)
}

// RegisterWithSource registers a provider globally with an optional source ID.
func RegisterWithSource(api string, p Provider, sourceID string) {
	globalRegistry.RegisterWithSource(api, p, sourceID)
}

// Get retrieves a provider from the global registry.
func Get(api string) (Provider, bool) {
	return globalRegistry.Get(api)
}

// GetProvider retrieves a provider from the global registry.
func GetProvider(api string) (Provider, bool) {
	return Get(api)
}

// Unregister removes a provider from the global registry.
func Unregister(api string) {
	globalRegistry.Unregister(api)
}

// UnregisterProviders removes all globally registered providers associated with
// the given source ID.
func UnregisterProviders(sourceID string) {
	globalRegistry.UnregisterSource(sourceID)
}

// Clear removes all providers from the global registry.
func Clear() {
	globalRegistry.Clear()
}

// ResetProviders clears the global registry and re-registers all default
// built-in providers. This is the inverse of Register: any custom providers
// registered via Register or RegisterWithSource are dropped and must be
// re-registered by the caller if still needed.
//
// ResetProviders is intended for test isolation and process reconfiguration.
// It does not provide atomic swap: a concurrent Get may observe an empty or
// partially populated registry between Clear and Init. Callers must not invoke
// ResetProviders while other goroutines are resolving providers.
func ResetProviders() {
	Clear()
	Init()
}

// ResetAPIProviders is a TypeScript-parity alias for ResetProviders.
func ResetAPIProviders() {
	ResetProviders()
}

// RegisteredAPIs returns the registered global API identifiers in insertion order.
func RegisteredAPIs() []string {
	return globalRegistry.APIs()
}

// GetProviders returns the registered global providers in API insertion order.
func GetProviders() []Provider {
	return globalRegistry.Providers()
}

// DefaultProviders returns the default set of providers.
func DefaultProviders() []Provider {
	return []Provider{
		&OpenAICompletionsProvider{},
		&OpenAIResponsesProvider{},
		&OpenAICompatProvider{},
		&AnthropicProvider{},
		&ClaudeAgentProvider{},
		&GoogleProvider{},
		&GoogleGeminiCLIProvider{},
		&MistralProvider{},
		&BedrockProvider{},
		&AzureProvider{},
		&VertexProvider{},
		&CodexProvider{},
	}
}

var bedrockClientFactory = func(model *Model) (Client, error) {
	return newBedrockClientForModel(model)
}

// SetBedrockClientFactory overrides the Bedrock client constructor used by the
// built-in Bedrock provider. Passing nil restores the default implementation.
func SetBedrockClientFactory(factory func(model *Model) (Client, error)) {
	if factory == nil {
		bedrockClientFactory = func(model *Model) (Client, error) {
			return newBedrockClientForModel(model)
		}
		return
	}

	bedrockClientFactory = factory
}

// APIForProvider returns the default API type for a provider identifier.
func APIForProvider(provider string) string {
	switch provider {
	case ProviderAnthropic:
		return APIAnthropicMessages
	case ProviderClaudeAgent:
		return APIClaudeAgent
	case ProviderOpenAI:
		return APIOpenAIResponses
	case ProviderGoogle:
		return APIGoogleGenerativeAI
	case ProviderGoogleGeminiCLI:
		return APIGoogleGeminiCLI
	case ProviderMistral:
		return APIMistralConversations
	case ProviderAmazonBedrock:
		return APIBedrockConverseStream
	case ProviderAzureOpenAI:
		return APIAzureOpenAIResponses
	case ProviderOpenAICodex:
		return APIOpenAICodexResponses
	case ProviderGitHubCopilot:
		return APIOpenAIResponses
	default:
		// For compat providers and unknown providers, use OpenAI-compatible
		return APIOpenAICompatible
	}
}

// OpenAICompatibleProviders returns the provider identifiers that reuse the
// shared OpenAI-compatible transport. The set is declarative: inclusion
// guarantees only that APIForProvider routes the identifier through
// APIOpenAICompatible and that the openAI-compat client can resolve a base
// URL and API key for it. It does not imply the identifier has any entries
// in the generated model registry, nor that the identifier appears in
// ListSupportedProviders.
func OpenAICompatibleProviders() []string {
	return []string{
		ProviderGroq,
		ProviderCerebras,
		ProviderxAI,
		ProviderOpenRouter,
		ProviderOllama,
		ProviderLMStudio,
		ProviderVLLM,
		ProviderDeepSeek,
		ProviderFireworks,
		ProviderMoonshot,
		ProviderCloudflare,
		ProviderXiaomi,
	}
}

// Init registers all default providers.
func Init() {
	for _, p := range DefaultProviders() {
		Register(p.API(), p)
	}
}

func init() {
	Init()
}

// OpenAICompletionsProvider handles OpenAI-compatible API endpoints.
type OpenAICompletionsProvider struct{}

// API implements Provider.
func (p *OpenAICompletionsProvider) API() string {
	return APIOpenAICompletions
}

// IsAlias implements Provider.
func (p *OpenAICompletionsProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *OpenAICompletionsProvider) NewClient(model *Model) (Client, error) {
	return newOpenAIClient(model)
}

// Capabilities implements CapableProvider.
func (p *OpenAICompletionsProvider) Capabilities() Capabilities {
	return OpenAICompletionsCapabilities
}

// AnthropicProvider handles Anthropic Messages API.
type AnthropicProvider struct{}

// API implements Provider.
func (p *AnthropicProvider) API() string {
	return APIAnthropicMessages
}

// IsAlias implements Provider.
func (p *AnthropicProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *AnthropicProvider) NewClient(model *Model) (Client, error) {
	return newAnthropicClient(model)
}

// Capabilities implements CapableProvider.
func (p *AnthropicProvider) Capabilities() Capabilities {
	return AnthropicCapabilities
}

// ClaudeAgentProvider handles Claude through the local Claude Code Agent SDK.
type ClaudeAgentProvider struct{}

// API implements Provider.
func (p *ClaudeAgentProvider) API() string {
	return APIClaudeAgent
}

// IsAlias implements Provider.
func (p *ClaudeAgentProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *ClaudeAgentProvider) NewClient(model *Model) (Client, error) {
	return newClaudeAgentClient(model)
}

// Capabilities implements CapableProvider.
func (p *ClaudeAgentProvider) Capabilities() Capabilities {
	return ClaudeAgentCapabilities
}

// GoogleProvider handles Google Generative AI API.
type GoogleProvider struct{}

// API implements Provider.
func (p *GoogleProvider) API() string {
	return APIGoogleGenerativeAI
}

// IsAlias implements Provider.
func (p *GoogleProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *GoogleProvider) NewClient(model *Model) (Client, error) {
	return newGoogleClient(model)
}

// Capabilities implements CapableProvider.
func (p *GoogleProvider) Capabilities() Capabilities {
	return GoogleGenerativeAICapabilities
}

// GoogleGeminiCLIProvider handles Google Gemini CLI / Antigravity API.
type GoogleGeminiCLIProvider struct{}

// API implements Provider.
func (p *GoogleGeminiCLIProvider) API() string {
	return APIGoogleGeminiCLI
}

// IsAlias implements Provider.
func (p *GoogleGeminiCLIProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *GoogleGeminiCLIProvider) NewClient(model *Model) (Client, error) {
	return newGoogleGeminiCLIClient(model)
}

// Capabilities implements CapableProvider.
func (p *GoogleGeminiCLIProvider) Capabilities() Capabilities {
	return GoogleGeminiCLICapabilities
}

// MistralProvider handles Mistral API.
type MistralProvider struct{}

// API implements Provider.
func (p *MistralProvider) API() string {
	return APIMistralConversations
}

// IsAlias implements Provider.
func (p *MistralProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *MistralProvider) NewClient(model *Model) (Client, error) {
	return newMistralClient(model)
}

// Capabilities implements CapableProvider.
func (p *MistralProvider) Capabilities() Capabilities {
	return MistralCapabilities
}

// BedrockProvider handles AWS Bedrock.
type BedrockProvider struct{}

// API implements Provider.
func (p *BedrockProvider) API() string {
	return APIBedrockConverseStream
}

// IsAlias implements Provider.
func (p *BedrockProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *BedrockProvider) NewClient(model *Model) (Client, error) {
	return bedrockClientFactory(model)
}

// Capabilities implements CapableProvider.
func (p *BedrockProvider) Capabilities() Capabilities {
	return BedrockCapabilities
}

// GetClient retrieves a client for a model.
func GetClient(model *Model) (Client, error) {
	p, ok := Get(model.API)
	if !ok {
		return nil, fmt.Errorf("no provider for API type: %s", model.API)
	}

	// API mismatch guard: the resolved provider must declare the same API it was
	// registered under. If the provider's API differs, this is a routing bug — the
	// caller passed a model with an API the registry is proxying through a different
	// provider (e.g. a model claiming "openai-responses" resolving to the OpenAI
	// completions provider). Dedicated providers should be registered for each API
	// to avoid silent mis-routing.
	if p.API() != model.API {
		return nil, fmt.Errorf("api mismatch for %q: model API %q resolved to provider %T which declares API %q; "+
			"expected a dedicated implementation for %q, not an alias",
			model.ID, model.API, p, p.API(), model.API)
	}

	return p.NewClient(model)
}

// GetClient retrieves a client for a model.

// HTTPTransport is an HTTP transport for making API requests.
type HTTPTransport struct {
	httpClient   *http.Client
	baseURL      string
	headers      http.Header
	apiKey       string
	refreshCreds func(ctx context.Context, provider string) (string, error)
	provider     string
}

// TransportOptions configures an HTTPTransport.
type TransportOptions struct {
	// RefreshCreds is an optional callback that fetches a fresh API key
	// for the given provider. It is called when a 401 response is
	// received, before retrying the request.
	RefreshCreds func(ctx context.Context, provider string) (string, error)
	// Provider is the provider name used for refresh callbacks.
	Provider string
}

// NewTransport creates a new transport.
func NewTransport(baseURL string, apiKey string, headers http.Header) *HTTPTransport {
	return &HTTPTransport{
		httpClient: http.DefaultClient,
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		headers:    headers.Clone(),
		apiKey:     apiKey,
	}
}

// NewTransportWithOptions creates a transport with additional options including
// credential refresh support for 401 retry.
func NewTransportWithOptions(baseURL string, apiKey string, headers http.Header, opts TransportOptions) *HTTPTransport {
	return &HTTPTransport{
		httpClient:   http.DefaultClient,
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		headers:      headers.Clone(),
		apiKey:       apiKey,
		refreshCreds: opts.RefreshCreds,
		provider:     opts.Provider,
	}
}

func (t *HTTPTransport) withHeaders(extra map[string]string) *HTTPTransport {
	if len(extra) == 0 {
		return t
	}

	clone := &HTTPTransport{
		httpClient:   t.httpClient,
		baseURL:      t.baseURL,
		headers:      t.headers.Clone(),
		apiKey:       t.apiKey,
		refreshCreds: t.refreshCreds,
		provider:     t.provider,
	}

	for k, v := range extra {
		clone.headers.Set(k, v)
	}

	return clone
}

func (t *HTTPTransport) withAPIKey(apiKey string) *HTTPTransport {
	if apiKey == "" || apiKey == t.apiKey {
		return t
	}

	return &HTTPTransport{
		httpClient:   t.httpClient,
		baseURL:      t.baseURL,
		headers:      t.headers.Clone(),
		apiKey:       apiKey,
		refreshCreds: t.refreshCreds,
		provider:     t.provider,
	}
}

// Do makes an HTTP request with proper headers and authentication.
// If a 401 response is received and a RefreshCreds callback is configured,
// Do will refresh the credentials, update the transport, and retry once.
func (t *HTTPTransport) Do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var bodyReader *strings.Reader

	if body != nil {
		// Body should already be JSON encoded by caller
		if s, ok := body.(string); ok {
			bodyReader = strings.NewReader(s)
		}
	}

	url := t.baseURL + path

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Set headers
	for k, v := range t.headers {
		req.Header.Set(k, v[0])
	}

	// Set auth
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}

	// Set content type if body present
	if body != nil && bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	// If we got a 401 and have a refresh callback, try refreshing credentials once
	if resp.StatusCode == http.StatusUnauthorized && t.refreshCreds != nil {
		if err := resp.Body.Close(); err != nil {
			return nil, fmt.Errorf("close unauthorized response body: %w", err)
		}

		newKey, refreshErr := t.refreshCreds(ctx, t.provider)
		if refreshErr != nil {
			return nil, fmt.Errorf("401 received, credential refresh failed: %w", refreshErr)
		}

		// Update transport with new credentials
		t.apiKey = newKey
		req.Header.Set("Authorization", "Bearer "+newKey)

		// Re-create body reader for retry
		var retryBody *strings.Reader
		if body != nil {
			if s, ok := body.(string); ok {
				retryBody = strings.NewReader(s)
			}
		}
		req.Body = nil
		if retryBody != nil {
			req.Body = io.NopCloser(retryBody)
		}

		return t.httpClient.Do(req)
	}

	return resp, nil
}

func responseMetadata(resp *http.Response) ProviderResponse {
	headers := make(map[string]string, len(resp.Header))
	for k, values := range resp.Header {
		if len(values) == 0 {
			continue
		}
		headers[k] = values[0]
	}

	return ProviderResponse{
		Status:  resp.StatusCode,
		Headers: headers,
	}
}
