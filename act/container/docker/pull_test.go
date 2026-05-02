package docker

import (
	"testing"

	"github.com/docker/cli/cli/config"

	log "github.com/sirupsen/logrus"
	assert "github.com/stretchr/testify/assert"
)

func init() {
	log.SetLevel(log.DebugLevel)
}

func TestCleanImage(t *testing.T) {
	tables := []struct {
		imageIn  string
		imageOut string
	}{
		{"myhost.com/foo/bar", "myhost.com/foo/bar"},
		{"localhost:8000/canonical/ubuntu", "localhost:8000/canonical/ubuntu"},
		{"localhost/canonical/ubuntu:latest", "localhost/canonical/ubuntu:latest"},
		{"localhost:8000/canonical/ubuntu:latest", "localhost:8000/canonical/ubuntu:latest"},
		{"ubuntu", "docker.io/library/ubuntu"},
		{"ubuntu:18.04", "docker.io/library/ubuntu:18.04"},
		{"cibuilds/hugo:0.53", "docker.io/cibuilds/hugo:0.53"},
	}

	for _, table := range tables {
		imageOut := cleanImage(t.Context(), table.imageIn)
		assert.Equal(t, table.imageOut, imageOut)
	}
}

func TestGetImagePullOptions(t *testing.T) {
	ctx := t.Context()

	config.SetDir("/non-existent/docker")

	options, err := getImagePullOptions(ctx, NewDockerPullExecutorInput{})
	assert.Nil(t, err, "Failed to create ImagePullOptions")
	assert.Equal(t, "", options.RegistryAuth, "RegistryAuth should be empty if no username or password is set")

	options, err = getImagePullOptions(ctx, NewDockerPullExecutorInput{
		Image:    "",
		Username: "username",
		Password: "password",
	})
	assert.Nil(t, err, "Failed to create ImagePullOptions")
	assert.Equal(t, "eyJ1c2VybmFtZSI6InVzZXJuYW1lIiwicGFzc3dvcmQiOiJwYXNzd29yZCJ9", options.RegistryAuth, "Username and Password should be provided")

	config.SetDir("testdata/docker-pull-options")

	options, err = getImagePullOptions(ctx, NewDockerPullExecutorInput{
		Image: "nektos/act",
	})
	assert.Nil(t, err, "Failed to create ImagePullOptions")
	assert.Equal(t, "eyJ1c2VybmFtZSI6InVzZXJuYW1lIiwicGFzc3dvcmQiOiJwYXNzd29yZFxuIiwic2VydmVyYWRkcmVzcyI6Imh0dHBzOi8vaW5kZXguZG9ja2VyLmlvL3YxLyJ9", options.RegistryAuth, "RegistryAuth should be taken from local docker config")
}

func TestIsPinnedImage(t *testing.T) {
	testCases := []struct {
		image  string
		pinned bool
	}{
		{"", false},
		{"alpine:3.23", false},
		{"alpine:latest", false},
		{"docker.io/library/alpine:latest", false},
		{"docker.io/library/alpine:3.23", false},
		{"alpine@sha256:5b10f432ef3da1b8d4c7eb6c487f2f5a8f096bc91145e68878dd4a5019afde11", true},
		{"alpine@sha256:5B10F432EF3DA1B8D4C7EB6C487F2F5A8F096BC91145E68878DD4A5019AFDE11", true},
		{"alpine@5b10f432ef3da1b8d4c7eb6c487f2f5a8f096bc91145e68878dd4a5019afde11", false},
		{"alpine@sha256:5b10f432ef3da1b8d4c7eb6c487f2f5a8f096bc91145e68878dd4a5019afde1", false},
		{"alpine:3.23@sha256:5b10f432ef3da1b8d4c7eb6c487f2f5a8f096bc91145e68878dd4a5019afde11", true},
		{"docker.io/library/alpine@sha256:5b10f432ef3da1b8d4c7eb6c487f2f5a8f096bc91145e68878dd4a5019afde11", true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.image, func(t *testing.T) {
			assert.Equal(t, testCase.pinned, isPinnedImage(testCase.image))
		})
	}
}
