package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	tauai "github.com/losinggeneration/tau/ai"
)

func TestRenderGoWritesCatalogMetadata(t *testing.T) {
	model := &tauai.Model{
		ID: "test", Type: "classifier", ThinkingLevelMap: map[string]*string{"max": new("max"), "minimal": nil},
		Cost:           tauai.CostProfile{Tiers: []tauai.CostTier{{InputTokensAbove: 200000, Input: 2, Output: 4}}},
		InputLimits:    &tauai.ModelInputLimits{MaxRequestBytes: 123, Images: &tauai.ModelImageInputLimits{MaxPerRequest: 2, Resize: &tauai.ModelImageResizeOptions{JPEGQuality: 70}}},
		SamplingParams: map[string]any{"temperature": 0.2}, SamplingParamsByThinkingLevel: map[string]map[string]any{"high": {"top_p": 0.8}},
		Input:       []tauai.InputType{tauai.InputText, tauai.InputImage},
		PromptCache: &tauai.PromptCacheTTL{Short: 300, Long: 3600},
		Enabled:     new(true),
		Providers:   []tauai.ModelServingProvider{{ID: "fireworks", Name: "Fireworks AI"}},
		Compat:      &tauai.CompatibilitySettings{SupportsStore: new(false), SupportsStrictTools: true, ChatTemplateArgs: json.RawMessage(`{"enable_thinking":true}`), AllowedFallbackModels: []tauai.FallbackModel{{Provider: "anthropic", Model: "claude-opus-4-8", Cost: tauai.CostProfile{Input: 5}}}},
	}
	output, err := renderGo([]*tauai.Model{model})
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.Join(strings.Fields(string(output)), " ")
	for _, want := range []string{
		`"encoding/json"`, `Type:`, `"classifier"`, `ThinkingLevelMap: map[string]*string{`, `"max":`, `new("max")`, `"minimal": nil`,
		`Tiers: []CostTier{`, `InputTokensAbove: 200000`, `MaxRequestBytes: 123`, `MaxPerRequest: 2`, `JPEGQuality: 70`,
		`"temperature": 0.2`, `"top_p": 0.8`, `Input: []InputType{"text", "image"}`, `PromptCache: &PromptCacheTTL{ Short: 300, Long: 3600, }`,
		`Enabled:`, `new(true)`, `Providers: []ModelServingProvider{`, `SupportsStore:`, `new(false)`, `SupportsStrictTools: true`,
		"json.RawMessage(`{\"enable_thinking\":true}`)", `AllowedFallbackModels: []FallbackModel{`,
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("missing %q in\n%s", want, output)
		}
	}
	if strings.Contains(string(output), "SamplingParams: map[string]any{},") {
		t.Error("empty values were written")
	}
}

func upstreamTarball(t *testing.T, files map[string]string, manifest map[string]string) *bytes.Buffer {
	t.Helper()
	if manifest == nil {
		manifest = map[string]string{}
		for name, content := range files {
			sum := sha256.Sum256([]byte(content))
			manifest[name] = hex.EncodeToString(sum[:])
		}
	}
	encoded, err := json.Marshal(map[string]any{"schemaVersion": 6, "files": manifest})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	zipped := gzip.NewWriter(&archive)
	writer := tar.NewWriter(zipped)
	add := func(name, content string) {
		if err := writer.WriteHeader(&tar.Header{Name: "package/dist/providers/data/" + name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	add(".manifest.json", string(encoded))
	for name, content := range files {
		add(name, content)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "package/dist/index.js", Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return &archive
}

func TestLoadUpstreamCatalogKeepsOperationsAndRejectsDrift(t *testing.T) {
	files := map[string]string{"router.json": `{
		"openai-completions":{"chat:same":{"type":"chat","id":"same","name":"Same","api":"openai-completions","provider":"router","baseUrl":"https://r","input":["text"],"cost":{"input":1,"output":2,"cacheRead":0,"cacheWrite":0},"contextWindow":10,"maxTokens":5,"thinkingLevelMap":{"off":null}}},
		"typesafe-system-one":{"classifier:same":{"type":"classifier","id":"same","name":"Same","api":"typesafe-system-one","provider":"router","baseUrl":"https://r","input":["text"],"cost":{"input":1,"output":0,"cacheRead":0,"cacheWrite":0}}}}`}
	models, err := loadUpstreamCatalog(upstreamTarball(t, files, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "same" || models[1].ID != "same" || models[0].Type == models[1].Type {
		t.Fatalf("models = %+v", models)
	}
	if _, exists := models[0].ThinkingLevelMap["off"]; !exists && models[1].ThinkingLevelMap == nil {
		t.Fatal("thinking map lost")
	}

	if _, err := loadUpstreamCatalog(upstreamTarball(t, map[string]string{"x.json": `{"a":{"chat:x":{"id":"x","api":"a","provider":"x","newField":1}}}`}, nil)); err == nil || !strings.Contains(err.Error(), "newField") {
		t.Fatalf("unknown catalog field error = %v", err)
	}
	if _, err := loadUpstreamCatalog(upstreamTarball(t, files, map[string]string{"router.json": "00"})); err == nil {
		t.Fatal("accepted a data file that does not match the manifest")
	}
	if _, err := loadUpstreamCatalog(upstreamTarball(t, files, map[string]string{"router.json": files["router.json"], "missing.json": "00"})); err == nil {
		t.Fatal("accepted a catalog missing a manifest file")
	}
}

func TestMergeCatalogsKeepsTauOnlyProviders(t *testing.T) {
	upstream := []*tauai.Model{{Provider: "openai", ID: "gpt", API: "openai-responses"}, {Provider: "openai", ID: "gpt", Type: "image", API: "openai-images"}}
	legacy := []*tauai.Model{{Provider: "openai", ID: "old", API: "openai-responses"}, {Provider: "google-gemini-cli", ID: "gemini", API: tauai.APIGoogleGeminiCLI}}
	merged := mergeCatalogs(upstream, legacy)
	var keys []string
	for _, model := range merged {
		keys = append(keys, model.Type+":"+model.Provider+"/"+model.ID)
	}
	if strings.Join(keys, ",") != ":google-gemini-cli/gemini,:openai/gpt,image:openai/gpt" {
		t.Fatalf("merged = %v", keys)
	}
}

func TestEffortThinkingMapPreservesVerifiedUnsupportedLevels(t *testing.T) {
	var model modelsDevModel
	if err := json.Unmarshal([]byte(`{"reasoning_options":[{"type":"effort","values":["none","low","high"]}]}`), &model); err != nil {
		t.Fatal(err)
	}
	levels := effortThinkingLevelMap(model.ReasoningOptions)
	if levels["off"] == nil || *levels["off"] != "none" || levels["low"] == nil || levels["medium"] != nil || len(levels) != len(tauai.ThinkingLevels) {
		t.Fatalf("map=%+v", levels)
	}
}
