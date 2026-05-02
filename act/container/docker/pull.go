//go:build !WITHOUT_DOCKER && (linux || darwin || windows || freebsd || openbsd)

package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"

	"code.forgejo.org/forgejo/runner/v12/act/common"
)

// NewDockerPullExecutor function to create a run executor for the container
func NewDockerPullExecutor(ep Endpoint, input NewDockerPullExecutorInput) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)

		if input.Platform == "" {
			return errors.New("docker pull input.Platform not specified")
		}

		if common.Dryrun(ctx) {
			logger.Infof("%sdocker pull image=%s platform=%s username=%s forcePull=%t", logPrefix, input.Image,
				input.Platform, input.Username, input.ForcePull)
			return nil
		}

		imageExists, err := ImageExistsLocally(ctx, ep, input.Image, input.Platform)
		logger.Debugf("Image exists? %v", imageExists)
		if err != nil {
			return fmt.Errorf("unable to determine if image already exists for image '%s' (%s): %w", input.Image, input.Platform, err)
		}

		if imageExists && isPinnedImage(input.Image) {
			return nil
		}
		if imageExists && !input.ForcePull {
			return nil
		}

		imageRef := cleanImage(ctx, input.Image)
		logger.Debugf("pulling image '%v' (%s)", imageRef, input.Platform)

		cli := ep.Client()

		imagePullOptions, err := getImagePullOptions(ctx, input)
		if err != nil {
			return err
		}

		logger.Infof("%sdocker pull image=%s platform=%s username=%s forcePull=%t", logPrefix, input.Image,
			input.Platform, input.Username, input.ForcePull)

		reader, err := cli.ImagePull(ctx, imageRef, imagePullOptions)

		_ = logDockerResponse(logger, reader, err != nil)
		if err != nil {
			if imagePullOptions.RegistryAuth != "" && strings.Contains(err.Error(), "unauthorized") {
				logger.Errorf("pulling image '%v' (%s) failed with credentials %s retrying without them, please check for stale docker config files", imageRef, input.Platform, err.Error())
				imagePullOptions.RegistryAuth = ""
				reader, err = cli.ImagePull(ctx, imageRef, imagePullOptions)

				_ = logDockerResponse(logger, reader, err != nil)
			} else if imageExists {
				logger.Warnf("%sCould not update image %s, continuing with outdated local copy; error: %s",
					logPrefix, input.Image, err)
				return nil
			}
			return err
		}
		return nil
	}
}

func getImagePullOptions(ctx context.Context, input NewDockerPullExecutorInput) (image.PullOptions, error) {
	imagePullOptions := image.PullOptions{
		Platform: input.Platform,
	}
	logger := common.Logger(ctx)

	if input.Username != "" && input.Password != "" {
		logger.Debugf("using authentication for docker pull")

		authConfig := registry.AuthConfig{
			Username: input.Username,
			Password: input.Password,
		}

		encodedJSON, err := json.Marshal(authConfig)
		if err != nil {
			return imagePullOptions, err
		}

		imagePullOptions.RegistryAuth = base64.URLEncoding.EncodeToString(encodedJSON)
	} else {
		authConfig, err := LoadDockerAuthConfig(ctx, input.Image)
		if err != nil {
			return imagePullOptions, err
		}
		if authConfig.Username == "" && authConfig.Password == "" {
			return imagePullOptions, nil
		}
		logger.Info("using DockerAuthConfig authentication for docker pull")

		encodedJSON, err := json.Marshal(authConfig)
		if err != nil {
			return imagePullOptions, err
		}

		imagePullOptions.RegistryAuth = base64.URLEncoding.EncodeToString(encodedJSON)
	}

	return imagePullOptions, nil
}

func cleanImage(ctx context.Context, image string) string {
	ref, err := reference.ParseAnyReference(image)
	if err != nil {
		common.Logger(ctx).Error(err)
		return ""
	}

	return ref.String()
}

var pinnedImagePattern = regexp.MustCompile(`(?i)@sha256:[a-z0-9]{64}$`)

func isPinnedImage(image string) bool {
	return pinnedImagePattern.MatchString(image)
}
