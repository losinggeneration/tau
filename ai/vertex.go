package ai

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
)

// VertexProvider handles Google Vertex AI API.
type VertexProvider struct{}

// API implements Provider.
func (p *VertexProvider) API() string {
	return "google-vertex"
}

// IsAlias implements Provider.
func (p *VertexProvider) IsAlias() bool { return false }

// NewClient implements Provider.
func (p *VertexProvider) NewClient(model *Model) (Client, error) {
	return &vertexClient{model: model}, nil
}

type vertexClient struct {
	model *Model
}

// Complete implements Client.
func (c *vertexClient) Complete(ctx context.Context, model *Model, prompt *Context, opts ...Option) (*AssistantMessage, error) {
	return completeViaStream(ctx, c, model, prompt, opts...)
}

// Stream implements Client.Streamer.
func (c *vertexClient) Stream(ctx context.Context, model *Model, prompt *Context, handler EventHandler, opts ...Option) (err error) {
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

	url, auth, err := resolveVertexEndpoint(ctx, model, &o)
	if err != nil {
		return err
	}

	// Build request body
	body, err := buildVertexRequestBody(model, prompt, &o)
	if err != nil {
		return fmt.Errorf("vertex ai: build request body: %w", err)
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
		return fmt.Errorf("vertex ai: marshal request: %w", err)
	}

	resp, err := sendProviderRequest(ctx, &o, providerRequest{label: "vertex ai", send: func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if auth.APIKey != "" {
			req.Header.Set("x-goog-api-key", auth.APIKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+auth.BearerToken)
		}
		for k, v := range o.headers {
			req.Header.Set(k, v)
		}
		return doHeaderRequest(req, o.timeout, "vertex ai")
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

	// Stream response
	return streamVertexResponse(ctx, resp.Body, model, handler)
}

const vertexCredentialsMarker = "gcp-vertex-credentials"

var vertexVersionedPath = regexp.MustCompile(`(?:^|/)v\d+(?:beta\d*)?(?:/|$)`)

type googleCloudAuth struct {
	APIKey      string
	BearerToken string
}

// resolveVertexEndpoint follows upstream's @google/genai client setup: an API
// key uses the global express endpoint without a project, credentials need a
// project and location, and a catalog base URL without {location} is a
// collection root that receives no project prefix.
func resolveVertexEndpoint(ctx context.Context, model *Model, o *options) (string, googleCloudAuth, error) {
	modelPath := "publishers/google/models/" + model.ID + ":streamGenerateContent?alt=sse"
	baseURL := strings.TrimSpace(model.BaseURL)
	if strings.Contains(baseURL, "{location}") {
		baseURL = ""
	}
	auth := googleCloudAuth{APIKey: vertexAPIKey(o.apiKey)}
	if auth.APIKey == "" && !o.noEnvCredentials {
		auth.APIKey = cmp.Or(o.providerEnvValue("GOOGLE_CLOUD_API_KEY"), o.providerEnvValue("VERTEXAI_API_KEY"))
	}
	if auth.APIKey != "" {
		if baseURL == "" {
			baseURL = "https://aiplatform.googleapis.com"
		}
		return joinVertexURL(baseURL, modelPath), auth, nil
	}

	if o.noEnvCredentials {
		return "", auth, errors.New("vertex ai: ambient credentials disabled; supply an explicit API key")
	}

	project := cmp.Or(strings.TrimSpace(o.vertexProject), o.providerEnvValue("GOOGLE_CLOUD_PROJECT"), o.providerEnvValue("GCLOUD_PROJECT"))
	if project == "" {
		return "", auth, errors.New("vertex ai: missing project (set GOOGLE_CLOUD_PROJECT/GCLOUD_PROJECT or use WithVertexProject)")
	}
	location := cmp.Or(strings.TrimSpace(o.vertexLocation), o.providerEnvValue("GOOGLE_CLOUD_LOCATION"))
	if location == "" {
		return "", auth, errors.New("vertex ai: missing location (set GOOGLE_CLOUD_LOCATION or use WithVertexLocation)")
	}
	auth, err := getGoogleCloudCredentials(ctx, o)
	if err != nil {
		return "", auth, fmt.Errorf("vertex ai: get credentials: %w", err)
	}
	if baseURL != "" {
		return joinVertexURL(baseURL, modelPath), auth, nil
	}
	switch location {
	case "global":
		baseURL = "https://aiplatform.googleapis.com"
	case "us", "eu":
		baseURL = "https://aiplatform." + location + ".rep.googleapis.com"
	default:
		baseURL = "https://" + location + "-aiplatform.googleapis.com"
	}
	return joinVertexURL(baseURL, "projects/"+project+"/locations/"+location+"/"+modelPath), auth, nil
}

func joinVertexURL(baseURL, path string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if !vertexVersionedPath.MatchString(baseURL[strings.Index(baseURL, "//")+2:]) {
		baseURL += "/v1"
	}
	return baseURL + "/" + path
}

// vertexAPIKey ignores the credential marker and placeholder keys such as
// "<authenticated>", which only report that ambient credentials exist.
func vertexAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == vertexCredentialsMarker || (strings.HasPrefix(key, "<") && strings.HasSuffix(key, ">")) {
		return ""
	}
	return key
}

// getGoogleCloudCredentials gets a Vertex AI bearer token from
// GOOGLE_ACCESS_TOKEN or Application Default Credentials
// (GOOGLE_APPLICATION_CREDENTIALS, the gcloud ADC file, or the metadata server).
func getGoogleCloudCredentials(ctx context.Context, o *options) (googleCloudAuth, error) {
	if token := o.providerEnvValue("GOOGLE_ACCESS_TOKEN"); token != "" {
		return googleCloudAuth{BearerToken: token}, nil
	}
	// ponytail: credentials are detected per request; cache them per client if token exchanges show up in latency.
	detect := &credentials.DetectOptions{
		Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
		Client: http.DefaultClient,
	}
	var (
		creds *auth.Credentials
		err   error
	)
	if path := o.providerEnvValue("GOOGLE_APPLICATION_CREDENTIALS"); path != "" {
		creds, err = credentialsFromFile(path, detect)
	} else {
		creds, err = credentials.DetectDefault(detect)
	}
	if err != nil {
		return googleCloudAuth{}, err
	}
	token, err := creds.Token(ctx)
	if err != nil {
		return googleCloudAuth{}, err
	}
	return googleCloudAuth{BearerToken: token.Value}, nil
}

// credentialsFromFile loads a GOOGLE_APPLICATION_CREDENTIALS file, accepting
// only key-based credential types. External account configurations can run
// arbitrary executables or reach arbitrary endpoints to fetch a token, so a
// path that may come from scoped provider settings must not select one.
func credentialsFromFile(path string, opts *credentials.DetectOptions) (*auth.Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Type credentials.CredType `json:"type"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	switch file.Type {
	case credentials.ServiceAccount, credentials.AuthorizedUser:
		return credentials.NewCredentialsFromJSON(file.Type, data, opts)
	default:
		return nil, fmt.Errorf("%s: unsupported credential type %q", path, file.Type)
	}
}

// buildVertexRequestBody builds the Vertex AI request body.
// Uses shared Google helper layer for message conversion and tool handling.
func buildVertexRequestBody(model *Model, aiCtx *Context, o *options) (map[string]any, error) {
	// Transform messages using shared layer: same-model vs cross-model thinking,
	// synthetic tool results, orphaned tool call handling, and errored/aborted
	// message filtering. NormalizeToolCallIDForGoogle handles Claude/GPT-OSS ID
	// format requirements (^[a-zA-Z0-9_-]{1,64}$).
	transformedCtx := aiCtx
	if transformed, err := TransformContext(aiCtx, model, NormalizeToolCallIDForGoogle); err == nil {
		transformedCtx = transformed
	}

	contents := make([]map[string]any, 0)
	var pendingToolResultParts []map[string]any

	// Build contents using shared helper from google_shared.go
	// isSameProviderModel: Vertex is same provider family as Google Generative AI
	for _, msg := range transformedCtx.Messages {
		role, parts, skip := buildGoogleContentBlock(msg, true, model.ID)
		if parts == nil {
			// Tool result: collect and merge into next user message
			if block, ok := msg.Content.([]ContentBlock); ok {
				pendingToolResultParts = append(pendingToolResultParts, googleToolResultPart(msg, block))
			}
			_ = skip // already handled
			continue
		}

		// Merge pending tool result parts into this user message
		if len(pendingToolResultParts) > 0 {
			parts = append(parts, pendingToolResultParts...)
			pendingToolResultParts = nil
		}

		if len(parts) == 0 {
			continue
		}

		contents = append(contents, map[string]any{
			"role":  role,
			"parts": parts,
		})
	}

	// Flush any remaining tool result parts as a new user message
	if len(pendingToolResultParts) > 0 {
		contents = append(contents, map[string]any{
			"role":  "user",
			"parts": pendingToolResultParts,
		})
	}

	// Build generation config
	genConfig := map[string]any{}
	if o.temperature != nil {
		genConfig["temperature"] = *o.temperature
	}

	if o.maxTokens > 0 {
		genConfig["maxOutputTokens"] = o.maxTokens
	} else if model.MaxTokens > 0 {
		genConfig["maxOutputTokens"] = model.MaxTokens
	}

	if o.topP != nil {
		genConfig["topP"] = *o.topP
	}

	body := map[string]any{
		"contents":         contents,
		"generationConfig": genConfig,
	}

	// Add system instruction if present
	if transformedCtx.SystemPrompt != "" {
		body["systemInstruction"] = map[string]any{
			"role": "system",
			"parts": []map[string]any{
				{"text": SanitizeSurrogates(transformedCtx.SystemPrompt)},
			},
		}
	}

	// Add tools if present with full toolConfig support (P7-GG01)
	if len(transformedCtx.Tools) > 0 {
		declarations, strict, err := googleFunctionDeclarations(model, transformedCtx.Tools, googleJSONSchemaParameters)
		if err != nil {
			return nil, err
		}
		body["tools"] = []map[string]any{{"functionDeclarations": declarations}}

		// Add toolConfig for tool choice and allowed function names (P7-GG01)
		if mode := googleFunctionCallingMode(o.toolChoice, strict); mode != "" {
			functionCallingConfig := map[string]any{"mode": mode}
			// When the caller specifies a particular function (OpenAI-style
			// {"type":"function","function":{"name":"..."}} or Anthropic-style
			// {"type":"tool","name":"..."}), restrict the model to that function.
			if fns := toolChoiceFunctions(o.toolChoice); fns != nil {
				functionCallingConfig["allowedFunctionNames"] = fns
			}
			body["toolConfig"] = map[string]any{
				"functionCallingConfig": functionCallingConfig,
			}
		}
	}

	// Add thinking config for thinking-enabled models (P7-VX02, P7-VX03)
	if err := addVertexThinkingConfig(body, model, o); err != nil {
		return nil, err
	}

	return body, nil
}

// addVertexThinkingConfig adds the request's thinking control, matching
// upstream google-vertex.
func addVertexThinkingConfig(body map[string]any, model *Model, o *options) error {
	thinking, ok, err := resolveGoogleThinking(model, o)
	if err != nil || !ok {
		return err
	}
	config := map[string]any{}
	if thinking.enabled {
		config["includeThoughts"] = true
	}
	if thinking.level != "" {
		config["thinkingLevel"] = string(thinking.level)
	} else if thinking.budget != nil {
		config["thinkingBudget"] = *thinking.budget
	}
	body["thinkingConfig"] = config
	return nil
}

// streamVertexResponse streams the SSE response.
func streamVertexResponse(ctx context.Context, body io.Reader, model *Model, handler EventHandler) error {
	reader := NewSSEReader(body)

	output := &AssistantMessage{
		Role:       "assistant",
		API:        APIGoogleVertex,
		Provider:   ProviderGoogleVertex,
		Model:      "",
		Content:    []ContentBlock{},
		StopReason: StopReasonStop,
		Usage:      Usage{},
		Timestamp:  timestamp(),
	}

	handler(ctx, Start{Partial: *output})

	var (
		currentBlock    *ContentBlock
		toolCallCounter int64
		finished        bool
	)
	// Track in-progress tool calls for deduplication
	activeToolCalls := make(map[string]*struct {
		idx   int
		ended bool
	})

	for {
		data, ok := reader.Next()
		if !ok {
			if err := reader.Error(); err != nil {
				streamErr := fmt.Errorf("vertex ai: read: %w", err)
				emitTerminalStreamError(ctx, handler, *output, streamErr, StopReasonError)
				return nil
			}

			break
		}

		var chunk vertexChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if chunk.ResponseID != "" {
			output.ResponseID = chunk.ResponseID
		}

		// Process candidates
		for _, candidate := range chunk.Candidates {
			for _, part := range candidate.Content.Parts {
				if part.Text != "" {
					if currentBlock == nil || currentBlock.Type != ContentBlockText {
						if currentBlock != nil {
							endVertexBlock(ctx, handler, *output, len(output.Content)-1, *currentBlock)
						}
						output.Content = append(output.Content, ContentBlock{Type: ContentBlockText})
						currentBlock = &output.Content[len(output.Content)-1]
						handler(ctx, TextStart{ContentIndex: len(output.Content) - 1, Partial: *output})
					}

					// Capture thoughtSignature/textSignature if present on text parts (P7-GG08)
					if part.ThoughtSignature != "" {
						currentBlock.ThoughtSignature = retainThoughtSignature(currentBlock.ThoughtSignature, part.ThoughtSignature)
					}

					currentBlock.Text += part.Text
					handler(ctx, TextDelta{ContentIndex: len(output.Content) - 1, Delta: part.Text, Partial: *output})
				}

				if part.FunctionCall != nil {
					// P7-GG06: Deduplicate tool calls - only emit start/delta/end once per ID
					providedID := part.FunctionCall.ID

					// Skip if this ID is already being processed (hasn't ended yet)
					if existing, ok := activeToolCalls[providedID]; ok && !existing.ended {
						// This is a continuation of an existing tool call - emit delta
						argsJSON, err := json.Marshal(part.FunctionCall.Args)
						if err == nil {
							handler(ctx, ToolCallDelta{ContentIndex: existing.idx, Delta: string(argsJSON), Partial: *output})
						}
						// If this is the last chunk for this tool call (finishReason present), emit end
						if candidate.FinishReason != "" && existing.idx < len(output.Content) {
							tc := output.Content[existing.idx]
							handler(ctx, ToolCallEnd{ContentIndex: existing.idx, ToolCall: ToolCall{
								ID:        tc.ID,
								Name:      tc.Name,
								Arguments: tc.Arguments,
							}, Partial: *output})
							existing.ended = true
						}
						continue
					}

					if currentBlock != nil {
						endVertexBlock(ctx, handler, *output, len(output.Content)-1, *currentBlock)
						currentBlock = nil
					}

					// Generate tool call ID with dedup against existing IDs
					needsNewID := providedID == "" || func() bool {
						for _, b := range output.Content {
							if b.Type == ContentBlockToolCall && b.ID == providedID {
								return true
							}
						}
						return false
					}()

					var toolCallID string
					if needsNewID {
						toolCallCounter++
						toolCallID = fmt.Sprintf("%s_%d_%d", part.FunctionCall.Name, timestamp(), toolCallCounter)
					} else {
						toolCallID = providedID
					}

					argsJSON, err := json.Marshal(part.FunctionCall.Args)
					if err != nil {
						continue // Should never happen with map[string]any
					}
					toolCall := ToolCall{
						ID:        toolCallID,
						Name:      part.FunctionCall.Name,
						Arguments: argsJSON,
					}

					// Capture thoughtSignature if present on the function call (P7-GG07)
					var thoughtSig string
					if part.ThoughtSignature != "" {
						thoughtSig = part.ThoughtSignature
					}

					output.Content = append(output.Content, ContentBlock{
						Type:             ContentBlockToolCall,
						ID:               toolCall.ID,
						Name:             toolCall.Name,
						Arguments:        toolCall.Arguments,
						ThoughtSignature: thoughtSig,
					})
					idx := len(output.Content) - 1

					// Track this tool call as active (will be ended on next chunk)
					activeToolCalls[toolCallID] = &struct {
						idx   int
						ended bool
					}{idx: idx, ended: false}

					handler(ctx, ToolCallStart{ContentIndex: idx, Partial: *output})
					handler(ctx, ToolCallDelta{ContentIndex: idx, Delta: string(argsJSON), Partial: *output})

					// If this chunk has finishReason, this is the only/last chunk for this tool call
					if candidate.FinishReason != "" {
						handler(ctx, ToolCallEnd{ContentIndex: idx, ToolCall: toolCall, Partial: *output})
						activeToolCalls[toolCallID].ended = true
					}
				}
			}

			// Handle finish reason
			if candidate.FinishReason != "" {
				finished = true
				output.StopReason, output.ErrorMessage = googleFinish(candidate.FinishReason, output.Content)
			}
		}

		// Handle usage
		if chunk.UsageMetadata != nil {
			output.Usage = Usage{
				Input:      chunk.UsageMetadata.PromptTokenCount - chunk.UsageMetadata.CachedContentTokenCount,
				Output:     chunk.UsageMetadata.CandidatesTokenCount + chunk.UsageMetadata.ThoughtsTokenCount,
				CacheRead:  chunk.UsageMetadata.CachedContentTokenCount,
				CacheWrite: 0,
			}
			output.Usage.CalculateCost(model.Cost)
			handler(ctx, &output.Usage)
		}
	}

	// Flush remaining content
	if currentBlock != nil {
		endVertexBlock(ctx, handler, *output, len(output.Content)-1, *currentBlock)
	}

	finishStream(ctx, handler, *output, finished, "google vertex stream ended without a finish reason")

	return nil
}

func endVertexBlock(ctx context.Context, handler EventHandler, output AssistantMessage, idx int, block ContentBlock) {
	if block.Type == ContentBlockText {
		handler(ctx, TextEnd{Content: block.Text, ContentIndex: idx, Partial: output})
	}
}

// Vertex API types.
type vertexChunk struct {
	ResponseID    string            `json:"responseId,omitempty"`
	Candidates    []vertexCandidate `json:"candidates,omitempty"`
	UsageMetadata *vertexUsage      `json:"usageMetadata,omitempty"`
}

type vertexCandidate struct {
	Content      vertexContent `json:"content,omitempty"`
	FinishReason string        `json:"finishReason,omitempty"`
}

type vertexContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []vertexPart `json:"parts,omitempty"`
}

type vertexPart struct {
	Text             string              `json:"text,omitempty"`
	FunctionCall     *vertexFunctionCall `json:"functionCall,omitempty"`
	ThoughtSignature string              `json:"thoughtSignature,omitempty"`
}

type vertexFunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type vertexUsage struct {
	PromptTokenCount        int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount    int `json:"candidatesTokenCount,omitempty"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount         int `json:"totalTokenCount,omitempty"`
}
