package plugin

import (
	"context"
	"fmt"
	"net"
	"strings"

	"code.forgejo.org/forgejo/runner/v12/act/container"
	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
)

type Client struct {
	conn *grpc.ClientConn
	rpc  pluginv1.BackendPluginClient
	caps *pluginv1.CapabilitiesResponse
}

func NewClient(ctx context.Context, address string) (*Client, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	if socketPath, ok := strings.CutPrefix(address, "unix://"); ok {
		opts = append(opts, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}))
		address = "passthrough:///" + socketPath
	}

	conn, err := grpc.NewClient(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial plugin at %s: %w", address, err)
	}

	rpc := pluginv1.NewBackendPluginClient(conn)

	healthClient := grpc_health_v1.NewHealthClient(conn)
	if _, err := healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("health check plugin at %s: %w", address, err)
	}

	caps, err := rpc.Capabilities(ctx, &pluginv1.CapabilitiesRequest{})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("get capabilities from plugin at %s: %w", address, err)
	}

	return &Client{
		conn: conn,
		rpc:  rpc,
		caps: caps,
	}, nil
}

func (c *Client) Capabilities() *pluginv1.CapabilitiesResponse {
	return c.caps
}

func (c *Client) NewEnvironment(input *container.NewContainerInput, backendOpts map[string]string) container.ExecutionsEnvironment {
	return &pluginEnvironment{
		client:      c.rpc,
		caps:        c.caps,
		backendOpts: backendOpts,
		input:       input,
		stdout:      input.Stdout,
		stderr:      input.Stderr,
	}
}

func (c *Client) Close() error {
	return c.conn.Close()
}
