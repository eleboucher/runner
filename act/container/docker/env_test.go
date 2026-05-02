//go:build !WITHOUT_DOCKER && (linux || darwin || windows || freebsd || openbsd)

package docker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"code.forgejo.org/forgejo/runner/v12/act/container"
)

// closeTrackingClient records whether Close was invoked on the underlying
// client. It embeds client.APIClient so it satisfies the interface without
// having to stub every method.
type closeTrackingClient struct {
	client.APIClient
	closed bool
}

func (c *closeTrackingClient) Close() error {
	c.closed = true
	return nil
}

func TestWithClientRoundtrip(t *testing.T) {
	ctx := context.Background()
	assert.Nil(t, clientFromCtx(ctx), "no client in bare ctx")

	cli := &closeTrackingClient{}
	ctx2 := WithClient(ctx, cli)
	assert.Same(t, cli, clientFromCtx(ctx2))

	// nil cli must not stomp existing key
	ctx3 := WithClient(ctx2, nil)
	assert.Same(t, cli, clientFromCtx(ctx3))
}

func TestGetDockerClientReturnsInjected(t *testing.T) {
	cli := &closeTrackingClient{}
	ctx := WithClient(context.Background(), cli)

	got, err := GetDockerClient(ctx)
	require.NoError(t, err)

	// Returned client must be a borrowedClient wrapping the injected cli.
	bc, ok := got.(borrowedClient)
	require.True(t, ok, "expected borrowedClient, got %T", got)
	assert.Same(t, cli, bc.APIClient)

	// Close on the borrowed wrapper must NOT close the underlying client.
	require.NoError(t, got.Close())
	assert.False(t, cli.closed, "borrowedClient.Close must be a no-op")
}

func TestTLSConfigHTTPClientBytes(t *testing.T) {
	caPEM, certPEM, keyPEM := generateTestCerts(t)

	cases := []struct {
		name string
		tls  *TLSConfig
	}{
		{"ca only", &TLSConfig{CA: caPEM}},
		{"keypair only", &TLSConfig{Cert: certPEM, Key: keyPEM}},
		{"ca + keypair", &TLSConfig{CA: caPEM, Cert: certPEM, Key: keyPEM}},
		{"insecure skip verify", &TLSConfig{InsecureSkipVerify: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hc, err := tc.tls.httpClient()
			require.NoError(t, err)
			require.NotNil(t, hc)
			require.NotNil(t, hc.Transport)
		})
	}
}

func TestTLSConfigInvalidCA(t *testing.T) {
	_, err := (&TLSConfig{CA: []byte("not a pem block")}).httpClient()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid CA bytes")
}

func TestTLSConfigCertWithoutKey(t *testing.T) {
	_, certPEM, _ := generateTestCerts(t)
	_, err := (&TLSConfig{Cert: certPEM}).httpClient()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Cert and Key must be set together")
}

func TestTLSConfigInvalidKeypair(t *testing.T) {
	_, certPEM, _ := generateTestCerts(t)
	_, err := (&TLSConfig{Cert: certPEM, Key: []byte("not a key")}).httpClient()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid keypair")
}

func TestEnvCloseIdempotent(t *testing.T) {
	cli := &closeTrackingClient{}
	d := &Env{cli: cli}

	require.NoError(t, d.Close())
	assert.True(t, cli.closed)

	// second close on a torn-down env is a no-op
	require.NoError(t, d.Close())
}

func TestEnvContextInjectsClient(t *testing.T) {
	cli := &closeTrackingClient{}
	d := &Env{cli: cli}

	ctx := d.Context(context.Background())
	got, err := GetDockerClient(ctx)
	require.NoError(t, err)
	bc, ok := got.(borrowedClient)
	require.True(t, ok)
	assert.Same(t, cli, bc.APIClient)
}

func TestEnvNewContainerPrewiresClient(t *testing.T) {
	cli := &closeTrackingClient{}
	d := &Env{cli: cli}

	env := d.NewContainer(&container.NewContainerInput{Image: "alpine"})
	cr, ok := env.(*containerReference)
	require.True(t, ok)
	bc, ok := cr.cli.(borrowedClient)
	require.True(t, ok, "containerReference.cli should be borrowedClient")
	assert.Same(t, cli, bc.APIClient)

	// containerReference.Close must not tear down the env-owned client.
	require.NoError(t, cr.Close()(context.Background()))
	assert.False(t, cli.closed, "container Close must not close env-owned client")
}

func TestOpenRejectsSSHWithTLS(t *testing.T) {
	_, err := Open(context.Background(), "ssh://user@host", &TLSConfig{InsecureSkipVerify: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TLS is not supported with ssh://")
}

// generateTestCerts produces a self-signed CA-style cert + matching keypair
// PEM-encoded for use in TLS config tests. P-256 ECDSA for speed.
func generateTestCerts(t *testing.T) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	caPEM = certPEM // self-signed: cert is its own CA

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return caPEM, certPEM, keyPEM
}
