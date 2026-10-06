package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const (
	// codexModelsClientVersion is sent as the required client_version query
	// parameter. The backend only returns models whose minimal_client_version
	// is at or below it, so a high value yields the full list.
	codexModelsClientVersion = "99.0.0"
	// codexModelVisible is the visibility of models shown in the Codex model
	// picker. Hidden entries are internal or retired models.
	codexModelVisible = "list"
)

type codexModelsResponse struct {
	Models []struct {
		Slug       string `json:"slug"`
		Visibility string `json:"visibility"`
	} `json:"models"`
}

// FetchCodexModelIDs returns the sorted IDs of the models the Codex backend
// lists for the account behind token. An empty baseURL uses the default
// ChatGPT backend.
func FetchCodexModelIDs(ctx context.Context, client *http.Client, baseURL, token string) (ids []string, err error) {
	accountID, err := extractCodexAccountID(token)
	if err != nil {
		return nil, fmt.Errorf("openai codex: extract account id: %w", err)
	}

	endpoint := resolveCodexModelsURL(baseURL) + "?" + url.Values{"client_version": {codexModelsClientVersion}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("openai codex: create models request: %w", err)
	}
	req.Header = buildBaseCodexHeaders(nil, nil, accountID, token)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai codex: models request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("openai codex: close models response body: %w", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("openai codex: models status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload codexModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("openai codex: decode models: %w", err)
	}

	for _, model := range payload.Models {
		if model.Slug == "" || model.Visibility != codexModelVisible {
			continue
		}
		ids = append(ids, model.Slug)
	}
	slices.Sort(ids)

	return slices.Compact(ids), nil
}

func resolveCodexModelsURL(baseURL string) string {
	return strings.TrimSuffix(resolveCodexURL(baseURL), "/responses") + "/models"
}
