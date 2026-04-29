package container

import (
	"context"
	"io"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/common"
)

// Port identifies a container port and protocol in the form "<number>/<protocol>" (e.g. "8080/tcp").
type Port string

// PortBinding represents a binding between a container port and a host address.
type PortBinding struct {
	HostIP   string
	HostPort string
}

// NewContainerInput the input for the New function
type NewContainerInput struct {
	Image           string
	Username        string
	Password        string
	Entrypoint      []string
	Cmd             []string
	Init            bool
	TTY             bool
	WorkingDir      string
	Env             []string
	ToolCache       string
	Binds           []string
	Mounts          map[string]string
	Name            string
	Stdout          io.Writer
	Stderr          io.Writer
	NetworkMode     string
	Privileged      bool
	UsernsMode      string
	DefaultPlatform string // platform if not overridden in JobOptions
	NetworkAliases  []string
	ExposedPorts    map[Port]struct{}
	PortBindings    map[Port][]PortBinding

	ConfigOptions string
	JobOptions    string

	ValidVolumes []string
}

// FileEntry is a file to copy to a container
type FileEntry struct {
	Name string
	Mode int64
	Body string
}

// Container for managing docker run containers
//
//mockery:generate: true
type Container interface {
	Create(capAdd, capDrop []string) common.Executor
	Copy(destPath string, files ...*FileEntry) common.Executor
	CopyTarStream(ctx context.Context, destPath string, tarStream io.Reader) error
	CopyDir(destPath, srcPath string, useGitIgnore bool) common.Executor
	GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error)
	Pull(forcePull bool) common.Executor
	Start(attach bool) common.Executor
	Exec(command []string, env map[string]string, user, workdir string) common.Executor
	UpdateFromEnv(srcPath string, env *map[string]string) common.Executor
	UpdateFromImageEnv(env *map[string]string) common.Executor
	Remove() common.Executor
	Close() common.Executor
	ReplaceLogWriter(io.Writer, io.Writer) (io.Writer, io.Writer)
	IsHealthy(ctx context.Context) (time.Duration, error)
}

// NewDockerBuildExecutorInput the input for the NewDockerBuildExecutor function
type NewDockerBuildExecutorInput struct {
	ContextDir   string
	Dockerfile   string
	BuildContext io.Reader
	ImageTag     string
	Platform     string
}

// NewDockerPullExecutorInput the input for the NewDockerPullExecutor function
type NewDockerPullExecutorInput struct {
	Image     string
	ForcePull bool
	Platform  string
	Username  string
	Password  string
}
