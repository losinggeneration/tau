package ai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestOpenRouterCatalogIsKeyScoped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/user" || r.Header.Get("Authorization") != "Bearer key" {
			t.Error("incorrect catalog request")
		}
		if _, err := io.WriteString(w, `{"data":[{"id":"allowed","context_length":500,"pricing":{"prompt":"0.000002","completion":"0.000004"}}]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	original := &Model{ID: "allowed", ContextWindow: 100, Cost: CostProfile{Input: 1}}
	models, err := PersonalizeOpenRouterCatalog(context.Background(), server.Client(), []*Model{original, {ID: "denied"}}, server.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ContextWindow != 500 || models[0].Cost.Input != 2 {
		t.Fatalf("catalog: %#v", models)
	}
	if original.ContextWindow != 100 || original.Cost.Input != 1 {
		t.Fatal("shared catalog mutated")
	}
}

func TestEmptyPersonalizedCatalogIsExplicit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, `{"data":[]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	models, err := PersonalizeOpenRouterCatalog(context.Background(), server.Client(), []*Model{{ID: "restricted"}}, server.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	if models == nil || len(models) != 0 {
		t.Fatalf("empty allowed catalog must persist as [], got %#v", models)
	}
}

func TestOpenRouterCatalogNormalizesPricingLikeUpstream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, `{"data":[
			{"id":"tiered","context_length":null,"top_provider":{"context_length":0,"max_completion_tokens":null},"pricing":{"prompt":"0.0000003","completion":"oops","input_cache_read":"-1","overrides":[
				{"min_prompt_tokens":200000,"prompt":"0.0000006"},
				{"min_prompt_tokens":1000,"utc_start":1,"prompt":"0.1"},
				{"utc_days":["mon"],"prompt":"0.1"}
			]}},
			{"id":"router","pricing":{"prompt":"-1","completion":"-1"}}
		]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	catalog := []*Model{
		{ID: "tiered", ContextWindow: 100, MaxTokens: 10},
		{ID: "router"},
		{ID: "flux", Type: "image"},
	}
	models, err := PersonalizeOpenRouterCatalog(context.Background(), server.Client(), catalog, server.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 || models[2].ID != "flux" {
		t.Fatalf("models = %#v", models)
	}
	tiered := models[0]
	want := CostProfile{Input: 0.3, Tiers: []CostTier{{InputTokensAbove: 200000, Input: 0.6}}}
	if !reflect.DeepEqual(tiered.Cost, want) || tiered.ContextWindow != 100 || tiered.MaxTokens != 10 {
		t.Fatalf("tiered = %+v", tiered)
	}
	if models[1].Cost.Input != 0 || models[1].Cost.Output != 0 {
		t.Fatalf("router = %+v", models[1].Cost)
	}
}
