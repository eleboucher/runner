package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"code.forgejo.org/forgejo/runner/v12/act/plugin"
	"github.com/sirupsen/logrus"
)

func init() {
	for _, f := range []Factory{
		dockerFactory{},
		hostFactory{},
		lxcFactory{},
		k8sPodFactory{},
	} {
		if err := RegisterFactory(f); err != nil {
			panic(err)
		}
	}
}

type dockerFactory struct{}

func (dockerFactory) GetID() ID { return "docker" }
func (dockerFactory) CreateBackend(_ map[string]any) (Backend, error) {
	return dockerBackend{baseBackend: baseBackend{id: "docker"}}, nil
}

type dockerBackend struct{ baseBackend }

func (dockerBackend) CreateExecutionEnvironment(rc *RunContext, _ *LabelConfiguration) common.Executor {
	return rc.startJobContainer()
}

type hostFactory struct{}

func (hostFactory) GetID() ID { return "host" }
func (hostFactory) CreateBackend(_ map[string]any) (Backend, error) {
	return hostBackend{baseBackend: baseBackend{id: "host"}}, nil
}

type hostBackend struct{ baseBackend }

func (hostBackend) ValidateLabelConfiguration(label string, lc *LabelConfiguration) error {
	if len(lc.Options) > 0 {
		return fmt.Errorf("label %q (host): backend takes no options", label)
	}
	return nil
}

func (hostBackend) CreateExecutionEnvironment(rc *RunContext, _ *LabelConfiguration) common.Executor {
	return rc.startHostEnvironment()
}

type lxcFactory struct{}

func (lxcFactory) GetID() ID { return "lxc" }
func (lxcFactory) CreateBackend(_ map[string]any) (Backend, error) {
	return lxcBackend{baseBackend: baseBackend{id: "lxc"}}, nil
}

type lxcBackend struct{ baseBackend }

func (lxcBackend) ValidateLabelString(label, str string) error {
	// <name>:lxc://<template>[:<release>[:<config>]]
	if !strings.Contains(str, "lxc://") {
		return fmt.Errorf("label %q (lxc): missing template", label)
	}
	return nil
}

func (lxcBackend) CreateExecutionEnvironment(rc *RunContext, _ *LabelConfiguration) common.Executor {
	return rc.startHostEnvironment()
}

type k8sPodFactory struct{}

func (k8sPodFactory) GetID() ID { return "k8spod" }
func (k8sPodFactory) CreateBackend(_ map[string]any) (Backend, error) {
	return k8sPodBackend{baseBackend: baseBackend{id: "k8spod"}}, nil
}

type k8sPodBackend struct{ baseBackend }

func (k8sPodBackend) ValidateLabelString(label, str string) error {
	if !strings.Contains(str, "k8spod://") {
		return fmt.Errorf("label %q (k8spod): missing podspec path", label)
	}
	return nil
}

func (k8sPodBackend) CreateExecutionEnvironment(rc *RunContext, _ *LabelConfiguration) common.Executor {
	return rc.startK8sEnvironment()
}

// pluginBackend caches a *plugin.Client across jobs so the gRPC connection,
// capabilities, and any plugin-side state (kubeconfig, watch caches, etc.)
// outlive a single job. The dialer is provided by the factory; the client
// is created lazily on first job to avoid blocking runner startup when the
// plugin is temporarily unreachable.
type pluginBackend struct {
	baseBackend
	options map[string]string
	dial    func(context.Context) (*plugin.Client, error)

	once   sync.Once
	client *plugin.Client
	err    error
}

func (p *pluginBackend) ensureClient(ctx context.Context) (*plugin.Client, error) {
	p.once.Do(func() {
		p.client, p.err = p.dial(ctx)
	})
	return p.client, p.err
}

func (p *pluginBackend) CreateExecutionEnvironment(rc *RunContext, _ *LabelConfiguration) common.Executor {
	return func(ctx context.Context) error {
		client, err := p.ensureClient(ctx)
		if err != nil {
			return fmt.Errorf("plugin %s: %w", p.id, err)
		}
		return rc.runPluginEnvironment(ctx, string(p.id), client, p.options)
	}
}

func (p *pluginBackend) Close() error {
	if p.client == nil {
		return nil
	}
	return p.client.Close()
}

type pluginV1Factory struct {
	id      ID
	address string
	options map[string]string
}

func (p *pluginV1Factory) GetID() ID { return p.id }
func (p *pluginV1Factory) CreateBackend(_ map[string]any) (Backend, error) {
	id := p.id
	address := p.address
	return &pluginBackend{
		baseBackend: baseBackend{id: id},
		options:     p.options,
		dial: func(ctx context.Context) (*plugin.Client, error) {
			logrus.WithContext(ctx).Infof("\U0001f50c Connecting to plugin %s at %s", id, address)
			// TODO: thread TLS config through PluginConfig.
			return plugin.NewClient(ctx, address, plugin.WithAllowPlainTCP())
		},
	}, nil
}

type pluginV2Factory struct {
	id      ID
	path    string
	options map[string]string
}

func (p *pluginV2Factory) GetID() ID { return p.id }
func (p *pluginV2Factory) CreateBackend(_ map[string]any) (Backend, error) {
	id := p.id
	path := p.path
	return &pluginBackend{
		baseBackend: baseBackend{id: id},
		options:     p.options,
		dial: func(ctx context.Context) (*plugin.Client, error) {
			logrus.WithContext(ctx).Infof("\U0001f50c Launching plugin %s from %s", id, path)
			return plugin.NewClientV2(ctx, path, plugin.WithLogLevel(logrus.GetLevel().String()))
		},
	}, nil
}

// RegisterPluginFactories registers one factory per configured plugin.
// Called once at runner startup, before any job is dispatched.
func RegisterPluginFactories(plugins map[string]PluginConfig, pluginsV2 map[string]PluginV2Config) error {
	for name, cfg := range plugins {
		if err := RegisterFactory(&pluginV1Factory{id: ID(name), address: cfg.Address, options: cfg.Options}); err != nil {
			return err
		}
	}
	for name, cfg := range pluginsV2 {
		if err := RegisterFactory(&pluginV2Factory{id: ID(name), path: cfg.Path, options: cfg.Options}); err != nil {
			return err
		}
	}
	return nil
}

// CloseBackends closes any backend that implements io.Closer (e.g. plugin
// backends). Call once at runner shutdown.
func CloseBackends() error {
	registryMu.Lock()
	defer registryMu.Unlock()
	var firstErr error
	for id, b := range instances {
		closer, ok := b.(interface{ Close() error })
		if !ok {
			continue
		}
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close backend %q: %w", id, err)
		}
	}
	return firstErr
}
