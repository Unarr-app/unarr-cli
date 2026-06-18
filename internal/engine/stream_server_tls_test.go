package engine

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"
)

// genSelfSignedCert builds an in-memory self-signed cert valid for 127.0.0.1,
// used to exercise the agent's HTTPS listener without any CA/ACME plumbing.
func genSelfSignedCert(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "unarr-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}

// freePort grabs an ephemeral TCP port and releases it, so the caller can hand
// a concrete port number to EnableTLS (which treats 0 as "disabled").
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

// TestStreamServerTLS_HotInstall verifies the HTTPS listener: it starts even
// with no certificate (handshake fails), and a certificate installed *after*
// Listen applies live via the GetCertificate path — no restart, which is what
// the future ACME broker relies on.
func TestStreamServerTLS_HotInstall(t *testing.T) {
	cert, leaf := genSelfSignedCert(t)

	ss := NewStreamServer(0, 1) // HTTP on a random free port
	ss.EnableTLS(freePort(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := ss.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ss.Shutdown(context.Background())

	if ss.HTTPSPort() == 0 {
		t.Fatal("HTTPSPort() = 0, want the armed HTTPS port")
	}
	if ss.HasTLSCertificate() {
		t.Fatal("no certificate should be installed yet")
	}

	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	url := fmt.Sprintf("https://127.0.0.1:%d/health", ss.HTTPSPort())

	// Before a cert is installed, the handshake must fail.
	if resp, err := client.Get(url); err == nil {
		resp.Body.Close()
		t.Fatal("GET succeeded before a certificate was installed; want handshake failure")
	}

	// Install the cert — the listener stays up and the next handshake succeeds.
	ss.SetTLSCertificate(&cert)
	if !ss.HasTLSCertificate() {
		t.Fatal("HasTLSCertificate() = false after install")
	}

	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
		}
		return // success
	}
	t.Fatalf("GET %s never succeeded after cert install: %v", url, lastErr)
}

// TestStreamServerTLS_Disabled verifies that with TLS not armed, no HTTPS port
// is opened and the HTTP listener is unaffected.
func TestStreamServerTLS_Disabled(t *testing.T) {
	ss := NewStreamServer(0, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := ss.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ss.Shutdown(context.Background())

	if ss.HTTPSPort() != 0 {
		t.Errorf("HTTPSPort() = %d, want 0 (TLS disabled)", ss.HTTPSPort())
	}
}

// TestLoadTLSCertificateFromFiles_Missing verifies the loader reports an error
// (not a panic) when the cert pair is absent — the daemon treats this as
// "TLS off, HTTP keeps serving".
func TestLoadTLSCertificateFromFiles_Missing(t *testing.T) {
	ss := NewStreamServer(0, 1)
	err := ss.LoadTLSCertificateFromFiles(
		t.TempDir()+"/nope.crt", t.TempDir()+"/nope.key")
	if err == nil {
		t.Fatal("expected error loading a missing cert pair")
	}
	if ss.HasTLSCertificate() {
		t.Error("no certificate should be installed after a failed load")
	}
}
