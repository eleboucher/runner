package docker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	actcontainer "code.forgejo.org/forgejo/runner/v12/act/container"
	"gotest.tools/v3/skip"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDocker(t *testing.T) {
	skip.If(t, runtime.GOOS != "linux") // Windows and macOS cannot natively run Linux containers
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := t.Context()
	ep, err := NewEndpoint(ctx, os.Getenv("DOCKER_HOST"))
	assert.NoError(t, err)
	defer ep.Close()
	cli := ep.Client()

	dockerBuild := NewDockerBuildExecutor(ep, NewDockerBuildExecutorInput{
		ContextDir: "testdata",
		ImageTag:   "envmergetest",
	})

	err = dockerBuild(ctx)
	assert.NoError(t, err)

	cr := &containerReference{
		cli: cli,
		input: &actcontainer.NewContainerInput{
			Image: "envmergetest",
		},
	}
	env := map[string]string{
		"PATH":         "/usr/local/bin:/usr/bin:/usr/sbin:/bin:/sbin",
		"RANDOM_VAR":   "WITH_VALUE",
		"ANOTHER_VAR":  "",
		"CONFLICT_VAR": "I_EXIST_IN_MULTIPLE_PLACES",
	}

	envExecutor := cr.extractFromImageEnv(&env)
	err = envExecutor(ctx)
	assert.NoError(t, err)
	assert.Equal(t, map[string]string{
		"PATH":            "/usr/local/bin:/usr/bin:/usr/sbin:/bin:/sbin:/this/path/does/not/exists/anywhere:/this/either",
		"RANDOM_VAR":      "WITH_VALUE",
		"ANOTHER_VAR":     "",
		"SOME_RANDOM_VAR": "",
		"ANOTHER_ONE":     "BUT_I_HAVE_VALUE",
		"CONFLICT_VAR":    "I_EXIST_IN_MULTIPLE_PLACES",
	}, env)
}

type mockDockerClient struct {
	client.APIClient
	mock.Mock
}

func (m *mockDockerClient) ContainerExecCreate(ctx context.Context, id string, opts container.ExecOptions) (container.ExecCreateResponse, error) {
	args := m.Called(ctx, id, opts)
	return args.Get(0).(container.ExecCreateResponse), args.Error(1)
}

func (m *mockDockerClient) ContainerExecAttach(ctx context.Context, id string, opts container.ExecAttachOptions) (types.HijackedResponse, error) {
	args := m.Called(ctx, id, opts)
	return args.Get(0).(types.HijackedResponse), args.Error(1)
}

func (m *mockDockerClient) ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error) {
	args := m.Called(ctx, execID)
	return args.Get(0).(container.ExecInspect), args.Error(1)
}

func (m *mockDockerClient) CopyToContainer(ctx context.Context, id, path string, content io.Reader, options container.CopyToContainerOptions) error {
	args := m.Called(ctx, id, path, content, options)
	return args.Error(0)
}

type endlessReader struct {
	io.Reader
}

func (r endlessReader) Read(_ []byte) (n int, err error) {
	return 1, nil
}

type mockConn struct {
	net.Conn
	mock.Mock
}

func (m *mockConn) Write(b []byte) (n int, err error) {
	args := m.Called(b)
	return args.Int(0), args.Error(1)
}

func (m *mockConn) Close() (err error) {
	return nil
}

func TestDockerExecAbort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	conn := &mockConn{}
	conn.On("Write", mock.AnythingOfType("[]uint8")).Return(1, nil)

	client := &mockDockerClient{}
	client.On("ContainerExecCreate", ctx, "123", mock.AnythingOfType("container.ExecOptions")).Return(container.ExecCreateResponse{ID: "id"}, nil)
	// container.ExecStartOptions should be container.ExecAttachOptions but fails
	client.On("ContainerExecAttach", ctx, "id", mock.AnythingOfType("container.ExecStartOptions")).Return(types.HijackedResponse{
		Conn:   conn,
		Reader: bufio.NewReader(endlessReader{}),
	}, nil)

	cr := &containerReference{
		id:  "123",
		cli: client,
		input: &actcontainer.NewContainerInput{
			Image: "image",
		},
	}

	channel := make(chan error)

	go func() {
		channel <- cr.exec([]string{""}, map[string]string{}, "user", "workdir")(ctx)
	}()

	time.Sleep(500 * time.Millisecond)

	cancel()

	err := <-channel
	assert.ErrorIs(t, err, context.Canceled)

	conn.AssertExpectations(t)
	client.AssertExpectations(t)
}

func TestDockerExecFailure(t *testing.T) {
	ctx := t.Context()

	conn := &mockConn{}

	client := &mockDockerClient{}
	client.On("ContainerExecCreate", ctx, "123", mock.AnythingOfType("container.ExecOptions")).Return(container.ExecCreateResponse{ID: "id"}, nil)
	// container.ExecStartOptions should be container.ExecAttachOptions but fails
	client.On("ContainerExecAttach", ctx, "id", mock.AnythingOfType("container.ExecStartOptions")).Return(types.HijackedResponse{
		Conn:   conn,
		Reader: bufio.NewReader(strings.NewReader("output")),
	}, nil)
	client.On("ContainerExecInspect", ctx, "id").Return(container.ExecInspect{
		ExitCode: 1,
	}, nil)

	cr := &containerReference{
		id:  "123",
		cli: client,
		input: &actcontainer.NewContainerInput{
			Image: "image",
		},
	}

	err := cr.exec([]string{""}, map[string]string{}, "user", "workdir")(ctx)
	assert.Error(t, err, "exit with `FAILURE`: 1")

	conn.AssertExpectations(t)
	client.AssertExpectations(t)
}

func TestDockerCopyTarStream(t *testing.T) {
	ctx := t.Context()

	conn := &mockConn{}

	client := &mockDockerClient{}
	client.On("CopyToContainer", ctx, "123", "/", mock.Anything, mock.AnythingOfType("container.CopyToContainerOptions")).Return(nil)
	client.On("CopyToContainer", ctx, "123", "/var/run/act", mock.Anything, mock.AnythingOfType("container.CopyToContainerOptions")).Return(nil)
	cr := &containerReference{
		id:  "123",
		cli: client,
		input: &actcontainer.NewContainerInput{
			Image: "image",
		},
	}

	_ = cr.CopyTarStream(ctx, "/var/run/act", &bytes.Buffer{})

	conn.AssertExpectations(t)
	client.AssertExpectations(t)
}

func TestDockerCopyTarStreamErrorInCopyFiles(t *testing.T) {
	ctx := t.Context()

	conn := &mockConn{}

	merr := fmt.Errorf("Failure")

	client := &mockDockerClient{}
	client.On("CopyToContainer", ctx, "123", "/", mock.Anything, mock.AnythingOfType("container.CopyToContainerOptions")).Return(merr)
	client.On("CopyToContainer", ctx, "123", "/", mock.Anything, mock.AnythingOfType("container.CopyToContainerOptions")).Return(merr)
	cr := &containerReference{
		id:  "123",
		cli: client,
		input: &actcontainer.NewContainerInput{
			Image: "image",
		},
	}

	err := cr.CopyTarStream(ctx, "/var/run/act", &bytes.Buffer{})
	assert.ErrorIs(t, err, merr)

	conn.AssertExpectations(t)
	client.AssertExpectations(t)
}

func TestDockerCopyTarStreamErrorInMkdir(t *testing.T) {
	ctx := t.Context()

	conn := &mockConn{}

	merr := fmt.Errorf("Failure")

	client := &mockDockerClient{}
	client.On("CopyToContainer", ctx, "123", "/", mock.Anything, mock.AnythingOfType("container.CopyToContainerOptions")).Return(nil)
	client.On("CopyToContainer", ctx, "123", "/var/run/act", mock.Anything, mock.AnythingOfType("container.CopyToContainerOptions")).Return(merr)
	cr := &containerReference{
		id:  "123",
		cli: client,
		input: &actcontainer.NewContainerInput{
			Image: "image",
		},
	}

	err := cr.CopyTarStream(ctx, "/var/run/act", &bytes.Buffer{})
	assert.ErrorIs(t, err, merr)

	conn.AssertExpectations(t)
	client.AssertExpectations(t)
}

// Type assert containerReference implements actcontainer.ExecutionsEnvironment
var _ actcontainer.ExecutionsEnvironment = &containerReference{}

func TestCheckVolumes(t *testing.T) {
	testCases := []struct {
		desc          string
		validVolumes  []string
		binds         []string
		expectedBinds []string
	}{
		{
			desc:         "match all volumes",
			validVolumes: []string{"**"},
			binds: []string{
				"shared_volume:/shared_volume",
				"/home/test/data:/test_data",
				"/etc/conf.d/base.json:/config/base.json",
				"sql_data:/sql_data",
				"/secrets/keys:/keys",
			},
			expectedBinds: []string{
				"shared_volume:/shared_volume",
				"/home/test/data:/test_data",
				"/etc/conf.d/base.json:/config/base.json",
				"sql_data:/sql_data",
				"/secrets/keys:/keys",
			},
		},
		{
			desc:         "no volumes can be matched",
			validVolumes: []string{},
			binds: []string{
				"shared_volume:/shared_volume",
				"/home/test/data:/test_data",
				"/etc/conf.d/base.json:/config/base.json",
				"sql_data:/sql_data",
				"/secrets/keys:/keys",
			},
			expectedBinds: []string{},
		},
		{
			desc: "only allowed volumes can be matched",
			validVolumes: []string{
				"shared_volume",
				"/home/test/data",
				"/etc/conf.d/*.json",
			},
			binds: []string{
				"shared_volume:/shared_volume",
				"/home/test/data:/test_data",
				"/etc/conf.d/base.json:/config/base.json",
				"sql_data:/sql_data",
				"/secrets/keys:/keys",
			},
			expectedBinds: []string{
				"shared_volume:/shared_volume",
				"/home/test/data:/test_data",
				"/etc/conf.d/base.json:/config/base.json",
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			logger, _ := test.NewNullLogger()
			ctx := common.WithLogger(t.Context(), logger)
			cr := &containerReference{
				input: &actcontainer.NewContainerInput{
					ValidVolumes: tc.validVolumes,
				},
			}
			_, hostConf := cr.sanitizeConfig(ctx, &container.Config{}, &container.HostConfig{Binds: tc.binds})
			assert.Equal(t, tc.expectedBinds, hostConf.Binds)
		})
	}
}

func TestMergeJobOptions(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		options    string
		config     *container.Config
		hostConfig *container.HostConfig
	}{
		{
			name:    "Ok",
			options: `--volume /frob:/nitz --volume somevolume --tmpfs /tmp:exec,noatime --hostname alternatehost --health-cmd "healthz one"  --health-interval 10s --health-timeout 5s --health-retries 3 --health-start-period 30s`,
			config: &container.Config{
				Volumes:  map[string]struct{}{"somevolume": {}},
				Hostname: "alternatehost",
				Healthcheck: &container.HealthConfig{
					Test:        []string{"CMD-SHELL", "healthz one"},
					Interval:    10 * time.Second,
					Timeout:     5 * time.Second,
					StartPeriod: 30 * time.Second,
					Retries:     3,
				},
			},
			hostConfig: &container.HostConfig{
				Binds: []string{"/frob:/nitz"},
				Tmpfs: map[string]string{"/tmp": "exec,noatime"},
			},
		},
		{
			name:    "DisableHealthCheck",
			options: `--no-healthcheck`,
			config: &container.Config{
				Healthcheck: &container.HealthConfig{
					Test: []string{"NONE"},
				},
			},
			hostConfig: &container.HostConfig{},
		},
		{
			name:       "Ignore",
			options:    "--pid=host --device=/dev/sda",
			config:     &container.Config{},
			hostConfig: &container.HostConfig{},
		},
		{
			name:    "MergeUserAndGroupAdd",
			options: "--user asdf --user root --group-add group1 --group-add wheel --group-add system --group-add wheel --group-add group1",
			config: &container.Config{
				User: "root",
			},
			hostConfig: &container.HostConfig{
				GroupAdd: []string{"group1", "wheel", "system"},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cr := &containerReference{
				input: &actcontainer.NewContainerInput{
					JobOptions: testCase.options,
				},
			}
			config, hostConfig, err := cr.mergeJobOptions(t.Context(), &container.Config{}, &container.HostConfig{})
			require.NoError(t, err)
			assert.EqualValues(t, testCase.config, config)
			assert.EqualValues(t, testCase.hostConfig, hostConfig)
		})
	}
}

func TestDockerRun_isHealthy(t *testing.T) {
	cr := containerReference{
		id: "containerid",
		input: &actcontainer.NewContainerInput{
			NetworkAliases: []string{"servicename"},
		},
	}
	ctx := t.Context()
	makeInspectResponse := func(interval time.Duration, status container.HealthStatus, test []string) container.InspectResponse {
		return container.InspectResponse{
			Config: &container.Config{
				Image: "example.com/some/image",
				Healthcheck: &container.HealthConfig{
					Interval: interval,
					Test:     test,
				},
			},
			ContainerJSONBase: &container.ContainerJSONBase{
				State: &container.State{
					Health: &container.Health{
						Status: status,
					},
				},
			},
		}
	}

	t.Run("IncompleteResponseOrNoHealthCheck", func(t *testing.T) {
		wait, err := cr.isHealthy(ctx, container.InspectResponse{})
		assert.Zero(t, wait)
		assert.NoError(t, err)

		// --no-healthcheck translates into a NONE test command
		resp := makeInspectResponse(0, container.NoHealthcheck, []string{"NONE"})
		wait, err = cr.isHealthy(ctx, resp)
		assert.Zero(t, wait)
		assert.NoError(t, err)
	})

	t.Run("StartingUndefinedIntervalIsNotZero", func(t *testing.T) {
		resp := makeInspectResponse(0, container.Starting, nil)
		wait, err := cr.isHealthy(ctx, resp)
		assert.NotZero(t, wait)
		assert.NoError(t, err)
	})

	t.Run("StartingWithInterval", func(t *testing.T) {
		expectedWait := time.Duration(42)
		resp := makeInspectResponse(expectedWait, container.Starting, nil)
		actualWait, err := cr.isHealthy(ctx, resp)
		assert.Equal(t, expectedWait, actualWait)
		assert.NoError(t, err)
	})

	t.Run("Unhealthy", func(t *testing.T) {
		resp := makeInspectResponse(0, container.Unhealthy, nil)
		wait, err := cr.isHealthy(ctx, resp)
		assert.Zero(t, wait)
		assert.ErrorContains(t, err, "is not healthy")
	})

	t.Run("Healthy", func(t *testing.T) {
		resp := makeInspectResponse(0, container.Healthy, nil)
		wait, err := cr.isHealthy(ctx, resp)
		assert.Zero(t, wait)
		assert.NoError(t, err)
	})

	t.Run("UnknownStatus", func(t *testing.T) {
		resp := makeInspectResponse(0, container.NoHealthcheck, nil)
		wait, err := cr.isHealthy(ctx, resp)
		assert.Zero(t, wait)
		assert.ErrorContains(t, err, "unexpected")
	})
}
