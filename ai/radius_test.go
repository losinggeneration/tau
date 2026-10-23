package ai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchRadiusCatalogReplacesShippedModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/config" || r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("path=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
		}
		if _, err := io.WriteString(w, `{"baseUrl":"https://gw.example/v1","models":[
			{"id":"fast","name":"Fast","reasoning":true,"thinkingLevelMap":{"high":"hi","max":null},"input":["text","image"],"cost":{"input":1,"output":2,"cacheRead":0.1,"cacheWrite":0},"contextWindow":1000,"maxTokens":100},
			{"id":"broken","name":"Broken"}
		]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	models, err := FetchRadiusCatalog(t.Context(), http.DefaultClient, server.URL+"/", "key")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %+v", models)
	}
	model := models[0]
	if model.ID != "fast" || model.Type != "chat" || model.API != APIPiMessages || model.Provider != ProviderRadius || model.BaseURL != "https://gw.example/v1" ||
		!model.Reasoning || model.Cost.Output != 2 || model.ContextWindow != 1000 || model.MaxTokens != 100 || *model.ThinkingLevelMap["high"] != "hi" || model.ThinkingLevelMap["max"] != nil || !model.SupportsInput(InputImage) {
		t.Fatalf("model = %+v", model)
	}
	if _, err := FetchRadiusCatalog(t.Context(), http.DefaultClient, "http://127.0.0.1:1", "key"); err == nil {
		t.Fatal("offline gateway accepted")
	}
}

func TestRadiusGateway(t *testing.T) {
	for in, want := range map[string]string{
		"":                         "https://radius.pi.dev",
		"https://radius.pi.dev/v1": "https://radius.pi.dev",
		"gw.example/v1/":           "https://gw.example",
		"http://local:8080":        "http://local:8080",
	} {
		if got := RadiusGateway(in); got != want {
			t.Errorf("RadiusGateway(%q) = %q, want %q", in, got, want)
		}
	}
}
