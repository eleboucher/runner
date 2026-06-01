package runner

import (
	"bytes"
	"context"
	"io"
	"testing"

	"code.forgejo.org/forgejo/runner/v12/act/container"
	"code.forgejo.org/forgejo/runner/v12/act/container/docker"
	"code.forgejo.org/forgejo/runner/v12/act/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestStepDockerMain(t *testing.T) {
	cm := &containerMock{}

	var input *container.NewContainerInput

	// mock the new container call
	origNewContainer := docker.NewContainer
	docker.NewContainer = func(_ docker.Endpoint, containerInput *container.NewContainerInput) container.ExecutionsEnvironment {
		input = containerInput
		return cm
	}
	defer func() {
		docker.NewContainer = origNewContainer
	}()

	ctx := t.Context()

	sd := &stepDocker{
		RunContext: &RunContext{
			StepResults: map[string]*model.StepResult{},
			Config:      &Config{},
			Run: &model.Run{
				JobID: "1",
				Workflow: &model.Workflow{
					Jobs: map[string]*model.Job{
						"1": {
							Defaults: model.Defaults{
								Run: model.RunDefaults{
									Shell: "bash",
								},
							},
						},
					},
				},
			},
			JobContainer: cm,
		},
		Step: &model.Step{
			ID:               "1",
			Uses:             "docker://node:14",
			WorkingDirectory: "workdir",
		},
	}
	sd.RunContext.ExprEval = sd.RunContext.NewExpressionEvaluator(ctx)

	cm.On("Pull", false).Return(func(ctx context.Context) error {
		return nil
	})

	cm.On("Remove").Return(func(ctx context.Context) error {
		return nil
	})

	cm.On("Create", []string(nil), []string(nil)).Return(func(ctx context.Context) error {
		return nil
	})

	cm.On("Start", true).Return(func(ctx context.Context) error {
		return nil
	})

	cm.On("Close").Return(func(ctx context.Context) error {
		return nil
	})

	cm.On("Copy", "/var/run/act", mock.AnythingOfType("[]*container.FileEntry")).Return(func(ctx context.Context) error {
		return nil
	})

	cm.On("UpdateFromEnv", "/var/run/act/workflow/envs.txt", mock.AnythingOfType("*map[string]string")).Return(func(ctx context.Context) error {
		return nil
	})

	cm.On("UpdateFromEnv", "/var/run/act/workflow/statecmd.txt", mock.AnythingOfType("*map[string]string")).Return(func(ctx context.Context) error {
		return nil
	})

	cm.On("UpdateFromEnv", "/var/run/act/workflow/outputcmd.txt", mock.AnythingOfType("*map[string]string")).Return(func(ctx context.Context) error {
		return nil
	})

	cm.On("GetContainerArchive", ctx, "/var/run/act/workflow/SUMMARY.md").Return(io.NopCloser(&bytes.Buffer{}), nil)
	cm.On("GetContainerArchive", ctx, "/var/run/act/workflow/pathcmd.txt").Return(io.NopCloser(&bytes.Buffer{}), nil)

	err := sd.main()(ctx)
	assert.Nil(t, err)

	assert.Equal(t, "node:14", input.Image)

	cm.AssertExpectations(t)
}

func TestStepDockerPrePost(t *testing.T) {
	ctx := t.Context()
	sd := &stepDocker{}

	err := sd.pre()(ctx)
	assert.Nil(t, err)

	err = sd.post()(ctx)
	assert.Nil(t, err)
}

// TestStepDockerNetworkConfiguration tests that step containers are created with proper network configuration
func TestStepDockerNetworkConfiguration(t *testing.T) {
	var input *container.NewContainerInput

	origNewContainer := docker.NewContainer
	docker.NewContainer = func(_ docker.Endpoint, containerInput *container.NewContainerInput) container.ExecutionsEnvironment {
		input = containerInput
		return &containerMock{}
	}
	defer func() {
		docker.NewContainer = origNewContainer
	}()

	ctx := t.Context()

	cm := &containerMock{}
	rc := &RunContext{
		StepResults: map[string]*model.StepResult{},
		Config:      &Config{Workdir: "/workspace"},
		Run: &model.Run{
			JobID: "test-job",
			Workflow: &model.Workflow{
				Jobs: map[string]*model.Job{
					"test-job": {},
				},
			},
		},
		JobContainer: cm,
	}

	sd := &stepDocker{
		RunContext: rc,
		Step: &model.Step{
			ID:   "test-step-1",
			Uses: "docker://alpine:latest",
		},
		env: map[string]string{},
	}

	// Call newStepContainer directly to test network configuration
	_ = sd.newStepContainer(ctx, docker.NewExecutionEnvironment(fakeEndpoint{}), "alpine:latest", nil, nil)

	// Verify network configuration
	// NetworkMode should use rc.getNetworkName() instead of container:jobContainerName
	// This ensures the step container doesn't inherit the wrong hostname from job container
	assert.NotEmpty(t, input.NetworkMode, "NetworkMode should be set")
	assert.NotContains(t, input.NetworkMode, "container:", "NetworkMode should not use container: mode")

	// Verify NetworkAliases is set with sanitized step ID
	assert.NotNil(t, input.NetworkAliases, "NetworkAliases should be set")
	assert.Len(t, input.NetworkAliases, 1, "Should have exactly one network alias")
	assert.Equal(t, "test-step-1", input.NetworkAliases[0], "Network alias should be sanitized step ID")
}
