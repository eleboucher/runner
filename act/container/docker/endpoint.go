//go:build !WITHOUT_DOCKER && (linux || darwin || windows || freebsd || openbsd)

package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/cli/cli/connhelper"
	"github.com/docker/docker/client"
)

type endpoint struct {
	cli    client.APIClient
	arch   string
	osType string
}

var _ Endpoint = (*endpoint)(nil)

func normalizeArch(arch string) string {
	switch arch {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64":
		return "arm64"
	}
	return arch
}

// NewEndpoint dials the daemon at the given host and queries its info. An
// empty host falls back to the docker client's defaults.
func NewEndpoint(ctx context.Context, dockerHost string) (Endpoint, error) {
	cli, err := dialDockerDaemon(ctx, dockerHost)
	if err != nil {
		return nil, err
	}
	return NewEndpointFromClient(ctx, cli)
}

// NewEndpointFromClient wraps an already-dialled client in an Endpoint,
// querying the daemon for its architecture and OS. It takes ownership of cli.
// Use it instead of NewEndpoint when dialling with bespoke options (e.g. TLS).
func NewEndpointFromClient(ctx context.Context, cli client.APIClient) (Endpoint, error) {
	info, err := cli.Info(ctx)
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("failed to query docker info: %w", err)
	}
	return &endpoint{
		cli:    cli,
		arch:   normalizeArch(info.Architecture),
		osType: info.OSType,
	}, nil
}

func dialDockerDaemon(ctx context.Context, dockerHost string) (client.APIClient, error) {
	var (
		cli client.APIClient
		err error
	)
	if strings.HasPrefix(dockerHost, "ssh://") {
		helper, helperErr := connhelper.GetConnectionHelper(dockerHost)
		if helperErr != nil {
			return nil, helperErr
		}
		cli, err = client.NewClientWithOpts(
			client.WithHost(helper.Host),
			client.WithDialContext(helper.Dialer),
		)
	} else if dockerHost != "" {
		cli, err = client.NewClientWithOpts(client.WithHost(dockerHost))
	} else {
		cli, err = client.NewClientWithOpts(client.FromEnv)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to docker daemon: %w", err)
	}
	cli.NegotiateAPIVersion(ctx)
	return cli, nil
}

func (e *endpoint) Client() client.APIClient { return e.cli }

func (e *endpoint) Close() error {
	if e.cli == nil {
		return nil
	}
	return e.cli.Close()
}

// RunnerArch translates the daemon's architecture to the value expected for
// RUNNER_ARCH.
func (e *endpoint) RunnerArch() string {
	switch e.arch {
	case "amd64":
		return "X64"
	case "386":
		return "X86"
	case "arm64":
		return "ARM64"
	}
	return e.arch
}

// CurrentSystemPlatform returns the daemon's platform as "os/arch", normalised
// to the values used for image tagging (docker info reports the kernel arch).
func (e *endpoint) CurrentSystemPlatform() string {
	return fmt.Sprintf("%s/%s", e.osType, e.arch)
}
