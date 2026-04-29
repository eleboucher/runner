package container

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gotest.tools/v3/skip"
)

// Type assert HostEnvironment implements ExecutionsEnvironment
var _ ExecutionsEnvironment = &HostEnvironment{}

func TestHostEnvironment_BackendID(t *testing.T) {
	e := &HostEnvironment{}
	assert.Equal(t, "host", e.BackendID())

	eLXC := &HostEnvironment{LXC: true}
	assert.Equal(t, "lxc", eLXC.BackendID())
}

func TestCopyDir(t *testing.T) {
	dir := t.TempDir()
	e := &HostEnvironment{
		Path:      filepath.Join(dir, "path"),
		TmpDir:    filepath.Join(dir, "tmp"),
		ToolCache: filepath.Join(dir, "tool_cache"),
		ActPath:   filepath.Join(dir, "act_path"),
		StdOut:    os.Stdout,
		Workdir:   path.Join("testdata", "scratch"),
	}
	_ = os.MkdirAll(e.Path, 0o700)
	_ = os.MkdirAll(e.TmpDir, 0o700)
	_ = os.MkdirAll(e.ToolCache, 0o700)
	_ = os.MkdirAll(e.ActPath, 0o700)

	t.Run("with gitignore", func(t *testing.T) {
		// U+1FAE3 is the "Face with peeking eye" emoji.
		src := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(src, "one.txt"), []byte("1"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "two.txt"), []byte("2"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "two-\U0001FAE3.txt"), []byte("2"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "\U0001FAE3.txt"), []byte("\U0001FAE3"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, ".gitignore"), []byte("two*\n"), 0o644))

		dst := t.TempDir()

		err := e.CopyDir(dst, src+string(filepath.Separator), true)(t.Context())
		assert.NoError(t, err)

		expectFileContents := func(path, expected string) {
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, expected, string(data))
		}

		expectFileContents(filepath.Join(dst, "one.txt"), "1")
		assert.NoFileExists(t, filepath.Join(dst, "two.txt"))
		assert.NoFileExists(t, filepath.Join(dst, "two-\U0001FAE3.txt"))
		expectFileContents(filepath.Join(dst, "\U0001FAE3.txt"), "\U0001FAE3")
		expectFileContents(filepath.Join(dst, ".gitignore"), "two*\n")
	})

	t.Run("with disabled gitignore", func(t *testing.T) {
		src := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(src, "one.txt"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "two.txt"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, ".gitignore"), []byte("two*\n"), 0o644))

		dst := t.TempDir()

		err := e.CopyDir(dst, src+string(filepath.Separator), false)(t.Context())
		assert.NoError(t, err)

		assert.FileExists(t, filepath.Join(dst, "one.txt"))
		assert.FileExists(t, filepath.Join(dst, "two.txt"))
		assert.FileExists(t, filepath.Join(dst, ".gitignore"))
	})

	t.Run("with subdirectories", func(t *testing.T) {
		src := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(src, "a", "b", "c"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, "one.txt"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "a", "b", "c", "two.txt"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "a", "b", "c", "three.txt"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "a", "b", "two.txt"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, ".gitignore"), []byte("**/two.txt\n"), 0o644))

		dst := t.TempDir()

		err := e.CopyDir(dst, src+string(filepath.Separator), true)(t.Context())
		assert.NoError(t, err)

		assert.FileExists(t, filepath.Join(dst, "one.txt"))
		assert.NoFileExists(t, filepath.Join(dst, "a", "b", "c", "two.txt"))
		assert.FileExists(t, filepath.Join(dst, "a", "b", "c", "three.txt"))
		assert.NoFileExists(t, filepath.Join(dst, "a", "b", "two.txt"))
	})

	t.Run("with gitignore in subdirectory", func(t *testing.T) {
		src := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(src, "a", "b", "c"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, "one.txt"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "a", "b", "c", "two.txt"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "a", "b", "c", "three.txt"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "a", "b", "two.txt"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(src, "a", "b", ".gitignore"), []byte("two.txt\n"), 0o644))

		dst := t.TempDir()

		err := e.CopyDir(dst, src+string(filepath.Separator), true)(t.Context())
		assert.NoError(t, err)

		assert.FileExists(t, filepath.Join(dst, "one.txt"))
		assert.NoFileExists(t, filepath.Join(dst, "a", "b", "c", "two.txt"))
		assert.FileExists(t, filepath.Join(dst, "a", "b", "c", "three.txt"))
		assert.FileExists(t, filepath.Join(dst, "a", "b", ".gitignore"))
		assert.NoFileExists(t, filepath.Join(dst, "a", "b", "two.txt"))
	})

	t.Run("retains UNIX permissions", func(t *testing.T) {
		skip.If(t, runtime.GOOS == "windows")

		src := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(src, "one.txt"), nil, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(src, "two.txt"), nil, 0o755))

		dst := t.TempDir()

		err := e.CopyDir(dst, src+string(filepath.Separator), true)(t.Context())
		assert.NoError(t, err)

		infoOne, err := os.Stat(filepath.Join(dst, "one.txt"))
		assert.NoError(t, err)
		assert.EqualValues(t, os.FileMode(0o700), infoOne.Mode().Perm())

		infoTwo, err := os.Stat(filepath.Join(dst, "two.txt"))
		assert.NoError(t, err)
		assert.EqualValues(t, os.FileMode(0o755), infoTwo.Mode().Perm())
	})

	t.Run("Git repository with submodule matching ignore pattern", func(t *testing.T) {
		submoduleRepo := makeTestRepo(t)
		require.NoError(t, os.WriteFile(filepath.Join(submoduleRepo, "one.txt"), []byte("1\n"), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(submoduleRepo, "a", "b"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(submoduleRepo, "a", "b", "two.txt"), []byte("2\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(submoduleRepo, "a", "b", "three.txt"), []byte("3\n"), 0o644))
		require.NoError(t, gitCmd("-C", submoduleRepo, "add", "--all"))
		require.NoError(t, gitCmd("-C", submoduleRepo, "commit", "-m", "Import"))

		src := makeTestRepo(t)

		require.NoError(t, gitCmd("-C", src, "-c", "protocol.file.allow=always", "submodule", "add", submoduleRepo, "test-submodule"))
		require.NoError(t, os.WriteFile(filepath.Join(src, ".gitignore"), []byte("test-submodule\n**/two.txt\n"), 0o644))
		require.NoError(t, gitCmd("-C", src, "add", "--all"))
		require.NoError(t, gitCmd("-C", src, "commit", "-m", "Import"))

		dst := t.TempDir()

		err := e.CopyDir(dst, src+string(filepath.Separator), true)(t.Context())
		assert.NoError(t, err)

		assert.DirExists(t, filepath.Join(dst, ".git"))
		assert.NoFileExists(t, filepath.Join(dst, "test-submodule", ".git"))
		assert.NoDirExists(t, filepath.Join(dst, "test-submodule", ".git"))
		assert.FileExists(t, filepath.Join(dst, ".gitignore"))
		assert.FileExists(t, filepath.Join(dst, "test-submodule", "one.txt"))
		assert.FileExists(t, filepath.Join(dst, "test-submodule", "a", "b", "two.txt"))
		assert.FileExists(t, filepath.Join(dst, "test-submodule", "a", "b", "three.txt"))
	})
}

func TestGetContainerArchive(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	e := &HostEnvironment{
		Path:      filepath.Join(dir, "path"),
		TmpDir:    filepath.Join(dir, "tmp"),
		ToolCache: filepath.Join(dir, "tool_cache"),
		ActPath:   filepath.Join(dir, "act_path"),
		StdOut:    os.Stdout,
		Workdir:   path.Join("testdata", "scratch"),
	}
	_ = os.MkdirAll(e.Path, 0o700)
	_ = os.MkdirAll(e.TmpDir, 0o700)
	_ = os.MkdirAll(e.ToolCache, 0o700)
	_ = os.MkdirAll(e.ActPath, 0o700)
	expectedContent := []byte("sdde/7sh")
	err := os.WriteFile(filepath.Join(e.Path, "action.yml"), expectedContent, 0o600)
	assert.NoError(t, err)
	archive, err := e.GetContainerArchive(ctx, e.Path)
	assert.NoError(t, err)
	defer archive.Close()
	reader := tar.NewReader(archive)
	h, err := reader.Next()
	assert.NoError(t, err)
	assert.Equal(t, "action.yml", h.Name)
	content, err := io.ReadAll(reader)
	assert.NoError(t, err)
	assert.Equal(t, expectedContent, content)
	_, err = reader.Next()
	assert.ErrorIs(t, err, io.EOF)
}

func TestCancelLongRunningCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()

	var argv []string
	var expectedExit string
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "evil.ps1")
		contents := `
Start-Job -ScriptBlock {
Start-Process -WorkingDirectory $using:PWD -FilePath 'powershell' -ArgumentList '-Command',@'
	while ($true) { echo $null >> check_file; Start-Sleep -Milliseconds 100 }
'@
}
while ($true) { Start-Sleep -Seconds 1 }
`
		powershellPath, err := exec.LookPath("pwsh")
		if err != nil {
			powershellPath, err = exec.LookPath("powershell")
			if err != nil {
				powershellPath = "powershell"
			}
		}
		_ = os.WriteFile(path, []byte(contents), 0o700)
		argv = []string{powershellPath, "-File", path}
		expectedExit = "exit status 1"
	} else {
		path := filepath.Join(dir, "evil.sh")
		contents := `#!/bin/sh
(
	nohup sh <<-EOF >/dev/null 2>/dev/null &
		while true; do
			touch check_file
			sleep 0.1
		done
	EOF
	while true; do sleep 1; done
)`
		_ = os.WriteFile(path, []byte(contents), 0o700)
		argv = []string{path}
		expectedExit = "signal: killed"
	}

	outputBuffer := bytes.NewBuffer(make([]byte, 0, 8192))
	e := &HostEnvironment{
		Path:      dir,
		TmpDir:    dir,
		ToolCache: dir,
		ActPath:   dir,
		StdOut:    outputBuffer,
		Workdir:   dir,
	}

	ctx, cancel := context.WithCancel(t.Context())

	execTime := time.Now()
	execResult := make(chan error)
	go func() {
		execResult <- e.Exec(argv, map[string]string{
			"PATH": os.Getenv("PATH"),
		}, "", dir)(ctx)
	}()

	// The child process tree will repeatedly create a file named 'check_file'. Wait for
	// that file to appear to detect when everything has spawned. To allow for a system
	// under extreme load, we wait up to 60 seconds for that to happen, though it will
	// typically happen much faster.
	checkFilePath := filepath.Join(dir, "check_file")
	if !assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.FileExists(c, checkFilePath)
	}, 60*time.Second, 100*time.Millisecond) {
		t.Logf("subcommand output was: %q", outputBuffer.String())
	}

	// Now that everything is running, cancel the child process.
	cancel()
	runTime := time.Since(execTime)
	assert.Error(t, <-execResult, fmt.Errorf("this step has been cancelled: ctx: context canceled, exec: RUN %s", expectedExit))

	// The child has been killed, so if we delete 'check_file', it should never return.
	_ = os.Remove(checkFilePath)
	// On a system under heavy load, using `runTime` here is a good heuristic for how long
	// we might have to wait before the child gets another chance to write the file.
	time.Sleep(runTime)
	assert.NoFileExists(t, checkFilePath)
}

func makeTestRepo(t *testing.T) string {
	t.Helper()
	repoPath := t.TempDir()
	require.NoError(t, gitCmd("-C", repoPath, "init", "--initial-branch=main"))
	require.NoError(t, gitCmd("-C", repoPath, "config", "user.name", "test"))
	require.NoError(t, gitCmd("-C", repoPath, "config", "user.email", "test@test.com"))
	return repoPath
}

func gitCmd(args ...string) error {
	_, err := gitCmdWithStdout(args...)
	return err
}

func gitCmdWithStdout(args ...string) ([]byte, error) {
	var stdoutBuffer bytes.Buffer
	stdout := bufio.NewWriter(&stdoutBuffer)

	cmd := exec.Command("git", args...)
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if exitError, ok := err.(*exec.ExitError); ok {
		if waitStatus, ok := exitError.Sys().(syscall.WaitStatus); ok {
			return nil, fmt.Errorf("Exit error %d", waitStatus.ExitStatus())
		}
		return nil, exitError
	}

	return stdoutBuffer.Bytes(), nil
}
