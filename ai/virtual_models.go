package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// VirtualModelAPI is the API of virtual catalog entries. Providers never
// stream it: each request is routed to a physical model first.
const VirtualModelAPI = "pi-virtual"

// ModelRouteReason says why a request is being routed.
type ModelRouteReason string

const (
	// ModelRouteUser is the first request after a message the user wrote.
	ModelRouteUser ModelRouteReason = "user"
	// ModelRouteContinuation is any other request in the agent loop.
	ModelRouteContinuation ModelRouteReason = "continuation"
	// ModelRouteRetry is an automatic retry after a failed request.
	ModelRouteRetry ModelRouteReason = "retry"
	// ModelRouteDirect is a request outside the agent loop, such as a summary.
	ModelRouteDirect ModelRouteReason = "direct"
)

// RoutedModel is a physical model and the thinking level it ran at.
type RoutedModel struct {
	Model         *Model `json:"model"`
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
}

// FailedModelRoute is the failed request a retry replaces.
type FailedModelRoute struct {
	Model         *Model            `json:"model"`
	Message       *AssistantMessage `json:"message"`
	ThinkingLevel string            `json:"thinkingLevel,omitempty"`
}

// ModelRouteRequest is one routing decision for a virtual model.
type ModelRouteRequest struct {
	// Model is the selected virtual model.
	Model         *Model           `json:"model"`
	ThinkingLevel string           `json:"thinkingLevel"`
	Reason        ModelRouteReason `json:"reason"`
	// Previous is the latest successful response in Messages.
	Previous *RoutedModel `json:"previous,omitempty"`
	// Failed is set for retries; Messages no longer contains it.
	Failed *FailedModelRoute `json:"failed,omitempty"`
	// State is the router state last stored on the session branch. It is
	// empty before the first state and for direct requests.
	State    json.RawMessage `json:"state,omitempty"`
	Messages []Message       `json:"messages"`
}

// ModelRoute is the physical model and thinking level for one request.
type ModelRoute struct {
	Model         *Model
	ThinkingLevel string
	// State replaces the stored router state unless empty or unchanged.
	State json.RawMessage
}

// VirtualModelDefinition lists a virtual model under Provider.
type VirtualModelDefinition struct {
	Route          func(context.Context, ModelRouteRequest) (ModelRoute, error)
	Provider       string
	ID             string
	Name           string
	ThinkingLevels []string
	Input          []InputType
	// ContextWindow and MaxTokens are shown before the first response; zero
	// means unknown.
	ContextWindow int
	MaxTokens     int
}

// IsVirtualModel reports whether model must be routed before streaming.
func IsVirtualModel(model *Model) bool {
	return model != nil && model.API == VirtualModelAPI
}

func newVirtualModel(definition VirtualModelDefinition) *Model {
	levels := definition.ThinkingLevels
	if len(levels) == 0 {
		levels = []string{"off"}
	}
	levelMap := make(map[string]*string, len(ThinkingLevels))
	for _, level := range ThinkingLevels {
		levelMap[level] = nil
		if slices.Contains(levels, level) {
			levelMap[level] = new(level)
		}
	}
	input := slices.Clone(definition.Input)
	if len(input) == 0 {
		input = []InputType{InputText, InputImage}
	}
	return &Model{
		ID:               definition.ID,
		Name:             definition.Name,
		API:              VirtualModelAPI,
		Provider:         definition.Provider,
		Reasoning:        slices.ContainsFunc(levels, func(level string) bool { return level != "off" }),
		ThinkingLevelMap: levelMap,
		Input:            input,
		Output:           []OutputType{OutputText},
		ContextWindow:    definition.ContextWindow,
		MaxTokens:        definition.MaxTokens,
	}
}

// RegisterVirtualModel lists a virtual chat model, replacing one with the same
// provider and id. It fails when a physical chat model already uses the id.
func (r *ModelRegistry) RegisterVirtualModel(definition VirtualModelDefinition) error {
	definition.Provider = canonicalProvider(strings.TrimSpace(definition.Provider))
	if definition.Provider == "" || strings.TrimSpace(definition.ID) == "" {
		return errors.New("virtual model provider and id must not be empty")
	}
	if definition.Route == nil {
		return fmt.Errorf("virtual model %s/%s has no router", definition.Provider, definition.ID)
	}
	key := definition.Provider + "/" + definition.ID
	if r.virtualModel(key) == nil {
		if _, err := r.resolvePhysicalModel("chat", key); err == nil {
			return fmt.Errorf("virtual model %s conflicts with a physical model", key)
		}
	}
	if definition.Name == "" {
		definition.Name = definition.ID
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.virtual == nil {
		r.virtual = map[string]VirtualModelDefinition{}
	}
	r.virtual[key] = definition
	return nil
}

// UnregisterVirtualModel removes a virtual model.
func (r *ModelRegistry) UnregisterVirtualModel(provider, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.virtual, canonicalProvider(provider)+"/"+id)
}

func (r *ModelRegistry) virtualModel(spec string) *Model {
	if r == nil {
		return nil
	}
	provider, id, ok := strings.Cut(spec, "/")
	if !ok {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	definition, ok := r.virtual[canonicalProvider(provider)+"/"+id]
	if !ok {
		return nil
	}
	return newVirtualModel(definition)
}

// PhysicalModel resolves a chat model that is not virtual.
func (r *ModelRegistry) PhysicalModel(provider, id string) *Model {
	model, err := r.resolvePhysicalModel("chat", provider+"/"+id)
	if err != nil {
		return nil
	}
	return model
}

// RouteModel asks a virtual model's router for one request's physical model.
// The target must be a physical catalog model whose provider has credentials;
// its thinking level is clamped to that model.
func (r *ModelRegistry) RouteModel(ctx context.Context, model *Model, request ModelRouteRequest, hasCredentials func(context.Context, string) bool) (ModelRoute, error) {
	name := fmt.Sprintf("virtual model %s/%s", model.Provider, model.ID)
	r.mu.RLock()
	definition, ok := r.virtual[canonicalProvider(model.Provider)+"/"+model.ID]
	r.mu.RUnlock()
	if !ok {
		return ModelRoute{}, fmt.Errorf("%s is not registered", name)
	}
	request.Model = model
	route, err := definition.Route(ctx, request)
	if err != nil {
		return ModelRoute{}, fmt.Errorf("route %s: %w", name, err)
	}
	if route.Model == nil {
		return ModelRoute{}, fmt.Errorf("%s returned no model", name)
	}
	routed := fmt.Sprintf("%s routed to %s/%s", name, route.Model.Provider, route.Model.ID)
	target := r.PhysicalModel(route.Model.Provider, route.Model.ID)
	if target == nil || IsVirtualModel(route.Model) {
		return ModelRoute{}, fmt.Errorf("%s, which is not a physical model", routed)
	}
	if !hasCredentials(ctx, target.Provider) {
		return ModelRoute{}, fmt.Errorf("%s, which has no credentials", routed)
	}
	return ModelRoute{Model: target, ThinkingLevel: ClampThinkingLevel(target, route.ThinkingLevel), State: route.State}, nil
}
