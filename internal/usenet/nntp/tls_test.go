package nntp

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// testCA is a throwaway certificate authority for a local TLS NNTP listener.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "nntp test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// leaf issues a server certificate valid for exactly the given names/IPs.
func (ca *testCA) leaf(t *testing.T, dns []string, ips []net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "nntp test server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// serveTLSNNTP runs a minimal TLS NNTP greeter on loopback and returns its port.
func serveTLSNNTP(t *testing.T, cert tls.Certificate) int {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go greet(conn)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func greet(conn net.Conn) {
	defer conn.Close()
	if _, err := conn.Write([]byte("200 tls nntp ready\r\n")); err != nil {
		return // client rejected the handshake
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if strings.HasPrefix(strings.ToUpper(line), "QUIT") {
			_, _ = conn.Write([]byte("205 bye\r\n"))
			return
		}
		_, _ = conn.Write([]byte("500 unknown\r\n"))
	}
}

func tlsClient(port int, serverName string, roots *x509.CertPool) *Client {
	c := NewClient(Config{Host: "127.0.0.1", Port: port, SSL: true, TLSServerName: serverName, MaxConnections: 2})
	c.rootCAs = roots
	return c
}

func connect(t *testing.T, c *Client) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = c.Close() })
	return c.Connect(ctx)
}

var loopbackIP = []net.IP{net.ParseIP("127.0.0.1")}

// The 2026-09-10 incident: the override names a host the certificate no longer
// covers, while the certificate is valid for the host actually dialled.
func TestTLSServerNameOverrideMismatchFallsBackToHost(t *testing.T) {
	ca := newTestCA(t)
	port := serveTLSNNTP(t, ca.leaf(t, []string{"localhost"}, loopbackIP))

	c := tlsClient(port, "xsnews.nl", ca.pool)
	if err := connect(t, c); err != nil {
		t.Fatalf("connect with stale override: %v", err)
	}
	if c.ActiveConnections() != 2 {
		t.Fatalf("ActiveConnections = %d, want 2", c.ActiveConnections())
	}
	if !c.tlsOverrideStale.Load() {
		t.Fatal("fallback not remembered: every later dial would pay a failed handshake")
	}
}

// An override the certificate DOES cover is used as-is, with no fallback.
func TestTLSServerNameOverrideMatchingIsUsed(t *testing.T) {
	ca := newTestCA(t)
	port := serveTLSNNTP(t, ca.leaf(t, []string{"xsnews.nl"}, nil))

	c := tlsClient(port, "xsnews.nl", ca.pool)
	if err := connect(t, c); err != nil {
		t.Fatalf("connect with matching override: %v", err)
	}
	if c.tlsOverrideStale.Load() {
		t.Fatal("fell back although the override matched")
	}
}

// A certificate valid for neither the override nor the host must still fail:
// the fallback is a second strict check, never a relaxation.
func TestTLSCertificateMatchingNeitherNameFails(t *testing.T) {
	ca := newTestCA(t)
	port := serveTLSNNTP(t, ca.leaf(t, []string{"reader.example"}, nil))

	c := tlsClient(port, "xsnews.nl", ca.pool)
	err := connect(t, c)
	if err == nil {
		t.Fatal("connected to a server whose certificate matches neither name")
	}
	if !isHostnameMismatch(err) {
		t.Fatalf("err = %v, want a hostname verification error", err)
	}
}

// An untrusted certificate must fail even when its names are right: the
// fallback only ever reacts to a name mismatch, never to a chain failure.
func TestTLSUntrustedCertificateFailsDespiteFallback(t *testing.T) {
	trusted, rogue := newTestCA(t), newTestCA(t)
	port := serveTLSNNTP(t, rogue.leaf(t, []string{"localhost"}, loopbackIP))

	c := tlsClient(port, "xsnews.nl", trusted.pool)
	err := connect(t, c)
	if err == nil {
		t.Fatal("connected to a server with a certificate from an untrusted CA")
	}
	var ua x509.UnknownAuthorityError
	if !errors.As(err, &ua) {
		t.Fatalf("err = %v, want x509.UnknownAuthorityError", err)
	}
}

// Without an override a name mismatch is simply an error (nothing to fall back to).
func TestTLSHostMismatchWithoutOverrideFails(t *testing.T) {
	ca := newTestCA(t)
	port := serveTLSNNTP(t, ca.leaf(t, []string{"reader.example"}, nil))

	c := tlsClient(port, "", ca.pool)
	if err := connect(t, c); err == nil || !isHostnameMismatch(err) {
		t.Fatalf("err = %v, want a hostname verification error", err)
	}
}
