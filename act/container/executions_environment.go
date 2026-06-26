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
	// BackendID returns a stable, unique identifier for the back-end.
	// Format: lowercase ASCII, alphanumeric plus dashes/underscores (slug form).
	// Used in error messages, log fields, and (for plugins) matching against
	// runs-on labels. Must be unique across registered back-ends. Renames are
	// breaking.
	BackendID() string
	// SupportsDockerContainerActions reports whether the back-end can run a
	// step that requires a Docker container action: `uses: docker://image` or
	// an action whose `runs.using: docker`. It does not gate job-level
	// `container:`, `services:`, or JS/composite actions.
	SupportsDockerContainerActions() bool
	// ManagesOwnNetworking reports whether the back-end provides its own
	// networking, in which case the runner skips creating the shared Docker
	// network used for services and aliases.
	ManagesOwnNetworking() bool
	GetActPath() string
	GetPathVariableName() string
	DefaultPathVariable() string
	JoinPathVariable(...string) string
	GetRunnerContext(ctx context.Context) map[string]any
	// On windows PATH and Path are the same key
	IsEnvironmentCaseInsensitive() bool
}
