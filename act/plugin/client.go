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

func resolveOptions(options []ClientOption) *clientOptions {
	cfg := &clientOptions{
		dialTimeout:      defaultDialTimeout,
		maxMessageSize:   defaultMaxMessageSize,
		keepaliveTime:    defaultKeepaliveTime,
		keepaliveTimeout: defaultKeepaliveTimeout,
	}
	for _, opt := range options {
		opt(cfg)
	}
	return cfg
}

// withDialTimeout applies cfg.dialTimeout when the caller's context has none, so
// a non-blocking grpc.NewClient still fails fast on the first RPC.
func withDialTimeout(ctx context.Context, cfg *clientOptions) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, cfg.dialTimeout)
}

// dial opens a connection to a plugin and confirms its health service is
// SERVING. It is shared by the backend and Docker-tunnel clients; the caller
// picks the gRPC service to talk over the returned connection.
func dial(ctx context.Context, address string, cfg *clientOptions) (*grpc.ClientConn, error) {
	isUnix := strings.HasPrefix(address, "unix://")

	creds, err := transportCredentials(isUnix, cfg)
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

	return conn, nil
}

// NewClient connects to a BackendPlugin: a plugin that owns the whole execution
// environment and every container operation within it.
func NewClient(ctx context.Context, address string, options ...ClientOption) (*Client, error) {
	cfg := resolveOptions(options)
	ctx, cancel := withDialTimeout(ctx, cfg)
	defer cancel()

	conn, err := dial(ctx, address, cfg)
	if err != nil {
		return nil, err
	}

	rpc := pluginv1.NewBackendPluginClient(conn)
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

// TunnelClient talks to a DockerTunnelPlugin: a plugin that boots an
// environment exposing a Docker daemon and hands the runner the endpoint to
// drive containers against.
type TunnelClient struct {
	conn *grpc.ClientConn
	rpc  pluginv1.DockerTunnelPluginClient
}

// NewTunnelClient connects to a DockerTunnelPlugin.
func NewTunnelClient(ctx context.Context, address string, options ...ClientOption) (*TunnelClient, error) {
	cfg := resolveOptions(options)
	ctx, cancel := withDialTimeout(ctx, cfg)
	defer cancel()

	conn, err := dial(ctx, address, cfg)
	if err != nil {
		return nil, err
	}

	rpc := pluginv1.NewDockerTunnelPluginClient(conn)
	caps, err := rpc.Capabilities(ctx, &pluginv1.CapabilitiesRequest{})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("get capabilities from plugin at %s: %w", address, err)
	}
	if err := validateTunnelCapabilities(caps); err != nil {
		conn.Close()
		return nil, fmt.Errorf("plugin at %s: %w", address, err)
	}

	return &TunnelClient{
		conn: conn,
		rpc:  rpc,
	}, nil
}

func validateTunnelCapabilities(caps *pluginv1.TunnelCapabilitiesResponse) error {
	if v := caps.GetProtocolVersion(); v != ProtocolVersion {
		return fmt.Errorf("unsupported plugin protocol version %d (runner speaks %d)", v, ProtocolVersion)
	}
	if caps.GetName() == "" {
		return fmt.Errorf("capabilities response missing required fields: name")
	}
	return nil
}

// DelegateEnvironment describes a Docker daemon a tunnel plug-in has
// provisioned (e.g. one running inside a VM it booted). The runner dials
// Endpoint and drives every container against it; EnvironmentID is the handle
// passed back to RemoveExecutionEnvironment to tear the daemon down.
type DelegateEnvironment struct {
	EnvironmentID         string
	Endpoint              string
	TLSCA                 []byte
	TLSCert               []byte
	TLSKey                []byte
	TLSInsecureSkipVerify bool
}

// CreateExecutionEnvironment asks a tunnel plug-in to provision its environment
// and return the Docker endpoint to drive containers against.
func (c *TunnelClient) CreateExecutionEnvironment(ctx context.Context, backendOpts map[string]string, platform string, forcePull bool) (*DelegateEnvironment, error) {
	req := &pluginv1.TunnelCreateRequest{
		BackendOptions: backendOpts,
		ForcePull:      forcePull,
	}
	if platform != "" {
		req.Platform = &platform
	}
	resp, err := c.rpc.Create(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("plugin create: %w", err)
	}
	del := resp.GetDelegate()
	if del == nil {
		_, _ = c.rpc.Remove(ctx, &pluginv1.RemoveRequest{EnvironmentId: resp.GetEnvironmentId()})
		return nil, fmt.Errorf("plugin returned no delegate block")
	}
	if del.GetEndpoint() == "" {
		_, _ = c.rpc.Remove(ctx, &pluginv1.RemoveRequest{EnvironmentId: resp.GetEnvironmentId()})
		return nil, fmt.Errorf("plugin delegate endpoint is empty")
	}
	return &DelegateEnvironment{
		EnvironmentID:         resp.GetEnvironmentId(),
		Endpoint:              del.GetEndpoint(),
		TLSCA:                 del.GetTlsCa(),
		TLSCert:               del.GetTlsCert(),
		TLSKey:                del.GetTlsKey(),
		TLSInsecureSkipVerify: del.GetTlsInsecureSkipVerify(),
	}, nil
}

// RemoveExecutionEnvironment tears down an environment provisioned by
// CreateExecutionEnvironment.
func (c *TunnelClient) RemoveExecutionEnvironment(ctx context.Context, environmentID string) error {
	_, err := c.rpc.Remove(ctx, &pluginv1.RemoveRequest{EnvironmentId: environmentID})
	return err
}

func (c *TunnelClient) Close() error {
	return c.conn.Close()
}
