package plugin

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	buildOnce      sync.Once
	builtBinaryDir string
	buildErr       error
)

func testPluginBinary(t *testing.T) string {
	t.Helper()

	buildOnce.Do(func() {
		builtBinaryDir, buildErr = os.MkdirTemp("", "testplugin-*")
		if buildErr != nil {
			return
		}
		binPath := filepath.Join(builtBinaryDir, "testplugin")
		cmd := exec.Command("go", "build", "-o", binPath, "./testplugin/cmd")
		cmd.Dir = "."
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		buildErr = cmd.Run()
	})
	require.NoError(t, buildErr, "failed to build testplugin binary")
	return filepath.Join(builtBinaryDir, "testplugin")
}

func TestV2Client_Capabilities(t *testing.T) {
	binPath := testPluginBinary(t)

	client, err := NewClientV2(t.Context(), binPath)
	require.NoError(t, err)
	defer client.Close()

	caps := client.Capabilities()
	assert.Equal(t, "test", caps.GetName())
	assert.Equal(t, "/shared", caps.GetRootPath())
	assert.Equal(t, "/shared/act", caps.GetActPath())
	assert.True(t, caps.GetManagesOwnNetworking())
	assert.False(t, caps.GetSupportsDockerActions())
}

func TestV2Client_Lifecycle(t *testing.T) {
	binPath := testPluginBinary(t)

	client, err := NewClientV2(t.Context(), binPath)
	require.NoError(t, err)
	defer client.Close()

	var stdout bytes.Buffer
	env := client.NewEnvironment(&container.NewContainerInput{
		Image:      "test:latest",
		Name:       "test-container",
		Env:        []string{"FOO=bar"},
		WorkingDir: "/workspace",
		Stdout:     &stdout,
		Stderr:     io.Discard,
	}, map[string]string{})

	require.NoError(t, env.Create(nil, nil)(t.Context()))
	require.NoError(t, env.Start(false)(t.Context()))

	require.NoError(t, env.Exec([]string{"echo", "hello from go-plugin"}, nil, "", "")(t.Context()))
	assert.Contains(t, stdout.String(), "hello from go-plugin")

	wait, err := env.IsHealthy(t.Context())
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), wait)

	require.NoError(t, env.Remove()(t.Context()))
}

func TestV2Client_Copy(t *testing.T) {
	binPath := testPluginBinary(t)

	client, err := NewClientV2(t.Context(), binPath)
	require.NoError(t, err)
	defer client.Close()

	env := client.NewEnvironment(&container.NewContainerInput{
		Image:  "test:latest",
		Name:   "test-container",
		Stdout: io.Discard,
		Stderr: io.Discard,
	}, map[string]string{})

	require.NoError(t, env.Create(nil, nil)(t.Context()))
	require.NoError(t, env.Start(false)(t.Context()))

	err = env.Copy("/dest", &container.FileEntry{
		Name: "hello.txt",
		Mode: 0o644,
		Body: "hello world",
	})(t.Context())
	require.NoError(t, err)

	// Read it back via CopyOut
	rc, err := env.GetContainerArchive(t.Context(), "/dest/hello.txt")
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.NotEmpty(t, data)

	require.NoError(t, env.Remove()(t.Context()))
}

func TestV2Client_InvalidBinary(t *testing.T) {
	_, err := NewClientV2(t.Context(), "/nonexistent/plugin/binary")
	require.Error(t, err)
}
