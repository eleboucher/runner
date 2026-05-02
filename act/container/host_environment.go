package container

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/common/gitignore"
	"github.com/djherbis/buffer"
	"github.com/djherbis/nio/v3"
	"golang.org/x/term"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"code.forgejo.org/forgejo/runner/v12/act/filecollector"
	"code.forgejo.org/forgejo/runner/v12/act/lookpath"
)

type HostEnvironment struct {
	Name      string
	Path      string
	TmpDir    string
	ToolCache string
	Workdir   string
	ActPath   string
	Root      string
	StdOut    io.Writer
	LXC       bool
}

func (e *HostEnvironment) Create(_, _ []string) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}

func (e *HostEnvironment) Close() common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}

func (e *HostEnvironment) Copy(destPath string, files ...*FileEntry) common.Executor {
	return func(ctx context.Context) error {
		for _, f := range files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(destPath, f.Name)), 0o777); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(destPath, f.Name), []byte(f.Body), fs.FileMode(f.Mode)); err != nil { //nolint:gosec
				return err
			}
		}
		return nil
	}
}

func (e *HostEnvironment) CopyTarStream(ctx context.Context, destPath string, tarStream io.Reader) error {
	if err := os.RemoveAll(destPath); err != nil {
		return err
	}
	tr := tar.NewReader(tarStream)
	cp := &filecollector.CopyCollector{
		DstDir: destPath,
	}
	for {
		ti, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if ti.FileInfo().IsDir() {
			continue
		}
		if ctx.Err() != nil {
			return fmt.Errorf("CopyTarStream has been cancelled")
		}
		if err := cp.WriteFile(ti.Name, ti.FileInfo(), ti.Linkname, tr); err != nil {
			return err
		}
	}
}

func (e *HostEnvironment) CopyDir(destPath, srcPath string, useGitIgnore bool) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		srcPrefix := filepath.Dir(srcPath)
		if !strings.HasSuffix(srcPrefix, string(filepath.Separator)) {
			srcPrefix += string(filepath.Separator)
		}
		logger.Debugf("Stripping prefix:%s src:%s", srcPrefix, srcPath)
		var ignorer gitignore.Matcher
		if useGitIgnore {
			ps, err := gitignore.ReadPatterns(srcPath)
			if err != nil {
				logger.Debugf("Error loading .gitignore: %v", err)
			}

			ignorer = gitignore.NewMatcher(ps)
		}
		fc := &filecollector.FileCollector{
			Ignorer:   ignorer,
			SrcPath:   srcPath,
			SrcPrefix: srcPrefix,
			Handler: &filecollector.CopyCollector{
				DstDir: destPath,
			},
		}
		return filepath.Walk(srcPath, fc.CollectFiles(ctx, []string{}))
	}
}

func (e *HostEnvironment) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	defer tw.Close()
	srcPath = filepath.Clean(srcPath)
	fi, err := os.Lstat(srcPath)
	if err != nil {
		return nil, err
	}
	tc := &filecollector.TarCollector{
		TarWriter: tw,
	}
	if fi.IsDir() {
		srcPrefix := srcPath
		if !strings.HasSuffix(srcPrefix, string(filepath.Separator)) {
			srcPrefix += string(filepath.Separator)
		}
		fc := &filecollector.FileCollector{
			SrcPath:   srcPath,
			SrcPrefix: srcPrefix,
			Handler:   tc,
		}
		err = filepath.Walk(srcPath, fc.CollectFiles(ctx, []string{}))
		if err != nil {
			return nil, err
		}
	} else {
		var f io.ReadCloser
		var linkname string
		if fi.Mode()&fs.ModeSymlink != 0 {
			linkname, err = os.Readlink(srcPath)
			if err != nil {
				return nil, err
			}
		} else {
			f, err = os.Open(srcPath)
			if err != nil {
				return nil, err
			}
			defer f.Close()
		}
		err := tc.WriteFile(fi.Name(), fi, linkname, f)
		if err != nil {
			return nil, err
		}
	}
	return io.NopCloser(buf), nil
}

func (e *HostEnvironment) Pull(_ bool) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}

func (e *HostEnvironment) Start(_ bool) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}

type localEnv struct {
	env map[string]string
}

func (l *localEnv) Getenv(name string) string {
	if runtime.GOOS == "windows" {
		for k, v := range l.env {
			if strings.EqualFold(name, k) {
				return v
			}
		}
		return ""
	}
	return l.env[name]
}

func lookupPathHost(cmd string, env map[string]string, writer io.Writer) (string, error) {
	f, err := lookpath.LookPath2(cmd, &localEnv{env: env})
	if err != nil {
		err := "Cannot find: " + fmt.Sprint(cmd) + " in PATH"
		if _, _err := writer.Write([]byte(err + "\n")); _err != nil {
			return "", fmt.Errorf("%v: %w", err, _err)
		}
		return "", errors.New(err)
	}
	return f, nil
}

func setupPty(cmd *exec.Cmd) (*os.File, *os.File, error) {
	master, slave, err := openPty()
	if err != nil {
		return nil, nil, err
	}
	if term.IsTerminal(int(slave.Fd())) {
		_, err := term.MakeRaw(int(slave.Fd()))
		if err != nil {
			master.Close()
			slave.Close()
			return nil, nil, err
		}
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	return master, slave, nil
}

func copyPtyOutput(writer io.Writer, master io.Reader, finishLog context.CancelFunc) {
	// LXC had a behaviour which permitted short writes to the PTY to cause discarded data, which is fixed upstream in
	// https://github.com/lxc/lxc/pull/4633.  As of writing, this isn't released for our Debian LXC images.  Until it
	// is, we have a partial workaround to reduce the risk of data loss.
	//
	// Writing to `writer` can be relatively slow; when forgejo-runner is in daemon mode then `writer` is `lineWriter`
	// which will split the contents up line-by-line and call a lineHandler, which will send the output to a logger,
	// which will end up in `Reporter` which acquires a mutex for each line received in order to append the line to its
	// internal buffers.  Experimentally, when using an LXC command and PTY configuration, if a command outputs a large
	// log chunk (~500kB), a straight `io.Copy()` between `master` and `reader` will end up with data being lost in
	// chunks in random places in the log -- sometimes the middle, sometimes the end.
	//
	// Introducing a memory buffer in forgejo-runner helps to address this problem.  We read as fast as possible in a
	// dedicated goroutine into the buffer, attempting to keep the PTY buffer clear and ready for writes from the
	// subcommand.  Concurrently, we drain that buffer into `writer`.
	//
	// There's no limit to the buffer size that could be required to get this right and always guarantee all data.  A 2
	// MB buffer was sufficient to meet the needs of reproduction test cases, but this has been bumped up to 100 MB for
	// anticipated real-world use cases.  `buffer.New(x)` is allocated on-demand, so 100 MB is a maximum buffer size.

	pipeReader, pipeWriter := nio.Pipe(buffer.New(100 * 1024 * 1024))
	var wg sync.WaitGroup
	wg.Go(func() {
		// Error is expected -- "read /dev/ptmx: input/output error" is the typical exit for io.Copy here.
		_, _ = io.Copy(pipeWriter, master)
		pipeWriter.Close()
	})
	wg.Go(func() {
		_, _ = io.Copy(writer, pipeReader)
	})
	wg.Wait()

	finishLog()
}

func (e *HostEnvironment) UpdateFromImageEnv(_ *map[string]string) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}

func getEnvListFromMap(env map[string]string) []string {
	envList := make([]string, 0)
	for k, v := range env {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}
	return envList
}

func (e *HostEnvironment) exec(ctx context.Context, commandparam []string, cmdline string, env map[string]string, user, workdir string) error {
	envList := getEnvListFromMap(env)
	var wd string
	if workdir != "" {
		if filepath.IsAbs(workdir) {
			wd = workdir
		} else {
			wd = filepath.Join(e.Path, workdir)
		}
	} else {
		wd = e.Path
	}

	if stat, err := os.Stat(wd); err != nil {
		return fmt.Errorf("failed to stat working directory %s %w", wd, err)
	} else if !stat.IsDir() {
		return fmt.Errorf("working directory %s is not a directory", wd)
	}

	command := make([]string, len(commandparam))
	copy(command, commandparam)

	if e.LXC {
		if user == "root" {
			command = append([]string{"/usr/bin/sudo"}, command...)
		} else {
			common.Logger(ctx).Debugf("lxc-attach --name %v %v", e.Name, command)
			command = append([]string{"/usr/bin/sudo", "--preserve-env", "--preserve-env=PATH", "/usr/bin/lxc-attach", "--keep-env", "--name", e.Name, "--"}, command...)
		}
	}

	f, err := lookupPathHost(command[0], env, e.StdOut)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, f)
	cmd.Path = f
	cmd.Args = command
	cmd.Stdin = nil
	cmd.Stdout = e.StdOut
	cmd.Env = envList
	cmd.Stderr = e.StdOut
	cmd.Dir = wd

	var master *os.File
	var slave *os.File
	defer func() {
		if master != nil {
			master.Close()
		}
		if slave != nil {
			slave.Close()
		}
	}()
	if true /* allocate Terminal */ {
		var err error
		master, slave, err = setupPty(cmd)
		if err != nil {
			common.Logger(ctx).Debugf("Failed to setup Pty %v\n", err.Error())
		}
	}

	logctx, finishLog := context.WithCancel(context.Background())
	if master != nil {
		go copyPtyOutput(e.StdOut, master, finishLog)
	} else {
		finishLog()
	}

	// Don't immediately return error if the command fails -- closing the pty and ensuring all data is flushed through
	// to the logs needs to occur in the command error case.
	runCmdErr := runCmdInGroup(cmd, cmdline, master != nil)

	if slave != nil {
		_ = slave.Close()
	}
	<-logctx.Done()

	if runCmdErr != nil {
		return fmt.Errorf("RUN %w", runCmdErr)
	}
	return nil
}

func (e *HostEnvironment) Exec(command []string /*cmdline string, */, env map[string]string, user, workdir string) common.Executor {
	return e.ExecWithCmdLine(command, "", env, user, workdir)
}

func (e *HostEnvironment) ExecWithCmdLine(command []string, cmdline string, env map[string]string, user, workdir string) common.Executor {
	return func(ctx context.Context) error {
		if err := e.exec(ctx, command, cmdline, env, user, workdir); err != nil {
			select {
			case <-ctx.Done():
				return fmt.Errorf("this step has been cancelled: ctx: %w, exec: %w", ctx.Err(), err)
			default:
				return err
			}
		}
		return nil
	}
}

func (e *HostEnvironment) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	return ParseEnvFile(e, srcPath, env)
}

func (e *HostEnvironment) Remove() common.Executor {
	return func(ctx context.Context) error {
		if e.LXC {
			// there may be files owned by root: removal
			// is the responsibility of the LXC backend
			return nil
		}
		return os.RemoveAll(e.Root)
	}
}

func (e *HostEnvironment) ToContainerPath(path string) string {
	if bp, err := filepath.Rel(e.Workdir, path); err != nil {
		return filepath.Join(e.Path, bp)
	} else if filepath.Clean(e.Workdir) == filepath.Clean(path) {
		return e.Path
	}
	return path
}

func (e *HostEnvironment) BackendID() string {
	if e.LXC {
		return "lxc"
	}
	return "host"
}

func (*HostEnvironment) SupportsDockerContainerActions() bool {
	return true
}

func (*HostEnvironment) ManagesOwnNetworking() bool {
	return true
}

func (e *HostEnvironment) GetName() string {
	return e.Name
}

func (e *HostEnvironment) GetRoot() string {
	return e.Root
}

func (e *HostEnvironment) GetActPath() string {
	actPath := e.ActPath
	if runtime.GOOS == "windows" {
		actPath = strings.ReplaceAll(actPath, "\\", "/")
	}
	return actPath
}

func (*HostEnvironment) GetPathVariableName() string {
	switch runtime.GOOS {
	case "plan9":
		return "path"
	case "windows":
		return "Path" // Actually we need a case insensitive map
	}
	return "PATH"
}

func (e *HostEnvironment) DefaultPathVariable() string {
	v, _ := os.LookupEnv(e.GetPathVariableName())
	return v
}

func (*HostEnvironment) JoinPathVariable(paths ...string) string {
	return strings.Join(paths, string(filepath.ListSeparator))
}

// Reference for Arch values for runner.arch
// https://docs.github.com/en/actions/learn-github-actions/contexts#runner-context
func goArchToActionArch(arch string) string {
	archMapper := map[string]string{
		"amd64":   "X64",
		"x86_64":  "X64",
		"386":     "X86",
		"aarch64": "ARM64",
	}
	if arch, ok := archMapper[arch]; ok {
		return arch
	}
	return arch
}

func goOsToActionOs(os string) string {
	osMapper := map[string]string{
		"linux":   "Linux",
		"windows": "Windows",
		"darwin":  "macOS",
	}
	if os, ok := osMapper[os]; ok {
		return os
	}
	return os
}

func (e *HostEnvironment) GetRunnerContext(_ context.Context) map[string]any {
	return map[string]any{
		"os":         goOsToActionOs(runtime.GOOS),
		"arch":       goArchToActionArch(runtime.GOARCH),
		"temp":       e.TmpDir,
		"tool_cache": e.ToolCache,
	}
}

func (e *HostEnvironment) IsHealthy(ctx context.Context) (time.Duration, error) {
	return 0, nil
}

func (e *HostEnvironment) ReplaceLogWriter(stdout, _ io.Writer) (io.Writer, io.Writer) {
	org := e.StdOut
	e.StdOut = stdout
	return org, org
}

func (*HostEnvironment) IsEnvironmentCaseInsensitive() bool {
	return runtime.GOOS == "windows"
}
