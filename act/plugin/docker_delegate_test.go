//go:build !WITHOUT_DOCKER && (linux || darwin || windows || freebsd || openbsd)

package plugin

import (
	"context"
	"net"
	"testing"

	"code.forgejo.org/forgejo/runner/v12/act/container"
	"code.forgejo.org/forgejo/runner/v12/act/container/docker"
	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

// delegateMockServer returns caps with delegates_to_docker=true and a
// configurable Create response.
type delegateMockServer struct {
	pluginv1.UnimplementedBackendPluginServer

	createResp   *pluginv1.CreateResponse
	createErr    error
	removeCalled bool
}

func (s *delegateMockServer) Capabilities(_ context.Context, _ *pluginv1.CapabilitiesRequest) (*pluginv1.CapabilitiesResponse, error) {
	return &pluginv1.CapabilitiesResponse{
		Name:                  "vm-backend",
		RootPath:              "/var/run",
		ActPath:               "/var/run/act",
		ToolCachePath:         "/opt/hostedtoolcache",
		PathVariableName:      "PATH",
		DefaultPathVariable:   "/usr/bin:/bin",
		PathSeparator:         ":",
		SupportsDockerActions: true,
		ManagesOwnNetworking:  false,
		DelegatesToDocker:     true,
	}, nil
}

func (s *delegateMockServer) Create(_ context.Context, _ *pluginv1.CreateRequest) (*pluginv1.CreateResponse, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	return s.createResp, nil
}

func (s *delegateMockServer) Remove(_ context.Context, _ *pluginv1.RemoveRequest) (*pluginv1.RemoveResponse, error) {
	s.removeCalled = true
	return &pluginv1.RemoveResponse{}, nil
}

func startDelegateServer(t *testing.T) (*delegateMockServer, *grpc.ClientConn) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	mock := &delegateMockServer{}
	pluginv1.RegisterBackendPluginServer(srv, mock)

	healthSrv := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, healthSrv)
	healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return mock, conn
}

func newTestDelegateEnv(t *testing.T, conn *grpc.ClientConn) *dockerDelegateEnvironment {
	t.Helper()
	rpc := pluginv1.NewBackendPluginClient(conn)
	caps, err := rpc.Capabilities(t.Context(), &pluginv1.CapabilitiesRequest{})
	require.NoError(t, err)
	return newDockerDelegateEnvironment(rpc, caps, &container.NewContainerInput{
		Image: "alpine",
		Name:  "delegate-job",
	}, map[string]string{"ns": "default"})
}

func TestClientNewEnvironmentDispatchesOnCapability(t *testing.T) {
	_, conn := startDelegateServer(t)
	rpc := pluginv1.NewBackendPluginClient(conn)
	caps, err := rpc.Capabilities(t.Context(), &pluginv1.CapabilitiesRequest{})
	require.NoError(t, err)

	c := &Client{conn: conn, rpc: rpc, caps: caps}
	env := c.NewEnvironment(&container.NewContainerInput{Image: "alpine", Name: "j"}, nil)

	_, ok := env.(*dockerDelegateEnvironment)
	assert.True(t, ok, "expected *dockerDelegateEnvironment, got %T", env)
}

func TestDockerDelegateCapabilitiesReflectCaps(t *testing.T) {
	_, conn := startDelegateServer(t)
	d := newTestDelegateEnv(t, conn)

	assert.Equal(t, "vm-backend", d.BackendID())
	assert.True(t, d.SupportsDockerContainerActions())
	assert.False(t, d.ManagesOwnNetworking())
	assert.Equal(t, "delegate-job", d.GetName())
}

func TestDockerDelegatePullErrsOnMissingDelegateBlock(t *testing.T) {
	mock, conn := startDelegateServer(t)
	mock.createResp = &pluginv1.CreateResponse{EnvironmentId: "env-1"}
	d := newTestDelegateEnv(t, conn)

	err := d.Pull(false)(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no delegate block")
	// plugin.Create succeeded so we must release the env to avoid leaking it
	assert.True(t, mock.removeCalled)
}

func TestDockerDelegatePullErrsOnEmptyEndpoint(t *testing.T) {
	mock, conn := startDelegateServer(t)
	mock.createResp = &pluginv1.CreateResponse{
		EnvironmentId: "env-2",
		Delegate:      &pluginv1.DockerDelegate{Endpoint: ""},
	}
	d := newTestDelegateEnv(t, conn)

	err := d.Pull(false)(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint is empty")
	assert.True(t, mock.removeCalled)
}

func TestDockerDelegateImplementsHosting(t *testing.T) {
	_, conn := startDelegateServer(t)
	d := newTestDelegateEnv(t, conn)

	var h docker.Hosting = d
	assert.Nil(t, h.DockerEnv(), "DockerEnv must be nil before Pull")
}

func TestDockerDelegateRemoveBeforePullIsNoop(t *testing.T) {
	mock, conn := startDelegateServer(t)
	d := newTestDelegateEnv(t, conn)

	require.NoError(t, d.Remove()(t.Context()))
	assert.False(t, mock.removeCalled, "no plugin Remove RPC when nothing was provisioned")
}

func TestDockerDelegateContainerMethodsErrBeforePull(t *testing.T) {
	_, conn := startDelegateServer(t)
	d := newTestDelegateEnv(t, conn)

	err := d.Create(nil, nil)(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "before Pull")

	err = d.Start(false)(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "before Pull")

	_, err = d.IsHealthy(t.Context())
	require.Error(t, err)
}

func TestDockerTLSFromDelegateNilWhenEmpty(t *testing.T) {
	got := dockerTLSFromDelegate(&pluginv1.DockerDelegate{Endpoint: "tcp://x:1"})
	assert.Nil(t, got)
}

func TestDockerTLSFromDelegateMapsBytes(t *testing.T) {
	got := dockerTLSFromDelegate(&pluginv1.DockerDelegate{
		TlsCa:                 []byte("ca"),
		TlsCert:               []byte("cert"),
		TlsKey:                []byte("key"),
		TlsInsecureSkipVerify: true,
	})
	require.NotNil(t, got)
	assert.Equal(t, []byte("ca"), got.CA)
	assert.Equal(t, []byte("cert"), got.Cert)
	assert.Equal(t, []byte("key"), got.Key)
	assert.True(t, got.InsecureSkipVerify)
}

func TestDockerTLSFromDelegateInsecureOnly(t *testing.T) {
	got := dockerTLSFromDelegate(&pluginv1.DockerDelegate{TlsInsecureSkipVerify: true})
	require.NotNil(t, got)
	assert.True(t, got.InsecureSkipVerify)
}
