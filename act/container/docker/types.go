package docker

import "io"

// NewDockerBuildExecutorInput is the input for NewDockerBuildExecutor.
type NewDockerBuildExecutorInput struct {
	ContextDir   string
	Dockerfile   string
	BuildContext io.Reader
	ImageTag     string
	Platform     string
}

// NewDockerPullExecutorInput is the input for NewDockerPullExecutor.
type NewDockerPullExecutorInput struct {
	Image     string
	ForcePull bool
	Platform  string
	Username  string
	Password  string
}
