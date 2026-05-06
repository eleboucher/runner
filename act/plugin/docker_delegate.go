//go:build !WITHOUT_DOCKER && (linux || darwin || windows || freebsd || openbsd)

package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"code.forgejo.org/forgejo/runner/v12/act/container"
	"code.forgejo.org/forgejo/runner/v12/act/container/docker"
	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
)

// dockerDelegateEnvironment is the ExecutionsEnvironment for plugins that
// declare delegates_to_docker=true. The plugin only provisions the env and
// returns a Docker endpoint + TLS material in CreateResponse.delegate; the
// runner drives all container operations through docker.Env.
type dockerDelegateEnvironment struct {
	docker.LinuxContainerEnvironmentExtensions

	client      pluginv1.BackendPluginClient
	caps        *pluginv1.CapabilitiesResponse
	backendOpts map[string]string
	input       *container.NewContainerInput

	mu        sync.Mutex
	envID     string
	dockerEnv *docker.Env
	inner     container.ExecutionsEnvironment
}

var (
	_ container.ExecutionsEnvironment = (*dockerDelegateEnvironment)(nil)
	_ docker.Hosting                  = (*dockerDelegateEnvironment)(nil)
)

var errBeforePull = errors.New("plugin delegate: called before Pull")

func newDockerDelegateEnvironment(
	client pluginv1.BackendPluginClient,
	caps *pluginv1.CapabilitiesResponse,
	input *container.NewContainerInput,
	backendOpts map[string]string,
) *dockerDelegateEnvironment {
	return &dockerDelegateEnvironment{
		client:      client,
		caps:        caps,
		backendOpts: backendOpts,
		input:       input,
	}
}

func (d *dockerDelegateEnvironment) BackendID() string {
	return d.caps.GetName()
}

func (d *dockerDelegateEnvironment) SupportsDockerContainerActions() bool {
	return d.caps.GetSupportsDockerActions()
}

func (d *dockerDelegateEnvironment) ManagesOwnNetworking() bool {
	return d.caps.GetManagesOwnNetworking()
}

func (d *dockerDelegateEnvironment) GetName() string {
	return d.input.Name
}

// DockerEnv returns the provisioned daemon, or nil before Pull / after
// Remove. Caller must not retain the pointer past Remove.
func (d *dockerDelegateEnvironment) DockerEnv() *docker.Env {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dockerEnv
}

// Pull provisions the env. Must be called before other Container methods.
func (d *dockerDelegateEnvironment) Pull(forcePull bool) common.Executor {
	return func(ctx context.Context) error {
		inner, err := d.provision(ctx, forcePull)
		if err != nil {
			return err
		}
		return inner.Pull(forcePull)(ctx)
	}
}

// provision creates the env on first call and returns the inner container.
// Returns the local reference so callers don't re-read d.inner — that read
// would race with Remove.
func (d *dockerDelegateEnvironment) provision(ctx context.Context, forcePull bool) (container.ExecutionsEnvironment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inner != nil {
		return d.inner, nil
	}

	resp, err := d.client.Create(ctx, &pluginv1.CreateRequest{
		Image:          d.input.Image,
		Name:           d.input.Name,
		Env:            d.input.Env,
		WorkingDir:     d.input.WorkingDir,
		BackendOptions: d.backendOpts,
		ForcePull:      forcePull,
	})
	if err != nil {
		return nil, fmt.Errorf("plugin create: %w", err)
	}

	// plugin.Create succeeded; any later failure must release the remote env.
	envID := resp.GetEnvironmentId()
	teardown := func() {
		_, _ = d.client.Remove(ctx, &pluginv1.RemoveRequest{EnvironmentId: envID})
	}

	del := resp.GetDelegate()
	if del == nil {
		teardown()
		return nil, errors.New("plugin declared delegates_to_docker but returned no delegate block")
	}
	if del.GetEndpoint() == "" {
		teardown()
		return nil, errors.New("plugin delegate endpoint is empty")
	}

	tls := dockerTLSFromDelegate(del)
	env, err := docker.Open(ctx, del.GetEndpoint(), tls)
	if err != nil {
		teardown()
		return nil, fmt.Errorf("dial delegate %s: %w", del.GetEndpoint(), err)
	}

	d.envID = envID
	d.dockerEnv = env
	d.inner = env.NewContainer(d.input)
	return d.inner, nil
}

func dockerTLSFromDelegate(del *pluginv1.DockerDelegate) *docker.TLSConfig {
	if len(del.GetTlsCa()) == 0 &&
		len(del.GetTlsCert()) == 0 &&
		len(del.GetTlsKey()) == 0 &&
		!del.GetTlsInsecureSkipVerify() {
		return nil
	}
	return &docker.TLSConfig{
		CA:                 del.GetTlsCa(),
		Cert:               del.GetTlsCert(),
		Key:                del.GetTlsKey(),
		InsecureSkipVerify: del.GetTlsInsecureSkipVerify(),
	}
}

func (d *dockerDelegateEnvironment) Create(capAdd, capDrop []string) common.Executor {
	return d.requireInner(func(inner container.ExecutionsEnvironment) common.Executor {
		return inner.Create(capAdd, capDrop)
	})
}

func (d *dockerDelegateEnvironment) Start(attach bool) common.Executor {
	return d.requireInner(func(inner container.ExecutionsEnvironment) common.Executor {
		return inner.Start(attach)
	})
}

func (d *dockerDelegateEnvironment) Exec(command []string, env map[string]string, user, workdir string) common.Executor {
	return d.requireInner(func(inner container.ExecutionsEnvironment) common.Executor {
		return inner.Exec(command, env, user, workdir)
	})
}

func (d *dockerDelegateEnvironment) Copy(destPath string, files ...*container.FileEntry) common.Executor {
	return d.requireInner(func(inner container.ExecutionsEnvironment) common.Executor {
		return inner.Copy(destPath, files...)
	})
}

func (d *dockerDelegateEnvironment) CopyDir(destPath, srcPath string, useGitIgnore bool) common.Executor {
	return d.requireInner(func(inner container.ExecutionsEnvironment) common.Executor {
		return inner.CopyDir(destPath, srcPath, useGitIgnore)
	})
}

func (d *dockerDelegateEnvironment) CopyTarStream(ctx context.Context, destPath string, tarStream io.Reader) error {
	inner := d.snapshotInner()
	if inner == nil {
		return errBeforePull
	}
	return inner.CopyTarStream(ctx, destPath, tarStream)
}

func (d *dockerDelegateEnvironment) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	inner := d.snapshotInner()
	if inner == nil {
		return nil, errBeforePull
	}
	return inner.GetContainerArchive(ctx, srcPath)
}

func (d *dockerDelegateEnvironment) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	return d.requireInner(func(inner container.ExecutionsEnvironment) common.Executor {
		return inner.UpdateFromEnv(srcPath, env)
	})
}

func (d *dockerDelegateEnvironment) UpdateFromImageEnv(env *map[string]string) common.Executor {
	return d.requireInner(func(inner container.ExecutionsEnvironment) common.Executor {
		return inner.UpdateFromImageEnv(env)
	})
}

func (d *dockerDelegateEnvironment) IsHealthy(ctx context.Context) (time.Duration, error) {
	inner := d.snapshotInner()
	if inner == nil {
		return 0, errBeforePull
	}
	return inner.IsHealthy(ctx)
}

func (d *dockerDelegateEnvironment) ReplaceLogWriter(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	inner := d.snapshotInner()
	if inner == nil {
		return stdout, stderr
	}
	return inner.ReplaceLogWriter(stdout, stderr)
}

// Remove tears down inner container, closes docker.Env, then plugin
// Remove. Errors joined so cleanup proceeds if one stage fails.
func (d *dockerDelegateEnvironment) Remove() common.Executor {
	return func(ctx context.Context) error {
		d.mu.Lock()
		inner := d.inner
		dockerEnv := d.dockerEnv
		envID := d.envID
		d.inner = nil
		d.dockerEnv = nil
		d.envID = ""
		d.mu.Unlock()

		var errs []error
		if inner != nil {
			if err := inner.Remove()(ctx); err != nil {
				errs = append(errs, fmt.Errorf("inner container remove: %w", err))
			}
		}
		if dockerEnv != nil {
			if err := dockerEnv.Close(); err != nil {
				errs = append(errs, fmt.Errorf("docker env close: %w", err))
			}
		}
		if envID != "" {
			if _, err := d.client.Remove(ctx, &pluginv1.RemoveRequest{EnvironmentId: envID}); err != nil {
				errs = append(errs, fmt.Errorf("plugin remove: %w", err))
			}
		}
		return errors.Join(errs...)
	}
}

// Close is a no-op: docker.Env owns the client lifetime, released in Remove.
func (d *dockerDelegateEnvironment) Close() common.Executor {
	return func(_ context.Context) error { return nil }
}

func (d *dockerDelegateEnvironment) snapshotInner() container.ExecutionsEnvironment {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inner
}

func (d *dockerDelegateEnvironment) requireInner(action func(container.ExecutionsEnvironment) common.Executor) common.Executor {
	return func(ctx context.Context) error {
		inner := d.snapshotInner()
		if inner == nil {
			return errBeforePull
		}
		return action(inner)(ctx)
	}
}
