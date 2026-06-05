package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerateHash(t *testing.T) {
	h1, err := GenerateHash()
	if err != nil {
		t.Fatal(err)
	}
	if len(h1) != 32 {
		t.Errorf("hash len = %d, want 32", len(h1))
	}
	h2, _ := GenerateHash()
	if h1 == h2 {
		t.Errorf("two hashes collided: %s", h1)
	}
}

func TestBuildCSR(t *testing.T) {
	dir := t.TempDir()
	hash := "deadbeefdeadbeef"
	csrPEM, err := BuildCSR(dir, hash, "agent.unarr.app")
	if err != nil {
		t.Fatal(err)
	}
	// Key persisted.
	keyPath, _ := Paths(dir)
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("key not persisted: %v", err)
	}
	// CSR parses + carries exactly the wildcard + base SANs.
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		t.Fatal("CSR is not valid PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"*.deadbeefdeadbeef.agent.unarr.app": false,
		"deadbeefdeadbeef.agent.unarr.app":   false,
	}
	for _, n := range csr.DNSNames {
		if _, ok := want[n]; !ok {
			t.Errorf("unexpected SAN: %s", n)
		}
		want[n] = true
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("missing SAN: %s", n)
		}
	}

	// A second BuildCSR reuses the same key (cert must match the persistent key).
	before, _ := os.ReadFile(keyPath)
	if _, err := BuildCSR(dir, hash, "agent.unarr.app"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(keyPath)
	if string(before) != string(after) {
		t.Errorf("key changed across BuildCSR calls — renewals would break")
	}
}

func TestNeedsIssue(t *testing.T) {
	dir := t.TempDir()
	// Missing cert → needs issue.
	if !NeedsIssue(dir) {
		t.Error("missing cert should need issue")
	}

	_, certPath := Paths(dir)
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		t.Fatal(err)
	}

	writeSelfSigned := func(notAfter time.Time) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject:      pkix.Name{CommonName: "*.x.agent.unarr.app"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     notAfter,
		}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		if err := os.WriteFile(certPath, pemBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Fresh cert (90d) → no issue needed.
	writeSelfSigned(time.Now().Add(90 * 24 * time.Hour))
	if NeedsIssue(dir) {
		t.Error("fresh cert should not need issue")
	}

	// Within renew window (10d left) → needs issue.
	writeSelfSigned(time.Now().Add(10 * 24 * time.Hour))
	if !NeedsIssue(dir) {
		t.Error("near-expiry cert should need issue")
	}

	// Garbage → needs issue.
	_ = os.WriteFile(certPath, []byte("not a cert"), 0o644)
	if !NeedsIssue(dir) {
		t.Error("unparseable cert should need issue")
	}
}
