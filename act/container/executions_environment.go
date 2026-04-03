package container

import "context"

type ServiceAdder interface {
	AddServiceContainerRaw(name, image string, env map[string]string, ports []string)
}

type ExecutionsEnvironment interface {
	Container
	ToContainerPath(string) string
	GetName() string
	GetRoot() string
	BackendName() string
	SupportsDockerActions() bool
	ManagesOwnNetworking() bool
	GetActPath() string
	GetPathVariableName() string
	DefaultPathVariable() string
	JoinPathVariable(...string) string
	GetRunnerContext(ctx context.Context) map[string]any
	// On windows PATH and Path are the same key
	IsEnvironmentCaseInsensitive() bool
}
