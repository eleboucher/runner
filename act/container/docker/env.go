//go:build !WITHOUT_DOCKER && (linux || darwin || windows || freebsd || openbsd)

package docker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/docker/cli/cli/connhelper"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"

	"code.forgejo.org/forgejo/runner/v12/act/common"
	"code.forgejo.org/forgejo/runner/v12/act/container"
)

// TLSConfig carries PEM-encoded TLS material for dialing a remote Docker
// daemon. Bytes form is what gets delivered over RPC by a delegating plug-in.
// Cert and Key must be set together. An empty TLSConfig is plaintext.
type TLSConfig struct {
	CA                 []byte
	Cert               []byte
	Key                []byte
	InsecureSkipVerify bool
}

func (t *TLSConfig) httpClient() (*http.Client, error) {
	cfg := &tls.Config{InsecureSkipVerify: t.InsecureSkipVerify} //nolint:gosec
	if len(t.CA) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(t.CA) {
			return nil, errors.New("docker TLS: invalid CA bytes")
		}
		cfg.RootCAs = pool
	}
	if len(t.Cert) > 0 || len(t.Key) > 0 {
		if len(t.Cert) == 0 || len(t.Key) == 0 {
			return nil, errors.New("docker TLS: Cert and Key must be set together")
		}
		pair, err := tls.X509KeyPair(t.Cert, t.Key)
		if err != nil {
			return nil, fmt.Errorf("docker TLS: invalid keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = cfg
	return &http.Client{Transport: tr}, nil
}

// Env is a reusable Docker back-end bound to a specific daemon. The caller
// owns the lifetime: invoke Close when done. Factory methods inject the owned
// client into the executor context so package-level helpers reuse the
// connection instead of dialing the daemon themselves.
type Env struct {
	cli client.APIClient
}

// Open dials the daemon at endpoint with the supplied TLS material. An empty
// endpoint resolves the daemon from the process environment (DOCKER_HOST,
// default unix socket, ssh:// shortcut). TLS on an ssh:// endpoint is
// rejected because connhelper provides its own transport.
func Open(ctx context.Context, endpoint string, tlsConf *TLSConfig) (*Env, error) {
	if endpoint == "" && tlsConf == nil {
		cli, err := GetDockerClient(ctx)
		if err != nil {
			return nil, err
		}
		return &Env{cli: cli}, nil
	}

	opts := []client.Opt{}
	switch {
	case endpoint == "":
		opts = append(opts, client.FromEnv)
	case strings.HasPrefix(endpoint, "ssh://"):
		if tlsConf != nil {
			return nil, errors.New("docker: TLS is not supported with ssh:// endpoints")
		}
		helper, err := connhelper.GetConnectionHelper(endpoint)
		if err != nil {
			return nil, fmt.Errorf("docker ssh helper: %w", err)
		}
		opts = append(opts, client.WithHost(helper.Host), client.WithDialContext(helper.Dialer))
	default:
		opts = append(opts, client.WithHost(endpoint))
	}
	if tlsConf != nil {
		hc, err := tlsConf.httpClient()
		if err != nil {
			return nil, err
		}
		opts = append(opts, client.WithHTTPClient(hc))
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to docker daemon: %w", err)
	}
	cli.NegotiateAPIVersion(ctx)
	return &Env{cli: cli}, nil
}

// OpenFromEnv constructs an Env from the process environment.
func OpenFromEnv(ctx context.Context) (*Env, error) {
	return Open(ctx, "", nil)
}

// Close releases the underlying client. The Env must not be reused after Close.
func (d *Env) Close() error {
	if d == nil || d.cli == nil {
		return nil
	}
	err := d.cli.Close()
	d.cli = nil
	return err
}

func (d *Env) Context(ctx context.Context) context.Context {
	return WithClient(ctx, d.cli)
}

// NewContainer returns an ExecutionsEnvironment bound to this Env. The
// container's Close executor does not tear down the env-owned client.
func (d *Env) NewContainer(input *container.NewContainerInput) container.ExecutionsEnvironment {
	cenv := NewContainer(input)
	if cr, ok := cenv.(*containerReference); ok {
		cr.cli = borrowedClient{d.cli}
	}
	return cenv
}

func (d *Env) NewPullExecutor(input NewDockerPullExecutorInput) common.Executor {
	return d.wrap(NewDockerPullExecutor(input))
}

func (d *Env) NewBuildExecutor(input NewDockerBuildExecutorInput) common.Executor {
	return d.wrap(NewDockerBuildExecutor(input))
}

func (d *Env) NewNetworkCreateExecutor(name string, config *network.CreateOptions) common.Executor {
	return d.wrap(NewDockerNetworkCreateExecutor(name, config))
}

func (d *Env) NewNetworkRemoveExecutor(name string) common.Executor {
	return d.wrap(NewDockerNetworkRemoveExecutor(name))
}

func (d *Env) NewVolumesRemoveExecutor(volumeNames []string) common.Executor {
	return d.wrap(NewDockerVolumesRemoveExecutor(volumeNames))
}

func (d *Env) ImageExistsLocally(ctx context.Context, image, platform string) (bool, error) {
	return ImageExistsLocally(d.Context(ctx), image, platform)
}

func (d *Env) RemoveImage(ctx context.Context, image string, force, pruneChildren bool) (bool, error) {
	return RemoveImage(d.Context(ctx), image, force, pruneChildren)
}

func (d *Env) HostInfo(ctx context.Context) (system.Info, error) {
	return GetHostInfo(d.Context(ctx))
}

func (d *Env) RunnerArch(ctx context.Context) string {
	return RunnerArch(d.Context(ctx))
}

func (d *Env) wrap(exec common.Executor) common.Executor {
	return func(ctx context.Context) error {
		return exec(d.Context(ctx))
	}
}

type ctxClientKey struct{}

// WithClient returns a context carrying cli for retrieval by GetDockerClient.
// The client is borrowed: GetDockerClient returns a wrapper whose Close is a
// no-op, so package-level helpers' `defer cli.Close()` does not tear down a
// shared connection.
func WithClient(ctx context.Context, cli client.APIClient) context.Context {
	if cli == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxClientKey{}, cli)
}

func clientFromCtx(ctx context.Context) client.APIClient {
	cli, _ := ctx.Value(ctxClientKey{}).(client.APIClient)
	return cli
}

type borrowedClient struct {
	client.APIClient
}

func (borrowedClient) Close() error { return nil }
