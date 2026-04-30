package ai

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
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
	var o options
	o.apply(opts)

	// Get project and location
	project := strings.TrimSpace(o.vertexProject)
	if project == "" {
		project = os.Getenv("GOOGLE_CLOUD_PROJECT")
	}
	if project == "" {
		project = os.Getenv("GCLOUD_PROJECT")
	}
	if project == "" {
		return errors.New("vertex ai: missing project (set GOOGLE_CLOUD_PROJECT/GCLOUD_PROJECT or use WithVertexProject)")
	}

	location := strings.TrimSpace(o.vertexLocation)
	if location == "" {
		location = os.Getenv("GOOGLE_CLOUD_LOCATION")
	}
	if location == "" {
		return errors.New("vertex ai: missing location (set GOOGLE_CLOUD_LOCATION or use WithVertexLocation)")
	}

	// Get credentials
	auth, err := getGoogleCloudCredentials(ctx)
	if err != nil {
		return fmt.Errorf("vertex ai: get credentials: %w", err)
	}

	// Build URL
	modelID := c.model.ID
	if modelID == "" {
		modelID = "gemini-2.0-flash"
	}

	url := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/%s/models/%s:streamGenerateContent?alt=sse",
		location, project, location, "google", modelID)

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

	// Create request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return fmt.Errorf("vertex ai: create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if auth.APIKey != "" {
		req.Header.Set("x-goog-api-key", auth.APIKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+auth.BearerToken)
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}

	// Execute request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("vertex ai: request: %w", err)
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
			return fmt.Errorf("vertex ai: status %d", resp.StatusCode)
		}

		return fmt.Errorf("vertex ai: status %d: %s", resp.StatusCode, string(body))
	}

	// Stream response
	return streamVertexResponse(ctx, resp.Body, model, handler)
}

type googleCloudAuth struct {
	APIKey      string
	BearerToken string
}

// getGoogleCloudCredentials gets Google Cloud auth for Vertex AI.
// It prefers explicit API keys, then ADC/service-account based bearer tokens.
func getGoogleCloudCredentials(ctx context.Context) (googleCloudAuth, error) {
	// First try environment variable
	if token := os.Getenv("GOOGLE_ACCESS_TOKEN"); token != "" {
		return googleCloudAuth{BearerToken: token}, nil
	}

	if apiKey := os.Getenv("GOOGLE_CLOUD_API_KEY"); apiKey != "" {
		return googleCloudAuth{APIKey: apiKey}, nil
	}

	// Check for VERTEXAI_API_KEY (legacy explicit API key for Vertex AI)
	if apiKey := os.Getenv("VERTEXAI_API_KEY"); apiKey != "" {
		return googleCloudAuth{APIKey: apiKey}, nil
	}

	// Try to read from credentials file
	credsFile := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if credsFile != "" {
		token, err := getTokenFromServiceAccount(ctx, credsFile)
		if err != nil {
			return googleCloudAuth{}, err
		}
		return googleCloudAuth{BearerToken: token}, nil
	}

	// Try gcloud CLI
	token, err := getTokenFromGcloud(ctx)
	if err == nil && token != "" {
		return googleCloudAuth{BearerToken: token}, nil
	}

	// Try metadata server (for GCE, Cloud Run, etc.)
	token, err = getTokenFromMetadata(ctx)
	if err == nil {
		return googleCloudAuth{BearerToken: token}, nil
	}

	// Return error with helpful message
	return googleCloudAuth{}, fmt.Errorf("vertex ai: no valid credentials found: %w", err)
}

// getTokenFromServiceAccount reads a service account JSON file and obtains an access token.
func getTokenFromServiceAccount(ctx context.Context, credsFile string) (string, error) {
	data, err := os.ReadFile(credsFile)
	if err != nil {
		return "", fmt.Errorf("read credentials file: %w", err)
	}

	var creds struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURL    string `json:"token_uri"`
	}

	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("parse credentials: %w", err)
	}

	token, err := getAccessTokenFromTokenURI(ctx, creds.TokenURL, creds.ClientEmail, creds.PrivateKey)
	if err != nil {
		return "", err
	}

	return token, nil
}

// getAccessTokenFromTokenURI gets an access token from the service-account token URI.
func getAccessTokenFromTokenURI(ctx context.Context, tokenURI, clientEmail, privateKey string) (string, error) {
	if tokenURI == "" || clientEmail == "" || privateKey == "" {
		return "", errors.New("vertex ai: incomplete service account credentials")
	}

	keyBlock, err := base64URLDecodePEMKey(privateKey)
	if err != nil {
		return "", fmt.Errorf("vertex ai: parse service account key: %w", err)
	}

	assertion, err := buildServiceAccountAssertion(clientEmail, tokenURI, keyBlock)
	if err != nil {
		return "", fmt.Errorf("vertex ai: build service account assertion: %w", err)
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vertex ai: token exchange status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", err
	}
	if tokenResp.AccessToken == "" {
		return "", errors.New("vertex ai: token exchange returned empty access token")
	}

	return tokenResp.AccessToken, nil
}

func base64URLDecodePEMKey(privateKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privateKey))
	if block == nil {
		return nil, errors.New("invalid PEM block")
	}

	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("private key is not RSA")
		}
		return rsaKey, nil
	}

	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func buildServiceAccountAssertion(clientEmail, tokenURI string, key *rsa.PrivateKey) (string, error) {
	headerJSON, err := json.Marshal(map[string]string{
		"alg": "RS256",
		"typ": "JWT",
	})
	if err != nil {
		return "", err
	}

	now := time.Now()
	claimsJSON, err := json.Marshal(map[string]any{
		"iss":   clientEmail,
		"scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud":   tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}

	encodedHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	encodedClaims := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := encodedHeader + "." + encodedClaims

	hash := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// getTokenFromGcloud tries to get a token using the gcloud CLI.
// Returns the access token if gcloud is available and configured.
func getTokenFromGcloud(ctx context.Context) (string, error) {
	// Check if gcloud is available
	_, err := exec.LookPath("gcloud")
	if err != nil {
		return "", errors.New("gcloud not found in PATH")
	}

	// Try to get access token via gcloud
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "print-access-token")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gcloud auth failed: %w", err)
	}

	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", errors.New("gcloud returned empty token")
	}

	return token, nil
}

// getTokenFromMetadata gets a token from the GCP metadata server.
func getTokenFromMetadata(ctx context.Context) (token string, _ error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token",
		nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata server: %w", err)
	}
	var closeErr error
	defer func() {
		if err := resp.Body.Close(); err != nil {
			if closeErr == nil {
				closeErr = err
			}
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata server status: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}

	if err := json.Unmarshal(data, &tokenResp); err != nil {
		return "", err
	}

	if closeErr != nil {
		return "", closeErr
	}

	return tokenResp.AccessToken, nil
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
		tools := buildVertexTools(transformedCtx.Tools)
		body["tools"] = tools

		// Add toolConfig for tool choice and allowed function names (P7-GG01)
		if mode := toolChoiceMode(o.toolChoice); mode != "" {
			functionCallingConfig := map[string]any{
				"mode": mapToolChoice(o.toolChoice),
			}
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

// addVertexThinkingConfig adds thinking configuration for Vertex AI.
// Matches pi-mono's google-vertex.ts behavior for thinking levels and budgets.
// P7-VX02: Build a ThinkingConfig with the full enum mapping.
// P7-VX03: Disabled ThinkingConfig for non-reasoning models.
func addVertexThinkingConfig(body map[string]any, model *Model, o *options) error {
	// Add thinking config if explicitly enabled with budget, or if budget tokens > 0
	// (budget tokens > 0 indicates thinking should be enabled even if thinkingEnabled is not set)
	if (o.thinkingEnabled || o.thinkingBudgetTokens > 0) && model.Reasoning {
		if o.googleThinkingLevel != "" {
			body["thinkingConfig"] = map[string]any{
				"thinkingLevel":   string(o.googleThinkingLevel),
				"includeThoughts": true,
			}
		} else if isGemini3ProModel(model.ID) {
			// Gemini 3 Pro: cannot use thinkingBudget, use thinkingLevel LOW instead
			body["thinkingConfig"] = map[string]any{
				"thinkingLevel":   "LOW",
				"includeThoughts": true,
			}
		} else if isGemini3FlashModel(model.ID) {
			// Gemini 3 Flash: use MINIMAL level
			body["thinkingConfig"] = map[string]any{
				"thinkingLevel":   "MINIMAL",
				"includeThoughts": true,
			}
		} else if isGemma4Model(model.ID) {
			// Gemma 4: use MINIMAL level
			body["thinkingConfig"] = map[string]any{
				"thinkingLevel":   "MINIMAL",
				"includeThoughts": true,
			}
		} else {
			// Gemini 2.x: use thinkingBudget
			body["thinkingConfig"] = map[string]any{
				"thinkingBudget":  o.thinkingBudgetTokens,
				"includeThoughts": true,
			}
		}
		return nil
	}

	// If thinking is explicitly disabled (budget = 0) but model has reasoning
	if !o.thinkingEnabled && o.thinkingBudgetTokens == 0 && model.Reasoning {
		// For models that cannot fully disable thinking, use lowest level
		if isGemini3ProModel(model.ID) {
			// Gemini 3 Pro: cannot disable thinking, use LOW level
			body["thinkingConfig"] = map[string]any{
				"thinkingLevel": "LOW",
			}
		} else if isGemma4Model(model.ID) || isGemini3FlashModel(model.ID) {
			// Gemma 4 / Gemini 3 Flash: use MINIMAL
			body["thinkingConfig"] = map[string]any{
				"thinkingLevel": "MINIMAL",
			}
		} else {
			// Gemini 2.x: supports disabling via thinkingBudget = 0
			body["thinkingConfig"] = map[string]any{
				"thinkingBudget": 0,
			}
		}
		return nil
	}

	// Default: if model has reasoning but no explicit option, don't force thinking config
	// This lets the model's default behavior apply
	return nil
}

// buildVertexTools converts tools to Vertex format.
func buildVertexTools(tools []Tool) []map[string]any {
	result := make([]map[string]any, len(tools))
	for i, tool := range tools {
		result[i] = map[string]any{
			"functionDeclarations": []map[string]any{
				{
					"name":        tool.Name,
					"description": tool.Description,
					"parameters":  tool.Parameters,
				},
			},
		}
	}

	return result
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

	var currentBlock *ContentBlock
	var toolCallCounter int64
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
					output.StopReason = StopReasonToolUse

					// If this chunk has finishReason, this is the only/last chunk for this tool call
					if candidate.FinishReason != "" {
						handler(ctx, ToolCallEnd{ContentIndex: idx, ToolCall: toolCall, Partial: *output})
						activeToolCalls[toolCallID].ended = true
					}
				}
			}

			// Handle finish reason
			if candidate.FinishReason != "" && output.StopReason != StopReasonToolUse {
				output.StopReason = googleStopReason(candidate.FinishReason)
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

	handler(ctx, Done{Reason: output.StopReason, Message: *output})

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
