package runner

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"text/template"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"code.forgejo.org/forgejo/runner/v12/act/container"
	"code.forgejo.org/forgejo/runner/v12/act/container/docker"
	"code.forgejo.org/forgejo/runner/v12/act/exprparser"
	"code.forgejo.org/forgejo/runner/v12/act/model"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	"github.com/opencontainers/selinux/go-selinux"
)

// RunContext contains info about current job
type RunContext struct {
	Name                string
	Config              *Config
	Matrix              map[string]any
	Run                 *model.Run
	EventJSON           string
	Env                 map[string]string
	GlobalEnv           map[string]string // to pass env changes of GITHUB_ENV and set-env correctly, due to dirty Env field
	ExtraPath           []string
	CurrentStep         string
	StepResults         map[string]*model.StepResult
	IntraActionState    map[string]map[string]string
	ExprEval            ExpressionEvaluator
	JobContainer        container.ExecutionsEnvironment
	ServiceContainers   []container.ExecutionsEnvironment
	OutputMappings      map[MappableOutput]MappableOutput
	JobName             string
	ActionPath          string
	Parent              *RunContext
	Masks               []string
	cleanUpJobContainer common.Executor
	caller              *caller // job calling this RunContext (reusable workflows)
	randomName          string
	networkName         string
	networkCreated      bool
	dockerEndpointMu    sync.Mutex
	dockerEndpoint      docker.Endpoint
}

func (rc *RunContext) DockerEndpoint(ctx context.Context) (docker.Endpoint, error) {
	rc.dockerEndpointMu.Lock()
	defer rc.dockerEndpointMu.Unlock()
	if rc.dockerEndpoint != nil {
		return rc.dockerEndpoint, nil
	}
	ep, err := docker.NewEndpoint(ctx, os.Getenv("DOCKER_HOST"))
	if err != nil {
		return nil, err
	}
	rc.dockerEndpoint = ep
	return ep, nil
}

func (rc *RunContext) AddMask(mask string) {
	rc.Masks = append(rc.Masks, mask)
}

type MappableOutput struct {
	StepID     string
	OutputName string
}

func (rc *RunContext) String() string {
	name := fmt.Sprintf("%s/%s", rc.Run.Workflow.Name, rc.Name)
	if rc.caller != nil {
		// prefix the reusable workflow with the caller job
		// this is required to create unique container names
		name = fmt.Sprintf("%s/%s", rc.caller.runContext.Name, name)
	}
	return name
}

// GetEnv returns the env for the context
func (rc *RunContext) GetEnv() map[string]string {
	if rc.Env == nil {
		rc.Env = map[string]string{}
		if rc.Run != nil && rc.Run.Workflow != nil && rc.Config != nil {
			job := rc.Run.Job()
			if job != nil {
				rc.Env = mergeMaps(rc.Run.Workflow.Env, job.Environment(), rc.Config.Env)
			}
		}
	}
	rc.Env["ACT"] = "true"

	if !rc.Config.NoSkipCheckout {
		rc.Env["ACT_SKIP_CHECKOUT"] = "true"
	}

	return rc.Env
}

func (rc *RunContext) jobContainerName() string {
	return createSimpleContainerName(rc.Config.ContainerNamePrefix, "WORKFLOW-"+common.Sha256(rc.String()), "JOB-"+rc.Name)
}

func getDockerDaemonSocketMountPath(daemonPath string) string {
	if protoIndex := strings.Index(daemonPath, "://"); protoIndex != -1 {
		scheme := daemonPath[:protoIndex]
		if strings.EqualFold(scheme, "npipe") {
			// linux container mount on windows, use the default socket path of the VM / wsl2
			return "/var/run/docker.sock"
		} else if strings.EqualFold(scheme, "unix") {
			return daemonPath[protoIndex+3:]
		} else if strings.IndexFunc(scheme, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
		}) == -1 {
			// unknown protocol use default
			return "/var/run/docker.sock"
		}
	}
	return daemonPath
}

func (rc *RunContext) getInternalVolumeNames(ctx context.Context) []string {
	return []string{
		rc.getInternalVolumeWorkdir(ctx),
		rc.getInternalVolumeEnv(ctx),
	}
}

func (rc *RunContext) getInternalVolumeWorkdir(ctx context.Context) string {
	rc.ensureRandomName(ctx)
	return rc.randomName
}

func (rc *RunContext) getInternalVolumeEnv(ctx context.Context) string {
	rc.ensureRandomName(ctx)
	return fmt.Sprintf("%s-env", rc.randomName)
}

// Returns the binds and mounts for the container, resolving paths as appopriate
func (rc *RunContext) GetBindsAndMounts(ctx context.Context) ([]string, map[string]string, []string) {
	binds := []string{}

	containerDaemonSocket := rc.Config.GetContainerDaemonSocket()
	if containerDaemonSocket != "-" {
		daemonPath := getDockerDaemonSocketMountPath(containerDaemonSocket)
		binds = append(binds, fmt.Sprintf("%s:%s", daemonPath, "/var/run/docker.sock"))
	}

	ext := docker.LinuxContainerEnvironmentExtensions{}

	mounts := map[string]string{
		rc.getInternalVolumeEnv(ctx): ext.GetActPath(),
	}

	if job := rc.Run.Job(); job != nil {
		if container := job.Container(); container != nil {
			interpolatedVolumes := make([]string, 0, len(container.Volumes))
			for _, volume := range container.Volumes {
				interpolatedVolumes = append(interpolatedVolumes, rc.ExprEval.Interpolate(ctx, volume))
			}

			for _, v := range interpolatedVolumes {
				if !strings.Contains(v, ":") || filepath.IsAbs(v) {
					// Bind anonymous volume or host file.
					binds = append(binds, v)
				} else {
					// Mount existing volume.
					paths := strings.SplitN(v, ":", 2)
					mounts[paths[0]] = paths[1]
				}
			}
		}
	}

	if rc.Config.BindWorkdir {
		bindModifiers := ""
		if runtime.GOOS == "darwin" {
			bindModifiers = ":delegated"
		}
		if selinux.GetEnabled() {
			bindModifiers = ":z"
		}
		binds = append(binds, fmt.Sprintf("%s:%s%s", rc.Config.Workdir, ext.ToContainerPath(rc.Config.Workdir), bindModifiers))
	} else {
		mounts[rc.getInternalVolumeWorkdir(ctx)] = ext.ToContainerPath(rc.Config.Workdir)
	}

	validVolumes := append(rc.getInternalVolumeNames(ctx), getDockerDaemonSocketMountPath(containerDaemonSocket))
	validVolumes = append(validVolumes, rc.Config.ValidVolumes...)
	return binds, mounts, validVolumes
}

//go:embed lxc-helpers-lib.sh
var lxcHelpersLib string

//go:embed lxc-helpers.sh
var lxcHelpers string

var startTemplate = template.Must(template.New("start").Parse(`#!/bin/bash -e

exec 5<>/tmp/forgejo-runner-lxc.lock ; flock --timeout 21600 5

LXC_CONTAINER_CONFIG="{{.Config}}"
LXC_CONTAINER_RELEASE="{{.Release}}"

source $(dirname $0)/lxc-helpers-lib.sh

function template_act() {
    echo $(lxc_template_release)-act
}

function install_nodejs() {
    local name="$1"

    local script=/usr/local/bin/lxc-helpers-install-node.sh

    cat > $(lxc_root $name)/$script <<'EOF'
#!/bin/sh -e
# https://github.com/nodesource/distributions#debinstall
export DEBIAN_FRONTEND=noninteractive
apt-get install -qq -y ca-certificates curl gnupg git
mkdir -p /etc/apt/keyrings
curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg
NODE_MAJOR=24
echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_$NODE_MAJOR.x nodistro main" | tee /etc/apt/sources.list.d/nodesource.list
apt-get update -qq
apt-get install -qq -y nodejs
EOF
    lxc_container_run_script $name $script
}

function build_template_act() {
    local name="$(template_act)"

    if lxc_exists_and_apt_not_old $name ; then
      return 0
    fi

    lxc_build_template $(lxc_template_release) $name
    lxc_container_start $name
    install_nodejs $name
    lxc_container_stop $name
}

lxc_prepare_environment
LXC_CONTAINER_CONFIG="" build_template_act
lxc_build_template $(template_act) "{{.Name}}"
lxc_container_mount "{{.Name}}" "{{ .Root }}"
lxc_container_start "{{.Name}}"
`))

var stopTemplate = template.Must(template.New("stop").Parse(`#!/bin/bash
source $(dirname $0)/lxc-helpers-lib.sh

lxc_container_destroy "{{.Name}}"
lxc_maybe_sudo
$LXC_SUDO rm -fr "{{ .Root }}"
`))

func (rc *RunContext) stopHostEnvironment(ctx context.Context) error {
	logger := common.Logger(ctx)
	logger.Debugf("stopHostEnvironment")

	if !rc.IsLXCHostEnv(ctx) {
		return nil
	}

	var stopScript bytes.Buffer
	if err := stopTemplate.Execute(&stopScript, struct {
		Name string
		Root string
	}{
		Name: rc.JobContainer.GetName(),
		Root: rc.JobContainer.GetRoot(),
	}); err != nil {
		return err
	}

	return common.NewPipelineExecutor(
		rc.JobContainer.Copy(rc.JobContainer.GetActPath()+"/", &container.FileEntry{
			Name: "workflow/stop-lxc.sh",
			Mode: 0o755,
			Body: stopScript.String(),
		}),
		rc.JobContainer.Exec([]string{rc.JobContainer.GetActPath() + "/workflow/stop-lxc.sh"}, map[string]string{}, "root", "/tmp"),
	)(ctx)
}

func (rc *RunContext) startHostEnvironment() common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		rawLogger := logger.WithField("raw_output", true)
		logWriter := common.NewLineWriter(rc.commandHandler(ctx), func(s string) bool {
			if rc.Config.LogOutput {
				rawLogger.Infof("%s", s)
			} else {
				rawLogger.Debugf("%s", s)
			}
			return true
		})
		cacheDir := rc.ActionCacheDir()
		randName := common.MustRandName(8)
		miscpath := filepath.Join(cacheDir, randName)
		actPath := filepath.Join(miscpath, "act")
		if err := os.MkdirAll(actPath, 0o777); err != nil {
			return err
		}
		path := filepath.Join(miscpath, "hostexecutor")
		if err := os.MkdirAll(path, 0o777); err != nil {
			return err
		}
		runnerTmp := filepath.Join(miscpath, "tmp")
		if err := os.MkdirAll(runnerTmp, 0o777); err != nil {
			return err
		}
		rc.JobContainer = &container.HostEnvironment{
			Name:      randName,
			Root:      miscpath,
			Path:      path,
			TmpDir:    runnerTmp,
			ToolCache: rc.getToolCache(ctx),
			Workdir:   rc.Config.Workdir,
			ActPath:   actPath,
			StdOut:    logWriter,
			LXC:       rc.IsLXCHostEnv(ctx),
		}
		rc.cleanUpJobContainer = func(ctx context.Context) error {
			if err := rc.stopHostEnvironment(ctx); err != nil {
				return err
			}
			if rc.JobContainer == nil {
				return nil
			}
			return rc.JobContainer.Remove()(ctx)
		}
		for k, v := range rc.JobContainer.GetRunnerContext(ctx) {
			if v, ok := v.(string); ok {
				rc.Env[fmt.Sprintf("RUNNER_%s", strings.ToUpper(k))] = v
			}
		}
		for _, env := range os.Environ() {
			if k, v, ok := strings.Cut(env, "="); ok {
				// don't override
				if _, ok := rc.Env[k]; !ok {
					rc.Env[k] = v
				}
			}
		}

		executors := make([]common.Executor, 0, 10)

		isLXCHost, LXCTemplate, LXCRelease, LXCConfig := rc.GetLXCInfo(ctx)

		if isLXCHost {
			var startScript bytes.Buffer
			if err := startTemplate.Execute(&startScript, struct {
				Name     string
				Template string
				Release  string
				Config   string
				Repo     string
				Root     string
				TmpDir   string
				Script   string
			}{
				Name:     rc.JobContainer.GetName(),
				Template: LXCTemplate,
				Release:  LXCRelease,
				Config:   LXCConfig,
				Repo:     "", // step.Environment["CI_REPO"],
				Root:     rc.JobContainer.GetRoot(),
				TmpDir:   runnerTmp,
				Script:   "", // "commands-" + step.Name,
			}); err != nil {
				return err
			}

			executors = append(
				executors,
				rc.JobContainer.Copy(rc.JobContainer.GetActPath()+"/", &container.FileEntry{
					Name: "workflow/lxc-helpers-lib.sh",
					Mode: 0o755,
					Body: lxcHelpersLib,
				}),
				rc.JobContainer.Copy(rc.JobContainer.GetActPath()+"/", &container.FileEntry{
					Name: "workflow/lxc-helpers.sh",
					Mode: 0o755,
					Body: lxcHelpers,
				}),
				rc.JobContainer.Copy(rc.JobContainer.GetActPath()+"/", &container.FileEntry{
					Name: "workflow/start-lxc.sh",
					Mode: 0o755,
					Body: startScript.String(),
				}),
				rc.JobContainer.Exec([]string{rc.JobContainer.GetActPath() + "/workflow/start-lxc.sh"}, map[string]string{}, "root", "/tmp"),
			)
		}

		executors = append(executors, rc.JobContainer.Copy(rc.JobContainer.GetActPath()+"/", &container.FileEntry{
			Name: "workflow/event.json",
			Mode: 0o644,
			Body: rc.EventJSON,
		}, &container.FileEntry{
			Name: "workflow/envs.txt",
			Mode: 0o666,
			Body: "",
		}))

		return common.NewPipelineExecutor(executors...)(ctx)
	}
}

func (rc *RunContext) ensureRandomName(ctx context.Context) {
	if rc.randomName == "" {
		logger := common.Logger(ctx)
		if rc.Parent != nil {
			// composite actions inherit their run context from the parent job
			rootRunContext := rc
			for rootRunContext.Parent != nil {
				rootRunContext = rootRunContext.Parent
			}
			rootRunContext.ensureRandomName(ctx)
			rc.randomName = rootRunContext.randomName
			logger.Debugf("RunContext inherited random name %s from its parent", rc.Name, rc.randomName)
		} else {
			rc.randomName = common.MustRandName(16)
			logger.Debugf("RunContext %s is assigned random name %s", rc.Name, rc.randomName)
		}
	}
}

func (rc *RunContext) getNetworkCreated(ctx context.Context) bool {
	rc.ensureNetworkName(ctx)
	return rc.networkCreated
}

func (rc *RunContext) getNetworkName(ctx context.Context) string {
	rc.ensureNetworkName(ctx)
	return rc.networkName
}

func (rc *RunContext) ensureNetworkName(ctx context.Context) {
	if rc.networkName == "" {
		rc.ensureRandomName(ctx)
		rc.networkName = string(rc.Config.ContainerNetworkMode)
		if rc.networkName == "" {
			rc.networkName = fmt.Sprintf("WORKFLOW-%s", rc.randomName)
			rc.networkCreated = true
		}
	}
}

var sanitizeNetworkAliasRegex = regexp.MustCompile("[^a-z0-9-]")

func sanitizeNetworkAlias(ctx context.Context, original string) string {
	sanitized := sanitizeNetworkAliasRegex.ReplaceAllString(strings.ToLower(original), "_")
	if sanitized != original {
		logger := common.Logger(ctx)
		logger.Infof("The network alias is %s (sanitized version of %s)", sanitized, original)
	}
	return sanitized
}

func (rc *RunContext) prepareJobContainer(ctx context.Context) error {
	logger := common.Logger(ctx)
	image := rc.platformImage(ctx)
	rawLogger := logger.WithField("raw_output", true)
	logWriter := common.NewLineWriter(rc.commandHandler(ctx), func(s string) bool {
		if rc.Config.LogOutput {
			rawLogger.Infof("%s", s)
		} else {
			rawLogger.Debugf("%s", s)
		}
		return true
	})

	ep, err := rc.DockerEndpoint(ctx)
	if err != nil {
		return err
	}
	xe := docker.NewExecutionEnvironment(ep)

	username, password, err := rc.handleCredentials(ctx)
	if err != nil {
		return fmt.Errorf("failed to handle credentials: %s", err)
	}

	logger.Infof("\U0001f680  Start image=%s", image)
	name := rc.jobContainerName()
	// For gitea, to support --volumes-from <container_name_or_id> in options.
	// We need to set the container name to the environment variable.
	rc.Env["JOB_CONTAINER_NAME"] = name

	envList := make([]string, 0, 5)

	envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_TOOL_CACHE", rc.getToolCache(ctx)))
	envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_OS", "Linux"))
	envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_ARCH", ep.RunnerArch()))
	envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_TEMP", "/tmp"))
	envList = append(envList, fmt.Sprintf("%s=%s", "LANG", "C.UTF-8")) // Use same locale as GitHub Actions

	ext := docker.LinuxContainerEnvironmentExtensions{}
	binds, mounts, validVolumes := rc.GetBindsAndMounts(ctx)

	// add service containers
	for serviceID, spec := range rc.Run.Job().Services {
		// Interpolate image first to check if we should skip this service
		interpolatedImage := rc.ExprEval.Interpolate(ctx, spec.Image)

		// Skip service if image is empty (allows conditional services via expressions)
		if interpolatedImage == "" {
			logger.Infof("Skipping service '%s' because image is empty", serviceID)
			continue
		}

		// interpolate env
		interpolatedEnvs := make(map[string]string, len(spec.Env))
		for k, v := range spec.Env {
			interpolatedEnvs[k] = rc.ExprEval.Interpolate(ctx, v)
		}
		envs := make([]string, 0, len(interpolatedEnvs))
		for k, v := range interpolatedEnvs {
			envs = append(envs, fmt.Sprintf("%s=%s", k, v))
		}
		interpolatedCmd := make([]string, 0, len(spec.Cmd))
		for _, v := range spec.Cmd {
			interpolatedCmd = append(interpolatedCmd, rc.ExprEval.Interpolate(ctx, v))
		}

		username, password, err := rc.handleServiceCredentials(ctx, spec.Credentials)
		if err != nil {
			return fmt.Errorf("failed to handle service %s credentials: %w", serviceID, err)
		}

		interpolatedVolumes := make([]string, 0, len(spec.Volumes))
		for _, volume := range spec.Volumes {
			interpolatedVolumes = append(interpolatedVolumes, rc.ExprEval.Interpolate(ctx, volume))
		}
		serviceBinds, serviceMounts := rc.GetServiceBindsAndMounts(interpolatedVolumes)

		interpolatedPorts := make([]string, 0, len(spec.Ports))
		for _, port := range spec.Ports {
			interpolatedPorts = append(interpolatedPorts, rc.ExprEval.Interpolate(ctx, port))
		}
		natExposed, natBindings, err := nat.ParsePortSpecs(interpolatedPorts)
		if err != nil {
			return fmt.Errorf("failed to parse service %s ports: %w", serviceID, err)
		}
		exposedPorts := make(map[container.PortSpec]struct{}, len(natExposed))
		for port := range natExposed {
			exposedPorts[container.PortSpec(port)] = struct{}{}
		}
		portBindings := make(map[container.PortSpec][]container.PortBinding, len(natBindings))
		for port, bindings := range natBindings {
			pb := make([]container.PortBinding, len(bindings))
			for i, b := range bindings {
				pb[i] = container.PortBinding{HostIP: b.HostIP, HostPort: b.HostPort}
			}
			portBindings[container.PortSpec(port)] = pb
		}

		serviceContainerName := createContainerName(rc.jobContainerName(), serviceID)
		c := xe.NewServiceContainer(&container.NewContainerInput{
			Name:            serviceContainerName,
			Image:           interpolatedImage,
			Username:        username,
			Password:        password,
			Cmd:             interpolatedCmd,
			Entrypoint:      spec.GetEntrypoint(nil),
			Init:            spec.EnableInit(false),
			TTY:             spec.WithTTY(false),
			Env:             envs,
			ToolCache:       rc.getToolCache(ctx),
			Mounts:          serviceMounts,
			Binds:           serviceBinds,
			Stdout:          logWriter,
			Stderr:          logWriter,
			Privileged:      rc.Config.Privileged,
			UsernsMode:      rc.Config.UsernsMode,
			DefaultPlatform: rc.dockerImagePlatform(ctx),
			NetworkMode:     rc.getNetworkName(ctx),
			NetworkAliases:  []string{sanitizeNetworkAlias(ctx, serviceID)},
			ExposedPorts:    exposedPorts,
			PortBindings:    portBindings,
			ValidVolumes:    rc.Config.ValidVolumes,

			JobOptions:    rc.ExprEval.Interpolate(ctx, spec.Options),
			ConfigOptions: rc.Config.ContainerOptions,
		})
		rc.ServiceContainers = append(rc.ServiceContainers, c)
	}

	rc.cleanUpJobContainer = func(ctx context.Context) error {
		// reinit logger from ctx since cleanUpJobContainer is be called after the job is complete, and using
		// prepareJobContainer's logger could cause logs to continue to append to the finished job
		logger := common.Logger(ctx)

		reuseJobContainer := func(ctx context.Context) bool {
			return rc.Config.ReuseContainers
		}

		if rc.JobContainer != nil {
			return rc.JobContainer.Remove().IfNot(reuseJobContainer).
				Then(docker.NewDockerVolumesRemoveExecutor(ep, rc.getInternalVolumeNames(ctx))).IfNot(reuseJobContainer).
				Then(func(ctx context.Context) error {
					if len(rc.ServiceContainers) > 0 {
						logger.Infof("Cleaning up services for job %s", rc.JobName)
						if err := rc.stopServiceContainers()(ctx); err != nil {
							logger.Errorf("Error while cleaning services: %v", err)
						}
					}
					if rc.getNetworkCreated(ctx) {
						logger.Infof("Cleaning up network for job %s, and network name is: %s", rc.JobName, rc.getNetworkName(ctx))
						if err := docker.NewDockerNetworkRemoveExecutor(ep, rc.getNetworkName(ctx))(ctx); err != nil {
							logger.Errorf("Error while cleaning network: %v", err)
						}
					}
					return nil
				})(ctx)
		}
		return nil
	}

	entrypoint := GetContainerProperty(rc, []string{"tail", "-f", "/dev/null"}, func(spec *model.ContainerSpec) []string {
		return spec.GetEntrypoint([]string{"tail", "-f", "/dev/null"})
	})
	enableInit := GetContainerProperty(rc, false, func(spec *model.ContainerSpec) bool {
		return spec.EnableInit(false)
	})
	withTTY := GetContainerProperty(rc, false, func(spec *model.ContainerSpec) bool {
		return spec.WithTTY(false)
	})

	rc.JobContainer = xe.NewJobContainer(&container.NewContainerInput{
		Cmd:             nil,
		Entrypoint:      entrypoint,
		Init:            enableInit,
		TTY:             withTTY,
		WorkingDir:      ext.ToContainerPath(rc.Config.Workdir),
		Image:           image,
		Username:        username,
		Password:        password,
		Name:            name,
		Env:             envList,
		ToolCache:       rc.getToolCache(ctx),
		Mounts:          mounts,
		NetworkMode:     rc.getNetworkName(ctx),
		NetworkAliases:  []string{sanitizeNetworkAlias(ctx, rc.Name)},
		Binds:           binds,
		Stdout:          logWriter,
		Stderr:          logWriter,
		Privileged:      rc.Config.Privileged,
		UsernsMode:      rc.Config.UsernsMode,
		DefaultPlatform: rc.dockerImagePlatform(ctx),
		ValidVolumes:    validVolumes,

		JobOptions:    rc.options(ctx),
		ConfigOptions: rc.Config.ContainerOptions,
	})
	if rc.JobContainer == nil {
		return errors.New("Failed to create job container")
	}

	return nil
}

func (rc *RunContext) startJobContainer() common.Executor {
	return func(ctx context.Context) error {
		if err := rc.prepareJobContainer(ctx); err != nil {
			return err
		}
		ep, err := rc.DockerEndpoint(ctx)
		if err != nil {
			return err
		}
		networkConfig := network.CreateOptions{
			Driver:     "bridge",
			Scope:      "local",
			EnableIPv6: &rc.Config.ContainerNetworkEnableIPv6,
		}
		return common.NewPipelineExecutor(
			rc.pullServicesImages(rc.Config.ForcePull),
			rc.JobContainer.Pull(rc.Config.ForcePull),
			rc.stopJobContainer(),
			docker.NewDockerNetworkCreateExecutor(ep, rc.getNetworkName(ctx), &networkConfig).IfBool(!rc.IsHostEnv(ctx) && rc.Config.ContainerNetworkMode == ""), // if the value of `ContainerNetworkMode` is empty string, then will create a new network for containers.
			rc.startServiceContainers(rc.getNetworkName(ctx)),
			rc.JobContainer.Create(rc.Config.ContainerCapAdd, rc.Config.ContainerCapDrop),
			rc.JobContainer.Start(false),
			rc.JobContainer.Copy(rc.JobContainer.GetActPath()+"/", &container.FileEntry{
				Name: "workflow/event.json",
				Mode: 0o644,
				Body: rc.EventJSON,
			}, &container.FileEntry{
				Name: "workflow/envs.txt",
				Mode: 0o666,
				Body: "",
			}),
			rc.waitForServiceContainers(),
		)(ctx)
	}
}

func (rc *RunContext) sh(ctx context.Context, script string) (stdout, stderr string, err error) {
	timeed, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	hout := &bytes.Buffer{}
	herr := &bytes.Buffer{}

	env := map[string]string{}
	maps.Copy(env, rc.Env)

	base := common.MustRandName(8)
	name := base + ".sh"
	oldStdout, oldStderr := rc.JobContainer.ReplaceLogWriter(hout, herr)
	err = rc.JobContainer.Copy(rc.JobContainer.GetActPath(), &container.FileEntry{
		Name: name,
		Mode: 0o644,
		Body: script,
	}).
		Then(rc.execJobContainer([]string{"sh", path.Join(rc.JobContainer.GetActPath(), name)},
			env, "", "")).
		Finally(func(context.Context) error {
			rc.JobContainer.ReplaceLogWriter(oldStdout, oldStderr)
			return nil
		})(timeed)
	if err != nil {
		return "", "", err
	}
	stdout = hout.String()
	stderr = herr.String()
	return stdout, stderr, nil
}

func (rc *RunContext) execJobContainer(cmd []string, env map[string]string, user, workdir string) common.Executor {
	return func(ctx context.Context) error {
		return rc.JobContainer.Exec(cmd, env, user, workdir)(ctx)
	}
}

func (rc *RunContext) ApplyExtraPath(ctx context.Context, env *map[string]string) {
	if len(rc.ExtraPath) > 0 {
		path := rc.JobContainer.GetPathVariableName()
		if rc.JobContainer.IsEnvironmentCaseInsensitive() {
			// On windows system Path and PATH could also be in the map
			for k := range *env {
				if strings.EqualFold(path, k) {
					path = k
					break
				}
			}
		}
		if (*env)[path] == "" {
			cenv := map[string]string{}
			var cpath string
			if err := rc.JobContainer.UpdateFromImageEnv(&cenv)(ctx); err == nil {
				if p, ok := cenv[path]; ok {
					cpath = p
				}
			}
			if len(cpath) == 0 {
				cpath = rc.JobContainer.DefaultPathVariable()
			}
			(*env)[path] = cpath
		}
		(*env)[path] = rc.JobContainer.JoinPathVariable(append(rc.ExtraPath, (*env)[path])...)
	}
}

func (rc *RunContext) UpdateExtraPath(ctx context.Context, githubEnvPath string) error {
	if common.Dryrun(ctx) {
		return nil
	}
	pathTar, err := rc.JobContainer.GetContainerArchive(ctx, githubEnvPath)
	if err != nil {
		return err
	}
	defer pathTar.Close()

	reader := tar.NewReader(pathTar)
	_, err = reader.Next()
	if err != nil && err != io.EOF {
		return err
	}
	s := bufio.NewScanner(reader)
	for s.Scan() {
		line := s.Text()
		if len(line) > 0 {
			rc.addPath(ctx, line)
		}
	}
	return nil
}

// stopJobContainer removes the job container (if it exists) and its volume (if it exists)
func (rc *RunContext) stopJobContainer() common.Executor {
	return func(ctx context.Context) error {
		if rc.cleanUpJobContainer != nil {
			return rc.cleanUpJobContainer(ctx)
		}
		return nil
	}
}

func (rc *RunContext) pullServicesImages(forcePull bool) common.Executor {
	return func(ctx context.Context) error {
		execs := make([]common.Executor, 0, len(rc.ServiceContainers))
		for _, c := range rc.ServiceContainers {
			execs = append(execs, c.Pull(forcePull))
		}
		return common.NewParallelExecutor(len(execs), execs...)(ctx)
	}
}

func (rc *RunContext) startServiceContainers(_ string) common.Executor {
	return func(ctx context.Context) error {
		execs := make([]common.Executor, 0, len(rc.ServiceContainers))
		for _, c := range rc.ServiceContainers {
			execs = append(execs, common.NewPipelineExecutor(
				c.Pull(false),
				c.Create(rc.Config.ContainerCapAdd, rc.Config.ContainerCapDrop),
				c.Start(false),
			))
		}
		return common.NewParallelExecutor(len(execs), execs...)(ctx)
	}
}

func waitForServiceContainer(ctx context.Context, c container.ExecutionsEnvironment) error {
	for {
		wait, err := c.IsHealthy(ctx)
		if err != nil {
			return err
		}
		if wait == time.Duration(0) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func (rc *RunContext) waitForServiceContainers() common.Executor {
	return func(ctx context.Context) error {
		execs := make([]common.Executor, 0, len(rc.ServiceContainers))
		for _, c := range rc.ServiceContainers {
			execs = append(execs, func(ctx context.Context) error {
				return waitForServiceContainer(ctx, c)
			})
		}
		return common.NewParallelExecutor(len(execs), execs...)(ctx)
	}
}

func (rc *RunContext) stopServiceContainers() common.Executor {
	return func(ctx context.Context) error {
		execs := make([]common.Executor, 0, len(rc.ServiceContainers))
		for _, c := range rc.ServiceContainers {
			execs = append(execs, c.Remove().Finally(c.Close()))
		}
		return common.NewParallelExecutor(len(execs), execs...)(ctx)
	}
}

// Prepare the mounts and binds for the worker

// ActionCacheDir is for rc
func (rc *RunContext) ActionCacheDir() string {
	if rc.Config.ActionCacheDir != "" {
		return rc.Config.ActionCacheDir
	}
	var xdgCache string
	var ok bool
	if xdgCache, ok = os.LookupEnv("XDG_CACHE_HOME"); !ok || xdgCache == "" {
		if home, err := os.UserHomeDir(); err == nil {
			xdgCache = filepath.Join(home, ".cache")
		} else if xdgCache, err = filepath.Abs("."); err != nil {
			// It's almost impossible to get here, so the temp dir is a good fallback
			xdgCache = os.TempDir()
		}
	}
	return filepath.Join(xdgCache, "act")
}

// Interpolate outputs after a job is done
func (rc *RunContext) interpolateOutputs() common.Executor {
	return func(ctx context.Context) error {
		ee := rc.NewExpressionEvaluator(ctx)
		for k, v := range rc.Run.Job().Outputs {
			interpolated := ee.Interpolate(ctx, v)
			if v != interpolated {
				rc.Run.Job().Outputs[k] = interpolated
			}
		}
		return nil
	}
}

func (rc *RunContext) getToolCache(ctx context.Context) string {
	if value, ok := rc.Config.Env["RUNNER_TOOL_CACHE"]; ok {
		return value
	}
	if rc.IsHostEnv(ctx) {
		return filepath.Join(rc.ActionCacheDir(), "tool_cache")
	}
	return "/opt/hostedtoolcache"
}

func (rc *RunContext) startContainer() common.Executor {
	return func(ctx context.Context) error {
		if rc.IsHostEnv(ctx) {
			return rc.startHostEnvironment()(ctx)
		}
		return rc.startJobContainer()(ctx)
	}
}

func (rc *RunContext) IsBareHostEnv(ctx context.Context) bool {
	platform := rc.runsOnImage(ctx)
	image := rc.containerImage(ctx)
	return image == "" && strings.EqualFold(platform, "-self-hosted")
}

const lxcPrefix = "lxc:"

func (rc *RunContext) IsLXCHostEnv(ctx context.Context) bool {
	platform := rc.runsOnImage(ctx)
	return strings.HasPrefix(platform, lxcPrefix)
}

func (rc *RunContext) GetLXCInfo(ctx context.Context) (isLXC bool, template, release, config string) {
	platform := rc.runsOnImage(ctx)
	if !strings.HasPrefix(platform, lxcPrefix) {
		return isLXC, template, release, config
	}
	isLXC = true
	s := strings.SplitN(strings.TrimPrefix(platform, lxcPrefix), ":", 3)
	template = s[0]
	if len(s) > 1 {
		release = s[1]
	}
	if len(s) > 2 {
		config = s[2]
	}
	return isLXC, template, release, config
}

func (rc *RunContext) IsHostEnv(ctx context.Context) bool {
	return rc.IsBareHostEnv(ctx) || rc.IsLXCHostEnv(ctx)
}

func (rc *RunContext) stopContainer() common.Executor {
	return func(ctx context.Context) error {
		return rc.stopJobContainer()(ctx)
	}
}

func (rc *RunContext) closeContainer() common.Executor {
	return func(ctx context.Context) error {
		if rc.JobContainer != nil {
			if err := rc.JobContainer.Close()(ctx); err != nil {
				return err
			}
		}
		rc.dockerEndpointMu.Lock()
		ep := rc.dockerEndpoint
		rc.dockerEndpoint = nil
		rc.dockerEndpointMu.Unlock()
		if ep != nil {
			return ep.Close()
		}
		return nil
	}
}

func (rc *RunContext) matrix() map[string]any {
	return rc.Matrix
}

func (rc *RunContext) result(result string) {
	rc.Run.Job().Result = result
}

func (rc *RunContext) steps() []*model.Step {
	return rc.Run.Job().Steps
}

// Executor returns a pipeline executor for all the steps in the job
func (rc *RunContext) Executor() (common.Executor, error) {
	var executor common.Executor
	jobType, err := rc.Run.Job().Type()

	switch jobType {
	case model.JobTypeDefault:
		executor = newJobExecutor(rc, &stepFactoryImpl{}, rc)
	case model.JobTypeReusableWorkflowLocal:
		executor = newLocalReusableWorkflowExecutor(rc)
	case model.JobTypeReusableWorkflowRemote:
		executor = newRemoteReusableWorkflowExecutor(rc)
	case model.JobTypeInvalid:
		return nil, err
	}

	return func(ctx context.Context) error {
		res, err := rc.isEnabled(ctx)
		if err != nil {
			return err
		}
		if res {
			timeoutctx, cancelTimeOut := evaluateTimeout(ctx, "job", rc.ExprEval, rc.Run.Job().TimeoutMinutes)
			defer cancelTimeOut()

			return executor(timeoutctx)
		}
		return nil
	}, nil
}

func (rc *RunContext) containerImage(ctx context.Context) string {
	job := rc.Run.Job()

	c := job.Container()
	if c != nil {
		return rc.ExprEval.Interpolate(ctx, c.Image)
	}

	return ""
}

func (rc *RunContext) runsOnImage(ctx context.Context) string {
	if rc.Run.Job().RunsOn() == nil {
		common.Logger(ctx).Errorf("'runs-on' key not defined in %s", rc.String())
	}

	runsOn := rc.Run.Job().RunsOn()
	for i, v := range runsOn {
		runsOn[i] = rc.ExprEval.Interpolate(ctx, v)
	}

	if pick := rc.Config.PlatformPicker; pick != nil {
		if image := pick(runsOn); image != "" {
			return image
		}
	}

	for _, platformName := range rc.runsOnPlatformNames(ctx) {
		image := rc.Config.Platforms[strings.ToLower(platformName)]
		if image != "" {
			return image
		}
	}

	return ""
}

// dockerImagePlatform returns the Docker image platform (e.g. "linux/amd64") to use for the job's
// containers. A per-label platform takes precedence over the runner-wide ContainerArchitecture.
func (rc *RunContext) dockerImagePlatform(ctx context.Context) string {
	if pick := rc.Config.DockerImagePlatformPicker; pick != nil {
		if platform := pick(rc.runsOnPlatformNames(ctx)); platform != "" {
			return platform
		}
	}
	return rc.Config.ContainerArchitecture
}

func (rc *RunContext) runsOnPlatformNames(ctx context.Context) []string {
	job := rc.Run.Job()

	if job.RunsOn() == nil {
		return []string{}
	}

	// Copy rawRunsOn from the job. `EvaluateYamlNode` later will mutate the yaml node in-place applying expression
	// evaluation to it from the RunContext -- but the job object is shared in matrix executions between multiple
	// running matrix jobs and `rc.EvalExpr` is specific to one matrix job.  By copying the object we avoid mutating the
	// shared field as it is accessed by multiple goroutines.
	rawRunsOn := job.RawRunsOn
	if err := rc.ExprEval.EvaluateYamlNode(ctx, &rawRunsOn); err != nil {
		common.Logger(ctx).Errorf("Error while evaluating runs-on: %v", err)
		return []string{}
	}

	return model.FlattenRunsOnNode(rawRunsOn)
}

func (rc *RunContext) platformImage(ctx context.Context) string {
	if containerImage := rc.containerImage(ctx); containerImage != "" {
		return containerImage
	}

	return rc.runsOnImage(ctx)
}

func (rc *RunContext) options(ctx context.Context) string {
	job := rc.Run.Job()
	c := job.Container()
	if c != nil {
		return rc.ExprEval.Interpolate(ctx, c.Options)
	}

	return ""
}

func (rc *RunContext) isEnabled(ctx context.Context) (bool, error) {
	job := rc.Run.Job()
	l := common.Logger(ctx)
	runJob, runJobErr := EvalBool(ctx, rc.ExprEval, job.IfClause(), exprparser.DefaultStatusCheckSuccess)
	jobType, jobTypeErr := job.Type()

	if runJobErr != nil {
		return false, fmt.Errorf("  \u274C  Error in if-expression: \"if: %s\" (%s)", job.IfClause(), runJobErr)
	}

	if jobType == model.JobTypeInvalid {
		return false, jobTypeErr
	}

	if !runJob {
		rc.result("skipped")
		l.WithField("jobResult", "skipped").Infof("Skipping job '%s' due to '%s'", job.Name, job.IfClause())
		return false, nil
	}

	if jobType != model.JobTypeDefault {
		return true, nil
	}

	img := rc.platformImage(ctx)
	if img == "" {
		for _, platformName := range rc.runsOnPlatformNames(ctx) {
			l.Infof("\U0001F6A7  Skipping unsupported platform -- Try running with `-P %+v=...`", platformName)
		}
		return false, nil
	}
	return true, nil
}

func mergeMaps(args ...map[string]string) map[string]string {
	rtnMap := make(map[string]string)
	for _, m := range args {
		maps.Copy(rtnMap, m)
	}
	return rtnMap
}

// Deprecated: use createSimpleContainerName
func createContainerName(parts ...string) string {
	name := strings.Join(parts, "-")
	pattern := regexp.MustCompile("[^a-zA-Z0-9]")
	name = pattern.ReplaceAllString(name, "-")
	name = strings.ReplaceAll(name, "--", "-")
	hash := sha256.Sum256([]byte(name))

	// SHA256 is 64 hex characters. So trim name to 63 characters to make room for the hash and separator
	trimmedName := strings.Trim(trimToLen(name, 63), "-")

	return fmt.Sprintf("%s-%x", trimmedName, hash)
}

func createSimpleContainerName(parts ...string) string {
	pattern := regexp.MustCompile("[^a-zA-Z0-9-]")
	name := make([]string, 0, len(parts))
	for _, v := range parts {
		v = pattern.ReplaceAllString(v, "-")
		v = strings.Trim(v, "-")
		for strings.Contains(v, "--") {
			v = strings.ReplaceAll(v, "--", "-")
		}
		if v != "" {
			name = append(name, v)
		}
	}
	return strings.Join(name, "_")
}

func trimToLen(s string, l int) string {
	if l < 0 {
		l = 0
	}
	if len(s) > l {
		return s[:l]
	}
	return s
}

func (rc *RunContext) getJobContext() *model.JobContext {
	jobStatus := "success"
	for _, stepStatus := range rc.StepResults {
		if stepStatus.Conclusion == model.StepStatusFailure {
			jobStatus = "failure"
			break
		}
	}
	return &model.JobContext{
		Status: jobStatus,
	}
}

func (rc *RunContext) getStepsContext() map[string]*model.StepResult {
	return rc.StepResults
}

func (rc *RunContext) getGithubContext(ctx context.Context) *model.GithubContext {
	logger := common.Logger(ctx)
	ghc := &model.GithubContext{
		Event:            make(map[string]any),
		Workflow:         rc.Run.Workflow.Name,
		RunAttempt:       rc.Config.Env["GITHUB_RUN_ATTEMPT"],
		RunID:            rc.Config.Env["GITHUB_RUN_ID"],
		RunNumber:        rc.Config.Env["GITHUB_RUN_NUMBER"],
		Actor:            rc.Config.Actor,
		EventName:        rc.Config.EventName,
		Action:           rc.CurrentStep,
		Token:            rc.Config.Token,
		Job:              rc.Run.JobID,
		ActionPath:       rc.ActionPath,
		ActionRepository: rc.Env["GITHUB_ACTION_REPOSITORY"],
		ActionRef:        rc.Env["GITHUB_ACTION_REF"],
		RepositoryOwner:  rc.Config.Env["GITHUB_REPOSITORY_OWNER"],
		RetentionDays:    rc.Config.Env["GITHUB_RETENTION_DAYS"],
		RunnerPerflog:    rc.Config.Env["RUNNER_PERFLOG"],
		RunnerTrackingID: rc.Config.Env["RUNNER_TRACKING_ID"],
		Repository:       rc.Config.Env["GITHUB_REPOSITORY"],
		Ref:              rc.Config.Env["GITHUB_REF"],
		Sha:              rc.Config.Env["SHA_REF"],
		RefName:          rc.Config.Env["GITHUB_REF_NAME"],
		RefType:          rc.Config.Env["GITHUB_REF_TYPE"],
		BaseRef:          rc.Config.Env["GITHUB_BASE_REF"],
		HeadRef:          rc.Config.Env["GITHUB_HEAD_REF"],
		WorkflowRef:      rc.Config.Env["GITHUB_WORKFLOW_REF"],
		Workspace:        rc.Config.Env["GITHUB_WORKSPACE"],
	}
	if rc.JobContainer != nil {
		ghc.EventPath = rc.JobContainer.GetActPath() + "/workflow/event.json"
		ghc.Workspace = rc.JobContainer.ToContainerPath(rc.Config.Workdir)
	}

	if ghc.RunAttempt == "" {
		ghc.RunAttempt = "1"
	}

	if ghc.RunID == "" {
		ghc.RunID = "1"
	}

	if ghc.RunNumber == "" {
		ghc.RunNumber = "1"
	}

	if ghc.RetentionDays == "" {
		ghc.RetentionDays = "0"
	}

	if ghc.RunnerPerflog == "" {
		ghc.RunnerPerflog = "/dev/null"
	}

	// Backwards compatibility for configs that require
	// a default rather than being run as a cmd
	if ghc.Actor == "" {
		ghc.Actor = "nektos/act"
	}

	{ // Adapt to Gitea
		if preset := rc.Config.PresetGitHubContext; preset != nil {
			ghc.Event = preset.Event
			ghc.RunID = preset.RunID
			ghc.RunNumber = preset.RunNumber
			ghc.RunAttempt = preset.RunAttempt
			ghc.Actor = preset.Actor
			ghc.Repository = preset.Repository
			ghc.EventName = preset.EventName
			ghc.Sha = preset.Sha
			ghc.Ref = preset.Ref
			ghc.RefName = preset.RefName
			ghc.RefType = preset.RefType
			ghc.HeadRef = preset.HeadRef
			ghc.BaseRef = preset.BaseRef
			ghc.Token = preset.Token
			ghc.RepositoryOwner = preset.RepositoryOwner
			ghc.RetentionDays = preset.RetentionDays
			ghc.WorkflowRef = preset.WorkflowRef

			instance := rc.Config.GitHubInstance
			if instance != "" && !strings.HasPrefix(instance, "http://") &&
				!strings.HasPrefix(instance, "https://") {
				instance = "https://" + instance
			}
			ghc.ServerURL = instance
			ghc.APIURL = instance + "/api/v1" // the version of Gitea is v1
			ghc.GraphQLURL = ""               // Gitea doesn't support graphql
			return ghc
		}
	}

	if rc.EventJSON != "" {
		err := json.Unmarshal([]byte(rc.EventJSON), &ghc.Event)
		if err != nil {
			logger.Errorf("Unable to Unmarshal event '%s': %v", rc.EventJSON, err)
		}
	}

	ghc.SetBaseAndHeadRef()
	repoPath := rc.Config.Workdir
	ghc.SetRepositoryAndOwner(ctx, rc.Config.GitHubInstance, rc.Config.RemoteName, repoPath)
	if ghc.Ref == "" {
		ghc.SetRef(ctx, rc.Config.DefaultBranch, repoPath)
	}
	if ghc.Sha == "" {
		ghc.SetSha(ctx, repoPath)
	}

	ghc.SetRefTypeAndName()

	// defaults
	ghc.ServerURL = "https://github.com"
	ghc.APIURL = "https://api.github.com"
	ghc.GraphQLURL = "https://api.github.com/graphql"
	// per GHES
	if rc.Config.GitHubInstance != "github.com" {
		ghc.ServerURL = fmt.Sprintf("https://%s", rc.Config.GitHubInstance)
		ghc.APIURL = fmt.Sprintf("https://%s/api/v3", rc.Config.GitHubInstance)
		ghc.GraphQLURL = fmt.Sprintf("https://%s/api/graphql", rc.Config.GitHubInstance)
	}

	{ // Adapt to Gitea
		instance := rc.Config.GitHubInstance
		if instance != "" && !strings.HasPrefix(instance, "http://") &&
			!strings.HasPrefix(instance, "https://") {
			instance = "https://" + instance
		}
		ghc.ServerURL = instance
		ghc.APIURL = instance + "/api/v1" // the version of Gitea is v1
		ghc.GraphQLURL = ""               // Gitea doesn't support graphql
	}

	// allow to be overridden by user
	if rc.Config.Env["GITHUB_SERVER_URL"] != "" {
		ghc.ServerURL = rc.Config.Env["GITHUB_SERVER_URL"]
	}
	if rc.Config.Env["GITHUB_API_URL"] != "" {
		ghc.APIURL = rc.Config.Env["GITHUB_API_URL"]
	}
	if rc.Config.Env["GITHUB_GRAPHQL_URL"] != "" {
		ghc.GraphQLURL = rc.Config.Env["GITHUB_GRAPHQL_URL"]
	}

	return ghc
}

func isLocalCheckout(ghc *model.GithubContext, step *model.Step) bool {
	if step.Type() == model.StepTypeInvalid {
		// This will be errored out by the executor later, we need this here to avoid a null panic though
		return false
	}
	if step.Type() != model.StepTypeUsesActionRemote {
		return false
	}
	remoteAction := newRemoteAction(step.Uses)
	if remoteAction == nil {
		// IsCheckout() will nil panic if we dont bail out early
		return false
	}
	if !remoteAction.IsCheckout() {
		return false
	}

	if repository, ok := step.With["repository"]; ok && repository != ghc.Repository {
		return false
	}
	if repository, ok := step.With["ref"]; ok && repository != ghc.Ref {
		return false
	}
	return true
}

func nestedMapLookup(m map[string]any, ks ...string) (rval any) {
	var ok bool

	if len(ks) == 0 { // degenerate input
		return nil
	}
	if rval, ok = m[ks[0]]; !ok {
		return nil
	} else if len(ks) == 1 { // we've reached the final key
		return rval
	} else if m, ok = rval.(map[string]any); !ok {
		return nil
	}
	// 1+ more keys
	return nestedMapLookup(m, ks[1:]...)
}

func (rc *RunContext) withGithubEnv(ctx context.Context, github *model.GithubContext, env map[string]string) map[string]string {
	set := func(k, v string) {
		for _, prefix := range []string{"FORGEJO", "GITHUB"} {
			env[prefix+"_"+k] = v
		}
	}
	env["CI"] = "true"
	set("WORKFLOW", github.Workflow)
	set("RUN_ATTEMPT", github.RunAttempt)
	set("RUN_ID", github.RunID)
	set("RUN_NUMBER", github.RunNumber)
	set("ACTION", github.Action)
	set("ACTION_PATH", github.ActionPath)
	set("ACTION_REPOSITORY", github.ActionRepository)
	set("ACTION_REF", github.ActionRef)
	set("ACTIONS", "true")
	set("ACTOR", github.Actor)
	set("REPOSITORY", github.Repository)
	set("EVENT_NAME", github.EventName)
	set("EVENT_PATH", github.EventPath)
	set("WORKSPACE", github.Workspace)
	set("SHA", github.Sha)
	set("REF", github.Ref)
	set("REF_NAME", github.RefName)
	set("REF_TYPE", github.RefType)
	set("TOKEN", github.Token)
	set("JOB", github.Job)
	set("REPOSITORY_OWNER", github.RepositoryOwner)
	set("RETENTION_DAYS", github.RetentionDays)
	env["RUNNER_PERFLOG"] = github.RunnerPerflog
	env["RUNNER_TRACKING_ID"] = github.RunnerTrackingID
	set("BASE_REF", github.BaseRef)
	set("HEAD_REF", github.HeadRef)
	set("SERVER_URL", github.ServerURL)
	set("API_URL", github.APIURL)
	set("WORKFLOW_REF", github.WorkflowRef)

	if rc.Config.ArtifactServerPath != "" {
		setActionRuntimeVars(rc, env)
	}

	for _, platformName := range rc.runsOnPlatformNames(ctx) {
		if platformName != "" {
			if platformName == "ubuntu-latest" {
				// hardcode current ubuntu-latest since we have no way to check that 'on the fly'
				env["ImageOS"] = "ubuntu20"
			} else {
				platformName = strings.SplitN(strings.Replace(platformName, `-`, ``, 1), `.`, 2)[0]
				env["ImageOS"] = platformName
			}
		}
	}

	return env
}

func setActionRuntimeVars(rc *RunContext, env map[string]string) {
	actionsRuntimeURL := os.Getenv("ACTIONS_RUNTIME_URL")
	if actionsRuntimeURL == "" {
		actionsRuntimeURL = fmt.Sprintf("http://%s:%s/", rc.Config.ArtifactServerAddr, rc.Config.ArtifactServerPort)
	}
	env["ACTIONS_RUNTIME_URL"] = actionsRuntimeURL

	actionsRuntimeToken := os.Getenv("ACTIONS_RUNTIME_TOKEN")
	if actionsRuntimeToken == "" {
		actionsRuntimeToken = "token"
	}
	env["ACTIONS_RUNTIME_TOKEN"] = actionsRuntimeToken
}

func (rc *RunContext) handleCredentials(ctx context.Context) (string, string, error) {
	// TODO: remove below 2 lines when we can release act with breaking changes
	username := rc.Config.Secrets["DOCKER_USERNAME"]
	password := rc.Config.Secrets["DOCKER_PASSWORD"]

	container := rc.Run.Job().Container()
	if container == nil || container.Credentials == nil {
		return username, password, nil
	}

	if container.Credentials != nil && len(container.Credentials) != 2 {
		err := fmt.Errorf("invalid property count for key 'credentials:'")
		return "", "", err
	}

	ee := rc.NewExpressionEvaluator(ctx)
	if username = ee.Interpolate(ctx, container.Credentials["username"]); username == "" {
		err := fmt.Errorf("failed to interpolate container.credentials.username")
		return "", "", err
	}
	if password = ee.Interpolate(ctx, container.Credentials["password"]); password == "" {
		err := fmt.Errorf("failed to interpolate container.credentials.password")
		return "", "", err
	}

	if container.Credentials["username"] == "" || container.Credentials["password"] == "" {
		err := fmt.Errorf("container.credentials cannot be empty")
		return "", "", err
	}

	return username, password, nil
}

func (rc *RunContext) handleServiceCredentials(ctx context.Context, creds map[string]string) (username, password string, err error) {
	if creds == nil {
		return username, password, err
	}
	if len(creds) != 2 {
		err = fmt.Errorf("invalid property count for key 'credentials:'")
		return username, password, err
	}

	ee := rc.NewExpressionEvaluator(ctx)
	if username = ee.Interpolate(ctx, creds["username"]); username == "" {
		err = fmt.Errorf("failed to interpolate credentials.username")
		return username, password, err
	}

	if password = ee.Interpolate(ctx, creds["password"]); password == "" {
		err = fmt.Errorf("failed to interpolate credentials.password")
		return username, password, err
	}

	return username, password, err
}

// GetServiceBindsAndMounts returns the binds and mounts for the service container, resolving paths as appopriate
func (rc *RunContext) GetServiceBindsAndMounts(svcVolumes []string) ([]string, map[string]string) {
	containerDaemonSocket := rc.Config.GetContainerDaemonSocket()
	binds := []string{}
	if containerDaemonSocket != "-" {
		daemonPath := getDockerDaemonSocketMountPath(containerDaemonSocket)
		binds = append(binds, fmt.Sprintf("%s:%s", daemonPath, "/var/run/docker.sock"))
	}

	mounts := map[string]string{}

	for _, v := range svcVolumes {
		if !strings.Contains(v, ":") || filepath.IsAbs(v) {
			// Bind anonymous volume or host file.
			binds = append(binds, v)
		} else {
			// Mount existing volume.
			paths := strings.SplitN(v, ":", 2)
			mounts[paths[0]] = paths[1]
		}
	}

	return binds, mounts
}

func GetContainerProperty[T any](rc *RunContext, defaultValue T, extract func(spec *model.ContainerSpec) T) T {
	if run := rc.Run; run != nil {
		if job := rc.Run.Job(); job != nil {
			if jobContainer := job.Container(); jobContainer != nil {
				return extract(jobContainer)
			}
		}
	}

	return defaultValue
}
