// Copyright 2024 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"code.forgejo.org/forgejo/runner/v12/internal/pkg/labels"
	gouuid "github.com/google/uuid"
	"github.com/powerman/fileuri"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gotest.tools/v3/skip"
)

func TestNew(t *testing.T) {
	t.Run("Missing configuration file results in error", func(t *testing.T) {
		config, err := New(FromFile("does-not-exist"))

		assert.Nil(t, config)
		assert.ErrorContains(t, err, "open does-not-exist:")
	})

	t.Run("Malformed configuration file results in error", func(t *testing.T) {
		tempDir := t.TempDir()

		configPath := filepath.Join(tempDir, "config.yaml")

		err := os.WriteFile(configPath, []byte("malformed"), 0o644)
		require.NoError(t, err)

		config, err := New(FromFile(configPath))

		assert.Nil(t, config)
		assert.ErrorContains(t, err, fmt.Sprintf(`cannot parse config file %q`, configPath))
	})

	t.Run("Without configuration file", func(t *testing.T) {
		config, err := New()
		require.NoError(t, err)

		home, err := os.UserHomeDir()
		require.NoError(t, err)

		assert.Equal(t, 7, reflect.TypeOf(Config{}).NumField())
		assert.Equal(t, 2, reflect.TypeOf(Log{}).NumField())
		assert.Equal(t, 11, reflect.TypeOf(Runner{}).NumField())
		assert.Equal(t, 8, reflect.TypeOf(Cache{}).NumField())
		assert.Equal(t, 9, reflect.TypeOf(Container{}).NumField())
		assert.Equal(t, 1, reflect.TypeOf(Host{}).NumField())
		assert.Equal(t, 3, reflect.TypeOf(Kubernetes{}).NumField())
		assert.Equal(t, 1, reflect.TypeOf(Server{}).NumField())

		assert.Equal(t, "info", config.Log.Level)
		assert.Equal(t, "info", config.Log.JobLevel)

		assert.Equal(t, ".runner", config.Runner.File)
		assert.Equal(t, 1, config.Runner.Capacity)
		assert.Empty(t, config.Runner.Envs)
		assert.Empty(t, config.Runner.EnvFile)
		assert.Equal(t, 3*time.Hour, config.Runner.Timeout)
		assert.Zero(t, config.Runner.ShutdownTimeout)
		assert.False(t, config.Runner.Insecure)
		assert.Equal(t, 30*time.Second, config.Runner.FetchTimeout)
		assert.Equal(t, 1*time.Second, config.Runner.ReportInterval)
		assert.Empty(t, config.Runner.DefaultLabels)
		assert.Equal(t, uint(10), config.Runner.ReportRetry.MaxRetries)
		assert.Equal(t, 100*time.Millisecond, config.Runner.ReportRetry.InitialDelay)
		assert.Zero(t, config.Runner.ReportRetry.MaxDelay)

		assert.True(t, config.Cache.Enabled)
		assert.Equal(t, filepath.Join(home, ".cache", "actcache"), config.Cache.Dir)
		assert.Empty(t, config.Cache.Host)
		assert.Zero(t, config.Cache.Port)
		assert.Zero(t, config.Cache.ProxyPort)
		assert.Empty(t, config.Cache.ExternalServer)
		assert.Empty(t, config.Cache.ActionsCacheURLOverride)
		assert.Empty(t, config.Cache.Secret)

		assert.Empty(t, config.Container.Network)
		assert.False(t, config.Container.EnableIPv6)
		assert.False(t, config.Container.Privileged)
		assert.Empty(t, config.Container.Options)
		assert.Equal(t, "workspace", config.Container.WorkdirParent)
		assert.Empty(t, config.Container.ValidVolumes)
		assert.Equal(t, "-", config.Container.DockerHost)
		assert.False(t, config.Container.ForcePull)
		assert.False(t, config.Container.ForceRebuild)

		assert.Equal(t, filepath.Join(home, ".cache", "act"), config.Host.WorkdirParent)
		assert.True(t, filepath.IsAbs(config.Host.WorkdirParent))

		assert.Equal(t, "default", config.Kubernetes.Namespace)
		assert.Empty(t, config.Kubernetes.KubeConfig)
		assert.Equal(t, 10*time.Minute, config.Kubernetes.PollTimeout)

		assert.Empty(t, config.Server.Connections)
	})

	t.Run("Defaults retained if configuration file is empty", func(t *testing.T) {
		tempDir := t.TempDir()

		configPath := filepath.Join(tempDir, "config.yaml")

		err := os.WriteFile(configPath, []byte(""), 0o644)
		require.NoError(t, err)

		config, err := New(FromFile(configPath))
		require.NoError(t, err)

		home, err := os.UserHomeDir()
		require.NoError(t, err)

		assert.Equal(t, 7, reflect.TypeOf(Config{}).NumField())
		assert.Equal(t, 2, reflect.TypeOf(Log{}).NumField())
		assert.Equal(t, 11, reflect.TypeOf(Runner{}).NumField())
		assert.Equal(t, 8, reflect.TypeOf(Cache{}).NumField())
		assert.Equal(t, 9, reflect.TypeOf(Container{}).NumField())
		assert.Equal(t, 1, reflect.TypeOf(Host{}).NumField())
		assert.Equal(t, 3, reflect.TypeOf(Kubernetes{}).NumField())
		assert.Equal(t, 1, reflect.TypeOf(Server{}).NumField())

		assert.Equal(t, "info", config.Log.Level)
		assert.Equal(t, "info", config.Log.JobLevel)

		assert.Equal(t, ".runner", config.Runner.File)
		assert.Equal(t, 1, config.Runner.Capacity)
		assert.Empty(t, config.Runner.Envs)
		assert.Empty(t, config.Runner.EnvFile)
		assert.Equal(t, 3*time.Hour, config.Runner.Timeout)
		assert.Zero(t, config.Runner.ShutdownTimeout)
		assert.False(t, config.Runner.Insecure)
		assert.Equal(t, 30*time.Second, config.Runner.FetchTimeout)
		assert.Equal(t, 1*time.Second, config.Runner.ReportInterval)
		assert.Empty(t, config.Runner.DefaultLabels)
		assert.Equal(t, uint(10), config.Runner.ReportRetry.MaxRetries)
		assert.Equal(t, 100*time.Millisecond, config.Runner.ReportRetry.InitialDelay)
		assert.Zero(t, config.Runner.ReportRetry.MaxDelay)

		assert.True(t, config.Cache.Enabled)
		assert.Equal(t, filepath.Join(home, ".cache", "actcache"), config.Cache.Dir)
		assert.Empty(t, config.Cache.Host)
		assert.Zero(t, config.Cache.Port)
		assert.Zero(t, config.Cache.ProxyPort)
		assert.Empty(t, config.Cache.ExternalServer)
		assert.Empty(t, config.Cache.ActionsCacheURLOverride)
		assert.Empty(t, config.Cache.Secret)

		assert.Empty(t, config.Container.Network)
		assert.False(t, config.Container.EnableIPv6)
		assert.False(t, config.Container.Privileged)
		assert.Empty(t, config.Container.Options)
		assert.Equal(t, "workspace", config.Container.WorkdirParent)
		assert.Empty(t, config.Container.ValidVolumes)
		assert.Equal(t, "-", config.Container.DockerHost)
		assert.False(t, config.Container.ForcePull)
		assert.False(t, config.Container.ForceRebuild)

		assert.Equal(t, filepath.Join(home, ".cache", "act"), config.Host.WorkdirParent)
		assert.True(t, filepath.IsAbs(config.Host.WorkdirParent))

		assert.Empty(t, config.Server.Connections)
	})

	t.Run("Connection defaults configured", func(t *testing.T) {
		rawConfig := `
server:
  connections:
    example:
      url: https://example.com/
      uuid: 7f7695df-a064-4c70-a597-56714e851e2c
      token: LxV7RrjXd
      labels: ["docker:docker://node:current-bookworm"]
    codeberg:
      url: https://codeberg.org/
      uuid: 33580597-5122-46f7-b997-ecc1e8ee8ffa
      token: LxV7RrjXd
      labels: ["docker:docker://node:current-bookworm"]
`

		tempDir := t.TempDir()

		configPath := filepath.Join(tempDir, "config.yml")
		err := os.WriteFile(configPath, []byte(rawConfig), 0o644)
		require.NoError(t, err)

		config, err := New(FromFile(configPath))
		require.NoError(t, err)

		require.Len(t, config.Server.Connections, 2)

		c, ok := config.Server.Connections["example"]
		require.True(t, ok)
		assert.Equal(t, 2*time.Second, c.FetchInterval)

		c, ok = config.Server.Connections["codeberg"]
		require.True(t, ok)
		assert.Equal(t, 30*time.Second, c.FetchInterval)
	})

	t.Run("Global connection overrides", func(t *testing.T) {
		rawConfig := `
runner:
  fetch_interval: 14s
  labels: ["docker:docker://node:current-bookworm"]
server:
  connections:
    example:
      url: https://example.com/
      uuid: 7f7695df-a064-4c70-a597-56714e851e2c
      token: LxV7RrjXd
`

		tempDir := t.TempDir()

		configPath := filepath.Join(tempDir, "config.yml")
		err := os.WriteFile(configPath, []byte(rawConfig), 0o644)
		require.NoError(t, err)

		config, err := New(FromFile(configPath))
		require.NoError(t, err)

		require.Len(t, config.Server.Connections, 1)
		c, ok := config.Server.Connections["example"]
		require.True(t, ok)
		assert.Equal(t, 14*time.Second, c.FetchInterval)
		assert.Len(t, c.Labels, 1)
	})

	t.Run("Configuration file takes precedence over defaults", func(t *testing.T) {
		rawConfig := `
log:
  level: warn
  job_level: warn
runner:
  file: runner.txt
  capacity: 37
  envs:
    MY_VARIABLE: value
  env_file: .env
  timeout: 6h
  shutdown_timeout: 30s
  insecure: true
  fetch_timeout: 25s
  fetch_interval: 8s
  report_interval: 5s
  labels:
    - docker:docker://node:24-trixie
  report_retry:
    max_retries: 26
    initial_delay: 600ms
    max_delay: 975s
cache:
  enabled: false
  dir: some/directory
  host: https://example.com/
  port: 8080
  proxy_port: 8081
  external_server: https://external.example.com/
  actions_cache_url_override: https://override.example.com/
  secret: vruvRdu5Rm
  secret_url:
container:
  network: host
  network_mode: bridge
  enable_ipv6: true
  privileged: true
  options: "--hostname=runner"
  workdir_parent: "a/workdir/parent"
  valid_volumes:
    - /etc/ssl/certs
  docker_host: tcp://10.10.10.10/
  force_pull: true
  force_rebuild: true
host:
  workdir_parent: some/path
kubernetes:
  namespace: ci-jobs
  kubeconfig: /etc/k8s/config
  poll_timeout: 5m
server:
  connections:
    example:
      url: https://example.com/
      uuid: 7f7695df-a064-4c70-a597-56714e851e2c
      token: LxV7RrjXd
      token_url:
      fetch_interval: 8s
      labels:
        - debian:docker://node:24-trixie
`

		tempDir := t.TempDir()

		configPath := filepath.Join(tempDir, "config.yml")
		err := os.WriteFile(configPath, []byte(rawConfig), 0o644)
		require.NoError(t, err)

		defaultConfig, err := New()
		require.NoError(t, err)

		config, err := New(FromFile(configPath))
		require.NoError(t, err)

		assert.Equal(t, 7, reflect.TypeOf(Config{}).NumField())
		assert.Equal(t, 2, reflect.TypeOf(Log{}).NumField())
		assert.Equal(t, 11, reflect.TypeOf(Runner{}).NumField())
		assert.Equal(t, 8, reflect.TypeOf(Cache{}).NumField())
		assert.Equal(t, 9, reflect.TypeOf(Container{}).NumField())
		assert.Equal(t, 1, reflect.TypeOf(Host{}).NumField())
		assert.Equal(t, 3, reflect.TypeOf(Kubernetes{}).NumField())
		assert.Equal(t, 1, reflect.TypeOf(Server{}).NumField())

		// Verify that each value loaded from the configuration file does not match the default configuration.
		// Otherwise, the test would be meaningless.
		assert.NotEqual(t, defaultConfig.Log.Level, config.Log.Level)
		assert.Equal(t, "warn", config.Log.Level)
		assert.NotEqual(t, defaultConfig.Log.JobLevel, config.Log.JobLevel)
		assert.Equal(t, "warn", config.Log.JobLevel)

		assert.NotEqual(t, defaultConfig.Runner.File, config.Runner.File)
		assert.Equal(t, "runner.txt", config.Runner.File)
		assert.NotEqual(t, defaultConfig.Runner.Capacity, config.Runner.Capacity)
		assert.Equal(t, 37, config.Runner.Capacity)
		assert.NotEqual(t, defaultConfig.Runner.Envs, config.Runner.Envs)
		assert.Equal(t, map[string]string{"MY_VARIABLE": "value"}, config.Runner.Envs)
		assert.NotEqual(t, defaultConfig.Runner.EnvFile, config.Runner.EnvFile)
		assert.Equal(t, ".env", config.Runner.EnvFile)
		assert.NotEqual(t, defaultConfig.Runner.Timeout, config.Runner.Timeout)
		assert.Equal(t, 6*time.Hour, config.Runner.Timeout)
		assert.NotEqual(t, defaultConfig.Runner.ShutdownTimeout, config.Runner.ShutdownTimeout)
		assert.Equal(t, 30*time.Second, config.Runner.ShutdownTimeout)
		assert.NotEqual(t, defaultConfig.Runner.Insecure, config.Runner.Insecure)
		assert.True(t, config.Runner.Insecure)
		assert.NotEqual(t, defaultConfig.Runner.FetchTimeout, config.Runner.FetchTimeout)
		assert.Equal(t, 25*time.Second, config.Runner.FetchTimeout)
		assert.NotEqual(t, defaultConfig.Runner.ReportInterval, config.Runner.ReportInterval)
		assert.Equal(t, 5*time.Second, config.Runner.ReportInterval)
		assert.NotEqual(t, defaultConfig.Runner.DefaultLabels, config.Runner.DefaultLabels)
		assert.Equal(t, []string{"docker:docker://node:24-trixie"}, config.Runner.DefaultLabels)
		assert.NotEqual(t, defaultConfig.Runner.ReportRetry.MaxRetries, config.Runner.ReportRetry.MaxRetries)
		assert.Equal(t, uint(26), config.Runner.ReportRetry.MaxRetries)
		assert.NotEqual(t, defaultConfig.Runner.ReportRetry.InitialDelay, config.Runner.ReportRetry.InitialDelay)
		assert.Equal(t, 600*time.Millisecond, config.Runner.ReportRetry.InitialDelay)
		assert.NotEqual(t, defaultConfig.Runner.ReportRetry.MaxDelay, config.Runner.ReportRetry.MaxDelay)
		assert.Equal(t, 975*time.Second, config.Runner.ReportRetry.MaxDelay)

		assert.NotEqual(t, defaultConfig.Cache.Enabled, config.Cache.Enabled)
		assert.False(t, config.Cache.Enabled)
		assert.NotEqual(t, defaultConfig.Cache.Dir, config.Cache.Dir)
		assert.Equal(t, "some/directory", config.Cache.Dir)
		assert.NotEqual(t, defaultConfig.Cache.Host, config.Cache.Host)
		assert.Equal(t, "https://example.com/", config.Cache.Host)
		assert.NotEqual(t, defaultConfig.Cache.Port, config.Cache.Port)
		assert.Equal(t, uint16(8080), config.Cache.Port)
		assert.NotEqual(t, defaultConfig.Cache.ProxyPort, config.Cache.ProxyPort)
		assert.Equal(t, uint16(8081), config.Cache.ProxyPort)
		assert.NotEqual(t, defaultConfig.Cache.ExternalServer, config.Cache.ExternalServer)
		assert.Equal(t, "https://external.example.com/", config.Cache.ExternalServer)
		assert.NotEqual(t, defaultConfig.Cache.ActionsCacheURLOverride, config.Cache.ActionsCacheURLOverride)
		assert.Equal(t, "https://override.example.com/", config.Cache.ActionsCacheURLOverride)
		assert.NotEqual(t, defaultConfig.Cache.Secret, config.Cache.Secret)
		assert.Equal(t, "vruvRdu5Rm", config.Cache.Secret)

		assert.NotEqual(t, defaultConfig.Container.Network, config.Container.Network)
		assert.Equal(t, "host", config.Container.Network)
		assert.NotEqual(t, defaultConfig.Container.EnableIPv6, config.Container.EnableIPv6)
		assert.True(t, config.Container.EnableIPv6)
		assert.NotEqual(t, defaultConfig.Container.Privileged, config.Container.Privileged)
		assert.True(t, config.Container.Privileged)
		assert.NotEqual(t, defaultConfig.Container.Options, config.Container.Options)
		assert.Equal(t, "--hostname=runner", config.Container.Options)
		assert.NotEqual(t, defaultConfig.Container.WorkdirParent, config.Container.WorkdirParent)
		assert.Equal(t, "a/workdir/parent", config.Container.WorkdirParent)
		assert.NotEqual(t, defaultConfig.Container.ValidVolumes, config.Container.ValidVolumes)
		assert.Equal(t, []string{"/etc/ssl/certs"}, config.Container.ValidVolumes)
		assert.NotEqual(t, defaultConfig.Container.DockerHost, config.Container.DockerHost)
		assert.Equal(t, "tcp://10.10.10.10/", config.Container.DockerHost)
		assert.NotEqual(t, defaultConfig.Container.ForcePull, config.Container.ForcePull)
		assert.True(t, config.Container.ForcePull)
		assert.NotEqual(t, defaultConfig.Container.ForceRebuild, config.Container.ForceRebuild)
		assert.True(t, config.Container.ForceRebuild)

		assert.NotEqual(t, defaultConfig.Host.WorkdirParent, config.Host.WorkdirParent)
		assert.True(t, strings.HasSuffix(config.Host.WorkdirParent, filepath.FromSlash("some/path")))
		assert.True(t, filepath.IsAbs(config.Host.WorkdirParent))

		assert.NotEqual(t, defaultConfig.Kubernetes.Namespace, config.Kubernetes.Namespace)
		assert.Equal(t, "ci-jobs", config.Kubernetes.Namespace)
		assert.NotEqual(t, defaultConfig.Kubernetes.KubeConfig, config.Kubernetes.KubeConfig)
		assert.Equal(t, "/etc/k8s/config", config.Kubernetes.KubeConfig)
		assert.NotEqual(t, defaultConfig.Kubernetes.PollTimeout, config.Kubernetes.PollTimeout)
		assert.Equal(t, 5*time.Minute, config.Kubernetes.PollTimeout)

		assert.NotEqual(t, len(defaultConfig.Server.Connections), len(config.Server.Connections))
		assert.Len(t, config.Server.Connections, 1)
		assert.Equal(t, "https://example.com/", config.Server.Connections["example"].URL.String())
		assert.Equal(t, "7f7695df-a064-4c70-a597-56714e851e2c", config.Server.Connections["example"].UUID.String())
		assert.Equal(t, "LxV7RrjXd", config.Server.Connections["example"].Token)
		assert.Equal(t, 8*time.Second, config.Server.Connections["example"].FetchInterval)
		assert.Len(t, config.Server.Connections["example"].Labels, 1)
		assert.Equal(t, labels.MustParse("debian:docker://node:24-trixie"), config.Server.Connections["example"].Labels[0])
	})

	t.Run("Imports optional env file configured with os-native path", func(t *testing.T) {
		tempDir := t.TempDir()
		configFile := filepath.Join(tempDir, "config.yaml")
		envFile := filepath.Join(tempDir, ".env")

		rawConfig := fmt.Sprintf(`{ runner: { env_file: %q } }`, envFile)
		err := os.WriteFile(configFile, []byte(rawConfig), 0o644)
		require.NoError(t, err)

		err = os.WriteFile(envFile, []byte("SOME_ENV_VAR=some-value"), 0o644)
		require.NoError(t, err)

		config, err := New(FromFile(configFile))
		require.NoError(t, err)

		assert.Equal(t, envFile, config.Runner.EnvFile)
		assert.Equal(t, map[string]string{"SOME_ENV_VAR": "some-value"}, config.Runner.Envs)
	})

	t.Run("Imports optional env file configured with UNIX path", func(t *testing.T) {
		tempDir := t.TempDir()
		configFile := filepath.Join(tempDir, "config.yaml")
		envFile := filepath.ToSlash(filepath.Join(tempDir, ".env"))

		rawConfig := fmt.Sprintf(`{ runner: { env_file: %q } }`, envFile)
		err := os.WriteFile(configFile, []byte(rawConfig), 0o644)
		require.NoError(t, err)

		err = os.WriteFile(envFile, []byte("SOME_ENV_VAR=some-value"), 0o644)
		require.NoError(t, err)

		config, err := New(FromFile(configFile))
		require.NoError(t, err)

		assert.Equal(t, envFile, config.Runner.EnvFile)
		assert.Equal(t, map[string]string{"SOME_ENV_VAR": "some-value"}, config.Runner.Envs)
	})

	t.Run("Env file merged with envs", func(t *testing.T) {
		tempDir := t.TempDir()
		configFile := filepath.Join(tempDir, "config.yaml")
		envFile := filepath.Join(tempDir, ".env")

		rawConfig := fmt.Sprintf(`
runner:
  envs:
    MY_VARIABLE: value
  env_file: %q
`, envFile)
		err := os.WriteFile(configFile, []byte(rawConfig), 0o644)
		require.NoError(t, err)

		err = os.WriteFile(envFile, []byte("SOME_ENV_VAR=some-value"), 0o644)
		require.NoError(t, err)

		config, err := New(FromFile(configFile))
		require.NoError(t, err)

		assert.Equal(t, envFile, config.Runner.EnvFile)
		assert.Equal(t, map[string]string{"MY_VARIABLE": "value", "SOME_ENV_VAR": "some-value"}, config.Runner.Envs)
	})

	t.Run("Ignores missing env file", func(t *testing.T) {
		tempDir := t.TempDir()
		configFile := filepath.Join(tempDir, "config.yaml")
		envFile := filepath.Join(tempDir, ".env")

		rawConfig := fmt.Sprintf(`{ runner: { env_file: %q } }`, envFile)
		err := os.WriteFile(configFile, []byte(rawConfig), 0o644)
		require.NoError(t, err)

		config, err := New(FromFile(configFile))
		require.NoError(t, err)

		assert.Equal(t, envFile, config.Runner.EnvFile)
		assert.Empty(t, config.Runner.Envs)
	})

	t.Run("Malformed env file results in error", func(t *testing.T) {
		tempDir := t.TempDir()
		configFile := filepath.Join(tempDir, "config.yaml")
		envFile := filepath.Join(tempDir, ".env")

		rawConfig := fmt.Sprintf(`{ runner: { env_file: %q } }`, envFile)
		err := os.WriteFile(configFile, []byte(rawConfig), 0o644)
		require.NoError(t, err)

		err = os.WriteFile(envFile, []byte("very/malformed"), 0o644)
		require.NoError(t, err)

		config, err := New(FromFile(configFile))

		assert.Nil(t, config)
		assert.ErrorContains(t, err, "could not read env file")
	})

	t.Run(".runner label overrides", func(t *testing.T) {
		rawConfig := `
runner:
  fetch_interval: 14s
  labels: ["docker:docker://node:current-bookworm"]
`
		tempDir := t.TempDir()

		configPath := filepath.Join(tempDir, "config.yml")
		err := os.WriteFile(configPath, []byte(rawConfig), 0o644)
		require.NoError(t, err)

		config, err := New(
			FromFile(configPath),
			func(config *Config) error {
				if config.Server.Connections == nil {
					config.Server.Connections = make(map[string]*Connection)
				}
				parsedURL, err := url.ParseRequestURI("https://example.com")
				require.NoError(t, err)
				config.Server.Connections["runner"] = &Connection{
					URL:           parsedURL,
					UUID:          gouuid.New(),
					Token:         "token-goes-here",
					Labels:        []*labels.Label{labels.MustParse("lxc:lxc://debian:bookworm")},
					labelPriority: overrideIfPossible, // indicates that this label should be overridden
				}
				return nil
			},
		)
		require.NoError(t, err)

		require.Len(t, config.Server.Connections, 1)
		c, ok := config.Server.Connections["runner"]
		require.True(t, ok)
		require.Len(t, c.Labels, 1)
		assert.Equal(t, c.Labels[0].String(), "docker:docker://node:current-bookworm")
	})
}

func TestSerializedLogSettings_applyTo(t *testing.T) {
	t.Run("accepts valid settings", func(t *testing.T) {
		expected := Log{
			Level:    "warn",
			JobLevel: "error",
		}

		settings := serializedLogSettings{
			Level:    "warn",
			JobLevel: "error",
		}

		config := Config{}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, expected, config.Log)
	})
	t.Run("rejects invalid level", func(t *testing.T) {
		settings := serializedLogSettings{
			Level:    "invalid",
			JobLevel: "error",
		}

		config := Config{}
		err := settings.applyTo(&config)

		assert.ErrorContains(t, err, "invalid `level` \"invalid\"")
	})
	t.Run("rejects invalid job_level", func(t *testing.T) {
		settings := serializedLogSettings{
			Level:    "error",
			JobLevel: "invalid",
		}

		config := Config{}
		err := settings.applyTo(&config)

		assert.ErrorContains(t, err, "invalid `job_level` \"invalid\"")
	})
}

func TestSerializedRunnerSettings_applyTo(t *testing.T) {
	t.Run("accepts valid settings without env file", func(t *testing.T) {
		expected := Runner{
			File:            ".my_runner",
			Capacity:        762,
			Envs:            map[string]string{"ABC": "def"},
			EnvFile:         "",
			Timeout:         892 * time.Second,
			ShutdownTimeout: 878 * time.Second,
			Insecure:        true,
			FetchTimeout:    299 * time.Second,
			ReportInterval:  868 * time.Second,
			DefaultLabels:   []string{"label-1", "label-2"},
			ReportRetry:     Retry{},
		}

		settings := serializedRunnerSettings{
			File:            ".my_runner",
			Capacity:        762,
			Envs:            map[string]string{"ABC": "def"},
			EnvFile:         "",
			Timeout:         892 * time.Second,
			ShutdownTimeout: 878 * time.Second,
			Insecure:        true,
			FetchTimeout:    299 * time.Second,
			ReportInterval:  868 * time.Second,
			Labels:          []string{"label-1", "label-2"},
			ReportRetry:     serializedReportRetrySettings{},
		}

		config := Config{}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, expected, config.Runner)
	})

	t.Run("accepts valid settings with env file", func(t *testing.T) {
		tempDir := t.TempDir()
		envPath := filepath.Join(tempDir, "env-file")

		err := os.WriteFile(envPath, []byte("B=99\nC=3"), 0o644)
		require.NoError(t, err)

		expected := Runner{
			File:            ".my_runner",
			Capacity:        762,
			Envs:            map[string]string{"A": "1", "B": "99", "C": "3"},
			EnvFile:         envPath,
			Timeout:         892 * time.Second,
			ShutdownTimeout: 878 * time.Second,
			Insecure:        true,
			FetchTimeout:    299 * time.Second,
			ReportInterval:  868 * time.Second,
			DefaultLabels:   []string{"label-1", "label-2"},
			ReportRetry:     Retry{},
		}

		settings := serializedRunnerSettings{
			File:            ".my_runner",
			Capacity:        762,
			Envs:            map[string]string{"A": "1", "B": "2"},
			EnvFile:         envPath,
			Timeout:         892 * time.Second,
			ShutdownTimeout: 878 * time.Second,
			Insecure:        true,
			FetchTimeout:    299 * time.Second,
			FetchInterval:   820 * time.Second,
			ReportInterval:  868 * time.Second,
			Labels:          []string{"label-1", "label-2"},
			ReportRetry:     serializedReportRetrySettings{},
		}

		config := Config{}
		err = settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, expected, config.Runner)
	})

	t.Run("ignores missing env file", func(t *testing.T) {
		expected := Runner{
			File:            ".my_runner",
			Capacity:        762,
			Envs:            map[string]string{"A": "1", "B": "2"},
			EnvFile:         "/does/not/exist",
			Timeout:         892 * time.Second,
			ShutdownTimeout: 878 * time.Second,
			Insecure:        true,
			FetchTimeout:    299 * time.Second,
			ReportInterval:  868 * time.Second,
			DefaultLabels:   []string{"label-1", "label-2"},
			ReportRetry:     Retry{},
		}

		settings := serializedRunnerSettings{
			File:            ".my_runner",
			Capacity:        762,
			Envs:            map[string]string{"A": "1", "B": "2"},
			EnvFile:         "/does/not/exist",
			Timeout:         892 * time.Second,
			ShutdownTimeout: 878 * time.Second,
			Insecure:        true,
			FetchTimeout:    299 * time.Second,
			FetchInterval:   820 * time.Second,
			ReportInterval:  868 * time.Second,
			Labels:          []string{"label-1", "label-2"},
			ReportRetry:     serializedReportRetrySettings{},
		}

		config := Config{}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, expected, config.Runner)
	})

	t.Run("ignores invalid capacity", func(t *testing.T) {
		settings := serializedRunnerSettings{
			File:            ".my_runner",
			Capacity:        0,
			Envs:            map[string]string{"ABC": "def"},
			EnvFile:         "",
			Timeout:         892 * time.Second,
			ShutdownTimeout: 878 * time.Second,
			Insecure:        false,
			FetchTimeout:    299 * time.Second,
			FetchInterval:   820 * time.Second,
			ReportInterval:  868 * time.Second,
			Labels:          []string{"label-1", "label-2"},
			ReportRetry:     serializedReportRetrySettings{},
		}

		config := Config{Runner: Runner{Capacity: 1}}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, 1, config.Runner.Capacity)
	})

	t.Run("ignores invalid timeout", func(t *testing.T) {
		settings := serializedRunnerSettings{
			File:            ".my_runner",
			Capacity:        370,
			Envs:            map[string]string{"ABC": "def"},
			EnvFile:         "",
			Timeout:         0 * time.Second,
			ShutdownTimeout: 878 * time.Second,
			Insecure:        false,
			FetchTimeout:    299 * time.Second,
			FetchInterval:   820 * time.Second,
			ReportInterval:  868 * time.Second,
			Labels:          []string{"label-1", "label-2"},
			ReportRetry:     serializedReportRetrySettings{},
		}

		config := Config{Runner: Runner{Timeout: time.Second}}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, time.Second, config.Runner.Timeout)
	})

	t.Run("ignores invalid shutdown_timeout", func(t *testing.T) {
		settings := serializedRunnerSettings{
			File:            ".my_runner",
			Capacity:        370,
			Envs:            map[string]string{"ABC": "def"},
			EnvFile:         "",
			Timeout:         588 * time.Second,
			ShutdownTimeout: -1 * time.Second,
			Insecure:        false,
			FetchTimeout:    299 * time.Second,
			FetchInterval:   820 * time.Second,
			ReportInterval:  868 * time.Second,
			Labels:          []string{"label-1", "label-2"},
			ReportRetry:     serializedReportRetrySettings{},
		}

		config := Config{Runner: Runner{ShutdownTimeout: 0}}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, 0*time.Second, config.Runner.ShutdownTimeout)
	})

	t.Run("ignores invalid fetch_timeout", func(t *testing.T) {
		settings := serializedRunnerSettings{
			File:            ".my_runner",
			Capacity:        370,
			Envs:            map[string]string{"ABC": "def"},
			EnvFile:         "",
			Timeout:         588 * time.Second,
			ShutdownTimeout: 0 * time.Second,
			Insecure:        false,
			FetchTimeout:    -1 * time.Second,
			FetchInterval:   820 * time.Second,
			ReportInterval:  868 * time.Second,
			Labels:          []string{"label-1", "label-2"},
			ReportRetry:     serializedReportRetrySettings{},
		}

		config := Config{Runner: Runner{FetchTimeout: 3 * time.Second}}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, 3*time.Second, config.Runner.FetchTimeout)
	})

	t.Run("ignores invalid fetch_interval", func(t *testing.T) {
		settings := serializedRunnerSettings{
			File:            ".my_runner",
			Capacity:        370,
			Envs:            map[string]string{"ABC": "def"},
			EnvFile:         "",
			Timeout:         588 * time.Second,
			ShutdownTimeout: 0 * time.Second,
			Insecure:        false,
			FetchTimeout:    939 * time.Second,
			FetchInterval:   -1 * time.Second,
			ReportInterval:  868 * time.Second,
			Labels:          []string{"label-1", "label-2"},
			ReportRetry:     serializedReportRetrySettings{},
		}

		config := Config{Server: Server{Connections: map[string]*Connection{"default": {}}}}
		err := settings.applyGlobalDefaultsTo(&config)
		require.NoError(t, err)

		assert.Equal(t, 0*time.Second, config.Server.Connections["default"].FetchInterval)
	})

	t.Run("ignores invalid report_interval", func(t *testing.T) {
		settings := serializedRunnerSettings{
			File:            ".my_runner",
			Capacity:        370,
			Envs:            map[string]string{"ABC": "def"},
			EnvFile:         "",
			Timeout:         588 * time.Second,
			ShutdownTimeout: 0 * time.Second,
			Insecure:        false,
			FetchTimeout:    939 * time.Second,
			FetchInterval:   477 * time.Second,
			ReportInterval:  -1 * time.Second,
			Labels:          []string{"label-1", "label-2"},
			ReportRetry:     serializedReportRetrySettings{},
		}

		config := Config{Runner: Runner{ReportInterval: 0}}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, 0*time.Second, config.Runner.ReportInterval)
	})
}

func TestSerializedReportRetrySettings_applyTo(t *testing.T) {
	t.Run("accepts valid settings", func(t *testing.T) {
		expected := Retry{
			MaxRetries:   13,
			InitialDelay: 200 * time.Millisecond,
			MaxDelay:     30 * time.Second,
		}

		maxRetries := uint(13)
		initialDelay := 200 * time.Millisecond
		settings := serializedReportRetrySettings{
			MaxRetries:   &maxRetries,
			InitialDelay: &initialDelay,
			MaxDelay:     30 * time.Second,
		}

		config := Config{}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, expected, config.Runner.ReportRetry)
	})
	t.Run("ignores invalid max_retries", func(t *testing.T) {
		maxRetries := uint(0)
		initialDelay := 100 * time.Millisecond
		settings := serializedReportRetrySettings{
			MaxRetries:   &maxRetries,
			InitialDelay: &initialDelay,
			MaxDelay:     0,
		}

		config := Config{Runner: Runner{ReportRetry: Retry{MaxRetries: 10}}}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, uint(10), config.Runner.ReportRetry.MaxRetries)
	})
	t.Run("ignores invalid initial_delay", func(t *testing.T) {
		maxRetries := uint(10)
		initialDelay := 0 * time.Millisecond
		settings := serializedReportRetrySettings{
			MaxRetries:   &maxRetries,
			InitialDelay: &initialDelay,
			MaxDelay:     0,
		}

		config := Config{Runner: Runner{ReportRetry: Retry{InitialDelay: 100 * time.Millisecond}}}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, 100*time.Millisecond, config.Runner.ReportRetry.InitialDelay)
	})
	t.Run("ignores invalid max_delay", func(t *testing.T) {
		maxRetries := uint(10)
		initialDelay := 100 * time.Millisecond
		settings := serializedReportRetrySettings{
			MaxRetries:   &maxRetries,
			InitialDelay: &initialDelay,
			MaxDelay:     -1 * time.Second,
		}

		config := Config{Runner: Runner{ReportRetry: Retry{MaxDelay: 0}}}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, 0*time.Second, config.Runner.ReportRetry.MaxDelay)
	})
}

func TestSerializedCacheSettings(t *testing.T) {
	t.Run("accepts valid settings", func(t *testing.T) {
		expected := Cache{
			Enabled:                 true,
			Dir:                     "/path/to/cache",
			Host:                    "cache.local",
			Port:                    1234,
			ProxyPort:               5678,
			ExternalServer:          "external.local",
			ActionsCacheURLOverride: "https://example.com/",
			Secret:                  "jaJEV8OHlux",
		}

		booleanTrue := true
		settings := serializedCacheSettings{
			Enabled:                 &booleanTrue,
			Dir:                     "/path/to/cache",
			Host:                    "cache.local",
			Port:                    1234,
			ProxyPort:               5678,
			ExternalServer:          "external.local",
			ActionsCacheURLOverride: "https://example.com/",
			Secret:                  "jaJEV8OHlux",
		}

		config := Config{}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, expected, config.Cache)
	})

	t.Run("resolves secret_url", func(t *testing.T) {
		tempDir := t.TempDir()
		secretPath := filepath.Join(tempDir, "secret.txt")

		err := os.WriteFile(secretPath, []byte("y114lUUM"), 0o644)
		require.NoError(t, err)

		secretURL, err := fileuri.FromFilePath(secretPath)
		require.NoError(t, err)

		booleanTrue := true
		settings := serializedCacheSettings{
			Enabled:                 &booleanTrue,
			Dir:                     "/path/to/cache",
			Host:                    "cache.local",
			Port:                    1234,
			ProxyPort:               5678,
			ExternalServer:          "external.local",
			ActionsCacheURLOverride: "https://example.com/",
			Secret:                  "",
			SecretURL:               secretURL.String(),
		}

		config := Config{}
		err = settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, "y114lUUM", config.Cache.Secret)
	})

	t.Run("trims whitespace around secret", func(t *testing.T) {
		tempDir := t.TempDir()
		secretPath := filepath.Join(tempDir, "secret.txt")

		err := os.WriteFile(secretPath, []byte("y114lUUM\n\n"), 0o644)
		require.NoError(t, err)

		secretURL, err := fileuri.FromFilePath(secretPath)
		require.NoError(t, err)

		booleanTrue := true
		settings := serializedCacheSettings{
			Enabled:                 &booleanTrue,
			Dir:                     "/path/to/cache",
			Host:                    "cache.local",
			Port:                    1234,
			ProxyPort:               5678,
			ExternalServer:          "external.local",
			ActionsCacheURLOverride: "https://example.com/",
			Secret:                  "",
			SecretURL:               secretURL.String(),
		}

		config := Config{}
		err = settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, "y114lUUM", config.Cache.Secret)
	})

	t.Run("rejects mutually exclusive secret and secret_url", func(t *testing.T) {
		booleanFalse := true
		settings := serializedCacheSettings{
			Enabled:                 &booleanFalse,
			Dir:                     "/path/to/cache",
			Host:                    "cache.local",
			Port:                    1234,
			ProxyPort:               5678,
			ExternalServer:          "external.local",
			ActionsCacheURLOverride: "https://example.com/",
			Secret:                  "hCCOI4b",
			SecretURL:               "file:some-secret.txt",
		}

		err := settings.applyTo(&Config{})

		assert.ErrorContains(t, err, "`secret` and `secret_url` are mutually exclusive")
	})
}

func TestSerializedContainerSettings(t *testing.T) {
	t.Run("accepts valid settings", func(t *testing.T) {
		expected := Container{
			Network:       "host",
			EnableIPv6:    true,
			Privileged:    true,
			Options:       "--init",
			WorkdirParent: "/path/to/parent",
			ValidVolumes:  []string{"/tmp"},
			DockerHost:    "unix:///run/user/1000/podman/podman.sock",
			ForcePull:     true,
			ForceRebuild:  true,
		}

		settings := serializedContainerSettings{
			Network:       "host",
			NetworkMode:   "",
			EnableIPv6:    true,
			Privileged:    true,
			Options:       "--init",
			WorkdirParent: "/path/to/parent",
			ValidVolumes:  []string{"/tmp"},
			DockerHost:    "unix:///run/user/1000/podman/podman.sock",
			ForcePull:     true,
			ForceRebuild:  true,
		}

		config := Config{}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, expected, config.Container)
	})
	t.Run("translates network_mode bridge to empty network", func(t *testing.T) {
		expected := Container{
			Network:       "",
			EnableIPv6:    true,
			Privileged:    true,
			Options:       "--init",
			WorkdirParent: "/path/to/parent",
			ValidVolumes:  []string{"/tmp"},
			DockerHost:    "unix:///run/user/1000/podman/podman.sock",
			ForcePull:     true,
			ForceRebuild:  true,
		}

		settings := serializedContainerSettings{
			Network:       "",
			NetworkMode:   "bridge",
			EnableIPv6:    true,
			Privileged:    true,
			Options:       "--init",
			WorkdirParent: "/path/to/parent",
			ValidVolumes:  []string{"/tmp"},
			DockerHost:    "unix:///run/user/1000/podman/podman.sock",
			ForcePull:     true,
			ForceRebuild:  true,
		}

		config := Config{}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, expected, config.Container)
	})
	t.Run("skips empty workdir_parent", func(t *testing.T) {
		expected := Container{
			Network:       "host",
			EnableIPv6:    true,
			Privileged:    true,
			Options:       "--init",
			WorkdirParent: "/path/to/parent",
			ValidVolumes:  []string{"/tmp"},
			DockerHost:    "unix:///run/user/1000/podman/podman.sock",
			ForcePull:     true,
			ForceRebuild:  true,
		}

		settings := serializedContainerSettings{
			Network:       "host",
			NetworkMode:   "bridge",
			EnableIPv6:    true,
			Privileged:    true,
			Options:       "--init",
			WorkdirParent: "",
			ValidVolumes:  []string{"/tmp"},
			DockerHost:    "unix:///run/user/1000/podman/podman.sock",
			ForcePull:     true,
			ForceRebuild:  true,
		}

		config := Config{Container: Container{WorkdirParent: "/path/to/parent"}}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, expected, config.Container)
	})
	t.Run("skips empty docker_host", func(t *testing.T) {
		expected := Container{
			Network:       "host",
			EnableIPv6:    true,
			Privileged:    true,
			Options:       "--init",
			WorkdirParent: "/path/to/parent",
			ValidVolumes:  []string{"/tmp"},
			DockerHost:    "unix:///run/user/1000/podman/podman.sock",
			ForcePull:     true,
			ForceRebuild:  true,
		}

		settings := serializedContainerSettings{
			Network:       "host",
			NetworkMode:   "bridge",
			EnableIPv6:    true,
			Privileged:    true,
			Options:       "--init",
			WorkdirParent: "/path/to/parent",
			ValidVolumes:  []string{"/tmp"},
			DockerHost:    "",
			ForcePull:     true,
			ForceRebuild:  true,
		}

		config := Config{Container: Container{DockerHost: "unix:///run/user/1000/podman/podman.sock"}}
		err := settings.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, expected, config.Container)
	})
}

func TestSerializedServerSettings_applyTo(t *testing.T) {
	t.Run("accepts valid settings", func(t *testing.T) {
		serverURL, err := url.Parse("https://example.com/")
		require.NoError(t, err)

		expected := Server{
			Connections: map[string]*Connection{
				"example": {
					URL:    serverURL,
					UUID:   gouuid.MustParse("4f79bf07-38f6-44c8-bbc6-d6531134f16a"),
					Token:  "doERMgMiVz",
					Labels: labels.Labels{labels.MustParse("label-1"), labels.MustParse("label-2")},
				},
			},
		}

		serialized := serializedServerSettings{
			Connections: map[string]serializedConnectionSettings{
				"example": {
					URL:    serverURL.String(),
					UUID:   "4f79bf07-38f6-44c8-bbc6-d6531134f16a",
					Token:  "doERMgMiVz",
					Labels: []string{"label-1", "label-2"},
				},
			},
		}

		config := Config{}
		err = serialized.applyTo(&config)
		require.NoError(t, err)

		assert.Equal(t, expected, config.Server)
	})
}

func TestSerializedConnectionSettings_applyTo(t *testing.T) {
	t.Run("accepts valid settings", func(t *testing.T) {
		serverURL, err := url.Parse("https://example.com/")
		require.NoError(t, err)

		expected := Connection{
			URL:    serverURL,
			UUID:   gouuid.MustParse("4f79bf07-38f6-44c8-bbc6-d6531134f16a"),
			Token:  "doERMgMiVz",
			Labels: labels.Labels{labels.MustParse("label-1"), labels.MustParse("label-2")},
		}

		serialized := serializedConnectionSettings{
			URL:    serverURL.String(),
			UUID:   "4f79bf07-38f6-44c8-bbc6-d6531134f16a",
			Token:  "doERMgMiVz",
			Labels: []string{"label-1", "label-2"},
		}

		config := Config{}
		err = serialized.applyTo(&config, "example")
		require.NoError(t, err)

		assert.Len(t, config.Server.Connections, 1)
		assert.Equal(t, &expected, config.Server.Connections["example"])
	})

	t.Run("rejects missing url", func(t *testing.T) {
		serialized := serializedConnectionSettings{
			URL:    "",
			UUID:   "26a6db40-e86c-4751-ba2e-adcab9615ba7",
			Token:  "a7h4uCzbB6B",
			Labels: []string{"label-1"},
		}

		err := serialized.applyTo(&Config{}, "example")
		assert.ErrorContains(t, err, "`url` is empty")
	})

	t.Run("rejects url without scheme", func(t *testing.T) {
		serialized := serializedConnectionSettings{
			URL:    "www.example.com",
			UUID:   "26a6db40-e86c-4751-ba2e-adcab9615ba7",
			Token:  "a7h4uCzbB6B",
			Labels: []string{"label-1"},
		}

		err := serialized.applyTo(&Config{}, "example")
		assert.ErrorContains(t, err, "malformed `url` \"www.example.com\"")
	})

	t.Run("only accepts schemes http and https", func(t *testing.T) {
		serialized := serializedConnectionSettings{
			URL:    "file:///some/path",
			UUID:   "26a6db40-e86c-4751-ba2e-adcab9615ba7",
			Token:  "a7h4uCzbB6B",
			Labels: []string{"label-1"},
		}

		err := serialized.applyTo(&Config{}, "example")
		assert.ErrorContains(t, err, "invalid scheme in `url` \"file:///some/path\": only http and https supported")
	})

	t.Run("rejects empty uuid", func(t *testing.T) {
		serverURL, err := url.Parse("https://example.com/")
		require.NoError(t, err)

		serialized := serializedConnectionSettings{
			URL:    serverURL.String(),
			UUID:   "",
			Token:  "a7h4uCzbB6B",
			Labels: []string{"label-1"},
		}

		err = serialized.applyTo(&Config{}, "example")
		assert.ErrorContains(t, err, "`uuid` is empty")
	})

	t.Run("rejects malformed uuid", func(t *testing.T) {
		serverURL, err := url.Parse("https://example.com/")
		require.NoError(t, err)

		serialized := serializedConnectionSettings{
			URL:    serverURL.String(),
			UUID:   "very invalid",
			Token:  "a7h4uCzbB6B",
			Labels: []string{"label-1"},
		}

		err = serialized.applyTo(&Config{}, "example")
		assert.ErrorContains(t, err, "malformed `uuid` \"very invalid\"")
	})

	t.Run("token and token_url cannot be empty simultaneously", func(t *testing.T) {
		serverURL, err := url.Parse("https://example.com/")
		require.NoError(t, err)

		serialized := serializedConnectionSettings{
			URL:    serverURL.String(),
			UUID:   "009e3230-0881-4690-8e0e-43ce2c01d2f9",
			Token:  "",
			Labels: []string{"label-1"},
		}

		err = serialized.applyTo(&Config{}, "example")
		assert.ErrorContains(t, err, "`token` and `token_url` are empty")
	})

	t.Run("token and token_url are mutually exclusive", func(t *testing.T) {
		serverURL, err := url.Parse("https://example.com/")
		require.NoError(t, err)

		serialized := serializedConnectionSettings{
			URL:      serverURL.String(),
			UUID:     "009e3230-0881-4690-8e0e-43ce2c01d2f9",
			Token:    "8pqwdboRFjX",
			TokenURL: "file:does-not-exist.txt",
			Labels:   []string{"label-1"},
		}

		err = serialized.applyTo(&Config{}, "example")
		assert.ErrorContains(t, err, "`token` and `token_url` are mutually exclusive")
	})

	t.Run("resolves token_url", func(t *testing.T) {
		tempDir := t.TempDir()
		secretPath := filepath.Join(tempDir, "secret.txt")

		err := os.WriteFile(secretPath, []byte("8tBZOQlSaH"), 0o644)
		require.NoError(t, err)

		serverURL, err := url.Parse("https://example.com/")
		require.NoError(t, err)

		tokenURL, err := fileuri.FromFilePath(secretPath)
		require.NoError(t, err)

		serialized := serializedConnectionSettings{
			URL:      serverURL.String(),
			UUID:     "009e3230-0881-4690-8e0e-43ce2c01d2f9",
			Token:    "",
			TokenURL: tokenURL.String(),
			Labels:   []string{"label-1"},
		}

		config := Config{}
		err = serialized.applyTo(&config, "example")
		require.NoError(t, err)

		assert.Equal(t, "8tBZOQlSaH", config.Server.Connections["example"].Token)
	})

	t.Run("rejects token from token containing invalid characters", func(t *testing.T) {
		serverURL, err := url.Parse("https://example.com/")
		require.NoError(t, err)

		serialized := serializedConnectionSettings{
			URL:      serverURL.String(),
			UUID:     "009e3230-0881-4690-8e0e-43ce2c01d2f9",
			Token:    "VV\nb1\n\teuy",
			TokenURL: "",
			Labels:   []string{"label-1"},
		}

		config := Config{}
		err = serialized.applyTo(&config, "example")
		require.ErrorContains(t, err, "token contains invalid characters")
	})

	t.Run("rejects token from token_url containing invalid characters", func(t *testing.T) {
		tempDir := t.TempDir()
		secretPath := filepath.Join(tempDir, "secret.txt")

		err := os.WriteFile(secretPath, []byte("VV\nb1\r\teuy"), 0o644)
		require.NoError(t, err)

		serverURL, err := url.Parse("https://example.com/")
		require.NoError(t, err)

		tokenURL, err := fileuri.FromFilePath(secretPath)
		require.NoError(t, err)

		serialized := serializedConnectionSettings{
			URL:      serverURL.String(),
			UUID:     "009e3230-0881-4690-8e0e-43ce2c01d2f9",
			Token:    "",
			TokenURL: tokenURL.String(),
			Labels:   []string{"label-1"},
		}

		config := Config{}
		err = serialized.applyTo(&config, "example")
		require.ErrorContains(t, err, "token contains invalid characters")
	})

	t.Run("trims whitespace from token", func(t *testing.T) {
		serverURL, err := url.Parse("https://example.com/")
		require.NoError(t, err)

		serialized := serializedConnectionSettings{
			URL:      serverURL.String(),
			UUID:     "009e3230-0881-4690-8e0e-43ce2c01d2f9",
			Token:    "\nVVb1teuy\r\n",
			TokenURL: "",
			Labels:   []string{"label-1"},
		}

		config := Config{}
		err = serialized.applyTo(&config, "example")
		require.NoError(t, err)

		assert.Equal(t, "VVb1teuy", config.Server.Connections["example"].Token)
	})

	t.Run("trims whitespace from token from token_url", func(t *testing.T) {
		tempDir := t.TempDir()
		secretPath := filepath.Join(tempDir, "secret.txt")

		err := os.WriteFile(secretPath, []byte("\nVVb1teuy\r\n"), 0o644)
		require.NoError(t, err)

		serverURL, err := url.Parse("https://example.com/")
		require.NoError(t, err)

		tokenURL, err := fileuri.FromFilePath(secretPath)
		require.NoError(t, err)

		serialized := serializedConnectionSettings{
			URL:      serverURL.String(),
			UUID:     "009e3230-0881-4690-8e0e-43ce2c01d2f9",
			Token:    "",
			TokenURL: tokenURL.String(),
			Labels:   []string{"label-1"},
		}

		config := Config{}
		err = serialized.applyTo(&config, "example")
		require.NoError(t, err)

		assert.Equal(t, "VVb1teuy", config.Server.Connections["example"].Token)
	})

	t.Run("rejects malformed label", func(t *testing.T) {
		serverURL, err := url.Parse("https://example.com/")
		require.NoError(t, err)

		serialized := serializedConnectionSettings{
			URL:    serverURL.String(),
			UUID:   "54d7e9a6-0b44-4d81-8948-50c1dd351d19",
			Token:  "Shoi0zUBg6P",
			Labels: []string{"label1", " very invalid "},
		}

		err = serialized.applyTo(&Config{}, "example")
		assert.ErrorContains(t, err, "malformed `label` \" very invalid \"")
	})
}

func TestFromFile(t *testing.T) {
	t.Run("ignores empty path", func(t *testing.T) {
		config := Config{}
		err := FromFile("")(&config)
		require.NoError(t, err)

		assert.Equal(t, Config{}, config)
	})

	t.Run("returns error if file missing", func(t *testing.T) {
		config := Config{}
		err := FromFile("/does/not/exist")(&config)

		assert.ErrorContains(t, err, "cannot open config file \"/does/not/exist\"")
	})

	t.Run("returns error if file is invalid", func(t *testing.T) {
		tempDir := t.TempDir()

		path := filepath.Join(tempDir, "config.yml")
		err := os.WriteFile(path, []byte("/mal/formed/"), 0o644)
		require.NoError(t, err)

		config := Config{}
		err = FromFile(path)(&config)

		assert.ErrorContains(t, err, "cannot parse config file")
	})

	t.Run("accepts empty config file", func(t *testing.T) {
		tempDir := t.TempDir()

		path := filepath.Join(tempDir, "config.yml")
		err := os.WriteFile(path, []byte{}, 0o644)
		require.NoError(t, err)

		config := Config{}
		err = FromFile(path)(&config)
		require.NoError(t, err)

		assert.Equal(t, Config{}, config)
	})

	t.Run("accepts partial config file", func(t *testing.T) {
		expected := Config{
			Log: Log{
				Level:    "info",
				JobLevel: "warn",
			},
		}

		tempDir := t.TempDir()

		rawConfig := `
log:
  level: info
  job_level: warn
`

		path := filepath.Join(tempDir, "config.yml")
		err := os.WriteFile(path, []byte(rawConfig), 0o644)
		require.NoError(t, err)

		config := Config{}
		err = FromFile(path)(&config)
		require.NoError(t, err)

		assert.Equal(t, expected, config)
	})
}

func TestResolveSecretURL(t *testing.T) {
	t.Run("file scheme", func(t *testing.T) {
		rawSecret := "0v2kLviH0\r\nV1OfGph"

		tempDir := t.TempDir()
		secretPath := filepath.Join(tempDir, "secret.txt")

		err := os.WriteFile(secretPath, []byte(rawSecret), 0o644)
		require.NoError(t, err)

		secretURL, err := fileuri.FromFilePath(secretPath)
		require.NoError(t, err)

		secret, err := ResolveSecretURL(secretURL.String())
		require.NoError(t, err)

		assert.Equal(t, rawSecret, secret)
	})

	t.Run("error returned if URL is empty", func(t *testing.T) {
		_, err := ResolveSecretURL("")

		assert.ErrorContains(t, err, "unsupported secret URL: \"\"")
	})

	t.Run("error returned if scheme is unsupported", func(t *testing.T) {
		_, err := ResolveSecretURL("unsupported:some-secret")

		assert.ErrorContains(t, err, "unsupported secret URL: \"unsupported:some-secret\"")
	})
}

func TestResolveFileSecret(t *testing.T) {
	t.Run("empty host", func(t *testing.T) {
		rawSecret := "8AftSFunni1"

		tempDir := t.TempDir()
		secretPath := filepath.Join(tempDir, "secret.txt")

		err := os.WriteFile(secretPath, []byte(rawSecret), 0o644)
		require.NoError(t, err)

		secretURL, err := fileuri.FromFilePath(secretPath)
		require.NoError(t, err)

		secret, err := resolveFileSecret(secretURL.String())
		require.NoError(t, err)

		assert.Equal(t, rawSecret, secret)
	})

	t.Run("without host", func(t *testing.T) {
		rawSecret := "VT6mi1t"

		tempDir := t.TempDir()
		secretPath := filepath.Join(tempDir, "secret.txt")

		err := os.WriteFile(secretPath, []byte(rawSecret), 0o644)
		require.NoError(t, err)

		secretURL, err := fileuri.FromFilePath(secretPath)
		require.NoError(t, err)

		secret, err := resolveFileSecret(secretURL.String())
		require.NoError(t, err)

		assert.Equal(t, rawSecret, secret)
	})

	t.Run("ignores host", func(t *testing.T) {
		rawSecret := "Jojr4bopPe"

		tempDir := t.TempDir()
		secretPath := filepath.Join(tempDir, "secret.txt")

		err := os.WriteFile(secretPath, []byte(rawSecret), 0o644)
		require.NoError(t, err)
		secretURL, err := fileuri.FromFilePath(secretPath)
		require.NoError(t, err)
		secretURL.Host = "some-host"

		secret, err := resolveFileSecret(secretURL.String())
		require.NoError(t, err)

		assert.Equal(t, rawSecret, secret)
	})

	t.Run("with env variable CREDENTIALS_DIRECTORY", func(t *testing.T) {
		skip.If(t, runtime.GOOS != "linux") // The $CREDENTIALS_DIRECTORY environment variable is only relevant on Linux.
		rawSecret := "zoN4nQX"

		tempDir := t.TempDir()
		t.Setenv("CREDENTIALS_DIRECTORY", tempDir)
		secretPath := filepath.Join(tempDir, "secret.txt")

		err := os.WriteFile(secretPath, []byte(rawSecret), 0o644)
		require.NoError(t, err)

		secret, err := resolveFileSecret("file://$CREDENTIALS_DIRECTORY/secret.txt")
		require.NoError(t, err)

		assert.Equal(t, rawSecret, secret)
	})

	t.Run("without env variable CREDENTIALS_DIRECTORY", func(t *testing.T) {
		_, ok := os.LookupEnv("CREDENTIALS_DIRECTORY")
		assert.False(t, ok, "environment variable CREDENTIALS_DIRECTORY exists")

		_, err := resolveFileSecret("file://$CREDENTIALS_DIRECTORY/secret.txt")

		assert.ErrorContains(t, err, "cannot read secret \"file://$CREDENTIALS_DIRECTORY/secret.txt\"")
	})

	t.Run("empty string if file is empty", func(t *testing.T) {
		tempDir := t.TempDir()
		secretPath := filepath.Join(tempDir, "secret.txt")

		err := os.WriteFile(secretPath, []byte{}, 0o644)
		require.NoError(t, err)

		secretURL, err := fileuri.FromFilePath(secretPath)
		require.NoError(t, err)

		secret, err := resolveFileSecret(secretURL.String())
		require.NoError(t, err)

		assert.Empty(t, secret)
	})

	t.Run("error if file does not exist", func(t *testing.T) {
		secret, err := resolveFileSecret("file:///does/not/exist")

		assert.ErrorContains(t, err, "cannot read secret \"file:///does/not/exist\"")
		assert.Equal(t, "", secret)
	})

	t.Run("error if path is empty", func(t *testing.T) {
		secret, err := resolveFileSecret("file:")

		assert.ErrorContains(t, err, "cannot read secret \"file:\"")
		assert.Equal(t, "", secret)
	})
}
