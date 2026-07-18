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
	const hash = "x"
	const base = "agent.unarr.app"
	dir := t.TempDir()
	// Missing cert → needs issue.
	if !NeedsIssue(dir, hash, base) {
		t.Error("missing cert should need issue")
	}

	_, certPath := Paths(dir)
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		t.Fatal(err)
	}

	// selfSigned returns a PEM cert. wildcard=true covers *.<h>.agent.unarr.app
	// (via the same WildcardName helper production uses); false emits a CA-ish
	// cert with no agent SANs (stands in for a chain intermediate).
	selfSigned := func(h string, wildcard bool, notAfter time.Time) []byte {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     notAfter,
		}
		if wildcard {
			w := WildcardName(h, "agent.unarr.app")
			tmpl.Subject = pkix.Name{CommonName: w}
			tmpl.DNSNames = []string{w, h + ".agent.unarr.app"}
		} else {
			tmpl.Subject = pkix.Name{CommonName: "Fake Intermediate CA"}
		}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	writeSelfSigned := func(h string, notAfter time.Time) {
		if err := os.WriteFile(certPath, selfSigned(h, true, notAfter), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Fresh cert (90d) for the current hash → no issue needed.
	writeSelfSigned(hash, time.Now().Add(90*24*time.Hour))
	if NeedsIssue(dir, hash, base) {
		t.Error("fresh cert should not need issue")
	}

	// Fresh cert but issued for a DIFFERENT hash (agent_hash was regenerated) →
	// needs issue, else direct-TLS stays silently broken until the stale cert
	// expires. Regression guard for the fleet-wide direct-TLS outage.
	if !NeedsIssue(dir, "y", base) {
		t.Error("cert for a stale hash should need re-issue")
	}

	// Empty hash (direct-TLS not configured) → pure expiry semantics, no re-issue
	// for a fresh cert.
	if NeedsIssue(dir, "", "") {
		t.Error("empty hash should keep expiry-only semantics (fresh cert, no issue)")
	}

	// CHAIN with the intermediate FIRST and the leaf second → still recognized
	// (the SAN check must walk every PEM block, not just block[0] — an
	// intermediate-first chain used to trigger a perpetual re-issue loop).
	fresh := time.Now().Add(90 * 24 * time.Hour)
	chain := append(selfSigned(hash, false, fresh), selfSigned(hash, true, fresh)...)
	if err := os.WriteFile(certPath, chain, 0o644); err != nil {
		t.Fatal(err)
	}
	if NeedsIssue(dir, hash, base) {
		t.Error("intermediate-first chain with a fresh matching leaf should not need issue")
	}

	// Case-insensitive SAN match: Let's Encrypt lowercases SANs, so a
	// mixed-case configured base domain must still match the on-disk cert.
	writeSelfSigned(hash, fresh)
	if NeedsIssue(dir, hash, "Agent.Unarr.App") {
		t.Error("mixed-case base domain should match the lowercased SAN (EqualFold)")
	}

	// Within renew window (10d left) → needs issue.
	writeSelfSigned(hash, time.Now().Add(10*24*time.Hour))
	if !NeedsIssue(dir, hash, base) {
		t.Error("near-expiry cert should need issue")
	}

	// Garbage → needs issue.
	_ = os.WriteFile(certPath, []byte("not a cert"), 0o644)
	if !NeedsIssue(dir, hash, base) {
		t.Error("unparseable cert should need issue")
	}
}
