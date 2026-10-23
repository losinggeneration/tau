package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// FauxProvider is a deterministic in-memory provider for testing and parity.
type FauxProvider struct {
	api             string
	provider        string
	model           *Model
	models          []*Model
	state           *fauxState
	responses       []fauxResponseStep
	tokensPerSecond float64
	minTokenSize    int
	maxTokenSize    int
	promptCache     map[string]string
}

type fauxState struct {
	callCount atomic.Int64
}

type (
	fauxResponseStep any
)

// FauxResponseStep is a queued faux response or response factory.
type FauxResponseStep = fauxResponseStep

// FauxModelDefinition defines a faux model for registration.
type FauxModelDefinition struct {
	ID            string
	Name          string
	Reasoning     bool
	Input         []InputType
	Cost          CostProfile
	ContextWindow int
	MaxTokens     int
}

// FauxContentBlock represents content in a faux response.
type FauxContentBlock any

// FauxText creates a text content block.
func FauxText(text string) ContentBlock {
	return ContentBlock{Type: ContentBlockText, Text: text}
}

// FauxThinking creates a thinking content block.
func FauxThinking(thinking string) ContentBlock {
	return ContentBlock{Type: ContentBlockThinking, Thinking: thinking}
}

// FauxToolCall creates a tool call content block.
func FauxToolCall(name string, arguments json.RawMessage, opts ...FauxToolCallOption) ContentBlock {
	o := fauxToolCallOptions{id: randomFauxID("tool")}
	for _, opt := range opts {
		opt(&o)
	}
	return ContentBlock{
		Type:      ContentBlockToolCall,
		ID:        o.id,
		Name:      name,
		Arguments: arguments,
	}
}

type fauxToolCallOptions struct {
	id string
}

type FauxToolCallOption func(*fauxToolCallOptions)

// WithFauxToolCallID sets the tool call ID.
func WithFauxToolCallID(id string) FauxToolCallOption {
	return func(o *fauxToolCallOptions) {
		o.id = id
	}
}

// FauxAssistantMessage creates an assistant message from content.
func FauxAssistantMessage(content any, opts ...FauxAssistantOption) AssistantMessage {
	o := fauxAssistantOptions{
		stopReason: StopReasonStop,
		model:      "faux-1",
	}
	for _, opt := range opts {
		opt(&o)
	}

	blocks := normalizeFauxContent(content)
	return AssistantMessage{
		Role:         "assistant",
		Content:      blocks,
		API:          "faux",
		Provider:     "faux",
		Model:        o.model,
		StopReason:   o.stopReason,
		ErrorMessage: o.errorMessage,
		ResponseID:   o.responseID,
		Timestamp:    o.timestamp,
		Usage: Usage{
			Input:       0,
			Output:      0,
			CacheRead:   0,
			CacheWrite:  0,
			TotalTokens: 0,
			Cost: Cost{
				Input:      0,
				Output:     0,
				CacheRead:  0,
				CacheWrite: 0,
				Total:      0,
			},
		},
	}
}

type fauxAssistantOptions struct {
	stopReason   StopReason
	errorMessage string
	responseID   string
	timestamp    int64
	model        string
}

type FauxAssistantOption func(*fauxAssistantOptions)

// WithFauxStopReason sets the stop reason.
func WithFauxStopReason(reason StopReason) FauxAssistantOption {
	return func(o *fauxAssistantOptions) {
		o.stopReason = reason
	}
}

// WithFauxErrorMessage sets the error message.
func WithFauxErrorMessage(msg string) FauxAssistantOption {
	return func(o *fauxAssistantOptions) {
		o.errorMessage = msg
	}
}

// WithFauxResponseID sets the response ID.
func WithFauxResponseID(id string) FauxAssistantOption {
	return func(o *fauxAssistantOptions) {
		o.responseID = id
	}
}

// WithFauxTimestamp sets the timestamp.
func WithFauxTimestamp(ts int64) FauxAssistantOption {
	return func(o *fauxAssistantOptions) {
		o.timestamp = ts
	}
}

// WithFauxModel sets the model ID.
func WithFauxModel(model string) FauxAssistantOption {
	return func(o *fauxAssistantOptions) {
		o.model = model
	}
}

func normalizeFauxContent(content any) []ContentBlock {
	if content == nil {
		return nil
	}
	switch c := content.(type) {
	case string:
		return []ContentBlock{FauxText(c)}
	case ContentBlock:
		return []ContentBlock{c}
	case []ContentBlock:
		return c
	default:
		return nil
	}
}

func randomFauxID(prefix string) string {
	return fmt.Sprintf("%s:%d:%s", prefix, time.Now().UnixMilli(), uuid.New().String()[:8])
}

// estimateTokens estimates token count for text.
func estimateTokens(text string) int {
	return int(math.Ceil(float64(len(text)) / 4))
}

func splitStringByTokenSize(text string, minTokenSize, maxTokenSize int) []string {
	if text == "" {
		return []string{""}
	}
	var chunks []string
	index := 0
	for index < len(text) {
		tokenSize := minTokenSize + int(time.Now().UnixNano()%int64(maxTokenSize-minTokenSize+1))
		charSize := tokenSize * 4
		if charSize < 1 {
			charSize = 1
		}
		end := index + charSize
		if end > len(text) {
			end = len(text)
		}
		chunks = append(chunks, text[index:end])
		index = end
	}
	if len(chunks) == 0 {
		return []string{""}
	}
	return chunks
}

func contentToFauxText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []ContentBlock:
		parts := make([]string, 0, len(c))
		for _, block := range c {
			switch block.Type {
			case ContentBlockText:
				parts = append(parts, block.Text)
			case ContentBlockImage:
				parts = append(parts, fmt.Sprintf("[image:%s:%d]", block.MimeType, len(block.Data)))
			case ContentBlockThinking:
				parts = append(parts, block.Thinking)
			case ContentBlockToolCall:
				parts = append(parts, block.Name+":"+string(block.Arguments))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func serializeFauxContext(ctx *Context) string {
	if ctx == nil {
		return ""
	}
	parts := make([]string, 0, len(ctx.Messages)+2)
	if ctx.SystemPrompt != "" {
		parts = append(parts, "system:"+ctx.SystemPrompt)
	}
	for _, msg := range ctx.Messages {
		switch normalizeMessageRole(msg.Role) {
		case "toolResult":
			parts = append(parts, "toolResult:"+msg.ToolName+"\n"+contentToFauxText(msg.Content))
		default:
			parts = append(parts, msg.Role+":"+contentToFauxText(msg.Content))
		}
	}
	if len(ctx.Tools) > 0 {
		data, _ := json.Marshal(ctx.Tools)
		parts = append(parts, "tools:"+string(data))
	}
	return strings.Join(parts, "\n\n")
}

func commonPrefixLength(a, b string) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	for i := 0; i < limit; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return limit
}

func withUsageEstimate(message AssistantMessage, ctx *Context, opts *options, promptCache map[string]string) AssistantMessage {
	promptText := serializeFauxContext(ctx)
	promptTokens := estimateTokens(promptText)
	outputTokens := estimateTokens(contentToFauxText(message.Content))

	usage := Usage{
		Input:       promptTokens,
		Output:      outputTokens,
		TotalTokens: promptTokens + outputTokens,
	}
	if opts != nil {
		sessionID := cacheSessionID(opts)
		if sessionID != "" {
			if previous, ok := promptCache[sessionID]; ok {
				cachedChars := commonPrefixLength(previous, promptText)
				usage.CacheRead = estimateTokens(previous[:cachedChars])
				usage.CacheWrite = estimateTokens(promptText[cachedChars:])
				usage.Input = max(0, promptTokens-usage.CacheRead)
			} else {
				usage.CacheWrite = promptTokens
			}
			promptCache[sessionID] = promptText
			usage.TotalTokens = usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
		}
	}
	message.Usage = usage
	return message
}

func fauxAbortedMessage(partial AssistantMessage) AssistantMessage {
	partial.StopReason = StopReasonAborted
	partial.ErrorMessage = "Request was aborted"
	if partial.Timestamp == 0 {
		partial.Timestamp = time.Now().UnixMilli()
	}
	return partial
}

// NewFauxProvider creates a new faux provider for testing.
func NewFauxProvider(opts ...FauxProviderOption) *FauxProvider {
	o := fauxProviderOptions{
		api:             "faux",
		provider:        "faux",
		tokensPerSecond: 0, // instant
		minTokenSize:    3,
		maxTokenSize:    5,
	}
	for _, opt := range opts {
		opt(&o)
	}

	if o.model == nil {
		o.model = &Model{
			ID:            "faux-1",
			Name:          "Faux Model",
			API:           o.api,
			Provider:      o.provider,
			BaseURL:       "http://localhost:0",
			Reasoning:     false,
			Input:         []InputType{InputText, InputImage},
			Cost:          CostProfile{},
			ContextWindow: 128000,
			MaxTokens:     16384,
		}
	}

	models := o.models
	if len(models) == 0 {
		models = []*Model{o.model}
	}
	if o.model == nil && len(models) > 0 {
		o.model = models[0]
	}

	return &FauxProvider{
		api:             o.api,
		provider:        o.provider,
		model:           o.model,
		models:          models,
		state:           &fauxState{},
		responses:       []fauxResponseStep{},
		tokensPerSecond: o.tokensPerSecond,
		minTokenSize:    o.minTokenSize,
		maxTokenSize:    o.maxTokenSize,
		promptCache:     map[string]string{},
	}
}

type fauxProviderOptions struct {
	api             string
	provider        string
	model           *Model
	models          []*Model
	tokensPerSecond float64
	minTokenSize    int
	maxTokenSize    int
}

type FauxProviderOption func(*fauxProviderOptions)

// WithFauxAPI sets the API identifier.
func WithFauxAPI(api string) FauxProviderOption {
	return func(o *fauxProviderOptions) {
		o.api = api
	}
}

// WithFauxProvider sets the provider identifier.
func WithFauxProvider(provider string) FauxProviderOption {
	return func(o *fauxProviderOptions) {
		o.provider = provider
	}
}

// WithFauxModelProvider sets the model for a provider.
func WithFauxModelProvider(model *Model) FauxProviderOption {
	return func(o *fauxProviderOptions) {
		o.model = model
		o.models = []*Model{model}
	}
}

// WithFauxModels configures multiple faux models on one provider registration.
func WithFauxModels(defs []FauxModelDefinition) FauxProviderOption {
	return func(o *fauxProviderOptions) {
		if len(defs) == 0 {
			o.models = nil
			return
		}
		models := make([]*Model, 0, len(defs))
		for _, def := range defs {
			input := def.Input
			if len(input) == 0 {
				input = []InputType{InputText, InputImage}
			}
			name := def.Name
			if name == "" {
				name = def.ID
			}
			contextWindow := def.ContextWindow
			if contextWindow == 0 {
				contextWindow = 128000
			}
			maxTokens := def.MaxTokens
			if maxTokens == 0 {
				maxTokens = 16384
			}
			models = append(models, &Model{
				ID:            def.ID,
				Name:          name,
				API:           o.api,
				Provider:      o.provider,
				BaseURL:       "http://localhost:0",
				Reasoning:     def.Reasoning,
				Input:         input,
				Cost:          def.Cost,
				ContextWindow: contextWindow,
				MaxTokens:     maxTokens,
			})
		}
		if len(models) > 0 {
			o.models = models
			o.model = models[0]
		}
	}
}

// WithFauxTokensPerSecond sets the streaming rate.
func WithFauxTokensPerSecond(tps float64) FauxProviderOption {
	return func(o *fauxProviderOptions) {
		o.tokensPerSecond = tps
	}
}

// WithFauxTokenSize sets the min/max token sizes for chunking.
func WithFauxTokenSize(min, max int) FauxProviderOption {
	return func(o *fauxProviderOptions) {
		o.minTokenSize = min
		o.maxTokenSize = max
	}
}

// SetResponses sets the pending response queue.
func (p *FauxProvider) SetResponses(responses []AssistantMessage) {
	p.responses = make([]fauxResponseStep, len(responses))
	for i, r := range responses {
		p.responses[i] = r
	}
}

// SetResponseSteps replaces the queued faux response steps.
func (p *FauxProvider) SetResponseSteps(steps []FauxResponseStep) {
	p.responses = append([]fauxResponseStep(nil), steps...)
}

// AppendResponses appends responses to the pending queue.
func (p *FauxProvider) AppendResponses(responses []AssistantMessage) {
	for _, r := range responses {
		p.responses = append(p.responses, r)
	}
}

// AppendResponseSteps appends response steps to the pending queue.
func (p *FauxProvider) AppendResponseSteps(steps []FauxResponseStep) {
	p.responses = append(p.responses, steps...)
}

// GetPendingResponseCount returns the number of pending responses.
func (p *FauxProvider) GetPendingResponseCount() int {
	return len(p.responses)
}

// CallCount returns the number of calls made to the provider.
func (p *FauxProvider) CallCount() int64 {
	return p.state.callCount.Load()
}

// Models returns all faux models registered on this provider.
func (p *FauxProvider) Models() []*Model {
	return append([]*Model(nil), p.models...)
}

// GetModel returns the first faux model or a specific model by ID.
func (p *FauxProvider) GetModel(ids ...string) *Model {
	if len(ids) == 0 {
		if len(p.models) == 0 {
			return nil
		}
		return p.models[0]
	}
	for _, model := range p.models {
		if model.ID == ids[0] {
			return model
		}
	}
	return nil
}

// Unregister removes the faux provider from the global registry.
func (p *FauxProvider) Unregister() {
	Unregister(p.api)
}

// API implements Provider.
func (p *FauxProvider) API() string {
	return p.api
}

// IsAlias implements Provider.
func (p *FauxProvider) IsAlias() bool { return false }

// Capabilities implements CapableProvider.
func (p *FauxProvider) Capabilities() Capabilities {
	return Capabilities{
		Tools:     true,
		Vision:    true,
		Streaming: true,
	}
}

// NewClient implements Provider.
func (p *FauxProvider) NewClient(model *Model) (Client, error) {
	return &fauxClient{
		provider: p,
		model:    model,
	}, nil
}

type fauxClient struct {
	provider *FauxProvider
	model    *Model
}

// Complete implements Client.
func (c *fauxClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

func (c *fauxClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) error {
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
	o := applyOptions(opts)
	c.provider.state.callCount.Add(1)

	// Get next response
	if len(c.provider.responses) == 0 {
		//nolint:staticcheck // Preserve TS faux-provider error message parity.
		emitTerminalStreamError(ctx, handler, AssistantMessage{
			Role:       "assistant",
			API:        c.provider.api,
			Provider:   c.provider.provider,
			Model:      model.ID,
			Content:    []ContentBlock{},
			StopReason: StopReasonError,
			Timestamp:  time.Now().UnixMilli(),
		}, errors.New("No more faux responses queued"), StopReasonError)
		return nil
	}

	var message AssistantMessage
	step := c.provider.responses[0]
	c.provider.responses = c.provider.responses[1:]

	switch s := step.(type) {
	case AssistantMessage:
		message = s
		message.API = c.provider.api
		message.Provider = c.provider.provider
		message.Model = model.ID
	case func(*Context, []Option, *fauxState, *Model) (AssistantMessage, error):
		var err error
		message, err = s(prompt, opts, c.provider.state, model)
		if err != nil {
			emitTerminalStreamError(ctx, handler, AssistantMessage{
				Role:       "assistant",
				API:        c.provider.api,
				Provider:   c.provider.provider,
				Model:      model.ID,
				StopReason: StopReasonError,
				Timestamp:  time.Now().UnixMilli(),
			}, err, StopReasonError)
			return nil
		}
	}

	message = withUsageEstimate(message, prompt, o, c.provider.promptCache)

	// Stream the message with deltas
	c.streamWithDeltas(ctx, model, message, handler, o)

	return nil
}

func (c *fauxClient) streamWithDeltas(ctx context.Context, model *Model, message AssistantMessage, handler EventHandler, opts *options) {
	// Emit start
	partial := AssistantMessage{
		Role:       message.Role,
		API:        message.API,
		Provider:   message.Provider,
		Model:      message.Model,
		Content:    []ContentBlock{},
		StopReason: message.StopReason,
		Timestamp:  message.Timestamp,
	}
	handler(ctx, Start{Partial: partial})

	// Stream each content block
	for i, block := range message.Content {
		switch block.Type {
		case ContentBlockThinking:
			// Emit thinking start
			partial.Content = append(partial.Content, ContentBlock{Type: ContentBlockThinking, Thinking: ""})
			handler(ctx, ThinkingStart{ContentIndex: i, Partial: partial})

			// Emit thinking deltas
			chunks := splitStringByTokenSize(block.Thinking, c.provider.minTokenSize, c.provider.maxTokenSize)
			for _, chunk := range chunks {
				if err := c.waitChunk(ctx, chunk); err != nil {
					emitTerminalStreamError(ctx, handler, fauxAbortedMessage(partial), err, StopReasonAborted)
					return
				}
				partial.Content[i].Thinking += chunk
				handler(ctx, ThinkingDelta{ContentIndex: i, Delta: chunk, Partial: partial})
			}

			// Emit thinking end
			handler(ctx, ThinkingEnd{ContentIndex: i, Content: block.Thinking, Partial: partial})

		case ContentBlockText:
			// Emit text start
			partial.Content = append(partial.Content, ContentBlock{Type: ContentBlockText, Text: ""})
			handler(ctx, TextStart{ContentIndex: i, Partial: partial})

			// Emit text deltas
			chunks := splitStringByTokenSize(block.Text, c.provider.minTokenSize, c.provider.maxTokenSize)
			for _, chunk := range chunks {
				if err := c.waitChunk(ctx, chunk); err != nil {
					emitTerminalStreamError(ctx, handler, fauxAbortedMessage(partial), err, StopReasonAborted)
					return
				}
				partial.Content[i].Text += chunk
				handler(ctx, TextDelta{ContentIndex: i, Delta: chunk, Partial: partial})
			}

			// Emit text end
			handler(ctx, TextEnd{ContentIndex: i, Content: block.Text, Partial: partial})

		case ContentBlockToolCall:
			// Emit tool call start
			partial.Content = append(partial.Content, ContentBlock{
				Type:      ContentBlockToolCall,
				ID:        block.ID,
				Name:      block.Name,
				Namespace: block.Namespace,
				Arguments: json.RawMessage{},
			})
			handler(ctx, ToolCallStart{ContentIndex: i, Partial: partial})

			// Emit tool call deltas (argument chunks)
			argJSON := string(block.Arguments)
			chunks := splitStringByTokenSize(argJSON, c.provider.minTokenSize, c.provider.maxTokenSize)
			for _, chunk := range chunks {
				if err := c.waitChunk(ctx, chunk); err != nil {
					emitTerminalStreamError(ctx, handler, fauxAbortedMessage(partial), err, StopReasonAborted)
					return
				}
				nextJSON := append([]byte(partial.Content[i].Arguments), []byte(chunk)...)
				partial.Content[i].Arguments = nextJSON
				handler(ctx, ToolCallDelta{ContentIndex: i, Delta: chunk, Partial: partial})
			}

			args := ParseStreamingJSON(argJSON)
			partial.Content[i].Arguments = mustMarshalStreamingJSON(args)

			// Emit tool call end
			handler(ctx, ToolCallEnd{ContentIndex: i, ToolCall: ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Namespace: block.Namespace,
				Arguments: mustMarshalStreamingJSON(args),
			}, Partial: partial})
		}
	}

	// Handle error/aborted stop reasons
	if message.StopReason == StopReasonError || message.StopReason == StopReasonAborted {
		errMsg := message.ErrorMessage
		if errMsg == "" {
			if message.StopReason == StopReasonAborted {
				errMsg = "Request was aborted"
			} else {
				errMsg = "Faux provider error"
			}
		}
		emitTerminalStreamError(ctx, handler, message, fmt.Errorf("%s", errMsg), message.StopReason)
		return
	}

	// Emit done
	handler(ctx, Done{Reason: message.StopReason, Message: message})
}

func (c *fauxClient) waitChunk(ctx context.Context, chunk string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if c.provider.tokensPerSecond <= 0 {
		return nil
	}

	delay := time.Duration(float64(estimateTokens(chunk))/c.provider.tokensPerSecond*1000) * time.Millisecond
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RegisterFauxProvider registers a faux provider with the global registry.
func RegisterFauxProvider(opts ...FauxProviderOption) *FauxProvider {
	p := NewFauxProvider(opts...)
	Register(p.api, p)
	return p
}

// LoadImageContentBlock loads an image from a file path or URL and returns
// a ContentBlock suitable for inclusion in a Message.
func LoadImageContentBlock(pathOrURL string) (block ContentBlock, err error) {
	var data []byte
	var mimeType string

	// Try as URL first
	if strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://") {
		resp, err := http.Get(pathOrURL)
		if err != nil {
			return ContentBlock{}, fmt.Errorf("fetch image URL: %w", err)
		}
		defer func() {
			if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
				err = fmt.Errorf("close image response body: %w", closeErr)
			}
		}()
		data, err = io.ReadAll(resp.Body)
		if err != nil {
			return ContentBlock{}, fmt.Errorf("read image data: %w", err)
		}
		mimeType = resp.Header.Get("Content-Type")
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
	} else {
		// Load from file
		var err error
		data, err = os.ReadFile(pathOrURL)
		if err != nil {
			return ContentBlock{}, fmt.Errorf("read image file: %w", err)
		}
		// Detect MIME type from extension
		switch strings.ToLower(filepath.Ext(pathOrURL)) {
		case ".png":
			mimeType = "image/png"
		case ".jpg", ".jpeg":
			mimeType = "image/jpeg"
		case ".gif":
			mimeType = "image/gif"
		case ".webp":
			mimeType = "image/webp"
		default:
			mimeType = "application/octet-stream"
		}
	}

	// Decode to verify it's a valid image and get dimensions
	_, _, err = image.DecodeConfig(strings.NewReader(string(data)))
	if err != nil {
		return ContentBlock{}, fmt.Errorf("decode image: %w", err)
	}

	// Encode as base64 for the ContentBlock
	b64 := base64.StdEncoding.EncodeToString(data)

	return ContentBlock{
		Type:     ContentBlockImage,
		Data:     b64,
		MimeType: mimeType,
	}, nil
}
