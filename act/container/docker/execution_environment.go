//go:build !WITHOUT_DOCKER && (linux || darwin || windows || freebsd || openbsd)

package docker

import (
	actcontainer "code.forgejo.org/forgejo/runner/v12/act/container"
)

// ExecutionEnvironment creates the job, service, and step containers a job
// needs, all against one Endpoint. It is the reusable Docker piece the docker
// and host back-ends and a delegating plug-in's daemon share, so container
// creation is the same regardless of which back-end selected the daemon.
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

// NewJobContainer creates the container the job runs in.
func (x *ExecutionEnvironment) NewJobContainer(input *actcontainer.NewContainerInput) actcontainer.ExecutionsEnvironment {
	return NewContainer(x.ep, input)
}

// NewServiceContainer creates a service container for the job.
func (x *ExecutionEnvironment) NewServiceContainer(input *actcontainer.NewContainerInput) actcontainer.ExecutionsEnvironment {
	return NewContainer(x.ep, input)
}

// NewStepContainer creates the container a single step runs in (container
// actions and `uses: docker://`).
func (x *ExecutionEnvironment) NewStepContainer(input *actcontainer.NewContainerInput) actcontainer.ExecutionsEnvironment {
	return NewContainer(x.ep, input)
}
