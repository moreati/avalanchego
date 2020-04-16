// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package rpcchainvm

import (
	"golang.org/x/net/context"

	"google.golang.org/grpc"

	"github.com/hashicorp/go-plugin"

	"github.com/ava-labs/gecko/snow/engine/snowman"
	"github.com/ava-labs/gecko/vms/rpcchainvm/proto"
)

// Handshake is a common handshake that is shared by plugin and host.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "VM_PLUGIN",
	MagicCookieValue: "dynamic",
}

// PluginMap is the map of plugins we can dispense.
var PluginMap = map[string]plugin.Plugin{
	"vm": &Plugin{},
}

// Plugin is the implementation of plugin.Plugin so we can serve/consume this.
// We also implement GRPCPlugin so that this plugin can be served over gRPC.
type Plugin struct {
	plugin.NetRPCUnsupportedPlugin
	// Concrete implementation, written in Go. This is only used for plugins
	// that are written in Go.
	vm snowman.ChainVM
}

// New ...
func New(vm snowman.ChainVM) *Plugin { return &Plugin{vm: vm} }

// GRPCServer ...
func (p *Plugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	proto.RegisterVMServer(s, NewServer(p.vm, broker))
	return nil
}

// GRPCClient ...
func (p *Plugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return NewClient(proto.NewVMClient(c), broker), nil
}
