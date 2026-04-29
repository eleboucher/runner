package container

import "context"

type ExecutionsEnvironment interface {
	Container
	ToContainerPath(string) string
	GetName() string
	GetRoot() string
	// BackendID returns a stable, unique identifier for the back-end.
	// Format: lowercase ASCII, alphanumeric plus dashes/underscores (slug form).
	// Used for error messages, log fields, and for plugins matching against
	// runs-on labels. Renames are breaking.
	BackendName() string
	// SupportsDockerActions reports whether the back-end can run Docker
	// container actions (`runs.using: docker` and `uses: docker://...`).
	SupportsDockerActions() bool
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
