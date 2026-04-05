package testplugin

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1024 * 1024

func startServer(t *testing.T) (pluginv1.BackendPluginClient, *Server) {
	t.Helper()

	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	ts := New()
	pluginv1.RegisterBackendPluginServer(srv, ts)
	t.Cleanup(func() {
		srv.Stop()
		ts.Cleanup()
	})

	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return pluginv1.NewBackendPluginClient(conn), ts
}

func createEnv(t *testing.T, client pluginv1.BackendPluginClient) string {
	t.Helper()
	resp, err := client.Create(t.Context(), &pluginv1.CreateRequest{
		Image: "test:latest",
		Name:  "test",
		Env:   []string{"TEST_VAR=hello"},
	})
	require.NoError(t, err)
	_, err = client.Start(t.Context(), &pluginv1.StartRequest{EnvironmentId: resp.GetEnvironmentId()})
	require.NoError(t, err)
	return resp.GetEnvironmentId()
}

func TestCapabilities(t *testing.T) {
	client, _ := startServer(t)
	caps, err := client.Capabilities(t.Context(), &pluginv1.CapabilitiesRequest{})
	require.NoError(t, err)

	assert.Equal(t, "test", caps.GetName())
	assert.Equal(t, "/shared", caps.GetRootPath())
	assert.Equal(t, "/shared/act", caps.GetActPath())
	assert.True(t, caps.GetManagesOwnNetworking())
	assert.False(t, caps.GetSupportsDockerActions())
	assert.True(t, caps.GetSupportsLocalCopy())
}

func TestCreateAndRemove(t *testing.T) {
	client, ts := startServer(t)
	envID := createEnv(t, client)
	assert.Contains(t, ts.envs, envID)

	_, err := client.Remove(t.Context(), &pluginv1.RemoveRequest{EnvironmentId: envID})
	require.NoError(t, err)
	assert.NotContains(t, ts.envs, envID)
}

func TestExec_Echo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo command differs on Windows")
	}
	client, _ := startServer(t)
	envID := createEnv(t, client)

	stream, err := client.Exec(t.Context(), &pluginv1.ExecRequest{
		EnvironmentId: envID,
		Command:       []string{"echo", "hello world"},
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	var exitCode int32
	for {
		out, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if out.GetDone() {
			exitCode = out.GetExitCode()
			break
		}
		if out.GetStream() == pluginv1.ExecOutput_STDOUT {
			stdout.Write(out.GetData())
		}
	}

	assert.Equal(t, int32(0), exitCode)
	assert.Equal(t, "hello world\n", stdout.String())
}

func TestExec_FailingCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("false command differs on Windows")
	}
	client, _ := startServer(t)
	envID := createEnv(t, client)

	stream, err := client.Exec(t.Context(), &pluginv1.ExecRequest{
		EnvironmentId: envID,
		Command:       []string{"false"},
	})
	require.NoError(t, err)

	var exitCode int32
	for {
		out, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if out.GetDone() {
			exitCode = out.GetExitCode()
			break
		}
	}
	assert.NotEqual(t, int32(0), exitCode)
}

func TestExec_EnvVars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("env command differs on Windows")
	}
	client, _ := startServer(t)
	envID := createEnv(t, client)

	stream, err := client.Exec(t.Context(), &pluginv1.ExecRequest{
		EnvironmentId: envID,
		Command:       []string{"sh", "-c", "echo $TEST_VAR $EXTRA"},
		Env:           map[string]string{"EXTRA": "world"},
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	for {
		out, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if out.GetDone() {
			break
		}
		if out.GetStream() == pluginv1.ExecOutput_STDOUT {
			stdout.Write(out.GetData())
		}
	}
	assert.Equal(t, "hello world\n", stdout.String())
}

func TestCopyIn_AndCopyOut(t *testing.T) {
	client, _ := startServer(t)
	envID := createEnv(t, client)

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := []byte("test content 123")
	_ = tw.WriteHeader(&tar.Header{Name: "test.txt", Mode: 0o644, Size: int64(len(content))})
	_, _ = tw.Write(content)
	tw.Close()

	copyInStream, err := client.CopyIn(t.Context())
	require.NoError(t, err)
	err = copyInStream.Send(&pluginv1.CopyInChunk{
		EnvironmentId: envID,
		DestPath:      "/shared/workdir",
		Data:          tarBuf.Bytes(),
	})
	require.NoError(t, err)
	_, err = copyInStream.CloseAndRecv()
	require.NoError(t, err)

	copyOutStream, err := client.CopyOut(t.Context(), &pluginv1.CopyOutRequest{
		EnvironmentId: envID,
		SrcPath:       "/shared/workdir/test.txt",
	})
	require.NoError(t, err)

	var outBuf bytes.Buffer
	for {
		chunk, err := copyOutStream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		outBuf.Write(chunk.GetData())
	}

	tr := tar.NewReader(&outBuf)
	hdr, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "test.txt", hdr.Name)
	data, _ := io.ReadAll(tr)
	assert.Equal(t, "test content 123", string(data))
}

func TestCopyLocal(t *testing.T) {
	client, ts := startServer(t)
	envID := createEnv(t, client)

	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "local.txt"), []byte("local data"), 0o644))

	_, err := client.CopyLocal(t.Context(), &pluginv1.CopyLocalRequest{
		EnvironmentId: envID,
		SrcPath:       srcDir,
		DestPath:      "/shared/workdir/copied",
	})
	require.NoError(t, err)

	ts.mu.Lock()
	env := ts.envs[envID]
	ts.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(env.root, "shared/workdir/copied/local.txt"))
	require.NoError(t, err)
	assert.Equal(t, "local data", string(data))
}

func TestUpdateEnv(t *testing.T) {
	client, ts := startServer(t)
	envID := createEnv(t, client)

	ts.mu.Lock()
	env := ts.envs[envID]
	ts.mu.Unlock()
	envFile := filepath.Join(env.root, "shared/workdir/env.txt")
	require.NoError(t, os.WriteFile(envFile, []byte("KEY1=value1\nKEY2=value2\n"), 0o644))

	resp, err := client.UpdateEnv(t.Context(), &pluginv1.UpdateEnvRequest{
		EnvironmentId: envID,
		SrcPath:       "/shared/workdir/env.txt",
		CurrentEnv:    map[string]string{"EXISTING": "yes"},
	})
	require.NoError(t, err)
	assert.Equal(t, "value1", resp.GetUpdatedEnv()["KEY1"])
	assert.Equal(t, "value2", resp.GetUpdatedEnv()["KEY2"])
	assert.Equal(t, "yes", resp.GetUpdatedEnv()["EXISTING"])
}

func TestUpdateEnv_MissingFile(t *testing.T) {
	client, _ := startServer(t)
	envID := createEnv(t, client)

	resp, err := client.UpdateEnv(t.Context(), &pluginv1.UpdateEnvRequest{
		EnvironmentId: envID,
		SrcPath:       "/shared/workdir/nonexistent",
		CurrentEnv:    map[string]string{"A": "1"},
	})
	require.NoError(t, err)
	assert.Equal(t, "1", resp.GetUpdatedEnv()["A"])
}

func TestIsHealthy(t *testing.T) {
	client, _ := startServer(t)
	envID := createEnv(t, client)

	resp, err := client.IsHealthy(t.Context(), &pluginv1.IsHealthyRequest{EnvironmentId: envID})
	require.NoError(t, err)
	assert.Equal(t, int64(0), resp.GetWaitNanos())
}

func TestIsHealthy_NotFound(t *testing.T) {
	client, _ := startServer(t)
	_, err := client.IsHealthy(t.Context(), &pluginv1.IsHealthyRequest{EnvironmentId: "bogus"})
	assert.Error(t, err)
}

func TestRemove_NotFound(t *testing.T) {
	client, _ := startServer(t)
	_, err := client.Remove(t.Context(), &pluginv1.RemoveRequest{EnvironmentId: "bogus"})
	assert.Error(t, err)
}

func TestRemove_CleansUpTempDir(t *testing.T) {
	client, ts := startServer(t)
	envID := createEnv(t, client)

	ts.mu.Lock()
	root := ts.envs[envID].root
	ts.mu.Unlock()

	_, err := os.Stat(root)
	require.NoError(t, err)

	_, err = client.Remove(t.Context(), &pluginv1.RemoveRequest{EnvironmentId: envID})
	require.NoError(t, err)

	_, err = os.Stat(root)
	assert.True(t, os.IsNotExist(err))
}
