package run

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	pingv1 "code.forgejo.org/forgejo/actions-proto/ping/v1"
	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"code.forgejo.org/forgejo/runner/v12/internal/pkg/config"
	"code.forgejo.org/forgejo/runner/v12/internal/pkg/labels"
	"code.forgejo.org/forgejo/runner/v12/internal/pkg/report"
	"connectrpc.com/connect"
	gouuid "github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/structpb"
	"gotest.tools/v3/skip"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func init() {
	log.SetLevel(log.TraceLevel)
}

func TestExplainFailedGenerateWorkflow(t *testing.T) {
	logged := ""
	log := func(message string, args ...any) {
		logged += fmt.Sprintf(message, args...) + "\n"
	}
	task := &runnerv1.Task{
		WorkflowPayload: []byte("on: [push]\njobs:\n"),
	}
	generateWorkflowError := errors.New("message 1\nmessage 2")
	err := explainFailedGenerateWorkflow(task, log, generateWorkflowError)
	assert.Error(t, err)
	assert.Equal(t, "    1: on: [push]\n    2: jobs:\n    3: \nErrors were found and although they tend to be cryptic the line number they refer to gives a hint as to where the problem might be.\nmessage 1\nmessage 2\n", logged)
}

func TestLabelUpdate(t *testing.T) {
	ctx := context.Background()
	ls := labels.Labels{}

	initialLabel, err := labels.Parse("testlabel:docker://alpine")
	assert.NoError(t, err)
	ls = append(ls, initialLabel)

	newLs := labels.Labels{}

	newLabel, err := labels.Parse("next label:host")
	assert.NoError(t, err)
	newLs = append(newLs, initialLabel)
	newLs = append(newLs, newLabel)

	runner := Runner{
		labels: ls,
	}

	assert.Contains(t, runner.labels, initialLabel)
	assert.NotContains(t, runner.labels, newLabel)

	runner.Update(ctx, newLs)

	assert.Contains(t, runner.labels, initialLabel)
	assert.Contains(t, runner.labels, newLabel)
}

type forgejoClientMock struct {
	mock.Mock
	sent string
}

func (m *forgejoClientMock) Address() string {
	args := m.Called()
	return args.String(0)
}

func (m *forgejoClientMock) Insecure() bool {
	args := m.Called()
	return args.Bool(0)
}

func (m *forgejoClientMock) FetchInterval() time.Duration {
	args := m.Called()
	return time.Duration(args.Int(0))
}

func (m *forgejoClientMock) SetRequestKey(requestKey gouuid.UUID) func() {
	args := m.Called(requestKey)
	return args.Get(0).(func())
}

func (m *forgejoClientMock) Ping(ctx context.Context, request *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
	args := m.Called(ctx, request)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*connect.Response[pingv1.PingResponse]), args.Error(1)
}

func (m *forgejoClientMock) Register(ctx context.Context, request *connect.Request[runnerv1.RegisterRequest]) (*connect.Response[runnerv1.RegisterResponse], error) {
	args := m.Called(ctx, request)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*connect.Response[runnerv1.RegisterResponse]), args.Error(1)
}

func (m *forgejoClientMock) Declare(ctx context.Context, request *connect.Request[runnerv1.DeclareRequest]) (*connect.Response[runnerv1.DeclareResponse], error) {
	args := m.Called(ctx, request)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*connect.Response[runnerv1.DeclareResponse]), args.Error(1)
}

func (m *forgejoClientMock) FetchTask(ctx context.Context, request *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error) {
	args := m.Called(ctx, request)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*connect.Response[runnerv1.FetchTaskResponse]), args.Error(1)
}

func (m *forgejoClientMock) FetchSingleTask(ctx context.Context, request *connect.Request[runnerv1.FetchSingleTaskRequest]) (*connect.Response[runnerv1.FetchSingleTaskResponse], error) {
	args := m.Called(ctx, request)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*connect.Response[runnerv1.FetchSingleTaskResponse]), args.Error(1)
}

func (m *forgejoClientMock) UpdateTask(ctx context.Context, request *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error) {
	args := m.Called(ctx, request)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*connect.Response[runnerv1.UpdateTaskResponse]), args.Error(1)
}

func rowsToString(rows []*runnerv1.LogRow) string {
	s := ""
	for _, row := range rows {
		s += row.Content + "\n"
	}
	return s
}

func (m *forgejoClientMock) UpdateLog(ctx context.Context, request *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error) {
	// Enable for log output from runs if needed.
	// for _, row := range request.Msg.Rows {
	// 	println(fmt.Sprintf("UpdateLog: %q", row.Content))
	// }
	m.sent += rowsToString(request.Msg.Rows)
	args := m.Called(ctx, request)
	mockRetval := args.Get(0)
	mockError := args.Error(1)
	if mockRetval != nil {
		return mockRetval.(*connect.Response[runnerv1.UpdateLogResponse]), mockError
	} else if mockError != nil {
		return nil, mockError
	}
	// Unless overridden by mock, default to returning indication that logs were received successfully
	return connect.NewResponse(&runnerv1.UpdateLogResponse{
		AckIndex: request.Msg.Index + int64(len(request.Msg.Rows)),
	}), nil
}

func TestRunner_getWriteIsolationKey(t *testing.T) {
	t.Run("push", func(t *testing.T) {
		key, err := getWriteIsolationKey(t.Context(), "push", "whatever", nil)
		require.NoError(t, err)
		assert.Empty(t, key)
	})

	t.Run("pull_request synchronized key is ref", func(t *testing.T) {
		expectedKey := "refs/pull/1/head"
		actualKey, err := getWriteIsolationKey(t.Context(), "pull_request", expectedKey, map[string]any{
			"action": "synchronized",
		})
		require.NoError(t, err)
		assert.Equal(t, expectedKey, actualKey)
	})

	t.Run("pull_request synchronized ref is invalid", func(t *testing.T) {
		invalidKey := "refs/is/invalid"
		key, err := getWriteIsolationKey(t.Context(), "pull_request", invalidKey, map[string]any{
			"action": "synchronized",
		})
		require.Empty(t, key)
		assert.ErrorContains(t, err, invalidKey)
	})

	t.Run("pull_request closed and not merged key is ref", func(t *testing.T) {
		expectedKey := "refs/pull/1/head"
		actualKey, err := getWriteIsolationKey(t.Context(), "pull_request", expectedKey, map[string]any{
			"action": "closed",
			"pull_request": map[string]any{
				"merged": false,
			},
		})
		require.NoError(t, err)
		assert.Equal(t, expectedKey, actualKey)
	})

	t.Run("pull_request closed and merged key is empty", func(t *testing.T) {
		key, err := getWriteIsolationKey(t.Context(), "pull_request", "whatever", map[string]any{
			"action": "closed",
			"pull_request": map[string]any{
				"merged": true,
			},
		})
		require.NoError(t, err)
		assert.Empty(t, key)
	})

	t.Run("pull_request missing event.pull_request", func(t *testing.T) {
		key, err := getWriteIsolationKey(t.Context(), "pull_request", "whatever", map[string]any{
			"action": "closed",
		})
		require.Empty(t, key)
		assert.ErrorContains(t, err, "event.pull_request is not a map")
	})

	t.Run("pull_request missing event.pull_request.merge", func(t *testing.T) {
		key, err := getWriteIsolationKey(t.Context(), "pull_request", "whatever", map[string]any{
			"action":       "closed",
			"pull_request": map[string]any{},
		})
		require.Empty(t, key)
		assert.ErrorContains(t, err, "event.pull_request.merged is not a bool")
	})

	t.Run("pull_request with event.pull_request.merge of an unexpected type", func(t *testing.T) {
		key, err := getWriteIsolationKey(t.Context(), "pull_request", "whatever", map[string]any{
			"action": "closed",
			"pull_request": map[string]any{
				"merged": "string instead of bool",
			},
		})
		require.Empty(t, key)
		assert.ErrorContains(t, err, "not a bool but string")
	})
}

func TestRunnerCacheConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	skip.If(t, runtime.GOOS != "linux") // Windows and macOS cannot run linux docker container natively

	forgejoClient := &forgejoClientMock{}

	forgejoClient.On("Address").Return("https://127.0.0.1:8080") // not expected to be used in this test
	forgejoClient.On("UpdateLog", mock.Anything, mock.Anything).Return(nil, nil)
	forgejoClient.On("UpdateTask", mock.Anything, mock.Anything).
		Return(connect.NewResponse(&runnerv1.UpdateTaskResponse{}), nil)

	cfg := &config.Config{
		Cache: config.Cache{
			// Note that this test requires that containers on the local dev environment can access localhost to
			// reach the cache proxy, and that the cache proxy can access localhost to reach the cache, both of
			// which are embedded servers that the Runner will start.  If any specific firewall config is needed
			// it's easier to do that with statically configured ports, so...
			Host:      "cache.internal",
			Port:      40713,
			ProxyPort: 40714,
			Dir:       t.TempDir(),
		},
		Host: config.Host{
			WorkdirParent: t.TempDir(),
		},
		Container: config.Container{
			DockerHost: os.Getenv("DOCKER_HOST"),
			Options:    "--add-host cache.internal:host-gateway",
		},
	}
	cacheProxy := SetupCache(cfg)
	defer func() {
		if cacheProxy != nil {
			cacheProxy.Close()
		}
	}()

	runner := NewRunner(
		cfg,
		"runner-name",
		[]*labels.Label{labels.MustParse("ubuntu-latest:docker://code.forgejo.org/oci/node:20-bookworm")},
		forgejoClient,
		cacheProxy)
	require.NotNil(t, runner)

	// Must set up cache for our test
	require.NotNil(t, runner.cacheProxy)

	runWorkflow := func(ctx context.Context, cancel context.CancelFunc, yamlContent, eventName, ref, description string) {
		task := &runnerv1.Task{
			WorkflowPayload: []byte(yamlContent),
			Context: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"token":                       structpb.NewStringValue("some token here"),
					"forgejo_default_actions_url": structpb.NewStringValue("https://data.forgejo.org"),
					"repository":                  structpb.NewStringValue("runner"),
					"event_name":                  structpb.NewStringValue(eventName),
					"ref":                         structpb.NewStringValue(ref),
				},
			},
		}

		reporter := report.NewReporter(ctx, cancel, forgejoClient, task, time.Second, &config.Retry{})
		err := runner.run(ctx, task, reporter)
		reporter.Close(nil)
		require.NoError(t, err, description)
	}

	t.Run("Cache accessible", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// Step 1: Populate shared cache with push workflow
		populateYaml := `
name: Cache Testing Action
on:
  push:
jobs:
  job-cache-check-1:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/cache@v4
        with:
          path: cache_path_1
          key: cache-key-1
      - run: |
          mkdir -p cache_path_1
          echo "Hello from push workflow!" > cache_path_1/cache_content_1
`
		runWorkflow(ctx, cancel, populateYaml, "push", "refs/heads/main", "step 1: push cache populate expected to succeed")

		// Step 2: Validate that cache is accessible; mostly a sanity check that the test environment and mock context
		// provides everything needed for the cache setup.
		checkYaml := `
name: Cache Testing Action
on:
  push:
jobs:
  job-cache-check-2:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/cache@v4
        with:
          path: cache_path_1
          key: cache-key-1
      - run: |
          [[ -f cache_path_1/cache_content_1 ]] && echo "Step 2: cache file found." || echo "Step 2: cache file missing!"
          [[ -f cache_path_1/cache_content_1 ]] || exit 1
`
		runWorkflow(ctx, cancel, checkYaml, "push", "refs/heads/main", "step 2: push cache check expected to succeed")
	})

	t.Run("PR cache pollution prevented", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// Step 1: Populate shared cache with push workflow
		populateYaml := `
name: Cache Testing Action
on:
  push:
jobs:
  job-cache-check-1:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/cache@v4
        with:
          path: cache_path_1
          key: cache-key-1
      - run: |
          mkdir -p cache_path_1
          echo "Hello from push workflow!" > cache_path_1/cache_content_1
`
		runWorkflow(ctx, cancel, populateYaml, "push", "refs/heads/main", "step 1: push cache populate expected to succeed")

		// Step 2: Check if pull_request can read push cache, should be available as it's a trusted cache.
		checkPRYaml := `
name: Cache Testing Action
on:
  pull_request:
jobs:
  job-cache-check-2:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/cache@v4
        with:
          path: cache_path_1
          key: cache-key-1
      - run: |
          [[ -f cache_path_1/cache_content_1 ]] && echo "Step 2: cache file found." || echo "Step 2: cache file missing!"
          [[ -f cache_path_1/cache_content_1 ]] || exit 1
`
		runWorkflow(ctx, cancel, checkPRYaml, "pull_request", "refs/pull/1234/head", "step 2: PR should read push cache")

		// Step 3: Pull request writes to cache; here we need to use a new cache key because we'll get a cache-hit like we
		// did in step #2 if we keep the same key, and then the cache contents won't be updated.
		populatePRYaml := `
name: Cache Testing Action
on:
  pull_request:
jobs:
  job-cache-check-3:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/cache@v4
        with:
          path: cache_path_1
          key: cache-key-2
      - run: |
          mkdir -p cache_path_1
          echo "Hello from PR workflow!" > cache_path_1/cache_content_2
`
		runWorkflow(ctx, cancel, populatePRYaml, "pull_request", "refs/pull/1234/head", "step 3: PR cache populate expected to succeed")

		// Step 4: Check if pull_request can read its own cache written by step #3.
		checkPRKey2Yaml := `
name: Cache Testing Action
on:
  pull_request:
jobs:
  job-cache-check-4:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/cache@v4
        with:
          path: cache_path_1
          key: cache-key-2
      - run: |
          [[ -f cache_path_1/cache_content_2 ]] && echo "Step 4 cache file found." || echo "Step 4 cache file missing!"
          [[ -f cache_path_1/cache_content_2 ]] || exit 1
`
		runWorkflow(ctx, cancel, checkPRKey2Yaml, "pull_request", "refs/pull/1234/head", "step 4: PR should read its own cache")

		// Step 5: Check that the push workflow cannot access the isolated cache that was written by the pull_request in
		// step #3, ensuring that it's not possible to pollute the cache by predicting cache keys.
		checkKey2Yaml := `
name: Cache Testing Action
on:
  push:
jobs:
  job-cache-check-6:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/cache@v4
        with:
          path: cache_path_1
          key: cache-key-2
      - run: |
          [[ -f cache_path_1/cache_content_2 ]] && echo "Step 5 cache file found, oh no!" || echo "Step 5: cache file missing as expected."
          [[ -f cache_path_1/cache_content_2 ]] && exit 1 || exit 0
`
		runWorkflow(ctx, cancel, checkKey2Yaml, "push", "refs/heads/main", "step 5: push cache should not be polluted by PR")
	})
}

func TestRunnerCacheStartupFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	skip.If(t, runtime.GOOS != "linux") // Windows and macOS cannot run linux docker container natively

	testCases := []struct {
		desc   string
		listen string
	}{
		{
			desc:   "disable cache server",
			listen: "127.0.0.1:40715",
		},
		{
			desc:   "disable cache proxy server",
			listen: "127.0.0.1:40716",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			forgejoClient := &forgejoClientMock{}

			forgejoClient.On("Address").Return("https://127.0.0.1:8080") // not expected to be used in this test
			forgejoClient.On("UpdateLog", mock.Anything, mock.Anything).Return(nil, nil)
			forgejoClient.On("UpdateTask", mock.Anything, mock.Anything).
				Return(connect.NewResponse(&runnerv1.UpdateTaskResponse{}), nil)

			// We'll be listening on some network port in this test that will conflict with the cache configuration...
			l, err := net.Listen("tcp4", tc.listen)
			require.NoError(t, err)
			defer l.Close()

			cfg := &config.Config{
				Cache: config.Cache{
					Port:      40715,
					ProxyPort: 40716,
					Dir:       t.TempDir(),
				},
				Host: config.Host{
					WorkdirParent: t.TempDir(),
				},
				Container: config.Container{
					DockerHost: os.Getenv("DOCKER_HOST"),
				},
			}
			cacheProxy := SetupCache(cfg)
			defer func() {
				if cacheProxy != nil {
					cacheProxy.Close()
				}
			}()

			runner := NewRunner(
				cfg,
				"runner-name",
				[]*labels.Label{labels.MustParse("ubuntu-latest:docker://code.forgejo.org/oci/node:20-bookworm")},
				forgejoClient,
				cacheProxy)
			require.NotNil(t, runner)

			// Ensure that cacheProxy failed to start
			assert.Nil(t, runner.cacheProxy)

			runWorkflow := func(ctx context.Context, cancel context.CancelFunc, yamlContent string) {
				task := &runnerv1.Task{
					WorkflowPayload: []byte(yamlContent),
					Context: &structpb.Struct{
						Fields: map[string]*structpb.Value{
							"token":                       structpb.NewStringValue("some token here"),
							"forgejo_default_actions_url": structpb.NewStringValue("https://data.forgejo.org"),
							"repository":                  structpb.NewStringValue("runner"),
							"event_name":                  structpb.NewStringValue("push"),
							"ref":                         structpb.NewStringValue("refs/heads/main"),
						},
					},
				}

				reporter := report.NewReporter(ctx, cancel, forgejoClient, task, time.Second, &config.Retry{})
				err := runner.run(ctx, task, reporter)
				reporter.Close(nil)
				require.NoError(t, err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			checkCacheYaml := `
name: Verify No ACTIONS_CACHE_URL
on:
  push:
jobs:
  job-cache-check-1:
    runs-on: ubuntu-latest
    steps:
      - run: echo $ACTIONS_CACHE_URL
      - run: '[[ "$ACTIONS_CACHE_URL" = "" ]] || exit 1'
`
			runWorkflow(ctx, cancel, checkCacheYaml)
		})
	}
}

func TestRunnerLXC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	_, err := exec.LookPath("lxc-create")
	skip.If(t, err != nil, "lxc-create is not on PATH")

	forgejoClient := &forgejoClientMock{}

	aaaaLogs := make([]int64, 0, 2400)

	forgejoClient.On("Address").Return("https://127.0.0.1:8080") // not expected to be used in this test
	forgejoClient.On("UpdateLog", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			updateRequest := args.Get(1).(*connect.Request[runnerv1.UpdateLogRequest])
			for _, row := range updateRequest.Msg.Rows {
				if strings.Contains(row.Content, "$(seq 2400)") {
					continue
				} else if strings.Contains(row.Content, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") {
					split := strings.Split(row.Content, " - ")
					index, err := strconv.ParseInt(split[0], 10, 64)
					require.NoError(t, err)
					aaaaLogs = append(aaaaLogs, index)
				}
			}
		}).
		Return(nil, nil)
	forgejoClient.On("UpdateTask", mock.Anything, mock.Anything).
		Return(connect.NewResponse(&runnerv1.UpdateTaskResponse{}), nil)

	workdirParent := t.TempDir()
	runner := NewRunner(
		&config.Config{
			Log: config.Log{
				JobLevel: "trace",
			},
			Host: config.Host{
				WorkdirParent: workdirParent,
			},
		},
		"runner-name",
		[]*labels.Label{labels.MustParse("lxc:lxc://debian:bookworm")},
		forgejoClient,
		nil)
	require.NotNil(t, runner)

	runMaybeWorkflow := func(ctx context.Context, cancel context.CancelFunc, yamlContent, eventName, ref, description string, success bool) {
		task := &runnerv1.Task{
			WorkflowPayload: []byte(yamlContent),
			Context: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"token":                       structpb.NewStringValue("some token here"),
					"forgejo_default_actions_url": structpb.NewStringValue("https://data.forgejo.org"),
					"repository":                  structpb.NewStringValue("runner"),
					"event_name":                  structpb.NewStringValue(eventName),
					"ref":                         structpb.NewStringValue(ref),
				},
			},
		}

		reporter := report.NewReporter(ctx, cancel, forgejoClient, task, time.Second, &config.Retry{})
		err := runner.run(ctx, task, reporter)
		reporter.Close(nil)
		if success {
			require.NoError(t, err, description)
		} else {
			require.Error(t, err, description)
		}
		// verify there are no leftovers
		assertDirectoryEmpty := func(t *testing.T, dir string) {
			f, err := os.Open(dir)
			require.NoError(t, err)
			defer f.Close()

			names, err := f.Readdirnames(-1)
			require.NoError(t, err)
			assert.Empty(t, names)
		}
		assertDirectoryEmpty(t, workdirParent)
	}
	runWorkflow := func(ctx context.Context, cancel context.CancelFunc, yamlContent, eventName, ref, description string) {
		runMaybeWorkflow(ctx, cancel, yamlContent, eventName, ref, description, true)
	}
	runFailedWorkflow := func(ctx context.Context, cancel context.CancelFunc, yamlContent, eventName, ref, description string) {
		runMaybeWorkflow(ctx, cancel, yamlContent, eventName, ref, description, false)
	}

	t.Run("OK", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		workflow := `
on:
  push:
jobs:
  job:
    runs-on: lxc
    steps:
      - run: mkdir -p some/directory/owned/by/root
`
		runWorkflow(ctx, cancel, workflow, "push", "refs/heads/main", "OK")
	})

	t.Run("LXC Environment", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		workflow := `
on:
  push:
jobs:
  validate:
    runs-on: lxc
    steps:
      - env:
          # pass an environment variable with a "-" into the step; regression test for when /bin/sh was stripping these variables
          abc-123: def-456
        run: |
          set -x

          # First clause (/home...) is expected for this workflow if pulled into a live Forgejo Actions system, and /tmp for the test environment
          if [[ "$PWD" =~ ^/home/[^/]+/\.cache/act/[^/]+/hostexecutor ]] || [[ "$PWD" =~ /tmp/TestRunnerLXC[0-9]+/[^/]+/[^/]+/hostexecutor ]]; then
            echo "Working directory matches expected pattern"
          else
            echo "Working directory does not match expected pattern"
            exit 1
          fi

          CGROUP_PATH=$(cat /proc/self/cgroup | grep '^0::' | cut -d: -f3)
          echo "Current cgroup: $CGROUP_PATH"
          # allow nested LXC (/../.lxc) or not (/.lxc) by matching just the tailing end
          if [[ "$CGROUP_PATH" =~ /.lxc$ ]]; then
            echo "Process is in LXC cgroup"
          else
            echo "Process cgroup does not match expected pattern"
            exit 1
          fi

          u=$(whoami)
          echo "Current user: $u"
          if [[ "$u" = "root" ]]; then
            echo "Process is running as root"
          else
            echo "Process user is unexpected"
            exit 1
          fi

          if [ -t 1 ]; then
            echo "stdout is a TTY"
          else
            echo "stdout is NOT a TTY"
            exit 1
          fi

          current_proc=$(readlink /proc/$$/exe)
          if [[ "$current_proc" = "/usr/bin/bash" ]]; then
            echo "current process is bash"
          else
            echo "current process is unexpected"
            exit 1
          fi

          if [[ "$FORGEJO_ACTIONS_RUNNER_VERSION" =~ ^(v[0-9].*|dev)$ ]]; then
            echo "FORGEJO_ACTIONS_RUNNER_VERSION is set indicating that env variables are available"
          else
            echo "FORGEJO_ACTIONS_RUNNER_VERSION is unexpected"
            exit 1
          fi

          env | grep abc-123=def-456
          ret=$?
          if [[ "$ret" -eq "0" ]]; then
            echo "able to find environment abc-123"
          else
            echo "failure to find expected environment variable abc-123"
          fi

          hostname1=$(hostname)
          hostname2=$(hostname -f)
          hostname3=$(cat /etc/hostname)
          if [[ "$hostname1" =~ ^[a-f0-9]{16}$ ]]; then
            echo "hostname matches expected pattern"
          else
            echo "unexpected hostname pattern"
            exit 1
          fi
          if [[ "$hostname1" = "$hostname2" ]] && [[ "$hostname1" = "$hostname3" ]]; then
            echo "all access to hostname is the same"
          else
            echo "unexpected mismatched hostname values"
            exit 1
          fi

          if [ -f /.dockerenv ]; then
            echo "I'm in docker?"
            exit 1
          elif [ -f /run/.containerenv ]; then
            echo "I'm in podman?"
            exit 1
          fi

          detect_virt=$(systemd-detect-virt)
          if [[ "$detect_virt" = "lxc" ]]; then
            echo "systemd-detect-virt thinks we're in LXC"
          else
            echo "unexpected systemd-detect-virt"
            exit 1
          fi

          init_env=$(cat /proc/1/environ)
          if [[ "$init_env" =~ container=lxc ]]; then
            echo "pid 1 is running in LXC"
          else
            echo "pid 1 is outside LXC; maybe I am too?"
            exit 1
          fi

          # make sure we're in the same namespaces as pid 1
          for namespace in cgroup ipc mnt net pid pid_for_children time time_for_children user uts; do
            INIT_NS=$(readlink /proc/1/ns/$namespace 2>/dev/null || echo "unknown")
            SELF_NS=$(readlink /proc/self/ns/$namespace 2>/dev/null || echo "unknown")
            if [[ "$INIT_NS" != "$SELF_NS" ]]; then
              echo "namespace $namespace different from init process"
              exit 1
            fi
          done
      - name: mutate action PATH
        run: |
          mkdir tmpdir
          echo "echo path mutation test successful" > tmpdir/test-mutated-path.sh
          chmod +x tmpdir/test-mutated-path.sh
          echo "$(pwd)/tmpdir" >> $FORGEJO_PATH
      - name: verify mutated action PATH
        run: test-mutated-path.sh
`
		runWorkflow(ctx, cancel, workflow, "push", "refs/heads/main", "OK")
	})

	t.Run("Large Fast Logs", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		aaaaLogs = make([]int64, 0, 2400) // reset to empty

		workflow := `
on:
  push:
jobs:
  test:
    runs-on: lxc
    steps:
      - run: |
          for i in $(seq 2400) ; do
             echo $i - AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
          done
`
		runWorkflow(ctx, cancel, workflow, "push", "refs/heads/main", "OK")

		slices.Sort(aaaaLogs)
		require.Len(t, aaaaLogs, 2400)
		for i := range 2400 {
			assert.EqualValues(t, i+1, aaaaLogs[i], "aaaaLogs[%d]", i)
		}
	})

	t.Run("Large Fast Logs (error)", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		aaaaLogs = make([]int64, 0, 2400) // reset to empty

		workflow := `
on:
  push:
jobs:
  test:
    runs-on: lxc
    steps:
      - run: |
          for i in $(seq 2400) ; do
             echo $i - AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
          done
          exit 1
`
		runFailedWorkflow(ctx, cancel, workflow, "push", "refs/heads/main", "OK")

		slices.Sort(aaaaLogs)
		require.Len(t, aaaaLogs, 2400)
		for i := range 2400 {
			assert.EqualValues(t, i+1, aaaaLogs[i], "aaaaLogs[%d]", i)
		}
	})
}

func TestRunnerResources(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	skip.If(t, runtime.GOOS != "linux") // Windows and macOS cannot run linux docker container natively

	forgejoClient := &forgejoClientMock{}

	forgejoClient.On("Address").Return("https://127.0.0.1:8080") // not expected to be used in this test
	forgejoClient.On("UpdateLog", mock.Anything, mock.Anything).Return(nil, nil)
	forgejoClient.On("UpdateTask", mock.Anything, mock.Anything).
		Return(connect.NewResponse(&runnerv1.UpdateTaskResponse{}), nil)

	workdirParent := t.TempDir()

	runWorkflow := func(ctx context.Context, cancel context.CancelFunc, yamlContent, options, errorMessage, logMessage string) {
		task := &runnerv1.Task{
			WorkflowPayload: []byte(yamlContent),
			Context: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"token":                       structpb.NewStringValue("some token here"),
					"forgejo_default_actions_url": structpb.NewStringValue("https://data.forgejo.org"),
					"repository":                  structpb.NewStringValue("runner"),
					"event_name":                  structpb.NewStringValue("push"),
					"ref":                         structpb.NewStringValue("refs/heads/main"),
				},
			},
		}

		runner := NewRunner(
			&config.Config{
				Log: config.Log{
					JobLevel: "trace",
				},
				Host: config.Host{
					WorkdirParent: workdirParent,
				},
				Container: config.Container{
					DockerHost: os.Getenv("DOCKER_HOST"),
					Options:    options,
				},
			},
			"runner-name",
			[]*labels.Label{labels.MustParse("ubuntu-latest:docker://code.forgejo.org/oci/node:20-bookworm")},
			forgejoClient,
			nil)
		require.NotNil(t, runner)

		reporter := report.NewReporter(ctx, cancel, forgejoClient, task, time.Second, &config.Retry{})
		err := runner.run(ctx, task, reporter)
		reporter.Close(nil)
		if len(errorMessage) > 0 {
			require.Error(t, err)
			assert.ErrorContains(t, err, errorMessage)
		} else {
			require.NoError(t, err)
		}
		if len(logMessage) > 0 {
			assert.Contains(t, forgejoClient.sent, logMessage)
		}
	}

	t.Run("config.yaml --memory set and enforced", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		workflow := `
on:
  push:
jobs:
  job:
    runs-on: docker
    steps:
      - run: |
          # more than 300MB
          perl -e '$a = "a" x (300 * 1024 * 1024)'
`
		runWorkflow(ctx, cancel, workflow, "--memory 200M", "Job 'job' failed", "Killed")
	})

	t.Run("config.yaml --memory set and within limits", func(t *testing.T) {
		skip.If(t, runtime.GOOS != "linux") // Windows and macOS cannot run linux docker container natively
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		workflow := `
on:
  push:
jobs:
  job:
    runs-on: docker
    steps:
      - run: echo OK
`
		runWorkflow(ctx, cancel, workflow, "--memory 200M", "", "")
	})

	t.Run("config.yaml --memory set and container fails to increase it", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		workflow := `
on:
  push:
jobs:
  job:
    runs-on: docker
    container:
      image: code.forgejo.org/oci/node:20-bookworm
      options: --memory 4G
    steps:
      - run: |
          # more than 300MB
          perl -e '$a = "a" x (300 * 1024 * 1024)'
`
		runWorkflow(ctx, cancel, workflow, "--memory 200M", "option found in the workflow cannot be greater than", "")
	})

	t.Run("container --memory set and enforced", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		workflow := `
on:
  push:
jobs:
  job:
    runs-on: docker
    container:
      image: code.forgejo.org/oci/node:20-bookworm
      options: --memory 200M
    steps:
      - run: |
          # more than 300MB
          perl -e '$a = "a" x (300 * 1024 * 1024)'
`
		runWorkflow(ctx, cancel, workflow, "", "Job 'job' failed", "Killed")
	})

	t.Run("container --memory set and within limits", func(t *testing.T) {
		skip.If(t, runtime.GOOS != "linux") // Windows and macOS cannot run linux docker container natively
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		workflow := `
on:
  push:
jobs:
  job:
    runs-on: docker
    container:
      image: code.forgejo.org/oci/node:20-bookworm
      options: --memory 200M
    steps:
      - run: echo OK
`
		runWorkflow(ctx, cancel, workflow, "", "", "")
	})
}

func TestRunnerContextsPopulated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	skip.If(t, runtime.GOOS != "linux") // Windows and macOS cannot run linux docker container natively

	forgejoClient := &forgejoClientMock{}

	forgejoClient.On("Address").Return("https://127.0.0.1:8080") // not expected to be used in this test
	forgejoClient.On("UpdateLog", mock.Anything, mock.Anything).Return(nil, nil)
	forgejoClient.On("UpdateTask", mock.Anything, mock.Anything).
		Return(connect.NewResponse(&runnerv1.UpdateTaskResponse{}), nil)

	workdirParent := t.TempDir()

	runWorkflow := func(ctx context.Context, cancel context.CancelFunc, yamlContent, options, errorMessage, logMessage string) {
		task := &runnerv1.Task{
			WorkflowPayload: []byte(yamlContent),
			Context: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"actor":                                  structpb.NewStringValue("somebody"),
					"event_name":                             structpb.NewStringValue("push"),
					"forgejo_default_actions_url":            structpb.NewStringValue("https://data.forgejo.org"),
					"forgejo_actions_id_token_request_token": structpb.NewStringValue("someTokenVal"),
					"forgejo_actions_id_token_request_url":   structpb.NewStringValue("https://data.forgejo.org/other"),
					"ref":                                    structpb.NewStringValue("refs/heads/sample-patch-1"),
					"ref_name":                               structpb.NewStringValue("sample-patch-1"),
					"ref_type":                               structpb.NewStringValue("branch"),
					"head_ref":                               structpb.NewStringValue("sample-patch-1"),
					"base_ref":                               structpb.NewStringValue("main"),
					"repository":                             structpb.NewStringValue("forgejo/runner"),
					"repository_owner":                       structpb.NewStringValue("forgejo"),
					"retention_days":                         structpb.NewStringValue("90"),
					"run_attempt":                            structpb.NewStringValue("3"),
					"run_id":                                 structpb.NewStringValue("150"),
					"run_number":                             structpb.NewStringValue("129"),
					"token":                                  structpb.NewStringValue("some-token-value"),
					"sha":                                    structpb.NewStringValue("5d64b71392b1e00a3ad893db02d381d58262c2d6"), // SHA1 of `random`
					"workflow_ref":                           structpb.NewStringValue("example/test/.forgejo/workflows/test.yaml@refs/heads/main"),
				},
			},
		}

		runner := NewRunner(
			&config.Config{
				Log: config.Log{
					JobLevel: "trace",
				},
				Host: config.Host{
					WorkdirParent: workdirParent,
				},
				Container: config.Container{
					DockerHost: os.Getenv("DOCKER_HOST"),
					Options:    options,
				},
			},
			"runner-name",
			[]*labels.Label{labels.MustParse("docker:docker://code.forgejo.org/oci/node:20-bookworm")},
			forgejoClient,
			nil)
		require.NotNil(t, runner)

		reporter := report.NewReporter(ctx, cancel, forgejoClient, task, time.Second, &config.Retry{})
		err := runner.run(ctx, task, reporter)
		reporter.Close(nil)
		if len(errorMessage) > 0 {
			require.Error(t, err)
			assert.ErrorContains(t, err, errorMessage)
		} else {
			require.NoError(t, err)
		}
		if len(logMessage) > 0 {
			assert.Contains(t, forgejoClient.sent, logMessage)
		}
	}

	t.Run("github", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		workflow := `
name: Predefined variables
on:
  push:
jobs:
  assert-github-context:
    runs-on: docker
    container:
      image: code.forgejo.org/oci/node:20-bookworm
    steps:
      - run: |
          echo github.run_id=${{ github.run_id }}
          [[ "${{ github.run_id }}" = "150" ]] || exit 1
          echo github.run_number=${{ github.run_number }}
          [[ "${{ github.run_number }}" = "129" ]] || exit 1
          echo github.run_attempt=${{ github.run_attempt }}
          [[ "${{ github.run_attempt }}" = "3" ]] || exit 1
          echo github.workflow_ref=${{ github.workflow_ref }}
          [[ "${{ github.workflow_ref }}" = "example/test/.forgejo/workflows/test.yaml@refs/heads/main" ]] || exit 1
`
		runWorkflow(ctx, cancel, workflow, "", "", "")
	})

	t.Run("forgejo", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		workflow := `
on:
  push:
jobs:
  assert-forgejo-context:
    runs-on: docker
    container:
      image: code.forgejo.org/oci/node:20-bookworm
    steps:
      - run: |
          echo forgejo.run_id=${{ forgejo.run_id }}
          [[ "${{ forgejo.run_id }}" = "150" ]] || exit 1
          echo forgejo.run_number=${{ forgejo.run_number }}
          [[ "${{ forgejo.run_number }}" = "129" ]] || exit 1
          echo forgejo.run_attempt=${{ forgejo.run_attempt }}
          [[ "${{ forgejo.run_attempt }}" = "3" ]] || exit 1
          echo forgejo.workflow_ref=${{ forgejo.workflow_ref }}
          [[ "${{ forgejo.workflow_ref }}" = "example/test/.forgejo/workflows/test.yaml@refs/heads/main" ]] || exit 1
`
		runWorkflow(ctx, cancel, workflow, "", "", "")
	})

	// This test is for testing compatibility with GitHub Actions. Put variables that are not supported by GitHub
	// Actions in another test.
	//
	// Partial list of variables: https://docs.github.com/en/actions/reference/workflows-and-actions/variables
	t.Run("GitHub Actions environment variables", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		workflow := `
name: Predefined variables
on:
  push:
jobs:
  assert-environment-variables:
    runs-on: docker
    container:
      image: code.forgejo.org/oci/node:20-bookworm
    steps:
      - name: Test presence of all variables supported by GitHub
        run: |
          echo CI="$CI"
          [[ "$CI" ]] || exit 1
          echo GITHUB_ACTION="$GITHUB_ACTION"
          [[ "$GITHUB_ACTION" = 0 ]] || exit 1
          echo GITHUB_ACTIONS="$GITHUB_ACTIONS"
          [[ "$GITHUB_ACTIONS" ]] || exit 1
          echo GITHUB_ACTION_PATH="$GITHUB_ACTION_PATH"
          [[ ${GITHUB_ACTION_PATH-x} = "" ]] || exit 1  # Only available in composite actions.
          echo GITHUB_ACTION_REF="$GITHUB_ACTION_REF"
          [[ ${GITHUB_ACTION_REF-x} = "" ]] || exit 1
          echo GITHUB_ACTION_REPOSITORY="$GITHUB_ACTION_REPOSITORY"
          [[ ${GITHUB_ACTION_REPOSITORY-x} = "" ]] || exit 1  # Only available in steps executing an action.
          echo GITHUB_ACTOR="$GITHUB_ACTOR"
          [[ "$GITHUB_ACTOR" = "somebody" ]] || exit 1
          echo GITHUB_ACTOR_ID="$GITHUB_ACTOR_ID"
          [[ -z ${GITHUB_ACTOR_ID+x} ]] || exit 1  # Currently unsupported.
          echo GITHUB_API_URL="$GITHUB_API_URL"
          [[ "$GITHUB_API_URL" = "https://127.0.0.1:8080/api/v1" ]] || exit 1
          echo GITHUB_BASE_REF="$GITHUB_BASE_REF"
          [[ "$GITHUB_BASE_REF" = "main" ]] || exit 1  # Only available in pull requests.
          echo GITHUB_ENV="$GITHUB_ENV"
          [[ "$GITHUB_ENV" = "/var/run/act/workflow/envs.txt" ]] || exit 1
          echo GITHUB_EVENT_NAME="$GITHUB_EVENT_NAME"
          [[ "$GITHUB_EVENT_NAME" = "push" ]] || exit 1
          echo GITHUB_EVENT_PATH="$GITHUB_EVENT_PATH"
          [[ "$GITHUB_EVENT_PATH" = "/var/run/act/workflow/event.json" ]] || exit 1
          echo GITHUB_GRAPHQL_URL="$GITHUB_GRAPHQL_URL"
          [[ -z ${GITHUB_GRAPHQL_URL+x} ]] || exit 1  # Not applicable, Forgejo does not have GraphQL endpoints.
          echo GITHUB_HEAD_REF="$GITHUB_HEAD_REF"
          [[ "$GITHUB_HEAD_REF" = "sample-patch-1" ]] || exit 1  # Only available in pull requests.
          echo GITHUB_JOB="$GITHUB_JOB"
          [[ "$GITHUB_JOB" = "assert-environment-variables" ]] || exit 1
          echo GITHUB_OUTPUT="$GITHUB_OUTPUT"
          [[ "$GITHUB_OUTPUT" = "/var/run/act/workflow/outputcmd.txt" ]] || exit 1
          echo GITHUB_PATH="$GITHUB_PATH"
          [[ "$GITHUB_PATH" = "/var/run/act/workflow/pathcmd.txt" ]] || exit 1
          echo GITHUB_REF="$GITHUB_REF"
          [[ "$GITHUB_REF" = "refs/heads/sample-patch-1" ]] || exit 1
          echo GITHUB_REF_NAME="$GITHUB_REF_NAME"
          [[ "$GITHUB_REF_NAME" = "sample-patch-1" ]] || exit 1
          echo GITHUB_REF_PROTECTED="$GITHUB_REF_PROTECTED"
          [[ -z ${GITHUB_REF_PROTECTED+x} ]] || exit 1  # Currently unsupported.
          echo GITHUB_REF_TYPE="$GITHUB_REF_TYPE"
          [[ "$GITHUB_REF_TYPE" = "branch" ]] || exit 1
          echo GITHUB_REPOSITORY="$GITHUB_REPOSITORY"
          [[ "$GITHUB_REPOSITORY" == "forgejo/runner" ]] || exit 1
          echo GITHUB_REPOSITORY_ID="$GITHUB_REPOSITORY_ID"
          [[ -z ${GITHUB_REPOSITORY_ID+x} ]] || exit 1  # Currently unsupported.
          echo GITHUB_REPOSITORY_OWNER="$GITHUB_REPOSITORY_OWNER"
          [[ "$GITHUB_REPOSITORY_OWNER" == "forgejo" ]] || exit 1
          echo GITHUB_REPOSITORY_OWNER_ID="$GITHUB_REPOSITORY_OWNER_ID"
          [[ -z ${GITHUB_REPOSITORY_OWNER_ID+x} ]] || exit 1  # Currently unsupported.
          echo GITHUB_RETENTION_DAYS="$GITHUB_RETENTION_DAYS"
          [[ "$GITHUB_RETENTION_DAYS" = 90 ]] || exit 1
          echo GITHUB_RUN_ATTEMPT="$GITHUB_RUN_ATTEMPT"
          [[ "$GITHUB_RUN_ATTEMPT" = 3 ]] || exit 1
          echo GITHUB_RUN_ID="$GITHUB_RUN_ID"
          [[ "$GITHUB_RUN_ID" = 150 ]] || exit 1
          echo GITHUB_RUN_NUMBER="$GITHUB_RUN_NUMBER"
          [[ "$GITHUB_RUN_NUMBER" = 129 ]] || exit 1
          echo GITHUB_SERVER_URL="$GITHUB_SERVER_URL"
          [[ "$GITHUB_SERVER_URL" = "https://127.0.0.1:8080" ]] || exit 1
          echo GITHUB_SHA="$GITHUB_SHA"
          [[ "$GITHUB_SHA" == "5d64b71392b1e00a3ad893db02d381d58262c2d6" ]] || exit 1
          echo GITHUB_STATE="$GITHUB_STATE"
          [[ "$GITHUB_STATE" = "/var/run/act/workflow/statecmd.txt" ]] || exit 1
          echo GITHUB_STEP_SUMMARY="$GITHUB_STEP_SUMMARY"
          [[ "$GITHUB_STEP_SUMMARY" = "/var/run/act/workflow/SUMMARY.md" ]] || exit 1
          echo GITHUB_TRIGGERING_ACTOR="$GITHUB_TRIGGERING_ACTOR"
          [[ -z ${GITHUB_TRIGGERING_ACTOR+x} ]] || exit 1  # Currently unsupported.
          echo GITHUB_TOKEN="$GITHUB_TOKEN"
          [[ "$GITHUB_TOKEN" = "some-token-value" ]] || exit 1
          echo GITHUB_WORKFLOW="$GITHUB_WORKFLOW"
          [[ "$GITHUB_WORKFLOW" = "Predefined variables" ]] || exit 1
          echo GITHUB_WORKFLOW_REF="$GITHUB_WORKFLOW_REF"
          [[ "$GITHUB_WORKFLOW_REF" = "example/test/.forgejo/workflows/test.yaml@refs/heads/main"  ]] || exit 1
          echo GITHUB_WORKFLOW_SHA="$GITHUB_WORKFLOW_SHA"
          [[ -z ${GITHUB_WORKFLOW_SHA+x} ]] || exit 1  # Currently unsupported.
          echo GITHUB_WORKSPACE="$GITHUB_WORKSPACE"
          [[ -d "$GITHUB_WORKSPACE" ]] || exit 1
          echo RUNNER_ARCH="$RUNNER_ARCH"
          [[ -n "$RUNNER_ARCH" ]] || exit 1
          echo RUNNER_DEBUG="$RUNNER_DEBUG"
          [[ -z ${RUNNER_DEBUG+x} ]] || exit 1  # Currently unsupported.
          echo RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT"
          [[ -z ${RUNNER_ENVIRONMENT+x} ]] || exit 1  # Not applicable to Forgejo Runner.
          echo RUNNER_NAME="$RUNNER_NAME"
          [[ -z ${RUNNER_NAME+x} ]] || exit 1  # Currently unsupported.
          echo RUNNER_OS="$RUNNER_OS"
          [[ -n "$RUNNER_OS" ]] || exit 1
          echo RUNNER_TEMP="$RUNNER_TEMP"
          [[ -d "$RUNNER_TEMP" ]] || exit 1
          echo RUNNER_TOOL_CACHE="$RUNNER_TOOL_CACHE"
          [[ -n "$RUNNER_TOOL_CACHE" ]] || exit 1  # Directory only exists if users mount a volume to it.
          echo RUNNER_TRACKING_ID="$RUNNER_TRACKING_ID"
          [[ ${RUNNER_TRACKING_ID-x} = "" ]] || exit 1
`
		runWorkflow(ctx, cancel, workflow, "", "", "")
	})

	t.Run("Forgejo environment variables", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		workflow := `
name: Predefined variables
on:
  push:
jobs:
  assert-environment-variables:
    enable-openid-connect: true
    runs-on: docker
    container:
      image: code.forgejo.org/oci/node:20-bookworm
    steps:
      - name: Test presence of additional variables supported by Forgejo
        run: |
          echo ACT="$ACT"
          [[ "$ACT" ]] || exit 1
          echo ACTIONS_CACHE_URL="$ACTIONS_CACHE_URL"
          [[ -z "${ACTIONS_CACHE_URL+x}" ]] || exit 1
          echo ACTIONS_RESULTS_URL="$ACTIONS_RESULTS_URL"
          [[ "$ACTIONS_RESULTS_URL" = "https://127.0.0.1:8080" ]] || exit 1
          echo ACTIONS_RUNTIME_TOKEN="$ACTIONS_RUNTIME_TOKEN"
          [[ "$ACTIONS_RUNTIME_TOKEN" = "some-token-value" ]] || exit 1
          echo ACTIONS_RUNTIME_URL="$ACTIONS_RUNTIME_URL"
          [[ "$ACTIONS_RUNTIME_URL" = "https://127.0.0.1:8080/api/actions_pipeline/" ]] || exit 1
          echo FORGEJO_ACTIONS_RUNNER_VERSION="$FORGEJO_ACTIONS_RUNNER_VERSION"
          [[ -n "$FORGEJO_ACTIONS_RUNNER_VERSION" ]] || exit 1
          echo ImageOS="$ImageOS"
          [[ -n "$ImageOS" ]] || exit 1
          echo JOB_CONTAINER_NAME="$JOB_CONTAINER_NAME"
          [[ -n "$JOB_CONTAINER_NAME" ]] || exit 1
          echo RUNNER_PERFLOG="$RUNNER_PERFLOG"
          [[ -e "$RUNNER_PERFLOG" ]] || exit 1
          echo ACTIONS_ID_TOKEN_REQUEST_TOKEN="$ACTIONS_ID_TOKEN_REQUEST_TOKEN"
          [[ "$ACTIONS_ID_TOKEN_REQUEST_TOKEN" = "someTokenVal" ]] || exit 1
          echo ACTIONS_ID_TOKEN_REQUEST_URL="$ACTIONS_ID_TOKEN_REQUEST_URL"
          [[ "$ACTIONS_ID_TOKEN_REQUEST_URL" = "https://data.forgejo.org/other" ]] || exit 1
`
		runWorkflow(ctx, cancel, workflow, "", "", "")
	})
}

func TestBuildK8sPodSpecs(t *testing.T) {
	t.Run("extracts k8spod labels", func(t *testing.T) {
		r := &Runner{
			labels: labels.Labels{
				{Name: "small", Schema: labels.SchemeK8sPod, Arg: "//config/podspec-small.yaml"},
				{Name: "gpu", Schema: labels.SchemeK8sPod, Arg: "//config/podspec-gpu.yaml"},
				{Name: "docker-runner", Schema: labels.SchemeDocker, Arg: labels.ArgDocker},
			},
		}
		specs := r.buildK8sPodSpecs()
		assert.Equal(t, map[string]string{
			"small": "config/podspec-small.yaml",
			"gpu":   "config/podspec-gpu.yaml",
		}, specs)
	})

	t.Run("skips non-k8spod labels", func(t *testing.T) {
		r := &Runner{
			labels: labels.Labels{
				{Name: "docker", Schema: labels.SchemeDocker, Arg: labels.ArgDocker},
				{Name: "host", Schema: labels.SchemeHost},
			},
		}
		specs := r.buildK8sPodSpecs()
		assert.Empty(t, specs)
	})

	t.Run("empty labels", func(t *testing.T) {
		r := &Runner{}
		specs := r.buildK8sPodSpecs()
		assert.Empty(t, specs)
	})
}
