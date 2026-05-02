//go:build !WITHOUT_DOCKER && (linux || darwin || windows || freebsd || openbsd)

package docker

import (
	"context"
	"slices"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"github.com/avast/retry-go/v4"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
)

func NewDockerVolumesRemoveExecutor(ep Endpoint, volumeNames []string) common.Executor {
	return func(ctx context.Context) error {
		cli := ep.Client()

		list, err := cli.VolumeList(ctx, volume.ListOptions{Filters: filters.NewArgs()})
		if err != nil {
			return err
		}

		for _, vol := range list.Volumes {
			if slices.Contains(volumeNames, vol.Name) {
				if err := removeExecutor(ep, vol.Name)(ctx); err != nil {
					return err
				}
			}
		}

		return nil
	}
}

func removeExecutor(ep Endpoint, volume string) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		logger.Debugf("%sdocker volume rm %s", logPrefix, volume)

		if common.Dryrun(ctx) {
			return nil
		}

		return retry.Do(
			func() error {
				force := false
				err := ep.Client().VolumeRemove(ctx, volume, force)
				if err != nil {
					if cerrdefs.IsNotFound(err) {
						logger.Debugf("volume %s not found, considering this as a success", volume)
						return nil
					}
					return err
				}
				return nil
			},
			retry.Context(ctx),
			retry.OnRetry(func(n uint, err error) {
				logger.Warnf("failed to remove docker volume %s (retry #%d): %s\n", volume, n, err)
			}),
		)
	}
}
