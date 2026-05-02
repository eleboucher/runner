package docker

import (
	"io"
	"runtime"
	"testing"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gotest.tools/v3/skip"
)

func init() {
	log.SetLevel(log.DebugLevel)
}

func TestImageExistsLocally(t *testing.T) {
	skip.If(t, runtime.GOOS != "linux") // Windows and macOS cannot natively run Linux containers
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := t.Context()
	// to help make this test reliable and not flaky, we need to have
	// an image that will exist, and one that won't exist

	// Test if image exists with specific tag
	invalidImageTag, err := ImageExistsLocally(ctx, "code.forgejo.org/oci/alpine:this-random-tag-will-never-exist", "linux/amd64")
	assert.Nil(t, err)
	assert.Equal(t, false, invalidImageTag)

	// Test if image exists with specific architecture (image platform)
	invalidImagePlatform, err := ImageExistsLocally(ctx, "code.forgejo.org/oci/alpine:latest", "windows/amd64")
	assert.Nil(t, err)
	assert.Equal(t, false, invalidImagePlatform)

	// pull an image
	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.Nil(t, err)
	cli.NegotiateAPIVersion(t.Context())

	// Chose alpine latest because it's so small
	// maybe we should build an image instead so that tests aren't reliable on dockerhub
	readerDefault, err := cli.ImagePull(ctx, "code.forgejo.org/oci/alpine:latest", image.PullOptions{
		Platform: "linux/amd64",
	})
	assert.Nil(t, err)
	defer readerDefault.Close()
	_, err = io.ReadAll(readerDefault)
	assert.Nil(t, err)

	imageDefaultArchExists, err := ImageExistsLocally(ctx, "code.forgejo.org/oci/alpine:latest", "linux/amd64")
	assert.Nil(t, err)
	assert.Equal(t, true, imageDefaultArchExists)

	// Validate if another architecture platform can be pulled
	readerArm64, err := cli.ImagePull(ctx, "code.forgejo.org/oci/alpine:latest", image.PullOptions{
		Platform: "linux/arm64",
	})
	assert.Nil(t, err)
	defer readerArm64.Close()
	_, err = io.ReadAll(readerArm64)
	assert.Nil(t, err)

	imageArm64Exists, err := ImageExistsLocally(ctx, "code.forgejo.org/oci/alpine:latest", "linux/arm64")
	assert.Nil(t, err)
	assert.Equal(t, true, imageArm64Exists)
}

func TestParsePlatform(t *testing.T) {
	tests := []struct {
		input  string
		output v1.Platform
	}{
		{
			input: "linux/amd64",
			output: v1.Platform{
				Architecture: "amd64",
				OS:           "linux",
			},
		},
	}
	for _, tc := range tests {
		plat, err := parsePlatform(tc.input)
		require.NoError(t, err)
		assert.Equal(t, tc.output.Architecture, plat.Architecture)
		assert.Equal(t, tc.output.OS, plat.OS)
	}
}
