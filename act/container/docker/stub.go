//go:build WITHOUT_DOCKER || !(linux || darwin || windows || freebsd || openbsd)

// Build stub for platforms without Docker support (the WITHOUT_DOCKER tag, or
// any GOOS outside linux/darwin/windows/freebsd/openbsd). Mirrors the API
// surface of the real Docker backend but returns "unsupported" errors. The
// runner can still be built and the non-docker code paths exercised.
package docker

import (
	"context"
	"errors"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	actcontainer "code.forgejo.org/forgejo/runner/v12/act/container"
	"github.com/docker/docker/api/types/network"
)

func NewEndpoint(ctx context.Context, dockerHost string) (Endpoint, error) {
	return nil, errors.New("Unsupported Operation")
}

// ImageExistsLocally returns a boolean indicating if an image with the
// requested name, tag and architecture exists in the local docker image store
func ImageExistsLocally(ctx context.Context, ep Endpoint, imageName, platform string) (bool, error) {
	return false, errors.New("Unsupported Operation")
}

// RemoveImage removes image from local store, the function is used to run different
// container image architectures
func RemoveImage(ctx context.Context, ep Endpoint, imageName string, force, pruneChildren bool) (bool, error) {
	return false, errors.New("Unsupported Operation")
}

// NewDockerBuildExecutor function to create a run executor for the container
func NewDockerBuildExecutor(ep Endpoint, input NewDockerBuildExecutorInput) common.Executor {
	return func(ctx context.Context) error {
		return errors.New("Unsupported Operation")
	}
}

// NewDockerPullExecutor function to create a run executor for the container
func NewDockerPullExecutor(ep Endpoint, input NewDockerPullExecutorInput) common.Executor {
	return func(ctx context.Context) error {
		return errors.New("Unsupported Operation")
	}
}

// NewContainer creates a reference to a container
func NewContainer(ep Endpoint, input *actcontainer.NewContainerInput) actcontainer.ExecutionsEnvironment {
	return nil
}

// ExecutionEnvironment mirrors the real type so the runner builds without Docker.
type ExecutionEnvironment struct {
	ep Endpoint
}

// NewExecutionEnvironment binds an ExecutionEnvironment to a daemon Endpoint.
func NewExecutionEnvironment(ep Endpoint) *ExecutionEnvironment {
	return &ExecutionEnvironment{ep: ep}
}

// Endpoint returns the daemon these containers run against.
func (x *ExecutionEnvironment) Endpoint() Endpoint {
	return x.ep
}

func (x *ExecutionEnvironment) NewJobContainer(input *actcontainer.NewContainerInput) actcontainer.ExecutionsEnvironment {
	return NewContainer(x.ep, input)
}

func (x *ExecutionEnvironment) NewServiceContainer(input *actcontainer.NewContainerInput) actcontainer.ExecutionsEnvironment {
	return NewContainer(x.ep, input)
}

func (x *ExecutionEnvironment) NewStepContainer(input *actcontainer.NewContainerInput) actcontainer.ExecutionsEnvironment {
	return NewContainer(x.ep, input)
}

func NewDockerVolumesRemoveExecutor(ep Endpoint, volumeNames []string) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}

func NewDockerNetworkCreateExecutor(ep Endpoint, name string, config *network.CreateOptions) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}

func NewDockerNetworkRemoveExecutor(ep Endpoint, name string) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}
