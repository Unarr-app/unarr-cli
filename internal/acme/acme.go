// Package acme handles the agent side of the per-agent direct-TLS feature
// (plex.direct model). The agent generates and keeps its private key LOCALLY,
// builds a CSR for *.<hash>.agent.unarr.app, and sends only the CSR to the
// web-side broker (which runs the ACME order against Let's Encrypt via DNS-01
// and returns the signed chain). The key never leaves the machine.
//
// File layout under the agent state dir:
//
//	certs/agent.key   ECDSA P-256 private key (PEM, persisted across renewals)
//	certs/agent.crt   issued certificate chain (PEM, hot-reloaded by the stream server)
package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GenerateHash returns a 32-hex-char (16-byte) high-entropy agent hash label.
func GenerateHash() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate agent hash: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Paths returns the key/cert file paths under the agent state dir.
func Paths(dataDir string) (keyPath, certPath string) {
	dir := filepath.Join(dataDir, "certs")
	return filepath.Join(dir, "agent.key"), filepath.Join(dir, "agent.crt")
}

// loadOrCreateKey returns the agent's persistent EC key, creating + persisting
// it on first use. Reused across renewals so the cert always matches the key.
func loadOrCreateKey(keyPath string) (*ecdsa.PrivateKey, error) {
	if data, err := os.ReadFile(keyPath); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("agent.key is not valid PEM")
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse agent.key: %w", err)
		}
		return key, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate EC key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal EC key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir certs: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write agent.key: %w", err)
	}
	return key, nil
}

// WildcardName is THE identity→SAN mapping for the per-agent cert: the
// wildcard name BuildCSR requests and NeedsIssue asserts on the issued cert.
// Single source so the two sides (request vs verify) can never drift — a
// silent drift would make NeedsIssue re-issue on every renewal tick forever.
func WildcardName(hash, baseDomain string) string {
	return "*." + hash + "." + baseDomain
}

// BuildCSR ensures the persistent key exists and returns a PEM CSR requesting
// the wildcard *.<hash>.<baseDomain> (plus the bare <hash>.<baseDomain> so a
// future non-wildcard use still validates). baseDomain e.g. "agent.unarr.app".
func BuildCSR(dataDir, hash, baseDomain string) (csrPEM string, err error) {
	keyPath, _ := Paths(dataDir)
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return "", err
	}
	wildcard := WildcardName(hash, baseDomain)
	base := hash + "." + baseDomain
	tmpl := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: wildcard},
		DNSNames:           []string{wildcard, base},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return "", fmt.Errorf("create CSR: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

// WriteCert persists the issued certificate chain atomically (temp file + rename)
// so a concurrent reader (NeedsIssue, or the listener's GetCertificate reload)
// can never observe a half-written PEM during a renewal.
func WriteCert(dataDir, certPEM string) error {
	_, certPath := Paths(dataDir)
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return fmt.Errorf("mkdir certs: %w", err)
	}
	tmp := certPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(certPEM), 0o644); err != nil {
		return fmt.Errorf("write agent.crt: %w", err)
	}
	if err := os.Rename(tmp, certPath); err != nil {
		return fmt.Errorf("rename agent.crt: %w", err)
	}
	return nil
}

// renewBefore is how long ahead of expiry we proactively renew.
const renewBefore = 30 * 24 * time.Hour

// NeedsIssue reports whether we should (re)request a cert: true when the cert is
// missing, unparseable, expired, within renewBefore of expiry, OR issued for a
// hash other than the current agent hash.
//
// The hash-mismatch case is load-bearing: agent_hash can be regenerated (config
// reset / identity migration) while the OLD cert — for *.<oldHash>.<base>, not
// yet expired — stays on disk. The web then encodes every direct-TLS hostname
// under the NEW hash (<ip>.<newHash>.<base>), so the browser is served a cert
// whose CN/SAN don't match → TLS validation fails → direct-TLS is silently dead
// for up to the cert's ~90-day lifetime, forcing every remote https:// browser
// onto the (flaky) cloudflared funnel. Re-issuing whenever the on-disk cert
// doesn't cover the wildcard for the CURRENT hash makes direct-TLS self-heal on
// the next renewal tick. hash/baseDomain empty (direct-TLS not configured) skips
// the check and keeps the pure expiry semantics.
func NeedsIssue(dataDir, hash, baseDomain string) bool {
	_, certPath := Paths(dataDir)
	data, err := os.ReadFile(certPath)
	if err != nil {
		return true
	}
	// agent.crt is a CHAIN — walk every PEM block instead of assuming the leaf
	// comes first. Anchoring on block[0] made chain ordering load-bearing: an
	// intermediate-first chain would fail the SAN check below on every renewal
	// tick and re-issue forever (Let's Encrypt rate-limit burn).
	var certs []*x509.Certificate
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if c, perr := x509.ParseCertificate(block.Bytes); perr == nil {
			certs = append(certs, c)
		}
	}
	if len(certs) == 0 {
		return true
	}

	if hash != "" && baseDomain != "" {
		// BuildCSR requests WildcardName(hash, base); that SAN must be present
		// on a still-fresh cert in the chain for the current identity to hold.
		// EqualFold: DNS names are case-insensitive and Let's Encrypt lowercases
		// SANs on issue — an exact compare against a mixed-case configured base
		// would re-issue on every tick despite a perfectly valid cert.
		want := WildcardName(hash, baseDomain)
		for _, c := range certs {
			for _, n := range c.DNSNames {
				if strings.EqualFold(n, want) {
					return time.Now().Add(renewBefore).After(c.NotAfter)
				}
			}
		}
		return true
	}
	// Direct-TLS not configured → pure expiry semantics on the first
	// (conventionally the leaf) certificate, as before.
	return time.Now().Add(renewBefore).After(certs[0].NotAfter)
}
