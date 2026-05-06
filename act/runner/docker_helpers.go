package runner

import (
	"context"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"code.forgejo.org/forgejo/runner/v12/act/container"
	"code.forgejo.org/forgejo/runner/v12/act/container/docker"
	"github.com/docker/docker/api/types/network"
)

// jobDockerEnv returns the Docker daemon the JobContainer is bound to, or
// nil if the back-end has no per-job daemon (built-in docker / host both
// use the process-environment daemon). For delegating plugins the env is
// nil until Pull has provisioned it; callers must invoke this after the
// JobContainer has been started, otherwise they silently fall back to the
// process daemon.
func (rc *RunContext) jobDockerEnv() *docker.Env {
	if rc.JobContainer == nil {
		return nil
	}
	h, ok := rc.JobContainer.(docker.Hosting)
	if !ok {
		return nil
	}
	return h.DockerEnv()
}

func (rc *RunContext) newContainer(input *container.NewContainerInput) container.ExecutionsEnvironment {
	if env := rc.jobDockerEnv(); env != nil {
		return env.NewContainer(input)
	}
	return docker.NewContainer(input)
}

func (rc *RunContext) newNetworkCreateExecutor(name string, config *network.CreateOptions) common.Executor {
	if env := rc.jobDockerEnv(); env != nil {
		return env.NewNetworkCreateExecutor(name, config)
	}
	return docker.NewDockerNetworkCreateExecutor(name, config)
}

func (rc *RunContext) newNetworkRemoveExecutor(name string) common.Executor {
	if env := rc.jobDockerEnv(); env != nil {
		return env.NewNetworkRemoveExecutor(name)
	}
	return docker.NewDockerNetworkRemoveExecutor(name)
}

func (rc *RunContext) newVolumesRemoveExecutor(volumeNames []string) common.Executor {
	if env := rc.jobDockerEnv(); env != nil {
		return env.NewVolumesRemoveExecutor(volumeNames)
	}
	return docker.NewDockerVolumesRemoveExecutor(volumeNames)
}

func (rc *RunContext) imageExistsLocally(ctx context.Context, image, platform string) (bool, error) {
	if env := rc.jobDockerEnv(); env != nil {
		return env.ImageExistsLocally(ctx, image, platform)
	}
	return docker.ImageExistsLocally(ctx, image, platform)
}

func (rc *RunContext) newDockerBuildExecutor(input docker.NewDockerBuildExecutorInput) common.Executor {
	if env := rc.jobDockerEnv(); env != nil {
		return env.NewBuildExecutor(input)
	}
	return docker.NewDockerBuildExecutor(input)
}

func (rc *RunContext) runnerArch(ctx context.Context) string {
	if env := rc.jobDockerEnv(); env != nil {
		return env.RunnerArch(ctx)
	}
	return docker.RunnerArch(ctx)
}
