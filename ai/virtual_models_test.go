package ai

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestModelRegistryRoutesVirtualModels(t *testing.T) {
	registry := NewModelRegistry()
	var seen ModelRouteRequest
	route := ModelRoute{Model: &Model{Provider: ProviderAnthropic, ID: "claude-sonnet-4-6"}, ThinkingLevel: "max", State: json.RawMessage(`{"n":1}`)}
	err := registry.RegisterVirtualModel(VirtualModelDefinition{Provider: "router", ID: "auto", Name: "Auto", ThinkingLevels: []string{"off", "high"}, Route: func(_ context.Context, request ModelRouteRequest) (ModelRoute, error) {
		seen = request
		return route, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	virtual, err := registry.ResolveModel("router/auto")
	if err != nil || !IsVirtualModel(virtual) || virtual.Name != "Auto" || !virtual.Reasoning || virtual.ThinkingLevelMap["high"] == nil || virtual.ThinkingLevelMap["low"] != nil || !virtual.SupportsInput(InputImage) {
		t.Fatalf("virtual catalog entry = %+v, %v", virtual, err)
	}
	if _, err := registry.ResolveOperationModel("image", "router/auto"); err == nil {
		t.Fatal("virtual chat model resolved for image operation")
	}
	catalog, err := registry.Models()
	if err != nil || !slices.ContainsFunc(catalog, IsVirtualModel) {
		t.Fatalf("catalog lacks virtual model: %v", err)
	}
	if err := registry.RegisterVirtualModel(VirtualModelDefinition{Provider: ProviderAnthropic, ID: "claude-sonnet-4-6", Route: func(context.Context, ModelRouteRequest) (ModelRoute, error) { return route, nil }}); err == nil {
		t.Fatal("virtual model replaced a physical model")
	}

	credentialed := func(context.Context, string) bool { return true }
	got, err := registry.RouteModel(context.Background(), virtual, ModelRouteRequest{Reason: ModelRouteUser, ThinkingLevel: "high", State: json.RawMessage(`{"n":0}`)}, credentialed)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.Provider != ProviderAnthropic || IsVirtualModel(got.Model) || got.Model.ContextWindow == 0 || string(got.State) != `{"n":1}` {
		t.Fatalf("route = %+v", got)
	}
	if got.ThinkingLevel != ClampThinkingLevel(got.Model, "max") {
		t.Fatalf("thinking level %q was not clamped", got.ThinkingLevel)
	}
	if seen.Model != virtual || seen.Reason != ModelRouteUser || string(seen.State) != `{"n":0}` {
		t.Fatalf("router request = %+v", seen)
	}

	route.Model = virtual
	if _, err := registry.RouteModel(context.Background(), virtual, ModelRouteRequest{}, credentialed); err == nil || !strings.Contains(err.Error(), "not a physical model") {
		t.Fatalf("recursive route error = %v", err)
	}
	route.Model = &Model{Provider: ProviderAnthropic, ID: "claude-sonnet-4-6"}
	if _, err := registry.RouteModel(context.Background(), virtual, ModelRouteRequest{}, func(context.Context, string) bool { return false }); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("uncredentialed route error = %v", err)
	}
	failure := errors.New("router down")
	if err := registry.RegisterVirtualModel(VirtualModelDefinition{Provider: "router", ID: "auto", Route: func(context.Context, ModelRouteRequest) (ModelRoute, error) { return ModelRoute{}, failure }}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RouteModel(context.Background(), virtual, ModelRouteRequest{}, credentialed); !errors.Is(err, failure) {
		t.Fatalf("router failure = %v", err)
	}
	registry.UnregisterVirtualModel("router", "auto")
	if _, err := registry.ResolveModel("router/auto"); err == nil {
		t.Fatal("unregistered virtual model still resolves")
	}
	if _, err := registry.RouteModel(context.Background(), virtual, ModelRouteRequest{}, credentialed); err == nil {
		t.Fatal("unregistered virtual model routed")
	}
}
