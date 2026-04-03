//go:build WITHOUT_KUBERNETES || !(linux || darwin || windows || freebsd || openbsd)

package container

import (
	"context"
	"fmt"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/common"
)

type K8sPodConfig struct {
	Namespace   string
	PodSpec     string
	KubeConfig  string
	PollTimeout time.Duration
	JobTimeout  time.Duration
}

func NewK8sPod(_ *NewContainerInput, _ *K8sPodConfig) (ExecutionsEnvironment, error) {
	return nil, fmt.Errorf("kubernetes support not compiled in (rebuild without the WITHOUT_KUBERNETES build tag to enable)")
}

func NewK8sNetworkCreateExecutor(_ string) common.Executor {
	return func(_ context.Context) error {
		return nil
	}
}

func NewK8sNetworkRemoveExecutor(_ string) common.Executor {
	return func(_ context.Context) error {
		return nil
	}
}
