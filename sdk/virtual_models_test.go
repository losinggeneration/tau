package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/losinggeneration/tau/agent"
	agentplugins "github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/agent/plugins/proto"
	sessionpkg "github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
)

type routerLog struct {
	mu       sync.Mutex
	requests []ai.ModelRouteRequest
}

func (l *routerLog) route(_ context.Context, request ai.ModelRouteRequest) (ai.ModelRoute, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, request)
	state := json.RawMessage(fmt.Sprintf(`{"routes":%d}`, len(l.requests)))
	if request.Reason == ai.ModelRouteDirect {
		state = nil
	}
	return ai.ModelRoute{Model: &ai.Model{Provider: "test-provider", ID: "test-model"}, ThinkingLevel: "high", State: state}, nil
}

func (l *routerLog) snapshot() []ai.ModelRouteRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ai.ModelRouteRequest(nil), l.requests...)
}

func sendAndDrain(t *testing.T, sess Session, text string) {
	t.Helper()
	events, err := sess.Send(testContext(t), Message{Text: text})
	require.NoError(t, err)
	collectSessionEvents(t, events)
}

func TestVirtualModelRoutesEachRequestAndPersistsBranchState(t *testing.T) {
	provider, _ := registerRecordingProvider(t, "routed reply")
	registry := ai.NewModelRegistry()
	router := &routerLog{}
	require.NoError(t, registry.RegisterVirtualModel(ai.VirtualModelDefinition{Provider: "router", ID: "auto", ThinkingLevels: []string{"off", "high"}, Route: router.route}))
	sessionsDir := t.TempDir()
	rt, err := NewRuntime(RuntimeOptions{Settings: compactAllSettings(), ModelRegistry: registry, ExplicitModel: "router/auto", SessionsDir: sessionsDir, CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("key")}, IgnoreEnv: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(testContext(t))) })

	sess, err := rt.NewSession(testContext(t), SessionOptions{})
	require.NoError(t, err)
	s := sess.(*agentSession)
	require.True(t, ai.IsVirtualModel(s.agent.State().Model))
	sendAndDrain(t, sess, "one")
	sendAndDrain(t, sess, "two")

	requests := router.snapshot()
	require.Len(t, requests, 2)
	assert.Equal(t, ai.ModelRouteUser, requests[0].Reason)
	assert.Empty(t, requests[0].State)
	assert.Nil(t, requests[0].Previous)
	assert.JSONEq(t, `{"routes":1}`, string(requests[1].State))
	require.NotNil(t, requests[1].Previous)
	assert.Equal(t, "test-model", requests[1].Previous.Model.ID)
	assert.Equal(t, "off", requests[1].Previous.ThinkingLevel, "route level is clamped to the physical model")
	assert.NotNil(t, provider.LastPrompt())
	assert.True(t, ai.IsVirtualModel(s.agent.State().Model), "routing replaced the selection")
	reply := latestResponse(s.agent.Messages())
	require.NotNil(t, reply)
	assert.Equal(t, "test-provider", reply.Provider)
	assert.Equal(t, "test-model", reply.Model)
	assert.Equal(t, "test-model", s.limitsModel().ID)

	require.NoError(t, s.Compact(testContext(t)))
	requests = router.snapshot()
	require.Len(t, requests, 3)
	assert.Equal(t, ai.ModelRouteDirect, requests[2].Reason)
	assert.Empty(t, requests[2].State)

	id := s.id
	require.NoError(t, sess.Close(testContext(t)))
	storage, err := sessionpkg.NewStorageAt(sessionsDir)
	require.NoError(t, err)
	reader, err := storage.Open(id)
	require.NoError(t, err)
	var states int
	for _, entry := range reader.Entries() {
		if entry.CustomType != nil && *entry.CustomType == VirtualModelStateEntry {
			states++
			assert.NotNil(t, entry.ParentID)
		}
	}
	assert.Equal(t, 2, states)

	resumed, err := rt.NewSession(testContext(t), SessionOptions{SessionID: id})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resumed.Close(testContext(t))) })
	assert.True(t, ai.IsVirtualModel(resumed.(*agentSession).agent.State().Model), "resume lost the virtual selection")
	sendAndDrain(t, resumed, "three")
	requests = router.snapshot()
	require.Len(t, requests, 4)
	assert.JSONEq(t, `{"routes":2}`, string(requests[3].State))
}

func TestVirtualModelStateFollowsBranch(t *testing.T) {
	registerRecordingProvider(t, "reply")
	registry := ai.NewModelRegistry()
	router := &routerLog{}
	require.NoError(t, registry.RegisterVirtualModel(ai.VirtualModelDefinition{Provider: "router", ID: "auto", Route: router.route}))
	rt, err := NewRuntime(RuntimeOptions{ModelRegistry: registry, ExplicitModel: "router/auto", SessionsDir: t.TempDir(), CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("key")}, IgnoreEnv: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(testContext(t))) })
	sess, err := rt.NewSession(testContext(t), SessionOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sess.Close(testContext(t))) })
	s := sess.(*agentSession)
	sendAndDrain(t, sess, "one")
	s.mu.Lock()
	first := *s.parentMsgID
	s.mu.Unlock()
	sendAndDrain(t, sess, "two")

	model := s.model
	state, err := s.virtualState(model)
	require.NoError(t, err)
	assert.JSONEq(t, `{"routes":2}`, string(state))
	s.mu.Lock()
	s.parentMsgID = &first
	s.mu.Unlock()
	state, err = s.virtualState(model)
	require.NoError(t, err)
	assert.JSONEq(t, `{"routes":1}`, string(state), "state from another branch leaked")
}

func TestVirtualModelRetryAndRoutingFailures(t *testing.T) {
	registerRecordingProvider(t, "reply")
	registry := ai.NewModelRegistry()
	var reasons []ai.ModelRouteReason
	var failed *ai.FailedModelRoute
	target := "test-provider/test-model"
	require.NoError(t, registry.RegisterVirtualModel(ai.VirtualModelDefinition{Provider: "router", ID: "auto", Route: func(_ context.Context, request ai.ModelRouteRequest) (ai.ModelRoute, error) {
		reasons = append(reasons, request.Reason)
		failed = request.Failed
		provider, id, _ := strings.Cut(target, "/")
		return ai.ModelRoute{Model: &ai.Model{Provider: provider, ID: id}}, nil
	}}))
	rt, err := NewRuntime(RuntimeOptions{ModelRegistry: registry, ExplicitModel: "router/auto", CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("key")}, IgnoreEnv: true, NoSession: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(testContext(t))) })
	sess, err := rt.NewSession(testContext(t), SessionOptions{NoSession: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sess.Close(testContext(t))) })
	s := sess.(*agentSession)

	failure := &agent.Message{Role: agent.RoleAssistant, Provider: "test-provider", Model: "test-model", StopReason: ai.StopReasonError, ErrorMessage: "overloaded"}
	s.failedResponse = failure
	update, err := s.prepareVirtualRequest(testContext(t), agent.PrepareRequestContext{Context: &agent.AgentContext{Messages: []*agent.Message{{Role: agent.RoleUser}, failure}}})
	require.NoError(t, err)
	assert.Equal(t, "test-model", update.Model.ID)
	assert.Equal(t, []ai.ModelRouteReason{ai.ModelRouteRetry}, reasons)
	require.NotNil(t, failed)
	assert.Equal(t, "overloaded", failed.Message.ErrorMessage)
	assert.Nil(t, s.failedResponse)

	_, err = s.prepareVirtualRequest(testContext(t), agent.PrepareRequestContext{Context: &agent.AgentContext{Messages: []*agent.Message{{Role: agent.RoleUser}, {Role: agent.RoleAssistant}, {Role: agent.RoleTool}}}})
	require.NoError(t, err)
	assert.Equal(t, ai.ModelRouteContinuation, reasons[1])

	target = "router/auto"
	_, err = s.prepareVirtualRequest(testContext(t), agent.PrepareRequestContext{Context: &agent.AgentContext{}})
	assert.ErrorContains(t, err, "not a physical model")

	client, err := s.clientFactory(s.model)
	require.NoError(t, err)
	_, err = client.Complete(testContext(t), s.model, &ai.Context{})
	assert.ErrorContains(t, err, "must be routed before streaming")

	require.NoError(t, registry.RegisterVirtualModel(ai.VirtualModelDefinition{Provider: "router", ID: "fast", Route: func(context.Context, ai.ModelRouteRequest) (ai.ModelRoute, error) { return ai.ModelRoute{}, nil }}))
	next, err := s.CycleModel()
	require.NoError(t, err)
	assert.Equal(t, "router/fast", next.Provider+"/"+next.ID)
	assert.True(t, ai.IsVirtualModel(s.agent.State().Model))
	require.NoError(t, sess.SetModel("test-provider/test-model"))
	update, err = s.prepareVirtualRequest(testContext(t), agent.PrepareRequestContext{Context: &agent.AgentContext{}})
	require.NoError(t, err)
	assert.Nil(t, update, "physical selection was routed")
}

func TestResumeWithUnregisteredVirtualModelFallsBackToAnsweringModel(t *testing.T) {
	registerRecordingProvider(t, "reply")
	registry := ai.NewModelRegistry()
	router := &routerLog{}
	require.NoError(t, registry.RegisterVirtualModel(ai.VirtualModelDefinition{Provider: "router", ID: "auto", Route: router.route}))
	sessionsDir := t.TempDir()
	options := RuntimeOptions{ModelRegistry: registry, ExplicitModel: "router/auto", SessionsDir: sessionsDir, CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("key")}, IgnoreEnv: true}
	rt, err := NewRuntime(options)
	require.NoError(t, err)
	sess, err := rt.NewSession(testContext(t), SessionOptions{})
	require.NoError(t, err)
	require.NoError(t, sess.SetModel("router/auto"))
	sendAndDrain(t, sess, "one")
	id := sess.(*agentSession).id
	require.NoError(t, sess.Close(testContext(t)))
	require.NoError(t, rt.Close(testContext(t)))

	options.ModelRegistry = ai.NewModelRegistry()
	options.ExplicitModel = ""
	rt, err = NewRuntime(options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(testContext(t))) })
	resumed, err := rt.NewSession(testContext(t), SessionOptions{SessionID: id})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resumed.Close(testContext(t))) })
	model := resumed.(*agentSession).agent.State().Model
	require.NotNil(t, model)
	assert.Equal(t, "test-provider/test-model", model.Provider+"/"+model.ID)
}

type routingPluginClient struct {
	proto.PluginClient
	registrations *proto.Registrations
	reasons       []string
}

func (c *routingPluginClient) Initialize(context.Context, *proto.InitRequest, ...grpc.CallOption) (*proto.InitResponse, error) {
	return &proto.InitResponse{ApiVersion: "1"}, nil
}

func (c *routingPluginClient) ListRegistrations(context.Context, *proto.ListRegistrationsRequest, ...grpc.CallOption) (*proto.Registrations, error) {
	return c.registrations, nil
}

func (c *routingPluginClient) OnEvent(context.Context, *proto.Event, ...grpc.CallOption) (*proto.EventResponse, error) {
	return &proto.EventResponse{}, nil
}

func (c *routingPluginClient) Shutdown(context.Context, *proto.ShutdownRequest, ...grpc.CallOption) (*proto.ShutdownResponse, error) {
	return &proto.ShutdownResponse{Graceful: true}, nil
}

func (c *routingPluginClient) RouteModel(_ context.Context, request *proto.ModelRouteRequest, _ ...grpc.CallOption) (*proto.ModelRouteResponse, error) {
	var decoded ai.ModelRouteRequest
	if err := json.Unmarshal([]byte(request.RequestJson), &decoded); err != nil {
		return nil, err
	}
	c.reasons = append(c.reasons, string(decoded.Reason))
	return &proto.ModelRouteResponse{Provider: "test-provider", ModelId: "test-model", StateJson: `{"plugin":true}`}, nil
}

func TestPluginVirtualModelsRegisterRouteAndReload(t *testing.T) {
	provider, _ := registerRecordingProvider(t, "plugin routed")
	configDir := t.TempDir()
	pluginDir := filepath.Join(configDir, "extensions")
	require.NoError(t, os.MkdirAll(pluginDir, 0o700))
	path := filepath.Join(pluginDir, "router")
	require.NoError(t, os.WriteFile(path, []byte("not executed"), 0o700))
	first := &routingPluginClient{registrations: &proto.Registrations{VirtualModels: []*proto.VirtualModelRegistration{{Provider: "router", Id: "auto", Name: "Auto"}}}}
	second := &routingPluginClient{registrations: &proto.Registrations{VirtualModels: []*proto.VirtualModelRegistration{{Provider: "router", Id: "fast"}}}}
	clients := []*routingPluginClient{first, second}
	mgr := agentplugins.NewManager(configDir, nil, nil, agentplugins.WithPluginConnector(func(context.Context, string, *agentplugins.Manifest, string, string) (proto.PluginClient, func(), error) {
		client := clients[0]
		clients = clients[1:]
		return client, func() {}, nil
	}))
	_, err := mgr.Load(t.Context(), path)
	require.NoError(t, err)
	registry := ai.NewModelRegistry()
	rt, err := NewRuntime(RuntimeOptions{PluginManager: mgr, ModelRegistry: registry, NoBuiltinTools: true, NoContextFiles: true, CredentialStore: &mockCredStore{getCred: ai.NewAPIKeyCredential("key")}, ExplicitModel: "test-provider/test-model", SessionsDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(testContext(t))) })
	sess, err := rt.NewSession(t.Context(), SessionOptions{NoSession: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sess.Close(testContext(t))) })
	s := sess.(*agentSession)

	require.NoError(t, sess.SetModel("router/auto"))
	sendAndDrain(t, sess, "hello")
	assert.Equal(t, []string{"user"}, first.reasons)
	assert.NotNil(t, provider.LastPrompt())
	assert.JSONEq(t, `{"plugin":true}`, string(s.virtualStates["router/auto"]))

	require.NoError(t, mgr.ReloadNamed(t.Context(), "router"))
	mgr.RefreshToolRegistry()
	require.NoError(t, s.ReloadToolRegistry(t.Context()))
	_, err = registry.ResolveModel("router/auto")
	assert.Error(t, err, "reload kept a removed virtual model")
	fast, err := registry.ResolveModel("router/fast")
	require.NoError(t, err)
	assert.True(t, ai.IsVirtualModel(fast))
}
