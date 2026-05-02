package container

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestParseEnvFile(t *testing.T) {
	data255b := strings.Builder{}
	for range 255 {
		data255b.Write([]byte("A"))
	}
	data1kb := strings.Builder{}
	for range 1024 {
		data1kb.Write([]byte("A"))
	}
	data65kb := strings.Builder{}
	for range 65 {
		data65kb.Write([]byte(data1kb.String()))
	}
	data1Mb := strings.Builder{}
	for range 1024 {
		data1Mb.Write([]byte(data1kb.String()))
	}

	testCases := []struct {
		name        string
		rawContents string
		mapContents map[string]string
		errContains string
	}{
		{
			name:        "simple single value",
			rawContents: "abc=123\n",
			mapContents: map[string]string{"abc": "123"},
		},
		{
			name:        "65kB value",
			rawContents: fmt.Sprintf("abc=%s\n", data65kb.String()),
			mapContents: map[string]string{"abc": data65kb.String()},
		},
		{
			name:        "max value length",
			rawContents: fmt.Sprintf("abc=%s\n", data1Mb.String()),
			mapContents: map[string]string{"abc": data1Mb.String()},
		},
		{
			name:        "max key length",
			rawContents: fmt.Sprintf("%s=123\n", data255b.String()),
			mapContents: map[string]string{data255b.String(): "123"},
		},
		{
			name:        "max key & value length",
			rawContents: fmt.Sprintf("%s=%s\n", data255b.String(), data1Mb.String()),
			mapContents: map[string]string{data255b.String(): data1Mb.String()},
		},
		{
			name:        "max key & value length exceeded",
			rawContents: fmt.Sprintf("%s_abc=%s_abc\n", data255b.String(), data1Mb.String()),
			mapContents: map[string]string{fmt.Sprintf("%s_abc", data255b.String()): fmt.Sprintf("%s_abc", data1Mb.String())},
			errContains: "token too long",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tarfile := bytes.Buffer{}
			writer := tar.NewWriter(&tarfile)

			body := []byte(tc.rawContents)
			hdr := &tar.Header{
				Name: "output.txt",
				Mode: 0o600,
				Size: int64(len(body)),
			}
			require.NoError(t, writer.WriteHeader(hdr))
			_, err := writer.Write(body)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			reader := io.NopCloser(bytes.NewReader(tarfile.Bytes()))
			container := NewMockContainer(t)
			container.On("GetContainerArchive", mock.Anything, "src-path").Return(reader, nil)

			env := make(map[string]string)
			executor := ParseEnvFile(container, "src-path", &env)

			err = executor(t.Context())
			if tc.errContains == "" {
				require.NoError(t, err)
				assert.EqualValues(t, tc.mapContents, env)
			} else {
				assert.ErrorContains(t, err, tc.errContains)
			}
		})
	}
}
