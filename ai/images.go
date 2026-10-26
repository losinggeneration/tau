package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ImagesResult is the output of one image generation request.
type ImagesResult struct {
	API          string
	Provider     string
	Model        string
	ResponseID   string
	Output       []ContentBlock
	Usage        Usage
	StopReason   StopReason
	ErrorMessage string
}

var dataURLPattern = regexp.MustCompile(`^data:([^;]+);base64,(.+)$`)

// GenerateImages sends image models to their operation-specific API. Once the
// model is an image model, failures return both the error and a result whose
// stop reason and message describe it.
func GenerateImages(ctx context.Context, model *Model, input []ContentBlock, opts ...Option) (result *ImagesResult, err error) {
	if model == nil || model.Type != "image" || model.API != APIOpenRouterImages {
		return nil, fmt.Errorf("unsupported image model")
	}
	result = &ImagesResult{API: model.API, Provider: model.Provider, Model: model.ID, StopReason: StopReasonStop}
	defer func() {
		if err != nil {
			result.StopReason = StopReasonError
			if ctx.Err() != nil {
				result.StopReason = StopReasonAborted
			}
			result.ErrorMessage = err.Error()
		}
	}()
	resolvedOptions, resolveErr := resolveRequestOptions(ctx, model, opts)
	if resolveErr != nil {
		return result, resolveErr
	}
	opts = resolvedOptions
	cfg := applyOptions(opts)
	key := cfg.apiKey
	if key == "" {
		key = cfg.envAPIKey(model.Provider)
	}
	if key == "" {
		return result, fmt.Errorf("no API key for provider: %s", model.Provider)
	}
	content := make([]map[string]any, 0, len(input))
	for _, block := range input {
		if block.Type == ContentBlockImage {
			content = append(content, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:" + block.MimeType + ";base64," + block.Data}})
			continue
		}
		content = append(content, map[string]any{"type": "text", "text": SanitizeSurrogates(block.Text)})
	}
	modalities := []string{"image"}
	if slices.Contains(model.Output, OutputText) {
		modalities = append(modalities, "text")
	}
	var payload any = map[string]any{
		"model":      model.ID,
		"messages":   []map[string]any{{"role": "user", "content": content}},
		"stream":     false,
		"modalities": modalities,
	}
	if hasPayloadCallback(cfg) {
		if replacement := invokePayloadCallback(cfg, model, payload); replacement != nil {
			payload = replacement
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return result, fmt.Errorf("marshal images request: %w", err)
	}
	baseURL, err := expandBaseURLWithEnv(strings.TrimRight(model.BaseURL, "/"), cfg.providerEnvValue)
	if err != nil {
		return result, err
	}
	data, err := postOperation(ctx, "images", model, cfg, baseURL+chatCompletionsPath, key, body)
	if err != nil {
		return result, err
	}
	var response struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content any `json:"content"`
				Images  []struct {
					ImageURL json.RawMessage `json:"image_url"`
				} `json:"images"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return result, fmt.Errorf("decode images response: %w", err)
	}
	result.ResponseID = response.ID
	if u := response.Usage; u != nil {
		cacheWrite := u.PromptTokensDetails.CacheWriteTokens
		cacheRead := u.PromptTokensDetails.CachedTokens
		if cacheWrite > 0 {
			cacheRead = max(0, cacheRead-cacheWrite)
		}
		result.Usage = Usage{
			Input:      max(0, u.PromptTokens-cacheRead-cacheWrite),
			Output:     u.CompletionTokens,
			CacheRead:  cacheRead,
			CacheWrite: cacheWrite,
		}
		result.Usage.TotalTokens = result.Usage.Input + result.Usage.Output + cacheRead + cacheWrite
		result.Usage.CalculateCost(model.Cost)
	}
	if len(response.Choices) == 0 {
		return result, nil
	}
	message := response.Choices[0].Message
	if text, ok := message.Content.(string); ok && text != "" {
		result.Output = append(result.Output, ContentBlock{Type: ContentBlockText, Text: text})
	}
	for _, image := range message.Images {
		var url string
		if json.Unmarshal(image.ImageURL, &url) != nil {
			var object struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(image.ImageURL, &object) != nil {
				continue
			}
			url = object.URL
		}
		if match := dataURLPattern.FindStringSubmatch(url); match != nil {
			result.Output = append(result.Output, ContentBlock{Type: ContentBlockImage, MimeType: match[1], Data: match[2]})
		}
	}
	return result, nil
}
