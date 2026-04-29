package runner

import (
	"context"

	"code.forgejo.org/forgejo/runner/v12/act/common"
)

type BackendFactory interface {
	Name() string
	Match(ctx context.Context, rc *RunContext) bool
	CreateExecutionEnvironment(rc *RunContext) common.Executor
}

var backends []BackendFactory

func RegisterBackend(f BackendFactory) {
	backends = append(backends, f)
}

func pickBackend(ctx context.Context, rc *RunContext) BackendFactory {
	for _, f := range backends {
		if f.Match(ctx, rc) {
			return f
		}
	}
	return nil
}

// Built-in order: specific matchers first, docker last as catch-all.
// Plugin factories insert before docker via RegisterPluginBackend.
func init() {
	RegisterBackend(hostBackend{})
	RegisterBackend(k8sBackend{})
	RegisterBackend(dockerBackend{})
}

type hostBackend struct{}

func (hostBackend) Name() string                                              { return "host" }
func (hostBackend) Match(ctx context.Context, rc *RunContext) bool            { return rc.IsHostEnv(ctx) }
func (hostBackend) CreateExecutionEnvironment(rc *RunContext) common.Executor { return rc.startHostEnvironment() }

type k8sBackend struct{}

func (k8sBackend) Name() string                                              { return "k8s" }
func (k8sBackend) Match(ctx context.Context, rc *RunContext) bool            { return rc.IsK8sEnv(ctx) }
func (k8sBackend) CreateExecutionEnvironment(rc *RunContext) common.Executor { return rc.startK8sEnvironment() }

type dockerBackend struct{}

func (dockerBackend) Name() string                                              { return "docker" }
func (dockerBackend) Match(ctx context.Context, rc *RunContext) bool            { return true }
func (dockerBackend) CreateExecutionEnvironment(rc *RunContext) common.Executor { return rc.startJobContainer() }

type pluginBackend struct{ name string }

func (p pluginBackend) Name() string                                   { return p.name }
func (p pluginBackend) Match(ctx context.Context, rc *RunContext) bool { return rc.pluginName(ctx) == p.name }
func (p pluginBackend) CreateExecutionEnvironment(rc *RunContext) common.Executor {
	return rc.startPluginEnvironment(p.name)
}

func RegisterPluginBackend(name string) {
	for _, b := range backends {
		if b.Name() == name {
			return
		}
	}
	// Insert before docker (always last).
	backends = append(backends[:len(backends)-1], pluginBackend{name: name}, backends[len(backends)-1])
}
