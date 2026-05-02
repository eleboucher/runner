//go:build !WITHOUT_DOCKER && (linux || darwin || windows || freebsd || openbsd)

package docker

import (
	"context"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"github.com/docker/docker/api/types/network"
)

func NewDockerNetworkCreateExecutor(ep Endpoint, name string, config *network.CreateOptions) common.Executor {
	return func(ctx context.Context) error {
		cli := ep.Client()

		// Only create the network if it doesn't exist
		networks, err := cli.NetworkList(ctx, network.ListOptions{})
		if err != nil {
			return err
		}
		for _, network := range networks {
			if network.Name == name {
				common.Logger(ctx).Debugf("Network %v exists", name)
				return nil
			}
		}

		_, err = cli.NetworkCreate(ctx, name, *config)
		if err != nil {
			return err
		}

		return nil
	}
}

func NewDockerNetworkRemoveExecutor(ep Endpoint, name string) common.Executor {
	return func(ctx context.Context) error {
		cli := ep.Client()

		// Make shure that all network of the specified name are removed
		// cli.NetworkRemove refuses to remove a network if there are duplicates
		networks, err := cli.NetworkList(ctx, network.ListOptions{})
		if err != nil {
			return err
		}
		common.Logger(ctx).Debugf("%v", networks)
		for _, net := range networks {
			if net.Name == name {
				result, err := cli.NetworkInspect(ctx, net.ID, network.InspectOptions{})
				if err != nil {
					return err
				}
				if len(result.Containers) != 0 {
					common.Logger(ctx).Debugf("Refusing to remove network %v because it still has active endpoints", name)
					continue
				}
				if err := cli.NetworkRemove(ctx, net.ID); err != nil {
					return err
				}
			}
		}
		return nil
	}
}
