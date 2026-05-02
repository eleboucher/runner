//go:build !WITHOUT_DOCKER && (linux || darwin || windows || freebsd || openbsd)

package docker

import (
	"context"
	"fmt"
	"strings"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func parsePlatform(platform string) (*v1.Platform, error) {
	desiredPlatform := strings.SplitN(platform, `/`, 2)
	if len(desiredPlatform) != 2 {
		return nil, fmt.Errorf("incorrect container platform option '%s'", platform)
	}
	platSpecs := &v1.Platform{
		Architecture: desiredPlatform[1],
		OS:           desiredPlatform[0],
	}
	return platSpecs, nil
}

// ImageExistsLocally returns a boolean indicating if an image with the
// requested name, tag and architecture exists in the local docker image store
func ImageExistsLocally(ctx context.Context, ep Endpoint, imageName, platform string) (bool, error) {
	logger := common.Logger(ctx)

	cli := ep.Client()

	if supportsImageInspectPlatform(ctx, cli) {
		platSpecs, err := parsePlatform(platform)
		if err != nil {
			return false, err
		}

		_, err = cli.ImageInspect(ctx, imageName, client.ImageInspectWithPlatform(platSpecs))
		if cerrdefs.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, err
		}

		return true, nil
	}

	inspectImage, err := cli.ImageInspect(ctx, imageName)
	if cerrdefs.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	imagePlatform := fmt.Sprintf("%s/%s", inspectImage.Os, inspectImage.Architecture)
	if platform == "" || platform == "any" || imagePlatform == platform {
		return true, nil
	}

	logger.Infof("Docker daemon does not support image platform inspection (API 1.49+ required) -- runner found an image but platform does not match expected: %s (image) != %s (platform)", imagePlatform, platform)

	return false, nil
}

// RemoveImage removes image from local store, the function is used to run different
// container image architectures
func RemoveImage(ctx context.Context, ep Endpoint, imageName string, force, pruneChildren bool) (bool, error) {
	cli := ep.Client()

	inspectImage, err := cli.ImageInspect(ctx, imageName)
	if cerrdefs.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	if _, err = cli.ImageRemove(ctx, inspectImage.ID, image.RemoveOptions{
		Force:         force,
		PruneChildren: pruneChildren,
	}); err != nil {
		return false, err
	}

	return true, nil
}
