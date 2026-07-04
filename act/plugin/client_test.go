package plugin

import (
	"context"
	"crypto/tls"
	"net"
	"testing"

	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

type healthOnlyServer struct {
	pluginv1.UnimplementedBackendPluginServer
	caps *pluginv1.CapabilitiesResponse
}

func (h *healthOnlyServer) Capabilities(_ context.Context, _ *pluginv1.CapabilitiesRequest) (*pluginv1.CapabilitiesResponse, error) {
	return h.caps, nil
}

func startListener(t *testing.T, srv *grpc.Server, healthStatus grpc_health_v1.HealthCheckResponse_ServingStatus) net.Listener {
	t.Helper()
	healthSrv := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, healthSrv)
	healthSrv.SetServingStatus("", healthStatus)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis
}

func TestNewClient_RejectsPlainTCPByDefault(t *testing.T) {
	_, err := NewClient(t.Context(), "127.0.0.1:1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TCP address requires WithTLS or WithAllowPlainTCP")
}

func TestNewClient_AcceptsTCPWithAllowPlainTCP(t *testing.T) {
	srv := grpc.NewServer()
	pluginv1.RegisterBackendPluginServer(srv, &healthOnlyServer{
		caps: &pluginv1.CapabilitiesResponse{ProtocolVersion: ProtocolVersion, Name: "tcp", RootPath: "/r", ActPath: "/r/act"},
	})
	lis := startListener(t, srv, grpc_health_v1.HealthCheckResponse_SERVING)

	c, err := NewClient(t.Context(), lis.Addr().String(), WithAllowPlainTCP())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	assert.Equal(t, "tcp", c.Capabilities().GetName())
}

func TestNewClient_AcceptsTLSConfig(t *testing.T) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13}
	creds, err := transportCredentials(false, &clientOptions{tlsConfig: cfg})
	require.NoError(t, err)
	assert.NotNil(t, creds)
}

func TestNewClient_RejectsNotServingHealth(t *testing.T) {
	srv := grpc.NewServer()
	pluginv1.RegisterBackendPluginServer(srv, &healthOnlyServer{
		caps: &pluginv1.CapabilitiesResponse{Name: "x", RootPath: "/r", ActPath: "/r/act"},
	})
	lis := startListener(t, srv, grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	_, err := NewClient(t.Context(), lis.Addr().String(), WithAllowPlainTCP())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status NOT_SERVING")
}

func TestNewClient_RejectsIncompleteCapabilities(t *testing.T) {
	srv := grpc.NewServer()
	pluginv1.RegisterBackendPluginServer(srv, &healthOnlyServer{
		caps: &pluginv1.CapabilitiesResponse{ProtocolVersion: ProtocolVersion, Name: "incomplete"},
	})
	lis := startListener(t, srv, grpc_health_v1.HealthCheckResponse_SERVING)

	_, err := NewClient(t.Context(), lis.Addr().String(), WithAllowPlainTCP())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required fields")
	assert.Contains(t, err.Error(), "root_path")
	assert.Contains(t, err.Error(), "act_path")
}

func TestNewClient_RejectsUnsupportedProtocolVersion(t *testing.T) {
	srv := grpc.NewServer()
	pluginv1.RegisterBackendPluginServer(srv, &healthOnlyServer{
		caps: &pluginv1.CapabilitiesResponse{ProtocolVersion: ProtocolVersion + 1, Name: "x", RootPath: "/r", ActPath: "/r/act"},
	})
	lis := startListener(t, srv, grpc_health_v1.HealthCheckResponse_SERVING)

	_, err := NewClient(t.Context(), lis.Addr().String(), WithAllowPlainTCP())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported plugin protocol version")
}

// tunnelServer is a minimal reference for a DockerTunnelPlugin: it returns a
// Docker endpoint + TLS material from Create.
type tunnelServer struct {
	pluginv1.UnimplementedDockerTunnelPluginServer
	caps       *pluginv1.TunnelCapabilitiesResponse
	createResp *pluginv1.TunnelCreateResponse
	createReq  *pluginv1.TunnelCreateRequest
	removed    []string
}

func (d *tunnelServer) Capabilities(context.Context, *pluginv1.CapabilitiesRequest) (*pluginv1.TunnelCapabilitiesResponse, error) {
	return d.caps, nil
}

func (d *tunnelServer) Create(_ context.Context, req *pluginv1.TunnelCreateRequest) (*pluginv1.TunnelCreateResponse, error) {
	d.createReq = req
	return d.createResp, nil
}

func (d *tunnelServer) Remove(_ context.Context, req *pluginv1.RemoveRequest) (*pluginv1.RemoveResponse, error) {
	d.removed = append(d.removed, req.GetEnvironmentId())
	return &pluginv1.RemoveResponse{}, nil
}

func tunnelCaps() *pluginv1.TunnelCapabilitiesResponse {
	return &pluginv1.TunnelCapabilitiesResponse{
		ProtocolVersion: ProtocolVersion,
		Name:            "vm",
	}
}

func TestTunnelClient_CreateExecutionEnvironment(t *testing.T) {
	srv := grpc.NewServer()
	server := &tunnelServer{
		caps: tunnelCaps(),
		createResp: &pluginv1.TunnelCreateResponse{
			EnvironmentId: "env-1",
			Delegate: &pluginv1.DockerDelegate{
				Endpoint: "tcp://10.0.0.2:2376",
				TlsCa:    []byte("ca"),
				TlsCert:  []byte("cert"),
				TlsKey:   []byte("key"),
			},
		},
	}
	pluginv1.RegisterDockerTunnelPluginServer(srv, server)
	lis := startListener(t, srv, grpc_health_v1.HealthCheckResponse_SERVING)

	c, err := NewTunnelClient(t.Context(), lis.Addr().String(), WithAllowPlainTCP())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	del, err := c.CreateExecutionEnvironment(t.Context(), nil, "linux/arm64", false)
	require.NoError(t, err)
	assert.Equal(t, "linux/arm64", server.createReq.GetPlatform())
	assert.Equal(t, "env-1", del.EnvironmentID)
	assert.Equal(t, "tcp://10.0.0.2:2376", del.Endpoint)
	assert.Equal(t, []byte("ca"), del.TLSCA)
	assert.Equal(t, []byte("cert"), del.TLSCert)
	assert.Equal(t, []byte("key"), del.TLSKey)

	require.NoError(t, c.RemoveExecutionEnvironment(t.Context(), del.EnvironmentID))
	assert.Equal(t, []string{"env-1"}, server.removed)
}

func TestTunnelClient_CreateExecutionEnvironment_RejectsMissingDelegate(t *testing.T) {
	srv := grpc.NewServer()
	pluginv1.RegisterDockerTunnelPluginServer(srv, &tunnelServer{
		caps:       tunnelCaps(),
		createResp: &pluginv1.TunnelCreateResponse{EnvironmentId: "env-2"}, // no delegate block
	})
	lis := startListener(t, srv, grpc_health_v1.HealthCheckResponse_SERVING)

	c, err := NewTunnelClient(t.Context(), lis.Addr().String(), WithAllowPlainTCP())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	_, err = c.CreateExecutionEnvironment(t.Context(), nil, "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no delegate block")
}
