package runner

import (
	"context"
	"fmt"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"code.forgejo.org/forgejo/runner/v12/act/container"
	"code.forgejo.org/forgejo/runner/v12/act/model"
	"github.com/sirupsen/logrus"
)

type jobInfo interface {
	matrix() map[string]any
	steps() []*model.Step
	startContainer() common.Executor
	stopContainer() common.Executor
	closeContainer() common.Executor
	interpolateOutputs() common.Executor
	result(result string)
}

const cleanupTimeout = 30 * time.Minute

func newJobExecutor(info jobInfo, sf stepFactory, rc *RunContext) common.Executor {
	steps := make([]common.Executor, 0)
	preSteps := make([]common.Executor, 0)
	var postExecutor common.Executor

	steps = append(steps, func(ctx context.Context) error {
		logger := common.Logger(ctx)
		if len(info.matrix()) > 0 {
			logger.Infof("\U0001F9EA  Matrix: %v", info.matrix())
		}
		return nil
	})

	infoSteps := info.steps()

	if len(infoSteps) == 0 {
		return common.NewDebugExecutor("No steps found")
	}

	// setupWorkflowLevelEnv evaluates expressions in env, but only for workflow-level environment variables that can be
	// evaluated before a job container is created.
	setupWorkflowLevelEnv := func(ctx context.Context) error {
		// Have to be skipped for some Tests
		if rc.Run == nil {
			return nil
		}
		rc.ExprEval = rc.NewExpressionEvaluator(ctx)

		// Env already contains job variables, not only workflow variables. And those job variables might have
		// overridden workflow variables. Identify the workflow variables to interpolate by looping over
		// rc.Run.Workflow.Env, but take their _current_ value from Env.
		for k, v := range rc.Run.Workflow.Env {
			// Only interpolate the workflow-level env variable if its value hasn't been overridden by a job-level env
			// variable. Otherwise, it might contain a reference to a variable that is only visible on the job-level.
			// But that wouldn't be safe because the job-level hasn't been fully populated, yet.
			if currentValue, ok := rc.Env[k]; ok && currentValue == v {
				rc.Env[k] = rc.ExprEval.Interpolate(ctx, v)
			}
		}
		return nil
	}
	preSteps = append(preSteps, func(ctx context.Context) error {
		// Have to be skipped for some Tests
		if rc.Run == nil {
			return nil
		}
		rc.ExprEval = rc.NewExpressionEvaluator(ctx)
		// evaluate environment variables since they can contain
		// GitHub's special environment variables.
		for k, v := range rc.GetEnv() {
			rc.Env[k] = rc.ExprEval.Interpolate(ctx, v)
		}
		return nil
	})

	for i, stepModel := range infoSteps {
		if stepModel == nil {
			return common.NewErrorExecutor(fmt.Errorf("invalid Step %v: missing run or uses key", i))
		} else if stepModel.Number != i {
			return common.NewErrorExecutor(fmt.Errorf("internal error: invalid Step: Number expected %v, was actually %v", i, stepModel.Number))
		}

		step, err := sf.newStep(stepModel, rc)
		if err != nil {
			return common.NewErrorExecutor(err)
		}

		preExec := step.pre()
		preSteps = append(preSteps, useStepLogger(rc, stepModel, stepStagePre, func(ctx context.Context) error {
			logger := common.Logger(ctx)
			preErr := preExec(ctx)
			if preErr != nil {
				logger.WithField("raw_output", true).Errorf("%s %v", runnerLogPrefix, preErr)
				common.SetJobError(ctx, preErr)
			} else if ctx.Err() != nil {
				logger.WithField("raw_output", true).Errorf("%s %v", runnerLogPrefix, ctx.Err())
				common.SetJobError(ctx, ctx.Err())
			}
			return preErr
		}))

		stepExec := step.main()
		steps = append(steps, useStepLogger(rc, stepModel, stepStageMain, func(ctx context.Context) error {
			logger := common.Logger(ctx)
			err := stepExec(ctx)
			if err != nil {
				logger.WithField("raw_output", true).Errorf("%s %v", runnerLogPrefix, err)
				common.SetJobError(ctx, err)
			} else if ctx.Err() != nil {
				logger.WithField("raw_output", true).Errorf("%s %v", runnerLogPrefix, ctx.Err())
				common.SetJobError(ctx, ctx.Err())
			}
			return nil
		}))

		postExec := useStepLogger(rc, stepModel, stepStagePost, step.post())
		if postExecutor != nil {
			// run the post executor in reverse order
			postExecutor = postExec.Finally(postExecutor)
		} else {
			postExecutor = postExec
		}
	}

	setJobResults := func(ctx context.Context) error {
		jobError := common.JobError(ctx)

		// Fresh context to ensure job result output works even if prev. context was a cancelled job
		ctx, cancel := context.WithTimeout(common.WithLogger(context.Background(), common.Logger(ctx)), time.Minute)
		defer cancel()
		setJobResult(ctx, info, rc, jobError == nil)

		return nil
	}

	cleanupJob := func(_ context.Context) error {
		var err error

		// Separate timeout for cleanup tasks; logger is cleared so that cleanup logs go to runner, not job
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()

		logger := common.Logger(ctx)
		logger.Debugf("Cleaning up container for job %s", rc.jobContainerName())
		if err = info.stopContainer()(ctx); err != nil {
			logger.Errorf("Error while stop job container %s: %v", rc.jobContainerName(), err)
		}

		if rc.JobContainer != nil && !rc.JobContainer.ManagesOwnNetworking() && rc.getNetworkCreated(ctx) {
			networkName := rc.getNetworkName(ctx)
			logger.Debugf("Cleaning up network %s for job %s", networkName, rc.jobContainerName())
			if err := container.NewDockerNetworkRemoveExecutor(networkName)(ctx); err != nil {
				logger.Errorf("Error while cleaning network %s: %v", networkName, err)
			}
		}

		return err
	}

	pipeline := make([]common.Executor, 0, len(preSteps)+len(steps))
	pipeline = append(pipeline, preSteps...)
	pipeline = append(pipeline, steps...)

	return common.NewPipelineExecutor(
		setupWorkflowLevelEnv, // allows workflow-level env to be used in the job container's definition
		info.startContainer(),
		common.NewPipelineExecutor(pipeline...).
			Finally(func(ctx context.Context) error { //nolint:contextcheck
				var cancel context.CancelFunc
				if ctx.Err() == context.Canceled {
					// in case of an aborted run, we still should execute the
					// post steps to allow cleanup.
					ctx, cancel = context.WithTimeout(common.WithLogger(context.Background(), common.Logger(ctx)), cleanupTimeout)
					defer cancel()
				}
				return postExecutor(ctx)
			}).
			Finally(info.interpolateOutputs()).
			Finally(setJobResults).
			Finally(cleanupJob).
			Finally(info.closeContainer()))
}

func setJobResult(ctx context.Context, info jobInfo, rc *RunContext, success bool) {
	logger := common.Logger(ctx)

	// As we're reading the matrix build's status (`rc.Run.Job().Result`), it's possible for it change in another
	// goroutine running `setJobResult` and invoking `.result(...)`. Prevent concurrent execution of `setJobResult`...
	rc.Run.Job().ResultMutex.Lock()
	defer rc.Run.Job().ResultMutex.Unlock()

	jobResult := "success"
	// we have only one result for a whole matrix build, so we need
	// to keep an existing result state if we run a matrix
	if len(info.matrix()) > 0 && rc.Run.Job().Result != "" {
		jobResult = rc.Run.Job().Result
	}

	if !success {
		jobResult = "failure"
	}

	// Set local result on current job (child or parent)
	info.result(jobResult)

	if rc.caller != nil {
		// Child reusable workflow:
		// 1) propagate result to parent job state
		rc.caller.runContext.result(jobResult)

		// 2) copy workflow_call outputs from child to parent (as in upstream)
		jobOutputs := make(map[string]string)
		ee := rc.NewExpressionEvaluator(ctx)
		if wfcc := rc.Run.Workflow.WorkflowCallConfig(); wfcc != nil {
			for k, v := range wfcc.Outputs {
				jobOutputs[k] = ee.Interpolate(ctx, ee.Interpolate(ctx, v.Value))
			}
		}
		rc.caller.runContext.Run.Job().Outputs = jobOutputs

		// 3) DO NOT print banner in child job (prevents premature token revocation)
		logger.Debugf("Reusable job result=%s (parent will finalize, no banner)", jobResult)
		return
	}

	// Parent job: print the final banner ONCE (job-level)
	jobResultMessage := "succeeded"
	if jobResult != "success" {
		jobResultMessage = "failed"
	}
	jobOutputs := rc.Run.Job().Outputs

	logger.
		WithFields(logrus.Fields{
			"jobResult":  jobResult,
			"jobOutputs": jobOutputs,
		}).
		Infof("\U0001F3C1  Job %s", jobResultMessage)
}

func useStepLogger(rc *RunContext, stepModel *model.Step, stage stepStage, executor common.Executor) common.Executor {
	return func(ctx context.Context) error {
		ctx = withStepLogger(ctx, stepModel.Number, stepModel.ID, rc.ExprEval.Interpolate(ctx, stepModel.String()), stage.String())

		rawLogger := common.Logger(ctx).WithField("raw_output", true)
		logWriter := common.NewLineWriter(rc.commandHandler(ctx), func(s string) bool {
			if rc.Config.LogOutput {
				rawLogger.Infof("%s", s)
			} else {
				rawLogger.Debugf("%s", s)
			}
			return true
		})

		oldout, olderr := rc.JobContainer.ReplaceLogWriter(logWriter, logWriter)
		defer rc.JobContainer.ReplaceLogWriter(oldout, olderr)

		return executor(ctx)
	}
}
