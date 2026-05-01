package runner

import (
	"context"
	"io"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"code.forgejo.org/forgejo/runner/v12/act/container"
	"github.com/stretchr/testify/mock"
)

type containerMock struct {
	mock.Mock
	container.Container
	container.LinuxContainerEnvironmentExtensions
}

func (cm *containerMock) Create(capAdd, capDrop []string) common.Executor {
	args := cm.Called(capAdd, capDrop)
	return args.Get(0).(func(context.Context) error)
}

func (cm *containerMock) Start(attach bool) common.Executor {
	args := cm.Called(attach)
	return args.Get(0).(func(context.Context) error)
}

func (cm *containerMock) Remove() common.Executor {
	args := cm.Called()
	return args.Get(0).(func(context.Context) error)
}

func (cm *containerMock) Close() common.Executor {
	args := cm.Called()
	return args.Get(0).(func(context.Context) error)
}

func (cm *containerMock) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	args := cm.Called(srcPath, env)
	return args.Get(0).(func(context.Context) error)
}

func (cm *containerMock) UpdateFromImageEnv(env *map[string]string) common.Executor {
	args := cm.Called(env)
	return args.Get(0).(func(context.Context) error)
}

func (cm *containerMock) Copy(destPath string, files ...*container.FileEntry) common.Executor {
	args := cm.Called(destPath, files)
	return args.Get(0).(func(context.Context) error)
}

func (cm *containerMock) CopyDir(destPath, srcPath string, useGitIgnore bool) common.Executor {
	args := cm.Called(destPath, srcPath, useGitIgnore)
	return args.Get(0).(func(context.Context) error)
}

func (cm *containerMock) Exec(command []string, env map[string]string, user, workdir string) common.Executor {
	args := cm.Called(command, env, user, workdir)
	return args.Get(0).(func(context.Context) error)
}

func (cm *containerMock) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	args := cm.Called(ctx, srcPath)
	err, hasErr := args.Get(1).(error)
	if !hasErr {
		err = nil
	}
	return args.Get(0).(io.ReadCloser), err
}
