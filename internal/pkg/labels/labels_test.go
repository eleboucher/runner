// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package labels

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLabel_Parse(t *testing.T) {
	tests := []struct {
		args    string
		want    *Label
		wantErr bool
	}{
		{
			args: "label1",
			want: &Label{
				Name:   "label1",
				Schema: SchemeDocker,
				Arg:    ArgDocker,
			},
			wantErr: false,
		},
		{
			args: "label1:docker",
			want: &Label{
				Name:   "label1",
				Schema: SchemeDocker,
				Arg:    ArgDocker,
			},
			wantErr: false,
		},
		{
			args: "label1:docker://node:18",
			want: &Label{
				Name:   "label1",
				Schema: SchemeDocker,
				Arg:    "//node:18",
			},
			wantErr: false,
		},

		{
			args: "label1:lxc",
			want: &Label{
				Name:   "label1",
				Schema: SchemeLXC,
				Arg:    ArgLXC,
			},
			wantErr: false,
		},
		{
			args: "label1:lxc://debian:buster",
			want: &Label{
				Name:   "label1",
				Schema: SchemeLXC,
				Arg:    "//debian:buster",
			},
			wantErr: false,
		},
		{
			args: "label1:host",
			want: &Label{
				Name:   "label1",
				Schema: "host",
				Arg:    "",
			},
			wantErr: false,
		},
		{
			args: "label1:docker://node:18?platform=linux/amd64",
			want: &Label{
				Name:    "label1",
				Schema:  SchemeDocker,
				Arg:     "//node:18",
				Options: map[string]string{OptionPlatform: "linux/amd64"},
			},
			wantErr: false,
		},
		{
			args: "label1:docker?platform=freebsd/amd64",
			want: &Label{
				Name:    "label1",
				Schema:  SchemeDocker,
				Arg:     ArgDocker,
				Options: map[string]string{OptionPlatform: "freebsd/amd64"},
			},
			wantErr: false,
		},
		{
			args:    "label1:host?platform=linux/amd64",
			want:    nil,
			wantErr: true,
		},
		{
			args:    "label1:lxc://debian:buster?platform=linux/amd64",
			want:    nil,
			wantErr: true,
		},
		{
			args:    "label1:docker://node:18?unknown=x",
			want:    nil,
			wantErr: true,
		},
		{
			args:    "label1:host:something",
			want:    nil,
			wantErr: true,
		},
		{
			args:    "label1:invalidscheme",
			want:    nil,
			wantErr: true, // unknown scheme without arg
		},
		{
			args: "k8s-runner:myplugin://some-config",
			want: &Label{
				Name:   "k8s-runner",
				Schema: "myplugin",
				Arg:    "//some-config",
			},
			wantErr: false,
		},
		{
			args:    " label1:lxc://debian:buster",
			want:    nil,
			wantErr: true,
		},
		{
			args:    "label1 :lxc://debian:buster",
			want:    nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			got, err := Parse(tt.args)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, got, tt.want)
		})
	}
}

func TestLabel_MustParse(t *testing.T) {
	t.Run("panics if label is invalid", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("MustParse() did not panic")
			}
		}()

		MustParse(" very invalid ")
	})

	t.Run("accepts valid label", func(t *testing.T) {
		label := MustParse("label1:docker://node:18")

		assert.Equal(t, label.Name, "label1")
		assert.Equal(t, label.Schema, SchemeDocker)
		assert.Equal(t, label.Arg, "//node:18")
	})
}

func TestLabel_String(t *testing.T) {
	testCases := []struct {
		name     string
		label    *Label
		expected string
	}{
		{
			name:     "Name only",
			label:    &Label{Name: "label-1"},
			expected: "label-1",
		},
		{
			name:     "Name and scheme",
			label:    &Label{Name: "label-2", Schema: "host"},
			expected: "label-2:host",
		},
		{
			name:     "Name and scheme and arg",
			label:    &Label{Name: "label-3", Schema: "docker", Arg: "//node:lts"},
			expected: "label-3:docker://node:lts",
		},
		{
			name:     "Name and scheme and arg and platform option",
			label:    &Label{Name: "label-4", Schema: "docker", Arg: "//node:lts", Options: map[string]string{OptionPlatform: "linux/amd64"}},
			expected: "label-4:docker://node:lts?platform=linux%2Famd64",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.expected, testCase.label.String())
		})
	}
}

func TestLabel_StringParseRoundTrip(t *testing.T) {
	for _, str := range []string{
		"label-2:host",
		"label-3:docker://node:lts",
		"label-4:docker://node:lts?platform=linux%2Famd64",
	} {
		t.Run(str, func(t *testing.T) {
			label, err := Parse(str)
			require.NoError(t, err)
			assert.Equal(t, str, label.String())
		})
	}
}

func TestLabels_PickDockerImagePlatform(t *testing.T) {
	ls := Labels{
		MustParse("ubuntu-latest:docker://node:20-bookworm?platform=linux/amd64"),
		MustParse("freebsd-15:docker://ghcr.io/freebsd/freebsd-runtime:15.0?platform=freebsd/amd64"),
		MustParse("plain:docker://node:lts"),
		MustParse("native:host"),
	}

	assert.Equal(t, "linux/amd64", ls.PickDockerImagePlatform([]string{"ubuntu-latest"}))
	assert.Equal(t, "freebsd/amd64", ls.PickDockerImagePlatform([]string{"freebsd-15"}))
	assert.Empty(t, ls.PickDockerImagePlatform([]string{"plain"}))
	assert.Empty(t, ls.PickDockerImagePlatform([]string{"native"}))
	assert.Empty(t, ls.PickDockerImagePlatform([]string{"unknown"}))
}

func TestLabels_Strings(t *testing.T) {
	expected := []string{
		"label-1",
		"label-2:host",
		"label-3:docker://node:lts",
	}

	labels := Labels{
		&Label{Name: "label-1"},
		&Label{Name: "label-2", Schema: "host"},
		&Label{Name: "label-3", Schema: "docker", Arg: "//node:lts"},
	}

	assert.Equal(t, expected, labels.Strings())
}

func TestLabels_PickPlatform_Plugin(t *testing.T) {
	t.Run("plugin scheme routes correctly", func(t *testing.T) {
		labels := Labels{
			{Name: "k8s-runner", Schema: "myplugin", Arg: "//some-config"},
		}
		platform := labels.PickPlatform([]string{"k8s-runner"})
		assert.Equal(t, "myplugin://some-config", platform)
	})

	t.Run("falls back to docker default when no match", func(t *testing.T) {
		labels := Labels{
			{Name: "k8s-runner", Schema: "myplugin", Arg: "//some-config"},
		}
		platform := labels.PickPlatform([]string{"unknown-runner"})
		assert.Equal(t, "node:22-bookworm", platform)
	})
}
