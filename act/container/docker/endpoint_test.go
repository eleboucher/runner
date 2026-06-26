//go:build !WITHOUT_DOCKER && (linux || darwin || windows || freebsd || openbsd)

package docker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewEndpointHonoursTLSEnv(t *testing.T) {
	var sawTLS, sawClientCert bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawTLS = r.TLS != nil
		sawClientCert = r.TLS != nil && len(r.TLS.PeerCertificates) > 0
		_, _ = w.Write([]byte("{}")) // any valid JSON satisfies cli.Info()
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	defer srv.Close()

	certDir := t.TempDir()
	writeClientCerts(t, certDir)
	// Trust the server's self-signed cert so DOCKER_TLS_VERIFY can verify it.
	require.NoError(t, os.WriteFile(filepath.Join(certDir, "ca.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))

	host := "tcp://" + strings.TrimPrefix(srv.URL, "https://")
	t.Setenv("DOCKER_HOST", host)
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", certDir)
	t.Setenv("DOCKER_API_VERSION", "1.45") // skip version negotiation against the stub

	ep, err := NewEndpoint(t.Context(), os.Getenv("DOCKER_HOST"))
	require.NoError(t, err, "DOCKER_TLS_VERIFY/DOCKER_CERT_PATH were ignored: plain HTTP hit the TLS endpoint")
	defer ep.Close()

	require.True(t, sawTLS, "connection was not made over TLS")
	require.True(t, sawClientCert, "client certificate from DOCKER_CERT_PATH was not presented")
}

// TestOpenHonoursTLSEnvForEndpoint checks Open's env-TLS fallback: an explicit
// endpoint with no PEM (tlsConf == nil) still honours DOCKER_TLS_VERIFY and
// DOCKER_CERT_PATH.
func TestOpenHonoursTLSEnvForEndpoint(t *testing.T) {
	var sawTLS, sawClientCert bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawTLS = r.TLS != nil
		sawClientCert = r.TLS != nil && len(r.TLS.PeerCertificates) > 0
		_, _ = w.Write([]byte("{}")) // any valid JSON satisfies cli.Info()
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	defer srv.Close()

	certDir := t.TempDir()
	writeClientCerts(t, certDir)
	require.NoError(t, os.WriteFile(filepath.Join(certDir, "ca.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))

	host := "tcp://" + strings.TrimPrefix(srv.URL, "https://")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", certDir)
	t.Setenv("DOCKER_API_VERSION", "1.45") // skip version negotiation against the stub

	ep, err := Open(t.Context(), host, nil)
	require.NoError(t, err, "DOCKER_TLS_VERIFY/DOCKER_CERT_PATH were ignored: plain HTTP hit the TLS endpoint")
	defer ep.Close()

	require.True(t, sawTLS, "connection was not made over TLS")
	require.True(t, sawClientCert, "client certificate from DOCKER_CERT_PATH was not presented")
}

// writeClientCerts writes a throwaway cert.pem/key.pem into dir.
func writeClientCerts(t *testing.T, dir string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "runner-test-client"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31-1, 0),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "cert.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "key.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
}
