package container

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Type assert HostEnvironment implements ExecutionsEnvironment
var _ ExecutionsEnvironment = &HostEnvironment{}

func TestHostEnvironment_BackendName(t *testing.T) {
	e := &HostEnvironment{}
	assert.Equal(t, "host", e.BackendName())

	eLXC := &HostEnvironment{LXC: true}
	assert.Equal(t, "lxc", eLXC.BackendName())
}

func TestCopyDir(t *testing.T) {
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
	err := e.CopyDir(e.Workdir, e.Path, true)(ctx)
	assert.NoError(t, err)
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
