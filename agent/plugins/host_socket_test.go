package plugins

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/losinggeneration/tau/agent/plugins/proto"
	"github.com/losinggeneration/tau/ai"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestHostSocketRejectsOtherPluginIdentity(t *testing.T) {
	for _, phase := range []string{"startup", "active"} {
		t.Run(phase, func(t *testing.T) {
			manager := NewManager(t.TempDir(), nil, nil)
			manager.SetSettings(&ai.Settings{Plugins: map[string]json.RawMessage{"other": json.RawMessage(`{"secret":"private"}`)}})
			ctx := t.Context()
			if phase == "startup" {
				ctx = context.WithValue(ctx, stagedHostContextKey{}, true)
			}
			path, err := manager.prepareHostSocket(ctx, "owner")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, manager.closeHostSocketLocked("owner")) })
			connection, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			defer func() { require.NoError(t, connection.Close()) }()
			host := proto.NewHostClient(connection)
			for _, names := range [][]string{nil, {"other"}, {"owner", "other"}} {
				rpcCtx, cancel := context.WithTimeout(t.Context(), time.Second)
				var pairs []string
				for _, name := range names {
					pairs = append(pairs, "tau-plugin-name", name)
				}
				rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs(pairs...))
				response, err := host.GetConfig(rpcCtx, &proto.GetConfigRequest{Key: "secret"})
				cancel()
				require.Equal(t, codes.PermissionDenied, status.Code(err))
				require.Nil(t, response)
			}
			rpcCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(t.Context(), metadata.Pairs("tau-plugin-name", "other")), time.Second)
			defer cancel()
			_, err = host.StateSet(rpcCtx, &proto.StateSetRequest{Key: "key", Value: `"changed"`})
			require.Equal(t, codes.PermissionDenied, status.Code(err))
			require.NoDirExists(t, filepath.Join(manager.stateDir, "other"))
			validCtx, validCancel := context.WithTimeout(metadata.NewOutgoingContext(t.Context(), metadata.Pairs("tau-plugin-name", "owner")), time.Second)
			defer validCancel()
			response, err := host.GetConfig(validCtx, &proto.GetConfigRequest{Key: "secret"})
			require.NoError(t, err)
			require.False(t, response.Exists)
		})
	}
}

func TestStartupHostOwnsConfigurationSnapshot(t *testing.T) {
	manager := NewManager(t.TempDir(), nil, nil)
	original := json.RawMessage(`{"mode":"original"}`)
	settings := &ai.Settings{Plugins: map[string]json.RawMessage{"owner": original}}
	manager.SetSettings(settings)
	manager.ResolveCLIFlags("owner", map[string]string{"cli": "original"})
	path, err := manager.prepareHostSocket(context.WithValue(t.Context(), stagedHostContextKey{}, true), "owner")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.closeHostSocketLocked("owner")) })
	copy(original, []byte(`{"mode":"modified"}`))
	settings.Plugins["owner"] = json.RawMessage(`{"mode":"replacement"}`)
	manager.ResolveCLIFlags("owner", map[string]string{"cli": "replacement"})
	connection, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer func() { require.NoError(t, connection.Close()) }()
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(t.Context(), metadata.Pairs("tau-plugin-name", "owner")), time.Second)
	defer cancel()
	response, err := proto.NewHostClient(connection).GetConfig(ctx, &proto.GetConfigRequest{})
	require.NoError(t, err)
	require.JSONEq(t, `{"mode":"original","cli":"original"}`, response.Value)
}
