package plugin

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"code.forgejo.org/forgejo/runner/v12/act/container"
	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
)

const copyChunkSize = 256 * 1024 // 256 KB

type pluginEnvironment struct {
	client      pluginv1.BackendPluginClient
	caps        *pluginv1.CapabilitiesResponse
	backendOpts map[string]string
	input       *container.NewContainerInput
	envID       string
	services    []*pluginv1.ServiceContainer
	imageEnv    map[string]string

	mu     sync.Mutex
	stdout io.Writer
	stderr io.Writer
}

var (
	_ container.ExecutionsEnvironment = (*pluginEnvironment)(nil)
	_ container.ServiceAdder          = (*pluginEnvironment)(nil)
)

func (p *pluginEnvironment) AddServiceContainerRaw(name, image string, env map[string]string, ports []string) {
	p.services = append(p.services, &pluginv1.ServiceContainer{
		Name:  name,
		Image: image,
		Env:   env,
		Ports: ports,
	})
}

func (p *pluginEnvironment) BackendName() string {
	return p.caps.GetName()
}

func (p *pluginEnvironment) SupportsDockerActions() bool {
	return p.caps.GetSupportsDockerActions()
}

func (p *pluginEnvironment) ManagesOwnNetworking() bool {
	return p.caps.GetManagesOwnNetworking()
}

func (p *pluginEnvironment) GetName() string {
	return p.caps.GetName()
}

func (p *pluginEnvironment) GetRoot() string {
	return p.caps.GetRootPath()
}

func (p *pluginEnvironment) GetActPath() string {
	return p.caps.GetActPath()
}

func (p *pluginEnvironment) GetPathVariableName() string {
	if v := p.caps.GetPathVariableName(); v != "" {
		return v
	}
	return "PATH"
}

func (p *pluginEnvironment) DefaultPathVariable() string {
	if v := p.caps.GetDefaultPathVariable(); v != "" {
		return v
	}
	return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}

func (p *pluginEnvironment) JoinPathVariable(paths ...string) string {
	sep := p.caps.GetPathSeparator()
	if sep == "" {
		sep = ":"
	}
	return strings.Join(paths, sep)
}

func (p *pluginEnvironment) GetRunnerContext(_ context.Context) map[string]any {
	rc := p.caps.GetRunnerContext()
	result := make(map[string]any, len(rc))
	for k, v := range rc {
		result[k] = v
	}
	return result
}

func (p *pluginEnvironment) IsEnvironmentCaseInsensitive() bool {
	return p.caps.GetEnvironmentCaseInsensitive()
}

func (p *pluginEnvironment) ToContainerPath(path string) string {
	return path
}

func (p *pluginEnvironment) Create(capAdd, capDrop []string) common.Executor {
	return func(ctx context.Context) error {
		resp, err := p.client.Create(ctx, &pluginv1.CreateRequest{
			Image:          p.input.Image,
			Name:           p.input.Name,
			Env:            p.input.Env,
			WorkingDir:     p.input.WorkingDir,
			CapAdd:         capAdd,
			CapDrop:        capDrop,
			Services:       p.services,
			BackendOptions: p.backendOpts,
		})
		if err != nil {
			return fmt.Errorf("plugin create: %w", err)
		}
		p.envID = resp.GetEnvironmentId()
		return nil
	}
}

func (p *pluginEnvironment) Start(_ bool) common.Executor {
	return func(ctx context.Context) error {
		resp, err := p.client.Start(ctx, &pluginv1.StartRequest{
			EnvironmentId: p.envID,
		})
		if err != nil {
			return fmt.Errorf("plugin start: %w", err)
		}
		p.imageEnv = resp.GetImageEnv()
		return nil
	}
}

func (p *pluginEnvironment) Pull(_ bool) common.Executor {
	return common.NewInfoExecutor("plugin manages image pull internally")
}

func (p *pluginEnvironment) ConnectToNetwork(_ string) common.Executor {
	return common.NewInfoExecutor("plugin manages networking internally")
}

func (p *pluginEnvironment) Exec(command []string, env map[string]string, user, workdir string) common.Executor {
	return func(ctx context.Context) error {
		stream, err := p.client.Exec(ctx, &pluginv1.ExecRequest{
			EnvironmentId: p.envID,
			Command:       command,
			Env:           env,
			User:          user,
			Workdir:       workdir,
		})
		if err != nil {
			return fmt.Errorf("plugin exec: %w", err)
		}

		var exitCode int32
		var errorMessage string
		for {
			out, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("plugin exec stream: %w", err)
			}

			if len(out.GetData()) > 0 {
				p.mu.Lock()
				switch out.GetStream() {
				case pluginv1.ExecOutput_STDOUT:
					_, _ = p.stdout.Write(out.GetData())
				case pluginv1.ExecOutput_STDERR:
					_, _ = p.stderr.Write(out.GetData())
				}
				p.mu.Unlock()
			}

			if out.GetDone() {
				exitCode = out.GetExitCode()
				errorMessage = out.GetErrorMessage()
				break
			}
		}

		if exitCode != 0 {
			if errorMessage != "" {
				return fmt.Errorf("plugin exec: %s", errorMessage)
			}
			return fmt.Errorf("plugin exec: exit code %d", exitCode)
		}
		return nil
	}
}

func (p *pluginEnvironment) Copy(destPath string, files ...*container.FileEntry) common.Executor {
	return func(ctx context.Context) error {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)

		for _, f := range files {
			if err := tw.WriteHeader(&tar.Header{
				Name: f.Name,
				Mode: f.Mode,
				Size: int64(len(f.Body)),
			}); err != nil {
				return fmt.Errorf("plugin copy tar header: %w", err)
			}
			if _, err := tw.Write([]byte(f.Body)); err != nil {
				return fmt.Errorf("plugin copy tar write: %w", err)
			}
		}

		if err := tw.Close(); err != nil {
			return err
		}

		return p.streamCopyIn(ctx, destPath, &buf)
	}
}

func (p *pluginEnvironment) CopyDir(destPath, srcPath string, _ bool) common.Executor {
	return func(ctx context.Context) error {
		if p.caps.GetSupportsLocalCopy() {
			_, err := p.client.CopyLocal(ctx, &pluginv1.CopyLocalRequest{
				EnvironmentId: p.envID,
				SrcPath:       srcPath,
				DestPath:      destPath,
			})
			return err
		}

		pr, pw := io.Pipe()
		go func() {
			tw := tar.NewWriter(pw)
			if err := tw.AddFS(os.DirFS(srcPath)); err != nil {
				pw.CloseWithError(err)
				return
			}
			if err := tw.Close(); err != nil {
				pw.CloseWithError(err)
				return
			}
			pw.Close()
		}()
		return p.streamCopyIn(ctx, destPath, pr)
	}
}

func (p *pluginEnvironment) CopyTarStream(ctx context.Context, destPath string, tarStream io.Reader) error {
	return p.streamCopyIn(ctx, destPath, tarStream)
}

func (p *pluginEnvironment) streamCopyIn(ctx context.Context, destPath string, r io.Reader) error {
	stream, err := p.client.CopyIn(ctx)
	if err != nil {
		return fmt.Errorf("plugin copyin: %w", err)
	}

	buf := make([]byte, copyChunkSize)
	first := true
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			chunk := &pluginv1.CopyInChunk{
				Data: buf[:n],
			}
			if first {
				chunk.EnvironmentId = p.envID
				chunk.DestPath = destPath
				first = false
			}
			if err := stream.Send(chunk); err != nil {
				return fmt.Errorf("plugin copyin send: %w", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("plugin copyin read: %w", readErr)
		}
	}

	if _, err := stream.CloseAndRecv(); err != nil {
		return fmt.Errorf("plugin copyin close: %w", err)
	}
	return nil
}

func (p *pluginEnvironment) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	stream, err := p.client.CopyOut(ctx, &pluginv1.CopyOutRequest{
		EnvironmentId: p.envID,
		SrcPath:       srcPath,
	})
	if err != nil {
		return nil, fmt.Errorf("plugin copyout: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				pw.Close()
				return
			}
			if err != nil {
				pw.CloseWithError(fmt.Errorf("plugin copyout stream: %w", err))
				return
			}
			if _, err := pw.Write(chunk.GetData()); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()

	return pr, nil
}

func (p *pluginEnvironment) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	return func(ctx context.Context) error {
		current := *env
		if current == nil {
			current = make(map[string]string)
		}
		resp, err := p.client.UpdateEnv(ctx, &pluginv1.UpdateEnvRequest{
			EnvironmentId: p.envID,
			SrcPath:       srcPath,
			CurrentEnv:    current,
		})
		if err != nil {
			return fmt.Errorf("plugin updateenv: %w", err)
		}
		updated := resp.GetUpdatedEnv()
		if updated == nil {
			updated = make(map[string]string)
		}
		*env = updated
		return nil
	}
}

func (p *pluginEnvironment) UpdateFromImageEnv(env *map[string]string) common.Executor {
	return func(_ context.Context) error {
		if p.imageEnv == nil {
			return nil
		}
		envMap := *env
		pathVar := p.GetPathVariableName()
		sep := p.caps.GetPathSeparator()
		if sep == "" {
			sep = ":"
		}
		for k, v := range p.imageEnv {
			if k == pathVar {
				if envMap[k] == "" {
					envMap[k] = v
				} else {
					envMap[k] += sep + v
				}
			} else if envMap[k] == "" {
				envMap[k] = v
			}
		}
		*env = envMap
		return nil
	}
}

func (p *pluginEnvironment) IsHealthy(ctx context.Context) (time.Duration, error) {
	resp, err := p.client.IsHealthy(ctx, &pluginv1.IsHealthyRequest{
		EnvironmentId: p.envID,
	})
	if err != nil {
		return 0, fmt.Errorf("plugin ishealthy: %w", err)
	}
	return time.Duration(resp.GetWaitNanos()), nil
}

func (p *pluginEnvironment) Remove() common.Executor {
	return func(ctx context.Context) error {
		_, err := p.client.Remove(ctx, &pluginv1.RemoveRequest{
			EnvironmentId: p.envID,
		})
		if err != nil {
			return fmt.Errorf("plugin remove: %w", err)
		}
		return nil
	}
}

func (p *pluginEnvironment) Close() common.Executor {
	return func(_ context.Context) error {
		return nil
	}
}

func (p *pluginEnvironment) ReplaceLogWriter(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	oldOut, oldErr := p.stdout, p.stderr
	p.stdout = stdout
	p.stderr = stderr
	return oldOut, oldErr
}
