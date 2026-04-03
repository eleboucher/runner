package container

import (
	"archive/tar"
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"code.forgejo.org/forgejo/runner/v12/act/common"
)

func ParseEnvFile(e Container, srcPath string, env *map[string]string) common.Executor {
	localEnv := *env
	return func(ctx context.Context) error {
		envTar, err := e.GetContainerArchive(ctx, srcPath)
		if err != nil {
			return nil
		}
		defer envTar.Close()
		reader := tar.NewReader(envTar)
		_, err = reader.Next()
		if err != nil && err != io.EOF {
			return err
		}

		// ParseEnvFile is used to parse an action's outputs into $FORGEJO_ENV, $FORGEJO_OUTPUTS, and $FORGEJO_STATE.
		// The limits described here are currently based upon defined limits for OUTPUTS, but seem reasonable for the
		// other usages as well.
		//
		// Max output value length 1MB in Forgejo:
		// https://codeberg.org/forgejo/forgejo/src/commit/b11ceeecaaf1b4d8e052a606d673bd68a5365b4c/routers/api/actions/runner/runner.go#L210-L213
		maxValueSize := 1024 * 1024
		// Max key length 255 in Forgejo:
		// https://codeberg.org/forgejo/forgejo/src/commit/b11ceeecaaf1b4d8e052a606d673bd68a5365b4c/routers/api/actions/runner/runner.go#L205-L208
		maxKeySize := 255

		s := bufio.NewScanner(reader)
		// Provide the scanner a moderate 1KB buffer. Scanner needs a maximum memory allocation to scan for newlines,
		// which it can grow the buffer to -- you'd have to have a single-line output at the max key size and max value
		// size to exceed this buffer length. +1 for the "=", and +1 for the newline.
		s.Buffer(make([]byte, 0, 1024), 2+maxKeySize+maxValueSize)

		for s.Scan() {
			line := s.Text()
			singleLineEnv := strings.Index(line, "=")
			multiLineEnv := strings.Index(line, "<<")
			if singleLineEnv != -1 && (multiLineEnv == -1 || singleLineEnv < multiLineEnv) {
				localEnv[line[:singleLineEnv]] = line[singleLineEnv+1:]
			} else if multiLineEnv != -1 {
				multiLineEnvContent := ""
				multiLineEnvDelimiter := line[multiLineEnv+2:]
				delimiterFound := false
				for s.Scan() {
					content := s.Text()
					if content == multiLineEnvDelimiter {
						delimiterFound = true
						break
					}
					if multiLineEnvContent != "" {
						multiLineEnvContent += "\n"
					}
					multiLineEnvContent += content
				}
				if !delimiterFound {
					return fmt.Errorf("invalid format delimiter '%v' not found before end of file", multiLineEnvDelimiter)
				}
				localEnv[line[:multiLineEnv]] = multiLineEnvContent
			} else {
				return fmt.Errorf("invalid format '%v', expected a line with '=' or '<<'", line)
			}
		}

		if err := s.Err(); err != nil {
			logger := common.Logger(ctx)
			logger.Errorf("Failed to parse file %q: %s", srcPath, err.Error())
			return fmt.Errorf("scanning file %q failed: %w", srcPath, err)
		}

		env = &localEnv
		return nil
	}
}
