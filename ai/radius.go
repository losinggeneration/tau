package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// DefaultRadiusGateway is the hosted Radius gateway.
const DefaultRadiusGateway = "https://radius.pi.dev"

var schemePattern = regexp.MustCompile(`(?i)^https?://`)

// RadiusGateway normalizes a gateway or model base URL to the gateway root.
func RadiusGateway(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultRadiusGateway
	}
	if !schemePattern.MatchString(value) {
		value = "https://" + value
	}
	value = strings.TrimRight(value, "/")
	return strings.TrimRight(strings.TrimSuffix(value, "/v1"), "/")
}

// FetchRadiusCatalog loads the account's gateway catalog. Radius org owners
// can disable models, so callers replace the shipped Radius entries with the
// result instead of overlaying it.
func FetchRadiusCatalog(ctx context.Context, client *http.Client, gateway, key string) (models []*Model, err error) {
	gateway = RadiusGateway(gateway)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway+"/v1/config", nil)
	if err != nil {
		return nil, fmt.Errorf("radius config request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("load radius config from %s: %w", gateway, err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 512))
		if readErr != nil {
			return nil, fmt.Errorf("read radius config error: %w", readErr)
		}
		return nil, fmt.Errorf("load radius config from %s: %d: %s", gateway, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var config struct {
		BaseURL *string           `json:"baseUrl"`
		Models  []json.RawMessage `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil || config.BaseURL == nil || config.Models == nil {
		return nil, errors.Join(fmt.Errorf("invalid radius config from %s", gateway), err)
	}
	models = make([]*Model, 0, len(config.Models))
	for _, raw := range config.Models {
		var entry struct {
			ID               *string            `json:"id"`
			Name             *string            `json:"name"`
			Reasoning        *bool              `json:"reasoning"`
			ThinkingLevelMap map[string]*string `json:"thinkingLevelMap"`
			Input            []InputType        `json:"input"`
			Cost             *CostProfile       `json:"cost"`
			ContextWindow    *int               `json:"contextWindow"`
			MaxTokens        *int               `json:"maxTokens"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.ID == nil || entry.Name == nil || entry.Reasoning == nil || entry.Input == nil || entry.Cost == nil || entry.ContextWindow == nil || entry.MaxTokens == nil {
			continue
		}
		models = append(models, &Model{
			Type:             "chat",
			ID:               *entry.ID,
			Name:             *entry.Name,
			API:              APIPiMessages,
			Provider:         ProviderRadius,
			BaseURL:          *config.BaseURL,
			Reasoning:        *entry.Reasoning,
			ThinkingLevelMap: entry.ThinkingLevelMap,
			Input:            entry.Input,
			Output:           []OutputType{OutputText},
			Cost:             *entry.Cost,
			ContextWindow:    *entry.ContextWindow,
			MaxTokens:        *entry.MaxTokens,
		})
	}
	return models, nil
}
