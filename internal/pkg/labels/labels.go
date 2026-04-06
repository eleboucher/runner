// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package labels

import (
	"fmt"
	"strings"
)

const (
	SchemeHost = "host"

	SchemeDocker = "docker"
	ArgDocker    = "//node:22-bookworm"

	SchemeLXC = "lxc"
	ArgLXC    = "//debian:bookworm"

	SchemeK8sPod = "k8spod"
)

type Label struct {
	Name   string
	Schema string
	Arg    string
}

func Parse(str string) (*Label, error) {
	splits := strings.SplitN(str, ":", 3)
	label := &Label{
		Name:   splits[0],
		Schema: "docker",
	}

	if strings.TrimSpace(label.Name) != label.Name {
		return nil, fmt.Errorf("invalid label %q: starting or ending with a space is invalid", label.Name)
	}

	if len(splits) >= 2 {
		label.Schema = splits[1]
	}

	if len(splits) >= 3 {
		if label.Schema == SchemeHost {
			return nil, fmt.Errorf("schema: %s does not have arguments", label.Schema)
		}

		label.Arg = splits[2]
	}
	if label.Arg == "" {
		switch label.Schema {
		case SchemeDocker:
			label.Arg = ArgDocker
		case SchemeLXC:
			label.Arg = ArgLXC
		case SchemeK8sPod:
			return nil, fmt.Errorf("schema %q requires a podspec file path as argument (e.g. \"mylabel:k8spod://path/to/podspec.yaml\")", SchemeK8sPod)
		case SchemeHost:
			// host has no default arg
		default:
			// Plugin schemes require an argument (the plugin address or config reference).
			return nil, fmt.Errorf("schema %q requires an argument (e.g. \"mylabel:%s://arg\")", label.Schema, label.Schema)
		}
	}

	return label, nil
}

// MustParse is like Parse but panics if the string cannot be parsed.
func MustParse(str string) *Label {
	label, err := Parse(str)
	if err != nil {
		panic(`label: Parse(` + str + `): ` + err.Error())
	}
	return label
}

// String returns the string representation of a Label. It is the inverse operation of Parse.
func (l *Label) String() string {
	stringLabel := l.Name
	if l.Schema != "" {
		stringLabel += ":" + l.Schema
		if l.Arg != "" {
			stringLabel += ":" + l.Arg
		}
	}
	return stringLabel
}

type Labels []*Label

func (l Labels) RequireDocker() bool {
	for _, label := range l {
		if label.Schema == SchemeDocker {
			return true
		}
	}
	return false
}

func (l Labels) PickPlatform(runsOn []string) string {
	platforms := make(map[string]string, len(l))
	for _, label := range l {
		switch label.Schema {
		case SchemeDocker:
			// "//" will be ignored
			platforms[label.Name] = strings.TrimPrefix(label.Arg, "//")
		case SchemeHost:
			platforms[label.Name] = "-self-hosted"
		case SchemeLXC:
			platforms[label.Name] = "lxc:" + strings.TrimPrefix(label.Arg, "//")
		case SchemeK8sPod:
			platforms[label.Name] = "k8spod"
		default:
			platforms[label.Name] = label.Schema + ":" + label.Arg
		}
	}
	for _, v := range runsOn {
		if v, ok := platforms[v]; ok {
			return v
		}
	}

	return strings.TrimPrefix(ArgDocker, "//")
}

func (l Labels) Names() []string {
	names := make([]string, 0, len(l))
	for _, label := range l {
		names = append(names, label.Name)
	}
	return names
}

func (l Labels) Strings() []string {
	ls := make([]string, 0, len(l))
	for _, label := range l {
		ls = append(ls, label.String())
	}
	return ls
}
