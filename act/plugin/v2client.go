package plugin

import (
	"context"
	"fmt"
	"os/exec"

	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
	goplugin "github.com/hashicorp/go-plugin"
)

func NewClientV2(ctx context.Context, binaryPath string) (*Client, error) {
	gpClient := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: Handshake,
		Plugins: map[string]goplugin.Plugin{
			PluginName: &BackendGRPCPlugin{},
		},
		Cmd:              exec.Command(binaryPath),
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
	})

	rpcClient, err := gpClient.Client()
	if err != nil {
		gpClient.Kill()
		return nil, fmt.Errorf("start plugin %s: %w", binaryPath, err)
	}

	raw, err := rpcClient.Dispense(PluginName)
	if err != nil {
		gpClient.Kill()
		return nil, fmt.Errorf("dispense plugin %s: %w", binaryPath, err)
	}

	rpc, ok := raw.(pluginv1.BackendPluginClient)
	if !ok {
		gpClient.Kill()
		return nil, fmt.Errorf("plugin %s: dispensed object does not implement BackendPluginClient", binaryPath)
	}

	caps, err := rpc.Capabilities(ctx, &pluginv1.CapabilitiesRequest{})
	if err != nil {
		gpClient.Kill()
		return nil, fmt.Errorf("get capabilities from plugin %s: %w", binaryPath, err)
	}

	return &Client{
		gpClient: gpClient,
		rpc:      rpc,
		caps:     caps,
	}, nil
}
