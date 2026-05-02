package runner

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"code.forgejo.org/forgejo/runner/v12/act/container"
	"code.forgejo.org/forgejo/runner/v12/act/container/docker"
	"code.forgejo.org/forgejo/runner/v12/act/model"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"gotest.tools/v3/skip"
)

func TestJobExecutor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	skip.If(t, runtime.GOOS != "linux") // Windows and macOS cannot natively run Linux containers
	tables := []TestJobFileInfo{
		{workdir, "uses-and-run-in-one-step", "push", "Invalid run/uses syntax for job:test step:Test", platforms, secrets},
		{workdir, "uses-github-empty", "push", "job:test step:empty", platforms, secrets},
		{workdir, "uses-github-noref", "push", "Expected format {org}/{repo}[/path]@ref", platforms, secrets},
		{workdir, "uses-github-root", "push", "", platforms, secrets},
		{workdir, "uses-github-path", "push", "", platforms, secrets},
		{workdir, "uses-docker-url", "push", "", platforms, secrets},
		{workdir, "uses-github-full-sha", "push", "", platforms, secrets},
		{workdir, "uses-github-short-sha", "push", "short references are not supported", platforms, secrets},
		{workdir, "job-nil-step", "push", "invalid Step 0: missing run or uses key", platforms, secrets},
	}
	// These tests are sufficient to only check syntax.
	ctx := common.WithDryrun(t.Context(), true)
	for _, table := range tables {
		t.Run(table.workflowPath, func(t *testing.T) {
			table.runTest(ctx, t, &Config{})
		})
	}
}

type jobInfoMock struct {
	mock.Mock
}

func (jim *jobInfoMock) matrix() map[string]any {
	args := jim.Called()
	return args.Get(0).(map[string]any)
}

func (jim *jobInfoMock) steps() []*model.Step {
	args := jim.Called()

	return args.Get(0).([]*model.Step)
}

func (jim *jobInfoMock) startContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) stopContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) closeContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) interpolateOutputs() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) result(result string) {
	jim.Called(result)
}

type jobContainerMock struct {
	container.Container
	docker.LinuxContainerEnvironmentExtensions
}

func (jcm *jobContainerMock) ReplaceLogWriter(_, _ io.Writer) (io.Writer, io.Writer) {
	return nil, nil
}

type stepFactoryMock struct {
	mock.Mock
}

func (sfm *stepFactoryMock) newStep(model *model.Step, rc *RunContext) (step, error) {
	args := sfm.Called(model, rc)
	return args.Get(0).(step), args.Error(1)
}

func TestJobExecutorNewJobExecutor(t *testing.T) {
	table := []struct {
		name          string
		steps         []*model.Step
		preSteps      []bool
		postSteps     []bool
		executedSteps []string
		result        string
		hasError      bool
	}{
		{
			name:          "zeroSteps",
			steps:         []*model.Step{},
			preSteps:      []bool{},
			postSteps:     []bool{},
			executedSteps: []string{},
			result:        "success",
			hasError:      false,
		},
		{
			name: "stepWithoutPrePost",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{false},
			postSteps: []bool{false},
			executedSteps: []string{
				"startContainer",
				"step1",
				"interpolateOutputs",
				"setJobResults",
				"stopContainer",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepWithFailure",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{false},
			postSteps: []bool{false},
			executedSteps: []string{
				"startContainer",
				"step1",
				"interpolateOutputs",
				"setJobResults",
				"stopContainer",
				"closeContainer",
			},
			result:   "failure",
			hasError: true,
		},
		{
			name: "stepWithPre",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{true},
			postSteps: []bool{false},
			executedSteps: []string{
				"startContainer",
				"pre1",
				"step1",
				"interpolateOutputs",
				"setJobResults",
				"stopContainer",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepWithPost",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{false},
			postSteps: []bool{true},
			executedSteps: []string{
				"startContainer",
				"step1",
				"post1",
				"interpolateOutputs",
				"setJobResults",
				"stopContainer",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepWithPreAndPost",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{true},
			postSteps: []bool{true},
			executedSteps: []string{
				"startContainer",
				"pre1",
				"step1",
				"post1",
				"interpolateOutputs",
				"setJobResults",
				"stopContainer",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepsWithPreAndPost",
			steps: []*model.Step{{
				ID:     "1",
				Number: 0,
			}, {
				ID:     "2",
				Number: 1,
			}, {
				ID:     "3",
				Number: 2,
			}},
			preSteps:  []bool{true, false, true},
			postSteps: []bool{false, true, true},
			executedSteps: []string{
				"startContainer",
				"pre1",
				"pre3",
				"step1",
				"step2",
				"step3",
				"post3",
				"post2",
				"interpolateOutputs",
				"setJobResults",
				"stopContainer",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
	}

	contains := func(needle string, haystack []string) bool {
		return slices.Contains(haystack, needle)
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			fmt.Printf("::group::%s\n", tt.name)

			executorOrder := make([]string, 0)

			mockLogger := NewMockFieldLogger(t)
			mockLogger.On("Debugf", mock.Anything, mock.Anything, mock.Anything).Return(0).Maybe()
			mockLogger.On("Warningf", mock.Anything, mock.Anything, mock.Anything).Return(0).Maybe()
			mockLogger.On("WithField", mock.Anything, mock.Anything, mock.Anything).Return(&logrus.Entry{Logger: &logrus.Logger{}}).Maybe()
			// When `WithFields()` is called with jobResult & jobOutputs field, add `setJobResults` to executorOrder.
			mockLogger.On("WithFields",
				mock.MatchedBy(func(fields logrus.Fields) bool {
					_, okJobResult := fields["jobResult"]
					_, okJobOutput := fields["jobOutputs"]
					return okJobOutput && okJobResult
				})).
				Run(func(args mock.Arguments) {
					executorOrder = append(executorOrder, "setJobResults")
				}).
				Return(&logrus.Entry{Logger: &logrus.Logger{}}).Maybe()

			mockLogger.On("WithFields", mock.Anything).Return(&logrus.Entry{Logger: &logrus.Logger{}}).Maybe()

			ctx := common.WithLogger(common.WithJobErrorContainer(t.Context()), mockLogger)
			jim := &jobInfoMock{}
			sfm := &stepFactoryMock{}
			rc := &RunContext{
				JobContainer: &jobContainerMock{},
				Run: &model.Run{
					JobID: "test",
					Workflow: &model.Workflow{
						Jobs: map[string]*model.Job{
							"test": {},
						},
					},
				},
				Config: &Config{},
			}
			rc.ExprEval = rc.NewExpressionEvaluator(ctx)

			jim.On("steps").Return(tt.steps)

			if len(tt.steps) > 0 {
				jim.On("startContainer").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "startContainer")
					return nil
				})
			}

			for i, stepModel := range tt.steps {
				sm := &stepMock{}

				sfm.On("newStep", stepModel, rc).Return(sm, nil)

				sm.On("pre").Return(func(ctx context.Context) error {
					if tt.preSteps[i] {
						executorOrder = append(executorOrder, "pre"+stepModel.ID)
					}
					return nil
				})

				sm.On("main").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "step"+stepModel.ID)
					if tt.hasError {
						return fmt.Errorf("error")
					}
					return nil
				})

				sm.On("post").Return(func(ctx context.Context) error {
					if tt.postSteps[i] {
						executorOrder = append(executorOrder, "post"+stepModel.ID)
					}
					return nil
				})

				defer sm.AssertExpectations(t)
			}

			if len(tt.steps) > 0 {
				jim.On("matrix").Return(map[string]any{})

				jim.On("interpolateOutputs").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "interpolateOutputs")
					return nil
				})

				if contains("stopContainer", tt.executedSteps) {
					jim.On("stopContainer").Return(func(ctx context.Context) error {
						executorOrder = append(executorOrder, "stopContainer")
						return nil
					})
				}

				jim.On("result", tt.result)

				jim.On("closeContainer").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "closeContainer")
					return nil
				})
			}

			executor := newJobExecutor(jim, sfm, rc)
			err := executor(ctx)
			assert.Nil(t, err)
			assert.Equal(t, tt.executedSteps, executorOrder)

			jim.AssertExpectations(t)
			sfm.AssertExpectations(t)

			fmt.Println("::endgroup::")
		})
	}
}

func TestSetJobResultConcurrency(t *testing.T) {
	jim := &jobInfoMock{}
	job := model.Job{
		Result: "success",
	}
	// Distinct RunContext objects are used to replicate realistic setJobResult in matrix build
	rc1 := &RunContext{
		Run: &model.Run{
			JobID: "test",
			Workflow: &model.Workflow{
				Jobs: map[string]*model.Job{
					"test": &job,
				},
			},
		},
	}
	rc2 := &RunContext{
		Run: &model.Run{
			JobID: "test",
			Workflow: &model.Workflow{
				Jobs: map[string]*model.Job{
					"test": &job,
				},
			},
		},
	}
	// Hack: Job() invokes GetJob() which can mutate the job name, this will trip the data race detector if it is
	// encountered later when `setJobResult()` is being tested.  This is a false-positive caused by this test invoking
	// setJobResult outside of the regular RunContext, so it's invoked here before the goroutines are spawned to prevent
	// the false positive.
	rc1.Run.Job()
	rc2.Run.Job()

	jim.On("matrix").Return(map[string]interface{}{
		"python": []string{"3.10", "3.11", "3.12"},
	})

	// Synthesize a race condition in setJobResult where, by reading data from the job matrix earlier and then
	// performing unsynchronzied writes to the same shared data structure, it can overwrite a failure status.
	//
	// Goroutine 1: Start marking job as success
	//              (artificially suspended
	// 				by result() mock)
	//												Goroutine 2: Mark job as failure
	// Goroutine 1: Finish marking job as success
	//
	// Correct behavior: Job is marked as a failure
	// Bug behavior: Job is marked as a success

	var lastResult string
	jim.On("result", mock.Anything).Run(func(args mock.Arguments) {
		result := args.String(0)
		// Artificially suspend the "success" case so that the failure case races past it.
		if result == "success" {
			time.Sleep(1 * time.Second)
		}
		job.Result = result
		lastResult = result
	})

	var wg sync.WaitGroup
	wg.Add(2)
	// Goroutine 1, mark as success:
	go func() {
		defer wg.Done()
		setJobResult(t.Context(), jim, rc1, true)
	}()
	// Goroutine 2, mark as failure:
	go func() {
		defer wg.Done()
		setJobResult(t.Context(), jim, rc2, false)
	}()
	wg.Wait()

	assert.Equal(t, "failure", lastResult)
}

func TestSetJobResult_SkipsBannerInChildReusableWorkflow(t *testing.T) {
	// Test that child reusable workflow does not print final banner
	// to prevent premature token revocation

	mockLogger := NewMockFieldLogger(t)
	// Allow all variants of Debugf (git operations can call with 1-3 args)
	mockLogger.On("Debugf", mock.Anything).Return(0).Maybe()
	mockLogger.On("Debugf", mock.Anything, mock.Anything).Return(0).Maybe()
	mockLogger.On("Debugf", mock.Anything, mock.Anything, mock.Anything).Return(0).Maybe()
	// CRITICAL: In CI, git ref detection may fail and call Warningf
	mockLogger.On("Warningf", mock.Anything, mock.Anything).Return(0).Maybe()
	mockLogger.On("WithField", mock.Anything, mock.Anything).Return(&logrus.Entry{Logger: &logrus.Logger{}}).Maybe()
	mockLogger.On("WithFields", mock.Anything).Return(&logrus.Entry{Logger: &logrus.Logger{}}).Maybe()

	ctx := common.WithLogger(common.WithJobErrorContainer(t.Context()), mockLogger)

	// Setup parent job
	parentJob := &model.Job{
		Result: "success",
	}
	parentRC := &RunContext{
		Config: &Config{Env: map[string]string{}}, // Must have Config
		Run: &model.Run{
			JobID: "parent",
			Workflow: &model.Workflow{
				Jobs: map[string]*model.Job{
					"parent": parentJob,
				},
			},
		},
	}

	// Setup child job with caller reference
	childJob := &model.Job{
		Result: "success",
	}
	childRC := &RunContext{
		Config: &Config{Env: map[string]string{}}, // Must have Config
		Run: &model.Run{
			JobID: "child",
			Workflow: &model.Workflow{
				Jobs: map[string]*model.Job{
					"child": childJob,
				},
			},
		},
		caller: &caller{
			runContext: parentRC,
		},
	}

	jim := &jobInfoMock{}
	jim.On("matrix").Return(map[string]any{}) // REQUIRED: setJobResult always calls matrix()
	jim.On("result", "success")

	// Call setJobResult for child workflow
	setJobResult(ctx, jim, childRC, true)

	// Verify:
	// 1. Child result is set
	jim.AssertCalled(t, "result", "success")

	// 2. Parent result is propagated
	assert.Equal(t, "success", parentJob.Result)

	// 3. Final banner was NOT printed by child (critical for token security)
	mockLogger.AssertNotCalled(t, "WithFields", mock.MatchedBy(func(fields logrus.Fields) bool {
		_, okJobResult := fields["jobResult"]
		_, okJobOutput := fields["jobOutputs"]
		return okJobOutput && okJobResult
	}))
}
