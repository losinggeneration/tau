package plugins

import (
	"context"
	"fmt"
	"net/rpc"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// tauPluginSet declares the go-plugin plugin names/protocol versions that tau
// can negotiate with subprocess plugins. The manager talks to the plugin gRPC
// service directly after negotiation, so the client-side GRPCClient is not used.
func tauPluginSet() plugin.PluginSet {
	return plugin.PluginSet{
		"tau": &tauGRPCPlugin{},
	}
}

type tauGRPCPlugin struct {
	plugin.NetRPCUnsupportedPlugin
}

func (p *tauGRPCPlugin) GRPCServer(*plugin.GRPCBroker, *grpc.Server) error {
	return fmt.Errorf("tauGRPCPlugin is host-side only")
}

func (p *tauGRPCPlugin) GRPCClient(context.Context, *plugin.GRPCBroker, *grpc.ClientConn) (interface{}, error) {
	return nil, fmt.Errorf("tauGRPCPlugin is negotiated directly by manager")
}

func (p *tauGRPCPlugin) Client(*plugin.MuxBroker, *rpc.Client) (interface{}, error) {
	return nil, fmt.Errorf("tau plugin net/rpc protocol is not supported")
}
