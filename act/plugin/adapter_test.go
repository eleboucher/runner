package plugin

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/container"
	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1024 * 1024

type mockPluginServer struct {
	pluginv1.UnimplementedBackendPluginServer

	createReq    *pluginv1.CreateRequest
	removeCalled bool

	execExitCode int32
	execError    string
	execStdout   string
	execStderr   string

	copyInData    []byte
	copyInDest    string
	copyOutData   []byte
	updateEnvResp map[string]string
	startImageEnv map[string]string
}

func (s *mockPluginServer) Capabilities(_ context.Context, _ *pluginv1.CapabilitiesRequest) (*pluginv1.CapabilitiesResponse, error) {
	return &pluginv1.CapabilitiesResponse{
		Name:                      "test-backend",
		RootPath:                  "/test/root",
		ActPath:                   "/test/root/act",
		ToolCachePath:             "/test/root/toolcache",
		PathVariableName:          "PATH",
		DefaultPathVariable:       "/usr/bin:/bin",
		PathSeparator:             ":",
		SupportsDockerActions:     false,
		ManagesOwnNetworking:     true,
		SupportsServiceContainers: true,
		RunnerContext: map[string]string{
			"os":   "Linux",
			"arch": "x86_64",
			"temp": "/tmp",
		},
	}, nil
}

func (s *mockPluginServer) Create(_ context.Context, req *pluginv1.CreateRequest) (*pluginv1.CreateResponse, error) {
	s.createReq = req
	return &pluginv1.CreateResponse{EnvironmentId: "test-env-123"}, nil
}

func (s *mockPluginServer) Start(_ context.Context, _ *pluginv1.StartRequest) (*pluginv1.StartResponse, error) {
	return &pluginv1.StartResponse{ImageEnv: s.startImageEnv}, nil
}

func (s *mockPluginServer) Exec(_ *pluginv1.ExecRequest, stream grpc.ServerStreamingServer[pluginv1.ExecOutput]) error {
	if s.execStdout != "" {
		_ = stream.Send(&pluginv1.ExecOutput{
			Stream: pluginv1.ExecOutput_STDOUT,
			Data:   []byte(s.execStdout),
		})
	}
	if s.execStderr != "" {
		_ = stream.Send(&pluginv1.ExecOutput{
			Stream: pluginv1.ExecOutput_STDERR,
			Data:   []byte(s.execStderr),
		})
	}
	_ = stream.Send(&pluginv1.ExecOutput{
		Done:         true,
		ExitCode:     s.execExitCode,
		ErrorMessage: s.execError,
	})
	return nil
}

func (s *mockPluginServer) CopyIn(stream grpc.ClientStreamingServer[pluginv1.CopyInChunk, pluginv1.CopyInResponse]) error {
	var buf bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if s.copyInDest == "" {
			s.copyInDest = chunk.GetDestPath()
		}
		buf.Write(chunk.GetData())
	}
	s.copyInData = buf.Bytes()
	return stream.SendAndClose(&pluginv1.CopyInResponse{})
}

func (s *mockPluginServer) CopyOut(_ *pluginv1.CopyOutRequest, stream grpc.ServerStreamingServer[pluginv1.CopyOutChunk]) error {
	if s.copyOutData != nil {
		_ = stream.Send(&pluginv1.CopyOutChunk{Data: s.copyOutData})
	}
	return nil
}

func (s *mockPluginServer) UpdateEnv(_ context.Context, _ *pluginv1.UpdateEnvRequest) (*pluginv1.UpdateEnvResponse, error) {
	return &pluginv1.UpdateEnvResponse{UpdatedEnv: s.updateEnvResp}, nil
}

func (s *mockPluginServer) IsHealthy(_ context.Context, _ *pluginv1.IsHealthyRequest) (*pluginv1.IsHealthyResponse, error) {
	return &pluginv1.IsHealthyResponse{WaitNanos: 0}, nil
}

func (s *mockPluginServer) Remove(_ context.Context, _ *pluginv1.RemoveRequest) (*pluginv1.RemoveResponse, error) {
	s.removeCalled = true
	return &pluginv1.RemoveResponse{}, nil
}

func startMockServer(t *testing.T) (*mockPluginServer, *grpc.ClientConn) {
	t.Helper()

	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	mock := &mockPluginServer{
		execStdout: "hello\n",
	}
	pluginv1.RegisterBackendPluginServer(srv, mock)

	healthSrv := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, healthSrv)
	healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	go func() {
		_ = srv.Serve(lis)
	}()
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

func newTestEnv(t *testing.T, conn *grpc.ClientConn) *pluginEnvironment {
	t.Helper()
	rpc := pluginv1.NewBackendPluginClient(conn)
	caps, err := rpc.Capabilities(t.Context(), &pluginv1.CapabilitiesRequest{})
	require.NoError(t, err)
	return &pluginEnvironment{
		client: rpc,
		caps:   caps,
		input: &container.NewContainerInput{
			Image:      "test:latest",
			Name:       "test-container",
			Env:        []string{"FOO=bar"},
			WorkingDir: "/workspace",
		},
		backendOpts: map[string]string{"ns": "default"},
		stdout:      io.Discard,
		stderr:      io.Discard,
	}
}


func TestPluginEnvironment_Capabilities(t *testing.T) {
	_, conn := startMockServer(t)
	env := newTestEnv(t, conn)

	assert.Equal(t, "test-backend", env.BackendName())
	assert.Equal(t, "test-backend", env.GetName())
	assert.Equal(t, "/test/root", env.GetRoot())
	assert.Equal(t, "/test/root/act", env.GetActPath())
	assert.False(t, env.SupportsDockerActions())
	assert.True(t, env.ManagesOwnNetworking())
	assert.False(t, env.IsEnvironmentCaseInsensitive())
	assert.Equal(t, "PATH", env.GetPathVariableName())
	assert.Equal(t, "/usr/bin:/bin", env.DefaultPathVariable())
	assert.Equal(t, "/a:/b", env.JoinPathVariable("/a", "/b"))
	assert.Equal(t, "/some/path", env.ToContainerPath("/some/path"))

	rc := env.GetRunnerContext(t.Context())
	assert.Equal(t, "Linux", rc["os"])
	assert.Equal(t, "x86_64", rc["arch"])
	assert.Equal(t, "/tmp", rc["temp"])
}

func TestPluginEnvironment_CapabilitiesDefaults(t *testing.T) {
	_, conn := startMockServer(t)
	rpc := pluginv1.NewBackendPluginClient(conn)
	env := &pluginEnvironment{
		client: rpc,
		caps:   &pluginv1.CapabilitiesResponse{},
		input:  &container.NewContainerInput{},
		stdout: io.Discard,
		stderr: io.Discard,
	}

	assert.Equal(t, "PATH", env.GetPathVariableName())
	assert.Equal(t, "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", env.DefaultPathVariable())
	assert.Equal(t, "/a:/b", env.JoinPathVariable("/a", "/b"))
}

func TestPluginEnvironment_CreatePassesInput(t *testing.T) {
	mock, conn := startMockServer(t)
	env := newTestEnv(t, conn)

	env.AddServiceContainerRaw("redis", "redis:7", map[string]string{"REDIS_PASS": "secret"}, []string{"6379"})

	err := env.Create([]string{"NET_ADMIN"}, []string{"MKNOD"})(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "test-env-123", env.envID)

	req := mock.createReq
	require.NotNil(t, req)
	assert.Equal(t, "test:latest", req.Image)
	assert.Equal(t, "test-container", req.Name)
	assert.Equal(t, []string{"FOO=bar"}, req.Env)
	assert.Equal(t, "/workspace", req.WorkingDir)
	assert.Equal(t, []string{"NET_ADMIN"}, req.CapAdd)
	assert.Equal(t, []string{"MKNOD"}, req.CapDrop)
	assert.Equal(t, "default", req.BackendOptions["ns"])

	require.Len(t, req.Services, 1)
	assert.Equal(t, "redis", req.Services[0].Name)
	assert.Equal(t, "redis:7", req.Services[0].Image)
	assert.Equal(t, "secret", req.Services[0].Env["REDIS_PASS"])
	assert.Equal(t, []string{"6379"}, req.Services[0].Ports)
}

func TestPluginEnvironment_Lifecycle(t *testing.T) {
	mock, conn := startMockServer(t)
	env := newTestEnv(t, conn)

	require.NoError(t, env.Create(nil, nil)(t.Context()))
	require.NoError(t, env.Start(false)(t.Context()))

	var stdout bytes.Buffer
	env.ReplaceLogWriter(&stdout, io.Discard)

	require.NoError(t, env.Exec([]string{"echo", "hello"}, nil, "", "")(t.Context()))
	assert.Equal(t, "hello\n", stdout.String())

	wait, err := env.IsHealthy(t.Context())
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), wait)

	require.NoError(t, env.Remove()(t.Context()))
	assert.True(t, mock.removeCalled)
}

func TestPluginEnvironment_ExecStderr(t *testing.T) {
	mock, conn := startMockServer(t)
	mock.execStdout = ""
	mock.execStderr = "warning: something\n"

	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))

	var stderr bytes.Buffer
	env.ReplaceLogWriter(io.Discard, &stderr)

	require.NoError(t, env.Exec([]string{"cmd"}, nil, "", "")(t.Context()))
	assert.Equal(t, "warning: something\n", stderr.String())
}

func TestPluginEnvironment_ExecNonZeroExit(t *testing.T) {
	mock, conn := startMockServer(t)
	mock.execStdout = ""
	mock.execExitCode = 1
	mock.execError = ""

	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))

	err := env.Exec([]string{"false"}, nil, "", "")(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit code 1")
}

func TestPluginEnvironment_ExecErrorMessage(t *testing.T) {
	mock, conn := startMockServer(t)
	mock.execStdout = ""
	mock.execExitCode = 127
	mock.execError = "exec: command not found"

	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))

	err := env.Exec([]string{"nonexistent"}, nil, "", "")(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command not found")
	assert.NotContains(t, err.Error(), "exit code")
}

func TestPluginEnvironment_Copy(t *testing.T) {
	mock, conn := startMockServer(t)
	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))

	err := env.Copy("/dest", &container.FileEntry{
		Name: "hello.txt",
		Mode: 0o644,
		Body: "hello world",
	})(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "/dest", mock.copyInDest)
	require.NotEmpty(t, mock.copyInData)

	// Verify tar content
	tr := tar.NewReader(bytes.NewReader(mock.copyInData))
	hdr, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "hello.txt", hdr.Name)
	assert.Equal(t, int64(0o644), hdr.Mode)

	body, err := io.ReadAll(tr)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(body))
}

func TestPluginEnvironment_CopyTarStream(t *testing.T) {
	mock, conn := startMockServer(t)
	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "file.txt", Size: 4, Mode: 0o644})
	_, _ = tw.Write([]byte("data"))
	_ = tw.Close()

	err := env.CopyTarStream(t.Context(), "/tar-dest", &buf)
	require.NoError(t, err)
	assert.Equal(t, "/tar-dest", mock.copyInDest)
}

func TestPluginEnvironment_GetContainerArchive(t *testing.T) {
	mock, conn := startMockServer(t)
	mock.copyOutData = []byte("tar-archive-bytes")

	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))

	rc, err := env.GetContainerArchive(t.Context(), "/src/file.txt")
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "tar-archive-bytes", string(data))
}

func TestPluginEnvironment_UpdateFromEnv(t *testing.T) {
	mock, conn := startMockServer(t)
	mock.updateEnvResp = map[string]string{"FOO": "bar", "NEW": "value"}

	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))

	envMap := map[string]string{"FOO": "old"}
	err := env.UpdateFromEnv("/path/to/env", &envMap)(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "bar", envMap["FOO"])
	assert.Equal(t, "value", envMap["NEW"])
}

func TestPluginEnvironment_ReplaceLogWriter(t *testing.T) {
	_, conn := startMockServer(t)
	env := newTestEnv(t, conn)

	var w1, w2 bytes.Buffer
	env.stdout = &w1
	env.stderr = &w2

	var w3, w4 bytes.Buffer
	oldOut, oldErr := env.ReplaceLogWriter(&w3, &w4)
	assert.Equal(t, &w1, oldOut)
	assert.Equal(t, &w2, oldErr)

	env.mu.Lock()
	assert.Equal(t, &w3, env.stdout)
	assert.Equal(t, &w4, env.stderr)
	env.mu.Unlock()
}

func TestPluginEnvironment_NoOpMethods(t *testing.T) {
	_, conn := startMockServer(t)
	env := newTestEnv(t, conn)

	require.NoError(t, env.Pull(false)(t.Context()))
	require.NoError(t, env.ConnectToNetwork("net")(t.Context()))
	require.NoError(t, env.Close()(t.Context()))

	envMap := map[string]string{}
	require.NoError(t, env.UpdateFromImageEnv(&envMap)(t.Context()))
}

func TestClient_HealthCheckAndCapabilities(t *testing.T) {
	_, conn := startMockServer(t)

	healthClient := grpc_health_v1.NewHealthClient(conn)
	resp, err := healthClient.Check(t.Context(), &grpc_health_v1.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, grpc_health_v1.HealthCheckResponse_SERVING, resp.GetStatus())

	rpc := pluginv1.NewBackendPluginClient(conn)
	caps, err := rpc.Capabilities(t.Context(), &pluginv1.CapabilitiesRequest{})
	require.NoError(t, err)
	assert.Equal(t, "test-backend", caps.Name)
}

func TestClient_NewEnvironment(t *testing.T) {
	_, conn := startMockServer(t)
	rpc := pluginv1.NewBackendPluginClient(conn)
	caps, err := rpc.Capabilities(t.Context(), &pluginv1.CapabilitiesRequest{})
	require.NoError(t, err)

	c := &Client{conn: conn, rpc: rpc, caps: caps}
	input := &container.NewContainerInput{
		Image:  "img:latest",
		Stdout: io.Discard,
		Stderr: io.Discard,
	}
	env := c.NewEnvironment(input, map[string]string{"key": "val"})
	assert.Equal(t, "test-backend", env.BackendName())
	assert.Equal(t, "/test/root", env.GetRoot())
}

func TestPluginEnvironment_ExecMixedOutput(t *testing.T) {
	mock, conn := startMockServer(t)
	mock.execStdout = "out-line\n"
	mock.execStderr = "err-line\n"

	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))

	var stdout, stderr bytes.Buffer
	env.ReplaceLogWriter(&stdout, &stderr)

	err := env.Exec([]string{"cmd"}, map[string]string{"K": "V"}, "user", "/work")(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "out-line\n", stdout.String())
	assert.Equal(t, "err-line\n", stderr.String())
}

func TestPluginEnvironment_CopyMultipleFiles(t *testing.T) {
	mock, conn := startMockServer(t)
	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))

	err := env.Copy("/dest",
		&container.FileEntry{Name: "a.txt", Mode: 0o644, Body: "aaa"},
		&container.FileEntry{Name: "b.txt", Mode: 0o755, Body: "bbb"},
	)(t.Context())
	require.NoError(t, err)

	tr := tar.NewReader(bytes.NewReader(mock.copyInData))
	names := []string{}
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
		body, _ := io.ReadAll(tr)
		switch hdr.Name {
		case "a.txt":
			assert.Equal(t, "aaa", string(body))
		case "b.txt":
			assert.Equal(t, "bbb", string(body))
		}
	}
	assert.Equal(t, []string{"a.txt", "b.txt"}, names)
}

func TestPluginEnvironment_ServiceAdder(t *testing.T) {
	mock, conn := startMockServer(t)
	env := newTestEnv(t, conn)

	env.AddServiceContainerRaw("db", "postgres:16", map[string]string{"POSTGRES_PASSWORD": "pw"}, []string{"5432"})
	env.AddServiceContainerRaw("cache", "redis:7", nil, []string{"6379"})

	require.NoError(t, env.Create(nil, nil)(t.Context()))

	req := mock.createReq
	require.Len(t, req.Services, 2)
	assert.Equal(t, "db", req.Services[0].Name)
	assert.Equal(t, "postgres:16", req.Services[0].Image)
	assert.Equal(t, "pw", req.Services[0].Env["POSTGRES_PASSWORD"])
	assert.Equal(t, "cache", req.Services[1].Name)
}

func TestPluginEnvironment_ExecContextCancelled(t *testing.T) {
	_, conn := startMockServer(t)
	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := env.Exec([]string{"sleep", "10"}, nil, "", "")(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("%v", context.Canceled))
}

func TestPluginEnvironment_UpdateFromImageEnv(t *testing.T) {
	mock, conn := startMockServer(t)
	mock.startImageEnv = map[string]string{
		"PATH":    "/custom/bin:/usr/bin",
		"GOPATH":  "/go",
		"LANG":    "C.UTF-8",
	}

	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))
	require.NoError(t, env.Start(false)(t.Context()))

	envMap := map[string]string{"LANG": "en_US.UTF-8"}
	require.NoError(t, env.UpdateFromImageEnv(&envMap)(t.Context()))

	assert.Equal(t, "/custom/bin:/usr/bin", envMap["PATH"])
	assert.Equal(t, "/go", envMap["GOPATH"])
	assert.Equal(t, "en_US.UTF-8", envMap["LANG"]) // existing value preserved
}

func TestPluginEnvironment_UpdateFromImageEnv_MergesPath(t *testing.T) {
	mock, conn := startMockServer(t)
	mock.startImageEnv = map[string]string{
		"PATH": "/image/bin",
	}

	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))
	require.NoError(t, env.Start(false)(t.Context()))

	envMap := map[string]string{"PATH": "/existing/bin"}
	require.NoError(t, env.UpdateFromImageEnv(&envMap)(t.Context()))

	assert.Equal(t, "/existing/bin:/image/bin", envMap["PATH"])
}

func TestPluginEnvironment_UpdateFromImageEnv_NilImageEnv(t *testing.T) {
	_, conn := startMockServer(t)
	env := newTestEnv(t, conn)
	require.NoError(t, env.Create(nil, nil)(t.Context()))
	require.NoError(t, env.Start(false)(t.Context()))

	envMap := map[string]string{"FOO": "bar"}
	require.NoError(t, env.UpdateFromImageEnv(&envMap)(t.Context()))

	assert.Equal(t, map[string]string{"FOO": "bar"}, envMap)
}
