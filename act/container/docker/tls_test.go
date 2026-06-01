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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
