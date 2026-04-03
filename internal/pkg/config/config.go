// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package config

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"code.forgejo.org/forgejo/runner/v12/internal/pkg/labels"
	gouuid "github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/powerman/fileuri"
	log "github.com/sirupsen/logrus"
	"go.yaml.in/yaml/v3"
)

// Log represents the configuration for logging.
type Log struct {
	Level    string // Level indicates the logging level.
	JobLevel string // JobLevel indicates the job logging level.
}

// Runner represents the configuration for the runner.
type Runner struct {
	File            string            // File specifies the file path for the runner.
	Capacity        int               // Capacity specifies the capacity of the runner.
	Envs            map[string]string // Envs stores environment variables for the runner.
	EnvFile         string            // EnvFile specifies the path to the file containing environment variables for the runner.
	Timeout         time.Duration     // Timeout specifies the duration for runner timeout.
	ShutdownTimeout time.Duration     // ShutdownTimeout specifies the duration to wait for running jobs to complete during a shutdown of the runner.
	Insecure        bool              // Insecure indicates whether the runner operates in an insecure mode.
	FetchTimeout    time.Duration     // FetchTimeout specifies the timeout duration for fetching resources.
	ReportInterval  time.Duration     // ReportInterval specifies the interval duration for reporting status and logs of a running job.
	DefaultLabels   []string          // Default labels for a runner, if not configured at server connection.
	ReportRetry     Retry             // At the end of a job, configures retrying sending logs to remote.
}

// Retry defines the retry behaviour of Runner when sending logs to Forgejo.
type Retry struct {
	MaxRetries   uint          // Maximum number of retry attempts, defaults to 10.
	InitialDelay time.Duration // Initial delay between retries, defaults to 100ms.  Delay between retries doubles up to `max_delay`.
	MaxDelay     time.Duration // Maximum delay between retries, defaults to 0, 0 is treated as no maximum.
}

// Cache represents the configuration for caching.
type Cache struct {
	Enabled                 bool   // Enabled indicates whether caching is enabled.
	Dir                     string // Dir specifies the directory path for caching.
	Host                    string // Host specifies the caching host.
	Port                    uint16 // Port specifies the caching port.
	ProxyPort               uint16 // ProxyPort specifies the cache proxy port.
	ExternalServer          string // ExternalServer specifies the URL of external cache server
	ActionsCacheURLOverride string // Allows the user to override the ACTIONS_CACHE_URL passed to the workflow containers
	Secret                  string // Shared secret to secure caches.
}

// Container represents the configuration for the container.
type Container struct {
	Network       string   // Network specifies the network for the container.
	EnableIPv6    bool     // EnableIPv6 indicates whether the network is created with IPv6 enabled.
	Privileged    bool     // Privileged indicates whether the container runs in privileged mode.
	Options       string   // Options specifies additional options for the container.
	WorkdirParent string   // WorkdirParent specifies the parent directory for the container's working directory.
	ValidVolumes  []string // ValidVolumes specifies the volumes (including bind mounts) can be mounted to containers.
	DockerHost    string   // DockerHost specifies the Docker host. It overrides the value specified in environment variable DOCKER_HOST.
	ForcePull     bool     // Pull docker image(s) even if already present
	ForceRebuild  bool     // Rebuild local docker image(s) even if already present
}

// Host represents the configuration for the host.
type Host struct {
	WorkdirParent string // WorkdirParent specifies the parent directory for the host's working directory.
}

type Kubernetes struct {
	Namespace   string
	KubeConfig  string // Empty means in-cluster config.
	PollTimeout time.Duration
}

// Server configures connections to Forgejo and their behaviour.
type Server struct {
	Connections map[string]*Connection // Connections defines which Forgejo instance(s) Forgejo Runner should connect to. The map's key serves as connection name.
}

// Connection defines a connection to a Forgejo instance.
type Connection struct {
	URL           *url.URL      // URL of the Forgejo instance to connect to. Mandatory value.
	UUID          gouuid.UUID   // UUID of the runner. Mandatory value.
	Token         string        // Token of the runner. Mandatory value.
	Labels        labels.Labels // Labels of the runner. Mandatory value.
	FetchInterval time.Duration // FetchInterval specifies the interval duration for fetching resources.

	// Legacy support for `.runner` registration file leaves the need for a hack here, which should be removed when
	// `.runner` is deprecated and removed.  Labels in the `.runner` file came from the first runner registration and
	// may be used, but may also be overridden by other configuration sources.  If labels are specified in `.runner`,
	// then they are configured on the connection with the priority `OverrideIfPossible` which indicates that any other
	// source of labels (such as `Runner.DefaultLabels`) should override these labels.
	//
	// This field is internal to the `config` package because the priority should be resolved by `New()` before the
	// config is exposed for usage.
	labelPriority labelPriority
}

type labelPriority int64

const (
	userSpecified      labelPriority = iota // default priority -- indicates to use these labels
	overrideIfPossible                      // provided labels should be overridden by default labels
)

// Config represents the overall configuration.
type Config struct {
	Log        Log       // Log represents the configuration for logging.
	Runner     Runner    // Runner represents the configuration for the runner.
	Cache      Cache     // Cache represents the configuration for caching.
	Container  Container // Container represents the configuration for the container.
	Host       Host      // Host represents the configuration for the host.
	Kubernetes Kubernetes
	Server     Server // Server configures connections to Forgejo and their behaviour.
}

// serializedConfiguration is the top-level structure of the on-disk format of the Forgejo Runner configuration.
type serializedConfiguration struct {
	Log        serializedLogSettings        `yaml:"log"`       // Log represents the configuration for logging.
	Runner     serializedRunnerSettings     `yaml:"runner"`    // Runner represents the configuration for the runner.
	Cache      serializedCacheSettings      `yaml:"cache"`     // Cache represents the configuration for caching.
	Container  serializedContainerSettings  `yaml:"container"` // Container represents the configuration for the container.
	Host       serializedHostSettings       `yaml:"host"`      // Host represents the configuration for the host.
	Kubernetes serializedKubernetesSettings `yaml:"kubernetes"`
	Server     serializedServerSettings     `yaml:"server"` // Server configures connections to Forgejo and their behaviour.
}

func (s *serializedConfiguration) applyTo(config *Config) error {
	if err := s.Log.applyTo(config); err != nil {
		return fmt.Errorf("invalid `log` settings: %w", err)
	}
	if err := s.Runner.applyTo(config); err != nil {
		return fmt.Errorf("invalid `runner` settings: %w", err)
	}
	if err := s.Cache.applyTo(config); err != nil {
		return fmt.Errorf("invalid `cache` settings: %w", err)
	}
	if err := s.Container.applyTo(config); err != nil {
		return fmt.Errorf("invalid `container` settings: %w", err)
	}
	if err := s.Host.applyTo(config); err != nil {
		return fmt.Errorf("invalid `host` settings: %w", err)
	}
	if err := s.Kubernetes.applyTo(config); err != nil {
		return fmt.Errorf("invalid `kubernetes` settings: %w", err)
	}
	if err := s.Server.applyTo(config); err != nil {
		return fmt.Errorf("invalid `server` settings: %w", err)
	}
	if err := s.Runner.applyGlobalDefaultsTo(config); err != nil {
		return fmt.Errorf("invalid `server` settings: %w", err)
	}
	return nil
}

// serializedLogSettings represents the on-disk format for configuring logging.
type serializedLogSettings struct {
	Level    string `yaml:"level"`     // Level indicates the logging level of Forgejo Runner.
	JobLevel string `yaml:"job_level"` // JobLevel indicates the logging level of jobs.
}

func (s *serializedLogSettings) applyTo(config *Config) error {
	if s.Level != "" {
		if _, err := log.ParseLevel(s.Level); err != nil {
			return fmt.Errorf("invalid `level` %q: %w", s.Level, err)
		}

		config.Log.Level = s.Level
	}
	if s.JobLevel != "" {
		if _, err := log.ParseLevel(s.JobLevel); err != nil {
			return fmt.Errorf("invalid `job_level` %q: %w", s.JobLevel, err)
		}

		config.Log.JobLevel = s.JobLevel
	}
	return nil
}

// serializedRunnerSettings defines the on-disk format for configuring the runner's behaviour.
type serializedRunnerSettings struct {
	File            string                        `yaml:"file"`             // File specifies the path where `.runner` can be found.
	Capacity        int                           `yaml:"capacity"`         // Capacity specifies the maximum number of jobs that the runner executes concurrently.
	Envs            map[string]string             `yaml:"envs"`             // Envs stores environment variables for the runner.
	EnvFile         string                        `yaml:"env_file"`         // EnvFile specifies the path to the file containing environment variables for the runner.
	Timeout         time.Duration                 `yaml:"timeout"`          // Timeout specifies the duration for runner timeout.
	ShutdownTimeout time.Duration                 `yaml:"shutdown_timeout"` // ShutdownTimeout specifies the duration to wait for running jobs to complete during a shutdown of the runner.
	Insecure        bool                          `yaml:"insecure"`         // Insecure indicates whether the runner operates in an insecure mode.
	FetchTimeout    time.Duration                 `yaml:"fetch_timeout"`    // FetchTimeout specifies the timeout duration for fetching resources.
	FetchInterval   time.Duration                 `yaml:"fetch_interval"`   // FetchInterval specifies the interval duration for fetching resources.  Operates as a default for all connections, if not provided by a specific connection.
	ReportInterval  time.Duration                 `yaml:"report_interval"`  // ReportInterval specifies the interval duration for reporting status and logs of a running job.
	Labels          []string                      `yaml:"labels"`           // Labels specify the labels of the runner. Labels are declared on each startup.
	ReportRetry     serializedReportRetrySettings `yaml:"report_retry"`     // ReportRetry defines whether sending logs to the remote should be retried after a job has completed.
}

func (s *serializedRunnerSettings) applyTo(config *Config) error {
	if s.File != "" {
		config.Runner.File = s.File
	}
	if s.Capacity != 0 {
		if s.Capacity < 0 {
			log.Warnf("Ignoring invalid `runner.capacity` %q", s.Capacity)
		} else {
			config.Runner.Capacity = s.Capacity
		}
	}
	if len(s.Envs) > 0 {
		if config.Runner.Envs == nil {
			config.Runner.Envs = make(map[string]string, len(s.Envs))
		}
		maps.Copy(config.Runner.Envs, s.Envs)
	}
	if s.EnvFile != "" {
		config.Runner.EnvFile = s.EnvFile
		if config.Runner.Envs == nil {
			config.Runner.Envs = make(map[string]string, len(s.Envs))
		}
		env, err := readEnvFile(filepath.FromSlash(s.EnvFile))
		if err != nil {
			return err
		}
		maps.Copy(config.Runner.Envs, env)
	}
	if s.Timeout != 0 {
		if s.Timeout < 0 {
			log.Warnf("Ignoring invalid `runner.timeout`: %q", s.Timeout)
		} else {
			config.Runner.Timeout = s.Timeout
		}
	}
	if s.ShutdownTimeout < 0 {
		log.Warnf("Ignoring invalid `runner.shutdown_timeout`: %q", s.ShutdownTimeout)
	} else {
		config.Runner.ShutdownTimeout = s.ShutdownTimeout
	}
	config.Runner.Insecure = s.Insecure
	if s.FetchTimeout != 0 {
		if s.FetchTimeout < 0 {
			log.Warnf("Ignoring invalid `runner.fetch_timeout`: %q", s.FetchTimeout)
		} else {
			config.Runner.FetchTimeout = s.FetchTimeout
		}
	}
	if s.ReportInterval != 0 {
		if s.ReportInterval < 0 {
			log.Warnf("Ignoring invalid `runner.report_interval`: %q", s.ReportInterval)
		} else {
			config.Runner.ReportInterval = s.ReportInterval
		}
	}
	if len(s.Labels) != 0 {
		if config.Runner.DefaultLabels == nil {
			config.Runner.DefaultLabels = make([]string, 0, len(s.Labels))
		}
		config.Runner.DefaultLabels = append(config.Runner.DefaultLabels, s.Labels...)
	}
	if err := s.ReportRetry.applyTo(config); err != nil {
		return fmt.Errorf("invalid `report_retry`: %w", err)
	}
	return nil
}

func (s *serializedRunnerSettings) applyGlobalDefaultsTo(config *Config) error {
	if s.FetchInterval != 0 {
		if s.FetchInterval < 0 {
			log.Warnf("Ignoring invalid `runner.fetch_interval`: %q", s.FetchInterval)
		} else {
			for _, conn := range config.Server.Connections {
				if conn.FetchInterval == 0 {
					conn.FetchInterval = s.FetchInterval
				}
			}
		}
	}
	return nil
}

// serializedReportRetrySettings adjusts Runner's retry behaviour when sending job logs to Forgejo.
type serializedReportRetrySettings struct {
	MaxRetries   *uint          `yaml:"max_retries"`   // Maximum number of retry attempts, defaults to 10.
	InitialDelay *time.Duration `yaml:"initial_delay"` // Initial delay between retries, defaults to 100ms.  Delay between retries doubles up to `max_delay`.
	MaxDelay     time.Duration  `yaml:"max_delay"`     // Maximum delay between retries, defaults to 0, 0 is treated as no maximum.
}

func (s *serializedReportRetrySettings) applyTo(config *Config) error {
	if s.MaxRetries != nil {
		if *s.MaxRetries < 1 {
			log.Warnf("Ignoring invalid `runner.report_retry.max_retries`: %d", *s.MaxRetries)
		} else {
			config.Runner.ReportRetry.MaxRetries = *s.MaxRetries
		}
	}
	if s.InitialDelay != nil {
		if *s.InitialDelay <= 0 {
			log.Warnf("Ignoring invalid `runner.report_retry.initial_delay`: %q", *s.InitialDelay)
		} else {
			config.Runner.ReportRetry.InitialDelay = *s.InitialDelay
		}
	}
	if s.MaxDelay < 0 {
		log.Warnf("Ignoring invalid `runner.report_retry.max_delay`: %q", s.MaxDelay)
	} else {
		config.Runner.ReportRetry.MaxDelay = s.MaxDelay
	}
	return nil
}

// serializedCacheSettings represents the configuration for caching.
type serializedCacheSettings struct {
	Enabled                 *bool  `yaml:"enabled"`                    // Enabled indicates whether caching is enabled. It is a pointer to distinguish between false and not set. If not set, it will be true.
	Dir                     string `yaml:"dir"`                        // Dir specifies the directory path for caching.
	Host                    string `yaml:"host"`                       // Host specifies the caching host.
	Port                    uint16 `yaml:"port"`                       // Port specifies the caching port.
	ProxyPort               uint16 `yaml:"proxy_port"`                 // ProxyPort specifies the cache proxy port.
	ExternalServer          string `yaml:"external_server"`            // ExternalServer specifies the URL of external cache server
	ActionsCacheURLOverride string `yaml:"actions_cache_url_override"` // Allows the user to override the ACTIONS_CACHE_URL passed to the workflow containers
	Secret                  string `yaml:"secret"`                     // Secret defines a secret to secure all caches. Secret and SecretURL are mutually exclusive.
	SecretURL               string `yaml:"secret_url"`                 // SecretURL defines a URL where the Secret can be loaded from. Secret and SecretURL are mutually exclusive.
}

func (s *serializedCacheSettings) applyTo(config *Config) error {
	if s.Secret != "" && s.SecretURL != "" {
		return fmt.Errorf("`secret` and `secret_url` are mutually exclusive")
	}

	if s.Enabled != nil {
		config.Cache.Enabled = *s.Enabled
	}
	if s.Dir != "" {
		config.Cache.Dir = s.Dir
	}
	config.Cache.Host = s.Host
	config.Cache.Port = s.Port
	config.Cache.ProxyPort = s.ProxyPort
	config.Cache.ExternalServer = s.ExternalServer
	config.Cache.ActionsCacheURLOverride = s.ActionsCacheURLOverride

	var resolvedSecret string
	if s.SecretURL != "" {
		var err error
		if resolvedSecret, err = ResolveSecretURL(s.SecretURL); err != nil {
			return fmt.Errorf("cannot resolve `secret_url`: %w", err)
		}
	} else {
		resolvedSecret = s.Secret
	}
	config.Cache.Secret = strings.TrimSpace(resolvedSecret)

	return nil
}

// serializedContainerSettings is the on-disk format of settings that configure the job containers' behaviour.
type serializedContainerSettings struct {
	Network       string   `yaml:"network"`        // Network specifies the network for the container.
	NetworkMode   string   `yaml:"network_mode"`   // Deprecated: use Network instead. Could be removed after Gitea 1.20
	EnableIPv6    bool     `yaml:"enable_ipv6"`    // EnableIPv6 indicates whether the network is created with IPv6 enabled.
	Privileged    bool     `yaml:"privileged"`     // Privileged indicates whether the container runs in privileged mode.
	Options       string   `yaml:"options"`        // Options specifies additional options for the container.
	WorkdirParent string   `yaml:"workdir_parent"` // WorkdirParent specifies the parent directory for the container's working directory.
	ValidVolumes  []string `yaml:"valid_volumes"`  // ValidVolumes specifies the volumes (including bind mounts) can be mounted to containers.
	DockerHost    string   `yaml:"docker_host"`    // DockerHost specifies the Docker host. It overrides the value specified in environment variable DOCKER_HOST.
	ForcePull     bool     `yaml:"force_pull"`     // Pull docker image(s) even if already present
	ForceRebuild  bool     `yaml:"force_rebuild"`  // Rebuild local docker image(s) even if already present
}

func (s *serializedContainerSettings) applyTo(config *Config) error {
	config.Container.Network = s.Network
	if s.NetworkMode != "" && s.Network == "" {
		log.Warn("`container.network_mode` is deprecated, use `container.network` instead.")
		if s.NetworkMode == "bridge" {
			// `bridge` means to create a new network for a job. This translates to an empty network name with the new
			// setting.
			config.Container.Network = ""
		} else {
			config.Container.Network = s.NetworkMode
		}
	}
	config.Container.EnableIPv6 = s.EnableIPv6
	config.Container.Privileged = s.Privileged
	config.Container.Options = s.Options
	if s.WorkdirParent != "" {
		config.Container.WorkdirParent = s.WorkdirParent
	}
	if len(s.ValidVolumes) > 0 {
		if config.Container.ValidVolumes == nil {
			config.Container.ValidVolumes = make([]string, 0, len(s.ValidVolumes))
		}
		config.Container.ValidVolumes = append(config.Container.ValidVolumes, s.ValidVolumes...)
	}
	if s.DockerHost != "" {
		config.Container.DockerHost = s.DockerHost
	}
	config.Container.ForcePull = s.ForcePull
	config.Container.ForceRebuild = s.ForceRebuild
	return nil
}

// serializedHostSettings represents the configuration for the host.
type serializedHostSettings struct {
	WorkdirParent string `yaml:"workdir_parent"` // WorkdirParent specifies the parent directory for the host's working directory.
}

func (s *serializedHostSettings) applyTo(config *Config) error {
	if s.WorkdirParent != "" {
		config.Host.WorkdirParent = filepath.FromSlash(s.WorkdirParent)
	}

	return nil
}

type serializedKubernetesSettings struct {
	Namespace   string        `yaml:"namespace"`
	KubeConfig  string        `yaml:"kubeconfig"`
	PollTimeout time.Duration `yaml:"poll_timeout"`
}

func (s *serializedKubernetesSettings) applyTo(config *Config) error {
	if s.Namespace != "" {
		config.Kubernetes.Namespace = s.Namespace
	}
	if s.KubeConfig != "" {
		config.Kubernetes.KubeConfig = s.KubeConfig
	}
	if s.PollTimeout != 0 {
		if s.PollTimeout < 0 {
			log.Warnf("Ignoring invalid `kubernetes.poll_timeout`: %q", s.PollTimeout)
		} else {
			config.Kubernetes.PollTimeout = s.PollTimeout
		}
	}
	return nil
}

// serializedServerSettings declares connections to Forgejo instances.
type serializedServerSettings struct {
	Connections map[string]serializedConnectionSettings `yaml:"connections"` // Connections defines which Forgejo instance(s) Forgejo Runner should connect to. The map's key serves as connection name.
}

func (s *serializedServerSettings) applyTo(config *Config) error {
	for name, conn := range s.Connections {
		if err := conn.applyTo(config, name); err != nil {
			return fmt.Errorf("connection %q is invalid: %w", name, err)
		}
	}

	return nil
}

// serializedConnectionSettings defines a connection to a Forgejo instance.
type serializedConnectionSettings struct {
	URL           string        `yaml:"url"`            // URL of the Forgejo instance to connect to. Mandatory value.
	UUID          string        `yaml:"uuid"`           // UUID of the runner. Mandatory value.
	Token         string        `yaml:"token"`          // Token of the runner. Token and TokenURL are mutually exclusive.
	TokenURL      string        `yaml:"token_url"`      // TokenURL defines a URL where the runner token can be loaded from. Token and TokenURL are mutually exclusive.
	Labels        []string      `yaml:"labels"`         // Labels of the runner. If not present, runner.labels will be used instead.
	FetchInterval time.Duration `yaml:"fetch_interval"` // FetchInterval specifies the interval duration for fetching resources.
}

func (s *serializedConnectionSettings) applyTo(config *Config, connectionName string) error {
	if s.URL == "" {
		return errors.New("`url` is empty")
	}
	if s.UUID == "" {
		return errors.New("`uuid` is empty")
	}
	if s.Token == "" && s.TokenURL == "" {
		return errors.New("`token` and `token_url` are empty")
	}
	if s.Token != "" && s.TokenURL != "" {
		return errors.New("`token` and `token_url` are mutually exclusive")
	}

	parsedURL, err := url.ParseRequestURI(s.URL)
	if err != nil {
		return fmt.Errorf("malformed `url` %q: %w", s.URL, err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return fmt.Errorf("invalid scheme in `url` %q: only http and https supported", s.URL)
	}
	parsedUUID, err := gouuid.Parse(s.UUID)
	if err != nil {
		return fmt.Errorf("malformed `uuid` %q: %w", s.UUID, err)
	}
	var parsedLabels labels.Labels
	for _, label := range s.Labels {
		parsedLabel, err := labels.Parse(label)
		if err != nil {
			return fmt.Errorf("malformed `label` %q: %w", label, err)
		}
		parsedLabels = append(parsedLabels, parsedLabel)
	}
	var resolvedToken string
	if s.TokenURL != "" {
		if resolvedToken, err = ResolveSecretURL(s.TokenURL); err != nil {
			return fmt.Errorf("invalid `token_url`: %w", err)
		}
	} else {
		resolvedToken = s.Token
	}
	resolvedToken = strings.TrimSpace(resolvedToken)
	if !IsValidToken(resolvedToken) {
		return errors.New("token contains invalid characters")
	}

	if config.Server.Connections == nil {
		config.Server.Connections = map[string]*Connection{}
	}
	config.Server.Connections[connectionName] = &Connection{
		URL:           parsedURL,
		UUID:          parsedUUID,
		Token:         resolvedToken,
		Labels:        parsedLabels,
		FetchInterval: s.FetchInterval,
	}

	return nil
}

// Option for customizing Config instances after their initialization.
type Option func(config *Config) error

// New returns a new Config initialized with default values. Use any number of Option arguments to customize the
// Config after initialization.
func New(opts ...Option) (*Config, error) {
	home, _ := os.UserHomeDir()

	config := &Config{
		Log: Log{Level: "info", JobLevel: "info"},
		Runner: Runner{
			File:           ".runner",
			Capacity:       1,
			Timeout:        3 * time.Hour,
			FetchTimeout:   30 * time.Second,
			ReportInterval: time.Second,
			ReportRetry: Retry{
				MaxRetries:   10,
				InitialDelay: 100 * time.Millisecond,
				MaxDelay:     0,
			},
			DefaultLabels: []string{},
		},
		Cache:      Cache{Enabled: true, Dir: filepath.Join(home, ".cache", "actcache")},
		Container:  Container{DockerHost: "-", WorkdirParent: "workspace", ValidVolumes: []string{}},
		Host:       Host{WorkdirParent: filepath.Join(home, ".cache", "act")},
		Kubernetes: Kubernetes{Namespace: "default", PollTimeout: 10 * time.Minute},
	}

	for _, opt := range opts {
		err := opt(config)
		if err != nil {
			return nil, err
		}
	}

	compatibleWithOldEnvs(len(opts) > 0, config)

	// Ensure WorkdirParent is an absolute path so that operations in `act/common/git` work consistently.
	absWorkdirParent, err := filepath.Abs(config.Host.WorkdirParent)
	if err != nil {
		return nil, fmt.Errorf("cannot convert %q into absolute path: %w", config.Host.WorkdirParent, err)
	}
	config.Host.WorkdirParent = absWorkdirParent

	var parsedDefaultLabels labels.Labels
	for _, label := range config.Runner.DefaultLabels {
		parsedLabel, err := labels.Parse(label)
		if err != nil {
			return nil, fmt.Errorf("malformed `label` %q: %w", label, err)
		}
		parsedDefaultLabels = append(parsedDefaultLabels, parsedLabel)
	}

	// Apply default values to each server connection if they weren't populated by `opts`:
	for name, conn := range config.Server.Connections {
		if conn.FetchInterval == 0 {
			conn.FetchInterval = 2 * time.Second
		}
		if strings.HasSuffix(conn.URL.Hostname(), "codeberg.org") && conn.FetchInterval < 30*time.Second {
			log.Infof("Fetch interval for connection %s has been increased to the minimum of 30 seconds for Codeberg", name)
			conn.FetchInterval = 30 * time.Second
		}
		if len(parsedDefaultLabels) > 0 && (len(conn.Labels) == 0 || conn.labelPriority == overrideIfPossible) {
			conn.Labels = parsedDefaultLabels
		}
	}

	return config, nil
}

// FromFile reads settings from a configuration file and applies them to an existing Config instance.
func FromFile(path string) Option {
	return func(config *Config) error {
		if path == "" {
			log.Info("No configuration file specified; using default settings.")
			return nil
		}

		var readConfiguration serializedConfiguration
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("cannot open config file %q: %q", path, err)
		}
		if err := yaml.Unmarshal(content, &readConfiguration); err != nil {
			return fmt.Errorf("cannot parse config file %q: %w", path, err)
		}

		return readConfiguration.applyTo(config)
	}
}

func readEnvFile(path string) (map[string]string, error) {
	stat, err := os.Stat(path)
	if err != nil {
		log.Warnf("Missing env file %q is ignored", path)
		return map[string]string{}, nil
	}
	if stat.IsDir() {
		return map[string]string{}, fmt.Errorf("env file is a directory: %q", path)
	}

	env, err := godotenv.Read(path)
	if err != nil {
		return map[string]string{}, fmt.Errorf("could not read env file %q: %w", path, err)
	}

	return env, nil
}

func ResolveSecretURL(input string) (string, error) {
	// We're not using url.Parse() for identifying the secret's scheme because, depending on the scheme, reparsing might
	// be necessary.
	if strings.HasPrefix(input, "file:") {
		return resolveFileSecret(input)
	}

	return "", fmt.Errorf("unsupported secret URL: %q", input)
}

func resolveFileSecret(input string) (string, error) {
	// Replace placeholder `$CREDENTIALS_DIRECTORY` with the value of the environment variable `CREDENTIALS_DIRECTORY`
	// if it exists. That adds support for systemd Credentials (https://systemd.io/CREDENTIALS/).
	if credentialsDirectory, ok := os.LookupEnv("CREDENTIALS_DIRECTORY"); ok {
		input = strings.Replace(input, "$CREDENTIALS_DIRECTORY", credentialsDirectory, 1)
	}

	fileURL, err := url.Parse(input)
	if err != nil {
		return "", fmt.Errorf("malformed secret URL %q: %w", input, err)
	}

	hostname := fileURL.Hostname()
	if hostname != "" {
		log.Warnf("Ignoring hostname %q in secret: %q", hostname, input)
		fileURL.Host = ""
	}

	filePath, _ := fileuri.ToFilePath(fileURL)
	value, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("cannot read secret %q: %w", input, err)
	}

	return string(value), nil
}

var validTokenPattern = regexp.MustCompile("(?i)^[a-z0-9]*$")

// IsValidToken tests whether the given string does not contain characters that are not allowed in a runner token.
func IsValidToken(str string) bool {
	return validTokenPattern.MatchString(str)
}
