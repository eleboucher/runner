package docker

import "github.com/docker/docker/client"

type Endpoint interface {
	Client() client.APIClient
	Close() error
	RunnerArch() string
	CurrentSystemPlatform() string
}
