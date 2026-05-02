//go:build !WITHOUT_DOCKER && (linux || darwin || windows || freebsd || openbsd)

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/common/gitignore"
	"dario.cat/mergo"
	"github.com/Masterminds/semver"
	"github.com/avast/retry-go/v4"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/cli/cli/compose/loader"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"github.com/gobwas/glob"
	"github.com/joho/godotenv"
	"github.com/kballard/go-shellquote"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	actcontainer "code.forgejo.org/forgejo/runner/v12/act/container"
	"code.forgejo.org/forgejo/runner/v12/act/filecollector"
)

// NewContainer creates a reference to a container
var NewContainer = func(ep Endpoint, input *actcontainer.NewContainerInput) actcontainer.ExecutionsEnvironment {
	cr := new(containerReference)
	cr.endpoint = ep
	cr.input = input
	cr.toolCache = input.ToolCache
	return cr
}

func (cr *containerReference) platform(ctx context.Context) (string, error) {
	if cr.calculatedPlatform != "" {
		return cr.calculatedPlatform, nil
	}

	platform := cr.input.DefaultPlatform

	c, err := parseOptions(ctx, cr.input.JobOptions)
	if err != nil {
		return "", err
	} else if c.Platform != "" {
		platform = c.Platform
	}

	if platform == "" {
		// cr.input.DefaultPlatform wasn't provided, --platform wasn't provided, fallback to the system platform
		platform = cr.endpoint.CurrentSystemPlatform()
		common.Logger(ctx).Debugf("platform not specified, defaulting to detected %s", platform)
	}

	cr.calculatedPlatform = platform
	return platform, nil
}

// supportsContainerImagePlatform returns true if the underlying Docker server
// API version is 1.41 and beyond
func supportsContainerImagePlatform(ctx context.Context, cli client.APIClient) bool {
	ver, err := cli.ServerVersion(ctx)
	if err != nil {
		panic(fmt.Sprintf("Failed to get Docker API Version: %s", err))
	}
	sv, err := semver.NewVersion(ver.APIVersion)
	if err != nil {
		panic(fmt.Sprintf("Failed to unmarshal Docker Version: %s", err))
	}
	constraint, _ := semver.NewConstraint(">= 1.41")
	return constraint.Check(sv)
}

// supportsImageInspectPlatform returns true if the underlying Docker server supports using
// `client.ImageInspectWithPlatform`, which is API version 1.49 and beyond.
func supportsImageInspectPlatform(ctx context.Context, cli client.APIClient) bool {
	ver, err := cli.ServerVersion(ctx)
	if err != nil {
		panic(fmt.Sprintf("Failed to get Docker API Version: %s", err))
	}
	sv, err := semver.NewVersion(ver.APIVersion)
	if err != nil {
		panic(fmt.Sprintf("Failed to unmarshal Docker Version: %s", err))
	}
	constraint, _ := semver.NewConstraint(">= 1.49")
	return constraint.Check(sv)
}

func (cr *containerReference) Create(capAdd, capDrop []string) common.Executor {
	var infoExecutor common.Executor = func(ctx context.Context) error {
		platform, err := cr.platform(ctx)
		if err != nil {
			return err
		}
		logger := common.Logger(ctx)
		logger.Infof("%sdocker create image=%s platform=%s entrypoint=%+q cmd=%+q network=%+q", logPrefix, cr.input.Image, platform, cr.input.Entrypoint, cr.input.Cmd, cr.input.NetworkMode)
		return nil
	}
	return infoExecutor.
		Then(
			common.NewPipelineExecutor(
				cr.connect(),
				cr.find(),
				cr.create(capAdd, capDrop),
			).IfNot(common.Dryrun),
		)
}

func (cr *containerReference) Start(attach bool) common.Executor {
	var infoExecutor common.Executor = func(ctx context.Context) error {
		platform, err := cr.platform(ctx)
		if err != nil {
			return err
		}
		logger := common.Logger(ctx)
		logger.Infof("%sdocker run image=%s platform=%s entrypoint=%+q cmd=%+q network=%+q", logPrefix, cr.input.Image, platform, cr.input.Entrypoint, cr.input.Cmd, cr.input.NetworkMode)
		return nil
	}
	return infoExecutor.
		Then(
			common.NewPipelineExecutor(
				cr.connect(),
				cr.find(),
				cr.attach().IfBool(attach),
				cr.start(),
				cr.wait().IfBool(attach),
				cr.tryReadUID(),
				cr.tryReadGID(),
				func(ctx context.Context) error {
					// If this fails, then folders have wrong permissions on non root container
					if cr.UID != 0 || cr.GID != 0 {
						_ = cr.Exec([]string{"chown", "-R", fmt.Sprintf("%d:%d", cr.UID, cr.GID), cr.input.WorkingDir}, nil, "0", "")(ctx)
					}
					return nil
				},
			).IfNot(common.Dryrun),
		)
}

func (cr *containerReference) Pull(forcePull bool) common.Executor {
	return func(ctx context.Context) error {
		platform, err := cr.platform(ctx)
		if err != nil {
			return err
		}

		return NewDockerPullExecutor(cr.endpoint, NewDockerPullExecutorInput{
			Image:     cr.input.Image,
			ForcePull: forcePull,
			Platform:  platform,
			Username:  cr.input.Username,
			Password:  cr.input.Password,
		})(ctx)
	}
}

func (cr *containerReference) Copy(destPath string, files ...*actcontainer.FileEntry) common.Executor {
	return common.NewPipelineExecutor(
		cr.connect(),
		cr.find(),
		cr.copyContent(destPath, files...),
	).IfNot(common.Dryrun)
}

func (cr *containerReference) CopyDir(destPath, srcPath string, useGitIgnore bool) common.Executor {
	return common.NewPipelineExecutor(
		common.NewInfoExecutor("%sdocker cp src=%s dst=%s", logPrefix, srcPath, destPath),
		cr.copyDir(destPath, srcPath, useGitIgnore),
		func(ctx context.Context) error {
			// If this fails, then folders have wrong permissions on non root container
			if cr.UID != 0 || cr.GID != 0 {
				_ = cr.Exec([]string{"chown", "-R", fmt.Sprintf("%d:%d", cr.UID, cr.GID), destPath}, nil, "0", "")(ctx)
			}
			return nil
		},
	).IfNot(common.Dryrun)
}

func (cr *containerReference) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	if common.Dryrun(ctx) {
		return nil, fmt.Errorf("DRYRUN is not supported in GetContainerArchive")
	}
	a, _, err := cr.cli.CopyFromContainer(ctx, cr.id, srcPath)
	return a, err
}

func (cr *containerReference) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	return actcontainer.ParseEnvFile(cr, srcPath, env).IfNot(common.Dryrun)
}

func (cr *containerReference) UpdateFromImageEnv(env *map[string]string) common.Executor {
	return cr.extractFromImageEnv(env).IfNot(common.Dryrun)
}

func (cr *containerReference) Exec(command []string, env map[string]string, user, workdir string) common.Executor {
	return common.NewPipelineExecutor(
		common.NewInfoExecutor("%sdocker exec cmd=[%s] user=%s workdir=%s", logPrefix, strings.Join(command, " "), user, workdir),
		cr.connect(),
		cr.find(),
		cr.exec(command, env, user, workdir),
	).IfNot(common.Dryrun)
}

func (cr *containerReference) Remove() common.Executor {
	return common.NewPipelineExecutor(
		cr.connect(),
		cr.find(),
	).Finally(
		cr.remove(),
	).IfNot(common.Dryrun)
}

func (cr *containerReference) inspect(ctx context.Context) (container.InspectResponse, error) {
	resp, err := cr.cli.ContainerInspect(ctx, cr.id)
	if err != nil {
		err = fmt.Errorf("service %v: %s", cr.input.NetworkAliases, err)
	}
	return resp, err
}

func (cr *containerReference) IsHealthy(ctx context.Context) (time.Duration, error) {
	resp, err := cr.inspect(ctx)
	if err != nil {
		return 0, err
	}
	return cr.isHealthy(ctx, resp)
}

func (cr *containerReference) isHealthy(ctx context.Context, resp container.InspectResponse) (time.Duration, error) {
	logger := common.Logger(ctx)
	if resp.Config == nil || resp.Config.Healthcheck == nil || resp.State == nil || resp.State.Health == nil || len(resp.Config.Healthcheck.Test) == 1 && strings.EqualFold(resp.Config.Healthcheck.Test[0], "NONE") {
		logger.Debugf("no container health check defined, hope for the best")
		return 0, nil
	}

	switch resp.State.Health.Status {
	case container.Starting:
		wait := resp.Config.Healthcheck.Interval
		if wait <= 0 {
			wait = time.Second
		}
		logger.Infof("service %v: container health check %s (%s) is starting, waiting %v", cr.input.NetworkAliases, cr.id, resp.Config.Image, wait)
		return wait, nil
	case container.Healthy:
		logger.Infof("service %v: container health check %s (%s) is healthy", cr.input.NetworkAliases, cr.id, resp.Config.Image)
		return 0, nil
	case container.Unhealthy:
		return 0, fmt.Errorf("service %v: container health check %s (%s) is not healthy", cr.input.NetworkAliases, cr.id, resp.Config.Image)
	default:
		return 0, fmt.Errorf("service %v: unexpected health status %s (%s) %v", cr.input.NetworkAliases, cr.id, resp.Config.Image, resp.State.Health.Status)
	}
}

func (cr *containerReference) ReplaceLogWriter(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	out := cr.input.Stdout
	err := cr.input.Stderr

	cr.input.Stdout = stdout
	cr.input.Stderr = stderr

	return out, err
}

type containerReference struct {
	cli                client.APIClient
	id                 string
	input              *actcontainer.NewContainerInput
	UID                int
	GID                int
	calculatedPlatform string
	LinuxContainerEnvironmentExtensions
}

func (cr *containerReference) connect() common.Executor {
	return func(_ context.Context) error {
		if cr.cli == nil {
			cr.cli = cr.endpoint.Client()
		}
		return nil
	}
}

func (cr *containerReference) Close() common.Executor {
	return func(_ context.Context) error {
		cr.cli = nil
		return nil
	}
}

func (cr *containerReference) find() common.Executor {
	return func(ctx context.Context) error {
		if cr.id != "" {
			return nil
		}
		containers, err := cr.cli.ContainerList(ctx, container.ListOptions{
			All: true,
		})
		if err != nil {
			return fmt.Errorf("failed to list containers: %w", err)
		}

		for _, c := range containers {
			for _, name := range c.Names {
				if name[1:] == cr.input.Name {
					cr.id = c.ID
					return nil
				}
			}
		}

		cr.id = ""
		return nil
	}
}

func (cr *containerReference) remove() common.Executor {
	return func(ctx context.Context) error {
		if cr.id == "" {
			return nil
		}

		logger := common.Logger(ctx)
		return retry.Do(
			func() error {
				// Kill the container explicitly. It not only makes the desired behaviour obvious, it also aligns
				// Podman's behaviour with Docker's. Podman is much more cautious and sends a SIGTERM first when
				// `ContainerRemove` is invoked whereas Docker outright sends SIGKILL. The problem is that Podman waits
				// usually 10 sec (configurable) for SIGTERM to succeed.
				if err := cr.cli.ContainerKill(ctx, cr.id, ""); err != nil {
					// Only log the error. `ContainerRemove` might be able to fix the problem.
					logger.Debugf("Container %s could not be killed: %v", cr.id, err)
				}
				if err := cr.cli.ContainerRemove(ctx, cr.id, container.RemoveOptions{RemoveVolumes: true, Force: true}); err != nil {
					if cerrdefs.IsNotFound(err) {
						logger.Debugf("Container %s not found, considering this as a success", cr.id)
						return nil
					}
					return err
				}

				logger.Debugf("Container removed: %v", cr.id)
				cr.id = ""
				return nil
			},
			retry.Context(ctx),
			retry.OnRetry(func(n uint, err error) {
				logger.Warnf("Failed to remove docker container %s (retry #%d): %s\n", cr.id, n, err)
			}),
		)
	}
}

func (cr *containerReference) mergeOptions(ctx context.Context, config *container.Config, hostConfig *container.HostConfig) (*container.Config, *container.HostConfig, error) {
	if cr.input.ConfigOptions == "" && cr.input.JobOptions == "" {
		return config, hostConfig, nil
	}

	var err error

	if config, hostConfig, err = cr.mergeConfigOptions(ctx, config, hostConfig); err != nil {
		return nil, nil, err
	}

	if config, hostConfig, err = cr.mergeJobOptions(ctx, config, hostConfig); err != nil {
		return nil, nil, err
	}

	return config, hostConfig, nil
}

func (cr *containerReference) mergeConfigOptions(ctx context.Context, config *container.Config, hostConfig *container.HostConfig) (*container.Config, *container.HostConfig, error) {
	logger := common.Logger(ctx)
	input := cr.input

	containerConfig, err := parseOptions(ctx, input.ConfigOptions)
	if err != nil {
		return nil, nil, err
	}

	if !hostConfig.Privileged {
		containerConfig.HostConfig.Privileged = false
	}

	logger.Debugf("Custom container.Config from options ==> %+v", containerConfig.Config)

	err = mergo.Merge(config, containerConfig.Config, mergo.WithOverride, mergo.WithAppendSlice)
	if err != nil {
		return nil, nil, fmt.Errorf("Cannot merge container.Config options: '%s': '%w'", input.ConfigOptions, err)
	}
	logger.Debugf("Merged container.Config ==> %+v", config)

	logger.Debugf("Custom container.HostConfig from options ==> %+v", containerConfig.HostConfig)

	hostConfig.Binds = append(hostConfig.Binds, containerConfig.HostConfig.Binds...)
	hostConfig.Mounts = append(hostConfig.Mounts, containerConfig.HostConfig.Mounts...)
	binds := hostConfig.Binds
	mounts := hostConfig.Mounts
	networkMode := hostConfig.NetworkMode
	err = mergo.Merge(hostConfig, containerConfig.HostConfig, mergo.WithOverride)
	if err != nil {
		return nil, nil, fmt.Errorf("Cannot merge container.HostConfig options: '%s': '%w'", input.ConfigOptions, err)
	}
	hostConfig.Binds = binds
	hostConfig.Mounts = mounts
	hostConfig.NetworkMode = networkMode
	logger.Debugf("Merged container.HostConfig ==> %+v", hostConfig)
	return config, hostConfig, nil
}

func (cr *containerReference) mergeJobOptions(ctx context.Context, config *container.Config, hostConfig *container.HostConfig) (*container.Config, *container.HostConfig, error) {
	jobConfig, err := parseOptions(ctx, cr.input.JobOptions)
	if err != nil {
		return nil, nil, err
	}

	logger := common.Logger(ctx)

	if jobConfig.Config.Healthcheck != nil && len(jobConfig.Config.Healthcheck.Test) > 0 {
		logger.Debugf("--health-* options %+v", jobConfig.Config.Healthcheck)
		config.Healthcheck = jobConfig.Config.Healthcheck
	}

	if len(jobConfig.Config.Volumes) > 0 {
		logger.Debugf("--volume options (except bind) %v", jobConfig.Config.Volumes)
		err = mergo.Merge(&config.Volumes, jobConfig.Config.Volumes, mergo.WithOverride, mergo.WithAppendSlice)
		if err != nil {
			return nil, nil, fmt.Errorf("Cannot merge container.Config.Volumes options: '%s': '%w'", cr.input.JobOptions, err)
		}
	}

	if len(jobConfig.HostConfig.Binds) > 0 {
		logger.Debugf("--volume options (only bind) %v", jobConfig.HostConfig.Binds)
		err = mergo.Merge(&hostConfig.Binds, jobConfig.HostConfig.Binds, mergo.WithOverride, mergo.WithAppendSlice)
		if err != nil {
			return nil, nil, fmt.Errorf("Cannot merge hostConfig.Bind options: '%s': '%w'", cr.input.JobOptions, err)
		}
	}

	if len(jobConfig.HostConfig.Tmpfs) > 0 {
		logger.Debugf("--tmpfs options %v", jobConfig.HostConfig.Tmpfs)
		err = mergo.Merge(&hostConfig.Tmpfs, jobConfig.HostConfig.Tmpfs, mergo.WithOverride, mergo.WithAppendSlice)
		if err != nil {
			return nil, nil, fmt.Errorf("Cannot merge Config.Tmpfs options: '%s': '%w'", cr.input.JobOptions, err)
		}
	}

	if jobConfig.HostConfig.Memory > 0 {
		logger.Debugf("--memory %v", jobConfig.HostConfig.Memory)
		if hostConfig.Memory > 0 && jobConfig.HostConfig.Memory > hostConfig.Memory {
			return nil, nil, fmt.Errorf("the --memory %v option found in the workflow cannot be greater than the --memory %v option from the runner configuration file", jobConfig.HostConfig.Memory, hostConfig.Memory)
		}
		hostConfig.Memory = jobConfig.HostConfig.Memory
	}

	if len(jobConfig.Config.Hostname) > 0 {
		logger.Debugf("--hostname %v", jobConfig.Config.Hostname)
		config.Hostname = jobConfig.Config.Hostname
	}

	if len(jobConfig.Config.User) > 0 {
		logger.Debugf("--user %v", jobConfig.Config.User)
		config.User = jobConfig.Config.User
	}

	for _, group := range jobConfig.HostConfig.GroupAdd {
		if !slices.Contains(hostConfig.GroupAdd, group) {
			logger.Debugf("--group-add %v", group)
			hostConfig.GroupAdd = append(hostConfig.GroupAdd, group)
		}
	}

	return config, hostConfig, nil
}

func parseOptions(ctx context.Context, options string) (*containerConfig, error) {
	logger := common.Logger(ctx)

	flags := pflag.NewFlagSet("container_flags", pflag.ContinueOnError)
	copts := addFlags(flags)

	if len(copts.netMode.Value()) > 0 {
		logger.Warn("--network and --net in the options will be ignored.")
	}

	optionsArgs, err := shellquote.Split(options)
	if err != nil {
		return nil, fmt.Errorf("Cannot split container options: '%s': '%w'", options, err)
	}

	err = flags.Parse(optionsArgs)
	if err != nil {
		return nil, fmt.Errorf("Cannot parse container options: '%s': '%w'", options, err)
	}

	containerConfig, err := parse(flags, copts, runtime.GOOS)
	if err != nil {
		return nil, fmt.Errorf("Cannot process container options: '%s': '%w'", options, err)
	}

	return containerConfig, nil
}

func (cr *containerReference) create(capAdd, capDrop []string) common.Executor {
	return func(ctx context.Context) error {
		if cr.id != "" {
			return nil
		}
		logger := common.Logger(ctx)
		isTerminal := term.IsTerminal(int(os.Stdout.Fd()))
		input := cr.input

		exposedPorts := nat.PortSet{}
		for port := range input.ExposedPorts {
			exposedPorts[nat.Port(port)] = struct{}{}
		}

		config := &container.Config{
			Image:        input.Image,
			WorkingDir:   input.WorkingDir,
			Env:          input.Env,
			ExposedPorts: exposedPorts,
			Tty:          isTerminal || input.TTY,
		}
		logger.Debugf("Common container.Config ==> %+v", config)

		if len(input.Cmd) != 0 {
			config.Cmd = input.Cmd
		}

		if len(input.Entrypoint) != 0 {
			config.Entrypoint = input.Entrypoint
		}

		mounts := make([]mount.Mount, 0)
		for mountSource, mountTarget := range input.Mounts {
			mounts = append(mounts, mount.Mount{
				Type:   mount.TypeVolume,
				Source: mountSource,
				Target: mountTarget,
			})
		}

		var platform string
		var err error
		var platSpecs *specs.Platform
		if supportsContainerImagePlatform(ctx, cr.cli) {
			platform, err = cr.platform(ctx)
			if err != nil {
				return err
			}
			platSpecs, err = parsePlatform(platform)
			if err != nil {
				return err
			}
		}

		portBindings := nat.PortMap{}
		for port, bindings := range input.PortBindings {
			natBindings := make([]nat.PortBinding, len(bindings))
			for i, b := range bindings {
				natBindings[i] = nat.PortBinding{HostIP: b.HostIP, HostPort: b.HostPort}
			}
			portBindings[nat.Port(port)] = natBindings
		}

		hostConfig := &container.HostConfig{
			CapAdd:       capAdd,
			CapDrop:      capDrop,
			Binds:        input.Binds,
			Mounts:       mounts,
			NetworkMode:  container.NetworkMode(input.NetworkMode),
			Privileged:   input.Privileged,
			UsernsMode:   container.UsernsMode(input.UsernsMode),
			PortBindings: portBindings,
			Init:         &input.Init,
		}
		logger.Debugf("Common container.HostConfig ==> %+v", hostConfig)

		config, hostConfig, err = cr.mergeOptions(ctx, config, hostConfig)
		if err != nil {
			return err
		}

		// For Gitea
		config, hostConfig = cr.sanitizeConfig(ctx, config, hostConfig)

		// For Gitea
		// network-scoped alias is supported only for containers in user defined networks
		var networkingConfig *network.NetworkingConfig
		logger.Debugf("input.NetworkAliases ==> %v", input.NetworkAliases)
		n := hostConfig.NetworkMode
		// IsUserDefined and IsHost are broken on windows
		if n.IsUserDefined() && n != "host" && len(input.NetworkAliases) > 0 {
			endpointConfig := &network.EndpointSettings{
				Aliases: input.NetworkAliases,
			}
			networkingConfig = &network.NetworkingConfig{
				EndpointsConfig: map[string]*network.EndpointSettings{
					input.NetworkMode: endpointConfig,
				},
			}
		}

		resp, err := cr.cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, platSpecs, input.Name)
		if err != nil {
			return fmt.Errorf("failed to create container: '%w'", err)
		}

		logger.Debugf("Created container name=%s id=%v from image %v (platform: %s)", input.Name, resp.ID, input.Image, platform)
		logger.Debugf("ENV ==> %v", input.Env)

		cr.id = resp.ID
		return nil
	}
}

func (cr *containerReference) extractFromImageEnv(env *map[string]string) common.Executor {
	envMap := *env
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)

		inspect, err := cr.cli.ImageInspect(ctx, cr.input.Image)
		if err != nil {
			err = fmt.Errorf("inspect image: %w", err)
			logger.Error(err)
			return err
		}

		if inspect.Config == nil {
			return nil
		}

		imageEnv, err := godotenv.Unmarshal(strings.Join(inspect.Config.Env, "\n"))
		if err != nil {
			err = fmt.Errorf("unmarshal image env: %w", err)
			logger.Error(err)
			return err
		}

		for k, v := range imageEnv {
			if k == "PATH" {
				if envMap[k] == "" {
					envMap[k] = v
				} else {
					envMap[k] += `:` + v
				}
			} else if envMap[k] == "" {
				envMap[k] = v
			}
		}

		env = &envMap
		return nil
	}
}

func (cr *containerReference) exec(cmd []string, env map[string]string, user, workdir string) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		// Fix slashes when running on Windows
		if runtime.GOOS == "windows" {
			var newCmd []string
			for _, v := range cmd {
				newCmd = append(newCmd, strings.ReplaceAll(v, `\`, `/`))
			}
			cmd = newCmd
		}

		logger.Debugf("Exec command '%s'", cmd)
		isTerminal := term.IsTerminal(int(os.Stdout.Fd()))
		envList := make([]string, 0)
		for k, v := range env {
			envList = append(envList, fmt.Sprintf("%s=%s", k, v))
		}

		var wd string
		if workdir != "" {
			if strings.HasPrefix(workdir, "/") {
				wd = workdir
			} else {
				wd = fmt.Sprintf("%s/%s", cr.input.WorkingDir, workdir)
			}
		} else {
			wd = cr.input.WorkingDir
		}
		logger.Debugf("Working directory '%s'", wd)

		idResp, err := cr.cli.ContainerExecCreate(ctx, cr.id, container.ExecOptions{
			User:         user,
			Cmd:          cmd,
			WorkingDir:   wd,
			Env:          envList,
			Tty:          isTerminal,
			AttachStderr: true,
			AttachStdout: true,
		})
		if err != nil {
			// If exec fails, it's possible that the entrypoint of the container failed in some way; for example
			// "/usr/bin/tail: exec format error" has been observed if the container's platform doesn't match the
			// current platform.  In order to help diagnose these problems, run a `docker logs ...` on the container and
			// include it in the error output.
			logContext := ""
			reader, err2 := cr.cli.ContainerLogs(ctx, cr.id, container.LogsOptions{
				ShowStdout: true,
				ShowStderr: true,
			})
			if err2 == nil {
				output, err2 := io.ReadAll(reader)
				if err2 == nil {
					logContext = string(output)
				} else {
					logger.Warnf("unable to read container logs: %v", err2)
				}
			} else {
				logger.Warnf("unable to fetch container logs: %v", err2)
			}
			return fmt.Errorf("failed to create exec: %w; container logs: %q", err, logContext)
		}

		resp, err := cr.cli.ContainerExecAttach(ctx, idResp.ID, container.ExecAttachOptions{
			Tty: isTerminal,
		})
		if err != nil {
			return fmt.Errorf("failed to attach to exec: %w", err)
		}
		defer resp.Close()

		err = cr.waitForCommand(ctx, isTerminal, resp, idResp, user, workdir)
		if err != nil {
			return err
		}

		inspectResp, err := cr.cli.ContainerExecInspect(ctx, idResp.ID)
		if err != nil {
			return fmt.Errorf("failed to inspect exec: %w", err)
		}

		switch inspectResp.ExitCode {
		case 0:
			return nil
		case 127:
			return fmt.Errorf("exitcode '%d': command not found, please refer to https://github.com/nektos/act/issues/107 for more information", inspectResp.ExitCode)
		default:
			return fmt.Errorf("exitcode '%d': failure", inspectResp.ExitCode)
		}
	}
}

func (cr *containerReference) tryReadID(opt string, cbk func(id int)) common.Executor {
	return func(ctx context.Context) error {
		idResp, err := cr.cli.ContainerExecCreate(ctx, cr.id, container.ExecOptions{
			Cmd:          []string{"id", opt},
			AttachStdout: true,
			AttachStderr: true,
		})
		if err != nil {
			return nil
		}

		resp, err := cr.cli.ContainerExecAttach(ctx, idResp.ID, container.ExecAttachOptions{})
		if err != nil {
			return nil
		}
		defer resp.Close()

		sid, err := resp.Reader.ReadString('\n')
		if err != nil {
			return nil
		}
		exp := regexp.MustCompile(`\d+\n`)
		found := exp.FindString(sid)
		id, err := strconv.ParseInt(strings.TrimSpace(found), 10, 32)
		if err != nil {
			return nil
		}
		cbk(int(id))

		return nil
	}
}

func (cr *containerReference) tryReadUID() common.Executor {
	return cr.tryReadID("-u", func(id int) { cr.UID = id })
}

func (cr *containerReference) tryReadGID() common.Executor {
	return cr.tryReadID("-g", func(id int) { cr.GID = id })
}

func (cr *containerReference) waitForCommand(ctx context.Context, isTerminal bool, resp types.HijackedResponse, _ container.ExecCreateResponse, _, _ string) error {
	logger := common.Logger(ctx)

	cmdResponse := make(chan error)

	go func() {
		var outWriter io.Writer
		outWriter = cr.input.Stdout
		if outWriter == nil {
			outWriter = os.Stdout
		}
		errWriter := cr.input.Stderr
		if errWriter == nil {
			errWriter = os.Stderr
		}

		var err error
		if !isTerminal || os.Getenv("NORAW") != "" {
			_, err = stdcopy.StdCopy(outWriter, errWriter, resp.Reader)
		} else {
			_, err = io.Copy(outWriter, resp.Reader)
		}
		cmdResponse <- err
	}()

	select {
	case <-ctx.Done():
		// send ctrl + c
		_, err := resp.Conn.Write([]byte{3})
		if err != nil {
			logger.Warnf("Failed to send CTRL+C: %+s", err)
		}

		// we return the context canceled error to prevent other steps
		// from executing
		return ctx.Err()
	case err := <-cmdResponse:
		if err != nil {
			logger.Errorf("command response: %v", err)
		}

		return nil
	}
}

func (cr *containerReference) CopyTarStream(ctx context.Context, destPath string, tarStream io.Reader) error {
	// Mkdir
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	_ = tw.WriteHeader(&tar.Header{
		Name:     destPath,
		Mode:     0o777,
		Typeflag: tar.TypeDir,
	})
	tw.Close()
	err := cr.cli.CopyToContainer(ctx, cr.id, "/", buf, container.CopyToContainerOptions{})
	if err != nil {
		return fmt.Errorf("failed to mkdir to copy content to container: %w", err)
	}
	// Copy Content
	err = cr.cli.CopyToContainer(ctx, cr.id, destPath, tarStream, container.CopyToContainerOptions{})
	if err != nil {
		return fmt.Errorf("copyTarStream: failed to copy content to container: %w", err)
	}
	// If this fails, then folders have wrong permissions on non root container
	if cr.UID != 0 || cr.GID != 0 {
		_ = cr.Exec([]string{"chown", "-R", fmt.Sprintf("%d:%d", cr.UID, cr.GID), destPath}, nil, "0", "")(ctx)
	}
	return nil
}

func (cr *containerReference) copyDir(dstPath, srcPath string, useGitIgnore bool) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		tarFile, err := os.CreateTemp("", "act")
		if err != nil {
			return err
		}
		logger.Debugf("Writing tarball %s from %s", tarFile.Name(), srcPath)
		defer func(tarFile *os.File) {
			name := tarFile.Name()
			err := tarFile.Close()
			if err != nil && !errors.Is(err, os.ErrClosed) {
				logger.Errorf("close tar file: %s: %v", name, err)
			}
			err = os.Remove(name)
			if err != nil {
				logger.Errorf("remove file: %s: %v", name, err)
			}
		}(tarFile)
		tw := tar.NewWriter(tarFile)

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
			Handler: &filecollector.TarCollector{
				TarWriter: tw,
				UID:       cr.UID,
				GID:       cr.GID,
				DstDir:    dstPath[1:],
			},
		}

		err = filepath.Walk(srcPath, fc.CollectFiles(ctx, []string{}))
		if err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			err = fmt.Errorf("close tar writer: %w", err)
			logger.Debug(err)
			return err
		}

		logger.Debugf("Extracting content from '%s' to '%s'", tarFile.Name(), dstPath)
		_, err = tarFile.Seek(0, 0)
		if err != nil {
			return fmt.Errorf("failed to seek tar archive: %w", err)
		}
		err = cr.cli.CopyToContainer(ctx, cr.id, "/", tarFile, container.CopyToContainerOptions{})
		if err != nil {
			return fmt.Errorf("copyDir: failed to copy content to container: %w", err)
		}
		return nil
	}
}

func (cr *containerReference) copyContent(dstPath string, files ...*actcontainer.FileEntry) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, file := range files {
			logger.Debugf("Writing entry to tarball %s len:%d", file.Name, len(file.Body))
			hdr := &tar.Header{
				Name: file.Name,
				Mode: file.Mode,
				Size: int64(len(file.Body)),
				Uid:  cr.UID,
				Gid:  cr.GID,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if _, err := tw.Write([]byte(file.Body)); err != nil {
				return err
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}

		logger.Debugf("Extracting content to '%s'", dstPath)
		err := cr.cli.CopyToContainer(ctx, cr.id, dstPath, &buf, container.CopyToContainerOptions{})
		if err != nil {
			return fmt.Errorf("copyContent: failed to copy content to container: %w", err)
		}
		return nil
	}
}

func (cr *containerReference) attach() common.Executor {
	return func(ctx context.Context) error {
		out, err := cr.cli.ContainerAttach(ctx, cr.id, container.AttachOptions{
			Stream: true,
			Stdout: true,
			Stderr: true,
		})
		if err != nil {
			return fmt.Errorf("failed to attach to container: %w", err)
		}
		isTerminal := term.IsTerminal(int(os.Stdout.Fd()))

		var outWriter io.Writer
		outWriter = cr.input.Stdout
		if outWriter == nil {
			outWriter = os.Stdout
		}
		errWriter := cr.input.Stderr
		if errWriter == nil {
			errWriter = os.Stderr
		}
		go func() {
			if !isTerminal || os.Getenv("NORAW") != "" {
				_, err = stdcopy.StdCopy(outWriter, errWriter, out.Reader)
			} else {
				_, err = io.Copy(outWriter, out.Reader)
			}
			if err != nil {
				common.Logger(ctx).Errorf("redirect container output: %v", err)
			}
		}()
		return nil
	}
}

func (cr *containerReference) start() common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		logger.Debugf("Starting container: %v", cr.id)

		if err := cr.cli.ContainerStart(ctx, cr.id, container.StartOptions{}); err != nil {
			return fmt.Errorf("failed to start container: %w", err)
		}

		logger.Debugf("Started container: %v", cr.id)
		return nil
	}
}

func (cr *containerReference) wait() common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		statusCh, errCh := cr.cli.ContainerWait(ctx, cr.id, container.WaitConditionNotRunning)
		var statusCode int64
		select {
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("failed to wait for container: %w", err)
			}
		case status := <-statusCh:
			statusCode = status.StatusCode
		}

		logger.Debugf("Return status: %v", statusCode)

		if statusCode == 0 {
			return nil
		}

		return fmt.Errorf("exit with `FAILURE`: %v", statusCode)
	}
}

// For Gitea
// sanitizeConfig remove the invalid configurations from `config` and `hostConfig`
func (cr *containerReference) sanitizeConfig(ctx context.Context, config *container.Config, hostConfig *container.HostConfig) (*container.Config, *container.HostConfig) {
	logger := common.Logger(ctx)

	if len(cr.input.ValidVolumes) > 0 {
		globs := make([]glob.Glob, 0, len(cr.input.ValidVolumes))
		for _, v := range cr.input.ValidVolumes {
			if g, err := glob.Compile(v); err != nil {
				logger.Errorf("create glob from %s error: %v", v, err)
			} else {
				globs = append(globs, g)
			}
		}
		isValid := func(v string) bool {
			for _, g := range globs {
				if g.Match(v) {
					return true
				}
			}
			return false
		}
		// sanitize binds
		sanitizedBinds := make([]string, 0, len(hostConfig.Binds))
		for _, bind := range hostConfig.Binds {
			parsed, err := loader.ParseVolume(bind)
			if err != nil {
				logger.Warnf("parse volume [%s] error: %v", bind, err)
				continue
			}
			if parsed.Source == "" {
				// anonymous volume
				sanitizedBinds = append(sanitizedBinds, bind)
				continue
			}
			if isValid(parsed.Source) {
				sanitizedBinds = append(sanitizedBinds, bind)
			} else {
				logger.Warnf("[%s] is not a valid volume, will be ignored", parsed.Source)
			}
		}
		hostConfig.Binds = sanitizedBinds
		// sanitize mounts
		sanitizedMounts := make([]mount.Mount, 0, len(hostConfig.Mounts))
		for _, mt := range hostConfig.Mounts {
			if isValid(mt.Source) {
				sanitizedMounts = append(sanitizedMounts, mt)
			} else {
				logger.Warnf("[%s] is not a valid volume, will be ignored", mt.Source)
			}
		}
		hostConfig.Mounts = sanitizedMounts
	} else {
		hostConfig.Binds = []string{}
		hostConfig.Mounts = []mount.Mount{}
	}

	return config, hostConfig
}
