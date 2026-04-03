//go:build !WITHOUT_KUBERNETES && (linux || darwin || windows || freebsd || openbsd)

package container

import (
	"context"
	"fmt"

	"code.forgejo.org/forgejo/runner/v12/act/common"
)

const (
	k8sMainContainerName = "main"
	k8sSharedMount       = "/shared"
	k8sActPath           = k8sSharedMount + "/act"
	k8sToolCache         = k8sSharedMount + "/toolcache"
	k8sWorkDir           = k8sSharedMount + "/workdir"
)

func NewK8sPod(input *NewContainerInput, config *K8sPodConfig) (ExecutionsEnvironment, error) {
	if config == nil {
		return nil, fmt.Errorf("K8sPodConfig is required")
	}

	client, restCfg, err := GetK8sClient(config.KubeConfig)
	if err != nil {
		return nil, fmt.Errorf("init k8s client: %w", err)
	}

	ns := config.Namespace
	if ns == "" {
		ns = "default"
	}

	p := &K8sPod{
		client:    client,
		restCfg:   restCfg,
		namespace: ns,
		input:     *input,
		config:    config,
		stdout:    input.Stdout,
		stderr:    input.Stderr,
	}
	p.toolCache = k8sToolCache
	return p, nil
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
