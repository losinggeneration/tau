package ai // import "github.com/losinggeneration/tau/ai"

import (
	"context"
	"maps"
	"os"
)

// Option configures streaming or completion options.
type Option func(*options)

type options struct {
	apiKey                     string
	preferredTransport         Transport
	transportOpts              *TransportOption
	reasoning                  ReasoningLevel
	thinkingEnabled            bool
	thinkingBudgetTokens       int
	googleThinkingLevel        GoogleThinkingLevel
	thinkingDisplay            string // "summarized" | "omitted"
	effort                     string // "low" | "medium" | "high" | "xhigh" | "max"
	thinkingType               string // "adaptive" | "enabled" | "disabled"
	maxTokens                  int
	topP                       *float64
	temperature                *float64
	cacheRetention             string
	sessionID                  string
	maxRetryDelayMs            int
	metadata                   map[string]any
	onPayload                  func(any) any
	onPayloadWithModel         func(any, *Model) any
	onResponse                 func(ProviderResponse)
	onResponseWithModel        func(ProviderResponse, *Model)
	headers                    map[string]string
	textVerbosity              string
	serviceTier                string
	reasoningSummary           string
	reasoningEffort            string
	toolChoice                 any
	client                     Client
	azureAPIVersion            string
	azureResourceName          string
	azureBaseURL               string
	azureDeploymentName        string
	vertexProject              string
	vertexLocation             string
	geminiCLIProjectID         string
	bedrockRegion              string
	bedrockProfile             string
	bedrockRequestMetadata     map[string]string
	bedrockBearerToken         string
	bedrockThinkingBudgets     *ThinkingBudgets
	bedrockInterleavedThinking *bool
	interleavedThinking        *bool
}

func (o *options) apply(opts []Option) {
	for _, opt := range opts {
		opt(o)
	}
}

// WithAPIKey sets the API key for authentication.
func WithAPIKey(key string) Option {
	return func(o *options) {
		o.apiKey = key
	}
}

// WithReasoning sets the reasoning effort level.
func WithReasoning(level ReasoningLevel) Option {
	return func(o *options) {
		o.reasoning = level
	}
}

// WithThinkingEnabled enables thinking/reasoning for compatible models.
func WithThinkingEnabled(enabled bool) Option {
	return func(o *options) {
		o.thinkingEnabled = enabled
	}
}

// WithThinkingBudgetTokens sets the token budget for thinking.
func WithThinkingBudgetTokens(tokens int) Option {
	return func(o *options) {
		o.thinkingBudgetTokens = tokens
	}
}

// WithThinkingDisplay sets the thinking display mode ("summarized" | "omitted").
func WithThinkingDisplay(display string) Option {
	return func(o *options) {
		o.thinkingDisplay = display
	}
}

// WithEffort sets the reasoning effort level ("low" | "medium" | "high" | "xhigh" | "max").
func WithEffort(effort string) Option {
	return func(o *options) {
		o.effort = effort
	}
}

// WithMaxTokens sets the maximum number of tokens to generate.
func WithMaxTokens(tokens int) Option {
	return func(o *options) {
		o.maxTokens = tokens
	}
}

// WithTemperature sets the sampling temperature.
func WithTemperature(temp float64) Option {
	return func(o *options) {
		o.temperature = &temp
	}
}

// WithTopP sets the nucleus sampling parameter.
func WithTopP(topP float64) Option {
	return func(o *options) {
		o.topP = &topP
	}
}

// WithTransport sets the preferred transport for providers that support multiple transports.
func WithTransport(transport Transport) Option {
	return func(o *options) {
		o.preferredTransport = transport
	}
}

// TransportOption contains transport-level options like refresh callbacks.
type TransportOption struct {
	// RefreshCreds is called when a 401 is received to get fresh credentials.
	// It receives the provider name and returns the new API key.
	RefreshCreds func(ctx context.Context, provider string) (string, error)
	// Provider is the provider name for refresh callbacks.
	Provider string
}

// WithTransportOption sets transport-level options like credential refresh callbacks.
func WithTransportOption(opts TransportOption) Option {
	return func(o *options) {
		o.transportOpts = &opts
	}
}

// WithCacheRetention sets the prompt cache retention period.
func WithCacheRetention(retention string) Option {
	return func(o *options) {
		o.cacheRetention = retention
	}
}

// WithSessionID sets the session ID for provider caching.
func WithSessionID(id string) Option {
	return func(o *options) {
		o.sessionID = id
	}
}

// WithMaxRetryDelayMs sets the maximum provider-requested retry delay in milliseconds.
func WithMaxRetryDelayMs(delay int) Option {
	return func(o *options) {
		o.maxRetryDelayMs = delay
	}
}

// WithMetadata sets provider-specific request metadata.
func WithMetadata(metadata map[string]any) Option {
	return func(o *options) {
		if len(metadata) == 0 {
			o.metadata = nil
			return
		}

		o.metadata = maps.Clone(metadata)
	}
}

// WithPayloadCallback sets a callback for debugging request/response payloads.
func WithPayloadCallback(fn func(any) any) Option {
	return func(o *options) {
		o.onPayload = fn
	}
}

// WithPayloadCallbackForModel sets a model-aware callback for debugging and
// optionally replacing request/response payloads. It mirrors Pi's
// onPayload(payload, model) callback shape while remaining synchronous.
func WithPayloadCallbackForModel(fn func(any, *Model) any) Option {
	return func(o *options) {
		o.onPayloadWithModel = fn
	}
}

// WithResponseCallback sets a callback invoked after the HTTP response is received.
func WithResponseCallback(fn func(ProviderResponse)) Option {
	return func(o *options) {
		o.onResponse = fn
	}
}

// WithResponseCallbackForModel sets a model-aware callback invoked after the
// HTTP response is received. It mirrors Pi's onResponse(response, model)
// callback shape while remaining synchronous.
func WithResponseCallbackForModel(fn func(ProviderResponse, *Model)) Option {
	return func(o *options) {
		o.onResponseWithModel = fn
	}
}

// WithHeaders adds custom headers to the provider request.
func WithHeaders(headers map[string]string) Option {
	return func(o *options) {
		if len(headers) == 0 {
			o.headers = nil
			return
		}

		o.headers = maps.Clone(headers)
	}
}

// Context key for options.
type contextKey string

const optionsKey contextKey = "options"

// WithOptions adds options to the context.
func WithOptions(ctx context.Context, opts ...Option) context.Context {
	var o options
	o.apply(opts)

	return context.WithValue(ctx, optionsKey, &o)
}

// OptionsFromContext retrieves options from the context.
func OptionsFromContext(ctx context.Context) *options {
	o, ok := ctx.Value(optionsKey).(*options)
	if !ok {
		return &options{}
	}

	return o
}

// applyOptions applies a slice of options and returns the resulting options struct.
func applyOptions(opts []Option) *options {
	var o options
	o.apply(opts)

	return &o
}

func resolveCacheRetention(retention string) CacheRetention {
	switch CacheRetention(retention) {
	case CacheRetentionNone, CacheRetentionShort, CacheRetentionLong:
		return CacheRetention(retention)
	}

	if os.Getenv("PI_CACHE_RETENTION") == string(CacheRetentionLong) {
		return CacheRetentionLong
	}

	return CacheRetentionShort
}

func cacheSessionID(o *options) string {
	if o == nil {
		return ""
	}
	if resolveCacheRetention(o.cacheRetention) == CacheRetentionNone {
		return ""
	}

	return o.sessionID
}

func sessionHeaders(o *options) map[string]string {
	sessionID := cacheSessionID(o)
	if sessionID == "" {
		return nil
	}

	return map[string]string{
		"session_id":          sessionID,
		"x-client-request-id": sessionID,
	}
}

// getMaxTokens returns the max tokens to use based on options and model defaults.
// Default thinking budget tokens per level.
var DefaultThinkingBudgets = ThinkingBudgets{
	Minimal: new(1024),
	Low:     new(2048),
	Medium:  new(8192),
	High:    new(16384),
}

// ClampReasoningForModel collapses "xhigh" to "high" unless the model
// supports xhigh reasoning (see SupportsXhigh). Pass nil for model to force
// the legacy collapse unconditionally.
func ClampReasoningForModel(level ReasoningLevel, model *Model) ReasoningLevel {
	if level == ReasoningXHigh && !SupportsXhigh(model) {
		return ReasoningHigh
	}
	return level
}

// ClampReasoning collapses "xhigh" to "high" unconditionally. Equivalent to
// ClampReasoningForModel(level, nil). Providers should prefer
// ClampReasoningForModel when a model is in scope so xhigh-capable models
// (GPT-5.2+, Opus 4.6+) retain the xhigh level.
func ClampReasoning(level ReasoningLevel) ReasoningLevel {
	return ClampReasoningForModel(level, nil)
}

// ThinkingBudgetForLevel returns the thinking budget tokens for a reasoning
// level, using customBudgets if provided, otherwise DefaultThinkingBudgets.
func ThinkingBudgetForLevel(level ReasoningLevel, customBudgets *ThinkingBudgets) int {
	budgets := DefaultThinkingBudgets
	if customBudgets != nil {
		if customBudgets.Minimal != nil {
			budgets.Minimal = customBudgets.Minimal
		}
		if customBudgets.Low != nil {
			budgets.Low = customBudgets.Low
		}
		if customBudgets.Medium != nil {
			budgets.Medium = customBudgets.Medium
		}
		if customBudgets.High != nil {
			budgets.High = customBudgets.High
		}
	}

	switch level {
	case ReasoningMinimal:
		return *budgets.Minimal
	case ReasoningLow:
		return *budgets.Low
	case ReasoningMedium:
		return *budgets.Medium
	case ReasoningHigh, ReasoningXHigh:
		return *budgets.High
	default:
		return 0
	}
}

// AdjustMaxTokensForThinkingForModel is the model-aware variant of
// AdjustMaxTokensForThinking. xhigh is preserved when the model supports it
// (see SupportsXhigh), otherwise it is collapsed to high before applying
// budgets. Pass nil for model to force the legacy xhigh→high collapse.
func AdjustMaxTokensForThinkingForModel(baseMaxTokens int, modelMaxTokens int, level ReasoningLevel, model *Model, customBudgets *ThinkingBudgets) (adjustedMaxTokens int, thinkingBudget int) {
	level = ClampReasoningForModel(level, model)
	budget := ThinkingBudgetForLevel(level, customBudgets)

	maxTokens := min(baseMaxTokens+budget, modelMaxTokens)
	if maxTokens <= budget {
		// Ensure at least 1024 tokens for output
		budget = max(0, maxTokens-1024)
	}

	return maxTokens, budget
}

// AdjustMaxTokensForThinking computes the adjusted maxTokens and thinkingBudget
// per the shared algorithm:
//   - clamp "xhigh" → "high"
//   - budget = level default (or custom override)
//   - maxTokens = min(baseMaxTokens + budget, modelMaxTokens)
//   - if maxTokens <= budget, subtract a 1024-token floor from budget
//
// Providers supporting thinking should call this when building request options.
// Equivalent to AdjustMaxTokensForThinkingForModel(..., nil, customBudgets).
func AdjustMaxTokensForThinking(baseMaxTokens int, modelMaxTokens int, level ReasoningLevel, customBudgets *ThinkingBudgets) (adjustedMaxTokens int, thinkingBudget int) {
	return AdjustMaxTokensForThinkingForModel(baseMaxTokens, modelMaxTokens, level, nil, customBudgets)
}

func getMaxTokens(opts *options, model *Model) int {
	if opts.maxTokens > 0 {
		return opts.maxTokens
	}

	return model.MaxTokens
}

// GetTemperature returns the temperature from options.
func (o *options) GetTemperature() *float64 {
	return o.temperature
}

// GetTopP returns the topP from options.
func (o *options) GetTopP() *float64 {
	return o.topP
}

// WithTextVerbosity sets the verbosity level for text responses (codex).
func WithTextVerbosity(verbosity string) Option {
	return func(o *options) {
		o.textVerbosity = verbosity
	}
}

// WithServiceTier sets the service tier for Responses API.
func WithServiceTier(tier string) Option {
	return func(o *options) {
		o.serviceTier = tier
	}
}

// WithReasoningSummary sets the reasoning summary option.
func WithReasoningSummary(summary string) Option {
	return func(o *options) {
		o.reasoningSummary = summary
	}
}

// WithReasoningEffort sets the reasoning effort level for Responses API.
// Valid values are "minimal", "low", "medium", "high", "xhigh".
// Models that don't support reasoning will ignore this option.
func WithReasoningEffort(effort string) Option {
	return func(o *options) {
		o.reasoningEffort = effort
	}
}

// WithToolChoice controls which tools the model can call.
// Valid values depend on the provider and can be either a simple mode string
// (for example "auto", "none", "any", "required") or a structured provider
// request object such as {"type":"function","function":{"name":"read_file"}}.
func WithToolChoice(choice any) Option {
	return func(o *options) {
		o.toolChoice = choice
	}
}

// WithClient injects a prebuilt client instance for top-level helper calls.
// This mirrors the TypeScript provider-specific client override capability in a
// Go-idiomatic way by allowing any ai.Client implementation.
func WithClient(client Client) Option {
	return func(o *options) {
		o.client = client
	}
}

// WithAzureAPIVersion overrides the Azure OpenAI API version for a call.
func WithAzureAPIVersion(version string) Option {
	return func(o *options) {
		o.azureAPIVersion = version
	}
}

// WithAzureResourceName overrides the Azure OpenAI resource name for a call.
func WithAzureResourceName(resource string) Option {
	return func(o *options) {
		o.azureResourceName = resource
	}
}

// WithAzureBaseURL overrides the Azure OpenAI base URL for a call.
func WithAzureBaseURL(baseURL string) Option {
	return func(o *options) {
		o.azureBaseURL = baseURL
	}
}

// WithAzureDeploymentName overrides the Azure OpenAI deployment name for a call.
func WithAzureDeploymentName(name string) Option {
	return func(o *options) {
		o.azureDeploymentName = name
	}
}

// WithVertexProject overrides the Vertex AI project for a call.
func WithVertexProject(project string) Option {
	return func(o *options) {
		o.vertexProject = project
	}
}

// WithVertexLocation overrides the Vertex AI location for a call.
func WithVertexLocation(location string) Option {
	return func(o *options) {
		o.vertexLocation = location
	}
}

// WithGeminiCLIProjectID overrides the Google Gemini CLI project ID for a call.
func WithGeminiCLIProjectID(projectID string) Option {
	return func(o *options) {
		o.geminiCLIProjectID = projectID
	}
}

// WithGoogleThinkingLevel overrides the Google thinking level for Gemini 3 /
// Vertex / Gemini CLI request builders that support explicit level control.
func WithGoogleThinkingLevel(level GoogleThinkingLevel) Option {
	return func(o *options) {
		o.googleThinkingLevel = level
	}
}

// WithBedrockRegion overrides the AWS region for Bedrock calls.
func WithBedrockRegion(region string) Option {
	return func(o *options) {
		o.bedrockRegion = region
	}
}

// WithBedrockProfile overrides the AWS shared config profile for Bedrock calls.
func WithBedrockProfile(profile string) Option {
	return func(o *options) {
		o.bedrockProfile = profile
	}
}

// WithBedrockRequestMetadata attaches Bedrock request metadata tags.
func WithBedrockRequestMetadata(metadata map[string]string) Option {
	return func(o *options) {
		if len(metadata) == 0 {
			o.bedrockRequestMetadata = nil
			return
		}
		o.bedrockRequestMetadata = maps.Clone(metadata)
	}
}

// WithBedrockBearerToken configures Bedrock bearer-token authentication.
func WithBedrockBearerToken(token string) Option {
	return func(o *options) {
		o.bedrockBearerToken = token
	}
}

// WithBedrockThinkingBudgets overrides Bedrock thinking budgets.
func WithBedrockThinkingBudgets(budgets *ThinkingBudgets) Option {
	return func(o *options) {
		o.bedrockThinkingBudgets = budgets
	}
}

// WithBedrockInterleavedThinking controls Bedrock Anthropic interleaved thinking.
func WithBedrockInterleavedThinking(enabled bool) Option {
	return func(o *options) {
		o.bedrockInterleavedThinking = &enabled
	}
}

// WithInterleavedThinking controls Anthropic interleaved thinking.
// This enables Claude's ability to think while generating tool calls,
// allowing interleaved thinking and tool use within a single response.
func WithInterleavedThinking(enabled bool) Option {
	return func(o *options) {
		o.interleavedThinking = &enabled
	}
}

// SimpleStreamOptions holds the simple-stream options that can be forwarded.
// This is used by the agent proxy to extract options for forwarding.
type SimpleStreamOptions struct {
	Temperature     *float64
	MaxTokens       int
	Reasoning       ReasoningLevel
	APIKey          string
	CacheRetention  string
	SessionID       string
	Headers         map[string]string
	OnPayload       func(any) any
	OnPayloadModel  func(any, *Model) any
	OnResponse      func(ProviderResponse)
	OnResponseModel func(ProviderResponse, *Model)
	MaxRetryDelayMs int
	Metadata        map[string]any
}

// BuildBaseOptions builds a []Option from SimpleStreamOptions.
// This mirrors the TypeScript buildBaseOptions function from simple-options.ts.
//
// It applies the following defaults:
// - maxTokens: opts.MaxTokens if set, otherwise min(model.MaxTokens, 32000)
//
// Used internally by StreamSimple / CompleteSimple helpers to convert the
// *SimpleStreamOptions input into the []Option list accepted by
// StreamWithModel / CompleteWithModel. Also exported so callers (including
// the agent proxy path) can construct a base option set from a simple-options
// struct without going through the public simple helpers.
func BuildBaseOptions(model *Model, opts *SimpleStreamOptions) []Option {
	result := []Option{}

	if opts == nil {
		// Apply default maxTokens: min(model.maxTokens, 32000)
		maxTokens := model.MaxTokens
		if maxTokens > 32000 {
			maxTokens = 32000
		}
		return append(result, WithMaxTokens(maxTokens))
	}

	if opts.Temperature != nil {
		result = append(result, WithTemperature(*opts.Temperature))
	}

	if opts.MaxTokens > 0 {
		result = append(result, WithMaxTokens(opts.MaxTokens))
	} else {
		// Default to min(model.maxTokens, 32000)
		maxTokens := model.MaxTokens
		if maxTokens > 32000 {
			maxTokens = 32000
		}
		result = append(result, WithMaxTokens(maxTokens))
	}

	if opts.Reasoning != ReasoningOff && opts.Reasoning != "" {
		result = append(result, WithReasoning(opts.Reasoning))
	}
	if opts.APIKey != "" {
		result = append(result, WithAPIKey(opts.APIKey))
	}
	if opts.CacheRetention != "" {
		result = append(result, WithCacheRetention(opts.CacheRetention))
	}
	if opts.SessionID != "" {
		result = append(result, WithSessionID(opts.SessionID))
	}
	if len(opts.Headers) > 0 {
		result = append(result, WithHeaders(opts.Headers))
	}
	if opts.OnPayload != nil {
		result = append(result, WithPayloadCallback(opts.OnPayload))
	}
	if opts.OnPayloadModel != nil {
		result = append(result, WithPayloadCallbackForModel(opts.OnPayloadModel))
	}
	if opts.OnResponse != nil {
		result = append(result, WithResponseCallback(opts.OnResponse))
	}
	if opts.OnResponseModel != nil {
		result = append(result, WithResponseCallbackForModel(opts.OnResponseModel))
	}
	if opts.MaxRetryDelayMs > 0 {
		result = append(result, WithMaxRetryDelayMs(opts.MaxRetryDelayMs))
	}
	if len(opts.Metadata) > 0 {
		result = append(result, WithMetadata(opts.Metadata))
	}

	return result
}

// ExtractSimpleStreamOptions extracts simple-stream options from the provided options.
func ExtractSimpleStreamOptions(opts []Option) SimpleStreamOptions {
	var o options
	o.apply(opts)

	result := SimpleStreamOptions{
		Reasoning:       o.reasoning,
		APIKey:          o.apiKey,
		OnPayload:       o.onPayload,
		OnPayloadModel:  o.onPayloadWithModel,
		OnResponse:      o.onResponse,
		OnResponseModel: o.onResponseWithModel,
	}

	if o.temperature != nil {
		result.Temperature = o.temperature
	}
	if o.maxTokens > 0 {
		result.MaxTokens = o.maxTokens
	}
	if o.cacheRetention != "" {
		result.CacheRetention = o.cacheRetention
	}
	if o.sessionID != "" {
		result.SessionID = o.sessionID
	}
	if len(o.headers) > 0 {
		result.Headers = make(map[string]string, len(o.headers))
		for k, v := range o.headers {
			result.Headers[k] = v
		}
	}
	if o.maxRetryDelayMs > 0 {
		result.MaxRetryDelayMs = o.maxRetryDelayMs
	}
	if len(o.metadata) > 0 {
		result.Metadata = make(map[string]any, len(o.metadata))
		for k, v := range o.metadata {
			result.Metadata[k] = v
		}
	}

	return result
}
