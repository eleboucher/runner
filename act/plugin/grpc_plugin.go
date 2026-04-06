package plugin

import (
	"context"

	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// BackendGRPCPlugin bridges go-plugin with the BackendPlugin gRPC service.
type BackendGRPCPlugin struct {
	goplugin.Plugin
	Impl pluginv1.BackendPluginServer // only set on the plugin (server) side

}

func (p *BackendGRPCPlugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	pluginv1.RegisterBackendPluginServer(s, p.Impl)
	return nil
}

func (p *BackendGRPCPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pluginv1.NewBackendPluginClient(c), nil
}
