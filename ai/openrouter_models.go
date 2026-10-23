package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PersonalizeOpenRouterCatalog returns a key-scoped catalog without mutating shared models.
func PersonalizeOpenRouterCatalog(ctx context.Context, client *http.Client, catalog []*Model, baseURL, key string) (models []*Model, err error) {
	if key == "" {
		return catalog, nil
	}
	if baseURL == "" {
		baseURL = DefaultBaseURLs[ProviderOpenRouter]
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models/user", nil)
	if err != nil {
		return nil, fmt.Errorf("openrouter models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter models: %w", err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	switch resp.StatusCode {
	case 401, 403, 404:
		return catalog, nil
	case 200:
	default:
		return nil, fmt.Errorf("openrouter models status %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
			TopProvider   struct {
				ContextLength       int `json:"context_length"`
				MaxCompletionTokens int `json:"max_completion_tokens"`
			} `json:"top_provider"`
			Pricing *openRouterPricing `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode openrouter models: %w", err)
	}
	if payload.Data == nil {
		return nil, fmt.Errorf("invalid openrouter model list")
	}
	models = make([]*Model, 0, len(payload.Data))
	for _, model := range catalog {
		if model.Type != "" && model.Type != "chat" {
			models = append(models, model)
			continue
		}
		for _, entry := range payload.Data {
			if model.ID != entry.ID {
				continue
			}
			clone := *model
			// OpenRouter reports a missing limit as absent, null or 0.
			if entry.ContextLength > 0 {
				clone.ContextWindow = entry.ContextLength
			}
			if entry.TopProvider.ContextLength > 0 {
				clone.ContextWindow = entry.TopProvider.ContextLength
			}
			if entry.TopProvider.MaxCompletionTokens > 0 {
				clone.MaxTokens = entry.TopProvider.MaxCompletionTokens
			}
			if entry.Pricing != nil {
				clone.Cost = entry.Pricing.cost()
			}
			models = append(models, &clone)
			break
		}
	}
	return models, nil
}

type openRouterRates struct {
	Prompt          string `json:"prompt"`
	Completion      string `json:"completion"`
	InputCacheRead  string `json:"input_cache_read"`
	InputCacheWrite string `json:"input_cache_write"`
}

type openRouterPricing struct {
	openRouterRates
	Overrides []struct {
		openRouterRates
		MinPromptTokens *int     `json:"min_prompt_tokens"`
		UTCStart        *float64 `json:"utc_start"`
		UTCEnd          *float64 `json:"utc_end"`
		UTCDays         []string `json:"utc_days"`
	} `json:"overrides"`
}

// cost converts $/token prices to $/1M tokens. Prompt-length overrides become
// request-wide tiers; time-of-day overrides are skipped because CostProfile
// cannot express them.
func (p *openRouterPricing) cost() CostProfile {
	cost := CostProfile{
		Input:      openRouterPerMillion(p.Prompt, 0),
		Output:     openRouterPerMillion(p.Completion, 0),
		CacheRead:  openRouterPerMillion(p.InputCacheRead, 0),
		CacheWrite: openRouterPerMillion(p.InputCacheWrite, 0),
	}
	for _, override := range p.Overrides {
		if override.MinPromptTokens == nil || override.UTCStart != nil || override.UTCEnd != nil || override.UTCDays != nil {
			continue
		}
		cost.Tiers = append(cost.Tiers, CostTier{
			InputTokensAbove: *override.MinPromptTokens,
			Input:            openRouterPerMillion(override.Prompt, cost.Input),
			Output:           openRouterPerMillion(override.Completion, cost.Output),
			CacheRead:        openRouterPerMillion(override.InputCacheRead, cost.CacheRead),
			CacheWrite:       openRouterPerMillion(override.InputCacheWrite, cost.CacheWrite),
		})
	}
	return cost
}

// openRouterPerMillion converts one price, using fallback for missing,
// invalid and negative values. OpenRouter reports -1 for router models whose
// price depends on the model they pick.
func openRouterPerMillion(value string, fallback float64) float64 {
	perToken, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(perToken) || math.IsInf(perToken, 0) || perToken < 0 {
		return fallback
	}
	return math.Round(perToken*1e12) / 1e6
}
