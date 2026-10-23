package ai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestGenerateImagesOverOpenRouterChatCompletions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer key" || r.Header.Get("X-Custom") != "1" {
			t.Errorf("path=%s headers=%v", r.URL.Path, r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		want := map[string]any{
			"model":      "flux",
			"stream":     false,
			"modalities": []any{"image", "text"},
			"messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "a cat"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
			}}},
		}
		if !reflect.DeepEqual(body, want) {
			t.Errorf("body = %v", body)
		}
		if _, err := io.WriteString(w, `{"id":"gen-1","choices":[{"message":{"content":"here","images":[{"image_url":{"url":"data:image/webp;base64,QkJC"}},{"image_url":"data:image/png;base64,Q0ND"},{"image_url":"https://remote/skip.png"}]}}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30,"cache_write_tokens":10}}}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	model := &Model{Type: "image", ID: "flux", API: APIOpenRouterImages, Provider: ProviderOpenRouter, BaseURL: server.URL + "/api/v1", Output: []OutputType{"image", "text"}, Headers: map[string]string{"X-Custom": "1"}, Cost: CostProfile{Input: 1, Output: 2}}
	result, err := GenerateImages(t.Context(), model, []ContentBlock{
		{Type: ContentBlockText, Text: "a cat"},
		{Type: ContentBlockImage, MimeType: "image/png", Data: "AAAA"},
	}, WithAPIKey("key"))
	if err != nil {
		t.Fatal(err)
	}
	wantOutput := []ContentBlock{
		{Type: ContentBlockText, Text: "here"},
		{Type: ContentBlockImage, MimeType: "image/webp", Data: "QkJC"},
		{Type: ContentBlockImage, MimeType: "image/png", Data: "Q0ND"},
	}
	if !reflect.DeepEqual(result.Output, wantOutput) || result.ResponseID != "gen-1" || result.StopReason != StopReasonStop {
		t.Fatalf("result = %+v", result)
	}
	if u := result.Usage; u.Input != 70 || u.CacheRead != 20 || u.CacheWrite != 10 || u.Output != 20 || u.TotalTokens != 120 || u.Cost.Total == 0 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestGenerateImagesRejectsNonImageModelsAndReportsErrors(t *testing.T) {
	if _, err := GenerateImages(t.Context(), &Model{Type: "chat", API: APIOpenAICompletions}, nil, WithAPIKey("key")); err == nil {
		t.Fatal("chat model accepted")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		if _, err := io.WriteString(w, `{"error":"no"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	model := &Model{Type: "image", ID: "flux", API: APIOpenRouterImages, Provider: ProviderOpenRouter, BaseURL: server.URL, Output: []OutputType{"image"}}
	result, err := GenerateImages(t.Context(), model, []ContentBlock{{Type: ContentBlockText, Text: "x"}}, WithAPIKey("key"))
	if err == nil || result == nil || result.StopReason != StopReasonError || !strings.Contains(result.ErrorMessage, "400") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	t.Setenv("OPENROUTER_API_KEY", "")
	if result, err := GenerateImages(t.Context(), model, nil); err == nil || result.StopReason != StopReasonError {
		t.Fatalf("missing key: result=%+v err=%v", result, err)
	}
}

func TestGenerateImagesReportsUnresolvedConfig(t *testing.T) {
	t.Setenv("TAU_TEST_MISSING_IMAGE_KEY", "")
	model := &Model{Type: "image", ID: "flux", API: APIOpenRouterImages, Provider: ProviderOpenRouter, requestConfig: &modelRequestConfig{resolver: &configValueResolver{}, apiKey: "$TAU_TEST_MISSING_IMAGE_KEY"}}
	result, err := GenerateImages(t.Context(), model, nil)
	if err == nil || result == nil || result.StopReason != StopReasonError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
