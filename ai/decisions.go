package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"strings"
)

// ClassifierQuestion describes a choice, score, or boolean question.
type ClassifierQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria"`
}

// ClassifierContext contains state and named questions.
type ClassifierContext struct {
	State     map[string]any
	Images    []ContentBlock
	Questions map[string]ClassifierQuestion
}

// ClassifierResult retains billed usage even when answer parsing fails.
type ClassifierResult struct {
	API          string
	Provider     string
	Model        string
	Answers      map[string]json.RawMessage
	Usage        Usage
	StopReason   StopReason
	ErrorMessage string
}

// Classify sends classifier models to their operation-specific API. Once the
// model is a classifier, failures return both the error and a result whose
// stop reason and message describe it.
func Classify(ctx context.Context, model *Model, input ClassifierContext, opts ...Option) (result *ClassifierResult, err error) {
	defer func() {
		if err != nil && result == nil && model != nil && model.Type == "classifier" {
			result = &ClassifierResult{API: model.API, Provider: model.Provider, Model: model.ID}
		}
		if err != nil && result != nil {
			result.Answers = map[string]json.RawMessage{}
			result.StopReason = StopReasonError
			result.ErrorMessage = err.Error()
			if ctx.Err() != nil {
				result.StopReason = StopReasonAborted
			}
		}
	}()
	if model == nil || model.Type != "classifier" {
		return nil, fmt.Errorf("unsupported classifier model")
	}
	if len(input.Images) > 0 && !model.SupportsInput(InputImage) {
		return nil, fmt.Errorf("classifier model does not accept images")
	}
	if len(input.Questions) == 0 {
		return nil, fmt.Errorf("classifier questions are required")
	}
	resolvedOptions, resolveErr := resolveRequestOptions(ctx, model, opts)
	if resolveErr != nil {
		return nil, resolveErr
	}
	opts = resolvedOptions
	cfg := applyOptions(opts)
	key := cfg.apiKey
	if key == "" {
		key = cfg.envAPIKey(model.Provider)
	}
	if key == "" && model.Provider == ProviderLlamaCpp {
		// llama-server accepts any key unless started with --api-key.
		key = "local"
	}
	switch model.API {
	case APIOpenAIDecisions:
		return classifyDecisions(ctx, model, input, cfg, key)
	case APITypesafeSystemOne, APICloudflareWorkersAISystemOne:
		return classifySystemOne(ctx, model, input, cfg, key)
	case APILlamaCppClassify:
		return classifyLlamaCpp(ctx, model, input, cfg, key)
	default:
		return nil, fmt.Errorf("unsupported classifier API %q", model.API)
	}
}

func classifyDecisions(ctx context.Context, model *Model, input ClassifierContext, cfg *options, key string) (*ClassifierResult, error) {
	if len(input.Images) > 128 {
		return nil, fmt.Errorf("decisions accepts at most 128 images")
	}
	if key == "" {
		return nil, fmt.Errorf("decisions requires an API key")
	}
	if strings.Count(key, ".") == 2 {
		return nil, fmt.Errorf("decisions requires an API key, not a ChatGPT token")
	}
	var questions []map[string]any
	names := make([]string, 0, len(input.Questions))
	for name := range input.Questions {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		question := input.Questions[name]
		wire := map[string]any{"name": name, "type": question.Type, "instructions": question.Instructions}
		switch question.Type {
		case "choice":
			criteria, ok := question.Criteria.(map[string]string)
			if !ok || len(criteria) == 0 {
				return nil, fmt.Errorf("invalid choice criteria for %s", name)
			}
			var choices []map[string]string
			for value, description := range criteria {
				choice := map[string]string{"value": value}
				if description != "" {
					choice["description"] = description
				}
				choices = append(choices, choice)
			}
			wire["choices"] = choices
		case "score":
			criteria, ok := question.Criteria.([]string)
			if !ok || len(criteria) == 0 {
				return nil, fmt.Errorf("invalid score criteria for %s", name)
			}
			var levels []map[string]string
			for _, label := range criteria {
				levels = append(levels, map[string]string{"label": label})
			}
			wire["levels"] = levels
		case "bool":
			criteria, ok := question.Criteria.(map[string]string)
			if !ok {
				return nil, fmt.Errorf("invalid boolean criteria for %s", name)
			}
			wire["type"] = "predicate"
			meanings := []string{}
			if criteria["true"] != "" {
				meanings = append(meanings, "True means: "+criteria["true"])
			}
			if criteria["false"] != "" {
				meanings = append(meanings, "False means: "+criteria["false"])
			}
			if len(meanings) > 0 {
				wire["instructions"] = question.Instructions + "\n\n" + strings.Join(meanings, "\n")
			}
		default:
			return nil, fmt.Errorf("invalid classifier question type %q", question.Type)
		}
		questions = append(questions, wire)
	}
	state, err := json.Marshal(input.State)
	if err != nil {
		return nil, fmt.Errorf("marshal classifier state: %w", err)
	}
	blocks := append([]ContentBlock{{Type: ContentBlockText, Text: string(state)}}, input.Images...)
	if err := ValidateContextLimits(model, &Context{Messages: []Message{{Role: "user", Content: blocks}}}); err != nil {
		return nil, fmt.Errorf("validate classifier input: %w", err)
	}
	var wireInput any = string(state)
	if len(input.Images) > 0 {
		content := []map[string]any{{"type": "input_text", "text": string(state)}}
		for _, image := range input.Images {
			content = append(content, map[string]any{"type": "input_image", "image_url": "data:" + image.MimeType + ";base64," + image.Data})
		}
		wireInput = []map[string]any{{"role": "user", "content": content}}
	}
	payload := map[string]any{"model": model.ID, "input": wireInput, "questions": questions}
	if hasPayloadCallback(cfg) {
		if replacement, ok := invokePayloadCallback(cfg, model, payload).(map[string]any); ok {
			payload = replacement
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal decisions request: %w", err)
	}
	baseURL := model.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURLs[ProviderOpenAI]
	}
	data, err := postOperation(ctx, "decisions", model, cfg, strings.TrimRight(baseURL, "/")+"/decisions", key, body, http.StatusGatewayTimeout)
	if errors.Is(err, errGatewayTimeout) {
		return nil, fmt.Errorf("%w: the request timed out at the gateway; very large inputs (above roughly 600K tokens) currently exceed its time limit", err)
	}
	if err != nil {
		return nil, err
	}
	var response struct {
		Answers []json.RawMessage `json:"answers"`
		Usage   json.RawMessage   `json:"usage"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("decode decisions response: %w", err)
	}
	result := &ClassifierResult{API: model.API, Provider: model.Provider, Model: model.ID, Answers: make(map[string]json.RawMessage), StopReason: StopReasonStop, Usage: decisionsUsage(response.Usage)}
	result.Usage.TotalTokens = result.Usage.Input + result.Usage.Output
	result.Usage.CalculateCost(model.Cost)
	byName := make(map[string]map[string]any, len(response.Answers))
	for _, raw := range response.Answers {
		var answer map[string]any
		if err := json.Unmarshal(raw, &answer); err != nil {
			continue
		}
		if name, ok := answer["name"].(string); ok {
			byName[name] = answer
		}
	}
	for _, name := range names {
		question := input.Questions[name]
		answer, exists := byName[name]
		if !exists {
			return result, fmt.Errorf("missing classifier answer for %s", name)
		}
		expected := question.Type
		if expected == "bool" {
			expected = "predicate"
		}
		if answer["type"] == "refusal" {
			return result, fmt.Errorf("decisions refused to answer %s", name)
		}
		if answer["type"] != expected {
			return result, fmt.Errorf("invalid answer type for %s", name)
		}
		// Like upstream, answers need finite numbers but are not range-checked,
		// and the returned choice is passed through as the model gave it.
		finite := func(field string) (float64, error) {
			value, ok := answer[field].(float64)
			if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
				return 0, fmt.Errorf("decisions returned an invalid %s for %s", field, name)
			}
			return value, nil
		}
		normalized := map[string]any{"type": question.Type}
		switch expected {
		case "choice":
			choice, ok := answer["choice"].(string)
			if !ok {
				return result, fmt.Errorf("decisions did not return a choice answer for %s", name)
			}
			probabilities, ok := answer["probabilities"].([]any)
			if !ok {
				return result, fmt.Errorf("decisions returned invalid probabilities for %s", name)
			}
			values := make(map[string]float64, len(probabilities))
			for _, item := range probabilities {
				probability, ok := item.(map[string]any)
				label, labeled := probability["value"].(string)
				if !ok || !labeled {
					return result, fmt.Errorf("decisions returned invalid probabilities for %s", name)
				}
				value, ok := probability["probability"].(float64)
				if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
					return result, fmt.Errorf("decisions returned an invalid probability for %s.%s", name, label)
				}
				values[label] = value
			}
			confidence, err := finite("confidence")
			if err != nil {
				return result, err
			}
			normalized["choice"], normalized["probabilities"], normalized["confidence"] = choice, values, confidence
		case "score":
			score, err := finite("score")
			if err != nil {
				return result, err
			}
			confidence, err := finite("confidence")
			if err != nil {
				return result, err
			}
			normalized["score"], normalized["confidence"] = score, confidence
		default:
			probability, err := finite("probability")
			if err != nil {
				return result, err
			}
			normalized["probability"] = probability
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return result, fmt.Errorf("encode classifier answer %s: %w", name, err)
		}
		result.Answers[name] = encoded
	}
	if len(result.Answers) != len(input.Questions) {
		return result, fmt.Errorf("missing classifier answers")
	}
	return result, nil
}

var (
	errGatewayTimeout = errors.New("gateway timed out")
	errOperationClose = errors.New("close response body")
)

// postOperation posts one non-streaming operation request with provider retries. Statuses
// in noRetry fail at once although they are normally retried.
func postOperation(ctx context.Context, label string, model *Model, cfg *options, url, key string, body []byte, noRetry ...int) ([]byte, error) {
	retries := 2
	if cfg.maxRetries != nil {
		retries = *cfg.maxRetries
	}
	for attempt := 0; ; attempt++ {
		data, status, headers, err := operationAttempt(ctx, label, model, cfg, url, key, body)
		if err == nil {
			return data, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s request: %w", label, errors.Join(err, ctx.Err()))
		}
		if errors.Is(err, errOperationClose) || slices.Contains(noRetry, status) {
			return nil, err
		}
		retry, waitErr := waitProviderRetry(ctx, label, cfg, attempt, retries, status, headers, providerBackoff(attempt), err)
		if waitErr != nil {
			return nil, waitErr
		}
		if !retry {
			return nil, err
		}
	}
}

func operationAttempt(ctx context.Context, label string, model *Model, cfg *options, url, key string, body []byte) (data []byte, status int, headers http.Header, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("create %s request: %w", label, err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for name, value := range model.Headers {
		req.Header.Set(name, value)
	}
	for name, value := range cfg.headers {
		req.Header.Set(name, value)
	}
	resp, err := doHeaderRequest(req, cfg.timeout, label)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("%s request: %w", label, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("%s: %w: %w", label, errOperationClose, closeErr))
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if err != nil {
			return nil, resp.StatusCode, resp.Header, fmt.Errorf("read %s error: %w", label, err)
		}
		if resp.StatusCode == http.StatusGatewayTimeout {
			return nil, resp.StatusCode, resp.Header, fmt.Errorf("%s status 504: %w", label, errGatewayTimeout)
		}
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("%s status %d: %s", label, resp.StatusCode, data)
	}
	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read %s response: %w", label, err)
	}
	if hasResponseCallback(cfg) {
		invokeResponseCallback(cfg, model, responseMetadata(resp))
	}
	return data, resp.StatusCode, resp.Header, nil
}

func decisionsUsage(raw json.RawMessage) Usage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Usage{}
	}
	count := func(name string) int {
		var value int
		if err := json.Unmarshal(fields[name], &value); err != nil {
			return 0
		}
		return max(0, value)
	}
	return Usage{Input: count("input_tokens"), Output: count("output_tokens")}
}

// classifySystemOne runs TypeSafe's System One protocol. OpenRouter and
// OpenCode serve it natively; Cloudflare Workers AI wraps it in its REST
// envelope.
func classifySystemOne(ctx context.Context, model *Model, input ClassifierContext, cfg *options, key string) (*ClassifierResult, error) {
	label := "System One API"
	if model.API == APICloudflareWorkersAISystemOne {
		label = "Cloudflare Workers AI"
	}
	if len(input.Images) > 0 {
		return nil, fmt.Errorf("%s does not support image input", label)
	}
	if key == "" {
		return nil, fmt.Errorf("no API key for provider: %s", model.Provider)
	}
	questions := make(map[string]any, len(input.Questions))
	for id, question := range input.Questions {
		kind := question.Type
		if kind == "bool" {
			kind = "noul"
		}
		questions[id] = map[string]any{"type": kind, "instructions": question.Instructions, "criteria": question.Criteria}
	}
	request := map[string]any{"state": input.State, "questions": questions}
	payload := map[string]any{"model": model.ID}
	baseURL, err := expandBaseURLWithEnv(strings.TrimRight(model.BaseURL, "/"), cfg.providerEnvValue)
	if err != nil {
		return nil, err
	}
	url := baseURL + "/systemone"
	if model.API == APICloudflareWorkersAISystemOne {
		url = baseURL + "/run"
		payload["input"] = request
	} else {
		maps.Copy(payload, request)
	}
	var wire any = payload
	if hasPayloadCallback(cfg) {
		if replacement := invokePayloadCallback(cfg, model, payload); replacement != nil {
			wire = replacement
		}
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", label, err)
	}
	data, err := postOperation(ctx, label, model, cfg, url, key, body)
	if err != nil {
		return nil, err
	}
	output, err := systemOneOutput(label, model.API, data)
	if err != nil {
		return nil, err
	}
	result := &ClassifierResult{API: model.API, Provider: model.Provider, Model: model.ID, Answers: make(map[string]json.RawMessage), StopReason: StopReasonStop, Usage: decisionsUsage(output.Usage)}
	result.Usage.TotalTokens = result.Usage.Input + result.Usage.Output
	result.Usage.CalculateCost(model.Cost)
	for id, question := range input.Questions {
		var answer struct {
			Type          string             `json:"type"`
			Choice        *string            `json:"choice"`
			Probabilities map[string]float64 `json:"probabilities"`
			Confidence    *float64           `json:"confidence"`
			Score         *float64           `json:"score"`
			Noul          *float64           `json:"noul"`
		}
		raw, ok := output.Answers[id]
		if !ok || json.Unmarshal(raw, &answer) != nil {
			return result, fmt.Errorf("%s did not return an answer for %s", label, id)
		}
		var normalized map[string]any
		switch question.Type {
		case "choice":
			if answer.Type != "choice" || answer.Choice == nil || answer.Probabilities == nil || answer.Confidence == nil {
				return result, fmt.Errorf("%s did not return a choice answer for %s", label, id)
			}
			normalized = map[string]any{"type": "choice", "choice": *answer.Choice, "probabilities": answer.Probabilities, "confidence": *answer.Confidence}
		case "score":
			if answer.Type != "score" || answer.Score == nil || answer.Confidence == nil {
				return result, fmt.Errorf("%s did not return a score answer for %s", label, id)
			}
			normalized = map[string]any{"type": "score", "score": *answer.Score, "confidence": *answer.Confidence}
		default:
			if answer.Type != "noul" || answer.Noul == nil {
				return result, fmt.Errorf("%s did not return a bool answer for %s", label, id)
			}
			normalized = map[string]any{"type": "bool", "probability": *answer.Noul}
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return result, fmt.Errorf("encode classifier answer %s: %w", id, err)
		}
		result.Answers[id] = encoded
	}
	return result, nil
}

type systemOneResult struct {
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   json.RawMessage            `json:"usage"`
}

// systemOneOutput unwraps the System One output. Cloudflare-hosted models
// return it as the envelope result; third-party Workers AI models nest it in a
// completed run record.
func systemOneOutput(label, api string, data []byte) (*systemOneResult, error) {
	unexpected := fmt.Errorf("%s returned an unexpected response", label)
	if api != APICloudflareWorkersAISystemOne {
		var output systemOneResult
		if err := json.Unmarshal(data, &output); err != nil {
			return nil, fmt.Errorf("%w: %w", unexpected, err)
		}
		return &output, nil
	}
	var envelope struct {
		Success *bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Result *struct {
			systemOneResult
			State  any              `json:"state"`
			Result *systemOneResult `json:"result"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("%w: %w", unexpected, err)
	}
	if envelope.Success != nil && !*envelope.Success {
		var messages []string
		for _, e := range envelope.Errors {
			if e.Message != "" {
				messages = append(messages, e.Message)
			}
		}
		if len(messages) == 0 {
			return nil, fmt.Errorf("%s request failed", label)
		}
		return nil, fmt.Errorf("%s error: %s", label, strings.Join(messages, "; "))
	}
	switch {
	case envelope.Result == nil:
		return nil, unexpected
	case envelope.Result.Answers != nil:
		return &envelope.Result.systemOneResult, nil
	case envelope.Result.State != "Completed":
		return nil, fmt.Errorf("%s run did not complete (state: %v)", label, envelope.Result.State)
	case envelope.Result.Result == nil:
		return nil, unexpected
	}
	return envelope.Result.Result, nil
}
