package plugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
	goplugin "github.com/hashicorp/go-plugin"
)

// ClientV2Option configures NewClientV2.
type ClientV2Option func(*clientV2Options)

type clientV2Options struct {
	logLevel string
}

// WithLogLevel sets FORGEJO_RUNNER_LOG_LEVEL on the plugin process so it can
// align its logger with the runner. Empty is a no-op.
func WithLogLevel(level string) ClientV2Option {
	return func(o *clientV2Options) { o.logLevel = level }
}

func buildPluginCmd(binaryPath string, cfg clientV2Options) *exec.Cmd {
	cmd := exec.Command(binaryPath)
	if cfg.logLevel != "" {
		cmd.Env = append(os.Environ(), "FORGEJO_RUNNER_LOG_LEVEL="+cfg.logLevel)
	}
	return cmd
}

func NewClientV2(ctx context.Context, binaryPath string, opts ...ClientV2Option) (*Client, error) {
	cfg := clientV2Options{}
	for _, opt := range opts {
		opt(&cfg)
	}

	gpClient := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: Handshake,
		Plugins: map[string]goplugin.Plugin{
			PluginName: &BackendGRPCPlugin{},
		},
		Cmd:              buildPluginCmd(binaryPath, cfg),
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
