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
