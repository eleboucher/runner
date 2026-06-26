package plugin

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/container"
	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
)

// grpc.NewClient is non-blocking; defaultDialTimeout is applied to the
// first RPC (health check) when the caller's context has no deadline.
const defaultDialTimeout = 10 * time.Second

// 32 MiB lifts the stock 4 MiB gRPC cap so plugins can emit larger Exec
// frames and CopyOut chunks without ResourceExhausted.
const defaultMaxMessageSize = 32 * 1024 * 1024

const (
	defaultKeepaliveTime    = 30 * time.Second
	defaultKeepaliveTimeout = 10 * time.Second
)

type ClientOption func(*clientOptions)

type clientOptions struct {
	tlsConfig        *tls.Config
	allowPlainTCP    bool
	dialTimeout      time.Duration
	maxMessageSize   int
	keepaliveTime    time.Duration
	keepaliveTimeout time.Duration
}

func WithTLS(cfg *tls.Config) ClientOption {
	return func(o *clientOptions) { o.tlsConfig = cfg }
}

// WithAllowPlainTCP allows TCP without TLS, for trusted localhost or tests.
func WithAllowPlainTCP() ClientOption {
	return func(o *clientOptions) { o.allowPlainTCP = true }
}

func WithDialTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.dialTimeout = d }
}

// WithMaxMessageSize sets the per-call send/recv size limit in bytes.
func WithMaxMessageSize(bytes int) ClientOption {
	return func(o *clientOptions) { o.maxMessageSize = bytes }
}

// WithKeepalive sets the gRPC keepalive ping interval and ack timeout.
func WithKeepalive(interval, timeout time.Duration) ClientOption {
	return func(o *clientOptions) {
		o.keepaliveTime = interval
		o.keepaliveTimeout = timeout
	}
}

type Client struct {
	conn *grpc.ClientConn
	rpc  pluginv1.BackendPluginClient
	// caps is captured once; restart the runner if a plugin changes capabilities.
	caps *pluginv1.CapabilitiesResponse
}

func NewClient(ctx context.Context, address string, options ...ClientOption) (*Client, error) {
	cfg := clientOptions{
		dialTimeout:      defaultDialTimeout,
		maxMessageSize:   defaultMaxMessageSize,
		keepaliveTime:    defaultKeepaliveTime,
		keepaliveTimeout: defaultKeepaliveTimeout,
	}
	for _, opt := range options {
		opt(&cfg)
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.dialTimeout)
		defer cancel()
	}

	isUnix := strings.HasPrefix(address, "unix://")

	creds, err := transportCredentials(isUnix, &cfg)
	if err != nil {
		return nil, err
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(cfg.maxMessageSize),
			grpc.MaxCallSendMsgSize(cfg.maxMessageSize),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                cfg.keepaliveTime,
			Timeout:             cfg.keepaliveTimeout,
			PermitWithoutStream: false,
		}),
	}
	if isUnix {
		socketPath := strings.TrimPrefix(address, "unix://")
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
	healthResp, err := healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("health check plugin at %s: %w", address, err)
	}
	if got := healthResp.GetStatus(); got != grpc_health_v1.HealthCheckResponse_SERVING {
		conn.Close()
		return nil, fmt.Errorf("health check plugin at %s: status %s", address, got)
	}

	caps, err := rpc.Capabilities(ctx, &pluginv1.CapabilitiesRequest{})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("get capabilities from plugin at %s: %w", address, err)
	}
	if err := validateCapabilities(caps); err != nil {
		conn.Close()
		return nil, fmt.Errorf("plugin at %s: %w", address, err)
	}

	return &Client{
		conn: conn,
		rpc:  rpc,
		caps: caps,
	}, nil
}

func transportCredentials(isUnix bool, cfg *clientOptions) (credentials.TransportCredentials, error) {
	switch {
	case cfg.tlsConfig != nil:
		return credentials.NewTLS(cfg.tlsConfig), nil
	case isUnix, cfg.allowPlainTCP:
		return insecure.NewCredentials(), nil
	default:
		return nil, fmt.Errorf("plugin: TCP address requires WithTLS or WithAllowPlainTCP")
	}
}

// ProtocolVersion is the plugin protocol this runner speaks.
const ProtocolVersion uint32 = 1

func validateCapabilities(caps *pluginv1.CapabilitiesResponse) error {
	if v := caps.GetProtocolVersion(); v != ProtocolVersion {
		return fmt.Errorf("unsupported plugin protocol version %d (runner speaks %d)", v, ProtocolVersion)
	}
	missing := []string{}
	if caps.GetName() == "" {
		missing = append(missing, "name")
	}
	if caps.GetRootPath() == "" {
		missing = append(missing, "root_path")
	}
	if caps.GetActPath() == "" {
		missing = append(missing, "act_path")
	}
	if len(missing) > 0 {
		return fmt.Errorf("capabilities response missing required fields: %s", strings.Join(missing, ", "))
	}
	return nil
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
