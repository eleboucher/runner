package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/container"
	"code.forgejo.org/forgejo/runner/v12/act/exprparser"
	"code.forgejo.org/forgejo/runner/v12/act/model"
	"code.forgejo.org/forgejo/runner/v12/testutils"
	"gotest.tools/v3/skip"

	"github.com/docker/go-connections/nat"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v3"
)

func TestRunContext_EvalBool(t *testing.T) {
	var yml yaml.Node
	err := yml.Encode(map[string][]any{
		"os":  {"Linux", "Windows"},
		"foo": {"bar", "baz"},
	})
	assert.NoError(t, err)

	rc := &RunContext{
		Config: &Config{
			Workdir: ".",
		},
		Env: map[string]string{
			"SOMETHING_TRUE":  "true",
			"SOMETHING_FALSE": "false",
			"SOME_TEXT":       "text",
		},
		Run: &model.Run{
			JobID: "job1",
			Workflow: &model.Workflow{
				Name: "test-workflow",
				Jobs: map[string]*model.Job{
					"job1": {
						Strategy: &model.Strategy{
							RawMatrix: yml,
						},
					},
				},
			},
		},
		Matrix: map[string]any{
			"os":  "Linux",
			"foo": "bar",
		},
		StepResults: map[string]*model.StepResult{
			"id1": {
				Conclusion: model.StepStatusSuccess,
				Outcome:    model.StepStatusFailure,
				Outputs: map[string]string{
					"foo": "bar",
				},
			},
		},
	}
	rc.ExprEval = rc.NewExpressionEvaluator(t.Context())

	tables := []struct {
		in      string
		out     bool
		wantErr bool
	}{
		// The basic ones
		{in: "failure()", out: false},
		{in: "success()", out: true},
		{in: "cancelled()", out: false},
		{in: "always()", out: true},
		// TODO: move to sc.NewExpressionEvaluator(), because "steps" context is not available here
		// {in: "steps.id1.conclusion == 'success'", out: true},
		// {in: "steps.id1.conclusion != 'success'", out: false},
		// {in: "steps.id1.outcome == 'failure'", out: true},
		// {in: "steps.id1.outcome != 'failure'", out: false},
		{in: "true", out: true},
		{in: "false", out: false},
		// TODO: This does not throw an error, because the evaluator does not know if the expression is inside ${{ }} or not
		// {in: "!true", wantErr: true},
		// {in: "!false", wantErr: true},
		{in: "1 != 0", out: true},
		{in: "1 != 1", out: false},
		{in: "${{ 1 != 0 }}", out: true},
		{in: "${{ 1 != 1 }}", out: false},
		{in: "1 == 0", out: false},
		{in: "1 == 1", out: true},
		{in: "1 > 2", out: false},
		{in: "1 < 2", out: true},
		// And or
		{in: "true && false", out: false},
		{in: "true && 1 < 2", out: true},
		{in: "false || 1 < 2", out: true},
		{in: "false || false", out: false},
		// None boolable
		{in: "env.UNKNOWN == 'true'", out: false},
		{in: "env.UNKNOWN", out: false},
		// Inline expressions
		{in: "env.SOME_TEXT", out: true},
		{in: "env.SOME_TEXT == 'text'", out: true},
		{in: "env.SOMETHING_TRUE == 'true'", out: true},
		{in: "env.SOMETHING_FALSE == 'true'", out: false},
		{in: "env.SOMETHING_TRUE", out: true},
		{in: "env.SOMETHING_FALSE", out: true},
		// TODO: This does not throw an error, because the evaluator does not know if the expression is inside ${{ }} or not
		// {in: "!env.SOMETHING_TRUE", wantErr: true},
		// {in: "!env.SOMETHING_FALSE", wantErr: true},
		{in: "${{ !env.SOMETHING_TRUE }}", out: false},
		{in: "${{ !env.SOMETHING_FALSE }}", out: false},
		{in: "${{ ! env.SOMETHING_TRUE }}", out: false},
		{in: "${{ ! env.SOMETHING_FALSE }}", out: false},
		{in: "${{ env.SOMETHING_TRUE }}", out: true},
		{in: "${{ env.SOMETHING_FALSE }}", out: true},
		{in: "${{ !env.SOMETHING_TRUE }}", out: false},
		{in: "${{ !env.SOMETHING_FALSE }}", out: false},
		{in: "${{ !env.SOMETHING_TRUE && true }}", out: false},
		{in: "${{ !env.SOMETHING_FALSE && true }}", out: false},
		{in: "${{ !env.SOMETHING_TRUE || true }}", out: true},
		{in: "${{ !env.SOMETHING_FALSE || false }}", out: false},
		{in: "${{ env.SOMETHING_TRUE && true }}", out: true},
		{in: "${{ env.SOMETHING_FALSE || true }}", out: true},
		{in: "${{ env.SOMETHING_FALSE || false }}", out: true},
		// TODO: This does not throw an error, because the evaluator does not know if the expression is inside ${{ }} or not
		// {in: "!env.SOMETHING_TRUE || true", wantErr: true},
		{in: "${{ env.SOMETHING_TRUE == 'true'}}", out: true},
		{in: "${{ env.SOMETHING_FALSE == 'true'}}", out: false},
		{in: "${{ env.SOMETHING_FALSE == 'false'}}", out: true},
		{in: "${{ env.SOMETHING_FALSE }} && ${{ env.SOMETHING_TRUE }}", out: true},

		// All together now
		{in: "false || env.SOMETHING_TRUE == 'true'", out: true},
		{in: "true || env.SOMETHING_FALSE == 'true'", out: true},
		{in: "true && env.SOMETHING_TRUE == 'true'", out: true},
		{in: "false && env.SOMETHING_TRUE == 'true'", out: false},
		{in: "env.SOMETHING_FALSE == 'true' && env.SOMETHING_TRUE == 'true'", out: false},
		{in: "env.SOMETHING_FALSE == 'true' && true", out: false},
		{in: "${{ env.SOMETHING_FALSE == 'true' }} && true", out: true},
		{in: "true && ${{ env.SOMETHING_FALSE == 'true' }}", out: true},
		// Check github context
		{in: "github.actor == 'nektos/act'", out: true},
		{in: "github.actor == 'unknown'", out: false},
		{in: "github.job == 'job1'", out: true},
		// The special ACT flag
		{in: "${{ env.ACT }}", out: true},
		{in: "${{ !env.ACT }}", out: false},
		// Invalid expressions should be reported
		{in: "INVALID_EXPRESSION", wantErr: true},
	}

	for _, table := range tables {
		t.Run(table.in, func(t *testing.T) {
			assertObject := assert.New(t)
			b, err := EvalBool(t.Context(), rc.ExprEval, table.in, exprparser.DefaultStatusCheckSuccess)
			if table.wantErr {
				assertObject.Error(err)
			}

			assertObject.Equal(table.out, b, fmt.Sprintf("Expected %s to be %v, was %v", table.in, table.out, b))
		})
	}
}

func TestRunContext_GetBindsAndMounts(t *testing.T) {
	rctemplate := &RunContext{
		Name: "TestRCName",
		Run: &model.Run{
			Workflow: &model.Workflow{
				Name: "TestWorkflowName",
			},
		},
		Config: &Config{
			BindWorkdir: false,
		},
	}

	tests := []struct {
		windowsPath bool
		name        string
		rc          *RunContext
		wantbind    string
		wantmount   string
	}{
		{false, "/mnt/linux", rctemplate, "/mnt/linux", "/mnt/linux"},
		{false, "/mnt/path with spaces/linux", rctemplate, "/mnt/path with spaces/linux", "/mnt/path with spaces/linux"},
		{true, "C:\\Users\\TestPath\\MyTestPath", rctemplate, "/mnt/c/Users/TestPath/MyTestPath", "/mnt/c/Users/TestPath/MyTestPath"},
		{true, "C:\\Users\\Test Path with Spaces\\MyTestPath", rctemplate, "/mnt/c/Users/Test Path with Spaces/MyTestPath", "/mnt/c/Users/Test Path with Spaces/MyTestPath"},
		{true, "/LinuxPathOnWindowsShouldFail", rctemplate, "", ""},
	}

	isWindows := runtime.GOOS == "windows"

	for _, testcase := range tests {
		for _, bindWorkDir := range []bool{true, false} {
			testBindSuffix := ""
			if bindWorkDir {
				testBindSuffix = "Bind"
			}

			// Only run windows path tests on windows and non-windows on non-windows
			if (isWindows && testcase.windowsPath) || (!isWindows && !testcase.windowsPath) {
				t.Run((testcase.name + testBindSuffix), func(t *testing.T) {
					config := testcase.rc.Config
					config.Workdir = testcase.name
					config.BindWorkdir = bindWorkDir
					gotbind, gotmount, _ := rctemplate.GetBindsAndMounts(t.Context())

					// Name binds/mounts are either/or
					if config.BindWorkdir {
						fullBind := testcase.name + ":" + testcase.wantbind
						if runtime.GOOS == "darwin" {
							fullBind += ":delegated"
						}
						assert.Contains(t, gotbind, fullBind)
					} else {
						mountkey := testcase.rc.getInternalVolumeWorkdir(t.Context())
						assert.EqualValues(t, testcase.wantmount, gotmount[mountkey])
					}
				})
			}
		}
	}

	t.Run("ContainerVolumeMountTest", func(t *testing.T) {
		type PlatformRestriction int

		const (
			WindowsOnly    PlatformRestriction = iota // Shall run only on Windows
			NonWindowsOnly                            // Shall run only on non-Windows
			Always                                    // Shall always run, regardless of OS
		)

		tests := []struct {
			platform  PlatformRestriction
			name      string
			volumes   []string
			wantbind  string
			wantmount map[string]string
		}{
			{Always, "BindAnonymousVolume", []string{"/volume"}, "/volume", map[string]string{}},
			{NonWindowsOnly, "BindHostFile", []string{"/path/to/file/on/host:/volume"}, "/path/to/file/on/host:/volume", map[string]string{}},
			{WindowsOnly, "BindWindowsHostFile", []string{"C:\\path\\to\\file\\on\\host:/volume"}, "C:\\path\\to\\file\\on\\host:/volume", map[string]string{}},
			{Always, "MountExistingVolume", []string{"volume-id:/volume"}, "", map[string]string{"volume-id": "/volume"}},
		}

		for _, testcase := range tests {
			t.Run(testcase.name, func(t *testing.T) {
				skip.If(t, isWindows && testcase.platform == NonWindowsOnly)
				skip.If(t, !isWindows && testcase.platform == WindowsOnly)

				job := &model.Job{}
				err := job.RawContainer.Encode(map[string][]string{
					"volumes": testcase.volumes,
				})
				assert.NoError(t, err)

				rc := &RunContext{
					Name: "TestRCName",
					Run: &model.Run{
						JobID: "test",
						Workflow: &model.Workflow{
							Name: "TestWorkflowName",
							Jobs: map[string]*model.Job{
								"test": {},
							},
						},
					},
					Config: &Config{
						BindWorkdir: false,
					},
				}
				rc.ExprEval = rc.NewExpressionEvaluator(t.Context())
				rc.Run.JobID = "job1"
				rc.Run.Workflow.Jobs = map[string]*model.Job{"job1": job}

				gotbind, gotmount, _ := rc.GetBindsAndMounts(t.Context())

				if len(testcase.wantbind) > 0 {
					assert.Contains(t, gotbind, testcase.wantbind)
				}

				for k, v := range testcase.wantmount {
					assert.Contains(t, gotmount, k)
					assert.Equal(t, gotmount[k], v)
				}
			})
		}
	})
}

func TestRunContext_GetGithubContextURL(t *testing.T) {
	table := []struct {
		instance  string
		serverURL string
		APIURL    string
	}{
		{instance: "", serverURL: "", APIURL: "/api/v1"},
		{instance: "example.com", serverURL: "https://example.com", APIURL: "https://example.com/api/v1"},
		{instance: "http://example.com", serverURL: "http://example.com", APIURL: "http://example.com/api/v1"},
		{instance: "https://example.com", serverURL: "https://example.com", APIURL: "https://example.com/api/v1"},
	}
	for _, data := range table {
		t.Run(data.instance, func(t *testing.T) {
			rc := &RunContext{
				EventJSON: "{}",
				Config: &Config{
					GitHubInstance: data.instance,
				},
				Run: &model.Run{
					Workflow: &model.Workflow{
						Name: "GitHubContextTest",
					},
				},
			}

			ghc := rc.getGithubContext(t.Context())

			assert.Equal(t, data.serverURL, ghc.ServerURL)
			assert.Equal(t, data.APIURL, ghc.APIURL)
		})
	}
}

func TestRunContext_GetGithubContextRef(t *testing.T) {
	table := []struct {
		event string
		json  string
		ref   string
	}{
		{event: "push", json: `{"ref":"0000000000000000000000000000000000000000"}`, ref: "0000000000000000000000000000000000000000"},
		{event: "create", json: `{"ref":"0000000000000000000000000000000000000000"}`, ref: "0000000000000000000000000000000000000000"},
		{event: "workflow_dispatch", json: `{"ref":"0000000000000000000000000000000000000000"}`, ref: "0000000000000000000000000000000000000000"},
		{event: "delete", json: `{"repository":{"default_branch": "main"}}`, ref: "refs/heads/main"},
		{event: "pull_request", json: `{"number":123}`, ref: "refs/pull/123/merge"},
		{event: "pull_request_review", json: `{"number":123}`, ref: "refs/pull/123/merge"},
		{event: "pull_request_review_comment", json: `{"number":123}`, ref: "refs/pull/123/merge"},
		{event: "pull_request_target", json: `{"pull_request":{"base":{"ref": "main"}}}`, ref: "refs/heads/main"},
		{event: "deployment", json: `{"deployment": {"ref": "tag-name"}}`, ref: "tag-name"},
		{event: "deployment_status", json: `{"deployment": {"ref": "tag-name"}}`, ref: "tag-name"},
		{event: "release", json: `{"release": {"tag_name": "tag-name"}}`, ref: "refs/tags/tag-name"},
	}

	for _, data := range table {
		t.Run(data.event, func(t *testing.T) {
			rc := &RunContext{
				EventJSON: data.json,
				Config: &Config{
					EventName: data.event,
					Workdir:   "",
				},
				Run: &model.Run{
					Workflow: &model.Workflow{
						Name: "GitHubContextTest",
					},
				},
			}

			ghc := rc.getGithubContext(t.Context())

			assert.Equal(t, data.ref, ghc.Ref)
		})
	}
}

func createIfTestRunContext(jobs map[string]*model.Job) *RunContext {
	rc := &RunContext{
		Config: &Config{
			Workdir: ".",
			Platforms: map[string]string{
				"ubuntu-latest": "ubuntu-latest",
			},
		},
		Env: map[string]string{},
		Run: &model.Run{
			JobID: "job1",
			Workflow: &model.Workflow{
				Name: "test-workflow",
				Jobs: jobs,
			},
		},
	}
	rc.ExprEval = rc.NewExpressionEvaluator(context.Background())

	return rc
}

func createJob(t *testing.T, input, result string) *model.Job {
	var job *model.Job
	err := yaml.Unmarshal([]byte(input), &job)
	assert.NoError(t, err)
	job.Result = result

	return job
}

func TestRunContext_RunsOnPlatformNames(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assertObject := assert.New(t)

	rc := createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, ""),
	})
	assertObject.Equal([]string{"ubuntu-latest"}, rc.runsOnPlatformNames(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ${{ 'ubuntu-latest' }}`, ""),
	})
	assertObject.Equal([]string{"ubuntu-latest"}, rc.runsOnPlatformNames(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: [self-hosted, my-runner]`, ""),
	})
	assertObject.Equal([]string{"self-hosted", "my-runner"}, rc.runsOnPlatformNames(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: [self-hosted, "${{ 'my-runner' }}"]`, ""),
	})
	assertObject.Equal([]string{"self-hosted", "my-runner"}, rc.runsOnPlatformNames(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ${{ fromJSON('["ubuntu-latest"]') }}`, ""),
	})
	assertObject.Equal([]string{"ubuntu-latest"}, rc.runsOnPlatformNames(t.Context()))

	// test missing / invalid runs-on
	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `name: something`, ""),
	})
	assertObject.Equal([]string{}, rc.runsOnPlatformNames(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on:
  mapping: value`, ""),
	})
	assertObject.Equal([]string{}, rc.runsOnPlatformNames(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ${{ invalid expression }}`, ""),
	})
	assertObject.Equal([]string{}, rc.runsOnPlatformNames(t.Context()))
}

func TestRunContext_IsEnabled(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assertObject := assert.New(t)

	// success()
	rc := createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest
if: success()`, ""),
	})
	assertObject.True(rc.isEnabled(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, "failure"),
		"job2": createJob(t, `runs-on: ubuntu-latest
needs: [job1]
if: success()`, ""),
	})
	rc.Run.JobID = "job2"
	assertObject.False(rc.isEnabled(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, "success"),
		"job2": createJob(t, `runs-on: ubuntu-latest
needs: [job1]
if: success()`, ""),
	})
	rc.Run.JobID = "job2"
	assertObject.True(rc.isEnabled(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, "failure"),
		"job2": createJob(t, `runs-on: ubuntu-latest
if: success()`, ""),
	})
	rc.Run.JobID = "job2"
	assertObject.True(rc.isEnabled(t.Context()))

	// failure()
	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest
if: failure()`, ""),
	})
	assertObject.False(rc.isEnabled(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, "failure"),
		"job2": createJob(t, `runs-on: ubuntu-latest
needs: [job1]
if: failure()`, ""),
	})
	rc.Run.JobID = "job2"
	assertObject.True(rc.isEnabled(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, "success"),
		"job2": createJob(t, `runs-on: ubuntu-latest
needs: [job1]
if: failure()`, ""),
	})
	rc.Run.JobID = "job2"
	assertObject.False(rc.isEnabled(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, "failure"),
		"job2": createJob(t, `runs-on: ubuntu-latest
if: failure()`, ""),
	})
	rc.Run.JobID = "job2"
	assertObject.False(rc.isEnabled(t.Context()))

	// always()
	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest
if: always()`, ""),
	})
	assertObject.True(rc.isEnabled(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, "failure"),
		"job2": createJob(t, `runs-on: ubuntu-latest
needs: [job1]
if: always()`, ""),
	})
	rc.Run.JobID = "job2"
	assertObject.True(rc.isEnabled(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, "success"),
		"job2": createJob(t, `runs-on: ubuntu-latest
needs: [job1]
if: always()`, ""),
	})
	rc.Run.JobID = "job2"
	assertObject.True(rc.isEnabled(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, "success"),
		"job2": createJob(t, `runs-on: ubuntu-latest
if: always()`, ""),
	})
	rc.Run.JobID = "job2"
	assertObject.True(rc.isEnabled(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `uses: ./.github/workflows/reusable.yml`, ""),
	})
	assertObject.True(rc.isEnabled(t.Context()))

	rc = createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `uses: ./.github/workflows/reusable.yml
if: false`, ""),
	})
	assertObject.False(rc.isEnabled(t.Context()))
}

func TestRunContext_GetEnv(t *testing.T) {
	tests := []struct {
		description string
		rc          *RunContext
		targetEnv   string
		want        string
	}{
		{
			description: "Env from Config should overwrite",
			rc: &RunContext{
				Config: &Config{
					Env: map[string]string{"OVERWRITTEN": "true"},
				},
				Run: &model.Run{
					Workflow: &model.Workflow{
						Jobs: map[string]*model.Job{"test": {Name: "test"}},
						Env:  map[string]string{"OVERWRITTEN": "false"},
					},
					JobID: "test",
				},
			},
			targetEnv: "OVERWRITTEN",
			want:      "true",
		},
		{
			description: "No overwrite occurs",
			rc: &RunContext{
				Config: &Config{
					Env: map[string]string{"SOME_OTHER_VAR": "true"},
				},
				Run: &model.Run{
					Workflow: &model.Workflow{
						Jobs: map[string]*model.Job{"test": {Name: "test"}},
						Env:  map[string]string{"OVERWRITTEN": "false"},
					},
					JobID: "test",
				},
			},
			targetEnv: "OVERWRITTEN",
			want:      "false",
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			envMap := test.rc.GetEnv()
			assert.EqualValues(t, test.want, envMap[test.targetEnv])
		})
	}
}

func TestRunContext_CreateSimpleContainerName(t *testing.T) {
	tests := []struct {
		parts []string
		want  string
	}{
		{
			parts: []string{"a--a", "BB正", "c-C"},
			want:  "a-a_BB_c-C",
		},
		{
			parts: []string{"a-a", "", "-"},
			want:  "a-a",
		},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.parts, " "), func(t *testing.T) {
			assert.Equalf(t, tt.want, createSimpleContainerName(tt.parts...), "createSimpleContainerName(%v)", tt.parts)
		})
	}
}

func TestRunContext_ensureNetworkName(t *testing.T) {
	t.Run("CreateNetworkForServices", func(t *testing.T) {
		yaml := `
on:
  push:

jobs:
  job:
    runs-on: docker
    container:
      image: some:image
    services:
      service1:
        image: service1:image
    steps:
      - run: echo ok
`
		workflow, err := model.ReadWorkflow(strings.NewReader(yaml), true)
		require.NoError(t, err)

		rc := &RunContext{
			Config: &Config{
				ContainerNetworkMode: "host",
			},
			Run: &model.Run{
				JobID:    "job",
				Workflow: workflow,
			},
		}

		rc.ensureNetworkName(t.Context())
		assert.False(t, rc.getNetworkCreated(t.Context()))
		assert.Equal(t, "host", rc.getNetworkName(t.Context()))
	})

	yaml := `
on:
  push:

jobs:
  job:
    runs-on: docker
    container:
      image: some:image
    steps:
      - run: echo ok
`
	workflow, err := model.ReadWorkflow(strings.NewReader(yaml), true)
	require.NoError(t, err)

	run := &model.Run{
		JobID:    "job",
		Workflow: workflow,
	}

	t.Run("CreateNetworkIfEmptyNetworkMode", func(t *testing.T) {
		rc := &RunContext{
			Config: &Config{
				ContainerNetworkMode: "",
			},
			Run: run,
		}

		rc.ensureNetworkName(t.Context())
		assert.True(t, rc.getNetworkCreated(t.Context()))
		assert.True(t, strings.HasPrefix(rc.getNetworkName(t.Context()), "WORKFLOW-"), rc.getNetworkName(t.Context()))
	})

	t.Run("FixedNetworkIfSetByNetworkMode", func(t *testing.T) {
		rc := &RunContext{
			Config: &Config{
				ContainerNetworkMode: "host",
			},
			Run: run,
		}

		rc.ensureNetworkName(t.Context())
		assert.False(t, rc.getNetworkCreated(t.Context()))
		assert.Equal(t, "host", rc.getNetworkName(t.Context()))
	})
}

func TestRunContext_SanitizeNetworkAlias(t *testing.T) {
	same := "same"
	assert.Equal(t, same, sanitizeNetworkAlias(t.Context(), same))
	original := "or.igin'A-L"
	sanitized := "or_igin_a-l"
	assert.Equal(t, sanitized, sanitizeNetworkAlias(t.Context(), original))
}

func TestRunContext_PrepareJobContainer(t *testing.T) {
	skip.If(t, runtime.GOOS != "linux") // Windows and macOS cannot natively run Linux containers
	yaml := `
on:
  push:

jobs:
  job:
    runs-on: docker
    container:
      image: some:image
      credentials:
        username: containerusername
        password: containerpassword
    services:
      service1:
        image: service1:image
        credentials:
          username: service1username
          password: service1password
      service2:
        image: service2:image
        credentials:
          username: service2username
          password: service2password
    steps:
      - run: echo ok
`
	workflow, err := model.ReadWorkflow(strings.NewReader(yaml), true)
	require.NoError(t, err)

	testCases := []struct {
		name   string
		step   actionStep
		inputs []container.NewContainerInput
	}{
		{
			name: "Overlapping",
			step: &stepActionRemote{
				Step: &model.Step{
					Uses: "org/repo/path@ref",
				},
				RunContext: &RunContext{
					Config: &Config{
						Workdir: "/my/workdir",
					},
					Run: &model.Run{
						JobID:    "job",
						Workflow: workflow,
					},
				},
				env: map[string]string{},
			},
			inputs: []container.NewContainerInput{
				{
					Name:            "WORKFLOW-8a5edab282632443219e051e4ade2d1d5bbc671c781051bf1437897cbdfea0f1_JOB",
					Image:           "some:image",
					Username:        "containerusername",
					Password:        "containerpassword",
					Entrypoint:      []string{"tail", "-f", "/dev/null"},
					Cmd:             nil,
					WorkingDir:      "/my/workdir",
					Env:             []string{},
					ToolCache:       "/opt/hostedtoolcache",
					Binds:           []string{"/var/run/docker.sock:/var/run/docker.sock"},
					Privileged:      false,
					UsernsMode:      "",
					DefaultPlatform: "",
					NetworkAliases:  []string{""},
					ExposedPorts:    nil,
					PortBindings:    nil,
					ConfigOptions:   "",
					JobOptions:      "",
				},
				{
					Name:            "WORKFLOW-8a5edab282632443219e051e4ade2d1d5bbc671c781051bf143789-d083efaebdcab24d231fa091b85dbb8768b47136582b340132c197d9cb5e7430",
					Image:           "service1:image",
					Username:        "service1username",
					Password:        "service1password",
					Entrypoint:      nil,
					Cmd:             []string{},
					WorkingDir:      "",
					Env:             []string{},
					ToolCache:       "/opt/hostedtoolcache",
					Binds:           []string{"/var/run/docker.sock:/var/run/docker.sock"},
					Privileged:      false,
					UsernsMode:      "",
					DefaultPlatform: "",
					NetworkAliases:  []string{"service1"},
					ExposedPorts:    nat.PortSet{},
					PortBindings:    nat.PortMap{},
					ConfigOptions:   "",
					JobOptions:      "",
				},
				{
					Name:            "WORKFLOW-8a5edab282632443219e051e4ade2d1d5bbc671c781051bf143789-4c44cd5731ec445ebe33780a3e39ed5e20e80f8a697a6e34c7acdd3675e631a8",
					Image:           "service2:image",
					Username:        "service2username",
					Password:        "service2password",
					Entrypoint:      nil,
					Cmd:             []string{},
					WorkingDir:      "",
					Env:             []string{},
					ToolCache:       "/opt/hostedtoolcache",
					Binds:           []string{"/var/run/docker.sock:/var/run/docker.sock"},
					Privileged:      false,
					UsernsMode:      "",
					DefaultPlatform: "",
					NetworkAliases:  []string{"service2"},
					ExposedPorts:    nat.PortSet{},
					PortBindings:    nat.PortMap{},
					ConfigOptions:   "",
					JobOptions:      "",
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			containerInputs := make([]container.NewContainerInput, 0, 5)
			newContainer := container.NewContainer
			defer testutils.MockVariable(&container.NewContainer, func(input *container.NewContainerInput) container.ExecutionsEnvironment {
				c := *input
				c.Stdout = nil
				c.Stderr = nil
				c.Env = []string{}
				containerInputs = append(containerInputs, c)
				return newContainer(input)
			})()

			ctx := t.Context()
			rc := testCase.step.getRunContext()
			rc.ExprEval = rc.NewExpressionEvaluator(ctx)

			require.NoError(t, rc.prepareJobContainer(ctx))
			slices.SortFunc(containerInputs, func(a, b container.NewContainerInput) int { return cmp.Compare(a.Username, b.Username) })
			jobContainerInput := containerInputs[0]
			require.Equal(t, "containerusername", jobContainerInput.Username)
			require.NotEmpty(t, jobContainerInput.NetworkMode)
			for source := range jobContainerInput.Mounts {
				assert.Contains(t, jobContainerInput.ValidVolumes, source)
			}
			for i := 0; i < len(containerInputs); i++ {

				assert.Equal(t, jobContainerInput.NetworkMode, containerInputs[i].NetworkMode, containerInputs[i].Username)
				containerInputs[i].NetworkMode = ""

				if strings.HasPrefix(containerInputs[i].Username, "service") {
					assert.Empty(t, containerInputs[i].Mounts)
					assert.Empty(t, containerInputs[i].ValidVolumes)
				}
				containerInputs[i].Mounts = nil
				containerInputs[i].ValidVolumes = nil

				assert.EqualValues(t, testCase.inputs[i], containerInputs[i], containerInputs[i].Username)
			}
		})
	}
}

type waitForServiceContainerMock struct {
	mock.Mock
	container.Container
	container.LinuxContainerEnvironmentExtensions
}

func (o *waitForServiceContainerMock) IsHealthy(ctx context.Context) (time.Duration, error) {
	args := o.Called(ctx)
	return args.Get(0).(time.Duration), args.Error(1)
}

func Test_waitForServiceContainer(t *testing.T) {
	t.Run("Wait", func(t *testing.T) {
		m := &waitForServiceContainerMock{}
		ctx := t.Context()
		mock.InOrder(
			m.On("IsHealthy", ctx).Return(1*time.Millisecond, nil).Once(),
			m.On("IsHealthy", ctx).Return(time.Duration(0), nil).Once(),
		)
		require.NoError(t, waitForServiceContainer(ctx, m))
		m.AssertExpectations(t)
	})

	t.Run("Cancel", func(t *testing.T) {
		m := &waitForServiceContainerMock{}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		m.On("IsHealthy", ctx).Return(1*time.Millisecond, nil).Once()
		require.NoError(t, waitForServiceContainer(ctx, m))
		m.AssertExpectations(t)
	})

	t.Run("Error", func(t *testing.T) {
		m := &waitForServiceContainerMock{}
		ctx := t.Context()
		m.On("IsHealthy", ctx).Return(time.Duration(0), errors.New("ERROR"))
		require.ErrorContains(t, waitForServiceContainer(ctx, m), "ERROR")
		m.AssertExpectations(t)
	})
}

func TestRunContext_ensureRandomName(t *testing.T) {
	parent := &RunContext{
		Name: "parentname",
	}
	rc := &RunContext{
		Name:   "runname",
		Parent: parent,
	}

	parent.ensureRandomName(t.Context())
	assert.NotEmpty(t, parent.randomName)
	rc.ensureRandomName(t.Context())
	assert.Equal(t, parent.randomName, rc.randomName)
}

func TestRunContext_IsK8sEnv(t *testing.T) {
	newRC := func(t *testing.T, platformPicker func([]string) string, runsOn string) *RunContext {
		t.Helper()
		yamlStr := fmt.Sprintf(`
on: push
jobs:
  job:
    runs-on: %s
    steps:
      - run: echo ok
`, runsOn)
		workflow, err := model.ReadWorkflow(strings.NewReader(yamlStr), true)
		require.NoError(t, err)
		rc := &RunContext{
			Config: &Config{
				PlatformPicker: platformPicker,
			},
			Run: &model.Run{
				JobID:    "job",
				Workflow: workflow,
			},
			Env: map[string]string{},
		}
		rc.ExprEval = rc.NewExpressionEvaluator(t.Context())
		return rc
	}

	t.Run("k8spod platform returns true", func(t *testing.T) {
		picker := func(labels []string) string {
			if slices.Contains(labels, "k8s-runner") {
				return "k8spod"
			}
			return ""
		}
		rc := newRC(t, picker, "k8s-runner")
		assert.True(t, rc.IsK8sEnv(t.Context()))
	})

	t.Run("docker platform returns false", func(t *testing.T) {
		picker := func(labels []string) string {
			return "node:22-bookworm"
		}
		rc := newRC(t, picker, "ubuntu-latest")
		assert.False(t, rc.IsK8sEnv(t.Context()))
	})

	t.Run("host platform returns false", func(t *testing.T) {
		picker := func(labels []string) string {
			return "-self-hosted"
		}
		rc := newRC(t, picker, "self-hosted")
		assert.False(t, rc.IsK8sEnv(t.Context()))
	})
}
