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
			label:    &Label{"label-1", "", ""},
			expected: "label-1",
		},
		{
			name:     "Name and scheme",
			label:    &Label{"label-2", "host", ""},
			expected: "label-2:host",
		},
		{
			name:     "Name and scheme and arg",
			label:    &Label{"label-3", "docker", "//node:lts"},
			expected: "label-3:docker://node:lts",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.expected, testCase.label.String())
		})
	}
}

func TestLabels_Strings(t *testing.T) {
	expected := []string{
		"label-1",
		"label-2:host",
		"label-3:docker://node:lts",
	}

	labels := Labels{
		&Label{"label-1", "", ""},
		&Label{"label-2", "host", ""},
		&Label{"label-3", "docker", "//node:lts"},
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
