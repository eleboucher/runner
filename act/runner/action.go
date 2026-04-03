package runner

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/kballard/go-shellquote"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"code.forgejo.org/forgejo/runner/v12/act/container"
	"code.forgejo.org/forgejo/runner/v12/act/model"
)

type actionStep interface {
	step

	getActionModel() *model.Action
	getCompositeRunContext(context.Context) *RunContext
	getCompositeSteps() *compositeSteps
}

type readAction func(ctx context.Context, step *model.Step, actionDir, actionPath string, readFile actionYamlReader, writeFile fileWriter) (*model.Action, error)

type actionYamlReader func(filename string) (io.Reader, io.Closer, error)

type fileWriter func(filename string, data []byte, perm fs.FileMode) error

type runAction func(step actionStep, actionDir string, remoteAction *remoteAction) common.Executor

//go:embed res/trampoline.js
var trampoline embed.FS

func readActionImpl(ctx context.Context, step *model.Step, actionDir, actionPath string, readFile actionYamlReader, writeFile fileWriter) (*model.Action, error) {
	logger := common.Logger(ctx)
	otherErrors := []error{}
	filesNotFound := []string{}
	addError := func(fileName string, err error) {
		if err != nil {
			if os.IsNotExist(err) {
				filesNotFound = append(filesNotFound, fileName)
			} else {
				otherErrors = append(otherErrors, fmt.Errorf("failed to read '%s' from action '%s' with path '%s': %w", fileName, step.String(), actionPath, err))
			}
		} else {
			// One successful read, clear error state
			filesNotFound = nil
			otherErrors = nil
		}
	}
	reader, closer, err := readFile("action.yml")
	addError("action.yml", err)
	if os.IsNotExist(err) {
		reader, closer, err = readFile("action.yaml")
		addError("action.yaml", err)
		if os.IsNotExist(err) {
			_, closer, err := readFile("Dockerfile")
			addError("Dockerfile", err)
			if err == nil {
				closer.Close()
				action := &model.Action{
					Name: "(Synthetic)",
					Runs: model.ActionRuns{
						Using: "docker",
						Image: "Dockerfile",
					},
				}
				logger.Debugf("Using synthetic action %v for Dockerfile", action)
				return action, nil
			}
			if step.With != nil {
				if val, ok := step.With["args"]; ok {
					var b []byte
					if b, err = trampoline.ReadFile("res/trampoline.js"); err != nil {
						return nil, err
					}
					err2 := writeFile(filepath.Join(actionDir, actionPath, "trampoline.js"), b, 0o400)
					if err2 != nil {
						return nil, err2
					}
					action := &model.Action{
						Name: "(Synthetic)",
						Inputs: map[string]model.Input{
							"cwd": {
								Description: "(Actual working directory)",
								Required:    false,
								Default:     filepath.Join(actionDir, actionPath),
							},
							"command": {
								Description: "(Actual program)",
								Required:    false,
								Default:     val,
							},
						},
						Runs: model.ActionRuns{
							Using: "node12",
							Main:  "trampoline.js",
						},
					}
					logger.Debugf("Using synthetic action %v", action)
					return action, nil
				}
			}
		}
	}
	if len(otherErrors) > 0 {
		return nil, errors.Join(otherErrors...)
	}
	if len(filesNotFound) > 0 {
		return nil, fmt.Errorf("failed to read action '%s', no files found after reading paths: %s", step.String(), strings.Join(filesNotFound, ", "))
	}
	defer closer.Close()

	action, err := model.ReadAction(reader)
	if err != nil {
		err = fmt.Errorf("failed to validate action.y*ml from action '%s' with path '%s': %v", step.String(), actionPath, err)
	}
	logger.Debugf("Read action %v from '%s'", action, "Unknown")
	return action, err
}

func maybeCopyToActionDir(ctx context.Context, step actionStep, actionDir, actionPath, containerActionDir string) error {
	logger := common.Logger(ctx)
	rc := step.getRunContext()
	stepModel := step.getStepModel()

	if stepModel.Type() != model.StepTypeUsesActionRemote {
		return nil
	}

	var containerActionDirCopy string
	containerActionDirCopy = strings.TrimSuffix(containerActionDir, actionPath)
	logger.Debug(containerActionDirCopy)

	if !strings.HasSuffix(containerActionDirCopy, `/`) {
		containerActionDirCopy += `/`
	}

	if err := removeGitIgnore(ctx, actionDir); err != nil {
		return err
	}

	return rc.JobContainer.CopyDir(containerActionDirCopy, actionDir+"/", rc.Config.UseGitIgnore)(ctx)
}

func runActionImpl(step actionStep, actionDir string, remoteAction *remoteAction) common.Executor {
	rc := step.getRunContext()
	stepModel := step.getStepModel()

	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		actionPath := ""
		if remoteAction != nil && remoteAction.Path != "" {
			actionPath = remoteAction.Path
		}

		action := step.getActionModel()
		logger.Debugf("About to run action %v", action)

		actionLocation := path.Join(actionDir, actionPath)
		actionName, containerActionDir := getContainerActionPaths(stepModel, actionLocation, rc)

		if action.Runs.Using == model.ActionRunsUsingSh {
			rc.ActionPath = containerActionDir
		}

		err := setupActionEnv(ctx, step, remoteAction)
		if err != nil {
			return err
		}

		logger.Debugf("type=%v actionDir=%s actionPath=%s workdir=%s actionCacheDir=%s actionName=%s containerActionDir=%s", stepModel.Type(), actionDir, actionPath, rc.Config.Workdir, rc.ActionCacheDir(), actionName, containerActionDir)

		switch action.Runs.Using {
		case model.ActionRunsUsingNode12, model.ActionRunsUsingNode16, model.ActionRunsUsingNode20, model.ActionRunsUsingNode24:
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}

			containerArgs := []string{"node", path.Join(containerActionDir, action.Runs.Main)}
			logger.Debugf("executing remote job container: %s", containerArgs)

			rc.ApplyExtraPath(ctx, step.getEnv())

			return rc.execJobContainer(containerArgs, *step.getEnv(), "", "")(ctx)
		case model.ActionRunsUsingSh:
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}

			containerArgs := []string{"sh", "-c", action.Runs.Main}
			logger.Debugf("executing remote job container: %s", containerArgs)

			rc.ApplyExtraPath(ctx, step.getEnv())

			return rc.execJobContainer(containerArgs, *step.getEnv(), "", "")(ctx)
		case model.ActionRunsUsingDocker:
			if remoteAction == nil {
				actionDir = ""
				actionPath = containerActionDir
			}

			return execAsDocker(ctx, step, actionName, actionDir, actionPath, remoteAction == nil, "entrypoint")
		case model.ActionRunsUsingComposite:
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}

			return execAsComposite(step)(ctx)
		case model.ActionRunsUsingGo:
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}

			rc.ApplyExtraPath(ctx, step.getEnv())

			execFileName := fmt.Sprintf("%s.out", action.Runs.Main)
			buildArgs := []string{"go", "build", "-o", execFileName, action.Runs.Main}
			execArgs := []string{filepath.Join(containerActionDir, execFileName)}

			return common.NewPipelineExecutor(
				rc.execJobContainer(buildArgs, *step.getEnv(), "", containerActionDir),
				rc.execJobContainer(execArgs, *step.getEnv(), "", ""),
			)(ctx)
		default:
			return fmt.Errorf("The runs.using key must be one of: %v, got %s", []string{
				model.ActionRunsUsingDocker,
				model.ActionRunsUsingNode12,
				model.ActionRunsUsingNode16,
				model.ActionRunsUsingNode20,
				model.ActionRunsUsingNode24,
				model.ActionRunsUsingComposite,
				model.ActionRunsUsingGo,
				model.ActionRunsUsingSh,
			}, action.Runs.Using)
		}
	}
}

func setupActionEnv(ctx context.Context, step actionStep, _ *remoteAction) error {
	rc := step.getRunContext()

	// A few fields in the environment (e.g. GITHUB_ACTION_REPOSITORY)
	// are dependent on the action. That means we can complete the
	// setup only after resolving the whole action model and cloning
	// the action
	rc.withGithubEnv(ctx, step.getGithubContext(ctx), *step.getEnv())
	populateEnvsFromSavedState(step.getEnv(), step, rc)
	populateEnvsFromInput(ctx, step.getEnv(), step.getActionModel(), rc)

	return nil
}

// https://github.com/nektos/act/issues/228#issuecomment-629709055
// files in .gitignore are not copied in a Docker container
// this causes issues with actions that ignore other important resources
// such as `node_modules` for example
func removeGitIgnore(ctx context.Context, directory string) error {
	gitIgnorePath := path.Join(directory, ".gitignore")
	if _, err := os.Stat(gitIgnorePath); err == nil {
		// .gitignore exists
		common.Logger(ctx).Debugf("Removing %s before docker cp", gitIgnorePath)
		err := os.Remove(gitIgnorePath)
		if err != nil {
			return err
		}
	}
	return nil
}

func execAsDocker(ctx context.Context, step actionStep, actionName, basedir, subpath string, localAction bool, entrypointType string) error {
	logger := common.Logger(ctx)
	rc := step.getRunContext()

	if rc.IsK8sEnv(ctx) {
		return fmt.Errorf("Docker-based actions are not supported in Kubernetes mode")
	}

	action := step.getActionModel()

	targetPlatform := rc.Config.ContainerArchitecture
	if targetPlatform == "" {
		currentSystemPlatform, err := container.CurrentSystemPlatform(ctx)
		if err != nil {
			return fmt.Errorf("unable to evaluate current system architecture: %w", err)
		}
		targetPlatform = currentSystemPlatform
	}

	// Construct the full location path
	location := subpath
	if basedir != "" {
		location = filepath.Join(basedir, subpath)
	}

	var prepImage common.Executor
	var image string
	forcePull := false
	if after, ok := strings.CutPrefix(action.Runs.Image, "docker://"); ok {
		image = after
		// Apply forcePull only for prebuild docker images
		forcePull = rc.Config.ForcePull
	} else {
		if localAction {
			image = fmt.Sprintf("runner-local-docker-action-%s:latest", common.MustRandName(16))
		} else {
			image = fmt.Sprintf("runner-remote-docker-action-%s:latest", common.Sha256(step.getStepModel().Uses))
		}
		contextDir, fileName := filepath.Split(filepath.Join(location, action.Runs.Image))

		imageExists, err := container.ImageExistsLocally(ctx, image, targetPlatform)
		if err != nil {
			return err
		}

		if !imageExists || rc.Config.ForceRebuild {
			logger.Debugf("image '%s' for architecture '%s' will be built from context '%s", image, targetPlatform, contextDir)
			var buildContext io.ReadCloser
			if localAction {
				buildContext, err = rc.JobContainer.GetContainerArchive(ctx, contextDir+"/.")
				if err != nil {
					return err
				}
				defer buildContext.Close()
			}
			prepImage = container.NewDockerBuildExecutor(container.NewDockerBuildExecutorInput{
				ContextDir:   contextDir,
				Dockerfile:   fileName,
				ImageTag:     image,
				BuildContext: buildContext,
				Platform:     targetPlatform,
			})
		} else {
			logger.Debugf("image '%s' for architecture '%s' already exists", image, targetPlatform)
		}
	}
	eval := rc.NewStepExpressionEvaluator(ctx, step)
	cmd, err := shellquote.Split(eval.Interpolate(ctx, step.getStepModel().With["args"]))
	if err != nil {
		return err
	}
	if len(cmd) == 0 {
		cmd = action.Runs.Args
		evalDockerArgs(ctx, step, action, &cmd)
	}
	entrypoint := strings.Fields(eval.Interpolate(ctx, step.getStepModel().With[entrypointType]))
	if len(entrypoint) == 0 {
		var entrypointValue string

		switch entrypointType {
		case "pre-entrypoint":
			entrypointValue = action.Runs.PreEntrypoint
		case "entrypoint":
			entrypointValue = action.Runs.Entrypoint
		case "post-entrypoint":
			entrypointValue = action.Runs.PostEntrypoint
		}

		if entrypointValue != "" {
			entrypoint, err = shellquote.Split(entrypointValue)
			if err != nil {
				return err
			}
		} else {
			entrypoint = nil
		}
	}
	stepContainer := newStepContainer(ctx, step, image, cmd, entrypoint, targetPlatform)
	return common.NewPipelineExecutor(
		prepImage,
		stepContainer.Pull(forcePull),
		stepContainer.Remove().IfBool(!rc.Config.ReuseContainers),
		stepContainer.Create(rc.Config.ContainerCapAdd, rc.Config.ContainerCapDrop),
		stepContainer.Start(true),
	).Finally(
		stepContainer.Remove().IfBool(!rc.Config.ReuseContainers),
	).Finally(stepContainer.Close())(ctx)
}

func evalDockerArgs(ctx context.Context, step step, action *model.Action, cmd *[]string) {
	rc := step.getRunContext()
	stepModel := step.getStepModel()

	inputs := make(map[string]string)
	eval := rc.NewExpressionEvaluator(ctx)
	// Set Defaults
	for k, input := range action.Inputs {
		inputs[k] = eval.Interpolate(ctx, input.Default)
	}
	if stepModel.With != nil {
		for k, v := range stepModel.With {
			inputs[k] = eval.Interpolate(ctx, v)
		}
	}
	mergeIntoMap(step, step.getEnv(), inputs)

	stepEE := rc.NewStepExpressionEvaluator(ctx, step)
	for i, v := range *cmd {
		(*cmd)[i] = stepEE.Interpolate(ctx, v)
	}
	mergeIntoMap(step, step.getEnv(), action.Runs.Env)

	ee := rc.NewStepExpressionEvaluator(ctx, step)
	for k, v := range *step.getEnv() {
		(*step.getEnv())[k] = ee.Interpolate(ctx, v)
	}
}

func newStepContainer(ctx context.Context, step step, image string, cmd, entrypoint []string, targetPlatform string) container.Container {
	rc := step.getRunContext()
	stepModel := step.getStepModel()
	rawLogger := common.Logger(ctx).WithField("raw_output", true)
	logWriter := common.NewLineWriter(rc.commandHandler(ctx), func(s string) bool {
		if rc.Config.LogOutput {
			rawLogger.Infof("%s", s)
		} else {
			rawLogger.Debugf("%s", s)
		}
		return true
	})
	envList := make([]string, 0, len(*step.getEnv()))
	for k, v := range *step.getEnv() {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}

	envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_TOOL_CACHE", rc.getToolCache(ctx)))
	envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_OS", "Linux"))
	envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_ARCH", container.RunnerArch(ctx)))
	envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_TEMP", "/tmp"))

	binds, mounts, validVolumes := rc.GetBindsAndMounts(ctx)
	networkMode := fmt.Sprintf("container:%s", rc.jobContainerName())
	if rc.IsHostEnv(ctx) {
		networkMode = "default"
	}
	stepContainer := container.NewContainer(&container.NewContainerInput{
		Cmd:             cmd,
		Entrypoint:      entrypoint,
		WorkingDir:      rc.JobContainer.ToContainerPath(rc.Config.Workdir),
		Image:           image,
		Username:        rc.Config.Secrets["DOCKER_USERNAME"],
		Password:        rc.Config.Secrets["DOCKER_PASSWORD"],
		Name:            createSimpleContainerName(rc.jobContainerName(), "STEP-"+stepModel.ID),
		Env:             envList,
		ToolCache:       rc.getToolCache(ctx),
		Mounts:          mounts,
		NetworkMode:     networkMode,
		Binds:           binds,
		Stdout:          logWriter,
		Stderr:          logWriter,
		Privileged:      rc.Config.Privileged,
		UsernsMode:      rc.Config.UsernsMode,
		DefaultPlatform: targetPlatform,
		ValidVolumes:    validVolumes,

		ConfigOptions: rc.Config.ContainerOptions,
	})
	return stepContainer
}

func populateEnvsFromSavedState(env *map[string]string, step actionStep, rc *RunContext) {
	state, ok := rc.IntraActionState[step.getStepModel().ID]
	if ok {
		for name, value := range state {
			envName := fmt.Sprintf("STATE_%s", name)
			(*env)[envName] = value
		}
	}
}

func populateEnvsFromInput(ctx context.Context, env *map[string]string, action *model.Action, rc *RunContext) {
	eval := rc.NewExpressionEvaluator(ctx)
	for inputID, input := range action.Inputs {
		envKey := regexp.MustCompile("[^A-Z0-9-]").ReplaceAllString(strings.ToUpper(inputID), "_")
		envKey = fmt.Sprintf("INPUT_%s", envKey)
		if _, ok := (*env)[envKey]; !ok {
			(*env)[envKey] = eval.Interpolate(ctx, input.Default)
		}
	}
}

func getContainerActionPaths(step *model.Step, actionDir string, rc *RunContext) (string, string) {
	actionName := ""
	containerActionDir := "."
	if step.Type() != model.StepTypeUsesActionRemote {
		actionName = getOsSafeRelativePath(actionDir, rc.Config.Workdir)
		containerActionDir = rc.JobContainer.ToContainerPath(rc.Config.Workdir) + "/" + actionName
		actionName = "./" + actionName
	} else if step.Type() == model.StepTypeUsesActionRemote {
		actionName = getOsSafeRelativePath(actionDir, rc.ActionCacheDir())
		containerActionDir = rc.JobContainer.GetActPath() + "/actions/" + actionName
	}

	if actionName == "" {
		actionName = filepath.Base(actionDir)
		if runtime.GOOS == "windows" {
			actionName = strings.ReplaceAll(actionName, "\\", "/")
		}
	}
	return actionName, containerActionDir
}

func getOsSafeRelativePath(s, prefix string) string {
	actionName := strings.TrimPrefix(s, prefix)
	if runtime.GOOS == "windows" {
		actionName = strings.ReplaceAll(actionName, "\\", "/")
	}
	actionName = strings.TrimPrefix(actionName, "/")

	return actionName
}

func shouldRunPreStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		log := common.Logger(ctx)

		if step.getActionModel() == nil {
			log.Debugf("skip pre step for '%s': no action model available", step.getStepModel())
			return false
		}

		return true
	}
}

func hasPreStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		action := step.getActionModel()
		return (action.Runs.Using == model.ActionRunsUsingComposite) ||
			((action.Runs.Using == model.ActionRunsUsingNode12 ||
				action.Runs.Using == model.ActionRunsUsingNode16 ||
				action.Runs.Using == model.ActionRunsUsingNode20 ||
				action.Runs.Using == model.ActionRunsUsingNode24 ||
				action.Runs.Using == model.ActionRunsUsingGo ||
				action.Runs.Using == model.ActionRunsUsingSh) &&
				action.Runs.Pre != "") ||
			(action.Runs.Using == model.ActionRunsUsingDocker && action.Runs.PreEntrypoint != "")
	}
}

func runPreStep(step actionStep) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		logger.Debugf("run pre step for '%s'", step.getStepModel())

		rc := step.getRunContext()
		stepModel := step.getStepModel()
		action := step.getActionModel()

		// todo: refactor into step
		populateEnvsFromInput(ctx, step.getEnv(), action, rc)

		var actionDir string
		var actionPath string
		var actionLocation string
		var remoteAction *stepActionRemote
		if sar, ok := step.(*stepActionRemote); ok {
			actionPath = newRemoteAction(stepModel.Uses).Path
			actionDir = sar.workTree.WorktreeDir()
			remoteAction = sar
		} else {
			actionDir = filepath.Join(rc.Config.Workdir, stepModel.Uses)
			actionPath = ""
		}
		if actionPath != "" {
			actionLocation = path.Join(actionDir, actionPath)
		} else {
			actionLocation = actionDir
		}
		actionName, containerActionDir := getContainerActionPaths(stepModel, actionLocation, rc)

		switch action.Runs.Using {
		case model.ActionRunsUsingNode12, model.ActionRunsUsingNode16, model.ActionRunsUsingNode20, model.ActionRunsUsingNode24, model.ActionRunsUsingSh:
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}

			var containerArgs []string

			if action.Runs.Using == model.ActionRunsUsingSh {
				rc.ActionPath = containerActionDir
				containerArgs = []string{"sh", "-c", action.Runs.Pre}
			} else {
				containerArgs = []string{"node", path.Join(containerActionDir, action.Runs.Pre)}
			}
			logger.Debugf("executing remote job container: %s", containerArgs)

			rc.ApplyExtraPath(ctx, step.getEnv())

			return rc.execJobContainer(containerArgs, *step.getEnv(), "", "")(ctx)

		case model.ActionRunsUsingComposite:
			if step.getCompositeSteps() == nil {
				step.getCompositeRunContext(ctx)
			}

			if steps := step.getCompositeSteps(); steps != nil && steps.pre != nil {
				return steps.pre(ctx)
			}
			return fmt.Errorf("missing steps in composite action")

		case model.ActionRunsUsingGo:
			// defaults in pre steps were missing, however provided inputs are available
			populateEnvsFromInput(ctx, step.getEnv(), action, rc)
			// todo: refactor into step
			var actionDir string
			var actionPath string
			var actionLocation string
			if sar, ok := step.(*stepActionRemote); ok {
				actionPath = newRemoteAction(stepModel.Uses).Path
				actionDir = sar.workTree.WorktreeDir()
			} else {
				actionDir = filepath.Join(rc.Config.Workdir, stepModel.Uses)
				actionPath = ""
			}

			if actionPath != "" {
				actionLocation = path.Join(actionDir, actionPath)
			} else {
				actionLocation = actionDir
			}

			_, containerActionDir := getContainerActionPaths(stepModel, actionLocation, rc)

			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}

			rc.ApplyExtraPath(ctx, step.getEnv())

			execFileName := fmt.Sprintf("%s.out", action.Runs.Pre)
			buildArgs := []string{"go", "build", "-o", execFileName, action.Runs.Pre}
			execArgs := []string{filepath.Join(containerActionDir, execFileName)}

			return common.NewPipelineExecutor(
				rc.execJobContainer(buildArgs, *step.getEnv(), "", containerActionDir),
				rc.execJobContainer(execArgs, *step.getEnv(), "", ""),
			)(ctx)

		case model.ActionRunsUsingDocker:
			if remoteAction == nil {
				actionDir = ""
				actionPath = containerActionDir
			}
			return execAsDocker(ctx, step, actionName, actionDir, actionPath, remoteAction == nil, "pre-entrypoint")

		default:
			return nil
		}
	}
}

func shouldRunPostStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		log := common.Logger(ctx)
		stepResults := step.getRunContext().getStepsContext()
		stepResult := stepResults[step.getStepModel().ID]

		if stepResult == nil {
			log.WithField("stepResult", model.StepStatusSkipped).Infof("skipping post step for '%s'; step was not executed", step.getStepModel())
			return false
		}

		if stepResult.Conclusion == model.StepStatusSkipped {
			log.WithField("stepResult", model.StepStatusSkipped).Infof("skipping post step for '%s'; main step was skipped", step.getStepModel())
			return false
		}

		if step.getActionModel() == nil {
			log.WithField("stepResult", model.StepStatusSkipped).Infof("skipping post step for '%s': no action model available", step.getStepModel())
			return false
		}

		return true
	}
}

func hasPostStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		action := step.getActionModel()
		return (action.Runs.Using == model.ActionRunsUsingComposite) ||
			((action.Runs.Using == model.ActionRunsUsingNode12 ||
				action.Runs.Using == model.ActionRunsUsingNode16 ||
				action.Runs.Using == model.ActionRunsUsingNode20 ||
				action.Runs.Using == model.ActionRunsUsingNode24 ||
				action.Runs.Using == model.ActionRunsUsingGo ||
				action.Runs.Using == model.ActionRunsUsingSh) &&
				action.Runs.Post != "") ||
			(action.Runs.Using == model.ActionRunsUsingDocker && action.Runs.PostEntrypoint != "")
	}
}

func runPostStep(step actionStep) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		logger.Debugf("run post step for '%s'", step.getStepModel())

		rc := step.getRunContext()
		stepModel := step.getStepModel()
		action := step.getActionModel()

		// todo: refactor into step
		var actionDir string
		var actionPath string
		var remoteAction *stepActionRemote
		if sar, ok := step.(*stepActionRemote); ok {
			actionPath = newRemoteAction(stepModel.Uses).Path
			actionDir = sar.workTree.WorktreeDir()
			remoteAction = sar
		} else {
			actionDir = filepath.Join(rc.Config.Workdir, stepModel.Uses)
			actionPath = ""
		}

		var actionLocation string
		if actionPath != "" {
			actionLocation = path.Join(actionDir, actionPath)
		} else {
			actionLocation = actionDir
		}

		actionName, containerActionDir := getContainerActionPaths(stepModel, actionLocation, rc)

		switch action.Runs.Using {
		case model.ActionRunsUsingNode12, model.ActionRunsUsingNode16, model.ActionRunsUsingNode20, model.ActionRunsUsingNode24:

			populateEnvsFromSavedState(step.getEnv(), step, rc)
			populateEnvsFromInput(ctx, step.getEnv(), step.getActionModel(), rc)

			containerArgs := []string{"node", path.Join(containerActionDir, action.Runs.Post)}
			logger.Debugf("executing remote job container: %s", containerArgs)

			rc.ApplyExtraPath(ctx, step.getEnv())

			return rc.execJobContainer(containerArgs, *step.getEnv(), "", "")(ctx)

		case model.ActionRunsUsingSh:

			populateEnvsFromSavedState(step.getEnv(), step, rc)

			rc.ActionPath = containerActionDir
			containerArgs := []string{"sh", "-c", action.Runs.Post}
			logger.Debugf("executing remote job container: %s", containerArgs)

			rc.ApplyExtraPath(ctx, step.getEnv())

			return rc.execJobContainer(containerArgs, *step.getEnv(), "", "")(ctx)

		case model.ActionRunsUsingComposite:
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}

			if steps := step.getCompositeSteps(); steps != nil && steps.post != nil {
				return steps.post(ctx)
			}
			return fmt.Errorf("missing steps in composite action")

		case model.ActionRunsUsingGo:
			populateEnvsFromSavedState(step.getEnv(), step, rc)
			rc.ApplyExtraPath(ctx, step.getEnv())

			execFileName := fmt.Sprintf("%s.out", action.Runs.Post)
			buildArgs := []string{"go", "build", "-o", execFileName, action.Runs.Post}
			execArgs := []string{filepath.Join(containerActionDir, execFileName)}

			return common.NewPipelineExecutor(
				rc.execJobContainer(buildArgs, *step.getEnv(), "", containerActionDir),
				rc.execJobContainer(execArgs, *step.getEnv(), "", ""),
			)(ctx)

		case model.ActionRunsUsingDocker:
			if remoteAction == nil {
				actionDir = ""
				actionPath = containerActionDir
			}
			return execAsDocker(ctx, step, actionName, actionDir, actionPath, remoteAction == nil, "post-entrypoint")

		default:
			return nil
		}
	}
}
