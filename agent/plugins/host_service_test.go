package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/losinggeneration/tau/agent/plugins/proto"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestSessionSendCustomMessage(t *testing.T) {
	manager := NewManager(t.TempDir(), ui.NewNoOpUI(), nil)
	host := NewHostService(manager)
	request := &proto.SessionSendCustomMessageRequest{MessageJson: `{"customType":"notice","content":null}`, DeliverAs: "nextTurn", TriggerTurn: new(false)}
	_, err := host.SessionSendCustomMessage(t.Context(), request)
	require.Equal(t, codes.Unavailable, status.Code(err))
	var calls int
	manager.SetCustomMessageSender(func(ctx context.Context, raw json.RawMessage, trigger *bool, delivery string) error {
		calls++
		assert.Equal(t, t.Context(), ctx)
		assert.JSONEq(t, request.MessageJson, string(raw))
		require.NotNil(t, trigger)
		assert.False(t, *trigger)
		assert.Equal(t, "nextTurn", delivery)
		return nil
	})
	response, err := host.SessionSendCustomMessage(t.Context(), request)
	require.NoError(t, err)
	assert.True(t, response.Ok)
	assert.Equal(t, 1, calls)
	for _, invalid := range []*proto.SessionSendCustomMessageRequest{
		{MessageJson: "{"},
		{MessageJson: "{}", DeliverAs: "invalid"},
	} {
		_, err := host.SessionSendCustomMessage(t.Context(), invalid)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = host.SessionSendCustomMessage(ctx, request)
	assert.Equal(t, codes.Canceled, status.Code(err))
	assert.Equal(t, 1, calls)
	manager.SetCustomMessageSender(func(context.Context, json.RawMessage, *bool, string) error { return errors.New("delivery rejected") })
	response, err = host.SessionSendCustomMessage(t.Context(), request)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, response.Error, "delivery rejected")
}

func TestToolDiscovery(t *testing.T) {
	manager := NewManager(t.TempDir(), ui.NewNoOpUI(), nil)
	host := NewHostService(manager)
	req := &proto.ToolDiscoveryRequest{Operation: "search", Query: "docs", Limit: 8}
	_, err := host.ToolDiscovery(t.Context(), req)
	require.Equal(t, codes.Unavailable, status.Code(err))
	var calls int
	manager.SetToolDiscovery(func(ctx context.Context, got *proto.ToolDiscoveryRequest) (json.RawMessage, error) {
		calls++
		require.Equal(t, req, got)
		require.NoError(t, ctx.Err())
		// The callback may reconfigure discovery without deadlocking the manager.
		manager.SetToolDiscovery(nil)
		return json.RawMessage(`[{"name":"lookup"}]`), nil
	})
	result, err := host.ToolDiscovery(t.Context(), req)
	require.NoError(t, err)
	require.JSONEq(t, `[{"name":"lookup"}]`, result.ResultJson)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = host.ToolDiscovery(ctx, req)
	require.Equal(t, codes.Canceled, status.Code(err))
	require.Equal(t, 1, calls)
	_, err = host.ToolDiscovery(t.Context(), &proto.ToolDiscoveryRequest{Operation: "invalid"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestToolProgressNestedRecords(t *testing.T) {
	manager := NewManager(t.TempDir(), ui.NewNoOpUI(), nil)
	host := NewHostService(manager)
	events := make(chan ToolProgressEvent, 1)
	manager.RegisterToolProgressSubscriber("parent", events)
	defer manager.UnregisterToolProgressSubscriber("parent", events)
	_, err := host.ToolProgress(t.Context(), &proto.ToolProgressEvent{ToolCallId: "parent", Content: "parent output", NestedCalls: `[{"id":"child","name":"lookup","status":"running"}]`})
	require.NoError(t, err)
	select {
	case event := <-events:
		require.Len(t, event.NestedCalls, 1)
		assert.Equal(t, "running", event.NestedCalls[0].Status)
		assert.Equal(t, "parent output", event.Content)
	default:
		t.Fatal("missing progress")
	}
	_, err = host.ToolProgress(t.Context(), &proto.ToolProgressEvent{ToolCallId: "parent", NestedCalls: `{`})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	select {
	case <-events:
		t.Fatal("invalid progress was forwarded")
	default:
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = host.ToolProgress(ctx, &proto.ToolProgressEvent{ToolCallId: "parent"})
	require.Equal(t, codes.Canceled, status.Code(err))
}

func TestGetConfigHonorsExplicitEmptyCLIValue(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	manager.SetSettings(&ai.Settings{Plugins: map[string]json.RawMessage{"plugin": json.RawMessage(`{"mode":"configured"}`)}})
	manager.ResolveCLIFlags("plugin", map[string]string{"mode": ""})
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("tau-plugin-name", "plugin"))
	response, err := manager.host.GetConfig(ctx, &proto.GetConfigRequest{Key: "mode"})
	require.NoError(t, err)
	require.True(t, response.Exists)
	require.Equal(t, `""`, response.Value)
}

func TestGetConfigSettingsOverrideRegistrationDefaults(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	manager.SetSettings(&ai.Settings{Plugins: map[string]json.RawMessage{"plugin": json.RawMessage(`{"mode":"configured","other":false}`)}})
	manager.registerPlugin(&PluginInfo{Name: "plugin", Registrations: Registrations{CLIFlags: []CLIFlag{{Name: "mode", DefaultValue: "default"}, {Name: "missing", DefaultValue: "fallback"}}}})
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("tau-plugin-name", "plugin"))
	response, err := manager.host.GetConfig(ctx, &proto.GetConfigRequest{Key: "mode"})
	require.NoError(t, err)
	require.Equal(t, `"configured"`, response.Value)
	response, err = manager.host.GetConfig(ctx, &proto.GetConfigRequest{Key: "missing"})
	require.NoError(t, err)
	require.True(t, response.Exists)
	require.Equal(t, `"fallback"`, response.Value)
	response, err = manager.host.GetConfig(ctx, &proto.GetConfigRequest{})
	require.NoError(t, err)
	require.JSONEq(t, `{"mode":"configured","other":false,"missing":"fallback"}`, response.Value)
}
