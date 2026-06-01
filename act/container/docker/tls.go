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
	"github.com/docker/docker/client"
)

// TLSConfig carries PEM-encoded TLS material for dialing a remote Docker
// daemon. Cert and Key must be set together; an empty TLSConfig is plaintext.
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
	tr := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	}
	tr.TLSClientConfig = cfg
	return &http.Client{Transport: tr}, nil
}

// Open dials the daemon at endpoint with the supplied TLS material and returns
// an Endpoint. Unlike NewEndpoint it can attach PEM TLS material, which is what
// a delegating plug-in delivers over RPC. An empty endpoint and nil tlsConf
// resolve the daemon from the process environment (DOCKER_HOST, default socket,
// ssh:// shortcut). TLS on an ssh:// endpoint is rejected because connhelper
// provides its own transport.
func Open(ctx context.Context, endpoint string, tlsConf *TLSConfig) (Endpoint, error) {
	if endpoint == "" && tlsConf == nil {
		return NewEndpoint(ctx, "")
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
	return NewEndpointFromClient(ctx, cli)
}
