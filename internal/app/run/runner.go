// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"code.forgejo.org/forgejo/runner/v12/act/artifactcache"
	"code.forgejo.org/forgejo/runner/v12/act/cacheproxy"
	"code.forgejo.org/forgejo/runner/v12/act/common"
	"code.forgejo.org/forgejo/runner/v12/act/model"
	"code.forgejo.org/forgejo/runner/v12/act/runner"
	"connectrpc.com/connect"
	"github.com/docker/docker/api/types/container"
	log "github.com/sirupsen/logrus"

	"code.forgejo.org/forgejo/runner/v12/internal/pkg/client"
	"code.forgejo.org/forgejo/runner/v12/internal/pkg/config"
	"code.forgejo.org/forgejo/runner/v12/internal/pkg/labels"
	"code.forgejo.org/forgejo/runner/v12/internal/pkg/report"
	"code.forgejo.org/forgejo/runner/v12/internal/pkg/ver"
)

// Runner runs the pipeline.
type Runner struct {
	name string

	cfg *config.Config

	client client.Client
	labels labels.Labels
	envs   map[string]string

	cacheProxy *cacheproxy.Handler

	runningTasks sync.Map
}

//mockery:generate: true
//mockery:structname: MockRunner
//mockery:filename: mocks/runner.go
//mockery:pkgname: mocks
type RunnerInterface interface {
	Run(ctx context.Context, task *runnerv1.Task)
}

func NewRunner(cfg *config.Config, name string, ls labels.Labels, cli client.Client, cacheProxy *cacheproxy.Handler) *Runner {
	if cfg.Runner.Envs == nil {
		cfg.Runner.Envs = make(map[string]string, 10)
	}

	cfg.Runner.Envs["GITHUB_SERVER_URL"] = cli.Address()

	envs := make(map[string]string, len(cfg.Runner.Envs))
	maps.Copy(envs, cfg.Runner.Envs)

	if cacheProxy != nil {
		envs["ACTIONS_CACHE_URL"] = cacheProxy.ExternalURL()
	}

	artifactAPI := strings.TrimSuffix(cli.Address(), "/") + "/api/actions_pipeline/"
	envs["ACTIONS_RUNTIME_URL"] = artifactAPI
	envs["ACTIONS_RESULTS_URL"] = strings.TrimSuffix(cli.Address(), "/")

	envs["GITEA_ACTIONS"] = "true"
	envs["GITEA_ACTIONS_RUNNER_VERSION"] = ver.Version()

	envs["FORGEJO_ACTIONS"] = "true"
	envs["FORGEJO_ACTIONS_RUNNER_VERSION"] = ver.Version()

	return &Runner{
		name:       name,
		cfg:        cfg,
		client:     cli,
		labels:     ls,
		envs:       envs,
		cacheProxy: cacheProxy,
	}
}

func SetupCache(cfg *config.Config) *cacheproxy.Handler {
	var cacheURL string
	var cacheSecret string
	var internalCacheServer io.Closer

	if cfg.Cache.ExternalServer == "" {
		// No external cache server was specified, start internal cache server
		cacheSecret = cfg.Cache.Secret

		if cacheSecret == "" {
			// no cache secret was specified, generate one
			secretBytes := make([]byte, 64)
			_, err := rand.Read(secretBytes)
			if err != nil {
				log.Errorf("Failed to generate random bytes, this should not happen")
			}
			cacheSecret = hex.EncodeToString(secretBytes)
		}

		cacheServer, err := artifactcache.StartHandler(
			cfg.Cache.Dir,
			"", // automatically detect
			cfg.Cache.Port,
			cacheSecret,
			log.StandardLogger().WithField("module", "cache_request"),
		)
		if err != nil {
			log.Errorf("Could not start the cache server, cache will be disabled: %v", err)
			return nil
		}

		cacheURL = cacheServer.ExternalURL()
		internalCacheServer = cacheServer
	} else {
		// An external cache server was specified, use its url
		cacheSecret = cfg.Cache.Secret

		if cacheSecret == "" {
			log.Error("A cache secret must be specified to use an external cache server, cache will be disabled")
			return nil
		}

		cacheURL = strings.TrimSuffix(cfg.Cache.ExternalServer, "/")
	}

	cacheProxy, err := cacheproxy.StartHandler(
		cacheURL,
		cfg.Cache.Host,
		cfg.Cache.ProxyPort,
		cfg.Cache.ActionsCacheURLOverride,
		cacheSecret,
		log.StandardLogger().WithField("module", "cache_proxy"),
		internalCacheServer,
	)
	if err != nil {
		log.Errorf("cannot init cache proxy, cache will be disabled: %v", err)
		return nil
	}

	return cacheProxy
}

func (r *Runner) Run(ctx context.Context, task *runnerv1.Task) {
	if _, loaded := r.runningTasks.LoadOrStore(task.Id, struct{}{}); loaded {
		// Normally the responsibility of Run() is to ensure that, no matter what occurs within running the job, a
		// reporter is created that will report the job status to the server. But in this case, for some reason another
		// goroutine is currently running this exact same task on this same runner -- we can't report status in this
		// case because it would corrupt the status of the other running job.
		log.Errorf("task %d is already running", task.Id)
		return
	}
	defer r.runningTasks.Delete(task.Id)

	ctx, cancel := context.WithTimeout(ctx, r.cfg.Runner.Timeout)
	defer cancel()
	reporter := report.NewReporter(ctx, cancel, r.client, task, r.cfg.Runner.ReportInterval, &r.cfg.Runner.ReportRetry)
	var runErr error
	defer func() {
		closeErr := reporter.Close(runErr)
		if closeErr != nil {
			log.Errorf("unable to send final job logs and status: %v", closeErr)
		}
	}()
	reporter.RunDaemon()
	runErr = r.run(ctx, task, reporter)
}

func logAndReport(reporter *report.Reporter, message string, args ...any) {
	log.Debugf(message, args...)
	reporter.Logf(message, args...)
}

func explainFailedGenerateWorkflow(task *runnerv1.Task, log func(message string, args ...any), err error) error {
	for n, line := range strings.Split(string(task.WorkflowPayload), "\n") {
		log("%5d: %s", n+1, line)
	}
	log("Errors were found and although they tend to be cryptic the line number they refer to gives a hint as to where the problem might be.")
	for line := range strings.SplitSeq(err.Error(), "\n") {
		log("%s", line)
	}
	return fmt.Errorf("the workflow file is not usable")
}

func getWriteIsolationKey(ctx context.Context, eventName, ref string, event map[string]any) (string, error) {
	if eventName == "pull_request" {
		// The "closed" action of a pull request event runs in the context of the base repository
		// and was merged by a user with write access to the base repository. It is authorized to
		// write the repository cache.
		if event["action"] == "closed" {
			pullRequest, ok := event["pull_request"].(map[string]any)
			if !ok {
				return "", fmt.Errorf("getWriteIsolationKey: event.pull_request is not a map[string]any but %T", event["pull_request"])
			}
			merged, ok := pullRequest["merged"].(bool)
			if !ok {
				return "", fmt.Errorf("getWriteIsolationKey: event.pull_request.merged is not a bool but %T", pullRequest["merged"])
			}
			if merged {
				return "", nil
			}
			// a pull request that is closed but not merged falls thru and is expected to obey the same
			// constraints as an opened pull request, it may be closed by a user with no write permissions to the
			// base repository
		}
		// When performing an action on an event from an opened PR, provide a "write isolation key" to the cache. The generated
		// ACTIONS_CACHE_URL will be able to read the cache, and write to a cache, but its writes will be isolated to
		// future runs of the PR's workflows and won't be shared with other pull requests or actions. This is a security
		// measure to prevent a malicious pull request from poisoning the cache with secret-stealing code which would
		// later be executed on another action.
		// Ensure that `ref` has the expected format so that we don't end up with a useless write isolation key
		if !strings.HasPrefix(ref, "refs/pull/") {
			return "", fmt.Errorf("getWriteIsolationKey: expected ref to be refs/pull/..., but was %q", ref)
		}
		return ref, nil
	}

	// Other events do not allow the trigger user to modify the content of the repository and
	// are allowed to write the cache without an isolation key
	return "", nil
}

func (r *Runner) run(ctx context.Context, task *runnerv1.Task, reporter *report.Reporter) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	reporter.Logf("%s(version:%s) received task %v of job %v, be triggered by event: %s", r.name, ver.Version(), task.Id, task.Context.Fields["job"].GetStringValue(), task.Context.Fields["event_name"].GetStringValue())

	workflow, jobID, err := generateWorkflow(task)
	if err != nil {
		return explainFailedGenerateWorkflow(task, func(message string, args ...any) {
			logAndReport(reporter, message, args...)
		}, err)
	}

	plan, err := model.CombineWorkflowPlanner(workflow).PlanJob(jobID)
	if err != nil {
		return err
	}
	job := workflow.GetJob(jobID)
	reporter.ResetSteps(len(job.Steps))

	taskContext := task.Context.Fields

	defaultActionURL := client.BackwardCompatibleContext(task, "default_actions_url")
	if defaultActionURL == "" {
		return fmt.Errorf("task %v context does not contain a {forgejo,gitea}_default_actions_url key", task.Id)
	}

	serverVersion := client.BackwardCompatibleContext(task, "server_version")
	log.Debugf("Forgejo server version '%s'", serverVersion)

	log.Infof("task %v repo is %v %v %v", task.Id, taskContext["repository"].GetStringValue(),
		defaultActionURL,
		r.client.Address())

	eventName := taskContext["event_name"].GetStringValue()
	ref := taskContext["ref"].GetStringValue()
	event := taskContext["event"].GetStructValue().AsMap()
	preset := &model.GithubContext{
		Event:           event,
		RunID:           taskContext["run_id"].GetStringValue(),
		RunNumber:       taskContext["run_number"].GetStringValue(),
		RunAttempt:      taskContext["run_attempt"].GetStringValue(),
		Actor:           taskContext["actor"].GetStringValue(),
		Repository:      taskContext["repository"].GetStringValue(),
		EventName:       eventName,
		Sha:             taskContext["sha"].GetStringValue(),
		Ref:             ref,
		RefName:         taskContext["ref_name"].GetStringValue(),
		RefType:         taskContext["ref_type"].GetStringValue(),
		HeadRef:         taskContext["head_ref"].GetStringValue(),
		BaseRef:         taskContext["base_ref"].GetStringValue(),
		Token:           taskContext["token"].GetStringValue(),
		RepositoryOwner: taskContext["repository_owner"].GetStringValue(),
		RetentionDays:   taskContext["retention_days"].GetStringValue(),
		WorkflowRef:     taskContext["workflow_ref"].GetStringValue(),
	}
	if t := task.Secrets["FORGEJO_TOKEN"]; t != "" {
		preset.Token = t
	} else if t := task.Secrets["GITEA_TOKEN"]; t != "" {
		preset.Token = t
	} else if t := task.Secrets["GITHUB_TOKEN"]; t != "" {
		preset.Token = t
	}

	// Clone the runner default envs into a local envs map
	runEnvs := make(map[string]string)
	maps.Copy(runEnvs, r.envs)

	runtimeToken := client.BackwardCompatibleContext(task, "runtime_token")
	if runtimeToken == "" {
		// use task token to action api token for previous Gitea Server Versions
		runtimeToken = preset.Token
	}
	runEnvs["ACTIONS_RUNTIME_TOKEN"] = runtimeToken

	runEnvs["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = taskContext["forgejo_actions_id_token_request_token"].GetStringValue()
	runEnvs["ACTIONS_ID_TOKEN_REQUEST_URL"] = taskContext["forgejo_actions_id_token_request_url"].GetStringValue()

	// Register the run with the cacheproxy and modify the CACHE_URL
	if r.cacheProxy != nil {
		writeIsolationKey, err := getWriteIsolationKey(ctx, eventName, ref, event)
		if err != nil {
			return err
		}

		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		cacheRunData := r.cacheProxy.CreateRunData(preset.Repository, preset.RunID, timestamp, writeIsolationKey)
		cacheRunID, err := r.cacheProxy.AddRun(cacheRunData)
		if err == nil {
			defer func() { _ = r.cacheProxy.RemoveRun(cacheRunID) }()
			baseURL := runEnvs["ACTIONS_CACHE_URL"]
			runEnvs["ACTIONS_CACHE_URL"] = fmt.Sprintf("%s/%s/", baseURL, cacheRunID)
		}
	}

	eventJSON, err := json.Marshal(preset.Event)
	if err != nil {
		return err
	}

	maxLifetime := 3 * time.Hour
	if deadline, ok := ctx.Deadline(); ok {
		maxLifetime = time.Until(deadline)
	}

	var inputs map[string]string
	if preset.EventName == "workflow_dispatch" {
		if inputsRaw, ok := preset.Event["inputs"]; ok {
			inputs, _ = inputsRaw.(map[string]string)
		}
	}

	workdirParent := r.cfg.Container.WorkdirParent
	if len(r.buildK8sPodSpecs()) > 0 && workdirParent == "workspace" {
		workdirParent = "shared/workdir"
	}

	runnerConfig := &runner.Config{
		// On Linux, Workdir will be like "/<parent_directory>/<owner>/<repo>"
		// On Windows, Workdir will be like "\<parent_directory>\<owner>\<repo>"
		Workdir:        filepath.FromSlash(filepath.Clean(fmt.Sprintf("/%s/%s", workdirParent, preset.Repository))),
		BindWorkdir:    false,
		ActionCacheDir: filepath.FromSlash(r.cfg.Host.WorkdirParent),

		ReuseContainers:            false,
		ForcePull:                  r.cfg.Container.ForcePull,
		ForceRebuild:               r.cfg.Container.ForceRebuild,
		LogOutput:                  true,
		JSONLogger:                 false,
		Env:                        runEnvs,
		Secrets:                    task.Secrets,
		GitHubInstance:             strings.TrimSuffix(r.client.Address(), "/"),
		NoSkipCheckout:             true,
		PresetGitHubContext:        preset,
		EventJSON:                  string(eventJSON),
		ContainerNamePrefix:        fmt.Sprintf("FORGEJO-ACTIONS-TASK-%d", task.Id),
		ContainerMaxLifetime:       maxLifetime,
		ContainerNetworkMode:       container.NetworkMode(r.cfg.Container.Network),
		ContainerNetworkEnableIPv6: r.cfg.Container.EnableIPv6,
		ContainerOptions:           r.cfg.Container.Options,
		ContainerDaemonSocket:      r.cfg.Container.DockerHost,
		Privileged:                 r.cfg.Container.Privileged,
		DefaultActionInstance:      defaultActionURL,
		PlatformPicker:             r.labels.PickPlatform,
		Vars:                       task.Vars,
		ValidVolumes:               r.cfg.Container.ValidVolumes,
		InsecureSkipTLS:            r.cfg.Runner.Insecure,
		Inputs:                     inputs,
		ServerVersion:              serverVersion,

		KubernetesNamespace:   r.cfg.Kubernetes.Namespace,
		KubeConfig:            r.cfg.Kubernetes.KubeConfig,
		KubernetesPollTimeout: r.cfg.Kubernetes.PollTimeout,
		KubernetesPodSpecs:    r.buildK8sPodSpecs(),
		Plugins:               r.buildPluginConfigs(),
		PluginsV2:             r.buildPluginV2Configs(),
	}

	if r.cfg.Log.JobLevel != "" {
		level, err := log.ParseLevel(r.cfg.Log.JobLevel)
		if err != nil {
			return err
		}

		runnerConfig.JobLoggerLevel = &level
	}

	rr, err := runner.New(runnerConfig)
	if err != nil {
		return err
	}
	executor := rr.NewPlanExecutor(plan)

	reporter.Logf("workflow prepared")

	// add logger recorders
	ctx = common.WithLoggerHook(ctx, reporter)

	if !log.IsLevelEnabled(log.DebugLevel) {
		ctx = runner.WithJobLoggerFactory(ctx, NullLogger{})
	}

	execErr := executor(ctx)
	return execErr
}

func (r *Runner) Declare(ctx context.Context, labels []string) (*connect.Response[runnerv1.DeclareResponse], error) {
	return r.client.Declare(ctx, connect.NewRequest(&runnerv1.DeclareRequest{
		Version: ver.Version(),
		Labels:  labels,
	}))
}

func (r *Runner) Update(ctx context.Context, labels labels.Labels) {
	r.labels = labels
}

// buildK8sPodSpecs maps label names to their podspec file paths for K8s labels.
func (r *Runner) buildK8sPodSpecs() map[string]string {
	specs := make(map[string]string)
	for _, label := range r.labels {
		if label.Schema == labels.SchemeK8sPod && label.Arg != "" {
			specs[label.Name] = strings.TrimPrefix(label.Arg, "//")
		}
	}
	return specs
}

// buildPluginConfigs converts internal config.Plugin map to runner.PluginConfig map.
func (r *Runner) buildPluginConfigs() map[string]runner.PluginConfig {
	if len(r.cfg.Plugins) == 0 {
		return nil
	}
	plugins := make(map[string]runner.PluginConfig, len(r.cfg.Plugins))
	for name, p := range r.cfg.Plugins {
		plugins[name] = runner.PluginConfig{
			Address: p.Address,
			Options: p.Options,
		}
	}
	return plugins
}

func (r *Runner) buildPluginV2Configs() map[string]runner.PluginV2Config {
	if len(r.cfg.PluginsV2) == 0 {
		return nil
	}
	plugins := make(map[string]runner.PluginV2Config, len(r.cfg.PluginsV2))
	for name, p := range r.cfg.PluginsV2 {
		plugins[name] = runner.PluginV2Config{
			Path:    p.Path,
			Options: p.Options,
		}
	}
	return plugins
}
