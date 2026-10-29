package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/losinggeneration/tau/agent"
	sessionpkg "github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
)

// VirtualModelStateEntry is the custom entry type that stores router state on
// a session branch.
const VirtualModelStateEntry = "pi.virtual-model-state"

type virtualModelState struct {
	Provider string          `json:"provider"`
	ModelID  string          `json:"modelId"`
	State    json.RawMessage `json:"state"`
}

// unroutedClient fails every request for a virtual model that reached a
// provider without routing.
type unroutedClient struct{ model *ai.Model }

func (c unroutedClient) err() error {
	return fmt.Errorf("virtual model %s/%s must be routed before streaming", c.model.Provider, c.model.ID)
}

func (c unroutedClient) Stream(context.Context, *ai.Model, *ai.Context, ai.EventHandler, ...ai.Option) error {
	return c.err()
}

func (c unroutedClient) Complete(context.Context, *ai.Model, *ai.Context, ...ai.Option) (*ai.AssistantMessage, error) {
	return nil, c.err()
}

// hasModelCredentials reports whether a routed provider can authenticate.
func (s *agentSession) hasModelCredentials(ctx context.Context, provider string) bool {
	if ai.ProviderAllowsNoCredential(provider) {
		return true
	}
	if s.credentialProvider == nil {
		return false
	}
	key, err := s.credentialProvider(ctx, provider)
	return err == nil && key != ""
}

// latestResponse is the latest assistant message that did not fail.
func latestResponse(messages []*agent.Message) *agent.Message {
	for _, message := range slices.Backward(messages) {
		if message != nil && message.Role == agent.RoleAssistant && message.StopReason != ai.StopReasonError && message.StopReason != ai.StopReasonAborted {
			return message
		}
	}
	return nil
}

// routeVirtualModel asks the selected virtual model's router for the model of
// one request. Direct requests neither read nor store router state.
func (s *agentSession) routeVirtualModel(ctx context.Context, model *ai.Model, level agent.ThinkingLevel, reason ai.ModelRouteReason, messages []*agent.Message, failed *agent.Message) (ai.ModelRoute, error) {
	registry := s.runtime.modelRegistry
	request := ai.ModelRouteRequest{ThinkingLevel: level, Reason: reason}
	if failed != nil {
		messages = slices.DeleteFunc(slices.Clone(messages), func(message *agent.Message) bool { return message == failed })
		if physical := registry.PhysicalModel(failed.Provider, failed.Model); physical != nil {
			request.Failed = &ai.FailedModelRoute{Model: physical, ThinkingLevel: failed.ThinkingLevel, Message: &ai.AssistantMessage{Role: "assistant", Provider: failed.Provider, Model: failed.Model, StopReason: failed.StopReason, ErrorMessage: failed.ErrorMessage, Content: slices.Clone(failed.Content)}}
		}
	}
	if latest := latestResponse(messages); latest != nil {
		if physical := registry.PhysicalModel(latest.Provider, latest.Model); physical != nil {
			request.Previous = &ai.RoutedModel{Model: physical, ThinkingLevel: latest.ThinkingLevel}
		}
	}
	request.Messages = agent.ConvertMessagesToAI(messages)
	if reason != ai.ModelRouteDirect {
		state, err := s.virtualState(model)
		if err != nil {
			return ai.ModelRoute{}, err
		}
		request.State = state
	}
	route, err := registry.RouteModel(ctx, model, request, s.hasModelCredentials)
	if err != nil {
		return ai.ModelRoute{}, err
	}
	if reason != ai.ModelRouteDirect && len(route.State) > 0 && !bytes.Equal(route.State, request.State) {
		if err := s.storeVirtualState(model, route.State); err != nil {
			return ai.ModelRoute{}, err
		}
	}
	s.mu.Lock()
	s.routedModel = route.Model
	s.mu.Unlock()
	return route, nil
}

// prepareVirtualRequest routes each agent-loop request of a virtual selection.
// The selection stays on the session; only this request uses the route.
func (s *agentSession) prepareVirtualRequest(ctx context.Context, request agent.PrepareRequestContext) (*agent.AgentRequestUpdate, error) {
	s.mu.Lock()
	model, level, failed := s.model, s.thinkingLevel, s.failedResponse
	s.failedResponse = nil
	s.mu.Unlock()
	if !ai.IsVirtualModel(model) {
		return nil, nil
	}
	reason := ai.ModelRouteContinuation
	messages := request.Context.Messages
	lastResponse := -1
	for i, message := range messages {
		if message != nil && message.Role == agent.RoleAssistant {
			lastResponse = i
		}
	}
	if failed != nil {
		reason = ai.ModelRouteRetry
	} else if slices.ContainsFunc(messages[lastResponse+1:], func(message *agent.Message) bool { return message != nil && message.Role == agent.RoleUser }) {
		reason = ai.ModelRouteUser
	}
	route, err := s.routeVirtualModel(ctx, model, level, reason, messages, failed)
	if err != nil {
		return nil, err
	}
	return &agent.AgentRequestUpdate{Model: route.Model, ThinkingLevel: &route.ThinkingLevel}, nil
}

// directModel is the model and thinking level for a request outside the agent
// loop, such as a compaction summary.
func (s *agentSession) directModel(ctx context.Context) (*ai.Model, agent.ThinkingLevel, error) {
	s.mu.Lock()
	model, level := s.model, s.thinkingLevel
	s.mu.Unlock()
	if !ai.IsVirtualModel(model) {
		return model, level, nil
	}
	route, err := s.routeVirtualModel(ctx, model, level, ai.ModelRouteDirect, s.agent.Messages(), nil)
	if err != nil {
		return nil, "", err
	}
	return route.Model, route.ThinkingLevel, nil
}

// limitsModel is the model whose limits apply to the conversation: under a
// virtual selection, the physical model that answered last.
func (s *agentSession) limitsModel() *ai.Model {
	s.mu.Lock()
	model, routed := s.model, s.routedModel
	s.mu.Unlock()
	if !ai.IsVirtualModel(model) {
		return model
	}
	if routed != nil {
		return routed
	}
	if latest := latestResponse(s.agent.Messages()); latest != nil {
		if physical := s.runtime.modelRegistry.PhysicalModel(latest.Provider, latest.Model); physical != nil {
			return physical
		}
	}
	return model
}

func virtualStateKey(model *ai.Model) string { return model.Provider + "/" + model.ID }

// virtualState is the router state last stored on the current branch.
// ponytail: rereads the session file per routed request; cache per branch if
// large virtual-model sessions make that measurable.
func (s *agentSession) virtualState(model *ai.Model) (json.RawMessage, error) {
	s.mu.Lock()
	state := s.virtualStates[virtualStateKey(model)]
	s.mu.Unlock()
	entries, persisted, err := s.branchCustomEntries(VirtualModelStateEntry)
	if err != nil {
		return nil, fmt.Errorf("read virtual model state: %w", err)
	}
	if !persisted {
		return state, nil
	}
	for _, entry := range slices.Backward(entries) {
		var data virtualModelState
		if err := json.Unmarshal(entry.Data, &data); err != nil {
			return nil, fmt.Errorf("decode virtual model state %s: %w", entry.ID, err)
		}
		if data.Provider == model.Provider && data.ModelID == model.ID {
			return data.State, nil
		}
	}
	return nil, nil
}

// branchCustomEntries returns the custom entries of customType that hang off
// the current branch, oldest first. persisted is false when the session has
// no file or position yet.
func (s *agentSession) branchCustomEntries(customType string) (entries []sessionpkg.Entry, persisted bool, err error) {
	s.mu.Lock()
	writer, parent := s.sessionWriter, s.parentMsgID
	s.mu.Unlock()
	if writer == nil || parent == nil {
		return nil, false, nil
	}
	reader, err := sessionpkg.ReadFile(writer.Path())
	if err != nil {
		return nil, true, err
	}
	manager := sessionpkg.NewManager(reader.Header().CWD)
	manager.LoadEntries(reader)
	branch := map[string]bool{}
	for _, entry := range manager.GetBranch(*parent) {
		branch[entry.ID] = true
	}
	for _, entry := range reader.Entries() {
		if entry.Type == sessionpkg.EntryTypeCustom && entry.CustomType != nil && *entry.CustomType == customType && entry.ParentID != nil && branch[*entry.ParentID] {
			entries = append(entries, entry)
		}
	}
	return entries, true, nil
}

// storeVirtualState records router state after the current branch position.
// The entry hangs off that position, like error entries, so it follows the
// branch without moving the next message's parent.
func (s *agentSession) storeVirtualState(model *ai.Model, state json.RawMessage) error {
	if !json.Valid(state) {
		return fmt.Errorf("virtual model %s/%s returned state that is not JSON", model.Provider, model.ID)
	}
	s.mu.Lock()
	if s.virtualStates == nil {
		s.virtualStates = map[string]json.RawMessage{}
	}
	s.virtualStates[virtualStateKey(model)] = slices.Clone(state)
	writer, parent := s.sessionWriter, s.parentMsgID
	s.mu.Unlock()
	if writer == nil || parent == nil {
		return nil
	}
	data, err := json.Marshal(virtualModelState{Provider: model.Provider, ModelID: model.ID, State: state})
	if err != nil {
		return fmt.Errorf("encode virtual model state: %w", err)
	}
	entry := sessionpkg.NewCustomEntry(VirtualModelStateEntry, data)
	entry.ParentID = parent
	if err := writer.Append(entry); err != nil {
		return fmt.Errorf("append virtual model state: %w", err)
	}
	return nil
}

func latestAssistant(messages []*agent.Message) *agent.Message {
	for _, message := range slices.Backward(messages) {
		if message != nil && message.Role == agent.RoleAssistant {
			return message
		}
	}
	return nil
}

// syncPluginVirtualModels replaces the virtual models registered by plugins
// with the loaded plugins' current registrations.
func (r *runtime) syncPluginVirtualModels() {
	if r.pluginManager == nil || r.modelRegistry == nil {
		return
	}
	r.pluginVirtualMu.Lock()
	defer r.pluginVirtualMu.Unlock()
	for _, previous := range r.pluginVirtualModels {
		r.modelRegistry.UnregisterVirtualModel(previous.Provider, previous.ID)
	}
	r.pluginVirtualModels = nil
	for _, definition := range r.pluginManager.VirtualModels() {
		if err := r.modelRegistry.RegisterVirtualModel(definition); err != nil {
			r.logger.Warn("register plugin virtual model", "provider", definition.Provider, "id", definition.ID, "error", err)
			continue
		}
		r.pluginVirtualModels = append(r.pluginVirtualModels, definition)
	}
}
