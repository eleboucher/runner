//go:build WITHOUT_DOCKER || !(linux || darwin || windows || freebsd || openbsd)

package docker

import (
	"context"
	"errors"
	"runtime"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
)

// ImageExistsLocally returns a boolean indicating if an image with the
// requested name, tag and architecture exists in the local docker image store
func ImageExistsLocally(ctx context.Context, imageName, platform string) (bool, error) {
	return false, errors.New("Unsupported Operation")
}

// RemoveImage removes image from local store, the function is used to run different
// container image architectures
func RemoveImage(ctx context.Context, imageName string, force, pruneChildren bool) (bool, error) {
	return false, errors.New("Unsupported Operation")
}

// NewDockerBuildExecutor function to create a run executor for the container
func NewDockerBuildExecutor(input NewDockerBuildExecutorInput) common.Executor {
	return func(ctx context.Context) error {
		return errors.New("Unsupported Operation")
	}
}

func CurrentSystemPlatform(ctx context.Context) (string, error) {
	return "", errors.New("Unsupported Operation")
}

// NewDockerPullExecutor function to create a run executor for the container
func NewDockerPullExecutor(input NewDockerPullExecutorInput) common.Executor {
	return func(ctx context.Context) error {
		return errors.New("Unsupported Operation")
	}
}

// NewContainer creates a reference to a container
func NewContainer(input *NewContainerInput) ExecutionsEnvironment {
	return nil
}

func RunnerArch(ctx context.Context) string {
	return runtime.GOOS
}

func GetHostInfo(ctx context.Context) (info system.Info, err error) {
	return system.Info{}, nil
}

func NewDockerVolumesRemoveExecutor(volumeNames []string) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}

func NewDockerNetworkCreateExecutor(name string, config *network.CreateOptions) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}

func NewDockerNetworkRemoveExecutor(name string) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}
