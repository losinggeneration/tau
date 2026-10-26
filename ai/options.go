package ai // import "github.com/losinggeneration/tau/ai"

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// WithTimeout bounds the wait for response headers, not streaming body delivery.
func WithTimeout(timeout time.Duration) Option {
	return func(o *options) { o.timeout = timeout; o.timeoutSet = true }
}

// Option configures streaming or completion options.
type Option func(*options)

type options struct {
	maxRetries                 *int
	timeout                    time.Duration
	timeoutSet                 bool
	apiKey                     string
	noEnvCredentials           bool
	providerEnv                map[string]string
	preferredTransport         Transport
	transportOpts              *TransportOption
	reasoning                  ReasoningLevel
	thinkingEnabled            bool
	thinkingBudgetTokens       int
	thinkingBudgets            *ThinkingBudgets
	googleThinkingLevel        GoogleThinkingLevel
	thinkingDisplay            string // "summarized" | "omitted"
	effort                     string // "low" | "medium" | "high" | "xhigh" | "max"
	thinkingType               string // "adaptive" | "enabled" | "disabled"
	maxTokens                  int
	topP                       *float64
	samplingParams             map[string]any
	temperature                *float64
	cacheRetention             string
	sessionID                  string
	maxRetryDelayMs            int
	maxRetryDelaySet           bool
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

// WithoutEnvCredentials disables implicit environment and ambient cloud authentication.
// Explicit keys, configured headers, and configured variable references still apply.
func WithoutEnvCredentials() Option {
	return func(o *options) { o.noEnvCredentials = true }
}

func (o *options) envAPIKey(provider string) string {
	if o.noEnvCredentials {
		return ""
	}
	return getEnvAPIKey(provider, o.providerEnvValue)
}

func (o *options) envHeaders(provider string) map[string]string {
	if o.noEnvCredentials {
		return nil
	}
	return getEnvHeaders(provider, o.providerEnvValue)
}

// WithProviderEnv supplies request-local environment overrides. Empty entries
// fall back to the process environment.
func WithProviderEnv(env map[string]string) Option {
	owned := maps.Clone(env)
	return func(o *options) { o.providerEnv = maps.Clone(owned) }
}

func (o *options) providerEnvValue(name string) string {
	if o != nil {
		if value := o.providerEnv[name]; value != "" {
			return value
		}
	}
	return os.Getenv(name)
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
		o.thinkingType = ""
		if !enabled {
			o.thinkingType = "disabled"
		}
	}
}

// WithThinkingBudgetTokens sets the token budget for thinking.
func WithThinkingBudgetTokens(tokens int) Option {
	return func(o *options) {
		o.thinkingBudgetTokens = tokens
	}
}

// WithThinkingBudgets sets per-level budgets for token-based thinking providers.
func WithThinkingBudgets(budgets *ThinkingBudgets) Option {
	return func(o *options) { o.thinkingBudgets = budgets }
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

// WithSamplingParams merges arbitrary sampling parameters (for example
// top_k, min_p, or repetition_penalty) into OpenAI-compatible request bodies
// after the named fields, over the model's own sampling parameters. Other
// APIs ignore them.
func WithSamplingParams(params map[string]any) Option {
	return func(o *options) {
		o.samplingParams = maps.Clone(params)
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
		o.maxRetryDelaySet = true
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

func sessionHeaders(model *Model, o *options) map[string]string {
	sessionID := cacheSessionID(o)
	if sessionID == "" {
		return nil
	}
	var format string
	if model.Compat != nil {
		format = model.Compat.SessionAffinityFormat
	}
	if format == "" {
		format = "openai"
		if model.Provider == ProviderOpenRouter || strings.Contains(model.BaseURL, "openrouter.ai") {
			format = "openrouter"
		}
	}
	if format == "openrouter" {
		return map[string]string{"x-session-id": sessionID}
	}
	headers := map[string]string{"x-client-request-id": sessionID}
	if format == "openai" {
		headers["session_id"] = sessionID
	}
	if model.API == APIOpenAICompletions || model.API == APIOpenAICompatible {
		headers["x-session-affinity"] = sessionID
	}
	return headers
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
	if level == ReasoningMax {
		return ReasoningLevel(ClampThinkingLevel(model, string(level)))
	}
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
	case ReasoningHigh, ReasoningXHigh, ReasoningMax:
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
		budget = max(0, maxTokens-minAnswerTokens)
	}

	return maxTokens, budget
}

// fitThinkingBudget grows the response ceiling by the thinking budget up to
// the model cap and the context room, then trims the budget so the answer
// keeps minAnswerTokens, as upstream's budget-based thinking does.
func fitThinkingBudget(model *Model, prompt *Context, maxTokens, budget int) (int, int) {
	maxTokens += budget
	if model.MaxTokens > 0 {
		maxTokens = min(maxTokens, model.MaxTokens)
	}
	maxTokens = ClampMaxTokensToContext(model, prompt, maxTokens)
	return maxTokens, min(budget, max(0, maxTokens-minAnswerTokens))
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
	Timeout          *time.Duration
	MaxRetries       *int
	MaxRetryDelaySet bool
	Temperature      *float64
	MaxTokens        int
	Reasoning        ReasoningLevel
	ThinkingBudgets  *ThinkingBudgets
	APIKey           string
	NoEnvCredentials bool
	Env              map[string]string
	CacheRetention   string
	SessionID        string
	Transport        Transport
	Headers          map[string]string
	OnPayload        func(any) any
	OnPayloadModel   func(any, *Model) any
	OnResponse       func(ProviderResponse)
	OnResponseModel  func(ProviderResponse, *Model)
	MaxRetryDelayMs  int
	Metadata         map[string]any
	SamplingParams   map[string]any
}

// BuildBaseOptions builds a []Option from SimpleStreamOptions, mirroring
// upstream buildBaseOptions. maxTokens defaults to the model cap, and either
// value is clamped to the room prompt leaves in the context window.
func BuildBaseOptions(model *Model, prompt *Context, opts *SimpleStreamOptions) []Option {
	if opts == nil {
		opts = &SimpleStreamOptions{}
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = model.MaxTokens
	}
	result := []Option{WithMaxTokens(ClampMaxTokensToContext(model, prompt, maxTokens))}

	if opts.Temperature != nil {
		result = append(result, WithTemperature(*opts.Temperature))
	}
	if len(opts.SamplingParams) > 0 {
		result = append(result, WithSamplingParams(opts.SamplingParams))
	}

	if opts.ThinkingBudgets != nil {
		result = append(result, WithThinkingBudgets(opts.ThinkingBudgets))
	}
	if opts.Reasoning != "" {
		result = append(result, WithReasoning(opts.Reasoning))
	} else if model.API == APIAnthropicMessages {
		result = append(result, WithThinkingEnabled(false))
	}
	if opts.Env != nil {
		result = append(result, WithProviderEnv(opts.Env))
	}
	if opts.NoEnvCredentials {
		result = append(result, WithoutEnvCredentials())
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
	if opts.Transport != "" {
		result = append(result, WithTransport(opts.Transport))
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
	if opts.Timeout != nil {
		result = append(result, WithTimeout(*opts.Timeout))
	}
	if opts.MaxRetries != nil {
		result = append(result, WithMaxRetries(*opts.MaxRetries))
	}
	if opts.MaxRetryDelaySet || opts.MaxRetryDelayMs > 0 {
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
		Reasoning:        o.reasoning,
		ThinkingBudgets:  o.thinkingBudgets,
		APIKey:           o.apiKey,
		NoEnvCredentials: o.noEnvCredentials,
		Env:              maps.Clone(o.providerEnv),
		OnPayload:        o.onPayload,
		OnPayloadModel:   o.onPayloadWithModel,
		OnResponse:       o.onResponse,
		OnResponseModel:  o.onResponseWithModel,
	}

	if o.temperature != nil {
		result.Temperature = o.temperature
	}
	if o.maxTokens > 0 {
		result.MaxTokens = o.maxTokens
	}
	if len(o.samplingParams) > 0 {
		result.SamplingParams = maps.Clone(o.samplingParams)
	}
	if o.cacheRetention != "" {
		result.CacheRetention = o.cacheRetention
	}
	if o.sessionID != "" {
		result.SessionID = o.sessionID
	}
	result.Transport = o.preferredTransport
	if len(o.headers) > 0 {
		result.Headers = make(map[string]string, len(o.headers))
		for k, v := range o.headers {
			result.Headers[k] = v
		}
	}
	if o.timeoutSet {
		result.Timeout = new(o.timeout)
	}
	result.MaxRetries = o.maxRetries
	result.MaxRetryDelaySet = o.maxRetryDelaySet
	if o.maxRetryDelaySet || o.maxRetryDelayMs > 0 {
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

// responsesReasoningEffort returns the requested Responses effort mapped
// through the model's thinking map, or "" when reasoning is off or unset.
func responsesReasoningEffort(model *Model, config *options) string {
	effort := config.reasoningEffort
	// Explicit off is the agent's "no reasoning", so it is never clamped up
	// to the model's lowest level.
	if effort == "" && config.reasoning != "" && config.reasoning != ReasoningOff {
		effort = ClampThinkingLevel(model, string(config.reasoning))
	}
	if effort == "" || effort == "off" {
		return ""
	}
	if mapped := model.ThinkingLevelMap[effort]; mapped != nil {
		return *mapped
	}
	return effort
}

// responsesOffEffort returns the effort that turns reasoning off, and false
// when the model's thinking map marks off unsupported.
func responsesOffEffort(model *Model) (string, bool) {
	mapped, ok := model.ThinkingLevelMap["off"]
	switch {
	case !ok:
		return "none", true
	case mapped == nil:
		return "", false
	default:
		return *mapped, true
	}
}

// resolveSamplingParams merges model, thinking-level, and request sampling
// parameters per key, later sources winning. Nil means none apply.
func resolveSamplingParams(model *Model, config *options) map[string]any {
	level := string(config.reasoning)
	if level == "" {
		level = "off"
	}
	levelParams := model.SamplingParamsByThinkingLevel[ClampThinkingLevel(model, level)]
	if len(model.SamplingParams) == 0 && len(levelParams) == 0 && len(config.samplingParams) == 0 {
		return nil
	}
	params := maps.Clone(model.SamplingParams)
	if params == nil {
		params = make(map[string]any)
	}
	maps.Copy(params, levelParams)
	maps.Copy(params, config.samplingParams)
	return params
}

// WithMaxRetries sets the retry limit for providers that support request retries.
func WithMaxRetries(count int) Option {
	return func(o *options) { o.maxRetries = new(max(0, count)) }
}

const (
	charsPerToken        = 3.5
	estimatedImageChars  = 4800
	contextSafetyTokens  = 4096
	minResponseMaxTokens = 1
	// minAnswerTokens is always left for the answer when a thinking budget
	// shares the response ceiling.
	minAnswerTokens = 1024
)

// ClampMaxTokensToContext caps a response at the context window left after
// the estimated prompt and a fixed safety reserve, never below one token.
func ClampMaxTokensToContext(model *Model, prompt *Context, maxTokens int) int {
	if model.ContextWindow <= 0 {
		return max(minResponseMaxTokens, maxTokens)
	}
	available := model.ContextWindow - EstimateContextTokens(prompt) - contextSafetyTokens
	return min(maxTokens, max(minResponseMaxTokens, available))
}

// EstimateContextTokens estimates prompt tokens at 3.5 characters per token,
// counting each image as 4800 characters. It is the request-budget heuristic;
// compaction keeps its own.
// ponytail: ai messages carry no usage, so this never anchors on the last
// reported usage like upstream; add it if requests start overflowing.
func EstimateContextTokens(prompt *Context) int {
	var tokens int
	for _, message := range NormalizeContext(prompt).Messages {
		tokens += estimateMessageTokens(message)
	}
	return tokens
}

func estimateMessageTokens(message Message) int {
	estimate := func(chars int) int { return int(math.Ceil(float64(chars) / charsPerToken)) }
	if message.Role == "system" {
		chars := utf8.RuneCountInString(transcriptText(message.Content))
		for _, section := range message.Sections {
			if section != nil {
				chars += utf8.RuneCountInString(*section)
			}
		}
		tokens := estimate(chars)
		for _, tools := range []any{message.ToolsAdded, message.ToolsRemoved} {
			if data, err := json.Marshal(tools); err == nil && string(data) != "null" && string(data) != "[]" {
				tokens += estimate(utf8.RuneCount(data))
			}
		}
		return tokens
	}
	if text, ok := message.Content.(string); ok {
		return estimate(utf8.RuneCountInString(text))
	}
	var chars int
	for _, block := range message.ContentBlocks() {
		switch block.Type {
		case ContentBlockImage:
			chars += estimatedImageChars
		case ContentBlockThinking:
			chars += utf8.RuneCountInString(block.Thinking)
		case ContentBlockToolCall:
			chars += utf8.RuneCountInString(block.Name) + utf8.RuneCount(block.Arguments)
		default:
			chars += utf8.RuneCountInString(block.Text)
		}
	}
	return estimate(chars)
}
