// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package labels

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	SchemeHost = "host"

	SchemeDocker = "docker"
	ArgDocker    = "//node:22-bookworm"

	SchemeLXC = "lxc"
	ArgLXC    = "//debian:bookworm"

	// OptionPlatform is the name of the label option that sets the OS/architecture passed to the
	// container runtime, e.g. "platform=linux/amd64".
	OptionPlatform = "platform"
)

type Label struct {
	Name   string
	Schema string
	Arg    string
	// Options are encoded in the string form as a trailing query, e.g. "...?platform=linux/amd64".
	Options map[string]string
}

func Parse(str string) (*Label, error) {
	str, options, err := parseOptions(str)
	if err != nil {
		return nil, err
	}

	splits := strings.SplitN(str, ":", 3)
	label := &Label{
		Name:    splits[0],
		Schema:  "docker",
		Options: options,
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
		case SchemeHost:
			// host has no default arg
		default:
			// Plugin schemes require an argument (the plugin address or config reference).
			return nil, fmt.Errorf("schema %q requires an argument (e.g. \"mylabel:%s://arg\")", label.Schema, label.Schema)
		}
	}

	if err := validateOptions(label); err != nil {
		return nil, err
	}

	return label, nil
}

// parseOptions splits off and decodes the trailing "?key=value&..." query of a label string.
// It returns the label string without the query and the decoded options (nil when there is no query).
func parseOptions(str string) (string, map[string]string, error) {
	base, query, found := strings.Cut(str, "?")
	if !found {
		return str, nil, nil
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", nil, fmt.Errorf("invalid label options %q: %w", query, err)
	}
	options := make(map[string]string, len(values))
	for key := range values {
		options[key] = values.Get(key)
	}
	return base, options, nil
}

// validateOptions rejects unknown options and options that do not apply to the label's schema.
func validateOptions(label *Label) error {
	for key := range label.Options {
		switch key {
		case OptionPlatform:
			if label.Schema != SchemeDocker {
				return fmt.Errorf("option %q is only supported with the %q schema", key, SchemeDocker)
			}
		default:
			return fmt.Errorf("unknown label option %q", key)
		}
	}
	return nil
}

// Platform returns the configured OS/architecture for the label, or "" if unset.
func (l *Label) Platform() string {
	return l.Options[OptionPlatform]
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
	if q := encodeOptions(l.Options); q != "" {
		stringLabel += "?" + q
	}
	return stringLabel
}

// encodeOptions renders options as a query string. url.Values.Encode sorts by key, so the
// result is deterministic.
func encodeOptions(options map[string]string) string {
	if len(options) == 0 {
		return ""
	}
	values := make(url.Values, len(options))
	for key, value := range options {
		values.Set(key, value)
	}
	return values.Encode()
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

// PickDockerImagePlatform returns the Docker image platform (e.g. "linux/amd64") configured for the
// first matching docker label in runsOn, or "" when none is set.
func (l Labels) PickDockerImagePlatform(runsOn []string) string {
	platforms := make(map[string]string, len(l))
	for _, label := range l {
		if label.Schema == SchemeDocker {
			platforms[label.Name] = label.Platform()
		}
	}
	for _, v := range runsOn {
		if platform, ok := platforms[v]; ok {
			return platform
		}
	}

	return ""
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
